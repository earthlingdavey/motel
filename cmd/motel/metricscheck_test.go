package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/notify"
)

// stale is a rules.yml a config with a metrics_store block wrote.
const stale = "groups:\n  - name: motel-store\n    interval: 1m\n    rules:\n      - alert: metrics_store_disk_fill\n        expr: vector(1)\n"

// metricsDir stands a state directory in for setup's, holding stale.
func metricsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, notify.RulesName), stale, 0o640)
	was := metricsStateDir
	metricsStateDir = dir
	t.Cleanup(func() { metricsStateDir = was })
	return dir
}

// The operator removes metrics_store and restarts motel instead of
// reloading: no MetricsStore is built, so the boot's gate retires the
// rules. With the block, or at validate and reload, it writes nothing.
func TestABootWithNoMetricsBlockRetiresTheRules(t *testing.T) {
	config, _ := dirs(t)
	noBlock := config + "/Caddyfile"
	write(t, noBlock, "{\n\tadmin off\n}\n", 0o644)
	withBlock := config + "/Caddyfile.store"
	write(t, withBlock, "{\n\tadmin off\n\tmetrics_store\n}\n", 0o644)

	for _, tc := range []struct {
		name, cmd, file string
		retired         bool
	}{
		{"run, no block", "run", noBlock, true},
		{"run, the block", "run", withBlock, false},
		{"validate, no block", "validate", noBlock, false},
		{"reload, no block", "reload", noBlock, false},
	} {
		dir := metricsDir(t)
		var stderr bytes.Buffer
		if gate([]string{tc.cmd, "--config", tc.file}, &stderr) {
			t.Fatalf("%s: stopped: %s", tc.name, stderr.String())
		}
		b, err := os.ReadFile(filepath.Join(dir, notify.RulesName))
		if err != nil {
			t.Fatal(err)
		}
		said := strings.Contains(stderr.String(), notify.RulesRetiredMessage)
		switch {
		case tc.retired && (strings.Contains(string(b), "alert:") || !strings.Contains(string(b), "name: motel-store") || !said):
			t.Errorf("%s: rules.yml %q, said %v; want the groups empty, and the line", tc.name, b, said)
		case !tc.retired && (string(b) != stale || said):
			t.Errorf("%s: rules.yml %q, said %v; want it untouched, and no line", tc.name, b, said)
		}
	}

	// A box the store is not set up on has no state directory: nothing
	// is written, and nothing said.
	gone := filepath.Join(t.TempDir(), "motel-metrics")
	was := metricsStateDir
	metricsStateDir = gone
	t.Cleanup(func() { metricsStateDir = was })
	var stderr bytes.Buffer
	gate([]string{"run", "--config", noBlock}, &stderr)
	if _, err := os.Stat(gone); !os.IsNotExist(err) || strings.Contains(stderr.String(), "metrics_store") {
		t.Errorf("not set up: %v, said %q; want nothing made, and nothing said", err, stderr.String())
	}
}
