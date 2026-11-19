//go:build integration

package liveswap

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// The integration suite authenticates deploys with a local trust key:
// one ed25519 keypair is generated per test binary, its public half
// written to a temp file the Caddyfile's `deploy_trust local` block
// points at, and deploy requests carry a token minted with the private
// half (audience "itest"). itestPriv/itestPub reuse the shared
// authtest helpers.
var (
	itestPriv, itestPub = trusttest.GenerateKey()
	itestPubPath        string
	itestPubOnce        sync.Once
)

func itestPubKeyPath() string {
	itestPubOnce.Do(func() {
		der, err := x509.MarshalPKIXPublicKey(itestPub)
		if err != nil {
			panic(err)
		}
		f, err := os.CreateTemp("", "itest-deploy-*.pub")
		if err != nil {
			panic(err)
		}
		if _, err := f.Write(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
		itestPubPath = f.Name()
	})
	return itestPubPath
}

func itestToken(t *testing.T) string { return trusttest.Mint(t, itestPriv, "itest", nil) }

// The integration suite runs a real Caddy (in-process via caddytest)
// with the module loaded, deploys a real compiled test app through the
// webhook, and drives traffic through reverse_proxy's dynamic
// upstreams. It needs the Go toolchain (present in the dev container)
// to build the test app binary.

const testAppSource = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	version := "unknown"
	if b, err := os.ReadFile("version.txt"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		// Checked per request (not once at boot) so tests can break and
		// heal a RUNNING instance, which is what a watchdog reacts to.
		if _, err := os.Stat("broken"); err == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello %s pid %d", version, os.Getpid())
	})
	ln, err := net.Listen("unix", os.Getenv("SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := http.Serve(ln, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

// buildTestApp compiles the test app once and returns the binary path.
func buildTestApp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(testAppSource), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "server")
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building test app: %v\n%s", err, out)
	}
	return bin
}

// packRelease tarballs the test app binary plus a version marker (and
// optionally the "broken" flag file the app's health check honors).
func packRelease(t *testing.T, bin, version string, broken bool) []byte {
	t.Helper()
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	entries := []tarEntry{
		{name: "server", body: string(binData), mode: 0o755},
		{name: "version.txt", body: version + "\n"},
	}
	if broken {
		entries = append(entries, tarEntry{name: "broken", body: "1"})
	}
	path := buildTarGz(t, entries)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func integrationConfig(root, artifactPort string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%s
		deploy_trust local {
			public_key %s
			audience itest
		}

		app demo {
			command ./server
			env GREETING integration
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 2s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap demo
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, itestPubKeyPath())
}

func postDeploy(t *testing.T, artifactURL, version string) (*http.Response, string) {
	t.Helper()
	return postDeployApp(t, "demo", artifactURL, version)
}

// getBody GETs the proxy front door (:9080), the address every app is
// served on.
func getBody(t *testing.T) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://localhost:9080/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

var pidRe = regexp.MustCompile(`pid (\d+)`)

var socketRe = regexp.MustCompile(`"socket":"([^"]+)"`)

// socketFromStatus extracts the active instance's socket path from the
// webhook status JSON.
func socketFromStatus(t *testing.T, status string) string {
	t.Helper()
	m := socketRe.FindStringSubmatch(status)
	if m == nil {
		t.Fatalf("no socket in status: %s", status)
	}
	return m[1]
}

// integrationServer makes a test's liveswap root and the tester that
// loads its configs into the in-process Caddy, and ends the test's
// apps before the root is removed (#534).
//
// Caddy outlives each test: its config stays loaded until the next
// test's InitServer replaces it, so the test's apps, their watchdogs
// and their units would carry on under root after it returns. A
// watchdog restart makes a run dir and a socket there, and one made
// while t.TempDir's RemoveAll is emptying the tree fails the test
// with "directory not empty". Cleanups run last-registered first, and
// this one is registered after t.TempDir's, so it runs before the
// removal.
func integrationServer(t *testing.T) (string, *caddytest.Tester) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { endApps(t, root) })
	return root, caddytest.NewTester(t)
}

