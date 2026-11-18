package liveswap

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

// At a start only the caps that are set reach the unit, so an app with
// none gets exactly the manager's defaults; memory_max brings
// MemorySwapMax=0 with it, since a cap an app can spill past into
// swap bounds nothing.
func TestCapPropertiesAtStartEmitOnlyWhatIsSet(t *testing.T) {
	none := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: probeSandboxSpec()})
	for _, name := range []string{"MemoryMax", "MemorySwapMax", "TasksMax", "CPUQuotaPerSecUSec"} {
		if _, set := none[name]; set {
			t.Errorf("%s emitted for a spec with no caps", name)
		}
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: probeSandboxSpec(), Caps: resourceCaps{memoryMax: 64 << 20, cpuQuota: 150}})
	for name, want := range map[string]any{
		"MemoryMax":          uint64(64 << 20),
		"MemorySwapMax":      uint64(0),
		"CPUQuotaPerSecUSec": uint64(1_500_000), // 150% of a second, in µs
	} {
		if got[name] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", name, got[name], got[name], want, want)
		}
	}
	if _, set := got["TasksMax"]; set {
		t.Error("TasksMax emitted though unset: the manager's default must stand")
	}
}

// Live, every cap is sent: a cap a reload removed is lifted from the
// running unit, swap allowance included — memory and CPU to infinity,
// their defaults, and tasks to the manager's default, never infinity
// (which would lift the manager's own bound from the unit).
func TestCapPropertiesLiveLiftWhatIsUnset(t *testing.T) {
	got := map[string]any{}
	for _, p := range capProperties(resourceCaps{cpuQuota: 50}, true, 2273) {
		got[p.Name] = p.Value.Value()
	}
	for name, want := range map[string]any{
		"MemoryMax":          infinityUSec,
		"MemorySwapMax":      infinityUSec,
		"TasksMax":           uint64(2273),
		"CPUQuotaPerSecUSec": uint64(500_000),
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
	got = map[string]any{}
	for _, p := range capProperties(resourceCaps{tasksMax: 40}, true, 2273) {
		got[p.Name] = p.Value.Value()
	}
	if got["TasksMax"] != uint64(40) {
		t.Errorf("a set tasks cap must win over the lift: TasksMax = %v", got["TasksMax"])
	}
	// A kept tasks cap does not hold the memory and CPU resets back,
	// and neither does an unreadable default (liftTasksTo 0): the two
	// resets go, and only TasksMax is left as it is.
	for _, lift := range []uint64{2273, 0} {
		got = map[string]any{}
		for _, p := range capProperties(resourceCaps{tasksMax: 40}, true, lift) {
			got[p.Name] = p.Value.Value()
		}
		if got["MemoryMax"] != infinityUSec || got["MemorySwapMax"] != infinityUSec || got["CPUQuotaPerSecUSec"] != infinityUSec {
			t.Errorf("lift %d: removed memory/CPU caps not reset: %v", lift, got)
		}
	}
	got = map[string]any{}
	for _, p := range capProperties(resourceCaps{memoryMax: 1 << 20}, true, 0) {
		got[p.Name] = p.Value.Value()
	}
	if _, has := got["TasksMax"]; has {
		t.Errorf("TasksMax sent with no default to lift to: %v", got)
	}
}

// SetCaps lifts an unset tasks cap to the manager's DefaultTasksMax;
// with that unreadable it leaves the unit's tasks cap alone and the
// handle keeps reporting it, so status stays honest.
func TestSetCapsLiftsTasksToTheManagersDefault(t *testing.T) {
	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	r := newSystemdRunner(f, zap.NewNop())
	t.Cleanup(r.close)
	h := &systemdHandle{unit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service", done: make(chan struct{})}
	h.caps.Store(&resourceCaps{memoryMax: 64 << 20, tasksMax: 40})
	must(t, r.SetCaps(h, resourceCaps{memoryMax: 128 << 20}))
	sent := map[string]any{}
	for _, p := range f.setProps[h.unit][0] {
		sent[p.Name] = p.Value.Value()
	}
	if sent["TasksMax"] != uint64(2273) || sent["MemoryMax"] != uint64(128<<20) {
		t.Fatalf("sent %v, want TasksMax lifted to the manager's 2273 and MemoryMax 128M", sent)
	}
	if got := h.state().Caps; got != (resourceCaps{memoryMax: 128 << 20}) {
		t.Fatalf("handle reports %+v after the lift, want memory only", got)
	}
	// The default unreadable: no TasksMax is sent, and the handle
	// keeps the cap the unit still carries.
	h.caps.Store(&resourceCaps{tasksMax: 40})
	f.defaultTasksMax = 0
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 50}))
	sent = map[string]any{}
	for _, p := range f.setProps[h.unit][1] {
		sent[p.Name] = p.Value.Value()
	}
	if _, has := sent["TasksMax"]; has {
		t.Fatalf("TasksMax sent with the default unreadable: %v", sent)
	}
	if got := h.state().Caps; got != (resourceCaps{tasksMax: 40, cpuQuota: 50}) {
		t.Fatalf("handle reports %+v, want the kept tasks cap beside the new quota", got)
	}
	// A manager whose default is infinity: that is its default, and
	// the lift goes there (the only case TasksMax=infinity is written).
	f.defaultTasksMax = infinityUSec
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 50}))
	sent = map[string]any{}
	for _, p := range f.setProps[h.unit][2] {
		sent[p.Name] = p.Value.Value()
	}
	if sent["TasksMax"] != infinityUSec {
		t.Fatalf("sent %v, want TasksMax lifted to the manager's own infinity", sent)
	}
	if got := h.state().Caps; got != (resourceCaps{cpuQuota: 50}) {
		t.Fatalf("handle reports %+v after the lift to infinity, want the quota alone", got)
	}
	// A unit with no tasks cap the handle knows of: nothing to lift, so
	// no TasksMax= is written (one would pin the manager's default into
	// the unit, past a later change of it) though a default is there.
	f.defaultTasksMax = 2273
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 100}))
	sent = map[string]any{}
	for _, p := range f.setProps[h.unit][3] {
		sent[p.Name] = p.Value.Value()
	}
	if _, has := sent["TasksMax"]; has {
		t.Fatalf("TasksMax written for a unit that carries none: %v", sent)
	}
	if st := h.state(); st.CapsUnknown || st.Caps != (resourceCaps{cpuQuota: 100}) {
		t.Fatalf("handle reports %+v (unknown=%v), want the new quota, known", st.Caps, st.CapsUnknown)
	}
}

