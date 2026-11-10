package liveswap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"

	"github.com/smallhoursorg/motel/liveswap/backupdecl"
)

// Per-app sandboxing (issue #35) is systemd's own per-unit sandboxing
// on the user-manager runner: a user namespace (PrivateUsers=), a
// PID namespace (PrivatePIDs=, systemd 257+) and an IPC namespace
// (PrivateIPC=, with a /dev/shm of the unit's own; #311) on top of the
// mount, device, cgroup, seccomp and capability set.
//
// The user namespace is what closes /proc/<pid>/{root,environ,cwd} of
// every process outside it — commoncap refuses cross-user-namespace
// ptrace access whatever the uid — so the mount restrictions cannot be
// walked around via a same-UID neighbour's /proc. The PID namespace
// closes what is left: the supervisor, the user manager and every
// sibling become invisible and unsignalable.
//
// One sandbox, because the support matrix is one release: Debian 13
// ships systemd 257 and does not restrict unprivileged user
// namespaces. A host that cannot deliver it is refused rather than
// given a lesser one: Start fails, naming what the host lacks. (An
// earlier "filesystem" tier — the user namespace alone — was what
// Debian 12 and Ubuntu 24.04 could manage; it went with them.)
//
// The filesystem view is deny-by-default. An empty read-only tmpfs
// replaces the whole root (TemporaryFileSystem=/) and the only things
// that exist inside a unit are the ones this file names:
// sandboxBaseView and the app's own release, shared and run/<nonce>
// dirs. Nothing widens it. The guarantee is one sentence — anything not named is
// absent — and it holds by construction rather than by a list of
// things to hide being complete. Nothing has to be derived from the
// running configuration, and nothing ages: a secret that appears on
// the host after a unit started is absent from it for exactly the same
// reason every other path is.
//
// There is no policy. Every unit gets this sandbox — deploys,
// pre_start, crash relaunches, boot recovery, reattach — and nothing
// in the configuration can turn it off or widen it. What is decided
// at Start is whether this host can deliver it at all: a capability
// measured once per manager connection (not per config load: the
// measurement costs a whole unit — see App.measureSandbox), and a
// host that falls short fails Start with the probe's reason attached.
// That makes the sandbox an availability dependency, which is the
// deliberate trade: a supervisor that ran an app with no isolation —
// because the kernel changed its mind, or because one app was let out
// — hands that app every sibling's data and motel's own keys,
// since they all share one uid. There is no lesser tier and no
// escape, so nothing has to be recorded per instance either: a
// relaunch is sandboxed exactly like a deploy.

// bindPath is one of the app's own directories bound back into its
// view: dest is where the app sees it (and what WorkingDirectory,
// HOME and the {release_dir}/{shared_dir} placeholders name), source
// is what is mounted there. They differ only when the path to the
// liveswap root goes through a link (/srv/liveswap -> /mnt/liveswap);
// a link anywhere below the root is refused at launch
// (resolveBindSources). Binding the resolved path at the resolved
// path would put the
// directory somewhere the app has no reason to look, and its own
// spelling would be gone with the tmpfs that masks the root.
type bindPath struct {
	dest   string
	source string
}

// sandboxBaseView is everything an app sees of the host besides its
// own directories: the parts of the OS a program needs in order to
// execute at all. Under TemporaryFileSystem=/ this list, plus the
// app's own dirs, *is* the view — anything not
// named here is absent, not merely unreadable.
//
// /etc is named entry by entry rather than bound whole. Binding /etc
// would hand every app every other app's env_file and motel's own
// configuration, which is precisely the derived, ageing hidden set
// this model exists to delete; naming the dozen entries a runtime
// actually needs keeps the guarantee free of exceptions.
//
// Every entry is bound with IgnoreENOENT: no host has all of it
// (Debian 13 is merged-/usr, so /bin and /lib64 are symlinks rather
// than real directories and there is no /etc/pki; a host without
// systemd-resolved has no /run/systemd/resolve). A missing entry must
// not fail the unit. The names stay even where this distro does not
// use them: they are bound by name so a #! line or an interpreter's
// own ld path resolves to the spelling it was written with, and an
// app's tarball is not obliged to know which spelling its host uses.
var sandboxBaseView = []string{
	"/usr",
	// The usrmerge aliases: symlinks into /usr on Debian 13.
	"/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32",
	// The TLS trust store — the certificate directories themselves, not
	// the trees that contain them. /etc/ssl also holds /etc/ssl/private,
	// which nothing in an app needs: it is 0700 root:root on every cell
	// of the matrix, so binding /etc/ssl disclosed nothing today, but a
	// base view should name what apps need rather than rely on the
	// permissions of what it sweeps up. Recursive, so the hashed
	// symlinks under certs/ resolve into the /usr bound above.
	"/etc/ssl/certs", "/etc/ssl/openssl.cnf",
	"/etc/ca-certificates", "/etc/pki/tls/certs",
	// Name resolution and user lookup: getaddrinfo reads all four, and
	// without them every DNS lookup and every getpwuid fails.
	"/etc/resolv.conf", "/etc/hosts", "/etc/hostname", "/etc/nsswitch.conf",
	"/etc/passwd", "/etc/group",
	"/etc/localtime", "/etc/alternatives",
	// The dynamic linker's cache and configuration: without ld.so.cache
	// every dynamically-linked binary falls back to a search that
	// misses the distro's multiarch directories.
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	// systemd-resolved hosts symlink /etc/resolv.conf into here; if the
	// target is missing the link dangles and DNS fails silently on the
	// most common distro in the matrix.
	"/run/systemd/resolve",
}

