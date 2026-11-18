package liveswap

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
)

// These commands provide the non-CI ("local") deploy path: an operator
// generates an ed25519 keypair, points a `deploy_trust local` block at
// the public half, and mints short-lived tokens with the private half.
// The same path is what the offline e2e/smoke harness uses in place of
// a real OIDC provider.

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "deploy-keygen",
		Usage: "[--out <path>]",
		Short: "Generate an ed25519 keypair for local deploy tokens",
		Long: `Writes an ed25519 private key to <out> (PKCS#8 PEM, mode 0600) and
the matching public key to <out>.pub (PKIX PEM). Point a
'deploy_trust local { public_key <out>.pub; audience <a> }' block at the
public key, keep the private key on the machine that mints deploy tokens.`,
		Flags: func() *flag.FlagSet {
			fs := flag.NewFlagSet("deploy-keygen", flag.ExitOnError)
			fs.String("out", "deploy.key", "private key output path (public key is <out>.pub)")
			return fs
		}(),
		Func: cmdDeployKeygen,
	})

	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "deploy-token",
		Usage: "--key <path> --audience <aud> [--subject <s>] [--claims k=v,k2=v2] [--ttl 5m]",
		Short: "Mint a short-lived liveswap deploy token",
		Long: `Signs a JWT with a private key from 'deploy-keygen' and prints it to
stdout. Use it as the deploy bearer:

    curl -H "Authorization: Bearer $(moteld deploy-token --key deploy.key --audience deploy.example.com)" ...

The token's audience and any --claims must match the app's
'deploy_trust local' block for the deploy to be authorized.

The token lives for --ttl from now. Whoever holds it can deploy until
then, so mint one per deploy: a box refuses a token that lives longer
than its block's max_token_ttl (24h unless the block sets one), and
this command warns on stderr above 1h.`,
		Flags: func() *flag.FlagSet {
			fs := flag.NewFlagSet("deploy-token", flag.ExitOnError)
			fs.String("key", "deploy.key", "private key path (PKCS#8 PEM ed25519)")
			fs.String("audience", "", "token audience (required; must match deploy_trust)")
			fs.String("subject", "", "token subject (sub claim)")
			fs.String("claims", "", "extra claims as k=v,k2=v2")
			fs.Duration("ttl", 5*time.Minute, "token lifetime; a box refuses one over its max_token_ttl (24h by default), and over 1h this warns")
			return fs
		}(),
		Func: cmdDeployToken,
	})
}

func cmdDeployKeygen(fl caddycmd.Flags) (int, error) {
	out := fl.String("out")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return caddy1, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return caddy1, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return caddy1, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	pubPath := out + ".pub"
	// O_EXCL: never overwrite. Overwriting with WriteFile would leave an
	// existing 0644 file world-readable while reporting 0600, and a
	// partial write could split the pair — refuse instead.
	if err := writeNewFile(out, privPEM, 0o600); err != nil {
		return caddy1, err
	}
	if err := writeNewFile(pubPath, pubPEM, 0o644); err != nil {
		_ = os.Remove(out) // don't leave a private key with no matching public key
		return caddy1, err
	}
	fmt.Fprintf(os.Stderr, "wrote private key %s (0600) and public key %s\n", out, pubPath)
	return 0, nil
}

// writeNewFile creates path with exactly perm, refusing to overwrite an
// existing file (which would keep its own mode).
func writeNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // operator-chosen output path (CLI), created O_EXCL
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; refusing to overwrite (choose a new --out or remove it first)", path)
		}
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func cmdDeployToken(fl caddycmd.Flags) (int, error) {
	keyPath := fl.String("key")
	audience := fl.String("audience")
	if audience == "" {
		return caddy1, fmt.Errorf("--audience is required")
	}
	claims, err := parseClaimsFlag(fl.String("claims"))
	if err != nil {
		return caddy1, err
	}
	priv, err := loadEd25519PrivateKey(keyPath)
	if err != nil {
		return caddy1, err
	}

	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.EdDSA, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return caddy1, err
	}
	ttl := fl.Duration("ttl")
	iat, exp := tokenTimes(time.Now(), ttl)
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return caddy1, err
	}
	// One JSON object: go-jose rejects merging a second map[string]string
	// via a follow-up .Claims() call, so standard and custom claims are
	// combined here.
	payload := map[string]any{
		"aud": audience,
		"iat": iat,
		"exp": exp,
		"jti": hex.EncodeToString(jti),
	}
	if sub := fl.String("subject"); sub != "" {
		payload["sub"] = sub
	}
	for k, v := range claims {
		payload[k] = v
	}
	token, err := jwt.Signed(sig).Claims(payload).Serialize()
	if err != nil {
		return caddy1, err
	}
	if warning := ttlWarning(ttl); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	fmt.Println(token)
	return 0, nil
}

// tokenTimes is the iat and exp of a token minted at now to live ttl.
// A NumericDate keeps whole seconds, truncating each, so now is
// truncated first: exp - iat is then ttl rounded down, never up, and a
// --ttl equal to a box's max_token_ttl verifies however fractional
// either is.
func tokenTimes(now time.Time, ttl time.Duration) (iat, exp *jwt.NumericDate) {
	now = now.Truncate(time.Second)
	return jwt.NewNumericDate(now), jwt.NewNumericDate(now.Add(ttl))
}

// warnTTL is the --ttl above which deploy-token warns. A box's bound
// is its max_token_ttl, 24h by default: this is lower, because the
// command's own default is minutes and a longer token is a bearer
// credential for whoever reads it from a log, a shell history or a CI
// variable until it expires.
const warnTTL = time.Hour

// ttlWarning is what deploy-token says on stderr for a ttl over
// warnTTL, and empty for one within it. A warning, not a refusal: the
// box's max_token_ttl is the bound, and its operator may have raised
// it.
func ttlWarning(ttl time.Duration) string {
	if ttl <= warnTTL {
		return ""
	}
	return fmt.Sprintf("warning: --ttl %v is over %v: whoever holds the token can deploy until it expires. A box refuses one that lives longer than its deploy_trust local block's max_token_ttl (%v unless the block sets one).",
		ttl, warnTTL, deploytrust.DefaultMaxTokenTTL)
}

// caddy1 is the conventional generic failure exit code in Caddy's
// command funcs (caddy.ExitCodeFailedStartup).
const caddy1 = 1

// reservedClaims are set from dedicated flags (or the JWT machinery) and
// must not be overridable via --claims, or a minted token could
// contradict --audience/--subject/--ttl.
var reservedClaims = map[string]bool{
	"aud": true, "iss": true, "sub": true,
	"exp": true, "iat": true, "nbf": true, "jti": true,
}

func parseClaimsFlag(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("claim %q is not k=v", pair)
		}
		if reservedClaims[k] {
			return nil, fmt.Errorf("claim %q is reserved (use --audience / --subject / --ttl)", k)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

func loadEd25519PrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied signing key path (CLI)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ed25519 key (%T)", path, parsed)
	}
	return key, nil
}
