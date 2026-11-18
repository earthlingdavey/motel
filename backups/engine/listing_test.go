package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
)

// seenAt is when the record says a snapshot was last in the repository.
func seenAt(t *testing.T, s *record.Snapshot) time.Time {
	t.Helper()
	if s == nil || s.Seen == nil {
		t.Fatalf("the record does not say when the snapshot was last seen: %+v", s)
	}
	return *s.Seen
}

func listedAt(t *testing.T, st *record.Status) time.Time {
	t.Helper()
	if st.Listed == nil {
		t.Fatal("the record does not say when the repository was last listed")
	}
	return *st.Listed
}

func listings(b *box) (n int) {
	for _, r := range strings.Fields(b.roles()) {
		if r == "listing" {
			n++
		}
	}
	return n
}

// A run ends by asking the repository, once, what it holds of every
// app, and the record says when each snapshot it names was last there.
func TestARunListsTheRepositoryOnceAtTheEnd(t *testing.T) {
	b := newBox(t)
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if listings(b) != 1 || !strings.HasSuffix(b.roles(), " listing") {
		t.Fatalf("units: %s", b.roles())
	}
	s := b.spec("listing")
	// Every app's: no app's own tag (`motel` is every app's, and no
	// clean-run record's).
	if !slices.Contains(s.Argv, "--no-lock") || slices.ContainsFunc(s.Argv, func(a string) bool { return strings.HasPrefix(a, "app:") }) || s.Argv[0] != b.cfg.Restic || s.Argv[1] != "snapshots" {
		t.Errorf("argv: %q", s.Argv)
	}
	if s.User != backupUser || !s.Network || s.EnvironmentFile != b.cfg.EnvFile || len(s.Binds) != 0 {
		t.Errorf("the listing unit is given: %+v", s)
	}
	blog := st.Apps["blog"]
	if st.Listed == nil || st.Listed.Before(st.Started) {
		t.Fatalf("listed: %v", st.Listed)
	}
	for what, snap := range map[string]*record.Snapshot{"last_ok": blog.LastOK, "last_snapshot": blog.LastSnapshot, "restore_proven": &blog.RestoreProven.Snapshot} {
		if snap.Seen == nil || snap.Seen.Before(*st.Listed) {
			t.Errorf("%s: seen %v, listed %v", what, snap.Seen, st.Listed)
		}
	}
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if again.Listed == nil || again.Apps["blog"].LastOK.Seen == nil {
		t.Fatalf("the record on disk: listed %v, last_ok %+v", again.Listed, again.Apps["blog"].LastOK)
	}
}

// A snapshot the listing does not hold keeps when it was last seen,
// under a listing that is newer: that is what gone looks like.
func TestASnapshotTheListingLacksKeepsWhenItWasLastSeen(t *testing.T) {
	b := newBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	seen := seenAt(t, first.Apps["blog"].LastOK)

	b.summary = `{"message_type":"summary"}` // this run makes no snapshot: last_ok stays the first run's
	b.listing = `[]`
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	blog := st.Apps["blog"]
	if blog.Class != record.Failed || blog.LastOK == nil || blog.LastOK.ID != snapA {
		t.Fatalf("fixture: %+v", blog)
	}
	if blog.LastOK.Seen == nil || !blog.LastOK.Seen.Equal(seen) {
		t.Errorf("last seen %v, want the first run's %v", blog.LastOK.Seen, seen)
	}
	if st.Listed == nil || !st.Listed.After(seen) {
		t.Errorf("listed %v is not after seen %v: nothing says the snapshot is gone", st.Listed, seen)
	}
}

// A snapshot tagged for more than one app — motel writes one app tag,
// but a tag can be added off the box — is held for each of them: the
// last tag alone would call blog's snapshot gone while app:blog is on it.
func TestASnapshotTaggedForTwoAppsIsHeldForBoth(t *testing.T) {
	b := newBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	seen := seenAt(t, first.Apps["blog"].LastOK)

	b.summary = `{"message_type":"summary"}` // this run makes no snapshot: last_ok stays the first run's
	b.listing = `[{"id":"` + snapA + `","time":"2026-09-02T00:00:00Z","tags":["app:blog","app:shop"]}]`
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	blog := st.Apps["blog"]
	if blog.LastOK == nil || blog.LastOK.ID != snapA {
		t.Fatalf("fixture: %+v", blog)
	}
	if blog.LastOK.Seen == nil || !blog.LastOK.Seen.After(seen) {
		t.Errorf("blog's snapshot, still tagged app:blog, was not seen by the listing: seen %v, listed %v", blog.LastOK.Seen, st.Listed)
	}
}

