package liveswap

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

type recNotifier struct {
	mu  sync.Mutex
	evs []LifecycleEvent
}

func (r *recNotifier) Lifecycle(ev LifecycleEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *recNotifier) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.evs {
		out = append(out, e.Name)
	}
	return out
}

func (r *recNotifier) find(name string) (LifecycleEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.evs {
		if e.Name == name {
			return e, true
		}
	}
	return LifecycleEvent{}, false
}

func (r *recNotifier) reset() {
	r.mu.Lock()
	r.evs = nil
	r.mu.Unlock()
}

func notifiedRig(t *testing.T) (*testRig, *recNotifier) {
	t.Helper()
	rig := newTestRig(t)
	n := &recNotifier{}
	rig.ma.setNotifier(n)
	return rig, n
}

func TestLifecycleDeploySucceeded(t *testing.T) {
	rig, n := notifiedRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1", by: "local:/k.pub sub=alice"}))
	if got := n.names(); !slices.Equal(got, []string{EventDeployStarted, EventCutover, EventDeploySucceeded, EventDeployEnded}) {
		t.Fatalf("events = %v", got)
	}
	ev, _ := n.find(EventDeploySucceeded)
	if ev.App != "demo" || ev.Release != "v1" || ev.DeployedBy != "local:/k.pub sub=alice" || ev.Summary != "deployed v1" {
		t.Fatalf("event = %+v", ev)
	}
	// A first deploy replaced nothing; its cutover is when it published.
	cut, _ := n.find(EventCutover)
	if cut.Release != "v1" || cut.DeployedBy != "local:/k.pub sub=alice" || cut.Previous != "" || cut.Cutover.IsZero() {
		t.Fatalf("a first deploy's cutover: %+v", cut)
	}
}

// The release a deploy replaced is the one serving before its cutover,
// and so for a rollback; a failed deploy names none.
func TestLifecycleDeployNamesTheReleaseItReplaced(t *testing.T) {
	rig, n := notifiedRig(t)
	deployV1(t, rig)
	n.reset()
	before := rig.clock.Now()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	ev, _ := n.find(EventCutover)
	if ev.Previous != "v1" || ev.Cutover.Before(before) {
		t.Fatalf("v2 after v1: previous %q, cutover %v (the deploy began %v)", ev.Previous, ev.Cutover, before)
	}
	n.reset()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	if ev, _ := n.find(EventCutover); ev.Release != "v1" || ev.Previous != "v2" || ev.Cutover.IsZero() {
		t.Fatalf("the rollback to v1: previous %q, cutover %v", ev.Previous, ev.Cutover)
	}
	// One that died before the deploy (a crash loop, unrouted) served
	// nothing: no release before it.
	n.reset()
	rig.runner.handleAt(len(rig.runner.handles) - 1).dieQuietly()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/4", version: "v4"}))
	if ev, ok := n.find(EventCutover); !ok || ev.Previous != "" {
		t.Fatalf("v4 after a dead v1: %+v", ev)
	}
	n.reset()
	rig.fetch.err = errors.New("fetching failed")
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/3", version: "v3"}); err == nil {
		t.Fatal("the deploy succeeded")
	}
	if _, ok := n.find(EventCutover); ok {
		t.Fatalf("a deploy failed before its promote told a cutover: %v", n.names())
	}
}

