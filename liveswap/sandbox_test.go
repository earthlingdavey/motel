package liveswap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// sandboxPropertyNames is the set sandboxProperties emits; the test
// pins it so a property can neither disappear nor appear unnoticed.
var sandboxPropertyNames = []string{
	"PrivateUsers", "PrivatePIDs", "PrivateTmp", "PrivateDevices",
	"ProtectControlGroups", "ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs",
	"RestrictNamespaces", "RestrictRealtime", "RestrictSUIDSGID", "LockPersonality",
	"RestrictAddressFamilies", "SystemCallFilter", "SystemCallErrorNumber", "SystemCallArchitectures", "CapabilityBoundingSet",
	"TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths", "UnsetEnvironment",
}

// retiredSandboxPropertyNames are the properties the deny-by-default
// view removed. Nothing is left for them to act on — an unnamed path is
// absent, not merely unreadable — and emitting them again would be a
// silent return to masking a list.
var retiredSandboxPropertyNames = []string{"InaccessiblePaths", "ProtectSystem", "ProtectHome"}

func propMap(u unitSpec) map[string]any {
	got := map[string]any{}
	for _, p := range unitProperties(u) {
		got[p.Name] = p.Value.Value()
	}
	return got
}

// propValues is every value one property is given, in the order the
// manager reads them. SystemCallFilter= is emitted twice, an allow
// list and then a deny entry that edits it, and propMap keeps only
// the last.
func propValues(u unitSpec, name string) []any {
	var vs []any
	for _, p := range unitProperties(u) {
		if p.Name == name {
			vs = append(vs, p.Value.Value())
		}
	}
	return vs
}

// Promise: a unit has one Exec*= command, its ExecStartEx — no
// ExecStopPost= or other — so the manager spawns one process per unit
// (the app's own children join its namespaces as ever, the sibling the
// shell forks before it execs the app among them, #380): on a user
// manager a second process the manager spawns cannot join the IPC
// namespace the first made (#311). The instance's socket is motel's
// to remove, with the run dir (socketRef.retire), so a unit names no
// socket at all.
func TestUnitHasOneExecCommand(t *testing.T) {
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: probeSandboxSpec()})
	if _, ok := got["ExecStartEx"]; !ok {
		t.Fatal("no ExecStartEx")
	}
	for name, v := range got {
		if strings.HasPrefix(name, "Exec") && name != "ExecStartEx" {
			t.Errorf("%s=%v: a unit has one Exec*= command", name, v)
		}
	}
}

func TestSandboxPropertiesEveryUnit(t *testing.T) {
	spec := &sandboxSpec{
		root:    "/var/lib/liveswap",
		appDir:  "/var/lib/liveswap/blog",
		appName: "blog",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/releases/v3", source: "/var/lib/liveswap/blog/releases/v3"},
			{dest: "/var/lib/liveswap/blog/shared", source: "/var/lib/liveswap/blog/shared"}},
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	for _, name := range sandboxPropertyNames {
		if _, present := got[name]; !present {
			t.Errorf("%s missing", name)
		}
	}
	for _, name := range retiredSandboxPropertyNames {
		if _, present := got[name]; present {
			t.Errorf("%s is set: the deny-by-default view masks nothing, it names what exists", name)
		}
	}
	// One sandbox, every unit: both namespaces are emitted
	// unconditionally, never as a tier a lesser host could miss.
	for name, want := range map[string]any{
		"PrivatePIDs":           "yes",
		"PrivateUsers":          true,
		"PrivateTmp":            true,
		"PrivateDevices":        true,
		"ProtectControlGroups":  true,
		"RestrictNamespaces":    uint64(0),
		"CapabilityBoundingSet": uint64(0),
		"SystemCallErrorNumber": int32(1),
		"NoNewPrivileges":       true,
	} {
		if got[name] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", name, got[name], got[name], want, want)
		}
	}
	if fam, _ := got["RestrictAddressFamilies"].(allowList); !fam.AllowList || !reflect.DeepEqual(fam.Names, []string{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"}) {
		t.Errorf("RestrictAddressFamilies = %+v", got["RestrictAddressFamilies"])
	}
	if f := propValues(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec}, "SystemCallFilter"); !reflect.DeepEqual(f, []any{
		allowList{true, []string{"@system-service"}},
		allowList{false, []string{"io_uring_setup"}},
	}) {
		t.Errorf("SystemCallFilter = %+v", f)
	}
	if a, _ := got["SystemCallArchitectures"].([]string); !reflect.DeepEqual(a, []string{"native"}) {
		t.Errorf("SystemCallArchitectures = %+v", got["SystemCallArchitectures"])
	}
	// The whole filesystem, not just the liveswap root: this one line is
	// the difference between "these paths are hidden" and "only these
	// paths exist". Then a /dev/shm of the unit's own, inside the
	// private /dev, over the host's that PrivateDevices= binds (#311),
	// at the host's default size — systemd's own for a
	// TemporaryFileSystem= would be 10% of RAM.
	if tmp, _ := got["TemporaryFileSystem"].([]tmpfsMount); !reflect.DeepEqual(tmp, []tmpfsMount{{"/", "ro"}, {"/dev/shm", "mode=1777,size=50%"}}) {
		t.Errorf("TemporaryFileSystem = %+v, want the whole root replaced and a /dev/shm of the unit's own", got["TemporaryFileSystem"])
	}
	binds := func(name string) map[string]bindMount {
		m := map[string]bindMount{}
		for _, b := range got[name].([]bindMount) {
			m[b.Destination] = b
		}
		return m
	}
	rw := binds("BindPaths")
	// The app's OWN directories are mandatory: a release or shared dir
	// that is not there is a broken deploy, not a boot-order race, and
	// starting without it would give the app an empty tmpfs where its
	// code and data should be.
	for _, p := range []string{"/var/lib/liveswap/blog/releases/v3", "/var/lib/liveswap/blog/shared"} {
		b, ok := rw[p]
		if !ok || b.Source != p || b.Flags != mountRecursive || b.IgnoreENOENT {
			t.Errorf("BindPaths lacks a recursive, mandatory bind of %s at its real path: %+v", p, rw)
		}
	}
	ro := binds("BindReadOnlyPaths")
	// The base view: every entry present, and every one optional — no
	// host has all of it (see sandboxBaseView).
	for _, p := range sandboxBaseView {
		b, ok := ro[p]
		if !ok {
			t.Errorf("BindReadOnlyPaths lacks the base-view entry %s", p)
			continue
		}
		if b.Source != p || !b.IgnoreENOENT || b.Flags != mountRecursive {
			t.Errorf("base-view entry %s = %+v, want a recursive optional bind at its own path", p, b)
		}
	}
	// The base view must carry the runtime an app needs to exec at all,
	// and must NOT carry /etc wholesale — that would hand every app
	// every other app's env_file, which is the derived hidden set this
	// model deletes.
	for _, needed := range []string{"/usr", "/bin", "/etc/resolv.conf", "/etc/ssl/certs", "/run/systemd/resolve"} {
		if _, ok := ro[needed]; !ok {
			t.Errorf("base view lacks %s: apps cannot run without it", needed)
		}
	}
	if _, whole := ro["/etc"]; whole {
		t.Error("/etc is bound wholesale: every app would see every other app's env_file")
	}
	// Nothing but the base view and the app's own dirs is bound at all:
	// nothing widens a view, so
	// the rendered set IS the guarantee.
	for dest := range ro {
		if !slices.Contains(sandboxBaseView, dest) {
			t.Errorf("BindReadOnlyPaths carries %s, which is neither a base-view entry nor one of the app's own dirs", dest)
		}
	}
	if unset, _ := got["UnsetEnvironment"].([]string); !reflect.DeepEqual(unset, []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}) {
		t.Errorf("UnsetEnvironment = %v", unset)
	}
}

// TestSandboxViewIsExactlyWhatIsNamed is the guarantee as an assertion:
// with TemporaryFileSystem=/ the set of bind destinations IS the view,
// so anything that widens it accidentally fails here rather than in
// production. A path that is not a bind destination does not exist
// inside the unit.
func TestSandboxViewIsExactlyWhatIsNamed(t *testing.T) {
	spec := &sandboxSpec{
		root:    "/var/lib/liveswap",
		appDir:  "/var/lib/liveswap/blog",
		appName: "blog",
		writable: []bindPath{{dest: "/var/lib/liveswap/blog/releases/v3"},
			{dest: "/var/lib/liveswap/blog/shared"}},
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: spec})
	// The unit's own /dev/shm sits inside the private /dev, which
	// PrivateDevices= builds within the empty root: it names nothing
	// of the host's.
	if tmp, _ := got["TemporaryFileSystem"].([]tmpfsMount); !reflect.DeepEqual(tmp, []tmpfsMount{{"/", "ro"}, {"/dev/shm", "mode=1777,size=50%"}}) {
		t.Fatalf("the view is not deny-by-default: TemporaryFileSystem = %+v", tmp)
	}
	view := map[string]bool{}
	for _, name := range []string{"BindPaths", "BindReadOnlyPaths"} {
		for _, b := range got[name].([]bindMount) {
			if view[b.Destination] {
				t.Errorf("%s is bound twice", b.Destination)
			}
			view[b.Destination] = true
		}
	}
	want := map[string]bool{
		"/var/lib/liveswap/blog/releases/v3": true,
		"/var/lib/liveswap/blog/shared":      true,
	}
	for _, p := range sandboxBaseView {
		want[p] = true
	}
	if !reflect.DeepEqual(view, want) {
		for p := range view {
			if !want[p] {
				t.Errorf("the view contains %s, which nothing named", p)
			}
		}
		for p := range want {
			if !view[p] {
				t.Errorf("the view lacks %s", p)
			}
		}
	}
	// The named consequences: not one of these is bound, so not one of
	// them exists inside the unit — no InaccessiblePaths= entry needed,
	// and no way for the list to go stale.
	for _, absent := range []string{
		"/var/lib/liveswap", "/var/lib/liveswap/blog", "/var/lib/liveswap/blog/state.json",
		"/var/lib/liveswap/blog/tmp", "/var/lib/liveswap/shop",
		"/var/lib/motel", "/run/motel", "/etc/motel", "/etc/liveswap",
		"/var/lib/motel-box", "/etc/blog/blog.env", "/run/user", "/home", "/opt", "/srv", "/var/lib",
		// Nothing widens a view any more, so a same-box database
		// socket dir and an out-of-tree cache are absent like
		// everything else nobody named.
		"/run/postgresql", "/var/cache/blog",
	} {
		if view[absent] {
			t.Errorf("%s is in the view", absent)
		}
		if spec.inView(absent) {
			t.Errorf("inView says %s is in the view, but nothing binds it", absent)
		}
	}
	for _, present := range []string{
		"/var/lib/liveswap/blog/releases/v3/server", "/var/lib/liveswap/blog/shared/db.sqlite",
		"/usr/bin/node", "/bin/sh", "/etc/ssl/certs",
	} {
		if !spec.inView(present) {
			t.Errorf("inView says %s is absent, but it is under a bind", present)
		}
	}
}

