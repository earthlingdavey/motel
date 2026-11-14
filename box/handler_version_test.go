package box

import (
	"bytes"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

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

// wantRelease fails unless w is code with Motel-Version the release.
func wantRelease(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s: got %d, want %d: %s", what, w.Code, code, w.Body)
	}
	if got := sent(w).Values("Motel-Version"); len(got) != 1 || got[0] != testRelease {
		t.Errorf("%s: Motel-Version %q, want %q", what, got, testRelease)
	}
}

// TestVersionOnVerifiedAnswers: every answer to the box channel's
// token carries Motel-Version, the release: `GET /` (200, and 409
// before a baseline), each phase of a result poll and its 400 and 404,
// a push's 200, 202, 400, 409, 415, 422 and 504, and the 403 Decide's
// refusal would give.
func TestVersionOnVerifiedAnswers(t *testing.T) {
	linkRelease(t, testRelease)

	r := newRig(t)
	wantRelease(t, "GET / before a baseline", r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict)

	p := newPushRig(t)
	wantRelease(t, "GET /", p.get(t, "/", p.token(t, "")), http.StatusOK)

	id := randomID(t)
	wantRelease(t, "a poll with no id", p.get(t, "/?", p.token(t, "")), http.StatusBadRequest)
	wantRelease(t, "a poll for nothing", p.get(t, "/?result="+id, p.token(t, "")), http.StatusNotFound)
	p.markerAt(id, p.clock.Now().Add(-time.Hour))
	wantRelease(t, "a poll, admitted", p.get(t, "/?result="+id, p.token(t, "")), http.StatusAccepted)
	for phase, code := range map[string]int{phaseApplied: http.StatusOK, phaseRefused: http.StatusUnprocessableEntity} {
		p.writeResult(t, result{ID: id, Phase: phase, Commit: p.base})
		wantRelease(t, "a poll, "+phase, p.get(t, "/?result="+id, p.token(t, "")), code)
	}

	c2, bundle := p.change()
	wantRelease(t, "a push with no id", p.post(t, p.h, push{token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusBadRequest)
	wantRelease(t, "a push not gzip", p.post(t, p.h, push{id: randomID(t), ctype: "application/json", token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusUnsupportedMediaType)
	wantRelease(t, "a push for another commit", p.post(t, p.h, push{id: randomID(t), token: p.token(t, p.base), body: bytes.NewReader(bundle)}), http.StatusUnprocessableEntity)
	wantRelease(t, "a push of a known id", p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusConflict)
	same := tgz(t, p.repo.bundleFiles(p.base, p.base))
	wantRelease(t, "a push of the baseline", p.post(t, p.h, push{id: randomID(t), token: p.token(t, p.base), body: bytes.NewReader(same)}), http.StatusOK)
	waiting := randomID(t)
	wantRelease(t, "a push nothing answers", p.post(t, p.h, push{id: waiting, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusGatewayTimeout)

	q := newPushRig(t)
	c2, bundle = q.change()
	verified := randomID(t)
	q.clk.onSleep = func() { q.writeResult(t, result{ID: verified, Phase: phaseVerified}) }
	wantRelease(t, "a push verified", q.post(t, q.h, push{id: verified, token: q.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusAccepted)

	recordDecide(t, errors.New("refused"))
	wantRelease(t, "the 403", p.get(t, "/", p.token(t, "")), http.StatusForbidden)
}

// TestNoVersionBeforeAToken: the flat 401 and the 405 that comes
// before authentication carry no Motel-Version, whoever sends them,
// and their headers and bodies are exactly what a build with no
// release sends. Another path is passed on, untouched.
func TestNoVersionBeforeAToken(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		name, method, target string
		token                bool
		code                 int
		want                 http.Header
	}{
		{"GET / with no token", "GET", "/", false, http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}}},
		{"a poll with no token", "GET", "/?result=" + randomID(t), false, http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}}},
		{"a push with no token", "POST", "/", false, http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}}},
		{"PUT with no token", "PUT", "/", false, http.StatusMethodNotAllowed, http.Header{"Content-Type": {"application/json"}, "Allow": {"GET, POST"}}},
		{"PUT with the channel's token", "PUT", "/", true, http.StatusMethodNotAllowed, http.Header{"Content-Type": {"application/json"}, "Allow": {"GET, POST"}}},
		{"another path", "GET", "/x", true, http.StatusOK, http.Header{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := func(release string) *httptest.ResponseRecorder {
				linkRelease(t, release)
				q := req{method: c.method, target: c.target}
				if c.token {
					q.token = r.token(t)
				}
				return r.do(t, q)
			}
			before, after := answer(""), answer(testRelease)
			if after.Code != c.code {
				t.Fatalf("got %d, want %d: %s", after.Code, c.code, after.Body)
			}
			if !maps.EqualFunc(sent(after), c.want, slices.Equal) || !maps.EqualFunc(sent(before), sent(after), slices.Equal) {
				t.Fatalf("headers %v, without a release %v, want %v", sent(after), sent(before), c.want)
			}
			if after.Body.String() != before.Body.String() {
				t.Fatalf("body %q, without a release %q", after.Body, before.Body)
			}
		})
	}
}

// TestNoVersionWithoutARelease: a build that links no release sends no
// Motel-Version, not an empty one.
func TestNoVersionWithoutARelease(t *testing.T) {
	linkRelease(t, "")
	p := newPushRig(t)
	w := p.get(t, "/", p.token(t, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	if _, ok := sent(w)["Motel-Version"]; ok {
		t.Fatalf("Motel-Version %q, want none", sent(w).Values("Motel-Version"))
	}
}
