package liveswap

import (
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
// rules; every function here holds them, and the filter's own rules
// (redact.go) hold the second and third.
//
//  1. A record is bytes motel wrote once, through the filter, into
//     a directory it verified as its own. Anything read back is text
//     from disk: it may be served only through the filter, and it is
//     trusted for nothing else — not as a name, not as a retention
//     slot — unless it proves itself a record: a regular file, under
//     a valid version name, whose object names that version. One
//     value of a record is read back as a value: the pin, which a
//     rollback carries into the record that replaces it
//     (recordedPin) — only a digest-shaped string. It lands in the
//     new record, which goes through the filter again, and in
//     last_deploy, where like every pin it is a safe string
//     (redactorFor) until the next deploy replaces it. And one fact
//     about a record is read back, with its outcome: its write time,
//     off the open file and only for a record that proves itself one
//     (recordedDeploy), which is its version's place in the release
//     order that retention and available_versions follow (state.go) —
//     a key the app cannot write.
//  2. Nothing from disk reaches a response except through the filter
//     as body text. A recorded version is a safe string like any
//     other name, and no safe string equals a known value (redact.go,
//     rule 1). Nothing is appended to a body after the filter's final
//     pass (withField merges what the filtered body already reports;
//     it adds nothing else).
//  3. Whatever the filter does to a body, the record on disk names
//     its version and outcome, from values that cannot carry source
//     data: the version the deployer named and the digest it was
//     pinned to (unless the filter would change it, or the values
//     are not all known), vocabulary, timestamps.
//     The vocabulary (status, phase) stands outside the filter in
//     every body (redact.go, rule 3), so a value equal to a word of
//     it rewrites nothing; a record the filter leaves unreadable as
//     one, or too large to read back, is an envelope, never an
//     unreadable file.
//  4. The store never blocks a deploy or a status — a failure here is
//     a warning, a missing store is an empty list — and never follows
//     a link (`deploys -> ..`, a record name pointing elsewhere, an
//     ancestor pointing at another app): no app can plant one, but
//     nothing on disk says who did, and following an operator's would
//     write into or read from a directory that is not this app's
//     (ownfile.go). The bind sources' check draws the same line
//     (resolveBindSources, sandbox.go).
//
// The file holds the result as the record filter left it, with the
// env_file values known at the time already replaced: a secret rotated
// later is not in an old record for the new filter to miss. Records
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

// recordDeploy writes result's filtered form as the record of its
// version, then prunes. Failures are logged, not fatal: the deploy's
// outcome is already known and the response must say it.
func (ma *managedApp) recordDeploy(c collaborators, result deployResult) {
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
	rd, unknown := ma.recordRedactor(c, result)
	filtered := rd.redactJSON(raw)
	// Rule 3's envelope. The filter's whole-body fallback — a known
	// value that is a fragment of the record's own JSON — leaves an
	// object that is not a record of this version (recordHead: no
	// version, or an outcome the filter reached); and a record larger
	// than a reader accepts would be written only to be refused.
	// Either way the record is the version the deployer named, the
	// digest they pinned it to, the outcome vocabulary, the times,
	// and why the rest is missing —
	// nothing that could carry source data, so nothing the filter
	// would have had to see.
	why := ""
	switch _, isRecord := recordHead([]byte(filtered), result.Version+".json"); {
	case unknown != "":
		why = unknown
	case !isRecord && !rd.safeExact[result.Version]:
		// Redacted like the value it equals (redact.go, rule 1), so
		// the record could not name itself; the envelope does. (A
		// value too short to be a secret drops the version from the
		// safe list too, but redacts nothing: the record stands.)
		why = "the version equals an env_file value"
	case !isRecord:
		why = "a redacted value overlapped the record's own structure or outcome"
	case len(filtered)+1 > deployRecordMaxBytes: // +1: the newline the file ends with
		why = "larger than a record can be"
	}
	if why != "" {
		envelope := map[string]any{
			"version": result.Version, "status": result.Status,
			"started_at": result.StartedAt, "finished_at": result.FinishedAt,
			"error": "record withheld: " + why,
		}
		if result.Phase != "" { // absent, as a marshalled result has it, not ""
			envelope["phase"] = result.Phase
		}
		// The pin is digest-shaped (validated, or read back by
		// recordedPin); kept only as the filter would leave it whole —
		// not equal to a known value, and none inside it (rule 1
		// redacts a known value within a safe string too) — and only
		// when every value is known: with the env_file unread past a
		// bad line, that cannot be decided.
		if filteredPin, _ := rd.Redact(result.SHA256); unknown == "" && result.SHA256 != "" && filteredPin == result.SHA256 {
			envelope["sha256"] = result.SHA256
		}
		env, err := json.Marshal(envelope)
		if err != nil {
			c.logger.Warn("deploy record: cannot encode the envelope", zap.Error(err))
			return
		}
		filtered = string(env)
	}
	if err := writeDeployRecord(c.spec.dirs, result.Version, []byte(filtered)); err != nil {
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

// recordRedactor is the filter a record is written through: the
// deploy's own spec and every value the filter knows — remembered at
// any launch, and read from the deploy's own env_file now. It does
// not withhold on the *live* env_file as the response filter does (a
// record is about a deploy that is over, and a reload to an
// unreadable file while it ran must not turn its outcome into a
// placeholder nothing can recover once the file is fixed); but when
// the deploy's own env_file is there and cannot be parsed whole, the
// values before the bad line are unknown to the filter and could be
// in the very error that says so — the second result names that, and
// the record is the envelope (rule 3). A file that is absent holds no
// values to know. Its safe strings are the names the response filter
// exempts: the version and its pin, the app's dirs, the releases on
// disk — and none equal to a known value (redact.go, rule 1).
func (ma *managedApp) recordRedactor(c collaborators, result deployResult) (*Redactor, string) {
	ma.secretsMu.Lock()
	kvs := append([]string(nil), ma.secrets...)
	ma.secretsMu.Unlock()
	// The values remembered so far are those of every launch, and a
	// deploy that failed before its launch — a download that did not
	// complete — remembered nothing: read the deploy's own env_file
	// too, so what it holds now is known to the filter that writes
	// the record. Unreadable, the remembered values are all there is.
	unknown := ""
	if c.spec != nil && c.spec.envFile != "" {
		switch now, err := parseEnvFile(c.spec.envFile); {
		case err == nil:
			kvs = mergeSecrets(kvs, now)
		case !errors.Is(err, fs.ErrNotExist):
			unknown = "the deploy's env_file could not be read whole, so its values are unknown to the filter"
		}
	}
	safe := []string{ma.name, result.Version, result.SHA256}
	if c.spec != nil {
		safe = append(safe, c.spec.dirs.root, c.spec.dirs.app, c.spec.dirs.releases, c.spec.dirs.shared, c.spec.dirs.run)
		onDisk, _ := listReleases(c.spec.dirs) // names only: the order is not this filter's concern
		safe = append(safe, onDisk...)
	}
	return NewRedactor(kvs, safe), unknown
}

// recordsDir is the records directory as the directory it names
// (rule 4): with create, made if absent; refused when the app dir or it
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
// equal to the name it was read under. The rest of the object is body
// text for the filter.
func recordHead(b []byte, name string) (deploySummary, bool) {
	var s deploySummary
	if json.Unmarshal(b, &s) != nil || !validVersion(s.Version) || s.Version+".json" != name {
		return deploySummary{}, false
	}
	// And an outcome in the vocabulary: a status that is one, and a
	// phase — the one a failure reached — only on a failure, and one
	// of the vocabulary. The writer guarantees all of it (rule 3), so
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
func writeDeployRecord(d appDirs, version string, filtered []byte) error {
	dir, err := recordsDir(d, true)
	if err != nil {
		return err
	}
	return writeOwnFile(deployRecordPath(dir, version), ".record-*.tmp", append(filtered, '\n'))
}

// deployRecordMaxBytes bounds a record read: one holds a bounded
// result (an 8 KiB tail at most); anything larger is not one.
const deployRecordMaxBytes = 1 << 20

// openRecord reads a record under the read rule every store in the app
// dir shares (readOwnFile): no link followed (rule 4), no FIFO held,
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
// the filter redacted (equal to an env_file value) included, which a
// rollback then records as unpinned.
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
		// What its version then does in a response is rule 2's, in
		// redactorFor.
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
