package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	"go.uber.org/zap"
)

// systemdRunner runs app instances as transient systemd service units
// under the motel user's own service manager (user@<uid>.service,
// kept alive by lingering). That manager, not motel, is the parent
// of every app: cgroups make "stop" mean the whole process tree, the
// journal captures stdout/stderr, and units keep running while motel
// itself restarts or upgrades — Reattach adopts them afterwards.
//
// Invariants (the contract app.go and watchdog.go rely on):
//
//  1. Every unit created here has Restart=no. The liveswap watchdog is
//     the only restart authority; systemd never competes with it.
//  2. A handle's done channel closes only after an OBSERVED terminal
//     unit state (inactive, failed, or no longer loaded). A D-Bus or
//     transport error never marks a handle dead: declaring a live app
//     dead would make the watchdog launch a duplicate.
//  3. Unit names are unique per Start (random nonce), name their app
//     unambiguously (see unitName), and are recorded in state.json.
//     Reattach adopts only a unit observed running; a failed one is
//     logged with its exit status, reset, and never adopted; a unit
//     whose state cannot be read is an error, never "not running".
//  4. Stop returns nil only when the stop job reported "done" and the
//     unit was then observed gone — with KillMode=control-group and
//     SendSIGKILL the cgroup is empty at that point. Any other outcome
//     is an error, and callers keep the release on disk and skip GC.
//  5. Once a start request may have reached the manager, the unit is
//     reconciled by name: an ambiguous failure ends with the unit
//     observed running (adopted), observed gone, or reported as
//     unitUnconfirmedError — never silently dropped.
//  6. Failed units are always reset after their exit status has been
//     recorded, so the manager never accumulates dead units.
//  7. The manager is the ledger of what runs. Sweep(app, keep) returns
//     nil only when keep is the only loaded unit of app; recovery and
//     GC both sweep first, so nothing motel forgot (a unit whose
//     state.json write failed, a stop that could not be confirmed)
//     survives the next start or the next deploy — and nothing is ever
//     launched beside one: every Start is preceded by a sweep that
//     must confirm. Removing an app from the config sweeps all of it,
//     and every start sweeps the units of apps the config no longer
//     names (sweepUnknownApps), so a unit cannot outlive its app
//     definition either. (Removing the whole liveswap block and then
//     restarting motel is the one path that leaves units running:
//     on exit they are kept for reattach by design, and no liveswap
//     code runs afterwards to judge them — documented for operators.)
//  8. A unit's environment is exactly the env the caller built
//     (Environment=) on top of the manager's defaults; motel's own
//     environment is never inherited.
//
// The logger lives behind an atomic pointer because config reloads
// swap it while the watcher goroutines of running units still log.
type systemdRunner struct {
	conn   systemdConn
	logger atomic.Pointer[zap.Logger]
	poll   time.Duration // watcher interval between unit-state reads
	settle time.Duration // settleMainPID's window (mainPIDSettle; tests shrink it)

	// ctx bounds every D-Bus call and every watcher; cancel (close) is
	// for tests — in production watchers live as long as the process.
	ctx    context.Context
	cancel context.CancelFunc
}

// systemdConn is the narrow slice of the systemd D-Bus API the runner
// needs. userManagerClient (systemd_dbus.go) is the real one; tests
// script a fake.
type systemdConn interface {
	// StartTransientUnit creates and starts the unit and blocks until
	// its start job completes, returning the job result ("done",
	// "failed", "canceled", "timeout", "dependency", "skipped"). For a
	// oneshot unit that is when the command has exited.
	StartTransientUnit(ctx context.Context, u unitSpec) (string, error)
	// StopUnit enqueues a stop job and blocks until it completes.
	StopUnit(ctx context.Context, name string) (string, error)
	// UnitStatus reads the unit's load/active state and, for a loaded
	// service, its main PID and exit facts. A unit the manager no
	// longer knows is reported with LoadState "not-found", not an error.
	UnitStatus(ctx context.Context, name string) (unitStatus, error)
	// ResetFailedUnit clears a failed unit so the manager unloads it.
	ResetFailedUnit(ctx context.Context, name string) error
	// ListUnits returns the loaded units whose names match the glob.
	ListUnits(ctx context.Context, pattern string) ([]unitStatus, error)
	// SetUnitProperties sets runtime properties on a loaded unit — the
	// cgroup caps, which the manager applies to a running unit at once.
	SetUnitProperties(ctx context.Context, name string, props []sddbus.Property) error
	// DefaultTasksMax is the manager's own DefaultTasksMax=, resolved to
	// a number (or infinity, where the manager was configured so):
	// what a unit with no TasksMax= of its own is bounded to, and what
	// the manager reports as such a unit's TasksMax. 0 = unreadable.
	DefaultTasksMax(ctx context.Context) (uint64, error)
}

// unitSpec is a transient service unit as the runner wants it. The
// D-Bus client turns it into properties; the fixed hardening set
// (Restart=no, KillMode=control-group, NoNewPrivileges) is applied by
// the client to every unit and is not configurable here on purpose.
// Sandbox is the per-unit isolation set (sandbox.go); never nil —
// unitFor refuses a spec without one.
type unitSpec struct {
	Name             string
	Description      string
	SyslogIdentifier string
	WorkingDirectory string
	ExecStart        []string // ExecStart[0] is an absolute path
	Environment      []string
	Oneshot          bool // Type=oneshot (pre_start) instead of simple
	StopTimeout      time.Duration
	StartTimeout     time.Duration // TimeoutStartSec=; 0 sets none
	Sandbox          *sandboxSpec
	Caps             resourceCaps
}

// unitStatus is a snapshot of one unit as the manager reports it.
type unitStatus struct {
	Name        string
	LoadState   string // "loaded", "not-found", ...
	ActiveState string // "active", "activating", "deactivating", "inactive", "failed"
	SubState    string
	Result      string // service Result= once it has stopped
	MainPID     int
	// ExecMainPID is the main process's pid as the manager recorded it,
	// kept after the process has ended (MainPID is 0 by then) until the
	// unit is reset: the pid its journal lines carry.
	ExecMainPID int
	// ExecStart is the argv the manager runs, read back rather than
	// remembered — which is also what makes it right after a Reattach.
	ExecStart []string
	// ExecMainCode/Status describe how the main process ended, with
	// waitid semantics: code 1 = exited (status is the exit code),
	// 2 = killed and 3 = dumped (status is the signal number).
	ExecMainCode   int
	ExecMainStatus int
	// StopTimeout is the unit's own TimeoutStopSec — the SIGTERM→SIGKILL
	// budget it was created with, which is what bounds a stop of it.
	StopTimeout time.Duration
	// Sandboxed reports the unit carrying the namespaces every app unit
	// gets (PrivateUsers= and PrivatePIDs=). Reattach refuses to adopt a
	// running unit without them: nothing motel starts lacks them, so
	// such a unit was started by something else, and adopting it would
	// serve an app with no sandbox and no signal.
	Sandboxed bool
	// Caps is what the unit is bounded to as the manager holds it
	// (MemoryMax=, TasksMax=, CPUQuota=); "infinity" reads as unset.
	Caps resourceCaps
	// ControlGroup is the unit's cgroup path under /sys/fs/cgroup, as
	// the manager reports it; where the caps are, or are not, enforced.
	ControlGroup string
}

