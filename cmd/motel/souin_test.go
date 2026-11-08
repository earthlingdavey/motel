package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddytest"
)

// The tests in this file hold the Souin cache motel links in (cache +
// otter in front of reverse_proxy, the shape of packaging/Caddyfile and
// e2e/Caddyfile) to behaviour a shared HTTP cache owes the apps behind
// it. Each one reproduces a bug Souin fixed on master after v1.7.9, the
// pinned release, and names the upstream issue; written against the pin
// they fail, and the upgrade turns them green. The exception is the
// stale-copy test on otter, which fails before and after: that bug is in
// the storage module. They run Caddy in this test binary through
// caddytest, as Souin's own plugin tests do, so the module graph under
// test is motel's, not Souin's.

// otterCache is the cache block the shipped Caddyfiles use.
const otterCache = "otter\n\t\t\tttl 60s"

// souinCaddyfile is one Caddy config: the global cache block with the
// given body, and a localhost site that caches the upstream through a
// cache directive with the given block (empty for a bare one).
func souinCaddyfile(cacheBlock, handlerBlock, upstream string) string {
	if handlerBlock != "" {
		handlerBlock = " {\n\t\t\t\t" + handlerBlock + "\n\t\t\t}"
	}
	return fmt.Sprintf(`
	{
		admin localhost:2999
		http_port 9080
		cache {
			%s
		}
	}
	localhost:9080 {
		route {
			cache%s
			reverse_proxy %s
		}
	}`, cacheBlock, handlerBlock, upstream)
}

// souinRig is one Caddy config serving one origin through the cache.
type souinRig struct {
	t      *testing.T
	calls  atomic.Int64 // requests the origin answered
	prefix string       // path prefix: stored entries outlive a config reload
	origin *httptest.Server
	client *http.Client
}

var souinRigs atomic.Int64

// startSouin loads a Caddyfile with the given cache block body and a
// localhost site that caches the origin. The origin handler runs in this
// process; calls counts its requests.
func startSouin(t *testing.T, cacheBlock string, origin http.HandlerFunc) *souinRig {
	t.Helper()
	return startSouinHandler(t, cacheBlock, "", origin)
}

// startSouinHandler is startSouin with a block on the site's cache
// directive too.
func startSouinHandler(t *testing.T, cacheBlock, handlerBlock string, origin http.HandlerFunc) *souinRig {
	t.Helper()
	rig := &souinRig{
		t:      t,
		prefix: fmt.Sprintf("/rig%d", souinRigs.Add(1)),
		client: &http.Client{Timeout: 10 * time.Second},
	}
	rig.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.calls.Add(1)
		origin(w, r)
	}))
	t.Cleanup(rig.origin.Close)

	tester := caddytest.NewTester(t)
	tester.InitServer(souinCaddyfile(cacheBlock, handlerBlock, strings.TrimPrefix(rig.origin.URL, "http://")), "caddyfile")
	if t.Failed() {
		t.FailNow()
	}
	return rig
}

// reply is a response with its body read.
type reply struct {
	status int
	header http.Header
	body   string
}

// cacheStatus is the Cache-Status header Souin sets, "Souin; hit; ..."
// or "Souin; fwd=uri-miss; ...".
func (r reply) cacheStatus() string { return r.header.Get("Cache-Status") }

func (r reply) hit() bool { return strings.Contains(r.cacheStatus(), "; hit") }

// get requests the path on the cached site; headers are name, value pairs.
func (rig *souinRig) get(path string, headers ...string) reply {
	rig.t.Helper()
	return rig.fetch("http://localhost:9080"+rig.prefix+path, headers...)
}

func (rig *souinRig) fetch(url string, headers ...string) reply {
	rig.t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		rig.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := rig.client.Do(req)
	if err != nil {
		rig.t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		rig.t.Fatalf("GET %s: read body: %v", url, err)
	}
	return reply{status: res.StatusCode, header: res.Header, body: string(body)}
}

// concurrent sends one request, then the others once the first is in
// flight, so they coalesce on its upstream call, and returns the replies
// in order.
func (rig *souinRig) concurrent(n int, path string) []reply {
	rig.t.Helper()
	replies := make([]reply, n)
	var wg sync.WaitGroup
	for i := range replies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i] = rig.get(path)
		}()
		if i == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	wg.Wait()
	return replies
}

