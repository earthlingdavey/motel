package liveswap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A deploy of v2 that a moteld stop cut short (#318): what it left
// is a marker naming v2 and its start, and a release under that name.
// These are what the dead deploy would have left on disk.
func leaveInterruptedDeploy(t *testing.T, rig *testRig) {
	t.Helper()
	must(t, rig.spec.dirs.ensure())
	must(t, os.MkdirAll(rig.spec.dirs.release("v2"), 0o755))
	must(t, saveDeployMarker(rig.spec.dirs, deployMarker{Version: "v2", StartedAt: interruptedAt}))
}

func markerPresent(t *testing.T, rig *testRig) bool {
	t.Helper()
	_, err := os.Lstat(rig.spec.dirs.deploying)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func releasePresent(t *testing.T, rig *testRig, version string) bool {
	t.Helper()
	_, err := os.Stat(rig.spec.dirs.release(version))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// recordOfV2 is v2's deploy record as written.
func recordOfV2(t *testing.T, rig *testRig) deployResult {
	t.Helper()
	b, err := readDeployRecord(rig.spec.dirs, "v2")
	if err != nil {
		t.Fatalf("record of v2: %v", err)
	}
	var r deployResult
	must(t, json.Unmarshal(b, &r))
	return r
}

// interruptedAt is a start time no deploy in a rig has: the fake clock
// stands at its epoch unless a test sleeps it.
var interruptedAt = time.Unix(1_600_000_000, 0).UTC()

func TestDeployHoldsAMarkerWhileItRunsAndRemovesItAfter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome error
	}{{"succeeds", nil}, {"fails", errTest}} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			held, release := rig.holdFetch(t)
			done := make(chan error, 1)
			go func() { done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}) }()
			held()
			m, err := loadDeployMarker(rig.spec.dirs)
			if err != nil || m.Version != "v1" || !m.StartedAt.Equal(rig.clock.Now()) {
				t.Fatalf("a deploy in fetch must hold a marker naming it and its start, got %+v, %v", m, err)
			}
			release(tc.outcome)
			if err := <-done; (err == nil) != (tc.outcome == nil) {
				t.Fatalf("deploy: %v", err)
			}
			if markerPresent(t, rig) {
				t.Fatal("a deploy that returned must leave no marker, whatever it came to")
			}
		})
	}
}

func TestRollbackWritesNoMarker(t *testing.T) {
	// A rollback launches a release already on disk: no marker, since
	// nothing it does may ever lead to that release being removed.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() { done <- rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}) }()
	held()
	present := markerPresent(t, rig)
	release(nil)
	must(t, <-done)
	if present {
		t.Fatal("a rollback must write no marker")
	}
}

func TestPreStartCarriesTheDeadlineAndTheInstanceDoesNot(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	if len(rig.runner.started) != 2 {
		t.Fatalf("want the pre_start and the instance, got %d", len(rig.runner.started))
	}
	if got := rig.runner.started[0].startTimeout; got != rig.spec.deadline || got <= 0 {
		t.Fatalf("the pre_start unit must carry deadline (%s) as its own bound, got %s", rig.spec.deadline, got)
	}
	if got := rig.runner.started[1].startTimeout; got != 0 {
		t.Fatalf("the instance carries no start timeout, got %s", got)
	}
}

func TestRecoveryRemovesTheReleaseOfADeployNoMotelFinished(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v1", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
	leaveInterruptedDeploy(t, rig)

	must(t, rig.ma.ensureRunning())
	if releasePresent(t, rig, "v2") {
		t.Fatal("the release of the deploy motel did not finish must be removed")
	}
	if !releasePresent(t, rig, "v1") {
		t.Fatal("the serving release must stay")
	}
	if markerPresent(t, rig) {
		t.Fatal("a settled marker must be removed")
	}
	rec := recordOfV2(t, rig)
	if rec.Status != "failed" || !strings.Contains(rec.Error, "moteld") || !rec.StartedAt.Equal(interruptedAt) {
		t.Fatalf("the cut-short deploy must be recorded failed, naming why, at its own start: %+v", rec)
	}
	if last := rig.ma.status().LastDeploy; last == nil || last.Version != "v2" || last.Status != "failed" {
		t.Fatalf("last_deploy must say what became of the deploy: %+v", last)
	}

	// The version is deployable again: no "already exists".
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	if rec := recordOfV2(t, rig); rec.Status != "succeeded" {
		t.Fatalf("the redeploy's record replaces the failed one: %+v", rec)
	}
}

