package notify

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap"
)

// The post-deploy comparison (DESIGN-metrics.md, "Operator rules"):
// motel's, not vmalert's, which cannot know when a cutover was or which
// release came before. After a deploy or a rollback of an app with a
// post_deploy rule, each minute until the rule's window ends, the new
// release's numbers since the cutover are set against the old one's
// over the same length just before it. It only tells: one
// post_deploy_regressed, never a refusal or a rollback.
const (
	eventPostDeployRegressed = "post_deploy_regressed"
	// postDeployEvery is a check's spacing, from the cutover.
	postDeployEvery = time.Minute
	// postDeployTick is how often the comparator looks for a check due.
	postDeployTick = 10 * time.Second
	// storeLag is how far behind the present a check reads the store:
	// past its -search.latencyOffset (30 s), within which the store
	// shifts an instant query's time back by the offset, and so both
	// windows away from the cutover (measured in e2e metrics 10: the
	// old release's last half minute read as nothing).
	storeLag = 35 * time.Second
	// p95Margin is how much slower than the old release the new one's
	// p95 must be, over its bound, to fail a check: a quantile is an
	// estimate inside a bucket.
	p95Margin = 1.2
)

// instantQuerier is the store's instant query at a time.
type instantQuerier interface {
	queryAt(ctx context.Context, q string, t time.Time) ([]series, error)
}

// comparison is one deploy's, in flight.
type comparison struct {
	app, release, previous, deployedBy string
	cutover                            time.Time
	// next is the next check's number k: over the k minutes from the
	// cutover, due storeLag after them.
	next int
	// failed is how many checks in a row failed.
	failed int
}

// liveRules is a config's part of the comparator.
type liveRules struct {
	m      *MetricsStore
	store  instantQuerier
	logger *zap.Logger
}

