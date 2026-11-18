package model_test

import (
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