func TestAMarkerForTheServingVersionIsOnlyCleared(t *testing.T) {
	// A deploy that reached the cutover before moteld stopped: its
	// version is the one serving, and is never removed.
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v2", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	leaveInterruptedDeploy(t, rig)
	must(t, rig.ma.ensureRunning())
	if !releasePresent(t, rig, "v2") {
		t.Fatal("the serving release must never be removed")
	}
	if markerPresent(t, rig) {
		t.Fatal("the marker must be cleared")
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v2"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("nothing failed: no record may be written, got %v", err)
	}
}

func TestAStaleMarkerOfARecordedDeployIsOnlyCleared(t *testing.T) {
	// A deploy that was recorded and whose marker removal then failed:
	// the record carries the marker's start, and the release is a real
	// rollback target.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	must(t, saveDeployMarker(rig.spec.dirs, deployMarker{Version: "v2", StartedAt: recordOfV2(t, rig).StartedAt}))
	must(t, rig.ma.ensureRunning())
	if !releasePresent(t, rig, "v2") {
		t.Fatal("a recorded release must not be removed for a stale marker")
	}
	if markerPresent(t, rig) {
		t.Fatal("the stale marker must be cleared")
	}
	if rec := recordOfV2(t, rig); rec.Status != "succeeded" {
		t.Fatalf("the record must be left as it was: %+v", rec)
	}
}

func TestAnUnconfirmedSweepRemovesNothingAMarkerNames(t *testing.T) {
	// The dead deploy's pre_start, or its instance, may still run out
	// of the release: nothing is removed until a sweep confirms.
	rig := newTestRig(t)
	leaveInterruptedDeploy(t, rig)
	rig.runner.sweepErr = &migrationRunningError{unit: "motel-demo.v2.5555555555555555.prestart.service", version: "v2"}
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) || !strings.Contains(err.Error(), "pre_start of v2") {
		t.Fatalf("recovery must retry, naming the migration it waits for, got %v", err)
	}
	if !releasePresent(t, rig, "v2") || !markerPresent(t, rig) {
		t.Fatal("nothing may be removed while the migration may run")
	}

	// A deploy meanwhile is refused, naming the migration, and leaves
	// both too.
	err = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/3", version: "v3"})
	if !errors.Is(err, errSweepUnconfirmed) || !strings.Contains(err.Error(), "pre_start of v2") {
		t.Fatalf("a deploy beside the migration must be refused, naming it, got %v", err)
	}
	if !releasePresent(t, rig, "v2") || !markerPresent(t, rig) || releasePresent(t, rig, "v3") {
		t.Fatal("a refused deploy must change nothing on disk")
	}

	// Once the migration has ended, the next attempt settles it.
	rig.runner.sweepErr = nil
	must(t, rig.ma.ensureRunning())
	if releasePresent(t, rig, "v2") || markerPresent(t, rig) {
		t.Fatal("a confirmed sweep must settle the marker")
	}
}

func TestADeploySettlesAMarkerFirst(t *testing.T) {
	// No restart needed for the next deploy to clear the way: one of
	// the orphan's own version proceeds rather than 422.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	leaveInterruptedDeploy(t, rig)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"}))
	if rec := recordOfV2(t, rig); rec.Status != "succeeded" {
		t.Fatalf("the redeploy must succeed and be recorded: %+v", rec)
	}
	if markerPresent(t, rig) {
		t.Fatal("no marker may outlive the deploy")
	}
}

func TestAReleaseThatCannotBeRemovedEndsAsAFailedDeploysLeftOnDisk(t *testing.T) {
	// The marker has one slot: one the next deploy could overwrite must
	// not stay waiting on a removal that fails. Settling records the
	// release left on disk, in a failed deploy's words, and clears it.
	rig := newTestRig(t)
	leaveInterruptedDeploy(t, rig)
	// releases/ moved aside behind a link: removal is refused (linkAt).
	moved := rig.spec.dirs.releases + ".moved"
	must(t, os.Rename(rig.spec.dirs.releases, moved))
	must(t, os.Symlink(moved, rig.spec.dirs.releases))
	must(t, rig.ma.settleInterrupted(rig.ma.snapshot(), ""))
	if markerPresent(t, rig) {
		t.Fatal("the marker must go even when the release stays")
	}
	if _, err := os.Stat(moved + "/v2"); err != nil {
		t.Fatalf("nothing may be removed through the link: %v", err)
	}
	rec := recordOfV2(t, rig)
	if rec.Status != "failed" || !strings.Contains(rec.Error, "release v2 left on disk") || !strings.Contains(rec.Error, "cut short") {
		t.Fatalf("the record must name the restart and the release left: %+v", rec)
	}
}

