package deploytrust

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/smallhoursorg/motel/liveswap/deploytrust/trusttest"
)

// clocked is an OIDC source for iss whose refetch interval runs on c.
func clocked(iss *trusttest.Issuer, c Clock) *oidcVerifier {
	return &oidcVerifier{issuer: iss.URL, audience: "motel", claims: map[string]string{"sub": "ci"}, client: iss.Client, clock: c}
}

// settled is Prime on vs, as a start or a reload does it: it begins
// each OIDC source's discovery and key fetch, and returns once they
// have ended.
func settled(t *testing.T, vs ...Verifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Prime(ctx, vs); err != nil {
		t.Fatalf("priming: %v", err)
	}
}

// waited returns once the discovery and key fetch under way for each
// of vs, a token's, have ended; it begins neither.
func waited(t *testing.T, vs ...Verifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitFor(ctx, vs); err != nil {
		t.Fatalf("waiting on the fetches under way: %v", err)
	}
}

// ciToken is a valid token from iss for a clocked source, under the
// issuer's current key.
func ciToken(t *testing.T, iss *trusttest.Issuer) string {
	t.Helper()
	return iss.Mint(t, iss.Priv, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
}

// refusedPlainly fails the test unless err is a refusal of the token's
// own, not an unavailable.
func refusedPlainly(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || errors.As(err, &unavailable{}) {
		t.Fatalf("%s: err = %v, want a plain refusal", what, err)
	}
}

// fetches fails the test unless iss's JWKS endpoint has answered want
// requests.
func fetches(t *testing.T, iss *trusttest.Issuer, want int64, what string) {
	t.Helper()
	if n := iss.JWKSFetches.Load(); n != want {
		t.Fatalf("%s: %d JWKS fetches, want %d", what, n, want)
	}
}

// A bad signature under a kid the key set holds is refused on the
// spot, however long since the last fetch: a forged token naming the
// issuer's real kid costs the box no outbound request. The refusal
// names the one case that never fetches its way out — the issuer
// replaced the key and kept its kid — and the remedy.
func TestKeySetNeverFetchesForAKidItHolds(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	settled(t, v)
	if _, err := v.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	fetches(t, iss, 1, "primed")
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		c.Advance(RefetchInterval)
		_, err := v.verify(ctx, iss.Mint(t, forger, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute)))
		refusedPlainly(t, err, "bad signature under a held kid")
		if want := `signature does not match the key held for kid "` + iss.KID + `": the token is another issuer's, or, if the issuer replaced that key, systemctl reload moteld`; !strings.Contains(err.Error(), want) {
			t.Fatalf("bad signature under a held kid: reason = %q, want %q", err, want)
		}
	}
	fetches(t, iss, 1, "bad signatures under a held kid")
}

// A kid may name keys of other types, or for encryption, beside the
// signing key (RFC 7517 §4.5): a token under it verifies whichever
// order the set lists them in, as go-oidc matches keys.
func TestKeySetPicksTheKeyThatFitsUnderASharedKid(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, first := range []bool{true, false} {
		iss := trusttest.NewIssuer(t)
		iss.Publish(jose.JSONWebKey{Key: ec.Public(), KeyID: iss.KID}, first)
		iss.Publish(jose.JSONWebKey{Key: enc.Public(), KeyID: iss.KID, Use: "enc"}, first)
		v := clocked(iss, trusttest.NewClock())
		settled(t, v)
		if _, err := v.verify(context.Background(), ciToken(t, iss)); err != nil {
			t.Fatalf("other keys under the kid listed first=%v: refused: %v", first, err)
		}
	}
}

