// Package examples holds the tests of what the examples ship that is
// not an example's own code: scripts/deploy.sh, which every example
// copies and which has to read a box's answer in every shape a box
// can give it; and, for the template repositories each release
// publishes them as, the links in their docs and the script that does
// the publishing.
package examples

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A stand-in box. "single" answers the single response a box without
// stream support (before #103) gives; "stream" streams phases and the
// terminal line; "fail" is a single 500; "cut" streams one phase and
// closes; "redirect" is a 3xx, what an intermediary answers;
// "truncated" promises a single 200 body and drops the connection
// before delivering it; "accepted" is a bodiless 202, what a queue in
// front of the box answers: a 2xx that is not a completed deploy;
// "refused" is the box's 401, the same flat sentence whatever the
// reason (liveswap/handler.go pins it); "refused-elsewhere" is a 401
// from something that is not the box; "wrongarch" streams the 422 the
// box's pre-flight gives a release built for another architecture
// (liveswap/elf.go words it); "refused-once" is the box's 401 to the
// first request and a deploy to every later one, as when the box was
// fetching the issuer's new signing key. "starting" is the box's 503
// while liveswap's Start has not installed the app (liveswap/handler.go
// words it), with Retry-After: 1; "starting-once" is that to the first
// request and a deploy to every later one; "starting-no-retry-after"
// lacks the header; "unavailable" is a 503 with Retry-After from
// something that is not the box. The stand-in also mints the run's
// OIDC token at /token, the way Actions does.
func box(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	srv, _ := countedBox(t, mode)
	return srv
}

// countedBox is box, and the number of deploy requests it has answered
// (/token is not one).
func countedBox(t *testing.T, mode string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	status := map[string]any{"app": "demo", "current_version": "v1", "running": true, "last_deploy": map[string]any{"version": "v1", "status": "succeeded"}}
	single := func(w http.ResponseWriter, code int, v any) {
		b, _ := json.Marshal(v)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(append(b, '\n'))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			single(w, 200, map[string]any{"value": "minted-" + r.URL.Query().Get("audience")})
			return
		}
		m := mode
		if first := n.Add(1) == 1; m == "refused-once" && first {
			m = "refused"
		} else if m == "refused-once" {
			m = "stream"
		}
		if m == "starting-once" && n.Load() == 1 {
			m = "starting"
		} else if m == "starting-once" {
			m = "stream"
		}
		const startingMessage = "motel is starting and has not loaded this app yet; retry after 1 s"
		switch m {
		case "single":
			single(w, 200, status)
		case "refused":
			single(w, 401, map[string]any{"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)"})
		case "refused-elsewhere":
			single(w, 401, map[string]any{"error": "who are you"})
		case "starting":
			w.Header().Set("Retry-After", "1")
			single(w, 503, map[string]any{"error": startingMessage})
		case "starting-no-retry-after":
			single(w, 503, map[string]any{"error": startingMessage})
		case "unavailable":
			w.Header().Set("Retry-After", "1")
			single(w, 503, map[string]any{"error": "upstream unavailable"})
		case "fail":
			single(w, 500, map[string]any{"error": "health gate: boom", "status": status})
		case "accepted":
			w.WriteHeader(http.StatusAccepted)
		case "wrongarch":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"at":"t","event":"phase","phase":"preparing"}` + "\n" +
				`{"error":"pre-flight: this box is amd64 and ./server is an AArch64 (arm64) executable: build on a runner of the box's architecture (runs-on: ubuntu-24.04)","event":"done","http_status":422}` + "\n"))
		case "redirect":
			http.Redirect(w, r, "https://elsewhere.test/demo", http.StatusMovedPermanently)
		case "truncated":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"app":"demo","current_vers`))
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			f := w.(http.Flusher)
			_, _ = w.Write([]byte(`{"at":"t","event":"phase","phase":"downloading"}` + "\n"))
			f.Flush()
			if m == "cut" {
				return
			}
			b, _ := json.Marshal(status)
			_, _ = w.Write(append(b[:len(b)-1], []byte(`,"event":"done","http_status":200}`+"\n")...))
		}
	})), &n
}

