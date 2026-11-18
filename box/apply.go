package box

// `moteld box apply`: the root applier (DESIGN-box.md, "The trust
// chain" steps 9–16, "The install transaction", "Retention"). One shot,
// started by motel-box-apply.path when in/ or work/ is not empty or
// txn.json exists.
//
// The rules every function here keeps:
//
//  1. Nothing is verified from disk: each bundle is read once, into
//     memory, and every check runs on those bytes (step 9).
//  2. Every durable write goes through writeDurable or removeDurable,
//     which carry the test hooks, in the order the Transitions table
//     gives (I8: temporary fsynced, renamed, directory fsynced).
//  3. Nothing is written to disk from a defer, so a test that panics at
//     a hook stands for a crash faithfully.
//  4. Every value a bundle, a marker or the motel uid chose that
//     reaches a result, a record or the journal is held to a grammar
//     (ids, hosts, principals, app names) or passes proof.Bound or the
//     diff's redaction and cap.
//  5. Recovery acts on the record's phase, after checking the installed
//     file's digest against what the phase implies; never on digests
//     alone.
//
// The exit status is 0 for every run but the full-disk end, which
// leaves the record for the next run by design (Failure-mode table).
// Every run that leaves work on disk ends with one line saying so and
// what the console does (msgLeftOnDisk).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/box/proof"
)

// Applier is one run of the applier. Its fields are the seams the tests
// use: the filesystem root the box's paths are joined to, the service
// manager, the clock, the verifier and the hooks.
type Applier struct {
	root     string
	systemd  Systemd
	clock    Clock
	verifier *proof.Verifier
	logger   *zap.Logger
	hooks    hooks
	// waitUntil is the end of the run's one budget for waiting on a moteld that is
	// activating or reloading (running).
	waitUntil time.Time
	// lockWait, when set, is called once if lock must wait for another
	// holder: the console's commands say so rather than sit silent.
	lockWait func()
	// initPrev, when set, is shown the file runInit is about to replace,
	// read under the lock after recovery: init's host warning.
	initPrev func(prev []byte)
	// wrote is every id whose result this run wrote (or journaled,
	// unwritten), with its phase — the first terminal one, once there is
	// one: Retention keeps them, and a terminal one is settled (step 9).
	wrote map[string]string
	// listed is the names take listed in in/; nil until in/ is listed.
	listed map[string]bool
}

// nobody is the uid (and gid) ssh-keygen runs as (step 12).
const nobody = 65534

// newApplier is the applier as the unit runs it.
func newApplier(logger *zap.Logger) *Applier {
	return &Applier{
		root:     "/",
		systemd:  systemctl{unit: "moteld"},
		clock:    realClock{},
		verifier: &proof.Verifier{RunAs: nobody},
		logger:   logger,
	}
}

// x is a path in the exchange tree.
func (a *Applier) x(name string) string { return filepath.Join(a.root, exchangeDir, name) }

func (a *Applier) caddyfile() string { return filepath.Join(a.root, installedFile) }

// caddyfileTmp is the swap's temporary, `.Caddyfile.box-<id>`: beside
// the file, so the rename over it is atomic, and a dot-name whose
// prefix recovery sweeps after a crash (sweepTemporaries; DESIGN-box.md,
// Paths notes).
func (a *Applier) caddyfileTmp(id string) string {
	return filepath.Join(filepath.Dir(a.caddyfile()), ".Caddyfile.box-"+id)
}

