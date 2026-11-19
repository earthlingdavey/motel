package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/termtext"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// hostileBoxAlerts is what a box's Alertmanager holds, every free-text
// value carrying a hostile string: an alert's name, app, instance and
// summary (a refused deploy's summary holds a deployer's words), and a
// silence's matchers, maker, comment and id, each printed labelled and
// capped. A state the client does not know is quoted in its own words.
func hostileBoxAlerts() wire.BoxAlerts {
	at := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	until := at.Add(2 * time.Hour)
	b := wire.BoxAlerts{Monitor: wire.MonitorOn, Alerts: []wire.BoxAlert{
		{App: "shop-2", Alert: wire.Alert{Name: "deploy_refused", Severity: "warning", StartsAt: at.Add(-6 * time.Minute), State: "active",
			Summary: "deploy of v41 refused:\ncritical  firing since 1999-01-01 00:00 UTC " + strings.Repeat("s", 300)}},
		{Instance: "/var/lib/liveswap", Alert: wire.Alert{Name: "disk_low", Severity: "warning", Summary: "the filesystem holding /var/lib/liveswap is 84% full",
			StartsAt: at.Add(-3 * time.Hour), State: "suppressed", SilencedUntil: &until}},
		{App: "blog", Alert: wire.Alert{Name: "watchdog_throttled", Severity: "critical", Summary: "blog: the watchdog spent its restart budget",
			StartsAt: at.Add(-4 * time.Hour), State: "suppressed", Inhibited: true}},
		{Alert: wire.Alert{Name: "odd", Severity: "page", Summary: "posted by hand", StartsAt: at.Add(-5 * time.Hour), State: "weird " + hostile[3]}},
	}, Silences: []wire.Silence{
		{ID: "22e1715e-5a1b-4c7e-9f3d-0a8b6c4d2e1f", State: wire.SilenceActive, StartsAt: at.Add(-time.Hour), EndsAt: until,
			Matchers:  []wire.Matcher{{Name: "alertname", Op: wire.MatchEqual, Value: "disk_low"}, {Name: "instance " + hostile[1], Op: wire.MatchRegexp, Value: "/var/.*" + hostile[2]}},
			CreatedBy: "moteld monitor: root", Comment: "resizing the volume"},
	}}
	for i, h := range hostile {
		b.Alerts = append(b.Alerts, wire.BoxAlert{App: "x" + h, Instance: h, Alert: wire.Alert{Name: h, Severity: "info", Summary: h, StartsAt: at.Add(-time.Duration(6+i) * time.Hour), State: "active"}})
		b.Silences = append(b.Silences, wire.Silence{ID: h, State: wire.SilencePending, StartsAt: at.Add(time.Hour), EndsAt: until,
			Matchers: []wire.Matcher{{Name: h, Op: wire.MatchNotEqual, Value: h}}, CreatedBy: h, Comment: h})
	}
	return b
}

