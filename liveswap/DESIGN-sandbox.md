# DESIGN — per-app sandboxing

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- liveswap/DESIGN-sandbox.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

Every app unit runs under systemd's own per-unit sandboxing on the
motel user's manager: a user namespace, a PID namespace and a
deny-by-default filesystem view. There is one sandbox, every unit gets
it, and no configuration turns it off or widens it. The supported host
is Debian 13 (systemd 257). The implementation is `liveswap/sandbox.go`
(the spec, the base view, the probe) and `sandboxProperties` in
`liveswap/systemd_dbus.go` (the rendered property set); the reasoning
that chose the mechanism is [DESIGN-threat-model.md](../DESIGN-threat-model.md),
"The shared-UID rule".

## Why this feature exists

motel's trust model is one box, one trust domain: every app runs as
the `motel` user. Without a sandbox any app can read every other
app's files, env files, the TLS private keys and
`/proc/<sibling>/environ`, and can connect to the admin unix socket
(same UID). The realistic attacker is not a malicious tenant — it is a
poisoned transitive dependency (npm's 2025 wave: runtime payloads doing
broad filesystem sweeps for `.env` files, tokens and wallets, then
exfiltrating).

Containers are the industry answer and the product explicitly rejects
them. The sandbox is the container *primitives* without the container
*product*: apps stay files in directories, supervised by motel — but
each app's filesystem view contains only its own world, and siblings
are not merely unreadable, they do not exist.

