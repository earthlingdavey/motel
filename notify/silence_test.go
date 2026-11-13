package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func goodSilence() SilenceRequest {
	return SilenceRequest{Alert: "disk_low", Comment: "resizing the volume", Duration: "2h", By: "alice"}
}

func TestSilenceCheck(t *testing.T) {
	for _, r := range []SilenceRequest{
		goodSilence(),
		{App: "blog", Comment: "c", By: "alice"},
		{Alert: "deploy_failed", App: "shop", Release: "3f9c2a1be0d4c5f6a7b8c9d0e1f2a3b4c5d6e7f8", Comment: "c", By: "alice"},
	} {
		if err := r.Check(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	cases := []struct {
		name string
		mod  func(*SilenceRequest)
		want string
	}{
		{"no alert nor app", func(r *SilenceRequest) { r.Alert = "" }, "name --alert, --app or both"},
		{"a release alone", func(r *SilenceRequest) { r.Alert, r.Release = "", "v3" }, "name --alert, --app or both"},
		{"a release with a slash", func(r *SilenceRequest) { r.Release = "a/b" }, "--release is a release's name"},
		{"a release with a dot first", func(r *SilenceRequest) { r.Release = ".x" }, "--release is a release's name"},
		{"no comment", func(r *SilenceRequest) { r.Comment = " " }, "--comment is one line"},
		{"two lines", func(r *SilenceRequest) { r.Comment = "a\nb" }, "--comment is one line"},
		{"an escape", func(r *SilenceRequest) { r.Comment = "fix\x1b[2Jdone" }, "no control character"},
		{"a tab", func(r *SilenceRequest) { r.Comment = "a\tb" }, "no control character"},
		{"a line separator", func(r *SilenceRequest) { r.Comment = "a\u2028b" }, "no control character"},
		{"long comment", func(r *SilenceRequest) { r.Comment = strings.Repeat("é", 201) }, "201 characters"},
		{"duration", func(r *SilenceRequest) { r.Duration = "2 hours" }, "a Go duration"},
		{"too short", func(r *SilenceRequest) { r.Duration = "59s" }, "from 1m to 24h"},
		{"too long", func(r *SilenceRequest) { r.Duration = "24h1s" }, "from 1m to 24h"},
		{"negative", func(r *SilenceRequest) { r.Duration = "-1h" }, "from 1m to 24h"},
		{"by", func(r *SilenceRequest) { r.By = "a b" }, "by is"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := goodSilence()
			c.mod(&r)
			err := r.Check()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			// No refusal echoes a value: Caddy journals a route's refusal.
			for _, v := range []string{r.Release, r.Duration} {
				if len(v) > 2 && strings.Contains(err.Error(), v) {
					t.Fatalf("the refusal echoes %q: %v", v, err)
				}
			}
		})
	}
	r := goodSilence()
	r.Duration = ""
	if d, err := r.parse(); err != nil || d != defaultSilence {
		t.Fatalf("no duration: %s, %v", d, err)
	}
	for _, d := range []string{"1m", "24h", "60.5s"} {
		r.Duration = d
		if err := r.Check(); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if err := (ExpireRequest{ID: "not-an-id", By: "alice"}).Check(); err == nil || !strings.Contains(err.Error(), "not a silence id") || strings.Contains(err.Error(), "not-an-id") {
		t.Fatalf("expire of a bad id: %v", err)
	}
}

// silenceAM is Alertmanager's silence API on a unix socket, recording
// each request.
type silenceAM struct {
	mu     sync.Mutex
	reqs   []string // "METHOD path body"
	status int
	answer string
	// state is what a GET of a silence answers.
	state string
}

func (f *silenceAM) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

func (f *silenceAM) set(status int, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.answer = status, answer
}

// unixServe serves h on a unix socket in a short directory (a socket
// path is at most 108 bytes) and returns its path.
func unixServe(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sil")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

const testSilenceID = "9f2c6d1e-5b4a-4c3d-8e7f-0a1b2c3d4e5f"

func withFakeAM(t *testing.T) (*testEngine, *silenceAM) {
	t.Helper()
	te := withEngine(t)
	f := &silenceAM{status: http.StatusOK, answer: `{"silenceID":"` + testSilenceID + `"}`, state: "active"}
	useSocket(t, unixServe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path+" "+string(b))
		status, answer := f.status, f.answer
		if r.Method == http.MethodGet && status == http.StatusOK {
			answer = `{"id":"` + testSilenceID + `","status":{"state":"` + f.state + `"}}`
		}
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	})))
	te.active.known = te.known // as Provision sets it
	saved := releasesOf
	releasesOf = func(_ *App, app string) []string {
		if app == "shop" {
			return []string{testSha, "v2"}
		}
		return nil
	}
	t.Cleanup(func() { releasesOf = saved })
	return te, f
}

