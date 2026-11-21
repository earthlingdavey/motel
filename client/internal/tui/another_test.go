package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestAnotherKeyDropsWhatWasRead: everything the box said is dropped
// when whoami answers about another key, whatever was read just before:
// the list and every status (ctrl-r on the apps screen), the box's
// answer (:box), a deploy's record (:deploys), all read two seconds
// before, and a filter typed on the list. Nothing of it is held after
// the stop. Then r, at that same instant, whoami about this key again:
// the session fills from empty, as at the start, and nothing the
// engine or the screens kept of the box before stands in the way: the
// list is read and drawn whole, every app with its state, and the
// record opened is drawn.
func TestAnotherKeyDropsWhatWasRead(t *testing.T) {
	const rec = "e9a0b3f2c118"
	h := start(t, 100, 30, nil)
	names := slices.Clone(h.s.Names())
	switchAt := t0.Add(model.MeAlways) // whoami's own turn
	h.now = switchAt.Add(-2 * time.Second)
	h.press(tcell.KeyCtrlR)
	h.press(":", "box", tcell.KeyEnter)
	if h.top().Screen != model.ScreenBox || h.s.BoxSt == nil {
		t.Fatalf("the box screen: %+v, its answer held %v", h.top(), h.s.BoxSt != nil)
	}
	h.press(tcell.KeyEscape, ":", "deploys blog "+rec, tcell.KeyEnter)
	if g, _, ok := h.s.Record("blog", rec); !ok || g == nil {
		t.Fatalf("the record was not read")
	}
	h.press(tcell.KeyEscape, tcell.KeyEscape, "/", "sho", tcell.KeyEnter)
	if got := rowsShown(h.screen()); !slices.Equal(got, []string{"shop"}) || h.top().Screen != model.ScreenApps {
		t.Fatalf("the filter shows %v on %+v", got, h.top())
	}

	h.now = switchAt
	h.fake.Body("whoami.json", otherKey)
	h.pump()
	if h.s.Engine.Stopped() != model.NotThisKey {
		t.Fatalf("not stopped: %v", h.s.Engine.Stopped())
	}
	if _, _, ok := h.s.Record("blog", rec); ok {
		t.Error("the record is held after an answer about another key")
	}
	if h.s.BoxSt != nil || h.s.BoxErr != nil {
		t.Error("the box's answer is held after an answer about another key")
	}
	if h.s.Me != nil || h.s.List != nil || h.s.Names() != nil {
		t.Error("whoami's answer or the list is held after an answer about another key")
	}
	if v := h.s.AppsView(h.now, "", 0, 0); len(v.Rows) != 0 {
		t.Errorf("%d rows held after an answer about another key", len(v.Rows))
	}
	if !h.s.Forgotten() {
		t.Error("the session says it holds something the box said")
	}

	delete(h.fake.Bodies, "whoami.json")
	h.press("r")
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenApps}}) {
		t.Fatalf("after r: %+v", st)
	}
	s := h.screen()
	if got := rowsShown(s); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(names))) {
		t.Errorf("after r the apps screen lists %v, want every app the box lists, %v:\n%s", got, names, s)
	}
	if strings.Contains(s, "asking") {
		t.Errorf("after r an app's state, or the list, is not read:\n%s", s)
	}
	// The bucket's refill for the reads after r, the watch's among them.
	h.now = h.now.Add(2 * time.Second)
	h.press(":", "deploys blog "+rec, tcell.KeyEnter)
	if s := h.screen(); !strings.Contains(s, "FAILED in soaking") {
		t.Errorf("after r the record opened is not drawn:\n%s", s)
	}
}