// endApps loads a config that names no app, which removes every app
// the test configured: their Destruct stops the watchdog and waits for
// it, then stops the units and prunes the sockets, all before the load
// answers. It then waits for the manager to hold no unit of any app
// under root. The config keeps liveswap itself: a load with no
// liveswap at all would leave the units running for a later start to
// reattach (Destruct when no started config holds liveswap).
func endApps(t *testing.T, root string) {
	t.Helper()
	if t.Skipped() {
		return // skipped before or at its first InitServer
	}
	idle := fmt.Sprintf(`{
	admin localhost:2999
	liveswap {
		root %s
	}
}
`, root)
	req, err := http.NewRequest(http.MethodPost, "http://localhost:2999/load", strings.NewReader(idle))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/caddyfile")
	// Destruct waits for a deploy of the app still in flight.
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("loading a config with no app, to end the test's apps: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loading a config with no app, to end the test's apps: %d %s", resp.StatusCode, body)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		app := e.Name()
		if !e.IsDir() || !appNameRe.MatchString(app) {
			continue
		}
		pollUntil(t, 10*time.Second, "no unit of "+app+" left", func() bool { return appUnits(t, app) == "" })
	}
}

func TestIntegrationDeployLifecycle(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)

	// Artifact "release storage": one tarball per version, like a
	// forge's release assets.
	artifacts := map[string][]byte{
		"/demo-v1.tar.gz":        packRelease(t, bin, "v1", false),
		"/demo-v2.tar.gz":        packRelease(t, bin, "v2", false),
		"/demo-v3-broken.tar.gz": packRelease(t, bin, "v3", true),
	}
	artifactSrv := serveArtifacts(t, artifacts)

	// The allowlist entry declares the artifact server's literal port
	// (there is no wildcard), so the config is built after the server.
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tester.InitServer(integrationConfig(root, artifactURL.Port()), "caddyfile")

	t.Run("proxy errors before first deploy", func(t *testing.T) {
		code, _ := getBody(t)
		if code < 500 {
			t.Fatalf("expected a 5xx before any deploy, got %d", code)
		}
	})

	t.Run("webhook rejects a bad token", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:9081/demo",
			strings.NewReader(`{"url":"https://x/a.tgz","version":"v0"}`))
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("deploy v1 and serve it", func(t *testing.T) {
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz", "v1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
		}
		code, page := getBody(t)
		if code != http.StatusOK || !strings.Contains(page, "hello v1") {
			t.Fatalf("proxied response = %d %q", code, page)
		}
	})

	t.Run("deploy v2 cuts over and stops v1", func(t *testing.T) {
		_, prev := getBody(t)
		_, statusBefore := getStatus(t)
		v1Socket := socketFromStatus(t, statusBefore)

		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v2.tar.gz", "v2")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v2: %d %s", resp.StatusCode, body)
		}
		code, page := getBody(t)
		if code != http.StatusOK || !strings.Contains(page, "hello v2") {
			t.Fatalf("proxied response = %d %q", code, page)
		}
		if page == prev {
			t.Fatal("response did not change after cutover")
		}
		_, status := getStatus(t)
		if !strings.Contains(status, `"current_version":"v2"`) {
			t.Fatalf("status after v2: %s", status)
		}

		// The old socket must be GONE, not merely unrouted: v2 got its
		// own, and the deploy response returns only after drain +
		// stop-old, so by now v1's socket file has been removed and a
		// connect to it fails.
		v2Socket := socketFromStatus(t, status)
		if v2Socket == v1Socket {
			t.Fatalf("v2 was handed v1's socket %s while v1 was alive", v1Socket)
		}
		if _, err := os.Stat(v1Socket); !os.IsNotExist(err) {
			t.Fatalf("old v1 socket %s still present after stop: %v", v1Socket, err)
		}
		if conn, err := net.DialTimeout("unix", v1Socket, time.Second); err == nil {
			_ = conn.Close()
			t.Fatalf("old v1 socket %s still accepting connections after stop", v1Socket)
		}
	})

	t.Run("broken v3 fails and v2 keeps serving", func(t *testing.T) {
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v3-broken.tar.gz", "v3")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("broken deploy should 500, got %d %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "health gate") {
			t.Fatalf("500 body should name the health gate: %s", body)
		}
		code, page := getBody(t)
		if code != http.StatusOK || !strings.Contains(page, "hello v2") {
			t.Fatalf("v2 must keep serving after failed v3: %d %q", code, page)
		}
	})

	t.Run("config reload keeps the app process running", func(t *testing.T) {
		pidBefore := currentPID(t)

		// Reload the SAME config through the admin API. The UsagePool
		// must carry the running child across the reload untouched.
		tester.InitServer(integrationConfig(root, artifactURL.Port()), "caddyfile")

		code, after := getBody(t)
		if code != http.StatusOK {
			t.Fatalf("post-reload proxy = %d", code)
		}
		if !strings.Contains(after, "hello ") {
			t.Fatalf("post-reload body: %q", after)
		}
		if pidAfter := currentPID(t); pidAfter != pidBefore {
			t.Fatalf("app process restarted across reload: before=%d after=%d", pidBefore, pidAfter)
		}
	})

	t.Run("keep-N prunes old releases on the next successful deploy", func(t *testing.T) {
		// GC runs only after a SUCCESSFUL deploy (failed v3's dir is
		// kept for debugging), so trigger one more: v4 reuses the v1
		// artifact under a new version tag.
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz", "v4")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("deploy v4: %d %s", resp.StatusCode, body)
		}
		entries, err := os.ReadDir(filepath.Join(root, "demo", "releases"))
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				kept = append(kept, e.Name())
			}
		}
		if len(kept) > 2 {
			t.Fatalf("keep=2 violated after successful deploy: %v", kept)
		}
		if _, err := os.Stat(filepath.Join(root, "demo", "releases", "v4")); err != nil {
			t.Fatalf("current release v4 missing: %v", err)
		}
	})

	t.Run("un-allowed query parameter is a 422 that names the parameter", func(t *testing.T) {
		// The config's entry pins the artifact server's literal port
		// but declares no query parameters, so ?p=2 must be refused before any request leaves
		// the box, as a 422 whose body tells the CI author exactly what
		// tripped and how the entry would declare it.
		resp, body := postDeploy(t, artifactSrv.URL+"/demo-v1.tar.gz?p=2", "v5")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("query refusal: got %d %s, want 422", resp.StatusCode, body)
		}
		for _, want := range []string{`\"p\"`, "declares no query parameters"} {
			if !strings.Contains(body, want) {
				t.Errorf("422 body should contain %s: %s", want, body)
			}
		}
		// The refusal must not have disturbed the running version.
		if _, status := getStatus(t); !strings.Contains(status, `"current_version":"v4"`) {
			t.Fatalf("v4 no longer serving after refused deploy: %s", status)
		}
	})
}

