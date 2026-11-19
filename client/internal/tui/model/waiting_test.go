package model_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// settled is a session whose first reads (whoami, the list, every app)
// have been answered at t0, and whose clock is then at the next read
// due, which is taken and returned, not yet ended.
func settled(t *testing.T) (*model.Session, *demo.Fake, *time.Time, model.Job) {
	t.Helper()
	s, f, now := session(t)
	e := s.Engine
	for range 100 {
		j, ok, _ := e.Take(*now)
		if !ok {
			break
		}
		apply, err := s.Fetch(context.Background(), f, j)
		apply()
		e.Done(j, *now, err)
	}
	return s, f, now, next(t, s, now)
}

// next moves the clock to the next read due and takes it.
func next(t *testing.T, s *model.Session, now *time.Time) model.Job {
	t.Helper()
	for range 2000 {
		s.Engine.Touch(*now)
		j, ok, wait := s.Engine.Take(*now)
		if ok {
			return j
		}
		*now = now.Add(max(wait, 100*time.Millisecond))
	}
	t.Fatal("no read due in 200 s")
	return model.Job{}
}

// end ends the read j at now with err, its answer the fake's otherwise.
func end(s *model.Session, f *demo.Fake, j model.Job, now time.Time, err error) {
	apply, ferr := s.Fetch(context.Background(), f, j)
	if err == nil {
		apply()
		err = ferr
	}
	s.Engine.Done(j, now, err)
}

func word(s *model.Session, now time.Time) string {
	_, r := s.Title(now)
	return r.Text()
}

// TestWaitingSaid: a read in flight for WaitingAfter or longer makes
// the title bar say how long the box has been waited for, beside the
// age of the last answer; it leaves when the answer comes. An answer
// that takes 1, 2 or 4 s (a record's log tail, a busy box) shows
// nothing new.
func TestWaitingSaid(t *testing.T) {
	for _, lat := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		s, f, now, j := settled(t)
		asked := *now
		for d := time.Duration(0); d <= lat; d += 500 * time.Millisecond {
			if w := word(s, asked.Add(d)); !strings.HasPrefix(w, "live · ") {
				t.Errorf("an answer taking %v, at %v: %q", lat, d, w)
			}
		}
		*now = asked.Add(lat)
		end(s, f, j, *now, nil)
		if w := word(s, *now); w != "live · 0s ago" {
			t.Errorf("answered after %v: %q", lat, w)
		}
	}
	s, f, now, j := settled(t)
	asked, last := *now, t0 // the last answer came at t0
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{4900 * time.Millisecond, "live · " + model.Ago(asked.Add(4900*time.Millisecond).Sub(last)) + " ago"},
		{5 * time.Second, "waiting 5s for an answer · " + model.Ago(asked.Add(5*time.Second).Sub(last)) + " ago"},
		{29 * time.Second, "waiting 29s for an answer · " + model.Ago(asked.Add(29*time.Second).Sub(last)) + " ago"},
	} {
		if w := word(s, asked.Add(c.d)); w != c.want {
			t.Errorf("%v into the read: %q, want %q", c.d, w, c.want)
		}
	}
	*now = asked.Add(29 * time.Second)
	end(s, f, j, *now, nil)
	if w := word(s, *now); w != "live · 0s ago" {
		t.Errorf("once the answer comes: %q", w)
	}
}

// TestWaitingNot429: a 429's wait is not a read waited for: the title
// bar says the box's wait, and after it the reads go on as before.
func TestWaitingNot429(t *testing.T) {
	s, f, now, j := settled(t)
	*now = now.Add(6 * time.Second)
	if w := word(s, *now); !strings.HasPrefix(w, "waiting 6s") {
		t.Fatalf("6 s into the read: %q", w)
	}
	end(s, f, j, *now, &model.Failure{Kind: model.FailSlowed, RetryAfter: 20 * time.Second})
	for d := time.Duration(0); d < 20*time.Second; d += time.Second {
		at := now.Add(d)
		if h := s.Engine.Health(at); h.Waiting != 0 {
			t.Errorf("%v into the 429's wait: waiting %v", d, h.Waiting)
		}
		if w := word(s, at); !strings.HasPrefix(w, "slowed by the box (429)") {
			t.Errorf("%v into the 429's wait: %q", d, w)
		}
	}
}

