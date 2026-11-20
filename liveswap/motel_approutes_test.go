package liveswap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// fakeAlerts is an AlertsSource that counts its calls.
type fakeAlerts struct {
	mu    sync.Mutex
	held  []AppAlert
	ok    bool
	calls int
}

func (f *fakeAlerts) Alerts(context.Context) (HeldAlerts, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if !f.ok {
		return HeldAlerts{}, false
	}
	return HeldAlerts{Alerts: f.held}, true
}

func (f *fakeAlerts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// alertsRig is the handler of the app-scoped routes' tests: demo,
// which alice views and whose env_file holds a value, and blog, an app
// no grant of alice's or dave's covers.
func alertsRig(t *testing.T) (*Handler, *testPeople) {
	t.Helper()
	people := newTestPeople(t)
	h, rig := newTestHandler(t)
	h.people = people.reg
	blog := newManagedApp("blog")
	blog.spec, blog.verifiers, blog.logger = rig.spec, rig.ma.verifiers, zap.NewNop()
	h.app.managed["blog"] = blog
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("DB_PASSWORD=hunter2-of-demo\n"), 0o600))
	return h, people
}

// TestAlertsRoute: GET /_motel/apps/<app>/alerts is a read of one app
// for a person whose grants cover it. Every other caller is refused
// before the source is asked, each as on GET /<app>: a person whose
// grants do not cover the name the same 403 whether it is an app here
// or not, a viewer of the box the 404 for a name that is no app, a
// global deploy source the 403 that names a declared key, every other
// deploy token, a key no line lists and no token the flat 401.
func TestAlertsRoute(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeAlerts{ok: true}
	h.app.SetAlertsSource(src)
	const viewerApp = `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"app"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/apps/demo/alerts")
	if flat.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", flat.Code, flat.Body)
	}
	for _, c := range []struct {
		name, target, auth string
		code               int
		body               string // "" for the flat 401
	}{
		{"a viewer of demo, on blog", "/_motel/apps/blog/alerts", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, on a name that is no app", "/_motel/apps/ghost/alerts", token(people.alice), http.StatusForbidden, viewerApp},
		{"a person with no grant", "/_motel/apps/demo/alerts", token(people.dave), http.StatusForbidden, viewerApp},
		{"a person with no grant, on a name that is no app", "/_motel/apps/ghost/alerts", token(people.dave), http.StatusForbidden, viewerApp},
		{"a viewer of the box, on a name that is no app", "/_motel/apps/ghost/alerts", token(people.carol), http.StatusNotFound, `{"error":"unknown app \"ghost\""}` + "\n"},
		{"a global deploy source", "/_motel/apps/demo/alerts", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"a global deploy source, on a name that is no app", "/_motel/apps/ghost/alerts", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"demo's deploy source", "/_motel/apps/demo/alerts", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
		{"a key no line lists", "/_motel/apps/demo/alerts", token(people.stranger), http.StatusUnauthorized, ""},
		{"a viewer of demo, a query on blog", "/_motel/apps/blog/alerts?lines=50", token(people.alice), http.StatusForbidden, viewerApp},
	} {
		w := sendTo(t, h, "GET", c.target, c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
	}
	// A name the grants do not cover is told alike whether it is an app.
	sameAnswer(t, "an app no grant covers and a name that is no app",
		sendTo(t, h, "GET", "/_motel/apps/blog/alerts", token(people.alice)), sendTo(t, h, "GET", "/_motel/apps/ghost/alerts", token(people.alice)))
	if n := src.count(); n != 0 {
		t.Errorf("the refusals asked the source %d times, want none", n)
	}
	// A person's token is good on a GET alone: the flat 401.
	if w := sendTo(t, h, "POST", "/_motel/apps/demo/alerts", token(people.alice)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	for _, who := range []string{"alice", "carol"} {
		k := map[string]string{"alice": token(people.alice), "carol": token(people.carol)}[who]
		if w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts", k); w.Code != http.StatusOK || w.Body.String() != `{"monitor":"on","alerts":[]}`+"\n" {
			t.Errorf("%s, demo: %d %s", who, w.Code, w.Body)
		}
	}
	if n := src.count(); n != 2 {
		t.Errorf("two reads asked the source %d times", n)
	}
}

// TestAlertsRouteTakesNoQuery: the alerts row takes no query, so any
// query, an empty one and one that does not parse among them, is the
// 422 that says so, naming the route by its pattern and echoing nothing
// of the request; and the source is not asked.
func TestAlertsRouteTakesNoQuery(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeAlerts{ok: true}
	h.app.SetAlertsSource(src)
	const body = `{"error":"GET /_motel/apps/{app}/alerts takes no query"}` + "\n"
	for _, q := range []string{"?", "?lines=50", "?app=blog", "?%ZZ", "?a;b", "?secret-guess-hunter2", "?&", "?monitor=off&monitor=on"} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts"+q, token(people.alice))
		if w.Code != http.StatusUnprocessableEntity || w.Body.String() != body {
			t.Errorf("%s: %d %q, want 422 %q", q, w.Code, w.Body, body)
		}
	}
	if n := src.count(); n != 0 {
		t.Errorf("a query asked the source %d times", n)
	}
}

// TestAlertsRoutePaths: only /_motel/apps/<app>/alerts exactly is the
// row, taken as sent: the app segment held to the app grammar, and no
// segment more, less, empty or dotted. Anything else under /_motel/ is
// the 404 every path with no row gets once authenticated, and the
// source is not asked.
func TestAlertsRoutePaths(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeAlerts{ok: true}
	h.app.SetAlertsSource(src)
	notFound := `{"error":"not found"}` + "\n"
	for _, target := range []string{
		"/_motel/apps/demo/alerts/",
		"/_motel/apps/demo/alerts/x",
		"/_motel/apps/demo//alerts",
		"/_motel/apps//alerts",
		"/_motel/apps/demo/./alerts",
		"/_motel/apps/x/../demo/alerts",
		"/_motel/apps/./demo/alerts",
		"/_motel//apps/demo/alerts",
		"/_motel/apps/Demo/alerts",
		"/_motel/apps/demo/Alerts",
		"/_motel/apps/de_mo/alerts",
		"/_motel/apps/" + strings.Repeat("a", appNameMaxLen+1) + "/alerts",
		"/_motel/apps/demo",
		"/_motel/apps/demo/",
		"/_motel/apps/",
		"/_motel/apps/demo/logs",
		"/_motel/apps/demo/alerts%00",
		"/_motel/apps/demo%20/alerts",
	} {
		for _, k := range []string{token(people.carol), token(people.alice)} {
			if w := sendTo(t, h, "GET", target, k); w.Code != http.StatusNotFound || w.Body.String() != notFound {
				t.Errorf("%s: %d %q, want the 404", target, w.Code, w.Body)
			}
		}
		if w := sendTo(t, h, "GET", target, "Bearer "+globalToken(t)); w.Code != http.StatusNotFound {
			t.Errorf("%s, a global deploy source: %d %q, want the 404", target, w.Code, w.Body)
		}
	}
	if n := src.count(); n != 0 {
		t.Errorf("a path with no row asked the source %d times", n)
	}
	// The path is the decoded one, as for GET /<app>: an escaped slash
	// names the same app and read as the plain path, nothing else.
	if w := sendTo(t, h, "GET", "/_motel/apps/demo%2Falerts", token(people.alice)); w.Code != http.StatusOK {
		t.Errorf("an escaped slash: %d %s", w.Code, w.Body)
	}
}

// TestAlertsRouteBody: what the source holds of the handle's app
// alone, newest first, through that app's filter, with Alertmanager's
// state, when the last silence ends, and the deploy mute; another app's
// alerts and the box's (one naming the app in its instance and summary
// among them) are left out; no source is "off", and a source that did
// not answer "unanswered", both with an empty list.
func TestAlertsRouteBody(t *testing.T) {
	h, people := alertsRig(t)
	alice := token(people.alice)
	read := func() (int, string) {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts", alice)
		return w.Code, w.Body.String()
	}
	if code, body := read(); code != http.StatusOK || body != `{"monitor":"off","alerts":[]}`+"\n" {
		t.Errorf("no source: %d %s", code, body)
	}
	h.app.SetAlertsSource(&fakeAlerts{ok: false})
	if code, body := read(); code != http.StatusOK || body != `{"monitor":"unanswered","alerts":[]}`+"\n" {
		t.Errorf("unanswered: %d %s", code, body)
	}

	// A version demo's status names is a safe string to the filter: a
	// summary may name it.
	sha := strings.Repeat("4c2d9e7b1a05", 3) + "0f9e"
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"`+sha+`"}`); w.Code != http.StatusOK {
		t.Fatalf("deploying %s: %d %s", sha, w.Code, w.Body)
	}
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	until := t0.Add(2 * time.Hour)
	h.app.SetAlertsSource(&fakeAlerts{ok: true, held: []AppAlert{
		{App: "demo", Alert: wire.Alert{Name: "watchdog_throttled", Severity: "critical", Summary: "demo: the watchdog spent its restart budget", StartsAt: t0, State: "suppressed", Inhibited: true}},
		{App: "blog", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "blog: errors over 5 %", StartsAt: t0.Add(time.Minute), State: "active"}},
		{App: "demo", Alert: wire.Alert{Name: "deploy_failed", Severity: "warning", Summary: "deploy of " + sha + " failed at starting; the env said hunter2-of-demo", StartsAt: t0.Add(2 * time.Minute), State: "suppressed", SilencedUntil: &until}},
		{Alert: wire.Alert{Name: "backup_stale", Severity: "critical", Summary: "the backup of demo is stale", StartsAt: t0.Add(3 * time.Minute), State: "active"}},
		{App: "demo", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "demo: errors over 5 %", StartsAt: t0, State: "active"}},
	}})
	code, body := read()
	const want = `{"alerts":[` +
		`{"name":"deploy_failed","severity":"warning","summary":"deploy of 4c2d9e7b1a054c2d9e7b1a054c2d9e7b1a050f9e failed at starting; the env said [redacted:DB_PASSWORD]","starts_at":"2026-11-19T14:02:00Z","state":"suppressed","silenced_until":"2026-11-19T16:00:00Z"},` +
		`{"name":"errors_high","severity":"warning","summary":"demo: errors over 5 %","starts_at":"2026-11-19T14:00:00Z","state":"active"},` +
		`{"name":"watchdog_throttled","severity":"critical","summary":"demo: the watchdog spent its restart budget","starts_at":"2026-11-19T14:00:00Z","state":"suppressed","inhibited":true}],` +
		`"monitor":"on","redacted_env":["DB_PASSWORD"]}` + "\n"
	if code != http.StatusOK || body != want {
		t.Errorf("the alerts of demo:\n got %d %s\nwant %s", code, body, want)
	}
	var got wire.Alerts
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Monitor != wire.MonitorOn || len(got.Alerts) != 3 {
		t.Errorf("decoded: %v %+v", err, got)
	}
}