func TestSandboxSpecFor(t *testing.T) {
	spec := testSpec(t)
	rel := spec.dirs.release("v1")
	got := spec.sandboxSpecFor(rel, recordedNonce)
	if got == nil || got.root != spec.dirs.root {
		t.Fatalf("root wrong: %+v", got)
	}
	want := []bindPath{
		{dest: rel, source: rel},
		{dest: spec.dirs.shared, source: spec.dirs.shared},
		{dest: spec.dirs.runDir(recordedNonce), source: spec.dirs.runDir(recordedNonce)},
	}
	if !reflect.DeepEqual(got.writable, want) {
		t.Fatalf("writable = %v, want %v", got.writable, want)
	}
	// The view never includes state.json, tmp/ (upload staging), the
	// other releases, or the run/ parent holding every other
	// instance's socket: nothing but the three dirs above is bound.
	for _, p := range []string{spec.dirs.state, spec.dirs.tmp, spec.dirs.proxy, spec.dirs.run, spec.dirs.releases, spec.dirs.app} {
		for _, w := range got.writable {
			if w.dest == p || w.source == p {
				t.Errorf("%s must not be in the writable view", p)
			}
		}
	}
	// appName, not the resolved app dir, is what the launch-time check
	// derives its expected base from — see resolveBindSources.
	if got.appName != spec.name || got.appDir != spec.dirs.app {
		t.Fatalf("appName/appDir = %q/%q, want %q/%q", got.appName, got.appDir, spec.name, spec.dirs.app)
	}
	if !filepath.IsAbs(got.root) {
		t.Fatalf("root must be absolute: %q", got.root)
	}
}

func TestSandboxProbeCommand(t *testing.T) {
	full := sandboxProbeCommand("0a1b2c3d0a1b2c3d")
	if full[0] != "/bin/sh" || full[1] != "-c" || len(full) != 3 {
		t.Fatalf("probe must be a single sh -c script, got %v", full)
	}
	// The unit's argv reaches the shell byte for byte (ExecStartEx with
	// no-env-expand), so the shell's pid is spelled "$$" — and "$$$$",
	// the spelling systemd's substitution once needed, would now reach
	// the shell as the pid twice and fail the check on every host.
	if !strings.Contains(full[2], "uid_map") || !strings.Contains(full[2], `"$$" = 1`) {
		t.Fatalf("full probe checks both namespaces: %q", full[2])
	}
	// And the IPC namespace against the one motel passes in, and a
	// write into its /dev/shm named by the probe's nonce (#311).
	if !strings.Contains(full[2], `"$i" != "$MOTEL_IPC_NS"`) || !strings.Contains(full[2], `stat -c %u /dev/mqueue`) || !strings.Contains(full[2], probeShmCanary("0a1b2c3d0a1b2c3d")) {
		t.Fatalf("full probe checks the IPC namespace and /dev/shm: %q", full[2])
	}
	// And seccomp: the manager skips every seccomp-backed setting where
	// seccomp is unavailable to it, so the probe requires the filter
	// mode in its own status, read by the shell itself.
	if !strings.Contains(full[2], "< /proc/self/status") || !strings.Contains(full[2], `[ "$s" = 2 ]`) {
		t.Fatalf("full probe checks the syscall filter is in force: %q", full[2])
	}
	// And that the unit's own seccomp set is there, not a mode inherited
	// from the manager: RestrictNamespaces= refuses a user namespace with
	// EPERM, and is skipped exactly when the whole set is. Refused means
	// EPERM, never just a non-zero exit (a missing unshare must not pass).
	for _, clause := range []string{"unshare -U true", `*"Operation not permitted"*) u=refused`, `[ "$u" = refused ]`} {
		if !strings.Contains(full[2], clause) {
			t.Fatalf("full probe lacks %q: %q", clause, full[2])
		}
	}
	if strings.Contains(full[2], "$$$$") {
		t.Fatalf("$$$$ reaches the shell as the pid twice: %q", full[2])
	}
}

// probeRunner scripts RunOnce for probeSandboxCapability: fail is the
// answer to every attempt; queue, when non-empty, scripts successive
// calls instead (first entry first), for the retry cases where the two
// attempts must answer differently.
type probeRunner struct {
	*fakeRunner
	fail  error
	queue []error
	specs []startSpec
}

func (p *probeRunner) RunOnce(_ context.Context, spec startSpec) error {
	p.specs = append(p.specs, spec)
	if len(p.queue) > 0 {
		err := p.queue[0]
		p.queue = p.queue[1:]
		return err
	}
	return p.fail
}

// TestManagerSeamNeedsNoFuncSeams pins the promise on App.manager: an
// App with only a fake connection installed touches no real socket.
// Before this, probeManager and measureSandbox fell through to the
// process-wide client whenever the func seams were nil — a test that
// set manager alone would dial the real $XDG_RUNTIME_DIR socket and
// start a real probe unit on the machine running the tests.
func TestManagerSeamNeedsNoFuncSeams(t *testing.T) {
	conn := newFakeSystemdConn()
	a := &App{logger: zap.NewNop(), manager: conn}
	if err := a.probeManager(); err != nil {
		t.Fatalf("an installed connection is reachable by construction: %v", err)
	}
	if err := a.measureSandbox(); err != nil {
		t.Fatalf("the fake answers done, so the measurement through it must be capable: %v", err)
	}
	// The measurement went through the installed connection, not the
	// process-wide client: the fake saw the probe's own unit.
	probed := false
	for _, u := range conn.started {
		if strings.Contains(u.Name, "sandboxprobe_") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("no probe unit reached the installed connection; started: %v", len(conn.started))
	}
}

// fakeManagerClient stands in for the process-wide userManager: a
// scripted connection, plus the two things only the real client has —
// the reachability probe and the capability cache. It counts both so a
// test can assert which one ran, and in what order.
type fakeManagerClient struct {
	*fakeSystemdConn
	probeErr error
	probes   int
	measured int
}

func (f *fakeManagerClient) probe() error {
	f.probes++
	return f.probeErr
}

func (f *fakeManagerClient) cachedSandboxCapability(measure func() error) error {
	f.measured++
	return measure()
}

// TestMeasureSandboxProvesTheManagerBeforeMeasuring covers the branch
// every real start takes — no connection and no func seam installed, so
// probeManager and measureSandbox fall through to the process-wide
// client. It is the processManager counterpart of
// TestManagerSeamNeedsNoFuncSeams: same promise (no real socket), but
// reaching the fall-through itself rather than the branch above it.
//
// What it pins is an ordering whose failure is silent. Drop or reorder
// measureSandbox's probe() and a manager that died between Start's
// probeManager and here comes back as "this host cannot deliver the
// sandbox" — a refusal whose own text says there is no fix in config —
// instead of the error naming the uid, the socket and the lingering to
// enable. motel refuses to come up either way; only the operator's
// next hour differs.
func TestMeasureSandboxProvesTheManagerBeforeMeasuring(t *testing.T) {
	newApp := func(probeErr error) (*App, *fakeManagerClient) {
		mgr := &fakeManagerClient{fakeSystemdConn: newFakeSystemdConn(), probeErr: probeErr}
		// processManager alone: no manager, no func seams, so both
		// methods take the fall-through this test exists for.
		return &App{logger: zap.NewNop(), processManager: mgr}, mgr
	}

	t.Run("an unreachable manager is reported as itself", func(t *testing.T) {
		gone := errors.New("liveswap needs the systemd user manager for uid 1000: loginctl enable-linger")
		a, mgr := newApp(gone)
		err := a.measureSandbox()
		if !errors.Is(err, gone) {
			t.Fatalf("the manager's own reason must come back verbatim, so Start's refusal names the remedy; got %v", err)
		}
		if mgr.measured != 0 {
			t.Errorf("a manager that is gone was measured anyway (%d times): its absence would be reported as an incapable host", mgr.measured)
		}
		if len(mgr.started) != 0 {
			t.Errorf("a dead manager must cost no probe unit; started %d", len(mgr.started))
		}
	})

	t.Run("a reachable manager is measured over the client that was probed", func(t *testing.T) {
		a, mgr := newApp(nil)
		if err := a.measureSandbox(); err != nil {
			t.Fatalf("the fake answers done, so the measurement must be capable: %v", err)
		}
		if mgr.probes != 1 || mgr.measured != 1 {
			t.Fatalf("probed %d times and measured %d, want 1 and 1", mgr.probes, mgr.measured)
		}
		// Through the cache, and over the same client: the probe unit
		// reached the connection whose probe() had just answered.
		probed := false
		for _, u := range mgr.started {
			if strings.Contains(u.Name, "sandboxprobe_") {
				probed = true
			}
		}
		if !probed {
			t.Fatalf("no probe unit reached the probed client; started: %d", len(mgr.started))
		}
	})

	t.Run("probeManager reaches the same client", func(t *testing.T) {
		gone := errors.New("no manager")
		a, mgr := newApp(gone)
		if err := a.probeManager(); !errors.Is(err, gone) {
			t.Fatalf("probeManager must fall through to the installed client, got %v", err)
		}
		if mgr.probes != 1 {
			t.Fatalf("probed %d times, want 1", mgr.probes)
		}
	})

	t.Run("so does the connection this config's apps use", func(t *testing.T) {
		// Same promise as manager: set processManager alone and nothing
		// this App does touches a real socket. Measuring through the
		// fake and then handing Start's runners and its unknown-app
		// sweep the real $XDG_RUNTIME_DIR client would stop units
		// belonging to whoever is running a manager on this machine.
		a, mgr := newApp(nil)
		if a.systemdConn() != systemdConn(mgr) {
			t.Fatal("systemdConn fell through to the process-wide client past an installed fake")
		}
	})
}

