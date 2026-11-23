package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeSystemdConn scripts the manager: it records every unit it is
// asked to start/stop/reset and serves whatever status a test sets.
type fakeSystemdConn struct {
	mu          sync.Mutex
	started     []unitSpec
	stopped     []string
	reset       []string
	status      map[string]unitStatus
	statusErr   error  // UnitStatus returns this while set
	startResult string // job result for Start ("" = done)
	startErr    error
	// failStatus is what UnitStatus shows for a unit whose start job
	// did not return "done" (the failed unit the manager keeps loaded).
	failStatus *unitStatus
	blockStart bool // StartTransientUnit waits for ctx (oneshot cancellation)
	// startStatus is the status a newly started (non-oneshot) unit gets;
	// nil means the default active/4242. A test that needs MainPID to
	// be unpublished at adopt time sets one with MainPID 0 — it cannot
	// preset f.status, because the unit name carries a random nonce.
	startStatus *unitStatus
	stopResult  string
	stopErr     error
	stopLeaves  bool // StopUnit does not mark the unit gone
	listErr     error
	resetErr    error
	listCalls   int
	listHook    func() // runs inside ListUnits, before it returns (interleaving tests)
	// vanishOnRead names units that disappear on their next UnitStatus
	// read: listed, then gone by the time the reader looks.
	vanishOnRead map[string]bool
	stopDelay    time.Duration // StopUnit sleeps this long (concurrency tests)
	// setProps records every SetUnitProperties call by unit, in order.
	setProps    map[string][][]sddbus.Property
	setPropsErr error // SetUnitProperties returns this
	// defaultTasksMax is what DefaultTasksMax reports; 0 reads as the
	// manager's default being unreadable (defaultTasksMaxErr, or a
	// generic error when that is nil).
	defaultTasksMax    uint64
	defaultTasksMaxErr error

	// env is the environment UnitStatusAndEnvironment reports per unit.
	env map[string][]string
}

func (f *fakeSystemdConn) DefaultTasksMax(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.defaultTasksMaxErr != nil {
		return 0, f.defaultTasksMaxErr
	}
	if f.defaultTasksMax == 0 {
		return 0, errors.New("fake: no DefaultTasksMax set")
	}
	return f.defaultTasksMax, nil
}

func newFakeSystemdConn() *fakeSystemdConn {
	return &fakeSystemdConn{status: map[string]unitStatus{}}
}

func (f *fakeSystemdConn) SetUnitProperties(_ context.Context, name string, props []sddbus.Property) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setPropsErr != nil {
		return f.setPropsErr
	}
	if f.setProps == nil {
		f.setProps = map[string][][]sddbus.Property{}
	}
	f.setProps[name] = append(f.setProps[name], props)
	return nil
}

func (f *fakeSystemdConn) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	f.mu.Lock()
	f.started = append(f.started, u)
	blocking, startErr, res := f.blockStart, f.startErr, f.startResult
	if res == "" {
		res = "done"
	}
	switch {
	case (startErr != nil || res != "done") && f.failStatus != nil:
		f.status[u.Name] = *f.failStatus
	case startErr != nil:
	case !u.Oneshot:
		if _, preset := f.status[u.Name]; !preset {
			st := unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 4242, Sandboxed: true}
			if f.startStatus != nil {
				st = *f.startStatus
			}
			// The manager reports back the ExecStart it was handed,
			// whatever else the unit's status says — which is what
			// makes reading the command back from it faithful.
			st.ExecStart = u.ExecStart
			f.status[u.Name] = st
		}
	}
	f.mu.Unlock()
	if startErr != nil {
		return "", startErr
	}
	if blocking {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return res, nil
}

func (f *fakeSystemdConn) StopUnit(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	delay := f.stopDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	if f.stopErr != nil {
		return "", f.stopErr
	}
	if f.stopResult != "" && f.stopResult != "done" {
		return f.stopResult, nil
	}
	if !f.stopLeaves {
		delete(f.status, name)
	}
	return "done", nil
}

func (f *fakeSystemdConn) UnitStatus(_ context.Context, name string) (unitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return unitStatus{}, f.statusErr
	}
	if f.vanishOnRead[name] {
		// The unit ended and was unloaded between a listing and this
		// read: gone from here on.
		delete(f.status, name)
		delete(f.vanishOnRead, name)
	}
	st, ok := f.status[name]
	if !ok {
		return unitStatus{LoadState: "not-found"}, nil
	}
	return st, nil
}

func (f *fakeSystemdConn) UnitStatusAndEnvironment(ctx context.Context, name string) (unitStatus, []string, error) {
	st, err := f.UnitStatus(ctx, name)
	if err != nil || st.LoadState == "not-found" {
		return st, nil, err // as the real client: a unit not known has none
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return st, append([]string(nil), f.env[name]...), nil
}

func (f *fakeSystemdConn) ResetFailedUnit(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = append(f.reset, name)
	if f.resetErr != nil {
		return f.resetErr
	}
	delete(f.status, name)
	return nil
}

func (f *fakeSystemdConn) ListUnits(_ context.Context, pattern string) ([]unitStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listHook != nil {
		f.listHook()
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []unitStatus
	for name, st := range f.status {
		if ok, _ := path.Match(pattern, name); ok {
			// As the manager's listing: the states only. Exit facts,
			// PID and argv are on the unit (UnitStatus).
			out = append(out, unitStatus{Name: name, LoadState: st.LoadState, ActiveState: st.ActiveState, SubState: st.SubState})
		}
	}
	return out, nil
}

func (f *fakeSystemdConn) setStatus(name string, st unitStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[name] = st
}

func (f *fakeSystemdConn) setStatusErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErr = err
}

func (f *fakeSystemdConn) unit(i int) unitSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[i]
}

func (f *fakeSystemdConn) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func (f *fakeSystemdConn) resets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reset...)
}

var failedStatus = unitStatus{LoadState: "loaded", ActiveState: "failed", SubState: "failed", Result: "exit-code", ExecMainCode: 1, ExecMainStatus: 3}

func newTestSystemdRunner(t *testing.T) (*systemdRunner, *fakeSystemdConn) {
	t.Helper()
	conn := newFakeSystemdConn()
	r := newSystemdRunner(conn, zap.NewNop())
	r.poll = time.Millisecond
	// The fake never moves MainPID, so the settle wait every start now
	// pays would run its whole window out; the two settle tests set it
	// back to the real one.
	r.settle = 5 * time.Millisecond
	t.Cleanup(r.close)
	return r, conn
}

// testApp writes an executable ./server into a temp release dir and
// returns a startSpec for it, the way app.go would build one.
func testApp(t *testing.T) startSpec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return startSpec{
		app:     "demo",
		version: "v1.2",
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server", "--flag"},
		dir:     dir,
		env:     []string{"SOCKET=/var/lib/liveswap/demo/run/0a1b2c3d0a1b2c3d.sock"},
		grace:   3 * time.Second,
		// Every unit is sandboxed: the release dir is a writable bind,
		// which is also what puts ./server inside the unit's view.
		sandbox: &sandboxSpec{root: "/var/lib/liveswap", writable: []bindPath{{dest: dir, source: dir}}},
	}
}

func waitDone(t *testing.T, r *systemdRunner, h handle) {
	t.Helper()
	select {
	case <-r.Wait(h):
	case <-time.After(2 * time.Second):
		t.Fatal("instance never reported exited")
	}
}

var unitNamePattern = regexp.MustCompile(`^motel-demo\.v1\.2\.0a1b2c3d0a1b2c3d\.service$`)

func TestSystemdRunnerStartBuildsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	u := conn.unit(0)
	if !unitNamePattern.MatchString(u.Name) {
		t.Fatalf("unit name %q", u.Name)
	}
	if u.Oneshot {
		t.Fatal("app units are simple services, not oneshot")
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(spec.dir, "server"))
	got, _ := filepath.EvalSymlinks(u.ExecStart[0])
	if !filepath.IsAbs(u.ExecStart[0]) || got != want || u.ExecStart[1] != "--flag" {
		t.Fatalf("ExecStart must be the absolute resolved command, got %v", u.ExecStart)
	}
	if u.WorkingDirectory != spec.dir || u.SyslogIdentifier != "motel-demo" || u.StopTimeout != 3*time.Second {
		t.Fatalf("unit spec: %+v", u)
	}
	if strings.Join(u.Environment, ",") != "SOCKET=/var/lib/liveswap/demo/run/0a1b2c3d0a1b2c3d.sock" {
		t.Fatalf("environment must be exactly the caller's env, got %v", u.Environment)
	}
	// A unit this process started reports no launch environment: the
	// launch remembered what it rendered (handle.launchEnv).
	if env := h.launchEnv(); env != nil {
		t.Fatalf("a started handle reports a launch environment %q", env)
	}
	st := h.state()
	if st.Unit != u.Name || st.PID != 4242 || st.StartedAt.IsZero() {
		t.Fatalf("handle state %+v", st)
	}
	// The command is read back from the manager, not remembered from
	// the spec, so it is the unit's own ExecStart.
	if !slices.Equal(st.Command, u.ExecStart) {
		t.Fatalf("handle must report the manager's ExecStart %v, got %v", u.ExecStart, st.Command)
	}
	if !r.Alive(h) {
		t.Fatal("freshly started instance must be alive")
	}
}