// stopBudget is how long a stop of this unit can take before the
// manager's SIGKILL lands: its own TimeoutStopSec, or the caller's
// grace when that is unknown or larger.
func (s unitStatus) stopBudget(grace time.Duration) time.Duration {
	if s.StopTimeout > grace {
		return s.StopTimeout
	}
	if grace <= 0 {
		return defaultStopTimeout
	}
	return grace
}

func (s unitStatus) loaded() bool { return s.LoadState == "loaded" }

// leftover reports a unit the manager still holds with no process to
// it, which a reset unloads: one that ended with its file loaded, or
// one that ended failed whatever became of its file — a unit whose
// file did not read back after a daemon-reload and was then stopped
// ends failed, and stays until reset like any other (#380).
func (s unitStatus) leftover() bool { return s.loaded() || s.ActiveState == "failed" }

// running reports whether the unit still has (or may still have) live
// processes: anything but a terminal state counts, including
// deactivating — a stop in progress is not yet a stopped unit. The
// active state alone decides. A unit whose file the manager could not
// read back at a daemon-reload (LoadState error or bad-setting: an
// Environment= past the 1 MiB a unit-file line may hold, a root with a
// character BindPaths= gives meaning to), or whose file is gone
// (not-found), still runs its process; a unit the manager does not
// know at all reads back with no active state (UnitStatus) (#380).
func (s unitStatus) running() bool {
	switch s.ActiveState {
	case "", "inactive", "failed":
		return false
	}
	return true
}

// exitString renders the main process's end for logs and errors. An
// ExecMainCode of 0 means the manager recorded no exit at all (the
// unit never got as far as a process — a start job that was canceled,
// skipped or failed a dependency), which must not read as "exit 0".
func (s unitStatus) exitString() string {
	switch s.ExecMainCode {
	case 1: // CLD_EXITED
		return s.oomSuffix(fmt.Sprintf("exit status %d", s.ExecMainStatus))
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		sig := syscall.Signal(s.ExecMainStatus)
		return s.oomSuffix(fmt.Sprintf("killed by signal %d (%s)", s.ExecMainStatus, sig))
	default:
		if s.Result != "" && s.Result != "success" {
			return "no process exit recorded (result " + s.Result + ")"
		}
		return "no process exit recorded"
	}
}

// oomSuffix names the OOM kill the manager recorded for the unit
// (Result=oom-kill), whatever the main process's own end looked like:
// the kernel takes the process that asked for the memory, which under
// a `sh -c` or `npm start` command is a child, and the manager then
// stops the unit (OOMPolicy=stop) — so the main process reads as an
// exit code or as the stop's SIGTERM, and only the Result says why.
func (s unitStatus) oomSuffix(exit string) string {
	if s.Result != "oom-kill" {
		return exit
	}
	return exit + "; the kernel's OOM killer took a process of the unit (its memory_max, or the box's memory), and the unit was stopped for it"
}

const (
	// unitPollInterval bounds how quickly a crash is noticed; the
	// watchdog's own health interval is the coarser clock above it.
	unitPollInterval = 500 * time.Millisecond
	// startJobTimeout bounds the D-Bus start job for a simple service,
	// which completes as soon as the manager has forked the process.
	startJobTimeout = 30 * time.Second
	// stopSlack is how long past the unit's own stop budget a Stop call
	// waits for the manager before giving up on confirmation.
	stopSlack = 30 * time.Second
	// defaultStopTimeout backs a zero grace so a unit never gets
	// TimeoutStopSec=0, which systemd reads as "wait forever".
	defaultStopTimeout = 10 * time.Second
	// pollTimeout bounds one watcher status read.
	pollTimeout = 10 * time.Second
)

func newSystemdRunner(conn systemdConn, logger *zap.Logger) *systemdRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &systemdRunner{conn: conn, poll: unitPollInterval, settle: mainPIDSettle, ctx: ctx, cancel: cancel}
	r.logger.Store(logger)
	return r
}

func (r *systemdRunner) setLogger(logger *zap.Logger) { r.logger.Store(logger) }
func (r *systemdRunner) log() *zap.Logger             { return r.logger.Load() }

// close stops every watcher without closing their done channels (tests).
func (r *systemdRunner) close() { r.cancel() }

// systemdHandle tracks one unit. done is closed by the watcher
// goroutine — and only by it — once the unit is observed gone.
type systemdHandle struct {
	unit      string
	pid       atomic.Int64 // MainPID; 0 until read, backfilled by the watcher
	startedAt time.Time
	// stopTimeout is the unit's own SIGTERM→SIGKILL budget as known
	// when the handle was made (the start spec's grace, or the value
	// read at reattach) — the fallback when a fresh read fails.
	stopTimeout time.Duration
	// argv is the unit's command, read back at adopt and at reattach.
	// Written once, but the read can fail (the D-Bus outage that also
	// leaves pid unread) and the watcher repairs both — so it is
	// atomic like pid, not plain like unit and startedAt.
	argv atomic.Pointer[[]string]
	// caps is what the unit is bounded to as the manager last applied
	// it: the start spec's, the values read at reattach, or the last
	// SetCaps. Atomic for the same reason as argv: status reads it
	// while a reload's SetCaps writes it.
	caps atomic.Pointer[resourceCaps]
	// capsUnknown is set when a SetCaps failed ambiguously and the
	// unit could not be read back (readBackCaps): caps is then the
	// last known, not the applied, and no sync may take it as such.
	capsUnknown atomic.Bool
	// cgroup is the unit's cgroup path as read at adopt, at reattach or
	// by the watcher's backfill; controllers the set that reached it at
	// the last reading of its cgroup.controllers (nil = never read); and
	// unenforced the directives whose controller is not in that set
	// (missingCaps). Written only by setCapsState, under capsLock, so
	// the list is always the one computed from the caps beside it;
	// state takes the same lock to read them together, so a status
	// never pairs new caps with the list computed for the old ones.
	cgroup      atomic.Pointer[string]
	controllers atomic.Pointer[[]string]
	unenforced  atomic.Pointer[[]string]
	capsLock    sync.Mutex
	done        chan struct{}
	exit        atomic.Pointer[unitStatus] // final status, set before done closes
}

func (h *systemdHandle) state() handleState {
	st := handleState{PID: int(h.pid.Load()), StartedAt: h.startedAt, Unit: h.unit}
	if argv := h.argv.Load(); argv != nil {
		st.Command = *argv
	}
	h.capsLock.Lock()
	st.CapsUnknown = h.capsUnknown.Load()
	if caps := h.caps.Load(); caps != nil {
		st.Caps = *caps
	}
	if u := h.unenforced.Load(); u != nil {
		st.CapsUnenforced = *u
	}
	if cg := h.cgroup.Load(); cg != nil {
		st.Cgroup = *cg
	}
	h.capsLock.Unlock()
	return st
}

// cgroupRoot is where the kernel's cgroup tree is mounted; a variable
// for the unit tests, which point it at a directory of their own.
var cgroupRoot = "/sys/fs/cgroup"

// readControllers reads the controllers that reach the unit's cgroup
// (its cgroup.controllers). No cgroup path, or an unreadable file, is
// no reading (ok false), never an empty set: "unreadable" must not be
// taken for "no controller present", nor for "every controller
// present".
func readControllers(root, cg string) (have []string, ok bool) {
	if cg == "" {
		return nil, false
	}
	// cg is the manager's own report of the unit's cgroup path, under
	// the cgroup root; nothing of an app's or a request's reaches it.
	b, err := os.ReadFile(filepath.Join(root, cg, "cgroup.controllers")) //nolint:gosec // path from the manager, under the cgroup root
	if err != nil {
		return nil, false
	}
	return strings.Fields(string(b)), true
}