// The scripts stay byte-identical, and each reads every shape of
// answer to the same exit status.
func TestDeployShReadsEveryShapeOfAnswer(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	node, err := os.ReadFile(filepath.Join("node", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	deno, err := os.ReadFile(filepath.Join("deno", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(node) != string(deno) {
		t.Fatal("examples/node/scripts/deploy.sh and examples/deno/scripts/deploy.sh differ; they are one script")
	}
	cases := []struct {
		mode string
		exit int
		want string // in the output
		not  string // not in the output
	}{
		{"single", 0, `"current_version":"v1"`, ""},
		{"stream", 0, `"http_status":200`, ""},
		{"fail", 1, "health gate: boom", ""},
		{"cut", 1, `"phase":"downloading"`, ""},
		{"redirect", 1, "", ""},
		{"truncated", 1, "", ""},
		{"accepted", 1, "", ""},
		// A laptop deploy (MOTEL_TOKEN) refused: the hint names the
		// local block, not claims the run does not have.
		{"refused", 1, "deploy_trust local", "claim repository"},
		{"refused-elsewhere", 1, "who are you", "deploy_trust"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			srv := box(t, tc.mode)
			defer srv.Close()
			out, code := deploySh(t, srv, "MOTEL_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL=")
			if code != tc.exit || !strings.Contains(out, tc.want) || (tc.not != "" && strings.Contains(out, tc.not)) {
				t.Fatalf("mode %s: exit %d (want %d), output:\n%s", tc.mode, code, tc.exit, out)
			}
		})
	}
}

// deploySh runs the script against a stand-in box with env on top of
// the process's, and returns its combined output and exit status.
func deploySh(t *testing.T, srv *httptest.Server, env ...string) (string, int) {
	t.Helper()
	return deployShArgs(t, srv, []string{"https://example.test/a.tgz"}, env...)
}

// deployShArgs is deploySh with the operands chosen: a URL the box
// would fetch, a local file the script pushes, or --rollback and a
// version.
func deployShArgs(t *testing.T, srv *httptest.Server, args []string, env ...string) (string, int) {
	t.Helper()
	return deployShIn(t, srv, "", args, env...)
}

// deployShIn is deployShArgs run from dir (a checkout of its own, for
// the version the script takes from git); "" is this directory. An env
// entry that is a bare name, with no =, unsets that variable.
func deployShIn(t *testing.T, srv *httptest.Server, dir string, args []string, env ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("node", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// sh opens the script, not this process: the stat is what puts it in
	// Go's test cache, so an edit to it re-runs these tests.
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	// A bound, so a script stuck in its own shell (a slow expansion, a
	// loop) fails its test, not the run. It kills sh alone: a child
	// still holding the output pipe (a stalled curl) is left to go
	// test's -timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	// Later entries win, so a developer's own MOTEL_AUDIENCE, or the
	// job summary of an Actions run this test runs in, never reaches
	// the script unless a case sets it. ARTIFACT_SHA256 is dropped
	// rather than blanked: the script tells set-but-empty from unset.
	// GIT_* is dropped too (a GIT_DIR from a hook running the tests
	// would point the script's git at another repository). The stand-in
	// box is plain http, so MOTEL_ALLOW_HTTP=1 unless a case says
	// otherwise.
	all := append(append(environ("ARTIFACT_SHA256=", "GIT_"),
		"MOTEL_URL="+srv.URL+"/demo", "MOTEL_ALLOW_HTTP=1", "VERSION=v1", "MOTEL_AUDIENCE=", "GITHUB_STEP_SUMMARY="), env...)
	var unset []string
	for _, kv := range all {
		if !strings.Contains(kv, "=") {
			unset = append(unset, kv+"=")
		}
	}
	for _, kv := range all {
		if strings.Contains(kv, "=") && !hasPrefix(kv, unset) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("running deploy.sh: %v\n%s", err, out)
	}
	return string(out), 0
}

// ARTIFACT_SHA256 pins a URL deploy: it goes in the body as sha256 and
// its absence sends no pin. Set but empty (a digest step that produced
// nothing), or set with a local file or a rollback — neither of which
// the box pins — the script refuses before any output or request,
// rather than drop the pin.
func TestDeployShPinsTheArtifactDigest(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	// The body the stand-in last received, under a lock: the handler
	// runs on the server's goroutine, and the script's exit is no
	// happens-before edge the race detector can see.
	var mu sync.Mutex
	var lastBody string
	body := func() string { mu.Lock(); defer mu.Unlock(); return lastBody }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"demo","current_version":"v1","running":true}` + "\n"))
	}))
	defer srv.Close()
	pin := strings.Repeat("ab", 32)
	env := []string{"MOTEL_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL="}

	if out, code := deploySh(t, srv, append(env, "ARTIFACT_SHA256="+pin)...); code != 0 || body() != `{"url":"https://example.test/a.tgz","version":"v1","sha256":"`+pin+`"}` {
		t.Fatalf("pinned: exit %d, body %s\n%s", code, body(), out)
	}
	if out, code := deploySh(t, srv, env...); code != 0 || strings.Contains(body(), "sha256") {
		t.Fatalf("unpinned: exit %d, body %s\n%s", code, body(), out)
	}
	tarball := filepath.Join(t.TempDir(), "app.tar.gz")
	if err := os.WriteFile(tarball, []byte("not really gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{"set but empty", []string{"https://example.test/a.tgz"}, []string{"ARTIFACT_SHA256="}, "ARTIFACT_SHA256 is set but empty"},
		{"push", []string{tarball}, []string{"ARTIFACT_SHA256=" + pin}, "ARTIFACT_SHA256 pins a URL deploy"},
		{"rollback", []string{"--rollback", "v1"}, []string{"ARTIFACT_SHA256=" + pin}, "ARTIFACT_SHA256 pins a URL deploy"},
	} {
		mu.Lock()
		lastBody = ""
		mu.Unlock()
		// Under Actions, so a refusal after the group line would show.
		out, code := deployShArgs(t, srv, tc.args, append(append(env, "GITHUB_ACTIONS=true"), tc.env...)...)
		if code != 1 || !strings.Contains(out, tc.want) || strings.Contains(out, "::group::") || body() != "" {
			t.Fatalf("%s with a pin: exit %d (want 1 before any output or request), body %q\n%s", tc.name, code, body(), out)
		}
	}
}

// In Actions the run's token names the box: its audience is
// MOTEL_URL's host, lowercase, without a port or a trailing dot (the
// aud a person's token names the box by), unless MOTEL_AUDIENCE names
// another, which is sent encoded. A host that is not the stand-in's
// (.invalid, so nothing resolves it) shows the mint alone: its deploy
// fails.
func TestDeployShAsksForTheBoxsDeployHost(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	srv := box(t, "stream")
	defer srv.Close()
	_, port, _ := strings.Cut(srv.URL, "://127.0.0.1:")
	for _, tc := range []struct {
		url, audience, want string
		exit                int
	}{
		{"http://LocalHost:" + port + "/demo", "", "localhost", 0},
		{srv.URL + "/demo/", "", "127.0.0.1", 0},
		{srv.URL + "/demo", "blog", "blog", 0},
		{srv.URL + "/demo", "a&b c#d", `a\u0026b c#d`, 0}, // the stand-in's JSON escapes &
		{"http://u:p@Box.Invalid.:8443/demo", "", "box.invalid", 1},
		{"http://box.invalid?x=1", "", "box.invalid", 1},
		{"http://box.invalid#x", "", "box.invalid", 1},
	} {
		out, code := deploySh(t, srv, "GITHUB_ACTIONS=true", "MOTEL_URL="+tc.url, "MOTEL_AUDIENCE="+tc.audience,
			"ACTIONS_ID_TOKEN_REQUEST_URL="+srv.URL+"/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run", "MOTEL_TOKEN=")
		if code != tc.exit || !strings.Contains(out, "::add-mask::minted-"+tc.want+"\n") {
			t.Errorf("MOTEL_URL %s, MOTEL_AUDIENCE %q: exit %d (want %d), want the token minted for %s:\n%s", tc.url, tc.audience, code, tc.exit, tc.want, out)
		}
	}
}

// In Actions a refused OIDC token is followed by the claims the run
// minted it with — every value the run's own, so the box gives nothing
// away — in the log, the error annotation and the job summary.
func TestDeployShSaysWhatARefusedRunMustBeTrustedFor(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	srv := box(t, "refused")
	defer srv.Close()
	summary := filepath.Join(t.TempDir(), "summary.md")
	out, code := deploySh(t, srv,
		"GITHUB_ACTIONS=true", "GITHUB_REPOSITORY=org/blog", "GITHUB_REF=refs/heads/main",
		"GITHUB_WORKFLOW_REF=org/blog/.github/workflows/deploy.yml@refs/heads/main",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+srv.URL+"/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run",
		"MOTEL_TOKEN=", "GITHUB_STEP_SUMMARY="+summary)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		"::add-mask::minted-127.0.0.1",
		"audience           127.0.0.1",
		"claim repository   org/blog",
		"claim ref          refs/heads/main",
		"claim workflow_ref org/blog/.github/workflows/deploy.yml@refs/heads/main",
		"journalctl -u moteld",
		"::error title=motel%3A demo v1 failed::invalid or missing deploy token (Authorization: Bearer <jwt>); the log says what the box must trust",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "claim repository   org/blog") {
		t.Errorf("job summary lacks the checklist:\n%s", got)
	}
}

// The box's 401 is sent once more, 3s later: the first token under an
// issuer's new key is refused while the box fetches it (#306), and the
// retry deploys — a minted token's, and a MOTEL_TOKEN's, which can
// be an issuer's too (a GitLab id_token). A 401 from something that
// is not the box is not sent again.
func TestDeployShSendsARefusedTokenOnceMore(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	minted := func(srv *httptest.Server) []string {
		return []string{"GITHUB_ACTIONS=true", "GITHUB_REPOSITORY=org/blog", "GITHUB_REF=refs/heads/main",
			"ACTIONS_ID_TOKEN_REQUEST_URL=" + srv.URL + "/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run", "MOTEL_TOKEN="}
	}
	laptop := []string{"MOTEL_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL="}
	for _, tc := range []struct {
		name, mode string
		minted     bool
		exit       int
		requests   int64
	}{
		{"minted, the retry deploys", "refused-once", true, 0, 2},
		{"minted, refused again", "refused", true, 1, 2},
		{"MOTEL_TOKEN, the retry deploys", "refused-once", false, 0, 2},
		{"minted, a 401 not the box's", "refused-elsewhere", true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, n := countedBox(t, tc.mode)
			defer srv.Close()
			env := laptop
			if tc.minted {
				env = minted(srv)
			}
			out, code := deploySh(t, srv, env...)
			retried := strings.Contains(out, "the box refused the token; sending it once more in 3s (a box refuses the first token under a new signing key while it fetches the key)")
			if code != tc.exit || n.Load() != tc.requests || retried != (tc.requests == 2) {
				t.Fatalf("exit %d (want %d), %d requests (want %d), retry said: %v; output:\n%s", code, tc.exit, n.Load(), tc.requests, retried, out)
			}
		})
	}
}