// A cutover to the release the app last ran is a relaunch, whether its
// instance still served (Previous is the release) or had stopped
// (Previous empty): crashed, or dropped and named by state.json alone.
// A new release, or a rollback to another, is not one, dead instance or
// not (the operator's decision of 2026-11-12).
func TestLifecycleCutoverMarksARelaunch(t *testing.T) {
	rig, n := notifiedRig(t)
	cutover := func(desc, release, previous string, relaunch bool) {
		t.Helper()
		ev, ok := n.find(EventCutover)
		if !ok || ev.Release != release || ev.Previous != previous || ev.Relaunch != relaunch {
			t.Fatalf("%s: cutover %+v, want release %q, previous %q, relaunch %v", desc, ev, release, previous, relaunch)
		}
		n.reset()
	}
	must(t, deployV1Pinned(t, rig, pinA))
	cutover("a first deploy", "v1", "", false)
	must(t, deployV1Pinned(t, rig, pinA))
	cutover("v1 deployed again while it serves", "v1", "v1", true)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	cutover("a rollback to v1 while it serves", "v1", "v1", true)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	cutover("a new release", "v2", "v1", false)
	// v2 crashes and is not restarted (no watchdog here): a rollback to
	// it relaunches it, with nothing serving before; deploying it again
	// is refused as a version that exists.
	rig.runner.handleAt(len(rig.runner.handles) - 1).dieQuietly()
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("deploying the crashed v2 again: %v", err)
	}
	n.reset()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v2", rollback: true}))
	cutover("a rollback to the crashed v2", "v2", "", true)
	rig.runner.handleAt(len(rig.runner.handles) - 1).dieQuietly()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	cutover("a rollback to v1 after v2 crashed", "v1", "", false)
	// Dropped after a refused relaunch: no instance in memory, state.json
	// still names v1.
	dead := rig.ma.currentInstance()
	rig.runner.handleAt(len(rig.runner.handles) - 1).dieQuietly()
	rig.ma.dropIf(dead)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	cutover("a rollback to v1 with nothing in memory", "v1", "", true)
	// The deploy events carry no mark: the cutover is the comparison's.
	must(t, deployV1Pinned(t, rig, pinA))
	for _, name := range []string{EventDeployStarted, EventDeploySucceeded, EventDeployEnded} {
		if ev, _ := n.find(name); ev.Relaunch {
			t.Errorf("%s carries the relaunch mark", name)
		}
	}
}

// The watchdog's restart of a crashed instance, and a restart's
// recovery, publish no cutover: neither starts a post-deploy
// comparison.
func TestLifecycleRestartsTellNoCutover(t *testing.T) {
	rig, n := notifiedRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	n.reset()
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the watchdog's restart", func() bool { return rig.runner.startCount() == 2 })
	waitUntil(t, "the restarted instance published", func() bool {
		cur := rig.ma.currentInstance()
		return cur != nil && cur.handle == rig.runner.handleAt(1)
	})
	if _, ok := n.find(EventCutover); ok {
		t.Fatalf("the watchdog's restart told a cutover: %v", n.names())
	}

	rec, rn := notifiedRig(t)
	rec.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rec.store.ok = true
	must(t, os.MkdirAll(rec.spec.dirs.release("v7"), 0o755))
	must(t, rec.ma.ensureRunning())
	if rec.ma.activeSocket.Load() == nil {
		t.Fatal("recovery published nothing")
	}
	if _, ok := rn.find(EventCutover); ok {
		t.Fatalf("recovery told a cutover: %v", rn.names())
	}
}

// The cutover is when the traffic moved, not when the publication
// returned: the caps' sync after the swap can wait on the manager.
func TestLifecycleCutoverIsTheSwap(t *testing.T) {
	rig := newTestRig(t)
	n := &clockedNotifier{clock: rig.clock, at: map[string]time.Time{}}
	rig.ma.setNotifier(n)
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	}()
	held()
	// Installed during the fetch: the publication finds the instance
	// without these caps and applies them, a manager call that takes
	// ten seconds here.
	spec := testSpec(t)
	spec.caps = resourceCaps{cpuQuota: 100}
	rig.ma.configure(new(int), spec, nil, zap.NewNop(), &fetchClients{}, userManager)
	var applied time.Time
	rig.runner.setCapsHook = func(resourceCaps) {
		applied = rig.clock.Now()
		rig.clock.Advance(10 * time.Second)
	}
	release(nil)
	must(t, <-done)
	ev, ok := n.find(EventCutover)
	if !ok || applied.IsZero() || !ev.Cutover.Equal(applied) {
		t.Fatalf("cutover %v; the caps were applied at %v, ten seconds before the publication returned", ev.Cutover, applied)
	}
	// Told then too, not once the sync returned (Copilot round 2 on
	// #429).
	n.mu.Lock()
	toldAt := n.at[EventCutover]
	n.mu.Unlock()
	if !toldAt.Equal(applied) {
		t.Fatalf("the cutover was told at %v, after the caps' sync (%v)", toldAt, applied)
	}
}

