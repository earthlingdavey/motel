// Command motel-backup backs up what motel's apps declare.
//
//	motel-backup setup [--existing] <repository>
//	                        the account, the directories, the credential file, the repository; root, at a terminal.
//	                        --existing: the repository is there already (a rebuilt box), so no password is made
//	motel-backup run     one backup run; root; what the timer starts
//	motel-backup restore <app> [--snapshot <id>] [--to <dir>] [--no-pre-backup] [--yes]
//	                        one snapshot of one app, into place or into a new directory; root
//	motel-backup drill   fetch and check the newest snapshot of every app, installing nothing,
//	                        then check the repository itself and one fifty-second of its
//	                        data; root
//	motel-backup status [--json]
//	                        whether each app's backup is fresh and a restore of it proven; anyone.
//	                        Exits 0 healthy, 1 not, 3 when it could not tell; --json: the same, as JSON
//	motel-backup validate <Caddyfile>
//	                        whether a run could plan from that Caddyfile, before it goes live; anyone
//	motel-backup account whether the account restic runs as is one setup and a run accept; anyone;
//	                        what the package's postinstall asks
//	motel-backup sweep   take away what a killed command left — its units, its mounts under
//	                        /run/motel-backup — as the next command would; root; what the
//	                        package's preremove asks at a remove
//
// and subcommands that are only ever the command of a unit one of those
// starts, each with a fixed view and no arguments — nothing an operator
// or an app wrote reaches a command line:
//
//	motel-backup plan    print the plan read from /etc/motel/Caddyfile
//	motel-backup dump    copy the databases /plan.json declares from /shared to /staging
//	motel-backup clean   empty /staging
//	motel-backup check   check the snapshot fetched into /restore
//	motel-backup install check it, and install it into /target if all of it is sound
//	motel-backup extract check it, and install what is sound into /target
//
// (Two strings of a declaration's do reach a command line: the directory
// part of a nested path, to `restic ls` (engine.verify), and a declared
// sqlite path, in the SQL on `sqlite3`'s argv (dump.execute, RestoreOver).
// validate's argument is a path this process opens; it starts no unit.)
//
// It does not link Caddy, and motel does not run it: the two share a
// declaration format (liveswap/backupdecl) and nothing else.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/smallhoursorg/motel/backups/dump"
	"github.com/smallhoursorg/motel/backups/engine"
	"github.com/smallhoursorg/motel/backups/envfile"
	"github.com/smallhoursorg/motel/backups/plan"
	"github.com/smallhoursorg/motel/backups/record"
	"github.com/smallhoursorg/motel/backups/restore"
	"github.com/smallhoursorg/motel/backups/status"
	"github.com/smallhoursorg/motel/backups/unit"
	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

const caddyfile = "/etc/motel/Caddyfile"

const usage = `usage: motel-backup setup [--existing] <repository>
       motel-backup run
       motel-backup restore <app> [--snapshot <id>] [--to <dir>] [--no-pre-backup] [--yes]
       motel-backup drill
       motel-backup status [--json]
       motel-backup validate <Caddyfile>
       motel-backup account
       motel-backup sweep`

// arguments says whether a command takes those: restore reads its
// own, validate takes one file, setup one repository and its flag
// (setupArgs), status its one flag (statusArgs), and nothing else takes any — least of all the commands
// of units.
func arguments(name string, args []string) bool {
	switch name {
	case "restore":
		return true
	case "validate":
		return len(args) == 1
	case "setup":
		_, _, err := setupArgs(args)
		return err == nil
	case "status":
		_, ok := statusArgs(args)
		return ok
	}
	return len(args) == 0
}

// statusArgs reads status's: nothing, or --json (or -json) alone.
func statusArgs(args []string) (asJSON, ok bool) {
	switch {
	case len(args) == 0:
		return false, true
	case len(args) == 1 && (args[0] == "--json" || args[0] == "-json"):
		return true, true
	}
	return false, false
}

