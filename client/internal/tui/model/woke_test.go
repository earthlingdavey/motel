package model_test

import (
	"testing"
	"time"

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