func TestLifecycleDeployFailedNamesThePhaseNotTheError(t *testing.T) {
	rig, n := notifiedRig(t)
	rig.fetch.err = errors.New("fetching: the app printed SECRET-OUTPUT on stderr")
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); err == nil {
		t.Fatal("the deploy succeeded")
	}
	ev, ok := n.find(EventDeployFailed)
	if !ok {
		t.Fatalf("events = %v", n.names())
	}
	if strings.Contains(ev.Summary, "SECRET-OUTPUT") || !strings.Contains(ev.Summary, "deploy of v1 failed") || strings.Contains(ev.Summary, "keeps serving") {
		t.Fatalf("summary = %q", ev.Summary)
	}
	if got := n.names(); got[len(got)-1] != EventDeployEnded {
		t.Fatalf("the mute is not lifted last: %v", got)
	}
}

func TestLifecycleDeployRefused(t *testing.T) {
	rig, n := notifiedRig(t)
	deployV1(t, rig)
	n.reset()
	// A version already on disk: the request's own mistake, a 422.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	n.reset()
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1b", version: "v1"})
	var ve validationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	ev, ok := n.find(EventDeployRefused)
	if !ok || !strings.Contains(ev.Summary, "refused") {
		t.Fatalf("events = %v, refused = %+v", n.names(), ev)
	}
}

func TestLifecycleRollback(t *testing.T) {
	rig, n := notifiedRig(t)
	deployV1(t, rig)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	n.reset()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	ev, ok := n.find(EventRollback)
	if !ok || ev.Summary != "rolled back to v1" {
		t.Fatalf("events = %v", n.names())
	}
}

// TestLifecycleWatchdogLoop: a restart loop is reported at the throttle,
// its recovery at the first healthy probe with max(30s, grace) as the
// hold, and a deploy that follows a loop ends it.
func TestLifecycleWatchdogLoop(t *testing.T) {
	rig, n := notifiedRig(t)
	rig.spec.wdRestarts = 1
	rig.spec.wdWindow = 30 * time.Second
	rig.spec.wdGrace = 45 * time.Second
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the budgeted restart", func() bool { return rig.runner.startCount() == 2 })
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "the throttle", func() bool { return rig.ma.wd.currentState() == wdStateThrottled })
	waitUntil(t, "watchdog_throttled", func() bool { _, ok := n.find(EventWatchdogThrottled); return ok })
	ev, _ := n.find(EventWatchdogThrottled)
	if ev.Release != "v1" || !strings.Contains(ev.Summary, "restart loop") {
		t.Fatalf("throttled = %+v", ev)
	}
	if _, ok := n.find(EventWatchdogRecovered); ok {
		t.Fatal("recovered while throttled")
	}
	// The window frees, the watchdog restarts, the probes pass.
	advanceUntil(t, rig, 5*time.Second, "watchdog_recovered", func() bool { _, ok := n.find(EventWatchdogRecovered); return ok })
	ev, _ = n.find(EventWatchdogRecovered)
	if ev.Hold != 45*time.Second {
		t.Fatalf("hold = %v, want the watchdog grace", ev.Hold)
	}
	count := func(name string) int {
		c := 0
		for _, x := range n.names() {
			if x == name {
				c++
			}
		}
		return c
	}
	if got := count(EventWatchdogRecovered); got != 1 {
		t.Fatalf("recovered reported %d times", got)
	}
}

// TestLifecycleFailedProbeEndsTheRecoveryHold: after a loop's recovery is
// reported, one failed probe, far below the restart threshold, reports
// the loop again, so notify's hold ends and no recovery is told for an
// app that did not stay healthy.
func TestLifecycleFailedProbeEndsTheRecoveryHold(t *testing.T) {
	rig, n := notifiedRig(t)
	rig.spec.wdRestarts = 1
	rig.spec.wdWindow = 30 * time.Second
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the budgeted restart", func() bool { return rig.runner.startCount() == 2 })
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "the throttle", func() bool { return rig.ma.wd.currentState() == wdStateThrottled })
	advanceUntil(t, rig, 5*time.Second, "watchdog_recovered", func() bool { _, ok := n.find(EventWatchdogRecovered); return ok })
	count := func(name string) int {
		c := 0
		for _, x := range n.names() {
			if x == name {
				c++
			}
		}
		return c
	}
	before := count(EventWatchdogThrottled)
	rig.prober.setProbeErr(errors.New("health: 503"))
	advanceUntil(t, rig, time.Second, "the loop reported again", func() bool { return count(EventWatchdogThrottled) > before })
	if got := rig.ma.wd.failureCount(); got >= rig.spec.wdFailures {
		t.Fatalf("the report waited for the restart threshold: %d failures", got)
	}
	rig.prober.setProbeErr(nil)
	advanceUntil(t, rig, time.Second, "a second recovery", func() bool { return count(EventWatchdogRecovered) == 2 })
}