// setupArgs reads setup's: one repository, and --existing (or
// -existing) once, before or after it. Anything else — a second
// repository, the flag twice, any other word that begins with a dash,
// the terminator `--` among them — is the usage text. One pass, no
// flag package: there is one flag, and a URL never begins with a dash.
// An empty word (a shell variable that is not set) is the one
// repository, which the engine refuses as empty, naming that.
func setupArgs(args []string) (repository string, existing bool, err error) {
	given := false
	for _, a := range args {
		switch {
		case (a == "--existing" || a == "-existing") && !existing:
			existing = true
		case strings.HasPrefix(a, "-") || given:
			return "", false, errors.New(usage)
		default:
			repository, given = a, true
		}
	}
	if !given {
		return "", false, errors.New(usage)
	}
	return repository, existing, nil
}

func main() {
	if len(os.Args) < 2 || !arguments(os.Args[1], os.Args[2:]) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := command(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "motel-backup:", err)
		os.Exit(exitStatus(err))
	}
}

// exitStatus is what the command leaves with.
func exitStatus(err error) int {
	switch {
	case errors.Is(err, errOtherVersion):
		return engine.OtherVersionStatus
	case errors.As(err, new(couldNotTell)):
		return 3
	}
	return 1
}

// errOtherVersion is a helper that is not the version of the command
// that started it.
var errOtherVersion = errors.New("this helper is not the version of the command that started it (the package was upgraded while that command was under way), and does nothing")

// forThisCommand is whether a helper may work for the command that
// started it, which told it which program it is (engine.RunIdentityEnv):
// one that is another program does nothing, and one that cannot read
// which it is cannot say it is the same. Told nothing — started by
// hand, or by a version from before there was anything to tell — it
// works. clean works whatever it is told: it empties one directory of
// plaintext copies, which is every version's to want done at once.
func forThisCommand(name, told string, own func() (string, error)) error {
	switch name {
	case "plan", "dump", "check", "install", "extract":
	default:
		return nil
	}
	if told == "" {
		return nil
	}
	is, err := own()
	if err != nil {
		return fmt.Errorf("%w: which version this is could not be read: %w", errOtherVersion, err)
	}
	if is != told {
		return errOtherVersion
	}
	return nil
}

// couldNotTell is status not having been able to look — no setup, a
// record it cannot read — which is not the same news as backups that
// are unhealthy, and does not leave with the same status: 3, not 1.
type couldNotTell struct{ error }

func (e couldNotTell) Unwrap() error { return e.error }

func command(name string, args []string) error {
	// SIGINT, SIGTERM and SIGHUP cancel the context; a run then stops the unit
	// it is waiting on, by name, and confirms it gone before returning.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	if err := forThisCommand(name, os.Getenv(engine.RunIdentityEnv), engine.WhichProgram); err != nil {
		return err
	}
	switch name {
	case "setup":
		// main's check read them already; a caller that did not check
		// gets the usage text back.
		repository, existing, err := setupArgs(args)
		if err != nil {
			return err
		}
		return setup(ctx, repository, existing)
	case "run":
		return run(ctx)
	case "restore":
		return restoreApp(ctx, args)
	case "drill":
		return drill(ctx)
	case "status":
		asJSON, _ := statusArgs(args)
		return showStatus(ctx, asJSON)
	case "validate":
		return validate(ctx, args[0])
	case "account":
		// What setup and every run ask of the account restic runs as,
		// in their words; the package's postinstall asks it, and an
		// administrator after mending the account.
		if err := engine.AccountReady(ctx); err != nil {
			return err
		}
		fmt.Println("the motel-backup account is one setup and a run accept")
		return nil
	case "sweep":
		// The package's preremove, at a remove, while this program is
		// still there: what a killed command left, swept as the next
		// command would have, since there will be none.
		r, err := runner(ctx, "sweep")
		if err != nil {
			return err
		}
		defer r.Close()
		return engine.Sweep(ctx, config(), r)
	case "check":
		return settle(ctx, restore.CheckOnly)
	case "install":
		return settle(ctx, restore.AllOrNothing)
	case "extract":
		return settle(ctx, restore.WhatIsSound)
	case "plan":
		return printPlan(ctx)
	case "dump":
		return dumpDatabases(ctx)
	case "clean":
		return clean("/staging")
	}
	return fmt.Errorf("unknown command %q", name)
}

