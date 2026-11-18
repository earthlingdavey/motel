package box

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

func TestWebhookURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(file(strings.Replace(good, "deploy.example.com {", "Deploy.Example.COM {", 1)))
	if got, err := webhookURL(path); err != nil || got != "https://deploy.example.com/" {
		t.Fatalf("%q, %v", got, err)
	}

	write(file(strings.Replace(good, "\tbox_webhook\n", "", 1)))
	if _, err := webhookURL(path); err == nil || err.Error() != path+": the Caddyfile has no site with box_webhook" {
		t.Errorf("no webhook: %v", err)
	}
	write(file(strings.Replace(good, "deploy.example.com {", "*.example.com {", 1)))
	if _, err := webhookURL(path); err == nil || !strings.Contains(err.Error(), "whose address is not one bare hostname (*.example.com)") {
		t.Errorf("wildcard: %v", err)
	}
	write(append(file(good), bytes.Repeat([]byte("#"), 1<<20)...))
	if _, err := webhookURL(path); err == nil || !strings.Contains(err.Error(), "larger than 1048576 bytes") {
		t.Errorf("too big: %v", err)
	}
	if _, err := webhookURL(filepath.Join(dir, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// A pipe is read to its end, as `<(git show HEAD:deploy.example.com/Caddyfile)`
	// hands one over.
	fifo := filepath.Join(dir, "fifo")
	mkfifo(t, fifo)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.Write(file(good))
		_ = f.Close()
	}()
	if got, err := webhookURL(fifo); err != nil || got != "https://deploy.example.com/" {
		t.Errorf("a pipe: %q, %v", got, err)
	}
	// A symlink to a file is followed: this is the operator's own
	// path, not the exchange tree.
	write(file(good))
	if err := os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if got, err := webhookURL(filepath.Join(dir, "link")); err != nil || got != "https://deploy.example.com/" {
		t.Errorf("through a symlink: %q, %v", got, err)
	}
}

// TestBoxCommand runs `moteld box webhook` as caddycmd builds it:
// the registered command, its CobraFunc, its child.
func TestBoxCommand(t *testing.T) {
	cmd, ok := caddycmd.Commands()["box"]
	if !ok || cmd.CobraFunc == nil {
		t.Fatal("no box command with subcommands")
	}
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "box"}
		cmd.CobraFunc(root)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.Execute()
		return out.String(), err
	}
	path := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(path, file(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("webhook", path); err != nil || out != "https://deploy.example.com/\n" {
		t.Fatalf("%q, %v", out, err)
	}
	if _, err := run("webhook"); err == nil {
		t.Error("no file accepted")
	}
	if _, err := run("webhook", path, path); err == nil {
		t.Error("two files accepted")
	}
	// describe: the host, the apps and the signers, a line each, in
	// file order; the signer as its one canonical line.
	want := "host deploy.example.com\napp example\nsigner alice@example.com ssh-ed25519 " + keys[0] + "\nsigner bob ssh-ed25519 " + keys[1] + "\n"
	if out, err := run("describe", path); err != nil || out != want {
		t.Fatalf("describe: %q, %v\nwant %q", out, err, want)
	}
	if _, err := run("describe"); err == nil {
		t.Error("describe: no file accepted")
	}
	if err := os.WriteFile(path, []byte("{\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("webhook", path); err == nil || !strings.Contains(err.Error(), "has no box block") {
		t.Errorf("a refused file: %v", err)
	}
	if out, err := run("describe", path); err == nil || !strings.Contains(err.Error(), "has no box block") {
		t.Errorf("describe, a refused file: %q, %v", out, err)
	}
	// baseline takes exactly one argument; what it does with it is
	// TestBaseline's.
	if _, err := run("baseline"); err == nil {
		t.Error("baseline with no sha accepted")
	}
	if _, err := run("baseline", strings.Repeat("a", 40), "x"); err == nil {
		t.Error("baseline with two arguments accepted")
	}
}

// `moteld init` is a top-level command taking exactly the sha; what
// it does with it is TestInitCmd's.
func TestInitCommand(t *testing.T) {
	cmd, ok := caddycmd.Commands()["init"]
	if !ok || cmd.CobraFunc == nil {
		t.Fatal("no init command")
	}
	for _, args := range [][]string{{}, {strings.Repeat("a", 40), "x"}} {
		c := &cobra.Command{Use: "init"}
		cmd.CobraFunc(c)
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Errorf("init %q accepted", args)
		}
	}
}
