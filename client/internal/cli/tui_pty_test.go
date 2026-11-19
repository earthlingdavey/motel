package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/local"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// keygenWords is what the stand-in ssh-keygen says on stderr at its nth
// signature, as a hardware key's "Confirm user presence" is said.
func keygenWords(n int) string { return fmt.Sprintf("ZZ-KEYGEN-SAYS-%d", n) }

// standInKeygen is a machine whose ssh-keygen signs at once, its nth
// signature the bytes "SSHSIG-<n>", saying keygenWords(n) on stderr;
// it has no other program.
type standInKeygen struct {
	mu sync.Mutex
	n  int
}

func (k *standInKeygen) Output(_ context.Context, c local.Cmd) ([]byte, error) {
	if c.Name != "ssh-keygen" {
		return nil, exec.ErrNotFound
	}
	k.mu.Lock()
	k.n++
	n := k.n
	k.mu.Unlock()
	if c.Stderr != nil {
		_, _ = io.WriteString(c.Stderr, keygenWords(n)+"\r\n")
	}
	sig := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("SSHSIG-%d", n)))
	return []byte("-----BEGIN SSH SIGNATURE-----\n" + sig + "\n-----END SSH SIGNATURE-----\n"), nil
}

// TestTUIPtyHelper is the TUI as `motel tui` starts it, on the pty
// TestTUIRSignsAfresh runs it under: a box that refuses the first token
// signed (as one refused for its times), and answers every later one.
func TestTUIPtyHelper(t *testing.T) {
	if os.Getenv("MOTEL_TEST_TUI_PTY") == "" {
		t.Skip("run by TestTUIRSignsAfresh under a pty")
	}
	first := "." + base64.RawURLEncoding.EncodeToString([]byte("SSHSIG-1"))
	box := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.Header.Get("Authorization"), first) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}` + "\n"))
			return
		}
		switch r.URL.Path {
		case "/_motel/whoami":
			_, _ = w.Write(encode(wire.WhoAmI{Name: "alice@example.com", Key: meFingerprint(), Roles: []wire.RoleHeld{{Role: "viewer", Scope: "app:blog"}},
				Box: "127.0.0.1", Version: "v0.7.0", Protocol: 1, TokenExpiresAt: time.Now().Add(9 * time.Minute)}))
		case "/_motel/apps":
			_, _ = w.Write(encode(wire.Apps{Apps: []wire.AppEntry{{Name: "blog"}}}))
		case "/blog":
			_, _ = w.Write(encode(wire.Status{App: "blog", Phase: "idle"}))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}` + "\n"))
		}
	}))
	defer box.Close()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": box.URL + "/blog"}
	code := Main(context.Background(), []string{"tui"}, Env{
		Getenv: func(k string) string { return env[k] }, Stdout: os.Stdout, Stderr: os.Stderr, Terminal: true,
		Dir: t.TempDir(), Home: home, Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: box.Client().Transport, Run: &standInKeygen{}, Version: "test",
	})
	fmt.Printf("\r\nHELPER EXIT %d\r\n", code)
}

// TestTUIRSignsAfresh: `motel tui` as a person runs it, on a pty, with
// the client, the signer and ssh-keygen's stderr as tui.go builds them.
// The box refuses the token first signed, and the screen says so; r
// signs a new token, from the key, which the box takes, and the screen
// shows who you are. What ssh-keygen says on stderr reaches the
// terminal before the screen holds it (the first signature) and not
// once it does (r's).
func TestTUIRSignsAfresh(t *testing.T) {
	script := ptyScript(t, "r's new token and the stderr gate are not held here")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qec", self+" -test.run=^TestTUIPtyHelper$ -test.count=1", "/dev/null")
	cmd.Env = append(os.Environ(), "MOTEL_TEST_TUI_PTY=1", "TERM=xterm-256color", "COLUMNS=100", "LINES=30", "NO_COLOR=1")
	keys, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var out bytes.Buffer
	more := make(chan struct{}, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		buf := make([]byte, 4096)
		for {
			n, err := outPipe.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			select {
			case more <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}
	waitFor := func(what string) {
		t.Helper()
		for !strings.Contains(seen(), what) {
			select {
			case <-more:
			case <-ended:
				if !strings.Contains(seen(), what) {
					t.Fatalf("the TUI ended before showing %q:\n%q", what, seen())
				}
			case <-ctx.Done():
				t.Fatalf("no %q on the terminal in 30 s:\n%q", what, seen())
			}
		}
	}
	waitFor("SIGNED OUT")
	if _, err := io.WriteString(keys, "r"); err != nil {
		t.Fatal(err)
	}
	waitFor("alice@example.com")
	if _, err := io.WriteString(keys, "q"); err != nil {
		t.Fatal(err)
	}
	waitFor("HELPER EXIT")
	_ = keys.Close()
	<-ended
	_ = cmd.Wait()
	tty := seen()
	if !strings.Contains(tty, "HELPER EXIT 0") {
		t.Errorf("motel tui did not exit 0:\n%q", tty)
	}
	if !strings.Contains(tty, keygenWords(1)) {
		t.Errorf("ssh-keygen's words at the first signature, before the screen, did not reach the terminal:\n%q", tty)
	}
	if strings.Contains(tty, keygenWords(2)) {
		t.Errorf("ssh-keygen's words at r's signature reached the terminal the screen holds:\n%q", tty)
	}
}