// What this run itself saw written is not called gone by this run's own
// listing: a store that lists a new object late would otherwise turn
// every good backup into a missing one for an hour.
func TestARunsOwnSnapshotIsNotGoneByItsOwnListing(t *testing.T) {
	b := newBox(t)
	b.listing = `[]`
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	blog := st.Apps["blog"]
	if blog.Class != record.OK {
		t.Fatalf("fixture: %+v", blog)
	}
	if seenAt(t, blog.LastOK).Before(listedAt(t, st)) {
		t.Errorf("made by this run, and gone by its listing: seen %v, listed %v", blog.LastOK.Seen, st.Listed)
	}
}

// A snapshot of another app's, or a pre-restore one, in the listing is
// not this app's snapshot being there.
func TestTheListingIsReadPerApp(t *testing.T) {
	b := newBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	seen := seenAt(t, first.Apps["blog"].LastOK)
	b.summary = `{"message_type":"summary"}`
	b.listing = `[{"id":"` + snapA + `","time":"2026-09-02T00:00:00Z","tags":["app:shop"]}]`
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if got := st.Apps["blog"].LastOK.Seen; got == nil || !got.Equal(seen) {
		t.Fatalf("a snapshot tagged for another app counted as blog's: seen %v, want %v", got, seen)
	}
}

// A listing that fails — or says something that cannot be trusted — is
// a warning. It never turns a good backup bad, and never makes a
// snapshot look gone.
func TestAListingThatFailsChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome unit.Outcome
		listing string
	}{
		"exit 1":          {outcome: unit.Outcome{Result: "exit-code", ExitStatus: 1}},
		"no repository":   {outcome: unit.Outcome{Result: "exit-code", ExitStatus: 10}},
		"wrong password":  {outcome: unit.Outcome{Result: "exit-code", ExitStatus: 12}},
		"not JSON":        {outcome: unit.Outcome{Result: "success"}, listing: "Fatal: nope"},
		"an id like none": {outcome: unit.Outcome{Result: "success"}, listing: `[{"id":"latest","time":"2026-09-02T00:00:00Z","tags":["app:blog"]}]`},
		"nothing at all":  {outcome: unit.Outcome{Result: "success"}, listing: ""},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			first, err := Run(context.Background(), b.cfg, b)
			must(t, err)

			b.outcome["listing"], b.listing = tc.outcome, tc.listing
			st, err := Run(context.Background(), b.cfg, b)
			if err != nil {
				t.Fatalf("the run failed with its listing: %v", err)
			}
			blog := st.Apps["blog"]
			if blog.Class != record.OK || st.Error != "" {
				t.Fatalf("blog: %+v, run error %q", blog, st.Error)
			}
			if listings(b) != 2 {
				t.Fatalf("fixture: the second run did not try a listing: %s", b.roles())
			}
			if want := listedAt(t, first); !listedAt(t, st).Equal(want) {
				t.Errorf("listed %v, want the first run's %v", st.Listed, want)
			}
			// The run's own snapshot was found by its verify step: it has been
			// seen, and since the last listing that was answered.
			if seenAt(t, blog.LastOK).Before(*st.Listed) {
				t.Errorf("this run's snapshot looks gone: seen %v, listed %v", blog.LastOK.Seen, st.Listed)
			}
			if !strings.Contains(st.Warning, "list") {
				t.Errorf("warning: %q", st.Warning)
			}
		})
	}
}

