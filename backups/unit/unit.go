// Package unit runs one command to completion in a transient unit on
// the system manager, and reports how it ended.
//
// Everything about the unit travels as a typed D-Bus property. systemd
// parses none of it: a bind source holding a space, a colon, a percent
// sign or a dollar is a path and nothing else, and the command is sent
// as ExecStartEx with no-env-expand, so an argument spelled
// ${RESTIC_PASSWORD} reaches the process as those characters, whatever
// the unit's environment holds. (The plain ExecStart property expands
// it.)
//
// The unit is Type=oneshot, so its start job ends when the command
// does, and the job's result is the verdict: "done" is exit 0, and
// "failed" leaves the unit loaded in the failed state, where its exit
// status can be read before it is reset. Any other result is the unit
// being ended from outside, and is an error (reap). Nothing is read from a pipe,
// and nothing is ever stopped by a name pattern: a Runner stops the
// unit it started, by its exact name, and confirms it gone.
package unit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// Bind is one path put into the unit's view, which is otherwise empty.
type Bind struct {
	Source string
	// Dest is where the unit sees it; empty means at Source.
	Dest     string
	Writable bool
	// Optional binds are skipped when Source does not exist.
	Optional bool
}

// Spec is one unit. What a unit may reach is what its Spec names: a
// zero Spec has no network, no capability, no credential and an empty
// view. (SameUIDNamespaces is the one field whose zero value is the
// looser setting; it cannot be the default, because a unit with a
// capability over files cannot have it.)
type Spec struct {
	// Name is the unit's name, with its ".service". It matches nameRe,
	// which no liveswap app unit does.
	Name        string
	Description string
	// Argv is the command; Argv[0] is an absolute path. No element is
	// ever expanded.
	Argv []string

	// User is the account the command runs as. Leaving it empty is
	// refused unless AsRoot says so: nothing here runs as root by
	// omission. There is no DynamicUser=: with it the manager leaves
	// the whole host filesystem in the view, TemporaryFileSystem=/ or
	// not (TestIntegrationViewIsWhatIsNamed).
	User string
	// AsRoot runs the command as root, with exactly Capabilities.
	AsRoot bool
	// Capabilities is both the ambient and the bounding set.
	Capabilities []Capability

	// EnvironmentFile is read by the manager, as root, before it drops
	// to User; the Runner never opens it.
	EnvironmentFile string
	Environment     []string

	Binds []Bind
	// Masked paths exist in the view as empty files of mode 0: read
	// with CAP_DAC_READ_SEARCH they are empty, and without it they
	// cannot be opened. A path that is not there is skipped.
	Masked []string

	// Network leaves the unit in the host's network namespace.
	Network bool
	// SameUIDNamespaces puts the unit in its own user and PID
	// namespaces: what a unit running as an account that other
	// processes share needs, and what a unit holding a capability over
	// files cannot have (the capability would cover no file).
	SameUIDNamespaces bool

	// StdoutFile receives the command's stdout, truncated first. The
	// manager opens it, so it may be somewhere the command cannot
	// reach. Stderr goes to the journal, unless StderrFile names a file.
	StdoutFile string
	// StderrFile receives the command's stderr instead, truncated
	// first, for a command whose answer on stdout is only to be
	// believed when it had nothing to say beside it.
	StderrFile string
	// CacheDirectory is a name under /var/cache, made by the manager
	// and owned by User.
	CacheDirectory   string
	WorkingDirectory string

	// BindsTo names a unit whose end is this unit's end, however it
	// comes — the manager stops this unit itself.
	BindsTo string
}

// Capability is a Linux capability number.
type Capability uint

// The capabilities a backup unit is ever given.
const (
	CapChown         Capability = 0
	CapDACReadSearch Capability = 2
)

// nameRe is the shape of every unit name here. The underscores are the
// point: liveswap's app-name alphabet is [a-z0-9-], so no app, whatever
// it is called, has a unit of this shape.
var nameRe = regexp.MustCompile(`^motel_backup_[a-z0-9_-]{1,200}\.service$`)

// SyslogIdentifier is what every unit here writes its output under, for
// the same reason: liveswap's apps write under motel-<app>, and a person
// granted an app's logs is sent that app's identifier's lines
// (liveswap/motel_logs.go). Left to the manager, a unit's identifier is
// its command's name, motel-backup for the roles that run as the data
// user, and an app named backup would be sent what they write about
// every app. `journalctl -t motel_backup` shows every unit's.
const SyslogIdentifier = "motel_backup"