func config() engine.Config {
	return engine.Config{
		ConfigDir: "/etc/motel", EnvFile: "/etc/motel-backup/repository.env", OldEnvFile: "/etc/motel/backup.env",
		StateDir: "/var/lib/motel-backup", RunDir: "/run/motel-backup",
		Self: "/usr/bin/motel-backup", Restic: "/usr/bin/restic",
		BindsTo: ownService(),
	}
}

// runner connects to the system manager, which takes root. The refusal
// says to repeat the command as root and renders none of it: an
// argument printed back would have to be quoted for a shell, and one
// that was refused for holding a credential must not be echoed.
func runner(ctx context.Context, what string) (*unit.Runner, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("a %s starts system units, which needs root: run the same command again as root", what)
	}
	return unit.NewSystemRunner(ctx)
}

// setup needs root and a terminal, and says so before anything else:
// the secrets are typed there, with echo off, never given on a command
// line. The engine checks what else can be known to fail before it
// asks for one.
func setup(ctx context.Context, repository string, existing bool) error {
	cfg := config()
	r, err := runner(ctx, "setup")
	if err != nil {
		return err
	}
	defer r.Close()
	t, err := openTTY(cfg.EnvFile)
	if err != nil {
		return err
	}
	defer t.Close()
	rep, err := engine.Setup(ctx, cfg, r, engine.SetupOptions{Repository: repository, Existing: existing, Terminal: t})
	if err != nil {
		return err
	}
	t.Say(fmt.Sprintf("credentials: %s (root, 0600)", cfg.EnvFile))
	// What runs next is what is so on this box: the package's timer,
	// or nothing — the raw-binary tarball, an administrator's disable.
	active, enabled, err := r.Scheduled(ctx, "motel-backup.timer")
	t.Say(closing(len(rep.Apps), active, enabled, err))
	return nil
}

// closing is setup's last word, which never fails a setup that has
// succeeded: with the manager not answering, that it could not tell,
// and the line for a box with no timer.
func closing(apps int, active, enabled bool, err error) string {
	if err != nil {
		// No advice from not knowing: a scheduler added beside one that
		// is there meets it on the run lock every hour.
		return "next: could not tell whether motel-backup.timer is scheduled (" + err.Error() + "): systemctl list-timers motel-backup.timer says; to run a backup now, systemctl start motel-backup.service"
	}
	return nextLine(apps, active, enabled)
}

// nextLine is what happens now, from whether the package's hourly
// timer is active on this box.
func nextLine(apps int, active, enabled bool) string {
	const (
		notEnabled = "motel-backup.timer is not enabled: after a reboot nothing runs it (systemctl enable motel-backup.timer)"
		nothing    = "nothing runs it on a schedule: systemctl enable --now motel-backup.timer, or run motel-backup run hourly from a timer or cron entry of your own"
		declare    = "next: declare a backup in an app's Caddyfile block (liveswap/README.md); "
	)
	switch {
	case apps > 0 && active && enabled:
		return "next: the first backup runs within the hour and ten minutes (systemctl list-timers motel-backup.timer); to run one now, systemctl start motel-backup.service; then motel-backup status"
	case apps > 0 && active:
		return "next: the first backup runs within the hour and ten minutes, but " + notEnabled + "; then motel-backup status"
	case apps > 0:
		return "next: " + nothing + "; then motel-backup status"
	case active && enabled:
		return declare + "the hourly timer backs it up from then on, or systemctl start motel-backup.service runs one now"
	case active:
		return declare + "the hourly timer backs it up from then on, but " + notEnabled
	}
	return declare + nothing
}

