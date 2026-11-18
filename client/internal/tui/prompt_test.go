package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

func (h *harness) top() model.View {
	var v model.View
	h.on(func() { v = h.u.top() })
	return v
}

// TestColonSays: what the : line cannot run is said on its line and asks
// nothing: a version outside the grammar, an app not in the list (one
// sentence, whether it exists or not), an unknown command, the box for
// a viewer of apps alone. A mistyped version stops nothing.
func TestColonSays(t *testing.T) {
	h := start(t, 100, 30, nil)
	before := len(h.fake.Calls)
	for _, c := range []struct{ typed, says string }{
		{"deploys blog v1,", `"v1," is not a version`},
		{"status nope", `no app "nope" that you may view here`},
		{"frob [red]x", `no command "frob" here`},
		{"status", "status needs an app"},
	} {
		h.press(":", c.typed, tcell.KeyEnter)
		if s := h.screen(); !strings.Contains(s, c.says) {
			t.Errorf(":%s says nothing of it:\n%s", c.typed, s)
		}
		h.press("j") // any key clears what was said
	}
	if len(h.fake.Calls) != before {
		t.Errorf("%d reads made for what could not be run", len(h.fake.Calls)-before)
	}
	if h.s.Engine.Stopped() != model.Running {
		t.Error("a mistyped command stopped the reads")
	}
	h2 := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("whoami.json", `{"name":"bob@example.com","key":"SHA256:SAMPLE0000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"app:blog"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`)
	})
	h2.press(":", "box", tcell.KeyEnter)
	if v := h2.top(); v.Screen == model.ScreenBox {
		t.Error(":box opened for a viewer of an app alone")
	}
	if s := h2.screen(); !strings.Contains(s, "the box screen is for a viewer of the box") {
		t.Errorf(":box for a viewer of apps says:\n%s", s)
	}
}

// TestEnterRunsWhatIsTyped: Enter runs exactly what is typed, even with
// a longer completion offered; Tab takes the completion; esc leaves the
// line with nothing run; :q quits.
func TestEnterRunsWhatIsTyped(t *testing.T) {
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Body("apps.json", `{"apps": [{"name": "blog"}, {"name": "blog-staging"}]}`)
		f.Body("status/blog-staging.json", `{"app":"blog-staging","phase":"idle","running":true,"available_versions":[]}`)
	})
	h.press(":", "status blog", tcell.KeyEnter)
	if v := h.top(); v.Screen != model.ScreenApp || v.App != "blog" {
		t.Fatalf(":status blog + Enter opened %+v", v)
	}
	h.press(tcell.KeyEscape)
	h.press(":", "status blog-", tcell.KeyTab, tcell.KeyEnter)
	if v := h.top(); v.App != "blog-staging" {
		t.Fatalf("Tab then Enter opened %+v", v)
	}
	h.press(tcell.KeyEscape, ":", "me", tcell.KeyEscape)
	if v := h.top(); v.Screen == model.ScreenMe {
		t.Fatal("esc on the : line ran it")
	}
	h.press(":", "q")
	done := make(chan struct{})
	go h.u.App.QueueUpdate(func() {
		defer close(done)
		h.u.key(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	})
	<-done
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal(":q + Enter did not quit")
	}
}

// TestOverlayKeys: P prints the JSON from its overlay, nothing from
// help's; c and y are not offered on help or the refusal screen; opening
// a view closes the overlay.
func TestOverlayKeys(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.press("?", "c", "P")
	if h.u.Print != "" || h.u.overlay != 0 {
		t.Fatalf("help: c opened %d, P printed %q", h.u.overlay, h.u.Print)
	}
	h.press(tcell.KeyEscape, "c", ":", "me", tcell.KeyEnter)
	if h.u.overlay != 0 {
		t.Fatal("opening a view left the overlay open")
	}
	h.press(tcell.KeyEscape, "y")
	if s := h.screen(); !strings.Contains(s, "P quit and print") {
		t.Fatalf("the JSON overlay does not offer P:\n%s", s)
	}
	h.quit('P')
	if !strings.Contains(h.u.Print, `"apps": [`) {
		t.Fatalf("P on the JSON prints %q", h.u.Print)
	}
	// Both forms: the bytes for a pipe or a file, made safe for a
	// terminal (cli chooses).
	body, _, _ := h.s.Body(model.View{Screen: model.ScreenApps})
	if h.u.Print != string(body)+"\n" && h.u.Print != string(body) || h.u.Shown != model.JSONShown(body) {
		t.Fatalf("P's forms: raw %d bytes, shown %q", len(h.u.Print), h.u.Shown)
	}
}

// TestRefusalScreenOnce: help over the refusal screen returns to it, one
// refusal screen however often; its footer offers r once.
func TestRefusalScreenOnce(t *testing.T) {
	h := start(t, 100, 30, func(f *demo.Fake) {
		f.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"})
	})
	for range 3 {
		h.press("?", tcell.KeyEscape)
	}
	n := 0
	h.on(func() {
		for _, v := range h.u.stack {
			if v.Screen == model.ScreenRefused {
				n++
			}
		}
	})
	if n != 1 || h.top().Screen != model.ScreenRefused {
		t.Fatalf("%d refusal screens; on top %+v", n, h.top())
	}
	if s := h.screen(); strings.Contains(s, "r ask again  r retry") {
		t.Fatalf("the refusal footer offers r twice:\n%s", s)
	}
}

// TestStopShowsAtOnce: a stop mid-session brings the refusal screen with
// no key pressed (the reads driven as the product drives them), and P on
// it leaves the person line to print.
func TestStopShowsAtOnce(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog", &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"})
	ctx, cancel := context.WithCancel(context.Background())
	driven := make(chan struct{})
	go func() { defer close(driven); h.u.Drive(ctx, h.fake, h.clock) }()
	defer func() { cancel(); h.s.Engine.Touch(h.now); <-driven }()
	h.on(func() { h.s.Engine.Again(h.now, model.Job{Kind: model.KStatus, App: "blog"}) })
	deadline := time.Now().Add(10 * time.Second)
	for h.top().Screen != model.ScreenRefused {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal screen without a key press:\n%s", h.screen())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := h.screen(); !strings.Contains(s, "To be added, send the operator this line") {
		t.Fatalf("the refusal screen shows:\n%s", s)
	}
	h.quit('P')
	if !strings.Contains(h.u.Print, demo.Key.Line) {
		t.Fatalf("P on the refusal screen leaves %q", h.u.Print)
	}
}