// baseView is the part of the OS a command needs in order to run at
// all; every entry is optional. It is liveswap's list (sandbox.go)
// without name resolution, which only units with Network get.
var (
	baseView    = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc/passwd", "/etc/group", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d", "/etc/alternatives", "/etc/localtime"}
	networkView = []string{"/etc/ssl/certs", "/etc/ssl/openssl.cnf", "/etc/ca-certificates", "/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/run/systemd/resolve"}
)

// Outcome is how a unit's command ended.
type Outcome struct {
	// ExitStatus is the command's exit status; 0 when the job result
	// was "done".
	ExitStatus int
	// Result is the manager's word for a unit that did not succeed:
	// "exit-code", "signal", "timeout", "oom-kill", …; "success"
	// otherwise.
	Result string
}

// OK reports a clean exit.
func (o Outcome) OK() bool { return o.Result == "success" && o.ExitStatus == 0 }

// conn is the part of go-systemd's connection a Runner uses.
type conn interface {
	StartTransientUnitContext(ctx context.Context, name, mode string, properties []sddbus.Property, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	GetAllPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	ResetFailedUnitContext(ctx context.Context, name string) error
	GetManagerProperty(prop string) (string, error)
	Close()
}

// Runner starts units on one manager connection.
type Runner struct {
	conn conn
	// stopWithin bounds stopping a unit and confirming it gone, on a
	// context of the Runner's own: the caller's is already cancelled by
	// the time it is needed.
	stopWithin time.Duration
	// lookEvery is how often a running unit's state is read, in case
	// the signal that its job ended never arrives.
	lookEvery time.Duration
}

// NewSystemRunner connects to the system manager. It needs root.
//
// The connection does not end with ctx: the library closes a connection
// when the context it was made with is cancelled, and a cancelled
// context — Ctrl-C — is exactly when the connection is needed, to stop
// the unit that is running and see it gone [measured: "use of closed
// network connection", and the unit ran on]. Close ends it.
func NewSystemRunner(ctx context.Context) (*Runner, error) {
	c, err := sddbus.NewSystemConnectionContext(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("connecting to the system manager: %w", err)
	}
	return &Runner{conn: c, stopWithin: 2 * time.Minute, lookEvery: 30 * time.Second}, nil
}

// Close releases the connection. Units are not stopped by it.
func (r *Runner) Close() { r.conn.Close() }

// Scheduled says of a unit of the manager's — a timer, here — whether
// it is running now and whether it is enabled, which is whether it
// will be after a reboot: what setup asks before it says what runs
// next. A oneshot whose command runs is "activating" to the manager
// and a ticking timer "active"; a name the manager never loaded is
// inactive and not enabled to it, not an error.
func (r *Runner) Scheduled(ctx context.Context, name string) (active, enabled bool, err error) {
	props, err := r.conn.GetAllPropertiesContext(ctx, name)
	if err != nil {
		return false, false, fmt.Errorf("asking the manager about %s: %w", name, err)
	}
	state := str(props["ActiveState"])
	return state == "active" || state == "activating", str(props["UnitFileState"]) == "enabled", nil
}

// seesWithin bounds the wait for the manager to have read a mount that
// was just made: it reads its mounts when the kernel tells it they
// changed, which is at once [M72], and not before it answers a call
// that reached it first.
var seesWithin = 5 * time.Second

// Sees says whether the manager sees a mount at mountPoint, which the
// caller has just made: the manager keeps a mount unit for every mount
// of its own mount namespace, and none for a mount made in another. A
// command the manager does not see the mounts of cannot have them
// bound into a unit.
func (r *Runner) Sees(ctx context.Context, mountPoint string) (bool, error) {
	name := MountUnit(mountPoint)
	until := time.Now().Add(seesWithin)
	// The bound holds for each question too: a manager that takes one
	// and never answers is given up on at the bound, and said.
	// Running out of it having had answers — "no such unit" — is "not
	// seen"; running out of it with none is a manager that did not
	// answer.
	bounded, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	answered := false
	for {
		props, err := r.conn.GetAllPropertiesContext(bounded, name)
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil && bounded.Err() != nil && answered:
			return false, nil
		case err != nil && bounded.Err() != nil:
			return false, fmt.Errorf("asking the manager about %s: it did not answer within %s", name, seesWithin)
		case err != nil && !isNoSuchUnit(err):
			return false, fmt.Errorf("asking the manager about %s: %w", name, err)
		case err == nil && str(props["ActiveState"]) == "active":
			return true, nil
		}
		answered = true
		if !time.Now().Before(until) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// MountUnit is the name of the manager's mount unit for a path, as
// systemd-escape --path --suffix=mount writes it: the path without its
// first and last slash, each slash a dash, and every byte that is not
// a letter, a digit, ":", "_" or "." — and a "." that comes first —
// as \x and its two hex digits. The root is "-".
func MountUnit(path string) string {
	path = strings.Trim(filepath.Clean(path), "/")
	if path == "" {
		return "-.mount"
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String() + ".mount"
}

// ManagerVersion is the major version of the manager the Runner is
// connected to: what setup checks before asking anyone for a secret,
// since every property here is systemd 257's (PrivatePIDs= above all).
// The property is a string the manager formats ("257.13-1~deb13u1");
// only its leading number is read.
func (r *Runner) ManagerVersion(context.Context) (int, error) {
	raw, err := r.conn.GetManagerProperty("Version")
	if err != nil {
		return 0, fmt.Errorf("asking the manager its version: %w", err)
	}
	digits := strings.TrimLeft(strings.Trim(raw, `"`), " ")
	if i := strings.IndexFunc(digits, func(c rune) bool { return c < '0' || c > '9' }); i >= 0 {
		digits = digits[:i]
	}
	if digits == "" {
		return 0, fmt.Errorf("the manager's version %q has no leading number", raw)
	}
	return strconv.Atoi(digits)
}

// ErrEndedFromOutside is a unit ended before its command finished by
// something other than its command: stopped by someone, or by the
// manager because what it is bound to ended. The command gave no
// verdict.
var ErrEndedFromOutside = errors.New("ended from outside before its command finished")

// ErrNotConfirmedGone wraps the cause when a unit could not be stopped
// and observed gone: the one case in which something may still be
// running after Run returns.
var ErrNotConfirmedGone = errors.New("the unit could not be confirmed gone")

// Run starts the unit and waits for its command to end. The error is
// about running the unit, never about the command: a command that
// exits 12 is an Outcome. When ctx ends first the unit is stopped, by
// name, and Run returns ctx's error once it is observed gone.
func (r *Runner) Run(ctx context.Context, s Spec) (Outcome, error) {
	props, err := s.properties()
	if err != nil {
		return Outcome{}, err
	}
	job := make(chan string, 1)
	if _, err := r.conn.StartTransientUnitContext(ctx, s.Name, "fail", props, job); err != nil {
		// The request may have reached the manager all the same.
		return Outcome{}, r.stop(s.Name, fmt.Errorf("starting %s: %w", s.Name, err))
	}
	// The job's end arrives as a signal, on a connection of its own; a
	// signal can be lost, and nothing here has a time limit to notice
	// by. So the unit's state is also looked at now and then: a unit
	// that has ended with no word of its job is reaped as "failed" or
	// taken as done by what the manager says of it.
	look := time.NewTicker(r.lookEvery)
	defer look.Stop()
	unreadable := 0
	for {
		select {
		case res := <-job:
			if res == "done" {
				return Outcome{Result: "success"}, nil
			}
			return r.reap(s.Name, res)
		case <-ctx.Done():
			return Outcome{}, r.stop(s.Name, ctx.Err())
		case <-look.C:
			p, err := r.conn.GetAllPropertiesContext(ctx, s.Name)
			if err != nil {
				// Now and then is a manager busy; ten looks running is a
				// unit that cannot be watched, and waiting on it for ever
				// would hold the run's lock for ever. It is stopped, by
				// name, and that is the error.
				if unreadable++; unreadable >= 10 {
					return Outcome{}, r.stop(s.Name, fmt.Errorf("%s: its state could not be read %d times running: %w", s.Name, unreadable, err))
				}
				continue
			}
			unreadable = 0
			if str(p["LoadState"]) == "not-found" {
				// Gone, and not as failed — a failed unit stays loaded
				// until it is reset — so its command ended cleanly.
				return Outcome{Result: "success"}, nil
			}
			switch str(p["ActiveState"]) {
			case "failed":
				return r.reap(s.Name, "failed")
			case "inactive":
				if str(p["Result"]) == "success" {
					return Outcome{Result: "success"}, nil
				}
				return r.reap(s.Name, "failed")
			}
		}
	}
}

// Wait waits for a unit by its exact name to end on its own — a unit an
// earlier command left to finish — and reaps it; a unit that does not
// exist has ended. It looks every second for as long as ctx allows.
func (r *Runner) Wait(ctx context.Context, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("unit name %q does not match %s", name, nameRe)
	}
	unreadable := 0
	for {
		p, err := r.conn.GetAllPropertiesContext(ctx, name)
		if err != nil {
			if isNoSuchUnit(err) {
				return nil
			}
			// Now and then is a manager busy, as Run allows for.
			if unreadable++; unreadable >= 10 {
				return fmt.Errorf("%s: its state could not be read %d times: %w", name, unreadable, err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: its state could not be read: %w", name, ctx.Err())
			case <-time.After(time.Second):
			}
			continue
		}
		unreadable = 0
		if str(p["LoadState"]) == "not-found" {
			return nil
		}
		switch str(p["ActiveState"]) {
		case "inactive", "failed":
			_ = r.conn.ResetFailedUnitContext(ctx, name)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: still %s: %w", name, str(p["ActiveState"]), ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Stop stops a unit by its exact name and confirms it gone; a unit that
// does not exist is already gone. It is how a run ends what an earlier
// run — killed before it could — left behind.
func (r *Runner) Stop(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("unit name %q does not match %s", name, nameRe)
	}
	return r.stop(name, nil)
}

// reap reads how a unit whose start job did not end "done" finished,
// and resets it so the name does not linger as failed.
//
// Only a job that ended "failed" is the command's own doing. Any other
// result — "dependency" when the unit it is bound to died, "canceled"
// when someone stopped it — ends the job before the manager has
// finished killing the process, so the unit is first seen to its end
// (stopped, if it has to be), and what comes back is an error: the
// command gave no verdict.
func (r *Runner) reap(name, jobResult string) (Outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.stopWithin)
	defer cancel()
	var p map[string]any
	for {
		var err error
		if p, err = r.conn.GetAllPropertiesContext(ctx, name); err != nil {
			// Not knowing how it ended is not knowing that it ended.
			return Outcome{}, r.stop(name, fmt.Errorf("%s: start job ended %q and its status could not be read: %w", name, jobResult, err))
		}
		if st := str(p["ActiveState"]); st == "inactive" || st == "failed" {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return Outcome{}, r.stop(name, fmt.Errorf("%s: start job ended %q and the unit is still %s", name, jobResult, str(p["ActiveState"])))
		}
	}
	out := Outcome{Result: str(p["Result"]), ExitStatus: integer(p["ExecMainStatus"])}
	_ = r.conn.ResetFailedUnitContext(ctx, name)
	if jobResult != "failed" {
		return out, fmt.Errorf("%s: %w (start job %q, unit result %q)", name, ErrEndedFromOutside, jobResult, out.Result)
	}
	if out.Result == "" || out.Result == "success" {
		return out, fmt.Errorf("%s: start job failed and the unit records no failure", name)
	}
	// A unit the manager could not set up — no such user (217), a
	// namespace it could not build (226) — ends here too, as
	// "exit-code" with a status from 200 up: an Outcome like any other,
	// for the caller to put into words.
	return out, nil
}

// stop stops the unit by name and returns cause once it is observed
// gone, or ErrNotConfirmedGone wrapping cause.
func (r *Runner) stop(name string, cause error) error {
	if cause == nil {
		cause = errNone
	}
	err := r.stopCause(name, cause)
	if errors.Is(err, errNone) && !errors.Is(err, ErrNotConfirmedGone) {
		return nil
	}
	return err
}

// errNone stands in for "no cause" so stopCause can always wrap one.
var errNone = errors.New("stopped on request")

func (r *Runner) stopCause(name string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.stopWithin)
	defer cancel()
	job := make(chan string, 1)
	if _, err := r.conn.StopUnitContext(ctx, name, "replace", job); err != nil {
		if isNoSuchUnit(err) {
			return cause
		}
		return fmt.Errorf("%w: %s: %w (stopping it: %w)", ErrNotConfirmedGone, name, cause, err)
	}
	select {
	case <-job:
	case <-ctx.Done():
		return fmt.Errorf("%w: %s: %w (the stop job did not finish)", ErrNotConfirmedGone, name, cause)
	}
	p, err := r.conn.GetAllPropertiesContext(ctx, name)
	if err != nil {
		if isNoSuchUnit(err) {
			return cause
		}
		return fmt.Errorf("%w: %s: %w (reading its state: %w)", ErrNotConfirmedGone, name, cause, err)
	}
	switch str(p["ActiveState"]) {
	case "inactive", "failed":
		_ = r.conn.ResetFailedUnitContext(ctx, name)
		return cause
	}
	return fmt.Errorf("%w: %s: %w (it is %s)", ErrNotConfirmedGone, name, cause, str(p["ActiveState"]))
}

func isNoSuchUnit(err error) bool {
	var e godbus.Error
	return errors.As(err, &e) && e.Name == "org.freedesktop.systemd1.NoSuchUnit"
}

func str(v any) string { s, _ := v.(string); return s }

func integer(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case uint32:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}

// D-Bus shapes, as systemd's own bus-unit-util renders the unit-file
// syntax.
type (
	execCommand struct {
		Path  string
		Argv  []string
		Flags []string
	}
	bindMount struct {
		Source, Destination string
		IgnoreENOENT        bool
		Flags               uint64
	}
	tmpfsMount struct{ Path, Options string }
	envFile    struct {
		Path          string
		IgnoreMissing bool
	}
	// syscallFilter is SystemCallFilter= over the bus: (bas), an
	// allow-list flag and the names. The unit-file spelling ~name is the
	// manager's own rendering of (false, [name]) [measured: sent as a
	// string array, the manager refuses the unit outright — "Unexpected
	// message contents"].
	syscallFilter struct {
		AllowList bool
		Names     []string
	}
)

const mountRecursive uint64 = 0x4000 // MS_REC

func v(x any) godbus.Variant { return godbus.MakeVariant(x) }

// properties renders the Spec, refusing one that is not fully said.
func (s Spec) properties() ([]sddbus.Property, error) {
	if !nameRe.MatchString(s.Name) {
		return nil, fmt.Errorf("unit name %q does not match %s", s.Name, nameRe)
	}
	if len(s.Argv) == 0 || !strings.HasPrefix(s.Argv[0], "/") {
		return nil, fmt.Errorf("%s: the command must be an absolute path, got %q", s.Name, s.Argv)
	}
	if (s.User != "") == s.AsRoot {
		return nil, fmt.Errorf("%s: exactly one of User and AsRoot says who the command runs as", s.Name)
	}
	if s.SameUIDNamespaces && len(s.Capabilities) > 0 {
		return nil, fmt.Errorf("%s: a capability over files means nothing inside a user namespace, where every other owner is unmapped", s.Name)
	}

	var caps uint64
	dacRead := false
	for _, c := range s.Capabilities {
		caps |= 1 << c
		dacRead = dacRead || c == CapDACReadSearch
	}
	props := []sddbus.Property{
		{Name: "Description", Value: v(s.Description)},
		{Name: "Type", Value: v("oneshot")},
		// The start job is the wait, however long the command takes.
		{Name: "TimeoutStartUSec", Value: v(uint64(1<<64 - 1))},
		{Name: "ExecStartEx", Value: v([]execCommand{{Path: s.Argv[0], Argv: s.Argv, Flags: []string{"no-env-expand"}}})},
		{Name: "KillMode", Value: v("control-group")},
		{Name: "SyslogIdentifier", Value: v(SyslogIdentifier)},

		{Name: "TemporaryFileSystem", Value: v([]tmpfsMount{{"/", "ro"}})},
		{Name: "PrivateTmp", Value: v(true)},
		{Name: "PrivateDevices", Value: v(true)},
		{Name: "PrivateNetwork", Value: v(!s.Network)},
		{Name: "NoNewPrivileges", Value: v(true)},
		{Name: "CapabilityBoundingSet", Value: v(caps)},
		{Name: "AmbientCapabilities", Value: v(caps)},
		{Name: "ProtectControlGroups", Value: v(true)},
		{Name: "ProtectKernelTunables", Value: v(true)},
		{Name: "ProtectKernelModules", Value: v(true)},
		{Name: "ProtectKernelLogs", Value: v(true)},
		{Name: "RestrictNamespaces", Value: v(uint64(0))},
		{Name: "RestrictRealtime", Value: v(true)},
		{Name: "RestrictSUIDSGID", Value: v(true)},
		{Name: "LockPersonality", Value: v(true)},
		{Name: "UMask", Value: v(uint32(0o077))},
	}
	if s.StderrFile != "" {
		props = append(props, sddbus.Property{Name: "StandardErrorFileToTruncate", Value: v(s.StderrFile)})
	} else {
		props = append(props, sddbus.Property{Name: "StandardError", Value: v("journal")})
	}
	if s.User != "" {
		props = append(props, sddbus.Property{Name: "User", Value: v(s.User)})
	}
	if s.SameUIDNamespaces {
		props = append(props,
			sddbus.Property{Name: "PrivateUsers", Value: v(true)},
			sddbus.Property{Name: "PrivatePIDs", Value: v("yes")})
	}
	if s.EnvironmentFile != "" {
		props = append(props, sddbus.Property{Name: "EnvironmentFiles", Value: v([]envFile{{s.EnvironmentFile, false}})})
	}
	if len(s.Environment) > 0 {
		props = append(props, sddbus.Property{Name: "Environment", Value: v(s.Environment)})
	}
	if s.StdoutFile != "" {
		props = append(props, sddbus.Property{Name: "StandardOutputFileToTruncate", Value: v(s.StdoutFile)})
	}
	if s.CacheDirectory != "" {
		props = append(props, sddbus.Property{Name: "CacheDirectory", Value: v([]string{s.CacheDirectory})})
	}
	if s.WorkingDirectory != "" {
		props = append(props, sddbus.Property{Name: "WorkingDirectory", Value: v(s.WorkingDirectory)})
	}
	if s.BindsTo != "" {
		// Not After= as well: a unit ordered after a oneshot that is
		// still activating never starts.
		props = append(props, sddbus.Property{Name: "BindsTo", Value: v([]string{s.BindsTo})})
	}

	var ro, rw []bindMount
	view := baseView
	if s.Network {
		view = append(append([]string{}, baseView...), networkView...)
	}
	for _, p := range view {
		ro = append(ro, bindMount{p, p, true, mountRecursive})
	}
	for _, b := range s.Binds {
		if !strings.HasPrefix(b.Source, "/") || (b.Dest != "" && !strings.HasPrefix(b.Dest, "/")) {
			return nil, fmt.Errorf("%s: bind %q -> %q: both paths are absolute", s.Name, b.Source, b.Dest)
		}
		m := bindMount{b.Source, b.Dest, b.Optional, mountRecursive}
		if m.Destination == "" {
			m.Destination = m.Source
		}
		if b.Writable {
			rw = append(rw, m)
		} else {
			ro = append(ro, m)
		}
	}
	// One list in the manager behind two properties: an empty array
	// resets both, so an empty one is never sent.
	props = append(props, sddbus.Property{Name: "BindReadOnlyPaths", Value: v(ro)})
	if len(rw) > 0 {
		props = append(props, sddbus.Property{Name: "BindPaths", Value: v(rw)})
	}
	// CAP_DAC_READ_SEARCH is what lets open_by_handle_at reach a file by
	// its inode without a path — outside the unit's view, on any mounted
	// filesystem (CVE-2014-5277, "shocker") [measured: the filter blocks
	// it and leaves restic and chown at exit 0]. A denylist of that one
	// call, returning EPERM rather than a SIGSYS kill, and nothing else,
	// so every other syscall is still allowed.
	if dacRead {
		props = append(props,
			sddbus.Property{Name: "SystemCallFilter", Value: v(syscallFilter{false, []string{"open_by_handle_at"}})},
			sddbus.Property{Name: "SystemCallErrorNumber", Value: v(int32(unix.EPERM))})
	}
	if len(s.Masked) > 0 {
		masked := make([]string, len(s.Masked))
		for i, p := range s.Masked {
			if !strings.HasPrefix(p, "/") {
				return nil, fmt.Errorf("%s: masked path %q is not absolute", s.Name, p)
			}
			masked[i] = "-" + p
		}
		props = append(props, sddbus.Property{Name: "InaccessiblePaths", Value: v(masked)})
	}
	return props, nil
}
