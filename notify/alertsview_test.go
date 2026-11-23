package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// heldAnswer is GET /api/v2/alerts as Debian's Alertmanager 0.28.1
// answers it with no parameter (the shape measured in the e2e image,
// trimmed of the fields not read): an active alert, one silenced by two
// silences, one by a silence the list no longer holds, one the deploy
// mute inhibits, a box event, and the five alerts the reads leave out.
const heldAnswer = `[
 {"labels":{"alertname":"errors_high","app":"blog","subject":"app:blog","severity":"warning"},"annotations":{"summary":"blog: errors"},"startsAt":"2026-11-19T14:00:00.123+01:00","endsAt":"2026-11-19T15:00:00Z","fingerprint":"a1","receivers":[{"name":"state-warning-ops"}],"updatedAt":"2026-11-19T14:00:01Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"deploy_failed","app":"blog","severity":"warning","release":"v2"},"annotations":{"summary":"deploy of v2 failed"},"startsAt":"2026-11-19T13:01:00Z","status":{"state":"suppressed","silencedBy":["11111111-1111-4111-8111-111111111111","22222222-2222-4222-8222-222222222222"],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"rollback","app":"shop","severity":"warning"},"annotations":{"summary":"shop rolled back"},"startsAt":"2026-11-19T13:02:00Z","status":{"state":"suppressed","silencedBy":["99999999-9999-4999-8999-999999999999"],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"watchdog_throttled","app":"blog","severity":"critical"},"annotations":{"summary":"blog: throttled"},"startsAt":"2026-11-19T13:03:00Z","status":{"state":"suppressed","silencedBy":[],"inhibitedBy":["10dfa743d6cec7a1"],"mutedBy":[]}},
 {"labels":{"alertname":"backup_stale","subject":"box","instance":"backup_stale/blog","severity":"critical"},"annotations":{"summary":"the backup of blog is stale"},"startsAt":"2026-11-19T13:04:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"Watchdog"},"startsAt":"2026-11-19T13:05:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"NotifyTest","notify_channel":"ops"},"startsAt":"2026-11-19T13:06:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"deploy_in_progress","app":"blog"},"startsAt":"2026-11-19T13:07:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"person_new_network","subject":"box","source":"access","instance":"SHA256:example","severity":"warning"},"annotations":{"summary":"bob@example.com (key SHA256:example) used this box from 203.0.113.0/24 for the first time"},"startsAt":"2026-11-19T13:08:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}},
 {"labels":{"alertname":"person_outside_list","subject":"box","source":"access","instance":"SHA256:example","severity":"warning"},"annotations":{"summary":"a valid signature by bob@example.com (key SHA256:example) from 203.0.113.9, outside every address list on their grants; refused"},"startsAt":"2026-11-19T13:09:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[],"mutedBy":[]}}
]`

const silencesAnswer = `[
 {"id":"11111111-1111-4111-8111-111111111111","status":{"state":"active"},"matchers":[],"startsAt":"2026-11-19T13:00:00Z","endsAt":"2026-11-19T15:00:00Z","createdBy":"moteld monitor: root","comment":"known"},
 {"id":"22222222-2222-4222-8222-222222222222","status":{"state":"active"},"matchers":[],"startsAt":"2026-11-19T13:00:00Z","endsAt":"2026-11-19T16:30:00+01:00","createdBy":"moteld monitor: root","comment":"later"},
 {"id":"33333333-3333-4333-8333-333333333333","status":{"state":"expired"},"matchers":[],"startsAt":"2026-11-18T13:00:00Z","endsAt":"2026-11-18T14:00:00Z","createdBy":"x","comment":"old"},
 {"id":"44444444-4444-4444-8444-444444444444","status":{"state":"active"},"matchers":[{"name":"alertname","value":"person_new_network","isRegex":false,"isEqual":true},{"name":"instance","value":"SHA256:example","isRegex":false}],"startsAt":"2026-11-19T13:00:00Z","endsAt":"2026-11-19T15:00:00Z","createdBy":"moteld monitor: root","comment":"bob travelling"},
 {"id":"55555555-5555-4555-8555-555555555555","status":{"state":"pending"},"matchers":[{"name":"alertname","value":"person_outside_list"}],"startsAt":"2026-11-20T13:00:00Z","endsAt":"2026-11-20T15:00:00Z","createdBy":"moteld monitor: root","comment":"adding the team"}
]`

