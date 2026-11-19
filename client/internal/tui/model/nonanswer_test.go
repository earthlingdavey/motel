package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestWhoAmIMissAskedAtItsBackoff: a whoami that gets no answer (one
// dropped connection), the box answering every other read, is asked
// again at its backoff, 2 s, not at its 5-minute turn: at the start,
// after r from an answer about another key, and after a wake. No run of
// no answer begins.
func TestWhoAmIMissAskedAtItsBackoff(t *testing.T) {
	apps := []string{"blog", "shop"}
	for _, c := range []struct {
		name   string
		before func(e *model.Engine, now *time.Time)
	}{
		{"at the start", func(*model.Engine, *time.Time) {}},
		{"after r from an answer about another key", func(e *model.Engine, now *time.Time) {
			sim(e, apps, *now, time.Minute, 0, ok)
			*now = now.Add(time.Minute)
			e.Again(*now, model.Job{Kind: model.KWhoAmI})
			if !takeKind(e, *now, model.KWhoAmI) {
				t.Fatal("no whoami taken")
			}
			e.Done(model.Job{Kind: model.KWhoAmI}, *now, fail(model.FailAboutAnother))
			*now = now.Add(10 * time.Second)
			e.Touch(*now)
			if !e.Retry(*now) {
				t.Fatal("r did not ask")
			}
		}},
		{"after a wake", func(e *model.Engine, now *time.Time) {
			sim(e, apps, *now, time.Minute, 0, ok)
			*now = now.Add(time.Minute)
			e.Tick(*now)
			*now = now.Add(10 * time.Minute)
			if !e.Tick(*now) {
				t.Fatal("no wake")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := model.NewEngine(t0)
			now := t0
			c.before(e, &now)
			missed := time.Time{}
			calls := sim(e, apps, now, 2*time.Minute, 0, func(j model.Job, at time.Time) error {
				if j.Kind == model.KWhoAmI && missed.IsZero() {
					missed = at
					return fail(model.FailNoAnswer)
				}
				return nil
			})
			var again time.Time
			for _, c := range calls {
				if c.j.Kind == model.KWhoAmI && c.at.After(missed) {
					again = c.at
					break
				}
			}
			switch {
			case missed.IsZero():
				t.Fatal("whoami not read")
			case again.IsZero():
				t.Errorf("whoami got no answer at %v and was not asked again in 2 minutes", missed.Sub(t0))
			case again.Sub(missed) > 3*time.Second:
				t.Errorf("whoami got no answer at %v and was asked again %v later", missed.Sub(t0), again.Sub(missed))
			}
			if h := e.Health(now.Add(2 * time.Minute)); h.Misses != 0 {
				t.Errorf("a run of no answer: %d misses", h.Misses)
			}
		})
	}
}

// TestWhoAmIAloneNeverAnswers: with whoami's route alone getting no
// answer for ever, whoami is asked at its backoff's pace, 2 s doubling
// to once a minute, no run of no answer begins, and every other read
// keeps its turn: the list once a minute, each status every 30 s.
func TestWhoAmIAloneNeverAnswers(t *testing.T) {
	apps := []string{"blog", "shop"}
	e := model.NewEngine(t0)
	end := t0.Add(10 * time.Minute)
	runs := 0
	calls := sim(e, apps, t0, 10*time.Minute, 0, func(j model.Job, at time.Time) error {
		if e.Health(at).Misses > 0 {
			runs++
		}
		if j.Kind == model.KWhoAmI {
			return fail(model.FailNoAnswer)
		}
		return nil
	})
	var me []time.Time
	n := map[model.Job]int{}
	for _, c := range calls {
		n[c.j]++
		if c.j.Kind == model.KWhoAmI {
			me = append(me, c.at)
		}
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60}
	for i := 1; i < len(me); i++ {
		w := 60 * time.Second
		if i <= len(want) {
			w = want[i-1] * time.Second
		}
		if d := me[i].Sub(me[i-1]); d < w || d > w+time.Second {
			t.Errorf("whoami's read %d came %v after the one before, want %v", i+1, d, w)
		}
	}
	if len(me) < 10 {
		t.Errorf("whoami read %d times in 10 minutes", len(me))
	}
	if runs != 0 || e.Health(end).Misses != 0 {
		t.Errorf("a run of no answer began: %d reads within one", runs)
	}
	if l := n[model.Job{Kind: model.KApps}]; l < 10 || l > 12 {
		t.Errorf("the list read %d times in 10 minutes", l)
	}
	for _, a := range apps {
		if s := n[model.Job{Kind: model.KStatus, App: a}]; s < 19 || s > 22 {
			t.Errorf("%s read %d times in 10 minutes", a, s)
		}
	}
}

// TestNoAnswerBeforeAStopForgotten: a read that got no answer before a
// stop (a 401) and r does not count toward a run after it: the first
// read after r that gets none backs off alone, as at a start.
func TestNoAnswerBeforeAStopForgotten(t *testing.T) {
	for name, stop := range map[string]model.Fail{"401": model.FailSignedOut, "an answer about another key": model.FailAboutAnother} {
		e := model.NewEngine(t0)
		now := t0
		sim(e, []string{"blog", "shop"}, now, time.Minute, 0, ok)
		now = now.Add(time.Minute)
		blog := model.Job{Kind: model.KStatus, App: "blog"}
		e.Again(now, blog)
		if !takeApp(e, now, "blog") {
			t.Fatal("blog not taken")
		}
		e.Done(blog, now, fail(model.FailNoAnswer))
		e.Again(now, model.Job{Kind: model.KWhoAmI})
		now = now.Add(5 * time.Second)
		if !takeKind(e, now, model.KWhoAmI) {
			t.Fatal("no whoami taken")
		}
		e.Done(model.Job{Kind: model.KWhoAmI}, now, fail(stop))
		now = now.Add(10 * time.Second)
		e.Touch(now)
		if !e.Retry(now) {
			t.Fatal("r did not ask")
		}
		j, took, _ := e.Take(now)
		if !took || j.Kind != model.KWhoAmI {
			t.Fatalf("after r %v %v", j, took)
		}
		e.Done(j, now, fail(model.FailNoAnswer))
		if h := e.Health(now); h.Misses != 0 {
			t.Errorf("after a stop on %s and r, one whoami with no answer begins a run: %d misses, retry in %v", name, h.Misses, h.RetryIn)
		}
	}
}

// TestWhoAmIMissNotPutOffByClosingMe: who am I opened, its whoami gets
// no answer, and who am I is closed a second later: whoami is still
// asked again at its backoff, not put off to its 5-minute turn.
func TestWhoAmIMissNotPutOffByClosingMe(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, []string{"blog"}, t0, 2*time.Minute, 0, ok)
	now := t0.Add(2 * time.Minute)
	e.MeOpen(true, now)
	if !takeKind(e, now, model.KWhoAmI) {
		t.Fatal("opening who am I asks no whoami")
	}
	e.Done(model.Job{Kind: model.KWhoAmI}, now, fail(model.FailNoAnswer))
	now = now.Add(time.Second)
	e.MeOpen(false, now)
	calls := sim(e, []string{"blog"}, now, time.Minute, 0, ok)
	for _, c := range calls {
		if c.j.Kind == model.KWhoAmI {
			if d := c.at.Sub(now); d > 2*time.Second {
				t.Errorf("whoami asked again %v after who am I closed", d)
			}
			return
		}
	}
	t.Error("whoami not asked again within a minute of who am I closed")
}
