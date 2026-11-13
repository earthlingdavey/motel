package liveswap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Rule 4 (redact.go): a directory under releases/ is a release only
// under a version's name. Anything else — a staging dir, a dotfile, a
// name nothing could deploy or roll back to — is not listed, so it
// is neither offered to an operator nor put on the filter's safe list.
func TestListReleasesSkipsNamesThatAreNotVersions(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir := d.releases
	for _, n := range []string{"v1", "2026.09.14", ".extract-abc", "a b", "..x", strings.Repeat("v", 65)} {
		must(t, os.MkdirAll(filepath.Join(dir, n), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(dir, "v9"), nil, 0o600)) // a file, not a release
	got, _ := listReleases(d)
	sort.Strings(got)
	if strings.Join(got, ",") != "2026.09.14,v1" {
		t.Fatalf("listReleases = %v", got)
	}
}

// TestReleasesIsListReleasesForAConfiguredApp: what notify's silence
// --release takes; nothing for an app the config does not run.
func TestReleasesIsListReleasesForAConfiguredApp(t *testing.T) {
	root := t.TempDir()
	d := newAppDirs(root, "demo")
	must(t, os.MkdirAll(filepath.Join(d.releases, "v1"), 0o755))
	a := &App{Root: root, specs: map[string]*appSpec{"demo": {}}}
	if got := a.Releases("demo"); strings.Join(got, ",") != "v1" {
		t.Fatalf("Releases(demo) = %v", got)
	}
	if got := a.Releases("other"); got != nil {
		t.Fatalf("Releases(other) = %v", got)
	}
}

func TestFileStateStoreRoundTrip(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "app", "state.json")}

	if _, ok, err := store.load(); ok || err != nil {
		t.Fatalf("missing file must be (ok=false, err=nil), got ok=%v err=%v", ok, err)
	}

	want := appState{
		CurrentVersion: "v3",
		Nonce:          "0a1b2c3d0a1b2c3d",
		Handle: handleState{
			PID:       999,
			Command:   []string{"/usr/bin/node", "server.js"},
			StartedAt: time.Unix(1_700_000_000, 0).UTC(),
			Unit:      "motel-blog.v3.0a1b2c3d0a1b2c3d.service",
		},
		UpdatedAt: time.Unix(1_700_000_100, 0).UTC(),
	}
	must(t, store.save(want))
	got, ok, err := store.load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.CurrentVersion != want.CurrentVersion || got.Nonce != want.Nonce || got.Handle.Unit != want.Handle.Unit {
		t.Fatalf("round trip mismatch: %+v != %+v", got, want)
	}
	if !got.Handle.StartedAt.Equal(want.Handle.StartedAt) {
		t.Fatalf("started_at must survive the round trip: %s != %s", got.Handle.StartedAt, want.Handle.StartedAt)
	}
	// Only what Reattach needs is recorded. PID and Command are read
	// live from the manager and would be stale the moment they were
	// written, so they must not reach the file at all — asserted on
	// the bytes, because a json:"-" that is dropped still round-trips
	// as a zero value and would look identical from load() alone.
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pid", "command", "node", "999"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("state.json must not record %q; it holds:\n%s", key, raw)
		}
	}
	if got.Handle.PID != 0 || got.Handle.Command != nil {
		t.Fatalf("a loaded record must carry no pid or command, got pid=%d command=%v", got.Handle.PID, got.Handle.Command)
	}
	// No stray temp file left behind: state.json is all there is.
	assertOnlyEntries(t, filepath.Dir(store.path), "state.json")
	if fi, err := os.Lstat(store.path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json must be a regular file, mode 0600: %v %v", fi, err)
	}
}

// assertOnlyEntries fails unless dir holds exactly the names given.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	sort.Strings(names)
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("%s holds %v, want %v", dir, got, names)
	}
}

