package liveswap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// A deploy's outcome used to live in two places: the response the
// deployer got, and last_deploy in the status until the next deploy
// replaced it. The record of why v1.4.1 failed last Tuesday was gone
// by Wednesday. Now every deploy writes its result to
// <app>/deploys/<version>.json — the latest outcome of that version,
// a rollback included — and GET /<app>?deploy=<version> reads it
// back; the status lists each recorded version's outcome.
//
// The store is a trust boundary in both directions, and these are its
// rules; every function here holds them.
//
//  1. A record is bytes motel wrote once into a directory it verified
//     as its own: the deploy's result as the answer carried it, the
//     app's text in it already through the app's filter where it
//     entered (redact.go), and marked so (filtered_at_entry). Anything
//     read back is text from disk: it is served only when it proves
//     itself a record (a regular file, under a valid version name,
//     whose object names that version and an outcome of the
//     vocabulary), as it stands when it bears the mark, and with its
//     error and detail through the app's filter when it does not
//     (servedRecord); and it is trusted for nothing else. One
//     value of a record is read back as a value: the pin, which a
//     rollback carries into the record that replaces it (recordedPin),
//     only a digest-shaped string. And one fact about a record is read
//     back, with its outcome: its write time, off the open file
//     (recordedDeploy), which is its version's place in the release
//     order that retention and available_versions follow (state.go), a
//     key the app cannot write.
//  2. A record no reader would accept is not written: one too large to
//     read back is an envelope (version, outcome, times, why), never an
//     unreadable file.
//  3. The store never blocks a deploy or a status — a failure here is
//     a warning, a missing store is an empty list — and never follows
//     a link (`deploys -> ..`, a record name pointing elsewhere, an
//     ancestor pointing at another app): no app can plant one, but
//     nothing on disk says who did, and following an operator's would
//     write into or read from a directory that is not this app's
//     (ownfile.go). The bind sources' check draws the same line
//     (resolveBindSources, sandbox.go).
//
// The file holds the result with the env_file values known at the
// deploy already replaced in the app's text: a secret rotated later is
// not in an old record. A value added to env_file later is, if the app
// had printed it: no read filters a marked record again (an accepted
// row, DESIGN-liveswap.md). A record an older moteld wrote has no mark:
// that moteld filtered it whole when it wrote it and again at each
// read, so it is filtered when read here too, its error and detail
// alone. Records
// are pruned with the releases: one is kept for every version still on
// disk, plus the newest `keep` others — failed deploys, whose release
// is removed at once, and versions release GC has pruned — so the
// directory is bounded by 2×keep whatever the deploy rate.

// summaryPin is the summary's digest when it has the shape the writer records
// (lowercase hex); "" otherwise. A record is read off disk, so a
// string in the field names nothing until it is a digest, as a
// version names nothing until it is valid (recordHead) — and the safe
// string is the text the body carries, never a respelling of it.
func summaryPin(s deploySummary) string {
	var d string
	if json.Unmarshal(s.SHA256, &d) != nil || !sha256Re.MatchString(d) || d != strings.ToLower(d) {
		return ""
	}
	return d
}

// recordDeploy writes result as the record of its version, marked as
// filtered where its app text entered, then prunes. Failures are
// logged, not fatal: the deploy's outcome is already known and the
// response must say it.
func (ma *managedApp) recordDeploy(c collaborators, result deployResult) {
	result.FilteredAtEntry = true
	dir, err := recordsDir(c.spec.dirs, true)
	if err != nil {
		c.logger.Warn("deploy record: not written", zap.Error(err))
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		c.logger.Warn("deploy record: cannot encode", zap.Error(err))
		return
	}
	if len(raw)+1 > deployRecordMaxBytes { // +1: the newline the file ends with
		// Rule 2's envelope: a record larger than a reader accepts
		// would be written only to be refused.
		envelope := map[string]any{
			"version": result.Version, "status": result.Status,
			"started_at": result.StartedAt, "finished_at": result.FinishedAt,
			"error":             "record withheld: larger than a record can be",
			"filtered_at_entry": true,
		}
		if result.Phase != "" { // absent, as a marshalled result has it, not ""
			envelope["phase"] = result.Phase
		}
		if result.SHA256 != "" {
			envelope["sha256"] = result.SHA256
		}
		if raw, err = json.Marshal(envelope); err != nil {
			c.logger.Warn("deploy record: cannot encode the envelope", zap.Error(err))
			return
		}
	}
	if err := writeDeployRecord(c.spec.dirs, result.Version, raw); err != nil {
		c.logger.Warn("deploy record: cannot write", zap.String("version", result.Version), zap.Error(err))
		return
	}
	// The protection set is read strictly: a releases dir that cannot
	// be listed is no reason to prune, since an on-disk release would
	// then look like an "other" and lose its record.
	onDisk, err := releaseNames(c.spec.dirs.releases)
	if err != nil {
		c.logger.Warn("deploy record: releases unreadable, not pruning", zap.Error(err))
		return
	}
	pruneDeployRecords(dir, c.spec.keep, onDisk, c.logger)
}