func run(ctx context.Context) error {
	r, err := runner(ctx, "run")
	if err != nil {
		return err
	}
	defer r.Close()
	st, err := engine.Run(ctx, config(), r)
	if st != nil {
		report(st)
	}
	if err != nil {
		return err
	}
	for _, app := range st.Apps {
		if app == nil || (app.Class != record.OK && app.Class != record.Pending) {
			return errors.New("not every app was backed up")
		}
	}
	return nil
}

// say prints a line made of a record's words: a record is root's, and
// still nothing of it reaches a terminal unlooked at.
func say(line string) { fmt.Println(record.Clean(line)) }

// report is one line an app, and never says "backed up" of anything
// that was not: the class, and for an ok run the snapshot and what was
// found in it.
func report(st *record.Status) {
	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	if st.Warning != "" {
		say("warning: " + st.Warning)
	}
	if len(names) == 0 && st.Error == "" {
		fmt.Println("no app declares a backup; nothing was backed up")
	}
	for _, n := range names {
		app := st.Apps[n]
		if app == nil {
			continue
		}
		line := fmt.Sprintf("%s: %s", n, app.Class)
		if app.Class == record.NotRun && app.LastOK != nil {
			line += fmt.Sprintf(" (last ok %s, snapshot %s)", record.When(app.LastOK.Time), record.Short(app.LastOK.ID))
		}
		if app.Class == record.OK {
			var found []string
			for _, it := range app.Items {
				found = append(found, it.Kind+" "+it.Path)
			}
			line += fmt.Sprintf(": snapshot %s holds %s", record.Short(app.Snapshot.ID), strings.Join(found, ", "))
		} else if app.Detail != "" {
			line += ": " + app.Detail
		}
		say(line)
		// A run that drilled says what the drill found; one that did not
		// says nothing of it.
		if app.RestoreDrill != nil && !app.RestoreDrill.Time.Before(st.Started) {
			say(fmt.Sprintf("%s: restore not proven: %s", n, app.RestoreDrill.Detail))
		} else if app.RestoreProven != nil && !app.RestoreProven.Time.Before(st.Started) {
			say(fmt.Sprintf("%s: restore proven: snapshot %s was fetched and checked whole", n, record.Short(app.RestoreProven.Snapshot.ID)))
		}
	}
}

// showStatus needs no root: the record is for everyone to read, and
// the manager tells anyone what is running. asJSON changes what is on
// stdout and nothing else: the same Result, as JSON, and the same exit
// status.
func showStatus(ctx context.Context, asJSON bool) error {
	cfg := config()
	now := time.Now()
	// A stdout that takes none of it (a full disk; a closed pipe where
	// SIGPIPE is ignored, which otherwise ends the command) is said, on
	// stderr: could not tell is still 3, and a report is not 0 that its
	// reader never got, but 1.
	cannot := func(warnings []string, err error) error {
		if werr := printStatus(os.Stdout, status.CouldNotTell(now, warnings, err.Error()), asJSON); werr != nil {
			err = fmt.Errorf("%w; and it could not be printed: %w", err, werr)
		}
		return couldNotTell{err}
	}
	// Only a file that is not there says so. Any other answer — a
	// directory this user may not search — is not "not set up".
	env, err := os.Lstat(cfg.EnvFile)
	if errors.Is(err, fs.ErrNotExist) {
		return cannot(nil, fmt.Errorf("backups are not set up: %s is not there%s", cfg.EnvFile, engine.OldEnvFileNote(cfg)))
	} else if err != nil {
		return cannot(nil, fmt.Errorf("whether backups are set up cannot be told from here: %w", err))
	}
	// As root, the file itself, before anything else can end the look:
	// where a line of it is not read as it was written, the manager's
	// rules being what they are. Said, and nothing more: what the run
	// makes of the file the run says.
	var warnings []string
	if os.Geteuid() == 0 {
		raw, err := os.ReadFile(cfg.EnvFile)
		warnings = envWarnings(cfg.EnvFile, raw, err)
	}
	st, err := record.Read(filepath.Join(cfg.StateDir, "status.json"))
	if err != nil {
		return cannot(warnings, fmt.Errorf("the record of the last run could not be read: %w", err))
	}
	in := status.Input{Record: st, Now: now, SetUp: env.ModTime(), Warnings: warnings}
	var active []unit.Active
	active, in.RunningErr = unit.ListActive(ctx, engine.UnitPattern)
	for _, a := range active {
		if role, app, ok := engine.ParseUnitName(a.Name); ok {
			in.Running = append(in.Running, status.Running{Role: role, App: app, State: a.State, Since: a.Since})
		} else {
			in.Unrecognised = append(in.Unrecognised, a.Name)
		}
	}
	r := status.Report(in)
	if err := printStatus(os.Stdout, r, asJSON); err != nil {
		return fmt.Errorf("the report could not be printed: %w", err)
	}
	if !r.Healthy {
		return errors.New("not every app has a fresh backup and a restore proven lately")
	}
	return nil
}

