package liveswap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// newTestHandler wires a Handler to an App backed by one fake-driven
// managed app, bypassing caddy.Context entirely. The app trusts
// appTestPub for its own deploys; the App's global trust (for the
// unknown-app path) is globalTestPub.
func newTestHandler(t *testing.T) (*Handler, *testRig) {
	t.Helper()
	rig := newTestRig(t)
	app := &App{
		managed:         map[string]*managedApp{"demo": rig.ma},
		globalVerifiers: deploytrust.Verifiers([]deploytrust.Source{localSource(t, globalTestPub, "global")}, nil),
	}
	h := &Handler{app: app, logger: zap.NewNop(), limiter: deploytrust.NewLimiter(rig.clock)}
	return h, rig
}

// do sends a request with token as the Authorization bearer (empty =
// no Authorization header).
func do(t *testing.T, h *Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	reader := strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return w
}

func TestWebhookBearerAuth(t *testing.T) {
	h, _ := newTestHandler(t)
	valid := appToken(t)
	bearer := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/demo", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
		if err := h.ServeHTTP(w, req, next); err != nil {
			t.Fatalf("ServeHTTP returned error: %v", err)
		}
		return w
	}

	if w := bearer("Bearer " + valid); w.Code != http.StatusOK {
		t.Errorf("valid Bearer: got %d, want 200", w.Code)
	}
	if w := bearer("bearer " + valid); w.Code != http.StatusOK {
		t.Errorf("scheme is case-insensitive: got %d, want 200", w.Code)
	}
	if w := bearer("Bearer not-a-jwt"); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage Bearer: got %d, want 401", w.Code)
	}
	if w := bearer("Basic " + valid); w.Code != http.StatusUnauthorized {
		t.Errorf("non-Bearer scheme: got %d, want 401", w.Code)
	}
	if w := bearer("Bearer"); w.Code != http.StatusUnauthorized {
		t.Errorf("empty Bearer: got %d, want 401", w.Code)
	}
}

func TestWebhookRejectsBadToken(t *testing.T) {
	h, _ := newTestHandler(t)
	for name, token := range map[string]string{"missing": "", "garbage": "not-a-jwt"} {
		t.Run(name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", token, `{"url":"https://x/a.tgz","version":"v1"}`)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", w.Code)
			}
		})
	}
}

func TestWebhookUnknownAppIs404OnlyWhenAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t)
	// Garbage token + unknown app: still 401, no name enumeration.
	w := do(t, h, http.MethodPost, "/ghost", "not-a-jwt", "{}")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated unknown app must 401, got %d", w.Code)
	}
	// Valid global token + unknown app: 404.
	w = do(t, h, http.MethodPost, "/ghost", globalToken(t), "{}")
	if w.Code != http.StatusNotFound {
		t.Fatalf("authenticated unknown app must 404, got %d", w.Code)
	}
}

