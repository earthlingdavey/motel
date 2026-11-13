package liveswap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

// The watchdog is the continuous half of supervision: deploys and boot
// recovery start instances, the watchdog keeps them running. One
// goroutine per pooled app watches the current instance and restarts
// the same version when the process exits or when the health endpoint
// fails wdFailures consecutive probes — with a per-restart grace
// period, exponential backoff, and a restart budget. The watchdog
// never gives up on a failure a retry can fix (a relaunch its
// pre-flight refuses is not one): when the budget for the current window is spent it
// throttles — waits for the oldest restart to slide out of the window,
// then tries again — so an app that died during a transient incident
// (a DDoS, a dependency outage) comes back on its own once the
// incident ends, with nobody at the keyboard. The budget is what
// keeps that persistence safe: it bounds the restart rate, so a crash
// loop (or an attacker who can induce health failures) costs at most
// wdRestarts restarts per wdWindow, not an unbounded kill loop.

// Watchdog states reported via the status endpoint.
const (
	wdStateDisabled  = "disabled"
	wdStateIdle      = "idle"
	wdStateGrace     = "grace"
	wdStateWatching  = "watching"
	wdStateBackoff   = "backoff"
	wdStateThrottled = "throttled"
)

// Backoff policy between watchdog restarts, deliberately not
// configurable: 1s doubling to a 60s cap, with a ±20% jitter factor so
// several apps sharing a failed dependency do not restart in lockstep.
// The step resets only after the app has been continuously healthy for
// max(wdBackoffResetAfterMin, watchdog_grace) — a single good probe is
// not evidence the storm is over.
const (
	wdBackoffFloor         = time.Second
	wdBackoffCap           = time.Minute
	wdBackoffResetAfterMin = 30 * time.Second
)

// failureKind distinguishes the two restart triggers in logs and
// status; both consume the same budget.
type failureKind int

const (
	failureCrash failureKind = iota
	failureHealth
)

func (k failureKind) String() string {
	if k == failureCrash {
		return "crash"
	}
	return "health"
}

// watchdogState is the loop's bookkeeping, behind its own mutex so the
// status endpoint can read it without touching the loop.
type watchdogState struct {
	mu               sync.Mutex
	state            string
	failures         int         // consecutive failed probes on the current instance
	restarts         []time.Time // restart times inside the sliding budget window
	backoffStep      int
	healthySince     time.Time
	lastRestartAt    time.Time
	lastRestartCause string
	skipGraceFor     handle // superviseInstance re-arms no grace for this handle
	lastFailure      string
	jitter           func() float64 // test seam; nil = rand.Float64
	// What the notify module was told (lifecycle.go): alerted while a
	// restart loop is reported and not yet over, recovering once its
	// recovery is reported and the hold runs, refused while a relaunch
	// refusal stands. They outlive the process in alerts.json
	// (alertsChanged writes it), so that a motel restarted while one
	// stands reports it again (ReportStanding).
	alerted, recovering, refused bool
	// refusedRelease and refusedReason are what the refusal was told
	// with, kept beside the flag so that a standing report says them
	// again: Alertmanager merges it over the original alert, and a
	// generic summary would replace the reason in every repeat.
	refusedRelease, refusedReason string
	// persisted is the pair alerts.json was last asked to hold, and gen
	// counts each change of it; saveAlerts writes one (nil: nowhere to
	// write yet, before the app's first configure).
	persisted  reportedAlerts
	gen        uint64
	saveAlerts func(reportedAlerts) error
	// saveMu orders the writes, savedGen the newest one written: a
	// slower write of an older pair never lands over a newer one.
	saveMu   sync.Mutex
	savedGen uint64
	// restored is set once alerts.json has been read into the flags,
	// at the app's first configure in this process or its first
	// ReportStanding, whichever comes first.
	restored bool
}

// reportedAlerts is alerts.json: what the watchdog told the notify
// module and has not ended. Written on each change, read once per
// process.
type reportedAlerts struct {
	Loop       bool `json:"loop,omitempty"`       // a restart loop
	Recovering bool `json:"recovering,omitempty"` // its recovery told, the hold running
	Refused    bool `json:"refused,omitempty"`    // a relaunch refusal
	// The refusal's release and reason (its error's first line, within
	// refusedReasonMax bytes), as it was told.
	RefusedRelease string `json:"refused_release,omitempty"`
	RefusedReason  string `json:"refused_reason,omitempty"`
}