func TestLifecycleDeployEndsALoop(t *testing.T) {
	rig, n := notifiedRig(t)
	rig.spec.wdRestarts = 1
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the budgeted restart", func() bool { return rig.runner.startCount() == 2 })
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "the throttle", func() bool { return rig.ma.wd.currentState() == wdStateThrottled })
	waitUntil(t, "watchdog_throttled", func() bool { _, ok := n.find(EventWatchdogThrottled); return ok })
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	ev, ok := n.find(EventWatchdogRecovered)
	if !ok || ev.Release != "v2" || ev.Hold != 0 {
		t.Fatalf("recovered = %+v, %v (events %v)", ev, ok, n.names())
	}
}

// TestLifecycleKeepsAShaRelease: a version that is a git sha is a name
// in the summary, not a token for the entropy layer to mask.
func TestLifecycleKeepsAShaRelease(t *testing.T) {
	rig, n := notifiedRig(t)
	sha := "3f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d6c5b4a39"
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: sha}))
	ev, _ := n.find(EventDeploySucceeded)
	if ev.Summary != "deployed "+sha {
		t.Fatalf("summary = %q", ev.Summary)
	}
}

func TestLifecycleNoNotifierIsSilent(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig) // no notifier: nothing to call, nothing panics
}

func TestWatchdogAlertFlags(t *testing.T) {
	var w watchdogState
	now := time.Unix(0, 0)
	if w.throttledAlert(false) {
		t.Fatal("a failure with no loop reported is no report")
	}
	for range 2 {
		if !w.throttledAlert(true) {
			t.Fatal("each throttle is reported")
		}
	}
	if w.throttledAlert(false) {
		t.Fatal("a failure inside a loop, before any recovery, is not reported again")
	}
	w.healthySince = now
	if !w.healthyAlert(now, 30*time.Second) {
		t.Fatal("the first healthy probe of a loop is its recovery")
	}
	if w.healthyAlert(now.Add(time.Second), 30*time.Second) {
		t.Fatal("the recovery is reported once")
	}
	if !w.throttledAlert(false) {
		t.Fatal("a failure during the recovery's hold is reported, which ends the hold")
	}
	w.healthySince = now
	_ = w.healthyAlert(now, 30*time.Second)
	if w.healthyAlert(now.Add(31*time.Second), 30*time.Second) || w.alerted {
		t.Fatal("past the hold the loop's report is over")
	}
	if !w.setRefused("v1", "x") || w.setRefused("v1", "x") || !w.takeRefused() || w.takeRefused() {
		t.Fatal("refused flag")
	}
}

// TestRollbackRestoresTheNotifier: a candidate config rejected after
// liveswap's Start (another app's Start failed) had installed its own
// notifier, nil when it had no monitor block; the serving config's
// comes back with its definition.
func TestRollbackRestoresTheNotifier(t *testing.T) {
	rig := newTestRig(t)
	clients := &fetchClients{}
	specA, specB := testSpec(t), testSpec(t)
	ownerA, ownerB := new(int), new(int)
	served := &recNotifier{}
	rig.ma.configure(ownerA, specA, deploytrust.Verifiers(specA.trust, nil), zap.NewNop(), clients, userManager)
	rig.ma.setNotifier(served)
	rig.ma.configure(ownerB, specB, deploytrust.Verifiers(specB.trust, nil), zap.NewNop(), clients, userManager)
	rig.ma.setNotifier(nil) // the candidate has no monitor block
	if !rig.ma.rollbackConfig(ownerB) {
		t.Fatal("the candidate did not roll back")
	}
	if got := rig.ma.snapshot().notifier; got != Notifier(served) {
		t.Fatalf("after the rollback the notifier is %v, want the serving config's", got)
	}
}

