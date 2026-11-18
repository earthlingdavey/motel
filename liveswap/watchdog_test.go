package liveswap

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// currentState is a test accessor for the loop's reported state.
func (w *watchdogState) currentState() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *watchdogState) failureCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failures
}

// startWatchdogT starts the real supervision goroutine on the rig with
// jitter pinned to the nominal backoff, and guarantees teardown so the
// package's goleak gate stays green.
func (rig *testRig) startWatchdogT(t *testing.T) {
	t.Helper()
	rig.ma.wd.jitter = func() float64 { return 0.5 } // 0.8+0.4*0.5 = exactly nominal
	rig.ma.startWatchdog()
	t.Cleanup(rig.ma.stopWatchdog)
}

// pollFor is the one real-time poll loop behind both test builds:
// waitUntil (unit: fake clock, tight cadence) and the integration
// suite's pollUntil (real processes, coarser cadence) delegate here.
func pollFor(t *testing.T, timeout, step time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(step)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// waitUntil polls (real time) for an observable side effect of the
// watchdog goroutine.
func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	pollFor(t, 5*time.Second, 2*time.Millisecond, desc, cond)
}

// advanceUntil repeatedly advances the fake clock in step increments
// until cond holds. The loop paces itself entirely on the clock, so
// driving it means advancing time, not sleeping.
func advanceUntil(t *testing.T, rig *testRig, step time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		rig.clock.Advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func deployV1(t *testing.T, rig *testRig) {
	t.Helper()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
}

func TestWatchdogRestartsOnCrash(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	sockV1 := rig.ma.activeSocket.Load()
	v1 := rig.ma.currentInstance()
	// The new instance's save lands inside its publish, after the
	// instance is installed: a status read then shows the new instance,
	// and must show why it was started.
	var atPublish atomic.Pointer[statusSnapshot]
	rig.store.onSave = func(st appState) {
		if st.Nonce != v1.nonce {
			s := rig.ma.status()
			atPublish.CompareAndSwap(nil, &s)
		}
	}
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill() // crash: reaper channel closes

	advanceUntil(t, rig, time.Second, "restart after crash", func() bool {
		return rig.runner.startCount() == 2
	})
	waitUntil(t, "new instance published", func() bool {
		p := rig.ma.activeSocket.Load()
		return p != nil && p != sockV1
	})
	waitUntil(t, "restart persisted", func() bool { return atPublish.Load() != nil })
	if s := atPublish.Load(); s.Socket == v1.socket || s.Watchdog == nil || s.Watchdog.LastRestartCause != "crash" {
		t.Fatalf("a status showing the new instance must show its restart: socket %s, %+v", s.Socket, s.Watchdog)
	}
	st, ok, _ := rig.store.load()
	if !ok || st.CurrentVersion != "v1" {
		t.Fatalf("restart must persist state for the same version: %+v", st)
	}
	status := rig.ma.status()
	if status.Watchdog == nil || status.Watchdog.RestartsInWindow != 1 || status.Watchdog.LastRestartCause != "crash" {
		t.Fatalf("watchdog status not recorded: %+v", status.Watchdog)
	}
}

// TestRelaunchRendersTheCurrentCommandAndStatusShowsIt: a relaunch
// pins the version and nothing else — command, env and sandbox render
// from the spec as it is NOW (launchVersion), so an app definition
// edited since the deploy reaches the next crash restart. A reload
// does not restart a running app, so that edit can sit unapplied for
// hours and then take effect at a restart nobody connects to it.
//
// That is the behaviour, deliberately: the same rule watchdogDisabled
// follows for the same event. What makes it survivable is that status
// reports the argv the instance is actually running, so the change is
// visible instead of being inferred from an app that started behaving
// differently. This test is that promise.
func TestRelaunchRendersTheCurrentCommandAndStatusShowsIt(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	deployed := rig.ma.status().Command
	if len(deployed) == 0 {
		t.Fatal("status must report the argv the instance was launched with")
	}

	// The operator edits the command and reloads. Swapped before the
	// watchdog starts: a live spec is read by the watchdog goroutine,
	// and this test is about the relaunch, not about racing it.
	edited := *rig.spec
	edited.command = []string{"./server", "--edited", "{version}"}
	rig.ma.specMu.Lock()
	rig.ma.spec = &edited
	rig.ma.specMu.Unlock()

	// Nothing has restarted, so the running instance — and status —
	// still report the command it was actually started with.
	if got := rig.ma.status().Command; !slices.Equal(got, deployed) {
		t.Fatalf("a reload must not change what the running instance reports: %v became %v", deployed, got)
	}

	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "restart after crash", func() bool {
		return rig.runner.startCount() == 2
	})

	want := []string{"./server", "--edited", "v1"}
	waitUntil(t, "status to report the relaunched command", func() bool {
		return slices.Equal(rig.ma.status().Command, want)
	})
	if st, _, _ := rig.store.load(); st.CurrentVersion != "v1" {
		t.Fatalf("the relaunch pins the version; only the command re-renders: %+v", st)
	}
}

