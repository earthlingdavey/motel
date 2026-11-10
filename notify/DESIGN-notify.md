# DESIGN — notify

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- notify/DESIGN-notify.md`);
the "History" section at the end holds only dated one-liners.

**Status: proof of concept, slice one** (branch
`poc/notify-alertmanager`): the notify core. No metrics store, no
vmalert, no `metrics` in `GET /<app>`; those are slice two, designed in
"Later slices" and not built. Normative for what is built: a change
that cannot hold a line here changes the line in the same PR, with
the reason.

`notify` is who motel tells, about what. **motel detects; Debian's
Alertmanager delivers.** motel's detectors (the watchdog, the deploy
handler, the backup record, CertMagic's events, the filesystems) post
alerts to an Alertmanager that listens on a unix socket only, and
whose whole configuration motel renders from one Caddyfile block. It
is its own module (`notify/`), importing liveswap (the apps' deploy
trust, the response filter) and the backup record, never the other way
round: liveswap reports to a `Notifier` interface and knows nothing of
notify. The requirements are #418's and #137's; #418's own
"Decisions" were an in-process design this one replaces ("History").

## The requirements

From #418 and #137, without their solutions:

1. **Events at once**: #418's first events ("Events"), each a state
   alert with one recovery message or a one-shot; `critical` pages,
   `warning` and `info` report; a one-line summary with no secret and
   no log tail.
2. **Routing by audience**: box events to the
   operator's channels; app events to the operator's channels and the
   channels bound to the identities the app's `deploy_trust` trusts;
   nobody else. A binding to an identity no app trusts is refused.
3. **The mapping is a committed line**, never a token claim or
   self-registration. Channel URLs are secrets on the existing age
   path, readable by the motel uid, never printed.
4. **Slack, Discord, ntfy and a json webhook**, with retries; a failed
   delivery is a journal line, a counter and an event to the operator's
   other channels.
5. **Behaviour**: firing and resolved with one recovery, a short wait
   to batch a burst, a repeat while something stays wrong, a flapping
   guard, and a deploy mutes its own app's watchdog alerts.
6. **Dead-man and uptime are external**: the heartbeat's value is its
   absence.
7. **External producers** (cron, smartd, the WordPress profile's
   services) emit events with the same semantics.
8. **Nothing is lost silently**: every event is a journal line before
   delivery.

## The shape

motel's own footprint is unchanged by the module: 64.2 MB RSS (22 MB
cgroup) on hotserve-1 with the block loaded, against 64.3 MB for 0.6.1;
Alertmanager beside it 42.6 MB RSS (24 MB cgroup).

| Piece | What | Where |
|---|---|---|
| Alertmanager | Debian 13's `prometheus-alertmanager` 0.28.1: routing, grouping, dedupe, repeat, resolved-once, inhibition, retries, the notification log, 18 receiver kinds | Debian's unit with motel's drop-in (`notify/units/10-motel.conf`): socket-activated, `--cluster.listen-address=` (no gossip), `--enable-feature=receiver-name-in-metrics`, `MemoryMax=128M` |
| Its listener | `/run/motel-notify/am.sock`, 0660 root:motel, a systemd socket unit (`notify/units/motel-am.socket`); no TCP port | its own runtime directory: `/run/motel` is motel.service's `RuntimeDirectory`, removed whenever motel stops |
| Its config | `alertmanager.yml` and one `url-<KEY>` file per channel secret, 0640, rendered by motel at every config load | `/var/lib/motel-notify`, 2750 motel:prometheus (setup makes it); not `/var/lib/motel`, which is 0750 motel:motel and the prometheus uid cannot traverse |
| motel's part | the `notify` block and its refusals, the renderer, the engine (state table, flapping guard, mute, heartbeat, retry queue), the detectors, `channel_failed`, two admin routes, three commands | `notify/` |

**Who reaches the socket, and why.** Every deployed app runs as the
motel uid, so the socket's group is not what keeps apps out. Their
sandbox view is: an app unit's file view is deny-by-default and
`/run/motel-notify` and `/var/lib/motel-notify` are in
`sandboxMotelPaths` (liveswap/sandbox.go), exactly as `/run/motel` is
not in any app's view (DESIGN-threat-model.md, "Reverse proxy, admin
socket, penaltybox"). The group keeps out every other system user
(`prometheus`, `nobody`, an exporter). The view probe reports the three
paths (`notify_run`, `notify_socket`, `notify_state`) and the e2e
suite asserts them closed on a box where they exist.

## The notify block

```
notify {
    channel <name> json|ntfy|slack|discord {secret.NOTIFY_<NAME>} [severity critical|warning|info]
    operator <channel>...
    bind <identity> <channel>
    heartbeat {secret.NOTIFY_<NAME>}
    deploy_succeeded on|off          # off by default (#418's table)
    allow_insecure_channels          # test rigs only; warned at every load
    secrets_file <path>              # default /etc/motel/secrets/_box.env
    state_dir <path>                 # default /var/lib/motel-notify
    alertmanager <socket path>       # default /run/motel-notify/am.sock
    backup_record <path>|off         # default /var/lib/motel-backup/status.json
}
```

A URL is only ever `{secret.NOTIFY_<NAME>}`: a Slack or Discord hook is
a credential by itself, and the Caddyfile is a commit anyone who reads
the repository reads. The value is in the box tier's env file. An
identity is the `deployed_by` grammar read as a trust source:
`local:<public key path>` or `oidc:<issuer> <claim>=<value>...`; it
matches an app's `deploy_trust` source when the label is the same and
the source pins every named claim to the same value
(`deploytrust.Source.Label`, `PinnedClaim`). What a token merely
carries (`ref=`, `actor=`) matches nothing, and an oidc identity with no
claim is refused, since it would match every repository on the issuer.

What `motel validate` refuses (Provision, which writes nothing and
connects to nothing), each naming keys and channels, never a value:

| Condition | Refused as |
|---|---|
| a literal URL, or `{env.*}` | `a channel URL is written {secret.NOTIFY_<NAME>}, its value in the box's secrets, never in the Caddyfile` |
| a secret not named `NOTIFY_…` | `the secret's name is NOTIFY_ followed by capitals, digits and underscores` |
| no `operator` line | `no operator line: name the channels that receive every event with operator <channel>` |
| `operator` naming no channel line | `operator names <c>, which no channel line defines` |
| a channel defined twice | `channel <c> is defined twice` |
| `bind` to a channel that does not exist | `bind "<id>" names channel <c>, which no channel line defines` |
| `bind` to an identity no app's `deploy_trust` trusts | `bind "<id>": no app's deploy_trust trusts this identity; remove the line, or trust it` |
| a secret the box does not hold | `channel <c>: secret <KEY> is not in the box's secrets (<file>)` |
| a URL not https, not to a public address, or to a local name (`localhost`, `*.local`, `*.internal`, a single label) | `channel <c>: the value of <KEY> is not an https URL` / `… is not to a public address …` / `… is not to a public host name` |

