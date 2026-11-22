package liveswap

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// The client's commands against the real liveswap_webhook, in-process
// with a deploy token: the client's own module links no Caddy, so the
// tests that stand the handler up live here, and run the client as the
// program a person runs. It is built from the workspace (go.work), so
// these tests run where the repository's go.work does: make test, CI.

// clientBin builds the client into the test's own directory.
func clientBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "motel")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/smallhoursorg/motel/client/cmd/motel")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		if os.Getenv("GOWORK") == "off" {
			t.Skipf("SKIPPED — NOT TESTED HERE: GOWORK=off, and the client is built from the workspace: %s", out)
		}
		t.Fatalf("building the client: %v\n%s", err, out)
	}
	return bin
}

// clientBox is the webhook behind a TLS server whose certificate the
// client is told to trust, as a box's real one would be.
type clientBox struct {
	srv  *httptest.Server
	url  string // https://127.0.0.1:<port>
	cert string // the server's certificate, for SSL_CERT_FILE
}

func serveClient(t *testing.T, h http.Handler) *clientBox {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	// The refused handshake of the certificate test is the point.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "box.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &clientBox{srv: srv, url: srv.URL, cert: cert}
}

// webhook serves the handler as Caddy would.
func webhook(h *Handler) http.Handler {
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.ServeHTTP(w, r, next); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

type clientRun struct {
	code           int
	stdout, stderr string
}

// runClient runs the client with only the environment given: a home and a
// working directory of its own, so no key or box repository of the
// machine running the tests is read.
func runClient(t *testing.T, bin string, env []string, args ...string) clientRun {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + dir, "PATH=" + os.Getenv("PATH")}, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("running the client: %v", err)
	}
	return clientRun{cmd.ProcessState.ExitCode(), out.String(), errOut.String()}
}

// Each read: the exit code, and --json equal to the handler's own body
// byte for byte, asked of the same handler directly.
func TestClientReadsTheWebhook(t *testing.T) {
	bin := clientBin(t)
	h, _ := newTestHandler(t)
	tok := appToken(t)
	if w := do(t, h, http.MethodPost, "/demo", tok, `{"url":"https://x/a.tgz","version":"v1"}`); w.Code != http.StatusOK {
		t.Fatalf("deploy: %d %s", w.Code, w.Body)
	}
	box := serveClient(t, webhook(h))
	env := []string{"MOTEL_TOKEN=" + tok, "SSL_CERT_FILE=" + box.cert}
	app := box.url + "/demo"

	cases := []struct {
		name   string
		args   []string
		target string
		code   int
		pick   func(body []byte) string // the part of the body --json prints; nil is all of it
	}{
		{"status", []string{"status", app}, "/demo", 0, nil},
		{"status 24h", []string{"status", app, "--range", "24h"}, "/demo?range=24h", 0, nil},
		{"deploys", []string{"deploys", app}, "/demo", 0, func(b []byte) string {
			var st struct{ Deploys json.RawMessage }
			if err := json.Unmarshal(b, &st); err != nil {
				t.Fatal(err)
			}
			return string(st.Deploys) + "\n"
		}},
		{"one deploy", []string{"deploys", app, "v1"}, "/demo?deploy=v1", 0, nil},
		{"an unknown version", []string{"deploys", app, "v9"}, "/demo?deploy=v9", 6, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := do(t, h, http.MethodGet, c.target, tok, "").Body.String()
			if c.pick != nil {
				want = c.pick([]byte(want))
			}
			r := runClient(t, bin, env, append(c.args, "--json")...)
			if r.code != c.code || r.stdout != want {
				t.Fatalf("exit %d, stdout\n%s\nwant %d and the handler's\n%s\nstderr: %s", r.code, r.stdout, c.code, want, r.stderr)
			}
			r = runClient(t, bin, env, c.args...)
			if r.code != c.code || (c.code == 0) != (r.stdout != "") {
				t.Fatalf("human: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
			}
		})
	}
	r := runClient(t, bin, env, "status", app)
	if !strings.HasPrefix(r.stdout, "demo  ") || !strings.Contains(r.stdout, "versions     v1 (running)") {
		t.Errorf("status:\n%s", r.stdout)
	}
	r = runClient(t, bin, env, "deploys", app, "v9")
	if r.stderr != "motel: 127.0.0.1 answered 404; the box says: \"no deploy recorded for version v9\"\n" {
		t.Errorf("an unknown version: %q", r.stderr)
	}
}