func TestWebhookDeployHappyPath(t *testing.T) {
	h, rig := newTestHandler(t)
	tok := trusttest.Mint(t, appTestPriv, "demo", map[string]string{"sub": "alice"})
	w := do(t, h, http.MethodPost, "/demo", tok, `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var status statusSnapshot
	must(t, json.Unmarshal(w.Body.Bytes(), &status))
	if status.CurrentVersion != "v1" || !status.Running {
		t.Fatalf("response status wrong: %+v", status)
	}
	if rig.ma.activeSocket.Load() == nil {
		t.Fatal("deploy did not publish a port")
	}
	// The status records who authorized the deploy: the trust source,
	// and the subject its token names.
	// The source's label is the test key's path.
	by := func(got string) bool {
		return strings.HasPrefix(got, "local:") && strings.HasSuffix(got, "/deploy.pub sub=alice")
	}
	if status.LastDeploy == nil || !by(status.LastDeploy.By) {
		t.Fatalf("deployed_by = %+v, want the local source and sub=alice", status.LastDeploy)
	}
	// So does the version's record.
	if len(status.Deploys) == 0 || !by(status.Deploys[0].By) {
		t.Fatalf("recorded deployed_by = %+v, want the local source and sub=alice", status.Deploys)
	}
}

func TestWebhookUsesPerAppTrust(t *testing.T) {
	h, _ := newTestHandler(t)
	// The app trusts appTestPub (audience "demo"); a token valid only
	// under the GLOBAL trust must NOT authenticate against a known app.
	w := do(t, h, http.MethodPost, "/demo", globalToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("global token must not open a per-app-trust app, got %d", w.Code)
	}
}

func TestWebhookValidatesPayload(t *testing.T) {
	h, _ := newTestHandler(t)
	const pinShape = "sha256 must match " // the 422 quotes the pattern, as version's does
	cases := []struct {
		name     string
		body     string
		want     int
		wantBody string // in the response, when set
	}{
		{"bad json", "{nope", http.StatusBadRequest, ""},
		{"missing url", `{"version":"v1"}`, http.StatusUnprocessableEntity, ""},
		{"missing version", `{"url":"https://x/a.tgz"}`, http.StatusUnprocessableEntity, ""},
		{"evil version", `{"url":"https://x/a.tgz","version":"../../etc"}`, http.StatusUnprocessableEntity, ""},
		{"auth_header with CRLF", `{"url":"https://x/a.tgz","version":"v1","auth_header":"Bearer a\r\nX-Evil: 1"}`, http.StatusUnprocessableEntity, ""},
		// A pin is the digest as sha256sum prints it: 64 hex characters,
		// nothing else, and a refusal names the field. (The accepted
		// forms reach the fetcher in TestWebhookURLDeployForwardsWireFields.)
		{"sha256 too short", `{"url":"https://x/a.tgz","version":"v1","sha256":"` + strings.Repeat("a", 63) + `"}`, http.StatusUnprocessableEntity, pinShape},
		{"sha256 too long", `{"url":"https://x/a.tgz","version":"v1","sha256":"` + strings.Repeat("a", 65) + `"}`, http.StatusUnprocessableEntity, pinShape},
		{"sha256 not hex", `{"url":"https://x/a.tgz","version":"v1","sha256":"` + strings.Repeat("g", 64) + `"}`, http.StatusUnprocessableEntity, pinShape},
		{"sha256 prefixed", `{"url":"https://x/a.tgz","version":"v1","sha256":"sha256:` + strings.Repeat("a", 57) + `"}`, http.StatusUnprocessableEntity, pinShape},
		{"sha256 not a string", `{"url":"https://x/a.tgz","version":"v1","sha256":1}`, http.StatusBadRequest, ""},
		{"sha256 empty is no pin", `{"url":"https://x/a.tgz","version":"v1","sha256":""}`, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, h, http.MethodPost, "/demo", appToken(t), tc.body)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("body = %s, want it to carry %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}

// A refused pin's 422 names both digests as they are: real ones, which
// the filter's entropy layer would otherwise mask as tokens. Both are
// names (the deployer's own pin, and the hash of what a host served),
// never secrets. (No record to check: a refusal of the request's own
// content is not the version's history — see the deploy pipeline.)
func TestWebhookRefusedPinNamesBothDigests(t *testing.T) {
	h, rig := newTestHandler(t)
	digest := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	pinned, got := digest("what CI built"), digest("what the host served")
	rig.fetch.err = digestMismatch{pinned: pinned, got: got}
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1","sha256":"`+pinned+`"}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "request pinned "+pinned+", downloaded "+got) {
		t.Fatalf("response = %d %s; want a 422 naming both digests", w.Code, w.Body.String())
	}
}

// A pinned pull names its digest in the deploy authorized line; an
// unpinned one has no field to name.
func TestWebhookDeployAuthorizedNamesThePin(t *testing.T) {
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.InfoLevel)
	h.logger = zap.New(core)
	pin := strings.Repeat("ab", 32)
	for _, tc := range []struct{ body, want string }{
		{`{"url":"https://x/a.tgz","version":"v1","sha256":"` + strings.ToUpper(pin) + `"}`, pin},
		{`{"url":"https://x/a.tgz","version":"v2"}`, ""},
	} {
		logs.TakeAll()
		if w := do(t, h, http.MethodPost, "/demo", appToken(t), tc.body); w.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
		}
		var authorized []observer.LoggedEntry
		for _, e := range logs.All() {
			if e.Message == "deploy authorized" {
				authorized = append(authorized, e)
			}
		}
		if len(authorized) != 1 {
			t.Fatalf("logged %d deploy authorized lines, want one: %+v", len(authorized), logs.All())
		}
		got, has := authorized[0].ContextMap()["sha256"]
		if has != (tc.want != "") || (has && got != tc.want) {
			t.Fatalf("sha256 field = %v (present %v), want %q", got, has, tc.want)
		}
	}
}

// A pinned deploy's digest is on record wherever the record is: the
// response's last_deploy, the status's deploys list, the record read
// back, and the done line of a stream — whole, though a real digest is
// what the filter's entropy layer masks. An unpinned deploy has no
// field, and a rollback carries the pin of the record it replaces.
func TestWebhookDeployRecordNamesThePin(t *testing.T) {
	h, _ := newTestHandler(t)
	digest := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	pin := digest("what CI built")
	if body := NewRedactor(nil, nil).redactJSON([]byte(`{"sha256":"` + pin + `"}`)); strings.Contains(body, pin) {
		t.Fatalf("control: an unnamed digest must be masked, or this test proves nothing: %s", body)
	}
	pinOf := func(raw []byte) (any, bool) {
		var m map[string]any
		must(t, json.Unmarshal(raw, &m))
		v, has := m["sha256"]
		return v, has
	}
	lastDeploy := func(w *httptest.ResponseRecorder) json.RawMessage {
		var m struct {
			LastDeploy json.RawMessage `json:"last_deploy"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &m))
		return m.LastDeploy
	}

	// The upper-case pin is recorded as parseDeployPayload normalised it.
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1","sha256":"`+strings.ToUpper(pin)+`"}`)
	if got, _ := pinOf(lastDeploy(w)); w.Code != 200 || got != pin {
		t.Fatalf("pinned deploy: %d last_deploy.sha256 = %v, want %s: %s", w.Code, got, pin, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/demo", appToken(t), "")
	var st struct {
		Deploys []json.RawMessage `json:"deploys"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &st))
	if len(st.Deploys) != 1 {
		t.Fatalf("status deploys = %s", w.Body.String())
	}
	if got, _ := pinOf(st.Deploys[0]); got != pin {
		t.Fatalf("status deploys[0].sha256 = %v, want %s: %s", got, pin, w.Body.String())
	}
	if got, _ := pinOf(lastDeploy(w)); got != pin {
		t.Fatalf("status last_deploy.sha256 = %v, want %s", got, pin)
	}
	w = do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	if got, _ := pinOf(w.Body.Bytes()); w.Code != 200 || got != pin {
		t.Fatalf("record v1: %d sha256 = %v, want %s: %s", w.Code, got, pin, w.Body.String())
	}

	// Unpinned: absent, not a placeholder.
	w = do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v2"}`)
	if _, has := pinOf(lastDeploy(w)); w.Code != 200 || has {
		t.Fatalf("unpinned deploy: %d, last_deploy has sha256: %s", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/demo?deploy=v2", appToken(t), "")
	if _, has := pinOf(w.Body.Bytes()); w.Code != 200 || has {
		t.Fatalf("record v2: %d, has sha256: %s", w.Code, w.Body.String())
	}

	// A rollback to v1 replaces v1's record and keeps its pin; one to
	// v2 has none to keep.
	for _, tc := range []struct {
		version string
		want    any
	}{{"v1", pin}, {"v2", nil}} {
		w = do(t, h, http.MethodPost, "/demo?rollback="+tc.version, appToken(t), "")
		if got, _ := pinOf(lastDeploy(w)); w.Code != 200 || got != tc.want {
			t.Fatalf("rollback %s: %d last_deploy.sha256 = %v, want %v: %s", tc.version, w.Code, got, tc.want, w.Body.String())
		}
		w = do(t, h, http.MethodGet, "/demo?deploy="+tc.version, appToken(t), "")
		if got, _ := pinOf(w.Body.Bytes()); got != tc.want {
			t.Fatalf("record %s after rollback: sha256 = %v, want %v: %s", tc.version, got, tc.want, w.Body.String())
		}
	}

	// The done line of a streamed deploy.
	_, lines := streamLinesOf(t, h, `{"url":"https://x/a.tgz","version":"v3","sha256":"`+pin+`"}`)
	last := lines[len(lines)-1]
	ld, _ := last["last_deploy"].(map[string]any)
	if last["event"] != "done" || ld == nil || ld["sha256"] != pin {
		t.Fatalf("done line = %v", last)
	}
}

func TestWebhookConflictWhileDeploying(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func TestWebhookDeployFailureReturns500WithOldStatus(t *testing.T) {
	h, rig := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	rig.prober.err = errTest
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("500 body should show the still-serving version: %s", w.Body.String())
	}
}

func TestWebhookGetStatus(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"current_version":"v1"`) {
		t.Fatalf("status GET wrong: %d %s", w.Code, w.Body.String())
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodDelete, "/demo", appToken(t), "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w.Code)
	}
}

// The app name is the LAST path segment, so mounting under a prefix
// like handle /deploy/* works.
func TestWebhookAppNameFromLastSegment(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/deploy/demo", appToken(t), "")
	if w.Code != http.StatusOK {
		t.Fatalf("prefixed path should resolve the app, got %d", w.Code)
	}
}

// from sends a request as the given remote address.
func from(t *testing.T, h *Handler, remote, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/demo", nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return send(t, h, req)
}

// After the failure budget a client's bad tokens are no longer
// logged, and the answer does not change: the same status, headers and
// body as the first refusal, so a caller cannot see the budget and has
// no reason to retry. A valid token from that address is still
// admitted (and clears it): sharing an address with a flood costs log
// lines, never a deploy.
func TestWebhookThrottlesAuthFailures(t *testing.T) {
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	const attacker = "203.0.113.9:1"
	type answer struct {
		code   int
		header http.Header
		body   string
	}
	refuse := func() answer {
		t.Helper()
		w := from(t, h, attacker, "not-a-jwt")
		return answer{w.Code, w.Header(), w.Body.String()}
	}
	first := refuse()
	if first.code != http.StatusUnauthorized || strings.TrimSpace(first.body) != flat401 {
		t.Fatalf("first failure: %d %s, want the flat 401", first.code, first.body)
	}
	for i := 1; i < deploytrust.FailBudget; i++ {
		refuse()
	}
	logs.TakeAll()
	for i := range 3 {
		if got := refuse(); !reflect.DeepEqual(got, first) {
			t.Fatalf("past the budget, refusal %d answered %+v; want the first refusal's %+v", i+1, got, first)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("failures past the budget were logged: %+v", logs.All())
	}
	// Another address is unaffected.
	if w := from(t, h, "198.51.100.1:1", appToken(t)); w.Code != http.StatusOK {
		t.Fatalf("another address: code = %d, want 200", w.Code)
	}
	// The throttled address with a valid token deploys — and is
	// cleared by it, so the full budget is back.
	if w := from(t, h, attacker, appToken(t)); w.Code != http.StatusOK {
		t.Fatalf("a valid token from a throttled address: code = %d, want 200", w.Code)
	}
	logs.TakeAll()
	for range deploytrust.FailBudget + 1 {
		refuse()
	}
	if got := logs.Len(); got != deploytrust.FailBudget+1 {
		t.Fatalf("after success, logged %d records; want the full budget and the throttled line", got)
	}
	// And the window slides on its own.
	logs.TakeAll()
	rig.clock.Advance(deploytrust.FailWindow + time.Second)
	if got := refuse(); !reflect.DeepEqual(got, first) || logs.Len() != 1 {
		t.Fatalf("after the window: %+v and %d records; want the flat 401, logged", got, logs.Len())
	}
}

// The throttle is what bounds the log: a flood of N failures from one
// address writes budget+1 records (the failures, and one line saying
// the address is now throttled), a request-supplied name is cut to
// what a real app name could be — and the bound holds for a flood
// that arrives all at once, not just one at a time.
func TestWebhookThrottleBoundsTheLog(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "serial", true: "concurrent"}[concurrent], func(t *testing.T) {
			h, _ := newTestHandler(t)
			core, logs := observer.New(zap.WarnLevel)
			h.logger = zap.New(core)
			const flood = deploytrust.FailBudget + 50
			hit := func() {
				req := httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("x", 10_000), nil)
				req.RemoteAddr = "203.0.113.9:1"
				req.Header.Set("Authorization", "Bearer not-a-jwt")
				send(t, h, req)
			}
			if concurrent {
				var wg sync.WaitGroup
				for range flood {
					wg.Go(hit)
				}
				wg.Wait()
			} else {
				for range flood {
					hit()
				}
			}
			if got := logs.Len(); got != deploytrust.FailBudget+1 {
				t.Fatalf("logged %d Warn records for %d failures, want %d", got, flood, deploytrust.FailBudget+1)
			}
			for _, e := range logs.All() {
				for _, f := range e.Context {
					if f.Key == "app" && len(f.String) > appNameMaxLen+3 {
						t.Fatalf("app field is %d bytes; the name must be truncated", len(f.String))
					}
					// One entry per source, joined by "; ", each its label (the
					// operator's config) and a bounded reason: the unknown-app
					// path is refused by the global sources.
					bound := 0
					for _, v := range h.app.globalVerifiers {
						bound += len(v.Label()) + 2 + deploytrust.MaxRefusalLen + 3 + 2
					}
					if f.Key == "refused" && len(f.String) > bound {
						t.Fatalf("refused field is %d bytes; each source's reason must be bounded", len(f.String))
					}
				}
			}
		})
	}
}

// flat401 is the body every refusal answers with, JSON-encoded as
// RespondJSON writes it.
const flat401 = `{"error":"invalid or missing deploy token (Authorization: Bearer \u003cjwt\u003e)"}`

// A refused token leaves its reason in the journal — which source, and
// what failed — on the line the limiter already governs, and nowhere
// else: the 401 body is the same flat sentence whatever the reason, so
// an unauthenticated caller learns nothing about which apps exist or
// what a source pins.
func TestWebhookAuthFailureSaysWhyInTheJournalOnly(t *testing.T) {
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	for _, tc := range []struct {
		name, path, token, want string
	}{
		{"no header", "/demo", "", "no bearer token"},
		{"garbage", "/demo", "not-a-jwt", "local:"},
		{"wrong audience", "/demo", trusttest.Mint(t, appTestPriv, "other", nil), "aud"},
		{"unknown app", "/nope", "not-a-jwt", "local:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.TakeAll()
			w := do(t, h, http.MethodGet, tc.path, tc.token, "")
			if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != flat401 {
				t.Fatalf("response = %d %s; want the flat 401", w.Code, w.Body.String())
			}
			all := logs.All()
			if len(all) != 1 || all[0].Message != "webhook auth failed" {
				t.Fatalf("logged %d records, want the one auth-failed line: %+v", len(all), all)
			}
			refused, ok := all[0].ContextMap()["refused"].(string)
			if !ok || !strings.Contains(refused, tc.want) {
				t.Fatalf("refused = %q, want it to carry %q", refused, tc.want)
			}
		})
	}
}

// A flood from many addresses runs into the process-wide budget: past
// it, failures still 401 but the journal gets one line saying so and
// nothing more.
func TestWebhookThrottleIsBoundedAcrossAddresses(t *testing.T) {
	h, _ := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	for i := range deploytrust.FailGlobalBudget + 200 {
		w := from(t, h, "10."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256)+".1:1", "not-a-jwt")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("address %d: code = %d, want 401", i, w.Code)
		}
	}
	if got := logs.Len(); got != deploytrust.FailGlobalBudget+1 {
		t.Fatalf("logged %d Warn records, want the global budget plus one", got)
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "test error" }

// An oversized payload must get an honest 413, not a misleading
// "invalid JSON" 400 from silent truncation at the cap.
func TestWebhookOversizedPayloadIs413(t *testing.T) {
	h, _ := newTestHandler(t)
	big := `{"url":"https://x/a.tgz","version":"v1","pad":"` +
		strings.Repeat("a", maxPayloadBytes) + `"}`
	w := do(t, h, http.MethodPost, "/demo", appToken(t), big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payload exceeds") {
		t.Fatalf("body should name the limit: %s", w.Body.String())
	}
}

// send runs one request through the handler.
func send(t *testing.T, h *Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	return w
}

func gzipDeploy(t *testing.T, target, token, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	return req
}

func TestWebhookPushRoutesAndDeploys(t *testing.T) {
	h, rig := newTestHandler(t)
	w := send(t, h, gzipDeploy(t, "/demo?version=v9", appToken(t), "tarball-bytes"))
	if w.Code != http.StatusOK {
		t.Fatalf("push: got %d %s", w.Code, w.Body.String())
	}
	got := rig.fetch.lastReq
	if got.source() != "push" || got.localArchive == "" || got.version != "v9" {
		t.Fatalf("push not routed correctly: %+v", got)
	}
}

func TestWebhookPushRequiresValidVersion(t *testing.T) {
	h, _ := newTestHandler(t)
	// missing version
	if w := send(t, h, gzipDeploy(t, "/demo", appToken(t), "x")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing version: got %d", w.Code)
	}
	// traversal version
	if w := send(t, h, gzipDeploy(t, "/demo?version=../etc", appToken(t), "x")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("evil version: got %d", w.Code)
	}
}

func TestWebhookPushOversizedIs413(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.maxArtifactSize = 8 // tiny cap for the test
	w := send(t, h, gzipDeploy(t, "/demo?version=v1", appToken(t), "way past eight bytes"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized push: got %d, want 413", w.Code)
	}
}

func TestWebhookRollbackRoutes(t *testing.T) {
	h, rig := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?rollback=v3", nil)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	w := send(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rollback: got %d %s", w.Code, w.Body.String())
	}
	got := rig.fetch.lastReq
	if got.source() != "rollback" || !got.rollback || got.version != "v3" {
		t.Fatalf("rollback not routed correctly: %+v", got)
	}
}

func TestWebhookRollbackRequiresValidVersion(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?rollback=..", nil)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	if w := send(t, h, req); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("evil rollback version: got %d", w.Code)
	}
}

func TestWebhookStatusListsAvailableVersions(t *testing.T) {
	h, _ := newTestHandler(t)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/1.tgz","version":"v1"}`)
	do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/2.tgz","version":"v2"}`)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	var status statusSnapshot
	must(t, json.Unmarshal(w.Body.Bytes(), &status))
	if len(status.AvailableVersions) != 2 {
		t.Fatalf("available_versions = %v, want two entries", status.AvailableVersions)
	}
	set := map[string]bool{}
	for _, v := range status.AvailableVersions {
		set[v] = true
	}
	if !set["v1"] || !set["v2"] {
		t.Fatalf("available_versions missing a release: %v", status.AvailableVersions)
	}
}

// trackReader records whether its body was read.
type trackReader struct{ read bool }

func (t *trackReader) Read(p []byte) (int, error) { t.read = true; return 0, io.EOF }

func TestWebhookPushLocksBeforeStaging(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.ma.deployMu.Lock() // simulate an in-progress deploy
	defer rig.ma.deployMu.Unlock()
	tr := &trackReader{}
	req := httptest.NewRequest(http.MethodPost, "/demo?version=v1", tr)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	req.Header.Set("Content-Type", "application/gzip")
	w := send(t, h, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("push during an in-progress deploy should be 409, got %d", w.Code)
	}
	if tr.read {
		t.Fatal("push body must not be staged before the deploy lock is acquired")
	}
}

// A push into an app dir whose tmp/ is a link is refused before the
// upload is read or staged (#300): nothing lands where the link
// points, the refusal says why, and the journal says who asked.
func TestWebhookPushRefusesALinkedTmpBeforeStaging(t *testing.T) {
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.ErrorLevel)
	rig.ma.logger = zap.New(core)
	d := rig.spec.dirs
	must(t, d.ensure())
	elsewhere := t.TempDir()
	must(t, os.Remove(d.tmp))
	must(t, os.Symlink(elsewhere, d.tmp))
	tr := &trackReader{}
	req := httptest.NewRequest(http.MethodPost, "/demo?version=v9", tr)
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	req.Header.Set("Content-Type", "application/gzip")
	w := send(t, h, req)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "refusing to deploy") {
		t.Fatalf("push through a linked tmp/: got %d %s", w.Code, w.Body.String())
	}
	if tr.read {
		t.Fatal("the upload was read before the refusal")
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Fatalf("staged where the link points: %v %v", entries, err)
	}
	got := logs.FilterMessage("deploy failed").All()
	if len(got) != 1 || got[0].ContextMap()["source"] != "push" || got[0].ContextMap()["via"] == "" || got[0].ContextMap()["remote"] != req.RemoteAddr {
		t.Fatalf("want one journal line naming the push, who and from where, got %v", got)
	}
}

