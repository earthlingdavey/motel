package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// back is the step of the wall clock back in these tests: under
// WakeAfter, so it is not taken for a wake, and each time kept must
// move by it. Each test sets the clock back right after the last time
// the engine was given, so the step is all the engine sees.
const back = 20 * time.Second

func noAnswer(model.Job, time.Time) error { return fail(model.FailNoAnswer) }

// TestStepBackKeepsTheBucket: the bucket fills across a step back.
// Halfway through the first pass of 200 apps it is spent; set back, the
// reads go on at the rate, not after the length of the step.
func TestStepBackKeepsTheBucket(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(200), t0, time.Minute, time.Millisecond, ok)
	from := t0.Add(time.Minute - back)
	if n := len(sim(e, names(200), from, 10*time.Second, time.Millisecond, ok)); n < 12 {
		t.Errorf("%d reads in the 10 s after a step back of %s, at 1.5 a second", n, back)
	}
}

// TestStepBackKeepsTheBackoff: the box not answering, the next read is
// tried at the backoff's end in real time, not the step's length later.
func TestStepBackKeepsTheBackoff(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	out := t0.Add(10 * time.Second)
	if n := len(sim(e, names(3), out, time.Second, 10*time.Millisecond, noAnswer)); n != 1 {
		t.Fatalf("%d reads in the first second with no answer, want 1 and then the backoff", n)
	}
	h := e.Health(out.Add(time.Second))
	if h.RetryIn <= 0 || h.RetryIn > 2*time.Second {
		t.Fatalf("after no answer the retry is in %s", h.RetryIn)
	}
	from := out.Add(time.Second - back)
	e.Touch(from)
	all := []model.Job{{Kind: model.KApps}}
	for _, a := range names(3) {
		all = append(all, model.Job{Kind: model.KStatus, App: a})
	}
	e.Again(from, all...) // ctrl-r: reads due, held to the backoff
	calls := sim(e, names(3), from, 5*time.Second, 10*time.Millisecond, noAnswer)
	if len(calls) == 0 || calls[0].at.Sub(from) > h.RetryIn+200*time.Millisecond {
		t.Errorf("set back %s with %s of the backoff left: reads %v", back, h.RetryIn, calls)
	}
}

// TestStepBackKeepsA429sWait: a 429's wait runs its length in real
// time across a step back: it says what is left, and the reads resume
// when it ends.
func TestStepBackKeepsA429sWait(t *testing.T) {
	e := model.NewEngine(t0)
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(10 * time.Second)
	e.Touch(now)
	e.Again(now, model.Job{Kind: model.KStatus, App: "app-1"})
	j, took, _ := e.Take(now)
	if !took {
		t.Fatal("nothing taken")
	}
	e.Done(j, now, &model.Failure{Kind: model.FailSlowed, RetryAfter: 10 * time.Second})
	from := now.Add(-back)
	e.Touch(from)
	if h := e.Health(from.Add(time.Second)); h.SlowIn != 9*time.Second {
		t.Errorf("a 10 s wait set back %s a second in: %s left", back, h.SlowIn)
	}
	calls := sim(e, names(3), from.Add(time.Second), 15*time.Second, 10*time.Millisecond, ok)
	if len(calls) == 0 || calls[0].at.Sub(from) < 10*time.Second || calls[0].at.Sub(from) > 11*time.Second+100*time.Millisecond {
		t.Errorf("a 10 s wait set back %s: reads %v", back, calls)
	}
}

// TestStepBackKeepsTheIdleMinutes: ten minutes after the last key, in
// real time, the title says PAUSED though the clock was set back in
// them.
func TestStepBackKeepsTheIdleMinutes(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now) // a key before each
	}
	key := now
	s.Engine.Tick(key.Add(time.Minute))
	s.Engine.Tick(key.Add(time.Minute - back))
	if _, r := s.Title(key.Add(model.Idle - back)); !strings.HasPrefix(r.Text(), "PAUSED") {
		t.Errorf("ten minutes after the last key, the clock set back %s in them: the title says %q", back, r.Text())
	}
}

// TestStepBackKeepsRsFloor: r is taken once RetryEvery has passed since
// the last, in real time, the clock set back between the two.
func TestStepBackKeepsRsFloor(t *testing.T) {
	e := model.NewEngine(t0)
	j, _, _ := e.Take(t0)
	e.Done(j, t0, fail(model.FailSignedOut))
	r := t0.Add(time.Minute)
	if !e.Retry(r) {
		t.Fatal("r refused")
	}
	j, _, _ = e.Take(r)
	e.Done(j, r, fail(model.FailSignedOut)) // still refused
	e.Touch(r.Add(-back))
	if again := r.Add(model.RetryEvery + time.Second - back); !e.RetryReady(again) || !e.Retry(again) {
		t.Errorf("r %s after the last, the clock set back %s between: not taken", model.RetryEvery+time.Second, back)
	}
}

