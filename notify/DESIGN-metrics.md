# DESIGN — metrics

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- notify/DESIGN-metrics.md`);
the "History" section at the end holds only dated one-liners.

**Status: experimental, slice two** of notify and metrics, stacked on
slice one (notify/DESIGN-notify.md, PR #422). Opt-in twice over: nothing
runs without a `metrics_store` block, and the package only Suggests
`victoria-metrics`. Not for production boxes until slice one's Owed rows
that bar one have landed, and this document's own "Owed" rows marked so.
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
| Its listener | `/run/motel-metrics/vm.sock`, 0660 root:motel (`motel-vm.socket`) → `systemd-socket-proxyd` in the namespace → `127.0.0.1:8428` | no TCP port in the host namespace |
| vmalert | from the same package (Debian 13 has no `vmalert` package): evaluates `rules.yml` every minute, keeps its alert state in the store | `motel-vmalert.service`, `User=_victoria-metrics`, in the namespace, `MemoryMax=128M` |
| Its outlet | `127.0.0.1:9093` bound **inside** the namespace (`motel-am-proxy.socket`) → `systemd-socket-proxyd` → `/run/motel-notify/am.sock` | vmalert speaks TCP only, to its datasource and its notifier (unix_socket_test.md) |
| Its config | `rules.yml` (rendered by motel at every config load) and `scrape.yml` (written by setup), 0640 | `/var/lib/motel-metrics`, 2750 motel:_victoria-metrics (setup makes it) |
| motel's part | the `metrics_store` block, the push, the per-app collector, the rules render, the watch of the chain, `metrics` in `GET /<app>`, `motel metrics setup` | `notify/metrics*.go`; liveswap's `AppSamples` and `MetricsSource` (liveswap/appmetrics.go) |

Every unit file is the one measured on hotserve-1 since 2026-11-10
12:13 UTC (hotserve1_stack_test.md: three cold boots, no host listener,
both directions verified), with the paths and flags of this section.

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
them.

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
| `motel_app_up{app}`, `motel_app_release_info{app,release}` | liveswap's `AppSamples`: 1 while an instance runs; the running release |
| `motel_app_memory_bytes{app}`, `motel_app_memory_max_bytes{app}`, `motel_app_cpu_seconds_total{app,unit}` | the running unit's cgroup (`memory.current`, `MemoryMax=` when the kernel enforces it, `cpu.stat` `usage_usec`), read at each gather; absent while nothing runs or the file cannot be read. The CPU counter is one series per unit: each new unit counts from zero, and a count that rose across a cutover would read as the new unit's |

The per-app series are a collector registered on the same registry, so
an operator's own scraper of the admin `/metrics` sees them too. Label
values are what motel chose: app names, release names. No request path,
client address or header reaches the store.

**The push failing.** A failed push is retried at the next interval and
counted (`motel_metrics_push_failed_total`); the first failure after a
success is a warning line, `metrics_store: push failed`, and the first
success after failures an info line. Pushes failing for five minutes
fire `metrics_store_down` (box, state, warning; source `metrics`) through
notify's engine; the next success clears it, after the engine's hold.

## Rules

vmalert reads `rules.yml`, which motel renders at every config load
(`-configCheckInterval=1m` re-reads it: vmalert's HTTP listener is in
the namespace, so motel has no reload to call). setup writes the two
groups empty first, so vmalert starts before motel's first render. Every rule
is motel's text; nothing in the Caddyfile is a rule or a template.

| Rule | Expression | For | Labels | Reaches |
|---|---|---|---|---|
| `metrics_store_read_only` | `max(vm_storage_is_read_only) > 0` | 2 m | box, state, critical | the operator |
| `metrics_store_disk_fill` | `min(predict_linear(vm_free_disk_space_bytes[6h], 86400)) < 0` | 30 m | box, state, warning | the operator |
| `app_memory_high` | `max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > 0.9` | 10 m | `app`, `subject="app:<app>"`, state, warning | the operator and the app's bound channels |
| `ChannelFailed` (one per told channel) | `sum(increase(alertmanager_notifications_failed_total{integration=…, receiver_name=~…}[5m])) > 0` | — | exactly motel's `ChannelFailed` labels | the told channel alone |

Each fixed rule carries `kind="state"`, `severity`, `subject`, `source="vmalert"`
and `key="vmalert/<subject>/<name>"`, so slice one's route tree routes it
as one of motel's state alerts: route 6, per app when it names one, the
state receivers' `send_resolved`, one recovery when vmalert resolves it.
vmalert runs with `-disableAlertgroupLabel`, so an alert's labels are
the rule's and the expression's only.

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
file with no group at all).

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
| `metrics_rules_down` | metrics | state | warning | the store has not seen vmalert up (`max(max_over_time(up{job="vmalert"}[3m]))` empty or 0) at three reads in a row, a minute apart | one read sees it up |

A read the store does not answer is no reading: `metrics_store_down`
says so. When the last config with a `metrics_store` block stops, both
keys are reported cleared, so nothing fires on with no source. The
heartbeat (DESIGN-notify.md, "Dead-man and uptime") vouches for the box,
motel and Alertmanager; these two for the store and vmalert; the store's
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
  "cpu_cores": {"avg_5m": 0.004, "avg": 0.003}
}
```

