package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

func parseStore(src string) (*MetricsStore, error) {
	a := new(App)
	err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(src))
	return a.Metrics, err
}

func TestMetricsLine(t *testing.T) {
	m, err := parseStore("monitor {\n metrics\n}")
	if err != nil || m == nil || m.store() != DefaultStoreSocket || m.stateDir() != DefaultMetricsStateDir {
		t.Fatalf("the bare line: %+v, %v", m, err)
	}
	m, err = parseStore("monitor {\n metrics /run/x/vm.sock\n}")
	if err != nil || m == nil || m.store() != "/run/x/vm.sock" || m.stateDir() != DefaultMetricsStateDir {
		t.Fatalf("a store path: %+v, %v", m, err)
	}
	if m, err = parseStore("monitor {\n operator ops\n}"); err != nil || m != nil {
		t.Fatalf("no metrics line: %+v, %v; want none", m, err)
	}
	for src, want := range map[string]string{
		"monitor {\n metrics run/vm.sock\n}":       "not an absolute path",
		"monitor {\n metrics /a\n metrics /b\n}":   "metrics is given more than once",
		"monitor {\n metrics\n metrics\n}":         "metrics is given more than once",
		"monitor {\n metrics /a /b\n}":             "wrong argument count",
		"monitor {\n metrics /a {\n  x\n }\n}":     "take no block",
		"monitor {\n metrics {\n  store /a\n }\n}": "take no block",
		"monitor {\n metrics_store\n}":             `unknown monitor subdirective "metrics_store"`,
		"monitor {\n store /run/x/vm.sock\n}":      `unknown monitor subdirective "store"`,
		"monitor {\n interval 10s\n}":              `unknown monitor subdirective "interval"`,
	} {
		if _, err := parseStore(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", src, err, want)
		}
	}
}

type fakeSampler []liveswap.AppSample

func (f fakeSampler) AppSamples() []liveswap.AppSample { return f }

func gatherValues(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			k := mf.GetName()
			for _, l := range m.GetLabel() {
				k += "," + l.GetName() + "=" + l.GetValue()
			}
			out[k] = value(m)
		}
	}
	return out
}

func value(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	return m.GetCounter().GetValue()
}

func TestAppCollector(t *testing.T) {
	root := t.TempDir()
	cg := "/user.slice/app-blog.service"
	if err := os.MkdirAll(filepath.Join(root, cg), 0o750); err != nil {
		t.Fatal(err)
	}
	// blog holds 4 MiB of page cache the kernel would give back: its
	// working set is memory.current less inactive_file. nostat has no
	// memory.stat, and reads as memory.current.
	nostat := "/user.slice/app-nostat.service"
	if err := os.MkdirAll(filepath.Join(root, nostat), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		cg + "/memory.current":     "35651584\n",
		cg + "/memory.stat":        "anon 20971520\nfile 14680064\ninactive_anon 0\nactive_anon 20971520\ninactive_file 4194304\nactive_file 10485760\n",
		cg + "/cpu.stat":           "usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n",
		nostat + "/memory.current": "1048576\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := gatherValues(t, appCollector{root: root, apps: fakeSampler{
		{App: "blog", Release: "v2", Running: true, Cgroup: cg, MemoryMax: 268435456},
		{App: "shop", Release: "v1"}, // stopped: up 0, no numbers
		{App: "wiki"},                // never deployed
		{App: "evil", Release: "v1", Running: true, Cgroup: "/../../etc"},    // a path out of the root reads nothing
		{App: "nocg", Release: "v1", Running: true, Cgroup: "/nowhere/here"}, // unreadable: no number, no error
		{App: "nostat", Running: true, Cgroup: nostat},
	}})
	want := map[string]float64{
		"motel_app_up,app=blog": 1, "motel_app_up,app=shop": 0, "motel_app_up,app=wiki": 0,
		"motel_app_up,app=evil": 1, "motel_app_up,app=nocg": 1, "motel_app_up,app=nostat": 1,
		"motel_app_memory_bytes,app=nostat":          1048576,
		"motel_app_release_info,app=blog,release=v2": 1, "motel_app_release_info,app=shop,release=v1": 1,
		"motel_app_release_info,app=evil,release=v1": 1, "motel_app_release_info,app=nocg,release=v1": 1,
		"motel_app_memory_bytes,app=blog":                            31457280,
		"motel_app_memory_max_bytes,app=blog":                        268435456,
		"motel_app_cpu_seconds_total,app=blog,unit=app-blog.service": 2.5,
	}
	if len(got) != len(want) {
		t.Errorf("series = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// unixServer serves h on a unix socket in a test directory.
func unixServer(t *testing.T, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vm.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestStoreClient(t *testing.T) {
	var gotPath, gotBody string
	sock := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/import/prometheus":
			b, _ := io.ReadAll(r.Body)
			gotPath, gotBody = r.URL.RequestURI(), string(b)
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/query":
			if tm := r.URL.Query().Get("time"); tm != "" && (tm != "1762776000.250" || r.URL.Query().Get("latency_offset") != "5s") {
				http.Error(w, "bad time or offset", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"vmalert"},"value":[1762776000,"1"]}]}}`)
		case "/api/v1/query_range":
			if r.URL.Query().Get("step") != "60" || r.URL.Query().Get("start") == "" {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"m"},"values":[[1762776000,"1.5"],[1762776060.5,"2"]]}]}}`)
		default:
			http.Error(w, "unsupported path", http.StatusBadRequest)
		}
	}))
	c := newStoreClient(sock)
	ctx := context.Background()
	if err := c.importText(ctx, []byte("x 1\n")); err != nil {
		t.Fatal(err)
	}
	if gotPath != importPath || gotBody != "x 1\n" {
		t.Fatalf("import: %s %q", gotPath, gotBody)
	}
	v, err := c.query(ctx, rulesQuery)
	if err != nil || len(v) != 1 || v[0].labels["job"] != "vmalert" || v[0].points[0].v != 1 {
		t.Fatalf("query: %+v %v", v, err)
	}
	// To the millisecond: a second's truncation moved the old release's
	// window off the push after the cutover.
	if v, err := c.queryAt(ctx, rulesQuery, time.Unix(1762776000, 250_400_000)); err != nil || len(v) != 1 {
		t.Fatalf("queryAt: %+v %v", v, err)
	}
	if _, err := c.queryAt(ctx, rulesQuery, time.Unix(1762776000, 0)); err == nil {
		t.Fatal("queryAt sent another time")
	}
	now := time.Unix(1762776120, 0)
	m, err := c.queryRange(ctx, "m", now.Add(-time.Hour), now, time.Minute)
	if err != nil || len(m) != 1 || len(m[0].points) != 2 || m[0].points[1].v != 2 || m[0].points[1].t.UnixMilli() != 1762776060500 {
		t.Fatalf("query_range: %+v %v", m, err)
	}
	if _, err := c.do(ctx, http.MethodGet, "/nope", nil, ""); err == nil || !strings.Contains(err.Error(), "answered 400") {
		t.Fatalf("a refusal: %v", err)
	}
	if _, err := newStoreClient(filepath.Join(t.TempDir(), "absent.sock")).query(ctx, "x"); err == nil {
		t.Fatal("no socket, no error")
	}
}