// TestAlertsChosenByTheHandle: the alerts are chosen by the handle's
// app, the one allowApp made for the name Decide was asked about, and
// nothing else: the same source read for blog gives blog's alone.
func TestAlertsChosenByTheHandle(t *testing.T) {
	h, _ := alertsRig(t)
	src := &fakeAlerts{ok: true, held: []AppAlert{
		{App: "demo", Alert: wire.Alert{Name: "a", State: "active"}},
		{App: "blog", Alert: wire.Alert{Name: "b", State: "active"}},
		{App: "", Alert: wire.Alert{Name: "c", State: "active"}},
	}}
	for app, want := range map[string]string{"demo": "a", "blog": "b"} {
		calls := recordDecide(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		carol := access.Principal{Kind: access.Person, Name: "carol", Views: &access.Views{Box: true}}
		hd, err := h.allowApp(carol, access.AppAlerts, app)
		if err != nil || len(*calls) != 1 || (*calls)[0].resource != app {
			t.Fatalf("%s: %v %+v", app, err, *calls)
		}
		got := hd.alerts(req.Context(), src)
		if len(got.Alerts) != 1 || got.Alerts[0].Name != want {
			t.Errorf("%s: %+v, want %s's alone", app, got, app)
		}
	}
}

// TestAlertsTimesPassNoFilter: an env_file value that is a piece of an
// alert's time (a date) is masked in the summary and leaves the times
// whole, so the answer still reads.
func TestAlertsTimesPassNoFilter(t *testing.T) {
	h, people := alertsRig(t)
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("API_VERSION=2026-11-19\n"), 0o600))
	t0 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	until := t0.Add(time.Hour)
	h.app.SetAlertsSource(&fakeAlerts{ok: true, held: []AppAlert{
		{App: "demo", Alert: wire.Alert{Name: "deploy_failed", Severity: "warning", Summary: "on 2026-11-19", StartsAt: t0, State: "suppressed", SilencedUntil: &until}},
		{App: "demo", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "demo: errors", StartsAt: t0.Add(-time.Hour), State: "active"}},
	}})
	w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts", token(people.alice))
	var got wire.Alerts
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || len(got.Alerts) != 2 || got.Monitor != wire.MonitorOn {
		t.Fatalf("%v: %d %s", err, w.Code, w.Body)
	}
	a := got.Alerts[0]
	if a.Summary != "on [redacted:API_VERSION]" || !a.StartsAt.Equal(t0) || a.SilencedUntil == nil || !a.SilencedUntil.Equal(until) || !got.Alerts[1].StartsAt.Equal(t0.Add(-time.Hour)) {
		t.Errorf("the rows: %s", w.Body)
	}
	if !strings.HasSuffix(w.Body.String(), `"monitor":"on","redacted_env":["API_VERSION"]}`+"\n") {
		t.Errorf("the body does not name the key it masked: %s", w.Body)
	}
	// A withheld body (the env_file unreadable) is the filter's sentence.
	must(t, os.Remove(h.app.managed["demo"].spec.envFile))
	h.app.managed["demo"].secretsLoaded = false
	w = sendTo(t, h, "GET", "/_motel/apps/demo/alerts", token(people.alice))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), `{"error":"response withheld`) {
		t.Errorf("withheld: %d %s", w.Code, w.Body)
	}
}

// TestAlertsFilterFallsBack: a value of the app's that spans two
// alerts changes the body's structure: the body then goes whole through
// the filter, as before the times were kept out of it, and the value is
// not in the bytes sent.
func TestAlertsFilterFallsBack(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	held := []AppAlert{
		{App: "demo", Alert: wire.Alert{Name: "deploy_failed", Severity: "warning", Summary: "deploy of v2 failed at starting", StartsAt: t0.Add(time.Minute), State: "suppressed"}},
		{App: "demo", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "demo: 5xx over 5 %", StartsAt: t0, State: "active"}},
	}
	for _, value := range []string{`"},{"name":"errors_high`, `"},{"name":"errors_high","severity":"warning","summary":"demo`} {
		h, people := alertsRig(t)
		must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K="+value+"\n"), 0o600))
		h.app.SetAlertsSource(&fakeAlerts{ok: true, held: held})
		w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts", token(people.alice))
		if strings.Contains(w.Body.String(), value) || !strings.Contains(w.Body.String(), "redacted:K") || !strings.Contains(w.Body.String(), `"starts_at":"2026-11-19T13:01:00Z"`) {
			t.Errorf("K=%s: %d %s", value, w.Code, w.Body)
		}
	}
}
