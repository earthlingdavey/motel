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
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/smallhoursorg/motel/liveswap/wire"
	"github.com/smallhoursorg/motel/motelctl/internal/local"
	"github.com/smallhoursorg/motel/motelctl/internal/sshkey"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The hostile strings a box can send (motelctl-spec 5.5): every one
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
		App: "blog", Phase: "running", CurrentVersion: "v42" + hostile[6], Unit: "motel-app-blog.service" + hostile[1], PID: 41233, Running: true,
		Deploys: []wire.DeploySummary{
			{Version: "v42", Status: "succeeded", By: hostile[0] + hostile[9], FinishedAt: json.RawMessage(`"2026-11-14T09:12:30Z"`)},
			{Version: "v41", Status: "failed", Phase: "health_gate", By: hostile[2], FinishedAt: json.RawMessage(`"` + "\\u001b[31m" + `"`)},
		},
		LastDeploy:        &wire.DeployResult{Version: "v42", Status: "failed", Error: hostile[3], By: hostile[5], FinishedAt: at},
		Watchdog:          &wire.Watchdog{State: "ok", LastFailure: hostile[4]},
		AvailableVersions: []string{"v42" + hostile[6], "v41", hostile[8], hostile[9]},
		Metrics: &wire.Metrics{Window: "1h", Memory: &wire.MemorySummary{Now: pf(182 << 20), Max: pf(210 << 20)},
			CPU: &wire.CPUSummary{Avg: 0.02}, Requests: &wire.RequestSummary{Total: 1234, PerSecond: 0.34, Share5xx: pf(0.001), P95: pf(120)}},
	}
}

func hostileDeploy() wire.DeployResult {
	start := time.Date(2026, 11, 13, 17, 39, 2, 0, time.UTC)
	return wire.DeployResult{
		Version: "v41", Status: "failed", Phase: "health_gate", Error: "the health check answered 500 " + hostile[0],
		By: "github:example/blog@refs/heads/main", StartedAt: start, FinishedAt: start.Add(69 * time.Second),
		Phases: []wire.PhaseTiming{{Name: "fetch", Seconds: 2.1}, {Name: "health_gate" + hostile[1], Seconds: 65.4}},
		Detail: &wire.DeployDetail{
			Exit:    "exit status 3 " + hostile[5],
			Probe:   &wire.ProbeDetail{Status: 500, Location: hostile[2], Body: "{\"db\":\"connection refused\"}\n" + hostile[7]},
			LogTail: append([]string{"pg: connection refused (127.0.0.1:5432)", "two\nlines " + hostile[6]}, hostile...),
		},
	}
}

// fakeBox answers as liveswap_webhook and box_webhook do, from the
// wire types, encoded as the handlers encode them.
type fakeBox struct {
	srv    *httptest.Server
	status wire.Status
	deploy wire.DeployResult
	auth   []string
	bodies map[string][]byte // what each target was answered
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
	fb := &fakeBox{status: hostileStatus(), deploy: hostileDeploy(), bodies: map[string][]byte{}}
	fb.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.auth = append(fb.auth, r.Header.Get("Authorization"))
		code, body := http.StatusOK, []byte(nil)
		switch {
		case r.URL.Path == "/":
			body = encode(wire.BoxStatus{Commit: "3f9c2a1e" + strings.Repeat("0", 32), BoxWebhook: "deploy.example.com", SHA256: strings.Repeat("9b", 32), Recipient: "age1" + hostile[0]})
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

// --json prints the box's body as it came, byte for byte; deploys
// <app> prints the status's deploys array.
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
			if r.stdout != want {
				t.Errorf("stdout\n%q\nwant the box's\n%q", r.stdout, want)
			}
		})
	}
	if got := fb.auth[0]; got != "Bearer a.deploy.token" {
		t.Errorf("Authorization %q, want MOTEL_TOKEN as Bearer", got)
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
	if r.code != 6 || r.stdout != "" || r.stderr != "motelctl: 127.0.0.1: no deploy recorded for version v9\n" {
		t.Fatalf("%+v", r)
	}
	r = run(t, fb, deployToken, "deploys", appURL(fb, "blog"), "v9", "--json")
	if r.code != 6 || r.stdout != `{"error":"no deploy recorded for version v9"}`+"\n" {
		t.Fatalf("--json prints the box's 404 body: %+v", r)
	}
	// An unknown app answers 401 to a deploy token, like a bad token.
	r = run(t, fb, deployToken, "status", appURL(fb, "ghost"), "--json")
	if r.code != 4 || !strings.HasPrefix(r.stdout, `{"error":"127.0.0.1 refused the token in MOTEL_TOKEN`) {
		t.Fatalf("a refused token: %+v", r)
	}
}

func TestUsage(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no app", deployToken, []string{"status"}, "accepts 1 arg(s), received 0; motelctl status --help says more"},
		{"too many", deployToken, []string{"deploys", "a", "b", "c"}, "accepts between 1 and 2 arg(s), received 3"},
		{"bad app", deployToken, []string{"status", "Blog"}, `"Blog" is not an app name`},
		{"bad version", deployToken, []string{"deploys", "blog", ".v1", "--box", "deploy.example.com"}, `".v1" is not a version`},
		{"bad range", deployToken, []string{"status", "blog", "--range", "7d", "--box", "deploy.example.com"}, `--range "7d" is not one of 1h, 24h`},
		{"no box", deployToken, []string{"status", "blog"}, "name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository"},
		{"no key", nil, []string{"status", "blog", "--box", "deploy.example.com"}, "name your key: --key ~/.ssh/<key>.pub or MOTEL_KEY"},
		{"http", deployToken, []string{"status", "http://deploy.example.com/blog"}, "is not an https:// URL"},
		{"unknown flag", deployToken, []string{"status", "blog", "--insecure"}, "unknown flag: --insecure"},
		{"unknown command", nil, []string{"deploy", "blog"}, `unknown command "deploy" for "motelctl"`},
		{"box alone", nil, []string{"box"}, "box needs a command: motelctl box status"},
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
	for _, cmd := range [][]string{{"version"}, {"key"}, {"status"}, {"deploys"}, {"box"}, {"box", "status"}} {
		r := run(t, nil, nil, append(cmd, "--help")...)
		if r.code != 0 || !strings.Contains(r.stdout, "(experimental)") && !strings.Contains(r.stdout, "Every command is experimental") {
			t.Errorf("%v --help does not say experimental:\n%s", cmd, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "completion") || strings.Contains(r.stdout, "whoami") {
		t.Errorf("help lists a command this part does not have:\n%s", r.stdout)
	}
}

func TestVersion(t *testing.T) {
	r := run(t, nil, nil, "version")
	if r.code != 0 || r.stdout != "motelctl v0.0.0-test\nprotocol 1 (person token m1)\n" {
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
	if r.code != 2 || r.stderr != "motelctl: ~/id_rsa.pub is an RSA key. A box takes ed25519 and ecdsa keys, including hardware ones, not RSA: make one with ssh-keygen -t ed25519.\n" {
		t.Fatalf("an RSA key: %+v", r)
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
// MOTEL_KEY and every key motelctl finds by itself.
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

// motelctl is https only, with no way round it: no help text offers
// deploy.sh's MOTEL_ALLOW_HTTP.
func TestNoPlainHTTP(t *testing.T) {
	for _, cmd := range [][]string{{}, {"status"}, {"deploys"}, {"box", "status"}, {"key"}, {"version"}} {
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