// TestLifecycleWatchdogOffEndsALoop: a reload that turns the watchdog
// off while a restart loop is reported ends the report, since no probe
// will run again to say the app is healthy.
func TestLifecycleWatchdogOffEndsALoop(t *testing.T) {
	rig, n := notifiedRig(t)
	rig.spec.wdRestarts = 1
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the budgeted restart", func() bool { return rig.runner.startCount() == 2 })
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "the throttle", func() bool { return rig.ma.wd.currentState() == wdStateThrottled })
	waitUntil(t, "watchdog_throttled", func() bool { _, ok := n.find(EventWatchdogThrottled); return ok })
	// The reload: the same app with `watchdog off`.
	off := *rig.spec
	off.watchdogOn = false
	rig.ma.specMu.Lock()
	rig.ma.spec = &off
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
	waitUntil(t, "watchdog_recovered", func() bool { _, ok := n.find(EventWatchdogRecovered); return ok })
	if rig.ma.wd.currentState() != wdStateDisabled {
		t.Fatalf("state = %s", rig.ma.wd.currentState())
	}
	rig.ma.wd.mu.Lock()
	alerted := rig.ma.wd.alerted
	rig.ma.wd.mu.Unlock()
	if alerted {
		t.Fatal("the loop is still reported")
	}
}

// TestLifecyclePushFailuresBeforeThePipelineAreReported: a push that
// fails before deployLocked (a full disk while the upload is staged, or
// app dirs that are not motel's) skips the pipeline's own report, and is
// still a deploy_failed.
func TestLifecyclePushFailuresBeforeThePipelineAreReported(t *testing.T) {
	t.Run("staging", func(t *testing.T) {
		h, rig := newTestHandler(t)
		n := &recNotifier{}
		rig.ma.setNotifier(n)
		h.stage = func(io.Reader, string, int64) (string, string, error) {
			return "", "", &stagingError{syscall.ENOSPC}
		}
		w := send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		ev, ok := n.find(EventDeployFailed)
		if !ok || ev.Release != "v9" || !strings.Contains(ev.Summary, "could not be staged") || ev.DeployedBy == "" {
			t.Fatalf("events = %v, %+v", n.names(), ev)
		}
	})
	t.Run("dirs", func(t *testing.T) {
		h, rig := newTestHandler(t)
		n := &recNotifier{}
		rig.ma.setNotifier(n)
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		must(t, os.WriteFile(blocker, nil, 0o600))
		rig.spec.dirs.tmp = filepath.Join(blocker, "tmp")
		send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
		ev, ok := n.find(EventDeployFailed)
		if !ok || !strings.Contains(ev.Summary, "before it began") {
			t.Fatalf("events = %v, %+v", n.names(), ev)
		}
	})
}

