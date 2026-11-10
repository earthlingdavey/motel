package liveswap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallhoursorg/motel/liveswap/deploytrust"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeClock advances instantly on Sleep so pipeline tests never wait.
// After registers a waiter that Advance/Sleep fire once the fake time
// passes its deadline, which is how watchdog tests drive the loop.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(d)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advanceLocked(d)
}

func (c *fakeClock) advanceLocked(d time.Duration) {
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// fakeHandle is a runner handle whose liveness tests control. done
// mirrors execHandle's reaper channel: closed once the process "dies".
// Handles built as bare literals (no done channel) exercise the
// Wait-returns-nil polling fallback.
type fakeHandle struct {
	id    string
	alive bool
	exit  string // what Exit reports once dead
	done  chan struct{}
	// argv is what this handle was started with, reported back the way
	// the real runner reports the manager's ExecStart.
	argv []string
	// caps is what the "unit" is bounded to: the start spec's, then
	// whatever SetCaps last set, as the real handle reports them;
	// capsUnknown after a SetCaps the fake failed, as the real
	// handle's worst case (the unit unreadable after the failure).
	caps        resourceCaps
	capsUnknown bool
	mu          sync.Mutex
}

func (h *fakeHandle) state() handleState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return handleState{PID: 4242, Command: h.argv, Caps: h.caps, CapsUnknown: h.capsUnknown}
}

func (h *fakeHandle) isAlive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.alive
}

// dieQuietly makes the handle read as dead without closing done —
// the shape of a systemd unit whose exit the runner's state poll has
// not yet observed while health probes are already failing.
func (h *fakeHandle) dieQuietly() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = false
}

func (h *fakeHandle) kill() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = false
	if h.exit == "" {
		h.exit = "killed by signal 15 (terminated)" // what a Stop looks like from the runner
	}
	if h.done != nil {
		select {
		case <-h.done:
		default:
			close(h.done)
		}
	}
}

// fakeRunner records starts and stops; RunOnce failure is scriptable.
type fakeRunner struct {
	mu              sync.Mutex
	started         []startSpec
	handles         []*fakeHandle
	stopped         []handle
	runOnceErr      error
	runOnceCount    int
	startErr        error
	reattachOK      bool
	reattachErrs    []error // consumed one per Reattach call before reattachOK applies
	reattachCalls   int
	reattachSeen    []handleState
	stopErr         error  // Stop returns this
	stopLeavesAlive bool   // Stop does not actually kill the handle
	startDies       string // when set, Start's handle is already dead with this exit
	preflightErr    error  // Preflight returns this
	preflights      []startSpec
	sweepErr        error   // Sweep returns this
	sweepErrs       []error // consumed one per Sweep call before sweepErr applies
	sweeps          []handle
	setCaps         []resourceCaps // every SetCaps, in order
	setCapsErr      error          // SetCaps returns this (and sets nothing)
	// setCapsHook, when set, runs inside SetCaps after the call is
	// recorded and before the handle takes the caps, outside the
	// runner's lock: a test's chance to overlap another sync with it.
	setCapsHook func(resourceCaps)
}

func (r *fakeRunner) Start(spec startSpec) (handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return nil, r.startErr
	}
	h := &fakeHandle{id: fmt.Sprintf("h%d", len(r.handles)), alive: true, done: make(chan struct{}), argv: spec.command, caps: spec.caps}
	if r.startDies != "" {
		h.exit = r.startDies
		h.kill()
	}
	r.started = append(r.started, spec)
	r.handles = append(r.handles, h)
	return h, nil
}

func (r *fakeRunner) Preflight(spec startSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preflights = append(r.preflights, spec)
	return r.preflightErr
}

func (r *fakeRunner) Exit(h handle) (string, int) {
	fh, ok := h.(*fakeHandle)
	if !ok {
		return "", 0
	}
	if fh.isAlive() {
		return "", fh.state().PID
	}
	pid := fh.state().PID // state takes the handle's lock too: read it first
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.exit, pid
}

func (r *fakeRunner) RunOnce(_ context.Context, spec startSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started = append(r.started, spec)
	r.runOnceCount++
	return r.runOnceErr
}

func (r *fakeRunner) Alive(h handle) bool {
	fh, ok := h.(*fakeHandle)
	return ok && fh.isAlive()
}

func (r *fakeRunner) Stop(h handle, _ time.Duration) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, h)
	leave, serr := r.stopLeavesAlive, r.stopErr
	r.mu.Unlock()
	if fh, ok := h.(*fakeHandle); ok && !leave {
		fh.kill()
	}
	return serr
}

func (r *fakeRunner) Wait(h handle) <-chan struct{} {
	fh, ok := h.(*fakeHandle)
	if !ok || fh.done == nil {
		return nil
	}
	return fh.done
}

func (r *fakeRunner) SetCaps(h handle, caps resourceCaps) error {
	r.mu.Lock()
	if err := r.setCapsErr; err != nil {
		r.mu.Unlock()
		if fh, ok := h.(*fakeHandle); ok {
			fh.mu.Lock()
			fh.capsUnknown = true
			fh.mu.Unlock()
		}
		return err
	}
	r.setCaps = append(r.setCaps, caps)
	hook := r.setCapsHook
	r.mu.Unlock()
	if hook != nil {
		hook(caps)
	}
	if fh, ok := h.(*fakeHandle); ok {
		fh.mu.Lock()
		fh.caps = caps
		fh.capsUnknown = false
		fh.mu.Unlock()
	}
	return nil
}

func (r *fakeRunner) Reattach(st handleState) (handle, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reattachCalls++
	r.reattachSeen = append(r.reattachSeen, st)
	if len(r.reattachErrs) > 0 {
		err := r.reattachErrs[0]
		r.reattachErrs = r.reattachErrs[1:]
		return nil, false, err
	}
	// Nothing recorded is nothing to reattach to, as with the real
	// runner, which branches on the unit name alone; a record the
	// caller refused arrives here as zero.
	if !r.reattachOK || st.Unit == "" {
		return nil, false, nil
	}
	h := &fakeHandle{id: "reattached", alive: true}
	r.handles = append(r.handles, h)
	return h, true, nil
}

func (r *fakeRunner) reattachCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reattachCalls
}

func (r *fakeRunner) Sweep(_ string, keep handle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweeps = append(r.sweeps, keep)
	if len(r.sweepErrs) > 0 {
		err := r.sweepErrs[0]
		r.sweepErrs = r.sweepErrs[1:]
		return err
	}
	return r.sweepErr
}

// StopAll is recorded as a sweep keeping nothing. It spares nothing,
// as the real one: a scripted running migration is no error to it.
func (r *fakeRunner) StopAll(app string) error {
	err := r.Sweep(app, nil)
	if mr := (*migrationRunningError)(nil); errors.As(err, &mr) {
		return nil
	}
	return err
}

func (r *fakeRunner) lastSweep() handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sweeps[len(r.sweeps)-1]
}

func (r *fakeRunner) sweepCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sweeps)
}

func (r *fakeRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopped)
}

func (r *fakeRunner) setStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (r *fakeRunner) setPreflightErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preflightErr = err
}

// lastPreflightAndStart is the spec the last pre-flight checked and the
// one the last Start (or RunOnce) launched.
func (r *fakeRunner) lastPreflightAndStart() (preflight, started startSpec, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.preflights) == 0 || len(r.started) == 0 {
		return startSpec{}, startSpec{}, len(r.preflights)
	}
	return r.preflights[len(r.preflights)-1], r.started[len(r.started)-1], len(r.preflights)
}

func (r *fakeRunner) handleAt(i int) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[i]
}

// fakeProber approves or rejects the health gate by script. probeOnce
// (the watchdog path) consumes the result queue first, then repeats
// probeErr; both are settable while the watchdog goroutine runs.
type fakeProber struct {
	err  error
	bind bool // bind a real socket during the health gate, as the app would, and pin it as the real prober does

	mu           sync.Mutex
	probeErr     error
	probeResults []error
	probeCalls   int
	listeners    []net.Listener
}

func (p *fakeProber) waitHealthy(_ context.Context, sock *socketRef, alive func() bool, _ healthConfig) error {
	if p.bind {
		ln, err := listenSocket(sock.path)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.listeners = append(p.listeners, ln)
		p.mu.Unlock()
		if _, err := sock.dial(); err != nil {
			return err
		}
	}
	if !alive() {
		// What the real gate returns: the sentinel, with the scripted
		// probe alongside when the script is one.
		var pe *probeError
		errors.As(p.err, &pe)
		return &healthGateError{err: errProcessExited, probe: pe}
	}
	return p.err
}

func (p *fakeProber) closeListeners() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ln := range p.listeners {
		_ = ln.Close()
	}
	p.listeners = nil
}

func (p *fakeProber) probeOnce(_ context.Context, _ *socketRef, _ string, _ time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeCalls++
	if len(p.probeResults) > 0 {
		err := p.probeResults[0]
		p.probeResults = p.probeResults[1:]
		return err
	}
	return p.probeErr
}

func (p *fakeProber) setProbeErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probeErr = err
}

func (p *fakeProber) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probeCalls
}

// fakeFetcher materializes a release dir without any network. stats is
// what it reports the archive cost (zero unless a test sets it).
type fakeFetcher struct {
	err     error
	stats   archiveStats
	lastReq deployRequest
	// gate, when set, holds every fetch until the test sends its
	// outcome (nil: carry on), after telling inFetch: a deploy in
	// flight, holding deployMu, for as long as a test needs one.
	gate    chan error
	inFetch chan struct{}
}

// holdFetch makes the rig's deploys wait in fetch for the test. held
// waits until one is waiting there; release lets it go with err as the
// fetch's outcome (nil: the deploy proceeds).
func (rig *testRig) holdFetch(t *testing.T) (held func(), release func(error)) {
	rig.fetch.gate, rig.fetch.inFetch = make(chan error), make(chan struct{})
	held = func() {
		t.Helper()
		select {
		case <-rig.fetch.inFetch:
		case <-time.After(5 * time.Second):
			t.Fatal("no deploy reached fetch")
		}
	}
	release = func(err error) {
		t.Helper()
		select {
		case rig.fetch.gate <- err:
		case <-time.After(5 * time.Second):
			t.Fatal("no deploy is held in fetch")
		}
	}
	return held, release
}

func (f *fakeFetcher) fetch(_ context.Context, spec *appSpec, req deployRequest, progress func(string)) (string, archiveStats, error) {
	f.lastReq = req
	if !req.rollback { // as the real fetcher: a rollback fetches and extracts nothing
		progress("downloading")
		progress("extracting")
	}
	if f.gate != nil {
		f.inFetch <- struct{}{}
		if err := <-f.gate; err != nil {
			return "", archiveStats{}, err
		}
	}
	if f.err != nil {
		return "", archiveStats{}, f.err
	}
	dir := spec.dirs.release(req.version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", archiveStats{}, err
	}
	if req.rollback {
		return dir, archiveStats{}, nil // extracts nothing, like the real one
	}
	return dir, f.stats, nil
}

// fakeStore is an in-memory stateStore.
type fakeStore struct {
	mu    sync.Mutex
	state appState
	ok    bool
	err   error
	// onSave, when set, runs at each save with the state saved: a
	// test's look at what else was visible then. Set it before any
	// goroutine that saves is started.
	onSave func(appState)
}

func (s *fakeStore) load() (appState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.ok, s.err
}

