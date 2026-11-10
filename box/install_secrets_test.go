package box

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/smallhoursorg/motel/box/proof"
	"github.com/smallhoursorg/motel/liveswap"
)

// newSecretsBox is a test box whose applier installs secrets: the
// secrets directory as tmpfiles.d makes it (2750, owned as the exchange
// tree is), the key's directory, and the box's key.
func newSecretsBox(t testing.TB) (*testBox, *age.X25519Identity) {
	t.Helper()
	b := newTestBox(t)
	b.secretsDirAs(0o750 | os.ModeSetgid)
	id, _, err := ensureIdentity(b.path(identityFile), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return b, id
}

func (b *testBox) secretsDirAs(mode os.FileMode) {
	b.t.Helper()
	if err := os.MkdirAll(b.path(secretsDir), 0o700); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chmod(b.path(secretsDir), mode); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testBox) envPath(app string) string { return b.path(secretsDir + "/" + app + ".env") }

// env is app's env file as it stands, nil when there is none.
func (b *testBox) env(app string) []byte {
	b.t.Helper()
	data, err := os.ReadFile(b.envPath(app))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		b.t.Fatal(err)
	}
	return data
}

// envSnapshot is what stands at app's env file, read without opening
// anything that is not a regular file (a FIFO would block).
func (b *testBox) envSnapshot(app string) string {
	b.t.Helper()
	fi, err := os.Lstat(b.envPath(app))
	if errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		b.t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		return fi.Mode().String()
	}
	return fi.Mode().String() + " " + string(b.env(app))
}

func (b *testBox) putEnv(app string, data []byte, mode os.FileMode) {
	b.t.Helper()
	if err := os.WriteFile(b.envPath(app), data, mode); err != nil {
		b.t.Fatal(err)
	}
	if err := os.Chmod(b.envPath(app), mode); err != nil {
		b.t.Fatal(err)
	}
}

func (b *testBox) envMode(app string) os.FileMode {
	b.t.Helper()
	fi, err := os.Lstat(b.envPath(app))
	if err != nil {
		b.t.Fatal(err)
	}
	return fi.Mode()
}

// rendered is the env file root writes for vars.
func rendered(t testing.TB, vars ...string) []byte {
	t.Helper()
	body, err := liveswap.RenderEnvFile(vars)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(envFileHeader), body...)
}

// secretsTreeOf is the secrets tree a bundle's commit proves.
func secretsTreeOf(t testing.TB, files map[string][]byte) string {
	t.Helper()
	bundle, err := proof.ReadBundle(tgz(t, files))
	if err != nil {
		t.Fatal(err)
	}
	s, err := proof.ProveSecrets(bundle.Commit, bundle.Trees, bundle.Blobs, bundle.Path, secretNames)
	if err != nil {
		t.Fatal(err)
	}
	return s.Tree
}

type secrets = map[string]map[string][]byte

// A push installs an app's secrets as one rendered env file, and the
// pushes after it change, keep and remove it — each green, each named
// by <app>/<VAR>, the Caddyfile reloaded only when it changes.
func TestApplySecrets(t *testing.T) {
	b, id := newSecretsBox(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }
	v2 := boxFile(2, b.alice)

	// Secrets added and the Caddyfile changed: both, one reload. A
	// trailing newline on a value is `echo`'s, and is stripped.
	c1 := b.repo.commitSecrets(v2, secrets{"example": {"DATABASE_URL": enc("postgres://db\n"), "STRIPE_KEY": enc("sk_1")}}, &b.alice, b.base)
	files1 := b.repo.bundleFiles(c1, b.base)
	r := b.pushOne(files1)
	if r.Phase != phaseApplied {
		t.Fatalf("%+v", r)
	}
	want := rendered(t, "DATABASE_URL=postgres://db", "STRIPE_KEY=sk_1")
	if got := b.env("example"); !bytes.Equal(got, want) {
		t.Fatalf("env file:\n%s\nwant\n%s", got, want)
	}
	if m := b.envMode("example"); m != 0o640 {
		t.Errorf("env file mode %v, want 0640", m)
	}
	a := b.applied()
	if a.SHA != c1 || a.SecretsTree != secretsTreeOf(t, files1) || len(a.Secrets) != 1 || a.Secrets["example"] != digest(want) {
		t.Errorf("applied.json %+v", a)
	}
	if !slices.Equal(r.SecretsAdded, []string{"example/DATABASE_URL", "example/STRIPE_KEY"}) || r.SecretsChanged != nil || r.SecretsRemoved != nil || r.SecretsOutOfBand != nil {
		t.Errorf("names %+v", r)
	}
	if !bytes.Equal(b.installed(), v2) || b.sd.reloaded != 1 {
		t.Errorf("Caddyfile installed %v, %d reloads", bytes.Equal(b.installed(), v2), b.sd.reloaded)
	}

	// One value rotated, the other re-encrypted as it was, the Caddyfile
	// unchanged: the env file alone, and nothing reloaded.
	c2 := b.repo.commitSecrets(v2, secrets{"example": {"DATABASE_URL": enc("postgres://db"), "STRIPE_KEY": enc("sk_2")}}, &b.alice, c1)
	r = b.pushOne(b.repo.bundleFiles(c2, c1))
	want = rendered(t, "DATABASE_URL=postgres://db", "STRIPE_KEY=sk_2")
	if r.Phase != phaseApplied || !bytes.Equal(b.env("example"), want) || b.sd.reloaded != 1 {
		t.Fatalf("%+v; %d reloads", r, b.sd.reloaded)
	}
	if !slices.Equal(r.SecretsChanged, []string{"example/STRIPE_KEY"}) || r.SecretsAdded != nil || r.SecretsRemoved != nil {
		t.Errorf("names %+v", r)
	}
	if r.Diff != "" {
		t.Errorf("a diff for an unchanged Caddyfile: %q", r.Diff)
	}

	// A rekey: new ciphertext, the same values. Nothing changes on disk,
	// so no_change, and applied.json records the new tree.
	c3 := b.repo.commitSecrets(v2, secrets{"example": {"DATABASE_URL": enc("postgres://db"), "STRIPE_KEY": enc("sk_2")}}, &b.alice, c2)
	files3 := b.repo.bundleFiles(c3, c2)
	r = b.pushOne(files3)
	if r.Phase != phaseNoChange || r.SecretsChanged != nil {
		t.Fatalf("%+v", r)
	}
	if a := b.applied(); a.SHA != c3 || a.SecretsTree != secretsTreeOf(t, files3) || a.Secrets["example"] != digest(want) {
		t.Errorf("applied.json %+v", a)
	}

	// Every secret gone, the recipients file kept: the env file is
	// removed, and applied.json lists none.
	c4 := b.repo.commitSecrets(v2, nil, &b.alice, c3)
	files4 := b.repo.bundleFiles(c4, c3)
	r = b.pushOne(files4)
	if r.Phase != phaseApplied || b.env("example") != nil || b.sd.reloaded != 1 {
		t.Fatalf("%+v; env %q", r, b.env("example"))
	}
	if !slices.Equal(r.SecretsRemoved, []string{"example/DATABASE_URL", "example/STRIPE_KEY"}) {
		t.Errorf("names %+v", r)
	}
	if a := b.applied(); len(a.Secrets) != 0 || a.SecretsTree != secretsTreeOf(t, files4) || a.SecretsTree == "" {
		t.Errorf("applied.json %+v", a)
	}
}

