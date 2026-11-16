package liveswap

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"

	"github.com/smallhoursorg/motel/liveswap/access/accesstest"
	"github.com/smallhoursorg/motel/liveswap/wire"
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

// wantNoMotelHeader fails if any header of w's is named Motel-
// something, or names the release linked in.
func wantNoMotelHeader(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for k, vs := range sent(w) {
		if strings.HasPrefix(strings.ToLower(k), "motel-") {
			t.Errorf("header %s: %q, want no Motel- header", k, vs)
		}
		for _, v := range vs {
			if strings.Contains(v, testRelease) {
				t.Errorf("header %s: %q names the release", k, v)
			}
		}
	}
}

// TestNoMotelHeaderOnVerifiedAnswers: no answer to a token that
// verified, with a release linked in, carries a Motel- header or a
// header that names the release: each row of the route table, the
// 404 for a name that is no app, the 405 after authentication, the
// 400, 413 and 422s, a deploy stream (its headers go out with the first line),
// a deploy record's 500 and an app's 503 when no config describes it, a
// global source's 403s under /_motel/, and the 403 Decide's refusal
// would give.
func TestNoMotelHeaderOnVerifiedAnswers(t *testing.T) {
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
			wantNoMotelHeader(t, w)
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

	t.Run("a deploy record that does not read", func(t *testing.T) {
		h, rig := newTestHandler(t)
		dir, err := recordsDir(rig.spec.dirs, true)
		if err != nil {
			t.Fatal(err)
		}
		must(t, os.Mkdir(filepath.Join(dir, "v7.json"), 0o700))
		w := do(t, h, http.MethodGet, "/demo?deploy=v7", appToken(t), "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500: %s", w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
	})

	t.Run("an app no loaded config describes", func(t *testing.T) {
		// A reload between the token's check and the read: called on
		// the route's function, since no request can time it.
		h, rig := newTestHandler(t)
		rig.ma.specMu.Lock()
		rig.ma.spec = nil
		rig.ma.specMu.Unlock()
		w := httptest.NewRecorder()
		if err := h.deployRecord(w, rig.ma, "v1"); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("got %d, want 503: %s", w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
	})

	t.Run("a deploy source under /_motel/, refused and allowed", func(t *testing.T) {
		h, _ := newTestHandler(t)
		w := do(t, h, http.MethodGet, "/_motel/whoami", globalToken(t), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403: %s", w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
		// Decide allowing it is the table and the routes falling
		// apart: still a 403.
		recordDecide(t, nil)
		w = do(t, h, http.MethodGet, "/_motel/whoami", globalToken(t), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("Decide allowing: got %d, want 403: %s", w.Code, w.Body)
		}
		wantNoMotelHeader(t, w)
	})

	t.Run("the 403", func(t *testing.T) {
		h, _ := newTestHandler(t)
		recordDecide(t, errors.New("refused"))
		w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403", w.Code)
		}
		wantNoMotelHeader(t, w)
	})
}

// TestNoMotelHeaderOnTheFlat401: a refused token, an unknown name's and
// a known one's alike, gets the flat 401 with Content-Type its one
// header, so nothing names motel, and its headers and body are exactly
// what a build with no release sends.
func TestNoMotelHeaderOnTheFlat401(t *testing.T) {
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

// TestReleaseInWhoamiBodyAlone: with a release linked in, whoami's
// body names it, and no header of a person's answers does: whoami,
// the apps list, an app's status, Decide's 403, an unknown path's 404
// and a box viewer's 404 for a name that is no app carry no Motel-
// header either.
func TestReleaseInWhoamiBodyAlone(t *testing.T) {
	linkRelease(t, testRelease)
	people := newTestPeople(t)
	h, _ := newTestHandler(t)
	h.people = people.reg
	for _, c := range []struct {
		who    accesstest.Key
		target string
		code   int
	}{
		{people.alice, "/_motel/whoami", http.StatusOK},
		{people.alice, "/_motel/apps", http.StatusOK},
		{people.alice, "/demo", http.StatusOK},
		{people.alice, "/blog", http.StatusForbidden},
		{people.alice, "/_motel/nothing", http.StatusNotFound},
		{people.carol, "/ghost", http.StatusNotFound}, // a viewer of the box, told a name is no app
	} {
		w := sendTo(t, h, "GET", c.target, token(c.who))
		if w.Code != c.code {
			t.Fatalf("GET %s: %d %s, want %d", c.target, w.Code, w.Body, c.code)
		}
		wantNoMotelHeader(t, w)
		if c.target == "/_motel/whoami" {
			var me wire.WhoAmI
			if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || me.Version != testRelease {
				t.Errorf("whoami's version %q (%v), want %q", me.Version, err, testRelease)
			}
		}
	}
}