// TestWaitingNotCancelled: a record left while its read is waited for
// is not waited for from then, before the read has even ended.
func TestWaitingNotCancelled(t *testing.T) {
	s, f, now, j := settled(t)
	end(s, f, j, *now, nil)
	rec := model.Job{Kind: model.KDeploy, App: "blog", Version: "7c1e55d0a913"}
	s.Engine.Once(rec, *now)
	if j := next(t, s, now); j != rec {
		t.Fatalf("the read taken is %v, not the record's", j)
	}
	s.Engine.Started(func() {})
	*now = now.Add(6 * time.Second)
	if w := word(s, *now); !strings.HasPrefix(w, "waiting 6s") {
		t.Fatalf("6 s into the record's read: %q", w)
	}
	s.Engine.Drop(rec) // esc
	if w := word(s, *now); !strings.HasPrefix(w, "live · ") {
		t.Errorf("the record left, its read not yet ended: %q", w)
	}
	*now = now.Add(100 * time.Millisecond)
	end(s, f, rec, *now, context.Canceled)
	if w := word(s, *now); !strings.HasPrefix(w, "live · ") {
		t.Errorf("the record's read cancelled: %q", w)
	}
}

// TestWaitingStops: a read waited for that ends in a stop says the
// stop; while stopped nothing is waited for. PAUSED is said over a read
// still in flight when the person has gone.
func TestWaitingStops(t *testing.T) {
	s, f, now, j := settled(t)
	*now = now.Add(8 * time.Second)
	if w := word(s, *now); !strings.HasPrefix(w, "waiting 8s") {
		t.Fatalf("8 s into the read: %q", w)
	}
	end(s, f, j, *now, &model.Failure{Kind: model.FailSignedOut, Message: "no"})
	for d := time.Duration(0); d < time.Minute; d += 10 * time.Second {
		if w := word(s, now.Add(d)); !strings.HasPrefix(w, "SIGNED OUT") {
			t.Errorf("%v after the 401: %q", d, w)
		}
		if h := s.Engine.Health(now.Add(d)); h.Waiting != 0 {
			t.Errorf("%v after the 401: waiting %v", d, h.Waiting)
		}
	}

	s, _, now, _ = settled(t)
	if w := word(s, now.Add(model.Idle)); !strings.HasPrefix(w, "PAUSED") {
		t.Errorf("idle 10 minutes, a read in flight: %q", w)
	}
}

// TestWaitingNotAfterQuickFailure: a read that got no answer at once (a
// 5xx on one route) is not counted into the wait for the read made
// next: only one that was itself waited for is.
func TestWaitingNotAfterQuickFailure(t *testing.T) {
	s, f, now, j := settled(t)
	*now = now.Add(time.Second)
	end(s, f, j, *now, fail(model.FailNoAnswer))
	k := next(t, s, now) // brought forward, taken at once
	if k == j {
		t.Fatalf("the read made next is %v again", k)
	}
	if w := word(s, now.Add(4500*time.Millisecond)); !strings.HasPrefix(w, "live · ") {
		t.Errorf("4.5 s into the read after a quick failure: %q", w)
	}
}

// TestWaitingBetweenTwo: a read waited for that gets no answer goes on
// being waited for after it ends, before the next read is taken (the
// frame drawn after its failure comes first), and through that read.
func TestWaitingBetweenTwo(t *testing.T) {
	s, f, now, j := settled(t)
	asked := *now
	*now = now.Add(30 * time.Second)
	end(s, f, j, *now, fail(model.FailNoAnswer))
	if w := word(s, *now); !strings.HasPrefix(w, "waiting 30s for an answer") {
		t.Errorf("the read's failure applied, the next not yet taken: %q", w)
	}
	k := next(t, s, now)
	if k == j || !now.Equal(asked.Add(30*time.Second)) {
		t.Fatalf("the read made next is %v at %v", k, now.Sub(asked))
	}
	*now = now.Add(10 * time.Second)
	if w := word(s, *now); !strings.HasPrefix(w, "waiting 40s for an answer") {
		t.Errorf("10 s into the read brought forward: %q", w)
	}
	end(s, f, k, *now, nil)
	if w := word(s, *now); !strings.HasPrefix(w, "live · 0s ago") {
		t.Errorf("the read brought forward answered: %q", w)
	}
}
