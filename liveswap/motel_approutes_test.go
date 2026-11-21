package liveswap

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
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
		"/_motel/apps/demo/log",
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
		carol := access.Principal{Kind: access.Person, Name: "carol", Views: &access.Views{Viewer: access.Scopes{Box: true}}}
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

// withProbeRow adds, until the test ends, a row probe that takes lines,
// a closed set, and after, a key held to journalCursor, whose function
// records the query it is given and answers 204: the keys a read of
// the journal declares, with no reader. It swaps the package's table,
// as recordRows does, so no test that calls it may run in parallel.
func withProbeRow(t *testing.T) *[]appQuery {
	t.Helper()
	saved := motelAppRoutes
	t.Cleanup(func() { motelAppRoutes = saved })
	got := new([]appQuery)
	motelAppRoutes = append(slices.Clone(saved), motelAppRoute{
		"probe", access.AppAlerts,
		[]queryKey{{name: "lines", values: []string{"200", "50", "1000"}}, {name: "after", grammar: journalCursor}},
		func(_ *Handler, w http.ResponseWriter, _ *http.Request, _ appHandle, q appQuery) error {
			*got = append(*got, q)
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})
	return got
}

// probe422 is the probe row's 422: each key in the row's own words.
const probe422 = `{"error":"GET /_motel/apps/{app}/probe takes only lines (one of 200, 50, 1000) and after (a journal cursor, as an answer gave it), each once"}` + "\n"

// TestQueryKeyGrammar: a key held to a grammar is in the query only
// when named, and then with the value as url.ParseQuery decoded it, a
// cursor byte for byte, taken only when the grammar takes it whole;
// the closed set beside it keeps its default. A raw ";" is no
// separator url.ParseQuery takes, so a cursor sent with one is the 422,
// never a key dropped. Anything else is the row's 422 too, and the
// row's function does not run.
func TestQueryKeyGrammar(t *testing.T) {
	h, people := alertsRig(t)
	got := withProbeRow(t)
	alice := token(people.alice)
	esc := url.QueryEscape
	type cs = []struct {
		name, query string
		want        appQuery // nil for the 422
	}
	cases := cs{
		{"no query", "", appQuery{"lines": "200"}},
		{"an empty query", "?", appQuery{"lines": "200"}},
		{"lines alone", "?lines=50", appQuery{"lines": "50"}},
		{"after as url.Values writes it", "?" + url.Values{"after": {testCursor}}.Encode(), appQuery{"lines": "200", "after": testCursor}},
		{"after, = raw and ; as %3B", "?after=" + strings.ReplaceAll(testCursor, ";", "%3B"), appQuery{"lines": "200", "after": testCursor}},
		{"after, ; as %3b", "?after=" + strings.ReplaceAll(testCursor, ";", "%3b"), appQuery{"lines": "200", "after": testCursor}},
		{"after and lines", "?" + url.Values{"after": {testCursor}, "lines": {"1000"}}.Encode(), appQuery{"lines": "1000", "after": testCursor}},
		{"after, then lines", "?after=" + esc(testCursor) + "&lines=50", appQuery{"lines": "50", "after": testCursor}},
		{"the widest cursor", "?after=" + esc(widestCursor), appQuery{"lines": "200", "after": widestCursor}},
		{"a cursor of zeros", "?after=" + esc(zeroCursor), appQuery{"lines": "200", "after": zeroCursor}},
		{"after with a raw ;", "?after=" + testCursor, nil},
		{"after with a raw ;, after lines", "?lines=50&after=" + testCursor, nil},
		{"after with a raw ;, before lines", "?after=" + testCursor + "&lines=50", nil},
		{"after with %2B, a +", "?after=" + strings.Replace(esc(testCursor), "i%3D826", "i%3D%2B826", 1), nil},
		{"after with a raw +, a space", "?after=" + strings.Replace(esc(testCursor), "i%3D826", "i%3D82+", 1), nil},
		{"after empty", "?after=", nil},
		{"after with no =", "?after", nil},
		{"after with a NUL", "?after=" + esc(testCursor) + "%00", nil},
		{"after twice, the same", "?after=" + esc(testCursor) + "&after=" + esc(testCursor), nil},
		{"after that does not parse", "?after=%ZZ", nil},
		{"a key the row does not take", "?after=" + esc(testCursor) + "&since=1h", nil},
		{"After (case)", "?After=" + esc(testCursor), nil},
		{"lines outside its set", "?lines=7&after=" + esc(testCursor), nil},
		{"the app's env_file value", "?after=hunter2-of-demo", nil},
	}
	for name, form := range cursorForms {
		cases = append(cases, struct {
			name, query string
			want        appQuery
		}{"a cursor's form: " + name, "?after=" + esc(form), nil})
	}
	for _, c := range cases {
		*got = nil
		w := sendTo(t, h, "GET", "/_motel/apps/demo/probe"+c.query, alice)
		switch {
		case c.want == nil && (w.Code != http.StatusUnprocessableEntity || w.Body.String() != probe422):
			t.Errorf("%s: %d %q, want 422 %q", c.name, w.Code, w.Body, probe422)
		case c.want == nil && len(*got) != 0:
			t.Errorf("%s: the row's function ran with %v", c.name, *got)
		case c.want != nil && (w.Code != http.StatusNoContent || len(*got) != 1 || !maps.Equal((*got)[0], c.want)):
			t.Errorf("%s: %d %s, the function given %v, want %v", c.name, w.Code, w.Body, *got, c.want)
		}
	}
}

// TestQueryKeyGrammarSays: the 422 of a row with a grammar key is the
// row's words alone whatever the value: one holding the app's env_file
// value is told no marker of it, and nothing of a cursor or of a long
// value comes back.
func TestQueryKeyGrammarSays(t *testing.T) {
	h, people := alertsRig(t)
	withProbeRow(t)
	for _, q := range []string{
		"?after=hunter2-of-demo",
		"?after=" + url.QueryEscape(testCursor+";q=hunter2-of-demo"),
		"?lines=50&after=hunter2-of-demo",
		"?hunter2-of-demo=" + url.QueryEscape(testCursor),
		"?after=" + testCursor,
		"?after=" + strings.Repeat("hunter2-of-demo", 300),
	} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/probe"+q, token(people.alice))
		if w.Code != http.StatusUnprocessableEntity || w.Body.String() != probe422 {
			t.Errorf("%.40s: %d %q, want 422 %q", q, w.Code, w.Body, probe422)
		}
	}
}

