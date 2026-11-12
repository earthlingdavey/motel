package notify

import (
	"os"
	"path/filepath"
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
	both := []byte(`{"channels":[{"name":"ops","kind":"ntfy","secret":"NOTIFY_OPS_URL"}],"operator":["ops"],"state_dir":"` + t.TempDir() + `","metrics":{}}`)
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
	// A comment changed, and nothing else: not stale.
	put(dropInPath, append([]byte("# Written by an older motel monitor setup\n"), uncommented(unitDropIn)...))
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
	if w := LoadWarnings("run", nil); len(w) != 0 {
		t.Fatalf("no block: %v", w)
	}
}
