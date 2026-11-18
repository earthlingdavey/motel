package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/unit"
)

// The invariants of unit lifecycle across every command, as tables — the
// rule from 4b: a finding that fits one is a row, never a test of its
// own. Each row is one command on one box; what must never happen is
// asserted beside what must.

const testRepo = "s3:http://e2e-s3:9000/box"

// command is one of the commands a lock holder can be, driven against a
// box of its own kind.
type command struct {
	name string
	box  func(t *testing.T) (*box, *term)
	do   func(t *testing.T, b *box, m *term) error
}

// The three that need a credential file, on a box whose repository
// holds snapshots of blog. A restore --to needs a parent that is
// root's own, which the unit lane has only as root: that row skips
// elsewhere, as the restore tests do.
var runDrillRestore = []command{
	{"a run", plainBox, func(_ *testing.T, b *box, _ *term) error { _, err := Run(context.Background(), b.cfg, b); return err }},
	{"a drill", plainBox, func(_ *testing.T, b *box, _ *term) error {
		_, _, err := Drill(context.Background(), b.cfg, b)
		return err
	}},
	{"a restore in place", plainBox, func(_ *testing.T, b *box, _ *term) error {
		_, err := Restore(context.Background(), b.cfg, b, inPlace())
		return err
	}},
	{"a restore --to", plainBox, func(t *testing.T, b *box, _ *term) error {
		_, err := Restore(context.Background(), b.cfg, b, RestoreOptions{App: "blog", To: filepath.Join(toParent(t), "out")})
		return err
	}},
}

var setupCommand = command{"setup", setupBox, func(t *testing.T, b *box, m *term) error { _, err := b.setup(t, m, testRepo); return err }}

func plainBox(t *testing.T) (*box, *term) { return restoreBox(t), nil }

// Nothing starts before setup: with no credential file, a run, a drill
// and a restore each refuse naming the path — and the old path, where a
// file from before this version is still there, saying it is not read —
// start no unit and write no record. The unit file's own condition on
// the file (units_test, cmd/motel-backup) stands in front of this;
// this is what holds when the file goes between that check and the
// run's own.
func TestNothingStartsBeforeSetup(t *testing.T) {
	note := " is from before this version and is not read: run `motel-backup setup --existing <its RESTIC_REPOSITORY>` as root, which asks for its RESTIC_PASSWORD; then remove it"
	for _, cmd := range runDrillRestore {
		for _, old := range []string{"", ", with the old file beside"} {
			t.Run(cmd.name+old, func(t *testing.T) {
				b, m := cmd.box(t)
				must(t, os.Remove(b.cfg.EnvFile))
				b.cfg.OldEnvFile = filepath.Join(filepath.Dir(b.cfg.EnvFile), "motel", "backup.env")
				if old != "" {
					must(t, os.MkdirAll(filepath.Dir(b.cfg.OldEnvFile), 0o755))
					must(t, os.WriteFile(b.cfg.OldEnvFile, []byte("RESTIC_PASSWORD=old\n"), 0o600))
				}
				err := cmd.do(t, b, m)
				if err == nil || !strings.Contains(err.Error(), "backups are not set up: "+b.cfg.EnvFile+" is not there") || strings.Contains(err.Error(), "lstat") {
					t.Fatalf("err = %v", err)
				}
				if strings.Contains(err.Error(), b.cfg.OldEnvFile+note) != (old != "") {
					t.Fatalf("the old path: err = %v", err)
				}
				neverStarted(t, b, m)
			})
		}
	}
}