// sandboxNeverReachable is what nothing an app runs may hold, by any
// route: the user manager's private socket and session bus (with
// PrivateUsers= mapping the app's uid one-to-one, reaching it lets the
// app ask the manager for a unit with no sandbox at all — read-only is
// no defence, since connecting to a socket is not a write) and the
// kernel interfaces the unit gets its own.
var sandboxNeverReachable = []string{"/run/user", "/dev", "/sys", "/proc"}

// sandboxMotelPaths is the supervisor's own state on a packaged
// install: TLS keys and certificates, the admin socket's directory,
// the documented env-file directories, the box applier's exchange tree
// (box/DESIGN-box.md, "Paths, owners, and who may touch what"), and
// notify's Alertmanager socket and the config and channel URLs motel
// renders for it (notify/DESIGN-notify.md): a mistake to put the
// liveswap *root* inside of, and never reachable as a bind source. The
// socket's mode admits the motel uid, which every app runs as, so the
// view is what keeps an app from posting alerts or silencing them.
var sandboxMotelPaths = []string{
	"/var/lib/motel", "/run/motel", "/etc/motel", "/etc/liveswap",
	"/var/lib/motel-box", "/run/motel-notify", "/var/lib/motel-notify",
}

// sandboxSpec is what the runner needs to render the sandbox
// properties of one unit. Paths are real host paths; nothing is
// remapped.
type sandboxSpec struct {
	// root is the liveswap root. It is not masked as such any more —
	// the whole filesystem is — but it is still what the containment
	// checks measure against: a bind that reaches under it is one app
	// reaching into another's data.
	root string
	// writable are this app's own directories bound back at their real
	// paths, read-write (BindPaths=): the release being started, the
	// shared dir and the instance's own run/<nonce> dir (where it binds
	// its socket). Its state.json, its tmp/ (the upload staging dir
	// — a running instance must never be able to rewrite the next
	// version's tarball) and its other releases are simply not named,
	// so they do not exist inside the unit.
	writable []bindPath
	// appDir is this app's own directory under the root, and appName
	// the name it was configured with: a writable bind that resolves
	// inside the root must stay inside *this* app's directory, or one
	// app would be binding another's data.
	appDir  string
	appName string
}

// resolveBindSources resolves every path this unit will bind and
// re-checks it, immediately before the unit is created — the last
// moment before systemd follows these paths, and the only check that
// sees what they point at *now*.
//
// The mandatory binds need this, and are the easier target: `shared`
// and the release dirs live under the shared motel UID, and
// appDirs.ensure's MkdirAll succeeds on a symlink. A `shared` that is
// a link to /run/user/<uid> would have BindPaths= mount the user
// manager's private socket inside the app's sandbox on the next
// launch. No app can plant that link from inside its sandbox (see the
// end of this comment), but nothing on disk says who did.
//
// A mandatory bind must therefore resolve to the directory it NAMES.
// The only difference permitted is one an alias on the liveswap root
// itself explains; anything else is refused, including targets that
// look innocent. That is not conservatism, it is the only line that
// can be drawn: a symlink in the app's own directory cannot be
// distinguished from one the operator meant. `shop/shared ->
// /mnt/shop-data` being a legitimate layout is exactly what would
// make /mnt/shop-data an acceptable target for a
// `blog` to aim at, and no list of other apps' data and env_file
// locations can close that — it would be incomplete by construction
// and would go stale, which is the derived set this model deletes.
//
// An operator putting an app's data on another disk uses a bind mount
// at the same path (`mount --bind`, or an fstab entry). A mount
// resolves to itself, so it never reaches this check at all — and an
// app cannot forge one.
//
// Paths that do not resolve are left as written, having already passed
// the same checks lexically at config load.
//
// This is a check on a pathname, and between it and the manager
// following that pathname a process sharing the motel UID could
// swap what it points at. No such process exists: every app is
// sandboxed, and inside its view the app directory is a tmpfs holding
// mount points it cannot unlink — the symlink would have to be
// planted from outside every sandbox, by motel itself or by root.
// That holds only while nothing else runs as the motel uid outside
// a sandbox. The check is for root's links: an operator's symlink,
// made to move a directory or by mistake, is what it refuses (above).
func (s *sandboxSpec) resolveBindSources() error {
	// Compare canonical against canonical: a resolved bind source is
	// canonical, so a lexical root would miss a sibling reached through
	// a link when the root itself is one (/srv/liveswap ->
	// /mnt/liveswap: a planted shared -> /mnt/liveswap/shop/shared
	// resolves *outside* the lexical root and the sibling check would
	// never fire). Unresolvable falls back to the lexical value.
	rootC := s.root
	if c, err := filepath.EvalSymlinks(s.root); err == nil {
		rootC = c
	}
	// The expected app directory is derived from the canonical root and
	// the configured app name, NOT from what appDir itself resolves to.
	// Resolving appDir would let the alias vouch for itself: a link at
	// <root>/blog pointing at <root>/shop would make
	// shop's directory the expected base, and every one of blog's
	// mandatory binds (release, shared, run/<nonce>) would then match
	// it exactly — handing the sibling's release and shared data to the
	// new sandbox.
	appC := filepath.Join(rootC, s.appName)
	own := func(dest, resolved string) error {
		// The only difference a mandatory bind may have from its
		// configured path is one an alias on the root explains.
		// Anywhere else is the app pointing at data that is not the
		// directory we meant to bind: its own app dir or releases/
		// (whose recursive bind would carry state.json, the upload
		// staging dir and every other release in under `shared`), a
		// sibling's, or somewhere outside the root entirely — which
		// used to be permitted as "moved to another disk" and was the
		// hole that let one app alias another's external data.
		rel, err := filepath.Rel(s.appDir, dest)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%q is not inside the app's own directory %s", dest, s.appDir)
		}
		expected := filepath.Join(appC, rel)
		if resolved != expected {
			return fmt.Errorf("%q resolves to %q, which is not the directory it names (expected %q). A mandatory bind may differ from its configured path only by an alias on the liveswap root itself; to put an app's data on another disk, bind-mount it at %q instead of symlinking — a mount resolves to itself, and an app cannot forge one", dest, resolved, expected, dest)
		}
		// It IS the directory it names. The root was vetted at config
		// load, but rootC is read from the live filesystem every launch,
		// so re-check what it actually points at now.
		return refusedAsBindSource(resolved)
	}
	for i, b := range s.writable {
		resolved, err := filepath.EvalSymlinks(b.dest)
		if err != nil {
			return fmt.Errorf("resolving %q before binding it into the sandbox: %w", b.dest, err)
		}
		if resolved != b.dest {
			if err := own(b.dest, resolved); err != nil {
				return fmt.Errorf("refusing to launch sandboxed: %w", err)
			}
			// Only the source moves: the app must still find its
			// release and data where its command line, HOME and
			// placeholders say they are.
			s.writable[i].source = resolved
		}
	}
	return nil
}