// missingCaps names the caps among caps whose controller is not in
// have: the manager accepts and reports MemoryMax= on a host whose
// user@.service does not delegate the memory controller (a container,
// say), and the kernel then bounds nothing. Debian 13, Ubuntu 24.04
// and 26.04 and Fedora 43 delegate cpu, memory and pids (measured), so
// on a supported host this is empty.
func missingCaps(have []string, caps resourceCaps) []string {
	var missing []string
	if caps.memoryMax > 0 && !slices.Contains(have, "memory") {
		missing = append(missing, "memory_max")
	}
	if caps.tasksMax > 0 && !slices.Contains(have, "pids") {
		missing = append(missing, "tasks_max")
	}
	if caps.cpuQuota > 0 && !slices.Contains(have, "cpu") {
		missing = append(missing, "cpu_quota")
	}
	return missing
}

// setCapsState is the one writer of a handle's caps, cgroup path,
// controllers, unenforced list and unknown flag. Under the handle's
// lock, so the watcher's backfill of the cgroup path and a reload's
// SetCaps cannot leave the list computed from the other's caps
// (whichever writes last recomputes from both as they then are), and
// a status read (state) sees the caps and the flag that settles them
// together, never new caps under a stale "unknown". A nil caps, cgroup
// or unknown leaves that field as it is. The controllers are read
// afresh each time; a reading that fails leaves the last one standing,
// and the list is computed from that one for the caps as they now are
// (a cap added or removed meanwhile is classified either way), and
// with no reading ever made nothing is claimed either way (the
// watcher keeps trying, DESIGN-liveswap.md "Resource caps"). A cap
// that reads as set and bounds nothing is the "looks configured,
// quietly weaker" shape this code refuses to present as effective: it
// is said in the journal when it first appears in the list, and status
// lists such caps apart.
func (r *systemdRunner) setCapsState(h *systemdHandle, caps *resourceCaps, cgroup *string, unknown *bool) {
	h.capsLock.Lock()
	if caps != nil {
		h.caps.Store(caps)
	}
	if unknown != nil {
		h.capsUnknown.Store(*unknown)
	}
	if cgroup != nil && *cgroup != "" {
		h.cgroup.Store(cgroup)
	}
	var cur resourceCaps
	if c := h.caps.Load(); c != nil {
		cur = *c
	}
	cg := ""
	if p := h.cgroup.Load(); p != nil {
		cg = *p
	}
	if have, ok := readControllers(cgroupRoot, cg); ok {
		h.controllers.Store(&have)
	}
	var missing []string
	if have := h.controllers.Load(); have != nil {
		missing = missingCaps(*have, cur)
	}
	prev := h.unenforced.Swap(&missing)
	h.capsLock.Unlock()
	// Said after the unlock: a status read waits on the lock, and a
	// journal that stalls this write must not stall status with it.
	// Said for a cap new to the list: a list that only shrank is not
	// news, and may rest on a reading that was not made this time.
	var fresh []string
	for _, name := range missing {
		if prev == nil || !slices.Contains(*prev, name) {
			fresh = append(fresh, name)
		}
	}
	if len(fresh) > 0 {
		r.log().Warn("resource caps the host does not enforce: the user manager does not delegate their controller to the unit's cgroup, so the kernel bounds nothing; status lists them as caps_unenforced",
			zap.String("unit", h.unit), zap.Strings("caps", missing), zap.String("cgroup", cg))
	}
}

// setArgv records the command the manager reports, if it is not
// already known. Only the first non-empty read counts: a unit's
// ExecStart does not change, so a later one can only repeat it.
func (h *systemdHandle) setArgv(argv []string) {
	if len(argv) > 0 {
		h.argv.CompareAndSwap(nil, &argv)
	}
}

// Unit naming: motel-<app>.<version>.<nonce>[.prestart].service.
// App names are [a-z0-9-] and version tags [A-Za-z0-9._-] (validated
// in liveswap.go), all legal unit-name characters, so nothing is ever
// escaped. The "." after the app is the one character an app name
// cannot contain, which is what lets Sweep match "blog" without also
// matching "blog-api"; the nonce makes every Start unique, and is the
// same one that names the instance's socket (newNonce).
const unitPrefix = "motel-"

// unitNameRe is derived from the app-name and version alphabets in
// liveswap.go so the three can never drift apart.
var unitNameRe = regexp.MustCompile(
	`^` + regexp.QuoteMeta(unitPrefix) +
		`(` + strings.Trim(appNameRe.String(), "^$") + `)` +
		`\.(` + strings.Trim(versionRe.String(), "^$") + `)` +
		`\.(` + strings.Trim(nonceRe.String(), "^$") + `)(\.prestart)?\.service$`)

func unitName(spec startSpec, oneshot bool) (string, error) {
	// The webhook validates both before a deploy gets this far; the
	// runner enforces its own precondition anyway ("." and ".." match
	// the version alphabet but are not versions).
	if !spec.probe && (!appNameRe.MatchString(spec.app) || !validVersion(spec.version)) {
		return "", fmt.Errorf("cannot derive a unit name from app %q version %q", spec.app, spec.version)
	}
	if !nonceRe.MatchString(spec.nonce) {
		return "", fmt.Errorf("cannot derive a unit name for app %q version %q: instance nonce %q is not %s", spec.app, spec.version, spec.nonce, nonceRe)
	}
	if spec.probe {
		// Underscores are outside the app-name alphabet, so this name
		// can never be produced from config and unitNameRe never
		// matches it: unitApp reports "not an app unit" and every
		// sweep skips it (a probe named like an app would be stopped
		// by a concurrent sweepUnknownApps, or collide with an app
		// actually called "sandbox-probe", failing the start for no
		// reason at all).
		return unitPrefix + "sandboxprobe_" + spec.nonce + ".service", nil
	}
	name := unitPrefix + spec.app + "." + spec.version + "." + spec.nonce
	if oneshot {
		name += ".prestart"
	}
	name += ".service"
	if !unitNameRe.MatchString(name) {
		return "", fmt.Errorf("cannot derive a unit name from app %q version %q", spec.app, spec.version)
	}
	return name, nil
}

// unitBelongsTo reports whether name is one of app's units.
func unitBelongsTo(name, app string) bool {
	got, ok := unitApp(name)
	return ok && got == app
}