// refusedReasonMax bounds the reason kept in alerts.json: a pre-flight
// refusal's first line names a machine or a command, well inside it.
const refusedReasonMax = 512

// boundReason is the reason as kept: its first line, cut at a rune
// boundary within refusedReasonMax bytes.
func boundReason(s string) string {
	s = firstLine(s)
	if len(s) <= refusedReasonMax {
		return s
	}
	cut := refusedReasonMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// alertsChanged is called with mu held after a flag may have changed:
// it returns the write of the new pair to run once mu is released, or
// nil when nothing persisted changed.
func (w *watchdogState) alertsChanged() func() {
	now := reportedAlerts{Loop: w.alerted, Recovering: w.alerted && w.recovering, Refused: w.refused}
	if w.refused {
		now.RefusedRelease, now.RefusedReason = w.refusedRelease, w.refusedReason
	}
	if now == w.persisted || w.saveAlerts == nil {
		w.persisted = now // before the first configure: the file's own
		return nil
	}
	w.persisted = now
	w.gen++
	gen, save := w.gen, w.saveAlerts
	return func() { w.writeAlerts(gen, save, now) }
}

// rewriteAlerts writes what stands with the writer now installed, as a
// change newer than any write still in flight to the one before, which
// lands ahead of this one or not at all: a rollback's, whose candidate
// had pointed the writes at its own root (Copilot round 3 on #424).
func (w *watchdogState) rewriteAlerts() {
	w.mu.Lock()
	w.gen++
	gen, save, now := w.gen, w.saveAlerts, w.persisted
	w.mu.Unlock()
	w.writeAlerts(gen, save, now)
}

// writeAlerts writes r, the pair of change gen, with save, unless a
// newer change has been written already. Called without mu.
func (w *watchdogState) writeAlerts(gen uint64, save func(reportedAlerts) error, r reportedAlerts) {
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	if gen <= w.savedGen {
		return
	}
	w.savedGen = gen
	_ = save(r) // the writer journals its own failure
}

// runSave calls a deferred write if there is one.
func runSave(f func()) {
	if f != nil {
		f()
	}
}

// throttledAlert records that the restart loop is reported; it says
// whether a report is due: on the throttle itself, and on any failure
// while the loop is reported, which ends a recovery's hold.
func (w *watchdogState) throttledAlert(throttling bool) bool {
	w.mu.Lock()
	if !throttling && !w.alerted {
		w.mu.Unlock()
		return false
	}
	due := throttling || w.recovering
	w.alerted, w.recovering = true, false
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
	return due
}

// healthyAlert is a healthy probe: it says whether the recovery is to
// be reported (the first healthy probe of a reported loop), and ends
// the loop's report once the app has been healthy for hold, the span
// after which the backoff resets too.
func (w *watchdogState) healthyAlert(now time.Time, hold time.Duration) bool {
	w.mu.Lock()
	switch {
	case !w.alerted:
		w.mu.Unlock()
		return false
	case !w.healthySince.IsZero() && now.Sub(w.healthySince) >= hold:
		w.alerted, w.recovering = false, false
		save := w.alertsChanged()
		w.mu.Unlock()
		runSave(save)
		return false
	case w.recovering:
		w.mu.Unlock()
		return false
	}
	w.recovering = true
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
	return true
}

// endLoopAlert ends a reported loop, and says whether one was reported:
// a deploy that succeeded (#418: "or a deploy succeeds"), or the
// watchdog turned off, after which nothing would ever report it healthy.
func (w *watchdogState) endLoopAlert() bool {
	w.mu.Lock()
	was := w.alerted
	w.alerted, w.recovering = false, false
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
	return was
}

// setRefused records a relaunch refusal of release for reason, and says
// whether it is new and so to be told. One already standing keeps what
// it was told with.
func (w *watchdogState) setRefused(release, reason string) bool {
	w.mu.Lock()
	was := w.refused
	if !was {
		w.refused, w.refusedRelease, w.refusedReason = true, release, boundReason(reason)
	}
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
	return !was
}

func (w *watchdogState) takeRefused() bool {
	w.mu.Lock()
	was := w.refused
	w.refused, w.refusedRelease, w.refusedReason = false, "", ""
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
	return was
}

// forgetAlerts ends what was reported, for an app the serving config no
// longer runs: the notify module resolves its keys itself, and an app
// of the name added later starts with nothing standing.
func (w *watchdogState) forgetAlerts() {
	w.mu.Lock()
	w.alerted, w.recovering, w.refused = false, false, false
	w.refusedRelease, w.refusedReason = "", ""
	save := w.alertsChanged()
	w.mu.Unlock()
	runSave(save)
}

// standing is what the notify module was told and has not ended: a
// restart loop, whether its recovery's hold runs, a relaunch refusal. A
// recovery whose hold has passed (the app healthy for hold since) is
// over though the flag waits for the next probe to clear it: notify
// has resolved it, or will, so it is not told again.
func (w *watchdogState) standing(now time.Time, hold time.Duration) (loop, recovering, refused bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	loop, recovering = w.alerted, w.alerted && w.recovering
	if recovering && !w.healthySince.IsZero() && now.Sub(w.healthySince) >= hold {
		loop, recovering = false, false
	}
	return loop, recovering, w.refused
}

// refusal is the standing refusal's release and reason, as it was told.
func (w *watchdogState) refusal() (release, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refusedRelease, w.refusedReason
}

func (w *watchdogState) isRestored() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.restored
}

