// Package sshkey is the person's side of a person's token: which ssh
// key the client signs with, and the token that key's signature makes.
// It holds no crypto of its own. The key is read as its public line,
// and the signature is OpenSSH's, made by the system ssh-keygen, so
// the agent, a hardware key and the person's own ssh setup all work as
// they do for git's ssh signing.
package sshkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// personTypes are the key types a box takes in a person's token (the
// owner's decision): ed25519 and its hardware form, for now.
var personTypes = map[string]bool{
	"ssh-ed25519":                true,
	"sk-ssh-ed25519@openssh.com": true,
}

// ecdsaTypes are the ECDSA types, which a box does not take from a
// person yet (deferred by the owner on 2026-11-15; a later release may
// take some of them). A line of one parses, to be refused by name.
var ecdsaTypes = map[string]bool{
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// PublicKey is a public key as its .pub line has it.
type PublicKey struct {
	// Type is the key's type, as the line and the key itself name it.
	Type string
	// Blob is the key in its wire form: what the fingerprint hashes.
	Blob []byte
}

// ParsePublic reads a public key line, `<type> <base64> [comment]`:
// the type a box could take, ssh-rsa or an ECDSA type (both refused
// later, by name: Refusal), the
// base64 one canonical standard line, and the key's own first string
// equal to the declared type.
func ParsePublic(line string) (PublicKey, error) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return PublicKey{}, errors.New("not a public key line (<type> <base64> [comment])")
	}
	typ, b64 := f[0], f[1]
	if !personTypes[typ] && !isRSA(typ) && !ecdsaTypes[typ] {
		return PublicKey{}, fmt.Errorf("%q is not a key type a box takes (ssh-ed25519, or sk-ssh-ed25519@openssh.com for a hardware key)", bound(typ))
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || base64.StdEncoding.EncodeToString(blob) != b64 {
		return PublicKey{}, errors.New("the key is not one line of standard base64")
	}
	if len(blob) < 4 {
		return PublicKey{}, errors.New("the key is cut short")
	}
	// The key's first string is its type: four bytes of length, then
	// the name. A type name is a few dozen bytes, so its length fits.
	want := binary.BigEndian.AppendUint32(nil, uint32(len(typ))) //nolint:gosec // G115: len(typ) is a key type's name, a few dozen bytes
	if !bytes.HasPrefix(blob, append(want, typ...)) {
		return PublicKey{}, fmt.Errorf("the key inside the line is not a %s key", typ)
	}
	return PublicKey{Type: typ, Blob: blob}, nil
}

// Fingerprint is the key's SHA-256 fingerprint as `ssh-keygen -l`
// prints it, and as a box finds the key by.
func (k PublicKey) Fingerprint() string {
	sum := sha256.Sum256(k.Blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Line is the key as a person line carries it: type and base64.
func (k PublicKey) Line() string {
	return k.Type + " " + base64.StdEncoding.EncodeToString(k.Blob)
}

// RSA says whether the key is an RSA key, which no box takes from a
// person.
func (k PublicKey) RSA() bool { return isRSA(k.Type) }

// ECDSA says whether the key is an ECDSA key, which a box does not take
// from a person yet.
func (k PublicKey) ECDSA() bool { return ecdsaTypes[k.Type] }

func isRSA(typ string) bool {
	return typ == "ssh-rsa" || typ == "ssh-rsa-cert-v01@openssh.com"
}

// RSAError is an RSA key named for a person's token. It is refused
// before anything is signed or sent: the box would refuse the token
// with the same 401 as a key it does not list, and the person would
// not learn why.
type RSAError struct {
	// Shown is the key as the person named it.
	Shown string
}

func (e *RSAError) Error() string {
	return e.Shown + " is an RSA key. A box takes ed25519 keys, including hardware ones, not RSA: make one with ssh-keygen -t ed25519."
}

// ECDSAError is an ECDSA key named for a person's token, refused as an
// RSA key is, before anything is signed or sent: a box takes Ed25519
// keys for now and would answer the token with the same 401 as a key
// it does not list. It names the key, how it was chosen and its
// fingerprint, since a key found by a default (the default file, the
// agent's only key) is one the person did not name.
type ECDSAError struct {
	// Shown is the key as the person named it, or as it was found.
	Shown string
	// Source is how it was chosen; empty when not known.
	Source      string
	Type        string
	Fingerprint string
}

func (e *ECDSAError) Error() string {
	which := e.Fingerprint
	if e.Source != "" {
		which = e.Source + ", " + which
	}
	return e.Shown + " (" + which + ") is an " + e.Type + " key. A box takes Ed25519 keys for now, not ECDSA yet: make one with ssh-keygen -t ed25519 (or -t ed25519-sk for a hardware key) and name it with --key or MOTEL_KEY."
}

// Refusal is why no token may be signed with key, nil when one may:
// an RSA key (*RSAError) or an ECDSA one (*ECDSAError).
func Refusal(key Key) error {
	switch {
	case key.Public.RSA():
		return &RSAError{Shown: key.Shown}
	case key.Public.ECDSA():
		return &ECDSAError{Shown: key.Shown, Source: key.Source, Type: key.Public.Type, Fingerprint: key.Public.Fingerprint()}
	}
	return nil
}

// bound cuts what a refusal quotes of an argument to a printable size.
func bound(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