func TestWebhookAvailableVersionsIsArrayWhenEmpty(t *testing.T) {
	h, _ := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	if !strings.Contains(w.Body.String(), `"available_versions":[]`) {
		t.Fatalf("empty available_versions should serialize as [], not null/absent: %s", w.Body.String())
	}
}

func TestWebhookPushContentTypeCaseInsensitive(t *testing.T) {
	h, rig := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?version=v1", strings.NewReader("tarball"))
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	req.Header.Set("Content-Type", "Application/GZIP; charset=x") // mixed case + parameter
	w := send(t, h, req)
	if w.Code != http.StatusOK {
		t.Fatalf("mixed-case gzip Content-Type must route to push (200), got %d %s", w.Code, w.Body.String())
	}
	if rig.fetch.lastReq.source() != "push" {
		t.Fatalf("expected push routing, got %q", rig.fetch.lastReq.source())
	}
}

// TestDeployPayloadIsTheOnlyWireType pins the deployPayload /
// deployRequest split structurally (see deployPayload): the wire type
// carries exactly the four body fields, and the request type has no
// exported field at all — encoding/json cannot set an unexported
// field, so decoding a body into a deployRequest is a no-op by
// language rule, not by convention. Reverting to one decoded struct,
// or exporting a request field, fails here rather than in a CodeQL
// alert. (rollback and version still arrive, validated, via the query
// string; that path builds the request as a literal.)
func TestDeployPayloadIsTheOnlyWireType(t *testing.T) {
	wire := map[string]string{}
	pt := reflect.TypeFor[deployPayload]()
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		wire[f.Name] = f.Tag.Get("json")
	}
	want := map[string]string{"URL": "url", "Version": "version", "AuthHeader": "auth_header", "SHA256": "sha256"}
	if !reflect.DeepEqual(wire, want) {
		t.Fatalf("deployPayload wire fields = %v, want %v", wire, want)
	}
	rt := reflect.TypeFor[deployRequest]()
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.IsExported() || f.Tag.Get("json") != "" {
			t.Fatalf("deployRequest.%s is exported or tagged (%q); the request type must not be decodable from the wire", f.Name, f.Tag)
		}
	}
	// And the property itself, end to end: a body naming every field
	// by its wire name (and its Go name) leaves a deployRequest zero.
	var req deployRequest
	body := `{"url":"https://x/a.tgz","version":"v1","auth_header":"Bearer t","sha256":"` + strings.Repeat("a", 64) + `",` +
		`"URL":"https://x/a.tgz","Version":"v1","AuthHeader":"Bearer t","SHA256":"` + strings.Repeat("a", 64) + `",` +
		`"localArchive":"/etc/passwd","rollback":true,"by":"forged"}`
	must(t, json.Unmarshal([]byte(body), &req)) //nolint:staticcheck // SA9005 is the property under test: nothing on the wire can land in deployRequest
	if !reflect.DeepEqual(req, deployRequest{}) {
		t.Fatalf("a body reached deployRequest fields: %+v", req)
	}
}

