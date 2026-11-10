package deploytrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// localTrust is a local source for a test public key, built directly:
// the label is the key's name, as an operator's is its path.
func localTrust(pub ed25519.PublicKey, audience string) Source {
	return Source{kind: "local", audience: audience, pubKey: pub, keyPath: "test-key", maxTTL: DefaultMaxTokenTTL, attribution: attributionClaims["local"]}
}

func TestLocalVerifier(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	_, otherPub := trusttest.GenerateKey()
	want := map[string]string{"repository": "org/app"}
	base := func() *localVerifier {
		return &localVerifier{audience: "aud1", pub: pub, claims: want, maxTTL: DefaultMaxTokenTTL}
	}
	ctx := context.Background()

	// The happy path: right key, audience and claims.
	if _, err := base().verify(ctx, trusttest.Mint(t, priv, "aud1", want)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	cases := []struct {
		name  string
		build func() (Verifier, string)
	}{
		{"wrong key", func() (Verifier, string) {
			v := base()
			v.pub = otherPub
			return v, trusttest.Mint(t, priv, "aud1", want)
		}},
		{"wrong audience", func() (Verifier, string) {
			return base(), trusttest.Mint(t, priv, "other", want)
		}},
		{"claim mismatch", func() (Verifier, string) {
			return base(), trusttest.Mint(t, priv, "aud1", map[string]string{"repository": "evil/app"})
		}},
		{"missing claim", func() (Verifier, string) {
			return base(), trusttest.Mint(t, priv, "aud1", nil)
		}},
		{"expired", func() (Verifier, string) {
			return base(), trusttest.MintExpired(t, priv, "aud1", want)
		}},
		{"garbage", func() (Verifier, string) {
			return base(), "not-a-jwt"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, tok := tc.build()
			if _, err := v.verify(ctx, tok); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
	}
}

func TestOIDCPresetRequiresIdentityClaim(t *testing.T) {
	// Audience alone must be rejected — any repo/project can mint a token
	// for an arbitrary audience.
	for _, tc := range []TrustConfig{
		{Kind: "github", Audience: "motel"},
		{Kind: "gitlab", Audience: "motel"},
		{Kind: "oidc", Issuer: "https://idp.example", Audience: "motel"},
	} {
		if _, err := Build([]TrustConfig{tc}, nil); err == nil {
			t.Errorf("%s with only an audience must be rejected", tc.Kind)
		}
	}
	// With an identity claim it is accepted.
	ok := []TrustConfig{
		{Kind: "github", Audience: "motel", Claims: map[string]string{"repository": "o/r"}},
		{Kind: "gitlab", Audience: "motel", Claims: map[string]string{"project_path": "o/r"}},
		{Kind: "oidc", Issuer: "https://idp.example", Audience: "motel", Claims: map[string]string{"sub": "ci"}},
	}
	for _, tc := range ok {
		if _, err := Build([]TrustConfig{tc}, nil); err != nil {
			t.Errorf("%s with an identity claim rejected: %v", tc.Kind, err)
		}
	}
}

func TestSubjectAndClaimSubAreRefusedTogether(t *testing.T) {
	// The Caddyfile parser refuses the pair; the JSON form must too, or
	// one reader would have to pick a winner and the other could
	// disagree (the warning once did).
	_, err := Build([]TrustConfig{{Kind: "github", Audience: "a", Subject: "repo:o/r:pull_request", Claims: map[string]string{"repository": "o/r", "sub": "repo:o/r:ref:refs/heads/main"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("subject and claim sub together must be refused at load, got %v", err)
	}
}

func TestParseRefusesExactlyTheClaimKeysCaddyStrips(t *testing.T) {
	// Caddy removes every "@id" pair from the raw JSON before decoding
	// it (caddy.RemoveMetaFields), so a claim the parser let through
	// under such a key would never be enforced. The parser must refuse
	// the keys the strip changes, and no others: an issuer may emit any
	// other name.
	for _, key := range []string{"@id", `x"@id`, `"@id`, "@idx", "x@id", "@ID", "id", `@id"`, " @id", "@id ", "sub"} {
		raw, err := json.Marshal(TrustConfig{Kind: "oidc", Claims: map[string]string{key: "v", "zz": "w"}})
		if err != nil {
			t.Fatal(err)
		}
		stripped := !bytes.Equal(caddy.RemoveMetaFields(raw), raw)
		d := caddyfile.NewTestDispenser("deploy_trust oidc {\n\tclaim \"" + strings.ReplaceAll(key, `"`, `\"`) + "\" v\n}")
		d.Next()
		tc, err := Parse(d)
		switch {
		case stripped && (err == nil || !strings.Contains(err.Error(), "cannot be pinned")):
			t.Errorf("claim %q: Caddy strips it, so the parser must refuse it, got %v", key, err)
		case !stripped && (err != nil || tc.Claims[key] != "v"):
			t.Errorf("claim %q: Caddy keeps it, so the parser must too, got %+v, %v", key, tc, err)
		}
	}
}

func TestResolveTrustPlaceholdersFoldsTheJSONSubject(t *testing.T) {
	// The JSON subject becomes the sub claim at resolution, as the
	// Caddyfile parser already makes it, so an empty resolution is a
	// fail-closed constraint rather than a vanished one.
	t.Setenv("MOTEL_TEST_SUB", "repo:o/r:ref:refs/heads/main")
	tcs := []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.MOTEL_TEST_SUB}", Claims: map[string]string{"repository": "o/r"}}}
	ResolvePlaceholders(caddy.NewReplacer(), tcs)
	if tcs[0].Subject != "" || tcs[0].Claims["sub"] != "repo:o/r:ref:refs/heads/main" {
		t.Fatalf("subject must become the resolved sub claim, got %+v", tcs[0])
	}
	t.Setenv("MOTEL_TEST_SUB", "")
	tcs = []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.MOTEL_TEST_SUB}", Claims: map[string]string{"repository": "o/r"}}}
	ResolvePlaceholders(caddy.NewReplacer(), tcs)
	if v, ok := tcs[0].Claims["sub"]; !ok || v != "" {
		t.Fatalf("an empty subject must stay a sub=\"\" constraint, got %+v", tcs[0])
	}
	// Beside a claims.sub it is left as written, and the pair is refused.
	tcs = []TrustConfig{{Kind: "github", Audience: "a", Subject: "{env.MOTEL_TEST_SUB}", Claims: map[string]string{"repository": "o/r", "sub": "x"}}}
	ResolvePlaceholders(caddy.NewReplacer(), tcs)
	if _, err := Build(tcs, nil); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("subject beside claim sub must be refused after resolution, got %v", err)
	}
}

func TestLocalTokenRequiresExp(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	noExp := trusttest.SignEdDSA(t, priv, map[string]any{
		"aud": "a", "iat": jwt.NewNumericDate(time.Now()),
	})
	v := &localVerifier{audience: "a", pub: pub, maxTTL: DefaultMaxTokenTTL}
	if _, err := v.verify(context.Background(), noExp); err == nil {
		t.Fatal("local token without exp must be rejected")
	}
}

// max_token_ttl is measured from iat, so a local token must carry one:
// without it the refusal names the claim.
func TestLocalTokenRequiresIat(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	noIat := trusttest.SignEdDSA(t, priv, map[string]any{
		"aud": "a", "exp": jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	})
	v := &localVerifier{audience: "a", pub: pub, maxTTL: DefaultMaxTokenTTL}
	if _, err := v.verify(context.Background(), noIat); err == nil || !strings.Contains(err.Error(), "no iat claim") {
		t.Fatalf("local token without iat: err = %v, want a refusal naming iat", err)
	}
}

// A local token is accepted while exp - iat is at most its source's
// max_token_ttl, to the second, and refused one second past it — with
// the reason, which the journal's refused field carries, naming the
// directive.
func TestLocalTokenLifetimeBound(t *testing.T) {
	priv, pub := trusttest.GenerateKey()
	now := time.Now()
	lives := func(d time.Duration) string {
		return trusttest.SignEdDSA(t, priv, trusttest.Claims("", "a", now, now.Add(d), nil))
	}
	for _, limit := range []time.Duration{DefaultMaxTokenTTL, 2 * time.Hour} {
		src := localTrust(pub, "a")
		src.maxTTL = limit
		vs := Verifiers([]Source{src}, nil)
		if _, _, err := authorize(context.Background(), vs, lives(limit)); err != nil {
			t.Errorf("max_token_ttl %v: a token that lives exactly that long was refused: %v", limit, err)
		}
		_, _, err := authorize(context.Background(), vs, lives(limit+time.Second))
		if err == nil || !strings.Contains(err.Error(), "max_token_ttl "+limit.String()+" after its iat") {
			t.Errorf("max_token_ttl %v: a token a second longer: err = %v, want a refusal naming max_token_ttl", limit, err)
		}
	}

	// At the largest bound a duration holds (~292 years), exp - iat
	// would saturate at the bound itself; a token minted to outlive it
	// is still refused, and one inside it accepted.
	src := localTrust(pub, "a")
	src.maxTTL = time.Duration(math.MaxInt64)
	vs := Verifiers([]Source{src}, nil)
	inside := trusttest.SignEdDSA(t, priv, trusttest.Claims("", "a", now, now.AddDate(292, 0, 0), nil))
	if _, _, err := authorize(context.Background(), vs, inside); err != nil {
		t.Errorf("largest max_token_ttl: a token inside it was refused: %v", err)
	}
	beyond := trusttest.SignEdDSA(t, priv, trusttest.Claims("", "a", now, now.AddDate(974, 0, 0), nil))
	if _, _, err := authorize(context.Background(), vs, beyond); err == nil || !strings.Contains(err.Error(), "max_token_ttl") {
		t.Errorf("largest max_token_ttl: a token that outlives it: err = %v, want a refusal naming max_token_ttl", err)
	}
}

// max_token_ttl is the local source's: unset is DefaultMaxTokenTTL, a
// positive one is the bound, zero or negative is refused at config
// load, and an OIDC preset refuses it (the issuer sets its tokens'
// lifetime).
func TestMaxTokenTTLConfig(t *testing.T) {
	_, pub := trusttest.GenerateKey()
	key := trusttest.KeyFile(t, pub)
	ttl := func(d time.Duration) *caddy.Duration {
		c := caddy.Duration(d)
		return &c
	}
	for _, tc := range []struct {
		name string
		set  *caddy.Duration
		want time.Duration
	}{
		{"unset", nil, 24 * time.Hour},
		{"set", ttl(2 * time.Hour), 2 * time.Hour},
		{"over the default", ttl(87600 * time.Hour), 87600 * time.Hour},
	} {
		srcs, err := Build([]TrustConfig{{Kind: "local", PublicKey: key, MaxTokenTTL: tc.set}}, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if srcs[0].maxTTL != tc.want {
			t.Errorf("%s: max_token_ttl = %v, want %v", tc.name, srcs[0].maxTTL, tc.want)
		}
	}
	for name, set := range map[string]*caddy.Duration{"zero": ttl(0), "negative": ttl(-time.Hour)} {
		if _, err := Build([]TrustConfig{{Kind: "local", PublicKey: key, MaxTokenTTL: set}}, nil); err == nil || !strings.Contains(err.Error(), "max_token_ttl must be positive") {
			t.Errorf("%s max_token_ttl: err = %v, want a refusal", name, err)
		}
	}
	gh := TrustConfig{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r"}, MaxTokenTTL: ttl(time.Hour)}
	if _, err := Build([]TrustConfig{gh}, nil); err == nil || !strings.Contains(err.Error(), "does not take a max_token_ttl") {
		t.Errorf("github with max_token_ttl: err = %v, want a refusal", err)
	}
}

// max_token_ttl parses from the Caddyfile as a Caddy duration, and
// from JSON as one (a string) or as nanoseconds (what the Caddyfile
// adapter writes); an explicit zero survives the JSON, for Build to
// refuse rather than to read as unset.
func TestMaxTokenTTLParses(t *testing.T) {
	parse := func(block string) (TrustConfig, error) {
		d := caddyfile.NewTestDispenser("deploy_trust local {\n\tpublic_key /k.pub\n" + block + "}")
		d.Next()
		return Parse(d)
	}
	for in, want := range map[string]time.Duration{"2h": 2 * time.Hour, "1d": 24 * time.Hour, "0": 0, "-1h": -time.Hour} {
		tc, err := parse("\tmax_token_ttl " + in + "\n")
		if err != nil || tc.MaxTokenTTL == nil || time.Duration(*tc.MaxTokenTTL) != want {
			t.Errorf("max_token_ttl %s: %+v, %v; want %v", in, tc.MaxTokenTTL, err, want)
			continue
		}
		raw, err := json.Marshal(tc)
		if err != nil {
			t.Fatal(err)
		}
		var back TrustConfig
		if err := json.Unmarshal(raw, &back); err != nil || back.MaxTokenTTL == nil || *back.MaxTokenTTL != *tc.MaxTokenTTL {
			t.Errorf("max_token_ttl %s: JSON %s read back as %+v, %v", in, raw, back.MaxTokenTTL, err)
		}
	}
	for _, bad := range []string{"\tmax_token_ttl\n", "\tmax_token_ttl soon\n", "\tmax_token_ttl 1h\n\tmax_token_ttl 2h\n"} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%q: parsed, want an error", bad)
		}
	}
	var tc TrustConfig
	if err := json.Unmarshal([]byte(`{"kind":"local","public_key":"/k.pub","max_token_ttl":"36h"}`), &tc); err != nil || tc.MaxTokenTTL == nil || time.Duration(*tc.MaxTokenTTL) != 36*time.Hour {
		t.Errorf("JSON max_token_ttl \"36h\": %+v, %v", tc.MaxTokenTTL, err)
	}
}

func TestNewJWKSClientEnforcesHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close() // plain http

	if _, err := NewJWKSClient(false).Get(srv.URL); err == nil {
		t.Error("NewJWKSClient(false) must refuse a plain-http request")
	}
	resp, err := NewJWKSClient(true).Get(srv.URL)
	if err != nil {
		t.Errorf("NewJWKSClient(true) must allow http: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}

func TestNewJWKSClientBoundsIdleConnections(t *testing.T) {
	for _, allowInsecure := range []bool{false, true} {
		rt := NewJWKSClient(allowInsecure).Transport
		if w, ok := rt.(httpsOnlyTransport); ok {
			rt = w.base
		}
		if tr, ok := rt.(*http.Transport); !ok || tr.IdleConnTimeout <= 0 {
			t.Errorf("NewJWKSClient(%v): idle connections are not bounded (%T)", allowInsecure, rt)
		}
	}
}

func TestOIDCVerifier(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	want := map[string]string{"repository": "org/app"}
	base := func(t *testing.T) *oidcVerifier {
		v := &oidcVerifier{issuer: iss.URL, audience: "motel", claims: want, client: iss.Client}
		settled(t, v)
		return v
	}
	ctx := context.Background()

	if _, err := base(t).verify(ctx, iss.Mint(t, iss.Priv, "motel", want, time.Now().Add(5*time.Minute))); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	cases := []struct {
		name string
		tok  func() string
	}{
		{"wrong audience", func() string {
			return iss.Mint(t, iss.Priv, "other", want, time.Now().Add(5*time.Minute))
		}},
		{"claim mismatch", func() string {
			return iss.Mint(t, iss.Priv, "motel", map[string]string{"repository": "evil/app"}, time.Now().Add(5*time.Minute))
		}},
		{"missing claim", func() string {
			return iss.Mint(t, iss.Priv, "motel", nil, time.Now().Add(5*time.Minute))
		}},
		{"expired", func() string {
			return iss.Mint(t, iss.Priv, "motel", want, time.Now().Add(-time.Hour))
		}},
		{"unknown signing key", func() string {
			other, _ := rsa.GenerateKey(rand.Reader, 2048)
			return iss.Mint(t, other, "motel", want, time.Now().Add(5*time.Minute))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := base(t).verify(ctx, tc.tok()); err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
		})
	}
}

// TestOIDCVerifierNumericClaim pins a numeric identity claim
// (repository_id, one of the documented GitHub identity claims). The
// value arrives in the token as a JSON number, and the verifier must
// compare it by its exact decimal digits. A round-numbered id decoded to
// float64 would stringify as "1e+08" and never match — the S1 bug — so
// this guards the json.Number decode path.
func TestOIDCVerifierNumericClaim(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	v := &oidcVerifier{
		issuer: iss.URL, audience: "motel",
		claims: map[string]string{"repository_id": "100000000"},
		client: iss.Client,
	}
	settled(t, v)
	tok := iss.MintClaims(t, "motel", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": 100000000})
	if _, err := v.verify(context.Background(), tok); err != nil {
		t.Fatalf("token with numeric repository_id rejected: %v", err)
	}
	// A different numeric id must still be rejected.
	bad := iss.MintClaims(t, "motel", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": 999})
	if _, err := v.verify(context.Background(), bad); err == nil {
		t.Fatal("wrong repository_id must be rejected")
	}

	// An integer beyond 2^53 (float64's exact-integer ceiling): this only
	// verifies if the claim kept its exact digits through decoding, so it
	// guards the json.Number path against any regression to float64
	// reformatting (which would round 2^53+1 down to 2^53).
	const big = int64(9007199254740993) // 2^53 + 1
	bigV := &oidcVerifier{
		issuer: iss.URL, audience: "motel",
		claims: map[string]string{"repository_id": "9007199254740993"},
		client: iss.Client,
	}
	settled(t, bigV)
	bigTok := iss.MintClaims(t, "motel", time.Now().Add(5*time.Minute),
		map[string]any{"repository_id": big})
	if _, err := bigV.verify(context.Background(), bigTok); err != nil {
		t.Fatalf("token with a >2^53 repository_id rejected: %v", err)
	}
}

// TestOIDCVerifierIssuerDown pins what an issuer the box cannot get
// what it needs from looks like to the caller of verify: an
// unavailable, not a refusal — the token was never judged — for
// discovery failed and the key fetch failed, neither waited on, and a
// failure stands until a reload, or the refetch interval has passed
// and the fetch a token then begins has ended. A discovery not yet
// ended is a plain refusal: the source is not ready, not out. The
// key-fetch case holds keyFetchFailed against the pinned go-oidc (the
// text is the only handle on it). It also pins what is NOT an outage:
// a token the cached keys verify still deploys, a malformed token and
// a bad signature under a kid the box holds are refusals of their own,
// and an issuer answering as someone else is the config's. A caller
// that went away is judged as any other: nothing waits.
func TestOIDCVerifierIssuerDown(t *testing.T) {
	want := map[string]string{"repository": "org/app"}
	c := trusttest.NewClock()
	ctx := context.Background()
	// Each case its own issuer, since every source of one issuer shares
	// its state (issuerState).
	source := func(iss *trusttest.Issuer) *oidcVerifier {
		return &oidcVerifier{issuer: iss.URL, audience: "motel", claims: want, client: iss.Client, clock: c}
	}
	valid := func(iss *trusttest.Issuer) string {
		return iss.Mint(t, iss.Priv, "motel", want, time.Now().Add(5*time.Minute))
	}
	wantDown := func(t *testing.T, iss *trusttest.Issuer, err error, what string) unavailable {
		t.Helper()
		var u unavailable
		if !errors.As(err, &u) {
			t.Fatalf("%s: err = %v, want an unavailable", what, err)
		}
		if u.label != "oidc:"+iss.URL {
			t.Fatalf("%s: label = %q, want the source's", what, u.label)
		}
		return u
	}
	wantPlain := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil || errors.As(err, &unavailable{}) {
			t.Fatalf("%s: err = %v, want a plain refusal", what, err)
		}
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()

	// A source whose discovery has not ended is not ready, which is not
	// an outage: refused at once, saying a retry may go through.
	iss := trusttest.NewIssuer(t)
	v := source(iss)
	_, err := v.verify(ctx, valid(iss))
	wantPlain(t, err, "discovery under way")
	if !strings.Contains(err.Error(), "oidc discovery for "+iss.URL+": under way, so a retry once that ends may go through") {
		t.Fatalf("discovery under way: reason = %q, want it to say so", err)
	}
	waited(t, v)
	if _, err := v.verify(ctx, valid(iss)); err != nil {
		t.Fatalf("after the discovery the token began: refused: %v", err)
	}

	// Discovery down: the outage's, and retried by a token only past
	// the interval; the token that begins it is still the outage's, and
	// its retry once it ends deploys.
	iss = trusttest.NewIssuer(t)
	iss.DiscoveryDown.Store(true)
	v = source(iss)
	settled(t, v)
	_, err = v.verify(ctx, valid(iss))
	u := wantDown(t, iss, err, "discovery 503")
	if !strings.Contains(u.Error(), "oidc discovery for") || !strings.Contains(u.Error(), "503") {
		t.Fatalf("discovery 503: reason = %q, want it to say discovery and the answer", u.Error())
	}
	_, err = v.verify(gone, valid(iss))
	_ = wantDown(t, iss, err, "discovery 503, the caller gone: judged the same")
	iss.DiscoveryDown.Store(false)
	_, err = v.verify(ctx, valid(iss))
	_ = wantDown(t, iss, err, "discovery back within the interval")
	c.Advance(RefetchInterval)
	_, err = v.verify(ctx, valid(iss))
	_ = wantDown(t, iss, err, "discovery back, beginning it again")
	waited(t, v)
	if _, err := v.verify(ctx, valid(iss)); err != nil {
		t.Fatalf("discovery back: valid token rejected: %v", err)
	}
	// A reload retries it at once, whatever the interval.
	iss = trusttest.NewIssuer(t)
	iss.DiscoveryDown.Store(true)
	v = source(iss)
	settled(t, v)
	iss.DiscoveryDown.Store(false)
	settled(t, v)
	if _, err := v.verify(ctx, valid(iss)); err != nil {
		t.Fatalf("discovery back after a reload: valid token rejected: %v", err)
	}

	iss = trusttest.NewIssuer(t)
	iss.DiscoveredAs.Store("https://someone-else.example")
	v = source(iss)
	settled(t, v)
	_, err = v.verify(ctx, valid(iss))
	wantPlain(t, err, "discovery naming another issuer")
	if !strings.Contains(err.Error(), "someone-else.example") {
		t.Fatalf("issuer mismatch: reason = %q, want it to name the issuer found", err.Error())
	}

	iss = trusttest.NewIssuer(t)
	iss.JWKSDown.Store(true)
	v = source(iss)
	settled(t, v)
	_, err = v.verify(ctx, valid(iss))
	u = wantDown(t, iss, err, "JWKS 503 on first use")
	if !strings.HasPrefix(u.Error(), keyFetchFailed) {
		t.Fatalf("JWKS 503: reason = %q, want the keyFetchFailed prefix %q", u.Error(), keyFetchFailed)
	}
	_, err = v.verify(gone, valid(iss))
	_ = wantDown(t, iss, err, "JWKS 503, the caller gone: judged the same")

	// Recovery needs no restart: the key set fetches again once the
	// interval since the failed fetch has passed, for the token that then
	// needs it. Until that fetch has ended the failure stands, without a
	// fetch per token.
	iss.JWKSDown.Store(false)
	_, err = v.verify(ctx, valid(iss))
	_ = wantDown(t, iss, err, "issuer back within the interval")
	c.Advance(RefetchInterval)
	_, err = v.verify(ctx, valid(iss))
	_ = wantDown(t, iss, err, "issuer back, beginning the fetch")
	waited(t, v)
	if _, err := v.verify(ctx, valid(iss)); err != nil {
		t.Fatalf("issuer back past the interval: valid token rejected: %v", err)
	}
	// Nothing waits, so a caller whose context has ended (an HTTP/1.1
	// half-close, which still reads the answer) is judged as any other.
	if _, err := v.verify(gone, valid(iss)); err != nil {
		t.Fatalf("a valid token, the caller gone: rejected: %v", err)
	}

	// Down again, keys cached: a token those keys verify still deploys
	// — an outage only reaches tokens the box has no key for.
	iss.JWKSDown.Store(true)
	before := iss.JWKSFetches.Load()
	if _, err := v.verify(ctx, valid(iss)); err != nil {
		t.Fatalf("issuer down with keys cached: valid token rejected: %v", err)
	}
	// A malformed token never reaches the fetch: a refusal of its own.
	_, err = v.verify(ctx, "not-a-jwt")
	wantPlain(t, err, "malformed token during an outage")
	// So is a bad signature under a kid the box holds: refused on the
	// spot, with no fetch to fail.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, err = v.verify(ctx, iss.Mint(t, other, "motel", want, time.Now().Add(5*time.Minute)))
	wantPlain(t, err, "bad signature under a held kid during an outage")
	if n := iss.JWKSFetches.Load(); n != before {
		t.Fatalf("issuer down with keys cached: %d fetches, want none", n-before)
	}
	// A token whose kid the box holds no key for begins a fetch it does
	// not wait on: refused against the keys held, the last fetch having
	// succeeded. Once that fetch has failed, the next such token within
	// the interval is the outage's, without fetching again.
	c.Advance(RefetchInterval)
	unknown := func() error {
		_, err := v.verify(ctx, iss.MintKID(t, other, "unknown", "motel", want, time.Now().Add(5*time.Minute)))
		return err
	}
	wantPlain(t, unknown(), "unknown kid beginning a fetch during an outage")
	waited(t, v)
	for range 2 {
		_ = wantDown(t, iss, unknown(), "unknown kid during an outage")
	}
	if n := iss.JWKSFetches.Load(); n != before+1 {
		t.Fatalf("unknown kids during an outage: %d fetches, want one", n-before)
	}
}

// TestAuthorizeNamesEverySourceItCouldNotConsult: down lists the
// sources that could not be consulted, in config order, whether the
// token was then refused (and the journal text still carries every
// source's reason) or accepted by a source after them — a fallback
// that keeps deploys going must not hide the outage.
func TestAuthorizeNamesEverySourceItCouldNotConsult(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	iss.DiscoveryDown.Store(true)
	priv, pub := trusttest.GenerateKey()
	local := &localVerifier{audience: "motel", pub: pub, keyPath: "/k.pem", maxTTL: DefaultMaxTokenTTL}
	c := trusttest.NewClock()
	down := &oidcVerifier{issuer: iss.URL, audience: "motel", client: iss.Client, clock: c}
	settled(t, down) // its discovery has failed: an outage, not a source not yet ready
	labels := func(down []unavailable) string {
		var out []string
		for _, u := range down {
			out = append(out, u.label)
		}
		return strings.Join(out, " ")
	}
	bad := trusttest.Mint(t, priv, "other", nil) // the local source refuses it: wrong audience
	for _, order := range [][]Verifier{{local, down}, {down, local}} {
		_, got, err := authorize(context.Background(), order, bad)
		if err == nil || labels(got) != down.Label() {
			t.Fatalf("order %v: err = %v, down = %q, want a refusal naming %s", order, err, labels(got), down.Label())
		}
		if !strings.Contains(err.Error(), "local:/k.pem: ") || !strings.Contains(err.Error(), "oidc:"+iss.URL+": ") {
			t.Fatalf("order %v: reasons = %q, want both sources named", order, err.Error())
		}
	}
	// Two sources down: both named, in config order.
	iss2 := trusttest.NewIssuer(t)
	iss2.DiscoveryDown.Store(true)
	down2 := &oidcVerifier{issuer: iss2.URL, audience: "motel", client: iss2.Client}
	settled(t, down2)
	if _, got, err := authorize(context.Background(), []Verifier{down, local, down2}, bad); err == nil || labels(got) != down.Label()+" "+down2.Label() {
		t.Fatalf("two sources down: err = %v, down = %q, want both", err, labels(got))
	}
	// A source after the down one accepts the token: authorized, and
	// the down one is still named.
	good := trusttest.Mint(t, priv, "motel", nil)
	if by, got, err := authorize(context.Background(), []Verifier{down, local}, good); err != nil || by.By != local.Label() || labels(got) != down.Label() {
		t.Fatalf("accepted past a down source: by = %q, down = %q, err = %v", by.By, labels(got), err)
	}
	// Both up, and the down source's discovery retried by a reload:
	// nothing is down, and the bad token is a plain refusal.
	iss.DiscoveryDown.Store(false)
	settled(t, down)
	if _, got, err := authorize(context.Background(), []Verifier{local, down}, bad); err == nil || len(got) != 0 {
		t.Fatalf("both sources up: err = %v, down = %q, want a plain refusal", err, labels(got))
	}
}

// TestAttribute pins the form a deploy is recorded under: the label,
// then each named claim in the order named, a value that could be read
// as more than one field quoted.
func TestAttribute(t *testing.T) {
	names := []string{"repository", "ref", "actor"}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"all present, in list order", map[string]any{"actor": "alice", "ref": "refs/heads/main", "repository": "org/blog"},
			"L repository=org/blog ref=refs/heads/main actor=alice"},
		{"absent claims left out", map[string]any{"ref": "refs/heads/main"}, "L ref=refs/heads/main"},
		{"none present", map[string]any{"sub": "x"}, "L"},
		{"non-scalar and null left out", map[string]any{"repository": []any{"a"}, "ref": map[string]any{}, "actor": nil}, "L"},
		{"number by its digits", map[string]any{"actor": json.Number("9007199254740993")}, "L actor=9007199254740993"},
		{"space quoted", map[string]any{"actor": "alice actor=bob"}, `L actor="alice actor=bob"`},
		{"quote and backslash quoted", map[string]any{"actor": `a"b\c`}, `L actor="a\"b\\c"`},
		{"control rune quoted", map[string]any{"actor": "a\nb"}, `L actor="a\nb"`},
		{"non-ASCII space quoted", map[string]any{"actor": "a" + string(rune(0xa0)) + "b"}, `L actor="a\` + "u00a0b\""},
		{"empty quoted", map[string]any{"actor": ""}, `L actor=""`},
		{"printable unicode as is", map[string]any{"actor": "zoë"}, "L actor=zoë"},
		{"= inside a value as is", map[string]any{"ref": "refs/heads/a=b"}, "L ref=refs/heads/a=b"},
	} {
		if got := attribute("L", names, tc.claims); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPresetAttribution pins which claims each preset records, carried
// past the fold of github/gitlab into kind "oidc".
func TestPresetAttribution(t *testing.T) {
	dir := t.TempDir()
	_, pub := trusttest.GenerateKey()
	pubDER, _ := x509.MarshalPKIXPublicKey(pub)
	pubPath := dir + "/k.pub"
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tc   TrustConfig
		want string
	}{
		{TrustConfig{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "o/r"}}, "repository ref actor"},
		{TrustConfig{Kind: "gitlab", Audience: "a", Claims: map[string]string{"project_path": "o/r"}}, "project_path ref user_login"},
		{TrustConfig{Kind: "oidc", Issuer: "https://idp.example", Audience: "a", Subject: "ci"}, "sub"},
		{TrustConfig{Kind: "local", PublicKey: pubPath}, "sub"},
	} {
		srcs, err := Build([]TrustConfig{tc.tc}, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.tc.Kind, err)
		}
		if got := strings.Join(srcs[0].attribution, " "); got != tc.want {
			t.Errorf("%s records %q, want %q", tc.tc.Kind, got, tc.want)
		}
	}
}

// TestAuthorizeAttributes pins that authorize hands back the accepting
// verifier's label with the token's claims, for either kind — the
// string deployed_by and the `deploy authorized` line carry.
func TestAuthorizeAttributes(t *testing.T) {
	ctx := context.Background()
	priv, pub := trusttest.GenerateKey()
	local := Verifiers([]Source{localTrust(pub, "aud1")}, nil)
	for _, tc := range []struct {
		name   string
		claims map[string]string
		want   string
	}{
		{"with a subject", map[string]string{"sub": "alice"}, "local:test-key sub=alice"},
		{"without one", nil, "local:test-key"},
	} {
		by, _, err := authorize(ctx, local, trusttest.Mint(t, priv, "aud1", tc.claims))
		if err != nil || by.By != tc.want {
			t.Errorf("local %s: authorize = %q, %v; want %q", tc.name, by.By, err, tc.want)
		}
	}

	iss := trusttest.NewIssuer(t)
	gh := Verifiers([]Source{{
		kind: "oidc", issuer: iss.URL, audience: "motel",
		claims: map[string]string{"repository": "org/blog"}, attribution: attributionClaims["github"],
	}}, iss.Client)
	settled(t, gh...)
	tok := iss.Mint(t, iss.Priv, "motel", map[string]string{
		"repository": "org/blog", "ref": "refs/heads/main", "actor": "alice",
		"sub": "repo:org/blog:ref:refs/heads/main",
	}, time.Now().Add(5*time.Minute))
	want := "oidc:" + iss.URL + " repository=org/blog ref=refs/heads/main actor=alice"
	if by, _, err := authorize(ctx, gh, tok); err != nil || by.By != want {
		t.Errorf("github: authorize = %q, %v; want %q", by.By, err, want)
	}
}

// TestAuthorizeSaysWhy pins what a refused token leaves for the
// operator's journal: which source refused it and why, in config
// order, with the identity a verified token presented — and that the
// two cases where no source is asked say so. What a caller sees is
// pinned in handler_test (the flat 401, whatever the reason).
func TestAuthorizeSaysWhy(t *testing.T) {
	ctx := context.Background()
	priv, pub := trusttest.GenerateKey()
	local := Verifiers([]Source{localTrust(pub, "aud1")}, nil)
	iss := trusttest.NewIssuer(t)
	gh := Verifiers([]Source{{
		kind: "oidc", issuer: iss.URL, audience: "motel",
		claims: map[string]string{"repository": "org/blog"}, attribution: attributionClaims["github"],
	}}, iss.Client)
	settled(t, gh...)
	both := append(append([]Verifier{}, gh...), local...)
	otherRSA, _ := rsa.GenerateKey(rand.Reader, 2048)
	mint := func(aud string, claims map[string]string) string {
		return iss.Mint(t, iss.Priv, aud, claims, time.Now().Add(5*time.Minute))
	}
	for _, tc := range []struct {
		name string
		vs   []Verifier
		tok  string
		want []string // each in the refusal, in this order
	}{
		{"no token", local, "", []string{"no bearer token"}},
		{"no source", nil, trusttest.Mint(t, priv, "aud1", nil), []string{"no deploy_trust source"}},
		{"garbage", local, "not-a-jwt", []string{"local:test-key: "}},
		{"wrong audience", local, trusttest.Mint(t, priv, "other", nil), []string{"local:test-key: ", "aud"}},
		{"expired", local, trusttest.MintExpired(t, priv, "aud1", nil), []string{"local:test-key: ", "exp"}},
		{"bad signature under a held kid names the reload", gh, iss.Mint(t, otherRSA, "motel", nil, time.Now().Add(time.Minute)),
			[]string{"oidc:" + iss.URL + ": ", `kid "` + iss.KID + `": the token is another issuer's, or, if the issuer replaced that key, systemctl reload motel`}},
		{"claim mismatch names who tried", gh,
			mint("motel", map[string]string{"repository": "org/other", "ref": "refs/heads/main", "actor": "alice"}),
			[]string{"oidc:" + iss.URL + ": claim \"repository\" mismatch, presented repository=org/other ref=refs/heads/main actor=alice"}},
		{"claim absent, nothing presented", gh, mint("motel", nil), []string{"claim \"repository\" absent from token, presented nothing"}},
		{"every source, config order", both, "not-a-jwt", []string{"oidc:" + iss.URL + ": ", "; local:test-key: "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			by, _, err := authorize(ctx, tc.vs, tc.tok)
			if err == nil {
				t.Fatalf("authorized as %q", by.By)
			}
			got := err.Error()
			at := 0
			for _, w := range tc.want {
				i := strings.Index(got[at:], w)
				if i < 0 {
					t.Fatalf("refusal %q lacks %q (after offset %d)", got, w, at)
				}
				at += i + len(w)
			}
		})
	}

	// A refusal is bounded however long the token's own text is — the
	// audience go-oidc quotes back, or the identity a verified token
	// presents, in ASCII or not — it stays valid UTF-8 on one line, and
	// the cut takes the identity, never the reason in front of it.
	for name, tok := range map[string]string{
		"long audience":       mint(strings.Repeat("a", 10_000), nil),
		"long ascii identity": mint("motel", map[string]string{"repository": "org/" + strings.Repeat("r", 400)}),
		"long emoji identity": mint("motel", map[string]string{"repository": "org/" + strings.Repeat("😀", 200), "ref": "refs/heads/é"}),
		"control in identity": mint("motel", map[string]string{"repository": "org/" + strings.Repeat("a\n", 200)}),
	} {
		by, _, err := authorize(ctx, gh, tok)
		if err == nil {
			t.Fatalf("%s: authorized as %q", name, by.By)
		}
		got := err.Error()
		if len(got) > len(gh[0].Label())+2+MaxRefusalLen+len("...") {
			t.Errorf("%s: refusal is %d bytes; the reason must be cut at %d", name, len(got), MaxRefusalLen)
		}
		if !utf8.ValidString(got) || strings.ContainsRune(got, '\n') {
			t.Errorf("%s: refusal is not one line of UTF-8: %q", name, got)
		}
		if strings.Contains(name, "identity") && !strings.Contains(got, `claim "repository" mismatch`) {
			t.Errorf("%s: the cut took the reason: %q", name, got)
		}
	}
}

// TestBoundRefusal pins what a journal line needs of a refusal that
// may carry request text: one line, valid UTF-8, and at most
// MaxRefusalLen bytes plus the ellipsis — whatever the bytes.
func TestBoundRefusal(t *testing.T) {
	if got := boundRefusal("plain: claim \"repository\" mismatch"); got != "plain: claim \"repository\" mismatch" {
		t.Errorf("a plain refusal must pass through, got %q", got)
	}
	if got := boundRefusal("a\nb"); got != `"a\nb"` {
		t.Errorf("a control rune must leave the refusal Go-quoted, got %q", got)
	}
	if got := boundRefusal("a\xffb"); got != `"a\xffb"` {
		t.Errorf("a byte that is not UTF-8 must leave the refusal Go-quoted, got %q", got)
	}
	long := strings.Repeat("x", MaxRefusalLen+50)
	if got := boundRefusal(long); got != long[:MaxRefusalLen]+"..." {
		t.Errorf("an overlong refusal must be cut, got %d bytes", len(got))
	}
	for name, in := range map[string]string{
		"emoji":        strings.Repeat("😀", MaxRefusalLen),
		"emoji at cut": strings.Repeat("x", MaxRefusalLen-1) + "😀😀",
		"newlines":     strings.Repeat("\n", MaxRefusalLen),
		"bad bytes":    strings.Repeat("\xff", MaxRefusalLen),
	} {
		got := boundRefusal(in)
		if len(got) > MaxRefusalLen+len("...") {
			t.Errorf("%s: %d bytes, must be at most %d", name, len(got), MaxRefusalLen+3)
		}
		if !utf8.ValidString(got) || strings.ContainsFunc(got, func(r rune) bool { return !strconv.IsPrint(r) && r != ' ' }) {
			t.Errorf("%s: not one printable line of UTF-8: %q", name, got)
		}
	}
}

// TestMatchClaimsNumeric exercises the stringification directly across
// the number representations a claim set can carry.
func TestMatchClaimsNumeric(t *testing.T) {
	want := map[string]string{"repository_id": "100000000"}
	// The decodeClaims path yields json.Number.
	if err := matchClaims(want, map[string]any{"repository_id": json.Number("100000000")}); err != nil {
		t.Errorf("json.Number claim rejected: %v", err)
	}
	// A float64 claim means the set was decoded without UseNumber and
	// its digits are no longer trustworthy: refused, never formatted.
	if err := matchClaims(want, map[string]any{"repository_id": float64(100000000)}); err == nil {
		t.Error("float64 claim must be refused, not formatted and matched")
	}
	if err := matchClaims(want, map[string]any{"repository_id": json.Number("99")}); err == nil {
		t.Error("mismatched numeric claim must be rejected")
	}
	// A 2^53+1 integer survives json.Number exactly (float64 would not).
	big := map[string]string{"repository_id": "9007199254740993"}
	if err := matchClaims(big, map[string]any{"repository_id": json.Number("9007199254740993")}); err != nil {
		t.Errorf("exact big-int claim rejected: %v", err)
	}
	// A composite (array) claim must never match a configured string, even
	// one spelled like its %v rendering.
	if err := matchClaims(map[string]string{"groups": "[admin]"},
		map[string]any{"groups": []any{"admin"}}); err == nil {
		t.Error("array-valued claim must not match a string constraint")
	}
}

// TestSourceLabelAndPinnedClaims holds the two accessors notify's
// bindings read to the source Build makes: the preset's issuer behind
// the label, `subject` folded into `sub`, and nothing a token carries.
func TestSourceLabelAndPinnedClaims(t *testing.T) {
	srcs, err := Build([]TrustConfig{{Kind: "github", Audience: "a", Claims: map[string]string{"repository": "org/blog"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := srcs[0]
	if got := s.Label(); got != "oidc:"+githubIssuer {
		t.Fatalf("label = %q", got)
	}
	if v, ok := s.PinnedClaim("repository"); !ok || v != "org/blog" {
		t.Fatalf("repository = %q, %v", v, ok)
	}
	if _, ok := s.PinnedClaim("actor"); ok {
		t.Fatal("a claim the source does not pin was reported pinned")
	}
	srcs, err = Build([]TrustConfig{{Kind: "gitlab", Audience: "a", Subject: "project_path:org/app:ref_type:branch:ref:main"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := srcs[0].PinnedClaim("sub"); !ok || !strings.HasPrefix(v, "project_path:org/app") {
		t.Fatalf("sub = %q, %v", v, ok)
	}
	if l := localTrust(nil, "a").Label(); l != "local:test-key" {
		t.Fatalf("local label = %q", l)
	}
}
