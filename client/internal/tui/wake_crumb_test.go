package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWakeMarksEveryCrumb (#639): after a wake from sleep the crumb of
// every screen that says a kept answer's age says stale beside it, read
// from the screen's cells, until the read behind it answers again; and
// the lines on a screen that say an answer's age of their own (the box
// screen's backups and alerts, an app's alerts) say it too, by the same
// rule. A deploy's record, read once and held, is not marked.
func TestWakeMarksEveryCrumb(t *testing.T) {
	for _, c := range []struct {
		name, cmd string
		j         model.Job
		// lines are the labels of the screen's own rows that say an age
		// of another read, and that read.
		lines map[string]model.Job
	}{
		{"an app", "status blog", model.Job{Kind: model.KStatus, App: "blog"},
			map[string]model.Job{"alerts": {Kind: model.KAlerts, App: "blog"}}},
		{"who am I", "me", model.Job{Kind: model.KWhoAmI}, nil},
		{"the box", "box", model.Job{Kind: model.KBox},
			map[string]model.Job{"backups": {Kind: model.KBackups}, "alerts": {Kind: model.KBoxAlerts}}},
		{"the backups", "backups", model.Job{Kind: model.KBackups}, nil},
		{"the box's alerts", "alerts", model.Job{Kind: model.KBoxAlerts}, nil},
		{"the box's rules", "rules", model.Job{Kind: model.KBoxRules}, nil},
		{"an app's rules", "rules blog", model.Job{Kind: model.KRules, App: "blog"}, nil},
		{"the metrics", "metrics blog 1h", model.Job{Kind: model.KSeries, App: "blog", Range: "1h"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, nil)
			h.press(":", c.cmd, tcell.KeyEnter)
			h.walk(5*time.Second, model.Job{})
			if crumb := screenRows(h)[1].text; !strings.Contains(crumb, " ago") || strings.Contains(crumb, "stale") {
				t.Fatalf("before the wake, the crumb is %q", crumb)
			}
			h.sleep(t)
			if crumb := screenRows(h)[1].text; !strings.Contains(crumb, " ago · stale") {
				t.Errorf("after a wake the crumb is %q, with no stale beside the age", crumb)
			}
			for label, j := range c.lines {
				if l := labelled(h, label); !h.s.Engine.Woke(j) || !strings.Contains(l, " ago") || !strings.Contains(l, "stale") {
					t.Errorf("after a wake the %s line is %q (its read marked: %v)", label, l, h.s.Engine.Woke(j))
				}
			}
			// The reads come as the bucket fills again: whoami's and the
			// list's first, then the screen's.
			h.walk(10*time.Second, model.Job{})
			if h.s.Engine.Woke(c.j) {
				t.Fatalf("%v not read again in 10 s after the wake", c.j)
			}
			if crumb := screenRows(h)[1].text; strings.Contains(crumb, "stale") || !strings.Contains(crumb, " ago") {
				t.Errorf("read again since the wake, the crumb is %q", crumb)
			}
			for label := range c.lines {
				if l := labelled(h, label); strings.Contains(l, "stale") {
					t.Errorf("read again since the wake, the %s line is %q", label, l)
				}
			}
		})
	}
}

// TestWakeLeavesARecordUnmarked: a deploy's record is read once, on
// opening it, and held: a wake does not mark it, and its crumb says its
// age alone.
func TestWakeLeavesARecordUnmarked(t *testing.T) {
	h := start(t, 100, 30, nil)
	h.open(record("blog", "e9a0b3f2c118"))
	h.walk(5*time.Second, model.Job{})
	h.sleep(t)
	if crumb := screenRows(h)[1].text; !strings.Contains(crumb, " ago") || strings.Contains(crumb, "stale") {
		t.Errorf("after a wake a record's crumb is %q", crumb)
	}
}

// sleep ticks the clock once, then again past WakeAfter, as a machine
// asleep between two ticks does, and fails the test unless the second
// is taken for a wake. The first tick an engine is given is never one:
// it has no tick before it.
func (h *harness) sleep(t *testing.T) {
	t.Helper()
	var woke bool
	h.on(func() { h.s.Engine.Tick(h.now) })
	h.now = h.now.Add(model.WakeAfter + time.Second)
	h.on(func() { woke = h.s.Engine.Tick(h.now) })
	if !woke {
		t.Fatal("a tick past WakeAfter after the last not taken for a wake")
	}
}

// labelled is the screen's row whose label (its first word) is label,
// read from its cells; "" when there is none.
func labelled(h *harness, label string) string {
	for _, r := range screenRows(h)[2:] {
		if f := strings.Fields(r.text); len(f) > 0 && f[0] == label {
			return r.text
		}
	}
	return ""
}