// Every unit a command starts is bound to the command's own service,
// when it is one — the manager then ends them if the command is killed,
// however it dies [M8] — and to nothing when it is a shell's: from a
// shell there is no service, and the signal handler and the next lock
// holder's sweep do that work. Three are never bound. Setup's init
// unit, left to finish making the repository whatever becomes of
// setup. And the two that remove plaintext, clean and unstage: a
// service that is being stopped has a stop job queued, and the manager
// refuses to start a unit bound to it ("transaction is destructive"
// [M62]), so bound, the copies a stopped run made stayed until the
// next run; they hold no credential and no network, and are over in
// moments.
func TestEveryUnitOfACommandIsBoundToItsService(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand) {
		for _, own := range []string{"motel-backup.service", ""} {
			t.Run(cmd.name+" bound to "+own, func(t *testing.T) {
				b, m := cmd.box(t)
				b.cfg.BindsTo = own
				if err := cmd.do(t, b, m); err != nil {
					t.Fatal(err)
				}
				if len(b.specs) == 0 {
					t.Fatal("no unit ran: the row proves nothing")
				}
				unbound := 0
				for _, s := range b.specs {
					want := own
					if strings.Contains(s.Name, "_init_") || strings.Contains(s.Name, "_clean_") || strings.Contains(s.Name, "_unstage_") {
						want = ""
						unbound++
					}
					if s.BindsTo != want {
						t.Errorf("%s: BindsTo = %q, want %q", s.Name, s.BindsTo, want)
					}
				}
				if unbound == len(b.specs) {
					t.Fatal("no unit of the command is one that is bound: the row proves nothing")
				}
			})
		}
	}
}

// One lock across every command: whoever holds it, whoever comes says
// so, naming the holder, and starts nothing, asks nothing, and writes
// no record. Setup included — an operator at its prompts holds the
// lock for as long as the prompts wait, and a run that comes due says
// so and does nothing.
func TestOneLockAcrossCommands(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand) {
		t.Run(cmd.name, func(t *testing.T) {
			b, m := cmd.box(t)
			must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
			unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
			must(t, err)
			defer unlock()
			err = cmd.do(t, b, m)
			if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "pid ") {
				t.Fatalf("err = %v", err)
			}
			neverStarted(t, b, m)
			if m != nil {
				if _, err := os.Lstat(filepath.Dir(b.cfg.EnvFile)); err == nil {
					t.Fatal("setup made the credential directory while another command held the lock")
				}
			}
		})
	}
}

// neverStarted: no unit, no record, and nobody asked anything.
func neverStarted(t *testing.T, b *box, m *term) {
	t.Helper()
	if len(b.specs) != 0 {
		t.Fatalf("units were started: %s", b.roles())
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "status.json")); err == nil {
		t.Fatal("a record was written")
	}
	if m != nil && len(m.asked) != 0 {
		t.Fatalf("asked %q", m.asked)
	}
}