func (s *fakeStore) save(st appState) error {
	if s.onSave != nil {
		s.onSave(st)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.ok = true
	return nil
}

// testSpec builds a fully-populated spec rooted in a temp dir.
func testSpec(t *testing.T) *appSpec {
	t.Helper()
	return &appSpec{
		name:               "demo",
		command:            []string{"./server", "--version", "{version}"},
		env:                map[string]string{"DATA": "{shared_dir}/db"},
		trust:              []deploytrust.Source{localSource(t, appTestPub, "demo")},
		healthPath:         "/health",
		healthInterval:     5 * time.Second,
		healthTimeout:      2 * time.Second,
		soak:               15 * time.Second,
		deadline:           5 * time.Minute,
		drain:              5 * time.Second,
		grace:              10 * time.Second,
		watchdogOn:         true,
		wdFailures:         3,
		wdGrace:            30 * time.Second,
		wdRestarts:         5,
		wdWindow:           10 * time.Minute,
		keep:               2,
		maxArtifactSize:    1 << 20,
		maxArtifactEntries: 1000,
		// The host the tests' artifact URLs name. The fake fetcher
		// consults no allowlist; a relaunch of the running version does
		// (relaunchRefusal), before it decides.
		allowlist: mustAllowlist(t, "x"),
		dirs:      newAppDirs(shortTempDir(t), "demo"),
	}
}

type testRig struct {
	ma     *managedApp
	runner *fakeRunner
	prober *fakeProber
	fetch  *fakeFetcher
	clock  *fakeClock
	store  *fakeStore
	spec   *appSpec
}

// recordedNonce is the instance nonce the state.json fixtures record.
const recordedNonce = "0a1b2c3d0a1b2c3d"

// activeSocketIs reports whether the proxy is routed to the recorded
// instance's socket.
func activeSocketIs(rig *testRig) bool {
	s := rig.ma.activeSocket.Load()
	return s != nil && s.path == rig.spec.dirs.socket(recordedNonce)
}

// socketFiles lists the instance sockets present under the app's run
// dir, by nonce.
func socketFiles(t *testing.T, rig *testRig) []string {
	t.Helper()
	entries, err := os.ReadDir(rig.spec.dirs.run)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && nonceRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// pinnedFiles lists the pinned names under the app's proxy dir, by nonce.
func pinnedFiles(t *testing.T, rig *testRig) []string {
	t.Helper()
	return socketNames(t, rig.spec.dirs.proxy)
}

func socketNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".sock"); ok {
			out = append(out, n)
		}
	}
	return out
}

// listenSocket binds a real unix socket at path, as a running app
// does; closing the listener leaves the file, as an app's exit does
// (motel removes it with the instance's run dir: socketRef.retire).
func listenSocket(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	return ln, nil
}

// bindSocket is listenSocket for a test's own fixture.
func bindSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := listenSocket(path)
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
}

// touchSocket plants a stale socket file the way a dead app leaves one.
func touchSocket(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o750))
	must(t, os.WriteFile(path, nil, 0o600))
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	rig := &testRig{
		runner: &fakeRunner{},
		prober: &fakeProber{},
		fetch:  &fakeFetcher{},
		clock:  newFakeClock(),
		store:  &fakeStore{},
		spec:   testSpec(t),
	}
	ma := newManagedApp("demo")
	ma.spec = rig.spec
	ma.verifiers = deploytrust.Verifiers(rig.spec.trust, nil)
	ma.runner = rig.runner
	ma.prober = rig.prober
	ma.fetch = rig.fetch
	ma.clock = rig.clock
	ma.store = rig.store
	ma.logger = zap.NewNop()
	ma.wdCtx, ma.wdCancel = context.WithCancel(context.Background())
	t.Cleanup(ma.wdCancel)
	t.Cleanup(rig.prober.closeListeners)
	rig.ma = ma
	return rig
}

// An artifact past 75% of either cap warns on the deploy that got
// there, naming the directive, and the figures reach the status
// record; below that, and on a rollback, nothing is said.
func TestDeployWarnsNearArtifactCaps(t *testing.T) {
	cases := []struct {
		name  string
		stats archiveStats
		warns int
	}{
		{"well under", archiveStats{entries: 100, bytes: 1 << 16}, 0},
		{"entries at 75%", archiveStats{entries: 750, bytes: 1 << 16}, 1},
		{"bytes at 75%", archiveStats{entries: 100, bytes: 7_864_320}, 1}, // 0.75 × 1 MiB × 10
		{"both", archiveStats{entries: 999, bytes: 10 << 20}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			core, logs := observer.New(zap.WarnLevel)
			rig.ma.logger = zap.New(core)
			rig.fetch.stats = tc.stats
			must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
			if got := logs.Len(); got != tc.warns {
				t.Fatalf("warned %d times, want %d: %v", got, tc.warns, logs.All())
			}
			last := rig.ma.status().LastDeploy
			if last.ArtifactEntries != tc.stats.entries || last.ArtifactBytes != tc.stats.bytes {
				t.Fatalf("status did not record the artifact figures: %+v", last)
			}
			// A rollback extracts nothing: no figures, no warning, even
			// though the fake would report the same stats.
			must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}))
			logs.TakeAll()
			must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
			if logs.Len() != 0 {
				t.Fatalf("rollback warned: %v", logs.All())
			}
		})
	}
}

func TestDeployFirstVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if rig.ma.activeSocket.Load() == nil {
		t.Fatal("activeSocket not published after deploy")
	}
	st, ok, _ := rig.store.load()
	if !ok || st.CurrentVersion != "v1" {
		t.Fatalf("state not persisted: %+v ok=%v", st, ok)
	}
	if rig.runner.stopCount() != 0 {
		t.Fatal("nothing should be stopped on a first deploy")
	}
	status := rig.ma.status()
	if status.CurrentVersion != "v1" || !status.Running || status.Phase != "idle" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.LastDeploy == nil || status.LastDeploy.Status != "succeeded" {
		t.Fatalf("last deploy not recorded: %+v", status.LastDeploy)
	}
}

func TestDeploySecondVersionStopsOldAfterDrain(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()
	before := rig.clock.Now()

	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"}))
	if rig.ma.activeSocket.Load() == sockV1 {
		t.Fatal("cutover did not change the active socket")
	}
	if rig.runner.stopCount() != 1 {
		t.Fatalf("old instance not stopped exactly once: %d", rig.runner.stopCount())
	}
	if got := rig.ma.status().CurrentVersion; got != "v2" {
		t.Fatalf("current version = %s, want v2", got)
	}
	// Drain must have elapsed on the clock before the old stop.
	if rig.clock.Now().Sub(before) < rig.spec.drain {
		t.Fatal("drain period was not observed")
	}
}

func TestDeployPreStartFailureKeepsOldServing(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()

	rig.spec.preStart = []string{"./migrate"}
	rig.runner.runOnceErr = errors.New("migration exploded")
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || rig.ma.activeSocket.Load() != sockV1 {
		t.Fatalf("pre_start failure must abort and keep the old socket routed; err=%v", err)
	}
	if got := rig.ma.status(); got.CurrentVersion != "v1" || !got.Running {
		t.Fatalf("old version must keep serving: %+v", got)
	}
	if got := rig.ma.status().LastDeploy; got.Status != "failed" || got.Phase != "preparing" {
		t.Fatalf("failure not recorded with phase: %+v", got)
	}
}

// migrationRunner is a fakeRunner whose pre_start runs until the test
// releases it, the way a migration takes its time. Like the real
// runner it stops the unit when its context ends first, and says so.
type migrationRunner struct {
	*fakeRunner
	entered   chan context.Context
	release   chan struct{}
	completed atomic.Bool
}

func (r *migrationRunner) RunOnce(ctx context.Context, spec startSpec) error {
	r.entered <- ctx
	select {
	case <-r.release:
		r.completed.Store(true)
		return r.fakeRunner.RunOnce(ctx, spec)
	case <-ctx.Done():
		return ctx.Err() // the unit stopped mid-migration
	}
}

// Promise: a client that hangs up mid-pre_start does not stop the
// migration. pre_start runs under deadline, not under the request; it
// finishes, and the deploy then fails at the health gate, which reads
// the request's context as it does for any deploy whose client left.
// The old instance serves throughout, and the record names the gate,
// not the migration.
func TestDeployPreStartOutlivesTheRequest(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()

	runner := &migrationRunner{fakeRunner: rig.runner, entered: make(chan context.Context), release: make(chan struct{})}
	rig.ma.runner = runner
	rig.ma.prober = &httpProber{clock: rig.clock} // the real gate: it is what reads the request after pre_start
	rig.spec.preStart = []string{"./migrate"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"}) }()

	var preCtx context.Context
	select {
	case preCtx = <-runner.entered:
	case err := <-errc:
		t.Fatalf("the deploy ended before pre_start ran: %v", err)
	}
	cancel() // the CI job is cancelled mid-migration
	if err := preCtx.Err(); err != nil {
		t.Fatalf("pre_start's context ended with the request's: %v", err)
	}
	if dl, ok := preCtx.Deadline(); !ok || time.Until(dl) > rig.spec.deadline {
		t.Fatalf("pre_start must still be bounded by deadline (%v): deadline %v, set %v", rig.spec.deadline, dl, ok)
	}
	close(runner.release)

	err := <-errc
	if !runner.completed.Load() {
		t.Fatal("the migration did not run to completion")
	}
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "health gate") || strings.Contains(err.Error(), "pre_start") {
		t.Fatalf("want the health gate to fail the deploy on the cancelled request, got %v", err)
	}
	if rig.ma.activeSocket.Load() != sockV1 {
		t.Fatal("the old instance must keep serving")
	}
	if got := rig.ma.status().LastDeploy; got.Status != "failed" || got.Phase != "soaking" || strings.Contains(got.Error, "pre_start") {
		t.Fatalf("the record must name the gate, not the migration: %+v", got)
	}
}

func TestDeployHealthFailureStopsNewKeepsOld(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	sockV1 := rig.ma.activeSocket.Load()

	rig.prober.err = errors.New("never became healthy")
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
	if err == nil {
		t.Fatal("expected health-gate failure")
	}
	if rig.ma.activeSocket.Load() != sockV1 {
		t.Fatal("failed deploy must not move traffic")
	}
	// v1 still alive, v2 stopped: exactly one stop, and current still v1.
	if rig.runner.stopCount() != 1 {
		t.Fatalf("new instance should be stopped once, got %d stops", rig.runner.stopCount())
	}
	if got := rig.ma.status(); got.CurrentVersion != "v1" || !got.Running {
		t.Fatalf("old version must keep serving: %+v", got)
	}
}

// An unpinned deploy of the running version is refused: with no digest
// there is nothing to match the artifact by (a matching digest would
// relaunch it — app_relaunch_test.go).
func TestDeployRejectsSameRunningVersionWithoutAPin(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected validationError, got %v", err)
	}
}

func TestDeployConcurrentGets409Error(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"})
	if !errors.Is(err, errDeployInProgress) {
		t.Fatalf("expected errDeployInProgress, got %v", err)
	}
}

// deployAt deploys version through the rig and dates its deploy record
// minute minutes after base (stampRecord says why).
func deployAt(t *testing.T, rig *testRig, version string, base time.Time, minute int) {
	t.Helper()
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/" + version, version: version}))
	stampRecord(t, rig.spec.dirs, version, base.Add(time.Duration(minute)*time.Minute))
}

func TestDeployGCKeepsNewestReleases(t *testing.T) {
	rig := newTestRig(t)
	// keep=2 in testSpec; deploy three versions, each recorded a
	// minute after the last, so the GC inside the third deploy has v3
	// (its own) and v2 to keep.
	base := time.Now().Add(-time.Hour)
	for i, v := range []string{"v1", "v2", "v3"} {
		deployAt(t, rig, v, base, i)
	}
	entries, err := os.ReadDir(rig.spec.dirs.releases)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 2 releases kept, got %v", names)
	}
	if _, err := os.Stat(rig.spec.dirs.release("v3")); err != nil {
		t.Fatal("current release must survive GC")
	}
	if _, err := os.Stat(rig.spec.dirs.release("v2")); err != nil {
		t.Fatal("the release deployed before it must survive GC: it is the way back")
	}
}

