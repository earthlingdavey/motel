package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWakeMarksRowsStale: every app's status answers, and no row is
// stale; then a wake (two ticks of the clock further apart than
// WakeAfter, too close for a row to be stale by its age): every app's
// row says stale on the screen before it is read again, and none does
// once each has been.
func TestWakeMarksRowsStale(t *testing.T) {
	h := start(t, 100, 30, nil)
	apps := len(h.s.Names())
	if apps == 0 {
		t.Fatal("the box listed no apps")
	}
	h.walk(2*time.Minute, model.Job{})
	if n := staleRows(h); n != 0 {
		t.Fatalf("%d rows say stale before the wake:\n%s", n, h.screen())
	}
	// The engine's first tick is no wake, having none before it to
	// compare with: one here, so that the one after the sleep is.
	h.on(func() { h.u.S.Engine.Tick(h.now) })
	h.now = h.now.Add(model.WakeAfter + 15*time.Second)
	var woke bool
	h.on(func() { woke = h.u.S.Engine.Tick(h.now) })
	if !woke {
		t.Fatal("no wake: the tick after the sleep is not taken as one")
	}
	if n := staleRows(h); n != apps {
		t.Errorf("%d of %d rows say stale after the wake, before any is read again:\n%s", n, apps, h.screen())
	}
	h.walk(2*time.Minute, model.Job{})
	if n := staleRows(h); n != 0 {
		t.Errorf("%d rows say stale once every row was read again:\n%s", n, h.screen())
	}
}

// staleRows is how many of the apps table's rows say stale in STATE,
// read from the screen's cells: the rows under the table's header, not
// the title bar, the crumb or the strip above it.
func staleRows(h *harness) int {
	n, table := 0, false
	for _, r := range screenRows(h) {
		switch {
		case strings.HasPrefix(strings.TrimSpace(r.text), "NAME "):
			table = true
		case table && strings.Contains(r.text, "· stale"):
			n++
		}
	}
	return n
}