func TestParseQueryAnswerRefuses(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"status":"error","error":"bad query"}`,
		`{"status":"success","data":{"resultType":"scalar","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,2]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"x"]}]}}`,
		// json.Marshal refuses both, and the status would fail with them.
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"NaN"]]}]}}`,
		`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"+Inf"]]}]}}`,
	} {
		if _, err := parseQueryAnswer([]byte(body)); err == nil {
			t.Errorf("%s: accepted", body)
		}
	}
}

// fakeStore is the store, scripted.
type fakeStore struct {
	mu        sync.Mutex
	importErr error
	imports   []string
	up        []series
	queryErr  error
	ro        bool
	roErr     error
}

func (f *fakeStore) readOnly(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ro, f.roErr
}

func (f *fakeStore) importText(_ context.Context, text []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imports = append(f.imports, string(text))
	return f.importErr
}

func (f *fakeStore) query(context.Context, string) ([]series, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up, f.queryErr
}

type testPusher struct {
	*pusher
	at       time.Time
	reported []Event
	logs     *observer.ObservedLogs
	store    *fakeStore
	m        *MetricsStore
}

func newTestPusher(t *testing.T) *testPusher {
	t.Helper()
	tp := &testPusher{at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), store: &fakeStore{}, m: &MetricsStore{}}
	tp.pusher = newPusher(func(ev Event) { tp.reported = append(tp.reported, ev) })
	tp.now = func() time.Time { return tp.at }
	core, logs := observer.New(zap.DebugLevel)
	tp.logs = logs
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "motel_test_gauge", Help: "a gauge"}))
	// No goroutine: the test calls pushOnce and checkRules itself.
	tp.setLive(liveStore{m: tp.m, store: tp.store, gather: reg, logger: zap.New(core)})
	return tp
}

func (tp *testPusher) take() []Event {
	out := tp.reported
	tp.reported = nil
	return out
}

func TestPushSendsTheRegistry(t *testing.T) {
	tp := newTestPusher(t)
	tp.pushOnce(context.Background())
	if len(tp.store.imports) != 1 || !strings.Contains(tp.store.imports[0], "# TYPE motel_test_gauge gauge\nmotel_test_gauge 0\n") {
		t.Fatalf("imports = %q", tp.store.imports)
	}
}

func TestPushFailingFiresStoreDown(t *testing.T) {
	tp := newTestPusher(t)
	tp.store.importErr = errors.New("connect: no such file or directory")
	ctx := context.Background()
	for i := 0; i < 10; i++ { // 4.5 minutes of failures
		tp.pushOnce(ctx)
		tp.at = tp.at.Add(pushEvery)
	}
	if evs := tp.take(); len(evs) != 0 {
		t.Fatalf("before five minutes: %+v", evs)
	}
	if n := len(tp.logs.FilterMessage("monitor metrics: push failed").FilterLevelExact(zap.WarnLevel).All()); n != 1 {
		t.Fatalf("%d warning lines for one outage, want 1", n)
	}
	tp.pushOnce(ctx) // 5 minutes
	evs := tp.take()
	if len(evs) != 1 || evs[0].Name != eventStoreDown || !evs[0].Firing || evs[0].Kind != kindState || evs[0].Source != sourceMetrics {
		t.Fatalf("at five minutes: %+v", evs)
	}
	if got := counterValue(t, tp.failed); got != 11 {
		t.Fatalf("motel_metrics_push_failed_total = %v, want 11", got)
	}
	tp.store.importErr = nil
	tp.pushOnce(ctx)
	evs = tp.take()
	if len(evs) != 1 || evs[0].Name != eventStoreDown || evs[0].Firing {
		t.Fatalf("after a success: %+v", evs)
	}
	if len(tp.logs.FilterMessage("monitor metrics: push succeeded again").All()) != 1 {
		t.Fatal("no recovery line")
	}
	tp.pushOnce(ctx)
	if evs := tp.take(); len(evs) != 0 {
		t.Fatalf("a second success reported %+v", evs)
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestRulesDownAfterThreeReads(t *testing.T) {
	tp := newTestPusher(t)
	ctx := context.Background()
	up := func(v float64) []series {
		return []series{{points: []point{{t: tp.at, v: v}}}}
	}
	for _, res := range [][]series{nil, up(0)} {
		tp.store.up = res
		if tp.checkRules(ctx); len(tp.take()) != 0 {
			t.Fatal("fired before three reads")
		}
	}
	// A read the store does not answer is no reading: it neither counts
	// nor resets.
	tp.store.queryErr = errors.New("refused")
	tp.checkRules(ctx)
	tp.store.queryErr = nil
	if len(tp.take()) != 0 {
		t.Fatal("a failed read counted")
	}
	tp.checkRules(ctx)
	evs := tp.take()
	if len(evs) != 1 || evs[0].Name != eventRulesDown || !evs[0].Firing {
		t.Fatalf("the third read: %+v", evs)
	}
	tp.store.up = up(1)
	tp.checkRules(ctx)
	evs = tp.take()
	if len(evs) != 1 || evs[0].Name != eventRulesDown || evs[0].Firing {
		t.Fatalf("vmalert up again: %+v", evs)
	}
}

// A read-only store refuses its own scrapes, so no rule can see the
// flag: motel reads it from the store's /metrics and says so itself,
// once, as the critical that names the cause. What follows from it is
// not told as something else: no metrics_rules_down from a store that
// keeps no scrape of vmalert, no metrics_store_down for its refused
// imports.
func TestReadOnlyStoreIsToldAsItself(t *testing.T) {
	tp := newTestPusher(t)
	ctx := context.Background()
	tp.store.ro = true
	tp.store.importErr = errors.New("503: the storage is in read-only mode")
	// Told at every read that finds it, not once: the engine drops what
	// it is told while no config runs notify.
	for range 2 * rulesDownReads {
		tp.checkChain(ctx)
		evs := tp.take()
		if len(evs) != 1 || evs[0].Name != eventStoreReadOnly || !evs[0].Firing || evs[0].Severity != SeverityCritical || evs[0].Kind != kindState {
			t.Fatalf("events = %+v", evs)
		}
	}
	tp.pushOnce(ctx)
	tp.at = tp.at.Add(storeDownAfter)
	tp.pushOnce(ctx)
	if evs := tp.take(); len(evs) != 0 {
		t.Fatalf("a read-only store was told as something else: %+v", evs)
	}
	// One ask the store does not answer (a store at its floor is slow)
	// is no reading: what was told stands, and so does the hold, for the
	// pushes and for the count toward metrics_rules_down.
	tp.store.roErr = errors.New("timeout")
	tp.checkChain(ctx)
	tp.pushOnce(ctx)
	tp.store.roErr = nil
	tp.checkChain(ctx)
	for _, ev := range tp.take() {
		if ev.Name != eventStoreReadOnly {
			t.Fatalf("one unanswered ask let %s through", ev.Name)
		}
	}
	// readOnlyStale in a row: the store may be down, and
	// metrics_store_down says that. One line says the ask fails.
	tp.store.roErr = errors.New("refused")
	for range readOnlyStale {
		tp.checkChain(ctx)
	}
	if n := tp.logs.FilterMessage("monitor metrics: could not read whether the store is read-only").Len(); n != 2 {
		t.Fatalf("%d lines for two spells of failing asks, want 2", n)
	}
	tp.pushOnce(ctx)
	evs := tp.take()
	if len(evs) != 1 || evs[0].Name != eventStoreDown || !evs[0].Firing {
		t.Fatalf("a store that stopped answering while read-only: %+v", evs)
	}
	tp.store.roErr, tp.store.ro, tp.store.importErr = nil, false, nil
	tp.store.up = []series{{points: []point{{t: tp.at, v: 1}}}}
	tp.checkChain(ctx)
	tp.pushOnce(ctx)
	evs = tp.take()
	if len(evs) != 2 || evs[0].Name != eventStoreReadOnly || evs[0].Firing || evs[1].Name != eventStoreDown || evs[1].Firing {
		t.Fatalf("writable again: %+v", evs)
	}
	// Stopped while read-only: cleared, since nothing else can.
	tp.store.ro = true
	tp.checkChain(ctx)
	tp.take()
	tp.release(tp.m)
	cleared := false
	for _, ev := range tp.take() {
		cleared = cleared || (ev.Name == eventStoreReadOnly && !ev.Firing)
	}
	if !cleared {
		t.Fatal("released while read-only: not cleared")
	}
}

func TestParseReadOnly(t *testing.T) {
	for page, want := range map[string]bool{
		"vm_rows 3\nvm_storage_is_read_only 0\n":                              false,
		"vm_storage_is_read_only 1\nvm_rows 3\n":                              true,
		`vm_storage_is_read_only{path="/var/lib/victoria-metrics"} 1` + "\n":  true,
		`vm_storage_is_read_only{path="/var/lib/a b"} 0 1731300000000` + "\n": false, // a timestamp is not the value
		"vm_storage_is_read_only\t1\n":                                        true,
	} {
		if got, err := parseReadOnly([]byte(page)); err != nil || got != want {
			t.Errorf("%q: %v, %v", page, got, err)
		}
	}
	for _, page := range []string{"", "vm_rows 3\n", "vm_storage_is_read_only_total 1\n", "vm_storage_is_read_only x\n",
		"vm_storage_is_read_only \n", `vm_storage_is_read_only{path="x" 1` + "\n"} {
		if _, err := parseReadOnly([]byte(page)); err == nil {
			t.Errorf("%q: read as an answer", page)
		}
	}
}

// A reload that moves the `store` socket: the store before's failures
// are not the new one's history. Nothing fires on them, and what was
// firing is cleared by the new store's own answer.
func TestPusherStartsAfreshOnAnotherStore(t *testing.T) {
	tp := newTestPusher(t)
	ctx := context.Background()
	tp.store.importErr = errors.New("connect: no such file or directory")
	tp.pushOnce(ctx)
	tp.checkRules(ctx)
	tp.checkRules(ctx) // two reads without vmalert, 4.5 minutes of failed pushes
	tp.at = tp.at.Add(storeDownAfter - pushEvery)
	moved := &MetricsStore{Store: "/run/elsewhere/vm.sock"}
	tp.setLive(liveStore{m: moved, store: tp.store, gather: prometheus.NewRegistry(), logger: zap.NewNop()})
	tp.at = tp.at.Add(pushEvery)
	tp.pushOnce(ctx)
	tp.checkRules(ctx)
	if evs := tp.take(); len(evs) != 0 {
		t.Fatalf("the store before's history fired on the new one: %+v", evs)
	}
	// The same socket (a reload that changed nothing of the store) keeps
	// its history: the outage is the same outage.
	same := &MetricsStore{Store: "/run/elsewhere/vm.sock"}
	tp.setLive(liveStore{m: same, store: tp.store, gather: prometheus.NewRegistry(), logger: zap.NewNop()})
	tp.at = tp.at.Add(storeDownAfter)
	tp.pushOnce(ctx)
	evs := tp.take()
	if len(evs) != 1 || evs[0].Name != eventStoreDown || !evs[0].Firing {
		t.Fatalf("the same store's outage: %+v", evs)
	}
}

func TestPusherReleaseClearsWhatFires(t *testing.T) {
	tp := newTestPusher(t)
	tp.storeDown, tp.rulesDown = true, true
	tp.release(tp.m)
	evs := tp.take()
	if len(evs) != 2 || evs[0].Firing || evs[1].Firing {
		t.Fatalf("release reported %+v", evs)
	}
	tp.pushOnce(context.Background())
	if len(tp.store.imports) != 0 {
		t.Fatal("pushed with no config")
	}
}

func TestPusherGoesBackToTheOlderConfig(t *testing.T) {
	tp := newTestPusher(t)
	dir := t.TempDir()
	tp.m.stateDirOverride, tp.m.rules, tp.m.logger = dir, []byte("older\n"), zap.NewNop()
	tp.live = append(tp.live, liveStore{m: tp.m, store: tp.store, gather: prometheus.NewRegistry(), logger: zap.NewNop()})
	newer := &MetricsStore{stateDirOverride: dir, rules: []byte("newer\n"), logger: zap.NewNop()}
	newer.applyRules() // its Start
	tp.configure(liveStore{m: newer, store: tp.store, gather: prometheus.NewRegistry(), logger: zap.NewNop()})
	if tp.active != newer {
		t.Fatal("configure did not take the newer config")
	}
	tp.release(newer) // its reload failed after Start: the older serves on
	if tp.active != tp.m || !tp.running {
		t.Fatalf("active = %p, running %v; want the older config, running", tp.active, tp.running)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, RulesName)); string(b) != "older\n" {
		t.Fatalf("vmalert still reads the stopped config's rules: %q", b)
	}
	tp.release(tp.m)
	if tp.active != nil || tp.running {
		t.Fatal("still running with no config")
	}
	if tp.gather != nil || tp.pusher.store != nil {
		t.Fatal("the pusher holds a stopped config's registry or store")
	}
}

// A reload to a config with no metrics line (the monitor block to
// channels alone, or to none) retires the stopped config's rules;
// motel's own shutdown keeps them (Copilot round 3 on #423).
func TestReleaseRetiresTheRulesOnAReloadAlone(t *testing.T) {
	for _, reload := range []bool{true, false} {
		tp := newTestPusher(t)
		dir := t.TempDir()
		core, logs := observer.New(zap.InfoLevel)
		tp.m.stateDirOverride, tp.m.rules, tp.m.logger = dir, renderRules(notifyApp(t)), zap.New(core)
		tp.m.applyRules() // its Start
		was := configRunning
		configRunning = func() bool { return reload }
		tp.release(tp.m)
		configRunning = was
		b, err := os.ReadFile(filepath.Join(dir, RulesName))
		if err != nil {
			t.Fatal(err)
		}
		alerting := strings.Contains(string(b), "alert:")
		gone := logs.FilterMessage("monitor metrics: the metrics line is gone: vmalert is given no rule; the store and vmalert, which motel monitor setup --metrics started, run until their units are stopped").Len()
		switch {
		case reload && (alerting || gone != 1):
			t.Errorf("a reload took the metrics line away: rules.yml has alerting rules %v, %d journal lines; want none, and one", alerting, gone)
		case !reload && (!alerting || gone != 0):
			t.Errorf("motel's shutdown: rules.yml has alerting rules %v, %d journal lines; want them kept, and no line", alerting, gone)
		}
		if reload {
			if f := parseRules(t, b); len(f.Groups) != 3 {
				t.Errorf("retired rules: %d groups, want the three, empty (vmalert refuses a file with none)", len(f.Groups))
			}
		}
	}
}

// fakeRange is query_range and the instant queries, scripted per
// query: instant answers the memory peak (max_over_time), requests the
// four request queries.
type fakeRange struct {
	mu       sync.Mutex
	calls    int
	err      error
	block    chan struct{}
	ans      func(q string) []series
	instant  func(q string) []series
	requests func(q string) []series
}

func (f *fakeRange) query(ctx context.Context, q string) ([]series, error) {
	script := f.requests
	if strings.HasPrefix(q, "max_over_time(") {
		script = f.instant
	}
	if script == nil {
		return nil, f.err // not counted: calls is query_range's
	}
	// The view asks its queries at once: the scripts run one at a time.
	f.mu.Lock()
	defer f.mu.Unlock()
	return script(q), nil
}

func (f *fakeRange) queryRange(ctx context.Context, q string, _, _ time.Time, _ time.Duration) ([]series, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ans(q), nil
}

func TestMetricsViewSummary(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	pts := func(vals ...float64) []point {
		out := make([]point, len(vals))
		for i, v := range vals {
			out[i] = point{t: end.Add(time.Duration(i-len(vals)+1) * time.Minute), v: v}
		}
		return out
	}
	var asked []string
	f := &fakeRange{ans: func(q string) []series {
		asked = append(asked, q)
		if strings.HasPrefix(q, "sum(rate(") {
			return []series{{labels: map[string]string{"app": "blog"}, points: pts(0.25, 0.25, 0.25, 0.5, 0.5, 0.5, 0.5, 0.5)}}
		}
		return []series{
			{labels: map[string]string{"__name__": "motel_app_memory_bytes"}, points: pts(10, 40, 30)},
			{labels: map[string]string{"__name__": "motel_app_memory_max_bytes"}, points: pts(100, 100, 100)},
		}
	}, instant: func(q string) []series {
		asked = append(asked, q)
		return []series{{points: []point{{t: end, v: 40}}}}
	}}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T12:00:00Z","memory_bytes":{"now":30,"max":40},"memory_max_bytes":100,"cpu_cores":{"avg_5m":0.5,"avg":0.40625}}`
	if string(got) != want {
		t.Fatalf("metrics:\n got %s\nwant %s", got, want)
	}
	for _, q := range asked {
		if !strings.Contains(q, `app="blog"`) {
			t.Errorf("a query not scoped to the app: %s", q)
		}
	}
	// Cached: the second ask within 30 s queries nothing.
	v.AppMetrics(context.Background(), "blog", "1h")
	if f.calls != 2 {
		t.Fatalf("%d queries for two asks inside the cache", f.calls)
	}
	v.now = func() time.Time { return end.Add(viewCache) }
	v.AppMetrics(context.Background(), "blog", "1h")
	if f.calls != 4 {
		t.Fatalf("%d queries after the cache expired, want 4", f.calls)
	}
}