// TestSilenceKeepsTheRequestInstant: a request at .9 of a second starts
// then, not at the whole second before it, so a 1m silence lasts 1m.
func TestSilenceKeepsTheRequestInstant(t *testing.T) {
	te, f := withFakeAM(t)
	te.at = te.at.Truncate(time.Second).Add(900 * time.Millisecond)
	req := SilenceRequest{Alert: "rollback", App: "shop", Duration: "1m", By: "alice", Comment: "noisy"}
	if _, err := callAdmin(t, serveSilence, http.MethodPost, req); err != nil {
		t.Fatal(err)
	}
	reqs := f.take()
	if len(reqs) != 1 {
		t.Fatalf("Alertmanager got %q", reqs)
	}
	var sent amSilence
	if err := json.Unmarshal([]byte(strings.TrimPrefix(reqs[0], "POST /api/v2/silences ")), &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.StartsAt.Equal(te.at) || sent.EndsAt.Sub(sent.StartsAt) != time.Minute {
		t.Fatalf("starts %s ends %s, the request at %s", sent.StartsAt, sent.EndsAt, te.at)
	}
}

// TestServeSilence pins the request Alertmanager is sent, the answer
// and the journal line.
func TestServeSilence(t *testing.T) {
	te, f := withFakeAM(t)
	req := SilenceRequest{Alert: "rollback", App: "shop", Release: testSha, Duration: "60.5s", By: "alice",
		Comment: "rolling back " + testSha + ", see https://hooks.slack.com/services/T0/B0/secretpart"} // gitleaks:allow — made up for this test
	w, err := callAdmin(t, serveSilence, http.MethodPost, req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.Body.String(), `{"id":"`+testSilenceID+`","ends_at":"2026-11-10T12:01:00.5Z"}`+"\n"; got != want {
		t.Fatalf("answer:\n got %s\nwant %s", got, want)
	}
	reqs := f.take()
	if len(reqs) != 1 {
		t.Fatalf("Alertmanager got %q", reqs)
	}
	// The comment masked as a summary is, the release kept.
	var sent amSilence
	if err := json.Unmarshal([]byte(strings.TrimPrefix(reqs[0], "POST /api/v2/silences ")), &sent); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent.Comment, "secretpart") || !strings.Contains(sent.Comment, "rolling back "+testSha) {
		t.Fatalf("the comment sent: %q", sent.Comment)
	}
	want := `POST /api/v2/silences {"matchers":[` +
		`{"name":"alertname","value":"rollback","isRegex":false,"isEqual":true},` +
		`{"name":"app","value":"shop","isRegex":false,"isEqual":true},` +
		`{"name":"release","value":"` + testSha + `","isRegex":false,"isEqual":true}],` +
		`"startsAt":"2026-11-10T12:00:00Z","endsAt":"2026-11-10T12:01:00.5Z",` +
		`"createdBy":"motel monitor: alice","comment":` + string(must(json.Marshal(sent.Comment))) + `}`
	if reqs[0] != want {
		t.Fatalf("Alertmanager got:\n %s\nwant %s", reqs[0], want)
	}
	lines := te.lines("monitor: silence added")
	if len(lines) != 1 {
		t.Fatalf("journal lines: %d", len(lines))
	}
	fields := lines[0].ContextMap()
	if fields["id"] != testSilenceID || fields["ends_at"] != "2026-11-10T12:01:00.5Z" || fields["alert"] != "rollback" ||
		fields["app"] != "shop" || fields["release"] != testSha ||
		fields["created_by"] != "motel monitor: alice" || fields["comment"] != sent.Comment {
		t.Fatalf("journal fields: %v", fields)
	}

	// Refused before Alertmanager hears of it, naming motel's own names
	// and never the value given.
	for _, c := range []struct {
		name string
		req  SilenceRequest
		want string
	}{
		{"Watchdog", SilenceRequest{Alert: "Watchdog"}, "--alert names none of the alerts motel raises on this box; they are "},
		{"NotifyTest", SilenceRequest{Alert: "NotifyTest"}, "--alert names none"},
		{"the mute", SilenceRequest{Alert: "deploy_in_progress"}, "--alert names none"},
		{"an unknown alert", SilenceRequest{Alert: "https://ntfy.sh/ops-alerts"}, "--alert names none"},
		{"an external name", SilenceRequest{Alert: "disk_failing"}, "--alert names none"},
		{"an unknown app", SilenceRequest{App: "secret-topic"}, "--app names no app this box runs; they are blog, shop, wiki"},
		{"a box alert with --app", SilenceRequest{Alert: "disk_low", App: "shop"}, "disk_low is about the box, not an app"},
		{"ChannelFailed with --app", SilenceRequest{Alert: "ChannelFailed", App: "shop"}, "is about the box, not an app"},
		{"a release without --app", SilenceRequest{Alert: "rollback", Release: "v2"}, "--release narrows one app's alerts whose release is on disk, so it needs --app and --alert one of rollback, deploy_succeeded, post_deploy_regressed"},
		{"a failed deploy's release", SilenceRequest{Alert: "deploy_failed", App: "shop", Release: "v2"}, "a failed or refused deploy's release never stays on disk"},
		{"a release of a state alert", SilenceRequest{Alert: "watchdog_throttled", App: "shop", Release: "v2"}, "--release narrows"},
		{"a release with --app alone", SilenceRequest{App: "shop", Release: "v2"}, "--release narrows"},
		{"a release not on disk", SilenceRequest{Alert: "rollback", App: "shop", Release: "3f9c2a1"}, "--release names no release of shop on this box; they are " + testSha + ", v2"},
		{"a release of an app with none", SilenceRequest{Alert: "rollback", App: "blog", Release: "v1"}, "--release names no release of blog on this box; they are none on disk"},
	} {
		r := c.req
		r.Comment, r.By = "c", "alice"
		_, err := callAdmin(t, serveSilence, http.MethodPost, r)
		if apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.name, err)
		}
		// A value not in its set is never echoed.
		for _, v := range []string{"https://ntfy.sh/ops-alerts", "secret-topic", "disk_failing", "3f9c2a1, ", "v1"} {
			if strings.Contains(err.Error(), v) {
				t.Fatalf("%s: the refusal echoes %q: %v", c.name, v, err)
			}
		}
		if strings.Contains(err.Error(), "Watchdog") {
			t.Fatalf("%s: Watchdog listed as silenceable: %v", c.name, err)
		}
	}
	// An app alone: the deploy mute left alone.
	if _, err := callAdmin(t, serveSilence, http.MethodPost, SilenceRequest{App: "blog", Comment: "c", By: "alice"}); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], `"matchers":[{"name":"app","value":"blog","isRegex":false,"isEqual":true},{"name":"alertname","value":"deploy_in_progress","isRegex":false,"isEqual":false}]`) {
		t.Fatalf("an app's silence: %q", got)
	}
	if _, err := callAdmin(t, serveSilence, http.MethodPost, `{"alert":"disk_low","comment":"c","by":"a","matchers":["x=y"]}`); apiStatus(err) != http.StatusBadRequest {
		t.Fatalf("a matchers field: %v", err)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("Alertmanager heard of a refused silence: %q", got)
	}

	// Alertmanager refusing: 502 with its words, and no journal line.
	f.set(http.StatusBadRequest, `"silence invalid"`)
	if _, err := callAdmin(t, serveSilence, http.MethodPost, goodSilence()); apiStatus(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "silence invalid") {
		t.Fatalf("Alertmanager refusing: %v", err)
	}
	if n := len(te.lines("monitor: silence added")); n != 2 {
		t.Fatalf("a refused silence journalled: %d lines in all", n)
	}

	// Not set up: 503 naming setup, Alertmanager untouched.
	useStateDir(t, filepath.Join(t.TempDir(), "absent"))
	_ = f.take()
	if _, err := callAdmin(t, serveSilence, http.MethodPost, goodSilence()); apiStatus(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), errNotSetUp.Error()) {
		t.Fatalf("not set up: %v", err)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("Alertmanager heard: %q", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestServeExpire(t *testing.T) {
	te, f := withFakeAM(t)
	w, err := callAdmin(t, serveExpire, http.MethodPost, ExpireRequest{ID: testSilenceID, By: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.Body.String(), `{"id":"`+testSilenceID+`"}`+"\n"; got != want {
		t.Fatalf("answer:\n got %s\nwant %s", got, want)
	}
	if got := f.take(); len(got) != 2 || got[0] != "GET /api/v2/silence/"+testSilenceID+" " || got[1] != "DELETE /api/v2/silence/"+testSilenceID+" " {
		t.Fatalf("Alertmanager got %q", got)
	}
	lines := te.lines("monitor: silence expired")
	if len(lines) != 1 || lines[0].ContextMap()["id"] != testSilenceID || lines[0].ContextMap()["expired_by"] != "motel monitor: alice" {
		t.Fatalf("journal: %+v", lines)
	}
	// Already expired: Alertmanager would answer 200 to the DELETE.
	f.mu.Lock()
	f.state = "expired"
	f.mu.Unlock()
	if _, err := callAdmin(t, serveExpire, http.MethodPost, ExpireRequest{ID: testSilenceID, By: "alice"}); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "already expired") {
		t.Fatalf("an expired one: %v", err)
	}
	if got := f.take(); len(got) != 1 || !strings.HasPrefix(got[0], "GET ") {
		t.Fatalf("an expired one was expired again: %q", got)
	}
	f.set(http.StatusNotFound, `"silence not found"`)
	if _, err := callAdmin(t, serveExpire, http.MethodPost, ExpireRequest{ID: testSilenceID, By: "alice"}); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "no silence "+testSilenceID) {
		t.Fatalf("an unknown id: %v", err)
	}
	f.set(http.StatusInternalServerError, `"internal"`)
	if _, err := callAdmin(t, serveExpire, http.MethodPost, ExpireRequest{ID: testSilenceID, By: "alice"}); apiStatus(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("Alertmanager failing: %v", err)
	}
	if n := len(te.lines("monitor: silence expired")); n != 1 {
		t.Fatalf("a failed expire journalled: %d lines in all", n)
	}
}

// adminServe is the admin API's handling of a route's error, as Caddy
// does it: the APIError's status and {"error":…}.
func adminServe(t *testing.T) string {
	mux := http.NewServeMux()
	for _, r := range (adminAPI{}).Routes() {
		h := r.Handler
		mux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			err := h.ServeHTTP(w, req)
			if err == nil {
				return
			}
			status := http.StatusInternalServerError
			var ae caddy.APIError
			if errors.As(err, &ae) {
				status, err = ae.HTTPStatus, ae.Err
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		})
	}
	return "unix/" + unixServe(t, mux)
}

func runSilence(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	c := silenceCommand()
	c.SilenceUsage = true // as Caddy's root command has it
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(append(args, "--address", addr))
	err := c.Execute()
	return out.String(), err
}

// TestSilenceCommandOutput pins what both commands print, as text and
// with --json, through the admin route to a fake Alertmanager.
func TestSilenceCommandOutput(t *testing.T) {
	_, f := withFakeAM(t)
	addr := adminServe(t)
	t.Setenv("SUDO_USER", "alice")
	add := []string{"add", "--comment", "resizing the volume", "--alert", "disk_low"}
	cases := []struct {
		name string
		args []string
		out  string
		err  bool
	}{
		{"add", add, "silence " + testSilenceID + " added; it ends at 2026-11-10T14:00:00Z\n", false},
		{"add json", append(add, "--json"), `{"id":"` + testSilenceID + `","ends_at":"2026-11-10T14:00:00Z"}` + "\n", false},
		{"expire", []string{"expire", testSilenceID}, "silence " + testSilenceID + " expired\n", false},
		{"expire json", []string{"expire", testSilenceID, "--json"}, `{"id":"` + testSilenceID + `"}` + "\n", false},
		{"refused json", []string{"add", "--comment", "x", "--json", "--alert", "Watchdog"},
			`{"error":"--alert names none of the alerts motel raises on this box; they are ` + strings.Join(silenceableAlerts(theEngine.active), ", ") + `"}` + "\n", true},
		{"no comment json", []string{"add", "--json", "--alert", "disk_low"},
			`{"error":"--comment is one line of text, not empty and with no control character: say why, for whoever finds the silence"}` + "\n", true},
		{"an argument", []string{"add", "--comment", "x", "--alert", "disk_low", "alertname=disk_low"}, "", true},
		{"bad id json", []string{"expire", "--json", "x"}, `{"error":"that is not a silence id (Alertmanager's are UUIDs, as silence add prints them)"}` + "\n", true},
		{"refused text", []string{"add", "--comment", "x", "--app", "nosuch"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runSilence(t, addr, c.args...)
			if (err != nil) != c.err {
				t.Fatalf("err = %v", err)
			}
			if out != c.out {
				t.Fatalf("output:\n got %q\nwant %q", out, c.out)
			}
		})
	}
	reqs := f.take()
	if len(reqs) != 6 || !strings.Contains(reqs[0], `"createdBy":"motel monitor: alice"`) {
		t.Fatalf("Alertmanager got %q", reqs)
	}

	// A route's 502 that says permission denied (motel refused
	// Alertmanager's socket) is passed on, not taken for the caller's.
	f.set(http.StatusForbidden, `"connect: permission denied"`)
	out, err := runSilence(t, addr, "add", "--comment", "x", "--alert", "disk_low")
	if err == nil || !strings.Contains(err.Error(), "silencing in Alertmanager") || strings.Contains(err.Error(), "run motel monitor silence as root") {
		t.Fatalf("a 502 naming permission: %q, %v", out, err)
	}

	// The route's refusal, not the command's: Alertmanager's 404.
	f.set(http.StatusNotFound, `"silence not found"`)
	out, err = runSilence(t, addr, "expire", "--json", testSilenceID)
	if err == nil || out != `{"error":"no silence `+testSilenceID+` in Alertmanager"}`+"\n" {
		t.Fatalf("a 404 with --json: %q, %v", out, err)
	}
}

func TestSilenceCommandNotAdmitted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reaches any socket")
	}
	dir, err := os.MkdirTemp("", "sil")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "admin.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err = runSilence(t, "unix/"+path, "add", "--comment", "x", "--alert", "disk_low")
	if err == nil || !strings.Contains(err.Error(), "admits root and the motel user: run motel monitor silence as root") {
		t.Fatalf("err = %v", err)
	}
}

