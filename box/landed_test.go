package box

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/smallhoursorg/motel/box/proof"
)

// A write can land — its rename done — and still fail, at the
// directory's fsync. These hooks do the write's visible half themselves
// and then fail, so what is on disk, not the error, decides.
func TestWritesThatLandedThenFailed(t *testing.T) {
	errSync := errors.New("fsync: input/output error")

	t.Run("the swap landed: the post-swap rollback", func(t *testing.T) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		id := b.push(b.repo.bundleFiles(b.repo.commit(v2, &b.alice, b.base), b.base))
		fail := func(p string) error {
			if p == "caddyfile:new" {
				b.writeInstalled(v2)
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r.Phase != phaseRolledBack || !bytes.Equal(b.installed(), b.v1) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("the swap failed with a third file on disk: left as found", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		other := boxFile(9, b.alice)
		fail := func(p string) error {
			if p == "caddyfile:new" {
				b.writeInstalled(other)
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r.Phase != phaseUnknown || r.Error != msgChanged || !bytes.Equal(b.installed(), other) {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("verified landed: failed written over it", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		fail := func(p string) error {
			if p == "result:verified" {
				if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified}), 0o640); err != nil {
					t.Fatal(err)
				}
				return errSync
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseFailed || !strings.Contains(r.Error, "nothing changed") {
			t.Fatalf("a verified with nothing behind it was not settled: %+v", r)
		}
	})
	t.Run("verified landed and failed cannot be written: removed, Retention settles it", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		fail := func(p string) error {
			switch p {
			case "result:verified":
				if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified}), 0o640); err != nil {
					t.Fatal(err)
				}
				return errSync
			case "result:failed":
				return errNoSpace
			}
			return nil
		}
		if err := b.run(hooks{fail: fail}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r != nil {
			t.Fatalf("a verified with nothing behind it stayed: %+v", r)
		}
		b.clock.advance(pendingAge + time.Minute)
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if r := b.result(id); r == nil || r.Phase != phaseFailed || r.Error != msgNoRecord {
			t.Fatalf("Retention: %+v", r)
		}
	})
	t.Run("take landed, its fsync failing: taken, with an error line", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		if err := b.run(hooks{fail: failAt(map[string]int{"take:sync": -1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseApplied {
			t.Fatalf("%+v", r)
		}
		if !b.errorLogged("not known durable") {
			t.Error("no error-level line for the undurable take")
		}
	})
	t.Run("a settled push that comes back in in/ is removed, never run again", func(t *testing.T) {
		b := newTestBox(t)
		files := b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
		id := b.push(files)
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		// A power loss before in/'s unlink was durable: the entry is back.
		if err := os.WriteFile(filepath.Join(b.x("in"), id+".tar"), tgz(t, files), 0o644); err != nil {
			t.Fatal(err)
		}
		reloads := b.sd.reloaded
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseApplied || b.sd.reloaded != reloads {
			t.Fatalf("the settled push was run again: %+v, %d reloads", r, b.sd.reloaded)
		}
		if len(b.logged(zapcore.WarnLevel, "already settled")) != 1 {
			t.Error("no warning")
		}
	})
	t.Run("a settled push back in in/ whose rename would fail keeps its result", func(t *testing.T) {
		b := newTestBox(t)
		files := b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
		id := b.push(files)
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.x("in"), id+".tar"), tgz(t, files), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{fail: failAt(map[string]int{"take": -1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseApplied {
			t.Fatalf("a settled result was overwritten: %+v", r)
		}
	})
	t.Run("a failed take writes its result before removing the entry", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		b.runCrash(hooks{fail: failAt(map[string]int{"take": -1}), crash: crashAfter("result:failed", 1)})
		if !exists(filepath.Join(b.x("in"), id+".tar")) {
			t.Fatal("the entry was removed before its result")
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseFailed || b.sd.reloaded != 0 {
			t.Fatalf("the next run did not remove the entry as settled: %+v, %d reloads", r, b.sd.reloaded)
		}
	})
	t.Run("an attempt that reached only verified, back in in/: run afresh, the descent check decides", func(t *testing.T) {
		b := newTestBox(t)
		files := b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
		id := b.push(files)
		head := proof.ObjectID("commit", files["commit"])
		if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified, Commit: head}), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r == nil || r.Phase != phaseApplied {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a result that is not one the applier writes counts as none", func(t *testing.T) {
		for _, junk := range []string{"{", "null", `{"phase":"superseded"}`} {
			b := newTestBox(t)
			files := b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base)
			id := b.push(files)
			if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), []byte(junk), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := b.run(hooks{}); err != nil {
				t.Fatal(err)
			}
			b.settled()
			if r := b.result(id); r == nil || r.Phase != phaseApplied {
				t.Fatalf("%s: the push was not judged afresh: %+v", junk, r)
			}
		}
	})
	t.Run("recovery's failed that cannot be written clears the stale verified", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		if err := os.WriteFile(filepath.Join(b.x("work"), id+".tar"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseVerified}), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{fail: failAt(map[string]int{"result:failed": 1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if r := b.result(id); r != nil {
			t.Fatalf("a stale verified stayed: %+v", r)
		}
	})
	t.Run("what is not a bundle, named for a settled id, gets the not-a-bundle line", func(t *testing.T) {
		b := newTestBox(t)
		id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(b.x("in"), id+".tar"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if !b.errorLogged("not a bundle") {
			t.Error("no not-a-bundle line")
		}
	})
	t.Run("a push recovery settled this run, its result journaled, is not run again from in/", func(t *testing.T) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		head := b.repo.commit(v2, &b.alice, b.base)
		files := b.repo.bundleFiles(head, b.base)
		id := randomID(t)
		// The crash left the record, the swap and in/'s entry (its unlink
		// never durable); recovery rolls back but cannot write the result.
		b.writeInstalled(v2)
		b.lyingRecord(record{ID: id, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
			Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
		if err := os.WriteFile(filepath.Join(b.x("in"), id+".tar"), tgz(t, files), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{fail: failAt(map[string]int{"result:rolled_back": -1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if !bytes.Equal(b.installed(), b.v1) || b.sd.reloaded != 1 || b.result(id) != nil {
			t.Fatalf("the push was run again after its rollback: %d reloads, result %+v", b.sd.reloaded, b.result(id))
		}
	})
	t.Run("a take that fails on what is not a regular file gives no result", func(t *testing.T) {
		b := newTestBox(t)
		id := randomID(t)
		if err := os.Mkdir(filepath.Join(b.x("in"), id+".tar"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := b.run(hooks{fail: failAt(map[string]int{"take": -1})}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if b.result(id) != nil {
			t.Error("a directory got a result")
		}
	})
}

// A console edit landing during a no_change push is left as found and
// the baseline does not advance to bytes that are not on disk (I1, I4).
func TestNoChangeRechecksTheFile(t *testing.T) {
	b := newTestBox(t)
	head := b.repo.commit(b.v1, &b.alice, b.base)
	id := b.push(b.repo.bundleFiles(head, b.base))
	edited := append(append([]byte{}, b.v1...), "# 3am\n"...)
	// The edit lands once the record is durable, before the baseline.
	edit := func(p string) {
		if p == "record:no_change" {
			b.writeInstalled(edited)
		}
	}
	if err := b.run(hooks{crash: edit}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if r := b.result(id); r == nil || r.Phase != phaseUnknown || r.Error != msgChanged {
		t.Fatalf("%+v", r)
	}
	if b.applied().SHA != b.base || !bytes.Equal(b.installed(), edited) {
		t.Error("the baseline advanced, or the edit was overwritten")
	}
}

// An init record a crash left after its swap recovers as the applier's
// does: on a moteld still activating after the wait — not running —
// the previous bytes go back and it ends failed, never counted applied
// for the next start, and nothing is reloaded.
func TestInitRecordRecoversLikeTheApplier(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	b.writeInstalled(v2)
	id := randomID(t)
	b.lyingRecord(record{ID: id, Origin: originInit, Phase: phaseSwapped, Commit: sha, Path: testPath, Signer: "init",
		Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
	b.sd.states = []string{"activating"}
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	if b.applied().SHA != b.base || !bytes.Equal(b.installed(), b.v1) || b.sd.reloaded != 0 {
		t.Errorf("baseline %s, %d reloads; the previous bytes are back: %v", b.applied().SHA, b.sd.reloaded, bytes.Equal(b.installed(), b.v1))
	}
	if phase, msg := b.outcomeLogged(id); phase != phaseFailed || msg != msgInterruptedStopped {
		t.Errorf("journaled %s %q", phase, msg)
	}
}

// A reload in flight is waited out like `activating` (step 16); in
// recovery, one still in flight after the wait is up, and the rollback's
// reload queues behind it, where a restart loop still activating is not
// running and settles as such.
func TestReloadingIsWaitedOut(t *testing.T) {
	t.Run("push", func(t *testing.T) {
		b := newTestBox(t)
		b.sd.states = []string{"reloading", "reloading", "active"}
		r := b.pushOne(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
		if r.Phase != phaseApplied {
			t.Fatalf("%+v", r)
		}
	})
	recovery := func(t *testing.T, states []string) (*testBox, string) {
		b := newTestBox(t)
		v2 := boxFile(2, b.alice)
		head := b.repo.commit(v2, &b.alice, b.base)
		b.writeInstalled(v2)
		id := randomID(t)
		b.lyingRecord(record{ID: id, Origin: originApplier, Phase: phaseSwapped, Commit: head, Path: testPath, Signer: "alice@example.com",
			Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(v2)})
		b.sd.states = states
		if err := b.run(hooks{}); err != nil {
			t.Fatal(err)
		}
		b.settled()
		if !bytes.Equal(b.installed(), b.v1) {
			t.Error("the previous bytes are not back")
		}
		return b, id
	}
	t.Run("recovery, still reloading after the wait: the reload queues behind it", func(t *testing.T) {
		b, id := recovery(t, []string{"reloading"})
		if r := b.result(id); r == nil || r.Phase != phaseRolledBack || b.sd.reloaded != 1 {
			t.Fatalf("%+v, %d reloads", r, b.sd.reloaded)
		}
	})
	t.Run("recovery, a restart loop still activating after the wait: settled as not running", func(t *testing.T) {
		b, id := recovery(t, []string{"activating"})
		if r := b.result(id); r == nil || r.Phase != phaseFailed || r.Error != msgInterruptedStopped || b.sd.reloaded != 0 {
			t.Fatalf("%+v, %d reloads", r, b.sd.reloaded)
		}
	})
}

// Bundles queued behind a moteld that never settles share one wait.
func TestOneWaitPerRun(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"activating"}
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	first := b.pushAt(b.repo.bundleFiles(c1, b.base), b.clock.Now().Add(-time.Minute))
	second := b.push(b.repo.bundleFiles(c1, b.base))
	start := b.clock.Now()
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	b.settled()
	for _, id := range []string{first, second} {
		if r := b.result(id); r == nil || r.Error != msgStillStarting {
			t.Errorf("%+v", r)
		}
	}
	if waited := b.clock.Now().Sub(start); waited > activatingWait+activatingPoll {
		t.Errorf("waited %v for two bundles", waited)
	}
}

// init on a moteld still reloading after the wait reloads (the
// reload queues behind the one in flight), never records `applied`
// for bytes the running moteld may not have read.
func TestInitOnReloadingMotelReloads(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"reloading"}
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	out, err := b.applier(hooks{}).runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v2)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != phaseApplied || b.sd.reloaded != 1 {
		t.Fatalf("%+v, %d reloads", out, b.sd.reloaded)
	}
}

// init's rollback after a write that failed past the swap reloads the
// previous bytes, as the applier's does: init found moteld up before
// it swapped.
func TestInitRollbackReloads(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	a := b.applier(hooks{fail: failAt(map[string]int{"record:swapped": -1})})
	out, err := a.runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v2)
	if err != nil {
		t.Fatal(err)
	}
	b.settled()
	if out.Phase != phaseRolledBack || out.Error != msgRolledBackUnrecorded || b.sd.reloaded != 1 || !bytes.Equal(b.installed(), b.v1) {
		t.Fatalf("%+v, %d reloads", out.result, b.sd.reloaded)
	}
}

func TestRunInitRefusesARecordRecoveryCouldNotRead(t *testing.T) {
	b := newTestBox(t)
	sha := b.repo.commit(b.v1, &b.alice, b.base)
	for _, rec := range []record{
		{ID: "../../etc/x", Commit: sha, Path: testPath},
		{ID: randomID(t), Commit: strings.Repeat("a", 64), Path: testPath},
		{ID: randomID(t), Commit: sha, Path: "../x"},
	} {
		if _, err := b.applier(hooks{}).runInit(context.Background(), rec, b.v1); err == nil {
			t.Errorf("%+v accepted", rec)
		}
	}
}

// A changed line longer than the pre-redaction cut still gets the note.
func TestCaddyfileDiffLongLineNoted(t *testing.T) {
	long := strings.Repeat("x", maxDiffRedacted+10) + "\n"
	d := caddyfileDiff([]byte("a\n"), []byte("a\n"+long))
	if !strings.HasSuffix(d, diffCutNote) || len(d) > maxDiff {
		t.Fatalf("%d bytes: %q", len(d), d[:min(len(d), 80)])
	}
}

// Markers dated past the clock with no result — a hostile writer's —
// hold none of the 32 slots and get no result; a result whose marker is
// from the future (a push settled just before the clock stepped back)
// is aged by root's own write instead, and survives.
func TestRetentionFutureDates(t *testing.T) {
	b := newTestBox(t)
	now := b.clock.Now()
	var hostile, real []string
	for i := 0; i < keepIDs+5; i++ {
		id := randomID(t)
		hostile = append(hostile, id)
		b.markerAt(id, now.Add(time.Hour))
	}
	for i := 0; i < 3; i++ {
		id := randomID(t)
		real = append(real, id)
		b.resultAt(id, phaseApplied, now.Add(-time.Minute))
		b.markerAt(id, now.Add(-time.Minute))
	}
	stepped := randomID(t)
	b.resultAt(stepped, phaseApplied, now.Add(20*time.Minute))
	b.markerAt(stepped, now.Add(20*time.Minute))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range append(real, stepped) {
		if b.result(id) == nil || !b.hasMarker(id) {
			t.Error("a real result was swept")
		}
	}
	for _, id := range hostile {
		if b.hasMarker(id) || b.result(id) != nil {
			t.Error("a marker from the future was kept or given a result")
		}
	}
}

// Retention leaves the push under the record alone, reading the record
// itself.
func TestSweepReadsTheRecord(t *testing.T) {
	b := newTestBox(t)
	id := randomID(t)
	b.markerAt(id, b.clock.Now().Add(-time.Hour))
	b.lyingRecord(record{ID: id, Origin: originInit, Phase: phaseInstalling, Commit: b.base, Path: testPath, Prev: b.v1, PrevSHA256: digest(b.v1), NewSHA256: digest(b.v1)})
	b.applier(hooks{}).sweep()
	if b.result(id) != nil || !b.hasMarker(id) {
		t.Error("the push holding the record was settled as stranded")
	}
}

// A result a run wrote survives that run's sweep, whatever else is kept:
// here 32 results dated past the clock, as after a clock stepped back,
// which count as written now and so as newer than the push's own.
func TestRetentionKeepsThisRunsResults(t *testing.T) {
	b := newTestBox(t)
	b.clock.advance(time.Minute) // the push's result is written before the fake now
	now := b.clock.Now()
	for i := 0; i < keepIDs; i++ {
		id := randomID(t)
		b.resultAt(id, phaseApplied, now.Add(time.Hour))
		b.markerAt(id, now.Add(time.Hour))
	}
	id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	if err := b.run(hooks{}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(id); r == nil || r.Phase != phaseApplied || !b.hasMarker(id) {
		t.Fatalf("this run's result was swept: %+v", r)
	}
}

// The run has one budget for waiting: once it is spent, a later
// episode in the same run gets no wait — one is-active question, then
// the refusal.
func TestWaitBudgetIsPerRun(t *testing.T) {
	b := newTestBox(t)
	b.sd.states = []string{"activating", "active", "activating"}
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	c2 := b.repo.commit(boxFile(3, b.alice), &b.alice, c1)
	first := b.pushAt(b.repo.bundleFiles(c1, b.base), b.clock.Now().Add(-time.Minute))
	second := b.push(b.repo.bundleFiles(c2, c1))
	read := func(id string) {
		if id == second {
			b.clock.advance(2 * activatingWait) // a long verification, say
		}
	}
	start := b.clock.Now()
	if err := b.run(hooks{read: read}); err != nil {
		t.Fatal(err)
	}
	if waited := b.clock.Now().Sub(start) - 2*activatingWait; waited > activatingPoll {
		t.Errorf("waited %v beyond the verification's own time", waited)
	}
	if b.sd.asked != 3 {
		t.Errorf("%d is-active questions, want 3 (activating, active; then one past the budget)", b.sd.asked)
	}
	if r := b.result(first); r == nil || r.Phase != phaseApplied {
		t.Errorf("first: %+v", r)
	}
	if r := b.result(second); r == nil || r.Phase != phaseRefused || r.Error != msgStillStarting {
		t.Errorf("second: %+v", r)
	}
}

// init asks is-active once, when it begins, whatever follows.
func TestInitAsksOnce(t *testing.T) {
	b := newTestBox(t)
	v2 := boxFile(2, b.alice)
	sha := b.repo.commit(v2, &b.alice, b.base)
	out, err := b.applier(hooks{}).runInit(context.Background(), record{ID: randomID(t), Commit: sha, Path: testPath, BoxWebhook: "deploy.example.com"}, v2)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != phaseApplied || b.sd.asked != 1 || b.sd.reloaded != 1 {
		t.Fatalf("%+v; %d questions, %d reloads", out, b.sd.asked, b.sd.reloaded)
	}
}

// A push this run settled whose result did not land is not settled
// again as stranded in the same run: its outcome is in the journal.
func TestUnwrittenResultNotStrandedSameRun(t *testing.T) {
	b := newTestBox(t)
	c1 := b.repo.commit(boxFile(2, b.alice), &b.alice, b.base)
	id := b.pushAt(b.repo.bundleFiles(c1, b.base), b.clock.Now().Add(-time.Hour))
	if err := b.run(hooks{fail: failAt(map[string]int{"result:verified": 1, "result:failed": 1})}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(id); r != nil {
		t.Fatalf("the sweep wrote %+v for a push this run settled", r)
	}
	if !b.errorLogged("box result could not be written") {
		t.Error("no journal line")
	}
}

// A `failed` that landed in place of a failed `verified` is kept.
func TestLandedFailedIsKept(t *testing.T) {
	b := newTestBox(t)
	id := b.push(b.repo.bundleFiles(b.repo.commit(boxFile(2, b.alice), &b.alice, b.base), b.base))
	errSync := errors.New("fsync: input/output error")
	fail := func(p string) error {
		switch p {
		case "result:verified":
			return errNoSpace
		case "result:failed":
			if err := os.WriteFile(filepath.Join(b.x("out"), id+".json"), encodeJSON(result{ID: id, Phase: phaseFailed, Error: "x"}), 0o640); err != nil {
				t.Fatal(err)
			}
			return errSync
		}
		return nil
	}
	if err := b.run(hooks{fail: fail}); err != nil {
		t.Fatal(err)
	}
	if r := b.result(id); r == nil || r.Phase != phaseFailed {
		t.Fatalf("%+v", r)
	}
}
