package notify

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// An app's series (DESIGN-metrics.md, "An app's series"): GET
// /_motel/apps/<app>/series, liveswap's route, asks for one app's
// panels over one of wire.SeriesRanges. Four fixed range queries over
// the store's socket, scoped to the app the route's handle names, at
// the window's fixed step, run at once within one budget, and shared
// by every person and config for seriesEvery per app and window.
const (
	// seriesEvery is how long one answer, or one failure, is given to
	// every request for its app and window: a token, stolen or not,
	// drives at most four range queries per app and window per
	// seriesEvery, and the store pushes every 30 s.
	seriesEvery = 30 * time.Second
	// seriesBudget bounds the four queries together: the route never
	// waits longer on a store that does not answer. Measured on Debian's
	// 1.112.0 under the box's caps (DESIGN-metrics.md): the longest
	// window's four take well under a second there.
	seriesBudget = 3 * time.Second
)

// seriesWindow is one ?range= of the series: its span, the step
// between points, and the CPU rate's window (two steps at the hour's
// 60 s, as the summary's; one step otherwise).
type seriesWindow struct {
	span, step time.Duration
	rate       string
}

// seriesWindows are wire.SeriesRanges' spans and steps: at most 337
// points a panel (the hour's 61), so a body stays bounded whatever the
// window (DESIGN-metrics.md has the sizes measured).
var seriesWindows = map[string]seriesWindow{
	"1h":  {span: time.Hour, step: time.Minute, rate: "2m"},
	"24h": {span: 24 * time.Hour, step: 5 * time.Minute, rate: "5m"},
	"7d":  {span: 7 * 24 * time.Hour, step: 30 * time.Minute, rate: "30m"},
	"14d": {span: 14 * 24 * time.Hour, step: time.Hour, rate: "1h"},
}

// seriesQueries is the four range queries of the app sel names, at
// step: the memory's peak in each step (the push writes it every 30 s,
// and a point at the step would hold one sample of the step's); the
// running unit's CPU (cpuQuery, as the summary's); the responses by
// class over each step, through the operator rules' guard
// (counterIncrease, as the summary counts them), which gives both the
// rate and the 5xx share; and the 95th percentile of each step's
// responses, the guard's over the histogram.
func seriesQueries(sel string, w seriesWindow) (mem, cpu, classes, p95 string) {
	step := promDuration(w.step)
	mem = "max(max_over_time(motel_app_memory_bytes{" + sel + "}[" + step + "]))"
	cpu = cpuQuery(sel, w.rate)
	classes = "sum by (class) (" + counterIncrease("motel_app_requests_total{"+sel+"}", w.step, 0) + ")"
	p95 = "histogram_quantile(0.95, sum by (le) (" + histogramIncrease("motel_app_response_seconds", "{"+sel+"}", w.step, 0) + "))"
	return
}

// seriesEntry is one answer for an app and window, for every request
// until it is seriesEvery old; done closes when it is in.
type seriesEntry struct {
	at   time.Time
	val  wire.Series
	done chan struct{}
}

// seriesView is the process's answers, one per store, app and window.
type seriesView struct {
	now func() time.Time

	mu    sync.Mutex
	cache map[seriesKey]*seriesEntry
}

// seriesKey is an answer's store, by its socket, not the client: each
// config's provision makes a client of its own for the same socket.
type seriesKey struct {
	socket, app, window string
}

// theSeries is the process's: every config's source reads through it,
// so a reload does not ask the store again, and every person shares one
// answer per app and window.
var theSeries = &seriesView{now: time.Now, cache: map[seriesKey]*seriesEntry{}}

// get answers from the entry under seriesEvery old, or waits for the
// queries in flight, or makes them. A request that gives up first is
// told the store did not answer; the queries it waited for still fill
// the entry for the next. Entries past seriesEvery are dropped as
// another is made, so what is held is at most the answers asked for in
// the last seriesEvery.
func (v *seriesView) get(ctx context.Context, store rangeQuerier, socket, app, window string, logger *zap.Logger) wire.Series {
	w, ok := seriesWindows[window]
	if !ok {
		// liveswap refuses any other; a window it adds that this does not
		// know is told as the store not answering, never guessed at.
		return wire.Series{Store: wire.StoreUnanswered, Window: window, Panels: []wire.Panel{}}
	}
	k := seriesKey{socket, app, window}
	v.mu.Lock()
	now := v.now()
	if e := v.cache[k]; e != nil && now.Sub(e.at) < seriesEvery && !now.Before(e.at) {
		v.mu.Unlock()
		select {
		case <-e.done:
			return e.val
		case <-ctx.Done():
			return wire.Series{Store: wire.StoreUnanswered, Window: window, Panels: []wire.Panel{}}
		}
	}
	for kk, e := range v.cache {
		if now.Sub(e.at) >= seriesEvery || now.Before(e.at) {
			delete(v.cache, kk)
		}
	}
	e := &seriesEntry{at: now, done: make(chan struct{})}
	v.cache[k] = e
	v.mu.Unlock()
	// The queries are the box's, not this request's: a person who goes
	// away does not end them for the next. The entry is closed however
	// they end, a panic too.
	defer close(e.done)
	e.val = wire.Series{Store: wire.StoreUnanswered, Window: window, Panels: []wire.Panel{}}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seriesBudget)
	defer cancel()
	val, err := askSeries(qctx, store, app, window, w, now)
	if err != nil {
		// A person is told only that the store did not answer: the cause
		// is the operator's, at most one line per app and window per
		// seriesEvery, as kept.
		logger.Warn("monitor metrics: a person's read of an app's series was answered \"store\":\"unanswered\": the metrics store did not answer its queries",
			zap.String("app", app), zap.String("range", window), zap.Error(err))
		return e.val
	}
	e.val = val
	return e.val
}