func TestInvokingUser(t *testing.T) {
	t.Setenv("SUDO_USER", "alice")
	t.Setenv("USER", "root")
	if got := invokingUser(); got != "alice" {
		t.Fatalf("with sudo: %s", got)
	}
	t.Setenv("SUDO_USER", "")
	if got := invokingUser(); got != "root" {
		t.Fatalf("without: %s", got)
	}
	t.Setenv("USER", "a b")
	if got := invokingUser(); !byRe.MatchString(got) {
		t.Fatalf("a bad USER: %q", got)
	}
}

const testSha = "3f9c2a1be0d4c5f6a7b8c9d0e1f2a3b4c5d6e7f8"

// TestSilenceOfARuleOfOneApp: a threshold line pinned to one app is
// silenced with that --app, or none, and refused with another.
func TestSilenceOfARuleOfOneApp(t *testing.T) {
	te, _ := withFakeAM(t)
	te.active.Metrics = &MetricsStore{Rules: []Rule{
		{Name: "shop_slow", Kind: ruleThreshold, App: "shop", Metric: "p95", Op: ">", Value: 500, For: caddy.Duration(5 * 60e9)},
	}}
	for app, ok := range map[string]bool{"shop": true, "": true, "blog": false} {
		_, err := callAdmin(t, serveSilence, http.MethodPost, SilenceRequest{Alert: "shop_slow", App: app, Comment: "c", By: "alice"})
		if ok != (err == nil) || !ok && !strings.Contains(err.Error(), "shop_slow is a rule about shop alone") {
			t.Fatalf("--app %q: %v", app, err)
		}
	}
}

