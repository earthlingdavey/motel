package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/client/internal/local"
	"github.com/smallhoursorg/motel/client/internal/sshkey"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The hostile strings a box can send: every one
// must reach the terminal made visible, never acted on. Written with
// \x escapes so no control character is in this file itself.
var hostile = []string{
	"osc52 \x1b]52;c;cHduZWQ=\x07",
	"csi \x1b[2J\x1b[H",
	"osc8 \x1b]8;;https://evil.example/\x1b\\click\x1b]8;;\x1b\\",
	"title \x1b]0;pwned\x07",
	"tags [red]alert[-] [\"region\"]",
	"bidi abc\xe2\x80\xaedef",
	"c1 \xc2\x9b31mred",
	"fake ::error::file=x::owned",
	"bad utf8 \xff\xfe",
	"marks alm\xd8\x9c lrm\xe2\x80\x8e rlm\xe2\x80\x8f",
}

func pf(f float64) *float64 { return &f }

func hostileStatus() wire.Status {
	at := time.Date(2026, 11, 13, 17, 40, 11, 0, time.UTC)
	return wire.Status{
		App: "blog", Phase: "idle", CurrentVersion: "v42", Unit: "motel-blog.v42.0123456789abcdef.service", PID: 41233, Running: true,
		Deploys: []wire.DeploySummary{
			{Version: "v42", Status: "succeeded", By: hostile[0] + hostile[9], FinishedAt: json.RawMessage(`"2026-11-14T09:12:30Z"`)},
			{Version: "v41", Status: "failed", Phase: "soaking", By: hostile[2] + "  v40  succeeded", FinishedAt: json.RawMessage(`"` + "\\u001b[31m" + `"`)},
		},
		LastDeploy:        &wire.DeployResult{Version: "v42", Status: "failed", Error: hostile[3], By: hostile[5], Note: strings.Repeat("n", 300), FinishedAt: at},
		Watchdog:          &wire.Watchdog{State: "watching", LastFailure: hostile[4] + strings.Repeat(" ", 60) + "error        forged"},
		AvailableVersions: []string{"v42", "v41", "v40_rc.1"},
		Metrics: &wire.Metrics{Window: "1h", Memory: &wire.MemorySummary{Now: pf(182 << 20), Max: pf(210 << 20)},
			CPU: &wire.CPUSummary{Avg: 0.02}, Requests: &wire.RequestSummary{Total: 1234, PerSecond: 0.34, Share5xx: pf(0.001), P95: pf(120)}},
	}
}

func hostileDeploy() wire.DeployResult {
	start := time.Date(2026, 11, 13, 17, 39, 2, 0, time.UTC)
	return wire.DeployResult{
		Version: "v41", Status: "failed", Phase: "soaking", Error: "the health check answered 500 " + hostile[0],
		By: "github:example/blog@refs/heads/main", StartedAt: start, FinishedAt: start.Add(69 * time.Second),
		Phases: []wire.PhaseTiming{{Name: "downloading", Seconds: 2.1}, {Name: "soaking", Seconds: 65.4}},
		Detail: &wire.DeployDetail{
			Exit:    "exit status 3 " + hostile[5],
			Probe:   &wire.ProbeDetail{Status: 500, Location: hostile[2], Body: "{\"db\":\"connection refused\"}\n" + hostile[7]},
			LogTail: append([]string{"pg: connection refused (127.0.0.1:5432)", "two\nlines " + hostile[6], strings.Repeat("L", 500)}, hostile...),
		},
	}
}

// fakeBox answers as liveswap_webhook and box_webhook do, from the
// wire types, encoded as the handlers encode them.
type fakeBox struct {
	srv     *httptest.Server
	status  wire.Status
	deploy  wire.DeployResult
	whoami  wire.WhoAmI
	apps    wire.Apps
	backups wire.Backups
	alerts  wire.Alerts
	auth    []string
	bodies  map[string][]byte // what each target was answered
}

