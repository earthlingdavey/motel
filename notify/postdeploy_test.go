package notify

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap"
)

// relStore is the store, scripted per release and window: the new
// release's numbers since the cutover, the old one's before it.
type relStore struct {
	mu      sync.Mutex
	nums    map[string]releaseNumbers // "<release>" or "<release> old"
	timed   map[string]float64        // the histogram's count, the same keys
	err     error
	queries []string
	at      []time.Time
	// together, when set, holds each query until that many are in
	// flight at once, or fails it after a second.
	together int
	inFlight int
	cond     *sync.Cond
	// The store stamps a push as it takes it and back-fills none it
	// missed. Unless stopped, it takes one at every instant. From
	// stoppedAt it took none, until resumedAt when that is set: a query
	// at a time between reads the numbers as they stood at stoppedAt
	// (snap, snapTimed), and its last push of motel's is at stoppedAt.
	stoppedAt, resumedAt time.Time
	snap                 map[string]releaseNumbers
	snapTimed            map[string]float64
	// unseen is how many asks for motel's push more find none: a push
	// the store has stamped and not yet made searchable.
	unseen int
}

// stop is the store taking no push from at on: down, or read-only.
func (s *relStore) stop(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stoppedAt, s.resumedAt = at, time.Time{}
	s.snap, s.snapTimed = maps.Clone(s.nums), maps.Clone(s.timed)
}

// resume is the store taking pushes again from at.
func (s *relStore) resume(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumedAt = at
}

// gap is whether t falls where the store took no push.
func (s *relStore) gap(t time.Time) bool {
	return !s.stoppedAt.IsZero() && !t.Before(s.stoppedAt) && (s.resumedAt.IsZero() || t.Before(s.resumedAt))
}

func (s *relStore) set(key string, sent, errs float64, p95 float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nums[key] = releaseNumbers{sent: sent, errors: errs, p95: p95, hasP95: p95 > 0}
	s.timed[key] = sent
}

// setTimed is how many of a release's responses were timed: all it
// sent, unless set.
func (s *relStore) setTimed(key string, timed float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timed[key] = timed
}

var releaseRe = regexp.MustCompile(`release="([^"]*)"`)

// oldWindow is a query over the old release's window: its increase is
// offset (counterIncrease's guard has an offset in either).
var oldWindow = regexp.MustCompile(`increase\([^)]*\] offset `)

func (s *relStore) queryAt(_ context.Context, q string, t time.Time) ([]series, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.together > 0 {
		if s.cond == nil {
			s.cond = sync.NewCond(&s.mu)
		}
		s.inFlight++
		s.cond.Broadcast()
		deadline := time.Now().Add(time.Second)
		for s.inFlight < s.together && time.Now().Before(deadline) {
			go func() { time.Sleep(10 * time.Millisecond); s.cond.Broadcast() }()
			s.cond.Wait()
		}
		if s.inFlight < s.together {
			s.inFlight--
			return nil, errors.New("slow: alone")
		}
	}
	s.queries = append(s.queries, q)
	s.at = append(s.at, t)
	if s.err != nil {
		return nil, s.err
	}
	pt := func(v float64) []point { return []point{{t: time.Unix(0, 0), v: v}} }
	if q == pushedQuery { // proven over the store's own engine in TestPostDeployPushedQueryOverTheStore
		last := t
		if s.gap(t) {
			last = s.stoppedAt
		}
		if s.unseen > 0 {
			s.unseen--
			return nil, nil
		}
		if t.Sub(last) >= checkFresh {
			return nil, nil
		}
		return []series{{labels: map[string]string{}, points: pt(1)}}, nil
	}
	key := releaseRe.FindStringSubmatch(q)[1]
	if oldWindow.MatchString(q) {
		key += " old"
	}
	nums, timed := s.nums, s.timed
	if s.gap(t) {
		nums, timed = s.snap, s.snapTimed
	}
	n, ok := nums[key]
	if !ok {
		return nil, nil // no series
	}
	if strings.HasPrefix(q, "histogram_quantile") { // its buckets are told by the count
		if !n.hasP95 {
			return nil, nil
		}
		return []series{{labels: map[string]string{}, points: pt(n.p95)}}, nil
	}
	if strings.Contains(q, "motel_app_response_seconds_count") {
		return []series{{labels: map[string]string{}, points: pt(timed[key])}}, nil
	}
	return []series{
		{labels: map[string]string{"class": "2xx"}, points: pt(n.sent - n.errors)},
		{labels: map[string]string{"class": "5xx"}, points: pt(n.errors)},
		{labels: map[string]string{"class": "aborted"}, points: pt(7)}, // never sent: not in the share
	}, nil
}

type testComparator struct {
	*comparator
	at    time.Time
	store *relStore
	m     *MetricsStore
	evs   []Event
	logs  *observer.ObservedLogs
}

func newTestComparator(t *testing.T, rules ...Rule) *testComparator {
	t.Helper()
	tc := &testComparator{at: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), store: &relStore{nums: map[string]releaseNumbers{}, timed: map[string]float64{}}, m: &MetricsStore{Rules: rules}}
	var mu sync.Mutex
	tc.comparator = newComparator(func(ev Event) { mu.Lock(); tc.evs = append(tc.evs, ev); mu.Unlock() })
	tc.now = func() time.Time { return tc.at }
	tc.every = time.Hour // the tests tick it themselves
	core, logs := observer.New(zap.DebugLevel)
	tc.logs = logs
	tc.configure(liveRules{m: tc.m, store: tc.store, logger: zap.New(core)})
	t.Cleanup(func() { tc.release(tc.m) })
	return tc
}

