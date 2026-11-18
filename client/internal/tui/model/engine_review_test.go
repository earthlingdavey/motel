package model_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRefusalEndsAnOutage: after reads that got no answer, a refusal
// (a 403) or a 429 is an answer from the box: the count of misses is
// cleared, and the title bar does not stay STALE.
func TestRefusalEndsAnOutage(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"403", fail(model.FailForbidden)},
		{"429", &model.Failure{Kind: model.FailSlowed, RetryAfter: time.Second}},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			for range 20 {
				step(t, s, f, &now)
			}
			keys := append(s.Names(), "apps", "whoami")
			for _, n := range keys {
				f.Set(n, fail(model.FailNoAnswer))
			}
			for range model.StaleAfter {
				step(t, s, f, &now)
			}
			if _, r := s.Title(now); !strings.HasPrefix(r.Text(), "STALE") {
				t.Fatalf("after %d reads with no answer the title says %q", model.StaleAfter, r.Text())
			}
			for _, n := range keys {
				f.Set(n, c.err)
			}
			step(t, s, f, &now)
			if m := s.Engine.Health(now).Misses; m != 0 {
				t.Errorf("after a %s the misses are %d", c.name, m)
			}
			later := now.Add(2 * time.Second) // past the 429's wait
			if _, r := s.Title(later); strings.HasPrefix(r.Text(), "STALE") {
				t.Errorf("after a %s the title says %q", c.name, r.Text())
			}
		})
	}
}

// TestWakeDuringA429: a wake from sleep while a 429's wait runs leaves
// the bucket empty until the wait ends, as the 429 did: the reads
// resume at the rate, not in a burst.
func TestWakeDuringA429(t *testing.T) {
	apps := names(60)
	e := model.NewEngine(t0)
	sim(e, apps, t0, 30*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(30 * time.Second)
	e.Touch(now)
	j, took, _ := e.Take(now)
	for !took {
		now = now.Add(100 * time.Millisecond)
		j, took, _ = e.Take(now)
	}
	e.Done(j, now, &model.Failure{Kind: model.FailSlowed, RetryAfter: 2 * time.Minute})
	until := e.SlowUntil(now)
	e.Tick(now)
	now = now.Add(time.Minute)
	if !e.Tick(now) {
		t.Fatal("a minute between two ticks not taken for a wake")
	}
	calls := sim(e, apps, until, 2*time.Second, 10*time.Millisecond, ok)
	n := 0
	for _, c := range calls {
		if c.at.Before(until.Add(time.Second)) {
			n++
		}
	}
	if n > 2 {
		t.Errorf("%d reads in the first second after the wait: a burst", n)
	}
}

// TestClockStepsBack: after the wall clock is set back, the reads go
// on: the open app is read within its interval, an hour back as twenty
// seconds back. A step back past WakeAfter is a wake: every row is
// stale until read again.
func TestClockStepsBack(t *testing.T) {
	for _, back := range []time.Duration{time.Hour, 20 * time.Second} {
		t.Run(back.String(), func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			e := s.Engine
			e.Focus("blog", now)
			for range 20 {
				step(t, s, f, &now)
			}
			e.Tick(now)
			now = now.Add(time.Second)
			e.Tick(now)
			now = now.Add(-back)
			if woke := e.Tick(now); woke != (back > model.WakeAfter) {
				t.Fatalf("a step back of %s taken for a wake: %v", back, woke)
			}
			if r := row(s, now, "docs"); r.Stale != (back > model.WakeAfter) {
				t.Errorf("after a step back of %s a row's stale is %v", back, r.Stale)
			}
			from, read := now, false
			for !read && now.Before(from.Add(model.FocusEvery+time.Second)) {
				now = now.Add(100 * time.Millisecond)
				e.Touch(now)
				if j, took, _ := e.Take(now); took {
					apply, err := s.Fetch(t.Context(), f, j)
					apply()
					e.Done(j, now, err)
					read = j == model.Job{Kind: model.KStatus, App: "blog"}
				}
			}
			if !read {
				_, took, wait := e.Take(now)
				t.Fatalf("after a step back of %s the open app is not read in %s (take %v, wait %s)", back, model.FocusEvery+time.Second, took, wait)
			}
		})
	}
}