// TestSouinDoesNotStoreSetCookieResponses: a response that sets a cookie
// is for one client; a shared cache that stores it hands that client's
// session to everyone who asks for the page next (darkweak/souin#805).
// The origin numbers its cookies, so a replayed one shows as a repeat.
func TestSouinDoesNotStoreSetCookieResponses(t *testing.T) {
	for name, cacheControl := range map[string]string{
		"no cache-control": "",
		"public max-age":   "public, max-age=60",
		"s-maxage":         "s-maxage=60",
	} {
		t.Run(name, func(t *testing.T) {
			var n atomic.Int64
			rig := startSouin(t, otterCache, func(w http.ResponseWriter, r *http.Request) {
				if cacheControl != "" {
					w.Header().Set("Cache-Control", cacheControl)
				}
				w.Header().Set("Set-Cookie", fmt.Sprintf("session=%d; Path=/", n.Add(1)))
				_, _ = fmt.Fprint(w, "page")
			})
			first := rig.get("/login")
			second := rig.get("/login")
			if first.header.Get("Set-Cookie") == "" {
				t.Fatalf("origin's cookie did not reach the client: %+v", first.header)
			}
			if second.hit() {
				t.Errorf("a response carrying Set-Cookie was stored and served to the next client: %s", second.cacheStatus())
			}
			if a, b := first.header.Get("Set-Cookie"), second.header.Get("Set-Cookie"); a == b {
				t.Errorf("the second client received the first client's cookie %q", b)
			}
			if got := rig.calls.Load(); got != 2 {
				t.Errorf("origin answered %d requests, want 2", got)
			}
		})
	}
}

// TestSouinPrivateIsHonouredWhenAuthorizationIsAKeyedHeader: keying on
// the Authorization header lets a shared cache store authenticated
// responses per credential, but `Cache-Control: private` still forbids
// storing (RFC 9111 §5.2.2.7). v1.7.9 treated the keyed header as leave
// to ignore private (darkweak/souin#863).
func TestSouinPrivateIsHonouredWhenAuthorizationIsAKeyedHeader(t *testing.T) {
	rig := startSouin(t, otterCache+"\n\t\t\tkey {\n\t\t\t\theaders Authorization\n\t\t\t}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		_, _ = fmt.Fprint(w, "secret for "+r.Header.Get("Authorization"))
	})
	for i := 0; i < 2; i++ {
		got := rig.get("/me", "Authorization", "Bearer alice")
		if got.body != "secret for Bearer alice" {
			t.Fatalf("response %d body %q", i+1, got.body)
		}
		if got.hit() {
			t.Errorf("a private response was stored: %s", got.cacheStatus())
		}
	}
	if got := rig.calls.Load(); got != 2 {
		t.Errorf("origin answered %d requests, want 2", got)
	}
}

// staleStorages are the cache blocks the stale tests run under: Souin's
// built-in memory storage, and otter as motel ships it. Both keep a
// stale copy for 30s past the TTL.
var staleStorages = map[string]string{
	"default storage": "ttl 60s\n\t\t\tstale 30s",
	"otter":           otterCache + "\n\t\t\tstale 30s",
}

// TestSouinServesStaleCopiesOnError: with stale-if-error, a stored
// response outlives its TTL by the stale window so that an origin
// failure is answered with the stale copy. Otter keeps the response
// body only for the TTL (storages/otter SetMultiLevel stores it with
// `duration`, where badger, nuts, redis, nats and etcd use `duration +
// stale`), so with motel's storage the copy is gone the moment it goes
// stale, and the client gets the error. Unreported upstream; not fixed
// by the Souin bump.
func TestSouinServesStaleCopiesOnError(t *testing.T) {
	for name, cacheBlock := range staleStorages {
		t.Run(name, func(t *testing.T) {
			var version atomic.Int64
			rig := startSouin(t, cacheBlock, func(w http.ResponseWriter, r *http.Request) {
				v := version.Add(1)
				if v > 1 {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprint(w, "origin down")
					return
				}
				w.Header().Set("Cache-Control", "public, s-maxage=1, stale-if-error=30")
				_, _ = fmt.Fprintf(w, "version %d", v)
			})
			if got := rig.get("/sie"); got.body != "version 1" {
				t.Fatalf("first response %q", got.body)
			}
			time.Sleep(2 * time.Second)
			got := rig.get("/sie")
			if got.status != http.StatusOK || got.body != "version 1" {
				t.Errorf("origin error was not covered by the stale copy: %d %q (%s)", got.status, got.body, got.cacheStatus())
			}
		})
	}
}