func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func newFakeBox(t *testing.T) *fakeBox {
	t.Helper()
	fb := &fakeBox{status: hostileStatus(), deploy: hostileDeploy(), whoami: hostileWhoAmI(), apps: wire.Apps{Apps: []wire.AppEntry{{Name: "blog"}, {Name: "shop-2"}}}, backups: hostileBackups(), bodies: map[string][]byte{}}
	fb.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.auth = append(fb.auth, r.Header.Get("Authorization"))
		code, body := http.StatusOK, []byte(nil)
		switch {
		case r.URL.Path == "/_motel/whoami":
			body = encode(fb.whoami)
		case r.URL.Path == "/_motel/apps":
			body = encode(fb.apps)
		case r.URL.Path == "/_motel/apps/blog/alerts":
			body = encode(fb.alerts)
		case r.URL.Path == "/_motel/backups":
			body = encode(fb.backups)
		case r.URL.Path == "/":
			body = encode(wire.BoxStatus{Commit: "3f9c2a1e" + strings.Repeat("0", 32), BoxWebhook: "127.0.0.1", SHA256: strings.Repeat("9b", 32), Recipient: "age1" + hostile[0]})
		case r.URL.Path != "/blog":
			code, body = http.StatusUnauthorized, []byte(`{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}`+"\n")
		case r.URL.Query().Get("deploy") == "v41":
			body = encode(fb.deploy)
		case r.URL.Query().Has("deploy"):
			code, body = http.StatusNotFound, encode(wire.Error{Error: "no deploy recorded for version " + r.URL.Query().Get("deploy")})
		default:
			body = encode(fb.status)
		}
		fb.bodies[r.URL.RequestURI()] = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

// noPrograms is a machine with no git, ssh-add or ssh-keygen.
type noPrograms struct{}

func (noPrograms) Output(context.Context, local.Cmd) ([]byte, error) { return nil, exec.ErrNotFound }

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, fb *fakeBox, env map[string]string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	e := Env{
		Getenv:  func(k string) string { return env[k] },
		Stdout:  &out,
		Stderr:  &errOut,
		Dir:     t.TempDir(),
		Home:    t.TempDir(),
		Now:     time.Now,
		Sleep:   func(context.Context, time.Duration) error { return nil },
		Run:     noPrograms{},
		Version: "v0.0.0-test",
	}
	if h, ok := env["HOME"]; ok {
		e.Home = h
	}
	if fb != nil {
		e.Transport = fb.srv.Client().Transport
	}
	code := Main(context.Background(), args, e)
	return result{code, out.String(), errOut.String()}
}

func appURL(fb *fakeBox, app string) string { return fb.srv.URL + "/" + app }

var deployToken = map[string]string{"MOTEL_TOKEN": "a.deploy.token"}

// sameJSON fails unless got and want are the same JSON value.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the box's body is not JSON: %v\n%q", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("stdout\n%q\nis not the value of the box's\n%q", got, want)
	}
}

// --json prints the box's own answer: the same JSON value, and byte
// for byte when the body holds nothing a terminal acts on; deploys
// <app> prints the status's deploys array. The fake box's bodies hold
// the hostile strings as Go's encoder writes them, a C1 control and a
// bidi override raw among them, and none reaches stdout raw.
func TestJSONIsTheBoxsAnswer(t *testing.T) {
	fb := newFakeBox(t)
	cases := []struct {
		args   []string
		target string
		want   func([]byte) string
	}{
		{[]string{"status", appURL(fb, "blog"), "--json"}, "/blog", nil},
		{[]string{"status", appURL(fb, "blog"), "--range", "24h", "--json"}, "/blog?range=24h", nil},
		{[]string{"deploys", appURL(fb, "blog"), "v41", "--json"}, "/blog?deploy=v41", nil},
		{[]string{"box", "status", "--json"}, "/", nil},
		{[]string{"deploys", appURL(fb, "blog"), "--json"}, "/blog", func(body []byte) string {
			var raw struct{ Deploys json.RawMessage }
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			return string(raw.Deploys) + "\n"
		}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args[:len(c.args)-1], " "), func(t *testing.T) {
			env := map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": fb.srv.URL + "/blog"}
			r := run(t, fb, env, c.args...)
			if r.code != 0 || r.stderr != "" {
				t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
			}
			body, ok := fb.bodies[c.target]
			if !ok {
				t.Fatalf("nothing asked for %s: %v", c.target, fb.bodies)
			}
			want := string(body)
			if c.want != nil {
				want = c.want(body)
			}
			sameJSON(t, r.stdout, want)
			if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
				t.Errorf("stdout holds %q, or is not UTF-8:\n%q", bad, r.stdout)
			}
		})
	}
	// A body with nothing to escape is the box's bytes.
	fb.status = wire.Status{App: "blog", Phase: "idle", CurrentVersion: "v42", AvailableVersions: []string{"v42"}, LastDeploy: &wire.DeployResult{Version: "v42", Status: "succeeded", By: "caf\u00e9 \x1b[2J"}}
	if r := run(t, fb, deployToken, "status", appURL(fb, "blog"), "--json"); r.code != 0 || r.stdout != string(fb.bodies["/blog"]) {
		t.Errorf("a body with nothing to escape: exit %d\n%q\nwant the box's bytes\n%q", r.code, r.stdout, fb.bodies["/blog"])
	}
	if got := fb.auth[0]; got != "Bearer a.deploy.token" {
		t.Errorf("Authorization %q, want MOTEL_TOKEN as Bearer", got)
	}
}

