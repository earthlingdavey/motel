package notify

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// metrics in GET /<app> (DESIGN-metrics.md): seven fixed queries over
// the store's socket, scoped to the app the route resolved, over one of
// wire.MetricsRanges: two query_range templates (memory, CPU), the
// window's memory peak, and four instant queries over the requests
// observe counted; run at once within one budget, and cached
// per app and window.
const (
	// viewRecent is what avg_5m averages over, up to the newest point.
	viewRecent = 5 * time.Minute
	// viewFreshSteps is how many of the window's steps old the newest
	// point may be and still be the present (3 m for the hour, 15 m for
	// the day, whose newest step can be over 5 m old): a series that
	// stopped (an app stopped, a cap a later release dropped) keeps its
	// last point in the window, which is history, not now.
	viewFreshSteps = 3
	// viewBudget bounds the queries together: the route never waits longer.
	viewBudget = 2 * time.Second
	// viewCache is how long an answer, or a failure, is reused: a
	// token, stolen or not, drives at most one set of queries per app
	// and window per viewCache.
	viewCache = 30 * time.Second
)

// viewWindow is one `?range=`: its span, the range queries' step, and
// the CPU rate's own window (two steps at the hour's 60 s; one at the
// day's 5 m, which a rate over less would sample between).
type viewWindow struct {
	span, step time.Duration
	rate       string
}

var viewWindows = map[string]viewWindow{
	"1h":  {span: time.Hour, step: time.Minute, rate: "2m"},
	"24h": {span: 24 * time.Hour, step: 5 * time.Minute, rate: "5m"},
}

// errViewStore is the one failure a deployer reads: fixed text, never
// the store's own words.
const errViewStore = "the metrics store did not answer"

// rangeQuerier is the store's query_range and instant query.
type rangeQuerier interface {
	queryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]series, error)
	query(ctx context.Context, q string) ([]series, error)
}

type viewEntry struct {
	at   time.Time
	val  wire.Metrics
	done chan struct{}
}

// metricsView is liveswap's MetricsSource for one config.
type metricsView struct {
	store  rangeQuerier
	now    func() time.Time
	logger *zap.Logger

	mu    sync.Mutex
	cache map[string]*viewEntry
}

func newMetricsView(store rangeQuerier, logger *zap.Logger) *metricsView {
	return &metricsView{store: store, now: time.Now, logger: logger, cache: map[string]*viewEntry{}}
}

// AppMetrics answers from the cache, or waits for the one query in
// flight for app, or makes it. A request that gives up first gets the
// error object; the query it waited for still fills the cache.
func (v *metricsView) AppMetrics(ctx context.Context, app, window string) *wire.Metrics {
	w, ok := viewWindows[window]
	if !ok {
		// liveswap refuses any other; a window it adds that this does
		// not know is told as the store's failure, never guessed at.
		return &wire.Metrics{Error: errViewStore}
	}
	key := app + "\x00" + window
	now := v.now()
	v.mu.Lock()
	e := v.cache[key]
	if e != nil && now.Sub(e.at) < viewCache {
		v.mu.Unlock()
		select {
		case <-e.done:
			val := e.val // the cache's own stays as it was
			return &val
		case <-ctx.Done():
			return &wire.Metrics{Error: errViewStore}
		}
	}
	e = &viewEntry{at: now, done: make(chan struct{})}
	v.cache[key] = e
	v.mu.Unlock()

	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), viewBudget)
	defer cancel()
	val, err := v.ask(qctx, app, window, w, now)
	if err != nil {
		v.logger.Debug("monitor metrics: GET /<app>'s metrics", zap.String("app", app), zap.Error(err))
		val = wire.Metrics{Error: errViewStore}
	}
	e.val = val
	close(e.done)
	return &val
}

// viewAnswers is the seven queries' answers.
type viewAnswers struct {
	mem, cpu, peak, classes, p50, p95, cache []series
}

// viewRequests is the view's four request queries over a window.
type viewRequests struct{ classes, p50, p95, cache string }

// requestQueries is the request queries of the app sel names over
// span. The counters through counterIncrease, the buckets through
// histogramIncrease, over the window, as the operator's rules read
// them: an app's series appears with its first push, already at that
// push's count, which the store's increase drops when it is large (22
// responses read as 0, e2e metrics 3a), and increase_pure alone would
// read a series' every count since motel began as the window's own
// after a gap in the pushes or when the metrics line is first loaded
// on a box whose observer has counted.
func requestQueries(sel string, span time.Duration) viewRequests {
	hist := histogramIncrease("motel_app_response_seconds", "{"+sel+"}", span, 0)
	return viewRequests{
		classes: "sum by (class) (" + counterIncrease("motel_app_requests_total{"+sel+"}", span, 0) + ")",
		p50:     "histogram_quantile(0.5, sum by (le) (" + hist + "))",
		p95:     "histogram_quantile(0.95, sum by (le) (" + hist + "))",
		cache:   "sum by (result) (" + counterIncrease("motel_app_cache_responses_total{"+sel+"}", span, 0) + ")",
	}
}