// Every launch draws a fresh nonce (newNonce, in app.go), and the
// nonce is what makes two starts of one version two units.
func TestSystemdRunnerUnitNamesAreUnique(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	if _, err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	again := spec
	again.nonce = "deadbeefdeadbeef"
	if _, err := r.Start(again); err != nil {
		t.Fatal(err)
	}
	if conn.unit(0).Name == conn.unit(1).Name {
		t.Fatal("two starts of the same version must get distinct unit names")
	}
}

func TestSystemdRunnerStartRejectsMissingBinary(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.command = []string{"./nope"}
	if _, err := r.Start(spec); err == nil {
		t.Fatal("a missing binary must fail Start, not become a unit that dies at exec")
	}
	if len(conn.started) != 0 {
		t.Fatal("no unit must be created for an unresolvable command")
	}
}

func TestSystemdRunnerZeroGraceGetsDefaultStopTimeout(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.grace = 0
	if _, err := r.Start(spec); err != nil {
		t.Fatal(err)
	}
	if conn.unit(0).StopTimeout != defaultStopTimeout {
		t.Fatalf("TimeoutStopSec=0 would mean wait forever; got %s", conn.unit(0).StopTimeout)
	}
}

func TestUnitPropertiesApplyHardening(t *testing.T) {
	props := unitProperties(unitSpec{Name: "x.service", ExecStart: []string{"/bin/true"}, StopTimeout: 3 * time.Second, Oneshot: true, Sandbox: probeSandboxSpec()})
	got := map[string]any{}
	for _, p := range props {
		got[p.Name] = p.Value.Value()
	}
	for name, want := range map[string]any{
		"Restart":          "no",
		"KillMode":         "control-group",
		"NoNewPrivileges":  true,
		"LimitCORE":        uint64(0),
		"LimitCORESoft":    uint64(0),
		"TimeoutStopUSec":  uint64(3_000_000),
		"Type":             "oneshot",
		"StandardOutput":   "journal",
		"StandardError":    "journal",
		"WorkingDirectory": "",
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	if _, ok := got["Environment"].([]string); !ok {
		t.Errorf("Environment must be a string array even when empty, got %T", got["Environment"])
	}
	if _, ok := got["ExecStartEx"]; !ok {
		t.Error("ExecStartEx missing")
	}
	if _, ok := got["TimeoutStartUSec"]; ok {
		t.Error("a unit with no start timeout must carry no TimeoutStartUSec: the manager's default stands")
	}
	for _, p := range unitProperties(unitSpec{ExecStart: []string{"/bin/true"}, StopTimeout: 500 * time.Nanosecond, Sandbox: probeSandboxSpec()}) {
		if p.Name == "TimeoutStopUSec" && p.Value.Value() != uint64(1) {
			t.Errorf("a sub-microsecond grace must clamp to 1µs, not 0 (= never SIGKILL); got %v", p.Value.Value())
		}
	}
	starts := map[time.Duration]uint64{7 * time.Second: 7_000_000, 500 * time.Nanosecond: 1}
	for timeout, want := range starts {
		var have any
		for _, p := range unitProperties(unitSpec{ExecStart: []string{"/bin/true"}, Oneshot: true, StartTimeout: timeout, Sandbox: probeSandboxSpec()}) {
			if p.Name == "TimeoutStartUSec" {
				have = p.Value.Value()
			}
		}
		if have != want {
			t.Errorf("start timeout %s: TimeoutStartUSec = %v, want %d (0 would mean no bound at all)", timeout, have, want)
		}
	}
	_ = godbus.MakeVariant // keep the import honest: properties are godbus variants
}

// Promise: the argv a unit execs is the argv motel handed the
// manager, byte for byte. The plain ExecStart property carries no
// flags, and through it systemd substitutes the unit's environment
// (env_file values included) into every word at exec time: "$VAR"
// word-splits, "${VAR}" substitutes, "$$" becomes "$", and a word
// that names an unset variable leaves argv. So the command is set
// through ExecStartEx with no-env-expand and never through ExecStart.
// The words are what an operator's command line can carry: an inline
// script, a regex anchor, a literal dollar.
func TestUnitExecStartIsByteExact(t *testing.T) {
	argv := []string{"/usr/bin/node", "-e", `console.log("$HOME", "${HOME}")`, "$HOME", "${HOME}", "$$", "$MOTEL_UNSET_IN_EVERY_UNIT", "^a.*$", "a$b"}
	want := slices.Clone(argv)
	var ex []any
	for _, p := range unitProperties(unitSpec{Name: "x.service", ExecStart: argv, StopTimeout: time.Second, Sandbox: probeSandboxSpec()}) {
		switch p.Name {
		case "ExecStart":
			t.Fatalf("the command must not be set through the plain ExecStart, which carries no no-env-expand flag: %v", p.Value)
		case "ExecStartEx":
			// The wire shape systemd's ExecStartEx takes: the binary,
			// its argv, and the flag names.
			if sig := p.Value.Signature().String(); sig != "a(sasas)" {
				t.Fatalf("ExecStartEx signature = %s, want a(sasas)", sig)
			}
			ex = append(ex, p.Value.Value())
		}
	}
	if len(ex) != 1 {
		t.Fatalf("want exactly one ExecStartEx property, got %d", len(ex))
	}
	cmds, ok := ex[0].([]execCommandEx)
	if !ok || len(cmds) != 1 {
		t.Fatalf("ExecStartEx = %#v, want one command", ex[0])
	}
	// Behind the sibling shell, whose "$@" hands the words on untouched
	// (withSibling); the app's argv is the tail, byte for byte.
	if cmds[0].Path != siblingShell || !slices.Equal(cmds[0].Args, withSibling(want)) || !slices.Equal(appArgv(cmds[0].Args), want) {
		t.Fatalf("ExecStartEx argv = %q %q, want %q behind the sibling shell, byte for byte", cmds[0].Path, cmds[0].Args, want)
	}
	if !slices.Equal(cmds[0].Flags, []string{"no-env-expand"}) {
		t.Fatalf("ExecStartEx flags = %q, want exactly no-env-expand (and no ignore-failure: a failed command fails the unit)", cmds[0].Flags)
	}
}

// TestEveryUnitCarriesASibling: the manager execs a shell that forks
// one sleeping sibling into the cgroup and then becomes the app, so
// systemd 257 waits the grace on every stop of a one-process app
// (#380); and the argv read back from the manager is the app's own,
// the shell stripped, so status, state.json and a reattach see what
// the operator wrote. An argv of another shape (a unit from before the
// sibling) reads back as it is.
func TestEveryUnitCarriesASibling(t *testing.T) {
	argv := []string{"./server", "--flag"}
	got := withSibling(argv)
	// The script is pinned to the byte: appArgv recognises the units of
	// this moteld by it, so a reworded script would make every unit a
	// previous binary started read back as the shell's argv in status
	// and state.json after an upgrade, with this test still green if
	// it only looked for the parts.
	if got[0] != "/bin/sh" || got[1] != "-c" || got[2] != `unset PWD; /usr/bin/sleep 0 || exit 127; /usr/bin/sleep infinity & exec "$@"` || got[3] != "motel-app" || !slices.Equal(got[4:], argv) {
		t.Fatalf("withSibling = %q", got)
	}
	if !slices.Equal(appArgv(got), argv) {
		t.Fatalf("appArgv(withSibling) = %q, want %q", appArgv(got), argv)
	}
	st := statusFromProps(map[string]any{"ExecStart": [][]any{{"/bin/sh", got, false}}})
	if !slices.Equal(st.ExecStart, argv) {
		t.Fatalf("ExecStart read back = %q, want the app's argv %q", st.ExecStart, argv)
	}
	bare := statusFromProps(map[string]any{"ExecStart": [][]any{{"./server", argv, false}}})
	if !slices.Equal(bare.ExecStart, argv) {
		t.Fatalf("an argv with no sibling must read back as it is, got %q", bare.ExecStart)
	}
	if !slices.Equal(appArgv([]string{"/bin/sh", "-c", "echo", "x"}), []string{"/bin/sh", "-c", "echo", "x"}) {
		t.Fatal("a shell command of the operator's own is not the sibling's and must not be stripped")
	}
}

// TestPreflightRefusesAHostWithoutTheSiblingTools: the script exits
// 127 when its sleep cannot run, but that is after the unit exists, as
// a crash; the two tools are checked before, and the refusal names
// them.
func TestPreflightRefusesAHostWithoutTheSiblingTools(t *testing.T) {
	if err := siblingToolsPresent(siblingShell, siblingSleep); err != nil {
		t.Fatalf("this host has both tools, yet: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "sleep")
	err := siblingToolsPresent(siblingShell, missing)
	var pf *preflightError
	if !errors.As(err, &pf) || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "#380") {
		t.Fatalf("a missing sleep must be a pre-flight refusal naming it, got %v", err)
	}
	notExec := filepath.Join(t.TempDir(), "sh")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := siblingToolsPresent(notExec, siblingSleep); !errors.As(err, &pf) || !strings.Contains(err.Error(), notExec) {
		t.Fatalf("a shell that is not executable must be a pre-flight refusal naming it, got %v", err)
	}
}

func TestSystemdRunnerWatcherClosesDoneWhenUnitGone(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)
	if r.Alive(h) {
		t.Fatal("Alive must be false once the unit is gone")
	}
	if len(conn.resets()) != 0 {
		t.Fatal("a cleanly unloaded unit needs no reset")
	}
}

func TestSystemdRunnerWatcherResetsFailedUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	unit := h.state().Unit
	conn.setStatus(unit, failedStatus)
	waitDone(t, r, h)
	if rs := conn.resets(); len(rs) != 1 || rs[0] != unit {
		t.Fatalf("failed unit must be reset exactly once, got %v", rs)
	}
	if exit := h.(*systemdHandle).exit.Load(); exit == nil || exit.exitString() != "exit status 3" {
		t.Fatalf("exit facts must be recorded before done closes, got %v", exit)
	}
}

