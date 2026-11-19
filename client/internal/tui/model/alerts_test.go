package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestAlertsReadWhileTheAppIsOpen: an app's alerts are read only while
// its screen is open, at once when it opens, then every AlertsEvery;
// opening another app's screen forgets the first's; closing every
// app's stops them; ctrl-r asks them again no sooner than AgainAfter;
// and the box's limit is never passed.
func TestAlertsReadWhileTheAppIsOpen(t *testing.T) {
	e := model.NewEngine(t0)
	alerts := func(app string) model.Job { return model.Job{Kind: model.KAlerts, App: app} }
	count := func(calls []call, j model.Job) (n int, at []time.Time) {
		for _, c := range calls {
			if c.j == j {
				n++
				at = append(at, c.at)
			}
		}
		return n, at
	}
	calls := sim(e, names(3), t0, time.Minute, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j.Kind == model.KAlerts {
			t.Fatalf("alerts read with no app open: %v", c)
		}
	}
	from := t0.Add(time.Minute)
	e.Focus("app-1", from)
	e.AlertsOpen("app-1", from)
	calls = sim(e, names(3), from, time.Minute, 10*time.Millisecond, ok)
	boxBucket(t, calls)
	n, at := count(calls, alerts("app-1"))
	if n < 4 || n > 5 || at[0].Sub(from) > time.Second {
		t.Errorf("app-1's alerts read %d times in a minute, first at +%s: want at once, then every %s", n, at[0].Sub(from), model.AlertsEvery)
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < model.AlertsEvery || d > model.AlertsEvery+time.Second {
			t.Errorf("app-1's alerts read %s after the last", d)
		}
	}
	from = from.Add(time.Minute)
	e.Focus("app-2", from)
	e.AlertsOpen("app-2", from)
	calls = sim(e, names(3), from, 30*time.Second, 10*time.Millisecond, ok)
	if n, _ := count(calls, alerts("app-1")); n != 0 {
		t.Errorf("app-1's alerts read %d times once app-2 was opened", n)
	}
	if n, _ := count(calls, alerts("app-2")); n < 2 {
		t.Errorf("app-2's alerts read %d times in 30 s", n)
	}
	// ctrl-r: asked again, but not within AgainAfter of the last.
	from = from.Add(30 * time.Second)
	e.Again(from, alerts("app-2"))
	calls = sim(e, names(3), from, 2*time.Second, 10*time.Millisecond, ok)
	if n, _ := count(calls, alerts("app-2")); n > 1 {
		t.Errorf("ctrl-r read the alerts %d times in 2 s", n)
	}
	from = from.Add(2 * time.Second)
	e.Focus("", from)
	e.AlertsOpen("", from)
	calls = sim(e, names(3), from, time.Minute, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j.Kind == model.KAlerts {
			t.Fatalf("alerts read with no app open: %v", c)
		}
	}
}

// TestAlertLines: an app's screen says its alerts under its state, as
// the box said them: how many are held, each with what holds it and
// when it began, under it its summary; no monitor, an Alertmanager that
// did not answer, a monitor word the client does not know and a box
// older than the read each in the client's own words; a read that
// failed after an answer says so beside it. At most AlertsShown are
// listed, the rest counted.
func TestAlertLines(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	s.Engine.Focus("blog", now)
	s.Engine.AlertsOpen("blog", now)
	lines := func() []string {
		var out []string
		for _, l := range s.Overview("blog", now).Fields {
			if t := l.Text(); strings.HasPrefix(t, "alert") || strings.HasPrefix(t, "summary") || strings.HasPrefix(t, "            ") {
				out = append(out, t)
			}
		}
		return out
	}
	readAll := func() {
		t.Helper()
		for range 8 {
			step(t, s, f, &now)
		}
	}
	readAll()
	got := strings.Join(lines(), "\n")
	for _, want := range []string{
		"alerts      2 held · as of ",
		"alert       deploy_failed · warning · silenced until 15:",
		"summary     deploy of e9a0b3f2c118 failed at starting",
		"alert       errors_high · warning · since 25m ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the alerts lines:\n%s\nwant one holding %q", got, want)
		}
	}
	for _, c := range []struct{ body, want string }{
		{`{"monitor":"off","alerts":[]}`, "alerts      none: the box has no monitor, so nothing is raised"},
		{`{"monitor":"unanswered","alerts":[]}`, "alerts      not known: the box could not ask its Alertmanager"},
		{`{"monitor":"later","alerts":[]}`, "alerts      not known: the box says later of its monitor"},
		{`{"monitor":"on","alerts":[]}`, "alerts      none held"},
		{`{"monitor":"on","alerts":[` + strings.Repeat(`{"name":"a","severity":"info","summary":"","starts_at":"@-1m","state":"unprocessed"},`, 4) +
			`{"name":"a","severity":"info","summary":"","starts_at":"@-1m","state":"unprocessed"}]}`, "            and 2 more: motel monitor alerts --box deploy.example.com --app blog lists them"},
		{`{"monitor":"on","alerts":[{"name":"a","severity":"info","summary":"","starts_at":"@-1m","state":"unprocessed"}]}`, "alert       a · info · unprocessed · since 1m ago"},
	} {
		f.Body("alerts/blog.json", c.body)
		now = now.Add(model.AlertsEvery)
		readAll()
		if got := strings.Join(lines(), "\n"); !strings.Contains(got, c.want) {
			t.Errorf("%s:\n%s\nwant a line holding %q", c.body, got, c.want)
		}
	}
	f.Set("alerts:blog", fail(model.FailNoAnswer))
	now = now.Add(model.AlertsEvery)
	readAll()
	if got := lines(); len(got) == 0 || !strings.HasSuffix(got[0], " · the last read failed") {
		t.Errorf("a failed read beside an answer: %q", got)
	}
	f.Set("alerts:blog", &model.Failure{Kind: model.FailNotFound, Message: "deploy.example.com answered 404"})
	s.Engine.Again(now.Add(model.AgainAfter), model.Job{Kind: model.KAlerts, App: "blog"})
	now = now.Add(model.AlertsEvery)
	readAll()
	if got := lines(); len(got) != 1 || got[0] != "alerts      this box does not offer an app's alerts" {
		t.Errorf("a 404: %q", got)
	}
}

