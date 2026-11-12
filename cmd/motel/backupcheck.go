package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/smallhoursorg/motel/liveswap"
)

// gate holds a config to three rules before Caddy's own command runs
// (checkConfig): that the admin API is off TCP where apps run
// (checkAdmin), that a monitor rule naming an app reads responses some
// route observes (checkObserved, metricscheck.go), and that what a
// Caddyfile declares as a backup is what a backup run can read of it
// (checkBackups). A run (motel-backup)
// adapts the Caddyfile inside a view that holds the Caddyfile's own
// directory and nothing else, as an account that owns nothing there: a
// backup declared in a file outside that directory, or in one that
// account may not read, is simply not in its plan, and the app is not
// backed up while every run says ok.
//
// validate and reload refuse such a config — a refused reload keeps
// the running config [measured] — and run serves it, saying why: a
// start that refused would take every site down, after a reboot or an
// unattended upgrade, over something the backup run says loudly, or
// over an admin API that the starter's own line keeps off TCP. It
// returns whether the command is to go no further.
func gate(args []string, stderr io.Writer) (stop bool) {
	stop, _ = gateArgs(args, stderr)
	return stop
}

// gateArgs is gate, and the arguments Caddy's own command is to run
// with: args, with --force added to a `motel reload` of a config with a
// monitor block (reloadForces, monitorcheck.go).
func gateArgs(args []string, stderr io.Writer) (stop bool, run []string) {
	if len(args) == 0 {
		return false, args
	}
	cmd := args[0]
	if cmd != "validate" && cmd != "reload" && cmd != "run" {
		return false, args
	}
	run = args
	cfg, warning, err := checkAndLoad(cmd, args[1:])
	if cmd == "run" && cfg != nil {
		retireMetricsRules(cfg, stderr)
	}
	if monitor := monitorJSON(cfg); monitor != nil {
		warnMonitor(cmd, monitor, stderr)
		if cmd == "reload" && reloadForces(args[1:], monitor) {
			run = append(append([]string{}, args...), "--force")
		}
	}
	switch {
	case err != nil && cmd == "run":
		_, _ = fmt.Fprintf(stderr, "ERROR: %v (served all the same; `motel reload` refuses this until it is mended)\n", err)
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Error: %v\n", err)
		stop = true
	}
	if warning != "" {
		_, _ = fmt.Fprintf(stderr, "WARNING: %s\n", warning)
	}
	return stop, run
}

// backupCaddyfile is the Caddyfile motel-backup reads, and
// backupBinary motel-backup itself (backups/cmd/motel-backup):
// both fixed by its package, which ships beside this one. Variables only
// so a test can stand others in.
var (
	backupCaddyfile = "/etc/motel/Caddyfile"
	backupBinary    = "/usr/bin/motel-backup"
)

// commandFlags parses a command's arguments with the flags Caddy's own
// command declares — its own definition, whatever Caddy is linked in,
// never a copy that could drift — so that every form Caddy takes
// (`-cFILE`, `-fc FILE`, `--config=FILE`) names the same file here. An
// error is Caddy's to report.
func commandFlags(name string, args []string) (*pflag.FlagSet, bool) {
	c, ok := caddycmd.Commands()[name]
	if !ok || c.CobraFunc == nil {
		return nil, false
	}
	cmd := &cobra.Command{Use: name}
	c.CobraFunc(cmd)
	fs := cmd.Flags()
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return nil, false
	}
	return fs, true
}

// checkConfig holds the config the command's flags name to the gate's
// three rules, each on its own, and joins what they refuse: no rule's
// early returns can drop another's.
func checkConfig(cmd string, args []string) (warning string, err error) {
	_, warning, err = checkAndLoad(cmd, args)
	return warning, err
}

// checkAndLoad is checkConfig, and hands back the config it loaded:
// nil when it loaded none.
func checkAndLoad(cmd string, args []string) (cfg []byte, warning string, err error) {
	cfg, file, adapter, err := loadConfig(cmd, args)
	if errors.Is(err, errCaddysToSay) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	adminErr, observedErr := checkAdmin(cfg), checkObserved(cfg)
	warning, err = checkBackups(cmd, file, adapter)
	var said []string
	for _, e := range []error{adminErr, observedErr, err} {
		if e != nil {
			said = append(said, e.Error())
		}
	}
	if len(said) == 0 {
		return cfg, warning, nil
	}
	return cfg, warning, errors.New(strings.Join(said, "; and "))
}

// errCaddysToSay is loadConfig's answer for a config that does not load:
// the gate says nothing, and Caddy's own command says it next, in its
// own words.
var errCaddysToSay = errors.New("left to Caddy")

