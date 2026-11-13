package liveswap

import (
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/smallhoursorg/motel/liveswap/wire"
)

// The deploy route's bodies, byte for byte (testdata/wire): what a
// client decodes. A change to a field's name, tag or place, or to a
// refusal's shape, fails here; `go test -run TestWire -update-wire`
// rewrites the files, and the diff is the change on the wire.
var updateWire = flag.Bool("update-wire", false, "rewrite testdata/wire")

func checkWireGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "wire", name)
	if *updateWire {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-wire to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s changed:\n got %s\nwant %s", name, got, want)
	}
}

// Every field set, at fixed values: the order of every key, nested
// ones too, as a client reads them.
func TestWireStatusEveryField(t *testing.T) {
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	pin := json.RawMessage(`"` + strings.Repeat("ab", 32) + `"`)
	stamp := json.RawMessage(`"2026-11-13T09:30:00Z"`)
	s := statusSnapshot{
		App: "demo", Phase: "running", CurrentVersion: "v2",
		Socket: "/srv/motel/demo/run/n/app.sock", PID: 4242,
		Command: []string{"/srv/motel/demo/release/v2/bin/app", "serve"},
		Unit:    "app-demo-n.service", Running: true,
		Caps:           &capsSnapshot{MemoryMax: 256 << 20, TasksMax: 64, CPUQuota: 150},
		CapsUnenforced: []string{"cpu_quota"},
		Deploys: []deploySummary{{
			Version: "v2", Status: "failed", Phase: "starting", By: "local:/k.pub",
			SHA256: pin, StartedAt: stamp, FinishedAt: stamp,
		}},
		LastDeploy: &deployResult{
			Version: "v2", Status: "failed", Error: "boom", Phase: "starting",
			By: "local:/k.pub", SHA256: strings.Repeat("ab", 32),
			StartedAt: at, FinishedAt: at.Add(time.Second),
			Note:            "relaunched",
			ArtifactEntries: 3, ArtifactBytes: 1024,
			Phases: []phaseTiming{{Name: "starting", StartedAt: at, Seconds: 0.5}},
			Detail: &deployDetail{
				Exit:    "exit status 3",
				Probe:   &probeDetail{Status: 500, Location: "/login", Body: "no such table"},
				LogTail: []string{"line one"}, LogTailTruncated: true, LogTailError: "journal unreadable",
			},
			RedactedEnv: []string{"SECRET"},
		},
		Watchdog: &watchdogSnapshot{
			State: "backoff", Failures: 2, RestartsInWindow: 1,
			LastRestartAt: at, LastRestartCause: "unhealthy", LastFailure: "probe: 502",
		},
		AvailableVersions: []string{"v2", "v1"},
		OrderedByMtime:    []string{"v1"},
		Metrics:           &wire.Metrics{Window: "1h"},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	checkWireGolden(t, "status-every-field.json", string(raw)+"\n")
}

// wireMetrics is a metrics object with every field set, in the order
// the metrics view writes it (notify, TestMetricsObjectEveryField).
var wireMetrics = func() *wire.Metrics {
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	return &wire.Metrics{
		Window: "24h", At: &at,
		Memory:    &wire.MemorySummary{Now: f(30), Max: f(40)},
		MemoryMax: f(100),
		CPU:       &wire.CPUSummary{Avg5m: f(0.5), Avg: 0.25},
		Requests: &wire.RequestSummary{
			Total: 10, PerSecond: 0.000116, ByClass: map[string]uint64{"5xx": 1, "2xx": 9},
			Share5xx: f(0.1), P50: f(1.5), P95: f(20), CacheHitShare: f(0.5),
		},
		Error: "the metrics store did not answer",
	}
}()

// wireNoise is what varies from run to run in a handler's body: the
// times, the phases' durations, the rig's temp dirs. Replaced, not
// dropped, so every key keeps its place.
var wireNoise = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`"\d{4}-\d\d-\d\dT[0-9:.]+Z"`), `"<time>"`},
	{regexp.MustCompile(`"seconds":[0-9.e-]+`), `"seconds":<s>`},
	{regexp.MustCompile(`/run/[0-9a-f]{16}/`), `/run/<nonce>/`},
	{regexp.MustCompile(`local:/tmp/[^"]*/deploy\.pub`), `local:<key>`},
}

func wireBody(t *testing.T, rig *testRig, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := w.Body.String()
	for _, n := range wireNoise {
		body = n.re.ReplaceAllString(body, n.with)
	}
	return strings.ReplaceAll(body, rig.spec.dirs.root, "<root>")
}