// A staged artifact a stop left mid-transfer in tmp/ goes at the start
// that follows, once recovery settles the app — before the next deploy
// could fail an upload on the disk it holds. A name of another shape
// is left alone.
func TestRecoverySettleRemovesAStagedArtifactAStopLeft(t *testing.T) {
	rig := newTestRig(t)
	must(t, os.MkdirAll(rig.spec.dirs.tmp, 0o750))
	for _, n := range []string{"push-abc.tar.gz", "artifact-def.tar.gz", "other.tar.gz"} {
		must(t, os.WriteFile(rig.spec.dirs.tmp+"/"+n, []byte("x"), 0o600))
	}
	rig.ma.settleAfterRecovery(rig.ma.snapshot(), "")
	entries, err := os.ReadDir(rig.spec.dirs.tmp)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != "other.tar.gz" {
		t.Fatalf("tmp/ holds %v, want other.tar.gz alone", entries)
	}
	// Not through a link: tmp/ pointing at another app's leaves that
	// app's staged upload where it is.
	theirs := t.TempDir()
	must(t, os.WriteFile(theirs+"/push-theirs.tar.gz", []byte("x"), 0o600))
	must(t, os.RemoveAll(rig.spec.dirs.tmp))
	must(t, os.Symlink(theirs, rig.spec.dirs.tmp))
	rig.ma.settleAfterRecovery(rig.ma.snapshot(), "")
	if _, err := os.Stat(theirs + "/push-theirs.tar.gz"); err != nil {
		t.Fatalf("swept through a link: %v", err)
	}
}

func TestARecordThatCannotBeReadIsNeitherReplacedNorActedOn(t *testing.T) {
	// The record may be that deploy's own: nothing is written over it,
	// and nothing removed on a guess.
	rig := newTestRig(t)
	leaveInterruptedDeploy(t, rig)
	must(t, os.MkdirAll(rig.spec.dirs.deploys+"/v2.json", 0o700)) // not a regular file: unreadable
	must(t, rig.ma.settleInterrupted(rig.ma.snapshot(), ""))
	if markerPresent(t, rig) {
		t.Fatal("the marker must be cleared")
	}
	if !releasePresent(t, rig, "v2") {
		t.Fatal("nothing may be removed when the record cannot be read")
	}
	if fi, err := os.Lstat(rig.spec.dirs.deploys + "/v2.json"); err != nil || !fi.IsDir() {
		t.Fatalf("the unreadable record must be left as it is: %v", err)
	}
}

func TestNothingIsSettledThroughALinkedAppDir(t *testing.T) {
	// <root>/demo -> shop: demo's recovery must not read, remove or
	// act on shop's marker, which shop's own deploy may be holding.
	rig := newTestRig(t)
	shop := newAppDirs(rig.spec.dirs.root, "shop")
	must(t, shop.ensure())
	must(t, os.MkdirAll(shop.release("v2"), 0o755))
	must(t, saveDeployMarker(shop, deployMarker{Version: "v2", StartedAt: interruptedAt}))
	must(t, os.RemoveAll(rig.spec.dirs.app))
	must(t, os.Symlink("shop", rig.spec.dirs.app))
	must(t, rig.ma.ensureRunning())
	if _, err := os.Stat(shop.deploying); err != nil {
		t.Fatalf("the sibling's marker must be left alone: %v", err)
	}
	if _, err := os.Stat(shop.release("v2")); err != nil {
		t.Fatalf("the sibling's release must be left alone: %v", err)
	}
}

