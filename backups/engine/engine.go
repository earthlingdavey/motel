// Package engine is one backup run: it reads the plan, and for each app
// that declares a backup it looks for the data, dumps the databases,
// uploads, checks that what it uploaded is in the snapshot, and removes
// the plaintext copies — each step a unit of its own, with only what
// that step needs.
//
// The engine runs as root and is root-equivalent (it starts system
// units and makes mounts), so it touches as little as it can: it never
// reads or removes anything an app wrote — it opens an app's
// directories only as O_PATH, to name them (pin.go) — parses the
// credential file, secrets and all, to refuse before any unit one that
// is not root's alone, one the manager could not read or would refuse
// whole, and one that names a b2: repository or names it in another
// file (repositoryInUse),
// otherwise passing its path to the manager — and takes nothing from a
// unit but an exit status and a small JSON file, of which it keeps only
// what it recognises, cleaned (record.Text).
package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/smallhoursorg/motel/backups/dump"
	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/plan"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

// Config is where things are. Every field is a constant of the
// installation, none of it an operator's or an app's string.
type Config struct {
	ConfigDir string // /etc/motel: what the plan unit sees, for the Caddyfile's imports
	EnvFile   string // /etc/motel-backup/repository.env: root-only; read by the manager, and parsed here to refuse before any unit what repositoryInUse refuses
	// OldEnvFile is where an earlier version of this branch kept the
	// credential file. It is never read: named, when it is there and
	// EnvFile is not, so that a box set up by hand is told to run setup
	// rather than left wondering.
	OldEnvFile string
	StateDir   string // /var/lib/motel-backup
	RunDir     string // /run/motel-backup
	Self       string // /usr/bin/motel-backup
	Restic     string // /usr/bin/restic
	// BindsTo is the engine's own service when it has one, so that the
	// manager ends its units if it dies; empty when run from a shell.
	BindsTo string
}

// Accounts the units run as.
const (
	dataUser   = "motel"        // owns the apps' data; the only uid that reads a live database
	backupUser = "motel-backup" // holds the credential while restic runs; runs nothing else
)

// A command and the helpers it starts are one version, or the helper
// does nothing. An upgrade leaves a command under way alone (the
// owner, 2026-09-27) — its upload is restic's and finishes, where a
// stopped one was sent again whole — and the helpers it starts from
// then on, by path, are the new version's. Their answers are read
// strictly, which refuses another shape; an answer of the same shape
// that means something else is what nothing would see. So each unit
// started from this program is told which program that is, in
// RunIdentityEnv, and a helper that is another exits
// OtherVersionStatus having done nothing: 75, which no helper exits
// otherwise and the manager never does.
const (
	RunIdentityEnv     = "MOTEL_BACKUP_RUN_IS"
	OtherVersionStatus = 75
)

