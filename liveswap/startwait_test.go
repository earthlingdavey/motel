package liveswap

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"go.uber.org/zap"
)

// What a start of moteld does for the requests that arrive before
// liveswap has picked its apps back up (#489): they wait for the app's
// first recovery attempt, at most firstRecoveryWait, and a relaunch is
// routed only once the app accepts on its socket.

type upstreamsResult struct {
	ups     []*reverseproxy.Upstream
	err     error
	elapsed time.Duration
}

// getUpstreamsAsync runs one request's GetUpstreams in the background.
func getUpstreamsAsync(ctx context.Context, u *Upstreams) <-chan upstreamsResult {
	ch := make(chan upstreamsResult, 1)
	go func() {
		began := time.Now()
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil).WithContext(ctx))
		ch <- upstreamsResult{ups, err, time.Since(began)}
	}()
	return ch
}

func stillWaiting(t *testing.T, ch <-chan upstreamsResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("the request did not wait: %v %v", r.ups, r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func answered(t *testing.T, ch <-chan upstreamsResult) upstreamsResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the request is still waiting")
		return upstreamsResult{}
	}
}

// recordRunningV7 records v7 as running on recordedNonce's socket, as
// a moteld that stopped while the app kept running leaves it.
func recordRunningV7(t *testing.T, rig *testRig) {
	t.Helper()
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
}

// A request at a start, before recovery has run, waits, and goes to the
// instance recovery reattached: not a 503. The Upstreams is built as
// Caddy builds it, so the wait is the production one.
func TestGetUpstreamsWaitsForTheFirstRecovery(t *testing.T) {
	rig := newTestRig(t)
	recordRunningV7(t, rig)
	rig.runner.reattachOK = true
	u := &Upstreams{App: "demo", ma: rig.ma}
	ch := getUpstreamsAsync(context.Background(), u)
	stillWaiting(t, ch)
	rig.ma.recover(context.Background(), zap.NewNop())
	r := answered(t, ch)
	if r.err != nil || len(r.ups) != 1 {
		t.Fatalf("after recovery: %v %v", r.ups, r.err)
	}
	if want := "unix/" + rig.ma.currentInstance().sock.pinned; r.ups[0].Dial != want {
		t.Fatalf("dial %s, want the reattached instance's %s", r.ups[0].Dial, want)
	}
	if r.elapsed >= firstRecoveryWait {
		t.Fatalf("released after %v: by the cap, not by recovery", r.elapsed)
	}
}

// A first attempt that fails releases the waiting requests at once
// (they get the 503 they always got), though recovery goes on retrying.
func TestGetUpstreamsIsReleasedByAFailedFirstAttempt(t *testing.T) {
	rig := newTestRig(t)
	recordRunningV7(t, rig)
	rig.runner.reattachErrs = []error{errTest}
	u := &Upstreams{App: "demo", ma: rig.ma}
	ch := getUpstreamsAsync(context.Background(), u)
	stillWaiting(t, ch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rig.ma.recover(ctx, zap.NewNop()); close(done) }()
	r := answered(t, ch)
	cancel() // the retry waits on the fake clock
	<-done
	if r.err == nil || !strings.Contains(r.err.Error(), "no running version") {
		t.Fatalf("want the no-version error, got %v %v", r.ups, r.err)
	}
	if r.elapsed >= firstRecoveryWait {
		t.Fatalf("released after %v: by the cap, not by the attempt", r.elapsed)
	}
}

// The wait is bounded: by its cap, and by the client giving up.
func TestGetUpstreamsWaitIsBounded(t *testing.T) {
	t.Run("the cap", func(t *testing.T) {
		rig := newTestRig(t)
		u := &Upstreams{App: "demo", ma: rig.ma, waitFor: 80 * time.Millisecond}
		r := answered(t, getUpstreamsAsync(context.Background(), u))
		if r.err == nil || r.elapsed < 80*time.Millisecond {
			t.Fatalf("got %v after %v, want an error after the 80ms cap", r.err, r.elapsed)
		}
	})
	t.Run("the client", func(t *testing.T) {
		rig := newTestRig(t)
		u := &Upstreams{App: "demo", ma: rig.ma}
		ctx, cancel := context.WithCancel(context.Background())
		ch := getUpstreamsAsync(ctx, u)
		stillWaiting(t, ch)
		cancel()
		if r := answered(t, ch); r.err == nil || r.elapsed >= firstRecoveryWait {
			t.Fatalf("got %v after %v, want an error once the client gave up", r.err, r.elapsed)
		}
	})
}