// anchored is whether re matches whole values alone: its pattern, as
// parsed, one sequence from the start of the text to its end, so that
// no alternative at the top escapes either anchor (`^a|b$` matches any
// value ending in b).
func anchored(re *regexp.Regexp) bool {
	p, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return false
	}
	p = p.Simplify()
	return p.Op == syntax.OpConcat && len(p.Sub) >= 2 &&
		p.Sub[0].Op == syntax.OpBeginText && p.Sub[len(p.Sub)-1].Op == syntax.OpEndText
}

// TestAppRouteKeys: every key of every row, the probe's among them, has
// a name no other key of its row has, and exactly one of a closed set of
// distinct, non-empty values or a grammar; a grammar is anchored, takes
// no empty value (so a key not named is told from one named) and has
// its words. A key with neither would panic on its row's first request.
func TestAppRouteKeys(t *testing.T) {
	for pattern, want := range map[string]bool{
		`^[a-z]{3}$`: true, `^(?:a|b)$`: true, `^(a|bc)$`: true,
		`^a|b$`: false, `^a$|^b$`: false, `(?m)^a$`: false, `^a`: false, `a$`: false, `a`: false, `^a\z`: true,
	} {
		if anchored(regexp.MustCompile(pattern)) != want {
			t.Errorf("anchored(%s) = %v", pattern, !want)
		}
	}
	withProbeRow(t)
	for _, rt := range motelAppRoutes {
		seen := map[string]bool{}
		for _, k := range rt.keys {
			switch g := k.grammar; {
			case k.name == "" || seen[k.name]:
				t.Errorf("%s: key %q empty or twice", rt.read, k.name)
			case (g == nil) == (len(k.values) == 0):
				t.Errorf("%s: key %q has %d values and a grammar %v: it needs one of the two", rt.read, k.name, len(k.values), g)
			case g != nil && (!anchored(g.re) || g.re.MatchString("") || g.says == ""):
				t.Errorf("%s: key %q's grammar %s is not anchored, takes \"\" or has no words", rt.read, k.name, g.re)
			case g == nil && (slices.Contains(k.values, "") || len(slices.Compact(slices.Sorted(slices.Values(k.values)))) != len(k.values)):
				t.Errorf("%s: key %q's values %q are not distinct and non-empty", rt.read, k.name, k.values)
			}
			seen[k.name] = true
		}
	}
	if !slices.ContainsFunc(motelAppRoutes, func(rt motelAppRoute) bool {
		return slices.ContainsFunc(rt.keys, func(k queryKey) bool { return k.grammar != nil })
	}) {
		t.Error("no row holds a key to a grammar: the probe row did not reach the table")
	}
}