// restoreAlerts reads alerts.json into the flags, once per process, and
// points the writes of each change at it. A file that is not there is
// nothing standing; one that does not read is said and taken as
// nothing (the alert it held ends at Alertmanager's resolve_timeout, as
// before this file), never a reason to stop the app.
func (w *watchdogState) restoreAlerts(load func() (reportedAlerts, error), save func(reportedAlerts) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.saveAlerts = save
	if w.restored {
		return nil
	}
	w.restored = true
	r, err := load()
	if err != nil {
		return err
	}
	// Flags already set in this process (none before the first
	// configure) are newer than the file.
	if !w.alerted && r.Loop {
		w.alerted, w.recovering = true, r.Recovering
	}
	if !w.refused && r.Refused {
		w.refused, w.refusedRelease, w.refusedReason = true, r.RefusedRelease, boundReason(r.RefusedReason)
	}
	w.persisted = r
	return nil
}

func (w *watchdogState) setState(s string) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}

// arm resets the per-instance counters for a freshly (re)started
// instance. The backoff step is deliberately NOT reset here — it decays
// only through sustained health or a successful deploy.
func (w *watchdogState) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.healthySince = time.Time{}
	w.state = wdStateGrace
}

func (w *watchdogState) recordHealthy(now time.Time, resetAfter time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.lastFailure = ""
	if w.healthySince.IsZero() {
		w.healthySince = now
	}
	if now.Sub(w.healthySince) >= resetAfter {
		w.backoffStep = 0
	}
}

func (w *watchdogState) recordFailure(desc string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures++
	w.lastFailure = desc
	w.healthySince = time.Time{}
	return w.failures
}

// setLastFailure records why the app is not running when no probe
// will say it: a relaunch refused for good, or a recovery that gave
// up. The next launch clears it — recovery's (with ""), a deploy's
// (reset), or a healthy probe of a later instance — and so does a
// recovery that finds nothing recorded.
func (w *watchdogState) setLastFailure(desc string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastFailure = desc
}

// noteFlap breaks the continuous-health streak without counting toward
// the failure threshold. Used for probe failures during grace: they
// must not trigger a restart, but they also must not let a pre-flap
// pass keep an old streak alive and reset the backoff.
func (w *watchdogState) noteFlap() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.healthySince = time.Time{}
}

// consumeBudget prunes restarts that have slid out of the window and,
// if there is room left, claims a slot for the restart about to
// happen. Claiming before the restart (not after it succeeds) is what
// bounds a launch-fails-instantly storm.
func (w *watchdogState) consumeBudget(now time.Time, budget int, window time.Duration) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.restarts[:0]
	for _, t := range w.restarts {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	w.restarts = kept
	if len(w.restarts) >= budget {
		return false
	}
	w.restarts = append(w.restarts, now)
	return true
}

