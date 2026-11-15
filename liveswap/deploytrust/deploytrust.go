// Package deploytrust is how a webhook decides who sent a request: the
// `deploy_trust` grammar and its verifiers — an OIDC issuer's JWKS or a
// local public key, never a shared secret an app could read out of the
// supervisor's environment — the preamble every webhook request passes
// (Limiter.Authenticate), and what a failed attempt costs the journal
// (Limiter). liveswap's deploy webhook and the box's config webhook both
// authenticate here, on the one limiter Shared returns, so the two
// cannot drift and one address has one budget. The package imports
// nothing of liveswap's; trusttest beside it mints tokens for tests.
package deploytrust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"
)

// Deploy authentication is asymmetric: the box stores only PUBLIC
// material (an OIDC issuer's JWKS, or a local public key), never a
// symmetric secret an app could read out of the supervisor's
// environment. A deploy request carries `Authorization: Bearer <JWT>`;
// it is authorized if ANY configured trust source verifies the token's
// signature and standard claims AND every claim constraint matches
// (AND within a source, OR across sources — so "OIDC primary, local
// fallback" is simply two sources).

// githubIssuer is the GitHub Actions OIDC issuer (the `github` preset).
const githubIssuer = "https://token.actions.githubusercontent.com"

// gitlabIssuerDefault is the SaaS GitLab issuer; the `gitlab` preset
// accepts an `issuer` override for self-hosted instances.
const gitlabIssuerDefault = "https://gitlab.com"

// oidcLeeway tolerates modest clock skew between the box and the token
// issuer when validating exp/nbf/iat.
const oidcLeeway = 60 * time.Second

// DefaultMaxTokenTTL is a local source's max_token_ttl when its block
// sets none: the longest exp - iat it accepts. A local token is a
// bearer credential for as long as it lives, and nothing on the box
// can revoke one short of removing the key, so the bound is what a
// leaked token is worth. A day covers a job that mints once and runs
// for hours; one that runs for longer mints per deploy, or its block
// raises the bound.
const DefaultMaxTokenTTL = 24 * time.Hour

// TrustConfig is one `deploy_trust` block in Caddyfile/JSON form. Kind
// is the preset named on the block (`github`, `gitlab`, `oidc`,
// `local`); the remaining fields are resolved and validated into a
// Source by Build.
type TrustConfig struct {
	Kind      string `json:"kind,omitempty"`
	Issuer    string `json:"issuer,omitempty"`     // oidc/gitlab override
	Audience  string `json:"audience,omitempty"`   // required for OIDC presets
	PublicKey string `json:"public_key,omitempty"` // local: path to a PKIX PEM ed25519 key
	// MaxTokenTTL is local's longest exp - iat accepted; nil is
	// DefaultMaxTokenTTL. A pointer, so an explicit zero reaches Build
	// to be refused.
	MaxTokenTTL *caddy.Duration   `json:"max_token_ttl,omitempty"`
	Subject     string            `json:"subject,omitempty"` // sugar for claim `sub`
	Claims      map[string]string `json:"claims,omitempty"`  // exact-match claim constraints
}

// Source is a validated, resolved trust declaration: presets
// mapped to issuers, local public keys loaded, claims folded. It holds
// no network state — verifiers are built from it in Verifiers.
type Source struct {
	kind        string // "oidc" | "local"
	issuer      string // oidc
	audience    string
	pubKey      ed25519.PublicKey // local
	keyPath     string            // local (for error messages)
	maxTTL      time.Duration     // local: the longest exp - iat accepted
	claims      map[string]string
	attribution []string // the preset's attributionClaims, kept past the fold to "oidc"
}

// Label names the source the way deployed_by and Verifier.Label do:
// oidc:<issuer> (a preset's issuer, resolved) or local:<public key
// path>. For the notify module, whose `bind` lines name an identity by
// its source and the claims the source pins.
func (s Source) Label() string {
	if s.kind == "local" {
		return "local:" + s.keyPath
	}
	return "oidc:" + s.issuer
}

// PinnedClaim is the value the source requires of the named claim, as
// the operator configured it (`subject` folded in as `sub`), and
// whether it pins that claim at all. What a token merely carries is
// not here: a binding names who the box trusts, never what a token
// said.
func (s Source) PinnedClaim(name string) (string, bool) {
	v, ok := s.claims[name]
	return v, ok
}

// ResolvePlaceholders expands {env.*} (and other known Caddy
// placeholders) in a slice of trust configs, in place. Kind is a
// literal block token and is never a placeholder. The JSON `subject`
// becomes the `sub` claim here, resolved, as the Caddyfile parser
// already makes it: a placeholder that resolves empty stays a
// fail-closed sub="" constraint instead of vanishing, which would
// broaden the block. With a `claims.sub` as well it is left as
// written, for resolveTrustConfig to refuse the pair.
func ResolvePlaceholders(repl *caddy.Replacer, tcs []TrustConfig) {
	for i := range tcs {
		tc := &tcs[i]
		tc.Issuer = repl.ReplaceKnown(tc.Issuer, "")
		tc.Audience = repl.ReplaceKnown(tc.Audience, "")
		tc.PublicKey = repl.ReplaceKnown(tc.PublicKey, "")
		for k, v := range tc.Claims {
			tc.Claims[k] = repl.ReplaceKnown(v, "")
		}
		if tc.Subject != "" {
			if _, dup := tc.Claims["sub"]; !dup {
				if tc.Claims == nil {
					tc.Claims = make(map[string]string, 1)
				}
				tc.Claims["sub"] = repl.ReplaceKnown(tc.Subject, "")
				tc.Subject = ""
			}
		}
	}
}