func share(v float64) *float64 { return &v }

var regressRule = Rule{Name: "regress", Kind: rulePostDeploy, Window: caddyMinutes(5), Max5xxShare: share(0.02), MaxP95: caddyMs(800), Failures: 2}

func caddyMinutes(n int) caddy.Duration { return caddy.Duration(time.Duration(n) * time.Minute) }
func caddyMs(n int) caddy.Duration      { return caddy.Duration(time.Duration(n) * time.Millisecond) }

// deploy is liveswap's report of a cutover at the test's now; the
// clock then moves to where the store answers for it.
func (tc *testComparator) deploy(app, release, previous string) {
	tc.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: app, Release: release, Previous: previous, DeployedBy: "local:/k.pub", Cutover: tc.at})
	tc.at = tc.at.Add(storeLag + time.Second)
}

// relaunch is liveswap's report of a relaunch's cutover: previous the
// release itself when its instance still served, empty when it had
// stopped.
func (tc *testComparator) relaunch(app, release, previous string) {
	tc.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: app, Release: release, Previous: previous, DeployedBy: "local:/k.pub", Cutover: tc.at, Relaunch: true})
	tc.at = tc.at.Add(storeLag + time.Second)
}

// minute moves to the next check and makes it.
func (tc *testComparator) minute() {
	tc.at = tc.at.Add(time.Minute)
	tc.tick(context.Background())
}

func TestPostDeployFailsAfterItsCountInARow(t *testing.T) {
	tc := newTestComparator(t, regressRule)
	cutover := tc.at
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v1 old", 200, 1, 0.1)
	tc.store.set("v2", 200, 20, 0.1) // 10 % against 0.5 %
	tc.minute()
	if len(tc.evs) != 0 {
		t.Fatalf("told after one failing check of two: %+v", tc.evs)
	}
	tc.minute()
	if len(tc.evs) != 1 {
		t.Fatalf("after two failing checks: %d events, want one", len(tc.evs))
	}
	ev := tc.evs[0]
	if ev.Name != eventPostDeployRegressed || ev.Kind != kindEvent || ev.Severity != SeverityWarning || ev.App != "blog" ||
		ev.Source != sourceRules || ev.Release != "v2" || ev.DeployedBy != "local:/k.pub" || ev.Details["rule"] != "regress" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.key() != "rules/app:blog/post_deploy_regressed" {
		t.Errorf("key = %s", ev.key())
	}
	want := "blog v2 regressed 2m after its deploy, over 200 responses: 5xx share 0.1 against 0.005 before it, p95 100ms against 100ms before it; 2 failing checks in a row (rule regress)"
	if ev.Summary != want {
		t.Errorf("summary =\n%s\nwant\n%s", ev.Summary, want)
	}
	// One event: the comparison is over.
	tc.minute()
	tc.minute()
	if len(tc.evs) != 1 || len(tc.runs) != 0 {
		t.Fatalf("after the event: %d events, %d in flight", len(tc.evs), len(tc.runs))
	}
	// The windows: the new release's since the cutover, the old one's
	// the same length before it and a push past it, in whole minutes,
	// through counterIncrease, read at their end, which the store has.
	for i, at := range tc.store.at {
		if k := at.Sub(cutover); k%time.Minute != 0 || k < time.Minute || at.After(tc.at.Add(-storeLag)) {
			t.Errorf("query %d read at cutover + %s (now cutover + %s)", i, k, tc.at.Sub(cutover))
		}
	}
	var sawNew, sawOld bool
	for _, q := range tc.store.queries {
		if q == pushedQuery {
			continue
		}
		if !strings.Contains(q, " (present_over_time(") || !strings.Contains(q, ") or increase(") {
			t.Errorf("a query not through counterIncrease: %s", q)
		}
		if strings.Contains(q, "_bucket") && !strings.Contains(q, `) unless ignoring(le) (present_over_time(motel_app_response_seconds_count{app="blog"`) {
			t.Errorf("a bucket query not through histogramIncrease: %s", q)
		}
		sawNew = sawNew || strings.Contains(q, counterIncrease(`motel_app_requests_total{app="blog",release="v2"}`, 2*time.Minute, 0))
		sawOld = sawOld || strings.Contains(q, `{app="blog",release="v1"}[150s] offset 90s)`) &&
			strings.Contains(q, `present_over_time(motel_app_requests_total{app="blog",release="v1"}[1d] offset 4m)`)
	}
	if !sawNew || !sawOld {
		t.Errorf("queries = %q", tc.store.queries)
	}
}

func TestPostDeploySkipsBelowTheFloor(t *testing.T) {
	r := regressRule
	r.Failures, r.MinRequests = 1, 50
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v2", 49, 49, 0)
	tc.minute()
	if len(tc.evs) != 0 {
		t.Fatalf("told below the floor: %+v", tc.evs)
	}
	for _, q := range tc.store.queries {
		if oldWindow.MatchString(q) {
			t.Errorf("the old release asked about under the floor: %s", q)
		}
	}
	tc.store.set("v2", 50, 50, 0)
	tc.minute()
	if len(tc.evs) != 1 {
		t.Fatalf("at the floor, all 5xx: %d events", len(tc.evs))
	}
}