// What setup refuses, a run refuses, in setup's words and before any
// unit: a missing restic, sqlite3 or moteld (a unit whose command is
// not there ends 203/EXEC, which says nothing of what to install), and
// the account restic runs as — not there, or one someone else can be
// (Copilot on #153: the credential reaches restic's environment on
// every run, not at setup, so the run is where the check has to hold).
// One check, in the one place a run, a restore and a drill share. A run
// records the refusal as the run's own error and a drill as one that
// could not begin, so that status fails with the unit rather than
// reporting the run before as ok for three hours; a restore says it
// at the terminal, as every refusal of a restore is.
func TestARunRefusesWhatSetupRefusesWithSetupsWords(t *testing.T) {
	for _, p := range []struct {
		name string
		set  func(b *box)
		want string
	}{
		{"restic", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/restic" } }, "restic, the backup program motel-backup runs, is not installed at /usr/bin/restic: apt install restic"},
		{"sqlite3", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/sqlite3" } }, "sqlite3 is not installed at /usr/bin/sqlite3: apt install sqlite3"},
		{"moteld", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/moteld" } }, "moteld is not installed at /usr/bin/moteld"},
		{"motel-backup", func(b *box) { b.haveProgram = func(path string) bool { return path != "/usr/bin/motel-backup" } }, "motel-backup is not installed at /usr/bin/motel-backup, where the units run it"},
		{"the account", func(b *box) { b.account = false }, "the motel-backup account is not there: motel-backup setup <repository>, as root, makes it (" + useraddArgv() + ")"},
		// A b2: repository, which setup refuses: one written by hand, or
		// set up before setup refused it. Every unit would run restic's
		// own B2 backend (#177).
		{"a repository other than b2:", func(b *box) {
			writeOrPanic(b.cfg.EnvFile, "RESTIC_REPOSITORY=b2:hs-backups:dev\nRESTIC_PASSWORD=the-secret\nB2_ACCOUNT_ID=K001\nB2_ACCOUNT_KEY=k\n")
		}, "the repository is b2:, which is not supported: as root, motel-backup setup --existing 's3:https://s3.<region>.backblazeb2.com/hs-backups/dev' moves it to B2's S3 endpoint, with an application key; restic's own B2 backend retries a failed upload without end"},
		// A file the manager could not read either: said, in place of
		// every unit failing to start.
		{"a credential file that can be read", func(b *box) {
			if err := os.Remove(b.cfg.EnvFile); err != nil {
				panic(err)
			}
			if err := os.Mkdir(b.cfg.EnvFile, 0o700); err != nil {
				panic(err)
			}
		}, "the credential file " + "%ENVFILE%" + " could not be read: it is not a regular file (d---------)"},
		// A FIFO in its place, which a read would wait on for ever,
		// holding the run lock.
		{"a credential file that is not a FIFO", func(b *box) {
			if err := os.Remove(b.cfg.EnvFile); err != nil {
				panic(err)
			}
			if err := syscall.Mkfifo(b.cfg.EnvFile, 0o600); err != nil {
				panic(err)
			}
		}, "the credential file " + "%ENVFILE%" + " could not be read: it is not a regular file (p---------)"},
		{"an account motel did not make", func(b *box) { b.comment = "" }, "the motel-backup account on this box was not made by motel"},
		// The manager binds what it sees: where it does not see this
		// command's mounts it would show every unit a bare mount point
		// in an app's data's place [M54], so the command refuses before
		// it shows any unit anything, and before any upload [M72].
		{"the manager's sight of its mounts", func(b *box) { b.unseen = true }, "runs in a mount namespace of its own: the manager does not see the mounts it makes"},
		{"an answer from the manager about its mounts", func(b *box) { b.seesErr = errors.New("no reply") }, "whether the manager sees the mounts this command makes could not be asked: no reply"},
		// The data user's lookup is every command's to fail on, in the
		// lookup's own words: kept for whoever asked first, it was never
		// said where no app was reached, and where one was it came out
		// as "does not belong to the motel user" (Copilot on #155).
		{"the motel account", func(b *box) {
			dataOwner = func(context.Context) (int, int, error) { return 0, 0, errors.New("the motel account is not there") }
		}, "the motel account is not there"},
		{"an answer about the motel account", func(b *box) {
			dataOwner = func(context.Context) (int, int, error) {
				return 0, 0, errors.New("/usr/bin/getent passwd motel did not answer within 10s")
			}
		}, "/usr/bin/getent passwd motel did not answer within 10s"},
	} {
		for _, cmd := range runDrillRestore {
			t.Run(cmd.name+" without "+p.name, func(t *testing.T) {
				b, m := cmd.box(t)
				p.set(b)
				want := strings.ReplaceAll(p.want, "%ENVFILE%", b.cfg.EnvFile)
				err := cmd.do(t, b, m)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v\nwant %s", err, want)
				}
				if len(b.specs) != 0 {
					t.Fatalf("units were started: %s", b.roles())
				}
				st, rerr := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
				// The record holds the error as record.Text cuts it, and
				// nothing else: exactly, so a cut that took the remedy
				// would show.
				recorded := record.Text(err.Error())
				switch cmd.name {
				case "a run":
					if rerr != nil || st.Error != recorded {
						t.Fatalf("the run's record: %+v, %v\nwant %q", st, rerr, recorded)
					}
				case "a drill":
					if rerr != nil || st.LastDrill == nil || st.LastDrill.Detail != recorded {
						t.Fatalf("the drill's record: %+v, %v", st, rerr)
					}
				default:
					if _, err := os.Lstat(filepath.Join(b.cfg.StateDir, "status.json")); err == nil {
						t.Fatal("a restore wrote a record")
					}
				}
			})
		}
	}
}

// A drill that could not begin records why — a program not there, a
// unit that would not stop — but an interrupt is no verdict: stopped
// while it waits, the drill leaves the last drill's time and detail as
// they were, as it leaves the app an interrupt lands on. And what open
// had to say — a record it could not read and replaced — reaches the
// record it writes.
func TestADrillStoppedWhileItWaitsIsNoVerdict(t *testing.T) {
	b := restoreBox(t)
	last := &record.Drill{Time: time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC), Detail: ""}
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{LastDrill: last, Apps: map[string]*record.App{}}))
	must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "init-unit"), []byte("motel_backup_init_0123456789ab.service\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Drill(ctx, b.cfg, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	st, err := record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !st.LastDrill.Time.Equal(last.Time) || st.LastDrill.Detail != "" {
		t.Fatalf("the last drill's verdict was replaced by an interrupt: %+v", st.LastDrill)
	}

	// Stopped during the plan unit, the same: the plan that could not be
	// read is the interrupt's doing, and no verdict either.
	b = restoreBox(t)
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, record.Write(filepath.Join(b.cfg.StateDir, "status.json"), &record.Status{LastDrill: last, Apps: map[string]*record.App{}}))
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	b.before = func(s unit.Spec) {
		if strings.Contains(s.Name, "_plan_") {
			cancel()
		}
	}
	b.err["plan"] = context.Canceled
	if _, _, err := Drill(ctx, b.cfg, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !b.started("plan") {
		t.Fatal("the plan unit never started: the row proves nothing")
	}
	st, err = record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !st.LastDrill.Time.Equal(last.Time) || st.LastDrill.Detail != "" {
		t.Fatalf("the last drill's verdict was replaced by an interrupt during the plan: %+v", st.LastDrill)
	}

	// A record that could not be read is said by the drill that could
	// not begin, beside why it could not.
	b = restoreBox(t)
	b.haveProgram = func(path string) bool { return path != "/usr/bin/restic" }
	must(t, os.MkdirAll(b.cfg.StateDir, 0o755))
	must(t, os.WriteFile(filepath.Join(b.cfg.StateDir, "status.json"), []byte("not json"), 0o644))
	if _, _, err := Drill(context.Background(), b.cfg, b); err == nil {
		t.Fatal("a drill without restic began")
	}
	st, err = record.Read(filepath.Join(b.cfg.StateDir, "status.json"))
	must(t, err)
	if st.LastDrill == nil || !strings.Contains(st.LastDrill.Detail, "restic, the backup program motel-backup runs, is not installed") || !strings.Contains(st.Warning, "could not be read and was replaced") {
		t.Fatalf("the record: last drill %+v, warning %q", st.LastDrill, st.Warning)
	}
}

