package model_test

import (
	"strings"
	"testing"

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
		`{"monitor":"off","alerts":[],"silences":[]}`:        "monitor     off: this box raises no alerts (its config has no monitor block)",
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