// The p95 needs min_requests responses timed, not only sent: on a
// cached site, 300 sent of which 6 went to the app, one of them a cold
// start of 3 s, is no regression; with 100 timed it is.
func TestPostDeployP95NeedsItsFloorTimed(t *testing.T) {
	r := regressRule
	r.Failures, r.Max5xxShare = 1, nil
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v1 old", 300, 0, 0.1)
	tc.store.set("v2", 300, 0, 3)
	tc.store.setTimed("v2", 6)
	tc.minute()
	if len(tc.evs) != 0 {
		t.Fatalf("told over 6 timed responses: %+v", tc.evs)
	}
	for _, q := range tc.store.queries {
		if strings.HasPrefix(q, "histogram_quantile") {
			t.Errorf("the p95 asked under the floor: %s", q)
		}
	}
	tc.store.setTimed("v2", 100)
	tc.minute()
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "p95 3000ms against 100ms before it") {
		t.Fatalf("with 100 timed: %+v", tc.evs)
	}
	// An old release with too few timed is no p95 baseline: the bound
	// alone counts.
	tc2 := newTestComparator(t, r)
	tc2.deploy("blog", "v2", "v1")
	tc2.store.set("v1 old", 300, 0, 5)
	tc2.store.setTimed("v1 old", 6)
	tc2.store.set("v2", 300, 0, 3)
	tc2.minute()
	if len(tc2.evs) != 1 || strings.Contains(tc2.evs[0].Summary, "before it,") || strings.Contains(tc2.evs[0].Summary, "before it;") {
		t.Fatalf("an old release with 6 timed as the p95 baseline: %+v", tc2.evs)
	}
	// The summary says why there was no comparison: the old release had
	// enough sent, and too few timed (Copilot round 3 on #429).
	if !strings.Contains(tc2.evs[0].Summary, "p95 3000ms; the release before it had 6 responses timed, too few to compare its p95;") {
		t.Fatalf("summary %q: no word of the p95 baseline", tc2.evs[0].Summary)
	}
}

// Over the bound fails only when worse than the old release; a release
// before it with fewer responses than the floor is no baseline.
func TestPostDeployTheOldIsWorseGuard(t *testing.T) {
	r := regressRule
	r.Failures = 1
	for _, c := range []struct {
		name               string
		newSent, newErrs   float64
		newP95             float64
		oldSent, oldErrs   float64
		oldP95             float64
		previous           string
		fails              bool
		summaryHas, hasNot string
	}{
		{"5xx over the bound, old worse", 200, 20, 0.1, 200, 40, 0.1, "v1", false, "", ""},
		{"5xx over the bound, old the same", 200, 20, 0.1, 200, 20, 0.1, "v1", false, "", ""},
		{"5xx over the bound, old better", 200, 20, 0.1, 200, 2, 0.1, "v1", true, "5xx share 0.1 against 0.01 before it", ""},
		{"5xx under the bound, old far better", 200, 3, 0.1, 200, 0, 0.1, "v1", false, "", ""},
		// An old release within the bound: crossing it is the regression.
		{"5xx over the bound, old within it", 200, 6, 0.1, 200, 2, 0.1, "v1", true, "5xx share 0.03 against 0.01 before it", ""},
		// An old release already over it: a rise beyond chance and 1.2 times.
		{"5xx over the bound, old over it, a rise by chance", 200, 11, 0.1, 200, 8, 0.1, "v1", false, "", ""},
		{"5xx over the bound, old over it, a rise beyond chance", 400, 40, 0.1, 400, 12, 0.1, "v1", true, "5xx share 0.1 against 0.03 before it", ""},
		{"5xx over the bound, old over it, a rise under 1.2 times", 100000, 5500, 0.1, 100000, 5000, 0.1, "v1", false, "", ""},
		{"p95 over the bound, under old × 1.2", 200, 0, 1.0, 200, 0, 0.9, "v1", false, "", ""},
		{"p95 over the bound, over old × 1.2", 200, 0, 1.2, 200, 0, 0.9, "v1", true, "p95 1200ms against 900ms before it", ""},
		{"p95 over the bound, old has none", 200, 0, 1.2, 200, 0, 0, "v1", true, "p95 1200ms; the release before it has no p95 to compare;", "p95 1200ms against"},
		// Only a p95 over its bound says why it had no baseline.
		{"5xx over the bound, p95 within it, old has no p95", 200, 20, 0.5, 200, 2, 0, "v1", true, "5xx share 0.1 against 0.01 before it, p95 500ms; 1 failing", "compare"},
		{"no old samples: the bound alone", 200, 20, 0.1, 0, 0, 0, "v1", true, "; the release before it sent 0 responses, too few to compare;", "against"},
		{"old under the floor: the bound alone", 200, 20, 0.1, 99, 50, 0.1, "v1", true, "; the release before it sent 99 responses, too few to compare;", "against"},
		{"a first deploy: the bound alone", 200, 20, 0.1, 200, 100, 0.1, "", true, "no release before it to compare", "against"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := newTestComparator(t, r)
			tc.deploy("blog", "v2", c.previous)
			tc.store.set("v2", c.newSent, c.newErrs, c.newP95)
			if c.oldSent > 0 {
				tc.store.set("v1 old", c.oldSent, c.oldErrs, c.oldP95)
			}
			tc.minute()
			if got := len(tc.evs) == 1; got != c.fails {
				t.Fatalf("failed = %v, want %v (%+v)", got, c.fails, tc.evs)
			}
			if c.fails && (!strings.Contains(tc.evs[0].Summary, c.summaryHas) || c.hasNot != "" && strings.Contains(tc.evs[0].Summary, c.hasNot)) {
				t.Errorf("summary %q: want %q, not %q", tc.evs[0].Summary, c.summaryHas, c.hasNot)
			}
		})
	}
}

