package box

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/box/proof"
)

// pushOne drops files and runs once; it returns the result.
func (b *testBox) pushOne(files map[string][]byte) *result {
	b.t.Helper()
	id := b.push(files)
	if err := b.run(hooks{}); err != nil {
		b.t.Fatal(err)
	}
	b.settled()
	r := b.result(id)
	if r == nil {
		b.t.Fatal("no result")
	}
	return r
}

// unchanged asserts that nothing was installed and the baseline stayed.
func (b *testBox) unchanged() {
	b.t.Helper()
	if !bytes.Equal(b.installed(), b.v1) || b.applied().SHA != b.base || b.sd.reloaded != 0 {
		b.t.Error("a refused or failed push changed the box")
	}
}

// Steps 10 to 16 as the applier runs them: each refusal by name, in the
// catalogue's words; each box error `failed`, never `refused`.
func TestApplyTrustChain(t *testing.T) {
	type tc struct {
		name  string
		setup func(b *testBox) map[string][]byte
		phase string
		msg   func(b *testBox, files map[string][]byte) string // the exact error; nil: see contains
		has   string                                           // the error contains this
	}
	headOf := func(files map[string][]byte) string { return proof.ObjectID("commit", files["commit"]) }
	cases := []tc{
		{name: "10: the installed file lists no signer", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string { return msgNoSigner },
			setup: func(b *testBox) map[string][]byte {
				b.writeInstalled(boxFile(1))
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			}},
		{name: "10: the installed file is not a box's", phase: phaseFailed, has: "the install failed before the Caddyfile changed: the Caddyfile this box runs has no box block; nothing changed",
			setup: func(b *testBox) map[string][]byte {
				b.writeInstalled([]byte("example.com {\n\trespond hi\n}\n"))
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			}},
		{name: "no baseline", phase: phaseFailed, has: msgNoBaseline,
			setup: func(b *testBox) map[string][]byte {
				if err := os.Remove(b.x("applied.json")); err != nil {
					b.t.Fatal(err)
				}
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			}},
		{name: "11: another box's file", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string {
			return "this file is for other.example.com; this box is deploy.example.com"
		},
			setup: func(b *testBox) map[string][]byte {
				f := bytes.Replace(boxFile(2, b.alice), []byte("deploy.example.com {"), []byte("Other.Example.com {"), 1)
				return b.repo.bundleFiles(b.repo.commit(f, &b.alice, b.base), b.base)
			}},
		{name: "11: another path", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string {
			return "this box's file is box2/Caddyfile; the bundle is box1/Caddyfile — a box's file is deploy.example.com/Caddyfile at the top of the repository: moteld init, run again from that directory, records it"
		},
			setup: func(b *testBox) map[string][]byte {
				a := b.applied()
				a.Path = "box2/Caddyfile"
				b.writeApplied(a)
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			}},
		{name: "11: the bundle's file is out of place", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string {
			return "this box's file is deploy.example.com/Caddyfile; the bundle is box1/Caddyfile — a box's file is deploy.example.com/Caddyfile at the top of the repository: move it there"
		},
			setup: func(b *testBox) map[string][]byte {
				a := b.applied()
				a.Path = "deploy.example.com/Caddyfile"
				b.writeApplied(a)
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			}},
		{name: "11: the new file is not a box's", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string { return "the new Caddyfile has no deploy_trust" },
			setup: func(b *testBox) map[string][]byte {
				f := bytes.Replace(boxFile(2, b.alice), []byte("\t\tdeploy_trust github {\n\t\t\taudience motel\n\t\t\tclaim repository your-org/boxes\n\t\t}\n"), nil, 1)
				return b.repo.bundleFiles(b.repo.commit(f, &b.alice, b.base), b.base)
			}},
		{name: "12: HEAD signed by an unlisted key", phase: phaseRefused, msg: func(b *testBox, f map[string][]byte) string {
			return headOf(f) + " is signed by a key that is not a signer in the Caddyfile this box runs"
		},
			setup: func(b *testBox) map[string][]byte {
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.mallory, b.base), b.base)
			}},
		{name: "12: HEAD altered after signing (the control vector decides)", phase: phaseRefused, msg: func(b *testBox, f map[string][]byte) string {
			return headOf(f) + " is signed by alice@example.com, but the signature does not verify over the commit"
		},
			setup: func(b *testBox) map[string][]byte {
				head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
				files := b.repo.bundleFiles(head, b.base)
				files["commit"] = bytes.Replace(files["commit"], []byte("change "), []byte("chang3 "), 1)
				return files
			}},
		{name: "12: HEAD signed in the file namespace", phase: phaseRefused, has: "is signed in the file namespace, not git",
			setup: func(b *testBox) map[string][]byte {
				head := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
				files := b.repo.bundleFiles(head, b.base)
				c, _ := proof.ParseCommit(files["commit"])
				sig := sshsig(b.t, b.alice, "file", c.Payload)
				files["commit"] = bytes.Replace(files["commit"], foldSig(c.Signature), foldSig([]byte(sig)), 1)
				return files
			}},
		{name: "13: the file sent is not the committed file", phase: phaseRefused, msg: func(b *testBox, f map[string][]byte) string {
			return "the file sent is not box1/Caddyfile in " + headOf(f)
		},
			setup: func(b *testBox) map[string][]byte {
				files := b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
				files["Caddyfile"] = boxFile(3, b.alice)
				return files
			}},
		{name: "14: not a descendant of the baseline", phase: phaseRefused, has: "does not descend from the commit this box runs",
			setup: func(b *testBox) map[string][]byte {
				root := b.repo.commit(boxFile(7, b.alice), &b.alice)
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, root), b.base)
			}},
		{name: "14: an unsigned commit between", phase: phaseRefused, msg: func(b *testBox, f map[string][]byte) string {
			mid := proof.ObjectID("commit", f["parents/0001"])
			return mid + ", between the commit this box runs and " + headOf(f) + ", is not signed; every commit on main must be — rebase it out and force-push; the box still runs " + b.base
		},
			setup: func(b *testBox) map[string][]byte {
				mid := b.repo.commit(boxFile(2, b.alice), nil, b.base)
				return b.repo.bundleFiles(b.repo.commit(boxFile(3, b.alice), &b.alice, mid), b.base)
			}},
		{name: "14: a commit between signed by a key the box did not list", phase: phaseRefused, has: "is signed by a key this box did not list when it last applied (bob)",
			setup: func(b *testBox) map[string][]byte {
				mid := b.repo.commit(boxFile(2, b.alice, b.bob), &b.bob, b.base)
				return b.repo.bundleFiles(b.repo.commit(boxFile(3, b.alice, b.bob), &b.alice, mid), b.base)
			}},
		{name: "14: the chain cap", phase: phaseRefused, has: "is longer than 500 commits",
			setup: func(b *testBox) map[string][]byte {
				c := b.base
				for i := 0; i <= proof.MaxChain; i++ { // HEAD plus 500 above the baseline: one past the cap
					c = b.repo.commit(boxFile(10+i, b.alice), nil, c)
				}
				files := b.repo.bundleFiles(c, b.base)
				delete(files, fmt.Sprintf("parents/%04d", proof.MaxChain)) // a bundle holds 499
				return files
			}},
		{name: "15: the new file drops the key that signed it", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string {
			return "the new Caddyfile drops the key that signed this commit (alice@example.com); add the new key in one push, let it apply, then remove the old one"
		},
			setup: func(b *testBox) map[string][]byte {
				return b.repo.bundleFiles(b.repo.commit(boxFile(2, b.bob), &b.alice, b.base), b.base)
			}},
		{name: "9: not a gzip stream", phase: phaseRefused, msg: func(*testBox, map[string][]byte) string { return "bundle: not a gzip stream" },
			setup: func(*testBox) map[string][]byte { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			files := c.setup(b)
			var id string
			if files == nil {
				id = b.drop([]byte("not a bundle"), b.clock.Now())
			} else {
				id = b.push(files)
			}
			if err := b.run(hooks{}); err != nil {
				t.Fatal(err)
			}
			b.settled()
			r := b.result(id)
			if r == nil || r.Phase != c.phase {
				t.Fatalf("result %+v, want %s", r, c.phase)
			}
			if c.msg != nil && r.Error != c.msg(b, files) {
				t.Errorf("error\n got %q\nwant %q", r.Error, c.msg(b, files))
			}
			if c.has != "" && !strings.Contains(r.Error, c.has) {
				t.Errorf("error %q does not contain %q", r.Error, c.has)
			}
			// Every object id the error names is named, so a poll's
			// answer holds it out of the response filter.
			for _, id := range objectIDs.FindAllString(r.Error, -1) {
				if !slices.Contains(r.Named, id) {
					t.Errorf("error names %s, named = %v", id, r.Named)
				}
			}
			if !bytes.Equal(b.installed(), b.v1) && c.name != "10: the installed file lists no signer" && c.name != "10: the installed file is not a box's" {
				t.Error("installed file changed")
			}
			if b.sd.reloaded != 0 {
				t.Error("reloaded")
			}
		})
	}
}

