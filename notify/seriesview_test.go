package notify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// seriesStore is a rangeQuerier for the series view: each query's
// answer by the query's head, every call recorded.
type seriesStore struct {
	mu    sync.Mutex
	asked []seriesAsk
	ans   func(q string, start, end time.Time, step time.Duration) []series
	err   error
	block chan struct{}
}

type seriesAsk struct {
	q          string
	start, end time.Time
	step       time.Duration
	deadline   time.Duration // the budget left when asked
	cancelled  bool
}

func (f *seriesStore) queryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]series, error) {
	dl, _ := ctx.Deadline()
	f.mu.Lock()
	f.asked = append(f.asked, seriesAsk{q, start, end, step, time.Until(dl), ctx.Err() != nil})
	block, err, ans := f.block, f.err, f.ans
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if ans == nil {
		return nil, nil
	}
	return ans(q, start, end, step), nil
}

func (f *seriesStore) query(context.Context, string) ([]series, error) {
	return nil, errors.New("the series view asks no instant query")
}

func (f *seriesStore) calls() []seriesAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seriesAsk(nil), f.asked...)
}

// newSeriesView is a view on a clock the test moves.
func newSeriesView(now *time.Time) *seriesView {
	return &seriesView{now: func() time.Time { return *now }, cache: map[seriesKey]*seriesEntry{}}
}

// TestSeriesViewAsks: for each window, the four queries of the app the
// route named and nothing else, over the window's span ending at the
// last multiple of its step, at its step; and the body's step and end
// say so.
func TestSeriesViewAsks(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 47, 31, 0, time.UTC)
	for window, want := range map[string]struct {
		span, step time.Duration
		end        time.Time
	}{
		"1h":  {time.Hour, time.Minute, time.Date(2026, 11, 20, 9, 47, 0, 0, time.UTC)},
		"24h": {24 * time.Hour, 5 * time.Minute, time.Date(2026, 11, 20, 9, 45, 0, 0, time.UTC)},
		"7d":  {7 * 24 * time.Hour, 30 * time.Minute, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)},
		"14d": {14 * 24 * time.Hour, time.Hour, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
	} {
		store := &seriesStore{}
		v := newSeriesView(&now)
		got := v.get(context.Background(), store, "blog", window, zap.NewNop())
		if got.Store != wire.StoreOn || got.Window != window || got.StepSeconds != int(want.step/time.Second) || got.End == nil || !got.End.Equal(want.end) || got.Panels == nil || len(got.Panels) != 0 {
			t.Errorf("%s: %+v", window, got)
		}
		calls := store.calls()
		if len(calls) != 4 {
			t.Fatalf("%s: %d queries, want 4", window, len(calls))
		}
		var qs []string
		for _, c := range calls {
			if !c.end.Equal(want.end) || !c.start.Equal(want.end.Add(-want.span)) || c.step != want.step {
				t.Errorf("%s: %s over %v..%v at %v", window, c.q, c.start, c.end, c.step)
			}
			if c.deadline <= 0 || c.deadline > seriesBudget {
				t.Errorf("%s: asked with %v of budget left", window, c.deadline)
			}
			qs = append(qs, c.q)
		}
		mem, cpu, classes, p95 := seriesQueries(`app="blog"`, seriesWindows[window])
		for _, q := range []string{mem, cpu, classes, p95} {
			if !strings.Contains(strings.Join(qs, "\n"), q) {
				t.Errorf("%s: not asked %s", window, q)
			}
			// Every selector names the app, and only blog.
			if strings.Count(q, `app="blog"`) != strings.Count(q, "app=") {
				t.Errorf("%s: a selector of %s is not the app's", window, q)
			}
		}
		if !strings.HasPrefix(mem, "max(max_over_time(motel_app_memory_bytes{app=\"blog\"}["+promDuration(want.step)+"]))") {
			t.Errorf("%s: memory is %s", window, mem)
		}
	}
	// A window liveswap does not take: the store is not asked.
	store := &seriesStore{}
	if got := newSeriesView(&now).get(context.Background(), store, "blog", "2h", zap.NewNop()); got.Store != wire.StoreUnanswered || len(store.calls()) != 0 {
		t.Errorf("2h: %+v, %d calls", got, len(store.calls()))
	}
}

