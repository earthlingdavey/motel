package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/notify"
)

// stale is a rules.yml a config with a metrics line wrote.
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

// The operator removes the metrics line and restarts motel instead of
// reloading: no MetricsStore is built, so the boot's gate retires the
// rules, whether the monitor block stays or goes. With the line, or at
// validate and reload, it writes nothing.
func TestABootWithNoMetricsLineRetiresTheRules(t *testing.T) {
	config, _ := dirs(t)
	noBlock := config + "/Caddyfile"
	write(t, noBlock, "{\n\tadmin off\n}\n", 0o644)
	noLine := config + "/Caddyfile.notify"
	write(t, noLine, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}\n}\n", 0o644)
	withLine := config + "/Caddyfile.store"
	write(t, withLine, "{\n\tadmin off\n\tmonitor {\n\t\tmetrics\n\t}\n}\n", 0o644)
	withSocket := config + "/Caddyfile.socket"
	write(t, withSocket, "{\n\tadmin off\n\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t\tmetrics /run/x/vm.sock\n\t}\n}\n", 0o644)

	for _, tc := range []struct {
		name, cmd, file string
		retired         bool
	}{
		{"run, no block", "run", noBlock, true},
		{"run, a monitor block with no metrics line", "run", noLine, true},
		{"run, the metrics line alone", "run", withLine, false},
		{"run, the metrics line with a socket, beside channels", "run", withSocket, false},
		{"validate, no block", "validate", noBlock, false},
		{"reload, no block", "reload", noBlock, false},
		{"validate, no metrics line", "validate", noLine, false},
		{"reload, no metrics line", "reload", noLine, false},
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
	if _, err := os.Stat(gone); !os.IsNotExist(err) || strings.Contains(stderr.String(), "monitor metrics") {
		t.Errorf("not set up: %v, said %q; want nothing made, and nothing said", err, stderr.String())
	}
}

// The gate reads the adapted JSON: the line is apps.monitor.metrics, an
// object; anything else is no line, and a config that is not JSON, or
// whose monitor app is not an object, is Caddy's to refuse.
func TestTheGateReadsTheMetricsLineFromTheJSON(t *testing.T) {
	for _, tc := range []struct {
		json    string
		retired bool
	}{
		{`{"apps":{"monitor":{"metrics":{}}}}`, false},
		{`{"apps":{"monitor":{"metrics":{"store":"/run/x/vm.sock"}}}}`, false},
		{`{"apps":{"monitor":{"channels":[{"name":"ops"}],"operator":["ops"]}}}`, true},
		{`{"apps":{"monitor":{"metrics":null}}}`, true},
		{`{"apps":{"metrics_store":{}}}`, true},
		{`{"apps":{}}`, true},
		{`{}`, true},
		{`{"apps":{"monitor":"x"}}`, false},
		{`not json`, false},
	} {
		dir := metricsDir(t)
		var stderr bytes.Buffer
		retireMetricsRules([]byte(tc.json), &stderr)
		b, err := os.ReadFile(filepath.Join(dir, notify.RulesName))
		if err != nil {
			t.Fatal(err)
		}
		if retired := string(b) != stale; retired != tc.retired {
			t.Errorf("%s: retired %v, want %v (%s)", tc.json, retired, tc.retired, stderr.String())
		}
	}
}
