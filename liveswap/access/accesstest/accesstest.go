// Package accesstest makes people's keys and signs their tokens, for
// the tests of the two webhooks and of access: a key made in the test,
// its person line as an operator writes one, and the Authorization
// header a person's client sends, signed as `ssh-keygen -Y sign` signs.
// For tests only, in the shape of deploytrust/trusttest. No key it
// makes is ever written to disk.
package accesstest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
)

// Key is a person's key, private half and all, made in the test.
type Key struct {
	signer ssh.Signer
}

// Ed25519 is a fresh ssh-ed25519 key.
func Ed25519() Key {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return fromSigner(priv)
}

// ECDSA is a fresh ecdsa-sha2-nistp256 key.
func ECDSA() Key {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return fromSigner(priv)
}

// P521 is a fresh ecdsa-sha2-nistp521 key.
func P521() Key {
	priv, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return fromSigner(priv)
}

func fromSigner(priv crypto.Signer) Key {
	s, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		panic(err)
	}
	return Key{signer: s}
}

// Public is the key's public half.
func (k Key) Public() ssh.PublicKey { return k.signer.PublicKey() }

// Type is the key's type, as a line writes it.
func (k Key) Type() string { return k.signer.PublicKey().Type() }

// B64 is the key's base64, as a line writes it.
func (k Key) B64() string {
	return base64.StdEncoding.EncodeToString(k.signer.PublicKey().Marshal())
}

// Fingerprint is SHA256:<base64>, as ssh-keygen -l prints it.
func (k Key) Fingerprint() string { return ssh.FingerprintSHA256(k.signer.PublicKey()) }

// Line is a person line for name with this key.
func (k Key) Line(name string) string {
	return fmt.Sprintf("person %s %s %s", name, k.Type(), k.B64())
}

// Token is the Authorization header value a person's client sends to
// the box aud: claims issued at iat, good for life.
func (k Key) Token(aud string, iat time.Time, life time.Duration) string {
	claims := fmt.Sprintf(`{"aud":"%s","iat":%d,"exp":%d}`, aud, iat.Unix(), iat.Add(life).Unix())
	return k.Sign(claims)
}

// Sign is the Authorization header value carrying claims, whatever
// they are, signed by the key.
func (k Key) Sign(claims string) string {
	digest := sha512.Sum512([]byte(claims))
	signed := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace string
		Reserved  []byte
		HashAlg   string
		Hash      []byte
	}{access.TokenNamespace, nil, "sha512", digest[:]})...)
	sig, err := k.signer.Sign(rand.Reader, signed)
	if err != nil {
		panic(err)
	}
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version   uint32
		PublicKey []byte
		Namespace string
		Reserved  []byte
		HashAlg   string
		Signature []byte
	}{1, k.signer.PublicKey().Marshal(), access.TokenNamespace, nil, "sha512", ssh.Marshal(sig)})...)
	enc := base64.RawURLEncoding
	return access.TokenScheme + " " + access.TokenEnvelope + enc.EncodeToString([]byte(claims)) + "." + enc.EncodeToString(blob)
}
