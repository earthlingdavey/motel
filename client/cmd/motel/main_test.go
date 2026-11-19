package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestNotATerminal: stdout into /dev/null, a character device, or a
// pipe is no terminal, so motel alone prints its help there and asks
// nothing (TestPtyIsATerminal holds the other side).
func TestNotATerminal(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	for name, f := range map[string]*os.File{os.DevNull: null, "a pipe": w} {
		if terminal(f) {
			t.Errorf("%s is taken for a terminal", name)
		}
	}
}

// The client links no Caddy package, for any system it is built for:
// its whole point is a small binary with nothing of the server's
// in it. go list -deps is what the linker is given; a test that fails
// here names the import chain to cut (go mod why -m).
func TestLinksNoCaddy(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH, and this test is the check that the client links no Caddy")
	}
	for _, goos := range []string{"linux", "darwin"} {
		cmd := exec.Command(gobin, "list", "-deps", "-f", "{{with .Module}}{{.Path}}{{end}} {{.ImportPath}}", ".")
		cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH=arm64", "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list for %s: %v", goos, err)
		}
		modules := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			mod, pkg, _ := strings.Cut(line, " ")
			if strings.HasPrefix(mod, "github.com/caddyserver/") {
				t.Errorf("%s: the client links %s, a Caddy package", goos, pkg)
			}
			if strings.HasPrefix(pkg, "github.com/smallhoursorg/motel/liveswap/") && pkg != "github.com/smallhoursorg/motel/liveswap/wire" {
				t.Errorf("%s: the client links %s; of liveswap it takes the wire types alone", goos, pkg)
			}
			if mod != "" && mod != "std" {
				modules[mod] = true
			}
		}
		t.Logf("%s: modules linked besides the standard library: %d", goos, len(modules))
	}
}

// linked is the module versions go list says a build of . links, with
// the workspace as env leaves it or off.
func linked(t *testing.T, gobin, goos string, env ...string) map[string]string {
	t.Helper()
	cmd := exec.Command(gobin, "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}", ".")
	// GOFLAGS is the test's own, not the environment's (a -mod there
	// would change what is linked), with -buildvcs=false: the versions
	// linked do not depend on the checkout, and a checkout git will not
	// read (owned by another user, as in CI's container) must not fail
	// the list.
	cmd.Env = append(cmd.Environ(), append([]string{"GOOS=" + goos, "GOARCH=arm64", "CGO_ENABLED=0", "GOFLAGS=-buildvcs=false"}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list for %s %v: %v\n%s", goos, env, err, out)
	}
	mods := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p, v, ok := strings.Cut(line, " "); ok && p != "" {
			mods[p] = v
		}
	}
	return mods
}

// TestWorkspaceAgrees: a build inside the repository's workspace links
// the very module versions client/go.mod records. In workspace mode
// Go takes, for each module, the highest version any workspace module
// requires, so without this the binary built here could hold versions
// the client's own go.mod (which Dependabot, govulncheck and a reader
// go by, and a GOWORK=off release builds from) does not record.
func TestWorkspaceAgrees(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH, and this test is the check that the workspace and go.mod agree")
	}
	for _, goos := range []string{"linux", "darwin"} {
		work, off := linked(t, gobin, goos), linked(t, gobin, goos, "GOWORK=off")
		for m, v := range work {
			if m == "github.com/smallhoursorg/motel/liveswap" {
				continue // the directory next door: a workspace module, or a replace
			}
			if off[m] != v {
				t.Errorf("%s: %s is %s in the workspace build and %q in client/go.mod's (GOWORK=off): raise it there (go get %s@%s with GOWORK=off)", goos, m, v, off[m], m, v)
			}
		}
		for m := range off {
			if _, ok := work[m]; !ok && m != "github.com/smallhoursorg/motel/liveswap" {
				t.Errorf("%s: %s is linked with GOWORK=off and not in the workspace build", goos, m)
			}
		}
	}
}
