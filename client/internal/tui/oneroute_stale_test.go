package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestOneRouteStaleCounted: one app's status the box does not answer
// while it answers the rest makes no run of no answer (the title bar
// says live), and that row goes stale by its own misses alone; the
// apps screen's crumb counts it, and no other row is stale.
func TestOneRouteStaleCounted(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.fake.Set("blog", fiveHundred())
	w := h.walk(5*time.Minute, model.Job{Kind: model.KStatus, App: "blog"})
	if w.last != "live" {
		t.Errorf("the title bar's word at the end: %q (%v)", w.last, w.words)
	}
	scr := h.screen()
	lines := strings.Split(scr, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], " · 1 stale · ") {
		t.Errorf("the crumb does not count one stale row:\n%s", scr)
	}
	stale := 0
	for _, l := range lines {
		if strings.Contains(l, "· stale") {
			stale++
			if !strings.Contains(l, "blog") {
				t.Errorf("a row the box answers is stale: %q", l)
			}
		}
	}
	if stale != 1 {
		t.Errorf("%d rows say stale:\n%s", stale, scr)
	}
}