// Promise: a watchdog restart leaves the dead instance's socket
// behind for nobody — the relaunch's sweep removes it.
func TestRestartRemovesTheDeadSocket(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	deployV1(t, rig)
	dead := rig.ma.currentInstance()
	if _, err := os.Stat(dead.socket); err != nil {
		t.Fatalf("the bound socket should exist: %v", err)
	}
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "restart after crash", func() bool {
		return rig.runner.startCount() == 2
	})
	waitUntil(t, "dead socket removed", func() bool {
		_, err := os.Stat(dead.socket)
		return os.IsNotExist(err)
	})
	if cur := rig.ma.currentInstance(); cur.socket == dead.socket {
		t.Fatal("the replacement must get its own socket")
	}
}

// A health verdict whose instance turns out dead by restart time is
// recorded as a crash: which signal noticed first is an accident of
// probe interval versus the runner's unit-state poll.
func TestWatchdogHealthFailureOnDeadInstanceIsACrash(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("connection refused"))
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "health verdict reached", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	rig.runner.handleAt(0).dieQuietly()
	rig.prober.setProbeErr(nil)
	// The record, not the second Start: the cause is written after
	// the launch returns (TestWatchdogRestartsAfterConsecutiveHealthFailures).
	advanceUntil(t, rig, time.Second, "restart recorded", func() bool {
		s := rig.ma.status()
		return s.Watchdog != nil && s.Watchdog.LastRestartCause != ""
	})
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("expected exactly one relaunch (2 starts), got %d", got)
	}
	if rig.runner.stopCount() != 0 {
		t.Fatalf("a dead instance must not be stopped, got %d stops", rig.runner.stopCount())
	}
	if status := rig.ma.status(); status.Watchdog == nil || status.Watchdog.LastRestartCause != "crash" {
		t.Fatalf("a dead instance's restart must be recorded as a crash: %+v", status.Watchdog)
	}
}

// An unhealthy instance the runner cannot confirm stopped is left in
// place: launching a replacement beside it would be two instances.
func TestWatchdogUnconfirmedStopAbortsRestart(t *testing.T) {
	rig := newTestRig(t)
	// A long grace: the retry must not wait for it (the instance is
	// already known unhealthy), which is what proves skipNextGrace.
	rig.spec.wdGrace = time.Hour
	deployV1(t, rig)
	rig.ma.wd.skipNextGrace(rig.runner.handleAt(0)) // the first pass probes immediately; only the retry proves the skip
	sock := rig.ma.activeSocket.Load()
	rig.prober.setProbeErr(errors.New("health check returned 500"))
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "stop attempted", func() bool {
		return rig.runner.stopCount() >= 1
	})
	// Give the loop every chance to (wrongly) launch.
	advanceUntil(t, rig, rig.spec.healthInterval, "several more probes", func() bool {
		return rig.prober.calls() >= rig.spec.wdFailures*3
	})
	if rig.runner.startCount() != 1 {
		t.Fatalf("no replacement may be launched beside an unconfirmed instance, got %d starts", rig.runner.startCount())
	}
	if rig.ma.activeSocket.Load() != sock {
		t.Fatal("the still-alive instance stays routed")
	}
	if rig.runner.stopCount() < 2 {
		t.Fatalf("the next cycle must retry the stop, got %d stops", rig.runner.stopCount())
	}
	if wd := rig.ma.status().Watchdog; wd == nil || wd.RestartsInWindow != 0 {
		t.Fatalf("no restart happened, so no budget may be consumed: %+v", wd)
	}
}

// A crash restart is a launch like any other: without a confirmed
// sweep nothing is started, and the next cycle tries again.
func TestWatchdogRestartAbortsWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.sweepErr = errTest
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "restart attempted (sweep called)", func() bool {
		return rig.runner.sweepCount() >= 3 // 2 from the deploy, then the restart's
	})
	if rig.runner.startCount() != 1 {
		t.Fatalf("no launch without a confirmed sweep, got %d starts", rig.runner.startCount())
	}
	rig.runner.sweepErr = nil
	advanceUntil(t, rig, time.Second, "restart once the sweep confirms", func() bool {
		return rig.runner.startCount() == 2
	})
}

// A pre_start a moteld restart left running blocks every launch
// until it ends. A health restart that stopped the instance first
// would turn an unhealthy app into a down one for that long, so the
// watchdog leaves it serving, as it does while a deploy runs, and
// restarts it once the migration has ended.
func TestWatchdogLeavesAnUnhealthyInstanceServingBesideAMigration(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("health check returned 500"))
	rig.runner.sweepErr = &migrationRunningError{unit: "motel-demo.v2.5555555555555555.prestart.service", version: "v2"}
	rig.startWatchdogT(t)
	advanceUntil(t, rig, rig.spec.healthInterval, "restart attempted (sweep called)", func() bool {
		return rig.runner.sweepCount() >= 3 // 2 from the deploy, then the restart's
	})
	if rig.runner.stopCount() != 0 || rig.runner.startCount() != 1 {
		t.Fatalf("the unhealthy instance must be left serving: stops=%d starts=%d", rig.runner.stopCount(), rig.runner.startCount())
	}
	if !rig.runner.Alive(rig.runner.handleAt(0)) {
		t.Fatal("the instance must still run")
	}
	rig.runner.sweepErr = nil
	advanceUntil(t, rig, rig.spec.healthInterval, "restart once the migration has ended", func() bool {
		return rig.runner.startCount() == 2
	})
}