// refusedAsBindSource refuses a resolved mandatory bind source that
// reaches something no app may hold, in either direction — being
// *inside* a protected path is the obvious case, *containing* one just
// as bad, since these binds are recursive.
//
// /tmp, /var/tmp and /home are deliberately NOT here: a liveswap root
// may legitimately live under one of them and every bind beneath it
// would then "overlap" it. A unit gets its own /tmp and /home anyway,
// so nothing of the host's leaks through them.
func refusedAsBindSource(p string) error {
	// The root was vetted for these at config load (validateSandboxRoot),
	// but a source is resolved from the live filesystem at every launch,
	// and a root re-pointed since then can resolve to a path the unit's
	// file cannot carry (#380).
	if c, ok := unitSyntaxChar(p); ok {
		return fmt.Errorf("%q contains %s, which a systemd unit file cannot carry in a path: the bind would not survive a daemon-reload", p, c)
	}
	for _, c := range supervisorPaths() {
		if overlaps(p, c) {
			return fmt.Errorf("%q overlaps %s, which holds motel's own keys, sockets or config", p, c)
		}
	}
	for _, c := range sandboxNeverReachable {
		if overlaps(p, c) {
			return fmt.Errorf("%q overlaps %s, which no app may be given", p, c)
		}
	}
	// The base view is the one part of the host every unit shares, so a
	// bind source inside it is readable by every OTHER app whether or
	// not this app binds it — `shared -> /usr/local/app-data` would put
	// this app's data somewhere all its siblings can read, silently,
	// with nothing anywhere saying the sandbox was not what it seems. The
	// app-to-app boundary is exactly what the sandbox exists to build,
	// so this is a configuration error, not a choice.
	for _, b := range sandboxBaseView {
		if overlaps(p, b) {
			return fmt.Errorf("%q overlaps %s, which is bound read-only into EVERY app's sandbox — data placed there is readable by every other app; put it somewhere outside the base view", p, b)
		}
	}
	return nil
}

// supervisorPaths is motel's own state, each entry in both its
// configured and its canonical spelling.
//
// The static list is not sufficient here, and this is the one place the
// distinction matters. A path missing from it is still absent from
// every unit — nothing binds it — so for the VIEW the list is advisory.
// But a mandatory bind that resolves onto such a path *names* it, and
// naming is exactly what puts something in the view. Two ways that bit:
// `/var/lib/motel -> /srv/motel` made the lexical entry miss a
// planted `shared -> /srv/motel`, and a motel pointed at a
// non-default XDG data dir has its keys somewhere no literal mentions
// at all. Both would hand the supervisor's TLS keys to an app on the
// strength of one planted symlink.
//
// Resolved on each call rather than cached: this runs at most twice per
// launch (only for a bind whose source moved), and the answer must be
// what the filesystem says now, not what it said at config load — the
// same reason resolveBindSources exists at all.
func supervisorPaths() []string {
	out := make([]string, 0, len(sandboxMotelPaths)+8)
	add := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return
			}
			p = abs
		}
		p = filepath.Clean(p)
		out = append(out, p)
		if c, err := filepath.EvalSymlinks(p); err == nil && c != p {
			out = append(out, c)
		}
	}
	for _, p := range sandboxMotelPaths {
		add(p)
	}
	// Caddy's data and config dirs follow XDG_DATA_HOME/XDG_CONFIG_HOME,
	// so the TLS keys and the config autosave are not always where the
	// packaged layout puts them.
	add(caddy.AppDataDir())
	add(caddy.AppConfigDir())
	// systemd sets RUNTIME_DIRECTORY for RuntimeDirectory= (a
	// colon-separated list when there is more than one): the admin
	// socket, as the manager actually made it.
	for _, d := range filepath.SplitList(os.Getenv("RUNTIME_DIRECTORY")) {
		add(d)
	}
	return out
}

