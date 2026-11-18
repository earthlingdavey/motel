package box

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// initDir is the box's directory as the operator copies it over: file
// as its Caddyfile, in a directory named for its host (or "box" for a
// file that walks as no box's).
func initDir(t testing.TB, file []byte) string {
	t.Helper()
	name := "box"
	if shape, err := Walk(file); err == nil {
		name = shape.Host
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// initCmd runs `moteld init <sha>` as root against the box, from dir.
func (b *testBox) initCmd(dir, sha string) (string, error) {
	b.t.Helper()
	var out bytes.Buffer
	err := runInitCmd(context.Background(), 0, b.applier(hooks{}), dir, sha, &out)
	return out.String(), err
}

const fromHere = "init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow.\n"

// keyLine is init's last line on a green end: the box's key for
// secrets, as age.pub holds it.
func keyLine(b *testBox) string {
	b.t.Helper()
	pub, err := os.ReadFile(b.path(agePubFile))
	if err != nil {
		b.t.Fatalf("init made no age.pub: %v", err)
	}
	return "init: secrets for this box encrypt to " + strings.TrimSuffix(string(pub), "\n") + "; moteld box key prints it again\n"
}

// summary is init's first two lines for a boxFile on host.
func summary(host string) string {
	return "init: box_webhook on " + host + "; 1 app (example); 1 signer (alice@example.com)\n" +
		"init: this box's file is " + host + "/Caddyfile in the repository\n"
}

// What `moteld init` prints for each way a run can end, and what it
// leaves on disk (DESIGN-box.md, "`moteld init`").
func TestInitCmd(t *testing.T) {
	onHost := func(file []byte, host string) []byte {
		return bytes.Replace(file, []byte("deploy.example.com {"), []byte(host+" {"), 1)
	}
	type check struct {
		b    *testBox
		file []byte
		sha  string
	}
	for _, c := range []struct {
		name string
		// setup prepares the box and returns the directory's file and
		// the commit init is given.
		setup func(b *testBox) ([]byte, string)
		want  func(c check) string
		err   string
		// path and digest are applied.json's afterwards: "" for the
		// baseline as newTestBox wrote it.
		path   string
		reload int
	}{
		{
			name: "a fresh box",
			setup: func(b *testBox) ([]byte, string) {
				b.writeInstalled([]byte("example.com {\n\trespond \"hello\"\n}\n")) // the packaged file walks as no box's
				if err := os.Remove(b.x("applied.json")); err != nil {
					b.t.Fatal(err)
				}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/motel/Caddyfile\n" +
					"init: this box applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a re-init over an earlier baseline",
			setup: func(b *testBox) ([]byte, string) {
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/motel/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a re-init naming another host: warned, not refused",
			setup: func(b *testBox) ([]byte, string) {
				file := onHost(boxFile(2, b.alice), "other.example.com")
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("other.example.com") + "init: this box was deploy.example.com; this file names other.example.com\n" +
					"init: installed /etc/motel/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "other.example.com/Caddyfile", reload: 1,
		},
		{
			name: "a mixed-case host is recorded in lower case",
			setup: func(b *testBox) ([]byte, string) {
				file := onHost(boxFile(2, b.alice), "Deploy.Example.COM")
				return file, b.repo.commit(file, &b.alice)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/motel/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
		{
			name:  "no_change at the baseline",
			setup: func(b *testBox) ([]byte, string) { return b.v1, b.base },
			want: func(c check) string {
				return summary("deploy.example.com") + "init: unchanged; the baseline is now " + c.sha[:7] + "\n"
			},
			path: "deploy.example.com/Caddyfile",
		},
		{
			name: "no_change over another baseline",
			setup: func(b *testBox) ([]byte, string) {
				return b.v1, b.repo.commit(b.v1, &b.alice, b.base)
			},
			want: func(c check) string {
				return summary("deploy.example.com") +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" +
					"init: unchanged; the baseline is now " + c.sha[:7] + "\n"
			},
			path: "deploy.example.com/Caddyfile",
		},
		{
			name: "a reload that fails: rolled back, non-zero",
			setup: func(b *testBox) ([]byte, string) {
				b.sd.reloads = []error{errReload}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice, b.base)
			},
			want:   func(check) string { return summary("deploy.example.com") },
			err:    phaseRolledBack + ": " + msgReloadFailed,
			reload: 2,
		},
		{
			name: "activating, then active within the wait: applied",
			setup: func(b *testBox) ([]byte, string) {
				b.sd.states = []string{"activating", "activating", "active"}
				file := boxFile(2, b.alice)
				return file, b.repo.commit(file, &b.alice, b.base)
			},
			want: func(c check) string {
				return summary("deploy.example.com") + "init: installed /etc/motel/Caddyfile\n" +
					"init: this box ran " + c.b.base[:7] + "; it now applies commits descending from " + c.sha[:7] + "\n" + fromHere
			},
			path: "deploy.example.com/Caddyfile", reload: 1,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			was := b.applied()
			file, sha := c.setup(b)
			out, err := b.initCmd(initDir(t, file), sha)
			switch {
			case c.err == "" && err != nil:
				t.Fatal(err)
			case c.err != "" && (err == nil || err.Error() != c.err):
				t.Fatalf("%v, want %q", err, c.err)
			}
			want := c.want(check{b: b, file: file, sha: sha})
			if c.err == "" { // a green end makes the box's key last
				want += keyLine(b)
			}
			if out != want {
				t.Errorf("printed\n%s\nwant\n%s", out, want)
			}
			b.settled()
			if n := b.names(b.x("out")); len(n) != 0 {
				t.Errorf("init wrote results: %v", n)
			}
			if b.sd.reloaded != c.reload {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, c.reload)
			}
			a := b.applied()
			if c.path == "" {
				if !reflect.DeepEqual(a, was) || !bytes.Equal(b.installed(), b.v1) {
					t.Errorf("a red init changed the box: %+v", a)
				}
				return
			}
			if a.SHA != sha || a.Path != c.path || a.SHA256 != digest(file) || a.Signer != "init" || !bytes.Equal(b.installed(), file) {
				t.Errorf("applied.json %+v", a)
			}
		})
	}
}

// init takes root's lock and settles a record a crash left before its
// own transaction, as the applier would have.
func TestInitCmdRecoversFirst(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	b.writeInstalled(v2)
	crashed := randomID(t)
	b.lyingRecord(record{ID: crashed, Origin: originApplier, Phase: phaseSwapped, Commit: b.repo.commit(v2, &b.alice, b.base), Path: testPath,
		Signer: "alice@example.com", Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	v3 := boxFile(3, b.alice)
	sha := b.repo.commit(v3, &b.alice, b.base)
	if _, err := b.initCmd(initDir(t, v3), sha); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(crashed); r == nil || r.Phase != phaseRolledBack {
		t.Errorf("the crashed push: %+v", r)
	}
	if !bytes.Equal(b.installed(), v3) || b.applied().SHA != sha {
		t.Error("init did not apply after recovery")
	}
}

// Every refusal is by name, and writes nothing: refusals before the lock
// print nothing either; a moteld that is not up refuses after the
// summary, with the remedy filled in (DESIGN-box.md, "Console
// commands").
func TestInitCmdRefusals(t *testing.T) {
	edit := func(old, new string) func(*testBox) []byte {
		return func(b *testBox) []byte {
			if !strings.Contains(good, old) {
				b.t.Fatalf("the fixture has no %q", old)
			}
			return file(strings.Replace(good, old, new, 1))
		}
	}
	whole := func(b *testBox) []byte { return b.v1 }
	big := func(*testBox) []byte { return bytes.Repeat([]byte("#"), 1<<20+1) }
	const signers = "\t\tsigner alice@example.com ssh-ed25519 K1\n\t\tsigner bob ssh-ed25519 K2\n"
	const trust = "\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/boxes\n\t\t}\n"
	const boxBlock = "\tbox {\n" + trust + "\t\t# signers\n" + signers + "\t}\n"
	for _, c := range []struct {
		name   string
		euid   int
		file   func(b *testBox) []byte // nil: no Caddyfile in the directory
		dir    func(t *testing.T, b *testBox) string
		sha    string // "": a valid commit
		states []string
		want   string
		remedy bool
	}{
		{name: "not root", euid: 1000, file: whole, want: "moteld init runs as root: it installs /etc/motel/Caddyfile"},
		{name: "a short sha", file: whole, sha: "4f1c2a9", want: "4f1c2a9 is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one"},
		{name: "an upper-case sha", file: whole, sha: strings.Repeat("A", 40), want: strings.Repeat("A", 40) + " is not a commit id"},
		{name: "a sha that is not hex", file: whole, sha: strings.Repeat("z", 40), want: "is not a commit id"},
		{name: "no Caddyfile here", want: "no Caddyfile here; run init from inside the box's directory"},
		{name: "a Caddyfile over the cap", file: big, want: "Caddyfile: larger than 1048576 bytes"},
		{name: "a directory named Caddyfile", dir: func(t *testing.T, _ *testBox) string {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, "Caddyfile"), 0o755); err != nil {
				t.Fatal(err)
			}
			return d
		}, want: "Caddyfile: not a regular file"},
		{name: "a FIFO named Caddyfile", dir: func(t *testing.T, _ *testBox) string {
			d := t.TempDir()
			mkfifo(t, filepath.Join(d, "Caddyfile"))
			return d
		}, want: "Caddyfile: not a regular file"},
		{name: "a directory not named for the host", dir: func(t *testing.T, b *testBox) string {
			d := filepath.Join(t.TempDir(), "box1")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "Caddyfile"), b.v1, 0o600); err != nil {
				t.Fatal(err)
			}
			return d
		}, want: "this directory is box1, and the Caddyfile is for deploy.example.com: a box's directory is named for its host, so run init from deploy.example.com/ in a copy of the repository"},
		{name: "a directory named for the host in another case", dir: func(t *testing.T, b *testBox) string {
			d := filepath.Join(t.TempDir(), "Deploy.Example.com")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "Caddyfile"), b.v1, 0o600); err != nil {
				t.Fatal(err)
			}
			return d
		}, want: "this directory is Deploy.Example.com, and the Caddyfile is for deploy.example.com"},
		{name: "a link named for the host, to a directory that is not", dir: func(t *testing.T, b *testBox) string {
			parent := t.TempDir()
			d := filepath.Join(parent, "box1")
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "Caddyfile"), b.v1, 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(parent, "deploy.example.com")
			if err := os.Symlink(d, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, want: "this directory is box1, and the Caddyfile is for deploy.example.com"},
		{name: "no box block", file: edit(boxBlock, ""), want: "the Caddyfile has no box block"},
		{name: "no signer", file: edit(signers, ""), want: "the Caddyfile has no signer"},
		{name: "no deploy_trust", file: edit(trust, ""), want: "the Caddyfile has no deploy_trust"},
		{name: "no box_webhook", file: edit("\tbox_webhook\n", ""), want: "the Caddyfile has no site with box_webhook"},
		{name: "two box_webhook sites", file: edit("deploy.example.com {", "other.example.com {\n\tbox_webhook\n}\n\ndeploy.example.com {"), want: "the Caddyfile has more than one site with box_webhook"},
		{name: "a wildcard host", file: edit("deploy.example.com {", "*.example.com {"), want: "the Caddyfile has a box_webhook site whose address is not one bare hostname (*.example.com)"},
		{name: "a placeholder host", file: edit("deploy.example.com {", "{$HOST} {"), want: "the Caddyfile has a placeholder where a directive name, a site address or a box line goes ({$HOST})"},
		{name: "no host", file: edit("deploy.example.com {", ":443 {"), want: "the Caddyfile has a box_webhook site whose address is not one bare hostname (:443)"},
		{name: "an import in the box block", file: edit("\t\t# signers\n", "\t\timport signers\n"), want: "the Caddyfile imports signers; inline the snippet"},
		{name: "an import in the site", file: edit("\tbox_webhook\n", "\tbox_webhook\n\timport extra\n"), want: "the Caddyfile imports extra; inline the snippet"},
		{name: "moteld inactive", file: whole, states: []string{"inactive"}, want: msgNotRunning, remedy: true},
		{name: "moteld failed", file: whole, states: []string{"failed"}, want: msgNotRunning, remedy: true},
		{name: "moteld still activating after the wait", file: whole, states: []string{"activating"}, want: msgStillStarting, remedy: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			b.sd.states = c.states
			sha := c.sha
			if sha == "" {
				sha = b.repo.commit(b.v1, &b.alice, b.base)
			}
			var dir string
			switch {
			case c.dir != nil:
				dir = c.dir(t, b)
			case c.file != nil:
				dir = initDir(t, c.file(b))
			default:
				dir = t.TempDir()
			}
			snap := b.snapshot()
			var out bytes.Buffer
			err := runInitCmd(context.Background(), c.euid, b.applier(hooks{}), dir, sha, &out)
			want := c.want
			if c.remedy {
				want += "\n" + fmt.Sprintf(msgInitRemedy, sha, shellWord(dir))
				if err == nil || err.Error() != want {
					t.Fatalf("%v\nwant\n%s", err, want)
				}
				if s := out.String(); s != summary("deploy.example.com") {
					t.Errorf("printed %q", s)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%v, want %q", err, want)
				}
				if out.Len() != 0 {
					t.Errorf("printed %q before a refusal", out.String())
				}
			}
			if !maps.Equal(snap, b.snapshot()) {
				t.Error("a refused init changed the box")
			}
			if b.sd.reloaded != 0 {
				t.Error("a refused init reloaded")
			}
		})
	}
}