// What the watchdog told notify outlives the process in alerts.json:
// written on each change, removed once nothing stands, and told again,
// standing, by the next process's ReportStanding, whichever app Caddy
// starts first.
func TestReportStandingAfterARestart(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	rig.ma.restoreAlerts(rig.spec, zap.NewNop())
	file := func() string {
		data, err := os.ReadFile(rig.spec.dirs.alerts)
		if errors.Is(err, os.ErrNotExist) {
			return "absent"
		}
		must(t, err)
		return string(data)
	}
	if !rig.ma.wd.throttledAlert(true) {
		t.Fatal("the throttle was not due")
	}
	rig.ma.wd.setRefused("v1", "pre-flight: built for arm64, this box is amd64")
	if got := file(); got != `{"loop":true,"refused":true,"refused_release":"v1","refused_reason":"pre-flight: built for arm64, this box is amd64"}` {
		t.Fatalf("alerts.json = %s", got)
	}
	// A new process: a managedApp of the same name, never configured.
	restarted := func() (*App, *managedApp) {
		ma := newManagedApp("demo")
		return &App{managed: map[string]*managedApp{"demo": ma}, specs: map[string]*appSpec{"demo": rig.spec}, logger: zap.NewNop()}, ma
	}
	a, ma := restarted()
	n := &recNotifier{}
	a.ReportStanding(n)
	if got := n.names(); !slices.Equal(got, []string{EventWatchdogThrottled, EventRelaunchRefused}) {
		t.Fatalf("after a restart: %v", got)
	}
	for _, ev := range n.evs {
		if !ev.Standing || ev.App != "demo" || ev.Summary == "" {
			t.Errorf("%+v", ev)
		}
	}
	// The refusal is told again with its release and its reason, which
	// the standing report's summary replaces at Alertmanager.
	if ev, _ := n.find(EventRelaunchRefused); ev.Release != "v1" ||
		ev.Summary != "the watchdog's relaunch of v1 was refused, so the app is down: pre-flight: built for arm64, this box is amd64; still reported until an instance runs again" {
		t.Errorf("the standing refusal: %+v", ev)
	}
	// A recovery under way in this process is told as one, with its hold.
	n.reset()
	ma.wd.healthyAlert(time.Now(), time.Minute)
	a.ReportStanding(n)
	if ev, ok := n.find(EventWatchdogRecovered); !ok || !ev.Standing || ev.Hold != backoffResetAfter(rig.spec) {
		t.Fatalf("a recovery under way: %v", n.names())
	}
	// Ended: nothing stands, the file is gone, and a later process tells
	// nothing.
	ma.wd.endLoopAlert()
	ma.wd.takeRefused()
	if got := file(); got != "absent" {
		t.Fatalf("alerts.json = %s once nothing stands", got)
	}
	a, _ = restarted()
	n.reset()
	a.ReportStanding(n)
	if got := n.names(); len(got) != 0 {
		t.Fatalf("nothing stands, yet: %v", got)
	}
}

// An app the serving config removed forgets what it reported; a file
// that does not read is taken as nothing, never a reason to stop.
func TestReportedAlertsForgottenAndUnreadable(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	rig.ma.restoreAlerts(rig.spec, zap.NewNop())
	rig.ma.wd.throttledAlert(true)
	rig.ma.wd.forgetAlerts()
	if _, err := os.Stat(rig.spec.dirs.alerts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a removed app's alerts.json: %v", err)
	}
	must(t, os.WriteFile(rig.spec.dirs.alerts, []byte("{not json"), 0o600))
	ma := newManagedApp("demo")
	a := &App{managed: map[string]*managedApp{"demo": ma}, specs: map[string]*appSpec{"demo": rig.spec}, logger: zap.NewNop()}
	n := &recNotifier{}
	a.ReportStanding(n)
	if got := n.names(); len(got) != 0 {
		t.Fatalf("an unreadable file: %v", got)
	}
	if _, err := os.Stat(rig.spec.dirs.alerts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unreadable alerts.json is kept, to be warned of at every start: %v", err)
	}
}

// recoveringNotifier has the watchdog report the loop's recovery the
// moment the loop is told standing: the recovery a listener with no key
// yet dropped.
type recoveringNotifier struct {
	recNotifier
	ma *managedApp
}

func (r *recoveringNotifier) Lifecycle(ev LifecycleEvent) {
	r.recNotifier.Lifecycle(ev)
	if ev.Standing {
		r.ma.wd.healthyAlert(time.Now(), time.Hour)
	}
}

// A loop that moved to its recovery while it was told standing is told
// recovered after it, or nothing would ever end the standing alert.
func TestReportStandingTellsARecoveryBegunMeanwhile(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.wd.throttledAlert(true)
	n := &recoveringNotifier{ma: rig.ma}
	rig.ma.reportStanding(n, rig.spec)
	if got := n.names(); !slices.Equal(got, []string{EventWatchdogThrottled, EventWatchdogRecovered}) {
		t.Fatalf("told %v", got)
	}
}

