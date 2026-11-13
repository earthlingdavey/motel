# DESIGN — metrics

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- notify/DESIGN-metrics.md`);
the "History" section at the end holds only dated one-liners.

**Status: experimental, slice two** of notify and metrics, stacked on
slice one (notify/DESIGN-notify.md, PR #422), **with slice three's
response observer** ("The response observer") and the operator's `rule`
lines over its numbers, the post-deploy comparison among them
("Operator rules"). Opt-in twice over: nothing
runs without the monitor block's `metrics` line, and the package only
Suggests `victoria-metrics`. Slice one's Owed rows that barred a production box
landed with #424; not for production boxes until this document's own
"Owed" rows marked so land.
Normative for what is built: a change that cannot hold a line here
changes the line in the same PR, with the reason.

Why a sibling of DESIGN-notify.md and not more sections in it: the
store is its own line of the monitor block, its own setup
(`--metrics`), its own package and units, and its own entries in the
threat model; it reuses notify's engine and render, which
DESIGN-notify.md describes and this one cites. The code is in the same
module (`notify/`, files `metrics*.go`) and the same Caddy app
(`monitor`), because the store's rules are rendered from the monitor
block's receivers and its chain events go through notify's engine; a module of its own would
export half of notify. The import direction is kept: liveswap knows
nothing of it, and reports through two interfaces it owns.

**motel measures; Debian's VictoriaMetrics stores; vmalert evaluates;
Alertmanager delivers.** motel pushes its metrics registry to a store
that listens on TCP only inside a private network namespace, reached
from the host through one unix socket; vmalert, in the same namespace,
evaluates a rule file motel renders and posts to motel's Alertmanager
at a listener Alertmanager holds in the namespace; `GET /<app>` gains a `metrics` object
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
| The store | Debian 13's `victoria-metrics` 1.112.0: storage, MetricsQL, its own scrapes (no vmui: Debian builds it without, "Viewing") | Debian's unit with motel's drop-in (`notify/units/metrics/10-motel.conf`): `NetworkNamespacePath=`, `-httpListenAddr=127.0.0.1:8428` inside it, `-retentionPeriod=14d`, `MemoryMax=256M`, `-memory.allowedBytes=96MiB` |
| Its listener | `/run/motel-metrics/vm.sock`, 0660 root:motel (`motel-vm.socket`) → `systemd-socket-proxyd` in the namespace (`DynamicUser=yes`, no capability) → `127.0.0.1:8428` | no TCP port in the host namespace |
| vmalert | from the same package (Debian 13 has no `vmalert` package): evaluates `rules.yml` every minute, keeps its alert state in the store | `motel-vmalert.service`, `User=_victoria-metrics`, in the namespace, `MemoryMax=128M` |
| Its outlet | `127.0.0.1:9093` bound **inside** the namespace (`motel-am-metrics.socket`) and handed to Alertmanager itself, beside `/run/motel-notify/am.sock`; Alertmanager's drop-in `20-motel-metrics.conf` has `Wants=`, `After=` and `PartOf=` it | vmalert speaks TCP only, to its datasource and its notifier (unix_socket_test.md); Debian's Alertmanager 0.28.1 serves every listener systemd passes it (exporter-toolkit's `ServeMultiple`) |
| Its config | `rules.yml` (rendered by motel at every config load) and `scrape.yml` (written by setup), 0640 | `/var/lib/motel-metrics`, 2750 motel:_victoria-metrics (setup makes it) |
| motel's part | the monitor block's `metrics` line, the push, the per-app collector, the rules render, the watch of the chain, `metrics` in `GET /<app>`, `motel monitor setup --metrics` | `notify/metrics*.go`; liveswap's `AppSamples` and `MetricsSource` (liveswap/appmetrics.go) |
| The response observer | `observe <app>`, a handler the operator writes first in an app's site or route: its responses by class, time to headers and cache verdict, per release | liveswap/observe.go ("The response observer") |

Every unit file is the one measured on hotserve-1 since 2026-11-10
12:13 UTC (hotserve1_stack_test.md: three cold boots, no host listener,
both directions verified), with the paths and flags of this section,
except the two proxies' users and the drop-in's chmod of the store's
data, set after the box's audit of 2026-11-11 found both proxies running
as root and the data world-readable: measured in the e2e box (systemd
257), then on hotserve-1 with the deploy of 01b2fd5 on 2026-11-12,
whose reboot at about 17:53 UTC brought both proxies up as no root.
The namespace unit's test for a leftover before its delete (#430,
2026-11-12) is measured in the e2e box alone. A fourth cold boot, on
2026-11-11 at 15:04 UTC with motel (9228935) rendering `rules.yml` and
pushing, brought every
unit up within a second of the namespace (the am proxy's service at its
first connection, nine seconds later), with no failed unit, no
ordering cycle and no listener in the host namespace; vmalert loaded
motel's rules, and the store scraped itself, vmalert and Alertmanager.

**The store's proxy runs as no root.** It passes bytes it does not
parse, and motel shapes some of them (its own reads through `vm.sock`),
so a proxy as root would make a bug in it T5's way to root. It needs
nothing: its listener comes by socket activation and it connects to the
namespace's loopback, so `motel-vm.service` is `DynamicUser=yes` with an
empty `CapabilityBoundingSet=`. Alertmanager's outlet had a proxy of its
own until 2026-11-12, which held the motel group in a deny-by-default
view; Alertmanager now holds that listener itself (below, and
"History").

**Alertmanager listens in the namespace itself.**
`motel-am-metrics.socket` binds `127.0.0.1:9093` inside the namespace
(`NetworkNamespacePath=`), names `Service=prometheus-alertmanager.service`,
and is handed to Alertmanager when it starts, beside `am.sock`.
Alertmanager 0.28.1 serves every listener it is passed, in whatever
order the fds come, and reads no name, so the unit sets no
`FileDescriptorName=` (measured in the e2e box, 2026-11-12). It takes
its sockets only when it starts, and systemd refuses to start a socket
whose service already runs, so:

- Alertmanager's drop-in `20-motel-metrics.conf`, which `--metrics`
  writes beside notify's `10-motel.conf`, has `After=` the socket, so
  that it listens before Alertmanager starts, whatever starts it (either
  socket's first connection, a restart, a boot, Debian's postinst);
  `Wants=`, not `Requires=` or `BindsTo=`, so that a namespace that
  fails leaves Alertmanager on `am.sock` and notify working (with
  `BindsTo=`, a namespace that failed failed `motel-am.socket` at its
  trigger limit: measured); and `PartOf=`, so that a stop or restart of
  the socket, which the namespace's stop or restart brings (its
  `Requires=`), stops or restarts Alertmanager, which would otherwise
  keep a listener in a namespace that is gone while the socket could not
  start again. A stop of the namespace stops Alertmanager; `am.sock`
  still listens, so motel's next post, or its read of Alertmanager's
  counters a minute on, starts it again, and its start brings the socket
  and the namespace back.
- Setup restarts Alertmanager once when the socket is not active (a
  first run, an upgrade from the proxy, an Alertmanager left with no
  listener here), with one `systemctl restart`, whose transaction starts
  the namespace and the socket before Alertmanager. A re-run with the
  socket listening leaves Alertmanager as it runs.
- An Alertmanager that started while the namespace could not (the
  socket's start then fails, and `Wants=` lets Alertmanager start on
  `am.sock` alone) holds no listener in the namespace, and no later start
  of the socket can hand it one: vmalert's posts and the store's scrape
  of Alertmanager would fail from then on, and nothing reads
  `up{job="alertmanager"}`. So the namespace unit restarts a running
  Alertmanager whenever the namespace comes up
  (`ExecStartPost=-/usr/bin/systemctl --no-block try-restart prometheus-alertmanager.service`),
  which motel's next push brings about through the store. One not
  running, or starting in the same transaction, is left as it is.
  `TimeoutStartSec=30` bounds the namespace's start: a oneshot waits
  with no limit by default, and Alertmanager's start waits for it.
- A stop of the namespace stops the socket (its `Requires=`), and so
  Alertmanager (`PartOf=`); a restart restarts both, since systemd
  restarts a unit that requires one restarted. A start of the namespace
  alone starts neither: `Requires=` starts nothing back. So vmalert,
  whose posts are the socket's traffic, `Wants=` it: the store's start,
  which brings vmalert, brings the socket, and vmalert's post or the
  store's scrape starts Alertmanager through it. Without that line only
  Alertmanager's own start (motel's read of `am.sock`, a minute apart)
  brought the socket back, so with motel down vmalert's posts were
  refused until motel came back.

These two units and the namespace unit's two lines are measured in the
e2e box alone (systemd 257), with Debian's postinst restart, a
`daemon-reload` and `daemon-reexec`, a SIGKILL of Alertmanager, a
namespace restart, a stop and return, a stop and start by hand with
motel down, and an upgrade from the proxy
(the spike and a second opinion, 2026-11-12, and e2e metrics 1, 4d and 11);
a cold boot on hotserve-1 is owed ("Owed").


**Who reaches the socket, and why.** As for notify's: every app runs as
the motel uid, so the socket's group is not what keeps apps out; their
sandbox view is. `/run/motel-metrics` and `/var/lib/motel-metrics` are
in `sandboxMotelPaths` (liveswap/sandbox.go), and the view probe reports
`metrics_run`, `metrics_socket` and `metrics_state`, which the e2e suite
asserts closed. The group keeps out every other system user. The store's
TCP listener is in a namespace no app is in: from the host namespace
`127.0.0.1:8428` is refused (measured, debian_package_namespace_test.md).

## The metrics line

```
monitor {
    channel …                 # notify's lines (DESIGN-notify.md, "The monitor block")
    metrics [<socket path>]   # default /run/motel-metrics/vm.sock
}
```

A line of the monitor block, not a block of its own: Caddy's own
global option `metrics` turns on its HTTP metrics, which the push
carries when it is set, and one global option yields one Caddy app, so
the store is a part of the monitor app (`App.Metrics`; "Decisions").
The path is absolute, the line given once and with no block, or
refused. There is no `state_dir`: vmalert's unit reads the rules from
`/var/lib/motel-metrics`, so a directory of the line's own would be
written and never read (/code-review pass 2). The line takes nothing
else: the push interval, the fixed rules and the queries are motel's;
the operator's `rule` lines beside it pick among motel's kinds
("Operator rules").

A monitor block that is a `metrics` line alone runs no notify, loads
with a warning at every load (`monitor metrics: the monitor block has
no channel, only its metrics line`), and vmalert is given no rule:
Alertmanager keeps the last config notify rendered when the channels
are removed (notify's part of the app's Stop releases only its
engine), so a rule that fired would still reach the old operator's and
deployers' channels (Copilot on #423). The store keeps the numbers, and
`GET /<app>` reads them. A reload to a config with no `metrics` line
(the channels alone, or no monitor block) writes the groups empty too,
with the journal line `monitor metrics: the metrics line is gone`: the
store and vmalert are setup's units and still run, so the stopped
config's rules would go on evaluating. motel's own shutdown leaves the
rules, so the store's alerts reach the operator while motel is down;
Caddy empties its running context before it stops the last config's
apps, which is how the two are told apart (`pusher.release`; Copilot
round 3 on #423). A boot of a config with no `metrics` line (the line
removed, then a restart or a reboot rather than a reload) builds no
`MetricsStore`, so cmd/motel's gate does the same at `motel run` alone,
before Caddy loads the config it has just adapted, reading
`apps.monitor.metrics` in the adapted JSON: the groups empty in setup's
state directory, and the same line on stderr; nothing on a box the
store is not set up on, and nothing at `validate` or `reload`
(`notify.RetireRules`; Copilot round 4 on #423). Inside the one app,
Provision runs notify's part first (the rules are rendered from its
receivers), Start points notify's engine at the config before the
pusher, so the chain's events reach an engine serving it, and Stop
releases the pusher before the engine.

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
| `motel_app_observed{app}` | 1 for an app a route of the running config observes (`observe <app>`), from the config's first push, whatever its release; the request thresholds' guard (`PUSHED`, "Operator rules") |
| `motel_app_up{app}`, `motel_app_release_info{app,release}` | liveswap's `AppSamples`: 1 while an instance runs; the current release, running or not (`motel_app_up` says which) |
| `motel_app_requests_total{app,release,class}`, `motel_app_response_seconds{app,release}` (a histogram), `motel_app_cache_responses_total{app,release,result}` | the response observer, below: the current release's counts and the one before's, for an app whose route has `observe`: the current release's from its first push, at zero before its first response, and the one before's once it has a response (below) |
| `motel_disk_avail_bytes{path}` | statfs of the directories notify's disk source measures, one series per filesystem, which an operator's `disk_fill` rule reads ("Operator rules") |
| `motel_app_memory_bytes{app}`, `motel_app_memory_max_bytes{app}`, `motel_app_cpu_seconds_total{app,unit}` | the running unit's cgroup (its working set: `memory.current` less `memory.stat`'s `inactive_file`, the page cache the kernel gives back before the cap is felt, so an app that reads files under a cap does not read as at its limit; `MemoryMax=` when the kernel enforces it, `cpu.stat` `usage_usec`), read at each gather; absent while nothing runs or the file cannot be read. The CPU counter is one series per unit: each new unit counts from zero, and a count that rose across a cutover would read as the new unit's |

The per-app series are a collector registered on the same registry, so
an operator's own scraper of the admin `/metrics` sees them too. Label
values are what motel chose: app names, release names, and the
observer's fixed classes and cache results. No request path, client
address or header reaches the store.

**The push failing.** A failed push is retried at the next interval and
counted (`motel_metrics_push_failed_total`); the first failure after a
success is a warning line, `monitor metrics: push failed`, and the first
success after failures an info line. Pushes failing for five minutes
fire `metrics_store_down` (box, state, warning; source `metrics`) through
notify's engine; the next success clears it, after the engine's hold.

## The response observer

Caddy's request metrics carry a server and a handler, never an app, and
no handler motel had saw a liveswap app's responses (the Decisions row
this section replaces). `observe <app>` is that handler; its
mechanics, its order and its measured cost are DESIGN-liveswap.md's
("The response observer"). What it gives this store:

- **Classes.** `1xx` (a 101 alone: an upgrade's final response), `2xx`
  to `5xx`, and `aborted`, a request no headers went to: its client
  gone first, or its handler aborted (Caddy's `abort`, a panic), counted
  in the total, never in the 5xx share. An error with nothing written
  (no instance running: a 503 that `handle_errors` answers after) takes
  its handler error's status; no error and nothing written is the 200
  net/http sends.
- **Latency.** The time to the response's headers, in Prometheus's
  default buckets (5 ms to 10 s) and three below them, 0.5, 1 and
  2.5 ms (an app behind a socket on the same box answers in under 5 ms,
  which the defaults read as 2.5 ms at p50 and 4.8 ms at p95 whatever
  its times): of the app's own responses, those that
  went to its upstream or that the cache fetched from it (a miss or a
  bypass, a miss Souin coalesced onto another request's upstream call
  among them), so a cache hit, penaltybox's refusal, any other handler's
  answer, a 101 and an aborted request are left out. The headers are
  those the client is sent: behind Souin they go when it sends the
  response, which it buffers whole first to store it, so a miss's time
  runs to the end of the app's body, the time the client waited for its
  first byte; with no cache in front it is the app's own time to
  headers. p50 and p95 are `histogram_quantile` over them: an estimate
  inside a bucket.
- **The cache.** The `Cache-Status` the cache in front wrote (RFC 9211,
  as Souin writes it): hit, stale (a hit with `fwd=stale`), miss
  (`fwd=uri-miss`, or a revalidation) or bypass. Souin's entry is the
  header's first, which the observer reads: it replaces an app's own
  entry, or on a bypass comes before the one reverse_proxy adds. With no
  cache in front, an app's own `Cache-Status` is read as its cache's
  verdict, and its hits are left out of the latency like the cache's.
  Souin's `key=` is unquoted and holds the request's query, so the
  parameters are read up to it, and after it only the stale hit's ending
  (`; fwd=stale`, and a `; fwd-status=<n>` after it): a `;` or `,` in a
  URL is never a verdict. A miss whose upstream failed is a miss too: Souin names it
  (`detail=SERVE-HTTP-ERROR`) and returns the error, nothing written. A
  response no cache spoke for is in no result.
- **Releases.** Each response counts under the release whose instance
  it went to (the socket the app's upstream selected), or, when it went
  to none (a cache hit, a refusal, no instance running, a miss Souin
  coalesced onto another request's call), the release current when it
  began; kept for the current release and the one
  before (which the post-deploy comparison reads), for as long as
  the process runs.
  A motel restart starts them from zero, which the store's `increase()`
  reads as a counter reset, and so does an `observe` line put back
  after no loaded config had one for the app (below).

An app with no `observe` in its route pushes no request series,
and its `metrics` has no `requests`. That holds for an app whose line
is taken out too: its counts live on in the pool, but none is pushed
while the running config does not observe it, since pushed unchanged
they would read as an app no request reaches. That is from the
reload's first push, though the old config's handler counts its
requests in flight until they end (a stream, for as long as it lasts).
Its series go stale in the store,
and the request thresholds leave it out from the first push without
its `motel_app_observed` (`PUSHED`, "Operator rules"), so an every-app
`request_rate <` alert firing for it resolves. The
first observer after none zeroes them in place, so the line put back
counts from zero, as after a restart: resumed, their first push after
the gap would be read as every response since motel started, in one
window. An observed app's current release
is pushed from the first push that knows it, at zero before its first
response (the config's observe handlers say which apps it observes,
from its Provision; after a restart, liveswap knows the release once
it has reattached it, so the first pushes may come without it, while
`motel_app_observed` is in every push from the first): with no
series, a `request_rate <` rule could never hold for an app no one has
reached since motel started, the stopped traffic it exists to tell. The
release before is pushed once it has a response.

## Rules

vmalert reads `rules.yml`, which motel renders at every config load.
vmalert's HTTP listener is in the namespace, so motel has no reload to
call: `-configCheckInterval=5s` re-reads the file, a few KB, and
vmalert applies it only when it changed. (A path unit sending SIGHUP
was measured and dropped: a SIGHUP before vmalert installs its handler
ends it with a clean exit, which `Restart=on-failure` leaves dead, and
a rename while the path's oneshot runs is never seen.) The store's
`-search.latencyOffset` is 5 s, not VictoriaMetrics' 30 s default,
and vmalert's `-rule.evalDelay` matches it: the default waits out
scrapes arriving from many sources, and motel is the store's one
pusher, so the 30 s was a wait in every alert and every read. A
post-deploy check asks for the 5 s itself (`latency_offset`), so on a
store set up before, whose unit keeps the 30 s until setup runs again,
its windows are still right; `GET /<app>` and the chain's check read
30 s behind there meanwhile. Measured: the metrics suite 958 s to
about 460 s. A push at a deploy's cutover was measured too (about
15 s more) and dropped: it can follow the last push by milliseconds,
where the counter guard (`pushApart`) takes a whole `pushEvery`
between pushes. setup writes the three
groups (`motel-store`, `motel-channel-failed`, `motel-operator`) empty
first, so vmalert starts before motel's first render. Every rule
is motel's text; nothing in the Caddyfile is an expression or a
template, and the operator's `rule` lines choose among motel's kinds
("Operator rules").

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
`/metrics` at its listener in the namespace, every 30 s. For each
receiver of the notify render, each integration among its channels, and
each operator channel that is not one of that receiver's channels of
that integration (notify's `suspects`) and whose `severity` floor takes
a warning (notify's `failureAudience`), the render writes a rule whose
labels are exactly those motel's own `ChannelFailed` posts for that
failure: `alertname`, `notify_channel`, `severity="warning"`,
`subject="box"`, `integration`, `suspects`. Receivers that share all
three are one rule (`receiver_name=~"a|b"`). Two posts with the same labels
are one alert in Alertmanager, so the two sources deduplicate; the
heartbeat receiver (no channel, so no suspect) is left to motel's own.
With no channel (a `metrics` line alone) every group is empty
(vmalert refuses a rule file with no group at all); so are they after a
reload takes the `metrics` line away ("The metrics line").

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

### What the store scrapes

The store scrapes itself, vmalert and Alertmanager every 30 s
(`scrape.yml`, which setup writes), and each job keeps only the series
motel reads from the store (`metric_relabel_configs`):

| Job | Keeps | Read by |
|---|---|---|
| `victoria-metrics` | `vm_free_disk_space_bytes` | `metrics_store_disk_fill` |
| `vmalert` | nothing (`drop` of `.+`) | — |
| `alertmanager` | `alertmanager_notifications_failed_total` | the second `ChannelFailed` |

The scraper adds `up` and its six `scrape_*` series after the
relabelling, so every job keeps those: `up{job="vmalert"}` is what
`metrics_rules_down` reads. The store's read-only flag is read from its
`/metrics` and never from a series ("The chain"), and the e2e suite reads
vmalert's counters from its own `/metrics` too. Alertmanager's counter is
kept on a box with one operator channel as well, which renders no
`ChannelFailed` rule: setup writes `scrape.yml` and the render does not,
so it cannot follow the channels.

Unrelabelled, the three jobs were over 2,000 series on hotserve-1;
this list keeps 82 of them. Measured on 2026-11-13 with two stores side
by side on a dev VM, each scraping itself, one vmalert and one
Alertmanager with hotserve-1's twelve receivers, every second for
2,880 s (a day's samples at the box's 30 s): the store's samples went
from 1.006 MB a day to 0.022, its index from 0.26 MB to 0.007, its RSS
from 84 to 72 MiB, and `ChannelFailed` and the disk rule still fired on
the kept data. Trimmed eagerly, since the two directions
differ: a series kept later is a line in `scrape.yml` and costs nothing
that reads the store, and one dropped later loses its history.
`TestScrapeKeepsWhatMotelReads` holds the list to motel's own queries
(every rule kind rendered, the chain's check of vmalert, `GET /<app>`'s
view and a post-deploy comparison, scanned for the series and job each
reads): a series kept that nothing reads fails it, and so does one read
that is dropped. `TestScrapeConfigPassesTheStoresCheck` has Debian's
`victoria-metrics` check the file with unknown fields refused
(`-promscrape.config.dryRun`, strict by default).

`motel monitor setup --metrics` writes the file at every run and
restarts the store, which re-reads it. A box set up before this list
keeps every series until setup runs again, and each load says so (the
`files … differ` warning names `scrape.yml`, and one that cannot read it says so); after it, the dropped
series age out over the 14 days' retention, and nothing that reads the
store reads them.

### Operator rules

The operator's own rules are lines of the monitor block beside its
`metrics` line, each one of motel's kinds with parameters checked at
parse time. No line holds an expression, a selector, a label or a
template:

```
monitor {
    metrics
    rule <name> threshold <metric> > | < <value> for <duration> [app <name>] [min_requests N] [severity warning|critical]
    rule <name> disk_fill within <duration> [severity warning|critical]
    rule <name> post_deploy [app <name>] {
        window        30m     # default 30m: whole minutes, 1m to 24h
        min_requests  100     # default 100
        max_5xx_share 0.02    # max_5xx_share, max_p95, or both
        max_p95       800ms
        failures      3       # default 3: 1 to 60, at most the window's minutes
    }
}
```

A rule line in a block with no `metrics` line is refused (`a rule line
needs the monitor block's metrics line`), wherever the line is; in JSON
the rules are the `metrics` object's (`apps.monitor.metrics.rules`), so
none can be written without it. A block with rule lines and no channel
is refused (`rule <name> reaches no one: the monitor block has no
channel`): it runs no notify ("The metrics line"), so nothing would
render its rules or hear a deploy.

