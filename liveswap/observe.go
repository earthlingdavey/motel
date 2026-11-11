package liveswap

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(Observe{})
}

// Observe is the response observer (notify/DESIGN-metrics.md, "The
// response observer"): it counts an app's responses by status class,
// times them to their headers, and reads the cache's verdict, per
// release. Written by the operator first in the app's site or route:
//
//	route {
//	    liveswap_observe <app>
//	    cache
//	    reverse_proxy {
//	        dynamic liveswap <app>
//	    }
//	}
//
// The app is named on the line, not learned from the upstream: a cache
// hit never reaches it. Nothing of a request is kept: not its path,
// query, headers or client; only the class, a latency bucket and the
// cache result, in counters that live as long as the app does.
type Observe struct {
	// App names the liveswap app whose responses these are.
	App string `json:"app,omitempty"`

	ma *managedApp
}

// CaddyModule returns the Caddy module information.
func (Observe) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.liveswap_observe",
		New: func() caddy.Module { return new(Observe) },
	}
}

// Provision resolves the app reference, as Upstreams does.
func (o *Observe) Provision(ctx caddy.Context) error {
	if o.App == "" {
		return fmt.Errorf("an app name is required: liveswap_observe <app>")
	}
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	o.ma = appModule.(*App).managedApp(o.App)
	if o.ma == nil {
		return fmt.Errorf("unknown liveswap app %q (define it in the liveswap global options)", o.App)
	}
	return nil
}

// UnmarshalCaddyfile parses `liveswap_observe <app>`.
func (o *Observe) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // the directive's name
	if !d.NextArg() {
		return d.ArgErr()
	}
	o.App = d.Val()
	if d.NextArg() {
		return d.ArgErr()
	}
	if d.NextBlock(0) {
		return d.Err("liveswap_observe takes no block")
	}
	return nil
}

// ServeHTTP times the response to its headers and counts it under the
// app's current release. Never fails a request: it only reads.
func (o *Observe) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rc := o.ma.requests.current()
	ow := &observeWriter{ResponseWriter: w, start: time.Now()}
	err := next.ServeHTTP(ow, r)
	if !ow.seen {
		// Nothing written: an error the server's handle_errors answers
		// after this returns (no instance running, a handler's error),
		// or a client gone first.
		ow.seen, ow.at = true, time.Now()
		ow.status = http.StatusInternalServerError
		var he caddyhttp.HandlerError
		if errors.As(err, &he) && he.StatusCode != 0 {
			ow.status = he.StatusCode
		}
		if ow.status == statusClientClosed || (err != nil && r.Context().Err() != nil) {
			ow.status = 0
		}
	}
	rc.record(ow)
	return err
}

// statusClientClosed is the status reverse_proxy gives a request whose
// client went away (its own 499, never sent).
const statusClientClosed = 499

// observeWriter notes the final status, when the headers went, and the
// cache's verdict, at the first moment the headers are committed. It
// never buffers or changes a byte.
type observeWriter struct {
	http.ResponseWriter
	start  time.Time
	seen   bool
	status int // 0: the client went first
	at     time.Time
	cache  cacheResult
}

func (w *observeWriter) note(status int) {
	if w.seen {
		return
	}
	w.seen, w.status, w.at = true, status, time.Now()
	w.cache = parseCacheStatus(w.Header().Get("Cache-Status"))
}

