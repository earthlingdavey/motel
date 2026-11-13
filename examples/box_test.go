package examples

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hsbox "github.com/smallhoursorg/motel/box"
	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap"
)

// The box template's scripts — bin/signer, bin/check, bin/apply — in a
// repository made from examples/box, with keys ssh-keygen makes here,
// and bin/apply against a stand-in box that checks every bundle with
// the box's own proof code, as root does.

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// TestMain puts a `sleep` first on PATH that waits a twentieth of what
// it is asked: the scripts under test (bin/apply's poll backoff and
// resends, deploy.sh's resend) wait on the box's answers, which here
// are a test server's, there at once. Their waits are counted by the
// tests, never timed, so a twentieth of the time leaves every answer
// the same [measured: TestBoxApply 74 s to 9.7 s, the package's results
// unchanged]. A duration that is not a plain number goes to the real
// sleep.
func TestMain(m *testing.M) {
	shim, err := os.MkdirTemp("", "sleep-shim")
	if err != nil {
		panic(err)
	}
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		panic(err)
	}
	// Whole seconds alone are scaled, in the shell's integer arithmetic
	// (no awk, whose decimal point is the locale's), their leading zeros
	// dropped (a 0 would make them octal); anything else, nine digits or
	// more among it, goes to the real sleep as asked.
	q := "'" + strings.ReplaceAll(realSleep, "'", `'\''`) + "'"
	script := "#!/bin/sh\n" +
		"[ $# -eq 1 ] || exec " + q + " \"$@\"\n" +
		"case $1 in ''|*[!0-9]*|?????????*) exec " + q + " \"$@\" ;; esac\n" +
		"n=${1#\"${1%%[!0]*}\"}\n" +
		"ms=$((${n:-0} * 50))\n" +
		"exec " + q + " \"$((ms / 1000)).$(printf %03d $((ms % 1000)))\"\n"
	if err := os.WriteFile(filepath.Join(shim, "sleep"), []byte(script), 0o755); err != nil {
		panic(err)
	}
	if err := os.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(shim)
	if builtBin != "" {
		_ = os.RemoveAll(filepath.Dir(builtBin))
	}
	if ageDir != "" {
		_ = os.RemoveAll(ageDir)
	}
	os.Exit(code)
}

// motelBinary is this tree's motel, built once: the scripts' `box
// describe`, `box webhook` and `validate` are the code under test.
func motelBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if buildErr = statMotelSources(); buildErr != nil {
			return
		}
		dir, err := os.MkdirTemp("", "motel-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "motel")
		out, err := exec.Command("go", "build", "-o", builtBin, "github.com/smallhoursorg/motel/cmd/motel").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBin
}

// statMotelSources stats every file of this workspace the motel binary
// is built from, with go.work and each module's go.mod and go.sum. Go's
// test cache keys a package's results on its imports and on the files
// its tests open or stat; motelBinary builds the binary in a
// subprocess, which the cache cannot see, so a change to cmd/motel, or
// to a package of another module that only it imports, left this
// package's results "(cached)" [measured: a line added to
// cmd/motel/main.go, and a test that runs the binary answered
// (cached)]. Each package's directory is stated too, whose time moves
// when a file is added to it or taken out. A module-cache package is
// fixed by its version, which go.sum holds.
func statMotelSources() error {
	const files = `{{if and .Module .Module.Main}}{{$d := .Dir}}{{$d}}{{"\n"}}` +
		`{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}` +
		`{{range .SFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}` +
		`{{.Module.GoMod}}{{"\n"}}{{end}}`
	var stderr bytes.Buffer
	list := exec.Command("go", "list", "-deps", "-f", files, "github.com/smallhoursorg/motel/cmd/motel")
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		return fmt.Errorf("go list motel's sources: %w\n%s", err, stderr.Bytes())
	}
	// The workspace's files are at the root, the parent of this package's.
	paths := []string{filepath.Join("..", "go.work"), filepath.Join("..", "go.work.sum")}
	for _, p := range strings.Split(string(out), "\n") {
		if p == "" {
			continue
		}
		paths = append(paths, p)
		if strings.HasSuffix(p, "/go.mod") {
			paths = append(paths, strings.TrimSuffix(p, "mod")+"sum")
		}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		// The stat is what the cache records; a go.sum or go.work.sum
		// that is not there is recorded as absent, which is as much.
		_, _ = os.Stat(p)
	}
	if len(seen) < 10 {
		return fmt.Errorf("go list named %d of motel's files, too few to be all of them: %q", len(seen), paths)
	}
	return nil
}

// boxRepo is a clone of a repository made from the template: its first
// commit is the template as it ships, unsigned, as GitHub makes it.
type boxRepo struct {
	t    *testing.T
	dir  string
	home string
	env  []string
}