What is checked (notify/operatorrules.go: the Caddyfile's lines at
parse, a JSON config's at Provision, both by `motel validate`):

- **The name** is `[a-z][a-z0-9_]{0,47}`, unique among the rules, and
  none of motel's own alert names: DESIGN-notify.md's reserved list
  (case ignored), `watchdog_*` and `metrics_store_*`. It is the alert's
  name and part of its key, the one thing of the author's that reaches
  a label.
- **The metric** is one of seven, each motel's expression (below), and
  its value is in the metric's own form: a share a plain decimal from 0
  to 1; a latency `800ms` or `1.5s`, from 1 ms to 10 s (the top of the
  histogram's buckets); `request_rate` a plain decimal (a second) up to
  1 000 000; `cpu_cores` up to 1024. A comparison that can never hold
  (`< 0`, a share `> 1`, a latency `> 10s`) is refused.
- **`for`** is whole seconds from 1 m to 24 h; **`within`** from 1 h to
  30 d; the severity `warning` (the default) or `critical`.
- **`min_requests`**, the sample floor, belongs to a share or a quantile
  of the responses (`5xx_share`, `p50`, `p95`, `cache_hit_share`;
  default 20): it is refused on `request_rate`, where a floor would hide
  the `request_rate <` it exists for, and on `memory_share` and
  `cpu_cores`.
- **`app`** is an app the liveswap block configures, checked at
  Provision, which reads liveswap's apps; without it the rule is every
  app's. A threshold's scopes are independent: an every-app threshold
  and an app's own on the same metric both apply to that app, each its
  own rule and alert, so an app's line adds to the every-app ones and
  never loosens or silences them (only post_deploy has replacement,
  below; "Accepted residuals"). A rule that names an app and could never fire for it is
  refused: `memory_share` of an app with no `memory_max` (the cap's
  series is pushed only for a capped app), at Provision; a request
  threshold or a post_deploy of an app no route observes (no `observe
  <app>` handler, so no request series), by cmd/motel's gate at
  `motel validate` and `motel reload` (`motel run` serves it, saying
  why), since only the whole config holds the routes and notify's
  Provision may run before the HTTP app's (/code-review pass 4 on
  operator rules).
- **post_deploy**: an unknown parameter is refused, and so is a rule
  with neither bound; there is one for every app and one per app, the
  app's replacing the other for that app, and a second for the same
  scope is refused.

| Kind | Expression | For | Labels | Reaches |
|---|---|---|---|---|
| `threshold 5xx_share` | `(sum by (app) (inc(motel_app_requests_total{class="5xx"})) / SENT) > v and SENT >= floor and on (app) PUSHED`, SENT the same over `class!="aborted"` | `for` | `app`, `subject="app:<app>"`, `kind="state"`, `severity`, `source="rules"`, `key="rules/app:<app>/<name>"` | the operator and the app's bound channels |
| `threshold p50`, `p95` | `histogram_quantile(q, sum by (app, le) (hinc(motel_app_response_seconds))) > v and sum by (app) (inc(motel_app_response_seconds_count)) >= floor and on (app) PUSHED`, v in seconds | `for` | as above | as above |
| `threshold request_rate` | `sum by (app) (inc(motel_app_requests_total)) / 300 > v and on (app) PUSHED` | `for` | as above | as above |
| `threshold cache_hit_share` | `(HITS or SPOKE * 0) / SPOKE > v and SPOKE >= floor and on (app) PUSHED`, both `sum by (app) (inc(motel_app_cache_responses_total))`, HITS over `result=~"hit\|stale"` | `for` | as above | as above |
| `threshold memory_share` | `max by (app) (motel_app_memory_bytes / motel_app_memory_max_bytes) > v` | `for` | as above | as above |
| `threshold cpu_cores` | `sum by (app) (rate(motel_app_cpu_seconds_total[5m])) > v` | `for` | as above | as above |
| `disk_fill` | `min by (path) (predict_linear(motel_disk_avail_bytes[L], H)) < 0 and min by (path) (predict_linear(motel_disk_avail_bytes[L/6], H)) < 0 and max by (path) (present_over_time(motel_disk_avail_bytes[L] offset 5L/6))`, H `within` in seconds, L 6 h up to a `within` of a day and `within`/4 beyond it, in whole minutes | L/12: 30 m up to a day | `path`, `subject="box"`, `kind="state"`, `severity`, `source="rules"`, `key="rules/box/<name>/<path>"`, each filesystem's alert its own key, as `disk_low`'s | the operator |
| `post_deploy` | motel's queries, below | — | the event `post_deploy_regressed`: `app`, `subject="app:<app>"`, `kind="event"`, `severity="warning"`, `source="rules"`, `key="rules/app:<app>/post_deploy_regressed"`, `release`, `deployed_by`; the rule's name as `detail_rule` | the operator and the app's bound channels |

With `app`, every selector carries `app="<name>"`, quoted (a name of
liveswap's own grammar); without, the expression is per app
(`by (app)`), and the `app` label it carries is what route 6 sends to
the app's channels, as for `app_memory_high`. The request metrics are
over a fixed five minutes, and the 5xx share is of the responses sent, as
`GET /<app>`'s is. `inc(X)` is a counter's increase over the window
(notify/operatorrules.go, `counterIncrease`):

```
((increase_pure(X[5m]) unless (present_over_time(X[1d] offset 5m)
      unless (last_over_time(X[1d] offset 5m) > first_over_time(X[5m]))))
  and (tfirst_over_time(X[5m])
      > scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[7m]))) + 15))
or increase(X[5m])
```

The store's `increase_pure` counts from zero whenever no sample comes
within about the window before it, and its `increase` drops a first
value that is large beside the next step (both measured on 1.112.0).
Neither alone is right: a series new to the store, or back below its
last value (a fresh counter under a release name the store saw within
the day: a rollback past the two releases the observer keeps), first
seen over half a push after the push's first sample in the window and
the two minutes (four pushes) before it, is one of motel's new ones (a
release's first push after its cutover, a class's first response, a
cache result's first), counted from zero, whose first value `increase`
would drop (22 responses read as 0, e2e metrics 3a). The push's first
sample is before the window when the pushes ran up to it, and inside
it on a store younger than the window (the first hour, or day, of
`GET /<app>`'s numbers after setup) or after a gap in the pushes over
the window's start; a gap inside the window leaves it before the gap,
so a series born in that gap is first seen after it and counted from
zero, rightly, since its birth is inside the window. Half
a push, 15 s at the 30 s push (`pushApart`, `pushEvery / 2`): one push
is one import, which the store stamps chunk by chunk with each chunk's
own time, so one push's samples can be milliseconds apart, and the next
push comes 30 s later. A series back at or above its last value after a
gap in the pushes of over five minutes (the store down, or read-only),
or one that came with the push's first sample (born, or motel
restarted, in a gap over the window's start; the metrics line first loaded on a box whose
observer had counted for hours), holds every count since motel began,
or since it did, which `increase_pure` read as the window's own for
five minutes (a 20-minute gap at 10 a push: 1 730 where 10 was due,
/code-review pass 4 on operator rules; a series born 2 minutes into a
20-minute gap: 360 to 450 where 100 was due, pass 5, when the push term
asked for a push anywhere in the day before). With no push in the store
at all, `scalar` is NaN, nothing compares above it, and every series
goes through `increase`. Where a sample comes just before the window
the two agree. What is left: a fresh counter back above the last value
its name had is read as a gap, and so is a fresh counter that comes
with the push's first sample after the store's start or a gap of over
two minutes, through `increase`, which drops a first push that is large
beside the next ("Accepted residuals"). The push term was a push in the
two minutes before the window until 2026-11-12; on a store younger than
the window that read e2e metrics 3a's 21 cache misses as 0 in
`GET /<app>`, whose windows are an hour and a day.
`hinc(H)`, the response histogram's (`histogramIncrease`), is `inc`
over `H_bucket` with its tests asked of `H_count` and matched
`unless ignoring(le)`: a histogram's buckets go one way together.
Told per bucket, a fresh counter under a name the store saw had some
buckets above their last values and some below, and a p95 of 0.1 s
read 0.97 s (/code-review pass 5 on operator rules).

`PUSHED` (`pushedNow`) is the apps motel's last push says the running
config observes:

```
max by (app) (tlast_over_time(motel_app_observed[75s]))
  > scalar(max(tlast_over_time(process_start_time_seconds{job="motel"}[75s]))) - 15
```

A series no push carries any more keeps its last value, and `increase`
over the window loses one push's worth at each push after it, so
without `PUSHED` an app whose `observe` line is taken out read as its
traffic falling, and an every-app `request_rate <` fired for it (owed
by #434, measured then on the live store at 2 requests a second: 1.8,
1.6 … 0.2 over the five minutes, nothing after; a `for: 1m` fired from
a bound of 0.5 R). A share or a quantile was read over the part of the
window left, though its summary says five minutes. An app is out
from the first push without its `motel_app_observed`, the push that
begins the fall: measured on 1.112.0 over imported pushes 30 s apart,
the app's 2.0 reads 1.8 from 30 s after its last push and the guard
holds it out from the same instant, a live app in it at every second.
Not the request counts themselves, which every observed app's current
release has, at zero before its first response: a restart's first
pushes can come before liveswap has reattached the release they count
under, and an app no one reaches would drop out, resolving its
`request_rate <` (/code-review of the guard). Its last sample is held
to the push's last less `pushApart`, since one push's samples are
milliseconds apart (and `motel_app_` sorts before `process_` in the
push). With no push in 75 s (two pushes and a half: one lost leaves
every app in), `scalar` is NaN and every app is out: motel down, which
motel's heartbeat tells, or every push failing, which
`metrics_store_read_only` tells for a read-only store and
`metrics_store_down` once they have failed for five minutes. Inside
the 75 s the readings fall for 45 s at most (from the first push
missed), which holds for one of vmalert's minutely evaluations, and a
`for` is a minute or more ("Accepted residuals" for what the gap does to an alert
already firing). `memory_share` and `cpu_cores` read the unit's
series, pushed while it runs whatever the routes observe.
CPU is `rate` over the per-unit counters, each from
zero when its unit starts. The summary says the bound and the value in
plain words: `shop: the 5xx share of its responses over 5 minutes has
been above 0.05 for 10m (rule errors_high); now 12.5%`. The threshold
and disk_fill rules are `rules.yml`'s third group, `motel-operator`;
post_deploy is motel's.

**disk_fill's shape** is the store's own rule's
(`metrics_store_disk_fill`: a day ahead over 6 h and 1 h, held 30 m) up
to a `within` of a day, and beyond it the same shape scaled to the
horizon: windows of a quarter and a 24th of it, held for a 48th (7d: 42
h, 7 h, 3 h 30 m; 30d: 180 h, 30 h, 15 h), all inside the store's 14
days. A regression is scale-free, so a one-off step in free space (a
deploy's extraction, a staged backup, an image pulled) must then be as
large a share of it to fire at 30 days as at one, about half of what is
free; over 6 h and 1 h, a 30-day horizon fired on a step of 2 % and a
7-day one on 8 % (simulated at the push's 30 s, and the 7-day case in
vmalert-tool; /code-review pass 4 on operator rules). The third term
wants a sample older than the long window's last five sixths: over less
history the windows are shorter than their shape and a step fires again,
so a rule waits for its history (5 h at a day or less, 150 h at 30d)
after the store's setup or a filesystem's first series. Any sample in a
long window before them (back to 330 h at 30d, inside the 14 days), not
one in the long window's first sixth alone: a gap in the pushes longer
than the short window (the store down for 2 h, at a day) left a hole
there when it slid into that sixth, five hours on, and a firing alert
resolved and fired again (/code-review pass 5). A bound on `within` was
the other way: it would refuse the week and month horizons a disk is
watched over. The `for` kept at 30 m would still let a step of a quarter
of the free space fire at 30d; a `for` longer than the short window
would stop any step, but a horizon of an hour would wait longer than
itself.

**`motel_disk_avail_bytes{path}`** is the box series `disk_fill`
reads: statfs's available bytes (df's Avail: available blocks of
`f_frsize`, read by the same helper as the disk source) for the directories
notify's disk source measures (the apps' root, Caddy's data, notify's
state directory), one series per filesystem, read at each gather, all
at once, each within 2 s: a path whose statfs has not come back (a
network mount whose server went away) has no sample, and no gather
starts another for it until it does, so a filesystem that hangs holds
back neither the push nor the admin /metrics. It is
named by the first of them on it, and keeps that name while the path is
measured: a stat of it that fails once leaves the series to the next
path on the device under the same name, so a rule's history is not cut
in two. A path measured on another device (a disk mounted on it) names
that device's series, and the one it left is named by its next path: a
name is one series' at a time, since two are a gather error (the push
drops the second, the admin /metrics answers 500).