// monitor alerts: exit 0 whatever fires, with --json the box's body;
// the human output holds nothing a box could use on a terminal, has
// every value of the box's on a labelled line of its own, and is what
// the golden says; a box with no monitor block says so, exit 0; one
// whose Alertmanager did not answer it, or that says a monitor word the
// client does not know, could not tell (3), and a monitor that is not a
// word is not moteld's answer; a matcher's operator or a silence's
// state the client does not know is said as not known; at the cap the
// first line says the box holds more; and it is asked with a person's
// key alone.
func TestMonitorBoxAlerts(t *testing.T) {
	fb := newFakeBox(t)
	body := func() string { return string(withNewline(termtext.JSON(fb.bodies["/_motel/alerts"]))) }
	r := runKey(t, fb, nil, "monitor", "alerts", "--json")
	if r.code != 0 || r.stdout != body() {
		t.Fatalf("--json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
		t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
	}
	r = runKey(t, fb, nil, "monitor", "alerts")
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
		t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
	}
	for _, l := range strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n") {
		// A line is the client's (a head, at the margin, or a heading),
		// or a box's value after the client's label, four spaces in.
		if strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "    alert     ") && !strings.HasPrefix(l, "    severity  ") && !strings.HasPrefix(l, "    app       ") &&
			!strings.HasPrefix(l, "    instance  ") && !strings.HasPrefix(l, "    summary   ") && !strings.HasPrefix(l, "    matches   ") &&
			!strings.HasPrefix(l, "    by        ") && !strings.HasPrefix(l, "    comment   ") && !strings.HasPrefix(l, "    id        ") &&
			!strings.HasPrefix(l, "  active  ") && !strings.HasPrefix(l, "  pending  ") {
			t.Errorf("a line that is neither the client's nor a labelled value: %q", l)
		}
	}
	if strings.Contains(r.stdout, "\ncritical  firing since 1999") || !strings.Contains(r.stdout, "\n    severity  page\n") || !strings.Contains(r.stdout, "other  in a state not known, \"weird title \\x1b]0;pwned\\x07\"; firing since 2026-11-19 09:00 UTC") {
		t.Errorf("the output:\n%s", r.stdout)
	}
	golden(t, "hostile-box-alerts", r.stdout)

	fb.boxAlerts = wire.BoxAlerts{Monitor: wire.MonitorOff, Alerts: []wire.BoxAlert{}, Silences: []wire.Silence{}}
	if r := runKey(t, fb, nil, "monitor", "alerts"); r.code != 0 || r.stdout != "127.0.0.1 raises no alerts: its monitor block sends nothing to anyone, or it has none\n" {
		t.Errorf("off: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	for _, c := range []struct{ monitor, says string }{
		{wire.MonitorUnanswered, "127.0.0.1 could not tell what fires: its Alertmanager did not answer it in time"},
		{"asleep", `127.0.0.1 could not be told: it says its monitor is "asleep", a word this client does not know`},
	} {
		fb.boxAlerts.Monitor = c.monitor
		r := runKey(t, fb, nil, "monitor", "alerts", "--json")
		var e struct{ Error string }
		if r.code != 3 || json.Unmarshal([]byte(r.stdout), &e) != nil || e.Error != c.says {
			t.Errorf("%s, --json: exit %d, stdout %q, stderr %q", c.monitor, r.code, r.stdout, r.stderr)
		}
		if r := runKey(t, fb, nil, "monitor", "alerts"); r.code != 3 || r.stdout != "" || unsafeOutput.FindString(r.stderr) != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.monitor, r.code, r.stdout, r.stderr)
		}
	}

	// A monitor that is not a word is not moteld's answer, and none of it
	// is printed.
	fb.boxAlerts.Monitor = "sleeping " + hostile[0]
	if r := runKey(t, fb, nil, "monitor", "alerts"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: its monitor is not a word") {
		t.Errorf("a monitor outside its grammar: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// A silence's state and a matcher's operator the client does not know
	// are said as not known, never refused: a later box may add one.
	fb.boxAlerts = hostileBoxAlerts()
	fb.boxAlerts.Silences[0].Matchers[0].Op = "=="
	fb.boxAlerts.Silences[0].State = "dormant"
	if r := runKey(t, fb, nil, "monitor", "alerts"); r.code != 0 || !strings.Contains(r.stdout, "\n  in a state not known (\"dormant\"): from ") || !strings.Contains(r.stdout, `matches   alertname ? "disk_low"`) {
		t.Errorf("an unknown state and operator: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	// At the cap the head says the box holds more than it sends.
	fb.boxAlerts = hostileBoxAlerts()
	fb.boxAlerts.MoreSilences = true
	if r := runKey(t, fb, nil, "monitor", "alerts"); r.code != 0 || !strings.HasPrefix(r.stdout, "127.0.0.1's Alertmanager holds more than it sends: 14 alerts and 11 silences, the newest\n") {
		t.Errorf("at the cap: exit %d, stdout %q", r.code, r.stdout)
	}

	asked := len(fb.auth)
	if r := runKey(t, fb, map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_KEY": ""}, "monitor", "alerts"); r.code != 2 || len(fb.auth) != asked {
		t.Errorf("with MOTEL_TOKEN: exit %d, %d requests, stderr %q", r.code, len(fb.auth)-asked, r.stderr)
	}
	if r := runKey(t, fb, nil, "monitor"); r.code != 2 || !strings.Contains(r.stderr, "monitor needs a command: motel monitor alerts") {
		t.Errorf("monitor alone: exit %d, stderr %q", r.code, r.stderr)
	}
}
