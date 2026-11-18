package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

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