// unitApp extracts the app name from one of our unit names.
func unitApp(name string) (string, bool) {
	m := unitNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// unitPreStart reports whether name is one of our pre_start units,
// and the version it prepares.
func unitPreStart(name string) (string, bool) {
	m := unitNameRe.FindStringSubmatch(name)
	if m == nil || m[4] == "" {
		return "", false
	}
	return m[2], true
}

// unitNonce extracts the instance nonce from one of our unit names.
func unitNonce(name string) (string, bool) {
	m := unitNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[3], true
}

// resolveCommand turns command[0] into the absolute path a transient
// unit needs: transient ExecStart= is not resolved against the unit's
// working directory, so "./server" must become "<release>/server".
// exec.LookPath also checks the file is executable, which turns a
// missing or non-executable binary into a clear Start error instead of
// a unit that fails a moment later at the sibling shell's exec (exit
// status 127, "not found" in the journal).
func resolveCommand(cmd, dir string) (string, error) {
	if strings.Contains(cmd, "/") && !filepath.IsAbs(cmd) {
		cmd = filepath.Join(dir, cmd)
	}
	return exec.LookPath(cmd)
}

// resolveInView is the part of a launch that can be checked before a
// unit exists: command[0] as the absolute path the unit will exec,
// and the file that path finally names, inside the sandbox view. Both
// Preflight and unitFor run it — a deploy or a relaunch to refuse
// early, the launch as the last word before the manager acts.
func resolveInView(spec startSpec) (argv0, target string, err error) {
	if len(spec.command) == 0 {
		return "", "", errors.New("empty command")
	}
	if spec.sandbox == nil {
		return "", "", fmt.Errorf("unit for app %s %s has no sandbox spec: every app unit is sandboxed, so this is a caller bug", spec.app, spec.version)
	}
	argv0, err = resolveCommand(spec.command[0], spec.dir)
	if err != nil {
		// A command that is not there, or not executable: the release
		// as shipped, or the command as configured — the deployer's.
		return "", "", &preflightError{err.Error()}
	}
	// As late as possible, and before the manager follows any of them:
	// what a bind source points at is only knowable now (see
	// resolveBindSources). A refusal fails the launch — a deploy falls
	// back to the version still serving.
	if err := spec.sandbox.resolveBindSources(); err != nil {
		return "", "", err
	}
	// LookPath above ran in *this* process's view of the filesystem,
	// which under a deny-by-default sandbox is not the unit's: a
	// runtime installed outside /usr — /opt/node/bin/node, an asdf
	// or nvm shim under a home directory — resolves here and then
	// does not exist in there. The sibling shell would report that as
	// `exec: …: not found`, exit status 127, after the unit has
	// already been created, so say it now, in the language the
	// operator can act on.
	// Resolved, because LookPath does not follow symlinks: a shim at
	// /usr/local/bin/node -> /opt/node/bin/node is under the /usr
	// base entry and would pass unresolved, then die at the very
	// exec this check exists to replace — the symlink is inside the
	// view, its target is not.
	target = argv0
	if t, err := filepath.EvalSymlinks(argv0); err == nil {
		target = t
	}
	if !spec.sandbox.inView(target) {
		via := ""
		if target != argv0 {
			via = fmt.Sprintf(" (via %s, which is)", argv0)
		}
		return "", "", &preflightError{fmt.Sprintf("%s is not inside the sandbox view of app %s%s: an app sees its release dir, its shared dir and the OS runtime (/usr and a named handful of /etc), and nothing else on this host — ship the runtime inside the release, or install it under /usr", target, spec.app, via)}
	}
	if err := siblingToolsPresent(siblingShell, siblingSleep); err != nil {
		return "", "", err
	}
	return argv0, target, nil
}

// siblingToolsPresent: the shell and the sleep every unit's command
// runs behind (withSibling) exist and execute on this host. Both are
// Essential on Debian and under /usr, in every view, so this is a
// host that is not the supported one. The script itself exits 127 if
// its `sleep 0` cannot run (red, the watchdog's crash); this check
// says so before a unit exists, in the language the operator can act
// on.
func siblingToolsPresent(shell, sleep string) error {
	for _, p := range []string{shell, sleep} {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			return &preflightError{fmt.Sprintf("%s is missing or not executable on this host: every app unit runs its command behind `%s -c '%s'`, so the manager waits the app's grace on systemd 257 (#380); the supported host is Debian 13, where both are Essential", p, shell, siblingScript)}
		}
	}
	return nil
}

// Preflight is a launch's early word, a deploy's and a relaunch's: the
// command resolves inside the view, and the file it names was built
// for this machine.
func (r *systemdRunner) Preflight(spec startSpec) error {
	if err := checkUnitLines(spec); err != nil {
		return err
	}
	_, target, err := resolveInView(spec)
	if err != nil {
		return err
	}
	return checkMachine(target)
}

// unitLineMax is the longest line a unit file may hold (systemd's
// LONG_LINE_MAX, 1 MiB). unitLineBudget is the room motel allows a
// unit's Environment= and ExecStart=, each written as one line of the
// transient unit's file: half the limit, since unitLineLength is an
// estimate of the manager's escaping and not its letter.
const (
	unitLineMax    = 1 << 20
	unitLineBudget = unitLineMax / 2
)

// unitLineLength estimates the line the manager writes for a list
// setting: each entry quoted and C-escaped — a control or non-ASCII
// byte becomes four (measured: 30,000 bytes of CJK became a line of
// 120,187), a quote, a backslash or a specifier's % two.
func unitLineLength(key string, entries []string) int {
	n := len(key) + 1
	for _, e := range entries {
		n += 3 // the quotes and the separator
		for i := 0; i < len(e); i++ {
			switch c := e[i]; {
			case c < 0x20 || c >= 0x7f:
				n += 4
			case c == '\\' || c == '"' || c == '\'' || c == '%':
				n += 2
			default:
				n++
			}
		}
	}
	return n
}

// checkUnitLines refuses an environment or a command the manager could
// start but not read back. A transient unit's file holds the whole
// Environment= on one line and the whole ExecStart= on another, and a
// line past unitLineMax makes the unit's next daemon-reload — which
// every systemd upgrade performs on the user manager — read it back as
// LoadState=error: its stop timeout the manager's default, its sandbox
// settings unknown to the manager, a reattach refusing it (#380,
// measured with 1.2 MiB of environment). Named here, at the pre-flight,
// with the largest variable, rather than months later.
func checkUnitLines(spec startSpec) error {
	if n := unitLineLength("Environment", spec.env); n > unitLineBudget {
		largest, size := "", 0
		for _, e := range spec.env {
			if l := unitLineLength("", []string{e}); l > size {
				largest, size = e, l
			}
		}
		key, _, found := strings.Cut(largest, "=")
		if !found {
			key = "an entry with no =" // never the value
		}
		return &preflightError{fmt.Sprintf("the environment of app %s would be a %d-byte line in its unit file (the largest variable, %s, %d bytes of it), over the %d-byte bound: a line past 1 MiB makes the unit unreadable to systemd after a daemon-reload — move large values out of env_file and the Caddyfile, into a file under the app's shared dir", spec.app, n, key, size, unitLineBudget)}
	}
	if n := unitLineLength("ExecStart", withSibling(spec.command)); n > unitLineBudget { // the argv as the unit carries it, behind its sibling shell
		return &preflightError{fmt.Sprintf("the command of app %s would be a %d-byte line in its unit file, over the %d-byte bound: a line past 1 MiB makes the unit unreadable to systemd after a daemon-reload — shorten its arguments", spec.app, n, unitLineBudget)}
	}
	return nil
}

