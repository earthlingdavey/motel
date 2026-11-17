package main

import (
	"os/exec"
	"strings"
	"testing"
)

// motelctl links no Caddy package, for any system it is built for: the
// client's whole point is a small binary with nothing of the server's
// in it. go list -deps is what the linker is given; a test that fails
// here names the import chain to cut (go mod why -m).
func TestLinksNoCaddy(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the go command is not on PATH, and this test is the check that motelctl links no Caddy")
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
				t.Errorf("%s: motelctl links %s, a Caddy package", goos, pkg)
			}
			if strings.HasPrefix(pkg, "github.com/smallhoursorg/motel/liveswap/") && pkg != "github.com/smallhoursorg/motel/liveswap/wire" {
				t.Errorf("%s: motelctl links %s; of liveswap it takes the wire types alone", goos, pkg)
			}
			if mod != "" && mod != "std" {
				modules[mod] = true
			}
		}
		t.Logf("%s: modules linked besides the standard library: %d", goos, len(modules))
	}
}
