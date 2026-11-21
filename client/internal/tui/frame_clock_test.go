package tui

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// A frame reads the clock once, on every screen that reads it. The
// session and the engine take a time earlier than the last they saw
// for a step of the clock back and move every time they keep; with two
// readings in a frame, the later given first, that was every frame.
func TestFrameReadsTheClockOnce(t *testing.T) {
	h := start(t, 100, 30, nil)
	screens := []struct {
		name string
		open func()
	}{
		{"apps", func() {}},
		{"an app", func() { h.open(app("blog")) }},
		{"a record", func() { h.open(record("blog", "7c1e55d0a913")) }},
		{"who am I", func() { h.open(model.View{Screen: model.ScreenMe}) }},
		{"the box", func() { h.open(model.View{Screen: model.ScreenBox}) }},
		// Last: a stop, which brings the refusal screen up on its own.
		{"the refusal", func() {
			h.fake.Set("blog", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"})
			h.on(func() { h.s.Engine.Again(h.now, model.Job{Kind: model.KStatus, App: "blog"}) })
			h.pump()
			h.sync()
			if h.top().Screen != model.ScreenRefused {
				t.Fatalf("no refusal screen: %+v", h.top())
			}
		}},
	}
	for _, sc := range screens {
		sc.open()
		var read int
		h.on(func() {
			clock := h.s.Now
			h.s.Now = func() time.Time { read++; return clock() }
			read = 0
			h.u.render(h.u.now())
			h.s.Now = clock
		})
		if read != 1 {
			t.Errorf("%s: a frame read the clock %d times, not once", sc.name, read)
		}
	}
}