// ReportStanding, on a candidate config, reads the file once for this
// process but leaves where the writes go to configure: a config Caddy
// may still refuse never points them at its own root.
func TestReportStandingLeavesTheWritesWhereConfigurePutThem(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	rig.ma.restoreAlerts(rig.spec, zap.NewNop())
	other := testSpec(t) // another root
	must(t, os.MkdirAll(other.dirs.app, 0o750))
	a := &App{managed: map[string]*managedApp{"demo": rig.ma}, specs: map[string]*appSpec{"demo": other}, logger: zap.NewNop()}
	a.ReportStanding(&recNotifier{})
	rig.ma.wd.setRefused("v1", "pre-flight: built for arm64, this box is amd64")
	if _, err := os.Stat(rig.spec.dirs.alerts); err != nil {
		t.Fatalf("the serving config's alerts.json: %v", err)
	}
	if _, err := os.Stat(other.dirs.alerts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the candidate's root got a write: %v", err)
	}
}

// A recovery's hold outlives a restart: the next process tells it as
// the recovery, with its hold, not as a loop; and one whose hold has
// passed is not told at all, since notify has resolved it.
func TestReportStandingKeepsARecoverysHold(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	rig.ma.restoreAlerts(rig.spec, zap.NewNop())
	rig.ma.wd.throttledAlert(true)
	now := rig.clock.Now()
	if !rig.ma.wd.healthyAlert(now, time.Minute) {
		t.Fatal("the recovery was not due")
	}
	ma := newManagedApp("demo")
	a := &App{managed: map[string]*managedApp{"demo": ma}, specs: map[string]*appSpec{"demo": rig.spec}, logger: zap.NewNop()}
	n := &recNotifier{}
	a.ReportStanding(n)
	if got := n.names(); !slices.Equal(got, []string{EventWatchdogRecovered}) || !n.evs[0].Standing {
		t.Fatalf("after a restart in the hold: %v", got)
	}
	// In this process, healthy for the hold: over, though the flag waits
	// for the next probe.
	rig.ma.wd.mu.Lock()
	rig.ma.wd.healthySince = rig.clock.Now().Add(-time.Hour)
	rig.ma.wd.mu.Unlock()
	n.reset()
	rig.ma.reportStanding(n, rig.spec)
	if got := n.names(); len(got) != 0 {
		t.Fatalf("a recovery whose hold has passed was told again: %v", got)
	}
}

// endingNotifier ends the loop and the refusal the moment it is told
// they stand, as the watchdog's or recovery's goroutine could between
// ReportStanding's read and its report, and records what it is told.
type endingNotifier struct {
	recNotifier
	ma *managedApp
}

func (e *endingNotifier) Lifecycle(ev LifecycleEvent) {
	e.recNotifier.Lifecycle(ev)
	if ev.Standing {
		e.ma.wd.endLoopAlert()
		e.ma.wd.takeRefused()
	}
}

// clockedNotifier records when, on the rig's clock, each event was told.
type clockedNotifier struct {
	recNotifier
	clock *fakeClock
	at    map[string]time.Time
}

func (c *clockedNotifier) Lifecycle(ev LifecycleEvent) {
	c.recNotifier.Lifecycle(ev)
	c.mu.Lock()
	c.at[ev.Name] = c.clock.Now()
	c.mu.Unlock()
}

// The cutover is told at the swap, not with the deploy's result: a
// drain longer than a post-deploy window (Copilot round 1 on #429) must
// not start the comparison after its window.
func TestLifecycleCutoverIsToldBeforeTheDrain(t *testing.T) {
	rig := newTestRig(t)
	n := &clockedNotifier{clock: rig.clock, at: map[string]time.Time{}}
	rig.ma.setNotifier(n)
	deployV1(t, rig)
	n.reset()
	rig.spec.drain = 4 * time.Minute
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	cut, ok := n.find(EventCutover)
	if !ok || cut.Previous != "v1" || !n.at[EventCutover].Equal(cut.Cutover) {
		t.Fatalf("cutover %+v, told at %v", cut, n.at[EventCutover])
	}
	if done := n.at[EventDeploySucceeded]; done.Sub(cut.Cutover) < 4*time.Minute {
		t.Fatalf("deploy_succeeded told %v after the cutover: the drain did not run", done.Sub(cut.Cutover))
	}
	names := n.names()
	if slices.Index(names, EventCutover) > slices.Index(names, EventDeploySucceeded) {
		t.Fatalf("events = %v", names)
	}
}