// overlaps reports whether two paths are the same or either contains
// the other. Containment in one direction hides a path; in the other
// it carries it along.
func overlaps(a, b string) bool { return pathWithin(a, b) || pathWithin(b, a) }

// sandboxSpecFor is the sandbox of one instance of this app: its
// release being started, its shared dir and its own run/<nonce> dir
// (where it binds its socket) writable, plus the base view. Everything
// else on the host — every other app, every other release, every other
// instance's run dir, motel's own state, the proxy/ dir holding the
// pinned socket names — is absent because nothing names it.
func (s *appSpec) sandboxSpecFor(releaseDir, nonce string) *sandboxSpec {
	run := s.dirs.runDir(nonce)
	return &sandboxSpec{
		root:    s.dirs.root,
		appDir:  s.dirs.app,
		appName: s.name,
		writable: []bindPath{
			{dest: releaseDir, source: releaseDir},
			{dest: s.dirs.shared, source: s.dirs.shared},
			{dest: run, source: run},
		},
		// Copied: resolveBindSources rewrites entries to what they
		// resolve to, and the spec must not mutate the app's config.
	}
}

// probeSandboxSpec is the spec the capability probe runs with: the
// same deny-by-default view as a real app unit, and nothing of its
// own bound into it. The probe needs no root and no per-config state
// — the view is a policy, not a derivation, so there is nothing about
// this host's configuration for it to reproduce.
func probeSandboxSpec() *sandboxSpec {
	return &sandboxSpec{}
}

// sandboxProbeCommand is the command the capability probe runs inside
// a unit built with probeSandboxSpec. It checks the namespaces and
// seccomp are in effect — not merely that the manager accepted the
// properties: the manager ignores PrivatePIDs= on a kernel without PID
// namespaces, and where seccomp is unavailable to it (no seccomp in
// the kernel, SYSTEMD_SECCOMP=0 in its environment, or an outer filter
// that refuses to let it install one) skips every seccomp-backed
// setting at once with a debug line, while a user namespace the kernel
// or an LSM refuses fails the unit outright. In a user namespace the
// process's own uid_map covers a single id, not the 2^32-1 of the
// initial namespace; in a PID namespace the executed process is PID 1.
//
// Two checks cover seccomp. `Seccomp: 2` in the process's status says
// some filter is loaded, but neither whose — one the manager itself
// runs under reads 2 as well — nor which of the unit's several. So
// the probe also tries `unshare -U`, which RestrictNamespaces= (the
// empty set, sandboxProperties) refuses with EPERM: that setting is
// skipped exactly when the whole set is, so a namespace the probe can
// make means the unit's own filters are not there, whatever mode it
// inherited. An outer filter cannot refuse that unshare for the unit
// either: the manager makes the same unshare(CLONE_NEWUSER) call for
// PrivateUsers=, so such a filter fails the unit first. What neither
// check can see is SystemCallFilter= alone not loading while the rest
// do (DESIGN-sandbox.md, "What this does NOT stop").
//
// Two more cover sibling IPC (#311). The IPC namespace must be the
// unit's own: where the kernel refuses one the manager logs
// "PrivateIPC=yes is configured, but IPC namespace setup failed,
// ignoring" and goes on in its own, and PrivateIPC= then mounts a
// fresh mqueuefs on /dev/mqueue, which the kernel allows only to a
// process whose user namespace owns its IPC namespace — so the mount
// fails the unit (226/NAMESPACE, measured) rather than letting it run
// shared, and when it succeeds its root inode belongs to the mounter.
// The probe checks both: /dev/mqueue is owned by its own uid (the
// host's, bound in, reads as root or nobody), and its
// /proc/self/ns/ipc is not the one motel passes in
// (MOTEL_IPC_NS: motel's own, which is the manager's while
// neither motel.service nor the user manager sets PrivateIPC=).
// Two signals, since each alone can be made vacuous — the first by a
// motel running as uid 0, the second by a drop-in on
// motel.service. And /dev/shm must be the unit's own tmpfs: the
// probe writes probeShmCanary there, which must then not be in
// motel's /dev/shm, the host's (checkShmCanary) — a write that
// fails is a failure of its own, named on the line.
func sandboxProbeCommand(nonce string) []string {
	// The echo lands in the journal under the probe unit's identifier
	// (motel-sandbox-probe), so a refused host can be diagnosed
	// from `journalctl -t motel-sandbox-probe`. The argv reaches the
	// shell byte for byte, as every unit's command does (no-env-expand,
	// see unitProperties), so the script is spelled as a shell reads it.
	// /proc/self/status is read by the shell itself, so the mode is the
	// probe process's own, and a status with no Seccomp: line (a kernel
	// without seccomp) reads `absent`. unshare (util-linux, Essential on
	// Debian) counts as refused only on EPERM's text, which LC_ALL=C
	// (probeStartSpec) keeps in English: a missing binary or a namespace
	// limit (ENOSPC) is a failure of its own, named on the journal line,
	// not a pass.
	return []string{"/bin/sh", "-c", `read _ _ n < /proc/self/uid_map; s=absent; while read -r k v; do [ "$k" = Seccomp: ] && s=$v; done < /proc/self/status; e=$(unshare -U true 2>&1); case $?:$e in 0:*) u=allowed ;; *"Operation not permitted"*) u=refused ;; *) u="failed: $e" ;; esac; i=$(readlink /proc/self/ns/ipc); m=$(stat -c %u /dev/mqueue 2>&1); if echo probe > ` + probeShmCanary(nonce) + `; then w=ok; else w=failed; fi; echo "sandbox probe: uid_map range $n, pid $$, seccomp $s, unshare $u, ipc $i (manager $MOTEL_IPC_NS), mqueue owner $m (self $(id -u)), shm write $w"; [ "$n" != 4294967295 ] && [ "$$" = 1 ] && [ "$s" = 2 ] && [ "$u" = refused ] && [ -n "$i" ] && [ "$i" != "$MOTEL_IPC_NS" ] && [ "$m" = "$(id -u)" ] && [ "$w" = ok ]`}
}

