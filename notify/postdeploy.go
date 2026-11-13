package notify

import (
	"context"
	"fmt"
	"maps"
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
	// past its latency offset (storeLatencyOffset, which queryAt asks for), within which the store
	// shifts an instant query's time back by the offset, and so both
	// windows away from the cutover (measured in e2e metrics 10: the
	// old release's last half minute read as nothing).
	storeLag = 10 * time.Second
	// p95Margin is how much slower than the old release the new one's
	// p95 must be, over its bound, to fail a check: a quantile is an
	// estimate inside a bucket.
	p95Margin = 1.2
	// shareMargin and shareZ are how much higher than an old release
	// already over max_5xx_share the new one's share must be: 1.2 times
	// it, as for p95, and further from it than two samples of one rate
	// fall by chance (a two-proportion z-score of 2), so an app that
	// errs steadily is not told at every deploy.
	shareMargin = 1.2
	shareZ      = 2
	// relaunchSkip is how long after the cutover a relaunch after a
	// stop's window starts. Its release's counters are the ones the
	// stopped instance counted into, and its last answers before the
	// cutover (a crash loop's 503s among them: a request that reached
	// no upstream counts under the release current when it began) reach
	// the store with the first push after it, which a window from the
	// cutover would read as the relaunch's in every check (the windows
	// are cumulative). That push is gathered within a push of the
	// cutover and stamped by the store within its request's budget, so
	// from relaunchSkip on the window's base is a sample that holds
	// them. The relaunched instance's answers before it are not read: a
	// first check of 20 s, and every one 40 s short.
	relaunchSkip = pushEvery + storeRequestTimeout
	// postDeployGrace is how long past the window's end a check the
	// store did not answer is asked again before the comparison ends
	// unfinished.
	postDeployGrace = 10 * time.Minute
)

// instantQuerier is the store's instant query at a time.
type instantQuerier interface {
	queryAt(ctx context.Context, q string, t time.Time) ([]series, error)
}

// comparison is one deploy's, in flight.
type comparison struct {
	app, release, previous, deployedBy string
	cutover                            time.Time
	// relaunch is a cutover to the release the app last ran, whose
	// instance had stopped (LifecycleEvent.Relaunch with no Previous):
	// its bound alone counts, over a window from relaunchSkip after the
	// cutover, and its event says relaunch, not regressed.
	relaunch bool
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
	return postDeployRuleFor(c.active, app)
}

// postDeployRuleFor is the post_deploy rule of m that judges app's
// deploys: the app's own, else the every-app one. Also what `motel
// monitor silence add --alert post_deploy_regressed --app` holds to.
func postDeployRuleFor(m *MetricsStore, app string) (Rule, bool) {
	if m == nil {
		return Rule{}, false
	}
	var every *Rule
	for i, r := range m.Rules {
		if r.Kind != rulePostDeploy {
			continue
		}
		if r.App == app {
			return r, true
		}
		if r.App == "" {
			every = &m.Rules[i]
		}
	}
	if every != nil {
		return *every, true
	}
	return Rule{}, false
}

