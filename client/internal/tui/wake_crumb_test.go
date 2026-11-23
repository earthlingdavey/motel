package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
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
// opening it, and held. A wake marks its entry, and once ctrl-r lifts the
// hold the mark is there (Engine.Woke) until the read answers; its crumb
// still says its age alone (RecordAsOf).
func TestWakeLeavesARecordUnmarked(t *testing.T) {
	h := start(t, 100, 30, nil)
	rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "e9a0b3f2c118"}
	h.open(record(rec.App, rec.Version))
	h.walk(5*time.Second, model.Job{})
	h.sleep(t)
	// The bucket is empty at a wake: ctrl-r lifts the hold, and no read is
	// made yet.
	h.press(tcell.KeyCtrlR)
	if !h.s.Engine.Woke(rec) {
		t.Fatal("the record's hold lifted after a wake, and its read not marked: not the state this test is for")
	}
	if crumb := screenRows(h)[1].text; !strings.Contains(crumb, " ago") || strings.Contains(crumb, "stale") {
		t.Errorf("after a wake, the record asked again, its crumb is %q", crumb)
	}
}

// TestWakeMarkIsEachReadsOwn: after a wake, the word beside an age goes
// only with an answer to the read behind it: with that read refused and
// every other answered, it stays, and the lines of the reads answered
// lose it. A crumb says it before "the last read failed".
func TestWakeMarkIsEachReadsOwn(t *testing.T) {
	for _, c := range []struct {
		name, cmd, fails string
		// still says stale; gone no longer does: "" is the crumb, else a
		// row's label.
		still, gone []string
	}{
		{"the box refused", "box", "box", []string{""}, []string{"backups", "alerts"}},
		{"the box's alerts refused", "box", "box-alerts", []string{"alerts"}, []string{"", "backups"}},
		{"the box's backups refused", "box", "backups", []string{"backups"}, []string{"", "alerts"}},
		{"an app's alerts refused", "status blog", "alerts:blog", []string{"alerts"}, []string{""}},
		{"an app's status refused", "status blog", "blog", []string{""}, []string{"alerts"}},
		{"the list refused, on who am I", "me", "apps", nil, []string{""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := start(t, 100, 30, nil)
			h.press(":", c.cmd, tcell.KeyEnter)
			h.walk(5*time.Second, model.Job{})
			h.sleep(t)
			h.fake.Set(c.fails, &model.Failure{Kind: model.FailThis, Message: "no"})
			h.walk(10*time.Second, model.Job{})
			line := func(label string) string {
				if label == "" {
					return screenRows(h)[1].text
				}
				return labelled(h, label)
			}
			for _, l := range c.still {
				got := line(l)
				if !strings.Contains(got, "stale") {
					t.Errorf("its read refused since the wake, line %q is %q", l, got)
				}
				// The word before the failure's, on every line that says both.
				if f := strings.Index(got, "the last read failed"); f >= 0 && strings.Index(got, "stale") > f {
					t.Errorf("line %q says stale after the failure: %q", l, got)
				}
			}
			for _, l := range c.gone {
				if got := line(l); strings.Contains(got, "stale") || !strings.Contains(got, " ago") {
					t.Errorf("its read answered since the wake, line %q is %q", l, got)
				}
			}
		})
	}
}

// TestWakeCrumbOrder: a crumb whose read failed since a wake says stale
// after the age and before the failure.
func TestWakeCrumbOrder(t *testing.T) {
	for _, c := range [][2]string{{"backups", "backups"}, {"status blog", "blog"}, {"alerts", "box-alerts"}, {"rules", "box-rules"}, {"rules blog", "rules:blog"}, {"metrics blog 1h", "series:blog"}} {
		t.Run(c[0], func(t *testing.T) {
			h := start(t, 100, 30, nil)
			h.press(":", c[0], tcell.KeyEnter)
			h.walk(5*time.Second, model.Job{})
			h.sleep(t)
			h.fake.Set(c[1], &model.Failure{Kind: model.FailThis, Message: "no"})
			h.walk(10*time.Second, model.Job{})
			if crumb := screenRows(h)[1].text; !strings.Contains(crumb, " ago · stale · the last read failed") {
				t.Errorf("its read failed since a wake, the crumb is %q", crumb)
			}
		})
	}
}

// TestWakeAlertsLineAt80: an app's line of its alerts says stale at 80
// columns after a wake, whatever the box said of its monitor and with a
// failed read beside it: the age, the word and the failure come before
// the box's words, so however long those are (an Alertmanager not
// answered, a monitor word of the box's own, capped), the cut at the
// window's edge takes them first.
func TestWakeAlertsLineAt80(t *testing.T) {
	alert := `{"name":"a","severity":"warning","summary":"","starts_at":"@-1m","state":"active"}`
	for _, c := range []struct{ name, body string }{
		{"none held", `{"monitor":"on","alerts":[]}`},
		{"one held", `{"monitor":"on","alerts":[` + alert + `]}`},
		{"two held", `{"monitor":"on","alerts":[` + alert + `,` + alert + `]}`},
		{"no monitor", `{"monitor":"off","alerts":[]}`},
		{"unanswered", `{"monitor":"unanswered","alerts":[]}`},
		{"a word of the box's", `{"monitor":"` + strings.Repeat("x", 300) + `","alerts":[]}`},
	} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, failed %v", c.name, failed), func(t *testing.T) {
				h := start(t, 80, 24, func(f *demo.Fake) { f.Body("alerts/blog.json", c.body) })
				h.press(":", "status blog", tcell.KeyEnter)
				h.walk(5*time.Second, model.Job{})
				h.sleep(t)
				if failed {
					h.fake.Set("alerts:blog", &model.Failure{Kind: model.FailThis, Message: "no"})
					h.walk(10*time.Second, model.Job{})
				} else {
					h.sync()
				}
				want := " ago · stale · "
				if failed {
					want = " ago · stale · the last read failed · "
				}
				if l := labelled(h, "alerts"); !strings.Contains(l, want) {
					t.Errorf("after a wake, at 80 columns, the alerts line is %q", l)
				}
			})
		}
	}
}

// TestHelpSaysWhatStaleIs: the help's AGE line says what a crumb's stale
// means as well as a row's.
func TestHelpSaysWhatStaleIs(t *testing.T) {
	h := start(t, 100, 30, nil)
	var help string
	h.on(func() { help = h.u.help() })
	if !strings.Contains(help, `"stale" until it is read again after the machine slept, and on a row after three missed reads`) {
		t.Errorf("the help says of stale: %q", help)
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
