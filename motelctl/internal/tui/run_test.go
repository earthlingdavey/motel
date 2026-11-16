package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/demo"
	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

// panicky is the fixtures, but a status read panics.
type panicky struct{ *demo.Fake }

func (p panicky) Status(context.Context, string) (model.Got[wire.Status], error) {
	panic("a read went wrong")
}

// TestPanicEndsThroughTheLoop: a panic in a read ends the screen
// through its event loop, so the terminal is given back, and Run says
// it, with its stack.
func TestPanicEndsThroughTheLoop(t *testing.T) {
	now := t0
	clock := fixedClock{&now}
	f := &demo.Fake{Now: clock.Now}
	s := &model.Session{Host: demo.Host, Now: clock.Now, Engine: model.NewEngine(t0)}
	o := Options{Session: s, Client: panicky{f}, Clock: clock, Screen: &sizedSim{tcell.NewSimulationScreen("UTF-8"), 100, 30}}
	start := Prime(context.Background(), o)
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o, start) }()
	select {
	case err := <-done:
		var p *Panic
		if !errors.As(err, &p) || p.Value != "a read went wrong" || len(p.Stack) == 0 {
			t.Fatalf("Run ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panic in a read did not end the screen")
	}
}

// TestPostAfterTheEnd: once the event loop has ended, post returns at
// once and runs nothing.
func TestPostAfterTheEnd(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.quit('q')
	<-h.done
	close(h.u.ended)
	ran := false
	returned := make(chan struct{})
	go func() { h.u.post(func() { ran = true }); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("post waited on a loop that had ended")
	}
	if ran {
		t.Fatal("post ran after the loop had ended")
	}
}

// TestPanicOnTheLoop: a panic on tview's own event loop (here, while a
// frame is drawn) is recovered once tview has given the terminal back,
// and returned, not printed raw by Go.
func TestPanicOnTheLoop(t *testing.T) {
	now := t0
	clock := fixedClock{&now}
	f := &demo.Fake{Now: clock.Now}
	s := &model.Session{Host: demo.Host, Now: clock.Now, Engine: model.NewEngine(t0)}
	o := Options{Session: s, Client: f, Clock: clock, Screen: &sizedSim{tcell.NewSimulationScreen("UTF-8"), 100, 30}}
	o.Started = func() { panic("drawn wrong \x1b[2J") }
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), o, model.View{Screen: model.ScreenApps}) }()
	select {
	case err := <-done:
		var p *Panic
		if !errors.As(err, &p) || p.Value != "drawn wrong \x1b[2J" {
			t.Fatalf("Run ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panic on the event loop did not end the screen")
	}
}
