package access

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The vectors: the draft format's that still apply (named P<n> and
// N<n> after its numbers), the ones this form's cut makes (a scope, an
// act, a POST: all refused), and at least one for every reason
// CheckToken can give. Every key here is made in the test and lives
// only in its memory; the vectors OpenSSH signed are in
// testdata/token-openssh.json, with their public keys alone.

const (
	testIdentity       = "deploy.example.test"
	testNow      int64 = 1794657600 // 2026-11-14 12:00:00 UTC, the vectors' clock
)

// testKey is a key made in the test. sign signs data as the key would,
// with the given flags for an sk- key (ignored otherwise).
type testKey struct {
	pub    ssh.PublicKey
	signer ssh.Signer // plain keys only
	sign   func(data []byte, flags byte) ssh.Signature
}

func plainKey(priv crypto.Signer, algo string) *testKey {
	s, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		panic(err)
	}
	return &testKey{pub: s.PublicKey(), signer: s, sign: func(data []byte, _ byte) ssh.Signature {
		var sig *ssh.Signature
		var err error
		if algo != "" {
			sig, err = s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, data, algo)
		} else {
			sig, err = s.Sign(rand.Reader, data)
		}
		if err != nil {
			panic(err)
		}
		return *sig
	}}
}

// skSigned is what an authenticator signs for an sk- key (OpenSSH's
// PROTOCOL.u2f): the application's hash, the flags, the counter, the
// message's hash. The counter is always 7 here.
func skSigned(data []byte, flags byte) []byte {
	app := sha256.Sum256([]byte("ssh:"))
	msg := sha256.Sum256(data)
	return slices.Concat(app[:], []byte{flags, 0, 0, 0, 7}, msg[:])
}

func skTrailer(flags byte) []byte { return []byte{flags, 0, 0, 0, 7} }

// skEd25519Key and skECDSAKey make sk- keys without an authenticator:
// the same wire forms and signatures OpenSSH's software provider makes,
// which x/crypto verifies as it would a hardware key's.
func skEd25519Key() *testKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	k, err := ssh.ParsePublicKey(ssh.Marshal(struct {
		Type string
		Key  []byte
		App  string
	}{ssh.KeyAlgoSKED25519, pub, "ssh:"}))
	if err != nil {
		panic(err)
	}
	return &testKey{pub: k, sign: func(data []byte, flags byte) ssh.Signature {
		return ssh.Signature{Format: ssh.KeyAlgoSKED25519, Blob: ed25519.Sign(priv, skSigned(data, flags)), Rest: skTrailer(flags)}
	}}
}

func skECDSAKey() *testKey {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	point, err := priv.PublicKey.Bytes()
	if err != nil {
		panic(err)
	}
	k, err := ssh.ParsePublicKey(ssh.Marshal(struct {
		Type, Curve string
		Key         []byte
		App         string
	}{ssh.KeyAlgoSKECDSA256, "nistp256", point, "ssh:"}))
	if err != nil {
		panic(err)
	}
	return &testKey{pub: k, sign: func(data []byte, flags byte) ssh.Signature {
		digest := sha256.Sum256(skSigned(data, flags))
		r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
		if err != nil {
			panic(err)
		}
		return ssh.Signature{Format: ssh.KeyAlgoSKECDSA256, Blob: ssh.Marshal(struct{ R, S *big.Int }{r, s}), Rest: skTrailer(flags)}
	}}
}

type testKeySet struct {
	ed, p256, p384, p521, skEd, skEc, rsa *testKey
	unlisted                              *testKey
}

// testKeys are made once per run: an RSA key takes a moment.
var testKeys = sync.OnceValue(func() *testKeySet {
	ec := func(c elliptic.Curve) *testKey {
		k, err := ecdsa.GenerateKey(c, rand.Reader)
		if err != nil {
			panic(err)
		}
		return plainKey(k, "")
	}
	ed := func() *testKey {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		return plainKey(k, "")
	}
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &testKeySet{
		ed: ed(), p256: ec(elliptic.P256()), p384: ec(elliptic.P384()), p521: ec(elliptic.P521()),
		skEd: skEd25519Key(), skEc: skECDSAKey(), rsa: plainKey(r, ssh.KeyAlgoRSASHA512),
		unlisted: ed(),
	}
})

// lookupOf lists keys by fingerprint, as the registry will.
func lookupOf(keys ...ssh.PublicKey) KeyLookup {
	m := map[string]ssh.PublicKey{}
	for _, k := range keys {
		m[ssh.FingerprintSHA256(k)] = k
	}
	return func(fp string) (ssh.PublicKey, bool) {
		k, ok := m[fp]
		return k, ok
	}
}