// restic leaves a snapshot it cannot load out of the listing, says so on
// stderr only, and exits 0 [measured, 0.18.0, a cold cache]: an answer
// that came with anything beside it is not the whole repository, and is
// not taken for it.
func TestAListingResticHadSomethingToSayBesideIsNotBelieved(t *testing.T) {
	b := newBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	seen := seenAt(t, first.Apps["blog"].LastOK)

	b.summary = `{"message_type":"summary"}` // no new snapshot: last_ok stays the first run's
	b.listing = `[]`
	b.listingErr = `Ignoring "` + snapA + `": failed to load snapshot aaaaaaaa: invalid data returned` + "\n"
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if got := seenAt(t, st.Apps["blog"].LastOK); !got.Equal(seen) {
		t.Errorf("seen moved: %v, was %v", got, seen)
	}
	if want := listedAt(t, first); !listedAt(t, st).Equal(want) {
		t.Errorf("a listing restic left a snapshot out of was taken for the whole repository: listed %v, was %v", st.Listed, want)
	}
	if !strings.Contains(st.Warning, "could not load snapshot "+snapA[:8]) {
		t.Errorf("warning: %q", st.Warning)
	}

	// The record is for everyone to read, and restic's words can hold
	// where the repository is: none but that one known line reach it.
	b.listingErr = "Fatal: something about s3:https://bucket.example/secret-path\n"
	st, err = Run(context.Background(), b.cfg, b)
	must(t, err)
	if want := listedAt(t, first); !listedAt(t, st).Equal(want) {
		t.Errorf("listed %v, was %v", st.Listed, want)
	}
	if strings.Contains(st.Warning, "secret-path") || !strings.Contains(st.Warning, "beside") {
		t.Errorf("warning: %q", st.Warning)
	}
}