// envWarnings is what status, as root, says of the credential file at
// path, whose bytes are raw or which could not be read (err): never a
// value of it, only where a line is not what the manager reads, what is
// not set, and what of the repository the box cannot use.
func envWarnings(path string, raw []byte, err error) []string {
	if err != nil {
		// There (Lstat said so) and not readable — a directory in its
		// place, a file root itself cannot open: nothing the manager
		// could give a unit either.
		return []string{fmt.Sprintf("%s: could not be read: %v", path, err)}
	}
	var out []string
	v, findings := envfile.Parse(raw)
	for _, l := range envfile.Lint(v, findings) {
		out = append(out, fmt.Sprintf("%s: %s", path, l))
	}
	if repo, ok := v["RESTIC_REPOSITORY"]; ok && repo != "" {
		if err := engine.RepositoryUsable(repo, path); err != nil {
			out = append(out, fmt.Sprintf("%s: RESTIC_REPOSITORY: %v", path, err))
		}
		if a := engine.RegionAdvice(repo, v["AWS_DEFAULT_REGION"]); a != "" {
			out = append(out, fmt.Sprintf("%s: %s", path, a))
		}
	}
	return out
}

// printStatus writes r as status prints it: its lines, or, asJSON, the
// whole of it as one JSON object (backups/README.md "Status").
func printStatus(w io.Writer, r status.Result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	for _, l := range r.Text() {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}

// validate says whether a run could turn this Caddyfile into a plan —
// for whoever is about to make it live, before they do. It starts no
// unit, takes no lock and needs no root; the run's own plan step does
// the same to the live file, as another user, inside a view that holds
// the config directory and nothing else. What that view and that user
// would make of an import it cannot say from here, where the whole
// filesystem is in view: that is `motel validate`'s, which has the
// adapter's own word on where each backup block comes from.
func validate(ctx context.Context, file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil { //nolint:gosec // the Caddyfile the command was given, checked to be a file
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", abs)
	}
	ins, err := plan.Inspect(ctx, abs, config().ConfigDir)
	if err != nil {
		// It quotes the adapter, which quotes the Caddyfile: text fit for
		// a terminal only once it holds no control character.
		return errors.New(record.Clean(err.Error()))
	}
	names := ins.Plan.Names()
	if len(names) == 0 {
		fmt.Println("no app declares a backup: a run would back nothing up")
	} else {
		fmt.Printf("a run would back up %s, under %s\n", record.Clean(strings.Join(names, ", ")), record.Text(ins.Plan.Root))
	}
	if len(ins.UndeclaredByEnv) > 0 {
		fmt.Printf("an app named through the environment variable(s) %s declares no backup: nothing of it would be backed up\n", record.Clean(strings.Join(ins.UndeclaredByEnv, ", ")))
	}
	for _, n := range ins.Undeclared {
		fmt.Printf("%s declares no backup: nothing of it would be backed up\n", record.Text(n))
	}
	return nil
}

func restoreApp(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	o := engine.RestoreOptions{App: args[0]}
	var yes bool
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.Snapshot, "snapshot", "", "")
	fs.StringVar(&o.To, "to", "", "")
	fs.BoolVar(&o.NoPreBackup, "no-pre-backup", false, "")
	fs.BoolVar(&yes, "yes", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New(usage)
	}
	// A flag given with nothing in it — --to "$DIR", and DIR unset — is
	// not the same restore without the flag: that one overwrites.
	var empty string
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" {
			empty = f.Name
		}
	})
	if empty != "" {
		return fmt.Errorf("--%s was given with nothing in it", empty)
	}
	if !yes {
		o.Confirm = confirm(ctx)
	}
	r, err := runner(ctx, "restore")
	if err != nil {
		return err
	}
	defer r.Close()
	rep, err := engine.Restore(ctx, config(), r, o)
	if rep != nil {
		reportRestore(rep)
	}
	return err
}