// deployed starts a comparison at a deploy's or a rollback's cutover,
// which liveswap tells through lifecycleSink at the swap itself, before
// the old instance's drain and stop and the deploy's result. A deploy of
// an app with one in flight ends it: the release it compared no longer
// serves.
func (c *comparator) deployed(ev liveswap.LifecycleEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || ev.App == "" {
		return
	}
	if ev.Previous != "" && ev.Previous == ev.Release {
		// A relaunch of the release serving is no new release, and the
		// two instances count into the release's one set of counters,
		// so no window could tell them apart: no comparison starts, and
		// one in flight for that release goes on, its windows as they
		// were (Copilot round 2 on #429). liveswap marks it Relaunch,
		// but the names alone decide here: a cutover to the release
		// serving without the mark would compare the release with
		// itself. A relaunch whose instance had stopped (the mark, no
		// Previous) is compared with the bound alone, as a relaunch
		// (the operator's decision of 2026-11-12).
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
	c.runs[ev.App] = &comparison{app: ev.App, release: ev.Release, previous: ev.Previous, deployedBy: ev.DeployedBy, cutover: cut, next: 1, relaunch: ev.Relaunch && ev.Previous == ""}
}

// due is one check to make: over elapsed from the cutover, read at
// cutover + elapsed.
type due struct {
	run     *comparison
	cmp     comparison // the run as it stood, for the queries
	rule    Rule
	elapsed time.Duration
}

// tick makes every check due, outside the lock.
func (c *comparator) tick(ctx context.Context) {
	c.mu.Lock()
	if c.active == nil {
		c.mu.Unlock()
		return
	}
	now := c.now()
	var dues []due
	for _, app := range slices.Sorted(maps.Keys(c.runs)) {
		r := c.runs[app]
		rule, ok := c.ruleFor(app)
		if !ok {
			delete(c.runs, app) // the config serving has no rule for it any more
			continue
		}
		// The next minute owed, once the store can answer for it: one a
		// stalled tick or a store that did not answer left behind is
		// made late, read at its own time, one a tick, so that with
		// failures equal to the window's minutes the regression's count
		// can still be reached.
		latest := int(now.Sub(r.cutover.Add(storeLag)) / postDeployEvery)
		if latest < r.next {
			continue
		}
		if last := int(rule.window() / postDeployEvery); r.next > last {
			delete(c.runs, app) // the window is over: the release held
			c.logger.Debug("monitor rules: the post-deploy comparison ended with its window", zap.String("app", app), zap.String("release", r.release))
			continue
		}
		k := r.next
		elapsed := time.Duration(k) * postDeployEvery
		r.next = k + 1
		dues = append(dues, due{run: r, cmp: *r, rule: rule, elapsed: elapsed})
	}
	store, logger := c.store, c.logger
	c.mu.Unlock()

	// At once, each within its own budget: one after another, a slow
	// store would stretch a tick past the minute between checks, and
	// the window could end before a regression's count is reached.
	var wg sync.WaitGroup
	for _, d := range dues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.check(ctx, store, logger, d)
		}()
	}
	wg.Wait()
}