// watchdogConfig is integrationConfig with fast watchdog settings and
// a configurable restart budget/window. The app name must be unique
// per test function: the UsagePool is process-global, so a `demo`
// still running from another test would be adopted here.
func watchdogConfig(root, artifactPort, app string, restarts int, window string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %[1]s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%[2]s
		deploy_trust local {
			public_key %[6]s
			audience itest
		}

		app %[3]s {
			command ./server
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 1s
			watchdog_grace 200ms
			watchdog_failures 3
			watchdog_restarts %[4]d
			watchdog_window %[5]s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap %[3]s
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, app, restarts, window, itestPubKeyPath())
}

func postDeployApp(t *testing.T, app, artifactURL, version string) (*http.Response, string) {
	t.Helper()
	body := fmt.Sprintf(`{"url":%q,"version":%q}`, artifactURL, version)
	req, err := http.NewRequest(http.MethodPost, "http://localhost:9081/"+app, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+itestToken(t))
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func getStatusApp(t *testing.T, app string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:9081/"+app, nil)
	req.Header.Set("Authorization", "Bearer "+itestToken(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func serveArtifacts(t *testing.T, artifacts map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := artifacts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tryBody is getBody without the fatals, for polling through the
// window in which the proxy legitimately 502s.
func tryBody() (int, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:9080/")
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// statusPIDRe reads the host pid from the status endpoint. The pid an
// app prints about itself is 1 inside its PID namespace, so only the
// status (the unit's MainPID) can tell instances apart or be signalled.
var statusPIDRe = regexp.MustCompile(`"pid":(\d+)`)

// statusPID is the running instance's host pid, or (0, false) when the
// status carries none (nothing running yet).
func statusPID(t *testing.T, app string) (int, bool) {
	t.Helper()
	code, status := getStatusApp(t, app)
	m := statusPIDRe.FindStringSubmatch(status)
	if code != http.StatusOK || m == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid == 0 {
		return 0, false
	}
	return pid, true
}

func currentPID(t *testing.T) int { return currentPIDApp(t, "demo") }

func currentPIDApp(t *testing.T, app string) int {
	t.Helper()
	code, page := getBody(t)
	if code != http.StatusOK || !pidRe.MatchString(page) {
		t.Fatalf("app not serving: %d %q", code, page)
	}
	pid, ok := statusPID(t, app)
	if !ok {
		t.Fatalf("status carries no pid for %s", app)
	}
	return pid
}

func pollUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	pollFor(t, timeout, 25*time.Millisecond, desc, cond)
}

func TestIntegrationWatchdogRestarts(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	artifactSrv := serveArtifacts(t, map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
	})
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "wdcrash"
	tester.InitServer(watchdogConfig(root, artifactURL.Port(), app, 50, "10m"), "caddyfile")

	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}

	t.Run("SIGKILLed app comes back with a new pid", func(t *testing.T) {
		pid := currentPIDApp(t, app)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		pollUntil(t, 15*time.Second, "restart with a new pid", func() bool {
			if code, _ := tryBody(); code != http.StatusOK {
				return false
			}
			p, ok := statusPID(t, app)
			return ok && p != pid
		})
		// The new instance is published a beat before the restart is
		// recorded, so poll for the record rather than read it once.
		var status string
		pollUntil(t, 5*time.Second, "crash restart recorded", func() bool {
			_, status = getStatusApp(t, app)
			return strings.Contains(status, `"last_restart_cause":"crash"`)
		})
	})

	t.Run("sustained health failure triggers a restart", func(t *testing.T) {
		pid := currentPIDApp(t, app)
		brokenPath := filepath.Join(root, app, "releases", "v1", "broken")
		if err := os.WriteFile(brokenPath, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		pollUntil(t, 20*time.Second, "health-triggered restart", func() bool {
			if code, _ := tryBody(); code != http.StatusOK {
				return false
			}
			p, ok := statusPID(t, app)
			return ok && p != pid
		})
		// Heal the release so the replacement stays up.
		if err := os.Remove(brokenPath); err != nil {
			t.Fatal(err)
		}
		var status string
		pollUntil(t, 5*time.Second, "health restart recorded", func() bool {
			_, status = getStatusApp(t, app)
			return strings.Contains(status, `"last_restart_cause":"health"`)
		})
	})
}

func TestIntegrationWatchdogThrottleAutoRecovers(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	artifactSrv := serveArtifacts(t, map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
	})
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "wdex"
	tester.InitServer(watchdogConfig(root, artifactURL.Port(), app, 1, "10s"), "caddyfile")

	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}

	// Budget is 1 per 10s window: the first kill restarts immediately,
	// the second throttles until the window frees.
	pid := currentPIDApp(t, app)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 15*time.Second, "the single budgeted restart", func() bool {
		if code, _ := tryBody(); code != http.StatusOK {
			return false
		}
		p, ok := statusPID(t, app)
		return ok && p != pid
	})
	secondPID := currentPIDApp(t, app)
	if err := syscall.Kill(secondPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pollUntil(t, 15*time.Second, "throttle once the window is full", func() bool {
		_, status := getStatusApp(t, app)
		return strings.Contains(status, `"state":"throttled"`)
	})
	pollUntil(t, 5*time.Second, "clean 5xx while the dead app waits", func() bool {
		code, _ := tryBody()
		return code >= 500
	})

	// Never give up: with no deploy and no operator, the watchdog
	// restarts on its own once the oldest restart slides out of the
	// window (~10s), and the app serves again.
	pollUntil(t, 30*time.Second, "auto-recovery without a deploy", func() bool {
		code, page := tryBody()
		if code != http.StatusOK || !strings.Contains(page, "hello v1") {
			return false
		}
		p, ok := statusPID(t, app)
		return ok && p != secondPID
	})
	if _, status := getStatusApp(t, app); strings.Contains(status, `"state":"throttled"`) {
		t.Fatalf("throttle must clear after the auto-recovery: %s", status)
	}
}

func getStatus(t *testing.T) (int, string) {
	t.Helper()
	return getStatusApp(t, "demo")
}

// oidcConfig runs an app whose deploys are authenticated by an OIDC
// provider (our own in-process mock issuer), exercising the production
// OIDC path — Caddyfile parsing, Provision wiring, the JWKS client,
// discovery and token verification — end to end through the real
// binary, which the local-key e2e path does not.
func oidcConfig(root, artifactPort, issuer string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %[1]s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%[2]s
		deploy_trust oidc {
			issuer %[3]s
			audience e2e
			claim sub ci
		}

		app oidcdemo {
			command ./server
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 2s
			keep 2
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap oidcdemo
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, issuer)
}

func TestIntegrationOIDCDeploy(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	artifacts := map[string][]byte{"/demo-v1.tar.gz": packRelease(t, bin, "v1", false)}
	artifactSrv := serveArtifacts(t, artifacts)
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Our own OIDC provider, in-process: serves discovery + JWKS and
	// signs tokens, standing in for GitHub/GitLab.
	iss := trusttest.NewIssuer(t)

	tester.InitServer(oidcConfig(root, artifactURL.Port(), iss.URL), "caddyfile")

	hook := "http://localhost:9081/oidcdemo"
	deploy := func(token string) (int, string) {
		body := fmt.Sprintf(`{"url":%q,"version":"v1"}`, artifactSrv.URL+"/demo-v1.tar.gz")
		req, _ := http.NewRequest(http.MethodPost, hook, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	future := time.Now().Add(5 * time.Minute)

	t.Run("valid OIDC token deploys and serves", func(t *testing.T) {
		tok := iss.Mint(t, iss.Priv, "e2e", map[string]string{"sub": "ci"}, future)
		// Provision fetches the issuer's discovery and keys in the
		// background (deploytrust.Warm), and no token waits on them: one
		// sent before they end is refused, and a retry once they have
		// deploys. A loaded lane may take a while, so retry to a deadline.
		code, body := deploy(tok)
		for deadline := time.Now().Add(15 * time.Second); code == http.StatusUnauthorized && time.Now().Before(deadline); {
			time.Sleep(200 * time.Millisecond)
			code, body = deploy(tok)
		}
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", code, body)
		}
		code, b := getBody(t)
		if code != http.StatusOK || !strings.HasPrefix(b, "hello v1") {
			t.Fatalf("proxy did not serve v1: %d %q", code, b)
		}
	})

	// Each negative case is a validly-signed token that must still be
	// rejected — the whole point of the claim allowlist.
	t.Run("wrong identity claim is rejected", func(t *testing.T) {
		tok := iss.Mint(t, iss.Priv, "e2e", map[string]string{"sub": "attacker"}, future)
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong sub, got %d", code)
		}
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		tok := iss.Mint(t, iss.Priv, "other", map[string]string{"sub": "ci"}, future)
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for wrong audience, got %d", code)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		tok := iss.Mint(t, iss.Priv, "e2e", map[string]string{"sub": "ci"}, time.Now().Add(-time.Hour))
		if code, _ := deploy(tok); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for expired token, got %d", code)
		}
	})

	t.Run("garbage token is rejected", func(t *testing.T) {
		if code, _ := deploy("not-a-jwt"); code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for garbage token, got %d", code)
		}
	})
}