// listed is every test key but the unlisted one, the RSA key included:
// a token from it must be refused by its type, not by its absence.
func (k *testKeySet) listed() []ssh.PublicKey {
	return []ssh.PublicKey{k.ed.pub, k.p256.pub, k.p384.pub, k.p521.pub, k.skEd.pub, k.skEc.pub, k.rsa.pub}
}

// sigOpts sets an SSHSIG structure's fields; the zero value is what
// ssh-keygen -Y sign makes.
type sigOpts struct {
	version  uint32 // 0: 1
	ns       string // "": tokenNamespace
	reserved []byte
	hashAlg  string // "": sha512. A name that is neither is hashed with sha512
	flags    *byte  // sk- keys: nil is the touch flag alone
	skRest   []byte // replaces the sk- trailer after signing
	sigTail  []byte // bytes after the signature, inside its string
	sigBytes []byte // replaces the signature string whole
	keyWire  []byte // replaces the key in the structure, after signing
}

func flags(b byte) *byte { return &b }

// sshsig signs claims as `ssh-keygen -Y sign` would, then applies o.
func sshsig(k *testKey, claims string, o sigOpts) []byte {
	if o.version == 0 {
		o.version = 1
	}
	if o.ns == "" {
		o.ns = tokenNamespace
	}
	if o.hashAlg == "" {
		o.hashAlg = "sha512"
	}
	var digest []byte
	if o.hashAlg == "sha256" {
		s := sha256.Sum256([]byte(claims))
		digest = s[:]
	} else {
		s := sha512.Sum512([]byte(claims))
		digest = s[:]
	}
	signed := append([]byte(sshsigMagic), ssh.Marshal(struct {
		Namespace string
		Reserved  []byte
		HashAlg   string
		Hash      []byte
	}{o.ns, o.reserved, o.hashAlg, digest})...)
	f := byte(skUserPresent)
	if o.flags != nil {
		f = *o.flags
	}
	sig := k.sign(signed, f)
	if o.skRest != nil {
		sig.Rest = o.skRest
	}
	sigBytes := append(ssh.Marshal(sig), o.sigTail...)
	if o.sigBytes != nil {
		sigBytes = o.sigBytes
	}
	keyWire := k.pub.Marshal()
	if o.keyWire != nil {
		keyWire = o.keyWire
	}
	return append([]byte(sshsigMagic), ssh.Marshal(tokenSig{o.version, keyWire, o.ns, o.reserved, o.hashAlg, sigBytes})...)
}

var b64u = base64.RawURLEncoding

func tok(claims string, blob []byte) string {
	return tokenEnvelope + b64u.EncodeToString([]byte(claims)) + "." + b64u.EncodeToString(blob)
}

func hdr(token string) []string { return []string{tokenScheme + " " + token} }

func claimsDoc(aud string, iat, exp int64) string {
	return fmt.Sprintf(`{"aud":"%s","iat":%d,"exp":%d}`, aud, iat, exp)
}

// signed is the header of a token over claims signed by k.
func signed(k *testKey, claims string) []string {
	return hdr(tok(claims, sshsig(k, claims, sigOpts{})))
}

// tokenCase is one vector. An empty method is GET, an empty host the
// box's identity. expect is "accept", "not-mine", or the reason a
// refusal must carry: any other reason fails the test.
type tokenCase struct {
	name     string
	auth     []string
	method   string
	host     string
	lookup   KeyLookup // nil: every listed test key
	noLookup bool      // CheckToken gets a nil lookup
	key      *testKey  // accepted cases: the key that must be named
	expect   string
}

func verdict(req TokenRequest, identity string, now int64, lookup KeyLookup) (string, VerifiedToken) {
	v, err := CheckToken(req, identity, time.Unix(now, 0), lookup)
	var r *TokenRefusal
	switch {
	case err == nil:
		return "accept", v
	case errors.Is(err, ErrNoPersonToken):
		return "not-mine", v
	case errors.As(err, &r):
		return r.Reason, v
	}
	return "error: " + err.Error(), v
}

func (c tokenCase) run(k *testKeySet) (string, VerifiedToken) {
	if c.method == "" {
		c.method = "GET"
	}
	if c.host == "" {
		c.host = testIdentity
	}
	lookup := c.lookup
	if lookup == nil {
		lookup = lookupOf(k.listed()...)
	}
	if c.noLookup {
		lookup = nil
	}
	return verdict(TokenRequest{Authorization: c.auth, Method: c.method, Host: c.host}, testIdentity, testNow, lookup)
}