// answerWithin is how long a restore waits to be answered. It holds the
// run lock while it waits, and a backup run that comes due then does
// nothing: an abandoned question must not be what ends a box's backups.
const answerWithin = 10 * time.Minute

// confirm asks on standard input, and takes the app's own name, typed,
// and nothing else for a yes: not "y", which a hand types on its own.
// With nobody there to answer it says how to go without being asked,
// and that is a no; so is an interrupt, and so is silence.
func confirm(ctx context.Context) func(engine.RestoreAsk) bool {
	return func(a engine.RestoreAsk) bool {
		fmt.Printf("%s: restore snapshot %s, made %s, into %s?\n", a.App, record.Short(a.Snapshot.ID), record.When(a.Snapshot.Time), a.Into)
		if a.LastOK != nil {
			fmt.Println(notLastOK(a.App, a.LastOK))
		}
		if a.NotKnown != "" {
			fmt.Println(notKnown(a.App, a.NotKnown))
		}
		if a.PreBackup {
			fmt.Println("What is there is backed up first. Its databases are then replaced and its files overwritten; what the snapshot does not hold is left.")
		} else {
			fmt.Println("Its databases are replaced and its files overwritten, with no backup first; what the snapshot does not hold is left.")
		}
		fmt.Printf("Type the app's name, %s, to go on: ", a.App)
		line, err := readLine(ctx, bufio.NewReader(os.Stdin))
		switch {
		case errors.Is(err, errNoAnswer):
			fmt.Printf("\nno answer in %s\n", answerWithin)
			return false
		case errors.Is(err, context.Canceled):
			fmt.Println()
			return false
		case err != nil:
			fmt.Println("\nnobody answered: pass --yes to restore without being asked")
			return false
		}
		return strings.TrimSpace(line) == a.App
	}
}

// errNoAnswer is silence for answerWithin.
var errNoAnswer = errors.New("no answer")

// readLine reads one line from r, without its line end, for as long as
// answerWithin, or until ctx ends. What a read that ended with nothing
// says — end of input, nobody there — is the error as it came.
func readLine(ctx context.Context, r *bufio.Reader) (string, error) {
	type reply struct {
		line string
		err  error
	}
	said := make(chan reply, 1)
	go func() {
		line, err := r.ReadString('\n')
		said <- reply{line, err}
	}()
	select {
	case rep := <-said:
		if rep.err != nil && rep.line == "" {
			return "", rep.err
		}
		return strings.TrimRight(rep.line, "\r\n"), nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(answerWithin):
		return "", errNoAnswer
	}
}

