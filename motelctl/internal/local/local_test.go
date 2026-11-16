package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestIsHostname(t *testing.T) {
	for _, h := range []string{"deploy.example.com", "a", "127.0.0.1", "x-1.example"} {
		if !IsHostname(h) {
			t.Errorf("%q refused", h)
		}
	}
	for _, h := range []string{"", "Deploy.example.com", "deploy.EXAMPLE.com", "deploy_example.com", "-a.example", "a-.example", "a..example", "a.example.", strings.Repeat("a", 64) + ".example", "deploy.example.com:443", "déploy.example"} {
		if IsHostname(h) {
			t.Errorf("%q taken", h)
		}
	}
}

// git that answers rev-parse with top.
type gitTop string

func (g gitTop) Output(_ context.Context, c Cmd) ([]byte, error) {
	if c.Name == "git" && c.Args[0] == "rev-parse" {
		return []byte(string(g) + "\n"), nil
	}
	return nil, errors.New("exit status 1")
}

// A directory named without a dot is the repository's own, never a
// box, whatever it holds.
func TestBoxRepoNeedsADot(t *testing.T) {
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"docs", "packaging", "deploy.example.com"} {
		if err := os.MkdirAll(filepath.Join(top, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(top, d, "Caddyfile"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, ok := BoxRepo(context.Background(), gitTop(top), filepath.Join(top, "docs"))
	if !ok || !slices.Equal(repo.Boxes, []string{"deploy.example.com"}) || repo.Here != "" {
		t.Fatalf("BoxRepo = %+v, %v; want the one dotted box, and docs not a box", repo, ok)
	}
	if err := os.RemoveAll(filepath.Join(top, "deploy.example.com")); err != nil {
		t.Fatal(err)
	}
	if repo, ok := BoxRepo(context.Background(), gitTop(top), top); ok {
		t.Fatalf("a repository whose only Caddyfiles are in docs/ and packaging/ is a box repository: %+v", repo)
	}
}

// A program that leaves a child holding its stdout cannot hang the
// client past waitDelay.
func TestExecDoesNotWaitForAChildLeftBehind(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: no sh to start a program that leaves a child behind")
	}
	start := time.Now()
	out, err := Exec{}.Output(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "sleep 5 & echo done"}})
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("Output took %s: it waited for the child", took)
	}
	if string(out) != "done\n" || (err != nil && !errors.Is(err, exec.ErrWaitDelay)) {
		t.Fatalf("out %q, err %v", out, err)
	}
}

// FD3 reaches the program as its descriptor 3.
func TestExecFD3(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: no sh")
	}
	out, err := Exec{}.Output(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "cat <&3; cat"}, Stdin: []byte("in\n"), FD3: []byte("three\n")})
	if err != nil || string(out) != "three\nin\n" {
		t.Fatalf("out %q, err %v", out, err)
	}
}

// A box is a regular Caddyfile in a dotted directory, and the shell's
// directory names it only from inside that directory.
func TestBoxRepoHere(t *testing.T) {
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.example.com/apps", "dir.example.com/Caddyfile"} {
		if err := os.MkdirAll(filepath.Join(top, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "a.example.com", "Caddyfile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for dir, here := range map[string]string{
		filepath.Join(top, "a.example.com", "apps"): "a.example.com",
		filepath.Join(top, "a.example.com"):         "a.example.com",
		top:                                         "",
		filepath.Join(top, "dir.example.com"):       "",
		outside:                                     "",
	} {
		repo, ok := BoxRepo(context.Background(), gitTop(top), dir)
		if !ok || !slices.Equal(repo.Boxes, []string{"a.example.com"}) || repo.Here != here {
			t.Errorf("from %s: %+v, %v; want the one box, here %q (a Caddyfile that is a directory is no box)", dir, repo, ok, here)
		}
	}
}