Three fixed queries over the socket, scoped to `app="<name>"`: two
`query_range` templates over the last hour at a 60 s step, the memory
pair (`{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",app=…}`)
and `sum(rate(motel_app_cpu_seconds_total{app=…}[2m]))`, and one instant
`max_over_time(motel_app_memory_bytes{app=…}[1h])` for `max`, since a
60 s step holds one of the two samples a minute the push writes and a
peak between steps would be lost (Copilot on #423). `at` is the newest
point; `max` and `avg` are over the hour; `now`, `avg_5m` and
`memory_max_bytes` come only from points of the last three minutes and
five minutes, so an app that stopped, or a cap a later release dropped,
is history in `max` and `avg` and never reported as the present; a
number with no points is absent. The store answers 30 s behind the present
(`-search.latencyOffset`). No free-form query, no label enumeration, no
box series, no other app: the name is the configured app the route
already resolved.

The answer is best-effort: one 2 s budget for the three queries,
cached per app for 30 s, so a token (a stolen one, T2) cannot drive the
store harder than three queries per app per 30 s. A store that does not
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
   from until motel's render a moment later;
5. `daemon-reload`, `unmask`, `enable --now` the namespace, the store,
   both sockets and vmalert, then `restart` vmalert and `try-restart`
   both proxies, so a re-run's units are the ones running (the store was
   stopped in step 2, so its start takes the drop-in);
6. `try-reload-or-restart motel.service`, which renders.

Run again, it puts the units back as the binary ships them. The drop-in
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
| `/var/lib/victoria-metrics/` | Debian's | _victoria-metrics | the store, vmalert's state through it | the store |
| `/run/netns/motel-metrics` | — | root | the namespace unit | systemd |
| `/etc/systemd/system/motel-metrics-netns.service`, `motel-vm.socket`, `motel-vm.service`, `motel-am-proxy.socket`, `motel-am-proxy.service`, `motel-vmalert.service`, `victoria-metrics.service.d/10-motel.conf` | 0644 | root | setup | systemd |

## Threat model

New: two resident daemons motel did not write, in a namespace of their
own, one reachable from the host by unix-socket permission alone; and a
read path from the store into `GET /<app>`.