func TestPostDeployAPassResetsTheCount(t *testing.T) {
	r := regressRule
	r.Failures, r.Window = 2, caddyMinutes(10)
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "v1")
	bad := func() { tc.store.set("v2", 200, 20, 0.1) }
	good := func() { tc.store.set("v2", 200, 0, 0.1) }
	bad()
	tc.minute()
	good()
	tc.minute()
	bad()
	tc.minute()
	// The store not answering neither fails nor passes: the check is
	// asked again.
	tc.store.err = errors.New("down")
	tc.minute()
	tc.store.err = nil
	if len(tc.evs) != 0 {
		t.Fatalf("told with a pass between the failures: %+v", tc.evs)
	}
	if tc.logs.FilterMessage("monitor rules: the post-deploy check is asked again: the metrics store did not answer, or held no push of motel's for it").Len() != 1 {
		t.Error("no debug line for the store not answering")
	}
	tc.minute()
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "regressed 4m after") || !strings.Contains(tc.evs[0].Summary, "2 failing checks in a row") {
		t.Fatalf("two in a row, the second asked again: %+v", tc.evs)
	}
}

func TestPostDeployANewDeployCancels(t *testing.T) {
	r := regressRule
	r.Failures = 2
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v2", 200, 20, 0.1)
	tc.minute()
	tc.deploy("blog", "v3", "v2") // v2 no longer serves
	tc.store.set("v3", 200, 0, 0.1)
	tc.minute()
	tc.minute()
	if len(tc.evs) != 0 {
		t.Fatalf("v2's failure carried over to v3: %+v", tc.evs)
	}
	if run := tc.runs["blog"]; run == nil || run.release != "v3" {
		t.Fatalf("in flight: %+v", tc.runs)
	}
	// A rollback is a deploy too.
	tc.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v2", Previous: "v3", Cutover: tc.at})
	if run := tc.runs["blog"]; run == nil || run.release != "v2" || run.previous != "v3" {
		t.Fatalf("after the rollback: %+v", tc.runs["blog"])
	}
}

func TestPostDeployWindowEndsSilently(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(3), 3
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v2", 200, 0, 0.1)
	for range 3 {
		tc.minute()
	}
	if len(tc.runs) != 1 {
		t.Fatal("ended before its window")
	}
	n := len(tc.store.queries)
	tc.minute()
	if len(tc.runs) != 0 || len(tc.evs) != 0 || len(tc.store.queries) != n {
		t.Fatalf("after the window: %d in flight, %d events, %d more queries", len(tc.runs), len(tc.evs), len(tc.store.queries)-n)
	}
}

// One rule per app: the app's replaces the every-app one; an app with
// neither is not compared; a reload that drops the rule ends the
// comparison at its next check.
func TestPostDeployFollowsTheRules(t *testing.T) {
	every := regressRule
	own := Rule{Name: "shop_regress", Kind: rulePostDeploy, App: "shop", MaxP95: caddyMs(100), Failures: 1}
	tc := newTestComparator(t, every, own)
	if r, _ := tc.ruleFor("shop"); r.Name != "shop_regress" {
		t.Errorf("shop's rule = %s", r.Name)
	}
	if r, _ := tc.ruleFor("blog"); r.Name != "regress" {
		t.Errorf("blog's rule = %s", r.Name)
	}
	only := newTestComparator(t, own)
	only.deploy("blog", "v2", "v1")
	if len(only.runs) != 0 {
		t.Errorf("blog compared with only shop's rule: %+v", only.runs)
	}

	tc.deploy("shop", "s2", "s1")
	tc.store.set("s2", 200, 100, 0.5) // a 5xx share shop's rule has no bound for
	tc.minute()
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "p95 500ms") || strings.Contains(tc.evs[0].Summary, "5xx") || tc.evs[0].Details["rule"] != "shop_regress" {
		t.Fatalf("shop under its own rule: %+v", tc.evs)
	}

	tc.deploy("blog", "v2", "v1")
	next := &MetricsStore{} // a reload with no post_deploy line
	tc.configure(liveRules{m: next, store: tc.store, logger: zap.NewNop()})
	tc.minute()
	if len(tc.runs) != 0 {
		t.Fatalf("the rule is gone, the comparison is not: %+v", tc.runs)
	}
	// The older config back (the reload failed after Start): in flight
	// again from its next deploy.
	tc.release(next)
	tc.deploy("blog", "v3", "v2")
	if len(tc.runs) != 1 {
		t.Fatal("the older config's rule does not compare again")
	}
}