// skipNextGrace asks the next supervision pass of h not to re-arm the
// grace window: that instance is already known unhealthy. Tied to the
// handle so a deploy that replaces it meanwhile gets its full grace.
func (w *watchdogState) skipNextGrace(h handle) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.skipGraceFor = h
}

func (w *watchdogState) takeSkipGrace(h handle) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skipGraceFor == nil || w.skipGraceFor != h {
		return false // another instance's flag (or none) stays put
	}
	w.skipGraceFor = nil
	return true
}

// refundBudget returns the most recently claimed slot. Called when a
// claimed restart is abandoned without a Start ever happening (a
// deploy or recovery replaced the instance, the watchdog was turned
// off, the app is being destructed): an abandoned restart must not
// count against the budget. No-op if a deploy's reset already cleared
// the window.
func (w *watchdogState) refundBudget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.restarts); n > 0 {
		w.restarts = w.restarts[:n-1]
	}
}

// untilBudgetFrees returns how long until the oldest in-window restart
// slides out and a slot opens. Only meaningful right after
// consumeBudget refused (the window is full, so restarts is non-empty).
func (w *watchdogState) untilBudgetFrees(now time.Time, window time.Duration) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.restarts) == 0 {
		return wdBackoffFloor
	}
	wait := w.restarts[0].Add(window).Sub(now)
	if wait < wdBackoffFloor {
		wait = wdBackoffFloor
	}
	return wait
}

// nextBackoff returns the jittered delay before the next restart and
// advances the step.
func (w *watchdogState) nextBackoff() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := wdBackoffFloor
	for i := 0; i < w.backoffStep && d < wdBackoffCap; i++ {
		d *= 2
	}
	if d > wdBackoffCap {
		d = wdBackoffCap
	}
	w.backoffStep++
	r := w.jitter
	if r == nil {
		r = rand.Float64
	}
	return time.Duration(float64(d) * (0.8 + 0.4*r()))
}

func (w *watchdogState) recordRestart(now time.Time, kind failureKind) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastRestartAt = now
	w.lastRestartCause = kind.String()
}

// reset clears counters, budget and backoff. Called on a successful
// deploy: the operator (or CI) shipping a new instance is the fast
// path out of a throttle wait. Restart history for the status endpoint
// is kept.
func (w *watchdogState) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = 0
	w.restarts = nil
	w.backoffStep = 0
	w.healthySince = time.Time{}
	w.lastFailure = ""
	w.skipGraceFor = nil
}

func (w *watchdogState) statusSnapshot(now time.Time, window time.Duration) *watchdogSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == "" {
		return nil // watchdog never armed (e.g. before first Start)
	}
	inWindow := 0
	for _, t := range w.restarts {
		if now.Sub(t) < window {
			inWindow++
		}
	}
	return &watchdogSnapshot{
		State:            w.state,
		Failures:         w.failures,
		RestartsInWindow: inWindow,
		LastRestartAt:    w.lastRestartAt,
		LastRestartCause: w.lastRestartCause,
		LastFailure:      w.lastFailure,
	}
}

// watchdogLoop runs until ctx is canceled (Destruct). Each cycle
// re-snapshots the spec, so reloads — including `watchdog off` — take
// effect on the next iteration without touching the goroutine.
func (ma *managedApp) watchdogLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		c := ma.snapshot()
		inst := ma.currentInstance()
		if c.spec == nil || !c.spec.watchdogOn || inst == nil {
			ma.parkWatchdog(ctx, c)
			continue
		}
		if !ma.superviseInstance(ctx, c, inst) {
			return
		}
	}
}

// parkWatchdog blocks while there is nothing to supervise: watchdog
// off, or no instance. A poke (new instance, config nudge) re-enters
// the outer loop.
func (ma *managedApp) parkWatchdog(ctx context.Context, c collaborators) {
	if c.spec != nil && !c.spec.watchdogOn {
		ma.wd.setState(wdStateDisabled)
		// Every way a reload turns the watchdog off ends here, mid-wait
		// or not: a loop it was reporting is over, since no probe will
		// run to say so.
		if ma.wd.endLoopAlert() {
			ma.lifecycle(c, LifecycleEvent{Name: EventWatchdogRecovered, Summary: "the watchdog was turned off, which ends the restart loop's report"})
		}
	} else {
		ma.wd.setState(wdStateIdle)
	}
	select {
	case <-ctx.Done():
	case <-ma.wdNotify:
	}
}

