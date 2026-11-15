package sshkey

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/smallhoursorg/motel/motelctl/internal/local"
	"github.com/smallhoursorg/motel/motelctl/internal/termtext"
)

// The token's words. Each is in one place: the scheme and the envelope
// are proposals the owner has not confirmed, and the namespace is the
// owner's, the same constant the box's checker holds
// (liveswap/access/token.go).
const (
	// Scheme is the Authorization scheme of a person's token.
	Scheme = "Motel"
	// Envelope begins every token: version 1 of the envelope.
	Envelope = "m1."
	// Namespace is the SSHSIG namespace a token is signed in, which
	// keeps it apart from every other signature the key makes.
	Namespace = "motel-access-v1@motel.smallhours.org"
)

// The token's times. A box takes a token from 60 s before its iat to
// 60 s after its exp, and one whose life is at most 600 s. iat is set
// back a little so a clock a few seconds fast is no refusal, and the
// life is half what a box allows: a command uses its token at once,
// and a token captured on the way lasts as long as its life.
const (
	backdate = 30 * time.Second
	life     = 300 * time.Second
)

// Signer makes the SSHSIG signature of a message, armored as
// `ssh-keygen -Y sign` writes it. Keygen is the real one; a test gives
// its own, so only one test needs ssh-keygen.
type Signer interface {
	Sign(ctx context.Context, key Key, namespace string, message []byte) ([]byte, error)
}

// Keygen signs by running the system ssh-keygen.
type Keygen struct {
	Run local.Runner
	// Stderr is where ssh-keygen's prompts go: a passphrase prompt
	// is on the terminal, but a hardware key's "confirm user presence"
	// is on stderr.
	Stderr io.Writer
}

// Sign runs `ssh-keygen -Y sign -n <namespace> -f <key>` with the
// message on stdin, and returns what it writes on stdout. A key the
// agent alone holds is given to it as /dev/fd/3.
func (k Keygen) Sign(ctx context.Context, key Key, namespace string, message []byte) ([]byte, error) {
	cmd := local.Cmd{Name: "ssh-keygen", Args: []string{"-Y", "sign", "-n", namespace, "-f", key.Path}, Stdin: message}
	if key.Path == "" {
		cmd.Args[len(cmd.Args)-1] = "/dev/fd/3"
		cmd.FD3 = []byte(key.Public.Line() + "\n")
	}
	var said bytes.Buffer
	if k.Stderr != nil {
		cmd.Stderr = io.MultiWriter(k.Stderr, &said)
	} else {
		cmd.Stderr = &said
	}
	out, err := k.Run.Output(ctx, cmd)
	switch {
	case local.NotInstalled(err):
		return nil, errors.New("ssh-keygen is not installed: motelctl signs with OpenSSH's ssh-keygen (8.2 or later)")
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		last := strings.TrimSpace(said.String())
		if i := strings.LastIndexByte(last, '\n'); i >= 0 {
			last = last[i+1:]
		}
		if last == "" {
			last = err.Error()
		}
		return nil, fmt.Errorf("ssh-keygen could not sign with %s: %s", key.Shown, termtext.From(last))
	}
	return out, nil
}

// Token makes a person's token for the box aud, at now, and returns
// the whole Authorization header value:
//
//	Motel m1.<base64url claims>.<base64url SSHSIG>
//
// The claims are exactly {"aud":…,"iat":…,"exp":…}, in that order and
// that one spelling, which is the only one a box takes. aud must be
// the box's name as its identity has it: a lowercase hostname, which
// needs no escape in JSON.
func Token(ctx context.Context, s Signer, key Key, aud string, now time.Time) (string, error) {
	if key.Public.RSA() {
		return "", &RSAError{Shown: key.Shown}
	}
	iat := now.Add(-backdate).Unix()
	claims, err := encodeClaims(aud, iat, iat+int64(life/time.Second))
	if err != nil {
		return "", err
	}
	armored, err := s.Sign(ctx, key, Namespace, claims)
	if err != nil {
		return "", err
	}
	sig, err := Dearmor(armored)
	if err != nil {
		return "", fmt.Errorf("ssh-keygen's signature: %w", err)
	}
	return Scheme + " " + Envelope + base64.RawURLEncoding.EncodeToString(claims) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// encodeClaims is the claims document, encoded as the box re-encodes
// it to check its spelling: a struct in field order, compact, with no
// HTML escaping.
func encodeClaims(aud string, iat, exp int64) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Aud string `json:"aud"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{aud, iat, exp}); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

const (
	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
)

// Dearmor takes the one signature block ssh-keygen writes, and nothing
// around it, and returns the SSHSIG structure it armors. The box never
// parses armor: what travels is the structure.
func Dearmor(armored []byte) ([]byte, error) {
	text := strings.TrimSuffix(string(armored), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || lines[0] != armorBegin || lines[len(lines)-1] != armorEnd {
		return nil, errors.New("not exactly one SSH SIGNATURE block")
	}
	body := lines[1 : len(lines)-1]
	for _, l := range body {
		if l == "" || strings.HasPrefix(l, "-----") {
			return nil, errors.New("not exactly one SSH SIGNATURE block")
		}
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(strings.Join(body, ""))
	if err != nil {
		return nil, errors.New("the signature block is not base64")
	}
	if !bytes.HasPrefix(sig, []byte("SSHSIG")) {
		return nil, errors.New("the signature block is not an SSHSIG structure")
	}
	return sig, nil
}
