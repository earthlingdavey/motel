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

// motelctl box status against the real box_webhook, in-process with
// the box channel's token: the client's own module links no Caddy, so
// the test that stands the handler up lives here, and runs the client
// as the program a person runs. It is built from the workspace
// (go.work), as liveswap's own motelctl tests build it.

func motelctlBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "motelctl")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/smallhoursorg/motel/motelctl")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		if os.Getenv("GOWORK") == "off" {
			t.Skipf("SKIPPED — NOT TESTED HERE: GOWORK=off, and motelctl is built from the workspace: %s", out)
		}
		t.Fatalf("building motelctl: %v\n%s", err, out)
	}
	return bin
}

type ctlRun struct {
	code           int
	stdout, stderr string
}

// ctl runs motelctl with only the environment given, in a home and a
// working directory of its own.
func ctl(t *testing.T, bin string, env []string, args ...string) ctlRun {
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
		t.Fatalf("running motelctl: %v", err)
	}
	return ctlRun{cmd.ProcessState.ExitCode(), out.String(), errOut.String()}
}

func TestMotelctlBoxStatus(t *testing.T) {
	bin := motelctlBin(t)
	r := newRig(t)
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
	c := ctl(t, bin, env, "box", "status")
	if c.code != 1 || c.stdout != "" || c.stderr != "motelctl: 127.0.0.1 answered 409: "+msgNoBaseline+"\n" {
		t.Fatalf("no baseline: %+v", c)
	}
	want409 := r.do(t, req{target: "/", token: tok}).Body.String()
	if c := ctl(t, bin, env, "box", "status", "--json"); c.code != 1 || c.stdout != want409 {
		t.Fatalf("--json prints the box's 409 body: %+v, want %q", c, want409)
	}

	r.applied(t, sha)
	want := r.do(t, req{target: "/", token: tok}).Body.String()
	c = ctl(t, bin, env, "box", "status", "--json")
	if c.code != 0 || c.stdout != want {
		t.Fatalf("exit %d, stdout\n%s\nwant the handler's\n%s\nstderr %s", c.code, c.stdout, want, c.stderr)
	}
	c = ctl(t, bin, env, "box", "status")
	if c.code != 0 || !strings.Contains(c.stdout, "commit           "+sha+"\n") {
		t.Fatalf("human: %+v", c)
	}
}
