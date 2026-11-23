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
	// checkFresh is how recent a push of motel's the store must hold at
	// a check's time for the check to count (pushedQuery). The store
	// stamps a push as it takes it and back-fills none it missed, so a
	// check read past the store's last push reads the counts as they
	// stood then: down from minute 1.5 to 5, it answered minutes 2 and
	// 3, asked again once it was back, with one snapshot, and a warning
	// counted three failing checks over a minute and a half of
	// responses; down over the window's end, the checks passed on it and
	// the comparison ended as if the release held. Derived:
	//   - the check runs storeLag (10 s) after its time, past the latency
	//     offset queryAt asks for (5 s), so the store reads at the time
	//     as given and has every push it stamped by then searchable: a
	//     push missing then is missing for good, but for a late
	//     ingestion, which a later try finds;
	//   - within the minute before its time, the push is one the check a
	//     minute before could not read (the window leaves its start
	//     out), so no two checks count one snapshot, and the first
	//     reads a push from after the cutover;
	//   - a minute holds the pushes: one is gathered every pushEvery
	//     (30 s) and stamped as the store takes it, milliseconds later
	//     over the socket, so the last before a time is about 30 s old,
	//     and with one lost about 60 s: over it only by as much as the
	//     push after the lost one was stamped slower than the one
	//     before it.
	// A check with no such push whose store has taken one since (asked
	// the same at the tick's time less storeLag, once that is a minute
	// past the check's time, so a push in the minute before it is after
	// the check's time) is missed: none will come
	// for it. It is skipped, and the comparison goes on, its later
	// checks reading the missed minute's responses (the windows are
	// cumulative); the window's last ends it unfinished, the minutes
	// after the store's last push for it never read. With none since
	// either, the store takes no push, or shows none yet: the store's
	// error, asked again at the next tick, and past the grace the
	// comparison ends unfinished.
	checkFresh = postDeployEvery
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
	// store did not answer, or held no push for (checkFresh), is asked
	// again before the comparison ends unfinished.
	postDeployGrace = 10 * time.Minute
)

// pushedQuery is whether the store holds a push of motel's within
// checkFresh before the query's time: one series, or none.
var pushedQuery = "max(present_over_time(" + pushSeries + "[" + promDuration(checkFresh) + "]))"

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
	// oldEnd is the first push of motel's the store took after the
	// cutover, where the old release's window ends (oldWindow): zero
	// until a check first reads that window.
	oldEnd time.Time
	// judged, skipped and missed count the checks made: judged when a
	// bound was weighed, skipped under min_requests (responses sent, or
	// timed for a rule whose only bound is max_p95), missed for a push
	// the store never took; last is the last judged one's, and
	// skippedLast the new release's numbers at the last skipped one.
	// Read by the line at its end, which counts the checks finished by
	// then.
	judged, skipped, missed int
	last                    checkNumbers
	skippedLast             releaseNumbers
}

// checkNumbers are one judged check's, for the line at a comparison's
// end.
type checkNumbers struct {
	elapsed    time.Duration
	newer, old releaseNumbers
	// oldRead is whether the check read the old release, hasOld
	// whether that was a baseline (verdict).
	oldRead, hasOld bool
}

// countFields are a comparison's checks so far, for a line at its end.
func (r *comparison) countFields() []zap.Field {
	return []zap.Field{zap.Int("checks_judged", r.judged), zap.Int("checks_skipped", r.skipped), zap.Int("checks_missed", r.missed)}
}