// mtimeSettingFetcher is the fake fetcher, after which the new
// release's dir gets the mtime its pre_start or instance could give it
// before release GC runs: the release being started is bound
// read-write into its sandbox. The zero time leaves it alone.
type mtimeSettingFetcher struct {
	*fakeFetcher
	mtime time.Time
}

func (f mtimeSettingFetcher) fetch(ctx context.Context, spec *appSpec, req deployRequest, progress func(string)) (string, archiveStats, error) {
	dir, stats, err := f.fakeFetcher.fetch(ctx, spec, req, progress)
	if err == nil && !f.mtime.IsZero() {
		err = os.Chtimes(dir, f.mtime, f.mtime)
	}
	return dir, stats, err
}

// Adversarial: the app can set any mtime on the release it runs from —
// the running instance on its own dir, an earlier version's instance on
// what is now a sibling's, the next version's pre_start or instance on
// its own before GC — and none of it moves available_versions or what
// the next deploy's GC prunes. With keep 3 and v1..v3 deployed in that
// order, deploying v4 prunes v1 and only v1, and lists v4 v3 v2. With
// a release from before records (v0, no record) on disk too and keep 4,
// v0 is the one pruned whatever mtime it is given — 2099 included: a
// release with no record outranks no recorded one.
func TestReleaseOrderIgnoresMtimesTheAppSets(t *testing.T) {
	future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	epoch := time.Unix(0, 0)
	marks := map[string]time.Time{"untouched": {}, "future": future, "epoch": epoch}
	names := []string{"untouched", "future", "epoch"}
	for _, legacy := range []bool{false, true} {
		siblings := []string{"v1", "v2"}
		before, pruned, after := []string{"v3", "v2", "v1"}, "v1", []string{"v4", "v3", "v2"}
		var beforeByMtime []string
		if legacy {
			siblings = append(siblings, "v0")
			before, pruned, after = []string{"v3", "v2", "v1", "v0"}, "v0", []string{"v4", "v3", "v2", "v1"}
			beforeByMtime = []string{"v0"}
		}
		for _, own := range names {
			for _, sibling := range siblings {
				for _, sib := range names {
					for _, next := range names {
						t.Run(fmt.Sprintf("legacy=%t/v3=%s/%s=%s/v4=%s", legacy, own, sibling, sib, next), func(t *testing.T) {
							rig := newTestRig(t)
							rig.spec.keep = len(before)
							base := time.Now().Add(-time.Hour)
							if legacy {
								must(t, os.MkdirAll(rig.spec.dirs.release("v0"), 0o750))
								old := base.Add(-time.Hour)
								must(t, os.Chtimes(rig.spec.dirs.release("v0"), old, old))
							}
							for i, v := range []string{"v1", "v2", "v3"} {
								deployAt(t, rig, v, base, i)
							}
							for v, mark := range map[string]time.Time{"v3": marks[own], sibling: marks[sib]} {
								if !mark.IsZero() {
									must(t, os.Chtimes(rig.spec.dirs.release(v), mark, mark))
								}
							}
							if got := rig.ma.status(); !slices.Equal(got.AvailableVersions, before) || !slices.Equal(got.OrderedByMtime, beforeByMtime) {
								t.Fatalf("before v4: available_versions = %v, ordered_by_mtime = %v", got.AvailableVersions, got.OrderedByMtime)
							}
							rig.ma.fetch = mtimeSettingFetcher{fakeFetcher: rig.fetch, mtime: marks[next]}
							must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/v4", version: "v4"}))
							onDisk, err := releaseNames(rig.spec.dirs.releases)
							must(t, err)
							want := slices.DeleteFunc(slices.Clone(before), func(v string) bool { return v == pruned })
							want = append(want, "v4")
							slices.Sort(onDisk)
							slices.Sort(want)
							if !slices.Equal(onDisk, want) {
								t.Fatalf("GC left %v, want %v (%s pruned)", onDisk, want, pruned)
							}
							if got := rig.ma.status(); !slices.Equal(got.AvailableVersions, after) || got.OrderedByMtime != nil {
								t.Fatalf("after v4: available_versions = %v, ordered_by_mtime = %v", got.AvailableVersions, got.OrderedByMtime)
							}
						})
					}
				}
			}
		}
	}
}

// A release with no deploy record sorts after every recorded one, by
// its dir's mtime among its kind, and the status names it so: that one
// key is the app's to move.
func TestStatusNamesReleasesPlacedByMtime(t *testing.T) {
	rig := newTestRig(t)
	deployAt(t, rig, "v2", time.Now().Add(-time.Hour), 0)
	must(t, os.MkdirAll(rig.spec.dirs.release("v1"), 0o750)) // deployed before records existed
	old := time.Now().Add(-2 * time.Hour)
	must(t, os.Chtimes(rig.spec.dirs.release("v1"), old, old))
	got := rig.ma.status()
	if !slices.Equal(got.AvailableVersions, []string{"v2", "v1"}) || !slices.Equal(got.OrderedByMtime, []string{"v1"}) {
		t.Fatalf("available_versions = %v, ordered_by_mtime = %v", got.AvailableVersions, got.OrderedByMtime)
	}
	body, err := json.Marshal(got)
	must(t, err)
	if !strings.Contains(string(body), `"ordered_by_mtime":["v1"]`) {
		t.Fatalf("status JSON lacks ordered_by_mtime: %s", body)
	}
}

// A rollback that fails writes its version's record, newest of all,
// and that deploy never served: it takes no place from a version that
// did. keep 3, v1..v3 deployed, a failed rollback to v1, then v4: v4
// and v3 are held by rank, and the third place is v2's, not v1's.
func TestAFailedRollbackTakesNoPlaceFromAVersionThatServed(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.keep = 3
	base := time.Now().Add(-time.Hour)
	for i, v := range []string{"v1", "v2", "v3"} {
		deployAt(t, rig, v, base, i)
	}
	rig.prober.err = errors.New("never became healthy")
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err == nil {
		t.Fatal("the rollback to v1 should have failed its health gate")
	}
	rig.prober.err = nil
	if got := rig.ma.status().AvailableVersions; !slices.Equal(got, []string{"v3", "v2", "v1"}) {
		t.Fatalf("available_versions = %v, want v3 v2 v1: the failed v1 last", got)
	}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/v4", version: "v4"}))
	onDisk, err := releaseNames(rig.spec.dirs.releases)
	must(t, err)
	slices.Sort(onDisk)
	if !slices.Equal(onDisk, []string{"v2", "v3", "v4"}) {
		t.Fatalf("GC left %v, want v2 v3 v4 (v1, whose latest deploy failed, pruned)", onDisk)
	}
}

// A rollback whose record write is lost (a crash after the cutover, a
// full disk) leaves its version's record at the deploy before: the
// next deploy's GC still keeps that version, since it is the one the
// new deploy replaced. keep 2: v1, v2, a rollback to v1 whose record
// keeps v1's first time, then v3 — v1 and v3 stay.
func TestALostRollbackRecordDoesNotPruneTheVersionThatServed(t *testing.T) {
	rig := newTestRig(t)
	base := time.Now().Add(-time.Hour)
	deployAt(t, rig, "v1", base, 0)
	deployAt(t, rig, "v2", base, 1)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}))
	stampRecord(t, rig.spec.dirs, "v1", base) // the rollback's record never landed
	// The status agrees with the GC to come: v1 is running, so it is
	// listed first, whatever its record says.
	if got := rig.ma.status().AvailableVersions; !slices.Equal(got, []string{"v1", "v2"}) {
		t.Fatalf("available_versions = %v, want v1 v2: the running version first", got)
	}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/v3", version: "v3"}))
	onDisk, err := releaseNames(rig.spec.dirs.releases)
	must(t, err)
	slices.Sort(onDisk)
	if !slices.Equal(onDisk, []string{"v1", "v3"}) {
		t.Fatalf("GC left %v, want v1 v3: v1 is the way back from v3", onDisk)
	}
}

// The status lists the running version first under the lock that
// gives current_version: polled while rollbacks cut over between v1
// and v2, every snapshot that lists the running version lists it
// first, whichever side of a cutover its disk read fell on.
func TestStatusListsTheRunningVersionFirstThroughCutovers(t *testing.T) {
	rig := newTestRig(t)
	base := time.Now().Add(-time.Hour)
	deployAt(t, rig, "v1", base, 0)
	deployAt(t, rig, "v2", base, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			v := []string{"v1", "v2"}[i%2]
			if err := rig.ma.Deploy(context.Background(), deployRequest{version: v, rollback: true}); err != nil {
				t.Errorf("rollback to %s: %v", v, err)
				return
			}
		}
	}()
	// Errorf and break, never Fatalf: the rollbacks go on until done,
	// and a goroutine that logs after its test ended panics the binary.
	for polling := true; polling; {
		select {
		case <-done:
			polling = false
		default:
		}
		s := rig.ma.status()
		if s.CurrentVersion != "" && slices.Contains(s.AvailableVersions, s.CurrentVersion) && s.AvailableVersions[0] != s.CurrentVersion {
			t.Errorf("current_version %s, available_versions %v: the running version not first", s.CurrentVersion, s.AvailableVersions)
			break
		}
	}
	<-done
}

// With only proxy/ a link, the prune removes nothing — not even the
// app's own leftovers under run/, which it checks first and would
// otherwise remove before the refusal.
func TestPruneRemovesNothingWhenEitherDirIsALink(t *testing.T) {
	root := shortTempDir(t)
	shop := newAppDirs(root, "shop")
	must(t, shop.ensure())
	demo := newAppDirs(root, "demo")
	must(t, os.MkdirAll(demo.run, 0o750))
	must(t, os.Symlink(shop.proxy, demo.proxy))
	const stale = "5e5e5e5e5e5e5e5e"
	touchSocket(t, demo.socket(stale))
	pruneSockets(demo, nil, zap.NewNop())
	if _, err := os.Lstat(demo.runDir(stale)); err != nil {
		t.Fatalf("run/ was pruned before proxy/'s refusal: %v", err)
	}
}

func TestEnsureRunningRelaunchesFromState(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rig.ma.ensureRunning(); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if rig.ma.activeSocket.Load() == nil {
		t.Fatal("recovered instance not published")
	}
	if got := rig.ma.status().CurrentVersion; got != "v7" {
		t.Fatalf("recovered version = %s, want v7", got)
	}
	if len(rig.runner.started) != 1 {
		t.Fatalf("expected exactly one Start, got %d", len(rig.runner.started))
	}
	// A relaunch creates the dirs it binds, run/ included: the runner
	// resolves every bind source and a missing one refuses the launch.
	for _, d := range []string{rig.spec.dirs.run, rig.spec.dirs.shared} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("relaunch must create %s: %v", d, err)
		}
	}
}

func TestEnsureRunningMissingBinaryIsPermanent(t *testing.T) {
	// The runner refuses a launch whose command is not there (or not
	// executable, or outside the view) with a preflightError. That is
	// the release as shipped: no retry changes it, so recovery must
	// classify it permanent and stop, not warn once a minute forever.
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	rig.runner.setStartErr(&preflightError{"exec: \"server\": executable file not found"})
	err := rig.ma.ensureRunning()
	if err == nil || transientRecovery(err) {
		t.Fatalf("a missing binary must be a permanent recovery error, got %v", err)
	}
	if !strings.Contains(err.Error(), "relaunching v7") {
		t.Fatalf("the error must name the version it could not relaunch: %v", err)
	}
}

func TestEnsureRunningReattachesWhenRunnerCan(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, rig.ma.ensureRunning())
	if !activeSocketIs(rig) {
		t.Fatalf("reattach must keep the recorded socket, got %v", rig.ma.activeSocket.Load())
	}
	if len(rig.runner.started) != 0 {
		t.Fatal("reattach must not start a new process")
	}
}

func TestEnsureRunningNoStateIsNoop(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.ensureRunning())
	if rig.ma.activeSocket.Load() != nil || len(rig.runner.started) != 0 {
		t.Fatal("nothing should happen without persisted state")
	}
}

