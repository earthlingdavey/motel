# DESIGN — notify

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- notify/DESIGN-notify.md`);
the "History" section at the end holds only dated one-liners.

**Status: experimental, slice one**: the notify core. Opt-in (nothing
runs without a `monitor` block with channels, and the package only
Suggests Alertmanager); the three Owed rows that barred a production box
landed on 2026-11-10 ("History"), and what is owed still ("Owed") is
real Slack and Discord hooks and later slices. The metrics store,
vmalert and `metrics` in `GET /<app>` are slice two, in
notify/DESIGN-metrics.md. Normative for what is built: a change
that cannot hold a line here changes the line in the same PR, with
the reason.

`notify` is who motel tells, about what. **motel detects; Debian's
Alertmanager delivers.** motel's detectors (the watchdog, the deploy
handler, the backup record, CertMagic's events, the filesystems) post
alerts to an Alertmanager that listens on a unix socket only, and
whose whole configuration motel renders from the Caddyfile's `monitor`
block, which also holds the metrics store's line (DESIGN-metrics.md). It
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
| Its listener | `/run/motel-notify/am.sock`, 0660 root:motel, a systemd socket unit (`notify/units/motel-am.socket`); no TCP port in the host's namespace. With the store, a second socket, `127.0.0.1:9093` inside the store's namespace, for vmalert (DESIGN-metrics.md, "The shape") | its own runtime directory: `/run/motel` is motel.service's `RuntimeDirectory`, removed whenever motel stops |
| Its config | `alertmanager.yml` and one `url-<KEY>` file per channel secret, 0640, rendered by motel at every config load | `/var/lib/motel-notify`, 2750 motel:prometheus (setup makes it); not `/var/lib/motel`, which is 0750 motel:motel and the prometheus uid cannot traverse |
| motel's part | the `monitor` block's notify lines and their refusals, the renderer, the engine (state table, flapping guard, mute, heartbeat, retry queue), the detectors, `channel_failed`, two admin routes, `motel monitor send`, `test` and `setup` | `notify/` |

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

## The monitor block

```
monitor {
    channel <name> json|ntfy|slack|discord {secret.NOTIFY_<NAME>} [severity critical|warning|info]
    operator <channel>...
    bind <identity> <channel>
    heartbeat {secret.NOTIFY_<NAME>} # none: warned at every load
    deploy_succeeded on|off          # off by default (#418's table)
    allow_insecure_channels          # test rigs only; warned at every load
    secrets_file <path>              # default /etc/motel/secrets/_box.env
    backup_record <path>|off         # default /var/lib/motel-backup/status.json
    metrics [<socket path>]          # the metrics store (DESIGN-metrics.md, "The metrics line")
    rule <name> <kind> …             # the operator's rules; needs metrics (DESIGN-metrics.md, "Operator rules")
}
```

Every line but `metrics` and `rule` is notify's, and this document's. One block
holds who hears, the numbers and the operator's rules over them
("Decisions"); it is one Caddy app, `monitor`, of which the store is a
part. A block that is a `metrics` line alone runs no notify: no
render, no engine, no Alertmanager config, and the warning
DESIGN-metrics.md's "The metrics line" names. Any other line makes the
block notify's, held to every refusal below, its missing `operator`
line among them, whatever it says (`deploy_succeeded off` too); so is
a block with no line at all. A JSON config is read the same way: a
field of notify's with a value is a line, `"deploy_succeeded": false`
among them, and an empty list or string, or a false
`allow_insecure_channels`, is none, as no Caddyfile line writes one. `notify` and
`metrics_store` as global options are refused when the Caddyfile is
adapted, naming the block and the line to write instead, and the
upgrade order for a box that has them (docs/upgrading.md, "From the
notify and metrics_store blocks").

There is no `state_dir` and no `alertmanager` line: motel renders into
`/var/lib/motel-notify`, the directory setup makes and the drop-in's
`--config.file` names, and posts to `/run/motel-notify/am.sock`, where
setup's `motel-am.socket` listens. A directory of a line's own would
be written and never read while the load said it worked, and a socket
of its own would reach nothing ("History", 2026-11-13). In a
Caddyfile either line is refused by name, at its line, saying where
the path is fixed; in a JSON config the field is Caddy's `unknown
field`. A box that has either line takes it out before the install
(docs/upgrading.md, "From a state_dir or alertmanager line").

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
| a `notify` global option (refused at the adapt, before Provision) | ``the notify block is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed; to move a box onto it, docs/upgrading.md, "From the notify and metrics_store blocks"`` |
| a `state_dir` line (at the adapt, naming its line) | `state_dir is not a monitor line: the state directory is fixed at /var/lib/motel-notify, the one motel monitor setup makes and Alertmanager reads; remove the line` |
| an `alertmanager` line (at the adapt, naming its line) | `alertmanager is not a monitor line: Alertmanager's socket is fixed at /run/motel-notify/am.sock, the one motel monitor setup's motel-am.socket listens on; remove the line` |
| a literal URL, or `{env.*}` | `a channel URL is written {secret.NOTIFY_<NAME>}, its value in the box's secrets, never in the Caddyfile` |
| a secret not named `NOTIFY_…` | `the secret's name is NOTIFY_ followed by capitals, digits and underscores` |
| no `operator` line | `no operator line: name the channels that receive every event with operator <channel>` |
| `operator` naming no channel line | `operator names <c>, which no channel line defines` |
| a channel defined twice | `channel <c> is defined twice` |
| `bind` to a channel that does not exist | `bind "<id>" names channel <c>, which no channel line defines` |
| `bind` to an identity no app's `deploy_trust` trusts | `bind "<id>": no app's deploy_trust trusts this identity; remove the line, or trust it` |
| a secret the box does not hold (a boot drops the channel instead: "Secrets at load") | `channel <c>: secret <KEY> is not in the box's secrets (<file>)` |
| a URL not https, not to a public address, or to a local name (`localhost`, `*.local`, `*.internal`, a single label); a boot drops the channel instead | `channel <c>: the value of <KEY> is not an https URL` / `… is not to a public address …` / `… is not to a public host name` |

