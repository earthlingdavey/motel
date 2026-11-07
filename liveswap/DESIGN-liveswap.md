# DESIGN — liveswap

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- liveswap/DESIGN-liveswap.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

This is the handover brief: why the module exists, its normative
behavior, the architecture decisions and their traps, and what was
deliberately left out.

## Why this module exists

The predecessor stack ran a single VPS with Nomad as orchestrator, a ~490-line Deno
webhook to download GitHub release tarballs and drive `nomad job run`,
and Caddy configured through a Nomad template that re-rendered
upstreams from the service catalog. It worked, but three systems and a
custom webhook existed to deliver one behavior: *start the new version,
health-check it, move traffic, stop the old one.*

liveswap folds that entire loop into the reverse proxy that was
already there. The webhook, the artifact fetch, the health gate, the
cutover and the process supervision are one Caddy module set. Target
user: indie hackers, solo devs and small businesses deploying from
GitHub/GitLab to one server. Guiding principle: **simple and intuitive,
but powerful and helpful.**

Concept map from the Nomad-era stack:

| Nomad-era piece | liveswap replacement |
|---|---|
| `update { canary = 1, auto_promote }` | deploy pipeline: start → gate → atomic cutover |
| `min_healthy_time = "30s"` | `soak` (default 15s) |
| `auto_revert = true` | failures before promote never move traffic |
| webhook downloads GH release, `nomad job run` | webhook payload carries the artifact URL |
| `nomadService` template + SIGUSR1 | `http.reverse_proxy.upstreams.liveswap` atomic socket swap |
| versioned dirs + keep-6 GC | `releases/<version>/` + `keep` GC |
| prestart migration task | `pre_start` |

## Behavior specification (normative)