func newBoxRepo(t *testing.T) *boxRepo {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the box template's scripts need %s on PATH", tool)
		}
	}
	root := t.TempDir()
	r := &boxRepo{t: t, dir: filepath.Join(root, "repo"), home: filepath.Join(root, "home")}
	if err := os.MkdirAll(filepath.Join(r.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Nothing of the process's own: no git config, no Actions variables,
	// no curl config, no motel settings.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "GITHUB_") || strings.HasPrefix(k, "ACTIONS_") ||
			strings.HasPrefix(k, "MOTEL") || strings.HasPrefix(k, "CURL_") || k == "HOME" || k == "XDG_CONFIG_HOME" {
			continue
		}
		r.env = append(r.env, kv)
	}
	r.env = append(r.env, "HOME="+r.home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=init.defaultBranch", "GIT_CONFIG_VALUE_0=main",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	err := filepath.WalkDir("box", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("box", path)
		dst := filepath.Join(r.dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	r.git("init", "-q")
	r.git("config", "user.email", "alice@example.com")
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "template")
	return r
}

func (r *boxRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// run runs bin/<script> with env on top of the repository's, and
// returns its combined output and exit status.
func (r *boxRepo) run(script string, env ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(filepath.Join(r.dir, "bin", script))
	cmd.Dir, cmd.Env = r.dir, append(append([]string{}, r.env...), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		r.t.Fatalf("bin/%s: %v\n%s", script, err, out)
	}
	return string(out), 0
}

// key makes an ed25519 key in the repository's HOME/.ssh and returns the
// public half's path.
func (r *boxRepo) key(name string) string {
	r.t.Helper()
	priv := filepath.Join(r.home, ".ssh", name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", priv).CombinedOutput(); err != nil {
		r.t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	return priv + ".pub"
}

// b64 is the base64 field of a public key file.
func (r *boxRepo) b64(pub string) string {
	r.t.Helper()
	f := strings.Fields(r.read(pub))
	if len(f) < 2 {
		r.t.Fatalf("%s: not a public key", pub)
	}
	return f[1]
}

func (r *boxRepo) read(path string) string {
	r.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *boxRepo) write(rel, body string) {
	r.t.Helper()
	path := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *boxRepo) remove(rel string) {
	r.t.Helper()
	if err := os.RemoveAll(filepath.Join(r.dir, rel)); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits everything, signed as the checkout's config says, and
// returns the commit.
func (r *boxRepo) commit(msg string, extra ...string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git(append(append([]string{}, extra...), "commit", "-q", "--allow-empty", "-m", msg)...)
	return r.git("rev-parse", "HEAD")
}

const boxFile = "deploy.example.com/Caddyfile"

func TestBoxSigner(t *testing.T) {
	t.Parallel()
	r := newBoxRepo(t)
	if out, code := r.run("signer"); code != 1 || !strings.Contains(out, "no key at "+filepath.Join(r.home, ".ssh", "id_ed25519.pub")) {
		t.Fatalf("no key: exit %d\n%s", code, out)
	}
	alice := r.key("id_ed25519")
	if out, code := r.run("signer", "KEY="+strings.TrimSuffix(alice, ".pub")); code != 1 || !strings.Contains(out, "KEY is the public half") {
		t.Fatalf("a private key as KEY: exit %d\n%s", code, out)
	}
	if out, code := r.run("signer", "PRINCIPAL=alice smith"); code != 1 || !strings.Contains(out, "letters, digits and . _ @ + - only") {
		t.Fatalf("a principal with a space: exit %d\n%s", code, out)
	}
	before := r.read(boxFile)
	// A key whose blob is not a key: refused before any file changes.
	bad := filepath.Join(r.home, ".ssh", "bad.pub")
	if err := os.WriteFile(bad, []byte("ssh-ed25519 AAAAnotakey= bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := r.run("signer", "KEY="+bad); code != 1 || !strings.Contains(out, "ssh-keygen cannot read it as a public key") || r.read(boxFile) != before {
		t.Fatalf("a malformed key: exit %d\n%s", code, out)
	}
	// The defaults: ~/.ssh/id_ed25519.pub, as the checkout's git email.
	// The first signer of a box that lists none: no box runs this file
	// yet, so nothing is said about one that does.
	const running = "signer: on a box that already runs, the commit that adds you must be signed by a signer it lists:\n"
	if out, code := r.run("signer"); code != 0 || strings.Contains(out, running) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	line := "\t\tsigner alice@example.com ssh-ed25519 " + r.b64(alice) + "\n"
	after := r.read(boxFile)
	if want := strings.Replace(before, "\t\t# signers\n", "\t\t# signers\n"+line, 1); after != want {
		t.Fatalf("the Caddyfile after make signer:\n%s\nwant:\n%s", after, want)
	}
	for k, want := range map[string]string{
		"gpg.format":      "ssh",
		"user.signingkey": strings.TrimSuffix(alice, ".pub"), // the private file beside the .pub
		"commit.gpgsign":  "true",
	} {
		if got := r.git("config", "--local", k); got != want {
			t.Errorf("git config %s = %q, want %q", k, got, want)
		}
	}
	// A file with other unstaged edits gains the line but is not staged:
	// the signer's commit carries nothing else.
	r.git("reset", "-q")
	r.write(boxFile, strings.Replace(after, "\t\t# signers\n"+line, "\t\t# signers\n", 1)+"# an edit of mine\n")
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("unstaged edits: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--cached", "--name-only"); got != "" {
		t.Errorf("staged over unstaged edits: %q", got)
	}
	// Nor over edits already staged.
	r.write(boxFile, strings.Replace(after, "\t\t# signers\n"+line, "\t\t# signers\n", 1)+"# an edit of mine\n")
	r.git("add", boxFile)
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "has other edits, or is not committed yet, so it is not staged") {
		t.Fatalf("staged edits: exit %d\n%s", code, out)
	}
	if got := r.git("diff", "--name-only"); got != boxFile {
		t.Errorf("the signer line was staged over staged edits: unstaged %q", got)
	}
	r.git("reset", "-q")
	r.write(boxFile, after)
	r.git("add", boxFile)

	// Again: nothing added, signing still set up.
	if got := r.git("diff", "--cached", "--name-only"); got != boxFile {
		t.Errorf("staged after make signer: %q", got)
	}
	if out, code := r.run("signer"); code != 0 || !strings.Contains(out, "already lists this key, as alice@example.com") || r.read(boxFile) != after || strings.Contains(out, running) {
		t.Fatalf("a second run: exit %d\n%s", code, out)
	}

	// A second box with no marker: refused before either file changes.
	other := strings.Replace(after, "deploy.example.com {", "deploy2.example.com {", 1)
	r.write("deploy2.example.com/Caddyfile", strings.Replace(other, "# signers", "# keys", 1))
	bob := r.key("bob")
	if out, code := r.run("signer", "KEY="+bob, "PRINCIPAL=bob"); code != 1 || !strings.Contains(out, "deploy2.example.com/Caddyfile has no '# signers' line") || r.read(boxFile) != after {
		t.Fatalf("a box with no marker: exit %d\n%s", code, out)
	}
	// With it, every box gains the key; a key given as ~/... is expanded.
	// The boxes list alice already, so they may run: bob's commit must be
	// signed by her, and the run says so.
	r.write("deploy2.example.com/Caddyfile", other)
	if out, code := r.run("signer", "KEY=~/.ssh/bob.pub", "PRINCIPAL=bob"); code != 0 || !strings.Contains(out, running) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, f := range []string{boxFile, "deploy2.example.com/Caddyfile"} {
		if !strings.Contains(r.read(f), "\t\tsigner bob ssh-ed25519 "+r.b64(bob)+"\n\t\tsigner alice@example.com") {
			t.Errorf("%s lacks bob's line, first under the marker:\n%s", f, r.read(f))
		}
	}
	if got := r.git("config", "--local", "user.signingkey"); got != strings.TrimSuffix(bob, ".pub") {
		t.Errorf("user.signingkey = %q after bob's make signer", got)
	}
	// A box under a name git would quote by default (core.quotePath)
	// gains it too.
	r.write("café/Caddyfile", other)
	if out, code := r.run("signer", "KEY=~/.ssh/bob.pub", "PRINCIPAL=bob"); code != 0 || !strings.Contains(r.read("café/Caddyfile"), "\t\tsigner bob ssh-ed25519 "+r.b64(bob)+"\n") {
		t.Fatalf("a box under a non-ASCII name: exit %d\n%s", code, out)
	}
}

func TestBoxCheck(t *testing.T) {
	t.Parallel()
	bin := motelBinary(t)
	r := newBoxRepo(t)
	check := func() (string, int) { return r.run("check", "MOTEL="+bin) }
	// The template as it ships: no signer yet.
	if out, code := check(); code != 1 || !strings.Contains(out, "has no signer (make signer adds yours)") {
		t.Fatalf("the template as it ships: exit %d\n%s", code, out)
	}
	alice := r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	if out, code := check(); code != 0 || out != "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)\n" {
		t.Fatalf("the template with a signer: exit %d\n%s", code, out)
	}
	good := r.read(boxFile)

	// The directory is the host.
	r.git("mv", "deploy.example.com", "box1")
	if out, code := check(); code != 1 || !strings.Contains(out, "box1/Caddyfile: its box_webhook host is deploy.example.com, so it belongs at deploy.example.com/Caddyfile (git mv box1 deploy.example.com)") {
		t.Fatalf("a box under another name: exit %d\n%s", code, out)
	}
	// A name git would quote by default (core.quotePath) is read as it
	// is, not skipped.
	r.git("mv", "box1", "café")
	if out, code := check(); code != 1 || !strings.Contains(out, "café/Caddyfile: its box_webhook host is deploy.example.com, so it belongs at deploy.example.com/Caddyfile (git mv café deploy.example.com)") {
		t.Fatalf("a box under a non-ASCII name: exit %d\n%s", code, out)
	}
	r.git("mv", "café", "deploy.example.com")

	// One level down, nothing shallower or deeper; new files count.
	for _, f := range []string{"Caddyfile", "archive/old.example.com/Caddyfile"} {
		r.write(f, good)
		if out, code := check(); code != 1 || !strings.Contains(out, f+": a box's file is <host>/Caddyfile at the top of the repository") {
			t.Fatalf("%s: exit %d\n%s", f, code, out)
		}
		r.remove(f)
	}
	// A name git quotes even so is named as git shows it; tracked but
	// deleted here, it is on its way out.
	for dir, shown := range map[string]string{`a"b`: `"a\"b/Caddyfile"`, `a\b`: `"a\\b/Caddyfile"`, "a\tb": `"a\tb/Caddyfile"`, "a\nb": `"a\nb/Caddyfile"`} {
		r.write(dir+"/Caddyfile", good)
		if out, code := check(); code != 1 || !strings.Contains(out, "check: "+shown+": git quotes this name") {
			t.Fatalf("%q: exit %d\n%s", dir, code, out)
		}
		r.git("add", dir+"/Caddyfile")
		r.remove(dir)
		if out, code := check(); code != 0 {
			t.Fatalf("%q tracked but deleted: exit %d\n%s", dir, code, out)
		}
		r.git("rm", "-q", "--cached", "--", dir+"/Caddyfile")
	}

	// Every box lists the same keys; a principal may differ.
	second := strings.Replace(good, "deploy.example.com {", "deploy2.example.com {", 1)
	r.write("deploy2.example.com/Caddyfile", strings.Replace(second, "alice@example.com", "alice", 1))
	if out, code := check(); code != 0 || !strings.Contains(out, "deploy2.example.com/Caddyfile → https://deploy2.example.com/ (1 app, 1 signer)") {
		t.Fatalf("the same key under another name: exit %d\n%s", code, out)
	}
	bob := r.key("bob")
	r.write("deploy2.example.com/Caddyfile", strings.Replace(second, r.b64(alice), r.b64(bob), 1))
	if out, code := check(); code != 1 || !strings.Contains(out, "deploy2.example.com/Caddyfile lists other signer keys than deploy.example.com/Caddyfile") {
		t.Fatalf("another key: exit %d\n%s", code, out)
	}
	r.remove("deploy2.example.com")

	// A symlink: git stores its target's name, which the box refuses.
	r.write("real/Caddyfile", good)
	r.remove(boxFile)
	if err := os.Symlink("../real/Caddyfile", filepath.Join(r.dir, boxFile)); err != nil {
		t.Fatal(err)
	}
	if out, code := check(); code != 1 || !strings.Contains(out, boxFile+": a box's file is a regular file, not a symlink") {
		t.Fatalf("a symlinked file: exit %d\n%s", code, out)
	}
	r.remove("real")
	r.remove(boxFile)
	// Every box deleted here: nothing to check is not a pass.
	if out, code := check(); code != 1 || !strings.Contains(out, "no box here") {
		t.Fatalf("every box deleted: exit %d\n%s", code, out)
	}

	// What the box itself would refuse: validate's.
	r.write(boxFile, strings.Replace(good, "\tliveswap_webhook\n", "\tliveswap_webhook\n\tno_such_directive\n", 1))
	if out, code := check(); code != 1 || !strings.Contains(out, boxFile+" does not validate:") || !strings.Contains(out, "no_such_directive") {
		t.Fatalf("a file validate refuses: exit %d\n%s", code, out)
	}

	// A describe or a validate that fails and says nothing (killed, or
	// crashed): the file and the command that failed are named.
	r.write(boxFile, good)
	silent := filepath.Join(r.home, "silent-motel")
	script := "#!/bin/sh\n" +
		"case \"$FAIL $1\" in\n" +
		"\"describe box\"|\"validate validate\") exit 1 ;;\n" +
		"esac\n" +
		"exec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(silent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for fail, want := range map[string]string{
		"describe": "check: " + boxFile + ": " + silent + " box describe failed, and said nothing\n",
		"validate": "check: " + boxFile + " does not validate:\n  " + silent + " validate failed, and said nothing\n",
	} {
		if out, code := r.run("check", "MOTEL="+silent, "REAL="+bin, "FAIL="+fail); code != 1 || !strings.Contains(out, want) {
			t.Errorf("%s fails and says nothing: exit %d\n%s", fail, code, out)
		}
	}
}

// standIn is one box's box_webhook as bin/apply meets it. Each bundle
// is checked with box/proof — parser, chain, file proof, every
// signature by ssh-keygen against box.Walk's signer lists — what root
// runs, less the install; a bundle that does not parse or prove is the
// script's bug and fails the test.
type standIn struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	baseline  string
	installed []byte
	path      string
	results   map[string][]byte
	ids       map[string]bool
	tokens    map[string]bool // minted here: used yet?
	mints     int
	bundles   int
	onGet     string            // "": the baseline; "401"; "401once", a 401 and then the baseline; "front401", a proxy's; "nobaseline"
	onPost    []string          // a behaviour per POST, in order: "admitting", "unread", "never", "proxy504", "front<code>", "html409", "504", "502", "524", "lost", "sync"; then "": 202
	edited    bool              // results say the file was edited on the box
	recipient string            // GET /'s box key; "" leaves it out, as a box without one does
	secrets   map[string]string // the secrets the box runs: <app>/<VAR> → the value's blob
	envEdited []string          // results say these apps' env files were edited on the box
	pollGaps  []int             // polls answered, in order, by something in front of the box
	mintFails int               // token requests that fail before one succeeds
	audience  string            // the last token request's audience, decoded
}

var boxID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// secretNames are the grammars root holds the secrets' names to
// (box/secrets.go): an app's name as box/tokens.go reads one, or the
// box's own slot, _box; a variable's as liveswap does.
var secretNames = proof.Names{App: func(s string) bool { return s == "_box" || appNameRe.MatchString(s) }, Var: liveswap.ValidEnvKey}

var appNameRe = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

func newStandIn(t *testing.T, baseline string, installed []byte) *standIn {
	s := &standIn{t: t, baseline: baseline, installed: installed, path: boxFile,
		results: map[string][]byte{}, ids: map[string]bool{}, tokens: map[string]bool{}}
	s.srv = httptest.NewTLSServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// set changes the stand-in under its lock, and seen copies what it
// observed: the script's requests arrive on the server's goroutines.
func (s *standIn) set(f func(s *standIn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type seen struct {
	baseline, audience string
	bundles, mints     int
	unused             []string // tokens minted and never sent
}

func (s *standIn) seen() seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := seen{baseline: s.baseline, audience: s.audience, bundles: s.bundles, mints: s.mints}
	for tok, used := range s.tokens {
		if !used {
			v.unused = append(v.unused, tok)
		}
	}
	return v
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// reply answers as the box does (liveswap.RespondJSON): json.Marshal,
// which escapes <, > and & — bin/apply must undo that.
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(encode(v))
}

func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/token" {
		if s.mintFails > 0 {
			s.mintFails--
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		s.mints++
		s.audience = r.URL.Query().Get("audience")
		tok := fmt.Sprintf("minted-%d", s.mints)
		s.tokens[tok] = false
		reply(w, 200, map[string]string{"value": tok})
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		s.t.Errorf("%s %s without a token", r.Method, r.URL)
	}
	if used, minted := s.tokens[tok]; minted {
		if used {
			s.t.Errorf("%s %s: token %s used twice", r.Method, r.URL, tok)
		}
		s.tokens[tok] = true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.RawQuery != "":
		id := r.URL.Query().Get("result")
		if !boxID.MatchString(id) || r.URL.RawQuery != "result="+id {
			s.t.Errorf("a poll of %q", r.URL.RawQuery)
		}
		if len(s.pollGaps) > 0 {
			code := s.pollGaps[0]
			s.pollGaps = s.pollGaps[1:]
			proxied(w, code)
			return
		}
		res, ok := s.results[id]
		if !ok {
			reply(w, 404, map[string]string{"error": "no result and no marker for " + id + ": swept, or never admitted"})
			return
		}
		var ph struct{ Phase string }
		_ = json.Unmarshal(res, &ph)
		code := 422
		if ph.Phase == "applied" || ph.Phase == "no_change" {
			code = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(res)
	case r.Method == http.MethodGet:
		switch s.onGet {
		case "401", "401once":
			if s.onGet == "401once" {
				s.onGet = ""
			}
			reply(w, 401, map[string]string{"error": "invalid or missing deploy token (Authorization: Bearer <jwt>)"})
		case "front401":
			proxied(w, 401)
		case "nobaseline":
			reply(w, 409, map[string]string{"error": "this box has no baseline; motel init <sha>, as root inside the box's directory, sets one"})
		default:
			st := map[string]string{"commit": s.baseline, "box_webhook": "deploy.example.com", "sha256": digest(s.installed)}
			if s.recipient != "" {
				st["recipient"] = s.recipient
			}
			reply(w, 200, st)
		}
	case r.Method == http.MethodPost:
		s.post(w, r)
	default:
		s.t.Errorf("%s %s", r.Method, r.URL)
	}
}

func (s *standIn) post(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Box-Request-Id")
	if !boxID.MatchString(id) || r.Header.Get("Content-Type") != "application/gzip" {
		s.t.Errorf("POST: id %q, Content-Type %q", id, r.Header.Get("Content-Type"))
	}
	// The server's goroutine: a fault is an error and a 500, never
	// FailNow.
	fault := func(format string, args ...any) {
		s.t.Errorf(format, args...)
		reply(w, 500, map[string]string{"error": "the stand-in failed the test"})
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fault("reading the bundle: %v", err)
		return
	}
	how := ""
	if len(s.onPost) > 0 {
		how, s.onPost = s.onPost[0], s.onPost[1:]
	}
	if how == "admitting" {
		reply(w, 409, map[string]string{"error": "a push is being admitted; retry in a moment"})
		return
	}
	if how == "unread" { // the upload broke: nothing admitted, the id not held
		reply(w, 400, map[string]string{"error": "could not read the bundle: unexpected EOF"})
		return
	}
	if how == "proxy504" { // a proxy in front gave up before the box had it
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		return
	}
	if front, ok := strings.CutPrefix(how, "front"); ok { // the same, in a CDN's code
		code, err := strconv.Atoi(front)
		if err != nil {
			fault("onPost %q", how)
			return
		}
		proxied(w, code)
		return
	}
	if how == "html409" { // something in front of the box, in its own words
		http.Error(w, "<html>conflict</html>", http.StatusConflict)
		return
	}
	if how == "never" { // the connection broke before the box read a byte
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	if s.ids[id] {
		reply(w, 409, map[string]string{"error": "duplicate request id: poll /?result=" + id})
		return
	}
	s.ids[id] = true
	b, err := proof.ReadBundle(body)
	if err != nil {
		fault("the bundle: %v", err)
		return
	}
	s.bundles++
	if b.Path != s.path {
		s.t.Errorf("bundle path %q, want %q", b.Path, s.path)
	}
	if err := proof.ProveFile(b.Commit, b.Trees, b.Path, b.Caddyfile); err != nil {
		fault("the file proof: %v", err)
		return
	}
	// The secrets, as root proves them (step 13): every value the commit
	// lists must be in the bundle. The stand-in cannot decrypt, so a
	// value changed is a blob changed.
	sec, err := proof.ProveSecrets(b.Commit, b.Trees, b.Blobs, b.Path, secretNames)
	if err != nil {
		fault("the secrets proof: %v", err)
		return
	}
	values := map[string]string{}
	for _, a := range sec.Apps {
		for _, v := range a.Vars {
			values[a.Name+"/"+v.Name] = v.Blob
		}
	}
	chain, err := proof.Chain(b.Commit, b.Parents, s.baseline)
	if err != nil {
		fault("the chain: %v", err)
		return
	}
	if b.Commit.ID == s.baseline && bytes.Equal(b.Caddyfile, s.installed) && maps.Equal(values, s.secrets) {
		// The fast path writes nothing: no id held, no result to poll.
		delete(s.ids, id)
		if how == "lost" {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		reply(w, 200, map[string]string{"id": id, "phase": "no_change", "commit": b.Commit.ID, "path": b.Path})
		return
	}
	cur, err := hsbox.Walk(s.installed)
	if err != nil {
		fault("the installed file: %v", err)
		return
	}
	next, err := hsbox.Walk(b.Caddyfile)
	if err != nil {
		fault("the pushed file: %v", err)
		return
	}
	signer, err := proof.VerifyChain(context.Background(), &proof.Verifier{}, chain, cur.Signers, next.Signers, s.baseline)
	var ref *proof.Refusal
	if errors.As(err, &ref) {
		reply(w, 422, map[string]any{"id": id, "phase": "refused", "commit": b.Commit.ID, "path": b.Path, "caddyfile_edited_out_of_band": false, "error": ref.Msg})
		return
	}
	if err != nil {
		fault("the signatures: %v", err)
		return
	}
	res := map[string]any{"id": id, "phase": "verified", "commit": b.Commit.ID, "path": b.Path, "signer": signer,
		"box_webhook": "deploy.example.com", "caddyfile_edited_out_of_band": s.edited, "apps": next.Apps}
	verified := encode(res)
	res["phase"] = "applied"
	// A diff is the Caddyfile's: none when the push changed env files
	// alone, as the box's (box/diff.go).
	if !bytes.Equal(b.Caddyfile, s.installed) {
		res["diff"] = "--- installed\n+++ pushed\n@@ a hunk @@\n-\told \"line\"\n+\tnew\tline <a & b>\x1b[2J\r\n"
	}
	var added, changed, removed []string
	for k, blob := range values {
		switch old, ok := s.secrets[k]; {
		case !ok:
			added = append(added, k)
		case old != blob:
			changed = append(changed, k)
		}
	}
	for k := range s.secrets {
		if _, ok := values[k]; !ok {
			removed = append(removed, k)
		}
	}
	for key, names := range map[string][]string{"secrets_added": added, "secrets_changed": changed, "secrets_removed": removed, "secrets_edited_out_of_band": s.envEdited} {
		if len(names) > 0 {
			slices.Sort(names)
			res[key] = names
		}
	}
	// As the box's txn.reloads: the Caddyfile, or the box's own env
	// file's bytes — which a name in the lists or an out-of-band edit of
	// _box.env changes.
	if res["diff"] != nil || slices.Contains(s.envEdited, "_box") || slices.ContainsFunc(slices.Concat(added, changed, removed), func(n string) bool { return strings.HasPrefix(n, "_box/") }) {
		res["reloaded"] = true
	}
	s.results[id] = encode(res)
	s.baseline, s.installed, s.secrets = b.Commit.ID, b.Caddyfile, values
	switch how {
	case "504":
		reply(w, 504, map[string]string{"id": id})
	case "502": // a proxy in front of the box gave up on the answer
		http.Error(w, "bad gateway", http.StatusBadGateway)
	case "524": // Cloudflare's origin timeout: the box had the push
		proxied(w, 524)
	case "lost":
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case "sync":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(s.results[id])
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write(verified)
	}
}

// proxied answers as a proxy in front of the box does: its own status
// and an HTML page, never the box's JSON.
func proxied(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<html>Error %d</html>", code)
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

// applyRig is a repository whose box runs its first signed commit — as
// after `motel init` — and the stand-ins its boxes are reached at.
type applyRig struct {
	*boxRepo
	bin      string
	base     string
	box      *standIn
	curlHome string
}

func newApplyRig(t *testing.T) *applyRig {
	t.Helper()
	g := &applyRig{boxRepo: newBoxRepo(t), bin: motelBinary(t)}
	g.key("id_ed25519")
	if out, code := g.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	g.base = g.commit("deploy.example.com: the box's config")
	g.box = newStandIn(t, g.base, []byte(g.read(boxFile)))
	g.curlHome = t.TempDir()
	g.route(map[string]*standIn{"deploy.example.com": g.box})
	return g
}

// route points each host at its stand-in, by curl's own config file:
// bin/apply runs curl unchanged, against the https:// address the
// committed file names.
func (g *applyRig) route(hosts map[string]*standIn) {
	g.t.Helper()
	var rc strings.Builder
	for host, s := range hosts {
		fmt.Fprintf(&rc, "connect-to = %s:443:%s\n", host, s.srv.Listener.Addr().String())
		ca := filepath.Join(g.curlHome, host+".pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}), 0o644); err != nil {
			g.t.Fatal(err)
		}
		fmt.Fprintf(&rc, "cacert = %s\n", ca)
	}
	if err := os.WriteFile(filepath.Join(g.curlHome, ".curlrc"), []byte(rc.String()), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// apply runs bin/apply from a laptop: a fixed token.
func (g *applyRig) apply(env ...string) (string, int) {
	g.t.Helper()
	return g.run("apply", append([]string{"MOTEL=" + g.bin, "CURL_HOME=" + g.curlHome, "MOTEL_TOKEN=laptop"}, env...)...)
}

// runArgs runs bin/apply from a laptop with arguments.
func (g *applyRig) runArgs(args []string) (string, int) {
	g.t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, "bin", "apply"), args...)
	cmd.Dir = g.dir
	cmd.Env = append(append([]string{}, g.env...), "MOTEL="+g.bin, "CURL_HOME="+g.curlHome, "MOTEL_TOKEN=laptop")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		g.t.Fatalf("bin/apply: %v\n%s", err, out)
	}
	return string(out), 0
}

// change commits a change to the box's file and returns the commit.
func (g *applyRig) change(msg string, extra ...string) string {
	g.t.Helper()
	g.write(boxFile, strings.Replace(g.read(boxFile), "dynamic liveswap example", "dynamic liveswap example # "+msg, 1))
	return g.commit(msg, extra...)
}

func TestBoxApply(t *testing.T) {
	t.Parallel()
	t.Run("the commit the box runs", func(t *testing.T) {
		g := newApplyRig(t)
		out, code := g.apply()
		if code != 0 || !strings.Contains(out, "deploy.example.com at "+g.base[:7]+": the box already runs this Caddyfile") || g.box.seen().bundles != 1 {
			t.Fatalf("exit %d, %d bundles\n%s", code, g.box.seen().bundles, out)
		}
		// Its 200 lost: the fast path left no result, so the push is sent
		// again and answered the same way. By directory, as an operator
		// might type it.
		g.box.set(func(s *standIn) { s.onPost, s.bundles = []string{"lost"}, 0 })
		if out, code = g.runArgs([]string{"./deploy.example.com/"}); code != 0 || !strings.Contains(out, "the box already runs this Caddyfile") {
			t.Fatalf("a lost no_change: exit %d\n%s", code, out)
		}
		if out, code := g.runArgs([]string{"archive/deploy.example.com"}); code != 1 || !strings.Contains(out, "is not a box: name its directory") {
			t.Fatalf("a nested name: exit %d\n%s", code, out)
		}
	})

	t.Run("a chain applied, then the same again", func(t *testing.T) {
		g := newApplyRig(t)
		g.write("README.md", "a commit that does not touch the box's file\n")
		g.commit("readme")
		head := g.change("one")
		out, code := g.apply()
		for _, want := range []string{
			"the box runs " + g.base[:7] + "; sending " + head[:7] + " (2 commits)",
			"deploy.example.com at " + head[:7] + ", signed by alice@example.com; verified; installing",
			// A control character, and a carriage return, stay escaped.
			"--- installed\n+++ pushed\n@@ a hunk @@\n-\told \"line\"\n+\tnew\tline <a & b>\\u001b[2J\\r\n",
			"deploy.example.com at " + head[:7] + ": loaded; running apps are not restarted; apps example",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("no %q in the output", want)
			}
		}
		if code != 0 || g.box.seen().baseline != head || strings.ContainsAny(out, "\x1b\r") {
			t.Fatalf("exit %d, baseline %s\n%q", code, g.box.seen().baseline, out)
		}
		if out, code := g.apply(); code != 0 || !strings.Contains(out, "the box already runs this Caddyfile") {
			t.Fatalf("again: exit %d\n%s", code, out)
		}
	})

	t.Run("a later run got there first", func(t *testing.T) {
		g := newApplyRig(t)
		g.change("one")
		two := g.change("two")
		g.box.set(func(s *standIn) { s.baseline = two })
		g.git("checkout", "-q", "HEAD~1")
		out, code := g.apply()
		if code != 0 || !strings.Contains(out, "superseded, nothing to send") || g.box.seen().bundles != 0 {
			t.Fatalf("exit %d, %d bundles\n%s", code, g.box.seen().bundles, out)
		}
	})

	t.Run("an unsigned commit under a signed one", func(t *testing.T) {
		g := newApplyRig(t)
		wip := g.change("wip", "-c", "commit.gpgsign=false")
		head := g.change("signed")
		out, code := g.apply()
		if code != 1 || !strings.Contains(out, wip+", between the commit this box runs and "+head+", is not signed") || g.box.seen().baseline != g.base {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("an answer that comes late, or not at all", func(t *testing.T) {
		for how, said := range map[string]string{
			"504":       "the box has the push; asking for its result",
			"lost":      "no answer from the box to the push; asking for its result",
			"admitting": "a push is being admitted; retry in a moment; trying again",
			"sync":      ": loaded; running apps are not restarted",
			"unread":    "could not read the bundle: unexpected EOF; sending it again",
			"502":       "no answer from the box to the push; asking for its result",
			// Cloudflare's origin timeout, the push admitted behind it.
			"524": "no answer from the box to the push; asking for its result",
			// Cloudflare could not reach the box: never seen, sent again.
			"front520": "no answer from the box to the push; asking for its result",
			// Sent again, and Cloudflare answers that: not yet.
			"never,front522": "no answer from the box to the push; asking for its result",
			// Sent again, admitted, and its answer timed out in front.
			"never,524": "no answer from the box to the push; asking for its result",
			// The box never saw it: every poll is 404, and the push is
			// sent again under its id.
			"never": "no answer from the box to the push; asking for its result",
			// Sent again, and that upload breaks too: tried once more.
			"never,unread": "no answer from the box to the push; asking for its result",
			// A 504 without the push's id is not the box's: sent again.
			"proxy504": "no answer from the box to the push; asking for its result",
		} {
			t.Run(how, func(t *testing.T) {
				t.Parallel()
				g := newApplyRig(t)
				head := g.change("one")
				g.box.set(func(s *standIn) { s.onPost, s.edited = strings.Split(how, ","), true })
				out, code := g.apply()
				if code != 0 || g.box.seen().baseline != head || !strings.Contains(out, said) || !strings.Contains(out, ": loaded; running apps are not restarted") {
					t.Fatalf("exit %d\n%s", code, out)
				}
				if !strings.Contains(out, "the Caddyfile on the box had been edited there since the last apply; this push replaced the edit") {
					t.Errorf("no warning for an edit on the box:\n%s", out)
				}
			})
		}
	})

	t.Run("a poll answered by something in front of the box", func(t *testing.T) {
		t.Parallel()
		g := newApplyRig(t)
		head := g.change("one")
		g.box.set(func(s *standIn) { s.pollGaps = []int{522, 530} })
		out, code := g.apply()
		if code != 0 || g.box.seen().baseline != head || !strings.Contains(out, ": loaded; running apps are not restarted") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("a TLS failure in front of the box is red at once", func(t *testing.T) {
		t.Parallel()
		g := newApplyRig(t)
		g.change("one")
		g.box.set(func(s *standIn) { s.onPost = []string{"front525"} })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 525: <html>Error 525</html>") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("a refusal in something else's words", func(t *testing.T) {
		t.Parallel()
		g := newApplyRig(t)
		g.change("one")
		g.box.set(func(s *standIn) { s.onPost = []string{"html409"} })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 409: <html>conflict</html>") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("a 429 is red, like any status the box does not send", func(t *testing.T) {
		t.Parallel()
		g := newApplyRig(t)
		g.change("one")
		g.box.set(func(s *standIn) { s.onPost = []string{"front429"} })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 429: <html>Error 429</html>") {
			t.Fatalf("a POST answered 429: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onPost, s.pollGaps = []string{"504"}, []int{429} })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 429: <html>Error 429</html>") {
			t.Fatalf("a poll answered 429: exit %d\n%s", code, out)
		}
	})

	t.Run("refused before anything is sent", func(t *testing.T) {
		g := newApplyRig(t)
		g.box.set(func(s *standIn) { s.onGet = "nobaseline" })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "HTTP 409: this box has no baseline; motel init <sha>, as root") {
			t.Fatalf("no baseline: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onGet = "401" })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "deploy_trust local { public_key") || strings.Contains(out, "claim repository") || strings.Count(out, "sending it once more") != 1 {
			t.Fatalf("401 from a laptop (sent once more, then refused): exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.onGet = "" })
		// A shallow clone: refused before asking the box anything, never
		// read as a rewritten main.
		shallow := filepath.Join(t.TempDir(), "shallow")
		g.git("commit", "-q", "--allow-empty", "-m", "on top")
		g.git("clone", "-q", "--depth", "1", "file://"+g.dir, shallow)
		if err := os.MkdirAll(filepath.Join(shallow, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(shallow, "bin", "apply")
		if err := os.WriteFile(script, []byte(g.read("bin/apply")), 0o755); err != nil {
			t.Fatal(err)
		}
		inShallow := exec.Command(script)
		inShallow.Dir = shallow
		inShallow.Env = append(append([]string{}, g.env...), "MOTEL="+g.bin, "CURL_HOME="+g.curlHome, "MOTEL_TOKEN=laptop")
		if out, err := inShallow.CombinedOutput(); err == nil || !strings.Contains(string(out), "this clone is shallow") {
			t.Fatalf("a shallow clone: %v\n%s", err, out)
		}
		g.git("reset", "-q", "--hard", "HEAD~1")
		g.box.set(func(s *standIn) { s.baseline = strings.Repeat("ab", 20) })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "which this repository's history does not hold") {
			t.Fatalf("a baseline this history lacks: exit %d\n%s", code, out)
		}
		// Past the chain's cap: 501 commits since the one the box runs.
		g.box.set(func(s *standIn) { s.baseline = g.base })
		var fi strings.Builder
		for i := 1; i <= 501; i++ {
			fmt.Fprintf(&fi, "commit refs/heads/main\ncommitter t <t@example.com> %d +0000\ndata 1\nc\n", i)
			if i == 1 {
				fmt.Fprintf(&fi, "from %s\n", g.base)
			}
			fi.WriteString("\n")
		}
		cmd := exec.Command("git", "fast-import", "--quiet")
		cmd.Dir, cmd.Env, cmd.Stdin = g.dir, g.env, strings.NewReader(fi.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fast-import: %v\n%s", err, out)
		}
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "is longer than 500 commits; run motel box baseline") {
			t.Fatalf("past the cap: exit %d\n%s", code, out)
		}
		if g.box.seen().bundles != 0 {
			t.Errorf("%d bundles sent", g.box.seen().bundles)
		}
	})

	t.Run("in GitHub Actions", func(t *testing.T) {
		g := newApplyRig(t)
		head := g.change("one")
		summary := filepath.Join(t.TempDir(), "summary")
		actions := []string{
			"GITHUB_ACTIONS=true", "GITHUB_SHA=" + head, "GITHUB_STEP_SUMMARY=" + summary,
			"GITHUB_REPOSITORY=your-org/boxes", "GITHUB_REF=refs/heads/main",
			"ACTIONS_ID_TOKEN_REQUEST_URL=" + g.box.srv.URL + "/token?api-version=2.0", "ACTIONS_ID_TOKEN_REQUEST_TOKEN=req",
			"MOTEL_TOKEN=",
			// An audience with characters a query must encode.
			"MOTEL_AUDIENCE=a&b c#d",
		}
		g.box.set(func(s *standIn) { s.onPost = []string{"504"} })
		// A mint that fails is tried again.
		g.box.set(func(s *standIn) { s.mintFails = 1 })
		out, code := g.apply(actions...)
		if code != 0 || !strings.Contains(out, "::add-mask::minted-1\n") || g.box.seen().audience != "a&b c#d" || !strings.Contains(out, "::group::apply deploy.example.com\n") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		// A fresh token for each request: the GET, the POST, each poll.
		for _, tok := range g.box.seen().unused {
			t.Errorf("token %s minted and never sent", tok)
		}
		if g.box.seen().mints < 3 {
			t.Errorf("%d tokens for a GET, a POST and a poll", g.box.seen().mints)
		}
		// The summary says what the log says happened, names in backticks.
		if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "**deploy.example.com:** "+head[:7]+" applied\n- loaded; running apps are not restarted; apps `example`\n") {
			t.Errorf("the job summary:\n%s", b)
		}
		// A checkout that is not the run's commit: its token would name another.
		if out, code := g.apply(append(actions, "GITHUB_SHA="+g.base)...); code != 1 || !strings.Contains(out, "but this run is for "+g.base) {
			t.Fatalf("another commit: exit %d\n%s", code, out)
		}
		// A 401 to a minted token is sent once more, with a fresh token
		// (#306: the box refuses the first token under an issuer's new
		// key while it fetches it); a second 401 stands.
		mints := g.box.seen().mints
		g.box.set(func(s *standIn) { s.onGet = "401" })
		if out, code := g.apply(actions...); code != 1 || strings.Count(out, "the box refused the token; sending it once more in 3s (a box refuses the first token under a new signing key while it fetches the key)") != 1 || !strings.Contains(out, "claim repository  your-org/boxes") || !strings.Contains(out, "::error title=apply deploy.example.com failed::") {
			t.Fatalf("401 in Actions: exit %d\n%s", code, out)
		}
		if got := g.box.seen().mints - mints; got != 2 {
			t.Errorf("401 in Actions: %d tokens minted, want the GET's and its retry's", got)
		}
		// A 401 from something in front of the box is not about the
		// token, and is not sent again.
		g.box.set(func(s *standIn) { s.onGet = "front401" })
		if out, code := g.apply(actions...); code != 1 || strings.Contains(out, "sending it once more") {
			t.Fatalf("a proxy's 401 in Actions: exit %d\n%s", code, out)
		}
		head = g.change("two")
		g.box.set(func(s *standIn) { s.onGet = "401once" })
		if out, code := g.apply(append(actions, "GITHUB_SHA="+head)...); code != 0 || !strings.Contains(out, "the box refused the token; sending it once more in 3s (a box refuses the first token under a new signing key while it fetches the key)") {
			t.Fatalf("one 401 in Actions, then the box's answer: exit %d\n%s", code, out)
		}
	})

	t.Run("secrets ride along", func(t *testing.T) {
		g := newApplyRig(t)
		g.env = append(g.env, "PATH="+ageTools(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
		_, box := g.boxKey("box")
		g.box.set(func(s *standIn) { s.recipient = box })
		db := "deploy.example.com/secrets/example/DATABASE_URL.age"
		// The first secret, and the app's block reading it: the box's
		// consistency checks refuse either without the other (the
		// stand-in does not repeat them).
		if out, code := g.secrets("", "postgres://one", "set", "APP=example", "VAR=DATABASE_URL", "BOX_KEY="+box); code != 0 {
			t.Fatalf("make secret: exit %d\n%s", code, out)
		}
		g.write(boxFile, strings.Replace(g.read(boxFile), "# env_file /etc/motel/secrets/example.env", "env_file /etc/motel/secrets/example.env", 1))
		head := g.commit("example: DATABASE_URL")
		summary := filepath.Join(t.TempDir(), "summary")
		out, code := g.apply("GITHUB_STEP_SUMMARY=" + summary)
		for _, want := range []string{
			"apply: deploy.example.com secrets: example: added DATABASE_URL\n",
			"deploy.example.com at " + head[:7] + ": loaded; running apps are not restarted; apps example\n",
			"apply: example reads its secrets at its next launch; a deploy of a new version launches it now\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("no %q in the output", want)
			}
		}
		if code != 0 || g.box.seen().baseline != head {
			t.Fatalf("exit %d, baseline %s\n%s", code, g.box.seen().baseline, out)
		}
		// The job summary has the same lines, names in backticks (an _
		// would turn Markdown italic), never a value.
		if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "**deploy.example.com:** "+head[:7]+" applied\n"+
			"- secrets: `example`: added `DATABASE_URL`\n"+
			"- loaded; running apps are not restarted; apps `example`\n"+
			"- `example` reads its secrets at its next launch; a deploy of a new version launches it now\n") ||
			strings.Contains(string(b), "postgres://") {
			t.Errorf("the job summary:\n%s", b)
		}

		// Secrets alone: a value changed and one added in one push, the
		// added one a copy of the other's file, so one blob, which the
		// bundle holds once (the stand-in's ReadBundle refuses a name
		// twice).
		if out, code := g.secrets("", "postgres://two", "set", "APP=example", "VAR=DATABASE_URL"); code != 0 {
			t.Fatalf("make secret again: exit %d\n%s", code, out)
		}
		g.write("deploy.example.com/secrets/example/COPY.age", g.read(db))
		head = g.commit("two")
		summary = filepath.Join(t.TempDir(), "summary")
		out, code = g.apply("GITHUB_STEP_SUMMARY=" + summary)
		if code != 0 || !strings.Contains(out, "apply: deploy.example.com secrets: example: added COPY; changed DATABASE_URL\n") ||
			!strings.Contains(out, "deploy.example.com at "+head[:7]+": secrets updated; the Caddyfile is unchanged, nothing reloaded; apps example\n") {
			t.Fatalf("secrets alone: exit %d\n%s", code, out)
		}
		if b, _ := os.ReadFile(summary); !strings.Contains(string(b), "- secrets: `example`: added `COPY`; changed `DATABASE_URL`\n"+
			"- secrets updated; the Caddyfile is unchanged, nothing reloaded; apps `example`\n") {
			t.Errorf("the job summary, secrets alone:\n%s", b)
		}
		// The box's own secrets alone: notify reads them at a load, so
		// the push reloads, and no app is told to relaunch for them.
		if out, code := g.secrets("", "https://ntfy.sh/ops", "set", "APP=_box", "VAR=NOTIFY_OPS_URL"); code != 0 {
			t.Fatalf("make secret APP=_box: exit %d\n%s", code, out)
		}
		head = g.commit("the operator's channel")
		out, code = g.apply()
		if code != 0 || !strings.Contains(out, "apply: deploy.example.com secrets: _box: added NOTIFY_OPS_URL\n") ||
			!strings.Contains(out, "deploy.example.com at "+head[:7]+": the box's notify secrets updated and loaded; the Caddyfile is unchanged, running apps are not restarted") ||
			strings.Contains(out, "_box reads its secrets") || strings.Contains(out, "ntfy.sh") {
			t.Fatalf("the box's own secrets: exit %d\n%s", code, out)
		}
		// The same values over other bytes on the box (an operator's
		// hand-written _box.env from before the slot): no name changes,
		// the box rewrites the file and reloads, and the line says what
		// the result does.
		g.box.set(func(s *standIn) { s.envEdited = []string{"_box"} })
		head = g.commit("the box's file rewritten")
		out, code = g.apply()
		if code != 0 || !strings.Contains(out, "deploy.example.com at "+head[:7]+": the box's notify secrets updated and loaded; the Caddyfile is unchanged, running apps are not restarted") {
			t.Fatalf("the box's own file rewritten: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.envEdited = nil })
		g.remove("deploy.example.com/secrets/_box")
		g.change("no notify secrets")
		if out, code := g.apply(); code != 0 {
			t.Fatalf("the box's own secrets removed: exit %d\n%s", code, out)
		}

		// A recipients file written on Windows reads as age reads it.
		g.write(recipientsFile, strings.ReplaceAll(g.read(recipientsFile), "\n", "\r\n"))
		g.change("crlf")
		if out, code := g.apply(); code != 0 {
			t.Fatalf("a recipients file with CRLF endings: exit %d\n%s", code, out)
		}

		// One of two removed, and the Caddyfile changed in the same push,
		// over an env file edited on the box: the file is rewritten with
		// the value left, not removed.
		g.box.set(func(s *standIn) { s.envEdited = []string{"example"} })
		g.remove("deploy.example.com/secrets/example/COPY.age")
		head = g.change("three")
		out, code = g.apply()
		if code != 0 || !strings.Contains(out, "apply: deploy.example.com secrets: example: removed COPY\n") ||
			!strings.Contains(out, "deploy.example.com at "+head[:7]+": loaded; running apps are not restarted") ||
			!strings.Contains(out, "this push replaced the edit with deploy.example.com/secrets/example/ at "+head[:7]) {
			t.Fatalf("a removal: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.envEdited = nil })

		// An env file edited on the box: said, and on the job page.
		g.box.set(func(s *standIn) { s.envEdited = []string{"example"} })
		head = g.change("four")
		out, code = g.apply("GITHUB_ACTIONS=true")
		if code != 0 || !strings.Contains(out, "example's env file on the box had been edited there since the last apply; this push replaced the edit with deploy.example.com/secrets/example/ at "+head[:7]) ||
			!strings.Contains(out, "::warning title=apply deploy.example.com::example's env file") {
			t.Fatalf("an env file edited on the box: exit %d\n%s", code, out)
		}
		g.box.set(func(s *standIn) { s.envEdited = nil })

		// A rebuilt box has another key: nothing is sent.
		_, rebuilt := g.boxKey("rebuilt")
		g.box.set(func(s *standIn) { s.recipient = rebuilt })
		g.change("five")
		sent := g.box.seen().bundles
		out, code = g.apply()
		if code != 1 || !strings.Contains(out, "the box's key is "+rebuilt+", but the first recipient in deploy.example.com/secrets/recipients is "+box) ||
			!strings.Contains(out, "make rekey BOX_KEY="+rebuilt) || g.box.seen().bundles != sent {
			t.Fatalf("another key on the box: exit %d, %d bundles\n%s", code, g.box.seen().bundles-sent, out)
		}
		// A box that names no key.
		g.box.set(func(s *standIn) { s.recipient = "" })
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "the box names no key for secrets (no recipient in GET /): run motel box key as root on it; nothing was sent") || g.box.seen().bundles != sent {
			t.Fatalf("no key on the box: exit %d\n%s", code, out)
		}
		// Values, and no recipients file to check the key against.
		g.box.set(func(s *standIn) { s.recipient = box })
		g.remove("deploy.example.com/secrets/recipients")
		g.commit("no recipients")
		if out, code := g.apply(); code != 1 || !strings.Contains(out, "deploy.example.com/secrets holds values but no recipients file names a key") || g.box.seen().bundles != sent {
			t.Fatalf("no recipients file: exit %d\n%s", code, out)
		}
		// Every secret removed, over an env file edited on the box: the
		// push removed it, and says so.
		g.remove("deploy.example.com/secrets")
		g.write(boxFile, strings.Replace(g.read(boxFile), "\tenv_file /etc/motel/secrets/example.env", "\t# env_file /etc/motel/secrets/example.env", 1))
		g.box.set(func(s *standIn) { s.envEdited = []string{"example"} })
		head = g.commit("no secrets")
		if out, code := g.apply(); code != 0 || !strings.Contains(out, "apply: deploy.example.com secrets: example: removed DATABASE_URL\n") ||
			!strings.Contains(out, "example's env file on the box had been edited there since the last apply; this push removed it, as "+head[:7]+" has no secrets for example") {
			t.Fatalf("every secret removed: exit %d\n%s", code, out)
		}
	})

	t.Run("two boxes, one refused", func(t *testing.T) {
		g := newApplyRig(t)
		g.write("deploy2.example.com/Caddyfile", strings.Replace(g.read(boxFile), "deploy.example.com {", "deploy2.example.com {", 1))
		head := g.change("both")
		second := newStandIn(t, head, nil)
		second.set(func(s *standIn) { s.onGet = "nobaseline" })
		g.route(map[string]*standIn{"deploy.example.com": g.box, "deploy2.example.com": second})
		out, code := g.apply()
		if code != 1 || g.box.seen().baseline != head || !strings.Contains(out, "https://deploy2.example.com/: HTTP 409") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("names git would quote by default", func(t *testing.T) {
		// A box directory and a value read as they are (core.quotePath),
		// not skipped: the second box is asked, and the value is counted,
		// so the first box's key is checked before anything is sent.
		g := newApplyRig(t)
		g.write("café/Caddyfile", strings.Replace(g.read(boxFile), "deploy.example.com {", "deploy2.example.com {", 1))
		g.write("deploy.example.com/secrets/example/É.age", "x")
		head := g.commit("both")
		second := newStandIn(t, head, nil)
		second.set(func(s *standIn) { s.onGet = "nobaseline" })
		g.route(map[string]*standIn{"deploy.example.com": g.box, "deploy2.example.com": second})
		out, code := g.apply()
		if code != 1 || !strings.Contains(out, "https://deploy2.example.com/: HTTP 409") ||
			!strings.Contains(out, "deploy.example.com/secrets holds values but no recipients file names a key") || g.box.seen().bundles != 0 {
			t.Fatalf("exit %d, %d bundles\n%s", code, g.box.seen().bundles, out)
		}
	})

	t.Run("a box's file is a regular file", func(t *testing.T) {
		g := newApplyRig(t)
		// An executable file is a regular one; a second box sharing the
		// first's file through a symlink is named, and nothing is sent
		// to it.
		if err := os.Chmod(filepath.Join(g.dir, boxFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(g.dir, "deploy2.example.com"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../"+boxFile, filepath.Join(g.dir, "deploy2.example.com", "Caddyfile")); err != nil {
			t.Fatal(err)
		}
		head := g.change("both")
		second := newStandIn(t, head, nil)
		g.route(map[string]*standIn{"deploy.example.com": g.box, "deploy2.example.com": second})
		const said = "deploy2.example.com/Caddyfile is a symlink: a box's file is a regular file (the box refuses one; make check says the same)"
		out, code := g.apply()
		if code != 1 || !strings.Contains(out, said) || g.box.seen().baseline != head || second.seen().bundles != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		// By directory, as an operator might type it.
		if out, code := g.runArgs([]string{"deploy2.example.com"}); code != 1 || !strings.Contains(out, said) || second.seen().bundles != 0 {
			t.Fatalf("by directory: exit %d\n%s", code, out)
		}
	})
}

// recorder is a motel that writes a tag to $MOTEL_RAN each time
// it runs, then runs this tree's: what a stub release looks like.
func recorder(t *testing.T, path, tag, bin string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' '" + tag + "' >>\"$MOTEL_RAN\"\nexec \"" + bin + "\" \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestBoxCheckVersionFile: a box with a MOTEL_VERSION file is checked
// with $MOTEL-<version>; one without, with $MOTEL; a file that
// names no release is refused and named, never read as the latest.
func TestBoxCheckVersionFile(t *testing.T) {
	t.Parallel()
	bin := motelBinary(t)
	r := newBoxRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	// $MOTEL and its releases, each a recorder, in a directory of
	// this test's own.
	bins := t.TempDir()
	hs := filepath.Join(bins, "motel")
	recorder(t, hs, "default", bin)
	recorder(t, hs+"-0.3.1", "0.3.1", bin)
	recorder(t, hs+"-0.3.1-rc1", "0.3.1-rc1", bin)
	ran := filepath.Join(t.TempDir(), "ran")
	check := func() (string, int) {
		_ = os.Remove(ran)
		return r.run("check", "MOTEL="+hs, "MOTEL_RAN="+ran)
	}
	runs := func() string {
		b, _ := os.ReadFile(ran)
		return string(b)
	}
	const vfile = "deploy.example.com/MOTEL_VERSION"
	good := r.read(boxFile)
	second := strings.Replace(good, "deploy.example.com {", "deploy2.example.com {", 1)

	// No file: the default, twice a box (describe, validate).
	if out, code := check(); code != 0 || out != "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)\n" || runs() != "default\ndefault\n" {
		t.Fatalf("no file: exit %d\n%s\nran:\n%s", code, out, runs())
	}
	// A file: that release's binary, and the line says so. A second box
	// without one keeps the default; the v comes off.
	r.write("deploy2.example.com/Caddyfile", second)
	for file, tag := range map[string]string{"0.3.1\n": "0.3.1", "v0.3.1-rc1\n": "0.3.1-rc1", "0.3.1": "0.3.1"} {
		r.write(vfile, file)
		want := "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer), checked with " + hs + "-" + tag + "\n" +
			"deploy2.example.com/Caddyfile → https://deploy2.example.com/ (1 app, 1 signer)\n"
		if out, code := check(); code != 0 || out != want || runs() != tag+"\n"+tag+"\ndefault\ndefault\n" {
			t.Fatalf("file %q: exit %d\n%s\nran:\n%s", file, code, out, runs())
		}
	}
	r.remove("deploy2.example.com")

	// A release that is not here: refused, naming the binary and what
	// installs it; nothing ran.
	r.write(vfile, "0.3.0\n")
	if out, code := check(); code != 1 || !strings.Contains(out, "check: deploy.example.com/Caddyfile: "+vfile+" names 0.3.0, and "+hs+"-0.3.0 is not here to check it with: bin/install-motel installs one") || runs() != "" {
		t.Fatalf("a release not installed: exit %d\n%s\nran:\n%s", code, out, runs())
	}
	// Not a release: refused and named, the file empty included; the
	// default is not what an empty file means.
	for _, file := range []string{"", "\n", "v", "v\n", "0.3", "0.3.1\n0.3.2\n", "0.3.1 \n", "latest\n", "0.3.1-rc1 # the candidate\n", "\xef\xbb\xbf0.3.1\n"} {
		r.write(vfile, file)
		out, code := check()
		if code != 1 || !strings.Contains(out, "check: deploy.example.com/Caddyfile: "+vfile+" does not name a motel release: '") || !strings.Contains(out, "is not a release's version, 0.3.1 or v0.3.1-rc1, on one line") || runs() != "" {
			t.Fatalf("file %q: exit %d\n%s\nran:\n%s", file, code, out, runs())
		}
		if empty := strings.TrimSpace(file) == ""; empty != strings.Contains(out, "an empty file pins nothing: name the release this box runs, or delete the file") {
			t.Fatalf("file %q: the empty-file hint is for an empty file alone\n%s", file, out)
		}
	}
	// A directory at the name, or a link to nothing: refused, not read
	// as empty, and not as no file.
	r.remove(vfile)
	if err := os.Mkdir(filepath.Join(r.dir, vfile), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := check(); code != 1 || !strings.Contains(out, vfile+" cannot be read: a box's MOTEL_VERSION is a regular file of one line") {
		t.Fatalf("a directory: exit %d\n%s", code, out)
	}
	r.remove(vfile)
	if err := os.Symlink("../versions/gone", filepath.Join(r.dir, vfile)); err != nil {
		t.Fatal(err)
	}
	if out, code := check(); code != 1 || !strings.Contains(out, vfile+" cannot be read: a box's MOTEL_VERSION is a regular file of one line") || runs() != "" {
		t.Fatalf("a dangling link: exit %d\n%s", code, out)
	}
	r.remove(vfile)
	// A pipe at the name: refused, not waited on (the test would hang).
	if err := syscall.Mkfifo(filepath.Join(r.dir, vfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := check(); code != 1 || !strings.Contains(out, vfile+" cannot be read: a box's MOTEL_VERSION is a regular file of one line") || runs() != "" {
		t.Fatalf("a pipe: exit %d\n%s", code, out)
	}
	r.remove(vfile)

}

// releases is a stand-in for github.com's release pages: the latest's
// redirect, and each release's tarball and checksums.txt. The tarball's
// motel is a recorder tagged with its version.
type releases struct {
	t      *testing.T
	srv    *httptest.Server
	latest string
	arch   string
	bin    string

	known []string // the releases that exist; any other address is a 404

	mu        sync.Mutex
	downloads map[string]int // tarballs served, by version
	corrupt   string         // this version's checksums.txt names another digest
}

// newReleases serves as github.com, with a certificate for that name
// (ca, in PEM, is what curl is told to trust).
func newReleases(t *testing.T, latest, bin string) (g *releases, ca []byte) {
	t.Helper()
	g = &releases{t: t, latest: latest, arch: runtime.GOARCH, bin: bin, downloads: map[string]int{},
		known: []string{"0.3.0", "0.3.1", "0.3.1-rc1"}}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "github.com"},
		DNSNames:     []string{"github.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	g.srv = httptest.NewUnstartedServer(g)
	g.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	g.srv.StartTLS()
	t.Cleanup(g.srv.Close)
	return g, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

var releasePath = regexp.MustCompile(`^/smallhoursorg/motel/releases/download/v([0-9A-Za-z.-]+)/(.+)$`)

func (g *releases) tarball(v string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	script := "#!/bin/sh\nprintf '%s\\n' '" + v + "' >>\"$MOTEL_RAN\"\nexec \"" + g.bin + "\" \"$@\"\n"
	for _, f := range []struct{ name, body string }{{"LICENSE", "a licence\n"}, {"motel", script}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body))}); err != nil {
			g.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			g.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		g.t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		g.t.Fatal(err)
	}
	return buf.Bytes()
}

func (g *releases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/smallhoursorg/motel/releases/latest" {
		http.Redirect(w, r, "https://github.com/smallhoursorg/motel/releases/tag/v"+g.latest, http.StatusFound)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/smallhoursorg/motel/releases/tag/") {
		_, _ = io.WriteString(w, "a release page\n")
		return
	}
	m := releasePath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	v, name := m[1], m[2]
	if !slices.Contains(g.known, v) {
		http.NotFound(w, r)
		return
	}
	tgz := "motel_" + v + "_linux_" + g.arch + ".tar.gz"
	switch name {
	case tgz:
		g.mu.Lock()
		g.downloads[v]++
		g.mu.Unlock()
		_, _ = w.Write(g.tarball(v))
	case "checksums.txt":
		g.mu.Lock()
		corrupt := g.corrupt == v
		g.mu.Unlock()
		sum := digest(g.tarball(v))
		if corrupt {
			sum = digest([]byte("other bytes"))
		}
		other := "motel_" + v + "_linux_" + map[string]string{"amd64": "arm64", "arm64": "amd64"}[g.arch] + ".tar.gz"
		_, _ = fmt.Fprintf(w, "%s  %s\n%s  %s\n%s  motel_%s_%s.deb\n", sum, tgz, digest([]byte(other)), other, digest([]byte("deb")), v, g.arch)
	default:
		http.NotFound(w, r)
	}
}

func (g *releases) served() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.downloads)
}

func (g *releases) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.downloads = map[string]int{}
}

// TestBoxInstallMotel: bin/install-motel against a stand-in
// github.com, with sudo a script that puts /usr/local/bin under a
// directory of the test's. Every release the boxes name is installed
// once, verified, as motel-<version>; the default as motel too;
// and bin/check then picks each box's.
func TestBoxInstallMotel(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("bin/install-motel installs Linux binaries, and refuses elsewhere")
	}
	for _, tool := range []string{"sha256sum", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("bin/install-motel needs %s on PATH", tool)
		}
	}
	bin := motelBinary(t)
	r := newBoxRepo(t)
	r.key("id_ed25519")
	if out, code := r.run("signer"); code != 0 {
		t.Fatalf("signer: exit %d\n%s", code, out)
	}
	gh, caPEM := newReleases(t, "0.3.1", bin)

	// curl reaches the stand-in at github.com:443 by its own config file.
	curlHome := t.TempDir()
	ca := filepath.Join(curlHome, "github.pem")
	if err := os.WriteFile(ca, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	rc := "connect-to = github.com:443:" + gh.srv.Listener.Addr().String() + "\ncacert = " + ca + "\n"
	if err := os.WriteFile(filepath.Join(curlHome, ".curlrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	// sudo: the command as given, /usr/local/bin under $root.
	root := t.TempDir()
	usrLocalBin := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(usrLocalBin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := t.TempDir()
	sudo := "#!/bin/sh\nn=$#\nfor a; do\n\tcase $a in /usr/local/bin/*) a=" + root + "$a ;; esac\n\tset -- \"$@\" \"$a\"\ndone\nshift \"$n\"\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(stubs, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(t.TempDir(), "ran")
	env := []string{"CURL_HOME=" + curlHome, "PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"), "MOTEL_RAN=" + ran}
	installArgs := func(args []string, extra ...string) (string, int) {
		t.Helper()
		gh.reset()
		_ = os.RemoveAll(usrLocalBin)
		if err := os.MkdirAll(usrLocalBin, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(filepath.Join(r.dir, "bin", "install-motel"), args...)
		cmd.Dir, cmd.Env = r.dir, append(append(append([]string{}, r.env...), env...), extra...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("bin/install-motel: %v\n%s", err, out)
		}
		return string(out), 0
	}
	install := func(extra ...string) (string, int) {
		t.Helper()
		return installArgs(nil, extra...)
	}
	installed := func() []string {
		t.Helper()
		entries, err := os.ReadDir(usrLocalBin)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("%s: mode %o, not 0755", e.Name(), info.Mode().Perm())
			}
			names = append(names, e.Name())
		}
		return names
	}
	// tagOf runs an installed motel and returns the version its
	// recorder wrote: which release's bytes it is.
	tagOf := func(name string) string {
		t.Helper()
		_ = os.Remove(ran)
		cmd := exec.Command(filepath.Join(usrLocalBin, name), "box", "webhook", filepath.Join(r.dir, boxFile))
		cmd.Env = append(append([]string{}, r.env...), "MOTEL_RAN="+ran)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "https://deploy.example.com/" {
			t.Fatalf("%s box webhook: %v\n%s", name, err, out)
		}
		b, _ := os.ReadFile(ran)
		return strings.TrimSpace(string(b))
	}
	arch := runtime.GOARCH
	// ok is sha256sum's line for a verified tarball, before the install's.
	ok := func(v string) string { return "motel_" + v + "_linux_" + arch + ".tar.gz: OK\n" }
	stable := func(v string) string {
		return ok(v) + "install-motel: motel " + v + " (" + arch + "), and motel-" + v + "\n"
	}
	pinned := func(v string) string {
		return ok(v) + "install-motel: motel-" + v + " (" + arch + "), for the boxes whose MOTEL_VERSION names it\n"
	}

	// No file and no variable: the latest, once, as motel and
	// motel-<latest>.
	out, code := install()
	if code != 0 || out != stable("0.3.1") {
		t.Fatalf("the latest: exit %d\n%s", code, out)
	}
	if got := installed(); !slices.Equal(got, []string{"motel", "motel-0.3.1"}) {
		t.Fatalf("installed %v", got)
	}
	if !maps.Equal(gh.served(), map[string]int{"0.3.1": 1}) || tagOf("motel") != "0.3.1" || tagOf("motel-0.3.1") != "0.3.1" {
		t.Fatalf("downloads %v; motel is %s", gh.served(), tagOf("motel"))
	}

	// Four boxes, three releases between them: each release once,
	// whatever the number of boxes naming it, the v off, with or without
	// the line's newline.
	good := r.read(boxFile)
	box := func(host, version string) {
		r.write(host+"/Caddyfile", strings.Replace(good, "deploy.example.com {", host+" {", 1))
		if version != "" {
			r.write(host+"/MOTEL_VERSION", version)
		}
	}
	box("deploy2.example.com", "0.3.0\n")
	box("deploy3.example.com", "v0.3.1-rc1\n")
	box("deploy4.example.com", "0.3.0")
	out, code = install()
	want := stable("0.3.1") + pinned("0.3.0") + pinned("0.3.1-rc1")
	if code != 0 || out != want {
		t.Fatalf("three releases: exit %d\n%s", code, out)
	}
	if got := installed(); !slices.Equal(got, []string{"motel", "motel-0.3.0", "motel-0.3.1", "motel-0.3.1-rc1"}) {
		t.Fatalf("installed %v", got)
	}
	if !maps.Equal(gh.served(), map[string]int{"0.3.1": 1, "0.3.0": 1, "0.3.1-rc1": 1}) {
		t.Fatalf("downloads %v", gh.served())
	}
	for name, tag := range map[string]string{"motel": "0.3.1", "motel-0.3.0": "0.3.0", "motel-0.3.1": "0.3.1", "motel-0.3.1-rc1": "0.3.1-rc1"} {
		if got := tagOf(name); got != tag {
			t.Fatalf("%s is release %s, not %s", name, got, tag)
		}
	}
	// And bin/check picks each box's.
	_ = os.Remove(ran)
	hs := filepath.Join(usrLocalBin, "motel")
	out, code = r.run("check", "MOTEL="+hs, "MOTEL_RAN="+ran)
	want = "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)\n" +
		"deploy2.example.com/Caddyfile → https://deploy2.example.com/ (1 app, 1 signer), checked with " + hs + "-0.3.0\n" +
		"deploy3.example.com/Caddyfile → https://deploy3.example.com/ (1 app, 1 signer), checked with " + hs + "-0.3.1-rc1\n" +
		"deploy4.example.com/Caddyfile → https://deploy4.example.com/ (1 app, 1 signer), checked with " + hs + "-0.3.0\n"
	b, _ := os.ReadFile(ran)
	if code != 0 || out != want || string(b) != "0.3.1\n0.3.1\n0.3.0\n0.3.0\n0.3.1-rc1\n0.3.1-rc1\n0.3.0\n0.3.0\n" {
		t.Fatalf("check with the installed releases: exit %d\n%s\nran:\n%s", code, out, b)
	}

	// The variable: the default, and a box's file naming the same
	// release is one download.
	out, code = install("MOTEL_VERSION=v0.3.0")
	if code != 0 || out != stable("0.3.0")+pinned("0.3.1-rc1") {
		t.Fatalf("the variable: exit %d\n%s", code, out)
	}
	if !maps.Equal(gh.served(), map[string]int{"0.3.0": 1, "0.3.1-rc1": 1}) || tagOf("motel") != "0.3.0" {
		t.Fatalf("downloads %v; motel is %s", gh.served(), tagOf("motel"))
	}

	// A box's file that names no release is said, naming the file, and
	// skipped: the other boxes' releases are still installed, and
	// make check is what refuses the box. The variable's shape is the
	// same, and its refusal is the run's: nothing to check with.
	three := map[string]int{"0.3.1": 1, "0.3.0": 1, "0.3.1-rc1": 1}
	four := []string{"motel", "motel-0.3.0", "motel-0.3.1", "motel-0.3.1-rc1"}
	for _, c := range []struct{ file, said string }{
		{"", "'' (from deploy2.example.com/MOTEL_VERSION): a release's version, 0.3.1 or v0.3.1-rc1, on one line; an empty file pins nothing: name a release, or delete it\n"},
		{"\n", "'' (from deploy2.example.com/MOTEL_VERSION): a release's version, 0.3.1 or v0.3.1-rc1, on one line; an empty file pins nothing"},
		{"v\n", "'v' (from deploy2.example.com/MOTEL_VERSION): a release's version, 0.3.1 or v0.3.1-rc1, on one line\n"},
		{"0.3\n", "'0.3' (from deploy2.example.com/MOTEL_VERSION)"},
		{"0.3.0\n0.3.1\n", "'0.3.0\n0.3.1' (from deploy2.example.com/MOTEL_VERSION)"},
		{"latest\n", "'latest' (from deploy2.example.com/MOTEL_VERSION)"},
	} {
		r.write("deploy2.example.com/MOTEL_VERSION", c.file)
		out, code := install()
		if code != 0 || !strings.Contains(out, "install-motel: not a motel release: "+c.said) ||
			!strings.Contains(out, "install-motel: deploy2.example.com/MOTEL_VERSION skipped; make check refuses this box\n") ||
			!maps.Equal(gh.served(), three) || !slices.Equal(installed(), four) {
			t.Fatalf("file %q: exit %d\n%s\ndownloads %v, installed %v", c.file, code, out, gh.served(), installed())
		}
	}
	r.remove("deploy2.example.com/MOTEL_VERSION")
	if err := os.Mkdir(filepath.Join(r.dir, "deploy2.example.com/MOTEL_VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = install()
	if code != 0 || !strings.Contains(out, "install-motel: deploy2.example.com/MOTEL_VERSION cannot be read: a box's MOTEL_VERSION is a regular file of one line, 0.3.1 or v0.3.1-rc1; skipped, and make check refuses this box\n") ||
		strings.Contains(out, "not a motel release") || !maps.Equal(gh.served(), three) {
		t.Fatalf("a directory: exit %d\n%s\ndownloads %v", code, out, gh.served())
	}
	r.remove("deploy2.example.com/MOTEL_VERSION")
	// A pipe at the name: said, skipped, not waited on.
	if err := syscall.Mkfifo(filepath.Join(r.dir, "deploy2.example.com/MOTEL_VERSION"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = install()
	if code != 0 || !strings.Contains(out, "install-motel: deploy2.example.com/MOTEL_VERSION cannot be read: a box's MOTEL_VERSION is a regular file of one line, 0.3.1 or v0.3.1-rc1; skipped, and make check refuses this box\n") || !maps.Equal(gh.served(), three) {
		t.Fatalf("a pipe: exit %d\n%s\ndownloads %v", code, out, gh.served())
	}
	r.remove("deploy2.example.com/MOTEL_VERSION")
	for _, v := range []string{"v", "0.3", "latest"} {
		out, code := install("MOTEL_VERSION=" + v)
		if code != 1 || !strings.Contains(out, "install-motel: not a motel release: '"+v+"' (from MOTEL_VERSION): a release's version, 0.3.1 or v0.3.1-rc1, on one line\n") || len(gh.served()) != 0 {
			t.Fatalf("MOTEL_VERSION=%s: exit %d\n%s", v, code, out)
		}
	}

	// A release that does not exist, or has no Linux build: a box's is
	// said, naming the files that asked for it, and skipped; check then
	// refuses that box and the others still pass. The default's is the
	// run's.
	tgz := func(v string) string {
		return "https://github.com/smallhoursorg/motel/releases/download/v" + v + "/motel_" + v + "_linux_" + arch + ".tar.gz"
	}
	r.write("deploy2.example.com/MOTEL_VERSION", "0.3.9\n")
	r.write("deploy4.example.com/MOTEL_VERSION", "v0.3.9\n")
	out, code = install()
	if code != 0 || !strings.Contains(out, "install-motel: no release at "+tgz("0.3.9")+", which deploy2.example.com/MOTEL_VERSION, deploy4.example.com/MOTEL_VERSION names; skipped, and make check refuses that box\n") ||
		!maps.Equal(gh.served(), map[string]int{"0.3.1": 1, "0.3.1-rc1": 1}) || !slices.Equal(installed(), []string{"motel", "motel-0.3.1", "motel-0.3.1-rc1"}) {
		t.Fatalf("a release with no build: exit %d\n%s\ndownloads %v, installed %v", code, out, gh.served(), installed())
	}
	out, code = r.run("check", "MOTEL="+hs, "MOTEL_RAN="+ran)
	if code != 1 || !strings.Contains(out, "check: deploy2.example.com/Caddyfile: deploy2.example.com/MOTEL_VERSION names 0.3.9, and "+hs+"-0.3.9 is not here to check it with: bin/install-motel installs one for each box's file (in the workflow; there is no macOS build), and says when a release has no build to install\n") ||
		!strings.Contains(out, "check: deploy4.example.com/Caddyfile: deploy4.example.com/MOTEL_VERSION names 0.3.9, and ") ||
		!strings.Contains(out, "deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer)\n") ||
		!strings.Contains(out, "deploy3.example.com/Caddyfile → https://deploy3.example.com/ (1 app, 1 signer), checked with "+hs+"-0.3.1-rc1\n") {
		t.Fatalf("check after a skipped release: exit %d\n%s", code, out)
	}
	if out, code := install("MOTEL_VERSION=0.3.9"); code != 1 || !strings.Contains(out, "install-motel: no release at "+tgz("0.3.9")+" (from MOTEL_VERSION)\n") || len(installed()) != 0 {
		t.Fatalf("a default with no build: exit %d\n%s\ninstalled %v", code, out, installed())
	}
	r.write("deploy4.example.com/MOTEL_VERSION", "0.3.0")
	r.write("deploy2.example.com/MOTEL_VERSION", "latest\n")
	// `default`: the default alone, the boxes' files unread — the
	// apply job's, which one box's bad file must not stop.
	out, code = installArgs([]string{"default"})
	if code != 0 || out != stable("0.3.1") || !maps.Equal(gh.served(), map[string]int{"0.3.1": 1}) || !slices.Equal(installed(), []string{"motel", "motel-0.3.1"}) {
		t.Fatalf("default: exit %d\n%s\ndownloads %v", code, out, gh.served())
	}
	if out, code := installArgs([]string{"all"}); code != 1 || !strings.Contains(out, "install-motel: 'all'?") {
		t.Fatalf("an unknown argument: exit %d\n%s", code, out)
	}
	r.write("deploy2.example.com/MOTEL_VERSION", "0.3.0\n")

	// A tarball that does not match its checksums.txt is not installed,
	// and the run fails there.
	gh.mu.Lock()
	gh.corrupt = "0.3.0"
	gh.mu.Unlock()
	out, code = install()
	if code == 0 || !strings.Contains(out, "FAILED") || slices.Contains(installed(), "motel-0.3.0") {
		t.Fatalf("a corrupt tarball: exit %d\n%s\ninstalled %v", code, out, installed())
	}
}