// What ends while ReportStanding tells it is told ended too: the
// goroutine that ended it may have told a listener that had no key yet.
func TestReportStandingTellsWhatEndedMeanwhile(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.app, 0o750))
	rig.ma.restoreAlerts(rig.spec, zap.NewNop())
	rig.ma.wd.throttledAlert(true)
	rig.ma.wd.setRefused("v1", "pre-flight: built for arm64, this box is amd64")
	n := &endingNotifier{ma: rig.ma}
	rig.ma.reportStanding(n, rig.spec)
	want := []string{EventWatchdogThrottled, EventRelaunchRefused, EventWatchdogRecovered, EventRelaunchResolved}
	if got := n.names(); !slices.Equal(got, want) {
		t.Fatalf("told %v, want %v", got, want)
	}
}

// A candidate config that moved liveswap's root, then was refused,
// leaves the watchdog writing alerts.json under the serving root.
func TestRollbackPointsAlertsBackAtTheServingRoot(t *testing.T) {
	rig := newTestRig(t)
	serving, candidate := rig.spec, testSpec(t) // two roots
	for _, s := range []*appSpec{serving, candidate} {
		must(t, os.MkdirAll(s.dirs.app, 0o750))
	}
	servingOwner, candidateOwner := new(int), new(int)
	rig.ma.configure(servingOwner, serving, nil, zap.NewNop(), nil, nil)
	rig.ma.configure(candidateOwner, candidate, nil, zap.NewNop(), nil, nil)
	if !rig.ma.rollbackConfig(candidateOwner) {
		t.Fatal("the candidate was not rolled back")
	}
	rig.ma.wd.setRefused("v1", "pre-flight: built for arm64, this box is amd64")
	if _, err := os.Stat(serving.dirs.alerts); err != nil {
		t.Fatalf("the serving root's alerts.json: %v", err)
	}
	if _, err := os.Stat(candidate.dirs.alerts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused candidate's root got the write: %v", err)
	}
}

// The reason kept is the first line, cut at a rune boundary within its
// bound; a refusal already standing keeps what it was told with.
func TestRefusalReasonBounded(t *testing.T) {
	w := &watchdogState{}
	long := strings.Repeat("é", refusedReasonMax) + "\nsecond line"
	if !w.setRefused("v2", long) {
		t.Fatal("not new")
	}
	_, reason := w.refusal()
	if len(reason) > refusedReasonMax || !utf8.ValidString(reason) || strings.Contains(reason, "second") {
		t.Fatalf("reason %d bytes, valid %v", len(reason), utf8.ValidString(reason))
	}
	w.setRefused("v3", "another")
	if rel, _ := w.refusal(); rel != "v2" {
		t.Fatalf("release %s", rel)
	}
	w.takeRefused()
	if rel, reason := w.refusal(); rel != "" || reason != "" {
		t.Fatalf("after the end: %q %q", rel, reason)
	}
}

// A watchdog change whose write is still in flight when a refused
// candidate is rolled back (taken under the lock before, run after)
// leaves the serving root's alerts.json holding it all the same.
func TestRollbackOutrunsAnAlertsWriteInFlight(t *testing.T) {
	rig := newTestRig(t)
	serving, candidate := rig.spec, testSpec(t) // two roots
	for _, s := range []*appSpec{serving, candidate} {
		must(t, os.MkdirAll(s.dirs.app, 0o750))
	}
	servingOwner, candidateOwner := new(int), new(int)
	rig.ma.configure(servingOwner, serving, nil, zap.NewNop(), nil, nil)
	rig.ma.configure(candidateOwner, candidate, nil, zap.NewNop(), nil, nil)
	w := &rig.ma.wd
	w.mu.Lock()
	w.refused, w.refusedRelease, w.refusedReason = true, "v1", "refused"
	inFlight := w.alertsChanged() // its write still to run, at the candidate's root
	w.mu.Unlock()
	if !rig.ma.rollbackConfig(candidateOwner) {
		t.Fatal("the candidate was not rolled back")
	}
	runSave(inFlight)
	data, err := os.ReadFile(serving.dirs.alerts)
	if err != nil || !strings.Contains(string(data), `"refused":true`) {
		t.Fatalf("the serving root's alerts.json: %q, %v", data, err)
	}
	if _, err := os.Stat(candidate.dirs.alerts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the late write landed at the refused candidate's root: %v", err)
	}
}