// TestSeriesViewPanels: the answers as panels, in their fixed order,
// each value rounded so no number is a long run of digits: a step with
// no point is no pair, a point that is no number (a quantile of no
// response) or below zero is none, a time outside the window is none;
// a class with no point at a time another has one counted nothing then;
// the 5xx share leaves aborted out and has no point where nothing was
// sent; an app with no request series has no request panel.
func TestSeriesViewPanels(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	end := now
	at := func(min int) time.Time { return end.Add(time.Duration(min) * time.Minute) }
	ts := func(min int) float64 { return float64(at(min).Unix()) }
	answers := func(q string, _, _ time.Time, _ time.Duration) []series {
		switch {
		case strings.HasPrefix(q, "max(max_over_time(motel_app_memory_bytes"):
			return []series{{points: []point{{at(-3), 190840832.4}, {at(-1), 191889408}, {at(0), 12345678901.6}}}}
		case strings.HasPrefix(q, "sum(rate("):
			return []series{{points: []point{{at(-3), 0.00012345678}, {at(-2), math.NaN()}, {at(0), 0.5}, {at(1), 9}}}}
		case strings.HasPrefix(q, "sum by (class)"):
			return []series{
				{labels: map[string]string{"class": "2xx"}, points: []point{{at(-3), 120}, {at(-2), 60}, {at(-1), 0}}},
				{labels: map[string]string{"class": "5xx"}, points: []point{{at(-2), 30}}},
				{labels: map[string]string{"class": "aborted"}, points: []point{{at(-2), 10}, {at(-1), 6}}},
			}
		case strings.HasPrefix(q, "histogram_quantile"):
			return []series{{points: []point{{at(-3), 0.0801234}, {at(-2), math.NaN()}, {at(-1), math.Inf(1)}, {at(0), -1}}}}
		}
		return nil
	}
	v := newSeriesView(&now)
	got := v.get(context.Background(), &seriesStore{ans: answers}, "blog", "1h", zap.NewNop())
	want := []wire.Panel{
		{Name: "memory_bytes", Unit: "bytes", Points: [][2]float64{{ts(-3), 190840832}, {ts(-1), 191889408}, {ts(0), 12345678902}}},
		{Name: "cpu_cores", Unit: "cores", Points: [][2]float64{{ts(-3), 0.000123}, {ts(0), 0.5}}},
		{Name: "requests_per_second", Unit: "per_second", Points: [][2]float64{{ts(-3), 2}, {ts(-2), 1.666667}, {ts(-1), 0.1}}},
		{Name: "5xx_share", Unit: "share", Points: [][2]float64{{ts(-3), 0}, {ts(-2), 0.3333}}},
		{Name: "p95_ms", Unit: "ms", Points: [][2]float64{{ts(-3), 80.1}}},
	}
	if !reflect.DeepEqual(got.Panels, want) {
		t.Errorf("panels:\n got %v\nwant %v", got.Panels, want)
	}
	// An app no observe names: memory and CPU alone.
	got = v.get(context.Background(), &seriesStore{ans: func(q string, s, e time.Time, st time.Duration) []series {
		if strings.HasPrefix(q, "sum by (class)") || strings.HasPrefix(q, "histogram_quantile") {
			return nil
		}
		return answers(q, s, e, st)
	}}, "shop", "1h", zap.NewNop())
	if len(got.Panels) != 2 || got.Panels[0].Name != wire.PanelMemory || got.Panels[1].Name != wire.PanelCPU {
		t.Errorf("an app with no observe: %+v", got.Panels)
	}
}

// TestSeriesViewShares: one set of queries per app and window per
// seriesEvery, whatever the number of callers, those at once waiting
// for the one in flight; another app or window is its own; a failure
// is kept as long, journalled once, and told as "unanswered", never
// with the store's words; a caller that goes away does not end the
// queries, which fill the entry for the next; and an entry past
// seriesEvery is dropped when the next is made.
func TestSeriesViewShares(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	v := newSeriesView(&now)
	store := &seriesStore{block: make(chan struct{})}
	var wg sync.WaitGroup
	got := make([]wire.Series, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = v.get(context.Background(), store, "blog", "24h", zap.NewNop())
		}()
	}
	// The first is in flight (its four queries asked) before the store
	// answers; the rest wait on it.
	for len(store.calls()) < 4 {
		time.Sleep(time.Millisecond)
	}
	close(store.block)
	wg.Wait()
	if n := len(store.calls()); n != 4 {
		t.Errorf("eight callers at once: %d queries, want 4", n)
	}
	for _, g := range got {
		if g.Store != wire.StoreOn {
			t.Errorf("a caller got %+v", g)
		}
	}
	store.block = nil
	v.get(context.Background(), store, "blog", "24h", zap.NewNop())
	v.get(context.Background(), store, "blog", "7d", zap.NewNop())
	v.get(context.Background(), store, "shop", "24h", zap.NewNop())
	if n := len(store.calls()); n != 12 {
		t.Errorf("the same app and window, and two others: %d queries, want 12", n)
	}
	now = now.Add(seriesEvery)
	v.get(context.Background(), store, "blog", "24h", zap.NewNop())
	if n := len(store.calls()); n != 16 {
		t.Errorf("after seriesEvery: %d queries, want 16", n)
	}
	v.mu.Lock()
	held := len(v.cache)
	v.mu.Unlock()
	if held != 1 {
		t.Errorf("the cache holds %d entries after the others' time, want 1", held)
	}

	// A failure: unanswered, kept, journalled once, the store's words in
	// the journal alone.
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	bad := &seriesStore{err: errors.New("the metrics store answered 503 to /api/v1/query_range: secret-ish words")}
	for range 3 {
		g := v.get(context.Background(), bad, "blog", "1h", logger)
		if g.Store != wire.StoreUnanswered || g.Window != "1h" || len(g.Panels) != 0 || g.Panels == nil || g.End != nil || g.StepSeconds != 0 {
			t.Errorf("a failure: %+v", g)
		}
	}
	if n := len(bad.calls()); n != 4 {
		t.Errorf("a failure was asked again within seriesEvery: %d queries", n)
	}
	if logs.Len() != 1 || !strings.Contains(logs.All()[0].ContextMap()["error"].(string), "secret-ish") {
		t.Errorf("journal: %v", logs.All())
	}

	// A caller that goes away is told unanswered; the queries go on
	// past it, with their own budget, and fill the entry.
	slow := &seriesStore{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan wire.Series)
	go func() { done <- v.get(ctx, slow, "blog", "14d", zap.NewNop()) }()
	for len(slow.calls()) < 4 {
		time.Sleep(time.Millisecond)
	}
	// A second caller waits on the first's queries; it goes away too.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan wire.Series)
	go func() { done2 <- v.get(ctx2, slow, "blog", "14d", zap.NewNop()) }()
	cancel2()
	if g := <-done2; g.Store != wire.StoreUnanswered {
		t.Errorf("a waiter gone: %+v", g)
	}
	cancel()
	close(slow.block)
	if g := <-done; g.Store != wire.StoreOn {
		t.Errorf("the caller gone first still got its own fetch's answer? %+v", g)
	}
	for _, c := range slow.calls() {
		if c.cancelled {
			t.Errorf("a query was asked with the caller's context: %+v", c)
		}
	}
	if g := v.get(context.Background(), slow, "blog", "14d", zap.NewNop()); g.Store != wire.StoreOn || len(slow.calls()) != 4 {
		t.Errorf("the next caller: %+v, %d queries", g, len(slow.calls()))
	}
}