func TestEnsureRunningSkipsWhenAlreadyAlive(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	started := len(rig.runner.started)
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.started) != started {
		t.Fatal("ensureRunning must be a no-op while the instance is alive (reload case)")
	}
}

func TestDestructStopsCurrentInstance(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 1 {
		t.Fatalf("Destruct must stop the running instance, got %d stops", rig.runner.stopCount())
	}
}

func TestBuildEnvPrecedenceAndPlaceholders(t *testing.T) {
	spec := testSpec(t)
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("# comment\nexport FROM_FILE=yes\nOVERRIDE=\"file\"\n\n"), 0o600))
	spec.envFile = envFile
	spec.env = map[string]string{"OVERRIDE": "inline", "DB": "sqlite:{shared_dir}/app.db", "V": "{version}:{socket}"}

	sock := spec.dirs.socket("0a1b2c3d0a1b2c3d")
	env, err := buildEnv(spec, "v9", sock, spec.dirs.release("v9"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, kv := range env {
		k, v := stringsCut(kv)
		byKey[k] = v // later entries win, matching exec env semantics
	}
	for k, want := range map[string]string{
		"FROM_FILE": "yes",
		"OVERRIDE":  "inline",
		"DB":        "sqlite:" + spec.dirs.shared + "/app.db",
		"V":         "v9:" + sock,
		"SOCKET":    sock,
	} {
		if byKey[k] != want {
			t.Errorf("%s = %q, want %q", k, byKey[k], want)
		}
	}
	for _, gone := range []string{"PORT", "HOST"} {
		if v, ok := byKey[gone]; ok {
			t.Errorf("%s=%q is set: the contract is the socket, nothing listens on a port", gone, v)
		}
	}
}

// A NOTIFY_ variable in an app's env_file is a notify channel's URL,
// which the box writes there from secrets/<app>/NOTIFY_*.age: withheld
// from the unit's environment, and still a value the filter knows.
func TestBuildEnvWithholdsNotifyVariables(t *testing.T) {
	spec := testSpec(t)
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("DATABASE_URL=\"postgres://db\"\nNOTIFY_BLOG_URL=\"https://ntfy.sh/blog-topic\"\n"), 0o600))
	spec.envFile = envFile
	env, secrets, _, err := buildEnvFull(spec, "v1", spec.dirs.socket("0a1b2c3d0a1b2c3d"), spec.dirs.release("v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "NOTIFY_") {
			t.Errorf("%s reached the unit's environment", kv)
		}
	}
	if !slices.Contains(env, "DATABASE_URL=postgres://db") {
		t.Errorf("the file's other values are withheld too: %v", env)
	}
	if !slices.Contains(secrets, "NOTIFY_BLOG_URL=https://ntfy.sh/blog-topic") {
		t.Errorf("a withheld value is no longer a known value to the filter: %v", secrets)
	}
}

func TestBuildEnvDoesNotLeakSupervisorSecrets(t *testing.T) {
	t.Setenv("ACME_DNS_API_TOKEN", "hunter2")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "tok")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LC_ALL", "C.UTF-8")

	env, err := buildEnv(testSpec(t), "v1", "/var/lib/liveswap/demo/run/0a1b2c3d0a1b2c3d.sock", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, kv := range env {
		k, v := stringsCut(kv)
		byKey[k] = v
	}
	for _, k := range []string{"ACME_DNS_API_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		if _, leaked := byKey[k]; leaked {
			t.Errorf("%s must not leak from the supervisor env into apps", k)
		}
	}
	if byKey["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q, want inherited /usr/bin:/bin", byKey["PATH"])
	}
	if byKey["LC_ALL"] != "C.UTF-8" {
		t.Errorf("LC_ALL = %q, want inherited C.UTF-8", byKey["LC_ALL"])
	}
}

func stringsCut(kv string) (string, string) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
}

func TestParseEnvFileRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.env")
	must(t, os.WriteFile(p, []byte("NOT A VAR LINE\n"), 0o600))
	if _, err := parseEnvFile(p); err == nil {
		t.Fatal("expected error for malformed env file")
	}
}

// The box writes env files with RenderEnvFile; every launch reads them
// with parseEnvLines. A value must come back byte for byte, whatever
// the quoting, spacing or punctuation in it; what the format cannot
// carry is refused, never altered.
func TestRenderEnvFileRoundTrips(t *testing.T) {
	values := []string{
		"plain", "", `"x"`, `"`, `'`, `''`, `a"b`, " lead", "trail ", "  both  ",
		"\xc2\xa0nbsp\xc2\xa0", "\xe2\x80\xa8", "\t", "# not a comment", "a=b=c",
		"export X=1", "$HOME", "`cmd`", "\\n", "emoji \xf0\x9f\x94\x91",
	}
	for i, v := range values {
		vars := []string{"Z_LAST=z", fmt.Sprintf("K%d=%s", i, v), "A=a"}
		out, err := RenderEnvFile(vars)
		if err != nil {
			t.Fatalf("RenderEnvFile(%q): %v", vars, err)
		}
		got, err := ParseEnvLines("rendered", out)
		if err != nil {
			t.Fatalf("ParseEnvLines(%q): %v", out, err)
		}
		want := []string{"A=a", fmt.Sprintf("K%d=%s", i, v), "Z_LAST=z"}
		if !slices.Equal(got, want) {
			t.Fatalf("value %q: rendered %q, read back %q", v, out, got)
		}
	}
}

func TestRenderEnvFileSortsByKeyNotByLine(t *testing.T) {
	out, err := RenderEnvFile([]string{"A9=x", "A=y", "A_B=z"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "A=\"y\"\nA9=\"x\"\nA_B=\"z\"\n"; string(out) != want {
		t.Fatalf("rendered %q, want %q", out, want)
	}
}

func TestRenderEnvFileRefusesWhatTheFormatCannotCarry(t *testing.T) {
	for _, c := range []struct {
		vars []string
		want string
	}{
		{[]string{"K=a\nb"}, "line break"},
		{[]string{"K=a\r"}, "line break"},
		{[]string{"K=a\x00"}, "NUL"},
		{[]string{"K=\xff"}, "not UTF-8"},
		{[]string{"1K=v"}, "not a valid environment variable name"},
		{[]string{"my-var=v"}, "not a valid environment variable name"},
		{[]string{"=v"}, "not a valid environment variable name"},
		{[]string{"novalue"}, "not a valid environment variable name"},
		{[]string{"OPTIND=2"}, "set by the shell"},
		{[]string{"K=a", "L=b", "K=c"}, "K is given twice"},
	} {
		_, err := RenderEnvFile(c.vars)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("RenderEnvFile(%q) = %v, want an error naming %q", c.vars, err, c.want)
		}
	}
}

func TestWarnEnvFileMode(t *testing.T) {
	cases := []struct {
		name  string
		mode  os.FileMode
		warns []string // message substrings, in order
	}{
		{"the documented 0640 is quiet", 0o640, nil},
		{"0600 is quiet", 0o600, nil},
		{"group bits are not the check's business", 0o660, nil},
		{"world-readable warns once", 0o644, []string{"world-readable"}},
		{"world-writable without world-readable warns once, for the write", 0o622, []string{"world-writable"}},
		{"world-readable and -writable warns for each", 0o666, []string{"world-readable", "world-writable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "app.env")
			must(t, os.WriteFile(p, []byte("KEY=value\n"), 0o600))
			must(t, os.Chmod(p, tc.mode)) // WriteFile's mode is subject to the umask; Chmod is not
			core, logs := observer.New(zap.WarnLevel)
			warnEnvFileMode(zap.New(core), map[string]*appSpec{"blog": {envFile: p}})
			got := logs.All()
			if len(got) != len(tc.warns) {
				t.Fatalf("warned %d times, want %d: %v", len(got), len(tc.warns), got)
			}
			for i, want := range tc.warns {
				fields := got[i].ContextMap()
				if !strings.Contains(got[i].Message, want) || fields["app"] != "blog" || fields["env_file"] != p || fields["mode"] != fmt.Sprintf("%04o", tc.mode) || fields["fix"] == nil {
					t.Errorf("warning %d = %q %v, want message containing %q for app blog, env_file %s, mode %04o, with a fix", i, got[i].Message, fields, want, p, tc.mode)
				}
			}
		})
	}
	t.Run("an absent file is silent: loading the config does not open it", func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		warnEnvFileMode(zap.New(core), map[string]*appSpec{"blog": {envFile: filepath.Join(t.TempDir(), "missing.env")}})
		if logs.Len() != 0 {
			t.Fatalf("an absent env_file warned: %v", logs.All())
		}
	})
	t.Run("a file that cannot be checked is reported as such, never as fine", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "file")
		must(t, os.WriteFile(notADir, nil, 0o600))
		p := filepath.Join(notADir, "app.env")
		core, logs := observer.New(zap.WarnLevel)
		warnEnvFileMode(zap.New(core), map[string]*appSpec{"blog": {envFile: p}})
		got := logs.All()
		if len(got) != 1 || got[0].Level != zap.WarnLevel || !strings.Contains(got[0].Message, "could not be checked") || got[0].ContextMap()["app"] != "blog" || got[0].ContextMap()["env_file"] != p {
			t.Fatalf("a stat that fails with ENOTDIR must warn, naming the app and the file, that the mode went unchecked, got %v", got)
		}
	})
	t.Run("a file this account cannot reach is noted at info level, never reported as fine", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reaches everything, so EACCES cannot be produced here")
		}
		dir := filepath.Join(t.TempDir(), "closed")
		must(t, os.Mkdir(dir, 0o700))
		p := filepath.Join(dir, "app.env")
		must(t, os.WriteFile(p, []byte("KEY=value\n"), 0o600))
		must(t, os.Chmod(dir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		core, logs := observer.New(zap.InfoLevel)
		warnEnvFileMode(zap.New(core), map[string]*appSpec{"blog": {envFile: p}})
		got := logs.All()
		if len(got) != 1 || got[0].Level != zap.InfoLevel || !strings.Contains(got[0].Message, "not checked") || got[0].ContextMap()["app"] != "blog" || got[0].ContextMap()["env_file"] != p {
			t.Fatalf("EACCES at load must be noted at info level, naming the app and the file, got %v", got)
		}
	})
	t.Run("a launch warns from the descriptor it read, through the logger it was given", func(t *testing.T) {
		rig := newTestRig(t)
		p := filepath.Join(t.TempDir(), "app.env")
		must(t, os.WriteFile(p, []byte("KEY=value\n"), 0o600))
		must(t, os.Chmod(p, 0o644))
		rig.spec.envFile = p
		core, logs := observer.New(zap.WarnLevel)
		l, err := rig.spec.prepareLaunch(zap.New(core), "v1")
		must(t, err)
		if l.envFileMode != 0o644 || logs.FilterMessageSnippet("world-readable").Len() != 1 {
			t.Fatalf("prepareLaunch recorded mode %04o and warned %d times, want 0644 and 1: %v", l.envFileMode, logs.Len(), logs.All())
		}
	})
	t.Run("a deploy warns too, so a chmod after load is not silent until the next reload", func(t *testing.T) {
		rig := newTestRig(t)
		p := filepath.Join(t.TempDir(), "app.env")
		must(t, os.WriteFile(p, []byte("KEY=value\n"), 0o600))
		must(t, os.Chmod(p, 0o644))
		rig.spec.envFile = p
		core, logs := observer.New(zap.WarnLevel)
		rig.ma.logger = zap.New(core)
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
		if n := logs.FilterMessageSnippet("world-readable").Len(); n != 1 {
			t.Fatalf("the deploy warned %d times about a world-readable env_file, want 1: %v", n, logs.All())
		}
	})
	warnEnvFileMode(nil, map[string]*appSpec{"blog": {envFile: "/x"}}) // a nil logger is skipped, not dereferenced
}