// Build resolves the effective trust sources for one app. Per-app
// sources override the global default wholesale (same semantics as
// artifact_allowlist), never append. It loads local public keys and
// validates presets here so a bad key path or missing audience fails
// config load — fail-closed, like the rest of liveswap's config.
func Build(global, perApp []TrustConfig) ([]Source, error) {
	src := global
	if len(perApp) > 0 {
		src = perApp
	}
	out := make([]Source, 0, len(src))
	for i, tc := range src {
		ts, err := resolveTrustConfig(tc)
		if err != nil {
			return nil, fmt.Errorf("deploy_trust[%d]: %w", i, err)
		}
		out = append(out, ts)
	}
	return out, nil
}

// Resolve is the verifiers for a set of deploy_trust blocks on
// one JWKS client: {env.*} placeholders resolved in place
// (ResolvePlaceholders), presets validated and local keys loaded
// (Build — a bad key path or an unaudienced OIDC source is a
// config error, fail-closed), then Verifiers. Provision calls
// it for liveswap's global set and New for the box's,
// so the two cannot drift; the per-app sets, which fall back to the
// global blocks, go through Build beside it.
func Resolve(repl *caddy.Replacer, configs []TrustConfig, jwks *http.Client) ([]Verifier, error) {
	ResolvePlaceholders(repl, configs)
	sources, err := Build(configs, nil)
	if err != nil {
		return nil, err
	}
	return Verifiers(sources, jwks), nil
}

// New is the verifiers for a set of `deploy_trust` blocks, for a
// webhook that is not liveswap's — the box's: Resolve, on a JWKS
// client that fetches over https only unless allowInsecure, with each
// OIDC issuer's discovery and keys fetched again in the background
// (Warm), the keys the process already holds in use meanwhile. Two
// things liveswap's Provision does not do, because its caller is
// itself: the placeholders are resolved on a copy, so
// configs still read as parsed — a caller that renders or diffs its
// config later writes {env.NAME}, never the value — and no source at
// all is refused here, at config load, rather than as a 401 for every
// token at request time.
func New(configs []TrustConfig, allowInsecure bool) ([]Verifier, error) {
	if len(configs) == 0 {
		return nil, errors.New("deploy_trust: no source configured; a webhook with none would refuse every token")
	}
	own := slices.Clone(configs)
	for i := range own {
		own[i].Claims = maps.Clone(own[i].Claims)
	}
	vs, err := Resolve(caddy.NewReplacer(), own, NewJWKSClient(allowInsecure))
	if err != nil {
		return nil, err
	}
	Warm(vs)
	return vs, nil
}

// effectiveClaims is a block's claim constraints as the verifier sees
// them: the JSON `subject` field is the `sub` constraint. Setting both
// is refused by resolveTrustConfig, as the Caddyfile parser refuses
// it, so no reader has to pick one; resolveTrustConfig and
// warnUnboundSource read the same map, so the warning cannot be quiet
// about a subject the verifier enforces.
func effectiveClaims(tc TrustConfig) map[string]string {
	claims := make(map[string]string, len(tc.Claims)+1)
	for k, v := range tc.Claims {
		claims[k] = v
	}
	if tc.Subject != "" {
		claims["sub"] = tc.Subject
	}
	return claims
}

func resolveTrustConfig(tc TrustConfig) (Source, error) {
	if _, dup := tc.Claims["sub"]; dup && tc.Subject != "" {
		return Source{}, fmt.Errorf("subject and `claim sub` are both set")
	}
	claims := effectiveClaims(tc)

	switch tc.Kind {
	case "github", "gitlab", "oidc":
		issuer := tc.Issuer
		switch tc.Kind {
		case "github":
			if issuer != "" {
				return Source{}, fmt.Errorf("github preset does not take an issuer (it is %s)", githubIssuer)
			}
			issuer = githubIssuer
		case "gitlab":
			if issuer == "" {
				issuer = gitlabIssuerDefault
			}
		case "oidc":
			if issuer == "" {
				return Source{}, fmt.Errorf("oidc requires an issuer")
			}
		}
		if tc.Audience == "" {
			return Source{}, fmt.Errorf("%s requires an audience (never trust an unaudienced token)", tc.Kind)
		}
		if tc.PublicKey != "" {
			return Source{}, fmt.Errorf("%s does not take a public_key", tc.Kind)
		}
		if tc.MaxTokenTTL != nil {
			return Source{}, fmt.Errorf("%s does not take a max_token_ttl (the issuer sets its tokens' lifetime)", tc.Kind)
		}
		if err := requireIdentityClaim(tc.Kind, claims); err != nil {
			return Source{}, err
		}
		return Source{kind: "oidc", issuer: issuer, audience: tc.Audience, claims: claims, attribution: attributionClaims[tc.Kind]}, nil

	case "local":
		if tc.PublicKey == "" {
			return Source{}, fmt.Errorf("local requires a public_key path")
		}
		if tc.Issuer != "" {
			return Source{}, fmt.Errorf("local does not take an issuer")
		}
		maxTTL := DefaultMaxTokenTTL
		if tc.MaxTokenTTL != nil {
			if maxTTL = time.Duration(*tc.MaxTokenTTL); maxTTL <= 0 {
				return Source{}, fmt.Errorf("max_token_ttl must be positive, got %v", maxTTL)
			}
		}
		key, err := loadEd25519PublicKey(tc.PublicKey)
		if err != nil {
			return Source{}, fmt.Errorf("local public_key: %w", err)
		}
		return Source{kind: "local", audience: tc.Audience, pubKey: key, keyPath: tc.PublicKey, maxTTL: maxTTL, claims: claims, attribution: attributionClaims["local"]}, nil

	case "":
		return Source{}, fmt.Errorf("missing preset (use `deploy_trust github|gitlab|oidc|local { ... }`)")
	default:
		return Source{}, fmt.Errorf("unknown preset %q (use github|gitlab|oidc|local)", tc.Kind)
	}
}