// TestSouinStaleWhileRevalidateRefreshesThroughReverseProxy: a stale
// response marked stale-while-revalidate is served as it is, and the
// refresh runs behind it on a request of its own. v1.7.9 fails twice
// over: it only takes that branch when the request itself says
// max-stale, so a browser's request fetches synchronously
// (darkweak/souin#684); and when it did, the Caddy wrapper handed
// reverse_proxy the client's request, whose context is cancelled as
// soon as the stale response is written, so the refresh always failed
// and the entry was stale forever (darkweak/souin#699). On otter the
// stale copy is not kept at all (TestSouinServesStaleCopiesOnError).
func TestSouinStaleWhileRevalidateRefreshesThroughReverseProxy(t *testing.T) {
	for name, cacheBlock := range staleStorages {
		t.Run(name, func(t *testing.T) {
			var version atomic.Int64
			rig := startSouin(t, cacheBlock, func(w http.ResponseWriter, r *http.Request) {
				v := version.Add(1)
				time.Sleep(100 * time.Millisecond)
				w.Header().Set("Cache-Control", "public, s-maxage=1, stale-while-revalidate=30")
				_, _ = fmt.Fprintf(w, "version %d", v)
			})
			if got := rig.get("/swr"); got.body != "version 1" {
				t.Fatalf("first response %q", got.body)
			}
			time.Sleep(2 * time.Second)
			if got := rig.get("/swr"); got.body != "version 1" {
				t.Fatalf("the stale copy should be served while revalidating, got %q (%s)", got.body, got.cacheStatus())
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if got := rig.get("/swr"); got.body == "version 2" {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Errorf("the background refresh never stored a new response (origin calls: %d)", rig.calls.Load())
		})
	}
}

// TestSouinCoalescedRequestsGetTheUpstreamError: requests that join an
// in-flight upstream call must receive what it received. When the
// origin answered 5xx, v1.7.9 gave the joiners an empty 200
// (darkweak/souin#876).
func TestSouinCoalescedRequestsGetTheUpstreamError(t *testing.T) {
	rig := startSouin(t, otterCache, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "upstream down")
	})
	for i, got := range rig.concurrent(4, "/down") {
		if got.status != http.StatusServiceUnavailable || got.body != "upstream down" {
			t.Errorf("request %d: %d %q, want 503 %q (%s)", i+1, got.status, got.body, "upstream down", got.cacheStatus())
		}
	}
}

// TestSouinCoalescedPrivateResponseIsFetchedOncePerClient: a private
// (or cookie-setting) response cannot be shared with the requests that
// joined its upstream call, so each of those fetches its own. The
// request that made the call already has its answer; v1.7.9 ran the
// handler again for it too, into the same response, duplicating the
// headers (darkweak/souin#886).
func TestSouinCoalescedPrivateResponseIsFetchedOncePerClient(t *testing.T) {
	rig := startSouin(t, otterCache, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Cache-Control", "private")
		w.Header().Set("Set-Cookie", "session=1; Path=/")
		_, _ = fmt.Fprint(w, "private body")
	})
	for i, got := range rig.concurrent(2, "/account") {
		if got.body != "private body" {
			t.Errorf("request %d body %q", i+1, got.body)
		}
		if cookies := got.header.Values("Set-Cookie"); len(cookies) != 1 {
			t.Errorf("request %d carries %d Set-Cookie headers, want 1: %q", i+1, len(cookies), cookies)
		}
	}
	if got := rig.calls.Load(); got != 2 {
		t.Errorf("origin answered %d requests for 2 clients, want 2", got)
	}
}

// TestSouinKeysOnThePathAsForwarded: the cache key must spell the path
// the way the origin sees it. v1.7.9 keyed on the decoded path, so
// /%70age and /page shared one entry while the origin answered them
// differently (darkweak/souin#882).
func TestSouinKeysOnThePathAsForwarded(t *testing.T) {
	rig := startSouin(t, otterCache, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = fmt.Fprint(w, r.URL.EscapedPath())
	})
	if got := rig.get("/%70age"); got.body != rig.prefix+"/%70age" {
		t.Fatalf("origin saw %q for /%%70age; the proxy rewrote the path, so this test cannot tell the keys apart", got.body)
	}
	got := rig.get("/page")
	if got.hit() || got.body != rig.prefix+"/page" {
		t.Errorf("/page was answered from /%%70age's entry: body %q, %s", got.body, got.cacheStatus())
	}
}

