package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRecordReadOnOpening: a record the box rewrote (a redeploy or a
// rollback of the version) shows as it is now once it is opened again;
// a read on opening it that fails keeps the record on screen, and says
// so beside its age; and enter and esc pressed again and again read it
// no more often than RecordAgainAfter.
func TestRecordReadOnOpening(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("blog"))
	rec := record("blog", "e9a0b3f2c118")
	h.open(rec)
	if s := h.screen(); !strings.Contains(s, "✖ FAILED in soaking") {
		t.Fatalf("the record is not on screen:\n%s", s)
	}
	h.fake.Body("deploys/blog/e9a0b3f2c118.json", `{"version": "e9a0b3f2c118", "status": "succeeded", "started_at": "@-2m", "finished_at": "@-1m"}`)
	h.press(tcell.KeyEscape)
	h.now = h.now.Add(model.RecordAgainAfter)
	h.open(rec)
	if s := h.screen(); !strings.Contains(s, "✓ SUCCEEDED") || strings.Contains(s, "FAILED") {
		t.Fatalf("the record rewritten on the box, opened again:\n%s", s)
	}

	h.fake.Set("blog@e9a0b3f2c118", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com closed the connection"})
	h.press(tcell.KeyEscape)
	h.now = h.now.Add(model.RecordAgainAfter)
	h.open(rec)
	s := h.screen()
	if !strings.Contains(s, "✓ SUCCEEDED") || !strings.Contains(s, "the last read failed: deploy.example.com closed the connection") {
		t.Fatalf("a read again that failed, the screen:\n%s", s)
	}
	if !strings.Contains(s, "as of ") || !strings.Contains(s, "5s ago · the last read failed") {
		t.Fatalf("the crumb does not give the record's age beside the failure:\n%s", s)
	}

	h.fake.Set("blog@e9a0b3f2c118", nil)
	h.now = h.now.Add(model.RecordAgainAfter)
	before := len(h.fake.Calls)
	for range 10 {
		h.press(tcell.KeyEscape)
		h.open(rec)
	}
	n := 0
	for _, c := range h.fake.Calls[before:] {
		if c.Job.Kind == model.KDeploy {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("esc and enter ten times read the record %d times, want once", n)
	}
	if s := h.screen(); strings.Contains(s, "the last read failed") {
		t.Fatalf("the failure is still said after a read that answered:\n%s", s)
	}
}

// TestStepDropsTheRecordLeft: p (or n) leaves a record as esc does: its
// read in flight is cancelled, and the record stepped to is read next.
func TestStepDropsTheRecordLeft(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(app("shop"))
	left := model.Job{Kind: model.KDeploy, App: "shop", Version: "e9a0b3f2c118"}
	h.on(func() { h.u.open(model.View{Screen: model.ScreenRecord, App: "shop", Version: left.Version}) }) // its read not made yet
	j, took, _ := h.s.Engine.Take(h.now)
	if !took || j != left {
		t.Fatalf("took %v %v, want the record's read", j, took)
	}
	cancelled := false
	h.s.Engine.Started(func() { cancelled = true })
	h.press("p")
	if v := h.top(); v.Version != "7c1e55d0a913" {
		t.Fatalf("p opened %+v", v)
	}
	if !cancelled {
		t.Fatal("p left a record whose read was in flight, and did not cancel it")
	}
	h.s.Engine.Done(left, h.now, context.Canceled)
	if j, took, _ := h.s.Engine.Take(h.now); !took || j != (model.Job{Kind: model.KDeploy, App: "shop", Version: "7c1e55d0a913"}) {
		t.Fatalf("after the read left was cancelled, took %v %v", j, took)
	}
}

// TestOneAppStart: a person with one app opens on its screen, with the
// list under it: esc goes back to the list, whose crumb and footer are
// the apps screen's, and an app granted later is listed there. A stop
// still comes over the app's screen and goes when the reads run again;
// an answer about another key still leaves the stop screen alone.
func TestOneAppStart(t *testing.T) {
	one := func(f *demo.Fake) { f.Body("apps.json", `{"apps": [{"name": "blog"}]}`) }
	h := start(t, 100, 30, one)
	opened := []model.View{{Screen: model.ScreenApps}, app("blog")}
	if st := h.stack(); !slices.Equal(st, opened) {
		t.Errorf("one app opens on %+v", st)
	}
	foot := func() string {
		lines := strings.Split(strings.TrimRight(h.screen(), "\n"), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	if f := foot(); !strings.Contains(f, "esc back") {
		t.Fatalf("the app's footer: %q", f)
	}
	h.press(tcell.KeyEscape)
	if v := h.top(); v.Screen != model.ScreenApps {
		t.Fatalf("esc on the app opened at the start left %+v", v)
	}
	lines := strings.Split(h.screen(), "\n")
	if c := strings.Fields(lines[1]); len(c) == 0 || c[0] != "apps" {
		t.Errorf("the crumb after esc: %q", lines[1])
	}
	if f := foot(); f != footers[model.ScreenApps] {
		t.Errorf("the footer after esc: %q", f)
	}
	h.fake.Body("apps.json", `{"apps": [{"name": "blog"}, {"name": "shop"}]}`)
	h.now = h.now.Add(model.AppsEvery)
	h.pump()
	if got := rowsShown(h.screen()); !slices.Equal(got, []string{"shop", "blog"}) && !slices.Equal(got, []string{"blog", "shop"}) {
		t.Errorf("an app granted later, the list shows %v", got)
	}

	// A stop over the app's screen, and back.
	h = start(t, 100, 30, one)
	refused := &model.Failure{Kind: model.FailSignedOut, Message: "deploy.example.com did not accept your key"}
	h.fake.Set("blog", refused)
	h.press(tcell.KeyCtrlR)
	if st := h.stack(); !slices.Equal(st, append(slices.Clone(opened), model.View{Screen: model.ScreenRefused})) {
		t.Fatalf("a 401 on the app opened at the start: %+v", st)
	}
	h.fake.Set("blog", nil)
	h.now = h.now.Add(model.RetryEvery)
	h.press("r")
	if st := h.stack(); !slices.Equal(st, opened) {
		t.Fatalf("after r: %+v", st)
	}

	// An answer about another key: the stop screen alone.
	h.fake.Body("whoami.json", otherKey)
	h.now = h.now.Add(model.MeAlways)
	h.pump()
	if st := h.stack(); !slices.Equal(st, []model.View{{Screen: model.ScreenRefused}}) {
		t.Fatalf("an answer about another key: %+v", st)
	}
}