func TestWatchdogSkipGraceIsScopedToTheHandle(t *testing.T) {
	var w watchdogState
	h1, h2 := &fakeHandle{id: "1"}, &fakeHandle{id: "2"}
	w.skipNextGrace(h1)
	if w.takeSkipGrace(h2) {
		t.Fatal("another instance must get its full grace")
	}
	if !w.takeSkipGrace(h1) {
		t.Fatal("the flagged instance must skip grace (once)")
	}
	if w.takeSkipGrace(h1) {
		t.Fatal("consumed")
	}
	w.skipNextGrace(h1)
	w.reset() // a successful deploy
	if w.takeSkipGrace(h1) {
		t.Fatal("a deploy reset must clear a pending skip")
	}
}

func TestWatchdogHealthFailuresBelowThresholdReset(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	// Two failures, then a pass: the pass must reset the counter and no
	// restart may happen no matter how long we keep probing (default
	// probeErr stays nil = healthy).
	rig.prober.probeResults = []error{errors.New("boom"), errors.New("boom")}
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "several probes", func() bool {
		return rig.prober.calls() >= 5
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("no restart expected below the failure threshold, got %d starts", got)
	}
	if got := rig.ma.wd.failureCount(); got != 0 {
		t.Fatalf("a passing probe must reset the consecutive count, got %d", got)
	}
}

func TestWatchdogRestartsAfterConsecutiveHealthFailures(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("health check returned 500"))
	rig.startWatchdogT(t)

	// Wait for the restart to be RECORDED, not merely for the runner's
	// second Start: the cause is written after the launch returns, so
	// a predicate on the start count alone can observe the launch and
	// read the status a moment before the record lands.
	advanceUntil(t, rig, rig.spec.healthInterval, "restart after threshold", func() bool {
		s := rig.ma.status()
		return s.Watchdog != nil && s.Watchdog.LastRestartCause != ""
	})
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("expected exactly one relaunch (2 starts), got %d", got)
	}
	if calls := rig.prober.calls(); calls < rig.spec.wdFailures {
		t.Fatalf("restarted after only %d probes, threshold is %d", calls, rig.spec.wdFailures)
	}
	if rig.runner.stopCount() != 1 {
		t.Fatalf("an alive-but-unhealthy instance must be stopped before relaunch, got %d stops", rig.runner.stopCount())
	}
	status := rig.ma.status()
	if status.Watchdog == nil || status.Watchdog.LastRestartCause != "health" {
		t.Fatalf("restart cause not recorded: %+v", status.Watchdog)
	}
	// Let the replacement be seen healthy so the loop settles.
	rig.prober.setProbeErr(nil)
	advanceUntil(t, rig, rig.spec.healthInterval, "loop settles on replacement", func() bool {
		return rig.ma.wd.currentState() == wdStateWatching
	})
}

func TestWatchdogGraceIgnoresProbeFailures(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = time.Hour
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("still booting"))
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "probes during grace", func() bool {
		return rig.prober.calls() >= 3
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("probe failures during grace must not restart, got %d starts", got)
	}
	if got := rig.ma.wd.failureCount(); got != 0 {
		t.Fatalf("grace failures must not count, got %d", got)
	}
}

func TestWatchdogCrashDuringGraceStillCounts(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = time.Hour
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool { return rig.ma.wd.currentState() == wdStateGrace })

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "restart despite grace", func() bool {
		return rig.runner.startCount() == 2
	})
}

func TestWatchdogThrottlesThenResumesAfterWindow(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 2
	rig.spec.wdWindow = 30 * time.Second
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// Kill every instance the watchdog brings up: 2 restarts fit the
	// window, the third failure throttles.
	for kill := 0; kill < 3; kill++ {
		rig.runner.handleAt(rig.runner.startCount() - 1).kill()
		want := kill + 2
		advanceUntil(t, rig, time.Second, "restart or throttle", func() bool {
			return rig.runner.startCount() == want || rig.ma.wd.currentState() == wdStateThrottled
		})
	}
	if got := rig.ma.wd.currentState(); got != wdStateThrottled {
		t.Fatalf("state = %s, want throttled", got)
	}
	if got := rig.runner.startCount(); got != 3 {
		t.Fatalf("window of 2 must pace to exactly 2 restarts before throttling (3 starts total), got %d", got)
	}
	if rig.ma.activeSocket.Load() != nil {
		t.Fatal("a dead instance must not stay routed during the throttle wait")
	}
	if s := rig.ma.status(); s.Watchdog.State != wdStateThrottled {
		t.Fatalf("status must report the throttle: %+v", s.Watchdog)
	}

	// Never give up: once the oldest restart slides out of the window,
	// the watchdog restarts on its own — no deploy, no operator.
	advanceUntil(t, rig, 5*time.Second, "auto-resume after the window frees", func() bool {
		return rig.runner.startCount() == 4
	})
	waitUntil(t, "resumed instance routed again", func() bool {
		return rig.ma.activeSocket.Load() != nil
	})
}

