package model_test

import (
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/tui/demo"
	"github.com/smallhoursorg/motel/client/internal/tui/model"
)

// TestBoxAlertsSays: the alerts screen says what the box's Alertmanager
// holds in the client's words, every value of the box's on a row of its
// own after the client's label and cut to the window, so a summary or
// a comment that writes a row of the screen's own shape, or a terminal
// escape, never begins a row; an alert the deploy mute holds is there,
// held, and a silenced one says until when; a state the client does not
// know is said as not known, the box's word quoted; and the box
// screen's line gives the age and the way to the screen first.
func TestBoxAlertsSays(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxAlertsOpen(true, *now)
	forged := strings.Repeat("x", 70) + "✖ critical  firing since 12:00 UTC (1h ago)"
	f.Body("box-alerts.json", `{"monitor":"on","alerts":[`+
		`{"name":"deploy_refused","severity":"warning","summary":"deploy of v2 refused: `+forged+` \u001b]0;pwned\u0007","starts_at":"@-6m","state":"active","app":"shop"},`+
		`{"name":"watchdog_throttled","severity":"critical","summary":"images: throttled","starts_at":"@-2h","state":"suppressed","inhibited":true,"app":"images"},`+
		`{"name":"disk_low","severity":"warning","summary":"84% full","starts_at":"@-3h","state":"suppressed","silenced_until":"@2h","instance":"/var/lib/liveswap"},`+
		`{"name":"odd","severity":"page","summary":"by hand","starts_at":"@-4h","state":"weird"}],`+
		`"silences":[{"id":"22e1715e-5a1b-4c7e-9f3d-0a8b6c4d2e1f","state":"active","matchers":[{"name":"alertname","op":"=","value":"disk_low"}],"starts_at":"@-1h","ends_at":"@2h","created_by":"moteld monitor: root","comment":"`+forged+`"}]}`)
	for range 3 {
		step(t, s, f, now)
	}
	if s.BoxAlertsSt == nil {
		t.Fatalf("not read: %v", s.BoxAlertsErr)
	}
	v := s.BoxAlertsViewFit(*now, 60, runes)
	text := joined(v)
	for _, l := range v {
		row := l.Text()
		if l.Width(runes) > 60 {
			t.Errorf("a row wider than 60: %q", row)
		}
		// The box's words are on rows of their own, after the client's
		// label: the forged row and the escape never begin one.
		if strings.Contains(row, "xxx") && !strings.HasPrefix(row, "    summary   ") && !strings.HasPrefix(row, "    comment   ") ||
			strings.HasPrefix(strings.TrimLeft(row, " "), "✖ critical  firing") || strings.ContainsAny(row, "\x1b\x07") {
			t.Errorf("a row the box's words begin, or with an escape: %q", row)
		}
	}
	flat := strings.Join(strings.Fields(text), " ")
	if !strings.Contains(flat, "monitor on · 4 alerts, 1 silence, as the box's Alertmanager holds them") {
		t.Errorf("the monitor's line:\n%s", text)
	}
	for _, want := range []string{
		"▲ warning  firing since ",
		"  critical held while a deploy of its app runs",
		"  warning  silenced until ",
		"? other    state not known · firing since ",
		"    severity  page\n",
		"    instance  /var/lib/liveswap\n",
		"    matches   alertname = \"disk_low\"\n",
		"    by        moteld monitor: root\n",
		"  active   until ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the screen does not say %q:\n%s", want, text)
		}
	}
	box := s.BoxAlertsLine(*now).Cut(60, runes).Text()
	if !strings.HasPrefix(box, "alerts      ") || !strings.Contains(box, " ago · :alerts · 2 firing, 2 held quiet · ") {
		t.Errorf("the box screen's line at 60 columns: %q", box)
	}

	// No monitor block, and an Alertmanager that did not answer: said,
	// and nothing listed.
	for body, want := range map[string]string{
		`{"monitor":"off","alerts":[],"silences":[]}`:        "monitor     off: this box raises no alerts (its monitor block sends nothing to anyone, or it has none)",
		`{"monitor":"unanswered","alerts":[],"silences":[]}`: "monitor     not known: the box's Alertmanager did not answer it in time",
		`{"monitor":"asleep","alerts":[],"silences":[]}`:     `monitor     not known: the box said "asleep"`,
	} {
		*now = now.Add(model.BoxEvery)
		f.Body("box-alerts.json", body)
		for range 3 {
			step(t, s, f, now)
		}
		if got := joined(s.BoxAlertsView(*now)); got != want+"\n" {
			t.Errorf("%s: the screen says\n%s", body, got)
		}
		if line := s.BoxAlertsLine(*now).Text(); !strings.HasSuffix(line, strings.TrimPrefix(want, "monitor     ")) {
			t.Errorf("%s: the box screen's line %q", body, line)
		}
	}

	// A read that gets no answer keeps the answer before it, and the
	// crumb says the last read failed; a 403 drops it and says how to ask
	// again.
	*now = now.Add(model.BoxEvery)
	f.Set("box-alerts", &model.Failure{Kind: model.FailNoAnswer, Message: "no answer"})
	for range 3 {
		step(t, s, f, now)
	}
	if s.BoxAlertsSt == nil || !strings.HasSuffix(s.BoxAlertsAsOf(*now).Text(), " · the last read failed") {
		t.Errorf("after a read that got no answer: held %v, the crumb says %q", s.BoxAlertsSt != nil, s.BoxAlertsAsOf(*now).Text())
	}
	*now = now.Add(model.BoxEvery)
	f.Set("box-alerts", &model.Failure{Kind: model.FailForbidden, Message: "not allowed"})
	for range 3 {
		step(t, s, f, now)
	}
	if s.BoxAlertsSt != nil || !strings.Contains(joined(s.BoxAlertsView(*now)), "ctrl-r") {
		t.Errorf("after a 403: held %v, the screen says\n%s", s.BoxAlertsSt != nil, joined(s.BoxAlertsView(*now)))
	}
}

