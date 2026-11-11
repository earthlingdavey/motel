package liveswap

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestParseCacheStatus(t *testing.T) {
	for _, c := range []struct {
		in   string
		want cacheResult
	}{
		{"", cacheNone},
		{"Souin; hit; ttl=59; key=GET-http-example.com-/; detail=OTTER", cacheHit},
		{"Souin; hit; ttl=-3; key=GET-http-example.com-/; detail=OTTER; fwd=stale", cacheStale},
		{"Souin; fwd=uri-miss; key=GET-http-example.com-/; detail=OTTER; stored", cacheMiss},
		{"Souin; fwd=request; detail=REQUEST-REVALIDATION", cacheMiss},
		{"Souin; fwd=bypass; detail=UNSUPPORTED-METHOD", cacheBypass},
		{`Souin; fwd="bypass"`, cacheBypass},
		// The outermost cache's entry is the first; a CDN's after it is not
		// this cache's verdict.
		{"Souin; hit, CDN; fwd=uri-miss", cacheHit},
		{"Souin", cacheNone},
		{"garbage;;;", cacheNone},
	} {
		if got := parseCacheStatus(c.in); got != c.want {
			t.Errorf("parseCacheStatus(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// observeOnce runs one request through an observer of a fresh app and
// returns its counts.
func observeOnce(t *testing.T, ctx context.Context, next caddyhttp.HandlerFunc) (ReleaseRequests, *httptest.ResponseRecorder, error) {
	t.Helper()
	o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
	o.ma.requests.setRelease("v1")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	err := o.ServeHTTP(rec, r, next)
	s := o.ma.requests.samples()
	if len(s) == 0 || s[0].Release != "v1" {
		t.Fatalf("samples %+v", s)
	}
	return s[0], rec, err
}

func classCounts(r ReleaseRequests) map[string]uint64 {
	out := map[string]uint64{}
	for i, n := range r.ByClass {
		if n > 0 {
			out[RequestClasses[i]] = n
		}
	}
	return out
}

func TestObserveClasses(t *testing.T) {
	he := caddyhttp.Error(http.StatusBadGateway, errors.New("liveswap app \"demo\" has no running version yet"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name    string
		ctx     context.Context
		next    caddyhttp.HandlerFunc
		class   string
		latency bool
	}{
		{"implicit 200", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			_, err := w.Write([]byte("hello"))
			return err
		}, "2xx", true},
		{"503", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusServiceUnavailable)
			return nil
		}, "5xx", true},
		{"interim 103 then 302", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusFound)
			return nil
		}, "3xx", true},
		{"an upgrade", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusSwitchingProtocols)
			return nil
		}, "1xx", false},
		{"an error, nothing written", context.Background(), func(http.ResponseWriter, *http.Request) error {
			return he
		}, "5xx", true},
		{"an error with no status", context.Background(), func(http.ResponseWriter, *http.Request) error {
			return errors.New("boom")
		}, "5xx", true},
		{"a 404 error", context.Background(), func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(http.StatusNotFound, nil)
		}, "4xx", true},
		{"reverse_proxy's 499", context.Background(), func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(statusClientClosed, context.Canceled)
		}, "aborted", false},
		{"the client gone, an error", cancelled, func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(http.StatusBadGateway, context.Canceled)
		}, "aborted", false},
		{"a flush before any write", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			return http.NewResponseController(w).Flush()
		}, "2xx", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := observeOnce(t, c.ctx, c.next)
			if cc := classCounts(got); len(cc) != 1 || cc[c.class] != 1 {
				t.Fatalf("classes %v, want one %s", cc, c.class)
			}
			if timed := got.Count == 1; timed != c.latency {
				t.Fatalf("timed %v, want %v (count %d)", timed, c.latency, got.Count)
			}
		})
	}
}

func TestObserveReturnsTheErrorAndWritesNothing(t *testing.T) {
	he := caddyhttp.Error(http.StatusBadGateway, errors.New("no upstream"))
	_, rec, err := observeOnce(t, context.Background(), func(http.ResponseWriter, *http.Request) error { return he })
	if !errors.Is(err, he) {
		t.Fatalf("error %v, want the handler's", err)
	}
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK || len(rec.Header()) != 0 {
		t.Fatalf("the observer wrote: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestObserveCache(t *testing.T) {
	for _, c := range []struct {
		header  string
		result  int
		latency bool
	}{
		{"Souin; hit; ttl=10; detail=OTTER", 0, false},
		{"Souin; hit; ttl=-1; fwd=stale", 1, false},
		{"Souin; fwd=uri-miss; stored", 2, true},
		{"Souin; fwd=bypass; detail=IS-MUTATION-REQUEST", 3, true},
	} {
		got, rec, _ := observeOnce(t, context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("Cache-Status", c.header)
			w.WriteHeader(http.StatusOK)
			return nil
		})
		if got.Cache[c.result] != 1 {
			t.Errorf("%q: cache %v, want %s", c.header, got.Cache, CacheResults[c.result])
		}
		if (got.Count == 1) != c.latency {
			t.Errorf("%q: timed %v, want %v", c.header, got.Count == 1, c.latency)
		}
		if rec.Header().Get("Cache-Status") != c.header {
			t.Errorf("%q: the header was changed: %q", c.header, rec.Header().Get("Cache-Status"))
		}
	}
	// No cache in front: counted under no result.
	got, _, _ := observeOnce(t, context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	})
	if got.Cache != [len(CacheResults)]uint64{} {
		t.Errorf("no Cache-Status, yet %v", got.Cache)
	}
}