func (w *observeWriter) WriteHeader(status int) {
	// An interim 1xx is not the response; 101 is the final one of an
	// upgrade (as penaltybox's hintInterceptor reads them).
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.note(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *observeWriter) Write(b []byte) (int, error) {
	w.note(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// ReadFrom keeps the underlying writer's fast path (sendfile) after
// the implicit 200 is noted.
func (w *observeWriter) ReadFrom(r io.Reader) (int64, error) {
	w.note(http.StatusOK)
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

// FlushError notes the implicit 200 a flush before any write commits;
// http.ResponseController tries it before Unwrap.
func (w *observeWriter) FlushError() error {
	w.note(http.StatusOK)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.NewResponseController reach Hijack and the rest.
func (w *observeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// cacheResult is the cache's verdict on a response, from its
// Cache-Status header (RFC 9211, as Souin writes it).
type cacheResult int

const (
	cacheNone cacheResult = iota // no cache in front, or it said nothing
	cacheHit
	cacheStale
	cacheMiss
	cacheBypass
)

// parseCacheStatus reads the first (outermost) cache's entry: a hit
// (stale when it says fwd=stale), a miss (fwd=uri-miss, or a stored
// revalidation, fwd=request), or a bypass (fwd=bypass).
func parseCacheStatus(v string) cacheResult {
	if v == "" {
		return cacheNone
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	hit, stale := false, false
	fwd := ""
	// Cut, not Split: this runs on every response, and Split allocates.
	_, rest, _ := strings.Cut(v, ";")
	for rest != "" {
		var p string
		p, rest, _ = strings.Cut(rest, ";")
		p = strings.TrimSpace(p)
		switch {
		case p == "hit":
			hit = true
		case strings.HasPrefix(p, "fwd="):
			fwd = strings.ToLower(strings.Trim(p[len("fwd="):], `"`))
			stale = stale || fwd == "stale"
		}
	}
	switch {
	case hit && stale:
		return cacheStale
	case hit:
		return cacheHit
	case fwd == "bypass":
		return cacheBypass
	case fwd != "":
		return cacheMiss
	}
	return cacheNone
}

// The classes a response is counted under, in RequestClasses' order:
// 101 alone of the 1xx, and "aborted" for a client gone before the
// headers (in the total, not in the 5xx share).
const (
	class1xx = iota
	class2xx
	class3xx
	class4xx
	class5xx
	classAborted
	numClasses
)

// RequestClasses names the classes, by index into ReleaseRequests.ByClass.
var RequestClasses = [numClasses]string{"1xx", "2xx", "3xx", "4xx", "5xx", "aborted"}

// CacheResults names the cache's verdicts, by index into
// ReleaseRequests.Cache; a response no cache spoke for is not counted.
var CacheResults = [4]string{"hit", "stale", "miss", "bypass"}

// ResponseBuckets are the latency histogram's upper bounds in seconds
// (Prometheus's defaults): the time to the response's headers.
var ResponseBuckets = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

func classOf(status int) int {
	switch {
	case status == 0:
		return classAborted
	case status < 200:
		return class1xx
	case status < 300:
		return class2xx
	case status < 400:
		return class3xx
	case status < 500:
		return class4xx
	}
	return class5xx
}

// releaseCounts is one release's counters: atomics only, so the request
// path takes no lock.
type releaseCounts struct {
	release string
	classes [numClasses]atomic.Uint64
	// buckets are per bucket, not cumulative; the last is +Inf.
	buckets  [len(ResponseBuckets) + 1]atomic.Uint64
	sumNanos atomic.Uint64
	cache    [len(CacheResults)]atomic.Uint64
}

func (rc *releaseCounts) record(w *observeWriter) {
	c := classOf(w.status)
	rc.classes[c].Add(1)
	if w.cache != cacheNone {
		rc.cache[w.cache-1].Add(1)
	}
	// The latency is the release's own: a cache hit's is the cache's, an
	// upgrade's is no response time, and an aborted one has no headers.
	if c == class1xx || c == classAborted || w.cache == cacheHit || w.cache == cacheStale {
		return
	}
	d := w.at.Sub(w.start)
	if d < 0 {
		d = 0
	}
	rc.sumNanos.Add(uint64(d))
	s := d.Seconds()
	i := 0
	for i < len(ResponseBuckets) && s > ResponseBuckets[i] {
		i++
	}
	rc.buckets[i].Add(1)
}

// ReleaseRequests is one release's counts as the collector reads them.
type ReleaseRequests struct {
	Release string
	ByClass [numClasses]uint64
	// Buckets are cumulative at ResponseBuckets' bounds; Count is the
	// +Inf bucket.
	Buckets    [len(ResponseBuckets)]uint64
	Count      uint64
	SumSeconds float64
	Cache      [len(CacheResults)]uint64
}

func (rc *releaseCounts) snapshot() ReleaseRequests {
	out := ReleaseRequests{Release: rc.release}
	for i := range rc.classes {
		out.ByClass[i] = rc.classes[i].Load()
	}
	var cum uint64
	for i := range rc.buckets {
		cum += rc.buckets[i].Load()
		if i < len(out.Buckets) {
			out.Buckets[i] = cum
		}
	}
	out.Count = cum
	out.SumSeconds = float64(rc.sumNanos.Load()) / 1e9
	for i := range rc.cache {
		out.Cache[i] = rc.cache[i].Load()
	}
	return out
}

// requestCounts is an app's counters: the current release's, and the
// one before's (still pushed, for the post-deploy comparison). Pool-
// scoped like the managedApp that holds it, so a reload does not reset
// them.
type requestCounts struct {
	cur  atomic.Pointer[releaseCounts]
	mu   sync.Mutex // guards prev and every swap of cur
	prev *releaseCounts
}

// current is the counters a response is recorded under: the current
// release's, or, before any, release "".
func (r *requestCounts) current() *releaseCounts {
	if rc := r.cur.Load(); rc != nil {
		return rc
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rc := r.cur.Load(); rc != nil {
		return rc
	}
	rc := &releaseCounts{}
	r.cur.Store(rc)
	return rc
}

// setRelease makes version the release responses are counted under.
// Called where the current instance changes (publishInstance, the
// reattach), not where it is unrouted: a release that crashed still
// owns the 502s that follow. A return to the release before swaps the
// two back, so neither's counters restart.
func (r *requestCounts) setRelease(version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	switch {
	case cur != nil && cur.release == version:
		return
	case r.prev != nil && r.prev.release == version:
		back := r.prev
		r.prev = cur
		r.cur.Store(back)
		return
	}
	// The counts made before any release ("") are set aside only while
	// there is nothing better to keep.
	if cur != nil && (cur.release != "" || r.prev == nil) {
		r.prev = cur
	}
	r.cur.Store(&releaseCounts{release: version})
}

// samples is both releases' counts, current first.
func (r *requestCounts) samples() []ReleaseRequests {
	r.mu.Lock()
	cur, prev := r.cur.Load(), r.prev
	r.mu.Unlock()
	var out []ReleaseRequests
	if cur != nil {
		out = append(out, cur.snapshot())
	}
	if prev != nil {
		out = append(out, prev.snapshot())
	}
	return out
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Observe)(nil)
	_ caddyhttp.MiddlewareHandler = (*Observe)(nil)
	_ caddyfile.Unmarshaler       = (*Observe)(nil)
)
