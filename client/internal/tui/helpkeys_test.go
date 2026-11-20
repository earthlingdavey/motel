package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// helpKeys is each screen's keys as its help lists them, written out
// here rather than read from the table: a binding removed from the
// table, or moved off a screen, fails TestHelpIsTheKeys.
var helpKeys = map[model.Screen][]string{
	model.ScreenApps:     {"enter", "d", "/", "s", "j k ↑ ↓", "PgUp PgDn Home End", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenApp:      {"enter", "g", "j k ↑ ↓", "PgUp PgDn Home End", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenMetrics:  {"t", "PgUp PgDn Home End", "↑ ↓", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenRecord:   {"w", "p n", "j k ↑ ↓", "PgUp PgDn Home End", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenMe:       {"PgUp PgDn Home End", "↑ ↓", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenBox:      {"PgUp PgDn Home End", "↑ ↓", "↑ ↓", "c", "y", "P", "ctrl-r", ":", "esc", "?", "q"},
	model.ScreenHelp:     {"PgUp PgDn Home End", "↑ ↓", ":", "esc", "?", "q"},
	model.ScreenRefused:  {"PgUp PgDn Home End", "↑ ↓", "P", "r", "?", "q"},
	model.ScreenStopHelp: {"PgUp PgDn Home End", "↑ ↓", "r", "esc", "q"},
}

// listedKeys is the keys help lists for s, each by the name it gives.
func listedKeys(s model.Screen) []string {
	var out []string
	for _, l := range strings.Split(keyHelp(s), "\n") {
		r := []rune(l)
		out = append(out, strings.TrimRight(string(r[1:20]), " "))
	}
	return out
}

// probe is a key pressed at a screen, and the name help would give it.
type probe struct {
	name string
	key  any // a string of one rune, or a tcell.Key
}

// probes is every key a person could press that the probe tries: each
// printable ASCII character, and the named and control keys.
func probes() []probe {
	var out []probe
	for r := rune(0x20); r < 0x7f; r++ {
		out = append(out, probe{string(r), string(r)})
	}
	named := map[tcell.Key]string{
		tcell.KeyEnter: "enter", tcell.KeyEscape: "esc", tcell.KeyUp: "↑", tcell.KeyDown: "↓",
		tcell.KeyPgUp: "PgUp", tcell.KeyPgDn: "PgDn", tcell.KeyHome: "Home", tcell.KeyEnd: "End",
		tcell.KeyLeft: "left", tcell.KeyRight: "right", tcell.KeyTab: "tab", tcell.KeyBacktab: "backtab",
		tcell.KeyBackspace2: "backspace", tcell.KeyDelete: "delete", tcell.KeyInsert: "insert",
		tcell.KeyF1: "F1", tcell.KeyF5: "F5", tcell.KeyF10: "F10",
	}
	for k, n := range named {
		out = append(out, probe{n, k})
	}
	for k := tcell.KeyCtrlA; k <= tcell.KeyCtrlZ; k++ {
		switch k {
		case tcell.KeyCtrlC, tcell.KeyCtrlH, tcell.KeyCtrlI, tcell.KeyCtrlM:
			continue // tview's quit; backspace, tab and enter, above
		}
		out = append(out, probe{"ctrl-" + string(rune('a'+k-tcell.KeyCtrlA)), k})
	}
	return out
}

// state is everything a key could change: the screens, the overlay and
// the : line, the apps screen's filter and sort, the wrap, what P would
// print, every read made, and the screen as drawn.
func (h *harness) state() string {
	var st string
	h.on(func() {
		front, _ := h.u.row2.GetFrontPage()
		st = fmt.Sprintf("%v|%d|%q|%s|%q|%d|%v|%q|%q", h.u.stack, h.u.overlay, h.u.promptMode, front, h.u.filter, h.u.sort, h.u.wrap, h.u.Print, h.u.Shown)
	})
	return st + "|" + strconv.Itoa(len(h.fake.Calls)) + "\n" + h.screen()
}

// TestHelpIsTheKeys: help lists, for each screen, exactly the keys
// written in helpKeys; and on each screen every key help does not list
// changes nothing: not the screens, the overlay, the : line, a list's
// order or wrap, what P prints, the reads, or what is drawn. A key
// bound anywhere but the table, which help is written from, fails it.
func TestHelpIsTheKeys(t *testing.T) {
	stop := func(h *harness) {
		h.fake.Set("blog", &model.Failure{Kind: model.FailSignedOut, Message: "no"})
		h.on(func() { h.s.Engine.Again(h.now, model.Job{Kind: model.KStatus, App: "blog"}) })
		h.pump()
	}
	screens := []struct {
		s    model.Screen
		open func(h *harness)
	}{
		{model.ScreenApps, func(*harness) {}},
		{model.ScreenApp, func(h *harness) { h.open(app("blog")) }},
		{model.ScreenRecord, func(h *harness) { h.open(record("blog", "e9a0b3f2c118")) }},
		{model.ScreenMe, func(h *harness) { h.open(model.View{Screen: model.ScreenMe}) }},
		{model.ScreenBox, func(h *harness) { h.open(model.View{Screen: model.ScreenBox}) }},
		{model.ScreenMetrics, func(h *harness) { h.open(app("blog")); h.press("g") }},
		{model.ScreenHelp, func(h *harness) { h.open(model.View{Screen: model.ScreenHelp}) }},
		{model.ScreenRefused, stop},
		{model.ScreenStopHelp, func(h *harness) { stop(h); h.press("?") }},
	}
	if len(screens) != len(helpKeys) {
		t.Fatalf("%d screens probed, %d in helpKeys", len(screens), len(helpKeys))
	}
	for _, sc := range screens {
		if got := listedKeys(sc.s); !slices.Equal(got, helpKeys[sc.s]) {
			t.Errorf("screen %d: help lists %q, want %q", sc.s, got, helpKeys[sc.s])
		}
		listed := map[string]bool{}
		for _, k := range helpKeys[sc.s] {
			for _, n := range strings.Fields(k) {
				listed[n] = true
			}
		}
		at := func() (*harness, string) {
			h := start(t, 100, 30, nil)
			sc.open(h)
			if h.top().Screen != sc.s {
				t.Fatalf("opened %+v, not screen %d", h.top(), sc.s)
			}
			return h, h.state()
		}
		var unlisted []probe
		for _, p := range probes() {
			if !listed[p.name] {
				unlisted = append(unlisted, p)
			}
		}
		// All at once first; one at a time only to find the key that
		// changed something.
		h, before := at()
		h.pressAll(unlisted)
		if h.state() == before {
			continue
		}
		h, before = at()
		for _, p := range unlisted {
			h.pressAll([]probe{p})
			if after := h.state(); after != before {
				t.Errorf("screen %d: %s, which help does not list, changed what is shown:\n%s\n---\n%s", sc.s, p.name, before, after)
				h, before = at()
			}
		}
	}
}

// pressAll is press for many keys in one turn of the event loop, then
// the reads they caused and a frame.
func (h *harness) pressAll(ps []probe) {
	var evs []*tcell.EventKey
	for _, p := range ps {
		switch k := p.key.(type) {
		case string:
			evs = append(evs, tcell.NewEventKey(tcell.KeyRune, []rune(k)[0], tcell.ModNone))
		case tcell.Key:
			evs = append(evs, tcell.NewEventKey(k, 0, tcell.ModNone))
		}
	}
	h.on(func() {
		for _, ev := range evs {
			if ev = h.u.key(ev); ev != nil {
				if p := h.u.App.GetFocus(); p != nil {
					p.InputHandler()(ev, func(p2 tview.Primitive) { h.u.App.SetFocus(p2) })
				}
			}
		}
	})
	h.pump()
}