// numberFields are a judged check's numbers: the new release's
// responses, 5xx share and p95; the old release's responses when the
// check read them (only when the new one was over a bound and there
// was a release before it), and its share and p95 only when they were
// a baseline, as in the event. A share to 1/10 000, a p95 to the
// millisecond.
func numberFields(n checkNumbers) []zap.Field {
	f := []zap.Field{zap.Duration("last_check", n.elapsed), zap.Int("responses", int(n.newer.sent)), zap.Float64("5xx_share", roundShare(n.newer.share()))}
	if n.newer.hasP95 {
		f = append(f, zap.Duration("p95", roundMs(n.newer.p95)))
	}
	if n.oldRead {
		f = append(f, zap.Int("previous_responses", int(n.old.sent)))
	}
	if n.hasOld {
		f = append(f, zap.Float64("previous_5xx_share", roundShare(n.old.share())))
		if n.old.hasP95 {
			f = append(f, zap.Duration("previous_p95", roundMs(n.old.p95)))
		}
	}
	return f
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
// deploys: the app's own, else the every-app one. Also what `moteld
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
	if r, ok := c.runs[ev.App]; ok {
		delete(c.runs, ev.App)
		c.logger.Info("monitor rules: a new deploy ends the post-deploy comparison in flight",
			append([]zap.Field{zap.String("app", ev.App), zap.String("release", r.release), zap.String("new_release", ev.Release)}, r.countFields()...)...)
	}
	rule, ok := c.ruleFor(ev.App)
	if !ok {
		return
	}
	cut := ev.Cutover
	if cut.IsZero() {
		cut = c.now()
	}
	r := &comparison{app: ev.App, release: ev.Release, previous: ev.Previous, deployedBy: ev.DeployedBy, cutover: cut, next: 1, relaunch: ev.Relaunch && ev.Previous == ""}
	c.runs[ev.App] = r
	// Said once, so that a comparison that holds is seen to have run:
	// the line at its end says how (#511).
	f := []zap.Field{zap.String("app", r.app), zap.String("release", r.release)}
	if r.previous != "" {
		f = append(f, zap.String("previous", r.previous))
	}
	f = append(f, zap.Bool("relaunch", r.relaunch), zap.String("rule", rule.Name), zap.Duration("window", rule.window()))
	if rule.Max5xxShare != nil {
		f = append(f, zap.Float64("max_5xx_share", *rule.Max5xxShare))
	}
	if rule.MaxP95 != 0 {
		f = append(f, zap.Duration("max_p95", time.Duration(rule.MaxP95)))
	}
	f = append(f, zap.Int("min_requests", rule.minRequests()), zap.Int("failures", rule.failures()))
	c.logger.Info("monitor rules: the post-deploy comparison starts", f...)
}