// releaseNames is every release directory's name, or an error when the
// directory cannot be listed — unlike listReleases, which is
// best-effort and ordered, this is the set pruning must not miss.
func releaseNames(releasesDir string) ([]string, error) {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// servedRecord is rec, the bytes of version's record as
// readDeployRecord proved them one, as GET /<app>?deploy= sends them.
// A record that bears the mark (filtered_at_entry) is sent as it
// stands. One an older moteld wrote does not: that moteld filtered a
// record whole when it wrote it and again whenever it was read, so its
// error and detail pass the app's filter here, as the app's text and
// an error's text do where they enter now, with what the filter knows
// today (a value learnt since the record was written is hidden). Its
// other fields are sent as they stand, its keys in sorted order, and
// the keys the filter replaced are added to its redacted_env. An error
// or a detail that is not of its type in such a record is not sent:
// it could not be filtered as one.
func (ma *managedApp) servedRecord(rec []byte, version string) []byte {
	rec = bytes.TrimSuffix(rec, []byte("\n")) // the file's own, which the answer adds again
	var top map[string]json.RawMessage
	if json.Unmarshal(rec, &top) != nil {
		return rec // readDeployRecord parsed it: not reached
	}
	if marked, ok := top["filtered_at_entry"]; ok && string(marked) == "true" {
		return rec
	}
	names := []string{version}
	var pin string
	if json.Unmarshal(top["sha256"], &pin) == nil {
		names = append(names, pin)
	}
	d := ma.door(true, names...)
	if raw, ok := top["error"]; ok {
		var e string
		if json.Unmarshal(raw, &e) != nil {
			delete(top, "error")
		} else {
			top["error"], _ = json.Marshal(d.errText(e)) // a string: nothing to fail
		}
	}
	if raw, ok := top["detail"]; ok {
		var det deployDetail
		if json.Unmarshal(raw, &det) != nil {
			delete(top, "detail")
		} else {
			top["detail"], _ = json.Marshal(d.again(det)) // strings, bools, an int: nothing to fail
		}
	}
	if keys := d.keys(); len(keys) > 0 {
		var had []string
		_ = json.Unmarshal(top["redacted_env"], &had) // a key that is not a list is replaced
		top["redacted_env"], _ = json.Marshal(union(had, keys))
	}
	out, err := json.Marshal(top)
	if err != nil {
		return rec // raw values that parsed: not reached
	}
	return out
}

// recordNames is the versions the records directory holds a file
// under, those valid as versions: names the app's filter leaves to its
// known values alone (managedApp.door), as the versions the status
// lists. The directory is listed, never followed, and no record is
// read: a name is only a name, and a safe string equal to a known value
// exempts nothing.
func recordNames(d appDirs) []string {
	dir, err := recordsDir(d, false)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if v, ok := strings.CutSuffix(e.Name(), ".json"); ok && validVersion(v) {
			names = append(names, v)
		}
	}
	return names
}

// recordsDir is the records directory as the directory it names
// (rule 3): with create, made if absent; refused when the app dir or it
// is a link, is not a directory, or cannot be checked for one
// (appDirs.linkAt, the rule every dir motel deletes in is held to) —
// `<root>/blog -> <root>/shop` would have blog's deploys list, serve
// and prune shop's records. An alias on the liveswap root itself is
// the one difference allowed. Without create — every read — a missing
// directory is fs.ErrNotExist and nothing is made: a status poll must
// not create an app's tree.
func recordsDir(d appDirs, create bool) (string, error) {
	// The app dir first, before anything is created through it: a link
	// there would otherwise have MkdirAll make the directory where it
	// points before the check below refused it.
	if p, why := d.linkAt(); p != "" {
		return "", fmt.Errorf("%s %w", p, why)
	}
	if create {
		if err := os.MkdirAll(d.deploys, 0o750); err != nil {
			return "", err
		}
	}
	if p, why := d.linkAt(d.deploys); p != "" {
		return "", fmt.Errorf("%s %w", p, why)
	}
	if _, err := os.Lstat(d.deploys); err != nil {
		return "", err // fs.ErrNotExist when absent and not creating
	}
	return d.deploys, nil
}