func TestSystemdRunnerTransportErrorNeverKillsHandle(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatusErr(errors.New("dbus: connection closed"))
	time.Sleep(20 * time.Millisecond) // many polls' worth of errors
	if !r.Alive(h) {
		t.Fatal("a manager outage must not be reported as an instance exit")
	}
	select {
	case <-r.Wait(h):
		t.Fatal("done closed during a transport outage")
	default:
	}
	conn.setStatusErr(nil)
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)
}

func TestSystemdRunnerStopConfirmsUnitGone(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(h, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != h.state().Unit {
		t.Fatalf("stops %v", stops)
	}
	if r.Alive(h) {
		t.Fatal("stopped instance must not be alive")
	}
	if err := r.Stop(h, time.Second); err != nil || len(conn.stops()) != 1 {
		t.Fatal("a second Stop is a no-op nil")
	}
}

func TestSystemdRunnerStopErrorsWhenJobDoesNotFinish(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	conn.stopResult = "failed"
	conn.mu.Unlock()
	if err := r.Stop(h, time.Second); err == nil || !strings.Contains(err.Error(), "stop job failed") {
		t.Fatalf("a non-done stop job must be an error, got %v", err)
	}
	if !r.Alive(h) {
		t.Fatal("an unconfirmed stop must leave the handle alive")
	}
	conn.mu.Lock()
	conn.stopResult = ""
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Stop(h, time.Second); err == nil || !strings.Contains(err.Error(), "dbus down") {
		t.Fatalf("a transport error on stop must be reported, got %v", err)
	}
}

func TestSystemdRunnerStopWaitsForObservedExit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	// The stop job reports done but the unit lingers (say, the manager
	// is slow to unload it). Stop must not return nil on the job alone.
	conn.mu.Lock()
	conn.stopLeaves = true
	conn.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		conn.setStatus(h.state().Unit, failedStatus)
	}()
	if err := r.Stop(h, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.Alive(h) {
		t.Fatal("handle must be dead after Stop returns nil")
	}
}

func TestSystemdRunnerStartJobFailureResetsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	fs := failedStatus
	conn.mu.Lock()
	conn.startResult = "failed"
	conn.failStatus = &fs
	conn.mu.Unlock()
	spec := testApp(t)
	_, err := r.Start(spec)
	if err == nil || !strings.HasPrefix(err.Error(), "unit ") || !strings.HasSuffix(err.Error(), ": start job failed (exit status 3)") {
		t.Fatalf("the start job's failure must read as it always did, got %v", err)
	}
	if len(conn.resets()) != 1 {
		t.Fatalf("a unit whose start job failed must be reset, got %v", conn.resets())
	}
}

// endedJobConn is the manager when a start job's result came in as the
// job's context ended (jobResult reports the result then): any further
// call on that context fails, as a D-Bus call on an ended one does.
type endedJobConn struct {
	*fakeSystemdConn
	mu  sync.Mutex
	job context.Context
}

func (c *endedJobConn) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	c.mu.Lock()
	c.job = ctx
	c.mu.Unlock()
	return c.fakeSystemdConn.StartTransientUnit(ctx, u)
}

func (c *endedJobConn) onEndedJob(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ctx == c.job
}

func (c *endedJobConn) UnitStatus(ctx context.Context, name string) (unitStatus, error) {
	if c.onEndedJob(ctx) {
		return unitStatus{}, context.DeadlineExceeded
	}
	return c.fakeSystemdConn.UnitStatus(ctx, name)
}

func (c *endedJobConn) UnitStatusAndEnvironment(ctx context.Context, name string) (unitStatus, []string, error) {
	if c.onEndedJob(ctx) {
		return unitStatus{}, nil, context.DeadlineExceeded
	}
	return c.fakeSystemdConn.UnitStatusAndEnvironment(ctx, name)
}

func (c *endedJobConn) ResetFailedUnit(ctx context.Context, name string) error {
	if c.onEndedJob(ctx) {
		return context.DeadlineExceeded
	}
	return c.fakeSystemdConn.ResetFailedUnit(ctx, name)
}

// Promise: the reads after a start job run on a context of their own,
// as reconcileStart's do, so a job whose result came in as its context
// ended still gets them: a started unit is adopted with its pid, and a
// failed one is reported with its exit status and reset.
func TestSystemdRunnerStartReadsAfterTheJobOnTheirOwnContext(t *testing.T) {
	r, fake := newTestSystemdRunner(t)
	r.conn = &endedJobConn{fakeSystemdConn: fake}
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if pid := h.state().PID; pid != 4242 {
		t.Fatalf("adopted with pid %d, want the unit's 4242", pid)
	}

	r, fake = newTestSystemdRunner(t)
	r.conn = &endedJobConn{fakeSystemdConn: fake}
	fs := failedStatus
	fake.mu.Lock()
	fake.startResult = "failed"
	fake.failStatus = &fs
	fake.mu.Unlock()
	if _, err := r.Start(testApp(t)); err == nil || !strings.HasSuffix(err.Error(), ": start job failed (exit status 3)") {
		t.Fatalf("a failed start must carry its exit status, got %v", err)
	}
	if len(fake.resets()) != 1 {
		t.Fatalf("the failed unit must be reset, got %v", fake.resets())
	}
}

// Promise: a job whose result is in when the context ends reports
// that result, not the context's error. A select picks among ready
// cases at random, so a pre_start that finished at the moment of its
// deadline was reported as cancelled half the time, and a migration
// that ran had its release deleted. Many rounds, since the bug is a
// coin toss per round.
func TestJobResultPrefersAFinishedJobOverAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		ch := make(chan string, 1)
		ch <- "done"
		if res, err := jobResult(ctx, ch); err != nil || res != "done" {
			t.Fatalf("a finished job read as %q, %v", res, err)
		}
	}
	// With no result in, the context's end is the answer.
	if _, err := jobResult(ctx, make(chan string, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled with no result, got %v", err)
	}
}

func TestSystemdRunnerRunOnce(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	if err := r.RunOnce(context.Background(), spec); err != nil {
		t.Fatalf("RunOnce success: %v", err)
	}
	u := conn.unit(0)
	if !u.Oneshot || !strings.HasSuffix(u.Name, ".prestart.service") || !unitBelongsTo(u.Name, "demo") {
		t.Fatalf("pre_start must be a oneshot unit, got %+v", u)
	}
	if u.SyslogIdentifier != "motel-demo" || u.StopTimeout != spec.grace {
		t.Fatalf("pre_start unit shares the app's identity: %+v", u)
	}
}

