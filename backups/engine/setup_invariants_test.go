package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
)

// The invariants of setup, each a table: every way a thing can happen
// against everything that must hold of it. A finding that fits a table
// is a row added, never a test of its own.

// Whatever leaves init running leaves it what it needs. Four ways to
// leave it: its clock, an interrupt, a runner that lost sight of it,
// and a stop it could not confirm. Whichever, the unit is not stopped,
// it is recorded for the next lock holder, its staged credential file
// and its run directory stay — the manager opens both in the unit's
// own first moments — and the operator is told to keep the password.
func TestWhateverLeavesInitRunningLeavesItWhatItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box, cancel func())
	}{
		{"its clock", func(b *box, _ func()) { b.hang = "init"; clockMet() }},
		{"an interrupt", func(b *box, cancel func()) {
			b.hang = "init"
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					cancel()
				}
			}
		}},
		{"a runner that lost sight of it", func(b *box, _ func()) {
			b.err["init"] = errors.New("motel_backup_init: its state could not be read 10 times running")
		}},
		{"a stop it could not confirm", func(b *box, _ func()) {
			b.err["init"] = fmt.Errorf("%w: motel_backup_init: (stopping it: timeout)", unit.ErrNotConfirmedGone)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.set(b, cancel)
			_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
			if err == nil || !strings.Contains(err.Error(), "keep the password shown above") {
				t.Fatalf("err = %v", err)
			}
			if len(b.stopped) != 0 {
				t.Fatalf("init was stopped: %v", b.stopped)
			}
			raw, rerr := os.ReadFile(filepath.Join(b.cfg.RunDir, "init-unit"))
			if rerr != nil || !strings.Contains(string(raw), "_init_") {
				t.Fatalf("init is not recorded for the next lock holder: %q %v", raw, rerr)
			}
			if _, err := os.Lstat(envfile.Staged(b.cfg.EnvFile)); err != nil {
				t.Fatal("the staged file init reads its environment from is gone")
			}
			if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the working file was written")
			}
			if runDirs(t, b) != 1 {
				t.Fatal("the run directory init writes its output to is gone")
			}
		})
	}
}