// TestWebhookURLDeployForwardsWireFields is the positive half of the
// split: every wire field a caller sends reaches the fetcher, and the
// server-side ones stay zero however the body spells them.
func TestWebhookURLDeployForwardsWireFields(t *testing.T) {
	h, rig := newTestHandler(t)
	body := `{"url":"https://x/private.tgz","version":"v7","auth_header":"Bearer artifact-token","sha256":"` + strings.Repeat("Ab", 32) + `",` +
		`"localArchive":"/etc/passwd","local_archive":"/etc/passwd","rollback":true}`
	w := do(t, h, http.MethodPost, "/demo", appToken(t), body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got := rig.fetch.lastReq
	// The pin reaches the fetcher in the form the download compares
	// against: lowercase.
	if got.url != "https://x/private.tgz" || got.version != "v7" || got.authHeader != "Bearer artifact-token" || got.sha256 != strings.Repeat("ab", 32) {
		t.Fatalf("wire fields did not reach the fetcher: %+v", got)
	}
	if got.localArchive != "" || got.rollback {
		t.Fatalf("server-side fields reachable from the body: %+v", got)
	}
}

// streamLines runs a v1 URL deploy with Accept: application/x-ndjson
// and returns the response and its lines, each parsed.
func streamLines(t *testing.T, h *Handler) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	return streamLinesOf(t, h, `{"url":"https://example.test/v1.tgz","version":"v1"}`)
}