// due is one check to make: over elapsed from the cutover, read at
// cutover + elapsed.
type due struct {
	run     *comparison
	cmp     comparison // the run as it stood, for the queries
	rule    Rule
	elapsed time.Duration
	now     time.Time // the tick's
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
			delete(c.runs, app)
			c.logger.Info("monitor rules: the post-deploy comparison ended: the config loaded last has no post_deploy rule for the app",
				append([]zap.Field{zap.String("app", app), zap.String("release", r.release)}, r.countFields()...)...)
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
			delete(c.runs, app) // the window is over
			f := append([]zap.Field{zap.String("app", app), zap.String("release", r.release)}, r.countFields()...)
			if r.judged == 0 {
				// Not a release that held: every check was under
				// min_requests, or missed.
				f = append(f, zap.Int("min_requests", rule.minRequests()), zap.Int("responses", int(r.skippedLast.sent)))
				if rule.MaxP95 != 0 {
					f = append(f, zap.Int("responses_timed", int(r.skippedLast.timed)))
				}
				c.logger.Info("monitor rules: the post-deploy comparison ended with its window having judged no check: each was under min_requests, or missed", f...)
				continue
			}
			msg := "monitor rules: the post-deploy comparison ended with its window: the release held"
			if r.failed > 0 {
				msg = "monitor rules: the post-deploy comparison ended with its window: the release held, its last judged checks failing, fewer in a row than failures"
			}
			c.logger.Info(msg, append(append(f, zap.Int("failing_in_a_row", r.failed)), numberFields(r.last)...)...)
			continue
		}
		k := r.next
		elapsed := time.Duration(k) * postDeployEvery
		r.next = k + 1
		dues = append(dues, due{run: r, cmp: *r, rule: rule, elapsed: elapsed, now: now})
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
	v := checkRelease(cctx, store, d.cmp, d.rule, d.elapsed, d.now)
	cancel()
	var ev *Event
	c.mu.Lock()
	if c.runs[d.cmp.app] == d.run { // not ended or replaced meanwhile
		if !v.oldEnd.IsZero() {
			d.run.oldEnd = v.oldEnd
		}
		switch {
		case v.err != nil:
			// Asked again at the next tick: a check skipped would leave
			// the count short of failures equal to the window's minutes.
			// Past the grace the comparison ends, said where it is seen.
			if c.now().Sub(d.cmp.cutover.Add(d.rule.window()+storeLag)) > postDeployGrace {
				delete(c.runs, d.cmp.app)
				logger.Warn("monitor rules: the post-deploy comparison ended unfinished: the metrics store did not answer its checks, or held no push of motel's for them",
					append([]zap.Field{zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Int("failing_in_a_row", d.run.failed), zap.Error(v.err)}, d.run.countFields()...)...)
				break
			}
			d.run.next = int(d.elapsed / postDeployEvery)
			logger.Debug("monitor rules: the post-deploy check is asked again: the metrics store did not answer, or held no push of motel's for it",
				zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Error(v.err))
		case v.missed && d.elapsed >= d.rule.window():
			delete(c.runs, d.cmp.app)
			d.run.missed++
			logger.Warn("monitor rules: the post-deploy comparison ended unfinished: the metrics store took no push of motel's for its last check",
				append([]zap.Field{zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Int("failing_in_a_row", d.run.failed), zap.Duration("elapsed", d.elapsed)}, d.run.countFields()...)...)
		case v.missed:
			d.run.missed++
			logger.Debug("monitor rules: the post-deploy check is skipped: the metrics store took no push of motel's for it",
				zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Duration("elapsed", d.elapsed))
		case v.skipped:
			d.run.skipped++
			d.run.skippedLast = v.newer
		default:
			if d.rule.Max5xxShare != nil || v.newer.hasP95 {
				d.run.judged++
				d.run.last = checkNumbers{elapsed: d.elapsed, newer: v.newer, old: v.old, oldRead: v.oldRead, hasOld: v.hasOld}
			} else {
				// Sent enough, timed too few for a p95, and no 5xx bound:
				// no bound weighed, so not judged, though it passes as
				// before.
				d.run.skipped++
				d.run.skippedLast = v.newer
			}
			if !v.failed {
				d.run.failed = 0
				break
			}
			d.run.failed++
			if d.run.failed >= d.rule.failures() {
				delete(c.runs, d.cmp.app)
				e := regressedEvent(d.cmp, d.rule, d.elapsed, v, d.run.failed)
				ev = &e
				logger.Info("monitor rules: the post-deploy comparison ended: the release failed its checks, reported as post_deploy_regressed",
					append(append([]zap.Field{zap.String("app", d.cmp.app), zap.String("release", d.cmp.release), zap.Int("failing_in_a_row", d.run.failed)}, d.run.countFields()...), numberFields(d.run.last)...)...)
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
	err     error
	skipped bool
	// missed is a check the store took no push for, and will take none:
	// it has taken one since (checkFresh).
	missed     bool
	failed     bool
	newer, old releaseNumbers
	hasOld     bool
	// oldEnd is the end of the old release's window, when this check
	// asked for it (comparison.oldEnd).
	oldEnd time.Time
	// oldRead is whether the check read the old release (old): only
	// when the new one was over a bound.
	oldRead bool
}

// checkRelease is one check: the new release over elapsed since the
// cutover, and, when it is over a bound, the old one over the same
// length before it, to the first push the store took after the
// cutover (oldWindow); both read at cutover + elapsed. First, once a
// check (at its time), whether the store took a push of motel's in the
// minute before then (checkFresh), and with none whether it has taken
// one since (freshness; now: the tick's time): asked before the floor
// too, so that a skip, like a verdict, is drawn from
// counts pushed within that minute. It is motel's push, not the app's
// series, so a quiet app's check is no more often without one than a
// busy app's.
func checkRelease(ctx context.Context, store instantQuerier, r comparison, rule Rule, elapsed time.Duration, now time.Time) verdict {
	var v verdict
	wantP95 := rule.MaxP95 != 0
	floor := float64(rule.minRequests())
	at := r.cutover.Add(elapsed)
	if v.missed, v.err = freshness(ctx, store, at, elapsed, now); v.err != nil || v.missed {
		return v
	}
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
		// Where the old release's window ends is asked once a
		// comparison, when it is first read, and kept (check).
		end := r.oldEnd
		if end.IsZero() {
			var ok bool
			if end, ok, v.err = firstPushAfter(ctx, store, r.cutover, at); v.err != nil {
				return v
			}
			if !ok {
				// The check's own push is one stamped within
				// storeRequestTimeout of the cutover (the first minute's
				// alone can be), and the next was lost, or is not
				// searchable yet: asked again, as freshness asks, until a
				// minute past the check's time, then missed.
				if now.Add(-storeLag).Before(at.Add(checkFresh)) {
					v.err = fmt.Errorf("the metrics store shows no push of motel's gathered after the cutover yet, for the check %s after it", promDuration(elapsed))
				} else {
					v.missed = true
				}
				return v
			}
			v.oldEnd = end
		}
		win, off := oldWindow(elapsed, at, end)
		if v.old, v.err = queryRelease(ctx, store, r.app, r.previous, at, win, off, wantP95, floor); v.err != nil {
			return v
		}
		v.oldRead = true
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

// freshness is whether the check over elapsed, at at, may count: with
// a push of motel's within checkFresh before at, it may; with none, and
// one taken since, it is missed; with neither, the store's error, asked
// again. Since is asked at now less storeLag only once that is checkFresh
// past at, so the minute it reads is wholly after at: a push stamped
// before at that the store made searchable between the two asks is not
// taken for one after it (/code-review pass 2 of the push ask).
func freshness(ctx context.Context, store instantQuerier, at time.Time, elapsed time.Duration, now time.Time) (missed bool, err error) {
	ok, err := pushedBefore(ctx, store, at)
	if err != nil || ok {
		return false, err
	}
	if since := now.Add(-storeLag); !since.Before(at.Add(checkFresh)) {
		if ok, err = pushedBefore(ctx, store, since); err != nil || ok {
			return ok, err
		}
	}
	return false, fmt.Errorf("the metrics store holds no push of motel's from the minute before the check %s after the cutover, nor one since yet: it is down or read-only, or motel is not pushing", promDuration(elapsed))
}

// pushedBefore is whether the store holds a push of motel's within
// checkFresh before t.
func pushedBefore(ctx context.Context, store instantQuerier, t time.Time) (bool, error) {
	ss, err := store.queryAt(ctx, pushedQuery, t)
	if err != nil || len(ss) == 0 {
		return false, err
	}
	_, ok := instantValue(ss[0])
	return ok, nil
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
// length before the cutover and on to the first push after it
// (oldWindow). The p95 is asked only once
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

// firstPushQuery is firstPushAfter's ask, at the check's time at: the
// time of the first push of motel's the store took more than
// storeRequestTimeout after the cutover: the store's window leaves its
// start out, so a push stamped exactly that past it is not one
// (TestPostDeployTheBaselineOverTheStore).
func firstPushQuery(cutover, at time.Time) string {
	return "min(tfirst_over_time(" + pushSeries + "[" + promDuration(at.Sub(cutover)-storeRequestTimeout) + "]))"
}

// firstPushAfter is the first push of motel's the store took after the
// cutover that was gathered after it, asked at the check's time at, by
// then searchable (storeLag): one stamped more than storeRequestTimeout
// after the cutover, since the store stamps an import within its
// request's budget (relaunchSkip). One stamped sooner may have been
// gathered before the cutover, without the old release's last answers
// (/code-review pass 2 of the old window's end). ok is whether there is
// one.
func firstPushAfter(ctx context.Context, store instantQuerier, cutover, at time.Time) (end time.Time, ok bool, err error) {
	ss, err := store.queryAt(ctx, firstPushQuery(cutover, at), at)
	if err != nil || len(ss) == 0 {
		return time.Time{}, false, err
	}
	v, ok := instantValue(ss[0])
	if !ok {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(int64(math.Round(v * 1e3))), true, nil
}

// oldWindow is the window a check over elapsed, at at, reads of the old
// release, and its offset: from elapsed before the cutover to end, the
// first push the store took after the cutover and gathered after it
// (firstPushAfter), and within a second past it, as an offset is whole
// seconds. Its answers in the last half minute before the cutover
// reach the store with that push, and one push's samples are stamped
// milliseconds apart, the
// request series before motel's own (pushedNow). The store back-fills
// no push it missed, so a window that ended a push past the cutover
// read, with the store down over that push and back by the check, the
// counts as they stood at the old release's last push before the
// outage: a baseline as short as the outage was long before the
// cutover, or none. Every push after the cutover carries the old
// release's final counts while the comparison lasts (liveswap's
// requestCounts keeps it beside the current release until the next
// deploy, and a moteld restart ends the comparison). With no outage that
// push is the first or the second after the cutover, within
// storeRequestTimeout + pushEvery of it. Not to the check's time: the
// old instance drains
// past the cutover, and a long drain's answers, and the errors of the
// requests its stop cuts, would be read as its baseline (/code-review
// of this window).
func oldWindow(elapsed time.Duration, at, end time.Time) (win, off time.Duration) {
	off = at.Sub(end) / time.Second * time.Second
	return 2*elapsed - off, off
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
	return strconv.FormatFloat(roundShare(v), 'f', -1, 64)
}

// roundShare is a share to 1/10 000.
func roundShare(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// msText is seconds in whole milliseconds: 1200ms.
func msText(v float64) string { return strconv.Itoa(int(math.Round(v*1e3))) + "ms" }

// roundMs is seconds as a duration in whole milliseconds.
func roundMs(v float64) time.Duration { return time.Duration(math.Round(v*1e3)) * time.Millisecond }

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
