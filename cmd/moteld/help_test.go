package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelpNamesTheBinary: the usage lines Caddy's CLI prints name the
// binary, moteld, not caddy (the deploy of a8038b4 to hotserve-1 found
// `caddy monitor setup [--metrics]` in the help), and the root's help is
// moteld's description, not Caddy's `caddy run`. main runs in a child,
// since Caddy's commands are built once a process.
func TestHelpNamesTheBinary(t *testing.T) {
	if args := os.Getenv("MOTEL_HELP_ARGS"); args != "" {
		os.Args = append([]string{"moteld"}, strings.Fields(args)...)
		main()
		return
	}
	for _, c := range []struct {
		args string
		want []string
	}{
		{"monitor --help", []string{"Usage:\n  moteld monitor [command]\n"}},
		{"monitor setup --help", []string{"Usage:\n  moteld monitor setup [--metrics] [flags]\n"}},
		{"monitor send --help", []string{"Usage:\n  moteld monitor send --source <name>"}},
		{"--help", []string{"Usage:\n  moteld [command]\n", "moteld is the smallhours app server"}},
	} {
		// The child's own -test.timeout bounds a help that came to block,
		// naming this test, before go test's timeout for the whole run.
		child := exec.Command(os.Args[0], "-test.run=^TestHelpNamesTheBinary$", "-test.timeout=60s")
		child.Env = append(os.Environ(), "MOTEL_HELP_ARGS="+c.args)
		out, err := child.CombinedOutput()
		if err != nil {
			t.Errorf("moteld %s: %v\n%s", c.args, err, out)
		}
		for _, w := range c.want {
			if !strings.Contains(string(out), w) {
				t.Errorf("moteld %s:\n%s\nwant %q", c.args, out, w)
			}
		}
		for _, caddy := range []string{"caddy monitor", "Use \"caddy ", "help for caddy", "To run Caddy, use"} {
			if strings.Contains(string(out), caddy) {
				t.Errorf("moteld %s says %q:\n%s", c.args, caddy, out)
			}
		}
	}
}
