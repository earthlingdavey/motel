package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWakeMarksRowsStale: every app's status answers, and no row is
// stale; then a wake (two ticks of the clock further apart than
// WakeAfter, too close for a row to be stale by its age): every row of
// the apps table says stale on the screen before it is read again, and
// none does once each has been.
func TestWakeMarksRowsStale(t *testing.T) {
	// The sleep. A row is stale by its age past StaleAfter+1 of its
	// intervals (BackgroundEvery at the least), and each was answered
	// within one interval before the sleep: a sleep of StaleAfter
	// intervals or more could make the rows stale without the wake.
	sleep := model.WakeAfter + 15*time.Second
	if sleep >= model.StaleAfter*model.BackgroundEvery {
		t.Fatalf("a sleep of %s makes the rows stale by their age: this test would not see the wake", sleep)
	}
	h := start(t, 100, 30, nil)
	h.walk(2*time.Minute, model.Job{})
	if n, rows := staleRows(h); n != 0 {
		t.Fatalf("%d of %d rows say stale before the wake:\n%s", n, rows, h.screen())
	}
	// The engine's first tick is no wake, having none before it to
	// compare with: one here, so that the one after the sleep is.
	h.on(func() { h.u.S.Engine.Tick(h.now) })
	h.now = h.now.Add(sleep)
	var woke bool
	h.on(func() { woke = h.u.S.Engine.Tick(h.now) })
	if !woke {
		t.Fatal("no wake: the tick after the sleep is not taken as one")
	}
	if n, rows := staleRows(h); n != rows {
		t.Errorf("%d of %d rows say stale after the wake, before any is read again:\n%s", n, rows, h.screen())
	}
	h.walk(2*time.Minute, model.Job{})
	if n, rows := staleRows(h); n != 0 {
		t.Errorf("%d of %d rows say stale once every row was read again:\n%s", n, rows, h.screen())
	}
}

// staleRows is how many of the apps table's rows drawn say stale in
// STATE, and how many are drawn, read from the screen's cells: the rows
// from the table's header to the first blank row, not the title bar,
// the crumb or the strip above it, nor the footer under it. The test
// fails when no row is drawn there.
func staleRows(h *harness) (stale, rows int) {
	h.t.Helper()
	table := false
	for _, r := range screenRows(h) {
		if !table {
			table = strings.HasPrefix(strings.TrimSpace(r.text), "NAME ")
			continue
		}
		if r.text == "" {
			break
		}
		rows++
		if strings.Contains(r.text, "· stale") {
			stale++
		}
	}
	if rows == 0 {
		h.t.Fatalf("no row of the apps table on the screen (its header found: %v):\n%s", table, h.screen())
	}
	return stale, rows
}