// The last config's Stop ends the goroutine and what is in flight;
// nothing is compared while no config has a metrics line.
func TestPostDeployRelease(t *testing.T) {
	c := newComparator(func(Event) {})
	m := &MetricsStore{Rules: []Rule{regressRule}}
	c.configure(liveRules{m: m, store: &relStore{nums: map[string]releaseNumbers{}, timed: map[string]float64{}}, logger: zap.NewNop()})
	c.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v2"})
	if !c.running || len(c.runs) != 1 {
		t.Fatalf("running %v, in flight %d", c.running, len(c.runs))
	}
	done := c.done
	c.release(m)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the goroutine did not end")
	}
	if c.running || len(c.runs) != 0 {
		t.Fatalf("after the last Stop: running %v, in flight %d", c.running, len(c.runs))
	}
	c.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v3"})
	if len(c.runs) != 0 {
		t.Fatal("compared with no config")
	}
}

// liveswap's success reaches the comparator before the engine's
// toggles: deploy_succeeded off still starts one.
func TestLifecycleSinkTapsTheDeploy(t *testing.T) {
	te := newTestEngine(t)
	tc := newTestComparator(t, regressRule)
	s := lifecycleSink{e: te.engine, c: tc.comparator}
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v2", Previous: "v1", Cutover: tc.at})
	if len(te.queue) != 0 {
		t.Errorf("the cutover is no event, yet queued: %+v", te.queue[0].alerts)
	}
	if run := tc.runs["blog"]; run == nil || run.previous != "v1" {
		t.Fatalf("in flight: %+v", tc.runs)
	}
	// The deploy's result, after the drain, neither starts one nor
	// replaces it.
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeploySucceeded, App: "blog", Release: "v2"})
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeployFailed, App: "blog", Release: "v3"})
	if run := tc.runs["blog"]; run == nil || run.release != "v2" || run.previous != "v1" {
		t.Fatalf("a deploy's result changed the comparison: %+v", tc.runs)
	}
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeploySucceeded, App: "shop", Release: "s2"})
	if tc.runs["shop"] != nil {
		t.Fatal("deploy_succeeded started a comparison")
	}
}

// A drain longer than the window (Copilot round 1 on #429): the
// comparison runs from the cutover, so its three checks are made while
// the old instance drains, and the regression is told.
func TestPostDeployADrainLongerThanTheWindow(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(3), 3
	te := newTestEngine(t)
	tc := newTestComparator(t, r)
	s := lifecycleSink{e: te.engine, c: tc.comparator}
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v2", Previous: "v1", Cutover: tc.at})
	tc.at = tc.at.Add(storeLag + time.Second)
	tc.store.set("v2", 200, 200, 0.1)
	for range 3 {
		tc.minute()
	}
	// The drain of 4 m ends now: the deploy's result.
	s.Lifecycle(liveswap.LifecycleEvent{Name: liveswap.EventDeploySucceeded, App: "blog", Release: "v2"})
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "3 failing checks in a row") {
		t.Fatalf("a regression under a 4 m drain: %+v", tc.evs)
	}
}

// The checks due at once are made at once: a store that takes its time
// over each does not stretch the tick past the minute between checks.
func TestPostDeployChecksAtOnce(t *testing.T) {
	r := regressRule
	r.Failures = 1
	tc := newTestComparator(t, r)
	tc.store.together = 2
	tc.store.set("v2", 200, 100, 0.1)
	tc.store.set("s2", 200, 100, 0.1)
	tc.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "shop", Release: "s2", Cutover: tc.at})
	tc.deploy("blog", "v2", "")
	tc.minute()
	if len(tc.evs) != 2 {
		t.Fatalf("two apps due at once: %d events, want both", len(tc.evs))
	}
}

// A tick late past the window's end makes the window's last check: with
// failures equal to the window's minutes, the event needs it.
func TestPostDeployALateTickMakesTheLastCheck(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(3), 3
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "")
	tc.store.set("v2", 200, 20, 0.1)
	tc.minute()
	tc.minute()
	tc.at = tc.at.Add(5 * time.Minute) // a stall: minute 3 is long past
	tc.tick(context.Background())
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "regressed 3m after") {
		t.Fatalf("after a stall past the window: %+v", tc.evs)
	}
	tc.deploy("blog", "v3", "v2")
	tc.minute()
	tc.minute()
	tc.at = tc.at.Add(5 * time.Minute)
	tc.store.set("v3", 200, 0, 0.1)
	tc.tick(context.Background())
	n := len(tc.store.queries)
	tc.at = tc.at.Add(time.Minute)
	tc.tick(context.Background())
	if len(tc.runs) != 0 || len(tc.store.queries) != n {
		t.Fatalf("after the last check: %d in flight, %d more queries", len(tc.runs), len(tc.store.queries)-n)
	}
}