// probeShmCanary is the file the probe unit writes into its /dev/shm:
// unique per probe, so a leftover of one run cannot answer for the
// next.
func probeShmCanary(nonce string) string { return "/dev/shm/motel-sandbox-probe-" + nonce }

// ownIPCNamespace is the IPC namespace motel runs in, as the probe
// compares it: the manager's, since motel.service and the motel
// user's manager both sit in the initial one.
func ownIPCNamespace() (string, error) {
	ns, err := os.Readlink("/proc/self/ns/ipc")
	if err != nil {
		return "", fmt.Errorf("reading motel's own IPC namespace: %w", err)
	}
	return ns, nil
}

// checkShmCanary is the half of the /dev/shm check the probe cannot
// make from inside its unit: the canary it wrote must not be in
// motel's /dev/shm, which is the host's (motel.service sets no
// PrivateDevices=, and PrivateDevices= binds the host's anyway).
// Found, every app's /dev/shm is one directory. Removed either way.
// A motel that cannot see /dev/shm at all (a drop-in hiding it)
// cannot make the check, and says so rather than passing it.
func checkShmCanary(nonce string) error {
	if st, err := os.Stat("/dev/shm"); err != nil || !st.IsDir() {
		return fmt.Errorf("motel cannot see /dev/shm, so it cannot check that the probe unit's is its own: %w", err)
	}
	p := probeShmCanary(nonce)
	_, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	_ = os.Remove(p)
	if err != nil {
		return fmt.Errorf("checking %s after the probe: %w", p, err)
	}
	return fmt.Errorf("the probe unit's /dev/shm is the host's: %s, written inside the unit, was visible outside it", p)
}

// probeStartSpec is one probe unit's spec. LC_ALL=C because the
// script reads unshare's error text, and a unit's environment is the
// manager's defaults under the one given here (runner_systemd.go,
// invariant 8): a host whose locale translates strerror would
// otherwise turn a refusal into a failure, on a capable box. ipcNS is
// motel's own IPC namespace (ownIPCNamespace), which the probe's
// must differ from.
func probeStartSpec(nonce, ipcNS string) startSpec {
	return startSpec{
		app:     "sandbox-probe",
		version: "probe",
		nonce:   nonce,
		command: sandboxProbeCommand(nonce),
		dir:     "/",
		env:     []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "MOTEL_IPC_NS=" + ipcNS},
		grace:   5 * time.Second,
		sandbox: probeSandboxSpec(),
		probe:   true,
	}
}

// sandboxProbeTimeout bounds one probe unit: a shell that exits at
// once, plus the manager's namespace setup. Two attempts of it (each
// followed, on a timeout, by the stop of the unit that hung: its grace
// plus stopSlack), after the manager connection's two dials (dialTimeout
// each to connect and again to authenticate: NewConnection opens two
// sockets) and the two manager probes App.Start makes (probeTimeout
// each), are what a start spends before Caddy reports ready — 40 + 20
// + 130 = 190 s bounded, which motel.service's TimeoutStartSec
// (packaging/motel.service, 240 s) is sized above with room for
// Caddy's own start. Change one, re-derive the other.
const sandboxProbeTimeout = 30 * time.Second