// The manager's DefaultTasksMax= is read as the manager renders it:
// a number, or infinity (kept: it is a default like any other), with
// anything else unreadable. DefaultLimitNOFILE keeps reading infinity
// as unknown, since a unit's LimitNOFILE= is never lifted to it.
func TestParseManagerLimitKeepsInfinity(t *testing.T) {
	for in, want := range map[string]uint64{"2273": 2273, "@t 2273": 2273, "18446744073709551615": infinityUSec, "infinity": 0, "": 0} {
		if got := parseManagerLimit(in); got != want {
			t.Errorf("parseManagerLimit(%q) = %d, want %d", in, got, want)
		}
	}
	if got := parseManagerUint("18446744073709551615"); got != 0 {
		t.Errorf("parseManagerUint(infinity) = %d, want 0 (unknown)", got)
	}
}

// A unit with no TasksMax= of its own reports the manager's resolved
// default as its TasksMax (a number, never "infinity"): a reattach
// reads that as unset, so an uncapped app is not "capped at 2273" in
// status and no sync keeps re-sending it.
func TestReattachReadsTheDefaultTasksMaxAsUnset(t *testing.T) {
	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	unit := "motel-demo.v1.0a1b2c3d0a1b2c3d.service"
	setStatus := func(caps resourceCaps) { // under the fake's lock: a reattached handle's watcher reads it
		f.mu.Lock()
		defer f.mu.Unlock()
		f.status[unit] = unitStatus{Name: unit, LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true, Caps: caps}
	}
	setStatus(resourceCaps{memoryMax: 64 << 20, tasksMax: 2273})
	r := newSystemdRunner(f, zap.NewNop())
	t.Cleanup(r.close)
	h, ok, err := r.Reattach(handleState{Unit: unit})
	if err != nil || !ok {
		t.Fatalf("reattach: ok=%v err=%v", ok, err)
	}
	if got := h.state().Caps; got != (resourceCaps{memoryMax: 64 << 20}) {
		t.Fatalf("reattached handle reports %+v, want the memory cap alone (2273 is the manager's default)", got)
	}
	// A tasks cap that is not the default is the operator's.
	setStatus(resourceCaps{tasksMax: 512})
	h, _, _ = r.Reattach(handleState{Unit: unit})
	if got := h.state().Caps; got != (resourceCaps{tasksMax: 512}) {
		t.Fatalf("reattached handle reports %+v, want tasks_max 512", got)
	}
}

