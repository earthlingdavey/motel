package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// fakeRules is a RulesSource that gives its listing, or its error, and
// counts its calls.
type fakeRules struct {
	mu    sync.Mutex
	l     wire.RulesListing
	err   error
	calls int
}

func (f *fakeRules) Rules(context.Context) (wire.RulesListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.l, f.err
}

func (f *fakeRules) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func num(v float64) *float64 { return &v }

var rulesAt = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// rulesListing is a box's listing as notify's source gives it, for the
// apps demo and blog: a store rule of every app with both apps'
// numbers, one pinned to each, a store rule of the box's disks and one
// with no label, the box's checks (the disks, the backups, which name
// the apps in their instance) and an app's check with each app's
// budget, an app's event and a box event; and the store's socket and
// every query, which no person's answer holds.
func rulesListing() wire.RulesListing {
	since := rulesAt.Add(-4 * time.Minute)
	guards := []wire.GuardListing{
		{Query: `sum by (app) (increase(motel_app_requests_total[5m]))`, Op: ">=", Value: num(20), Match: "on (app)", Meaning: "at least 20 responses sent over 5m: under it, too few to judge"},
	}
	return wire.RulesListing{
		Store: &wire.StoreQueries{Socket: "/run/motel-metrics/vm.sock", QueryOffsetSeconds: 65,
			StateQuery: "last_over_time(ALERTS[2m])", ActiveSinceQuery: "last_over_time(ALERTS_FOR_STATE[2m])", Values: wire.StoreOn},
		RulesFile: wire.RulesFileDiffers,
		Alerts: []wire.AlertListing{
			{Name: "ChannelFailed", Source: "motel check", Severity: "warning", Subject: "box", Kind: "event"},
			{Name: "app_memory_high", Source: "builtin", Severity: "warning", Subject: "app", Kind: "threshold",
				ValueQuery: `max by (app) (last_over_time(motel_app_memory_bytes[75s]) / last_over_time(motel_app_memory_max_bytes[75s]))`,
				Op:         ">", Threshold: num(0.9), Unit: "share", ForSeconds: 600, Readings: []wire.Reading{
					{App: "blog", Value: num(0.93104), MeasuredAt: rulesAt, State: wire.RuleFiring, ActiveSince: &since},
					{App: "demo", Value: num(0.55), MeasuredAt: rulesAt, State: wire.RuleInactive},
				}},
			{Name: "backup_stale", Source: "motel check", Severity: "critical", Subject: "box", Kind: "threshold", Op: ">", Threshold: num(10800), Unit: "seconds",
				Readings: []wire.Reading{{Instance: "blog", Value: num(3480), MeasuredAt: rulesAt}, {Instance: "demo", Value: num(14370), MeasuredAt: rulesAt}}},
			{Name: "blog_slow", Source: "operator", Severity: "critical", Subject: "app", App: "blog", Kind: "guarded",
				ValueQuery: `histogram_quantile(0.95, sum by (app, le) (increase(motel_app_response_seconds_bucket{app="blog"}[5m])))`,
				Op:         ">", Threshold: num(1.5), Unit: "seconds", ForSeconds: 900, Guards: guards,
				Readings: []wire.Reading{{App: "blog", Value: num(1.75), MeasuredAt: rulesAt, State: wire.RulePending, ActiveSince: &since}}},
			{Name: "demo_quiet", Source: "operator", Severity: "warning", Subject: "app", App: "demo", Kind: "threshold",
				ValueQuery: `sum by (app) (increase(motel_app_requests_total{app="demo"}[5m])) / 300`,
				Op:         "<", Threshold: num(0.1), Unit: "requests/s", ForSeconds: 3600,
				Readings: []wire.Reading{{App: "demo", Value: num(0.25), MeasuredAt: rulesAt, State: wire.RuleInactive}}},
			{Name: "deploy_failed", Source: "motel check", Severity: "warning", Subject: "app", Kind: "event"},
			{Name: "disk_filling", Source: "operator", Severity: "critical", Subject: "box", Kind: "guarded",
				ValueQuery: `min by (path) (predict_linear(motel_disk_avail_bytes[42h], 604800))`, Op: "<", Threshold: num(0), Unit: "bytes", ForSeconds: 12600,
				Guards:   []wire.GuardListing{{Query: `max by (path) (present_over_time(motel_disk_avail_bytes[42h] offset 35h))`, Meaning: "the store holds a sample older than 35h"}},
				Readings: []wire.Reading{{Instance: "/srv/motel", Value: num(-1234570000), MeasuredAt: rulesAt, State: wire.RuleInactive}}},
			{Name: "disk_low", Source: "motel check", Severity: "warning", Subject: "box", Kind: "threshold", Op: ">=", Unit: "percent",
				Levels:   []wire.LevelListing{{Severity: "warning", Threshold: 80, ClearsBelow: 75}, {Severity: "critical", Threshold: 90, ClearsBelow: 85}},
				Readings: []wire.Reading{{Instance: "/srv/motel", Value: num(79.96), Level: "warning", MeasuredAt: rulesAt}}},
			{Name: "errors_high", Source: "operator", Severity: "warning", Subject: "app", Kind: "guarded",
				ValueQuery: `sum by (app) (increase(motel_app_requests_total{class="5xx"}[5m])) / sum by (app) (increase(motel_app_requests_total[5m]))`,
				Op:         ">", Threshold: num(0.05), Unit: "share", ForSeconds: 600, Guards: guards, Readings: []wire.Reading{
					{App: "blog", Value: num(0.0123), MeasuredAt: rulesAt, State: wire.RuleInactive},
					{App: "demo", Value: num(0.0612346), MeasuredAt: rulesAt, State: wire.RulePending, ActiveSince: &since},
				}},
			{Name: "metrics_store_disk_fill", Source: "builtin", Severity: "warning", Subject: "box", Kind: "threshold",
				ValueQuery: `min(predict_linear(vm_free_disk_space_bytes[6h], 86400))`, Op: "<", Threshold: num(0), Unit: "bytes", ForSeconds: 1800,
				Readings: []wire.Reading{{Value: num(53687100000), MeasuredAt: rulesAt, State: wire.RuleInactive}}},
			{Name: "watchdog_throttled", Source: "motel check", Severity: "critical", Subject: "app", Kind: "threshold", Op: ">=", Unit: "restarts",
				Readings: []wire.Reading{
					{App: "blog", Value: num(4), Threshold: num(5), WindowSeconds: 600, MeasuredAt: rulesAt},
					{App: "demo", Value: num(1), Threshold: num(5), WindowSeconds: 600, MeasuredAt: rulesAt},
				}},
		},
	}
}

