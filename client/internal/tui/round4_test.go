package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// otherKey is whoami's answer about a key this session did not sign
// with: a made-up fingerprint, plainly not a real one.
const otherKey = `{"name":"mallory@example.com","key":"SHA256:OTHER00000000000000000000000000000000000000","roles":[{"role":"viewer","scope":"box"}],"box":"deploy.example.com","version":"v0.7.0","protocol":1,"token_expires_at":"@9m"}`

func (h *harness) stack() []model.View {
	var st []model.View
	h.on(func() { st = slices.Clone(h.u.stack) })
	return st
}

func (h *harness) row2() string {
	var page string
	h.on(func() { page, _ = h.u.row2.GetFrontPage() })
	return page
}

// TestAnotherKeyForgets: after a good session, the periodic whoami
// names another key. Everything the box said is dropped, the stop
// screen is all the stack holds, and neither it nor help over it opens
// anything: not ? then :apps and Enter, not y, c or P, not the : line
// with its completions. The title names no one. Then r, with whoami
// about this key again, starts from empty and fills as at the start.
func TestAnotherKeyForgets(t *testing.T) {
	h := start(t, 100, 30, nil)
	names := slices.Clone(h.s.Names())
	if len(names) < 2 || h.top().Screen != model.ScreenApps {
		t.Fatalf("the fixtures: %v on %+v", names, h.top())
	}
	h.press(tcell.KeyEnter, "y")
	if h.top().Screen != model.ScreenApp || h.u.overlay != 2 {
		t.Fatalf("not on an app's answer: %+v overlay %d", h.top(), h.u.overlay)
	}
	if s := h.screen(); !strings.Contains(s, "alice@example.com") {
		t.Fatalf("the title before:\n%s", s)
	}

	h.fake.Body("whoami.json", otherKey)
	h.now = h.now.Add(model.MeAlways)
	h.pump()
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenRefused}}) {
		t.Fatalf("the stack after an answer about another key: %+v", st)
	}
	if !h.s.Forgotten() || h.s.Names() != nil || h.u.overlay != 0 {
		t.Fatalf("kept: forgotten %v, names %v, overlay %d", h.s.Forgotten(), h.s.Names(), h.u.overlay)
	}
	for _, v := range []model.View{{Screen: model.ScreenApps}, app(names[0]), {Screen: model.ScreenMe}} {
		if b, _, ok := h.s.Body(v); ok {
			t.Errorf("a body kept for %+v: %d bytes", v, len(b))
		}
	}

	shows := func(when string) {
		t.Helper()
		s := h.screen()
		if strings.Contains(s, "alice@example.com") || strings.Contains(s, "mallory") || !strings.Contains(s, "answered about another key or box") {
			t.Errorf("%s, the screen:\n%s", when, s)
		}
		for _, n := range []string{"billing", "cron-runner", "mailer", "status-page", "wiki"} {
			if !slices.Contains(names, n) {
				t.Fatalf("the fixtures have no %s", n)
			}
			if strings.Contains(s, n) {
				t.Errorf("%s, the screen names %s:\n%s", when, n, s)
			}
		}
	}
	shows("on the stop")

	// The walk round: help, then a view, then the overlays and P.
	h.press("?")
	if h.top().Screen != model.ScreenStopHelp {
		t.Fatalf("? on the stop opened %+v", h.top())
	}
	h.press(":")
	if p := h.row2(); p != "crumb" {
		t.Fatalf(": on help over the stop opened the %s line", p)
	}
	h.press("apps", tcell.KeyEnter, "y", "c", "P", "s", "/", tcell.KeyCtrlR, tcell.KeyTab)
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenRefused}, {Screen: model.ScreenStopHelp}}) || h.u.overlay != 0 || h.u.Print != "" || h.row2() != "crumb" {
		t.Fatalf("keys acted on help over the stop: %+v overlay %d print %q row %s", st, h.u.overlay, h.u.Print, h.row2())
	}
	shows("on help over the stop")
	h.press(tcell.KeyEscape)
	h.press(":", "apps", tcell.KeyEnter, "y", "c", "P")
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenRefused}}) || h.u.overlay != 0 || h.u.Print != "" {
		t.Fatalf("keys acted on the stop: %+v overlay %d print %q", st, h.u.overlay, h.u.Print)
	}
	before := len(h.fake.Calls)
	h.now = h.now.Add(model.MeAlways)
	h.pump()
	if len(h.fake.Calls) != before {
		t.Fatalf("%d reads while stopped", len(h.fake.Calls)-before)
	}

	// r, and the box answers about this key: from empty, as at the start.
	delete(h.fake.Bodies, "whoami.json")
	h.press("r")
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenApps}}) {
		t.Fatalf("after r: %+v", st)
	}
	if !slices.Equal(h.s.Names(), names) {
		t.Fatalf("the list after r: %v", h.s.Names())
	}
	read := map[model.Kind]int{}
	apps := map[string]bool{}
	for _, c := range h.fake.Calls[before:] {
		read[c.Job.Kind]++
		if c.Job.Kind == model.KStatus {
			apps[c.Job.App] = true
		}
	}
	if read[model.KWhoAmI] != 1 || read[model.KApps] != 1 || len(apps) != len(names) {
		t.Fatalf("after r, read %v and the statuses of %v", read, apps)
	}
	if s := h.screen(); !strings.Contains(s, "alice@example.com") || !strings.Contains(s, names[0]) {
		t.Errorf("after r the screen:\n%s", s)
	}
}

// TestNothingOpensFromAStop: the other stops (here a refused key) keep
// what was read, but nothing opens from the stop screen or from help
// over it, and esc on help goes back to the stop.
func TestNothingOpensFromAStop(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("whoami", &model.Failure{Kind: model.FailSignedOut, Message: "no"})
	h.now = h.now.Add(model.MeAlways)
	h.pump()
	if h.top().Screen != model.ScreenRefused {
		t.Fatalf("not stopped: %+v", h.top())
	}
	below := h.stack()
	h.press("?", ":", "apps", tcell.KeyEnter, "y", "c", "P", "d", tcell.KeyCtrlR)
	if h.top().Screen != model.ScreenStopHelp || h.u.overlay != 0 || h.u.Print != "" || h.row2() != "crumb" {
		t.Fatalf("keys acted on help over the stop: %+v overlay %d print %q", h.top(), h.u.overlay, h.u.Print)
	}
	h.press(tcell.KeyEscape)
	if st := h.stack(); !slices.Equal(st, below) {
		t.Fatalf("esc on help over the stop: %+v, want %+v", st, below)
	}
}