// An app that stopped 20 minutes ago, or a cap a later release dropped,
// keeps its last points in the hour: history, never now.
func TestMetricsViewStaleIsHistory(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	old := func(v float64) []point {
		return []point{{t: end.Add(-25 * time.Minute), v: v}, {t: end.Add(-20 * time.Minute), v: v}}
	}
	f := &fakeRange{ans: func(q string) []series {
		if strings.HasPrefix(q, "sum(rate(") {
			return []series{{points: old(0.5)}}
		}
		return []series{
			{labels: map[string]string{"__name__": "motel_app_memory_bytes"}, points: old(30)},
			{labels: map[string]string{"__name__": "motel_app_memory_max_bytes"}, points: old(100)},
		}
	}, instant: func(string) []series { return []series{{points: []point{{t: end, v: 30}}}} }}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T11:40:00Z","memory_bytes":{"max":30},"cpu_cores":{"avg":0.5}}`
	if string(got) != want {
		t.Fatalf("stale series:\n got %s\nwant %s", got, want)
	}
}

// The day's range steps 5 m, so its newest point can be over 5 m old
// for a running app: still now, and avg_5m is that point alone (a 5 m
// rate already). Twenty minutes is history, for the day too.
func TestMetricsViewDayFreshness(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		newest time.Duration
		want   string
	}{
		{"running", 5*time.Minute + 20*time.Second,
			`{"window":"24h","at":"2026-11-10T11:54:40Z","memory_bytes":{"now":30,"max":40},"memory_max_bytes":100,"cpu_cores":{"avg_5m":0.5,"avg":0.375}}`},
		{"stopped", 20 * time.Minute,
			`{"window":"24h","at":"2026-11-10T11:40:00Z","memory_bytes":{"max":40},"cpu_cores":{"avg":0.375}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			at := end.Add(-c.newest)
			two := func(old, last float64) []point {
				return []point{{t: at.Add(-5 * time.Minute), v: old}, {t: at, v: last}}
			}
			f := &fakeRange{ans: func(q string) []series {
				if strings.HasPrefix(q, "sum(rate(") {
					return []series{{points: two(0.25, 0.5)}}
				}
				return []series{
					{labels: map[string]string{"__name__": "motel_app_memory_bytes"}, points: two(40, 30)},
					{labels: map[string]string{"__name__": "motel_app_memory_max_bytes"}, points: two(100, 100)},
				}
			}, instant: func(string) []series { return []series{{points: []point{{t: end, v: 40}}}} }}
			v := newMetricsView(f, zap.NewNop())
			v.now = func() time.Time { return end }
			got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "24h"))
			if string(got) != c.want {
				t.Fatalf("the day:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// The hour's max is the store's max_over_time over the raw samples
// (pushed every 30 s), not the max of a 60 s-step range, which holds one
// sample a step: a peak between two steps is the max.
func TestMetricsViewMaxKeepsAPeakBetweenSteps(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := &fakeRange{
		ans: func(q string) []series {
			if strings.HasPrefix(q, "sum(rate(") {
				return nil
			}
			return []series{{labels: map[string]string{"__name__": "motel_app_memory_bytes"},
				points: []point{{t: end.Add(-time.Minute), v: 10}, {t: end, v: 10}}}}
		},
		instant: func(q string) []series {
			if q != `max_over_time(motel_app_memory_bytes{app="blog"}[1h])` {
				t.Errorf("instant query %s", q)
			}
			return []series{{points: []point{{t: end, v: 1000}}}}
		},
	}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T12:00:00Z","memory_bytes":{"now":10,"max":1000}}`
	if string(got) != want {
		t.Fatalf("a peak between steps:\n got %s\nwant %s", got, want)
	}
}

// The range's first step can carry a sample from before the hour; the
// raw peak leaves it out, and so does the max.
func TestMetricsViewMaxLeavesOutASampleBeforeTheHour(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := &fakeRange{
		ans: func(q string) []series {
			if strings.HasPrefix(q, "sum(rate(") {
				return nil
			}
			return []series{{labels: map[string]string{"__name__": "motel_app_memory_bytes"},
				points: []point{{t: end.Add(-time.Hour), v: 1000}, {t: end, v: 10}}}}
		},
		instant: func(string) []series { return []series{{points: []point{{t: end, v: 10}}}} },
	}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T12:00:00Z","memory_bytes":{"now":10,"max":10}}`
	if string(got) != want {
		t.Fatalf("a sample before the hour:\n got %s\nwant %s", got, want)
	}
}

// An app that stopped just over an hour ago: the range's first step
// still carries its last sample, and the raw peak holds nothing. The max
// is absent, never 0.
func TestMetricsViewNoPeakIsNoMax(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := &fakeRange{
		ans: func(q string) []series {
			if strings.HasPrefix(q, "sum(rate(") {
				return nil
			}
			return []series{{labels: map[string]string{"__name__": "motel_app_memory_bytes"},
				points: []point{{t: end.Add(-time.Hour), v: 1000}}}}
		},
		instant: func(string) []series { return nil },
	}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T11:00:00Z","memory_bytes":{}}`
	if string(got) != want {
		t.Fatalf("no raw sample in the hour:\n got %s\nwant %s", got, want)
	}
}

// An idle app's CPU averages are tiny floats; unrounded, each marshals
// as a run of twenty or more digits, which the response filter masks as
// a generated secret, and the whole status is withheld.
func TestMetricsViewCPUIsRounded(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	f := &fakeRange{ans: func(q string) []series {
		if !strings.HasPrefix(q, "sum(rate(") {
			return nil
		}
		return []series{{points: []point{
			{t: end.Add(-10 * time.Minute), v: 0.00005740305648246404},
			{t: end.Add(-time.Minute), v: 0.00001234567890123456},
			{t: end, v: 0.0000000912345678901234},
		}}}
	}}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	want := `{"window":"1h","at":"2026-11-10T12:00:00Z","cpu_cores":{"avg_5m":0.000006,"avg":0.000023}}`
	if string(got) != want {
		t.Fatalf("tiny averages:\n got %s\nwant %s", got, want)
	}
	if regexp.MustCompile(`[A-Za-z0-9+=_-]{20,}`).Match(got) {
		t.Fatalf("a run the response filter's entropy layer would read: %s", got)
	}
}

func TestMetricsViewNoData(t *testing.T) {
	v := newMetricsView(&fakeRange{ans: func(string) []series { return nil }}, zap.NewNop())
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	if string(got) != `{"window":"1h"}` {
		t.Fatalf("no data: %s", got)
	}
}

func TestMetricsViewStoreFailing(t *testing.T) {
	f := &fakeRange{err: errors.New("dial unix /run/motel-metrics/vm.sock: connect: connection refused")}
	v := newMetricsView(f, zap.NewNop())
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	if string(got) != `{"error":"the metrics store did not answer"}` {
		t.Fatalf("a failing store: %s", got)
	}
}

func TestMetricsViewBudgetAndOneQueryInFlight(t *testing.T) {
	f := &fakeRange{block: make(chan struct{}), ans: func(string) []series { return nil }}
	v := newMetricsView(f, zap.NewNop())
	start := time.Now()
	var wg sync.WaitGroup
	answers := make([]*wire.Metrics, 3)
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = v.AppMetrics(context.Background(), "blog", "1h")
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > viewBudget+time.Second {
		t.Fatalf("a store that never answers held the route %v", d)
	}
	for _, a := range answers {
		if a.Error != errViewStore {
			t.Fatalf("answers = %+v", answers)
		}
	}
	// One ask in flight for the three: its two range queries, at once.
	if f.calls != 2 {
		t.Fatalf("%d range queries for three concurrent asks, want 2", f.calls)
	}
	// A request that gives up before the store answers gets the error at
	// once.
	v2 := newMetricsView(&fakeRange{block: make(chan struct{}), ans: func(string) []series { return nil }}, zap.NewNop())
	go v2.AppMetrics(context.Background(), "blog", "1h")
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a := v2.AppMetrics(ctx, "blog", "1h"); a.Error != errViewStore {
		t.Fatalf("a cancelled request: %+v", a)
	}
}

func TestWriteRules(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeRules(filepath.Join(dir, "absent"), []byte("x")); !errors.Is(err, errMetricsNotSetUp) {
		t.Fatalf("no state dir: %v", err)
	}
	changed, err := writeRules(dir, renderRules(nil))
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, err = writeRules(dir, renderRules(nil)); err != nil || changed {
		t.Fatalf("the same rules again: %v %v", changed, err)
	}
	st, err := os.Stat(filepath.Join(dir, RulesName))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("rules.yml: %v %v", st, err)
	}
}

