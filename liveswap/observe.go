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
//	    observe <app>
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

	ma      *managedApp
	counted bool // in ma.observers
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
		return fmt.Errorf("an app name is required: observe <app>")
	}
	appModule, err := ctx.App("liveswap")
	if err != nil {
		return err
	}
	ma := appModule.(*App).managedApp(o.App)
	if ma == nil {
		return fmt.Errorf("unknown liveswap app %q (define it in the liveswap global options)", o.App)
	}
	o.observe(ma)
	return nil
}

// observe makes ma the app observed, by one handler more.
func (o *Observe) observe(ma *managedApp) {
	o.ma = ma
	ma.observers.Add(1)
	o.counted = true
}

// Cleanup is the config's unload (or its load failing): the app is
// observed by one handler fewer. A reload provisions the new config's
// before it cleans up the old's, so an app observed by both stays so.
func (o *Observe) Cleanup() error {
	if o.counted {
		o.ma.observers.Add(-1)
		o.counted = false
	}
	return nil
}

// UnmarshalCaddyfile parses `observe <app>`.
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
		return d.Err("observe takes no block")
	}
	return nil
}

// ServeHTTP times the response to its headers and counts it under the
// release whose instance the request went to, which the app's
// GetUpstreams marks (markUpstream); one that went to none (a cache
// hit, penaltybox's refusal, any other handler's answer, a miss Souin
// coalesced onto another request's upstream call) counts under the
// release current when it began. Never fails a request: it only
// reads. The count is deferred, so that a handler's panic is counted
// too (Caddy's `abort`, and reverse_proxy's on a stream cut mid-body,
// both panic http.ErrAbortHandler); the panic goes on as it came.
func (o *Observe) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) (err error) {
	rc := o.ma.requests.current()
	ow := &observeWriter{ResponseWriter: w, ma: o.ma, start: time.Now()}
	// In the request's vars, which every copy of it shares (Souin's,
	// reverse_proxy's clone): no new context or request to allocate.
	caddyhttp.SetVar(r.Context(), observeVar, ow)
	returned := false
	defer func() {
		if !ow.seen {
			ow.unanswered(r, err, returned)
		}
		up := ow.upstream.Load()
		if up != nil {
			rc = up
		}
		rc.record(ow, up != nil)
	}()
	err = next.ServeHTTP(ow, r)
	returned = true
	return err
}

// observeVar is the vars key the observer leaves itself under for the
// app's GetUpstreams.
const observeVar = "liveswap_observe"

// markUpstream tells the observer of r, when it observes ma, that the
// request reached ma's upstream selection, and so is the release's own
// response, and which release's instance it went to: sock's, or with
// none routed (reverse_proxy's 503 follows), the current release's,
// which owns it.
// reverse_proxy may select again (a retry): the last selection counts.
func markUpstream(r *http.Request, ma *managedApp, sock *socketRef) {
	ow, _ := caddyhttp.GetVar(r.Context(), observeVar).(*observeWriter)
	if ow == nil || ow.ma != ma {
		return
	}
	var rc *releaseCounts
	if sock != nil {
		rc = sock.counts.Load()
	}
	if rc == nil {
		rc = ma.requests.current()
	}
	// Atomic: under Souin the selection runs on its own goroutine, which
	// can outlive the response it gave up on.
	ow.upstream.Store(rc)
}

// unanswered notes what a request nothing was written for gets: no
// response, when a handler panicked or the client went first; the 200
// net/http sends for a handler that returned no error; or the status
// of the error the server's handle_errors answers after this returns
// (no instance running, a handler's error). The cache's verdict is read
// as for a written response: a miss whose upstream failed is still a
// miss (Souin writes `fwd=uri-miss; ... detail=SERVE-HTTP-ERROR` and
// returns the error).
func (w *observeWriter) unanswered(r *http.Request, err error, returned bool) {
	w.seen, w.at = true, time.Now()
	w.cache = parseCacheStatus(w.Header().Get("Cache-Status"))
	var he caddyhttp.HandlerError
	switch {
	case !returned || r.Context().Err() != nil:
		w.status = 0
	case err == nil:
		w.status = http.StatusOK
	case errors.As(err, &he) && he.StatusCode != 0:
		w.status = he.StatusCode
		if w.status == statusClientClosed {
			w.status = 0
		}
	default:
		w.status = http.StatusInternalServerError
	}
}

// statusClientClosed is the status reverse_proxy gives a request whose
// client went away (its own 499, never sent).
const statusClientClosed = 499

