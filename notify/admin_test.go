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
		{"metrics chain", func(r *SendRequest) { r.Name = "metrics_store_down" }, "motel's own"},
		{"store rule", func(r *SendRequest) { r.Name = "app_memory_high" }, "motel's own"},
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
	useStateDir(t, t.TempDir()) // set up: setup made the state directory
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
	if lines := te.lines("monitor: event"); len(lines) != 1 {
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
		t.Fatalf("no notify: %v", err)
	}
}

func amCounts(notifications, requests, requestsFailed, failed int) string {
	const receiver = "ch-ops"
	return fmt.Sprintf("alertmanager_notifications_total{integration=\"slack\",receiver_name=%q} %d\n", receiver, notifications) +
		fmt.Sprintf("alertmanager_notification_requests_total{integration=\"slack\",receiver_name=%q} %d\n", receiver, requests) +
		fmt.Sprintf("alertmanager_notification_requests_failed_total{integration=\"slack\",receiver_name=%q} %d\n", receiver, requestsFailed) +
		failedLine("slack", receiver, "serverError", failed)
}

// TestServeTest: a delivery is an attempt that did not fail. A receiver
// whose attempts fail and are retried has notifications_total raised
// before the first one, which must not read as delivered.
func TestServeTest(t *testing.T) {
	te := withEngine(t)
	saved := testWait
	testWait = 2 * time.Second
	t.Cleanup(func() { testWait = saved })
	var body atomic.Pointer[string]
	set := func(s string) { body.Store(&s) }
	te.metrics = func(context.Context) ([]byte, error) { return []byte(*body.Load()), nil }

	set(amCounts(0, 0, 0, 0))
	go func() {
		time.Sleep(300 * time.Millisecond)
		set(amCounts(1, 1, 1, 0)) // attempted, failed, being retried
		time.Sleep(300 * time.Millisecond)
		set(amCounts(1, 2, 1, 0)) // the retry got through
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

	// Attempts that fail and are given up: failed, never delivered.
	set(amCounts(0, 0, 0, 0))
	go func() {
		time.Sleep(300 * time.Millisecond)
		set(amCounts(1, 1, 1, 0))
		time.Sleep(300 * time.Millisecond)
		set(amCounts(1, 3, 3, 1))
	}()
	w, err = callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	res = TestResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Delivered || res.Failed == "" {
		t.Fatalf("a failing channel: %s", w.Body.String())
	}
	// Attempts failing and still retried (a 5xx): named failing, not
	// "neither".
	set(amCounts(0, 0, 0, 0))
	go func() {
		time.Sleep(300 * time.Millisecond)
		set(amCounts(1, 2, 2, 0))
	}()
	w, err = callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	res = TestResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Delivered || !strings.Contains(res.Failed, "still retrying") {
		t.Fatalf("a retrying channel: %s", w.Body.String())
	}
	// No baseline: no verdict.
	te.metrics = func(context.Context) ([]byte, error) { return nil, errors.New("starting") }
	w, err = callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	res = TestResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Delivered || res.Failed != "" || res.Note == "" {
		t.Fatalf("no baseline: %s", w.Body.String())
	}
	if _, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "nosuch"}); apiStatus(err) != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown channel: %v", err)
	}
}