// noQuery fails a person's rules body that holds the store's socket, a
// query, or a guard's matching: the box's alone.
func noQuery(t *testing.T, what, body string) {
	t.Helper()
	for _, s := range []string{"vm.sock", "socket", "query", "motel_app_", "ALERTS", "predict_linear", "on (app)", `"match"`, "query_offset"} {
		if strings.Contains(body, s) {
			t.Errorf("%s: the body holds %q: %s", what, s, body)
		}
	}
}

// TestRulesRoute: GET /_motel/rules is a viewer of the box's alone. A
// viewer of one app, a person with no grant and a global deploy source
// are refused before the source is asked, and every other deploy
// token, a key no line lists and no token get the flat 401; a query or
// a path below it is the 404 of every row under /_motel/; the box's
// viewer gets every rule.
func TestRulesRoute(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeRules{l: wire.RulesListing{Alerts: []wire.AlertListing{}}}
	h.app.SetRulesSource(src)
	const viewerBox = `{"error":"this needs the viewer role on the box","needs":"viewer","scope":"box"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"box"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/rules")
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
		w := sendTo(t, h, "GET", "/_motel/rules", c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
		for _, target := range []string{"/_motel/rules?app=demo", "/_motel/rules?", "/_motel/rules/", "/_motel/rules/demo", "/_motel//rules", "/_motel/./rules", "/_motel/Rules", "/_motel/rules%00"} {
			w := sendTo(t, h, "GET", target, c.auth)
			if c.body == "" {
				sameAnswer(t, c.name+", "+target, w, flat)
			} else if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"not found"}`+"\n" {
				t.Errorf("%s, %s: %d %q, want the 404", c.name, target, w.Code, w.Body)
			}
		}
	}
	if w := sendTo(t, h, "POST", "/_motel/rules", token(people.carol)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/_motel/rules?app=demo", "/_motel/rules?", "/_motel/rules/", "/_motel/rules/demo", "/_motel//rules", "/_motel/./rules", "/_motel/Rules"} {
		if w := sendTo(t, h, "GET", target, token(people.carol)); w.Code != http.StatusNotFound {
			t.Errorf("the box's viewer, %s: %d %q, want the 404", target, w.Code, w.Body)
		}
	}
	if n := src.count(); n != 0 {
		t.Errorf("the refusals asked the source %d times, want none", n)
	}
	w := sendTo(t, h, "GET", "/_motel/rules", token(people.carol))
	if w.Code != http.StatusOK || w.Body.String() != `{"store":"off","alerts":[]}`+"\n" || src.count() != 1 {
		t.Errorf("the box's viewer: %d %s, the source asked %d times", w.Code, w.Body, src.count())
	}
	wantNoMotelHeader(t, w)
}

