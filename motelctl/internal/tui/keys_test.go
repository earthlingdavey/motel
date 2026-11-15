package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/motelctl/internal/tui/model"
)

func rowsShown(scr string) []string {
	var names []string
	in := false
	for _, l := range strings.Split(scr, "\n") {
		if strings.Contains(l, "NAME") && strings.Contains(l, "STATE") {
			in = true
			continue
		}
		if r := []rune(l); in && len(r) > 2 && !strings.HasPrefix(l, "enter") {
			if f := strings.Fields(string(r[2:])); len(f) > 0 {
				names = append(names, f[0])
			}
		}
	}
	return names
}

// TestKeys walks the screens by key: filter, open, back, the record's
// own keys, and the sort, each landing where the footer says.
func TestKeys(t *testing.T) {
	h := start(t, 100, 30, nil)
	top := func() model.View {
		var v model.View
		h.on(func() { v = h.u.top() })
		return v
	}

	h.press("/", "sho")
	if got := rowsShown(h.screen()); len(got) != 1 || got[0] != "shop" {
		t.Fatalf("filter sho shows %v", got)
	}
	h.press(tcell.KeyEnter)
	h.press(tcell.KeyEnter) // the table: open the one row
	if v := top(); v.Screen != model.ScreenApp || v.App != "shop" {
		t.Fatalf("enter opened %+v", v)
	}
	h.press(tcell.KeyEnter) // the newest deploy's record
	if v := top(); v.Screen != model.ScreenRecord || v.Version != "e9a0b3f2c118" {
		t.Fatalf("enter on a deploy opened %+v", v)
	}
	if s := h.screen(); !strings.Contains(s, "✖ FAILED in soaking") || !strings.Contains(s, "log tail: 40 lines") {
		t.Fatalf("the record is not on screen:\n%s", s)
	}
	h.press("p")
	if v := top(); v.Version != "7c1e55d0a913" {
		t.Fatalf("p opened %+v", v)
	}
	h.press("n")
	if v := top(); v.Version != "e9a0b3f2c118" {
		t.Fatalf("n opened %+v", v)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApps {
		t.Fatalf("esc esc is %+v", v)
	}
	if got := rowsShown(h.screen()); len(got) != 1 {
		t.Fatalf("the filter did not stay: %v", got)
	}
	h.press("/", tcell.KeyEscape)
	if got := rowsShown(h.screen()); len(got) != 12 {
		t.Fatalf("esc on the filter left %d rows", len(got))
	}
	h.press("/", "!ok", tcell.KeyEnter)
	if got := rowsShown(h.screen()); len(got) != 5 {
		t.Fatalf("/!ok shows %v, want the five marked", got)
	}
	h.press("/", tcell.KeyEscape)
	h.press(tcell.KeyHome, "j", "j", "d") // the third row, hostile: its newest deploy failed
	if v := top(); v.Screen != model.ScreenRecord || v.App != "hostile" || v.Version != "v7" {
		t.Fatalf("d opened %+v", v)
	}
	h.press(tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApp || v.App != "hostile" {
		t.Fatalf("esc from d's record is %+v", v)
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape, tcell.KeyEscape)
	if v := top(); v.Screen != model.ScreenApps {
		t.Fatalf("esc never leaves the first screen; it is %+v", v)
	}
	h.press("s")
	if s := h.screen(); !strings.Contains(s, "by name") {
		t.Fatalf("s did not sort by name")
	}
	h.press("q")
}

// TestTooSmall: below 60x16 the screen says what it needs.
func TestTooSmall(t *testing.T) {
	h := start(t, 59, 20, nil)
	if s := h.screen(); !strings.Contains(s, "needs 60x16") {
		t.Fatalf("too small a screen shows:\n%s", s)
	}
}