// A repository that was there already is opened with its lock — cat
// config without --no-lock, which writes a lock file and removes it —
// so that the opening proves, before the box is pointed at it, that the
// key may write and delete a lock file: it once only read, and a key that may
// read and not write was taken, every run after failing a quarter of an
// hour in [RC check 6, on B2's native API]. What restic said is read, verbatim from
// M4 (rc-results/M4-unlock.out) and M3: its own "error while unlocking:"
// line at exit 0 is a refusal; an exit 1 is one neutral line with what
// restic said, without its retry lines, cut at 300 runes, and the clock
// the same with its last line; an exit 11 is said with the
// holder, and setup goes on. However it is refused, setup ends before
// anything of its own is written.
func TestAKeyThatCannotWriteAnExistingRepositoryIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	// The remedy every lock-left row ends with, written out: plain
	// unlock from this box strips a live lock of a second machine of
	// the same hostname [M4 case E], so the from-this-box clause is
	// qualified.
	if unlockWhere != "from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise from another machine that holds it once the lock is half an hour old; or with restic unlock --remove-all sooner, when nothing else holds the repository" {
		t.Fatalf("the lock-left remedy is %q", unlockWhere)
	}
	// M4 case A, three times out of three: DELETE refused (403), exit 0.
	// As captured, the last line with no newline after it.
	const unlockRefused = "Remove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.\nerror while unlocking: client.RemoveObject: Access Denied."
	// M4 case B: DELETE failing (500), restic's retries, then exit 0.
	const unlockRetried = "Remove(<lock/9e10eeec08>) returned error, retrying after 1.056546101s: client.RemoveObject: 500 Internal Server Error\n" +
		"Remove(<lock/9e10eeec08>) returned error, retrying after 2.475348444s: client.RemoveObject: 500 Internal Server Error\n" +
		"Remove(<lock/9e10eeec08>) returned error, retrying after 3.850439386s: client.RemoveObject: 500 Internal Server Error\n" +
		"Remove(<lock/9e10eeec08>) returned error, retrying after 10.155761906s: client.RemoveObject: 500 Internal Server Error\n" +
		"Remove(<lock/9e10eeec08>) returned error, retrying after 8.853734653s: client.RemoveObject: 500 Internal Server Error\n" +
		"Remove(<lock/9e10eeec08>) returned error, retrying after 34.639635739s: client.RemoveObject: 500 Internal Server Error\n" +
		"error while unlocking: context canceled"
	// M4 case G: an exclusive lock held from another host, exit 11.
	const heldElsewhere = "unable to create lock in backend: repository is already locked exclusively by PID 175 on m4-elsewhere by root (UID 0, GID 0)\n" +
		"lock was created at 2026-10-25 12:51:50 (1.728985751s ago)\n" +
		"storage ID 1403fae9\n" +
		"the `unlock` command can be used to remove stale locks\n"
	// M3 (rc-results/M3-readonly-proxy.out): every write refused (403), exit 1.
	const writeRefused = "Save(<lock/b8e7993d85>) failed: client.PutObject: Access Denied.\nunable to create lock in backend: client.PutObject: Access Denied.\n"
	// RC check 6, on B2: the lock file's write retried to the clock.
	const b2 = "Save(<lock/0a11f6bb7c>) returned error, retrying after 1.494715917s: b2_get_upload_url: 401:"
	const load = "Load(<config/>, 0, 0) returned error, retrying after 552.330144ms: client.GetObject: 503 Slow Down"
	const remedy = ": a key that may not delete, or a storage that refuses or does not answer deletes, ends here; the lock stays until restic unlock removes it, with a key that may delete — not this one: " + unlockWhere
	const clock = "restic had not finished opening the repository with its lock within 200ms"
	const exit1 = "restic could not open the repository with its lock (exit 1): "
	for _, tc := range []struct {
		name    string
		set     func(b *box)
		refused string // in the error; "" where setup goes on
		said    string
	}{
		{"M4 A: restic wrote its lock file and could not remove it (DELETE refused, exit 0): refused, the Remove line quoted", func(b *box) {
			b.openErr = unlockRefused
		}, "restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.)" + remedy, ""},
		{"M4 B: the removal retried, then given up (DELETE failing, exit 0): refused, restic's last retry line quoted with its own line, the storage's 500 kept", func(b *box) {
			b.openErr = unlockRetried
		}, "restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/9e10eeec08>) returned error, retrying after 34.639635739s: client.RemoveObject: 500 Internal Server Error error while unlocking: context canceled)" + remedy, ""},
		{"M4 A's lines at exit 0 with no id printed: the lock left is named first", func(b *box) {
			b.openErr, b.probeOut = unlockRefused, ""
		}, "restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.)" + remedy, ""},
		{"a plain exit 0, no such line: ready", func(*box) {}, "", "repository ready"},
		{"M4 G: an exclusive lock held from elsewhere (exit 11): read again without the lock, the holder said as restic named it, setup goes on", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 11}, heldElsewhere
		}, "", "the repository is locked by another restic (exit 11) — a prune or a check from elsewhere: whether this key can write to it, or remove a lock file, was not checked; restic never passes a lock nothing holds by itself: if the holder is gone, restic unlock clears it from the holder's own host at once, from elsewhere once it is half an hour old, or with --remove-all when nothing else holds the repository (restic said: unable to create lock in backend: repository is already locked exclusively by PID 175 on m4-elsewhere by root (UID 0, GID 0)"},
		{"exit 11 after retried reads: the holder is said with its host, the retries gone before the cut", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 11}, strings.Repeat(load+"\n", 3)+heldElsewhere
		}, "", "(restic said: unable to create lock in backend: repository is already locked exclusively by PID 175 on m4-elsewhere by root (UID 0, GID 0)"},
		{"M3: every write refused (403, exit 1): the neutral line, all restic said, cleaned", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, writeRefused
		}, exit1 + "Save(<lock/b8e7993d85>) failed: client.PutObject: Access Denied. unable to create lock in backend: client.PutObject: Access Denied.", ""},
		{"M4 G's shape at exit 1: a cause before restic's trailing hint line is kept", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, "Fatal: unable to open repository at s3:http://e2e-s3:9000/box: wrong password or no key found\nthe `unlock` command can be used to remove stale locks\n"
		}, exit1 + "Fatal: unable to open repository at s3:http://e2e-s3:9000/box: wrong password or no key found the `unlock` command can be used to remove stale locks", ""},
		{"exit 1 on a read retried three times, then the Fatal (a storage that went away): the retries go before the cut, and the Fatal's cause is quoted", func(b *box) {
			// Three retry lines alone would fill record.Text's 300 runes
			// before the Fatal line.
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, strings.Repeat(load+"\n", 3)+"Fatal: unable to open repository at s3:http://e2e-s3:9000/box: Load(<config/>, 0, 0) failed: client.GetObject: 503 Slow Down\n"
		}, exit1 + "Fatal: unable to open repository at s3:http://e2e-s3:9000/box: Load(<config/>, 0, 0) failed: client.GetObject: 503 Slow Down", ""},
		{"exit 1 where the retries are all restic said: the last of them is quoted", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, strings.Repeat(load+"\n", 2)+b2+"\n"
		}, exit1 + b2, ""},
		{"exit 1 after restic wrote its lock and could not remove it: the neutral line, then restic's own word on the lock, and where it goes", func(b *box) {
			// The shape restic 0.18's lock.go gives — the deferred unlock
			// runs on every exit, so a failure after the lock's write is
			// followed by its removal's — not a capture: no storage of the
			// e2e's makes cat config fail after a lock it wrote.
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, "Load(<config/>, 0, 0) failed: client.GetObject: 503 Slow Down\nRemove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.\nerror while unlocking: client.RemoveObject: Access Denied.\nFatal: unable to open config file: client.GetObject: 503 Slow Down\n"
		}, "; and it could not remove the lock file it wrote (restic said: Remove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.): the lock stays until restic unlock removes it, with a key that may delete: " + unlockWhere, ""},
		{"exit 1 with nothing said: the neutral line, and that restic said nothing", func(b *box) {
			b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		}, "restic could not open the repository with its lock (exit 1), and said nothing", ""},
		{"exit 11, then the reread exits 1 after three retried reads: the reread's Fatal is quoted, its retries gone before the cut", func(b *box) {
			b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 11}, heldElsewhere
			b.outcome["reopen"], b.probeErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, strings.Repeat(load+"\n", 3)+"Fatal: unable to open repository at s3:http://e2e-s3:9000/box: Load(<config/>, 0, 0) failed: client.GetObject: 503 Slow Down\n"
		}, "restic could not open the repository (exit 1): Fatal: unable to open repository at s3:http://e2e-s3:9000/box: Load(<config/>, 0, 0) failed: client.GetObject: 503 Slow Down", ""},
		{"the write retried to the clock (RC check 6's 401): the neutral clock line, the retry line appended", func(b *box) {
			b.hang, b.openErr = "open", b2+" \n"
		}, clock + " (restic said: " + b2 + ")", ""},
		{"M4 B's removal retries cut by the clock: the same, the last retry line appended", func(b *box) {
			b.hang, b.openErr = "open", strings.SplitN(unlockRetried, "\nerror while unlocking", 2)[0]+"\n"
		}, clock + " (restic said: Remove(<lock/9e10eeec08>) returned error, retrying after 34.639635739s: client.RemoveObject: 500 Internal Server Error)", ""},
		{"the stop's own lines last, the retry line before them appended", func(b *box) {
			b.hang, b.openErr = "open", b2+" \nsignal terminated received, cleaning up\nunable to create lock in backend: context canceled\n"
		}, clock + " (restic said: " + b2 + ")", ""},
		{"a read retried to the clock: the Load line appended, judged by nobody here", func(b *box) {
			b.hang, b.openErr = "open", load+"\n"
		}, clock + " (restic said: " + load + ")", ""},
		{"the clock with nothing from restic: the line alone", func(b *box) { b.hang = "open" }, clock, ""},
		{"the clock says the lock file restic was writing may stay, and where restic unlock clears it", func(b *box) {
			b.hang, b.openErr = "open", b2+" \n"
		}, clock + " (restic said: " + b2 + "); " + lockLeft + ", with a key that may delete: " + unlockWhere, ""},
		{"a blank last line is nothing said: the line before it appended", func(b *box) {
			b.hang, b.openErr = "open", b2+" \n \n"
		}, clock + " (restic said: " + b2 + ")", ""},
		{"restic's words as the stop ends it are its last, and not the storage's: nothing appended", func(b *box) {
			b.hang, b.openErr = "open", "signal terminated received, cleaning up\n"
		}, clock, ""},
		{"the unit could not be set up", func(b *box) { b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 217} },
			"systemd could not set the unit up (status 217)", ""},
		{"the unit could not be started", func(b *box) { b.err["open"] = errors.New("starting motel_backup_open: no such user") },
			"no such user", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
			tc.set(b)
			if b.hang == "open" {
				clockMet()
			}
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if tc.refused == "" {
				if err != nil || !strings.Contains(m.saidAll(), tc.said) {
					t.Fatalf("err %v\nsaid:\n%s", err, m.saidAll())
				}
				if envOf(t, b.cfg.EnvFile)["RESTIC_PASSWORD"] != "its-own-password" {
					t.Fatal("the file was not put in place")
				}
				if b.outcome["open"].ExitStatus == 11 && (b.roles() != "plan probe open reopen" || !strings.Contains(strings.Join(b.spec("reopen").Argv, " "), "--no-lock")) {
					t.Fatalf("units %s; reopen %v", b.roles(), b.spec("reopen").Argv)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("err = %v\nsaid:\n%s", err, m.saidAll())
			}
			if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the working file was written")
			}
			if _, err := os.Lstat(envfile.Staged(b.cfg.EnvFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the staged file, and the key in it, was left behind")
			}
			if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "repository-id")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the repository's id was recorded")
			}
		})
	}
	// A repository this setup made was written by its init, which proved
	// the key may write and deleted nothing: it is opened with the lock
	// once, and restic's own word on a lock file it could not remove
	// refuses the key there too, the password said to be the
	// repository's.
	b, m := setupBox(t)
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err != nil || b.roles() != "plan probe init open" {
		t.Fatalf("err %v, units %s", err, b.roles())
	}
	b, m = setupBox(t)
	b.openErr = unlockRefused
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/bb19e73ef3>) failed: client.RemoveObject: Access Denied.)"+remedy+"; the password shown above is the repository's: keep it") {
		t.Fatalf("err %v, units %s", err, b.roles())
	}
	// A storage that does not show the repository just made for a
	// moment (exit 10 at the opening) is said as that, and is no
	// contradiction of the password's fate.
	b, m = setupBox(t)
	b.outcome["open"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), "the repository restic just made does not open yet: the storage may not show all of it for a moment; run setup again, which looks first and asks for the password; the password shown above is the repository's: keep it") {
		t.Fatalf("err %v, units %s", err, b.roles())
	}
	// That opening's clock, and its exit 1, are the same neutral lines,
	// made or not, with the password's fate.
	b, m = setupBox(t)
	b.hang, b.openErr = "open", load+"\n"
	// Shortened once init is under way: its answer is not raced
	// against the clock the opening waits out.
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			clockMet()
		}
	}
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), clock+" (restic said: "+load+"); "+lockLeftMade+", with a key that may delete: "+unlockWhere+"; the password shown above is the repository's: keep it") {
		t.Fatalf("err %v, units %s", err, b.roles())
	}
	b, m = setupBox(t)
	b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 1}, writeRefused
	if _, err := b.setup(t, m, "s3:http://e2e-s3:9000/box"); err == nil || !strings.Contains(err.Error(), exit1+"Save(<lock/b8e7993d85>) failed: client.PutObject: Access Denied. unable to create lock in backend: client.PutObject: Access Denied.; the password shown above is the repository's: keep it") {
		t.Fatalf("err %v, units %s", err, b.roles())
	}
	// One an earlier attempt's init made, opened with the password
	// shown, was written with the key of that attempt, not the one asked
	// for since: the opening holds that key to deleting the same, and
	// refuses it the same, the shown password said to be the
	// repository's (the opening proved it).
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed // past the opening: it may have made the repository
	inits := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if inits++; inits == 1 {
				// The second attempt's look finds a repository.
				b.hang = ""
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			}
		}
	}
	b.openErr = unlockRefused
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDNODELETE", "the-no-delete-secret"}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "restic opened the repository and could not remove the lock file it wrote") || !strings.Contains(err.Error(), "the password shown above is the repository's: keep it") {
		t.Fatalf("err = %v\nsaid:\n%s", err, m.saidAll())
	}
	if got, want := b.roles(), "plan probe init probe open"; got != want {
		t.Fatalf("units %s, want %s", got, want)
	}
	if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the working file was written with the key that cannot delete")
	}
}