- The webhook MUST authenticate every request by verifying an
  `Authorization: Bearer <JWT>` against the app's `deploy_trust`
  sources (OIDC against an issuer's public JWKS, or a local public
  key), and its answer MUST NOT reveal whether an app exists to
  unauthenticated callers (unknown apps verify against the global trust
  sources, then 404; a refused token gets the same 401 either way).
  No verification MUST wait on an issuer (#306): each check is local,
  against the discovery and keys the process holds once per issuer
  and `allow_insecure_http` setting for every source that trusts it,
  fetched in the background — at start and at each reload
  (`deploytrust.Warm`), the held keys in use meanwhile, then for a
  token under a kid none is held for, at most once a minute per held
  state, which that token does not wait on. A token longer than
  `deploytrust.MaxTokenLen` (8 KiB) is refused before any source reads
  it. The time to the 401 can still differ by the local work a name's
  sources do, a fraction of a millisecond per source: the residual
  DESIGN-threat-model.md accepts ("Webhook endpoint"). No shared
  secret is stored on the box.
- Config load MUST fail if any app resolves to zero `deploy_trust`
  sources.
- Config load and every launch MUST warn when an app's `env_file` is
  world-readable or world-writable (the documented mode is 0640
  root:motel; owner and group are not checked), and MUST NOT refuse
  on it: a mode changed between loads is not a reason to fail a
  relaunch. The launch reads the mode from the descriptor it rendered
  the environment through, so it reports the file the unit got. At
  load, an absent file is silent (the launch that needs it fails and
  says so), one the loading account cannot reach is noted at info
  level (`validate` runs unprivileged by design), and any other
  failure to check is warned about as such — never reported as fine.
- Config load MUST warn, once per block, when a `github` or `gitlab`
  `deploy_trust` that some app inherits or names pins an identity but
  no branch (`ref` — on GitLab only beside `ref_type` — `ref_path`,
  `sha`, `workflow_ref`, a `ref_protected` not pinned false, or a
  default-form `subject`), and when a `local` one has no audience.
  Both load — each is the documented minimum — and each is wider than
  it reads: any ref of the identity deploys; a token minted for any
  box trusting the key is accepted. An `environment` as the only
  binding is noted at info level, since it binds only as far as the
  provider's deployment-branch rule for it. A claim that resolved
  empty admits no token and MUST be warned about as that, not as
  width. The generic `oidc` preset is not read: it requires `sub`,
  whose shape is the issuer's.
- The deploy MUST be rejected (409) if one is already running for that
  app; other apps' deploys proceed independently.
- A deploy, push or rollback MUST be refused (500) before it reads or
  writes anything through the app's dirs when the app dir, or a dir
  it makes (`releases/`, `shared/`, `tmp/`, `run/`, `proxy/`), is a
  link, is not a directory, or cannot be checked for a link
  (`writeRefusal`): through `<root>/blog -> shop` it would stage in
  shop's `tmp/` and extract into shop's `releases/`. The refusal's error
  names the path as the app spells it, never what a link resolves to;
  the status the response carries still reads through the link
  (Accepted residuals). An alias on the liveswap root itself is no
  refusal.
- A deploy or rollback naming the version that is running MUST
  relaunch it from its release on disk through the deploy pipeline
  minus fetch and extract (`preparing` runs no `pre_start`): a second
  instance of the installed release under a fresh nonce, soaked,
  promoted, the first drained and stopped — so a changed `env_file`
  value or app definition reaches the app without a new version. A URL
  or push deploy relaunches only for the same artifact: its digest
  (the URL deploy's `sha256` pin; a push's upload, hashed as it is
  staged and recorded as its pin) MUST equal the version's recorded
  pin (`recordedPin`). A request without a digest, a version whose
  record has none (an unpinned pull; a push made before pushes were
  hashed), or a different digest MUST be refused (422) without fetching
  anything to compare, in a message that says how a new value reaches
  the app; a URL deploy whose digest matches MUST still pass the url
  checks a fetch gives it (scheme, `artifact_allowlist`), so a URL
  that could not deploy cannot relaunch either. A rollback to the
  running version needs no digest: it is the same release by
  definition. A relaunch MUST write no deploy
  record — the version's record stays the deploy that put it there,
  with its pin and its times, whether the relaunch succeeded or failed
  — and MUST say what it did in `last_deploy` and the response
  (`note`: `relaunched <version> (same artifact); it reads its env
  file now`). A relaunch never writes under the release: it can only
  run the bytes already there.
- Artifact downloads MUST enforce `max_artifact_size` both via
  Content-Length and the streamed byte count, MUST default to https
  only (enforced on every redirect hop), and MUST match the required
  `artifact_allowlist` — host, optionally with a literal port (no
  wildcards; default-port-only otherwise), a path prefix (mandatory in
  spirit on multi-tenant hosts), and declared query parameter names
  (queries are refused unless every name is vouched for) — first hop
  only. The outgoing URL is constructed from the entry's own config
  bytes; the payload contributes only the canonical-form path suffix
  and vetted query.
- Extraction MUST reject: absolute paths, `..` traversal, symlink and
  hardlink targets resolving outside the archive root, special files
  (devices/FIFOs; setuid, setgid and sticky bits are stripped, not
  refused — `rootWriter.write`), decompressed content beyond 10×
  `max_artifact_size` — as decompressed stream and as the content the
  entries declare, since a sparse entry expands from no stream, and in
  both readings the budget is the archive's, so one entry may
  legitimately be all of it — more than `max_artifact_entries`
  filesystem objects (files, directories and links, implied parents
  included: the byte cap bounds the stream, not the inodes and blocks
  extraction consumes), and names or link targets over PATH_MAX under
  the release directory or with a component over NAME_MAX. Validation
  is a full pre-pass; nothing is written unless every entry is clean.
  A successful deploy reports the entry count and decompressed size
  and warns past 75% of either cap, so the cliff is visible deploys
  ahead. Extraction goes to a staging dir renamed into place on
  success.
- Failed webhook authentications MUST be throttled in the journal:
  per client address (10 failures logged per minute) and process-wide
  (100 failures logged per minute, every line included), so an
  unauthenticated flood has a bounded journal cost from any number of
  sources. The throttle MUST NOT change the answer: every refusal is
  the same flat 401 whatever the budgets say. A valid token MUST still
  be admitted from a throttled address — the throttle bounds lines,
  never deploys. A source the box cannot consult (discovery, key
  fetch) is charged like any failure but MUST be named in
  the journal once per window per source however spent the budgets
  are and whether or not another source then accepted the token, so
  an issuer outage never leaves the journal quiet.
- `pre_start` (if configured) MUST run to completion in the release dir
  before the new instance starts; non-zero exit aborts the deploy. It
  runs under `deadline`, not under the request: a client that hangs up
  MUST NOT stop it midway (a half-applied migration harms the app's
  data); the health gate then fails the deploy on the cancelled
  request, before any traffic moves. Its unit carries `deadline` as
  its own `TimeoutStartSec=`, so it ends on time when no motel is
  watching it. A motel stop or restart MUST NOT stop it midway
  either: the manager runs it on past motel, and every sweep but
  an app's removal leaves a running `pre_start` unit alone and
  reports itself unconfirmed, naming it, so recovery, the watchdog
  and the next deploy start and delete nothing until it has ended
  (recovery retrying on its backoff).
- The new instance MUST be continuously healthy for `soak` before any
  traffic moves (`soak 0`: the first healthy probe; refused at load
  with `health_path off`, where nothing is probed), and MUST be
  stopped and the deploy failed if not healthy within `deadline`. A
  flapping health check resets the soak. The gate is judged once per
  `health_interval`, so it ends within `deadline` plus one
  `health_interval` and two `health_timeout`s; a soak that falls due
  on the first tick past `deadline` still passes if that tick's probe
  does.
- Any failure before the promote step MUST leave the old version
  serving, untouched. The webhook response MUST be synchronous and MUST
  carry a 5xx on failure: as its status (CI turns red; `curl
  --fail-with-body`), or, for a client that sent `Accept:
  application/x-ndjson`, as the last line's `http_status`. That client
  gets the deploy streamed — a line per phase, then the single
  response's body with its code as `http_status` — and the status line
  is 200 once the first line has gone out.
- The cutover MUST NOT trigger a config reload; it is an atomic value
  swap read per-request by the upstream source.
- After cutover the pipeline MUST wait `drain`, then SIGTERM the old
  instance's unit — SIGTERM to its whole cgroup, then SIGKILL after
  `grace`.
- Release GC MUST run only after successful deploys and keep the
  `keep` most recently deployed releases, the one the deploy promoted
  first — so the serving version is never deleted and counts toward
  `keep` — and the one it replaced next, the way back, by what served
  rather than by its record (a rollback whose record write was lost
  leaves that version's record at an older deploy). A release's
  deploy time MUST be one no app can write: its deploy record's write
  time (`deploys/`, outside every view; the version's latest deploy or
  rollback), never the release dir's own mtime, which the app running
  from that dir can set to anything. A release whose latest deploy
  failed (a failed rollback to it) comes after every release whose
  latest deploy succeeded: that deploy never served, and takes no
  place from one that did. A release with no record that proves itself
  one (deployed before records existed, or its record write failed)
  comes after every recorded release, ordered among its kind by its
  dir's mtime, and the status MUST name it in `ordered_by_mtime`: that
  mtime is the app's to set, so it decides nothing among recorded
  releases, and such a release is the first GC candidate after the two
  held by rank, and a rollback target while it is on disk. A version's
  first deploy writes its record as the deploy returns, so from the
  cutover until then the status names the running version in
  `ordered_by_mtime` too. `available_versions` lists the releases in the
  same order, the running version first — it is what the next deploy
  keeps beside the new one — so the rollback targets an operator sees
  are the ones the next GC keeps. GC MUST delete no release, and log
  why, when the app dir or `releases/` is not the directory it names
  (`linkAt`, as for the deploy's refusal above: through a link it
  would delete another app's releases) or the records dir cannot be
  read (refused the same way: the whole order would be the app's to
  set). A release whose
  own record is there but cannot be read (permissions, I/O, not a
  regular file — not the same as none) MUST be held: neither deleted
  nor counted toward `keep`, and named each GC, while the rest are
  pruned by their records. Crash-orphaned `.extract-*` staging dirs
  are removed whenever GC runs, since no order decides them. A staged
  artifact (`tmp/artifact-*.tar.gz`, `tmp/push-*.tar.gz`) a motel
  stopped mid-transfer left is removed at each start, once recovery
  has settled the app and before any deploy of the start could stage
  one — never through a link at the app dir or at `tmp/` (`linkAt`,
  as GC asks of `releases/`), which would reach a sibling's upload:
  its deploy would have removed it as it ended, `tmp/` is in no
  app's view, so nothing else does, and left there it could fail the
  next upload on the disk it holds. A dir
  under `releases/` whose name is not a version sorts after every
  release and holds no `keep` place, so it is removed whenever GC
  prunes. Nothing could deploy it or roll back to it, and the status
  does not list it, so it never takes the place of a version the
  status lists. A failed URL or push deploy's freshly extracted release dir is
  removed (a rollback, or a relaunch of the running version, launches
  an existing release and never deletes it), so its version stays
  retriable — unless the failed instance
  may still be running (the
  dir is kept rather than pulled from under a live process; the next
  launch's sweep stops the stray unit, or, for a `pre_start`, waits
  for it to end) or the removal itself fails;
  either way release GC (after a later successful deploy) removes it
  once it falls outside the newest `keep`.
- A URL or push deploy cut short by motel stopping (a crash, a
  restart, an upgrade) MUST leave its version deployable, and MUST
  NOT leave a release a rollback could launch without its
  `pre_start`. Before it extracts, the deploy writes
  `<app>/deploying.json` (its version and start time; outside every
  view, through `ownfile.go`), and removes it once its record is
  written, whatever it came to; a rollback or a relaunch writes none.
  A marker found by the next recovery or the next deploy, each after
  a confirmed sweep, is a deploy no motel finished: if it names
  the version `state.json` names or the instance kept, that deploy
  reached the cutover, and the marker is only removed; if the
  version's record carries the marker's start time, that deploy was
  recorded and the marker is stale, only removed; otherwise the
  release is removed (refused through a link, `linkAt`), a failed
  record names the restart, and then the marker goes. Settling
  refuses nothing and removes the marker whatever it decides (a
  removal that fails is logged), since the next deploy's marker
  would overwrite it: a release that cannot be
  removed is recorded as a failed deploy's left on disk is
  (`cleanup of failed release`), and a record there that cannot be
  read may be that deploy's, so nothing is removed or written over
  it. A `state.json` that cannot be read vouches for nothing (recovery
  refuses it, so nothing serves from it): the record decides. Through
  an app dir that is not the directory it names (`linkAt`) nothing is
  settled, not even read: the marker and the releases there are a
  sibling's, and the sibling's own deploy may be holding that marker.
  A marker that does not prove itself one (it does not parse, names
  no version or start, or is not a regular file) is acted on for
  nothing and removed where it can be: a non-empty directory at its
  name cannot be, and every extracting deploy then fails writing its
  own marker, naming the path, until it is removed by hand. While a
  running `pre_start` holds launches, the watchdog leaves an
  unhealthy instance serving rather than stop
  it, as while a deploy runs.
- Config reloads MUST NOT restart running apps. Changed app definitions
  apply at the app's next launch: a deploy, a rollback, or a relaunch
  after a crash, a sustained health failure, or a reboot.
- A reload that removes an app MUST leave none of its units running:
  a deploy of it in flight is waited for, then everything the manager
  holds for the app is stopped, and the removed app takes no deploy
  after.
- On Caddy start, each app with recorded state MUST be relaunched (or
  reattached, if the runner supports it) and published as soon as the
  process is up — the health gate is a deploy gate, not a boot gate.
- Every relaunch — a restart's recovery, a watchdog restart, a
  rollback — MUST run the deploy's pre-flight on the spec it starts,
  and a refusal (a release built for another machine; a command not
  there, or outside the sandbox view) MUST end it once, red and
  named, never in a loop: a rollback is refused as a deploy is (422),
  recovery gives up, and the watchdog drops the instance — nothing
  current, nothing routed — and parks. The journal says so once, and
  the status (`watchdog.last_failure`) until a launch replaces it.
- Deploy secrets, `auth_header` values, and artifact URL query strings
  MUST NOT be logged.

## Architecture

Three cooperating modules, flat package `liveswap`:

1. **`liveswap`** (`liveswap.go`) — a `caddy.App`, registered as a
   Caddyfile global option. Owns app definitions, defaults
   (Provision), invariants (Validate), boot recovery (Start), and pool
   reference lifecycle (Cleanup).
2. **`http.handlers.liveswap_webhook`** (`handler.go`) — resolves the
   app module via `ctx.App("liveswap")`; authenticates; `POST /<app>`
   deploys (synchronous), `GET /<app>` reports status. The app name is
   the last path segment so the directive works under any mount path.
3. **`http.reverse_proxy.upstreams.liveswap`** (`upstreams.go`) —
   implements `reverseproxy.UpstreamSource`. `GetUpstreams` reads an
   atomic socket path, returning `unix/<path>`. This was chosen over (a)
   rewriting config via the admin API — a reload per deploy, config
   drift, races with operator edits — and (b) a bespoke proxy handler —
   reimplementing reverse_proxy badly.

### The reload trap (the one Caddy-specific thing you must not break)

Caddy provisions the NEW config before cleaning up the OLD one on every
reload, and module instances are always rebuilt. Anything owned by a
module instance dies on reload. Therefore all live state — the running
process handle, active socket, deploy mutex, last-deploy record — lives
in `managedApp` objects stored in a package-level `caddy.UsagePool`
keyed by app name (`liveswap.go`). Provision takes a pool reference;
Cleanup releases it; the refcount never reaches zero across a reload,
so `Destruct` (which stops the child) only runs at real shutdown or
when an app is removed from the config. The integration suite pins this
with a reload-keeps-PID test; the e2e suite reloads via the admin API
mid-traffic.

Corollary: `App.Stop()` is deliberately a no-op — on reload the old
config's Stop runs *after* the new config serves, and stopping children
there would defeat the pool.

### Concurrency model

- `managedApp.specMu` guards the spec and collaborator wiring
  (re-wired each reload); deploys take a `snapshot()` once at start, so
  a mid-deploy reload never tears the config.
- `deployMu.TryLock` serializes deploys per app; 409 instead of queue.
  `Destruct`, removing the app, takes it with a blocking `Lock` and
  keeps it: a reload stops the old config's servers without waiting
  for their requests, so a deploy of the removed app can still be in
  flight, and past Destruct's sweep it would start a unit nothing
  supervises, routes or stops. The wait is the deploy's remaining
  run. Once the artifact is on the box the app's own settings bound
  it — `deadline` for a `pre_start` and again for the health gate
  (plus one `health_interval` and two `health_timeout`s), `drain`,
  and each stop's `grace` — with the runner's fixed allowances (30 s
  for the start job, 30 s past a stop's grace); a download's or a
  push upload's transfer has no bound of motel's own, only the
  artifact host's or the client's pace and the server's read
  timeouts. The removing reload's cleanup, and so its response, waits
  that long, and says so in the journal when it has to wait. Kept, the
  lock leaves the removed app no further deploy: a request the old
  config still serves gets a 409.
- `mu` guards current instance/phase/last-deploy.
- `activeSocket` is the single atomic the proxy hot path reads: the
  current instance's pinned socket, or nil while nothing serves.
- `ownerLock(app)` (one mutex per app name) makes "stop what nobody
  owns, then prune its socket dirs" one step with respect to recovery:
  the start-time sweep of unconfigured apps holds it across the manager
  sweep and the prune (and a single ownership veto during the sweep
  disables the prune, since ownership is not monotonic), `ensureRunning`
  across the whole of recovery, so a reload adding an app back cannot
  adopt dirs the sweep is removing — and one app's slow recovery holds
  up no other app's.
- The runner's logger sits behind an atomic pointer because the
  per-unit watcher goroutines outlive the config that created them.
- The two seams the start-time sweep reads — `caddyExiting` and
  `appConfigured` — sit behind atomic pointers for the same reason:
  that sweep is deliberately detached, joined by nothing, so a test
  restoring a seam can overlap a sweep still reading it.

### Runner abstraction (`runner.go`)

```go
type runner interface {
	Preflight(spec startSpec) error
	Start(spec startSpec) (handle, error)
	RunOnce(ctx context.Context, spec startSpec) error
	Alive(h handle) bool
	Wait(h handle) <-chan struct{}
	Exit(h handle) string
	Stop(h handle, grace time.Duration) error
	Reattach(st handleState) (handle, bool, error)
	Sweep(app string, keep handle) error
}
```

`Preflight` is the launch's resolution run before any unit exists:
`command[0]` resolves (`resolveInView`, shared with `unitFor`) to a
file inside the sandbox view, and an ELF built for another 64-bit
machine is refused naming the box's architecture and the `runs-on`
to build on, and a 32-bit ELF naming the architecture to build for,
since units allow the native system-call ABI only (`elf.go`, twenty
bytes of header, no dependency); an environment or a command whose
line in the unit's file would pass half of systemd's 1 MiB line limit
is refused too, naming the largest variable, since a `daemon-reload`
could not read such a unit back (`checkUnitLines`, #380). The
deploy runs it for the command and the `pre_start` in phase
`preparing` and reports a refusal as a 422; every relaunch
(`launchVersion`: recovery and the watchdog) runs it for the command
after its sweep, and a refusal there is permanent; `unitFor` still
resolves at launch, as the last word before the manager acts.

`Exit` is how the instance's main process ended ("exit status 3",
"killed by signal 9 (killed)"), "" while it runs, and non-empty
whenever `Alive` is false: a runner records the exit before it
declares the handle dead. A failed deploy's response reads it
(`failureDetail`, app.go), so a runner that cannot observe the exit
must still say so in words rather than return "". `Start` and
`RunOnce` report a unit that ran and failed as a typed `exitError`,
which the same detail reads for the `pre_start`'s and a failed start
job's exit.

The one implementation is `systemdRunner` (`runner_systemd.go`, D-Bus
client in `systemd_dbus.go`): every instance is a transient service
unit under the **motel user's own systemd manager**
(`user@<uid>.service`), reached over that manager's private socket
(`/run/user/<uid>/systemd/private` — no session bus, no polkit, no
root). There is no other runner: an exec runner would need a page of
init-system emulation for what cgroups do natively, and would tie app
lifetime to the binary's.

What the unit gives us: `KillMode=control-group` makes stop and crash
cleanup mean the whole process tree (npm → node → workers), the
journal captures stdout/stderr under `SyslogIdentifier=motel-<app>`,
`Restart=no` keeps the watchdog the sole restart authority, the
command is set as `ExecStartEx` with `no-env-expand` so its argv execs
byte for byte (systemd substitutes no `$VAR` into it, and no shell is
in the way either), `NoNewPrivileges` is on, and `LimitCORE=0` on both limits means a
crash writes no core dump of the app (DESIGN-threat-model.md, "Where
`env_file` values sit"). `handleState.Unit` (persisted in
`state.json`) is what `Reattach` looks up after a motel restart: a
running unit is adopted with the socket its recorded nonce names —
pinned afresh, so a name that no longer holds a socket refuses the
adoption — and a failed one is
logged, reset and relaunched. On process exit `Destruct` leaves units
running for exactly that reason; only removing an app from the config
stops it.

What the unit does not give us by itself, on systemd 257, is the grace
on a stop of a one-process app. Under `PrivatePIDs=` the manager forks
an intermediate that sends it the app's pid and exits; a manager that
reads the app's parent before that exit records the pid as not its
child ("Supervising process … which is not our child" in the
journal), counts it as nothing to wait for, and with nothing else in
the cgroup follows its SIGTERM with SIGKILL at once
(`unit_kill_context_one` returns `!is_alien`; `service_enter_signal`,
v257), on every stop path: a deploy's, a sweep's, an operator's
`systemctl --user stop`, the manager's own shutdown at a reboot.
Measured 2026-10-31 in the dev-systemd lane (257.13): 22 of 36 stops
of a one-process app lost that race, and each of those lost its SIGTERM
handler; with a `sleep` child in the cgroup 11 of 12 lost the race and
none lost the handler (#380). The manager's own shutdown, which is what
a reboot does to the user's units, measured the same day in the same
lane with twelve such apps: `systemctl stop user@0.service` from the
system manager drained 5 of 12 without the sibling (7 lost the race)
and 12 of 12 with it; a `systemctl poweroff` of the container's
systemd drained 6 of 12 without and 12 of 12 with. systemd 258 fixes the fork (commit
`698ac172aa`, `FORK_DETACH`); Debian 13 ships 257. So every unit's
command runs behind `/bin/sh -c 'unset PWD; /usr/bin/sleep 0 || exit
127; /usr/bin/sleep infinity & exec "$@"'` (`withSibling`,
systemd_dbus.go): the shell forks one
sleeping sibling into the cgroup and becomes the app, which is still
PID 1 of its namespace with its own pid, argv and exit status, and the
manager has something to wait for, so it holds `TimeoutStopSec=` as
configured with no motel in the path. To the app the sibling is one
child it did not spawn (the fork survives the exec; under `PrivatePIDs=`
any extra process is the app's child or is reparented to it): it dies
at the stop's SIGTERM with the rest of the cgroup, so the app gets one
SIGCHLD then, as a prefork master does from a worker. It costs one task
(`tasks_max`'s floor is 2 for it) and about a megabyte, and shows in
`systemctl --user status` beside the app. `unset PWD` keeps invariant 8
(dash exports it), and the four names dash resets at its start
whatever it inherited — `IFS`, `OPTIND`, `PPID`, `PWD` — are refused
as `env` and `env_file` keys (`shellOwnedEnvKey`), named; measured,
every other inherited variable passes byte for byte and nothing is
added. Both tools are Essential on Debian and under /usr,
which every view binds; Preflight checks them all the same
(`siblingToolsPresent`), and the script runs `sleep 0` once before the
fork and exits 127 if it cannot, because a shell whose `sleep … &`
failed would otherwise exec the app alone, the race open and the unit
green. The argv the manager reports
is read back with the shell stripped (`appArgv`), so status,
`state.json` and a reattach see the operator's command. A stop
motel drives is still the plain stop job, and `Stop` returns nil
only once the unit is observed gone, as before.

The runner's invariants are written in its doc comment. The ones the
state machine leans on: a handle is declared dead only on an
*observed* terminal unit state (a D-Bus outage never becomes a phantom
crash and a duplicate instance), and an active state is a running unit
whatever became of its file — a `daemon-reload` that cannot read a
unit's transient file back (`error`, `bad-setting`, or `not-found`
once the file is gone) leaves its process running, so the watcher
follows it, `Stop` ends it and a sweep stops it (#380); `Reattach` reports an unreadable unit
as an error, and recovery retries rather than launching beside it;
`Stop` returns nil only once the unit is observed gone; a start or
pre_start whose D-Bus call failed is reconciled by unit name and, if
even that is impossible, reported as "may still be running" so the
deploy keeps the release; and **the manager is the ledger** — every
unit is named `motel-<app>.<version>.<nonce>.service` (the `.`
after the app is the one character an app name cannot contain), and
`Sweep(app, keep)` stops every other unit of the app but a running
`pre_start`, which it leaves to finish and names, unconfirmed.
Recovery sweeps after reattach/relaunch, deploys sweep before a
`pre_start`, a start and GC, removing an app from the config stops
all of it (`StopAll`, sparing nothing), and `App.Start` stops the
units of any app the config no longer names — so nothing motel lost track
of (a failed `state.json` write, an unconfirmed stop, an app renamed
while motel was down) survives the next start or the next deploy
but a running `pre_start`, which nothing is launched beside until it
ends (on its own `deadline` at the latest), and no release is deleted while the manager still runs something out
of it. There is
deliberately no persisted "leaked releases" ledger: systemd already
knows what is running, and asking it is cheaper and truer than
remembering.

Why the user manager and not the system one: a polkit rule granting
`org.freedesktop.systemd1.manage-units` cannot inspect unit
properties, so it would let a compromised motel start a root unit.
Per-app UIDs — the one thing the system manager would buy — are a later
milestone behind a small privileged helper. Isolation between apps
and motel comes from systemd's per-unit sandboxing on every unit
(probe-gated at start: a host without unprivileged user namespaces is
refused, and there is no setting that runs an app without one), see
`DESIGN-sandbox.md` and `DESIGN-threat-model.md`.

Packaging consequences: `libpam-systemd` (pam_systemd hands the user
manager its `XDG_RUNTIME_DIR`) and `dbus` are hard dependencies;
postinstall enables lingering and drops in `Wants=/After=user@<uid>`
on `motel.service`; preremove stops the user manager (and with it
every app) on real removal only. Start fails loudly if the manager is
unreachable — there is deliberately no fallback runner; the check is
in Start rather than Provision so `motel validate` (which
provisions and cleans up without starting) works as any user.

Config commit is transactional in the way Caddy's activation demands:
Provision only takes pool references, Start installs the definition
on the pooled app, and a candidate Caddy rejects after our Start
(another app's Start failing) has its definition rolled back by its
own Cleanup — last-writer semantics, so a config replaced by a
successful reload is untouched. Destruct stops a removed app's units
only when another *started* config is live, and only once a deploy of
the app in flight has ended (Concurrency model); a rejected candidate,
`validate`, and process exit leave them to whoever is or will be
serving them, and wait for nothing. Boot recovery is owned by the config that started it,
retried with backoff for anything not explicitly permanent, and
cancelled/joined by that config's Cleanup.

### Layering

The package is flat, so the compiler cannot report a dependency
pointing the wrong way: an import cycle is unreachable and every file
sees every other. Nothing enforces the order below — it is written
down because #60 found eight edges running backwards, and the only
reason they were found at all was somebody building this map by hand.

| Layer | Files |
|---|---|
| 0 | `clock.go`, `names.go` |
| 1 | `appdirs.go`, `ownfile.go`, `socket.go` (+`_linux`/`_other`), `extract.go`, `elf.go`, `allowlist.go`, `health.go`, `journal.go` |
| 2 | `runner.go`, `sandbox.go`, `download.go`, `state.go` |
| 3 | `runner_systemd.go`, `systemd_dbus.go` |
| 4 | `app.go`, `deploys.go`, `redact.go`, `watchdog.go`, `sweep.go` |
| 5 | `liveswap.go`, `handler.go`, `upstreams.go`, `caddyfile.go`, `trustwarn.go`, `deploytoken_cmd.go` |

A file may use a lower layer or its own. Sideways is fine: a layer is a
concern, and a concern is sometimes split across files — `appdirs.go`
and `socket.go` are one path vocabulary; `runner.go` and `sandbox.go`
hand each other value types.

Six edges run upward on purpose. They are listed here so a seventh has
to justify itself rather than blend in:

| Edge | Why it is right |
|---|---|
| `app.go` → `liveswap.go` (`caddyExiting`, `liveStartedApps`) | process-lifecycle facts live with the module that owns the pool; `Destruct` reads them to tell a shutdown from an app being removed |
| `sweep.go` → `liveswap.go` (`appPool`, `poolKey`, `caddyExiting`) | the pool is the ledger the sweep asks what is still configured |
| `download.go` → `app.go` (`appSpec`, `deployRequest`, `validationError`) | `app.go` holds the `fetcher` interface and `download.go` implements it, so the call points down; only the value types it is handed point back |
| `sandbox.go` → `app.go` (`appSpec`) | `sandboxSpecFor` is a method on `*appSpec` — methods live with the concern, not the receiver |
| `sandbox.go` → `liveswap.go` (`App`) | `validateEnvFileIsolation` is a whole-config `Validate` rule: checking one app's `env_file` against every other app's dirs needs the whole config |
| `systemd_dbus.go` → `liveswap.go` (`managerClient`) | the interface is declared where it is consumed and asserted where it is implemented — Go's normal direction |

A machine-checked version of this table was built and rejected (#75):
resolving "which file declares this name" from the syntax tree alone
takes a guess at every construct that shares a shape with another —
map keys against struct fields, generic instantiations against
collections — and six review rounds each found another. Doing it
properly needs the type checker, which is a real dependency for a
property no operator ever sees. The trigger for revisiting is
backwards edges reappearing *despite* this table.

### File-by-file

| File | Concern |
|---|---|
| `liveswap.go` | App module, config structs + defaults, Validate, pool wiring |
| `names.go` | the app-name and version alphabets, and the two helpers over them; shared by every layer |
| `app.go` | `managedApp` state machine, Deploy pipeline, recovery, env building |
| `appdirs.go` | `appDirs`: the on-disk layout for one app — path arithmetic, and the one check that a dir of it is the directory it names (`linkAt`, by Lstat), which every place motel writes or deletes in an app dir runs. No state, no lock |
| `ownfile.go` | how a file motel keeps in an app dir is written and read — temp file + rename; no link followed at the name, no FIFO held, size-bounded — shared by the deploy records, `state.json` and the deploy marker |
| `caddyfile.go` | all Caddyfile parsing (global option, directive, upstreams) but the `deploy_trust` block, which is `deploytrust.Parse`; NO defaults here — Provision owns them, reading a zero field as unset, so a value below an option's minimum (0 included) is refused here, named; `soak` and `deploy_log_lines`, whose 0 is a setting, are pointers |
| `handler.go` | the webhook: payload validation, deploy dispatch, the status endpoint, and every body's filter (`RespondJSON`); authentication is `deploytrust`'s |
| `redact.go` | the response filter (`Redactor`) that `RespondJSON` applies: known `env_file` values in their encoded forms, a safe list, shape rules, entropy; the four rules every writer of a body holds head the file |
| `deploytrust/` | leaf package: deploy auth — the `deploy_trust` grammar (`Parse`), its sources and verifiers (OIDC + local-key JWT verification), the webhook preamble (`Limiter.Authenticate`) and what a failed attempt costs the journal (`Limiter`, on an injected clock). Imports nothing of liveswap's; the box webhook authenticates through it too. `trusttest/` beside it mints tokens and runs an issuer for tests |
| `internal/dispenser/` | leaf package: the one Caddyfile rule the parsers here and `deploytrust.Parse` apply alike — a repeated subdirective is refused — in one place so the two cannot drift |
| `upstreams.go` | dynamic upstream source (the cutover read side) |
| `runner.go` / `runner_systemd.go` / `systemd_dbus.go` | runner interface + the systemd transient-unit implementation + its D-Bus client |
| `sweep.go` | `App.Start`'s reconciliation against the manager: stop the units, and prune the dirs, of apps no loaded config names. Module-layer (the pool is the ledger), driving the runner |
| `socket.go` | the per-instance socket: nonce, path checks, stale-socket pruning, the unix dialer |
| `sandbox.go` | the per-unit sandbox: view spec, base view, bind-source checks, capability probe (see DESIGN-sandbox.md) |
| `harden/` | leaf package: the supervisor goes non-dumpable at init, before `os` |
| `allowlist.go` | `artifact_allowlist` parsing, canonicalisation and URL pinning |
| `download.go` | capped, redacted artifact download; `releaseFetcher` (download+extract orchestration) |
| `extract.go` | validate-then-extract hardened tar handling |
| `elf.go` | the pre-flight's ELF check: a release's command built for another machine, or 32-bit, is refused before any unit exists. It also declares `preflightError`, the type every pre-flight refusal shares, reported as a 422 |
| `health.go` | prober with soak/deadline arithmetic on an injected clock |
| `journal.go` | the journal tail a failed deploy's response carries: `journalctl` run with argument lists built here, its output bounded |
| `watchdog.go` | continuous crash/health supervision: per-app loop, restart budget, backoff, the wait for a deploy holding the lock |
| `state.go` | `state.json` atomic persistence; the release order (deploy time) that keep-N GC and `available_versions` share |
| `deploying.go` | the deploy marker, `<app>/deploying.json`: a URL or push deploy in flight, so the next start or deploy removes the release of one motel stopped during, and records it failed |
| `deploys.go` | the deploy record store, `<app>/deploys/<version>.json`: written once through the filter, served back only through it, pruned with the releases; its four rules head the file |
| `trustwarn.go` | the load-time pass that asks `deploytrust.WarnUnboundSource` about each `deploy_trust` block once, the global ones only when some app inherits them |
| `deploytoken_cmd.go` | the `motel deploy-keygen` / `deploy-token` subcommands |
| `clock.go` | `Now()`/`Sleep()` clock seam |

## Security posture

- Keyless webhook auth; CI never gets SSH, and no shared secret lives
  on the box. Deploys carry a short-lived JWT verified against public
  material only — CI OIDC (issuer JWKS + a claim allowlist) or a local
  public key. Verification uses vetted libraries (`go-oidc`,
  `go-jose`), never hand-rolled JWT crypto.
- Tar hardening is a pure-Go port of the predecessor webhook's
  `validateArchive`, extended with a decompression-ratio cap and
  setuid stripping, and unit-tested against crafted malicious archives.
- Apps listen on a per-instance unix socket in their own `run/<nonce>/`
  dir; `SOCKET` is injected. Nothing motel runs listens on a port,
  so there is no sibling port to reach. The socket and the unit share
  a nonce, which is what `state.json` records; a record whose nonce is
  not the unit's, or whose socket is not there to dial, is treated as
  nothing recorded. motel never dials the socket by its
  app-writable name: the first connect verifies the inode
  (`O_NOFOLLOW`, `S_IFSOCK` only), hard-links it to
  `<app>/proxy/<nonce>.sock` — outside every view — and proxy and
  prober dial that from then on; retiring an instance unlinks it. So
  bind once per instance. Retiring an instance removes its run dir and
  socket with the pin (the unit has one `Exec*=` and removes nothing
  itself, #311; an instance that dies while motel is down keeps
  its socket until the next recovery or confirmed sweep, and nothing
  routes to it); a confirmed sweep prunes any left behind, but never through a link: if the app dir, `run/` or
  `proxy/` is one (`<root>/blog -> shop`), is not a directory, or
  cannot be checked for a link, the sweep logs it and prunes nothing.
  The health probe is `GET <health_path>` with `Host: localhost`.
- `Authorization` (`auth_header`) follows a redirect only while the
  hop's host is the first hop's or a subdomain of it, and is dropped
  for good at the first hop outside that domain (Go's
  `shouldCopyHeaderOnRedirect`) — exactly right for GitHub's asset→S3
  redirect, which leaves github.com's domain.
- `artifact_allowlist` is required — there is no "deploy from
  anywhere" mode. Host+path entries pin the tenant on shared hosts
  (a bare `github.com` would admit anyone's artifacts), and the
  request host is rebuilt from the config's own string after the
  match, so the attacker-steerable part of a deploy URL is a path
  suffix under an operator-pinned origin.
- Nothing secret is logged: URL query strings are redacted, and the
  deploy token arrives via `Authorization: Bearer` (Caddy-redacted).

## Testing acceptance criteria (all implemented)

Every lane but the first runs against **real systemd** (a privileged
container with the host cgroup namespace); only unit tests use a fake
D-Bus connection. Tests are written from the invariants, adversarially
where the real system can be made to misbehave — a fake encodes the
author's beliefs about systemd, and only the real thing corrects them.

1. Unit (`make test`, race, goleak): the deploy state machine (happy
   path; pre_start abort; health abort keeps old serving; 409;
   same-version relaunch — a matching pin, a push of the same bytes, a
   rollback onto the running version — and its refusals — no digest
   on either side, a different digest — with the version's record
   untouched either way and a failed relaunch leaving the old
   instance serving; GC gated on a confirmed sweep); recovery
   (reattach, relaunch, no-state sweep, transient vs permanent
   classification, retry loop on the fake clock, refusal of a foreign
   unit name in state.json, Cleanup joining recovery); the pre-flight
   on every relaunch path, on the spec it starts, and a refusal ending
   each once; Destruct's
   live-config gate, its wait for a deploy in flight (nothing left
   running whatever the deploy came to, no wait on exit), and rollback
   of a rejected candidate's definition;
   the runner over a scripted manager (unit properties and naming,
   watcher terminal states and transport-error immunity, PID backfill,
   Stop/RunOnce outcomes, reconcile of an ambiguous start, concurrent
   guarded sweeps, unknown-app sweep judged against the live pool);
   watchdog crash/health/throttle/unconfirmed-stop paths, and the
   ledger of a restart meeting a deploy over a dead instance (one
   crash Warn however long the deploy, the instance back on the fake
   clock's same instant after a failed deploy, a nudge ending no
   step), with every lock holder waking the watchdog as it lets go; env
   precedence, placeholders and key validation; malicious-tar table;
   download caps/allowlist/redaction; prober soak/flap/deadline;
   parser and Validate tables.
2. Fuzz (`make fuzz`, weekly in CI; seed corpora run in `make test`):
   artifact extraction, path containment, URL redaction — and, for the
   runner: unit-name round-trip (a name parses back to exactly its app
   and never matches a sibling; anything invalid is refused),
   env_file keys (every key handed to a unit is one systemd accepts),
   and the D-Bus value parsers (no overflowed budgets, no "infinity"
   sentinel from the parsers that feed a budget or a limit —
   `parseManagerLimit` alone keeps it, by design, as the lift target
   for a manager whose `DefaultTasksMax=` is infinity — no panics).
3. Integration (`make test-integration`, caddytest under a real user
   manager): webhook deploy v1 → proxied body; v2 cutover; broken v3 →
   500 and v2 still serving; reload keeps the app PID; keep-N; OIDC,
   push and rollback deploys; watchdog restart and throttle recovery;
   a relaunch of a release built for another machine refused once by
   the watchdog and by recovery, with no unit started; a reload that
   removes an app mid-deploy leaves no unit of it running.
   The runner's own suite: a leader+worker tree is gone after Stop and
   after a leader crash; SIGKILL lands after grace, not before; exit
   statuses as systemd reports them; a second runner adopts a live
   unit and never a failed one; Sweep stops strays; a manager
   SIGSTOPped past a poll timeout is not a crash.
4. e2e (`make e2e`, the packaged unit under systemd as `User=motel`,
   laid out by `packaging/postinstall.sh`): module-linked build guard;
   auth; 5xx-before-first-deploy; **continuous curl loop through the
   v2 cutover with zero non-200s**; broken-v3 containment; 409;
   **admin-API reload under traffic with unchanged app PID**; status;
   watchdog after crash and after health failure; push and rollback.
   Then, inside the container: the app is a unit under `user@<uid>`
   with `Restart=no`; `systemctl restart motel` keeps the same app
   PID (reattach), and so do motel's own deaths — SIGKILL, exit 2,
   an OOM kill — which the unit undoes on its own, while a start
   motel refuses stays down; stopping a version takes its whole
   process tree; a crash leaves no failed unit; a unit gone behind
   motel's back is relaunched on start; removing the app via reload
   stops its units and re-adding it relaunches; an app removed while
   motel was down is swept on the next start; app output is in the
   journal. Finally the recovery suite from the runner's side.
5. Package (`make install-test`, the built `.deb` on debian:13 — the
   support matrix, on both arches): Depends resolution (`libpam-systemd`,
   `dbus`), user manager active with lingering, a deploy under the
   shipped unit as a transient unit with its output and `LimitNOFILE`
   in the journal and `LimitCORE=0` on the unit and inside it,
   `motel validate` as root and as motel leaving
   the running app alone, the app keeping its PID across the upgrade
   restart, and removal taking the apps, drop-ins and linger flag.

## Watchdog

Design summary (full operator docs in README.md "Watchdog"):

- One goroutine per pooled `managedApp` (`watchdog.go`), started at
  first Start, torn down in `Destruct` **before** the child is
  stopped — that ordering is the invariant that makes a mid-restart
  watchdog unable to orphan a freshly started process. Reloads never
  touch the goroutine; it re-snapshots the spec every cycle.
- Triggers: unit exit (the runner's per-unit watcher, exposed as
  `runner.Wait`; a nil channel degrades to `Alive` polling) or
  `watchdog_failures` consecutive failed probes. A health verdict on
  an instance that is dead by restart time is recorded as a crash —
  which signal noticed first is only probe interval vs. unit poll. `watchdog_grace` re-arms after every start; crashes count
  even during grace.
- Restarts take `deployMu` and re-check instance identity — promote
  swaps `current` before stopping the old handle, so a deploy-stopped
  handle never reads as a crash. A restart that finds the lock held
  waits for its holder to let go rather than re-entering supervision
  per backoff step: every holder but the watchdog lets go through
  `unlockDeploy`, which pokes `wdNotify`. A promoted deploy has
  installed its own instance; after any other end the restart goes at
  once. A deploy that ends after the failure was found and leaves the
  instance current (`endDeploy`, counted in `deploysEnded`) also ends
  the backoff step, so a dead instance is back about a second after a
  failed deploy rather than at the end of a step of up to 72 s; a
  throttle, the budget's, still stands, a config nudge ends no step,
  and the step is never refunded (refunding it made a 0.8 s loop,
  #150). While it waits for the lock the state stays `backoff`. The
  relaunch path is `launchVersion`, shared with boot recovery: the
  record pins the **version**, and command, env and sandbox render
  from the spec as it is now, so an app definition edited since the
  deploy reaches the next crash restart — the same rule
  `watchdogDisabled` follows for the same event, and the same exposure
  a reload has always had. The record deliberately holds no launch
  disposition: reproducing a stale instance faithfully would close the
  one way an operator has to fix a crash-looping app without a
  deploy, make "every unit is sandboxed" a property of a file rather
  than of the code, and write env secrets to disk. What makes the
  drift survivable is visibility, not fidelity — status reports the
  argv the manager holds for the running unit (`ExecStart`, read back
  at adopt and at reattach, never persisted), so a definition that has
  drifted from what is running is visible rather than inferred from an
  app that changed behaviour at a restart.
- Pacing is fixed: 1s exponential backoff, 60s cap, ±20% jitter,
  reset only after sustained health. The budget
  (`watchdog_restarts`/`watchdog_window`, shared by crash and health
  triggers) is a rate limiter, not a give-up point — Nomad's
  `mode="delay"`, chosen over hard-fail so an app killed by a
  transient incident recovers unattended once the incident ends. A
  full window throttles (dead instances are unrouted during the wait)
  until the oldest restart slides out; a successful deploy resets the
  budget immediately. The rate bound is also the DoS bound on induced
  health failures. Alerting on a sustained restart loop is deferred
  to the events/metrics milestone.
- Never giving up is for failures a retry can fix. A relaunch its
  pre-flight refuses (the release is for another machine; its command
  is not there or not in the view) is not one: the watchdog drops the
  instance (`dropIf`: nothing current, nothing routed), records the
  refusal as `last_failure`, logs it once and parks, as recovery gives
  up on the same refusal. A deploy, or a reload's recovery once the
  cause is fixed, launches the app again; `state.json` keeps the
  version, so a restart meets the same refusal.
- The watchdog is the **single restart authority**: the systemd
  runner creates every unit with `Restart=no`, or systemd and
  liveswap would fight over the same failure.

## Accepted residuals

Findings scored by the repository's triage rule
(`.claude/skills/triage/SKILL.md`) and accepted; the scores are
severity × occurrence × detection, then the fix's size and risk.

| Failure | Where | S | O | D | Product | Fix | Why accepted |
|---|---|---|---|---|---|---|---|
| A relaunch of the running version starts a second instance of the same release in the same release dir, bound read-write (`sandbox.go`), for the overlap — `soak`, then `drain` and the old instance's stop. An app that keeps mutable state in its release dir rather than under `HOME` (its shared dir) may see its two instances contend there. | `deployLocked`, relaunch | 3 | 5 | 3 | 45 | size 3, risk 2 (a copy per instance, or a stop-then-start branch) | Scores to fix; accepted by decision (#234, 2026-10-16). Both fixes cost what the feature exists to avoid: stop-then-start drops every request for the launch time, which the watchdog's restart already does and a deploy never does; a copy per instance doubles the disk of each relaunch and makes two paths to one release (`{release_dir}`, status, GC). The shared dir is already shared by both versions in every deploy, so an app's state there meets the same overlap today; the release dir is the app's code, and nothing of motel's writes under it during a relaunch. An app that conflicts fails the health gate or crashes in the watchdog's view, red and named; the silent case needs state the app chose to keep where every deploy already discards it. |
| A relaunch matches the request's digest against the version's record, not the release's bytes (`recordedPin`'s known limit: a version release GC pruned, deployed again, whose record write then failed). A request carrying the old digest then relaunches the bytes on disk. | `relaunchRefusal` | 3 | 1 | 3 | 9 | size 3 (a pin kept with the release) | The relaunch introduces no bytes: whatever the match decides, what runs is the release that was already serving. Closing it is the same fix the known limit names for rollbacks. |
| A request cancelled before or during `pre_start` does not stop it: the migration runs to its end, then the deploy starts the new instance and the health gate fails it on the cancelled request (phase `soaking`, `deploy canceled while waiting for health`). The new instance runs its startup for that moment, the release is removed, and a retry runs `pre_start` again, as it does after any gate failure that follows a `pre_start`. | `deployLocked`, after `pre_start` | 3 | 3 | 1 | 9 | size 2, risk 1 (a request check before the start); size 3, risk 3 (run the rest of the deploy detached) | Rule 2 holds (it ends red, the cause named); under rule 3 the product is under 20 and neither fix is size 1, risk 1. The shape is the owner's decision (#295, 2026-10-17): the gate aborts the deploy as it does for any client that left. The old instance serves throughout, and a migration that cannot run twice already meets the same retry after any gate failure. |
| A watchdog restart whose command cannot be looked up at that moment for a reason a retry would fix — its runtime under `/usr` (`/usr/bin/node`) missing while its package is removed and reinstalled, or the lookup failing with an I/O error or a passing `EACCES` (`resolveInView` reports every `exec.LookPath` error as a refusal) — is refused at the pre-flight as permanent: the app stays down, unrouted, until a deploy or a reload launches it. | `handleFailure`, a pre-flight refusal | 4 | 3 | 1 | 12 | size 2, risk 2 (retry a lookup refusal under the budget, a branch on the refusal's kind) | Rule 2 holds (it ends red and named: the journal and status carry the lookup's error); under rule 3 the product is under 20 and the fix is not size 1, risk 1. An upgrade never opens the missing-runtime window (dpkg replaces a file by renaming over it), and restart recovery has always given up on the same refusal. |
| A health-failure restart stops the instance — unhealthy, but serving — before its relaunch's pre-flight runs, so a release made unrunnable meanwhile (a reload's edited `command`, an executable the app rewrote) is refused only once nothing serves: the app is down until a deploy or a reload launches it. | `handleFailure`, the stop before `launchVersion` | 4 | 4 | 1 | 16 | size 3, risk 2 (prepare the launch and run its pre-flight before the stop) | Rule 2 holds (it ends red and named: the refusal in the journal and in `last_failure`); under rule 3 the product is under 20 and the fix is not size 1, risk 1. The order predates the pre-flight on relaunch: before it, the same stop was followed by a relaunch that failed at every step. |
| A reload that removes an app waits for a deploy of it in flight (`Destruct`), and every config change after it waits too: a caller with a deadline of its own (`systemctl reload`, the box applier) can give up first though the reload then completes, and a push upload from a client that stalls without closing holds it until the connection ends, since no bound of motel's covers a transfer. | `Destruct`, the blocking `Lock` | 4 | 3 | 1 | 12 | size 3 (`Destruct` cancels the deploy's transfer; or a time bound on uploads, a new limit) | The shape is the owner's decision (#297): the wait is what keeps a removed app's deploy from starting a unit nothing stops. It is named in the journal; under rule 3 the product is under 20. A bound on uploads could fail a legitimate deploy and wants its own sizing. |
| A recovery that gives up records its reason (`recover`) after `ensureRunning` has let go of `deployMu`: a deploy that succeeds in that moment can be followed by the stale reason in `last_failure`, beside a running instance, until the watchdog's first healthy probe of it or the next launch. | `recover`, `setLastFailure` | 2 | 2 | 3 | 12 | size 2 (record the reason at each permanent return, under the lock) | Under rule 3 the product is under 20. It takes a deploy landing within the microseconds between the two, and status shows the instance running beside the reason. |
| A deploy request the old config was still serving when a reload removed its app, reaching the deploy lock after `Destruct` took it, gets a 409 ("a deploy is already in progress") rather than the answer for an app that is gone. | `Destruct`, the lock kept | 2 | 2 | 3 | 12 | size 2 (a removed flag with its own refusal) | Under rule 3 the product is under 20. Only a request already inside the old server as it shut down can meet it (a slow authentication, an HTTP/2 stream opened before the GOAWAY), and the retry the 409 invites reaches the new config, which answers for the app truthfully. |
| A watchdog restart that waits for a deploy holding the lock keeps the budget slot it claimed before the wait: a wait longer than `watchdog_window` lets the slot slide out before the restart happens, so that restart goes uncounted and a crash loop right after gets one restart more than the budget. | `handleFailure`, the wait for the lock | 2 | 2 | 3 | 12 | size 2 (re-stamp the slot once the lock is taken) | Under rule 3 the product is under 20. It takes a deploy longer than the window (ten minutes by default) over a dead instance, and costs one restart per such deploy. |
| A `pre_start` that exits within milliseconds of `deadline` can still read as timed out: `jobResult` prefers a job result already received when the context ends, but not one whose signal is still in flight. The deploy fails red naming the deadline, and the release of a migration that ran is removed. | `jobResult` | 2 | 3 | 1 | 6 | size 3 (a second completion source: a oneshot's transient unit is unloaded on success, so no `Result` is left to ask for) | Rule 2 holds (red, named); under rule 3 the product is under 20. A migration that ends on its deadline has used its whole budget; raising `deadline` is the remedy. |
| A Debian 13 host that passes the tutorial's step 2 (apt's `systemd (>= 257)` floor, `systemd-detect-virt --container`) but whose kernel or container refuses the sandbox's namespaces installs and serves the starter config, and first fails at step 3: `motel init`'s reload is liveswap's first start. That reload is refused, the probe's error names the cause, and the starter keeps serving; the box's repository, signer and workflow carry over to a fixed or new host. | `probeSandboxCapability`, from `App.Start` | 2 | 5 | 1 | 10 | size 3, risk 1 (a sandbox-check subcommand that step 2 runs as the motel user) | Rule 2 holds (it ends red and named); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#274, 2026-10-17; #225 scored the same case at occurrence 3, but a host is the operator's choice, so it scores as input). The probe needs the motel user and its lingering manager, which the package's postinst creates, so the earliest any check could run is the end of `apt install`, minutes before step 3, with the same refusal and the same fix. A check in postinst could only warn, since a failed postinst leaves the package half-configured: one line in apt's output, after up to two 30-second probe budgets inside dpkg. And liveswap with no apps runs no probe (`len(a.managed) > 0` in `App.Start`), so the starter config cannot carry one. |
| Releases with no deploy record that proves itself one (deployed before records existed, or whose record write failed) sort after every recorded release, ordered among themselves by their dirs' mtimes. A compromised instance running from one of them can set its own dir's mtime, keep its release, and have another unrecorded one pruned first. No recorded release is affected, nor the two GC holds by rank (`TestReleaseOrderLedger`, "the fallback's limit"). | `newestFirst`, state.go | 2 | 2 | 3 | 12 | size 3 (a birth-time key, with its own fallback where a filesystem reports none) | Rule 2 holds. Under rule 3 the product is under 20 and the fix is not size 1, risk 1. What moves is only which of the releases already first in line for pruning goes first, and each is named in the status (`ordered_by_mtime`) and in the GC line (`placed_by`). It needs two unrecorded releases on disk past the held pair plus a compromised app running from one of them. On a box with records since its first deploy that means two failed record writes as well (occurrence 1). Sorting these releases last is the owner's decision (#299, 2026-10-18): it leaves no app-writable input deciding a prune among recorded releases. |
| A deploy that finds no instance in memory (the watchdog dropped one whose relaunch was refused for good, `dropIf`, or recovery has not launched the app) gives GC no replaced version to hold by rank. If the version that last served came in by a rollback whose record write was lost, its record is at an older deploy, and with `keep 2` that GC can prune it. | `deployLocked` → `gcReleases`, `previous` | 3 | 2 | 3 | 18 | size 2, risk 2 (fall back to `state.json`'s last-served version) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1. It takes two independent faults: the lost record write, and no instance in memory at the next deploy. With `keep 3` or more, that version stays among those kept unless newer ones were deployed since. The GC line names what it prunes (`placed_by`). |
| A deploy that takes the lock after a motel start, before the background recovery has recorded the instance, sweeps with nothing to keep, so it stops the recorded instance's unit: recovery then relaunches that version where it would have reattached it. Nothing was routed before the deploy either; recovery cannot reattach while the deploy holds the lock. | `deployLocked`, its sweeps (`old` is nil) | 2 | 2 | 3 | 12 | size 3, risk 2 (the deploy recovers first under the lock it holds, or sweeps keeping `state.json`'s handle) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1. Recovery takes the lock first and holds it through the reattach, so it takes two faults: a restart, and recovery's first attempt failing transiently before it records the instance (the manager does not answer `Reattach`, or the app dirs cannot be made) while the manager answers again by the deploy's sweep. Recovery retries on its own backoff (2 s, doubling to a minute). It predates the deploy marker (#317/#318), whose settle adds a sweep earlier in the deploy, before the version check and the fetch, when a marker is on disk. |
| A marker for the version `state.json` names, or for the instance recovery has just relaunched, is taken as a deploy that reached the cutover, and only cleared. A `state.json` can name a version whose release is gone (removed by hand, a partial restore); recovery then refuses it for good, and the remedy, deploying that version again, writes a marker for it. A motel stop after that deploy extracts and before its `pre_start` succeeds leaves the release kept: recovery waits for a `pre_start` still running, then relaunches the release whether or not it succeeded. | `settleInterrupted`, `ensureRunning`'s relaunch | 3 | 2 | 3 | 18 | size 2, risk 2 (recovery sweeps and settles before it relaunches, a marker newer than the state's `updated_at` being no cutover) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1. It takes two faults: a release missing under the recorded version, and a stop in that window (the redeploy is the remedy, not a fault). The status then shows the version running beside its old successful record. Every other marker case ends with the release removed, proven served, or left on disk with the reason in the journal. |
| A `pre_start` whose start motel could not observe and whose stop it could not confirm (`RunOnce`: the start's D-Bus call failed in transit, then `stopUnobserved` could not reach the manager or see the unit gone) fails its deploy as "may still be running", its release kept, while the manager runs it on. Every sweep but an app's removal leaves a running `pre_start` to finish, so until it ends — at the latest at its unit's `TimeoutStartSec=` (`deadline`, five minutes by default) and stop grace — the app's deploys (after their fetch), rollbacks and relaunches are refused, naming it. The old instance serves throughout and the watchdog leaves an unhealthy one serving; one that crashes meanwhile stays down, the watchdog retrying, until the `pre_start` has ended. | `Sweep` (`spare`), after `RunOnce` | 2 | 2 | 1 | 4 | size 3, risk 2 (remember the unit an unconfirmed `RunOnce` left, and stop it at the next sweep as before #317) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1. It ends red and named: the failed deploy's record names the unit as possibly running, and each refusal names it again. That fix is the stop midway that half-applies a migration, which sparing exists to prevent (#317); the wait is bounded by the unit's own deadline. |
| A non-empty directory at `<app>/deploying.json` fails every deploy that extracts (a URL or push deploy of a new version) until it is removed by hand. The settle reads it as no marker, cannot remove it (`directory not empty`) and goes on; the deploy's own marker write then fails at its rename (`file exists`) before anything is fetched: a 500 naming the path, the version recorded failed, the old instance serving on. Rollbacks, relaunches and recovery go on; each settle logs the directory again, and each deploy, rollback and relaunch runs the settle's sweep first. | `settleInterrupted` → `clearDeployMarker`; `saveDeployMarker` | 4 | 1 | 1 | 4 | size 2, risk 1 (move a marker that is not a regular file aside under a name of its own); size 1, risk 1 (`os.RemoveAll` in `clearDeployMarker`), one-way: it deletes what the directory holds, through a mount under it too | Under rule 3 the product is under 20; the size-1 fix was declined (owner, 2026-10-19): it deletes what motel did not write, whatever the directory holds. `state.json`'s reader deletes nothing, and the temp-file sweep removes only names of its own, and never a directory (the next row). Nothing in motel makes a directory there and no app's view holds the app dir (`sandbox.go`), so it takes root or the motel uid by hand, or a damaged filesystem; a hostile motel uid gains nothing it lacks (T5). The journal names the path at each settle (`is not a regular file`, `directory not empty`). |
| Anything but a directory at the top of an app dir under the name of one of motel's temp files (`.state-*.tmp`, `.deploying-*.tmp`) is removed by the next save of `state.json` or of the deploy marker, and nothing says so: a leftover is known by its name alone. `shared/`, `releases/` and `run/` are not listed. Record pruning does the same to any `*.tmp` in `deploys/`, and logs the file. | `removeOwnLeftovers`; `pruneDeployRecords` | 3 | 1 | 5 | 15 | size 2, risk 1 (only the digits `os.CreateTemp` puts in the name: a detail Go does not document, and `.state-123.tmp` still goes) | Under rule 3's bar, and the fix is not size 1, risk 1. No app can make such a name: inside its sandbox the app dir is a tmpfs of mount points, and it writes only its release, `shared/` and `run/<nonce>/`. So the file came from root or the motel uid, by hand, into a directory that is motel's own and holds no data of the app's: occurrence 1, as the previous row scores a directory put at one of motel's names the same way. A hostile motel uid (T5) can remove the file anyway. The upgrade from a motel without a sandbox, where an app could have made one (Copilot on #371), is one no box will make (#331). |
| A deploy that finds a deploy marker on disk settles it after the relaunch check and before its own version check and fetch (`settleBeforeDeploy`): a sweep keeping the instance in memory, which stops strays, resets leftovers and prunes sockets, and, for a deploy no motel finished, its release removed and its version recorded failed. A request then refused — its version already on disk (422), its URL refused by the allowlist, its download failing — has still made those changes, and a 422 carries the refusal alone. | `deployLocked` → `settleBeforeDeploy` | 2 | 4 | 1 | 8 | size 2, risk 1 (the version check before the settle, except for the marker's own version); size 3 for the fetch's refusals (the old marker held past the new one's write) | Rule 2 holds: the changes are the settle the design gives the next recovery or deploy, made earlier, so the box ends as it would have; the removal and each unit stopped or reset are named in the journal, and the version's record says why. Under rule 3 the product is under 20 and neither fix is size 1, risk 1. The settle precedes the version check because a retry of the interrupted version is what it makes deployable (#318). One restart is enough: one during a `pre_start` leaves the marker for a deploy that lands between the migration's end and recovery's next attempt (its backoff reaches a minute). With no instance in memory the sweep is the one the row on a deploy before recovery describes. |
| `max_token_ttl {env.NAME}` in a `deploy_trust local` block is refused at config load (`invalid max_token_ttl "{env.NAME}"`), though the README says `{env.*}` placeholders are resolved at config load: the duration is parsed when the Caddyfile is, before placeholders are. | `deploytrust.Parse` | 2 | 5 | 1 | 10 | size 2, risk 2 (a string field, resolved in `ResolvePlaceholders` and parsed in `Build`) | Rule 2 holds (refused and named, before anything runs); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#296, 2026-10-17). Every liveswap duration directive (`health_interval`, `deadline`, …) parses the same way, so this one matches them; the Caddyfile's own `{$NAME}`, substituted when the file is read, works for all of them. |
| An app dir, or a dir it makes, that is a link to a sibling's (`<root>/blog -> shop`, `blog/proxy -> ../shop/proxy`) is still followed outside a deploy. blog's status lists shop's releases as its own available versions, and so does the status a refused deploy's response carries. blog's recovery reads shop's `state.json`; it and the watchdog's relaunch make and remove a run dir in shop's `run/` and are refused at launch for a linked app dir, `releases/`, `shared/` or `run/`, recovery retrying, backing off to once a minute. A linked `proxy/` launches: blog's pins land in shop's `proxy/`, where shop's sweep removes them, so blog, not shop, goes unreachable. | `status`, `ensureRunning`, `launchVersion` | 3 | 5 | 3 | 45 | size 2, risk 1 (`writeRefusal` in status and before a relaunch, #300) | Scores to fix (rule 3); held by decision (#300, 2026-10-18: the deploy side is fixed, the rest held) until a box runs a link under the root, or renaming or aliasing an app becomes a feature. No box does. Nothing here writes into the sibling's releases or data: a deploy, push or rollback through such a dir is refused before it writes (`writeRefusal`), so a rollback to a listed version is refused and named; a relaunch through a linked app dir, `releases/`, `shared/` or `run/` is refused at launch (`resolveBindSources`), red and named; and the socket prune never follows a link (`linkAt`), so no sibling loses its socket and pin that way. |
| A push refused before its upload is staged (a dir that is not the directory it names, like a 413 or a staging failure) leaves `last_deploy` as it was, where a URL deploy or rollback refused the same way replaces it. | `deployPush` | 2 | 5 | 1 | 10 | size 2, risk 2 (a push refusal through the pipeline's bookkeeping) | Under 20, and not a size-1, risk-1 fix (rule 3). The push's own response is the refusal, a 500 naming the path, the reason and the remedy, and the journal says it with who asked (`deploy failed`, `via`, `remote`). Every refusal of a push before staging already leaves `last_deploy` alone, so a status read after a push tells the same story for each. |
| An issuer's discovery that failed at start (motel up before DNS or the route), or its key fetch after a discovery that succeeded, at start or a reload, is tried again by a token at most once a minute from when it began (nothing retries it in the background): deploys are refused until then even once the network is back, and the token that begins the retry is refused too, with the old error; a retry once that discovery and the key fetch after it have ended goes through. | `issuerState.ready`, deploytrust | 2 | 3 | 1 | 6 | size 2, risk 2 (a faster retry after a failure, a second timing) | Rule 2 holds (red and named: the journal's `could not consult` line carries the error); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#306, 2026-10-20). The minute is the bound on how often traffic makes the box ask an issuer anything; a reload retries at once. |
| A deploy that arrives just after motel starts (an upgrade, a restart), or just after a reload that adds an issuer the process holds nothing for, is refused while that issuer's first discovery and key fetch run (`under way`, `being fetched for`), and the templates' one retry 3s later is refused too when those two round trips take longer; a re-run goes through. A reload refuses no token for an issuer already held: the keys held stay in use. | `issuerState.ready`, deploytrust | 2 | 4 (start); 5 (an added issuer, operator input) | 1 | 8; 10 | size 2, risk 2 (wait on the first discovery, bounded) | Rule 2 holds (red and named in `refused`); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#306, 2026-10-20). Waiting on the first discovery is the timing the change removes; the window is once per process start, and the retry covers a fetch under 3s. |
| A refused token's time can still name a configured app by the local work its sources do: each source a name resolves to parses the token and checks its signature against a key it holds under the token's kid (or each key it holds, for a token naming none), whatever issuer and audience the token claims, where a name with no source does neither. Bounded by `MaxTokenLen`: an OIDC source refuses a 2 KiB token in 0.11 ms and an 8 KiB one under 0.33 ms (arm64, measured), per source. No source waits on an issuer. | `authorize`, deploytrust | 3 | 5 | 5 | 75 | size 3, risk 3 (a refusal floor held to the slowest name, with caps on sources and keys tried; built and reviewed in #337, then removed) | Scores to fix; accepted by the owner's decision (#306, 2026-10-20). Six review rounds of a floor each found another way around it (token size, source count, key count, cancellation, load, logging) and each fix narrowed which tokens verify; the asset is whether an app name exists, which its domain or repository often says anyway, and a sub-millisecond difference per source needs many samples through network jitter. The 8 KiB token cap stays: without it a 1 MiB token made the difference 16 ms. |
| An issuer that replaced a key under the same kid is picked up by `systemctl reload motel`, as the refusal says, only when that reload's discovery succeeds: a reload whose discovery fails keeps the held key and fetches nothing, silently (the held state answers; the discovery error is not surfaced), and tokens under the new key stay refused, still naming the reload. | `issuerState.discover`, deploytrust | 3 | 2 | 3 | 18 | size 2, risk 1 (refetch the held set's keys when a reload's discovery fails) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#337, 2026-10-20). It takes two faults: an issuer swapping a key under its kid, which issuers do not do, and the reload's discovery failing; a second reload once the issuer answers fetches. |
| A reload whose discovery names a moved jwks_uri while a token-begun key fetch on the old set is in flight leaves that fetch's answer in the old set, which nothing reads; a reload while such a fetch is in flight on the same set forces nothing and takes that fetch's answer. A new kid verifies once the new set's own fetch succeeds; if that fails, the token is refused with the fetch's error, named (the row on failed fetches above). | `issuerState.discover`, deploytrust | 2 | 2 | 1 | 4 | size 2, risk 2 (carry the in-flight fetch over, or force a second) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#337, 2026-10-20): two faults — an issuer moving its jwks_uri, and a fetch in flight at that reload — and it ends red and named. |
| A key rotation within a minute of the issuer's last key fetch (a reload, a deploy under another new kid, a token from another issuer — the signature is checked first — or a forged token naming a new kid, began it) is refused for every token until a minute after that fetch, the templates' 3s retry included: that run goes red, and a re-run from then goes through. A forged token once a minute can make a rotation refused for up to a minute after the issuer publishes its key (each such fetch picks up what the issuer then publishes). | `keySet.refresh`, deploytrust | 2 | 5 | 1 | 10 | size 2, risk 2 (let a retry skip the interval) | Rule 2 holds: red and named, the refusal saying the keys were fetched under a minute ago, from when a token fetches them again, and that `systemctl reload motel` fetches them now (#306, 2026-10-20). Under rule 3 the product is under 20 and the fix is not size 1, risk 1. Skipping the interval for any token would hand the fetch rate to whoever sends tokens. |
| On a box without docs/hardening.md step 5, an app unit's `socket()` under `AF_INET`/`AF_INET6` with protocol 132, 115 or 256 loads `sctp`, `l2tp_ip`/`l2tp_ip6` or `smc` (Debian 13's `modules.alias` carries exactly those; none gates creation on a capability) and gets a live socket into kernel code the sandbox meant to keep out. On every box, a `socket(AF_NETLINK, SOCK_RAW, n)` loads the kernel module for netlink protocol `n` (`nfnetlink`, `xfrm_user`, `crypto_user`, `ib_core`, `scsi_transport_iscsi`), a generic-netlink `GETFAMILY` by name loads any module with a family alias, and a `socketpair()` loads any aliased family's module and runs its create path before the call fails: a compromised app reaches those modules' init and request parsers, and the few requests they answer unprivileged (`crypto_user`'s `GETALG` report, `sock_diag`'s socket dumps): a live netlink socket from the first two routes, nothing from the `socketpair()`. | `sandboxProperties` (`RestrictAddressFamilies=`); the kernel's `inet_create`, `__sock_create`, `netlink_create`, `genl_rcv_msg` | 3 | 3 | 5 | 45 | the seven `install <module> /bin/false` lines of docs/hardening.md step 5, the operator's (size 1 as text; the same file shipped by the `.deb` was built, reviewed and declined 2026-10-22: a modprobe.d file is the whole box's policy, as the `libatomic1` Recommends was in #279); none in systemd for the no-socket loads; blocking `nfnetlink` breaks nftables; dropping `AF_NETLINK` breaks `getifaddrs()` in Node and Go | Scores to fix (rule 2); accepted by the owner's decision (#315, 2026-10-22) as the operator's step, open by default. `TestHardeningStepBlocksTheModuleBackedProtocols` holds the page's one line to the test's own list, the one copy of the seven outside DESIGN-sandbox.md's prose ("Module-backed protocols"). |
| A handle whose caps were marked unknown (a `SetCaps` that failed with the unit unreadable after it) stays unknown through a later `SetCaps` that succeeds but writes no `TasksMax=` — the manager's `DefaultTasksMax` unreadable since the dial, and a tasks cap of the unit's to lift. Status then shows no `caps` though the memory and CPU caps written are in force, and every sync re-sends them, until a relaunch or a restart of motel reads the unit afresh. | `SetCaps`, `capsUnknown` | 3 | 2 | 3 | 18 | size 3 (an uncertainty per cap, or a read of the unit after every write) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#345, 2026-10-23): two faults — a dial whose `DefaultTasksMax` read failed, then a transport failure across the write and its read-back — and the next deploy or restart settles it. |
| A `SetCaps` whose write the manager applied before the transport failed is logged by `syncCaps` as not applied ("they apply at its next launch"), while the read-back that followed found the caps applied and status shows them in effect. | `syncCaps`, `SetCaps` | 2 | 3 | 1 | 6 | size 2, risk 1 (`SetCaps` returns no error when the read-back holds what it sent) | Rule 2 holds: it ends correct, the caps in force and status saying so; under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#345, 2026-10-23). |
| `configure` runs `syncCaps` on the reload's own path, so a reload while the user manager has stopped answering waits on each capped app's write and read-back (two `pollTimeout`s, 20 s) before Caddy moves to the next app; the caps apply at the next launch regardless, and the journal names the wait per app. | `configure`, `syncCaps` | 2 | 3 | 1 | 6 | size 2, risk 2 (run the sync from the watchdog's goroutine, which `configure` already wakes) | Rule 2 holds: red and named, and the reload ends correct. Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#345, 2026-10-23). A manager that has stopped answering is the incident already: deploys and the watchdog wait on it the same way. |
| A unit whose `cgroup.controllers` motel can never read (its cgroup path absent from motel's view of the tree: a cgroup namespace between the two, or a path the manager reports that does not exist) on a host whose user manager does not delegate a controller reports its caps under `caps`, as the manager holds them, with no `caps_unenforced` and no journal line; the watcher re-reads at each poll and never succeeds. | `setCapsState`, `readControllers` | 3 | 1 | 5 | 15 | size 3 (a third state in status, "enforcement unknown", on the wire) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#345, 2026-10-23). Two host conditions outside the support matrix at once; on Debian 13 the path is readable and every controller is delegated (measured). |
| The manager's `DefaultTasksMax` is read once, with the dial. A read that fails while the connection stays up leaves it unreadable for the connection's life: every reattach and read-back then takes the manager's resolved default for an operator's cap (`unitCaps`), status shows `tasks_max` at that number for an app with none configured, and each sync re-sends the other caps with the "could not be read" Warn, until a redial. | `get`, `DefaultTasksMax`, `unitCaps` | 2 | 3 | 3 | 18 | size 2, risk 2 (a bounded re-read of the property when the cached value is 0; go-systemd's property get takes no context, so the bound is the dial's) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (#345, 2026-10-23). The number shown is the unit's real bound; the kernel is unaffected; a restart of motel or a dropped connection reads it again. |
| An app whose SIGTERM path breaks on a child it did not spawn — a handler that treats any SIGCHLD as a worker's death, or a drain that aborts on a pid it does not know — loses its grace: the sibling (`withSibling`) is the app's child and dies at the stop's SIGTERM, so the app sees one SIGCHLD then. (A drain that reaps until `ECHILD` does not hang: the sibling dies in the same instant as the SIGTERM reaches the app, and is reaped like a worker.) | `withSibling`, systemd_dbus.go | 5 | 2 | 3 | 30 | none that keeps the grace on 257: under `PrivatePIDs=` any extra process in the unit is the app's child or is reparented to it, so no sibling avoids the SIGCHLD; the alternative is the one-process race of #380 on every stop | The app is PID 1 of its namespace already and has to reap what is reparented to it; the common masters (gunicorn, php-fpm, puma, libuv, Go, the JVM) wait on known pids or ignore an unknown one, so the failure needs an app that aborts on one (#380 review, 2026-10-31). The remedy is systemd 258 (`698ac172aa`), where the sibling can go. Stated in the README beside the sibling. |
| A `daemon-reload` or `daemon-reexec` of the user manager — which Debian's systemd postinst sends every user manager (`SIGRTMIN+25`) on each systemd upgrade — re-reads every running unit's transient file, and two settings do not survive the round trip: `RestrictNamespaces=yes` is written as an empty set and reads back as no restriction, and `TemporaryFileSystem=/dev/shm:…,size=50%` is written `50%%` and read back verbatim. Nothing running changes: an app unit execs once, before the reload. A start whose fork lands after a reload of the same instant runs with the re-read settings, and fails `226/NAMESPACE` on the invalid tmpfs option — red, named by the watchdog or the health gate — before anything runs with the namespace restriction lifted; its next start is a fresh unit. | the manager's transient-unit serialisation (`dbus-execute.c`, v257) | 2 | 2 | 1 | 4 | none in motel: both are the manager's. Dropping `size=50%` (tmpfs's own default) would make the round trip clean, and leave a start racing a reload running unrestricted instead of failing red | Under rule 3 the product is under 20 (#380, 2026-10-31): a reload and a start within the same instant. Measured in the dev-systemd lane (257.13): the properties before and after a reload, the transient file's lines, and under continuous reloads 6 of 40 starts not reaching ready with the namespace probe never succeeding. The option stays for this reason too (DESIGN-sandbox.md, "Sibling IPC closed"). |
| An `env_file` value that is a URL followed by prose holding a `:` and then an `@` before any `/` or `?` (`smtp://mail.example.com:587 contact: ops@example.com`) reads a password that is none (`587 contact: ops`), and every response masks that string under the key's name. | `urlPasswords` (redact.go) | 2 | 5 | 3 | 30 | size 1, risk 1 — but a bound at a space un-reads a password that holds one, which libpq takes as it stands, and that is a secret the log would carry (3 × 5 × 5) | The two cannot both hold; the mask over nothing is named for its key, and the value's shape is the operator's to see there. |
| An admin API on a socket-activated listener (`admin fd/3`, `fdgram/3`) is refused by `motel validate` and `reload`, and said at `run`, as a TCP one is, though the descriptor systemd passed may be a unix socket: the gate reads the config alone and cannot see what the descriptor is. | `checkAdmin` (cmd/motel/backupcheck.go) | 2 | 5 | 1 | 10 | size 2, risk 2 (taking `fd/` on trust is fail-open for a TCP descriptor; asking the manager what it passed is a new state) | Red and named, with the remedy in the message (`admin unix//run/motel/admin.sock`, or `admin off`); the package ships no socket unit, and the starter's socket needs none. |
| Layer 4 of the response filter misses a short generated token: its bar follows the token's length (`looksGenerated`, `log2(len) − 0.3` bits a character up to 4.5), which masks about 55% of random 20- and 22-character url-safe tokens, 41% at 24, 29% at 26 and all that the fixed bar masked above; the rest of a runtime secret an app prints bare (`secrets.token_urlsafe(16)`'s 22 characters, half the time) reaches the CI log. | `looksGenerated`, `maskGenerated` (redact.go) | 3 | 5 | 5 | 75 | size 1, risk 2: a bar of `0.9·log2(len)`, as first proposed, masks 80% of each short length but also nine of the 148 mixed-case tokens in this repository's own text — identifiers with a digit (`loadEd25519PrivateKey`), unit settings (`RandomizedDelaySec=40s`) — in the failed-deploy bodies the filter exists to keep readable; the bar taken masks one (an e-mail's local part). Measured 2026-11-07 | The known values are not at stake: `env_file` values are replaced whole, and now before the caps cut (`failureDetail`), so no fragment of one leaks. What remains is a secret the app fetched at runtime and printed bare, under 26 characters, on a failed deploy; `deploy_log_lines 0` keeps the app's output on the box, as the README says for a public repository. An assignment is judged by its parts, so `KEY=<secret>` keeps its key and masks its value whatever the key's letters add. |
| A read bound — the journal tail's 16 KiB, the probe body's 4 KiB — that falls inside an `env_file` value holding whitespace leaves the part of it before (journal) or after (probe) the nearest space as a fragment the filter cannot match, when the replacement of other values shrinks the text enough for the kept cap to reach it. `dropUnfinished` removes the unfinished token up to the nearest whitespace, which is the whole of a value without one. | `dropUnfinished` (journal.go), `probeOnce` (health.go) | 3 | 1 | 5 | 15 | size 2, risk 2: cutting at a line instead of a token drops a diagnosis's first or last line for a case that needs a value with a space, printed at the bound, behind several kilobytes of other known values | Three things must hold at once: a value with whitespace in it (a URL password or a sentence, not a token), printed so that the read bound falls inside it, and enough other known values before (probe) or after (journal) it for their markers to pull the fragment under the kept cap. The fragment is at most one space-delimited piece of the value. `deploy_log_lines 0` keeps the app's output on the box. |

**Owed** (over the bar, not yet fixed):

| Finding | S | O | D | Where the fix lands |
|---|---|---|---|---|
| A bind mount can give two apps one directory, or put one app's data inside another's: an fstab source another app also uses, or a whole app dir mounted at another's. A mount resolves to itself, so the launch check (`resolveBindSources`) passes it: blog launches on shop's data, writable, and nothing says so. | 5 | 5 | 5 | #308, to design: equal `(st_dev, st_ino)` alone misses a source nested in another's; when to check, what to refuse and what to say are open there. Until then, the README's bind-mount advice says the source must be the app's own |

## Resource caps

Per app, `memory_max`, `tasks_max` and `cpu_quota` (liveswap.go
`AppConfig`) become the unit's `MemoryMax=` (with `MemorySwapMax=0`),
`TasksMax=` and `CPUQuotaPerSecUSec=` on every unit of the app — the
instance and the pre_start — rendered by `capProperties`
(systemd_dbus.go) beside the sandbox set. A unit with none set gets
none of the properties, so the manager's defaults stand unchanged. A
running instance gets a reload's values through `SetUnitProperties`
(runtime) rather than at its next launch: `syncCaps` (app.go) runs
with every install of a spec (`configure`, `rollbackConfig`), after a
reattach (ensureRunning) and at every publication of an instance
(`publishInstance`: a deploy launches from the spec it snapshotted,
and a reload during its fetch or soak would otherwise reach only the
instance being replaced), compares the spec's caps
with the handle's — what the manager last applied, or read back at
reattach (`propCaps`) — and sends every cap when they differ: an
unset memory or CPU cap as infinity, their default, and an unset
tasks cap as the manager's `DefaultTasksMax` (read once per
connection), because an unset `TasksMax=` means that default, not
infinity — infinity would lift the manager's own 15% bound from the
unit, unless infinity is what the manager was configured with, in
which case it is the default and the lift goes there. A unit the
handle knows carries no tasks cap gets no `TasksMax=` written at all:
there is nothing to lift. A lift writes the default's number into the
unit, and on 257 that leaves a de-capped unit where an untouched one
already is (measured below: a change of `DefaultTasksMax=` followed
by `daemon-reload` or `daemon-reexec` moves the manager's report of
an untouched unit's `TasksMax` and not its `pids.max`), until its next
launch. With the handle's caps unknown the lift is written whatever
the unit may carry. The same number is the reason a reattach cannot take the
manager's word for a tasks cap as it stands: the manager reports an
unset `TasksMax=` resolved to its default (a number, never
"infinity"), so a reattach reads a `TasksMax` equal to
`DefaultTasksMax` as unset (`unitCaps`). With the default unreadable
a lift leaves the unit's tasks cap alone and the handle keeps
reporting it. Status reports the handle's (`caps`). `memory_max` has
a floor of 1 MB and `cpu_quota` a ceiling of 100000% (`minMemoryMax`,
`maxCPUQuota`, caddyfile.go): below the one the unit cannot exec,
above the other is no box, so the line is a mistyped one, refused by
name rather than accepted as no cap at all.

Measured 2026-10-22 on multipass VMs (2 vCPU, 2 GB, no swap): Debian
13 (systemd 257), Ubuntu 24.04 (255), Ubuntu 26.04 (259), with the
caps set on a sandboxed transient unit of a lingering user's manager.
What the design rests on:

- `user@.service` delegates `cpu memory pids` on all three, and on
  Fedora 43 (258): the three caps are the whole set a user manager
  can hold; the kernel reads back each value given, and a write from
  inside the unit is EROFS (`ProtectControlGroups=`).
- `set-property --runtime` on the running unit changed `memory.max`,
  `pids.max` and `cpu.max` with the same pid, and cleared them to
  `max` again — the live path.
- A `MemoryMax=` hit ends as `Result=oom-kill`, `ActiveState=failed`
  (`OOMPolicy=stop` is the default on all three), which the runner
  already reads as a crash; `memory.oom.group` is 0, so the kernel
  kills the one process, and the stop policy takes the unit.
- `TasksMax=40` against a fork loop: a `sh -c` command died with
  status 2 at the first refused fork; Python kept running with 39
  children and 61 refusals. Idle threads: Node 7 (11 under ten file
  reads), Deno 8.
- Without caps: a fork bomb is bounded at `DefaultTasksMax=15%` of
  threads-max (2273 at 2 GB) under a user slice at 33%; at the bound
  the system manager answered in 0 s, a new user unit started in 0–2 s,
  `stop` took 2–5 s. A leak was killed by the global OOM killer after
  35–49 s, the leaker first (`oom_score_adj` 200 against motel's 0:
  systemd's defaults for a user manager's units and a system service,
  not motel's setting),
  sibling and managers untouched; the box's responsiveness during
  those seconds was not measured, and a box with swap would thrash
  longer.
- The real victim is a sibling: under the bomb a second app's requests
  hit a 10 s timeout on Debian 13 and Ubuntu 24.04 (1.3 s on 26.04)
  while a server in `system.slice` kept a 5–9 ms median (the root
  splits CPU 50/50 between `system.slice` and `user.slice`).
  `CPUQuota=50%` or `CPUWeight=10` on the offender brought the sibling
  back under 200 ms.
- `systemctl set-property user-<uid>.slice …` applies live and persists
  on all three; a drop-in plus `daemon-reload` does not re-apply on
  257 (the manager shows the new value, the kernel keeps the old) and
  does on 255 and 259. Docs say set-property.
- Ubuntu 24.04 and 26.04 refuse the sandboxed unit outright under their
  default `apparmor_restrict_unprivileged_userns=1` (exit 218); with
  it 0 every number above matched Debian.
- Measured 2026-10-23 on Debian 13 (257): an uncapped unit's cgroup
  carries the memory and pids controllers and not cpu (CPU accounting
  on cgroup v2 needs no controller), so a reload that gives a unit its
  first `cpu_quota` is what makes the manager enable cpu on the unit's
  path. The runner's own path — `SetUnitProperties`, then a read of
  the unit's `cgroup.controllers` — found the controller present at the
  reply in 40 of 40 rounds, and gone again at the reply to the lift:
  the manager realises the cgroup before it answers, so the enforcement
  list read right after a write is current, and nothing recomputes it
  between writes.
- Measured 2026-10-23 on Debian 13 (257): a change of
  `DefaultTasksMax=` (user.conf.d, then `daemon-reload` or
  `daemon-reexec`) reaches no running unit's kernel limit. An untouched
  unit reports the new `TasksMax=3000` from the manager while its
  `pids.max` stays 2273; a unit whose `TasksMax=` was set to the old
  default reports and holds 2273; a capped unit keeps its cap. So a
  lift to the default's number leaves a de-capped unit where an
  untouched one already is, until the next launch — and the manager's
  report of an untouched unit's `TasksMax` is not what the kernel
  enforces after such a change (the same quirk as the slice drop-in
  above). Measured on 257 only: on 255 and 259, where a
  `daemon-reload` does re-apply a slice drop-in, an untouched unit
  may follow the new default while a lifted one holds its number; both
  are outside the support matrix and were not measured.

A cap the host cannot enforce is named, not refused: the manager
accepts `MemoryMax=` on a unit whose cgroup the memory controller does
not reach (a container's user manager), so at adopt, at reattach and
after every live write the runner reads the unit's
`cgroup.controllers` (`readControllers`, `missingCaps`), warns when a
cap first appears in the list, and status lists those caps as
`caps_unenforced` rather than under `caps`. A reading that fails
leaves the last one standing, and the list is recomputed from it for
the caps as they now are; a handle with no reading yet claims nothing
either way, and the watcher reads again at each poll until one
succeeds (on a supported host the first succeeds). Refusing the launch, as the sandbox probe does for its
namespaces, was considered and not taken: a cap is availability, not
isolation, the hosts in question are outside the support matrix, and
a refusal would turn a host change into an outage for every capped
app. The infinity sentinel (2^64−1) is refused as a `memory_max` at
validation: the handle would report it as a cap and a reattach as
none.

Accepted: `memory_max` applies to the pre_start too — a migration
needing more than the server is raised like any other cap, and the
deploy's response names the OOM kill. A cap the manager refuses to
set live (a stale connection) applies at the next launch and the
journal says so; the next reload, a publication or a reattach tries
again, nothing in between does. The failure is ambiguous (the manager
may have applied the properties before the transport failed), so the
runner reads the unit back and takes the manager's word for the
handle's caps; with the unit unreadable too the caps are marked
unknown, which no sync takes as "already applied" and status does not
report as in effect, until a SetCaps succeeds.

## Non-goals

- Post-promote *auto*-revert. Rollback is an explicit operation —
  `POST /<app>?rollback=<version>` relaunches an on-disk release. The
  watchdog restarts the same version; it never auto-reverts.
- Multi-node or any cluster awareness.
- Resource limits beyond the three a user manager can set (`memory_max`,
  `tasks_max`, `cpu_quota` — see "Resource caps"): IO and cpuset caps
  need controllers `user@.service` does not delegate (measured on
  systemd 255, 257, 259); a CPU *weight* directive is one property
  away and waits for a case.
- Prometheus metrics (deploys_total, duration).
- Deploy queueing; 409 + CI retry is the queue.
- Windows.

## Open questions (with leans)

1. **Deploy events** — emit Caddy events (deploy_succeeded/_failed) so
   users can wire notifications. Lean: yes, cheap and composable.
2. **Admin endpoints** (`caddy liveswap status/rollback`) alongside the
   webhook. Lean: later; the webhook + status JSON covers CI and cron.
3. **Metrics.** Lean: add with events.
4. **Config-change restarts** — an explicit "apply now" admin action
   instead of waiting for the next launch. A rollback to the running
   version, or a deploy of it with the same artifact, is that action
   through the webhook (Behavior specification); the lean is to add
   no admin endpoint beside it.

## History

Dated one-liners; the full text of each is in git.

- 2026-08-17 (v0.1.0) — v1 shipped with an exec runner: apps as child
  processes in their own process group, restarting with the binary.
  The continuous watchdog was a stated non-goal.
- 2026-08-18 (#28) — Watchdog shipped as a follow-up, on by default.
- 2026-08-28 (#29) — Shared deploy secret replaced by `deploy_trust`
  (OIDC + local key).
- 2026-08-29 (#34, `17f1a04`) — Exec runner replaced, not fixed, by the
  systemd transient-unit runner on the motel user's manager; units
  survive a motel restart (`Reattach`). The test lane it needed is
  a privileged systemd container.
- 2026-09-02 (#40) — Every unit sandboxed; see DESIGN-sandbox.md.
- 2026-09-09 (#52) — A relaunch renders from the current spec: written
  down as the rule rather than the record growing a launch
  disposition, and status now reports the unit's own `ExecStart` so
  the drift is visible. `pid` left `state.json` with it.
- 2026-10-10 (#188) — Deploy auth became the leaf package
  `deploytrust/`: the `deploy_trust` grammar, sources, verifiers, the
  limiter and the webhook preamble, which now returns who the token is
  (`Identity`, the claims included) or a `Refusal` the caller writes
  through its own filter. The box webhook authenticates through it on
  the same limiter. `NewRedactor` and `RespondJSON` give it the
  response filter. One visible change: an app name carrying a control
  byte or invalid UTF-8 is Go-quoted in the two auth journal lines, as
  a refusal already was. Nothing else changed.
- 2026-10-16 (#234) — A deploy or rollback to the running version
  relaunches it from its release on disk (same artifact only, by
  digest; a push's upload is hashed and recorded as its pin from here
  on) instead of being refused; the two instances share the release
  dir for the overlap, recorded as an accepted residual.
- 2026-11-06 — A security sweep: `motel validate` and `reload` refuse,
  and `run` says, a config that runs apps with the admin API on TCP
  (cmd/motel's gate; scored 5×5×5), unless the liveswap block says it
  is meant (`allow_tcp_admin`, for the e2e rig whose runner drives the
  admin API over the network); a URL password net/url does not read
  (an unescaped `#`, a bare `%`, a space) is a known value all the same
  (3×5×5), and one holding `/` or `?` is no longer read short; recovery
  sweeps the staged artifacts a stop left in `tmp/` (3×4×5). The
  entropy bar's gap under 23 characters stays owed. Copilot's two
  rounds on #407: the gate loads each `--envfile` first, as Caddy does,
  the file read whole so a key's last assignment wins (5×5×5); an
  abstract socket (`unix/@name`) is refused as TCP is; a unix socket
  no app's view may hold — not under the liveswap root, whose
  directories are bound into the apps' sandboxes, nor under the base
  view every sandbox holds read-only (`AdminSocketInView`, 5×5×5), and
  named whole, since a relative path (`unix/admin.sock`) is Caddy's
  working directory's to place, which the gate cannot see (Copilot's
  fourth round, 5×5×5); a config that is not a regular file (stdin, a
  pipe), which the gate had left to Caddy unread, is refused at
  `validate` and `reload` and said at `run`, since the gate cannot see
  whether it runs apps with the admin API on TCP (Copilot's fifth
  round, 5×5×5);
  recovery's sweep of `tmp/` asks `linkAt` first, as GC does; a
  percent-encoded URL password on a later authority is registered
  decoded too.
- 2026-11-07 — The response filter's first layer runs as a failed
  deploy's detail is built, before the journal tail and the probe body
  are cut to their caps (`failureDetail`; the probe reads 4 KiB and
  keeps 512 bytes after the replacement): a cap that fell inside an
  `env_file` value left a fragment the filter could not match and no
  heuristic could see (scored 3×5×5; size 2, risk 1). The values held
  to are the launch's own (`launch.secrets`), so no file is read and a
  file that went unreadable since costs the record nothing, as
  `recordRedactor` asks; the keys found travel in the result's
  `redacted_env`, which the filter hoists into the body's own
  (`withField`). The token a read's own bound cut in two — the
  journal's 16 KiB, the probe's 4 KiB — goes too (`dropUnfinished`),
  since a replacement that shrinks the text can carry that fragment
  under the kept cap (self-review, 3×2×5); a value with a space in it
  is the residual row. The entropy bar follows
  the token's length (`log2(len) − 0.3`, up to 4.5), measured against
  the repository's own tokens; the owed row is paid as an accepted
  residual with the numbers, and the steeper bar first proposed is
  declined there. An assignment is judged by its parts, so a setting
  stays readable and `KEY=<secret>` keeps its key.