func TestHostOfStripsUserinfoCredentials(t *testing.T) {
	cases := map[string]string{
		"https://example.com/artifact.tar.gz":             "example.com",
		"https://example.com:8443/a?token=x":              "example.com:8443",
		"https://user:s3cr3t@example.com/artifact.tar.gz": "example.com",
		"https://ci-token@host:443/x":                     "host:443",
		"not a url":                                       "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
		if got := hostOf(in); strings.Contains(got, "@") || strings.Contains(got, "s3cr3t") || strings.Contains(got, "ci-token") {
			t.Errorf("hostOf(%q) = %q leaks credentials", in, got)
		}
	}
}

func TestValidVersionRejectsDotPrefix(t *testing.T) {
	for _, v := range []string{"v1", "1.2.3", "release_2024-01-01", "v1..2", "a.", "_", "-"} {
		if !validVersion(v) {
			t.Errorf("validVersion(%q) = false, want true", v)
		}
	}
	// A leading dot is refused as a whole class: "." and ".." resolve
	// onto the releases dir or the app root, ".extract-1" is deleted by
	// release GC as a staging orphan, and any other dot-name is
	// invisible to it. See versionRe.
	for _, v := range []string{".", "..", "..foo", "...", ".extract-1", ".v1", "", "has/slash", "with space", "..\x00"} {
		if validVersion(v) {
			t.Errorf("validVersion(%q) = true, want false (path-unsafe)", v)
		}
	}
}

func TestExpandArgs(t *testing.T) {
	spec := testSpec(t)
	got := expandArgs([]string{"run", "--rel={release_dir}", "{version}", "--listen={socket}"}, spec, "v2", "/run/x.sock", "/rel/v2")
	if got[1] != "--rel=/rel/v2" || got[2] != "v2" || got[3] != "--listen=/run/x.sock" {
		t.Fatalf("placeholders not expanded: %v", got)
	}
}

// shortTempDir is t.TempDir without the test name in the path: a
// liveswap root the tests bind sockets under, where a long test name
// would push <root>/demo/proxy/<nonce>.sock past sun_path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ls")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// swapSeam installs f for the duration of the test and restores the
// previous value after. Atomically both ways: App.Start's unknown-app
// sweep reads these seams from a goroutine nothing joins, so a restore
// can land while a sweep started by an earlier test is still reading.
func swapSeam[T any](t *testing.T, seam *atomic.Pointer[T], f T) {
	t.Helper()
	prev := seam.Swap(&f)
	t.Cleanup(func() { seam.Store(prev) })
}

func TestRollbackSkipsPreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"./migrate"}

	// A normal deploy runs pre_start.
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("deploy should run pre_start once, got %d", rig.runner.runOnceCount)
	}

	// Deploy a second version so v1 is no longer the running one.
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("second deploy should run pre_start, got %d", rig.runner.runOnceCount)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to v1: %v", err)
	}
	if rig.runner.runOnceCount != 2 {
		t.Fatalf("rollback must not run pre_start; count = %d, want 2", rig.runner.runOnceCount)
	}
}

func TestDeployRejectsExistingVersion(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// Deploy a second version so v1 is on disk but not running.
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// Re-deploying v1 (URL) must be rejected — versions are immutable.
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	var vErr validationError
	if !errors.As(err, &vErr) || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("re-deploy of existing version should be a validation error about immutability, got %v", err)
	}
	// But rollback to v1 (which exists) is allowed.
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err != nil {
		t.Fatalf("rollback to existing v1 should succeed, got %v", err)
	}
}

func TestFailedDeployCleansUpRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails its health gate.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err == nil {
		t.Fatal("deploy v2 should have failed the health gate")
	}
	// Its freshly-extracted release must be gone.
	if _, err := os.Stat(rig.spec.dirs.release("v2")); !os.IsNotExist(err) {
		t.Fatalf("failed deploy should remove its release dir, stat err = %v", err)
	}
	// So the same version is retriable once healthy.
	rig.prober.err = nil
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("re-deploy of a cleaned-up failed version should succeed: %v", err)
	}
}

func TestFailedRollbackKeepsRelease(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	// A rollback to v1 that fails its health gate must NOT delete v1's
	// pre-existing release.
	rig.prober.err = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{version: "v1", rollback: true}); err == nil {
		t.Fatal("rollback should have failed the health gate")
	}
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err != nil {
		t.Fatalf("failed rollback must not delete the on-disk release: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("body read boom") }

func TestStageUploadClassifiesErrors(t *testing.T) {
	var se *stagingError

	// A body-read failure is a client fault — bare error, not stagingError.
	if _, _, err := stageUpload(errReader{}, t.TempDir(), 100); err == nil || errors.As(err, &se) {
		t.Fatalf("body-read error should be a bare (client) error, got %v", err)
	}
	// A local filesystem failure (tmpDir is actually a file → MkdirAll
	// fails) is a server fault — *stagingError.
	notADir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stageUpload(strings.NewReader("data"), notADir, 100); !errors.As(err, &se) {
		t.Fatalf("a local FS failure should be a *stagingError (server), got %v", err)
	}
	// Happy path still works, and the digest is the staged bytes'.
	sum := sha256.Sum256([]byte("data"))
	if _, digest, err := stageUpload(strings.NewReader("data"), t.TempDir(), 100); err != nil || digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("valid upload should stage with its digest: %q %v", digest, err)
	}
}

func TestFailedDeployKeepsReleaseWhenStopUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v2 fails the health gate, and Stop can't confirm the instance
	// exited (it stays alive) — the release must NOT be deleted beneath it.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
	if err == nil {
		t.Fatal("deploy v2 should have failed")
	}
	if !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("error should note the release was left in place: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must not be deleted under a still-running failed instance: %v", statErr)
	}
}

func TestFailedDeployKeepsReleaseWhenStopErrorsEvenIfDead(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	// v2 fails the health gate and Stop reports an error even though
	// the handle then reads as dead. Under cgroup kill "Stop errored"
	// is the only signal a caller gets, so the release stays on disk.
	rig.prober.err = errTest
	rig.runner.stopErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("expected the release to be kept: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must survive an unconfirmed stop: %v", statErr)
	}
}

func TestDeployStopOldErrorDefersToSweep(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// Stopping v2 errors, but the sweep — the runner's own ledger —
	// vouches that only v3 remains, so keep=2 GC proceeds: our memory
	// of a failed stop is not the source of truth, the manager is.
	rig.runner.stopErr = errTest
	rig.runner.stopLeavesAlive = true
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v3"}))
	if rig.runner.lastSweep() != rig.runner.handleAt(2) {
		t.Fatal("the pre-GC sweep must keep the just-promoted instance")
	}
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err == nil {
		t.Fatal("with the sweep vouching, GC runs as usual")
	}
	if rig.ma.currentInstance().version != "v3" {
		t.Fatal("the deploy itself succeeds regardless")
	}
}

// Promise: the nonce in the start spec is the one that names the
// socket, so the unit (named from the spec) and the socket (in the
// env) can be checked against each other on reattach.
func TestStartSpecNonceNamesTheSocket(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	spec := rig.runner.started[0]
	if !nonceRe.MatchString(spec.nonce) {
		t.Fatalf("start spec nonce %q", spec.nonce)
	}
	want := "SOCKET=" + rig.spec.dirs.socket(spec.nonce)
	if spec.env[len(spec.env)-1] != want {
		t.Fatalf("env ends with %q, want %q", spec.env[len(spec.env)-1], want)
	}
	inst := rig.ma.currentInstance()
	if inst.nonce != spec.nonce || inst.socket != rig.spec.dirs.socket(spec.nonce) {
		t.Fatalf("instance %+v does not carry the spec's nonce", inst)
	}
	if st, _, _ := rig.store.load(); st.Nonce != inst.nonce {
		t.Fatalf("state.json records nonce %q, want %q", st.Nonce, inst.nonce)
	}
	if s := rig.ma.status(); s.Socket != inst.socket {
		t.Fatalf("status reports %q, want %q", s.Socket, inst.socket)
	}
}

// Promise: a confirmed sweep is the manager's word that nothing but
// the kept instance runs, so every other socket under run/ is a
// leftover and is removed. Files that are not instance sockets are
// not ours to touch.
func TestSweepPrunesSocketsNotHeldByTheManager(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	must(t, rig.spec.dirs.ensure())
	touchSocket(t, rig.spec.dirs.socket("aaaaaaaaaaaaaaaa"))
	must(t, os.WriteFile(filepath.Join(rig.spec.dirs.run, "junk.txt"), nil, 0o600))
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	cur := rig.ma.currentInstance()
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != cur.nonce {
		t.Fatalf("sockets after deploy = %v, want only the current instance's %s", got, cur.nonce)
	}
	if _, err := os.Stat(filepath.Join(rig.spec.dirs.run, "junk.txt")); err != nil {
		t.Fatal("a file that is not an instance socket must be left alone")
	}
	// An unconfirmed sweep vouches for nothing: nothing is pruned, and
	// the aborted launch takes its own dir with it.
	touchSocket(t, rig.spec.dirs.socket("bbbbbbbbbbbbbbbb"))
	rig.runner.sweepErr = errTest
	_ = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	got := socketFiles(t, rig)
	slices.Sort(got)
	if want := []string{"bbbbbbbbbbbbbbbb", cur.nonce}; !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("sockets after an unconfirmed sweep = %v, want %v", got, want)
	}
}

// Promise: an instance's socket is gone once the instance is stopped —
// retiring the instance removes its run dir (the unit itself removes
// nothing: it has one Exec*=, #311), and the deploy's sweep would
// prune it anyway: the old one after a cutover, the new one after a
// failed health gate.
func TestStopRemovesTheInstanceSocket(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	ctx := context.Background()
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
	v1 := rig.ma.currentInstance()
	if _, err := os.Stat(v1.socket); err != nil {
		t.Fatalf("the bound socket should exist: %v", err)
	}

	rig.prober.err = errors.New("never became healthy")
	if err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"}); err == nil {
		t.Fatal("expected the health gate to fail")
	}
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != v1.nonce {
		t.Fatalf("after a failed gate only v1's socket remains, got %v", got)
	}

	rig.prober.err = nil
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/3", version: "v3"}))
	v3 := rig.ma.currentInstance()
	if got := socketFiles(t, rig); len(got) != 1 || got[0] != v3.nonce {
		t.Fatalf("after a cutover only v3's socket remains, got %v", got)
	}
	if _, err := os.Stat(v1.socket); !os.IsNotExist(err) {
		t.Fatalf("v1's socket must be removed after it is stopped: %v", err)
	}
}

// Promise: the recorded nonce names the socket the proxy will dial, so
// a record is adopted only when the nonce is the recorded unit's own
// and the socket it names is there. Anything else relaunches rather
// than routing to a socket that answers nobody.
func TestEnsureRunningRefusesARecordWhoseSocketItCannotDial(t *testing.T) {
	for name, tc := range map[string]struct {
		nonce   string
		bind    bool
		symlink bool
	}{
		"nonce is not the unit's":  {nonce: "ffffffffffffffff", bind: true},
		"socket file is gone":      {nonce: recordedNonce},
		"the name is a symlink":    {nonce: recordedNonce, symlink: true},
		"the name is a plain file": {nonce: recordedNonce, bind: false, symlink: false},
		// A nonce that is not one is never a path component: the
		// decoy it would escape to is planted, and must survive.
		"the nonce is a traversal": {nonce: "../../target"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.runner.reattachOK = true
			socket := rig.spec.dirs.socket(tc.nonce)
			decoy := filepath.Join(rig.spec.dirs.root, "target.sock")
			switch {
			case tc.bind:
				bindSocket(t, socket)
			case tc.symlink:
				victim := filepath.Join(t.TempDir(), "admin.sock")
				bindSocket(t, victim)
				must(t, os.MkdirAll(filepath.Dir(socket), 0o750))
				must(t, os.Symlink(victim, socket))
			case name == "the name is a plain file":
				touchSocket(t, socket)
			case name == "the nonce is a traversal":
				touchSocket(t, decoy)
			}
			rig.store.state = appState{CurrentVersion: "v7", Nonce: tc.nonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
			rig.store.ok = true
			must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
			must(t, rig.ma.ensureRunning())
			if _, err := os.Stat(decoy); name == "the nonce is a traversal" && err != nil {
				t.Fatalf("a traversal in the recorded nonce reached outside the app's dirs: %v", err)
			}
			if len(rig.runner.reattachSeen) != 1 || rig.runner.reattachSeen[0].Unit != "" {
				t.Fatalf("the record must not reach the manager: %+v", rig.runner.reattachSeen)
			}
			if rig.runner.startCount() != 1 {
				t.Fatal("the recorded version is relaunched instead")
			}
			if s := rig.ma.activeSocket.Load(); s == nil || s.path == rig.spec.dirs.socket(tc.nonce) {
				t.Fatalf("the recorded socket must never be routed: %v", s)
			}
		})
	}
	// And a record whose nonce is the unit's, with the socket bound, is
	// adopted as-is.
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("a record whose socket is the unit's own is adopted, starts=%d", rig.runner.startCount())
	}
	if got := rig.ma.currentInstance(); got.nonce != recordedNonce {
		t.Fatalf("the adopted instance carries the recorded nonce, got %+v", got)
	}
}

