package liveswap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// userManagerClient talks to the systemd service manager of the user
// moteld runs as, over that manager's private socket
// ($XDG_RUNTIME_DIR/systemd/private; /run/user/<uid> by default) — the
// same path `systemctl --user` takes when no session bus is around. No
// session bus, no polkit: the socket is owned by our uid and answers
// only to it, and it exists whenever user@<uid>.service is running
// (lingering keeps it running with nobody logged in).
//
// The connection is dialed lazily and shared process-wide: pooled
// managedApps outlive any one config, so it belongs to the process,
// not to an App. A transport failure drops it and the next call
// redials; callers see the error of the call that hit it.
type userManagerClient struct {
	mu   sync.Mutex
	conn *sddbus.Conn
	// generation counts successful dials, so a measurement can name the
	// connection it was taken on. Starts at 1: 0 means "never dialed",
	// which no cached measurement can match.
	generation atomic.Uint64
	// nofile is the manager's DefaultLimitNOFILE (its hard ceiling for
	// units), read once per connection; 0 = unknown, leave units alone.
	nofile atomic.Uint64

	// The sandbox capability is a property of the manager, not of a
	// config load, and measuring it costs a whole throwaway unit — far
	// too much to repeat on Caddy's critical path, which is what
	// App.Start used to do on every reload. It is measured at most once
	// per connection: a manager restart forces a redial, which is the
	// event the generation exists to catch.
	//
	// What the probe measures is the kernel and the LSM rather than
	// the manager, so a sysctl or an LSM policy reload can move the
	// answer with the connection still up. That is deliberately not
	// chased: the verdict is a start-time input (App.Start either
	// comes up or refuses on it), so a measurement taken at the start
	// that resolved it is the honest one to hold. A host whose
	// namespace policy changes underneath a running moteld is
	// re-measured on the next dial — a manager restart, or moteld's
	// own.
	//
	// Its own mutex, not mu: a measurement takes a unit start plus its
	// exit, and holding the mutex every D-Bus call needs for that long
	// would stall the runner.
	sandboxMu  sync.Mutex
	sandboxGen uint64 // generation the host was measured capable for; 0 = never
	// defaultTasksMax is the manager's DefaultTasksMax= as read with the
	// connection (get), like nofile; infinity kept, 0 = unreadable.
	defaultTasksMax atomic.Uint64
}

// userManager is the process-wide client every systemdRunner shares.
var userManager = &userManagerClient{}

// cachedSandboxCapability returns the measurement held for the current
// connection, taking a fresh one via measure when the cache is empty
// or belongs to an older one. The measurement itself is the caller's
// (App.measureSandbox): this owns only the caching rule, which keeps
// it testable without a manager to dial.
//
// Precondition every caller must hold: probe() the connection first.
// A manager that is simply gone would otherwise surface through the
// measurement as "this host cannot deliver the sandbox", losing the
// error that names the uid, the socket and the lingering to enable —
// and there is no remedy in config for the reason it would report.
//
// Only a capable verdict is cached. A failed one is not a measurement
// of the host so much as the absence of one: probeSandboxCapability
// reports the same thing whether the namespaces are genuinely
// unavailable or the probe unit merely timed out under boot load, and
// caching that would refuse the server for the life of a connection
// that nothing will drop — a moteld that will not come up until the
// manager restarts. So a failed verdict is reported and re-measured
// next time, which is what this code did before the cache existed.
// The cost of re-measuring falls only on hosts that cannot sandbox at
// all; the supported one pays it once.
func (c *userManagerClient) cachedSandboxCapability(measure func() error) error {
	c.sandboxMu.Lock()
	defer c.sandboxMu.Unlock()
	// Read under the lock: sampled outside it, a redial between the
	// read and the lock would let a cache hit serve a measurement of a
	// manager that no longer exists.
	gen := c.generation.Load()
	if gen != 0 && c.sandboxGen == gen {
		return nil
	}
	err := measure()
	// Cache only against a real connection, and only if that
	// connection is still the current one: a redial while the probe
	// ran means it described a manager that is no longer live. The
	// caller that asked still gets what was measured.
	if err == nil && gen != 0 && c.generation.Load() == gen {
		c.sandboxGen = gen
	}
	return err
}

// userManagerSocket is the private socket of this uid's manager.
func userManagerSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	return filepath.Join(dir, "systemd", "private")
}

// probeTimeout bounds the Provision-time round trip to the manager.
const probeTimeout = 10 * time.Second

