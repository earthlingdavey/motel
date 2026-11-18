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

// TestHeldCtrlROnMeAndBox: ctrl-r held for a minute on who am I, or on
// the box screen, a press at every key repeat, reads whoami (or the
// box) at most once in AgainAfter, and every app's status still has its
// turn.
func TestHeldCtrlROnMeAndBox(t *testing.T) {
	if model.AgainAfter != 5*time.Second {
		t.Fatalf("AgainAfter is %s", model.AgainAfter)
	}
	for name, kind := range map[string]model.Kind{"whoami": model.KWhoAmI, "the box": model.KBox} {
		apps := names(20)
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
		for range 100 {
			e.Touch(now)
			take(nil)
			now = now.Add(100 * time.Millisecond)
		}
		if kind == model.KWhoAmI {
			e.MeOpen(true, now)
		} else {
			e.BoxOpen(true, now)
		}
		const repeat = 33 * time.Millisecond
		var reads []time.Time
		byKind, byApp := map[model.Kind]int{}, map[string]int{}
		for end := now.Add(time.Minute); now.Before(end); now = now.Add(repeat) {
			e.Touch(now)
			e.Again(now, model.Job{Kind: kind})
			take(func(j model.Job) {
				byKind[j.Kind]++
				if j.Kind == kind {
					reads = append(reads, now)
				}
				if j.Kind == model.KStatus {
					byApp[j.App]++
				}
			})
		}
		t.Logf("%s: whoami %d, box %d, list %d, statuses %d", name, byKind[model.KWhoAmI], byKind[model.KBox], byKind[model.KApps], byKind[model.KStatus])
		if len(reads) > 13 {
			t.Errorf("%s read %d times in a minute of ctrl-r", name, len(reads))
		}
		for i := 1; i < len(reads); i++ {
			if g := reads[i].Sub(reads[i-1]); g < model.AgainAfter {
				t.Errorf("%s read %s after the last", name, g)
			}
		}
		if len(byApp) != len(apps) {
			t.Errorf("%s: %d of %d apps' statuses read in a minute of ctrl-r", name, len(byApp), len(apps))
		}
	}
}
