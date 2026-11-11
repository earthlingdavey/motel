package liveswap

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeMetricsSource struct {
	asked []string
	reply any
}

func (f *fakeMetricsSource) AppMetrics(_ context.Context, app string) any {
	f.asked = append(f.asked, app)
	return f.reply
}

func TestAppSamplesReportTheRunningInstance(t *testing.T) {
	h, _ := newTestHandler(t)
	got := h.app.AppSamples()
	if len(got) != 1 || got[0] != (AppSample{App: "demo"}) {
		t.Fatalf("before a deploy: %+v", got)
	}
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	got = h.app.AppSamples()
	if len(got) != 1 || got[0].App != "demo" || got[0].Release != "v1" || !got[0].Running {
		t.Fatalf("after deploying v1: %+v", got)
	}
}

func TestStatusMetricsOnlyWithASource(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	if strings.Contains(w.Body.String(), `"metrics"`) {
		t.Fatalf("no source, yet the status has metrics: %s", w.Body.String())
	}

	src := &fakeMetricsSource{reply: map[string]any{"window": "1h", "memory_bytes": map[string]float64{"now": 42}}}
	h.app.SetMetricsSource(src)
	w = do(t, h, http.MethodGet, "/demo", appToken(t), "")
	var body struct {
		Metrics struct {
			Window string `json:"window"`
			Memory struct {
				Now float64 `json:"now"`
			} `json:"memory_bytes"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || body.Metrics.Window != "1h" || body.Metrics.Memory.Now != 42 {
		t.Fatalf("status with a source: %d %s", w.Code, w.Body.String())
	}
	if len(src.asked) != 1 || src.asked[0] != "demo" {
		t.Fatalf("the source was asked %v, want [demo]", src.asked)
	}

	// Only the status: a deploy's answer and a record carry none.
	w = do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	if strings.Contains(w.Body.String(), `"metrics"`) || len(src.asked) != 1 {
		t.Fatalf("a deploy's answer asked the source or carries metrics: %s", w.Body.String())
	}
	if w = do(t, h, http.MethodGet, "/demo?deploy=v2", appToken(t), ""); strings.Contains(w.Body.String(), `"metrics"`) {
		t.Fatalf("a deploy record carries metrics: %s", w.Body.String())
	}

	// An unauthenticated GET never reaches the source.
	if w = do(t, h, http.MethodGet, "/demo", "", ""); w.Code != http.StatusUnauthorized || len(src.asked) != 1 {
		t.Fatalf("unauthenticated: %d, source asked %d times", w.Code, len(src.asked))
	}
}

// A cap the kernel does not enforce is no cap, as status says
// (capsForStatus): the store must not hold one either.
func TestSampleMemoryMaxOnlyWhenEnforced(t *testing.T) {
	caps := resourceCaps{memoryMax: 256 << 20}
	for _, c := range []struct {
		hs   handleState
		want uint64
	}{
		{handleState{Caps: caps}, 256 << 20},
		{handleState{Caps: caps, CapsUnknown: true}, 0},
		{handleState{Caps: caps, CapsUnenforced: []string{"memory_max"}}, 0},
		{handleState{Caps: caps, CapsUnenforced: []string{"tasks_max"}}, 256 << 20},
	} {
		if got := sampleMemoryMax(c.hs); got != c.want {
			t.Errorf("sampleMemoryMax(%+v) = %d, want %d", c.hs, got, c.want)
		}
	}
}

func TestSystemdHandleStateCarriesItsCgroup(t *testing.T) {
	h := &systemdHandle{unit: "app-demo.service"}
	if got := h.state().Cgroup; got != "" {
		t.Fatalf("before the manager reported one: %q", got)
	}
	cg := "/user.slice/user-1000.slice/user@1000.service/app.slice/app-demo.service"
	h.cgroup.Store(&cg)
	if got := h.state().Cgroup; got != cg {
		t.Fatalf("Cgroup = %q, want %q", got, cg)
	}
}