// streamLinesOf is streamLines for a deploy payload of the caller's.
func streamLinesOf(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/demo", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	req.Header.Set("Accept", "application/x-ndjson")
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", raw, err)
		}
		lines = append(lines, m)
	}
	return w, lines
}

// With Accept: application/x-ndjson the deploy is one JSON line per
// phase as it happens, then the single response's body with
// "event":"done" and the status code it would have had.
func TestWebhookDeployStreamsPhases(t *testing.T) {
	h, _ := newTestHandler(t)
	w, lines := streamLines(t, h)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status %d, content-type %q", w.Code, w.Header().Get("Content-Type"))
	}
	var phases []string
	for _, l := range lines[:len(lines)-1] {
		if l["event"] != "phase" {
			t.Fatalf("not a phase line: %v", l)
		}
		phases = append(phases, l["phase"].(string))
	}
	if got := strings.Join(phases, ","); !strings.HasPrefix(got, "downloading,extracting,") || !strings.Contains(got, "promoting") {
		t.Fatalf("phases streamed = %s", got)
	}
	last := lines[len(lines)-1]
	if last["event"] != "done" || last["http_status"] != float64(200) || last["current_version"] != "v1" {
		t.Fatalf("last line = %v", last)
	}
}

// A failure streams as it would have responded: the last line is the
// 500 body — error and the old status — with http_status 500, on a
// 200 stream.
func TestWebhookDeployStreamCarriesTheFailure(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.runner.startErr = errors.New("boom")
	w, lines := streamLines(t, h)
	last := lines[len(lines)-1]
	if w.Code != 200 || last["event"] != "done" || last["http_status"] != float64(500) || !strings.Contains(last["error"].(string), "boom") || last["status"] == nil {
		t.Fatalf("status %d, last line = %v", w.Code, last)
	}
	if lines[0]["event"] != "phase" || lines[0]["phase"] != "downloading" {
		t.Fatalf("first line = %v", lines[0])
	}
}