// TestRecordNoAnswerHeld: a record whose read gets a 5xx (no answer,
// by cli's failure) is held as a refused one is: with its screen open
// for two minutes it is read once, the open app every FocusEvery, and
// the title bar is not STALE. Opening it again asks it again.
func TestRecordNoAnswerHeld(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "e9a0b3f2c118"}
	f.Set("blog@e9a0b3f2c118", &model.Failure{Kind: model.FailNoAnswer, Message: "deploy.example.com answered 500"})
	s.Engine.Focus("blog", now)
	s.Engine.Once(rec, now)
	before := len(f.Calls)
	end := now.Add(2 * time.Minute)
	for now.Before(end) {
		step(t, s, f, &now)
	}
	reads := map[model.Job]int{}
	for _, c := range f.Calls[before:] {
		reads[c.Job]++
	}
	if n := reads[rec]; n != 1 {
		t.Errorf("the record read %d times in two minutes", n)
	}
	if n := reads[model.Job{Kind: model.KStatus, App: "blog"}]; n < 20 {
		t.Errorf("the open app read %d times in two minutes", n)
	}
	if _, r := s.Title(now); strings.HasPrefix(r.Text(), "STALE") {
		t.Errorf("the title says %q", r.Text())
	}
	s.Engine.Once(rec, now)
	if j := step(t, s, f, &now); j != rec {
		t.Errorf("opening the record again read %v", j)
	}
}

// TestRecordLeftInFlight: a record read before is asked again on
// opening it, never twice at once: opened again while that read is in
// flight, it is read once. Left while the read is in flight, the read is
// cancelled and the record held as it was, not read with nothing asking;
// opened again before the cancelled read ended, it is read once after.
func TestRecordLeftInFlight(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(1), t0, time.Second, time.Millisecond, ok)
	now := t0.Add(time.Minute)
	rec := model.Job{Kind: model.KDeploy, App: "app-0", Version: "v1"}
	e.Once(rec, now)
	if !takeRec(e, now, rec) {
		t.Fatal("the record was not read")
	}
	e.Done(rec, now, nil)
	for i, left := range []bool{false, true, true} {
		now = now.Add(time.Minute)
		e.Once(rec, now)
		if !takeRec(e, now, rec) {
			t.Fatalf("%d: the record was not read again on opening it", i)
		}
		later := now.Add(model.RecordAgainAfter)
		if !left {
			e.Once(rec, later) // opened again, the read in flight
			e.Done(rec, later, nil)
			if takeRec(e, later, rec) {
				t.Fatalf("%d: opened again while its read was in flight: read twice", i)
			}
			continue
		}
		cancelled := false
		e.Started(func() { cancelled = true })
		e.Drop(rec) // esc
		if !cancelled {
			t.Fatalf("%d: esc did not cancel the read in flight", i)
		}
		if i == 2 {
			e.Once(rec, later) // enter, the cancelled read not yet ended
		}
		e.Done(rec, later, context.Canceled)
		if read := takeRec(e, later, rec); read != (i == 2) {
			t.Fatalf("%d: after the read left was cancelled, read again: %v", i, read)
		}
		if i == 2 {
			e.Done(rec, later, nil)
			if takeRec(e, later, rec) {
				t.Fatalf("%d: read twice", i)
			}
		}
	}
}

// TestClockBackTitle: after the wall clock is set back an hour, the
// title bar's word is true of the reads made since: not "back" for the
// length of the step after a run of no answer ended before it, nor
// "refused" for answers that came after a refusal made before it.
func TestClockBackTitle(t *testing.T) {
	for _, before := range []string{"back", "refused"} {
		t.Run(before, func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			e := s.Engine
			for range 20 {
				step(t, s, f, &now)
			}
			keys := append(s.Names(), "apps", "whoami")
			switch before {
			case "back":
				for _, n := range keys {
					f.Set(n, fail(model.FailNoAnswer))
				}
				step(t, s, f, &now)
				for _, n := range keys {
					f.Set(n, nil)
				}
				step(t, s, f, &now)
			case "refused":
				for _, n := range keys {
					f.Set(n, fail(model.FailThis))
				}
				step(t, s, f, &now)
				for _, n := range keys {
					f.Set(n, nil)
				}
			}
			if _, r := s.Title(now); !strings.HasPrefix(r.Text(), before) {
				t.Fatalf("before the step the title says %q", r.Text())
			}
			e.Tick(now)
			now = now.Add(time.Second)
			e.Tick(now)
			now = now.Add(-time.Hour)
			e.Tick(now)
			for end := now.Add(time.Minute); now.Before(end); {
				step(t, s, f, &now)
				e.Tick(now)
			}
			if _, r := s.Title(now); !strings.HasPrefix(r.Text(), "live") {
				t.Errorf("a minute after the step the title says %q", r.Text())
			}
		})
	}
}