// programOf is which program a file is: its hash. A command tells its
// helpers that of the file they are started from, as it is when the
// command begins — not its own: run from a build directory or another
// path, the helpers are the installed program, and a command that told
// them its own hash would find every one "upgraded" for good.
var programOf = func(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // cfg.Self, a constant of the installation
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WhichProgram is which program this is: the hash of the file it was
// started from, as the kernel still holds it — whatever is at its path
// by now. Not a version's name: two builds of one name are two
// programs, and a build with no name is one.
func WhichProgram() (string, error) { return whichProgram() }

var whichProgram = sync.OnceValues(func() (string, error) {
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
})

// otherVersion is a helper that was another version's, and did
// nothing: the unit's own word, so nothing had begun; and every helper
// after it would say the same, so nothing after it is tried.
func otherVersion(o unit.Outcome, role string) error {
	if o.Result != "exit-code" || o.ExitStatus != OtherVersionStatus {
		return nil
	}
	return repositoryWideError{upgradedError{refusedError{fmt.Errorf("the package was upgraded while this command was under way: its %s helper is another version's, and did nothing; the same command, run again, is of one version", role)}}}
}

// upgradedError is a helper that did nothing because it is another
// version's: nothing was found out, so it is no drill's verdict.
type upgradedError struct{ error }

func (e upgradedError) Unwrap() error { return e.error }

// retryLock is how long restic waits for a repository something else
// holds — a check takes it exclusively. A flag, because restic 0.18
// does not read it from the environment.
const retryLock = "2h"

// oneTry is one attempt at each request where restic's S3 backend
// makes ten (minio-go's default), for the upload, the fetch and the
// check, which have no clock (D4). A storage that takes the request
// and never answers holds each attempt until restic's five-minute
// stuck-request watchdog ends it, so ten, under restic's own retries,
// held a backup 100 minutes at the opening and 106 mid-upload; with
// one, restic's own quarter of an hour of retries, each printed, is
// all there is — 15 minutes at the opening, 21–36 mid-upload, 35–51
// mid-fetch, where the packs in flight are given up a batch at a time
// until restic's own lock monitor ends the command [M92, M93]. The
// check takes no lock, so mid-read it has no such end: a quarter of
// an hour for each five packs left in its group [M94].
// A storage that answers is retried as long as before, and a
// ten-minute silence still recovers. Every other backend ignores the
// option [M92]. Not the listings: there one 500 retried by restic, not
// minio-go, is a line on stderr [M92], and a listing with anything
// beside it is not believed (besides).
var oneTry = []string{"-o", "s3.retries=1"}

// Runner is the part of unit.Runner a run uses.
type Runner interface {
	Run(ctx context.Context, s unit.Spec) (unit.Outcome, error)
	Stop(name string) error
	// Wait waits for a unit to end on its own: one a setup left to
	// finish making the repository.
	Wait(ctx context.Context, name string) error
	// ManagerVersion is the manager's major version: setup refuses one
	// older than the properties here are built on.
	ManagerVersion(ctx context.Context) (int, error)
	// Sees says whether the manager sees a mount at the path: whether
	// a mount this command makes is one the manager can bind into a
	// unit.
	Sees(ctx context.Context, mountPoint string) (bool, error)
}

// initWait bounds the wait for a restic init an earlier setup left
// running: its own clock, and slack.
const initWait = 3 * time.Minute

// ErrBusy is returned when another run holds the lock.
var ErrBusy = errors.New("another backup run is in progress")

type run struct {
	cfg    Config
	r      Runner
	nonce  string
	dir    string // this run's own directory under RunDir
	status *record.Status
	prev   *record.Status
	mounts int // how many mount points this run has made, for their names
	// left are the apps on the last record that this run's plan no
	// longer has: said once, by this run, and never kept.
	left map[string]bool
	// keepDir keeps the run directory when the command ends: a unit
	// left running writes its output there, and the manager opens those
	// files in the unit's own first moments, which the end may come
	// before. The next lock holder's sweep removes it, after waiting.
	keepDir bool
	// swept is what open removed of an interrupted setup's leavings.
	swept []string
	// data is the data user's ids, looked up once as the command
	// begins, and account the backup account as the check found it.
	dataUID, dataGID int
	dataErr          error
	account          passwd
	// program is which program this is, told to every helper.
	program string
	// upgraded is set once a helper was another version's.
	upgraded error
	// given is which file each files item of the app under way is, by
	// its path in the upload unit's view: what the snapshot is held to.
	given map[string]identity
	// made is when restic says it made each app's snapshot of this run,
	// from the listing that verified it: its clean-run record's time.
	made map[string]time.Time
	// attended is a command someone started and can stop — a restore:
	// its reads of the repository have no backstop (readClock).
	attended bool
	// unanswered is the first unit this command gave up at its clock:
	// the repository not answering, which it will not do for the next
	// app either. startWithin sets it; the loops over apps stop on it.
	unanswered error
}

// preRestoreTag is on a snapshot a restore made of what it was about to
// restore over. Such a snapshot is there to undo that restore, by name:
// it is never what "the newest" means to a restore or to a drill — it
// holds, as often as not, the damage that was being restored over.
const preRestoreTag = "pre-restore"

// Run does one run and writes the record. The error is about the run as
// a whole; how each app fared is in the record.
func Run(ctx context.Context, cfg Config, r Runner) (*record.Status, error) {
	x, end, err := begin(ctx, cfg, r)
	if x == nil {
		return nil, err
	}
	defer end()
	if err != nil {
		return x.finish(err)
	}
	return x.finish(x.apps(ctx))
}

// begin is how a run, a restore and a drill each start: the one lock,
// the last record, whatever an earlier one left running or mounted taken
// away, and a directory of its own. With an error and a run, the lock is
// held and the record is still to be written; end removes the directory
// and releases the lock.
func begin(ctx context.Context, cfg Config, r Runner) (x *run, end func(), err error) {
	// The lock, the wait for an init a setup left running, and the sweep
	// of what it left come first, whatever the box is: a first setup
	// interrupted mid-init leaves no credential file, and its init is
	// still every lock holder's to wait for.
	x, end, err = open(ctx, cfg, r, nil)
	if err != nil {
		return x, end, err
	}
	if _, err := os.Lstat(cfg.EnvFile); errors.Is(err, fs.ErrNotExist) {
		end()
		return nil, nil, fmt.Errorf("backups are not set up: %s is not there%s", cfg.EnvFile, OldEnvFileNote(cfg))
	} else if err != nil {
		end()
		return nil, nil, fmt.Errorf("backups are not set up: %s: %w", cfg.EnvFile, err)
	}
	// With the run in hand and the lock held: a run records the refusal
	// as its own error and a drill as one that could not begin, so that
	// status fails with the red unit rather than reporting the run
	// before as ok for three hours.
	if err := programsInstalled(cfg); err != nil {
		return x, end, err
	}
	// The repository too, where setup refuses it and every unit would
	// run restic's own B2 backend: a b2: file is one written by hand, or
	// before setup refused b2:.
	if err := repositoryInUse(cfg.EnvFile); err != nil {
		return x, end, err
	}
	// The data user's lookup, made as the command opened: a lookup that
	// failed stops the command here, in its own words, whether or not
	// anything after would have used its answer.
	if x.dataErr != nil {
		return x, end, x.dataErr
	}
	if x.account, err = accountReady(ctx); err != nil {
		return x, end, err
	}
	if err := x.seen(ctx); err != nil {
		return x, end, err
	}
	return x, end, nil
}

// seen is whether the manager sees the mounts this command makes. It
// binds what it sees: where it does not — this command in a mount
// namespace of its own, which a service gets from any of a dozen
// properties [M22, M54] — it binds the bare mount point, root's and
// empty, into every unit in the place of an app's data, and an upload
// is a snapshot of empty directories. So the command makes one mount
// of its own, of nothing, asks the manager whether it has a mount unit
// for it, and refuses where it has none: once, before any unit is
// shown anything [M72]. (The manager's namespace is not this
// command's to read: /proc/1/ns/mnt asks for more capabilities than
// the run's service has.)
func (x *run) seen(ctx context.Context) error {
	probe := filepath.Join(x.dir, "seen")
	if err := os.Mkdir(probe, 0o700); err != nil {
		return err
	}
	defer os.Remove(probe) //nolint:errcheck // an empty directory of the run's own; removeRunDir is behind it
	if err := selfBind(probe); err != nil {
		return fmt.Errorf("making a mount of this command's own: %w", err)
	}
	seen, err := x.r.Sees(ctx, probe)
	if uerr := unmountDetach(probe); uerr != nil && err == nil {
		err = fmt.Errorf("taking away the mount of this command's own: %w", uerr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("whether the manager sees the mounts this command makes could not be asked: %w", err)
	}
	if !seen {
		where := "it was started from a shell that has one: run it from the box's own"
		if x.cfg.BindsTo != "" {
			where = fmt.Sprintf("%s is given one by a property it must not have — PrivateMounts=, ProtectSystem=, PrivateTmp=, PrivateNetwork= and their kin: systemctl cat %s shows it, a drop-in among what it lists", x.cfg.BindsTo, x.cfg.BindsTo)
		}
		return fmt.Errorf("this command runs in a mount namespace of its own: the manager does not see the mounts it makes, and would show every unit an empty directory in the place of an app's data; nothing was uploaded; %s", where)
	}
	return nil
}

// Sweep takes away what a killed command left — the units it
// recorded, stopped by those names, and what it left mounted under the
// run directory, made private and taken away — under the run lock, as
// the next command would. The package's preremove asks it at a remove,
// while the program is still there: after it there is no next command
// (the owner, 2026-09-27: the engine's own sweep, not a shell copy).
//
// No more than that: no state directories made on a box that never set
// backups up, no wait for an init a setup left running — which would
// leave the rest unswept — and nothing looked up or hashed.
func Sweep(ctx context.Context, cfg Config, r Runner) error {
	if _, err := os.Lstat(cfg.RunDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := runDirUsable(cfg.RunDir); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(cfg.RunDir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return (&run{cfg: cfg, r: r}).sweep()
}

// programPath is the file a command hashes for its helpers: the
// program running, where it is the installed one — replaced under it
// since it started, the kernel still holds the old file, and the old
// command must not take the new one's hash — and the installed file
// where it was started from anywhere else, whose helpers are the
// installed program.
func programPath(self, running string) string {
	if strings.TrimSuffix(running, " (deleted)") == self {
		return "/proc/self/exe"
	}
	return self
}

// open is begin without the credential file: what setup, which is
// about to write that file, shares with a run. say, when there is
// someone to tell, hears of a wait for an earlier setup's init.
func open(ctx context.Context, cfg Config, r Runner, say func(string)) (x *run, end func(), err error) {
	running, _ := os.Readlink("/proc/self/exe")
	program, err := programOf(programPath(cfg.Self, running))
	if err != nil {
		return nil, nil, fmt.Errorf("which version of motel-backup this is could not be read: %w", err)
	}
	// The state dir is where the status record is read from by anyone;
	// everything else is root's alone. Units reach staging through
	// binds the manager makes, not by walking here.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil { //nolint:gosec // holds status.json, which is for everyone to read
		return nil, nil, err
	}
	// Said again, because a mode given to mkdir is cut by the caller's
	// umask, and sudo hands down a shell's.
	if err := os.Chmod(cfg.StateDir, 0o755); err != nil { //nolint:gosec // as above
		return nil, nil, err
	}
	for _, d := range []string{cfg.RunDir, filepath.Join(cfg.StateDir, "staging"), filepath.Join(cfg.StateDir, "restore")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, err
		}
	}
	if err := runDirUsable(cfg.RunDir); err != nil {
		return nil, nil, err
	}
	unlock, err := lock(filepath.Join(cfg.RunDir, "lock"))
	if err != nil {
		return nil, nil, err
	}
	nonce, err := newNonce()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	prev, err := record.Read(filepath.Join(cfg.StateDir, "status.json"))
	unreadable := ""
	if err != nil {
		// A record that cannot be read costs what it remembered, and is
		// said; it does not cost the box its backups.
		unreadable = record.Text(fmt.Sprintf("the previous record could not be read and was replaced: %v", err))
		prev = &record.Status{Apps: map[string]*record.App{}}
	}
	uid, gid, derr := dataOwner(ctx)
	x = &run{cfg: cfg, r: r, nonce: nonce, dir: filepath.Join(cfg.RunDir, nonce), prev: prev, dataUID: uid, dataGID: gid, dataErr: derr, program: program,
		status: &record.Status{Started: time.Now().UTC(), Warning: unreadable, Apps: map[string]*record.App{}, Listed: prev.Listed, Unlisted: prev.Unlisted, LastDrill: prev.LastDrill, LastCheck: prev.LastCheck, CheckNext: prev.CheckNext, CheckSince: prev.CheckSince}}
	if err := x.awaitInit(ctx, say); err != nil {
		return x, unlock, err
	}
	if err := x.sweep(); err != nil {
		return x, unlock, err
	}
	if err := x.sweepStaged(say); err != nil {
		return x, unlock, err
	}
	if err := os.Mkdir(x.dir, 0o700); err != nil {
		return x, unlock, err
	}
	// Every mount under it has been taken away by then, each by its own
	// defer — but a mount that would not go is the app's own data, bound
	// here, and root must never walk into it deleting: removeRunDir does
	// not recurse.
	return x, func() {
		if !x.keepDir {
			removeRunDir(x.dir)
		}
		unlock()
	}, nil
}

// finish writes the record, whatever became of the run. An app the run
// did not reach is recorded as not run — never as whatever the last
// run found, under this run's date — and keeps only when it was last
// ok.
func (x *run) finish(runErr error) (*record.Status, error) {
	statusPath := filepath.Join(x.cfg.StateDir, "status.json")
	if runErr != nil {
		x.status.Error = record.Text(runErr.Error())
		for name, old := range x.prev.Apps {
			if _, reached := x.status.Apps[name]; !reached && old != nil && !x.left[name] {
				x.status.Apps[name] = &record.App{Class: record.NotRun, Detail: "the run ended before it reached this app", Looked: old.Looked, LastOK: old.LastOK, LastSnapshot: old.LastSnapshot,
					RestoreProven: old.RestoreProven, RestoreDrill: old.RestoreDrill}
			}
		}
	}
	x.status.Finished = time.Now().UTC()
	if err := record.Write(statusPath, x.status); err != nil {
		return x.status, errors.Join(runErr, err)
	}
	return x.status, runErr
}

// dataOwner is the data user's ids, as they were looked up when the
// command began.
func (x *run) dataOwner() (uid, gid int, err error) { return x.dataUID, x.dataGID, x.dataErr }

func (x *run) apps(ctx context.Context) error {
	p, err := x.plan(ctx)
	if err != nil {
		return err
	}
	x.status.Root = p.Root
	// An app that has left the plan with snapshots in the repository — a
	// block deleted, an import that stopped matching — is said by the
	// run that finds it gone. Once: this record drops it, as the
	// operator may have meant it to, and nothing after remembers.
	var left []string
	x.left = map[string]bool{}
	for name, old := range x.prev.Apps {
		if _, planned := p.Apps[name]; planned {
			continue
		}
		x.left[name] = true
		if old != nil && old.LastSnapshot != nil {
			left = append(left, fmt.Sprintf("%s no longer declares a backup and is not backed up any more: its last snapshot is %s, of %s.", name, record.Short(old.LastSnapshot.ID), record.When(old.LastSnapshot.Time)))
		}
	}
	sort.Strings(left)
	for _, l := range left {
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(l))
	}
	x.forget(ctx, p)
	x.sweepFetched(ctx)
	// Name order, but what failed last run goes last: a failure that is
	// one app's own — and ends the run, as every restic exit 1 does —
	// costs its neighbours one run, not every run.
	names := p.Names()
	sort.SliceStable(names, func(i, j int) bool {
		return !x.failedLast(names[i]) && x.failedLast(names[j])
	})
	var stop *record.App // set once the repository refuses for a reason every app shares
	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stop != nil {
			x.status.Apps[name] = &record.App{Class: record.NotAttempted, Detail: stop.Detail, Looked: backupdecl.SharedDir(p.Root, name)}
		} else {
			app, repositoryWide := x.app(ctx, p.Root, name, p.Apps[name])
			x.status.Apps[name] = app
			if repositoryWide {
				stop = x.stopAfter(name, app.Detail)
			}
			// Said in the repository too, once nothing of the app's run is
			// left to change how it ended: what a rebuilt box, which has
			// no record, has to go on.
			if app.Class == record.OK {
				x.vouch(ctx, name, app.Snapshot.ID)
			}
		}
		x.carryLastOK(name)
		// A backup nobody has restored is a hypothesis: an app's first
		// good backup has its snapshot fetched and checked, there and
		// then. After that — proven or not — it is the drill's: a drill
		// that keeps failing is not re-fetched, in full, every hour. Not
		// where the repository has just not answered — the app's record
		// given up at its clock: no verdict, so the next run drills it.
		if app := x.status.Apps[name]; app.Class == record.OK && app.RestoreProven == nil && app.RestoreDrill == nil && ctx.Err() == nil && x.unanswered == nil {
			if x.firstDrill(ctx, name, app) {
				// An upgrade leaves no verdict: stopAfter says it instead.
				detail := ""
				if app.RestoreDrill != nil {
					detail = app.RestoreDrill.Detail
				}
				stop = x.stopAfter(name, detail)
				if x.upgraded != nil {
					// No verdict: the next run drills it again.
					x.status.Warning = strings.TrimSpace(x.status.Warning + " " + stop.Detail)
				}
			}
		}
		// A read or a write given up at its clock — a verify, a listing, a
		// size, a record — is a repository that will not answer the next
		// app either, whose upload nothing bounds.
		if stop == nil && x.unanswered != nil {
			stop = x.stopAfter(name, "")
		}
	}
	// Once the repository has refused for a reason every app shares,
	// asking it again is one more wait for the same answer.
	if stop == nil && ctx.Err() == nil {
		x.list(ctx)
	}
	return ctx.Err()
}

