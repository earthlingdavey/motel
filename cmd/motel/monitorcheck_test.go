package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/notify"
)

// A Caddyfile with a monitor block on a box setup has not run on: what
// a load journals alone, validate and reload say too; a boot says it in
// the journal already, and the gate does not repeat it.
func TestTheGateSaysMonitorIsNotSetUp(t *testing.T) {
	if _, err := os.Stat(notify.DefaultStateDir); err == nil {
		t.Skip("this machine has setup's state directory")
	}
	config, _ := dirs(t)
	file := config + "/Caddyfile"
	write(t, file, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n}\n", 0o644)
	for _, cmd := range []string{"validate", "reload", "run"} {
		var stderr bytes.Buffer
		if stop, _ := gateArgs([]string{cmd, "--config", file}, &stderr); stop {
			t.Fatalf("%s: stopped: %s", cmd, stderr.String())
		}
		said := strings.Contains(stderr.String(), "WARNING: monitor: Alertmanager is not set up on this box") &&
			strings.Contains(stderr.String(), "motel monitor setup")
		if said != (cmd != "run") {
			t.Errorf("%s: said %v: %s", cmd, said, stderr.String())
		}
	}
}

// A restart or a boot into a config with no monitor block with channels
// gives Alertmanager the config that sends nothing: no notify App would
// take back the last render, and Alertmanager would end what it holds
// with a recovery that did not happen. A config with channels renders
// its own at Start and is left alone, and a state directory already
// holding the config that sends nothing is not touched again.
func TestTheGateSendsNothingAtABootWithoutChannels(t *testing.T) {
	config, _ := dirs(t)
	noBlock := config + "/Caddyfile"
	write(t, noBlock, "{\n\tadmin off\n}\n", 0o644)
	withBlock := config + "/Caddyfile.notify"
	write(t, withBlock, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n}\n", 0o644)
	state := t.TempDir()
	wasDir, wasSock := notifyStateDir, notifySocket
	notifyStateDir, notifySocket = state, filepath.Join(t.TempDir(), "absent.sock")
	t.Cleanup(func() { notifyStateDir, notifySocket = wasDir, wasSock })
	rendered := func() {
		write(t, filepath.Join(state, notify.ConfigName), "# Rendered by motel\nreceivers: [ops]\n", 0o640)
		write(t, filepath.Join(state, "url-NOTIFY_OPS_URL"), "https://ntfy.sh/ops\n", 0o640)
	}
	rendered()
	var stderr bytes.Buffer
	gate([]string{"run", "--config", withBlock}, &stderr)
	if b, _ := os.ReadFile(filepath.Join(state, notify.ConfigName)); !strings.Contains(string(b), "Rendered by motel") || strings.Contains(stderr.String(), notify.SendNothingMessage) {
		t.Fatalf("a config with channels: %q, said %q", b, stderr.String())
	}
	for _, cmd := range []string{"validate", "reload"} {
		stderr.Reset()
		gate([]string{cmd, "--config", noBlock}, &stderr)
		if strings.Contains(stderr.String(), notify.SendNothingMessage) {
			t.Fatalf("%s wrote: %s", cmd, stderr.String())
		}
	}
	stderr.Reset()
	gate([]string{"run", "--config", noBlock}, &stderr)
	b, _ := os.ReadFile(filepath.Join(state, notify.ConfigName))
	_, urlErr := os.Stat(filepath.Join(state, "url-NOTIFY_OPS_URL"))
	if !strings.Contains(string(b), "receiver: blackhole") || !os.IsNotExist(urlErr) || !strings.Contains(stderr.String(), notify.SendNothingMessage) {
		t.Fatalf("a boot without the block: %q, url file %v, said %q", b, urlErr, stderr.String())
	}
	stderr.Reset()
	gate([]string{"run", "--config", noBlock}, &stderr)
	if strings.Contains(stderr.String(), notify.SendNothingMessage) {
		t.Fatalf("the config that sends nothing was written again: %s", stderr.String())
	}
}

// asAnotherUser has the gate run as a user that owns none of the test's
// directories: root trying a file by hand on a live box.
func asAnotherUser(t *testing.T) {
	t.Helper()
	was := euid
	euid = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { euid = was })
}

