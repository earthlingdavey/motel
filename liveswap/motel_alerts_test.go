package liveswap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// fakeHeld is an AlertsSource that gives what it holds, alerts and
// silences, and counts its calls.
type fakeHeld struct {
	mu    sync.Mutex
	held  HeldAlerts
	ok    bool
	calls int
}

func (f *fakeHeld) Alerts(context.Context) (HeldAlerts, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if !f.ok {
		return HeldAlerts{}, false
	}
	return f.held, true
}

func (f *fakeHeld) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestBoxAlertsRoute: GET /_motel/alerts is a viewer of the box's
// alone. A viewer of one app, a person with no grant and a global
// deploy source are refused before the source is asked, and every
// other deploy token, a key no line lists and no token get the flat
// 401; a query or a path below it is the 404 of every row under
// /_motel/; the box's viewer gets every alert and silence.
func TestBoxAlertsRoute(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeHeld{ok: true}
	h.app.SetAlertsSource(src)
	const viewerBox = `{"error":"this needs the viewer role on the box","needs":"viewer","scope":"box"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/alerts")
	if flat.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", flat.Code, flat.Body)
	}
	for _, c := range []struct {
		name, auth string
		code       int
		body       string // "" for the flat 401
	}{
		{"a viewer of demo", token(people.alice), http.StatusForbidden, viewerBox},
		{"a person with no grant", token(people.dave), http.StatusForbidden, viewerBox},
		{"a key no line lists", token(people.stranger), http.StatusUnauthorized, ""},
		{"a global deploy source", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"an app's deploy source", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
	} {
		w := sendTo(t, h, "GET", "/_motel/alerts", c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
		// A query or a path trick is no row: the 404 once authenticated,
		// before Decide, so the refused are told it too.
		for _, target := range []string{"/_motel/alerts?app=demo", "/_motel/alerts?", "/_motel/alerts/", "/_motel/alerts/demo", "/_motel//alerts", "/_motel/./alerts", "/_motel/Alerts", "/_motel/alerts%00"} {
			w := sendTo(t, h, "GET", target, c.auth)
			if c.body == "" {
				sameAnswer(t, c.name+", "+target, w, flat)
			} else if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"not found"}`+"\n" {
				t.Errorf("%s, %s: %d %q, want the 404", c.name, target, w.Code, w.Body)
			}
		}
	}
	if w := sendTo(t, h, "POST", "/_motel/alerts", token(people.carol)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/_motel/alerts?app=demo", "/_motel/alerts?", "/_motel/alerts/", "/_motel/alerts/demo", "/_motel//alerts", "/_motel/./alerts", "/_motel/Alerts"} {
		if w := sendTo(t, h, "GET", target, token(people.carol)); w.Code != http.StatusNotFound {
			t.Errorf("the box's viewer, %s: %d %q, want the 404", target, w.Code, w.Body)
		}
	}
	if n := src.count(); n != 0 {
		t.Errorf("the refusals asked the source %d times, want none", n)
	}
	w := sendTo(t, h, "GET", "/_motel/alerts", token(people.carol))
	if w.Code != http.StatusOK || w.Body.String() != `{"monitor":"on","alerts":[],"silences":[]}`+"\n" || src.count() != 1 {
		t.Errorf("the box's viewer: %d %s, the source asked %d times", w.Code, w.Body, src.count())
	}
	wantNoMotelHeader(t, w)
}