func TestWatchdogDeployCutsThrottleShort(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1 // default 10m window: the wait is long
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the single budgeted restart", func() bool {
		return rig.runner.startCount() == 2
	})
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "throttle once the window is full", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})

	// A deploy is the fast path out of the wait: it resets the budget
	// and installs a fresh instance immediately.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	waitUntil(t, "watchdog re-arms on the new instance", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.handleAt(rig.runner.startCount() - 1).kill()
	advanceUntil(t, rig, time.Second, "restart works again after deploy", func() bool {
		return rig.ma.status().CurrentVersion == "v2" && rig.ma.status().Running
	})
}

func TestWatchdogIgnoresDeployStoppedOldInstance(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// The deploy stops the old handle after promote; its Wait channel
	// closing must read as "replaced", never as a crash.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	waitUntil(t, "watchdog adopts v2", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	advanceUntil(t, rig, rig.spec.healthInterval, "a probe on the new instance", func() bool {
		return rig.prober.calls() >= 1
	})
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("old instance's exit must not trigger a restart, got %d starts", got)
	}
	if got := rig.ma.status().CurrentVersion; got != "v2" {
		t.Fatalf("current version = %s, want v2", got)
	}
}

// advanceBy moves the fake clock in one-second steps, letting the loop
// run between them, as time passing does.
func advanceBy(rig *testRig, d time.Duration) {
	for range int(d / time.Second) {
		rig.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// The ledger of a watchdog restart that meets a deploy over an
// instance that is dead and unrouted. The restart waits for the
// deploy's end — no re-entry per backoff step, so one crash Warn
// however long the deploy runs — and what the deploy comes to decides
// what follows: a promoted deploy's instance is adopted and no restart
// happens; after a failed one the restart goes at once, whether it was
// waiting for the lock or still waiting out a step of up to a minute.
// "At once" is measured on the fake clock: the restart lands with the
// clock where the deploy left it. Nothing but a deploy's end shortens
// a step: a config nudge does not, and the step itself is never
// refunded (it grows as before).
func TestWatchdogWaitsForADeployOverADeadInstance(t *testing.T) {
	setup := func(t *testing.T, step int) (*testRig, *observer.ObservedLogs) {
		t.Helper()
		rig := newTestRig(t)
		core, logs := observer.New(zap.WarnLevel)
		rig.ma.logger = zap.New(core)
		deployV1(t, rig)
		rig.ma.wd.backoffStep = step // before the loop runs: nothing else reads it yet
		rig.startWatchdogT(t)
		waitUntil(t, "watchdog to arm", func() bool {
			s := rig.ma.wd.currentState()
			return s == wdStateGrace || s == wdStateWatching
		})
		rig.runner.handleAt(0).kill()
		waitUntil(t, "the restart waits out its backoff", func() bool {
			return rig.ma.wd.currentState() == wdStateBackoff
		})
		return rig, logs
	}
	crashWarns := func(logs *observer.ObservedLogs) int {
		return logs.FilterMessage("watchdog: process exited unexpectedly").Len()
	}
	restartedInPlace := func(t *testing.T, rig *testRig, logs *observer.ObservedLogs) {
		t.Helper()
		at := rig.clock.Now()
		waitUntil(t, "the restart after the failed deploy", func() bool {
			return rig.runner.startCount() == 2 && rig.ma.activeSocket.Load() != nil
		})
		if !rig.clock.Now().Equal(at) {
			t.Fatal("the restart must not wait for the clock")
		}
		if n := crashWarns(logs); n != 1 {
			t.Fatalf("crash Warns = %d, want 1", n)
		}
		waitUntil(t, "the restart recorded", func() bool {
			s := rig.ma.status()
			return s.CurrentVersion == "v1" && s.Running && s.Watchdog.RestartsInWindow == 1 && s.Watchdog.LastRestartCause == "crash"
		})
	}

	t.Run("a deploy holds the lock past the step, then fails", func(t *testing.T) {
		rig, logs := setup(t, 0) // a one-second step
		held, release := rig.holdFetch(t)
		deployed := make(chan error, 1)
		go func() {
			deployed <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
		}()
		held()
		advanceBy(rig, 5*time.Minute) // a long deploy
		if n := rig.runner.startCount(); n != 1 {
			t.Fatalf("no restart while a deploy holds the lock: %d starts", n)
		}
		if n := crashWarns(logs); n != 1 {
			t.Fatalf("the restart waits for the deploy rather than re-entering per step: %d crash Warns", n)
		}
		release(errors.New("download failed"))
		if err := <-deployed; err == nil {
			t.Fatal("the deploy should have failed")
		}
		restartedInPlace(t, rig, logs)
	})
	t.Run("a deploy that fails ends a minute-long step", func(t *testing.T) {
		rig, logs := setup(t, 6) // a 60s step
		rig.fetch.err = errors.New("download failed")
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}); err == nil {
			t.Fatal("the deploy should have failed")
		}
		restartedInPlace(t, rig, logs)
	})
	t.Run("a deploy that fails during a throttle ends the step after it", func(t *testing.T) {
		rig := newTestRig(t)
		rig.spec.wdRestarts, rig.spec.wdWindow = 1, 30*time.Second
		deployV1(t, rig)
		rig.startWatchdogT(t)
		waitUntil(t, "watchdog to arm", func() bool {
			s := rig.ma.wd.currentState()
			return s == wdStateGrace || s == wdStateWatching
		})
		rig.runner.handleAt(0).kill()
		advanceUntil(t, rig, time.Second, "the one budgeted restart", func() bool {
			return rig.runner.startCount() == 2 && rig.ma.wd.currentState() == wdStateGrace
		})
		rig.runner.handleAt(1).kill()
		waitUntil(t, "throttled", func() bool { return rig.ma.wd.currentState() == wdStateThrottled })
		rig.fetch.err = errors.New("download failed")
		if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}); err == nil {
			t.Fatal("the deploy should have failed")
		}
		// The throttle is the budget's and stands; the backoff step after
		// it (2 s by now) is not waited out.
		rig.clock.Advance(rig.ma.wd.untilBudgetFrees(rig.clock.Now(), rig.spec.wdWindow))
		// The loop may have read the clock before the advance and armed
		// its timer after it, from the advanced time: a poke makes it
		// look at the budget again, without moving the clock through the
		// backoff step this row is about.
		rig.ma.pokeWatchdog()
		waitUntil(t, "the restart as the budget frees", func() bool { return rig.runner.startCount() == 3 })
	})
	t.Run("a config nudge ends no step", func(t *testing.T) {
		rig, _ := setup(t, 6)
		reloadSpec(rig, func(*appSpec) {})
		time.Sleep(20 * time.Millisecond)
		advanceBy(rig, 59*time.Second)
		if n := rig.runner.startCount(); n != 1 {
			t.Fatalf("a nudge must not cut the step short: %d starts", n)
		}
		advanceUntil(t, rig, time.Second, "the restart at the step's end", func() bool {
			return rig.runner.startCount() == 2
		})
	})
	t.Run("a deploy that promotes replaces the dead instance", func(t *testing.T) {
		rig, logs := setup(t, 0)
		held, release := rig.holdFetch(t)
		deployed := make(chan error, 1)
		go func() {
			deployed <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
		}()
		held()
		advanceBy(rig, 5*time.Second) // past the step: the restart waits for the lock
		release(nil)
		must(t, <-deployed)
		waitUntil(t, "the watchdog supervises the deployed instance", func() bool {
			s := rig.ma.wd.currentState()
			inst := rig.ma.currentInstance()
			return (s == wdStateGrace || s == wdStateWatching) && inst != nil && inst.version == "v2"
		})
		if n := rig.runner.startCount(); n != 2 {
			t.Fatalf("starts = %d, want the two deploys' and no restart", n)
		}
		if s := rig.ma.status(); s.Watchdog.RestartsInWindow != 0 || crashWarns(logs) != 1 {
			t.Fatalf("no restart happened: %+v, %d crash Warns", s.Watchdog, crashWarns(logs))
		}
	})
	t.Run("recovery letting go wakes a restart waiting for the lock", func(t *testing.T) {
		rig, _ := setup(t, 0)
		rig.ma.deployMu.Lock() // what ensureRunning holds
		advanceBy(rig, 5*time.Second)
		if n := rig.runner.startCount(); n != 1 {
			rig.ma.unlockDeploy()
			t.Fatalf("no restart while recovery holds the lock: %d starts", n)
		}
		rig.ma.unlockDeploy()
		waitUntil(t, "the restart once recovery lets go", func() bool { return rig.runner.startCount() == 2 })
	})
}