// askSeries runs the four queries at once over [end - span, end], end
// being the last multiple of the step at least the store's latency
// offset old, and reads their answers into the panels. The store aligns
// a range query so itself (measured on Debian's 1.112.0: start and end
// rounded down to the step, which lets it reuse what it computed for
// the last one), so the newest point is of the last whole step, up to a
// step old (the body's end says which); and a step that ended inside
// the latency offset may not hold its last push yet, or is filled with
// the one before it, so it is left for the next answer.
func askSeries(ctx context.Context, store rangeQuerier, app, window string, w seriesWindow, now time.Time) (wire.Series, error) {
	step := int64(w.step / time.Second)
	end := time.Unix(now.Add(-storeLatencyOffset).Unix()/step*step, 0).UTC()
	start := end.Add(-w.span)
	memQ, cpuQ, classesQ, p95Q := seriesQueries(fmt.Sprintf(`app=%q`, app), w)
	qs := []string{memQ, cpuQ, classesQ, p95Q}
	ans := make([][]series, len(qs))
	errs := make([]error, len(qs))
	var wg sync.WaitGroup
	for i, q := range qs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ans[i], errs[i] = store.queryRange(ctx, q, start, end, w.step)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return wire.Series{}, err
		}
	}
	out := wire.Series{Store: wire.StoreOn, Window: window, StepSeconds: int(w.step / time.Second), End: &end, Panels: []wire.Panel{}}
	add := func(name, unit string, pts [][2]float64) {
		if len(pts) > 0 {
			out.Panels = append(out.Panels, wire.Panel{Name: name, Unit: unit, Points: pts})
		}
	}
	add(wire.PanelMemory, wire.UnitBytes, seriesPoints(ans[0], start, end, math.Round))
	add(wire.PanelCPU, wire.UnitCores, seriesPoints(ans[1], start, end, func(v float64) float64 { return roundTo(v, 1e6) }))
	rps, share := requestPoints(ans[2], start, end, w.step)
	add(wire.PanelRequests, wire.UnitPerSecond, rps)
	add(wire.Panel5xx, wire.UnitShare, share)
	add(wire.PanelP95, wire.UnitMs, seriesPoints(ans[3], start, end, func(v float64) float64 { return roundTo(v*1e3, 10) }))
	return out, nil
}

// seriesPoints is a range answer's one series as points, oldest first,
// each value through round: a point below zero, or a time outside
// [start, end], is a gap. (The store leaves a step with no response out
// of a quantile's answer, measured; a NaN or an infinity in an answer
// fails the store client's read, parsePoint, so none reaches here.) The queries aggregate to
// one series; were there more, the points of a time are the highest.
func seriesPoints(ss []series, start, end time.Time, round func(float64) float64) [][2]float64 {
	at := map[int64]float64{}
	for _, s := range ss {
		for _, p := range s.points {
			if !usable(p, start, end) {
				continue
			}
			if old, ok := at[p.t.Unix()]; !ok || p.v > old {
				at[p.t.Unix()] = p.v
			}
		}
	}
	out := make([][2]float64, 0, len(at))
	for _, t := range slices.Sorted(maps.Keys(at)) {
		out = append(out, [2]float64{float64(t), round(at[t])})
	}
	return out
}

// requestPoints is the responses by class as two panels: the rate,
// every class's together over the step, and the 5xx share of those
// sent (aborted left out), where any were. A class with no point at a
// time another has one counted nothing then (its series begins with its
// first response); a time no class has a point at is a gap in both.
func requestPoints(ss []series, start, end time.Time, step time.Duration) (rps, share [][2]float64) {
	type counts struct{ total, aborted, err5xx float64 }
	at := map[int64]*counts{}
	for _, s := range ss {
		for _, p := range s.points {
			if !usable(p, start, end) {
				continue
			}
			c := at[p.t.Unix()]
			if c == nil {
				c = &counts{}
				at[p.t.Unix()] = c
			}
			c.total += p.v
			switch s.labels["class"] {
			case "aborted":
				c.aborted += p.v
			case "5xx":
				c.err5xx += p.v
			}
		}
	}
	rps, share = [][2]float64{}, [][2]float64{}
	for _, t := range slices.Sorted(maps.Keys(at)) {
		c := at[t]
		rps = append(rps, [2]float64{float64(t), roundTo(c.total/step.Seconds(), 1e6)})
		if sent := c.total - c.aborted; sent > 0 {
			share = append(share, [2]float64{float64(t), roundTo(min(c.err5xx/sent, 1), 1e4)})
		}
	}
	return rps, share
}

// usable is a point of a number at or above zero inside [start, end].
func usable(p point, start, end time.Time) bool {
	return !math.IsNaN(p.v) && !math.IsInf(p.v, 0) && p.v >= 0 && !p.t.Before(start) && !p.t.After(end)
}

// seriesSource is liveswap's SeriesSource for one config: the process's
// view, the config's store and its socket, and its logger for what goes
// wrong.
type seriesSource struct {
	view   *seriesView
	store  rangeQuerier
	socket string
	logger *zap.Logger
}

func (s seriesSource) AppSeries(ctx context.Context, app, window string) wire.Series {
	return s.view.get(ctx, s.store, s.socket, app, window, s.logger)
}