// identityClaims lists, per preset, the claims that scope a token to a
// specific deployer. An audience alone is NOT identity: both GitHub and
// GitLab let any project mint a token for any audience, so a source with
// only an audience would authorize every repository on the issuer.
var identityClaims = map[string][]string{
	"github": {"repository", "repository_id", "repository_owner", "repository_owner_id", "sub"},
	"gitlab": {"project_path", "project_id", "namespace_path", "namespace_id", "sub"},
}

// attributionClaims lists, per preset, the token claims a deploy is
// recorded under after the source's label (attribute): where it came
// from and who started it. Not the identity constraints — those are
// the operator's config, already known — but what this token says.
// No e-mail claim: the result lands in CI logs.
var attributionClaims = map[string][]string{
	"github": {"repository", "ref", "actor"},
	"gitlab": {"project_path", "ref", "user_login"},
	"oidc":   {"sub"},
	"local":  {"sub"},
}

// requireIdentityClaim fails closed when an OIDC source pins no identity.
func requireIdentityClaim(kind string, claims map[string]string) error {
	if kind == "oidc" {
		if _, ok := claims["sub"]; ok {
			return nil
		}
		return fmt.Errorf("oidc requires an identity constraint — pin `subject`/`claim sub` (every OIDC token has one); an audience alone lets any workload of this issuer deploy")
	}
	for _, name := range identityClaims[kind] {
		if _, ok := claims[name]; ok {
			return nil
		}
	}
	return fmt.Errorf("%s requires an identity claim (one of: %s) — an audience alone authorizes every %s project", kind, strings.Join(identityClaims[kind], ", "), kind)
}

// bindingClaims lists, per OIDC preset, the claims that can tie a
// token to a branch, tag or environment. An identity claim says which
// repository may deploy; nothing in it says from where, and a token is
// minted by whatever workflow ran — on any branch, edited by anyone
// who can push one. Left out on purpose: `job_workflow_ref` names the
// reusable workflow's ref, and a caller on any branch can invoke that;
// GitLab's `ci_config_ref_uri` names the CI configuration's ref, which
// an external configuration pins while the pipeline runs on any
// branch. `environment` is listed but counts for less — see
// claimBinds. The generic `oidc` preset requires `sub` already and
// never warns.
var bindingClaims = map[string][]string{
	"github": {"sub", "ref", "sha", "ref_protected", "environment", "workflow_ref"},
	"gitlab": {"sub", "ref", "ref_path", "sha", "ref_protected", "environment", "environment_protected"},
}

// binding is what one pinned claim buys: nothing; an environment,
// which is as strong as the provider's deployment-branch rule for it
// — a rule the box cannot read, so it is noted rather than counted;
// or a branch or commit.
type binding int

const (
	bindsNothing binding = iota
	bindsEnvironment
	bindsRef
)

// claimBinds reads one pinned claim against the whole block. Most
// bind by being pinned at all. `ref_protected` pinned "false" admits
// every unprotected ref; "true" admits protected refs only; any other
// literal admits no token at all. `environment_protected` says the
// environment is protected — a rule about who may deploy to it, not
// from where — so "true" counts as an `environment` does, "false"
// admits every unprotected one, and any other literal admits nothing.
// A bare GitLab `ref` is a name a branch and a tag can share, so it
// counts only beside a `ref_type`. `sub` binds only in the provider's
// default form, which carries `:ref:` or `:environment:`
// (`repo:o/r:ref:refs/heads/main`,
// `project_path:o/r:ref_type:branch:ref:main`): GitHub lets an org
// customise the template, and its pull_request form
// (`repo:o/r:pull_request`) names no branch at all.
func claimBinds(kind, name, value string, claims map[string]string) binding {
	switch name {
	case "sub":
		switch {
		case strings.Contains(value, ":ref:"):
			return bindsRef
		case strings.Contains(value, ":environment:"):
			return bindsEnvironment
		}
		return bindsNothing
	case "environment":
		return bindsEnvironment
	case "ref_protected":
		if value == "false" {
			return bindsNothing
		}
	case "environment_protected":
		switch value {
		case "false":
			return bindsNothing
		case "true":
			return bindsEnvironment
		}
	case "ref":
		if _, typed := claims["ref_type"]; kind == "gitlab" && !typed {
			return bindsNothing
		}
	}
	return bindsRef
}