**The post-deploy comparison** (notify/postdeploy.go) is motel's: vmalert
knows neither when the cutover was nor which release came before.
liveswap tells both at the swap itself, in a lifecycle event of its
own, `cutover`, which is no notification: `Previous`, the release whose
instance served until the new one was published (none after a first
deploy, or when that instance was not running: a crash loop's release
is no baseline; a relaunch names its own release), `Cutover`, when
the traffic moved, and `Relaunch`, set when the release published is
the one the app last ran (the instance it replaced, alive or not, else
the version `state.json` names when no instance is in memory). It is
told at that moment, from inside the publication, before the state's persist and the caps' sync, which can
wait on the manager (Copilot round 2 on #429), and before the old instance's
drain and stop and before the deploy's result (`deploy_succeeded`,
`rollback`), which a long `drain` would otherwise have started the
comparison after its window (Copilot round 1 on #429); nothing after
the swap fails a deploy. A deploy, a rollback and a relaunch tell it;
the watchdog's restarts and a motel restart's recovery publish an
instance without one, and start no comparison. `lifecycleSink` hands
it to the comparison alone, so `deploy_succeeded off` still compares.
A relaunch is one of two cases, told apart by liveswap's `Relaunch`
and `Previous`, not by comparing names (the operator's decision of
2026-11-12):

- **its instance still served** (`Relaunch`, `Previous` = the release:
  a deploy of the running version with the same artifact, or a
  rollback to it): it starts no comparison, and leaves one in flight
  for that release as it was. It is no new release, and both instances
  count into the release's one set of counters, so no window could
  tell the old instance's answers from the new one's (a baseline over
  both would take in the relaunched instance's first push, and could
  hide its regression).
- **its instance had stopped** (`Relaunch`, no `Previous`): a rollback
  to the release that had crashed (its instance dead, the watchdog
  throttled), or that nothing in memory runs (dropped after a refused
  relaunch, or a recovery that failed). Deploying it again is refused
  as a version that exists ("versions are immutable"); the rollback is
  the operator's or deployer's way to it. It is compared as a first
  deploy is, with the bound alone, and its event says it is a
  relaunch, not a regression (below). Its window starts 40 s after the
  cutover (`relaunchSkip`: a push, and the 10 s a push's import has):
  its counters are the ones the stopped instance counted into, and that
  instance's last answers before the cutover, a crash loop's 503s among
  them (a request that reached no upstream counts under the release
  current when it began), reach the store with the first push after
  it. The windows are cumulative, so read from the cutover they were
  the relaunch's in every check: 10 503s against 40 2xx at the first
  (`TestPostDeployARelaunchReadsPastTheStoppedInstance`, /code-review
  of the mark). From 40 s on, the window's first sample is a push
  gathered after the cutover, which holds them; the relaunched
  instance's answers before it are not compared ("Accepted residuals").

  Both cases are told from what holds at the swap: `Previous` is the
  replaced instance if it is alive then, and a relaunch taken while it
  served whose instance died before the swap is one after a stop. With
  no instance in memory nothing routes to the app, and the launch's
  sweep stops any unit the manager holds, so a relaunch `state.json`
  names had stopped too. A cutover with `Previous` = the release starts
  no comparison whether or not it carries the mark.

For an app with a rule (its own, else the every-app one),
for each whole minute e since the cutover up to `window`, both read at
cutover + e, 10 s after it: the store answers an instant query within
its `-search.latencyOffset` (5 s) of the present as of 5 s earlier,
which moved both windows back off the cutover (measured in e2e metrics
10: the old release's last half minute read as nothing), so the time
is given, past the offset:

- **the new release**: `sum by (class) (inc(motel_app_requests_total{app="A",release="NEW"}))`,
  and with `max_p95`, `histogram_quantile(0.95, sum by (le) (hinc(motel_app_response_seconds{app="A",release="NEW"})))`,
  `inc` and `hinc` the threshold rules' over `[<e>m]` (their guards' offsets moved by the window and any offset), for a relaunch after a stop over `[<e>m − 40s]`;
- **the old**, asked only once the new is over a bound: the same over
  `release="OLD"` with `[<e+30>s] offset <e−30>s`, its last e before
  the cutover and one push past it: its answers in the last half minute
  before the cutover reach the store with the first push after it.

The checks due at a tick run at once, each within the store's 10 s
budget, so a slow store does not stretch the minute between them.
A check is **skipped**, neither a pass nor a failure, while the new
release has fewer than `min_requests` responses sent (aborted left
out). The p95 is of the responses timed, the histogram's count (a cache
hit is sent and never timed), and a release has one only with
`min_requests` of them: on a cached site, a few misses after a deploy,
one of them a cold start, would otherwise make the quantile, and the
windows are cumulative, so the same slow request would fail every
check after it (/code-review pass 4 on operator rules). An old release
with fewer timed is no p95 baseline. One the store does not answer is **asked again** at the next tick
(a debug line, `the post-deploy check is asked again`), and the minutes
it held up after it in turn, one a tick, each read at its own time, so a
late answer is the one it would have been and failures equal to the
window's minutes stay reachable; a minute a stalled tick left behind is
made late the same way. A store that has not answered 10 minutes past
the window's end ends the comparison, with a warning line (`ended
unfinished`). It **fails** when the new release is over
`max_p95` with a p95 above 1.2 × the old's (a quantile is an estimate
inside a bucket), or over `max_5xx_share` and worse than the old: an old
release within the bound makes crossing it the regression; an old one
already over it needs a share above 1.2 × its own, and further above it
than chance puts two samples of one rate (a two-proportion z-score of
2), so an app that errs steadily is not told at every deploy. That is
this design's refinement of the planning's `new 5xx > old 5xx`
("Decisions"), which the operator confirmed on 2026-11-12. An old
release with fewer than `min_requests` responses sent in its window, or with none, is no baseline: the bound alone
counts. A pass resets the count; a check skipped neither breaks nor
extends it, so "in a row" counts the checks over the floor. After `failures` failing checks in a row, motel
reports one `post_deploy_regressed` through notify's engine (the filter,
the bucket, the journal line), and the comparison ends: `blog v2
regressed 3m after its deploy, over 240 responses: 5xx share 0.0833
against 0.004 before it; 3 failing checks in a row (rule regress)`, a share
to 1/10 000, a p95 to the millisecond. With no baseline, it says which:
`no release before it to compare`, or `the release before it sent 12
responses, too few to compare`; and for a p95 over its bound with no
old p95, `the release before it had 6 responses timed, too few to
compare its p95` (a cached site's old release can send enough and time
too few; Copilot round 3 on #429). A relaunch whose instance had
stopped says so in place of both: `blog v1 is over its bound 3m after
its relaunch (its earlier instance had stopped), over 240 responses:
5xx share 0.0833; 3 failing checks in a row (rule regress)`, with the
same name, labels, severity and audiences; a new release after a
stopped one keeps `regressed … after its deploy` and `no release
before it to compare`. The release before is not named:
the filter keeps the event's own release whole, and would mask another
git sha. It never acts: no refusal, no rollback (requirement 4). A new
deploy or rollback of the app ends the comparison in flight and starts
its own; the window's end ends it silently; a reload follows the config
serving at each check (a rule taken away ends it, a changed one applies
from the next check), and the last config's Stop drops what is in
flight. A motel restart mid-window loses it ("Accepted residuals").

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
config runs notify, so one told only when it changed could be lost).
While a fresh read has it set, the two things that follow from it are not told as something
else: the count toward `metrics_rules_down` does not run, and
`metrics_store_down` is not raised for the refused imports. One read
the store does not answer leaves that standing (a store at its floor is
slow); three in a row end it, since the store may by then be down, and
`metrics_store_down` says that.
 When the last config with a `metrics` line stops, the
chain's keys are reported cleared, so nothing fires on with no source. The
heartbeat (DESIGN-notify.md, "Dead-man and uptime") vouches for the box,
motel and Alertmanager; these three for the store and vmalert; the store's
own scrape of Alertmanager feeds the second `ChannelFailed`.

## metrics in GET /<app>

The deployer's route and credential, unchanged (the fresh-slate draft,
2.8): `GET /<app>` with the bearer JWT `deploy_trust` accepts. With a
`metrics` line loaded, the status gains:

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
(`MetricsRanges` in liveswap/wire, beside the object's type, `Metrics`); any other is a 422, on a box with no
store too. Seven fixed queries over the socket, scoped to `app="<name>"`
and run at once. Two `query_range` templates over the window, at a 60 s
step for the hour and 5 m for the day, the memory
pair (`{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",app=…}`)
and `sum(rate(motel_app_cpu_seconds_total{app=…}[2m]))` (`[5m]` for the
day, one step); one instant `max_over_time(motel_app_memory_bytes{app=…}[<window>])` for `max`, since a
60 s step holds one of the two samples a minute the push writes and a
peak between steps would be lost (Copilot on #423). `at` is the newest
point; `max` and `avg` are over the window; `now`, `avg_5m` and
`memory_max_bytes` come only from a newest point at most three steps
old (3 minutes for the hour, 15 for the day, whose newest step can be
over 5 minutes old for a running app), `avg_5m` being the mean of the
points in the five minutes up to it (the day's newest alone, a 5 m
rate already), so an app that stopped, or a cap a later release
dropped, is history in `max` and `avg` and never reported as the
present; a number with no points is absent (`max` too, when the window
holds no raw sample). The CPU averages are rounded to a millionth of a core: an
unrounded float is a run of twenty digits the response filter masks as
a generated value, and the status would be withheld.

`requests` is four instant queries over the window, each counter
through the operator rules' guard with the window as its own
(`inc` and `hinc`, "Operator rules"; notify/metricsview.go,
`requestQueries`): `sum by (class)` of
`inc(motel_app_requests_total)`, `histogram_quantile` at 0.5 and 0.95
of `sum by (le)` of `hinc(motel_app_response_seconds)`, and
`sum by (result)` of `inc(motel_app_cache_responses_total)`. For the
hour:

```
sum by (class) (((increase_pure(motel_app_requests_total{app="A"}[1h])
      unless (present_over_time(motel_app_requests_total{app="A"}[1d] offset 1h)
        unless (last_over_time(motel_app_requests_total{app="A"}[1d] offset 1h)
                > first_over_time(motel_app_requests_total{app="A"}[1h]))))
    and (tfirst_over_time(motel_app_requests_total{app="A"}[1h])
      > scalar(min(tfirst_over_time(process_start_time_seconds{job="motel"}[62m]))) + 15))
  or increase(motel_app_requests_total{app="A"}[1h]))
```

and the day's the same over `[1d]`, its push term over `[1442m]`.
Neither of the store's functions alone: a series appears with its
first push, already at that push's count, which `increase` drops when
it is large (measured on 1.112.0 in e2e metrics 3a: 21 cache misses
read as 0, so a hit share of 1; 22 responses under a series not pushed
at zero read as 0); and `increase_pure`, which these queries used until
2026-11-12, read a series' every count since motel began as the
window's own after a gap in the pushes over the window's start (a
20-minute gap at 10 a push: 4 990 where `increase` reads 990 of the
1 200 made), and for the first hour or day after the metrics line is
first loaded on a box whose observer had counted (7 800 where 600 came
in the store's half hour). Measured against the store in
`TestMetricsViewCountsOverTheStore`. On a store younger than the window
a series that comes after the push's first sample is counted from zero
(the 22 read as 22, at the day too); one that came with it, under
`increase`, reads the counts since that first push. The guard reads
each series three more times (`tfirst_over_time` over the window, and
the day before it twice) and the push series once: on a store holding
a day of ten apps at the push's 30 s (421 series, 1.3 million samples),
a day's request query took about 1 ms against about 0.5 ms for
`increase_pure` alone (2026-11-12, uncached). `per_second` is the total over the window's length;
`5xx_share` is of the responses sent (the total less `aborted`);
`cache_hit_share` is hits and stale hits of the responses a cache spoke
for. Every release in the window is summed: the status beside it names
the current one. Every float is rounded (shares to 1/10 000, rates to a
millionth, so that a day's few requests are not 0, milliseconds to a
tenth), for the CPU averages' reason. A
share or quantile with no response under it is absent, and so is
`requests` when the store holds no request series for the app.
The store answers 5 s behind the present (30 s on a store set up
before the 5 s, until setup runs again)
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
there is one package to suggest. **`motel monitor setup --metrics`**,
run by root, is the store's setup. On a box where Alertmanager's has
not run to its end (`motel-am.socket` not installed, not enabled or not
listening, or the service still masked: a first run that failed after
writing the socket unit), Alertmanager's runs first, as its recovery,
(DESIGN-notify.md, "Packaging and setup") and the store's only once it
has succeeded; where it has, Alertmanager's setup is not run, since it
masks and stops Alertmanager, which loses the alerts it holds in memory
and what waits in a group. The store's restarts it once, the first
time, so that it takes up its socket in the namespace (step 6), and not
again while that socket listens. `motel monitor setup` alone is
Alertmanager's. The store's:

1. refuses unless Alertmanager's setup has run (`motel-am.socket` is
   installed): vmalert's outlet is Alertmanager;
2. `systemctl mask victoria-metrics.service`, then `stop` it;
3. `apt-get install -y --no-install-recommends victoria-metrics` unless
   installed, with `iproute2` when it is not (it is on every Debian
   install: `ip` makes the namespace);
4. writes the drop-in, the namespace unit, the store's socket and its
   proxy, Alertmanager's socket in the namespace and its drop-in
   `prometheus-alertmanager.service.d/20-motel-metrics.conf`, and
   `motel-vmalert.service` (all in the binary,
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
5. on a box set up before Alertmanager held its own listener in the
   namespace, retires the proxy shape: `disable --now
   motel-am-proxy.socket motel-am-proxy.service` and removes both
   files, so that `127.0.0.1:9093` is free for Alertmanager's socket,
   and says so;
6. `daemon-reload`, `unmask`; then, unless `motel-am-metrics.socket` is
   active, one `systemctl restart prometheus-alertmanager.service` (a
   start, and said so, when it was not running)
   (its transaction starts the namespace and the socket first), and
   says which it did;
7. `enable --now` the namespace, the store, both sockets and vmalert,
   then `restart` vmalert and `try-restart` the store's proxy, so a
   re-run's units are the ones running (the store was stopped in step
   2, so its start takes the drop-in);
8. `try-reload-or-restart motel.service`, which renders.

Run again, it puts the store's units back as the binary ships them,
and Alertmanager's second socket and drop-in, and runs nothing of
Alertmanager's setup; `motel monitor setup` puts Alertmanager's own
back. A re-run
that fails after the mask (an install, a unit written to a full disk, a
reload) unmasks the store on its way out, starts `motel-vm.socket` (a
connection that arrived while the store was masked fails the socket),
and starts the store and vmalert again if the store was running, so a stack that worked is not left
masked and stopped; one that fails from step 5 on also restarts
Alertmanager if its socket in the namespace is not active, so vmalert
is not left with no outlet. A first run that fails before step 6's `unmask`
stays masked, since Debian's unit without the drop-in listens on
`0.0.0.0`, and one that fails after it is left as far as it got, every
unit already written ("Accepted residuals"). Going back to a build
whose store reached Alertmanager through the proxy, that build's
`setup --metrics` fails to start its proxy's socket, since
Alertmanager holds `127.0.0.1:9093` in the namespace, until root runs
`systemctl disable --now motel-am-metrics.socket` (which stops
Alertmanager), removes it and
`prometheus-alertmanager.service.d/20-motel-metrics.conf`, and runs
`systemctl daemon-reload` (measured by the second opinion,
2026-11-12). The drop-in
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
| `/etc/systemd/system/motel-metrics-netns.service`, `motel-vm.socket`, `motel-vm.service`, `motel-am-metrics.socket`, `motel-vmalert.service`, `victoria-metrics.service.d/10-motel.conf`, `prometheus-alertmanager.service.d/20-motel-metrics.conf` | 0644 | root | setup | systemd |

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
  bytes it sends through the store's proxy reach no root process ("The
  shape"), and what vmalert posts from the rules it renders reaches
  Alertmanager, as motel's own posts do.
- **T6** (a compromised config repository): can add or remove the line
  and move its socket path, and add or remove rules of motel's fixed
  kinds with their parameters, no more; no expression, query, selector
  or template is configurable. A rule reads only motel's expressions, of
  a configured app (quoted in) or every app, and reaches the audiences
  motel's own alerts about that app reach; its name, the one thing of the
  author's in a label, is held to one grammar and kept off motel's own
  names, so it cannot pose as one of them.
- **The namespace's daemons** (`_victoria-metrics`, Debian's packages):
  Alertmanager holds a listener in the namespace (`127.0.0.1:9093`,
  `motel-am-metrics.socket`), which gives whatever runs there
  Alertmanager's whole API, silences included, as the proxy it replaced
  did. Accepted: only the store and vmalert run there, and a compromise
  of either is a compromise of a root-installed Debian package, outside
  what motel defends. motel's own units give them no more: the store's
  proxy runs as no root ("The shape"), and nothing in the namespace
  holds the motel group, so the paths that group reads (the env files,
  the box's exchange tree) are not one step from them.
- The store holds label values motel chose (app, release, Caddy's
  server and handler names). A leak of it is low-sensitivity by
  construction; `GET /<app>` is app-scoped anyway, so a deployer never
  learns the box's shape from another app's numbers.

## Viewing

No new listener, no new credential kind, no session, as for
Alertmanager, and no UI: Debian's package is built without vmui, so
`/vmui/` answers `404 page not found`, though the store's index page at
`/` still links it (measured on 1.112.0 in the e2e box, 2026-11-12).
On the box, `curl --unix-socket /run/motel-metrics/vm.sock
http://vm/api/v1/query?...` (docs/monitor.md, "Look at the numbers",
has the queries). root, on a laptop: `ssh -L
/run/user/$UID/vm.sock:/run/motel-metrics/vm.sock root@box` for
command-line clients, or a short-lived `ssh -L 8428:/run/motel-metrics/vm.sock`
for a client that speaks only TCP, such as a laptop's own Grafana,
closed after (a TCP forward is an unauthenticated full-power API for
every process on the laptop). Neither forward is measured. A deployer
reads `GET /<app>`'s `metrics`; not the store, not the rules, not
other apps.

## Footprint

Measured on hotserve-1 (Hetzner arm64, 3.8 GB, Debian 13), the whole
stack under these caps (store `MemoryMax=256M` with the default 60 %
cache share, before `-memory.allowedBytes` was set) from 12:14 to 22:00
UTC on 2026-11-10, 2,600 series: the store 108 to 117 MB RSS
(cgroup 101 to 127 MB, peak 140 MB), vmalert 27 MB, each proxy 7 MB
while active and gone after five idle minutes, the store's disk 4.8 MB.
Alertmanager and motel unchanged. `-memory.allowedBytes=96MiB` holds the
caches under the 154 MB the default allowed, so the cap leaves room for
queries.

A day with that cap and motel pushing, on the same box from 15:00 UTC on
2026-11-11 (25 hourly samples, a cold boot at 15:04; the units of
9228935, before #426, whose changes, the proxies' users and the data
directories' modes, leave the store's memory as it was), 3,415 to 3,512
series:

| | Min | Median | Max |
|---|---|---|---|
| The store's RSS, from an hour after the boot | 174 MB | 186 MB | 191 MB |
| Its caches (`vm_cache_size_bytes`, summed), from the same hour | 101 MiB | 103 MiB | 116 MiB |
| vmalert | 27 MB | 28 MB | 28 MB |
| Alertmanager | 40 MB | 42 MB | 42 MB |
| Each proxy | 7 MB | 7 MB | 7 MB |
| motel | 63 MB | 66 MB | 67 MB |
| `GET /example` with its `metrics`, through the deploy host from the box | 22 ms | 31 ms (p95 34) | 37 ms |

The store reached its level within an hour of each start (107 MB three
minutes after setup restarted it, 174 MB an hour after the boot) and
moved 3 MB in the last twelve hours; its cgroup peaked at 203 MiB, 53 MiB
under `MemoryMax`. That is above the 156 MB it had reached on the default
share 16 hours after its start (2026-11-11 14:00, 3,264 series, before
motel pushed): the cap sizes the caches, which sit a little over it
(three of them at 32 MiB each, 96 MiB of the total), and not the rest of
the process; why the rest grew is not measured. The disk grew from 5.8 to
8.0 MB, 2.2 MB a day. No OOM kill, no restart, no failed unit. The
store's proxy no longer idles out, since motel's push keeps it busy: the
stack beside motel is about 270 MB. The response observer adds at
most 27 series per observed app and release (six classes, the
histogram's 17, four cache results), two releases each: 540 for ten
apps, against the 2,600 to 3,500 measured. The histogram had 14 when
the day above was measured; the three buckets under 5 ms (#434) are
three series more per app and release, about 60 for ten apps, which no
measurement since has taken.

Both days scraped the stack whole: over 2,000 of those series were its
own, which `scrape.yml` now drops ("What the store scrapes"); the store
under the kept list is not yet measured on hotserve-1. The caches'
`vm_cache_size_bytes` is no longer a series the store keeps: a later
measurement reads it from the store's `/metrics`.

## Decisions

Each is a choice the fresh-slate draft or DESIGN-notify.md left open,
or a line of theirs the facts overturned:

| Decision | Why |
|---|---|
| One block, `monitor`; the store is its `metrics` line | notify, the numbers and the rules over them are one idea to the person configuring the box; `metrics` is Caddy's own global option, so the store cannot be a block of that name; one global option yields one Caddy app, so the store is a part of the monitor app, not an app of its own. A `metrics_store` block is refused by name, and `motel metrics` exits 1 with the new spelling: unreleased, so no alias |
| The code is in the notify module | its rules are rendered from notify's receivers and its chain events go through notify's engine (the opening section) |
| No second package (`motel-metrics`); the units ship in the binary | slice one's precedent: the package installs nothing of the stack until root runs setup, so a second `.deb` would hold six unit files and a command |
| vmalert comes from `victoria-metrics` | Debian 13 ships `/usr/bin/vmalert` in it and has no `vmalert` package (`apt-cache search vmalert`, 2026-11-10) |
| The store's setup is `--metrics` on `motel monitor setup`, after Alertmanager's | vmalert's only outlet is Alertmanager, whose socket in the namespace is handed to the service Alertmanager's setup installs |
| `--metrics` runs Alertmanager's setup only where it has not run to its end | Alertmanager's setup masks and stops a running Alertmanager, which loses the alerts it holds in memory and what waits in a group: the store is added, or its units put back, without that; `motel monitor setup` puts Alertmanager's back when they need it. Its end is the socket enabled and listening and the service unmasked, not the socket unit's file, which a first run writes before it unmasks (Copilot round 1 on #428). /code-review pass 2 on the monitor block, reversing pass 1's every-time rule |
| `--metrics` restarts Alertmanager once, when its socket in the namespace is not active; it no longer leaves it as it runs | **Reversed on 2026-11-12** ("Alertmanager is left as it runs", with the proxy): Alertmanager takes its sockets only when it starts, and systemd refuses to start a socket whose service runs, so the listener in the namespace needs one restart. One `systemctl restart`, in one transaction, at a first run, an upgrade from the proxy, or an Alertmanager left with no listener there; a re-run with the socket active leaves it. The loss is the restart's, once per box, and setup says so |
| Alertmanager listens in the namespace itself (`motel-am-metrics.socket`), not through `systemd-socket-proxyd` | the proxy was a process of its own that held the motel group, and so needed a view built by hand (a static uid, motel as its primary group against the store's ptrace, `TemporaryFileSystem=/`) to keep that group's reach to one socket; and its socket, which only `multi-user.target` wanted, stayed down after a stop and return of the namespace, so vmalert's posts and the store's scrape of Alertmanager went nowhere, silently (S3 O4 D5 = 60, measured on 01b2fd5 and 09d2ed0). Alertmanager 0.28.1 serves every listener systemd passes; the cost is a restart of Alertmanager at setup's first run and at each restart of the namespace (an accepted row). The spike and a second opinion of 2026-11-12, then this PR's e2e rows |
| A start of the namespace restarts a running Alertmanager (`ExecStartPost=` `try-restart`), not a chain event on `up{job="alertmanager"}` | an Alertmanager that started while the namespace failed holds no listener there until it restarts (S3 O3 D5 = 45, fixed under the single-fault rule). The line ends it correct with no one acting, at the namespace's next start, which motel's push brings (size 2, risk 2: one line; the normal path's namespace start runs it as a no-op, with Alertmanager not running or starting in the same transaction, which e2e metrics 1 and 4d measure). Measured: with the line, 9093 answered 16 s after the fault went; with it cleared, 9093 was still dead 119 s on. A chain event (size 3: a new state in the chain's reads) would tell the operator and leave the fix to them; it would also tell a listener lost some other way, which no fault measured here produces |
| No vmalert `Watchdog` | the merge hazard above; motel watches the chain instead |
| `ChannelFailed` from vmalert is rendered per told channel with motel's labels | the only form in which Alertmanager deduplicates the two sources |
| The store keeps of its own scrapes only what motel reads (`scrape.yml`'s `metric_relabel_configs`), not the stack whole | a series kept later is a line added and breaks nothing; one dropped later loses its history. The kept list is held to motel's queries by a test ("What the store scrapes") |
| `MemoryMax=256M`, `-memory.allowedBytes=96MiB`, 14 days | the measured working set above; both caps mandatory, since the store sizes its caches from what it can see |
| The operator's rules are a grammar of fixed kinds with parameters (`threshold`, `disk_fill`, `post_deploy`), never an expression or a template | the review's finding 17: a rule or template a config author writes could pull any app's series, or the box's, into a notification that reaches an app's deployers (T6); a kind reads only motel's own expression, of the app named or each app for itself, so a config author chooses a bound and a duration, never what is read |
| disk_fill's windows and `for` scale with `within` beyond a day, and wait for their history | a regression extrapolated a hundred times past its window fires on a step; scaled, a step must be the same share of free space at every horizon ("Operator rules", disk_fill's shape). Not a bound on `within`, which would refuse the horizons a disk is watched over. /code-review pass 4 on operator rules |
| A request metric is over a fixed five minutes, with a floor of 20 by default | long enough for a small app to reach a floor, short enough that a rule fires within its `for` and five minutes; one window keeps the expression motel's. The floor keeps two requests, one of them a 5xx, from being a 50 % share |
| A relaunch of the release serving starts no post-deploy comparison; a relaunch of a release whose instance had stopped is compared with the bound alone and told as a relaunch | the first is no new release; the observer's counters are per release, so the old and new instance share them, and a comparison would need the counters split at the cutover (a new state in the observer) to keep the new instance's answers out of the baseline. A relaunch that regresses is told by the threshold rules (Copilot round 2 on #429). The second (a rollback to the release that had crashed; deploying it again is refused as a version that exists) has no baseline, but the bound still holds, so the warning stays, worded `is over its bound … after its relaunch (its earlier instance had stopped)`, not `regressed … after its deploy`. liveswap marks both on the `cutover` (`Relaunch`), so notify does not infer a relaunch after a stop from the names; a cutover with `Previous` = the release starts no comparison with the mark or without it. The deploy events carry no mark, since the comparison is the mark's one reader. The operator's decision of 2026-11-12; the second's window starts 40 s after the cutover, past the stopped instance's last push (/code-review of the mark) |
| The post-deploy comparison is motel's, not vmalert's, and only tells | vmalert knows neither the cutover nor the release before; #418 and requirement 4: motel never refuses a cutover or rolls back over a number |
| A post-deploy check fails only when the new release is over its bound and worse than the old one (p95: by 1.2 ×; a 5xx share, when the old was over the bound too: by 1.2 × and beyond chance, a z-score of 2) | the user's decision in slice three's planning: an app that was always so is not a deploy's regression; an old release under the floor is no baseline, so the bound alone counts after a first deploy or a quiet one. The 5xx margin refines the planning's `new > old` (/code-review pass 2 on operator rules): with none, an app erring steadily over its bound is told at a deploy whose share is higher by noise alone. The operator confirmed the refinement on 2026-11-12 |
| Per-app request numbers need a handler the operator writes, `observe <app>` | Caddy's request metrics carry server and handler, never the app; a Caddy app cannot add a handler to the HTTP app's routes, and a dynamic upstream sees no response. The app is named on the line, since a cache hit never reaches the upstream |
| The observer sorts before `redir` | the earliest standard directive an order can anchor on; `cache` sorts before `rewrite`, so an observer anchored later would never see a hit (cmd/motel's order tests) |
| Its counts follow the selected instance's release, else `current`, not `activeSocket` | a request selected after a cutover is the new release's, one selected before it the old's, however long it runs; and a release that crashed owns the errors that follow it (502s while its socket is routed, then 503s with no socket, so `current`), which is what a post-deploy comparison must see |
| Latency is to the headers, of the responses that went to the app or that the cache fetched from it | a streamed body or an upgrade has no response time to speak of; a hit is the cache's, a refusal penaltybox's |
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
| An app's threshold beside an every-app one on the same metric both fire for that app: the app's line adds a rule and cannot loosen or silence the every-app one, so when both hold its channels get two alerts | notify/operatorrules.go | 2 | 5 | 1 | 10; each alert is true and names its own rule; replacement (an app's rule taking the app out of the every-app one's selectors) is a feature for the operator to weigh, an explicit `replaces <rule>` the shape that neither breaks layered severities nor drops coverage silently; not built here (Copilot round 3 on #429) |
| An app whose route has no `observe` has no request numbers, and nothing says so at load | liveswap/observe.go | 2 | 3 | 2 | 12; `metrics` then has no `requests`, which a deployer sees; a load-time check needs the HTTP app's routes, which the liveswap app cannot read. A rule naming such an app is refused by cmd/motel's gate ("Operator rules") |
| An every-app `request_rate <` alert firing for an app whose `observe` line is taken out resolves at the first push without its `motel_app_observed` (`PUSHED`), a recovery the traffic did not make | notify/metricscollector.go (`collectRequests`), notify/operatorrules.go (`pushedNow`) | 2 | 5 | 3 | 30, accepted under the skill's rule 4: the rule no longer covers the app, and the alert ends rather than firing on numbers nobody measures any more, which it did before #434; telling the two apart needs a rule per app for an absent series (size 3, risk 2). /code-review pass 2 on #434 |
| The upgrade to the buckets under 5 ms (#434): a window that spans it holds the responses before it in the 5 ms bucket alone, so for up to a day `GET /<app>`'s p50 and p95 of a fast app read between 2.5 and 5 ms, and a post_deploy baseline read across the restart is high, so a regression right after the upgrade can read as no worse | liveswap/observe.go (`ResponseBuckets`) | 3 | 4 | 3 | 36, accepted under the skill's rule 4: once per box, at this upgrade, for at most the 24 h range; leaving the earlier samples out needs the store to know when a series' bucket set began (size 3, risk 2). /code-review pass 1 on #434 |
| `requests.per_second` is the window's total over the window's whole length, so in the first window after the observer begins counting an app's responses (`observe` added to its route, a new app) it reads low by the share of the window before; `total` counts the same responses | notify/metricsview.go (`summarizeRequests`) | 2 | 5 | 3 | 30, accepted under the skill's rule 4: the covered span needs the series' first sample (an eighth query, `tfirst_over_time`; size 3, risk 2), and that sample holds every count since motel began counting, an unknown span (hours, when the store is set up on a running box), so a rate over the covered span reads high where this one reads low. It ends correct once the series is a window old, and a deploy starts no new span: every release in the window is summed. /code-review pass 1 on slice three. The operator accepted it on 2026-11-12 |
| A miss Souin coalesced onto another request's upstream call counts under the release current when it began, not the one the call went to: across a cutover, the waiters of a call in flight at that instant can be counted under the other release | liveswap/observe.go (`markUpstream`) | 2 | 5 | 3 | 30, accepted under the skill's rule 4: only the first request's selection is made, inside Souin's singleflight, and the waiters learn nothing of it but the shared response; carrying it to them needs a change to Souin (size 3, risk 3). It is bounded to the misses on one URL waiting at the cutover instant, the same mix the cached-page row above accepts, and their latency is counted. /code-review pass 2 on slice three. The operator accepted it on 2026-11-12 |
| With no cache in front, an app's own `Cache-Status` is read as a cache's verdict: its own hits are counted in `cache_hit_share` and left out of the latency, so an app that marks every response a hit leaves its own latency out of its numbers | liveswap/observe.go (`parseCacheStatus`) | 2 | 5 | 3 | 30, accepted under the skill's rule 4: telling Souin's entry from an app's needs Souin's `cache_name` (the observer would read the cache app's configuration, a dependency liveswap does not have) or the request's path through the cache, which a stale hit's background revalidation shares (it runs on a clone of the request, vars and all); size 3, risk 2. The app's own cache is a cache that spoke, and an app moves only its own numbers (T1); the post-deploy comparison reads latency through it too, for both releases alike. /code-review pass 2 on slice three. The operator accepted it on 2026-11-12 |
| The counts a motel restart (or a crash) ends within the last 30 s were never pushed, and are lost | liveswap/observe.go | 1 | 3 | 4 | 12; at most one push's worth, and the store reads the restart as a reset |
| A fresh counter is read through `increase`, which drops a first push that is large beside the next (a burst, then a lull), in two cases the store cannot tell from a gap or from a lifetime: one that comes with the push's first sample after the store's start or after a gap in the pushes of over two minutes (a new release deployed in the gap, a class's first response in it: the series' first sample is the push's), and under a release name the store saw within the day whose fresh count is above the last it had (a rollback past the two releases the observer keeps). The window under-reads that push for its length: five minutes for a rule, an hour or a day for `GET /<app>` | notify/operatorrules.go (`counterIncrease`), notify/metricsview.go | 2 | 5 | 3 | 30, accepted under the skill's rule 4: both read the same as a series that holds counts from before the window, which `increase_pure` would count whole; telling them apart needs the series' age from motel (a label or a series per process and release on every request series: size 3, risk 3). An under-read of one push, against a lifetime read in its place. /code-review pass 4 on operator rules; the gap case pass 5's, the push term narrowed from a day to two minutes; narrowed 2026-11-12 from the first five minutes after the push first came, or came back, to the series that came with that push, so that a series born after it counts from zero on a store younger than the window (`GET /<app>`'s Owed row) |
| A gap in every push over 75 s (motel down, or the store refusing every push) takes every app out of the request thresholds, the shares and quantiles too: one pending starts again after it, one firing resolves (a recovery the traffic did not make) and, still over its bound, fires again after its `for` once the pushes are back | notify/operatorrules.go (`pushedNow`) | 2 | 3 | 3 | 18; it ends correct once the pushes are back (rule 2), and the cause is told by motel's heartbeat with motel down, by `metrics_store_read_only` for a read-only store, by `metrics_store_down` once the pushes have failed for five minutes (a gap of 75 s to five minutes otherwise told by nothing but the resolve); read through the gap, every app's readings fell a push's worth a push instead, and each `<` fired falsely for every app, while a share or a quantile was read over the part of the window left, whose summary says five minutes, and one guard holds for every request kind (/code-review of the guard, for the shares and quantiles) (the unscored note on #434's Owed row, 18 and fixed here as a cheap fix: the guard's push window, size 1, risk 1). Holding a firing alert through a gap is vmalert's `keep_firing_for`, a field per rule whose length no gap bounds. Scored with the guard (#434's Owed row) |
| Before liveswap has reattached an app's release after a motel restart (milliseconds, or its recovery's backoff while the manager does not answer), motel's pushes carry no series of that release: the app stays in `PUSHED` (`motel_app_observed`), so its request thresholds read the old process's series, falling a push's worth a push, and `GET /<app>` misses the responses counted meanwhile under no release | liveswap/appmetrics.go (`sample`), notify/metricscollector.go (`collectRequests`) | 2 | 2 | 3 | 12; it needs the restart and a recovery slow past a push or two, and the fall reaches a `for` of a minute only past about 90 s; counting under the release state.json names from the config's start, before the reattach, is a change to liveswap's recovery (size 3, risk 2). /code-review pass 2 on the guard |
| A motel restart (or a crash) inside a post-deploy window loses the comparison: nothing compares that deploy again, and a regression is told only by a threshold rule | notify/postdeploy.go | 2 | 2 | 3 | 12; the comparison is in memory, and keeping it across a restart is state on disk for a notification (size 2, risk 2); a restart rarely falls in the half hour after a deploy, and the numbers stay in `GET /<app>` |
| A relaunch after a stop is compared from 40 s after its cutover (`relaunchSkip`): the relaunched instance's answers before then are in none of its checks, so a regression over by then is not told by the comparison | notify/postdeploy.go (`newWindow`) | 2 | 5 | 3 | 30, accepted under the skill's rule 4: those answers come in the first push after the cutover with the stopped instance's last ones, under the release's one set of counters, and only counters split at the cutover (a new state in the observer, size 3, risk 3) tell the two apart; read from the cutover, a crash loop's last 503s were the relaunch's in every check. The threshold rules and `GET /<app>` count them. /code-review of the relaunch mark; the operator accepted it on 2026-11-12 |
| A relaunch after a stop whose first push after the cutover the store did not take within 40 s of the cutover (an import that failed, or a push gathered late behind a slow chain check) reads the stopped instance's last push as its own: a crash loop's last 503s in every check | notify/postdeploy.go (`relaunchSkip`) | 2 | 2 | 3 | 12; it needs the release's instance to have failed before (the crash loop) and the store to fail that one import; starting the window at the pusher's first import taken after the cutover is a state shared between the pusher and the comparator (size 3, risk 2). /code-review of the relaunch mark |
| vmalert's rules have no flapping guard: a condition that flaps around its threshold fires and resolves at each crossing | notify/metricsrules.go | 2 | 3 | 3 | 18; every rule has a `for:` of 2 to 30 minutes, which bounds a flap to one pair per `for:` |
| `GET /<app>`'s p50 and p95 each run the guarded histogram expression over every bucket, the view's heaviest, where `histogram_quantiles("phi", 0.5, 0.95, …)` would run it once | notify/metricsview.go (`requestQueries`) | 2 | 2 | 1 | 4; on a store of two days of ten apps at the push's 30 s (401 series, 2.3 million samples), one p95 took 1.6 ms at the day and 2.0 ms at the hour, both quantiles in one 1.3 ms and 1.8 ms (2026-11-12, uncached), against the 2 s budget; a view over it answers `the metrics store did not answer`. One query is a `phi` label to read (size 2, risk 2). /code-review of the view's guard |
| `GET /<app>?range=24h` reads two days through the guard (`[1d] offset 1d` for the series' earlier sample) and three more rollups a series | notify/operatorrules.go (`guardedIncrease`), notify/metricsview.go | 2 | 2 | 1 | 4; on the same store, the day's request query took 1.4 ms against 0.4 ms for `increase_pure` alone, the p95 1.6 ms against 0.7 ms, against the 2 s budget, and a view over it says so; a lookback sized to the window, or no guard when the push is older than the window, is a second expression to prove against the store (size 2, risk 2). /code-review of the view's guard |
| `now` and `at` in `GET /<app>` are judged by the range's step timestamps, not the samples': the store carries a sample forward onto the step grid, so a stopped app's or a dropped cap's last value can read as now for up to about one step past the three steps `fresh` allows (3 m for the hour, 15 m for the day) | notify/metricsview.go (`fresh`) | 2 | 3 | 3 | 18; the status beside it says whether the app runs; the source timestamps need a `timestamp()` query per series (size 2, risk 2). Copilot round 2 on #423 |
| vmalert's `ChannelFailed` has no baseline: the store's `increase()` counts a series' first sample from zero (measured on 1.112.0: first seen at 3 gives 3, and again after a 17-minute gap), so a failure Alertmanager counted before `motel monitor setup --metrics`, or before a store outage of over ten minutes, is told once as new, for five minutes | notify/metricsrules.go | 2 | 2 | 3 | 12; one late warning to the operator's other channels about a channel that did fail; motel's own source keeps its baseline. A gate on the counter's age changes the expression every channel's rule shares (risk 2) and wants a run against the store to prove. /code-review pass 3 on #423 |
| A failed re-run's put-back starts `motel-vm.socket` (and `motel-am.socket`) whether or not it was active when setup began: one an operator had stopped to keep the store stopped is started, and a re-run after a first run that failed past the drop-in starts the socket of a stack that never ran | notify/setup.go (`putBackOnFailure`) | 2 | 2 | 3 | 12; a setup that succeeds enables and starts the same socket, which is what the operator ran it for; with the drop-in in place the store it can start is confined to the namespace. /code-review pass 2 on #425 |
| A first run of either setup that fails after its own `unmask` (`disable`, `enable --now`, a `restart`, the motel reload) is left unmasked and maybe partly started, not masked | notify/setup.go, notify/metricssetup.go | 2 | 3 | 1 | 6; every unit and drop-in is written before the unmask, so what can run is motel's confined shape (Alertmanager on its socket with no cluster port, the store in its namespace); setup ends on its error naming the step, and the next run is a re-run, with the put-back. A remask there (size 2, risk 2) would mask a service whose socket may be enabled already, so a connection fails the socket: the state the put-back exists to undo. Copilot round 3 on #425 |
| A store that turns read-only while `metrics_store_down` or `metrics_rules_down` already fires leaves that warning firing beside the critical until the store is writable (the read-only path holds back new ones, and does not clear old ones) | notify/metricspush.go (`checkReadOnly`) | 2 | 2 | 3 | 12; the warning was true when raised and the critical beside it names the cause; clearing them there is two more branches in the path every read takes. Copilot on #425 |
| A reload that moves the `metrics` line to another socket starts the counts afresh (`setLive`) but keeps what already fires, `metrics_store_read_only` among them, until the new store answers: a read-only alert raised for the old store stays told while the new one does not answer | notify/metricspush.go (`setLive`) | 2 | 2 | 1 | 4; it needs the old store read-only and the new one not answering; the first answer of the new store clears it, and the new store's own silence fires `metrics_store_down`, which names the push failing, once `readOnlyStale` asks have gone unanswered (`heldReadOnly`, as for any read-only store that stops answering). Clearing it at the move would resolve it on no evidence. Copilot round 2 on #425 |
| `motel monitor setup --metrics` on a box where Alertmanager is not set up yet reloads motel twice, Alertmanager's setup's first, before the store is installed: a `metrics` line that this reload loads for the first time (written, never reloaded) starts the push's failure count then, so an install of the store slower than five minutes fires `metrics_store_down`, and the journal has a `not set up` and a `push failed` line, until the first push after the store's setup clears it | notify/cmd.go (`runSetup`) | 2 | 2 | 1 | 4; it needs Alertmanager not set up, the line unloaded before setup and a slow install; the alert names its cause and clears itself, and the two setups run as two commands had the same order; skipping Alertmanager's reload under `--metrics` is a flag through its setup (size 2, risk 2). /code-review pass 1 on the monitor block |
| A rule refused at Provision (an `app` the liveswap block does not configure, `memory_share` of an uncapped app, rules in a block with no channel, a rule naming an app with no liveswap block) names the rule, not its Caddyfile file and line: Provision reads the adapted JSON, which carries no position | notify/metrics.go (`provision`) | 2 | 5 | 1 | 10; the rule's name is unique in the block, so the refusal points at one line, and notify's own Provision refusals (an `operator` or `bind` naming no channel) name keys, not lines, alike; a position in each rule's JSON, set by the adapter alone, is a field every config carries (size 3, risk 2), and moving the no-channel check into the parse fixes one refusal of four (size 2). Copilot round 3 on #429 |
| e2e metrics 4 reads vmalert's send counters, `vmalert_alerts_sent_total` and `vmalert_alerts_send_errors_total`, which are per notifier and so every group's, not the ChannelFailed group's: a send error of another group's alert (an operator rule firing in the wait) fails the check, and another group's send raises the sent count | e2e/metrics/run.sh | 2 | 2 | 1 | 4; it needs another alert firing and Alertmanager refusing its post, which is a failure of the outlet the check exists to catch, and the fail line names the counters; vmalert 1.112.0 has no per-group send counter. The group's own evaluation count is the gate, and the sent count only corroborates. /code-review passes 1 and 2 on #430 |
| e2e metrics 4 no longer sees vmalert's post inside Alertmanager: it proves a send vmalert counted as accepted and one alert held, so a notifier pointed at another Alertmanager that answers 2xx would pass it | e2e/metrics/run.sh | 3 | 1 | 1 | 3; metrics 10 fails on it, naming it: it reads `errors_high`, which vmalert alone posts, in Alertmanager with vmalert's `generatorURL`. The outlet is `motel-am-metrics.socket`, handed to the Alertmanager that holds `/run/motel-notify/am.sock` (metrics 4d sees both in its one process), pinned by `TestMetricsUnitsAreTheMeasuredOnes`, and the store's scrape of Alertmanager (metrics 4's `up{job="alertmanager"}`) reaches the same listener; reading the post in Alertmanager without the phase race needs motel's post held off or both phases timed (size 3, risk 2). The merged alert's latest poster is still printed. /code-review pass 2 on #430; rescored from 15 by the review of the rebased tree |
| e2e metrics 4 takes vmalert's counters from one scrape, which is not one instant: vmalert writes the send-error counter earlier in the page than the group's evaluation count, so a second evaluation whose send fails between the two lines is counted with its error unseen, and a refused post passes | e2e/metrics/run.sh | 3 | 1 | 5 | 15; it needs Alertmanager refusing vmalert's post, the first evaluation's send having succeeded, and the failure landing in the microseconds between two lines of one page; one more scrape after the gate closes it (size 1, risk 2). /code-review of the rebased tree on #430 |
| A restart of the namespace restarts Alertmanager (its socket there is `PartOf=` it, and the socket the namespace's): what it holds in memory goes, a one-shot waiting in a group among it | notify/units/metrics/20-motel-metrics.conf | 3 | 2 | 5 | 30, accepted under the skill's rule 4: inherent to the design, since Alertmanager takes a socket only when it starts and a new namespace is a new socket; keeping it running needs the proxy back, whose own socket was the bug this replaced. The namespace is restarted by hand, never by setup or a push; the journal has the restart, and motel posts each firing state again within a minute. The second opinion on the namespace socket, 2026-11-12 |
| `motel monitor setup` (Alertmanager's) re-run masks Alertmanager while `motel-am-metrics.socket` listens: a connection there during the mask could fail that socket, and neither the setup's end nor its put-back starts it | notify/setup.go | 2 | 3 | 3 | 18; measured not to happen (the re-run with 9093 knocked every 50 ms throughout left the socket `success` and answering), and were it to, Alertmanager's next start through `am.sock` (motel reads its counters every minute) starts the socket again (`Wants=`); starting it from Alertmanager's setup is a branch on the store's units in a setup that knows nothing of them (size 2, risk 2). /code-review pass 1 on #435 |
| `--metrics` now restarts a running Alertmanager once: one that runs on a config it could not load again (a render it refused at its reload, a storage path gone unwritable) does not come back, and notify is down until the console | notify/metricssetup.go (`amListenInNamespace`) | 4 | 2 | 1 | 8; two faults, an Alertmanager whose next start would fail and a setup run meanwhile; setup ends on the restart's error with systemd's words, and Alertmanager's own journal names the cause. Checking its config first (`amtool check-config`) is a probe of a state motel's own renders already hold to (size 2, risk 2). /code-review pass 1 on #435 |
| A failed re-run whose own restart of Alertmanager failed restarts it once more in the put-back, and says the failure twice | notify/metricssetup.go (`then`) | 2 | 2 | 1 | 4; the second try is a retry of the step that failed, and setup ends on the error either way; remembering that the restart ran is a state carried into the put-back (size 2). /code-review pass 1 on #435 |
| A re-run over the proxy shape that fails after the new units are written but before the proxy is retired (a full disk at the state directory) leaves both shapes on disk: the put-back's reload gives Alertmanager the drop-in, whose socket then fails at its next start (the proxy holds 9093), and the proxy carries vmalert's posts as before | notify/metricssetup.go | 2 | 3 | 1 | 6; setup ends on the error, and its next run finishes the upgrade; vmalert keeps its outlet throughout. Retiring the proxy before the writes instead leaves no outlet after the same failure. /code-review pass 1 on #435 |
| A re-run over the proxy shape whose `daemon-reload` is refused after the proxy is retired (systemd refuses one with `/run` nearly full, or past `ReloadLimitBurst=`) leaves the manager on the units it had: the put-back's own reload is refused too, and its restart of Alertmanager takes the definition without `20-motel-metrics.conf`, so at worst Alertmanager comes back on `motel-am.socket` alone and vmalert's posts to 9093 have nowhere to go until the next run | notify/metricssetup.go | 4 | 3 | 1 | 12; setup ends on the reload's error, and the put-back says it failed too, naming `systemctl daemon-reload`: "put back" is said only when the put-back's reload got through, and then the new units are loaded and its restart gives Alertmanager its socket in the namespace. The next run once reloads work finishes the upgrade. Not measured: the fault persists, so the put-back's message is the same whatever systemd starts from memory (vmalert's put-back start on its old definition may bring the proxy's socket back). The reload before the retirement keeps the proxy through the same failure (size 1, risk 2: the upgrade's order changes under its test). Copilot round 2 on #435 |
| A re-run rewrites `motel-am-metrics.socket` but, with it active, restarts neither it nor Alertmanager: a socket whose `ListenStream=` or namespace a later binary changed keeps the old listener until the namespace restarts or the box reboots, while setup says Alertmanager was left as it runs | notify/metricssetup.go (`amListenInNamespace`) | 2 | 2 | 3 | 12; no binary has changed it, and `motel-vm.socket` and the namespace unit are alike (a re-run leaves them running too: the Owed row's `ExecStartPre`); a restart of the socket when its file changed is a comparison and a new step, to measure (size 2, risk 2). /code-review pass 2 on #435 |
| `metrics_store_disk_fill` reads `vm_free_disk_space_bytes` with no `job` matcher, and `TestScrapeKeepsWhatMotelReads` gives a read with no job to the job its prefix names (`vm_` to `victoria-metrics`): a second source of a `vm_*` series would join the rule's `min` and pass the test | notify/metricsrules.go, notify/metricssetup_test.go (`readJob`) | 2 | 2 | 3 | 12; the store's one other writer is motel's push (`job="motel"`, `motel_*` names) and vmalert's remote write (`ALERTS*`), so it takes a change that adds a source and a series of that name. `{job="victoria-metrics"}` on the rule changes an expression the vmalert-tool tests pin (size 1, risk 2). /code-review pass 1 on the keep list |
| `TestScrapeKeepsWhatMotelReads`'s scan refuses a selector with a single-quoted matcher (`x{job='motel'}`), which MetricsQL accepts: the test fails on a valid query of motel's until it is written with double quotes | notify/metricssetup_test.go (`storeReads`) | 2 | 5 | 1 | 10; red and named (`matcher "job='motel'"`), never a false pass, and every query motel writes uses Go's `%q` or literal double quotes; single quotes in the matchers' split and unquote are a second quoting path to test (size 2, risk 1). /code-review pass 2 on the keep list |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| The store's RSS on a 1 or 2 GB box (a day on hotserve-1's 3.8 GB is in "Footprint") | — | — | — | a deploy to a box of that size |
| On a booted Debian host, the namespace unit's test for a leftover before its delete (#430's `ExecStartPre`): setup's re-run does not restart the namespace unit, which is active, so it first runs at a reboot; measured in the e2e box alone. #426's proxies have had their cold boot: the deploy of 01b2fd5 to hotserve-1 rebooted it at about 17:53 UTC on 2026-11-12, and both came back as no root; the am proxy is gone since #435, and the cold boot of the units that replaced it is the last dashed row | — | — | — | the next reboot of hotserve-1 after a deploy that carries #430 |
| Debian's security support for `victoria-metrics` 1.112.0 and `prometheus-alertmanager` 0.28.1 on trixie, whose binaries are built with Go 1.24.2 (the store, vmalert) and 1.24.1 (Alertmanager), and a dist-upgrade check of both drop-ins' flags | — | — | — | before any release |
| A cold boot on hotserve-1 with Alertmanager's socket in the namespace, its drop-in and the namespace unit's `TimeoutStartSec=` and `ExecStartPost=`, and the upgrade from the proxy shape there (measured in the e2e box alone: "The shape") | — | — | — | the next deploy to hotserve-1, `motel monitor setup --metrics` after it, and a reboot |

## e2e rows

`make e2e-metrics` (e2e/metrics/run.sh): a box built from notify-box's
image with Debian's `victoria-metrics` installed and masked
(e2e/Dockerfile, `metrics-box`), and the same stub receiver. The suite
runs `motel monitor setup` and then `motel monitor setup --metrics`
itself and asserts: nothing of the store set up without `--metrics`,
and with it the store's setup alone (Alertmanager not masked), the
first run restarting Alertmanager once (one start in its journal, 9093
answering in the namespace after) and saying so, a second run leaving
it as it runs (the same invocation, no start) and saying so;
no TCP listener on 8428, 8880 or 9093 in the host namespace, all three
in the namespace; the socket `srw-rw---- root motel` and the state
directory `drwxr-s--- motel _victoria-metrics`; the store's data
`drwxr-x--- _victoria-metrics _victoria-metrics`; root and motel reach the
store and nobody does; the store's and vmalert's `MemoryMax`; the
rendered `rules.yml` passes `vmalert -dryRun`; notify's thirteen tests
over vmalert, vmalert-tool and the store's own binary, which `make test` skips for want of them
(the dry runs, the fixed rules firing, the threshold and disk_fill
rules firing, a counter after a push gap, a counter on a fresh store,
the request thresholds over the pushed apps alone, a histogram read
whole, disk history over a gap, a disk step, `GET /<app>`'s request
queries over the store, a relaunch read past its stopped
instance, and `scrape.yml` checked by `victoria-metrics` with unknown fields refused), every test that calls `vmTool`
(`TestE2EMetricsListsEveryVMAlertTest` holds the list to them), run
from a test binary built for the box (e2e/Dockerfile's `notify-tests`), none skipped; motel's push arrives
(`job="motel"`), with a deployed app's `motel_app_up`, release and CPU,
and its memory where the host's memory controller reaches the user
manager's units (Debian 13 delegates it; a CI container's host may not,
and the suite then asserts the series absent); `GET /<app>` with the
deploy token carries `metrics`, and without one is a 401; an observed, cached site's
responses counted by class, cache result and latency, and a site of an
app with nothing running counted as 5xx; `GET /<app>?range=24h` carrying
`requests`; at the hour and the day, on the store minutes old, the
store's own 2xx count and the cache misses counted (a hit share under
0.1, where `increase` alone read 1); and `?range=7d` a 422; vmalert's
rules loaded, and its `ChannelFailed` for the failing channel firing
with motel's labels while Alertmanager holds one such alert, not two; the store keeping of each job only `up` and what motel reads (its free space, Alertmanager's failed-notification counter, nothing of vmalert's), and its free space read live;
the store's proxy running as a uid not root with no capability;
one Alertmanager process holding `am.sock` and the namespace's
`127.0.0.1:9093` and answering on both (metrics 4d); after a SIGKILL,
started again on both by the next connection; a restart of the
namespace restarting Alertmanager once, 9093 answering in the new
namespace; a stop of the namespace stopping Alertmanager and its socket
there with `am.sock` listening on, then the namespace and 9093 back with
nothing done by hand and the store scraping Alertmanager again (the
proxy's socket stayed down here: 22 s for the namespace, 9093 still dead
75 s on, measured at 09d2ed0); the namespace failing to start,
Alertmanager started on `am.sock` alone, and once the fault is gone the
namespace's start restarting it onto 9093; with motel down, the
namespace stopped and started by hand and the store started, vmalert's
start starting the socket and the store's scrape Alertmanager through
it (without vmalert's `Wants=`, measured on 6471dee: the socket and
Alertmanager inactive 150 s on, two of vmalert's sends refused, back
only at motel's start); the store scraping Alertmanager again after the
namespace's restart;
the store stopped by hand, started again by motel's next push with vmalert beside it;
the store made read-only (its free-space floor set above any disk): no sample of its free space kept from then while one from before is (the flag itself is no series it keeps), and motel's `metrics_store_read_only` is in Alertmanager, critical;
a reload to a monitor block that is a `metrics` line alone writing no
alerting rule, vmalert loading none, and its warning;
motel's shutdown leaving `rules.yml`'s alerting rules, and a reload
without the `metrics` line (and its rule lines) writing none; a restart without it
writing none too, and vmalert loading zero alerting rules after;
the operator's rules (e2e/metrics/Caddyfile's four `rule` lines): the
`motel-operator` group in `rules.yml` through the counter guard (`inc`), keyed
`rules/<subject>/<name>`, with no post_deploy in it, vmalert loading
it, an every-app `5xx_share` threshold firing for shop (whose site
answers 503) and reaching Alertmanager with `app="shop"`,
`source="rules"`, its key and vmalert's `generatorURL`; every app
`motel_app_observed` named in the last 75 s in `PUSHED` at each second
of two minutes of the pushes as the store stamped them, blog and shop
throughout; and a deploy of
`demo-errors` (healthy, every page a 500) after the probe release
producing one `post_deploy_regressed` for blog, release `errs1`, with
`5xx share 1 against 0 before it`, delivered to the operator's channel
and blog's deployer's;
the view probe's three metrics keys closed from inside a sandboxed app;
the store's socket stopped, `GET /<app>` answering at once (13 ms
measured) with the error and the rest of the status whole, and the
`push failed` line; an upgrade from the proxy shape (its two units
planted, Alertmanager on `am.sock` alone): `--metrics` retiring both
(stopped, disabled, removed) and saying so, then restarting
Alertmanager once, which holds 9093 itself (metrics 11). First run 2026-11-10 on the lima arm64 VM: the
two sources' `ChannelFailed` held by Alertmanager as one alert, with
vmalert's `generatorURL`.
Not in e2e (unit-tested only): `metrics_store_down`'s five minutes and
`metrics_rules_down`'s three reads, the cache's 30 s; the post-deploy
comparison's old-is-worse guard, its p95 bound, a pass resetting the
count, a redeploy cancelling it and its window's end (a fake store and
clock); `disk_fill` firing (vmalert-tool over input series, as the
threshold kinds are too).

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
- 2026-11-11 — Slice three's response observer, `observe`:
  request series per release in the push, `requests` and `?range=` in
  `GET /<app>`, whose queries now run at once (the serial-queries row
  goes); the request path measured before and after.
- 2026-11-11 — One block, `monitor`: `metrics_store` is the monitor
  block's `metrics` line, the store a part of the monitor app, and
  `motel metrics setup` is `motel monitor setup --metrics` ("Decisions").
- 2026-11-11 — The operator's `rule` lines: `threshold` and `disk_fill`
  rendered into `motel-operator`, `post_deploy` evaluated by motel; the
  two slice-three rows of "Owed" close.
- 2026-11-11 — /code-review pass 1 on operator rules, triaged: rules in a
  block with no channel are refused, a dead instance is no release's
  baseline, the post-deploy checks due together run at once, a disk
  series keeps its path's name, a refusal of two rule lines names its
  line, `max_p95` kept to the nanosecond.
- 2026-11-12 — /code-review pass 2 on operator rules, triaged: the cutover
  is the swap, the old release's window runs a push past it, a late tick
  makes the window's last check, a 5xx rise over an old release already
  over its bound must be beyond chance, and the disk series counts
  `f_frsize` blocks, read by the disk source's own statfs.
- 2026-11-12 — Copilot round 1 on #429, triaged: the comparison starts at
  liveswap's `cutover`, told at the swap, not with the deploy's result
  after the drain.
- 2026-11-12 — Copilot round 2 on #429, triaged: `cutover` is told from
  inside the publication, before the persist and the caps' sync; a
  relaunch of the release serving starts no comparison.
- 2026-11-12 — /code-review pass 3 on operator rules, triaged: a path
  moved to another filesystem names that one's disk series, a disk
  statfs that hangs holds back no gather, an observed app's current
  release is pushed at zero (`request_rate <` for an app no one has
  reached), a post-deploy check the store did not answer is asked again,
  the regression's summary says why there was no baseline, and a
  disk_fill alert's key names its filesystem.
- 2026-11-12 — /code-review pass 4 on operator rules (another model's), triaged:
  a request counter after a push gap, or one that came with the push, is
  not read as its lifetime (`counterIncrease`); disk_fill's windows and
  `for` scale with `within` beyond a day and wait for their history; a
  post-deploy p95 needs `min_requests` responses timed; a rule on an app
  it could never fire for is refused (`memory_share` uncapped, at
  Provision; a request rule unobserved, by the gate); a post-deploy
  query is read to the millisecond; notify's vmalert tests run in the
  e2e metrics box. Two rows accepted, `GET /<app>`'s counters owed.
- 2026-11-12 — /code-review pass 5 on operator rules, triaged: the guard's
  push term asks for a push in the two minutes before the window, not
  the day, so a counter born or reset in a push gap is not read as its
  lifetime (and the over-a-day row closes); a histogram's buckets are
  told new or fresh by its count, as one; disk_fill's history term
  takes any sample before the long window's last five sixths, so a push
  gap leaves no hole; the e2e metrics box runs every vmalert test, held
  to the list by a test. No row added; the fresh-counter row widened to
  the first five minutes after a gap.
- 2026-11-12 — Copilot round 3 on #429, triaged: notify's disk source
  runs its statfs as the disk series' do, so a filesystem that hangs
  holds back no poll of the engine (DESIGN-notify.md); a regression's
  summary says when the release before had too few responses timed for
  a p95; a refusal at Provision names its rule and not its line, a row.
  an app's threshold beside an every-app one on the same metric both
  fire (S2 O5 D1 = 10), a row; whether an app's rule should override,
  and how, is a feature for the operator to weigh, and "Operator rules"
  now says a threshold's scopes are independent.
- 2026-11-12 — `GET /<app>`'s Owed row closed: its four request queries
  read the counters through the operator rules' guard (`inc`, `hinc`)
  with the window as theirs, so a gap in the pushes over the window's
  start, or the metrics line first loaded on a box whose observer had
  counted, is not read as the window's own (4 990 and 7 800 under
  `increase_pure`, 990 and 600 now). The guard's push term is now the
  push's first sample in the window and the four pushes before it,
  half a push before the series' own, not a push in those four: on a
  store younger than the window, as e2e metrics 3a's is, a series that
  comes after the first push counts from zero (21 misses had read as 0
  through `increase`, a hit share of 1), for the rules too; the
  fresh-counter row narrows to the series that came with that push.
  Measured against the store in `TestMetricsViewCountsOverTheStore` and
  `TestOperatorRulesCountOnAFreshStore`, both in the e2e list.
- 2026-11-12 — The operator's decisions on #429: a relaunch of a
  release whose instance had stopped (a rollback to the release that had
  crashed) is still compared with the bound alone, and its
  `post_deploy_regressed` says `is over its bound … after its relaunch
  (its earlier instance had stopped)`; liveswap marks a relaunch on the
  `cutover` (`Relaunch`), and the comparison reads the mark, not
  `Previous` = the release. The refined 5xx rule (1.2 × and a z-score of
  2 over an old release already over its bound) confirmed. #427's three
  rows scored 30 (`per_second` low for a young series, a coalesced miss
  counted under the release current when it began, an app's own
  `Cache-Status` read with no cache in front) accepted.
- 2026-11-12 — /code-review of the view's guard and the relaunch mark
  (ten findings): a relaunch after a stop is compared from 40 s after
  its cutover (`relaunchSkip`), since the stopped instance's last
  answers, a crash loop's 503s among them, come with the first push
  after it under the same counters and were read as the relaunch's in
  every check (10 503s against 40 2xx at the first, measured against
  the store); a cutover with `Previous` = the release starts no
  comparison whatever its mark. Both cases are told from what holds at
  the swap; a relaunch `state.json` alone names had stopped, nothing
  routing to it and the launch's sweep stopping any unit (the finding
  that said otherwise did not hold). e2e metrics 3a reads the store's
  2xx beside each view. Accepted: the relaunch's first 40 s (30,
  accepted by the operator), a first push the store did not take (12), p50 and
  p95 apart and the day's two-day scan (4 each, measured at 1.3 to
  2.2 ms a query against the 2 s budget). The `inc` text on a gap inside the window corrected.
- 2026-11-12 — A day on hotserve-1 with the store's caps and motel
  pushing, and a cold boot ("Footprint", "The shape"); the two rows that
  owed them narrowed to what is left. The namespace unit deletes a
  leftover only when there is one, so a start with none logs no error; e2e
  metrics 4 counts vmalert's post at its send, not by the merged alert's
  `generatorURL`, which motel's post can hide.
- 2026-11-12 — /code-review of #430's rebased tree (ten findings),
  triaged: a reading of vmalert must hold the send-error counter's
  line, which a renamed counter had read as no error (30); the fail
  line says which reading failed; the namespace check looks for
  `motel-metrics` alone before setup. The Owed row asks for the reboot
  the new `ExecStartPre` needs. Accepted: a send error between two
  lines of one scrape (15); the other-Alertmanager row rescored to 3,
  since metrics 10 sees vmalert's post in Alertmanager.
- 2026-11-12 — **The am proxy is gone**: Alertmanager holds
  `127.0.0.1:9093` in the namespace itself (`motel-am-metrics.socket`,
  its drop-in `20-motel-metrics.conf`), after a spike and a second
  opinion. It fixes the proxy's socket staying down after a stop and
  return of the namespace (S3 O4 D5 = 60). Setup restarts Alertmanager
  once, retiring the proxy on an upgrade; the namespace's start restarts
  an Alertmanager left without its socket there (45), and its start is
  bounded to 30 s. "Alertmanager is left as it runs" reversed; the
  proxy's sandbox (its uid, primary group and view), its e2e checks and
  `TestE2EProxyViewHidesTheMotelPaths` go with it.
- 2026-11-13 — Copilot on #435: a start of the namespace alone does not
  start Alertmanager's socket in it (measured: with motel down, both
  stayed inactive until motel came back); vmalert `Wants=` the socket.
  A restart was measured to reach it (`Requires=`), as e2e 4d had it.
  Its round 2, accepted (12): a `daemon-reload` refused after the
  proxy's retirement can leave vmalert no outlet, red and named.
- 2026-11-13 — #434's Owed row closed (#442): the request thresholds
  read the apps motel's last push says are observed (`PUSHED`,
  `pushedNow`, over a new `motel_app_observed`, pushed for every
  observed app from the config's first push, since a restart's first
  pushes can come before liveswap reattaches the release the counts are
  under), so an app whose `observe` line is taken out is out from the
  first push without it, as its readings first fall, and no
  `request_rate <` fires for it; the shares and quantiles too, which
  read the part of the window left. The row's unscored note, every push
  stopped, scored 18 and fixed as a cheap fix (the guard's push window
  75 s): every app is out then, and what that does to a firing alert is
  a row (18). Measured over imported pushes on 1.112.0 and in
  `TestOperatorRulesReadThePushedApps`, in the e2e list; e2e metrics 10
  holds every pushing app in the guard at each second of two minutes.
- 2026-11-13 — The store keeps of its scrapes only what motel reads
  ("What the store scrapes"): its free space, Alertmanager's failed
  counter, `up`; 82 of over 2,000 series on hotserve-1. A test holds
  the list to motel's queries, and each load warns of a `scrape.yml`
  setup wrote before.