// takePoke reports whether a poke is waiting for the watchdog, taking it.
func takePoke(ma *managedApp) bool {
	select {
	case <-ma.wdNotify:
		return true
	default:
		return false
	}
}

// Every holder of deployMu but the watchdog wakes it as it lets go — a
// restart that found the lock held waits for exactly that — and a
// deploy's end is told apart from any other wake-up, which is what
// lets a restart still waiting out its backoff go at once. The rows
// are the ends that poke nothing on the way (no promote, no publish),
// so the poke seen is the release's own.
func TestEveryLockHolderWakesTheWatchdogAsItLetsGo(t *testing.T) {
	cases := []struct {
		name   string
		deploy bool // a deploy's end, which deploysEnded counts
		run    func(t *testing.T, h *Handler, rig *testRig)
	}{
		{"a deploy that fails before a launch", true, func(t *testing.T, _ *Handler, rig *testRig) {
			rig.fetch.err = errTest
			if rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}) == nil {
				t.Fatal("the deploy should have failed")
			}
		}},
		{"a deploy refused with a 422", true, func(t *testing.T, _ *Handler, rig *testRig) {
			var ve validationError
			if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); !errors.As(err, &ve) {
				t.Fatalf("want the running version's refusal, got %v", err)
			}
		}},
		{"a deploy that fails its health gate", true, func(t *testing.T, _ *Handler, rig *testRig) {
			rig.prober.err = errTest
			if rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}) == nil {
				t.Fatal("the deploy should have failed")
			}
		}},
		{"a push refused while staging", true, func(t *testing.T, h *Handler, rig *testRig) {
			rig.spec.maxArtifactSize = 8
			if w := send(t, h, gzipDeploy(t, "/demo?version=v2", appToken(t), "way past eight bytes")); w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("push: got %d, want 413", w.Code)
			}
		}},
		{"a push that fails its health gate", true, func(t *testing.T, h *Handler, rig *testRig) {
			rig.prober.err = errTest
			if w := send(t, h, gzipDeploy(t, "/demo?version=v2", appToken(t), "tarball-bytes")); w.Code != http.StatusInternalServerError {
				t.Fatalf("push: got %d, want 500", w.Code)
			}
		}},
		{"a recovery that finds the instance running", false, func(t *testing.T, _ *Handler, rig *testRig) {
			must(t, rig.ma.ensureRunning())
		}},
		{"a recovery that gives up", false, func(t *testing.T, _ *Handler, rig *testRig) {
			rig.runner.handleAt(0).kill()
			must(t, os.RemoveAll(rig.spec.dirs.release("v1")))
			if err := rig.ma.ensureRunning(); err == nil || transientRecovery(err) {
				t.Fatalf("want a permanent recovery error, got %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rig := newTestHandler(t)
			deployV1(t, rig)
			takePoke(rig.ma)
			ended := rig.ma.deploysEnded.Load()
			tc.run(t, h, rig)
			if !rig.ma.deployMu.TryLock() {
				t.Fatal("the lock must be free once the holder is done")
			}
			rig.ma.deployMu.Unlock()
			if !takePoke(rig.ma) {
				t.Fatal("letting go of the lock must wake the watchdog")
			}
			if moved := rig.ma.deploysEnded.Load() != ended; moved != tc.deploy {
				t.Fatalf("deploysEnded moved = %v, want %v (only a deploy's end counts)", moved, tc.deploy)
			}
		})
	}
}