// TestJSONWritesNothingATerminalActsOn: valid JSON can hold, raw in a
// string, a C1 control (CSI and OSC in one character each), DEL, a
// bidi override and bytes that are not UTF-8, and a carriage return
// between its values. --json writes each as JSON's escape, or drops
// the carriage return, in a 200's body and in a refusal's.
func TestJSONWritesNothingATerminalActsOn(t *testing.T) {
	fb := newFakeBox(t)
	const raw = "c1 \u009b2J osc \u009d52;c;cHduZWQ=\u009c del \x7f bidi abc\u202edef bad \xff"
	const escaped = `c1 \u009b2J osc \u009d52;c;cHduZWQ=\u009c del \u007f bidi abc\u202edef bad \ufffd`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Query().Has("deploy"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("{\"error\":\"" + raw + "\"}\r\n"))
		case r.URL.Path == "/shop":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("{\"error\":\"" + raw + "\",\r\"needs\":\"viewer\",\"scope\":\"app\"}"))
		default:
			_, _ = w.Write([]byte("{\"app\":\"blog\",\r\n\"phase\":\"idle\",\"added_later\":\"" + raw + "\",\"deploys\":[{\"version\":\"v1\",\"status\":\"succeeded\",\"deployed_by\":\"" + raw + "\"}]}\r\n"))
		}
	}))
	t.Cleanup(srv.Close)
	fb.srv = srv
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"a status":          {[]string{"status", srv.URL + "/blog"}, 0, "{\"app\":\"blog\",\n\"phase\":\"idle\",\"added_later\":\"" + escaped + "\",\"deploys\":[{\"version\":\"v1\",\"status\":\"succeeded\",\"deployed_by\":\"" + escaped + "\"}]}\n"},
		"the deploys array": {[]string{"deploys", srv.URL + "/blog"}, 0, "[{\"version\":\"v1\",\"status\":\"succeeded\",\"deployed_by\":\"" + escaped + "\"}]\n"},
		"a 404's body":      {[]string{"deploys", srv.URL + "/blog", "v9"}, 6, "{\"error\":\"" + escaped + "\"}\n"},
		"a 403's body":      {[]string{"status", srv.URL + "/shop"}, 5, "{\"error\":\"" + escaped + "\",\"needs\":\"viewer\",\"scope\":\"app\"}\n"},
	} {
		r := run(t, fb, deployToken, append(c.args, "--json")...)
		if r.code != c.code || r.stdout != c.want {
			t.Errorf("%s: exit %d\n%q\nwant %d and\n%q\nstderr %s", name, r.code, r.stdout, c.code, c.want, r.stderr)
		}
		if bad := unsafeOutput.FindString(r.stdout + r.stderr); bad != "" || !utf8.ValidString(r.stdout) {
			t.Errorf("%s: the output holds %q, or is not UTF-8", name, bad)
		}
	}
}

func TestJSONDeploysWithNone(t *testing.T) {
	fb := newFakeBox(t)
	fb.status.Deploys = nil
	r := run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "--json")
	if r.code != 0 || r.stdout != "[]\n" {
		t.Fatalf("exit %d, stdout %q, want [] for no deploys", r.code, r.stdout)
	}
}

