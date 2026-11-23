package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// wokeEngine is an engine every read of whose kinds has answered once:
// whoami, the list, blog's status (open), its alerts, rules, a window of
// its series and its lines, the box's status, backups, alerts and rules;
// shop's status refused with a 403, so held; and a record, read once and
// held. It returns the reads that are not held, and the time.
func wokeEngine(t *testing.T) (*model.Engine, []model.Job, time.Time) {
	t.Helper()
	now := t0
	e := model.NewEngine(now)
	e.SetApps([]string{"blog", "shop"}, now)
	e.Focus("blog", now)
	e.AlertsOpen("blog", now)
	e.RulesOpen("blog", now)
	e.SeriesOpen("blog", "1h", now)
	e.LogsOpen("blog", now)
	e.BoxOpen(true, now)
	e.BackupsOpen(true, now)
	e.BoxAlertsOpen(true, now)
	e.BoxRulesOpen(true, now)
	e.MeOpen(true, now)
	e.Once(heldRecord, now)
	live := []model.Job{
		{Kind: model.KWhoAmI}, {Kind: model.KApps}, {Kind: model.KStatus, App: "blog"},
		{Kind: model.KAlerts, App: "blog"}, {Kind: model.KRules, App: "blog"},
		{Kind: model.KSeries, App: "blog", Range: "1h"}, {Kind: model.KLogs, App: "blog"},
		{Kind: model.KBox}, {Kind: model.KBackups}, {Kind: model.KBoxAlerts}, {Kind: model.KBoxRules},
	}
	read := map[model.Job]bool{}
	for end := now.Add(time.Minute); now.Before(end) && len(read) < len(live)+2; now = now.Add(100 * time.Millisecond) {
		e.Touch(now)
		j, ok, _ := e.Take(now)
		if !ok {
			continue
		}
		var err error
		if j == heldStatus {
			err = fail(model.FailForbidden)
		}
		e.Done(j, now, err)
		read[j] = true
	}
	for _, j := range append(live, heldStatus, heldRecord) {
		if !read[j] {
			t.Fatalf("%v not read in a minute", j)
		}
	}
	return e, live, now
}

var (
	heldStatus = model.Job{Kind: model.KStatus, App: "shop"}
	heldRecord = model.Job{Kind: model.KDeploy, App: "blog", Version: "e9a0b3f2c118"}
)

// TestWakeMarksEveryRead (#639): a wake marks the answer of every read
// the engine keeps, whatever its kind, not the statuses alone; a held
// read (a 403, a record) is not marked, nor a read it has no entry for.
// An answer clears the mark of its own read and no other's; a read that
// fails keeps it.
func TestWakeMarksEveryRead(t *testing.T) {
	e, live, now := wokeEngine(t)
	for _, j := range append(live, heldStatus, heldRecord) {
		if e.Woke(j) {
			t.Fatalf("%v marked before any wake", j)
		}
	}
	e.Tick(now)
	now = now.Add(model.WakeAfter + time.Second)
	if !e.Tick(now) {
		t.Fatal("a tick past WakeAfter not taken for a wake")
	}
	for _, j := range live {
		if !e.Woke(j) {
			t.Errorf("%v not marked after a wake", j)
		}
	}
	for _, j := range []model.Job{heldStatus, heldRecord, {Kind: model.KStatus, App: "nope"}} {
		if e.Woke(j) {
			t.Errorf("%v marked after a wake: it is held, or never read", j)
		}
	}
	// The first read after the wake answered, the next refused.
	answered := map[model.Job]bool{}
	var failed model.Job
	refused := false
	for end := now.Add(time.Minute); now.Before(end) && !refused; now = now.Add(100 * time.Millisecond) {
		j, ok, _ := e.Take(now)
		switch {
		case !ok:
		case len(answered) == 0:
			e.Done(j, now, nil)
			answered[j] = true
		default:
			e.Done(j, now, fail(model.FailThis))
			failed, refused = j, true
		}
	}
	if len(answered) != 1 || !refused {
		t.Fatalf("not two reads in a minute after the wake: answered %v, failed %v", answered, failed)
	}
	for _, j := range live {
		if want := !answered[j]; e.Woke(j) != want {
			t.Errorf("%v marked %v after the first read since the wake (answered: %v; refused: %v)", j, e.Woke(j), answered, failed)
		}
	}
}