// Without the Accept header nothing changes: one body, the real code.
func TestWebhookDeployWithoutAcceptIsUnchanged(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.runner.startErr = errors.New("boom")
	w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://example.test/v1.tgz","version":"v1"}`)
	if w.Code != 500 || w.Header().Get("Content-Type") != "application/json" || strings.Contains(w.Body.String(), `"event"`) {
		t.Fatalf("status %d, content-type %q, body %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

// A push streams too: the lock is taken and the upload staged before
// any phase, then the pipeline's phases and outcome are lines.
func TestWebhookPushStreams(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/demo?version=v1", strings.NewReader("not-really-gzip"))
	req.Header.Set("Authorization", "Bearer "+appToken(t))
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Accept", "application/x-ndjson")
	w := httptest.NewRecorder()
	var next caddyhttp.Handler = caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })
	if err := h.ServeHTTP(w, req, next); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || !strings.Contains(lines[0], `"phase":"downloading"`) || !strings.Contains(lines[len(lines)-1], `"http_status":200`) {
		t.Fatalf("status %d, content-type %q, body %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

// An outcome reached before the first phase — here a version the box
// already has — has nothing streamed yet, and is the single response
// with its real code, so a client's --fail-with-body catches it.
func TestWebhookStreamRefusedBeforeAnyPhaseKeepsItsCode(t *testing.T) {
	h, _ := newTestHandler(t)
	if w, _ := streamLines(t, h); w.Code != 200 {
		t.Fatalf("first deploy: %d %s", w.Code, w.Body.String())
	}
	w, lines := streamLines(t, h)
	if w.Code != 422 || w.Header().Get("Content-Type") != "application/json" || len(lines) != 1 || lines[0]["event"] != nil || !strings.Contains(lines[0]["error"].(string), "already running") {
		t.Fatalf("status %d, content-type %q, lines %v", w.Code, w.Header().Get("Content-Type"), lines)
	}
}

// The stream is asked for by the media type exactly, in any Accept
// header sent: a list, a parameter, a second header; not a lookalike.
func TestWantsStreamReadsAcceptExactly(t *testing.T) {
	cases := []struct {
		accept []string
		want   bool
	}{
		{[]string{"application/x-ndjson"}, true},
		{[]string{"Application/X-NDJSON; q=0.9"}, true},
		{[]string{"application/json, application/x-ndjson"}, true},
		{[]string{"application/json", "application/x-ndjson"}, true},
		{[]string{"application/x-ndjson-backup"}, false},
		{[]string{"application/x-ndjson;q=0"}, false},
		{[]string{"application/x-ndjson;q=0.0, application/json"}, false},
		{[]string{"application/x-ndjson;q=abc"}, false},
		{[]string{"application/x-ndjson;q=NaN"}, false},
		{[]string{"application/x-ndjson;q=+Inf"}, false},
		{[]string{"application/x-ndjson;q=2"}, false},
		{[]string{"application/x-ndjson;q=1"}, true},
		{[]string{"application/json"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/demo", nil)
		for _, a := range tc.accept {
			r.Header.Add("Accept", a)
		}
		if got := wantsStream(r); got != tc.want {
			t.Errorf("Accept %q: wantsStream = %v, want %v", tc.accept, got, tc.want)
		}
	}
}

// A filter that withholds the whole body — the app's env_file cannot
// be read, so its values are unknown — still leaves the last line's
// markers in place: the outcome is appended after the filter.
func TestWebhookStreamLastLineSurvivesWithholding(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "missing.env")
	w, lines := streamLines(t, h)
	last := lines[len(lines)-1]
	if w.Code != 200 || last["event"] != "done" || last["http_status"] != float64(500) {
		t.Fatalf("status %d, last line = %v", w.Code, last)
	}
	if _, withheld := last["status"]; withheld {
		t.Fatalf("the body should have been withheld while the env_file is unreadable: %v", last)
	}
	// The phase lines keep their event and phase beside the filter's
	// diagnostic, for the same reason.
	for _, l := range lines[:len(lines)-1] {
		if l["event"] != "phase" || l["phase"] == nil {
			t.Fatalf("a withheld phase line lost its markers: %v", l)
		}
	}
	if lines[0]["phase"] != "downloading" {
		t.Fatalf("first phase = %v", lines[0])
	}
}

// GET /<app>?deploy=<version> is that version's recorded outcome;
// a version never deployed is a 404, a malformed one a 422.
func TestWebhookDeployRecordRoute(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.runner.startErr = errors.New("boom")
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`); w.Code != 500 {
		t.Fatalf("setup deploy: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":"v1"`) || !strings.Contains(w.Body.String(), `"status":"failed"`) || !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("record: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo?deploy=v2", appToken(t), ""); w.Code != 404 {
		t.Fatalf("never deployed: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo?deploy=../state", appToken(t), ""); w.Code != 422 {
		t.Fatalf("malformed: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo?deploy=", appToken(t), ""); w.Code != 422 {
		t.Fatalf("an empty deploy query is malformed, not the status: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo?deploy=%ZZ", appToken(t), ""); w.Code != 422 {
		t.Fatalf("a query that does not decode is malformed, not the status: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodGet, "/demo", appToken(t), ""); !strings.Contains(w.Body.String(), `"deploys":[{"version":"v1","status":"failed"`) {
		t.Fatalf("status lacks deploys: %s", w.Body.String())
	}
}

// A recorded version's name survives the filter's entropy layer in the
// status list and in its own record, as the running and on-disk
// versions' names do — a commit SHA is a name, not a secret.
func TestDeployRecordVersionsAreNamesNotSecrets(t *testing.T) {
	h, rig := newTestHandler(t)
	sha := "9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"
	rig.runner.startErr = errors.New("boom")
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"`+sha+`"}`); w.Code != 500 {
		t.Fatalf("setup deploy: %d %s", w.Code, w.Body.String())
	}
	rig.runner.startErr = nil
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/b.tgz","version":"v2"}`); w.Code != 200 {
		t.Fatalf("second deploy: %d %s", w.Code, w.Body.String())
	}
	// The failed SHA's release is gone; it is in no field but deploys.
	// The assertion reads the version fields alone: other fields may be
	// masked by the entropy layer (deployed_by names the test's random
	// t.TempDir() key path, which sometimes looks like a secret).
	for _, path := range []string{"/demo", "/demo?deploy=" + sha} {
		w := do(t, h, http.MethodGet, path, appToken(t), "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var body any
		must(t, json.Unmarshal(w.Body.Bytes(), &body))
		versions := versionFields(body, "", nil)
		if !slices.Contains(versions, sha) {
			t.Fatalf("%s: version fields %q lack %s: %s", path, versions, sha, w.Body.String())
		}
		for _, v := range versions {
			if strings.Contains(v, "[masked") || strings.Contains(v, "[redacted") {
				t.Fatalf("%s: version field %q filtered: %s", path, v, w.Body.String())
			}
		}
	}
}

// versionFields collects every string a JSON body holds under a key
// that names a version: version, current_version, available_versions.
func versionFields(v any, key string, out []string) []string {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			out = versionFields(child, k, out)
		}
	case []any:
		for _, child := range v {
			out = versionFields(child, key, out)
		}
	case string:
		if key == "version" || key == "current_version" || key == "available_versions" {
			out = append(out, v)
		}
	}
	return out
}

// An app the pool holds without a loaded spec answers the record route
// with a 503, as its status answers without one — never a panic. (Over
// HTTP such an app has no trust to verify against and is a 401 first;
// the route is exercised directly.)
func TestWebhookDeployRecordWithoutASpecIs503(t *testing.T) {
	h, _ := newTestHandler(t)
	w := httptest.NewRecorder()
	if err := h.deployRecord(w, &managedApp{name: "bare"}, "v1"); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("record route without a spec: %d %s", w.Code, w.Body.String())
	}
}

// A record read back through the live filter keeps the keys it was
// recorded with, and gains any the live filter redacts now.
func TestWebhookDeployRecordKeepsItsRecordedKeys(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("NEW=newvaluenewvalue1234\n"), 0o600))
	must(t, writeDeployRecord(rig.spec.dirs, "v1", []byte(`{"version":"v1","status":"failed","error":"[redacted:OLD] then newvaluenewvalue1234","redacted_env":["OLD"]}`)))
	w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redacted_env":["NEW","OLD"]`) || strings.Contains(w.Body.String(), "newvaluenewvalue1234") {
		t.Fatalf("record: %d %s", w.Code, w.Body.String())
	}
}

// A failed deploy whose only known value was in its detail — replaced
// as the detail was built, so the response filter finds nothing — still
// answers with the body's own top-level redacted_env, in the failure
// body and in the status after it.
func TestWebhookFailureBodyReportsTheDetailsKeys(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.deployLogLines = 40
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2\n"), 0o600))
	rig.prober.err = fmt.Errorf("not healthy within deadline 5m: %w", &probeError{status: 500, body: "bad token hunter2hunter2"})
	post := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
	get := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	for name, w := range map[string]*httptest.ResponseRecorder{"failure body": post, "status": get} {
		var got struct {
			RedactedEnv []string `json:"redacted_env"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %d %s: %v", name, w.Code, w.Body.String(), err)
		}
		if body := w.Body.String(); strings.Join(got.RedactedEnv, ",") != "SECRET" || strings.Contains(body, "hunter2hunter2") || !strings.Contains(body, "bad token [redacted:SECRET]") {
			t.Fatalf("%s: %d %s", name, w.Code, body)
		}
	}
}

// The examples' deploy.sh reads the failing phase as the last "phase"
// in a failure body, the stream's last line; with older records listed
// in the status that must still be last_deploy's, so deploys is
// serialized before it. A redaction does not move it: the
// redacted_env it adds re-marshals only the body's top level, which
// holds no phase.
func TestFailureBodyEndsWithLastDeploysPhase(t *testing.T) {
	for _, tc := range []struct {
		name           string
		secret, stream bool
	}{
		{"single", false, false},
		{"stream", false, true},
		{"single redacted", true, false},
		{"stream redacted", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rig := newTestHandler(t)
			must(t, writeDeployRecord(rig.spec.dirs, "v0", []byte(`{"version":"v0","status":"failed","phase":"soaking"}`)))
			rig.runner.startErr = errors.New("boom")
			if tc.secret {
				rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
				must(t, os.WriteFile(rig.spec.envFile, []byte("SECRET=hunter2hunter2\n"), 0o600))
				rig.runner.startErr = errors.New("boom hunter2hunter2")
			}
			var code int
			var body string
			if tc.stream {
				w, lines := streamLines(t, h)
				raw := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
				status, _ := lines[len(lines)-1]["http_status"].(float64)
				code, body = int(status), raw[len(raw)-1]
			} else {
				w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`)
				code, body = w.Code, w.Body.String()
			}
			i := strings.LastIndex(body, `"phase":"`)
			if code != 500 || i < 0 || !strings.HasPrefix(body[i:], `"phase":"starting"`) {
				t.Fatalf("status %d, last phase in body is not last_deploy's: %s", code, body)
			}
			// Each case takes the path it names: withField ran exactly
			// when a known value was in the body.
			if strings.Contains(body, `"redacted_env":["SECRET"]`) != tc.secret || strings.Contains(body, "hunter2hunter2") {
				t.Fatalf("redaction: %s", body)
			}
		})
	}
}

