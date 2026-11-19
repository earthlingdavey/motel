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
	if !strings.Contains(help.stdout, "motel [flags]") || !strings.Contains(help.stdout, "--box") || !strings.Contains(help.stdout, "With no arguments at all") || !strings.Contains(help.stdout, "no box or key to read with, or no screen to draw on.") {
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
// that is no command is still refused before anything starts, on one
// line naming the nearest commands; tui is no command. A word typed with
// a line break or an escape in it, even one that copies cobra's own
// suggestions, prints them visible, inside its quotes, as %q writes
// them; a flag typed so, as termtext.From writes them. A word cobra
// does not look at, after -- or empty (motel "$CMD" with CMD empty), is
// refused in the same words, into a pipe too (no help), and on a
// terminal before anything is signed or sent. With --json, stdout's
// error is that line after "motel: ".
func TestNotACommandStaysRefused(t *testing.T) {
	fb := newFakeBox(t)
	home := keyHome(t)
	found := map[string]string{"MOTEL_KEY": "~/me.pub", "MOTEL_URL": fb.srv.URL + "/blog"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"stauts"}, `unknown command "stauts" for "motel". Did you mean this? status`},
		{[]string{"tui"}, `unknown command "tui" for "motel"`},
		{[]string{"kex"}, `unknown command "kex" for "motel". Did you mean this? key or box`},
		{[]string{"st\nauts\x1b]0;t\x07\u202e"}, `unknown command "st\nauts\x1b]0;t\a\u202e" for "motel"`},
		{[]string{"x\n\nDid you mean this?\n\trm\n"}, `unknown command "x\n\nDid you mean this?\n\trm\n" for "motel"`},
		{[]string{"--fo\no\x1b[2J"}, `unknown flag: --fo\x0ao\x1b[2J`},
		{[]string{"--", "stauts"}, `unknown command "stauts" for "motel"`},
		{[]string{"--", "status", "blog"}, `unknown command "status" for "motel"`},
		{[]string{""}, `unknown command "" for "motel"`},
		{[]string{"--box", "deploy.example.com", "--", "x\x1b[2J"}, `unknown command "x\x1b[2J" for "motel"`},
		{[]string{"--demo", "--", "foo"}, `unknown command "foo" for "motel"`},
	} {
		r := run(t, nil, nil, c.args...)
		if r.code != client.ExitUsage || r.stderr != "motel: "+c.want+"\n" || r.stdout != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr\n%q\nwant 2, nothing on stdout, and\n%q", c.args, r.code, r.stdout, r.stderr, "motel: "+c.want+"\n")
		}
		st := startMotel(t, fb, found, home, true, nil, c.args...)
		if st.code != client.ExitUsage || st.stderr != "motel: "+c.want+"\n" || st.stdout != "" || st.signed != 0 || st.sent != 0 {
			t.Errorf("%q on a terminal: exit %d, stdout %q, stderr %q, %d signed, %d sent; want 2, nothing on stdout, nothing signed or sent, and %q", c.args, st.code, st.stdout, st.stderr, st.signed, st.sent, "motel: "+c.want+"\n")
		}
		r = run(t, nil, nil, append([]string{"--json"}, c.args...)...)
		var e struct{ Error string }
		if r.code != client.ExitUsage || json.Unmarshal([]byte(r.stdout), &e) != nil || "motel: "+e.Error+"\n" != r.stderr || e.Error != c.want {
			t.Errorf("--json %q: exit %d, stdout %q, stderr %q; want 2 and {\"error\":%q}", c.args, r.code, r.stdout, r.stderr, c.want)
		}
	}
	// No word comes near three commands of motel's: cobra's text for one
	// that would.
	three := "unknown command \"q\" for \"motel\"\n\nDid you mean this?\n\ta\n\tb\n\tc\n"
	if got, want := cobraLine(three), `unknown command "q" for "motel". Did you mean this? a, b or c`; got != want {
		t.Errorf("three suggestions read %q, want %q", got, want)
	}
}