func (r *systemdRunner) unitFor(spec startSpec, oneshot bool) (unitSpec, error) {
	if len(spec.command) == 0 {
		return unitSpec{}, errors.New("empty command")
	}
	name, err := unitName(spec, oneshot)
	if err != nil {
		return unitSpec{}, err
	}
	// Every unit is sandboxed. A spec without one is a caller bug, and
	// the one way an app could ever run bare, so it is refused here —
	// before a unit exists — rather than rendered with the floor only.
	if spec.sandbox == nil {
		return unitSpec{}, fmt.Errorf("unit for app %s %s has no sandbox spec: every app unit is sandboxed, so this is a caller bug", spec.app, spec.version)
	}
	argv0, _, err := resolveInView(spec)
	if err != nil {
		return unitSpec{}, err
	}
	if err := checkUnitLines(spec); err != nil {
		return unitSpec{}, err
	}
	// HOME is a default buildEnv applies before env_file and inline
	// env, so an operator may point it elsewhere on purpose. One
	// pointed outside the view is allowed and reported: it is the
	// difference between "npm failed" and "npm's HOME was never in
	// my view".
	if home, outside := homeOutsideView(spec.env, spec.sandbox); outside {
		r.log().Warn("HOME is set to a path the sandbox does not bind; it does not exist inside the unit",
			zap.String("app", spec.app),
			zap.String("home", home),
			zap.String("effect", "any runtime that touches $HOME (npm, corepack, pip) will fail with ENOENT naming no cause"),
			zap.String("fix", "leave HOME unset to get the app's shared dir, the one writable persistent path in the view"))
	}
	desc := "motel app " + spec.app + " " + spec.version
	if oneshot {
		desc += " (pre_start)"
	}
	grace := spec.grace
	if grace <= 0 {
		grace = defaultStopTimeout
	}
	env := spec.env
	if env == nil {
		env = []string{}
	}
	return unitSpec{
		Name:             name,
		Description:      desc,
		SyslogIdentifier: syslogIdentifier(spec.app),
		WorkingDirectory: spec.dir,
		ExecStart:        append([]string{argv0}, spec.command[1:]...),
		Environment:      env,
		Oneshot:          oneshot,
		StopTimeout:      grace,
		StartTimeout:     spec.startTimeout,
		Sandbox:          spec.sandbox,
		Caps:             spec.caps,
	}, nil
}

// syslogIdentifier is what every unit of an app writes its output
// under, the pre_start's and every instance's alike: what `journalctl
// -t motel-<app>` finds it by.
func syslogIdentifier(app string) string { return "motel-" + app }

// Start creates the unit; the start job of a simple service completes
// once the manager has forked it, so this returns promptly. A unit
// that then dies at exec time is caught by the watcher and surfaces as
// an instance exit, exactly like any other early crash.
func (r *systemdRunner) Start(spec startSpec) (handle, error) {
	u, err := r.unitFor(spec, false)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.ctx, startJobTimeout)
	defer cancel()
	res, err := r.conn.StartTransientUnit(ctx, u)
	if err != nil {
		// The request may or may not have reached the manager
		// (invariant 5): reconcile by name rather than guess.
		return r.reconcileStart(u, fmt.Errorf("starting unit %s: %w", u.Name, err))
	}
	// The reads below get a context of their own, as reconcileStart's
	// do: the job's may have ended with its result already in
	// (jobResult takes the result then), and a read on it would fail.
	readCtx, readCancel := context.WithTimeout(r.ctx, pollTimeout)
	defer readCancel()
	if res != "done" {
		st := r.reapFailed(readCtx, u.Name)
		return nil, startExit(st.exitString(), u.Name, res, st.ExecMainPID)
	}
	return r.adopt(readCtx, u.Name, time.Now(), u.StopTimeout, u.Caps), nil
}

// SetCaps sets the unit's caps as runtime properties — the manager
// applies cgroup properties to a running unit at once (measured on
// systemd 255, 257 and 259: the kernel's memory.max, pids.max and
// cpu.max change with the process untouched). Every cap is sent, so a
// cap a reload removed is lifted: memory and CPU to infinity, which is
// their default, and tasks to the manager's DefaultTasksMax, which is
// what an unset TasksMax= means (infinity only where that is the
// manager's default: otherwise it would lift the manager's own bound,
// 15% of threads-max, from the unit). A unit the handle knows carries
// no tasks cap gets no TasksMax= written: there is nothing to lift.
// A lift writes the default's number into the unit, which is also
// what the kernel holds for an untouched unit until its next launch:
// a later change of DefaultTasksMax= reaches neither running unit
// (measured, DESIGN-liveswap.md "Resource caps"). With the handle's
// caps unknown the lift is written whatever the unit may carry. With
// the default unreadable the unit keeps its tasks cap, and the handle
// says so.
func (r *systemdRunner) SetCaps(h handle, caps resourceCaps) error {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return errors.New("not a systemd handle")
	}
	ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
	defer cancel()
	lift := uint64(0)
	applied := caps
	have := sh.caps.Load()
	if caps.tasksMax == 0 && (sh.capsUnknown.Load() || have == nil || have.tasksMax > 0) {
		def, err := r.conn.DefaultTasksMax(ctx)
		if err != nil || def == 0 {
			r.log().Warn("the manager's DefaultTasksMax could not be read; the unit keeps its tasks cap until its next launch", zap.String("unit", sh.unit), zap.Error(err))
			if have != nil {
				applied.tasksMax = have.tasksMax
			}
		} else {
			lift = def
		}
	}
	if err := r.conn.SetUnitProperties(ctx, sh.unit, capProperties(caps, true, lift)); err != nil {
		// Ambiguous (invariant 5): the manager may have applied the
		// properties before the transport failed, and the handle's
		// caps would then be stale — a later sync back to them would
		// find them "already applied" and send nothing. Read the unit
		// back on a context of its own; with no answer the caps are
		// marked unknown, which no sync takes as applied.
		r.readBackCaps(sh)
		return fmt.Errorf("setting the caps of unit %s: %w", sh.unit, err)
	}
	// Settled only for what was written: a tasks cap left as it was
	// (no default to lift to) stays as uncertain as it was.
	var settled *bool
	if caps.tasksMax > 0 || lift > 0 {
		settled = new(bool)
	}
	r.setCapsState(sh, &applied, nil, settled)
	return nil
}

// readBackCaps replaces the handle's caps with what the manager holds
// for the unit, or marks them unknown when the manager cannot be
// asked: the ledger is the manager's, and a handle that cannot read
// it must not claim to know.
func (r *systemdRunner) readBackCaps(sh *systemdHandle) {
	ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
	defer cancel()
	us, err := r.conn.UnitStatus(ctx, sh.unit)
	if err != nil || !us.loaded() {
		unknown := true
		r.setCapsState(sh, nil, nil, &unknown)
		return
	}
	caps := r.unitCaps(ctx, us)
	// The cgroup path with it, and the enforcement recomputed from
	// what the manager says the unit now carries: a cap the failed
	// write did apply is listed as the host enforces it, or not.
	r.setCapsState(sh, &caps, &us.ControlGroup, new(bool))
}

// unitCaps is what the manager's report of a unit means as caps: a
// TasksMax equal to the manager's default is no cap of the operator's
// (the manager resolves an unset TasksMax= to its default and reports
// the number, never "infinity"), so it reads as unset. With the
// default unreadable the number stands, and a sync re-sends it.
func (r *systemdRunner) unitCaps(ctx context.Context, us unitStatus) resourceCaps {
	caps := us.Caps
	if caps.tasksMax > 0 {
		if def, err := r.conn.DefaultTasksMax(ctx); err == nil && uint64(caps.tasksMax) == def {
			caps.tasksMax = 0
		}
	}
	return caps
}

// adopt builds a watched handle for a unit known to be running.
func (r *systemdRunner) adopt(ctx context.Context, unit string, startedAt time.Time, stopTimeout time.Duration, caps resourceCaps) *systemdHandle {
	h := &systemdHandle{unit: unit, startedAt: startedAt, stopTimeout: stopTimeout, done: make(chan struct{})}
	h.caps.Store(&caps)
	if st, err := r.conn.UnitStatus(ctx, unit); err == nil {
		h.pid.Store(int64(st.MainPID))
		h.setArgv(st.ExecStart)
		r.setCapsState(h, nil, &st.ControlGroup, nil)
		// Spelled as sandboxProperties spells it (systemd_dbus.go):
		// every unit gets PrivatePIDs=, so every unit has the
		// intermediate PID to settle past.
		r.settleMainPID(ctx, h, st.MainPID)
	} else {
		r.log().Warn("unit started but its main PID could not be read yet", zap.String("unit", unit), zap.Error(err))
	}
	r.log().Info("instance started", zap.String("unit", unit), zap.Int64("pid", h.pid.Load()))
	go r.watch(h)
	return h
}

