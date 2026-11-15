// Package sshkey is the person's side of a person's token: which ssh
// key motelctl signs with, and the token that key's signature makes.
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
// owner's decision): ed25519, ecdsa, and the hardware forms of both.
var personTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"sk-ssh-ed25519@openssh.com":         true,
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
// the type a box could take or ssh-rsa (refused later, by name), the
// base64 one canonical standard line, and the key's own first string
// equal to the declared type.
func ParsePublic(line string) (PublicKey, error) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return PublicKey{}, errors.New("not a public key line (<type> <base64> [comment])")
	}
	typ, b64 := f[0], f[1]
	if !personTypes[typ] && !isRSA(typ) {
		return PublicKey{}, fmt.Errorf("%q is not a key type a box takes (ssh-ed25519, ecdsa-sha2-nistp256/384/521, or the sk- form of either)", bound(typ))
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
	return e.Shown + " is an RSA key. A box takes ed25519 and ecdsa keys, including hardware ones, not RSA: make one with ssh-keygen -t ed25519."
}

// bound cuts what a refusal quotes of an argument to a printable size.
func bound(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