// observeWriter notes the final status, when the headers went, and the
// cache's verdict, at the first moment the headers are committed. It
// never buffers or changes a byte.
type observeWriter struct {
	http.ResponseWriter
	ma     *managedApp
	start  time.Time
	seen   bool
	status int // 0: no response (the client went first, or a panic)
	at     time.Time
	cache  cacheResult
	// upstream is the counts of the release whose instance the request
	// went to (markUpstream); nil: it reached no upstream selection.
	upstream atomic.Pointer[releaseCounts]
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

// parseCacheStatus reads the first entry of the header's first value,
// which is Souin's whenever it is in front: it Sets its one entry after
// the response on a lookup, and before next on a bypass, where
// reverse_proxy then Adds an app's own Cache-Status after it. RFC 9211
// puts the cache nearest the client last, but Souin replaces what came
// before it, so the last would be an app's on a bypass. With no cache in
// front, it is the app's own, and is read as its cache's verdict. The
// entry is a hit (stale when it says fwd=stale), a miss (fwd=uri-miss,
// or a stored revalidation, fwd=request), or a bypass (fwd=bypass).
//
// Souin's key is unquoted and holds the request's query (and any header
// the key is configured to take), so a ',' or ';' after `key=` is the
// client's, never the header's: the parameters are read up to it, and
// after it only the ending Souin gives a stale hit (staleEnding).
func parseCacheStatus(v string) cacheResult {
	if v == "" {
		return cacheNone
	}
	hit, stale := false, false
	fwd := ""
	// Cut, not Split: this runs on every response, and Split allocates.
	name, rest, _ := strings.Cut(v, ";")
	if strings.IndexByte(name, ',') >= 0 {
		return cacheNone // the first entry has no parameters
	}
	for rest != "" {
		var p string
		p, rest, _ = strings.Cut(rest, ";")
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "key=") {
			// Souin writes hit before the key: only a hit is read for its
			// stale ending.
			stale = stale || (hit && staleEnding(v))
			break
		}
		last := false
		if i := strings.IndexByte(p, ','); i >= 0 {
			// The first entry ends here.
			p, last = strings.TrimSpace(p[:i]), true
		}
		switch {
		case p == "hit":
			hit = true
		case strings.HasPrefix(p, "fwd="):
			fwd = strings.ToLower(strings.Trim(p[len("fwd="):], `"`))
			stale = stale || fwd == "stale"
		}
		if last {
			break
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

// staleEnding is whether a value with Souin's key in it ends as a stale
// hit's does: `; fwd=stale`, which Souin appends last, or that and the
// `; fwd-status=<n>` it adds after it when the origin failed. Read at
// the end alone, so that a key holding either text is never taken for
// it: a hit's key is followed by its `; detail=`.
func staleEnding(v string) bool {
	if strings.HasSuffix(v, "; fwd=stale") {
		return true
	}
	i := len(v)
	for i > 0 && v[i-1] >= '0' && v[i-1] <= '9' {
		i--
	}
	return i < len(v) && strings.HasSuffix(v[:i], "; fwd=stale; fwd-status=")
}

// The classes a response is counted under, in RequestClasses' order:
// 101 alone of the 1xx, and "aborted" for a request no headers went to,
// its client gone first or its handler aborted (in the total, not in
// the 5xx share).
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

// record counts w; proxied is whether it reached the app's upstream
// selection.
func (rc *releaseCounts) record(w *observeWriter, proxied bool) {
	c := classOf(w.status)
	rc.classes[c].Add(1)
	if w.cache != cacheNone {
		rc.cache[w.cache-1].Add(1)
	}
	// The latency is the release's own: of a response that went to it, or
	// that the cache fetched from it for this request (a miss or a bypass:
	// Souin coalesces concurrent misses into one upstream call, and only
	// the first request's selection is made, yet each waited on the app).
	// A response that went to neither (a hit, penaltybox's refusal, a
	// `respond`) is another handler's, a hit is the cache's though a
	// stale one's revalidation went on, an upgrade's is no response time,
	// and an aborted one has no headers.
	fetched := proxied || w.cache == cacheMiss || w.cache == cacheBypass
	if !fetched || c == class1xx || c == classAborted || w.cache == cacheHit || w.cache == cacheStale {
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

// setRelease makes version the release responses are counted under,
// and returns its counters, which the caller gives the instance's
// socket before routing it (socketRef.counts). Called where the
// current instance changes (publishInstance, the reattach), not where
// it is unrouted: a release that crashed still owns the errors that
// follow (502s while its socket is routed and refuses, 503s once it is
// unrouted). A return to the release before swaps the two back, so
// neither's counters restart.
func (r *requestCounts) setRelease(version string) *releaseCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.cur.Load()
	switch {
	case cur != nil && cur.release == version:
		return cur
	case r.prev != nil && r.prev.release == version:
		back := r.prev
		r.prev = cur
		r.cur.Store(back)
		return back
	}
	// The counts made before any release ("") are set aside only while
	// there is nothing better to keep.
	if cur != nil && (cur.release != "" || r.prev == nil) {
		r.prev = cur
	}
	rc := &releaseCounts{release: version}
	r.cur.Store(rc)
	return rc
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
	_ caddy.CleanerUpper          = (*Observe)(nil)
	_ caddyhttp.MiddlewareHandler = (*Observe)(nil)
	_ caddyfile.Unmarshaler       = (*Observe)(nil)
)
