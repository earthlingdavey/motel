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
