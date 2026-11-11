package liveswap

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// runner abstracts how app instances are executed so the orchestration
// pipeline in app.go is testable with a fake. The one production
// implementation is systemdRunner (runner_systemd.go): every instance
// is a transient systemd service unit under the motel user's own
// service manager, which is what lets apps outlive motel restarts.
// exitError is the runner's report of a unit that ran and failed: how
// its process ended, in which unit, with what job, and the pid the
// manager recorded for that process (0 where it recorded none). The
// deploy's failure detail reads the exit, and finds the process's
// lines in the journal by the pid; the text is what each error always
// said — RunOnce's "exit status 3 (unit X: job failed)", Start's
// "unit X: start job failed (exit status 3)" — so nothing parsing the
// response's top-level error sees a change.
type exitError struct {
	exit, unit, job string
	msg             string
	pid             int
}

func (e *exitError) Error() string { return e.msg }

// runOnceExit is RunOnce's exitError, in RunOnce's words.
func runOnceExit(exit, unit, res string, pid int) *exitError {
	return &exitError{exit: exit, unit: unit, job: "job " + res, msg: fmt.Sprintf("%s (unit %s: job %s)", exit, unit, res), pid: pid}
}

// startExit is Start's exitError for a failed start job, in Start's words.
func startExit(exit, unit, res string, pid int) *exitError {
	return &exitError{exit: exit, unit: unit, job: "start job " + res, msg: fmt.Sprintf("unit %s: start job %s (%s)", unit, res, exit), pid: pid}
}

type runner interface {
	// Preflight checks a launch before any unit exists: the command
	// resolves to a file the unit will see inside its sandbox view,
	// and an executable built for another machine is refused with the
	// fix named. A deploy runs it for the command and the pre_start,
	// and reports a refusal as the deployer's error (422); every
	// relaunch runs it for the command (launchVersion), and gives up on
	// a refusal.
	Preflight(spec startSpec) error

	// Start launches a long-running instance and returns immediately.
	Start(spec startSpec) (handle, error)

	// RunOnce runs a command to completion (pre_start hooks). A non-nil
	// error includes non-zero exits; ctx cancellation kills the process.
	RunOnce(ctx context.Context, spec startSpec) error

	// Alive reports whether the instance is still running.
	Alive(h handle) bool

	// Exit is how the instance's main process ended ("exit status 3",
	// "killed by signal 9 (killed)"), or "" while it runs. Non-empty
	// whenever Alive is false: the runner records the exit before it
	// declares the handle dead (finish, runner_systemd.go). With it,
	// the main process's pid — as the manager recorded it once the
	// process has ended, the one the handle follows while it runs; 0
	// when neither is known: what the process's journal lines carry.
	Exit(h handle) (string, int)

	// Wait returns a channel that is closed once the instance exits.
	// It may return nil when the runner cannot wait on this handle;
	// callers must then fall back to polling Alive.
	Wait(h handle) <-chan struct{}

	// Stop terminates gracefully: SIGTERM, wait up to the grace the
	// instance was started with, then SIGKILL — delivered to the whole
	// process tree. Returns nil only once the instance is confirmed
	// gone; any error means the caller must assume it may still run.
	Stop(h handle, grace time.Duration) error

	// SetCaps gives a running instance's unit the caps, replacing what
	// it had: a cap now unset is lifted. nil once the manager has
	// applied them, from when the handle's state reports them.
	SetCaps(h handle, caps resourceCaps) error

	// Reattach tries to re-adopt an instance from persisted state after
	// a motel restart: the unit named in st is still running under
	// the service manager. (nil, false, nil) means it was observed not
	// running and the caller may launch afresh; a non-nil error means
	// its state could not be read, and the caller must NOT launch (the
	// unit may well be running) — retry instead.
	Reattach(st handleState) (handle, bool, error)

	// Sweep stops every instance of app other than keep (nil: all of
	// them). The service manager, not state.json, is the ledger of what
	// is running: callers sweep before trusting their own view of an
	// app (recovery) and before deleting any release (GC). nil means
	// keep is now the only unit of the app; any error means something
	// may still be running and nothing may be deleted. A running
	// pre_start is left to finish, never stopped: the sweep reports it
	// (migrationRunningError) and is unconfirmed until it has ended.
	Sweep(app string, keep handle) error

	// StopAll stops every unit of app, a running pre_start included:
	// the app is going (a reload removed it), so nothing will wait for
	// the migration's end. nil means no unit of the app is left.
	StopAll(app string) error
}

// migrationRunningError is a sweep that found a pre_start still
// running — an earlier deploy's, since a deploy in flight holds the
// lock every sweeping caller takes: one a motel stop cut short, or
// one whose start and stop this motel could not observe. Stopping it would
// half-apply a migration, so the sweep leaves it to end on its own
// clock (TimeoutStartSec=) and reports itself unconfirmed, naming it.
type migrationRunningError struct {
	unit    string
	version string
}

func (e *migrationRunningError) Error() string {
	return "pre_start of " + e.version + " (" + e.unit + ") is still running from an earlier deploy; waiting for it to end"
}

// unitUnconfirmedError reports that a runner operation could not
// establish whether the named unit is still running (the request may
// have reached the manager before the transport failed). Callers must
// treat the unit as possibly alive: keep its release on disk, skip GC.
type unitUnconfirmedError struct {
	unit string
	err  error
}

func (e *unitUnconfirmedError) Error() string {
	return "unit " + e.unit + " may still be running: " + e.err.Error()
}