// Every box string reaches the terminal through termtext: the human
// output holds no control character, no C1 and no bidi control, and
// is what the goldens say.
func TestHumanOutputOfHostileStrings(t *testing.T) {
	fb := newFakeBox(t)
	for _, c := range []struct {
		golden string
		args   []string
	}{
		{"status", []string{"status", appURL(fb, "blog")}},
		{"deploys", []string{"deploys", appURL(fb, "blog")}},
		{"deploy", []string{"deploys", appURL(fb, "blog"), "v41"}},
		{"box", []string{"box", "status"}},
	} {
		t.Run(c.golden, func(t *testing.T) {
			// The box's port is in MOTEL_URL, for box status.
			env := map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": fb.srv.URL + "/blog"}
			r := run(t, fb, env, c.args...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			if bad := unsafeOutput.FindString(r.stdout); bad != "" || !utf8.ValidString(r.stdout) {
				t.Errorf("the output holds %q, or is not UTF-8:\n%s", bad, r.stdout)
			}
			golden(t, "hostile-"+c.golden, r.stdout)
		})
	}
}

// box status says, on its first line, that the Caddyfile is not the
// one the commit installed, when the box says so (#577); a
// box that says nothing (one older than the field, or a file as
// applied) is said as before. --json is the box's body, the field in it.
func TestBoxStatusSaysAnEditedCaddyfile(t *testing.T) {
	for _, c := range []struct {
		edited bool
		first  string
	}{
		{true, "runs aaaaaaaaaaaa, and its Caddyfile is not the one that commit installed\n"},
		{false, "runs aaaaaaaaaaaa\n"},
	} {
		var body []byte
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		body = encode(wire.BoxStatus{Commit: strings.Repeat("a", 40), BoxWebhook: "127.0.0.1", SHA256: strings.Repeat("b", 64), OutOfBand: c.edited})
		fb := &fakeBox{srv: srv, bodies: map[string][]byte{}}
		env := map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": srv.URL + "/blog"}
		r := run(t, fb, env, "box", "status")
		want := "127.0.0.1  " + c.first + "commit           " + strings.Repeat("a", 40) + "\nCaddyfile sha256 " + strings.Repeat("b", 64) + "\n"
		if r.code != 0 || r.stdout != want {
			t.Errorf("edited %v: exit %d, stdout\n%s\nwant\n%s", c.edited, r.code, r.stdout, want)
		}
		if r := run(t, fb, env, "box", "status", "--json"); r.code != 0 || r.stdout != string(body) || strings.Contains(r.stdout, "caddyfile_edited_out_of_band") != c.edited {
			t.Errorf("edited %v, --json: %+v, want %s", c.edited, r, body)
		}
	}
}

// unsafeOutput is any C0 control but the line feed, DEL, a C1 control,
// a bidi control (the whole Bidi_Control set), U+2028 or U+2029.
var unsafeOutput = regexp.MustCompile(`[\x00-\x09\x0b-\x1f\x7f\x{80}-\x{9f}\x{061c}\x{200e}\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}\x{2028}\x{2029}]`)

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -run %s -update writes it)", err, t.Name())
	}
	if got != string(want) {
		t.Errorf("output differs from %s:\n%s\nwant:\n%s", path, got, want)
	}
}

func TestBoxAnswersFailure(t *testing.T) {
	fb := newFakeBox(t)
	r := run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "v9")
	if r.code != 6 || r.stdout != "" || r.stderr != "motel: 127.0.0.1 answered 404; the box says: \"no deploy recorded for version v9\"\n" {
		t.Fatalf("%+v", r)
	}
	r = run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "v9", "--json")
	if r.code != 6 || r.stdout != `{"error":"no deploy recorded for version v9"}`+"\n" {
		t.Fatalf("--json prints the box's 404 body: %+v", r)
	}
	// An unknown app answers 401 to a deploy token, like a bad token.
	// The box's 401 body is not printed: --json is an object whose one
	// key is error, holding the client's own sentence, the one on stderr.
	r = run(t, fb, deployToken, "status", appURL(fb, "ghost"), "--json")
	var got map[string]string
	said := strings.TrimSuffix(strings.TrimPrefix(r.stderr, "motel: "), "\n")
	if r.code != 4 || json.Unmarshal([]byte(r.stdout), &got) != nil || len(got) != 1 || got["error"] != said ||
		!strings.HasPrefix(said, "127.0.0.1 refused the token in MOTEL_TOKEN") {
		t.Fatalf("a refused token: %+v", r)
	}
}