// What root does with what already stands at an env file's name
// (DESIGN-box.md, "Values and the env file", its table): a file it
// installed, at its digest or not, one a console made, a link, and a
// name it never writes.
func TestApplySecretsWhatStands(t *testing.T) {
	type tc struct {
		name string
		// before sets the box up: it may push, and may write in secrets/.
		before func(b *testBox, enc func(string) []byte) (head string)
		// headFiles is the push's secrets.
		head   func(enc func(string) []byte) secrets
		phase  string
		oob    []string
		listed bool // applied.json lists example at the end
	}
	good := func(enc func(string) []byte) secrets { return secrets{"example": {"KEY": enc("value")}} }
	want := func(t *testing.T) []byte { return rendered(t, "KEY=value") }
	install := func(b *testBox, enc func(string) []byte) string {
		head := b.repo.commitSecrets(b.v1, good(enc), &b.alice, b.base)
		if r := b.pushOne(b.repo.bundleFiles(head, b.base)); r.Phase != phaseApplied {
			b.t.Fatalf("setup: %+v", r)
		}
		return head
	}
	cases := []tc{
		{name: "a console's file with HEAD's bytes is adopted, unnamed", phase: phaseNoChange, listed: true, head: good,
			before: func(b *testBox, _ func(string) []byte) string {
				b.putEnv("example", rendered(b.t, "KEY=value"), 0o640)
				return ""
			}},
		{name: "a console's file with other bytes is overwritten and named", phase: phaseApplied, oob: []string{"example"}, listed: true, head: good,
			before: func(b *testBox, _ func(string) []byte) string {
				b.putEnv("example", []byte("KEY=old\n"), 0o640)
				return ""
			}},
		{name: "a console's file at the wrong mode is rewritten and named", phase: phaseApplied, oob: []string{"example"}, listed: true, head: good,
			before: func(b *testBox, _ func(string) []byte) string {
				b.putEnv("example", rendered(b.t, "KEY=value"), 0o644)
				return ""
			}},
		{name: "an installed file edited at the console is rewritten and named", phase: phaseApplied, oob: []string{"example"}, listed: true, head: good,
			before: func(b *testBox, enc func(string) []byte) string {
				head := install(b, enc)
				b.putEnv("example", []byte("KEY=edited\n"), 0o640)
				return head
			}},
		{name: "an installed file whose mode drifted is rewritten and named", phase: phaseApplied, oob: []string{"example"}, listed: true, head: good,
			before: func(b *testBox, enc func(string) []byte) string {
				head := install(b, enc)
				if err := os.Chmod(b.envPath("example"), 0o644); err != nil {
					b.t.Fatal(err)
				}
				return head
			}},
		{name: "an installed file lost is written again and named", phase: phaseApplied, oob: []string{"example"}, listed: true, head: good,
			before: func(b *testBox, enc func(string) []byte) string {
				head := install(b, enc)
				if err := os.Remove(b.envPath("example")); err != nil {
					b.t.Fatal(err)
				}
				return head
			}},
		{name: "an installed file HEAD drops, edited at the console, is removed and named", phase: phaseApplied, oob: []string{"example"},
			head: func(func(string) []byte) secrets { return nil },
			before: func(b *testBox, enc func(string) []byte) string {
				head := install(b, enc)
				b.putEnv("example", []byte("KEY=edited\n"), 0o640)
				return head
			}},
		{name: "an installed file HEAD drops, already gone, is named and no longer listed", phase: phaseNoChange, oob: []string{"example"},
			head: func(func(string) []byte) secrets { return nil },
			before: func(b *testBox, enc func(string) []byte) string {
				head := install(b, enc)
				if err := os.Remove(b.envPath("example")); err != nil {
					b.t.Fatal(err)
				}
				return head
			}},
		{name: "a console's file for an app HEAD lacks, never listed, is left alone", phase: phaseNoChange,
			head: func(func(string) []byte) secrets { return nil },
			before: func(b *testBox, _ func(string) []byte) string {
				b.putEnv("example", []byte("KEY=console\n"), 0o640)
				return ""
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, id := newSecretsBox(t)
			enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }
			// Names root never writes stay as they are throughout.
			if err := os.WriteFile(b.path(secretsDir+"/notes.txt"), []byte("the console's\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			b.putEnv("other", []byte("OTHER=1\n"), 0o640)
			base := c.before(b, enc)
			if base == "" {
				base = b.base
			}
			head := b.repo.commitSecrets(b.v1, c.head(enc), &b.alice, base)
			r := b.pushOne(b.repo.bundleFiles(head, base))
			if r.Phase != c.phase || !slices.Equal(r.SecretsOutOfBand, c.oob) {
				t.Fatalf("%+v, want %s, out of band %v", r, c.phase, c.oob)
			}
			wantEnv := []byte(nil)
			if c.head(enc) != nil {
				wantEnv = want(t)
			}
			if strings.HasSuffix(c.name, "left alone") {
				wantEnv = []byte("KEY=console\n")
			}
			if got := b.env("example"); !bytes.Equal(got, wantEnv) {
				t.Errorf("env file %q, want %q", got, wantEnv)
			}
			if wantEnv != nil && !strings.HasSuffix(c.name, "left alone") && b.envMode("example") != 0o640 {
				t.Errorf("env file mode %v", b.envMode("example"))
			}
			if _, listed := b.applied().Secrets["example"]; listed != c.listed {
				t.Errorf("listed %v, want %v: %+v", listed, c.listed, b.applied())
			}
			if got, err := os.ReadFile(b.path(secretsDir + "/notes.txt")); err != nil || string(got) != "the console's\n" {
				t.Errorf("notes.txt: %q %v", got, err)
			}
			if got, err := os.ReadFile(b.path(secretsDir + "/other.env")); err != nil || string(got) != "OTHER=1\n" {
				t.Errorf("other.env: %q %v", got, err)
			}
		})
	}
}

// A link at an env file's name is replaced, never followed: the file it
// points at is untouched.
func TestApplySecretsReplacesALink(t *testing.T) {
	b, id := newSecretsBox(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("NOT=root's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, b.envPath("example")); err != nil {
		t.Fatal(err)
	}
	head := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
	if r := b.pushOne(b.repo.bundleFiles(head, b.base)); r.Phase != phaseApplied || !slices.Equal(r.SecretsOutOfBand, []string{"example"}) {
		t.Fatalf("%+v", r)
	}
	if m := b.envMode("example"); m != 0o640 {
		t.Errorf("the link was not replaced by the env file: %v", m)
	}
	if got, _ := os.ReadFile(target); string(got) != "NOT=root's\n" {
		t.Errorf("the link was followed: %q", got)
	}
}

// Step 15a's failures: each the catalogue's text, the box's own errors
// `failed` and the push's `refused`, and nothing changed. A HEAD with no
// secret needs neither key nor directory.
func TestApplySecretsFailures(t *testing.T) {
	type tc struct {
		name  string
		setup func(b *testBox, id *age.X25519Identity) secrets
		phase string
		msg   string // exact, or "~" + a substring
	}
	one := func(id *age.X25519Identity, t testing.TB) secrets {
		return secrets{"example": {"KEY": encrypt(t.(*testing.T), []byte("v"), id.Recipient())}}
	}
	cases := []tc{
		{name: "no key", phase: phaseFailed, msg: msgNoKey, setup: func(b *testBox, id *age.X25519Identity) secrets {
			if err := os.Remove(b.path(identityFile)); err != nil {
				b.t.Fatal(err)
			}
			return one(id, b.t)
		}},
		{name: "a key that does not read", phase: phaseFailed, msg: "~the install failed before the box's files changed: " + "", setup: func(b *testBox, id *age.X25519Identity) secrets {
			if err := os.WriteFile(b.path(identityFile), []byte("not a key\n"), 0o600); err != nil {
				b.t.Fatal(err)
			}
			return one(id, b.t)
		}},
		{name: "no secrets directory", phase: phaseFailed, msg: msgSecretsDir, setup: func(b *testBox, id *age.X25519Identity) secrets {
			if err := os.Remove(b.path(secretsDir)); err != nil {
				b.t.Fatal(err)
			}
			return one(id, b.t)
		}},
		{name: "a secrets directory others can read", phase: phaseFailed, msg: msgSecretsDir, setup: func(b *testBox, id *age.X25519Identity) secrets {
			b.secretsDirAs(0o755 | os.ModeSetgid)
			return one(id, b.t)
		}},
		{name: "a secrets directory that is not setgid", phase: phaseFailed, msg: msgSecretsDir, setup: func(b *testBox, id *age.X25519Identity) secrets {
			b.secretsDirAs(0o750)
			return one(id, b.t)
		}},
		{name: "an env file larger than any push writes", phase: phaseFailed, msg: fill(msgEnvTooLarge, "example"), setup: func(b *testBox, id *age.X25519Identity) secrets {
			b.putEnv("example", bytes.Repeat([]byte("x"), maxEnvRead+1), 0o640)
			return one(id, b.t)
		}},
		{name: "a secret not encrypted to this box", phase: phaseRefused, setup: func(b *testBox, _ *age.X25519Identity) secrets {
			return one(newIdentity(b.t.(*testing.T)), b.t)
		}},
		{name: "is-active unanswered after 15a", phase: phaseFailed, msg: "~the install failed before the box's files changed: ", setup: func(b *testBox, id *age.X25519Identity) secrets {
			b.sd.stateErr = errors.New("dbus: no reply")
			return one(id, b.t)
		}},
		{name: "a directory at the env file's name", phase: phaseFailed, msg: fill(msgEnvNotRegular, "example"), setup: func(b *testBox, id *age.X25519Identity) secrets {
			if err := os.Mkdir(b.envPath("example"), 0o750); err != nil {
				b.t.Fatal(err)
			}
			return one(id, b.t)
		}},
		{name: "a FIFO at the env file's name", phase: phaseFailed, msg: fill(msgEnvNotRegular, "example"), setup: func(b *testBox, id *age.X25519Identity) secrets {
			if err := syscall.Mkfifo(b.envPath("example"), 0o640); err != nil {
				b.t.Fatal(err)
			}
			return one(id, b.t)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, id := newSecretsBox(t)
			s := c.setup(b, id)
			envBefore := b.envSnapshot("example")
			head := b.repo.commitSecrets(boxFile(2, b.alice), s, &b.alice, b.base)
			r := b.pushOne(b.repo.bundleFiles(head, b.base))
			msg := c.msg
			if c.phase == phaseRefused {
				msg = "box1/secrets/example/KEY.age is not encrypted to this box's key (" + id.Recipient().String() + "): put that key first in box1/secrets/recipients and run make rekey"
			}
			switch {
			case r.Phase != c.phase:
				t.Fatalf("%+v, want %s", r, c.phase)
			case strings.HasPrefix(msg, "~") && !strings.Contains(r.Error, msg[1:]):
				t.Fatalf("%q, want it to contain %q", r.Error, msg[1:])
			case !strings.HasPrefix(msg, "~") && r.Error != msg:
				t.Fatalf("%q, want %q", r.Error, msg)
			}
			b.unchanged()
			if b.envSnapshot("example") != envBefore {
				t.Error("the env file changed")
			}
		})
	}
}

// A HEAD with no secret reads no key and needs no directory: a box with
// neither applies it — and a push that removes the
// env files applied.json lists reads no key either.
func TestApplySecretsNeedNoKeyWithout(t *testing.T) {
	b := newTestBox(t) // no key, no secrets directory
	head := b.repo.commitSecrets(boxFile(2, b.alice), nil, &b.alice, b.base)
	if r := b.pushOne(b.repo.bundleFiles(head, b.base)); r.Phase != phaseApplied {
		t.Fatalf("recipients alone: %+v", r)
	}

	b, _ = newSecretsBox(t)
	if err := os.Remove(b.path(identityFile)); err != nil {
		t.Fatal(err)
	}
	old := rendered(t, "KEY=old")
	b.putEnv("example", old, 0o640)
	a := b.applied()
	a.SecretsTree, a.Secrets = strings.Repeat("e", 40), map[string]string{"example": digest(old)}
	b.writeApplied(a)
	head = b.repo.commitSecrets(b.v1, nil, &b.alice, b.base)
	r := b.pushOne(b.repo.bundleFiles(head, b.base))
	if r.Phase != phaseApplied || b.env("example") != nil || !slices.Equal(r.SecretsRemoved, []string{"example/KEY"}) || b.sd.reloaded != 0 {
		t.Fatalf("%+v; env %q, %d reloads", r, b.env("example"), b.sd.reloaded)
	}

	// A bundle that leaves out a listed blob is refused, never read as a
	// deletion.
	val := encrypt(t, []byte("v"), newIdentity(t).Recipient())
	c := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": val}}, &b.alice, head)
	files := b.repo.bundleFiles(c, head)
	delete(files, "blobs/"+proof.ObjectID("blob", val))
	r = b.pushOne(files)
	if r.Phase != phaseRefused || r.Error != "bundle: blob "+proof.ObjectID("blob", val)+", needed for box1/secrets/example/KEY.age in "+c+", is not in the bundle" {
		t.Fatalf("%+v", r)
	}
}

// A failed reload rolls back the env files with the Caddyfile: one the
// push added is removed, one it changed gets its previous bytes back.
func TestApplySecretsRollBack(t *testing.T) {
	b, id := newSecretsBox(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }
	old := rendered(t, "KEY=old")
	b.putEnv("example", old, 0o640)
	a := b.applied()
	a.SecretsTree, a.Secrets = strings.Repeat("e", 40), map[string]string{"example": digest(old)}
	b.writeApplied(a)
	head := b.repo.commitSecrets(boxFile(2, b.alice), secrets{"example": {"KEY": enc("new")}, "api": {"TOKEN": enc("t")}}, &b.alice, b.base)
	b.sd.reloads = []error{errReload}
	r := b.pushOne(b.repo.bundleFiles(head, b.base))
	if r.Phase != phaseRolledBack || r.Error != msgReloadFailed {
		t.Fatalf("%+v", r)
	}
	if !bytes.Equal(b.env("example"), old) || b.env("api") != nil || !bytes.Equal(b.installed(), b.v1) {
		t.Errorf("not rolled back: example %q, api %q", b.env("example"), b.env("api"))
	}
	if b.applied().SHA != b.base || b.sd.reloaded != 2 {
		t.Errorf("baseline %s, %d reloads", b.applied().SHA, b.sd.reloaded)
	}
}

// failNth fails point at its nth arrival (1-based) only.
func failNth(point string, n int) func(string) error {
	seen := 0
	return func(p string) error {
		if p != point {
			return nil
		}
		if seen++; seen == n {
			return fmt.Errorf("%s: %w", p, errNoSpace)
		}
		return nil
	}
}

// The Failure-mode table's rows for a set with env files in it: each
// durable write failing, or a crash once it is durable, ends with every
// file of the set at its previous bytes (1) or its new ones (2) — I1 at
// every instant a test can see — in the catalogue's words. "change" is
// a push of the Caddyfile and two env files (api added, example
// changed); "env" a push of env files alone (api added, example
// removed), which reloads nothing.
func TestFailureModesSecrets(t *testing.T) {
	type row struct {
		name, holds, scen string
		fail              func(string) error
		crash             string
		crashN            int
		reloads           []error
		crashReload       bool
		fullDisk          bool
		fullPhase         string
		journal           string
		state             int
		advanced          bool
		phase, msg        string
		reloads2          int
	}
	failed := "~the install failed before the box's files changed: "
	rows := []row{
		{name: "result verified/crash after", holds: "I1 I2 I3", scen: "change", crash: "result:verified", crashN: 1, state: 1, phase: phaseFailed, msg: msgInterruptedFiles},
		{name: "first env file/write fails", holds: "I1", scen: "change", fail: failNth("envfile:new", 1), state: 1, phase: phaseFailed, msg: failed + "envfile:new: no space"},
		{name: "first env file/crash after", holds: "I1 I5", scen: "change", crash: "envfile:new", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloads2: 1},
		{name: "second env file/write fails", holds: "I1", scen: "change", fail: failNth("envfile:new", 2), state: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecordedFiles, reloads2: 1},
		{name: "second env file/crash after", holds: "I1 I5", scen: "change", crash: "envfile:new", crashN: 2, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloads2: 1},
		{name: "the Caddyfile after env files/write fails", holds: "I1", scen: "change", fail: failNth("caddyfile:new", 1), state: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecordedFiles, reloads2: 1},
		{name: "the Caddyfile after env files/crash after", holds: "I1 I5", scen: "change", crash: "caddyfile:new", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloads2: 1},
		{name: "record swapped/write fails", holds: "I1 I4", scen: "change", fail: failNth("record:swapped", 1), state: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecordedFiles, reloads2: 1},
		{name: "record swapped/crash after", holds: "I1 I5", scen: "change", crash: "record:swapped", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloads2: 1},
		{name: "reload/fails", holds: "I1 I4", scen: "change", reloads: []error{errReload}, state: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "reload/crash after", holds: "I1 I4 I5", scen: "change", crashReload: true, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles, reloads2: 2},
		{name: "record applied/write fails", holds: "I1 I4", scen: "change", fail: failNth("record:applied", 1), state: 1, phase: phaseRolledBack, msg: msgRecordAfterReload, reloads2: 2},
		{name: "record applied/crash after", holds: "I4 I5 I8", scen: "change", crash: "record:applied", crashN: 1, state: 2, advanced: true, phase: phaseApplied, reloads2: 1},
		{name: "applied.json/write fails", holds: "I4", scen: "change", fail: failNth("applied.json", 1), journal: "box baseline could not be recorded", state: 2, phase: phaseUnknown, reloads2: 1},
		{name: "previous env file back/fails twice: full disk", holds: "I1 I2 I5", scen: "change", reloads: []error{errReload}, fail: failAt(map[string]int{"envfile:prev": -1}),
			fullDisk: true, fullPhase: phaseRollingBack, journal: fill(msgEnvDiskFull, "example"), state: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "an added env file removed/fails twice: full disk", holds: "I1 I2 I5", scen: "change", reloads: []error{errReload}, fail: failAt(map[string]int{"envfile:unlink": -1}),
			fullDisk: true, fullPhase: phaseRollingBack, journal: fill(msgEnvDiskFull, "api"), state: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "previous env file back/crash after", holds: "I1 I5", scen: "change", reloads: []error{errReload}, crash: "envfile:prev", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgReloadFailed, reloads2: 2},
		{name: "env files alone/applied", holds: "I1 I4", scen: "env", state: 2, advanced: true, phase: phaseApplied},
		{name: "env files alone/a removal fails", holds: "I1", scen: "env", fail: failAt(map[string]int{"envfile:remove": -1}), state: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecordedFiles},
		{name: "env files alone/crash after the removal", holds: "I1 I5", scen: "env", crash: "envfile:remove", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles},
		{name: "env files alone/record swapped crash after", holds: "I1 I5", scen: "env", crash: "record:swapped", crashN: 1, state: 1, phase: phaseRolledBack, msg: msgRolledBackInterruptedFiles},
		{name: "env files alone/record applied fails", holds: "I1 I4", scen: "env", fail: failNth("record:applied", 1), state: 1, phase: phaseRolledBack, msg: msgRolledBackUnrecordedFiles},
		{name: "env files alone/record installing fails", holds: "I1 I5", scen: "env", fail: failNth("record:installing", 1), state: 1, phase: phaseFailed, msg: failed + "record:installing: no space"},
		{name: "env files alone/record installing crash after", holds: "I1 I5", scen: "env", crash: "record:installing", crashN: 1, state: 1, phase: phaseFailed, msg: msgInterruptedFiles},
	}
	for _, rw := range rows {
		t.Run(rw.name+" ["+rw.holds+"]", func(t *testing.T) {
			b, id := newSecretsBox(t)
			enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }
			// The box before: example's secrets installed by a push of
			// env files alone, which reloads nothing.
			before := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": enc("old")}}, &b.alice, b.base)
			if r := b.pushOne(b.repo.bundleFiles(before, b.base)); r.Phase != phaseApplied || b.sd.reloaded != 0 {
				t.Fatalf("setup: %+v", r)
			}
			v1, exampleOld := b.v1, rendered(t, "KEY=old")
			var head string
			v2, exampleNew := v1, []byte(nil)
			switch rw.scen {
			case "change":
				v2, exampleNew = boxFile(2, b.alice), rendered(t, "KEY=new")
				head = b.repo.commitSecrets(v2, secrets{"example": {"KEY": enc("new")}, "api": {"TOKEN": enc("t")}}, &b.alice, before)
			case "env":
				head = b.repo.commitSecrets(v1, secrets{"api": {"TOKEN": enc("t")}}, &b.alice, before)
			}
			apiNew := rendered(t, "TOKEN=t")
			id1 := b.push(b.repo.bundleFiles(head, before))
			b.sd.reloads = rw.reloads
			h := hooks{fail: rw.fail}
			// I1 at every instant a test can see: each file of the set is
			// one of its two.
			whole := func(when string) {
				t.Helper()
				if got := b.installed(); !bytes.Equal(got, v1) && !bytes.Equal(got, v2) {
					t.Fatalf("%s: the Caddyfile is neither file (I1)", when)
				}
				if got := b.env("example"); !bytes.Equal(got, exampleOld) && !bytes.Equal(got, exampleNew) {
					t.Fatalf("%s: example's env file is neither (I1): %q", when, got)
				}
				if got := b.env("api"); got != nil && !bytes.Equal(got, apiNew) {
					t.Fatalf("%s: api's env file is neither (I1): %q", when, got)
				}
			}
			switch {
			case rw.crash != "":
				h.crash = crashAfter(rw.crash, rw.crashN)
				if p := b.runCrash(h); p != rw.crash {
					t.Fatalf("crashed at %s", p)
				}
				whole("after the crash")
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("recovery: %v", err)
				}
			case rw.crashReload:
				b.sd.onReload = func(n int) {
					if n == 1 {
						panic(crashed{point: "reload"})
					}
				}
				b.runCrash(h)
				b.sd.onReload = nil
				whole("after the crash")
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("recovery: %v", err)
				}
			case rw.fullDisk:
				// The exit text names the file that could not be put back.
				if err := b.run(h); !errors.Is(err, errFullDisk) || err.Error() != rw.journal {
					t.Fatalf("first run: %v, want the full-disk end %q", err, rw.journal)
				}
				whole("at the full-disk end")
				if r := b.record(); r == nil || r.Phase != rw.fullPhase {
					t.Fatalf("full disk leaves the record (phase %s): %+v", rw.fullPhase, r)
				}
				if !b.errorLogged(rw.journal) {
					t.Errorf("no error-level line %q", rw.journal)
				}
				if err := b.run(hooks{}); err != nil {
					t.Fatalf("the console's run after freeing space: %v", err)
				}
			default:
				if err := b.run(h); err != nil {
					t.Fatalf("run: %v", err)
				}
			}
			b.settled()
			wantCaddy, wantExample, wantAPI := v1, exampleOld, []byte(nil)
			if rw.state == 2 {
				wantCaddy, wantExample, wantAPI = v2, exampleNew, apiNew
			}
			if !bytes.Equal(b.installed(), wantCaddy) || !bytes.Equal(b.env("example"), wantExample) || !bytes.Equal(b.env("api"), wantAPI) {
				t.Errorf("the box is not at state %d: example %q, api %q", rw.state, b.env("example"), b.env("api"))
			}
			if wantAPI != nil && b.envMode("api") != 0o640 {
				t.Errorf("api's env file mode %v, want 0640", b.envMode("api"))
			}
			if adv := b.applied().SHA == head; adv != rw.advanced {
				t.Errorf("baseline advanced = %v, want %v (I4)", adv, rw.advanced)
			}
			r := b.result(id1)
			switch {
			case r == nil:
				t.Fatalf("no result, want %s", rw.phase)
			case r.Phase != rw.phase:
				t.Errorf("result phase %s (%s), want %s", r.Phase, r.Error, rw.phase)
			case strings.HasPrefix(rw.msg, "~") && !strings.Contains(r.Error, rw.msg[1:]):
				t.Errorf("result error %q, want it to contain %q", r.Error, rw.msg[1:])
			case rw.msg != "" && !strings.HasPrefix(rw.msg, "~") && r.Error != rw.msg:
				t.Errorf("result error %q, want %q", r.Error, rw.msg)
			}
			if rw.journal != "" && !b.errorLogged(rw.journal) {
				t.Errorf("no error-level line %q", rw.journal)
			}
			if b.sd.reloaded != rw.reloads2 {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, rw.reloads2)
			}
			// I9: no value reached a result or a journal line.
			for _, e := range b.logs.All() {
				for _, v := range []string{"KEY=new", "TOKEN=t", "KEY=old"} {
					if strings.Contains(fmt.Sprint(e.ContextMap()), v) || strings.Contains(e.Message, v) {
						t.Errorf("a value in the journal: %s %v", e.Message, e.ContextMap())
					}
				}
			}
		})
	}
}

