package box

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `moteld box key` (DESIGN-box.md, "The box's key"; Console commands):
// root only; the key made once and never replaced; standard output the
// recipient alone; age.pub written when missing or another key's, and
// left as it is otherwise.
func TestBoxKey(t *testing.T) {
	run := func(b *testBox, euid int) (out, errOut string, err error) {
		var o, e bytes.Buffer
		err = runKey(euid, b.applier(hooks{}), &o, &e)
		return o.String(), e.String(), err
	}

	b := newTestBox(t)
	if _, _, err := run(b, 1000); err == nil || err.Error() != msgKeyNotRoot {
		t.Fatalf("not root: %v", err)
	}
	if _, err := os.Lstat(b.path(identityFile)); err == nil {
		t.Fatal("a refusal made a key")
	}

	out, errOut, err := run(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	recipient := strings.TrimSuffix(out, "\n")
	if !strings.HasPrefix(recipient, "age1") || strings.Count(out, "\n") != 1 {
		t.Fatalf("stdout %q: want the recipient alone", out)
	}
	if want := "box key: made /etc/motel/age/identity; its key above is the first line of deploy.example.com/secrets/recipients\n"; errOut != want {
		t.Errorf("stderr %q, want %q", errOut, want)
	}
	id, err := readIdentity(b.path(identityFile))
	if err != nil || id.Recipient().String() != recipient {
		t.Fatalf("the identity does not hold the key printed: %v", err)
	}
	if fi, err := os.Stat(b.path(identityFile)); err != nil || fi.Mode() != 0o600 {
		t.Errorf("identity mode: %v %v", fi.Mode(), err)
	}
	pub, err := os.ReadFile(b.path(agePubFile))
	if err != nil || string(pub) != recipient+"\n" {
		t.Fatalf("age.pub %q: %v", pub, err)
	}
	if fi, _ := os.Stat(b.path(agePubFile)); fi.Mode() != 0o644 {
		t.Errorf("age.pub mode %v", fi.Mode())
	}

	// Again: the same key, said so, nothing rewritten.
	before, _ := os.Stat(b.path(agePubFile))
	out, errOut, err = run(b, 0)
	if err != nil || out != recipient+"\n" || errOut != "box key: this box's key for secrets\n" {
		t.Fatalf("again: %q %q %v", out, errOut, err)
	}
	if after, _ := os.Stat(b.path(agePubFile)); !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Error("age.pub was rewritten though it held the key")
	}

	// age.pub naming another key, or gone, is written again; a temporary
	// a crash left beside it is removed.
	if err := os.WriteFile(b.path(agePubFile), []byte("age1other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(b.path("etc/motel"), ".age.pub.box-123")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(b, 0); err != nil {
		t.Fatal(err)
	}
	if pub, _ := os.ReadFile(b.path(agePubFile)); string(pub) != recipient+"\n" {
		t.Errorf("age.pub not put back: %q", pub)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("a crash's temporary was left")
	}
	if err := os.Remove(b.path(agePubFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(b, 0); err != nil {
		t.Fatal(err)
	}
	if pub, _ := os.ReadFile(b.path(agePubFile)); string(pub) != recipient+"\n" {
		t.Errorf("age.pub not made again: %q", pub)
	}

	// The right line in a file the handler cannot read, or another
	// account's, is written again: the mode and the owner a run makes.
	drift := []func(string) error{func(p string) error { return os.Chmod(p, 0o600) }}
	if os.Geteuid() == 0 {
		drift = append(drift, func(p string) error { return os.Lchown(p, 1000, 1000) })
	}
	for i, change := range drift {
		if err := change(b.path(agePubFile)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := run(b, 0); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(b.path(agePubFile))
		if err != nil || fi.Mode() != 0o644 || !ownedByUs(fi) {
			t.Errorf("drift %d: age.pub left as %v: %v", i, fi.Mode(), err)
		}
		if pub, _ := os.ReadFile(b.path(agePubFile)); string(pub) != recipient+"\n" {
			t.Errorf("drift %d: age.pub %q", i, pub)
		}
	}

	// An identity that does not read is refused and left as it is.
	garbage := []byte("not a key\n")
	if err := os.WriteFile(b.path(identityFile), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(b, 0)
	if err == nil || !strings.HasSuffix(err.Error(), "/etc/motel/age/identity "+errNotIdentity.Error()) {
		t.Fatalf("a bad identity: %v", err)
	}
	if got, _ := os.ReadFile(b.path(identityFile)); !bytes.Equal(got, garbage) {
		t.Error("a bad identity was replaced")
	}

	// Before init, the packaged Caddyfile names no host.
	b = newTestBox(t)
	b.writeInstalled([]byte("example.com {\n\trespond \"hello\"\n}\n"))
	if _, errOut, err := run(b, 0); err != nil || !strings.Contains(errOut, "the first line of <host>/secrets/recipients") {
		t.Errorf("before init: %q %v", errOut, err)
	}
}

// init makes the key after a green end and prints it; when it cannot,
// it says so and still exits 0, since the install stands.
func TestInitKeyLine(t *testing.T) {
	b := newTestBox(t)
	if err := os.Remove(b.path(filepath.Dir(identityFile))); err != nil {
		t.Fatal(err)
	}
	file := boxFile(2, b.alice)
	sha := b.repo.commit(file, &b.alice, b.base)
	out, err := b.initCmd(initDir(t, file), sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "init: could not make the box's key for secrets: ") || !strings.HasSuffix(out, "; moteld box key, as root, makes it\n") {
		t.Errorf("printed %q", out)
	}
	if !bytes.Equal(b.installed(), file) || b.applied().SHA != sha {
		t.Error("the install did not stand")
	}
}

// init installs no secrets: the secrets applied.json lists, and their
// tree, are kept from the file it replaces.
func TestInitKeepsSecrets(t *testing.T) {
	b := newTestBox(t)
	a := b.applied()
	a.SecretsTree, a.Secrets = strings.Repeat("e", 40), map[string]string{"example": digest([]byte("x"))}
	b.writeApplied(a)
	file := boxFile(2, b.alice)
	sha := b.repo.commit(file, &b.alice, b.base)
	if _, err := b.initCmd(initDir(t, file), sha); err != nil {
		t.Fatal(err)
	}
	if got := b.applied(); got.SHA != sha || got.SecretsTree != a.SecretsTree || got.Secrets["example"] != a.Secrets["example"] || len(got.Secrets) != 1 {
		t.Errorf("applied.json %+v", got)
	}
}