// loadConfig loads the config the command's flags name with Caddy's
// own LoadConfig — its default Caddyfile, its rule for what is one, its
// adapt. A config that does not load is errCaddysToSay. One that is not
// a regular file — stdin, a pipe — is refused by name: it is read once,
// and that read is Caddy's (read here first, Caddy would be given
// nothing, and a pipe nobody writes to would never answer), so the gate
// cannot see whether it runs apps with the admin API on TCP; silence
// here would let that config through (Copilot's fifth round on #407).
//
// Each `--envfile` is loaded first, as Caddy's own command loads it
// before it adapts (loadEnvFile): a {$NAME} set only there can be the
// admin listener, or change what the Caddyfile imports, and the config
// judged has to be the one Caddy runs. An envfile Caddy would refuse
// is left to Caddy to refuse.
func loadConfig(cmd string, args []string) (cfg []byte, file, adapter string, err error) {
	fs, found := commandFlags(cmd, args)
	if !found {
		return nil, "", "", errCaddysToSay
	}
	if envfiles, err := fs.GetStringSlice("envfile"); err == nil {
		for _, envfile := range envfiles {
			if err := loadEnvFile(envfile); err != nil {
				return nil, "", "", errCaddysToSay
			}
		}
	}
	file, _ = fs.GetString("config")
	adapter, _ = fs.GetString("adapter")
	probe := file
	if probe == "" {
		probe = "Caddyfile" // what LoadConfig reads with none
	}
	if st, err := os.Stat(probe); file == "-" || err == nil && !st.Mode().IsRegular() { //nolint:gosec // the config the command was given, looked at before Caddy reads it
		where := probe
		if file == "-" {
			where = "stdin"
		}
		return nil, "", "", fmt.Errorf("the config is %s, not a regular file, which this check cannot read before Caddy reads it once — so it cannot see whether the config runs apps with the admin API on TCP, which every app could then reconfigure the server through; write the config to a file and name that", where)
	}
	liveswap.ClearBackupSources()
	cfg, file, adapter, err = caddycmd.LoadConfig(file, adapter)
	if err != nil {
		return nil, "", "", errCaddysToSay
	}
	return cfg, file, adapter, nil
}