// list asks the repository, once, what it holds of every app, and
// writes beside each snapshot the record names when it was last there:
// a snapshot pruned off the box is otherwise "the last good backup" for
// as long as nothing replaces it. A listing that fails, or that cannot
// be trusted, is a warning and changes nothing — it is never what makes
// a snapshot look gone, nor a good backup bad.
func (x *run) list(ctx context.Context) {
	named := func(app *record.App) (out []*record.Snapshot) {
		if app == nil {
			return nil
		}
		for _, snap := range []*record.Snapshot{app.Snapshot, app.LastOK, app.LastSnapshot} {
			if snap != nil {
				out = append(out, snap)
			}
		}
		if app.RestoreProven != nil {
			out = append(out, &app.RestoreProven.Snapshot)
		}
		return out
	}
	nothing := true
	for _, app := range x.status.Apps {
		nothing = nothing && len(named(app)) == 0
	}
	kept := filepath.Join(x.cfg.StateDir, "listing.err")
	if nothing {
		// Nothing to list, so nothing a listing failed to say: an earlier
		// failure was about snapshots this record no longer names, and
		// kept, it would turn status unhealthy for good.
		x.status.Unlisted = nil
		_ = os.Remove(kept)
		return
	}
	snaps, _, err := x.snapshots(ctx, "listing", "", "motel")
	if ctx.Err() != nil {
		return
	}
	// The unit's stderr is in a file, so the journal has none of it, and
	// the record is for everyone to read, so restic's words — which can
	// say where the repository is — do not go there either, but for the
	// id in that one line. They are kept for root, until a listing is
	// answered.
	said, _ := os.ReadFile(filepath.Join(x.dir, ".listing.err"))
	if err != nil {
		where := ""
		if len(bytes.TrimSpace(said)) > 0 && os.WriteFile(kept, said, 0o600) == nil && os.Chmod(kept, 0o600) == nil { //nolint:gosec // a constant name under root's own state dir
			where = fmt.Sprintf(" What restic said is in %s, root's to read.", kept)
		}
		if x.status.Unlisted == nil {
			since := time.Now().UTC()
			x.status.Unlisted = &since
		}
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf("the repository could not be listed, so nothing is known of whether it still holds each snapshot named here: %v.%s", err, where)))
		return
	}
	_ = os.Remove(kept) // an answered listing: what was said of an earlier one is history
	x.status.Unlisted = nil
	held := map[string]bool{} // app, a slash — no app's name holds one — and the id
	for _, s := range snaps {
		for _, app := range s.Apps {
			held[app+"/"+s.ID] = true
		}
	}
	now := time.Now().UTC()
	for name, app := range x.status.Apps {
		for _, snap := range named(app) {
			// What this run itself saw written, or fetched, is not called
			// gone by this run's listing: a store that lists a new object
			// late [not measured of any] would turn every good backup into
			// a missing one until the next run. The next listing judges it.
			sawItself := snap.Seen != nil && !snap.Seen.Before(x.status.Started)
			if held[name+"/"+snap.ID] || sawItself {
				snap.Seen = &now
			}
		}
	}
	x.status.Listed = &now
}

func (x *run) failedLast(name string) bool {
	old := x.prev.Apps[name]
	return old != nil && old.Class == record.Failed
}

func (x *run) carryLastOK(name string) {
	app, old := x.status.Apps[name], x.prev.Apps[name]
	if old != nil {
		// A backup run does not unprove a restore.
		app.RestoreProven, app.RestoreDrill = old.RestoreProven, old.RestoreDrill
		app.LastOK = old.LastOK
		if app.LastSnapshot == nil { // unless the repository has just said
			app.LastSnapshot = old.LastSnapshot
		}
	}
	if app.Snapshot != nil {
		app.LastSnapshot = app.Snapshot
		if app.Class == record.OK {
			app.LastOK = app.Snapshot
		}
	}
}

// forget empties and removes the staging directory of every app that
// no longer declares a backup: what a killed run copied there would
// otherwise stay for good. Names only are read here — the parent is
// root's — and what is inside is removed by a unit, as always.
func (x *run) forget(ctx context.Context, p *plan.Plan) {
	warn := func(format string, a ...any) {
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf(format, a...)))
	}
	entries, err := os.ReadDir(filepath.Join(x.cfg.StateDir, "staging"))
	if err != nil {
		warn("staging could not be looked through for what apps that no longer declare a backup left there: %v.", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if _, declared := p.Apps[name]; declared || !backupdecl.ValidAppName(name) || !e.IsDir() {
			continue
		}
		dir := filepath.Join(x.cfg.StateDir, "staging", name)
		// Plaintext that stays is said, not shrugged at: the run goes
		// on, and the record carries it.
		if err := x.clean(ctx, name, dir); err != nil {
			warn("plaintext copies of %s, which no longer declares a backup, may still be in %s: emptying it failed: %v.", name, dir, err)
		} else if err := os.Remove(dir); err != nil { // rmdir: refuses a directory the unit left anything in
			warn("%s could not be removed: %v.", dir, err)
		}
	}
}

// mountPoint is whether something is mounted on dir itself. Nothing of
// the engine's is mounted on its run directory; a mount there would
// have the lock, the list of units and a run's files written, and
// removed, through it.
var mountPoint = func(dir string) (bool, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 && unescapeMount(f[4]) == dir {
			return true, nil
		}
	}
	return false, nil
}

// runDirUsable refuses a run directory that is itself a mount point.
func runDirUsable(dir string) error {
	mounted, err := mountPoint(dir)
	if err != nil {
		return fmt.Errorf("whether %s is itself a mount point could not be read: %w", dir, err)
	}
	if mounted {
		return fmt.Errorf("%s is itself a mount point: nothing of motel-backup's is mounted there, and what it writes and removes there would be written and removed through the mount; unmount it (umount %s)", dir, dir)
	}
	return nil
}

