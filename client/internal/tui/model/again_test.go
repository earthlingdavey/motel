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

// TestHeldCtrlRFloorsAcrossAStepBack: the one entry.asked, set in Take
// for a deploy's record, whoami and the box, each with its own floor
// (RecordAgainAfter in next, AgainAfter in Again), and the list's last
// read (ListAgainAfter): ctrl-r held on each
// reads it at most once in 5 s. Then ctrl-r is let go for a second, the
// wall clock is set back (an hour, a wake too; 20 s, not one), and
// ctrl-r is held again: shift moves asked with every other time, so the
// read comes within 5 s, and then at most once in 5 s again; no floor
// holds it back for the length of the step, nor till its kind's own
// interval.
func TestHeldCtrlRFloorsAcrossAStepBack(t *testing.T) {
	if model.RecordAgainAfter != 5*time.Second || model.AgainAfter != 5*time.Second {
		t.Fatalf("RecordAgainAfter is %s, AgainAfter %s", model.RecordAgainAfter, model.AgainAfter)
	}
	rec := model.Job{Kind: model.KDeploy, App: "app-0", Version: "v1"}
	kinds := []struct {
		name string
		job  model.Job
		open func(e *model.Engine, now time.Time)
	}{
		{"a record", rec, func(e *model.Engine, now time.Time) { e.Focus("app-0", now); e.Once(rec, now) }},
		{"who am I", model.Job{Kind: model.KWhoAmI}, func(e *model.Engine, now time.Time) { e.MeOpen(true, now) }},
		{"the box", model.Job{Kind: model.KBox}, func(e *model.Engine, now time.Time) { e.BoxOpen(true, now) }},
		{"the backups", model.Job{Kind: model.KBackups}, func(e *model.Engine, now time.Time) { e.BackupsOpen(true, now) }},
		// The list's floor is its own (ListAgainAfter, from its last read).
		{"the list", model.Job{Kind: model.KApps}, func(*model.Engine, time.Time) {}},
	}
	for _, back := range []time.Duration{time.Hour, 20 * time.Second} {
		for _, c := range kinds {
			name := c.name + ", the clock set back " + back.String()
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
			c.open(e, now)
			const repeat = 33 * time.Millisecond
			hold := func(d time.Duration, press bool) []time.Time {
				var reads []time.Time
				for end := now.Add(d); now.Before(end); now = now.Add(repeat) {
					e.Touch(now)
					if press {
						e.Again(now, c.job)
					}
					take(func(j model.Job) {
						if j == c.job {
							reads = append(reads, now)
						}
					})
				}
				return reads
			}
			gaps := func(when string, reads []time.Time) {
				for i := 1; i < len(reads); i++ {
					if g := reads[i].Sub(reads[i-1]); g < 5*time.Second {
						t.Errorf("%s, %s: read %s after the last", name, when, g)
					}
				}
			}
			before := hold(20*time.Second, true)
			gaps("before the step", before)
			if len(before) < 3 || len(before) > 5 {
				t.Errorf("%s: read %d times in 20 s of ctrl-r", name, len(before))
			}
			hold(time.Second, false)
			now = now.Add(-back)
			step := now
			after := hold(12*time.Second, true)
			gaps("after the step", after)
			switch {
			case len(after) == 0:
				t.Errorf("%s: not read in 12 s of ctrl-r after the step", name)
			case after[0].Sub(step) > 5*time.Second:
				t.Errorf("%s: first read %s after the step", name, after[0].Sub(step))
			case len(after) < 2 || len(after) > 3:
				t.Errorf("%s: read %d times in 12 s of ctrl-r after the step", name, len(after))
			}
		}
	}
}