// probe proves the manager answers a real request within a deadline.
func (c *userManagerClient) probe() error {
	conn, err := c.get()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		_, err = conn.ListUnitsByPatternsContext(ctx, nil, []string{unitPrefix + "*.service"})
		cancel()
		c.dropIfDisconnected(conn, err)
	}
	if err != nil {
		return fmt.Errorf("liveswap needs the systemd user manager for uid %d (socket %s): %w — apps run as transient units under it; on a packaged install make sure `loginctl enable-linger motel` is in effect and user@%d.service is active",
			os.Getuid(), userManagerSocket(), err, os.Getuid())
	}
	return nil
}

func (c *userManagerClient) get() (*sddbus.Conn, error) {
	c.mu.Lock()
	if c.conn != nil {
		if c.conn.Connected() {
			conn := c.conn
			c.mu.Unlock()
			return conn, nil
		}
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	// Dial outside the lock: a slow or wedged manager must not make
	// every caller queue behind one redial past their own deadlines.
	// The socket deadlines set in dialPrivate stay in force until
	// NewConnection has finished (it dials twice and then installs its
	// signal match), so the whole establishment is bounded.
	sock := userManagerSocket()
	var raws []net.Conn
	conn, err := sddbus.NewConnection(func() (*godbus.Conn, error) {
		dc, nc, err := dialPrivate(sock)
		if err == nil {
			raws = append(raws, nc)
		}
		return dc, err
	})
	if err != nil {
		return nil, err
	}
	// systemd gives services soft NOFILE 1024 by default however high
	// the hard limit is; the exec runner's children inherited
	// moteld.service's soft=hard. Units are created with both set to
	// the manager's hard ceiling (which the package raises via a
	// user@<uid>.service.d drop-in), so a setrlimit can never exceed
	// what the manager permits. Read while the handshake deadlines are
	// still armed, so a manager that stops answering here cannot hang
	// every caller; unknown just means "leave the limits alone".
	var nofile uint64 // 0 = unknown: leave units' limits alone
	if v, err := conn.GetManagerProperty("DefaultLimitNOFILE"); err == nil {
		nofile = parseManagerUint(v)
	} else if !conn.Connected() {
		conn.Close()
		return nil, err
	}
	// The manager's DefaultTasksMax, read the same way for the same
	// reason: it is what an unset tasks_max is lifted to, and what a
	// unit with none reports (SetCaps, unitCaps). Published with the
	// connection, so a manager restarted with another value is read
	// again on the redial. Infinity is kept: a manager configured so
	// has that as its default, and a lift goes there. 0 = unreadable:
	// nothing to lift to.
	var tasksMax uint64
	if v, err := conn.GetManagerProperty("DefaultTasksMax"); err == nil {
		tasksMax = parseManagerLimit(v)
	} else if !conn.Connected() {
		conn.Close()
		return nil, err
	}
	for _, nc := range raws {
		if derr := nc.SetDeadline(time.Time{}); derr != nil {
			conn.Close()
			return nil, derr
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.Connected() {
		conn.Close() // another caller won the redial; its limit stands
		return c.conn, nil
	}
	c.conn = conn
	c.nofile.Store(nofile) // published with the connection it was read from
	c.defaultTasksMax.Store(tasksMax)
	c.generation.Add(1) // ... as is the generation the sandbox cache keys on
	return conn, nil
}

// parseManagerUint reads go-systemd's string rendering of a uint64
// manager property ("@t 524288"); 0 for anything unparseable or
// unlimited, which callers treat as "don't set".
func parseManagerUint(s string) uint64 {
	if n := parseManagerLimit(s); n != infinityUSec {
		return n
	}
	return 0
}

// parseManagerLimit reads the same rendering and keeps infinity: a
// DefaultTasksMax= of infinity is a default of the manager's like any
// number, the one a lifted tasks cap then goes to. 0 = unparsable.
func parseManagerLimit(s string) uint64 {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@t"))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// dropIfDisconnected forgets a connection that a failed call has
// found dead, so the next call redials instead of failing forever.
func (c *userManagerClient) dropIfDisconnected(conn *sddbus.Conn, err error) {
	if err == nil || conn.Connected() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn.Close()
		c.conn = nil
	}
}

// dialTimeout bounds connecting to and authenticating with the private
// socket, so a manager that accepts the connection but never finishes
// the handshake cannot hang a call that advertises a deadline.
const dialTimeout = 10 * time.Second

// dialPrivate mirrors go-systemd's own private-socket dial for
// /run/systemd/private: EXTERNAL auth with our uid and no Hello (the
// private socket is not a bus daemon). The returned net.Conn carries a
// deadline the caller clears once the connection is fully set up; the
// established connection then lives for the process (godbus would
// close it with a call-scoped context, hence Background).
func dialPrivate(socket string) (*godbus.Conn, net.Conn, error) {
	nc, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return nil, nil, err
	}
	if err := nc.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	conn, err := godbus.NewConn(nc, godbus.WithContext(context.Background()))
	if err != nil {
		_ = nc.Close()
		return nil, nil, err
	}
	if err := conn.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, nc, nil
}

// unitProperties renders a unitSpec as transient-unit properties. The
// hardening set is fixed here for every unit motel creates:
// Restart=no (the watchdog restarts), KillMode=control-group (stop
// means the whole tree), NoNewPrivileges (no setuid escalation),
// LimitCORE=0 on both limits (no core dump), and both output streams
// to the journal under the app's identifier.
//
// The command goes through ExecStartEx with no-env-expand, never the
// plain ExecStart: that one carries no flags, and through it systemd
// substitutes the unit's environment into every argv word at exec
// time ("$VAR" word-splits, "$$" becomes "$", a word naming an unset
// variable leaves argv). With the flag the argv reaches the sibling
// shell byte for byte, and its `exec "$@"` hands it on as it is — the
// unit-file spelling is a ":" before the path. The manager still
// reports it as ExecStart, which propExecStart reads back with the
// shell stripped.
func unitProperties(u unitSpec) []sddbus.Property {
	typ := "simple"
	if u.Oneshot {
		typ = "oneshot"
	}
	env := u.Environment
	if env == nil {
		env = []string{}
	}
	// TimeoutStopUSec=0 means "never SIGKILL"; a sub-microsecond grace
	// must not truncate into that.
	stopUSec := uint64(u.StopTimeout / time.Microsecond) //nolint:gosec // durations here are small positive config values
	if stopUSec == 0 {
		stopUSec = 1
	}
	props := []sddbus.Property{
		sddbus.PropDescription(u.Description),
		sddbus.PropType(typ),
		{Name: "ExecStartEx", Value: godbus.MakeVariant([]execCommandEx{
			{Path: siblingShell, Args: withSibling(u.ExecStart), Flags: []string{"no-env-expand"}},
		})},
		{Name: "WorkingDirectory", Value: godbus.MakeVariant(u.WorkingDirectory)},
		{Name: "Environment", Value: godbus.MakeVariant(env)},
		{Name: "Restart", Value: godbus.MakeVariant("no")},
		{Name: "KillMode", Value: godbus.MakeVariant("control-group")},
		{Name: "NoNewPrivileges", Value: godbus.MakeVariant(true)},
		// The manager's default is soft 0 under an unlimited hard
		// limit, which an app may raise with one setrlimit; a hard 0
		// is the one it cannot. A dump would hold the app's memory,
		// env values included, past the unit's life
		// (DESIGN-threat-model.md, "Where env_file values sit").
		{Name: "LimitCORE", Value: godbus.MakeVariant(uint64(0))},
		{Name: "LimitCORESoft", Value: godbus.MakeVariant(uint64(0))},
		{Name: "TimeoutStopUSec", Value: godbus.MakeVariant(stopUSec)},
		{Name: "SyslogIdentifier", Value: godbus.MakeVariant(u.SyslogIdentifier)},
		{Name: "StandardOutput", Value: godbus.MakeVariant("journal")},
		{Name: "StandardError", Value: godbus.MakeVariant("journal")},
	}
	if u.StartTimeout > 0 {
		// 0 is no bound at all for a oneshot; a sub-microsecond
		// timeout must not truncate into it.
		startUSec := max(uint64(u.StartTimeout/time.Microsecond), 1)
		props = append(props, sddbus.Property{Name: "TimeoutStartUSec", Value: godbus.MakeVariant(startUSec)})
	}
	// No ExecStopPost=, nor any Exec*= beside ExecStartEx: the manager
	// spawns one process per unit (the app's own children join its
	// namespaces as ever). On a user manager every process the manager
	// spawns gets a user namespace of its own, so a second one cannot
	// join the IPC namespace the first made (PrivateIPC=,
	// sandboxProperties) and fails 226/NAMESPACE on its /dev/mqueue
	// mount (measured, #311). The instance's socket goes with its run
	// dir when motel retires the instance (socketRef.retire), or in
	// a confirmed sweep's prune; nothing routes to a leftover
	// (socket.go).
	props = append(props, capProperties(u.Caps, false, 0)...)
	return append(props, sandboxProperties(u.Sandbox)...)
}

// usecPerPercent is one percent of a second: CPUQuota=50% is
// CPUQuotaPerSecUSec=500ms on the bus.
const usecPerPercent = 10_000

// infinityUSec is what the manager reads, and reports, as "infinity"
// for a uint64 cgroup property: no cap.
const infinityUSec = ^uint64(0)

// capProperties renders resourceCaps as the unit's cgroup properties.
// At a start (live=false) only what is set is emitted, so an app with
// no caps gets exactly the manager's defaults. Live (SetCaps) every
// cap is, so a cap a reload removed is lifted from the running unit
// rather than kept: memory and CPU to infinity, their defaults, and an
// unset tasks cap to liftTasksTo, the manager's DefaultTasksMax —
// never infinity, which would lift the manager's own bound too; with
// liftTasksTo 0 (the default unreadable) TasksMax alone is left as it
// is, and the memory and CPU resets still go. memory_max comes with
// MemorySwapMax=0: a cap an app can spill past into swap bounds
// nothing, and lifting the cap lifts that too.
func capProperties(c resourceCaps, live bool, liftTasksTo uint64) []sddbus.Property {
	var props []sddbus.Property
	if c.memoryMax > 0 || live {
		mem, swap := infinityUSec, infinityUSec
		if c.memoryMax > 0 {
			mem, swap = c.memoryMax, 0
		}
		props = append(props,
			sddbus.Property{Name: "MemoryMax", Value: godbus.MakeVariant(mem)},
			sddbus.Property{Name: "MemorySwapMax", Value: godbus.MakeVariant(swap)},
		)
	}
	if c.tasksMax > 0 || (live && liftTasksTo > 0) {
		tasks := liftTasksTo
		if c.tasksMax > 0 {
			tasks = uint64(c.tasksMax)
		}
		props = append(props, sddbus.Property{Name: "TasksMax", Value: godbus.MakeVariant(tasks)})
	}
	if c.cpuQuota > 0 || live {
		quota := infinityUSec
		if c.cpuQuota > 0 {
			quota = uint64(c.cpuQuota) * usecPerPercent
		}
		props = append(props, sddbus.Property{Name: "CPUQuotaPerSecUSec", Value: godbus.MakeVariant(quota)})
	}
	return props
}

// execCommandEx is the D-Bus shape of one Exec*Ex= entry, "(sasas)":
// the binary, its argv, and flag names in place of the bool
// ("ignore-failure", "no-env-expand", ...; systemd ≥ 245, and the
// package's floor is 257).
type execCommandEx struct {
	Path  string
	Args  []string
	Flags []string
}

// Every unit's command runs behind a shell that forks one sibling
// process into the cgroup and then execs the app in its own place —
// the app is still PID 1 of its namespace, with its own pid, argv and
// exit status — because on systemd 257 the manager does not always
// wait the grace for a one-process app (#380). Under PrivatePIDs= the
// manager forks an intermediate that sends it the app's pid and
// exits; a manager that reads the app's parent before that exit
// records the pid as not its child ("Supervising process … which is
// not our child" in the journal), counts it as nothing to wait for,
// and with nothing else in the cgroup follows its SIGTERM with SIGKILL
// at once, on every stop path: a deploy's, an operator's `systemctl
// --user stop`, the manager's own shutdown at a reboot. A second
// process in the cgroup is something to wait for, so the manager
// holds TimeoutStopSec= as configured (unit_kill_context, v257);
// measured, every instance with a sibling kept its grace, 22 of 36
// without lost it. systemd 258 fixes the fork (commit 698ac172aa);
// Debian 13 ships 257.
//
// What the app sees of it: one child it did not spawn (the shell's
// fork survives the exec), which sleeps for the app's whole life and
// dies at the stop's SIGTERM — the manager signals the whole cgroup —
// so the app gets one SIGCHLD then, as a prefork master does from a
// worker. Under PrivatePIDs= any extra process is the app's child or
// is reparented to it, so no sibling avoids that. It costs one task,
// which is why tasks_max's floor is 2, and about a megabyte. `unset
// PWD` first: dash exports PWD, and invariant 8 of the runner says a
// unit's environment is exactly the one the caller built. Both tools
// are Essential on Debian and under /usr, which every view binds;
// Preflight still checks them (siblingToolsPresent), and the script
// runs `sleep 0` once before the fork and exits 127 if it cannot —
// a shell whose `sleep … &` failed would otherwise exec the app
// alone, the race open and the unit green, with one line in the
// journal. (A sleep that starts and then exits on `infinity` is not
// caught: a sleep in /usr/bin that is not coreutils' is not Debian.)
// no-env-expand still holds: the argv reaches the shell byte for
// byte and "$@" hands it on. The script is a wire format: appArgv
// recognises units of this moteld by it, so a change here makes
// every running unit read back as the shell's argv until relaunched.
const (
	siblingShell  = "/bin/sh"
	siblingSleep  = "/usr/bin/sleep"
	siblingScript = `unset PWD; ` + siblingSleep + ` 0 || exit 127; ` + siblingSleep + ` infinity & exec "$@"`
	siblingArgv0  = "motel-app"
)

// withSibling is the argv the manager execs for an app's argv.
func withSibling(argv []string) []string {
	return append([]string{siblingShell, "-c", siblingScript, siblingArgv0}, argv...)
}

// appArgv is the app's argv from the one the manager reports, the
// sibling shell stripped: what status, state.json and a reattach see.
// An argv not in that shape (a unit of an older moteld) is returned
// as it is.
func appArgv(argv []string) []string {
	if len(argv) >= 4 && argv[0] == siblingShell && argv[1] == "-c" && argv[2] == siblingScript && argv[3] == siblingArgv0 {
		return argv[4:]
	}
	return argv
}

// D-Bus shapes of the sandbox properties, as systemd's own
// bus-unit-util renders the unit-file syntax: list-valued path
// options are "(ssbt)" bind tuples or "a(ss)" (path, options) pairs,
// the allow-list options are "(bas)", and the flag/capability sets
// are uint64 masks.
type (
	bindMount struct {
		Source, Destination string
		IgnoreENOENT        bool
		Flags               uint64 // mount flags: mountRecursive for rbind
	}
	tmpfsMount struct{ Path, Options string }
	allowList  struct {
		AllowList bool
		Names     []string
	}
)

const (
	mountRecursive uint64 = 0x4000 // MS_REC: BindPaths=' default "rbind"
	errnoEPERM     int32  = 1      // SystemCallErrorNumber=EPERM, every Linux arch
)

// sandboxProperties renders a sandboxSpec. The set is the one measured
// in issue #35; see sandbox.go for what it closes. Every unit gets
// every property: there is one sandbox and no lesser one (unitFor
// refuses a spec without one, so s is never nil here).
//
// The view is deny-by-default: TemporaryFileSystem=/ replaces the
// whole filesystem with an empty read-only tmpfs, and the binds below
// are the only things that exist inside the unit. There is no
// InaccessiblePaths= and no ProtectSystem= because there is nothing
// left for either to act on — an unnamed path is absent, not merely
// unreadable, which is a stronger statement than either option makes
// and one that cannot go stale. MountAPIVFS= is not set: /proc, /sys
// and /dev are mounted inside the tmpfs by PrivateDevices= and
// PrivatePIDs=, measured on 252/255/257/259.
func sandboxProperties(s *sandboxSpec) []sddbus.Property {
	writable := make([]bindMount, 0, len(s.writable))
	for _, b := range s.writable {
		src := b.source
		if src == "" {
			src = b.dest
		}
		writable = append(writable, bindMount{Source: src, Destination: b.dest, Flags: mountRecursive})
	}
	// The base view first: every entry optional. Debian 13 is merged-/usr
	// and has no /etc/pki, so several names are aliases or simply absent
	// — and /run/systemd/resolve exists only where resolved runs. A
	// missing entry must not fail the unit (see sandboxBaseView).
	readOnly := make([]bindMount, 0, len(sandboxBaseView))
	for _, p := range sandboxBaseView {
		readOnly = append(readOnly, bindMount{Source: p, Destination: p, IgnoreENOENT: true, Flags: mountRecursive})
	}
	props := []sddbus.Property{
		// Explicit on purpose: the mount options below do not imply it
		// (without it the unit fails to exec), and it is the piece that
		// closes /proc reads of processes outside the namespace.
		{Name: "PrivateUsers", Value: godbus.MakeVariant(true)},
		{Name: "PrivateTmp", Value: godbus.MakeVariant(true)},
		{Name: "PrivateDevices", Value: godbus.MakeVariant(true)},
		// Its own IPC namespace: SysV objects and POSIX message queues
		// are the unit's alone, and /dev/mqueue is a fresh mqueuefs of
		// that namespace. On a user manager only the first process the
		// manager spawns can set it up (unitProperties), which is why a
		// unit has one Exec*=. POSIX shared memory is not covered by
		// it: that lives in /dev/shm, which PrivateDevices= binds from
		// the host, and the TemporaryFileSystem= entry below replaces
		// (#311).
		{Name: "PrivateIPC", Value: godbus.MakeVariant(true)},
		// Read-only cgroupfs: the delegated subtree is owned by the
		// app's own uid, so resource caps are otherwise rewritable.
		{Name: "ProtectControlGroups", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelTunables", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelModules", Value: godbus.MakeVariant(true)},
		{Name: "ProtectKernelLogs", Value: godbus.MakeVariant(true)},
		// RestrictNamespaces=yes: the allowed set is empty.
		{Name: "RestrictNamespaces", Value: godbus.MakeVariant(uint64(0))},
		{Name: "RestrictRealtime", Value: godbus.MakeVariant(true)},
		{Name: "RestrictSUIDSGID", Value: godbus.MakeVariant(true)},
		{Name: "LockPersonality", Value: godbus.MakeVariant(true)},
		// AF_NETLINK stays: getifaddrs() — Node's os.networkInterfaces(),
		// Go's net.Interfaces(), which frameworks call at startup — needs
		// a netlink socket. A route, link or address change needs
		// CAP_NET_ADMIN, which the empty capability set denies; what a
		// generic-netlink family admits unprivileged is its own rule,
		// and the modules a netlink socket can load are an accepted
		// residual (DESIGN-sandbox.md, "Module-backed protocols").
		{Name: "RestrictAddressFamilies", Value: godbus.MakeVariant(allowList{true, []string{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"}})},
		{Name: "SystemCallFilter", Value: godbus.MakeVariant(allowList{true, []string{"@system-service"}})},
		// Less io_uring_setup, which @system-service admits through @aio.
		// A ring's IORING_OP_SOCKET (kernel 5.19+) opens a socket of any
		// family without calling socket(2), and socket(2) is the only
		// call RestrictAddressFamilies= filters. With no ring, the other
		// two io_uring calls have nothing to act on. A deny entry after
		// the allow list takes names out of it, as
		// SystemCallFilter=~io_uring_setup does on a second unit-file line.
		{Name: "SystemCallFilter", Value: godbus.MakeVariant(allowList{false, []string{"io_uring_setup"}})},
		{Name: "SystemCallErrorNumber", Value: godbus.MakeVariant(errnoEPERM)},
		// The native syscall ABI only. On amd64 the x86-32 ABI reaches
		// socket(2) through socketcall(), and systemd does not apply
		// RestrictAddressFamilies= to that ABI at all.
		{Name: "SystemCallArchitectures", Value: godbus.MakeVariant([]string{"native"})},
		{Name: "CapabilityBoundingSet", Value: godbus.MakeVariant(uint64(0))},
		// Start from nothing. The two bind properties appended below
		// put back the OS the app needs to run and the app's own
		// directories, and between them they are the whole view.
		// Then a /dev/shm of the unit's own over the host's, which
		// PrivateDevices= binds into its private /dev: POSIX shared
		// memory and named semaphores live there, every app has the
		// one uid, and the IPC namespace does not reach it. size=50%
		// is the kernel's own default for the host's /dev/shm, so each
		// app keeps the room the shared one gave it; left unsaid,
		// systemd makes every TemporaryFileSystem= 10% of RAM
		// (NESTED_TMPFS_LIMITS, namespace.c), which on a 2 GiB box is
		// 204 MiB — headless Chromium's known trap at Docker's 64 MiB,
		// a size nearer. Its pages are charged to the unit's memory
		// cgroup like any other, so memory_max (#345) is the practical
		// cap. mode=1777 as the host's. Owner's call (#311).
		{Name: "TemporaryFileSystem", Value: godbus.MakeVariant([]tmpfsMount{{"/", "ro"}, {"/dev/shm", "mode=1777,size=50%"}})},
		// The manager hands every unit its own XDG_RUNTIME_DIR and bus
		// address; both point into a /run/user that is not in the view.
		{Name: "UnsetEnvironment", Value: godbus.MakeVariant([]string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"})},
	}
	// BindPaths= and BindReadOnlyPaths= are two views of ONE list in the
	// manager, and setting either to an empty array does not mean "add
	// nothing" — it resets the whole list, read-only entries included.
	// Emitting an empty BindPaths= after the base view would therefore
	// take /usr and /bin away again and every unit would fail
	// 203/EXEC — measured, and exactly what the capability probe (which
	// has no directories of its own) used to do. Omit what is empty.
	props = appendIfAny(props, "BindReadOnlyPaths", readOnly)
	props = appendIfAny(props, "BindPaths", writable)
	props = append(props, sddbus.Property{Name: "PrivatePIDs", Value: godbus.MakeVariant("yes")})
	return props
}

// appendIfAny adds a list-valued property only when it has entries;
// see the reset semantics noted in sandboxProperties.
func appendIfAny(props []sddbus.Property, name string, binds []bindMount) []sddbus.Property {
	if len(binds) == 0 {
		return props
	}
	return append(props, sddbus.Property{Name: name, Value: godbus.MakeVariant(binds)})
}

func (c *userManagerClient) StartTransientUnit(ctx context.Context, u unitSpec) (string, error) {
	conn, err := c.get()
	if err != nil {
		return "", err
	}
	props := unitProperties(u)
	if n := c.nofile.Load(); n > 0 {
		props = append(props,
			sddbus.Property{Name: "LimitNOFILE", Value: godbus.MakeVariant(n)},
			sddbus.Property{Name: "LimitNOFILESoft", Value: godbus.MakeVariant(n)},
		)
	}
	// go-systemd delivers the result with its listener lock held, so
	// the channel must never block: buffer it.
	ch := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, u.Name, "fail", props, ch); err != nil {
		c.dropIfDisconnected(conn, err)
		return "", err
	}
	return jobResult(ctx, ch)
}

// jobResult waits for a job's result, or for ctx to end. A result
// already in when ctx ends wins: a select picks among ready cases at
// random, and a job that finished at that moment — a pre_start done as
// its deadline passed — would otherwise read as cancelled, and a
// migration that ran would have its release deleted.
func jobResult(ctx context.Context, ch <-chan string) (string, error) {
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		select {
		case res := <-ch:
			return res, nil
		default:
			return "", ctx.Err()
		}
	}
}

func (c *userManagerClient) StopUnit(ctx context.Context, name string) (string, error) {
	conn, err := c.get()
	if err != nil {
		return "", err
	}
	ch := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, name, "replace", ch); err != nil {
		if isNoSuchUnit(err) {
			return "done", nil // already unloaded: stopped by definition
		}
		c.dropIfDisconnected(conn, err)
		return "", err
	}
	return jobResult(ctx, ch)
}

// SetUnitProperties sets runtime (not persisted — transient units
// persist nothing anyway) properties on a loaded unit.
func (c *userManagerClient) SetUnitProperties(ctx context.Context, name string, props []sddbus.Property) error {
	conn, err := c.get()
	if err != nil {
		return err
	}
	err = conn.SetUnitPropertiesContext(ctx, name, true, props...)
	c.dropIfDisconnected(conn, err)
	return err
}

// DefaultTasksMax is the manager's DefaultTasksMax= as read with the
// connection (get), resolved: the manager resolves its own percentage
// to a number, and a unit with no TasksMax= reports exactly this; a
// manager configured with DefaultTasksMax=infinity reports infinity,
// which is kept (it is that manager's default, where a lift goes). The
// read happened under the dial's deadlines, so nothing here can wait
// on a manager that has stopped answering; a dropped connection reads
// it again on the redial, which is also when a manager restarted with
// another value is seen.
func (c *userManagerClient) DefaultTasksMax(_ context.Context) (uint64, error) {
	if _, err := c.get(); err != nil {
		return 0, err
	}
	v := c.defaultTasksMax.Load()
	if v == 0 {
		// Unreadable at the dial: no number to lift a tasks cap to.
		return 0, errors.New("the manager's DefaultTasksMax is unreadable")
	}
	return v, nil
}

func (c *userManagerClient) ResetFailedUnit(ctx context.Context, name string) error {
	conn, err := c.get()
	if err != nil {
		return err
	}
	err = conn.ResetFailedUnitContext(ctx, name)
	if isNoSuchUnit(err) {
		return nil // already unloaded: nothing left to reset
	}
	c.dropIfDisconnected(conn, err)
	return err
}

// UnitStatus reads every property of the unit object in one round
// trip (GetAll with an empty interface spans Unit and Service). A unit
// the manager does not know comes back as not-found, never an error.
func (c *userManagerClient) UnitStatus(ctx context.Context, name string) (unitStatus, error) {
	conn, err := c.get()
	if err != nil {
		return unitStatus{}, err
	}
	props, err := conn.GetAllPropertiesContext(ctx, name)
	if err != nil {
		if isNoSuchUnit(err) {
			return unitStatus{Name: name, LoadState: "not-found"}, nil
		}
		c.dropIfDisconnected(conn, err)
		return unitStatus{}, err
	}
	st := statusFromProps(props)
	st.Name = name
	return st, nil
}

// isNoSuchUnit reports the manager saying it does not know the unit —
// for our purposes "not loaded", never a transport failure.
func isNoSuchUnit(err error) bool {
	var derr godbus.Error
	if !errors.As(err, &derr) {
		return false
	}
	switch derr.Name {
	case "org.freedesktop.systemd1.NoSuchUnit", "org.freedesktop.DBus.Error.UnknownObject":
		return true
	}
	return false
}

// statusFromProps reads the fields the runner needs out of a unit's
// full property map (Unit + Service interfaces).
// Not the unit's Environment, which the map holds too: that holds
// env_file values, and a status is read and kept everywhere
// (UnitEnvironment is its one reader).
func statusFromProps(props map[string]any) unitStatus {
	return unitStatus{
		LoadState:      propString(props, "LoadState"),
		ActiveState:    propString(props, "ActiveState"),
		SubState:       propString(props, "SubState"),
		Result:         propString(props, "Result"),
		MainPID:        propInt(props, "MainPID"),
		ExecMainPID:    propInt(props, "ExecMainPID"),
		ExecStart:      propExecStart(props),
		Sandboxed:      propYes(props, "PrivateUsers") && propYes(props, "PrivatePIDs"),
		ExecMainCode:   propInt(props, "ExecMainCode"),
		ExecMainStatus: propInt(props, "ExecMainStatus"),
		StopTimeout:    usecDuration(propInt(props, "TimeoutStopUSec")),
		Caps:           propCaps(props),
		ControlGroup:   propString(props, "ControlGroup"),
	}
}

// propCaps reads the unit's caps back as the manager holds them: a
// uint64 each, "infinity" (uint64 max) meaning none. A TasksMax= or a
// CPU quota too large for an int reads as none too, rather than as a
// bogus bound.
func propCaps(props map[string]any) resourceCaps {
	var c resourceCaps
	if v := propUint64(props, "MemoryMax"); v != infinityUSec {
		c.memoryMax = v
	}
	if v := propUint64(props, "TasksMax"); v != infinityUSec && v <= uint64(math.MaxInt) {
		c.tasksMax = int(v)
	}
	if v := propUint64(props, "CPUQuotaPerSecUSec"); v != infinityUSec && v/usecPerPercent <= uint64(math.MaxInt) {
		c.cpuQuota = int(v / usecPerPercent)
	}
	return c
}

// propUint64 reads a "t" property; absent reads as infinity (no cap),
// never as 0, which these properties never mean.
func propUint64(props map[string]any, key string) uint64 {
	switch v := props[key].(type) {
	case uint64:
		return v
	case int64:
		if v >= 0 {
			return uint64(v)
		}
	}
	return infinityUSec
}

// usecDuration converts a systemd USec property; "infinity" (uint64
// max, negative once narrowed) and anything that would overflow a
// Duration read as unknown (0) rather than as a bogus budget.
func usecDuration(usec int) time.Duration {
	if usec <= 0 || usec > int(time.Duration(1<<62)/time.Microsecond) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
}

// ListUnits returns the loaded units whose names match pattern, with
// only the fields the manager's listing carries (name and states).
func (c *userManagerClient) ListUnits(ctx context.Context, pattern string) ([]unitStatus, error) {
	conn, err := c.get()
	if err != nil {
		return nil, err
	}
	units, err := conn.ListUnitsByPatternsContext(ctx, nil, []string{pattern})
	if err != nil {
		c.dropIfDisconnected(conn, err)
		return nil, err
	}
	out := make([]unitStatus, 0, len(units))
	for _, u := range units {
		out = append(out, unitStatus{Name: u.Name, LoadState: u.LoadState, ActiveState: u.ActiveState, SubState: u.SubState})
	}
	return out, nil
}

func propString(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

// UnitEnvironment reads the service's Environment property on its own
// (statusFromProps leaves it out of every status). A unit the manager
// does not know has none.
func (c *userManagerClient) UnitEnvironment(ctx context.Context, name string) ([]string, error) {
	conn, err := c.get()
	if err != nil {
		return nil, err
	}
	p, err := conn.GetServicePropertyContext(ctx, name, "Environment")
	if err != nil {
		if isNoSuchUnit(err) {
			return nil, nil
		}
		c.dropIfDisconnected(conn, err)
		return nil, err
	}
	return envStrings(p.Value.Value()), nil
}

// envStrings reads an "as" value as godbus decodes it, a []string, as a
// copy: every entry as the manager gave it, in its order, an empty
// value included. Any other shape is nothing.
func envStrings(v any) []string {
	s, _ := v.([]string)
	return append([]string(nil), s...)
}

// propYes reads a namespace property as "in effect". PrivateUsers= is
// a bool on the bus; PrivatePIDs= is a string ("yes"/"no" — later
// versions add modes, none of which is "no").
func propYes(props map[string]any, key string) bool {
	switch v := props[key].(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "no" && v != "false"
	}
	return false
}

// propExecStart reads the argv out of the ExecStart property:
// a(sasbttttuii), an array of exec entries whose second member is the
// argv. godbus decodes that as [][]any with the argv a []string — the
// layout the integration lane pins against a real user manager, and
// the only one read here, since an unrecognised shape already yields
// nothing. Only the first entry is taken (every unit the runner builds
// has exactly one ExecStart); the result is a copy.
func propExecStart(props map[string]any) []string {
	entries, ok := props["ExecStart"].([][]any)
	if !ok || len(entries) == 0 || len(entries[0]) < 2 {
		return nil
	}
	argv, ok := entries[0][1].([]string)
	if !ok {
		return nil
	}
	return append([]string(nil), appArgv(argv)...)
}

func propInt(props map[string]any, key string) int {
	switch v := props[key].(type) {
	case int32:
		return int(v)
	case uint32:
		return int(v)
	case int64:
		return int(v)
	case uint64:
		return int(v) //nolint:gosec // PIDs and exit codes fit comfortably
	case int:
		return v
	}
	return 0
}

// managerClient embeds systemdConn, so this asserts both: the client is
// the runner's connection, and it is the probe and cache App.measureSandbox
// reaches when no connection is installed.
var _ managerClient = (*userManagerClient)(nil)