// flipLowBit changes the last base64url character's lowest bit: for a
// part whose length leaves unused bits, the same bytes in a loose form.
func flipLowBit(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[i^1])
}

func tokenCases(k *testKeySet) []tokenCase {
	n := testNow
	c0 := claimsDoc(testIdentity, n-30, n+570)
	good := sshsig(k.ed, c0, sigOpts{})
	goodTok := tok(c0, good)
	claims64, sig64, _ := strings.Cut(strings.TrimPrefix(goodTok, tokenEnvelope), ".")
	withSig := func(claims string) []string { return hdr(tok(claims, good)) }
	cert := &ssh.Certificate{Key: k.ed.pub, CertType: ssh.UserCert, ValidPrincipals: []string{"alice"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, k.ed.signer); err != nil {
		panic(err)
	}
	return []tokenCase{
		// Accepted.
		{name: "P1-read-ed25519", auth: signed(k.ed, c0), key: k.ed, expect: "accept"},
		{name: "P2-read-ecdsa-p256", auth: signed(k.p256, c0), key: k.p256, expect: "accept"},
		{name: "read-ecdsa-p384", auth: signed(k.p384, c0), key: k.p384, expect: "accept"},
		{name: "read-ecdsa-p521", auth: signed(k.p521, c0), key: k.p521, expect: "accept"},
		{name: "read-sk-ed25519", auth: signed(k.skEd, c0), key: k.skEd, expect: "accept"},
		{name: "read-sk-ecdsa-p256", auth: signed(k.skEc, c0), key: k.skEc, expect: "accept"},
		{name: "read-sk-touch-and-pin", auth: hdr(tok(c0, sshsig(k.skEd, c0, sigOpts{flags: flags(0x05)}))), key: k.skEd, expect: "accept"},
		{name: "P5-read-sha256", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{hashAlg: "sha256"}))), key: k.ed, expect: "accept"},
		{name: "P6-expired-within-skew", auth: signed(k.ed, claimsDoc(testIdentity, n-600, n-30)), key: k.ed, expect: "accept"},
		{name: "expired-at-the-skew", auth: signed(k.ed, claimsDoc(testIdentity, n-600, n-60)), key: k.ed, expect: "accept"},
		{name: "iat-ahead-at-the-skew", auth: signed(k.ed, claimsDoc(testIdentity, n+60, n+600)), key: k.ed, expect: "accept"},
		{name: "life-at-the-cap", auth: signed(k.ed, claimsDoc(testIdentity, n-100, n+500)), key: k.ed, expect: "accept"},
		{name: "scheme-in-lower-case", auth: []string{"motel " + goodTok}, key: k.ed, expect: "accept"},
		{name: "host-in-upper-case-with-port", auth: hdr(goodTok), host: "Deploy.Example.TEST:443", key: k.ed, expect: "accept"},

		// 1. Not a person's request.
		{name: "N20-person-token-as-bearer", auth: []string{"Bearer " + goodTok}, expect: "not-mine"},
		{name: "no-authorization", auth: nil, expect: "not-mine"},
		{name: "two-bearer-headers", auth: []string{"Bearer a", "Bearer b"}, expect: "not-mine"},
		{name: "scheme-glued-to-token", auth: []string{tokenScheme + goodTok}, expect: "not-mine"},

		// 2.
		{name: "N21-bearer-and-motel", auth: []string{"Bearer x", tokenScheme + " " + goodTok}, expect: "authorization-not-one-header"},
		{name: "two-motel-headers", auth: []string{tokenScheme + " " + goodTok, tokenScheme + " " + goodTok}, expect: "authorization-not-one-header"},

		// 3.
		{name: "N22-too-long", auth: hdr(tokenEnvelope + strings.Repeat("A", maxTokenLen)), expect: "too-long"},
		// The caps as numbers, not as the constants: changing a cap, or
		// > to >=, fails here.
		{name: "token-of-8193-bytes", auth: hdr("m1." + strings.Repeat("A", 8190)), expect: "too-long"},
		{name: "token-of-8192-bytes-is-parsed", auth: hdr("m1." + strings.Repeat("A", 8189)), expect: "envelope-parts"},

		// 4.
		{name: "N19-jwt-under-motel", auth: hdr("eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJkZXBsb3kuZXhhbXBsZS50ZXN0In0.c2ln"), expect: "envelope-prefix"}, // gitleaks:allow — made up for this test
		{name: "envelope-in-upper-case", auth: hdr("M1." + claims64 + "." + sig64), expect: "envelope-prefix"},
		{name: "scheme-alone", auth: []string{tokenScheme}, expect: "envelope-prefix"},
		{name: "two-spaces-after-scheme", auth: []string{tokenScheme + "  " + goodTok}, expect: "envelope-prefix"},
		{name: "one-part", auth: hdr(tokenEnvelope + claims64), expect: "envelope-parts"},
		{name: "three-parts-a-later-form", auth: hdr(goodTok + "." + claims64), expect: "envelope-parts"},
		{name: "claims-padded", auth: hdr(tokenEnvelope + claims64 + "=." + sig64), expect: "claims-encoding"},
		{name: "claims-standard-alphabet", auth: hdr(tokenEnvelope + "+" + claims64[1:] + "." + sig64), expect: "claims-encoding"},
		{name: "claims-newline-go-would-skip", auth: hdr(tokenEnvelope + claims64[:10] + "\n" + claims64[10:] + "." + sig64), expect: "claims-encoding"},
		{name: "claims-loose-trailing-bits", auth: hdr(tokenEnvelope + flipLowBit(b64u.EncodeToString([]byte("{}"))) + "." + sig64), expect: "claims-encoding"},
		{name: "claims-of-1025-bytes", auth: hdr(tok(strings.Repeat("a", 1025), good)), expect: "claims-too-long"},
		{name: "claims-of-1024-bytes-reach-the-signature", auth: hdr(tok(strings.Repeat("a", 1024), good)), expect: "signature"},
		{name: "sig-padded", auth: hdr(goodTok + "="), expect: "sig-encoding"},
		{name: "sig-crlf-go-would-skip", auth: hdr(tokenEnvelope + claims64 + "." + sig64[:20] + "\r\n" + sig64[20:]), expect: "sig-encoding"},

		// 5.
		{name: "no-magic", auth: hdr(tok(c0, append([]byte("SSHSIX"), good[6:]...))), expect: "blob-magic"},
		{name: "empty-signature-part", auth: hdr(tokenEnvelope + claims64 + "."), expect: "blob-magic"},
		{name: "N8-byte-after-blob", auth: hdr(tok(c0, append(slices.Clone(good), 0))), expect: "blob-structure"},
		{name: "blob-cut-short", auth: hdr(tok(c0, good[:len(good)-1])), expect: "blob-structure"},
		{name: "version-2", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{version: 2}))), expect: "blob-version"},

		// 6.
		{name: "N25-rsa-listed-but-refused", auth: signed(k.rsa, c0), expect: "key-type"},
		{name: "N23-rsa-ssh-rsa-sha1", auth: hdr(tok(c0, sshsig(plainKeySHA1(k.rsa), c0, sigOpts{}))), expect: "key-type"},
		{name: "certificate-as-key", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{keyWire: cert.Marshal()}))), expect: "key-type"},
		{name: "key-not-a-key", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{keyWire: []byte{1, 2, 3}}))), expect: "key-type"},
		{name: "N10-key-not-listed", auth: signed(k.unlisted, c0), expect: "key-not-listed"},
		{name: "unlisted-before-any-signature-check", auth: hdr(tok(c0, sshsig(k.unlisted, c0, sigOpts{sigBytes: []byte("garbage")}))), expect: "key-not-listed"},
		{name: "lookup-names-another-key", auth: signed(k.ed, c0), lookup: func(string) (ssh.PublicKey, bool) { return k.p256.pub, true }, expect: "key-not-listed"},
		{name: "lookup-says-yes-with-no-key", auth: signed(k.ed, c0), lookup: func(string) (ssh.PublicKey, bool) { return nil, true }, expect: "key-not-listed"},
		{name: "lookup-gives-a-key-but-says-no", auth: signed(k.ed, c0), lookup: func(string) (ssh.PublicKey, bool) { return k.ed.pub, false }, expect: "key-not-listed"},
		{name: "no-lookup-lists-nothing", auth: signed(k.ed, c0), noLookup: true, expect: "key-not-listed"},

		// 7.
		{name: "N1-namespace-git", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{ns: "git"}))), expect: "signature-namespace"},
		{name: "N2-namespace-file", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{ns: "file"}))), expect: "signature-namespace"},
		{name: "namespace-with-a-suffix", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{ns: tokenNamespace + ".x"}))), expect: "signature-namespace"},
		{name: "namespace-in-another-case", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{ns: "Motel-Access-v1@motel.smallhours.org"}))), expect: "signature-namespace"},
		{name: "reserved-not-empty", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{reserved: []byte("x")}))), expect: "signature-reserved"},
		{name: "N24-hash-sha1", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{hashAlg: "sha1"}))), expect: "signature-hash"},
		{name: "hash-name-in-upper-case", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{hashAlg: "SHA512"}))), expect: "signature-hash"},
		{name: "signature-not-a-signature", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{sigBytes: []byte{0, 0, 0, 1, 'x'}}))), expect: "signature-form"},
		{name: "N9-byte-inside-signature", auth: hdr(tok(c0, sshsig(k.ed, c0, sigOpts{sigTail: []byte{0}}))), expect: "signature-trailing"},
		{name: "ecdsa-byte-inside-signature", auth: hdr(tok(c0, sshsig(k.p256, c0, sigOpts{sigTail: []byte{0}}))), expect: "signature-trailing"},
		{name: "sk-trailer-short", auth: hdr(tok(c0, sshsig(k.skEd, c0, sigOpts{skRest: []byte{1, 0, 0, 7}}))), expect: "signature-sk-trailer"},
		{name: "sk-trailer-long", auth: hdr(tok(c0, sshsig(k.skEc, c0, sigOpts{skRest: []byte{1, 0, 0, 0, 7, 0}}))), expect: "signature-sk-trailer"},
		{name: "sk-no-touch", auth: hdr(tok(c0, sshsig(k.skEd, c0, sigOpts{flags: flags(0)}))), expect: "signature-sk-touch"},
		{name: "sk-pin-without-touch", auth: hdr(tok(c0, sshsig(k.skEc, c0, sigOpts{flags: flags(0x04)}))), expect: "signature-sk-touch"},
		{name: "sk-flags-changed-after-signing", auth: hdr(tok(c0, sshsig(k.skEd, c0, sigOpts{skRest: skTrailer(0x05)}))), expect: "signature"},
		{name: "claims-changed-after-signing", auth: withSig(claimsDoc(testIdentity, n-30, n+3570)), expect: "signature"},
		{name: "signed-by-another-key", auth: hdr(tok(c0, sshsig(k.unlisted, c0, sigOpts{keyWire: k.ed.pub.Marshal()}))), expect: "signature"},
		{name: "claims-read-only-after-signature", auth: withSig("not json"), expect: "signature"},

		// 8. Decision 5: the claims are aud, iat and exp, nothing else.
		{name: "P4-scope", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":1794657570,"exp":1794658170,"scope":"app:blog"}`), expect: "claims-json"},
		{name: "P7-act", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":1794657595,"exp":1794657655,"act":{"method":"POST","target":"/blog?rollback=v1","body_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","nonce":"uFPXqMYr91l5ZYgrUt3GBA"}}`), method: "POST", expect: "claims-json"},
		{name: "N17-role", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":1794657570,"exp":1794658170,"role":"deployer"}`), expect: "claims-json"},
		{name: "claims-not-json", auth: signed(k.ed, "not json"), expect: "claims-json"},
		{name: "claims-empty", auth: signed(k.ed, ""), expect: "claims-json"},
		{name: "claims-an-array", auth: signed(k.ed, "[1]"), expect: "claims-json"},
		{name: "iat-a-string", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":"1794657570","exp":1794658170}`), expect: "claims-json"},
		{name: "iat-with-exponent", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":1.79465757e9,"exp":1794658170}`), expect: "claims-json"},
		{name: "N16-space-after-colon", auth: signed(k.ed, `{"aud": "deploy.example.test","iat":1794657570,"exp":1794658170}`), expect: "claims-not-canonical"},
		{name: "N18-keys-reordered", auth: signed(k.ed, `{"iat":1794657570,"aud":"deploy.example.test","exp":1794658170}`), expect: "claims-not-canonical"},
		{name: "exp-missing", auth: signed(k.ed, `{"aud":"deploy.example.test","iat":1794657570}`), expect: "claims-not-canonical"},
		{name: "aud-twice", auth: signed(k.ed, `{"aud":"deploy.example.test","aud":"deploy.example.test","iat":1794657570,"exp":1794658170}`), expect: "claims-not-canonical"},
		{name: "key-in-upper-case", auth: signed(k.ed, `{"AUD":"deploy.example.test","iat":1794657570,"exp":1794658170}`), expect: "claims-not-canonical"},
		{name: "aud-with-an-escape", auth: signed(k.ed, `{"aud":"\u0064eploy.example.test","iat":1794657570,"exp":1794658170}`), expect: "claims-not-canonical"},
		{name: "claims-then-newline", auth: signed(k.ed, c0+"\n"), expect: "claims-not-canonical"},
		{name: "claims-twice", auth: signed(k.ed, c0+c0), expect: "claims-not-canonical"},

		// 9 to 11, and 13.
		{name: "N6-wrong-audience", auth: signed(k.ed, claimsDoc("deploy.other.test", n-30, n+570)), expect: "aud"},
		{name: "aud-in-upper-case", auth: signed(k.ed, claimsDoc("Deploy.example.test", n-30, n+570)), expect: "aud"},
		{name: "N7-wrong-host", auth: hdr(goodTok), host: "www.example.test", expect: "host"},
		{name: "host-with-trailing-dot", auth: hdr(goodTok), host: testIdentity + ".", expect: "host"},
		{name: "host-a-suffix", auth: hdr(goodTok), host: testIdentity + ".evil.test", expect: "host"},
		{name: "host-bracketed", auth: hdr(goodTok), host: "[deploy.example.test]:443", expect: "host"},
		{name: "host-empty-port", auth: hdr(goodTok), host: "deploy.example.test:", expect: "host"},
		{name: "host-named-port", auth: hdr(goodTok), host: "deploy.example.test:abc", expect: "host"},
		{name: "host-six-digit-port", auth: hdr(goodTok), host: "deploy.example.test:443443", expect: "host"},
		{name: "host-two-ports", auth: hdr(goodTok), host: "deploy.example.test:443:443", expect: "host"},
		{name: "host-empty", auth: hdr(goodTok), host: ":443", expect: "host"},
		{name: "iat-zero", auth: signed(k.ed, claimsDoc(testIdentity, 0, n+60)), expect: "times"},
		{name: "exp-equals-iat", auth: signed(k.ed, claimsDoc(testIdentity, n, n)), expect: "times"},
		{name: "exp-before-iat", auth: signed(k.ed, claimsDoc(testIdentity, n, n-1)), expect: "times"},
		{name: "N4-iat-120s-ahead", auth: signed(k.ed, claimsDoc(testIdentity, n+120, n+600)), expect: "iat-in-future"},
		{name: "iat-one-past-the-skew", auth: signed(k.ed, claimsDoc(testIdentity, n+61, n+600)), expect: "iat-in-future"},
		{name: "N3-expired-100s", auth: signed(k.ed, claimsDoc(testIdentity, n-700, n-100)), expect: "expired"},
		{name: "expired-one-past-the-skew", auth: signed(k.ed, claimsDoc(testIdentity, n-600, n-61)), expect: "expired"},
		{name: "exp-at-int64-max", auth: signed(k.ed, claimsDoc(testIdentity, n, 1<<63-1)), expect: "life"},
		{name: "N14-read-token-on-post", auth: hdr(goodTok), method: "POST", expect: "method"},
		{name: "head", auth: hdr(goodTok), method: "HEAD", expect: "method"},
		{name: "get-in-lower-case", auth: hdr(goodTok), method: "get", expect: "method"},
		{name: "one-hour-on-a-post-is-life-first", auth: signed(k.ed, claimsDoc(testIdentity, n-30, n+3570)), method: "POST", expect: "life"},
		{name: "N5-one-hour", auth: signed(k.ed, claimsDoc(testIdentity, n-30, n+3570)), expect: "life"},
		{name: "life-one-past-the-cap", auth: signed(k.ed, claimsDoc(testIdentity, n-100, n+501)), expect: "life"},
	}
}

// plainKeySHA1 signs as k with ssh-rsa, RSA over SHA-1: what x/crypto
// alone would still verify.
func plainKeySHA1(k *testKey) *testKey {
	return &testKey{pub: k.pub, signer: k.signer, sign: func(data []byte, _ byte) ssh.Signature {
		sig, err := k.signer.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSA)
		if err != nil {
			panic(err)
		}
		return *sig
	}}
}

// TestTokenVectors runs every vector made in the test. A refused one
// must be refused by its own check: another reason fails it.
func TestTokenVectors(t *testing.T) {
	k := testKeys()
	seen := map[string]bool{}
	for _, c := range tokenCases(k) {
		if seen[c.name] {
			t.Fatalf("two vectors named %s", c.name)
		}
		seen[c.name] = true
		got, v := c.run(k)
		if got != c.expect {
			t.Errorf("%s: got %q, want %q", c.name, got, c.expect)
			continue
		}
		if got == "accept" {
			if c.key == nil {
				t.Fatalf("%s: an accepted vector names its key", c.name)
			}
			if want := ssh.FingerprintSHA256(c.key.pub); v.Fingerprint != want {
				t.Errorf("%s: fingerprint %s, want %s", c.name, v.Fingerprint, want)
			}
		}
	}
}

type opensshVectors struct {
	Identity string `json:"identity"`
	Now      int64  `json:"now"`
	Keys     []struct {
		Name        string `json:"name"`
		Line        string `json:"line"`
		Fingerprint string `json:"fingerprint"`
	} `json:"keys"`
	Vectors []struct {
		Name          string   `json:"name"`
		Key           string   `json:"key"`
		Method        string   `json:"method"`
		Host          string   `json:"host"`
		Authorization []string `json:"authorization"`
		Expect        string   `json:"expect"`
	} `json:"vectors"`
}

func loadOpenSSHVectors(t testing.TB) (*opensshVectors, KeyLookup, map[string]string) {
	b, err := os.ReadFile("testdata/token-openssh.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc opensshVectors
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []ssh.PublicKey
	fps := map[string]string{}
	for _, k := range doc.Keys {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Line))
		if err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
		keys = append(keys, pub) // the RSA key too, so its refusal is by type
		fps[k.Name] = k.Fingerprint
	}
	return &doc, lookupOf(keys...), fps
}

// TestTokenOpenSSH runs the vectors `ssh-keygen -Y sign` made, so the
// format is held to OpenSSH and not only to this package's own signer.
// The fingerprint is ssh-keygen -l's, the times the claims'.
func TestTokenOpenSSH(t *testing.T) {
	doc, lookup, fps := loadOpenSSHVectors(t)
	accepted := 0
	for _, v := range doc.Vectors {
		got, res := verdict(TokenRequest{Authorization: v.Authorization, Method: v.Method, Host: v.Host}, doc.Identity, doc.Now, lookup)
		if got != v.Expect {
			t.Errorf("%s: got %q, want %q", v.Name, got, v.Expect)
			continue
		}
		if got != "accept" {
			continue
		}
		accepted++
		if res.Fingerprint != fps[v.Key] {
			t.Errorf("%s: fingerprint %s, ssh-keygen -l says %s", v.Name, res.Fingerprint, fps[v.Key])
		}
		if res.IssuedAt.Unix() != doc.Now-30 || res.ExpiresAt.Unix() != doc.Now+570 {
			t.Errorf("%s: times %v to %v", v.Name, res.IssuedAt, res.ExpiresAt)
		}
	}
	if accepted == 0 {
		t.Fatal("no OpenSSH vector was accepted")
	}
}

// TestTokenEveryReasonHasAVector reads the reasons out of token.go, so
// a check added without a vector that reaches it fails here.
func TestTokenEveryReasonHasAVector(t *testing.T) {
	src, err := os.ReadFile("token.go")
	if err != nil {
		t.Fatal(err)
	}
	reasons := regexp.MustCompile(`(?:refuseToken\(|return )"([a-z][a-z-]*)"`).FindAllSubmatch(src, -1)
	if len(reasons) < 20 {
		t.Fatalf("found only %d reasons in token.go: has the pattern changed?", len(reasons))
	}
	covered := map[string]bool{}
	for _, c := range tokenCases(testKeys()) {
		covered[c.expect] = true
	}
	doc, _, _ := loadOpenSSHVectors(t)
	for _, v := range doc.Vectors {
		covered[v.Expect] = true
	}
	for _, r := range reasons {
		if !covered[string(r[1])] {
			t.Errorf("no vector is refused with %q", r[1])
		}
	}
}

// acceptedHeaders is every accepted vector, OpenSSH's first, with the
// lookup that lists its key, for the mutation test and the fuzz seeds.
func acceptedHeaders(t testing.TB) (headers []string, lookup KeyLookup) {
	k := testKeys()
	doc, opensshLookup, _ := loadOpenSSHVectors(t)
	testLookup := lookupOf(k.listed()...)
	for _, v := range doc.Vectors {
		if v.Expect == "accept" {
			headers = append(headers, v.Authorization[0])
		}
	}
	for _, c := range tokenCases(k) {
		if c.expect == "accept" && c.host == "" && c.lookup == nil {
			headers = append(headers, c.auth[0])
		}
	}
	return headers, func(fp string) (ssh.PublicKey, bool) {
		if k, ok := testLookup(fp); ok {
			return k, true
		}
		return opensshLookup(fp)
	}
}

// TestTokenMutation changes every byte of an accepted token of each key
// type, one byte at a time, each of its eight bits in turn and to the
// next base64url character: nothing is accepted. One token per type
// keeps it to a few seconds; OpenSSH's come first, so they are the
// ones changed where they exist.
func TestTokenMutation(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_."
	headers, lookup := acceptedHeaders(t)
	tried, types := 0, map[string]bool{}
	for _, h := range headers {
		_, v := verdict(TokenRequest{Authorization: []string{h}, Method: "GET", Host: testIdentity}, testIdentity, testNow, lookup)
		key, _ := lookup(v.Fingerprint)
		if types[key.Type()] {
			continue
		}
		types[key.Type()] = true
		scheme, token, _ := strings.Cut(h, " ")
		for i := range len(token) {
			var subs []byte
			for bit := range 8 {
				subs = append(subs, token[i]^1<<bit)
			}
			if j := strings.IndexByte(alphabet, token[i]); j >= 0 {
				subs = append(subs, alphabet[(j+1)%len(alphabet)])
			}
			for _, b := range subs {
				m := token[:i] + string(b) + token[i+1:]
				tried++
				if got, _ := verdict(TokenRequest{Authorization: []string{scheme + " " + m}, Method: "GET", Host: testIdentity}, testIdentity, testNow, lookup); got == "accept" {
					t.Errorf("byte %d of %s… changed to %q: accepted", i, token[:24], b)
				}
			}
		}
	}
	if len(types) != len(tokenKeyTypes) {
		t.Errorf("changed tokens of %d key types, want all %d", len(types), len(tokenKeyTypes))
	}
	t.Logf("%d key types, %d changed tokens refused", len(types), tried)
}

// TestTokenHostNotFolded: a Host with a byte no hostname holds is
// refused, not folded to the box's name. Go's lower-casing folds the
// Kelvin sign to k and a dotted capital I to i, so the identity here
// has both letters.
func TestTokenHostNotFolded(t *testing.T) {
	k := testKeys()
	const identity = "box.kitchen.test"
	auth := signed(k.ed, claimsDoc(identity, testNow-30, testNow+570))
	for host, want := range map[string]string{
		"BOX.KITCHEN.TEST:443":  "accept",
		"box.\u212Aitchen.test": "host",
		"box.k\u0130tchen.test": "host",
		"box.kitchen.t\u00e9st": "host",
	} {
		got, _ := verdict(TokenRequest{Authorization: auth, Method: "GET", Host: host}, identity, testNow, lookupOf(k.listed()...))
		if got != want {
			t.Errorf("Host %q: got %q, want %q", host, got, want)
		}
	}
}

// TestTokenEmptyIdentity: an empty identity is no box's, so nothing
// is accepted for it.
func TestTokenEmptyIdentity(t *testing.T) {
	k := testKeys()
	claims := claimsDoc("", testNow-30, testNow+570)
	got, _ := verdict(TokenRequest{Authorization: signed(k.ed, claims), Method: "GET", Host: ""}, "", testNow, lookupOf(k.listed()...))
	if got != "aud" {
		t.Fatalf("an empty identity: got %q, want aud", got)
	}
}

// FuzzCheckToken feeds header values to CheckToken. It fails on a
// panic, and on any accepted token whose claims and key are not a pair
// the test signed: nothing is accepted that was not signed here. (Pairs,
// not token bytes: an ECDSA signature has a second form that verifies
// alike, and a token derives no id from its signature, so either form
// may be carried.)
func FuzzCheckToken(f *testing.F) {
	headers, lookup := acceptedHeaders(f)
	signedPairs := map[string]bool{}
	pair := func(h string) string {
		_, token, _ := strings.Cut(h, " ")
		claims, _, _ := strings.Cut(strings.TrimPrefix(token, tokenEnvelope), ".")
		return claims
	}
	for _, h := range headers {
		v, err := CheckToken(TokenRequest{Authorization: []string{h}, Method: "GET", Host: testIdentity}, testIdentity, time.Unix(testNow, 0), lookup)
		if err != nil {
			f.Fatalf("seed %s…: %v", h[:24], err)
		}
		signedPairs[pair(h)+" "+v.Fingerprint] = true
		f.Add(h)
	}
	for _, c := range tokenCases(testKeys()) {
		if len(c.auth) == 1 {
			f.Add(c.auth[0])
		}
	}
	f.Fuzz(func(t *testing.T, h string) {
		v, err := CheckToken(TokenRequest{Authorization: []string{h}, Method: "GET", Host: testIdentity}, testIdentity, time.Unix(testNow, 0), lookup)
		if err != nil {
			return
		}
		if !signedPairs[pair(h)+" "+v.Fingerprint] {
			t.Fatalf("accepted a token the test did not sign: %q", h)
		}
	})
}
