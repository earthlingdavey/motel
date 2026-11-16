package sshkey

import (
	"context"
	"encoding/pem"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestPassphrased: the TUI refuses a key file that would ask for a
// passphrase while its screen holds the terminal, and nothing else.
func TestPassphrased(t *testing.T) {
	const line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrn"                                                  // gitleaks:allow — made up for this test
	const skLine = "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIMuTYJV40qpZ7rH7T4/BJP6ERoj5JgqFQwHIEBTe5wrnAAAABHNzaDo=" // gitleaks:allow — made up for this test
	dir := t.TempDir()
	enc := filepath.Join(dir, "id_enc")
	write(t, enc, string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr")})))
	plain := filepath.Join(dir, "id_plain")
	write(t, plain, string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x04none")})))
	pk, err := ParsePublic(line)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := ParsePublic(skLine)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		key   Key
		agent string
		down  bool
		want  bool
	}{
		{"an encrypted file the agent does not hold", Key{Public: pk, Path: enc}, "", false, true},
		{"an encrypted file and no agent", Key{Public: pk, Path: enc}, "", true, true},
		{"an encrypted file the agent holds", Key{Public: pk, Path: enc}, line + " me@laptop\n", false, false},
		{"a file with no passphrase", Key{Public: pk, Path: plain}, "", false, false},
		{"a hardware key's file with no passphrase", Key{Public: sk, Path: plain}, "", false, false},
		{"named by its .pub, the agent holding the rest", Key{Public: pk, Path: enc + ".pub"}, "", false, false},
		{"the agent's only key", Key{Public: pk}, "", false, false},
	} {
		run := &fakeRun{agent: c.agent}
		if c.down {
			run.agentErr = errors.New("no agent")
		}
		if got := Passphrased(ctx, run, c.key); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// The life cap: no token lasts longer than a box takes.
	now := time.Unix(1794657600, 0)
	iat, exp := TimesLasting(now, 2*MaxLife)
	if exp.Sub(iat) != MaxLife {
		t.Errorf("a token asked for 20 minutes lasts %s, want %s", exp.Sub(iat), MaxLife)
	}
	if iat, exp := TimesLasting(now, 0); exp.Sub(iat) != life {
		t.Errorf("a zero life lasts %s, want the commands' %s", exp.Sub(iat), life)
	}
}