// What an earlier opening said goes before the next one starts: a
// unit the manager never started, given up on at the clock, is not
// quoted with the earlier attempt's words. Here a wrong password's
// opening says something, and the second opening's start never comes.
func TestAnOpeningNeverStartedIsNotQuotedWithAnEarlierOnesWords(t *testing.T) {
	b, m := setupBox(t)
	clockMet()
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	b.outcome["open"], b.openErr = unit.Outcome{Result: "exit-code", ExitStatus: 12}, "Fatal: wrong password or no key found — the first attempt's words\n"
	opens := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_open_") {
			if opens++; opens == 2 {
				delete(b.outcome, "open")
				b.hang, b.openErr = "open", ""
			}
		}
	}
	m.answers = []string{"AKIDX", "the-secret", "not-it", "its-own-password"}
	_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
	if err == nil || !strings.Contains(err.Error(), "restic had not finished opening the repository with its lock within 200ms; ") || strings.Contains(err.Error(), "the first attempt's words") {
		t.Fatalf("err = %v\nsaid:\n%s", err, m.saidAll())
	}
}

// Ctrl-C during the opening is said with what it may leave: the lock
// file restic was writing — which restic never passes by itself [M75]
// — not "nothing has been written", which is the look's.
func TestAnInterruptDuringTheOpeningNamesTheLockItMayLeave(t *testing.T) {
	b, m := setupBox(t)
	b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
	ctx, cancel := context.WithCancel(context.Background())
	b.hang = "open"
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_open_") {
			cancel()
		}
	}
	m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
	_, err := Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: "+lockLeft {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(b.cfg.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the working file was written")
	}
	// After init made the repository, the interrupt during the opening
	// after it says the repository was made, not that nothing was.
	b, m = setupBox(t)
	ctx, cancel = context.WithCancel(context.Background())
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_open_") {
			b.hang = "open"
			cancel()
		}
	}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: "+lockLeftMade+"; the password shown above is the repository's: keep it" {
		t.Fatalf("err = %v\nsaid:\n%s", err, m.saidAll())
	}
	// On a repository an earlier attempt's init made, the interrupt's
	// fate names the password as the repository's.
	b, m = setupBox(t)
	b.hang = "probe"
	b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
	b.initOut, b.initErr = "", keyWriteFailed
	ctx, cancel = context.WithCancel(context.Background())
	inits := 0
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_init_") {
			if inits++; inits == 1 {
				b.hang = ""
				b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			}
		}
		if strings.Contains(s.Name, "_open_") {
			b.hang = "open"
			cancel()
		}
	}
	m.answers = []string{"AKIDX", "wrong", "stored", "AKIDRO", "the-read-only-secret"}
	_, err = Setup(ctx, b.cfg, b, SetupOptions{Repository: "s3:http://e2e-s3:9000/box", Terminal: m})
	if !errors.Is(err, context.Canceled) || err.Error() != "interrupted: "+lockLeft+"; keep the password shown above: restic init ran with it, and may have made the repository; run setup again, which looks first and asks for it if the repository is there" {
		t.Fatalf("err = %v\nsaid:\n%s", err, m.saidAll())
	}
}