// TestAlertsReopened: an app's screen opened again within AlertsEvery
// of its alerts' answer reads them when that answer is AlertsEvery old,
// not at once; refused (a box older than the read answers 404) they
// are not asked again by opening the screen, only by ctrl-r.
func TestAlertsReopened(t *testing.T) {
	e := model.NewEngine(t0)
	j := model.Job{Kind: model.KAlerts, App: "app-1"}
	sim(e, names(3), t0, 10*time.Second, 10*time.Millisecond, ok)
	open := t0.Add(10 * time.Second)
	e.Focus("app-1", open)
	e.AlertsOpen("app-1", open)
	calls := sim(e, names(3), open, 2*time.Second, 10*time.Millisecond, ok)
	var answered time.Time
	for _, c := range calls {
		if c.j == j {
			answered = c.at
		}
	}
	if answered.IsZero() {
		t.Fatal("the alerts were not read on opening the screen")
	}
	at := open.Add(2 * time.Second)
	e.AlertsOpen("", at)
	e.AlertsOpen("app-1", at.Add(3*time.Second))
	calls = sim(e, names(3), at.Add(3*time.Second), 20*time.Second, 10*time.Millisecond, ok)
	for _, c := range calls {
		if c.j == j {
			if d := c.at.Sub(answered); d < model.AlertsEvery {
				t.Errorf("read again %s after its answer, on opening the screen again", d)
			}
			break
		}
	}
	at = at.Add(23 * time.Second)
	notFound := func(k model.Job, _ time.Time) error {
		if k == j {
			return fail(model.FailNotFound)
		}
		return nil
	}
	sim(e, names(3), at, 20*time.Second, 10*time.Millisecond, notFound)
	at = at.Add(20 * time.Second)
	e.AlertsOpen("", at)
	e.AlertsOpen("app-1", at)
	calls = sim(e, names(3), at, time.Minute, 10*time.Millisecond, notFound)
	for _, c := range calls {
		if c.j == j {
			t.Fatalf("a refused read asked again by opening the screen, at +%s", c.at.Sub(at))
		}
	}
	at = at.Add(time.Minute)
	e.Again(at, j)
	calls = sim(e, names(3), at, 2*time.Second, 10*time.Millisecond, notFound)
	n := 0
	for _, c := range calls {
		if c.j == j {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ctrl-r asked the refused read %d times", n)
	}
}

// TestOldBoxAlertsNotRefused: a box older than the alerts read answers
// it 404, which the app's screen says; the title bar does not call it a
// refusal.
func TestOldBoxAlertsNotRefused(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	f.Body("apps.json", `{"apps": [{"name": "blog"}]}`)
	f.Set("alerts:blog", &model.Failure{Kind: model.FailNotFound, Message: "deploy.example.com answered 404"})
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	s.Engine.Focus("blog", now)
	s.Engine.AlertsOpen("blog", now)
	for range 8 {
		if j := step(t, s, f, &now); j.Kind == model.KAlerts {
			break
		}
	}
	a, b := s.Title(now)
	if title := a.Text() + b.Text(); strings.Contains(title, "refused") {
		t.Errorf("the title bar says %q after the alerts' 404", title)
	}
}