// TestProbeSandboxCapabilityRetriesTimeout pins the one retry, and
// its shape. The probe's verdict decides whether motel starts at
// all, and a refused start (exit 1) is the one exit motel.service
// never retries — so a transient timeout under boot load must get a
// second attempt in-process before it becomes a refusal to start.
// Only a timeout: a probe that FAILED measured the host, and retrying
// a measurement would just slow every refusal down.
func TestProbeSandboxCapabilityRetriesTimeout(t *testing.T) {
	t.Run("a timeout is retried and the host recovers", func(t *testing.T) {
		r := &probeRunner{fakeRunner: &fakeRunner{}, queue: []error{context.DeadlineExceeded, nil}}
		if err := probeSandboxCapability(r); err != nil {
			t.Fatalf("one slow attempt must not decide the start: %v", err)
		}
		if len(r.specs) != 2 {
			t.Fatalf("tried %d times, want 2", len(r.specs))
		}
	})
	t.Run("a second timeout is the verdict", func(t *testing.T) {
		r := &probeRunner{fakeRunner: &fakeRunner{}, queue: []error{context.DeadlineExceeded, context.DeadlineExceeded}}
		err := probeSandboxCapability(r)
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("want a refusal with the timeout in the reason, got %v", err)
		}
		if len(r.specs) != 2 {
			t.Fatalf("tried %d times, want 2 — the retry is bounded", len(r.specs))
		}
	})
	// A fast failure is not retried; TestProbeSandboxCapability pins
	// that with its exact one-attempt assertions.
}

func TestProbeSandboxCapability(t *testing.T) {
	denied := errors.New("unit failed: Failed to set up user namespacing: Permission denied")
	cases := []struct {
		name      string
		fail      error
		wantWords []string // nil: the host is capable
	}{
		{"host delivers it", nil, nil},
		// A manager that accepts the properties but a kernel that
		// silently ignores PrivatePIDs=: the probe exits non-zero
		// because the process is not pid 1. There is no lower tier to
		// fall to, so this is a refusal — the operator must see why.
		{"kernel ignores the PID namespace", errors.New("exit status 1"), []string{"sandbox probe unit", "exit status 1", "journalctl -t motel-sandbox-probe", "Debian 13", "systemd 257"}},
		// A container, an LXC VPS, or a kernel built without user
		// namespaces: the manager refuses the unit outright.
		{"userns denied", denied, []string{"Permission denied", "Debian 13", "systemd 257"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &probeRunner{fakeRunner: &fakeRunner{}, fail: tc.fail}
			err := probeSandboxCapability(r)
			if (err != nil) != (tc.wantWords != nil) {
				t.Fatalf("verdict = %v, want capable=%v", err, tc.wantWords == nil)
			}
			// One sandbox means exactly one attempt: a probe that tried
			// a lesser property set would be measuring a capability
			// nothing can be launched with.
			if len(r.specs) != 1 {
				t.Fatalf("tried %d times, want exactly 1", len(r.specs))
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("reason %q lacks %q", err, w)
				}
			}
			for _, s := range r.specs {
				// The probe carries the sandbox and nothing else: with a
				// deny-by-default view there is no per-host state for it
				// to reproduce, and it exposes not one path of its own.
				if s.sandbox == nil || len(s.sandbox.writable) != 0 || s.sandbox.root != "" {
					t.Errorf("probe unit must carry the sandbox and expose nothing: %+v", s.sandbox)
				}
				if !reflect.DeepEqual(s.command, sandboxProbeCommand(s.nonce)) {
					t.Errorf("probe command mismatch: %v", s.command)
				}
				// The script matches unshare's English EPERM text, and a
				// unit gets the manager's default environment (LANG
				// included) under its own: without LC_ALL=C a translated
				// strerror would refuse a capable host.
				if !slices.Contains(s.env, "LC_ALL=C") {
					t.Errorf("probe env %v lacks LC_ALL=C", s.env)
				}
				if !s.probe {
					t.Error("the probe unit must be marked as one, or a sweep can mistake it for an app's")
				}
			}
		})
	}
}

func TestBuildEnvSandboxedHome(t *testing.T) {
	spec := testSpec(t)
	t.Setenv("HOME", "/var/lib/motel")
	sandboxed, err := buildEnv(spec, "v1", spec.dirs.socket("0a1b2c3d0a1b2c3d"), spec.dirs.release("v1"))
	must(t, err)
	find := func(env []string, key string) (string, int) {
		var val string
		n := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				val = strings.TrimPrefix(kv, key+"=")
				n++
			}
		}
		return val, n
	}
	// The inherited HOME (motel's state dir, absent from the view)
	// never reaches the unit: replaced, not merely shadowed.
	if v, n := find(sandboxed, "HOME"); v != spec.dirs.shared || n != 1 {
		t.Fatalf("sandboxed HOME = %q (×%d), want %s once", v, n, spec.dirs.shared)
	}
	// env_file / inline env still win: they come later.
	spec.env = map[string]string{"HOME": "/elsewhere"}
	sandboxed, err = buildEnv(spec, "v1", spec.dirs.socket("0a1b2c3d0a1b2c3d"), spec.dirs.release("v1"))
	must(t, err)
	if v, _ := find(sandboxed, "HOME"); !strings.HasSuffix(strings.Join(sandboxed, "\n"), "SOCKET="+spec.dirs.socket("0a1b2c3d0a1b2c3d")) || v == "" {
		t.Fatalf("env ordering broken: %v", sandboxed)
	}
	last := ""
	for _, kv := range sandboxed {
		if strings.HasPrefix(kv, "HOME=") {
			last = kv
		}
	}
	if last != "HOME=/elsewhere" {
		t.Fatalf("an explicit HOME must come after the sandbox default, got last %q", last)
	}
}