// The state at its largest — the longest version and app name, both
// timestamps with nanoseconds and a zone — sits far under the read
// bound, so the bound refuses nothing motel wrote; and it loads.
func TestStateMaxBytesBoundsTheLargestState(t *testing.T) {
	version := strings.Repeat("v", 64)
	app := strings.Repeat("a", appNameMaxLen)
	if !validVersion(version) || !appNameRe.MatchString(app) {
		t.Fatal("the fixture must be the longest valid version and app name")
	}
	stamp := time.Date(2026, 10, 10, 12, 34, 56, 123456789, time.FixedZone("", -34200)) // -09:30: a zone, with minutes
	st := appState{
		CurrentVersion: version,
		Nonce:          "0a1b2c3d0a1b2c3d",
		Handle:         handleState{StartedAt: stamp, Unit: unitPrefix + app + "." + version + ".0a1b2c3d0a1b2c3d.service"},
		UpdatedAt:      stamp,
	}
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	must(t, store.save(st))
	fi, err := os.Stat(store.path)
	must(t, err)
	if fi.Size() >= 512 { // what stateMaxBytes's margin is reckoned against
		t.Fatalf("the largest state is %d bytes; stateMaxBytes (%d) assumes under 512", fi.Size(), stateMaxBytes)
	}
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != version {
		t.Fatalf("the largest state must load: ok=%v err=%v", ok, err)
	}
}

// Rule 4 (deploys.go) for state.json's temp file: a link at a name of
// save's own temp pattern must not have the next save write through
// it. Save succeeds, the target is untouched, and the link is gone.
func TestFileStateStoreSaveDoesNotWriteThroughAPlantedTempLink(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	must(t, os.MkdirAll(app, 0o750))
	victim := filepath.Join(root, "victim.txt") // outside the app dir
	must(t, os.WriteFile(victim, []byte("precious"), 0o600))
	must(t, os.Symlink(victim, filepath.Join(app, ".state-planted.tmp")))
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Fatalf("a planted link's target was written through: %q %v", b, err)
	}
	assertOnlyEntries(t, app, "state.json")
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != "v1" {
		t.Fatalf("load after save: %+v ok=%v err=%v", got, ok, err)
	}
}

// A state.json that is a link is refused, not followed — even to a
// state that would load — and the refusal names the file and why. The
// next save replaces the link itself, never its target.
func TestFileStateStoreLoadRefusesALink(t *testing.T) {
	root := t.TempDir()
	elsewhere := &fileStateStore{path: filepath.Join(root, "other", "state.json")}
	must(t, elsewhere.save(appState{CurrentVersion: "v9", Nonce: "0a1b2c3d0a1b2c3d"}))
	before, err := os.ReadFile(elsewhere.path)
	must(t, err)
	app := filepath.Join(root, "app")
	must(t, os.MkdirAll(app, 0o750))
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, os.Symlink(elsewhere.path, store.path))

	_, ok, err := store.load()
	if err == nil || ok {
		t.Fatalf("a linked state.json was followed: ok=%v err=%v", ok, err)
	}
	if msg := err.Error(); !strings.Contains(msg, store.path) || !strings.Contains(msg, "is a link") {
		t.Fatalf("the refusal must name the file and say it is a link: %v", err)
	}

	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	if after, err := os.ReadFile(elsewhere.path); err != nil || string(after) != string(before) {
		t.Fatalf("save wrote through the link: %s %v", after, err)
	}
	if fi, err := os.Lstat(store.path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("save must replace the link with a regular file: %v %v", fi, err)
	}
}

// A FIFO at state.json must not hold the open — and with it recovery,
// and the deploy lock it holds — for good: the open does not block on
// it, and it is refused as not a regular file.
func TestFileStateStoreLoadDoesNotBlockOnAFIFO(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	must(t, syscall.Mkfifo(store.path, 0o600))
	done := make(chan error, 1)
	go func() {
		_, _, err := store.load()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), store.path) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a FIFO at state.json must be refused by name: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a planted FIFO blocked the state store")
	}
}

