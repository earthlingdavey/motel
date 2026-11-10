//go:build integration

package liveswap

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// These run against a real systemd user manager (make test-integration
// boots one in the dev-systemd container and points XDG_RUNTIME_DIR at
// it). They prove the properties unit tests can only assume: that
// "stop" and "crash" empty the whole cgroup, that the manager reports
// exits the way the runner reads them, and that a second runner can
// adopt a unit the first one created.

func integrationRunner(t *testing.T) *systemdRunner {
	t.Helper()
	if err := userManager.probe(); err != nil {
		t.Fatalf("no systemd user manager (run via `make test-integration`): %v", err)
	}
	logger, _ := zap.NewDevelopment()
	r := newSystemdRunner(userManager, logger.Named(t.Name()))
	r.poll = 50 * time.Millisecond
	t.Cleanup(r.close)
	return r
}

// scriptApp writes ./server with the given body into a release dir.
func scriptApp(t *testing.T, body string) startSpec {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return startSpec{
		app:     "itest",
		version: strings.ToLower(strings.TrimPrefix(t.Name(), "TestIntegrationSystemd")),
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     dir,
		env:     []string{"SOCKET=/var/lib/liveswap/itest/run/0a1b2c3d0a1b2c3d.sock", "PATH=" + os.Getenv("PATH")},
		grace:   2 * time.Second,
		sandbox: itestSandbox(dir),
	}
}

// itestSandbox is the sandbox every integration unit runs in: the
// script's own directory bound writable, under a root nothing else
// uses. Every unit is sandboxed, so the lane's are too — which puts
// each in its own PID namespace, where the pids a script writes are
// namespace-local. unitPIDs reads the host pids from the cgroup.
func itestSandbox(dir string) *sandboxSpec {
	return &sandboxSpec{root: "/var/tmp/liveswap-itest", writable: []bindPath{{dest: dir, source: dir}}}
}

// unitPIDs is the unit's process set as the manager's own ledger has
// it — host pids from the unit's cgroup — the same way the e2e lane
// reads them.
func unitPIDs(t *testing.T, unit string) []int {
	t.Helper()
	cg := strings.TrimSpace(run(t, "systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit))
	if cg == "" {
		t.Fatalf("unit %s has no control group", unit)
	}
	b, err := os.ReadFile("/sys/fs/cgroup" + cg + "/cgroup.procs")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
	}
	return pids
}

func hasPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// workerTree is a leader that forks two workers and records all three
// PIDs in pids.txt, the shape (npm → node → …) cgroup kill exists for.
const workerTree = `sleep 300 & w1=$!
sleep 300 & w2=$!
echo "$$ $w1 $w2" > pids.txt
wait
`

// waitForPIDs blocks until pids.txt holds the leader's line (two
// spaces), so the workers are forked, and fails on a non-numeric field.
// It does not count the fields: an empty $w1 still passes.
func waitForPIDs(t *testing.T, dir string) {
	t.Helper()
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "pids.txt"))
		if err == nil && strings.Count(string(b), " ") == 2 {
			data = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pids.txt never written: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, f := range strings.Fields(string(data)) {
		if _, err := strconv.Atoi(f); err != nil {
			t.Fatal(err)
		}
	}
}

func alivePID(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	// EPERM etc.: exists but not ours — still alive.
	// Zombies also answer kill(0); systemd reaps its units' children,
	// so a lingering zombie here would itself be a bug.
	return true
}

func waitPIDsGone(t *testing.T, pids []int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var live []int
		for _, p := range pids {
			if alivePID(p) {
				live = append(live, p)
			}
		}
		if len(live) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still alive after %s: %v", within, live)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIntegrationSystemdStopKillsWholeTree(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	if len(pids) < 3 || !hasPID(pids, h.state().PID) {
		t.Fatalf("cgroup holds %v; want the leader (handle pid %d) and two workers", pids, h.state().PID)
	}
	if !r.Alive(h) {
		t.Fatal("instance should be alive")
	}
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop returned nil ⇒ the cgroup was empty (invariant 4); the
	// workers must be gone already, not "soon".
	waitPIDsGone(t, pids, 200*time.Millisecond)
	if r.Alive(h) {
		t.Fatal("stopped instance reads alive")
	}
	if st, err := userManager.UnitStatus(context.Background(), h.state().Unit); err != nil || st.loaded() {
		t.Fatalf("stopped unit must be unloaded, got %+v (%v)", st, err)
	}
}

func TestIntegrationSystemdLeaderCrashKillsWorkers(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, `sleep 300 & w1=$!
sleep 300 & w2=$!
echo "$$ $w1 $w2" > pids.txt
sleep 1
exit 3
`)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	if len(pids) < 3 {
		t.Fatalf("cgroup holds %v; want the leader and two workers", pids)
	}
	select {
	case <-r.Wait(h):
	case <-time.After(10 * time.Second):
		t.Fatal("crash never observed")
	}
	// With KillMode=control-group the leader's exit takes the workers
	// with it; done closes only once the manager reports the unit
	// gone, i.e. after they are dead.
	waitPIDsGone(t, pids, 200*time.Millisecond)
	exit := h.(*systemdHandle).exit.Load()
	if exit == nil || exit.exitString() != "exit status 3" {
		t.Fatalf("exit facts %+v", exit)
	}
	if st, err := userManager.UnitStatus(context.Background(), h.state().Unit); err != nil || st.loaded() {
		t.Fatalf("failed unit must have been reset and unloaded, got %+v (%v)", st, err)
	}
}

// Promise: a unit's IPC namespace and /dev/shm are its own (#311). The
// script reports its IPC namespace and writes a file into /dev/shm:
// the namespace is not this process's — the manager's, which every
// app would otherwise share — and the file is not in the host's
// /dev/shm, which PrivateDevices= alone would bind into the unit.
func TestIntegrationSystemdUnitIPCAndShmArePrivate(t *testing.T) {
	r := integrationRunner(t)
	n, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	canary := "/dev/shm/motel-itest-" + n
	t.Cleanup(func() { _ = os.Remove(canary) })
	spec := scriptApp(t, "readlink /proc/self/ns/ipc > ns.txt\necho x > "+canary+"\n")
	if err := r.RunOnce(context.Background(), spec); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(spec.dir, "ns.txt"))
	if err != nil {
		t.Fatal(err)
	}
	own, err := os.Readlink("/proc/self/ns/ipc")
	if err != nil {
		t.Fatal(err)
	}
	if ns := strings.TrimSpace(string(got)); !strings.HasPrefix(ns, "ipc:[") || ns == own {
		t.Fatalf("unit's IPC namespace %q, this process's %q: want the unit's own", ns, own)
	}
	if _, err := os.Lstat(canary); !os.IsNotExist(err) {
		t.Fatalf("%s written inside the unit is visible on the host (%v): /dev/shm is shared", canary, err)
	}
}