// superviseInstance watches one instance until it is replaced, the
// watchdog acts on a failure, or ctx ends. Returns false only when the
// loop should exit entirely (ctx canceled).
func (ma *managedApp) superviseInstance(ctx context.Context, c collaborators, inst *instance) bool {
	waitCh := c.runner.Wait(inst.handle) // nil ⇒ Alive polling on the tick below
	grace := c.spec.wdGrace
	if ma.wd.takeSkipGrace(inst.handle) {
		grace = 0 // re-supervising a known-unhealthy instance whose stop failed
	}
	graceUntil := c.clock.Now().Add(grace)
	// The next-probe deadline survives non-tick wakeups: re-arming a
	// full interval on every wdNotify poke would let sustained config
	// reload churn postpone health probing indefinitely.
	tickAt := c.clock.Now().Add(c.spec.healthInterval)
	ma.wd.arm()

	for {
		// Re-snapshot every cycle: a healthy instance can be watched
		// for weeks, and a reload's new spec (watchdog off, changed
		// intervals/thresholds) must apply on the next tick, not on
		// the next restart.
		c = ma.snapshot()
		spec := c.spec
		if spec == nil || !spec.watchdogOn {
			return true // outer loop parks as disabled
		}
		if c.clock.Now().Before(graceUntil) {
			ma.wd.setState(wdStateGrace)
		} else {
			ma.wd.setState(wdStateWatching)
		}
		tickWait := tickAt.Sub(c.clock.Now())
		if tickWait < 0 {
			tickWait = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				return true // promote/recovery installed a new instance; re-arm on it
			}
			continue // config nudge: re-snapshot without re-arming grace
		case <-waitCh:
			if ma.currentInstance() != inst {
				return true // deploy stopped the old handle; not a crash
			}
			c.logger.Warn("watchdog: process exited unexpectedly",
				zap.String("version", inst.version), zap.Int("pid", inst.handle.state().PID))
			return ma.handleFailure(ctx, c, inst, failureCrash)
		case <-c.clock.After(tickWait):
			tickAt = c.clock.Now().Add(spec.healthInterval)
			if ma.currentInstance() != inst {
				return true
			}
			if !c.runner.Alive(inst.handle) {
				c.logger.Warn("watchdog: process exited unexpectedly",
					zap.String("version", inst.version), zap.Int("pid", inst.handle.state().PID))
				return ma.handleFailure(ctx, c, inst, failureCrash)
			}
			if spec.healthPath == "" {
				// health_path off: liveness is the whole check.
				ma.wd.recordHealthy(c.clock.Now(), backoffResetAfter(spec))
				ma.noteHealthy(c, spec, inst.version)
				continue
			}
			err := c.prober.probeOnce(ctx, inst.sock, spec.healthPath, spec.healthTimeout)
			if ma.currentInstance() != inst {
				// A deploy replaced the instance while the probe was in
				// flight; its result belongs to the old era and must not
				// touch the freshly reset counters.
				return true
			}
			if err == nil {
				ma.wd.recordHealthy(c.clock.Now(), backoffResetAfter(spec))
				ma.noteHealthy(c, spec, inst.version)
				continue
			}
			if c.clock.Now().Before(graceUntil) {
				ma.wd.noteFlap() // don't count, but break the healthy streak
				ma.noteUnhealthy(c, inst.version)
				c.logger.Debug("watchdog: probe failed during grace", zap.Error(err))
				continue
			}
			n := ma.wd.recordFailure(err.Error())
			ma.noteUnhealthy(c, inst.version)
			c.logger.Warn("watchdog: health probe failed",
				zap.String("kind", classifyProbeErr(err)),
				zap.Int("consecutive", n),
				zap.Int("threshold", spec.wdFailures),
				zap.Error(err))
			if n >= spec.wdFailures {
				return ma.handleFailure(ctx, c, inst, failureHealth)
			}
		}
	}
}