// TestSendAndTestSayWhatTheJournalSays: what was said in the journal
// alone is said by the commands too. On a box setup has not run on, a
// sent event goes nowhere but the journal, and send exits 1 naming
// setup; test names setup instead of a socket error. A resolve that
// finds nothing firing under its key says so. A channel a boot dropped
// is named as dropped, not as unknown among no channels.
func TestSendAndTestSayWhatTheJournalSays(t *testing.T) {
	te := withEngine(t)
	var out bytes.Buffer

	// A resolve with nothing firing under its key (another subject).
	r := goodSend()
	r.State = "firing"
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Instance = "resolved", "/dev/sdb"
	w, err := callAdmin(t, serveSend, http.MethodPost, r)
	if err != nil {
		t.Fatal(err)
	}
	var res SendResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || !res.ResolvedNothing || res.Undelivered != "" {
		t.Fatalf("a resolve of another instance: %s", w.Body.String())
	}
	if err := sendAnswer(&out, res); err != nil || !strings.HasPrefix(out.String(), "resolved nothing: no state fires under external:smartd/box/disk_failing//dev/sdb") {
		t.Fatalf("send said %q, %v", out.String(), err)
	}
	// The one that fires: an ordinary answer.
	r.Instance = "/dev/sda"
	w, _ = callAdmin(t, serveSend, http.MethodPost, r)
	res = SendResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	out.Reset()
	if err := sendAnswer(&out, res); err != nil || res.ResolvedNothing || out.String() != "sent external:smartd/box/disk_failing//dev/sda\n" {
		t.Fatalf("the matching resolve: %s; said %q, %v", w.Body.String(), out.String(), err)
	}

	// A dropped channel, and none left.
	cfg := te.active.cfg
	te.active.cfg = &resolved{channels: map[string]Channel{}, apps: cfg.apps}
	if _, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"}); apiStatus(err) != http.StatusUnprocessableEntity ||
		!strings.Contains(err.Error(), "channel ops was dropped from this load") || !strings.Contains(err.Error(), "systemctl reload motel") {
		t.Fatalf("a dropped channel: %v", err)
	}
	if _, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "nosuch"}); err == nil || !strings.Contains(err.Error(), "channels are none in this load") {
		t.Fatalf("an unknown channel with none left: %v", err)
	}
	te.active.cfg = cfg

	// Not set up.
	useStateDir(t, filepath.Join(t.TempDir(), "absent"))
	w, err = callAdmin(t, serveSend, http.MethodPost, goodSend())
	if err != nil {
		t.Fatal(err)
	}
	res = SendResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if err := sendAnswer(&out, res); err == nil || !strings.Contains(err.Error(), "delivered nowhere") || !strings.Contains(err.Error(), "run motel monitor setup as root") {
		t.Fatalf("send before setup: %s; %v", w.Body.String(), err)
	}
	if lines := te.lines("monitor: event"); len(lines) == 0 {
		t.Fatal("the event was not journalled")
	}
	if _, err := callAdmin(t, serveTest, http.MethodPost, TestRequest{Channel: "ops"}); apiStatus(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "run motel monitor setup as root") {
		t.Fatalf("test before setup: %v", err)
	}
}

// TestServeSendRefusesAMaskedInstance: the instance is in the key, the
// labels and the answer, which the filter does not rewrite.
func TestServeSendRefusesAMaskedInstance(t *testing.T) {
	te := withEngine(t)
	r := goodSend()
	r.Instance = "https://hooks.slack.com/services/T0/B0/secretpart" // gitleaks:allow
	_, err := callAdmin(t, serveSend, http.MethodPost, r)
	if apiStatus(err) != http.StatusUnprocessableEntity || strings.Contains(err.Error(), "secretpart") {
		t.Fatalf("a channel URL as the instance: %v", err)
	}
	if len(te.queue) != 0 || len(te.lines("monitor: event")) != 0 {
		t.Fatal("a refused event was journalled or queued")
	}
}

func TestServeSendCarriesDetails(t *testing.T) {
	te := withEngine(t)
	r := goodSend()
	r.Details = map[string]string{"serial": "XYZ", "temp": "61"}
	if _, err := callAdmin(t, serveSend, http.MethodPost, r); err != nil {
		t.Fatal(err)
	}
	got := te.drain()
	if len(got) != 1 || got[0].Annotations["detail_serial"] != "XYZ" || got[0].Annotations["detail_temp"] != "61" {
		t.Fatalf("posts = %+v", got)
	}
}

// fakeSystem records what setup does.
type fakeSystem struct {
	cmds        []string
	isInstalled bool
	// failOn fails every command that begins with one of its
	// |-separated prefixes.
	failOn string
	owners map[string][2]int
	// monitor is the running config's monitor app ("" for none), or
	// monitorErr motel's silence.
	monitor    string
	monitorErr error
}