// decodesAsWire holds a body to package wire's type for it: decoded
// with no field left over and marshalled again, it is the same bytes,
// so a client decoding into the type reads everything the handler
// sent.
func decodesAsWire(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	type (
		done struct {
			wire.Status
			wire.DoneEvent
		}
		doneFailed struct {
			wire.Failure
			wire.DoneEvent
		}
		doneRefused struct {
			wire.Error
			wire.DoneEvent
		}
	)
	check := func(line string, v any) {
		t.Helper()
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Fatalf("%s: %T does not decode %s: %v", what, v, line, err)
		}
		got, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != line {
			t.Errorf("%s: %T loses something:\n got %s\nwant %s", what, v, got, line)
		}
	}
	body := strings.TrimRight(w.Body.String(), "\n")
	switch {
	case w.Header().Get("Content-Type") == "application/x-ndjson":
		lines := strings.Split(body, "\n")
		for _, l := range lines[:len(lines)-1] {
			check(l, new(wire.PhaseEvent))
		}
		switch last := lines[len(lines)-1]; {
		case strings.Contains(last, `"status":{`):
			check(last, new(doneFailed))
		case strings.HasPrefix(last, `{"error":`):
			check(last, new(doneRefused))
		default:
			check(last, new(done))
		}
	case w.Code == http.StatusOK && strings.Contains(what, "?deploy="):
		check(body, new(wire.DeployResult))
	case w.Code == http.StatusOK:
		check(body, new(wire.Status))
	case strings.Contains(body, `"status":{`):
		check(body, new(wire.Failure))
	default:
		check(body, new(wire.Error))
	}
}

// The handler's own bodies, through the response filter: each
// refusal, the status before and after a deploy, the 500 that carries
// the status, a record, and a streamed deploy's lines.
func TestWireHandlerBodies(t *testing.T) {
	h, rig := newTestHandler(t)
	var out strings.Builder
	add := func(what string, w *httptest.ResponseRecorder) {
		t.Helper()
		out.WriteString("# " + what + "\n")
		out.WriteString(w.Header().Get("Content-Type") + " " + http.StatusText(w.Code) + "\n")
		out.WriteString(wireBody(t, rig, w))
		decodesAsWire(t, what, w)
	}
	add("GET, no token", do(t, h, http.MethodGet, "/demo", "", ""))
	add("GET an unknown app", do(t, h, http.MethodGet, "/nope", globalToken(t), ""))
	add("GET before any deploy", do(t, h, http.MethodGet, "/demo", appToken(t), ""))
	add("GET a range not offered", do(t, h, http.MethodGet, "/demo?range=7d", appToken(t), ""))
	add("GET a malformed query", do(t, h, http.MethodGet, "/demo?deploy=%ZZ", appToken(t), ""))
	add("PUT", do(t, h, http.MethodPut, "/demo", appToken(t), ""))
	add("POST a body that is not JSON", do(t, h, http.MethodPost, "/demo", appToken(t), "{"))
	add("POST v1", do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`))
	add("POST v1 again", do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`))
	add("GET after v1", do(t, h, http.MethodGet, "/demo", appToken(t), ""))
	add("GET ?deploy=v1", do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), ""))
	add("GET ?deploy=v9", do(t, h, http.MethodGet, "/demo?deploy=v9", appToken(t), ""))
	add("GET ?deploy=bad/version", do(t, h, http.MethodGet, "/demo?deploy=bad%2Fversion", appToken(t), ""))
	rig.runner.startErr = errors.New("boom")
	add("POST v2, which fails to start", do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`))
	rig.runner.startErr = nil

	stream := func(version string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(`{"url":"https://x/`+version+`.tgz","version":"`+version+`"}`))
		req.Header.Set("Authorization", "Bearer "+appToken(t))
		req.Header.Set("Accept", "application/x-ndjson")
		w := httptest.NewRecorder()
		var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, req, next); err != nil {
			t.Fatal(err)
		}
		return w
	}
	add("POST v3, streamed", stream("v3"))

	// The metrics object sits last, as the source gave it.
	h.app.SetMetricsSource(&fakeMetricsSource{reply: wireMetrics})
	add("GET with metrics, ?range=24h", do(t, h, http.MethodGet, "/demo?range=24h", appToken(t), ""))

	rig.runner.startErr = errors.New("boom")
	add("POST v4, streamed, which fails to start", stream("v4"))
	// An env_file that cannot be read withholds every body: each line
	// is the filter's diagnostic with the event's fields after it.
	rig.runner.startErr = nil
	rig.spec.envFile = filepath.Join(rig.spec.dirs.root, "missing.env")
	add("POST v5, streamed, the env_file unreadable", stream("v5"))
	rig.runner.startErr = nil
	rig.spec.envFile = ""

	checkWireGolden(t, "handler-bodies.txt", out.String())
}