// The box's 503 while it is starting is sent once more after its
// Retry-After: nothing ran on the box for it. A second one is the run's
// failure, with the box's sentence; a 503 without Retry-After, or with
// another sentence (something in front of the box), is not sent again.
func TestDeployShSendsAStartingBoxOnceMore(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	for _, tc := range []struct {
		name, mode string
		exit       int
		requests   int64
	}{
		{"a 503, then the deploy", "starting-once", 0, 2},
		{"two 503s", "starting", 1, 2},
		{"a 503 without Retry-After", "starting-no-retry-after", 1, 1},
		{"a 503 not the box's", "unavailable", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, n := countedBox(t, tc.mode)
			defer srv.Close()
			out, code := deploySh(t, srv, "MOTEL_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL=")
			resent := strings.Contains(out, "the box is starting; sending the request once more in 1s, as it asks")
			if code != tc.exit || n.Load() != tc.requests || resent != (tc.requests == 2) {
				t.Fatalf("exit %d (want %d), %d requests (want %d), resend said: %v; output:\n%s", code, tc.exit, n.Load(), tc.requests, resent, out)
			}
			if tc.mode == "starting" && !strings.Contains(out, "motel is starting and has not loaded this app yet") {
				t.Errorf("the run's failure does not give the box's sentence:\n%s", out)
			}
		})
	}
}

