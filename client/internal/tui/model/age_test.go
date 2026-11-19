package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestRowStaleByAge: a row whose reads did not come says stale once its
// answer is older than three of its intervals and one of grace, though
// no read of it failed: nothing is taken here after the first pass, as
// when every read hangs.
func TestRowStaleByAge(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	r := row(s, now, "docs")
	if r.Stale || r.Cells[model.ColAge].String() == "" {
		t.Fatalf("docs just read: stale %v, age %q", r.Stale, r.Cells[model.ColAge])
	}
	every := s.Engine.Interval("docs")
	var at time.Time // its last answer: the fake answers at once
	for _, c := range f.Calls {
		if c.Job == (model.Job{Kind: model.KStatus, App: "docs"}) {
			at = c.At
		}
	}
	if at.IsZero() {
		t.Fatal("docs was not read")
	}
	edge := at.Add((model.StaleAfter + 1) * every)
	if r := row(s, edge.Add(-time.Second), "docs"); r.Stale {
		t.Errorf("docs stale %s after its answer, its interval %s", edge.Add(-time.Second).Sub(at), every)
	}
	r = row(s, edge.Add(time.Second), "docs")
	if !r.Stale || !strings.Contains(r.Cells[model.ColState].String(), "stale") || r.Cells[model.ColAge].String() != "stale" {
		t.Errorf("docs %s after its answer, three reads of %s not come: stale %v, state %q, age %q",
			edge.Add(time.Second).Sub(at), every, r.Stale, r.Cells[model.ColState], r.Cells[model.ColAge])
	}
}

// TestTitleSaysPaused: ten minutes with no key pressed, the title bar
// says the reads are paused and that a key resumes them; a key, and it
// does not.
func TestTitleSaysPaused(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	s.Engine.Touch(now) // the last key
	if _, r := s.Title(now.Add(model.Idle - time.Second)); strings.HasPrefix(r.Text(), "PAUSED") {
		t.Errorf("%s with no key: the title says %q", model.Idle-time.Second, r.Text())
	}
	idle := now.Add(model.Idle)
	if _, r := s.Title(idle); !strings.HasPrefix(r.Text(), "PAUSED") || !strings.Contains(r.Text(), "any key resumes") {
		t.Errorf("%s with no key: the title says %q", model.Idle, r.Text())
	}
	s.Engine.Touch(idle)
	if _, r := s.Title(idle); strings.HasPrefix(r.Text(), "PAUSED") {
		t.Errorf("after a key the title says %q", r.Text())
	}
}