// handleFailure is the restart decision: consume budget, back off,
// then — still holding the same one-deploy-at-a-time lock deploys use —
// stop what is left of the instance and relaunch its version. Every
// wake-up re-checks that inst is still current: promote swaps current
// before stopping the old handle, so a stale instance here always means
// "a deploy already replaced it" and the watchdog stands down.
func (ma *managedApp) handleFailure(ctx context.Context, c collaborators, inst *instance, kind failureKind) bool {
	var spec *appSpec // (re)read from the live snapshot in the loop below
	// Deploys that end from here on, leaving inst current, ended over
	// it: the backoff below is not waited out after one.
	deploys := ma.deploysEnded.Load()
	if ma.wd.throttledAlert(false) {
		ma.lifecycle(c, LifecycleEvent{Name: EventWatchdogThrottled, Release: inst.version,
			Summary: "the restart loop goes on: " + kind.String() + " again before the app was healthy for long"})
	}
	if !c.runner.Alive(inst.handle) {
		// Never route to a dead port while pacing the restart: a fast
		// clean 5xx beats dialing it, and the freed port could be
		// re-bound by another app's deploy in the meantime.
		ma.unrouteIf(inst)
	}
	// The window is a rate limiter, not a give-up point: when it is
	// full, throttle until the oldest restart slides out, then keep
	// going. An app taken down by a transient incident comes back on
	// its own once the incident ends; a deploy (wdNotify) is the
	// fast path out of the wait. Every iteration re-reads the live
	// spec: an operator reloading a bigger budget to revive a
	// throttled app means it NOW, not after the old window drains.
	for {
		c = ma.snapshot()
		spec = c.spec
		if spec == nil || !spec.watchdogOn {
			return true // reload turned the watchdog off mid-wait; no slot was claimed
		}
		if ma.wd.consumeBudget(c.clock.Now(), spec.wdRestarts, spec.wdWindow) {
			break
		}
		wait := ma.wd.untilBudgetFrees(c.clock.Now(), spec.wdWindow)
		ma.wd.setState(wdStateThrottled)
		if ma.wd.throttledAlert(true) {
			ma.lifecycle(c, LifecycleEvent{Name: EventWatchdogThrottled, Release: inst.version,
				Summary: fmt.Sprintf("restart loop: %d restarts in %s spent the watchdog's budget (last cause: %s); the next waits %s",
					spec.wdRestarts, spec.wdWindow, kind, wait.Round(time.Second))})
		}
		alive := c.runner.Alive(inst.handle)
		if !alive {
			ma.unrouteIf(inst)
		}
		c.logger.Error("watchdog: restart budget exhausted; throttling before the next restart",
			zap.String("cause", kind.String()),
			zap.Int("budget", spec.wdRestarts),
			zap.Duration("window", spec.wdWindow),
			zap.Duration("wait", wait),
			zap.Bool("process_alive", alive))
		select {
		case <-ctx.Done():
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				return true // a deploy replaced the instance and reset the budget
			}
			continue // config nudge: re-read the spec, recompute the wait
		case <-c.clock.After(wait):
		}
	}

	d := ma.wd.nextBackoff()
	ma.wd.setState(wdStateBackoff)
	deadline := c.clock.Now().Add(d)
	// Once a deploy has ended since the failure was found and left this
	// instance current — it failed, or was refused before it began —
	// the dead instance has waited through it, the throttle included:
	// restart now rather than at the end of the step. The step is not
	// refunded: the next failure waits longer, as after any restart.
	for ma.deploysEnded.Load() == deploys {
		wait := deadline.Sub(c.clock.Now())
		if wait <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			ma.wd.refundBudget()
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst {
				// Replaced mid-wait. A deploy's reset makes the refund
				// a no-op; an ensureRunning relaunch (reload during
				// the wait) does NOT reset, and without the refund its
				// unbudgeted restart would leave a phantom slot in the
				// window.
				ma.wd.refundBudget()
				return true
			}
			if ma.watchdogDisabled() {
				ma.wd.refundBudget()
				return true // reload turned the watchdog off mid-wait
			}
			continue // a deploy's end is read above; anything else is a config nudge: keep waiting out the backoff
		case <-c.clock.After(wait):
		}
	}

	// A deploy (or recovery) holding the lock owns the lifecycle: wait
	// for it to let go, which wakes the loop (unlockDeploy), rather than
	// re-entering supervision per backoff step — that logged the same
	// dead instance once a step for as long as the deploy ran. A
	// promoted deploy installs its own instance; after any other end
	// the restart goes at once. The claimed slot is kept for it, and
	// given back if it never happens.
	for !ma.deployMu.TryLock() {
		select {
		case <-ctx.Done():
			ma.wd.refundBudget()
			return false
		case <-ma.wdNotify:
			if ma.currentInstance() != inst || ma.watchdogDisabled() {
				ma.wd.refundBudget()
				return true
			}
		}
	}
	defer ma.deployMu.Unlock()
	if ctx.Err() != nil {
		ma.wd.refundBudget()
		return false // Destruct won the race; it waits for this goroutine before stopping the child
	}
	if ma.currentInstance() != inst {
		ma.wd.refundBudget() // replaced mid-wait (see the backoff arm); no restart happened
		return true
	}
	// A reload during the final wait can land without its poke being
	// consumed (the timer may fire first). Re-read the live spec
	// before acting: `watchdog off` must never be followed by one more
	// restart — least of all one that kills a live process.
	c = ma.snapshot()
	spec = c.spec
	if spec == nil || !spec.watchdogOn {
		ma.wd.refundBudget()
		return true
	}

	// A health verdict on an instance that is in fact dead is a crash:
	// the probes merely noticed before the runner's unit-state poll
	// did. Record the real cause.
	if kind == failureHealth && !c.runner.Alive(inst.handle) {
		kind = failureCrash
	}

	// A health verdict ages while we wait: the dependency outage that
	// failed the probes may be long over, and killing a process that
	// has been serving fine for the last nine minutes of a ten-minute
	// throttle helps nobody. One fresh probe settles it.
	if kind == failureHealth && spec.healthPath != "" && c.runner.Alive(inst.handle) {
		if err := c.prober.probeOnce(ctx, inst.sock, spec.healthPath, spec.healthTimeout); err == nil {
			ma.wd.refundBudget()
			c.logger.Info("watchdog: instance recovered during the restart wait; restart canceled",
				zap.String("version", inst.version))
			return true // re-arm and keep watching the recovered instance
		}
	}

	// A pre_start a motel restart left running holds every launch
	// until it ends (Sweep): stopping this instance first would turn
	// an unhealthy app into a down one for that long. It is left
	// serving, as while a deploy runs, and the restart is tried again
	// on the next failure verdict. A dead instance has nothing to keep:
	// its relaunch below waits on the migration instead.
	if c.runner.Alive(inst.handle) {
		var mr *migrationRunningError
		if err := ma.sweepOrWhy(c, inst, ""); errors.As(err, &mr) {
			ma.wd.refundBudget()
			c.logger.Warn("watchdog: leaving the unhealthy instance serving until the pre_start has ended",
				zap.String("version", inst.version), zap.Error(err))
			return true
		}
	}

	// Stop can block for the unit's own stop budget while deployMu is held, so a
	// webhook landing in that window gets a 409 — the same contract as
	// a deploy's own drain+stop-old phase (one lifecycle operation at
	// a time), and the same bound Destruct accepts when it waits for
	// this goroutine at shutdown.
	if c.runner.Alive(inst.handle) {
		if err := c.runner.Stop(inst.handle, spec.grace); err != nil {
			// The runner could not confirm the instance is gone, so a
			// replacement could run beside it. Leave things as they
			// are (still routed if it is alive, unhealthy as it may
			// be). Nothing was restarted, so the budget slot is
			// refunded, and the next supervision pass skips its grace
			// window so the failure is re-detected and the stop
			// retried promptly rather than after wdGrace.
			ma.wd.refundBudget()
			ma.wd.skipNextGrace(inst.handle)
			c.logger.Error("watchdog: cannot confirm the unhealthy instance stopped; not launching a replacement",
				zap.String("version", inst.version), zap.Error(err))
			return true
		}
	}
	newInst, err := ma.launchVersion(c, inst.version)
	var refused *preflightError
	if errors.As(err, &refused) {
		// Refused at its pre-flight: the release cannot run on this box
		// (built for another machine) or its command is not there to
		// run. No restart changes that, so the never-give-up loop is
		// not for it: the instance is dropped — nothing current,
		// nothing routed — the loop parks, and status names the
		// refusal, until a deploy, or a reload's recovery once the
		// cause is fixed, launches the app again.
		ma.dropIf(inst)
		ma.wd.setLastFailure(err.Error())
		if ma.wd.setRefused(inst.version, err.Error()) {
			// The refusal's own text names the release's machine or its
			// command, never a log tail; the filter runs over it anyway.
			ma.lifecycle(c, LifecycleEvent{Name: EventRelaunchRefused, Release: inst.version,
				Summary: refusedSummary(inst.version, boundReason(err.Error()))})
		}
		c.logger.Error("watchdog: relaunch refused; the app stays down until a deploy of another version (or a rollback to one), or a reload once the cause is fixed, launches it",
			zap.String("version", inst.version), zap.Error(err))
		return true
	}
	if err != nil {
		// The budget slot stays consumed, so a launch that fails
		// instantly is bounded exactly like any other restart storm;
		// the next cycle sees the dead instance and tries again. The
		// instance is gone either way (crashed, or stopped above), so
		// stop routing to its socket.
		ma.unrouteIf(inst)
		c.logger.Error("watchdog: restart failed",
			zap.String("version", inst.version), zap.Error(err))
		return true
	}
	// Recorded before it is published: a status read that shows the
	// new instance must show why it was started, not the previous
	// restart's cause.
	ma.wd.recordRestart(c.clock.Now(), kind)
	if err := ma.publishInstance(c, newInst, nil); err != nil {
		c.logger.Warn("persisting restarted instance state", zap.Error(err))
	}
	inst.sock.retire()
	c.logger.Warn("watchdog: restarted app",
		zap.String("cause", kind.String()),
		zap.String("version", newInst.version),
		zap.String("socket", newInst.socket),
		zap.Int("pid", newInst.handle.state().PID),
		zap.Duration("backoff_was", d))
	return true
}