// TestEveryLaunchIsSandboxed: there is one sandbox and every path that
// starts a unit applies it — a deploy, its pre_start, and a relaunch
// from the record (recovery, and the watchdog through the same
// launchVersion) — with nothing read from state.json deciding how much
// isolation an instance gets.
func TestEveryLaunchIsSandboxed(t *testing.T) {
	rig := newTestRig(t)
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/a.tgz", version: "v1"}))
	want := rig.spec.sandboxSpecFor(rig.spec.dirs.release("v1"), rig.runner.started[0].nonce)
	if got := rig.runner.started[0].sandbox; !reflect.DeepEqual(got, want) {
		t.Fatalf("deploy sandbox = %+v, want %+v", got, want)
	}
	// pre_start runs under the same sandbox.
	rig.spec.preStart = []string{"./migrate"}
	must(t, rig.ma.Deploy(context.Background(), deployRequest{url: "https://x/b.tgz", version: "v2"}))
	// The fake records RunOnce beside Start: pre_start, then the app.
	if n := len(rig.runner.started); n != 3 {
		t.Fatalf("started = %d, want deploy, pre_start, deploy", n)
	}
	for i := 1; i <= 2; i++ {
		want2 := rig.spec.sandboxSpecFor(rig.spec.dirs.release("v2"), rig.runner.started[i].nonce)
		if got := rig.runner.started[i].sandbox; !reflect.DeepEqual(got, want2) {
			t.Fatalf("started[%d] sandbox = %+v, want %+v", i, got, want2)
		}
	}

	// A relaunch from a record is sandboxed like a deploy.
	rig2 := newTestRig(t)
	must(t, rig2.store.save(appState{CurrentVersion: "v1", Nonce: "0a1b2c3d0a1b2c3d", Handle: handleState{Unit: "motel-demo.v1.0a1b2c3d0a1b2c3d.service"}}))
	must(t, mkdirRelease(rig2.spec, "v1"))
	rig2.runner.reattachOK = false
	if err := rig2.ma.ensureRunning(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	want = rig2.spec.sandboxSpecFor(rig2.spec.dirs.release("v1"), rig2.runner.started[0].nonce)
	if got := rig2.runner.started[0].sandbox; !reflect.DeepEqual(got, want) {
		t.Fatalf("relaunch sandbox = %+v, want %+v", got, want)
	}
}

// TestRetiredSandboxConfigIsRejected: there is no sandbox config
// surface. `sandbox on|off` and `extra_path` were both removed, and
// neither may be silently accepted in either config form — a config
// that names them is asking for a policy this build does not have,
// and an operator who wrote `sandbox off` must not get a sandboxed app
// believing they got none. The Caddyfile arms are gone, and Caddy
// loads module JSON with StrictUnmarshalJSON (DisallowUnknownFields),
// which reaches the nested app objects too. (No "was replaced" alias,
// as allowed_artifact_hosts got: there is no installed base with
// these in a config.)
func TestRetiredSandboxConfigIsRejected(t *testing.T) {
	for _, bad := range []string{
		"liveswap {\n sandbox on\n app x {\n command a\n }\n}",
		"liveswap {\n sandbox off\n app x {\n command a\n }\n}",
		"liveswap {\n app x {\n command a\n sandbox off\n }\n}",
		"liveswap {\n app x {\n command a\n extra_path /p\n }\n}",
	} {
		var a App
		if err := a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	for _, bad := range []string{
		`{"sandbox":"off","apps":{"x":{"command":["a"]}}}`,
		`{"apps":{"x":{"command":["a"],"sandbox":"off"}}}`,
		`{"apps":{"x":{"command":["a"],"extra_path":["/p"]}}}`,
	} {
		var a App
		if err := caddy.StrictUnmarshalJSON([]byte(bad), &a); err == nil {
			t.Errorf("JSON accepted: %s", bad)
		}
	}
	// And the control: the same JSON without the retired key loads.
	var a App
	if err := caddy.StrictUnmarshalJSON([]byte(`{"apps":{"x":{"command":["a"]}}}`), &a); err != nil {
		t.Fatalf("a sound JSON config was refused: %v", err)
	}
}

// TestStartRefusesAnIncapableHost drives App.Start with a scripted host
// that cannot deliver the sandbox — a container, or a kernel that
// refuses user namespaces — and with one that can. The refusal names
// the probe's reason and where its output went; there is no setting
// that starts anyway, so nothing else to test.
func TestStartRefusesAnIncapableHost(t *testing.T) {
	newApp := func(verdict error) (*App, *int) {
		spec := testSpec(t)
		// A managed app, not an empty map: Start gates the
		// reachability probe on there being one, so an empty pool
		// leaves that seam dead and the ordering it guarantees —
		// manager reachable BEFORE the host is measured — unasserted.
		rig := newTestRig(t)
		rig.spec = spec
		rig.ma.spec = spec
		// Start installs its own watchdog context over the rig's, so
		// the rig's t.Cleanup no longer reaches it; stop it explicitly
		// or goleak fails the package on the leaked goroutine.
		t.Cleanup(rig.ma.stopWatchdog)
		managerProbes := 0
		measuredHere := false
		return &App{
			Root:    spec.dirs.root,
			logger:  zap.NewNop(),
			specs:   map[string]*appSpec{"demo": spec},
			managed: map[string]*managedApp{"demo": rig.ma},
			clients: &fetchClients{},
			// A fake connection, so the unknown-app sweep Start spawns
			// cannot reach a real manager and stop units belonging to
			// whoever is running one on this machine.
			manager: newFakeSystemdConn(),
			managerProbe: func() error {
				managerProbes++
				measuredHere = true
				return nil
			},
			sandboxProbe: func(*zap.Logger) error {
				if !measuredHere {
					t.Error("this App measured the host before proving its manager reachable")
				}
				return verdict
			},
		}, &managerProbes
	}
	a, probes := newApp(errors.New("sandbox probe unit: unit failed: Failed to set up user namespacing: Permission denied"))
	err := a.Start()
	if err == nil {
		t.Fatal("a host without the sandbox must refuse to start")
	}
	for _, w := range []string{"Permission denied", "no setting"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal must carry %q: %v", w, err)
		}
	}
	_ = a.Cleanup()
	if *probes != 1 {
		t.Fatalf("manager reachability probed %d times, want 1", *probes)
	}

	a, probes = newApp(nil)
	if err := a.Start(); err != nil {
		t.Fatalf("a capable host must start: %v", err)
	}
	_ = a.Cleanup()
	if *probes != 1 {
		t.Fatalf("manager reachability probed %d times, want 1", *probes)
	}
}

// TestStartLogsWhatItStarted: a fresh install's journal carries one
// line saying liveswap is up and how many apps it holds — the
// confirmation a first user looks for before deploying anything. With
// no apps configured Start skips the manager and sandbox probes, so
// the line is the only evidence the module loaded at all.
func TestStartLogsWhatItStarted(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	a := &App{Root: t.TempDir(), logger: zap.New(core), manager: newFakeSystemdConn()}
	if err := a.Start(); err != nil {
		t.Fatalf("Start with no apps: %v", err)
	}
	_ = a.Cleanup()
	got := logs.FilterMessage("liveswap started").All()
	if len(got) != 1 {
		t.Fatalf("want one 'liveswap started' line, got %d", len(got))
	}
	if n := got[0].ContextMap()["apps"]; n != int64(0) {
		t.Fatalf("apps = %v, want 0", n)
	}

	// With apps, the names are listed sorted — a map walk would print
	// them in a different order on every start, and the line is meant
	// to be compared across boots.
	core, logs = observer.New(zap.InfoLevel)
	managed := map[string]*managedApp{}
	specs := map[string]*appSpec{}
	for _, name := range []string{"zeta", "alpha"} {
		rig := newTestRig(t)
		t.Cleanup(rig.ma.stopWatchdog)
		managed[name], specs[name] = rig.ma, rig.spec
	}
	a = &App{
		Root: t.TempDir(), logger: zap.New(core), specs: specs, managed: managed,
		clients: &fetchClients{}, manager: newFakeSystemdConn(),
		managerProbe: func() error { return nil },
		sandboxProbe: func(*zap.Logger) error { return nil },
	}
	if err := a.Start(); err != nil {
		t.Fatalf("Start with two apps: %v", err)
	}
	_ = a.Cleanup()
	got = logs.FilterMessage("liveswap started").All()
	if len(got) != 1 {
		t.Fatalf("want one 'liveswap started' line, got %d", len(got))
	}
	if names := got[0].ContextMap()["app_names"]; !reflect.DeepEqual(names, []any{"alpha", "zeta"}) {
		t.Fatalf("app_names = %#v, want [alpha zeta]", names)
	}
}

// TestSandboxRootUnderTmpIsAllowed: only motel's own state is a
// root worth failing config load over. A root under a path the sandbox
// replaces (/tmp, /var/tmp — every t.TempDir, and the real-systemd
// integration lane's own root) still works, because systemd creates
// the mount point for TemporaryFileSystem= and BindPaths= inside the
// namespace; refusing it would have turned an odd-but-working setup
// into a server that will not start.
func TestSandboxRootUnderTmpIsAllowed(t *testing.T) {
	for _, root := range []string{"/tmp/liveswap-test", "/var/tmp/x", "/home/deploy/apps", "/srv/apps"} {
		if err := validateSandboxRoot(root); err != nil {
			t.Errorf("root %s must not fail config load: %v", root, err)
		}
	}
}

// TestEnvFilesAreAbsentWithoutBeingListed is what replaced the derived
// hidden set: no app's env_file is named anywhere in a sibling's spec,
// and none of them is reachable inside its view — not because they
// were enumerated and masked, but because nothing bound them. This is
// the same guarantee the old set gave only for the paths it happened
// to know about, and only for units started after they were declared.
func TestEnvFilesAreAbsentWithoutBeingListed(t *testing.T) {
	t.Setenv("SECRETS", "/etc/secrets")
	a := &App{
		Root:              "/var/lib/liveswap",
		ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
		DeployTrust:       githubTrust(),
		Apps: map[string]*AppConfig{
			"blog": {Command: []string{"x"}, EnvFile: "{env.SECRETS}/blog.env"},
			"shop": {Command: []string{"x"}, EnvFile: "{env.SECRETS}/shop.env"},
		},
	}
	repl := caddy.NewReplacer()
	for name, cfg := range a.Apps {
		cfg.applyDefaults(repl)
		spec, err := a.buildSpec(name, cfg)
		if err != nil {
			t.Fatal(err)
		}
		sb := spec.sandboxSpecFor(spec.dirs.release("v1"), recordedNonce)
		for _, secret := range []string{"/etc/secrets/blog.env", "/etc/secrets/shop.env", "/etc/secrets"} {
			if sb.inView(secret) {
				t.Errorf("%s's view reaches %s", name, secret)
			}
		}
		// And an env_file declared *later* — the case the snapshot
		// model could not cover, because a running unit's view is never
		// rebuilt — is absent for exactly the same reason.
		if sb.inView("/etc/secrets/added-tomorrow.env") {
			t.Errorf("%s's view reaches an env_file that does not exist yet", name)
		}
	}
}

// TestWarnEnvFileInView: the one place an env_file can still land in a
// view is the app's own directories, which are the only host paths
// bound writable. Its own environment is legitimately reachable, but
// under shared/ the app can rewrite the file and so choose its own
// next launch's environment — worth saying out loud at config load.
func TestWarnEnvFileInView(t *testing.T) {
	spec := testSpec(t)
	for _, tc := range []struct {
		name    string
		envFile string
		want    int
	}{
		{"outside every view", "/etc/motel/demo.env", 0},
		{"in shared", filepath.Join(spec.dirs.shared, "demo.env"), 1},
		{"in releases", filepath.Join(spec.dirs.releases, "v1", "demo.env"), 1},
		{"no env_file", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			s := *spec
			s.envFile = tc.envFile
			warnEnvFileInView(zap.New(core), map[string]*appSpec{"demo": &s})
			if got := logs.Len(); got != tc.want {
				t.Fatalf("warned %d times, want %d: %v", got, tc.want, logs.All())
			}
		})
	}
	// A nil logger is the pre-Provision case (Validate called directly
	// by a test or by `caddy validate`); it must not panic.
	warnEnvFileInView(nil, map[string]*appSpec{"demo": spec})

	// A symlink must not walk past the warning. An env_file that only
	// reaches shared/ through a link is exactly as rewritable by the
	// app, and a warning a symlink defeats is worse than none: it reads
	// as "checked, and fine".
	t.Run("through a symlink into shared", func(t *testing.T) {
		outside, err := os.MkdirTemp("/var", "envlink-")
		if err != nil {
			t.Skipf("no writable dir outside the refused prefixes: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(outside) })
		must(t, os.MkdirAll(spec.dirs.shared, 0o750))
		target := filepath.Join(spec.dirs.shared, "demo.env")
		must(t, os.WriteFile(target, []byte("A=1\n"), 0o600))
		link := filepath.Join(outside, "demo.env")
		must(t, os.Symlink(target, link))

		core, logs := observer.New(zap.WarnLevel)
		s := *spec
		s.envFile = link
		warnEnvFileInView(zap.New(core), map[string]*appSpec{"demo": &s})
		if logs.Len() != 1 {
			t.Fatalf("an env_file linked into shared/ must warn: %v", logs.All())
		}
	})
}

// TestValidateRejectsUncleanRoot: every containment check is lexical,
// so a root spelled with .. would let a bind reach a sibling
// through a spelling the check never sees.
func TestValidateRejectsUncleanRoot(t *testing.T) {
	base := func(root string) *App {
		a := &App{Root: root, ArtifactAllowlist: []string{"github.com/smallhoursorg/"},
			DeployTrust: githubTrust(), Apps: map[string]*AppConfig{"blog": defaultedApp(t)}}
		return a
	}
	if err := base("/var/lib/liveswap").Validate(); err != nil {
		t.Fatalf("clean root rejected: %v", err)
	}
	for _, bad := range []string{"/srv/liveswap/../liveswap", "/srv//liveswap", "/srv/liveswap/."} {
		if err := base(bad).Validate(); err == nil {
			t.Errorf("unclean root %q accepted", bad)
		}
	}
}

// TestValidateSandboxRootRefusesUnitSyntax: a root with a character a
// unit file's path grammar owns is refused at config load, named. The
// manager writes BindPaths= and the rest to the unit's transient file
// unquoted and reads it back at every daemon-reload; measured (#380),
// a space split the binds, a colon emptied them, a backslash left the
// unit bad-setting, a tab or a quote a unit that never ran. '%', ';'
// and non-ASCII letters came back unchanged, and stay allowed.
func TestValidateSandboxRootRefusesUnitSyntax(t *testing.T) {
	for _, bad := range []string{"/srv/live swap", "/srv/live:swap", `/srv/live\swap`, "/srv/live\tswap", `/srv/live"swap`, "/srv/live'swap", "/srv/live\x7fswap"} {
		err := validateSandboxRoot(bad)
		if err == nil || !strings.Contains(err.Error(), "cannot carry in a path") {
			t.Errorf("root %q accepted (%v)", bad, err)
		}
	}
	for _, ok := range []string{"/srv/liveswap", "/srv/live%swap", "/srv/live;swap", "/srv/livéswap", "/srv/live-swap.d_1"} {
		if err := validateSandboxRoot(ok); err != nil {
			t.Errorf("root %q refused: %v", ok, err)
		}
	}
	// A root that is a link to such a path: refused naming the target,
	// which is where the character is.
	root := filepath.Join(t.TempDir(), "liveswap")
	target := filepath.Join(t.TempDir(), "live swap")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxRoot(root); err == nil || !strings.Contains(err.Error(), "which resolves to "+strconv.Quote(target)+", contains a space") {
		t.Errorf("a root linking to a path with a space must be refused naming the target: %v", err)
	}
	// And a source resolved at launch, for a root re-pointed since the
	// config loaded: the same refusal, from the bind-source check.
	if err := refusedAsBindSource("/run/live swap/blog/shared"); err == nil || !strings.Contains(err.Error(), "contains a space") {
		t.Errorf("a resolved bind source with a space must be refused: %v", err)
	}
	if err := refusedAsBindSource("/run/liveswap/blog/shared"); err != nil {
		t.Errorf("a plain resolved bind source refused: %v", err)
	}
}

// TestResolveBindSourcesRefusesPlantedSymlink: `shared` and the
// release dirs live under the shared motel UID and
// appDirs.ensure's MkdirAll succeeds on a symlink, so a link planted
// at an app's shared dir — pointing at the user manager's socket
// directory — would have BindPaths= mount that inside the sandbox on
// the next launch. Nothing sandboxed can plant one (the mount points
// are not unlinkable from inside a view), so this is the guard that
// keeps that true for whatever was on disk before.
func TestResolveBindSourcesRefusesPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	sibling := filepath.Join(root, "shop", "shared")
	// "Outside" areas live under /run: the root itself is a t.TempDir
	// under /tmp, which is a closed prefix, so a link resolving there
	// would be refused for that reason instead of the one under test.
	outside, err := os.MkdirTemp("/run", "bindsrc-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	elsewhere := filepath.Join(outside, "data")
	for _, d := range []string{release, shared, sibling, elsewhere} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// These have to exist for the containment branch to be the one that
	// refuses them: a link to a path that does not resolve is refused
	// by the missing-source case instead — fail-closed either way, but
	// a different rule than the one under test. Neither is present in
	// the plain build container.
	for _, p := range []string{"/run/motel", "/run/user"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			continue
		}
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Skipf("cannot create %s: %v", p, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(p) })
	}
	spec := func(sharedPath string) *sandboxSpec {
		return &sandboxSpec{root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: sharedPath, source: sharedPath}}}
	}
	// Baseline: real directories resolve to themselves and are kept.
	s := spec(shared)
	if err := s.resolveBindSources(); err != nil {
		t.Fatalf("plain directories refused: %v", err)
	}
	if !reflect.DeepEqual(s.writable, []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}) {
		t.Fatalf("writable rewritten unnecessarily: %v", s.writable)
	}

	link := filepath.Join(appDir, "shared-link")
	// Targets that exist here, so resolution succeeds and the
	// containment check is what refuses them. (A link to a target that
	// does not resolve is refused too, by the missing-source case at
	// the end — fail-closed either way.)
	// Every planted target is refused by the same rule now: a mandatory
	// bind must BE the directory it names. The deny lists are no longer
	// what catches these — they are the backstop for a root re-aimed
	// after config load, tested directly elsewhere.
	for _, tc := range []struct{ name, target, wantIn string }{
		{"a closed tree", "/dev", "not the directory it names"},
		{"the private tmp", "/tmp", "not the directory it names"},
		{"motel's own state", "/run/motel", "not the directory it names"},
		{"the manager's socket dir", "/run/user", "not the directory it names"},
		{"a sibling app's data", sibling, "not the directory it names"},
		{"a sibling's data on another disk", elsewhere, "not the directory it names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			err := spec(link).resolveBindSources()
			if err == nil {
				t.Fatalf("a shared dir pointing at %s was bound into the sandbox", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}

	// A shared dir symlinked anywhere outside the root is refused too —
	// including somewhere that looks perfectly innocent. The app's own
	// directory is writable by the app, so a symlink placed there
	// cannot be told from one the operator meant; the refusal names the
	// mechanism that CAN be told apart.
	_ = os.Remove(link)
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	err = spec(link).resolveBindSources()
	if err == nil {
		t.Fatal("a shared dir symlinked outside the root was accepted; an app can plant that symlink itself")
	}
	if !strings.Contains(err.Error(), "bind-mount") {
		t.Fatalf("the refusal must name the supported alternative: %v", err)
	}

	// A missing bind source is an error, not a silently skipped bind.
	if err := spec(filepath.Join(appDir, "gone")).resolveBindSources(); err == nil {
		t.Fatal("a missing shared dir must fail the launch")
	}
}

// TestUnitForResolvesBindSources: the check runs at unit creation, the
// last point before systemd follows the paths, so a launch whose bind
// source was replaced after config load fails instead of mounting it.
func TestUnitForResolvesBindSources(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(appDir, "shared")
	// /dev stands in for /run/user/<uid> — the real target on a live
	// host, absent in the test container; both are closed prefixes.
	if err := os.Symlink("/dev", shared); err != nil {
		t.Fatal(err)
	}
	spec := startSpec{
		app: "blog", version: "v1", nonce: "0a1b2c3d0a1b2c3d", command: []string{"./server"}, dir: release,
		sandbox: &sandboxSpec{root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}},
	}
	if _, err := r.unitFor(spec, false); err == nil || !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("unitFor must refuse a planted bind source, got %v", err)
	}
	if _, err := r.Start(spec); err == nil {
		t.Fatal("Start must fail rather than launch with the planted bind")
	}
}

