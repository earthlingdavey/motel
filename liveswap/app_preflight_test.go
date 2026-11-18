package liveswap

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// wrongArch is the refusal checkMachine gives an executable built for
// another machine, in the words the tests look for.
const wrongArch = "this box is arm64 and ./server is an x86-64 executable: build on a runner of the box's architecture (runs-on: ubuntu-24.04-arm)"

// A pre-flight refusal is the deployer's error: a 422 in phase
// preparing, before any unit exists — nothing started, no pre_start
// run, no detail (nothing of the app's ran), and the release cleaned
// up so the version can be deployed again once fixed.
func TestPreflightRefusalIs422BeforeAnyUnit(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}
	rig.runner.preflightErr = &preflightError{wrongArch}
	err := deployOnceV1(t, rig)
	var ve validationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "pre-flight: this box is arm64") {
		t.Fatalf("want a validationError naming the pre-flight, got %v", err)
	}
	if n := rig.runner.runOnceCount; n != 0 || len(rig.runner.started) != 0 {
		t.Fatalf("nothing may run after a refusal: pre_start runs %d, starts %d", n, len(rig.runner.started))
	}
	ld := rig.ma.status().LastDeploy
	if ld.Status != "failed" || ld.Phase != "preparing" || ld.Detail != nil {
		t.Fatalf("last deploy = %+v (detail %+v)", ld, ld.Detail)
	}
	rig.runner.preflightErr = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v1.tgz", version: "v1", by: "test"}); err != nil {
		t.Fatalf("the refused version deploys once fixed: %v", err)
	}
}

// The command and the pre_start are both checked, in that order, each
// with the spec the launch would use; a rollback checks the command
// only, since it runs no pre_start.
func TestPreflightChecksCommandThenPreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate", "--up"}
	if err := deployOnceV1(t, rig); err != nil {
		t.Fatal(err)
	}
	p := rig.runner.preflights
	if len(p) != 2 || p[0].command[0] != rig.spec.command[0] || p[1].command[0] != "./migrate" || p[1].dir != p[0].dir {
		t.Fatalf("preflights = %+v", p)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://example.test/v2.tgz", version: "v2", by: "test"}); err != nil {
		t.Fatal(err)
	}
	rig.runner.preflights = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true, by: "test"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if p := rig.runner.preflights; len(p) != 1 || p[0].command[0] != rig.spec.command[0] {
		t.Fatalf("a rollback pre-flights the command only: %+v", p)
	}
}

// A pre-flight that could not be made — a bind source that would not
// resolve, a file that would not read — is motel's failure, not the
// deployer's: no 422.
func TestPreflightFailureIsNotTheDeployers(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.preflightErr = errors.New("resolving bind source /var/lib/liveswap/demo/shared: permission denied")
	err := deployOnceV1(t, rig)
	var ve validationError
	if err == nil || errors.As(err, &ve) || !strings.Contains(err.Error(), "pre-flight: resolving bind source") {
		t.Fatalf("want a plain (5xx) error naming the pre-flight, got %v", err)
	}
}

// Every relaunch runs the deploy's pre-flight on the very spec it then
// starts: restart recovery and the watchdog's restarts (launchVersion),
// and a rollback (the deploy pipeline). A release that cannot run on
// this box is so refused before any unit exists, whichever path would
// have launched it.
func TestEveryRelaunchPreflightsWhatItStarts(t *testing.T) {
	same := func(t *testing.T, r *fakeRunner, wantChecks int) {
		t.Helper()
		p, s, n := r.lastPreflightAndStart()
		if n != wantChecks {
			t.Fatalf("pre-flights = %d, want %d", n, wantChecks)
		}
		if p.nonce != s.nonce || p.dir != s.dir || p.version != s.version || !slices.Equal(p.command, s.command) {
			t.Fatalf("the pre-flight checked %+v; the launch started %+v", p, s)
		}
	}
	t.Run("restart recovery", func(t *testing.T) {
		rig := newTestRig(t)
		rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
		rig.store.ok = true
		must(t, mkdirRelease(rig.spec, "v7"))
		must(t, rig.ma.ensureRunning())
		same(t, rig.runner, 1)
	})
	t.Run("watchdog restart", func(t *testing.T) {
		rig := newTestRig(t)
		deployV1(t, rig)
		rig.startWatchdogT(t)
		waitUntil(t, "watchdog to arm", func() bool {
			s := rig.ma.wd.currentState()
			return s == wdStateGrace || s == wdStateWatching
		})
		rig.runner.handleAt(0).kill()
		advanceUntil(t, rig, time.Second, "restart after crash", func() bool {
			return rig.runner.startCount() == 2
		})
		same(t, rig.runner, 2) // the deploy's, then the restart's
	})
	t.Run("rollback", func(t *testing.T) {
		rig := newTestRig(t)
		deployV1(t, rig)
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
		must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
		same(t, rig.runner, 3)
	})
}