// noteUnhealthy is a failed probe: one inside a reported recovery's hold
// ends the hold (the loop is reported again), so a recovery is told
// only for an app that stayed healthy the whole hold, whether or not
// its failures reach the restart threshold.
func (ma *managedApp) noteUnhealthy(c collaborators, version string) {
	if ma.wd.throttledAlert(false) {
		ma.lifecycle(c, LifecycleEvent{Name: EventWatchdogThrottled, Release: version,
			Summary: "the restart loop goes on: a health probe failed before the app was healthy for long"})
	}
}

// noteHealthy reports the end of a restart loop on the first healthy
// probe after it, with the hold the notify module waits before it says
// so (max(30s, watchdog_grace)).
func (ma *managedApp) noteHealthy(c collaborators, spec *appSpec, version string) {
	hold := backoffResetAfter(spec)
	if ma.wd.healthyAlert(c.clock.Now(), hold) {
		ma.lifecycle(c, LifecycleEvent{Name: EventWatchdogRecovered, Release: version, Hold: hold,
			Summary: "healthy again after a restart loop"})
	}
}

// refusedSummary is a relaunch refusal's summary, as told and as a
// standing report tells it again.
func refusedSummary(release, reason string) string {
	s := "the watchdog's relaunch"
	if release != "" {
		s += " of " + release
	}
	s += " was refused, so the app is down"
	if reason != "" {
		s += ": " + reason
	}
	return s
}

// firstLine is s up to its first line break.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// watchdogDisabled re-reads the live spec: the answer to "may I still
// restart?" must come from the config as it is NOW, not from the
// snapshot taken when the failure was detected — an operator flipping
// `watchdog off` mid-wait means it.
func (ma *managedApp) watchdogDisabled() bool {
	s := ma.snapshot().spec
	return s == nil || !s.watchdogOn
}

func backoffResetAfter(spec *appSpec) time.Duration {
	if spec.wdGrace > wdBackoffResetAfterMin {
		return spec.wdGrace
	}
	return wdBackoffResetAfterMin
}

// classifyProbeErr labels a probe failure for logs. A refused
// connection, or a socket file that is not there at all, is strong
// evidence the process is gone or wedged; a timeout is weak evidence
// (GC pause, load) — both count identically toward the threshold, but
// operators reading logs deserve the distinction.
func classifyProbeErr(err error) string {
	var nerr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &nerr) && nerr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, fs.ErrNotExist):
		return "refused"
	default:
		return "unhealthy"
	}
}