func TestWatchdogReloadAppliesWithoutRestart(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	// Simulate a config reload flipping watchdog off: install a new
	// spec (as configure does) and poke. The loop must adopt it while
	// the instance is healthy — not wait for the next restart.
	off := *rig.spec
	off.watchdogOn = false
	rig.ma.specMu.Lock()
	rig.ma.spec = &off
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
	waitUntil(t, "watchdog parks disabled after the reload", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.runner.handleAt(0).kill()
	rig.clock.Advance(time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off (via reload) must not restart, got %d starts", got)
	}

	// Reload it back on: the parked loop must wake and resume — and
	// then recover the crashed instance.
	rig.ma.specMu.Lock()
	rig.ma.spec = rig.spec
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
	advanceUntil(t, rig, time.Second, "supervision resumes after re-enable", func() bool {
		return rig.runner.startCount() == 2
	})
}

// reloadSpec simulates a config reload: installs a modified copy of
// the rig's spec (as configure does) and pokes the loop.
func reloadSpec(rig *testRig, mutate func(*appSpec)) {
	s := *rig.spec
	mutate(&s)
	rig.ma.specMu.Lock()
	rig.ma.spec = &s
	rig.ma.specMu.Unlock()
	rig.ma.pokeWatchdog()
}

func TestWatchdogOffDuringBackoffCancelsRestart(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	waitUntil(t, "loop enters backoff", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// Reload to watchdog off while the restart is pending: the pending
	// restart must be abandoned, and its claimed budget slot refunded.
	reloadSpec(rig, func(s *appSpec) { s.watchdogOn = false })
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.clock.Advance(5 * time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off mid-backoff must cancel the pending restart, got %d starts", got)
	}
	if s := rig.ma.status(); s.Watchdog.RestartsInWindow != 0 {
		t.Fatalf("abandoned attempt must refund its budget slot: %+v", s.Watchdog)
	}
}

func TestWatchdogOffDuringThrottleCancelsRestart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the single budgeted restart", func() bool {
		return rig.runner.startCount() == 2
	})
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "throttle once the window is full", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})
	reloadSpec(rig, func(s *appSpec) { s.watchdogOn = false })
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.clock.Advance(time.Hour) // far past the throttle window
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 2 {
		t.Fatalf("watchdog off mid-throttle must cancel the pending restart, got %d starts", got)
	}
}