// A relaunch of the release serving (liveswap's Relaunch, Previous =
// Release) is no new release: it starts no comparison, and one in
// flight for that release goes on as it was, so its baseline never
// takes in the relaunched instance's answers (Copilot round 2 on #429).
func TestPostDeployARelaunchIsNoNewRelease(t *testing.T) {
	r := regressRule
	r.Failures = 2
	tc := newTestComparator(t, r)
	tc.relaunch("blog", "v2", "v2")
	if len(tc.runs) != 0 {
		t.Fatalf("a relaunch started a comparison: %+v", tc.runs["blog"])
	}
	// The names alone decide: a cutover to the release serving without
	// the mark compares nothing either (/code-review of the mark).
	tc.deploy("blog", "v2", "v2")
	if len(tc.runs) != 0 {
		t.Fatalf("a cutover to the release serving, unmarked, started a comparison: %+v", tc.runs["blog"])
	}
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v1 old", 200, 0, 0.1)
	tc.store.set("v2", 200, 100, 2) // 5xx and p95 both over
	tc.minute()
	run := tc.runs["blog"]
	tc.deployed(liveswap.LifecycleEvent{Name: liveswap.EventCutover, App: "blog", Release: "v2", Previous: "v2", Cutover: tc.at, Relaunch: true})
	if tc.runs["blog"] != run || run.previous != "v1" || run.failed != 1 {
		t.Fatalf("the relaunch touched the comparison in flight: %+v", tc.runs["blog"])
	}
	tc.minute()
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "against 0 before it") || !strings.Contains(tc.evs[0].Summary, "p95 2000ms against 100ms") {
		t.Fatalf("after the relaunch, v2 against v1: %+v", tc.evs)
	}
}

// A relaunch of a release whose instance had stopped (Relaunch, no
// Previous: a rollback to the release that had crashed) is compared
// with the bound alone, and its event says so as a relaunch rather than
// a regression after a deploy; a new release after a stopped one keeps
// the deploy's words. Name, labels, severity and audience are the same
// (the operator's decision of 2026-11-12).
func TestPostDeployARelaunchAfterAStop(t *testing.T) {
	for _, c := range []struct {
		name, release string
		relaunch      bool
		want          string
		first         string // the first check's window
	}{
		{"the release that had stopped, relaunched", "v1", true,
			"blog v1 is over its bound 2m after its relaunch (its earlier instance had stopped), over 200 responses: 5xx share 0.1, p95 100ms; 2 failing checks in a row (rule regress)", "[20s]"},
		{"a new release after a stopped one", "v2", false,
			"blog v2 regressed 2m after its deploy, over 200 responses: 5xx share 0.1, p95 100ms; no release before it to compare; 2 failing checks in a row (rule regress)", "[1m]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := newTestComparator(t, regressRule)
			if c.relaunch {
				tc.relaunch("blog", c.release, "")
			} else {
				tc.deploy("blog", c.release, "")
			}
			if run := tc.runs["blog"]; run == nil || run.relaunch != c.relaunch {
				t.Fatalf("the comparison: %+v", run)
			}
			tc.store.set(c.release, 200, 20, 0.1)
			tc.minute()
			tc.minute()
			if len(tc.evs) != 1 {
				t.Fatalf("after two failing checks: %+v", tc.evs)
			}
			ev := tc.evs[0]
			if ev.Name != eventPostDeployRegressed || ev.Kind != kindEvent || ev.Severity != SeverityWarning || ev.App != "blog" ||
				ev.Source != sourceRules || ev.Release != c.release || ev.DeployedBy != "local:/k.pub" || ev.Details["rule"] != "regress" || len(ev.Details) != 1 {
				t.Fatalf("event = %+v", ev)
			}
			if ev.Summary != c.want {
				t.Errorf("summary =\n%s\nwant\n%s", ev.Summary, c.want)
			}
			for _, q := range tc.store.queries {
				if oldWindow.MatchString(q) {
					t.Errorf("a release before it was asked for: %s", q)
				}
			}
			// A relaunch's window starts past the stopped instance's
			// last push (relaunchSkip).
			if q := tc.store.queries[1]; !strings.Contains(q, c.first) { // after the ask for motel's push
				t.Errorf("the first check's query has no %s: %s", c.first, q)
			}
		})
	}
}

// A relaunch after a stop, over the store's own MetricsQL: v2
// crash-looped and the operator rolled back to it. The crash loop's
// 503s count under v2, the release current when they began, and the
// last of them reach the store with the first push after the cutover,
// in the counters the relaunched instance counts into. Pushes every
// 30 s from 0, the cutover at 10m05s: 10 503s a push until it, the push
// at 10m30s carries the last 10 and the relaunched instance's first 20
// 2xx, then 20 2xx a push. Each check reads none of the 503s, and the
// 2xx from that push on; read from the cutover, the first check's 5xx
// share was 0.2 (10 of 50), over any bound, for an instance that
// answered nothing but 2xx (/code-review of the relaunch mark).
func TestPostDeployARelaunchReadsPastTheStoppedInstance(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	r := comparison{app: "blog", release: "v2", cutover: time.Unix(605, 0), relaunch: true}
	var b strings.Builder
	b.WriteString(`rule_files: [` + RulesName + `]
evaluation_interval: 30s
tests:
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x40'
      - series: 'motel_app_requests_total{app="blog",release="v2",class="5xx",job="motel"}'
        values: '0+10x21 210x19'
      - series: 'motel_app_requests_total{app="blog",release="v2",class="2xx",job="motel"}'
        values: '0x20 20+20x19'
    metricsql_expr_test:
`)
	for k, ok2xx := range []int{20, 60, 100} {
		elapsed := time.Duration(k+1) * postDeployEvery
		q := classesQuery(releaseSelector(r.app, r.release), newWindow(r, elapsed), 0)
		fmt.Fprintf(&b, "      - expr: '%s'\n        eval_time: %ds\n        exp_samples:\n", strings.ReplaceAll(q, "'", "''"), r.cutover.Add(elapsed).Unix())
		fmt.Fprintf(&b, "          - {labels: '{class=\"2xx\"}', value: %d}\n          - {labels: '{class=\"5xx\"}', value: 0}\n", ok2xx)
	}
	runUnittest(t, tool, dir, b.String())
}