// Once the repository has refused for a reason every app shares, asking
// it again is one more wait for the same answer.
func TestNoListingAfterTheRepositoryRefused(t *testing.T) {
	b := newBox(t)
	b.outcome["upload"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	_, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if listings(b) != 0 {
		t.Fatalf("units: %s", b.roles())
	}
}

// With no snapshot in the record there is nothing to look for.
func TestNoListingWithNothingToLookFor(t *testing.T) {
	b := newBox(t)
	must(t, os.RemoveAll(filepath.Join(b.root, "blog")))
	b.history = "[]"
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if st.Apps["blog"].Class != record.Pending || listings(b) != 0 {
		t.Fatalf("blog %+v, units: %s", st.Apps["blog"], b.roles())
	}
}

// A drill and a restore write the record too, and keep what the last
// listing said.
func TestADrillAndARestoreKeepWhatTheListingSaid(t *testing.T) {
	b := restoreBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	listed := listedAt(t, first)
	st, _, err := Drill(context.Background(), b.cfg, b)
	must(t, err)
	if st.Listed == nil || !st.Listed.Equal(listed) || st.Apps["blog"].LastOK.Seen == nil {
		t.Errorf("after a drill: listed %v, last_ok %+v", st.Listed, st.Apps["blog"].LastOK)
	}
	if p := st.Apps["blog"].RestoreProven; p == nil || p.Snapshot.Seen == nil {
		t.Errorf("a drill fetched the snapshot and does not say it saw it: %+v", p)
	}
	_, err = Restore(context.Background(), b.cfg, b, RestoreOptions{App: "blog", Confirm: func(RestoreAsk) bool { return true }})
	must(t, err)
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if again.Listed == nil || !again.Listed.Equal(listed) {
		t.Errorf("after a restore: listed %v, want %v", again.Listed, listed)
	}
}

// status reads what is running out of unit names: what name makes,
// ParseUnitName reads back — an app named like a nonce included.
func TestAUnitNameIsReadBack(t *testing.T) {
	nonce, err := newNonce()
	must(t, err)
	x := &run{nonce: nonce}
	for _, tc := range []struct{ role, app string }{
		{"plan", ""}, {"listing", ""}, {"upload", "blog"}, {"clean", "a-b"}, {"fetch", "0123456789ab"}, {"upload", "abcdef"},
	} {
		role, app, ok := ParseUnitName(x.name(tc.role, tc.app))
		if !ok || role != tc.role || app != tc.app {
			t.Errorf("%s: read back as %q of %q (%v)", x.name(tc.role, tc.app), role, app, ok)
		}
	}
	for _, other := range []string{"moteld.service", "motel-backup.service", "motel_backup_test_x.service", "motel_backup_upload_Blog_0123456789ab.service", "motel_backup_upload_blog_0123456789ab.timer"} {
		if role, app, ok := ParseUnitName(other); ok {
			t.Errorf("%s was read as %q of %q", other, role, app)
		}
	}
}

// A listing that keeps failing is not a warning for ever: the record
// says since when the repository has gone unlisted, for status to tire
// of, and a listing that is answered clears it.
func TestTheRecordSaysSinceWhenListingsHaveFailed(t *testing.T) {
	b := newBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if first.Unlisted != nil {
		t.Fatalf("after a listing that was answered: unlisted %v", first.Unlisted)
	}
	b.outcome["listing"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	second, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if second.Unlisted == nil || second.Unlisted.Before(second.Started) {
		t.Fatalf("after a listing that failed: unlisted %v", second.Unlisted)
	}
	third, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if third.Unlisted == nil || !third.Unlisted.Equal(*second.Unlisted) {
		t.Fatalf("a second failure moved it: %v, was %v", third.Unlisted, second.Unlisted)
	}
	st, _, err := Drill(context.Background(), b.cfg, b)
	must(t, err)
	if st.Unlisted == nil || !st.Unlisted.Equal(*second.Unlisted) {
		t.Fatalf("a drill lost it: %v", st.Unlisted)
	}
	delete(b.outcome, "listing")
	fourth, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if fourth.Unlisted != nil {
		t.Fatalf("after a listing that was answered again: unlisted %v", fourth.Unlisted)
	}
}

// What restic said beside a listing, or of one that failed, is kept
// where root can read it and nobody else — the journal never had it,
// and the record is for everyone — until a listing is answered.
func TestWhatResticSaidOfAListingIsKeptForRoot(t *testing.T) {
	b := newBox(t)
	kept := filepath.Join(b.cfg.StateDir, "listing.err")
	b.listingErr = "Fatal: something about s3:https://bucket.example/secret-path\n"
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	got, err := os.ReadFile(kept)
	if err != nil || string(got) != b.listingErr {
		t.Fatalf("%s: %q, %v", kept, got, err)
	}
	if info, err := os.Stat(kept); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, %v", kept, info.Mode(), err)
	}
	if !strings.Contains(st.Warning, kept) || strings.Contains(st.Warning, "secret-path") {
		t.Errorf("warning: %q", st.Warning)
	}
	b.listingErr = ""
	_, err = Run(context.Background(), b.cfg, b)
	must(t, err)
	if _, err := os.Lstat(kept); err == nil {
		t.Errorf("%s is still there after a listing that was answered", kept)
	}
}

// A stderr file that cannot be read is said as that, not as restic
// having spoken.
func TestAListingWhoseStderrCannotBeReadSaysSo(t *testing.T) {
	b := newBox(t)
	b.noListingErr = true
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if st.Listed != nil {
		t.Errorf("believed all the same: listed %v", st.Listed)
	}
	if strings.Contains(st.Warning, "had something to say") || !strings.Contains(st.Warning, "could not be read") {
		t.Errorf("warning: %q", st.Warning)
	}
}

// An app that leaves the plan with snapshots in the repository — a
// block deleted, an import that stopped matching — is said by the run
// that finds it gone, once: the record drops it, and nothing after
// remembers.
func TestAnAppThatLeftThePlanIsSaidOnce(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared"), 0o755))
	both := fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["."]},"notyet":{"files":["."]}}}`, b.root)
	b.plan = both
	b.history = "[]"
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if first.Apps["blog"].LastSnapshot == nil || first.Apps["notyet"].Class != record.Pending || strings.Contains(first.Warning, "no longer") {
		t.Fatalf("fixture: %+v, warning %q", first.Apps, first.Warning)
	}
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"shop":{"files":["."]}}}`, b.root)
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if !strings.Contains(st.Warning, "blog no longer declares a backup") || !strings.Contains(st.Warning, snapA[:8]) {
		t.Errorf("warning: %q", st.Warning)
	}
	if strings.Contains(st.Warning, "notyet") || strings.Contains(st.Warning, "shop") {
		t.Errorf("an app that was never backed up, or still is, is warned about: %q", st.Warning)
	}
	st, err = Run(context.Background(), b.cfg, b)
	must(t, err)
	if strings.Contains(st.Warning, "no longer") {
		t.Errorf("said again: %q", st.Warning)
	}
}