// The series' queries over the store's own MetricsQL (vmalert-tool,
// as the summary's are tested): a point of a range query is the query
// at its step. A push every 30 s; at 20m the pushes stop for ten
// minutes (moteld down, say) and come back at 30m30s.
//
//   - memory: one push of 500 between two steps is the step's point
//     (the push at the step alone, a plain selector's, is 100); no
//     point inside the gap at the hour's step; the day's hour step
//     over the gap has its point, of the pushes it holds.
//   - requests: 10 responses a push is 20 a step; none inside the gap;
//     the first step after it counts its own push alone (the store
//     keeps no sample from before the gap as the step's base), so the
//     gap is no spike; over an hour the gap's counts are in (1200,
//     from 0 at the hour's start).
//   - p95: the step's responses, interpolated in the buckets.
func TestSeriesViewOverTheStore(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	hour, day := seriesWindows["1h"], seriesWindows["14d"]
	mem1, _, classes1, p951 := seriesQueries(`app="a"`, hour)
	mem14, _, classes14, _ := seriesQueries(`app="a"`, day)
	const gap = "_x20"
	// Buckets: 0.1 s, 1 s, +Inf; each push 10 responses, 9 under 0.1 s.
	bucket := func(le string, per int) string {
		return fmt.Sprintf(`      - series: 'motel_app_response_seconds_bucket{app="a",release="v1",le="%s",job="moteld"}'
        values: '0+%dx40 %s %d+%dx99'
`, le, per, gap, 41*per+20*per, per)
	}
	test := `rule_files: [` + RulesName + `]
evaluation_interval: 30s
tests:
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="moteld"}'
        values: '1x40 ` + gap + ` 1x99'
      - series: 'motel_app_memory_bytes{app="a",job="moteld"}'
        values: '100x20 500 100x19 ` + gap + ` 100x99'
      - series: 'motel_app_requests_total{app="a",release="v1",class="2xx",job="moteld"}'
        values: '0+10x40 ` + gap + ` 610+10x99'
` + bucket("0.1", 9) + bucket("1", 10) + bucket("+Inf", 10) + `      - series: 'motel_app_response_seconds_count{app="a",release="v1",job="moteld"}'
        values: '0+10x40 ` + gap + ` 610+10x99'
    metricsql_expr_test:
`
	expect := func(expr, at, samples string) {
		test += fmt.Sprintf("      - expr: '%s'\n        eval_time: %s\n        exp_samples: %s\n", strings.ReplaceAll(expr, "'", "''"), at, samples)
	}
	expect(mem1, "11m", `[{labels: '{}', value: 500}]`)
	expect(`max(motel_app_memory_bytes{app="a"})`, "11m", `[{labels: '{}', value: 100}]`)
	expect(mem1, "25m", `[]`)
	expect(mem14, "60m", `[{labels: '{}', value: 500}]`)
	expect(classes1, "10m", `[{labels: '{class="2xx"}', value: 20}]`)
	expect(classes1, "25m", `[]`)
	expect(classes1, "31m", `[{labels: '{class="2xx"}', value: 10}]`)
	expect(classes1, "32m", `[{labels: '{class="2xx"}', value: 20}]`)
	expect(classes14, "60m", `[{labels: '{class="2xx"}', value: 1200}]`)
	expect("round("+p951+", 0.001)", "10m", `[{labels: '{}', value: 0.55}]`)
	expect("round("+p951+", 0.001)", "25m", `[]`)
	runUnittest(t, tool, dir, test)
}