// TestSouinUntaggedResponsesAreNotIndexedUnderTheEmptyTag: every
// response without a Surrogate-Key header was appended to one index
// entry, SURROGATE_ with the empty tag, which grew by a key per cached
// page and was rewritten whole on every store (darkweak/souin#884). The
// Souin API, served on Caddy's admin endpoint, lists the index.
func TestSouinUntaggedResponsesAreNotIndexedUnderTheEmptyTag(t *testing.T) {
	rig := startSouin(t, otterCache+"\n\t\t\tapi {\n\t\t\t\tsouin\n\t\t\t}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = fmt.Fprint(w, "page")
	})
	for _, p := range []string{"/one", "/two", "/three"} {
		if got := rig.get(p); got.status != http.StatusOK {
			t.Fatalf("GET %s: %d", p, got.status)
		}
	}
	// The admin module wires its handlers once the middleware is up.
	var listing reply
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		listing = rig.fetch("http://localhost:2999/souin-api/souin/surrogate_keys")
		if listing.status == http.StatusOK || time.Now().After(deadline) {
			break
		}
	}
	var index map[string]string
	if err := json.Unmarshal([]byte(listing.body), &index); err != nil {
		t.Fatalf("surrogate index listing %d %q: %v", listing.status, listing.body, err)
	}
	if len(index) == 0 {
		t.Fatal("the surrogate index is empty; the API or the stores did not work")
	}
	if entry, ok := index[""]; ok {
		t.Errorf("untagged responses are indexed under the empty tag: %q", entry)
	}
}

// cacheKeysRules are the one-rule cache_keys blocks the cache_keys tests
// configure: a rule dropping the host from the key of the test's asset,
// spelled without and with a backslash.
var cacheKeysRules = map[string]string{
	"plain":     `^/rig[0-9]+/assets/.+$`,
	"backslash": `.+\.js`,
}

func cacheKeysBlock(regexp string) string {
	return "cache_keys {\n\t\t\t\t" + regexp + " {\n\t\t\t\t\tdisable_host\n\t\t\t\t}\n\t\t\t}"
}

// assertHostDroppedFromKey requests the asset the rule matches and reads
// the key Cache-Status reports, which shows whether the rule applied.
func assertHostDroppedFromKey(t *testing.T, rig *souinRig) {
	t.Helper()
	got := rig.get("/assets/app.js")
	if !strings.Contains(got.cacheStatus(), "key=") {
		t.Fatalf("no key in %q", got.cacheStatus())
	}
	if strings.Contains(got.cacheStatus(), "localhost") {
		t.Errorf("the rule disabling the host in the key did not apply: %s", got.cacheStatus())
	}
}

// TestSouinCacheKeysFirstRuleApplies: the Caddyfile is adapted to JSON
// and the cache directive's cache_keys rules go through that round trip.
// v1.7.9 lost the first rule's regexp on the way back, so a configuration
// with one rule had none; and a regexp with a backslash was written as
// invalid JSON, which the adapter turned into a null cache app the
// config load then refused (darkweak/souin#688).
func TestSouinCacheKeysFirstRuleApplies(t *testing.T) {
	for name, regexp := range cacheKeysRules {
		t.Run(name, func(t *testing.T) {
			adapted, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(souinCaddyfile(otterCache, cacheKeysBlock(regexp), "localhost:1")), nil)
			if err != nil {
				t.Fatalf("adapt: %v", err)
			}
			quoted, _ := json.Marshal(regexp)
			if !strings.Contains(string(adapted), string(quoted)) {
				t.Fatalf("the adapted config lost the cache_keys rule %s:\n%s", quoted, adapted)
			}
			rig := startSouinHandler(t, otterCache, cacheKeysBlock(regexp), func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "public, max-age=60")
				_, _ = fmt.Fprint(w, "js")
			})
			assertHostDroppedFromKey(t, rig)
		})
	}
}

// TestSouinGlobalCacheKeysReachTheHandler: cache_keys in the global
// cache block are documented as the default for every cache directive,
// which is where a motel Caddyfile would put them. The handler's FromApp
// takes them from the app but merges the handler's own list into itself
// instead, so a bare `cache` directive never sees them. Unreported
// upstream; master has the same loop, so the bump does not fix it.
func TestSouinGlobalCacheKeysReachTheHandler(t *testing.T) {
	rig := startSouin(t, otterCache+"\n\t\t\t"+cacheKeysBlock(cacheKeysRules["plain"]), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = fmt.Fprint(w, "js")
	})
	assertHostDroppedFromKey(t, rig)
}

// TestSouinNoCacheResponseIsRevalidated: a stored response marked
// no-cache may be reused only after the origin confirmed it (RFC 9111
// §5.2.2.4), so every request reaches the origin. v1.7.9 does this; the
// test guards it across the bump, which reworks how no-cache is read
// (only the unqualified directive forces revalidation from then on).
func TestSouinNoCacheResponseIsRevalidated(t *testing.T) {
	var version atomic.Int64
	rig := startSouin(t, otterCache, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = fmt.Fprintf(w, "version %d", version.Add(1))
	})
	rig.get("/news")
	if got := rig.get("/news"); got.body != "version 2" {
		t.Errorf("second response %q (%s): the no-cache response was reused without revalidation", got.body, got.cacheStatus())
	}
	if got := rig.calls.Load(); got != 2 {
		t.Errorf("origin answered %d requests, want 2", got)
	}
}