// objectIDs finds the 40-hex runs in a message.
var objectIDs = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// foldSig is a signature as a gpgsig header carries it: continuation
// lines begin with a space.
func foldSig(sig []byte) []byte {
	return []byte(strings.ReplaceAll(strings.TrimSuffix(string(sig), "\n"), "\n", "\n "))
}

// A bundle over the body cap is refused by name, read no further than
// the cap plus one byte.
func TestApplyBundleTooLarge(t *testing.T) {
	b := newTestBox(t)
	id := b.drop(bytes.Repeat([]byte{0}, maxBody+1), b.clock.Now())
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(id); r == nil || r.Phase != phaseRefused || r.Error != "bundle: larger than 16 MiB" {
		t.Fatalf("%+v", r)
	}
	b.unchanged()
}

// Step 16: `activating` is waited out up to 300 s elapsed; anything but
// active refuses; a question that gets no answer is the box's error.
func TestApplyActive(t *testing.T) {
	for _, c := range []struct {
		name   string
		states []string
		err    error
		phase  string
		msg    string
	}{
		{"activating, then active", []string{"activating", "activating", "active"}, nil, phaseApplied, ""},
		{"activating past 300 s", []string{"activating"}, nil, phaseRefused, msgStillStarting},
		{"inactive", []string{"inactive"}, nil, phaseRefused, msgNotRunning},
		{"failed", []string{"failed"}, nil, phaseRefused, msgNotRunning},
		{"reloading past 300 s", []string{"reloading"}, nil, phaseRefused, msgStillStarting},
		{"deactivating", []string{"deactivating"}, nil, phaseRefused, msgNotRunning},
		{"is-active could not be asked", nil, errors.New("systemctl: not found"), phaseFailed, installFailed(errors.New("systemctl: not found"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			b.sd.states, b.sd.stateErr = c.states, c.err
			start := b.clock.Now()
			r := b.pushOne(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
			if r.Phase != c.phase || r.Error != c.msg {
				t.Fatalf("%s %q, want %s %q", r.Phase, r.Error, c.phase, c.msg)
			}
			if c.msg == msgStillStarting {
				if waited := b.clock.Now().Sub(start); waited < activatingWait || waited > activatingWait+activatingPoll {
					t.Errorf("waited %v", waited)
				}
			}
		})
	}
}

// A verifier that cannot run is the box's error: `failed`, never a
// refusal (step 12).
func TestApplyVerifierCannotRun(t *testing.T) {
	b := newTestBox(t)
	b.verifier = &proof.Verifier{SSHKeygen: "/nonexistent/ssh-keygen"}
	r := b.pushOne(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	if r.Phase != phaseFailed || !strings.HasPrefix(r.Error, "the install failed before the Caddyfile changed: ") {
		t.Fatalf("%+v", r)
	}
	b.unchanged()
}

// Step 10's flag: a console edit since the last apply is reported, not
// refused, and the edited bytes are what a rollback would restore.
func TestApplyOutOfBand(t *testing.T) {
	b := newTestBox(t)
	edited := append(boxFile(1, b.alice), "# a 3am fix\n"...)
	b.writeInstalled(edited)
	b.sd.reloads = []error{errReload}
	r := b.pushOne(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	if !r.OutOfBand || r.Phase != phaseRolledBack {
		t.Fatalf("%+v", r)
	}
	if !bytes.Equal(b.installed(), edited) {
		t.Error("the rollback did not restore the bytes step 10 read")
	}
}

// Rotation is two applied pushes (step 15): add bob, signed by alice;
// then a commit bob signs, listing only bob.
func TestApplyRotation(t *testing.T) {
	b := newTestBox(t)
	c1 := b.repo.commit(boxFile(2, b.alice, b.bob), &b.alice, b.base)
	if r := b.pushOne(b.repo.bundleFiles(c1, b.base)); r.Phase != phaseApplied {
		t.Fatalf("add: %+v", r)
	}
	c2 := b.repo.commit(boxFile(3, b.bob), &b.bob, c1)
	if r := b.pushOne(b.repo.bundleFiles(c2, c1)); r.Phase != phaseApplied || r.Signer != "bob" {
		t.Fatalf("remove: %+v", r)
	}
	if b.applied().SHA != c2 {
		t.Error("baseline")
	}
}

// Every bundle in in/ is taken and settled in one run, in the order of
// its marker's posted time; one with no marker goes last.
func TestApplyOrderByPosted(t *testing.T) {
	b := newTestBox(t)
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	c2 := b.repo.commit(boxFile(3, b.alice), &b.alice, c1)
	now := b.clock.Now()
	// c2 posted later than c1, c1's bundle carries only c1; an unmarked
	// third bundle (a replay of c1) goes last: by then HEAD is the baseline.
	late := b.pushAt(b.repo.bundleFiles(c2, b.base), now.Add(-time.Minute))
	early := b.pushAt(b.repo.bundleFiles(c1, b.base), now.Add(-2*time.Minute))
	unmarked := b.push(b.repo.bundleFiles(c1, b.base))
	if err := os.Remove(b.x("stage/" + unmarked + ".auth")); err != nil {
		t.Fatal(err)
	}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(early); r.Phase != phaseApplied {
		t.Errorf("early: %+v", r)
	}
	if r := b.result(late); r.Phase != phaseRefused || !strings.Contains(r.Error, "parents/0001 is past the end of the chain") {
		t.Errorf("late (its bundle was cut for the old baseline): %+v", r)
	}
	if r := b.result(unmarked); r.Phase != phaseNoChange {
		t.Errorf("unmarked: %+v", r)
	}
}