// What the manager reports for an unbounded unit is "infinity", which
// must read as no cap — never as a bound of 2^64, and never as 0.
func TestPropCapsReadsInfinityAsNone(t *testing.T) {
	got := propCaps(map[string]any{"MemoryMax": infinityUSec, "TasksMax": infinityUSec, "CPUQuotaPerSecUSec": infinityUSec})
	if got.any() {
		t.Fatalf("infinity read as a cap: %+v", got)
	}
	if got := propCaps(map[string]any{}); got.any() {
		t.Fatalf("absent properties read as a cap: %+v", got)
	}
	got = propCaps(map[string]any{"MemoryMax": uint64(64 << 20), "TasksMax": uint64(40), "CPUQuotaPerSecUSec": uint64(500_000)})
	if want := (resourceCaps{memoryMax: 64 << 20, tasksMax: 40, cpuQuota: 50}); got != want {
		t.Fatalf("propCaps = %+v, want %+v", got, want)
	}
}

// A reload's changed caps reach the instance already running (and
// status shows them from then on); a reload that drops them lifts
// them, and one that changes nothing about them makes no call.
func TestSyncCapsAppliesAReloadsCapsToTheRunningInstance(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	if rig.ma.status().Caps != nil {
		t.Fatal("no caps configured, none may be reported")
	}
	capped := testSpec(t)
	capped.caps = resourceCaps{memoryMax: 256 << 20, tasksMax: 512, cpuQuota: 200}
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != capped.caps {
		t.Fatalf("SetCaps calls = %+v, want the reload's caps once", rig.runner.setCaps)
	}
	if got := rig.ma.status().Caps; got == nil || got.MemoryMax != 256<<20 || got.TasksMax != 512 || got.CPUQuota != 200 {
		t.Fatalf("status caps = %+v, want the applied ones", got)
	}
	// Unchanged caps: nothing to send.
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 1 {
		t.Fatalf("SetCaps called again for unchanged caps: %+v", rig.runner.setCaps)
	}
	// Dropped caps are lifted, as one call carrying none.
	rig.ma.configure(new(int), testSpec(t), nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if len(rig.runner.setCaps) != 2 || rig.runner.setCaps[1].any() {
		t.Fatalf("SetCaps calls = %+v, want a lifting call with no caps", rig.runner.setCaps)
	}
	if rig.ma.status().Caps != nil {
		t.Fatal("lifted caps still reported")
	}
}

// A sync that fails is a warning, and the instance keeps reporting
// what it had: the caps still apply at the next launch (startSpec).
func TestSyncCapsFailureIsAWarning(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	capped := testSpec(t)
	capped.caps = resourceCaps{cpuQuota: 50}
	rig.runner.setCapsErr = os.ErrDeadlineExceeded
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager)
	rig.ma.syncCaps()
	if rig.ma.status().Caps != nil {
		t.Fatal("a failed sync must not report the caps as applied")
	}
	// The failure left the unit's caps unknown (the fake's worst case):
	// a sync back to what the handle last knew re-sends rather than
	// trusting the equality — the manager may have applied the
	// change before the failure.
	rig.runner.setCapsErr = nil
	rig.ma.configure(new(int), testSpec(t), nil, zap.NewNop(), &fetchClients{}, userManager)
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0].any() {
		t.Fatalf("SetCaps calls = %+v, want one lifting call after the ambiguous failure, not a skip", rig.runner.setCaps)
	}
	// The next launch carries the spec's caps regardless.
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}))
	if last := rig.runner.started[len(rig.runner.started)-1]; last.caps != capped.caps {
		t.Fatalf("the launch after a failed sync carried %+v, want %+v", last.caps, capped.caps)
	}
}

