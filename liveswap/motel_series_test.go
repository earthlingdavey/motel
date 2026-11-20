package liveswap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// fakeSeries is a SeriesSource that answers each app by its name, and
// counts and records what it was asked.
type fakeSeries struct {
	mu    sync.Mutex
	byApp map[string]wire.Series
	// unanswered answers every app as the store not answering.
	unanswered bool
	asked      []string // app and window, "demo 24h"
}

func (f *fakeSeries) AppSeries(_ context.Context, app, window string) wire.Series {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, app+" "+window)
	if f.unanswered {
		return wire.Series{Store: wire.StoreUnanswered, Window: window}
	}
	s, ok := f.byApp[app]
	if !ok {
		return wire.Series{Store: wire.StoreOn, Window: window}
	}
	s.Window = window
	return s
}

func (f *fakeSeries) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// seriesEnd is the fixtures' window end.
var seriesEnd = time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)

// demoSeries is a day of demo's numbers: a memory, a CPU with a gap,
// and the request panels.
func demoSeries() wire.Series {
	end := seriesEnd
	t0 := float64(end.Unix())
	return wire.Series{Store: wire.StoreOn, Window: "24h", StepSeconds: 300, End: &end, Panels: []wire.Panel{
		{Name: wire.PanelMemory, Unit: wire.UnitBytes, Points: [][2]float64{{t0 - 600, 190840832}, {t0 - 300, 191889408}, {t0, 12345678901}}},
		{Name: wire.PanelCPU, Unit: wire.UnitCores, Points: [][2]float64{{t0 - 600, 0.004}, {t0, 0.000123}}},
		{Name: wire.PanelRequests, Unit: wire.UnitPerSecond, Points: [][2]float64{{t0 - 300, 2.5}, {t0, 3}}},
		{Name: wire.Panel5xx, Unit: wire.UnitShare, Points: [][2]float64{{t0 - 300, 0}, {t0, 0.0123}}},
		{Name: wire.PanelP95, Unit: wire.UnitMs, Points: [][2]float64{{t0, 80.1}}},
	}}
}

// TestSeriesRoute: GET /_motel/apps/<app>/series is a read of one app
// for a person whose grants cover it, every other caller refused before
// the source is asked, each as the alerts row refuses them (the same
// 403 for an app no grant covers and a name that is no app, the 404 to
// a viewer of the box alone), a query among them.
func TestSeriesRoute(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeSeries{}
	h.app.SetSeriesSource(src)
	const viewerApp = `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}` + "\n"
	const needsKey = `{"error":"this needs a token signed by a declared key","needs":"key","scope":"app"}` + "\n"
	flat := sendTo(t, h, "GET", "/_motel/apps/demo/series")
	if flat.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", flat.Code, flat.Body)
	}
	for _, c := range []struct {
		name, target, auth string
		code               int
		body               string // "" for the flat 401
	}{
		{"a viewer of demo, on blog", "/_motel/apps/blog/series", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, on a name that is no app", "/_motel/apps/ghost/series", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, a window on blog", "/_motel/apps/blog/series?range=14d", token(people.alice), http.StatusForbidden, viewerApp},
		{"a viewer of demo, a bad query on blog", "/_motel/apps/blog/series?range=2h", token(people.alice), http.StatusForbidden, viewerApp},
		{"a person with no grant", "/_motel/apps/demo/series", token(people.dave), http.StatusForbidden, viewerApp},
		{"a person with no grant, on a name that is no app", "/_motel/apps/ghost/series", token(people.dave), http.StatusForbidden, viewerApp},
		{"a viewer of the box, on a name that is no app", "/_motel/apps/ghost/series", token(people.carol), http.StatusNotFound, `{"error":"unknown app \"ghost\""}` + "\n"},
		{"a global deploy source", "/_motel/apps/demo/series", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"a global deploy source, on a name that is no app", "/_motel/apps/ghost/series?range=7d", "Bearer " + globalToken(t), http.StatusForbidden, needsKey},
		{"demo's deploy source", "/_motel/apps/demo/series", "Bearer " + appToken(t), http.StatusUnauthorized, ""},
		{"a key no line lists", "/_motel/apps/demo/series", token(people.stranger), http.StatusUnauthorized, ""},
	} {
		w := sendTo(t, h, "GET", c.target, c.auth)
		if c.body == "" {
			sameAnswer(t, c.name, w, flat)
		} else if w.Code != c.code || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.name, w.Code, w.Body, c.code, c.body)
		}
		wantNoMotelHeader(t, w)
	}
	// A name the grants do not cover is told alike whether it is an app,
	// with a window or without.
	for _, q := range []string{"", "?range=24h", "?range=nope"} {
		sameAnswer(t, "an app no grant covers and a name that is no app "+q,
			sendTo(t, h, "GET", "/_motel/apps/blog/series"+q, token(people.alice)), sendTo(t, h, "GET", "/_motel/apps/ghost/series"+q, token(people.alice)))
	}
	if n := src.calls(); len(n) != 0 {
		t.Errorf("the refusals asked the source %v, want nothing", n)
	}
	if w := sendTo(t, h, "POST", "/_motel/apps/demo/series", token(people.alice)); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: %d %s", w.Code, w.Body)
	}
	for _, k := range []string{token(people.alice), token(people.carol)} {
		if w := sendTo(t, h, "GET", "/_motel/apps/demo/series", k); w.Code != http.StatusOK || w.Body.String() != `{"store":"on","window":"1h","panels":[]}`+"\n" {
			t.Errorf("demo: %d %s", w.Code, w.Body)
		}
	}
	if got := src.calls(); !reflect.DeepEqual(got, []string{"demo 1h", "demo 1h"}) {
		t.Errorf("two reads asked the source %v", got)
	}
}