// A status the box's own filter replaced values in is read, and each
// such value is shown in the client's words, with nothing of what the
// box sent in it; the app and a record's version are the ones asked for
// when the box's equal them but at a marker.
func TestStatusTheFilterReplacedValuesIn(t *testing.T) {
	fb := newFakeBox(t)
	fb.status = wire.Status{
		App: "bl[redacted:APP_ENV]", Phase: "idle", CurrentVersion: "v42", PID: 41233, Running: true,
		Unit:              "motel-[redacted:DB_NAME].v42.0123456789abcdef.service motel: forged",
		Watchdog:          &wire.Watchdog{State: "[redacted:FLAG]"},
		Deploys:           []wire.DeploySummary{{Version: "v42", Status: "succeeded"}, {Version: "v[redacted:X]", Status: "failed", Phase: "soaking"}},
		AvailableVersions: []string{"v42", "v[redacted:X]"},
	}
	r := run(t, fb, deployToken, "status", appURL(fb, "blog"))
	const hidden = "[hidden by the box's filter]"
	if r.code != 0 || r.stderr != "" || !strings.HasPrefix(r.stdout, "blog  running v42  unit "+hidden+"  pid 41233\n") ||
		!strings.Contains(r.stdout, "versions     v42 (running) "+hidden+"\n") || strings.Contains(r.stdout, "redacted:") || strings.Contains(r.stdout, "forged") {
		t.Fatalf("status: %+v", r)
	}
	if n := strings.Count(r.stdout, hidden); n != 3 { // the unit, the watchdog's state, a version
		t.Errorf("%d values shown as hidden, want 3:\n%s", n, r.stdout)
	}
	r = run(t, fb, deployToken, "deploys", appURL(fb, "blog"))
	if r.code != 0 || !strings.Contains(r.stdout, hidden) || strings.Contains(r.stdout, "redacted:") {
		t.Fatalf("deploys: %+v", r)
	}
	// --json is the box's own answer, markers and all.
	r = run(t, fb, deployToken, "status", appURL(fb, "blog"), "--json")
	if r.code != 0 || r.stdout != string(fb.bodies["/blog"]) {
		t.Fatalf("--json: %+v", r)
	}
	// Another app's status is still another app's.
	fb.status.App = "sh[redacted:APP_ENV]"
	if r := run(t, fb, deployToken, "status", appURL(fb, "blog")); r.code != 3 || !strings.Contains(r.stderr, "it is the status of another app") {
		t.Fatalf("another app's status behind a marker: %+v", r)
	}

	// One record: the version asked for is the one shown.
	fb.deploy = wire.DeployResult{Version: "v[redacted:X]", Status: "succeeded"}
	r = run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "v41")
	if r.code != 0 || !strings.Contains(r.stdout, "v41") || strings.Contains(r.stdout, "redacted:") {
		t.Fatalf("one record: %+v", r)
	}
	fb.deploy.Version = "w[redacted:X]"
	if r := run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "v41"); r.code != 3 || !strings.Contains(r.stderr, "it is the record of another version") {
		t.Fatalf("another version's record behind a marker: %+v", r)
	}
}

