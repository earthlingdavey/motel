package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/smallhoursorg/motel/client/internal/local"
)

// gitAt answers `git rev-parse --show-toplevel` with top, or as
// outside a repository when top is empty.
type gitAt string

func (g gitAt) Output(_ context.Context, c local.Cmd) ([]byte, error) {
	if c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "rev-parse" && g != "" {
		return []byte(string(g) + "\n"), nil
	}
	return nil, errors.New("exit status 128")
}

func boxRepo(t *testing.T, boxes ...string) string {
	t.Helper()
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if err := os.MkdirAll(filepath.Join(top, b, "apps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(top, b, "Caddyfile"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not boxes: a directory with no Caddyfile, and one not named as
	// a host.
	for _, d := range []string{"docs", "Not_A_Host"} {
		if err := os.MkdirAll(filepath.Join(top, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "Not_A_Host", "Caddyfile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return top
}

func TestResolveBox(t *testing.T) {
	one := boxRepo(t, "deploy.example.com")
	two := boxRepo(t, "a.example.com", "b.example.com")
	fromApp := &Box{Host: "app.example.com", Port: "8443"}
	all := map[string]string{"MOTEL_BOX": "env.example.com", "MOTEL_URL": "https://url.example.com/blog"}
	cases := []struct {
		name     string
		l        BoxLookup
		env      map[string]string
		want     string
		wantFail string
	}{
		{"an app URL first", BoxLookup{FromApp: fromApp, Flag: "flag.example.com", Dir: one, Run: gitAt(one)}, all, "app.example.com:8443", ""},
		{"then --box", BoxLookup{Flag: "Flag.Example.com", Dir: one, Run: gitAt(one)}, all, "flag.example.com", ""},
		{"then MOTEL_BOX", BoxLookup{Dir: one, Run: gitAt(one)}, all, "env.example.com", ""},
		{"then MOTEL_URL's host", BoxLookup{Dir: one, Run: gitAt(one)}, map[string]string{"MOTEL_URL": "https://url.example.com/blog"}, "url.example.com", ""},
		{"then the box's directory", BoxLookup{Dir: filepath.Join(two, "b.example.com", "apps"), Run: gitAt(two)}, nil, "b.example.com", ""},
		{"or the one box at the top", BoxLookup{Dir: one, Run: gitAt(one)}, nil, "deploy.example.com", ""},
		{"not two at the top", BoxLookup{Dir: two, Run: gitAt(two)}, nil, "", "this repository has 2 boxes (a.example.com, b.example.com): name one with --box, or run inside its directory"},
		{"not from elsewhere in the repository", BoxLookup{Dir: filepath.Join(one, "docs"), Run: gitAt(one)}, nil, "", "name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository"},
		{"nothing names one", BoxLookup{Dir: one, Run: gitAt("")}, nil, "", "name the box: --box deploy.example.com, MOTEL_BOX, or run inside the box repository"},
		{"--box takes a name, and :port alone", BoxLookup{Flag: "deploy.example.com/blog"}, nil, "", `--box "deploy.example.com/blog" is not a box's name: give its host name, deploy.example.com, and :port when it is not 443`},
		{"MOTEL_URL is https", BoxLookup{}, map[string]string{"MOTEL_URL": "http://url.example.com/blog"}, "", `MOTEL_URL "http://url.example.com/blog" is not an https:// URL, e.g. https://deploy.example.com/blog`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.l.Getenv = func(k string) string { return c.env[k] }
			if c.l.Run == nil {
				c.l.Run = gitAt("")
			}
			b, err := ResolveBox(context.Background(), c.l)
			if c.wantFail != "" {
				if f := failure(t, err); f.Exit != ExitUsage || f.Message != c.wantFail {
					t.Fatalf("exit %d %q, want 2 %q", f.Exit, f.Message, c.wantFail)
				}
				return
			}
			if err != nil || b.Addr() != c.want {
				t.Fatalf("box %q, %v, want %q", b.Addr(), err, c.want)
			}
		})
	}
}

// TestBoxWithPort: --box and MOTEL_BOX take host:port, as an app URL
// with a port names the box; 443 is no port; a port is digits alone,
// read as a number, 1 to 65535.
func TestBoxWithPort(t *testing.T) {
	for _, c := range []struct {
		in         string
		host, port string
		bad        bool
	}{
		{"deploy.example.com", "deploy.example.com", "", false},
		{"Deploy.Example.com:8443", "deploy.example.com", "8443", false},
		{"deploy.example.com:443", "deploy.example.com", "", false},
		{"deploy.example.com:08443", "deploy.example.com", "8443", false},
		{"deploy.example.com:0008443", "deploy.example.com", "8443", false},
		{"deploy.example.com:0443", "deploy.example.com", "", false},
		{"deploy.example.com:065536", "", "", true},
		{"deploy.example.com:99999999999999999999", "", "", true},
		{"deploy.example.com:0", "", "", true},
		{"deploy.example.com:65536", "", "", true},
		{"deploy.example.com:123456", "", "", true},
		{"deploy.example.com:+80", "", "", true},
		{"deploy.example.com:", "", "", true},
		{":8443", "", "", true},
		{"deploy.example.com:84:43", "", "", true},
	} {
		b, err := bareBox(c.in, "--box")
		if c.bad != (err != nil) || !c.bad && (b.Host != c.host || b.Port != c.port) {
			t.Errorf("%q: %+v, %v", c.in, b, err)
		}
	}
	// The token is for the host alone.
	b, _ := bareBox("deploy.example.com:8443", "--box")
	if b.Addr() != "deploy.example.com:8443" || b.Host != "deploy.example.com" {
		t.Errorf("addr %q host %q", b.Addr(), b.Host)
	}
}
