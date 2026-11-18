// Package local is what the client reads from the machine it runs on,
// besides its environment: the programs it runs (ssh-keygen, ssh-add,
// git) and the box repository the working directory may be in. It
// writes nothing: no file, no cache, no log.
package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Cmd is one program to run and wait for.
type Cmd struct {
	Name string
	Args []string
	// Dir is the working directory; empty is the client's own.
	Dir string
	// Stdin is the program's whole input.
	Stdin []byte
	// FD3, when not nil, is given to the program as its file
	// descriptor 3, which it can open as /dev/fd/3: a public key for
	// ssh-keygen when the agent holds the only copy of the key, with
	// no file written for it.
	FD3 []byte
	// NoTerminal runs the program so that it cannot reach a terminal:
	// in a session of its own, which has no controlling terminal, so
	// /dev/tty does not open; and with SSH_ASKPASS_REQUIRE=force and an
	// SSH_ASKPASS that fails, so OpenSSH asks no passphrase or PIN on
	// stdin either. A key that needs one then fails at once. The TUI
	// signs so, while its screen holds the terminal.
	NoTerminal bool
	// Stderr receives what the program writes there, as it writes it:
	// a hardware key's "touch your key" prompt must reach the person
	// while ssh-keygen waits. Nil discards it.
	Stderr io.Writer
}

// Runner runs programs. Exec is the real one; a test gives its own.
type Runner interface {
	// Output runs c and returns its stdout. A program that is not
	// installed is an error that matches exec.ErrNotFound; one that
	// exits non-zero, an *exec.ExitError.
	Output(ctx context.Context, c Cmd) ([]byte, error)
}

// waitDelay bounds the wait for a program's pipes after it ends.
const waitDelay = time.Second

// Exec runs programs with os/exec, in the client's own environment, so
// ssh-keygen and ssh-add reach the agent SSH_AUTH_SOCK names. That is
// the one way they have: unlike ssh, neither reads IdentityAgent from
// ~/.ssh/config (the README says so to the person).
type Exec struct{}

// Output runs c.
func (Exec) Output(ctx context.Context, c Cmd) ([]byte, error) {
	//nolint:gosec // G204: the program is one of the client's own three, its arguments the client's; nothing reaches a shell
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	// Once the program has exited or the context has ended, its pipes
	// are waited on this long and no longer: a child it left behind
	// holding one cannot hang the client.
	cmd.WaitDelay = waitDelay
	cmd.Stdin = bytes.NewReader(c.Stdin)
	cmd.Stderr = c.Stderr
	if c.NoTerminal {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		// "false" fails whatever it is asked: no passphrase, no PIN. A
		// hardware key's touch needs no answer, and still works.
		askpass, err := exec.LookPath("false")
		if err != nil {
			askpass = "/usr/bin/false" //nolint:gosec // G101: a program's path, no credential
		}
		cmd.Env = append(os.Environ(), "SSH_ASKPASS="+askpass, "SSH_ASKPASS_REQUIRE=force")
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if c.FD3 != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer func() { _ = r.Close() }()
		// A public key line is far below a pipe's buffer, so it is
		// written whole before the program starts.
		_, err = w.Write(c.FD3)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		cmd.ExtraFiles = []*os.File{r}
	}
	err := cmd.Run()
	return out.Bytes(), err
}

// Repo is the box repository a directory is in: git's top level, the
// boxes at its top (a box is <host>/Caddyfile there, the layout
// examples/box sets up and bin/apply reads), and the box whose
// directory the given directory is inside, if any.
type Repo struct {
	Top   string
	Boxes []string
	Here  string
}

// BoxRepo finds the box repository dir is in. ok is false outside a
// git repository, when git is not installed, and in a repository with
// no box at its top: only a box repository says anything about which
// box, or which key, a command means.
func BoxRepo(ctx context.Context, run Runner, dir string) (Repo, bool) {
	out, err := run.Output(ctx, Cmd{Name: "git", Args: []string{"rev-parse", "--show-toplevel"}, Dir: dir})
	if err != nil {
		return Repo{}, false
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return Repo{}, false
	}
	entries, err := os.ReadDir(top)
	if err != nil {
		return Repo{}, false
	}
	repo := Repo{Top: top}
	for _, e := range entries {
		// A box's main name has a dot; a directory without one (docs,
		// packaging) is the repository's, not a box's, whatever it
		// holds.
		name := e.Name()
		if !IsHostname(name) || !strings.Contains(name, ".") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(top, name, "Caddyfile")); err == nil && fi.Mode().IsRegular() {
			repo.Boxes = append(repo.Boxes, name)
		}
	}
	if len(repo.Boxes) == 0 {
		return Repo{}, false
	}
	// git names the top level by its real path; the directory may be
	// reached through a link.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	// The first element of dir's path under the top level: "." for the
	// top itself and ".." outside it, neither of which is a box's name.
	if rel, err := filepath.Rel(top, dir); err == nil {
		first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		if slices.Contains(repo.Boxes, first) {
			repo.Here = first
		}
	}
	return repo, true
}

// AtTop says whether dir is the repository's top level.
func (r Repo) AtTop(dir string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir) == filepath.Clean(r.Top)
}

// GitConfig reads one key of git's config as seen from dir: "" when it
// is unset or git cannot say.
func GitConfig(ctx context.Context, run Runner, dir, key string) string {
	out, err := run.Output(ctx, Cmd{Name: "git", Args: []string{"config", "--get", key}, Dir: dir})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// IsHostname is the box's name as box.BareHost reads a box_webhook
// address: dot-separated labels of letters, digits and inner hyphens,
// each at most 63 bytes, at most 253 in all; and lowercase, as the
// box's identity is.
func IsHostname(h string) bool {
	// Upper case is refused below, with every byte outside a-z, 0-9
	// and the hyphen.
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// NotInstalled says whether err is a program that is not installed.
func NotInstalled(err error) bool { return errors.Is(err, exec.ErrNotFound) }