The URL check is every URL's, not only a deployer's: Alertmanager sends
from the network namespace every app shares, so a URL to loopback or a
private address would reach a listener an app opened.

## Secrets

The values are read from `/etc/motel/secrets/_box.env` (`KEY="value"`
lines, liveswap's env-file format, `NOTIFY_` keys only), 0640
root:motel, bound into no unit. Every value is a known value to
liveswap's filter, which every summary passes through. motel writes
each value once more, to its `url-<KEY>` file in the state directory,
because Alertmanager's `*_file` fields read a file: that is the copy
the prometheus uid reads, and the config itself holds paths only.

**Owed** (see "Owed"): the box channel writing `_box.env` from a
signed push's `secrets/_box/NOTIFY_*.age` (#418's reserved slot, which
reverses box/DESIGN-box.md open question 5), and deployers' channels
from `secrets/<app>/NOTIFY_*` withheld from the app's environment.
Until then root writes `_box.env` by hand, as the e2e image does.

## The route tree

Rendered by `render` (notify/render.go); the golden file is
`notify/testdata/alertmanager.golden.yml`. In order:

| # | Route | Receiver | Timing |
|---|---|---|---|
| 1 | `alertname="Watchdog"` | `heartbeat` (the heartbeat's `url_file`), or a blackhole | `group_by [alertname]`, wait 0 s, interval 5 m, repeat 4 m (under the interval, so each 5 m flush re-sends) |
| 2 | `notify_channel=~".+"` (ChannelFailed, NotifyTest), one child per channel | `ch-<channel>` | `group_by ['...']`, wait 0 s, interval 1 m |
| 3 | `alertname="deploy_in_progress"` | blackhole: it exists to inhibit | — |
| 4 | `kind="state", alertname="disk_low"` | the operator's, by severity | wait 30 s, interval 1 m, repeat 24 h |
| 5 | `kind="event"`, a child per app with channels of its own, each with `severity` children | `event-<box\|app-a>-<severity>` | `group_by ['...']`, wait 0 s, interval 1 m |
| 6 | `kind="state"`, the same children | `state-<box\|app-a>-<severity>` | wait 30 s, interval 1 m, repeat 4 h |
| root | anything else (nothing motel posts) | `fallback`, the operator's | — |

Plumbing first, so nothing below can swallow it; no `continue:`
anywhere. An app's receiver is **composed**: the operator's channels
plus every channel bound to an identity the app trusts, each only at
or above its floor; an app with no binding has no child and its events
take the kind's own receivers, the operator's. State receivers
`send_resolved`; event and direct ones never do. The inhibit rule:
`source_matchers [alertname="deploy_in_progress"]`, `target_matchers
[alertname=~"watchdog_.*"]`, `equal [app]`, with no severity matcher
(#418's watchdog events are critical). Slack and Discord get a fixed
title and text built from motel's summary; json and ntfy the payload
whole. Nothing in the block is a template.

The timings are measured (2026-11-10, hotserve-1, Debian 13 arm64):

| Fact | Measured | Consequence |
|---|---|---|
| Events grouped `alertname, app, subject` | of four distinct one-shots posted 5 s apart, two delivered: the second in a live group waits for the next tick, its `endsAt` passes, and with `send_resolved: false` it is never sent | `group_by ['...']`; one-shots carry `endsAt` +5 m |
| Events with `group_by ['...']`, wait 0 s | four distinct one-shots delivered 0 ms after their posts (the stub's clock); an exact duplicate deduplicated | the event route |
| A recovery on a 5 m `group_interval` | delivered 290 s after the resolve | state routes take 1 m: measured 50 s |
| `endsAt=now` on a firing alert | resolved at once (the younger resolved alert wins the merge) | motel resolves explicitly |
| Default route | 30.02 s `group_wait` | the state wait |
| Event latency, post to the webhook's request | 6 to 7 ms; to a real ntfy.sh topic, 1.6 s | — |
| `POST /-/reload` over the socket | 200, live config replaced | how motel applies a render |
| `amtool` | has no unix-socket support | tests run `amtool check-config` and `amtool config routes test` offline on the rendered file (thirteen label sets) |
| The corrected tree with two apps | every alert reached only its receiver; `watchdog_*` of the deploying app suppressed, `inhibitedBy` the mute | e2e reproduces it ("e2e rows") |

## Events

`Event` (notify/engine.go) is what a detector reports. Its key is
#418's, `<source>/<subject>/<name>[/<instance>]`, and names what is
wrong, never when. Labels: `alertname`, `kind`, `severity`, `subject`
(`box` or `app:<a>`), `key`, `source`, `app` (an app event only: it is
what routes to the app's channels, so a box event about an app names
it in `instance`), `instance`. A one-shot also carries `release` and
`deployed_by` as labels; a state alert carries them as annotations, so
a deploy does not change its identity.

| Event | Source | Kind | Severity | Fires | Resolves |
|---|---|---|---|---|---|
| `watchdog_throttled` | liveswap | state | critical | the restart budget is spent; again on any failed probe or crash during a recovery's hold, below the restart threshold too | the first healthy probe after it, held `max(30s, watchdog_grace)`; a deploy that succeeds; `watchdog off` |
| `relaunch_refused` | liveswap | state | critical | the watchdog's relaunch is refused at its pre-flight | any path publishes an instance |
| `deploy_failed` | liveswap | event | warning | a 5xx outcome; the summary names the version and the phase, never the error's text | — |
| `deploy_refused` | liveswap | event | warning | a 422 (liveswap's own words, through the filter) | — |
| `rollback` | liveswap | event | warning | `?rollback=` succeeds | — |
| `deploy_succeeded` | liveswap | event | info | a deploy succeeds; dropped unless `deploy_succeeded on` | — |
| `deploy_in_progress` | liveswap | the mute | none | a deploy starts; re-posted each minute while it runs | it ends, whatever it came to |
| `backup_stale/<app>` | backup | state | critical | the app's last complete backup is older than `status.StaleAfter` (3 h) | it is fresh |
| `backup_run_failed` | backup | state | warning | the last run ended early, or an app's class is anything but ok or pending (`motel-backup run`'s own rule for a failed run) | a run ends with every app ok or pending |
| `drill_failed` | backup | state | critical | the last drill could not begin, the check found damage, or an app's latest drill proved nothing (`App.RestoreDrill`, as `status` reads it); the summary names the apps, never the drill's detail | the next drill that proves every app |
| `backup_unreadable` | backup | state | warning | the record does not parse | it reads |
| `cert_failed/<identifier>` | tls | state | warning; critical with under 7 days left | CertMagic's `cert_failed` | its `cert_obtained`, or 13 h with no report (a name no config manages any more) |
| `disk_low/<path>` | disk | state | warning at 80 % used, critical at 90 | statfs, every minute, one per device, of the liveswap root, Caddy's data and the state directory | five points under its threshold |
| `Watchdog` | notify | heartbeat | none | every minute, alive 4 m | — |
| `ChannelFailed` | notify | direct | warning | Alertmanager's failure counter rose ("Failed delivery") | `endsAt` +15 m |

liveswap reports through `liveswap.Notifier` (liveswap/lifecycle.go),
on the goroutine where the event happened. notify subscribes to
`cert_failed` and `cert_obtained` by name through Caddy's events app.
The backup record and the disks are polled by the engine. A box with
no backup record has no backups set up and says nothing.

## Behaviour

The engine is one per process and outlives a config load, as
liveswap's app pool does; each config's Start points it at its
Alertmanager, and the Stop of the config it serves ends its goroutine.
Its pollers are the engine's too, so a key one fired is cleared by
whichever config serves when it clears.

- **Journal first.** Every event is a `notify: event` line (key,
  state, severity, summary, source, release, deployed_by) before its
  post is queued. Re-posts, the heartbeat and the mute are not events.
- **State.** A key fires once and is re-posted every minute while it
  holds, with no `endsAt`, so `resolve_timeout` (5 m) ends it if motel
  stops posting. Cleared, it is held for `max(30s, the detector's
  hold)`; fired again inside the hold, no recovery is told. Then it is
  posted with `endsAt=now`. A key that changes severity (a disk from
  80 to 90 %) ends its old alert in the post that fires the new one.
- **The flapping guard.** Every key's transitions spend from a bucket
  of four, refilled one per 15 minutes: a flapping key sends four at
  once, then one per quarter-hour, so up to about eight in its first
  hour and four an hour after. Past it a
  one-shot is suppressed and a state change held, each with a line.
- **Unreachable Alertmanager.** Posts wait in a bounded in-memory
  queue (512), retried with backoff from 2 s to 30 s for five minutes,
  then given up as `notify: delivery failed` (error level for an
  event) and `motel_notify_delivery_failed_total` on the admin
  `/metrics`. A refusal (4xx) is given up at once; a 5xx is retried like a refused connect. Socket activation
  restarts Alertmanager in about a second, so the usual outage is one
  failed connect.
- **The mute** is `deploy_in_progress` for the app, posted at a
  deploy's start, re-posted each minute, ended at its end.
- **Summaries** pass through liveswap's filter, primed with every
  channel URL (and, at liveswap's end, with the app's own values), with
  the release as a safe name (a git sha is otherwise masked by the
  entropy layer), and are cut to 200 runes.
- **A config that drops a source** ends what nothing can clear any
  more: the keys of an app it no longer runs (and its mute), the backup
  keys once `backup_record off`, a filesystem's once its path is not
  measured; each fired key is resolved with a journal line. An
  unreadable backup record keeps what fired firing beside
  `backup_unreadable`, and a filesystem statfs cannot measure keeps
  what it fired.
- **Reloads.** A reload Alertmanager does not answer is a warning, and
  the config counts as unacknowledged, so the next load reloads it
  though the file has not changed. A reload that fails after notify's
  Start stops the new config while the old one serves on; the engine
  goes back to the old one, and puts the old one's render back in the
  state directory and in Alertmanager. When the last config running
  notify stops, the engine's table, mutes and buckets are emptied: a
  block added back later starts clean.
- **A certificate no config names** is never obtained and never
  reported again, so no `cert_obtained` clears it. CertMagic reports
  `cert_failed` on every failed attempt for a name it manages, and its
  longest retry interval is six hours; a `cert_failed` key unreported
  for 13 hours is taken as cleared, with a line, and resolved after the
  hold.
- **The watchdog turned off** while a restart loop is reported ends the
  report (`watchdog_recovered`): no probe would ever say so.

## Failed delivery

Once a minute motel reads Alertmanager's own `/metrics` over the
socket, with Prometheus's text parser (`prometheus/common/expfmt`,
already in the binary through Caddy's metrics). The read is strict: a
body cut short, a malformed line, or one without the failed counter is
no reading, and the baseline stays the last good one. `alertmanager_notifications_failed_total` names the integration
and, with the drop-in's `receiver-name-in-metrics`, the receiver. A
rise (the first read is a baseline; a lower value is a restart,
counted from zero) is a `notify: channel failed` line, and one
`ChannelFailed` alert to each operator channel that is not a suspect,
each routed to that channel alone (`notify_channel=`). The suspects are
the failing integration's channels in that receiver (`render` returns
each receiver's channels), or in every receiver when the counter names
none. A key's bucket bounds the storm. This needs no metrics package:
the review's point that a notify-only box would otherwise have no
`channel_failed`.

## Dead-man and uptime

motel posts `Watchdog` every minute; its route sends it to the
`heartbeat` channel's URL every five minutes, a Healthchecks ping
(`https://hc-ping.com/<uuid>`, Period 5 min). The route's repeat (4 m)
is under its group interval (5 m): Alertmanager re-sends at a flush only
when its last send's log entry, stamped after the send returned, is
older than the repeat, so an equal repeat skipped every other flush and
pinged every ten minutes (Fable review of #422, from Alertmanager
0.28.1's dedupe stage). Silence means the box,
motel or Alertmanager is down, which is the chain that delivers
everything else. Uptime stays an external keyword check per host.

## External producers and tests

`motel notify send` posts to `POST /notify/events`, and `motel notify
test <channel>` to `POST /notify/test`, both `admin.api.notify` routes
on the admin socket: root and the motel uid reach it, no app does.
External producers never post to Alertmanager's socket on the
documented path; root can, as root can do anything. Both the command
and the route check (`SendRequest.Check`):

| Refused | Why |
|---|---|
| a source not `[a-z0-9-]{1,32}` (stamped `external:<name>`) | one grammar, so a key cannot pose as a detector's |
| a subject not `box` or `app:<an app the box runs>` | an app subject reaches its deployers |
| motel's own names: `Watchdog`, `ChannelFailed`, `NotifyTest`, `deploy_in_progress`, `watchdog_*`, the deploy events, `disk_low`, `cert_failed`, `backup_*`, `drill_failed` | a producer that could post them could mute an app, fake the heartbeat, or speak as a detector |
| a summary empty, on two lines, or over 200 characters; details over 2 KiB or with names not `[a-z][a-z0-9_]*` | #418's limits |
| a body with unknown fields (a raw Alertmanager label set) | the route takes the request, never labels |
| a name or instance the filter would mask (a channel URL, a secret-shaped token) | both are the key, the labels and the answer, which the filter does not rewrite |

Accepted, an external event is one like motel's own: the filter, the
bucket, the journal line, the queue. `motel notify test` posts one
`NotifyTest` alert with a nonce label, routed to the channel alone,
then reads the channel's receiver's counters for 15 s: delivered when
an attempt did not fail (`notification_requests_total` rose more than
`notification_requests_failed_total`; `notifications_total` is raised
before the first attempt and says nothing of the outcome), failed when
Alertmanager gave up (with its reason class), or neither yet; it exits
1 unless delivered, and never prints the URL. An external producer's
`--detail key=value` pairs are annotations `detail_<key>`, through the
filter.

## Packaging and setup

The `.deb` **Suggests** `prometheus-alertmanager`. A `Depends` cannot
close the window in which Debian's unit starts on install (`:9093`,
gossip on `0.0.0.0:9094`): apt may configure a dependency, and run its
postinst, before motel's own scripts run, and `Pre-Depends` makes that
order certain rather than preventing it. On a box with no firewall
(hotserve-1 has none) that is the alert and silence API on the
internet. So **`motel notify setup`**, run by root, does it in an order
motel controls:

1. `systemctl mask prometheus-alertmanager.service`, then `stop` it:
   one installed by hand before setup runs on Debian's ports, and a
   mask stops nothing, nor would the socket be handed to a service
   already running;
2. `apt-get install -y --no-install-recommends prometheus-alertmanager`
   unless installed (measured: the masked install left nothing
   listening);
3. writes the drop-in and the socket unit (both ship in the binary,
   `notify/units/`), `/var/lib/motel-notify` 2750 motel:prometheus with
   a config that sends nothing (kept if motel has rendered one), and
   Alertmanager's storage directory;
4. `daemon-reload`, `unmask`, `disable` the service (its socket starts
   it), `enable --now motel-am.socket`;
5. `try-reload-or-restart motel.service`, which renders.

Run again, it puts the units back as the binary ships them. Debian
upgrading or reinstalling the package, whose postinst restarts the
unit directly, was measured on hotserve-1 (2026-11-10, 0.28.1
reinstalled under motel 0.6.2~notifypoc1): the socket's fd is passed
(`Listening on systemd activated listeners`), nothing listens on 9093
or 9094, and `motel notify test` delivers at once; the postinst
re-enables the service, so it then starts at boot rather than at its
socket's first connection, still on the socket alone. A reboot with motel rendering (2026-11-10, hotserve-1,
13.2 s to multi-user): `motel-am.socket` listening at 11.9 s,
Alertmanager started at 12.0 s on the rendered file left from before,
motel's render and reload at 12.8 s, motel ready at 12.9 s; no ordering
cycle, no listener on 9093 or 9094, a heartbeat at once and `motel
notify test` delivered. Two unit facts were measured too: `motel-am.socket` must name its service
(`Service=prometheus-alertmanager.service`), or systemd refuses it; and
Alertmanager needs Debian's `/usr/share/prometheus` templates, so the
package is installed rather than a binary fetched.

## Paths

| Path | Mode | Owner | Writes | Reads |
|---|---|---|---|---|
| `/run/motel-notify/am.sock` | 0660 | root:motel | systemd (the socket unit) | root, motel |
| `/var/lib/motel-notify/` | 2750 | motel:prometheus | setup (makes), motel | Alertmanager |
| `…/alertmanager.yml`, `…/url-<KEY>` | 0640 | motel:prometheus (born in the setgid dir) | motel, each by rename | Alertmanager |
| `/etc/motel/secrets/_box.env` | 0640 | root:motel | root (the box applier, owed) | motel |
| `/etc/systemd/system/motel-am.socket`, `…/prometheus-alertmanager.service.d/10-motel.conf` | 0644 | root | setup | systemd |
| `/var/lib/prometheus/alertmanager/` | 0750 | prometheus | Alertmanager (the notification log, silences) | Alertmanager |

## Threat model

New: a resident daemon motel did not write, reachable by unix-socket
permission alone, holding outbound credentials in files only it and
motel read; and two admin routes.

- **T1** (a poisoned dependency in an app): the socket, the state
  directory and the admin socket are not in its view; it runs as the
  motel uid, so the view, not the mode, is the gate (sandboxMotelPaths,
  the probe's three keys).
- **T2** (a stolen deploy token): can deploy, and so cause
  `deploy_*` events and the mute for that app only; the bucket bounds
  a storm of refusals; it cannot post, silence or read alerts.
- **T5** (RCE in motel): can post any alert, silence anything and
  rewrite the rendered config, which its own in-process notify could
  have done; through `*_file` it could point Alertmanager at any file
  the prometheus uid reads and send it out. The renderer writes only
  fixed fields and `*_file` paths in its own directory, so this is
  T5's reach, not a config author's.
- **T6** (a compromised config repository): can repoint channels, so
  every summary is built to carry no secret and no log tail, and every
  URL must be https to a public address. A signer can already run
  anything as an app.
- Channel URLs are in no config, no API echo and no journal (the
  `*_file` form; measured on hotserve-1 with planted tokens:
  Alertmanager's failure lines print `Post "<redacted>"`; the e2e
  suite greps both journals, the rendered file and `/api/v2/status`).

## Viewing

No new listener, no new credential kind, no session. Alertmanager's UI
and API have no authentication of their own and are reached only
through the socket: root by `ssh -L 9093:/run/motel-notify/am.sock
root@box` (OpenSSH forwards to a remote unix socket since 6.7), then
`http://localhost:9093` or `amtool --alertmanager.url`; on the box, the
`motel notify` commands or `curl --unix-socket`. A deployer sees
notifications at their channel; not Alertmanager's UI, other apps, box
events or silences.

## Triage

Findings are triaged by the triage skill (`.claude/skills/triage/SKILL.md`)
on its general scales. notify's examples:

| Axis | Examples |
|---|---|
| **Severity 5** | an event lost silently (no journal line and no delivery); a channel URL printed; an app reaching the socket |
| **Severity 4** | Alertmanager unable to start after a render until the console |
| **Severity 3** | a recovery told while the condition holds; an event to the wrong audience |
| **Severity 2** | a duplicate or late message |

### Accepted residuals

| Residual | Where | S | O | D | Why accepted |
|---|---|---|---|---|---|
| A public host name that resolves, or rebinds, to a private address is posted to: the check is on the URL as written | notify/config.go (`checkURL`) | 1 | 5 | 5 | a channel URL is a signer's line today, and a signer can already run any code as an app that reaches the same addresses; scored by what T6 gains (nothing). Rescore when #417 lets a deployer choose a URL |
| motel stopped for more than five minutes: Alertmanager resolves each firing state alert at `resolve_timeout`, and the channels get a recovery that did not happen | notify/engine.go (no `endsAt` on re-posts) | 2 | 3 | 3 | 18; a stop that long is itself a fault the heartbeat reports (Watchdog's silence pages first); keeping the alerts alive across a stop needs state on disk (size 3) |
| Two channels of one integration in one receiver: a failure names both as suspects, and neither is told | notify/channelfailed.go (`suspects`) | 2 | 5 | 3 | an operator's choice of channels; the line names both, and the counter is Alertmanager's own; one receiver per channel would double every route |
| An operator with a single channel: `ChannelFailed` has no other channel to go to | notify/channelfailed.go | 2 | 5 | 1 | the `notify: channel failed` line and Alertmanager's counter say it; the design asks for "the other operator channels" and there are none |
| `motel notify test` reads the counters for 15 s; a slow receiver answers "neither yet" | notify/admin.go (`testWait`) | 2 | 2 | 1 | the answer says so; Alertmanager's retries continue |
| Alertmanager refuses a reload: motel puts the previous config back, but the URL files are already the new ones (a changed value rewritten, a dropped channel's removed), so the old config in memory sends a changed channel to its new URL, and Alertmanager's next start fails on a removed file until motel's next load | notify/state.go (`apply`) | 4 | 2 | 1 | 8; two faults (a render Alertmanager refuses, which the amtool tests rule out for every shape, then a restart before the next load), and Alertmanager names the missing file; a new URL is the one the signer committed; keeping the old files until a reload succeeds adds state to the write (size 2, risk 2). /code-review of the PoC, passes 1 and 2 |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| The box channel's `secrets/_box/` slot writing `_box.env`, and deployers' `secrets/<app>/NOTIFY_*` withheld from the unit's environment (#418) | — | — | — | the box subsystem, before any release: today root writes `_box.env` by hand |
| motel restarted while a state key fires: the engine's table is in memory and liveswap's watchdog does not re-report a loop it was already in, so the alert ends at `resolve_timeout` and a false recovery is told; the backup, disk and TLS sources re-detect on their next poll | 3 | 4 | 3 | the next slice: liveswap re-reports its watchdog state at Start, or the engine reads Alertmanager's alert list at start (#22 of the review) |
| A secret missing from `_box.env` refuses the whole config at load, as `motel validate` must: at a reboot motel does not start, and every site is down for an alert channel (named in the journal) | 4 | 5 | 1 | the `_box` slot: the box handler's consistency check refuses a push whose `notify` block names a secret the push does not carry, before it reaches the box; at load a missing secret then drops its channel with a warning. Load and validate cannot be told apart in Provision today (/code-review pass 2) |
| Slack and Discord with real hooks; ntfy's formatting of Alertmanager's payload (the hotserve-1 test delivered the JSON body as the message) | — | — | — | needs the owner's hooks |
| A per-app `notify` block (`deploy_succeeded on` per app, app-scoped channels); the pull API and the later channel kinds, for which the `notify: event` line is the record | — | — | — | later slices |

## e2e rows

`make e2e-notify` (e2e/notify/run.sh): a box built from e2e-motel's
image with Debian's Alertmanager installed and masked
(e2e/Dockerfile, `notify-box`), and a stub receiver (`e2e-sink`). The
suite runs `motel notify setup` itself and asserts: no TCP listener on
9093 or 9094; the socket `srw-rw---- root motel` and the state
directory `drwxr-s--- motel prometheus`; the rendered config passes
`amtool check-config`; root and motel reach the socket and nobody does;
no URL in the config or the status API; the heartbeat; a real failing
deploy's `deploy_failed` to the operator and the bound deployer within
the same seconds, `deploy_succeeded` and `deploy_in_progress` to
nobody, its journal line; an external one-shot to the operator alone;
four distinct one-shots in a burst, four deliveries; `send`'s
refusals, and nobody unable to send; the inhibition of the deploying
app's `watchdog_throttled` while another app's arrives after the
`group_wait`; a state from outside at +30 s and its recovery after
motel's hold and one tick; a channel answering 400 named in
`ChannelFailed` to the other operator channel; `motel notify test`
delivered and failed; the view probe's three keys closed from inside a
sandboxed app; no URL in either journal. Not in e2e (unit-tested
only): the five-minute give-up of an undeliverable post, the bucket's
hour.

## Later slices

Designed in the fresh-slate draft, not built: the metrics store
(Debian's `victoria-metrics` in a private network namespace behind a
`systemd-socket-proxyd` unix socket), motel pushing its registry to it,
rules over its series, `metrics` in `GET /<app>`, and a
`motel metrics setup` with the same mask-first order. vmalert's
`Watchdog` and failed-delivery rules would be second sources beside
motel's own; Alertmanager deduplicates.

## History

Dated one-liners; the full text of each is in git.

- 2026-11-08 — #418 filed (on hold): notify in the motel process, no
  new unit, four hand-written webhook formats, an in-memory state table
  with `group_wait` and `repeat_interval`, a dead-man recipe as a root
  timer; metrics later as an in-process window.
- 2026-11-10 — **#418's "No new unit" and "notify lives in the motel
  process" reversed** by this design, on measurements made the same
  day on hotserve-1 (Debian 13, arm64, 3.8 GB): Debian's
  `prometheus-alertmanager` 0.28.1 runs with no TCP listener at all
  (`--web.systemd-socket` and a socket unit; three cold boots clean),
  idles at 40 to 42 MB RSS, delivers an event 6 to 7 ms after motel's
  post (1.6 s to a real ntfy.sh topic), keeps channel URLs out of its
  config, API and failure log through `*_file` fields, and brings
  grouping, dedupe, repeat, resolved-once, inhibition, retries and a
  notification log motel would otherwise write. Measuring it also found
  the one-shot loss under shared grouping and the 290 s recovery on the
  default `group_interval`, which shaped the route tree. An independent
  review (Opus 5.5) found one blocker and fourteen majors in the first
  draft, all accepted: the route tree, the install window, the paths,
  the heartbeat and `channel_failed` without the metrics package, the
  flapping guard, the deploy mute's matcher, external producers through
  a validating command, the unreachable-Alertmanager behaviour, the URL
  check, and the sandbox-view argument.
- 2026-11-10 — Slice one built on `poc/notify-alertmanager`: the block
  and its refusals, the renderer, the engine, liveswap's `Notifier`,
  the backup, TLS and disk sources, the heartbeat and `channel_failed`,
  `motel notify send`, `test` and `setup`, the package's `Suggests`,
  and the e2e suite. The state directory is `/var/lib/motel-notify`,
  not the draft's `/var/lib/motel/notify`: `/var/lib/motel` is 0750
  motel:motel and the prometheus uid cannot traverse it.