// Every lock holder waits for an init left running, then sweeps what
// it was left: setup, a run and a drill alike, on a box with a
// credential file and on one with none but the staged file — a first
// setup interrupted mid-init leaves exactly that.
func TestEveryLockHolderWaitsForAnInitLeftRunningThenSweeps(t *testing.T) {
	const left = "motel_backup_init_0123456789ab.service"
	holders := []struct {
		name string
		hold func(b *box) error
	}{
		{"setup", func(b *box) error {
			_, err := b.setup(b.t, &term{answers: []string{"AKIDX", "the-secret", "stored"}}, "s3:http://e2e-s3:9000/box")
			return err
		}},
		{"a run", func(b *box) error { _, err := Run(context.Background(), b.cfg, b); return err }},
		{"a drill", func(b *box) error { _, _, err := Drill(context.Background(), b.cfg, b); return err }},
	}
	for _, box := range []string{"with a credential file", "with the staged file alone"} {
		for _, h := range holders {
			t.Run(box+", "+h.name, func(t *testing.T) {
				b, _ := setupBox(t)
				must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
				must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "init-unit"), []byte(left+"\n"), 0o600))
				must(t, os.MkdirAll(filepath.Join(b.cfg.RunDir, "0123456789ab"), 0o700))
				must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "0123456789ab", "init.err"), nil, 0o600))
				must(t, os.MkdirAll(filepath.Dir(b.cfg.EnvFile), 0o755))
				staged := envfile.Staged(b.cfg.EnvFile)
				must(t, envfile.Write(staged, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: "s3:http://e2e-s3:9000/box"}, {Key: "RESTIC_PASSWORD", Value: "shown"}}))
				if box == "with a credential file" {
					must(t, envfile.Write(b.cfg.EnvFile, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: "s3:http://e2e-s3:9000/box"}, {Key: "RESTIC_PASSWORD", Value: "x"}}))
				}
				err := h.hold(b)
				if len(b.waited) != 1 || b.waited[0] != left {
					t.Fatalf("%s did not wait for the init left running: waited %v (err %v)", h.name, b.waited, err)
				}
				if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the staged file, and its password, behind (err %v)", h.name, err)
				}
				if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "0123456789ab")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the run directory behind", h.name)
				}
				if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "init-unit")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left the marker after the wait", h.name)
				}
				if box == "with the staged file alone" && h.name != "setup" && (err == nil || !strings.Contains(err.Error(), "backups are not set up")) {
					t.Fatalf("%s on a box with no credential file: %v", h.name, err)
				}
			})
		}
	}
}