// Promise: an app removed from the config leaves no socket behind.
func TestDestructRemovedAppLeavesNoSocket(t *testing.T) {
	rig := newTestRig(t)
	rig.prober.bind = true
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	touchSocket(t, rig.spec.dirs.socket("aaaaaaaaaaaaaaaa"))
	touchSocket(t, filepath.Join(rig.spec.dirs.proxy, "bbbbbbbbbbbbbbbb.sock"))
	must(t, rig.ma.Destruct())
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("sockets after removal = %v, want none", got)
	}
	if got := pinnedFiles(t, rig); len(got) != 0 {
		t.Fatalf("pinned names after removal = %v, want none", got)
	}
}

func TestDestructOnRemovalSweepsWholeApp(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 1 {
		t.Fatalf("Destruct must stop the tracked instance, got %d stops", rig.runner.stopCount())
	}
	// Deploy swept twice (pre-start, pre-GC); removal sweeps everything (keep=nil).
	if n := rig.runner.sweepCount(); n != 3 || rig.runner.lastSweep() != nil {
		t.Fatalf("removal must sweep the whole app with keep=nil, sweeps=%d last=%v", n, rig.runner.sweeps)
	}
}

// liveHandles is every instance the runner started that still runs.
func (r *fakeRunner) liveHandles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var live []string
	for _, h := range r.handles {
		if h.isAlive() {
			live = append(live, h.id)
		}
	}
	return live
}

// Removing an app waits for a deploy of it in flight. A reload stops
// the old config's servers without waiting for their requests, so
// without the wait a deploy past Destruct's sweep would start a unit
// nothing supervises, routes or stops. Whatever the deploy comes to,
// nothing of the app is left running once Destruct returns, and the
// removed app takes no deploy after.
func TestDestructWaitsForADeployInFlight(t *testing.T) {
	cases := []struct {
		name     string
		fetchErr error // the held deploy's fetch outcome
		gateErr  error // its health gate's
	}{
		{"the deploy promotes", nil, nil},
		{"the deploy fails its health gate", nil, errors.New("health check returned 500")},
		{"the deploy fails before any launch", errors.New("download failed"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			core, logs := observer.New(zap.WarnLevel)
			rig.ma.logger = zap.New(core)
			markLive(t)
			deployV1(t, rig) // serving: Destruct has an instance of its own to stop
			rig.prober.err = tc.gateErr
			held, release := rig.holdFetch(t)
			deployed := make(chan error, 1)
			go func() {
				deployed <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
			}()
			held()
			destructed := make(chan error, 1)
			go func() { destructed <- rig.ma.Destruct() }()
			// The wait is named: a reload that hangs on it says why.
			waitUntil(t, "Destruct to name its wait", func() bool {
				return logs.FilterMessageSnippet("waiting for the deploy to end").Len() == 1
			})
			select {
			case err := <-destructed:
				t.Fatalf("Destruct returned (%v) while a deploy of the app was in flight", err)
			default:
			}
			release(tc.fetchErr)
			for _, ch := range []chan error{deployed, destructed} {
				select {
				case <-ch: //nolint:errcheck // whatever each comes to: the test judges what is left running
				case <-time.After(5 * time.Second):
					t.Fatal("the deploy and then Destruct must both end")
				}
			}
			if live := rig.runner.liveHandles(); len(live) != 0 {
				t.Fatalf("instances left running after the app's removal: %v", live)
			}
			if rig.runner.lastSweep() != nil {
				t.Fatal("removal must end with a sweep of the whole app")
			}
			if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/3", version: "v3"}); !errors.Is(err, errDeployInProgress) {
				t.Fatalf("a removed app must take no deploy, got %v", err)
			}
		})
	}
}

// On process exit Destruct does not wait for a deploy: the units stay
// for the next start to reattach, and shutdown is not held up.
func TestDestructOnExitDoesNotWaitForADeploy(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	deployV1(t, rig)
	held, release := rig.holdFetch(t)
	deployed := make(chan error, 1)
	go func() {
		deployed <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	}()
	held()
	swapSeam(t, &caddyExitingSeam, func() bool { return true })
	destructed := make(chan error, 1)
	go func() { destructed <- rig.ma.Destruct() }()
	select {
	case err := <-destructed:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Destruct on exit must not wait for a deploy")
	}
	release(nil)
	must(t, <-deployed)
	if rig.runner.stopCount() != 1 || len(rig.runner.liveHandles()) != 1 {
		t.Fatalf("the deploy runs on to its end, and its instance is kept: stops %d, live %v", rig.runner.stopCount(), rig.runner.liveHandles())
	}
}

func TestDestructLeavesInstanceRunningOnProcessExit(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	swapSeam(t, &caddyExitingSeam, func() bool { return true })
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 {
		t.Fatal("on process exit the unit must be left running for reattach")
	}
	if !rig.runner.Alive(rig.runner.handleAt(0)) {
		t.Fatal("instance must still be alive after Destruct on exit")
	}
}

func TestStartSpecCarriesUnitIdentity(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"true"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	if rig.runner.startCount() != 2 {
		t.Fatalf("expected pre_start + start, got %d", rig.runner.startCount())
	}
	for i := range 2 {
		s := rig.runner.started[i]
		if s.app != "demo" || s.version != "v1" || s.grace != rig.spec.grace {
			t.Fatalf("startSpec %d lacks unit identity: %+v", i, s)
		}
	}
}

func TestEnsureRunningUnreadableReattachIsTransientAndLaunchesNothing(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest}
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) || !strings.Contains(err.Error(), "not relaunching") {
		t.Fatalf("expected a transient refusal, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activeSocket.Load() != nil {
		t.Fatal("must not launch or publish while the recorded unit's state is unknown")
	}
	// The pin made before asking the manager survives a transient
	// answer: the unit may be serving, and the app may since have
	// replaced its own name, so the retry must adopt through the pin.
	if got := pinnedFiles(t, rig); len(got) != 1 || got[0] != recordedNonce {
		t.Fatalf("a transient reattach must keep the pin, got %v", got)
	}
	must(t, os.Remove(rig.spec.dirs.socket(recordedNonce)))
	rig.runner.reattachOK = true
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("the retry must adopt through the surviving pin, starts=%d", rig.runner.startCount())
	}
}

// Recovery holds the app's ownerLock for its whole run, so a
// start-time prune of this app's dirs (a reload adding it back races
// the sweep of apps no longer configured) can never interleave with
// adopting them. Pinned from the other side: recovery waits for the
// lock — its own app's, another app's held lock is nothing to it.
func TestEnsureRunningWaitsForTheOwnershipLock(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))

	otherApp := ownerLock("somebody-else")
	otherApp.Lock()
	defer otherApp.Unlock()
	mu := ownerLock("demo")
	mu.Lock()
	done := make(chan error, 1)
	go func() { done <- rig.ma.ensureRunning() }()
	time.Sleep(50 * time.Millisecond)
	if rig.ma.activeSocket.Load() != nil {
		mu.Unlock()
		t.Fatal("recovery adopted while the app's ownership lock was held")
	}
	mu.Unlock()
	must(t, <-done)
	if !activeSocketIs(rig) {
		t.Fatal("recovery adopts once the lock is free")
	}
}

// A definitive "not running" from the manager, by contrast, retires
// the pin made for the record: the relaunch gets its own.
func TestEnsureRunningNotAttachedRetiresThePin(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning()) // reattachOK is false: definitively not running
	if rig.runner.startCount() != 1 {
		t.Fatalf("the recorded version is relaunched, starts=%d", rig.runner.startCount())
	}
	for _, n := range pinnedFiles(t, rig) {
		if n == recordedNonce {
			t.Fatal("the pin made for a unit the manager does not hold must be retired")
		}
	}
}

func TestRecoverRetriesTransientErrorsUntilReattached(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest, errTest}
	rig.runner.reattachOK = true
	done := make(chan struct{})
	go func() { rig.ma.recover(context.Background(), zap.NewNop()); close(done) }()
	advanceUntil(t, rig, recoveryBackoffFloor, "reattach after two transient failures", func() bool {
		return activeSocketIs(rig)
	})
	<-done
	if rig.runner.startCount() != 0 || !activeSocketIs(rig) {
		t.Fatalf("expected a reattach on the third try, starts=%d socket=%v", rig.runner.startCount(), rig.ma.activeSocket.Load())
	}
	if rig.runner.reattachCalls != 3 {
		t.Fatalf("expected 3 reattach attempts, got %d", rig.runner.reattachCalls)
	}
	if rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != rig.runner.handleAt(0) {
		t.Fatal("recovery must sweep stray units, keeping the adopted one")
	}
}

// Rule 4 (redact.go): the version state.json names becomes a path
// component, an argument and an environment value, so one that is not
// a version is refused before anything is looked up or launched — a
// permanent error, like corrupt state, never a silent reset — even
// with a release directory of that name planted for it to find.
func TestEnsureRunningRefusesAPlantedVersion(t *testing.T) {
	for _, bad := range []string{"../../etc", "a/b", "v1\n", strings.Repeat("v", 65), ".hidden", "v 1"} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			rig := newTestRig(t)
			rig.store.state = appState{CurrentVersion: bad, Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
			rig.store.ok = true
			must(t, os.MkdirAll(rig.spec.dirs.release(bad), 0o755))
			err := rig.ma.ensureRunning()
			if err == nil || transientRecovery(err) {
				t.Fatalf("a planted version must be a permanent recovery error, got %v", err)
			}
			if rig.runner.startCount() != 0 || len(rig.runner.reattachSeen) != 0 {
				t.Fatalf("a planted version reached the runner: starts=%d reattach=%+v", rig.runner.startCount(), rig.runner.reattachSeen)
			}
		})
	}
}