// TestSeriesRouteQuery: the series row takes one key, range, once, with
// a value of wire.SeriesRanges, the hour when it is not named. Anything
// else is the 422 that says what the row takes, in its own words alone
// (the value sent is never in it, nor anything of the app's), and the
// source is not asked.
func TestSeriesRouteQuery(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeSeries{}
	h.app.SetSeriesSource(src)
	alice := token(people.alice)
	for q, window := range map[string]string{"": "1h", "?": "1h", "?range=1h": "1h", "?range=24h": "24h", "?range=7d": "7d", "?range=14d": "14d", "?range=7d&": "7d", "?&range=14d": "14d"} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series"+q, alice)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"window":"`+window+`"`) {
			t.Errorf("%q: %d %s, want the %s", q, w.Code, w.Body, window)
		}
	}
	if n := len(src.calls()); n != 8 {
		t.Fatalf("eight reads asked the source %d times", n)
	}
	const body = `{"error":"GET /_motel/apps/{app}/series takes only range (one of 1h, 24h, 7d, 14d), each once"}` + "\n"
	for _, q := range []string{
		"?range=", "?range", "?range=2h", "?range=1H", "?range=24h%20", "?range=+24h", "?range=24h&range=24h", "?range=1h&range=24h",
		"?Range=1h", "?step=60", "?range=24h&step=60", "?query=up", "?app=blog", "?range=24h&app=blog", "?panel=memory_bytes",
		"?%ZZ", "?range=%ZZ", "?a;b", "?range=1h;x", "?range=hunter2-of-demo", "?hunter2-of-demo", "?range=24h&hunter2-of-demo=1",
		"?lines=50", "?range=" + strings.Repeat("1", 4096),
	} {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series"+q, alice)
		if w.Code != http.StatusUnprocessableEntity || w.Body.String() != body {
			t.Errorf("%.40s: %d %q, want 422 %q", q, w.Code, w.Body, body)
		}
	}
	if n := len(src.calls()); n != 8 {
		t.Errorf("a refused query asked the source (%d calls, want 8)", n)
	}
	// The alerts row takes none still, an empty query among them.
	if w := sendTo(t, h, "GET", "/_motel/apps/demo/alerts?range=24h", alice); w.Code != http.StatusUnprocessableEntity ||
		w.Body.String() != `{"error":"GET /_motel/apps/{app}/alerts takes no query"}`+"\n" {
		t.Errorf("alerts with a range: %d %s", w.Code, w.Body)
	}
}

// TestSeriesRoutePaths: only /_motel/apps/<app>/series exactly is the
// row, taken as sent; anything else is the 404 every path with no row
// gets, and the source is not asked.
func TestSeriesRoutePaths(t *testing.T) {
	h, people := alertsRig(t)
	src := &fakeSeries{}
	h.app.SetSeriesSource(src)
	notFound := `{"error":"not found"}` + "\n"
	for _, target := range []string{
		"/_motel/apps/demo/series/", "/_motel/apps/demo/series/24h", "/_motel/apps/demo//series", "/_motel/apps//series",
		"/_motel/apps/demo/./series", "/_motel/apps/x/../demo/series", "/_motel/apps/Demo/series", "/_motel/apps/demo/Series",
		"/_motel/apps/demo/series%00", "/_motel/apps/demo%20/series", "/_motel/apps/demo/series.json", "/_motel/apps/demo/metrics",
		"/_motel/series", "/_motel/apps/series", "/_motel/apps/" + strings.Repeat("a", appNameMaxLen+1) + "/series",
	} {
		for _, k := range []string{token(people.carol), token(people.alice)} {
			if w := sendTo(t, h, "GET", target+"?range=24h", k); w.Code != http.StatusNotFound || w.Body.String() != notFound {
				t.Errorf("%s: %d %q, want the 404", target, w.Code, w.Body)
			}
		}
	}
	if n := src.calls(); len(n) != 0 {
		t.Errorf("a path with no row asked the source %v", n)
	}
}

