package tui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// t0 is the tests' clock: it does not move unless a test moves it.
var t0 = time.Date(2026, 11, 15, 14, 12, 40, 0, time.UTC)

type fixedClock struct{ t *time.Time }

func (c fixedClock) Now() time.Time                       { return *c.t }
func (c fixedClock) After(time.Duration) <-chan time.Time { return nil }

// harness is a UI on a simulation screen, its event loop in a
// goroutine, driven only by press and pump: no engine runs on its own,
// so every frame is the same on every run.
type harness struct {
	t     *testing.T
	u     *UI
	scr   tcell.Screen
	fake  *demo.Fake
	s     *model.Session
	now   time.Time
	clock fixedClock
	// done is closed when the event loop has ended.
	done chan struct{}
}

// start opens the TUI on the fixtures at w x h, after setup has had
// its say on the fake, and each of with on the session, and reads
// every app's status first.
func start(t *testing.T, w, h int, setup func(*demo.Fake), with ...func(*model.Session)) *harness {
	t.Helper()
	return startOn(t, &sizedSim{tcell.NewSimulationScreen("UTF-8"), w, h}, setup, with...)
}

// startOn is start on a screen of the test's own.
func startOn(t *testing.T, scr tcell.Screen, setup func(*demo.Fake), with ...func(*model.Session)) *harness {
	t.Helper()
	hs := &harness{t: t, now: t0}
	hs.clock = fixedClock{&hs.now}
	hs.fake = &demo.Fake{Now: hs.clock.Now}
	if setup != nil {
		setup(hs.fake)
	}
	s := &model.Session{Host: demo.Host, Version: "v0.7.0", Key: demo.Key, Now: hs.clock.Now, Engine: model.NewEngine(t0),
		TokenExpires: func() time.Time { return t0.Add(9 * time.Minute) }}
	for _, f := range with {
		f(s)
	}
	hs.s = s
	o := Options{Session: s, Client: hs.fake, Clock: hs.clock}
	v := Prime(context.Background(), o)
	hs.pump()
	hs.scr = scr
	hs.u = New(s, false, v)
	hs.u.App.SetScreen(hs.scr)
	if _, sim := scr.(tcell.SimulationScreen); !sim {
		takeFirstResize(t, scr)
	}
	done := make(chan struct{})
	hs.done = done
	go func() { defer close(done); _ = hs.u.App.Run(); close(hs.u.ended) }()
	// Stopped as the product stops it, from inside the event loop:
	// tview clears its screen unlocked as Run returns, so a Stop from
	// outside races it.
	t.Cleanup(func() { hs.u.App.QueueEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone)); <-done })
	hs.sync()
	return hs
}

// takeFirstResize takes the size a terminfo screen (the hostile
// package's) says at its Init, in SetScreen, before the event loop
// starts, and fails the test if another event comes first. tcell posts
// it from a goroutine of its own, so tview would draw its frame at no
// point any wait here orders: a frame that reads the clock as a test
// moves it, a data race (TestSafeBytesAlertArrives failed so on arm64).
// Run draws the first frame itself; a simulation screen says no size.
func takeFirstResize(t *testing.T, scr tcell.Screen) {
	t.Helper()
	got := make(chan tcell.Event, 1)
	go func() { got <- scr.PollEvent() }()
	select {
	case ev := <-got:
		if _, ok := ev.(*tcell.EventResize); !ok {
			scr.Fini()
			t.Fatalf("the screen's first event is %T, not its size", ev)
		}
	case <-time.After(10 * time.Second):
		// Fini ends the screen's own loops, and the PollEvent waiting.
		scr.Fini()
		t.Fatal("the screen said no size in 10 s")
	}
}

// pump makes every read the engine would make now, in its order, each
// answer applied on the event loop with its frame.
func (h *harness) pump() { h.reads(true) }

// pumpUnseen is pump with no frame drawn, for a step no one reads the
// screen after.
func (h *harness) pumpUnseen() { h.reads(false) }

