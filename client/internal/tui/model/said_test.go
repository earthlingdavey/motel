package model_test

import (
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestOpenAppReadThoughUnlisted: the open app is read every FocusEvery
// though the box's list no longer names it, so its own screen comes to
// say what the box now says of it, rather than the last status it sent
// standing unread.
func TestOpenAppReadThoughUnlisted(t *testing.T) {
	e := model.NewEngine(t0)
	e.Focus("app-1", t0)
	sim(e, names(3), t0, 20*time.Second, 10*time.Millisecond, ok)
	now := t0.Add(20 * time.Second)
	rest := []string{"app-0", "app-2"}
	e.SetApps(rest, now)
	n := 0
	for _, c := range sim(e, rest, now, 30*time.Second, 10*time.Millisecond, ok) {
		if c.j == (model.Job{Kind: model.KStatus, App: "app-1"}) {
			n++
		}
	}
	if want := int(30*time.Second/model.FocusEvery) - 1; n < want {
		t.Errorf("the open app, no longer listed, read %d times in 30 s, want %d or more", n, want)
	}
}

// TestRefusedRowShowsItsAge: an app whose every read was refused shows
// how long ago the box last refused it: every value shows its age.
func TestRefusedRowShowsItsAge(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Set("blog", fail(model.FailForbidden))
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	for range 20 {
		step(t, s, f, &now)
	}
	var at time.Time
	for _, c := range f.Calls {
		if c.Job == (model.Job{Kind: model.KStatus, App: "blog"}) {
			at = c.At
		}
	}
	if at.IsZero() {
		t.Fatal("blog was not read")
	}
	if got, want := row(s, at.Add(90*time.Second), "blog").Cells[model.ColAge].String(), model.Ago(90*time.Second); got != want {
		t.Errorf("90 s after the box refused blog, its age says %q, want %q", got, want)
	}
}