// TestSilenceCommentMaskedAcrossAReload: the comment is masked with
// the known values of the config the route checked and the engine's
// now, so that a reload between the two neither unmasks a secret the
// checked config knew nor misses one the new config added (Copilot
// round 2 on #444, and its self-review). The journal line carries the
// same masked text; the marker and the rest of the comment survive.
func TestSilenceCommentMaskedAcrossAReload(t *testing.T) {
	const (
		oldURL = "https://hooks.slack.com/services/T0/B0/oldsecretpart" // gitleaks:allow — made up for this test
		newURL = "https://hooks.slack.com/services/T0/B0/newsecretpart" // gitleaks:allow — made up for this test
	)
	for _, c := range []struct {
		name         string
		checked, now []string
		url, key     string
	}{
		{"a secret the checked config knew", []string{"NOTIFY_OLD_URL=" + oldURL}, nil, oldURL, "NOTIFY_OLD_URL"},
		{"a secret the reload added", nil, []string{"NOTIFY_NEW_URL=" + newURL}, newURL, "NOTIFY_NEW_URL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			te, f := withFakeAM(t)
			te.active.known, te.known = c.checked, c.now
			req := goodSilence()
			req.Comment = "resizing, see " + c.url + " for the thread"
			if _, err := callAdmin(t, serveSilence, http.MethodPost, req); err != nil {
				t.Fatal(err)
			}
			got := f.take()
			if len(got) != 1 {
				t.Fatalf("Alertmanager got %q", got)
			}
			var sent amSilence
			if err := json.Unmarshal([]byte(strings.TrimPrefix(got[0], "POST /api/v2/silences ")), &sent); err != nil {
				t.Fatal(err)
			}
			want := "resizing, see [redacted:" + c.key + "] for the thread"
			if sent.Comment != want {
				t.Fatalf("comment sent = %q, want %q", sent.Comment, want)
			}
			lines := te.lines("monitor: silence added")
			if len(lines) != 1 || lines[0].ContextMap()["comment"] != want {
				t.Fatalf("journal: %+v", lines)
			}
		})
	}
	_, f := withFakeAM(t)
	req := goodSilence()
	req.Comment = strings.Repeat("x", 200)
	if _, err := callAdmin(t, serveSilence, http.MethodPost, req); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0], `"comment":"`+strings.Repeat("x", 200)+`"`) {
		t.Fatalf("a 200-character comment: %q", got)
	}
}