// TestLatencyBoundsAreTheBuckets: a p50 or p95 rule's bound is told
// apart by the histogram only within its buckets: the lowest bound a
// rule takes is a bucket's edge, at or above the first, so it is not
// inside one that reads every response under it as half of it; the
// highest is the last bucket.
func TestLatencyBoundsAreTheBuckets(t *testing.T) {
	b := liveswap.ResponseBuckets
	if !slices.Contains(b[:], minLatency.Seconds()) {
		t.Errorf("the least bound a rule takes (%v) is no bucket's edge: %v", minLatency, b)
	}
	if first := time.Duration(b[0] * float64(time.Second)); first > minLatency {
		t.Errorf("the first bucket is %v, above the least bound a rule takes (%v)", first, minLatency)
	}
	if last := time.Duration(b[len(b)-1] * float64(time.Second)); last != maxLatency {
		t.Errorf("the last bucket is %v, the greatest bound %v", last, maxLatency)
	}
}

// observe's counts reach the registry per release, a stopped
// app's among them; a release with no response pushes nothing, but the
// current release of an observed app, at zero. An app the running
// config no longer observes pushes nothing of what it counted: unchanged
// counters would read as an app with no request. Every observed app,
// one with no release or no counts yet among them, is
// motel_app_observed.
func TestAppCollectorRequests(t *testing.T) {
	var cur liveswap.ReleaseRequests
	cur.Release = "v2"
	cur.ByClass[1], cur.ByClass[4] = 90, 10 // 2xx, 5xx
	cur.Buckets[0], cur.Buckets[len(cur.Buckets)-1] = 40, 95
	cur.Count, cur.SumSeconds = 100, 4.5
	cur.Cache[0], cur.Cache[2] = 30, 60 // hit, miss
	empty := liveswap.ReleaseRequests{Release: "v1"}
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(appCollector{root: t.TempDir(), apps: fakeSampler{
		{App: "blog", Release: "v2", Observed: true, Requests: []liveswap.ReleaseRequests{cur, empty}},
		{App: "gone", Release: "g2", Requests: []liveswap.ReleaseRequests{cur}},
		{App: "shop", Release: "s2", Observed: true, Requests: []liveswap.ReleaseRequests{{Release: "s2"}, {Release: "s1"}}},
		{App: "idle", Release: "i1", Requests: []liveswap.ReleaseRequests{{Release: "i1"}}},
		{App: "new", Observed: true, Requests: []liveswap.ReleaseRequests{{}}},
		{App: "booting", Observed: true}, // a restart's first push, before liveswap reattaches its release
	}}); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	text := b.String()
	for _, line := range []string{
		`motel_app_requests_total{app="blog",class="2xx",release="v2"} 90`,
		`motel_app_requests_total{app="blog",class="5xx",release="v2"} 10`,
		`motel_app_requests_total{app="blog",class="aborted",release="v2"} 0`,
		`motel_app_response_seconds_bucket{app="blog",release="v2",le="0.0005"} 40`,
		`motel_app_response_seconds_bucket{app="blog",release="v2",le="10"} 95`,
		`motel_app_response_seconds_bucket{app="blog",release="v2",le="+Inf"} 100`,
		`motel_app_response_seconds_sum{app="blog",release="v2"} 4.5`,
		`motel_app_response_seconds_count{app="blog",release="v2"} 100`,
		`motel_app_cache_responses_total{app="blog",release="v2",result="hit"} 30`,
		`motel_app_cache_responses_total{app="blog",release="v2",result="miss"} 60`,
		`motel_app_requests_total{app="shop",class="2xx",release="s2"} 0`,
		`motel_app_response_seconds_count{app="shop",release="s2"} 0`,
		`motel_app_observed{app="blog"} 1`, `motel_app_observed{app="shop"} 1`, `motel_app_observed{app="new"} 1`, `motel_app_observed{app="booting"} 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("missing %s", line)
		}
	}
	for _, absent := range []string{`release="v1"`, `result="stale"`, `release="s1"`, `app="idle",class`, `motel_app_response_seconds_count{app="idle"`, `release=""`, `motel_app_requests_total{app="gone"`, `motel_app_response_seconds_count{app="gone"`,
		`motel_app_observed{app="gone"`, `motel_app_observed{app="idle"`} {
		if strings.Contains(text, absent) {
			t.Errorf("%s pushed: %s", absent, text)
		}
	}
}

func TestMetricsViewRequests(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	one := func(labels map[string]string, v float64) series {
		return series{labels: labels, points: []point{{t: end, v: v}}}
	}
	var asked []string
	f := &fakeRange{ans: func(string) []series { return nil }, requests: func(q string) []series {
		asked = append(asked, q)
		switch {
		case strings.HasPrefix(q, "sum by (class)"):
			return []series{
				one(map[string]string{"class": "2xx"}, 2990.0000001),
				one(map[string]string{"class": "5xx"}, 7),
				one(map[string]string{"class": "aborted"}, 3),
				one(map[string]string{"class": "4xx"}, 0),
			}
		case strings.HasPrefix(q, "histogram_quantile(0.5,"):
			return []series{one(nil, 0.0123456789)}
		case strings.HasPrefix(q, "histogram_quantile(0.95,"):
			return []series{one(nil, 0.2468)}
		case strings.HasPrefix(q, "sum by (result)"):
			return []series{
				one(map[string]string{"result": "hit"}, 1000),
				one(map[string]string{"result": "stale"}, 1),
				one(map[string]string{"result": "miss"}, 2000),
			}
		}
		t.Errorf("unexpected instant query %s", q)
		return nil
	}}
	v := newMetricsView(f, zap.NewNop())
	v.now = func() time.Time { return end }
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "24h"))
	want := `{"window":"24h","requests":{"total":3000,"per_second":0.034722,"by_class":{"2xx":2990,"5xx":7,"aborted":3},` +
		`"5xx_share":0.0023,"p50_ms":12.3,"p95_ms":246.8,"cache_hit_share":0.3336}}`
	if string(got) != want {
		t.Fatalf("requests:\n got %s\nwant %s", got, want)
	}
	if len(asked) != 4 {
		t.Fatalf("%d request queries, want 4: %v", len(asked), asked)
	}
	// Each through the operator rules' guard, over the window: a
	// series new to the store since the push's first sample counts from
	// zero, one that came with it or back after a gap does not.
	day := func(sel string) string { return counterIncrease(sel, 24*time.Hour, 0) }
	dayHist := histogramIncrease("motel_app_response_seconds", `{app="blog"}`, 24*time.Hour, 0)
	if want := []string{
		`sum by (class) (` + day(`motel_app_requests_total{app="blog"}`) + `)`,
		`histogram_quantile(0.5, sum by (le) (` + dayHist + `))`,
		`histogram_quantile(0.95, sum by (le) (` + dayHist + `))`,
		`sum by (result) (` + day(`motel_app_cache_responses_total{app="blog"}`) + `)`,
	}; !slices.Equal(slices.Sorted(slices.Values(asked)), slices.Sorted(slices.Values(want))) {
		t.Errorf("the day's request queries =\n%s\nwant\n%s", strings.Join(asked, "\n"), strings.Join(want, "\n"))
	}
	if regexp.MustCompile(`[A-Za-z0-9+=_-]{20,}`).Match(got) {
		t.Fatalf("a run the response filter's entropy layer would read: %s", got)
	}
	// The windows are cached apart.
	if f.calls != 2 {
		t.Fatalf("%d range queries", f.calls)
	}
	asked = nil
	v.AppMetrics(context.Background(), "blog", "1h")
	if f.calls != 4 {
		t.Fatalf("%d range queries after asking the hour too, want 4", f.calls)
	}
	// The hour's, whole.
	hour := `((increase_pure(%[1]s{app="blog"}[1h]) unless%[3]s (present_over_time(%[2]s{app="blog"}[1d] offset 1h)` +
		` unless (last_over_time(%[2]s{app="blog"}[1d] offset 1h) > first_over_time(%[2]s{app="blog"}[1h]))))` +
		` and%[3]s (tfirst_over_time(%[2]s{app="blog"}[1h]) > scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[62m]))) + 15))` +
		` or increase(%[1]s{app="blog"}[1h])`
	reqs := fmt.Sprintf(hour, "motel_app_requests_total", "motel_app_requests_total", "")
	hist := fmt.Sprintf(hour, "motel_app_response_seconds_bucket", "motel_app_response_seconds_count", " ignoring(le)")
	if want := []string{
		`sum by (class) (` + reqs + `)`,
		`histogram_quantile(0.5, sum by (le) (` + hist + `))`,
		`histogram_quantile(0.95, sum by (le) (` + hist + `))`,
		`sum by (result) (` + fmt.Sprintf(hour, "motel_app_cache_responses_total", "motel_app_cache_responses_total", "") + `)`,
	}; !slices.Equal(slices.Sorted(slices.Values(asked)), slices.Sorted(slices.Values(want))) {
		t.Errorf("the hour's request queries =\n%s\nwant\n%s", strings.Join(asked, "\n"), strings.Join(want, "\n"))
	}
}

// No observer on the app's route: no request series, no `requests`;
// NaN quantiles (no response in the window) are absent, not NaN.
func TestMetricsViewRequestsAbsent(t *testing.T) {
	v := newMetricsView(&fakeRange{ans: func(string) []series { return nil }}, zap.NewNop())
	got, _ := json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	if string(got) != `{"window":"1h"}` {
		t.Fatalf("no observer: %s", got)
	}
	nan := series{points: []point{{t: time.Now(), v: math.NaN()}}}
	v = newMetricsView(&fakeRange{ans: func(string) []series { return nil }, requests: func(q string) []series {
		if strings.HasPrefix(q, "sum by (class)") {
			return []series{{labels: map[string]string{"class": "2xx"}, points: []point{{t: time.Now(), v: 0}}}}
		}
		return []series{nan}
	}}, zap.NewNop())
	got, _ = json.Marshal(v.AppMetrics(context.Background(), "blog", "1h"))
	if string(got) != `{"window":"1h","requests":{"total":0,"per_second":0}}` {
		t.Fatalf("an idle window: %s", got)
	}
	if a := v.AppMetrics(context.Background(), "blog", "7d"); a.Error != errViewStore {
		t.Fatalf("an unknown window: %+v", a)
	}
}

// The metrics object with every field set: each key's name and place,
// as GET /<app> carries it (liveswap's testdata/wire holds the same
// object inside a status).
func TestMetricsObjectEveryField(t *testing.T) {
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	m := wire.Metrics{
		Window: "24h", At: &at,
		Memory:    &wire.MemorySummary{Now: f(30), Max: f(40)},
		MemoryMax: f(100),
		CPU:       &wire.CPUSummary{Avg5m: f(0.5), Avg: 0.25},
		Requests: &wire.RequestSummary{
			Total: 10, PerSecond: 0.000116, ByClass: map[string]uint64{"5xx": 1, "2xx": 9},
			Share5xx: f(0.1), P50: f(1.5), P95: f(20), CacheHitShare: f(0.5),
		},
		Error: errViewStore,
	}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"window":"24h","at":"2026-11-13T09:30:00Z","memory_bytes":{"now":30,"max":40},"memory_max_bytes":100,"cpu_cores":{"avg_5m":0.5,"avg":0.25},"requests":{"total":10,"per_second":0.000116,"by_class":{"2xx":9,"5xx":1},"5xx_share":0.1,"p50_ms":1.5,"p95_ms":20,"cache_hit_share":0.5},"error":"the metrics store did not answer"}`
	if string(got) != want {
		t.Fatalf("metrics object:\n got %s\nwant %s", got, want)
	}
}
