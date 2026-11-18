package box

import (
	"bytes"
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
)

// motel box status against the real box_webhook, in-process with
// the box channel's token: the client's own module links no Caddy, so
// the test that stands the handler up lives here, and runs the client
// as the program a person runs. It is built from the workspace
// (go.work), as liveswap's own client tests build it.

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

type clientRun struct {
	code           int
	stdout, stderr string
}

// runClient runs the client with only the environment given, in a home and a
// working directory of its own.
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

func TestClientBoxStatus(t *testing.T) {
	bin := clientBin(t)
	r := newRig(t)
	// The installed file names the box by the host the client asks,
	// which box status must name.
	r.write(t, r.installed, file(strings.Replace(good, "deploy.example.com {", "127.0.0.1 {", 1)))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if err := r.h.ServeHTTP(w, q, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil })); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "box.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := r.token(t)
	// The box is the host of MOTEL_URL, which carries the test
	// server's port.
	env := []string{"MOTEL_TOKEN=" + tok, "MOTEL_URL=" + srv.URL + "/app", "SSL_CERT_FILE=" + cert}

	// No baseline yet: the box's 409, its sentence, exit 1.
	c := runClient(t, bin, env, "box", "status")
	if c.code != 1 || c.stdout != "" || c.stderr != "motel: 127.0.0.1 answered 409; the box says: \""+msgNoBaseline+"\"\n" {
		t.Fatalf("no baseline: %+v", c)
	}
	want409 := r.do(t, req{target: "/", token: tok}).Body.String()
	if c := runClient(t, bin, env, "box", "status", "--json"); c.code != 1 || c.stdout != want409 {
		t.Fatalf("--json prints the box's 409 body: %+v, want %q", c, want409)
	}

	r.applied(t, sha)
	want := r.do(t, req{target: "/", token: tok}).Body.String()
	c = runClient(t, bin, env, "box", "status", "--json")
	if c.code != 0 || c.stdout != want {
		t.Fatalf("exit %d, stdout\n%s\nwant the handler's\n%s\nstderr %s", c.code, c.stdout, want, c.stderr)
	}
	c = runClient(t, bin, env, "box", "status")
	if c.code != 0 || !strings.Contains(c.stdout, "commit           "+sha+"\n") {
		t.Fatalf("human: %+v", c)
	}
}