func TestADeployBeforeRecoveryKeepsTheReleaseStateNames(t *testing.T) {
	// Recovery runs in the background: a deploy can take the lock
	// first, with no instance in memory. A marker for the version
	// state.json names is a deploy that reached the cutover; its
	// release is the one recovery is about to relaunch.
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v2", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	leaveInterruptedDeploy(t, rig)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/3", version: "v3"}))
	if !releasePresent(t, rig, "v2") {
		t.Fatal("the release state.json names must never be removed")
	}
	if markerPresent(t, rig) {
		t.Fatal("the marker must be cleared")
	}
	if _, err := readDeployRecord(rig.spec.dirs, "v2"); !errors.Is(err, errNoDeployRecord) {
		t.Fatalf("a deploy that reached the cutover must not be recorded cut short, got %v", err)
	}
}

func TestAStateThatCannotBeReadVouchesForNothing(t *testing.T) {
	// Only a deploy reaches a settle past an unreadable state.json
	// (recovery refuses it first). Nothing serves from such a state,
	// so the record decides: none, and the release is removed.
	rig := newTestRig(t)
	leaveInterruptedDeploy(t, rig)
	rig.store.err = errTest
	must(t, rig.ma.settleInterrupted(rig.ma.snapshot(), ""))
	if releasePresent(t, rig, "v2") || markerPresent(t, rig) {
		t.Fatal("the release of the cut-short deploy must be removed and the marker cleared")
	}
	if rec := recordOfV2(t, rig); rec.Status != "failed" || !strings.Contains(rec.Error, "cut short") {
		t.Fatalf("the cut-short deploy must be recorded: %+v", rec)
	}
}

func TestAMarkerThatIsNotAFileIsRemovedAndActedOnForNothing(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.spec.dirs.ensure())
	must(t, os.MkdirAll(rig.spec.dirs.release("v2"), 0o755))
	elsewhere := t.TempDir() + "/marker.json"
	must(t, os.WriteFile(elsewhere, []byte(`{"version":"v2","started_at":"2026-10-19T00:00:00Z"}`), 0o600))
	must(t, os.Symlink(elsewhere, rig.spec.dirs.deploying))
	must(t, rig.ma.ensureRunning())
	if markerPresent(t, rig) {
		t.Fatal("a link at the marker's name must be removed")
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("never what the link points at: %v", err)
	}
	if !releasePresent(t, rig, "v2") {
		t.Fatal("and nothing removed for it")
	}
}

func TestADirectoryAtTheMarkerFailsExtractingDeploysNamingIt(t *testing.T) {
	// Only root or the motel user by hand can put one there. It is
	// never deleted (what it holds is not motel's): each extracting
	// deploy fails naming the path, and the journal says what was and
	// was not done, until it is removed by hand. Accepted residual.
	rig := newTestRig(t)
	core, logs := observer.New(zap.WarnLevel)
	rig.ma.logger = zap.New(core)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, os.MkdirAll(rig.spec.dirs.deploying+"/kept", 0o700))

	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "writing the deploy marker") || !strings.Contains(err.Error(), rig.spec.dirs.deploying) {
		t.Fatalf("the deploy must fail naming the marker's path, got %v", err)
	}
	if _, err := os.Stat(rig.spec.dirs.deploying + "/kept"); err != nil {
		t.Fatalf("what the directory holds must never be deleted: %v", err)
	}
	if releasePresent(t, rig, "v2") {
		t.Fatal("nothing may be fetched past a marker the deploy could not write")
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "removed") && !strings.Contains(e.Message, "not removed") && !strings.Contains(e.Message, "where it can be") {
			t.Errorf("a log line claims a removal that did not happen: %q", e.Message)
		}
		if strings.Contains(e.Message, "clears it") {
			t.Errorf("a log line promises a clear nothing will make: %q", e.Message)
		}
	}
	// A rollback writes no marker, so it still goes through.
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
}

func TestAMarkerThatIsNotOneIsRemovedAndActedOnForNothing(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      "{",
		"not a version": `{"version":"../v1","started_at":"2026-10-19T00:00:00Z"}`,
		"no version":    `{"started_at":"2026-10-19T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			must(t, rig.spec.dirs.ensure())
			must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o755))
			must(t, os.WriteFile(rig.spec.dirs.deploying, []byte(body), 0o600))
			must(t, rig.ma.ensureRunning())
			if markerPresent(t, rig) {
				t.Fatal("a marker that is not one must be removed")
			}
			if !releasePresent(t, rig, "v1") {
				t.Fatal("and nothing removed for it")
			}
		})
	}
}