// probeSandboxCapability reports whether the host delivers the
// sandbox. One attempt, one verdict: the support matrix is Debian 13
// (systemd 257), where PrivatePIDs= exists and the kernel does not
// restrict unprivileged user namespaces, and there is no lesser
// sandbox to try next.
//
// It stays a measurement rather than a version check because the host
// still gets a vote: a container or LXC VPS, a kernel built without
// user namespaces, or an LSM that refuses them all fail the unit on a
// manager whose version says it should have worked. A unit the manager
// refuses, or a probe that exits non-zero (a property silently
// ignored), is the reason Start's refusal carries.
//
// The probe also proves the base view: /bin/sh has to exist inside the
// unit for the script to run at all, so a host whose runtime the base
// view fails to name cannot pass.
//
// The verdict is an error: nil when the probe unit ran, otherwise why
// it did not, with what the supported host looks like attached — this
// is the text an operator reads when the server will not come up.
func probeSandboxCapability(r runner) error {
	attempt := func() error {
		nonce, err := newNonce()
		if err != nil {
			return err
		}
		ipcNS, err := ownIPCNamespace()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
		defer cancel()
		if err := r.RunOnce(ctx, probeStartSpec(nonce, ipcNS)); err != nil {
			return err
		}
		return checkShmCanary(nonce)
	}
	err := attempt()
	if errors.Is(err, context.DeadlineExceeded) {
		// One retry, for a timeout only. A probe that fails is a
		// measurement; one that times out is the absence of one — the
		// shape a capable host under boot load produces — and this
		// verdict decides whether motel starts at all. A refused
		// start exits 1, which motel.service deliberately never
		// retries (RestartPreventExitStatus=1: a refusal is final
		// until an operator acts), so at boot there is no next start
		// to re-measure on: without the retry, one slow attempt keeps
		// a fully capable box down until someone starts it by hand.
		// This retry is the one re-measurement such a host gets. A
		// genuinely incapable host is not slowed by this — the manager
		// refuses, or the unit fails, fast and with a reason on the
		// first attempt. The cost is a worst case of two probe budgets
		// on a host that times out twice, paid on a Start that was
		// about to be refused anyway.
		err = attempt()
	}
	if err != nil {
		return fmt.Errorf("sandbox probe unit: %w — check `journalctl -t motel-sandbox-probe` for the probe's own output, if it got as far as running; the supported host is Debian 13: PrivatePIDs= needs systemd 257, both namespaces need a kernel that permits them, and the syscall filter needs seccomp the user manager can apply to the unit (the probe's line says which check failed: \"seccomp 0\" or \"unshare allowed\" means the manager skipped the unit's seccomp settings; an ipc equal to the manager's, or a mqueue owner other than self, means the unit has no IPC namespace of its own, and \"shm write failed\" that its /dev/shm is not writable)", err)
	}
	return nil
}

// validateSandboxRoot rejects a liveswap root inside motel's own
// state: the root holds the apps' own data, and putting it inside the
// supervisor's TLS keys or env files is a configuration mistake worth
// failing on, sandbox or no sandbox.
//
// A root under /tmp, /var/tmp or /home is deliberately NOT refused:
// systemd creates the mount points for
// TemporaryFileSystem= and BindPaths= inside the namespace, so a root
// under /tmp, /var/tmp or /home is masked with everything else and the
// app's own dirs bind back into it exactly as they do under /var/lib.
// The real-systemd integration lane runs with a /var/tmp root and
// asserts the view from inside. Refusing those would turn an odd-but-working
// setup into a server that will not start.
func validateSandboxRoot(root string) error {
	// Both spellings: the check is lexical, so `/srv/liveswap ->
	// /var/lib/motel/apps` would otherwise walk straight past it.
	// An unresolvable root (not created yet, which is the normal case
	// at first config load) falls back to the lexical value.
	roots := []string{root}
	if c, err := filepath.EvalSymlinks(root); err == nil && c != root {
		roots = append(roots, c)
	}
	supervisor := supervisorPaths()
	for _, r := range roots {
		// The root reaches the unit as BindPaths=, WorkingDirectory=
		// and ExecStart= text, which the manager writes to the unit's
		// transient file and reads back at every daemon-reload. The
		// characters that grammar owns — whitespace and control
		// characters, the ':' between a bind's source and destination,
		// '\' and both quotes — come back as a different bind list (a
		// space, a colon), as a unit the manager can no longer load (a
		// backslash: bad-setting), or not at all (a tab, a quote): the
		// unit then runs on, unknown to motel (#380, measured).
		if c, ok := unitSyntaxChar(r); ok {
			where := fmt.Sprintf("root %q", root)
			if r != root {
				where = fmt.Sprintf("root %q, which resolves to %q,", root, r)
			}
			return fmt.Errorf("%s contains %s, which a systemd unit file cannot carry in a path: the apps' bind mounts would not survive a daemon-reload; use a root of plain characters (the default is /var/lib/liveswap)", where, c)
		}
		for _, h := range supervisor {
			if pathWithin(r, h) {
				return fmt.Errorf("root %q is inside %s, which holds motel's own keys, sockets and env files; use a root outside it (the default is /var/lib/liveswap)", root, h)
			}
		}
		// A root inside the base view would put EVERY app's state and
		// every app's data under a tree bound read-only into every
		// other app's sandbox — the app-to-app boundary gone wholesale,
		// which is worse than the single-app case closedPath refuses.
		for _, b := range sandboxBaseView {
			if pathWithin(r, b) {
				return fmt.Errorf("root %q is inside %s, which is bound read-only into every app's sandbox — every app's data would be readable by every other; use a root outside the base view (the default is /var/lib/liveswap)", root, b)
			}
			// Overlap is symmetric here as it is for binds: a root that
			// CONTAINS a base-view entry is the same exposure reached
			// from the other side, because an app's directory is derived
			// from the root and its name. `root /etc/ssl` passes the
			// check above, and then an app called `certs` puts its
			// releases and shared dir at /etc/ssl/certs — an entry every
			// sandbox binds read-only and recursively.
			if pathWithin(b, r) {
				return fmt.Errorf("root %q contains %s, which is bound read-only into every app's sandbox — an app whose name lands on that path would have its releases and shared dir readable by every other app; use a root that neither sits inside nor contains a base-view entry (the default is /var/lib/liveswap)", root, b)
			}
		}
	}
	return nil
}