// A run that ends early keeps, as not run, the apps it did not reach —
// never one that has left the plan: that one is said once, by the run
// that found it gone, interrupted or not.
func TestAnAppThatLeftThePlanIsNotKeptByARunThatEndsEarly(t *testing.T) {
	b := newBox(t)
	must(t, os.MkdirAll(filepath.Join(b.root, "shop", "shared"), 0o755))
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"blog":{"files":["uploads"]},"shop":{"files":["."]}}}`, b.root)
	b.history = "[]"
	_, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{"shop":{"files":["."]}}}`, b.root)
	ctx, cancel := context.WithCancel(context.Background())
	b.before = func(s unit.Spec) {
		if roleRe.FindStringSubmatch(s.Name)[1] == "upload" {
			cancel()
		}
	}
	st, _ := Run(ctx, b.cfg, b)
	if st.Error == "" || !strings.Contains(st.Warning, "blog no longer declares a backup") {
		t.Fatalf("fixture: error %q, warning %q", st.Error, st.Warning)
	}
	if st.Apps["blog"] != nil {
		t.Errorf("kept: %+v", st.Apps["blog"])
	}
	b.before = nil
	st, err = Run(context.Background(), b.cfg, b)
	must(t, err)
	if strings.Contains(st.Warning, "no longer") {
		t.Errorf("said again: %q", st.Warning)
	}
}

// With nothing left on record to look for there is nothing to list, and
// an old listing failure is no longer about anything: kept, it would
// turn status unhealthy for good over a repository nobody is asking.
func TestAnOldListingFailureDoesNotOutliveWhatItWasAbout(t *testing.T) {
	b := newBox(t)
	b.listingErr = "Fatal: no\n"
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	kept := filepath.Join(b.cfg.StateDir, "listing.err")
	if _, statErr := os.Lstat(kept); first.Unlisted == nil || statErr != nil {
		t.Fatalf("fixture: unlisted %v, %v", first.Unlisted, statErr)
	}
	b.plan = fmt.Sprintf(`{"root":%q,"apps":{}}`, b.root)
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if listings(b) != 1 {
		t.Fatalf("fixture: a run with nothing on record listed: %s", b.roles())
	}
	if st.Unlisted != nil {
		t.Errorf("unlisted %v, with nothing to list", st.Unlisted)
	}
	if _, err := os.Lstat(kept); err == nil {
		t.Errorf("%s is still there", kept)
	}
}

// A drill that fetched a snapshot whole has seen it: every name the
// record has for that snapshot says so, not only the proof's own copy —
// or a last good backup that a listing once missed stays "gone" beside
// the proof that it is there.
func TestADrillThatFetchedASnapshotHasSeenItUnderEveryName(t *testing.T) {
	b := restoreBox(t)
	first, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	listed := listedAt(t, first)
	// A listing that missed it, as the record would then stand.
	rec, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	long := listed.Add(-time.Hour)
	rec.Apps["blog"].LastOK.Seen, rec.Apps["blog"].LastSnapshot.Seen = &long, &long
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), rec))

	st, _, err := Drill(context.Background(), b.cfg, b)
	must(t, err)
	blog := st.Apps["blog"]
	if blog.RestoreProven == nil || blog.RestoreProven.Snapshot.ID != blog.LastOK.ID {
		t.Fatalf("fixture: proven %+v, last ok %+v", blog.RestoreProven, blog.LastOK)
	}
	for what, snap := range map[string]*record.Snapshot{"last_ok": blog.LastOK, "last_snapshot": blog.LastSnapshot} {
		if seenAt(t, snap).Before(listed) {
			t.Errorf("%s: fetched whole by the drill, and still last seen %v, before the listing of %v", what, snap.Seen, listed)
		}
	}
}

// A run writes a record of its own, and what the last drill said goes
// into it: a drill that could not begin is not forgotten by the next
// hourly run, with no drill since to say otherwise.
func TestARunKeepsWhatTheLastDrillSaid(t *testing.T) {
	b := restoreBox(t)
	_, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	b.err["plan"] = errors.New("the plan unit: no")
	drilled, _, err := Drill(context.Background(), b.cfg, b)
	if err == nil || drilled == nil || drilled.LastDrill == nil || drilled.LastDrill.Detail == "" {
		t.Fatalf("fixture: a drill that could not begin: %+v, %v", drilled, err)
	}
	delete(b.err, "plan")
	st, err := Run(context.Background(), b.cfg, b)
	must(t, err)
	if st.LastDrill == nil || !st.LastDrill.Time.Equal(drilled.LastDrill.Time) || st.LastDrill.Detail != drilled.LastDrill.Detail {
		t.Fatalf("after the next run: last drill %+v, want %+v", st.LastDrill, drilled.LastDrill)
	}
	again, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if again.LastDrill == nil || again.LastDrill.Detail == "" {
		t.Fatalf("the record on disk: %+v", again.LastDrill)
	}
}
