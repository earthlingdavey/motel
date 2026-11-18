package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/notify"
)

// liveswapConfig is a Caddyfile whose liveswap block names root, or
// none for "".
func liveswapConfig(t *testing.T, root string) string {
	t.Helper()
	config, _ := dirs(t)
	line := ""
	if root != "" {
		line = "\t\troot " + root + "\n"
	}
	file := config + "/Caddyfile"
	write(t, file, "{\n\tadmin off\n\tliveswap {\n"+line+"\t}\n}\n", 0o644)
	return file
}

// asRoot has the gate run as root, whoever runs the test.
func asRoot(t *testing.T) {
	t.Helper()
	was := euid
	euid = func() int { return 0 }
	t.Cleanup(func() { euid = was })
}

// notRoots is a directory root does not own, as the motel user owns a
// box's liveswap root: the test's own, given away when the test runs as
// root.
func notRoots(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// `moteld run` by root with a config whose liveswap root is another
// user's (root trying a file by hand beside moteld.service) is refused
// before the gate writes anything, and the one line said is the
// refusal, with what to do instead. validate and reload of the same
// file go on.
//
// The state directory is the test's own, and the gate's write after the
// refusal would show either way: run as root, the gate would give it
// the config that sends nothing (it is root's); run as another user, it
// would leave it with a WARNING, a second line.
func TestARunByHandAsRootIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	root := notRoots(t)
	file := liveswapConfig(t, root)
	state := t.TempDir()
	wasDir, wasSock, wasMetrics := notifyStateDir, notifySocket, metricsStateDir
	notifyStateDir, notifySocket = state, filepath.Join(t.TempDir(), "absent.sock")
	metricsStateDir = filepath.Join(t.TempDir(), "motel-metrics")
	t.Cleanup(func() { notifyStateDir, notifySocket, metricsStateDir = wasDir, wasSock, wasMetrics })
	rendered := "# Rendered by motel\nreceivers: [ops]\n"
	write(t, filepath.Join(state, notify.ConfigName), rendered, 0o640)
	asRoot(t)

	var stderr bytes.Buffer
	if !gate([]string{"run", "--config", file}, &stderr) {
		t.Fatalf("went on: %s", stderr.String())
	}
	if b, _ := os.ReadFile(filepath.Join(state, notify.ConfigName)); string(b) != rendered {
		t.Errorf("the state directory was changed: %q", b)
	}
	said := stderr.String()
	for _, want := range []string{
		"Error: this `moteld run` is refused, and nothing was changed: the liveswap root " + root + " is owned by ",
		", and this process runs as root, not as its owner. On a box, moteld runs as moteld.service",
		"`moteld validate --config <file>`",
		"`journalctl -u moteld`",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("said %q; want %q in it", said, want)
		}
	}
	if strings.Count(said, "\n") != 1 {
		t.Errorf("said more than the refusal: %q", said)
	}
	for _, cmd := range []string{"validate", "reload"} {
		stderr.Reset()
		if gate([]string{cmd, "--config", file}, &stderr) || strings.Contains(stderr.String(), "refused") {
			t.Errorf("%s: %s", cmd, stderr.String())
		}
	}
}

// What goes on: any user but root (the motel user, a service user that
// writes a root it does not own), and root with a config that has no
// liveswap block, a root not there yet (a first start), or a root of
// its own (a container that runs moteld as root). A root left to its
// default, or read from the environment at load, is judged where it
// leads.
func TestARunByAnotherUserOrWithNoRootOfAnothersGoesOn(t *testing.T) {
	wasDir, wasMetrics, wasDefault := notifyStateDir, metricsStateDir, liveswapDefaultRoot
	notifyStateDir = filepath.Join(t.TempDir(), "motel-notify")
	metricsStateDir = filepath.Join(t.TempDir(), "motel-metrics")
	t.Cleanup(func() { notifyStateDir, metricsStateDir, liveswapDefaultRoot = wasDir, wasMetrics, wasDefault })
	run := func(file string) (bool, string) {
		var stderr bytes.Buffer
		stop := gate([]string{"run", "--config", file}, &stderr)
		return stop, stderr.String()
	}

	asAnotherUser(t)
	if stop, said := run(liveswapConfig(t, t.TempDir())); stop || said != "" {
		t.Errorf("another user, not the root's owner: stop %v, said %q", stop, said)
	}

	asRoot(t)
	config, _ := dirs(t)
	write(t, config+"/Caddyfile", "{\n\tadmin off\n}\n", 0o644)
	if stop, said := run(config + "/Caddyfile"); stop || said != "" {
		t.Errorf("no liveswap block: stop %v, said %q", stop, said)
	}
	if stop, said := run(liveswapConfig(t, filepath.Join(t.TempDir(), "liveswap"))); stop || said != "" {
		t.Errorf("a root not there yet: stop %v, said %q", stop, said)
	}
	// "/" is root's on every machine.
	if stop, said := run(liveswapConfig(t, "/")); stop || said != "" {
		t.Errorf("a root of root's: stop %v, said %q", stop, said)
	}

	liveswapDefaultRoot = notRoots(t)
	if stop, said := run(liveswapConfig(t, "")); !stop || !strings.Contains(said, liveswapDefaultRoot+" is owned by ") {
		t.Errorf("the default root: stop %v, said %q", stop, said)
	}
	env := notRoots(t)
	t.Setenv("MOTEL_TEST_LIVESWAP_ROOT", env)
	if stop, said := run(liveswapConfig(t, "{env.MOTEL_TEST_LIVESWAP_ROOT}")); !stop || !strings.Contains(said, env+" is owned by ") {
		t.Errorf("a root from the environment: stop %v, said %q", stop, said)
	}
}