// unitSyntaxChar reports the first character of p that a unit file's
// path grammar gives meaning to, described for a message. '%' is not
// one: the manager escapes a specifier on the way out and reads it
// back; nor is any non-ASCII letter (both measured unchanged across a
// daemon-reload).
func unitSyntaxChar(p string) (string, bool) {
	for _, c := range p {
		switch {
		case c == ' ':
			return "a space", true
		case c == ':':
			return "a colon", true
		case c == '\\':
			return "a backslash", true
		case c == '"' || c == '\'':
			return "a quote", true
		case c < 0x20 || c == 0x7f:
			return fmt.Sprintf("a control character (U+%04X)", c), true
		}
	}
	return "", false
}

// homeOutsideView reports an operator-set HOME that the unit's view
// does not contain. buildEnv applies the sandbox HOME (the shared dir)
// BEFORE env_file and inline env so an operator can point it elsewhere
// — inside the release dir, say — which is deliberate. What is not
// deliberate is doing it by accident: a HOME that is absent inside the
// unit surfaces as an ENOENT from whichever runtime touches $HOME
// first, naming no cause. The last assignment wins, as it does for
// systemd's Environment=.
func homeOutsideView(env []string, s *sandboxSpec) (string, bool) {
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	if home == "" {
		return "", false
	}
	// Resolved, because the view is recognised by its bind sources and
	// a symlink would walk straight past a lexical comparison — the
	// same reason resolveBindSources canonicalises before checking.
	resolved := home
	if c, err := filepath.EvalSymlinks(home); err == nil {
		resolved = c
	}
	if s.inView(resolved) {
		return "", false
	}
	return home, true
}

// pathWithin reports whether p equals dir or lies beneath it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// AdminSocketInView says what makes a unix socket at path — the admin
// API's listener — reachable from an app's sandbox, or "" where no
// app's view holds it. Read by motel's command gate (cmd/motel), which
// lets a unix listener through only when this says nothing. Two places
// are an app's: under the liveswap root (empty: the default), where an
// app's shared and release directories are bound into its sandbox,
// writable, and every name is some app's by its name; and under a
// base-view entry, bound read-only into every sandbox — connecting to
// a socket is no write, so read-only is no defence. The check is
// lexical, as validateSandboxRoot's is, so both spellings of the root
// and of the socket's directory are measured: a link at either would
// walk past one.
func AdminSocketInView(root, path string) string {
	if root == "" {
		root = backupdecl.DefaultRoot
	}
	path = filepath.Clean(path)
	paths := []string{path}
	// The socket need not exist yet; its directory may, and may lead
	// elsewhere.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		if p := filepath.Join(dir, filepath.Base(path)); p != path {
			paths = append(paths, p)
		}
	}
	roots := []string{filepath.Clean(root)}
	if c, err := filepath.EvalSymlinks(root); err == nil && c != roots[0] {
		roots = append(roots, c)
	}
	for _, p := range paths {
		for _, r := range roots {
			if pathWithin(p, r) {
				return fmt.Sprintf("%s is under the liveswap root %s, where an app's shared and release directories are bound into its sandbox, writable", path, root)
			}
		}
		for _, b := range sandboxBaseView {
			if pathWithin(p, b) {
				return fmt.Sprintf("%s is under %s, which is bound read-only into every app's sandbox, and connecting to a socket is no write", path, b)
			}
		}
	}
	return ""
}

// inView reports whether p exists inside a unit built from this spec:
// under one of the app's own directories, or in the base view.
// Everything else on the host is absent, so this is
// the whole of the view — the same two sources sandboxProperties
// renders, and the reason it can be answered without asking systemd.
func (s *sandboxSpec) inView(p string) bool {
	// Destination AND source. The caller resolves symlinks before
	// asking, and a resolved path is canonical, so a bind whose source
	// differs from its destination — a symlinked liveswap root — is
	// only recognisable by its
	// source. Testing destinations alone would refuse `./server` under
	// a root like /srv/liveswap -> /mnt/liveswap on every launch.
	both := func(dest, source string) bool {
		if source == "" {
			source = dest
		}
		return pathWithin(p, dest) || pathWithin(p, source)
	}
	for _, b := range s.writable {
		if both(b.dest, b.source) {
			return true
		}
	}
	for _, base := range sandboxBaseView {
		if pathWithin(p, base) {
			return true
		}
	}
	return false
}