// settleMainPID waits, briefly, for the MainPID of a unit in its own
// PID namespace to be the app's: with PrivatePIDs= the manager first
// reports the intermediate it forked to set the namespace up, and
// switches to that intermediate's child — the app — a few
// milliseconds later. A pid read in between is stale (and dead almost
// at once); status, state.json and the watchdog all read the handle,
// so the switch is awaited here rather than at the next watcher poll.
// Bounded: if the manager never changes its mind the first value
// stands and the watcher keeps following it.
//
// Two measurements on systemd 257 say what this can and cannot be:
//
//   - Nothing the manager exposes distinguishes the intermediate from
//     the settled pid — ExecMainPID tracks MainPID exactly and changes
//     with it — so observing the value change is the only signal, and
//     a unit already settled when adopt first reads it runs the whole
//     window out.
//   - That case is the minority: StartTransientUnit returns when the
//     job is done, and over ten such starts the pid still changed
//     afterwards in nine.
//
// So the wait earns its keep and must not be deleted. Its cost is the
// window, not the reads: pacing cannot shrink 500ms of waiting, and
// stepping the poll interval up only delays the nine-in-ten case that
// exits early. It therefore polls at a flat interval, and the real
// fix — ending the wait the instant the manager publishes the change,
// via a PropertiesChanged subscription on the unit — is its own
// change, because it needs a signal path this client does not have.
func (r *systemdRunner) settleMainPID(ctx context.Context, h *systemdHandle, first int) {
	deadline := time.Now().Add(r.settle)
	// baseline is the pid the switch must move away from. When adopt
	// read the unit before the manager had published any MainPID it is
	// 0, and the first non-zero value is the intermediate rather than
	// the app: adopting it as the baseline (and as the handle's pid,
	// which beats leaving 0 there) keeps the wait honest instead of
	// returning on a pid that is about to die.
	baseline := first
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(mainPIDSettleStep):
		}
		st, err := r.conn.UnitStatus(ctx, h.unit)
		if err != nil || !st.running() {
			return
		}
		if st.MainPID == 0 {
			continue
		}
		if baseline == 0 {
			h.pid.Store(int64(st.MainPID))
			baseline = st.MainPID
			continue
		}
		if st.MainPID != baseline {
			h.pid.Store(int64(st.MainPID))
			return
		}
	}
}

const (
	mainPIDSettle     = 500 * time.Millisecond
	mainPIDSettleStep = 10 * time.Millisecond
)

// reconcileStart settles an ambiguous start: the unit is observed
// running (adopted — the deploy proceeds), observed gone (a plain
// failure), or, when the manager cannot even be asked, stopped on a
// best-effort basis and reported as possibly running.
func (r *systemdRunner) reconcileStart(u unitSpec, cause error) (handle, error) {
	unit := u.Name
	readCtx, readCancel := context.WithTimeout(r.ctx, pollTimeout)
	st, err := r.conn.UnitStatus(readCtx, unit)
	readCancel()
	switch {
	case err == nil && st.running():
		r.log().Warn("start reported an error but the unit is running; adopting it", zap.String("unit", unit), zap.Error(cause))
		ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
		defer cancel()
		return r.adopt(ctx, unit, time.Now(), u.StopTimeout, u.Caps), nil
	case err == nil:
		if st.leftover() {
			ctx, cancel := context.WithTimeout(r.ctx, pollTimeout)
			_ = r.resetFailed(ctx, unit)
			cancel()
		}
		return nil, cause
	}
	// The manager cannot be asked: stop by name and confirm, or report
	// the unit as possibly running — the same sequence RunOnce uses,
	// given the stop budget the unit was created with.
	ctx, cancel := context.WithTimeout(r.ctx, u.StopTimeout+stopSlack)
	defer cancel()
	return nil, r.stopUnobserved(ctx, unit, cause)
}

// RunOnce runs a oneshot unit to completion. Its start job only
// completes when the command has exited, so the job result is the
// verdict; a failed unit is reset after its exit status is read.
func (r *systemdRunner) RunOnce(ctx context.Context, spec startSpec) error {
	u, err := r.unitFor(spec, true)
	if err != nil {
		return err
	}
	res, err := r.conn.StartTransientUnit(ctx, u)
	if err != nil {
		cause := fmt.Errorf("starting unit %s: %w", u.Name, err)
		if ctx.Err() != nil {
			cause = ctx.Err()
		}
		// Cancelled, or the transport failed mid-flight: either way
		// the unit may be running, and a pre_start whose outcome we
		// cannot observe is a failed pre_start. Stop it and confirm,
		// on the runner's own context — the caller's is already done.
		stopCtx, cancel := context.WithTimeout(r.ctx, u.StopTimeout+stopSlack)
		defer cancel()
		return r.stopUnobserved(stopCtx, u.Name, cause)
	}
	if res == "done" {
		return nil
	}
	reapCtx, cancel := context.WithTimeout(r.ctx, stopSlack)
	defer cancel()
	st := r.reapFailed(reapCtx, u.Name)
	return runOnceExit(st.exitString(), u.Name, res, st.ExecMainPID)
}

// Exit is the recorded end of the instance's main process, once the
// watcher has seen it (finish); "" before that.
func (r *systemdRunner) Exit(h handle) (string, int) {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return "", 0
	}
	st := sh.exit.Load()
	if st == nil {
		return "", int(sh.pid.Load())
	}
	// The pid as the manager recorded it; from a unit unloaded before
	// that could be read, the one the handle followed.
	pid := st.ExecMainPID
	if pid == 0 {
		pid = int(sh.pid.Load())
	}
	return st.exitAfterEnd(), pid
}

// exitAfterEnd is exitString for a unit the watcher saw end. A clean
// exit unloads a transient unit before the watcher looks again, and
// the manager then has no ExecMainCode to give: that is a success
// result whose status was not read, not "no exit recorded".
func (s unitStatus) exitAfterEnd() string {
	if s.ExecMainCode == 0 && !s.loaded() && (s.Result == "" || s.Result == "success") {
		return "exited without failure (the unit was unloaded before its exit status could be read)"
	}
	return s.exitString()
}

// stopUnobserved stops a unit and returns cause if the unit is
// confirmed gone, or an unconfirmed error if not.
func (r *systemdRunner) stopUnobserved(ctx context.Context, unit string, cause error) error {
	res, err := r.conn.StopUnit(ctx, unit)
	if err != nil {
		return &unitUnconfirmedError{unit: unit, err: cause}
	}
	if res != "done" {
		return &unitUnconfirmedError{unit: unit, err: fmt.Errorf("%w (stop job %s)", cause, res)}
	}
	st, err := r.conn.UnitStatus(ctx, unit)
	if err != nil || st.running() {
		return &unitUnconfirmedError{unit: unit, err: cause}
	}
	if st.leftover() {
		if rerr := r.resetFailed(ctx, unit); rerr != nil {
			return &unitUnconfirmedError{unit: unit, err: errors.Join(cause, rerr)}
		}
	}
	return cause
}

