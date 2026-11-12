package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// A Caddyfile with a monitor block on a box setup has not run on: what
// a load journals alone, validate and reload say too; a boot says it in
// the journal already, and the gate does not repeat it.
func TestTheGateSaysMonitorIsNotSetUp(t *testing.T) {
	config, _ := dirs(t)
	file := config + "/Caddyfile"
	write(t, file, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tstate_dir "+config+"/absent\n\t}\n}\n", 0o644)
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
