package liveswap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// TestStatusNumbersPassNoFilter: an env_file value of eight or more
// digits that is a piece of a number the status holds leaves the number
// whole and the status read, where it was withheld whole: the app's own
// memory limit in bytes, equal to its memory_max, on the deploy's answer
// and every read, and on after the value left the env_file; and a
// memory figure the metrics store answered.
func TestStatusNumbersPassNoFilter(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	rig.spec.caps = resourceCaps{memoryMax: 256 << 20}
	must(t, os.WriteFile(rig.spec.envFile, []byte("NODE_MEMORY_LIMIT=268435456\n"), 0o600))
	read := func(what, body string) {
		t.Helper()
		var s wire.Status
		if err := json.Unmarshal([]byte(body), &s); err != nil || s.App != "demo" || s.Caps == nil || s.Caps.MemoryMax != 256<<20 || strings.Contains(body, "redacted") {
			t.Errorf("%s: %v: %s", what, err, body)
		}
	}
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	read("the deploy's answer", w.Body.String())
	w = do(t, h, http.MethodGet, "/demo", appToken(t), "")
	read("a read", w.Body.String())
	must(t, os.WriteFile(rig.spec.envFile, []byte("OTHER=x\n"), 0o600))
	w = do(t, h, http.MethodGet, "/demo", appToken(t), "")
	read("a read after the value left the env_file", w.Body.String())

	h, _ = heldRig(t, "ACCOUNT_ID=12345678\n")
	now := 123456789.0
	h.app.SetMetricsSource(&fakeMetricsSource{reply: &wire.Metrics{Window: "1h", Memory: &wire.MemorySummary{Now: &now}}})
	w = do(t, h, http.MethodGet, "/demo", appToken(t), "")
	var s wire.Status
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || s.Metrics == nil || s.Metrics.Memory == nil || *s.Metrics.Memory.Now != now || strings.Contains(w.Body.String(), "redacted") {
		t.Errorf("a metrics figure: %v: %s", err, w.Body)
	}
}

// TestDeployRecordNumbersPassNoFilter: a record whose number holds a
// value's digits (the artifact's size) is written whole, not as the
// envelope, and reads back.
func TestDeployRecordNumbersPassNoFilter(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	rig.fetch.stats = archiveStats{entries: 3, bytes: 12345678}
	must(t, os.WriteFile(rig.spec.envFile, []byte("UPLOAD_MAX=12345678\n"), 0o600))
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`); w.Code != http.StatusOK {
		t.Fatalf("deploying v1: %d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(deployRecordPath(rig.spec.dirs.deploys, "v1"))
	if err != nil || !strings.Contains(string(b), `"artifact_bytes":12345678`) || strings.Contains(string(b), "record withheld") {
		t.Fatalf("the record on disk: %v %s", err, b)
	}
	w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	var rec wire.DeployResult
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || rec.ArtifactBytes != 12345678 || len(rec.Phases) == 0 {
		t.Fatalf("%v: %d %s", err, w.Code, w.Body)
	}
}

// TestFailedDeployNumberBesideItsMarker: a failed deploy whose app
// printed its memory limit, which equals its memory_max and the
// artifact's size, has the marker in its detail before the filter runs;
// the deploy's answer, the status (v0 serving, its caps shown), the
// record and the stream's last line are each served with the figures
// whole beside the marker, the printed value replaced and its key named,
// never withheld or the envelope.
func TestFailedDeployNumberBesideItsMarker(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "demo.env")
	rig.spec.caps = resourceCaps{memoryMax: 256 << 20}
	rig.spec.deployLogLines = 40
	must(t, os.WriteFile(rig.spec.envFile, []byte("NODE_MEMORY_LIMIT=268435456\n"), 0o600))
	rig.fetch.stats = archiveStats{entries: 3, bytes: 268435456}
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/0.tgz","version":"v0"}`); w.Code != http.StatusOK {
		t.Fatalf("deploying v0: %d %s", w.Code, w.Body)
	}
	rig.ma.journal = &fakeJournal{lines: []string{"node: heap limit 268435456 exceeded"}}
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: "limit 268435456"})
	check := func(what, body string, figures ...string) {
		t.Helper()
		var keys struct {
			RedactedEnv []string `json:"redacted_env"`
		}
		if err := json.Unmarshal([]byte(body), &keys); err != nil || !slices.Contains(keys.RedactedEnv, "NODE_MEMORY_LIMIT") ||
			!strings.Contains(body, "[redacted:NODE_MEMORY_LIMIT]") || strings.Contains(body, "withheld") {
			t.Errorf("%s: %v: %s", what, err, body)
		}
		// The figures whole, and the value nowhere else.
		rest := body
		for _, f := range figures {
			if !strings.Contains(rest, f) {
				t.Errorf("%s: %s is not whole: %s", what, f, body)
			}
			rest = strings.ReplaceAll(rest, f, "")
		}
		if strings.Contains(rest, "268435456") {
			t.Errorf("%s: the value stands outside the figures: %s", what, body)
		}
	}
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	check("the deploy's answer", w.Body.String(), `"memory_max":268435456`, `"artifact_bytes":268435456`)
	check("the status", do(t, h, http.MethodGet, "/demo", appToken(t), "").Body.String(), `"memory_max":268435456`, `"artifact_bytes":268435456`)
	b, _ := os.ReadFile(deployRecordPath(rig.spec.dirs.deploys, "v1"))
	check("the record on disk", string(b), `"artifact_bytes":268435456`)
	check("the record read", do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "").Body.String(), `"artifact_bytes":268435456`)
	// A fresh tail, as the real journal reads one: the fake's lines
	// were replaced in place by the first deploy's detail.
	rig.ma.journal = &fakeJournal{lines: []string{"node: heap limit 268435456 exceeded"}}
	sw, _ := streamLinesOf(t, h, `{"url":"https://x/2.tgz","version":"v2"}`)
	lines := strings.Split(strings.TrimRight(sw.Body.String(), "\n"), "\n")
	check("the stream's last line", lines[len(lines)-1], `"memory_max":268435456`, `"artifact_bytes":268435456`)
}