// A state.json larger than any state motel writes is refused, and
// not read whole into memory; one at the bound still loads.
func TestFileStateStoreLoadRefusesAnOversizedFile(t *testing.T) {
	store := &fileStateStore{path: filepath.Join(t.TempDir(), "state.json")}
	padded := func(size int) []byte { // a state that would load, padded with an unknown field
		head := `{"current_version":"v1","nonce":"0a1b2c3d0a1b2c3d","pad":"`
		return []byte(head + strings.Repeat("x", size-len(head)-2) + `"}`)
	}
	must(t, os.WriteFile(store.path, padded(stateMaxBytes+1), 0o600))
	_, ok, err := store.load()
	if err == nil || ok || !strings.Contains(err.Error(), store.path) || !strings.Contains(err.Error(), "larger than a state file can be") {
		t.Fatalf("an oversized state.json must be refused by name: ok=%v err=%v", ok, err)
	}
	must(t, os.WriteFile(store.path, padded(stateMaxBytes), 0o600))
	if got, ok, err := store.load(); err != nil || !ok || got.CurrentVersion != "v1" {
		t.Fatalf("a state.json at the bound must load: ok=%v err=%v", ok, err)
	}
}

// A save removes what an interrupted one left: a temp file of its own
// pattern, and nothing else that happens to end in .tmp
// (persistState's current.tmp is its own).
func TestFileStateStoreSaveRemovesLeftovers(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	must(t, os.MkdirAll(app, 0o750))
	for _, n := range []string{".state-123456.tmp", "other.tmp"} {
		must(t, os.WriteFile(filepath.Join(app, n), []byte("{"), 0o600))
	}
	must(t, os.Symlink("releases/v1", filepath.Join(app, "current.tmp")))
	must(t, os.Mkdir(filepath.Join(app, ".state-dir.tmp"), 0o750)) // a directory is not a temp file of save's
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d"}))
	assertOnlyEntries(t, app, "state.json", "other.tmp", "current.tmp", ".state-dir.tmp")
}

// A save that fails after its temp file exists removes the temp file:
// here the rename, over a directory standing at state.json.
func TestFileStateStoreFailedSaveLeavesNoTempFile(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	store := &fileStateStore{path: filepath.Join(app, "state.json")}
	must(t, os.MkdirAll(filepath.Join(store.path, "x"), 0o750))
	if err := store.save(appState{CurrentVersion: "v1"}); err == nil {
		t.Fatal("a save over a directory must fail")
	}
	assertOnlyEntries(t, app, "state.json")
}

// Recovery reads state.json through the store's rules: a link, a FIFO
// or an oversized file there is a permanent recovery error — never a
// silent reset, never a launch — and the journal line names the file
// and the reason.
func TestRecoveryRefusesAPlantedStateFile(t *testing.T) {
	cases := []struct {
		name, reason string
		plant        func(t *testing.T, path string)
	}{
		{"link", "is a link", func(t *testing.T, path string) {
			elsewhere := &fileStateStore{path: filepath.Join(shortTempDir(t), "state.json")}
			must(t, elsewhere.save(appState{CurrentVersion: "v7", Nonce: recordedNonce}))
			must(t, os.Symlink(elsewhere.path, path))
		}},
		{"fifo", "not a regular file", func(t *testing.T, path string) {
			must(t, syscall.Mkfifo(path, 0o600))
		}},
		{"oversized", "larger than a state file can be", func(t *testing.T, path string) {
			must(t, os.WriteFile(path, []byte(`{"current_version":"v7","pad":"`+strings.Repeat("x", stateMaxBytes)+`"}`), 0o600))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.ma.store = &fileStateStore{path: rig.spec.dirs.state}
			must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
			tc.plant(t, rig.spec.dirs.state)
			core, logs := observer.New(zap.ErrorLevel)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { rig.ma.recover(ctx, zap.New(core)); close(done) }()
			// Should a regression block on the FIFO or retry, the
			// goroutine would outlive this test holding — or coming
			// back for — ownerLock("demo"), every rig's, and hang some
			// later test instead of failing this one. Release it here:
			// cancel ends a retry loop, and a writer's open lets a
			// reader blocked in its own open return, to the EOF of the
			// writer's close.
			t.Cleanup(func() {
				cancel()
				if fi, err := os.Lstat(rig.spec.dirs.state); err == nil && fi.Mode()&os.ModeNamedPipe != 0 {
					if w, err := os.OpenFile(rig.spec.dirs.state, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
						if err := w.Close(); err != nil {
							t.Error(err) // not Fatal: the wait below must still run
						}
					}
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("the recovery goroutine is still running after the test")
				}
			})
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery must give up on a planted state.json, not block or retry")
			}
			entries := logs.FilterMessage("recovery failed").All()
			if len(entries) != 1 {
				t.Fatalf("want one recovery failed line, got %v", logs.All())
			}
			msg, _ := entries[0].ContextMap()["error"].(string)
			if !strings.Contains(msg, rig.spec.dirs.state) || !strings.Contains(msg, tc.reason) {
				t.Fatalf("the journal must name the file and %q: %q", tc.reason, msg)
			}
			if rig.runner.startCount() != 0 || len(rig.runner.reattachSeen) != 0 {
				t.Fatalf("a refused state.json reached the runner: starts=%d reattach=%+v", rig.runner.startCount(), rig.runner.reattachSeen)
			}
		})
	}
}

func TestFileStateStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	must(t, os.WriteFile(path, []byte("{nope"), 0o600))
	store := &fileStateStore{path: path}
	if _, _, err := store.load(); err == nil {
		t.Fatal("corrupt state must error, not silently reset")
	}
}

// The release-order ledger. Retention deletes the way back, which
// nothing else holds, so for a fixed set of releases — with and
// without deploy records, under directory mtimes the app may have set
// to anything — each row spells out exactly what available_versions
// lists (newest first), which entries it names as placed by mtime, and
// exactly what release GC leaves on disk.
func TestReleaseOrderLedger(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(minute int) time.Time { return base.Add(time.Duration(minute) * time.Minute) }
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	epoch := time.Unix(0, 0)
	type rel struct {
		name string
		// record is the outcome its deploy record holds: "" for none,
		// "forged" for a file under its name that names another version,
		// "unreadable" for something there that cannot be read as one.
		record string
		rec    time.Time // when motel wrote the record
		dir    time.Time // the release dir's mtime: the app's to set
	}
	cases := []struct {
		name        string
		rels        []rel
		linkRecords bool // deploys/ is a planted link: no record is read through it
		keep        int
		protect     string // the version the deploy running GC promoted
		previous    string // the version it replaced: running until the cutover, so the list has it first
		available   string // listReleases, newest first
		byMtime     string // the entries it names as placed by mtime
		kept        string // on disk after GC, by name
	}{
		{
			name: "every release recorded: directory mtimes count for nothing",
			rels: []rel{
				{"v1", "succeeded", at(1), future},
				{"v2", "succeeded", at(2), epoch},
				{"v3", "succeeded", at(3), at(100)},
				{"v4", "succeeded", at(4), epoch},
			},
			keep: 2, protect: "v4",
			available: "v4 v3 v2 v1", kept: "v3 v4",
		},
		{
			name: "the version just deployed has no record until the deploy returns: GC puts it first whatever its dir says",
			rels: []rel{
				{"v1", "succeeded", at(1), at(1)},
				{"v2", "succeeded", at(2), future},
				{"v3", "", time.Time{}, epoch}, // its pre_start or instance set it
			},
			keep: 2, protect: "v3",
			available: "v2 v1 v3", byMtime: "v3", kept: "v2 v3",
		},
		{
			name: "a rollback: the version rolled back to is this deploy's, so newest; keep counts it",
			rels: []rel{
				{"v1", "succeeded", at(1), epoch},
				{"v2", "succeeded", at(2), future},
				{"v3", "succeeded", at(3), at(3)},
			},
			keep: 2, protect: "v1",
			available: "v3 v2 v1", kept: "v1 v3",
		},
		{
			name: "a release whose latest deploy failed comes after every one whose latest succeeded",
			rels: []rel{
				{"v1", "failed", at(5), future}, // rolled back to after v3, and failed
				{"v2", "succeeded", at(2), at(2)},
				{"v3", "succeeded", at(3), at(3)}, // served until v4
				{"v4", "", time.Time{}, at(6)},    // this deploy's
			},
			keep: 2, protect: "v4",
			available: "v3 v2 v1 v4", byMtime: "v4", kept: "v3 v4",
		},
		{
			name: "a failed rollback does not push the version that served out of keep",
			rels: []rel{
				{"v1", "failed", at(3), at(1)},    // the rollback to it failed
				{"v2", "succeeded", at(2), at(2)}, // served throughout
				{"v3", "", time.Time{}, at(4)},    // this deploy's
			},
			keep: 2, protect: "v3",
			available: "v2 v1 v3", byMtime: "v3", kept: "v2 v3",
		},
		{
			name: "a rollback whose record write was lost: the version it put in service is kept next to the new one",
			rels: []rel{
				{"v1", "succeeded", at(1), at(1)}, // rolled back to after v2; that record never landed
				{"v2", "succeeded", at(2), at(2)},
				{"v3", "", time.Time{}, at(4)}, // this deploy's, replacing v1
			},
			keep: 2, protect: "v3", previous: "v1",
			available: "v1 v2 v3", byMtime: "v3", kept: "v1 v3",
		},
		{
			name: "a record that is there but cannot be read: listed by mtime and named; GC holds its release uncounted and prunes the rest",
			rels: []rel{
				{"v1", "succeeded", at(1), at(1)},
				{"v2", "unreadable", at(5), at(2)},
				{"v3", "succeeded", at(3), at(3)},
			},
			keep: 1, protect: "v3", previous: "v2",
			available: "v2 v3 v1", byMtime: "v2", kept: "v2 v3",
		},
		{
			name: "a release with no record (deployed before records existed) sorts after every recorded one, and is named",
			rels: []rel{
				{"v0", "", time.Time{}, at(0)},
				{"v1", "succeeded", at(1), future},
				{"v2", "succeeded", at(2), epoch},
			},
			keep: 2, protect: "v2",
			available: "v2 v1 v0", byMtime: "v0", kept: "v1 v2",
		},
		{
			name: "no record and a 2099 mtime outranks no recorded release: it is the first GC candidate after the two held",
			rels: []rel{
				{"v0", "", time.Time{}, future},
				{"v1", "succeeded", at(1), at(1)},
				{"v2", "succeeded", at(2), at(2)}, // served until this deploy
				{"v3", "", time.Time{}, at(3)},    // this deploy's
			},
			keep: 3, protect: "v3", previous: "v2",
			available: "v2 v1 v0 v3", byMtime: "v0 v3", kept: "v1 v2 v3",
		},
		{
			name: "the fallback's limit: among releases with no record, the dir mtime the app can set decides which goes first",
			rels: []rel{
				{"va", "", time.Time{}, at(1)},
				{"vb", "", time.Time{}, future}, // whatever its age, its instance set 2099
				{"v2", "succeeded", at(2), at(2)},
			},
			keep: 2, protect: "v2",
			available: "v2 vb va", byMtime: "vb va", kept: "v2 vb",
		},
		{
			name: "a dir whose name is not a version holds no keep place, whatever its mtime: a listed version is never pruned for it",
			rels: []rel{
				{"v0", "", time.Time{}, at(0)}, // listed, by mtime
				{"v1", "succeeded", at(1), at(1)},
				{"v2", "succeeded", at(2), at(2)},
				{"old copy", "", time.Time{}, future}, // not a version: never listed
			},
			keep: 3, protect: "v2", previous: "v1",
			available: "v1 v2 v0", byMtime: "v0", kept: "v0 v1 v2",
		},
		{
			name: "a record that does not prove itself one counts as none",
			rels: []rel{
				{"v1", "forged", future, at(1)},
				{"v2", "succeeded", at(2), epoch},
			},
			keep: 2, protect: "v2",
			available: "v2 v1", byMtime: "v1", kept: "v1 v2",
		},
		{
			name: "a planted link at deploys/: nothing is read through it, every release is listed by mtime, and GC prunes nothing",
			rels: []rel{
				{"v1", "succeeded", at(2), at(1)},
				{"v2", "succeeded", at(1), at(2)},
				{"v3", "succeeded", at(3), at(3)},
			},
			linkRecords: true,
			keep:        2, protect: "v3",
			available: "v3 v2 v1", byMtime: "v3 v2 v1", kept: "v1 v2 v3",
		},
		{
			name: "deploy times that tie order by name, the same on every read",
			rels: []rel{
				{"v1", "succeeded", at(1), future},
				{"v2", "succeeded", at(1), epoch},
				{"v3", "succeeded", at(1), at(1)},
			},
			keep: 2, protect: "v1",
			available: "v3 v2 v1", kept: "v1 v3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newAppDirs(t.TempDir(), "demo")
			for _, r := range tc.rels {
				must(t, os.MkdirAll(d.release(r.name), 0o750))
				switch r.record {
				case "":
				case "forged":
					must(t, writeDeployRecord(d, r.name, []byte(`{"version":"other","status":"succeeded"}`)))
				case "unreadable": // a directory at the record's name: open, stat, not a regular file
					_, err := recordsDir(d, true)
					must(t, err)
					must(t, os.Mkdir(deployRecordPath(d.deploys, r.name), 0o750))
				default:
					must(t, writeDeployRecord(d, r.name, []byte(`{"version":"`+r.name+`","status":"`+r.record+`"}`)))
				}
				if r.record != "" {
					stampRecord(t, d, r.name, r.rec)
				}
			}
			if tc.linkRecords {
				elsewhere := filepath.Join(t.TempDir(), "deploys")
				must(t, os.Rename(d.deploys, elsewhere))
				must(t, os.Symlink(elsewhere, d.deploys))
			}
			for _, r := range tc.rels {
				must(t, os.Chtimes(d.release(r.name), r.dir, r.dir))
			}

			available, byMtime := listReleases(d)
			available, byMtime = runningFirst(available, tc.previous), runningFirst(byMtime, tc.previous) // as the status does
			if got := strings.Join(available, " "); got != tc.available {
				t.Errorf("available_versions = %q, want %q", got, tc.available)
			}
			if got := strings.Join(byMtime, " "); got != tc.byMtime {
				t.Errorf("ordered_by_mtime = %q, want %q", got, tc.byMtime)
			}

			gcReleases(d, tc.keep, tc.protect, tc.previous, zap.NewNop())
			onDisk, err := releaseNames(d.releases)
			must(t, err)
			sort.Strings(onDisk)
			if got := strings.Join(onDisk, " "); got != tc.kept {
				t.Fatalf("kept %q, want %q", got, tc.kept)
			}
			// What is left lists in the order it had: GC removes, and
			// reorders nothing.
			after, _ := listReleases(d)
			after = runningFirst(after, tc.previous)
			var want []string
			for _, v := range available {
				if slices.Contains(onDisk, v) {
					want = append(want, v)
				}
			}
			if !slices.Equal(after, want) {
				t.Fatalf("available_versions after GC = %v, want %v", after, want)
			}
		})
	}
}

