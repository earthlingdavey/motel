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

// hostileAlerts is what a box says of blog's alerts, every free-text
// value carrying a hostile string: the summaries, printed labelled and
// capped. Names, severities and states are held to their grammars
// (client's TestReadAlerts) and are not hostile here.
func hostileAlerts() wire.Alerts {
	at := time.Date(2026, 11, 19, 14, 2, 0, 0, time.UTC)
	until := at.Add(2 * time.Hour)
	a := wire.Alerts{Monitor: wire.MonitorOn, Alerts: []wire.Alert{
		{Name: "deploy_failed", Severity: "warning", Summary: "deploy of v2 failed at starting", StartsAt: at, State: "suppressed", SilencedUntil: &until},
		{Name: "watchdog_throttled", Severity: "critical", Summary: strings.Repeat("s", 300) + strings.Repeat(" ", 60) + "alert    forged  critical", StartsAt: at.Add(-time.Hour), State: "suppressed", Inhibited: true},
	}}
	for _, h := range hostile {
		a.Alerts = append(a.Alerts, wire.Alert{Name: "errors_high", Severity: "info", Summary: "blog: " + h, StartsAt: at.Add(-2 * time.Hour), State: "active"})
	}
	return a
}

// monitor alerts --app: the box's answer is --json on a read; a box
// that could not ask its Alertmanager, or says a word for its monitor
// the client does not know, exits 3 with the client's own sentence; no
// monitor is said and exits 0; the human output holds nothing a box
// could use on a terminal, and is what the golden says; --app is
// needed, and the read is a person's key's alone.
func TestMonitorAlerts(t *testing.T) {
	fb := newFakeBox(t)
	fb.alerts = hostileAlerts()
	body := func() string {
		return string(withNewline(termtext.JSON(fb.bodies["/_motel/apps/blog/alerts"])))
	}
	r := runKey(t, fb, nil, "monitor", "alerts", "--app", "blog", "--json")
	if r.code != 0 || r.stdout != body() || r.stderr != "" {
		t.Fatalf("--json: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.HasPrefix(fb.auth[len(fb.auth)-1], "Motel m1.") {
		t.Errorf("Authorization %q, want a person's token", fb.auth[len(fb.auth)-1])
	}
	r = runKey(t, fb, nil, "monitor", "alerts", "--app", "blog")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
		t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
	}
	for _, l := range strings.Split(strings.TrimSuffix(r.stdout, "\n"), "\n")[1:] {
		if !strings.HasPrefix(l, "alert    ") && !strings.HasPrefix(l, "summary  ") {
			t.Errorf("a line that is not the client's: %q", l)
		}
	}
	golden(t, "hostile-alerts", r.stdout)
	// The app as its URL names the box too.
	if r := runKey(t, fb, map[string]string{"MOTEL_URL": ""}, "monitor", "alerts", "--app", fb.srv.URL+"/blog", "--json"); r.code != 0 || r.stdout != body() {
		t.Errorf("the app's URL: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	for _, c := range []struct {
		monitor, line string
		code          int
	}{
		{wire.MonitorOff, "127.0.0.1 has no monitor for blog: nothing is raised there\n", 0},
		{wire.MonitorUnanswered, "127.0.0.1 could not ask its Alertmanager about blog's alerts\n", 3},
		{"later", "127.0.0.1 answered about blog's alerts with a monitor word this client does not know: later\n", 3},
	} {
		fb.alerts = wire.Alerts{Monitor: c.monitor, Alerts: []wire.Alert{}}
		r := runKey(t, fb, nil, "monitor", "alerts", "--app", "blog")
		if r.code != c.code || r.stdout != c.line || (c.code == 0) != (r.stderr == "") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.monitor, r.code, r.stdout, r.stderr)
		}
		r = runKey(t, fb, nil, "monitor", "alerts", "--app", "blog", "--json")
		var e struct{ Error string }
		switch {
		case c.code == 0 && r.stdout != body():
			t.Errorf("%s, --json: stdout %q", c.monitor, r.stdout)
		case c.code != 0 && (r.code != c.code || json.Unmarshal([]byte(r.stdout), &e) != nil || !strings.HasPrefix(e.Error, "127.0.0.1 ")):
			t.Errorf("%s, --json: exit %d, stdout %q", c.monitor, r.code, r.stdout)
		}
	}
	fb.alerts = wire.Alerts{Monitor: wire.MonitorOn, Alerts: []wire.Alert{}}
	if r := runKey(t, fb, nil, "monitor", "alerts", "--app", "blog"); r.code != 0 || r.stdout != "127.0.0.1 says no alert of blog is held\n" {
		t.Errorf("none: exit %d, stdout %q", r.code, r.stdout)
	}

	// A name outside its grammar is not moteld's answer, and none of the
	// answer is printed.
	fb.alerts = hostileAlerts()
	fb.alerts.Alerts[0].Name = "x " + hostile[0]
	if r := runKey(t, fb, nil, "monitor", "alerts", "--app", "blog"); r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, "not moteld's answer: an alert's name in it is not one an alert has") {
		t.Errorf("a name outside the grammar: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	asked := len(fb.auth)
	for _, args := range [][]string{{"monitor", "alerts"}, {"monitor"}, {"monitor", "alerts", "--app", "Blog"}, {"monitor", "alerts", "blog"}} {
		if r := runKey(t, fb, nil, args...); r.code != 2 || len(fb.auth) != asked {
			t.Errorf("%v: exit %d, %d requests, stderr %q", args, r.code, len(fb.auth)-asked, r.stderr)
		}
	}
	// It is asked with a person's key alone: a deploy token is not sent.
	if r := runKey(t, fb, map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_KEY": ""}, "monitor", "alerts", "--app", "blog"); r.code != 2 || len(fb.auth) != asked {
		t.Errorf("with MOTEL_TOKEN: exit %d, %d requests, stderr %q", r.code, len(fb.auth)-asked, r.stderr)
	}
}
