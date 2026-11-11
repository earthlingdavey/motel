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

	"github.com/caddyserver/caddy/v2"
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
		// One value holding two entries: the first is read.
		{"Souin; hit, CDN; fwd=uri-miss", cacheHit},
		{"Souin", cacheNone},
		{"garbage;;;", cacheNone},
	} {
		if got := parseCacheStatus(c.in); got != c.want {
			t.Errorf("parseCacheStatus(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// publishFor routes a socket of version on ma as publishInstance does:
// the release set, its counts on the socket, then the socket routed.
func publishFor(ma *managedApp, version string) {
	sock := &socketRef{pinned: "/run/" + version + ".sock", linked: true}
	sock.counts.Store(ma.requests.setRelease(version))
	ma.activeSocket.Store(sock)
}

// caddyRequest is a request prepared as Caddy's server prepares one
// (its vars among the rest).
func caddyRequest(ctx context.Context) *http.Request {
	return caddyhttp.PrepareRequest(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), caddy.NewReplacer(), nil, nil)
}

// proxied is next behind reverse_proxy to the observed app: the upstream
// selected first (GetUpstreams), then next.
func proxied(next caddyhttp.HandlerFunc) caddyhttp.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ow := caddyhttp.GetVar(r.Context(), observeVar).(*observeWriter)
		_, _ = (&Upstreams{App: ow.ma.name, ma: ow.ma}).GetUpstreams(r)
		return next(w, r)
	}
}

// noInstance is next behind reverse_proxy to the observed app with no
// instance routed: the selection made, and none found.
func noInstance(next caddyhttp.HandlerFunc) caddyhttp.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ow := caddyhttp.GetVar(r.Context(), observeVar).(*observeWriter)
		markUpstream(r, ow.ma, nil)
		return next(w, r)
	}
}

// observeOnce runs one request through an observer of a fresh app, v1
// routed, and returns its counts.
func observeOnce(t *testing.T, ctx context.Context, next caddyhttp.HandlerFunc) (ReleaseRequests, *httptest.ResponseRecorder, error) {
	t.Helper()
	o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
	publishFor(o.ma, "v1")
	rec := httptest.NewRecorder()
	err := o.ServeHTTP(rec, caddyRequest(ctx), next)
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
		{"implicit 200", context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
			_, err := w.Write([]byte("hello"))
			return err
		}), "2xx", true},
		{"503", context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusServiceUnavailable)
			return nil
		}), "5xx", true},
		{"interim 103 then 302", context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusFound)
			return nil
		}), "3xx", true},
		{"an upgrade", context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusSwitchingProtocols)
			return nil
		}), "1xx", false},
		{"no instance, nothing written", context.Background(), noInstance(func(http.ResponseWriter, *http.Request) error {
			return he
		}), "5xx", true},
		{"an error with no status", context.Background(), proxied(func(http.ResponseWriter, *http.Request) error {
			return errors.New("boom")
		}), "5xx", true},
		{"a 404 error", context.Background(), proxied(func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(http.StatusNotFound, nil)
		}), "4xx", true},
		{"reverse_proxy's 499", context.Background(), proxied(func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(statusClientClosed, context.Canceled)
		}), "aborted", false},
		{"the client gone, an error", cancelled, proxied(func(http.ResponseWriter, *http.Request) error {
			return caddyhttp.Error(http.StatusBadGateway, context.Canceled)
		}), "aborted", false},
		{"a flush before any write", context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
			return http.NewResponseController(w).Flush()
		}), "2xx", true},
		// Answered before the upstream (penaltybox's refusal, a `respond`):
		// counted, never timed as the release's.
		{"a refusal before the upstream", context.Background(), func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusTooManyRequests)
			return nil
		}, "4xx", false},
		// net/http sends a 200 for a handler that returns no error having
		// written nothing (a route no terminal handler matched).
		{"nothing written, no error", context.Background(), func(http.ResponseWriter, *http.Request) error {
			return nil
		}, "2xx", false},
		{"the client gone, no error", cancelled, func(http.ResponseWriter, *http.Request) error {
			return nil
		}, "aborted", false},
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