- **T1** (a poisoned dependency in an app): the socket and the state
  directory are not in its view (the probe's three keys); the store's
  TCP listener is in a namespace it is not in. It runs as the motel uid,
  so the view, not the mode, is the gate.
- **T2** (a stolen deploy token): reads that app's `metrics` object,
  far less than the deploy it can already do, at most three queries
  per 30 s; nothing of the box or of other apps; it cannot write to the
  store or reach vmalert.
- **T5** (RCE in motel): full read and write of the store (including
  its delete API) and the ability to rewrite `rules.yml`, which can make
  vmalert post any alert to Alertmanager, which T5 could already post
  itself. scrape.yml is setup's, but motel's uid owns the directory and
  could rewrite it to scrape anything the namespace reaches, which is
  the namespace's own loopback and Alertmanager: nothing T5 lacks.
- **T6** (a compromised config repository): can add or remove the block
  and move its socket path, no more; no rule, query or template is
  configurable.
- **The namespace's daemons** (`_victoria-metrics`, Debian's packages):
  the proxy gives whatever runs in the namespace Alertmanager's whole
  API, silences included. Only the store and vmalert run there; a
  compromise of either is a compromise of a root-installed Debian
  package, outside what motel defends.
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
queries; its RSS on a box is owed below.

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
| Per-app numbers are the unit's cgroup, not its requests | Caddy's request metrics carry server and handler, never the app, and no handler sees a liveswap app's responses without a Caddyfile change; the response observer is owed |
| `metrics` is a summary, not a series | the smallest object that answers "is my app well"; `?range=` is owed with the request numbers it is for |

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
| `GET /<app>` runs its three queries one after the other in the one 2 s budget: a store answering each in over two thirds of a second answers the whole object as the error, cached 30 s | notify/metricsview.go (`ask`) | 2 | 2 | 1 | 4; the answer says the store did not answer, which is true; running them at once adds goroutines and a join (size 2) to a path that is best-effort by design. /code-review pass 2; the third query from Copilot on #423 leaves the score as it was |
| vmalert's rules have no flapping guard: a condition that flaps around its threshold fires and resolves at each crossing | notify/metricsrules.go | 2 | 3 | 3 | 18; every rule has a `for:` of 2 to 30 minutes, which bounds a flap to one pair per `for:` |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| The response observer: per-app requests by class, 5xx share, p50 and p95, cache hit share, in the push, `metrics` and the rules (with `?range=24h`, and the post-deploy comparison) | — | — | — | slice three: needs a handler on the app's request path |
| Operator rules (the fresh-slate draft's `rule` line), as a grammar of fixed kinds with parameters, never a template | — | — | — | slice three |
| The store's RSS with `-memory.allowedBytes=96MiB` after a day, and on a 1 or 2 GB box | — | — | — | the next deploy to hotserve-1 |
| A reboot with motel rendering `rules.yml` and pushing, as slice one's was measured | — | — | — | the next deploy to hotserve-1 |
| Debian's security support for `victoria-metrics` 1.112.0 on trixie, and a dist-upgrade check of the drop-in's flags | — | — | — | before any release |

## e2e rows

`make e2e-metrics` (e2e/metrics/run.sh): a box built from notify-box's
image with Debian's `victoria-metrics` installed and masked
(e2e/Dockerfile, `metrics-box`), and the same stub receiver. The suite
runs `motel notify setup` and `motel metrics setup` itself and asserts:
no TCP listener on 8428, 8880 or 9093 in the host namespace, all three
in the namespace; the socket `srw-rw---- root motel` and the state
directory `drwxr-s--- motel _victoria-metrics`; root and motel reach the
store and nobody does; the store's and vmalert's `MemoryMax`; the
rendered `rules.yml` passes `vmalert -dryRun`; motel's push arrives
(`job="motel"`), with a deployed app's `motel_app_up`, release and CPU,
and its memory where the host's memory controller reaches the user
manager's units (Debian 13 delegates it; a CI container's host may not,
and the suite then asserts the series absent); `GET /<app>` with the
deploy token carries `metrics`, and without one is a 401; vmalert's
rules loaded, and its `ChannelFailed` for the failing channel firing
with motel's labels while Alertmanager holds one such alert, not two;
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
