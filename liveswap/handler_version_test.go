package liveswap

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

// testRelease stands for the release the release build links in.
const testRelease = "v9.8.7-test"

// linkRelease sets caddy.CustomVersion as the release build would,
// until the test ends.
func linkRelease(t *testing.T, v string) {
	t.Helper()
	saved := caddy.CustomVersion
	t.Cleanup(func() { caddy.CustomVersion = saved })
	caddy.CustomVersion = v
}

// sent is the header the handler sent: the recorder's snapshot at
// WriteHeader, not the live map a later write could change.
func sent(w *httptest.ResponseRecorder) http.Header { return w.Result().Header }

// TestVersionOnVerifiedAnswers: every answer to a token that verified
// carries Motel-Version, the release: each row of the route table, the
// 404 for a name that is no app, the 405 after authentication, the
// 400, 413 and 422s, a deploy stream (its headers go out with the first line), and
// the 403 Decide's refusal would give.
func TestVersionOnVerifiedAnswers(t *testing.T) {
	linkRelease(t, testRelease)
	pull := `{"url":"https://x/1.tgz","version":"v1"}`
	oversized := `{"url":"https://x/1.tgz","version":"v1","pad":"` + strings.Repeat("a", maxPayloadBytes) + `"}`
	landed := map[*route]bool{}
	for _, c := range []struct {
		name, method, target, token, contentType, accept, body string
		code                                                   int
	}{
		{"status", "GET", "/demo", "app", "", "", "", http.StatusOK},
		{"a range not offered", "GET", "/demo?range=7d", "app", "", "", "", http.StatusUnprocessableEntity},
		{"a malformed query", "GET", "/demo?deploy=%ZZ", "app", "", "", "", http.StatusUnprocessableEntity},
		{"a deploy record", "GET", "/demo?deploy=v1", "app", "", "", "", http.StatusNotFound},
		{"a pull", "POST", "/demo", "app", "application/json", "", pull, http.StatusOK},
		{"a payload refused", "POST", "/demo", "app", "application/json", "", "{", http.StatusBadRequest},
		{"a payload too large", "POST", "/demo", "app", "application/json", "", oversized, http.StatusRequestEntityTooLarge},
		{"a push", "POST", "/demo?version=v9", "app", "application/gzip", "", "tarball-bytes", http.StatusOK},
		{"a push without a version", "POST", "/demo", "app", "application/gzip", "", "x", http.StatusUnprocessableEntity},
		{"a rollback", "POST", "/demo?rollback=v3", "app", "", "", "", http.StatusOK},
		{"a deploy stream", "POST", "/demo", "app", "application/json", "application/x-ndjson", pull, http.StatusOK},
		{"another method", "DELETE", "/demo", "app", "", "", "", http.StatusMethodNotAllowed},
		{"a name that is no app", "GET", "/ghost", "global", "", "", "", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			if c.accept != "" {
				req.Header.Set("Accept", c.accept)
			}
			token := appToken(t)
			if c.token == "global" {
				token = globalToken(t)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			if rt := findRoute(req); rt != nil && c.target != "/ghost" {
				landed[rt] = true
			}
			w := send(t, h, req)
			if w.Code != c.code {
				t.Fatalf("got %d, want %d: %s", w.Code, c.code, w.Body)
			}
			if got := sent(w).Values("Motel-Version"); len(got) != 1 || got[0] != testRelease {
				t.Fatalf("Motel-Version %q, want %q", got, testRelease)
			}
			// The stream's case must have taken the streaming path,
			// whose headers go out with its first line.
			if ct := sent(w).Get("Content-Type"); c.accept != "" && ct != c.accept {
				t.Fatalf("Content-Type %q, want %q: the deploy did not stream", ct, c.accept)
			}
		})
	}
	for i := range routes {
		if !landed[&routes[i]] {
			t.Errorf("no case answers through the row %s %s", routes[i].method, routes[i].shape)
		}
	}

	t.Run("the 403", func(t *testing.T) {
		h, _ := newTestHandler(t)
		recordDecide(t, errors.New("refused"))
		w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
		if w.Code != http.StatusForbidden || sent(w).Get("Motel-Version") != testRelease {
			t.Fatalf("got %d, Motel-Version %q", w.Code, sent(w).Get("Motel-Version"))
		}
	})
}

// TestNoVersionOnTheFlat401: a refused token, an unknown name's and a
// known one's alike, gets the flat 401 with no Motel-Version, and its
// headers and body are exactly what a build with no release sends: the
// release tells a stranger nothing.
func TestNoVersionOnTheFlat401(t *testing.T) {
	for _, c := range []struct{ name, method, target, token string }{
		{"no token", "GET", "/demo", ""},
		{"no token, a POST", "POST", "/demo", ""},
		{"garbage", "GET", "/demo", "not-a-jwt"},
		{"another app's source", "GET", "/demo", "global"},
		{"an app's source on a name that is no app", "GET", "/ghost", "app"},
		{"another method", "DELETE", "/demo", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := func(release string) *httptest.ResponseRecorder {
				linkRelease(t, release)
				h, _ := newTestHandler(t)
				token := c.token
				switch token {
				case "app":
					token = appToken(t)
				case "global":
					token = globalToken(t)
				}
				return do(t, h, c.method, c.target, token, "")
			}
			before, after := answer(""), answer(testRelease)
			if after.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", after.Code)
			}
			want := http.Header{"Content-Type": {"application/json"}}
			if !maps.EqualFunc(sent(after), want, slices.Equal) || !maps.EqualFunc(sent(before), sent(after), slices.Equal) {
				t.Fatalf("headers %v, without a release %v, want %v", sent(after), sent(before), want)
			}
			if after.Body.String() != before.Body.String() {
				t.Fatalf("body %q, without a release %q", after.Body, before.Body)
			}
		})
	}
}

// TestNoVersionWithoutARelease: a build that links no release (go
// build, every other test) sends no Motel-Version, not an empty one.
func TestNoVersionWithoutARelease(t *testing.T) {
	linkRelease(t, "")
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	if _, ok := sent(w)["Motel-Version"]; ok {
		t.Fatalf("Motel-Version %q, want none", sent(w).Values("Motel-Version"))
	}
}