// TestSeriesRouteBody: the source's answer for the handle's app and the
// window asked, as the wire type writes it; "off" with no source, and
// the source's "unanswered", each with no panel.
func TestSeriesRouteBody(t *testing.T) {
	h, people := alertsRig(t)
	alice := token(people.alice)
	read := func(q string) (int, string) {
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series"+q, alice)
		return w.Code, w.Body.String()
	}
	if code, body := read("?range=7d"); code != http.StatusOK || body != `{"store":"off","window":"7d","panels":[]}`+"\n" {
		t.Errorf("no source: %d %s", code, body)
	}
	h.app.SetSeriesSource(&fakeSeries{unanswered: true})
	if code, body := read("?range=14d"); code != http.StatusOK || body != `{"store":"unanswered","window":"14d","panels":[]}`+"\n" {
		t.Errorf("unanswered: %d %s", code, body)
	}
	h.app.SetSeriesSource(&fakeSeries{byApp: map[string]wire.Series{"demo": demoSeries()}})
	code, body := read("?range=24h")
	const want = `{"store":"on","window":"24h","step_seconds":300,"end":"2026-11-19T14:00:00Z","panels":[` +
		`{"name":"memory_bytes","unit":"bytes","points":[[1795096200,190840832],[1795096500,191889408],[1795096800,12345678901]]},` +
		`{"name":"cpu_cores","unit":"cores","points":[[1795096200,0.004],[1795096800,0.000123]]},` +
		`{"name":"requests_per_second","unit":"per_second","points":[[1795096500,2.5],[1795096800,3]]},` +
		`{"name":"5xx_share","unit":"share","points":[[1795096500,0],[1795096800,0.0123]]},` +
		`{"name":"p95_ms","unit":"ms","points":[[1795096800,80.1]]}]}` + "\n"
	if code != http.StatusOK || body != want {
		t.Errorf("demo's day:\n got %d %s\nwant %s", code, body, want)
	}
	var got wire.Series
	if err := json.Unmarshal([]byte(body), &got); err != nil || !reflect.DeepEqual(got, demoSeries()) {
		t.Errorf("decoded: %v %+v", err, got)
	}
}

