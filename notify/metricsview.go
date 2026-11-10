package notify

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// metrics in GET /<app> (DESIGN-metrics.md): two fixed query_range
// templates over the store's socket, scoped to the app the route
// resolved, the last hour at a minute's step; best-effort within one
// budget, and cached per app.
const (
	viewWindow = time.Hour
	viewStep   = time.Minute
	viewRecent = 5 * time.Minute
	// viewFresh is how old the newest point may be and still be the
	// present: a series that stopped (an app stopped, a cap a later
	// release dropped) keeps its last point in the hour, which is history,
	// not now.
	viewFresh = 3 * viewStep
	// viewBudget bounds both queries: the route never waits longer.
	viewBudget = 2 * time.Second
	// viewCache is how long an answer, or a failure, is reused: a
	// token, stolen or not, drives at most one query pair per app per
	// viewCache.
	viewCache = 30 * time.Second
)

// errViewStore is the one failure a deployer reads: fixed text, never
// the store's own words.
const errViewStore = "the metrics store did not answer"

// rangeQuerier is the store's query_range and instant query.
type rangeQuerier interface {
	queryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]series, error)
	query(ctx context.Context, q string) ([]series, error)
}

// AppMetrics is the `metrics` object.
type AppMetrics struct {
	Window    string      `json:"window,omitempty"`
	At        *time.Time  `json:"at,omitempty"`
	Memory    *memorySumm `json:"memory_bytes,omitempty"`
	MemoryMax *float64    `json:"memory_max_bytes,omitempty"`
	CPU       *cpuSummary `json:"cpu_cores,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type memorySumm struct {
	// Now is absent when the series has no point in the last viewFresh.
	Now *float64 `json:"now,omitempty"`
	Max float64  `json:"max"`
}

type cpuSummary struct {
	// Avg5m is absent when the series has no point in the last five
	// minutes.
	Avg5m *float64 `json:"avg_5m,omitempty"`
	Avg   float64  `json:"avg"`
}

type viewEntry struct {
	at   time.Time
	val  AppMetrics
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
func (v *metricsView) AppMetrics(ctx context.Context, app string) any {
	now := v.now()
	v.mu.Lock()
	e := v.cache[app]
	if e != nil && now.Sub(e.at) < viewCache {
		v.mu.Unlock()
		select {
		case <-e.done:
			return e.val
		case <-ctx.Done():
			return AppMetrics{Error: errViewStore}
		}
	}
	e = &viewEntry{at: now, done: make(chan struct{})}
	v.cache[app] = e
	v.mu.Unlock()

	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), viewBudget)
	defer cancel()
	val, err := v.ask(qctx, app, now)
	if err != nil {
		v.logger.Debug("metrics_store: GET /<app>'s metrics", zap.String("app", app), zap.Error(err))
		val = AppMetrics{Error: errViewStore}
	}
	e.val = val
	close(e.done)
	return val
}

func (v *metricsView) ask(ctx context.Context, app string, now time.Time) (AppMetrics, error) {
	start, end := now.Add(-viewWindow), now
	mem, err := v.store.queryRange(ctx,
		fmt.Sprintf(`{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",app=%q}`, app), start, end, viewStep)
	if err != nil {
		return AppMetrics{}, err
	}
	cpu, err := v.store.queryRange(ctx, fmt.Sprintf(`sum(rate(motel_app_cpu_seconds_total{app=%q}[2m]))`, app), start, end, viewStep)
	if err != nil {
		return AppMetrics{}, err
	}
	// The hour's peak over the raw samples, which the push writes every
	// 30 s: the range above holds one sample a step, and a peak between
	// two steps would be lost.
	peak, err := v.store.query(ctx, fmt.Sprintf(`max_over_time(motel_app_memory_bytes{app=%q}[1h])`, app))
	if err != nil {
		return AppMetrics{}, err
	}
	out := AppMetrics{Window: "1h"}
	var newest time.Time
	seen := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	var memPts, maxPts []point
	for _, s := range mem {
		switch s.labels["__name__"] {
		case "motel_app_memory_bytes":
			memPts = append(memPts, s.points...)
		case "motel_app_memory_max_bytes":
			maxPts = append(maxPts, s.points...)
		}
	}
	fresh := func(p point) bool { return !p.t.Before(end.Add(-viewFresh)) }
	if last, ok := latest(memPts); ok {
		m := &memorySumm{}
		if fresh(last) {
			m.Now = &last.v
		}
		// The max is the raw peak's alone: the range's first step can
		// carry a sample from before the hour, which max_over_time
		// rightly leaves out.
		for _, s := range peak {
			for _, p := range s.points {
				m.Max = max(m.Max, p.v)
			}
		}
		out.Memory = m
		seen(last.t)
	}
	if last, ok := latest(maxPts); ok && fresh(last) {
		out.MemoryMax = &last.v
	}
	var cpuPts []point
	for _, s := range cpu {
		cpuPts = append(cpuPts, s.points...)
	}
	if last, ok := latest(cpuPts); ok {
		c := &cpuSummary{}
		var sum, recent float64
		var n int
		for _, p := range cpuPts {
			sum += p.v
			if p.t.After(end.Add(-viewRecent)) {
				recent += p.v
				n++
			}
		}
		c.Avg = sum / float64(len(cpuPts))
		if n > 0 {
			avg := recent / float64(n)
			c.Avg5m = &avg
		}
		out.CPU = c
		seen(last.t)
	}
	if !newest.IsZero() {
		out.At = &newest
	}
	return out, nil
}

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
