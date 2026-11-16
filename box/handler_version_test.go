package box

import (
	"bytes"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// wantProtocol fails unless w is code with Motel-Protocol 1, once, and
// no header that names the release linked in.
func wantProtocol(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s: got %d, want %d: %s", what, w.Code, code, w.Body)
	}
	wantProtocolOnly(t, what, sent(w))
}

// wantProtocolOnly fails unless h carries Motel-Protocol 1, once, and
// no header that names the release linked in.
func wantProtocolOnly(t *testing.T, what string, h http.Header) {
	t.Helper()
	if got := h.Values("Motel-Protocol"); len(got) != 1 || got[0] != "1" {
		t.Errorf("%s: Motel-Protocol %q, want 1", what, got)
	}
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, testRelease) {
				t.Errorf("%s: header %s: %q names the release", what, k, v)
			}
		}
	}
}

// TestProtocolNotReleaseOnVerifiedAnswers: every answer to the box
// channel's token, with a release linked in, carries Motel-Protocol and
// no header names the release: `GET /` (200, and 409
// before a baseline and over a file that does not walk), each phase of a result poll and its 400 and 404,
// a push's 200, 202, 400, 409, 413, 415, 422 and 504, and the 403 Decide's
// refusal would give.
func TestProtocolNotReleaseOnVerifiedAnswers(t *testing.T) {
	linkRelease(t, testRelease)

	r := newRig(t)
	wantProtocol(t, "GET / before a baseline", r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict)
	r.applied(t, sha)
	r.write(t, r.installed, file("{\n}\nx.com {\n}\n")) // no box block: a file the channel cannot speak for
	wantProtocol(t, "GET / over a file that does not walk", r.do(t, req{target: "/", token: r.token(t)}), http.StatusConflict)

	p := newPushRig(t)
	wantProtocol(t, "GET /", p.get(t, "/", p.token(t, "")), http.StatusOK)

	id := randomID(t)
	wantProtocol(t, "a poll with no id", p.get(t, "/?", p.token(t, "")), http.StatusBadRequest)
	wantProtocol(t, "a poll for nothing", p.get(t, "/?result="+id, p.token(t, "")), http.StatusNotFound)
	p.markerAt(id, p.clock.Now().Add(-time.Hour))
	wantProtocol(t, "a poll, admitted", p.get(t, "/?result="+id, p.token(t, "")), http.StatusAccepted)
	for phase, code := range map[string]int{phaseApplied: http.StatusOK, phaseRefused: http.StatusUnprocessableEntity} {
		p.writeResult(t, result{ID: id, Phase: phase, Commit: p.base})
		wantProtocol(t, "a poll, "+phase, p.get(t, "/?result="+id, p.token(t, "")), code)
	}

	c2, bundle := p.change()
	wantProtocol(t, "a push with no id", p.post(t, p.h, push{token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusBadRequest)
	wantProtocol(t, "a push too large", p.post(t, p.h, push{id: randomID(t), length: maxBody + 1, token: p.token(t, c2), body: failingReader{t}}), http.StatusRequestEntityTooLarge)
	wantProtocol(t, "a push not gzip", p.post(t, p.h, push{id: randomID(t), ctype: "application/json", token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusUnsupportedMediaType)
	wantProtocol(t, "a push for another commit", p.post(t, p.h, push{id: randomID(t), token: p.token(t, p.base), body: bytes.NewReader(bundle)}), http.StatusUnprocessableEntity)
	wantProtocol(t, "a push of a known id", p.post(t, p.h, push{id: id, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusConflict)
	same := tgz(t, p.repo.bundleFiles(p.base, p.base))
	wantProtocol(t, "a push of the baseline", p.post(t, p.h, push{id: randomID(t), token: p.token(t, p.base), body: bytes.NewReader(same)}), http.StatusOK)
	waiting := randomID(t)
	wantProtocol(t, "a push nothing answers", p.post(t, p.h, push{id: waiting, token: p.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusGatewayTimeout)

	q := newPushRig(t)
	c2, bundle = q.change()
	verified := randomID(t)
	q.clk.onSleep = func() { q.writeResult(t, result{ID: verified, Phase: phaseVerified}) }
	wantProtocol(t, "a push verified", q.post(t, q.h, push{id: verified, token: q.token(t, c2), body: bytes.NewReader(bundle)}), http.StatusAccepted)

	recordDecide(t, errors.New("refused"))
	wantProtocol(t, "the 403", p.get(t, "/", p.token(t, "")), http.StatusForbidden)
}

// TestProtocolNotReleaseOnPersonAnswers: a person's answers from the
// box webhook on `/`, with a release linked in, carry Motel-Protocol
// and no header names the release: a viewer of the box's, and the 403
// to a person whose grants are on an app alone.
func TestProtocolNotReleaseOnPersonAnswers(t *testing.T) {
	linkRelease(t, testRelease)
	b := newPeopleBox(t)
	b.install()
	b.load()
	now := time.Now()
	for _, c := range []struct {
		name string
		code int
	}{
		{"carol@example.test", 0}, // the box's status, read from the box's own paths: whatever they hold
		{"alice@example.test", http.StatusForbidden},
	} {
		w := b.get("/", b.people[c.name].Token(peopleIdentity, now, time.Minute))
		if w.Code == http.StatusUnauthorized || c.code != 0 && w.Code != c.code {
			t.Errorf("%s GET /: %d %s, want %d", c.name, w.Code, w.Body, c.code)
		}
		wantProtocolOnly(t, c.name+" GET /", w.Result().Header)
	}
}

// TestNoMotelHeaderBeforeAToken: the flat 401 and the 405 that comes
// before authentication carry no header that names motel, whoever
// sends them, and their headers and bodies are exactly what a build
// with no release sends. Another path is passed on, untouched.
func TestNoMotelHeaderBeforeAToken(t *testing.T) {
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
