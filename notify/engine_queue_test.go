package notify

import (
	"context"
	"reflect"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestOneShotPastItsLifeIsGivenUp: a one-shot retried past its endsAt
// would be taken by Alertmanager as resolved and told to no one, though
// the POST succeeds. It is given up as a failed delivery instead, both
// when retried past it (the try at 4:50 fails, the next at 5:20 would
// succeed) and when first tried after it (a post queued behind others
// is tried only when they leave).
func TestOneShotPastItsLifeIsGivenUp(t *testing.T) {
	t.Run("retried at the head", func(t *testing.T) {
		te := newTestEngine(t)
		before := testutil.ToFloat64(te.failures.c)
		te.p.err = &netErr{syscall.ECONNREFUSED}
		te.report(deployFailed())
		te.attempt(context.Background())
		te.advance(4*time.Minute + 50*time.Second)
		if again, _ := te.attempt(context.Background()); again {
			t.Fatal("given up before five minutes")
		}
		te.advance(30 * time.Second)
		te.p.err = nil // Alertmanager answers again
		te.attempt(context.Background())
		if got := te.p.take(); len(got) != 0 {
			t.Fatalf("posted past its endsAt: %+v", got)
		}
		lines := te.lines("monitor: delivery failed")
		if len(lines) != 1 || lines[0].Level != zap.ErrorLevel || lines[0].ContextMap()["key"] != "liveswap/app:blog/deploy_failed" {
			t.Fatalf("delivery failed lines = %+v", lines)
		}
		if after := testutil.ToFloat64(te.failures.c); after != before+1 {
			t.Fatalf("counter %v → %v", before, after)
		}
		if len(te.queue) != 0 {
			t.Fatal("an expired post stayed queued")
		}
	})
	t.Run("first tried after its life", func(t *testing.T) {
		te := newTestEngine(t)
		te.report(deployFailed())
		te.advance(oneShotLife)
		if got := te.drain(); len(got) != 0 {
			t.Fatalf("posted past its endsAt: %+v", got)
		}
		if lines := te.lines("monitor: delivery failed"); len(lines) != 1 {
			t.Fatalf("delivery failed lines = %+v", lines)
		}
	})
	t.Run("the heartbeat, at debug", func(t *testing.T) {
		te := newTestEngine(t)
		te.tick()
		te.advance(heartbeatLife)
		if got := te.drain(); countAlert(got, alertWatchdog) != 0 {
			t.Fatalf("a dead heartbeat was posted: %+v", got)
		}
		if lines := te.lines("monitor: delivery failed"); len(lines) != 1 || lines[0].Level != zap.DebugLevel {
			t.Fatalf("delivery failed lines = %+v", lines)
		}
	})
}

// wallStepped is t with its wall clock moved by d and its monotonic
// reading left as it was: what time.Now returns after a clock step.
// time.Time has no way to make one, so the reading (its ext field) is
// copied back; the test fails if the layout is not that one, so that
// a Go upgrade that moves it is seen, not skipped unread.
func wallStepped(t *testing.T, at time.Time, d time.Duration) time.Time {
	t.Helper()
	u := at.Add(d)
	src := reflect.ValueOf(&at).Elem().FieldByName("ext")
	dst := reflect.ValueOf(&u).Elem().FieldByName("ext")
	if !src.IsValid() || src.Kind() != reflect.Int64 {
		t.Fatalf("time.Time has no int64 ext field in this Go")
	}
	*(*int64)(unsafe.Pointer(dst.UnsafeAddr())) = *(*int64)(unsafe.Pointer(src.UnsafeAddr()))
	if u.Sub(at) != 0 || u.Round(0).Sub(at.Round(0)) != d {
		t.Fatalf("time.Time's monotonic reading is not in ext in this Go")
	}
	return u
}

// TestExpiryIsOnTheWallClock: Alertmanager reads a post's endsAt
// against its own wall clock, so a step of the box's clock is what
// counts, not the monotonic time that passed in motel.
func TestExpiryIsOnTheWallClock(t *testing.T) {
	for _, c := range []struct {
		name          string
		passed, step  time.Duration
		wantDelivered bool
	}{
		// A minute passed, the clock stepped ten forward: past its
		// endsAt to Alertmanager, which would tell no one.
		{"stepped forward", time.Minute, 10 * time.Minute, false},
		// Six minutes passed, the clock stepped ten back: still ahead of
		// its endsAt to Alertmanager, which takes it as firing.
		{"stepped back", 6 * time.Minute, -10 * time.Minute, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			te := newTestEngine(t)
			base := time.Now() // a monotonic reading, as the engine's clock gives
			te.now = func() time.Time { return base }
			te.report(deployFailed())
			stepped := wallStepped(t, base.Add(c.passed), c.step)
			te.now = func() time.Time { return stepped }
			got := countAlert(te.drain(), "deploy_failed")
			if delivered := got == 1; delivered != c.wantDelivered {
				t.Fatalf("delivered %v, want %v (delivery failed lines %+v)", delivered, c.wantDelivered, te.lines("monitor: delivery failed"))
			}
		})
	}
}