func (v *metricsView) ask(ctx context.Context, app, window string, w viewWindow, now time.Time) (wire.Metrics, error) {
	start, end := now.Add(-w.span), now
	sel := fmt.Sprintf(`app=%q`, app)
	var a viewAnswers
	rq := requestQueries(sel, w.span)
	type job struct {
		dst *[]series
		run func() ([]series, error)
	}
	instant := func(q string) func() ([]series, error) {
		return func() ([]series, error) { return v.store.query(ctx, q) }
	}
	jobs := []job{
		{&a.mem, func() ([]series, error) {
			return v.store.queryRange(ctx, `{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",`+sel+`}`, start, end, w.step)
		}},
		{&a.cpu, func() ([]series, error) {
			return v.store.queryRange(ctx, `sum(rate(motel_app_cpu_seconds_total{`+sel+`}[`+w.rate+`]))`, start, end, w.step)
		}},
		// The window's peak over the raw samples, which the push writes
		// every 30 s: the range above holds one sample a step, and a peak
		// between two steps would be lost.
		{&a.peak, instant(`max_over_time(motel_app_memory_bytes{` + sel + `}[` + window + `])`)},
		{&a.classes, instant(rq.classes)},
		{&a.p50, instant(rq.p50)},
		{&a.p95, instant(rq.p95)},
		{&a.cache, instant(rq.cache)},
	}
	// At once: seven one after another would not fit the budget on a
	// slow store. Each writes its own answer; the first error is the
	// answer's.
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			*j.dst, errs[i] = j.run()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return wire.Metrics{}, err
		}
	}
	out := wire.Metrics{Window: window}
	var newest time.Time
	seen := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	var memPts, maxPts []point
	for _, s := range a.mem {
		switch s.labels["__name__"] {
		case "motel_app_memory_bytes":
			memPts = append(memPts, s.points...)
		case "motel_app_memory_max_bytes":
			maxPts = append(maxPts, s.points...)
		}
	}
	fresh := func(p point) bool { return !p.t.Before(end.Add(-viewFreshSteps * w.step)) }
	if last, ok := latest(memPts); ok {
		m := &wire.MemorySummary{}
		if fresh(last) {
			m.Now = &last.v
		}
		// The max is the raw peak's alone: the range's first step can
		// carry a sample from before the window, which max_over_time
		// rightly leaves out.
		for _, s := range a.peak {
			for _, p := range s.points {
				if m.Max == nil || p.v > *m.Max {
					v := p.v
					m.Max = &v
				}
			}
		}
		out.Memory = m
		seen(last.t)
	}
	if last, ok := latest(maxPts); ok && fresh(last) {
		out.MemoryMax = &last.v
	}
	var cpuPts []point
	for _, s := range a.cpu {
		cpuPts = append(cpuPts, s.points...)
	}
	if last, ok := latest(cpuPts); ok {
		c := &wire.CPUSummary{}
		var sum, recent float64
		var n int
		for _, p := range cpuPts {
			sum += p.v
			if fresh(last) && p.t.After(last.t.Add(-viewRecent)) {
				recent += p.v
				n++
			}
		}
		c.Avg = roundTo(sum/float64(len(cpuPts)), 1e6)
		if n > 0 {
			avg := roundTo(recent/float64(n), 1e6)
			c.Avg5m = &avg
		}
		out.CPU = c
		seen(last.t)
	}
	if !newest.IsZero() {
		out.At = &newest
	}
	out.Requests = summarizeRequests(a, w.span)
	return out, nil
}

// summarizeRequests reads the four request queries; nil when the store
// holds no request series for the app.
func summarizeRequests(a viewAnswers, span time.Duration) *wire.RequestSummary {
	if len(a.classes) == 0 {
		return nil
	}
	r := &wire.RequestSummary{ByClass: map[string]uint64{}}
	for _, s := range a.classes {
		v, ok := instantValue(s)
		if !ok {
			continue
		}
		n := uint64(math.Round(v))
		if class := s.labels["class"]; class != "" && n > 0 {
			r.ByClass[class] += n
		}
		r.Total += n
	}
	r.PerSecond = roundTo(float64(r.Total)/span.Seconds(), 1e6)
	if sent := r.Total - r.ByClass["aborted"]; sent > 0 {
		share := roundTo(float64(r.ByClass["5xx"])/float64(sent), 1e4)
		r.Share5xx = &share
	}
	r.P50 = quantileMs(a.p50)
	r.P95 = quantileMs(a.p95)
	var hits, spoke float64
	for _, s := range a.cache {
		v, ok := instantValue(s)
		if !ok {
			continue
		}
		spoke += v
		if res := s.labels["result"]; res == "hit" || res == "stale" {
			hits += v
		}
	}
	if spoke > 0 {
		share := roundTo(hits/spoke, 1e4)
		r.CacheHitShare = &share
	}
	return r
}

// instantValue is an instant answer's one value, when it is a number.
func instantValue(s series) (float64, bool) {
	if len(s.points) == 0 {
		return 0, false
	}
	v := s.points[len(s.points)-1].v
	return v, !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// quantileMs is histogram_quantile's answer in milliseconds, to a tenth;
// nil with no response in the window (no answer, or NaN).
func quantileMs(ss []series) *float64 {
	for _, s := range ss {
		if v, ok := instantValue(s); ok {
			ms := roundTo(v*1e3, 10)
			return &ms
		}
	}
	return nil
}

// roundTo keeps v to 1/per; every fraction in the object is rounded (a
// CPU average to a millionth of a core; the memory values are bytes). An unrounded one marshals as a run
// of twenty or more digits (5.740305648246404e-05 is
// 0.00005740305648246404 in JSON), which the app's response filter
// reads as a generated secret and masks inside the number; the body is
// then no JSON, and the whole status is withheld.
func roundTo(v, per float64) float64 { return math.Round(v*per) / per }

// latest is the newest point.
func latest(pts []point) (point, bool) {
	var out point
	for _, p := range pts {
		if p.t.After(out.t) {
			out = p
		}
	}
	return out, len(pts) > 0
}