// A check the store did not answer is asked again, at its own time, and
// the minutes it held up after it in turn: with failures equal to the
// window's minutes, a store that missed one still lets the regression be
// told. One that does not answer past the grace ends the comparison,
// with a warning.
func TestPostDeployAsksAgainWhatTheStoreDidNotAnswer(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(3), 3
	tc := newTestComparator(t, r)
	cutover := tc.at
	tc.deploy("blog", "v2", "")
	tc.store.set("v2", 200, 20, 0.1)
	tc.minute()
	tc.store.err = errors.New("timeout")
	tc.minute() // minute 2: no answer
	tc.minute() // minute 3, and 2 still owed: no answer
	tc.store.err = nil
	tc.at = tc.at.Add(10 * time.Second)
	tc.tick(context.Background()) // minute 2
	if len(tc.evs) != 0 {
		t.Fatalf("told before the third check: %+v", tc.evs)
	}
	tc.at = tc.at.Add(10 * time.Second)
	tc.tick(context.Background()) // minute 3
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "regressed 3m after") || !strings.Contains(tc.evs[0].Summary, "3 failing checks in a row") {
		t.Fatalf("after the store came back: %+v", tc.evs)
	}
	// Each check read at its own minute, whenever it was made: its
	// first query, for motel's push, at each try.
	var mins []time.Duration
	for i, at := range tc.store.at {
		if tc.store.queries[i] == pushedQuery {
			mins = append(mins, at.Sub(cutover))
		}
	}
	if want := []time.Duration{time.Minute, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute, 3 * time.Minute}; !slices.Equal(mins, want) {
		t.Errorf("read at cutover + %v, want %v", mins, want)
	}

	tc.deploy("blog", "v3", "v2")
	tc.store.err = errors.New("timeout")
	for range 2 + int(postDeployGrace/time.Minute) {
		tc.minute()
	}
	if len(tc.runs) != 1 {
		t.Fatal("ended within the grace")
	}
	tc.minute()
	if len(tc.runs) != 0 || len(tc.evs) != 1 {
		t.Fatalf("past the grace: %d in flight, %d events", len(tc.runs), len(tc.evs))
	}
	if n := tc.logs.FilterMessageSnippet("ended unfinished").FilterField(zap.String("release", "v3")).Len(); n != 1 {
		t.Errorf("%d warning lines, want one", n)
	}
}

// A check counts only on a push the store took in the minute before its
// time: the store stamps a push as it takes it and back-fills none, so
// a check read past its last push reads the counts as they stood then.
// The store down from 1m30s to 5m, over a 3-minute window's end, and
// back inside the grace: the checks of minutes 2 and 3, asked again,
// passed on the snapshot of 1m30s while the release failed after it,
// and the comparison ended as if it had held. Minute 2 counts on that
// push, 30 s before its time; minute 3 has none, and the comparison
// ends unfinished, saying why.
func TestPostDeployCountsNoCheckTheStoreTookNoPushFor(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(3), 2
	tc := newTestComparator(t, r)
	cutover := tc.at
	tc.deploy("blog", "v2", "v1")
	tc.store.set("v1 old", 200, 0, 0.1)
	tc.store.set("v2", 200, 0, 0.1)
	tc.minute() // minute 1: a pass
	tc.store.stop(cutover.Add(90 * time.Second))
	tc.store.err = errors.New("down")
	tc.store.set("v2", 400, 200, 0.1) // what the store would hold had it taken the pushes
	tc.minute()                       // minute 2: no answer
	tc.minute()                       // minute 3, and 2 still owed
	tc.at = cutover.Add(5 * time.Minute)
	tc.store.resume(tc.at)
	tc.store.err = nil
	for range 6 {
		tc.at = tc.at.Add(10 * time.Second)
		tc.tick(context.Background())
	}
	run := tc.runs["blog"]
	if run == nil || len(tc.evs) != 0 {
		t.Fatalf("after the store came back: %d events, in flight %+v (nil: ended as held on the snapshot of 1m30s)", len(tc.evs), run)
	}
	if run.next != 3 || run.failed != 0 {
		t.Errorf("minute 2 a pass on the push of 1m30s, minute 3 owed: next %d, failed %d", run.next, run.failed)
	}
	tc.at = cutover.Add(r.window() + storeLag + postDeployGrace + time.Second)
	tc.tick(context.Background())
	if len(tc.runs) != 0 || len(tc.evs) != 0 {
		t.Fatalf("past the grace: %d in flight, %d events", len(tc.runs), len(tc.evs))
	}
	unfinished(t, tc, "3m")

	// A store read-only for the whole window (queries answered, pushes
	// refused) reads as nothing pushed: no check counts, a skip under the
	// floor no more than a verdict, and the comparison ends unfinished.
	ro := newTestComparator(t, r)
	ro.store.stop(ro.at)
	ro.store.set("v2", 200, 200, 0.1)
	ro.deploy("blog", "v2", "v1")
	for range int((r.window() + postDeployGrace) / time.Minute) {
		ro.minute()
	}
	if len(ro.runs) != 0 || len(ro.evs) != 0 {
		t.Fatalf("read-only throughout: %d in flight, %d events", len(ro.runs), len(ro.evs))
	}
	unfinished(t, ro, "1m")
	for _, q := range ro.store.queries {
		if q != pushedQuery {
			t.Errorf("a release read with no push in the store: %s", q)
		}
	}
}