// TestStepBackKeepsTheLastTick: a sleep that comes after a step back,
// before the next tick, is still a sleep: the gap between the two ticks
// is measured in real time.
func TestStepBackKeepsTheLastTick(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	e := s.Engine
	e.Tick(now)
	e.Touch(now.Add(-back))                                    // set back, and seen at once
	slept := now.Add(-back + model.WakeAfter + 10*time.Second) // then asleep 40 s
	if !e.Tick(slept) {
		t.Fatalf("%s between two ticks, the clock set back %s first: not a wake", model.WakeAfter+10*time.Second, back)
	}
	if r := row(s, slept, "docs"); !r.Stale {
		t.Error("a row not re-read since the wake is not stale")
	}
}

// TestStepBackKeepsTheDueTimes: every app keeps its turn in real time
// across a step back: each is read again within its interval of its
// last read before the step, not the step's length later.
func TestStepBackKeepsTheDueTimes(t *testing.T) {
	apps := names(12)
	e := model.NewEngine(t0)
	before := sim(e, apps, t0, 2*time.Minute, 10*time.Millisecond, ok)
	last := map[string]time.Time{}
	for _, c := range before {
		if c.j.Kind == model.KStatus {
			last[c.j.App] = c.at
		}
	}
	end := t0.Add(2 * time.Minute)
	for _, c := range before {
		end = maxT(end, c.at.Add(10*time.Millisecond))
	}
	from := end.Add(-back)
	after := map[string]time.Time{}
	for _, c := range sim(e, apps, from, 45*time.Second, 10*time.Millisecond, ok) {
		if _, seen := after[c.j.App]; c.j.Kind == model.KStatus && !seen {
			after[c.j.App] = c.at.Add(end.Sub(from)) // in real time
		}
	}
	every := e.Interval("app-0")
	for _, a := range apps {
		if g := after[a].Sub(last[a]); after[a].IsZero() || g > every+time.Second {
			t.Errorf("%s read %s after its last read, across a step back of %s; its interval %s", a, g, back, every)
		}
	}
}

func maxT(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// TestStepBackKeepsTheAnswerTimes: an app answered more than the open
// app's interval ago, in real time, is read at once on opening it,
// though the clock was set back since its answer.
func TestStepBackKeepsTheAnswerTimes(t *testing.T) {
	apps := names(12)
	e := model.NewEngine(t0)
	calls := sim(e, apps, t0, 40*time.Second, 10*time.Millisecond, ok)
	end := t0.Add(40 * time.Second)
	for _, c := range calls {
		end = maxT(end, c.at.Add(10*time.Millisecond))
	}
	answered := map[string]time.Time{}
	for _, c := range calls {
		if c.j.Kind == model.KStatus {
			answered[c.j.App] = c.at.Add(10 * time.Millisecond)
		}
	}
	app := ""
	for _, a := range apps {
		if g := end.Sub(answered[a]); g >= model.FocusEvery+time.Second && g < back-time.Second {
			app = a
			break
		}
	}
	if app == "" {
		t.Fatalf("no app answered between %s and %s before the step: %v", model.FocusEvery, back, answered)
	}
	from := end.Add(-back)
	e.Touch(from)
	e.Focus(app, from)
	if !takeApp(e, from, app) {
		t.Errorf("%s, answered %s before in real time, not read at once on opening it, the clock set back %s since", app, end.Sub(answered[app]), back)
	}
}

// TestStepBackKeepsWhoamisTimes: who am I opened a minute and more
// after whoami's answer, in real time, reads it at once; and a 403 a
// minute and more after the last that asked whoami asks it again; the
// clock set back in between.
func TestStepBackKeepsWhoamisTimes(t *testing.T) {
	e := fresh(t)
	now := t0.Add(50 * time.Second)
	e.Touch(now)
	e.Touch(now.Add(-back))
	open := t0.Add(model.MeEvery + 10*time.Second - back)
	e.MeOpen(true, open)
	if !at(e, open, func(model.Job) error { return nil }) {
		t.Errorf("who am I opened %s after whoami's answer, the clock set back %s: whoami not read", model.MeEvery+10*time.Second, back)
	}

	e = fresh(t)
	first := t0.Add(2 * time.Minute)
	e.Again(first, model.Job{Kind: model.KStatus, App: "app-0"})
	refusedMe := func(j model.Job) error {
		if j.Kind == model.KWhoAmI {
			return fail(model.FailThis) // its answer stays old
		}
		return forbidApp(j)
	}
	if !at(e, first, refusedMe) {
		t.Fatal("the first 403 did not ask whoami")
	}
	e.Touch(first.Add(-back))
	second := first.Add(model.MeEvery + 10*time.Second - back)
	e.Again(second, model.Job{Kind: model.KStatus, App: "app-0"})
	if !at(e, second, refusedMe) {
		t.Errorf("a 403 %s after the last that asked whoami, the clock set back %s: whoami not asked", model.MeEvery+10*time.Second, back)
	}
}