// A SetCaps that fails after the manager may have applied it reads
// the unit back and takes the manager's word; with the unit
// unreadable too, the caps are unknown and status shows none.
func TestSetCapsFailureReadsTheUnitBack(t *testing.T) {
	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	r := newSystemdRunner(f, zap.NewNop())
	t.Cleanup(r.close)
	unit := "motel-demo.v1.0a1b2c3d0a1b2c3d.service"
	h := &systemdHandle{unit: unit, done: make(chan struct{})}
	h.caps.Store(&resourceCaps{cpuQuota: 50})
	// Applied, then the transport failed: the manager holds 200%.
	f.mu.Lock()
	f.setPropsErr = os.ErrDeadlineExceeded
	f.status[unit] = unitStatus{Name: unit, LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true, Caps: resourceCaps{cpuQuota: 200}}
	f.mu.Unlock()
	if err := r.SetCaps(h, resourceCaps{cpuQuota: 200}); err == nil {
		t.Fatal("SetCaps must report the failure")
	}
	if st := h.state(); st.CapsUnknown || st.Caps != (resourceCaps{cpuQuota: 200}) {
		t.Fatalf("after an ambiguous failure the handle reports %+v (unknown=%v), want the manager's 200%%", st.Caps, st.CapsUnknown)
	}
	// The unit unreadable as well: unknown, until a SetCaps succeeds.
	f.mu.Lock()
	f.statusErr = os.ErrDeadlineExceeded
	f.mu.Unlock()
	if err := r.SetCaps(h, resourceCaps{cpuQuota: 50}); err == nil {
		t.Fatal("SetCaps must report the failure")
	}
	if st := h.state(); !st.CapsUnknown {
		t.Fatalf("with the unit unreadable the caps must be unknown, got %+v", st)
	}
	f.mu.Lock()
	f.statusErr, f.setPropsErr = nil, nil
	f.defaultTasksMax = 0 // no default to lift a tasks cap to
	f.mu.Unlock()
	// A success that wrote no TasksMax (nothing to lift to) settles
	// nothing about the tasks cap: still unknown.
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 50}))
	if st := h.state(); !st.CapsUnknown {
		t.Fatalf("a SetCaps that left TasksMax untouched must keep the caps unknown, got %+v", st)
	}
	// One that writes TasksMax settles them.
	must(t, r.SetCaps(h, resourceCaps{cpuQuota: 50, tasksMax: 40}))
	if st := h.state(); st.CapsUnknown || st.Caps != (resourceCaps{cpuQuota: 50, tasksMax: 40}) {
		t.Fatalf("a successful SetCaps that wrote TasksMax must settle the caps, got %+v (unknown=%v)", st.Caps, st.CapsUnknown)
	}
}

