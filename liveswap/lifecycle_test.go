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
	if got := n.names(); !slices.Equal(got, []string{EventDeployStarted, EventDeploySucceeded, EventDeployEnded}) {
		t.Fatalf("events = %v", got)
	}
	ev, _ := n.find(EventDeploySucceeded)
	if ev.App != "demo" || ev.Release != "v1" || ev.DeployedBy != "local:/k.pub sub=alice" || ev.Summary != "deployed v1" {
		t.Fatalf("event = %+v", ev)
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
	if !w.setRefused() || w.setRefused() || !w.takeRefused() || w.takeRefused() {
		t.Fatal("refused flag")
	}
}

// TestRollbackRestoresTheNotifier: a candidate config rejected after
// liveswap's Start (another app's Start failed) had installed its own
// notifier, nil when it had no notify block; the serving config's
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
	rig.ma.setNotifier(nil) // the candidate has no notify block
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
