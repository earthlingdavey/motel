package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// The origin: init rows of the Transitions and States tables, through
// runInit, the entry `moteld init` calls: no result files, and on a
// moteld that is not up once the wait is over, a refusal that writes
// nothing (the reload is the check; DESIGN-box.md, "`moteld init`").
func TestInitOrigin(t *testing.T) {
	initRec := func(b *testBox, file []byte, sha string) record {
		shape, err := Walk(file)
		if err != nil {
			b.t.Fatal(err)
		}
		return record{ID: randomID(b.t), Commit: sha, Path: testPath, BoxWebhook: shape.Host, Apps: shape.Apps}
	}
	for _, c := range []struct {
		name     string
		same     bool
		states   []string
		reloads  []error
		phase    string // the outcome; "" for the refusal
		refusal  string
		file     int
		advanced bool
		reloaded int
		asked    int // 0: not counted (a wait that runs out asks every second)
	}{
		{name: "no_change on an active box", same: true, states: []string{"active"}, phase: phaseNoChange, file: 1, advanced: true, asked: 1},
		{name: "no_change on a box that is not running: refused", same: true, states: []string{"inactive"}, refusal: msgNotRunning, file: 1, asked: 1},
		{name: "not running: refused, nothing written", states: []string{"inactive"}, refusal: msgNotRunning, file: 1, asked: 1},
		{name: "failed: refused", states: []string{"failed"}, refusal: msgNotRunning, file: 1, asked: 1},
		{name: "still activating after the wait: refused", states: []string{"activating"}, refusal: msgStillStarting, file: 1},
		{name: "activating, then active within the wait: applied", states: []string{"activating", "activating", "active"}, phase: phaseApplied, file: 2, advanced: true, reloaded: 1, asked: 3},
		{name: "active: reloaded", states: []string{"active"}, phase: phaseApplied, file: 2, advanced: true, reloaded: 1, asked: 1},
		{name: "active, reload fails: rolled back", states: []string{"active"}, reloads: []error{errReload}, phase: phaseRolledBack, file: 1, reloaded: 2, asked: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBox(t)
			b.sd.states, b.sd.reloads = c.states, c.reloads
			file := boxFile(2, b.alice)
			if c.same {
				file = b.v1
			}
			sha := b.repo.commit(file, &b.alice, b.base)
			before := b.snapshot()
			var points []string
			h := hooks{fail: func(p string) error { points = append(points, p); return nil }}
			out, err := b.applier(h).runInit(context.Background(), initRec(b, file, sha), file)
			if c.refusal != "" {
				var nu *notUpError
				if !errors.As(err, &nu) || nu.Error() != c.refusal {
					t.Fatalf("%v, want the refusal %q", err, c.refusal)
				}
				if len(points) != 0 {
					t.Errorf("a refused init reached writes: %v", points)
				}
				if after := b.snapshot(); !maps.Equal(before, after) {
					t.Error("a refused init changed the tree")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if out.Phase != c.phase || out.Signer != "init" {
					t.Errorf("outcome %+v, want %s by init", out.result, c.phase)
				}
			}
			b.settled()
			want := b.v1
			if c.file == 2 {
				want = file
			}
			if !bytes.Equal(b.installed(), want) {
				t.Errorf("installed is not version %d", c.file)
			}
			a := b.applied()
			if (a.SHA == sha) != c.advanced || (c.advanced && a.Signer != "init") {
				t.Errorf("applied.json %+v", a)
			}
			if n := b.names(b.x("out")); len(n) != 0 {
				t.Errorf("init wrote results: %v", n)
			}
			// init asks once, when it begins, whatever follows: rollback
			// included. A wait asks again until it ends.
			if c.asked != 0 && b.sd.asked != c.asked {
				t.Errorf("%d is-active questions, want %d", b.sd.asked, c.asked)
			}
			if b.sd.reloaded != c.reloaded {
				t.Errorf("%d reloads, want %d", b.sd.reloaded, c.reloaded)
			}
		})
	}
}

// init takes root's lock and settles a record a crash left before it
// starts its own transaction.
func TestInitRecoversFirst(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	head := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	crashedID := randomID(t)
	b.lyingRecord(record{ID: crashedID, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	v3 := boxFile(3, b.alice)
	sha := b.repo.commit(v3, &b.alice, b.base)
	out, err := b.applier(hooks{}).runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v3)
	if err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(crashedID); r == nil || r.Phase != phaseRolledBack {
		t.Errorf("the crashed push: %+v", r)
	}
	if out.Phase != phaseApplied || !bytes.Equal(b.installed(), v3) {
		t.Errorf("init: %+v", out.result)
	}
}

// snapshot is every entry under the box's root by its path, with its
// mode and, for a file, its contents' digest — all but root's lock,
// which the first lock() creates and which holds nothing.
func (b *testBox) snapshot() map[string]string {
	b.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(b.root, path)
		if err != nil {
			return err
		}
		if rel == filepath.Join(exchangeDir[1:], "lock") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			v += " " + hex.EncodeToString(sum[:])
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return out
}