// comparator is the process's, as the pusher is: pointed by each
// config's Start at its own rules and store; what is in flight outlives
// a reload, and follows the rules the config serving holds at each
// check.
type comparator struct {
	mu     sync.Mutex
	now    func() time.Time
	report func(Event)
	every  time.Duration // the goroutine's tick

	active *MetricsStore
	store  instantQuerier
	logger *zap.Logger
	live   []liveRules
	runs   map[string]*comparison

	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newComparator(report func(Event)) *comparator {
	return &comparator{now: time.Now, report: report, every: postDeployTick, logger: zap.NewNop(), runs: map[string]*comparison{}}
}

// theComparator reports through notify's engine, as the pusher does.
var theComparator = newComparator(theEngine.report)

// ruleFor is the post_deploy rule for app in the active config: its
// own, else the one for every app. Called with mu held.
func (c *comparator) ruleFor(app string) (Rule, bool) {
	if c.active == nil {
		return Rule{}, false
	}
	var every *Rule
	for i, r := range c.active.Rules {
		if r.Kind != rulePostDeploy {
			continue
		}
		if r.App == app {
			return r, true
		}
		if r.App == "" {
			every = &c.active.Rules[i]
		}
	}
	if every != nil {
		return *every, true
	}
	return Rule{}, false
}

// deployed starts a comparison at a deploy's or a rollback's success,
// told by liveswap through lifecycleSink before the engine's toggles. A
// deploy of an app with one in flight ends it: the release it compared
// no longer serves.
func (c *comparator) deployed(ev liveswap.LifecycleEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || ev.App == "" {
		return
	}
	if _, ok := c.runs[ev.App]; ok {
		delete(c.runs, ev.App)
		c.logger.Debug("monitor rules: a new deploy ends the post-deploy comparison in flight", zap.String("app", ev.App), zap.String("release", ev.Release))
	}
	if _, ok := c.ruleFor(ev.App); !ok {
		return
	}
	cut := ev.Cutover
	if cut.IsZero() {
		cut = c.now()
	}
	c.runs[ev.App] = &comparison{app: ev.App, release: ev.Release, previous: ev.Previous, deployedBy: ev.DeployedBy, cutover: cut, next: 1}
}

// due is one check to make: over elapsed from the cutover, read at
// cutover + elapsed.
type due struct {
	run     *comparison
	cmp     comparison // the run as it stood, for the queries
	rule    Rule
	elapsed time.Duration
}

// tick makes every check due: each outside the lock, one after another.
func (c *comparator) tick(ctx context.Context) {
	c.mu.Lock()
	if c.active == nil {
		c.mu.Unlock()
		return
	}
	now := c.now()
	var dues []due
	for _, app := range sortedKeys(c.runs) {
		r := c.runs[app]
		rule, ok := c.ruleFor(app)
		if !ok {
			delete(c.runs, app) // the config serving has no rule for it any more
			continue
		}
		// The latest whole minute the store can answer for: one a
		// stalled tick missed is not made late.
		k := int(now.Sub(r.cutover.Add(storeLag)) / postDeployEvery)
		if k < r.next {
			continue
		}
		elapsed := time.Duration(k) * postDeployEvery
		if elapsed > rule.window() {
			delete(c.runs, app) // the window is over: the release held
			c.logger.Debug("monitor rules: the post-deploy comparison ended with its window", zap.String("app", app), zap.String("release", r.release))
			continue
		}
		r.next = k + 1
		dues = append(dues, due{run: r, cmp: *r, rule: rule, elapsed: elapsed})
	}
	store, logger := c.store, c.logger
	c.mu.Unlock()

	for _, d := range dues {
		cctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
		v := checkRelease(cctx, store, d.cmp, d.rule, d.elapsed)
		cancel()
		if v.err != nil {
			logger.Debug("monitor rules: the post-deploy check is skipped: the metrics store did not answer",
				zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Error(v.err))
			continue
		}
		var ev *Event
		c.mu.Lock()
		if c.runs[d.cmp.app] == d.run { // not ended or replaced meanwhile
			switch {
			case v.skipped:
			case !v.failed:
				d.run.failed = 0
			default:
				d.run.failed++
				if d.run.failed >= d.rule.failures() {
					delete(c.runs, d.cmp.app)
					e := regressedEvent(d.cmp, d.rule, d.elapsed, v, d.run.failed)
					ev = &e
				}
			}
		}
		c.mu.Unlock()
		if ev != nil {
			c.report(*ev)
		}
	}
}

func sortedKeys(m map[string]*comparison) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// releaseNumbers are one release's over a window.
type releaseNumbers struct {
	sent, errors float64
	p95          float64
	hasP95       bool
}

func (n releaseNumbers) share() float64 {
	if n.sent == 0 {
		return 0
	}
	return n.errors / n.sent
}

// verdict is one check's outcome.
type verdict struct {
	err        error
	skipped    bool
	failed     bool
	newer, old releaseNumbers
	hasOld     bool
}

// checkRelease is one check: the new release over elapsed since the
// cutover, and, when it is over a bound, the old one over the same
// length before it; both read at cutover + elapsed, so the windows meet
// at the cutover.
func checkRelease(ctx context.Context, store instantQuerier, r comparison, rule Rule, elapsed time.Duration) verdict {
	var v verdict
	wantP95 := rule.MaxP95 != 0
	floor := float64(rule.minRequests())
	if v.newer, v.err = queryRelease(ctx, store, r.app, r.release, r.cutover.Add(elapsed), elapsed, false, wantP95); v.err != nil {
		return v
	}
	if v.newer.sent < floor {
		v.skipped = true
		return v
	}
	over5xx := rule.Max5xxShare != nil && v.newer.share() > *rule.Max5xxShare
	overP95 := wantP95 && v.newer.hasP95 && v.newer.p95 > time.Duration(rule.MaxP95).Seconds()
	if !over5xx && !overP95 {
		return v
	}
	if r.previous != "" {
		if v.old, v.err = queryRelease(ctx, store, r.app, r.previous, r.cutover.Add(elapsed), elapsed, true, wantP95); v.err != nil {
			return v
		}
		// An old release with fewer responses than the floor is no
		// baseline: only the bounds count.
		v.hasOld = v.old.sent >= floor
	}
	fail5xx := over5xx && (!v.hasOld || v.newer.share() > v.old.share())
	failP95 := overP95 && (!v.hasOld || !v.old.hasP95 || v.newer.p95 > v.old.p95*p95Margin)
	v.failed = fail5xx || failP95
	return v
}

// queryRelease asks the store, at at, for a release's responses sent,
// its 5xx and its p95 over the elapsed time: up to at (the new
// release's since its cutover) or up to the cutover (old: offset by the
// same length).
// increase_pure, never increase: the new release's series appears with
// its first push already at that push's count.
func queryRelease(ctx context.Context, store instantQuerier, app, release string, at time.Time, elapsed time.Duration, old, wantP95 bool) (releaseNumbers, error) {
	var n releaseNumbers
	win := "[" + strconv.Itoa(int(elapsed.Seconds())) + "s]"
	if old {
		win += " offset " + strconv.Itoa(int(elapsed.Seconds())) + "s"
	}
	sel := "{app=" + strconv.Quote(app) + ",release=" + strconv.Quote(release) + "}"
	classes, err := store.queryAt(ctx, "sum by (class) (increase_pure(motel_app_requests_total"+sel+win+"))", at)
	if err != nil {
		return n, err
	}
	for _, s := range classes {
		val, ok := instantValue(s)
		if !ok {
			continue
		}
		switch s.labels["class"] {
		case "aborted":
		case "5xx":
			n.errors += val
			n.sent += val
		default:
			n.sent += val
		}
	}
	if !wantP95 || n.sent == 0 {
		return n, nil
	}
	q, err := store.queryAt(ctx, "histogram_quantile(0.95, sum by (le) (increase_pure(motel_app_response_seconds_bucket"+sel+win+")))", at)
	if err != nil {
		return n, err
	}
	for _, s := range q {
		if val, ok := instantValue(s); ok {
			n.p95, n.hasP95 = val, true
			break
		}
	}
	return n, nil
}

// regressedEvent is the one event a comparison tells: the app's, so it
// reaches the operator and the app's bound channels, with both
// releases' numbers. The release before is not named in the summary:
// the filter keeps the event's own release whole, and would mask
// another git sha.
func regressedEvent(r comparison, rule Rule, elapsed time.Duration, v verdict, failed int) Event {
	var parts []string
	if rule.Max5xxShare != nil {
		p := "5xx share " + shareText(v.newer.share())
		if v.hasOld {
			p += " against " + shareText(v.old.share()) + " before it"
		}
		parts = append(parts, p)
	}
	if rule.MaxP95 != 0 && v.newer.hasP95 {
		p := "p95 " + msText(v.newer.p95)
		if v.hasOld && v.old.hasP95 {
			p += " against " + msText(v.old.p95) + " before it"
		}
		parts = append(parts, p)
	}
	baseline := ""
	if !v.hasOld {
		baseline = "; no release before it to compare"
	}
	checks := "check"
	if failed > 1 {
		checks = "checks in a row"
	}
	summary := fmt.Sprintf("%s %s regressed %s after its deploy, over %d responses: %s%s; %d failing %s (rule %s)",
		r.app, r.release, promDuration(elapsed), int(v.newer.sent), strings.Join(parts, ", "), baseline, failed, checks, rule.Name)
	return Event{
		Name: eventPostDeployRegressed, Kind: kindEvent, Severity: SeverityWarning, App: r.app, Source: sourceRules,
		Release: r.release, DeployedBy: r.deployedBy, Summary: summary,
		Details: map[string]string{"rule": rule.Name},
	}
}

// shareText is a share to three significant digits, at most four
// decimals: 0.0833.
func shareText(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64)
}