// A handler that panics is counted, and its panic goes on: Caddy's
// `abort` before any header (no response: aborted), reverse_proxy's on a
// stream cut mid-body (the status that went).
func TestObservePanicIsCounted(t *testing.T) {
	for _, c := range []struct {
		name  string
		next  caddyhttp.HandlerFunc
		class string
	}{
		{"abort", func(http.ResponseWriter, *http.Request) error {
			panic(http.ErrAbortHandler)
		}, "aborted"},
		{"a stream cut mid-body", func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("part"))
			panic(http.ErrAbortHandler)
		}, "2xx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
			o.ma.requests.setRelease("v1")
			func() {
				defer func() {
					if p, _ := recover().(error); !errors.Is(p, http.ErrAbortHandler) {
						t.Errorf("panic %v, want http.ErrAbortHandler passed on", p)
					}
				}()
				_ = o.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), c.next)
			}()
			if cc := classCounts(o.ma.requests.samples()[0]); len(cc) != 1 || cc[c.class] != 1 {
				t.Fatalf("classes %v, want one %s", cc, c.class)
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
		header   string
		upstream bool // the request went to the app
		result   int
		latency  bool
	}{
		{"Souin; hit; ttl=10; detail=OTTER", false, 0, false},
		{"Souin; hit; ttl=-1; fwd=stale", false, 1, false},
		// A stale hit whose revalidation went to the app: the cache's
		// answer still.
		{"Souin; hit; ttl=-1; fwd=stale", true, 1, false},
		{"Souin; fwd=uri-miss; stored", true, 2, true},
		// A miss Souin coalesced onto another request's upstream call: no
		// selection of its own, yet it waited on the app.
		{"Souin; fwd=uri-miss; stored", false, 2, true},
		{"Souin; fwd=bypass; detail=IS-MUTATION-REQUEST", true, 3, true},
	} {
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("Cache-Status", c.header)
			w.WriteHeader(http.StatusOK)
			return nil
		})
		if c.upstream {
			next = proxied(next)
		}
		got, rec, _ := observeOnce(t, context.Background(), next)
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
	// On a bypass Souin sets its entry before the app answers, and
	// reverse_proxy adds the app's own after it: Souin's is the verdict.
	got, _, _ := observeOnce(t, context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("Cache-Status", "Souin; fwd=bypass; detail=IS-MUTATION-REQUEST")
		w.Header().Add("Cache-Status", "app; hit")
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if got.Cache[3] != 1 || got.Count != 1 {
		t.Errorf("an app's entry after Souin's bypass: cache %v, timed %d; want bypass, timed", got.Cache, got.Count)
	}
	// A miss whose upstream failed: Souin names the miss and returns the
	// error, nothing written. Still a miss, so a hit share is not of the
	// responses that worked alone.
	got, _, _ = observeOnce(t, context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("Cache-Status", "Souin; fwd=uri-miss; key=GET-http-example.com-/; detail=SERVE-HTTP-ERROR")
		return caddyhttp.Error(http.StatusServiceUnavailable, errors.New("no upstreams available"))
	}))
	if got.Cache[2] != 1 || got.ByClass[class5xx] != 1 {
		t.Errorf("a failed miss: cache %v, classes %v; want one miss, a 5xx", got.Cache, classCounts(got))
	}
	// No cache in front: counted under no result.
	got, _, _ = observeOnce(t, context.Background(), proxied(func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if got.Cache != [len(CacheResults)]uint64{} {
		t.Errorf("no Cache-Status, yet %v", got.Cache)
	}
}

func TestObserveLatencyBuckets(t *testing.T) {
	rc := &releaseCounts{release: "v1"}
	start := time.Unix(1000, 0)
	for _, d := range []time.Duration{time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, 3 * time.Second, time.Minute} {
		rc.record(&observeWriter{start: start, at: start.Add(d), seen: true, status: http.StatusOK}, true)
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

// A response counts under the release whose instance the request went
// to, not the one current when it began: a request selected after a
// cutover is the new release's, one selected before it the old's
// whenever it ends. Another app's selection is not this observer's.
func TestObserveCountsTheReleaseThatServed(t *testing.T) {
	ma := &managedApp{name: "demo"}
	publishFor(ma, "v1")
	o := &Observe{App: "demo", ma: ma}
	u := &Upstreams{App: "demo", ma: ma}
	// Entered under v1, selected after the cutover to v2.
	_ = o.ServeHTTP(httptest.NewRecorder(), caddyRequest(context.Background()), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		publishFor(ma, "v2")
		_, _ = u.GetUpstreams(r)
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	// Selected under v2, answered after the rollback to v1.
	_ = o.ServeHTTP(httptest.NewRecorder(), caddyRequest(context.Background()), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = u.GetUpstreams(r)
		publishFor(ma, "v1")
		w.WriteHeader(http.StatusBadGateway)
		return nil
	}))
	s := ma.requests.samples()
	if len(s) != 2 || s[0].Release != "v1" || s[1].Release != "v2" {
		t.Fatalf("samples %+v", s)
	}
	if cc := classCounts(s[0]); len(cc) != 0 {
		t.Errorf("v1 counted %v, want nothing", cc)
	}
	if cc := classCounts(s[1]); cc["2xx"] != 1 || cc["5xx"] != 1 || s[1].Count != 2 {
		t.Errorf("v2 counted %v (timed %d), want a 2xx and a 5xx, both timed", cc, s[1].Count)
	}
	// Another app's upstream in the observed route.
	other := &managedApp{name: "shop"}
	publishFor(other, "s1")
	_ = o.ServeHTTP(httptest.NewRecorder(), caddyRequest(context.Background()), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = (&Upstreams{App: "shop", ma: other}).GetUpstreams(r)
		w.WriteHeader(http.StatusOK)
		return nil
	}))
	if s := ma.requests.samples(); s[0].ByClass[class2xx] != 1 || s[0].Count != 0 {
		t.Errorf("demo's v1 after shop's selection: %+v, want one 2xx, untimed", s[0])
	}
	if s := other.requests.samples(); s[0].ByClass != [numClasses]uint64{} {
		t.Errorf("shop counted %+v", s[0])
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
// response through it, in a request prepared as Caddy's server prepares
// one, to a handler that selects the app's upstream (as reverse_proxy
// does) and writes a status.
func BenchmarkObserve(b *testing.B) {
	o := &Observe{App: "demo", ma: &managedApp{name: "demo"}}
	publishFor(o.ma, "v1")
	u := &Upstreams{App: "demo", ma: o.ma}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		if _, err := u.GetUpstreams(r); err != nil {
			return err
		}
		w.Header().Set("Cache-Status", "Souin; fwd=uri-miss; stored")
		w.WriteHeader(http.StatusOK)
		return nil
	})
	w := &discardWriter{h: http.Header{}}
	r := caddyhttp.PrepareRequest(httptest.NewRequest(http.MethodGet, "/", nil), caddy.NewReplacer(), w, nil)
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
