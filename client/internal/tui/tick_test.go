package tui

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// sleepClock is a wall clock the test moves, read from Run's
// goroutines; its timers never fire, so no read is made but those Run
// makes at once. asleep is signalled when Run's engine goes to sleep
// on a timer (After) once ready says so.
type sleepClock struct {
	mu     sync.Mutex
	t      time.Time
	ready  func() bool
	asleep chan struct{}
}

func (c *sleepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sleepClock) After(time.Duration) <-chan time.Time {
	if c.ready() {
		select {
		case c.asleep <- struct{}{}:
		default:
		}
	}
	return nil
}

// countingFake is the fixtures, counting the statuses read.
type countingFake struct {
	*demo.Fake
	statuses atomic.Int64
}

func (c *countingFake) Status(ctx context.Context, app string) (model.Got[wire.Status], error) {
	defer c.statuses.Add(1)
	return c.Fake.Status(ctx, app)
}

func (c *sleepClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// shownSim keeps the text of each frame tview shows, as it shows it (on
// the event loop's goroutine), and says when one was shown.
type shownSim struct {
	*sizedSim
	mu    sync.Mutex
	last  string
	shown chan struct{}
}

func (s *shownSim) Show() {
	s.sizedSim.Show()
	text := dump(s.SimulationScreen)
	s.mu.Lock()
	s.last = text
	s.mu.Unlock()
	select {
	case s.shown <- struct{}{}:
	default:
	}
}

func (s *shownSim) frame() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// TestRunNoticesASleep: Run, with its own tick and nothing else, sees
// the laptop sleep: the wall clock jumps two minutes between two of its
// ticks, and every row then says stale on screen, though nothing the
// test calls tells the engine (no Tick here: Run's ticker alone). The
// jump is made once Run's reads sleep, so no read taken before it is
// handed to the engine after, which the engine would see as the clock
// set back (a wake too).
func TestRunNoticesASleep(t *testing.T) {
	clk := &sleepClock{t: t0, asleep: make(chan struct{}, 1)}
	f := &countingFake{Fake: &demo.Fake{Now: clk.Now}}
	clk.ready = func() bool { return f.statuses.Load() >= 12 }
	s := &model.Session{Host: demo.Host, Version: "v0.7.0", Key: demo.Key, Now: clk.Now, Engine: model.NewEngine(t0),
		TokenExpires: func() time.Time { return t0.Add(9 * time.Minute) }}
	scr := &shownSim{sizedSim: &sizedSim{tcell.NewSimulationScreen("UTF-8"), 100, 30}, shown: make(chan struct{}, 1)}
	o := Options{Session: s, Client: f, Clock: clk, Screen: scr, Tick: time.Millisecond}
	start := Prime(context.Background(), o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Run(ctx, o, start); done <- err }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run ended with %v", err)
		}
	}()
	// until waits for frames until one is as wanted; Run's ticker draws
	// one each millisecond.
	until := func(what string, ok func(string) bool) {
		t.Helper()
		bound := time.After(20 * time.Second)
		for range 5000 {
			select {
			case <-scr.shown:
			case err := <-done:
				done <- err // for the deferred wait
				t.Fatalf("Run ended (%v) before %s", err, what)
			case <-bound:
				t.Fatalf("no frame %s in 20 s:\n%s", what, scr.frame())
			}
			if ok(scr.frame()) {
				return
			}
		}
		t.Fatalf("5000 frames, none %s:\n%s", what, scr.frame())
	}
	stale := func(s string) int {
		n := 0
		for _, l := range strings.Split(s, "\n") {
			if strings.Contains(l, "· stale") {
				n++
			}
		}
		return n
	}
	until("with every app read", func(s string) bool {
		return len(rowsShown(s)) == 12 && !strings.Contains(s, "asking") && stale(s) == 0
	})
	select {
	case <-clk.asleep:
	case <-time.After(20 * time.Second):
		t.Fatal("Run's reads did not go to sleep after the twelve statuses")
	}
	clk.add(2 * time.Minute)                                                          // asleep: no tick for two minutes
	until("saying every row is stale", func(s string) bool { return stale(s) >= 12 }) // the rows, and any strip line that says it too
}

// TestRealClockIsTheWall: the TUI's clock carries no monotonic reading,
// so every age and due time it is used for is on the wall clock, which
// runs on while a laptop sleeps (a monotonic reading stops).
func TestRealClockIsTheWall(t *testing.T) {
	if n := (RealClock{}).Now(); n != n.Round(0) {
		t.Fatalf("RealClock's time carries a monotonic reading: %s", n)
	}
}