func TestWatchdogUnroutesDeadInstanceDuringBackoff(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	// Without advancing the clock the loop sits in backoff: the dead
	// port must already be unrouted (clean 5xx, and the freed port
	// cannot leak another app's traffic here if reused).
	waitUntil(t, "dead instance unrouted before the restart", func() bool {
		return rig.ma.activeSocket.Load() == nil
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("unroute must happen before any restart, got %d starts", got)
	}
	advanceUntil(t, rig, time.Second, "restart republishes the port", func() bool {
		return rig.ma.activeSocket.Load() != nil && rig.runner.startCount() == 2
	})
}

func TestWatchdogUnroutesWhenRelaunchKeepsFailing(t *testing.T) {
	rig := newTestRig(t)
	// Wider than the cumulative backoff (1+2+4+8+16s), or slots slide
	// out before the window fills and the loop never throttles.
	rig.spec.wdWindow = 5 * time.Minute
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.setStartErr(errors.New("no such binary"))
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "launch failures drain into throttle", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})
	if rig.ma.activeSocket.Load() != nil {
		t.Fatal("port must stay unrouted while relaunches fail")
	}
	// Heal the launcher: the next budgeted attempt must recover and
	// route the app again.
	rig.runner.setStartErr(nil)
	advanceUntil(t, rig, 5*time.Second, "recovery once launches succeed again", func() bool {
		return rig.ma.activeSocket.Load() != nil && rig.ma.status().Running
	})
}

func TestWatchdogCancelsRestartWhenInstanceRecovers(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("dependency outage"))
	rig.startWatchdogT(t)

	advanceUntil(t, rig, rig.spec.healthInterval, "threshold reached, backoff pending", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// The outage ends while the restart is pending: the fresh re-probe
	// must cancel the restart instead of killing a recovered process.
	rig.prober.setProbeErr(nil)
	advanceUntil(t, rig, time.Second, "restart canceled, back to watching", func() bool {
		return rig.ma.wd.currentState() == wdStateWatching
	})
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("recovered instance must not be restarted, got %d starts", got)
	}
	if got := rig.runner.stopCount(); got != 0 {
		t.Fatalf("recovered instance must not be stopped, got %d stops", got)
	}
	if s := rig.ma.status(); s.Watchdog.RestartsInWindow != 0 {
		t.Fatalf("canceled restart must refund its budget slot: %+v", s.Watchdog)
	}
}

func TestWatchdogReloadRaisingBudgetEndsThrottle(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1 // default 10m window: the throttle wait is long
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the single budgeted restart", func() bool {
		return rig.runner.startCount() == 2
	})
	rig.runner.handleAt(1).kill()
	advanceUntil(t, rig, time.Second, "throttle once the window is full", func() bool {
		return rig.ma.wd.currentState() == wdStateThrottled
	})

	// The operator reloads a bigger budget to revive the app: the
	// throttle loop must re-read the live spec and restart now — not
	// after the old 10m window drains.
	reloadSpec(rig, func(s *appSpec) { s.wdRestarts = 5 })
	advanceUntil(t, rig, time.Second, "restart under the raised budget", func() bool {
		return rig.runner.startCount() == 3
	})
}

func TestWatchdogRefundsSlotWhenRecoveryReplacesInstance(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdRestarts = 1
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	waitUntil(t, "backoff pending with the slot claimed", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// A reload-triggered ensureRunning relaunches the dead instance
	// outside the watchdog (no wd.reset on that path). The claimed
	// slot must be refunded, or this unbudgeted restart leaves a
	// phantom in the window.
	must(t, rig.ma.ensureRunning())
	waitUntil(t, "watchdog adopts the recovered instance", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	if s := rig.ma.status(); s.Watchdog.RestartsInWindow != 0 {
		t.Fatalf("abandoned slot must be refunded after recovery replaced the instance: %+v", s.Watchdog)
	}

	// The refunded slot must be spendable on the next genuine crash.
	rig.runner.handleAt(rig.runner.startCount() - 1).kill()
	advanceUntil(t, rig, time.Second, "budgeted restart still available", func() bool {
		return rig.ma.status().Running && rig.ma.status().Watchdog.RestartsInWindow == 1
	})
}

func TestWatchdogGraceFlapBreaksHealthyStreak(t *testing.T) {
	w := &watchdogState{jitter: func() float64 { return 0.5 }}
	base := time.Unix(1_700_000_000, 0)
	// Grow the backoff, then: pass, flap (grace failure), pass 31s
	// later. The flap must have broken the streak, so the late pass
	// must NOT reset the backoff off the pre-flap timestamp.
	w.backoffStep = 6 // next nominal backoff = 60s cap
	w.recordHealthy(base, 30*time.Second)
	w.noteFlap()
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Minute {
		t.Fatalf("flap-interrupted streak must not reset backoff, got %v", got)
	}
	// An uninterrupted 31s streak does reset it.
	w.reset()
	w.backoffStep = 6
	w.recordHealthy(base, 30*time.Second)
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Second {
		t.Fatalf("uninterrupted streak must reset backoff to the floor, got %v", got)
	}
}

func TestWatchdogRefundBudget(t *testing.T) {
	w := &watchdogState{}
	base := time.Unix(1_700_000_000, 0)
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("first claim must fit")
	}
	if w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("window of 1 must refuse a second claim")
	}
	w.refundBudget()
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("a refunded slot must be claimable again")
	}
	w.reset()
	w.refundBudget() // refund after a deploy's reset must be a safe no-op
	if !w.consumeBudget(base, 1, 10*time.Minute) {
		t.Fatal("budget must be intact after a no-op refund")
	}
}