func (e *unitUnconfirmedError) Unwrap() error { return e.err }

// unitUnconfirmed reports whether err says a unit may still be running.
func unitUnconfirmed(err error) bool {
	var u *unitUnconfirmedError
	return errors.As(err, &u)
}

// startSpec is everything a runner needs to launch one command.
type startSpec struct {
	app     string        // app name (unit naming, journal identifier)
	version string        // version tag (unit naming, description)
	nonce   string        // instance id (newNonce): names the unit, and the socket the app binds
	command []string      // argv; command[0] looked up in PATH unless it contains a slash
	dir     string        // working directory (the release dir)
	env     []string      // complete environment, KEY=VALUE form
	grace   time.Duration // SIGTERM→SIGKILL budget applied when the instance is stopped
	// startTimeout bounds a pre_start on the unit itself
	// (TimeoutStartSec=), so it still ends on time when the motel
	// that started it has stopped; 0 sets none.
	startTimeout time.Duration
	// sandbox is the per-unit sandbox to apply. Every unit has one;
	// the runner refuses a spec without it rather than running the
	// command with the floor only (unitFor).
	sandbox *sandboxSpec
	// probe marks the throwaway unit the sandbox capability probe runs.
	// It is named outside the app grammar (see unitName) so no sweep
	// can mistake it for an app's unit — and so a configured app can
	// never collide with it.
	probe bool
	// caps bounds the unit (resourceCaps); the zero value sets none.
	caps resourceCaps
}

// resourceCaps is what an app's units are bounded to: the three cgroup
// caps the Caddyfile sets per app (memory_max, tasks_max, cpu_quota),
// zero meaning none. Every unit of the app gets them at its start —
// the instance and the pre_start alike (capProperties,
// systemd_dbus.go) — and a running instance gets a reload's changed
// values through SetCaps rather than at its next launch. The sandbox's
// read-only cgroupfs is what makes them the app's ceiling rather than
// a suggestion: the user manager's subtree is owned by the app's own
// uid, so without it an app could rewrite its own memory.max.
//
// Measured on Debian 13 (systemd 257), Ubuntu 24.04 (255) and 26.04
// (259): the user manager is delegated exactly the cpu, memory and
// pids controllers, so these three are the caps a user manager can
// set, and each reads back from the kernel at the value given.
type resourceCaps struct {
	// memoryMax is the RAM the unit's whole tree may use, in bytes. At
	// it the kernel kills the process that asked (the biggest, when it
	// is the tree's), the unit fails with Result=oom-kill and the
	// watchdog restarts the app. The unit gets no swap under it
	// (MemorySwapMax=0): a cap an app could spill past into swap would
	// bound nothing.
	memoryMax uint64
	// tasksMax is the threads and processes of the unit's whole tree.
	// Past it a fork or thread creation fails with EAGAIN; a `sh -c`
	// wrapped command dies on the first one (dash exits 2), which the
	// watchdog reads as a crash. An unset value leaves the user
	// manager's own default, 15% of the kernel's threads-max.
	tasksMax int
	// cpuQuota is CPU time in percent of one CPU: 200 is two CPUs'
	// worth. Past it the unit waits for the next period; nothing is
	// killed. Unset is no bound beyond the fair share every unit has.
	cpuQuota int
}

// any reports whether one cap is set.
func (c resourceCaps) any() bool { return c.memoryMax > 0 || c.tasksMax > 0 || c.cpuQuota > 0 }

// handle identifies a running instance to its runner.
type handle interface {
	// state returns the serializable form persisted in state.json so a
	// runner can attempt Reattach after a restart.
	state() handleState
}

// handleState is what a runner reports about a running instance —
// read live by status and the watchdog, and partly persisted. Only
// Unit and StartedAt reach state.json: they are all Reattach needs,
// and the manager is the ledger for the rest (DESIGN-liveswap.md).
// Everything else here is json:"-" on purpose.
type handleState struct {
	// PID is the unit's main process as last read. Not persisted: the
	// watcher corrects it in the live handle, never in the file.
	PID int `json:"-"`
	// Command is the argv the manager reports. Not persisted: it shows
	// what is running, and must never be what decides a relaunch.
	Command   []string  `json:"-"`
	StartedAt time.Time `json:"started_at,omitempty"`
	// Unit is the transient systemd unit running the instance; it is
	// what Reattach looks up after a motel restart.
	Unit string `json:"unit,omitempty"`
	// Caps is what the unit is bounded to, as the manager last applied
	// them: the start's, a SetCaps's, or the values read at reattach.
	// Not persisted: the manager is the ledger, and status reads this.
	Caps resourceCaps `json:"-"`
	// CapsUnknown says Caps is the last known, not the applied: a
	// SetCaps failed after the manager may have applied it and the
	// unit could not be read back. A sync re-sends rather than trusts
	// an equality, and status reports no caps as in effect.
	CapsUnknown bool `json:"-"`
	// CapsUnenforced names the caps in Caps whose controller does not
	// reach the unit's cgroup (unenforcedCaps): the manager holds them,
	// the kernel bounds nothing. Status lists them apart from the caps
	// in effect.
	CapsUnenforced []string `json:"-"`
	// Cgroup is the unit's cgroup path under /sys/fs/cgroup, as the
	// manager last reported it; "" until it has. Not persisted: the
	// metrics store reads the unit's numbers from it.
	Cgroup string `json:"-"`
}