// Release GC deletes only in the directory it names: with a link at
// the app dir (`demo -> shop`) or at releases/, demo's GC would remove
// shop's releases, so it prunes nothing and says why.
func TestGCReleasesDoesNotPruneThroughALink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, demo, shop appDirs)
	}{
		{"app dir", func(t *testing.T, demo, shop appDirs) {
			must(t, os.Symlink("shop", demo.app))
		}},
		{"releases", func(t *testing.T, demo, shop appDirs) {
			must(t, os.MkdirAll(demo.app, 0o750))
			must(t, os.Symlink(shop.releases, demo.releases))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			shop, demo := newAppDirs(root, "shop"), newAppDirs(root, "demo")
			for _, v := range []string{"v1", "v2", "v3"} {
				must(t, os.MkdirAll(shop.release(v), 0o750))
			}
			tc.plant(t, demo, shop)
			core, logs := observer.New(zap.WarnLevel)
			gcReleases(demo, 1, "v3", "", zap.New(core))
			onDisk, err := releaseNames(shop.releases)
			must(t, err)
			sort.Strings(onDisk)
			if got := strings.Join(onDisk, " "); got != "v1 v2 v3" {
				t.Fatalf("shop's releases after demo's GC: %s", got)
			}
			got := logs.FilterMessageSnippet("release GC: not pruning").All()
			if len(got) != 1 || !strings.Contains(fmt.Sprint(got[0].ContextMap()["fix"]), "bind-mount") {
				t.Fatalf("the refusal must be logged once, with the remedy: %v", logs.All())
			}
		})
	}
}

