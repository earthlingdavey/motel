package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestHeldCtrlR: ctrl-r held on the apps screen of 200 apps for a
// minute, a press at every key repeat, reads the list at most once in
// ListAgainAfter and gives the rest of the rate to the statuses, each
// read once before any is read again: the press does not put the
// statuses behind the list, nor a status just read before one waiting.
func TestHeldCtrlR(t *testing.T) {
	if model.ListAgainAfter != 5*time.Second {
		t.Fatalf("ListAgainAfter is %s", model.ListAgainAfter)
	}
	apps := names(200)
	e := model.NewEngine(t0)
	now := t0
	take := func(count func(model.Job)) {
		for {
			j, ok, _ := e.Take(now)
			if !ok {
				return
			}
			if count != nil {
				count(j)
			}
			if j.Kind == model.KApps {
				e.SetApps(apps, now)
			}
			e.Done(j, now, nil)
		}
	}
	// The first pass: every app read once.
	seen := map[string]bool{}
	for len(seen) < len(apps) {
		e.Touch(now)
		take(func(j model.Job) { seen[j.App] = j.Kind == model.KStatus || seen[j.App] })
		now = now.Add(100 * time.Millisecond)
	}

	shown := []model.Job{{Kind: model.KApps}}
	for _, a := range apps {
		shown = append(shown, model.Job{Kind: model.KStatus, App: a})
	}
	const repeat = 33 * time.Millisecond
	var lists []time.Time
	byApp := map[string]int{}
	statuses := 0
	for end := now.Add(time.Minute); now.Before(end); now = now.Add(repeat) {
		e.Touch(now)
		e.Again(now, shown...)
		take(func(j model.Job) {
			switch j.Kind {
			case model.KApps:
				lists = append(lists, now)
			case model.KStatus:
				byApp[j.App]++
				statuses++
			}
		})
	}
	if len(lists) > 13 {
		t.Errorf("the list read %d times in a minute of ctrl-r", len(lists))
	}
	for i := 1; i < len(lists); i++ {
		if g := lists[i].Sub(lists[i-1]); g < model.ListAgainAfter {
			t.Errorf("the list read %s after the last", g)
		}
	}
	if statuses < 70 {
		t.Errorf("%d statuses read in a minute of ctrl-r", statuses)
	}
	for a, n := range byApp {
		if n > 1 {
			t.Errorf("%s read %d times while %d apps waited", a, n, len(apps)-len(byApp))
		}
	}
}

// TestHeldCtrlROnARecord: ctrl-r held on a deploy's record for 10 s, a
// press at every key repeat, reads the record at most once in
// RecordAgainAfter, and the other reads keep their turn: the record
// does not take the whole rate as the one read always due first.
func TestHeldCtrlROnARecord(t *testing.T) {
	if model.RecordAgainAfter != 5*time.Second {
		t.Fatalf("RecordAgainAfter is %s", model.RecordAgainAfter)
	}
	apps := names(200)
	e := model.NewEngine(t0)
	now := t0
	take := func(count func(model.Job)) {
		for {
			j, ok, _ := e.Take(now)
			if !ok {
				return
			}
			if count != nil {
				count(j)
			}
			if j.Kind == model.KApps {
				e.SetApps(apps, now)
			}
			e.Done(j, now, nil)
		}
	}
	seen := map[string]bool{}
	for len(seen) < len(apps) {
		e.Touch(now)
		take(func(j model.Job) { seen[j.App] = j.Kind == model.KStatus || seen[j.App] })
		now = now.Add(100 * time.Millisecond)
	}
	rec := model.Job{Kind: model.KDeploy, App: "app-0", Version: "v1"}
	e.Focus("app-0", now)
	e.Once(rec, now) // the record opened
	var recs []time.Time
	others := 0
	count := func(j model.Job) {
		if j == rec {
			recs = append(recs, now)
		} else {
			others++
		}
	}
	for i := 0; len(recs) == 0 && i < 100; i++ {
		e.Touch(now)
		take(count)
		now = now.Add(100 * time.Millisecond)
	}
	if len(recs) != 1 {
		t.Fatalf("opening the record read it %d times", len(recs))
	}
	shown := []model.Job{rec, {Kind: model.KStatus, App: "app-0"}}
	const repeat = 33 * time.Millisecond
	recs, others = nil, 0
	for end := now.Add(10 * time.Second); now.Before(end); now = now.Add(repeat) {
		e.Touch(now)
		e.Again(now, shown...)
		take(count)
	}
	if len(recs) > 2 {
		t.Errorf("the record read %d times in 10 s of ctrl-r", len(recs))
	}
	for i := 1; i < len(recs); i++ {
		if g := recs[i].Sub(recs[i-1]); g < model.RecordAgainAfter {
			t.Errorf("the record read %s after the last", g)
		}
	}
	if others < 10 {
		t.Errorf("%d other reads in 10 s of ctrl-r on a record (the record read %d times)", others, len(recs))
	}
}