// A power cut at any point of the commit leaves a state the next setup
// reads rightly. Only a directory's sync makes its writes durable: the
// durable state at any moment is each directory as it was at its last
// sync. For each such state a run then writes a record against the
// repository the durable credential file names, and a setup onto the
// new repository must put that record aside whenever the file named
// the old one — never keep a record written against another
// repository. Where the durable state is whole, the record is kept.
func TestAPowerCutAtAnyPointOfTheCommitLeavesAStateTheNextSetupReads(t *testing.T) {
	const oldRepo, newRepo = "s3:http://e2e-s3:9000/old", "s3:http://e2e-s3:9000/box"
	const oldID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	type dirState map[string]string // file name → content, one directory, files alone
	snap := func(dir string) dirState {
		st := dirState{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				raw, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				st[e.Name()] = string(raw)
			}
		}
		return st
	}
	type durable struct{ state, etc dirState }
	key := func(d durable) string { return fmt.Sprintf("%v|%v", d.state, d.etc) }

	// The capture: a setup from the old repository to a new one, with a
	// record beside the old id, every hook noting the durable state
	// before and after it.
	b, m := setupBox(t)
	etc := filepath.Dir(b.cfg.EnvFile)
	must(t, os.MkdirAll(etc, 0o755))
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, envfile.Write(b.cfg.EnvFile, []envfile.Pair{{Key: "RESTIC_REPOSITORY", Value: oldRepo}, {Key: "RESTIC_PASSWORD", Value: "old"}}))
	must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "repository-id"), []byte(oldID+"\n"), 0o644))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	last := durable{snap(b.cfg.StateDir), snap(etc)}
	var states []durable
	seen := map[string]bool{}
	note := func() {
		if !seen[key(last)] {
			seen[key(last)] = true
			states = append(states, last)
		}
	}
	note()
	oldCommit, oldWrite := commit, writeID
	t.Cleanup(func() { commit, writeID = oldCommit, oldWrite })
	commit = func(from, to string) error { note(); err := oldCommit(from, to); note(); return err }
	writeID = func(path, id string) error { note(); err := oldWrite(path, id); note(); return err }
	syncDir = func(path string) error {
		note()
		switch path {
		case b.cfg.StateDir:
			last = durable{snap(b.cfg.StateDir), last.etc}
		case etc:
			last = durable{last.state, snap(etc)}
		}
		note()
		return nil
	}
	if _, err := b.setup(t, m, newRepo); err != nil {
		t.Fatal(err)
	}
	note()
	if len(states) < 4 {
		t.Fatalf("only %d durable states were seen", len(states))
	}

	// Each durable state on a fresh box: a run's record, then a setup
	// onto the new repository, which it opens with its own password.
	for i, d := range states {
		t.Run(fmt.Sprintf("state %d", i), func(t *testing.T) {
			b, m := setupBox(t)
			etc := filepath.Dir(b.cfg.EnvFile)
			must(t, os.MkdirAll(etc, 0o755))
			must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
			for name, content := range d.etc {
				must(t, os.WriteFile(filepath.Join(etc, name), []byte(content), 0o600))
			}
			for name, content := range d.state {
				must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, name), []byte(content), 0o644))
			}
			fileNames := "" // the repository the durable credential file names, if any
			if raw, err := os.ReadFile(b.cfg.EnvFile); err == nil {
				v, _ := envfile.Parse(raw)
				fileNames = v["RESTIC_REPOSITORY"]
			}
			// A run writes its record against the repository the file names.
			must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
			m.answers = []string{"AKIDX", "the-secret", "its-own-password"}
			rep, err := b.setup(t, m, newRepo)
			if err != nil {
				t.Fatalf("%v\nsaid:\n%s", err, m.saidAll())
			}
			whole := fileNames == newRepo && strings.TrimSpace(d.state["repository-id"]) == repoID
			switch {
			case fileNames != newRepo && rep.Aside == "":
				t.Fatalf("the record was written against %q and kept for %s\ndurable: %v\nsaid:\n%s", fileNames, newRepo, d, m.saidAll())
			case whole && rep.Aside != "":
				t.Fatalf("a whole state had its record put aside: %s\nsaid:\n%s", rep.Aside, m.saidAll())
			}
			if left, _ := filepath.Glob(b.cfg.EnvFile + ".*"); len(left) != 0 {
				t.Fatalf("a leftover was not swept: %v", left)
			}
		})
	}
}