// UnitPattern matches the name of every unit a run, a restore, a drill
// or a setup starts, and no other unit on the box.
const UnitPattern = "motel_backup_*"

var unitNameRe = regexp.MustCompile(`^motel_backup_([a-z]+[0-9]*)(?:_([a-z0-9-]{1,63}))?_[0-9a-f]{12}\.service$`)

// ParseUnitName reads what a unit does, and to which app's data, back
// out of a name that name made. A unit of the whole run has no app.
func ParseUnitName(unit string) (role, app string, ok bool) {
	m := unitNameRe.FindStringSubmatch(unit)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// name is a unit name no app's unit and no operator's can have (the
// underscores), unique to this run.
func (x *run) name(role, app string) string {
	n := "motel_backup_" + role
	if app != "" {
		n += "_" + app
	}
	return n + "_" + x.nonce + ".service"
}

// start records the unit's exact name before starting it, so that a run
// killed right here leaves the next one a list of what to stop — by
// name, never by pattern.
func (x *run) start(ctx context.Context, s unit.Spec) (unit.Outcome, error) {
	s.BindsTo = x.cfg.BindsTo
	// The two that remove plaintext are bound to nothing: a service
	// being stopped — an upgrade, a remove, systemctl stop — has a stop
	// job queued, and the manager refuses to start a unit bound to it
	// ("transaction is destructive" [M62]), so the copies a stopped run
	// had made stayed until the next run. They hold no credential and
	// no network, and are over in moments.
	if strings.HasPrefix(s.Name, "motel_backup_clean_") || strings.HasPrefix(s.Name, "motel_backup_unstage_") {
		s.BindsTo = ""
	}
	// A unit of this program is told which program that is.
	if len(s.Argv) > 0 && s.Argv[0] == x.cfg.Self {
		s.Environment = append(slices.Clone(s.Environment), RunIdentityEnv+"="+x.program)
	}
	f, err := os.OpenFile(filepath.Join(x.cfg.RunDir, "units"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return unit.Outcome{}, err
	}
	_, werr := fmt.Fprintln(f, s.Name)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return unit.Outcome{}, errors.Join(werr, cerr)
	}
	return x.r.Run(ctx, s)
}

// sweep stops whatever an earlier run recorded and did not live to
// stop. A unit that is already gone is the usual case.
func (x *run) sweep() error {
	if err := x.sweepUnits(); err != nil {
		return err
	}
	// Then the mounts such a run made, and its directory. The lock is
	// held, so whatever is here is nobody's. Every mount under the run
	// directory is taken away, by the topmost ones alone — those with
	// no mount above them, at paths only root can reach, since the
	// directory is root's and 0700 — each made private with all beneath
	// it and detached with all beneath it (unmountDetach). A mount there
	// of another name is root's own doing, and is taken away too: left,
	// the removal of the run's directories below would remove files
	// through it.
	// Never a nested one by its path: that runs through an app's
	// directory, which the app can re-aim at another's with a link
	// between the look and the call (the owner's review of #155); and a
	// nested one taken away under a parent still shared takes the disk
	// beneath the app's own directory with it [M71]. One that cannot be
	// made private is not detached as it is, and the sweep says so.
	mounts, err := mountsUnder(x.cfg.RunDir)
	if err != nil {
		return err
	}
	for _, m := range topmost(mounts) {
		if err := unmountDetach(m); err != nil {
			return fmt.Errorf("a mount from an earlier run is still there: %s: %w", m, err)
		}
	}
	entries, err := os.ReadDir(x.cfg.RunDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			removeRunDir(filepath.Join(x.cfg.RunDir, e.Name()))
		}
	}
	return nil
}

// topmost is the mounts with no other of them above: the ones a
// recursive private and a detach take away with all beneath them.
func topmost(mounts []string) []string {
	var out []string
	for _, m := range mounts {
		above := false
		for _, o := range mounts {
			if o != m && strings.HasPrefix(m, o+"/") {
				above = true
				break
			}
		}
		if !above {
			out = append(out, m)
		}
	}
	return out
}

// removeRunDir removes a run's directory: its files, and its mount
// points, which are empty directories once unmounted. It never
// descends. A mount point that is still mounted holds an app's data,
// and rmdir refuses it; it is left for the next sweep, which unmounts
// first.
func removeRunDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name())) //nolint:gosec // root's own run directory, and an entry it made; unlink or rmdir, never a walk
	}
	_ = os.Remove(dir) //nolint:gosec // as above
}

// mountsUnder lists the mount points beneath dir, deepest first.
var mountsUnder = func(dir string) ([]string, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) > 4 && strings.HasPrefix(unescapeMount(f[4]), dir+"/") {
			out = append(out, unescapeMount(f[4]))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// unescapeMount undoes mountinfo's octal escapes (space, tab, newline,
// backslash).
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// awaitInit waits for the restic init a setup that did not finish left
// running — recorded under RunDir/init-unit, not stopped: stopped half
// way it would leave a repository no password opens — before anything
// here looks at or makes a repository.
func (x *run) awaitInit(ctx context.Context, say func(string)) error {
	file := filepath.Join(x.cfg.RunDir, "init-unit")
	raw, err := os.ReadFile(file) //nolint:gosec // root's own file under /run
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := strings.TrimSpace(string(raw))
	if name != "" {
		if say != nil {
			say("waiting for the restic init a setup that did not finish left running: " + name)
		}
		// Bounded by the command's own context too: an interrupt ends
		// the wait at once, and leaves the marker for the next lock
		// holder — the init it names is still to be waited for.
		within, cancel := context.WithTimeout(ctx, initWait)
		defer cancel()
		if err := x.r.Wait(within, name); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("a restic init from an earlier setup is still running after %s (%w); if it must be ended: systemctl stop %s", initWait, err, name)
		}
	}
	return os.Remove(file)
}

func (x *run) sweepUnits() error {
	list := filepath.Join(x.cfg.RunDir, "units")
	raw, err := os.ReadFile(list) //nolint:gosec // root's own file under /run
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(raw)) {
		// The file is root's own, and still: a name the engine does not
		// write is not the manager's to be asked to stop.
		if _, _, ok := ParseUnitName(name); !ok {
			continue
		}
		if err := x.r.Stop(name); err != nil {
			return fmt.Errorf("a unit from an earlier run is still there and could not be stopped: %w", err)
		}
	}
	return os.Remove(list)
}

// sweepStaged removes what a setup that did not live to the end left
// beside the credential file — its staged file, or the one envfile
// makes on the way to it, and nothing else: a copy an operator keeps
// there under another name is theirs — once the init that may still
// read it has been waited for. Said, where there is someone to tell;
// what was removed is in x.swept.
func (x *run) sweepStaged(say func(string)) error {
	dir := filepath.Dir(x.cfg.EnvFile)
	// A directory that is a link is not root's own, and is setup's to
	// refuse; nothing is removed through it.
	if st, err := os.Lstat(dir); err != nil || st.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !envfile.IsLeftover(e.Name(), filepath.Base(x.cfg.EnvFile)) {
			continue
		}
		f := filepath.Join(dir, e.Name())
		if err := os.Remove(f); err != nil {
			return fmt.Errorf("a file an interrupted setup left could not be removed: %w", err)
		}
		if say != nil {
			say("removed a file an interrupted setup left: " + f)
		}
		x.swept = append(x.swept, f)
	}
	return nil
}

func (x *run) plan(ctx context.Context) (*plan.Plan, error) { return x.planWith(ctx, "") }

// planWith reads the plan, keeping the unit's stderr in stderrFile when
// one is named: what setup shows at the terminal, where an operator is
// waiting for the reason. A run leaves it in the journal — the
// adapter quotes the Caddyfile, and the record is everyone's to read.
func (x *run) planWith(ctx context.Context, stderrFile string) (*plan.Plan, error) {
	out := filepath.Join(x.dir, "plan.json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("plan", ""), Description: "motel backup: read what each app declares",
		Argv: []string{x.cfg.Self, "plan"},
		// Not the data user: the config dir holds the apps' env files,
		// readable by its group. In its own namespaces, since it shares
		// an account with the unit that will hold the credential.
		User: backupUser, SameUIDNamespaces: true,
		Binds:      []unit.Bind{{Source: x.cfg.ConfigDir}},
		StdoutFile: out, StderrFile: stderrFile,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	if err := otherVersion(o, "plan"); err != nil {
		return nil, err
	}
	if !o.OK() {
		if stderrFile != "" {
			if said, err := os.ReadFile(stderrFile); err == nil && len(bytes.TrimSpace(said)) > 0 { //nolint:gosec // written by the manager into root's own run dir
				return nil, fmt.Errorf("the Caddyfile could not be turned into a plan (exit %d): %s", o.ExitStatus, record.Text(string(said)))
			}
		}
		return nil, fmt.Errorf("the Caddyfile could not be turned into a plan (exit %d); `journalctl -u %s` has the reason", o.ExitStatus, x.name("plan", ""))
	}
	raw, err := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, err
	}
	return plan.Decode(raw)
}