// fakeAMReads is Alertmanager's two read routes on a unix socket, counting
// what is asked.
type fakeAMReads struct {
	mu                  sync.Mutex
	alerts, silences    string
	alertsCode, silCode int
	asked               map[string]int
}

func (f *fakeAMReads) serve(t *testing.T) string {
	f.asked = map[string]int{}
	return unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked[r.Method+" "+r.URL.RequestURI()]++
		body, code := "", http.StatusNotFound
		switch r.URL.Path {
		case "/api/v2/alerts":
			body, code = f.alerts, f.alertsCode
		case "/api/v2/silences":
			body, code = f.silences, f.silCode
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
}

func (f *fakeAMReads) count(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[what]
}

// TestHeldAlerts: what Alertmanager holds, as the reads take it. Every
// alert but the heartbeat, the test alert and the deploy mute, with its
// app label (none for a box event), its name, severity, summary, start
// in UTC and Alertmanager's state; when the last of its silences ends,
// of those the silence list holds; and the mute's hold. Both reads are
// GETs with no parameter: no matcher is sent.
func TestHeldAlerts(t *testing.T) {
	am := &fakeAMReads{alerts: heldAnswer, silences: silencesAnswer, alertsCode: 200, silCode: 200}
	c := newAMClient(am.serve(t))
	held, err := c.heldAlerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v.UTC()
	}
	until := at("2026-11-19T15:30:00Z")
	want := []liveswap.AppAlert{
		{App: "blog", Alert: wire.Alert{Name: "errors_high", Severity: "warning", Summary: "blog: errors", StartsAt: at("2026-11-19T13:00:00.123Z"), State: "active"}},
		{App: "blog", Alert: wire.Alert{Name: "deploy_failed", Severity: "warning", Summary: "deploy of v2 failed", StartsAt: at("2026-11-19T13:01:00Z"), State: "suppressed", SilencedUntil: &until}},
		{App: "shop", Alert: wire.Alert{Name: "rollback", Severity: "warning", Summary: "shop rolled back", StartsAt: at("2026-11-19T13:02:00Z"), State: "suppressed"}},
		{App: "blog", Alert: wire.Alert{Name: "watchdog_throttled", Severity: "critical", Summary: "blog: throttled", StartsAt: at("2026-11-19T13:03:00Z"), State: "suppressed", Inhibited: true}},
		{App: "", Alert: wire.Alert{Name: "backup_stale", Severity: "critical", Summary: "the backup of blog is stale", StartsAt: at("2026-11-19T13:04:00Z"), State: "active"}},
	}
	if len(held.Alerts) != len(want) {
		t.Fatalf("%d alerts, want %d: %+v", len(held.Alerts), len(want), held.Alerts)
	}
	for i, w := range want {
		g := held.Alerts[i]
		if g.App != w.App || g.Name != w.Name || g.Severity != w.Severity || g.Summary != w.Summary || !g.StartsAt.Equal(w.StartsAt) ||
			g.StartsAt.Location() != time.UTC || g.State != w.State || g.Inhibited != w.Inhibited ||
			(g.SilencedUntil == nil) != (w.SilencedUntil == nil) || g.SilencedUntil != nil && (!g.SilencedUntil.Equal(*w.SilencedUntil) || g.SilencedUntil.Location() != time.UTC) {
			t.Errorf("alert %d: %+v, want %+v", i, g, w)
		}
	}
	if am.count("GET /api/v2/alerts") != 1 || am.count("GET /api/v2/silences") != 1 || len(am.asked) != 2 {
		t.Errorf("asked %v, want each read once, with no parameter", am.asked)
	}

	// No alert names a silence: the list is asked for all the same, for
	// the box-wide read's silences.
	am.alerts = `[{"labels":{"alertname":"errors_high","app":"blog"},"startsAt":"2026-11-19T13:00:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[]}}]`
	if held, err := c.heldAlerts(context.Background()); err != nil || len(held.Alerts) != 1 || len(held.Silences) != 2 || am.count("GET /api/v2/silences") != 2 {
		t.Errorf("no silence named: %v %+v, asked %v", err, held, am.asked)
	}
	am.alerts = `[]`
	if held, err := c.heldAlerts(context.Background()); err != nil || held.Alerts == nil || len(held.Alerts) != 0 {
		t.Errorf("none held: %v %+v", err, held)
	}
}