// WarnUnboundSource logs the two shapes of one deploy_trust block
// that load and are wider than they read: an OIDC block whose claims
// pin an identity but no branch (any ref of that identity deploys),
// and a local block without an audience (a token minted for any box
// that trusts the same key is accepted here). Warnings, not refusals
// — both are the documented minimum, and refusing them would fail
// every box on it. where says which block: liveswap's warnUnboundTrust
// calls this once per block it loads, and the box's option may for
// its own. It reads the block as configured, after placeholder
// resolution: an audience that resolved empty is no audience, and a
// claim that resolved empty is a constraint no token meets —
// fail-closed, nothing deploys, which is the opposite of "any ref
// deploys", so it gets its own warning and not that one.
func WarnUnboundSource(logger *zap.Logger, tc TrustConfig, where ...zap.Field) {
	switch tc.Kind {
	case "github", "gitlab":
		claims := effectiveClaims(tc)
		for _, name := range sortedKeys(claims) {
			if claims[name] == "" {
				logger.Warn("deploy_trust pins a claim that resolved empty: no token carries one, so nothing deploys through this block",
					append(where, zap.String("preset", tc.Kind), zap.String("claim", name),
						zap.String("fix", "set the placeholder the claim names, or drop the line"))...)
				return
			}
		}
		strongest := bindsNothing
		for _, name := range bindingClaims[tc.Kind] {
			if value, ok := claims[name]; ok {
				strongest = max(strongest, claimBinds(tc.Kind, name, value, claims))
			}
		}
		switch strongest {
		case bindsRef:
			return
		case bindsEnvironment:
			logger.Info("deploy_trust pins an environment, which binds a branch only as far as that environment restricts its deployment branches",
				append(where, zap.String("preset", tc.Kind))...)
			return
		}
		ref := "claim ref refs/heads/main"
		if tc.Kind == "gitlab" {
			ref = "claim ref_path refs/heads/main"
		}
		logger.Warn("deploy_trust pins no branch: a token minted on any ref of the pinned identity deploys",
			append(where, zap.String("preset", tc.Kind),
				zap.String("fix", "add `"+ref+"` so a token minted on another branch is refused"))...)
	case "local":
		if tc.Audience != "" {
			return
		}
		logger.Warn("deploy_trust local has no audience: a token minted for any box that trusts this key is accepted here",
			append(where, zap.String("public_key", tc.PublicKey),
				zap.String("fix", "add `audience <a name for this box>` and mint with `motel deploy-token --audience <the same>`"))...)
	}
}

// NewJWKSClient builds the HTTP client the OIDC verifier uses for
// discovery and JWKS fetches. It refuses plain-http requests (and
// https→http redirects) so a network attacker cannot swap the
// verification keys and forge deploy tokens — unless allow_insecure_http
// is set, the documented escape hatch for test rigs and LANs. There is
// one client per setting for the process, so every config load hands
// its sources the same one, and an issuer's shared state (issuerFor)
// outlives a reload.
func NewJWKSClient(allowInsecure bool) *http.Client {
	if allowInsecure {
		return insecureJWKSClient()
	}
	return httpsJWKSClient()
}

var (
	httpsJWKSClient    = sync.OnceValue(func() *http.Client { return newJWKSClient(false) })
	insecureJWKSClient = sync.OnceValue(func() *http.Client { return newJWKSClient(true) })
)

func newJWKSClient(allowInsecure bool) *http.Client {
	var rt http.RoundTripper = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		// A kept-alive connection holds its transport's goroutines until
		// the issuer hangs up; this bounds how long one idles.
		// DefaultTransport's value.
		IdleConnTimeout: 90 * time.Second,
	}
	if !allowInsecure {
		rt = httpsOnlyTransport{base: rt}
	}
	// A small control request, so unlike the artifact download it takes
	// a bounded overall timeout.
	return &http.Client{Timeout: 30 * time.Second, Transport: rt}
}

type httpsOnlyTransport struct{ base http.RoundTripper }

func (t httpsOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-https OIDC request to %s (set allow_insecure_http for test rigs)", r.URL.Redacted())
	}
	return t.base.RoundTrip(r)
}

// loadEd25519PublicKey reads a PKIX PEM file (as emitted by
// `motel deploy-keygen`) and returns the ed25519 public key.
func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured public-key path, not request input
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key (%T)", path, pub)
	}
	return key, nil
}

// Verifier authenticates a raw bearer JWT for one trust source. The
// name is exported for the box subsystem, which holds the verifiers
// New builds and hands them to Authenticate; the
// methods are not, so every implementation is this package's.
type Verifier interface {
	// verify returns a nil error iff the token's signature and standard
	// claims are valid and every configured claim constraint matches,
	// and with it who the token is (Identity): who the deploy is
	// recorded under (attribute), and the claims it carries. The
	// error says what failed — signature, exp, audience, a claim — and
	// is for the operator's journal, never a response. An error that
	// is an unavailable (errors.As) says the source could not be
	// consulted at all, which is the box's failure, not the token's.
	verify(ctx context.Context, rawToken string) (Identity, error)
	// Label names the source the way deployed_by and the journal do:
	// oidc:<issuer> or local:<public key path>.
	Label() string
}

// Identity is who a verified token is, as the source that accepted it
// saw it. By is the attribution a deploy is recorded under — the
// source's label and the token's attribution claims (attribute) — and
// the one thing liveswap's own webhook uses of it. The claims are the
// token's whole verified claim set, for a caller whose check the
// attribution does not carry: the box webhook binds the bundle it is
// handed to the token's `sha` (box/DESIGN-box.md, "Token ↔ bundle").
type Identity struct {
	By     string
	claims map[string]any
}

// Claim is the named claim rendered the way matchClaims compares one
// against the config — a json.Number by its source digits, never %v
// on a float64 (decodeClaims) — and ok=false for a claim that is
// absent or not a scalar: an array or an object is never an identity
// value, and a caller must not be handed an empty string it could
// mistake for one.
func (id Identity) Claim(name string) (value string, ok bool) {
	return claimScalar(id.claims[name])
}

// Verifiers turns validated trust sources into live verifiers.
// It is infallible: key loading and preset validation already happened
// in Build, so a config reload can rewire auth without error.
func Verifiers(sources []Source, jwksClient *http.Client) []Verifier {
	return VerifiersOn(systemClock{}, sources, jwksClient)
}