// A release built for another architecture is refused with the runner
// to build on, which the box words without knowing the workflow; in
// Actions the run adds the repository variable that picks the runner
// and the command that sets it. A laptop deploy has no workflow, and
// gets no such line.
func TestDeployShNamesTheRunnerVariableForAWrongArchitecture(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	srv := box(t, "wrongarch")
	defer srv.Close()
	out, code := deploySh(t, srv,
		"GITHUB_ACTIONS=true", "GITHUB_REPOSITORY=org/blog", "GITHUB_REF=refs/heads/main",
		"ACTIONS_ID_TOKEN_REQUEST_URL="+srv.URL+"/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run",
		"MOTEL_TOKEN=")
	// The build job is what the variable moves, so the line asks for a
	// new build: a re-run of the deploy job alone sends the same one.
	if code != 1 || !strings.Contains(out, "The build job's runner is the repository variable MOTEL_RUNS_ON: gh variable set MOTEL_RUNS_ON -R org/blog --body ubuntu-24.04 ") ||
		!strings.Contains(out, "then build again: push, or Re-run all jobs (a re-run of the deploy job alone sends this build again).") {
		t.Fatalf("in Actions: exit %d (want 1), and the output should name the variable and its value:\n%s", code, out)
	}
	out, code = deploySh(t, srv, "MOTEL_TOKEN=x", "GITHUB_ACTIONS=", "ACTIONS_ID_TOKEN_REQUEST_URL=")
	if code != 1 || !strings.Contains(out, "(runs-on: ubuntu-24.04)") || strings.Contains(out, "MOTEL_RUNS_ON") {
		t.Fatalf("from a laptop: exit %d (want 1), and the box's refusal without the workflow's variable:\n%s", code, out)
	}
}