// TestEndsArePostedLate: a resolve and a mute's end carry endsAt=now
// when they are queued, so their endsAt is past whenever they are
// posted; they are owed all the same, however late.
func TestEndsArePostedLate(t *testing.T) {
	te := newTestEngine(t)
	te.tick() // the heartbeat now, so that none is queued with the ends
	te.report(watchdog(true))
	te.mute("blog", true)
	te.drain()
	te.report(watchdog(false))
	te.advance(minHold)
	te.tick()
	te.mute("blog", false)
	te.advance(10 * time.Minute)
	got := te.drain()
	var resolved, unmuted bool
	for _, a := range got {
		switch a.Labels["alertname"] {
		case "watchdog_throttled":
			resolved = !a.EndsAt.IsZero() && a.EndsAt.Before(te.at)
		case alertDeployMute:
			unmuted = !a.EndsAt.IsZero() && a.EndsAt.Before(te.at)
		}
	}
	if !resolved || !unmuted {
		t.Fatalf("late ends: resolve posted %v, mute's end posted %v; posts %+v", resolved, unmuted, got)
	}
	if lines := te.lines("monitor: delivery failed"); len(lines) != 0 {
		t.Fatalf("an end was given up: %+v", lines)
	}
}

// TestReleaseGivesUpTheQueue: the last config running notify stops with
// a post still waiting for Alertmanager. It is given up then, a line and
// the counter, and is not posted when a later config brings the block
// back, whatever its age: a firing post would fire an alert no state
// backs.
func TestReleaseGivesUpTheQueue(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	e := newEngine()
	before := testutil.ToFloat64(e.failures.c)
	a := &App{}
	e.configure(a, &recPoster{err: &netErr{syscall.ECONNREFUSED}}, nil, zap.New(core))
	e.report(watchdog(true))
	e.release(a)
	e.mu.Lock()
	queued := len(e.queue)
	e.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d posts left queued after the last config stopped", queued)
	}
	var failed []observer.LoggedEntry
	for _, l := range logs.FilterMessage("monitor: delivery failed").All() {
		if l.Level == zap.ErrorLevel {
			failed = append(failed, l)
		}
	}
	if len(failed) != 1 || failed[0].ContextMap()["key"] != "liveswap/app:blog/watchdog_throttled" {
		t.Fatalf("delivery failed lines = %+v", failed)
	}
	if after := testutil.ToFloat64(e.failures.c); after < before+1 {
		t.Fatalf("counter %v → %v", before, after)
	}
	p := &recPoster{}
	b := &App{}
	e.configure(b, p, nil, zap.NewNop())
	e.release(b)
	if n := countAlert(p.take(), "watchdog_throttled"); n != 0 {
		t.Fatal("the block brought back posted what the stopped one had queued")
	}
}

// queued says whether a post of the key waits in e's queue.
func queued(e *engine, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.queue {
		if p.key == key {
			return true
		}
	}
	return false
}

// TestReleaseOfAnOlderConfigKeepsTheQueue: only the last config running
// notify gives the queue up. A reload's old Stop, after the new config's
// Start, leaves it to the new config, which posts it; a failed reload's
// Stop of the new config leaves it to the old one serving on.
func TestReleaseOfAnOlderConfigKeepsTheQueue(t *testing.T) {
	const key = "liveswap/app:blog/watchdog_throttled"
	t.Run("a reload: the new config posts it", func(t *testing.T) {
		e := newEngine()
		a, b := &App{}, &App{}
		e.configure(a, &recPoster{err: &netErr{syscall.ECONNREFUSED}}, nil, zap.NewNop())
		e.report(watchdog(true))
		// b's Alertmanager is down too until a's Stop has run, so that
		// the post cannot leave through b before it.
		p := &recPoster{err: &netErr{syscall.ECONNREFUSED}}
		e.configure(b, p, nil, zap.NewNop())
		e.release(a)
		defer e.release(b)
		p.mu.Lock()
		p.err = nil
		p.mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			n := 0
			for _, post := range p.posts {
				n += countAlert(post, "watchdog_throttled")
			}
			p.mu.Unlock()
			if n > 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("not posted through the new config; still queued: %v", queued(e, key))
	})
	t.Run("a failed reload: the old config keeps it", func(t *testing.T) {
		e := newEngine()
		a, b := &App{}, &App{}
		down := &recPoster{err: &netErr{syscall.ECONNREFUSED}}
		e.configure(a, down, nil, zap.NewNop())
		e.report(watchdog(true))
		e.configure(b, down, nil, zap.NewNop())
		e.release(b)
		defer e.release(a)
		if !queued(e, key) {
			t.Fatal("a failed reload's Stop gave up the queue the serving config posts")
		}
	})
}