// Rule 3 over HTTP, on the bodies that are not a record: the status
// GET's last_deploy and deploys, and a failed POST's body, keep their
// outcome words while the same words in the error are redacted.
func TestWebhookOutcomeWordsSurviveInEveryBody(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("WORD=succeeded\nPHASE=starting\n"), 0o600))
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("deploy: %d %s", w.Code, w.Body.String())
	}
	rig.runner.startErr = errors.New("starting succeeded? no")
	post := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/b.tgz","version":"v2"}`)
	get := do(t, h, http.MethodGet, "/demo", appToken(t), "")
	for name, w := range map[string]*httptest.ResponseRecorder{"POST 500": post, "GET": get} {
		body := w.Body.String()
		for _, want := range []string{`"status":"failed"`, `"phase":"starting"`, `"status":"succeeded"`, `"name":"starting"`, `[redacted:PHASE] [redacted:WORD]? no`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %s in %d %s", name, want, w.Code, body)
			}
		}
	}
	if post.Code != 500 || get.Code != 200 {
		t.Fatalf("codes: %d %d", post.Code, get.Code)
	}
}

// GET ?deploy= is a body like any other: the record's outcome words
// stand outside the live filter, so a known value spelled like one
// does not read as redacted.
func TestWebhookDeployRecordOutcomeSurvivesAVocabularySecret(t *testing.T) {
	h, rig := newTestHandler(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(rig.spec.envFile, []byte("WORD=succeeded\n"), 0o600))
	if w := do(t, h, http.MethodPost, "/demo", appToken(t), `{"url":"https://x/a.tgz","version":"v1"}`); w.Code != 200 {
		t.Fatalf("deploy: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodGet, "/demo?deploy=v1", appToken(t), "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("record: %d %s", w.Code, w.Body.String())
	}
}

// A trust source the box cannot reach is the box's failure, not the
// caller's — but it is charged like any other: the same flat 401, and
// its "webhook auth failed" lines count against the same budgets. What
// the outage changes is
// the journal: one line per source per window naming it, written
// however spent the budgets are, so a CI loop retrying through an
// issuer outage cannot leave the journal quiet about the cause. And
// the first valid token once the issuer is back — and the source's
// refetch interval has passed since its last failed fetch — is
// admitted from the throttled address, so charging costs the deployer
// nothing.
func TestWebhookIssuerOutageIsNamedPastTheBudget(t *testing.T) {
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	iss := trusttest.NewIssuer(t)
	iss.JWKSDown.Store(true)
	// The key set's own clock, apart from the limiter's: its interval
	// is advanced alone at the end, so the address is still throttled.
	keys := trusttest.NewClock()
	rig.ma.verifiers = primed(t, deploytrust.VerifiersOn(keys, []deploytrust.Source{oidcSource(t, iss.URL)}, iss.Client))
	token := func() string {
		return iss.Mint(t, iss.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	}
	const ci, other = "203.0.113.9:1", "203.0.113.10:1"
	outageLines := func(all []observer.LoggedEntry) (n int) {
		for _, e := range all {
			if strings.Contains(e.Message, "could not consult") {
				if src := e.ContextMap()["source"]; src != "oidc:"+iss.URL {
					t.Fatalf("source = %v, want the down issuer", src)
				}
				n++
			}
		}
		return n
	}

	// Within the budget: charged and logged as any refusal, plus the
	// one outage line for the window.
	for i := range deploytrust.FailBudget {
		w := from(t, h, ci, token())
		if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != flat401 {
			t.Fatalf("request %d during the outage: %d %s; want the flat 401", i+1, w.Code, w.Body.String())
		}
	}
	all := logs.TakeAll()
	if got := outageLines(all); got != 1 {
		t.Fatalf("logged %d outage lines within the budget, want one: %+v", got, all)
	}
	if len(all) != deploytrust.FailBudget+2 { // the per-request lines, the address tripped, the outage
		t.Fatalf("logged %d records, want the budget plus the tripped line plus the outage line: %+v", len(all), all)
	}
	// Past it: the flat 401 as for any failure, and silent — the
	// outage line for this window is already written.
	for i := range 5 {
		if w := from(t, h, ci, token()); w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != flat401 {
			t.Fatalf("request %d past the budget: %d %s; want the flat 401", i+1, w.Code, w.Body.String())
		}
	}
	if got := logs.TakeAll(); len(got) != 0 {
		t.Fatalf("past the budget logged %+v, want nothing within the window", got)
	}

	// A window on: another address writes this window's outage line;
	// a second later the CI address spends its fresh budget again.
	rig.clock.Advance(deploytrust.FailWindow)
	if w := from(t, h, other, token()); w.Code != http.StatusUnauthorized {
		t.Fatalf("another address, next window: code = %d, want 401", w.Code)
	}
	if got := logs.TakeAll(); len(got) != 2 || outageLines(got) != 1 {
		t.Fatalf("another address logged %+v, want its refusal and the window's outage line", got)
	}
	rig.clock.Advance(time.Second)
	for range deploytrust.FailBudget {
		from(t, h, ci, token())
	}
	logs.TakeAll()
	// Another window on from that line: the CI address is still
	// throttled (its failures are a second younger than the line), and
	// its throttled request — silent as any other — still writes the
	// outage line for the new window.
	rig.clock.Advance(deploytrust.FailWindow - time.Second)
	if w := from(t, h, ci, token()); w.Code != http.StatusUnauthorized {
		t.Fatalf("throttled during the outage: code = %d, want 401", w.Code)
	}
	if got := logs.TakeAll(); len(got) != 1 || outageLines(got) != 1 {
		t.Fatalf("throttled request logged %+v, want the outage line alone", got)
	}

	// The issuer is back: within the source's refetch interval its
	// failed fetch stands; past it the next token begins the fetch and
	// is refused without waiting on it, and once that fetch has ended
	// the throttled address's first valid token deploys and clears it.
	iss.JWKSDown.Store(false)
	if w := from(t, h, ci, token()); w.Code != http.StatusUnauthorized {
		t.Fatalf("issuer back within the refetch interval: code = %d, want 401", w.Code)
	}
	keys.Advance(deploytrust.RefetchInterval)
	if w := from(t, h, ci, token()); w.Code != http.StatusUnauthorized {
		t.Fatalf("issuer back past the refetch interval, the token beginning the fetch: code = %d, want 401", w.Code)
	}
	// Only that token's fetch can let a retry through: nothing else
	// fetches within the interval.
	w := from(t, h, ci, token())
	for deadline := time.Now().Add(5 * time.Second); w.Code == http.StatusUnauthorized && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		w = from(t, h, ci, token())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("issuer back past the refetch interval: code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if h.limiter.Size() != 1 { // the other address's one failure remains
		t.Fatalf("after the deploy %d addresses are charged, want the other one alone", h.limiter.Size())
	}
}

// A source that could not be consulted is named even when a source
// after it accepts the token: the deploy goes through, nothing is
// charged, and the journal still says the first source is down. Two
// issuers, since a token reaches the first one's key fetch only if it
// is shaped for it — a local-key token is refused for its algorithm
// before any fetch.
func TestWebhookOutageIsNamedWhenAnotherSourceAccepts(t *testing.T) {
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	down, up := trusttest.NewIssuer(t), trusttest.NewIssuer(t)
	down.JWKSDown.Store(true)
	rig.ma.verifiers = primed(t, append(
		deploytrust.Verifiers([]deploytrust.Source{oidcSource(t, down.URL)}, down.Client),
		deploytrust.Verifiers([]deploytrust.Source{oidcSource(t, up.URL)}, up.Client)...))
	tok := up.Mint(t, up.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	if w := do(t, h, http.MethodGet, "/demo", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	all := logs.All()
	if len(all) != 1 || !strings.Contains(all[0].Message, "could not consult") || all[0].ContextMap()["source"] != "oidc:"+down.URL {
		t.Fatalf("logged %+v, want the one line naming the down issuer", all)
	}
	if h.limiter.Size() != 0 {
		t.Fatalf("an accepted token charged %d addresses, want none", h.limiter.Size())
	}
}

// Every source that could not be consulted gets its own line: with
// two issuers down, an alert keyed on the source field sees both.
func TestWebhookOutageNamesEverySourceDown(t *testing.T) {
	h, rig := newTestHandler(t)
	core, logs := observer.New(zap.WarnLevel)
	h.logger = zap.New(core)
	a, b := trusttest.NewIssuer(t), trusttest.NewIssuer(t)
	a.JWKSDown.Store(true)
	b.JWKSDown.Store(true)
	rig.ma.verifiers = primed(t, append(
		deploytrust.Verifiers([]deploytrust.Source{oidcSource(t, a.URL)}, a.Client),
		deploytrust.Verifiers([]deploytrust.Source{oidcSource(t, b.URL)}, b.Client)...))
	tok := a.Mint(t, a.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	if w := do(t, h, http.MethodGet, "/demo", tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	var sources []string
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "could not consult") {
			sources = append(sources, e.ContextMap()["source"].(string))
		}
	}
	if strings.Join(sources, " ") != "oidc:"+a.URL+" oidc:"+b.URL {
		t.Fatalf("sources named = %q, want both issuers in config order", sources)
	}
}