// VerifiersOn is Verifiers with the OIDC sources' RefetchInterval
// on clock: for tests, which advance it. clock must be comparable (a
// pointer, or a struct of comparable fields): it is part of the key
// the issuer's shared state is held under (issuerFor).
func VerifiersOn(clock Clock, sources []Source, jwksClient *http.Client) []Verifier {
	out := make([]Verifier, 0, len(sources))
	for _, ts := range sources {
		switch ts.kind {
		case "oidc":
			out = append(out, &oidcVerifier{
				issuer: ts.issuer, audience: ts.audience,
				claims: ts.claims, attribution: ts.attribution, client: jwksClient, clock: clock,
			})
		case "local":
			out = append(out, &localVerifier{
				audience: ts.audience, pub: ts.pubKey, maxTTL: ts.maxTTL,
				keyPath: ts.keyPath, claims: ts.claims, attribution: ts.attribution,
			})
		}
	}
	return out
}

// Warm begins each OIDC source's discovery, and after it the fetch of
// its issuer's keys, in the background, whatever RefetchInterval says:
// at start, so the source holds both by the first real deploy, and at
// each reload, so a reload picks up what the issuer now publishes —
// a key replaced under the same kid among it. The issuer's state is
// shared (issuerState), so what a source held before the reload stays
// in use until the new fetch has ended, and N sources trusting one
// issuer make one fetch. No token waits on either (RefetchInterval).
func Warm(verifierSets ...[]Verifier) {
	for _, set := range verifierSets {
		for _, v := range set {
			if ov, ok := v.(*oidcVerifier); ok {
				ov.state().warm()
			}
		}
	}
}

// Prime is Warm, returning once each OIDC source's discovery and the
// key fetch after it have ended, or ctx has: for a test, whose first
// token would otherwise be refused while they run. The outcome is
// what the source then holds; Prime does not report it.
func Prime(ctx context.Context, verifierSets ...[]Verifier) error {
	Warm(verifierSets...)
	return waitFor(ctx, verifierSets...)
}