func TestSystemdRunnerRunOnceReportsExitStatus(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	fs := failedStatus
	conn.mu.Lock()
	conn.startResult = "failed"
	conn.failStatus = &fs
	conn.mu.Unlock()
	err := r.RunOnce(context.Background(), testApp(t))
	if err == nil || !strings.HasPrefix(err.Error(), "exit status 3 (unit ") || !strings.HasSuffix(err.Error(), ": job failed)") {
		t.Fatalf("error must carry the exit status and job result in the format it always had, got %v", err)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != conn.unit(0).Name {
		t.Fatalf("failed oneshot must be reset, got %v", rs)
	}
}

func TestSystemdRunnerRunOnceCancelStopsUnit(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.mu.Lock()
	conn.blockStart = true
	conn.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := r.RunOnce(ctx, testApp(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if stops := conn.stops(); len(stops) != 1 || stops[0] != conn.unit(0).Name {
		t.Fatalf("a cancelled pre_start must be stopped, stops=%v", stops)
	}
}

func TestSystemdRunnerReattach(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	started := time.Now().Add(-time.Hour)

	if _, ok, err := r.Reattach(handleState{PID: 1}); ok || err != nil {
		t.Fatal("no unit recorded ⇒ cannot reattach, not an error")
	}
	if _, ok, err := r.Reattach(handleState{Unit: "motel-demo.v1.aaaaaaaaaaaaaaaa.service"}); ok || err != nil {
		t.Fatal("unknown unit ⇒ observed not running")
	}

	adopted := []string{"/opt/app/server", "--port=0"}
	launched := []string{"API_TOKEN=launched-value-01", "SOCKET=/run/app.sock"}
	conn.mu.Lock()
	conn.env = map[string][]string{"live.service": launched}
	conn.mu.Unlock()
	conn.setStatus("live.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 77, Sandboxed: true, ExecStart: adopted})
	h, ok, err := r.Reattach(handleState{Unit: "live.service", StartedAt: started})
	if !ok || err != nil {
		t.Fatalf("running unit must be adopted: ok=%v err=%v", ok, err)
	}
	if st := h.state(); st.PID != 77 || st.Unit != "live.service" || !st.StartedAt.Equal(started) {
		t.Fatalf("adopted state %+v", st)
	}
	// Reattach is the path with no launch in this process to remember,
	// so the command can only come from the manager — and must, or
	// status would report nothing for every app across a restart.
	if st := h.state(); !slices.Equal(st.Command, adopted) {
		t.Fatalf("a reattached handle must report the unit's command %v, got %v", adopted, st.Command)
	}
	// The same for the environment it was launched with: the values the
	// running app holds, which the response filter must know (#657).
	if got := h.launchEnv(); !slices.Equal(got, launched) {
		t.Fatalf("a reattached handle must report the unit's environment %q, got %q", launched, got)
	}
	// A running unit WITHOUT the sandbox is never adopted: nothing
	// this moteld starts lacks it, so it is something else's unit —
	// a development build that still had `sandbox off`, say — and
	// adopting it would serve an app bare with no signal. It is
	// stopped here and now, not left for the relaunch's sweep (which
	// a failing relaunch preparation would never reach).
	conn.setStatus("bare.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 78})
	if _, ok, err := r.Reattach(handleState{Unit: "bare.service"}); ok || err != nil {
		t.Fatalf("a running unit without the sandbox must be stopped, not adopted: ok=%v err=%v", ok, err)
	}
	if stops := conn.stops(); len(stops) == 0 || stops[len(stops)-1] != "bare.service" {
		t.Fatalf("the bare unit was not stopped: stops=%v", stops)
	}
	// And a stop that cannot be confirmed is an error, so the caller
	// does not launch beside a unit that may still be running.
	conn.setStatus("bare2.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 79})
	conn.stopErr = errors.New("manager went away")
	if _, ok, err := r.Reattach(handleState{Unit: "bare2.service"}); ok || err == nil {
		t.Fatalf("an unconfirmed stop of a bare unit must be an error: ok=%v err=%v", ok, err)
	}
	conn.stopErr = nil
	if !r.Alive(h) || r.Wait(h) == nil {
		t.Fatal("adopted handle must be watched like a spawned one")
	}
	conn.setStatus("live.service", unitStatus{LoadState: "not-found"})
	waitDone(t, r, h)

	conn.setStatus("dead.service", failedStatus)
	if _, ok, err := r.Reattach(handleState{Unit: "dead.service"}); ok || err != nil {
		t.Fatal("a failed unit must never be adopted, and is not an error")
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "dead.service" {
		t.Fatalf("failed unit must be reset on discovery, got %v", rs)
	}

	conn.setStatusErr(errors.New("dbus down"))
	if _, ok, err := r.Reattach(handleState{Unit: "live.service"}); ok || err == nil {
		t.Fatal("an unreadable unit is an error, never 'not running'")
	}
}

func TestSystemdRunnerStartReconcilesAmbiguousFailure(t *testing.T) {
	// The D-Bus call errors but the unit is running: adopt it.
	r, conn := newTestSystemdRunner(t)
	running := unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 99, Sandboxed: true}
	conn.mu.Lock()
	conn.startErr = errors.New("dbus: reply lost")
	conn.failStatus = &running
	conn.mu.Unlock()
	spec := testApp(t)
	h, err := r.Start(spec)
	if err != nil || h == nil {
		t.Fatalf("a unit observed running after an ambiguous start must be adopted: %v", err)
	}
	if h.state().PID != 99 || !r.Alive(h) {
		t.Fatalf("adopted handle %+v", h.state())
	}

	// The unit is observed absent: a plain failure, no unconfirmed flag.
	r2, conn2 := newTestSystemdRunner(t)
	conn2.mu.Lock()
	conn2.startErr = errors.New("dbus: reply lost")
	conn2.mu.Unlock()
	_, err = r2.Start(spec)
	if err == nil || unitUnconfirmed(err) {
		t.Fatalf("unit observed gone ⇒ ordinary error, got %v", err)
	}

	// Nothing can be read and the stop fails too: unconfirmed.
	r3, conn3 := newTestSystemdRunner(t)
	conn3.mu.Lock()
	conn3.startErr = errors.New("dbus: reply lost")
	conn3.statusErr = errors.New("dbus down")
	conn3.stopErr = errors.New("dbus down")
	conn3.mu.Unlock()
	_, err = r3.Start(spec)
	if !unitUnconfirmed(err) {
		t.Fatalf("unreadable + unstoppable ⇒ unconfirmed, got %v", err)
	}
	if len(conn3.stops()) != 1 {
		t.Fatal("a best-effort stop must be attempted")
	}

	// Nothing can be read; the stop job says done but the unit can
	// still not be observed: unconfirmed (invariant 4 wants an
	// observation, not a job result).
	r4, conn4 := newTestSystemdRunner(t)
	conn4.mu.Lock()
	conn4.startErr = errors.New("dbus: reply lost")
	conn4.statusErr = errors.New("dbus down")
	conn4.mu.Unlock()
	_, err = r4.Start(spec)
	if !unitUnconfirmed(err) {
		t.Fatalf("unobservable after stop ⇒ unconfirmed, got %v", err)
	}
}

func TestParseManagerUint(t *testing.T) {
	for in, want := range map[string]uint64{
		"@t 524288":               524288,
		"524288":                  524288,
		"@t 18446744073709551615": 0, // unlimited: leave the unit alone
		"garbage":                 0,
		"":                        0,
	} {
		if got := parseManagerUint(in); got != want {
			t.Errorf("parseManagerUint(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIsNoSuchUnit(t *testing.T) {
	if !isNoSuchUnit(godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}) || !isNoSuchUnit(godbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject"}) {
		t.Fatal("systemd's not-loaded errors must map to not-found")
	}
	if isNoSuchUnit(errors.New("dbus: connection closed")) || isNoSuchUnit(godbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"}) {
		t.Fatal("transport failures must not read as not-found")
	}
}

func TestStatusFromPropsAndStopBudget(t *testing.T) {
	st := statusFromProps(map[string]any{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"MainPID": uint32(77), "ExecMainCode": int32(1), "ExecMainStatus": int32(3),
		"TimeoutStopUSec": uint64(60_000_000),
		// As godbus decodes a(sasbttttuii): an array of structs, each
		// []any, whose second member is the argv.
		"ExecStart": [][]any{{"/opt/app/server", []string{"/opt/app/server", "--flag"}, false}},
	})
	if st.MainPID != 77 || st.ExecMainStatus != 3 || st.StopTimeout != time.Minute {
		t.Fatalf("parsed %+v", st)
	}
	if !slices.Equal(st.ExecStart, []string{"/opt/app/server", "--flag"}) {
		t.Fatalf("ExecStart parsed as %v", st.ExecStart)
	}
	// The unit's own budget wins over a smaller caller grace; a larger
	// caller grace wins; nothing known ⇒ default, never zero.
	if st.stopBudget(3*time.Second) != time.Minute || st.stopBudget(2*time.Minute) != 2*time.Minute {
		t.Fatal("stopBudget must take the larger of unit budget and grace")
	}
	if (unitStatus{}).stopBudget(0) != defaultStopTimeout {
		t.Fatal("unknown budget and zero grace must fall back to the default")
	}
}

// TestEnvStrings: the unit's Environment, an "as" that godbus decodes
// as a []string, is read as the manager gave it, every pair in its
// order: an empty value, a value holding = whole, and a repeated key
// twice (the manager refuses a key twice at a unit's start, "Invalid
// environment block", so a running unit never reports one; the reader
// still drops nothing it is given). It is what a reattach hands the
// response filter (#657), so a pair dropped here is a value sent as
// written. Any other shape is no environment, never a panic.
func TestEnvStrings(t *testing.T) {
	env := []string{"API_TOKEN=one-value-0001", "EMPTY=", "DSN=user=app password=p=q", "API_TOKEN=two-value-0002", "SOCKET=/run/app.sock"}
	got := envStrings(env)
	if !slices.Equal(got, env) {
		t.Fatalf("Environment read as %q, want %q", got, env)
	}
	env[0] = "API_TOKEN=changed"
	if got[0] != "API_TOKEN=one-value-0001" {
		t.Fatal("the environment read must be a copy of the manager's")
	}
	for _, v := range []any{nil, "API_TOKEN=x", []any{"API_TOKEN=x"}, [][]string{{"API_TOKEN=x"}}, []string{}} {
		if got := envStrings(v); got != nil {
			t.Errorf("%#v read as %q, want nothing", v, got)
		}
	}
}

// TestStatusCarriesNoEnvironment: a status, read on every poll, stop
// and sweep and kept as a handle's last, holds none of the unit's
// environment, which the full property map holds (#657's review).
func TestStatusCarriesNoEnvironment(t *testing.T) {
	st := statusFromProps(map[string]any{"LoadState": "loaded", "Environment": []string{"API_TOKEN=one-value-0001"}})
	if strings.Contains(fmt.Sprintf("%+v", st), "one-value-0001") {
		t.Fatalf("a status holds the environment: %+v", st)
	}
}

// TestSystemdRunnerReattachAdoptsAUnitWithNoEnvironment: a read that
// succeeds with no environment (the property missing, or a shape not
// read) still adopts the running unit, which serves; the handle reports
// none, and the app's filter knows the file as it is now.
func TestSystemdRunnerReattachAdoptsAUnitWithNoEnvironment(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.setStatus("live.service", unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 77, Sandboxed: true})
	h, ok, err := r.Reattach(handleState{Unit: "live.service"})
	if !ok || err != nil {
		t.Fatalf("a running unit must be adopted though it reports no environment: ok=%v err=%v", ok, err)
	}
	if env := h.launchEnv(); env != nil {
		t.Fatalf("no environment reported as %q", env)
	}
}

// propExecStart reads a nested D-Bus shape out of a map motel does
// not build, so every way it can be malformed must read as "no command"
// rather than as a panic or as a half-parsed argv. A missing command is
// reported as missing; it is never guessed at.
func TestPropExecStartRefusesEveryShapeButItsOwn(t *testing.T) {
	argv := []string{"/opt/app/server", "--flag"}
	for _, tc := range []struct {
		name string
		val  any
		want []string
	}{
		{"godbus struct array", [][]any{{"/opt/app/server", argv, false}}, argv},
		{"first entry wins", [][]any{{"/x", argv, false}, {"/y", []string{"other"}, false}}, argv},
		{"no ExecStart at all", nil, nil},
		{"empty array", [][]any{}, nil},
		{"entry too short", [][]any{{"/opt/app/server"}}, nil},
		{"argv is not a list", [][]any{{"/x", "server --flag", false}}, nil},
		{"property is a scalar", uint64(3), nil},
		// Shapes godbus does not produce for this property are refused
		// rather than accommodated — reporting a command inferred from
		// an unrecognised layout would be worse than reporting none.
		{"outer decoded as []any", []any{[]any{"/x", argv, false}}, nil},
		{"argv decoded as []any", [][]any{{"/x", []any{"/opt/app/server"}, false}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			props := map[string]any{}
			if tc.val != nil {
				props["ExecStart"] = tc.val
			}
			if got := propExecStart(props); !slices.Equal(got, tc.want) {
				t.Fatalf("propExecStart = %v, want %v", got, tc.want)
			}
		})
	}
	// The result never aliases the property map: a caller holding the
	// argv cannot be edited from under it by a later read.
	props := map[string]any{"ExecStart": [][]any{{"/x", argv, false}}}
	got := propExecStart(props)
	got[0] = "mutated"
	if argv[0] != "/opt/app/server" {
		t.Fatal("propExecStart must copy the argv, not alias the property map")
	}
}

func TestSystemdRunnerRunOnceCancelUnconfirmedWhenStopFails(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.mu.Lock()
	conn.blockStart = true
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := r.RunOnce(ctx, testApp(t))
	if !unitUnconfirmed(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled pre_start that could not be stopped is unconfirmed and still carries the cause, got %v", err)
	}
}

func TestSystemdRunnerSweepLogsTheLeftoverExitItReadOffTheUnit(t *testing.T) {
	// ListUnits reports states only, so a leftover's exit rendered
	// from the listing always read "no process exit recorded" — on the
	// one line an operator would learn why it stopped. The sweep reads
	// the unit before logging.
	r, conn := newTestSystemdRunner(t)
	core, logs := observer.New(zap.WarnLevel)
	r.setLogger(zap.New(core))
	spec := testApp(t)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	oom := failedStatus
	oom.ExecMainCode, oom.ExecMainStatus = 2, 9 // CLD_KILLED by SIGKILL
	conn.setStatus("motel-demo.v0.9.1111111111111111.service", oom)
	if err := r.Sweep("demo", keep); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var got string
	for _, e := range logs.All() {
		if e.Message == "resetting leftover unit" {
			got = e.ContextMap()["exit"].(string)
		}
	}
	if got != "killed by signal 9 (killed)" {
		t.Fatalf("leftover exit logged as %q, want the unit's own exit", got)
	}
}

func TestSystemdRunnerSweepDoesNotStopAStrayThatEndedAfterTheListing(t *testing.T) {
	// A stray in its last moments: listed as active, gone by the time
	// the sweep reads it. Stopping a unit the manager has unloaded
	// fails and would report the whole sweep unconfirmed — refusing a
	// launch over a unit that no longer exists. The fresh read must
	// route it to the leftover branch instead.
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	const stray = "motel-demo.v2.4444444444444444.service"
	conn.setStatus(stray, unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})
	conn.mu.Lock()
	conn.vanishOnRead = map[string]bool{stray: true}
	conn.mu.Unlock()
	if err := r.Sweep("demo", keep); err != nil {
		t.Fatalf("a stray that ended on its own must not make the sweep unconfirmed: %v", err)
	}
	if stops := conn.stops(); len(stops) != 0 {
		t.Fatalf("no stop may be sent to a unit that is already gone, got %v", stops)
	}
	if rs := conn.resets(); len(rs) != 0 {
		t.Fatalf("an unloaded unit has nothing to reset, got %v", rs)
	}
}

func TestSystemdRunnerSweep(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	straySpec := spec
	straySpec.nonce = "deadbeefdeadbeef"
	stray, err := r.Start(straySpec)
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatus("motel-demo.v0.9.1111111111111111.service", failedStatus) // leftover failed unit of ours
	conn.setStatus("motel-demo-api.v1.2222222222222222.service", unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})
	conn.setStatus("motel-demo.v1.notanonce.service", unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})

	if err := r.Sweep("demo", keep); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	stops := conn.stops()
	if len(stops) != 1 || stops[0] != stray.state().Unit {
		t.Fatalf("exactly the stray unit of this app must be stopped, got %v", stops)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != "motel-demo.v0.9.1111111111111111.service" {
		t.Fatalf("the failed leftover must be reset, got %v", rs)
	}
	waitDone(t, r, stray)
	if !r.Alive(keep) {
		t.Fatal("keep must be untouched")
	}

	// A stop that cannot be confirmed is an error: nothing may be GC'd.
	conn.setStatus("motel-demo.v2.3333333333333333.service", unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true})
	conn.mu.Lock()
	conn.stopErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Sweep("demo", keep); !unitUnconfirmed(err) {
		t.Fatalf("unconfirmed stray stop must surface, got %v", err)
	}
	conn.mu.Lock()
	conn.stopErr = nil
	conn.listErr = errors.New("dbus down")
	conn.mu.Unlock()
	if err := r.Sweep("demo", keep); err == nil {
		t.Fatal("a listing failure is an error")
	}

	// A failed unit that cannot be reset stays loaded: not a clean sweep.
	conn.mu.Lock()
	conn.listErr = nil
	conn.resetErr = errors.New("dbus down")
	conn.mu.Unlock()
	conn.setStatus("motel-demo.v3.4444444444444444.service", failedStatus)
	if err := r.Sweep("demo", keep); err == nil {
		t.Fatal("a reset failure must be reported: the unit is still loaded")
	}
}

func TestSystemdRunnerSweepLeavesARunningPreStartToFinish(t *testing.T) {
	// A motel that stopped during a deploy leaves that deploy's
	// pre_start running under the manager. Stopping it would half-apply
	// a migration (#317): the sweep leaves it, names it, and reports
	// itself unconfirmed so no caller starts or deletes anything beside
	// it. A pre_start that has already ended is a leftover like any.
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	const migration = "motel-demo.v2.5555555555555555.prestart.service"
	const ended = "motel-demo.v1.6666666666666666.prestart.service"
	conn.setStatus(migration, unitStatus{LoadState: "loaded", ActiveState: "activating", SubState: "start", Sandboxed: true})
	conn.setStatus(ended, failedStatus)

	err = r.Sweep("demo", keep)
	var mr *migrationRunningError
	if !errors.As(err, &mr) || mr.unit != migration || mr.version != "v2" {
		t.Fatalf("a running pre_start must make the sweep unconfirmed, naming it; got %v", err)
	}
	if !strings.Contains(err.Error(), "pre_start of v2") || !strings.Contains(err.Error(), "from an earlier deploy") {
		t.Fatalf("the message must name the migration's version and say it is an earlier deploy's, got %q", err)
	}
	if stops := conn.stops(); len(stops) != 0 {
		t.Fatalf("a running pre_start must not be stopped, got %v", stops)
	}
	if rs := conn.resets(); len(rs) != 1 || rs[0] != ended {
		t.Fatalf("an ended pre_start is a leftover and is reset, got %v", rs)
	}

	// Removing the app spares nothing: there is no deploy left to wait for.
	if err := r.StopAll("demo"); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	stops := conn.stops()
	if !slices.Contains(stops, migration) || !slices.Contains(stops, keep.state().Unit) {
		t.Fatalf("StopAll must stop the migration and the instance, got %v", stops)
	}
}

func TestSystemdRunnerRunOnceCarriesItsOwnDeadline(t *testing.T) {
	// A pre_start outlives a moteld that stops during it, and with it
	// the context that bounded it: the unit's own TimeoutStartSec= is
	// what still ends it on time.
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.startTimeout = 90 * time.Second
	if err := r.RunOnce(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := conn.unit(0).StartTimeout; got != 90*time.Second {
		t.Fatalf("the pre_start unit's start timeout = %s, want its deadline", got)
	}
}

func TestSystemdRunnerWatcherBackfillsPIDAndCommand(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	// Start succeeds but the immediate MainPID read fails: the unit is
	// fine, motel just could not ask. The watcher repairs the PID
	// once the manager answers again.
	running := unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true}
	conn.mu.Lock()
	conn.failStatus = &running
	conn.mu.Unlock()
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4242 {
		t.Fatalf("sanity: pid read at start, got %d", h.state().PID)
	}
	// Now the same with the read failing at start.
	conn.setStatusErr(errors.New("dbus hiccup"))
	h2, err := r.Start(testApp(t))
	if err != nil {
		t.Fatalf("the start job succeeded; a failed PID read must not fail Start: %v", err)
	}
	if h2.state().PID != 0 || h2.state().Command != nil {
		t.Fatalf("pid and command should be unknown after a failed read, got %+v", h2.state())
	}
	// The command is repaired for the same reason as the pid, and it
	// matters more: an instance that came up during a D-Bus outage
	// would otherwise report no command for its whole life — blank in
	// exactly the incident the field exists to explain.
	conn.setStatusErr(nil)
	deadline := time.Now().Add(2 * time.Second)
	for h2.state().PID != 4242 || len(h2.state().Command) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never backfilled the handle, still %+v", h2.state())
		}
		time.Sleep(time.Millisecond)
	}
	// One unit, one command: a later read cannot change it.
	first := h2.state().Command
	conn.setStatus(h2.state().Unit, unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true, ExecStart: []string{"/somewhere/else"}})
	time.Sleep(20 * time.Millisecond)
	if !slices.Equal(h2.state().Command, first) {
		t.Fatalf("the command must be read once and kept, got %v after %v", h2.state().Command, first)
	}
}

// An app that dies before the watcher's first good poll must still
// report what it was running. The adopt-time read failed, so the only
// status the handle ever sees is the terminal one — and that is
// precisely the crash an operator is trying to explain, so the command
// is taken off every successful read rather than only off a running
// one.
func TestSystemdRunnerCommandSurvivesACrashBeforeTheFirstGoodPoll(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	conn.setStatusErr(errors.New("dbus hiccup"))
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.state().Command != nil {
		t.Fatalf("sanity: the adopt read failed, so no command yet, got %v", h.state().Command)
	}
	// The manager answers again, and what it reports is a unit that has
	// already failed — never a running one.
	unit := h.state().Unit
	started := conn.unit(0)
	conn.setStatus(unit, unitStatus{
		LoadState: "loaded", ActiveState: "failed", Result: "exit-code",
		ExecMainCode: 1, ExecMainStatus: 2, Sandboxed: true, ExecStart: started.ExecStart,
	})
	conn.setStatusErr(nil)
	select {
	case <-r.Wait(h):
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher never observed the unit go")
	}
	if !slices.Equal(h.state().Command, started.ExecStart) {
		t.Fatalf("a dead instance must still report what it was running: got %v, want %v", h.state().Command, started.ExecStart)
	}
}

func TestUnitApp(t *testing.T) {
	for name, want := range map[string]string{
		"motel-blog.v1.4.2.0a1b2c3d0a1b2c3d.service":          "blog",
		"motel-blog-api.v1.0a1b2c3d0a1b2c3d.prestart.service": "blog-api",
		"motel-blog.service":                                  "",
		"dbus.service":                                        "",
	} {
		got, ok := unitApp(name)
		if got != want || ok != (want != "") {
			t.Errorf("unitApp(%q) = %q,%v want %q", name, got, ok, want)
		}
	}
}

func TestSystemdRunnerSweepStopsStraysConcurrentlyAndHonoursTheGuard(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	running := unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true}
	conn.setStatus("motel-demo.v1.1111111111111111.service", running)
	conn.setStatus("motel-demo.v2.2222222222222222.service", running)
	// A guard that turns false after the listing: nothing may be stopped.
	if err := r.sweep(context.Background(), "demo", nil, func() bool { return false }, false); err != nil {
		t.Fatal(err)
	}
	if len(conn.stops()) != 0 {
		t.Fatalf("guard says the app is owned now; stops=%v", conn.stops())
	}
	// Without the guard both strays go, and the sweep is one round of
	// stops, not two in sequence: a slow StopUnit is paid once.
	conn.mu.Lock()
	conn.stopDelay = 150 * time.Millisecond
	conn.mu.Unlock()
	started := time.Now()
	if err := r.sweep(context.Background(), "demo", nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 280*time.Millisecond {
		t.Fatalf("strays must be stopped concurrently; two sequential stops would take ~300ms, took %s", took)
	}
	if len(conn.stops()) != 2 {
		t.Fatalf("both strays must be stopped, got %v", conn.stops())
	}
}

func TestSystemdRunnerHandleRecordsStopBudget(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	spec.grace = 42 * time.Second
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.(*systemdHandle).stopTimeout != 42*time.Second {
		t.Fatalf("Start must record the unit's stop budget, got %s", h.(*systemdHandle).stopTimeout)
	}
	conn.setStatus("old.service", unitStatus{LoadState: "loaded", ActiveState: "active", StopTimeout: 7 * time.Minute, Sandboxed: true})
	h2, ok, err := r.Reattach(handleState{Unit: "old.service"})
	if !ok || err != nil {
		t.Fatal("adopt")
	}
	if h2.(*systemdHandle).stopTimeout != 7*time.Minute {
		t.Fatalf("Reattach must record the unit's own budget, got %s", h2.(*systemdHandle).stopTimeout)
	}
	conn.setStatus("old.service", unitStatus{LoadState: "not-found"})
	waitDone(t, r, h2)
}

func TestUsecDuration(t *testing.T) {
	if usecDuration(60_000_000) != time.Minute || usecDuration(0) != 0 || usecDuration(-1) != 0 {
		t.Fatal("usec conversion")
	}
	if usecDuration(int(^uint64(0)>>1)) != 0 {
		t.Fatal("an overflowing (infinity) budget must read as unknown, not negative")
	}
}

func TestUnitBelongsTo(t *testing.T) {
	for name, want := range map[string]bool{
		"motel-blog.v1.4.2.0a1b2c3d0a1b2c3d.service":          true,
		"motel-blog.v1.4.2.0a1b2c3d0a1b2c3d.prestart.service": true,
		"motel-blog-api.v1.0a1b2c3d0a1b2c3d.service":          false, // another app
		"motel-blog.v1.service":                               false, // no nonce
		"motel-blogx.v1.0a1b2c3d0a1b2c3d.service":             false,
		"blog.v1.0a1b2c3d0a1b2c3d.service":                    false,
	} {
		if got := unitBelongsTo(name, "blog"); got != want {
			t.Errorf("unitBelongsTo(%q, blog) = %v, want %v", name, got, want)
		}
	}
}

func TestUnitStatusRunning(t *testing.T) {
	for _, tc := range []struct {
		st   unitStatus
		want bool
	}{
		{unitStatus{LoadState: "loaded", ActiveState: "active", Sandboxed: true}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "activating"}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "deactivating"}, true},
		{unitStatus{LoadState: "loaded", ActiveState: "inactive"}, false},
		{unitStatus{LoadState: "loaded", ActiveState: "failed"}, false},
		{unitStatus{LoadState: "not-found", ActiveState: "inactive"}, false},
		// The manager does not know the unit: no active state at all.
		{unitStatus{LoadState: "not-found"}, false},
		// A daemon-reload could not read the unit's file back, or the
		// file is gone; the manager still runs its process (#380).
		{unitStatus{LoadState: "error", ActiveState: "active"}, true},
		{unitStatus{LoadState: "bad-setting", ActiveState: "active"}, true},
		{unitStatus{LoadState: "not-found", ActiveState: "active"}, true},
		{unitStatus{LoadState: "error", ActiveState: "inactive"}, false},
	} {
		if got := tc.st.running(); got != tc.want {
			t.Errorf("%+v running=%v want %v", tc.st, got, tc.want)
		}
	}
	for _, tc := range []struct {
		st   unitStatus
		want string
	}{
		{unitStatus{ExecMainCode: 1, ExecMainStatus: 3}, "exit status 3"},
		{unitStatus{ExecMainCode: 1, ExecMainStatus: 0}, "exit status 0"},
		{unitStatus{ExecMainCode: 2, ExecMainStatus: 9}, "killed by signal 9 (killed)"},
		// The manager's Result names the OOM kill the main process's
		// own end does not: the kernel takes the process that asked
		// (a child, under `sh -c`), and the manager stops the unit, so
		// the main process reads as a signal, an exit code or the
		// stop's SIGTERM — and the suffix lands on all of them.
		{unitStatus{ExecMainCode: 2, ExecMainStatus: 9, Result: "oom-kill"}, "killed by signal 9 (killed); the kernel's OOM killer took a process of the unit (its memory_max, or the box's memory), and the unit was stopped for it"},
		{unitStatus{ExecMainCode: 1, ExecMainStatus: 137, Result: "oom-kill"}, "exit status 137; the kernel's OOM killer took a process of the unit (its memory_max, or the box's memory), and the unit was stopped for it"},
		{unitStatus{ExecMainCode: 2, ExecMainStatus: 15, Result: "oom-kill"}, "killed by signal 15 (terminated); the kernel's OOM killer took a process of the unit (its memory_max, or the box's memory), and the unit was stopped for it"},
		{unitStatus{ExecMainCode: 0, ExecMainStatus: 0}, "no process exit recorded"},
		{unitStatus{ExecMainCode: 0, ExecMainStatus: 0, Result: "exec-condition"}, "no process exit recorded (result exec-condition)"},
	} {
		st, want := tc.st, tc.want
		if got := st.exitString(); got != want {
			t.Errorf("%+v exitString = %q want %q", st, got, want)
		}
	}
}