The URL check is every URL's, not only a deployer's: Alertmanager sends
from the network namespace every app shares, so a URL to loopback or a
private address would reach a listener an app opened.

### Secrets at load

The last two rows are a secret's, and a boot does not refuse them.
Provisioning cannot tell `motel validate` from a config load (Caddy's
`Context` says neither), so cmd/motel tells notify the command it runs
(`SetCommand`, before Caddy's own command), and a load reads Caddy's
running config (`caddy.ActiveContext`):

| Load | A channel whose secret is missing, or whose value is refused |
|---|---|
| `motel validate` (by hand, CI's `make check`, the box handler's step 6) | refused, as the table says; the handler and `make check` name the secrets in `secrets/_box/` (`MOTEL_NOTIFY_PUSHED_SECRETS`), taken as held ("Secrets"). validate runs as root, who reads any file, so it also refuses a secrets file the motel user could not read, by its owner, group and mode against motel's uid and groups (`motelCannotRead`): root:root 0600 after an `mv`, which the server's own read would refuse at a reload and drop every channel over at a boot |
| a reload (`motel reload`, `systemctl reload`, the box applier's): a config is running | refused; the running config serves on, and the box applier rolls its push back |
| the first load of `motel run` (a boot, a restart, an unattended upgrade's): nothing running | the channel is **dropped** from that load (from the operator line, each binding, the route tree; the heartbeat, if it is its secret), each a `monitor: a channel is dropped from this load` error line naming every line that reads the key, and the key, never a value; and one more if no operator channel is left. A secrets file that cannot be read or parsed (a line without `=`, over 1 MiB, unreadable) is every secret unusable: one error line names the file and its error, and every line that reads a secret is dropped. motel serves |

Refusing a boot would take every site down for an alert channel; a
dropped channel is the one cost, said in the journal, and the next
`motel validate` or reload says it again. This is the rule cmd/motel's
gate keeps for its own checks (an admin API on TCP, an unread backup
declaration): validate and reload refuse, run serves. The box makes
the case rare: its consistency check refuses a push whose `monitor`
block names a secret the push does not carry in `secrets/_box/`
(box/DESIGN-box.md, "Which checks run where"), so `_box.env` lacks one
only by a hand edit or a lost file. Every other refusal of the table
refuses a boot too: they are the Caddyfile's, which the same signer
committed whole.

## Secrets

The values are read from `/etc/motel/secrets/_box.env` (`KEY="value"`
lines, liveswap's env-file format; channel lines name `NOTIFY_` keys),
0640 root:motel, bound into no unit. Every value in the file, named by
a channel line or not, is a known value to liveswap's filter, which
every summary passes through. motel writes
each value once more, to its `url-<KEY>` file in the state directory,
because Alertmanager's `*_file` fields read a file: that is the copy
the prometheus uid reads, and the config itself holds paths only.

**The box channel writes the file**: a signed push's
`secrets/_box/NOTIFY_*.age` (#418's reserved slot; box/DESIGN-box.md,
"The box's own slot", which reverses its open question 5 for this one
use) is decrypted by root into `_box.env` as an app's secrets are into
its env file, and a push that changes the file reloads motel, so a new
URL is rendered at once (`make secret APP=_box VAR=NOTIFY_OPS_URL` in
the box's repository; docs/secrets.md). The box handler validates a
push before root writes its values, and cannot read them: `motel
validate` is told the names the push carries
(`MOTEL_NOTIFY_PUSHED_SECRETS`, names only, read in `validate` alone and
for the default `secrets_file` alone, `PushedSecretsEnv`) and takes each
as held, unchecked; a value the URL check refuses is refused at the
reload the push makes, which the box rolls back. A box with no
repository, or a `secrets_file` of the operator's own, is written by
root by hand, as the e2e image does. A box whose root wrote `_box.env`
by hand and that takes pushes moves each value into its repository
(`make secret APP=_box …`) in its next push, whatever that push
changes: the box refuses any push whose `monitor` block names a secret
it does not carry, and the first that carries them replaces the
hand-made file, named as edited out of band.

**The `NOTIFY_` prefix is notify's in what the box writes.** A
`NOTIFY_` variable among an app's own secrets (`secrets/<app>/NOTIFY_*.age`,
in the env file the box writes under `/etc/motel/secrets`) is withheld
from the app unit's environment, named in a warning at each launch, and
stays a known value to the filter (liveswap, `notifyOwnedEnvKey`); `make
check` warns of it before the push (`motel box describe`) and `make
secret` as it is set. An inline `env` line and an env file the operator
keeps elsewhere are the operator's, and pass: refusing them would refuse
a boot over an app's own variable name (/code-review of this change).
No line reads such a value yet: a deployer's
channel is a `channel` line whose URL is in `_box/`, bound with `bind`;
the per-app block that would read an app's own (#417's bundled secrets
too) is a later slice ("Owed").

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
whole; in a grouped Slack or Discord message each alert that has resolved is marked `[RESOLVED]`. Nothing in the block is a template.

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
| `deploy_failed` | liveswap | event | warning | a 5xx outcome, a push's upload that could not be staged, or app dirs that are not motel's; the summary names the version and the phase, never the error's text, and says nothing of what serves now | — |
| `deploy_refused` | liveswap | event | warning | a 422 (liveswap's own words, through the filter) | — |
| `rollback` | liveswap | event | warning | `?rollback=` succeeds | — |
| `deploy_succeeded` | liveswap | event | info | a deploy succeeds; dropped unless `deploy_succeeded on` | — |
| `deploy_in_progress` | liveswap | the mute | none | a deploy starts; re-posted each minute while it runs | it ends, whatever it came to |
| `backup_stale/<app>` | backup | state | critical | the app's last complete backup is older than `status.StaleAfter` (3 h) | it is fresh |
| `backup_run_failed` | backup | state | warning | the last run ended early, or an app's class is anything but ok or pending (`motel-backup run`'s own rule for a failed run) | a run ends with every app ok or pending |
| `drill_failed` | backup | state | critical | the last drill could not begin, the check found damage, or an app's latest drill proved nothing (`App.RestoreDrill`, as `status` reads it); the summary names the apps, never the drill's detail | the next drill that proves every app |
| `backup_unreadable` | backup | state | warning | the record does not parse | it reads |
| `cert_failed/<identifier>` | tls | state | warning; critical with under 7 days left | CertMagic's `cert_failed` | its `cert_obtained`, or 13 h with no report (a name no config manages any more) |
| `disk_low/<path>` | disk | state | warning at 80 % used, critical at 90 | statfs, every minute, one per device, of the liveswap root, Caddy's data and the state directory, each on its own goroutine within 2 s (a path not back is not measured, and starts no other statfs until it is back); the level is held per device, whichever path stands for it | five points under its threshold |
| `Watchdog` | notify | heartbeat | none | every minute, alive 4 m | — |
| `ChannelFailed` | notify | direct | warning | Alertmanager's failure counter rose ("Failed delivery") | `endsAt` +15 m |
| `post_deploy_regressed` | rules | event | warning | a `post_deploy` rule's checks after a deploy or a rollback of the app failed `failures` times in a row: the new release over a bound and worse than the one it replaced (DESIGN-metrics.md, "Operator rules"); once per deploy, and it never acts | — |
| `<rule name>` | rules | state, by vmalert | the rule's | an operator's `threshold` or `disk_fill` rule holds for its `for:` (DESIGN-metrics.md, "Operator rules"); `subject` is the app's, or `box` for a disk | vmalert resolves it |

Sources are `liveswap`, `backup`, `tls`, `disk`, `notify`, `metrics`
(the store's chain, DESIGN-metrics.md), `vmalert` (its fixed rules),
`rules` (the operator's rules, #418's reserved `source: rules` and
`key: rules/<subject>/<rule name>`, and `/<path>` after it for a
`disk_fill` rule's filesystem) and `external:<name>`.
liveswap reports through `liveswap.Notifier` (liveswap/lifecycle.go),
on the goroutine where the event happened. A deploy's or a rollback's
`cutover`, told at the swap with the release it replaced, when, and
whether it is a relaunch of the release the app last ran (`Previous`,
`Cutover`, `Relaunch`), is no event of the engine's: it starts the
post-deploy comparison (DESIGN-metrics.md, "Operator rules"). notify subscribes to
`cert_failed` and `cert_obtained` by name through Caddy's events app.
The backup record and the disks are polled by the engine. A box with
no backup record has no backups set up and says nothing.

## Behaviour

The engine is one per process and outlives a config load, as
liveswap's app pool does; each config's Start points it at its
Alertmanager, and the Stop of the config it serves ends its goroutine.
Its pollers are the engine's too, so a key one fired is cleared by
whichever config serves when it clears.

- **Journal first.** Every event is a `monitor: event` line (key,
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
  then given up as `monitor: delivery failed` (error level for an
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
  what it fired, and fires nothing new: a statfs that fails, or that
  is not back within 2 s (a network mount whose server went away), so
  that a filesystem that hangs holds back neither the engine's posts
  nor the heartbeat (Copilot round 3 on #429).
- **Reloads.** A reload Alertmanager does not answer is a warning, and
  the config counts as unacknowledged, so the next load reloads it
  though the file has not changed. A reload that fails after notify's
  Start stops the new config while the old one serves on; the engine
  goes back to the old one, and puts the old one's render back in the
  state directory and in Alertmanager. When the last config running
  notify stops, the engine's table, mutes and buckets are emptied: a
  block added back later starts clean. When that stop is a reload's
  (a config serves on without notify: the block or its channels taken
  out), Alertmanager is given setup's config that sends nothing and
  the URL files are removed (`applyNothing`): what it holds ends at
  `resolve_timeout` through the blackhole, where the last render's
  receivers would have told every channel a recovery that did not
  happen. motel's own shutdown leaves the render, so that a restart
  keeps what fired: Caddy empties its running context before it stops
  the last config's apps, so `configRunning` is a reload's alone at
  Stop, as for the store's rules (DESIGN-metrics.md, "The metrics
  line"). A restart or a boot into a config that runs no notify starts
  no App to do it, so cmd/motel's gate does it at `motel run`, as it
  retires the store's rules (`notify.SendNothing`), reloading
  Alertmanager only when the state directory changed, so that a box
  whose block never had channels starts no Alertmanager for it.
  `motel reload` of a Caddyfile with a monitor block passes
  `--force`, as `systemctl reload` does: the block reads the secrets
  file, which the config does not hold, so a mended `_box.env` would
  otherwise load nothing (cmd/motel's gate, `reloadForces`).
- **A certificate no config names** is never obtained and never
  reported again, so no `cert_obtained` clears it. CertMagic reports
  `cert_failed` on every failed attempt for a name it manages, and its
  longest retry interval is six hours; a `cert_failed` key unreported
  for 13 hours is taken as cleared, with a line, and resolved after the
  hold.
- **The watchdog turned off** while a restart loop is reported ends the
  report (`watchdog_recovered`): no probe would ever say so.
- **A motel restart** empties the table while Alertmanager keeps each
  firing alert until `resolve_timeout`. The backup, disk and TLS
  sources re-detect within a poll, before it; liveswap's watchdog would
  not, since a fresh process does not know a loop it was already in,
  and a refused relaunch is refused again by recovery with nothing
  reported. So liveswap keeps what it told in each app's `alerts.json`
  (a loop, whether its recovery's hold runs, a refusal with its release
  and reason, so that the report says them again; written on each
  change, removed when none stands, forgotten for an app a reload
  removes or the start's sweep finds unconfigured), and **re-reports it at
  notify's Start** (`ReportStanding`, pulled by notify once its engine
  serves the config, whichever app Caddy starts first). Each is a
  standing report: the engine ignores it for a key it holds (a reload),
  and takes it as told for one it does not, journaled with
  `standing: true` (the report's own lines; the key's later lines are
  this process's) — a loop fires again (Alertmanager merges it with the
  alert it holds: no second message), a recovery under way is held for
  its hold and then resolved; a recovery whose hold has passed is not
  told again. What ends while the reports are told (a recovery's
  relaunch at the same start) is told ended once more after them, so a
  standing report never outlives what it reports. The other way round, reading
  Alertmanager's alert list at start, was not taken: Alertmanager keeps
  no alerts across its own restart (a reboot), and what it holds says
  nothing to liveswap, whose watchdog would still not report the
  loop's end, leaving an adopted key firing for good.

## Failed delivery

Once a minute motel reads Alertmanager's own `/metrics` over the
socket, with Prometheus's text parser (`prometheus/common/expfmt`,
already in the binary through Caddy's metrics). The read is strict: a
body cut short, a malformed line, or one without the failed counter is
no reading, and the baseline stays the last good one. `alertmanager_notifications_failed_total` names the integration
and, with the drop-in's `receiver-name-in-metrics`, the receiver. A
rise (the first read is a baseline; a lower value is a restart,
counted from zero) is a `monitor: channel failed` line, and one
`ChannelFailed` alert to each operator channel that is not a suspect
and whose `severity` floor takes a warning, which `ChannelFailed` is
(`failureAudience`, vmalert's rules' too), each routed to that channel
alone (`notify_channel=`). The suspects are
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
A block with no `heartbeat` line loads, with its `Watchdog` routed to
the blackhole receiver, and every load (`motel validate` too) warns
`monitor: no heartbeat in this load`, as for `allow_insecure_channels`:
nothing outside the box would hear that it stopped. A boot that drops
the line for its secret ("Secrets at load") warns the same.

## External producers and tests

`motel monitor send` posts to `POST /notify/events`, and `motel monitor
test <channel>` to `POST /notify/test`, both `admin.api.notify` routes
on the admin socket: root and the motel uid reach it, no app does.
External producers never post to Alertmanager's socket on the
documented path; root can, as root can do anything. Both the command
and the route check (`SendRequest.Check`):

| Refused | Why |
|---|---|
| a source not `[a-z0-9-]{1,32}` (stamped `external:<name>`) | one grammar, so a key cannot pose as a detector's |
| a subject not `box` or `app:<an app the box runs>` | an app subject reaches its deployers |
| motel's own names: `Watchdog`, `ChannelFailed`, `NotifyTest`, `deploy_in_progress`, `watchdog_*`, the deploy events, `disk_low`, `cert_failed`, `backup_*`, `drill_failed`, and the metrics store's (`metrics_store_down`, `metrics_rules_down`, `metrics_store_read_only`, `metrics_store_disk_fill`, `app_memory_high`) and the operator's rules' `post_deploy_regressed` | a producer that could post them could mute an app, fake the heartbeat, or speak as a detector |
| a summary empty, on two lines, or over 200 characters; details over 2 KiB or with names not `[a-z][a-z0-9_]*` | #418's limits |
| a body with unknown fields (a raw Alertmanager label set) | the route takes the request, never labels |
| a name or instance the filter would mask (a channel URL, a secret-shaped token) | both are the key, the labels and the answer, which the filter does not rewrite |

Accepted, an external event is one like motel's own: the filter, the
bucket, the journal line, the queue. `motel monitor test` posts one
`NotifyTest` alert with a nonce label, routed to the channel alone,
then reads the channel's receiver's counters for 15 s: delivered when
an attempt did not fail (`notification_requests_total` rose more than
`notification_requests_failed_total`; `notifications_total` is raised
before the first attempt and says nothing of the outcome), failed when
Alertmanager gave up (with its reason class), or neither yet; it exits
1 unless delivered, and never prints the URL. An external producer's
`--detail key=value` pairs are annotations `detail_<key>`, through the
filter.

What a load would say in the journal alone, the commands say too. On a
box setup has not run on (no state directory), `send` journals the
event and exits 1 (`journalled as <key>, and delivered nowhere`), and
`test` answers 503, both naming `motel monitor setup`; `motel validate`
and `motel reload` warn (cmd/motel's gate, `notify.LoadWarnings`). A
`resolved` that finds nothing firing under its key answers `resolved
nothing` with exit 0, since a producer may resolve whatever it last
said. `test` on a channel a boot dropped names it dropped, and the
mend. Every load's gate, `run`'s among them, warns when a unit setup
wrote differs from the one this binary would write, naming the setup
to run again: an upgraded box keeps the old units until root does.

## Packaging and setup

The `.deb` **Suggests** `prometheus-alertmanager`. A `Depends` cannot
close the window in which Debian's unit starts on install (`:9093`,
gossip on `0.0.0.0:9094`): apt may configure a dependency, and run its
postinst, before motel's own scripts run, and `Pre-Depends` makes that
order certain rather than preventing it. On a box with no firewall
(hotserve-1 has none) that is the alert and silence API on the
internet. So **`motel monitor setup`**, run by root, does it in an order
motel controls (`--metrics` adds the store's setup, after this one
only where this one has not run to its end: DESIGN-metrics.md,
"Packaging and setup"):

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
   Alertmanager's storage directory, whose mode is the drop-in's:
   `ExecStartPre=+/usr/bin/chmod 0750` at every start, since Debian ships
   it 0755 and a purge and reinstall makes it so again (dpkg keeps an
   existing directory's mode on an upgrade, measured with a reinstall of
   0.28.1, and the postinst only chowns it);
4. `daemon-reload`, `unmask`, `disable` the service (its socket starts
   it), `enable --now motel-am.socket`;
5. `try-reload-or-restart motel.service`, which renders.

Run again, it puts the units back as the binary ships them. A re-run
that fails after the mask unmasks the service on its way out and starts its socket
(a connection that arrived during the mask fails the socket, which
then starts nothing), so a failed re-run does not
leave Alertmanager unstartable; a first run that fails before step 4's
`unmask` stays masked, and one that fails after it is left as far as it
got, with the drop-in and the socket already written
(notify/DESIGN-metrics.md, "Accepted residuals").
Debian
upgrading or reinstalling the package, whose postinst restarts the
unit directly, was measured on hotserve-1 (2026-11-10, 0.28.1
reinstalled under motel 0.6.2~notifypoc1): the socket's fd is passed
(`Listening on systemd activated listeners`), nothing listens on 9093
or 9094, and `motel monitor test` delivers at once; the postinst
re-enables the service, so it then starts at boot rather than at its
socket's first connection, still on the socket alone. A reboot with motel rendering (2026-11-10, hotserve-1,
13.2 s to multi-user): `motel-am.socket` listening at 11.9 s,
Alertmanager started at 12.0 s on the rendered file left from before,
motel's render and reload at 12.8 s, motel ready at 12.9 s; no ordering
cycle, no listener on 9093 or 9094, a heartbeat at once and `motel
monitor test` delivered. Two unit facts were measured too: `motel-am.socket` must name its service
(`Service=prometheus-alertmanager.service`), or systemd refuses it; and
Alertmanager needs Debian's `/usr/share/prometheus` templates, so the
package is installed rather than a binary fetched. With the store,
`--metrics` adds a second socket in the store's namespace and a second
drop-in, `20-motel-metrics.conf`, which this setup leaves as they are
(DESIGN-metrics.md, "The shape"); a re-run of this setup with
connections arriving on that socket throughout left it listening and
Alertmanager answering on both (measured in the e2e box, 2026-11-12).

## Paths

| Path | Mode | Owner | Writes | Reads |
|---|---|---|---|---|
| `/run/motel-notify/am.sock` | 0660 | root:motel | systemd (the socket unit) | root, motel |
| `/var/lib/motel-notify/` | 2750 | motel:prometheus | setup (makes), motel | Alertmanager |
| `…/alertmanager.yml`, `…/url-<KEY>` | 0640 | motel:prometheus (born in the setgid dir) | motel, each by rename | Alertmanager |
| `/etc/motel/secrets/_box.env` | 0640 | root:motel | root: the box applier from `secrets/_box/`, or by hand | motel |
| `/etc/systemd/system/motel-am.socket`, `…/prometheus-alertmanager.service.d/10-motel.conf` | 0644 | root | setup | systemd |
| `/var/lib/prometheus/alertmanager/` | 0750 (the drop-in, at every start; Debian ships 0755) | prometheus | Alertmanager (the notification log, silences) | Alertmanager |
| `<liveswap root>/<app>/alerts.json` | 0600 | motel | motel (liveswap's watchdog, on each change; removed when nothing stands) | motel (once per process, for `ReportStanding`); in no app's view |

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
- With the store, Alertmanager also listens on `127.0.0.1:9093` inside
  the store's namespace, which no app and no host process outside it
  reaches; what runs there (the store and vmalert) has its whole API.
  DESIGN-metrics.md's threat model accepts it ("The namespace's
  daemons").
- Channel URLs are in no config, no API echo and no journal (the
  `*_file` form; measured on hotserve-1 with planted tokens:
  Alertmanager's failure lines print `Post "<redacted>"`; the e2e
  suite greps both journals, the rendered file and `/api/v2/status`).

## Viewing

No new listener, no new credential kind, no session, and no UI.
Alertmanager's API has no authentication of its own and is reached
only through the socket. Debian's package ships no web application: its
`/` is a page that says so and points to `amtool` (measured on 0.28.1
in the e2e box, 2026-11-12). On the box, root uses the `motel monitor`
commands or `curl --unix-socket /run/motel-notify/am.sock
http://am/api/v2/alerts`. `amtool`, which the package ships, speaks TCP
only: on a box with the store it runs inside the store's namespace,
against Alertmanager's listener there (`ip netns exec motel-metrics amtool
--alertmanager.url=http://127.0.0.1:9093 alert`, measured), and from
root's laptop through `ssh -L 9093:/run/motel-notify/am.sock root@box`
(OpenSSH forwards to a remote unix socket since 6.7; not measured).
Slack's title link, which Alertmanager's defaults point at its external
URL (`http://<hostname>:9093/#/alerts…`, where nothing answers), is
rendered empty, and its fallback is motel's title. A deployer
sees notifications at their channel; not Alertmanager, other apps, box
events or silences. The operator's guide to all of this is
docs/monitor.md.

## Decisions

| Decision | Why |
|---|---|
| One block, `monitor`, for notify and the metrics store | notify, the numbers and the rules over them are one idea to the person configuring the box; `metrics` is Caddy's own global option, so the store cannot be a block of that name; one global option yields one Caddy app, so the store is a part of the monitor app (`App.Metrics`), not an app of its own. A `notify` or `metrics_store` block is refused by name, and `motel notify` and `motel metrics` exit 1 with the new spelling: everything here is unreleased, so there is no alias |

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
| motel stopped for more than five minutes: Alertmanager resolves each firing state alert at `resolve_timeout`, and the channels get a recovery that did not happen (at motel's start the watchdog's standing reports fire its alerts again, and the pollers theirs) | notify/engine.go (no `endsAt` on re-posts) | 2 | 3 | 3 | 18; a stop that long is itself a fault the heartbeat reports (Watchdog's silence pages first); keeping Alertmanager's alerts alive across a stop would need a poster outside motel; a restart (seconds) is "Behaviour"'s, and keeps them |
| A box on the feature branch's `notify` or `metrics_store` block cannot move to the `monitor` block in one load: the new binary refuses the old blocks and the old binary the new one, so the upgrade runs with no notify between two pushes (docs/upgrading.md, "From the notify and metrics_store blocks"): no event is delivered, the heartbeat stops (the dead-man check pages), Alertmanager ends what fired at `resolve_timeout` with a recovery that did not happen, and the store's rules are retired at the first push's reload | notify/caddyfile.go (`refusedOption`) | 3 | 5 | 1 | 15; once per box, only for boxes that ran the unreleased feature branch, in an order the doc names and the refusal points at, and the dead-man check reports the gap; a transitional alias (the old names loading, warned, for one release) is a second grammar and a new state to retire (size 3, risk 2). /code-review pass 1 on the monitor block, finding 1; the user chose the documented order over a transitional alias |
| A recovery on an ntfy channel carries its alert's priority: a critical state's `[RESOLVED]` is sent at ntfy's 5 (urgent) too | notify/render.go (`ntfyPriority`) | 2 | 5 | 1 | 10; the header is fixed per receiver, and a receiver takes an alert and its end alike; the body says `resolved`. A priority by status needs a receiver per status (size 3, risk 2). #433 |
| Two producers send a `firing` and a `resolved` of one key at once: the resolve's answer (`resolved nothing`, or `sent`) is read before its report runs, so it can say the opposite of what the report did | notify/admin.go (`serveSend`) | 2 | 2 | 3 | 12; two producers racing on one key, and the state ends as the reports leave it; the answer alone is wrong, and the journal lines say what happened. The report returning what it found changes a function the pusher takes as a value (size 2, risk 2). /code-review pass 2 on #433 |
| `motel --help` and its commands' usage say `caddy` and link caddyserver.com's docs | Caddy's root command (binary-wide) | 2 | 5 | 1 | 10; the commands themselves are motel's and work as printed under `motel`; renaming Caddy's root command is a change to every command's help, not monitor's (size 2, risk 2). The deploy of 01b2fd5 to hotserve-1 |
| `motel monitor test` reads the counters for 15 s; a slow receiver answers "neither yet" | notify/admin.go (`testWait`) | 2 | 2 | 1 | the answer says so; Alertmanager's retries continue |
| At a boot, an event liveswap reports before notify's Start (Caddy starts the apps in no fixed order) is dropped, the engine serving no config yet: a recovery at that instant ends its alert at `resolve_timeout`, up to five minutes late | notify/engine.go (`report`, `active == nil`) | 2 | 3 | 3 | 18; what stands is told again at notify's Start (`ReportStanding`), and an end told meanwhile is told again after it; holding events for a config not yet started adds a queue with its own lifetime (size 3). /code-review of the Owed rows |
| A restart after notify has resolved a recovery but before the next healthy probe clears `alerts.json` (with `health_interval 2m` and a 30 s hold, up to about 90 s) replays the recovery: the emptied engine posts the alert held, then resolved, and the operator sees the firing and recovery messages again. A reload does the same when it lengthens the hold (the standing report judges it with the candidate's `watchdog_grace`), and so does a push just after a restart (the watchdog's own `healthySince` is new) | liveswap/watchdog.go (`reportedAlerts` keeps `Recovering`, not when the hold began); liveswap/lifecycle.go (`reportStanding`) | 2 | 2 | 1 | 4; a duplicate the operator sees, only for a restart or reload inside one health interval after a hold ends; persisting the hold's start, and the engine honouring it, is size 3 (risk 2). Copilot round 1 on #424, widened by /code-review xhigh on #424 |
| A restart inside a recovery's hold, then a relaunch that fails for good (no instance runs after the restart): the persisted `recovering` is re-reported as a held recovery, so the engine resolves the alert of an app that is down, and the flags stay for the next restart | liveswap/lifecycle.go (`reportStanding`) | 3 | 2 | 3 | 18; two faults, a restart inside a hold and then a relaunch that fails; the journal names the failed relaunch and status shows the app down; telling it apart needs a branch on whether an instance runs after recovery (size 2, risk 2). /code-review xhigh on #424 |
| A failed write of `alerts.json` (EIO, a read-only remount) leaves the old file until the next change, and a restart before it reports what the file still says | liveswap/watchdog.go (`alertsChanged`) | 3 | 3 | 1 | 9; the warning at the failure names the file and the consequence; a retry needs a timer outside the transitions (size 2, risk 2). /code-review of the Owed rows |
| An app that read a `NOTIFY_` variable from an env file in `/etc/motel/secrets/` — the box's `secrets/<app>/`, or an operator's own file kept in that directory — loses it from the app's environment at the next launch after the upgrade | liveswap/app.go (`notifyOwnedEnvKey`, `boxWritten`: a test of the directory) | 3 | 5 | 1 | 15; each launch names the withheld keys and the directory in the journal, and `make check` warns before a push; the prefix is what keeps a channel URL from an app. /code-review of the Owed rows, widened by /code-review xhigh on #424 |
| Alertmanager refuses a reload: motel puts the previous config back, but the URL files are already the new ones (a changed value rewritten, a dropped channel's removed), so the old config in memory sends a changed channel to its new URL, and Alertmanager's next start fails on a removed file until motel's next load | notify/state.go (`apply`) | 4 | 2 | 1 | 8; two faults (a render Alertmanager refuses, which the amtool tests rule out for every shape, then a restart before the next load), and Alertmanager names the missing file; a new URL is the one the signer committed; keeping the old files until a reload succeeds adds state to the write (size 2, risk 2). /code-review of slice one, passes 1 and 2 |
| A JSON config (or a `/load`) with `"state_dir"` or `"alertmanager"` is refused as Caddy's `json: unknown field "state_dir"`, not with the Caddyfile's sentence that names the fixed path | notify/app.go (no field; Caddy's `StrictUnmarshalJSON`) | 2 | 5 | 1 | 10; refused, naming the field, and the Caddyfile is what a box loads; the sentence in JSON needs a field kept only to refuse it, or an `UnmarshalJSON` (size 2, risk 2). /code-review of #438 |
| `motel monitor setup` from a new binary while an old motel still serves a config with a `state_dir` line names `/var/lib/motel-notify/alertmanager.yml`, where that motel does not render | notify/setup.go (`loadedSays`) | 3 | 2 | 3 | 18; the install restarts motel onto the new binary, which refuses the line, so this needs setup run from an unpacked `.deb` before the install (docs/upgrading.md suggests only `validate` there) on a box that kept the line the upgrade note says to remove; saying so needs a branch on the raw JSON (size 2, risk 2). /code-review of #438 |
| The journal lines of `applyRender` and `applyNothing`, and the metrics store's not-set-up warning, name their directory under the key `state_dir`, a line this version refuses: an operator who adds one to move it is refused at the next load | notify/app.go, metrics.go (`zap.String("state_dir", …)`) | 2 | 5 | 1 | 10; the refusal names the fixed path and says to remove the line; renaming the key changes what a journal search for it finds, and no test reads it (size 1, risk 2). /code-review of #438 |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| ntfy and json share the webhook integration, and so do any two channels of one kind in one receiver: a failure of one names every such channel as a suspect, a working one among them, and none of them is told (`ChannelFailed` goes only to the operator channels that are not suspects). Moved from Accepted: its product is 30, over the bar of 20 (the operator/deployer review of 2026-11-11, rechecked at 01b2fd5 and at 04e52a9) | 2 | 5 | 3 | its own PR, "monitor: one receiver per channel" (notify/render.go, channelfailed.go `suspects`, metricsrules.go: each channel its own receiver under `continue` routes, so that a failure names one channel; size 3, risk 2). Independent of #433, and it reshapes every route. It carries the heartbeat's hold too (the next row) |
| An operator with no channel to hear a failure (a single channel, or the others taking criticals alone, `failureAudience`): `ChannelFailed` is told nowhere and the heartbeat goes on pinging, so a dead channel delivers no alert while the dead-man check stays green (hotserve-1: one ntfy channel, `ops`, and a Healthchecks heartbeat). The `monitor: channel failed` line names the cause, in the journal alone. Moved from Accepted (2 5 1) at the rescore of 2026-11-13: S 3, not 2, since the green check is a verdict the operator trusts (not 5: the journal line keeps it from "lost silently", and not 4: delivery resumes with the channel); O 4, not 5, since the block is operator input but the dead channel is one common fault; D 3, not 1, since the green check and a quiet channel are a plausible answer and nothing sends the operator to the journal. A single fault that ends green (rule 2); its product is 36 | 3 | 4 | 3 | the PR "monitor: one receiver per channel", after its receivers, so that a hold never stands while a working channel is among the suspects (notify/engine.go `tick`, channelfailed.go `scrapeFailed`). While a failure is told to no channel, motel stops posting `Watchdog`; it ends within its 4 m life and the dead-man service alerts through its own channels. Any service that takes a ping URL works; there is no new endpoint and no new Caddyfile line. An Alertmanager inhibit rule was not taken: it needs a source alert and a render change. The hold comes only once the untold failure has stood 5 m with no delivery through a suspect's receiver since (the requests counters motel reads already). The pings resume at the next such delivery (`motel monitor test` makes one), or at a restart of motel (the hold is in memory, as the baseline is). The heartbeat receiver renders `send_resolved: false`, so Watchdog's end sends nothing. `monitor: heartbeat held` says why, naming the suspects, and `monitor: heartbeat resumed` follows. Size 3 (a new state), risk 2 (the minute's heartbeat changes under its tests and a new one). **Wide**: it changes what the dead-man check off the box reports for every box with a heartbeat, and a wrong hold pages. A pulled health endpoint was not taken: a public route, a token, and the box's state exposed. Later, additive: Healthchecks' `<ping-url>/fail` with the reason in the body, and a load that leaves no channel |
| An external producer's state (`motel monitor send --state firing`) does not outlive a restart of motel: the engine's table is emptied and nothing reports the key again, as liveswap's standing reports and the pollers do for motel's own, so Alertmanager ends the alert at `resolve_timeout` and the channels get a recovery that did not happen; the producer's later `resolved` finds no state. Measured in the metrics e2e box at c979404: restart at 19:36:55, `[RESOLVED]` for both states at 19:41:55. docs/monitor.md tells a producer to send `firing` again every few minutes meanwhile (the second-model review of #432) | 3 | 4 | 5 | the next code PR on feature/notify-and-metrics (notify/engine.go: keep external states across a restart, as liveswap keeps `alerts.json`; size 3, risk 2) |
| Slack and Discord with real hooks; ntfy's formatting of Alertmanager's payload (the hotserve-1 test delivered the JSON body as the message) and its title, which needs ntfy's message templating, a server feature; Discord's colour, dark red for anything firing whatever the severity (2 5 3 = 30): Alertmanager 0.28.1's `discord_config` has no colour field (amtool refuses `color`, measured 2026-11-12), and the way round it, Discord's Slack-compatible endpoint, is untried without a real hook | — | — | — | needs the owner's hooks |
| Per-app notify lines (`deploy_succeeded on` per app, app-scoped channels, reading the app's own withheld `NOTIFY_` values); the pull API and the later channel kinds, for which the `monitor: event` line is the record | — | — | — | later slices |

## e2e rows

`make e2e-notify` (e2e/notify/run.sh): a box built from e2e-motel's
image with Debian's Alertmanager installed and masked
(e2e/Dockerfile, `notify-box`), and a stub receiver (`e2e-sink`). The
suite runs `motel monitor setup` itself, and before it asserts that
`motel validate` warns Alertmanager is not set up, `send` exits 1 and
`test` fails, each naming setup; after it, it asserts: no TCP listener on
9093 or 9094; the socket `srw-rw---- root motel` and the state
directory `drwxr-s--- motel prometheus`; Alertmanager's storage
`drwxr-x--- prometheus prometheus`; the rendered config passes
`amtool check-config`; root and motel reach the socket and nobody does;
no URL in the config or the status API; the heartbeat; a real failing
deploy's `deploy_failed` to the operator and the bound deployer within
the same seconds (the deployer's ntfy channel with `X-Priority: 4`),
`deploy_succeeded` and `deploy_in_progress` to
nobody, its journal line; an external one-shot to the operator alone;
four distinct one-shots in a burst, four deliveries; `send`'s
refusals, and nobody unable to send; the inhibition of the deploying
app's `watchdog_throttled` while another app's arrives after the
`group_wait`; a state from outside at +30 s and its recovery after
motel's hold and one tick; a channel answering 400 named in
`ChannelFailed` to the other operator channel; `motel monitor test`
delivered and failed; the view probe's three keys closed from inside a
sandboxed app; no URL in either journal; a channel whose secret is
missing refused by `motel validate` and `systemctl reload`, dropped at
`systemctl restart` with its journal line while the other channels
deliver, named dropped by `motel monitor test`, and back after its key
is added to `_box.env` and a plain `motel reload` of the unchanged
Caddyfile; a root:root 0600 `_box.env` refused by root's `motel
validate`; a relaunch refused, then motel restarted: told again,
standing, Alertmanager's alert kept alive (its `endsAt` moves) and no
recovery told, then the cause mended and a reload: the recovery after
the hold, and `alerts.json` gone; a `notify` block refused by `motel
validate` naming `monitor`, and `motel notify setup` exiting 1 with
`motel monitor setup`; the block taken out by a reload while a state
fires: the state directory holds the config that sends nothing and no
URL file, the journal says so, and the alert's end, posted by root at
once rather than awaited at `resolve_timeout`, reaches no channel. Not
in e2e (unit-tested only): the
five-minute give-up of an undeliverable post, the bucket's hour.

## Later slices

Slice two, the metrics store (Debian's `victoria-metrics` in a private
network namespace behind a `systemd-socket-proxyd` unix socket), motel's
push, rules over its series, `metrics` in `GET /<app>`, the monitor
block's `metrics` line and `motel monitor setup --metrics`, is
notify/DESIGN-metrics.md. Its vmalert renders a
second `ChannelFailed` with motel's labels, which Alertmanager
deduplicates; it posts no `Watchdog`, which this section once promised:
with motel's labels it would either keep the heartbeat pinging while
motel is down or resolve motel's own (DESIGN-metrics.md, "Rules").

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
- 2026-11-10 — Reframed from proof of concept to experimental: the
  code is kept, not thrown away; the Owed rows that bar a production
  box (the `_box` slot, a missing secret refusing boot, the watchdog
  after a restart) are a follow-up PR before the next release.
- 2026-11-10 — The first Owed row landed: the box channel writes
  `_box.env` from `secrets/_box/` and reloads on its change, the
  handler's validate is told the names a push carries, and the `NOTIFY_`
  prefix is withheld from the env files in `/etc/motel/secrets/`, where
  the box writes them.
- 2026-11-10 — The Owed row "a missing secret refuses the boot" landed:
  the box refuses a push whose notify block names a secret it does not
  carry; validate and a reload refuse a missing one, and a boot drops
  its channel with an error line ("Secrets at load").
- 2026-11-10 — The Owed row "a restart while a state key fires" landed:
  liveswap keeps what its watchdog reported in `alerts.json` and
  re-reports it at notify's Start, as standing reports the engine
  ignores when it holds the key ("Behaviour"). With the two rows before
  it, the three that barred a production box are in.
- 2026-11-11 — /code-review xhigh of #424 and Copilot's third round:
  a boot drops every line of a secrets file it cannot use, a dropped
  secret's reason names every line that reads it, `alerts.json` keeps
  a refusal's release and reason for the standing report, a standing
  report's later lines are not standing, and a rollback writes what
  stands to the serving root ahead of any write still in flight.
- 2026-11-11 — The audit of hotserve-1 on the feature branch: a load
  with no heartbeat is warned, and the drop-in sets Alertmanager's
  storage to the 0750 this document gave it at every start (Debian
  ships 0755).
- 2026-11-11 — One block, `monitor`: the notify block's lines and the
  metrics store's `metrics` line, one Caddy app; `motel notify` is
  `motel monitor`, and `notify` and `metrics_store` are refused by name
  ("Decisions").
- 2026-11-11 — The operator's `rule` lines (DESIGN-metrics.md,
  "Operator rules"): `source: rules`, the `post_deploy_regressed` event,
  and its name reserved.
- 2026-11-12 — Copilot round 3 on #429, triaged: the disk source's
  statfs ran on the engine's goroutine, so a filesystem that hung held
  every post and the heartbeat until it came back (single fault, fixed):
  it runs as the disk series' does, each path on its own goroutine
  within 2 s, a path not back not measured, and none started again
  while one is out.
- 2026-11-13 — The Owed row "a `state_dir` line renders where setup's
  Alertmanager never reads" (4 5 5 = 100, a late finding on #433)
  landed (#438): the line is refused at the adapt, by name and at its
  line, saying the directory is fixed at
  `/var/lib/motel-notify`, the one setup makes and the drop-in's
  `--config.file` names; tests stand a directory in through an
  unexported variable, and setup's last line names the fixed path
  again (undoing aa097e8). Not taken: refusing only a path the drop-in
  does not name, and setup writing the path into the drop-in (the row's
  reasons, in git).
- 2026-11-13 — The `alertmanager <socket path>` line, which the row
  above left to the same PR to weigh, went with it (#438): setup's
  `motel-am.socket` fixes the socket at `/run/motel-notify/am.sock`, a
  path with no listener reads as "Alertmanager loads the file when it
  starts" (notify/state.go `apply`), and each post then fails in the
  journal (4 5 3 = 60, size 1, risk 2). The line is refused the same
  way, saying where the socket is fixed; tests stand a socket in
  through a second unexported variable.
