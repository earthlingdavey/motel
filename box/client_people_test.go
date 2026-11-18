package box

import (
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// motel box status with a person's token against the real
// box_webhook, with people declared; every key made, and every token
// signed, by ssh-keygen. The box's identity is the address the test
// server listens on: a token's aud and the request's Host must both
// equal it.

type sshKey struct {
	path, typ, b64, fp string
}

// keygen makes a key with the system ssh-keygen, with no passphrase.
func keygen(t *testing.T, dir, name string) sshKey {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: no ssh-keygen on this machine, and the client signs with it; CI's runners have it")
	}
	path := filepath.Join(dir, name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	line, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(line))
	return sshKey{path: path, typ: f[0], b64: f[1], fp: ssh.FingerprintSHA256(pub)}
}

func TestClientBoxStatusPeople(t *testing.T) {
	bin := clientBin(t)
	dir := t.TempDir()
	carol, alice, dave, erin, stranger := keygen(t, dir, "carol"), keygen(t, dir, "alice"), keygen(t, dir, "dave"), keygen(t, dir, "erin"), keygen(t, dir, "stranger")
	var l access.Lines
	for name, k := range map[string]sshKey{"carol@example.test": carol, "alice@example.test": alice, "dave@example.test": dave, "erin@example.test": erin} {
		key, err := access.ParsePerson([]string{name, k.typ, k.b64})
		if err != nil {
			t.Fatal(err)
		}
		l.Keys = append(l.Keys, key)
	}
	for _, line := range []string{"carol@example.test viewer box", "alice@example.test viewer app a", "erin@example.test viewer box from 198.51.100.0/24"} {
		g, err := access.ParseGrant(strings.Fields(line))
		if err != nil {
			t.Fatal(err)
		}
		l.Grants = append(l.Grants, g)
	}
	if _, err := access.CheckLines(l, nil); err != nil {
		t.Fatal(err)
	}

	r := newRig(t)
	r.write(t, r.installed, file(strings.Replace(good, "deploy.example.com {", "127.0.0.1 {", 1)))
	r.h.app.registry = access.NewRegistry("127.0.0.1", l)
	r.applied(t, sha)
	var mu sync.Mutex
	var last string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		rec := httptest.NewRecorder()
		if err := r.h.ServeHTTP(rec, q, nextFunc(func(http.ResponseWriter, *http.Request) error { return nil })); err != nil {
			http.Error(rec, err.Error(), http.StatusInternalServerError)
		}
		mu.Lock()
		last = rec.Body.String()
		mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "box.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	const needsBox = "motel: 127.0.0.1 does not allow your key this: it needs the viewer role on the box. `motel whoami` lists the roles you hold from here.\n"
	for _, c := range []struct {
		who    string
		key    sshKey
		code   int
		stderr string // "" for none; else what stderr holds
	}{
		{"a viewer of the box", carol, 0, ""},
		{"a viewer of one app", alice, 5, needsBox},
		{"a person with no grant", dave, 5, needsBox},
		{"a key no line lists", stranger, 4, "the box's moteld is too old to take a key's token at all"},
		{"a person outside their from list", erin, 4, "your address is outside the from list on every grant you hold"},
	} {
		t.Run(c.who, func(t *testing.T) {
			// The box is the host of MOTEL_URL, which carries the test
			// server's port.
			env := []string{"MOTEL_KEY=" + c.key.path, "MOTEL_URL=" + srv.URL + "/app", "SSL_CERT_FILE=" + cert}
			got := runClient(t, bin, env, "box", "status", "--json")
			mu.Lock()
			body := last
			mu.Unlock()
			if c.code == 4 {
				body = "" // the client's own {"error"}: a 401 has nothing of the box's to print
			}
			if got.code != c.code || (body != "" && got.stdout != body) || (body == "" && !strings.HasPrefix(got.stdout, `{"error":"`)) {
				t.Fatalf("--json: exit %d, stdout %q, want %d and %q; stderr %s", got.code, got.stdout, c.code, body, got.stderr)
			}
			got = runClient(t, bin, env, "box", "status")
			if got.code != c.code || (c.stderr == "") != (got.stderr == "") || !strings.Contains(got.stderr, c.stderr) {
				t.Fatalf("exit %d, stderr %q; want %d and %q", got.code, got.stderr, c.code, c.stderr)
			}
			if c.code == 0 && !strings.Contains(got.stdout, "commit           "+sha+"\n") {
				t.Errorf("human: %q", got.stdout)
			}
		})
	}
}
