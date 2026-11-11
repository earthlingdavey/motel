package box

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/box/proof"
)

// Phases (DESIGN-box.md, "States" and Glossary): a result's phase is
// `verified` or terminal; a record's is one of the five it can be in.
const (
	phaseVerified    = "verified"
	phaseRefused     = "refused"
	phaseNoChange    = "no_change"
	phaseApplied     = "applied"
	phaseFailed      = "failed"
	phaseRolledBack  = "rolled_back"
	phaseUnknown     = "unknown"
	phaseInstalling  = "installing"
	phaseSwapped     = "swapped"
	phaseRollingBack = "rolling_back"
)

// terminal reports whether a result's phase ends the push.
func terminal(phase string) bool {
	switch phase {
	case phaseRefused, phaseNoChange, phaseApplied, phaseFailed, phaseRolledBack, phaseUnknown:
		return true
	}
	return false
}

// The applier's messages, word for word from DESIGN-box.md's Message
// catalogue. The proof's and the walk's refusals carry their own.
const (
	msgNotRunning         = "motel is not running; nothing applied"
	msgStillStarting      = "motel is still starting after 300 s; nothing applied"
	msgNoSigner           = "the Caddyfile this box runs lists no signer; motel init is the way back"
	msgInterrupted        = "interrupted before the Caddyfile changed; nothing changed"
	msgInterruptedStopped = "interrupted; the previous Caddyfile is on disk; motel is not running — systemctl reset-failed motel, then systemctl start motel, loads it"
	msgNoRecord           = "the box has no record of this push; push again"
	msgReloadFailed       = "the reload failed; the previous Caddyfile is back and running — journalctl -u motel -n 50 on the box says why"
	// The same, for a set whose Caddyfile is unchanged: the box's own
	// env file (_box.env) was what reloaded, and notify refused it.
	msgReloadFailedBox   = "the reload failed on the box's new notify secrets; the previous ones are back and running — journalctl -u motel -n 50 on the box says why (notify takes a channel URL that is https to a public host)"
	msgRecordAfterReload = "the record could not be updated after a successful reload (disk full); rolled back to keep the file and the record consistent"
	// The swap landed, then the `swapped` record or the swap's own fsync
	// failed: nothing was reloaded before the rollback.
	msgRolledBackUnrecorded = "the Caddyfile was swapped, then a write after the swap failed; rolled back: the previous Caddyfile is back and running"
	// A crash fell after the swap: recovery rolled back, the reload's
	// outcome unknown.
	msgRolledBackInterrupted = "interrupted after the Caddyfile changed; rolled back: the previous Caddyfile is back and running"
	msgBothReloads           = "the reload failed and so did the reload of the previous file; the previous file is on disk; journalctl -u motel"
	// The reload of the previous file failed in a rollback whose first
	// reload did not (bothFailed).
	msgPrevReloadFailed = "rolled back, but the reload of the previous file failed; the previous file is on disk; journalctl -u motel"
	msgChanged          = "the Caddyfile changed during the transaction; it is left as found"
	msgDiskFull         = "disk full; the previous Caddyfile could not be restored"
	// The texts for a set with env files in it, where the catalogue
	// names "the box's files" (DESIGN-box.md, Message catalogue: the
	// first text of a row for the Caddyfile alone, the second with env
	// files); <app> is filled in by fill.
	msgInterruptedFiles           = "interrupted before the box's files changed; nothing changed"
	msgRolledBackUnrecordedFiles  = "a write after the first file changed failed; rolled back: the previous Caddyfile and env files are back"
	msgRolledBackInterruptedFiles = "interrupted after the box's files changed; rolled back: the previous files are back"
	msgEnvChanged                 = "the env file for <app> changed during the transaction; it is left as found"
	msgEnvDiskFull                = "disk full; the previous env file for <app> could not be restored"
	// msgInitRemedy follows init's refusal on a motel that is not up
	// ("Console commands"); %[1]s is init's argument and %[2]s its
	// directory, absolute, since the operator may read it after a
	// one-shot ssh and follow it from another. motel starts on the
	// file it has, so init's own swap and rollback still stand between
	// the change and the box; the old file is never touched by hand. A
	// restart, not a start: a start joins one already in flight, which
	// read the old file; reset-failed first, as msgInterruptedStopped
	// says, since a unit at its start limit refuses a restart.
	msgInitRemedy = `the Caddyfile is accepted only by a running motel. Start it on the file it has, then run init again:
  systemctl enable motel && systemctl reset-failed motel && systemctl restart motel
  cd %[2]s && motel init %[1]s
If it will not start, journalctl -u motel says why.`
)

// installFailed is `failed` for an install that stopped before the
// Caddyfile changed, the box's own error named.
func installFailed(err error) string {
	return "the install failed before the Caddyfile changed: " + proof.Bound(err.Error()) + "; nothing changed"
}