// The swap writes env files, then the Caddyfile, then removes env files;
// a rollback puts back the env files that had bytes, then the Caddyfile,
// then removes the ones the push added — so no Caddyfile on disk names
// an env file that is not there.
func TestSecretsSwapOrder(t *testing.T) {
	b, id := newSecretsBox(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }
	before := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": enc("old")}}, &b.alice, b.base)
	if r := b.pushOne(b.repo.bundleFiles(before, b.base)); r.Phase != phaseApplied {
		t.Fatalf("setup: %+v", r)
	}
	// api added, example removed, the Caddyfile changed; the reload fails.
	head := b.repo.commitSecrets(boxFile(2, b.alice), secrets{"api": {"TOKEN": enc("t")}}, &b.alice, before)
	b.push(b.repo.bundleFiles(head, before))
	b.sd.reloads = []error{errReload}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, p := range b.lastPoint {
		if strings.HasPrefix(p, "envfile:") || strings.HasPrefix(p, "caddyfile:") {
			writes = append(writes, p)
		}
	}
	want := []string{"envfile:new", "caddyfile:new", "envfile:remove", "envfile:prev", "caddyfile:prev", "envfile:unlink"}
	if !slices.Equal(writes, want) {
		t.Errorf("writes in order %v, want %v", writes, want)
	}
	if !bytes.Equal(b.env("example"), rendered(t, "KEY=old")) || b.env("api") != nil || !bytes.Equal(b.installed(), b.v1) {
		t.Error("not rolled back")
	}
}