// The account restic runs as is one useradd line in two places: setup's,
// and the package's postinstall, which makes it before setup ever runs
// (the owner, 2026-09-26). The script holds setup's argv verbatim, so
// that the two cannot drift.
func TestPostinstallMakesTheAccountAsSetupDoes(t *testing.T) {
	line := "useradd " + strings.Join(useradd[1:], " ")
	// And the e2e fixture that makes it by hand, and the README that
	// says how: every copy of the line, or one drifts.
	for _, f := range []string{"../../packaging/postinstall.sh", "../../e2e/backup/lib.sh", "../README.md", "../../.github/workflows/release.yml"} {
		raw, err := os.ReadFile(f)
		must(t, err)
		if !strings.Contains(string(raw), line) {
			t.Fatalf("%s does not make the account as setup does:\nwant a line holding %q", f, line)
		}
	}
}

// An existing motel-backup account is not trusted by its name:
// motel uses only an account it made, which carries its mark, and
// refuses any other — made by hand, by another package, by an earlier
// version of this branch — before any prompt, changing nothing, and
// naming the remedy (the owner, 2026-09-27: a clear refusal, where the
// account was judged by its shell, home, password, uid and groups).
func TestAnAccountMotelDidNotMakeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, comment string
		refused       bool
	}{
		{"as motel makes it", accountMark, false},
		{"no comment: plain useradd, or this branch before the mark", "", true},
		{"another package's", "Debian backup account", true},
		// The remedy for an account made by hand for motel is the mark,
		// which keeps the credential file: said beside userdel.
		{"a comment that holds the mark", "not " + accountMark, true},
		{"the mark with a space after", accountMark + " ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			b.comment = tc.comment
			rep, err := b.setup(t, m, testRepo)
			if !tc.refused {
				if err != nil || rep.Account != "present" {
					t.Fatalf("motel's account refused: %+v, %v", rep, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("not refused: %+v", rep)
			}
			for _, w := range []string{"the motel-backup account on this box was not made by motel", "as root, remove it (userdel motel-backup) and run motel-backup setup <repository>", useraddArgv(), "if it is one you made for motel, mark it: usermod --comment made-by-motel motel-backup"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v\nwant it to say %q", err, w)
				}
			}
			if b.accountsMade != 0 || b.comment != tc.comment {
				t.Fatalf("the account was changed: made %d, comment %q", b.accountsMade, b.comment)
			}
			neverStarted(t, b, m)
			if _, err := os.Lstat(filepath.Dir(b.cfg.EnvFile)); err == nil {
				t.Fatal("the credential directory was made before the account was found wrong")
			}
		})
	}
}