// MOTEL_URL is https:// or the run stops before the token is minted
// and before any request: over http the deploy token (and, for a URL
// deploy, the job's GITHUB_TOKEN in the body) would cross the wire in
// the clear. The scheme is matched in any case; anything else —
// http://, no scheme, a space in front — is refused unless
// MOTEL_ALLOW_HTTP is exactly 1.
func TestDeployShRefusesAPlaintextURLBeforeTheMint(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	// Every request the run makes, the mint's included, under a lock
	// for the same reason as the digest test's body.
	var mu sync.Mutex
	var paths []string
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"value":"minted"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"app":"demo","current_version":"v1","running":true}` + "\n"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	// In Actions, minting from the stand-in, so a refusal after the mint
	// or the group line would show.
	actions := []string{"GITHUB_ACTIONS=true", "MOTEL_TOKEN=",
		"ACTIONS_ID_TOKEN_REQUEST_URL=" + srv.URL + "/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run"}
	run := func(url, allow string) (string, int) {
		mu.Lock()
		paths = nil
		mu.Unlock()
		return deploySh(t, srv, append(actions, "MOTEL_URL="+url, "MOTEL_ALLOW_HTTP="+allow)...)
	}

	for _, tc := range []struct{ name, url, allow string }{
		{"http", srv.URL + "/demo", ""},
		{"HTTP", "HTTP://" + host + "/demo", ""},
		{"schemeless", host + "/demo", ""},
		{"space before https", " https://" + host + "/demo", ""},
		{"https without its slashes", "https:" + host + "/demo", ""},
		{"opt-out not exactly 1", srv.URL + "/demo", "true"},
		{"opt-out padded", srv.URL + "/demo", " 1"},
	} {
		out, code := run(tc.url, tc.allow)
		if code != 1 || !strings.Contains(out, "is not https://") || !strings.Contains(out, "MOTEL_ALLOW_HTTP=1") ||
			strings.Contains(out, "::add-mask::") || strings.Contains(out, "::group::") || len(seen()) != 0 {
			t.Fatalf("%s: exit %d (want 1 before the mint or any request), requests %v\n%s", tc.name, code, seen(), out)
		}
	}

	// Past the check, the run mints. Nothing listens on port 1, so the
	// deploy itself then fails: what is asserted is that the check let
	// the run through to the mint.
	for _, url := range []string{"https://127.0.0.1:1/demo", "HTTPS://127.0.0.1:1/demo", "HttpS://127.0.0.1:1/demo"} {
		out, _ := run(url, "")
		if got := seen(); len(got) != 1 || got[0] != "/token" || strings.Contains(out, "is not https://") {
			t.Fatalf("%s: requests %v, want the mint alone\n%s", url, got, out)
		}
	}

	// The opt-out: http deploys, the mint first.
	if out, code := run(srv.URL+"/demo", "1"); code != 0 || strings.Join(seen(), " ") != "/token /demo" {
		t.Fatalf("MOTEL_ALLOW_HTTP=1: exit %d, requests %v\n%s", code, seen(), out)
	}
}

// The version the script defaults to is the commit, and the box keeps
// a version for good, so for a pushed file the default is refused while
// a tracked file anywhere in the repository differs from HEAD, staged
// or not, rather than put the commit's name on uncommitted changes. The
// refusal names the way out (VERSION) and comes before the mint or any
// request; so does git failing to compare, with git's own reason.
// Untracked files (the deploy key, a .env, the tarball itself) and a
// file only touched leave the checkout clean, an explicit VERSION
// deploys whatever the checkout holds, and a URL's artifact, built
// elsewhere, is not judged by edits here.
func TestDeployShRefusesADirtyCheckoutsDefaultVersion(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"curl", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("this test needs %s; install it to run it", tool)
		}
	}
	srv, seen := requestLog(t)
	// In Actions, minting from the stand-in, so a refusal after the mint
	// or the group line would show; git without the caller's config, and
	// in English.
	env := []string{"GITHUB_ACTIONS=true", "MOTEL_TOKEN=",
		"ACTIONS_ID_TOKEN_REQUEST_URL=" + srv.URL + "/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C"}
	write := func(t *testing.T, path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	modify := func(t *testing.T, repo string, _ func(...string) string) {
		write(t, filepath.Join(repo, "app", "server.js"), "changed")
	}
	const (
		head  = "HEAD" // the case deploys the commit's own version
		dirty = "tracked files have uncommitted changes"
	)
	for _, tc := range []struct {
		name    string
		change  func(t *testing.T, repo string, git func(...string) string)
		version string // VERSION as the script gets it; a bare name unsets it
		url     bool   // a URL deploy, not a pushed file
		deploys string // the version deployed, or ""
		refused string // when nothing is deployed, what the refusal says
		gitSays string // and what git's own stderr says before it
	}{
		{name: "clean", version: "VERSION", deploys: head},
		{name: "clean, VERSION empty", version: "VERSION=", deploys: head},
		{name: "untracked files only", change: func(t *testing.T, repo string, _ func(...string) string) {
			write(t, filepath.Join(repo, "app", "deploy.key"), "secret")
			write(t, filepath.Join(repo, ".env"), "A=1")
		}, version: "VERSION", deploys: head},
		{name: "a tracked file only touched", change: func(t *testing.T, repo string, _ func(...string) string) {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(repo, "app", "server.js"), later, later); err != nil {
				t.Fatal(err)
			}
		}, version: "VERSION", deploys: head},
		{name: "modified in the app dir", change: modify, version: "VERSION", refused: dirty},
		{name: "modified, VERSION empty", change: modify, version: "VERSION=", refused: dirty},
		{name: "modified outside the app dir", change: func(t *testing.T, repo string, _ func(...string) string) {
			write(t, filepath.Join(repo, "README.md"), "changed")
		}, version: "VERSION", refused: dirty},
		// diff.relative would narrow the diff to the app dir the script
		// runs in; the check is the repository's whatever the config.
		{name: "modified outside the app dir, diff.relative set", change: func(t *testing.T, repo string, git func(...string) string) {
			git("config", "diff.relative", "true")
			write(t, filepath.Join(repo, "README.md"), "changed")
		}, version: "VERSION", refused: dirty},
		{name: "staged", change: func(t *testing.T, repo string, git func(...string) string) {
			modify(t, repo, git)
			git("add", "app/server.js")
		}, version: "VERSION", refused: dirty},
		{name: "deleted", change: func(t *testing.T, repo string, _ func(...string) string) {
			if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
				t.Fatal(err)
			}
		}, version: "VERSION", refused: dirty},
		{name: "modified, VERSION set", change: modify, version: "VERSION=wip-3", deploys: "wip-3"},
		{name: "modified, a URL deploy", change: modify, version: "VERSION", url: true, deploys: head},
		// git's own reason comes through, ahead of the script's line.
		{name: "index unreadable", change: func(t *testing.T, repo string, _ func(...string) string) {
			write(t, filepath.Join(repo, ".git", "index"), "not an index")
		}, version: "VERSION", refused: "git could not compare", gitSays: "fatal: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitCheckout(t, map[string]string{"README.md": "readme", "app/server.js": "server"})
			app := filepath.Join(repo, "app")
			// The tarball sits in the checkout untracked, as a build leaves it.
			write(t, filepath.Join(app, "app.tar.gz"), "not really gzip")
			if tc.change != nil {
				tc.change(t, repo, git)
			}
			want := tc.deploys
			if want == head {
				want = git("rev-parse", "--short=12", "HEAD")
			}
			args := []string{"app.tar.gz"}
			if tc.url {
				args = []string{"https://example.test/a.tgz"}
			}
			out, code := deployShIn(t, srv, app, args, append(env, tc.version)...)
			got := seen()
			if want == "" {
				if code != 1 || !strings.Contains(out, tc.refused) || !strings.Contains(out, "set VERSION") || !strings.Contains(out, tc.gitSays) ||
					strings.Contains(out, "::add-mask::") || strings.Contains(out, "::group::") || len(got) != 0 {
					t.Fatalf("exit %d (want 1 before the mint or any request, saying %q), requests %v\n%s", code, tc.refused, got, out)
				}
				return
			}
			deployed := "/demo?version=" + want
			if tc.url {
				deployed = `/demo? {"url":"https://example.test/a.tgz","version":"` + want + `"}`
			}
			if code != 0 || strings.Join(got, " ") != "/token "+deployed {
				t.Fatalf("exit %d, requests %v (want the mint, then version %s)\n%s", code, got, want, out)
			}
		})
	}
}

// A version outside the box's alphabet is refused for a pushed file, a
// URL deploy and a rollback alike, before the mint or any request: in
// the query a `#` would cut the rest off and `%31` would arrive as `1`,
// deploying under another version than the one printed. What the box
// accepts goes through unchanged.
func TestDeployShRefusesAVersionOutsideTheBoxAlphabet(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	srv, seen := requestLog(t)
	actions := []string{"GITHUB_ACTIONS=true", "MOTEL_TOKEN=",
		"ACTIONS_ID_TOKEN_REQUEST_URL=" + srv.URL + "/token?api-version=1", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=run"}
	tarball := filepath.Join(t.TempDir(), "app.tar.gz")
	if err := os.WriteFile(tarball, []byte("not really gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1#x", "v%31", "v1&rollback=v0", "a b", "../x", ".v1", "a/b", `v\1`, "v1\n"} {
		for _, tc := range []struct {
			name string
			args []string
			env  string
		}{
			{"push", []string{tarball}, "VERSION=" + v},
			{"url", []string{"https://example.test/a.tgz"}, "VERSION=" + v},
			{"rollback", []string{"--rollback", v}, "VERSION="},
		} {
			out, code := deployShArgs(t, srv, tc.args, append(actions, tc.env)...)
			if got := seen(); code != 1 || !strings.Contains(out, "is not a version") ||
				strings.Contains(out, "::add-mask::") || strings.Contains(out, "::group::") || len(got) != 0 {
				t.Fatalf("%s %q: exit %d (want 1 before the mint or any request), requests %v\n%s", tc.name, v, code, got, out)
			}
		}
	}
	for _, v := range []string{"v1", "dx1", "0123456789ab", "v1.2.3", "wip-3", "_x", "-x"} {
		if out, code := deployShArgs(t, srv, []string{tarball}, append(actions, "VERSION="+v)...); code != 0 || strings.Join(seen(), " ") != "/token /demo?version="+v {
			t.Fatalf("push %q: exit %d, requests %v\n%s", v, code, seen(), out)
		}
		if out, code := deployShArgs(t, srv, []string{"--rollback", v}, actions...); code != 0 || strings.Join(seen(), " ") != "/token /demo?rollback="+v {
			t.Fatalf("rollback %q: exit %d, requests %v\n%s", v, code, seen(), out)
		}
	}
}

// requestLog is a stand-in box that mints at /token and answers every
// other request with a single 200 status; seen returns each request's
// path (and, past the mint, its query, then a space and the body when
// that is JSON) since the last call, and forgets them.
func requestLog(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/token" {
			reqs = append(reqs, r.URL.Path)
			_, _ = w.Write([]byte(`{"value":"minted"}` + "\n"))
			return
		}
		req := r.URL.Path + "?" + r.URL.RawQuery
		if r.Header.Get("Content-Type") == "application/json" {
			req += " " + string(b)
		}
		reqs = append(reqs, req)
		_, _ = w.Write([]byte(`{"app":"demo","current_version":"v1","running":true}` + "\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		got := reqs
		reqs = nil
		return got
	}
}

// environ is the process's environment without the entries that start
// with any of drop.
func environ(drop ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !hasPrefix(kv, drop) {
			env = append(env, kv)
		}
	}
	return env
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// gitCheckout makes a repository in a temporary directory with files
// committed, and returns it with a function that runs git there and
// returns its trimmed output, free of the caller's git configuration.
func gitCheckout(t *testing.T, files map[string]string) (string, func(...string) string) {
	t.Helper()
	repo := t.TempDir()
	env := append(environ("GIT_"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	for name, s := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	return repo, git
}

// The bodies a box answers a failed deploy with, as liveswap writes
// them (paths shortened), and what the script names as the failure
// (#533): the body's own cause, and last_deploy's phase only when it is
// a deploy of the version sent.
const (
	linkRefusal = `refusing to deploy: /srv/motel/demo/tmp is a link, which is not followed under the liveswap root`
	// A failure body whose last_deploy and deploys are another
	// version's, v2's, failed in starting: a URL deploy's or rollback's
	// status read after another deploy took the lock (the race in
	// DESIGN-liveswap.md's accepted residuals).
	otherVersionsLastDeploy = `{"error":"` + linkRefusal + `","status":{"app":"demo","phase":"idle","current_version":"v1","running":true,"deploys":[{"version":"v2","status":"failed","phase":"starting","started_at":"t","finished_at":"t"},{"version":"v1","status":"succeeded","started_at":"t","finished_at":"t"}],"last_deploy":{"version":"v2","status":"failed","error":"start failed: boom","phase":"starting","started_at":"t","finished_at":"t","artifact_entries":0,"artifact_bytes":0,"phases":[{"name":"downloading","started_at":"t","seconds":0},{"name":"starting","started_at":"t","seconds":0}]},"available_versions":["v1"]}}`
	// v3 failed in the pipeline before its first phase: last_deploy is
	// its own, with no phase; v2's failed record is in deploys.
	failedBeforeAnyPhase = `{"error":"` + linkRefusal + `","status":{"app":"demo","phase":"idle","current_version":"v1","running":true,"deploys":[{"version":"v2","status":"failed","phase":"starting","started_at":"t","finished_at":"t"}],"last_deploy":{"version":"v3","status":"failed","error":"` + linkRefusal + `","started_at":"t","finished_at":"t","artifact_entries":0,"artifact_bytes":0},"available_versions":["v1"]}}`
	// v3 failed in starting, as a stream's last line.
	failedInStarting = `{"at":"t","event":"phase","phase":"downloading"}` + "\n" + `{"error":"start failed: boom","status":{"app":"demo","phase":"idle","current_version":"v1","running":true,"deploys":[{"version":"v3","status":"failed","phase":"starting","started_at":"t","finished_at":"t"}],"last_deploy":{"version":"v3","status":"failed","error":"start failed: boom","phase":"starting","started_at":"t","finished_at":"t","artifact_entries":0,"artifact_bytes":0},"available_versions":["v1"]},"event":"done","http_status":500}`
	// A body an older moteld's filter changed: its top level
	// re-marshalled with its keys sorted, redacted_env among them; v1
	// failed in starting.
	failedRedacted = `{"error":"start failed: [redacted]","redacted_env":["API_KEY"],"status":{"app":"demo","phase":"idle","current_version":"v0","running":true,"deploys":[{"version":"v0","status":"failed","phase":"soaking","started_at":"t","finished_at":"t"}],"last_deploy":{"version":"v1","status":"failed","error":"start failed: [redacted]","phase":"starting","started_at":"t","finished_at":"t","artifact_entries":0,"artifact_bytes":0},"available_versions":["v0"]}}`
	// The same failure as this moteld writes it: the wire type's order,
	// redacted_env last.
	failedRedactedNow = `{"error":"start failed: [redacted]","status":{"app":"demo","phase":"idle","current_version":"v0","running":true,"deploys":[{"version":"v0","status":"failed","phase":"soaking","started_at":"t","finished_at":"t"}],"last_deploy":{"version":"v1","status":"failed","error":"start failed: [redacted]","phase":"starting","started_at":"t","finished_at":"t","artifact_entries":0,"artifact_bytes":0},"available_versions":["v0"]},"redacted_env":["API_KEY"]}`
)

func TestDeployShNamesTheFailureOfTheVersionSent(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	for _, tc := range []struct {
		name, body, version string
		rollback            string // --rollback's version; none is a deploy of version
		stream              bool
		annotation          string // the ::error message
		summary             string // the job summary's first line, after "**motel:** "
	}{
		{"a body whose last_deploy is another version's", otherVersionsLastDeploy, "v3", "", false, linkRefusal, "demo v3 failed, "},
		{"a refusal alone", `{"error":"` + linkRefusal + `"}`, "v3", "", false, linkRefusal, "demo v3 failed, "},
		{"a failure before the first phase", failedBeforeAnyPhase, "v3", "", false, linkRefusal, "demo v3 failed, "},
		{"a failure in starting, streamed", failedInStarting, "v3", "", true, "in starting: start failed: boom", "demo v3 failed in `starting`, "},
		{"a filtered body", failedRedacted, "v1", "", false, "in starting: start failed: [redacted]", "demo v1 failed in `starting`, "},
		{"a filtered body in the wire type's order", failedRedactedNow, "v1", "", false, "in starting: start failed: [redacted]", "demo v1 failed in `starting`, "},
		{"a rollback to last_deploy's version", failedRedacted, "v3", "v1", false, "in starting: start failed: [redacted]", "demo rollback to v1 failed in `starting`, "},
		{"a rollback to another version", failedRedacted, "v3", "v0", false, "start failed: [redacted]", "demo rollback to v0 failed, "},
		// What Go's JSON escapes reads as written; an escaped backslash
		// before such a sequence stays a backslash.
		{"a cause with escapes", `{"error":"probe: \u003cb\u003e \u0026 \\u003c\nnext"}`, "v3", "", false, `probe: <b> & \u003c next`, "demo v3 failed, "},
		// Not the box's: its cause is not the first key, so the
		// annotation points at the body, which the log printed.
		{"JSON that is not the box's", `{"code":7,"error":"bad gateway","phase":"x"}`, "v3", "", false, "see the response above", "demo v3 failed, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.WriteHeader(http.StatusOK)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
				}
				_, _ = w.Write([]byte(tc.body + "\n"))
			}))
			defer srv.Close()
			args := []string{"https://example.test/a.tgz"}
			if tc.rollback != "" {
				args = []string{"--rollback", tc.rollback}
			}
			summary := filepath.Join(t.TempDir(), "summary.md")
			out, code := deployShArgs(t, srv, args, "VERSION="+tc.version, "MOTEL_TOKEN=x", "GITHUB_ACTIONS=true",
				"ACTIONS_ID_TOKEN_REQUEST_URL=", "GITHUB_STEP_SUMMARY="+summary)
			var annotation string
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, "::error ") {
					annotation = l[strings.LastIndex(l, "::")+2:]
				}
			}
			sum, err := os.ReadFile(summary)
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 || annotation != tc.annotation || !strings.HasPrefix(string(sum), "**motel:** "+tc.summary) {
				t.Errorf("exit %d, annotation %q (want %q), summary %q (want it to begin %q)\n%s", code, annotation, tc.annotation, sum, tc.summary, out)
			}
			// The summary's second paragraph is the cause, as Markdown
			// that GitHub renders with its HTML: <, > and & as text.
			cause := tc.annotation
			if _, after, ok := strings.Cut(cause, ": "); ok && strings.HasPrefix(cause, "in ") {
				cause = after
			}
			if cause == "see the response above" {
				cause = "see the log"
			}
			cause = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(cause)
			if !strings.Contains(string(sum), "\n\n"+cause+"\n") {
				t.Errorf("summary %q lacks the cause %q", sum, cause)
			}
		})
	}
}

// A long one-line answer that is not the box's (a wrong MOTEL_URL
// serving a minified page) is read in time linear in its length: a
// strip with the shell's own pattern match took 11 s for 100 KB in
// dash, and four times that for each doubling.
func TestDeployShReadsALongAnswerQuickly(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatal("deploy.sh needs curl; install it to run this test")
	}
	long := strings.Repeat("a", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(long + "\n"))
	}))
	defer srv.Close()
	began := time.Now()
	out, code := deploySh(t, srv, "MOTEL_TOKEN=x", "GITHUB_ACTIONS=true", "ACTIONS_ID_TOKEN_REQUEST_URL=")
	if took := time.Since(began); code != 1 || took > 30*time.Second || !strings.Contains(out, "::see the response above") {
		t.Errorf("exit %d after %s, output ending %q", code, took, out[max(0, len(out)-300):])
	}
}