// TestBindDestinationsAreTheConfiguredPaths: a permitted symlink (the
// operator's `shared` on another disk) must change only where the data
// is read FROM. The app is told about its directories by
// WorkingDirectory, HOME and the {release_dir}/{shared_dir}
// placeholders, all of which name the configured path — and that path
// lives under the liveswap root, which the sandbox replaces with an
// empty tmpfs, so a bind that moved the destination would leave the
// app pointing at something that no longer exists inside its view.
func TestBindDestinationsAreTheConfiguredPaths(t *testing.T) {
	// The one way a bind source may legitimately differ from its
	// destination is an alias on the liveswap root itself, so that is
	// what this exercises. Built under /var: a t.TempDir root lives
	// under /tmp, and the app dirs must resolve somewhere the deny list
	// for bind sources does not object to.
	base, err := os.MkdirTemp("/var", "binddest-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	real := filepath.Join(base, "mnt-liveswap")
	release := filepath.Join(real, "blog", "releases", "v1")
	shared := filepath.Join(real, "blog", "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(base, "srv-liveswap")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRelease := filepath.Join(root, "blog", "releases", "v1")
	aliasShared := filepath.Join(root, "blog", "shared")
	sb := &sandboxSpec{root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
		writable: []bindPath{{dest: aliasRelease, source: aliasRelease}, {dest: aliasShared, source: aliasShared}}}
	if err := sb.resolveBindSources(); err != nil {
		t.Fatalf("a symlinked root was refused: %v", err)
	}
	got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: sb})
	binds := map[string]bindMount{}
	for _, b := range got["BindPaths"].([]bindMount) {
		binds[b.Destination] = b
	}
	b, ok := binds[aliasShared]
	if !ok {
		t.Fatalf("the shared dir is not bound where the app expects it (%s): %+v", aliasShared, binds)
	}
	// Only the SOURCE moves. The destination stays the configured path,
	// because that is what WorkingDirectory, HOME and the
	// {release_dir}/{shared_dir} placeholders name — binding the
	// resolved path at the resolved path would put the data somewhere
	// the app never looks, and its own spelling would be gone with the
	// tmpfs that replaces the filesystem.
	if b.Source != shared {
		t.Fatalf("shared bound from %q, want the resolved %q", b.Source, shared)
	}
	if _, moved := binds[shared]; moved {
		t.Fatal("the shared dir was bound at its resolved path: the app would never find it")
	}
}

// TestOverlapIsSymmetric: a path that CONTAINS a protected tree
// exposes it exactly as surely as one inside it, because the binds are
// recursive — `shared -> /run` carries /run/user (the manager's
// socket) and /run/motel (the admin socket) in with it, and
// a root under one of them would carry every app's data in.
func TestOverlapIsSymmetric(t *testing.T) {
	if err := refusedAsBindSource("/run"); err == nil {
		t.Error("a bind source containing /run/user was accepted at launch time")
	}
	if err := refusedAsBindSource("/etc"); err == nil {
		t.Error("a bind source containing motel's own config was accepted at launch time")
	}
}