// Ours is local as well as marked: the account in /etc/passwd, which
// only root writes and where useradd puts it, and the one the system
// resolves by that name. A directory's account of that name carries
// whatever comment its administrator gives it, the mark among them,
// and is refused (the owner's review of #155).
func TestOursIsTheLocalAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(b *box)
		want string
	}{
		{"a directory's, with the mark, none in /etc/passwd", func(b *box) { b.notLocal = true }, "is not in /etc/passwd"},
		{"a directory's, with the mark, shadowing an unmarked local one", func(b *box) { b.localComment = "" }, "is not the one in /etc/passwd"},
		{"a directory's, with the mark, at another uid than the local one", func(b *box) {
			old := localAccount
			localAccount = func(context.Context, string) (passwd, error) {
				return passwd{name: backupUser, comment: accountMark, uid: 4242, gid: 995, exists: true}, nil
			}
			b.t.Cleanup(func() { localAccount = old })
		}, "is not the one in /etc/passwd"},
		// The same uid and the same mark, and a login shell: the box
		// resolves the directory's line, not /etc/passwd's (Copilot on
		// #155). The whole line is held to the local one.
		{"a directory's, with the mark and the uid, and a login shell", func(b *box) {
			b.localLine = "motel-backup:x:995:995:made-by-motel:/nonexistent:/usr/sbin/nologin"
			b.nssLine = "motel-backup:*:995:995:made-by-motel:/home/motel-backup:/bin/bash"
		}, "is not the one in /etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := setupBox(t)
			tc.set(b)
			_, err := b.setup(t, m, testRepo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			neverStarted(t, b, m)
		})
	}
}

// Sweep is what the package's preremove asks at a remove, while the
// program is still there: what a killed command left — the units it
// recorded, stopped by those names, and what it left mounted under the
// run directory, made private and taken away — swept under the run
// lock, as the next run would have, since there will be none. The
// engine's own sweep, in Go, in the place of a shell copy of it (the
// owner, 2026-09-27); and no more than that sweep: no state
// directories made on a box that never set backups up, no wait for an
// init a setup left running, which would leave the rest unswept.
func TestSweepTakesAwayWhatAKilledCommandLeft(t *testing.T) {
	b := restoreBox(t)
	must(t, os.MkdirAll(filepath.Join(b.cfg.RunDir, "0123456789ab"), 0o700))
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "units"), []byte(
		"motel_backup_upload_blog_0123456789ab.service\nsmoke-bystander.service\nmotel_backup_bystander.service\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "init-unit"), []byte("motel_backup_init_0123456789ab.service\n"), 0o600))
	top := filepath.Join(b.cfg.RunDir, "0123456789ab", "mount-1")
	nested := filepath.Join(top, "uploads", "disk")
	b.leftMounts = []string{nested, top}
	if err := Sweep(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	// By the names the engine writes, and no other: the file is root's
	// own, and still a name it did not write is not the manager's to be
	// asked to stop.
	if len(b.stopped) != 1 || b.stopped[0] != "motel_backup_upload_blog_0123456789ab.service" {
		t.Errorf("stopped %q", b.stopped)
	}
	// The engine's own mount point alone, made private with all beneath
	// it and detached with all beneath it: the nested one's path runs
	// through an app's directory, which the app can re-aim at another's
	// with a link between the look and the call (the owner's review).
	// (That it is made private first is unmountDetach's own, held by
	// the integration pin on a shared mount; this lane stands the mount
	// calls in, and asks what they were asked of.)
	if !slices.Equal(b.unmountedAt, []string{top}) || slices.Contains(b.madePrivate, nested) {
		t.Errorf("made private %q, unmounted %q; want %q alone, and nothing of %q", b.madePrivate, b.unmountedAt, top, nested)
	}
	if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "units")); err == nil {
		t.Error("the list of units is still there")
	}
	if len(b.specs) != 0 || len(b.waited) != 0 {
		t.Errorf("units were started %s, or waited for %q", b.roles(), b.waited)
	}
	if _, err := os.Lstat(b.cfg.StateDir); err == nil {
		t.Error("a sweep made the state directory")
	}

	// With the lock held — a command from a shell — nothing is touched,
	// and it is said by the lock's own words.
	b = restoreBox(t)
	must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
	unlock, err := lock(filepath.Join(b.cfg.RunDir, "lock"))
	must(t, err)
	defer unlock()
	must(t, os.WriteFile(filepath.Join(b.cfg.RunDir, "units"), []byte("motel_backup_upload_blog_0123456789ab.service\n"), 0o600))
	if err := Sweep(context.Background(), b.cfg, b); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "pid ") {
		t.Fatalf("err = %v", err)
	}
	if len(b.stopped) != 0 {
		t.Errorf("stopped under a held lock: %q", b.stopped)
	}

	// No run directory: nothing was ever left, and nothing is made.
	b = restoreBox(t)
	if err := Sweep(context.Background(), b.cfg, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(b.cfg.RunDir); err == nil {
		t.Error("a sweep made the run directory")
	}
}