// A token whose kid the key set holds no key for begins a fetch, but
// at most once per RefetchInterval, and is refused without waiting on
// it: within the interval it is refused against what the last fetch
// left; past it, refused saying a fetch is under way, so a retry once
// that ends may go through.
func TestKeySetFetchesForAnUnknownKidOncePerInterval(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	settled(t, v)
	fetches(t, iss, 1, "primed")
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := func(kid string) error {
		_, err := v.verify(ctx, iss.MintKID(t, forger, kid, "motel", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute)))
		return err
	}
	refusedPlainly(t, forged("forged-1"), "unknown kid at the instant of the last fetch")
	c.Advance(RefetchInterval - time.Second)
	err = forged("forged-2")
	refusedPlainly(t, err, "unknown kid a second before the interval")
	// Within the interval the refusal names why no fetch began, when one
	// may, and the reload that fetches now: a rotation in that minute is
	// not read as a trust mismatch.
	next := c.Now().Add(time.Second).UTC().Format(time.RFC3339)
	if want := `kid "forged-2"): the token is another issuer's, or is under a new key they were fetched too soon for; a token from ` + next + ` on fetches them again, as does systemctl reload moteld now`; !strings.Contains(err.Error(), want) {
		t.Fatalf("unknown kid within the interval: reason = %q, want %q", err, want)
	}
	fetches(t, iss, 1, "unknown kids within the interval")
	c.Advance(time.Second)
	err = forged("forged-3")
	refusedPlainly(t, err, "unknown kid at the interval")
	if want := `kid "forged-3"): the token is another issuer's, or is under a new key, which they are being fetched for, so a retry once that ends may go through`; !strings.Contains(err.Error(), want) {
		t.Fatalf("unknown kid at the interval: reason = %q, want %q", err, want)
	}
	waited(t, v)
	fetches(t, iss, 2, "unknown kid at the interval")
}

// A key rotation is refused for the first token under the new key: one
// within RefetchInterval of the last fetch begins none, and one past
// it begins the fetch without waiting on it. The retry once that fetch
// has ended deploys.
func TestKeySetPicksUpARotation(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	v := clocked(iss, c)
	ctx := context.Background()
	settled(t, v)
	if _, err := v.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	iss.Rotate(t)
	rotated := ciToken(t, iss)
	_, err := v.verify(ctx, rotated)
	refusedPlainly(t, err, "new key within the interval")
	fetches(t, iss, 1, "new key within the interval")
	c.Advance(RefetchInterval)
	_, err = v.verify(ctx, rotated)
	refusedPlainly(t, err, "new key past the interval, beginning the fetch")
	waited(t, v)
	fetches(t, iss, 2, "new key past the interval")
	if _, err := v.verify(ctx, rotated); err != nil {
		t.Fatalf("new key after the fetch it began: refused: %v", err)
	}
	fetches(t, iss, 2, "new key after the fetch it began")
}

// Every source that trusts an issuer shares its discovery and keys,
// across reloads (#306): a source built after another has fetched —
// another app's, or the next config's — judges its first token at
// once, with no fetch of its own. A reload (Warm) fetches the keys
// again whatever RefetchInterval says, so a rotation reaches the box
// with no refusal at all; and a reload whose fetch fails keeps the
// keys it had in use.
func TestIssuerStateIsSharedAcrossSourcesAndReloads(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	c := trusttest.NewClock()
	ctx := context.Background()
	first := clocked(iss, c)
	settled(t, first)
	fetches(t, iss, 1, "the first source primed")
	other := &oidcVerifier{issuer: iss.URL, audience: "other", claims: map[string]string{"sub": "ci"}, client: iss.Client, clock: c}
	tok := iss.Mint(t, iss.Priv, "other", map[string]string{"sub": "ci"}, time.Now().Add(5*time.Minute))
	if _, err := other.verify(ctx, tok); err != nil {
		t.Fatalf("a second source's first token: refused: %v", err)
	}
	fetches(t, iss, 1, "a second source")

	iss.Rotate(t)
	settled(t, first) // a reload, well within the interval
	fetches(t, iss, 2, "a reload")
	if _, err := first.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("the new key after a reload: refused: %v", err)
	}

	iss.JWKSDown.Store(true)
	settled(t, first) // a reload whose fetch fails
	if _, err := first.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("a held key after a reload whose fetch failed: refused: %v", err)
	}
	// So does one whose discovery names a moved jwks_uri: the keys held
	// stay in use until the new endpoint's first fetch replaces them.
	iss.JWKSMoved.Store(true)
	settled(t, first)
	if _, err := first.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("a held key after a reload that moved the jwks_uri, its fetch failed: refused: %v", err)
	}
	iss.JWKSDown.Store(false)
	settled(t, first)
	if _, err := first.verify(ctx, ciToken(t, iss)); err != nil {
		t.Fatalf("after the moved endpoint's fetch: refused: %v", err)
	}
}