func TestRecoverGivesUpOnPermanentErrors(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true // release dir deliberately missing: not something a retry fixes
	done := make(chan struct{})
	go func() { rig.ma.recover(context.Background(), zap.NewNop()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recover must return on a permanent error, not retry forever")
	}
}

// A recovery that gave up says why in status until the cause is gone:
// with state.json removed there is nothing to recover and nothing
// failing, so the next recovery clears the reason.
func TestRecoveryWithNothingRecordedClearsAGivenUpReason(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true // the release dir missing: recovery gives up
	rig.startWatchdogT(t)
	rig.ma.recover(context.Background(), zap.NewNop())
	waitUntil(t, "status to name why recovery gave up", func() bool {
		wd := rig.ma.status().Watchdog
		return wd != nil && strings.Contains(wd.LastFailure, "release dir is missing")
	})
	rig.store.mu.Lock()
	rig.store.state, rig.store.ok = appState{}, false
	rig.store.mu.Unlock()
	must(t, rig.ma.ensureRunning())
	if wd := rig.ma.status().Watchdog; wd.LastFailure != "" {
		t.Fatalf("nothing recorded is nothing failing: %+v", wd)
	}
}

func TestEnsureRunningReattachSweepFailureIsTransient(t *testing.T) {
	rig := newTestRig(t)
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachOK = true
	rig.runner.sweepErr = errTest
	err := rig.ma.ensureRunning()
	if !transientRecovery(err) {
		t.Fatalf("an unsettled ledger after reattach must be reported as transient, got %v", err)
	}
	if !activeSocketIs(rig) {
		t.Fatal("the reattached instance still serves meanwhile")
	}
	// The retry path (instance alive) sweeps again and succeeds.
	rig.runner.sweepErr = nil
	must(t, rig.ma.ensureRunning())
	if rig.runner.sweepCount() != 2 || rig.runner.lastSweep() != rig.runner.handleAt(0) {
		t.Fatalf("retry must sweep keeping the live instance, sweeps=%v", rig.runner.sweeps)
	}
}

func TestCleanupJoinsRecoveryBeforeReleasing(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.reattachErrs = []error{errTest, errTest, errTest, errTest, errTest, errTest}
	// What App.Start does for its apps: recovery owned by the config.
	a := &App{logger: zap.NewNop(), managed: map[string]*managedApp{"demo": rig.ma}}
	ctx, cancel := context.WithCancel(context.Background())
	a.recoverCancel = cancel
	a.recoverWG = new(sync.WaitGroup)
	a.recoverWG.Add(1)
	exited := make(chan struct{})
	go func() { defer a.recoverWG.Done(); rig.ma.recover(ctx, zap.NewNop()); close(exited) }()
	waitUntil(t, "first attempt", func() bool { return rig.runner.reattachCount() >= 1 })
	// The config is cleaned up while recovery is parked in backoff:
	// Cleanup must end it before anything else happens.
	must(t, a.Cleanup())
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery must have exited by the time Cleanup returns")
	}
	if rig.runner.startCount() != 0 {
		t.Fatal("nothing may be launched by a cancelled recovery")
	}
}

// markLive stands in for a started liveswap config being live for the
// rest of the test (what App.Start/Cleanup maintain).
func markLive(t *testing.T) {
	t.Helper()
	liveStartedApps.Add(1)
	t.Cleanup(func() { liveStartedApps.Add(-1) })
}

func TestDestructOnUnconfiguredAppIsANoop(t *testing.T) {
	// A pooled app whose config's Start failed before configuring it
	// (the manager probe, on a reload adding the app) has no runner.
	markLive(t)
	ma := newManagedApp("fresh")
	must(t, ma.Destruct())
}

func TestConfigureInstallsTheVerifiersItIsGiven(t *testing.T) {
	// configure installs the set Provision built and warmed, rather
	// than resolving one of its own.
	rig := newTestRig(t)
	spec := testSpec(t)
	spec.trust = []deploytrust.Source{oidcSource(t, "https://issuer.example")}
	warmed := deploytrust.Verifiers(spec.trust, nil)
	rig.ma.configure(new(int), spec, warmed, zap.NewNop(), &fetchClients{}, userManager)
	got := rig.ma.currentVerifiers()
	if len(got) != 1 || got[0] != warmed[0] {
		t.Fatalf("configure must install the verifiers it was handed (the warmed ones); got %v want %v", got, warmed)
	}
}

func TestRollbackConfigRestoresTheServingDefinition(t *testing.T) {
	rig := newTestRig(t)
	clients := &fetchClients{}
	specA, specB := testSpec(t), testSpec(t)
	specB.grace = 99 * time.Second
	ownerA, ownerB := new(int), new(int)
	rig.ma.configure(ownerA, specA, deploytrust.Verifiers(specA.trust, nil), zap.NewNop(), clients, userManager)
	rig.ma.configure(ownerB, specB, deploytrust.Verifiers(specB.trust, nil), zap.NewNop(), clients, userManager)
	// A successful reload: A is cleaned up after B configured — not
	// the last writer, nothing happens.
	if rig.ma.rollbackConfig(ownerA) || rig.ma.snapshot().spec != specB {
		t.Fatal("a replaced config must not roll back the newer one")
	}
	// A rejected candidate: B is cleaned up while A still serves.
	if !rig.ma.rollbackConfig(ownerB) || rig.ma.snapshot().spec != specA {
		t.Fatal("a rejected candidate must restore the serving definition")
	}
	if rig.ma.rollbackConfig(ownerB) {
		t.Fatal("rollback is one-shot")
	}
}

func TestRollbackConfigWakesTheWatchdog(t *testing.T) {
	rig := newTestRig(t)
	clients := &fetchClients{}
	on, off := testSpec(t), testSpec(t)
	off.watchdogOn = false
	ownerA, ownerB := new(int), new(int)
	rig.ma.configure(ownerA, on, deploytrust.Verifiers(on.trust, nil), zap.NewNop(), clients, userManager)
	rig.ma.configure(ownerB, off, deploytrust.Verifiers(off.trust, nil), zap.NewNop(), clients, userManager)
	// Drain the pokes configure sent, then roll back: the rollback
	// itself must poke, or a parked loop never re-reads watchdog=on.
	for len(rig.ma.wdNotify) > 0 {
		<-rig.ma.wdNotify
	}
	if !rig.ma.rollbackConfig(ownerB) {
		t.Fatal("rollback expected")
	}
	select {
	case <-rig.ma.wdNotify:
	default:
		t.Fatal("rollback must wake the watchdog so it re-snapshots the restored definition")
	}
}

func TestDeploySweepsBeforePreStart(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.preStart = []string{"migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	// pre-pre_start (keep old=nil), pre-Start (keep old=nil), pre-GC (keep new).
	if n := rig.runner.sweepCount(); n != 3 {
		t.Fatalf("expected 3 sweeps, got %d", n)
	}
	rig.runner.sweepErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "not running pre_start") {
		t.Fatalf("an unconfirmed ledger must block the migration too, got %v", err)
	}
	if rig.runner.runOnceCount != 1 {
		t.Fatalf("no second pre_start may run, got %d", rig.runner.runOnceCount)
	}
}

func TestDestructBeforeStartTouchesNothing(t *testing.T) {
	// `motel validate`, and a candidate config that never became
	// the serving one (another app's Start failed): no started config
	// is live, so whatever the manager runs belongs to whoever is or
	// will be serving it and must be left alone.
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	before := rig.runner.sweepCount()
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 || rig.runner.sweepCount() != before {
		t.Fatalf("Destruct without Start must not stop or sweep: stops=%d sweeps=%d", rig.runner.stopCount(), rig.runner.sweepCount()-before)
	}
	if !rig.runner.Alive(rig.runner.handleAt(0)) {
		t.Fatal("the instance must still be running")
	}
}

func TestParseEnvFileRejectsInvalidKeys(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("GOOD=1\nmy-var=2\n"), 0o600))
	_, err := parseEnvFile(envFile)
	if err == nil || !strings.Contains(err.Error(), `"my-var"`) || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("an invalid key must be named with its line, got %v", err)
	}
}

// The exported predicate the box names its secrets by refuses the four
// shell-owned names too, so a secret called PWD is refused where it is
// named rather than at RenderEnvFile.
func TestValidEnvKeyRefusesShellOwnedNames(t *testing.T) {
	for _, k := range []string{"IFS", "OPTIND", "PPID", "PWD"} {
		if ValidEnvKey(k) {
			t.Errorf("ValidEnvKey(%q) = true; the sibling shell resets it", k)
		}
	}
	for _, k := range []string{"PWD_X", "IFS2", "HOME", "DATABASE_URL"} {
		if !ValidEnvKey(k) {
			t.Errorf("ValidEnvKey(%q) = false", k)
		}
	}
}

// The four names the sibling shell resets at its start (IFS, OPTIND,
// PPID, PWD) are refused with the line, like an invalid key: a value
// for one would not reach the app.
func TestParseEnvFileRejectsShellOwnedKeys(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "app.env")
	must(t, os.WriteFile(envFile, []byte("GOOD=1\nPPID=2\n"), 0o600))
	_, err := parseEnvFile(envFile)
	if err == nil || !strings.Contains(err.Error(), `"PPID" is set by the shell`) || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("a shell-owned key must be named with its line, got %v", err)
	}
}

func TestEnsureRunningRefusesAForeignUnitName(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-other.v1.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.reattachSeen) != 1 || rig.runner.reattachSeen[0].Unit != "" {
		t.Fatalf("a unit that is not ours must never reach the manager: %+v", rig.runner.reattachSeen)
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("the recorded version is relaunched instead")
	}
}

func TestEnsureRunningNoStateStillSweeps(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.ensureRunning())
	if rig.runner.startCount() != 0 {
		t.Fatal("nothing recorded ⇒ nothing launched")
	}
	if n := rig.runner.sweepCount(); n != 1 || rig.runner.sweeps[0] != nil {
		t.Fatalf("with no state the manager must still be swept with keep=nil, sweeps=%d", n)
	}
	rig.runner.sweepErr = errTest
	if err := rig.ma.ensureRunning(); !transientRecovery(err) {
		t.Fatalf("an unconfirmed no-state sweep must be retried, got %v", err)
	}
}

func TestEnsureRunningDeployInProgressIsTransient(t *testing.T) {
	rig := newTestRig(t)
	rig.ma.deployMu.Lock()
	defer rig.ma.deployMu.Unlock()
	if err := rig.ma.ensureRunning(); !transientRecovery(err) {
		t.Fatalf("a deploy holding the lock must make recovery look again later, got %v", err)
	}
}

func TestRecoveryErrorClassification(t *testing.T) {
	if transientRecovery(nil) {
		t.Fatal("nil is not an error")
	}
	if !transientRecovery(errTest) || !transientRecovery(&unitUnconfirmedError{unit: "u", err: errTest}) {
		t.Fatal("unclassified and unconfirmed errors are retried")
	}
	if transientRecovery(&permanentRecoveryError{errTest}) {
		t.Fatal("permanent errors are not retried")
	}
}

func TestEnsureRunningRelaunchSweepsStrays(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	must(t, rig.ma.ensureRunning())
	if rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != nil {
		t.Fatal("relaunch must sweep everything before starting")
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("relaunched once")
	}
}

func TestEnsureRunningDoesNotRelaunchWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig.store.ok = true
	must(t, os.MkdirAll(rig.spec.dirs.release("v7"), 0o755))
	rig.runner.sweepErr = errTest
	if err := rig.ma.ensureRunning(); err == nil || !strings.Contains(err.Error(), "not launching") {
		t.Fatalf("expected a refusal to launch, got %v", err)
	}
	if rig.runner.startCount() != 0 || rig.ma.activeSocket.Load() != nil {
		t.Fatal("nothing may be launched or published beside a possibly-running unit")
	}
	// Recovery retries this every minute: the launch prepared and then
	// abandoned must leave no run dir behind, or the retries leak one
	// apiece until a sweep confirms.
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("an abandoned launch left run dirs behind: %v", got)
	}
}

// A start the runner cannot confirm may have reached the manager: the
// unit may be coming up, and its socket dir must stay for it to bind
// in — the next confirmed sweep settles it. Same for a stop that
// could not be confirmed. Only a launch known to be dead is retired.
func TestAmbiguousStartKeepsTheLaunchSocketDir(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest})
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); err == nil {
		t.Fatal("expected the ambiguous start to fail the deploy")
	}
	if got := socketFiles(t, rig); len(got) != 1 {
		t.Fatalf("an ambiguous start must keep the launch's run dir, got %v", got)
	}
	// A definitively failed start, by contrast, retires its launch —
	// and this deploy's confirmed pre-Start sweep is the "next
	// confirmed sweep" that prunes the ambiguous one's dir.
	rig.runner.setStartErr(errTest)
	_ = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("a failed start retires its launch and the confirmed sweep prunes the ambiguous one, got %v", got)
	}
	// The relaunch path makes the same distinction.
	rig2 := newTestRig(t)
	rig2.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{PID: 1}}
	rig2.store.ok = true
	must(t, os.MkdirAll(rig2.spec.dirs.release("v7"), 0o755))
	rig2.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest})
	if err := rig2.ma.ensureRunning(); err == nil {
		t.Fatal("expected the ambiguous relaunch to be reported")
	}
	if got := socketFiles(t, rig2); len(got) != 1 {
		t.Fatalf("an ambiguous relaunch must keep its run dir, got %v", got)
	}
}