// waitFor returns once each OIDC source's discovery and key fetch
// under way, if any, have ended, or ctx has; it begins neither.
func waitFor(ctx context.Context, verifierSets ...[]Verifier) error {
	for _, set := range verifierSets {
		for _, v := range set {
			ov, ok := v.(*oidcVerifier)
			if !ok {
				continue
			}
			is := ov.state()
			is.mu.Lock()
			done := is.inflight
			is.mu.Unlock()
			if err := waitOn(ctx, done); err != nil {
				return err
			}
			is.mu.Lock()
			keys := is.keys
			is.mu.Unlock()
			if keys == nil {
				continue
			}
			if err := waitOn(ctx, keys.underway()); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitOn waits for done to close, or ctx to end; a nil done is closed.
func waitOn(ctx context.Context, done chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unavailable is a refusal that is the box's, not the token's: the
// source could not be consulted — its discovery or its key fetch
// failed — so the token was never judged. label names the source. The
// first discovery or fetch still under way is not one: the source is
// not ready, and the token is refused plainly, its retry soon judged.
// A retry under way after a failure is: until it ends, the token gets
// the failure it follows.
//
// Authenticate charges it like any refusal: it answers the same flat
// 401, and its "webhook auth failed" lines count against the same
// budgets, so a CI loop retrying through an outage writes no more of
// them than any other. Charging costs a deployer nothing real: the
// first valid token the source can judge again — the issuer back and
// the fetch RefetchInterval past the failed one ended — is
// admitted from a throttled address. What an unavailable changes is
// the journal: one line per source per window naming it, written
// however spent the budgets are (Limiter.outage) and whether or
// not another source then accepted the token, so a CI loop retrying
// through an issuer outage never leaves the journal quiet about the
// cause, and neither does a fallback source that keeps deploys going.
// Not an unavailable: an issuer answering as someone else (config).
// The caller's context decides nothing: no check waits on it.
type unavailable struct {
	label string
	err   error
}

func (u unavailable) Error() string { return u.err.Error() }
func (u unavailable) Unwrap() error { return u.err }

// authorize returns who the first verifier that accepts the token
// says it is (Identity): the label and attribution claims the deploy
// is recorded under, for the deploy record and audit log, and the
// token's claims for a caller that needs one of them. When none
// does, the error says why each source refused, one entry per source
// in config order (or the one reason no source was tried), for the
// operator's journal. down is every source that could not be
// consulted before that point, accepted or not: the journal must say
// so either way. The response stays a flat 401 whatever the reason
// (see Limiter.Authenticate), so nothing here reaches a caller.
func authorize(ctx context.Context, verifiers []Verifier, rawToken string) (who Identity, down []unavailable, err error) {
	if rawToken == "" {
		return Identity{}, nil, errors.New("no bearer token in Authorization header")
	}
	// Before any source parses it, and whatever the name: what a
	// refusal costs grows with the token, and a name with no source
	// would not pay it (MaxTokenLen).
	if len(rawToken) > MaxTokenLen {
		return Identity{}, nil, fmt.Errorf("bearer token is %d bytes, longer than %d", len(rawToken), MaxTokenLen)
	}
	if len(verifiers) == 0 {
		return Identity{}, nil, errors.New("no deploy_trust source resolves for this app")
	}
	refused := make([]string, 0, len(verifiers))
	for _, v := range verifiers {
		who, err := v.verify(ctx, rawToken)
		if err == nil {
			return who, down, nil
		}
		var u unavailable
		if errors.As(err, &u) {
			down = append(down, u)
		}
		// The bound is on the reason alone — the label is the operator's
		// config, and it must survive however long the reason is.
		refused = append(refused, v.Label()+": "+boundRefusal(err.Error()))
	}
	return Identity{}, down, errors.New(strings.Join(refused, "; "))
}

// MaxTokenLen is the longest bearer token judged; authorize refuses a
// longer one before any source parses it. Each source a name resolves
// to parses the token, and a name with no source parses nothing, so the
// time to a refusal grows with the token for a name that exists: an
// OIDC source refused a 2 KiB token (about a real GitHub one) in
// 0.11 ms, a 16 KiB one in 0.33 ms, and a 1 MiB one, which the header
// limit allows, in 16 ms (arm64, measured 2026-10-20). The cap keeps
// that difference to a fraction of a millisecond per source, the
// timing residual DESIGN-threat-model.md accepts (#306). A GitHub or
// GitLab OIDC token is 1–2 KiB, a `motel deploy-token` one smaller.
const MaxTokenLen = 8 << 10

// MaxRefusalLen bounds one source's reason in the journal. A reason
// can carry token-supplied text — an issuer library quotes the token's
// audience or issuer in its error, and a claim mismatch names the
// identity the token presented — so, like scopeField for the caller's
// field, the size of a line an unauthenticated caller can write is
// fixed; Limiter bounds how many. The source's label in front of
// it is the operator's own config and is not cut.
const MaxRefusalLen = 300

// boundRefusal makes a refusal one journal line of at most
// MaxRefusalLen bytes plus an ellipsis. One line first: a refusal
// holding a control rune (a newline in a library's error would split
// the line) or bytes that are not UTF-8 is Go-quoted to ASCII — the
// rule attribute applies to claim values — and only then cut, at a
// rune boundary, so the cut can neither leave a partial rune nor
// grow the string it bounds.
func boundRefusal(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return !strconv.IsPrint(r) && r != ' ' }) {
		s = strconv.QuoteToASCII(s)
	}
	if len(s) > MaxRefusalLen {
		s = CutRunes(s, MaxRefusalLen) + "..."
	}
	return s
}

// CutRunes is s cut to at most n bytes at a rune boundary, so a cut
// never turns valid UTF-8 into bytes that would have to be quoted: the
// rule every bound here cuts by, and liveswap's app name too.
func CutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// attribute is who a deploy is recorded under: the source's label,
// then ` name=value` for each named claim the token carries, in the
// order named. A claim that is absent or not a scalar is left out. A
// value that is empty or holds a space, a quote, a backslash or an
// unprintable rune is Go-quoted, so a subject the token's signer chose
// ("alice actor=bob") reads as one value, never as a second field.
func attribute(label string, names []string, claims map[string]any) string {
	return label + presented(names, claims)
}

// presented is the ` name=value` tail of attribute: the identity a
// verified token carries, rendered the same way whether the deploy is
// recorded under it or refused because of it.
func presented(names []string, claims map[string]any) string {
	var b strings.Builder
	for _, name := range names {
		s, ok := claimScalar(claims[name])
		if !ok {
			continue
		}
		if s == "" || strings.ContainsFunc(s, func(r rune) bool {
			return r == ' ' || r == '"' || r == '\\' || !strconv.IsPrint(r)
		}) {
			s = strconv.Quote(s)
		}
		b.WriteString(" " + name + "=" + s)
	}
	return b.String()
}

// oidcVerifier validates a provider-issued OIDC token against the
// issuer's published JWKS. The issuer's discovery and keys are its
// issuerState, shared by every source in the process that trusts the
// same issuer over the same client, and built in the background
// (Warm), so a network blip fails a deploy — which is safe (the old
// version keeps serving) — rather than failing config load for the
// whole server, and no token waits on it.
type oidcVerifier struct {
	issuer      string
	audience    string
	claims      map[string]string
	attribution []string
	client      *http.Client
	clock       Clock // RefetchInterval runs on it, for discovery and the key set; nil is the system clock
}

func (v *oidcVerifier) Label() string { return "oidc:" + v.issuer }

// state is the issuer's shared discovery and key set.
func (v *oidcVerifier) state() *issuerState {
	clock := v.clock
	if clock == nil {
		clock = systemClock{}
	}
	return issuerFor(v.issuer, v.client, clock)
}

// ensure is the IDTokenVerifier the provider's Verifier would build —
// the issuer's advertised algorithms that go-oidc supports, RS256 when
// it advertises none — over the issuer's keySet in place of go-oidc's
// RemoteKeySet, once a discovery of the issuer has succeeded; until
// then, issuerState.ready's answer. Built per call, so a reload's new
// discovery reaches every source at once.
func (v *oidcVerifier) ensure() (*oidc.IDTokenVerifier, error) {
	keys, algs, err := v.state().ready()
	if err != nil {
		return nil, err
	}
	return oidc.NewVerifier(v.issuer, keys, &oidc.Config{ClientID: v.audience, SupportedSigningAlgs: algs}), nil
}

// issuerKey names one issuer's shared state: the issuer, the client
// it is fetched over (NewJWKSClient's, one per allow_insecure_http
// setting, so the same across reloads) and the clock its
// RefetchInterval runs on.
type issuerKey struct {
	url    string
	client *http.Client
	clock  Clock
}

// issuers holds every issuer's state the process has trusted. An
// issuer a reload drops keeps its entry, a discovery document and a
// key set, until the process ends; trusting it again picks it up.
var (
	issuersMu sync.Mutex
	issuers   = map[issuerKey]*issuerState{}
)

func issuerFor(url string, client *http.Client, clock Clock) *issuerState {
	issuersMu.Lock()
	defer issuersMu.Unlock()
	key := issuerKey{url, client, clock}
	is, ok := issuers[key]
	if !ok {
		is = &issuerState{url: url, client: client, clock: clock}
		issuers[key] = is
	}
	return is
}

// issuerState is one issuer's discovery and key set. A discovery runs
// in the background: Warm begins one whenever none is under way, and a
// token, while none has succeeded, when RefetchInterval has passed
// since the last began. One that succeeds replaces the advertised
// algorithms and begins a key fetch; one that fails leaves what an
// earlier one found in use.
type issuerState struct {
	url    string
	client *http.Client
	clock  Clock

	mu       sync.Mutex
	keys     *keySet       // nil until a discovery succeeds; replaced only when the jwks_uri changes
	algs     []string      // the advertised algorithms go-oidc supports, from the last discovery that succeeded
	tried    time.Time     // when the last discovery began; zero before the first
	err      error         // the last discovery's error; nil when it succeeded
	inflight chan struct{} // closed when the discovery under way ends; nil when none is
}

// ready is the issuer's key set and algorithms once a discovery has
// succeeded. Until then it begins one when none is under way and
// RefetchInterval has passed since the last began, or none ever has,
// and answers at once: with the last discovery's error, or, before any
// has ended, a refusal saying one is under way.
func (is *issuerState) ready() (*keySet, []string, error) {
	is.mu.Lock()
	defer is.mu.Unlock()
	if is.keys != nil {
		return is.keys, is.algs, nil
	}
	is.begin(false)
	if is.err != nil {
		return nil, nil, is.err
	}
	return nil, nil, fmt.Errorf("oidc discovery for %s: under way, so a retry once that ends may go through", is.url)
}

// warm begins a discovery whatever RefetchInterval says, unless one is
// under way (Warm).
func (is *issuerState) warm() {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.begin(true)
}

// begin starts a discovery when none is under way and either force or
// RefetchInterval has passed since the last began. is.mu is held.
func (is *issuerState) begin(force bool) {
	if now := is.clock.Now(); is.inflight == nil && (force || is.tried.IsZero() || now.Sub(is.tried) >= RefetchInterval) {
		is.inflight, is.tried = make(chan struct{}), now
		go is.discover(is.inflight)
	}
}

// discover records one discovery's outcome — the algorithms and the
// key set, with a fetch of its keys begun whatever RefetchInterval
// says, or the error — and only then closes done.
func (is *issuerState) discover(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	jwksURL, algs, err := is.provide(ctx)
	is.mu.Lock()
	defer is.mu.Unlock()
	is.err = err
	if err == nil {
		if is.keys == nil || is.keys.url != jwksURL {
			next := &keySet{url: jwksURL, client: is.client, clock: is.clock}
			if is.keys != nil {
				// A moved jwks_uri: the keys held stay in use until the
				// new set's first fetch replaces them.
				next.keys = is.keys.held()
			}
			is.keys = next
		}
		is.algs = algs
		_, _, _ = is.keys.refresh(true)
	}
	is.inflight = nil
	close(done)
}

// provide is the issuer's jwks_uri and the advertised algorithms that
// go-oidc supports, from its discovery document.
func (is *issuerState) provide(ctx context.Context) (jwksURL string, algs []string, err error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, is.client), is.url)
	if err != nil {
		err = fmt.Errorf("oidc discovery for %s: %w", is.url, err)
		// An issuer that answers as someone else (a misnamed issuer in
		// the config) is not the issuer being out of reach.
		var mismatch *oidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			return "", nil, err
		}
		return "", nil, unavailable{"oidc:" + is.url, err}
	}
	// NewProvider has decoded this document already, so the decode
	// cannot fail on it now.
	var meta struct {
		JWKS string   `json:"jwks_uri"`
		Algs []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := provider.Claims(&meta); err != nil {
		return "", nil, fmt.Errorf("oidc discovery for %s: %w", is.url, err)
	}
	for _, a := range meta.Algs {
		if slices.Contains(signingAlgs, jose.SignatureAlgorithm(a)) {
			algs = append(algs, a)
		}
	}
	return meta.JWKS, algs, nil
}