// app backs one app up. repositoryWide reports a failure that every
// other app would meet too — and meet slowly.
//
// tags go on the snapshot beside the app's own; a restore names what it
// backs up first with preRestoreTag.
func (x *run) app(ctx context.Context, root, name string, decl *backupdecl.Config, tags ...string) (app *record.App, repositoryWide bool) {
	shared := backupdecl.SharedDir(root, name)
	app = &record.App{Looked: shared}
	fail := func(class record.Class, format string, a ...any) (*record.App, bool) {
		app.Class, app.Detail = class, fmt.Sprintf(format, a...)
		return app, false
	}

	// Plaintext copies do not outlive the run that made them, and do
	// not wait for the next run that gets this far either: staging is
	// emptied before anything else is looked at — an earlier run may
	// have died, and this one may be about to find the data gone — and
	// again at the end.
	staging, err := x.staging(name)
	if err != nil {
		return fail(record.Failed, "%v", err)
	}
	if err := x.clean(ctx, name, staging); err != nil {
		return fail(record.Failed, "emptying staging before the run: %v", err)
	}

	// Whether the data is there is decided here, on the real
	// filesystem, never from inside a unit, where a path that was not
	// bound looks exactly like a path that does not exist. And only "no
	// such file" means absent: anything else is an error.
	//
	// It is decided by opening it (pin.go): what the units are shown is
	// the directory that was found here, not whatever its name leads to
	// by the time the manager binds it.
	rootPin, err := pinRoot(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(record.Failed, "looking at the liveswap root: %v", err)
	}
	var sharedPin pin
	if err == nil {
		defer rootPin.close()
		sharedPin, err = rootPin.beneath(name + "/shared")
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// "Not deployed yet" is the one absence a run exits 0 on, so it
		// is said only of an app nothing has ever been backed up of. The
		// record remembers a snapshot; and where it has none for the app
		// — a rebuilt box, a new disk, a purge, or an app that really is
		// new — the repository is asked, every run that finds it so: it
		// remembers what the box does not, and an answer of "none" is
		// not kept, so it cannot go stale.
		old := x.prev.Apps[name]
		last := (*record.Snapshot)(nil)
		if old != nil {
			last = old.LastSnapshot
		}
		if last == nil {
			var wide bool
			if last, wide, err = x.lastInRepository(ctx, name); err != nil {
				app.Class, app.Detail = record.Failed, fmt.Sprintf("%s does not exist, and the repository could not be asked whether this app was ever backed up: %v", shared, err)
				return app, wide
			}
			app.LastSnapshot = last
		}
		if last != nil {
			return fail(record.DataMissing, "%s does not exist, and this app was last backed up on %s (snapshot %s): its data is gone, or the disk it lives on is not mounted", shared, record.When(last.Time), record.Short(last.ID))
		}
		return fail(record.Pending, "%s does not exist yet: the app has not been deployed", shared)
	case errors.Is(err, errLink):
		return fail(record.Failed, "%s is reached through a symbolic link, which a backup does not follow; liveswap itself takes a bind mount there, not a link", shared)
	case err != nil:
		return fail(record.Failed, "looking at %s: %v", shared, err)
	}
	defer sharedPin.close()
	if !sharedPin.isDir() {
		return fail(record.Failed, "%s is not a directory", shared)
	}
	// The Caddyfile's author is not root, and the upload unit reads any
	// file it is shown: a root written to make <root>/<app>/shared land
	// on something that is not an app's data is refused by whose it is.
	if uid, _, err := x.dataOwner(); err != nil || sharedPin.owner() != uid {
		return fail(record.Failed, "%s does not belong to the %s user, so it is not an app's data dir (owner uid %d)", shared, dataUser, sharedPin.owner())
	}

	defer func() {
		if err := x.clean(context.WithoutCancel(ctx), name, staging); err != nil {
			if app.Class == record.OK {
				app.Class = record.Incomplete
			}
			app.Detail = strings.TrimSpace(fmt.Sprintf("%s (plaintext copies are still in %s: they could not be removed: %v)", app.Detail, staging, err))
		}
	}()

	declFile := filepath.Join(x.dir, name+".plan.json")
	raw, _ := json.Marshal(decl)
	if err := os.WriteFile(declFile, raw, 0o644); err != nil { //nolint:gosec // read by units running as other users; it is the app's own declaration
		return fail(record.Failed, "%v", err)
	}
	if err := os.Chmod(declFile, 0o644); err != nil { //nolint:gosec // as above; the umask may have cut it
		return fail(record.Failed, "%v", err)
	}

	sharedSource, unmountShared, err := x.bound(sharedPin)
	if err != nil {
		return fail(record.Failed, "%s: %v", shared, err)
	}
	defer unmountShared()

	excludeFile := filepath.Join(x.dir, name+".exclude")
	if err := os.WriteFile(excludeFile, []byte(excludes(name, decl)), 0o644); err != nil { //nolint:gosec // read by the upload unit; it holds the app's own declared paths
		return fail(record.Failed, "%v", err)
	}
	if err := os.Chmod(excludeFile, 0o644); err != nil { //nolint:gosec // as above; the umask may have cut it
		return fail(record.Failed, "%v", err)
	}

	dumped := x.dump(ctx, name, sharedSource, staging, declFile, decl, app)
	if ctx.Err() != nil {
		return fail(record.Failed, "interrupted before anything was uploaded")
	}
	// Nothing is uploaded of an app whose databases nobody copied, and
	// no app after it is tried: its helpers are the same program's.
	if x.upgraded != nil {
		app.Class, app.Detail = record.Failed, record.Text(x.upgraded.Error())
		return app, true
	}
	binds, masked, present, unpin := x.view(name, sharedPin, staging, declFile, decl, app)
	defer unpin()
	if dumped+present == 0 {
		return fail(record.Failed, "nothing that is declared could be read: %s", firstDetail(app.Items))
	}

	binds = append(binds, unit.Bind{Source: excludeFile, Dest: excludePath})
	id, class, detail, wide := x.upload(ctx, name, binds, masked, tags)
	app.Class, app.Detail = class, detail
	if id == "" {
		return app, wide
	}
	// restic has just said it wrote it: the repository holds it now.
	made := time.Now().UTC()
	app.Snapshot = &record.Snapshot{ID: id, Time: made, Seen: &made}
	if err := x.verify(ctx, name, id, app); err != nil {
		app.Class, app.Detail = record.Incomplete, fmt.Sprintf("snapshot %s was made, but what is in it could not be checked: %v", record.Short(id), err)
		return app, false
	}
	for _, it := range app.Items {
		if !it.OK && app.Class == record.OK {
			app.Class, app.Detail = record.Incomplete, fmt.Sprintf("snapshot %s lacks %s %q: %s", record.Short(id), it.Kind, it.Path, it.Detail)
		}
	}
	return app, false
}

// bound mounts what is pinned in the run's own directory and returns
// the path to give the manager as a bind source.
func (x *run) bound(p pin) (source string, unmount func(), err error) {
	x.mounts++
	source = filepath.Join(x.dir, fmt.Sprintf("mount-%d", x.mounts))
	unmount, err = p.mountAt(source)
	return source, unmount, err
}

// staging is the app's own directory for plaintext copies: at a fixed
// path made of a name that matched the app alphabet, under a parent
// only root can write, owned by the data user. Root makes it and never
// looks inside.
func (x *run) staging(app string) (string, error) {
	dir := filepath.Join(x.cfg.StateDir, "staging", app)
	uid, gid, err := x.dataOwner()
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if err := os.Lchown(dir, uid, gid); err != nil {
		return "", err
	}
	return dir, nil
}

func (x *run) clean(ctx context.Context, app, staging string) error {
	return x.cleanAs(ctx, "clean", app, staging)
}

// cleanAs is the one unit that empties a directory of plaintext as the
// data user, under the role it plays: "clean" for a dump's staging,
// "unstage" for what a restore fetched.
func (x *run) cleanAs(ctx context.Context, role, app, staging string) error {
	o, err := x.start(ctx, unit.Spec{
		Name: x.name(role, app), Description: "motel backup: remove " + app + "'s plaintext copies",
		Argv: []string{x.cfg.Self, "clean"},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{{Source: staging, Dest: "/staging", Writable: true}},
	})
	if err != nil {
		return err
	}
	if !o.OK() {
		return fmt.Errorf("exit %d", o.ExitStatus)
	}
	return nil
}

