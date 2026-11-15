package sshkey

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/smallhoursorg/motel/liveswap/access"
	"github.com/smallhoursorg/motel/motelctl/internal/local"
)

const testBox = "deploy.example.test"

// newSigner is a key made in the test, in memory.
func newSigner(t *testing.T, kind string) ssh.Signer {
	t.Helper()
	var priv any
	var err error
	switch kind {
	case "ed25519":
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	case "ecdsa":
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func lineOf(s ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

// rsaLine is an ssh-rsa public key line: only its type is read.
func rsaLine() string {
	blob := ssh.Marshal(struct {
		Type string
		E, N []byte
	}{"ssh-rsa", []byte{1, 0, 1}, bytes.Repeat([]byte{0xab}, 64)})
	return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob)
}

// memSigner signs as ssh-keygen -Y sign does, with a key in memory: the
// SSHSIG structure of OpenSSH's PROTOCOL.sshsig, armored.
type memSigner struct {
	s     ssh.Signer
	calls int
}

func (m *memSigner) Sign(_ context.Context, _ Key, namespace string, message []byte) ([]byte, error) {
	m.calls++
	h := sha512.Sum512(message)
	signed := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace string
		Reserved  []byte
		HashAlg   string
		Hash      []byte
	}{namespace, nil, "sha512", h[:]})...)
	sig, err := m.s.Sign(rand.Reader, signed)
	if err != nil {
		return nil, err
	}
	blob := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version   uint32
		PublicKey []byte
		Namespace string
		Reserved  []byte
		HashAlg   string
		Signature []byte
	}{1, m.s.PublicKey().Marshal(), namespace, nil, "sha512", ssh.Marshal(sig)})...)
	return armor(blob), nil
}

func armor(blob []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(blob)
	var out bytes.Buffer
	out.WriteString(armorBegin + "\n")
	for len(b64) > 70 {
		out.WriteString(b64[:70] + "\n")
		b64 = b64[70:]
	}
	out.WriteString(b64 + "\n" + armorEnd + "\n")
	return out.Bytes()
}

// check puts a header through the box's own checker, as a GET to the
// box's name at now, with pub the one listed key.
func check(t *testing.T, header string, pub ssh.PublicKey, now time.Time) (access.VerifiedToken, error) {
	t.Helper()
	lookup := func(fp string) (ssh.PublicKey, bool) {
		if fp == ssh.FingerprintSHA256(pub) {
			return pub, true
		}
		return nil, false
	}
	listed := func(keyType string) bool { return keyType == pub.Type() }
	return access.CheckToken(access.TokenRequest{Authorization: []string{header}, Method: "GET", Host: testBox}, testBox, now, lookup, listed)
}

func keyFor(t *testing.T, s ssh.Signer) Key {
	t.Helper()
	pk, err := ParsePublic(lineOf(s))
	if err != nil {
		t.Fatal(err)
	}
	return Key{Public: pk, Path: "/nowhere", Shown: "~/.ssh/test.pub", Source: SourceFlag}
}

// The token this code makes is one the box's checker accepts: every
// key type a person may hold that x/crypto makes here.
func TestTokenIsAcceptedByTheBox(t *testing.T) {
	now := time.Unix(1794657600, 0)
	for _, kind := range []string{"ed25519", "ecdsa"} {
		t.Run(kind, func(t *testing.T) {
			s := newSigner(t, kind)
			header, err := Token(context.Background(), &memSigner{s: s}, keyFor(t, s), testBox, now)
			if err != nil {
				t.Fatal(err)
			}
			v, err := check(t, header, s.PublicKey(), now)
			if err != nil {
				t.Fatalf("the box refused the token: %v\n%s", err, header)
			}
			if v.Fingerprint != keyFor(t, s).Public.Fingerprint() {
				t.Errorf("fingerprint %s, want the key's %s", v.Fingerprint, keyFor(t, s).Public.Fingerprint())
			}
			if life := v.ExpiresAt.Sub(v.IssuedAt); life <= 0 || life > 600*time.Second {
				t.Errorf("life %s, want at most 600 s", life)
			}
			// Good for its whole life on the box's clock, and not past it.
			if _, err := check(t, header, s.PublicKey(), v.ExpiresAt.Add(59*time.Second)); err != nil {
				t.Errorf("refused within the skew after exp: %v", err)
			}
			if _, err := check(t, header, s.PublicKey(), v.ExpiresAt.Add(61*time.Second)); err == nil {
				t.Error("accepted after exp and the skew")
			}
		})
	}
}

