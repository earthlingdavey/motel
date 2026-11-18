package box

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

// baseline is a trust reset (DESIGN-box.md, "`box baseline`"): sha
// from the argument, path and sha256 kept, signer "baseline", when now;
// one atomic write under root's lock, after recovery, with no record.
func TestBaseline(t *testing.T) {
	b := newTestBox(t)
	b.clock.advance(time.Hour)
	before := b.applied()
	sha := b.repo.commit(boxFile(7, b.alice), &b.alice) // a rewritten history: no parent
	var out bytes.Buffer
	if err := runBaseline(context.Background(), 0, b.applier(hooks{}), sha, &out); err != nil {
		t.Fatal(err)
	}
	b.settled()
	reset := func(a applied) bool {
		return a.SHA == sha && a.Path == before.Path && a.SHA256 == before.SHA256 && a.Signer == "baseline" && a.When.Equal(b.clock.Now())
	}
	if got := b.applied(); !reset(got) {
		t.Errorf("applied.json %+v, was %+v", got, before)
	}
	if wantOut := "baseline: this box ran " + before.SHA[:7] + "; it now applies commits descending from " + sha[:7] + "\n"; out.String() != wantOut {
		t.Errorf("printed %q, want %q", out.String(), wantOut)
	}
	if !bytes.Equal(b.installed(), b.v1) || b.sd.reloaded != 0 {
		t.Error("baseline touched the Caddyfile or reloaded")
	}

	// Run again: nothing to reset, nothing written.
	out.Reset()
	b.clock.advance(time.Hour)
	if err := runBaseline(context.Background(), 0, b.applier(hooks{}), sha, &out); err != nil {
		t.Fatal(err)
	}
	if again := b.applied(); again.SHA != sha || again.When.Equal(b.clock.Now()) {
		t.Errorf("a second run rewrote it: %+v", again)
	}
	if want := "baseline: this box already runs " + sha[:7] + "; nothing changed\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

// Refused before it touches anything: not root, a bad sha, and no
// baseline to reset — init is the only way in.
func TestBaselineRefusals(t *testing.T) {
	b := newTestBox(t)
	sha := b.repo.commit(boxFile(7, b.alice), &b.alice)
	noBaseline := newTestBox(t)
	if err := os.Remove(noBaseline.x("applied.json")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		box  *testBox
		euid int
		sha  string
		want string
	}{
		{"not root", b, 1000, sha, "moteld box baseline runs as root: it rewrites the box's baseline"},
		{"short", b, 0, sha[:12], sha[:12] + " is not a commit id: 40 lowercase hex, as git rev-parse HEAD prints one"},
		{"upper case", b, 0, strings.ToUpper(sha), strings.ToUpper(sha) + " is not a commit id"},
		{"not hex", b, 0, strings.Repeat("g", 40), "is not a commit id"},
		{"a sha-256 id", b, 0, strings.Repeat("a", 64), "is not a commit id"},
		{"a control byte, quoted", b, 0, "x\ny", `"x\ny" is not a commit id`},
		{"before init", noBaseline, 0, sha, msgNoBaseline},
	} {
		t.Run(c.name, func(t *testing.T) {
			snap := c.box.snapshot()
			var out bytes.Buffer
			err := runBaseline(context.Background(), c.euid, c.box.applier(hooks{}), c.sha, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if out.Len() != 0 {
				t.Errorf("printed %q", out.String())
			}
			if !maps.Equal(snap, c.box.snapshot()) {
				t.Error("a refused baseline changed the tree")
			}
		})
	}
}

// Keeping sha256 keeps step 10's flag honest: a console edit made
// before the reset is still out of band at the next push (D1).
func TestBaselineKeepsTheEditOutOfBand(t *testing.T) {
	b := newTestBox(t)
	edited := append(append([]byte{}, b.v1...), "# 3am\n"...)
	b.writeInstalled(edited)
	base := b.repo.commit(boxFile(7, b.alice), &b.alice)
	if err := runBaseline(context.Background(), 0, b.applier(hooks{}), base, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	head := b.repo.commit(boxFile(8, b.alice), &b.alice, base)
	r := b.pushOne(b.repo.bundleFiles(head, base))
	if r.Phase != phaseApplied || !r.OutOfBand {
		t.Fatalf("%+v: the edit made before the reset is not out of band", r)
	}
}

// A record a crash left is settled before the baseline is written, so
// the baseline never runs ahead of it (I4).
func TestBaselineRecoversFirst(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	head := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	crashed := randomID(t)
	b.lyingRecord(record{ID: crashed, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	sha := b.repo.commit(boxFile(7, b.alice), &b.alice)
	if err := runBaseline(context.Background(), 0, b.applier(hooks{}), sha, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(crashed); r == nil || r.Phase != phaseRolledBack {
		t.Errorf("the crashed push: %+v", r)
	}
	if a := b.applied(); a.SHA != sha || a.SHA256 != digest(b.v1) || !bytes.Equal(b.installed(), b.v1) {
		t.Errorf("applied.json %+v", a)
	}
}

// A record recovery cannot read stops baseline with nothing written.
func TestBaselineUnderAnUnreadableRecord(t *testing.T) {
	b := newTestBox(t)
	if err := os.WriteFile(b.x("txn.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := b.snapshot()
	sha := b.repo.commit(boxFile(7, b.alice), &b.alice)
	err := runBaseline(context.Background(), 0, b.applier(hooks{}), sha, &bytes.Buffer{})
	if !errors.Is(err, errUnsettled) {
		t.Fatalf("%v", err)
	}
	if !maps.Equal(snap, b.snapshot()) {
		t.Error("baseline wrote under a record it could not read")
	}
}

// baseline waits for root's lock: while an applier run holds it, the
// baseline is not written.
func TestBaselineWaitsForTheLock(t *testing.T) {
	b := newTestBox(t)
	unlock, err := b.applier(hooks{}).lock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock) // a second close is harmless
	sha := b.repo.commit(boxFile(7, b.alice), &b.alice)
	done := make(chan error, 1)
	go func() { done <- runBaseline(context.Background(), 0, b.applier(hooks{}), sha, &bytes.Buffer{}) }()
	select {
	case err := <-done:
		t.Fatalf("baseline ran while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	moved := b.applied().SHA != b.base
	unlock()
	err = <-done // drained before any failure: it writes in the test's tree
	if moved {
		t.Fatal("the baseline moved while the lock was held")
	}
	if err != nil {
		t.Fatal(err)
	}
	if b.applied().SHA != sha {
		t.Error("the baseline was not written once the lock was free")
	}
}
