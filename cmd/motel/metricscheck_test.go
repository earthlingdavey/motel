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

// A monitor rule that names an app and reads its responses needs a
// route observing that app, at any depth; memory and CPU need none, nor
// does an every-app rule.
func TestARuleOnAnAppNoRouteObservesIsRefused(t *testing.T) {
	rules := func(r string) string { return `{"apps":{"monitor":{"metrics":{"rules":[` + r + `]}}` }
	observed := `,"http":{"servers":{"s":{"routes":[{"handle":[{"handler":"subroute","routes":[{"handle":[{"handler":"liveswap_observe","app":"blog"}]}]}]}]}}}}}`
	for name, c := range map[string]struct{ cfg, want string }{
		"a threshold, unobserved": {rules(`{"name":"slow","kind":"threshold","metric":"p95","op":">","value":1,"for":"10m","app":"blog"}`) + `}}`,
			"rule slow reads app blog's responses, and no route has `observe blog`"},
		"a post_deploy, unobserved": {rules(`{"name":"regress","kind":"post_deploy","app":"blog","max_p95":"1s"}`) + `}}`,
			"rule regress reads app blog's responses"},
		"observed in a subroute": {rules(`{"name":"slow","kind":"threshold","metric":"p95","op":">","value":1,"for":"10m","app":"blog"}`) + observed, ""},
		"another app observed":   {rules(`{"name":"slow","kind":"threshold","metric":"p95","op":">","value":1,"for":"10m","app":"shop"}`) + observed, "app shop's responses"},
		"memory_share":           {rules(`{"name":"mem","kind":"threshold","metric":"memory_share","op":">","value":0.8,"for":"5m","app":"blog"}`) + `}}`, ""},
		"cpu_cores":              {rules(`{"name":"cpu","kind":"threshold","metric":"cpu_cores","op":">","value":1,"for":"5m","app":"blog"}`) + `}}`, ""},
		"every app":              {rules(`{"name":"slow","kind":"threshold","metric":"p95","op":">","value":1,"for":"10m"}`) + `}}`, ""},
		"no metrics line":        {`{"apps":{"monitor":{}}}`, ""},
		"not JSON":               {`{`, ""},
	} {
		err := checkObserved([]byte(c.cfg))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// Through the gate, from a Caddyfile: validate and reload refuse it,
	// run serves it saying why.
	config, _ := dirs(t)
	caddyfile := func(site string) string {
		return "{\n\tadmin off\n\tliveswap {\n\t\tapp blog {\n\t\t\tcommand /bin/true\n\t\t}\n\t}\n" +
			"\tmonitor {\n\t\tmetrics\n\t\trule slow threshold p95 > 1s for 10m app blog\n\t}\n}\n:8080 {\n" + site + "\treverse_proxy {\n\t\tdynamic liveswap blog\n\t}\n}\n"
	}
	write(t, config+"/Caddyfile", caddyfile("\tobserve blog\n"), 0o644)
	if _, err := checkConfig("validate", []string{"--config", config + "/Caddyfile"}); err != nil {
		t.Errorf("an observed app: %v", err)
	}
	write(t, config+"/Caddyfile", caddyfile(""), 0o644)
	for cmd, stops := range map[string]bool{"validate": true, "reload": true, "run": false} {
		var stderr bytes.Buffer
		if got := gate([]string{cmd, "--config", config + "/Caddyfile"}, &stderr); got != stops || !strings.Contains(stderr.String(), "no route has `observe blog`") {
			t.Errorf("%s: stop = %v, want %v; said %q", cmd, got, stops, stderr.String())
		}
	}
}
