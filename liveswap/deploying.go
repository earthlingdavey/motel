package liveswap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"go.uber.org/zap"
)

// The deploy marker, <app>/deploying.json: a URL or push deploy in
// flight. Such a deploy extracts a release that it removes again if
// it fails; a moteld that stops during it (a crash, a restart, an
// upgrade) removes nothing, and the release would stay under the
// version's name — a redeploy refused as "already exists", a rollback
// launching it without its pre_start (#318). The marker says which
// release that is. Its rules (DESIGN-liveswap.md):
//
//   - Written before the deploy extracts, and only by a deploy that
//     extracts: a rollback or a relaunch launches a release already
//     on disk, which nothing here may remove.
//   - Removed once the deploy's record is written, whatever the deploy
//     came to, so a marker that outlives its deploy is one no motel
//     finished, or one whose removal failed after its record was
//     written — told apart by the record's start time.
//   - Settled (settleInterrupted) only after a confirmed sweep, by
//     recovery and by the next deploy, each holding deployMu: nothing
//     runs out of the release by then, and no deploy writes a marker
//     meanwhile. Settling never refuses anything: what it cannot tell
//     or cannot remove ends as a failed deploy's release left on disk
//     does, and the marker is removed either way (a removal that
//     fails is logged), so one slot never holds two deploys.
//
// It lives beside state.json, outside every view, written and read
// under ownfile.go's rules.
type deployMarker struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// deployMarkerMaxBytes bounds a marker read: one version and one
// timestamp are under 200 bytes.
const deployMarkerMaxBytes = 4 << 10

// deployMarkerTempPattern names a marker write's temp file.
const deployMarkerTempPattern = ".deploying-*.tmp"

// errNotAMarker is a marker that does not prove itself one.
var errNotAMarker = errors.New("not a deploy marker")

func saveDeployMarker(d appDirs, m deployMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	removeOwnLeftovers(d.app, deployMarkerTempPattern)
	return writeOwnFile(d.deploying, deployMarkerTempPattern, data)
}

// loadDeployMarker reads the marker: fs.ErrNotExist when there is
// none, errNotAMarker for one whose content is not a marker's, and
// readOwnFile's refusal for anything but a regular file at its name.
func loadDeployMarker(d appDirs) (deployMarker, error) {
	var m deployMarker
	data, _, err := readOwnFile(d.deploying, deployMarkerMaxBytes, "a deploy marker")
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%w: %w", errNotAMarker, err)
	}
	// The version becomes a path: one that is not a version is used
	// for nothing (validVersion, as for state.json's).
	if !validVersion(m.Version) {
		return m, fmt.Errorf("%w: it names %q, which is not a version", errNotAMarker, m.Version)
	}
	if m.StartedAt.IsZero() { // every marker motel writes carries its deploy's start
		return m, fmt.Errorf("%w: it carries no start time", errNotAMarker)
	}
	return m, nil
}