func (f *fakeSystem) run(name string, args ...string) error {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	if f.failOn != "" && slices.ContainsFunc(strings.Split(f.failOn, "|"), func(p string) bool { return strings.HasPrefix(f.cmds[len(f.cmds)-1], p) }) {
		return errors.New("exit status 1")
	}
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
func (f *fakeSystem) loadedMonitor() ([]byte, error) {
	if f.monitorErr != nil {
		return nil, f.monitorErr
	}
	if f.monitor == "" {
		return nil, nil
	}
	return []byte(f.monitor), nil
}

// TestSetupSaysWhatTheLoadedConfigDoes: setup's last line is what the
// running config does with what it set up, never a promise of a render
// or a push its config has no line for.
func TestSetupSaysWhatTheLoadedConfigDoes(t *testing.T) {
	const channels = `{"channels":[{"name":"ops","kind":"ntfy","secret":"NOTIFY_OPS_URL"}],"operator":["ops"]}`
	for _, c := range []struct {
		monitor string
		err     error
		metrics bool
		want    string
	}{
		{channels, nil, false, "reloaded motel, which renders the monitor block's channels into /var/lib/motel-notify/alertmanager.yml"},
		{"", nil, false, "whose config has no monitor block with channels: Alertmanager sends nothing until there is one"},
		{`{"metrics":{}}`, nil, false, "whose config has no monitor block with channels"},
		{`{"metrics":{}}`, nil, true, "reloaded motel, which renders the store's rules into /var/lib/motel-metrics/rules.yml and pushes"},
		{channels, nil, true, "whose config has no monitor block with a metrics line: nothing is pushed until there is one"},
		{"", errors.New("dial unix /run/motel/admin.sock: connect: no such file or directory"), false, "motel did not answer on its admin socket, unix//run/motel/admin.sock (dial unix /run/motel/admin.sock: connect: no such file or directory): it is not running, or its Caddyfile sets another admin address or none"},
	} {
		got := loadedSays(&fakeSystem{monitor: c.monitor, monitorErr: c.err}, c.metrics)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s (metrics %v): %q, want %q", c.monitor, c.metrics, got, c.want)
		}
	}
}

// A re-run that fails after the mask must not leave Alertmanager
// masked: its socket could then start nothing. It is not started here;
// its socket does that at the next connection.
func TestSetupFailingUnmasksAlertmanager(t *testing.T) {
	root := t.TempDir()
	if err := setup(&fakeSystem{}, root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	f := &fakeSystem{isInstalled: true, failOn: "systemctl daemon-reload"}
	if err := setup(f, root, &bytes.Buffer{}); err == nil {
		t.Fatal("the re-run did not fail")
	}
	want := []string{
		"systemctl mask " + amService,
		"systemctl stop " + amService,
		"systemctl daemon-reload", // fails
		"systemctl daemon-reload", // and again: the rest is still tried
		"systemctl unmask " + amService,
		// A connection during the mask fails the socket; started, it
		// listens again.
		"systemctl start " + amSocket,
	}
	if !slices.Equal(f.cmds, want) {
		t.Fatalf("commands:\n%s\nwant\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	first := &fakeSystem{failOn: "systemctl daemon-reload"}
	if err := setup(first, t.TempDir(), &bytes.Buffer{}); err == nil || slices.Contains(first.cmds, "systemctl unmask "+amService) {
		t.Fatalf("a failed first run: err %v, commands %v", err, first.cmds)
	}
}

func TestSetupOrderAndFiles(t *testing.T) {
	root := t.TempDir()
	fs0 := &fakeSystem{}
	var out bytes.Buffer
	if err := setup(fs0, root, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl mask prometheus-alertmanager.service",
		"systemctl stop prometheus-alertmanager.service",
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
		if !bytes.HasPrefix(b, []byte("# Written by `motel monitor setup`")) {
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
	for _, want := range []string{"ExecStart=\n", "--web.systemd-socket", "--config.file=" + DefaultStateDir + "/" + ConfigName, "--cluster.listen-address= ", "receiver-name-in-metrics", "Requires=motel-am.socket", "ExecStartPre=+/usr/bin/chmod 0750 " + amStorage + "\n"} {
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