// TestSystemdRunnerWatcherFollowsMainPID: a sandboxed unit's MainPID
// changes shortly after start (PrivatePIDs= makes the manager fork an
// intermediate that sets the namespace up, then report its child), so
// the handle must track the manager's current MainPID, not the first
// value it read.
func TestSystemdRunnerWatcherFollowsMainPID(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	h, err := r.Start(testApp(t))
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4242 { // the fake's MainPID for a started unit
		t.Fatalf("sanity: pid read at start, got %d", h.state().PID)
	}
	conn.setStatus(h.state().Unit, unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4243, Sandboxed: true})
	deadline := time.Now().Add(2 * time.Second)
	for h.state().PID != 4243 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not follow the MainPID change, still %d", h.state().PID)
		}
		time.Sleep(time.Millisecond)
	}
	if r.Alive(h) != true {
		t.Fatal("a MainPID change must not read as an exit")
	}
}

// TestSystemdRunnerStartSettlesMainPID: a start returns with the app's
// pid, not the intermediate the manager reports for its first few
// milliseconds while it sets the PID namespace up — every unit has
// one, so every start settles.
func TestSystemdRunnerStartSettlesMainPID(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	r.settle = mainPIDSettle
	spec := testApp(t)
	// The fake presets MainPID 4242 at start; flip it to 4243 shortly
	// after, like the manager does once the namespace is set up.
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			conn.mu.Lock()
			n := len(conn.started)
			conn.mu.Unlock()
			if n > 0 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		conn.setStatus(conn.unit(0).Name, unitStatus{LoadState: "loaded", ActiveState: "active", MainPID: 4243, Sandboxed: true})
	}()
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if h.state().PID != 4243 {
		t.Fatalf("start returned the intermediate pid %d, want the settled 4243", h.state().PID)
	}
}