// keyFetchFailed prefixes the error a verify returns when the issuer's
// keys could not be fetched — the issuer down, or answering anything
// but 200: go-oidc's verifier flattens (%v) what the key set returns
// into "failed to verify signature: <it>", and a failed fetch's error
// begins "fetching keys" (keySet.fetch), so the text is the only
// handle. TestOIDCVerifierIssuerDown holds the first half against the
// pinned library, so a bump that changes it fails there rather than
// silently turning an outage back into a plain refusal, which would
// lose the journal line naming the source.
const keyFetchFailed = "failed to verify signature: fetching keys"

func (v *oidcVerifier) verify(ctx context.Context, rawToken string) (Identity, error) {
	// Nothing here waits, so the caller's context decides nothing: a
	// valid token is accepted even from a client that half-closed its
	// side (which ends the context) and still reads the answer.
	idv, err := v.ensure()
	if err != nil {
		return Identity{}, err
	}
	tok, err := idv.Verify(oidc.ClientContext(ctx, v.client), rawToken)
	if err != nil {
		// go-oidc checks the signature before any claim (verifyJWT,
		// then issuer, audience and expiry), and a token whose kid the
		// key set holds no key for gets the last fetch's error; so
		// during an outage such a token lands here, while one under a
		// key the set holds is judged as ever.
		if strings.HasPrefix(err.Error(), keyFetchFailed) {
			return Identity{}, unavailable{v.Label(), err}
		}
		return Identity{}, err
	}
	// go-oidc unmarshals into json.RawMessage by copying the verified
	// claim bytes; decodeClaims then re-parses them with numbers kept as
	// json.Number (see decodeClaims for why %v on a float64 is unsafe).
	var raw json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return Identity{}, err
	}
	claims, err := decodeClaims(raw)
	if err != nil {
		return Identity{}, err
	}
	if err := matchClaims(v.claims, claims); err != nil {
		return Identity{}, presentedErr(v.attribution, claims, err)
	}
	return Identity{By: attribute(v.Label(), v.attribution, claims), claims: claims}, nil
}

