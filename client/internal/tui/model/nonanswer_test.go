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
			if !takeMe(e, *now) {
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
		if !takeMe(e, now) {
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
	if !takeMe(e, now) {
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

// TestOwnCountForgottenAtR: a read's own count of failed reads in a
// row does not outlive a stop and r, which is a new start. A status
// that got no answer before a 401, and whoami that got none before a
// 401 or an answer about another key (which keeps whoami's read alone),
// each gets no answer again after r: it counts 1 in a row, the first,
// which is what brings the read of another job forward (Done's probe)
// and starts its backoff at 2 s: whoami is asked again 2 s later, not 4.
func TestOwnCountForgottenAtR(t *testing.T) {
	me := model.Job{Kind: model.KWhoAmI}
	blog := model.Job{Kind: model.KStatus, App: "blog"}
	for _, c := range []struct {
		name string
		j    model.Job
		stop model.Fail
	}{
		{"a status, then a 401", blog, model.FailSignedOut},
		{"whoami, then a 401", me, model.FailSignedOut},
		{"whoami, then an answer about another key", me, model.FailAboutAnother},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := model.NewEngine(t0)
			now := t0
			sim(e, []string{"blog", "shop"}, now, time.Minute, 0, ok)
			now = now.Add(time.Minute)
			// take is c.j's read at now, the others answered.
			take := func() {
				t.Helper()
				e.Touch(now)
				e.Again(now, c.j)
				if c.j.Kind == model.KWhoAmI && !takeMe(e, now) || c.j.Kind == model.KStatus && !takeApp(e, now, c.j.App) {
					t.Fatalf("%v not taken at %v", c.j, now.Sub(t0))
				}
			}
			take()
			e.Done(c.j, now, fail(model.FailNoAnswer))
			e.Again(now, me)
			now = now.Add(5 * time.Second)
			if !takeMe(e, now) {
				t.Fatal("no whoami taken")
			}
			e.Done(me, now, fail(c.stop))
			now = now.Add(10 * time.Second)
			e.Touch(now)
			if !e.Retry(now) {
				t.Fatal("r did not ask")
			}
			if !takeMe(e, now) {
				t.Fatal("r asked no whoami")
			}
			if c.j == me {
				e.Done(me, now, fail(model.FailNoAnswer))
			} else {
				e.Done(me, now, nil)
				now = now.Add(5 * time.Second)
				take()
				e.Done(c.j, now, fail(model.FailNoAnswer))
			}
			if n := e.Misses(c.j); n != 1 {
				t.Errorf("after r, the first read with no answer counts %d in a row, want 1", n)
			}
			if c.j == me {
				j, took, wait := e.Take(now.Add(2 * time.Second))
				if !took || j != me {
					t.Errorf("2 s after r's whoami got no answer, whoami is not taken: %+v (taken %v, wait %v)", j, took, wait)
				}
			}
		})
	}
}

// TestBackoffForgottenAtR: r is a new start for a read's backoff too,
// not only for its count. A status the box does not answer, while it
// answers the rest, backs off to 60 s; a 401 then stops every read,
// and r a few seconds later. Its read is due at once after r, behind
// whoami, rather than waiting out the rest of the backoff it had before
// the stop.
func TestBackoffForgottenAtR(t *testing.T) {
	me := model.Job{Kind: model.KWhoAmI}
	blog := model.Job{Kind: model.KStatus, App: "blog"}
	apps := []string{"blog", "shop"}
	e := model.NewEngine(t0)
	sim(e, apps, t0, time.Minute, 0, ok)
	// The box stops answering blog's status, and answers the rest, until
	// blog's sixth read in a row gets none: its next is then 60 s away.
	now, last := t0.Add(time.Minute), time.Time{}
	for last.IsZero() {
		if now.After(t0.Add(10 * time.Minute)) {
			t.Fatalf("blog got no answer %d times in a row by +10m, want 6", e.Misses(blog))
		}
		e.Touch(now)
		j, took, wait := e.Take(now)
		if !took {
			now = now.Add(min(max(wait, 100*time.Millisecond), time.Second))
			continue
		}
		var err error
		if j == blog {
			err = fail(model.FailNoAnswer)
		}
		if j.Kind == model.KApps {
			e.SetApps(apps, now)
		}
		e.Done(j, now, err)
		if j == blog && e.Misses(blog) >= 6 {
			last = now
		}
	}
	now = last.Add(time.Second)
	e.Again(now, me)
	if !takeMe(e, now) {
		t.Fatal("no whoami taken")
	}
	e.Done(me, now, fail(model.FailSignedOut))
	now = now.Add(5 * time.Second)
	e.Touch(now)
	if !e.Retry(now) {
		t.Fatal("r did not ask")
	}
	after := sim(e, apps, now, time.Minute, 0, ok)
	if len(after) == 0 || after[0].j != me {
		t.Fatalf("r's first read is not whoami: %v", after)
	}
	for _, c := range after {
		if c.j == blog {
			if d := c.at.Sub(now); d > 2*time.Second {
				t.Errorf("blog read %v after r, its backoff from before the stop kept (its last read before was %v before r)", d, now.Sub(last))
			}
			return
		}
	}
	t.Error("blog not read within a minute of r")
}
