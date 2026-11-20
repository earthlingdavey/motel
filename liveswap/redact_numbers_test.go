package liveswap

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
// included, are text and filtered, at any depth.
func TestHeldNumbersAreTheBodysOwn(t *testing.T) {
	r := NewRedactor([]string{"K=12345678"}, nil)
	for _, c := range []struct{ in, want string }{
		{`{"a":123456789,"b":[-0.123456789,1.2345678e+21,9123456789e-7],"c":{"d":{"e":[[912345678]]}}}`,
			`{"a":123456789,"b":[-0.123456789,1.2345678e+21,9123456789e-7],"c":{"d":{"e":[[912345678]]}}}`},
		{`{"n": 123456789 , "m" :-123456789}`, `{"n": 123456789 , "m" :-123456789}`},
		{`{"s":"123456789","n":123456789}`, `{"n":123456789,"redacted_env":["K"],"s":"[redacted:K]9"}`},
		{`{"a":[{"b":"memory 123456789 bytes"}],"n":1}`, `{"a":[{"b":"memory [redacted:K]9 bytes"}],"n":1,"redacted_env":["K"]}`},
		{`{"log_tail":["12345678","x"]}`, `{"log_tail":["[redacted:K]","x"],"redacted_env":["K"]}`},
		{`{"version":"v12345678","seconds":12345678.5}`, `{"redacted_env":["K"],"seconds":12345678.5,"version":"v[redacted:K]"}`},
	} {
		if got := r.redactJSON([]byte(c.in)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// TestHeldNumbersManyTokens: a value of digits alone is no piece of a
// placeholder, whatever the number of tokens a body holds (the
// 1235th's is "\u0000#1234"), so a body of thousands of numbers is
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