// unfinished is the one warning of a comparison that ended unfinished,
// for the check over elapsed, naming the push the store did not take.
func unfinished(t *testing.T, tc *testComparator, elapsed string) {
	t.Helper()
	lines := tc.logs.FilterMessageSnippet("ended unfinished").AllUntimed()
	if len(lines) != 1 {
		t.Fatalf("%d warning lines, want one", len(lines))
	}
	want := "the metrics store holds no push of motel's from the minute before the check " + elapsed + " after the cutover"
	if got, _ := lines[0].ContextMap()["error"].(string); !strings.Contains(got, want) {
		t.Errorf("the warning's error = %q, want %q", got, want)
	}
}

// The store down from 1m30s to 5m (or read-only then, or a clock
// stepped forward): minute 1 failed on a cold start's blip, minutes 2
// and 3, asked again once it came back, read the one snapshot of 1m30s
// and failed again, and the warning counted three failing checks in a
// row over a minute and a half of responses. Minute 2 counts on that
// push, 30 s before its time; minute 3 has none and is not counted.
func TestPostDeployCountsNoSnapshotTwice(t *testing.T) {
	r := regressRule
	r.Window, r.Failures = caddyMinutes(5), 3
	tc := newTestComparator(t, r)
	cutover := tc.at
	tc.deploy("blog", "v2", "")
	tc.store.set("v2", 200, 20, 0.1) // a cold start's blip
	tc.minute()
	tc.store.stop(cutover.Add(90 * time.Second))
	tc.store.err = errors.New("down")
	tc.store.set("v2", 2000, 20, 0.1) // the blip, diluted since
	for range 3 {
		tc.minute()
	}
	tc.at = cutover.Add(5 * time.Minute)
	tc.store.resume(tc.at)
	tc.store.err = nil
	for range 3 {
		tc.at = tc.at.Add(10 * time.Second)
		tc.tick(context.Background())
	}
	if len(tc.evs) != 0 {
		t.Fatalf("told on one snapshot counted as three checks: %+v", tc.evs)
	}
	if run := tc.runs["blog"]; run == nil || run.failed != 2 || run.next != 3 {
		t.Fatalf("minutes 1 and 2 failing, 3 owed: %+v", run)
	}
}

// A push the store stamped before a check's time but had not made
// searchable when the check asked is found when the check is asked
// again, and the check counts then, once.
func TestPostDeployAPushSeenLateCountsOnce(t *testing.T) {
	r := regressRule
	r.Failures = 2
	tc := newTestComparator(t, r)
	tc.deploy("blog", "v2", "")
	tc.store.set("v2", 200, 20, 0.1)
	tc.store.unseen = 1
	tc.minute()
	run := tc.runs["blog"]
	if run == nil || run.failed != 0 || run.next != 1 {
		t.Fatalf("counted before its push was seen: %+v", run)
	}
	for range 2 {
		tc.at = tc.at.Add(10 * time.Second)
		tc.tick(context.Background())
		if run.failed != 1 || run.next != 2 {
			t.Fatalf("asked again: %+v", run)
		}
	}
	tc.minute()
	if len(tc.evs) != 1 || !strings.Contains(tc.evs[0].Summary, "regressed 2m after") || !strings.Contains(tc.evs[0].Summary, "2 failing checks in a row") {
		t.Fatalf("the second failing check: %+v", tc.evs)
	}
	classes := 0
	for _, q := range tc.store.queries {
		if strings.HasPrefix(q, "sum by (class)") {
			classes++
		}
	}
	if classes != 2 {
		t.Errorf("the release read %d times, want once a counted check", classes)
	}
}

// The ask for motel's push, over the store's own MetricsQL: pushes
// every 30 s from 0 to 90 s, then none until 300 s. A check at 120 s
// (its last push 30 s before it) or at 149 s counts; at 150 s the push
// of 90 s is the one the check a minute before read, and the window
// leaves it out; at 270 s there is none; from 300 s there is again.
func TestPostDeployPushedQueryOverTheStore(t *testing.T) {
	tool := vmTool(t, "vmalert-tool")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RulesName), renderRules(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`rule_files: [` + RulesName + `]
evaluation_interval: 30s
tests:
  - interval: 30s
    input_series:
      - series: 'process_start_time_seconds{job="motel"}'
        values: '1x3 _x6 1x4'
    metricsql_expr_test:
`)
	for _, c := range []struct {
		at     int
		pushed bool
	}{{60, true}, {120, true}, {149, true}, {150, false}, {180, false}, {270, false}, {300, true}, {330, true}} {
		fmt.Fprintf(&b, "      - expr: '%s'\n        eval_time: %ds\n", strings.ReplaceAll(pushedQuery, "'", "''"), c.at)
		if c.pushed {
			b.WriteString("        exp_samples:\n          - {labels: '{}', value: 1}\n")
		} else {
			b.WriteString("        exp_samples: []\n")
		}
	}
	runUnittest(t, tool, dir, b.String())
}