// The claims are the one spelling the box takes, in order, and
// nothing else.
func TestTokenClaims(t *testing.T) {
	s := newSigner(t, "ed25519")
	now := time.Unix(1794657600, 0)
	header, err := Token(context.Background(), &memSigner{s: s}, keyFor(t, s), testBox, now)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(header, "Motel m1.")
	if !ok {
		t.Fatalf("header %q does not start Motel m1.", header)
	}
	claims64, sig64, _ := strings.Cut(rest, ".")
	claims, err := base64.RawURLEncoding.Strict().DecodeString(claims64)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"aud":"deploy.example.test","iat":1794657570,"exp":1794657870}`; string(claims) != want {
		t.Errorf("claims %s, want %s", claims, want)
	}
	if strings.ContainsAny(header, "=+/") || strings.Count(sig64, ".") != 0 {
		t.Errorf("not unpadded base64url in three parts: %s", header)
	}
}

// An RSA key is refused before anything is signed.
func TestTokenRefusesRSA(t *testing.T) {
	pk, err := ParsePublic(rsaLine())
	if err != nil {
		t.Fatal(err)
	}
	m := &memSigner{s: newSigner(t, "ed25519")}
	_, err = Token(context.Background(), m, Key{Public: pk, Shown: "~/.ssh/id_rsa.pub"}, testBox, time.Now())
	var rsaErr *RSAError
	if !errors.As(err, &rsaErr) || m.calls != 0 {
		t.Fatalf("err %v after %d signatures, want an RSAError before any", err, m.calls)
	}
	if want := "~/.ssh/id_rsa.pub is an RSA key. A box takes ed25519 and ecdsa keys, including hardware ones, not RSA: make one with ssh-keygen -t ed25519."; err.Error() != want {
		t.Errorf("message\n%s\nwant\n%s", err, want)
	}
}

func TestDearmor(t *testing.T) {
	blob := append([]byte("SSHSIG"), bytes.Repeat([]byte{7}, 200)...)
	good := armor(blob)
	if got, err := Dearmor(good); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("Dearmor = %x, %v", got, err)
	}
	for name, in := range map[string][]byte{
		"empty":                nil,
		"text before":          append([]byte("note\n"), good...),
		"text after":           append(bytes.Clone(good), []byte("note\n")...),
		"two blocks":           append(bytes.Clone(good), good...),
		"not base64":           []byte(armorBegin + "\n!!!!\n" + armorEnd + "\n"),
		"not SSHSIG":           armor([]byte("NOTSIG and more")),
		"no end":               []byte(armorBegin + "\nU1NIU0lH\n"),
		"blank line":           []byte(armorBegin + "\n\nU1NIU0lH\n" + armorEnd + "\n"),
		"other armor type":     []byte("-----BEGIN PGP SIGNATURE-----\nU1NIU0lH\n-----END PGP SIGNATURE-----\n"),
		"the BEGIN line wrong": []byte("-----BEGIN PGP SIGNATURE-----\nU1NIU0lH\n" + armorEnd + "\n"),
		"the END line wrong":   []byte(armorBegin + "\nU1NIU0lH\n-----END PGP SIGNATURE-----\n"),
		"a BEGIN line padded":  []byte(" " + armorBegin + "\nU1NIU0lH\n" + armorEnd + "\n"),
	} {
		if _, err := Dearmor(in); err == nil {
			t.Errorf("%s: Dearmor took %q", name, in)
		}
	}
}

func TestParsePublic(t *testing.T) {
	ed := lineOf(newSigner(t, "ed25519"))
	k, err := ParsePublic(ed + " alice@laptop")
	if err != nil || k.Type != "ssh-ed25519" || k.Line() != ed {
		t.Fatalf("ParsePublic(%q) = %+v, %v", ed, k, err)
	}
	typ, b64, _ := strings.Cut(ed, " ")
	for name, line := range map[string]string{
		"one field":      typ,
		"dss":            "ssh-dss " + b64,
		"type mismatch":  "ecdsa-sha2-nistp256 " + b64,
		"padding added":  typ + " " + b64 + "==",
		"not base64":     typ + " !!!!",
		"cut short":      typ + " AAAA",
		"certificate":    "ssh-ed25519-cert-v01@openssh.com " + b64,
		"private header": "-----BEGIN OPENSSH PRIVATE KEY-----", // gitleaks:allow — made up for this test
	} {
		if _, err := ParsePublic(line); err == nil {
			t.Errorf("%s: ParsePublic took %q", name, line)
		}
	}
	if k, err := ParsePublic(rsaLine()); err != nil || !k.RSA() {
		t.Errorf("an ssh-rsa line parses, to be refused by name: %+v, %v", k, err)
	}
}

// fakeRun answers git and ssh-add as a test sets them.
type fakeRun struct {
	top       string
	config    map[string]string
	agent     string
	agentErr  error
	agentAsks int
}

func (f *fakeRun) Output(_ context.Context, c local.Cmd) ([]byte, error) {
	switch {
	case c.Name == "git" && c.Args[0] == "rev-parse":
		if f.top == "" {
			return nil, errors.New("not a git repository")
		}
		return []byte(f.top + "\n"), nil
	case c.Name == "git" && c.Args[0] == "config":
		if v, ok := f.config[c.Args[2]]; ok {
			return []byte(v + "\n"), nil
		}
		return nil, errors.New("exit status 1")
	case c.Name == "ssh-add":
		f.agentAsks++
		return []byte(f.agent), f.agentErr
	}
	return nil, exec.ErrNotFound
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// One key, in the order the spec sets, never several.
func TestResolveOrder(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	write(t, filepath.Join(repo, testBox, "Caddyfile"), "")
	keys := map[string]string{}
	for _, n := range []string{"flag", "env", "git", "agent"} {
		keys[n] = lineOf(newSigner(t, "ed25519"))
		write(t, filepath.Join(home, n+".pub"), keys[n]+" "+n+"\n")
	}
	keys["default"] = lineOf(newSigner(t, "ecdsa"))
	write(t, filepath.Join(home, ".ssh", "id_ecdsa.pub"), keys["default"]+"\n")
	write(t, filepath.Join(home, ".ssh", "id_rsa.pub"), rsaLine()+"\n")

	type setup struct {
		flag, env string
		git       bool
		dflt      bool
		agent     string
	}
	cases := []struct {
		name       string
		s          setup
		want       string // the key, by name
		wantSource string
		wantPath   string
	}{
		{"flag first", setup{flag: "~/flag.pub", env: "~/env.pub", git: true, dflt: true, agent: keys["agent"]}, "flag", SourceFlag, filepath.Join(home, "flag.pub")},
		{"then MOTEL_KEY", setup{env: filepath.Join(home, "env.pub"), git: true, dflt: true, agent: keys["agent"]}, "env", SourceEnv, filepath.Join(home, "env.pub")},
		{"then git in a box repository", setup{git: true, dflt: true, agent: keys["agent"]}, "git", SourceGit, filepath.Join(home, "git.pub")},
		{"then the default file", setup{dflt: true, agent: keys["agent"]}, "default", SourceDefault, filepath.Join(home, ".ssh", "id_ecdsa")},
		{"then the agent's only key", setup{agent: keys["agent"] + " agent\n"}, "agent", SourceAgent, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := home
			if !c.s.dflt {
				h = t.TempDir() // no ~/.ssh
			}
			run := &fakeRun{agent: c.s.agent, config: map[string]string{}}
			if c.s.git {
				run.top = repo
				run.config["gpg.format"] = "ssh"
				run.config["user.signingkey"] = filepath.Join(home, "git.pub")
			}
			env := map[string]string{"MOTEL_KEY": c.s.env}
			k, err := Resolve(context.Background(), Lookup{Flag: c.s.flag, Getenv: func(k string) string { return env[k] }, Home: h, Dir: filepath.Join(repo, testBox), Run: run})
			if err != nil {
				t.Fatal(err)
			}
			if k.Public.Line() != keys[c.want] || k.Source != c.wantSource || k.Path != c.wantPath {
				t.Errorf("got %s from %q at %q, want the %s key from %q at %q", k.Public.Line(), k.Source, k.Path, c.want, c.wantSource, c.wantPath)
			}
			if c.want != "agent" && run.agentAsks != 0 {
				t.Error("the agent was asked although a key was named")
			}
		})
	}
}

func TestResolveRefusals(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "rsa.pub"), rsaLine()+"\n")
	write(t, filepath.Join(home, "lonely"), "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n") // gitleaks:allow — made up for this test
	two := lineOf(newSigner(t, "ed25519")) + "\n" + lineOf(newSigner(t, "ed25519")) + "\n"
	none := func(string) string { return "" }
	cases := []struct {
		name string
		l    Lookup
		want string
	}{
		{"RSA by flag", Lookup{Flag: "~/rsa.pub"}, "~/rsa.pub is an RSA key."},
		{"RSA in the agent", Lookup{Run: &fakeRun{agent: rsaLine() + " me\n"}}, "the agent's key is an RSA key."},
		{"git outside a box repository is not read", Lookup{Run: &fakeRun{config: map[string]string{"gpg.format": "ssh", "user.signingkey": "~/rsa.pub"}}}, ErrNoKey.Error()},
		{"a private key with no .pub", Lookup{Flag: "~/lonely"}, "~/lonely (--key): no public key beside it: name its .pub"},
		{"a file that is not there", Lookup{Flag: "~/missing.pub"}, "~/missing.pub (--key): open "},
		{"two keys in the agent", Lookup{Run: &fakeRun{agent: two}}, ErrNoKey.Error()},
		{"no agent", Lookup{Run: &fakeRun{agentErr: errors.New("exit status 2")}}, ErrNoKey.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.l.Run == nil {
				c.l.Run = &fakeRun{}
			}
			c.l.Getenv, c.l.Home, c.l.Dir = none, home, home
			_, err := Resolve(context.Background(), c.l)
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Fatalf("err %v, want it to start %q", err, c.want)
			}
		})
	}
}

// keygen is the system ssh-keygen, or a skip that says loudly what is
// not tested here. The dev container (golang:1.27-trixie) has it, and
// CI runs the tests in that container.
func keygen(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: ssh-keygen is not installed, so no token signed by OpenSSH was checked against the box's checker. Run this test where ssh-keygen is (the dev container has it).")
	}
	return path
}

// makeKey makes a throwaway key with ssh-keygen, in the test's own
// directory, never committed.
func makeKey(t *testing.T, bin, typ string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_"+typ)
	if out, err := exec.Command(bin, "-q", "-t", typ, "-N", "", "-C", "motelctl-test", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -t %s: %v\n%s", typ, err, out)
	}
	return path
}

// A token signed by the real ssh-keygen, through Keygen, is one the
// box's checker accepts: the format is OpenSSH's, not only this code's.
func TestKeygenTokenIsAcceptedByTheBox(t *testing.T) {
	bin := keygen(t)
	for _, typ := range []string{"ed25519", "ecdsa"} {
		t.Run(typ, func(t *testing.T) {
			path := makeKey(t, bin, typ)
			k, err := Lookup{Home: t.TempDir()}.load(path, SourceFlag)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			header, err := Token(context.Background(), Keygen{Run: local.Exec{}}, k, testBox, now)
			if err != nil {
				t.Fatal(err)
			}
			pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Public.Line()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := check(t, header, pub, now); err != nil {
				t.Fatalf("the box refused ssh-keygen's token: %v\n%s", err, header)
			}
		})
	}
}

// A key the agent alone holds (no file at all) is handed to ssh-keygen
// on a pipe, and the token it signs is accepted.
func TestKeygenAgentOnlyKey(t *testing.T) {
	bin := keygen(t)
	agent, err := exec.LookPath("ssh-agent")
	if err != nil {
		t.Skip("SKIPPED — NOT TESTED HERE: ssh-agent is not installed, so a key the agent alone holds was not signed with.")
	}
	path := makeKey(t, bin, "ed25519")
	sock := filepath.Join(t.TempDir(), "agent.sock")
	cmd := exec.Command(agent, "-D", "-a", sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; ; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if i == 100 {
			t.Fatal("ssh-agent made no socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
	if out, err := exec.Command("ssh-add", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, out)
	}
	pubLine, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + ".pub"} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	k, err := Resolve(context.Background(), Lookup{Getenv: func(string) string { return "" }, Home: t.TempDir(), Dir: t.TempDir(), Run: local.Exec{}})
	if err != nil {
		t.Fatal(err)
	}
	if k.Source != SourceAgent || k.Path != "" {
		t.Fatalf("resolved %+v, want the agent's only key", k)
	}
	now := time.Now()
	header, err := Token(context.Background(), Keygen{Run: local.Exec{}}, k, testBox, now)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pubLine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := check(t, header, pub, now); err != nil {
		t.Fatalf("the box refused the agent key's token: %v", err)
	}
}

// A key file protected by a passphrase, with no terminal to ask it on:
// ssh-keygen reads the token's claims as the passphrase and fails, and
// motelctl says to add the key to the agent. With a terminal, or with
// a key file that is not encrypted, ssh-keygen's own words stand.
func TestKeygenPassphraseWithNoTerminal(t *testing.T) {
	bin := keygen(t)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if out, err := exec.Command(bin, "-q", "-t", "ed25519", "-N", "a passphrase", "-C", "motelctl-test", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	k, err := Lookup{Home: t.TempDir()}.load(path, SourceFlag)
	if err != nil {
		t.Fatal(err)
	}
	if !encryptedKeyFile(path) || encryptedKeyFile(makeKey(t, bin, "ed25519")) || encryptedKeyFile(path+".pub") {
		t.Fatal("encryptedKeyFile reads a key file's header wrongly")
	}
	for _, c := range []struct {
		name     string
		terminal bool
		want     string
	}{
		{"no terminal", false, "is protected by a passphrase, and with no terminal to ask it on, ssh-keygen cannot read it: add the key to the agent (ssh-add " + strings.TrimSuffix(k.Shown, ".pub") + ") and run motelctl again"},
		{"a terminal", true, "ssh-keygen could not sign with " + k.Shown + ": "},
	} {
		_, err := Token(context.Background(), Keygen{Run: local.Exec{}, Terminal: func() bool { return c.terminal }}, k, testBox, time.Now())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// encryptedKeyFile reads the older PEM forms too, and nothing that is
// no key file.
func TestEncryptedKeyFileForms(t *testing.T) {
	dir := t.TempDir()
	begin := "-----BEGIN " // split, so no line here is a key's header
	for _, c := range []struct {
		name, content string
		encrypted     bool
	}{
		{"PEM, encrypted", begin + "EC PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\nAAAA\n-----END EC PRIVATE KEY-----\n", true},
		{"PEM, plain", begin + "EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n", false},
		{"PKCS #8, encrypted", begin + "ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n", true},
		{"OpenSSH, truncated", begin + "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----\n", false},
		{"not a key", "hello\n", false},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_"))
		write(t, p, c.content)
		if got := encryptedKeyFile(p); got != c.encrypted {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if encryptedKeyFile(filepath.Join(dir, "none")) {
		t.Error("a file that is not there")
	}
}

// encryptedKeyFile reads at most 64 KiB (written here as a number, so
// a change of the bound fails this test): a file whose header ends past
// it is not read whole, and is not said to be encrypted. A FIFO at the
// path is not waited on.
func TestEncryptedKeyFileBounds(t *testing.T) {
	dir := t.TempDir()
	block := func(pad int) []byte {
		b := append([]byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr"), make([]byte, pad)...)
		return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
	}
	small, big := filepath.Join(dir, "small"), filepath.Join(dir, "big")
	write(t, small, string(block(40000))) // about 53 KiB encoded
	write(t, big, string(block(65536)))   // about 87 KiB encoded
	if st, _ := os.Stat(small); st.Size() >= 65536 {
		t.Fatalf("the small file is %d bytes", st.Size())
	}
	if !encryptedKeyFile(small) || encryptedKeyFile(big) {
		t.Errorf("small %v, past the bound %v: want true, false", encryptedKeyFile(small), encryptedKeyFile(big))
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- encryptedKeyFile(fifo) }()
	select {
	case got := <-done:
		if got {
			t.Error("a FIFO is said to be an encrypted key")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO at the key's path blocked")
	}
}

// The passphrase advice is for a private key file alone: a key whose
// .pub is the path (the agent holds the private half) that fails to
// sign, with an encrypted file beside it and no terminal, gets
// ssh-keygen's own words. And with no Terminal set, the check is
// whether /dev/tty opens.
func TestPassphraseAdviceOnlyForAKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	write(t, path, string(pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: []byte("openssh-key-v1\x00\x00\x00\x00\x0aaes256-ctr")})))
	pk, err := ParsePublic(lineOf(newSigner(t, "ed25519")))
	if err != nil {
		t.Fatal(err)
	}
	failing := failRun{}
	for _, c := range []struct {
		name, path string
		advice     bool
	}{
		{"the private key file", path, true},
		{"its .pub, the agent holding the key", path + ".pub", false},
	} {
		k := Key{Public: pk, Path: c.path, Shown: c.path, Source: SourceFlag}
		_, err := Keygen{Run: failing, Terminal: func() bool { return false }}.Sign(context.Background(), k, Namespace, []byte("{}"))
		if err == nil || strings.Contains(err.Error(), "ssh-add") != c.advice {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	_, ttyErr := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if got := (Keygen{}).terminal(); got != (ttyErr == nil) {
		t.Errorf("terminal() %v, /dev/tty opens: %v", got, ttyErr == nil)
	}
}

// failRun is ssh-keygen failing as it does on a wrong passphrase.
type failRun struct{}

func (failRun) Output(_ context.Context, c local.Cmd) ([]byte, error) {
	if c.Stderr != nil {
		_, _ = io.WriteString(c.Stderr, "Load key: incorrect passphrase supplied to decrypt private key\n")
	}
	return nil, errors.New("exit status 255")
}