// appConfig is integrationConfig parameterized by app name, for the
// push/rollback suites (which must use unique app names — the pool is
// process-global).
func appConfig(root, artifactPort, app string) string {
	return fmt.Sprintf(`{
	skip_install_trust
	admin localhost:2999
	http_port 9080
	grace_period 1ns

	liveswap {
		root %[1]s
		allow_insecure_http
		artifact_allowlist 127.0.0.1:%[2]s
		deploy_trust local {
			public_key %[4]s
			audience itest
		}

		app %[3]s {
			command ./server
			health_interval 50ms
			health_timeout 1s
			soak 100ms
			deadline 5s
			drain 1ms
			grace 2s
			keep 3
		}
	}
}
http://localhost:9080 {
	reverse_proxy {
		dynamic liveswap %[3]s
	}
}
http://localhost:9081 {
	liveswap_webhook
}
`, root, artifactPort, app, itestPubKeyPath())
}

func TestIntegrationPushDeploy(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)

	// No artifact server: push provides the bytes directly. The
	// allowlist entry is required by config but unused on this path.
	tester.InitServer(appConfig(root, "9999", "pushdemo"), "caddyfile")

	push := func(version string, tarball []byte) (int, string) {
		req, _ := http.NewRequest(http.MethodPost,
			"http://localhost:9081/pushdemo?version="+version, bytes.NewReader(tarball))
		req.Header.Set("Authorization", "Bearer "+itestToken(t))
		req.Header.Set("Content-Type", "application/gzip")
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}

	t.Run("streamed tarball deploys and serves", func(t *testing.T) {
		if code, body := push("v1", packRelease(t, bin, "v1", false)); code != http.StatusOK {
			t.Fatalf("push v1: %d %s", code, body)
		}
		code, page := getBody(t)
		if code != http.StatusOK || !strings.HasPrefix(page, "hello v1") {
			t.Fatalf("proxy did not serve pushed v1: %d %q", code, page)
		}
	})

	t.Run("a non-gzip / garbage body fails the deploy, old keeps serving", func(t *testing.T) {
		if code, _ := push("v2", []byte("not a tarball")); code < 500 {
			t.Fatalf("garbage push should 5xx, got %d", code)
		}
		code, page := getBody(t)
		if code != http.StatusOK || !strings.HasPrefix(page, "hello v1") {
			t.Fatalf("v1 should still serve after a bad push: %d %q", code, page)
		}
	})
}