// `motel run` by a user other than the state directory's owner (root
// trying a file by hand, the service running) is not motel.service's
// start: it leaves Alertmanager's config, the URL files and the
// producers' kept states as the running service left them, writes
// nothing the service could not read, and says why. A box setup has
// not run on stays unsaid.
func TestARunByHandLeavesTheServicesAlertmanagerAlone(t *testing.T) {
	config, _ := dirs(t)
	noBlock := config + "/Caddyfile"
	write(t, noBlock, "{\n\tadmin off\n}\n", 0o644)
	state := t.TempDir()
	wasDir, wasSock := notifyStateDir, notifySocket
	notifyStateDir, notifySocket = state, filepath.Join(t.TempDir(), "absent.sock")
	t.Cleanup(func() { notifyStateDir, notifySocket = wasDir, wasSock })
	// The store's directory is not there: this machine's own, if it has
	// one, is not this test's to report on.
	wasMetrics := metricsStateDir
	metricsStateDir = filepath.Join(t.TempDir(), "motel-metrics")
	t.Cleanup(func() { metricsStateDir = wasMetrics })
	rendered := "# Rendered by motel\nreceivers: [ops]\n"
	write(t, filepath.Join(state, notify.ConfigName), rendered, 0o640)
	write(t, filepath.Join(state, "url-NOTIFY_OPS_URL"), "https://ntfy.sh/ops\n", 0o640)
	write(t, filepath.Join(state, "external.json"), "{}\n", 0o640)
	asAnotherUser(t)

	var stderr bytes.Buffer
	if gate([]string{"run", "--config", noBlock}, &stderr) {
		t.Fatalf("stopped: %s", stderr.String())
	}
	b, _ := os.ReadFile(filepath.Join(state, notify.ConfigName))
	_, urlErr := os.Stat(filepath.Join(state, "url-NOTIFY_OPS_URL"))
	_, extErr := os.Stat(filepath.Join(state, "external.json"))
	entries, _ := os.ReadDir(state)
	if string(b) != rendered || urlErr != nil || extErr != nil || len(entries) != 3 {
		t.Errorf("the state directory was changed: config %q, url file %v, external.json %v, %d entries", b, urlErr, extErr, len(entries))
	}
	said := stderr.String()
	if strings.Contains(said, notify.SendNothingMessage) ||
		!strings.Contains(said, "WARNING: monitor: this config has no monitor block with channels, and Alertmanager was left as it is: "+state+" is owned by ") ||
		!strings.Contains(said, ", not as its owner: this process writes nothing there and reloads nothing\n") {
		t.Errorf("said %q; want why the service's Alertmanager was left alone", said)
	}

	notifyStateDir = filepath.Join(t.TempDir(), "motel-notify")
	stderr.Reset()
	gate([]string{"run", "--config", noBlock}, &stderr)
	if strings.Contains(stderr.String(), "monitor") {
		t.Errorf("not set up: said %q; want nothing", stderr.String())
	}
}

// `motel reload` of a config with a monitor block loads it even when it
// is unchanged: the block reads the box's secrets file, which the config
// does not hold, so a mended _box.env would otherwise load nothing.
// Others, and a reload already forced, run as given.
func TestTheGateForcesAReloadOfTheMonitorBlock(t *testing.T) {
	config, _ := dirs(t)
	withBlock := config + "/Caddyfile"
	write(t, withBlock, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n}\n", 0o644)
	noBlock := config + "/Caddyfile.none"
	write(t, noBlock, "{\n\tadmin off\n}\n", 0o644)
	storeAlone := config + "/Caddyfile.store"
	write(t, storeAlone, "{\n\tadmin off\n\tmonitor {\n\t\tmetrics\n\t}\n}\n", 0o644)
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"reload", "--config", withBlock}, []string{"reload", "--config", withBlock, "--force"}},
		{[]string{"reload", "--config", withBlock, "--force"}, []string{"reload", "--config", withBlock, "--force"}},
		{[]string{"reload", "-f", "--config", withBlock}, []string{"reload", "-f", "--config", withBlock}},
		{[]string{"reload", "--config", withBlock, "--force=false"}, []string{"reload", "--config", withBlock, "--force=false"}},
		{[]string{"reload", "--config", noBlock}, []string{"reload", "--config", noBlock}},
		// A metrics line alone reads no secrets.
		{[]string{"reload", "--config", storeAlone}, []string{"reload", "--config", storeAlone}},
		{[]string{"validate", "--config", withBlock}, []string{"validate", "--config", withBlock}},
		{[]string{"version"}, []string{"version"}},
	} {
		var stderr bytes.Buffer
		stop, run := gateArgs(c.args, &stderr)
		if stop || !slices.Equal(run, c.want) {
			t.Errorf("%v: stop %v, run %v; want %v (%s)", c.args, stop, run, c.want, stderr.String())
		}
	}
}
