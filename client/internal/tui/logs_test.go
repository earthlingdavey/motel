package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestLogsScreen: l on an app's screen opens its lines, esc goes back to
// it; :logs <app> goes there from anywhere, the app's screen under it,
// and Tab offers it; c gives the command that follows the same lines; a
// row up holds the view and End follows again; an app with no logs
// grant says which grant, and nothing is asked.
func TestLogsScreen(t *testing.T) {
	h := start(t, 80, 24, nil)
	h.open(app("blog"))
	h.press("l")
	if v := h.top(); v.Screen != model.ScreenLogs || v.App != "blog" {
		t.Fatalf("l opened %+v", v)
	}
	if rows := screenRows(h); !strings.HasPrefix(rows[1].text, "apps › blog › logs") || !strings.Contains(h.screen(), "── following: a read every 2 s") {
		t.Errorf("the logs screen:\n%s", h.screen())
	}
	h.press("k")
	if !strings.Contains(h.screen(), "── held: 0 new lines since · End follows") {
		t.Errorf("k does not hold the view:\n%s", h.screen())
	}
	h.press(tcell.KeyEnd)
	if !strings.Contains(h.screen(), "── following") {
		t.Errorf("End does not follow:\n%s", h.screen())
	}
	h.press("c")
	if s := h.screen(); !strings.Contains(s, "motel logs --box deploy.example.com -- blog") {
		t.Errorf("c:\n%s", s)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape)
	if v := h.top(); v != app("blog") {
		t.Fatalf("esc went to %+v", v)
	}
	var offered []string
	h.on(func() { h.u.promptMode = ':'; offered = h.u.complete("lo"); h.u.promptMode = 0 })
	if !slices.Contains(offered, "logs blog") {
		t.Errorf("Tab offers %q", offered)
	}
	h.press(":", "logs shop", tcell.KeyEnter)
	var stack []model.View
	h.on(func() { stack = slices.Clone(h.u.stack) })
	if n := len(stack); stack[n-1] != (model.View{Screen: model.ScreenLogs, App: "shop"}) || stack[n-2] != app("shop") {
		t.Fatalf(":logs shop: %+v", stack)
	}
	if s := h.screen(); !strings.Contains(s, "needs a logs grant on shop") {
		t.Errorf("no grant on shop:\n%s", s)
	}
	for _, c := range h.fake.Calls {
		if c.Job.Kind == model.KLogs && c.Job.App == "shop" {
			t.Errorf("shop's lines were asked for without a grant")
		}
	}
}