// TestAdminBodyRefusalEchoesNothing: a body the decoder refuses (an
// unknown field named like a secret, a field of the wrong type, broken
// JSON) is answered by every notify route with one fixed 400 that holds
// nothing of it, since Caddy journals the refusal; a GET is still 405.
func TestAdminBodyRefusalEchoesNothing(t *testing.T) {
	withFakeAM(t)
	for name, h := range map[string]caddy.AdminHandlerFunc{
		"events": serveSend, "test": serveTest, "silences": serveSilence, "expire": serveExpire,
	} {
		for _, body := range []string{
			`{"hunter2_token_q8Rm2vTx9Lk4":1}`, `{"comment":5}`, `{"summary":5}`, `{"channel":5}`, `{"id":5}`, `{"id":"hunter2_token_q8Rm2vTx9Lk4"`,
		} {
			_, err := callAdmin(t, h, http.MethodPost, body)
			if apiStatus(err) != http.StatusBadRequest || err.Error() != errBadBody.Error() {
				t.Fatalf("%s %s: %v", name, body, err)
			}
		}
		if _, err := callAdmin(t, h, http.MethodGet, ""); apiStatus(err) != http.StatusMethodNotAllowed {
			t.Fatalf("%s a GET: %v", name, err)
		}
	}
}