// A start whose status read failed has no cgroup path, so nothing can
// say which of its caps the host enforces; the watcher backfills the
// path on its first good read, and the enforcement with it. A
// read-back after an ambiguous SetCaps recomputes it too, from what
// the manager says the unit now carries.
func TestWatcherAndReadBackRefreshEnforcement(t *testing.T) {
	root := t.TempDir()
	cg := "/user.slice/user-1000.slice/user@1000.service/app.slice/x.service"
	if err := os.MkdirAll(root+cg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("pids cpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	f := newFakeSystemdConn()
	f.defaultTasksMax = 2273
	unit := "motel-demo.v1.0a1b2c3d0a1b2c3d.service"
	running := unitStatus{Name: unit, LoadState: "loaded", ActiveState: "active", MainPID: 4242, Sandboxed: true, ControlGroup: cg}
	f.mu.Lock()
	f.status[unit] = running
	f.mu.Unlock()
	r := newSystemdRunner(f, zap.NewNop())
	r.poll = 10 * time.Millisecond
	t.Cleanup(r.close)
	// A handle adopted with no status read: caps, no cgroup path.
	h := &systemdHandle{unit: unit, done: make(chan struct{})}
	h.caps.Store(&resourceCaps{memoryMax: 1 << 20, tasksMax: 40})
	if got := h.state().CapsUnenforced; got != nil {
		t.Fatalf("with no cgroup path nothing can be claimed, got %v", got)
	}
	go r.watch(h)
	deadline := time.Now().Add(2 * time.Second)
	for strings.Join(h.state().CapsUnenforced, ",") != "memory_max" {
		if time.Now().After(deadline) {
			t.Fatalf("the watcher did not backfill the cgroup path and enforcement; state %+v", h.state())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// An ambiguous SetCaps whose read-back says the manager now holds a
	// cpu cap as well: the list follows the read-back's caps.
	f.mu.Lock()
	f.setPropsErr = os.ErrDeadlineExceeded
	running.Caps = resourceCaps{memoryMax: 1 << 20, cpuQuota: 50}
	f.status[unit] = running
	f.mu.Unlock()
	if err := r.SetCaps(h, resourceCaps{memoryMax: 1 << 20, cpuQuota: 50}); err == nil {
		t.Fatal("SetCaps must report the failure")
	}
	if st := h.state(); st.CapsUnknown || st.Caps != (resourceCaps{memoryMax: 1 << 20, cpuQuota: 50}) || strings.Join(st.CapsUnenforced, ",") != "memory_max" {
		t.Fatalf("after the read-back: %+v", st)
	}
	// And a read-back that drops the memory cap drops it from the list.
	f.mu.Lock()
	running.Caps = resourceCaps{cpuQuota: 50}
	f.status[unit] = running
	f.mu.Unlock()
	_ = r.SetCaps(h, resourceCaps{cpuQuota: 50})
	if st := h.state(); len(st.CapsUnenforced) != 0 {
		t.Fatalf("no memory cap left, yet %v is unenforced", st.CapsUnenforced)
	}
	// A list computed from a reading the host has since outgrown (the
	// cpu controller arriving after a write whose own reading failed)
	// is read again by the watcher while anything is listed, and
	// clears once the reading shows the controller.
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.setCapsState(h, &resourceCaps{cpuQuota: 50}, nil, nil)
	if got := h.state().CapsUnenforced; strings.Join(got, ",") != "cpu_quota" {
		t.Fatalf("with the cpu controller absent the list must name cpu_quota, got %v", got)
	}
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("pids cpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(h.state().CapsUnenforced) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the watcher did not re-read the controllers while cpu_quota was listed; state %+v", h.state())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A status read takes the caps and the enforcement list together: a
// reader racing a writer never sees a memory cap paired with a list
// computed before that cap existed, on a host whose memory controller
// is absent — which would show the cap as in effect for an instant.
func TestStateReadsCapsAndEnforcementTogether(t *testing.T) {
	root := t.TempDir()
	cg := "/app.slice/x.service"
	if err := os.MkdirAll(root+cg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("pids cpu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
	r := newSystemdRunner(newFakeSystemdConn(), zap.NewNop())
	t.Cleanup(r.close)
	h := &systemdHandle{unit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service", done: make(chan struct{})}
	r.setCapsState(h, &resourceCaps{cpuQuota: 50}, &cg, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				r.setCapsState(h, &resourceCaps{cpuQuota: 50, memoryMax: 1 << 20}, nil, nil)
			} else {
				r.setCapsState(h, &resourceCaps{cpuQuota: 50}, nil, nil)
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		st := h.state()
		if st.Caps.memoryMax > 0 && !slices.Contains(st.CapsUnenforced, "memory_max") {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: a memory cap shown with a list that does not name it: %+v", i, st)
		}
		if st.Caps.memoryMax == 0 && len(st.CapsUnenforced) != 0 {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d: no memory cap, yet the list names %v", i, st.CapsUnenforced)
		}
	}
	close(stop)
	wg.Wait()
}

// A cap whose controller does not reach the unit's cgroup is held by
// the manager and enforced by nobody: it is named as unenforced, and
// status lists it apart from the caps in effect rather than as one.
func TestUnenforcedCapsNamesTheMissingControllers(t *testing.T) {
	root := t.TempDir()
	cg := "/user.slice/user-1000.slice/user@1000.service/app.slice/x.service"
	if err := os.MkdirAll(root+cg, 0o755); err != nil {
		t.Fatal(err)
	}
	all := resourceCaps{memoryMax: 1 << 20, tasksMax: 40, cpuQuota: 50}
	// No controllers file, or no cgroup path: no reading, never an
	// empty set.
	if have, ok := readControllers(root, cg); ok || have != nil {
		t.Fatalf("an unreadable controllers file read as %v (ok=%v)", have, ok)
	}
	if have, ok := readControllers(root, ""); ok || have != nil {
		t.Fatalf("no cgroup path read as %v (ok=%v)", have, ok)
	}
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	have, ok := readControllers(root, cg)
	if !ok || strings.Join(have, ",") != "pids" {
		t.Fatalf("controllers = %v (ok=%v), want pids", have, ok)
	}
	if got := missingCaps(have, all); strings.Join(got, ",") != "memory_max,cpu_quota" {
		t.Fatalf("unenforced = %v, want memory_max and cpu_quota (only pids is delegated)", got)
	}
	if got := missingCaps(have, resourceCaps{tasksMax: 40}); got != nil {
		t.Fatalf("a cap whose controller is there read as unenforced: %v", got)
	}
	if got := missingCaps([]string{"cpu", "memory", "pids"}, all); got != nil {
		t.Fatalf("every controller delegated, yet %v", got)
	}
	// A reading that fails leaves the last one standing, and the list
	// follows the caps as they now are — a cap removed leaves it, a cap
	// added joins it — until a reading succeeds again.
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
	r := newSystemdRunner(newFakeSystemdConn(), zap.NewNop())
	t.Cleanup(r.close)
	h := &systemdHandle{unit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service", done: make(chan struct{})}
	r.setCapsState(h, &all, &cg, nil)
	if got := h.state().CapsUnenforced; strings.Join(got, ",") != "memory_max,cpu_quota" {
		t.Fatalf("handle lists %v, want memory_max and cpu_quota", got)
	}
	if err := os.Remove(root + cg + "/cgroup.controllers"); err != nil {
		t.Fatal(err)
	}
	r.setCapsState(h, &all, nil, nil)
	if got := h.state().CapsUnenforced; strings.Join(got, ",") != "memory_max,cpu_quota" {
		t.Fatalf("an unreadable controllers file dropped the known list: %v", got)
	}
	r.setCapsState(h, &resourceCaps{cpuQuota: 50}, nil, nil)
	if got := h.state().CapsUnenforced; strings.Join(got, ",") != "cpu_quota" {
		t.Fatalf("a removed cap must leave the kept list: %v", got)
	}
	r.setCapsState(h, &resourceCaps{cpuQuota: 50, memoryMax: 1 << 20}, nil, nil)
	if got := h.state().CapsUnenforced; strings.Join(got, ",") != "memory_max,cpu_quota" {
		t.Fatalf("a cap added under no reading must be classified by the last one: %v", got)
	}
	if err := os.WriteFile(root+cg+"/cgroup.controllers", []byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.setCapsState(h, &all, nil, nil)
	if got := h.state().CapsUnenforced; len(got) != 0 {
		t.Fatalf("a good reading must replace the kept one: %v", got)
	}
	// A handle that never had a reading claims nothing either way.
	bare := &systemdHandle{unit: "motel-demo.v2.0a1b2c3d0a1b2c3d.service", done: make(chan struct{})}
	none := "/user.slice/user-1000.slice/user@1000.service/app.slice/nowhere.service"
	r.setCapsState(bare, &all, &none, nil)
	if got := bare.state().CapsUnenforced; got != nil || bare.controllers.Load() != nil {
		t.Fatalf("with no reading ever made the handle claimed %v", got)
	}
	// Status: the unenforced ones are named, not valued.
	caps := capsForStatus(all, []string{"memory_max", "cpu_quota"})
	if caps == nil || caps.MemoryMax != 0 || caps.CPUQuota != 0 || caps.TasksMax != 40 {
		t.Fatalf("status caps = %+v, want tasks_max alone in effect", caps)
	}
	if caps := capsForStatus(resourceCaps{memoryMax: 1 << 20}, []string{"memory_max"}); caps != nil {
		t.Fatalf("nothing in effect must read as no caps, got %+v", caps)
	}
}

// systemd's infinity, written as a number in a JSON config, is no cap:
// the handle would report it as one and a reattach as none.
func TestValidateRefusesInfinityAsAMemoryCap(t *testing.T) {
	cfg := defaultedApp(t)
	cfg.MemoryMax = infinityUSec
	a := &App{Root: "/var/lib/liveswap", ArtifactAllowlist: []string{"github.com/smallhoursorg/"}, DeployTrust: githubTrust(), Apps: map[string]*AppConfig{"blog": cfg}}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "systemd's infinity, not a cap") {
		t.Fatalf("want the infinity refusal, got %v", err)
	}
}

// A deploy launches from the spec it snapshotted at its start; a
// reload during its fetch or soak changes the caps on the instance it
// is replacing (or on none, for a first deploy), not on the one about
// to be published. Publication reconciles against the spec as it is
// now, so the serving instance carries the accepted configuration's
// caps whichever path published it.
func TestAReloadDuringADeployReachesTheInstanceItPublishes(t *testing.T) {
	rig := newTestRig(t)
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	}()
	held()
	capped := testSpec(t)
	capped.caps = resourceCaps{cpuQuota: 150}
	rig.ma.configure(new(int), capped, nil, zap.NewNop(), &fetchClients{}, userManager) // nothing runs yet: nothing to sync
	release(nil)
	must(t, <-done)
	if last := rig.runner.started[len(rig.runner.started)-1]; last.caps.any() {
		t.Fatalf("the deploy launched with %+v, want the snapshot's none (the point of the test)", last.caps)
	}
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != capped.caps {
		t.Fatalf("SetCaps calls = %+v, want the reload's caps once, on the published instance", rig.runner.setCaps)
	}
	if got := rig.ma.status().Caps; got == nil || got.CPUQuota != 150 {
		t.Fatalf("status caps = %+v, want cpu_quota 150 on the serving instance", got)
	}
}

// Two syncs can start together: a publication's (the deploy's lock
// held) and a reload's (configure, no lock of the deploy's). Without
// serialization the publication could snapshot caps A, the reload
// apply B in between, and A land last. syncCaps is one step per app,
// so the reload's sync waits behind the publication's apply and then
// applies B over it: the last applied is the latest spec.
func TestOverlappingSyncsApplyTheLatestSpecLast(t *testing.T) {
	rig := newTestRig(t)
	held, release := rig.holdFetch(t)
	done := make(chan error, 1)
	go func() {
		done <- rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"})
	}()
	held()
	specA, specB := testSpec(t), testSpec(t)
	specA.caps = resourceCaps{cpuQuota: 100}
	specB.caps = resourceCaps{cpuQuota: 200}
	// Installed during the fetch: the publication will find the
	// instance without caps and apply A.
	rig.ma.configure(new(int), specA, nil, zap.NewNop(), &fetchClients{}, userManager)
	reloaded := make(chan struct{})
	var once sync.Once
	rig.runner.setCapsHook = func(caps resourceCaps) {
		if caps != specA.caps {
			return
		}
		// Inside the publication's apply of A: a reload installs B and
		// syncs. Its sync must wait for this apply to end, so its B
		// lands after A, not under it.
		once.Do(func() {
			go func() {
				rig.ma.configure(new(int), specB, nil, zap.NewNop(), &fetchClients{}, userManager)
				close(reloaded)
			}()
			time.Sleep(100 * time.Millisecond) // let the reload reach the sync and block there
		})
	}
	release(nil)
	must(t, <-done)
	<-reloaded
	if got := rig.runner.setCaps; len(got) != 2 || got[0] != specA.caps || got[1] != specB.caps {
		t.Fatalf("SetCaps order = %+v, want A then B", got)
	}
	if got := rig.ma.status().Caps; got == nil || got.CPUQuota != 200 {
		t.Fatalf("status caps = %+v, want the latest spec's cpu_quota 200 applied last", got)
	}
}

// A unit reattached after a moteld restart carries the caps it was
// started with; a config changed in between reaches it on recovery.
func TestEnsureRunningSyncsCapsOfAReattachedUnit(t *testing.T) {
	rig := newTestRig(t)
	rig.runner.reattachOK = true
	rig.spec.caps = resourceCaps{memoryMax: 128 << 20}
	bindSocket(t, rig.spec.dirs.socket(recordedNonce))
	rig.store.state = appState{CurrentVersion: "v7", Nonce: recordedNonce, Handle: handleState{Unit: "motel-demo.v7.0a1b2c3d0a1b2c3d.service"}}
	rig.store.ok = true
	if err := os.MkdirAll(rig.spec.dirs.release("v7"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, rig.ma.ensureRunning())
	if len(rig.runner.setCaps) != 1 || rig.runner.setCaps[0] != rig.spec.caps {
		t.Fatalf("SetCaps calls = %+v, want the spec's caps once, on the reattached unit", rig.runner.setCaps)
	}
}

// Every launch carries the spec's caps: a deploy's instance, its
// pre_start, a watchdog relaunch — they are the app's, not the
// deploy's.
func TestEveryLaunchCarriesTheSpecsCaps(t *testing.T) {
	rig := newTestRig(t)
	rig.spec.caps = resourceCaps{tasksMax: 64}
	rig.spec.preStart = []string{"./migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	if n := len(rig.runner.started); n != 2 {
		t.Fatalf("started %d units, want the pre_start and the instance", n)
	}
	for _, s := range rig.runner.started {
		if s.caps != rig.spec.caps {
			t.Errorf("unit for %s %s started with %+v, want %+v", s.app, s.version, s.caps, rig.spec.caps)
		}
	}
}

// A CPU quota is written as systemd writes it, with its percent sign:
// a bare number would read as a count of something, and a 2 as two
// percent of one CPU.
func TestCaddyfileRefusesACPUQuotaWithoutItsPercent(t *testing.T) {
	for _, line := range []string{"cpu_quota 150", "cpu_quota two%", "cpu_quota %", "cpu_quota 1.5%"} {
		var a App
		err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser("liveswap {\n\tapp a {\n\t\tcommand x\n\t\t" + line + "\n\t}\n}"))
		if err == nil || !strings.Contains(err.Error(), "a percentage of one CPU, like 200% for two CPUs' worth") {
			t.Errorf("%q: want the percentage error, got %v", line, err)
		}
	}
}