// Every exit after the password was shown says what became of it, in
// one of three ways, from what setup knows: made, ran, or never used.
// The made and the never-used are certain; "ran" is the rest.
func TestEveryExitAfterTheShowingSaysOneOfThreeFates(t *testing.T) {
	const (
		made = "the password shown above is the repository's: keep it"
		ran  = "keep the password shown above: restic init ran with it"
		dead = "the password shown above was never used: discard it"
	)
	seedRecord := func(b *box) {
		must(b.t, os.MkdirAll(b.cfg.StateDir, 0o755))
		must(b.t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{Apps: map[string]*record.App{"blog": {Class: record.OK}}}))
	}
	existsOwn := func(b *box, m *term) { // the look could not tell, init says exists, the operator's own password opens it
		b.hang = "probe"
		b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
		b.initOut, b.initErr = "", alreadyInit
		m.answers = []string{"AKIDX", "the-secret", "stored", "its-own-password"}
	}
	for _, tc := range []struct {
		name string
		set  func(b *box, m *term)
		want string
	}{
		{"init made it; the credential directory's sync fails", func(b *box, m *term) {
			syncDir = func(path string) error {
				if path == filepath.Dir(b.cfg.EnvFile) {
					return errors.New("EIO on the credential directory")
				}
				return nil
			}
		}, made},
		{"init made it; the id cannot be written", func(b *box, m *term) {
			writeID = func(string, string) error { return errors.New("EIO on the id") }
		}, made},
		{"init made it; the state directory's sync after the id fails", func(b *box, m *term) {
			syncs := 0
			syncDir = func(path string) error {
				if path == b.cfg.StateDir {
					if syncs++; syncs == 1 {
						return errors.New("EIO on the state directory")
					}
				}
				return nil
			}
		}, made},
		{"init made it; the aside's sync fails before the commit", func(b *box, m *term) {
			seedRecord(b)
			syncDir = func(string) error { return errors.New("EIO on the state directory") }
		}, made},
		{"init made it; the commit fails", func(b *box, m *term) {
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, made},
		{"init was left running at its clock", func(b *box, m *term) { b.hang = "init"; clockMet() }, ran},
		{"init refused three times at the opening", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", wrongKey
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, dead},
		{"init failed three times past the opening", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", keyWriteFailed
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, ran},
		{"init failed past the opening once, then was refused at it twice", func(b *box, m *term) {
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", keyWriteFailed
			inits := 0
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					if inits++; inits == 2 {
						b.initErr = wrongKey
					}
				}
			}
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong", "AKIDX", "wrong"}
		}, ran},
		{"init found files of a repository and no config", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", halfThere
		}, dead},
		{"init failed past the opening once, then found files and no config — its own key file, which nothing opens", func(b *box, m *term) {
			b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 10}
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", keyWriteFailed
			inits := 0
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					if inits++; inits == 2 {
						b.initErr = halfThere
					}
				}
			}
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "wrong"}
		}, dead},
		{"it existed, its own password opened it; the credential directory's sync fails", func(b *box, m *term) {
			existsOwn(b, m)
			syncDir = func(path string) error {
				if path == filepath.Dir(b.cfg.EnvFile) {
					return errors.New("EIO on the credential directory")
				}
				return nil
			}
		}, dead},
		{"it existed, its own password opened it; the id cannot be written", func(b *box, m *term) {
			existsOwn(b, m)
			writeID = func(string, string) error { return errors.New("EIO on the id") }
		}, dead},
		{"it existed, its own password opened it; the commit fails", func(b *box, m *term) {
			existsOwn(b, m)
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, dead},
		{"the write of the password before init fails", func(b *box, m *term) {
			writes := 0
			writeEnv = func(path string, pairs []envfile.Pair) error {
				if writes++; writes == 2 {
					return errors.New("ENOSPC on the credential directory")
				}
				return envfile.Write(path, pairs)
			}
		}, dead},
		{"the opening's write fails after init said the repository exists", func(b *box, m *term) {
			existsOwn(b, m)
			writes := 0
			writeEnv = func(path string, pairs []envfile.Pair) error {
				if writes++; writes == 3 {
					return errors.New("ENOSPC on the credential directory")
				}
				return envfile.Write(path, pairs)
			}
		}, dead},
		{"init exited 0 and said nothing of the id", func(b *box, m *term) { b.initOut = "not json at all\n" }, made},
		{"the shown password opened what an earlier init made; the commit fails", func(b *box, m *term) {
			b.hang = "probe"
			b.outcome["init"] = unit.Outcome{Result: "exit-code", ExitStatus: 1}
			b.initOut, b.initErr = "", keyWriteFailed
			inits := 0
			b.before = func(s unit.Spec) {
				if strings.Contains(s.Name, "_init_") {
					if inits++; inits == 1 {
						b.hang = ""
						b.outcome["probe"] = unit.Outcome{Result: "exit-code", ExitStatus: 12}
					}
				}
			}
			m.answers = []string{"AKIDX", "wrong", "stored", "AKIDX", "the-secret"}
			commit = func(string, string) error { return errors.New("EIO on the rename") }
		}, made},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			oldCommit, oldWrite := commit, writeEnv
			t.Cleanup(func() { commit, writeEnv = oldCommit, oldWrite })
			tc.set(b, m)
			_, err := b.setup(t, m, "s3:http://e2e-s3:9000/box")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\nsaid:\n%s", err, tc.want, m.saidAll())
			}
			if strings.Count(err.Error(), "the password shown above") != 1 {
				t.Fatalf("the password's fate is said more than once: %v", err)
			}
		})
	}
}

// runDirs counts the run directories under RunDir.
func runDirs(t *testing.T, b *box) int {
	t.Helper()
	entries, _ := os.ReadDir(b.cfg.RunDir)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}