// notLastOK says that the snapshot is not the last one a run ended ok
// on, and which that is: a run that ended incomplete leaves a snapshot
// with files left out that no check at restore can see.
func notLastOK(app string, ok *record.Snapshot) string {
	return fmt.Sprintf("%s: this is not the last snapshot a backup run ended ok on — that is %s, made %s. A run that ended incomplete leaves files out of a directory it read, which a restore cannot tell; --snapshot %s restores that one", app, record.Short(ok.ID), record.When(ok.Time), record.Short(ok.ID))
}

// notKnown says that whether a run ended ok on the snapshot is not
// known, and why: the box has no record of the app ending ok, and the
// repository's records of clean runs could not say.
func notKnown(app, why string) string {
	return fmt.Sprintf("%s: whether the run that made this snapshot ended ok is not known — %s. A run that ended incomplete leaves files out of a directory it read, which a restore cannot tell", app, why)
}

// reportRestore says what was restored, what was left and what was not,
// and never "restored" of an item that was not.
func reportRestore(rep *engine.RestoreReport) {
	if rep.Warning != "" {
		fmt.Println("warning:", rep.Warning)
	}
	if rep.LastOK != nil {
		fmt.Println(notLastOK(rep.App, rep.LastOK))
	}
	if rep.NotKnown != "" {
		fmt.Println(notKnown(rep.App, rep.NotKnown))
	}
	if rep.PreBackup != nil {
		if rep.PreBackupClass == record.OK {
			fmt.Printf("%s: backed up first: snapshot %s (restore --snapshot %s puts back what was there)\n", rep.App, record.Short(rep.PreBackup.ID), record.Short(rep.PreBackup.ID))
		} else {
			fmt.Printf("%s: backed up first, and that backup ended %s: %s — snapshot %s holds what it could, not all of what was there\n", rep.App, rep.PreBackupClass, rep.PreBackupDetail, record.Short(rep.PreBackup.ID))
		}
	}
	var restored, not []string
	for _, it := range rep.Items {
		if it.OK {
			restored = append(restored, it.Kind+" "+it.Path)
		} else {
			not = append(not, fmt.Sprintf("%s %s (%s)", it.Kind, it.Path, it.Detail))
		}
	}
	from := fmt.Sprintf("snapshot %s of %s", record.Short(rep.Snapshot.ID), record.When(rep.Snapshot.Time))
	switch {
	case len(restored) > 0:
		// The items are the ones the snapshot's own plan.json declares.
		fmt.Printf("%s: restored from %s into %s: %s\n", rep.App, from, rep.Into, strings.Join(restored, ", "))
	case len(rep.Items) > 0:
		fmt.Printf("%s: nothing was restored from %s\n", rep.App, from)
	default:
		// No answer came back: the error that follows says what is known.
	}
	if len(not) > 0 {
		fmt.Printf("%s: not restored: %s\n", rep.App, strings.Join(not, "; "))
	}
	list := func(what string, names []string, more int) {
		if len(names) == 0 {
			return
		}
		line := what + ": " + strings.Join(names, ", ")
		if more > 0 {
			line += fmt.Sprintf(", and %d more", more)
		}
		fmt.Println(line)
	}
	list("left in place, not in the snapshot", rep.Left, rep.LeftMore)
	list("in the snapshot and not installed, being neither files, directories nor links (a FIFO, socket or device)", rep.Skipped, rep.SkippedMore)
}

func drill(ctx context.Context) error {
	r, err := runner(ctx, "drill")
	if err != nil {
		return err
	}
	defer r.Close()
	st, checked, err := engine.Drill(ctx, config(), r)
	unproven, unchecked := false, false
	if st != nil {
		unproven, unchecked = reportDrill(st, checked)
	}
	if err != nil {
		return err
	}
	switch {
	case unproven && unchecked:
		return errors.New("not every app's restore was proven, and the repository's check was not clean")
	case unproven:
		return errors.New("not every app's restore was proven")
	case unchecked:
		return errors.New("the repository's check was not clean")
	}
	return nil
}