// clearDeployMarker removes the marker; none there is no error. The
// name is unlinked, never what a link at it points to.
func clearDeployMarker(d appDirs) error {
	if err := os.Remove(d.deploying); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// interruptedDeployError is what a deploy cut short by a moteld stop
// is recorded as; a release that could not be removed adds why, in the
// words a failed deploy's cleanup uses.
const interruptedDeployError = "the deploy was cut short: moteld stopped or restarted during it"

// settleInterrupted settles a marker a deploy no motel finished
// left, under the rules above. The caller holds deployMu and has just
// confirmed a sweep keeping the instance of version keep ("" for
// none). The marker is gone afterwards unless removing it failed,
// which is the error.
func (ma *managedApp) settleInterrupted(c collaborators, keep string) error {
	d := c.spec.dirs
	// Through a linked app dir the marker, and every release, are a
	// sibling's, whose own deploy may hold that marker: nothing is
	// read, removed or written (linkAt, as a deploy refuses).
	if p, why := d.linkAt(); p != "" {
		c.logger.Warn("deploy marker not settled: the app dir is not the directory it names",
			zap.String("path", p), zap.Error(why), zap.String("fix", notOwnDirFix))
		return nil
	}
	m, err := loadDeployMarker(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		// Not a marker: its content, or a link, FIFO or other non-file
		// at its name. Acted on for nothing — a release it might have
		// named stays, as before there were markers.
		c.logger.Error("deploy marker is not one: nothing is done for it, and it is removed where it can be", zap.String("path", d.deploying), zap.Error(err))
		return ma.clearMarker(c)
	}
	logger := c.logger.With(zap.String("version", m.Version))
	if m.Version == keep {
		// That deploy reached the cutover: what it put there serves.
		return ma.clearMarker(c)
	}
	// So did one state.json names, though no instance of it is in
	// memory yet: recovery runs in the background, and a deploy can
	// take the lock before it has relaunched that version. A state
	// that cannot be read vouches for nothing — recovery refuses it,
	// so nothing serves from it — and the record decides below.
	if st, ok, err := c.store.load(); err != nil {
		logger.Warn("state.json cannot be read; settling the deploy marker by the version's record alone", zap.Error(err))
	} else if ok && st.CurrentVersion == m.Version {
		return ma.clearMarker(c)
	}
	started, recorded, err := recordedStart(d, m.Version)
	switch {
	case err != nil:
		// A record there that cannot be read may be that deploy's:
		// nothing is removed or written over it.
		logger.Error("deploy marker removed without acting: the version's deploy record cannot be read, so whether motel finished that deploy cannot be told; a release it left stays until GC", zap.Error(err))
		return ma.clearMarker(c)
	case recorded && started.Equal(m.StartedAt):
		// That deploy was recorded; only its marker's removal failed.
		return ma.clearMarker(c)
	}
	result := deployResult{
		Version:    m.Version,
		Status:     "failed",
		Error:      interruptedDeployError + "; its release was removed, so this version can be deployed again",
		StartedAt:  m.StartedAt,
		FinishedAt: c.clock.Now(),
	}
	if rmErr := removeRelease(d, m.Version); rmErr != nil {
		result.Error = fmt.Sprintf("%s; release %s left on disk: cleanup of failed release %s: %v", interruptedDeployError, m.Version, m.Version, rmErr)
		logger.Error("the release of a deploy motel did not finish could not be removed; it stays until GC", zap.Error(rmErr))
	} else {
		logger.Warn("removed the release of a deploy motel did not finish; the version can be deployed again")
	}
	ma.recordDeploy(c, result)
	ma.mu.Lock()
	if ma.lastDeploy == nil || !ma.lastDeploy.StartedAt.After(m.StartedAt) {
		ma.lastDeploy = &result
	}
	ma.mu.Unlock()
	return ma.clearMarker(c)
}

func (ma *managedApp) clearMarker(c collaborators) error {
	if err := clearDeployMarker(c.spec.dirs); err != nil {
		return fmt.Errorf("removing the deploy marker: %w", err)
	}
	return nil
}

// removeRelease removes one release dir, never through a link at the
// app dir or releases/ (linkAt), as GC removes one.
func removeRelease(d appDirs, version string) error {
	if p, why := d.linkAt(d.releases); p != "" {
		return fmt.Errorf("%s is not the directory it names: %w (%s)", p, why, notOwnDirFix)
	}
	return os.RemoveAll(d.release(version))
}

// recordedStart is when the deploy version's record holds began, and
// whether there is one; err is a record there that cannot be read,
// which is not the same as none.
func recordedStart(d appDirs, version string) (time.Time, bool, error) {
	b, err := readDeployRecord(d, version)
	if errors.Is(err, errNoDeployRecord) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	var r struct {
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return time.Time{}, false, err
	}
	return r.StartedAt, true, nil
}

// settleBeforeDeploy settles a marker before a deploy goes on, once a
// sweep keeping old confirms: the deploy that left the marker may have
// left its pre_start running too, and the deploy is refused, naming
// it, until that has ended. Only a marker there costs the sweep.
func (ma *managedApp) settleBeforeDeploy(c collaborators, old *instance) error {
	if _, err := os.Lstat(c.spec.dirs.deploying); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := ma.sweepOrWhy(c, old, ""); err != nil {
		return fmt.Errorf("%w; not deploying until the deploy motel did not finish is settled", err)
	}
	keep := ""
	if old != nil {
		keep = old.version
	}
	// A marker that cannot be removed is replaced by this deploy's own
	// if it extracts, or fails that write, naming the path (a
	// non-empty directory there: an accepted residual).
	if err := ma.settleInterrupted(c, keep); err != nil {
		c.logger.Error("deploy marker not removed; deploying on", zap.Error(err))
	}
	return nil
}

// settleAfterRecovery settles a marker once recovery's sweep has
// confirmed, keeping the instance of version keep. A marker that
// cannot be removed is logged: the app is up, and each start and
// deploy tries again (a file there: the record it was settled into
// then says to clear it; a non-empty directory: never, until it is
// removed by hand).
func (ma *managedApp) settleAfterRecovery(c collaborators, keep string) {
	if err := ma.settleInterrupted(c, keep); err != nil {
		c.logger.Error("deploy marker not removed; each start and deploy tries again", zap.Error(err))
	}
	// And what a stop left mid-transfer: a staged artifact in tmp/,
	// which no deploy would remove and the next upload could fail on.
	// Not through a link at the app dir or at tmp/ (linkAt, as GC asks
	// of releases/ and a deploy's writeRefusal of tmp/): blog's tmp/
	// linked to shop's would have blog's start remove shop's upload.
	if p, why := c.spec.dirs.linkAt(c.spec.dirs.tmp); p != "" {
		c.logger.Warn("staged artifacts not swept: the app dir or tmp/ is not the directory it names",
			zap.String("path", p), zap.Error(why), zap.String("fix", notOwnDirFix))
		return
	}
	removeStagedArtifacts(c.spec.dirs)
}