func TestIntegrationSystemdStopEscalatesToSIGKILL(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "trap '' TERM\necho \"$$\" > pids.txt\nwhile :; do sleep 1; done\n")
	spec.grace = time.Second
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(spec.dir, "pids.txt")); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	took := time.Since(started)
	if took < spec.grace {
		t.Fatalf("a TERM-ignoring app was killed after %s, before its %s grace", took, spec.grace)
	}
	if took > spec.grace+3*time.Second {
		t.Fatalf("SIGKILL escalation took %s", took)
	}
	if r.Alive(h) {
		t.Fatal("instance still alive after escalation")
	}
}

func TestIntegrationSystemdRunOnce(t *testing.T) {
	r := integrationRunner(t)
	ok := scriptApp(t, "echo \"$SOCKET $(pwd)\" > out.txt\nexit 0\n")
	if err := r.RunOnce(context.Background(), ok); err != nil {
		t.Fatalf("RunOnce ok: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(ok.dir, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	realDir, _ := filepath.EvalSymlinks(ok.dir)
	if len(fields) != 2 || fields[0] != "/var/lib/liveswap/itest/run/0a1b2c3d0a1b2c3d.sock" || fields[1] != realDir {
		t.Fatalf("env/cwd not propagated: %q (want the SOCKET path and %s)", out, realDir)
	}

	bad := scriptApp(t, "exit 4\n")
	if err := r.RunOnce(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "exit status 4") {
		t.Fatalf("non-zero exit: %v", err)
	}

	slow := scriptApp(t, "sleep 300\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = r.RunOnce(ctx, slow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled RunOnce: %v", err)
	}
	// A cancelled RunOnce stops its unit; the manager must not still
	// hold one (its pids are inside the unit's own namespace, so the
	// unit list is the host-side fact to check).
	deadline := time.Now().Add(5 * time.Second)
	for {
		units := strings.TrimSpace(run(t, "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "motel-itest.runonce.*"))
		if units == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled RunOnce left a unit behind:\n%s", units)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A failed pre_start reports its exit through exitError, and the
// journal tail reads back what the unit wrote, stdout and stderr, in
// order, under the real user manager — a child's line too. journald
// names a line's unit from its writer's /proc entry when it reads the
// line, and a shell reaps its child at once: a child that prints one
// line and exits leaves it with no unit field [measured: 20 of 20, the
// journal idle], and only the unit's stream holds it.
func TestIntegrationSystemdJournalTail(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "echo first line\necho second line >&2\nsh -c 'echo from a child'\nexit 2\n")
	since := time.Now()
	err := r.RunOnce(context.Background(), spec)
	var ee *exitError
	if err == nil || !errors.As(err, &ee) || ee.exit != "exit status 2" {
		t.Fatalf("RunOnce: %v (want an exitError with exit status 2)", err)
	}
	of := journalTailOf(t, spec, ee)
	if got := strings.Join(waitTail(t, of, since, 3), "|"); got != "first line|second line|from a child" {
		t.Fatalf("tail = %q", got)
	}
	// Nothing since a moment after the exit: the window is honoured.
	if lines, err := (journalctlReader{}).tail(context.Background(), of, time.Now().Add(5*time.Second), 40); err != nil || len(lines) != 0 {
		t.Fatalf("tail since the future: %v, %v", lines, err)
	}
}

// journald behind: the unit's lines reach it only after the unit has
// ended and its processes are reaped, so it stores every one of them
// with no unit field — the lane's flake, which journald stopped for
// the unit's life reproduces every time. The tail finds them all the
// same: by the pid the manager recorded for the main process, and then
// by its stream.
func TestIntegrationSystemdJournalTailWhenJournaldIsBehind(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "echo first line\necho second line >&2\nexit 2\n")
	since := time.Now()
	resume := pauseJournald(t)
	err := r.RunOnce(context.Background(), spec)
	resume()
	var ee *exitError
	if err == nil || !errors.As(err, &ee) || ee.exit != "exit status 2" {
		t.Fatalf("RunOnce: %v (want an exitError with exit status 2)", err)
	}
	if got := strings.Join(waitTail(t, journalTailOf(t, spec, ee), since, 2), "|"); got != "first line|second line" {
		t.Fatalf("tail = %q", got)
	}
}

// The instance's side of it: an app that prints and dies while
// journald is behind. Its lines carry no unit; the tail finds them by
// the pid the runner reports with its exit — the manager's record, as
// the watcher last read the unit.
func TestIntegrationSystemdJournalTailOfAnInstance(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "echo first line\necho second line >&2\nexit 3\n")
	since := time.Now()
	resume := pauseJournald(t)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-r.Wait(h):
	case <-time.After(10 * time.Second):
		t.Fatal("the instance did not end")
	}
	resume()
	exit, pid := r.Exit(h)
	if exit != "exit status 3" {
		t.Fatalf("Exit = %q, pid %d", exit, pid)
	}
	unit, err := unitName(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	of := tailOf{units: []string{unit}, pid: pid, ident: syslogIdentifier(spec.app)}
	if got := strings.Join(waitTail(t, of, since, 2), "|"); got != "first line|second line" {
		t.Fatalf("tail = %q", got)
	}
}

// journalTailOf is what a failed deploy asks the journal for, after a
// pre_start that failed: its unit, and its main process's pid under the
// app's identifier.
func journalTailOf(t *testing.T, spec startSpec, ee *exitError) tailOf {
	t.Helper()
	unit, err := unitName(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	return tailOf{units: []string{unit}, pid: ee.pid, ident: syslogIdentifier(spec.app)}
}

// waitTail reads the tail until it holds want lines or 5 s have
// passed: the journal lags the process by a moment.
func waitTail(t *testing.T, of tailOf, since time.Time, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), journalTailTimeout)
		lines, err := journalctlReader{}.tail(ctx, of, since, 40)
		cancel()
		if err == nil && len(lines) >= want {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal tail of %v (pid %d): %q, %v", of.units, of.pid, lines, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pauseJournald stops journald until resume is called or the test
// ends: a journald behind its writers, on demand. A process of its own
// resumes it 3 s on whatever becomes of the test binary — killed by
// the lane's timeout it runs no cleanup, and a journald left stopped
// [measured: still stopped 5 s after] would hang the lane's later
// tests.
func pauseJournald(t *testing.T) (resume func()) {
	t.Helper()
	backstop := exec.Command("sh", "-c", "sleep 3; pkill -CONT -x systemd-journal")
	backstop.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := backstop.Start(); err != nil {
		t.Fatalf("the resume backstop: %v", err)
	}
	go func() { _ = backstop.Wait() }()
	run(t, "pkill", "-STOP", "-x", "systemd-journal")
	resume = sync.OnceFunc(func() { _ = exec.Command("pkill", "-CONT", "-x", "systemd-journal").Run() })
	t.Cleanup(resume)
	return resume
}

func TestIntegrationSystemdReattachAdoptsLiveUnit(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h1, err := r1.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPIDs(t, spec.dir) // readiness: the workers are forked
	st := h1.state()
	pids := unitPIDs(t, st.Unit)
	if st.Unit == "" {
		t.Fatal("state must name the unit")
	}
	// The command is decoded out of the manager's own ExecStart
	// property — a nested D-Bus shape no fake can prove the layout of,
	// which is why the assertion belongs in this lane. A parser that
	// reads it as nothing would leave status quietly silent about what
	// every app is running.
	if len(st.Command) == 0 || !filepath.IsAbs(st.Command[0]) || filepath.Base(st.Command[0]) != "server" {
		t.Fatalf("the manager's ExecStart must decode to the resolved argv, got %v", st.Command)
	}
	// A "new motel": its own runner, same manager.
	r2 := newSystemdRunner(userManager, zap.NewNop())
	r2.poll = 50 * time.Millisecond
	t.Cleanup(r2.close)
	h2, ok, err := r2.Reattach(st)
	if !ok || err != nil {
		t.Fatalf("live unit must be adopted: ok=%v err=%v", ok, err)
	}
	if h2.state().PID != st.PID || !hasPID(pids, h2.state().PID) || h2.state().Unit != st.Unit {
		t.Fatalf("adopted %+v, want pid %d (in cgroup %v) unit %s", h2.state(), st.PID, pids, st.Unit)
	}
	// Reattach has no launch of its own to remember, so an adopted
	// handle can only report the command by reading it back — and must,
	// or status would go blank for every app across a motel restart.
	if !slices.Equal(h2.state().Command, st.Command) {
		t.Fatalf("adopted handle reports command %v, want the running unit's %v", h2.state().Command, st.Command)
	}
	if err := r2.Stop(h2, spec.grace); err != nil {
		t.Fatalf("Stop via adopter: %v", err)
	}
	waitPIDsGone(t, pids, 200*time.Millisecond)
	// The original runner's watcher sees the same truth.
	select {
	case <-r1.Wait(h1):
	case <-time.After(5 * time.Second):
		t.Fatal("original handle never saw the unit go")
	}
	if _, ok, err := r2.Reattach(st); ok || err != nil {
		t.Fatalf("a stopped unit must not be adoptable: ok=%v err=%v", ok, err)
	}
}

func TestIntegrationSystemdReattachResetsFailedUnit(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, "exit 7\n")
	h, err := r1.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Silence r1's watcher before it can reset the failure, standing in
	// for "motel was down when the app died".
	r1.close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := userManager.UnitStatus(context.Background(), h.state().Unit)
		if err == nil && st.ActiveState == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unit never reached failed: %+v (%v)", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r2 := newSystemdRunner(userManager, zap.NewNop())
	t.Cleanup(r2.close)
	if _, ok, err := r2.Reattach(h.state()); ok || err != nil {
		t.Fatalf("a failed unit must never be adopted: ok=%v err=%v", ok, err)
	}
	st, err := userManager.UnitStatus(context.Background(), h.state().Unit)
	if err != nil || st.loaded() {
		t.Fatalf("failed unit must be reset on discovery, got %+v (%v)", st, err)
	}
}

func TestIntegrationSystemdSweepStopsStrays(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	keep, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	strayDir := t.TempDir()
	straySpec := spec
	straySpec.nonce = "deadbeefdeadbeef"
	straySpec.dir = strayDir
	straySpec.sandbox = itestSandbox(strayDir)
	if err := os.WriteFile(filepath.Join(strayDir, "server"), []byte("#!/bin/sh\n"+workerTree), 0o755); err != nil {
		t.Fatal(err)
	}
	stray, err := r.Start(straySpec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPIDs(t, strayDir)
	waitForPIDs(t, spec.dir)
	strayPIDs := unitPIDs(t, stray.state().Unit)
	keepPIDs := unitPIDs(t, keep.state().Unit)
	if err := r.Sweep(spec.app, keep); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	waitPIDsGone(t, strayPIDs, 200*time.Millisecond)
	for _, p := range keepPIDs {
		if !alivePID(p) {
			t.Fatalf("keep's process %d was killed by the sweep", p)
		}
	}
	select {
	case <-r.Wait(stray):
	case <-time.After(5 * time.Second):
		t.Fatal("stray handle never saw its unit go")
	}
	if err := r.Stop(keep, spec.grace); err != nil {
		t.Fatal(err)
	}
}

// A motel that stops during a deploy leaves the deploy's pre_start
// running under the manager (#317). The next motel's sweep must
// leave it to finish — reporting itself unconfirmed, naming it — and
// confirm once it has ended: the migration runs to its end.
func TestIntegrationSystemdSweepLeavesARunningPreStart(t *testing.T) {
	r1 := integrationRunner(t)
	spec := scriptApp(t, "sleep 3\necho migrated > done.txt\n")
	spec.startTimeout = 30 * time.Second
	unit, err := unitName(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- r1.RunOnce(context.Background(), spec) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := userManager.UnitStatus(context.Background(), unit)
		if err == nil && st.running() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pre_start never ran: %+v (%v)", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r1.close() // the motel that started it is gone; the manager runs it on

	r2 := newSystemdRunner(userManager, zap.NewNop())
	t.Cleanup(r2.close)
	err = r2.Sweep(spec.app, nil)
	var mr *migrationRunningError
	if !errors.As(err, &mr) || mr.unit != unit {
		t.Fatalf("the sweep must leave the running pre_start and name it, got %v", err)
	}
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("the migration must run to its end, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the pre_start never ended")
	}
	if out, err := os.ReadFile(filepath.Join(spec.dir, "done.txt")); err != nil || strings.TrimSpace(string(out)) != "migrated" {
		t.Fatalf("the migration was cut short: %q (%v)", out, err)
	}
	if err := r2.Sweep(spec.app, nil); err != nil {
		t.Fatalf("once the pre_start has ended the sweep confirms, got %v", err)
	}
}

// With no motel watching it, a pre_start's own TimeoutStartSec= is
// what ends it: RunOnce here waits on no deadline of its own.
func TestIntegrationSystemdPreStartEndsOnItsOwnDeadline(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "sleep 300\n")
	spec.startTimeout = time.Second
	start := time.Now()
	err := r.RunOnce(context.Background(), spec)
	if err == nil {
		t.Fatal("a pre_start past its deadline must fail")
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("the unit's own deadline did not end it: took %s", took)
	}
}

// managerPID is the user manager's own process, for the stall test.
func managerPID(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("systemctl", "show", "-p", "MainPID", "--value", "user@"+strconv.Itoa(os.Getuid())+".service").Output()
	if err != nil {
		t.Fatalf("systemctl show user@: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid == 0 {
		t.Fatalf("no user manager pid: %q", out)
	}
	return pid
}

// The manager stops answering (SIGSTOP) for longer than a poll timeout:
// invariant 2 says the handle stays alive, and once the manager is
// back the watcher resumes and a Stop is honoured.
func TestIntegrationSystemdManagerStallIsNotACrash(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, workerTree)
	h, err := r.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	waitForPIDs(t, spec.dir) // readiness: the workers are forked
	pids := unitPIDs(t, h.state().Unit)
	mgr := managerPID(t)
	if err := syscall.Kill(mgr, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			_ = syscall.Kill(mgr, syscall.SIGCONT)
		}
	}
	defer resume()
	// Longer than pollTimeout: at least one poll must time out.
	deadline := time.Now().Add(pollTimeout + 3*time.Second)
	for time.Now().Before(deadline) {
		if !r.Alive(h) {
			t.Fatal("a stalled manager was reported as an instance exit")
		}
		time.Sleep(200 * time.Millisecond)
	}
	resume()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("Stop after the manager resumed: %v", err)
	}
	waitPIDsGone(t, pids, 2*time.Second)
}