// TestBoxAlertsBody: every alert and every silence that has not ended,
// newest first; an app's alert through that app's filter, so its
// env_file value is masked and a version its status names stands; a box
// event, an alert of an app this config does not run, and every silence
// through a filter with no app's values, a generated token masked and
// the silence ids, app names and release names standing; an alert the
// deploy mute holds is there, inhibited; an expired silence is left out;
// no source is "off", and a source that did not answer "unanswered",
// both with empty lists.
func TestBoxAlertsBody(t *testing.T) {
	h, people := alertsRig(t)
	carol := token(people.carol)
	read := func() (int, string) {
		w := sendTo(t, h, "GET", "/_motel/alerts", carol)
		return w.Code, w.Body.String()
	}
	if code, body := read(); code != http.StatusOK || body != `{"monitor":"off","alerts":[],"silences":[]}`+"\n" {
		t.Errorf("no source: %d %s", code, body)
	}
	h.app.SetAlertsSource(&fakeHeld{ok: false})
	if code, body := read(); code != http.StatusOK || body != `{"monitor":"unanswered","alerts":[],"silences":[]}`+"\n" {
		t.Errorf("unanswered: %d %s", code, body)
	}

	sha := strings.Repeat("4c2d9e7b1a05", 3) + "0f9e"
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"`+sha+`"}`); w.Code != http.StatusOK {
		t.Fatalf("deploying %s: %d %s", sha, w.Code, w.Body)
	}
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	until := t0.Add(2 * time.Hour)
	h.app.SetAlertsSource(&fakeHeld{ok: true, held: HeldAlerts{
		Alerts: []AppAlert{
			{App: "demo", Alert: wire.Alert{Name: "watchdog_throttled", Severity: "critical", Summary: "demo: throttled", StartsAt: t0, State: "suppressed", Inhibited: true}},
			{App: "demo", Alert: wire.Alert{Name: "deploy_failed", Severity: "warning", Summary: "deploy of " + sha + " failed; the env said hunter2-of-demo", StartsAt: t0.Add(2 * time.Minute), State: "suppressed", SilencedUntil: &until}},
			{Instance: "/var/lib/liveswap", Alert: wire.Alert{Name: "disk_low", Severity: "warning", Summary: "the disk said " + generated + " and hunter2-of-demo", StartsAt: t0.Add(3 * time.Minute), State: "active"}},
			{App: "gone", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "gone: hunter2-of-demo", StartsAt: t0.Add(time.Minute), State: "active"}},
		},
		Silences: []wire.Silence{
			{ID: "11111111-1111-4111-8111-111111111111", State: wire.SilenceActive, StartsAt: t0, EndsAt: until, CreatedBy: "moteld monitor: root", Comment: "known " + generated,
				Matchers: []wire.Matcher{{Name: "app", Op: wire.MatchEqual, Value: "demo"}, {Name: "release", Op: wire.MatchEqual, Value: sha}}},
			{ID: "22222222-2222-4222-8222-222222222222", State: "expired", StartsAt: t0.Add(-time.Hour), EndsAt: t0, CreatedBy: "x", Comment: "old"},
			{ID: "33333333-3333-4333-8333-333333333333", State: wire.SilencePending, StartsAt: until, EndsAt: until.Add(time.Hour), CreatedBy: "root", Comment: "later",
				Matchers: []wire.Matcher{{Name: "alertname", Op: wire.MatchRegexp, Value: "cert_.*"}, {Name: "instance", Op: wire.MatchNotEqual, Value: "b.example"}}},
		},
	}})
	code, body := read()
	const want = `{"monitor":"on","alerts":[` +
		`{"name":"disk_low","severity":"warning","summary":"the disk said [masked, 32 chars] and hunter2-of-demo","starts_at":"2026-11-19T14:03:00Z","state":"active","instance":"/var/lib/liveswap"},` +
		`{"app":"demo","name":"deploy_failed","redacted_env":["DB_PASSWORD"],"severity":"warning","silenced_until":"2026-11-19T16:00:00Z","starts_at":"2026-11-19T14:02:00Z","state":"suppressed","summary":"deploy of 4c2d9e7b1a054c2d9e7b1a054c2d9e7b1a050f9e failed; the env said [redacted:DB_PASSWORD]"},` +
		`{"name":"errors_high","severity":"warning","summary":"gone: hunter2-of-demo","starts_at":"2026-11-19T14:01:00Z","state":"active","app":"gone"},` +
		`{"name":"watchdog_throttled","severity":"critical","summary":"demo: throttled","starts_at":"2026-11-19T14:00:00Z","state":"suppressed","inhibited":true,"app":"demo"}],` +
		`"silences":[` +
		`{"id":"33333333-3333-4333-8333-333333333333","state":"pending","matchers":[{"name":"alertname","op":"=~","value":"cert_.*"},{"name":"instance","op":"!=","value":"b.example"}],"starts_at":"2026-11-19T16:00:00Z","ends_at":"2026-11-19T17:00:00Z","created_by":"root","comment":"later"},` +
		`{"id":"11111111-1111-4111-8111-111111111111","state":"active","matchers":[{"name":"app","op":"=","value":"demo"},{"name":"release","op":"=","value":"4c2d9e7b1a054c2d9e7b1a054c2d9e7b1a050f9e"}],"starts_at":"2026-11-19T14:00:00Z","ends_at":"2026-11-19T16:00:00Z","created_by":"moteld monitor: root","comment":"known [masked, 32 chars]"}]}` + "\n"
	if code != http.StatusOK || body != want {
		t.Errorf("the box's alerts:\n got %d %s\nwant %s", code, body, want)
	}
	var got wire.BoxAlerts
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Monitor != wire.MonitorOn || len(got.Alerts) != 4 || len(got.Silences) != 2 {
		t.Errorf("decoded: %v %+v", err, got)
	}
}

// TestBoxAlertsCaps: the box sends at most BoxAlertsMax alerts and
// SilencesMax silences, the newest, and says it held more; at the cap
// exactly it says nothing more.
func TestBoxAlertsCaps(t *testing.T) {
	h, people := alertsRig(t)
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	held := func(alerts, silences int) HeldAlerts {
		var b HeldAlerts
		for i := range alerts {
			b.Alerts = append(b.Alerts, AppAlert{Instance: fmt.Sprintf("/d/%d", i), Alert: wire.Alert{Name: "disk_low", Severity: "warning", Summary: "full", StartsAt: t0.Add(time.Duration(i) * time.Second), State: "active"}})
		}
		for i := range silences {
			b.Silences = append(b.Silences, wire.Silence{ID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", i), State: wire.SilenceActive, StartsAt: t0.Add(time.Duration(i) * time.Second), EndsAt: t0.Add(time.Hour), Matchers: []wire.Matcher{}})
		}
		return b
	}
	for _, c := range []struct {
		alerts, silences int
		more             [2]bool
	}{
		{wire.BoxAlertsMax, wire.SilencesMax, [2]bool{false, false}},
		{wire.BoxAlertsMax + 1, wire.SilencesMax, [2]bool{true, false}},
		{wire.BoxAlertsMax, wire.SilencesMax + 7, [2]bool{false, true}},
	} {
		h.app.SetAlertsSource(&fakeHeld{ok: true, held: held(c.alerts, c.silences)})
		w := sendTo(t, h, "GET", "/_motel/alerts", token(people.carol))
		var got wire.BoxAlerts
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if len(got.Alerts) != min(c.alerts, wire.BoxAlertsMax) || len(got.Silences) != min(c.silences, wire.SilencesMax) || got.MoreAlerts != c.more[0] || got.MoreSilences != c.more[1] {
			t.Errorf("%d alerts, %d silences held: sent %d and %d, more %v %v", c.alerts, c.silences, len(got.Alerts), len(got.Silences), got.MoreAlerts, got.MoreSilences)
		}
		// The newest are sent.
		if got.Alerts[0].Instance != fmt.Sprintf("/d/%d", c.alerts-1) || got.Silences[0].ID != fmt.Sprintf("%08d-1111-4111-8111-111111111111", c.silences-1) {
			t.Errorf("%d alerts, %d silences held: the first sent are %q and %q", c.alerts, c.silences, got.Alerts[0].Instance, got.Silences[0].ID)
		}
	}
}

// TestBoxAlertsRowWithheld: a row the filter withholds keeps what holds
// the alert, says the filter's sentence in its summary, and loses its
// name and instance, which a value may be part of; it keeps its app
// only when the filter was that app's, an app this config runs, since
// another label's value passed no filter. A silence the filter
// withholds keeps its state, its times and an id Alertmanager made, the
// filter's sentence its comment, and loses its matchers and maker.
func TestBoxAlertsRowWithheld(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	until := t0.Add(time.Hour)
	a := wire.BoxAlert{App: "demo", Instance: "x", Alert: wire.Alert{Name: "n", Severity: "warning", Summary: "s", StartsAt: t0, State: "suppressed", SilencedUntil: &until}}
	withholds := &Redactor{withhold: "the app's env_file could not be read"}
	row := alertRow(a, withholds, true)
	const want = `{"name":"","severity":"warning","summary":"response withheld: the app's env_file could not be read","starts_at":"2026-11-19T14:00:00Z","state":"suppressed","silenced_until":"2026-11-19T15:00:00Z","app":"demo"}`
	if row != want {
		t.Errorf("withheld:\n got %s\nwant %s", row, want)
	}
	a.Severity = "page"
	if row := alertRow(a, withholds, true); !strings.Contains(row, `"severity":""`) {
		t.Errorf("a severity not motel's, withheld: %s", row)
	}
	a.App = "Xk29fQa7Lm3Zp8Rt5Wn1Yc6Vb4Hd0Jg2" // gitleaks:allow — made up for this test
	if row := alertRow(a, withholds, false); strings.Contains(row, a.App) || strings.Contains(row, `"app"`) {
		t.Errorf("a label of no app here, withheld: %s", row)
	}

	s := wire.Silence{ID: "11111111-1111-4111-8111-111111111111", State: wire.SilencePending, StartsAt: t0, EndsAt: until, CreatedBy: "root",
		Comment: "c", Matchers: []wire.Matcher{{Name: "app", Op: wire.MatchEqual, Value: "demo"}}}
	const wantSilence = `{"id":"11111111-1111-4111-8111-111111111111","state":"pending","matchers":[],"starts_at":"2026-11-19T14:00:00Z","ends_at":"2026-11-19T15:00:00Z","created_by":"","comment":"response withheld: the app's env_file could not be read"}`
	if row := silenceRow(s, withholds); row != wantSilence {
		t.Errorf("a silence withheld:\n got %s\nwant %s", row, wantSilence)
	}
	s.ID = "not-an-id " + generated
	if row := silenceRow(s, withholds); strings.Contains(row, generated) || !strings.Contains(row, `"id":""`) {
		t.Errorf("a silence withheld, its id not Alertmanager's: %s", row)
	}
	// Not withheld: the row as marshalled, through the filter.
	if row := silenceRow(wire.Silence{ID: "i", State: wire.SilenceActive, Matchers: []wire.Matcher{}, StartsAt: t0, EndsAt: until}, NewRedactor(nil, nil)); !strings.HasPrefix(row, `{"id":"i","state":"active"`) {
		t.Errorf("a silence not withheld: %s", row)
	}
}

// TestBoxAlertsEachAppItsFilter: with two apps, each alert of an app is
// filtered by that app's filter alone: each app's env_file value is
// masked in its own alert, and an alert of one app carrying the other's
// value keeps it (the filter of one app never sees the other's values),
// as GET /_motel/apps/<app>/alerts would send each.
func TestBoxAlertsEachAppItsFilter(t *testing.T) {
	r := newTwoAppsRig(t)
	alpha, bravo := r.canary["appalpha"], r.canary["appbravo"]
	r.h.app.SetAlertsSource(&fakeHeld{ok: true, held: HeldAlerts{Alerts: []AppAlert{
		{App: "appalpha", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "alpha said " + alpha, State: "active"}},
		{App: "appbravo", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "bravo said " + bravo + " and " + alpha, State: "active"}},
	}}})
	req := httptest.NewRequest(http.MethodGet, "http://"+personBox+"/_motel/alerts", nil)
	req.Header.Set("Authorization", r.authorization(t, "viewer of the box"))
	w := httptest.NewRecorder()
	if err := r.h.ServeHTTP(w, req, nil); err != nil {
		t.Fatal(err)
	}
	var got wire.BoxAlerts
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || len(got.Alerts) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	byApp := map[string]string{}
	for _, a := range got.Alerts {
		byApp[a.App] = a.Summary
	}
	if byApp["appalpha"] != "alpha said [redacted:SECRET_TOKEN]" || byApp["appbravo"] != "bravo said [redacted:SECRET_TOKEN] and "+alpha {
		t.Errorf("each app's alert: %s", w.Body)
	}
}

// TestBoxAlertsBodyIsTheWireType: the body the route writes, rows as
// raw bytes, has wire.BoxAlerts's keys in its order.
func TestBoxAlertsBodyIsTheWireType(t *testing.T) {
	tags := func(v any) []string {
		var out []string
		ty := reflect.TypeOf(v)
		for i := range ty.NumField() {
			out = append(out, ty.Field(i).Tag.Get("json"))
		}
		return out
	}
	if a, b := tags(boxAlertsBody{}), tags(wire.BoxAlerts{}); !reflect.DeepEqual(a, b) {
		t.Errorf("the route's body %v, the wire type %v", a, b)
	}
}