func TestUsage(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no app", deployToken, []string{"status"}, "accepts 1 arg(s), received 0; motel status --help says more"},
		{"too many", deployToken, []string{"deploys", "a", "b", "c"}, "accepts between 1 and 2 arg(s), received 3"},
		{"bad app", deployToken, []string{"status", "Blog"}, `"Blog" is not an app name`},
		{"bad version", deployToken, []string{"deploys", "blog", ".v1", "--box", "deploy.example.com"}, `".v1" is not a version`},
		{"bad range", deployToken, []string{"status", "blog", "--range", "7d", "--box", "deploy.example.com"}, `--range "7d" is not one of 1h, 24h`},
		{"no box", deployToken, []string{"status", "blog"}, "name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository"},
		{"no key", nil, []string{"status", "blog", "--box", "deploy.example.com"}, "name your key: --key ~/.ssh/<key>.pub or MOTEL_KEY"},
		{"http", deployToken, []string{"status", "http://deploy.example.com/blog"}, "is not an https:// URL"},
		{"unknown flag", deployToken, []string{"status", "blog", "--insecure"}, "unknown flag: --insecure"},
		{"unknown command", nil, []string{"deploy", "blog"}, `unknown command "deploy" for "motel"`},
		{"box alone", nil, []string{"box"}, "box needs a command: motel box status"},
		{"a hostile argument", deployToken, []string{"status", "x\x1b]0;t\x07"}, `is not an app name`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := run(t, nil, c.env, c.args...)
			if r.code != 2 || !strings.Contains(r.stderr, c.want) || r.stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 2 and %q", r.code, r.stdout, r.stderr, c.want)
			}
			if strings.ContainsRune(r.stderr, '\x1b') {
				t.Errorf("stderr holds an ESC: %q", r.stderr)
			}
			r = run(t, nil, c.env, append(c.args, "--json")...)
			var e struct{ Error string }
			if r.code != 2 || json.Unmarshal([]byte(r.stdout), &e) != nil || !strings.Contains(e.Error, c.want) {
				t.Errorf("--json: exit %d, stdout %q", r.code, r.stdout)
			}
		})
	}
}