// dump runs the dump unit and records each database; it returns how
// many copies are in staging. The unit parses what the app chose, so it
// is the app's own kind of sandbox: the data user in its own
// namespaces, no network, no credential, one app.
func (x *run) dump(ctx context.Context, app, shared, staging, declFile string, decl *backupdecl.Config, rec *record.App) int {
	if len(decl.SQLite) == 0 {
		return 0
	}
	failAll := func(format string, a ...any) int {
		for _, p := range decl.SQLite {
			rec.Items = append(rec.Items, record.Item{Kind: "sqlite", Path: p, Detail: fmt.Sprintf(format, a...)})
		}
		return 0
	}
	out := filepath.Join(x.dir, app+".dump.json")
	o, err := x.start(ctx, unit.Spec{
		Name: x.name("dump", app), Description: "motel backup: copy " + app + "'s databases",
		Argv: []string{x.cfg.Self, "dump"},
		User: dataUser, SameUIDNamespaces: true,
		Binds: []unit.Bind{
			// Writable: SQLite reads a WAL database only where it can
			// create the -shm beside it.
			{Source: shared, Dest: "/shared", Writable: true},
			{Source: staging, Dest: "/staging", Writable: true},
			{Source: declFile, Dest: "/plan.json"},
		},
		StdoutFile: out,
	})
	if err != nil {
		return failAll("the dump unit: %v", err)
	}
	if x.upgraded = otherVersion(o, "dump"); x.upgraded != nil {
		return failAll("%v", x.upgraded)
	}
	raw, rerr := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	var results []dump.Result
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if rerr != nil || dec.Decode(&results) != nil || len(results) != len(decl.SQLite) {
		return failAll("the dump unit said nothing usable (exit %d)", o.ExitStatus)
	}
	// All of it is checked before any of it is recorded: the unit
	// handled the app's bytes, and what it says is believed only where
	// it is an answer to what was asked, in words this side knows.
	for i, p := range decl.SQLite {
		if results[i].Path != p {
			return failAll("the dump unit answered about %q where %q was asked", record.Text(results[i].Path), p)
		}
		if !dump.Known(results[i].Class) {
			return failAll("the dump unit answered %q, which is not an answer", record.Text(string(results[i].Class)))
		}
	}
	n := 0
	for i, p := range decl.SQLite {
		res := results[i]
		it := record.Item{Kind: "sqlite", Path: p, OK: res.Class == dump.OK}
		if it.OK {
			n++
		} else {
			it.Detail = record.Text(string(res.Class) + ": " + res.Detail)
		}
		rec.Items = append(rec.Items, it)
	}
	return n
}

// view is what the upload unit sees: each declared files path at
// /backup/<app>/files/<path>, the copies at /backup/<app>/sqlite, and
// the declaration itself, so that a snapshot says what it is a snapshot
// of. The paths inside are the same on every box, whatever its liveswap
// root. A declared database inside a files path is masked there, with
// its sidecars: the live file is never what gets uploaded.
func (x *run) view(app string, shared pin, staging, declFile string, decl *backupdecl.Config, rec *record.App) (binds []unit.Bind, masked []string, present int, unpin func()) {
	base := "/backup/" + app
	binds = []unit.Bind{{Source: staging, Dest: base + "/sqlite"}, {Source: declFile, Dest: base + "/plan.json"}}
	x.given = map[string]identity{}
	var undo []func()
	unpin = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	for _, p := range decl.Files {
		it := record.Item{Kind: "files", Path: p}
		// Pinned, not named: see pin.go. A link anywhere in a declared
		// path is refused — the upload unit reads any file it is shown,
		// so what it is shown is never for a link the app made to say.
		item, err := shared.beneath(p)
		if err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				it.Detail = "missing: no such file or directory under the shared dir"
			case errors.Is(err, errLink):
				it.Detail = "a symbolic link is in the way, and a backup does not follow one: declare the real path"
			default:
				it.Detail = err.Error()
			}
			rec.Items = append(rec.Items, it)
			continue
		}
		undo = append(undo, item.close)
		if kind := item.kind(); kind != "" {
			it.Detail = "it is " + kind + ", not a file or a directory, and a backup keeps only those"
			rec.Items = append(rec.Items, it)
			continue
		}
		// Which file it is, for the snapshot to be held to.
		given, err := item.identity()
		if err != nil {
			it.Detail = err.Error()
			rec.Items = append(rec.Items, it)
			continue
		}
		source, unmount, err := x.bound(item)
		if err != nil {
			it.Detail = err.Error()
			rec.Items = append(rec.Items, it)
			continue
		}
		undo = append(undo, unmount)
		x.given[path.Join(base, "files", p)] = given
		it.OK = true // until the snapshot says otherwise
		rec.Items = append(rec.Items, it)
		present++
		binds = append(binds, unit.Bind{Source: source, Dest: path.Join(base, "files", p)})
		for _, db := range decl.SQLite {
			if p == "." || db == p || strings.HasPrefix(db, p+"/") {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					masked = append(masked, path.Join(base, "files", db)+suffix)
				}
			}
		}
	}
	return binds, masked, present, unpin
}

// dataOwner is the data user's ids, and ownerOf those of the backup
// account as the account check found it; variables so a test can run
// where the accounts do not exist. Both by the lookup the check makes,
// getent: os/user reads /etc/passwd alone in this build, and an account
// a directory holds — which postinstall finds, and makes no local one
// beside — would be unknown here.
var (
	dataOwner = func(ctx context.Context) (uid, gid int, err error) { return ids(ctx, dataUser) }
	// ownerOf is the ids of an account that was looked up; a variable
	// so that a test that is not root can stand its own in.
	ownerOf = func(acct passwd) (uid, gid int) { return acct.uid, acct.gid }
	// selfBind makes a mount of a directory onto itself: a mount of
	// this command's own making, with nothing of an app's in it.
	selfBind = func(dir string) error { return bindMount(dir, dir) }
)

func ids(ctx context.Context, name string) (uid, gid int, err error) {
	acct, err := account(ctx, name)
	if err != nil {
		return 0, 0, err
	}
	if !acct.exists {
		return 0, 0, fmt.Errorf("the %s account is not there", name)
	}
	return acct.uid, acct.gid, nil
}

// excludePath is where the upload unit finds its exclude file: outside
// /backup/<app>, so it is not itself backed up.
const excludePath = "/backup-exclude"

// excludes is the restic exclude file for one app: every declared
// database that sits inside a declared files path, and its sidecars, by
// their exact paths in the unit's view. It stands behind the masks,
// which are made when the unit starts and cover only what exists then:
// a database the app creates, or puts back under its name, while restic
// walks is not masked, and is excluded all the same. (And what is
// excluded is not in the snapshot at all, not even as the mask's empty
// file.)
//
// restic reads each line as a pattern and expands $VAR in it
// [measured], so every pattern character is escaped and every dollar
// doubled: a database called app*.db excludes itself and not appX.db.
func excludes(app string, decl *backupdecl.Config) string {
	var b strings.Builder
	for _, p := range decl.Files {
		for _, db := range decl.SQLite {
			if p != "." && db != p && !strings.HasPrefix(db, p+"/") {
				continue
			}
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				b.WriteString(excludeEscaper.Replace(path.Join("/backup", app, "files", db)+suffix) + "\n")
			}
		}
	}
	return b.String()
}

var excludeEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `$`, `$$`)

var snapshotRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// upload runs restic. It returns the id of the snapshot this run made —
// from this run's own summary, never "the latest" — or none.
func (x *run) upload(ctx context.Context, app string, binds []unit.Bind, masked []string, tags []string) (id string, class record.Class, detail string, repositoryWide bool) {
	out := filepath.Join(x.dir, app+".summary.json")
	argv := append([]string{x.cfg.Restic, "backup", "--quiet", "--json", "--retry-lock", retryLock}, oneTry...)
	argv = append(argv, "--exclude-file", excludePath,
		"--host", "motel", "--tag", "motel", "--tag", "app:"+app)
	for _, tag := range tags {
		argv = append(argv, "--tag", tag)
	}
	s := resticUnit(x.name("upload", app), "motel backup: upload "+app, x.cfg.EnvFile, append(argv, "/backup/"+app), out)
	// An account of its own, so that the motel uid — the server,
	// every app — can neither read this process's environment nor
	// signal it; and one capability, to read files that account does not
	// own, in a view that holds this app and nothing else.
	s.Capabilities, s.Binds, s.Masked = []unit.Capability{unit.CapDACReadSearch}, binds, masked
	o, err := x.start(ctx, s)
	if err != nil {
		return "", record.Failed, fmt.Sprintf("the upload unit: %v", err), false
	}
	id = summaryID(out)
	switch {
	case o.OK() && id != "":
		return id, record.OK, "", false
	case o.OK():
		// A snapshot existing proves nothing, and neither does exit 0
		// without this run's own id.
		return "", record.Failed, "restic exited 0 but its summary names no snapshot", false
	case o.ExitStatus == 3 && id != "":
		return id, record.Incomplete, "restic could not read everything it was given (exit 3); snapshot " + record.Short(id) + " holds the rest", false
	}
	detail, repositoryWide = resticFailure(o)
	return "", record.Failed, detail + "; `journalctl -u " + x.name("upload", app) + "` has restic's own words", repositoryWide
}