// TestStepBackMarksEveryRead: the clock set back past WakeAfter is a
// wake too (seen): every read's answer is marked, as at a tick's.
func TestStepBackMarksEveryRead(t *testing.T) {
	e, live, now := wokeEngine(t)
	e.Tick(now)
	if !e.Tick(now.Add(-model.WakeAfter - time.Second)) {
		t.Fatal("the clock set back past WakeAfter not taken for a wake")
	}
	for _, j := range live {
		if !e.Woke(j) {
			t.Errorf("%v not marked after the clock was set back past WakeAfter", j)
		}
	}
	if e.Woke(heldStatus) || e.Woke(heldRecord) {
		t.Error("a held read marked after the clock was set back")
	}
}

// TestWakeMarksTheListFailed: the apps screen's line of a list kept beside
// a failed read says its age, and stale after a wake, before the failure.
func TestWakeMarksTheListFailed(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	s.Engine.Tick(now)
	now = now.Add(model.WakeAfter + time.Second)
	if !s.Engine.Tick(now) {
		t.Fatal("a tick past WakeAfter not taken for a wake")
	}
	f.Set("apps", fail(model.FailThis))
	for step(t, s, f, &now).Kind != model.KApps {
	}
	if l := s.AppsView(now, "", 0, 0).ListFailed.Text(); !strings.Contains(l, " ago · stale · the last read failed: ") {
		t.Errorf("after a wake, the list's read failed: %q", l)
	}
}

// TestWakeMarkStaysThroughFailures: after a wake a read that gets no
// answer, or a 429, keeps its mark: only an answer clears it.
func TestWakeMarkStaysThroughFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"no answer", fail(model.FailNoAnswer)},
		{"a 429", &model.Failure{Kind: model.FailSlowed, RetryAfter: time.Second}},
		{"refused", fail(model.FailThis)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, now := wokeEngine(t)
			e.Tick(now)
			now = now.Add(model.WakeAfter + time.Second)
			if !e.Tick(now) {
				t.Fatal("a tick past WakeAfter not taken for a wake")
			}
			var j model.Job
			for ok, end := false, now.Add(time.Minute); !ok; {
				if now = now.Add(100 * time.Millisecond); now.After(end) {
					t.Fatal("no read in a minute after the wake")
				}
				e.Tick(now)
				e.Touch(now)
				j, ok, _ = e.Take(now)
			}
			e.Done(j, now, c.err)
			if !e.Woke(j) {
				t.Errorf("%v, its first read after the wake ended in %s, is no longer marked", j, c.name)
			}
		})
	}
}

// TestWakeMarksTheWatchedAppsAlerts: for a person with app grants only,
// the watch reads each listed app's alerts in the background; a wake
// marks those reads too, whose cycle it spreads again.
func TestWakeMarksTheWatchedAppsAlerts(t *testing.T) {
	now := t0
	e := model.NewEngine(now)
	e.SetWatch(model.WatchApps, now)
	e.SetApps([]string{"blog", "shop"}, now)
	shopAlerts := model.Job{Kind: model.KAlerts, App: "shop"}
	answered := false
	for end := now.Add(2 * time.Minute); !answered; {
		if now = now.Add(100 * time.Millisecond); now.After(end) {
			t.Fatal("shop's alerts not read in two minutes")
		}
		e.Tick(now)
		e.Touch(now)
		if j, ok, _ := e.Take(now); ok {
			e.Done(j, now, nil)
			answered = answered || j == shopAlerts
		}
	}
	e.Tick(now)
	now = now.Add(model.WakeAfter + time.Second)
	if !e.Tick(now) {
		t.Fatal("a tick past WakeAfter not taken for a wake")
	}
	if !e.Woke(shopAlerts) {
		t.Error("a background app's alerts, read by the watch, not marked after a wake")
	}
}

// TestStaleIsAWarning: the word beside an age is in the warning style,
// as the failed read's words are.
func TestStaleIsAWarning(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	s.Engine.BoxOpen(true, now)
	for range 20 {
		step(t, s, f, &now)
	}
	s.Engine.Tick(now)
	now = now.Add(model.WakeAfter + time.Second)
	if !s.Engine.Tick(now) {
		t.Fatal("a tick past WakeAfter not taken for a wake")
	}
	found := false
	for _, g := range s.BoxAsOf(now) {
		if strings.Contains(g.T.String(), "stale") {
			found = true
			if g.C != model.Warn {
				t.Errorf("the box's crumb says %q in style %v, not Warn", g.T.String(), g.C)
			}
		}
	}
	if !found {
		t.Errorf("the box's crumb after a wake: %q", s.BoxAsOf(now).Text())
	}
}