// msText is seconds in whole milliseconds: 1200ms.
func msText(v float64) string { return strconv.Itoa(int(math.Round(v*1e3))) + "ms" }

// run is the comparator's goroutine.
func (c *comparator) run(ctx context.Context, every time.Duration) {
	defer close(c.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// configure points the comparator at a config and starts its goroutine
// the first time. Called from MetricsStore.start.
func (c *comparator) configure(l liveRules) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = slices.DeleteFunc(c.live, func(x liveRules) bool { return x.m == l.m })
	c.live = append(c.live, l)
	c.active, c.store, c.logger = l.m, l.store, l.logger
	if !c.running {
		ctx, cancel := context.WithCancel(context.Background())
		c.running, c.cancel, c.done = true, cancel, make(chan struct{})
		go c.run(ctx, c.every)
	}
}

// release is the monitor app's Stop: as the pusher, it goes back to an
// older config still live (a reload that failed after Start), and with
// none it stops, dropping what is in flight: nothing would tell it.
func (c *comparator) release(m *MetricsStore) {
	c.mu.Lock()
	c.live = slices.DeleteFunc(c.live, func(x liveRules) bool { return x.m == m })
	if c.active != m {
		c.mu.Unlock()
		return
	}
	if n := len(c.live); n > 0 {
		older := c.live[n-1]
		c.active, c.store, c.logger = older.m, older.store, older.logger
		c.mu.Unlock()
		return
	}
	c.active, c.store = nil, nil
	c.runs = map[string]*comparison{}
	cancel, done, running := c.cancel, c.done, c.running
	c.running = false
	c.mu.Unlock()
	if running {
		cancel()
		<-done
	}
}