What this stops: lateral movement from one compromised app (sibling
files/env/releases, TLS keys, admin socket, sibling `/proc`, signals,
sibling SysV IPC, message queues and `/dev/shm`).
What this does NOT stop, stated honestly: theft of the compromised
app's own secrets (its env is its env), network exfiltration (netns is
shared by design), resource exhaustion (the per-app caps are the
operator's to set, none by default — DESIGN-liveswap.md "Resource
caps"; the sandbox only keeps a set cap from being rewritten),
install-time supply-chain attacks (those run in
CI, not on the box), a syscall filter the kernel refused on a host
that already filters the user manager. Nor, on a box without
docs/hardening.md step 5, the module-backed protocols under the
admitted families (`sctp`, `l2tp_ip`, `l2tp_ip6`, `smc`), which an app
opens and the kernel loads for it; nor, on any box, the kernel
modules an app loads through the families it has: a netlink
protocol's or generic-netlink family's module on a `socket()` or a
`GETFAMILY`, which hands it a live netlink socket into that module's
parsers, and any aliased family's module on a `socketpair()`, which
hands it nothing, as any user; a compromised app reaches those
modules' init and parsers, and the few requests they answer
unprivileged (both accepted by decision, S3 × O3 × D5 = 45;
"Module-backed protocols" below).

The refused filter is the one route around the syscall filter that the
capability probe cannot see. Each seccomp filter on a process counts
against one kernel budget of 32768 instructions for the whole chain.
On a host whose user manager already runs under a long outer chain (a
container runtime's profile, say), the kernel can refuse the unit's
`SystemCallFilter=`, which the manager loads after its other seccomp
settings. The kernel refuses with `ENOMEM`; libseccomp 2.6 hands that
back as `ECANCELED` unless a filter asks for raw errors
(`sys_filter_load`, `SCMP_FLTATR_API_SYSRAWRC`), which systemd 257's
do not; and systemd counts only `EPERM`, `EACCES`, `ENOMEM` and
`EFAULT` as fatal (`ERRNO_IS_NEG_SECCOMP_FATAL`), so it logs the
failure at debug level and starts the unit anyway, its other filters
in force, without the allow list and the `io_uring_setup` deny. Such a
unit reads `Seccomp: 2` and refuses `unshare -U` like any other, so
nothing the probe reads tells it apart (S5 × O5 × D5 = 125: scores to
fix; accepted by decision, user, 2026-10-19). A second probe unit
could: one with every seccomp setting but `SystemCallFilter=`, whose
`Seccomp_filters:` count the whole unit must exceed. That is not
built (the same decision); it would be a second throwaway unit inside
the start budget that `TimeoutStartSec=` is sized on.

The mount namespace alone does not close the threat model: the
attacker just described dumps `process.env` first, and a stolen ACME
token lets them issue or alter certificates. `buildEnv` therefore
inherits only an allowlist, pinned by
`TestBuildEnvDoesNotLeakSupervisorSecrets`; the requirement is restated
below as a contract the sandbox path must keep.

## Behaviour specification (normative)

- Sandboxing MUST be unconditional: every unit — deploy, `pre_start`,
  crash relaunch, boot recovery, reattach — gets the one sandbox, and
  there is no configuration surface that turns it off or widens it.
  The host MUST be probed by running a throwaway unit and checking the
  namespaces, seccomp and `/dev/shm` from inside (the manager silently
  ignores `PrivatePIDs=` on a kernel without PID namespaces, runs the
  unit in its own IPC namespace where it cannot make one, and skips
  `SystemCallFilter=` where seccomp is unavailable, so accepting a
  property proves nothing), and a host that cannot deliver the sandbox
  MUST fail the start with the probe's reason — a weaker sandbox
  accepted silently is the "looks configured, quietly weaker" trap, and
  no sandbox at all, on a shared uid, hands one app every sibling's
  data and motel's own keys. The runner MUST refuse a start spec with no sandbox
  (`TestUnitRefusesToRunWithoutASandbox`).
- The sandboxed filesystem view MUST be **deny-by-default**: an empty
  read-only tmpfs replaces the whole filesystem
  (`TemporaryFileSystem=/:ro`) and the only things that exist inside a
  unit are the ones explicitly bound. The guarantee is one sentence —
  *anything not named is absent* — and it holds by construction rather
  than by a list of things to hide being complete and current.
  `ProtectSystem=`, `ProtectHome=` and `InaccessiblePaths=` are NOT
  set: nothing is left for any of them to act on, and "absent" is the
  stronger statement.
- What a unit's view MUST contain, at REAL host paths (no remapping —
  `current` symlinks, state paths and operator debugging all assume
  real paths):
  - the app's release dir being started (rw), its shared dir (rw) and
    its own `run/<nonce>/` dir (rw — where it binds its socket; no
    other instance's run dir is in the view, and motel pins the
    socket by inode and never dials the app-writable name, see
    DESIGN-threat-model.md "The socket name is app-writable"), and
    nothing else of the liveswap root. Its `state.json`, its
    `tmp/` (the upload staging dir — a running instance must never be
    able to rewrite the next version's tarball), its `current` symlink
    and its other releases are simply never named, so they do not
    exist inside the unit;
  - a private `/tmp` and `/var/tmp` (`PrivateTmp=`), a minimal `/dev`
    (`PrivateDevices=`) with a `/dev/shm` of the unit's own
    (`TemporaryFileSystem=/dev/shm:mode=1777,size=50%` — the kernel's
    default for the host's `/dev/shm`, where systemd's own for a
    `TemporaryFileSystem=` is 10% of RAM; `memory_max` is the
    practical cap — over the host's, which `PrivateDevices=` binds in;
    #311) and the API VFS. `MountAPIVFS=` is NOT set: `/proc`, `/sys`
    and `/dev` are mounted inside the tmpfs by `PrivateDevices=` and
    `PrivatePIDs=`;
  - a **base view** of the OS, bound read-only, named entry by entry
    in `sandboxBaseView` (liveswap/sandbox.go): `/usr` and the usrmerge
    aliases (`/bin`, `/sbin`, `/lib*`), the TLS trust store
    (`/etc/ssl/certs`, `/etc/ssl/openssl.cnf`, `/etc/ca-certificates`,
    `/etc/pki/tls/certs` — the certificate directories themselves, never
    the `/etc/ssl` tree that also holds `/etc/ssl/private`), name and user
    resolution (`/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname`,
    `/etc/nsswitch.conf`, `/etc/passwd`, `/etc/group`), `/etc/localtime`,
    `/etc/alternatives`, the linker's cache and configuration
    (`/etc/ld.so.cache`, `/etc/ld.so.conf`, `/etc/ld.so.conf.d`), and
    `/run/systemd/resolve`. Every entry is bound with `IgnoreENOENT`:
    no host has all of it (Debian 13 is merged-/usr, so `/bin` and
    `/lib64` are symlinks and there is no `/etc/pki`; a host without
    systemd-resolved has no `/run/systemd/resolve`). The names stay
    even where this distro does not use them, so a `#!` line or an
    interpreter's own ld path resolves to the spelling it was written
    with.
- `/etc` MUST NOT be bound whole. Binding it would hand every app
  every other app's `env_file` and motel's own configuration —
  precisely the derived, ageing hidden set this model exists to
  delete. Naming the dozen entries a runtime actually needs is what
  keeps the guarantee free of exceptions.
- The view therefore MUST NOT contain, without anything having to
  enumerate them: other apps' directories, `/var/lib` outside the
  liveswap root's path to this app's own dirs (motel's TLS keys
  and the box applier's exchange tree live there), `/run/motel`
  (admin socket), `/etc/motel` and
  `/etc/liveswap` (env files), `/home`, `/root`, `/opt`, `/srv`, or
  any operator `env_file` wherever it lives. `/run/user` MUST NOT be
  reachable by any route, read-only included: with `PrivateUsers=`
  mapping the app's uid one-to-one, the user manager's private socket
  lets an app ask the manager for a unit with no sandbox at all, and
  connecting to a socket is not a write (`sandboxNeverReachable`).
  `/sys/fs/cgroup` MUST be read-only (`ProtectControlGroups=`): the
  delegated subtree is owned by the app's own UID, so resource caps
  are otherwise rewritable.
- `BindPaths=` and `BindReadOnlyPaths=` are two views of ONE list in
  the manager, and setting either to an empty array RESETS that list
  rather than adding nothing. An empty `BindPaths=` emitted after the
  base view therefore takes `/usr` and `/bin` away again and the unit
  fails `203/EXEC` — measured, and exactly what the capability probe
  (which has no directories of its own) would otherwise do. Empty list
  properties MUST NOT be emitted.
- An app's mandatory binds — the release being started, `shared/` and
  the instance's `run/<nonce>/` — MUST resolve to the directories they name, and a launch whose bind
  source resolves anywhere else MUST be refused. The only permitted
  difference is one an alias on the liveswap root itself explains.
  Resolution to a path outside the root is unsound: a symlink in the
  app's own directory cannot be distinguished from one the operator
  meant (no app can plant one from inside its sandbox, but nothing on
  disk says who did), and a sibling's legitimate external data
  (`shop/shared -> /mnt/shop-data`) would be an acceptable target for
  `blog` to aim at. **The supported way to
  put an app's data on another disk is a bind mount at the same
  path**: a mount resolves to itself, so it never reaches the check,
  and an app cannot forge one.
- One app's `env_file` MUST NOT sit inside another app's view. The
  deny-by-default view does not close this on its own, because the OS
  base view is bound into every unit. Config load MUST refuse it,
  comparing canonical spellings as well as configured ones. An
  `env_file` inside its OWN app's dirs is a warning, not an error —
  the app receives those variables regardless; the reason to warn is
  that under `shared/` it can rewrite the file and choose its own next
  launch's environment. Pinned by `TestEnvFileMayNotLandInAnotherAppsView`.
- Nothing in `state.json` decides how much isolation a relaunch gets:
  there is no recorded sandbox disposition, so a relaunch from the
  record is sandboxed like a deploy.
  Pinned by `TestEveryLaunchIsSandboxed`.
- A reattach MUST verify rather than trust: a recorded unit that is
  running without `PrivateUsers=`/`PrivatePIDs=` was not started by
  this moteld (a development build, or another same-uid process) and
  is refused, logged at ERROR, and replaced by a sandboxed relaunch
  through the ordinary sweep (`TestSystemdRunnerReattach`). That
  catches an honest stale unit, not a same-uid process starting units
  on purpose — that process can give a unit any property set and is
  inside the trust domain already.
- A command that resolves outside the view MUST be refused at launch
  with a message that says where a runtime has to live (ship it in
  the release, or install it under `/usr`), not left to fail at exec
  (exit status 127, the sibling shell's `not found` in the journal):
  `exec.LookPath` runs in the supervisor's view, so a
  runtime installed outside `/usr` (`/opt/node/bin/node`, an nvm or
  asdf shim under a home directory) resolves for the supervisor and
  then does not exist inside the unit
  (`TestUnitForRefusesCommandOutsideTheView`). A deploy and every
  relaunch ask the same question first, at the pre-flight — a deploy
  reports the refusal as a 422, a relaunch gives up on it
  (`TestSystemdRunnerPreflight`); the launch's own check stays as the
  last word.
- `/run/systemd/resolve` MUST stay reachable (read-only) when it
  exists: on systemd-resolved hosts `/etc/resolv.conf` is a symlink
  into that directory, and since nothing else under `/run` is in the
  view the link would otherwise dangle — all DNS inside the sandbox
  failing, silently. Local unix sockets (`/run/postgresql`,
  `/run/mysqld`, …) are deliberately not reachable: a same-box
  database is reached over TCP loopback.
- The app environment MUST be scrubbed: an allowlist of inherited
  variables (`PATH`, `LANG`, `TZ`, `LC_*`; `HOME` is set to the app's
  shared dir, never inherited), then `env_file`, then inline `env`,
  then the `SOCKET` contract — never a blanket `os.Environ()`
  inheritance, which would leak ACME tokens (and any other supervisor
  secrets) into every app. The sandbox path MUST NOT regress this.
  `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`, which the manager
  hands every unit, are unset (`UnsetEnvironment=`): both point into a
  `/run/user` that is not in the view.
- Nothing in the app's environment may name a path outside its view.
  `HOME` MUST DEFAULT to a writable in-sandbox path (the app's shared
  dir): inherited it points at `/var/lib/motel`, and every runtime
  that touches `$HOME` (npm, corepack, pip) would ENOENT. It is applied
  before `env_file` and inline `env`, so an operator can still point it
  elsewhere — deliberately, since an app may want its cache on another
  bound path. An override that lands OUTSIDE the view is not refused
  (the operator asked for it, and a config-load check cannot see a view
  that is built per launch) but MUST be reported at launch
  (`homeOutsideView`), or the app fails later with an ENOENT that
  names no cause. `XDG_DATA_HOME` and `XDG_CONFIG_HOME` MUST NOT be
  inherited: leaving them unset makes every runtime derive them from
  `$HOME`, which is already correct. They are not in `envAllowlist`,
  which is what enforces it. Pinned by
  `TestSandboxedEnvNamesNoPathOutsideTheView`.
- The app MUST run inside a new user namespace (`PrivateUsers=yes`,
  set explicitly — the mount options do not imply it): this is the
  load-bearing property for the filesystem rows — see
  DESIGN-threat-model.md, "The shared-UID rule": under a shared UID,
  every mount restriction above is void without a namespace the
  kernel's ptrace check honours, and the user namespace is the one it
  does. The app MUST additionally run inside a new PID namespace
  (`PrivatePIDs=yes`): the supervisor, the user manager and every
  sibling become invisible and unsignalable, and systemd's
  in-namespace stub init reaps orphaned grandchildren. And inside a
  new IPC namespace (`PrivateIPC=yes`): SysV objects, POSIX message
  queues and `/dev/mqueue` are the unit's alone (#311). A unit
  therefore has one `Exec*=` — no `ExecStopPost=` or other — so the
  manager spawns one process per unit (the app's own children join its
  namespaces as ever): on a user manager every process the manager
  spawns gets a user namespace of its own, and a second one cannot
  join the IPC namespace the first made. A manager that cannot deliver
  any of the three is refused at start.
- The unit's cgroup is the lifetime boundary (`KillMode=control-group`):
  nothing outlives the unit. A unit has no controlling terminal.
- Privilege and system-call surface, every unit: `NoNewPrivileges=`,
  `CapabilityBoundingSet=` (empty), `RestrictNamespaces=` (empty set —
  an app cannot create further namespaces), `RestrictRealtime=`,
  `RestrictSUIDSGID=`, `LockPersonality=`, `ProtectKernelTunables=`,
  `ProtectKernelModules=`, `ProtectKernelLogs=`,
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`
  (netlink for `getifaddrs()`, which runtimes call at startup; a
  route, link or address change needs `CAP_NET_ADMIN`, which no unit
  holds, and what a generic-netlink family admits unprivileged is that
  family's own rule), `SystemCallFilter=@system-service` less `io_uring_setup`,
  with `SystemCallErrorNumber=EPERM` — systemd's curated allowlist,
  adopted wholesale, which is the bar the non-goals below set for any
  seccomp filter, with one call taken out — and
  `SystemCallArchitectures=native`. Pinned by
  `TestSandboxPropertiesEveryUnit`.
- `RestrictAddressFamilies=` filters `socket(2)` alone, so the other
  routes to a socket of a family it leaves out MUST be closed too. Two
  are. `@aio`, inside `@system-service`, admits `io_uring_setup`: a
  ring's `IORING_OP_SOCKET` (kernel 5.19+; Debian 13 ships 6.12) opens
  a socket of any family without that call, which would leave `AF_ALG`
  and `AF_VSOCK` one ring away. So the filter takes `io_uring_setup`
  back out (`SystemCallFilter=~io_uring_setup`, after the allow list:
  the first entry sets the filter's mode), and with no ring
  `io_uring_enter` and `io_uring_register` have nothing to act on. And
  on amd64 the kernel also runs the x86-32 ABI, where `socket(2)` is
  reached through `socketcall()` and systemd applies no address-family
  filter at all, so `SystemCallArchitectures=native` takes that ABI
  away. A 32-bit executable would die at its first system call, so the
  pre-flight refuses one naming the setting (`checkMachine`,
  liveswap/elf.go): a deploy as a 422, a relaunch for good. The third
  call that makes a socket, `socketpair(2)`, the filter does not cover
  either, and on Debian 13 it gives an app no socket outside
  `AF_UNIX`: in 6.12 only `AF_UNIX` and `AF_TIPC` implement it (every
  other family's `proto_ops` carries the stub, `EOPNOTSUPP` once the
  family is registered; a family no module answers for is
  `EAFNOSUPPORT`), and `tipc` has no autoload alias, so it loads only
  by root's hand, which docs/hardening.md step 5 (next bullet) refuses
  too where taken. What the call still does is load the module of any
  family that has an alias, with no socket to show for it (next
  bullet, the accepted part). Measured against the source 2026-10-22
  (#315). Denying `socketpair` was never an option: Node's
  `child_process` uses it.
  Pinned by
  `TestTwoRoutesAroundTheAddressFamilyFilterAreClosed` and
  `TestCheckMachineRefusesA32BitExecutable`; the integration, e2e and
  install-test lanes read the built filter and the architecture back
  off a live unit. On a kernel without seccomp the manager skips
  `SystemCallFilter=` and that read-back still passes, so the view
  probe also shows from inside the unit that the filter is in force:
  `Seccomp:` 2, and `io_uring_setup` refused with EPERM where the same
  call, made by the lane as the same user outside the unit, reaches
  the kernel. What it costs the runtimes, checked 2026-10-18:
  Node's libuv sets up a ring for batching `epoll_ctl` at every loop
  start and, refused, uses plain `epoll_ctl` (`uv__iou_init`, libuv
  1.51), and the Node example starts in the e2e lane; Deno's event
  loop is tokio's epoll, and the Deno example starts in the same lane;
  Bun has no lane, and its event loop is usockets' epoll backend
  (`packages/bun-usockets/src/eventing/` holds `epoll_kqueue.c` and,
  for Windows, `libuv.c`), with no `io_uring` call in its runtime
  (oven-sh/bun `ed58ba2`).
- **Module-backed protocols.** The family list says nothing about the
  protocol number, and the kernel loads a protocol's module on the
  first `socket()` that names it, as any user (`inet_create`,
  `net/ipv4/af_inet.c`; the same in `af_inet6.c`, and for netlink in
  `af_netlink.c`). On Debian 13's 6.12 three protocols under `AF_INET`
  and `AF_INET6` carry that alias and gate nothing at creation: `sctp`
  (132), `l2tp_ip`/`l2tp_ip6` (115) and `smc` (256; `modules.alias` of
  6.12.113, read 2026-10-22). A compromised app gets a live socket into
  kernel code the sandbox meant to keep out (#315: S3 × O3 × D5 = 45).
  `dccp` (`SOCK_DCCP`) would be a fourth, but Debian strips its
  autoload aliases (its
  `dccp-disable-auto-loading-as-mitigation-against-local-exploits`
  patch; sibling patches strip `rds`'s, `af_802154`'s and the
  ham-radio families'), so it loads only by root's hand, as `tipc`
  does. motel closes none of this. systemd filters no
  protocol argument, and the package carries no host policy: a
  modprobe.d file is the box's setting, for every program on it, so it
  is the operator's step, not the `.deb`'s (owner's decision,
  2026-10-22, the line the `libatomic1` Recommends was declined on in
  #279; the shipped file was built, reviewed and taken out again).
  docs/hardening.md step 5 is that step: `install <module> /bin/false`
  for `sctp`, `l2tp_ip`, `l2tp_ip6` and `smc`, and for `dccp`, `tipc`
  and `rds`, which refuse the root-loaded case (a loaded `tipc` is
  configurable by any netlink sender, its generic family carrying no
  admin flag, with UDP media built in) and make the seven the set the
  CIS Debian benchmark makes unavailable. The line stops the kernel's
  autoload and a hand `modprobe` alike, a module depending on the
  named one with them (measured: `modprobe dccp_ipv4` stops at
  `dccp`'s line); a module already loaded stays until reboot. So the
  route is **open by default**: on a box without the step a
  compromised app opens those three protocols' sockets. Accepted by
  decision with that score (rows in DESIGN-liveswap.md and
  DESIGN-threat-model.md); no probe checks the step either way.
  Pinned by `TestHardeningStepBlocksTheModuleBackedProtocols`, which
  holds the page's one line to the test's own list, the one copy of
  these seven outside this prose: a change to the list changes the
  test, the page and this bullet together. What stays open on every
  box, in the same accepted row, is the modules an app loads through
  the families it has. A `socket(AF_NETLINK, SOCK_RAW, n)` loads the
  module for netlink protocol `n` (`nfnetlink`, `xfrm_user`,
  `crypto_user`, `ib_core`, `scsi_transport_iscsi` on Debian 13) and
  hands the app a live netlink socket into it, and the generic-netlink
  control family loads any module with a family alias on a
  `GETFAMILY` by name, `smc` among them; `nfnetlink` and `xfrm_user`
  refuse an unprivileged sender at their first check, `crypto_user`
  answers a `GETALG` report (what `/proc/crypto` shows; CVE-2018-19854
  was an info leak there) and `sock_diag` dumps the namespace's
  sockets as `/proc/net` does. And `socketpair()` in any family with
  an alias (`appletalk`, `atm`, `af_key`, `pppox`, `llc2`,
  `mpls_router`, `can`, `bluetooth`, `rxrpc`, `phonet`, `af_alg`,
  `nfc`, `hv_sock`, `kcm`, `qrtr`, `smc` on Debian 13) loads it and
  runs its create path before the stub refuses the call, so the app
  gets no socket from that one. systemd has
  no filter for either; blocking `nfnetlink` breaks nftables. A longer
  list in the hardening step, for the family modules and
  `crypto_user`, is possible and not decided.
- The network namespace MUST be shared: apps make outbound calls and
  reach a same-box database over loopback. The app itself listens on
  a per-instance unix socket in its own `run/<nonce>/` (`SOCKET`); nothing
  motel runs listens on a port, and a sibling's socket is outside
  the view. motel MUST NOT dial that app-writable name: it verifies
  the inode and hard-links it to `<app>/proxy/<nonce>.sock`, a
  directory no view contains, and dials that (DESIGN-threat-model.md,
  "The socket name is app-writable"). `AF_UNIX` MUST stay in
  `RestrictAddressFamilies=` (`TestNetworkNamespaceIsShared`).
- `pre_start` MUST run under the same sandbox as the app it precedes
  (a migration that writes where the app cannot read is a bug caught
  at deploy time, not 3am).
- Stop semantics MUST remain: SIGTERM to the whole cgroup, `grace`
  (`TimeoutStopSec=`), then SIGKILL — with PID-namespace teardown as
  the backstop (killing the in-namespace init kills everything in the
  namespace; no survivor is possible). The existing escalation tests
  define the contract. On systemd 257 the manager holds the grace only
  for a unit with more than its main process in the cgroup (#380), so
  every unit's command runs behind a shell that forks one sleeping
  sibling and execs the app in its place (`withSibling`,
  DESIGN-liveswap.md); the app stays PID 1 of its namespace.
- The view a unit gets is fixed at its start. Nothing changes the view
  today, and this is not a security asymmetry: a secret declared after
  a unit started is absent from every view, old units included,
  because nothing ever bound it.
- The status endpoint reports no sandbox field: there is one sandbox
  and every unit has it, so there is nothing to report. The lanes
  assert the sandbox from inside the unit, which is the only honest
  assertion hook.

## Config surface

There is none. The view is the one listed above — `sandboxBaseView`
plus the app's own release, `shared/` and `run/<nonce>/` dirs — and
nothing in the Caddyfile widens or disables it; a config naming `sandbox` fails to load
(`TestRetiredSandboxConfigIsRejected`). Raw unit directives are
deliberately not exposed — hand-rolled flag soup is the "looks
configured, quietly weaker" failure mode. See "`extra_path` — rules
for when it returns" for the one widening mechanism that has been
designed and deferred.

## Mechanism

`unitFor` (liveswap/runner_systemd.go) builds the transient unit:
`resolveBindSources` resolves and re-checks every bind source at the
last moment before the manager acts (closing the planted-symlink
case), `homeOutsideView` reports an operator `HOME` the view cannot
satisfy, and `unitProperties` (liveswap/systemd_dbus.go) renders the
lifecycle properties — `Restart=no`, `KillMode=control-group`,
`NoNewPrivileges=`, `LimitCORE=0` on both limits — then appends
`sandboxProperties` for the
namespaces, the view and the privilege surface. `unitFor` refuses a
spec without a sandbox.

**The probe.** At `App.Start` — every config activation —
`probeSandboxCapability` starts a throwaway unit with the same property
set whose script reads `/proc/self/uid_map`, its own pid and the
`Seccomp:` line of `/proc/self/status`, and tries `unshare -U true`. It
passes only if the uid range is not the host's, the pid is 1, the mode
is 2 and the unshare is refused with EPERM — both namespaces engaged
and the unit's seccomp set in force. Where seccomp is unavailable to
it (no seccomp in the kernel, `SYSTEMD_SECCOMP=0` in its environment,
or an outer filter that refuses it one), the manager skips every
seccomp-backed setting at once, with a debug line. The mode alone says
neither whose filter is loaded (one the manager itself runs under
reads 2 as well) nor which of the unit's several; the refused unshare
is `RestrictNamespaces=`, skipped exactly when the whole set is. One
case passes both checks: `SystemCallFilter=` alone not loading while
the rest do ("What this does NOT stop").

One attempt, one retry on timeout only (the shape a
capable host under boot load produces; a refused start exits 1, which
moteld.service deliberately never retries, so without it one slow
attempt keeps a capable box down until someone starts it by hand),
30 s each, and a hung attempt is then stopped with its grace and the
runner's stop slack: 190 s bounded with the manager connection's two
dials (connect, then authenticate, 10 s each, for each of the two
sockets `NewConnection` opens) and the two manager probes `Start`
makes first, which the unit's `TimeoutStartSec=240s` is sized above — the
retry must reach its own verdict, not be cut off by a start timeout
whose restart would begin the sequence again. The probe also proves
the base view: `/bin/sh` has to exist inside the unit for the script
to run at all. Its output lands in `journalctl -t motel-sandbox-probe`,
which the refusal message names. The verdict is cached against the
manager connection's generation
(`userManagerClient.cachedSandboxCapability`): later activations on
the same connection read the cache; a redial — a manager restart, or
motel's own — starts a new generation that the next `Start`
measures afresh. A failed verdict is never cached. A unit launched
between a redial and that next `Start` (a watchdog relaunch after the
manager came back) is not preceded by a probe: if the host can no
longer deliver the namespaces the unit fails `226/NAMESPACE` and the
watchdog reports it — never a bare app.

The probe stays a measurement rather than a version check because the
host still gets a vote: a container, an LXC VPS, a kernel built
without user namespaces, or an LSM that refuses them all fail the unit
on a manager whose version says it should have worked. CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on GitHub's Ubuntu
kernel so the runner behaves like a Debian host; no lane exercises a
restricted kernel, and the probe is what stands between such a host
and a silent loss of isolation.

Within a live connection nothing invalidates the verdict — not a
sysctl, not an LSM policy load — and this is deliberate: app units are
`Type=simple`, whose start job completes once the manager has forked —
before namespace setup and before exec — so a namespace failure
(`226/NAMESPACE`, when a kernel or LSM change withdraws the namespaces
under a live connection) arrives asynchronously through the unit
watcher, where no synchronous hook can see it.
`TestIntegrationSystemdSandboxedUnitFailsAfterItsStartJobSucceeds` pins
the systemd semantics so the invalidation is not reinvented.

**Packaging.** `postinstall.sh` enables linger for the motel user so
its manager runs without a session. The `.deb` depends on
`systemd (>= 257)`, the release that added `PrivatePIDs=`, so apt
refuses an older release at install rather than letting it fail when
liveswap first starts. That floor is not the check: the probe above
still decides. There is no profile to install.

## Availability dependency

The sandbox is an availability dependency of the whole server: on a
host that cannot sandbox, a config that defines an app does not load,
so motel does not come up on one at all — admin socket and proxy
included — and a reload to one is refused. Only a config with no app,
such as the starter, skips the probe and serves. There is no graceful
degrade for host incapability (a container, an LXC VPS, a locked-down
kernel), because
the setting that would run apps without one hands a bare app every
sibling's data and motel's keys on the shared uid. The refusal
message names where the probe's output is and says that no setting
runs without one. The accepted cost: unprivileged LXC containers from
budget providers cannot run motel, and a workload the sandbox
cannot host has nowhere to run.

The governing asymmetry an operator should know:

- On a **deploy**, a failed health gate is safe — the new instance is
  discarded and the previous one keeps serving. A view mistake (a DNS
  break, a scrubbed env var an app leaned on) fails here, harmlessly.
- On a **relaunch** — boot recovery, a unit stopped behind motel's
  back, every watchdog restart — there is no previous instance to fall
  back to. The view is the same fixed one every launch of that app
  gets, so a mistake surfaces on the next deploy or relaunch alike;
  what never happens is the view changing under a running app.

Before restarting into a new box or binary, establish that the host
can deliver the sandbox. A reload is the safe place to find out — a
config that cannot activate leaves the running one serving, where a
restart does not. `moteld validate` does not measure the host (it
never starts a unit). The verdict is cached per manager connection, so
a reload measures the host only the first time after motel dialled
the manager; a namespace policy changed underneath a running moteld
(a sysctl, an LSM policy load) is not seen until the next restart.
There is deliberately no uncached preflight: the cache is what keeps a
throwaway unit off Caddy's reload path.

Nothing here is destructive: the release, `shared/` and the
instance's `run/<nonce>/` are bind-mounted at their real paths and everything else is simply absent
from the unit's view — `state.json` included, which the supervisor
still reads and writes as itself. moteld itself is never sandboxed.

## Testing acceptance criteria

**Every promise above is asserted somewhere, and a promise that is not
asserted is not a promise.** Three review rounds on this feature found
the same class of defect — one app reaching another's data — by
inspection, one instance at a time, because the model's guarantees
lived in prose while the tests checked the diff.
`liveswap/sandbox_promises_test.go` carries the ones whose only other
statement was prose, and its header maps every other normative bullet
to the test that pins it. Adding a MUST here means adding its
assertion in the same change.

Three of these lanes read the view from *inside* a unit, and they do
it with one asset: `liveswap/testdata/sandbox-view.sh`, the **view probe**. It
derives every path from its working directory, writes one `k=v` line
per check into the app's shared dir and ends with `done=1`; each lane
sources it from its own `./server` and asserts the subset it can. Not
to be confused with the **capability probe** (`probeSandboxCapability`),
which is production code that runs a throwaway unit at `App.Start` to
decide whether the host can deliver a sandbox at all. A check whose
input a lane cannot supply is emitted as `skipped`, never omitted, so a
lane that silently loses an input fails rather than passes vacuously;
`TestSandboxViewProbeEmitsEveryKey` pins the key set in `make test`.
It was three diverged copies until #72: one of them had stopped
testing that `/etc/ssl/private` stays out of the view.

- Unit: the unit-property builder's table tests (the base view,
  real-path invariants, and above all that the set of bind
  destinations IS the view — nothing named that should not be) against
  the fake D-Bus connection; the host-refusal pins (an incapable host
  fails Start with the probe's reason; a start spec with no sandbox is
  refused by the runner).
- Integration: real systemd in the dev-systemd container — the
  property set is read back from the transient unit, SIGTERM reaches
  the app inside its PID namespace, escalation, no orphans after
  SIGKILL, and the view probe runs with `MGR_PID` supplied so the
  manager's `/proc` is asserted closed from inside. The 2026-08-30
  spike measured the split the shared-UID rule rests on: bare,
  `/proc/<manager-pid>/root` opens; with `PrivateUsers=` alone it is
  denied — the user namespace is the load-bearing piece for the read
  assertions below, `PrivatePIDs=` for visibility and signals. This
  lane's fixture root is a `/var/tmp` temp dir, so the root dir is
  writable here (`PrivateTmp=`) and only e2e and install-test assert
  it read-only.
- e2e: ALL scenarios pass sandboxed (the zero-downtime suite doubles as
  sandbox-compat proof); one scenario deploys a probe app whose view
  must name only its own release, shared and run dirs — the liveswap
  root lists this app and no sibling — with motel's state dir, its
  runtime dir and the user manager's private socket all absent, and
  `/proc` listing only in-namespace PIDs. It does not assert the admin
  socket: this config puts the admin API on TCP so the runner can drive
  `/load`, so nothing creates one and "absent" would prove nothing;
  install-test asserts it against a real socket. The probe app also asserts
  the positive contract: a DNS lookup and an outbound HTTP fetch
  succeed (the resolv.conf trap would otherwise ship silently), `$HOME`
  is writable, and the scrubbed env does NOT contain a seeded
  supervisor secret (a test ACME token). This lane supplies no
  `MGR_PID` — its Caddyfile is baked at image build and the manager's
  pid is a runtime value — so the two `/proc/<manager-pid>` checks
  report `skipped` here and are asserted as such; they are covered from
  *outside* the unit by `denied_to_motel`, and from inside by the
  integration and install-test lanes.
- install-test: the smoke asserts the sandbox from inside the unit on
  Debian 13, against the packaged .deb's real layout: pid 1 and a
  single-id `uid_map` (both namespaces), motel's state, sockets and
  a sibling's files absent, `/etc` only the base-view entries, and a
  runnable OS present so "absent" cannot mean "empty unit". It is the
  lane that proves the manager's `/proc` and the admin socket are
  closed while both are reachable from outside the unit — the vacuity
  guards that make "closed" mean the sandbox closed it.
- soak: full churn; RSS/goroutine/fd deltas quantify the (expected
  ~zero) overhead as a measured claim.
- Every launch is sandboxed: `TestEveryLaunchIsSandboxed` asserts the
  specs the runner receives for a deploy, its `pre_start`, and a
  relaunch from a record (including one with no sandbox field) all
  carry the app's sandbox, and `TestUnitRefusesToRunWithoutASandbox`
  that the runner refuses a spec without one.
- Negative test once during development: widen the view (add a bind
  for `/etc/motel`, or for all of `/etc/ssl` rather than
  `/etc/ssl/certs`) and confirm the isolation assertions fail — in all
  three of the lanes that read the view from inside, which is the point
  of their sharing one probe. Under a deny-by-default view the mutation
  to make is *adding* a name, not removing a mask.
- Mind the failure *signature*: under a deny-by-default view a
  runtime that probes a path nobody named gets **ENOENT** ("no such
  file or directory"), not EACCES or EROFS. It is why a missing
  runtime surfaces as the sibling shell's `not found` (exit status
  127) rather than a permission complaint.
  Any journal grep in packaging/test/smoke.sh that scans for denial
  strings must be scoped to the stage it guards: sandboxed runtimes
  legitimately probe absent paths at startup, and the isolation
  assertions intentionally generate such lines.

## Non-goals

- Resource limits are liveswap's (`memory_max`, `tasks_max`,
  `cpu_quota`; DESIGN-liveswap.md, "Resource caps"). The sandbox's part
  is that `/sys/fs/cgroup` is read-only in the unit, which is what
  keeps a cap the app's own uid owns the cgroup of from being
  rewritten by the app.
- Network egress control — kernel sandboxes cannot scope by hostname.
  The sandbox is the kernel-enforced ceiling; a runtime's own
  `--allow-*` flags are the app narrowing itself inside it, enforced
  in-process and therefore not a substitute — see liveswap/README.md,
  "Runtime permissions". liveswap stays runtime-agnostic: no app
  types, no flag synthesis, no parsing of `command`.
- Per-app UIDs — a later milestone behind a root-owned template or a
  minimal privileged helper; the unit's namespaces deliver the
  file/process isolation without them.
- Hand-rolled seccomp filtering — exactly the "looks configured,
  quietly weaker" trap. systemd's curated `@system-service` set is a
  vetted filter adopted wholesale. Taking a call out of it, as
  `io_uring_setup` above, narrows that set; it does not write a list.
- Protecting an app from itself — its own env and its own database
  are legitimately reachable by definition.
- macOS/Windows sandboxing — servers are Linux; elsewhere the probe
  refuses to start, which is the documented behaviour, not a no-op.

## `extra_path` — rules for when it returns

`extra_path` — a per-app declaration binding a host path into the view
(ro by default, rw when declared) — was designed, reviewed and lifted
out of #40 before merge. It is the only mechanism that *adds* to a
deny-by-default view, it is operator-controlled, and it has to hold
simultaneously against symlinks, TOCTOU, cross-app containment and the
base view; every real defect across six review rounds was in it. It
returns when a running app needs a path outside its own dirs, designed
and reviewed on its own. Until then: persistent data goes in `shared/`;
runtimes go under `/usr` or ship in the release; a same-box database
is reached over TCP loopback.

The rules below were paid for and should not be rediscovered: a bind
must BE the directory it names (equality, not resolve-then-check — a
mount resolves to itself and an app cannot forge one); overlap is
symmetric; the base view may not be named at all, read-only or
writable; a missing source must fail the unit rather than be skipped
(an `IgnoreENOENT` bind does not defer a missing source, it drops it,
and the app serves permanently blind to a path it declared); a
writable `extra_path` aliasing over a second one must be refused (it
let a compromised app repoint the second at any directory on the box
between launches, a sibling's `env_file` included, with every
containment check passing because the planted target was a real path
that simply was not the one the operator named); `{env.*}` expansion
applies like every other path option; and the cross-app `env_file`
check must compare canonical *and* configured spellings, resolving the
deepest existing ancestor for a path whose leaf does not exist yet.

## History

Dated one-liners; the full text of each is in git.

- 2026-08 — Design brief written for bubblewrap as the mechanism (M8):
  spawn path, signal trap, probe-and-fallback ladder, `Depends:
  bubblewrap`. Never shipped.
- 2026-08-30 (#38, `6a080d0`) — The shared-UID rule and the spike
  measurement that a user namespace closes cross-process `/proc` reads
  made systemd's own namespaces the mechanism; bubblewrap dropped, not
  layered.
- 2026-08-31 (#40 branch) — Deny-by-default view replaces
  `ProtectSystem=strict` + a derived `InaccessiblePaths=` set (which
  could be incomplete and aged between deploys); `/etc` named entry by
  entry; bind sources must be the directories they name.
- 2026-09-01 (`3446a53` on the #40 branch; tier code removed in #47
  `ce97f37`) — Debian 13 only: one tier, AppArmor profile and
  user-manager wrapper deleted, CI sets
  `apparmor_restrict_unprivileged_userns=0`.
- 2026-09-02 (#40 branch, `446774e`/`719c898`) — `extra_path` lifted
  out before merge; rules kept above.
- 2026-09-02 (#40 merged, `6e8dfa2`) — Sandbox shipped: user + PID
  namespaces, one tier, `sandbox auto|require|off`, a recorded tier in
  `state.json` with "engage on next deploy, never on a relaunch", and
  a four-step operator rollout ladder.
- 2026-09-03 (#48 `5e36764`) — Capability measured once per manager
  connection, not per config load; failure never cached.
- 2026-09-03 (#49 `20e61c1`) — `auto`/`require` collapsed to `on|off`
  (`auto` ran apps bare with a WARN on an incapable host — the trap
  this document names); #48's cache invalidation deleted as
  unworkable (`Type=simple` semantics, pinned above).
- 2026-09-04 (#51 `24ff8e9`) — `sandbox off`, the recorded tier, the
  status field and the rollout ladder removed; the runner refuses a
  spec without a sandbox; reattach verifies the live unit's
  namespaces. `off` was per-app in syntax only: on a shared uid a bare
  app read every sibling's data and motel's keys.
- 2026-09-06 — App contract moved from `127.0.0.1:$PORT` to a
  per-instance unix socket under a third writable bind, `run/<nonce>/`; the
  socket and the unit share a nonce.
- 2026-10-12 (#225) — The `.deb` depends on `systemd (>= 257)`. Since
  the starter config left liveswap off (#85), the probe first ran at
  the box's first config, so an older release installed and failed
  there. The probe still decides; the floor moves the commonest refusal
  to install.
- 2026-10-18 (#298) — `io_uring_setup` taken out of `@system-service`
  and `SystemCallArchitectures=native` added: an io_uring ring, and on
  amd64 the x86-32 ABI's `socketcall()`, opened sockets of families
  `RestrictAddressFamilies=` leaves out; `socketpair(2)` still can
  (#315). 32-bit executables are refused at the deploy's pre-flight.
  `PrivateIPC=` was measured breaking
  `ExecStopPost=` on systemd 257's user manager and moved to #311.
- 2026-10-22 (#315) — `socketpair(2)` measured to give no socket
  outside `AF_UNIX` on Debian 13: only `AF_TIPC` implements it besides,
  and `tipc` has no autoload alias. The route found instead, a
  module-backed protocol over an admitted family (`sctp`, `l2tp_ip`,
  `l2tp_ip6`, `smc`), is the operator's to close (docs/hardening.md
  step 5, the seven CIS modules; a file shipped by the `.deb` was
  built and declined): open by default, accepted by decision with the
  modules a netlink socket or a `socketpair()` loads.
- 2026-10-23 (#311) — Sibling IPC closed. `PrivateIPC=yes` gives each
  unit its own SysV objects, POSIX message queues and `/dev/mqueue`;
  `TemporaryFileSystem=/dev/shm:mode=1777` gives it its own POSIX
  shared memory and named semaphores, over the host's `/dev/shm` that
  `PrivateDevices=` binds in (systemd's `mount_private_dev`, measured:
  a file written inside a unit was on the host and in the next unit).
  `size=50%` because systemd gives every `TemporaryFileSystem=`
  mount `size=10%,nr_inodes=400k` unless the unit's options say
  otherwise (`NESTED_TMPFS_LIMITS`, namespace.c; measured: 1.2 GiB on
  a 12 GiB host), and 50% is the kernel's own default for the host's
  `/dev/shm`, so each app keeps the room the shared one gave it. A
  `daemon-reload` of the user manager rewrites it `50%%` in the
  unit's record (systemd escapes the `%` on the way out and does not
  unescape it on the way back), which touches nothing running and
  fails, red, a start whose fork lands after a reload of the same
  instant — the same reload lifts `RestrictNamespaces=` in the record,
  so the failure is what keeps such a start from running unrestricted
  (#380; DESIGN-liveswap.md, "Accepted residuals")
  (headless Chromium's known trap is Docker's 64 MiB; 10% of a 2 GiB
  box is 204 MiB). cgroup v2 charges tmpfs pages to the cgroup that
  wrote them (`memory.stat`'s `shmem`, Documentation/admin-guide/
  cgroup-v2.rst), so `memory_max` (#345) is the practical cap; not
  measured here, since the integration container delegates no memory
  controller to its units. The price is one `Exec*=` per unit: on a
  user manager every process the manager spawns gets a user namespace
  of its own, so a second one cannot join the IPC namespace the first
  made (`setns` needs CAP_SYS_ADMIN in its owning user namespace) and
  fails 226/NAMESPACE on its `/dev/mqueue` mount. So the `ExecStopPost=` that
  removed the instance's socket is gone (retiring the instance removes
  the run dir; a confirmed sweep prunes leftovers; nothing routes to
  one). Measured on systemd 257 under root's and an unprivileged user
  manager: the `+` prefix fails the same way (no user namespace, so no
  mount namespace, and `TemporaryFileSystem=` makes that fatal), and
  no systemd through main (~v263) shares a unit's user namespace on an
  unprivileged manager (`UserNamespacePath=`, v259, is refused there).
  The probe now checks that `/dev/mqueue` is owned by its own uid (a
  mqueuefs it mounted itself, in an IPC namespace its user namespace
  owns) and that its IPC namespace is not motel's own, and that a
  file written in its `/dev/shm` is not on the host.
