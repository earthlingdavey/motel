package sshkey

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
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
	// BoxSkew is how far a box lets its clock and a token's times
	// disagree, either way (liveswap/access's tokenSkew).
	BoxSkew = 60 * time.Second
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
	// Terminal says whether a passphrase can be asked on a terminal;
	// nil opens /dev/tty to see.
	Terminal func() bool
}

// Sign runs `ssh-keygen -q -Y sign -n <namespace> -f <key>` with the
// message on stdin, and returns what it writes on stdout. A key the
// agent alone holds is given to it as /dev/fd/3. -q drops the
// "Signing data on standard input" every command would otherwise
// print; a hardware key's prompt is printed whatever -q says, and a
// passphrase is asked on the terminal.
func (k Keygen) Sign(ctx context.Context, key Key, namespace string, message []byte) ([]byte, error) {
	cmd := local.Cmd{Name: "ssh-keygen", Args: []string{"-q", "-Y", "sign", "-n", namespace, "-f", key.Path}, Stdin: message}
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
		// With no terminal, ssh-keygen asks a key file's passphrase on
		// stdin, reads the token's claims as it, and fails. Told by
		// three things together: it failed signing from a private key
		// file (Path is the .pub when the agent holds the key, and an
		// agent's failure is another matter: a confirmation refused, a
		// touch timed out), that file's header says it is encrypted,
		// and no terminal opens.
		if key.Path != "" && !strings.HasSuffix(key.Path, ".pub") && encryptedKeyFile(key.Path) && !k.terminal() {
			// ssh-add takes the private key: Shown may name its .pub.
			shown := termtext.From(strings.TrimSuffix(key.Shown, ".pub"))
			return nil, fmt.Errorf("%s is protected by a passphrase, and with no terminal to ask it on, ssh-keygen cannot read it: add the key to the agent (ssh-add %s) and run motelctl again", shown, shown)
		}
		return nil, fmt.Errorf("ssh-keygen could not sign with %s: %s", key.Shown, termtext.From(last))
	}
	return out, nil
}

func (k Keygen) terminal() bool {
	if k.Terminal != nil {
		return k.Terminal()
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// maxKeyFile bounds what is read of a private key file: a key's is a
// few KiB at most.
const maxKeyFile = 64 << 10

// encryptedKeyFile says whether the private key file at path says it
// is encrypted: an OpenSSH key whose cipher is not "none", or a PEM
// key marked ENCRYPTED. Only a regular file is read, opened without
// blocking, so a FIFO at the path never holds the command; anything it
// cannot read is not said to be.
func encryptedKeyFile(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the key the person named, which ssh-keygen was just given
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return false
	}
	block, _ := pem.Decode(b)
	switch {
	case block == nil:
		return false
	case block.Type == "OPENSSH PRIVATE KEY":
		const magic = "openssh-key-v1\x00"
		d, ok := bytes.CutPrefix(block.Bytes, []byte(magic))
		if !ok || len(d) < 4 {
			return false
		}
		n := binary.BigEndian.Uint32(d)
		d = d[4:]
		return uint64(n) <= uint64(len(d)) && string(d[:n]) != "none"
	case block.Type == "ENCRYPTED PRIVATE KEY":
		return true
	}
	return strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED")
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
	iat, exp := Times(now)
	claims, err := encodeClaims(aud, iat.Unix(), exp.Unix())
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

// Times are the iat and exp of a token made at now, to the second.
func Times(now time.Time) (iat, exp time.Time) {
	iat = time.Unix(now.Add(-backdate).Unix(), 0)
	return iat, iat.Add(life)
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