// heldTransport holds every request until hold is closed, counting
// them.
type heldTransport struct {
	base http.RoundTripper
	hold chan struct{}
	n    atomic.Int64
}

func (h *heldTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h.n.Add(1)
	<-h.hold
	return h.base.RoundTrip(r)
}

// No caller waits on a fetch (#306), the first included: with the
// issuer's answer held, many callers at once are each refused at once,
// saying a fetch is under way, and make one fetch between them. Once
// it has ended, the token verifies.
func TestKeySetNoCallerWaitsOnAFetch(t *testing.T) {
	iss := trusttest.NewIssuer(t)
	held := &heldTransport{base: iss.Client.Transport, hold: make(chan struct{})}
	release := sync.OnceFunc(func() { close(held.hold) })
	defer release()
	ks := &keySet{url: iss.URL + "/jwks", client: &http.Client{Transport: held}, clock: trusttest.NewClock()}
	tok := ciToken(t, iss)

	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			_, err := ks.VerifySignature(context.Background(), tok)
			errs <- err
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("callers waited on the held fetch")
	}
	close(errs)
	for err := range errs {
		if err == nil || !strings.Contains(err.Error(), "which they are being fetched for") {
			t.Fatalf("a caller under the held fetch: err = %v, want a refusal naming the fetch under way", err)
		}
	}
	// The callers can all return before the fetch they began reaches the
	// transport.
	for deadline := time.Now().Add(5 * time.Second); held.n.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("no fetch reached the transport")
		}
		time.Sleep(time.Millisecond)
	}
	if n := held.n.Load(); n != 1 {
		t.Fatalf("%d concurrent callers made %d fetches, want one", callers, n)
	}
	done := ks.underway()
	release()
	if done != nil {
		<-done
	}
	if _, err := ks.VerifySignature(context.Background(), tok); err != nil {
		t.Fatalf("after the fetch: refused: %v", err)
	}
}

// parseKeySet leaves out what it cannot verify with — a key type
// go-jose does not support, a key missing a member, a key for another
// algorithm — and keeps the rest of the set; a body that is not a key
// set is an error.
func TestParseKeySetLeavesOutKeysItCannotUse(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := func(alg, kid string) string {
		b, err := json.Marshal(jose.JSONWebKey{Key: priv.Public(), KeyID: kid, Algorithm: alg})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	body := `{"keys":[` + strings.Join([]string{
		`{"kty":"OKP","crv":"Ed448","x":"AAAA","kid":"ed448"}`,
		`{"kty":"RSA","kid":"no-modulus"}`,
		jwk("RSA-OAEP", "encryption"),
		jwk("RS256", "signing"),
		jwk("", "no-alg"),
	}, ",") + `]}`
	keys, err := parseKeySet([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range keys {
		kids = append(kids, k.KeyID)
	}
	if got := strings.Join(kids, " "); got != "signing no-alg" {
		t.Fatalf("kept %q, want the signing key and the one naming no algorithm", got)
	}
	if _, err := parseKeySet([]byte("<html>")); err == nil {
		t.Fatal("a body that is not a key set must be an error")
	}
}