// TestSilenceOfPostDeployRegressedNeedsTheAppsRule: post_deploy_regressed
// with --app only for an app a post_deploy rule judges: its own, or the
// every-app one (postDeployRuleFor, the comparator's).
func TestSilenceOfPostDeployRegressedNeedsTheAppsRule(t *testing.T) {
	te, _ := withFakeAM(t)
	try := func(rules []Rule, app string) error {
		te.active.Metrics = &MetricsStore{Rules: rules}
		_, err := callAdmin(t, serveSilence, http.MethodPost, SilenceRequest{Alert: eventPostDeployRegressed, App: app, Comment: "c", By: "alice"})
		return err
	}
	shop := Rule{Name: "shop_regress", Kind: rulePostDeploy, App: "shop"}
	wiki := Rule{Name: "wiki_regress", Kind: rulePostDeploy, App: "wiki"}
	every := Rule{Name: "regress", Kind: rulePostDeploy}
	if err := try([]Rule{shop}, "blog"); err == nil || !strings.Contains(err.Error(), "no post_deploy rule judges blog's deploys") {
		t.Fatalf("blog with shop's rule alone: %v", err)
	}
	for _, app := range []string{"shop", "wiki"} {
		if err := try([]Rule{shop, wiki}, app); err != nil {
			t.Fatalf("%s among two app rules: %v", app, err)
		}
	}
	if err := try([]Rule{shop, wiki}, "blog"); err == nil {
		t.Fatal("blog among two other apps' rules was taken")
	}
	if err := try([]Rule{shop, every}, "blog"); err != nil {
		t.Fatalf("blog under the every-app rule: %v", err)
	}
}