// stopAfter is what a run says of every app after name, once there is
// no point asking the repository for them: in the words of why — a
// helper of another version, a repository that did not answer, or one
// that refused name's backup for a reason every app shares — never as
// though what befell name were theirs.
func (x *run) stopAfter(name, detail string) *record.App {
	switch {
	case x.upgraded != nil:
		return &record.App{Detail: record.Text(x.upgraded.Error())}
	case x.unanswered != nil:
		return &record.App{Detail: record.Text(fmt.Sprintf("the repository did not answer (%v), so nothing more was asked of it", x.unanswered))}
	}
	return &record.App{Detail: record.Text(fmt.Sprintf("the repository refused %s's backup for a reason every app shares: %s", name, detail))}
}

// vouch writes into the repository the record that this run ended ok
// on app's snapshot id (vouchArgv). One that could not be written is
// said, and costs nothing else: the backup is sound, and only a rebuilt
// box goes without the word for it.
//
// It is given up at listClock, as a listing is (the owner, 2026-09-30):
// a write of some four hundred bytes, which a storage that stops
// answering must not turn into a run that never ends.
func (x *run) vouch(ctx context.Context, app, id string) {
	// The snapshot first, before anything a cut could take.
	warn := func(what string) {
		x.status.Warning = strings.TrimSpace(x.status.Warning + " " + record.Text(fmt.Sprintf("%s: snapshot %s is a complete backup, and the record that says so %s: a restore on a rebuilt box will not know it.", app, record.Short(id), what)))
	}
	// Stopped — Ctrl-C, systemctl stop, a shutdown — after the app's run
	// ended ok and before its record was begun.
	if ctx.Err() != nil {
		warn("was not written: the command was stopped before it was begun")
		return
	}
	at, ok := x.made[app]
	if !ok {
		warn("was not written: its listing said nothing of when it was made")
		return
	}
	out := filepath.Join(x.dir, app+".vouch.json")
	o, err := x.startWithin(ctx, listClock, resticUnit(x.name("vouch", app), "motel backup: record in the repository that "+app+"'s backup ended ok", x.cfg.EnvFile, vouchArgv(x.cfg.Restic, app, id, at), out))
	// Only restic's own word that it failed is "could not": a stop, a
	// clock, a unit ended from outside or by a signal, an exit 0 that
	// names nothing — restic may have saved it first, and which is not
	// known here.
	switch {
	case err == nil && o.OK() && summaryID(out) != "":
		// Written: a stop that came as it finished changes nothing.
	case err == nil && o.Result == "exit-code" && o.ExitStatus == 3:
		warn("may not have been written: restic exited 3, having saved a snapshot of what it could read")
	case err == nil && o.Result == "exit-code" && o.ExitStatus == 11:
		warn("could not be written into the repository (it stayed locked by something else for " + vouchRetryLock + ", exit 11)")
	case err == nil && o.Result == "exit-code" && o.ExitStatus != 0:
		detail, _ := resticFailure(o)
		warn("could not be written into the repository (" + detail + ")")
	case err == nil && o.OK():
		warn("may not have been written: restic exited 0 and its summary names no snapshot")
	case err == nil:
		detail, _ := resticFailure(o)
		warn("may not have been written: " + detail)
	case ctx.Err() != nil:
		warn("may not have been written: the command was stopped while it was being written")
	default:
		warn("may not have been written: " + err.Error())
	}
}

// resticUnit is a unit that runs restic as the backup account, with the
// credential file the manager reads, the network and restic's own
// cache — what every restic unit has — and stdout into a file of the
// command's. The caller adds what its command needs beyond that.
//
// In UTC (the owner, 2026-09-30): restic stores a snapshot's time in
// the zone it runs in, and forget sorts each into its days by that zone
// [measured] — a Berlin box's 23:30 snapshot at +02:00 and its record
// at Z fell into different days, and --keep-daily kept the one and
// removed the other. A unit sees the box's /etc/localtime, so the zone
// is said here, for every restic alike.
func resticUnit(name, description, envFile string, argv []string, stdout string) unit.Spec {
	return unit.Spec{
		Name: name, Description: description, Argv: argv,
		User: backupUser, Network: true, EnvironmentFile: envFile,
		Environment:    []string{"RESTIC_CACHE_DIR=/var/cache/motel-backup", "HOME=/nonexistent", "TZ=UTC"},
		CacheDirectory: "motel-backup", StdoutFile: stdout,
	}
}

// resticFailure puts restic 0.18's exit statuses into words [measured].
// 1 is "anything else", and is never called "no repository": a wrong
// storage key ends, after a quarter of an hour of retrying, in exit 1
// and a message about a missing repository.
func resticFailure(o unit.Outcome) (detail string, repositoryWide bool) {
	if o.Result != "exit-code" {
		return "restic was ended by " + o.Result, false
	}
	// 200 and up are systemd's own, for a unit it could not set up —
	// 217 the user, 226 the namespace: restic never ran.
	if o.ExitStatus >= 200 && o.ExitStatus <= 243 {
		return fmt.Sprintf("systemd could not set the unit up (status %d), so restic never ran", o.ExitStatus), false
	}
	switch o.ExitStatus {
	case 10:
		return "there is no repository at the configured location (exit 10)", true
	case 11:
		return "the repository stayed locked by something else for " + retryLock + " (exit 11)", true
	case 12:
		return "the repository password is wrong (exit 12)", true
	case 1:
		return "restic failed (exit 1): the storage could not be reached, or refused the key, or something else went wrong", true
	}
	return fmt.Sprintf("restic failed (exit %d)", o.ExitStatus), false
}

// summaryID reads the snapshot id out of restic's --json output, of
// which --quiet leaves one line, the summary.
func summaryID(file string) string {
	var m struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if jsonLine(file, "summary", &m) && snapshotRe.MatchString(m.SnapshotID) {
		return m.SnapshotID
	}
	return ""
}

// jsonLine reads restic's --json output a line at a time and decodes
// into v the first line whose message_type is the one wanted.
func jsonLine(file, messageType string, v any) bool {
	raw, err := os.ReadFile(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var m struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal(line, &m) == nil && m.MessageType == messageType && json.Unmarshal(line, v) == nil {
			return true
		}
	}
	return false
}

// verify looks in the snapshot for every declared item that was given
// to restic. restic leaves out, silently and with exit 0, a file that
// vanishes while it runs [measured], so this is what stands between a
// declared path that disappeared and a run that says ok. `ls <id>
// <dir>` lists a directory's direct children, so each item is looked
// for in a listing of its parent.
func (x *run) verify(ctx context.Context, app, id string, rec *record.App) error {
	base := "/backup/" + app
	want := map[string][]int{} // parent dir -> the items expected in it
	for i, it := range rec.Items {
		if !it.OK {
			continue
		}
		full := itemPath(base, it)
		want[path.Dir(full)] = append(want[path.Dir(full)], i)
	}
	parents := make([]string, 0, len(want))
	for p := range want {
		parents = append(parents, p)
	}
	sort.Strings(parents)
	out := filepath.Join(x.dir, app+".ls.json")
	// One listing of every parent. Given a directory, restic 0.18 lists
	// it and its direct children and no deeper [measured: 2 nodes for a
	// parent, 409 for the same snapshot with no directory given, 404
	// with --recursive], so the listing is a handful of lines however
	// large the app — TestIntegrationResticLsOfADirectoryIsNotRecursive
	// is what says so if a later restic changes its mind. --no-lock: a listing must not fail
	// because a check holds the repository. The parents are the one
	// thing on any command line here that comes from a declaration — the
	// directory part of a declared path, after /backup/<app>/ — and
	// restic takes each as a path and nothing else: they follow "--",
	// start with "/", and like every argument are never expanded.
	o, err := x.startWithin(ctx, x.readClock(), resticUnit(x.name("verify", app), "motel backup: check "+app+"'s snapshot", x.cfg.EnvFile,
		append([]string{x.cfg.Restic, "ls", "--json", "--no-lock", "--", id}, parents...), out))
	if err != nil {
		return err
	}
	if !o.OK() {
		return fmt.Errorf("restic ls exited %d", o.ExitStatus)
	}
	wanted := map[string]bool{}
	for _, items := range want {
		for _, i := range items {
			wanted[itemPath(base, rec.Items[i])] = true
		}
	}
	nodes, made, err := lsNodes(out, wanted)
	if err != nil {
		return err
	}
	// When restic says it made the snapshot: the time its clean-run
	// record is given.
	if !made.IsZero() {
		if x.made == nil {
			x.made = map[string]time.Time{}
		}
		x.made[app] = made
	}
	for _, parent := range parents {
		for _, i := range want[parent] {
			it := &rec.Items[i]
			node, ok := nodes[itemPath(base, *it)]
			switch {
			case !ok:
				it.OK, it.Detail = false, "it is not in the snapshot: it disappeared while the backup ran"
			case it.Kind == "sqlite" && (node.Type != "file" || node.Size == 0):
				it.OK, it.Detail = false, fmt.Sprintf("in the snapshot it is a %s of %d bytes, not a database copy", record.Text(node.Type), node.Size)
			case it.Kind == "files" && node.Type != "file" && node.Type != "dir":
				// It was a file or a directory when it was pinned, and
				// the app's to replace since.
				it.OK, it.Detail = false, fmt.Sprintf("in the snapshot it is a %s, not a file or a directory", record.Text(node.Type))
			case it.Kind == "files":
				// A node of the right name and kind is not yet the file
				// that was given: shown a bare mount point in its place,
				// restic uploads an empty directory of that name [M54].
				// What was bound is the pinned file itself, and a bind
				// mount shows its inode and its owner [M63].
				given, pinned := x.given[itemPath(base, *it)]
				what := "file"
				if node.Type == "dir" {
					what = "directory"
				}
				switch {
				case !pinned:
					it.OK, it.Detail = false, "which file was given to the backup is not known, so the snapshot cannot be held to it"
				case node.Inode == nil:
					it.OK, it.Detail = false, "the listing does not say which file it is, so the snapshot cannot be held to what was given"
				case *node.Inode != given.inode || node.UID != given.uid || node.GID != given.gid:
					it.OK, it.Detail = false, fmt.Sprintf("in the snapshot it is not the %s that was given to the backup: it is inode %d, owner %d:%d, and what was given is inode %d, owner %d:%d — the upload was shown something else in its place",
						what, *node.Inode, node.UID, node.GID, given.inode, given.uid, given.gid)
				}
			}
		}
	}
	return nil
}

