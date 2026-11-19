package client

import (
	"net/http"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestReadAlerts: GET /_motel/apps/<app>/alerts's answer is taken only
// with its monitor and its alerts, and with every value the client
// prints as a token in its grammar: an alert's name, its severity and
// its state, a word each (a marker of the box's filter is shown as the
// client's words); the monitor a word, which may be one the client does
// not know. The summary is free text, printed labelled and capped.
func TestReadAlerts(t *testing.T) {
	cl := &Client{Box: Box{Host: "127.0.0.1"}}
	const whole = `{"monitor":"on","alerts":[{"name":"deploy_failed","severity":"warning","summary":"deploy of v2 failed \u001b[2J","starts_at":"2026-11-19T14:02:00Z","state":"suppressed","silenced_until":"2026-11-19T16:00:00Z"},` +
		`{"name":"watchdog_throttled","severity":"critical","summary":"","starts_at":"2026-11-19T13:00:00Z","state":"suppressed","inhibited":true}]}`
	read := func(body string) (wire.Alerts, error) {
		var a wire.Alerts
		ans := &Answer{Code: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
		return a, cl.Read(ans, &a)
	}
	if a, err := read(whole); err != nil || len(a.Alerts) != 2 || a.Alerts[0].SilencedUntil == nil || !a.Alerts[1].Inhibited {
		t.Fatalf("the whole answer: %v, %+v", err, a)
	}
	for _, c := range []struct{ name, body string }{
		{"a monitor this client does not know", strings.Replace(whole, `"monitor":"on"`, `"monitor":"later"`, 1)},
		{"no alert", `{"monitor":"off","alerts":[]}`},
		{"a state a later box says", strings.Replace(whole, `"state":"suppressed"`, `"state":"muted"`, 1)},
		{"no severity", strings.Replace(whole, `"severity":"warning",`, "", 1)},
		{"a name of capitals", strings.Replace(whole, `"deploy_failed"`, `"ChannelFailed"`, 1)},
		{"a Prometheus rule's name", strings.Replace(whole, `"deploy_failed"`, `"job:latency.p99-high"`, 1)},
		{"a monitor word with an underscore", strings.Replace(whole, `"monitor":"on"`, `"monitor":"not_set_up"`, 1)},
	} {
		if _, err := read(c.body); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if a, err := read(strings.Replace(whole, `"deploy_failed"`, `"[redacted:NAME]"`, 1)); err != nil || a.Alerts[0].Name != Filtered {
		t.Errorf("a marker in a name: %v, %q", err, a.Alerts[0].Name)
	}
	for _, c := range []struct{ name, body, want string }{
		{"no monitor", strings.Replace(whole, `"monitor":"on",`, "", 1), "it has no monitor)"},
		{"no alerts", `{"monitor":"on"}`, "it has no alerts)"},
		{"alerts not a list", `{"monitor":"on","alerts":{}}`, "it has no alerts)"},
		{"a monitor not a word", strings.Replace(whole, `"monitor":"on"`, `"monitor":"on \u001b[2J"`, 1), "its monitor is not a word)"},
		{"a name outside the grammar", strings.Replace(whole, `"deploy_failed"`, `"deploy failed"`, 1), "an alert's name in it is not one an alert has)"},
		{"a severity not a word", strings.Replace(whole, `"warning"`, `"warning\u001b[31m"`, 1), "an alert's severity or state in it is not a word)"},
		{"a state not a word", strings.Replace(whole, `"state":"suppressed"`, `"state":"Suppressed!"`, 1), "an alert's severity or state in it is not a word)"},
		{"an empty state", strings.Replace(whole, `"state":"suppressed"`, `"state":""`, 1), "an alert's severity or state in it is not a word)"},
	} {
		_, err := read(c.body)
		f := failure(t, err)
		if f.Exit != ExitUnknown || !strings.Contains(f.Message, "not moteld's answer: "+c.want) {
			t.Errorf("%s: exit %d %q", c.name, f.Exit, f.Message)
		}
	}
}