// installFailedFiles is installFailed with env files in the set, or
// step 15a's own error.
func installFailedFiles(err error) string {
	return "the install failed before the box's files changed: " + proof.Bound(err.Error()) + "; nothing changed"
}

// baselineUnrecorded is `unknown` for a transaction whose applied.json
// could not be written, by who ran it and how it ended: the file stands
// (applied, or unchanged), and the remedy records it.
func baselineUnrecorded(o origin, phase, commit string) string {
	what := "applied but"
	if phase == phaseNoChange {
		what = "unchanged, but"
	}
	switch {
	case o == originInit:
		return what + " the baseline could not be recorded; motel init " + commit + " again, which ends no_change and records it"
	case phase == phaseNoChange:
		// The file did not change, so a push of the same chain is
		// verified as this one was.
		return what + " the baseline could not be recorded; push again, which records it"
	}
	// Not "push again": a chain whose signer list changed on the way
	// is verified again against the file now installed, and refused.
	// applied.json keeps the previous digest (D1), so step 10 flags the
	// file the next push finds.
	return what + " the baseline could not be recorded; motel box baseline " + commit + " on the box records it (the next push then reports the file as edited out of band; it was not)"
}

// result is out/<id>.json (DESIGN-box.md, "Record and result fields"):
// what a push came to, as the handler answers a result poll. Every
// string in it is the applier's own text, a value held to a grammar
// (id, commit, host, principal, app names), or bounded: the path by
// proof.SplitPath's rules, the diff redacted and capped (diff.go), the
// error built from proof.Bound pieces.
type result struct {
	ID         string   `json:"id"`
	Phase      string   `json:"phase"`
	Commit     string   `json:"commit,omitempty"`
	Path       string   `json:"path,omitempty"`
	Signer     string   `json:"signer,omitempty"`
	BoxWebhook string   `json:"box_webhook,omitempty"`
	OutOfBand  bool     `json:"caddyfile_edited_out_of_band"`
	Apps       []string `json:"apps,omitempty"`
	Diff       string   `json:"diff,omitempty"`
	Error      string   `json:"error,omitempty"`
	// Named is the object ids Error names, which an answer holds out of
	// the response filter as it does Commit.
	Named []string `json:"named,omitempty"`
	// Reloaded, on an applied result, says that the push reloaded motel:
	// the Caddyfile changed, or the box's own env file did (txn.reloads),
	// which the names below cannot tell — the same values over other
	// bytes reload, and a rekey of the same bytes does not.
	Reloaded bool `json:"reloaded,omitempty"`
	// The secrets the push adds, changes and removes, as <app>/<VAR>,
	// and the apps whose env file root wrote or removed over bytes it
	// had not installed: names, never a value (I9).
	SecretsAdded     []string `json:"secrets_added,omitempty"`
	SecretsChanged   []string `json:"secrets_changed,omitempty"`
	SecretsRemoved   []string `json:"secrets_removed,omitempty"`
	SecretsOutOfBand []string `json:"secrets_edited_out_of_band,omitempty"`
}

// marshal is the result's bytes, never more than the handler reads: the
// caps on its fields keep a result well under maxResult (a 64 KiB diff
// whose every byte JSON escapes is 384 KiB; app names are plain ASCII;
// 16384 secrets' names of at most some 200 bytes), and should that
// arithmetic ever be wrong the diff and the app list give way, then the
// secrets' names, never the verdict.
func (r result) marshal() []byte {
	b := encodeJSON(r)
	if len(b) > maxResult {
		r.Diff, r.Apps = "(diff and apps omitted: the result would be larger than the handler reads)", nil
		b = encodeJSON(r)
	}
	if len(b) > maxResult {
		r.SecretsAdded, r.SecretsChanged, r.SecretsRemoved = nil, nil, nil
		b = encodeJSON(r)
	}
	return b
}

func encodeJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // `<` in a diff is a byte, not six
	if err := enc.Encode(v); err != nil {
		// Every type marshalled here is strings, bools and slices of
		// them: an error is a programming mistake.
		panic(fmt.Sprintf("box: encoding %T: %v", v, err))
	}
	return buf.Bytes()
}

// writeResult writes out/<id>.json, 0640 in the setgid out/, so the
// handler's group can read it. When it cannot, the result goes to the
// journal instead, every field of it, at error level (I3). Either way
// the outcome is logged once.
func (a *Applier) writeResult(r result) error {
	path := filepath.Join(a.x("out"), r.ID+".json")
	tmp := filepath.Join(a.x("out"), "."+r.ID+".json.tmp")
	err := a.writeDurable("result:"+r.Phase, path, tmp, r.marshal(), 0o640)
	// Kept from this run's sweep whether or not the write succeeded: one
	// whose rename landed before its fsync failed is the push's answer
	// all the same, and one that never landed leaves nothing to keep.
	if a.wrote == nil {
		a.wrote = map[string]string{}
	}
	if !terminal(a.wrote[r.ID]) { // an ended push stays ended
		a.wrote[r.ID] = r.Phase
	}
	if err != nil {
		a.logger.Error("box result could not be written", append(resultFields(r), zap.String("write_error", proof.Bound(err.Error())))...)
		return err
	}
	a.logOutcome(r)
	return nil
}

