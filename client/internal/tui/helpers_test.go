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
// its say on the fake, and reads every app's status first.
func start(t *testing.T, w, h int, setup func(*demo.Fake)) *harness {
	t.Helper()
	return startOn(t, &sizedSim{tcell.NewSimulationScreen("UTF-8"), w, h}, setup)
}

// startOn is start on a screen of the test's own.
func startOn(t *testing.T, scr tcell.Screen, setup func(*demo.Fake)) *harness {
	t.Helper()
	hs := &harness{t: t, now: t0}
	hs.clock = fixedClock{&hs.now}
	hs.fake = &demo.Fake{Now: hs.clock.Now}
	if setup != nil {
		setup(hs.fake)
	}
	s := &model.Session{Host: demo.Host, Version: "v0.7.0", Key: demo.Key, Now: hs.clock.Now, Engine: model.NewEngine(t0),
		TokenExpires: func() time.Time { return t0.Add(9 * time.Minute) }}
	hs.s = s
	o := Options{Session: s, Client: hs.fake, Clock: hs.clock}
	v := Prime(context.Background(), o)
	hs.pump()
	hs.scr = scr
	hs.u = New(s, false, v)
	hs.u.App.SetScreen(hs.scr)
	done := make(chan struct{})
	hs.done = done
	go func() { defer close(done); _ = hs.u.App.Run() }()
	// Stopped as the product stops it, from inside the event loop:
	// tview clears its screen unlocked as Run returns, so a Stop from
	// outside races it.
	t.Cleanup(func() { hs.u.App.QueueEvent(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone)); <-done })
	hs.sync()
	return hs
}

// pump makes every read the engine would make now, in its order.
func (h *harness) pump() {
	s := h.s
	for range 200 {
		j, ok, _ := s.Engine.Take(h.now)
		if !ok {
			return
		}
		apply, err := s.Fetch(context.Background(), h.fake, j)
		if h.u != nil {
			h.on(apply)
		} else {
			apply()
		}
		s.Engine.Done(j, h.now, err)
	}
}

// on runs f on the event loop and waits until it and its frame are done.
func (h *harness) on(f func()) {
	h.u.App.QueueUpdateDraw(f)
	h.sync()
}

// sync waits for every update queued before it, and its frame.
func (h *harness) sync() {
	done := make(chan struct{})
	h.u.App.QueueUpdate(func() { close(done) })
	<-done
	h.u.App.QueueUpdateDraw(func() {})
	done = make(chan struct{})
	h.u.App.QueueUpdate(func() { close(done) })
	<-done
}

// press sends keys as tview would: the capture first, then the focused
// widget. A string is typed rune by rune; a tcell.Key is that key.
func (h *harness) press(keys ...any) {
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
	h.sync()
}

// open goes to v as the screens do, by enter and esc.
func (h *harness) open(v model.View) {
	h.on(func() { h.u.open(v) })
	h.pump()
	h.sync()
}

// quit sends a key that ends the event loop (q), and waits only for
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