func TestIntegrationRollback(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	artifacts := map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
		"/demo-v2.tar.gz": packRelease(t, bin, "v2", false),
	}
	artifactSrv := serveArtifacts(t, artifacts)
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tester.InitServer(appConfig(root, artifactURL.Port(), "rollbackdemo"), "caddyfile")

	rollback := func(version string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:9081/rollbackdemo?rollback="+version, nil)
		req.Header.Set("Authorization", "Bearer "+itestToken(t))
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}

	// Deploy v1 then v2 (both real URL pulls).
	if resp, body := postDeployApp(t, "rollbackdemo", artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}
	if resp, body := postDeployApp(t, "rollbackdemo", artifactSrv.URL+"/demo-v2.tar.gz", "v2"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v2: %d %s", resp.StatusCode, body)
	}
	if _, page := getBody(t); !strings.HasPrefix(page, "hello v2") {
		t.Fatalf("expected v2 serving, got %q", page)
	}

	t.Run("rollback relaunches the on-disk v1 without a fetch", func(t *testing.T) {
		if code, body := rollback("v1"); code != http.StatusOK {
			t.Fatalf("rollback v1: %d %s", code, body)
		}
		if _, page := getBody(t); !strings.HasPrefix(page, "hello v1") {
			t.Fatalf("expected v1 after rollback, got %q", page)
		}
	})

	t.Run("rollback to a version not on disk is 422", func(t *testing.T) {
		if code, _ := rollback("v99"); code != http.StatusUnprocessableEntity {
			t.Fatalf("rollback to missing version: got %d, want 422", code)
		}
	})
}