// lastInRepository asks the repository for the newest snapshot of app,
// or nil when it holds none. It is what a run falls back on where the
// record holds no snapshot of an app.
func (x *run) lastInRepository(ctx context.Context, app string) (last *record.Snapshot, repositoryWide bool, err error) {
	snaps, wide, err := x.history(ctx, app)
	if err != nil || len(snaps) == 0 {
		return nil, wide, err
	}
	return &snaps[len(snaps)-1].Snapshot, false, nil
}

// A listed snapshot is one the repository holds of an app.
type listed struct {
	record.Snapshot
	// Apps are whose it is, by its tags: one, as motel writes it —
	// though a tag can be added off the box — and none for a snapshot
	// with none.
	Apps []string
	// PreRestore: made by a restore, of what it then restored over.
	PreRestore bool
	// Vouches are the snapshots a clean-run record says a run ended ok
	// on: none, but for a record.
	Vouches []string
}

// newest is the newest snapshot a backup run made, or nil.
func newest(snaps []listed) *record.Snapshot {
	for i := len(snaps) - 1; i >= 0; i-- {
		if !snaps[i].PreRestore {
			return &snaps[i].Snapshot
		}
	}
	return nil
}

// history asks the repository for every snapshot of app, oldest first.
// It is what a restore on a rebuilt box starts from: the repository
// remembers what the box does not.
func (x *run) history(ctx context.Context, app string) (snaps []listed, repositoryWide bool, err error) {
	return x.snapshots(ctx, "history", app, "app:"+app)
}

// snapshots asks the repository for the snapshots of app — of every
// app, where app is empty — oldest first, of those tagged tag: the
// app's own, every app's (`motel`, which no clean-run record has), or
// the records (cleanTag). --no-lock: it reads, and a check that holds
// the repository exclusively must not fail it. Given up at listClock.
func (x *run) snapshots(ctx context.Context, role, app, tag string) (snaps []listed, repositoryWide bool, err error) {
	out := filepath.Join(x.dir, app+"."+role+".json")
	about := app
	switch {
	case tag == cleanTag:
		about = "which backups ended ok"
	case app == "":
		about = "every app"
	}
	// An app's history fails loudly or not at all, and its words belong
	// in the journal. A listing of every app's, or of the records, is
	// only believed when restic had nothing to say beside it (besides).
	stderr := ""
	if app == "" {
		stderr = filepath.Join(x.dir, "."+role+".err")
	}
	s := resticUnit(x.name(role, app), "motel backup: ask the repository about "+about, x.cfg.EnvFile,
		[]string{x.cfg.Restic, "snapshots", "--json", "--no-lock", "--host", "motel", "--tag", tag}, out)
	s.StderrFile = stderr
	o, err := x.startWithin(ctx, x.readClock(), s)
	if err != nil {
		return nil, false, err
	}
	if !o.OK() {
		detail, wide := resticFailure(o)
		return nil, wide, errors.New(detail)
	}
	raw, err := os.ReadFile(out) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, false, err
	}
	var said []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal(raw, &said); err != nil {
		return nil, false, fmt.Errorf("what restic said of its snapshots could not be read: %w", err)
	}
	asked := time.Now().UTC()
	for _, s := range said {
		if !snapshotRe.MatchString(s.ID) {
			return nil, false, fmt.Errorf("restic named a snapshot %q", record.Text(s.ID))
		}
		// The repository has just said it holds it.
		one := listed{Snapshot: record.Snapshot{ID: s.ID, Time: s.Time, Seen: &asked}}
		for _, tag := range s.Tags {
			one.PreRestore = one.PreRestore || tag == preRestoreTag
			if name, ok := strings.CutPrefix(tag, "app:"); ok {
				one.Apps = append(one.Apps, name)
			}
			if id, ok := strings.CutPrefix(tag, vouchesTag); ok && snapshotRe.MatchString(id) {
				one.Vouches = append(one.Vouches, id)
			}
		}
		snaps = append(snaps, one)
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	if stderr != "" {
		if err := x.besides(role); err != nil {
			return nil, false, err
		}
	}
	return snaps, false, nil
}

// besides says whether restic said anything on stderr beside a listing
// of every app's snapshots, or of the records — snapshots holds each such
// listing to it: it leaves a snapshot it cannot load out of the listing,
// says so on stderr alone, and exits 0 [measured, a cold cache], so an
// answer that came with anything beside it is not taken for the whole.
func (x *run) besides(role string) error {
	said, err := os.ReadFile(filepath.Join(x.dir, "."+role+".err")) //nolint:gosec // written by the manager into root's own run dir
	switch {
	case err != nil:
		return fmt.Errorf("what restic said beside the listing could not be read, so the listing is not believed: %w", err)
	case len(bytes.TrimSpace(said)) == 0:
		return nil
	}
	if m := ignoringRe.FindSubmatch(said); m != nil {
		return fmt.Errorf("restic could not load snapshot %s and left it out of the listing, so the listing is not believed", record.Short(string(m[1])))
	}
	return errors.New("restic had something to say beside the listing, so the listing is not believed")
}

// ignoringRe is what restic says of a snapshot file it cannot load.
var ignoringRe = regexp.MustCompile(`(?m)^Ignoring "([0-9a-f]{64})"`)

// itemPath is where a declared item sits in the snapshot.
func itemPath(base string, it record.Item) string {
	if it.Kind == "files" {
		return path.Join(base, "files", it.Path)
	}
	return path.Join(base, "sqlite", it.Path)
}

type lsNode struct {
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Which file it is, as restic found it in the unit's view: what a
	// bind mount shows is the bound file's own inode and owner [M63].
	Inode *uint64 `json:"inode"`
	UID   uint32  `json:"uid"`
	GID   uint32  `json:"gid"`
}

// lsNodes reads a listing a line at a time and keeps the nodes at the
// wanted paths, and nothing else: what it costs in memory does not
// depend on how long the listing is.
func lsNodes(file string, wanted map[string]bool) (nodes map[string]lsNode, made time.Time, err error) {
	f, err := os.Open(file) //nolint:gosec // written by the manager into root's own run dir
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	nodes = map[string]lsNode{}
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		var n struct {
			StructType string `json:"struct_type"`
			Path       string `json:"path"`
			// The snapshot's own line says when it was made.
			Time time.Time `json:"time"`
			lsNode
		}
		switch {
		case json.Unmarshal(lines.Bytes(), &n) != nil:
		case n.StructType == "snapshot" && made.IsZero():
			made = n.Time
		case n.StructType == "node" && wanted[n.Path]:
			nodes[n.Path] = n.lsNode
		}
	}
	return nodes, made, lines.Err()
}

func firstDetail(items []record.Item) string {
	for _, it := range items {
		if !it.OK {
			return fmt.Sprintf("%s %q: %s", it.Kind, it.Path, it.Detail)
		}
	}
	return "nothing is declared"
}

func newNonce() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// lock takes the one run lock without waiting, and says who has it
// when it is taken.
func lock(file string) (unlock func(), err error) {
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // a constant path under root's run dir
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(file) //nolint:gosec // as above
		f.Close()                      //nolint:errcheck,gosec // nothing was written
		return nil, fmt.Errorf("%w: %s", ErrBusy, strings.TrimSpace(string(holder)))
	}
	if err := f.Truncate(0); err == nil {
		fmt.Fprintf(f, "pid %d, since %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339)) //nolint:errcheck // a courtesy to whoever finds the lock held
	}
	return func() { f.Close() }, nil //nolint:errcheck,gosec // closing is what releases it
}
