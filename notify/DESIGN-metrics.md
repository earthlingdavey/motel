# DESIGN — metrics

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- notify/DESIGN-metrics.md`);
the "History" section at the end holds only dated one-liners.

**Status: experimental, slice two** of notify and metrics, stacked on
slice one (notify/DESIGN-notify.md, PR #422), **with slice three's
response observer** ("The response observer"; its rules, the operator's
`rule` lines and the post-deploy comparison, are owed). Opt-in twice over: nothing
runs without a `metrics_store` block, and the package only Suggests
`victoria-metrics`. Slice one's Owed rows that barred a production box
landed with #424; not for production boxes until this document's own
"Owed" rows marked so land.
Normative for what is built: a change that cannot hold a line here
changes the line in the same PR, with the reason.

Why a sibling of DESIGN-notify.md and not more sections in it: the
store is its own block, its own setup, its own package and units, and
its own entries in the threat model; it reuses notify's engine and
render, which DESIGN-notify.md describes and this one cites. The code
is in the same module (`notify/`, files `metrics*.go`), because the
store's rules are rendered from the notify block's receivers and its
chain events go through notify's engine; a module of its own would
export half of notify. The import direction is kept: liveswap knows
nothing of it, and reports through two interfaces it owns.

**motel measures; Debian's VictoriaMetrics stores; vmalert evaluates;
Alertmanager delivers.** motel pushes its metrics registry to a store
that listens on TCP only inside a private network namespace, reached
from the host through one unix socket; vmalert, in the same namespace,
evaluates a rule file motel renders and posts to motel's Alertmanager
through an in-namespace proxy; `GET /<app>` gains a `metrics` object
from fixed queries over the socket.

## The requirements

From #418 and #137 (requirement 7 of the fresh-slate draft), without
their solutions:

1. **Numbers per app and for the box**, kept for days, queryable after
   the fact, with no new listener and no new credential.
2. **Rules over the numbers**, reaching the same audiences as motel's
   own events (the operator; an app's deployers for its app).
3. **A deployer reads their app's numbers** on the route and with the
   credential they already use (`GET /<app>`, the deploy JWT), and
   nothing of the box or of other apps.
4. **motel never acts on a metric**: no refused cutover, no rollback.
5. **Exposed to an operator's own scraper** (the admin `/metrics`).
6. **The chain watches itself**: a store or evaluator that stops is
   told, not discovered.

## The shape

| Piece | What | Where |
|---|---|---|
| The namespace | `motel-metrics`, holding a loopback only | `motel-metrics-netns.service`, a oneshot (`ip netns add`, `lo up`) |
| The store | Debian 13's `victoria-metrics` 1.112.0: storage, MetricsQL, vmui, its own scrapes | Debian's unit with motel's drop-in (`notify/units/metrics/10-motel.conf`): `NetworkNamespacePath=`, `-httpListenAddr=127.0.0.1:8428` inside it, `-retentionPeriod=14d`, `MemoryMax=256M`, `-memory.allowedBytes=96MiB` |
| Its listener | `/run/motel-metrics/vm.sock`, 0660 root:motel (`motel-vm.socket`) → `systemd-socket-proxyd` in the namespace (`DynamicUser=yes`, no capability) → `127.0.0.1:8428` | no TCP port in the host namespace |
| vmalert | from the same package (Debian 13 has no `vmalert` package): evaluates `rules.yml` every minute, keeps its alert state in the store | `motel-vmalert.service`, `User=_victoria-metrics`, in the namespace, `MemoryMax=128M` |
| Its outlet | `127.0.0.1:9093` bound **inside** the namespace (`motel-am-proxy.socket`) → `systemd-socket-proxyd` (`_victoria-metrics` with motel as its group, no capability, a view of the socket's directory alone) → `/run/motel-notify/am.sock` | vmalert speaks TCP only, to its datasource and its notifier (unix_socket_test.md) |
| Its config | `rules.yml` (rendered by motel at every config load) and `scrape.yml` (written by setup), 0640 | `/var/lib/motel-metrics`, 2750 motel:_victoria-metrics (setup makes it) |
| motel's part | the `metrics_store` block, the push, the per-app collector, the rules render, the watch of the chain, `metrics` in `GET /<app>`, `motel metrics setup` | `notify/metrics*.go`; liveswap's `AppSamples` and `MetricsSource` (liveswap/appmetrics.go) |
| The response observer | `liveswap_observe <app>`, a handler the operator writes first in an app's site or route: its responses by class, time to headers and cache verdict, per release | liveswap/observe.go ("The response observer") |

Every unit file is the one measured on hotserve-1 since 2026-11-10
12:13 UTC (hotserve1_stack_test.md: three cold boots, no host listener,
both directions verified), with the paths and flags of this section,
except the two proxies' users and the drop-in's chmod of the store's
data, set after the box's audit of 2026-11-11 found both proxies running
as root and the data world-readable: measured in the e2e box (systemd
257), not yet on hotserve-1.

**The proxies run as no root.** Each passes bytes it does not parse, and
motel shapes some of them (its own reads through `vm.sock`, and what
vmalert posts from the rules motel renders), so a proxy as root would
make a bug in it T5's way to root. `motel-vm.service` needs nothing: its
listener comes by socket activation and it connects to the namespace's
loopback, so it is `DynamicUser=yes` with an empty
`CapabilityBoundingSet=`. `motel-am-proxy.service` must connect to
Alertmanager's socket, 0660 root:motel, so it holds the motel group,
which also reads the env files and the box's exchange tree; its view is
deny-by-default as an app's is (`TemporaryFileSystem=/:ro`, then `/usr`,
the linker's cache, `/run/motel-notify` and systemd's notify socket bound
back read-only). That needs a static uid, since `DynamicUser=` implies
`ProtectSystem=strict`, under which systemd keeps the host's root
(measured): the uid is `_victoria-metrics`, the namespace daemons' own,
whose processes are the proxy's only clients. A bug in it gives them the
uid they hold, and a group whose one path in that view is the socket
they reach through it anyway. motel is the proxy's primary group, not a
supplementary one: the kernel lets a process attach to, or read the
`/proc` of, one whose uid and gids all match its own, whatever their
supplementary groups, so with the store's gid the store could have
attached to the proxy and used its group past its view (Debian leaves
`ptrace_scope` at 0; measured, Copilot on #426). With motel's gid the
store's uid is refused both. `PrivateUsers=` would refuse them too; it
failed (217/USER, with `yes` and with `identity`) beside a supplementary
group, and with motel as the primary group it is untried. A signal checks
the uid alone, so the store's processes can still stop or kill the
proxy, which stops nothing they could not stop by stopping vmalert.

**Who reaches the socket, and why.** As for notify's: every app runs as
the motel uid, so the socket's group is not what keeps apps out; their
sandbox view is. `/run/motel-metrics` and `/var/lib/motel-metrics` are
in `sandboxMotelPaths` (liveswap/sandbox.go), and the view probe reports
`metrics_run`, `metrics_socket` and `metrics_state`, which the e2e suite
asserts closed. The group keeps out every other system user. The store's
TCP listener is in a namespace no app is in: from the host namespace
`127.0.0.1:8428` is refused (measured, debian_package_namespace_test.md).

## The metrics_store block

```
metrics_store {
    store <socket path>      # default /run/motel-metrics/vm.sock
}
```

Not `metrics`: Caddy's own global option of that name turns on its HTTP
metrics, which the push carries when it is set. The path is absolute,
and given once, or refused. There is no `state_dir`: vmalert's unit
reads the rules from `/var/lib/motel-metrics`, so a directory of the
block's own would be written and never read (/code-review pass 2). The
block takes nothing else: the push interval, the rules and the queries
are motel's, fixed. A `metrics_store` block with no
`notify` block loads, with a warning at every load, and vmalert is given
no rule: Alertmanager keeps the last config notify rendered when the
block is removed (notify's Stop releases only its engine), so a rule
that fired would still reach the old operator's and deployers' channels
(Copilot on #423). The store keeps the numbers, and `GET /<app>` reads
them. A reload to a config with no `metrics_store` block (notify alone,
or neither) writes the groups empty too, with the journal line
`metrics_store: the block is gone`: the store and vmalert are setup's
units and still run, so the stopped config's rules would go on
evaluating. motel's own shutdown leaves the rules, so the store's alerts
reach the operator while motel is down; Caddy empties its running
context before it stops the last config's apps, which is how the two
are told apart (`pusher.release`; Copilot round 3 on #423). A boot of a
config with no block (the block removed, then a restart or a reboot
rather than a reload) builds no `MetricsStore`, so cmd/motel's gate
does the same at `motel run` alone, before Caddy loads the config it
has just adapted: the groups empty in setup's state directory, and the
same line on stderr; nothing on a box the store is not set up on, and
nothing at `validate` or `reload` (`notify.RetireRules`; Copilot round
4 on #423).

## What motel pushes

Every 30 s motel gathers its config's metrics registry (Caddy's
per-config registry, the one the admin `/metrics` serves), encodes it
with `prometheus/common/expfmt`, already in the binary, and POSTs the
text to `/api/v1/import/prometheus?extra_label=job=motel` over the
socket (measured: 204, the label applied; 2026-11-10, 1.112.0). No new
Go module; no metrics state in motel. The registry holds:

| Series | From |
|---|---|
| `go_*`, `process_*`, `caddy_admin_*`, `caddy_config_last_reload_*` | Caddy's own collectors, on every config |
| `caddy_http_*` | Caddy, when the Caddyfile has Caddy's `metrics` option; per server and handler, never per app |
| `motel_notify_delivery_failed_total` | notify's engine |
| `motel_metrics_push_failed_total` | the push, below |
| `motel_app_up{app}`, `motel_app_release_info{app,release}` | liveswap's `AppSamples`: 1 while an instance runs; the current release, running or not (`motel_app_up` says which) |
| `motel_app_requests_total{app,release,class}`, `motel_app_response_seconds{app,release}` (a histogram), `motel_app_cache_responses_total{app,release,result}` | the response observer, below: the current release's counts and the one before's, for an app whose route has `liveswap_observe` and that has answered at least once |
| `motel_app_memory_bytes{app}`, `motel_app_memory_max_bytes{app}`, `motel_app_cpu_seconds_total{app,unit}` | the running unit's cgroup (its working set: `memory.current` less `memory.stat`'s `inactive_file`, the page cache the kernel gives back before the cap is felt, so an app that reads files under a cap does not read as at its limit; `MemoryMax=` when the kernel enforces it, `cpu.stat` `usage_usec`), read at each gather; absent while nothing runs or the file cannot be read. The CPU counter is one series per unit: each new unit counts from zero, and a count that rose across a cutover would read as the new unit's |

The per-app series are a collector registered on the same registry, so
an operator's own scraper of the admin `/metrics` sees them too. Label
values are what motel chose: app names, release names, and the
observer's fixed classes and cache results. No request path, client
address or header reaches the store.

**The push failing.** A failed push is retried at the next interval and
counted (`motel_metrics_push_failed_total`); the first failure after a
success is a warning line, `metrics_store: push failed`, and the first
success after failures an info line. Pushes failing for five minutes
fire `metrics_store_down` (box, state, warning; source `metrics`) through
notify's engine; the next success clears it, after the engine's hold.

## The response observer

Caddy's request metrics carry a server and a handler, never an app, and
no handler motel had saw a liveswap app's responses (the Decisions row
this section replaces). `liveswap_observe <app>` is that handler; its
mechanics, its order and its measured cost are DESIGN-liveswap.md's
("The response observer"). What it gives this store:

- **Classes.** `1xx` (a 101 alone: an upgrade's final response), `2xx`
  to `5xx`, and `aborted`, a client gone before the headers (counted in
  the total, never in the 5xx share). An error with nothing written (no
  instance running: a 502 that `handle_errors` answers after) takes its
  handler error's status.
- **Latency.** The time to the response's headers, in Prometheus's
  default buckets (5 ms to 10 s): of the app's own responses, so a cache
  hit, a 101 and an aborted request are left out. p50 and p95 are
  `histogram_quantile` over them: an estimate inside a bucket.
- **The cache.** The `Cache-Status` the cache in front wrote (RFC 9211,
  as Souin writes it): hit, stale (a hit with `fwd=stale`), miss
  (`fwd=uri-miss`, or a revalidation) or bypass. A response no cache
  spoke for is in no result.
- **Releases.** Each response counts under the release current when it
  began, kept for the current release and the one before (which the
  post-deploy comparison will read), for as long as the process runs.
  A motel restart starts them from zero, which the store's `increase()`
  reads as a counter reset.

An app with no `liveswap_observe` in its route pushes no request series,
and its `metrics` has no `requests`.

## Rules

vmalert reads `rules.yml`, which motel renders at every config load
(`-configCheckInterval=1m` re-reads it: vmalert's HTTP listener is in
the namespace, so motel has no reload to call). setup writes the two
groups empty first, so vmalert starts before motel's first render. Every rule
is motel's text; nothing in the Caddyfile is a rule or a template.

| Rule | Expression | For | Labels | Reaches |
|---|---|---|---|---|
| `metrics_store_disk_fill` | `min(predict_linear(vm_free_disk_space_bytes[6h], 86400)) < 0 and min(predict_linear(vm_free_disk_space_bytes[1h], 86400)) < 0` | 30 m | box, state, warning | the operator |
| `app_memory_high` | `max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.9` | 10 m | `app`, `subject="app:<app>"`, state, warning | the operator and the app's bound channels |
| `ChannelFailed` (one per told channel) | `sum(increase(alertmanager_notifications_failed_total{integration=…, receiver_name=~…}[5m])) > 0` | — | exactly motel's `ChannelFailed` labels | the told channel alone |

Each fixed rule carries `kind="state"`, `severity`, `subject`, `source="vmalert"`
and `key="vmalert/<subject>/<name>"`, so slice one's route tree routes it
as one of motel's state alerts: route 6, per app when it names one, the
state receivers' `send_resolved`, one recovery when vmalert resolves it.
vmalert runs with `-disableAlertgroupLabel`, so an alert's labels are
the rule's and the expression's only.

**The disk rule reads two windows.** Free space is the whole filesystem's, and one write of D bytes tilts the six-hour line below zero for hours on any disk with under about 7 × D free (computed at 30 s samples: 70 minutes at 6 × D, 286 at 2 × D). The hour's line is flat again once the write is an hour old, so with both and the 30 minutes' `for:` a single write fires only when it took half of what was free.

**ChannelFailed, a second source.** The store scrapes Alertmanager's
`/metrics` through the in-namespace proxy, every 30 s. For each
receiver of the notify render, each integration among its channels, and
each operator channel that is not one of that receiver's channels of
that integration (notify's `suspects`), the render writes a rule whose
labels are exactly those motel's own `ChannelFailed` posts for that
failure: `alertname`, `notify_channel`, `severity="warning"`,
`subject="box"`, `integration`, `suspects`. Receivers that share all
three are one rule (`receiver_name=~"a|b"`). Two posts with the same labels
are one alert in Alertmanager, so the two sources deduplicate; the
heartbeat receiver (no channel, so no suspect) is left to motel's own.
With no `notify` block both groups are empty (vmalert refuses a rule
file with no group at all); so are they after a reload takes the
`metrics_store` block away ("The metrics_store block").

**Not the Watchdog.** The fresh-slate draft and DESIGN-notify.md's
"Later slices" had vmalert's `Watchdog` beside motel's, deduplicated.
It cannot be, either way it is written. With motel's labels and an
always-true expression, it keeps the heartbeat pinging while motel is
down, and the dead-man is blind to what it exists to catch. Gated on
motel's pushes, it resolves when the push chain breaks, and vmalert
re-sends that resolve every minute for 15 minutes; a resolve with the
same labels resolves motel's own Watchdog too (measured in slice one:
the younger resolved alert wins the merge), so the ping skips while
motel is fine. So the heartbeat stays motel's alone, and motel watches
the chain instead (below).

## The chain

| Event | Source | Kind | Severity | Fires | Resolves |
|---|---|---|---|---|---|
| `metrics_store_down` | metrics | state | warning | every push has failed for 5 minutes | a push succeeds |
| `metrics_store_read_only` | metrics | state | critical | the store's own `/metrics`, read over the socket once a minute, has `vm_storage_is_read_only` above 0: its disk is below its free-space floor and nothing new is kept | one read has it 0 |
| `metrics_rules_down` | metrics | state | warning | the store has not seen vmalert up (`max(max_over_time(up{job="vmalert"}[3m]))` empty or 0) at three reads in a row, a minute apart | one read sees it up |

A read the store does not answer is no reading: `metrics_store_down`
says so.

**Read-only is motel's to tell, not a rule's.** A read-only store
refuses every write, its own scrapes among them (measured on Debian's
1.112.0: `/metrics` has the flag at 1, `max(vm_storage_is_read_only) > 0`
returns nothing), so the rule slice two first shipped could never fire,
and `up{job="vmalert"}` went unstored with it. motel reads the flag
from the store's `/metrics` and reports it through the engine, at every
read that finds it set (the engine drops what it is told while no
`notify` block runs, so one told only when it changed could be lost).
While a fresh read has it set, the two things that follow from it are not told as something
else: the count toward `metrics_rules_down` does not run, and
`metrics_store_down` is not raised for the refused imports. One read
the store does not answer leaves that standing (a store at its floor is
slow); three in a row end it, since the store may by then be down, and
`metrics_store_down` says that.
 When the last config with a `metrics_store` block stops, the
chain's keys are reported cleared, so nothing fires on with no source. The
heartbeat (DESIGN-notify.md, "Dead-man and uptime") vouches for the box,
motel and Alertmanager; these three for the store and vmalert; the store's
own scrape of Alertmanager feeds the second `ChannelFailed`.

## metrics in GET /<app>

The deployer's route and credential, unchanged (the fresh-slate draft,
2.8): `GET /<app>` with the bearer JWT `deploy_trust` accepts. With a
`metrics_store` block loaded, the status gains:

```json
"metrics": {
  "window": "1h",
  "at": "2026-11-10T12:00:00Z",
  "memory_bytes": {"now": 31457280, "max": 41943040},
  "memory_max_bytes": 268435456,
  "cpu_cores": {"avg_5m": 0.004, "avg": 0.003},
  "requests": {"total": 1234, "per_second": 0.343, "by_class": {"2xx": 1200, "4xx": 31, "5xx": 3},
               "5xx_share": 0.0024, "p50_ms": 12.3, "p95_ms": 80.1, "cache_hit_share": 0.61}
}
```

The window is `?range=`: `1h`, the default, or `24h`, a fixed set
(liveswap's `MetricsRanges`); any other is a 422, on a box with no
store too. Seven fixed queries over the socket, scoped to `app="<name>"`
and run at once. Two `query_range` templates over the window, at a 60 s
step for the hour and 5 m for the day, the memory
pair (`{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",app=…}`)
and `sum(rate(motel_app_cpu_seconds_total{app=…}[2m]))` (`[5m]` for the
day, one step); one instant `max_over_time(motel_app_memory_bytes{app=…}[<window>])` for `max`, since a
60 s step holds one of the two samples a minute the push writes and a
peak between steps would be lost (Copilot on #423). `at` is the newest
point; `max` and `avg` are over the hour; `now`, `avg_5m` and
`memory_max_bytes` come only from points of the last three minutes and
five minutes, so an app that stopped, or a cap a later release dropped,
is history in `max` and `avg` and never reported as the present; a
number with no points is absent (`max` too, when the hour holds no raw
sample). The CPU averages are rounded to a millionth of a core: an
unrounded float is a run of twenty digits the response filter masks as
a generated value, and the status would be withheld.

`requests` is four instant queries over the window: `sum by (class)` of
`increase_pure(motel_app_requests_total)`, `histogram_quantile` at 0.5
and 0.95 of the summed buckets' `increase_pure`, and `sum by (result)`
of the cache counts. `increase_pure`, not `increase`: a series appears
with its first push, already at that push's count, and the store's
`increase` drops a series' first value when it is large (measured on
1.112.0 in e2e metrics 3a: 22 responses read as 0, a single hit as 1);
`increase_pure` counts from zero when no sample comes before. `per_second` is the total over the window's length;
`5xx_share` is of the responses sent (the total less `aborted`);
`cache_hit_share` is hits and stale hits of the responses a cache spoke
for. Every release in the window is summed: the status beside it names
the current one. Every float is rounded (shares to 1/10 000, rates to a
millionth, so that a day's few requests are not 0, milliseconds to a
tenth), for the CPU averages' reason. A
share or quantile with no response under it is absent, and so is
`requests` when the store holds no request series for the app.
The store answers 30 s behind the present
(`-search.latencyOffset`). No free-form query, no label enumeration, no
box series, no other app: the name is the configured app the route
already resolved.

The answer is best-effort: one 2 s budget for the seven queries, run at
once, and cached per app and window for 30 s, so a token (a stolen one,
T2) cannot drive the store harder than seven queries per app and window
per 30 s, fourteen with both windows. A store that does not
answer is `"metrics": {"error": "the metrics store did not answer"}`,
fixed text, and the status is otherwise the same; the route never waits
longer or fails for it. The whole body passes the app's response filter,
as every status does.

## Packaging and setup

The `.deb` **Suggests** `victoria-metrics`, for the reason it Suggests
Alertmanager (DESIGN-notify.md, "Packaging and setup"): Debian's unit
starts on install listening on `0.0.0.0:8428`, unauthenticated
(measured, debian_package_namespace_test.md), and a `Depends` cannot
order motel's scripts before it. vmalert ships in the same package, so
there is one package to suggest. **`motel metrics setup`**, run by root,
after `motel notify setup`:

1. refuses unless `motel notify setup` has run (`motel-am.socket` is
   installed): vmalert's outlet is Alertmanager's socket;
2. `systemctl mask victoria-metrics.service`, then `stop` it;
3. `apt-get install -y --no-install-recommends victoria-metrics` unless
   installed, with `iproute2` when it is not (it is on every Debian
   install: `ip` makes the namespace);
4. writes the drop-in, the namespace unit, both sockets and their
   proxies, and `motel-vmalert.service` (all in the binary,
   `notify/units/metrics/`), and `/var/lib/motel-metrics` 2750
   motel:_victoria-metrics with `scrape.yml` and, unless motel has
   rendered one, a `rules.yml` of empty groups, which vmalert starts
   from until motel's render a moment later. The drop-in sets the
   store's data directory `/var/lib/victoria-metrics` to 0750 at every
   start (`ExecStartPre=+/usr/bin/chmod`): Debian ships it 0755, which
   lets every local uid read every series past the socket's group, and
   a purge and reinstall makes it so again;
   The drop-in's `Wants=motel-vmalert.service` has the store's start
   bring vmalert: a `systemctl stop victoria-metrics` stops vmalert too,
   and motel's next push starts the store again through the socket. To
   keep the store stopped, stop `motel-vm.socket` as well;
5. `daemon-reload`, `unmask`, `enable --now` the namespace, the store,
   both sockets and vmalert, then `restart` vmalert and `try-restart`
   both proxies, so a re-run's units are the ones running (the store was
   stopped in step 2, so its start takes the drop-in);