// Alive reports whether the watcher has not yet seen the unit gone.
func (r *systemdRunner) Alive(h handle) bool {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return false
	}
	select {
	case <-sh.done:
		return false
	default:
		return true
	}
}

// Wait returns the watcher's done channel (never nil for our handles).
func (r *systemdRunner) Wait(h handle) <-chan struct{} {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return nil
	}
	return sh.done
}

// Stop asks the manager to stop the unit, which applies the unit's own
// TimeoutStopSec (the grace it was started with) between SIGTERM and
// SIGKILL across the whole cgroup, then waits for the watcher to
// observe the unit gone. grace here only sizes that wait: a reattached
// unit keeps the budget it was created with.
func (r *systemdRunner) Stop(h handle, grace time.Duration) error {
	sh, ok := h.(*systemdHandle)
	if !ok {
		return errors.New("not a systemd handle")
	}
	select {
	case <-sh.done:
		return nil
	default:
	}
	// The wait is sized from the unit's own TimeoutStopSec — a
	// reattached unit keeps the grace it was created with, whatever
	// the config says now — read fresh, else as recorded on the
	// handle, else the caller's grace. Callers hold deployMu for this
	// long, which is the operator's grace by construction.
	fallback := sh.stopTimeout
	if grace > fallback {
		fallback = grace
	}
	budget := r.stopBudgetFor(r.ctx, sh.unit, fallback)
	ctx, cancel := context.WithTimeout(r.ctx, budget+stopSlack)
	defer cancel()
	res, err := r.conn.StopUnit(ctx, sh.unit)
	if err != nil {
		return fmt.Errorf("stopping unit %s: %w", sh.unit, err)
	}
	if res != "done" {
		return fmt.Errorf("unit %s: stop job %s", sh.unit, res)
	}
	select {
	case <-sh.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("unit %s: stop job done but the unit was not observed gone within %s", sh.unit, budget+stopSlack)
	}
}

// Reattach adopts the unit recorded in state.json if the manager still
// runs it — and runs it sandboxed: a running unit without the
// namespaces is refused, since nothing this motel starts lacks them.
// Whatever happened to it while motel was away is logged; a failed
// unit is reset so it cannot linger, and the caller relaunches.
// A unit whose state cannot be read is an error: the caller must not
// launch beside a unit that may be running (invariant 3).
func (r *systemdRunner) Reattach(st handleState) (handle, bool, error) {
	if st.Unit == "" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, startJobTimeout)
	defer cancel()
	us, err := r.conn.UnitStatus(ctx, st.Unit)
	if err != nil {
		return nil, false, fmt.Errorf("reading recorded unit %s: %w", st.Unit, err)
	}
	if us.running() {
		if !us.Sandboxed {
			// Every unit motel starts is sandboxed, so this one was
			// not started by this motel: a development build that
			// still had `sandbox off`, say. It is running an app with
			// no sandbox, which nothing else would ever report. Not
			// adopted: the caller's relaunch sweeps this app's units
			// first, so it is stopped and the version relaunches
			// sandboxed. This catches an honest stale unit; it is not a
			// defence against a same-uid process that starts units on
			// purpose — such a process can start one with any property
			// set, including motel's own, and is already inside the
			// trust domain (DESIGN-threat-model.md, the shared-UID
			// rule), so no comparison here would keep it out.
			//
			// Stopped here, not left for the relaunch's sweep: the
			// relaunch prepares (dirs, socket, env) before it sweeps, and a
			// preparation that keeps failing — a missing env_file —
			// would keep this unit serving bare across every retry.
			// An unconfirmed stop is an error, so the caller does not
			// launch beside a unit that may still be running.
			msg := "recorded unit is running WITHOUT the sandbox; stopping it — the version relaunches sandboxed"
			reason := "was running without the sandbox"
			if !us.loaded() {
				// A daemon-reload could not read its file back: the
				// manager holds none of its settings, PrivatePIDs= among
				// them, and reports what it holds (#380).
				msg = "recorded unit's file no longer reads back, so the manager holds none of its settings; stopping it — the version relaunches"
				reason = "was running with a file the manager could not read back (" + us.LoadState + ")"
			}
			r.log().Error(msg, zap.String("unit", st.Unit), zap.Int("pid", us.MainPID), zap.String("load_state", us.LoadState))
			grace := us.StopTimeout
			if grace <= 0 {
				grace = defaultStopTimeout
			}
			stopCtx, stopCancel := context.WithTimeout(r.ctx, grace+stopSlack)
			defer stopCancel()
			bare := fmt.Errorf("recorded unit %s %s", st.Unit, reason)
			if err := r.stopUnobserved(stopCtx, st.Unit, bare); !errors.Is(err, bare) || errors.As(err, new(*unitUnconfirmedError)) {
				return nil, false, fmt.Errorf("stopping it: %w", err)
			}
			return nil, false, nil
		}
		h := &systemdHandle{unit: st.Unit, startedAt: st.StartedAt, stopTimeout: us.StopTimeout, done: make(chan struct{})}
		h.pid.Store(int64(us.MainPID))
		h.setArgv(us.ExecStart)
		// The caps the unit was started with, which a config changed
		// while motel was down may no longer be: syncCaps (app.go)
		// compares them with the spec's once the instance is current.
		caps := r.unitCaps(ctx, us)
		r.setCapsState(h, &caps, &us.ControlGroup, nil)
		go r.watch(h)
		return h, true, nil
	}
	if us.ActiveState == "failed" {
		r.log().Warn("recorded instance died while motel was down", zap.String("unit", st.Unit), zap.String("exit", us.exitString()), zap.String("result", us.Result))
		_ = r.resetFailed(ctx, st.Unit) // Sweep reports it if it stays loaded
	} else {
		r.log().Info("recorded unit is no longer running", zap.String("unit", st.Unit), zap.String("load_state", us.LoadState), zap.String("active_state", us.ActiveState))
	}
	return nil, false, nil
}

// Sweep stops every unit of app except keep (invariant 7). Failed
// leftovers are reset; running ones are stopped and confirmed gone.
// nil only when, afterwards, keep is the only loaded unit of app.
func (r *systemdRunner) Sweep(app string, keep handle) error {
	return r.sweep(r.ctx, app, keep, nil, true)
}

// StopAll is a sweep that keeps nothing and spares nothing.
func (r *systemdRunner) StopAll(app string) error {
	return r.sweep(r.ctx, app, nil, nil, false)
}