// removeResult removes out/<id>.json, if there is one.
func (a *Applier) removeResult(id string) {
	if err := a.removeDurable("result:remove", filepath.Join(a.x("out"), id+".json")); err != nil {
		a.logger.Error("box: could not remove a result", zap.String("id", id), zap.String("error", proof.Bound(err.Error())))
	}
}

// logOutcome is the applier's journal line for a push (DESIGN-box.md,
// "Journal lines"): id, commit, signer, box and phase, and the error
// for a red one.
func (a *Applier) logOutcome(r result) {
	fields := []zap.Field{zap.String("id", r.ID), zap.String("commit", r.Commit), zap.String("signer", r.Signer), zap.String("box", r.BoxWebhook), zap.String("phase", r.Phase)}
	switch r.Phase {
	case phaseVerified, phaseNoChange, phaseApplied:
		a.logger.Info("box apply", fields...)
	default:
		a.logger.Warn("box apply", append(fields, zap.String("error", r.Error))...)
	}
}

func resultFields(r result) []zap.Field {
	return []zap.Field{
		zap.String("id", r.ID), zap.String("phase", r.Phase), zap.String("commit", r.Commit),
		zap.String("path", proof.Bound(r.Path)), zap.String("signer", r.Signer), zap.String("box", r.BoxWebhook),
		zap.Bool("caddyfile_edited_out_of_band", r.OutOfBand), zap.Strings("apps", r.Apps),
		zap.String("diff", r.Diff), zap.String("error", r.Error), zap.Strings("named", r.Named), zap.Bool("reloaded", r.Reloaded),
		zap.Strings("secrets_added", r.SecretsAdded), zap.Strings("secrets_changed", r.SecretsChanged),
		zap.Strings("secrets_removed", r.SecretsRemoved), zap.Strings("secrets_edited_out_of_band", r.SecretsOutOfBand),
	}
}

// readResult reads out/<id>.json as the handler does (readResultFile).
func (a *Applier) readResult(id string) (*result, error) {
	return readResultFile(filepath.Join(a.root, exchangeDir), id)
}

// readResultFile reads out/<id>.json, which only root writes, held to
// the shape root writes: the id it is filed under, and `verified` or a
// terminal phase. The applier (settled, recovery) and the handler (step
// 8, the poll, admission's pending check) read it the same way.
func readResultFile(dir, id string) (*result, error) {
	path := filepath.Join(dir, "out", id+".json")
	b, err := readFile(path, maxResult, false)
	if err != nil {
		return nil, err
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// The secrets' names, as named, reach an answer: anything but what
	// root writes is refused, not forwarded.
	if r.ID != id || (r.Phase != phaseVerified && !terminal(r.Phase)) || !namesIDs(r.Named) ||
		!validNameLists(r.SecretsAdded, r.SecretsChanged, r.SecretsRemoved, r.SecretsOutOfBand) {
		return nil, fmt.Errorf("%s: not a result", path)
	}
	return &r, nil
}

// maxNamed bounds a result's `named`: a refusal names at most a few
// objects (the descent refusal: HEAD and the baseline).
const maxNamed = 8

// namesIDs reports whether named is what a refusal records: at most
// maxNamed entries, each in the id grammar. Root writes out/, so
// anything else is a disk fault or the console; the answer would hold
// it out of the filter, so it is refused, not passed on.
func namesIDs(named []string) bool {
	if len(named) > maxNamed {
		return false
	}
	for _, s := range named {
		if !proof.IsID(s) {
			return false
		}
	}
	return true
}

// marker is stage/<id>.auth (DESIGN-box.md, "Record and result fields"),
// written by the handler's admission after the bundle is in in/: the
// time of admission. The applier reads it
// for two things only — the order it takes bundles in, by `posted`, and
// Retention's age — and never trusts it for anything else: the motel
// uid writes stage/, and a marker is that uid's claim.
type marker struct {
	Posted time.Time `json:"posted"`
}

// readMarker reads a marker as the handler will: without following a
// symlink at its name, without blocking on a FIFO, held to a regular
// file, at most maxMarker bytes. A marker that does not read, or whose
// fields are not the shape admission writes, is an error the caller
// treats as "no posted time".
func readMarker(path string) (*marker, error) {
	b, err := readFile(path, maxMarker, false)
	if err != nil {
		return nil, err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Posted.IsZero() {
		return nil, fmt.Errorf("%s: not a marker", path)
	}
	return &m, nil
}