// reportDrill says what a drill proved of each app, and what its check
// of the repository found where it made one, and whether anything was
// left unproven or unchecked.
func reportDrill(st *record.Status, checked *record.Check) (unproven, unchecked bool) {
	names := make([]string, 0, len(st.Apps))
	for n := range st.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		app := st.Apps[n]
		switch {
		case app == nil:
		case app.RestoreDrill != nil:
			unproven = true
			line := fmt.Sprintf("%s: restore not proven", n)
			if app.RestoreDrill.Snapshot.ID != "" {
				line += fmt.Sprintf(": snapshot %s", record.Short(app.RestoreDrill.Snapshot.ID))
			}
			line += ": " + app.RestoreDrill.Detail
			if app.RestoreProven != nil {
				line += fmt.Sprintf(" (last proven: snapshot %s, on %s)", record.Short(app.RestoreProven.Snapshot.ID), record.When(app.RestoreProven.Time))
			}
			say(line)
		case app.RestoreProven != nil:
			say(fmt.Sprintf("%s: restore proven: snapshot %s, on %s", n, record.Short(app.RestoreProven.Snapshot.ID), record.When(app.RestoreProven.Time)))
		default:
			say(fmt.Sprintf("%s: nothing to prove: the repository holds no snapshot of it", n))
		}
	}
	// The repository's own check, where this drill made one: the
	// engine's word, not the record's last check, which another command
	// may have written since.
	if c := checked; c != nil {
		line := fmt.Sprintf("repository: its structure, and data group %s: %s", c.Group, c.Class)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		say(line)
		unchecked = c.Class != record.CheckClean
	}
	return unproven, unchecked
}

// settle is the unit that reads a fetched snapshot. Each item is
// reported on its own, so the unit's exit status says only whether it
// could report at all.
func settle(ctx context.Context, mode restore.Mode) error {
	return json.NewEncoder(os.Stdout).Encode(restore.Run(ctx, "/restore", "/target", mode))
}

var serviceRe = regexp.MustCompile(`^[A-Za-z0-9:_.@-]{1,200}\.service$`)

// ownService is the unit this run is, when it is one: the units a run
// starts are bound to it, so the manager ends them if the run is killed.
// It is what the unit says it is (Environment=MOTEL_BACKUP_UNIT=%n in
// the unit file), not a guess from the cgroup: run from a shell inside
// tmux, or by cron, the cgroup names a service that is somebody else's,
// and binding to it would end a backup when sshd restarts — or fail
// every unit, where the system manager has no such service. From a
// shell there is none, and the signal handler and the next run's sweep
// do that work.
func ownService() string {
	if name := os.Getenv("MOTEL_BACKUP_UNIT"); serviceRe.MatchString(name) {
		return name
	}
	return ""
}

func printPlan(ctx context.Context) error {
	p, err := plan.Make(ctx, caddyfile)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(p)
}

func dumpDatabases(ctx context.Context) error {
	raw, err := os.ReadFile("/plan.json")
	if err != nil {
		return err
	}
	var decl backupdecl.Config
	if err := json.Unmarshal(raw, &decl); err != nil {
		return err
	}
	if err := decl.Validate(); err != nil {
		return err
	}
	// Each database is reported on its own, so the unit's exit status
	// says only whether it could report at all.
	return json.NewEncoder(os.Stdout).Encode(dump.Databases(ctx, "/shared", "/staging", decl.SQLite))
}

// clean empties dir without removing it (it is a mount point), and
// without following anything out of it. What is in it may be a tree out
// of a snapshot, whose modes are the app's: a directory closed to
// writing — or to its owner altogether — is opened first, or what is in
// it could never be removed, and plaintext would stay for good.
func clean(dir string) error {
	// Best effort: what the opening could not reach, the removing says.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir { // by lstat: a link to a directory is not one
			_ = os.Chmod(p, 0o700) //nolint:gosec // a directory about to be removed
		}
		return nil
	})
	entries, err := os.ReadDir(dir)
	errs := []error{err}
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	return errors.Join(errs...)
}