// warnEnvFileInView warns about the one place an env_file can still
// land inside a sandbox view: the app's own directories, which are the
// only host paths bound writable. An app's own environment is
// legitimately reachable — protecting an app from itself is a stated
// non-goal — but the *file* is a different thing from the variables.
// Under `shared/` the app can rewrite it and so choose the environment
// of its own next launch, which outlives the compromise that arranged
// it. Everywhere else an env_file is simply absent from every view,
// which is why this is the whole of what used to be a derived,
// per-config hidden set.
func warnEnvFileInView(logger *zap.Logger, specs map[string]*appSpec) {
	if logger == nil {
		return
	}
	for name, spec := range specs {
		if spec.envFile == "" {
			continue
		}
		// Both spellings of both sides, as validateEnvFileIsolation
		// does: comparing lexically alone misses an env_file that is a
		// link into the app's own shared dir, and misses the canonical
		// spelling of one under a symlinked liveswap root. A warning
		// that a symlink walks past is worse than none — it reads as
		// "checked, and fine".
		within := func(dir string) bool {
			for _, f := range absAndCanonical(spec.envFile) {
				for _, d := range absAndCanonical(dir) {
					if pathWithin(f, d) {
						return true
					}
				}
			}
			return false
		}
		f := spec.envFile
		switch {
		case within(spec.dirs.shared):
			logger.Warn("env_file is inside the app's shared dir, which the app can read and rewrite inside its sandbox",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/motel), where no app can see it"))
		case within(spec.dirs.releases):
			logger.Warn("env_file is inside the app's release dir, which the app can read and rewrite inside its sandbox, as well as receive its variables",
				zap.String("app", name), zap.String("env_file", f),
				zap.String("fix", "move it outside the liveswap root (the documented location is /etc/motel), where no app can see it"))
		}
	}
}

// absAndCanonical returns a path made absolute and cleaned, plus its
// resolved form when that differs and resolves at all. Containment
// checks are lexical, so both spellings have to be compared or a
// symlink walks straight past them.
func absAndCanonical(p string) []string {
	if p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil
		}
		p = a
	}
	p = filepath.Clean(p)
	out := []string{p}
	if c := canonicalDeepest(p); c != p {
		out = append(out, c)
	}
	return out
}

// canonicalDeepest is EvalSymlinks for a path whose leaf may not exist
// yet: it resolves the deepest ancestor that does and re-appends the
// rest unchanged, falling back to p when nothing resolves.
//
// EvalSymlinks fails outright on a missing final component, and an
// env_file is explicitly allowed to be absent until the first deploy —
// so resolving the whole path would silently give back only the
// lexical spelling for exactly the configurations that need checking.
// With `/safe/link -> /var/lib/liveswap/blog/shared` and
// `env_file /safe/link/shop.env` not yet created, blog's own bind
// exposes that file the moment it appears, while a lexical comparison
// sees two unrelated strings. The link is in the PARENT, which is why the
// deepest existing ancestor is the thing to resolve.
func canonicalDeepest(p string) string {
	rest := ""
	for cur := p; ; {
		if c, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(c, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without resolving anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// validateEnvFileIsolation refuses a configuration in which one app's
// env_file lands inside ANOTHER app's view. An app's own env file being
// reachable is a stated non-goal to defend against — it receives those
// variables anyway — but a sibling's is the secret this whole feature
// exists to keep apart, and the documented guarantee says such a file
// is absent from every other app's view.
//
// Two routes make that false, neither of which the deny-by-default view
// closes on its own, because both are things the operator NAMED:
//
//   - the base view, which is bound read-only into every unit, so an
//     env_file under /usr or a named /etc entry is readable by every
//     app on the box;
//   - another app's own directories, which sandboxSpecFor binds into
//     its unit — `shared/` read-WRITE. An env file parked in a
//     neighbour's shared dir is readable and rewritable by that
//     neighbour, which is the same exposure by a shorter route.
//
// A hard error rather than a warning: the alternative is documenting
// that env files are only usually isolated. Validate runs before a
// reload is committed, so a configuration rejected here leaves the
// previous one serving.
func validateEnvFileIsolation(a *App) error {
	for name, cfg := range a.Apps {
		if cfg == nil || cfg.EnvFile == "" {
			continue
		}
		for _, f := range absAndCanonical(cfg.EnvFile) {
			for _, b := range sandboxBaseView {
				if pathWithin(f, b) {
					return fmt.Errorf("app %s: env_file %q is inside %s, which is bound read-only into EVERY app's sandbox — every other app on this box could read it; keep env files outside the base view (the documented location is /etc/motel, which no app may name)", name, cfg.EnvFile, b)
				}
			}
			for other, ocfg := range a.Apps {
				if other == name || ocfg == nil {
					continue
				}
				// The neighbour's own bound directories, both bound
				// read-write into its unit (the release being started
				// and its shared dir), so this is the one route on
				// which the file can also be REWRITTEN by another app.
				od := newAppDirs(a.Root, other)
				for _, d := range []struct{ path, what, rw string }{
					{od.shared, "shared dir", "read and rewrite"},
					{od.releases, "release dirs", "read and rewrite"},
				} {
					// Both spellings: the env file is compared
					// canonically, so a lexical
					// app dir alone would miss it whenever the root is a
					// symlink (/srv/liveswap -> /mnt/liveswap, an env
					// file configured as /mnt/liveswap/blog/shared/...).
					for _, dp := range absAndCanonical(d.path) {
						if pathWithin(f, dp) {
							return fmt.Errorf("app %s: env_file %q is inside app %s's %s %q, which is bound into %s's own sandbox — %s could %s it; move the env file outside the liveswap root (the documented location is /etc/motel)", name, cfg.EnvFile, other, d.what, d.path, other, other, d.rw)
						}
					}
				}
			}
		}
	}
	return nil
}