func (h *harness) reads(frames bool) {
	s := h.s
	for range 200 {
		j, ok, _ := s.Engine.Take(h.now)
		if !ok {
			return
		}
		// As Drive does: the engine records the read, then the answer is
		// applied and the screen looks for a stop.
		apply, err := s.Fetch(context.Background(), h.fake, j)
		s.Engine.Done(j, h.now, err)
		switch {
		case h.u != nil && frames:
			h.on(func() { apply(); h.u.stopped() })
		case h.u != nil:
			h.loop(func() { apply(); h.u.stopped() })
		default:
			apply()
		}
	}
}

// on runs f on the event loop and waits until it and its frame are done:
// one frame, as the product draws one after each key and each answer.
func (h *harness) on(f func()) {
	h.u.App.QueueUpdateDraw(f)
	h.wait()
}

// loop runs f on the event loop and waits for it, drawing no frame: for
// a step no one reads the screen after.
func (h *harness) loop(f func()) {
	h.u.App.QueueUpdate(f)
	h.wait()
}

// sync waits for every update queued before it, then draws a frame and
// waits for it.
func (h *harness) sync() { h.on(func() {}) }

// wait waits for every update queued before it, and their frames: tview
// runs its updates in the order queued, each update's frame inside it.
func (h *harness) wait() {
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() { close(done) })
	<-done
}

// press sends keys as tview would: the capture first, then the focused
// widget, then one frame. A string is typed rune by rune; a tcell.Key
// is that key. The reads the keys make due are made, each answer with
// its frame; with no key at all, one frame is drawn.
func (h *harness) press(keys ...any) {
	sent := false
	for _, k := range keys {
		var evs []*tcell.EventKey
		switch k := k.(type) {
		case string:
			for _, r := range k {
				evs = append(evs, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
			}
		case tcell.Key:
			evs = append(evs, tcell.NewEventKey(k, 0, tcell.ModNone))
		}
		for _, ev := range evs {
			sent = true
			h.on(func() {
				if ev = h.u.key(ev); ev != nil {
					if p := h.u.App.GetFocus(); p != nil {
						p.InputHandler()(ev, func(p2 tview.Primitive) { h.u.App.SetFocus(p2) })
					}
				}
			})
		}
	}
	h.pump()
	if !sent {
		h.sync()
	}
}

// quit sends a key that ends the event loop (q, P), and waits only for
// the key itself: after the loop ends no update is run, so press's
// waits would never return.
func (h *harness) quit(r rune) {
	done := make(chan struct{})
	go h.u.App.QueueUpdate(func() {
		defer close(done)
		h.u.key(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	})
	<-done
}

// open goes to v as the screens do, by enter and esc: one frame, and
// the reads it makes due.
func (h *harness) open(v model.View) {
	h.on(func() { h.u.open(v) })
	h.pump()
}

func app(a string) model.View { return model.View{Screen: model.ScreenApp, App: a} }

func record(a, v string) model.View {
	return model.View{Screen: model.ScreenRecord, App: a, Version: v}
}

// screen is the simulation screen as text, trailing spaces cut.
func (h *harness) screen() string {
	var out string
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() { out = dump(h.scr.(tcell.SimulationScreen)); close(done) })
	<-done
	return out
}

func dump(scr tcell.SimulationScreen) string {
	cells, w, hgt := scr.GetContents()
	var b strings.Builder
	for y := range hgt {
		var l strings.Builder
		for x := range w {
			l.WriteString(string(cells[y*w+x].Runes))
		}
		b.WriteString(strings.TrimRight(l.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// sizedSim sets its size in Init: tview's SetScreen calls Init itself,
// and a simulation screen's Init resets the size to 80x25.
type sizedSim struct {
	tcell.SimulationScreen
	w, h int
}

func (s *sizedSim) Init() error {
	if err := s.SimulationScreen.Init(); err != nil {
		return err
	}
	s.SetSize(s.w, s.h)
	return nil
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s differs; got:\n%s", name, got)
	}
}