func TestHelpMarksEveryCommandExperimental(t *testing.T) {
	r := run(t, nil, nil, "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "Experimental commands:") || !strings.Contains(r.stdout, "Every command is experimental") {
		t.Fatalf("%+v", r)
	}
	for _, cmd := range [][]string{{"version"}, {"key"}, {"whoami"}, {"apps"}, {"status"}, {"deploys"}, {"box"}, {"box", "status"}} {
		r := run(t, nil, nil, append(cmd, "--help")...)
		if r.code != 0 || !strings.Contains(r.stdout, "(experimental)") && !strings.Contains(r.stdout, "Every command is experimental") {
			t.Errorf("%v --help does not say experimental:\n%s", cmd, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "completion") || strings.Contains(r.stdout, "rollback") {
		t.Errorf("help lists a command this part does not have:\n%s", r.stdout)
	}
}

func TestVersion(t *testing.T) {
	r := run(t, nil, nil, "version")
	if r.code != 0 || r.stdout != "motel v0.0.0-test\nprotocol 1 (person token m1)\n" {
		t.Fatalf("%+v", r)
	}
	r = run(t, nil, nil, "version", "--json")
	if r.code != 0 || r.stdout != `{"version":"v0.0.0-test","protocols":[1],"token_forms":["m1"]}`+"\n" {
		t.Fatalf("%+v", r)
	}
}

func TestKey(t *testing.T) {
	home := t.TempDir()
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn" // gitleaks:allow — made up for this test
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte(line+" alice@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rsa := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAAQC7" // gitleaks:allow — made up for this test
	if err := os.WriteFile(filepath.Join(home, "id_rsa.pub"), []byte(rsa+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home}
	r := run(t, nil, env, "key", "--name", "alice@example.com")
	want := "key  ~/.ssh/id_ed25519.pub (the default file)  SHA256:VKOVWJR/wCqOglzLV3//JOoCbHbUSBD6npnXCox7/P4\n" +
		"Send this line to the box's operator, for the box block of its Caddyfile:\n" +
		"    person alice@example.com " + line + "\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("exit %d\n%s\nwant\n%s", r.code, r.stdout, want)
	}
	r = run(t, nil, env, "key", "--json")
	var info KeyInfo
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &info) != nil || info.PersonLine != "person <name> "+line || info.Source != "the default file" {
		t.Fatalf("%+v", r)
	}
	r = run(t, nil, env, "key", "--key", "~/id_rsa.pub")
	if r.code != 2 || r.stderr != "motel: ~/id_rsa.pub is an RSA key. A box takes ed25519 keys, including hardware ones, not RSA: make one with ssh-keygen -t ed25519.\n" {
		t.Fatalf("an RSA key: %+v", r)
	}
	// An ECDSA key prints no person line a box would refuse: it is
	// refused, as RSA is, with the remedy.
	ec := "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBDwDfrEFh7S+4W3sDNtlO0anzlUu3vMU7X2BFcjr0e8A4abZE2yeaKs/RS/NEixorLYz0QmWu/Ms6E1WAdjjDrI=" // gitleaks:allow — made up for this test
	if err := os.WriteFile(filepath.Join(home, "id_ecdsa.pub"), []byte(ec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ecRefusal := "~/id_ecdsa.pub (--key, SHA256:2V9I6P3ozgWx2JpYaQK7AK8DiYFppveucXuFQ7NFEXk) is an ecdsa-sha2-nistp256 key. A box takes Ed25519 keys for now, not ECDSA yet: make one with ssh-keygen -t ed25519 (or -t ed25519-sk for a hardware key) and name it with --key or MOTEL_KEY."
	r = run(t, nil, env, "key", "--key", "~/id_ecdsa.pub")
	if r.code != 2 || r.stdout != "" || r.stderr != "motel: "+ecRefusal+"\n" {
		t.Fatalf("an ECDSA key: %+v", r)
	}
	r = run(t, nil, env, "key", "--key", "~/id_ecdsa.pub", "--json", "--name", "alice@example.com")
	var refused struct{ Error string }
	if r.code != 2 || json.Unmarshal([]byte(r.stdout), &refused) != nil || refused.Error != ecRefusal || strings.Contains(r.stdout, "person") {
		t.Fatalf("an ECDSA key, --json: %+v", r)
	}
	r = run(t, nil, env, "key", "--name", "alice example")
	if r.code != 2 {
		t.Fatalf("a name a box would refuse: %+v", r)
	}
}

// With no MOTEL_TOKEN, the request carries a person's token, signed by
// the key Resolve found.
type fixedSigner struct{ calls int }

func (s *fixedSigner) Sign(context.Context, sshkey.Key, string, []byte) ([]byte, error) {
	s.calls++
	return []byte("-----BEGIN SSH SIGNATURE-----\nU1NIU0lH\n-----END SSH SIGNATURE-----\n"), nil
}

func TestPersonToken(t *testing.T) {
	fb := newFakeBox(t)
	home := t.TempDir()
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn" // gitleaks:allow — made up for this test
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	s := &fixedSigner{}
	code := Main(context.Background(), []string{"status", appURL(fb, "blog"), "--key", "~/me.pub", "--json"}, Env{
		Getenv: func(string) string { return "" }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
		Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: s, Version: "test",
	})
	if code != 0 || s.calls != 1 || !strings.HasPrefix(fb.auth[0], "Motel m1.") {
		t.Fatalf("exit %d, %d signatures, Authorization %q, stderr %s", code, s.calls, fb.auth[0], errOut.String())
	}
}

// An interrupted command exits 130.
func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := Main(ctx, []string{"status", "blog", "--box", "deploy.example.com"}, Env{
		Getenv: func(k string) string { return deployToken[k] }, Stdout: &out, Stderr: &errOut,
		Now: time.Now, Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }, Run: noPrograms{},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }),
	})
	if code != 130 {
		t.Fatalf("exit %d, want 130; stderr %s", code, errOut.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return f(r)
}

// Which credential: a --key typed beats MOTEL_TOKEN; MOTEL_TOKEN beats
// MOTEL_KEY and every key the client finds by itself.
func TestCredentialOrder(t *testing.T) {
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn" // gitleaks:allow — made up for this test
	home := t.TempDir()
	for _, f := range []string{"me.pub", ".ssh/id_ed25519.pub"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, f)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, f), []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		env    map[string]string
		flags  []string
		bearer bool
	}{
		{"--key beats MOTEL_TOKEN", map[string]string{"MOTEL_TOKEN": "a.deploy.token"}, []string{"--key", "~/me.pub"}, false},
		{"MOTEL_TOKEN beats MOTEL_KEY", map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_KEY": "~/me.pub"}, nil, true},
		{"MOTEL_TOKEN beats the default file", map[string]string{"MOTEL_TOKEN": "a.deploy.token"}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb := newFakeBox(t)
			var out, errOut bytes.Buffer
			code := Main(context.Background(), append([]string{"status", appURL(fb, "blog"), "--json"}, c.flags...), Env{
				Getenv: func(k string) string { return c.env[k] }, Stdout: &out, Stderr: &errOut, Dir: t.TempDir(), Home: home,
				Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
				Transport: fb.srv.Client().Transport, Run: noPrograms{}, Signer: &fixedSigner{}, Version: "test",
			})
			if code != 0 || len(fb.auth) != 1 || strings.HasPrefix(fb.auth[0], "Bearer ") != c.bearer {
				t.Fatalf("exit %d, Authorization %v, stderr %s; want Bearer: %v", code, fb.auth, errOut.String(), c.bearer)
			}
		})
	}
}

