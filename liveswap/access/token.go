package access

// The person token: a short claims document a person's own ssh key
// signs with OpenSSH's SSHSIG (`ssh-keygen -Y sign`), sent as
//
//	Authorization: Motel m1.<base64url claims>.<base64url SSHSIG>
//
// CheckToken makes every check of it, numbered in the code as in the
// format's list of checks. The claims are exactly aud, iat and exp,
// and a token is good on a GET alone: a later form that adds a claim
// (an app scope, an act) is refused here, as an unknown key.
//
// The order is the point. Cheap checks that read no key come first;
// then the key, by type and by a lookup of its fingerprint, so a key
// nobody listed never costs a signature check; then the one signature
// check; and only then the claims, which are attacker bytes until the
// signature over them verifies.

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// tokenScheme is the Authorization scheme of a person's token,
	// compared without regard to case as RFC 9110 asks of a scheme.
	// The word is a proposal the owner has not confirmed: this is the
	// one place to change it.
	tokenScheme = "Motel"
	// tokenEnvelope begins every token: version 1 of the envelope. A
	// proposal too, kept here alone.
	tokenEnvelope = "m1."
	// tokenNamespace is the SSHSIG namespace a token is signed in. It
	// keeps a token apart from every other signature the same key
	// makes: a commit is signed in "git", a file in "file", and
	// neither can be read as a token. Chosen by the owner; changing it
	// is a new format.
	tokenNamespace = "motel-access-v1@motel.smallhours.org"

	// maxTokenLen is deploytrust.MaxTokenLen, so one cap holds for
	// both kinds of token. It is checked before anything is parsed.
	maxTokenLen = 8 << 10
	// maxClaimsLen bounds the claims document. Three fields with a
	// hostname of at most 253 bytes fit in a third of it.
	maxClaimsLen = 1 << 10
	// tokenSkew is how far the box's clock and the person's may
	// disagree, either way: deploytrust's oidcLeeway, in seconds.
	tokenSkew = 60
	// maxTokenLife is the longest a token may say it lasts, in seconds
	// from iat to exp. A captured token repeats a read for at most this
	// long, plus the skew.
	maxTokenLife = 600
	// sshsigMagic begins an SSHSIG structure, and the bytes it signs.
	sshsigMagic = "SSHSIG"
	// skUserPresent is the touch flag in an sk- signature's flags byte
	// (OpenSSH's PROTOCOL.u2f): the authenticator saw a person touch it.
	skUserPresent = 0x01
)

// ErrNoPersonToken is CheckToken's answer to a request with no
// Authorization header in the Motel scheme: the request is not a
// person's, and goes to the deploy token's path untouched.
var ErrNoPersonToken = errors.New("no Authorization header in the person token's scheme")

// TokenRefusal is CheckToken's refusal of a token. Reason is one short
// word for the journal; whatever it says, the caller answers with the
// same 401 as every other refusal, so a caller learns nothing from it.
type TokenRefusal struct {
	Reason string
}

func (r *TokenRefusal) Error() string { return "person token refused: " + r.Reason }

// TokenRequest is what CheckToken reads of a request.
type TokenRequest struct {
	// Authorization is every Authorization header value, in order.
	Authorization []string
	Method        string
	// Host is the request's Host as it arrived; CheckToken lowers its
	// case and drops a port.
	Host string
}

// VerifiedToken is a token CheckToken accepted.
type VerifiedToken struct {
	// Fingerprint is the signing key's, SHA256:<base64> as
	// `ssh-keygen -l` prints it: the lookup's key, and the journal's.
	Fingerprint string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// KeyLookup finds a listed key by its fingerprint, in the form
// VerifiedToken.Fingerprint has. A nil KeyLookup lists nothing.
type KeyLookup func(fingerprint string) (ssh.PublicKey, bool)

// tokenKeyTypes are the key types a person may hold (the owner's
// decision): ed25519, ecdsa, and the hardware forms of both. No RSA,
// whatever its size: an RSA signer key signs commits only.
var tokenKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519:    true,
	ssh.KeyAlgoSKED25519:  true,
	ssh.KeyAlgoECDSA256:   true,
	ssh.KeyAlgoECDSA384:   true,
	ssh.KeyAlgoECDSA521:   true,
	ssh.KeyAlgoSKECDSA256: true,
}