// TestMandatoryBindMustBeTheDirectoryItNames: inside the root, a
// mandatory bind may differ from its configured path only by an
// ancestor's symlink. Pointing `shared` at the app's own dir or its
// releases passes a naive "is it inside appDir" test, and the
// recursive bind then carries state.json, the upload staging dir and
// every other release in under the shared path — the integrity
// boundary the design states as normative.
func TestMandatoryBindMustBeTheDirectoryItNames(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	for _, d := range []string{release, shared, filepath.Join(appDir, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(appDir, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(appDir, "shared-link")
	spec := func(sharedPath string) *sandboxSpec {
		return &sandboxSpec{root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: sharedPath, source: sharedPath}}}
	}
	for _, target := range []string{appDir, filepath.Join(appDir, "releases"), filepath.Join(appDir, "tmp"), root} {
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := spec(link).resolveBindSources()
		if err == nil {
			t.Errorf("a shared dir pointing at %s was accepted; its recursive bind would expose state.json, tmp/ and other releases", target)
			continue
		}
		if !strings.Contains(err.Error(), "not the directory it names") {
			t.Errorf("refused for the wrong reason: %v", err)
		}
	}
	// The legitimate difference — an ancestor is itself a symlink — must
	// still resolve cleanly. Built under /run: a t.TempDir root lives
	// under /tmp, which the sandbox replaces, so a source resolving back
	// into it would be refused for that reason instead of this one.
	base, err := os.MkdirTemp("/run", "rootalias-")
	if err != nil {
		t.Skipf("no writable dir outside the closed prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	realRoot := filepath.Join(base, "real")
	realShared := filepath.Join(realRoot, "blog", "shared")
	if err := os.MkdirAll(realShared, 0o750); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(base, "liveswap")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	aliasShared := filepath.Join(aliasRoot, "blog", "shared")
	viaAlias := &sandboxSpec{root: aliasRoot, appDir: filepath.Join(aliasRoot, "blog"), appName: "blog",
		writable: []bindPath{{dest: aliasShared, source: aliasShared}}}
	if err := viaAlias.resolveBindSources(); err != nil {
		t.Fatalf("a symlinked root must still work: %v", err)
	}
	if got := viaAlias.writable[0]; got.dest != aliasShared || got.source != filepath.Join(realRoot, "blog", "shared") {
		t.Fatalf("through a symlinked root the app must still see its dir where it names it, bound from the real one: %+v", got)
	}
}

// TestAppDirAliasToSiblingRefused: the expected base for a mandatory
// bind is derived from the canonical root plus the configured app
// name, never from what the app's own directory resolves to. Resolving
// appDir would let the alias vouch for itself — a link replacing
// <root>/blog with <root>/shop makes shop's
// directory the expected base, and both of blog's binds then match it
// exactly, handing the sibling's release and shared data to the new
// sandbox.
func TestAppDirAliasToSiblingRefused(t *testing.T) {
	root := t.TempDir()
	sibling := filepath.Join(root, "shop")
	for _, d := range []string{filepath.Join(sibling, "releases", "v1"), filepath.Join(sibling, "shared")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	appDir := filepath.Join(root, "blog")
	if err := os.Symlink(sibling, appDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	s := &sandboxSpec{root: root, appDir: appDir, appName: "blog",
		writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
	err := s.resolveBindSources()
	if err == nil {
		t.Fatalf("an app dir aliased to the sibling %s was accepted: blog would bind shop's release and shared data", sibling)
	}
	if !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestUnitForRefusesCommandOutsideTheView: LookPath runs in the
// supervisor's view of the filesystem, which under a deny-by-default
// sandbox is not the unit's. A runtime installed outside /usr —
// /opt/node/bin/node, an nvm shim under a home directory — resolves
// here and then does not exist in there, and the sibling shell reports
// that as `exec: …: not found`, exit status 127, after the unit has
// already been created.
func TestUnitForRefusesCommandOutsideTheView(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	shared := filepath.Join(appDir, "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// A runtime outside the base view. Under /var, which is not bound
	// at all (/run is, but the container mounts it noexec, so LookPath
	// would fail there for the wrong reason).
	outside, err := os.MkdirTemp("/var", "runtime-")
	if err != nil {
		t.Skipf("no writable dir outside the base view: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	runtime := filepath.Join(outside, "node")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb := func() *sandboxSpec {
		return &sandboxSpec{root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
	}
	base := startSpec{app: "blog", version: "v1", nonce: "0a1b2c3d0a1b2c3d", dir: release}

	spec := base
	spec.command, spec.sandbox = []string{runtime}, sb()
	_, err = r.unitFor(spec, false)
	if err == nil || !strings.Contains(err.Error(), "install it under /usr") {
		t.Fatalf("unitFor must refuse a command outside the view and name the fix, got %v", err)
	}
	// There is no way to widen the view and no way to run without one,
	// so the answer for a runtime outside /usr is to ship it in the
	// release or install it under /usr — which is what the message says.
	// The ordinary cases still pass: a binary in the release dir, and
	// one in the base view.
	for _, cmd := range []string{"./server", "/bin/sh"} {
		spec := base
		spec.command, spec.sandbox = []string{cmd}, sb()
		if _, err := r.unitFor(spec, false); err != nil {
			t.Errorf("%s refused: %v", cmd, err)
		}
	}
}

// TestSandboxPropertiesNeverEmitsAnEmptyBindList: BindPaths= and
// BindReadOnlyPaths= are two views of one list in the manager, and
// setting either to an empty array resets the whole list rather than
// adding nothing — taking the base view back out and failing every
// such unit with 203/EXEC. The capability probe is exactly this case:
// it has no directories of its own.
func TestSandboxPropertiesNeverEmitsAnEmptyBindList(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *sandboxSpec
		want []string // properties that must be present
		gone []string // properties that must not be emitted at all
	}{
		{"the capability probe: nothing of its own", probeSandboxSpec(),
			[]string{"BindReadOnlyPaths", "TemporaryFileSystem"}, []string{"BindPaths"}},
		{"an app with no read-only binds of its own", &sandboxSpec{root: "/var/lib/liveswap",
			writable: []bindPath{{dest: "/var/lib/liveswap/blog/shared"}}},
			[]string{"BindPaths", "BindReadOnlyPaths"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: tc.spec})
			for _, name := range tc.want {
				if _, ok := got[name]; !ok {
					t.Errorf("%s missing", name)
				}
			}
			for _, name := range tc.gone {
				if _, ok := got[name]; ok {
					t.Errorf("%s emitted empty: it resets the manager's whole bind list, base view included", name)
				}
			}
			for _, name := range []string{"BindPaths", "BindReadOnlyPaths"} {
				if b, ok := got[name].([]bindMount); ok && len(b) == 0 {
					t.Errorf("%s emitted as an empty list", name)
				}
			}
			// Whatever else is missing, the probe must still get an OS
			// it can exec in.
			ro, _ := got["BindReadOnlyPaths"].([]bindMount)
			var sawUsr bool
			for _, b := range ro {
				if b.Destination == "/usr" {
					sawUsr = true
				}
			}
			if !sawUsr {
				t.Error("the base view is missing /usr: the unit cannot exec anything")
			}
		})
	}
}

// TestSymlinkedRootUnderPrivateTmpLaunches: validateSandboxRoot
// deliberately allows a liveswap root under /tmp, /var/tmp or /home —
// the binds nest into the unit's own private copies, and the
// real-systemd integration lane runs from /var/tmp. The launch-time
// check must agree: a root reached through a symlink resolves into one
// of those trees, and refusing it there would make the same root fail
// every launch merely for being spelled with a link.
func TestSymlinkedRootUnderPrivateTmpLaunches(t *testing.T) {
	for _, under := range []string{"/var/tmp", "/tmp"} {
		t.Run(under, func(t *testing.T) {
			real, err := os.MkdirTemp(under, "liveswap-real-")
			if err != nil {
				t.Skipf("cannot create a root under %s: %v", under, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(real) })
			// The alias lives outside the tree it points at, the way
			// /srv/liveswap -> /var/tmp/ls would.
			base, err := os.MkdirTemp("/var", "liveswap-alias-")
			if err != nil {
				t.Skipf("no writable dir for the alias: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(base) })
			root := filepath.Join(base, "liveswap")
			if err := os.Symlink(real, root); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			release := filepath.Join(root, "blog", "releases", "v1")
			shared := filepath.Join(root, "blog", "shared")
			for _, d := range []string{release, shared} {
				if err := os.MkdirAll(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			s := &sandboxSpec{root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
				writable: []bindPath{{dest: release, source: release}, {dest: shared, source: shared}}}
			if err := s.resolveBindSources(); err != nil {
				t.Fatalf("a symlinked root under %s must launch, exactly as an unaliased one does: %v", under, err)
			}
			// Only the source moves; the app still finds its dirs where
			// its command line and HOME name them.
			for i, want := range []string{release, shared} {
				if s.writable[i].dest != want {
					t.Errorf("bind %d destination moved to %q, want %q", i, s.writable[i].dest, want)
				}
				if !strings.HasPrefix(s.writable[i].source, real) {
					t.Errorf("bind %d source %q is not under the real root %q", i, s.writable[i].source, real)
				}
			}
			// The deny list still applies to a source that is NOT the
			// directory it names — that is the planted-symlink case, and
			// it must not be let through with the root's alias.
			planted := filepath.Join(root, "blog", "shared-link")
			if err := os.Symlink("/dev", planted); err != nil {
				t.Fatal(err)
			}
			bad := &sandboxSpec{root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
				writable: []bindPath{{dest: release, source: release}, {dest: planted, source: planted}}}
			if err := bad.resolveBindSources(); err == nil {
				t.Error("a planted shared -> /dev was accepted under a symlinked root")
			}
		})
	}
}

// TestUnitForResolvesTheCommandBeforeTestingTheView: LookPath does not
// follow symlinks, so a shim under a bound directory pointing at an
// unbound one (the vendored-runtime layout /usr/local/bin/node ->
// /opt/node/bin/node) would pass an unresolved check and then die as
// the shell's `not found`, exit 127, the check exists to replace.
func TestUnitForResolvesTheCommandBeforeTestingTheView(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	root := t.TempDir()
	appDir := filepath.Join(root, "blog")
	release := filepath.Join(appDir, "releases", "v1")
	if err := os.MkdirAll(release, 0o750); err != nil {
		t.Fatal(err)
	}
	// The real runtime, outside the base view; /var is bound nowhere.
	outside, err := os.MkdirTemp("/var", "runtime-")
	if err != nil {
		t.Skipf("no writable dir outside the base view: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	realBin := filepath.Join(outside, "node")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim, inside the release dir (which IS bound), pointing out.
	shim := filepath.Join(release, "node")
	if err := os.Symlink(realBin, shim); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := startSpec{app: "blog", version: "v1", nonce: "0a1b2c3d0a1b2c3d", dir: release, command: []string{shim},
		sandbox: &sandboxSpec{root: root, appDir: appDir, appName: "blog",
			writable: []bindPath{{dest: release, source: release}}}}
	_, err = r.unitFor(spec, false)
	if err == nil {
		t.Fatal("a shim inside the view pointing at a target outside it was accepted")
	}
	if !strings.Contains(err.Error(), realBin) || !strings.Contains(err.Error(), "install it under /usr") {
		t.Fatalf("the refusal must name the resolved target and the fix: %v", err)
	}
}

// TestBaseViewNamesTheTrustStoreNotTheTree: /etc/ssl also holds
// /etc/ssl/private. It is 0700 root:root on every cell of the support
// matrix, so binding /etc/ssl disclosed nothing — but a base view
// should name what apps need rather than depend on the permissions of
// what it sweeps up, and the narrower bind costs nothing.
func TestBaseViewNamesTheTrustStoreNotTheTree(t *testing.T) {
	for _, tree := range []string{"/etc/ssl", "/etc/pki", "/etc"} {
		for _, e := range sandboxBaseView {
			if e == tree {
				t.Errorf("base view binds the whole %s tree; name the entries apps need instead", tree)
			}
		}
	}
	for _, needed := range []string{"/etc/ssl/certs", "/etc/pki/tls/certs"} {
		found := false
		for _, e := range sandboxBaseView {
			if e == needed {
				found = true
			}
		}
		if !found {
			t.Errorf("base view lacks %s: TLS verification would fail inside the sandbox", needed)
		}
	}
	// And the private-key directory must not be reachable through any
	// base-view entry, by prefix or otherwise.
	spec := &sandboxSpec{root: "/var/lib/liveswap", appDir: "/var/lib/liveswap/blog", appName: "blog"}
	if spec.inView("/etc/ssl/private") || spec.inView("/etc/ssl/private/host.key") {
		t.Error("/etc/ssl/private is inside the view")
	}
}

// TestBindSourceInsideTheBaseViewRefused: the base view is the one part
// of the host every unit shares, so an app whose own data resolves into
// it is readable by every sibling — silently, with status still
// reporting the tier as applied. Both the per-app bind and the liveswap
// root itself must refuse it.
func TestBindSourceInsideTheBaseViewRefused(t *testing.T) {
	for _, p := range []string{"/usr/local/app-data", "/usr", "/etc/ssl/certs/x", "/bin/data"} {
		if err := refusedAsBindSource(p); err == nil {
			t.Errorf("a mandatory bind source at %s was accepted: every other app can read it", p)
		} else if !strings.Contains(err.Error(), "EVERY app's sandbox") {
			t.Errorf("%s refused for the wrong reason: %v", p, err)
		}
	}
	// Somewhere genuinely outside the view is still fine — that is the
	// supported "shared moved to another disk" case.
	for _, p := range []string{"/mnt/blog-data", "/srv/blog-data", "/var/cache/blog"} {
		if err := refusedAsBindSource(p); err != nil {
			t.Errorf("%s refused, but it is outside every app's view: %v", p, err)
		}
	}
	for _, bad := range []string{"/usr/local/liveswap", "/usr/share/liveswap", "/etc/ssl/certs/apps"} {
		if err := validateSandboxRoot(bad); err == nil {
			t.Errorf("root %s accepted: every app's data would be readable by every other app", bad)
		}
	}
	// Overlap is symmetric: a root that CONTAINS a base-view entry is
	// the same exposure from the other side, because an app's own
	// directory is derived from the root plus its name. `root /etc/ssl`
	// sits inside nothing, and then an app named `certs` lands its
	// releases and shared dir on /etc/ssl/certs — bound read-only and
	// recursively into every sandbox on the box.
	for _, bad := range []string{"/etc/ssl", "/etc", "/"} {
		if err := validateSandboxRoot(bad); err == nil {
			t.Errorf("root %s accepted: it contains a base-view entry, so an app named for that entry would be readable by every other app", bad)
		}
	}
	for _, ok := range []string{"/var/lib/liveswap", "/srv/apps", "/mnt/apps", "/tmp/liveswap-test"} {
		if err := validateSandboxRoot(ok); err != nil {
			t.Errorf("root %s refused: %v", ok, err)
		}
	}
}

// TestTheBoxExchangeTreeIsSupervisorState: /var/lib/motel-box holds
// the box applier's record, results and baseline, written by root and
// read by the handler (box/DESIGN-box.md, "Paths, owners, and who may
// touch what"). An app holding it could read every result or plant a
// bundle in in/, so it is refused as a bind source and as a root the
// way motel's own state is — by its path components, so a sibling
// that only shares the prefix is not.
func TestTheBoxExchangeTreeIsSupervisorState(t *testing.T) {
	for _, p := range []string{"/var/lib/motel-box", "/var/lib/motel-box/out", "/var/lib/motel-box/in/x"} {
		if err := refusedAsBindSource(p); err == nil {
			t.Errorf("a mandatory bind source at %s was accepted: the app would hold the box's exchange tree", p)
		} else if !strings.Contains(err.Error(), "overlaps /var/lib/motel-box,") {
			t.Errorf("%s refused for the wrong reason: %v", p, err)
		}
	}
	for _, bad := range []string{"/var/lib/motel-box", "/var/lib/motel-box/apps"} {
		if err := validateSandboxRoot(bad); err == nil {
			t.Errorf("root %s accepted: every app would live in the box's exchange tree", bad)
		} else if !strings.Contains(err.Error(), "inside /var/lib/motel-box,") {
			t.Errorf("root %s refused for the wrong reason: %v", bad, err)
		}
	}
	if err := refusedAsBindSource("/var/lib/motel-boxes/blog"); err != nil {
		t.Errorf("a path that only shares the prefix was refused: %v", err)
	}
	if err := validateSandboxRoot("/var/lib/motel-boxes"); err != nil {
		t.Errorf("a root that only shares the prefix was refused: %v", err)
	}
}

// TestValidateSandboxRootFollowsSymlinks: the root check is lexical, so
// `/srv/liveswap -> /var/lib/motel/apps` would otherwise walk past
// the motel-state refusal entirely.
func TestValidateSandboxRootFollowsSymlinks(t *testing.T) {
	base, err := os.MkdirTemp("/var", "rootalias-")
	if err != nil {
		t.Skipf("no writable dir for the alias: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	for _, target := range []string{"/var/lib/motel", "/usr/local"} {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if err := os.MkdirAll(target, 0o750); err != nil {
				t.Skipf("cannot create %s: %v", target, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(target) })
		}
		link := filepath.Join(base, "liveswap")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validateSandboxRoot(link); err == nil {
			t.Errorf("a root symlinked to %s was accepted: the lexical spelling hides it", target)
		}
	}
}

// TestUnitForAcceptsACommandUnderASymlinkedRoot: LookPath is resolved
// before the view test, and a resolved path is canonical — so with a
// symlinked liveswap root the command resolves to the bind's SOURCE
// while the destination keeps the configured spelling. Testing
// destinations alone would refuse every launch on such a host.
func TestUnitForAcceptsACommandUnderASymlinkedRoot(t *testing.T) {
	r, _ := newTestSystemdRunner(t)
	base, err := os.MkdirTemp("/var", "symroot-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	real := filepath.Join(base, "mnt-liveswap")
	release := filepath.Join(real, "blog", "releases", "v1")
	shared := filepath.Join(real, "blog", "shared")
	for _, d := range []string{release, shared} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(release, "server"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "srv-liveswap")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRelease := filepath.Join(root, "blog", "releases", "v1")
	spec := startSpec{app: "blog", version: "v1", nonce: "0a1b2c3d0a1b2c3d", dir: aliasRelease, command: []string{"./server"},
		sandbox: &sandboxSpec{root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
			writable: []bindPath{{dest: aliasRelease, source: aliasRelease},
				{dest: filepath.Join(root, "blog", "shared"), source: filepath.Join(root, "blog", "shared")}}}}
	if _, err := r.unitFor(spec, false); err != nil {
		t.Fatalf("a command under a symlinked root must launch, exactly as under an unaliased one: %v", err)
	}
	// The escape this check exists for still fails: a shim inside the
	// release dir pointing at a target outside every bind.
	outside := filepath.Join(base, "runtime")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "node"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "node"), filepath.Join(release, "node")); err != nil {
		t.Fatal(err)
	}
	spec.command = []string{"./node"}
	if _, err := r.unitFor(spec, false); err == nil {
		t.Error("a shim pointing outside every bind was accepted under a symlinked root")
	}
}

// TestPlantedBindOntoTheSupervisorsRealStateRefused: the refusal list
// is advisory for the VIEW — a path it omits is absent anyway, because
// nothing binds it — but a mandatory bind that resolves onto such a
// path *names* it, and naming is what puts something in the view. So
// here the list must be both canonical and derived from where this
// motel actually keeps its state, not a set of literals.
func TestPlantedBindOntoTheSupervisorsRealStateRefused(t *testing.T) {
	base, err := os.MkdirTemp("/var", "supervisor-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// (a) A protected path reached through its canonical spelling: the
	// literal is /var/lib/motel, the keys are really elsewhere.
	realState := filepath.Join(base, "srv-motel")
	if err := os.MkdirAll(realState, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat("/var/lib/motel"); os.IsNotExist(err) {
		if err := os.MkdirAll("/var/lib", 0o755); err != nil {
			t.Skipf("cannot prepare /var/lib: %v", err)
		}
		if err := os.Symlink(realState, "/var/lib/motel"); err != nil {
			t.Skipf("cannot alias /var/lib/motel: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove("/var/lib/motel") })
		if err := refusedAsBindSource(realState); err == nil {
			t.Errorf("a bind onto %s was accepted, but /var/lib/motel resolves there — the supervisor's keys", realState)
		}
	}

	// (b) Caddy's data dir where XDG actually points it, which no
	// literal in the package mentions.
	xdg := filepath.Join(base, "xdg-data")
	if err := os.MkdirAll(filepath.Join(xdg, "caddy"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", xdg)
	dataDir := caddy.AppDataDir()
	if !strings.HasPrefix(dataDir, xdg) {
		t.Skipf("caddy.AppDataDir() does not follow XDG_DATA_HOME here (%s)", dataDir)
	}
	if err := refusedAsBindSource(dataDir); err == nil {
		t.Errorf("a bind onto %s was accepted: that is where this motel keeps its TLS keys", dataDir)
	}

	// (c) The admin socket directory as systemd actually made it.
	runtimeDir := filepath.Join(base, "run-motel")
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)
	if err := refusedAsBindSource(runtimeDir); err == nil {
		t.Errorf("a bind onto RUNTIME_DIRECTORY %s was accepted: the admin socket lives there", runtimeDir)
	}

	// A neighbour that merely shares a prefix is still fine.
	if err := refusedAsBindSource(filepath.Join(base, "xdg-data-elsewhere")); err != nil {
		t.Errorf("an unrelated sibling path was refused: %v", err)
	}
}

// TestBindOntoASiblingsExternalDataRefused is the finding that removed
// the out-of-root escape hatch. `shop/shared` legitimately resolving to
// /mnt/shop-data used to make /mnt/shop-data an acceptable target for
// ANY app: a bare `blog` could aim its own shared symlink there and
// bind the sibling's data — read-write — into a unit whose status
// reported it sandboxed. Enumerating every other app's external
// locations cannot fix that (it is incomplete by construction, and it
// is the derived, ageing set this model exists to delete); the answer
// is that a mandatory bind must be the directory it names.
func TestBindOntoASiblingsExternalDataRefused(t *testing.T) {
	base, err := os.MkdirTemp("/var", "sibling-")
	if err != nil {
		t.Skipf("no writable dir outside the refused prefixes: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "liveswap")
	shopData := filepath.Join(base, "mnt-shop-data")
	blogRelease := filepath.Join(root, "blog", "releases", "v1")
	for _, d := range []string{blogRelease, shopData} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shopData, "shop.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// blog, running bare, aims its own shared dir at shop's data.
	blogShared := filepath.Join(root, "blog", "shared")
	if err := os.Symlink(shopData, blogShared); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	spec := &sandboxSpec{root: root, appDir: filepath.Join(root, "blog"), appName: "blog",
		writable: []bindPath{{dest: blogRelease, source: blogRelease}, {dest: blogShared, source: blogShared}}}
	err = spec.resolveBindSources()
	if err == nil {
		t.Fatalf("blog was allowed to bind %s — a sibling's data — as its own shared dir", shopData)
	}
	if !strings.Contains(err.Error(), "not the directory it names") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// Nothing was rewritten: the launch fails rather than mounting.
	if spec.writable[1].source != blogShared {
		t.Errorf("bind source was rewritten despite the refusal: %+v", spec.writable)
	}
	// An env_file directory belonging to another app is the same shape
	// and is refused by the same rule — no list of secret locations to
	// keep current.
	secrets := filepath.Join(base, "etc-shop")
	if err := os.MkdirAll(secrets, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(blogShared)
	if err := os.Symlink(secrets, blogShared); err != nil {
		t.Fatal(err)
	}
	if err := spec.resolveBindSources(); err == nil {
		t.Errorf("blog was allowed to bind another app's env_file directory %s", secrets)
	}
}

// TestAppViewsAreDisjointExceptTheBaseView is the app-to-app boundary
// as an executable invariant rather than something a reviewer has to
// notice. Three separate review findings on this PR were the same
// defect wearing different clothes — one app reaching another's data,
// via an out-of-root bind, via the shared base view, via a root inside
// it — and each was caught one at a time by inspection. This asserts
// the property directly: for any two apps, the only paths their views
// share are the base view, which is read-only and identical for
// everyone by design.
//
// It is deliberately written over the whole rendered property set, not
// over sandboxSpec, so it also covers how the binds are emitted.
func TestAppViewsAreDisjointExceptTheBaseView(t *testing.T) {
	root := t.TempDir()
	view := func(name string) map[string]bindMount {
		spec := &appSpec{name: name, dirs: newAppDirs(root, name)}
		sb := spec.sandboxSpecFor(spec.dirs.release("v1"), recordedNonce)
		got := propMap(unitSpec{ExecStart: []string{"/bin/true"}, Sandbox: sb})
		m := map[string]bindMount{}
		for _, prop := range []string{"BindPaths", "BindReadOnlyPaths"} {
			if bs, ok := got[prop].([]bindMount); ok {
				for _, b := range bs {
					m[b.Destination] = b
				}
			}
		}
		return m
	}
	base := map[string]bool{}
	for _, p := range sandboxBaseView {
		base[p] = true
	}

	blog := view("blog")
	shop := view("shop")
	for dest, b := range blog {
		if base[dest] {
			if !b.IgnoreENOENT {
				t.Errorf("base-view entry %s is not optional", dest)
			}
			continue
		}
		// Not a base-view entry: it must be blog's own, and it must not
		// appear in shop's view at all.
		if _, shared := shop[dest]; shared {
			t.Errorf("%s is in both blog's and shop's view", dest)
		}
		if !strings.HasPrefix(dest, filepath.Join(root, "blog")+"/") && dest != filepath.Join(root, "blog") {
			t.Errorf("blog's view contains %s, which is not under its own app directory", dest)
		}
	}
	// Every writable path in a view must be the app's own. This is the
	// half that matters: a read-only leak is a disclosure, a writable
	// one lets an app corrupt a sibling.
	for name, other := range map[string]string{"blog": "shop", "shop": "blog"} {
		got := propMap(unitSpec{ExecStart: []string{"/bin/true"},
			Sandbox: (&appSpec{name: name, dirs: newAppDirs(root, name)}).sandboxSpecFor(
				newAppDirs(root, name).release("v1"), recordedNonce)})
		for _, b := range got["BindPaths"].([]bindMount) {
			if !strings.HasPrefix(b.Destination, filepath.Join(root, name)+"/") {
				t.Errorf("%s has a writable bind at %s, outside its own app directory", name, b.Destination)
			}
			if strings.HasPrefix(b.Destination, filepath.Join(root, other)+"/") {
				t.Errorf("%s has a writable bind into %s's directory: %s", name, other, b.Destination)
			}
			if b.Source != b.Destination && strings.HasPrefix(b.Source, filepath.Join(root, other)) {
				t.Errorf("%s's bind at %s is taken FROM %s's data (%s)", name, b.Destination, other, b.Source)
			}
		}
	}
	// And an app's own state must not be in its own view either — the
	// integrity boundary the design states as normative.
	d := newAppDirs(root, "blog")
	for _, p := range []string{d.app, d.state, d.tmp, d.current, d.releases} {
		if _, present := blog[p]; present {
			t.Errorf("blog's view contains %s", p)
		}
	}
}

// TestSandboxCapabilityCachedPerConnection pins the rule that keeps a
// throwaway unit off Caddy's critical path: a delivered capability is
// measured at most once per manager connection, and again after a
// redial. Drives the caching directly, so it needs no manager to dial.
func TestSandboxCapabilityCachedPerConnection(t *testing.T) {
	c := &userManagerClient{}
	measured := 0
	capable := func() error {
		measured++
		return nil
	}

	c.generation.Add(1) // a first dial
	for i := 0; i < 5; i++ {
		if err := c.cachedSandboxCapability(capable); err != nil {
			t.Fatalf("call %d: %v, want capable", i, err)
		}
	}
	if measured != 1 {
		t.Fatalf("measured %d times across 5 config loads, want 1: the probe starts a unit and belongs to the connection", measured)
	}

	// A redial invalidates it: the manager that restarted may not be
	// the one that was measured.
	c.generation.Add(1)
	if err := c.cachedSandboxCapability(capable); err != nil {
		t.Fatalf("after redial: %v", err)
	}
	if measured != 2 {
		t.Fatalf("measured %d times, want 2: a redial must re-measure", measured)
	}

	// A redial *during* a measurement means it described a manager that
	// is no longer current: report it, cache nothing, measure again.
	c.generation.Add(1)
	gen := c.generation.Load()
	before := measured
	racing := func() error {
		measured++
		c.generation.Add(1)
		return nil
	}
	if err := c.cachedSandboxCapability(racing); err != nil || measured != before+1 {
		t.Fatalf("the caller that asked must still get the measurement it took: %v (measured %d)", err, measured-before)
	}
	if c.sandboxGen == gen {
		t.Fatal("a measurement taken across a redial was cached against the stale generation")
	}

	// Generation 0 is "never dialed": nothing to cache against, in
	// either direction. The write guard is asserted as well as the
	// read one, so the two halves cannot disagree about the sentinel.
	fresh := &userManagerClient{}
	measured = 0
	_ = fresh.cachedSandboxCapability(capable)
	_ = fresh.cachedSandboxCapability(capable)
	if measured != 2 {
		t.Fatalf("measured %d times at generation 0, want 2: no connection means nothing to cache against", measured)
	}
	if fresh.sandboxGen != 0 {
		t.Fatalf("sandboxGen = %d after measuring with no connection; 0 is reserved for \"never measured\"", fresh.sandboxGen)
	}
}

// TestSandboxFailedMeasurementIsNotCached pins the half of the caching
// rule that decides whether a bad minute keeps a host down for the
// life of a connection. probeSandboxCapability reports a failure the
// same way whether the namespaces are genuinely unavailable or the
// probe unit merely timed out under boot load, and a timeout leaves
// the connection Connected() — so nothing would ever drop it and
// re-measure.
func TestSandboxFailedMeasurementIsNotCached(t *testing.T) {
	c := &userManagerClient{}
	c.generation.Add(1)
	measured := 0
	failing := func() error {
		measured++
		return errors.New("sandbox probe unit: unit failed: probe timed out")
	}
	for i := 0; i < 3; i++ {
		if err := c.cachedSandboxCapability(failing); err == nil {
			t.Fatalf("call %d: capable, want the failure reported", i)
		}
	}
	if measured != 3 {
		t.Fatalf("measured %d times, want 3: a failed verdict must not keep motel down until a redial", measured)
	}
	if c.sandboxGen != 0 {
		t.Fatal("a failed verdict was cached")
	}
	// And once the host answers, that verdict is cached as usual.
	if err := c.cachedSandboxCapability(func() error { return nil }); err != nil {
		t.Fatalf("capable verdict reported as %v", err)
	}
	if err := c.cachedSandboxCapability(failing); err != nil || measured != 3 {
		t.Fatalf("a cached success must still be served: %v (measured %d)", err, measured)
	}
}

// The gate's question of a unix admin listener: no app's view may hold
// it. Under the root (an app's shared dir is bound writable), under the
// base view (read-only, and connecting is no write), through a link at
// the socket's directory or at the root, or spelled uncleanly; beside
// the root, or in a directory no sandbox names, it is nobody's.
func TestAdminSocketInView(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "apps")
	shared := filepath.Join(root, "blog", "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"sockdir": shared, "apps-link": root} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct{ root, path, want string }{
		"the default root":               {"", "/var/lib/liveswap/blog/shared/admin.sock", "is under the liveswap root /var/lib/liveswap,"},
		"a configured root":              {root, filepath.Join(shared, "admin.sock"), "is under the liveswap root " + root + ","},
		"the root itself":                {root, filepath.Join(root, "admin.sock"), "is under the liveswap root " + root + ","},
		"the base view":                  {root, "/usr/local/run/admin.sock", "is under /usr, which is bound read-only"},
		"a base-view directory":          {root, "/etc/ssl/certs/admin.sock", "is under /etc/ssl/certs,"},
		"a link at the directory":        {root, filepath.Join(dir, "sockdir", "admin.sock"), "is under the liveswap root " + root + ","},
		"a link at the root":             {filepath.Join(dir, "apps-link"), filepath.Join(shared, "admin.sock"), "is under the liveswap root " + filepath.Join(dir, "apps-link") + ","},
		"an unclean spelling":            {root, root + "/blog/../blog/shared/admin.sock", "is under the liveswap root " + root + ","},
		"beside the root":                {root, filepath.Join(dir, "admin.sock"), ""},
		"the starter's":                  {"", "/run/motel/admin.sock", ""},
		"a sibling of a base-view entry": {"", "/usr-local/admin.sock", ""},
	} {
		got := AdminSocketInView(tc.root, tc.path)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: AdminSocketInView(%q, %q) = %q, want %q", name, tc.root, tc.path, got, tc.want)
		}
	}
}
