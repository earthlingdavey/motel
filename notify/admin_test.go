package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func goodSend() SendRequest {
	return SendRequest{Source: "smartd", Subject: "box", Name: "disk_failing", Severity: SeverityCritical, Instance: "/dev/sda", Summary: "SMART says /dev/sda is failing"}
}

func TestSendCheck(t *testing.T) {
	if err := goodSend().Check(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mod  func(*SendRequest)
		want string
	}{
		{"no source", func(r *SendRequest) { r.Source = "" }, "source is"},
		{"source external:", func(r *SendRequest) { r.Source = "external:x" }, "source is"},
		{"subject", func(r *SendRequest) { r.Subject = "app" }, "subject is"},
		{"subject app bad", func(r *SendRequest) { r.Subject = "app:Blog" }, "subject is"},
		{"name", func(r *SendRequest) { r.Name = "Disk-Failing" }, "name is"},
		{"Watchdog", func(r *SendRequest) { r.Name = "watchdog" }, "motel's own"},
		{"watchdog_", func(r *SendRequest) { r.Name = "watchdog_custom" }, "motel's own"},
		{"mute", func(r *SendRequest) { r.Name = "deploy_in_progress" }, "motel's own"},
		{"deploy_failed", func(r *SendRequest) { r.Name = "deploy_failed" }, "motel's own"},
		{"channel failed", func(r *SendRequest) { r.Name = "channelfailed" }, "motel's own"},
		{"state", func(r *SendRequest) { r.State = "on" }, "state is"},
		{"severity", func(r *SendRequest) { r.Severity = "page" }, "severity is"},
		{"instance", func(r *SendRequest) { r.Instance = "a b" }, "instance is"},
		{"summary empty", func(r *SendRequest) { r.Summary = " " }, "summary is one line"},
		{"summary lines", func(r *SendRequest) { r.Summary = "a\nb" }, "summary is one line"},
		{"summary long", func(r *SendRequest) { r.Summary = strings.Repeat("é", 201) }, "201 characters"},
		{"detail name", func(r *SendRequest) { r.Details = map[string]string{"Bad": "x"} }, "detail"},
		{"details big", func(r *SendRequest) { r.Details = map[string]string{"log": strings.Repeat("x", 3000)} }, "at most 2048"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := goodSend()
			c.mod(&r)
			err := r.Check()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// withEngine points the process's engine at a test one for the length
// of a test: the admin routes read theEngine.
func withEngine(t *testing.T) *testEngine {
	t.Helper()
	te := newTestEngine(t)
	a := mustParse(t, fullBlock)
	r, err := a.resolve(testTrust(), testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	a.cfg = r
	te.active = a
	saved := theEngine
	theEngine = te.engine
	t.Cleanup(func() { theEngine = saved })
	return te
}

func callAdmin(t *testing.T, h caddy.AdminHandlerFunc, method string, body any) (*httptest.ResponseRecorder, error) {
	t.Helper()
	var buf bytes.Buffer
	if s, ok := body.(string); ok {
		buf.WriteString(s)
	} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	err := h(w, httptest.NewRequest(method, "/notify/x", &buf))
	return w, err
}

func apiStatus(err error) int {
	var ae caddy.APIError
	if errors.As(err, &ae) {
		return ae.HTTPStatus
	}
	return 0
}

func TestServeSend(t *testing.T) {
	te := withEngine(t)
	w, err := callAdmin(t, serveSend, http.MethodPost, goodSend())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"key":"external:smartd/box/disk_failing//dev/sda"`) {
		t.Fatalf("answer = %s", w.Body.String())
	}
	got := te.drain()
	if len(got) != 1 || got[0].Labels["source"] != "external:smartd" || got[0].Labels["kind"] != "event" {
		t.Fatalf("posts = %+v", got)
	}
	if lines := te.lines("notify: event"); len(lines) != 1 {
		t.Fatalf("no journal line: %+v", lines)
	}
	// A state alert to an app's channels.
	r := goodSend()
	r.Subject, r.State, r.Instance = "app:blog", "firing", ""
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); err != nil {
		t.Fatal(err)
	}
	got = te.drain()
	if len(got) != 1 || got[0].Labels["app"] != "blog" || got[0].Labels["kind"] != "state" {
		t.Fatalf("posts = %+v", got)
	}
}

func TestServeSendRefusals(t *testing.T) {
	withEngine(t)
	r := goodSend()
	r.Subject = "app:nosuch"
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); apiStatus(err) != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), "names no app") {
		t.Fatalf("an unknown app: %v", err)
	}
	r = goodSend()
	r.Name = "deploy_in_progress"
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); apiStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("the mute's name: %v", err)
	}
	if _, err := callAdmin(t, serveSend, http.MethodPost, `{"source":"x","labels":{"app":"blog"}}`); apiStatus(err) != http.StatusBadRequest {
		t.Fatalf("a raw Alertmanager label set: %v", err)
	}
	if _, err := callAdmin(t, serveSend, http.MethodGet, goodSend()); apiStatus(err) != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %v", err)
	}
	theEngine.active = nil
	if _, err := callAdmin(t, serveSend, http.MethodPost, goodSend()); apiStatus(err) != http.StatusServiceUnavailable {
		t.Fatalf("no notify block: %v", err)
	}
}

func TestServeTest(t *testing.T) {
	te := withEngine(t)
	saved := testWait
	testWait = 2 * time.Second
	t.Cleanup(func() { testWait = saved })
	var sent atomic.Int32
	te.metrics = func(context.Context) ([]byte, error) {
		body := fmt.Sprintf("alertmanager_notifications_total{integration=\"slack\",receiver_name=\"ch-ops\"} %d\n", sent.Load()) +
			failedLine("slack", "ch-ops", "clientError", 0)
		return []byte(body), nil
	}
	te.p.err = nil
	// Alertmanager delivers on the second read.
	go func() {
		time.Sleep(300 * time.Millisecond)
		sent.Store(1)
	}()
	w, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	var res TestResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Delivered {
		t.Fatalf("result = %s", w.Body.String())
	}
	posts := te.p.take()
	if len(posts) != 1 || posts[0].Labels[labelChannel] != "ops" || posts[0].Labels["alertname"] != "NotifyTest" || posts[0].Labels["test"] == "" {
		t.Fatalf("posts = %+v", posts)
	}
	if _, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "nosuch"}); apiStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown channel: %v", err)
	}
}

// fakeSystem records what setup does.
type fakeSystem struct {
	cmds        []string
	isInstalled bool
	owners      map[string][2]int
}

func (f *fakeSystem) run(name string, args ...string) error {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	return nil
}
func (f *fakeSystem) installed(string) bool { return f.isInstalled }
func (f *fakeSystem) chown(p string, uid, gid int) error {
	if f.owners == nil {
		f.owners = map[string][2]int{}
	}
	f.owners[p] = [2]int{uid, gid}
	return nil
}
func (f *fakeSystem) lookupUser(name string) (int, int, error) {
	if name == "motel" {
		return 986, 986, nil
	}
	return 103, 103, nil
}
func (f *fakeSystem) lookupGroup(string) (int, error) { return 103, nil }

func TestSetupOrderAndFiles(t *testing.T) {
	root := t.TempDir()
	fs0 := &fakeSystem{}
	var out bytes.Buffer
	if err := setup(fs0, root, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl mask prometheus-alertmanager.service",
		"apt-get install -y --no-install-recommends prometheus-alertmanager",
		"systemctl daemon-reload",
		"systemctl unmask prometheus-alertmanager.service",
		"systemctl disable prometheus-alertmanager.service",
		"systemctl enable --now motel-am.socket",
		"systemctl try-reload-or-restart motel.service",
	}
	if !slices.Equal(fs0.cmds, want) {
		t.Fatalf("commands:\n%s\nwant\n%s", strings.Join(fs0.cmds, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range []string{dropInPath, socketPath} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte("# Written by `motel notify setup`")) {
			t.Errorf("%s = %q", p, b[:40])
		}
	}
	dir := filepath.Join(root, DefaultStateDir)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&fs.ModeSetgid == 0 || st.Mode().Perm() != 0o750 {
		t.Errorf("state dir mode = %v", st.Mode())
	}
	if fs0.owners[dir] != [2]int{986, 103} {
		t.Errorf("state dir owner = %v, want motel:prometheus", fs0.owners[dir])
	}
	cfg, err := os.ReadFile(filepath.Join(dir, ConfigName))
	if err != nil || !bytes.Contains(cfg, []byte("receiver: blackhole")) {
		t.Fatalf("default config = %q, %v", cfg, err)
	}
	// Run again on a box already set up: the package is not installed
	// again, and the config motel rendered since is not replaced.
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte("rendered\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	fs1 := &fakeSystem{isInstalled: true}
	if err := setup(fs1, root, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range fs1.cmds {
		if strings.HasPrefix(c, "apt-get") {
			t.Fatal("installed again")
		}
	}
	if fs1.cmds[0] != "systemctl mask prometheus-alertmanager.service" {
		t.Fatalf("not masked first: %v", fs1.cmds)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ConfigName)); string(b) != "rendered\n" {
		t.Fatalf("setup replaced motel's rendered config: %q", b)
	}
}

// TestUnitsAreTheMeasuredOnes holds the shipped units to what ran on
// hotserve-1: no TCP, the socket's service named, the group, gossip off.
func TestUnitsAreTheMeasuredOnes(t *testing.T) {
	sock := string(unitSocket)
	for _, want := range []string{"Service=prometheus-alertmanager.service", "ListenStream=/run/motel-notify/am.sock", "SocketGroup=motel", "SocketMode=0660"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket unit lacks %q", want)
		}
	}
	drop := string(unitDropIn)
	for _, want := range []string{"ExecStart=\n", "--web.systemd-socket", "--config.file=" + DefaultStateDir + "/" + ConfigName, "--cluster.listen-address= ", "receiver-name-in-metrics", "Requires=motel-am.socket"} {
		if !strings.Contains(drop, want) {
			t.Errorf("drop-in lacks %q", want)
		}
	}
	if strings.Contains(drop, "web.listen-address") {
		t.Error("the drop-in names a TCP listen address")
	}
}

func TestAPIRefusal(t *testing.T) {
	err := apiRefusal(errors.New(`caddy responded with error: HTTP 422: {"error":"subject app:x names no app this box runs"}` + "\n"))
	if err.Error() != "subject app:x names no app this box runs" {
		t.Fatalf("err = %q", err)
	}
	other := errors.New("performing request: dial unix /run/motel/admin.sock: connect: permission denied")
	if !errors.Is(apiRefusal(other), other) {
		t.Fatal("a transport error was rewritten")
	}
}