// recordHead is what proves a file a record (rule 1): a valid version
// equal to the name it was read under.
func recordHead(b []byte, name string) (deploySummary, bool) {
	var s deploySummary
	if json.Unmarshal(b, &s) != nil || !validVersion(s.Version) || s.Version+".json" != name {
		return deploySummary{}, false
	}
	// And an outcome in the vocabulary: a status that is one, and a
	// phase — the one a failure reached — only on a failure, and one
	// of the vocabulary. The writer guarantees all of it, so
	// a file without is not a record of ours.
	if !outcomeStatuses[s.Status] || (s.Phase != "" && (s.Status != "failed" || !outcomePhases[s.Phase])) {
		return deploySummary{}, false
	}
	// And its times and pin strings, as the writer marshals them: the
	// summary carries them as they are into the status body, and a
	// string is the one value that re-encodes as text rather than
	// structure.
	for _, raw := range []json.RawMessage{s.StartedAt, s.FinishedAt, s.SHA256} {
		var str string
		if len(raw) != 0 && (string(raw) == "null" || json.Unmarshal(raw, &str) != nil) {
			return deploySummary{}, false
		}
	}
	return s, true
}

// writeDeployRecord writes the record atomically, under the write rule
// every store in the app dir shares (writeOwnFile): a fresh temp file
// under a random name (O_EXCL, never a path something could have
// planted), then a rename over the record's name. Mode 0600 like
// state.json: a record carries the app's own lines, filtered but
// still the app's, and nothing but motel reads records — the
// webhook is the interface.
func writeDeployRecord(d appDirs, version string, record []byte) error {
	dir, err := recordsDir(d, true)
	if err != nil {
		return err
	}
	return writeOwnFile(deployRecordPath(dir, version), ".record-*.tmp", append(record, '\n'))
}

// deployRecordMaxBytes bounds a record read: one holds a bounded
// result (an 8 KiB tail at most); anything larger is not one.
const deployRecordMaxBytes = 1 << 20

// openRecord reads a record under the read rule every store in the app
// dir shares (readOwnFile): no link followed (rule 3), no FIFO held,
// nothing but a regular file of a record's size; the modification time
// comes from the same open file as the bytes, so a record replaced
// between two lookups cannot pair one outcome with another's time.
func openRecord(path string) ([]byte, time.Time, error) {
	return readOwnFile(path, deployRecordMaxBytes, "a record")
}

func deployRecordPath(dir, version string) string {
	return filepath.Join(dir, versionPathComponent(version)+".json")
}

// recordedPin is the digest the version's record says it was pinned
// to, or "" when there is no record, it does not prove
// itself one (rule 1), or its pin is absent or not a digest — a pin
// an older moteld's filter redacted (equal to an env_file value)
// included, which a rollback then records as unpinned.
//
// Known limit: nothing ties the record to the release's bytes. A
// version whose release GC pruned can be deployed again; if that
// deploy leaves its bytes on disk without rewriting the record — the
// record write fails (logged, not fatal), or a refusal's cleanup of
// the release fails (a refusal writes no record) — the old record and
// its pin stay, and a rollback carries that pin onto bytes it never
// checked; a deploy of the running version is matched against the
// same pin (relaunchRefusal, app.go) and relaunches the bytes on
// disk, introducing none. Closing it means keeping the pin with the
// release instead.
func recordedPin(d appDirs, version string) string {
	b, err := readDeployRecord(d, version)
	if err != nil {
		return ""
	}
	s, ok := recordHead(b, version+".json")
	if !ok {
		return ""
	}
	return summaryPin(s)
}

// recordedDeploy is when motel last wrote version's record in
// records (a directory recordsDir vouched for; "" for none) — the
// version's deploy time, which orders the releases (state.go) — and
// whether that deploy failed. ok is false when there is no record that
// proves itself one (rule 1): none under the name, the name is not a
// version, or the object is not a record of it. err is a record that
// is there and cannot be read — permissions, I/O, a link or anything
// but a regular file at its name — which is not the same as none: GC
// must not act on an order it could not read.
func recordedDeploy(records, version string) (written time.Time, failed, ok bool, err error) {
	if records == "" || !validVersion(version) {
		return time.Time{}, false, false, nil
	}
	name := versionPathComponent(version) + ".json"
	b, written, err := openRecord(filepath.Join(records, name))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, false, nil
	}
	if err != nil {
		return time.Time{}, false, false, err
	}
	s, ok := recordHead(b, name)
	if !ok {
		return time.Time{}, false, false, nil
	}
	return written, s.Status == "failed", true, nil
}

// errNoDeployRecord is a version no deploy has been recorded for.
var errNoDeployRecord = errors.New("no deploy recorded for this version")