6. `try-reload-or-restart motel.service`, which renders.

Run again, it puts the units back as the binary ships them. A re-run
that fails after the mask (an install, a unit written to a full disk, a
reload) unmasks the store on its way out, starts `motel-vm.socket` (a
connection that arrived while the store was masked fails the socket),
and starts the store and vmalert again if the store was running, so a stack that worked is not left
masked and stopped; a first run that fails before step 5's `unmask`
stays masked, since Debian's unit without the drop-in listens on
`0.0.0.0`, and one that fails after it is left as far as it got, every
unit already written ("Accepted residuals"). The drop-in
replaces Debian's `ExecStart=`, so `/etc/default/victoria-metrics` (a
conffile) stays as shipped and an upgrade never prompts. One unit fact
was measured on hotserve-1 and is in the files: a socket unit bound
inside the namespace has `DefaultDependencies=no` and orders itself
after the namespace by hand (the first boot's ordering cycle).

## Paths

| Path | Mode | Owner | Writes | Reads |
|---|---|---|---|---|
| `/run/motel-metrics/vm.sock` | 0660 | root:motel | systemd (the socket unit) | root, motel |
| `/var/lib/motel-metrics/` | 2750 | motel:_victoria-metrics | setup (makes), motel | the store, vmalert |
| `…/rules.yml` | 0640 | motel:_victoria-metrics | motel, by rename; setup when absent | vmalert |
| `…/scrape.yml` | 0640 | motel:_victoria-metrics | setup | the store |
| `/var/lib/victoria-metrics/` | 0750 (the drop-in, at every start; Debian ships 0755) | _victoria-metrics | the store, vmalert's state through it | the store |
| `/run/netns/motel-metrics` | — | root | the namespace unit | systemd |
| `/etc/systemd/system/motel-metrics-netns.service`, `motel-vm.socket`, `motel-vm.service`, `motel-am-proxy.socket`, `motel-am-proxy.service`, `motel-vmalert.service`, `victoria-metrics.service.d/10-motel.conf` | 0644 | root | setup | systemd |

## Threat model

New: two resident daemons motel did not write, in a namespace of their
own, one reachable from the host by unix-socket permission alone; a
read path from the store into `GET /<app>`; and, with slice three, a
handler on every observed app's request path.

- **T1** (a poisoned dependency in an app): the socket and the state
  directory are not in its view (the probe's three keys); the store's
  TCP listener is in a namespace it is not in. It runs as the motel uid,
  so the view, not the mode, is the gate. Its responses reach the
  observer as a status, a time and a `Cache-Status` value, reduced to
  a class, a bucket and one of four results: an app can make its own
  numbers read badly, and no label of its choosing reaches the store.
- **T2** (a stolen deploy token): reads that app's `metrics` object,
  far less than the deploy it can already do, at most fourteen queries
  per 30 s (seven per window); nothing of the box or of other apps; it cannot write to the
  store or reach vmalert.
- **T5** (RCE in motel): full read and write of the store (including
  its delete API) and the ability to rewrite `rules.yml`, which can make
  vmalert post any alert to Alertmanager, which T5 could already post
  itself. scrape.yml is setup's, but motel's uid owns the directory and
  could rewrite it to scrape anything the namespace reaches, which is
  the namespace's own loopback and Alertmanager: nothing T5 lacks. The
  bytes it sends through either proxy reach no root process ("The
  shape").
- **T6** (a compromised config repository): can add or remove the block
  and move its socket path, no more; no rule, query or template is
  configurable.
- **The namespace's daemons** (`_victoria-metrics`, Debian's packages):
  the proxy gives whatever runs in the namespace Alertmanager's whole
  API, silences included. Only the store and vmalert run there; a
  compromise of either is a compromise of a root-installed Debian
  package, outside what motel defends. motel's own units give it no
  more: neither proxy runs as root ("The shape"), and the am proxy, the
  one they reach, holds their uid with motel's gid, so they cannot attach
  to it or read its `/proc`, and the group reaches only the socket in its
  view.
- The store holds label values motel chose (app, release, Caddy's
  server and handler names). A leak of it is low-sensitivity by
  construction; `GET /<app>` is app-scoped anyway, so a deployer never
  learns the box's shape from another app's numbers.

## Viewing

No new listener, no new credential kind, no session, as for
Alertmanager. root, on a laptop: `ssh -L
/run/user/$UID/vm.sock:/run/motel-metrics/vm.sock root@box` for
command-line clients, or a short-lived `ssh -L 8428:/run/motel-metrics/vm.sock`
for vmui in a browser, closed after (a TCP forward is an unauthenticated
full-power API for every process on the laptop). On the box, `curl
--unix-socket /run/motel-metrics/vm.sock http://vm/api/v1/query?...`.
A deployer reads `GET /<app>`'s `metrics`; not vmui, not the rules,
not other apps.

## Footprint

Measured on hotserve-1 (Hetzner arm64, 3.8 GB, Debian 13), the whole
stack under these caps (store `MemoryMax=256M` with the default 60 %
cache share, before `-memory.allowedBytes` was set) from 12:14 to 22:00
UTC on 2026-11-10, 2,600 series: the store 108 to 117 MB RSS
(cgroup 101 to 127 MB, peak 140 MB), vmalert 27 MB, each proxy 7 MB
while active and gone after five idle minutes, the store's disk 4.8 MB.
Alertmanager and motel unchanged. `-memory.allowedBytes=96MiB` holds the
caches under the 154 MB the default allowed, so the cap leaves room for
queries; its RSS on a box is owed below. The response observer adds at
most 24 series per observed app and release (six classes, the
histogram's 14, four cache results), two releases each: under 500 for
ten apps, against the 2,600 measured.

## Decisions

Each is a choice the fresh-slate draft or DESIGN-notify.md left open,
or a line of theirs the facts overturned:

| Decision | Why |
|---|---|
| The block is `metrics_store`, not `metrics` | Caddy's own global option is `metrics` |
| The code is in the notify module | its rules are rendered from notify's receivers and its chain events go through notify's engine (the opening section) |
| No second package (`motel-metrics`); the units ship in the binary | slice one's precedent: the package installs nothing of the stack until root runs setup, so a second `.deb` would hold six unit files and a command |
| vmalert comes from `victoria-metrics` | Debian 13 ships `/usr/bin/vmalert` in it and has no `vmalert` package (`apt-cache search vmalert`, 2026-11-10) |
| `metrics setup` requires `notify setup` | vmalert's only outlet is Alertmanager's socket, and its proxy's unit requires `motel-am.socket` |
| No vmalert `Watchdog` | the merge hazard above; motel watches the chain instead |
| `ChannelFailed` from vmalert is rendered per told channel with motel's labels | the only form in which Alertmanager deduplicates the two sources |
| `MemoryMax=256M`, `-memory.allowedBytes=96MiB`, 14 days | the measured working set above; both caps mandatory, since the store sizes its caches from what it can see |
| The rules are motel's text; the Caddyfile has no `rule` line yet | the review's finding 17: a rule or template a config author writes can pull any app's series into a notification; operator rules are owed with a grammar of fixed kinds |
| Per-app request numbers need a handler the operator writes, `liveswap_observe <app>` | Caddy's request metrics carry server and handler, never the app; a Caddy app cannot add a handler to the HTTP app's routes, and a dynamic upstream sees no response. The app is named on the line, since a cache hit never reaches the upstream |
| The observer sorts before `redir` | the earliest standard directive an order can anchor on; `cache` sorts before `rewrite`, so an observer anchored later would never see a hit (cmd/motel's order tests) |
| Its counts follow `current`, not `activeSocket` | a release that crashed owns the 502s that follow it, which is what a post-deploy comparison must see |
| Latency is to the headers, of the app's own responses | a streamed body or an upgrade has no response time to speak of; a hit is the cache's |
| `metrics` is a summary, not a series; `?range=` is `1h` or `24h` | the smallest object that answers "is my app well", over the two spans a deployer asks about; a fixed set, so a token cannot ask the store for any span |
| `metrics`' queries run at once | seven one after another would not fit the 2 s budget on a slow store (the residual row this replaced) |

## Triage

Findings are triaged by the triage skill (`.claude/skills/triage/SKILL.md`)
on its general scales. metrics' examples:

| Axis | Examples |
|---|---|
| **Severity 5** | an app or another uid reaching the store's socket; a box or another app's series in a deployer's `metrics`; a listener on the store's port in the host namespace |
| **Severity 4** | the store or vmalert unable to start after setup or a reboot until the console; `GET /<app>` failing or stalling because the store does not answer |
| **Severity 3** | the heartbeat pinging while motel is down; a rule alert to the wrong audience; a chain event never told |
| **Severity 2** | a duplicate or late rule alert; a number in `metrics` a minute stale |

### Accepted residuals

| Residual | Where | S | O | D | Why accepted |
|---|---|---|---|---|---|
| vmalert's `ChannelFailed` resolves 5 minutes after the last failure and re-sends the resolve for 15; with motel's labels it ends motel's own alert early, and a rise after that is told again | notify/metricsrules.go | 2 | 3 | 3 | 18; direct alerts never send a recovery, so the early end is silent; a second rise is a second failure, told |
| A cache hit after a cutover counts under the new release though the page was the old one's: the new release's 5xx and hit shares carry the old's cached answers until they expire | liveswap/observe.go | 2 | 3 | 3 | 18; latency leaves hits out, and a cache's TTL bounds the mix (30 s in the starter); telling a stored page's release needs a header the cache would store, a change to every app's responses |
| An app whose route has no `liveswap_observe` has no request numbers, and nothing says so at load | liveswap/observe.go | 2 | 3 | 2 | 12; `metrics` then has no `requests`, which a deployer sees; a load-time check needs the HTTP app's routes, which the liveswap app cannot read |
| The counts a motel restart (or a crash) ends within the last 30 s were never pushed, and are lost | liveswap/observe.go | 1 | 3 | 4 | 12; at most one push's worth, and the store reads the restart as a reset |
| vmalert's rules have no flapping guard: a condition that flaps around its threshold fires and resolves at each crossing | notify/metricsrules.go | 2 | 3 | 3 | 18; every rule has a `for:` of 2 to 30 minutes, which bounds a flap to one pair per `for:` |
| `now` and `at` in `GET /<app>` are judged by the range's step timestamps, not the samples': the store carries a sample forward onto the 60 s grid, so a stopped app's or a dropped cap's last value can read as now for up to about one step past `viewFresh` (3 m) | notify/metricsview.go (`fresh`) | 2 | 3 | 3 | 18; the status beside it says whether the app runs; the source timestamps need a `timestamp()` query per series (size 2, risk 2). Copilot round 2 on #423 |
| vmalert's `ChannelFailed` has no baseline: the store's `increase()` counts a series' first sample from zero (measured on 1.112.0: first seen at 3 gives 3, and again after a 17-minute gap), so a failure Alertmanager counted before `motel metrics setup`, or before a store outage of over ten minutes, is told once as new, for five minutes | notify/metricsrules.go | 2 | 2 | 3 | 12; one late warning to the operator's other channels about a channel that did fail; motel's own source keeps its baseline. A gate on the counter's age changes the expression every channel's rule shares (risk 2) and wants a run against the store to prove. /code-review pass 3 on #423 |
| A failed re-run's put-back starts `motel-vm.socket` (and `motel-am.socket`) whether or not it was active when setup began: one an operator had stopped to keep the store stopped is started, and a re-run after a first run that failed past the drop-in starts the socket of a stack that never ran | notify/setup.go (`putBackOnFailure`) | 2 | 2 | 3 | 12; a setup that succeeds enables and starts the same socket, which is what the operator ran it for; with the drop-in in place the store it can start is confined to the namespace. /code-review pass 2 on #425 |
| A first run of either setup that fails after its own `unmask` (`disable`, `enable --now`, a `restart`, the motel reload) is left unmasked and maybe partly started, not masked | notify/setup.go, notify/metricssetup.go | 2 | 3 | 1 | 6; every unit and drop-in is written before the unmask, so what can run is motel's confined shape (Alertmanager on its socket with no cluster port, the store in its namespace); setup ends on its error naming the step, and the next run is a re-run, with the put-back. A remask there (size 2, risk 2) would mask a service whose socket may be enabled already, so a connection fails the socket: the state the put-back exists to undo. Copilot round 3 on #425 |
| A store that turns read-only while `metrics_store_down` or `metrics_rules_down` already fires leaves that warning firing beside the critical until the store is writable (the read-only path holds back new ones, and does not clear old ones) | notify/metricspush.go (`checkReadOnly`) | 2 | 2 | 3 | 12; the warning was true when raised and the critical beside it names the cause; clearing them there is two more branches in the path every read takes. Copilot on #425 |
| A reload that moves the `store` line to another socket starts the counts afresh (`setLive`) but keeps what already fires, `metrics_store_read_only` among them, until the new store answers: a read-only alert raised for the old store stays told while the new one does not answer | notify/metricspush.go (`setLive`) | 2 | 2 | 1 | 4; it needs the old store read-only and the new one not answering; the first answer of the new store clears it, and the new store's own silence fires `metrics_store_down`, which names the push failing, once `readOnlyStale` asks have gone unanswered (`heldReadOnly`, as for any read-only store that stops answering). Clearing it at the move would resolve it on no evidence. Copilot round 2 on #425 |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| The response observer's numbers in the rules: the post-deploy comparison (#418: the new release against the old one's trailing window of the same length, a sample floor, absolute thresholds, a failure count; it notifies only) | — | — | — | slice three's rules PR, with the operator's `rule` lines |
| Operator rules (the fresh-slate draft's `rule` line), as a grammar of fixed kinds with parameters, never a template | — | — | — | slice three |
| The store's RSS with `-memory.allowedBytes=96MiB` after a day, and on a 1 or 2 GB box | — | — | — | the next deploy to hotserve-1 |
| A reboot with motel rendering `rules.yml` and pushing, as slice one's was measured, on a booted Debian host: the namespace's and the sockets' cold-boot order, a connection made while setup has the store masked, and a failed setup re-run's put-back (Copilot round 2 on #425); the two proxies' users and the am proxy's view, measured so far in the e2e box only | — | — | — | the next deploy to hotserve-1 |
| motel says nothing when setup's units on disk are older than the binary's: a box upgraded past a change to them (this one: the proxies' users, the chmods) keeps the old units until root re-runs both setups. A load could compare the installed files with the embedded ones and warn | 3 | 5 | 5 | the next PR on the feature branch, before it merges to main (/code-review of #426) |
| Debian's security support for `victoria-metrics` 1.112.0 and `prometheus-alertmanager` 0.28.1 on trixie, whose binaries are built with Go 1.24.2 (the store, vmalert) and 1.24.1 (Alertmanager), and a dist-upgrade check of both drop-ins' flags | — | — | — | before any release |

## e2e rows

`make e2e-metrics` (e2e/metrics/run.sh): a box built from notify-box's
image with Debian's `victoria-metrics` installed and masked
(e2e/Dockerfile, `metrics-box`), and the same stub receiver. The suite
runs `motel notify setup` and `motel metrics setup` itself and asserts:
no TCP listener on 8428, 8880 or 9093 in the host namespace, all three
in the namespace; the socket `srw-rw---- root motel` and the state
directory `drwxr-s--- motel _victoria-metrics`; the store's data
`drwxr-x--- _victoria-metrics _victoria-metrics`; root and motel reach the
store and nobody does; the store's and vmalert's `MemoryMax`; the
rendered `rules.yml` passes `vmalert -dryRun`; motel's push arrives
(`job="motel"`), with a deployed app's `motel_app_up`, release and CPU,
and its memory where the host's memory controller reaches the user
manager's units (Debian 13 delegates it; a CI container's host may not,
and the suite then asserts the series absent); `GET /<app>` with the
deploy token carries `metrics`, and without one is a 401; an observed, cached site's
responses counted by class, cache result and latency, and a site of an
app with nothing running counted as 5xx; `GET /<app>?range=24h` carrying
`requests`, and `?range=7d` a 422; vmalert's
rules loaded, and its `ChannelFailed` for the failing channel firing
with motel's labels while Alertmanager holds one such alert, not two;
both proxies running as a uid not root with no capability, the am proxy
as `_victoria-metrics` with gid motel and no other group, with
Alertmanager's socket in its view and none of `/etc/motel`,
`/var/lib/liveswap` or any other path an app's view hides, and the
store's uid refused its `/proc` and an attach (with a control: the same
uid attaching to a sleeper of its own; a host that refuses that too has
the `/proc` check stand for it);
the store stopped by hand, started again by motel's next push with vmalert beside it;
the store made read-only (its free-space floor set above any disk): no series carries the flag, and motel's `metrics_store_read_only` is in Alertmanager, critical;
motel's shutdown leaving `rules.yml`'s alerting rules, and a reload
without the `metrics_store` block writing none; a restart without it
writing none too, and vmalert loading zero alerting rules after;
the view probe's three metrics keys closed from inside a sandboxed app;
the store's socket stopped, `GET /<app>` answering at once (13 ms
measured) with the error and the rest of the status whole, and the
`push failed` line. First run 2026-11-10 on the lima arm64 VM: the
two sources' `ChannelFailed` held by Alertmanager as one alert, with
vmalert's `generatorURL`.
Not in e2e (unit-tested only): `metrics_store_down`'s five minutes and
`metrics_rules_down`'s three reads, the cache's 30 s.

## History

Dated one-liners; the full text of each is in git.

- 2026-11-10 — Designed and built as slice two on `notify/slice-two-metrics`,
  stacked on #422: Debian's `victoria-metrics` in a private namespace
  behind `systemd-socket-proxyd` (the units measured on hotserve-1 the
  same day), motel's push, the per-app collector, rendered rules,
  `metrics` in `GET /<app>`, `motel metrics setup`. **The draft's vmalert
  `Watchdog` beside motel's is reversed** (the merge hazard, "Rules");
  vmalert's `ChannelFailed` is kept as a second source in the one form
  Alertmanager deduplicates.
- 2026-11-11 — /code-review pass 3 (xhigh, the user's) on #423, triaged: the CPU
  averages rounded (the response filter withheld an idle app's status),
  the working set for `motel_app_memory_bytes`, `max` absent with no raw
  sample, the disk rule's second window, the store's `Wants=` for
  vmalert; one row accepted, two owed.
- 2026-11-11 — The two rows pass 3 left owed: `metrics_store_read_only` is
  motel's own read of the store's `/metrics` (the rule could never fire,
  and is gone), and both setups unmask on a failed re-run.
- 2026-11-11 — Copilot round 3 on #423, triaged: a reload that takes the
  `metrics_store` block away retires its rules (the both-to-neither row
  goes with it); motel's shutdown keeps them (e2e metrics 8).
- 2026-11-11 — Copilot round 4 on #423, triaged: a restart (not a
  reload) after the block is removed retires the rules too, from
  cmd/motel's gate at `motel run` (e2e metrics 9); `motel_app_release_info`
  documented as the current release, running or not.
- 2026-11-11 — #425 rebased onto #423's merge; Copilot round 2 on #425,
  triaged: a store move's carried alert accepted (row), the booted-host
  checks named in the reboot row.
- 2026-11-11 — Copilot round 3 on #425, triaged: a first run that fails
  after its unmask is left unmasked in motel's confined shape (row); the
  three sentences that said it stays masked corrected.
- 2026-11-11 — The audit of hotserve-1 on the feature branch: both
  socket proxies run as no root (the am proxy in a deny-by-default view,
  for the motel group it needs, which is its primary group so the store
  cannot attach to it), and the drop-in sets the store's data to 0750 at
  every start (Debian ships 0755).
- 2026-11-11 — Slice three's response observer, `liveswap_observe`:
  request series per release in the push, `requests` and `?range=` in
  `GET /<app>`, whose queries now run at once (the serial-queries row
  goes); the request path measured before and after.