// installedDigest is d: the installed file's digest as it stands. A
// file that is missing or over the cap is neither prev nor new — a
// console's doing — and reads as "" so that it matches nothing.
func (a *Applier) installedDigest() (string, error) {
	b, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	var big *tooLargeError
	if errors.Is(err, fs.ErrNotExist) || errors.As(err, &big) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

// removeTemp removes the swap's temporaries for id if a crash left
// them: the Caddyfile's and each env file's. Not writes recovery
// reasons from: their bytes are never read.
func (a *Applier) removeTemp(id string) {
	if err := os.Remove(a.caddyfileTmp(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.logger.Warn("box: could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
	}
	a.removeEnvTemps(id)
}

// lock takes root's blocking lock (DESIGN-box.md, Paths: `lock`), shared
// with init and baseline. A crash releases it with the descriptor. When
// another holds it, lockWait (the console's) is told before the wait.
func (a *Applier) lock() (func(), error) {
	f, err := os.OpenFile(a.x("lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if a.lockWait != nil {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		a.lockWait()
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Run is one applier run: recover, then take, then each bundle, then
// Retention. It returns an error only for the full-disk end. Whatever
// ended it, the last thing it does is say whether it leaves work on disk
// (leftOnDisk).
func (a *Applier) Run(ctx context.Context) error {
	unlock, err := a.lock()
	if err != nil {
		a.logger.Error("box apply could not take its lock", zap.String("error", proof.Bound(err.Error())))
		a.leftOnDisk()
		return nil
	}
	defer unlock()
	err = a.run(ctx)
	a.leftOnDisk()
	return fullDiskOnly(err)
}

func (a *Applier) run(ctx context.Context) error {
	if err := a.recoverAll(ctx); err != nil {
		return err
	}
	for _, id := range a.take() {
		if err := a.process(ctx, id); err != nil {
			// The record stays; what else was taken stays in work/ with
			// it (I2's exceptions).
			return err
		}
	}
	a.sweep()
	return nil
}

// msgLeftOnDisk is the line a run that leaves work on disk ends with
// (I2's exceptions; DESIGN-box.md, "The applier unit"). The path unit
// starts the next run at once, so a cause that does not clear by itself
// loops it to its trigger limit, after which nothing runs until the
// console restarts it.
const msgLeftOnDisk = "box apply leaves work on disk: the path unit starts it again at once, and stops at its trigger limit (trigger-limit-hit) if the cause persists; once the console has cleared the cause, systemctl restart motel-box-apply.path, and again ten seconds later if that fails"

// leftOnDisk journals msgLeftOnDisk, once, if the run leaves what starts
// the next run at once: the record, an entry in work/, or an entry of
// in/ the run listed — every entry of in/ if it listed none. An entry
// that landed in in/ after the listing is the next run's work, not a
// stop, and is not counted. The line carries counts, never the names,
// which the motel uid chose.
func (a *Applier) leftOnDisk() {
	var left []string
	if exists(a.x("txn.json")) {
		left = append(left, "txn.json")
	}
	count := func(dir string, only map[string]bool) {
		entries, err := os.ReadDir(a.x(dir))
		if err != nil {
			left = append(left, dir+"/: cannot be listed")
			return
		}
		n := 0
		for _, e := range entries {
			if only == nil || only[e.Name()] {
				n++
			}
		}
		if n > 0 {
			left = append(left, dir+"/: "+strconv.Itoa(n))
		}
	}
	count("work", nil)
	count("in", a.listed)
	if len(left) > 0 {
		a.logger.Error(msgLeftOnDisk, zap.Strings("left", left))
	}
}

func fullDiskOnly(err error) error {
	if errors.Is(err, errFullDisk) {
		return err
	}
	return nil
}

// recoverAll settles what a crash left (step 9): the record by its phase,
// then the applier's own stale temporaries, then every work/ entry with
// no record behind it (States table, "(no record)").
func (a *Applier) recoverAll(ctx context.Context) error {
	rec, err := a.readRecord()
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		// Only root writes the record, whole, by rename: one that does
		// not read is a fault no table reasons about. It is kept, and
		// nothing is touched (States table, "record unreadable").
		a.logger.Error("box: txn.json cannot be read; nothing is applied until the console settles it by hand, as DESIGN-box.md's States table says for an unreadable record — it may hold the only copy of the previous Caddyfile, so it is read before it is removed", zap.String("error", proof.Bound(err.Error())))
		return errUnsettled
	default:
		if err := a.recoverRecord(ctx, rec); err != nil {
			return err
		}
	}
	a.sweepTemporaries()
	entries, err := os.ReadDir(a.x("work"))
	if err != nil {
		a.logger.Error("box: work/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return errUnsettled
	}
	for _, e := range entries {
		id, ok := a.bundleEntry("work", e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(a.x("work"), e.Name())
		if a.settled(id) {
			a.removeEntry(path)
			continue
		}
		// Taken, or verified, and no record: nothing changed.
		t := &txn{rec: record{ID: id, Origin: originApplier}, entry: path}
		msg := msgInterrupted
		if v, err := a.readResult(id); err == nil && v.Phase == phaseVerified { // keep what it said
			t.rec.Commit, t.rec.Path, t.rec.Signer, t.rec.BoxWebhook = v.Commit, v.Path, v.Signer, v.BoxWebhook
			t.rec.OutOfBand, t.rec.Apps, t.rec.Diff = v.OutOfBand, v.Apps, v.Diff
			t.rec.SecretsAdded, t.rec.SecretsChanged, t.rec.SecretsRemoved = v.SecretsAdded, v.SecretsChanged, v.SecretsRemoved
			t.rec.SecretsOutOfBand = v.SecretsOutOfBand
			// A `verified` that named secrets was a push with env files
			// in its set: the catalogue's text names the box's files.
			if len(v.SecretsAdded)+len(v.SecretsChanged)+len(v.SecretsRemoved)+len(v.SecretsOutOfBand) > 0 {
				msg = msgInterruptedFiles
			}
		}
		a.finish(t, phaseFailed, msg)
		a.removeVerified(id) // if `failed` could not be written over it
	}
	return nil
}

// settled reports whether a push has already ended (step 9): a terminal
// result on disk, or one this run journaled when it could not write it
// (I3). An entry of a settled push is never run again; the guarantee
// lasts while Retention keeps the result. A result that does not read,
// is not filed under its own id, or whose phase is not terminal, counts
// as none (readResultFile): out/ is root's alone and written whole by
// rename, so only a disk fault or the console makes a bad one, and the
// push is then judged afresh on its merits.
func (a *Applier) settled(id string) bool {
	if phase, ok := a.wrote[id]; ok && terminal(phase) {
		return true
	}
	r, err := a.readResult(id)
	return err == nil && terminal(r.Phase)
}

// removeVerified removes out/<id>.json if it reads as `verified`: the
// stale answer of an attempt whose `failed` could not be written in its
// place, which nothing else would ever replace — a marker with no result
// is Retention's to settle. A file the applier cannot identify stays.
func (a *Applier) removeVerified(id string) {
	if r, err := a.readResult(id); err == nil && r.Phase == phaseVerified {
		a.removeResult(id)
	}
}

// sweepTemporaries removes the temporaries a crash can leave: the swap's
// in /etc/motel and /etc/motel/secrets, and writeDurable's beside
// txn.json, applied.json and the results. Every one is in a directory
// only root writes.
func (a *Applier) sweepTemporaries() {
	a.removeEnvTemps("")
	etc := filepath.Dir(a.caddyfile())
	var stale []string
	if names, err := filepath.Glob(filepath.Join(etc, ".Caddyfile.box-*")); err == nil {
		stale = append(stale, names...)
	}
	stale = append(stale, a.x(".txn.json.tmp"), a.x(".applied.json.tmp"))
	if names, err := filepath.Glob(filepath.Join(a.x("out"), ".*.json.tmp")); err == nil {
		stale = append(stale, names...)
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.logger.Warn("box: could not remove a temporary", zap.String("error", proof.Bound(err.Error())))
		}
	}
}

// bundleID is the id an entry of in/ or work/ is named for, if it is
// named as a bundle: `<id>.tar`.
func bundleID(name string) (string, bool) {
	id, ok := strings.CutSuffix(name, ".tar")
	return id, ok && isRequestID(id)
}

// bundleEntry classifies an entry of in/'s or work/'s listing (dir is
// "work"): a regular file named `<id>.tar` is a bundle, and its id is
// returned; anything else has no legal result name and is removed with
// an error-level line and no result (step 9).
func (a *Applier) bundleEntry(dir, name string) (string, bool) {
	path := filepath.Join(a.x(dir), name)
	fi, err := os.Lstat(path)
	if id, named := bundleID(name); err == nil && fi.Mode().IsRegular() && named {
		return id, true
	}
	kind := "missing"
	if err == nil {
		kind = fi.Mode().Type().String()
	}
	a.logger.Error("box: removing an entry that is not a bundle; no result", zap.String("entry", proof.Bound(name)), zap.String("type", kind))
	if err := a.removeDurable("entry:remove", path); err != nil {
		a.logger.Error("box: could not remove an entry that is not a bundle", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(err.Error())))
	}
	return "", false
}

// take renames every entry of in/ into work/, whatever it is (step 9),
// removes what is not a bundle, and returns the bundles' ids in the
// order of their markers' `posted` time, unmarked last. The order
// decides only which bundle is tried first.
func (a *Applier) take() []string {
	in, work := a.x("in"), a.x("work")
	entries, err := os.ReadDir(in)
	if err != nil {
		a.logger.Error("box: in/ cannot be listed", zap.String("error", proof.Bound(err.Error())))
		return nil
	}
	a.listed = make(map[string]bool, len(entries))
	var ids []string
	for _, e := range entries {
		name := e.Name()
		a.listed[name] = true
		src := filepath.Join(in, name)
		landed, err := a.takeOne(in, work, name)
		if err != nil && landed {
			// The rename landed and a directory's fsync failed: the take
			// stands, not known durable. Should in/'s unlink not persist,
			// the entry comes back after its push has settled and is
			// removed below while its result is kept.
			a.logger.Error("box: an entry was taken but the take is not known durable", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(err.Error())))
			err = nil
		}
		if err != nil {
			// The rename did not land. The entry must leave in/ all the
			// same (I2): removed where it stands, with a failed result
			// if it is a regular file named as a bundle whose push has
			// not already settled.
			a.logger.Error("box: could not take an entry", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(err.Error())))
			fi, lerr := os.Lstat(src)
			id, named := bundleID(name)
			bundle := named && lerr == nil && fi.Mode().IsRegular()
			// The result before the entry's removal, as every row orders
			// them: a crash between leaves the entry with its result, and
			// the next run removes it as settled.
			if bundle && !a.settled(id) {
				a.finish(&txn{rec: record{ID: id, Origin: originApplier}}, phaseFailed, installFailed(err))
			}
			if rerr := a.removeDurable("entry:remove", src); rerr != nil {
				a.logger.Error("box: could not remove an entry from in/", zap.String("entry", proof.Bound(name)), zap.String("error", proof.Bound(rerr.Error())))
			}
			continue
		}
		id, ok := a.bundleEntry("work", name)
		if !ok {
			continue
		}
		// A push settles once (step 9): a bundle whose push already
		// ended — back in in/ after a power loss beat its unlink's
		// durability, or replayed by the motel uid — is removed and
		// never run again.
		if a.settled(id) {
			a.logger.Warn("box: removed a bundle whose push has already settled", zap.String("id", id))
			a.removeEntry(filepath.Join(work, name))
			continue
		}
		ids = append(ids, id)
	}
	posted := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		if m, err := readMarker(filepath.Join(a.x("stage"), id+".auth")); err == nil {
			posted[id] = m.Posted
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		pi, iok := posted[ids[i]]
		pj, jok := posted[ids[j]]
		if iok != jok {
			return iok
		}
		return iok && pi.Before(pj)
	})
	return ids
}

// takeOne is one rename from in/ into work/, durable in both
// directories. landed says the rename happened, whatever came after it.
func (a *Applier) takeOne(in, work, name string) (landed bool, err error) {
	if err := a.fail("take"); err != nil {
		return false, err
	}
	if err := os.Rename(filepath.Join(in, name), filepath.Join(work, name)); err != nil {
		return false, err
	}
	if err := a.syncTake(work, in); err != nil {
		return true, err
	}
	a.crash("take")
	return true, nil
}

// syncTake fsyncs both directories of a take, work/ first.
func (a *Applier) syncTake(work, in string) error {
	if err := a.fail("take:sync"); err != nil {
		return err
	}
	if err := syncDir(work); err != nil {
		return err
	}
	return syncDir(in)
}

// process is one bundle through steps 9 to 16 and the transaction. It
// returns errFullDisk or errUnsettled when the run must stop, nil once
// the bundle has its terminal result.
func (a *Applier) process(ctx context.Context, id string) error {
	t := &txn{rec: record{ID: id, Origin: originApplier}, entry: filepath.Join(a.x("work"), id+".tar")}
	// 9. Read once: no symlink followed, no FIFO waited on, a regular
	// file, the body cap plus one byte. Nothing below touches the file.
	data, err := readFile(t.entry, maxBody, false)
	if a.hooks.read != nil {
		a.hooks.read(id)
	}
	var big *tooLargeError
	switch {
	case errors.As(err, &big):
		a.finish(t, phaseRefused, "bundle: larger than 16 MiB")
		return nil
	case err != nil:
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	b, err := proof.ReadBundle(data)
	if err != nil {
		return a.checked(t, asRefusal(err))
	}
	t.rec.Commit, t.rec.Path = b.Commit.ID, b.Path
	base, err := readApplied(a.x(""))
	if errors.Is(err, fs.ErrNotExist) {
		err = errors.New(msgNoBaseline)
	}
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	installed, err := readFile(a.caddyfile(), proof.MaxCaddyfile, true)
	if err != nil {
		a.finish(t, phaseFailed, installFailed(err))
		return nil
	}
	// 10 to 15.
	vd, err := check(ctx, a.verifier, b, installed, base)
	if err != nil {
		return a.checked(t, err)
	}
	t.rec.Signer, t.rec.BoxWebhook, t.rec.Apps, t.rec.OutOfBand = vd.signer, vd.host, vd.apps, vd.outOfBand
	t.rec.Prev, t.rec.PrevSHA256, t.rec.NewSHA256 = installed, vd.prevSum, vd.newSum
	// 15a. Only now, every proof and every signature passed: the box's
	// key opens the secrets, and what stands in secretsDir decides which
	// env files are in the set. The box's errors here name the box's
	// files.
	plan, err := a.planSecrets(vd.secrets, base, b.Path)
	if err != nil {
		var f *failure
		var ref *refusal
		if !errors.As(err, &f) && !errors.As(err, &ref) {
			err = &failure{msg: installFailedFiles(err)}
		}
		return a.checked(t, err)
	}
	// 16. Active? `activating` and `reloading` are waited out (running);
	// anything but `active` then refuses.
	state, err := a.running(ctx)
	if err != nil {
		// The set is known from 15a: with env files in it, the
		// catalogue's text names the box's files.
		msg := installFailed(err)
		if len(plan.entries) > 0 {
			msg = installFailedFiles(err)
		}
		a.finish(t, phaseFailed, msg)
		return nil
	}
	switch {
	case state == "active":
	case transient(state): // still on its way after the wait
		a.finish(t, phaseRefused, msgStillStarting)
		return nil
	default:
		a.finish(t, phaseRefused, msgNotRunning)
		return nil
	}
	t.rec.Diff = caddyfileDiff(installed, b.Caddyfile)
	t.next = b.Caddyfile
	plan.into(t)
	return a.install(ctx, t)
}

// checked ends a bundle that did not pass a check: refused for a verdict
// on the push, failed for the box's own error (checking → failed), in
// the catalogue's text for it where it has one of its own.
func (a *Applier) checked(t *txn, err error) error {
	var ref *refusal
	var f *failure
	switch {
	case errors.As(err, &ref):
		t.named = ref.ids
		a.finish(t, phaseRefused, ref.msg)
	case errors.As(err, &f):
		a.finish(t, phaseFailed, f.msg)
	default:
		a.finish(t, phaseFailed, installFailed(err))
	}
	return nil
}
