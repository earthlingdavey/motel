package deploytrust

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
	"github.com/smallhoursorg/motel/liveswap/wire"
)

// forgetTestAddress is the cleanup for a test that charged the shared
// limiter: the address, and the process window its failure was
// counted in, so a repeated run (-count) starts as a fresh process
// would.
func forgetTestAddress(key string) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	delete(shared.keys, key)
	shared.global = failWindow{}
}

// charged reports whether the shared limiter holds a window for the
// address.
func charged(key string) bool {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	_, ok := shared.keys[key]
	return ok
}

// oneAuthLine is the single `webhook auth failed` record the observer
// holds, taken.
func oneAuthLine(t *testing.T, logs *observer.ObservedLogs) observer.LoggedEntry {
	t.Helper()
	all := logs.TakeAll()
	if len(all) != 1 || all[0].Message != "webhook auth failed" {
		t.Fatalf("logged %d records, want the one auth-failed line: %+v", len(all), all)
	}
	return all[0]
}

func request(addr, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = addr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// answered is what a Refusal writes: the status and body it hands the
// caller's writer.
func answered(t *testing.T, ref *Refusal) (code int, body wire.Error) {
	t.Helper()
	if err := ref.Write(func(c int, b any) error {
		code = c
		body, _ = b.(wire.Error)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return code, body
}

// TestAuthenticateOnTheSharedLimiter pins the preamble on the limiter
// both webhooks share: one refusal through it is charged there and
// answered with the flat 401 under the caller's field; an accepted
// token is who it is, with the claim the box binds a bundle to, and
// clears the address.
func TestAuthenticateOnTheSharedLimiter(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	vs := Verifiers([]Source{localTrust(pub, "box")}, nil)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	// An address of this test's own on the shared limiter, which no
	// other test touches.
	const addr, key = "203.0.113.9:4242", "203.0.113.9"
	t.Cleanup(func() { forgetTestAddress(key) })

	who, ref := Shared().Authenticate(request(addr, "not-a-jwt"), vs, logger, "webhook", "box")
	if ref == nil || who.By != "" {
		t.Fatalf("refused token: who = %+v, refusal = %+v; want a refusal and nobody", who, ref)
	}
	if code, body := answered(t, ref); code != http.StatusUnauthorized || body.Error != unauthorizedMessage {
		t.Fatalf("a refusal writes %d %v; want the flat 401", code, body)
	}
	if !charged(key) {
		t.Fatalf("a refusal through the shared limiter was not charged to it")
	}
	fields := oneAuthLine(t, logs).ContextMap()
	if fields["webhook"] != "box" || fields["remote"] != key {
		t.Errorf("fields = %v; want the caller's field and the address", fields)
	}
	if refused, _ := fields["refused"].(string); !strings.HasPrefix(refused, "local:test-key: ") {
		t.Errorf("refused = %q, want the source's reason", refused)
	}

	const sha = "4f1c2a9d0e8b7c6a5f4e3d2c1b0a998877665544"
	who, ref = Shared().Authenticate(request(addr, trusttest.Mint(t, priv, "box", map[string]string{"sub": "ci", "sha": sha})), vs, logger, "webhook", "box")
	if ref != nil {
		t.Fatalf("accepted token refused: %+v", ref)
	}
	if who.By != "local:test-key sub=ci" {
		t.Errorf("By = %q, want the attribution a deploy record carries", who.By)
	}
	if got, ok := who.Claim("sha"); !ok || got != sha {
		t.Errorf("Claim(sha) = %q, %v; want the token's", got, ok)
	}
	if got, ok := who.Claim("nope"); ok || got != "" {
		t.Errorf("Claim(nope) = %q, %v; want absent", got, ok)
	}
	if charged(key) {
		t.Errorf("an accepted token did not clear the address")
	}
	if logs.Len() != 0 {
		t.Errorf("an accepted token wrote %d journal lines, want none: %+v", logs.Len(), logs.All())
	}
}

// TestAuthenticateBudgetAndBound pins the preamble's budget on a
// clocked limiter — ten failures logged, the eleventh unlogged and
// answered exactly as the first was, the window draining it, a valid
// token clearing it — and the bound on the caller's field: one line, the
// length of a refusal, whatever the caller put in it; a value with a
// control byte reads Go-quoted; no key is no field.
func TestAuthenticateBudgetAndBound(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	vs := Verifiers([]Source{localTrust(pub, "box")}, nil)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	clk := trusttest.NewClock()
	l := NewLimiter(clk)
	const addr = "203.0.113.10:4242"
	call := func(token, key, value string) *Refusal {
		t.Helper()
		_, ref := l.Authenticate(request(addr, token), vs, logger, key, value)
		return ref
	}

	var first *Refusal
	for i := range FailBudget {
		ref := call("not-a-jwt", "webhook", "box")
		if ref == nil || ref.Status != http.StatusUnauthorized {
			t.Fatalf("failure %d: refusal = %+v; want 401", i+1, ref)
		}
		if first == nil {
			first = ref
		}
	}
	if got := logs.TakeAll(); len(got) != FailBudget+1 {
		t.Fatalf("logged %d records for %d failures; want each, and the line saying the budget is spent", len(got), FailBudget)
	}
	// Past the budget the answer is the first refusal's, field for
	// field: a caller cannot tell a spent budget from a fresh one.
	ref := call("not-a-jwt", "webhook", "box")
	if ref == nil || *ref != *first {
		t.Fatalf("past the budget: refusal = %+v; want %+v, the first one's", ref, first)
	}
	if code, body := answered(t, ref); code != http.StatusUnauthorized || body.Error != unauthorizedMessage {
		t.Errorf("past the budget a refusal writes %d %v; want the flat 401", code, body)
	}
	if logs.Len() != 0 {
		t.Fatalf("a refusal past the budget was logged: %+v", logs.All())
	}
	clk.Advance(FailWindow)
	if ref := call("not-a-jwt", "webhook", "box"); ref == nil || ref.Status != http.StatusUnauthorized {
		t.Fatalf("after the window: refusal = %+v; want 401, the budget fresh", ref)
	}
	if ref := call(trusttest.Mint(t, priv, "box", nil), "webhook", "box"); ref != nil {
		t.Fatalf("a valid token: refusal = %+v; want admitted", ref)
	}
	if l.Size() != 0 {
		t.Fatalf("a valid token left %d addresses charged, want none", l.Size())
	}
	logs.TakeAll()

	// The bound: whatever the caller put in the field, one line of at
	// most a refusal's length.
	long := strings.Repeat("r", 2*MaxRefusalLen) + "\n"
	call("not-a-jwt", "ref", long)
	if ref, _ := oneAuthLine(t, logs).ContextMap()["ref"].(string); len(ref) > MaxRefusalLen+len("...") || strings.Contains(ref, "\n") || !strings.HasPrefix(ref, `"rrr`) {
		t.Errorf("ref = %d bytes %q; want one quoted line of at most %d bytes", len(ref), ref, MaxRefusalLen)
	}

	// A value with a control byte — an app name liveswap took from the
	// request path — reads Go-quoted.
	call("not-a-jwt", "app", "demo\n")
	if app := oneAuthLine(t, logs).ContextMap()["app"]; app != strconv.QuoteToASCII("demo\n") {
		t.Errorf("app = %q, want the value Go-quoted", app)
	}

	// No key is no field: the line is the address and the reason.
	call("not-a-jwt", "", "")
	if fields := oneAuthLine(t, logs).ContextMap(); len(fields) != 2 || fields["remote"] == nil || fields["refused"] == nil {
		t.Errorf("fields = %v; want remote and refused alone", fields)
	}
}

// A token longer than MaxTokenLen is refused before any source reads
// it, whatever the name: one with no source and one with a source the
// same way, with the length named for the journal. One at the limit
// reaches the sources (#306).
func TestAuthorizeRefusesALongTokenUnread(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	vs := Verifiers([]Source{localTrust(pub, "box")}, nil)
	long := strings.Repeat("a", MaxTokenLen+1)
	for _, set := range [][]Verifier{nil, vs} {
		_, _, err := authorize(context.Background(), set, long)
		if want := "bearer token is " + strconv.Itoa(MaxTokenLen+1) + " bytes, longer than " + strconv.Itoa(MaxTokenLen); err == nil || err.Error() != want {
			t.Fatalf("%d sources: err = %v, want %q", len(set), err, want)
		}
	}
	atLimit := strings.Repeat("a", MaxTokenLen)
	if _, _, err := authorize(context.Background(), vs, atLimit); err == nil || !strings.HasPrefix(err.Error(), "local:") {
		t.Fatalf("a token at the limit: err = %v, want the source's own refusal", err)
	}
	if _, _, err := authorize(context.Background(), vs, trusttest.Mint(t, priv, "box", nil)); err != nil {
		t.Fatalf("a real token: refused: %v", err)
	}
}

// TestOutageLineIsPerCaller pins that the once-per-window line naming
// a source the box could not consult is written for each webhook that
// meets it, under that webhook's field: an operator reading one
// webhook's lines sees the outage whichever webhook met it first.
func TestOutageLineIsPerCaller(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	iss.JWKSDown.Store(true)
	vs := Verifiers([]Source{{kind: "oidc", issuer: iss.URL, audience: "motel", claims: map[string]string{"sub": "ci"}}}, iss.Client)
	settled(t, vs...)
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	l := NewLimiter(trusttest.NewClock())
	tok := iss.Mint(t, iss.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	for _, c := range []struct{ key, value, addr string }{
		{"app", "demo", "203.0.113.11:1"},
		{"webhook", "box", "203.0.113.12:1"},
		{"app", "demo", "203.0.113.11:1"},
	} {
		if _, ref := l.Authenticate(request(c.addr, tok), vs, logger, c.key, c.value); ref == nil || ref.Status != http.StatusUnauthorized {
			t.Fatalf("%s=%s: refusal = %+v; want the flat 401 while the issuer is down", c.key, c.value, ref)
		}
	}
	var outages []string
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "could not consult") {
			f := e.ContextMap()
			outages = append(outages, strings.TrimSpace(strings.Join([]string{asString(f["app"]), asString(f["webhook"])}, " ")))
		}
	}
	if strings.Join(outages, ",") != "demo,box" {
		t.Fatalf("outage lines = %v; want one per caller, each under its own field, and none for the repeat", outages)
	}
}

// asString is a context field's value, or empty when absent.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// TestIdentityClaimRendersAsMatchClaimsCompares pins the one rendering
// rule: a number claim by its source digits, a bool as its word, a
// composite claim as no value at all.
func TestIdentityClaimRendersAsMatchClaimsCompares(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	vs := Verifiers([]Source{localTrust(pub, "box")}, nil)
	now := time.Now()
	m := trusttest.Claims("", "box", now, now.Add(5*time.Minute), map[string]string{"sub": "ci"})
	m["repository_id"] = 100000000
	m["roles"] = []string{"admin"}
	m["verified"] = true
	who, _, err := authorize(context.Background(), vs, trusttest.SignEdDSA(t, priv, m))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		value string
		ok    bool
	}{
		"repository_id": {"100000000", true},
		"verified":      {"true", true},
		"sub":           {"ci", true},
		"roles":         {"", false},
		"absent":        {"", false},
	} {
		if got, ok := who.Claim(name); got != want.value || ok != want.ok {
			t.Errorf("Claim(%s) = %q, %v; want %q, %v", name, got, ok, want.value, want.ok)
		}
	}
}

// TestNewIsResolveOnACopy pins New to Resolve — the placeholder
// resolution, the validation (the same refusal, word for word), the
// https-only JWKS client unless allowInsecure, verifiers that accept
// what the source accepts — and to the two things it does on its own:
// the caller's config is left as parsed, and no source is a config
// error.
func TestNewIsResolveOnACopy(t *testing.T) {
	ctx := context.Background()
	priv, pub := trusttest.GenerateKey()
	keyPath := trusttest.KeyFile(t, pub)

	// A local source whose audience and subject are placeholders:
	// resolved, as liveswap's Provision resolves them, so a token for
	// the values is accepted and one for the placeholders' own text is
	// not — and resolved on a copy, so the config still says {env.NAME}.
	t.Setenv("MOTEL_TEST_AUDIENCE", "box")
	t.Setenv("MOTEL_TEST_SUBJECT", "ci")
	cfgs := []TrustConfig{{Kind: "local", PublicKey: keyPath, Audience: "{env.MOTEL_TEST_AUDIENCE}", Claims: map[string]string{"sub": "{env.MOTEL_TEST_SUBJECT}"}}}
	vs, err := New(cfgs, false)
	if err != nil || len(vs) != 1 || vs[0].Label() != "local:"+keyPath {
		t.Fatalf("New(local) = %v, %v; want one verifier labelled local:%s", vs, err, keyPath)
	}
	if who, _, err := authorize(ctx, vs, trusttest.Mint(t, priv, "box", map[string]string{"sub": "ci"})); err != nil || who.By != "local:"+keyPath+" sub=ci" {
		t.Errorf("a token for the resolved values: By = %q, %v; want accepted", who.By, err)
	}
	if _, _, err := authorize(ctx, vs, trusttest.Mint(t, priv, "{env.MOTEL_TEST_AUDIENCE}", map[string]string{"sub": "ci"})); err == nil {
		t.Errorf("a token for the placeholder's own text was accepted: the audience was not resolved")
	}
	if _, _, err := authorize(ctx, vs, trusttest.Mint(t, priv, "box", map[string]string{"sub": "{env.MOTEL_TEST_SUBJECT}"})); err == nil {
		t.Errorf("a token presenting the placeholder's own text was accepted: the claim was not resolved")
	}
	if cfgs[0].Audience != "{env.MOTEL_TEST_AUDIENCE}" || cfgs[0].Claims["sub"] != "{env.MOTEL_TEST_SUBJECT}" {
		t.Errorf("New resolved the caller's config in place: %+v", cfgs[0])
	}

	// No source is refused at config load, not as a 401 per token.
	if _, err := New(nil, false); err == nil || !strings.Contains(err.Error(), "no source") {
		t.Errorf("New(nil) = %v; want a refusal naming the missing source", err)
	}

	// A source Build refuses is refused in Build's words.
	bad := []TrustConfig{{Kind: "github", Audience: "motel"}}
	_, got := New(bad, false)
	_, want := Build(bad, nil)
	if got == nil || want == nil || got.Error() != want.Error() {
		t.Errorf("New(bad) = %v; Build = %v; want the same refusal", got, want)
	}

	// An OIDC source: the JWKS client is https-only unless
	// allowInsecure. The test issuer speaks plain http, so only the
	// insecure client reaches it; the other names it as unreachable.
	// The warm each one starts is a single bounded discovery fetch,
	// then a key fetch, that the issuer answers, or the https-only
	// client refuses, at once; settled waits them out, so nothing of
	// them outlives the test.
	iss := trusttest.NewIssuer(t)
	cfg := func() []TrustConfig {
		return []TrustConfig{{Kind: "oidc", Issuer: iss.URL, Audience: "motel", Subject: "ci"}}
	}
	tok := iss.Mint(t, iss.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	insecure, err := New(cfg(), true)
	if err != nil {
		t.Fatal(err)
	}
	settled(t, insecure...)
	if who, _, err := authorize(ctx, insecure, tok); err != nil || who.By != "oidc:"+iss.URL+" sub=ci" {
		t.Errorf("allow_insecure_http: By = %q, %v; want the token accepted over plain http", who.By, err)
	}
	secure, err := New(cfg(), false)
	if err != nil {
		t.Fatal(err)
	}
	settled(t, secure...)
	if _, down, err := authorize(ctx, secure, tok); err == nil || len(down) != 1 || !strings.Contains(err.Error(), "refusing non-https") {
		t.Errorf("https only: err = %v, down = %d; want the issuer refused as unreachable over http", err, len(down))
	}
}