// A push of env files alone puts them back after a failure even if the
// console edited the Caddyfile meanwhile: the push never touched it.
// The edit is left as it is, and the next push reports it.
func TestApplySecretsRollbackIgnoresAnUntouchedCaddyfile(t *testing.T) {
	b, id := newSecretsBox(t)
	head := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
	pushed := b.push(b.repo.bundleFiles(head, b.base))
	edited := append(slices.Clone(b.v1), "# edited\n"...)
	// The edit lands after the swap's files were read back, as the
	// record `applied` fails: the post-swap failure's rollback meets it.
	h := hooks{fail: func(p string) error {
		if p != "record:applied" {
			return nil
		}
		b.writeInstalled(edited)
		return fmt.Errorf("%s: %w", p, errNoSpace)
	}}
	if err := b.run(h); err != nil {
		t.Fatal(err)
	}
	r := b.result(pushed)
	if r == nil || r.Phase != phaseRolledBack || r.Error != msgRolledBackUnrecordedFiles {
		t.Fatalf("%+v", r)
	}
	if b.env("example") != nil || !bytes.Equal(b.installed(), edited) || b.sd.reloaded != 0 {
		t.Errorf("env %q, the console's edit kept %v, %d reloads", b.env("example"), bytes.Equal(b.installed(), edited), b.sd.reloaded)
	}
}