// A reload that removes an app while a deploy of it is in flight leaves
// none of its units running. The reload stops the old config's servers
// without waiting for the deploy's request, which carries on; Destruct
// waits for the deploy itself, then stops and sweeps what it left.
func TestIntegrationRemovingAnAppMidDeployLeavesNoUnit(t *testing.T) {
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	tarball := packRelease(t, bin, "v1", false)
	inFetch := make(chan struct{}, 1)
	artifactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case inFetch <- struct{}{}:
		default:
		}
		time.Sleep(time.Second) // the deploy is in flight while the reload lands
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(artifactSrv.Close)
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "rmdemo"
	tester.InitServer(appConfig(root, artifactURL.Port(), app), "caddyfile")

	token := itestToken(t)
	deployed := make(chan string, 1)
	go func() {
		body := fmt.Sprintf(`{"url":%q,"version":"v1"}`, artifactSrv.URL+"/rmdemo-v1.tar.gz")
		req, _ := http.NewRequest(http.MethodPost, "http://localhost:9081/"+app, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			deployed <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		deployed <- fmt.Sprintf("%d %s", resp.StatusCode, data)
	}()
	select {
	case <-inFetch:
	case <-time.After(10 * time.Second):
		t.Fatal("the deploy never reached the artifact server")
	}
	// The reload that removes the app, mid-download.
	tester.InitServer(appConfig(root, artifactURL.Port(), "rmother"), "caddyfile")
	select {
	case got := <-deployed:
		if !strings.HasPrefix(got, "200 ") {
			t.Fatalf("the deploy in flight runs to its end: %s", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the deploy in flight never answered")
	}
	pollUntil(t, 10*time.Second, "no unit of the removed app", func() bool { return appUnits(t, app) == "" })
	time.Sleep(time.Second)
	if units := appUnits(t, app); units != "" {
		t.Fatalf("a unit of the removed app runs on:\n%s", units)
	}
	for _, d := range []string{"run", "proxy"} {
		if left := leftIn(t, filepath.Join(root, app, d)); len(left) != 0 {
			t.Fatalf("the removed app leaves nothing in %s/: %v", d, left)
		}
	}
}

// appUnits is every unit the manager holds for app, in any state.
func appUnits(t *testing.T, app string) string {
	t.Helper()
	return strings.TrimSpace(run(t, "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "motel-"+app+".*"))
}

// leftIn lists what dir still holds; nothing when it is gone.
func leftIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A release that cannot run on this box is refused at the relaunch's
// pre-flight, once, on the real runner: the watchdog's restart of a
// crashed instance whose executable became one for another machine,
// and then a reload's recovery of the same release. No unit is started
// to die at its exec, nothing loops, status names the architecture, and a
// deploy of a release that can run brings the app back.
func TestIntegrationRelaunchRefusedAtPreflight(t *testing.T) {
	var other uint16 // a machine that is not this box's
	switch runtime.GOARCH {
	case "amd64":
		other = 183 // AArch64
	case "arm64":
		other = 62 // x86-64
	default:
		t.Skipf("no hosted box is %s", runtime.GOARCH)
	}
	bin := buildTestApp(t)
	root, tester := integrationServer(t)
	artifactSrv := serveArtifacts(t, map[string][]byte{
		"/demo-v1.tar.gz": packRelease(t, bin, "v1", false),
		"/demo-v2.tar.gz": packRelease(t, bin, "v2", false),
	})
	artifactURL, err := url.Parse(artifactSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	const app = "archdemo"
	config := watchdogConfig(root, artifactURL.Port(), app, 50, "10m")
	tester.InitServer(config, "caddyfile")
	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v1.tar.gz", "v1"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v1: %d %s", resp.StatusCode, body)
	}
	pid := currentPIDApp(t, app)

	// The executable becomes one for another machine under the running
	// instance (a new inode: the running one is busy), and the instance
	// dies.
	server := filepath.Join(root, app, "releases", "v1", "server")
	if err := os.Remove(server); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, elfHeader(2, false, other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	const named = "build on a runner of the box's architecture"
	var status string
	pollUntil(t, 15*time.Second, "the watchdog's relaunch refused", func() bool {
		_, status = getStatusApp(t, app)
		return strings.Contains(status, named)
	})
	// Refused once: a loop would claim a budget slot every backoff step.
	time.Sleep(4 * time.Second)
	_, status = getStatusApp(t, app)
	for _, want := range []string{`"state":"idle"`, `"restarts_in_window":1`, named} {
		if !strings.Contains(status, want) {
			t.Fatalf("status after the refusal lacks %s: %s", want, status)
		}
	}
	for _, unwanted := range []string{`"current_version"`, `"last_restart_cause"`} {
		if strings.Contains(status, unwanted) {
			t.Fatalf("a refused relaunch is no restart and leaves nothing current (%s): %s", unwanted, status)
		}
	}
	pollUntil(t, 5*time.Second, "no unit of the app left", func() bool { return appUnits(t, app) == "" })
	if code, _ := tryBody(); code < 500 {
		t.Fatalf("nothing serves after the refusal, got %d", code)
	}
	for _, d := range []string{"run", "proxy"} {
		if left := leftIn(t, filepath.Join(root, app, d)); len(left) != 0 {
			t.Fatalf("a refused relaunch leaves nothing in %s/: %v", d, left)
		}
	}
	if st, err := os.ReadFile(filepath.Join(root, app, "state.json")); err != nil || !strings.Contains(string(st), `"current_version": "v1"`) {
		t.Fatalf("state.json keeps the version, so a restart meets the same refusal: %s %v", st, err)
	}

	// A reload's recovery meets the same refusal, once, by name. (A
	// changed config: Caddy skips loading one identical to the running.)
	tester.InitServer(watchdogConfig(root, artifactURL.Port(), app, 49, "10m"), "caddyfile")
	pollUntil(t, 10*time.Second, "recovery's relaunch refused", func() bool {
		_, status = getStatusApp(t, app)
		return strings.Contains(status, `relaunching v1: this box is `+runtime.GOARCH)
	})
	if units := appUnits(t, app); units != "" {
		t.Fatalf("recovery's refusal started a unit:\n%s", units)
	}

	if resp, body := postDeployApp(t, app, artifactSrv.URL+"/demo-v2.tar.gz", "v2"); resp.StatusCode != http.StatusOK {
		t.Fatalf("deploy v2: %d %s", resp.StatusCode, body)
	}
	if code, page := getBody(t); code != http.StatusOK || !strings.HasPrefix(page, "hello v2") {
		t.Fatalf("v2 must serve: %d %q", code, page)
	}
	if _, status = getStatusApp(t, app); strings.Contains(status, named) {
		t.Fatalf("a deploy clears the refusal: %s", status)
	}
}