// The socket ledger after every way a deploy can end: what is on disk
// under run/ and proxy/ is exactly the instances that are live — or
// may be, when the runner could not say — and nothing else. Each row
// injects one failure into a second deploy over a serving v1; the
// expectation names which of the two launches' dirs may remain.
func TestSocketLedgerAfterEveryDeployOutcome(t *testing.T) {
	type outcome struct {
		inject func(rig *testRig)
		keepV1 bool // v1 still serves (its dirs stay)
		keepV2 bool // v2's dirs stay: it serves, or it may be live and cannot be touched
	}
	for name, tc := range map[string]outcome{
		"success":                 {inject: func(*testRig) {}, keepV1: false, keepV2: true},
		"env_file missing":        {inject: func(r *testRig) { r.spec.envFile = filepath.Join(t.TempDir(), "none.env") }, keepV1: true},
		"pre_start fails":         {inject: func(r *testRig) { r.spec.preStart = []string{"./migrate"}; r.runner.runOnceErr = errTest }, keepV1: true},
		"sweep unconfirmed":       {inject: func(r *testRig) { r.runner.sweepErr = errTest }, keepV1: true},
		"start fails":             {inject: func(r *testRig) { r.runner.setStartErr(errTest) }, keepV1: true},
		"start ambiguous":         {inject: func(r *testRig) { r.runner.setStartErr(&unitUnconfirmedError{unit: "u", err: errTest}) }, keepV1: true, keepV2: true},
		"health gate fails":       {inject: func(r *testRig) { r.prober.err = errTest }, keepV1: true},
		"new instance stop hangs": {inject: func(r *testRig) { r.prober.err = errTest; r.runner.stopErr = errTest; r.runner.stopLeavesAlive = true }, keepV1: true, keepV2: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newTestRig(t)
			rig.prober.bind = true
			ctx := context.Background()
			must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/1", version: "v1"}))
			v1 := rig.ma.currentInstance().nonce
			tc.inject(rig)
			err := rig.ma.Deploy(ctx, deployRequest{url: "https://x/2", version: "v2"})
			if (err == nil) != (name == "success") {
				t.Fatalf("deploy v2: err = %v", err)
			}
			for _, dir := range []struct {
				name string
				got  []string
			}{{"run", socketFiles(t, rig)}, {"proxy", pinnedFiles(t, rig)}} {
				var want []string
				if tc.keepV1 {
					want = append(want, v1)
				}
				others := 0
				for _, n := range dir.got {
					if n != v1 {
						others++
					}
				}
				if dir.name == "proxy" && name == "start ambiguous" {
					continue // never pinned: the start never returned a handle to probe
				}
				if (tc.keepV1 && !slices.Contains(dir.got, v1)) || (!tc.keepV1 && slices.Contains(dir.got, v1)) {
					t.Errorf("%s: v1's entry present = %v, want %v (got %v)", dir.name, slices.Contains(dir.got, v1), tc.keepV1, dir.got)
				}
				if wantOthers := map[bool]int{true: 1, false: 0}[tc.keepV2]; others != wantOthers {
					t.Errorf("%s: %d entries besides v1's, want %d (got %v, v1 %s, want-v1 %v)", dir.name, others, wantOthers, dir.got, v1, want)
				}
			}
		})
	}
}

// Promise: an address the proxy was ever handed never resolves to a
// live socket again once its instance is retired — the property the
// hard-linked, nonce-named pin exists for (a descriptor number would
// be recycled). Across several cutovers, every retired address is
// gone and only the current one remains.
func TestRetiredAddressesNeverResolveAgain(t *testing.T) {
	rig := newUpstreamsRig(t)
	u := &Upstreams{App: "demo", ma: rig.ma}
	var retired []string
	for i := 1; i <= 6; i++ {
		must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a", version: fmt.Sprintf("v%d", i)}))
		ups, err := u.GetUpstreams(httptest.NewRequest("GET", "/", nil))
		must(t, err)
		addr := strings.TrimPrefix(ups[0].Dial, "unix/")
		if slices.Contains(retired, addr) {
			t.Fatalf("deploy %d was handed a retired address %s", i, addr)
		}
		for _, old := range retired {
			if _, err := os.Lstat(old); !os.IsNotExist(err) {
				t.Fatalf("retired address %s still resolves after deploy %d: %v", old, i, err)
			}
		}
		if st, err := os.Lstat(addr); err != nil || st.Mode()&os.ModeSocket == 0 {
			t.Fatalf("current address %s is not a socket: %v", addr, err)
		}
		retired = append(retired, addr)
	}
}

// Promise: a launch whose preparation fails — a missing env_file, the
// case recovery retries forever — reserves nothing. The nonce dir is
// created exclusively, so an existing one is never adopted either.
func TestPrepareLaunchFailureReservesNothing(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.envFile = filepath.Join(t.TempDir(), "missing.env")
	if _, err := rig.spec.prepareLaunch(nil, "v1"); err == nil {
		t.Fatal("a missing env_file must fail preparation")
	}
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("a failed preparation left run dirs behind: %v", got)
	}
	// And a deploy that aborts after preparation (unconfirmed sweep)
	// leaves none either.
	rig.spec.envFile = ""
	rig.runner.sweepErr = errTest
	if err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}); err == nil {
		t.Fatal("expected the deploy to abort on the unconfirmed sweep")
	}
	if got := socketFiles(t, rig); len(got) != 0 {
		t.Fatalf("an aborted deploy left run dirs behind: %v", got)
	}
}

func TestDeployAbortsStartWhenSweepUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/1", version: "v1"}))
	rig.runner.sweepErr = errTest
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/2", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "not starting") {
		t.Fatalf("expected the deploy to abort before Start, got %v", err)
	}
	if rig.runner.startCount() != 1 {
		t.Fatal("no second instance may be started")
	}
	if rig.runner.lastSweep() != rig.runner.handleAt(0) {
		t.Fatal("the pre-start sweep must keep the serving instance")
	}
	if rig.ma.currentInstance().version != "v1" {
		t.Fatal("v1 keeps serving")
	}
}

func TestDestructOnRemovalSweepsEvenWithoutInstance(t *testing.T) {
	rig := newTestRig(t)
	markLive(t)
	must(t, rig.ma.Destruct())
	if rig.runner.stopCount() != 0 || rig.runner.sweepCount() != 1 || rig.runner.sweeps[0] != nil {
		t.Fatalf("removal with nothing tracked must still sweep the app: stops=%d sweeps=%v", rig.runner.stopCount(), rig.runner.sweeps)
	}
}

func TestDeploySweepsBeforeGCAndSkipsGCWhenSweepFails(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2"} {
		must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: v}))
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(rig.spec.dirs.release(v), mt, mt)
	}
	// Each deploy sweeps twice: before Start (keeping the serving
	// instance) and before GC (keeping the promoted one).
	if rig.runner.sweepCount() != 4 || rig.runner.sweeps[0] != nil || rig.runner.sweeps[2] != rig.runner.handleAt(0) {
		t.Fatalf("sweeps %v", rig.runner.sweeps)
	}
	// keep=2: v3 would GC v1 — unless the pre-GC sweep cannot vouch
	// that nothing else is running.
	rig.runner.sweepErrs = []error{nil, errTest}
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v3"}))
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := os.Stat(rig.spec.dirs.release(v)); err != nil {
			t.Fatalf("release %s must survive a deploy whose sweep failed: %v", v, err)
		}
	}
	if rig.runner.lastSweep() != rig.runner.handleAt(2) {
		t.Fatal("the pre-GC sweep must keep the just-promoted instance")
	}
	must(t, rig.ma.Deploy(ctx, deployRequest{url: "https://x/a", version: "v4"}))
	if _, err := os.Stat(rig.spec.dirs.release("v1")); err == nil {
		t.Fatal("once the sweep vouches, GC catches up")
	}
}

func TestFailedDeployKeepsReleaseWhenStartUnconfirmed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	rig.runner.setStartErr(&unitUnconfirmedError{unit: "motel-demo.v2.deadbeefdeadbeef.service", err: errTest})
	err := rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled start must keep the release: %v", err)
	}
	if _, statErr := os.Stat(rig.spec.dirs.release("v2")); statErr != nil {
		t.Fatalf("release must survive an unconfirmed start: %v", statErr)
	}
	rig.runner.setStartErr(nil)
	rig.runner.runOnceErr = &unitUnconfirmedError{unit: "motel-demo.v3.deadbeefdeadbeef.prestart.service", err: errTest}
	rig.spec.preStart = []string{"migrate"}
	err = rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/c.tgz", version: "v3"})
	if err == nil || !strings.Contains(err.Error(), "left on disk") {
		t.Fatalf("an unreconciled pre_start must keep the release: %v", err)
	}
}

// treeOf lists every path under dir, relative to it, so a test can
// tell whether anything was written there.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	must(t, filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, rel)
		return err
	}))
	return out
}

// A deploy or rollback into an app dir that is not the directory it
// names is refused before it reads or writes anything through it
// (#300): through `<root>/demo -> shop`, or any of the dirs it makes
// linked to shop's, nothing lands in shop's dirs, nothing is fetched
// or launched, and the refusal names demo's own path, never shop's.
func TestDeployRefusesAnAppDirThatIsALink(t *testing.T) {
	for _, link := range []string{"", "releases", "shared", "tmp", "run", "proxy"} {
		for _, req := range []deployRequest{
			{url: "https://x/a.tgz", version: "v9"},
			{version: "v1", rollback: true}, // a version shop has
		} {
			t.Run(link+"/"+req.source(), func(t *testing.T) {
				rig := newTestRig(t)
				demo := rig.spec.dirs
				shop := newAppDirs(demo.root, "shop")
				staleInstance(t, shop, linkTestNonce)
				must(t, os.MkdirAll(shop.release("v1"), 0o750))
				before := treeOf(t, shop.app)
				want := demo.app
				if link == "" {
					must(t, os.Symlink("shop", demo.app))
				} else {
					must(t, demo.ensure())
					want = filepath.Join(demo.app, link)
					must(t, os.Remove(want))
					must(t, os.Symlink(filepath.Join("..", "shop", link), want))
				}
				err := rig.ma.Deploy(context.Background(), req)
				if err == nil || !strings.Contains(err.Error(), "refusing to deploy: "+want+" ") || strings.Contains(err.Error(), "shop") {
					t.Fatalf("want a refusal naming %s and not shop, got %v", want, err)
				}
				if rig.fetch.lastReq.version != "" || rig.runner.startCount() != 0 {
					t.Fatal("the deploy went on through the link")
				}
				if after := treeOf(t, shop.app); !slices.Equal(before, after) {
					t.Fatalf("shop's dirs changed through demo's link:\n before %v\n after  %v", before, after)
				}
			})
		}
	}
}

// The one difference the refusal allows is an alias on the liveswap
// root itself (/srv/liveswap -> /mnt/liveswap): a deploy goes on.
func TestDeployThroughAnAliasedRoot(t *testing.T) {
	rig := newTestRig(t)
	alias := filepath.Join(shortTempDir(t), "alias")
	must(t, os.Symlink(rig.spec.dirs.root, alias))
	rig.spec.dirs = newAppDirs(alias, "demo")
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
}