// decodeClaims unmarshals a JWT claim set with numbers preserved as
// json.Number instead of float64. matchClaims stringifies each claim to
// compare it, and %v on a float64 mangles numeric identity claims:
// round-numbered IDs render in scientific notation (100000000 → "1e+08")
// and integers above 2^53 lose precision, so a pinned `claim
// repository_id 100000000` (a documented identity constraint) would
// silently never match. json.Number carries the exact source digits.
func decodeClaims(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// localVerifier validates a JWT signed by the operator's local private
// key against the configured public key. Standard-claim checks (exp,
// nbf, iat, aud) and the lifetime bound are enforced here because
// there is no OIDC provider to do them.
type localVerifier struct {
	audience    string
	pub         ed25519.PublicKey
	maxTTL      time.Duration // the longest exp - iat accepted
	keyPath     string
	claims      map[string]string
	attribution []string
	clock       Clock // what exp, nbf and iat are judged against; nil is the system clock
}

func (v *localVerifier) Label() string { return "local:" + v.keyPath }

func (v *localVerifier) verify(_ context.Context, rawToken string) (Identity, error) {
	tok, err := jwt.ParseSigned(rawToken, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return Identity{}, err
	}
	var std jwt.Claims
	var raw json.RawMessage
	// Claims verifies the signature against v.pub, then unmarshals the
	// payload into both the standard-claims struct and the raw bytes.
	if err := tok.Claims(v.pub, &std, &raw); err != nil {
		return Identity{}, err
	}
	// Decode with numbers kept as json.Number, so a numeric claim in a
	// hand-minted local token compares the same way as an OIDC one.
	all, err := decodeClaims(raw)
	if err != nil {
		return Identity{}, err
	}
	// ValidateWithLeeway only checks exp when present, so a local token
	// with no expiry would be accepted forever. Require it, and the
	// iat its lifetime is measured from: the bound below is what a
	// leaked token is worth.
	if std.Expiry == nil {
		return Identity{}, fmt.Errorf("local token has no exp claim")
	}
	if std.IssuedAt == nil {
		return Identity{}, fmt.Errorf("local token has no iat claim, which max_token_ttl is measured from")
	}
	// exp against iat + max, not exp - iat against max: the difference
	// saturates at time.Duration's largest value, so against a bound
	// that large it would let a longer-lived token through. go-jose
	// keeps both within ±2^62 s, so neither the sum nor the times
	// printed below overflow.
	if exp, iat := std.Expiry.Time(), std.IssuedAt.Time(); exp.After(iat.Add(v.maxTTL)) {
		return Identity{}, fmt.Errorf("local token's exp (%s) is more than this source's max_token_ttl %v after its iat (%s)",
			exp.UTC().Format(time.RFC3339), v.maxTTL, iat.UTC().Format(time.RFC3339))
	}
	now := time.Now()
	if v.clock != nil {
		now = v.clock.Now()
	}
	expected := jwt.Expected{Time: now}
	if v.audience != "" {
		expected.AnyAudience = jwt.Audience{v.audience}
	}
	if err := std.ValidateWithLeeway(expected, oidcLeeway); err != nil {
		return Identity{}, err
	}
	if err := matchClaims(v.claims, all); err != nil {
		return Identity{}, presentedErr(v.attribution, all, err)
	}
	return Identity{By: attribute(v.Label(), v.attribution, all), claims: all}, nil
}

// presentedErr is a claim refusal followed by the identity the token
// presented — the signature verified, so the values are the issuer's
// word, and they are what the operator lacks when the pinned claim is
// the wrong one: `claim "repository" mismatch, presented
// repository=o/other ref=refs/heads/main actor=alice`. The reason
// comes first so that boundRefusal's cut, when an identity is long,
// takes the identity and never the reason. A token carrying none of
// the attribution claims reads as `presented nothing`.
func presentedErr(names []string, claims map[string]any, err error) error {
	p := presented(names, claims)
	if p == "" {
		p = " nothing"
	}
	return fmt.Errorf("%w, presented%s", err, p)
}

// matchClaims requires every constraint to equal the token's claim
// value (string-compared). Missing or mismatched → error naming the
// first offending claim, so a misconfigured allowlist is diagnosable.
func matchClaims(want map[string]string, got map[string]any) error {
	for _, name := range sortedKeys(want) {
		v, ok := got[name]
		if !ok {
			return fmt.Errorf("claim %q absent from token", name)
		}
		s, ok := claimScalar(v)
		if !ok {
			// An array/object/null claim is not an identity value; it must
			// never match a configured string. (Falling through to
			// fmt.Sprintf("%v", …) would let a config value like "[admin]"
			// match the array claim ["admin"].)
			return fmt.Errorf("claim %q is not a scalar; identity constraints match scalar claims only", name)
		}
		if s != want[name] {
			return fmt.Errorf("claim %q mismatch", name)
		}
	}
	return nil
}

// claimScalar renders a scalar token claim for exact-string comparison
// against the operator's config, reporting ok=false for composite
// (array/object) or null claims — which are never a meaningful identity
// constraint. Scalars format canonically: json.Number by its source
// digits (see decodeClaims), never via %v on a float64.
func claimScalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		// A claim set that reached here without UseNumber has already
		// lost the identity it is meant to prove (2^53+1 is ...992 by
		// now; 1e8 may or may not be 100000000). Refuse rather than
		// format: unreachable from either verifier — both go through
		// decodeClaims, and FuzzMatchClaims pins that it never yields a
		// float64 — so this exists to fail loud if a third path ever
		// appears.
		return "", false
	default:
		return "", false
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