func TestObserveLatencyBuckets(t *testing.T) {
	rc := &releaseCounts{release: "v1"}
	start := time.Unix(1000, 0)
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, 3 * time.Second, time.Minute} {
		rc.record(&observeWriter{start: start, at: start.Add(d), seen: true, status: http.StatusOK})
	}
	s := rc.snapshot()
	if s.Count != 5 {
		t.Fatalf("count %d", s.Count)
	}
	// Cumulative: ≤5 ms holds 1, ≤25 ms 3, ≤5 s 4; a minute is +Inf only.
	want := map[float64]uint64{.005: 1, .01: 1, .025: 3, 2.5: 3, 5: 4, 10: 4}
	for i, b := range ResponseBuckets {
		if n, ok := want[b]; ok && s.Buckets[i] != n {
			t.Errorf("bucket le=%v: %d, want %d", b, s.Buckets[i], n)
		}
	}
	if s.SumSeconds < 63.04 || s.SumSeconds > 63.05 {
		t.Errorf("sum %v s, want 63.041", s.SumSeconds)
	}
}

// Hijack is reached through Unwrap.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestObserveUnwrapReachesHijack(t *testing.T) {
	o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
	hr := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	_ = o.ServeHTTP(hr, httptest.NewRequest(http.MethodGet, "/", nil), caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusSwitchingProtocols)
		_, _, err := http.NewResponseController(w).Hijack()
		return err
	}))
	if !hr.hijacked {
		t.Fatal("Hijack did not reach the underlying writer")
	}
}

// ReadFrom (io.Copy's fast path, sendfile) notes the implicit 200.
func TestObserveReadFrom(t *testing.T) {
	got, rec, _ := observeOnce(t, context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
		_, err := w.(io.ReaderFrom).ReadFrom(strings.NewReader("body"))
		return err
	})
	if got.ByClass[class2xx] != 1 || rec.Body.String() != "body" {
		t.Fatalf("ReadFrom: %v %q", classCounts(got), rec.Body.String())
	}
}

func TestRequestCountsReleases(t *testing.T) {
	var r requestCounts
	r.current().classes[class2xx].Add(1) // before any release
	r.setRelease("v1")
	r.current().classes[class2xx].Add(2)
	if s := r.samples(); len(s) != 2 || s[0].Release != "v1" || s[1].Release != "" || s[1].ByClass[class2xx] != 1 {
		t.Fatalf("after v1: %+v", s)
	}
	r.setRelease("v1") // a relaunch of the same release keeps its counts
	r.setRelease("v2")
	r.current().classes[class5xx].Add(1)
	if s := r.samples(); len(s) != 2 || s[0].Release != "v2" || s[1].Release != "v1" || s[1].ByClass[class2xx] != 2 {
		t.Fatalf("after v2: %+v", s)
	}
	// Back to v1 (a rollback): the two swap, and neither restarts.
	r.setRelease("v1")
	if s := r.samples(); s[0].Release != "v1" || s[0].ByClass[class2xx] != 2 || s[1].Release != "v2" || s[1].ByClass[class5xx] != 1 {
		t.Fatalf("after the rollback: %+v", s)
	}
	r.setRelease("v3")
	if s := r.samples(); len(s) != 2 || s[0].Release != "v3" || s[1].Release != "v1" {
		t.Fatalf("after v3, two kept: %+v", s)
	}
}

// A deploy sets the release responses are counted under, and the
// sample carries them.
func TestObserveFollowsTheDeploy(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	o := &Observe{App: "demo", ma: h.app.managedApp("demo")}
	_ = o.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	got := h.app.AppSamples()
	if len(got) != 1 || len(got[0].Requests) == 0 || got[0].Requests[0].Release != "v1" || got[0].Requests[0].ByClass[class2xx] != 1 {
		t.Fatalf("samples after one response to v1: %+v", got)
	}
}

func TestObserveCaddyfile(t *testing.T) {
	var o Observe
	if err := o.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`liveswap_observe blog`)); err != nil || o.App != "blog" {
		t.Fatalf("parse: %v %+v", err, o)
	}
	for _, in := range []string{
		`liveswap_observe`,
		`liveswap_observe blog shop`,
		"liveswap_observe blog {\n\tx\n}",
	} {
		var o Observe
		if err := o.UnmarshalCaddyfile(caddyfile.NewTestDispenser(in)); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

// BenchmarkObserve is the observer's own cost on the request path: one
// response through it to a handler that writes a status.
func BenchmarkObserve(b *testing.B) {
	o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
	o.ma.requests.setRelease("v1")
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("Cache-Status", "Souin; fwd=uri-miss; stored")
		w.WriteHeader(http.StatusOK)
		return nil
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = o.ServeHTTP(w, r, next)
	}
}

type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardWriter) WriteHeader(int)             {}