// TestSeriesChosenByTheHandle: the source is asked for the handle's
// app, the one allowApp made for the name Decide was asked about.
func TestSeriesChosenByTheHandle(t *testing.T) {
	h, _ := alertsRig(t)
	src := &fakeSeries{}
	h.app.SetSeriesSource(src)
	for _, app := range []string{"demo", "blog"} {
		calls := recordDecide(t, nil)
		carol := access.Principal{Kind: access.Person, Name: "carol", Views: &access.Views{Viewer: access.Scopes{Box: true}}}
		hd, err := h.allowApp(carol, access.AppSeries, app)
		if err != nil || len(*calls) != 1 || (*calls)[0].resource != app || (*calls)[0].a != access.AppSeries {
			t.Fatalf("%s: %v %+v", app, err, *calls)
		}
		w := httptest.NewRecorder()
		if err := h.appSeries(w, httptest.NewRequest(http.MethodGet, "/", nil), hd, appQuery{"range": "7d"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := src.calls(); !reflect.DeepEqual(got, []string{"demo 7d", "blog 7d"}) {
		t.Errorf("the source was asked %v", got)
	}
}

// TestSeriesNumbersPassNoFilter: an env_file value shaped like a
// number or a time of the body (a point's unix time, a run of digits
// inside a memory value, a date, the end's hour, a short number) leaves
// every point, the step and the end as the source gave them, and the
// read whole. The points and the step are numbers, which rule 7 of
// redact.go holds in any body but in its fallbacks (a value across a
// number's edge, or one replaced elsewhere in the body), and
// filterSeries holds even then; the end is a time under a key rule 7
// does not hold, so a date value or a piece of one would be replaced
// inside it but for filterSeries.
func TestSeriesNumbersPassNoFilter(t *testing.T) {
	for _, value := range []string{"1795096800", "19084083", "12345678901", "2026-11-19", "2026-11-19T14:00:00Z", "T14:00:00", "0.000123", "42", "300", "191889408,"} {
		h, people := alertsRig(t)
		must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("NUMBERISH="+value+"\n"), 0o600))
		h.app.SetSeriesSource(&fakeSeries{byApp: map[string]wire.Series{"demo": demoSeries()}})
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series?range=24h", token(people.alice))
		var got wire.Series
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || !reflect.DeepEqual(got, demoSeries()) {
			t.Errorf("NUMBERISH=%s: %v: %d %s", value, err, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "redacted") {
			t.Errorf("NUMBERISH=%s: the filter found the value in the body's words: %s", value, w.Body)
		}
	}
}

// TestSeriesWordsPassTheFilter: the body's words do pass the app's
// filter: a value equal to a panel's name is masked there, the body
// names the key, and the points stay; a body the filter withholds (its
// env_file unreadable) is the filter's sentence; and a value spanning
// the words' punctuation changes their structure, so the body goes
// whole through the filter, the value not in the bytes sent. (One made
// of the punctuation and a number's digits, which only the whole body
// holds, can stand in it: the accepted row in DESIGN-access.md.)
func TestSeriesWordsPassTheFilter(t *testing.T) {
	h, people := alertsRig(t)
	ma := h.app.managed["demo"]
	must(t, os.WriteFile(ma.spec.envFile, []byte("PANEL=requests_per_second\n"), 0o600))
	h.app.SetSeriesSource(&fakeSeries{byApp: map[string]wire.Series{"demo": demoSeries()}})
	w := sendTo(t, h, "GET", "/_motel/apps/demo/series?range=24h", token(people.alice))
	var got struct {
		wire.Series
		RedactedEnv []string `json:"redacted_env"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Panels) != 5 || got.Panels[2].Name != "[redacted:PANEL]" ||
		!reflect.DeepEqual(got.Panels[2].Points, demoSeries().Panels[2].Points) || !reflect.DeepEqual(got.RedactedEnv, []string{"PANEL"}) ||
		got.End == nil || !got.End.Equal(seriesEnd) || got.StepSeconds != 300 || got.Store != wire.StoreOn {
		t.Errorf("a panel's name as a value: %v: %s", err, w.Body)
	}
	if strings.Contains(w.Body.String(), "requests_per_second") {
		t.Errorf("the value is in the body: %s", w.Body)
	}

	// The store's word and the window are the box's own: a value equal
	// to one leaves them as they are, and the answer read.
	for _, c := range []struct{ value, store string }{{"unanswered", wire.StoreUnanswered}, {"unanswered", wire.StoreOn}} {
		h, people := alertsRig(t)
		must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("WORD="+c.value+"\n"), 0o600))
		src := &fakeSeries{byApp: map[string]wire.Series{"demo": demoSeries()}, unanswered: c.store == wire.StoreUnanswered}
		h.app.SetSeriesSource(src)
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series?range=24h", token(people.alice))
		var got wire.Series
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Store != c.store || got.Window != "24h" || strings.Contains(w.Body.String(), "redacted") {
			t.Errorf("WORD=%s, the store %s: %v %s", c.value, c.store, err, w.Body)
		}
	}

	must(t, os.Remove(ma.spec.envFile))
	ma.secretsLoaded = false
	w = sendTo(t, h, "GET", "/_motel/apps/demo/series?range=24h", token(people.alice))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), `{"error":"response withheld`) {
		t.Errorf("withheld: %d %s", w.Code, w.Body)
	}

	// The first stands in the words alone (the points come between them
	// in the body), the second in both: neither is in the bytes sent,
	// whatever the filter then did with the whole body.
	for _, value := range []string{`bytes"},{"name":"cpu_cores`, `"name":"cpu_cores","unit":"cores`} {
		h, people := alertsRig(t)
		must(t, os.WriteFile(h.app.managed["demo"].spec.envFile, []byte("K="+value+"\n"), 0o600))
		h.app.SetSeriesSource(&fakeSeries{byApp: map[string]wire.Series{"demo": demoSeries()}})
		w := sendTo(t, h, "GET", "/_motel/apps/demo/series?range=24h", token(people.alice))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), value) || !json.Valid(w.Body.Bytes()) {
			t.Errorf("K=%s: %d %s", value, w.Code, w.Body)
		}
	}
}