func TestWatchdogDestructDuringBackoffExitsCleanly(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	rig.runner.handleAt(0).kill()
	waitUntil(t, "loop enters backoff", func() bool {
		return rig.ma.wd.currentState() == wdStateBackoff
	})
	// Destruct while the restart is pending: the goroutine must exit
	// (stopWatchdog waits for it) and no orphan Start may follow.
	must(t, rig.ma.Destruct())
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("no restart may happen after Destruct, got %d starts", got)
	}
}

func TestWatchdogOffNeverRestarts(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.watchdogOn = false
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog parks disabled", func() bool {
		return rig.ma.wd.currentState() == wdStateDisabled
	})
	rig.runner.handleAt(0).kill()
	rig.clock.Advance(time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := rig.runner.startCount(); got != 1 {
		t.Fatalf("watchdog off must never restart, got %d starts", got)
	}
}

func TestWatchdogHealthPathOffIsLivenessOnly(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.healthPath = ""
	rig.spec.wdGrace = 0
	deployV1(t, rig)
	rig.prober.setProbeErr(errors.New("must never be called"))
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		return rig.ma.wd.currentState() == wdStateWatching || rig.ma.wd.currentState() == wdStateGrace
	})

	rig.clock.Advance(rig.spec.healthInterval * 3)
	time.Sleep(10 * time.Millisecond)
	if rig.prober.calls() != 0 {
		t.Fatalf("health_path off must not probe, got %d probes", rig.prober.calls())
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("liveness-only watchdog restarted a healthy process")
	}
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "crash restart still works", func() bool {
		return rig.runner.startCount() == 2
	})
}

func TestWatchdogBackoffGrowsAndCaps(t *testing.T) {
	w := &watchdogState{jitter: func() float64 { return 0.5 }} // nominal
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	}
	for i, exp := range want {
		if got := w.nextBackoff(); got != exp {
			t.Fatalf("backoff step %d = %v, want %v", i, got, exp)
		}
	}
	// Sustained health resets the curve; a single good probe does not.
	base := time.Unix(1_700_000_000, 0)
	w.recordHealthy(base, 30*time.Second)
	if got := w.nextBackoff(); got != time.Minute {
		t.Fatalf("one good probe must not reset backoff, got %v", got)
	}
	w.recordHealthy(base, 30*time.Second)
	w.recordHealthy(base.Add(31*time.Second), 30*time.Second)
	if got := w.nextBackoff(); got != time.Second {
		t.Fatalf("sustained health must reset backoff to the floor, got %v", got)
	}
}

func TestWatchdogBackoffJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 1} {
		w := &watchdogState{jitter: func() float64 { return r }}
		got := w.nextBackoff()
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jittered floor backoff %v outside ±20%% band", got)
		}
	}
}

func TestWatchdogBudgetWindowSlides(t *testing.T) {
	w := &watchdogState{}
	base := time.Unix(1_700_000_000, 0)
	if !w.consumeBudget(base, 2, 10*time.Minute) || !w.consumeBudget(base.Add(time.Minute), 2, 10*time.Minute) {
		t.Fatal("first two restarts must fit the budget")
	}
	if w.consumeBudget(base.Add(2*time.Minute), 2, 10*time.Minute) {
		t.Fatal("third restart inside the window must be refused")
	}
	if !w.consumeBudget(base.Add(11*time.Minute), 2, 10*time.Minute) {
		t.Fatal("a restart must be admitted again once the oldest slid out of the window")
	}
}

func TestWatchdogFallsBackToPollingWithoutWaitChannel(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.wdGrace = 0
	// A reattached-style handle has no done channel: Wait returns nil
	// and crash detection must ride the health tick's Alive poll.
	h := &fakeHandle{id: "reattached", alive: true}
	sock := rig.spec.dirs.socket("0a1b2c3d0a1b2c3d")
	rig.ma.mu.Lock()
	inst := &instance{version: "v1", nonce: "0a1b2c3d0a1b2c3d", socket: sock, sock: rig.spec.dirs.socketRef("0a1b2c3d0a1b2c3d"), handle: h}
	rig.ma.current = inst
	rig.ma.mu.Unlock()
	rig.ma.activeSocket.Store(inst.sock)
	must(t, rig.store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d", Handle: handleState{PID: 1}}))
	must(t, mkdirRelease(rig.spec, "v1"))
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})

	h.kill()
	advanceUntil(t, rig, rig.spec.healthInterval, "poll-detected crash restart", func() bool {
		return rig.runner.startCount() == 1
	})
}

func mkdirRelease(spec *appSpec, version string) error {
	return os.MkdirAll(spec.dirs.release(version), 0o755)
}