// TestAppRulesRoute: GET /_motel/apps/<app>/rules is a read of one app
// for a person whose grants cover it, refused as GET
// /_motel/apps/<app>/alerts is: the same 403 for an app no grant covers
// and a name that is none, the 404 for a viewer of the box naming none,
// the global deploy source's 403, the flat 401 for the rest; a query is
// the 422; every refusal before the source is asked.
func TestAppRulesRoute(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeRules{l: wire.RulesListing{Alerts: []wire.AlertListing{}}}
	h.app.SetRulesSource(src)
	const viewerApp = `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"app"}` + "\n"
	const noQuery = `{"error":"GET /_motel/apps/{app}/rules takes no query"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/apps/demo/rules")
	if flat.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", flat.Code, flat.Body)
	}
	for _, c := range []struct {
		name, target, auth string
		code               int
		body               string // "" for the flat 401
	}{
		{"a viewer of demo, on blog", "/_motel/apps/blog/rules", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, on a name that is no app", "/_motel/apps/ghost/rules", token(people.alice), http.StatusForbidden, viewerApp},
		{"a person with no grant", "/_motel/apps/demo/rules", token(people.dave), http.StatusForbidden, viewerApp},
		{"a person with no grant, on a name that is no app", "/_motel/apps/ghost/rules", token(people.dave), http.StatusForbidden, viewerApp},
		{"a viewer of the box, on a name that is no app", "/_motel/apps/ghost/rules", token(people.carol), http.StatusNotFound, `{"error":"unknown app \"ghost\""}` + "\n"},
		{"a global deploy source", "/_motel/apps/demo/rules", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"a global deploy source, on a name that is no app", "/_motel/apps/ghost/rules", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"demo's deploy source", "/_motel/apps/demo/rules", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
		{"a key no line lists", "/_motel/apps/demo/rules", token(people.stranger), http.StatusUnauthorized, ""},
		{"a viewer of demo, a query on blog", "/_motel/apps/blog/rules?app=demo", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, a query", "/_motel/apps/demo/rules?app=blog", token(people.alice), http.StatusUnprocessableEntity, noQuery},
		{"a viewer of demo, an empty query", "/_motel/apps/demo/rules?", token(people.alice), http.StatusUnprocessableEntity, noQuery},
		{"a viewer of demo, a query that does not parse", "/_motel/apps/demo/rules?%ZZ", token(people.alice), http.StatusUnprocessableEntity, noQuery},
		{"a viewer of the box, a query", "/_motel/apps/demo/rules?store=on", token(people.carol), http.StatusUnprocessableEntity, noQuery},
	} {
		w := sendTo(t, h, "GET", c.target, c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
	}
	sameAnswer(t, "an app no grant covers and a name that is no app",
		sendTo(t, h, "GET", "/_motel/apps/blog/rules", token(people.alice)), sendTo(t, h, "GET", "/_motel/apps/ghost/rules", token(people.alice)))
	for _, target := range []string{
		"/_motel/apps/demo/rules/", "/_motel/apps/demo//rules", "/_motel/apps//rules", "/_motel/apps/demo/./rules",
		"/_motel/apps/x/../demo/rules", "/_motel/apps/Demo/rules", "/_motel/apps/demo/Rules", "/_motel/apps/demo/rules%00",
		"/_motel/apps/demo/rules/x", "/_motel//apps/demo/rules",
	} {
		for _, k := range []string{token(people.carol), token(people.alice)} {
			if w := sendTo(t, h, "GET", target, k); w.Code != http.StatusNotFound || w.Body.String() != `{"error":"not found"}`+"\n" {
				t.Errorf("%s: %d %q, want the 404", target, w.Code, w.Body)
			}
		}
	}
	if w := sendTo(t, h, "POST", "/_motel/apps/demo/rules", token(people.alice)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	if n := src.count(); n != 0 {
		t.Errorf("the refusals asked the source %d times, want none", n)
	}
	for who, k := range map[string]string{"alice": token(people.alice), "carol": token(people.carol)} {
		if w := sendTo(t, h, "GET", "/_motel/apps/demo/rules", k); w.Code != http.StatusOK || w.Body.String() != `{"store":"off","alerts":[]}`+"\n" {
			t.Errorf("%s, demo: %d %s", who, w.Code, w.Body)
		}
	}
	if n := src.count(); n != 2 {
		t.Errorf("two reads asked the source %d times", n)
	}
}

// TestRulesBody: the box's viewer reads every rule, each with every
// number, and the rules file's word; nobody reads the store's socket,
// a query or a guard's matching; the store's word is on, unanswered
// (the box's own evaluation did not answer, or said a word that is not
// on), or off (no metrics line, or no source: nothing raised).
func TestRulesBody(t *testing.T) {
	h, people := alertsRig(t)
	carol := token(people.carol)
	read := func() (int, string) {
		w := sendTo(t, h, "GET", "/_motel/rules", carol)
		return w.Code, w.Body.String()
	}
	if code, body := read(); code != http.StatusOK || body != `{"store":"off","alerts":[]}`+"\n" {
		t.Errorf("no source: %d %s", code, body)
	}
	l := rulesListing()
	h.app.SetRulesSource(&fakeRules{l: l})
	code, body := read()
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	noQuery(t, "the box's rules", body)
	var got wire.Rules
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Store != wire.StoreOn || got.RulesFile != wire.RulesFileDiffers || len(got.Alerts) != len(l.Alerts) {
		t.Fatalf("%v: %s", err, body)
	}
	for i, a := range got.Alerts {
		want := l.Alerts[i]
		want.ValueQuery = ""
		for j := range want.Guards {
			if a.Guards[j].Op != want.Guards[j].Op || a.Guards[j].Meaning != want.Guards[j].Meaning || (want.Guards[j].Value == nil) != (a.Guards[j].Value == nil) {
				t.Errorf("%s's guard %d: %+v", a.Name, j, a.Guards[j])
			}
		}
		a.Guards, want.Guards = nil, nil
		w, _ := json.Marshal(want)
		g, _ := json.Marshal(a)
		if string(w) != string(g) {
			t.Errorf("%s:\n got %s\nwant %s", a.Name, g, w)
		}
	}
	// The source's listing is not changed by a read of it.
	if l.Alerts[1].ValueQuery == "" || l.Alerts[3].Guards[0].Query == "" {
		t.Error("the read emptied the source's queries")
	}
	for _, c := range []struct {
		store *wire.StoreQueries
		want  string
	}{
		{&wire.StoreQueries{Values: wire.StoreUnanswered}, wire.StoreUnanswered},
		{&wire.StoreQueries{Values: "later"}, wire.StoreUnanswered},
		{&wire.StoreQueries{}, wire.StoreUnanswered},
		{nil, wire.StoreOff},
	} {
		h.app.SetRulesSource(&fakeRules{l: wire.RulesListing{Store: c.store, Alerts: []wire.AlertListing{}}})
		if code, body := read(); code != http.StatusOK || body != `{"store":"`+c.want+`","alerts":[]}`+"\n" {
			t.Errorf("%+v: %d %s", c.store, code, body)
		}
	}
	// A listing the box could not make is said in fixed words, on both
	// routes.
	h.app.SetRulesSource(&fakeRules{err: errors.New("reading vmalert's unit: permission denied")})
	const failed = `{"error":"the box could not list its rules: moteld's journal says why"}` + "\n"
	for _, target := range []string{"/_motel/rules", "/_motel/apps/demo/rules"} {
		if w := sendTo(t, h, "GET", target, carol); w.Code != http.StatusInternalServerError || w.Body.String() != failed {
			t.Errorf("%s: %d %s", target, w.Code, w.Body)
		}
	}
}

// TestAppRulesBody: an app's viewer reads the rules whose subject is an
// app, every app's and their app's alone, each with their app's numbers
// and no other's: no box check (the disks, the backups, the store's
// disk), no rule pinned to another app, no other app's reading; and the
// rules file's word, which qualifies a state.
func TestAppRulesBody(t *testing.T) {
	h, people := alertsRig(t)
	h.app.SetRulesSource(&fakeRules{l: rulesListing()})
	w := sendTo(t, h, "GET", "/_motel/apps/demo/rules", token(people.alice))
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, body)
	}
	noQuery(t, "demo's rules", body)
	for _, s := range []string{"blog", "/srv", "disk", "backup", "ChannelFailed", "0.93104", "0.0123", "1.75", "53687100000", `"instance"`} {
		if strings.Contains(body, s) {
			t.Errorf("demo's rules hold %q: %s", s, body)
		}
	}
	var got wire.Rules
	// The rules file's word: a state is vmalert's, which reads the file.
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Store != wire.StoreOn || got.RulesFile != wire.RulesFileDiffers {
		t.Fatalf("%v: %s", err, body)
	}
	var names []string
	for _, a := range got.Alerts {
		names = append(names, a.Name)
		for _, r := range a.Readings {
			if r.App != "demo" {
				t.Errorf("%s: a reading of %q", a.Name, r.App)
			}
		}
	}
	if strings.Join(names, " ") != "app_memory_high demo_quiet deploy_failed errors_high watchdog_throttled" {
		t.Errorf("demo's rules: %v", names)
	}
	want := `{"name":"errors_high","source":"operator","severity":"warning","subject":"app","kind":"guarded","op":"\u003e","threshold":0.05,"unit":"share","for_seconds":600,` +
		`"guards":[{"op":"\u003e=","value":20,"meaning":"at least 20 responses sent over 5m: under it, too few to judge"}],` +
		`"readings":[{"app":"demo","value":0.0612346,"measured_at":"2026-11-13T12:00:00Z","state":"pending","active_since":"2026-11-13T11:56:00Z"}]}`
	if !strings.Contains(body, want) {
		t.Errorf("errors_high for demo:\n%s\nwant within it\n%s", body, want)
	}
	// An event pinned to apps demo is not among (post_deploy_regressed,
	// of blog's post_deploy lines alone) is not in demo's read.
	l := rulesListing()
	l.Alerts = append(l.Alerts, wire.AlertListing{Name: "post_deploy_regressed", Source: "operator", Severity: "warning", Subject: "app", Kind: "event", Apps: []string{"blog", "shop"}})
	h.app.SetRulesSource(&fakeRules{l: l})
	if w := sendTo(t, h, "GET", "/_motel/apps/demo/rules", token(people.alice)); strings.Contains(w.Body.String(), "post_deploy_regressed") || w.Code != http.StatusOK {
		t.Errorf("demo's rules hold an event pinned to other apps: %d %s", w.Code, w.Body)
	}
	if w := sendTo(t, h, "GET", "/_motel/rules", token(people.carol)); !strings.Contains(w.Body.String(), `{"name":"post_deploy_regressed","source":"operator","severity":"warning","subject":"app","apps":["blog","shop"],"kind":"event"}`) {
		t.Errorf("the box's rules: %d %s", w.Code, w.Body)
	}
	// blog's read holds it as blog's, and names no other app.
	if w := sendTo(t, h, "GET", "/_motel/apps/blog/rules", token(people.carol)); !strings.Contains(w.Body.String(), `{"name":"post_deploy_regressed","source":"operator","severity":"warning","subject":"app","app":"blog","kind":"event"}`) || strings.Contains(w.Body.String(), "shop") {
		t.Errorf("blog's rules: %d %s", w.Code, w.Body)
	}
	// blog's viewer (the box's, here) reads blog's.
	w = sendTo(t, h, "GET", "/_motel/apps/blog/rules", token(people.carol))
	if b := w.Body.String(); w.Code != http.StatusOK || strings.Contains(b, "demo") || !strings.Contains(b, "blog_slow") || !strings.Contains(b, "0.93104") {
		t.Errorf("blog's rules: %d %s", w.Code, b)
	}
}

// TestRulesPassNoAppValue: a rules body holds no byte of an app's, and
// no filter runs over it: an env_file value shaped like a date, a
// number, a word or a name in the body masks nothing in it, and the
// body reads whole, on both routes.
func TestRulesPassNoAppValue(t *testing.T) {
	h, people := alertsRig(t)
	must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte(
		"API_VERSION=2026-11-13\nRATIO=0.0612346\nLIMIT=53687100000\nSTATE=inactive\nLEVEL=critical\nKIND=threshold\nNAME=errors_high\nAT=12:00:00Z\n"), 0o600))
	h.app.SetRulesSource(&fakeRules{l: rulesListing()})
	for _, c := range []struct{ target, auth string }{
		{"/_motel/apps/demo/rules", token(people.alice)},
		{"/_motel/rules", token(people.carol)},
	} {
		w := sendTo(t, h, "GET", c.target, c.auth)
		body := w.Body.String()
		var got wire.Rules
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || strings.Contains(body, "redacted") || strings.Contains(body, "masked") {
			t.Fatalf("%s: %v: %d %s", c.target, err, w.Code, body)
		}
		for _, s := range []string{`"measured_at":"2026-11-13T12:00:00Z"`, `"value":0.0612346`, `"state":"inactive"`, `"severity":"critical"`, `"kind":"threshold"`, `"name":"errors_high"`} {
			if !strings.Contains(body, s) {
				t.Errorf("%s: the body lost %s: %s", c.target, s, body)
			}
		}
	}
	// Nor do the shape and entropy layers, which an older moteld ran
	// over it: a generated token in a rule's words (the operator's) is
	// sent as written.
	l := rulesListing()
	l.Alerts[3].Guards = []wire.GuardListing{{Op: ">=", Value: num(1), Meaning: "token q8Zr3XvLp2NwTb7KmYc4Hd9Fs6Jg"}} // gitleaks:allow — made up for this test
	h.app.SetRulesSource(&fakeRules{l: l})
	if w := sendTo(t, h, "GET", "/_motel/rules", token(people.carol)); !strings.Contains(w.Body.String(), "q8Zr3XvLp2NwTb7KmYc4Hd9Fs6Jg") || w.Code != http.StatusOK { // gitleaks:allow — made up for this test
		t.Errorf("a token in a rule's words: %d %s", w.Code, w.Body)
	}
}

// TestRulesChosenByTheHandle: an app's rules are chosen by the handle's
// app, the one allowApp made for the name Decide was asked about.
func TestRulesChosenByTheHandle(t *testing.T) {
	h, _ := alertsRig(t)
	src := &fakeRules{l: rulesListing()}
	for app, pinned := range map[string]string{"demo": "demo_quiet", "blog": "blog_slow"} {
		calls := recordDecide(t, nil)
		hd, err := h.allowApp(access.Principal{Kind: access.Person, Name: "carol", Views: &access.Views{Viewer: access.Scopes{Box: true}}}, access.AppRules, app)
		if err != nil || len(*calls) != 1 || (*calls)[0].resource != app {
			t.Fatalf("%s: %v %+v", app, err, *calls)
		}
		got, ok := hd.rules(context.Background(), src, h.logger)
		if !ok {
			t.Fatal("not listed")
		}
		var names []string
		for _, a := range got.Alerts {
			names = append(names, a.Name)
		}
		if !strings.Contains(strings.Join(names, " "), pinned) {
			t.Errorf("%s: %v, want its own %s", app, names, pinned)
		}
	}
}

// TestRulesSafeNames: an app's name the body holds (an alert's app, the
// apps a pinned alert names, a reading's app or backup) is sent as
// written, however generated it looks: no filter runs over the body.
func TestRulesSafeNames(t *testing.T) {
	h, people := alertsRig(t)
	name := "k3x9q2m7v8w1z4p6r5t0"
	h.app.SetRulesSource(&fakeRules{l: wire.RulesListing{Alerts: []wire.AlertListing{
		{Name: "post_deploy_regressed", Source: "operator", Severity: "warning", Subject: "app", Kind: "event", Apps: []string{"blog", name}},
	}}})
	if w := sendTo(t, h, "GET", "/_motel/rules", token(people.carol)); !strings.Contains(w.Body.String(), `"apps":["blog","`+name+`"]`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// TestRulesCheckNumbersWhole: a check's number as its detector read it,
// a long run of digits (a nearly empty filesystem's used percent, in
// either of Go's forms), is sent whole: no filter runs over the body,
// and it stays JSON.
func TestRulesCheckNumbersWhole(t *testing.T) {
	h, people := alertsRig(t)
	for _, v := range []float64{0.000123456789012345, 5.740305648246404e-05, 1.2345678901234567e-07} {
		h.app.SetRulesSource(&fakeRules{l: wire.RulesListing{Alerts: []wire.AlertListing{
			{Name: "disk_low", Source: "motel check", Severity: "warning", Subject: "box", Kind: "threshold", Op: ">=", Unit: "percent",
				Readings: []wire.Reading{{Instance: "/srv/motel", Value: num(v), MeasuredAt: rulesAt}}},
		}}})
		w := sendTo(t, h, "GET", "/_motel/rules", token(people.carol))
		var got wire.Rules
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Alerts) != 1 || *got.Alerts[0].Readings[0].Value != v {
			t.Errorf("%v: %v: %d %s", v, err, w.Code, w.Body)
		}
	}
}