// sweep is Sweep under a parent context. Listing and each reset are
// bounded by pollTimeout. Strays are stopped concurrently, each under
// its own stop budget plus slack, so a sweep takes as long as its
// longest stray's TimeoutStopSec — never the sum, and never a flat cap
// that would report a slow-draining unit unconfirmed. still, when
// given, is consulted immediately before each stop; a false answer
// means someone now owns the app and that stray is left alone. spare
// leaves a running pre_start to finish and reports it (Sweep).
func (r *systemdRunner) sweep(parent context.Context, app string, keep handle, still func() bool, spare bool) error {
	keepUnit := ""
	if kh, ok := keep.(*systemdHandle); ok {
		keepUnit = kh.unit
	}
	listCtx, listCancel := context.WithTimeout(parent, pollTimeout)
	units, err := r.conn.ListUnits(listCtx, unitPrefix+app+".*.service")
	listCancel()
	if err != nil {
		return fmt.Errorf("listing units of %s: %w", app, err)
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, u := range units {
		if u.Name == keepUnit || !unitBelongsTo(u.Name, app) {
			continue
		}
		// The listing carries only the load/active/sub states; the
		// exit and PID the lines below record are read off the unit,
		// once per stray or leftover (both rare).
		if !u.running() {
			if u.leftover() {
				u = r.unitStatusOf(parent, u)
				r.log().Warn("resetting leftover unit", zap.String("unit", u.Name), zap.String("active_state", u.ActiveState), zap.String("exit", u.exitString()))
				ctx, cancel := context.WithTimeout(parent, pollTimeout)
				if rerr := r.resetFailed(ctx, u.Name); rerr != nil {
					record(rerr)
				}
				cancel()
			}
			continue
		}
		wg.Add(1)
		go func(u unitStatus) {
			defer wg.Done()
			// One read serves both the stop budget and the pid logged.
			// A stray that ended between the listing and this read is
			// a leftover now: reset if still loaded, never stopped — a
			// stop of a unit the manager has already unloaded fails,
			// and would report the whole sweep unconfirmed.
			u = r.unitStatusOf(parent, u)
			if !u.running() {
				if u.leftover() {
					r.log().Warn("resetting leftover unit", zap.String("unit", u.Name), zap.String("active_state", u.ActiveState), zap.String("exit", u.exitString()))
					ctx, cancel := context.WithTimeout(parent, pollTimeout)
					defer cancel()
					if rerr := r.resetFailed(ctx, u.Name); rerr != nil {
						record(rerr)
					}
				}
				return
			}
			if version, ok := unitPreStart(u.Name); ok && spare {
				r.log().Warn("leaving a running pre_start of an earlier deploy to end on its own", zap.String("unit", u.Name), zap.Int("pid", u.MainPID))
				record(&migrationRunningError{unit: u.Name, version: version})
				return
			}
			budget := u.stopBudget(0)
			if still != nil && !still() {
				r.log().Info("stray unit adopted by a config meanwhile; leaving it", zap.String("unit", u.Name))
				return
			}
			r.log().Warn("stopping stray unit not owned by this instance", zap.String("unit", u.Name), zap.Int("pid", u.MainPID))
			ctx, cancel := context.WithTimeout(parent, budget+stopSlack)
			defer cancel()
			if serr := r.stopUnobserved(ctx, u.Name, fmt.Errorf("stray unit %s", u.Name)); unitUnconfirmed(serr) {
				record(serr)
			}
		}(u)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// watch polls the unit until it is observed in a terminal state, then
// records the exit, resets a failed unit and closes done. Transport
// errors are logged once per outage and never end the watch.
func (r *systemdRunner) watch(h *systemdHandle) {
	t := time.NewTicker(r.poll)
	defer t.Stop()
	outage := false
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		// Each poll is bounded on its own: a manager that accepts the
		// request but never answers must not freeze the watch forever
		// (a crash would then never be observed).
		pollCtx, cancel := context.WithTimeout(r.ctx, pollTimeout)
		st, err := r.conn.UnitStatus(pollCtx, h.unit)
		cancel()
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			if !outage {
				outage = true
				r.log().Warn("unit state unavailable; instance is assumed running until the manager answers", zap.String("unit", h.unit), zap.Error(err))
			}
			continue
		}
		if outage {
			outage = false
			r.log().Info("unit state readable again", zap.String("unit", h.unit))
		}
		// Off every successful read, before the terminal branch: an app
		// that crashes before the first good poll would otherwise take
		// its command to the grave. A terminal snapshot still has it.
		h.setArgv(st.ExecStart)
		// The cgroup path too, for the same reason: a start whose status
		// read failed has none, and without it no later SetCaps can tell
		// which of its caps the host enforces. And the controllers, until
		// one reading of them has succeeded (a path whose file could not
		// be read leaves the handle claiming nothing) and while any cap
		// is listed as unenforced (a reading that failed right after the
		// write that enabled a controller would otherwise stand until
		// the next write): tried again each poll, a file read.
		listed := h.unenforced.Load()
		if (h.cgroup.Load() == nil || h.controllers.Load() == nil || listed != nil && len(*listed) > 0) && st.ControlGroup != "" {
			r.setCapsState(h, nil, &st.ControlGroup, nil)
		}
		if st.running() {
			// The handle follows the unit's MainPID: a read that failed
			// right after start is repaired here, and so is the pid of a
			// sandboxed unit — with PrivatePIDs= the manager forks an
			// intermediate that sets the namespace up and then reports
			// its child (the app, "supervising a process which is not
			// our child" in the journal) as MainPID a moment later.
			if st.MainPID != 0 && int64(st.MainPID) != h.pid.Load() {
				h.pid.Store(int64(st.MainPID))
			}
			continue
		}
		r.finish(h, st)
		return
	}
}

// finish is the single place a handle is declared dead.
func (r *systemdRunner) finish(h *systemdHandle, st unitStatus) {
	fields := []zap.Field{zap.String("unit", h.unit), zap.Int64("pid", h.pid.Load())}
	switch {
	case st.ActiveState == "failed":
		fields = append(fields, zap.String("exit", st.exitString()), zap.String("result", st.Result))
		r.log().Warn("instance exited", fields...)
		ctx, cancel := context.WithTimeout(r.ctx, stopSlack)
		_ = r.resetFailed(ctx, h.unit) // logged; the next Sweep reports a leftover
		cancel()
	case st.loaded():
		r.log().Info("instance exited", append(fields, zap.String("exit", st.exitString()))...)
	default:
		// A clean exit unloads a transient unit before we look again;
		// the exit status is then gone with it, and that is fine.
		r.log().Info("instance exited", fields...)
	}
	h.exit.Store(&st)
	close(h.done)
}

// unitStatusOf re-reads a listed unit in full, bounded by pollTimeout.
// What the caller already knows of the unit (a listing's states, or
// just its name) stands in when the manager cannot answer — logged,
// so an exit rendered off that ("no process exit recorded") is not
// mistaken for a unit that never ran.
func (r *systemdRunner) unitStatusOf(parent context.Context, listed unitStatus) unitStatus {
	ctx, cancel := context.WithTimeout(parent, pollTimeout)
	defer cancel()
	st, err := r.conn.UnitStatus(ctx, listed.Name)
	if err != nil {
		r.log().Warn("cannot read unit; acting on what is already known of it", zap.String("unit", listed.Name), zap.Error(err))
		return listed
	}
	st.Name = listed.Name
	return st
}

// stopBudgetFor is how long stopping unit may take before SIGKILL: its
// own TimeoutStopSec when readable, else the caller's grace, else the
// default — one policy for Stop and sweep alike.
func (r *systemdRunner) stopBudgetFor(parent context.Context, unit string, grace time.Duration) time.Duration {
	return r.unitStatusOf(parent, unitStatus{Name: unit}).stopBudget(grace)
}

// reapFailed reads a failed unit's exit facts and resets it.
func (r *systemdRunner) reapFailed(ctx context.Context, name string) unitStatus {
	st, err := r.conn.UnitStatus(ctx, name)
	if err != nil {
		r.log().Warn("cannot read failed unit", zap.String("unit", name), zap.Error(err))
	}
	if st.leftover() {
		_ = r.resetFailed(ctx, name)
	}
	return st
}

func (r *systemdRunner) resetFailed(ctx context.Context, name string) error {
	if err := r.conn.ResetFailedUnit(ctx, name); err != nil {
		r.log().Warn("resetting failed unit", zap.String("unit", name), zap.Error(err))
		return fmt.Errorf("resetting failed unit %s: %w", name, err)
	}
	return nil
}

var _ runner = (*systemdRunner)(nil)
