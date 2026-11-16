package sshkey

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/local"
)

// realAgent starts an ssh-agent for the test and points SSH_AUTH_SOCK
// at it.
func realAgent(t *testing.T) {
	t.Helper()
	bin, err := exec.LookPath("ssh-agent")
	if err != nil {
		t.Skip("no ssh-agent")
	}
	out, err := exec.Command(bin, "-s").Output()
	if err != nil {
		t.Fatal(err)
	}
	sock := regexp.MustCompile(`SSH_AUTH_SOCK=([^;]+);`).FindSubmatch(out)
	pid := regexp.MustCompile(`SSH_AGENT_PID=([0-9]+);`).FindSubmatch(out)
	if sock == nil || pid == nil {
		t.Fatalf("ssh-agent said %q", out)
	}
	t.Setenv("SSH_AUTH_SOCK", string(sock[1]))
	t.Cleanup(func() { _ = exec.Command("kill", string(pid[1])).Run() })
}

// sshAdd adds or removes (-d) a key, the passphrase from a program, so
// no terminal is asked.
func sshAdd(t *testing.T, pass string, args ...string) {
	t.Helper()
	dir := t.TempDir()
	askpass := filepath.Join(dir, "pass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho "+pass+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ssh-add", args...)
	cmd.Env = append(os.Environ(), "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add %v: %v %s", args, err, out)
	}
}

// TestNoTerminalSigning signs as the TUI does, with a real encrypted
// key and a real agent: named by its path and by its .pub, the agent
// with and without it, and the key taken out of the agent between two
// signatures. A key the agent does not hold fails at once, saying to
// add it; it never waits for a passphrase.
func TestNoTerminalSigning(t *testing.T) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	realAgent(t)
	path := makeKey(t, bin, "ed25519")
	other := makeKey(t, bin, "ed25519")
	if out, err := exec.Command(bin, "-q", "-p", "-N", "secretpw", "-P", "", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("setting a passphrase: %v %s", err, out)
	}
	l := Lookup{Getenv: func(string) string { return "" }, Run: local.Exec{}}
	byPath, err := l.load(path, SourceFlag)
	if err != nil {
		t.Fatal(err)
	}
	byPub, err := l.load(path+".pub", SourceFlag)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := Keygen{Run: local.Exec{}, NoTerminal: true}
	sign := func(key Key) error {
		start := time.Now()
		_, err := k.Sign(ctx, key, Namespace, []byte(`{"aud":"x"}`))
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("signing took %s: it waited for something", d)
		}
		return err
	}
	// An agent holding another key of the same type does not hold this
	// one: matched by bytes.
	sshAdd(t, "", other)
	for _, key := range []Key{byPath, byPub} {
		if !Passphrased(ctx, local.Exec{}, key) {
			t.Errorf("%s: not said to need a passphrase with the agent holding another key", key.Path)
		}
		err := sign(key)
		if err == nil || !strings.Contains(err.Error(), "ssh-add "+path) || !strings.Contains(err.Error(), "r tries again") {
			t.Errorf("%s with the agent lacking it: %v", key.Path, err)
		}
	}
	sshAdd(t, "secretpw", path)
	for _, key := range []Key{byPath, byPub} {
		if Passphrased(ctx, local.Exec{}, key) {
			t.Errorf("%s: said to need a passphrase with the agent holding it", key.Path)
		}
		if err := sign(key); err != nil {
			t.Errorf("%s with the agent holding it: %v", key.Path, err)
		}
	}
	sshAdd(t, "", "-d", path+".pub")
	if err := sign(byPub); err == nil || !strings.Contains(err.Error(), "ssh-add") {
		t.Errorf("signing after the agent dropped the key: %v", err)
	}
}