// An apply whose baseline cannot be recorded ends `unknown` with a
// remedy that works: init again for init, and `box baseline` for the
// applier, since a push again is refused after a signer rotation.
func TestBaselineUnrecordedRemedies(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		b := newTestBox(t)
		file := boxFile(2, b.alice)
		sha := b.repo.commit(file, &b.alice, b.base)
		dir := initDir(t, file)
		var out bytes.Buffer
		err := runInitCmd(context.Background(), 0, b.applier(hooks{fail: failAt(map[string]int{"applied.json": -1})}), dir, sha, &out)
		want := phaseUnknown + ": applied but the baseline could not be recorded; moteld init " + sha + " again, which ends no_change and records it"
		if err == nil || err.Error() != want {
			t.Fatalf("%v, want %q", err, want)
		}
		if out, err := b.initCmd(dir, sha); err != nil || !strings.HasSuffix(out, "init: unchanged; the baseline is now "+sha[:7]+"\n"+keyLine(b)) {
			t.Fatalf("init again: %q, %v", out, err)
		}
		if a := b.applied(); a.SHA != sha || a.SHA256 != digest(file) {
			t.Errorf("applied.json %+v", a)
		}
	})
	// The applier's no_change: nothing was installed, so the same push
	// again is verified as this one was, and records it.
	t.Run("applier, no_change", func(t *testing.T) {
		b := newTestBox(t)
		head := b.repo.commit(b.v1, &b.alice, b.base)
		id := b.push(b.repo.bundleFiles(head, b.base))
		if err := b.run(hooks{fail: failAt(map[string]int{"applied.json": -1})}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(id); r == nil || r.Phase != phaseUnknown || r.Error != "unchanged, but the baseline could not be recorded; push again, which records it" {
			t.Fatalf("%+v", r)
		}
		if r := b.pushOne(b.repo.bundleFiles(head, b.base)); r.Phase != phaseNoChange || r.OutOfBand || b.applied().SHA != head {
			t.Fatalf("push again: %+v", r)
		}
	})
	// The applier's: a chain that rotated a signer on the way (alice
	// signs c7, bob signs c8, which drops alice). Pushing it again is
	// refused — c7 is verified against the file now installed — so the
	// remedy is `box baseline`, after which the next push applies.
	t.Run("applier", func(t *testing.T) {
		b := newTestBox(t)
		c6 := b.repo.commit(boxFile(2, b.alice, b.bob), &b.alice, b.base)
		if r := b.pushOne(b.repo.bundleFiles(c6, b.base)); r.Phase != phaseApplied {
			t.Fatalf("c6: %+v", r)
		}
		c7 := b.repo.commit(boxFile(3, b.alice, b.bob), &b.alice, c6)
		c8 := b.repo.commit(boxFile(4, b.bob), &b.bob, c7)
		id := b.push(b.repo.bundleFiles(c8, c6))
		if err := b.run(hooks{fail: failAt(map[string]int{"applied.json": -1})}); err != nil {
			t.Fatal(err)
		}
		want := "applied but the baseline could not be recorded; moteld box baseline " + c8 + " on the box records it (the next push then reports the file as edited out of band; it was not)"
		if r := b.result(id); r == nil || r.Phase != phaseUnknown || r.Error != want {
			t.Fatalf("%+v", r)
		}
		if r := b.pushOne(b.repo.bundleFiles(c8, c6)); r.Phase != phaseRefused {
			t.Fatalf("pushing the rotated chain again: %+v (why the remedy is not a push)", r)
		}
		if err := runBaseline(context.Background(), 0, b.applier(hooks{}), c8, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		c9 := b.repo.commit(boxFile(5, b.bob), &b.bob, c8)
		if r := b.pushOne(b.repo.bundleFiles(c9, c8)); r.Phase != phaseApplied || !r.OutOfBand || b.applied().SHA != c9 {
			t.Fatalf("the next push: %+v", r)
		}
	})
}

// init says so while it waits for root's lock, then goes on.
func TestInitSaysItWaitsForTheLock(t *testing.T) {
	b := newTestBox(t)
	unlock, err := b.applier(hooks{}).lock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock) // a second close is harmless
	file := boxFile(2, b.alice)
	sha := b.repo.commit(file, &b.alice, b.base)
	a := b.applier(hooks{})
	waiting := make(chan struct{})
	var out bytes.Buffer
	dir := initDir(t, file)
	done := make(chan error, 1)
	go func() {
		w := &notifyingWriter{w: &out, notify: waiting, on: "waiting for root's lock"}
		done <- runInitCmd(context.Background(), 0, a, dir, sha, w)
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("init ran while the lock was held: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("init said nothing while it waited")
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "init: waiting for root's lock: the applier, or another init or baseline, holds it\n") || !strings.HasSuffix(out.String(), fromHere+keyLine(b)) {
		t.Errorf("printed %q", out.String())
	}
}

// notifyingWriter closes notify the first time a write contains on.
type notifyingWriter struct {
	w      *bytes.Buffer
	notify chan struct{}
	on     string
	done   bool
}

func (n *notifyingWriter) Write(p []byte) (int, error) {
	if !n.done && strings.Contains(string(p), n.on) {
		n.done = true
		close(n.notify)
	}
	return n.w.Write(p)
}

// "applied" means these bytes: a console edit landing after init's
// reload and before its baseline leaves the baseline where it was (I1,
// I4), init's own record as the applier's.
func TestInitRechecksTheFileBeforeTheBaseline(t *testing.T) {
	b := newTestBox(t)
	file := boxFile(2, b.alice)
	sha := b.repo.commit(file, &b.alice, b.base)
	edited := append(append([]byte{}, file...), "# 3am\n"...)
	edit := func(p string) {
		if p == "record:applied" {
			b.writeInstalled(edited)
		}
	}
	var out bytes.Buffer
	err := runInitCmd(context.Background(), 0, b.applier(hooks{crash: edit}), initDir(t, file), sha, &out)
	if err == nil || err.Error() != phaseUnknown+": "+msgChanged {
		t.Fatalf("%v", err)
	}
	b.settled()
	if b.applied().SHA != b.base || !bytes.Equal(b.installed(), edited) {
		t.Error("the baseline advanced past a console edit, or the edit was overwritten")
	}
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"/root/deploy.example.com": "/root/deploy.example.com",
		"/root/my box":             "'/root/my box'",
		"/root/it's":               `'/root/it'\''s'`,
		"/tmp/a_b-c+d@e":           "/tmp/a_b-c+d@e",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// The host warning describes the file init replaces, as recovery left
// it: an init to another host that crashed after its swap is rolled
// back first, so its re-run warns about the rename it is about to make.
func TestInitHostWarningAfterRecovery(t *testing.T) {
	b := newTestBox(t)
	other := bytes.Replace(boxFile(2, b.alice), []byte("deploy.example.com {"), []byte("other.example.com {"), 1)
	sha := b.repo.commit(other, &b.alice, b.base)
	b.writeInstalled(other) // the crashed init's swap
	b.lyingRecord(record{ID: randomID(t), Origin: originInit, Phase: phaseSwapped, Commit: sha, Path: "other.example.com/Caddyfile", Signer: "init",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(other)})
	out, err := b.initCmd(initDir(t, other), sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "init: this box was deploy.example.com; this file names other.example.com\n") {
		t.Errorf("no rename warning after recovery rolled the swap back:\n%s", out)
	}
	b.settled()
	if !bytes.Equal(b.installed(), other) || b.applied().SHA != sha {
		t.Error("init did not apply after recovery")
	}
}