// tokenSig is the SSHSIG structure after its six-byte magic
// (OpenSSH's PROTOCOL.sshsig).
type tokenSig struct {
	Version   uint32
	PublicKey []byte
	Namespace string
	Reserved  []byte
	HashAlg   string
	Signature []byte
}

// tokenClaims is the claims document, its fields in the one order they
// are spelled in.
type tokenClaims struct {
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func refuseToken(reason string) (VerifiedToken, error) {
	return VerifiedToken{}, &TokenRefusal{Reason: reason}
}

// CheckToken checks a person's token. identity is the box's identity
// (its box_webhook host, a lowercase bare hostname): the token's aud
// and the request's Host must both equal it. now is the box's clock.
//
// It returns ErrNoPersonToken when no header carries the scheme, a
// *TokenRefusal when the token is refused, and otherwise the key that
// signed and the token's times. Who that key belongs to, and what they
// may do, is the caller's to decide.
func CheckToken(req TokenRequest, identity string, now time.Time, lookup KeyLookup) (VerifiedToken, error) {
	// 1. The scheme. A request with no header in it is not a person's.
	var token string
	mine := false
	for _, v := range req.Authorization {
		scheme, rest, _ := strings.Cut(v, " ")
		if strings.EqualFold(scheme, tokenScheme) {
			mine, token = true, rest
		}
	}
	if !mine {
		return VerifiedToken{}, ErrNoPersonToken
	}
	// 2. One header, so a person's token and a deploy token can never
	// both be judged for one request.
	if len(req.Authorization) != 1 {
		return refuseToken("authorization-not-one-header")
	}
	// 3. The size, before any parse.
	if len(token) > maxTokenLen {
		return refuseToken("too-long")
	}
	// 4. The envelope: the prefix, exactly two parts, each strict
	// unpadded base64url.
	rest, ok := strings.CutPrefix(token, tokenEnvelope)
	if !ok {
		return refuseToken("envelope-prefix")
	}
	claims64, sig64, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(sig64, ".") {
		return refuseToken("envelope-parts")
	}
	claims, ok := tokenBase64(claims64)
	if !ok {
		return refuseToken("claims-encoding")
	}
	if len(claims) > maxClaimsLen {
		return refuseToken("claims-too-long")
	}
	blob, ok := tokenBase64(sig64)
	if !ok {
		return refuseToken("sig-encoding")
	}
	// 5. The SSHSIG structure: magic, then the fields with no byte
	// after them (ssh.Unmarshal refuses leftovers), then version 1.
	body, ok := bytes.CutPrefix(blob, []byte(sshsigMagic))
	if !ok {
		return refuseToken("blob-magic")
	}
	var sig tokenSig
	if err := ssh.Unmarshal(body, &sig); err != nil {
		return refuseToken("blob-structure")
	}
	if sig.Version != 1 {
		return refuseToken("blob-version")
	}
	// 6. The key: a type a person may hold, read from the key's first
	// string without parsing the rest; then its fingerprint in the
	// lookup. Fingerprints are not secret, so a stranger can name a
	// listed key and buy one signature check, and no more.
	var keyType struct {
		Type string
		Rest []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(sig.PublicKey, &keyType); err != nil || !tokenKeyTypes[keyType.Type] {
		return refuseToken("key-type")
	}
	sum := sha256.Sum256(sig.PublicKey)
	fingerprint := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	if lookup == nil {
		return refuseToken("key-not-listed")
	}
	key, ok := lookup(fingerprint)
	// The listed key, parsed at load, is the one the signature is
	// checked with; the token's copy is never parsed as a key. Byte
	// equality also means a certificate never stands for a plain key.
	if !ok || key == nil || !bytes.Equal(key.Marshal(), sig.PublicKey) {
		return refuseToken("key-not-listed")
	}
	// 7. The signature.
	if reason := tokenVerify(&sig, key, claims); reason != "" {
		return refuseToken(reason)
	}
	// 8. The claims, now signed, in their one spelling: the bytes must
	// equal the compact encoding of what they decode to. That refuses
	// another key order, white space, a key twice, a key in another
	// case, an escape and a missing key, with no canonicaliser to get
	// wrong. A key the struct lacks (a later form's scope or act among
	// them) is refused as unknown before that.
	dec := json.NewDecoder(bytes.NewReader(claims))
	dec.DisallowUnknownFields()
	var c tokenClaims
	if err := dec.Decode(&c); err != nil {
		return refuseToken("claims-json")
	}
	var again bytes.Buffer
	enc := json.NewEncoder(&again)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(c); err != nil || !bytes.Equal(bytes.TrimSuffix(again.Bytes(), []byte("\n")), claims) {
		return refuseToken("claims-not-canonical")
	}
	// 9. For this box. An empty identity, which no box has, matches
	// nothing.
	if identity == "" || c.Aud != identity {
		return refuseToken("aud")
	}
	// 10. Sent to this box's own name, not to another site the same
	// box serves.
	host := strings.ToLower(req.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != identity {
		return refuseToken("host")
	}
	// 11. The times, with the skew either way. Each comparison is
	// written so that no sum can overflow.
	t := now.Unix()
	if c.Iat <= 0 || c.Exp <= c.Iat {
		return refuseToken("times")
	}
	if c.Iat-tokenSkew > t {
		return refuseToken("iat-in-future")
	}
	if t-tokenSkew > c.Exp {
		return refuseToken("expired")
	}
	// 13. A person's token only reads, and a read is a GET. (12, a
	// token's scope, belongs to a later form.)
	if req.Method != "GET" {
		return refuseToken("method")
	}
	if c.Exp-c.Iat > maxTokenLife {
		return refuseToken("life")
	}
	return VerifiedToken{
		Fingerprint: fingerprint,
		IssuedAt:    time.Unix(c.Iat, 0).UTC(),
		ExpiresAt:   time.Unix(c.Exp, 0).UTC(),
	}, nil
}

// tokenBase64 decodes one part of the envelope. Go's decoder skips CR
// and LF even when strict, so any byte outside the base64url alphabet
// is refused first: one token, one spelling.
func tokenBase64(s string) ([]byte, bool) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := range len(s) {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return nil, false
		}
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, err == nil
}

// tokenVerify checks the signature over the claims with the listed
// key, and says why it is refused, or "" when it verifies. OpenSSH's
// own verifier leaves the sk- flags unchecked, and x/crypto would
// verify a signature with bytes after it, so both are checked here.
func tokenVerify(sig *tokenSig, key ssh.PublicKey, claims []byte) string {
	if sig.Namespace != tokenNamespace {
		return "signature-namespace"
	}
	if len(sig.Reserved) != 0 {
		return "signature-reserved"
	}
	var h hash.Hash
	switch sig.HashAlg {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return "signature-hash"
	}
	var s ssh.Signature
	if err := ssh.Unmarshal(sig.Signature, &s); err != nil {
		return "signature-form"
	}
	sk := key.Type() == ssh.KeyAlgoSKED25519 || key.Type() == ssh.KeyAlgoSKECDSA256
	switch {
	case !sk && len(s.Rest) != 0:
		return "signature-trailing"
	case sk && len(s.Rest) != 5: // the flags byte and a uint32 counter
		return "signature-sk-trailer"
	case sk && s.Rest[0]&skUserPresent == 0:
		return "signature-sk-touch"
	}
	// What the key signed: the magic, the namespace, the reserved
	// field, the hash's name and the claims' hash (PROTOCOL.sshsig).
	h.Write(claims)
	signed := append([]byte(sshsigMagic), ssh.Marshal(struct {
		Namespace string
		Reserved  []byte
		HashAlg   string
		Hash      []byte
	}{sig.Namespace, sig.Reserved, sig.HashAlg, h.Sum(nil)})...)
	if err := key.Verify(signed, &s); err != nil {
		return "signature"
	}
	return ""
}