// TestClientReadsAppsWhoseValuesMeetTheirNames runs the client against
// the real handler for apps whose env_file values meet their own names,
// motel's words and the markers' words: no filter runs over a status,
// so each comes back as moteld wrote it, with no marker anywhere, and
// the client reads it. An older moteld replaced those values inside the
// status, and withheld the status of an app whose env_file does not
// parse; that app's status is sent whole too.
func TestClientReadsAppsWhoseValuesMeetTheirNames(t *testing.T) {
	bin := clientBin(t)
	const named, staged, keyed, broken = "billingapp", "blog-production", "shopfront", "brokenenv"
	const shaped, spanned, nested, angled = "shop-production", "spannedapp", "nest-production", "deep-production"
	r := newAppsRig(t, named, staged, keyed, broken, shaped, spanned, nested, angled)
	must(t, os.WriteFile(r.apps[angled].spec.envFile, []byte("AKIAABCDEFGHIJKLMNOP=production\nP1=redacted\nP2=REDACTED\nP3=withheld\n"), 0o600)) // gitleaks:allow — made up for this test: a key's name, not a key
	must(t, os.WriteFile(r.apps[nested].spec.envFile, []byte("AKIAABCDEFGHIJKLMNOP=production\nPLACEHOLDER=redacted\n"), 0o600))                  // gitleaks:allow — made up for this test: a key's name, not a key
	must(t, os.WriteFile(r.apps[shaped].spec.envFile, []byte("AKIAABCDEFGHIJKLMNOP=production\n"), 0o600))                                        // gitleaks:allow — made up for this test: a key's name, not a key
	must(t, os.WriteFile(r.apps[spanned].spec.envFile, []byte("K=ucceeded\nJ=withheld: a\n"), 0o600))
	must(t, os.WriteFile(r.apps[named].spec.envFile, []byte("DB_NAME="+named+"\n"), 0o600))
	must(t, os.WriteFile(r.apps[staged].spec.envFile, []byte("APP_ENV=production\n"), 0o600))
	must(t, os.WriteFile(r.apps[keyed].spec.envFile, []byte(keyed+"_db="+keyed+"\n"), 0o600))
	for _, name := range []string{named, staged, keyed, shaped, spanned, nested, angled} {
		ma := r.apps[name]
		must(t, ma.Deploy(context.Background(), deployRequest{url: "https://x/1.tgz", version: "v1", by: "ci"}))
		ma.mu.Lock()
		h := ma.current.handle.(*fakeHandle)
		ma.mu.Unlock()
		h.mu.Lock()
		h.unit = "motel-" + name + ".v1.0a1b2c3d0a1b2c3d.service"
		h.mu.Unlock()
	}
	must(t, os.WriteFile(r.apps[broken].spec.envFile, []byte("this line is no assignment\n"), 0o600))
	r.apps[broken].secretsLoaded = false
	tok := appToken(t)
	box := serveClient(t, webhook(r.h))
	env := []string{"MOTEL_TOKEN=" + tok, "SSL_CERT_FILE=" + box.cert}

	for _, name := range []string{named, staged, keyed, shaped, spanned, nested, angled} {
		w := do(t, r.h, http.MethodGet, "/"+name, tok, "")
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `"app":"`+name+`"`) || !strings.Contains(body, `"unit":"motel-`+name+`.v1.0a1b2c3d0a1b2c3d.service"`) ||
			strings.Contains(body, "redacted") || strings.Contains(body, "REDACTED") || strings.Contains(body, "[#") || strings.Contains(body, "removed>") || strings.Contains(body, "withheld") {
			t.Errorf("GET /%s: %d %s, want moteld's own words as it wrote them", name, w.Code, body)
		}
		got := runClient(t, bin, env, "status", box.url+"/"+name)
		if got.code != 0 || !strings.HasPrefix(got.stdout, name+"  running v1  unit motel-"+name+".v1.0a1b2c3d0a1b2c3d.service  pid 4242\n") {
			t.Errorf("status %s: exit %d\n%s%s", name, got.code, got.stdout, got.stderr)
		}
		if got := runClient(t, bin, env, "deploys", box.url+"/"+name, "v1"); got.code != 0 || !strings.Contains(got.stdout, "v1") {
			t.Errorf("deploys %s v1: exit %d\n%s%s", name, got.code, got.stdout, got.stderr)
		}
		if got := runClient(t, bin, env, "status", box.url+"/"+name, "--json"); got.code != 0 || got.stdout != body {
			t.Errorf("status %s --json: exit %d\n%s\nwant the handler's\n%s", name, got.code, got.stdout, body)
		}
	}
	w := do(t, r.h, http.MethodGet, "/"+broken, tok, "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), `{"app":"`+broken+`"`) {
		t.Errorf("GET /%s, an env_file that does not parse: %d %s", broken, w.Code, w.Body)
	}
	if got := runClient(t, bin, env, "status", box.url+"/"+broken); got.code != 0 || !strings.HasPrefix(got.stdout, broken+"  ") {
		t.Errorf("status of an app whose env_file does not parse: exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

// The faults a person meets: a token the box refuses (after one retry,
// with the clock named when the box's Date says it is off), a
// certificate that does not verify, nothing answering, a 429.
func TestClientFaults(t *testing.T) {
	bin := clientBin(t)
	h, _ := newTestHandler(t)
	served := webhook(h)
	box := serveClient(t, served)
	staleDate := serveClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(-5*time.Minute).UTC().Format(http.TimeFormat))
		served.ServeHTTP(w, r)
	}))
	busy := serveClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"too many requests from this person; retry after 1 s"}` + "\n"))
	}))
	// Port 1 (tcpmux): nothing in a test environment listens there,
	// and no test can take it, as a port just closed could be taken.
	closed := "https://127.0.0.1:1"

	cases := []struct {
		name string
		env  []string
		app  string
		code int
		want string
	}{
		{"a refused token", []string{"MOTEL_TOKEN=not-a-jwt", "SSL_CERT_FILE=" + box.cert}, box.url, 4,
			"refused the token in MOTEL_TOKEN (sent as Bearer). Most often this app's deploy_trust does not accept"},
		// The laptop's clock plays no part in a deploy token, so a
		// Date 5 minutes off is not named.
		{"a refused token, and the box's Date off", []string{"MOTEL_TOKEN=not-a-jwt", "SSL_CERT_FILE=" + staleDate.cert}, staleDate.url, 4,
			"refused the token in MOTEL_TOKEN (sent as Bearer)"},
		{"a self-signed certificate", []string{"MOTEL_TOKEN=" + appToken(t)}, box.url, 8,
			"did not verify; the check says: \"x509: certificate signed by unknown authority\". motel never sends a token over an unverified connection."},
		{"nothing answering", []string{"MOTEL_TOKEN=" + appToken(t)}, closed, 3,
			"(connection refused). If moteld is down, as root on the box: systemctl status moteld"},
		{"a 429 that stands", []string{"MOTEL_TOKEN=" + appToken(t), "SSL_CERT_FILE=" + busy.cert}, busy.url, 3,
			"asked you to slow down; try again in 1 s."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // the refusals each wait out deploy.sh's 3 s retry
			r := runClient(t, bin, c.env, "status", c.app+"/demo", "--json")
			if r.code != c.code || !strings.Contains(r.stderr, c.want) || strings.Contains(r.stderr, "clock") {
				t.Fatalf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, c.code, c.want)
			}
			if !strings.HasPrefix(r.stdout, `{"error":"`) {
				t.Errorf("--json: stdout %q, want an {\"error\"}", r.stdout)
			}
		})
	}
}
