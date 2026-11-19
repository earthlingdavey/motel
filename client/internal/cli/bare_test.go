package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/client/internal/client"
)

// started is what one run of motel did: its exit, its output, and how
// many tokens it signed and requests it sent.
type started struct {
	code           int
	stdout, stderr string
	signed, sent   int
}

// startMotel runs motel with args against the fake box, its stdout a
// terminal or not, and draw as Env.Draw.
func startMotel(t *testing.T, fb *fakeBox, env map[string]string, home string, terminal bool, draw func() error, args ...string) started {
	t.Helper()
	sent := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent++
		return fb.srv.Client().Transport.RoundTrip(r)
	})
	s := &fixedSigner{}
	var out, errOut bytes.Buffer
	code := Main(context.Background(), args, Env{
		Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: &errOut, Terminal: terminal,
		Dir: t.TempDir(), Home: home, Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil },
		Transport: rt, Run: noPrograms{}, Signer: s, Version: "test", Draw: draw,
	})
	return started{code, out.String(), errOut.String(), s.calls, sent}
}

func keyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "me.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr")})
	if err := os.WriteFile(filepath.Join(home, "locked"), enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "locked.pub"), []byte(meLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestBareIntoAPipeIsTheHelp: motel alone with stdout not a terminal
// (a pipe, a file, $(...)) prints its help, exit 0, and signs and sends
// nothing, whatever box and key it could find.
func TestBareIntoAPipeIsTheHelp(t *testing.T) {
	fb := newFakeBox(t)
	home := keyHome(t)
	help := run(t, nil, nil, "--help")
	env := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	r := startMotel(t, fb, env, home, false, nil)
	if r.code != 0 || r.stdout != help.stdout || r.stderr != "" || r.signed != 0 || r.sent != 0 {
		t.Errorf("exit %d, stderr %q, %d signed, %d sent, help printed %v; want 0, nothing said, signed or sent, and the help", r.code, r.stderr, r.signed, r.sent, r.stdout == help.stdout)
	}
	if !strings.Contains(help.stdout, "motel [flags]") || !strings.Contains(help.stdout, "--box") || !strings.Contains(help.stdout, "With no arguments at all") {
		t.Errorf("the help does not say what motel alone does:\n%s", help.stdout)
	}
}

// TestBareOnATerminalTeaches: motel alone on a terminal that finds no
// box, no key it may use, or no screen to draw on prints its help, and
// under it the line that says what it did not find, exit 2, with
// nothing signed or sent.
func TestBareOnATerminalTeaches(t *testing.T) {
	fb := newFakeBox(t)
	home := keyHome(t)
	url := fb.srv.URL + "/blog"
	help := run(t, nil, nil, "--help")
	noTTY := func() error { return errors.New("open /dev/tty: no such device or address") }
	for _, c := range []struct {
		name string
		env  map[string]string
		draw func() error
		want string
	}{
		{"no box", map[string]string{"MOTEL_KEY": "~/me.pub"}, nil,
			"motel: the screen did not open: name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository\n"},
		{"no key", map[string]string{"MOTEL_URL": url}, nil,
			"motel: the screen did not open: name your key: --key ~/.ssh/<key>.pub or MOTEL_KEY\n"},
		{"a deploy token", map[string]string{"MOTEL_URL": url, "MOTEL_TOKEN": "x"}, nil,
			"motel: the screen is asked with your own ssh key, and MOTEL_TOKEN is set, which motel would send instead: a deploy token is no one's key. Name your key with --key, or unset MOTEL_TOKEN.\n"},
		{"a passphrase", map[string]string{"MOTEL_KEY": "~/locked", "MOTEL_URL": url}, nil,
			"motel: the screen did not open: ~/locked is protected by a passphrase, and the agent does not hold it: the screen signs again about every nine minutes while it holds the terminal, where ssh-keygen could not ask for it. Add the key to the agent (ssh-add ~/locked) and run motel again.\n"},
		{"no screen", map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": url}, noTTY,
			"motel: the screen could not start: open /dev/tty: no such device or address\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := startMotel(t, fb, c.env, home, true, c.draw)
			if r.code != client.ExitUsage || r.stderr != c.want || r.stdout != help.stdout || r.signed != 0 || r.sent != 0 {
				t.Errorf("exit %d, stderr %q, %d signed, %d sent, help printed %v; want 2, %q, nothing signed or sent, and the help", r.code, r.stderr, r.signed, r.sent, r.stdout == help.stdout, c.want)
			}
		})
	}
}

// TestFlagsAskForTheScreen: motel with a flag but no command asks for
// the screen, terminal or not, and a step that fails says so as a
// command does: no help, exit 2.
func TestFlagsAskForTheScreen(t *testing.T) {
	fb := newFakeBox(t)
	home := t.TempDir()
	for _, terminal := range []bool{false, true} {
		r := startMotel(t, fb, nil, home, terminal, nil, "--box", "deploy.example.com")
		if r.code != client.ExitUsage || r.stdout != "" || r.stderr != "motel: name your key: --key ~/.ssh/<key>.pub or MOTEL_KEY\n" || r.sent != 0 {
			t.Errorf("terminal %v: %+v", terminal, r)
		}
	}
	r := startMotel(t, fb, nil, home, true, nil, "--json")
	var e struct{ Error string }
	if r.code != client.ExitUsage || json.Unmarshal([]byte(r.stdout), &e) != nil || !strings.Contains(e.Error, "motel alone has no --json") || r.sent != 0 {
		t.Errorf("--json: %+v", r)
	}
}

// TestNotACommandStaysRefused: with the root running the screen, a word
// that is no command is still refused before anything starts, naming
// the nearest; tui is no command.
func TestNotACommandStaysRefused(t *testing.T) {
	for word, near := range map[string]string{"stauts": "status", "tui": ""} {
		r := run(t, nil, nil, word)
		if r.code != client.ExitUsage || !strings.Contains(r.stderr, `unknown command "`+word+`" for "motel"`) || !strings.Contains(r.stderr, near) || r.stdout != "" {
			t.Errorf("%s: %+v", word, r)
		}
	}
}
