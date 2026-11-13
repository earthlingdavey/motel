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

	"github.com/caddyserver/caddy/v2"
)

func goodSilence() SilenceRequest {
	return SilenceRequest{Matchers: []string{"alertname=disk_low"}, Comment: "resizing the volume", Duration: "2h", By: "alice"}
}

func TestSilenceCheck(t *testing.T) {
	goods := [][]string{
		{"alertname=disk_low"},
		{"app=blog", "alertname=~watchdog_.*"},
		{"alertname=disk_failing", "instance!=/dev/sdb"},
		{"subject=box", "severity!~critical|warning"},
		// Watchdog named, and something it does not have: not the heartbeat.
		{"alertname=Watchdog", "app=blog"},
		{"severity=none", "alertname!=Watchdog"},
	}
	for _, m := range goods {
		r := goodSilence()
		r.Matchers = m
		if err := r.Check(); err != nil {
			t.Errorf("%q: %v", m, err)
		}
	}
	cases := []struct {
		name string
		mod  func(*SilenceRequest)
		want string
	}{
		{"no matcher", func(r *SilenceRequest) { r.Matchers = nil }, "at least one matcher"},
		{"match everything", func(r *SilenceRequest) { r.Matchers = []string{"alertname=~.*"} }, "label=value with a value"},
		{"negations alone", func(r *SilenceRequest) { r.Matchers = []string{"alertname!=x", "app!~y"} }, "label=value with a value"},
		{"empty equality", func(r *SilenceRequest) { r.Matchers = []string{"app="} }, "label=value with a value"},
		{"empty equality beside a regex", func(r *SilenceRequest) { r.Matchers = []string{"app=", "alertname=~.+"} }, "label=value with a value"},
		{"Watchdog", func(r *SilenceRequest) { r.Matchers = []string{"alertname=Watchdog"} }, "silence Watchdog"},
		{"Watchdog by severity", func(r *SilenceRequest) { r.Matchers = []string{"severity=none"} }, "silence Watchdog"},
		{"Watchdog among", func(r *SilenceRequest) { r.Matchers = []string{"alertname=~Watchdog|disk_low", "severity=none"} }, "silence Watchdog"},
		{"quoted", func(r *SilenceRequest) { r.Matchers = []string{`alertname="disk_low"`} }, "no quotes"},
		{"single quoted", func(r *SilenceRequest) { r.Matchers = []string{`alertname='disk_low'`} }, "no quotes"},
		{"no operator", func(r *SilenceRequest) { r.Matchers = []string{"alertname"} }, "label=value, label!=value"},
		{"bad name", func(r *SilenceRequest) { r.Matchers = []string{"1app=blog"} }, "label name"},
		{"spaced", func(r *SilenceRequest) { r.Matchers = []string{"alertname= disk_low"} }, "a space at an end"},
		{"control", func(r *SilenceRequest) { r.Matchers = []string{"alertname=a\tb"} }, "control character"},
		{"bad regex", func(r *SilenceRequest) { r.Matchers = []string{"app=blog", "alertname=~(x"} }, "missing closing"},
		{"long matcher", func(r *SilenceRequest) { r.Matchers = []string{"app=" + strings.Repeat("x", 200)} }, "at most 200 bytes"},
		{"many", func(r *SilenceRequest) { r.Matchers = make([]string, 17) }, "at most 16"},
		{"no comment", func(r *SilenceRequest) { r.Comment = " " }, "comment is one line"},
		{"two lines", func(r *SilenceRequest) { r.Comment = "a\nb" }, "comment is one line"},
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
		})
	}
	r := goodSilence()
	r.Duration = ""
	if _, d, err := r.parse(); err != nil || d != defaultSilence {
		t.Fatalf("no duration: %s, %v", d, err)
	}
	for _, d := range []string{"1m", "24h"} {
		r.Duration = d
		if err := r.Check(); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
	if err := (ExpireRequest{ID: "not-an-id", By: "alice"}).Check(); err == nil || !strings.Contains(err.Error(), "not a silence id") {
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
	te.active.Socket = unixServe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	return te, f
}

// TestServeSilence pins the request Alertmanager is sent, the answer
// and the journal line.
func TestServeSilence(t *testing.T) {
	te, f := withFakeAM(t)
	req := goodSilence()
	req.Matchers = []string{"alertname=disk_low", "app!~blog|shop", "instance=~/var/.*"}
	w, err := callAdmin(t, serveSilence, http.MethodPost, req)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := w.Body.String(), `{"id":"`+testSilenceID+`","ends_at":"2026-11-10T14:00:00Z"}`+"\n"; got != want {
		t.Fatalf("answer:\n got %s\nwant %s", got, want)
	}
	want := `POST /api/v2/silences {"matchers":[` +
		`{"name":"alertname","value":"disk_low","isRegex":false,"isEqual":true},` +
		`{"name":"app","value":"blog|shop","isRegex":true,"isEqual":false},` +
		`{"name":"instance","value":"/var/.*","isRegex":true,"isEqual":true}],` +
		`"startsAt":"2026-11-10T12:00:00Z","endsAt":"2026-11-10T14:00:00Z",` +
		`"createdBy":"motel monitor: alice","comment":"resizing the volume"}`
	if got := f.take(); len(got) != 1 || got[0] != want {
		t.Fatalf("Alertmanager got:\n %q\nwant %q", got, want)
	}
	lines := te.lines("monitor: silence added")
	if len(lines) != 1 {
		t.Fatalf("journal lines: %d", len(lines))
	}
	fields := lines[0].ContextMap()
	if fields["id"] != testSilenceID || fields["ends_at"] != "2026-11-10T14:00:00Z" ||
		fields["created_by"] != "motel monitor: alice" || fields["comment"] != "resizing the volume" {
		t.Fatalf("journal fields: %v", fields)
	}
	if m, _ := fields["matchers"].([]any); len(m) != 3 || m[1] != "app!~blog|shop" {
		t.Fatalf("journal matchers: %#v", fields["matchers"])
	}

	// A release's git sha is refused, as the filter masks it (accepted:
	// DESIGN-notify.md, "Accepted residuals").
	sha := goodSilence()
	sha.Matchers = []string{"alertname=deploy_failed", "release=3f9c2a1be0d4c5f6a7b8c9d0e1f2a3b4c5d6e7f8"}
	if _, err := callAdmin(t, serveSilence, http.MethodPost, sha); apiStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("a sha in a matcher: %v", err)
	}

	// Refused before Alertmanager hears of it.
	bad := goodSilence()
	bad.Matchers = []string{"alertname=Watchdog"}
	if _, err := callAdmin(t, serveSilence, http.MethodPost, bad); apiStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("Watchdog: %v", err)
	}
	bad = goodSilence()
	bad.Comment = "see https://hooks.slack.com/services/T0/B0/secretpart" // gitleaks:allow — made up for this test
	if _, err := callAdmin(t, serveSilence, http.MethodPost, bad); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "filter masks") {
		t.Fatalf("a URL in the comment: %v", err)
	}
	bad = goodSilence()
	// A matcher goes through the whole filter: a webhook's random part,
	// regex-escaped so that no known form matches, is masked as a token.
	bad.Matchers = []string{"alertname=x", `notify_channel=~https://hooks\.slack\.com/services/T0/B0/Zx9Kq2Lm8Vb4Nw7Rt5Yp3Hs6Jd1F`} // gitleaks:allow — made up for this test
	if _, err := callAdmin(t, serveSilence, http.MethodPost, bad); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "filter masks") {
		t.Fatalf("a token in a matcher: %v", err)
	}
	if _, err := callAdmin(t, serveSilence, http.MethodPost, `{"matchers":["alertname=x"],"comment":"c","by":"a","labels":{}}`); apiStatus(err) != http.StatusBadRequest {
		t.Fatalf("an unknown field: %v", err)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("Alertmanager heard of a refused silence: %q", got)
	}

	// Alertmanager refusing: 502 with its words, and no journal line.
	f.set(http.StatusBadRequest, `"silence invalid"`)
	if _, err := callAdmin(t, serveSilence, http.MethodPost, goodSilence()); apiStatus(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "silence invalid") {
		t.Fatalf("Alertmanager refusing: %v", err)
	}
	if n := len(te.lines("monitor: silence added")); n != 1 {
		t.Fatalf("a refused silence journalled: %d lines in all", n)
	}

	// Not set up: 503 naming setup, Alertmanager untouched.
	te.active.StateDir = filepath.Join(t.TempDir(), "absent")
	_ = f.take()
	if _, err := callAdmin(t, serveSilence, http.MethodPost, goodSilence()); apiStatus(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), errNotSetUp.Error()) {
		t.Fatalf("not set up: %v", err)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("Alertmanager heard: %q", got)
	}
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
	add := []string{"add", "--comment", "resizing the volume", "alertname=disk_low"}
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
		{"refused json", []string{"add", "--comment", "x", "--json", "alertname=Watchdog"},
			`{"error":"these matchers silence Watchdog, the heartbeat: its pings would stop and the check off the box would report the box down; pause that check instead"}` + "\n", true},
		{"no comment json", []string{"add", "--json", "alertname=disk_low"},
			`{"error":"comment is one line, not empty: say why, for whoever finds the silence"}` + "\n", true},
		{"bad id json", []string{"expire", "--json", "x"}, `{"error":"\"x\" is not a silence id (Alertmanager's are UUIDs, as silence add prints them)"}` + "\n", true},
		{"refused text", []string{"add", "--comment", "x", "severity=none"}, "", true},
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
	out, err := runSilence(t, addr, "add", "--comment", "x", "alertname=disk_low")
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
	_, err = runSilence(t, "unix/"+path, "add", "--comment", "x", "alertname=disk_low")
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