// Something mounted on the run directory itself — nothing of the
// engine's is — and everything a command does there, the lock, the
// list of units, a run's files, would be done through it: a run, a
// restore, a drill, setup and the sweep refuse before any of it
// (Copilot and the owner's review on #155).
func TestAMountOnTheRunDirectoryIsRefused(t *testing.T) {
	for _, cmd := range append(runDrillRestore, setupCommand, command{"a sweep", plainBox, func(_ *testing.T, b *box, _ *term) error {
		return Sweep(context.Background(), b.cfg, b)
	}}) {
		t.Run(cmd.name, func(t *testing.T) {
			b, m := cmd.box(t)
			must(t, os.MkdirAll(b.cfg.RunDir, 0o700))
			b.runDirMounted = true
			err := cmd.do(t, b, m)
			if err == nil || !strings.Contains(err.Error(), b.cfg.RunDir+" is itself a mount point") {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(b.cfg.RunDir, "lock")); err == nil {
				t.Fatal("the lock was taken through the mount")
			}
			if len(b.specs) != 0 || len(b.stopped) != 0 {
				t.Fatalf("units were started %s or stopped %q", b.roles(), b.stopped)
			}
		})
	}
}

// postremove stops the units a killed command left by the names it
// recorded — at a purge, with the program gone and no sweep to ask
// (Copilot on #155: a shell command killed between a remove and a
// purge left its upload unit running, with the credential) — and only
// names the engine writes: its pattern and the engine's own grammar
// agree on every name.
func TestPostremoveStopsOnlyNamesTheEngineWrites(t *testing.T) {
	raw, err := os.ReadFile("../../packaging/postremove.sh")
	must(t, err)
	m := regexp.MustCompile(`grep -Eq '(\^motel_backup_[^']+)'`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("postremove holds no pattern of the engine's unit names")
	}
	shell := regexp.MustCompilePOSIX(m[1])
	for _, name := range []string{
		"motel_backup_upload_demo_0123456789ab.service",
		"motel_backup_plan_0123456789ab.service",
		"motel_backup_listing_0123456789ab.service",
		"motel_backup_upload_my-app2_0123456789ab.service",
		"motel_backup_bystander.service",
		"motel_backup_upload_demo_0123456789a.service",
		"motel_backup_upload_demo_0123456789AB.service",
		"motel_backup_upload_Demo_0123456789ab.service",
		"motel_backup__0123456789ab.service",
		"smoke-bystander.service",
		"motel_backup_upload_demo_0123456789ab.timer",
		"motel_backup_upload_demo_0123456789ab.service.d",
	} {
		_, _, engine := ParseUnitName(name)
		if got := shell.MatchString(name); got != engine {
			t.Errorf("%s: postremove says %v, the engine %v", name, got, engine)
		}
	}
}

// writeOrPanic writes a file a table row's setter makes, which runs in
// a subtest with no *testing.T of its own to fail.
func writeOrPanic(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		panic(err)
	}
}