// loadEnvFile puts an `--envfile`'s variables in the environment as
// Caddy's own command does right after (cmd/main.go, loadEnvFromFile
// and parseEnvFile, Apache-2.0, followed line for line so the two
// read one file alike): `KEY=VALUE` lines, blank lines and `#`
// comments skipped, an `export ` prefix dropped, a trailing `#`
// comment cut, a quoted value running over lines; the file read whole
// before any of it is set, so a key assigned twice takes its last
// value, as Caddy's map does; and a variable the environment already
// holds is kept. Caddy's load after this one then finds every key set
// and changes nothing.
func loadEnvFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // the envfile the command was given, read as Caddy is about to
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read only
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, cut := strings.Cut(line, "=")
		if !cut {
			return fmt.Errorf("%s: a line is not KEY=VALUE", path)
		}
		key = strings.TrimPrefix(key, "export ")
		if key == "" || strings.Contains(key, " ") || strings.HasPrefix(val, " ") || strings.HasPrefix(val, "\t") {
			return fmt.Errorf("%s: a line is not KEY=VALUE", path)
		}
		if before, _, found := strings.Cut(val, "#"); found {
			val = strings.TrimRight(before, " \t")
		}
		if strings.HasPrefix(val, `"`) || strings.HasPrefix(val, "'") {
			quote := string(val[0])
			for !strings.HasSuffix(line, quote) || strings.HasSuffix(line, `\`+quote) {
				val = strings.ReplaceAll(val, `\`+quote, quote)
				if !scanner.Scan() {
					break
				}
				line = strings.ReplaceAll(scanner.Text(), `\`+quote, quote)
				val += "\n" + line
			}
			val = strings.TrimPrefix(val, quote)
			val = strings.TrimSuffix(val, quote)
		}
		values[key] = val
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for key, val := range values {
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkBackups holds every file a backup declaration came from, in the
// config loadConfig just loaded, to the Caddyfile's directory. A config
// that is not a Caddyfile says nothing here.
func checkBackups(cmd, file, adapter string) (warning string, err error) {
	if adapter != "caddyfile" {
		return "", nil
	}
	sources := liveswap.BackupSources()
	if len(sources) == 0 {
		return "", nil
	}
	main, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	// motel-backup reads one Caddyfile. A server run from another,
	// declaring a backup, runs what no backup run reads: an app only in
	// it is never backed up, and status says all is well. Refused where
	// motel-backup is installed (a start is not refused: gate), said
	// where it is not; a candidate being validated is not asked.
	var elsewhere string
	if cmd != "validate" && !sameFile(main, backupCaddyfile) {
		elsewhere = fmt.Sprintf("this %s runs %s, which declares backups, but motel-backup reads %s: backups follow that file, not this one, and an app only in this one would not be backed up", cmd, main, backupCaddyfile)
		if _, err := os.Stat(backupBinary); err != nil {
			warning = elsewhere + " (were motel-backup installed, a reload of it would be refused)"
			elsewhere = ""
		} else {
			elsewhere += fmt.Sprintf(" — to see what a backup run would make of this file, `motel-backup validate %s`; to make it the one backups read, put it at %s (a signed push does)", main, backupCaddyfile)
		}
	}
	dir := filepath.Dir(main)
	// What a backup run's view holds is the directory itself, and so what
	// a file leads to is judged against where the directory leads.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return warning, err
	}
	var problems []string
	seen := map[string]bool{}
	say := func(format string, a ...any) {
		if p := fmt.Sprintf(format, a...); !seen[p] {
			seen[p] = true
			problems = append(problems, p)
		}
	}
	// check says what a backup run could not read of file: that it is not
	// under the directory, as written or where it leads through a link,
	// or that others may not read it or enter a directory on its way.
	check := func(what, file string) {
		// Only the main Caddyfile is ever relative, as it was given here.
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
		if !under(file, dir) {
			say("%s is read from %s, outside %s", what, file, dir)
			return
		}
		real, err := filepath.EvalSymlinks(file)
		switch {
		case err != nil:
			say("%s is read from %s, which cannot be followed: %v", what, file, err)
			return
		case !under(real, realDir):
			say("%s is read from %s, which leads to %s, outside %s", what, file, real, dir)
			return
		}
		if st, err := os.Stat(real); err == nil && st.Mode().Perm()&0o004 == 0 {
			say("%s is read from %s, which others may not read", what, file)
		}
		for d := filepath.Dir(real); ; d = filepath.Dir(d) {
			if st, err := os.Stat(d); err == nil && st.Mode().Perm()&0o001 == 0 {
				say("%s is read from under %s, which others may not enter", what, d)
			}
			if !under(d, realDir) {
				break // realDir itself, the last
			}
		}
	}
	// A candidate being validated is not the file a run reads: the box
	// applier's step 6 validates a copy it staged 0600 in a directory of
	// its own. So validate does not ask the given file's own mode, nor
	// the way to it; the reload asks them of the file installed. What
	// the candidate imports from other files is asked now. The file a
	// run reads is no candidate: validated in place, it is asked all.
	candidate := cmd == "validate" && !sameFile(main, backupCaddyfile)
	if st, err := os.Stat(main); !candidate && err == nil && st.Mode().Perm()&0o004 == 0 { //nolint:gosec // the Caddyfile the command was given; its mode is what is checked
		say("the Caddyfile, %s, may not be read by others", main)
	}
	for _, s := range sources {
		if candidate && sameFile(main, s.File) {
			continue
		}
		if s.App == "" {
			check("the liveswap root", s.File)
		} else {
			check(s.App+"'s backup", s.File)
		}
	}
	var errs []string
	if elsewhere != "" {
		errs = append(errs, elsewhere)
	}
	if len(problems) > 0 {
		errs = append(errs, fmt.Sprintf("a backup run would not see what this Caddyfile declares: %s — a backup run reads the Caddyfile inside a view that holds %s and nothing else, as an account that owns nothing there, so keep every file a backup is declared in under %s, readable by others (0644, directories 0755)", strings.Join(problems, "; "), dir, dir))
	}
	if len(errs) == 0 {
		return warning, nil
	}
	return warning, errors.New(strings.Join(errs, "; and "))
}

// checkAdmin refuses a config that runs apps while the admin API
// listens on TCP. Caddy's default is localhost:2019, and localhost
// includes every app motel runs: apps share the box's network
// namespace (liveswap/DESIGN-sandbox.md), so one that reaches the admin
// API loads a config of its own — a sibling's root and env file, the
// TLS keys served as files — with every sandbox intact. The starter
// puts the API on a unix socket (`admin unix//run/motel/admin.sock`,
// packaging/Caddyfile), which no HTTP client can be steered at and no
// app's view holds; this asks that the line stayed, whoever edited or
// pushed the file, and that the socket is where no app's view holds
// it (liveswap.AdminSocketInView: not under the liveswap root, whose
// directories are bound into the apps' sandboxes, nor under the base
// view every sandbox holds read-only), named by its whole path, since
// a relative one is Caddy's working directory's to place and this
// cannot see where that is. A config that runs no app is
// not asked, one that says the TCP listener is meant (`allow_tcp_admin`
// in the liveswap block: a test rig whose runner drives the admin API
// over the network) is let through, and one Caddy cannot read is
// Caddy's to refuse.
// adminRestart ends each refusal of checkAdmin: a new admin line takes
// effect at a restart, not a reload.
const adminRestart = "then `systemctl restart motel` once: a reload asks the admin API where the new file puts it, and the running motel answers where the old one did"

func checkAdmin(cfg []byte) error {
	var c struct {
		Admin *struct {
			Disabled bool   `json:"disabled"`
			Listen   string `json:"listen"`
		} `json:"admin"`
		Apps struct {
			Liveswap *struct {
				Root          string                     `json:"root"`
				Apps          map[string]json.RawMessage `json:"apps"`
				AllowTCPAdmin bool                       `json:"allow_tcp_admin"`
			} `json:"liveswap"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil || c.Apps.Liveswap == nil || len(c.Apps.Liveswap.Apps) == 0 || c.Apps.Liveswap.AllowTCPAdmin {
		return nil
	}
	// Caddy's default, as the config alone says it: CADDY_ADMIN in the
	// command's environment moves Caddy's own default, but a shell's
	// environment is not the unit's, and the config is what both run.
	listen, implied := "localhost:2019", true
	if c.Admin != nil {
		if c.Admin.Disabled {
			return nil
		}
		if c.Admin.Listen != "" {
			listen, implied = c.Admin.Listen, false
		}
	}
	// A unix socket on the filesystem, named whole, where no app's view
	// holds it; not an abstract one (`unix/@name`), which lives in the
	// network namespace the apps share, as a TCP port does, and not a
	// relative path, which is read against a working directory this
	// check cannot see. Placeholders in the
	// root and the path are read as Caddy reads them at load, from this
	// environment, which the envfiles have joined.
	if addr, err := caddy.ParseNetworkAddress(listen); err == nil && addr.IsUnixNetwork() && !strings.HasPrefix(addr.Host, "@") {
		sock, _, _ := strings.Cut(addr.Host, "|") // `|0660`: the socket's mode, Caddy's listener's to read
		repl := caddy.NewReplacer()
		path := repl.ReplaceKnown(sock, "")
		if !filepath.IsAbs(path) {
			// `unix/admin.sock`: Caddy opens it against the working
			// directory of whatever runs it, which this check cannot
			// see, and a unit's WorkingDirectory can put under the
			// liveswap root.
			return fmt.Errorf("the admin API listens on %s, a unix socket at a relative path, which Caddy opens against the working directory of whatever runs it — one this check cannot see, and one under the liveswap root puts the socket in an app's view; name the socket's whole path (`admin unix//run/motel/admin.sock`, as packaging/Caddyfile has it), %s", listen, adminRestart)
		}
		why := liveswap.AdminSocketInView(repl.ReplaceKnown(c.Apps.Liveswap.Root, ""), path)
		if why == "" {
			return nil
		}
		return fmt.Errorf("the admin API listens on %s, which an app this config runs can reach: %s; one that reaches the admin API reconfigures the whole server — put the socket where no app's view holds it (`admin unix//run/motel/admin.sock`, as packaging/Caddyfile has it), %s", listen, why, adminRestart)
	}
	where := listen
	if strings.HasPrefix(listen, "unix") && strings.Contains(listen, "/@") {
		where += " (an abstract socket, which is the network namespace's, not the filesystem's)"
	}
	if implied {
		where += " (Caddy's default: the config names no admin listener; a CADDY_ADMIN in this environment is not read here)"
	}
	return fmt.Errorf("the admin API listens on %s, which every app this config runs can reach: apps share the box's network namespace, and one that reaches the admin API reconfigures the whole server — put `admin unix//run/motel/admin.sock` in the global block (packaging/Caddyfile has it), or `admin off`, %s", where, adminRestart)
}

// sameFile says whether a and b are one Caddyfile: the same path, or the
// same file where each leads, read from the same directory. Caddy
// resolves a Caddyfile's relative imports from the directory it was
// given in, so the same file from another directory imports what is
// beside it there.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a) //nolint:gosec // Caddyfile paths the command was given, compared, never read
	sb, errB := os.Stat(b)
	if errA != nil || errB != nil || !os.SameFile(sa, sb) {
		return false
	}
	da, errA := os.Stat(filepath.Dir(a)) //nolint:gosec // the directory of a Caddyfile path the command was given
	db, errB := os.Stat(filepath.Dir(b))
	return errA == nil && errB == nil && os.SameFile(da, db)
}

// under says whether p is below dir.
func under(p, dir string) bool {
	return strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}
