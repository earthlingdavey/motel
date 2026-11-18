package model_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// driver runs a session's engine as Run and the TUI's tick do: Take; a
// read when it gives one (Fetch, Done, then the answer applied); else a
// sleep on the wait it gives, ended by a poke (Woken) or the wait's end
// and nothing else; and a Tick every second. A timer counts real time,
// the wall clock may step: the two are kept apart, and a step moves
// the wall clock alone.
type driver struct {
	s *model.Session
	f *demo.Fake
	// real is the time since t0 as a timer counts it; off is the wall
	// clock less it; tick is the next tick, in real time.
	real, off, tick time.Duration
	// during is called while a read is in flight.
	during func(model.Job)
}

func newDriver() *driver {
	d := &driver{tick: time.Second}
	d.f = &demo.Fake{Now: d.wall}
	d.s = &model.Session{Host: demo.Host, Now: d.wall, Engine: model.NewEngine(t0)}
	return d
}

func (d *driver) wall() time.Time { return t0.Add(d.real + d.off) }

type drove struct {
	j    model.Job
	real time.Duration
}

// run drives for dur of real time, and returns each read made, with the
// real time it was made at.
func (d *driver) run(dur time.Duration) []drove {
	e := d.s.Engine
	end := d.real + dur
	var out []drove
	for d.real < end {
		j, ok, wait := e.Take(d.wall())
		if ok {
			if d.during != nil {
				d.during(j)
			}
			apply, err := d.s.Fetch(context.Background(), d.f, j)
			e.Done(j, d.wall(), err)
			apply()
			out = append(out, drove{j, d.real})
			continue
		}
		select {
		case <-e.Woken(): // a poke not yet taken wakes Run at once
			continue
		default:
		}
		deadline := end
		if wait > 0 {
			deadline = min(end, d.real+wait)
		}
		for {
			if deadline < d.tick {
				d.real = deadline
				break
			}
			d.real = d.tick
			d.tick += time.Second
			e.Tick(d.wall())
			d.s.Title(d.wall()) // the frame the tick draws
			select {
			case <-e.Woken():
			default:
				continue
			}
			break
		}
	}
	return out
}

// TestClockStepsBack: the wall clock set back while a read of the open
// app is in flight, by any amount, and Run, which sleeps on the wait
// Take gives and wakes only on a poke or the wait's end, reads the open
// app again within its interval and a tick, and every interval after.
func TestClockStepsBack(t *testing.T) {
	for _, back := range []time.Duration{2 * time.Second, 20 * time.Second, time.Hour} {
		t.Run(back.String(), func(t *testing.T) {
			d := newDriver()
			blog := model.Job{Kind: model.KStatus, App: "blog"}
			d.s.Engine.Focus("blog", d.wall())
			d.run(20 * time.Second)
			stepped := time.Duration(-1)
			d.during = func(j model.Job) {
				if j == blog && stepped < 0 {
					d.off -= back
					stepped = d.real
				}
			}
			reads := d.run(40 * time.Second)
			if stepped < 0 {
				t.Fatal("the open app was not read")
			}
			last := stepped
			n := 0
			for _, r := range reads {
				if r.j != blog || r.real <= stepped {
					continue
				}
				if g := r.real - last; g > model.FocusEvery+time.Second {
					t.Errorf("the open app read %s after the last, at +%s since the step", g, r.real-stepped)
				}
				last = r.real
				n++
			}
			if g := d.real - last; g > model.FocusEvery+time.Second {
				t.Errorf("the open app not read in the last %s (read %d times since the step)", g, n)
			}
		})
	}
}

// TestClockStepsBackAges: after the wall clock is set back, by a little
// or an hour, no age on screen is less than it was: the record's, the
// open app's, who am I's, a row held by a 403.
func TestClockStepsBackAges(t *testing.T) {
	for _, back := range []time.Duration{20 * time.Second, time.Hour} {
		t.Run(back.String(), func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			e := s.Engine
			f.Set("shop", fail(model.FailForbidden))
			for range 20 {
				step(t, s, f, &now)
			}
			rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "e9a0b3f2c118"}
			e.Focus("blog", now)
			e.Once(rec, now)
			for step(t, s, f, &now) != rec {
			}
			now = now.Add(10 * time.Minute)
			ages := func() []string {
				age := func(l model.Line) string {
					_, a, _ := strings.Cut(l.Text(), " · ")
					return a
				}
				return []string{
					age(s.RecordAsOf("blog", rec.Version, now)),
					age(s.Overview("blog", now).AsOf),
					age(s.MeAsOf(now)),
					row(s, now, "shop").Cells[model.ColAge].String(),
				}
			}
			before := ages()
			now = now.Add(-back)
			e.Tick(now)
			after := ages()
			for i := range before {
				if after[i] != before[i] {
					t.Errorf("set back %s: %q before, %q after", back, before[i], after[i])
				}
			}
		})
	}
}

// TestClockStepsBackOnce: a time the engine records after the step and
// before the next tick (a key's, a 429's wait) is on the new clock
// already, and is not moved again: the title does not say PAUSED, and
// the 429's wait is its length. A step back past WakeAfter is a wake:
// every row stale until read again.
func TestClockStepsBackOnce(t *testing.T) {
	for _, back := range []time.Duration{20 * time.Second, time.Hour} {
		t.Run(back.String(), func(t *testing.T) {
			now := t0
			f := &demo.Fake{Now: func() time.Time { return now }}
			s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
			e := s.Engine
			for range 20 {
				step(t, s, f, &now)
			}
			e.Tick(now)
			j, took, _ := e.Take(now)
			for !took {
				now = now.Add(100 * time.Millisecond)
				j, took, _ = e.Take(now)
			}
			now = now.Add(-back) // set back while the read is in flight
			e.Done(j, now, &model.Failure{Kind: model.FailSlowed, RetryAfter: 10 * time.Second})
			e.Touch(now)
			now = now.Add(500 * time.Millisecond)
			e.Tick(now)
			h := e.Health(now)
			if h.Paused {
				t.Errorf("a key pressed just after the step: paused")
			}
			if want := 9500 * time.Millisecond; h.SlowIn != want {
				t.Errorf("a 429's wait of 10 s has %s left after half a second", h.SlowIn)
			}
			if _, r := s.Title(now); strings.HasPrefix(r.Text(), "PAUSED") {
				t.Errorf("the title says %q", r.Text())
			}
			if r := row(s, now, "docs"); r.Stale != (back > model.WakeAfter) {
				t.Errorf("after a step back of %s a row's stale is %v", back, r.Stale)
			}
		})
	}
}