// A MOTEL_TOKEN that cannot be a header is refused before anything is
// sent, and never echoed.
func TestBadDeployToken(t *testing.T) {
	fb := newFakeBox(t)
	r := run(t, fb, map[string]string{"MOTEL_TOKEN": "s3cr3t\nX-Evil: 1"}, "status", appURL(fb, "blog"))
	if r.code != 2 || len(fb.auth) != 0 || strings.Contains(r.stderr, "s3cr3t") || !strings.Contains(r.stderr, "MOTEL_TOKEN holds something that is not a token") {
		t.Fatalf("exit %d, %d requests, stderr %q", r.code, len(fb.auth), r.stderr)
	}
}

// A key path from the environment reaches stderr made safe to print,
// in the error the file system gives too.
func TestHostileKeyPath(t *testing.T) {
	r := run(t, nil, map[string]string{"MOTEL_KEY": "~/evil\x1b]0;pwned\x07.pub"}, "status", "blog", "--box", "deploy.example.com")
	if r.code != 2 || strings.ContainsAny(r.stderr, "\x1b\x07") || !strings.Contains(r.stderr, `evil\x1b]0;pwned\x07.pub`) {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}

// The client is https only, with no way round it: no help text offers
// deploy.sh's MOTEL_ALLOW_HTTP.
func TestNoPlainHTTP(t *testing.T) {
	for _, cmd := range [][]string{{}, {"status"}, {"deploys"}, {"box", "status"}, {"key"}, {"version"}, {"whoami"}, {"apps"}} {
		r := run(t, nil, nil, append(cmd, "--help")...)
		if strings.Contains(r.stdout, "ALLOW_HTTP") || strings.Contains(r.stdout, "insecure") {
			t.Errorf("%v --help offers plain http:\n%s", cmd, r.stdout)
		}
	}
	if r := run(t, nil, map[string]string{"MOTEL_TOKEN": "t", "MOTEL_ALLOW_HTTP": "1"}, "status", "http://deploy.example.com/blog"); r.code != 2 {
		t.Fatalf("an http URL with MOTEL_ALLOW_HTTP=1: exit %d", r.code)
	}
}

// A command that resolves another box than MOTEL_URL's sends
// MOTEL_TOKEN nowhere.
func TestDeployTokenStaysWithItsBox(t *testing.T) {
	fb := newFakeBox(t)
	env := map[string]string{"MOTEL_TOKEN": "a.deploy.token", "MOTEL_URL": "https://deploy.example.com/blog"}
	for _, args := range [][]string{{"status", appURL(fb, "blog")}, {"status", "blog", "--box", "other.example.com"}} {
		r := run(t, fb, env, args...)
		if r.code != 2 || len(fb.auth) != 0 || !strings.Contains(r.stderr, "the token in MOTEL_TOKEN belongs with MOTEL_URL's box, deploy.example.com") {
			t.Fatalf("%v: exit %d, %d requests, stderr %q", args, r.code, len(fb.auth), r.stderr)
		}
	}
	env["MOTEL_URL"] = fb.srv.URL + "/blog"
	if r := run(t, fb, env, "status", appURL(fb, "blog")); r.code != 0 {
		t.Fatalf("MOTEL_URL's own box: exit %d %s", r.code, r.stderr)
	}
}

func TestRoots(t *testing.T) {
	got := roots(func(k string) string { return map[string]string{"SSL_CERT_DIR": "/etc/x"}[k] })
	if len(got) != 1 || got[0] != "SSL_CERT_DIR=/etc/x" {
		t.Fatalf("roots = %q", got)
	}
}