// sandboxRoot makes a liveswap-style root outside /tmp (PrivateTmp=
// would hide a t.TempDir) with one app's release and shared dirs, a
// sibling app, and a state.json — the layout the sandbox must slice.
func sandboxRoot(t *testing.T) (root, release, shared string) {
	t.Helper()
	root, err := os.MkdirTemp("/var/tmp", "liveswap-itest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	release = filepath.Join(root, "itest", "releases", "v1")
	shared = filepath.Join(root, "itest", "shared")
	for _, d := range []string{release, shared, filepath.Join(root, "itest", "tmp"), filepath.Join(root, "other", "shared")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(root, "itest", "state.json"), filepath.Join(root, "other", "shared", "secret")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The convenience symlink an app really has next to its dirs.
	// Without it the probe's current=closed would be a check on a path
	// that never existed on the host either.
	if err := os.Symlink(filepath.Join("releases", "v1"), filepath.Join(root, "itest", "current")); err != nil {
		t.Fatal(err)
	}
	return root, release, shared
}

// TestIntegrationSystemdSandboxProbe: the capability probe against the
// real manager. The dev-systemd image is trixie (systemd 257) — the
// support matrix — so the answer is capable, unconditionally. A
// refusal here is a real failure and not a host to accommodate:
// either the kernel under the test container refuses user namespaces,
// or the sandbox regressed.
func TestIntegrationSystemdSandboxProbe(t *testing.T) {
	r := integrationRunner(t)
	if err := probeSandboxCapability(r); err != nil {
		t.Fatalf("the supported host must deliver the sandbox: %v", err)
	}
}

// TestIntegrationSandboxProbeRefusesAnUnfilteredUnit starts the
// capability probe's own unit, built as probeSandboxCapability builds
// it (probeStartSpec, unitFor, unitProperties), three times: whole;
// without RestrictNamespaces=, where the mode still reads 2 as it
// would from a filter the manager inherited and the unshare check must
// fail it; and without any seccomp-backed setting, which is what the
// manager runs where seccomp is unavailable to it, and where the mode
// check must fail it. Each failure must be the one the probe's journal
// line names. TestIntegrationSystemdSandboxProbe cannot stand in for
// the last two, since this host delivers the whole set.
func TestIntegrationSandboxProbeRefusesAnUnfilteredUnit(t *testing.T) {
	r := integrationRunner(t)
	conn, err := userManager.get()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(drop ...string) (result, line string) {
		t.Helper()
		nonce, err := newNonce()
		if err != nil {
			t.Fatal(err)
		}
		ipcNS, err := ownIPCNamespace()
		if err != nil {
			t.Fatal(err)
		}
		u, err := r.unitFor(probeStartSpec(nonce, ipcNS), true)
		if err != nil {
			t.Fatal(err)
		}
		// An identifier of its own, matched below: journald can store a
		// short-lived unit's lines with no unit field at all
		// (TestIntegrationSystemdJournalTailWhenJournaldIsBehind), but
		// the stream's identifier is on every one.
		u.SyslogIdentifier = "itest-probe-" + nonce
		var props []sddbus.Property
		for _, p := range unitProperties(u) {
			if !slices.Contains(drop, p.Name) {
				props = append(props, p)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
		defer cancel()
		ch := make(chan string, 1)
		if _, err := conn.StartTransientUnitContext(ctx, u.Name, "fail", props, ch); err != nil {
			t.Fatalf("starting %s: %v", u.Name, err)
		}
		if result, err = jobResult(ctx, ch); err != nil {
			t.Fatalf("waiting for %s: %v", u.Name, err)
		}
		_ = conn.ResetFailedUnitContext(ctx, u.Name)
		// The probe's own line names what it saw; the journal may take
		// a moment to have it.
		deadline := time.Now().Add(5 * time.Second)
		for {
			out, _ := exec.Command("journalctl", "--no-pager", "-o", "cat", "-t", u.SyslogIdentifier).CombinedOutput()
			if line = string(out); strings.Contains(line, "sandbox probe:") || time.Now().After(deadline) {
				return result, line
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// Every setting the manager applies through seccomp, and so skips
	// together where seccomp is unavailable to it. A new one added to
	// sandboxProperties without being listed here leaves the mode at 2,
	// and the last case below fails, naming it.
	seccompBacked := []string{"SystemCallFilter", "SystemCallErrorNumber", "SystemCallArchitectures",
		"RestrictAddressFamilies", "RestrictNamespaces", "RestrictRealtime", "RestrictSUIDSGID",
		"LockPersonality", "ProtectKernelModules", "ProtectKernelLogs", "ProtectKernelTunables", "PrivateDevices"}
	for _, c := range []struct {
		name, want string
		pass       bool
		drop       []string
	}{
		{"whole", ", seccomp 2, unshare refused", true, nil},
		{"no RestrictNamespaces", ", seccomp 2, unshare allowed", false, []string{"RestrictNamespaces"}},
		{"no seccomp-backed setting", ", seccomp 0, unshare allowed", false, seccompBacked},
	} {
		result, line := probe(c.drop...)
		if (result == "done") != c.pass {
			t.Errorf("%s: the probe unit's job is %q, want passed=%v: %q", c.name, result, c.pass, line)
		}
		if !strings.Contains(line, c.want) {
			t.Errorf("%s: the probe's journal line does not say %q: %q", c.name, c.want, line)
		}
	}
}

// writeSandboxView installs the shared view probe (sandboxViewScript,
// the same file e2e/liveswap/systemd.sh and packaging/test/smoke.sh
// use) into the release dir, alongside a ./server that sources it and
// then idles. Sourced, not run as a child, so the probe reports $$ as
// the unit's main pid.
func writeSandboxView(t *testing.T, release string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(release, sandboxViewName), []byte(sandboxViewScript), 0o644); err != nil {
		t.Fatal(err)
	}
	// sleep, not exec sleep: as namespace PID 1 an exec'd sleep would
	// have no default SIGTERM disposition and Stop would always have to
	// escalate to SIGKILL.
	server := "#!/bin/sh\n. ./sandbox-view.sh\nsleep 300\n"
	if err := os.WriteFile(filepath.Join(release, "server"), []byte(server), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedRuntimeDirFixture creates the runtime dir and admin socket that a
// real motel has, because this container runs none. Without it
// run_motel=closed and admin_socket=closed pass on paths that never
// existed on the host, proving nothing about the view — the same
// vacuity the manager-socket and /proc checks guard against by pinning
// the uid and the pid. The packaged unit creates /run/motel through
// RuntimeDirectory=; the socket is a real one, so [ -e ] sees what it
// would see in production.
func seedRuntimeDirFixture(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll("/run/motel", 0o750); err != nil {
		t.Fatalf("seeding /run/motel: %v", err)
	}
	sock := "/run/motel/admin.sock"
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seeding %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = l.Close(); _ = os.Remove(sock) })
	// The probe's "closed" is only evidence if the path is here to hide.
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("%s was not created; the in-unit probe would prove nothing: %v", sock, err)
	}
}

func readProbe(t *testing.T, shared string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(shared, "view.txt"))
		if err == nil && strings.Contains(string(b), "done=1") {
			m := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				k, v, _ := strings.Cut(line, "=")
				m[k] = v
			}
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("view.txt not written: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIntegrationSystemdSandboxedUnit starts a unit with the full
// sandbox and reads the view from inside: the namespaces are in
// effect, the root shows only the app's own dirs, everything nothing
// named is absent (not merely unreadable), the manager's /proc is
// closed, cgroupfs is read-only, the syscall filter is in force, /tmp
// is private and writable, HOME is the shared dir. Then the unit is
// stopped through the runner like any other.
func TestIntegrationSystemdSandboxedUnit(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	writeSandboxView(t, release)
	seedRuntimeDirFixture(t)
	mgrPID := strings.TrimSpace(run(t, "systemctl", "show", "-p", "MainPID", "--value", "user@"+strconv.Itoa(os.Getuid())+".service"))
	if mgrPID == "" || mgrPID == "0" {
		mgrPID = strconv.Itoa(os.Getppid())
	}
	spec := startSpec{
		app:     "itest",
		version: "sandboxed",
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     release,
		env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + shared,
			"MGR_PID=" + mgrPID, "MOTEL_UID=" + strconv.Itoa(os.Getuid())},
		grace: 2 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(h, 2*time.Second) })
	got := readProbe(t, shared)
	t.Logf("probe: %v", got)
	for k, want := range map[string]string{
		"pid": "1", "mgr_root": "closed", "mgr_environ": "closed",
		// The root shows this app and nothing else: sandboxRoot puts a
		// second app's shared/secret next door, and it must not be
		// nameable from in here.
		"root_listing": "itest ",
		"state":        "closed", "apptmp": "closed", "current": "closed",
		"release": "writable",
		// Deliberately no "root" expectation here. This lane's fixture
		// root is a /var/tmp temp dir, and PrivateTmp= gives the unit
		// its own writable /var/tmp with the binds created inside it
		// (sandbox.go, validateSandboxRoot) — so the root dir itself is
		// writable, while still naming nothing but this app. Under the
		// real /var/lib layout it is read-only, which e2e and the
		// packaging smoke both assert.
		"motel_lib": "closed", "run_motel": "closed", "etc_motel": "closed",
		"admin_socket": "closed", "mgr_socket": "closed",
		"notify_run": "closed", "notify_socket": "closed", "notify_state": "closed",
		"cgroup": "readonly", "tmp": "writable", "home": shared, "xdg_runtime": "unset",
		// No core dump of a crashed app, and no way to raise the soft
		// limit back: LimitCORE=0 on both, read back from inside.
		"core_soft": "0", "core_hard": "0",
		// The syscall filter is in force in here, not only set on the
		// unit: seccomp's filter mode, and the call the filter takes
		// out of @system-service refused (EPERM). The same call from
		// outside the unit is checked below.
		"seccomp": "2", "io_uring_setup": "refused",
		// The base view: an OS the app can actually run on. Without
		// these, "absent" below would only mean the unit is empty.
		"binsh": "ok", "usrbinenv": "ok", "etcssl": "ok", "resolvconf": "ok",
		// The trust store is named, not the tree holding it: /etc/ssl
		// also holds /etc/ssl/private. This lane used to check only
		// that /etc/ssl existed, which a bind of the whole tree — every
		// app handed motel's TLS keys — would have satisfied.
		"sslprivate": "absent",
		// No name to resolve was supplied, and the probe says so rather
		// than omitting the key: e2e is where DNS from inside is proved.
		"dns": "skipped",
		// The probe echoes back what it was handed. A blank or literal
		// value here would make every /proc assertion above vacuous.
		"saw_mgr_pid": mgrPID, "saw_uid": strconv.Itoa(os.Getuid()),
		// And the uid it actually used for the manager-socket path:
		// /run/user/<wrong uid> is absent too, so mgr_socket=closed
		// only means something once this is pinned.
		"uid": strconv.Itoa(os.Getuid()),
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// Deny-by-default: not one of these is bound, so not one of them
	// exists — no InaccessiblePaths= entry, and no list to keep current.
	for _, k := range []string{
		"abs_varlib", "abs_etcliveswap", "abs_opt", "abs_srv",
		"abs_home", "abs_root", "abs_mnt", "abs_media",
	} {
		if got[k] != "absent" {
			t.Errorf("%s = %q, want absent", k, got[k])
		}
	}
	// /etc is named entry by entry, never bound whole: an app that could
	// list all of /etc would see every other app's env_file.
	for _, unwanted := range []string{"motel", "liveswap", "shadow", "sudoers"} {
		if strings.Contains(" "+got["etc_listing"], " "+unwanted+" ") {
			t.Errorf("/etc inside the unit contains %s: %q", unwanted, got["etc_listing"])
		}
	}
	if got["uidmap"] == "4294967295" {
		t.Error("no user namespace: uid_map covers the whole id space")
	}
	if n, _ := strconv.Atoi(got["nprocs"]); n == 0 || n > 8 {
		t.Errorf("nprocs = %q, want a handful (own PID namespace)", got["nprocs"])
	}
	// From this process, outside the unit, the same call reaches the
	// kernel (EFAULT on the null params): otherwise "refused" above
	// could be a filter this test inherited rather than the unit's.
	if _, _, errno := syscall.Syscall(unix.SYS_IO_URING_SETUP, 1, 0, 0); errno == syscall.EPERM {
		sysctl, _ := os.ReadFile("/proc/sys/kernel/io_uring_disabled")
		t.Errorf("io_uring_setup is refused outside the unit too, so io_uring_setup=refused inside proves nothing about the unit's filter (kernel.io_uring_disabled=%s; anything but 0 can refuse it outside too)", strings.TrimSpace(string(sysctl)))
	}
	unit := h.state().Unit
	props := run(t, "systemctl", "--user", "show", unit, "-p", "PrivatePIDs,PrivateUsers,ProtectSystem,ProtectControlGroups,TemporaryFileSystem,BindPaths,BindReadOnlyPaths,InaccessiblePaths,LimitCORE,LimitCORESoft,SystemCallArchitectures")
	for _, want := range []string{"PrivatePIDs=yes", "PrivateUsers=yes", "ProtectControlGroups=yes", "TemporaryFileSystem=/:ro", "LimitCORE=0", "LimitCORESoft=0", "SystemCallArchitectures=native"} {
		if !strings.Contains(props, want) {
			t.Errorf("unit lacks %s:\n%s", want, props)
		}
	}
	// The filter the manager built from the allow list and the deny
	// entry after it: still an allow list (no leading ~), still holding
	// the rest of @aio — so io_uring_setup's absence is the deny entry's
	// doing, not a missing set — and without io_uring_setup.
	filter := strings.Fields(strings.TrimPrefix(strings.TrimSpace(run(t, "systemctl", "--user", "show", unit, "-p", "SystemCallFilter")), "SystemCallFilter="))
	if len(filter) == 0 || strings.HasPrefix(filter[0], "~") || !slices.Contains(filter, "io_uring_enter") || slices.Contains(filter, "io_uring_setup") {
		t.Errorf("SystemCallFilter is not @system-service less io_uring_setup: %v", filter)
	}
	// The retired half of the old model, read back off the live unit.
	for _, gone := range []string{"ProtectSystem=strict", "InaccessiblePaths=/"} {
		if strings.Contains(props, gone) {
			t.Errorf("unit still carries %s: the view names what exists, it does not mask a list\n%s", gone, props)
		}
	}
	if err := r.Stop(h, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// run executes a command and returns its stdout, failing the test on
// error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds pins
// the systemd semantics that decide WHERE a sandboxed unit's failure
// can be observed, and so what a synchronous caller is entitled to
// conclude from a start that returned cleanly.
//
// App units are Type=simple (unitProperties sets oneshot only for
// pre_start). A Type=simple start job completes as soon as the manager
// has forked — before the child sets its namespaces up and before it
// execs — so everything that can go wrong in the child arrives
// afterwards, through watch → finish, with the start job already
// reported "done".
//
// That is why the capability invalidation removed in this branch could
// not work: it hung off Start's and RunOnce's synchronous failure
// paths, and the failure it existed to catch (226/NAMESPACE, when a
// kernel or LSM change withdraws the namespaces under a live manager
// connection) never reaches them. A future round tempted to reinstate
// it should fail here first.
//
// A missing interpreter rather than broken namespaces: a failed exec
// (the sibling shell's, exit status 127 with "not found" in the
// journal) and 226/NAMESPACE both come from the same forked child
// after the same "done", and this one needs no host surgery to
// provoke. unitFor's own in-view check passes — the script is
// executable and inside the sandbox view — so the failure genuinely
// happens in the child.
func TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app:     "itest",
		version: "asyncfail",
		nonce:   "0a1b2c3d0a1b2c3d",
		command: []string{"./server"},
		dir:     release,
		env:     []string{"PATH=" + os.Getenv("PATH"), "HOME=" + shared},
		grace:   2 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("the start job reported this unit's failure synchronously: %v\n"+
			"If systemd now completes a Type=simple job only after exec, the asynchrony this pins is gone "+
			"and the sandbox capability cache could be invalidated from a launch path again.", err)
	}
	t.Cleanup(func() { _ = r.Stop(h, 2*time.Second) })

	// The unit is already doomed, and nothing synchronous saw it.
	select {
	case <-r.Wait(h):
	case <-time.After(30 * time.Second):
		t.Fatal("unit never reached a terminal state")
	}
	exit := h.(*systemdHandle).exit.Load()
	if exit == nil {
		t.Fatal("no exit recorded for a unit observed dead")
	}
	if exit.ExecMainStatus != 127 {
		t.Fatalf("ExecMainStatus = %d (%s), want 127 — the sibling shell's exec of the script failed (its interpreter not found), after the start job said done",
			exit.ExecMainStatus, exit.exitString())
	}
}

// TestIntegrationSystemdSIGTERMReachesNamespaceInit pins a property the
// full tier could plausibly have broken: with PrivatePIDs= the app is
// PID 1 of its own namespace, and the kernel discards signals sent to a
// namespace init from an ancestor namespace when the handler is SIG_DFL
// (the classic "docker stop takes ten seconds" behaviour). If that
// applied here, every cutover, drain and watchdog stop would silently
// wait out `grace` and end in SIGKILL,
// losing in-flight requests — the e2e cannot see it because its app is
// a Go binary, whose runtime installs handlers for every signal.
func TestIntegrationSystemdSIGTERMReachesNamespaceInit(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	// A shell that handles SIGTERM and takes its time about it, the way
	// a draining server does.
	body := "trap 'echo drained > " + shared + "/drained.txt; sleep 2; exit 0' TERM\nwhile :; do sleep 0.2; done\n"
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app: "itest", version: "sigterm", nonce: "0a1b2c3d0a1b2c3d", command: []string{"./server"}, dir: release,
		env: []string{"PATH=" + os.Getenv("PATH")}, grace: 10 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	started := time.Now()
	if err := r.Stop(h, spec.grace); err != nil {
		t.Fatalf("stop: %v", err)
	}
	took := time.Since(started)
	if _, err := os.Stat(filepath.Join(shared, "drained.txt")); err != nil {
		t.Fatalf("the app's SIGTERM handler never ran inside its PID namespace: %v", err)
	}
	if took >= spec.grace {
		t.Fatalf("stop took %s, the whole grace — SIGTERM was discarded and SIGKILL did the work", took)
	}
	t.Logf("SIGTERM honoured inside the PID namespace: handler ran, stop took %s of a %s grace", took, spec.grace)
}

// TestIntegrationSystemdCapsHoldAndChangeLive: the caps a unit starts
// with read back from the manager at the values given; SetCaps changes
// a running unit's with its process untouched, lifting what is now
// unset; a reattach reads them back. The kernel's own view (the
// cgroup files) is asserted where this lane's container delegates the
// controllers, and the e2e lane asserts it from inside the sandbox.
func TestIntegrationSystemdCapsHoldAndChangeLive(t *testing.T) {
	r := integrationRunner(t)
	spec := scriptApp(t, "exec sleep 300\n")
	spec.caps = resourceCaps{memoryMax: 64 << 20, tasksMax: 40, cpuQuota: 50}
	h, err := r.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(h, spec.grace) })
	unit := h.state().Unit
	show := func() string {
		return run(t, "systemctl", "--user", "show", unit, "-p", "MemoryMax,MemorySwapMax,TasksMax,CPUQuotaPerSecUSec")
	}
	expect := func(label string, wants ...string) {
		t.Helper()
		props := show()
		for _, want := range wants {
			if !strings.Contains(props, want+"\n") {
				t.Errorf("%s: unit lacks %s:\n%s", label, want, props)
			}
		}
	}
	expect("at start", "MemoryMax=67108864", "MemorySwapMax=0", "TasksMax=40", "CPUQuotaPerSecUSec=500ms")
	if got := h.state().Caps; got != spec.caps {
		t.Errorf("handle reports %+v, want the start's %+v", got, spec.caps)
	}
	cg := strings.TrimSpace(run(t, "systemctl", "--user", "show", "-p", "ControlGroup", "--value", unit))
	kernel := func(file string) string {
		b, err := os.ReadFile("/sys/fs/cgroup" + cg + "/" + file)
		if err != nil {
			return "absent"
		}
		return strings.TrimSpace(string(b))
	}
	delegated := kernel("memory.max") != "absent"
	if delegated {
		for file, want := range map[string]string{"memory.max": "67108864", "memory.swap.max": "0", "pids.max": "40", "cpu.max": "50000 100000"} {
			if got := kernel(file); got != want {
				t.Errorf("kernel %s = %q, want %q", file, got, want)
			}
		}
		if got := h.state().CapsUnenforced; len(got) != 0 {
			t.Errorf("every controller reaches the unit, yet the handle names %v as unenforced", got)
		}
	} else {
		t.Log("the memory controller does not reach the unit's cgroup in this container; the kernel's view is the e2e lane's to assert")
		// What the handle must say about it: the manager holds the
		// caps, the kernel enforces none of them.
		if got := h.state().CapsUnenforced; !slices.Contains(got, "memory_max") {
			t.Errorf("the memory controller is absent, yet the handle does not name memory_max as unenforced: %v", got)
		}
	}

	// A lifted tasks cap goes to the manager's own default, which is
	// what the manager reports for a unit that never had a TasksMax=
	// — never to infinity, which would lift the manager's bound too.
	def := strings.TrimSpace(run(t, "systemctl", "--user", "show", "-p", "DefaultTasksMax", "--value"))
	if def == "" || def == "infinity" {
		t.Fatalf("DefaultTasksMax = %q: the lift has no number to go to", def)
	}
	pid := h.state().PID
	next := resourceCaps{memoryMax: 128 << 20, cpuQuota: 25} // tasks_max lifted
	if err := r.SetCaps(h, next); err != nil {
		t.Fatalf("SetCaps: %v", err)
	}
	expect("after SetCaps", "MemoryMax=134217728", "MemorySwapMax=0", "TasksMax="+def, "CPUQuotaPerSecUSec=250ms")
	if got := h.state().Caps; got != next {
		t.Errorf("handle reports %+v after SetCaps, want %+v", got, next)
	}
	if got := h.state().PID; got != pid {
		t.Errorf("pid changed from %d to %d: the caps must apply without a restart", pid, got)
	}
	if delegated {
		for file, want := range map[string]string{"memory.max": "134217728", "pids.max": def, "cpu.max": "25000 100000"} {
			if got := kernel(file); got != want {
				t.Errorf("kernel %s = %q after SetCaps, want %q", file, got, want)
			}
		}
	}
	// A reattach reads the caps the unit carries, not the spec's.
	if again, ok, err := r.Reattach(h.state()); err != nil || !ok {
		t.Fatalf("reattach: ok=%v err=%v", ok, err)
	} else if got := again.state().Caps; got != next {
		t.Errorf("reattached handle reports %+v, want the unit's %+v", got, next)
	}
	if err := r.SetCaps(h, resourceCaps{}); err != nil {
		t.Fatalf("SetCaps (lift): %v", err)
	}
	expect("lifted", "MemoryMax=infinity", "MemorySwapMax=infinity", "TasksMax="+def, "CPUQuotaPerSecUSec=infinity")
	if delegated && kernel("memory.max") != "max" {
		t.Errorf("kernel memory.max = %q after lifting, want max", kernel("memory.max"))
	}
	if got := h.state().Caps; got.any() {
		t.Errorf("handle reports %+v after lifting, want none", got)
	}
}

// oneProcessApp is the shape #380 found: an app that is one process —
// no worker, no shell child — which traps TERM, takes a few hundred
// milliseconds of shell builtins to "drain", writes a marker and exits.
// It stays one process by blocking on a FIFO open, not on a sleep.
func oneProcessApp(t *testing.T, version string, grace time.Duration) (spec startSpec, shared string) {
	t.Helper()
	root, release, shared := sandboxRoot(t)
	if err := unix.Mkfifo(filepath.Join(shared, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "trap 'i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done; : > \"$SHARED/drained.txt\"; exit 0' TERM\n" +
		": > \"$SHARED/ready.txt\"\n" +
		"while :; do read x < \"$SHARED/fifo\"; done\n"
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return startSpec{
		app: "itest", version: version, nonce: "0a1b2c3d0a1b2c3d", command: []string{"./server"}, dir: release,
		env: []string{"PATH=" + os.Getenv("PATH"), "SHARED=" + shared}, grace: grace,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}, shared
}

// waitForFile polls for path, up to within.
func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s", path, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lostMainPIDRace reports whether the manager marked the unit's main
// pid as not its child — the race #380 describes, logged by the manager
// at the unit's start. A journal that cannot be read fails the test: a
// count of zero must mean no lost race, not no journal.
func lostMainPIDRace(t *testing.T, unit string) bool {
	t.Helper()
	out, err := exec.Command("journalctl", "-q", "-o", "cat", "--no-pager", "USER_UNIT="+unit).CombinedOutput()
	if err != nil {
		t.Fatalf("journalctl for %s: %v\n%s", unit, err, out)
	}
	return strings.Contains(string(out), "not our child")
}

// TestIntegrationSystemdOneProcessAppKeepsItsGrace: a one-process app
// gets its grace on every stop path. On systemd 257 the manager marks
// such a unit's main pid alien on half or more of its starts (the pid
// namespace's intermediate exits after sending the pid, and the manager
// may read the parent first), and then follows SIGTERM with SIGKILL at
// once when nothing else is in the cgroup; the sibling every unit
// carries (withSibling) is that something else (#380). Half the stops
// are motel's (Stop), half the manager's alone (`systemctl --user
// stop`: an operator's, or the shape of a reboot), which no code of
// motel's could reach. Before the sibling, 22 of 36 such stops lost
// their handler, every one of them a lost race; the run logs how many
// starts lost it, so the lane shows the race was exercised.
func TestIntegrationSystemdOneProcessAppKeepsItsGrace(t *testing.T) {
	r := integrationRunner(t)
	const n = 12
	lost := 0
	for i := 0; i < n; i++ {
		spec, shared := oneProcessApp(t, "oneproc"+strconv.Itoa(i), 10*time.Second)
		h, err := r.Start(spec)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		waitForFile(t, filepath.Join(shared, "ready.txt"), 5*time.Second)
		unit := h.state().Unit
		started := time.Now()
		how := "Stop"
		if i%2 == 1 {
			how = "systemctl --user stop"
			run(t, "systemctl", "--user", "stop", unit)
			waitDone(t, r, h)
		} else if err := r.Stop(h, spec.grace); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
		took := time.Since(started)
		raced := lostMainPIDRace(t, unit)
		if raced {
			lost++
		}
		if _, err := os.Stat(filepath.Join(shared, "drained.txt")); err != nil {
			t.Fatalf("instance %d, %s (main pid marked alien: %v): its SIGTERM handler did not finish — SIGKILL came with no grace (stop took %s)", i, how, raced, took)
		}
		if took >= spec.grace {
			t.Fatalf("instance %d, %s: took %s, the whole grace", i, how, took)
		}
	}
	t.Logf("%d of %d starts had the manager mark the main pid alien (\"not our child\"); every handler finished", lost, n)
}

// TestIntegrationSystemdUnitEnvironmentIsTheBuiltEnv: the sibling
// shell adds nothing to the app's environment. dash exports PWD, which
// the script unsets before the exec; invariant 8 of the runner says a
// unit's environment is exactly the one the caller built on the
// manager's defaults, and this is the one place a shell is in the way.
// The command is /usr/bin/env itself, not a script: a `#!/bin/sh` app
// would export PWD on its own account, which is its business.
func TestIntegrationSystemdUnitEnvironmentIsTheBuiltEnv(t *testing.T) {
	r := integrationRunner(t)
	root, release, shared := sandboxRoot(t)
	spec := startSpec{
		app: "itest", version: "envexact", nonce: "0a1b2c3d0a1b2c3d", command: []string{"/usr/bin/env"}, dir: release,
		env: []string{"PATH=" + os.Getenv("PATH"), "SHARED=" + shared}, grace: 5 * time.Second,
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "itest"), appName: "itest",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	unit, err := unitName(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.RunOnce(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// The app's stdout is its journal, which may be a moment behind.
	// _SYSTEMD_USER_UNIT is the field on the process's own lines; the
	// manager's lines about the unit carry USER_UNIT instead.
	var out string
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := exec.Command("journalctl", "-q", "-o", "cat", "--no-pager", "_SYSTEMD_USER_UNIT="+unit).CombinedOutput()
		if err != nil {
			t.Fatalf("journalctl for %s: %v\n%s", unit, err, b)
		}
		out = string(b)
		if strings.Contains(out, "SHARED="+shared) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(out, "SHARED="+shared) {
		t.Fatalf("the built env did not reach the app (or its journal):\n%s", out)
	}
	for _, kv := range strings.Split(out, "\n") {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "PWD", "OLDPWD", "SHLVL", "_":
			t.Errorf("the sibling shell left %q in the app's environment", kv)
		}
	}
}

// TestIntegrationSystemdPreStartKeepsItsGraceAtDeadline: a pre_start
// that runs past the unit's own TimeoutStartSec= is stopped by the
// manager with its grace, the sibling in its cgroup too. Before it, a
// one-process migration at its deadline was SIGKILLed at once.
func TestIntegrationSystemdPreStartKeepsItsGraceAtDeadline(t *testing.T) {
	r := integrationRunner(t)
	spec, shared := oneProcessApp(t, "predeadline", 5*time.Second)
	spec.startTimeout = 700 * time.Millisecond
	started := time.Now()
	err := r.RunOnce(context.Background(), spec)
	took := time.Since(started)
	if err == nil {
		t.Fatal("a pre_start past its deadline must fail")
	}
	if _, serr := os.Stat(filepath.Join(shared, "drained.txt")); serr != nil {
		t.Fatalf("the pre_start's SIGTERM handler did not finish: SIGKILL came with no grace (took %s, %v): %v", took, err, serr)
	}
	if took >= spec.startTimeout+spec.grace {
		t.Fatalf("took %s: the whole grace", took)
	}
	t.Logf("pre_start ended at its deadline with its grace: handler ran, took %s (%v)", took, err)
}

// TestIntegrationSystemdUnitOutlivesItsFile: a unit whose transient
// file the manager cannot read back after a daemon-reload — deleted
// here; in #380 an Environment= line past 1 MiB, or a root with a
// backslash — is still a running unit: the watcher keeps following it,
// Stop ends its process, and a sweep stops it as a stray. Before the
// fix such a unit read as exited: the watchdog relaunched beside it,
// Stop was a no-op and the sweep skipped it, the process running on.
func TestIntegrationSystemdUnitOutlivesItsFile(t *testing.T) {
	r := integrationRunner(t)
	body := "trap 'exit 0' TERM\nwhile :; do sleep 0.2; done\n"
	stopped, swept := scriptApp(t, body), scriptApp(t, body)
	stopped.version, swept.version = "nofile-stopped", "nofile-swept"
	hs, err := r.Start(stopped)
	if err != nil {
		t.Fatal(err)
	}
	hw, err := r.Start(swept)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	pidsStopped, pidsSwept := unitPIDs(t, hs.state().Unit), unitPIDs(t, hw.state().Unit)
	for _, h := range []handle{hs, hw} {
		if err := os.Remove(filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd", "transient", h.state().Unit)); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "systemctl", "--user", "daemon-reload")
	time.Sleep(4 * r.poll)
	for _, h := range []handle{hs, hw} {
		unit := h.state().Unit
		t.Logf("%s after the reload: %s", unit, strings.ReplaceAll(strings.TrimSpace(run(t, "systemctl", "--user", "show", unit, "-p", "LoadState,ActiveState")), "\n", " "))
		if !r.Alive(h) {
			t.Fatalf("%s was declared exited by the reload; its process runs on", unit)
		}
	}
	if err := r.Stop(hs, stopped.grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitPIDsGone(t, pidsStopped, 5*time.Second)
	if r.Alive(hs) {
		t.Fatal("stopped handle still alive")
	}
	// A second runner (a restarted motel) sees the other as a stray.
	r2 := integrationRunner(t)
	if err := r2.Sweep("itest", nil); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	waitPIDsGone(t, pidsSwept, 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for r.Alive(hw) && time.Now().Before(deadline) {
		time.Sleep(r.poll)
	}
	if r.Alive(hw) {
		t.Fatal("the swept unit's watcher never saw it go")
	}
}