// The instance nonce names the unit: the socket the app binds carries
// the same one (appDirs.socket), which is what lets a reattach check
// the recorded socket against the recorded unit.
func TestUnitNameCarriesTheSpecNonce(t *testing.T) {
	name, err := unitName(startSpec{app: "blog", version: "v1", nonce: "deadbeefdeadbeef"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "motel-blog.v1.deadbeefdeadbeef.service" {
		t.Fatalf("unit name = %q", name)
	}
	if got, ok := unitNonce(name); !ok || got != "deadbeefdeadbeef" {
		t.Fatalf("unitNonce(%q) = %q, %v", name, got, ok)
	}
	pre, err := unitName(startSpec{app: "blog", version: "v1", nonce: "deadbeefdeadbeef"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := unitNonce(pre); !ok || got != "deadbeefdeadbeef" {
		t.Fatalf("the pre_start unit shares the nonce: unitNonce(%q) = %q, %v", pre, got, ok)
	}
	for _, bad := range []string{"", "DEADBEEFDEADBEEF", "deadbeef", "deadbeefdeadbee", "deadbeefdeadbeef0", "zzzzzzzzzzzzzzzz"} {
		if _, err := unitName(startSpec{app: "blog", version: "v1", nonce: bad}, false); err == nil {
			t.Errorf("nonce %q accepted", bad)
		}
	}
	if _, ok := unitNonce("motel-blog.v1.service"); ok {
		t.Error("a name without a nonce is not one of ours")
	}
}

// TestProbeUnitsAreNotAppUnits: the capability probe's unit must sit
// outside the app-name grammar, or a concurrent sweepUnknownApps would
// stop it mid-probe — or an app legitimately called "sandbox-probe"
// would collide with it — failing the start for no reason at all.
func TestProbeUnitsAreNotAppUnits(t *testing.T) {
	name, err := unitName(startSpec{app: "sandbox-probe", version: "probe", nonce: "0a1b2c3d0a1b2c3d", probe: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, unitPrefix) || !strings.HasSuffix(name, ".service") {
		t.Fatalf("probe unit name %q is not a motel unit", name)
	}
	if app, ok := unitApp(name); ok {
		t.Fatalf("probe unit %q parses as app %q; a sweep would stop it", name, app)
	}
	// And an app really named sandbox-probe gets its own, distinct units.
	appUnit, err := unitName(startSpec{app: "sandbox-probe", version: "v1", nonce: "0a1b2c3d0a1b2c3d"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := unitApp(appUnit); !ok || got != "sandbox-probe" {
		t.Fatalf("a configured app must still parse: %q -> %q %v", appUnit, got, ok)
	}
	if strings.HasPrefix(appUnit, unitPrefix+"sandboxprobe_") {
		t.Fatal("a configured app collides with the probe namespace")
	}
}

// TestOwnIdentifiersAreNoAppsName: what the probe's unit writes its
// output under is no app's identifier, motel-<app>, whatever the app is
// called, so a logs read of an app named sandbox-probe (the logs row
// selects SYSLOG_IDENTIFIER=motel-<app>) is never given the probe's
// line. An app's units, the pre_start's too, write under the app's.
// The backup units' identifier is held in backups/unit.
func TestOwnIdentifiersAreNoAppsName(t *testing.T) {
	u, err := newSystemdRunner(nil, zap.NewNop()).unitFor(probeStartSpec("0a1b2c3d0a1b2c3d", "ipc:[4026531839]"), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{u.SyslogIdentifier, probeIdentifier} {
		if rest, ok := strings.CutPrefix(id, "motel-"); id != probeIdentifier || ok && appNameRe.MatchString(rest) {
			t.Errorf("the probe writes under %q, which an app's name makes", id)
		}
	}
	for _, oneshot := range []bool{false, true} {
		spec := startSpec{app: "sandbox-probe", version: "v1", nonce: "0a1b2c3d0a1b2c3d", command: []string{"/bin/sh"}, dir: "/", sandbox: probeSandboxSpec()}
		u, err := newSystemdRunner(nil, zap.NewNop()).unitFor(spec, oneshot)
		if err != nil {
			t.Fatal(err)
		}
		if u.SyslogIdentifier != "motel-sandbox-probe" {
			t.Errorf("an app named sandbox-probe writes under %q", u.SyslogIdentifier)
		}
	}
}

// TestSystemdRunnerSettlesFromUnpublishedMainPID covers the case the
// settle loop used to get wrong: StartTransientUnit can return before
// the manager has published any MainPID, so adopt reads 0. The first
// non-zero value is then the namespace-setup intermediate, not the
// app, and a predicate of "different from what adopt saw" accepted it
// — storing a pid that dies milliseconds later. The existing settle
// test starts at 4242 and never exercises this.
func TestSystemdRunnerSettlesFromUnpublishedMainPID(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	r.settle = mainPIDSettle
	spec := testApp(t)
	// 0 (nothing published yet) -> 4242 (the intermediate) -> 4243 (the app).
	conn.startStatus = &unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 0, Sandboxed: true}
	go func() {
		for {
			time.Sleep(2 * time.Millisecond)
			conn.mu.Lock()
			n := len(conn.started)
			conn.mu.Unlock()
			if n > 0 {
				break
			}
		}
		name := conn.unit(0).Name
		time.Sleep(20 * time.Millisecond)
		conn.setStatus(name, unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 4242, Sandboxed: true})
		time.Sleep(20 * time.Millisecond)
		conn.setStatus(name, unitStatus{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 4243, Sandboxed: true})
	}()
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.state().PID; got != 4243 {
		t.Fatalf("settled on pid %d, want the app's 4243 — 4242 is the intermediate the manager reports while it sets the namespace up, and 0 is nothing published yet", got)
	}
}

// After the watcher saw a unit end, a status with no ExecMainCode is
// a clean exit whose unit was unloaded before the status was read —
// not "no process exit recorded", which is for a start job that never
// got as far as a process.
func TestExitAfterEndNamesTheUnloadedCleanExit(t *testing.T) {
	gone := unitStatus{LoadState: "not-found"}
	if got := gone.exitAfterEnd(); !strings.Contains(got, "without failure") {
		t.Fatalf("unloaded clean exit = %q", got)
	}
	failed := unitStatus{LoadState: "loaded", ActiveState: "failed", Result: "exit-code", ExecMainCode: 1, ExecMainStatus: 3}
	if got := failed.exitAfterEnd(); got != "exit status 3" {
		t.Fatalf("a recorded exit is itself: %q", got)
	}
	skipped := unitStatus{LoadState: "loaded", ActiveState: "failed", Result: "dependency"}
	if got := skipped.exitAfterEnd(); !strings.Contains(got, "no process exit recorded") {
		t.Fatalf("a job that never ran a process: %q", got)
	}
}

// A release's command may be a symlink to a file outside the view whose
// path is any text the deployer chose. The 422 names it; the said the
// operator's channels are told stands the command's file in its place.
func TestPreflightOutsideTheViewSaidNamesNoPath(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	spec := testApp(t)
	target := filepath.Join(t.TempDir(), deployersWords)
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(spec.dir, "server")
	if err := os.Remove(server); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, server); err != nil {
		t.Fatal(err)
	}
	var pe *preflightError
	if err := r.Preflight(spec); !errors.As(err, &pe) || !strings.Contains(pe.msg, target) || !strings.Contains(pe.msg, "not inside the sandbox view") {
		t.Fatalf("want the 422 naming the file outside the view, got %v", err)
	}
	if want := commandsFile + " is not inside the sandbox view of app demo: "; !strings.HasPrefix(pe.said, want) || strings.Contains(pe.said, deployersWords) || strings.Contains(pe.said, spec.dir) {
		t.Fatalf("said = %q, want it to start %q and name no path", pe.said, want)
	}
}

// Preflight is the launch's resolution run early: the release's own
// script passes; an executable for another machine is refused with the
// runner to build on; a command outside the sandbox view is refused
// with where the runtime has to live — all before a unit exists.
// A command given by name is the one the unit's own PATH finds, in the
// pre-flight and at the launch alike, not the one moteld's PATH finds:
// the unit execs the path resolved for it (#666). The refusal for a
// name the unit's PATH does not hold prints no PATH: an env_file's is
// a secret.
func TestCommandIsTheOneTheUnitsPathFinds(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	spec := testApp(t)
	tool := func(dir, name string) string {
		must(t, os.MkdirAll(dir, 0o755))
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755))
		return p
	}
	moteld, own := filepath.Join(spec.dir, "moteld-bin"), filepath.Join(spec.dir, "app-bin")
	t.Setenv("PATH", moteld+":/usr/bin:/bin")
	for _, tc := range []struct {
		name string
		in   []string // the dirs that hold the tool
	}{
		{"only on the unit's PATH", []string{own}},
		{"on both: the unit's wins", []string{moteld, own}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "tool" + strconv.Itoa(len(tc.in))
			var want string
			for _, d := range tc.in {
				want = tool(d, name)
			}
			s := spec
			s.command = []string{name, "serve"}
			s.env = append([]string{"PATH=" + own + ":/usr/bin"}, spec.env...)
			if err := r.Preflight(s); err != nil {
				t.Fatalf("pre-flight: %v", err)
			}
			u, err := r.unitFor(s, false)
			if err != nil {
				t.Fatal(err)
			}
			if u.ExecStart[0] != want {
				t.Fatalf("the unit execs %s, want %s, the one its own PATH finds", u.ExecStart[0], want)
			}
		})
	}
	s := spec
	s.command = []string{"no-such-tool"}
	s.env = append([]string{"PATH=/srv/secret-path-value/bin"}, spec.env...)
	err := r.Preflight(s)
	if err == nil || !strings.Contains(err.Error(), "the PATH the app's env_file or inline env sets") || strings.Contains(err.Error(), "secret-path-value") {
		t.Fatalf("a name the unit's PATH does not hold must be refused naming which PATH, never its value: %v", err)
	}
}

func TestSystemdRunnerPreflight(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	spec := testApp(t)
	if err := r.Preflight(spec); err != nil {
		t.Fatalf("the release's script must pass: %v", err)
	}
	other := uint16(62)
	if runtime.GOARCH == "amd64" {
		other = 183
	}
	if machineFor(runtime.GOARCH) != 0 {
		if err := os.WriteFile(filepath.Join(spec.dir, "server"), elfHeader(2, false, other), 0o755); err != nil {
			t.Fatal(err)
		}
		err := r.Preflight(spec)
		if err == nil || !strings.Contains(err.Error(), "build on a runner of the box's architecture") {
			t.Fatalf("an executable for another machine must be refused: %v", err)
		}
	}
	// Outside the view: a runtime under a home directory, the nvm case
	// (/bin/sh would not do — /bin is /usr/bin on a merged /usr, and
	// /usr is in every view).
	elsewhere := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := spec
	outside.command = []string{elsewhere}
	if err := r.Preflight(outside); err == nil || !strings.Contains(err.Error(), "not inside the sandbox view") {
		t.Fatalf("a command outside the view must be refused: %v", err)
	}
	conn.mu.Lock()
	n := len(conn.started)
	conn.mu.Unlock()
	if n != 0 {
		t.Fatalf("Preflight created %d units; it must create none", n)
	}
	// An environment the manager could start but not read back after a
	// daemon-reload (#380): refused, naming the largest variable and
	// never its value; the same from unitFor, the last word at launch.
	big := spec
	big.env = append([]string{"SMALL=1", "BIG=" + strings.Repeat("x", unitLineBudget)}, spec.env...)
	err := r.Preflight(big)
	if err == nil || !strings.Contains(err.Error(), "the largest variable, BIG,") || strings.Contains(err.Error(), "xxxx") {
		t.Fatalf("an oversized environment must be refused naming the variable, not its value: %v", err)
	}
	var pe *preflightError
	if !errors.As(err, &pe) {
		t.Fatalf("the refusal must be a pre-flight error (a 422 at deploy), got %T", err)
	}
	if _, err := r.unitFor(big, false); err == nil || !strings.Contains(err.Error(), "the largest variable, BIG,") {
		t.Fatalf("unitFor must refuse it too: %v", err)
	}
	if _, err := r.Start(big); err == nil {
		t.Fatal("Start must refuse it before a unit exists")
	}
	conn.mu.Lock()
	n = len(conn.started)
	conn.mu.Unlock()
	if n != 0 {
		t.Fatalf("an oversized environment reached the manager (%d units)", n)
	}
}

// TestSweepResetsAFailedUnitWhoseFileDidNotLoad: a unit a daemon-reload
// could not read back (LoadState error) that has since ended failed is
// a leftover like any other: reset, so it does not sit in the manager
// as failed for good, and so the sweep says so (#380).
func TestSweepResetsAFailedUnitWhoseFileDidNotLoad(t *testing.T) {
	r, conn := newTestSystemdRunner(t)
	name, err := unitName(startSpec{app: "demo", version: "v9", nonce: "0a1b2c3d0a1b2c3d"}, false)
	if err != nil {
		t.Fatal(err)
	}
	conn.setStatus(name, unitStatus{LoadState: "error", ActiveState: "failed", Result: "timeout"})
	if err := r.Sweep("demo", nil); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if resets := conn.resets(); len(resets) != 1 || resets[0] != name {
		t.Fatalf("resets %v: a failed unit the manager could not read back must be reset like any other", resets)
	}
	if _, err := conn.UnitStatus(context.Background(), name); err != nil {
		t.Fatal(err)
	}
}

// TestUnitLineLength pins the estimate against what the manager was
// measured to write: plain ASCII one byte, a control or non-ASCII byte
// four (30,000 bytes of CJK read back as a 120,187-byte line), a quote,
// a backslash or a % two; and the bound against the transient files
// measured — a 720 KiB environment's unit read back after a reload, a
// 1.2 MiB one did not.
func TestUnitLineLength(t *testing.T) {
	if n := unitLineLength("Environment", []string{"A=b"}); n != len("Environment=")+3+3 {
		t.Fatalf("A=b: %d", n)
	}
	cjk := strings.Repeat("語", 10000) // 30,000 bytes
	if n := unitLineLength("", []string{cjk}); n < 120000 || n > 120100 {
		t.Fatalf("30,000 bytes of CJK: %d, want about 120,000", n)
	}
	if n := unitLineLength("", []string{"%\"\\'"}); n != 1+3+8 { // %, a double quote, a backslash, a single quote
		t.Fatalf("escaped punctuation: %d", n)
	}
	env := func(total int) []string {
		var l []string
		for i := 0; i*100*1024 < total; i++ {
			l = append(l, "BIG"+strconv.Itoa(i)+"="+strings.Repeat("x", 100*1024))
		}
		return l
	}
	// An entry with no = (nothing of motel's makes one) is named as
	// such, never printed: the refusal must not carry a value.
	if err := checkUnitLines(startSpec{app: "a", env: []string{strings.Repeat("v", unitLineBudget)}, command: []string{"./server"}}); err == nil || !strings.Contains(err.Error(), "an entry with no =") || strings.Contains(err.Error(), "vvvv") {
		t.Fatalf("an =-less entry must be refused without its value: %v", err)
	}
	if err := checkUnitLines(startSpec{app: "a", env: env(480 * 1024), command: []string{"./server"}}); err != nil {
		t.Fatalf("480 KiB of environment must pass: %v", err)
	}
	if err := checkUnitLines(startSpec{app: "a", env: env(720 * 1024), command: []string{"./server"}}); err == nil {
		t.Fatal("720 KiB of environment is past the bound (the file still read back, at 822 KB; 1.2 MiB did not, and the bound keeps the escaping's slack)")
	}
	if err := checkUnitLines(startSpec{app: "a", env: nil, command: []string{"./server", strings.Repeat("y", unitLineBudget)}}); err == nil || !strings.Contains(err.Error(), "the command of app a") {
		t.Fatalf("an oversized command must be refused: %v", err)
	}
}
