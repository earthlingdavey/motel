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
	"time"

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

// TestHeldNumbersAreTheBodysOwn: a number is held whole, byte for byte,
// in every form encoding/json writes (negative, a fraction, an
// exponent), at any depth; digits in a string, a number-shaped string
// included, are text and filtered, at any depth. K's value is in the
// numbers, S's in the strings.
func TestHeldNumbersAreTheBodysOwn(t *testing.T) {
	r := NewRedactor([]string{"K=12345678", "S=87654321"}, nil)
	for _, c := range []struct{ in, want string }{
		{`{"a":123456789,"b":[-0.123456789,1.2345678e+21,9123456789e-7],"c":{"d":{"e":[[912345678]]}}}`,
			`{"a":123456789,"b":[-0.123456789,1.2345678e+21,9123456789e-7],"c":{"d":{"e":[[912345678]]}}}`},
		{`{"n": 123456789 , "m" :-123456789}`, `{"n": 123456789 , "m" :-123456789}`},
		{`{"s":"987654321","n":123456789}`, `{"n":123456789,"redacted_env":["S"],"s":"9[redacted:S]"}`},
		{`{"a":[{"b":"memory 987654321 bytes"}],"n":123456789}`, `{"a":[{"b":"memory 9[redacted:S] bytes"}],"n":123456789,"redacted_env":["S"]}`},
		{`{"log_tail":["87654321","x"],"n":12345678}`, `{"log_tail":["[redacted:S]","x"],"n":12345678,"redacted_env":["S"]}`},
		{`{"version":"v87654321","seconds":12345678.5}`, `{"redacted_env":["S"],"seconds":12345678.5,"version":"v[redacted:S]"}`},
		{`{"s":"12345678"}`, `{"redacted_env":["K"],"s":"[redacted:K]"}`},
	} {
		if got := r.redactJSON([]byte(c.in)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// TestHeldNumbersBesideTheirMarker: a number that holds a value the
// filter replaced elsewhere in the body (the app printed it) is sent
// whole beside the marker, the body served and the key named: figures
// are motel's own data, never hidden for the value they hold (rule 7,
// the owner's decision of 2026-11-21). So too beside an earlier pass's
// marker, and a time beside its value's marker.
func TestHeldNumbersBesideTheirMarker(t *testing.T) {
	r := NewRedactor([]string{"NODE_MEMORY_LIMIT=268435456", "API_VERSION=2023-11-14"}, nil)
	got := r.redactJSON([]byte(`{"caps":{"memory_max":268435456},"log_tail":["limit 268435456"]}`))
	if want := `{"caps":{"memory_max":268435456},"log_tail":["limit [redacted:NODE_MEMORY_LIMIT]"],"redacted_env":["NODE_MEMORY_LIMIT"]}`; got != want {
		t.Errorf("a number beside its value's marker:\n got %s\nwant %s", got, want)
	}
	// The marker an earlier pass put there, which a nested redacted_env
	// names (a failed deploy's detail): the filter finds nothing itself.
	early := `{"artifact_bytes":268435456,"detail":{"log_tail":["heap limit [redacted:NODE_MEMORY_LIMIT] exceeded"]},"redacted_env":["NODE_MEMORY_LIMIT"]}`
	if got = r.redactJSON([]byte(early)); got != early {
		t.Errorf("a number beside an earlier pass's marker:\n got %s\nwant %s", got, early)
	}
	// An earlier pass's marker whose key the nested redacted_env names
	// as a marker (a value equal to the key's name), and the marker as
	// encoding/json escapes it: the number stands whole beside either,
	// and the nested redacted_env stays as it was.
	r2 := NewRedactor([]string{"NODE_MEMORY_LIMIT=268435456", "ALIAS=NODE_MEMORY_LIMIT"}, nil)
	line, _ := r2.Redact("heap limit 268435456 exceeded")
	name, _ := r2.Redact("NODE_MEMORY_LIMIT")
	named, _ := json.Marshal(map[string]any{"artifact_bytes": 268435456, "detail": map[string]any{"log_tail": []string{line}}, "redacted_env": []string{name}})
	r3 := NewRedactor([]string{"NODE_MEMORY_LIMIT=268435456", "X=:NODE_MEMORY_LIMIT]"}, nil)
	escLine, _ := r3.Redact("heap limit 268435456 exceeded")
	escaped, _ := json.Marshal(map[string]any{"artifact_bytes": 268435456, "log_tail": []string{escLine}})
	if strings.Contains(string(named), `"NODE_MEMORY_LIMIT"`) || !strings.Contains(string(escaped), `\u003c`) {
		t.Fatalf("the bodies are not the cases: %s, %s", named, escaped)
	}
	for _, c := range []struct {
		what string
		r    *Redactor
		body []byte
	}{{"a marker whose key is named by a marker", r2, named}, {"an escaped marker", r3, escaped}} {
		// The filter finds nothing in either: each is served as it came.
		if got := c.r.redactJSON(c.body); got != string(c.body) {
			t.Errorf("a number beside %s:\n got %s\nwant %s", c.what, got, c.body)
		}
	}
	got = r.redactJSON([]byte(`{"started_at":"2023-11-14T22:13:20Z","log_tail":["on 2023-11-14"],"n":268435456}`))
	if want := `{"log_tail":["on [redacted:API_VERSION]"],"n":268435456,"redacted_env":["API_VERSION"],"started_at":"2023-11-14T22:13:20Z"}`; got != want {
		t.Errorf("a time beside its value's marker:\n got %s\nwant %s", got, want)
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

// TestHeldNumbersManyTokens: a value of digits alone is no piece of a
// placeholder, whatever the number of tokens a body holds (the
// 1235th's is "\u0000;1234"), so a body of thousands of numbers is
// held whole.
func TestHeldNumbersManyTokens(t *testing.T) {
	nums := make([]int, 2000)
	for i := range nums {
		nums[i] = i
	}
	raw, _ := json.Marshal(map[string]any{"n": nums, "started_at": "2023-11-14T22:13:20Z"})
	if got := NewRedactor([]string{"PIN=00001234", "API_VERSION=2023-11-14"}, nil).redactJSON(raw); got != string(raw) {
		t.Errorf("the body was not held whole: %.200s", got)
	}
}

// TestHeldNumbersFallBack: a known value across a number's edge, with
// the JSON beside it, sends the body through the filter whole, as on
// main: the value is not in the bytes written.
func TestHeldNumbersFallBack(t *testing.T) {
	const body = `{"a":"x","n":268435456,"z":"y"}`
	for _, value := range []string{`268435456,"z`, `56,"z":"y`, `":268435456`, `"n":268435456`, `x","n":2684`} {
		got := NewRedactor([]string{"K=" + value}, nil).redactJSON([]byte(body))
		if strings.Contains(got, value) || !json.Valid([]byte(got)) || !strings.Contains(got, `"redacted_env":["K"]`) {
			t.Errorf("K=%s: %s", value, got)
		}
	}
}

// BenchmarkRedactStatus is the filter over a status as GET /<app> sends
// it: a deploy with its phases, three records, the watchdog, the caps and
// the metrics summary, through a filter of five env_file values, none in
// the body (the normal path), and one whose value is a piece of a time.
func BenchmarkRedactStatus(b *testing.B) {
	at := time.Date(2026, 11, 20, 14, 3, 7, 123456789, time.UTC)
	f := func(v float64) *float64 { return &v }
	phases := []wire.PhaseTiming{}
	for i, n := range []string{"downloading", "extracting", "preparing", "starting", "soaking", "promoting", "draining", "stopping_old"} {
		phases = append(phases, wire.PhaseTiming{Name: n, StartedAt: at.Add(time.Duration(i) * time.Second), Seconds: 1.25 * float64(i)})
	}
	raw := func(t time.Time) json.RawMessage { b, _ := json.Marshal(t); return b }
	s := wire.Status{App: "demo", Phase: "idle", CurrentVersion: "4c2d9e7b1a05", Socket: "/var/lib/motel/demo/run/0f6cc53a30dc2be9/app.sock", PID: 4242,
		Command: []string{"/var/lib/motel/demo/releases/4c2d9e7b1a05/server", "--port", "0"}, Unit: "motel-demo.4c2d9e7b1a05.0f6cc53a30dc2be9.service", Running: true,
		Caps:              &wire.Caps{MemoryMax: 256 << 20, TasksMax: 512, CPUQuota: 200},
		Deploys:           []wire.DeploySummary{{Version: "4c2d9e7b1a05", Status: "succeeded", By: "github:org/repo", StartedAt: raw(at), FinishedAt: raw(at.Add(9 * time.Second))}, {Version: "v2", Status: "failed", Phase: "soaking", StartedAt: raw(at), FinishedAt: raw(at)}, {Version: "v1", Status: "succeeded", StartedAt: raw(at), FinishedAt: raw(at)}},
		LastDeploy:        &wire.DeployResult{Version: "4c2d9e7b1a05", Status: "succeeded", By: "github:org/repo", StartedAt: at, FinishedAt: at.Add(9 * time.Second), ArtifactEntries: 1834, ArtifactBytes: 48211968, Phases: phases},
		Watchdog:          &wire.Watchdog{State: "healthy", RestartsInWindow: 1, LastRestartAt: at, LastRestartCause: "health check failed"},
		AvailableVersions: []string{"4c2d9e7b1a05", "v1"},
		Metrics: &wire.Metrics{Window: "1h", At: &at, Memory: &wire.MemorySummary{Now: f(91234304), Max: f(120586240)}, MemoryMax: f(268435456), CPU: &wire.CPUSummary{Avg: 0.0123456789},
			Requests: &wire.RequestSummary{Total: 184467, PerSecond: 51.240833333333335, ByClass: map[string]uint64{"2xx": 180000, "4xx": 4000, "5xx": 467}, CacheHitShare: f(0.4166666666666667)}},
	}
	body, err := json.Marshal(s)
	if err != nil {
		b.Fatal(err)
	}
	env := []string{"DATABASE_URL=postgres://app:s3cr3t-pa55word@db.internal:5432/app", "SECRET_KEY_BASE=8f2b6c1d9e0a7f3b5c4d2e1f0a9b8c7d", "SMTP_PASSWORD=hunter2hunter2", "S3_BUCKET=demo-uploads-prod", "LOG_LEVEL=info"} // gitleaks:allow — made up for this benchmark
	for _, c := range []struct {
		name string
		env  []string
	}{{"none in the body", env}, {"a date in the body", append(env[:4:4], "API_VERSION=2026-11-20")}} {
		r := NewRedactor(c.env, []string{"demo", "4c2d9e7b1a05", "v1", "v2"})
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = r.redactJSON(body)
			}
		})
	}
}