// Once the first attempt has returned, nothing waits: an app never
// deployed, or one the watchdog is relaunching, fails as fast as before.
func TestGetUpstreamsDoesNotWaitAfterRecovery(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.recover(context.Background(), zap.NewNop()) // nothing recorded
	u := &Upstreams{App: "demo", ma: rig.ma}
	r := answered(t, getUpstreamsAsync(context.Background(), u))
	if r.err == nil || r.elapsed > 40*time.Millisecond {
		t.Fatalf("got %v after %v, want an error at once", r.err, r.elapsed)
	}
}

// A config ended before its recovery made an attempt (a reload in the
// first moments) does not release the waiting requests: the config that
// replaced it recovers, and that attempt does.
func TestRecoveryEndedBeforeAnAttemptDoesNotRelease(t *testing.T) {
	rig := newTestRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rig.ma.recover(ctx, zap.NewNop())
	u := &Upstreams{App: "demo", ma: rig.ma, waitFor: 100 * time.Millisecond}
	ch := getUpstreamsAsync(context.Background(), u)
	stillWaiting(t, ch)
	rig.ma.recover(context.Background(), zap.NewNop())
	if r := answered(t, ch); r.elapsed >= 100*time.Millisecond {
		t.Fatalf("released after %v: by the cap, not by the replacing config's attempt", r.elapsed)
	}
}

// A relaunch (the recorded unit is gone: a reboot, or the app died while
// moteld was down) is routed only after the wait for its socket, and
// routed even when that wait fails.
func TestRelaunchIsRoutedOnceItAccepts(t *testing.T) {
	for _, acceptErr := range []error{nil, errors.New("not accepting on its socket within 2s")} {
		rig := newTestRig(t)
		rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
		rig.store.ok = true
		must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
		rig.prober.acceptErr = acceptErr
		rig.prober.onAccept = func() {
			if rig.ma.activeSocket.Load() != nil {
				t.Error("the relaunch was routed before the wait for its socket")
			}
		}
		must(t, rig.ma.ensureRunning())
		if rig.prober.acceptCalls != 1 {
			t.Fatalf("waitAccepting called %d times, want once", rig.prober.acceptCalls)
		}
		if rig.ma.activeSocket.Load() == nil {
			t.Fatalf("with the wait answering %v, the relaunch is not routed", acceptErr)
		}
	}
	// A reattach does not wait: the app was serving all along.
	rig := newTestRig(t)
	recordRunningV7(t, rig)
	rig.runner.reattachOK = true
	must(t, rig.ma.ensureRunning())
	if rig.prober.acceptCalls != 0 {
		t.Fatalf("a reattach waited for its socket (%d calls)", rig.prober.acceptCalls)
	}
}

// waitAccepting waits for a listener, not for a file: a socket left by a
// dead app does not count.
func TestWaitAccepting(t *testing.T) {
	p := &httpProber{clock: realClock{}}
	alive := func() bool { return true }
	setup := func(t *testing.T) (*testRig, *socketRef) {
		rig := newTestRig(t)
		must(t, rig.spec.dirs.ensure())
		return rig, rig.spec.dirs.socketRef(recordedNonce)
	}
	t.Run("a listener that comes up", func(t *testing.T) {
		rig, sock := setup(t)
		go func() {
			time.Sleep(40 * time.Millisecond)
			bindSocket(t, rig.spec.dirs.socket(recordedNonce))
		}()
		began := time.Now()
		must(t, p.waitAccepting(context.Background(), sock, alive, 2*time.Second))
		if d := time.Since(began); d < 40*time.Millisecond {
			t.Fatalf("returned after %v, before the listener was there", d)
		}
	})
	t.Run("a stale socket file", func(t *testing.T) {
		rig, sock := setup(t)
		touchSocket(t, rig.spec.dirs.socket(recordedNonce))
		err := p.waitAccepting(context.Background(), sock, alive, 60*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "not accepting") {
			t.Fatalf("want a timeout, got %v", err)
		}
	})
	t.Run("a process that exited", func(t *testing.T) {
		_, sock := setup(t)
		err := p.waitAccepting(context.Background(), sock, func() bool { return false }, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "exited") {
			t.Fatalf("want the exit, got %v", err)
		}
	})
}