// An env file rewritten for its mode alone changes no bytes: a later
// write of the swap that fails is the first write's failure, `failed`,
// with nothing to roll back or reload.
func TestApplySecretsModeFixIsNoChange(t *testing.T) {
	b, id := newSecretsBox(t)
	b.putEnv("example", rendered(t, "KEY=v"), 0o644)
	head := b.repo.commitSecrets(boxFile(2, b.alice), secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
	b.push(b.repo.bundleFiles(head, b.base))
	if err := b.run(hooks{fail: failAt(map[string]int{"caddyfile:new": -1})}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	r := b.result(b.names(b.x("out"))[0][:32])
	if r == nil || r.Phase != phaseFailed || !strings.HasPrefix(r.Error, "the install failed before the box's files changed: caddyfile:new") || b.sd.reloaded != 0 {
		t.Fatalf("%+v, %d reloads", r, b.sd.reloaded)
	}
}

// Names read from an env file a console made are held to the caps a
// record and a result are read back with: a variable name no secret can
// have is not reported, and the lists stop at their cap — so the push's
// result reads back, and so would its record.
func TestApplySecretsNamesFromAConsoleFile(t *testing.T) {
	b, id := newSecretsBox(t)
	var file strings.Builder
	file.WriteString(strings.Repeat("L", proof.MaxVarName+1) + "=x\n")
	for i := 0; i < maxSecretNames+100; i++ {
		fmt.Fprintf(&file, "V%04d=\n", i)
	}
	b.putEnv("example", []byte(file.String()), 0o640)
	head := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
	r := b.pushOne(b.repo.bundleFiles(head, b.base))
	if r.Phase != phaseApplied || len(r.SecretsRemoved) != maxSecretNames || !slices.Equal(r.SecretsAdded, []string{"example/KEY"}) {
		t.Fatalf("%s %q: %d removed", r.Phase, r.Error, len(r.SecretsRemoved))
	}
	if slices.ContainsFunc(r.SecretsRemoved, func(s string) bool { return strings.Contains(s, "LLL") }) {
		t.Error("a name no secret can have was reported")
	}
}

// A directory a console puts at an env file's name after the transaction
// read it is never removed with what it holds — not by the swap's
// removal, not by a rollback's — and the push ends `unknown`, left as
// found.
func TestApplySecretsRacedDirectoryIsKept(t *testing.T) {
	raceDir := func(b *testBox, app string) func(string) error {
		return func(string) error {
			if fi, err := os.Lstat(b.envPath(app)); err == nil && fi.IsDir() {
				return nil // a retry: already raced
			}
			_ = os.Remove(b.envPath(app))
			if err := os.Mkdir(b.envPath(app), 0o750); err != nil {
				b.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.envPath(app), "keep"), []byte("the console's"), 0o600); err != nil {
				b.t.Fatal(err)
			}
			return nil
		}
	}
	kept := func(b *testBox, app string) {
		b.t.Helper()
		if got, err := os.ReadFile(filepath.Join(b.envPath(app), "keep")); err != nil || string(got) != "the console's" {
			b.t.Errorf("the console's directory at %s.env was not kept: %q %v", app, got, err)
		}
	}

	t.Run("the swap's removal", func(t *testing.T) {
		b, id := newSecretsBox(t)
		before := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
		if r := b.pushOne(b.repo.bundleFiles(before, b.base)); r.Phase != phaseApplied {
			t.Fatalf("setup: %+v", r)
		}
		head := b.repo.commitSecrets(b.v1, nil, &b.alice, before)
		pushed := b.push(b.repo.bundleFiles(head, before))
		race := raceDir(b, "example")
		if err := b.run(hooks{fail: func(p string) error {
			if p == "envfile:remove" {
				return race(p)
			}
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(pushed); r == nil || r.Phase != phaseUnknown || r.Error != fill(msgEnvChanged, "example") {
			t.Fatalf("%+v", r)
		}
		kept(b, "example")
	})

	t.Run("the rollback's removal", func(t *testing.T) {
		b, id := newSecretsBox(t)
		head := b.repo.commitSecrets(boxFile(2, b.alice), secrets{"api": {"TOKEN": encrypt(t, []byte("t"), id.Recipient())}}, &b.alice, b.base)
		pushed := b.push(b.repo.bundleFiles(head, b.base))
		b.sd.reloads = []error{errReload}
		race := raceDir(b, "api")
		if err := b.run(hooks{fail: func(p string) error {
			if p == "envfile:unlink" {
				return race(p)
			}
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(pushed); r == nil || r.Phase != phaseUnknown || r.Error != fill(msgEnvChanged, "api") {
			t.Fatalf("%+v", r)
		}
		kept(b, "api")
	})
}

// The sweep of the env files' temporaries removes only the exact shape
// root writes, a regular file, and nothing through a secretsDir that is
// a link.
func TestEnvTempSweep(t *testing.T) {
	b, _ := newSecretsBox(t)
	id := strings.Repeat("ab", 16)
	ours := ".example.env.box-" + id
	keep := []string{".notes.env.box-backup", ".Example.env.box-" + id, "example.env.box-" + id, ".x.env.box-" + strings.Repeat("A", 32)}
	for _, n := range append([]string{ours}, keep...) {
		if err := os.WriteFile(b.path(secretsDir+"/"+n), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	b.applier(hooks{}).removeEnvTemps("")
	if _, err := os.Lstat(b.path(secretsDir + "/" + ours)); err == nil {
		t.Error("the swap's temporary was left")
	}
	for _, n := range keep {
		if _, err := os.Lstat(b.path(secretsDir + "/" + n)); err != nil {
			t.Errorf("%s was removed", n)
		}
	}

	// A link at secretsDir: nothing is removed through it.
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, ours), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(b.path(secretsDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, b.path(secretsDir)); err != nil {
		t.Fatal(err)
	}
	b.applier(hooks{}).removeEnvTemps("")
	if _, err := os.Lstat(filepath.Join(elsewhere, ours)); err != nil {
		t.Error("the sweep followed a link at secretsDir")
	}
}

// A console edit of an env file under the transaction is left as found,
// `unknown`, in the catalogue's words for that file.
func TestApplySecretsChangedDuringTransaction(t *testing.T) {
	b, id := newSecretsBox(t)
	head := b.repo.commitSecrets(boxFile(2, b.alice), secrets{"example": {"KEY": encrypt(t, []byte("new"), id.Recipient())}}, &b.alice, b.base)
	pushed := b.push(b.repo.bundleFiles(head, b.base))
	edited := []byte("KEY=console\n")
	h := hooks{crash: func(p string) {
		if p == "record:installing" {
			b.putEnv("example", edited, 0o640)
		}
	}}
	if err := b.run(h); err != nil {
		t.Fatal(err)
	}
	b.settled()
	r := b.result(pushed)
	if r == nil || r.Phase != phaseUnknown || r.Error != fill(msgEnvChanged, "example") {
		t.Fatalf("%+v", r)
	}
	if !bytes.Equal(b.env("example"), edited) || !bytes.Equal(b.installed(), b.v1) || b.applied().SHA != b.base {
		t.Error("the console's edit was not left as found")
	}
}

// The record's secrets are held to the shape root writes: one that is
// not is never acted on.
func TestRecordSecretsValid(t *testing.T) {
	v1 := []byte("v1")
	ok := record{ID: strings.Repeat("ab", 16), Origin: originApplier, Phase: phaseInstalling, Commit: strings.Repeat("c", 40), Path: testPath,
		Prev: v1, PrevSHA256: digest(v1), NewSHA256: digest([]byte("v2")),
		Secrets:          []envEntry{{App: "api", NewSHA256: digest([]byte("a"))}, {App: "example", Prev: []byte("old"), PrevSHA256: digest([]byte("old")), NewSHA256: ""}},
		SecretsTree:      strings.Repeat("d", 40),
		SecretsInstalled: map[string]string{"api": digest([]byte("a"))},
		SecretsAdded:     []string{"api/TOKEN"}, SecretsRemoved: []string{"example/KEY"}, SecretsOutOfBand: []string{"example"},
	}
	if err := ok.valid(); err != nil {
		t.Fatalf("a record root writes: %v", err)
	}
	for name, edit := range map[string]func(r *record){
		"out of order":          func(r *record) { r.Secrets[0], r.Secrets[1] = r.Secrets[1], r.Secrets[0] },
		"an app twice":          func(r *record) { r.Secrets[1].App = "api" },
		"an app out of grammar": func(r *record) { r.Secrets[0].App = "../x" },
		"prev not its digest":   func(r *record) { r.Secrets[1].Prev = []byte("other") },
		"absent with bytes":     func(r *record) { r.Secrets[0].Prev = []byte("x") },
		"absent before and after": func(r *record) {
			r.Secrets[1].Prev, r.Secrets[1].PrevSHA256 = nil, ""
		},
		"a digest not hex":       func(r *record) { r.Secrets[0].NewSHA256 = "x" },
		"a tree not an id":       func(r *record) { r.SecretsTree = "tree" },
		"a listed app not one":   func(r *record) { r.SecretsInstalled["A"] = digest(nil) },
		"a name not <app>/<VAR>": func(r *record) { r.SecretsAdded = []string{"api"} },
		"a name with a bad VAR":  func(r *record) { r.SecretsChanged = []string{"api/1X"} },
		"out of band not an app": func(r *record) { r.SecretsOutOfBand = []string{"x/y"} },
		"a no_change with env files": func(r *record) {
			r.Phase, r.NewSHA256 = phaseNoChange, r.PrevSHA256
		},
		"a written file listed at another digest": func(r *record) { r.SecretsInstalled["api"] = digest([]byte("other")) },
		"a written file not listed":               func(r *record) { delete(r.SecretsInstalled, "api") },
		"a removed file still listed":             func(r *record) { r.SecretsInstalled["example"] = digest([]byte("old")) },
		"env files listed with no tree":           func(r *record) { r.SecretsTree = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := ok
			r.Secrets = slices.Clone(ok.Secrets)
			r.SecretsInstalled = map[string]string{"api": digest([]byte("a"))}
			edit(&r)
			if r.valid() == nil {
				t.Error("accepted")
			}
		})
	}
}

// applied.json listing env files with no secrets tree is no state a
// write makes: it does not read, and a push is the box's error, never a
// removal of what the list names.
func TestAppliedSecretsNeedATree(t *testing.T) {
	b, _ := newSecretsBox(t)
	b.putEnv("example", rendered(t, "KEY=v"), 0o640)
	a := b.applied()
	a.Secrets = map[string]string{"example": digest(rendered(t, "KEY=v"))}
	b.writeApplied(a)
	if _, err := readApplied(b.x("")); err == nil || !strings.Contains(err.Error(), "env files listed with no secrets_tree") {
		t.Fatalf("%v", err)
	}
	head := b.repo.commitSecrets(b.v1, nil, &b.alice, b.base)
	if r := b.pushOne(b.repo.bundleFiles(head, b.base)); r.Phase != phaseFailed || b.env("example") == nil {
		t.Fatalf("%+v; env %q", r, b.env("example"))
	}
}

// init installs no secrets: applied.json's secrets fields stay as it
// found them, and so does baseline.
func TestInitAndBaselineKeepSecrets(t *testing.T) {
	b, _ := newSecretsBox(t)
	a := b.applied()
	a.SecretsTree, a.Secrets = strings.Repeat("e", 40), map[string]string{"example": digest([]byte("x"))}
	b.writeApplied(a)
	head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	if err := runBaseline(t.Context(), 0, b.applier(hooks{}), head, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := b.applied(); got.SHA != head || got.SecretsTree != a.SecretsTree || got.Secrets["example"] != a.Secrets["example"] {
		t.Errorf("baseline: %+v", got)
	}
}

// The handler reads an env file for the fast path only while it is as
// root wrote it: one whose mode drifted is not unchanged, so a replay
// goes to root, which writes it again.
func TestHandlerEnvDigestNeedsRootsMode(t *testing.T) {
	p := newPushRig(t)
	p.secretsDirAs(0o750 | os.ModeSetgid)
	body := rendered(t, "KEY=v")
	p.putEnv("example", body, 0o640)
	if d, err := p.h.envDigest("example"); err != nil || d != digest(body) {
		t.Fatalf("as root writes it: %q %v", d, err)
	}
	for _, mode := range []os.FileMode{0o644, 0o660, 0o600} {
		if err := os.Chmod(p.envPath("example"), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := p.h.envDigest("example"); err == nil {
			t.Errorf("mode %v answered unchanged", mode)
		}
	}
	if err := os.Chmod(p.envPath("example"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p.envPath("example"), p.envPath("other")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.h.envDigest("other"); err == nil {
		t.Error("a link answered unchanged")
	}
}

// The fast path (step 4) holds the env files too: the secrets tree
// applied.json records, and each listed file at its digest; one that is
// gone or cannot be read is not unchanged, so root reinstalls it.
func TestUnchangedSecrets(t *testing.T) {
	b, id := newSecretsBox(t)
	head := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": encrypt(t, []byte("v"), id.Recipient())}}, &b.alice, b.base)
	files := b.repo.bundleFiles(head, b.base) // HEAD's parent is the base: no parents/ bundled
	bundle, err := proof.ReadBundle(tgz(t, files))
	if err != nil {
		t.Fatal(err)
	}
	want := digest(rendered(t, "KEY=v"))
	base := &applied{SHA: head, Path: testPath, SecretsTree: secretsTreeOf(t, files), Secrets: map[string]string{"example": want}}
	at := func(d string, err error) func(string) (string, error) {
		return func(string) (string, error) { return d, err }
	}
	if !unchanged(bundle, b.v1, base, at(want, nil)) {
		t.Error("the commit and files the box runs: not unchanged")
	}
	if unchanged(bundle, b.v1, base, at(digest([]byte("edited")), nil)) {
		t.Error("an edited env file: unchanged")
	}
	if unchanged(bundle, b.v1, base, at("", fs.ErrNotExist)) {
		t.Error("a lost env file: unchanged")
	}
	other := *base
	other.SecretsTree = strings.Repeat("0", 40)
	if unchanged(bundle, b.v1, &other, at(want, nil)) {
		t.Error("another secrets tree: unchanged")
	}
	// A baseline whose listed apps are not the tree's goes to root.
	for name, list := range map[string]map[string]string{
		"none listed":     {},
		"another listed":  {"other": want},
		"one more listed": {"example": want, "other": want},
	} {
		odd := *base
		odd.Secrets = list
		if unchanged(bundle, b.v1, &odd, at(want, nil)) {
			t.Errorf("%s: unchanged", name)
		}
	}
}

// The box's own slot, secrets/_box/, is installed as an app's secrets
// are, into _box.env, and reloads motel whenever its bytes change:
// notify reads the file at a config load, not at an app's launch. An
// app's secrets alone still reload nothing.
func TestApplyBoxSecrets(t *testing.T) {
	b, id := newSecretsBox(t)
	enc := func(v string) []byte { return encrypt(t, []byte(v), id.Recipient()) }

	c1 := b.repo.commitSecrets(b.v1, secrets{"_box": {"NOTIFY_OPS_URL": enc("https://ntfy.sh/ops-1")}}, &b.alice, b.base)
	r := b.pushOne(b.repo.bundleFiles(c1, b.base))
	want := rendered(t, "NOTIFY_OPS_URL=https://ntfy.sh/ops-1")
	if r.Phase != phaseApplied || !bytes.Equal(b.env("_box"), want) || b.envMode("_box") != 0o640 {
		t.Fatalf("%+v; _box.env %q", r, b.env("_box"))
	}
	if b.sd.reloaded != 1 {
		t.Errorf("%d reloads for the box's own secrets, want 1", b.sd.reloaded)
	}
	if a := b.applied(); a.Secrets["_box"] != digest(want) || !slices.Equal(r.SecretsAdded, []string{"_box/NOTIFY_OPS_URL"}) {
		t.Errorf("applied.json %+v, result %+v", a, r)
	}

	// A rotation reloads again; an app's secrets beside it change nothing
	// of that.
	c2 := b.repo.commitSecrets(b.v1, secrets{"_box": {"NOTIFY_OPS_URL": enc("https://ntfy.sh/ops-2")}, "example": {"KEY": enc("v")}}, &b.alice, c1)
	r = b.pushOne(b.repo.bundleFiles(c2, c1))
	if r.Phase != phaseApplied || b.sd.reloaded != 2 || !slices.Equal(r.SecretsChanged, []string{"_box/NOTIFY_OPS_URL"}) {
		t.Fatalf("%+v; %d reloads", r, b.sd.reloaded)
	}

	// An app's secrets alone: no reload.
	c3 := b.repo.commitSecrets(b.v1, secrets{"_box": {"NOTIFY_OPS_URL": enc("https://ntfy.sh/ops-2")}, "example": {"KEY": enc("w")}}, &b.alice, c2)
	r = b.pushOne(b.repo.bundleFiles(c3, c2))
	if r.Phase != phaseApplied || b.sd.reloaded != 2 {
		t.Fatalf("%+v; %d reloads for an app's secrets alone", r, b.sd.reloaded)
	}

	// The box's file given back its mode alone changes nothing a load
	// reads: rewritten, not reloaded.
	if err := os.Chmod(b.envPath("_box"), 0o600); err != nil {
		t.Fatal(err)
	}
	c4 := b.repo.commitSecrets(b.v1, secrets{"_box": {"NOTIFY_OPS_URL": enc("https://ntfy.sh/ops-2")}, "example": {"KEY": enc("w")}}, &b.alice, c3)
	r = b.pushOne(b.repo.bundleFiles(c4, c3))
	if r.Phase != phaseApplied || b.envMode("_box") != 0o640 || b.sd.reloaded != 2 {
		t.Fatalf("%+v; mode %v; %d reloads", r, b.envMode("_box"), b.sd.reloaded)
	}

	// The slot gone: the file removed, and motel reloaded without it.
	c5 := b.repo.commitSecrets(b.v1, secrets{"example": {"KEY": enc("w")}}, &b.alice, c4)
	r = b.pushOne(b.repo.bundleFiles(c5, c4))
	if r.Phase != phaseApplied || b.env("_box") != nil || b.sd.reloaded != 3 || !slices.Equal(r.SecretsRemoved, []string{"_box/NOTIFY_OPS_URL"}) {
		t.Fatalf("%+v; _box.env %q; %d reloads", r, b.env("_box"), b.sd.reloaded)
	}
}

// A reload that refuses the box's new secrets rolls them back, with the
// Caddyfile it never touched, and reloads the previous file.
func TestApplyBoxSecretsRollBack(t *testing.T) {
	b, id := newSecretsBox(t)
	old := rendered(t, "NOTIFY_OPS_URL=https://ntfy.sh/old")
	b.putEnv("_box", old, 0o640)
	a := b.applied()
	a.SecretsTree, a.Secrets = strings.Repeat("e", 40), map[string]string{"_box": digest(old)}
	b.writeApplied(a)
	head := b.repo.commitSecrets(b.v1, secrets{"_box": {"NOTIFY_OPS_URL": encrypt(t, []byte("https://ntfy.sh/new"), id.Recipient())}}, &b.alice, b.base)
	b.sd.reloads = []error{errReload}
	r := b.pushOne(b.repo.bundleFiles(head, b.base))
	if r.Phase != phaseRolledBack || r.Error != msgReloadFailedBox {
		t.Fatalf("%+v", r)
	}
	if !bytes.Equal(b.env("_box"), old) || !bytes.Equal(b.installed(), b.v1) || b.applied().SHA != b.base || b.sd.reloaded != 2 {
		t.Errorf("not rolled back: _box.env %q, %d reloads", b.env("_box"), b.sd.reloaded)
	}
}