// A deploy record that is there but cannot be read is not a missing
// one: GC holds its release — not deleted, not counted toward keep —
// names it, and prunes the rest in the order the readable records
// give. A records directory that cannot be read leaves no order at
// all: no release is deleted. Either way a crash's staging dir goes.
func TestGCReleasesHoldsWhatItCannotOrder(t *testing.T) {
	setup := func(t *testing.T) appDirs {
		d := newAppDirs(t.TempDir(), "demo")
		for _, v := range []string{"v0", "v1", "v2", "v3"} {
			must(t, os.MkdirAll(d.release(v), 0o750))
			must(t, writeDeployRecord(d, v, []byte(`{"version":"`+v+`","status":"succeeded"}`)))
		}
		must(t, os.Mkdir(filepath.Join(d.releases, ".extract-x"), 0o750))
		return d
	}
	left := func(t *testing.T, d appDirs) string {
		entries, err := os.ReadDir(d.releases)
		must(t, err)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return strings.Join(names, " ")
	}

	t.Run("one record unreadable", func(t *testing.T) {
		d := setup(t)
		must(t, os.Remove(deployRecordPath(d.deploys, "v1")))
		must(t, os.Mkdir(deployRecordPath(d.deploys, "v1"), 0o750))
		core, logs := observer.New(zap.ErrorLevel)
		gcReleases(d, 2, "v3", "v2", zap.New(core))
		if got := left(t, d); got != "v1 v2 v3" {
			t.Fatalf("left %q, want v1 (held) v2 v3: v0 pruned, the staging dir removed", got)
		}
		got := logs.FilterMessageSnippet("holding a release").All()
		if len(got) != 1 || got[0].ContextMap()["version"] != "v1" {
			t.Fatalf("want one line holding v1: %v", logs.All())
		}
	})
	t.Run("records directory refused", func(t *testing.T) {
		d := setup(t)
		elsewhere := filepath.Join(t.TempDir(), "deploys")
		must(t, os.Rename(d.deploys, elsewhere))
		must(t, os.Symlink(elsewhere, d.deploys))
		core, logs := observer.New(zap.ErrorLevel)
		gcReleases(d, 1, "v3", "v2", zap.New(core))
		if got := left(t, d); got != "v0 v1 v2 v3" {
			t.Fatalf("left %q, want every release and no staging dir", got)
		}
		if logs.FilterMessageSnippet("records directory cannot be read").Len() != 1 {
			t.Fatalf("want one line naming the records directory: %v", logs.All())
		}
	})
}