// TestHeldSilences: every silence Alertmanager holds that has not
// expired, as the box-wide read takes it: its id, state, matchers (the
// operator spelled from isRegex and isEqual, a missing isEqual taken as
// true), times in UTC, maker and comment; and each alert's instance
// label. The answer is the shape Debian's 0.28.1 gives (measured: an
// expired silence stays in the list, "expired", until its retention
// ends).
func TestHeldSilences(t *testing.T) {
	am := &fakeAMReads{alertsCode: 200, silCode: 200,
		alerts: `[{"labels":{"alertname":"disk_low","subject":"box","instance":"/var/lib/liveswap","severity":"warning"},"annotations":{"summary":"84% full"},"startsAt":"2026-11-19T13:00:00Z","status":{"state":"suppressed","silencedBy":["11111111-1111-4111-8111-111111111111"],"inhibitedBy":[],"mutedBy":[]}}]`,
		silences: `[
 {"id":"11111111-1111-4111-8111-111111111111","status":{"state":"active"},"updatedAt":"2026-11-19T13:00:00Z","matchers":[{"name":"alertname","value":"disk_low","isRegex":false,"isEqual":true},{"name":"app","value":"blog","isRegex":false,"isEqual":false}],"startsAt":"2026-11-19T13:00:00+01:00","endsAt":"2026-11-19T15:00:00Z","createdBy":"moteld monitor: root","comment":"resizing"},
 {"id":"22222222-2222-4222-8222-222222222222","status":{"state":"pending"},"matchers":[{"name":"alertname","value":"cert_.*","isRegex":true,"isEqual":true},{"name":"instance","value":"b\\.example","isRegex":true,"isEqual":false},{"name":"app","value":"x","isRegex":false}],"startsAt":"2026-11-20T13:00:00Z","endsAt":"2026-11-20T15:00:00Z","createdBy":"root by hand","comment":"later"},
 {"id":"33333333-3333-4333-8333-333333333333","status":{"state":"expired"},"matchers":[],"startsAt":"2026-11-18T13:00:00Z","endsAt":"2026-11-18T14:00:00Z","createdBy":"x","comment":"old"}
]`}
	held, err := newAMClient(am.serve(t)).heldAlerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(held.Alerts) != 1 || held.Alerts[0].Instance != "/var/lib/liveswap" || held.Alerts[0].App != "" || held.Alerts[0].SilencedUntil == nil {
		t.Errorf("the alert: %+v", held.Alerts)
	}
	got, err := json.Marshal(held.Silences)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"id":"11111111-1111-4111-8111-111111111111","state":"active","matchers":[{"name":"alertname","op":"=","value":"disk_low"},{"name":"app","op":"!=","value":"blog"}],"starts_at":"2026-11-19T12:00:00Z","ends_at":"2026-11-19T15:00:00Z","created_by":"moteld monitor: root","comment":"resizing"},` +
		`{"id":"22222222-2222-4222-8222-222222222222","state":"pending","matchers":[{"name":"alertname","op":"=~","value":"cert_.*"},{"name":"instance","op":"!~","value":"b\\.example"},{"name":"app","op":"=","value":"x"}],"starts_at":"2026-11-20T13:00:00Z","ends_at":"2026-11-20T15:00:00Z","created_by":"root by hand","comment":"later"}]`
	if string(got) != want {
		t.Errorf("the silences:\n got %s\nwant %s", got, want)
	}
	// None held: an empty list, not none.
	am.silences = `[]`
	if held, err := newAMClient(am.serve(t)).heldAlerts(context.Background()); err != nil || held.Silences == nil || len(held.Silences) != 0 {
		t.Errorf("no silence: %v %+v", err, held.Silences)
	}
}

// TestHeldAlertsFailures: any answer that cannot be read whole is a
// failure, never a part: an alerts read refused or not JSON, and a
// silence list refused or not JSON, whether an alert names a silence or
// none does (the list is the box-wide read's too).
func TestHeldAlertsFailures(t *testing.T) {
	for _, c := range []struct {
		name                string
		alerts, silences    string
		alertsCode, silCode int
	}{
		{"the alerts refused", heldAnswer, silencesAnswer, 500, 200},
		{"the alerts not JSON", `[{"labels":`, silencesAnswer, 200, 200},
		{"the silences refused", heldAnswer, silencesAnswer, 200, 503},
		{"the silences not JSON", heldAnswer, `{`, 200, 200},
		{"the silences refused, no alert naming one", `[]`, silencesAnswer, 200, 503},
		{"the silences not JSON, no alert naming one", `[]`, `{`, 200, 200},
	} {
		am := &fakeAMReads{alerts: c.alerts, silences: c.silences, alertsCode: c.alertsCode, silCode: c.silCode}
		held, err := newAMClient(am.serve(t)).heldAlerts(context.Background())
		if err == nil || held.Alerts != nil {
			t.Errorf("%s: %v %+v, want a failure and nothing held", c.name, err, held)
		}
	}
	if _, err := newAMClient(t.TempDir() + "/none.sock").heldAlerts(context.Background()); err == nil {
		t.Error("no socket: no failure")
	}
}

// TestHeldAlertsOverTheLimit (#638): an answer of Alertmanager's longer
// than maxAMBody fails the fetch as over the limit, naming the route
// and the limit, not as the JSON a cut body would be; and the journal
// line says Alertmanager holds more than motel reads, and where to look.
// An answer of maxAMBody bytes exactly is read whole.
func TestHeldAlertsOverTheLimit(t *testing.T) {
	// heldAnswer padded with spaces to n bytes: still JSON.
	padded := func(answer string, n int) string { return answer + strings.Repeat(" ", n-len(answer)) }
	am := &fakeAMReads{alerts: padded(heldAnswer, maxAMBody), silences: padded(silencesAnswer, maxAMBody), alertsCode: 200, silCode: 200}
	c := newAMClient(am.serve(t))
	if held, err := c.heldAlerts(context.Background()); err != nil || len(held.Alerts) == 0 || len(held.Silences) == 0 {
		t.Fatalf("at the limit: %v, %d alerts, %d silences", err, len(held.Alerts), len(held.Silences))
	}

	for _, over := range []struct{ path, what string }{{"/api/v2/alerts", "alerts"}, {"/api/v2/silences", "silences"}} {
		am.mu.Lock()
		am.alerts, am.silences = padded(heldAnswer, maxAMBody), padded(silencesAnswer, maxAMBody)
		if over.what == "alerts" {
			am.alerts = padded(heldAnswer, maxAMBody+1)
		} else {
			am.silences = padded(silencesAnswer, maxAMBody+1)
		}
		am.mu.Unlock()
		_, err := c.heldAlerts(context.Background())
		var e *amOverLimitError
		if !errors.As(err, &e) || e.path != over.path || !strings.Contains(err.Error(), "its answer to "+over.path+" is over 4194304 bytes, the most motel reads") {
			t.Errorf("%s over the limit: %v", over.what, err)
		}

		core, logs := observer.New(zap.WarnLevel)
		v := &alertsView{now: time.Now, fetch: c.heldAlerts}
		if _, ok := v.get(context.Background(), zap.New(core)); ok {
			t.Errorf("%s over the limit: the read was answered", over.what)
		}
		all := logs.All()
		if len(all) != 1 || !strings.Contains(all[0].Message, "Alertmanager holds more "+over.what+" than motel reads (4 MiB of its answer)") ||
			!strings.Contains(all[0].Message, `docs/monitor.md, "More alerts than motel reads"`) || strings.Contains(all[0].Message, "did not answer") {
			t.Errorf("%s over the limit: journal %+v", over.what, all)
		}
	}
}

// TestAlertsView: one fetch per alertsEvery for every caller, a failure
// kept as long, a clock set back fetching again; one fetch at a time,
// the others waiting on it; a waiter that gives up told it did not
// answer while the fetch goes on; and the fetch bounded by alertsBudget
// whatever the caller's context, never ended by it.
func TestAlertsView(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	fetches, fail := 0, false
	var deadline time.Duration
	var cancelled bool
	v := &alertsView{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }}
	v.fetch = func(ctx context.Context) (liveswap.HeldAlerts, error) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		d, _ := ctx.Deadline()
		deadline = time.Until(d)
		cancelled = ctx.Err() != nil
		if fail {
			return liveswap.HeldAlerts{Alerts: []liveswap.AppAlert{{App: "partial"}}}, errors.New("alertmanager at /run/x: refused")
		}
		return liveswap.HeldAlerts{Alerts: []liveswap.AppAlert{{App: "blog", Alert: wire.Alert{Name: "errors_high"}}}}, nil
	}
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	count := func() int { mu.Lock(); defer mu.Unlock(); return fetches }
	step := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if held, ok := v.get(ended, logger); !ok || len(held.Alerts) != 1 || count() != 1 || cancelled || deadline > alertsBudget || deadline < alertsBudget-time.Second {
		t.Fatalf("first: %v %+v, %d fetches, cancelled %v, deadline in %v", ok, held, count(), cancelled, deadline)
	}
	step(alertsEvery - time.Millisecond)
	if _, ok := v.get(context.Background(), logger); !ok || count() != 1 {
		t.Errorf("within alertsEvery: %d fetches", count())
	}
	step(time.Millisecond)
	mu.Lock()
	fail = true
	mu.Unlock()
	if held, ok := v.get(context.Background(), logger); ok || held.Alerts != nil || count() != 2 {
		t.Errorf("a failure: %v %+v, %d fetches", ok, held, count())
	}
	if n := len(logs.FilterMessageSnippet("motel could not read what Alertmanager holds").All()); n != 1 {
		t.Errorf("%d journal lines for the failure, want 1", n)
	}
	if _, ok := v.get(context.Background(), logger); ok || count() != 2 {
		t.Errorf("a failure is kept: %d fetches", count())
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	step(-time.Hour)
	if _, ok := v.get(context.Background(), logger); !ok || count() != 3 {
		t.Errorf("after the clock was set back: %v, %d fetches", ok, count())
	}

	// One fetch at a time: the others wait for it, and one that gives up
	// first is told no answer while the fetch fills the entry.
	step(alertsEvery)
	release := make(chan struct{})
	inFetch := make(chan struct{})
	v.fetch = func(context.Context) (liveswap.HeldAlerts, error) {
		mu.Lock()
		fetches++
		mu.Unlock()
		close(inFetch)
		<-release
		return liveswap.HeldAlerts{Alerts: []liveswap.AppAlert{}}, nil
	}
	first := make(chan bool, 1)
	go func() { _, ok := v.get(context.Background(), logger); first <- ok }()
	<-inFetch
	var wg sync.WaitGroup
	oks := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, ok := v.get(context.Background(), logger); oks <- ok }()
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, ok := v.get(short, logger); ok {
		t.Error("a waiter that gave up was told an answer")
	}
	close(release)
	wg.Wait()
	close(oks)
	for ok := range oks {
		if !ok {
			t.Error("a waiter was not given the fetch's answer")
		}
	}
	if !<-first || count() != 4 {
		t.Errorf("one fetch at a time: %d fetches, want 4", count())
	}
}

// TestAlertsViewReusesItsConnection: the fetches over the socket share
// one client, so one connection serves them, not one left open each.
func TestAlertsViewReusesItsConnection(t *testing.T) {
	am := &fakeAMReads{alerts: `[]`, silences: `[]`, alertsCode: 200, silCode: 200}
	var mu sync.Mutex
	conns := 0
	sock := filepath.Join(t.TempDir(), "am.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	am.asked = map[string]int{}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(am.alerts))
	}), ConnState: func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	useSocket(t, sock)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	v := &alertsView{now: func() time.Time { return now }}
	for range 4 {
		if _, ok := v.get(context.Background(), zap.NewNop()); !ok {
			t.Fatal("not answered")
		}
		now = now.Add(alertsEvery)
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("four fetches opened %d connections, want 1", conns)
	}
}

// TestAlertsViewAfterAPanic: a fetch that panics still ends its entry,
// so a request that waited on it is told no answer rather than held.
func TestAlertsViewAfterAPanic(t *testing.T) {
	inFetch, release := make(chan struct{}), make(chan struct{})
	v := &alertsView{now: time.Now, fetch: func(context.Context) (liveswap.HeldAlerts, error) {
		close(inFetch)
		<-release
		panic("a fetch that panics")
	}}
	go func() {
		defer func() { _ = recover() }()
		v.get(context.Background(), zap.NewNop())
	}()
	<-inFetch
	got := make(chan bool, 1)
	go func() { _, ok := v.get(context.Background(), zap.NewNop()); got <- ok }()
	time.Sleep(20 * time.Millisecond) // the waiter on the entry
	close(release)
	select {
	case ok := <-got:
		if ok {
			t.Error("a waiter on a fetch that panicked was told an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter on a fetch that panicked is still held")
	}
}

// TestAlertsSourceIsTheView: the source a config installs answers
// through the process's view.
func TestAlertsSourceIsTheView(t *testing.T) {
	v := &alertsView{now: time.Now, fetch: func(context.Context) (liveswap.HeldAlerts, error) {
		return liveswap.HeldAlerts{Alerts: []liveswap.AppAlert{{App: "blog"}}}, nil
	}}
	var src liveswap.AlertsSource = alertsSource{view: v, logger: zap.NewNop()}
	if held, ok := src.Alerts(context.Background()); !ok || len(held.Alerts) != 1 || held.Alerts[0].App != "blog" {
		t.Errorf("%v %+v", ok, held)
	}
}

// TestHeldAlertsFoldChannelFailed: a ChannelFailed's copies addressed to
// each channel leave the people's reads only when Alertmanager holds the
// read-only copy of the same failure (integration and suspects): then
// the read carries that one row. With no read-only copy (an older
// moteld's failure, or vmalert's rules alone) they stay, as before; and
// a read-only copy of another failure folds none of them.
func TestHeldAlertsFoldChannelFailed(t *testing.T) {
	cf := func(channel, integration, suspects, summary string) string {
		return `{"labels":{"alertname":"ChannelFailed","notify_channel":"` + channel + `","severity":"warning","subject":"box","integration":"` + integration +
			`","suspects":"` + suspects + `"},"annotations":{"summary":"` + summary + `"},"startsAt":"2026-11-21T17:00:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[]}}`
	}
	other := `{"labels":{"alertname":"disk_low","subject":"box","instance":"/","severity":"warning"},"annotations":{"summary":"disk"},"startsAt":"2026-11-21T16:00:00Z","status":{"state":"active","silencedBy":[],"inhibitedBy":[]}}`
	addressed := cf("oncall", "webhook", "ops", "told") + "," + cf("pager", "webhook", "ops", "told") + "," + cf("chat", "webhook", "ops", "told")
	cases := []struct {
		name, held string
		want       []string // the ChannelFailed rows' summaries, in order
	}{
		{"three told and the read-only copy: one row", other + "," + addressed + "," + cf("-", "webhook", "ops", "read-only"), []string{"read-only"}},
		{"the read-only copy first", cf("-", "webhook", "ops", "read-only") + "," + addressed, []string{"read-only"}},
		{"three told, no read-only copy: three rows, as before", other + "," + addressed, []string{"told", "told", "told"}},
		{"one channel: the read-only copy alone", cf("-", "webhook", "ops", "read-only"), []string{"read-only"}},
		{"a read-only copy of another channel's failure folds nothing", addressed + "," + cf("-", "webhook", "oncall", "read-only"),
			[]string{"told", "told", "told", "read-only"}},
		{"nor one of another integration with the same suspects", addressed + "," + cf("-", "slack", "ops", "read-only"),
			[]string{"told", "told", "told", "read-only"}},
		{"two failures, one with its copy", addressed + "," + cf("ops", "discord", "alice", "alice told") + "," + cf("-", "webhook", "ops", "read-only"),
			[]string{"alice told", "read-only"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			am := &fakeAMReads{alerts: "[" + c.held + "]", silences: `[]`, alertsCode: 200, silCode: 200}
			held, err := newAMClient(am.serve(t)).heldAlerts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			others := 0
			for _, a := range held.Alerts {
				if a.Name == alertChannelFailed {
					got = append(got, a.Summary)
				} else {
					others++
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("ChannelFailed rows %q, want %q", got, c.want)
			}
			if want := strings.Count(c.held, `"disk_low"`); others != want {
				t.Errorf("%d other alerts, want %d", others, want)
			}
		})
	}
}
