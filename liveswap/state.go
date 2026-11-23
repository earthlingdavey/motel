package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// appState is what survives a Caddy restart: enough to reattach to, or
// relaunch, the current version.
type appState struct {
	CurrentVersion string `json:"current_version"`
	// Nonce identifies the instance: it names the unit (Handle.Unit
	// carries it too) and the socket, appDirs.socket(Nonce), which is
	// derived at load rather than recorded so the two cannot disagree.
	Nonce     string      `json:"nonce"`
	Handle    handleState `json:"handle"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// stateStore persists appState; an interface so pipeline unit tests
// can run against an in-memory fake.
type stateStore interface {
	load() (appState, bool, error) // bool: state file exists
	save(appState) error
}

// fileStateStore keeps state.json next to the app's releases, written
// and read by the functions the deploy records are (ownfile.go), so a
// link at state.json, or at a name of save's temp pattern, is not
// followed, whoever made it. The app dir itself is not
// checked here (ownfile.go says why). A state.json that is a
// link, a FIFO or anything but a regular file, or larger than any
// state motel writes, is an error like a corrupt one, which
// recovery never silently resets (ensureRunning).
type fileStateStore struct {
	path string
}

// stateMaxBytes bounds a state read with the margin a record's bound
// has over a record (deployRecordMaxBytes: 1 MiB over an 8 KiB tail):
// the largest state motel writes — a version, a nonce, a unit name
// built from the two and the app's name, two timestamps — is under
// 512 bytes, so the bound refuses nothing motel wrote.
const stateMaxBytes = 64 << 10

// stateTempPattern names save's temp file (writeOwnFile).
const stateTempPattern = ".state-*.tmp"

func (s *fileStateStore) load() (appState, bool, error) {
	var st appState
	data, _, err := readOwnFile(s.path, stateMaxBytes, "a state file")
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, false, fmt.Errorf("corrupt state file %s: %w", s.path, err)
	}
	return st, true, nil
}

// save writes atomically (temp file + rename) so a crash mid-write
// never leaves a truncated state file, and never through a link
// (writeOwnFile).
func (s *fileStateStore) save(st appState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	s.removeLeftovers()
	return writeOwnFile(s.path, stateTempPattern, data)
}

// removeLeftovers removes the temp files an interrupted save leaves
// behind, by save's own pattern. os.Remove unlinks the name, never
// what a link points at. Only these names: the app dir
// holds temp names that are not save's (persistState's current.tmp).
// Saves are serialized per app — every caller of persistState holds
// deployMu — so no save is in flight to lose its temp file here. Best
// effort: a leftover that cannot be removed costs a little disk, never
// a save.
func (s *fileStateStore) removeLeftovers() {
	removeOwnLeftovers(filepath.Dir(s.path), stateTempPattern)
}

var _ stateStore = (*fileStateStore)(nil)

// Release order. Retention (gcReleases) and the list an operator picks
// a rollback target from (listReleases, the status's
// available_versions) order releases newest first by deploy time, and
// must agree, so the key and the sort are written once, here.
//
// The key is an input to what retention deletes — the way back, which
// nothing else holds — so it is held to the deploy store's rule 1
// (deploys.go): nothing the app can write decides it. A release dir's
// modification time is not such a key: the release being started is
// bound read-write into its sandbox (sandbox.go), so its pre_start and
// its instance can set that dir's mtime to anything, future included.
// The key is the release's deploy record's write time — when motel
// last deployed or rolled back to that version — read off the open
// record (readOwnFile) in a directory no app's view holds, and only
// from a record that proves itself one. Three refinements:
//
//   - In GC, the version the deploy promoted is newest — it was
//     deployed now, and its record is written as the deploy returns —
//     and the version it replaced, the way back, comes next, whatever
//     its record says: a rollback whose record write was lost (a
//     crash after the cutover, a full disk) leaves that version's
//     record at an older deploy, and it must not be pruned for that.
//     The status lists the running version first for the same reason:
//     it is the next deploy's way back, so the list and the next GC
//     agree.
//   - A release whose latest deploy failed — a rollback to it that
//     failed, or a failed deploy whose release was left on disk —
//     comes after every release whose latest deploy succeeded: that
//     deploy never served, and must take no place from one that did.
//   - A release with no such record — deployed before records
//     existed, or its record could not be written — comes after every
//     release that has one, ordered among its kind by its dir's mtime,
//     and the status names it (ordered_by_mtime). That mtime is the
//     app's to move, so it decides nothing among recorded releases:
//     such a release is the first GC candidate after the two held by
//     rank, and a rollback target while it is on disk.
//
// Deploy times that tie order by name, so the order is the same on
// every read.
type release struct {
	name    string
	at      time.Time
	failed  bool // its latest deploy failed
	byMtime bool // no record: at is the dir's mtime
	// notRelease: a dir under releases/ whose name is not a version.
	// Nothing could deploy it or roll back to it, and the status does
	// not list it, so GC sorts it last and it holds no keep place.
	notRelease bool
}

// placeRelease is e's place in the order, from its record in records
// (the records dir, or "" for none), else from its dir's mtime; false
// when the dir itself cannot be read. err is a record that is there
// and cannot be read (recordedDeploy): the release is placed by mtime
// like one with none, for the status to name, and GC prunes nothing.
func placeRelease(records string, e os.DirEntry) (release, bool, error) {
	at, failed, ok, err := recordedDeploy(records, e.Name())
	if ok {
		return release{name: e.Name(), at: at, failed: failed}, true, nil
	}
	info, ierr := e.Info()
	if ierr != nil {
		return release{}, false, err
	}
	return release{name: e.Name(), at: info.ModTime(), byMtime: true}, true, err
}

// newestFirst sorts rels into the order above, the names in first
// (those not "") ahead of every other, in the order given.
func newestFirst(rels []release, first ...string) {
	rank := func(name string) int {
		for i, f := range first {
			if f != "" && name == f {
				return i
			}
		}
		return len(first)
	}
	// Recorded and succeeded, then recorded and failed, then no record,
	// then (in GC) a dir whose name is not a version.
	tier := func(r release) int {
		switch {
		case r.notRelease:
			return 3
		case r.byMtime:
			return 2
		case r.failed:
			return 1
		}
		return 0
	}
	sort.Slice(rels, func(i, j int) bool {
		if ri, rj := rank(rels[i].name), rank(rels[j].name); ri != rj {
			return ri < rj
		}
		if ti, tj := tier(rels[i]), tier(rels[j]); ti != tj {
			return ti < tj
		}
		if !rels[i].at.Equal(rels[j].at) {
			return rels[i].at.After(rels[j].at)
		}
		return rels[i].name > rels[j].name
	})
}

// orderRecords is the records dir the order reads: "" when there is
// none yet (no record was ever written, so every release is placed by
// its dir's mtime), or why it cannot be read — refused as a planted
// link (recordsDir), or unreadable.
func orderRecords(d appDirs) (string, error) {
	dir, err := recordsDir(d, false)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return dir, err
}

// listReleases returns the on-disk release versions in the order above,
// and byMtime, those of them placed by their dir's mtime, in the same
// order. The status then puts the running version first (runningFirst).
// Best-effort: a read error yields nil and status still renders. A
// release is a directory named as a version, nothing else (gcReleases
// enumerates more loosely: it also removes orphans).
func listReleases(d appDirs) (versions, byMtime []string) {
	entries, err := os.ReadDir(d.releases)
	if err != nil {
		return nil, nil
	}
	// Records that cannot be read place their releases by mtime, and
	// the status names each so; only GC must refuse to act on that.
	records, _ := orderRecords(d)
	var rels []release
	for _, e := range entries {
		// A directory whose name is not a version is not a release:
		// nothing could deploy it, roll back to it, or record it
		// (validVersion). Dotfiles and staging dirs are among
		// what the alphabet refuses.
		if !e.IsDir() || !validVersion(e.Name()) {
			continue
		}
		if r, ok, _ := placeRelease(records, e); ok { // unreadable: by mtime, named
			rels = append(rels, r)
		}
	}
	newestFirst(rels)
	versions = make([]string, len(rels))
	for i, r := range rels {
		versions[i] = r.name
		if r.byMtime {
			byMtime = append(byMtime, r.name)
		}
	}
	return versions, byMtime
}

// runningFirst moves the running version to the front of list, in
// place, the rest keeping their order: it is what the next deploy's GC
// keeps beside the new one, so the list an operator picks a rollback
// target from is the list GC keeps from. The status applies it under
// the lock that gives current_version, so a cutover between its disk
// read and that lock cannot leave the two disagreeing.
func runningFirst(list []string, running string) []string {
	if i := slices.Index(list, running); i > 0 {
		copy(list[1:i+1], list[:i])
		list[0] = running
	}
	return list
}

// The staged artifacts in tmp/: a URL deploy's download
// (downloadArtifact) and a push's upload (stageUpload), by the
// patterns os.CreateTemp takes.
const (
	artifactTempPattern = "artifact-*.tar.gz"
	pushTempPattern     = "push-*.tar.gz"
)

// removeStagedArtifacts removes the staged artifacts left in tmp/. The
// deploy that stages one removes it as it ends; one a moteld that
// stopped or died mid-transfer left is nobody's, and tmp/ is in no
// app's view, so nothing else would, and the next upload could fail
// on the disk it holds. Called at recovery's settle, under deployMu,
// before any deploy of the start could stage: never beside one.
func removeStagedArtifacts(d appDirs) {
	for _, pattern := range []string{artifactTempPattern, pushTempPattern} {
		removeOwnLeftovers(d.tmp, pattern)
	}
}

// gcReleases prunes the releases directory down to the newest keep
// releases in the order above: protect — the version this deploy
// promoted, now serving — first, so it is never deleted and holds one
// of the keep places, then previous — the version it replaced, the
// way back; "" when none, or protect itself on a relaunch. Crash
// orphans of extraction are always removed, and so is a dir whose name
// is not a version: it holds no keep place, so it never takes the
// place of a version the status lists. No release is deleted, and
// GC says why, when the app dir or releases/ is not the directory it
// names (linkAt: through a link it would delete another app's
// releases; the deploy's writeRefusal checked the same before it
// wrote, and this is the check at the deletion itself) or the records
// dir cannot be read (the whole order would then be one the app can
// set). A release whose own record is there but cannot be read is
// held: neither deleted nor counted toward keep, and named, so the
// rest are pruned in an order the app cannot set. Failures are
// logged, not fatal: GC must never break a deploy that already
// succeeded.
func gcReleases(d appDirs, keep int, protect, previous string, logger *zap.Logger) {
	if p, why := d.linkAt(d.releases); p != "" {
		logger.Warn("release GC: not pruning: the app dir or releases/ is not the directory it names",
			zap.String("app_dir", d.app), zap.String("path", p), zap.Error(why),
			zap.String("fix", notOwnDirFix))
		return
	}
	entries, err := os.ReadDir(d.releases)
	if err != nil {
		logger.Warn("release GC: cannot list releases", zap.Error(err))
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".extract-") {
			continue
		}
		// A staging dir still present here is a crash orphan: deploys
		// are serialized per app (deployMu), and the deploy running this
		// GC renamed its own staging away before the call. Left alone
		// they accumulate forever, invisible — hidden by the same
		// dot-prefix that exempts them below.
		if err := os.RemoveAll(filepath.Join(d.releases, e.Name())); err != nil {
			logger.Warn("release GC: cannot remove orphaned staging dir",
				zap.String("dir", e.Name()), zap.Error(err))
		} else {
			logger.Info("release GC: removed orphaned staging dir",
				zap.String("dir", e.Name()))
		}
	}
	records, err := orderRecords(d)
	if err != nil {
		logger.Error("release GC: not pruning: the deploy records directory cannot be read, and without it the order is one the app can set", zap.Error(err))
		return
	}
	var rels []release
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // dotfile strays (and the staging dirs above) are not releases
		}
		if !validVersion(e.Name()) {
			rels = append(rels, release{name: e.Name(), notRelease: true})
			continue
		}
		r, ok, err := placeRelease(records, e)
		if err != nil {
			logger.Error("release GC: holding a release whose deploy record cannot be read", zap.String("version", e.Name()), zap.Error(err))
			continue
		}
		if ok {
			rels = append(rels, r)
		}
	}
	newestFirst(rels, protect, previous)
	for i, r := range rels {
		if (i < keep && !r.notRelease) || r.name == protect {
			continue
		}
		placedBy := "deploy record"
		switch {
		case r.notRelease:
			placedBy = "not a version name"
		case r.byMtime:
			placedBy = "mtime"
		}
		if err := os.RemoveAll(filepath.Join(d.releases, r.name)); err != nil {
			logger.Warn("release GC: cannot remove old release",
				zap.String("version", r.name), zap.String("placed_by", placedBy), zap.Error(err))
			continue
		}
		logger.Info("release GC: removed old release", zap.String("version", r.name), zap.String("placed_by", placedBy))
	}
}