// readDeployRecord returns the record's bytes — one JSON object, as
// written — or errNoDeployRecord.
func readDeployRecord(d appDirs, version string) (json.RawMessage, error) {
	dir, err := recordsDir(d, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	name := versionPathComponent(version) + ".json"
	b, _, err := openRecord(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNoDeployRecord
	}
	if err != nil {
		return nil, err
	}
	if _, ok := recordHead(b, name); !ok {
		return nil, fmt.Errorf("deploy record for %s is not that version's record", version)
	}
	return json.RawMessage(b), nil
}

// listDeploySummaries reads every record's outcome, newest first by
// the record's write time. nil when there is no directory yet.
func listDeploySummaries(d appDirs) []deploySummary {
	dir, err := recordsDir(d, false)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type dated struct {
		summary deploySummary
		written time.Time
	}
	var recs []dated
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, written, err := openRecord(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // a link, or not a record
		}
		s, ok := recordHead(b, e.Name())
		if !ok {
			continue
		}
		recs = append(recs, dated{s, written})
	}
	// Newest first; two written within one timestamp tick by name, so
	// the order is the same on every read.
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].written.Equal(recs[j].written) {
			return recs[i].written.After(recs[j].written)
		}
		return recs[i].summary.Version > recs[j].summary.Version
	})
	out := make([]deploySummary, len(recs))
	for i, r := range recs {
		out[i] = r.summary
	}
	return out
}

// newestDeploySummary is listDeploySummaries(d)[0], read without
// reading every record: the names are ordered by their mtime, which a
// record is written with (the time listDeploySummaries orders by, read
// there from the open file), newest first and by version on a tie, and
// the first that opens and proves itself a record is the one. One
// directory listing and a stat per entry, and in the common case one
// record read, where the full list reads them all.
func newestDeploySummary(d appDirs) (deploySummary, bool) {
	dir, err := recordsDir(d, false)
	if err != nil {
		return deploySummary{}, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return deploySummary{}, false
	}
	type dated struct {
		name    string
		written time.Time
	}
	var names []dated
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // gone since the listing
		}
		names = append(names, dated{e.Name(), info.ModTime()})
	}
	// The order listDeploySummaries sorts by: a record's version is its
	// name without .json (recordHead), compared without the suffix, which
	// would sort "v1" after "v1-x".
	sort.Slice(names, func(i, j int) bool {
		if !names[i].written.Equal(names[j].written) {
			return names[i].written.After(names[j].written)
		}
		return strings.TrimSuffix(names[i].name, ".json") > strings.TrimSuffix(names[j].name, ".json")
	})
	for _, n := range names {
		b, _, err := openRecord(filepath.Join(dir, n.name))
		if err != nil {
			continue // a link, or not a record
		}
		if s, ok := recordHead(b, n.name); ok {
			return s, true
		}
	}
	return deploySummary{}, false
}

// pruneDeployRecords keeps a record for every version still on disk
// and the newest keep others, by modification time, and removes the
// rest.
func pruneDeployRecords(dir string, keep int, onDisk []string, logger *zap.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	disk := make(map[string]bool, len(onDisk))
	for _, v := range onDisk {
		disk[versionPathComponent(v)+".json"] = true
	}
	type rec struct {
		name    string
		modTime time.Time
	}
	var others []rec
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			// A write interrupted between the temp file and the rename:
			// deploys are serialized per app, so no write is in flight
			// now, and the leftover would otherwise sit outside the
			// bound — like release GC's crashed staging dirs.
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				logger.Info("deploy record: removed orphaned temp file", zap.String("file", e.Name()))
			}
			continue
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || disk[e.Name()] {
			continue
		}
		// Rule 1 again: only a record of ours holds a retention slot —
		// a link, a file that is not JSON, or a version that is not
		// its name would otherwise crowd out the records the slots are
		// for. Such an entry is not ours to keep either; it goes.
		b, written, err := openRecord(filepath.Join(dir, e.Name()))
		if _, ok := recordHead(b, e.Name()); err != nil || !ok {
			if rmErr := os.Remove(filepath.Join(dir, e.Name())); rmErr == nil {
				logger.Info("deploy record: removed an entry that is not a record", zap.String("file", e.Name()))
			}
			continue
		}
		others = append(others, rec{e.Name(), written})
	}
	sort.Slice(others, func(i, j int) bool {
		if !others[i].modTime.Equal(others[j].modTime) {
			return others[i].modTime.After(others[j].modTime)
		}
		return others[i].name > others[j].name
	})
	for i, r := range others {
		if i < keep {
			continue
		}
		if err := os.Remove(filepath.Join(dir, r.name)); err != nil {
			logger.Warn("deploy record: cannot prune", zap.String("record", r.name), zap.Error(err))
			continue
		}
		logger.Info("deploy record: pruned", zap.String("record", r.name))
	}
}