// check makes one due check and counts its outcome.
func (c *comparator) check(ctx context.Context, store instantQuerier, logger *zap.Logger, d due) {
	cctx, cancel := context.WithTimeout(ctx, storeRequestTimeout)
	v := checkRelease(cctx, store, d.cmp, d.rule, d.elapsed)
	cancel()
	var ev *Event
	c.mu.Lock()
	if c.runs[d.cmp.app] == d.run { // not ended or replaced meanwhile
		switch {
		case v.err != nil:
			// Asked again at the next tick: a check skipped would leave
			// the count short of failures equal to the window's minutes.
			// Past the grace the comparison ends, said where it is seen.
			if c.now().Sub(d.cmp.cutover.Add(d.rule.window()+storeLag)) > postDeployGrace {
				delete(c.runs, d.cmp.app)
				logger.Warn("monitor rules: the post-deploy comparison ended unfinished: the metrics store did not answer its checks",
					zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Int("failing_in_a_row", d.run.failed), zap.Error(v.err))
				break
			}
			d.run.next = int(d.elapsed / postDeployEvery)
			logger.Debug("monitor rules: the post-deploy check is asked again: the metrics store did not answer",
				zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Error(v.err))
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

// releaseNumbers are one release's over a window. The p95 is of the
// responses timed (the histogram's count: a cache hit is sent and never
// timed), and there is one only with min_requests of them; timed is
// that count, read when the p95 is wanted.
type releaseNumbers struct {
	sent, errors float64
	timed        float64
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
	at := r.cutover.Add(elapsed)
	if v.newer, v.err = queryRelease(ctx, store, r.app, r.release, at, newWindow(r, elapsed), 0, wantP95, floor); v.err != nil {
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
		// The old release's window ends a push after the cutover: its
		// answers in the last half minute before it reach the store
		// with the first push after it.
		if v.old, v.err = queryRelease(ctx, store, r.app, r.previous, at, elapsed+pushEvery, elapsed-pushEvery, wantP95, floor); v.err != nil {
			return v
		}
		// An old release with fewer responses than the floor is no
		// baseline: only the bounds count.
		v.hasOld = v.old.sent >= floor
	}
	// An old release within the bound makes crossing it the regression;
	// one already over it, a rise beyond chance.
	fail5xx := over5xx && (!v.hasOld || v.old.share() <= *rule.Max5xxShare || shareRose(v.newer, v.old))
	failP95 := overP95 && (!v.hasOld || !v.old.hasP95 || v.newer.p95 > v.old.p95*p95Margin)
	v.failed = fail5xx || failP95
	return v
}

// shareRose is whether the new release's 5xx share rose from the old
// one's by more than shareMargin and more than chance (shareZ).
func shareRose(newer, old releaseNumbers) bool {
	pn, po := newer.share(), old.share()
	if pn <= po*shareMargin {
		return false
	}
	p := (newer.errors + old.errors) / (newer.sent + old.sent)
	se := math.Sqrt(p * (1 - p) * (1/newer.sent + 1/old.sent))
	return se == 0 || (pn-po)/se >= shareZ
}

// queryRelease asks the store, at at, for a release's responses sent,
// its 5xx and its p95 over win, ending off before at: the new release's
// since its cutover (newWindow), or the old release's over the same
// length before the cutover and a push past it. The p95 is asked only once
// floor responses were timed: min_requests counts the responses sent,
// cache hits among them, and on a cached site a few misses (one of
// them a cold start) would otherwise make the quantile, in every
// check of the cumulative window after it.
// The counters through counterIncrease, the buckets through
// histogramIncrease: the new release's series appears with its first
// push already at that push's count.
func queryRelease(ctx context.Context, store instantQuerier, app, release string, at time.Time, win, off time.Duration, wantP95 bool, floor float64) (releaseNumbers, error) {
	var n releaseNumbers
	sel := releaseSelector(app, release)
	inc := func(metric string) string { return counterIncrease(metric+sel, win, off) }
	buckets := histogramIncrease("motel_app_response_seconds", sel, win, off)
	classes, err := store.queryAt(ctx, classesQuery(sel, win, off), at)
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
	timed, err := store.queryAt(ctx, "sum("+inc("motel_app_response_seconds_count")+")", at)
	if err != nil {
		return n, err
	}
	if len(timed) == 0 {
		return n, nil
	}
	val, ok := instantValue(timed[0])
	if !ok {
		return n, nil
	}
	if n.timed = val; val < floor {
		return n, nil
	}
	q, err := store.queryAt(ctx, "histogram_quantile(0.95, sum by (le) ("+buckets+"))", at)
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

// newWindow is the window a check over elapsed reads of the new
// release, up to the check: from the cutover, and for a relaunch after
// a stop from relaunchSkip after it.
func newWindow(r comparison, elapsed time.Duration) time.Duration {
	if r.relaunch {
		return elapsed - relaunchSkip
	}
	return elapsed
}

// releaseSelector is a release's series of an app.
func releaseSelector(app, release string) string {
	return "{app=" + strconv.Quote(app) + ",release=" + strconv.Quote(release) + "}"
}

// classesQuery is a release's responses by class (sel: its selector)
// over win, ending off before the query's time.
func classesQuery(sel string, win, off time.Duration) string {
	return "sum by (class) (" + counterIncrease("motel_app_requests_total"+sel, win, off) + ")"
}

// regressedEvent is the one event a comparison tells: the app's, so it
// reaches the operator and the app's bound channels, with both
// releases' numbers. The release before is not named in the summary:
// the filter keeps the event's own release whole, and would mask
// another git sha. A relaunch of a release whose instance had stopped
// is said as one: over its bound after its relaunch, not regressed
// after its deploy, and the parenthesis is why there is no baseline
// (the operator's decision of 2026-11-12).
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
	// Why the bound alone counted: no release before, too few sent, or,
	// for a p95 over its bound, too few of the old release's responses
	// timed (a cache hit is sent and never timed).
	overP95 := rule.MaxP95 != 0 && v.newer.hasP95 && v.newer.p95 > time.Duration(rule.MaxP95).Seconds()
	baseline := ""
	switch {
	case r.relaunch: // said by the parenthesis below
	case r.previous == "":
		baseline = "; no release before it to compare"
	case !v.hasOld: // under the floor: no baseline
		baseline = fmt.Sprintf("; the release before it sent %d responses, too few to compare", int(v.old.sent))
	case overP95 && !v.old.hasP95 && v.old.timed < float64(rule.minRequests()):
		baseline = fmt.Sprintf("; the release before it had %d responses timed, too few to compare its p95", int(v.old.timed))
	case overP95 && !v.old.hasP95:
		baseline = "; the release before it has no p95 to compare"
	}
	checks := "check"
	if failed > 1 {
		checks = "checks in a row"
	}
	what := "regressed " + promDuration(elapsed) + " after its deploy"
	if r.relaunch {
		what = "is over its bound " + promDuration(elapsed) + " after its relaunch (its earlier instance had stopped)"
	}
	summary := fmt.Sprintf("%s %s %s, over %d responses: %s%s; %d failing %s (rule %s)",
		r.app, r.release, what, int(v.newer.sent), strings.Join(parts, ", "), baseline, failed, checks, rule.Name)
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
