package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLoadWarningsSayStaleUnits: a box upgraded past a change to setup's
// units keeps the old ones until root runs setup again (hotserve-1 kept
// root proxies so). Each load's gate, the restart onto the new binary's
// among them, says which differ and which setup puts them back; units
// as this binary writes them, or none at all, say nothing.
func TestLoadWarningsSayStaleUnits(t *testing.T) {
	root := t.TempDir()
	was := unitRoot
	unitRoot = root
	t.Cleanup(func() { unitRoot = was })
	put := func(path string, data []byte) {
		t.Helper()
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	both := []byte(`{"channels":[{"name":"ops","kind":"ntfy","secret":"NOTIFY_OPS_URL"}],"operator":["ops"],"metrics":{}}`)
	stale := func(cmd string) string {
		var out []string
		for _, w := range LoadWarnings(cmd, both) {
			if strings.Contains(w, "differ") {
				out = append(out, w)
			}
		}
		return strings.Join(out, "\n")
	}
	if got := stale("run"); got != "" {
		t.Fatalf("no units on disk: %s", got)
	}
	for _, u := range append(amUnits(), metricsUnits()...) {
		put(u.path, u.data)
	}
	if got := stale("run"); got != "" {
		t.Fatalf("this binary's units: %s", got)
	}
	// A comment changed, a blank line and CRLF endings, and nothing
	// else: not stale.
	reformatted := bytes.ReplaceAll(append([]byte("# Written by an older motel monitor setup\n\n"), uncommented(unitDropIn)...), []byte("\n"), []byte("\r\n"))
	put(dropInPath, reformatted)
	if got := stale("run"); got != "" {
		t.Fatalf("a comment alone: %s", got)
	}
	put(socketPath, []byte("[Socket]\nListenStream=/run/old.sock\n"))
	put(metricsUnits()[5].path, []byte("[Service]\nUser=root\n"))
	got := stale("run")
	if !strings.Contains(got, "("+socketPath+"): run motel monitor setup as root") ||
		!strings.Contains(got, "("+metricsUnits()[5].path+"): run motel monitor setup --metrics as root") {
		t.Fatalf("stale units: %s", got)
	}
	// A block with channels alone is told of Alertmanager's units alone.
	if w := LoadWarnings("run", []byte(`{"channels":[{"name":"ops","kind":"ntfy","secret":"NOTIFY_OPS_URL"}],"operator":["ops"]}`)); len(w) != 1 || !strings.Contains(w[0], socketPath) {
		t.Fatalf("notify alone: %v", w)
	}
	// The store alone, of the store's alone.
	if w := LoadWarnings("run", []byte(`{"metrics":{}}`)); len(w) != 1 || !strings.Contains(w[0], "--metrics") {
		t.Fatalf("the store alone: %v", w)
	}
	// scrape.yml as setup wrote it before the store kept only what motel
	// reads: the store scrapes every series until setup runs again.
	put(metricsUnits()[5].path, metricsUnits()[5].data)
	scrape := filepath.Join(DefaultMetricsStateDir, scrapeName)
	put(scrape, []byte("global:\n  scrape_interval: 30s\nscrape_configs:\n  - job_name: vmalert\n    static_configs: [{targets: [\"127.0.0.1:8880\"]}]\n"))
	if got := stale("run"); !strings.Contains(got, "("+scrape+"): run motel monitor setup --metrics as root") {
		t.Fatalf("an old scrape.yml: %s", got)
	}
	put(scrape, append([]byte("# a comment of an older setup\n"), scrapeConfig...))
	if got := stale("run"); strings.Contains(got, scrape) {
		t.Fatalf("scrape.yml with a comment changed: %s", got)
	}
	// Indentation is YAML's structure: a key moved out of its parent
	// differs, though each line trims to the same text.
	moved := bytes.Replace(scrapeConfig, []byte("\n  scrape_interval: 30s"), []byte("\nscrape_interval: 30s"), 1)
	if bytes.Equal(moved, scrapeConfig) || !bytes.Equal(uncommented(moved), uncommented(scrapeConfig)) {
		t.Fatal("the re-indented scrape.yml is not the case it should be")
	}
	put(scrape, moved)
	if got := stale("run"); !strings.Contains(got, scrape) {
		t.Fatalf("scrape.yml re-indented: %s", got)
	}
	// One this uid cannot read is said so, not passed as the same.
	if err := os.Remove(filepath.Join(root, scrape)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, scrape), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := LoadWarnings("run", both); !slices.ContainsFunc(w, func(s string) bool {
		return strings.Contains(s, scrape+" could not be compared with this motel's (is a directory)") && strings.Count(s, scrape) == 1
	}) {
		t.Fatalf("an unreadable scrape.yml: %v", w)
	}
	if w := LoadWarnings("run", nil); len(w) != 0 {
		t.Fatalf("no block: %v", w)
	}
}

// TestLoadWarningsSayTheProxyGoes: a box set up before Alertmanager held
// its own socket in the namespace (#435) has the proxy's units, which
// setup retires, restarting Alertmanager once. The warning before setup
// says so (the deploy of a8038b4 to hotserve-1 said only which files
// differ), and says it with no file stale too: the new socket and drop-in
// are missing there, which no comparison sees.
func TestLoadWarningsSayTheProxyGoes(t *testing.T) {
	root := t.TempDir()
	was := unitRoot
	unitRoot = root
	t.Cleanup(func() { unitRoot = was })
	put := func(path string, data []byte) {
		t.Helper()
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range metricsUnits() {
		put(u.path, u.data)
	}
	store := []byte(`{"metrics":{}}`)
	if w := LoadWarnings("run", store); len(w) != 0 {
		t.Fatalf("this binary's units, no proxy: %v", w)
	}
	sock, svc := filepath.Join(unitDir, oldAMProxySocket), filepath.Join(unitDir, oldAMProxyService)
	put(sock, []byte("[Socket]\nListenStream=127.0.0.1:9093\n"))
	put(svc, []byte("[Service]\nExecStart=/usr/lib/systemd/systemd-socket-proxyd /run/motel-notify/am.sock\n"))
	// Setup restarts Alertmanager only when it does not listen in the
	// namespace yet (a retirement cut short may have left it listening).
	const restarts = " and, unless Alertmanager listens in the store's namespace already, restarts (or starts) Alertmanager once so that it does"
	want := "monitor: the proxy an older setup wrote for Alertmanager (" + sock + ", " + svc + ") is still in place: run motel monitor setup --metrics as root, which retires it" + restarts
	if w := LoadWarnings("run", store); len(w) != 1 || w[0] != want {
		t.Fatalf("the proxy alone:\n%v\nwant\n%s", w, want)
	}
	put(metricsUnits()[1].path, []byte("[Service]\nType=oneshot\n"))
	want = "monitor: the files motel monitor setup --metrics wrote differ from this motel's (" + metricsUnits()[1].path + "), and the proxy an older setup wrote for Alertmanager (" + sock + ", " + svc + ") is still in place: run motel monitor setup --metrics as root to put this version's in place; it retires the proxy" + restarts
	if w := LoadWarnings("run", store); len(w) != 1 || w[0] != want {
		t.Fatalf("a stale unit and the proxy:\n%v\nwant\n%s", w, want)
	}
	// A retirement cut short after the socket: the service alone named.
	put(metricsUnits()[1].path, metricsUnits()[1].data)
	if err := os.Remove(filepath.Join(root, sock)); err != nil {
		t.Fatal(err)
	}
	want = "monitor: the proxy an older setup wrote for Alertmanager (" + svc + ") is still in place: run motel monitor setup --metrics as root, which retires it" + restarts
	if w := LoadWarnings("run", store); len(w) != 1 || w[0] != want {
		t.Fatalf("the proxy's service alone:\n%v\nwant\n%s", w, want)
	}
}