func TestGCReleases(t *testing.T) {
	d := newAppDirs(t.TempDir(), "demo")
	dir := d.releases
	base := time.Now().Add(-time.Hour)
	// v1 oldest ... v4 newest (no records: placed by mtime), plus a
	// staging dir and a stray file.
	for i, v := range []string{"v1", "v2", "v3", "v4"} {
		p := filepath.Join(dir, v)
		must(t, os.MkdirAll(p, 0o755))
		mt := base.Add(time.Duration(i) * time.Minute)
		must(t, os.Chtimes(p, mt, mt))
	}
	must(t, os.MkdirAll(filepath.Join(dir, ".extract-v5"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o600))

	// keep 2, protect the OLDEST (a rollback to v1): v1 is this
	// deploy's, so newest, and holds one of the two places.
	gcReleases(d, 2, "v1", "", zap.NewNop())

	for _, v := range []string{"v1", "v4"} {
		if _, err := os.Stat(filepath.Join(dir, v)); err != nil {
			t.Errorf("%s should survive: %v", v, err)
		}
	}
	for _, v := range []string{"v2", "v3"} {
		if _, err := os.Stat(filepath.Join(dir, v)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", v)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".extract-v5")); !os.IsNotExist(err) {
		t.Error("orphaned staging dir should have been removed (deploys are serialized, so any .extract-* seen by GC is a crash leftover)")
	}
	if _, err := os.Stat(filepath.Join(dir, ".hidden")); err != nil {
		t.Error("non-staging dotdirs are not GC's business")
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Error("plain files are not GC's business")
	}
}