// A restart recovery the pre-flight refuses — a release restored onto
// a box of another architecture — is refused once, as permanent: no
// unit is started to die at its exec and be restarted for ever. The
// ledger is settled first (the sweep runs), the launch's socket dir
// goes, and status says why the app is not running.
func TestRecoveryGivesUpOnARelaunchItsPreflightRefuses(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, mkdirRelease(rig.spec, "v7"))
	rig.runner.setPreflightErr(&preflightError{wrongArch})
	rig.startWatchdogT(t) // nothing to supervise: it parks, and status reads it
	done := make(chan struct{})
	go func() { rig.ma.recover(context.Background(), zap.NewNop(), nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery must give up on a refused relaunch, not retry it")
	}
	if _, _, n := rig.runner.lastPreflightAndStart(); n != 1 {
		t.Fatalf("pre-flights = %d, want the one refused", n)
	}
	if n := rig.runner.startCount(); n != 0 {
		t.Fatalf("nothing may start after a refusal, got %d starts", n)
	}
	if n := rig.runner.sweepCount(); n != 1 {
		t.Fatalf("the ledger is settled before the refusal: sweeps = %d, want 1", n)
	}
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("the refused launch's socket dir must go, found %v", got)
	}
	if rig.ma.activeSocket.Load() != nil || rig.ma.currentInstance() != nil {
		t.Fatal("a refused relaunch publishes nothing")
	}
	waitUntil(t, "status to name the refusal", func() bool {
		wd := rig.ma.status().Watchdog
		return wd != nil && wd.LastFailure == "relaunching v7: "+wrongArch
	})
	// The cause fixed, a reload's recovery launches the app, and the
	// refusal goes with that launch — not at the first healthy probe,
	// which a disabled watchdog never makes.
	rig.runner.setPreflightErr(nil)
	must(t, rig.ma.ensureRunning())
	if s := rig.ma.status(); !s.Running || s.Watchdog.LastFailure != "" {
		t.Fatalf("a launch replaces the refusal: %+v %+v", s, s.Watchdog)
	}
}

// A watchdog restart the pre-flight refuses — the release rewritten
// under the running instance into an executable for another machine,
// say — is not a crash to retry under the never-give-up budget: the
// instance is dropped (unrouted, nothing current), the loop parks, and
// status names the refusal. One Warn for the crash and one Error for
// the refusal, however long the app then stays down; a deploy of a
// release that can run brings it back.
func TestWatchdogParksOnARelaunchItsPreflightRefuses(t *testing.T) {
	rig := newTestRig(t)
	core, logs := observer.New(zap.WarnLevel)
	rig.ma.logger = zap.New(core)
	deployV1(t, rig)
	rig.startWatchdogT(t)
	waitUntil(t, "watchdog to arm", func() bool {
		s := rig.ma.wd.currentState()
		return s == wdStateGrace || s == wdStateWatching
	})
	rig.runner.setPreflightErr(&preflightError{wrongArch})
	rig.runner.handleAt(0).kill()
	advanceUntil(t, rig, time.Second, "the relaunch refused", func() bool {
		return rig.ma.currentInstance() == nil
	})
	waitUntil(t, "the loop parks", func() bool { return rig.ma.wd.currentState() == wdStateIdle })
	for range 60 { // an hour: no further attempt
		rig.clock.Advance(time.Minute)
		time.Sleep(time.Millisecond)
	}
	if n := rig.runner.startCount(); n != 1 {
		t.Fatalf("a refused relaunch is not retried: %d starts, want the deploy's 1", n)
	}
	if _, _, n := rig.runner.lastPreflightAndStart(); n != 2 {
		t.Fatalf("pre-flights = %d, want the deploy's and the one refused", n)
	}
	s := rig.ma.status()
	if s.CurrentVersion != "" || s.Running || rig.ma.activeSocket.Load() != nil {
		t.Fatalf("a refused relaunch leaves nothing current or routed: %+v", s)
	}
	if s.Watchdog == nil || s.Watchdog.LastFailure != wrongArch || s.Watchdog.LastRestartCause != "" {
		t.Fatalf("status must name the refusal, and no restart: %+v", s.Watchdog)
	}
	if n := logs.FilterMessage("watchdog: process exited unexpectedly").Len(); n != 1 {
		t.Fatalf("crash Warns = %d, want 1", n)
	}
	if n := logs.FilterMessageSnippet("relaunch refused").Len(); n != 1 {
		t.Fatalf("refusal Errors = %d, want 1: %v", n, logs.All())
	}
	rig.runner.setPreflightErr(nil)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	waitUntil(t, "the watchdog supervises the deployed instance", func() bool {
		st := rig.ma.wd.currentState()
		return st == wdStateGrace || st == wdStateWatching
	})
	if s := rig.ma.status(); s.CurrentVersion != "v2" || !s.Running || s.Watchdog.LastFailure != "" {
		t.Fatalf("a deploy brings the app back and clears the refusal: %+v %+v", s, s.Watchdog)
	}
}

// A rollback is a relaunch through the deploy pipeline: its pre-flight
// refusal is a deploy's 422, before any unit, and the version serving
// keeps serving.
func TestRollbackRefusedAtPreflightKeepsServing(t *testing.T) {
	rig := newTestRig(t)
	deployV1(t, rig)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	rig.runner.setPreflightErr(&preflightError{wrongArch})
	err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true})
	var ve validationError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "pre-flight: "+wrongArch) {
		t.Fatalf("want the pre-flight's 422, got %v", err)
	}
	if n := rig.runner.startCount(); n != 2 {
		t.Fatalf("nothing may start after a refusal: %d starts", n)
	}
	if s := rig.ma.status(); s.CurrentVersion != "v2" || !s.Running {
		t.Fatalf("v2 keeps serving: %+v", s)
	}
}
