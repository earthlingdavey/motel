package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
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
	h.quit('q')
}

// TestTooSmall: below 60x16 the screen says what it needs.
func TestTooSmall(t *testing.T) {
	h := start(t, 59, 20, nil)
	if s := h.screen(); !strings.Contains(s, "needs 60x16") {
		t.Fatalf("too small a screen shows:\n%s", s)
	}
}

// TestRecoverWithR: a refused key stops every read and says so; once
// the box takes the key again, r asks once and the reads carry on.
func TestRecoverWithR(t *testing.T) {
	h := start(t, 100, 30, nil)
	refused := &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"}
	h.fake.Set("whoami", refused)
	h.fake.Set("blog", refused)
	h.press(tcell.KeyCtrlR)
	s := h.screen()
	if !strings.Contains(s, "SIGNED OUT") || !strings.Contains(s, "not asking: deploy.example.com did not accept your key") {
		t.Fatalf("a 401 is not said:\n%s", s)
	}
	h.press("r") // still refused: stopped again after one whoami
	if h.s.Engine.Stopped() == model.Running {
		t.Fatal("r with the key still refused left the reads running")
	}
	h.fake.Set("whoami", nil)
	h.fake.Set("blog", nil)
	// Held: r is taken at most once in RetryEvery, each a signature.
	before := len(h.fake.Calls)
	h.press("r", "r", "r")
	if h.s.Engine.Stopped() == model.Running || len(h.fake.Calls) != before {
		t.Fatalf("r within %s of the last asked again (%d reads)", model.RetryEvery, len(h.fake.Calls)-before)
	}
	h.now = h.now.Add(model.RetryEvery)
	resigned := 0
	h.u.Resign = func() { resigned++ }
	h.press("r")
	if h.s.Engine.Stopped() != model.Running {
		t.Fatal("r did not resume the reads")
	}
	if resigned != 1 {
		t.Fatalf("r dropped the token %d times, want once", resigned)
	}
	if s := h.screen(); strings.Contains(s, "SIGNED OUT") || strings.Contains(s, "not asking:") {
		t.Fatalf("the stop is still said:\n%s", s)
	}
}

// TestRForgetsFirst: r forgets the refused token before it lets any
// read go: when Resign runs, the reads are still stopped.
func TestRForgetsFirst(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog", &model.Failure{Kind: model.FailSignedOut, Message: "refused"})
	h.press(tcell.KeyCtrlR)
	if h.s.Engine.Stopped() == model.Running {
		t.Fatal("no stop")
	}
	h.fake.Set("blog", nil)
	order := ""
	h.u.Resign = func() {
		if h.s.Engine.Stopped() == model.Running {
			order = "retried before forgetting"
		} else {
			order = "forgot first"
		}
	}
	h.press("r")
	if order != "forgot first" {
		t.Fatalf("r: %q", order)
	}
}

// TestCtrlRRecord: a record whose read failed is read again by ctrl-r
// on its screen.
func TestCtrlRRecord(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog@e9a0b3f2c118", &model.Failure{Kind: model.FailThis, Message: "deploy.example.com answered 500"})
	h.open(app("blog"))
	h.open(record("blog", "e9a0b3f2c118"))
	if s := h.screen(); !strings.Contains(s, "answered 500") {
		t.Fatalf("the failure is not on screen:\n%s", s)
	}
	h.fake.Set("blog@e9a0b3f2c118", nil)
	h.press(tcell.KeyCtrlR)
	if s := h.screen(); !strings.Contains(s, "FAILED in soaking") {
		t.Fatalf("ctrl-r did not read the record again:\n%s", s)
	}
}

// TestNoCtrlRHintWhileSlowed: during a 429's wait the footer offers no
// ctrl-r, which would do nothing.
func TestNoCtrlRHintWhileSlowed(t *testing.T) {
	h := start(t, 100, 30, nil)
	if s := h.screen(); !strings.Contains(s, "ctrl-r ask again") {
		t.Fatalf("no ctrl-r hint to start with:\n%s", s)
	}
	h.fake.Set("blog", &model.Failure{Kind: model.FailSlowed, RetryAfter: time.Minute})
	h.press(tcell.KeyCtrlR)
	s := h.screen()
	if strings.Contains(s, "ctrl-r") || !strings.Contains(s, "slowed by the box (429) until") {
		t.Fatalf("during the wait the screen shows:\n%s", s)
	}
}