// TestOldBoxBoxWideReadsNotRefused: a box older than the box-wide
// reads (the alerts, the backups) answers them 404, which their screens
// and the box screen's lines say; the title bar does not call it a
// refusal, open as the box screen keeps them.
func TestOldBoxBoxWideReadsNotRefused(t *testing.T) {
	now := t0
	f := &demo.Fake{Now: func() time.Time { return now }}
	nf := &model.Failure{Kind: model.FailNotFound, Message: "deploy.example.com answered 404"}
	f.Set("box-alerts", nf)
	f.Set("backups", nf)
	s := &model.Session{Host: demo.Host, Now: f.Now, Engine: model.NewEngine(t0)}
	s.Engine.BoxAlertsOpen(true, now)
	s.Engine.BackupsOpen(true, now)
	read := map[model.Kind]bool{}
	for range 12 {
		read[step(t, s, f, &now).Kind] = true
	}
	if !read[model.KBoxAlerts] || !read[model.KBackups] {
		t.Fatalf("read %v", read)
	}
	a, b := s.Title(now)
	if title := a.Text() + b.Text(); strings.Contains(title, "refused") {
		t.Errorf("the title bar says %q after the box-wide reads' 404", title)
	}
	if v := joined(s.BoxAlertsView(now)); !strings.Contains(v, "its moteld is older than this read") {
		t.Errorf(":alerts after a 404 says\n%s", v)
	}
}

// TestBoxAlertsWithheldRows: a row the box's filter withheld (no name,
// no severity; a silence with no maker) shows its head and what it has,
// and no labelled row with nothing after it.
func TestBoxAlertsWithheldRows(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxAlertsOpen(true, *now)
	f.Body("box-alerts.json", `{"monitor":"on","alerts":[{"name":"","severity":"","summary":"response withheld: the app's env_file could not be read","starts_at":"@-1m","state":"active","app":"demo"}],`+
		`"silences":[{"id":"","state":"active","matchers":[],"starts_at":"@-1m","ends_at":"@1h","created_by":"","comment":"response withheld"}]}`)
	for range 3 {
		step(t, s, f, now)
	}
	for _, l := range s.BoxAlertsView(*now) {
		if row := l.Text(); strings.HasPrefix(row, "    ") && strings.TrimSpace(row[14:]) == "" {
			t.Errorf("a labelled row with nothing after it: %q", row)
		}
	}
}

// TestBoxAlertsLineCounts: the box screen's line counts as the screen
// marks: an info alert that fires (a deploy that succeeded) is counted
// apart and does not dress the line as needing a look.
func TestBoxAlertsLineCounts(t *testing.T) {
	s, f, now := session(t)
	s.Engine.BoxAlertsOpen(true, *now)
	f.Body("box-alerts.json", `{"monitor":"on","alerts":[{"name":"deploy_succeeded","severity":"info","summary":"deployed v2","starts_at":"@-1m","state":"active","app":"demo"}],"silences":[]}`)
	for range 3 {
		step(t, s, f, now)
	}
	l := s.BoxAlertsLine(*now)
	if !strings.Contains(l.Text(), ":alerts · 0 firing, 0 held quiet, 1 info · 0 silences") {
		t.Errorf("the line: %q", l.Text())
	}
	for _, sg := range l {
		if sg.C == model.Warn {
			t.Errorf("the line is dressed as needing a look: %q", l.Text())
		}
	}
}

// TestMeSaysWhoReadsTheBoxAlerts: who am I says the box's alerts are
// not this person's only when they are not a viewer of the box, and
// then where an app's alerts are.
func TestMeSaysWhoReadsTheBoxAlerts(t *testing.T) {
	for _, c := range []struct {
		scope string
		says  bool
	}{{"box", false}, {"app:blog", true}} {
		s, f, now := session(t)
		f.Body("whoami.json", `{"name":"alice@example.com","key":"`+demo.Key.Fingerprint+`","roles":[{"role":"viewer","scope":"`+c.scope+`"}],"box":"`+demo.Host+`","version":"v0.8.0","protocol":1,"token_expires_at":"@9m"}`)
		for range 3 {
			step(t, s, f, now)
		}
		if s.Me == nil {
			t.Fatalf("%s: whoami not read: %v", c.scope, s.MeErr)
		}
		v := strings.Join(strings.Fields(joined(s.MeView(*now))), " ")
		if strings.Contains(v, "Nor the box's own alerts and silences") != c.says || !strings.Contains(v, "Read logs: this client does not offer them yet.") {
			t.Errorf("%s: who am I says\n%s", c.scope, v)
		}
	}
}
