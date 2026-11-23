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
trust, the filter it passes summaries through) and the backup record, never the other way
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
| Its listener | `/run/motel-notify/am.sock`, 0660 root:motel, a systemd socket unit (`notify/units/motel-am.socket`); no TCP port in the host's namespace. With the store, a second socket, `127.0.0.1:9093` inside the store's namespace, for vmalert (DESIGN-metrics.md, "The shape") | its own runtime directory: `/run/motel` is moteld.service's `RuntimeDirectory`, removed whenever moteld stops |
| Its config | `alertmanager.yml` and one `url-<KEY>` file per channel secret, 0640, rendered by motel at every config load | `/var/lib/motel-notify`, 2750 motel:prometheus (setup makes it); not `/var/lib/motel`, which is 0750 motel:motel and the prometheus uid cannot traverse |
| motel's part | the `monitor` block's notify lines and their refusals, the renderer, the engine (state table, flapping guard, mute, heartbeat, retry queue), the detectors, `channel_failed`, five admin routes, `moteld monitor send`, `test`, `setup`, `silence` and `rules` | `notify/` |

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

What `moteld validate` refuses (Provision, which writes nothing and
connects to nothing), each naming keys and channels, never a value:

| Condition | Refused as |
|---|---|
| a `notify` global option (refused at the adapt, before Provision) | ``the notify block is now `monitor { … }`, with the same lines but state_dir and alertmanager, whose paths are fixed; to move a box onto it, docs/upgrading.md, "From the notify and metrics_store blocks"`` |
| a `state_dir` line (at the adapt, naming its line) | `state_dir is not a monitor line: the state directory is fixed at /var/lib/motel-notify, the one moteld monitor setup makes and Alertmanager reads; remove the line` |
| an `alertmanager` line (at the adapt, naming its line) | `alertmanager is not a monitor line: Alertmanager's socket is fixed at /run/motel-notify/am.sock, the one moteld monitor setup's motel-am.socket listens on; remove the line` |
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

Nor does Alertmanager follow a redirect, which would take a notification
past the check to wherever the endpoint names. Its default follows one:
measured on Debian's 0.28.1 in the e2e box (2026-11-20, a stub answering
307 to another port on loopback), a webhook, a Slack and a Discord
receiver each replayed the POST, body and all, to the second port. The
render writes `follow_redirects: false` in the global `http_config`,
which every receiver without one of its own takes, and in every
receiver's own (an ntfy channel's, for its `X-Priority`), which starts
from Alertmanager's default and not from the global one (measured: with
only the global line, that receiver still followed).
`TestAlertmanagerFollowsNoRedirect` asks it of Alertmanager on every run
of the metrics e2e box, a receiver of each kind against a redirecting
stub. A channel whose endpoint redirects fails, named: Alertmanager
counts the redirect as a failed notification (`unexpected status code
307`, not retried; `reason="other"`), which motel journals and tells as
any failed delivery ("Failed delivery"). The operator writes the URL the
endpoint answers at.

### Secrets at load

The last two rows are a secret's, and a boot does not refuse them.
Provisioning cannot tell `moteld validate` from a config load (Caddy's
`Context` says neither), so cmd/moteld tells notify the command it runs
(`SetCommand`, before Caddy's own command), and a load reads Caddy's
running config (`caddy.ActiveContext`):

| Load | A channel whose secret is missing, or whose value is refused |
|---|---|
| `moteld validate` (by hand, CI's `make check`, the box handler's step 6) | refused, as the table says; the handler and `make check` name the secrets in `secrets/_box/` (`MOTEL_NOTIFY_PUSHED_SECRETS`), taken as held ("Secrets"). validate runs as root, who reads any file, so it also refuses a secrets file the motel user could not read, by its owner, group and mode against motel's uid and groups (`motelCannotRead`): root:root 0600 after an `mv`, which the server's own read would refuse at a reload and drop every channel over at a boot |
| a reload (`moteld reload`, `systemctl reload`, the box applier's): a config is running | refused; the running config serves on, and the box applier rolls its push back |
| the first load of `moteld run` (a boot, a restart, an unattended upgrade's): nothing running | the channel is **dropped** from that load (from the operator line, each binding, the route tree; the heartbeat, if it is its secret), each a `monitor: a channel is dropped from this load` error line naming every line that reads the key, and the key, never a value; and one more if no operator channel is left. A secrets file that cannot be read or parsed (a line without `=`, over 1 MiB, unreadable) is every secret unusable: one error line names the file and its error, and every line that reads a secret is dropped. A malformed line is named by the file and its line number; its text is quoted only when it looks like a mistyped name (liveswap's `invalidEnvKeyMsg`), so a channel URL written without its `NOTIFY_…=` is not. motel serves |

Refusing a boot would take every site down for an alert channel; a
dropped channel is the one cost, said in the journal, and the next
`moteld validate` or reload says it again. This is the rule cmd/moteld's
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
every summary passes through. So is the credential part of each ntfy,
Slack or Discord channel's URL, the last non-empty path segment before
a fixed suffix (ntfy's `/publish`, `/send`, `/trigger`; Discord's
`/slack`, `/github`), as written and decoded, and an ntfy URL's `auth`
parameter as a whole: a topic on public ntfy.sh, a hook's token, the
`auth` value; and the heartbeat's ping URL's the same way, past
Healthchecks' `/start`, `/fail`, `/log` and exit status (0 to 255):
its check UUID. Each is `partial:<KEY>=<part>`: typed alone in a `moteld monitor
send` summary or a detail it is masked as
`[redacted:partial:<KEY>]`, and the whole URL, a longer form taken
first, still as `[redacted:<KEY>]` (notify/app.go, `knownValues`, the
one place notify builds the filter's values). The `partial:` name is a
marker's label, never a key: what checks the secrets reads the file's
map. A json channel's path is an arbitrary endpoint's and is not taken.
The limits are the filter's: a part under 8 characters is not
masked, and a topic that is an ordinary word of 8 or more is masked
wherever that word appears. motel writes
each value once more, to its `url-<KEY>` file in the state directory,
because Alertmanager's `*_file` fields read a file: that is the copy
the prometheus uid reads, and the config itself holds paths only.

**The box channel writes the file**: a signed push's
`secrets/_box/NOTIFY_*.age` (#418's reserved slot; box/DESIGN-box.md,
"The box's own slot", which reverses its open question 5 for this one
use) is decrypted by root into `_box.env` as an app's secrets are into
its env file, and a push that changes the file reloads moteld, so a new
URL is rendered at once (`make secret APP=_box VAR=NOTIFY_OPS_URL` in
the box's repository; docs/secrets.md). The box handler validates a
push before root writes its values, and cannot read them: `moteld
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
check` warns of it before the push (`moteld box describe`) and `make
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
| 2 | `notify_channel=~".+"` (ChannelFailed, NotifyTest), one child per channel; ChannelFailed's read-only copy (`notify_channel="-"`, which no channel can be named) matches no child and ends at this route's own blackhole ("Failed delivery") | `ch-<channel>`; the read-only copy `blackhole` | `group_by ['...']`, wait 0 s, interval 1 m |
| 3 | `alertname="deploy_in_progress"` | blackhole: it exists to inhibit | — |
| 4 | `kind="state", alertname="disk_low"`, the operator's audience | `state-<severity>-<channel>` | wait 30 s, interval 1 m, repeat 24 h |
| 5 | `kind="event"`, an audience per app with channels of its own, then the operator's | `event-<severity>-<channel>` | `group_by ['...']`, wait 0 s, interval 1 m |
| 6 | `kind="state"`, the same audiences | `state-<severity>-<channel>` | wait 30 s, interval 1 m, repeat 4 h |
| 7 | anything else (nothing motel posts), the operator's channels | `fallback-<channel>` | the root's |
| root | — | blackhole, which every route without a receiver inherits | `group_by [alertname, app, subject]`, the state timings |

Plumbing first, so nothing below can swallow it. **Every receiver
holds one channel** (and the heartbeat's its URL), so that
Alertmanager's `alertmanager_notifications_failed_total{receiver_name=…}`
names the one channel that failed ("Failed delivery"). An audience is
a route with three children, `severity="critical"`, `severity="warning"`
and one with no matcher (info, and any other severity), each with one
leaf per channel that takes that severity, every leaf under
`continue: true`; `continue` is on those leaves alone, so an alert
reaches every channel of one audience and no other audience. A
severity no channel of the audience takes keeps its route with no
leaf, and ends at the blackhole there rather than falling through to
the info channels. An app's audience is the operator's channels plus
every channel bound to an identity the app trusts, each only at or
above its floor; an app with no binding has no audience of its own
and its alerts take the kind's, the operator's. A receiver is one
channel's for one use and shared by every route that sends that use
to it (`state-warning-ops` serves the box's and every app's state
warnings, and disk_low's): a channel has at most eight (`ch-`,
`fallback-`, three `event-` and three `state-`). State and fallback
receivers `send_resolved`; event and direct ones never do. The
inhibit rule:
`source_matchers [alertname="deploy_in_progress"]`, `target_matchers
[alertname=~"watchdog_.*"]`, `equal [app]`, with no severity matcher
(#418's watchdog events are critical). Slack and Discord get a fixed
title and text built from motel's summary; json and ntfy the payload
whole; in a grouped Slack or Discord message each alert that has resolved is marked `[RESOLVED]`. Nothing in the block is a template.

Sibling leaves have the same matchers (none), so their groups share
one group key; Alertmanager's notification log keys an entry by the
group key and the receiver, so the log keeps them apart (the e2e
suite's "what was delivered once stays delivered once" holds with
them). The split is smaller than the composed receivers were, since
each channel's integration block (Slack's templates among them) is
written once per use rather than once per audience, and has more
routes (measured 2026-11-13: hotserve-1's one ntfy channel, 13 → 10
receivers, 14 → 28 routes, 4327 → 4245 bytes; three channels, two the
operator's and a deployer's bound to two apps, 27 → 23 receivers, 28 →
85 routes, 20844 → 13735 bytes).

The timings are measured (2026-11-10, hotserve-1, Debian 13 arm64):

| Fact | Measured | Consequence |
|---|---|---|
| Events grouped `alertname, app, subject` | of four distinct one-shots posted 5 s apart, two delivered: the second in a live group waits for the next tick, its `endsAt` passes, and with `send_resolved: false` it is never sent | `group_by ['...']`; one-shots carry `endsAt` +5 m |
| Events with `group_by ['...']`, wait 0 s | four distinct one-shots delivered 0 ms after their posts (the stub's clock); an exact duplicate deduplicated, and any repeat with the same labels held back until five to six minutes after the last (measured 2026-11-14) | the event route; `occurred_at` on every one-shot ("Events") |
| A recovery on a 5 m `group_interval` | delivered 290 s after the resolve | state routes take 1 m: measured 50 s |
| `endsAt=now` on a firing alert | resolved at once (the younger resolved alert wins the merge) | motel resolves explicitly |
| Default route | 30.02 s `group_wait` | the state wait |
| Event latency, post to the webhook's request | 6 to 7 ms; to a real ntfy.sh topic, 1.6 s | — |
| `POST /-/reload` over the socket | 200, live config replaced | how motel applies a render |
| `amtool` | has no unix-socket support | tests run `amtool check-config` and `amtool config routes test` offline on the rendered file (thirteen label sets) |
| The corrected tree with two apps | every alert reached only its receiver; `watchdog_*` of the deploying app suppressed, `inhibitedBy` the mute | e2e reproduces it ("e2e rows") |
| A `watchdog_*` resolve posted under its app's mute (measured 2026-11-14: Debian's 0.28.1 on the golden render, a stub receiver) | `alertmanager_notifications_suppressed_total{reason="inhibition"}` rose by one per leaf at the group's next flush and the alert was gone; no `[RESOLVED]` reached any channel then, or in the 140 s after the mute's end. Kept firing under the mute and resolved right behind its end instead: one `[RESOLVED]` per channel 33 s later, at the group's next tick; fired and resolved within the mute: nothing told. A resolve posted 20 s before the mute, before the group's next flush: suppressed and dropped the same way. Posted again at the mute's end, it or one posted under the mute: one `[RESOLVED]` per channel (0 s and 32 s after); posted again after its `[RESOLVED]` was sent: nothing more | the engine holds such a recovery to the mute's end ("Behaviour", the mute) |

## Events

`Event` (notify/engine.go) is what a detector reports. Its key is
#418's, `<source>/<subject>/<name>[/<instance>]`, and names what is
wrong, never when. Labels: `alertname`, `kind`, `severity`, `subject`
(`box` or `app:<a>`), `key`, `source`, `app` (an app event only: it is
what routes to the app's channels, so a box event about an app names
it in `instance`), `instance`. A one-shot also carries `release` and
`deployed_by` as labels; a state alert carries them as annotations, so
a deploy does not change its identity.

A one-shot carries `occurred_at` too: its `startsAt`, in UTC to the
nanosecond (`2026-11-14T10:03:00.123456789Z`), set once when
`oneShotAlert` builds it. It is the wall clock's reading, unless that
is no later than the last one-shot's and less than a second before it
(two in one clock reading, or a step back of under a second): then it
is a nanosecond after the last one's, so `startsAt` can be up to a
second ahead of the wall clock. A step back of a second or more is
taken as the clock reads. Alertmanager takes an alert whose labels are
those of one it holds as that same alert, and sends nothing for it, so
without the label a repeat inside the last one's five minutes was held
back until five to six minutes after the latest ("The route tree", the
measurements). With it every occurrence is delivered ("Decisions"),
and the queue's retries of one post, which send the same alert value,
stay one alert. Nothing
else carries it: not a state alert, whose identity holds across its
re-posts and its resolve, nor the mute, the heartbeat, `NotifyTest`
(its nonce does it) or `ChannelFailed`, whose labels vmalert's rule
copies so that Alertmanager merges the two (DESIGN-metrics.md,
"Rules"). No route, silence or inhibit rule matches on it, and Slack's
and Discord's title and text do not show it; a json or ntfy channel
gets it in the payload. The filter has nothing in it to mask (no run of
20 characters it scans). What bounds a burst is motel's limit per key
("The flapping guard"), which lets no more than four through in any
six minutes, so a key holds at most four alerts at once for their five
minutes. Each alert has a group (`group_by ['...']`) and a
notification-log entry for each channel's leaf it reaches, the entry
kept for twice the event route's `repeat_interval` (2 h): up to twelve
entries per key and leaf. The keys are bounded by their sources: liveswap
has a few per app, and a producer's vary with its `--instance`, which
only root and the motel uid can send.

| Event | Source | Kind | Severity | Fires | Resolves |
|---|---|---|---|---|---|
| `watchdog_throttled` | liveswap | state | critical | the restart budget is spent; again on any failed probe or crash during a recovery's hold, below the restart threshold too | the first healthy probe after it, held `max(30s, watchdog_grace)`; a deploy that succeeds; `watchdog off` |
| `relaunch_refused` | liveswap | state | critical | the watchdog's relaunch is refused at its pre-flight (its reason without the deployer's text, as for `deploy_refused`) | any path publishes an instance |
| `deploy_failed` | liveswap | event | warning | a 5xx outcome, a push's upload that could not be staged, or app dirs that are not motel's; the summary names the version and the phase, never the error's text, and says nothing of what serves now | — |
| `deploy_refused` | liveswap | event | warning | a 422 (liveswap's own words, through the filter: a refusal whose message quotes the deployer's text, the artifact URL or a path in the release, is told without it, which the 422 and the journal keep) | — |
| `rollback` | liveswap | event | warning | `?rollback=` succeeds | — |
| `deploy_succeeded` | liveswap | event | info | a deploy succeeds; dropped unless `deploy_succeeded on` | — |
| `deploy_in_progress` | liveswap | the mute | none | a deploy starts; re-posted each minute while it runs | it ends, whatever it came to |
| `backup_*[/<app>]` | backup | state | the level written at its rule in `status` (below) | each problem line of `motel-backup status`'s verdict, while it stands: the alert its rule names, with the app as its instance where the line is an app's | status no longer says it |
| `backup_unreadable` | backup | state | warning | status could not tell: the record does not parse, or the credential file cannot be looked at (an `Lstat` refused, an `EIO`); the summary says which, the file's pointing to `motel backup status`, which reads moteld's look and says why, then to root's `motel-backup status` on the box, which can look at the file and says the backups' health; what fired stays fired beside it | the file is looked at and the record reads (or the file is not there) |
| `cert_failed/<identifier>` | tls | state | warning; critical with under 7 days left | CertMagic's `cert_failed` | its `cert_obtained`, or 13 h with no report (a name no config manages any more) |
| `disk_low/<path>` | disk | state | warning at 80 % used, critical at 90 | statfs, every minute, one per device, of the liveswap root, Caddy's data and the state directory, each on its own goroutine within 2 s (a path not back is not measured, and starts no other statfs until it is back); the level is held per device, whichever path stands for it | five points under its threshold |
| `Watchdog` | notify | heartbeat | none | every minute, alive 4 m | — |
| `ChannelFailed` | notify | direct | warning | Alertmanager's failure counter rose ("Failed delivery"): a copy to each channel told (the key's bucket bounds them), and the read-only copy, told to no channel, at every rise; the read-only copy again at every read while a hold stands, from the hold's start | `endsAt` +15 m from its last post; the read-only copy of a hold when the hold's record ends |
| `person_new_network` | access | event | warning | a person's key accepted from a network (`client_ip` cut to a /24 or a /48) it was never accepted from, its first use among them (liveswap/access/DESIGN-access.md, "A key used from somewhere new"); instance the key's fingerprint, so the limit per key bounds a thief who cycles networks; no `app` label, whatever the person may read. One told while no config runs notify (a start's first seconds) is kept, up to 64 and four a key, and told when one does, if within a minute (`engine.early`); an older one has its journal line alone. Neither, nor a silence of either, is in the people's alerts reads (`personAlerts`) | — |
| `person_outside_list` | access | event | warning | a valid signature by a person's key from outside every address list on their grants, refused with the flat 401; at most once per key an hour; instance the key's fingerprint | — |
| `post_deploy_regressed` | rules | event | warning | a `post_deploy` rule's checks after a deploy or a rollback of the app failed `failures` times in a row: the new release over a bound and worse than the one it replaced (DESIGN-metrics.md, "Operator rules"); once per deploy, and it never acts | — |
| `<rule name>` | rules | state, by vmalert | the rule's | an operator's `threshold` or `disk_fill` rule holds for its `for:` (DESIGN-metrics.md, "Operator rules"); `subject` is the app's, or `box` for a disk | vmalert resolves it |


### Backup alerts: status is the only judge

`motel-backup status` is the one judge of the backups (owner's
decision, 2026-11-21; #590). Each rule in `status.Report` that makes
the box unhealthy names its alert and its level at the rule
(`bad("backup_stale", AlertCritical, …)`), and `status.Alerts` lists
them; `TestEveryRuleNamesAListedAlert` holds the rules to the list both
ways, so a new rule without a listed name fails a test. notify
keeps no rule of its own: `backupPoller` takes status's verdict on the
look any account on the box takes (`backups/look`, which `GET
/_motel/backups` reads), but for the running units, which status says
and never judges, and raises one alert per problem line. The names
are notify's own (the rules listing, the silences, the names an
outside producer may not post) through `backupRaised`, built from
`status.Alerts`.

| Alert | Level | status's rule |
|---|---|---|
| `backup_never_ran` | critical | no run yet, and backups were set up more than 3 h ago |
| `backup_not_running` | critical | no run has finished for more than 3 h |
| `backup_stale/<app>` | critical | the app's last complete backup is more than 3 h old |
| `backup_never_complete/<app>` | critical | the app is past pending with no complete backup |
| `backup_no_result/<app>` | critical | the app has no result on the record |
| `backup_run_failed` | warning | the last run ended early |
| `backup_run_failed/<app>` | warning | the last run failed for the app (failed, incomplete, not attempted) |
| `backup_data_missing/<app>` | warning | the app's data was not there to back up |
| `backup_snapshot_gone/<app>` | critical | the app's last complete snapshot is no longer in the repository |
| `backup_repository_unchecked` | critical | no check has come to a verdict for over 8 days since drills that check began; never checked, set up over 8 days ago with no drill since; or the last clean check is over 8 days old |
| `backup_repository_check_failed` | critical | the last check ended other than clean (damaged, unreachable, no repository, wrong password, failed) |
| `backup_repository_unlisted` | critical | the repository has not been listed for over 3 h |
| `backup_drill_failed` | critical | the last drill could not begin |
| `backup_drill_failed/<app>` | critical | the drill of the app's newest snapshot proved nothing (an app over 1 GiB, whose first backup the run does not drill, is this until the weekly drill) |
| `backup_restore_proof_old/<app>` | critical | the app's last proven restore is over 8 days old |
| `backup_restore_unproven/<app>` | critical | the app has snapshots and no drill of it has run |

- **The poll**, every five minutes and once at start: whether the
  credential file is there (its name alone, `Lstat`; theEngine's
  `/etc/motel-backup/repository.env`, a test's none), then the record,
  in look's order. No file: backups are not set up, nothing fires and
  whatever fired clears. A file that cannot be looked at, or a record
  that cannot be read: no verdict, `backup_unreadable` says which, and
  what fired stays fired beside it, never told resolved.
- **The summary** is `motel backup status (on the box: motel-backup
  status) says: ` and status's line: the laptop client first, the box's
  command second, then the line, so the cut at 200 characters takes
  the line's tail, never the pointer. The line is status's own words,
  restic's detail and paths included: the record holds no secret.
- **Each problem starts and ends by itself**: its own key, so a second
  problem beside a first is a new alert, told at once (its own group in
  Alertmanager when its name differs; the same group, re-sent with it
  added, when it is another app's of the same name). Ten apps gone stale
  in one run are ten keys, each with its own limit, posted together and
  grouped by name into one message.
- **A restart** needs no memory: the first poll finds every problem
  that still holds under the same key. What it cannot find is an
  accepted row below (a record unreadable at the restart).
- **A silence** takes one name (`--alert backup_stale`): every app's
  alert of that name, never one app's alone, since a box alert carries
  the app as its instance.
- **The rules listing** lists each name as a `motel check`, with each
  app's backup age beside `backup_stale`; the others are events with no
  number to be close to.

Sources are `liveswap`, `access` (a person's key, through
`liveswap.Announcer`, which Provision installs for the process: the
record of networks is the process's, across loads), `backup`, `tls`,
`disk`, `notify`, `metrics`
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
no backup credential file has no backups set up and says nothing
("Backup alerts: status is the only judge").

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
  `/metrics`. A refusal (4xx) is given up at once; a 5xx is retried like a refused connect. A post whose own
  `endsAt` passes while it waits (a one-shot's 5 m, `ChannelFailed`'s
  15 m, the heartbeat's 4 m; on the wall clock, as Alertmanager reads
  it) is given up the same way, not posted: Alertmanager would take it
  as resolved and tell no one. An end (a resolve, the mute's end) is
  queued with `endsAt` at its queuing and is not given up for it. Socket activation
  restarts Alertmanager in about a second, so the usual outage is one
  failed connect.
- **The mute** is `deploy_in_progress` for the app, posted at a
  deploy's start, re-posted each minute, ended at its end. It lives
  as long as the deploy, which liveswap's `deadline` bounds (on
  pre_start and on the health gate, 5 m each by default). The inhibit
  rule mutes the app's `watchdog_*` alerts under it, and Alertmanager
  mutes a resolve as it does a firing alert, then drops it as sent: no
  `[RESOLVED]` comes, then or after the mute's end ("The route tree",
  the measurements). So a `watchdog_*` recovery whose hold ends while
  its app's mute stands is held (`muted`), kept alive by the re-posts,
  and posted right behind the mute's end, whatever the deploy came to;
  fired again meanwhile, it has no recovery to post. Alertmanager
  judges the mute when the group flushes, up to a `group_interval`
  (1 m) after the post, so a resolve posted just before a deploy starts
  is swallowed the same way, the held one too when a second deploy
  follows the first within the minute: the engine keeps each
  `watchdog_*` resolve it posted while a post of it waits in the queue
  and for `endWindow` (2 m) after Alertmanager last took one, a mute of
  its app that starts in that time marks it, and the mute's end posts
  it again, unless its key has fired since. A resolve the queue gave up
  every post of is forgotten, as any failed delivery; one a full queue
  dropped while Alertmanager was taking it is kept. Alertmanager's notification
  log sends a resolve posted again once, and nothing for one it had
  already sent (measured, "The route tree"). A firing alert
  needs nothing: Alertmanager tells it at its group's first flush after
  the mute's end if it still fires (the recovery's hold not yet over),
  and one whose recovery's hold ends within the deploy reaches no one.
  A reload that drops the app ends its mute before it resolves the
  app's keys. The engine can start with no mutes while Alertmanager
  keeps one to `resolve_timeout`: at a process's start (a deploy ends
  with the process that ran it), and when a reload brings the block
  back after one took it out (`release` forgot the mutes). So whenever
  the engine starts, it ends the mute of every app its config runs, in
  one post before any other.
- **Summaries** pass through liveswap's filter, primed with every
  channel URL (and, at liveswap's end, with the app's own values), and
  are cut to 200 runes.
- **A config that drops a source** ends what nothing can clear any
  more: the keys of an app it no longer runs (and its mute), the backup
  keys once `backup_record off`, a filesystem's once its path is not
  measured; each fired key is resolved with a journal line, once the
  config serves: a reload's at the old config's Stop, and a poll that
  was reading then tells nothing of what it dropped. An
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
  state directory and in Alertmanager. What the new one drops (above,
  and a heartbeat hold on a channel it no longer has) is ended only at
  the old one's Stop, and the pollers read the old one's sources until
  then, so a fallback finds it as it was: Caddy has no hook that says
  a config serves. When the last config running
  notify stops, the engine's table, mutes and buckets are emptied, and
  each post still queued is given up as above (at moteld's shutdown
  too): a block added back later starts clean. When that stop is a reload's
  (a config serves on without notify: the block or its channels taken
  out), Alertmanager is given setup's config that sends nothing and
  the URL files are removed (`applyNothing`): what it holds ends at
  `resolve_timeout` through the blackhole, where the last render's
  receivers would have told every channel a recovery that did not
  happen. moteld's own shutdown leaves the render, so that a restart
  keeps what fired: Caddy empties its running context before it stops
  the last config's apps, so `configRunning` is a reload's alone at
  Stop, as for the store's rules (DESIGN-metrics.md, "The metrics
  line"). A restart or a boot into a config that runs no notify starts
  no App to do it, so cmd/moteld's gate does it at `moteld run`, as it
  retires the store's rules (`notify.SendNothing`), reloading
  Alertmanager only when the state directory changed, so that a box
  whose block never had channels starts no Alertmanager for it.
  Both of a start's writers act on the state directory only as its
  owner, the motel user moteld.service runs as (`notify.NotOwner`):
  the gate, for a config that runs no notify, and Start, for one that
  does, which then starts no engine, so it writes, reloads and posts
  nothing. Root trying a file by hand on a live box leaves this
  directory and Alertmanager as they are, whatever the file's monitor
  block, and is told so; the motel user by hand is not told apart
  (Accepted residuals).
  `moteld reload` of a Caddyfile with a monitor block passes
  `--force`, as `systemctl reload` does: the block reads the secrets
  file, which the config does not hold, so a mended `_box.env` would
  otherwise load nothing (cmd/moteld's gate, `reloadForces`).
- **A certificate no config names** is never obtained and never
  reported again, so no `cert_obtained` clears it. CertMagic reports
  `cert_failed` on every failed attempt for a name it manages, and its
  longest retry interval is six hours; a `cert_failed` key unreported
  for 13 hours is taken as cleared, with a line, and resolved after the
  hold.
- **The watchdog turned off** while a restart loop is reported ends the
  report (`watchdog_recovered`): no probe would ever say so.
- **A moteld restart** empties the table while Alertmanager keeps each
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
- **A producer's state** (`moteld monitor send --state`) has no detector
  in motel to tell it again, so the engine keeps it the same way
  (notify/external.go): `external.json` in the state directory (0600,
  motel's alone) holds each producer's key with its severity, its
  filtered summary and details, and whether it fires or its recovery's
  hold runs; written by rename on each change (a repeat that changes
  nothing writes nothing), with no flush, as liveswap's own files: the
  write runs under the engine's lock, and a power loss that leaves the
  file empty is a file that does not read, below. Removed when none
  stands. A write before setup has made the state directory is tried
  again at each tick, unsaid (`send` already says the box is not set
  up), so a state sent before setup is kept once it runs. At the first Start
  of a config with the block in a process, each is told again as a
  standing report, before the file is written again; one for an app
  the config does not run is resolved, as at a reload. A file that does
  not read, or holds an entry that is not a producer's, is said in the
  journal, taken as nothing and removed; a failed write is said, and the
  file stays as it was until the next change. A reload that takes the
  block out removes it with the table, and so does cmd/moteld's gate at a
  start without the block (`SendNothing`): a later block must not tell
  them again. moteld's shutdown keeps it.

## Failed delivery

Once a minute motel reads Alertmanager's own `/metrics` over the
socket, with Prometheus's text parser (`prometheus/common/expfmt`,
already in the binary through Caddy's metrics). The read is strict: a
body cut short, a malformed line, or one without the failed counter is
no reading, and the baseline stays the last good one. `alertmanager_notifications_failed_total` names the integration
and, with the drop-in's `receiver-name-in-metrics`, the receiver. A
rise is a `monitor: channel failed` line, and one
`ChannelFailed` alert to each operator channel that is not a suspect
and whose `severity` floor takes a warning, which `ChannelFailed` is
(`failureAudience`, vmalert's rules' too), each routed to that channel
alone (`notify_channel=`). The suspect is
the failing receiver's one channel (`render` returns each receiver's
channel; "The route tree"), so a working channel of the same
integration beside it (ntfy and json are both the webhook) is told,
not blamed; when the counter names no receiver (a drop-in without
`receiver-name-in-metrics`) the suspects are every channel of the
integration. A delivery Alertmanager's own reload cancelled is counted
too, under reason `contextCanceled` (the reason only when no attempt
failed before the cancel), and with `receiver-name-in-metrics` after
the reload has reset the counters, so it reads as a rise; it is no
channel's failure, and the new config sends it again wherever it still
routes it, so neither this read nor vmalert's rules count it.

**The read-only copy.** Each rise also posts one more `ChannelFailed`,
with the same labels but `notify_channel="-"`, whoever else is told
(`readOnlyCopy`). `-` is no channel's name (`channelNameRe` starts with
a letter or a digit), so route 2 matches it, none of its leaves does,
and it ends at route 2's own blackhole ("The route tree"): Alertmanager
holds it, `active`, and no channel is told it. It needs no route of its
own, so no render (an older moteld's, after a downgrade, too) can send
it anywhere else; without the label it would fall to the fallback and
reach every operator channel, the failing one and one kept for
criticals too (measured 2026-11-21 on Debian's 0.28.1). The blackhole
has no integration, so Alertmanager keeps no counter and no
notification-log entry for it (measured): the copy is no failure, no
delivery, and ends no hold. It exists for the people's reads: on a box
whose one operator channel fails, it is the only `ChannelFailed`
Alertmanager holds, so `GET /_motel/alerts` (the TUI's title bar and
bell, `:alerts`, `motel monitor alerts`) shows the failure, where
before the read had nothing of it. It is posted at every rise, the
ones the key's bucket keeps from the channels too (it tells no one), so
it does not lapse between two posts while the failure goes on; when
channels were told, the last post lives its 15 minutes. While a hold
stands ("Dead-man and uptime") the copy is posted again at every read
from the hold's start, carrying the latest rise's words, so it stays
one alert with one start for as long as the hold does, and it is
resolved when the hold's record ends (a delivery through the channel,
or a load whose config no longer has it); a later rise of the same
failure, whether or not a load has since given it a channel to tell,
gives the record its words, and one of another integration (a load
changed the channel's kind) resolves the old integration's copy, which
is another alert, as the record moves on. A record that stands across
a load that gave its failure channels to tell keeps the copies it then
told them, which the read folds under the read-only copy, and resolves
them with it at its end; their receivers send no resolved, so no
channel hears the end. A delivered `moteld monitor test <channel>`
resolves the copy naming that channel alone, its labels built as the
failure's own (`endTestedCopy`), so the documented mend ends the row at
once even after a restart of moteld, which drops the records it would
otherwise end by; the copies addressed to the other operator channels,
which the read folds under the read-only one, are resolved with it,
and so is every copy Alertmanager holds naming that channel alone as
the suspect, by its own labels (one addressed to a channel a load has
since taken out or set to criticals alone, a read-only one of the
channel's kind before a load) (measured on 0.28.1: a resolve of a copy
Alertmanager does not hold lists nothing, and the `suspects` filter
returns those copies alone). With the metrics package and another operator channel
the test resolves nothing: vmalert's rules post the addressed copies
again while their window holds the failure, and with the read-only copy
gone the read would list each of them, unfolded. Alertmanager keeps the
earliest start of the posts it merges, so a re-post never moves it
(measured). A post that carries the copy alone is given up at debug
when Alertmanager does not take it, as the hold's re-posts are: no
channel was to be told. The box-wide read
leaves out the copies addressed to channels when it holds the read-only
copy of the same failure (`integration` and `suspects`,
`foldChannelFailed`), and sends the addressed ones as before when it
does not (vmalert's rules alone, while moteld was not running; a box
whose moteld is older). The copy carries no `app` label, so no app's
read has it, as no app's read has the addressed copies; its summary is
theirs, which a viewer of the box reads already wherever another
channel is told. It has no vmalert rule (DESIGN-metrics.md,
"ChannelFailed, a second source").

What a rise is rests on when the counters last started from 0.
With `receiver-name-in-metrics` Alertmanager 0.28.1 resets all three
at every config load (`notify.Metrics.InitializeFor`), its start's
included, and then sets
`alertmanager_config_last_reload_success_timestamp_seconds`; a reload
that fails does neither. So a read whose stamp is not the last
read's (`loadSince`; an earlier stamp too, after a clock step back) has a baseline of 0: every count is since the
load, and 1, a load, then one failure before the next read (1 again)
is a rise, where comparing with the last read alone saw none. The
first read after moteld starts is a baseline, unless Alertmanager
loaded after moteld's start, when it too counts from 0: the reload
moteld's start asks for (`noteReload`, which needs no clock), or a
stamp after the start (setup's start of Alertmanager, or its socket's
at motel's first post); a load from before
the start may have been told of by the moteld before. Otherwise a
value lower than the last started again at a load the read gives no
stamp of (an Alertmanager without `receiver-name-in-metrics`, restarted),
and counts from 0. The hold's deliveries and `moteld monitor test`'s
baseline read the same stamp, and vmalert's rule reads it beside the
counter (DESIGN-metrics.md, "Rules"). A key's
bucket bounds the storm. A failure with no
channel left to tell holds the heartbeat ("Dead-man and uptime"). This
needs no metrics package: the review's point that a notify-only box
would otherwise have no `channel_failed`.

## Dead-man and uptime

motel posts `Watchdog` every minute; its route sends it to the
`heartbeat` channel's URL every five minutes, a Healthchecks ping
(`https://hc-ping.com/<uuid>`, Period 5 min). The route's repeat (4 m)
is under its group interval (5 m): Alertmanager re-sends at a flush only
when its last send's log entry, stamped after the send returned, is
older than the repeat, so an equal repeat skipped every other flush and
pinged every ten minutes (Fable review of #422, from Alertmanager
0.28.1's dedupe stage). Silence means the box,
moteld or Alertmanager is down, which is the chain that delivers
everything else. Uptime stays an external keyword check per host.
A block with no `heartbeat` line loads, with its `Watchdog` routed to
the blackhole receiver, and every load (`moteld validate` too) warns
`monitor: no heartbeat in this load`, as for `allow_insecure_channels`:
nothing outside the box would hear that it stopped. A boot that drops
the line for its secret ("Secrets at load") warns the same.

**The heartbeat's hold.** A failure no channel can be told of
(`failureAudience` holds no channel but the suspects: a single
operator channel, the others taking criticals alone, or a deployer's
channel failing when no operator channel takes warnings) would be heard
nowhere while the check off the box stays green. So once such a
failure has stood 5 minutes (`heartbeatHoldAfter`) with no delivery
through the suspect's receivers since, motel stops posting `Watchdog`:
it ends within its 4 m life, the heartbeat receiver renders
`send_resolved: false` so its end sends nothing, and the dead-man
service alerts through its own channels (any service that takes a
ping URL; no new endpoint, no new line). The 5 minutes are for a
passing fault: Alertmanager logs no failed notification, so it sends
it again at the group's next flushes (one delivered before motel's read
of the failure is not counted: "Accepted residuals"). A delivery is an attempt that did
not fail, read from `alertmanager_notification_requests_total` and
`…_requests_failed_total` by receiver, which motel reads from
Alertmanager's own `/metrics` over the socket with the failed counter
(not from the store). The pings resume at the next delivery through
the suspect's receivers (`moteld monitor test <channel>` makes one), at
a load whose config no longer has the channel, once it serves
("Reloads"), or at a restart of
motel: the hold is in memory, as the counter's baseline is.
`monitor: heartbeat held: …` (warn) names the suspect, when it failed
and what resumes it, and `monitor: heartbeat resumed` says through
which receiver, or why. A failure with no suspect is the heartbeat's
own receiver's, which its check hears already, and holds nothing; so
does one whose receiver is not this config's (the counter names none,
or a name from before a reload): its suspects are a guess, and no
delivery is counted by channel to end a hold. In a block with no
`heartbeat` line, where no check off the box would hear a hold, a
failure is recorded but nothing is held; a load that adds the line
holds what stands at the next read, and one that takes it away ends
the hold (self-review of #449).
While a failure is recorded (held or not yet), motel posts its
`ChannelFailed` read-only copy at every read, from the time the failure
was recorded, and resolves it when the record ends ("Failed delivery"),
so the people's reads show it for exactly as long as the record stands;
it reaches the blackhole, which counts nothing, so the hold's start,
its 5 minutes and its end are what they were without it.
Runtime state, not a rendered inhibition: "a rise told to no one, and
no delivery since" is a reading of Alertmanager's counters over time,
which only motel's minute read sees on a notify-only box (Alertmanager
has no rule over its own metrics, and vmalert is optional); an inhibit
rule would still need motel to post a source alert from that same
state, and a render change. The state is one map in the engine
(`untold`, notify/channelfailed.go).

## External producers and tests

(`moteld monitor rules`, on the same admin socket, lists
every alert this file's detectors raise beside the store's rules, with
the number each detector last read: DESIGN-metrics.md, "How close each
alert is". The alert names and their first severities are one table,
notify/silenceable.go, which silence and the listing share.)

`moteld monitor send` posts to `POST /notify/events`, and `moteld monitor
test <channel>` to `POST /notify/test`, both `admin.api.notify` routes
on the admin socket: root and the motel uid reach it, no app does.
External producers never post to Alertmanager's socket on the
documented path; root can, as root can do anything. Both the command
and the route check (`SendRequest.Check`):

| Refused | Why |
|---|---|
| a source not `[a-z0-9-]{1,32}` (stamped `external:<name>`) | one grammar, so a key cannot pose as a detector's |
| a subject not `box` or `app:<an app the box runs>` | an app subject reaches its deployers |
| motel's own names: `Watchdog`, `ChannelFailed`, `NotifyTest`, `deploy_in_progress`, `watchdog_*`, the deploy events, `disk_low`, `cert_failed`, `backup_*`, and the metrics store's (`metrics_store_down`, `metrics_rules_down`, `metrics_store_read_only`, `metrics_store_killed`, `metrics_store_disk_fill`, `app_memory_high`) and the operator's rules' `post_deploy_regressed`, and a person's key's `person_new_network` and `person_outside_list` | a producer that could post them could mute an app, fake the heartbeat, or speak as a detector |
| a summary empty, on two lines, or over 200 characters; details over 2 KiB or with names not `[a-z][a-z0-9_]*` | #418's limits |
| a body with unknown fields (a raw Alertmanager label set) | the route takes the request, never labels |
| a name, instance or source the filter would mask (a channel URL or its topic or token, the heartbeat's URL or its last path segment, a secret-shaped token) | each is part of the key, the labels and the answer, which the filter does not rewrite |

Accepted, an external event is one like motel's own: the filter, the
bucket, the journal line, the queue. `moteld monitor test` posts one
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
`test` answers 503, both naming `moteld monitor setup`; `moteld validate`
and `moteld reload` warn (cmd/moteld's gate, `notify.LoadWarnings`). A
`resolved` that finds nothing firing under its key answers `resolved
nothing` with exit 0, since a producer may resolve whatever it last
said. `test` on a channel a boot dropped names it dropped, and the
mend; on a name the block does not have, it lists the block's channels
and not the name given. No refusal of these two routes echoes a value
given that is not one of motel's own names (a channel, an event's or a
detail's name, an app): Caddy journals a route's refusal (`admin.api`
`request error`), and the value may be a channel's URL or topic pasted
for a name. Every load's gate, `run`'s among them, warns when a unit
setup wrote differs from the one this binary would write, naming the setup
to run again: an upgraded box keeps the old units until root does.

## Silences

`moteld monitor silence add [--alert <name>] [--app <name>] [--release
<version>] --comment <text> [--duration 2h]` posts to `POST
/notify/silences`, and `moteld monitor silence expire <id>` to `POST
/notify/silences/expire`, two more `admin.api.notify` routes. The route
checks the request again (the command's check names a mistake of shape
first, as `send`'s does), holds each name to a closed set of the loaded
config, builds the matchers itself, writes the silence to
Alertmanager's v2 API (`POST /api/v2/silences`, `DELETE
/api/v2/silence/{id}`) over the socket, and once Alertmanager has taken
it writes one line in moteld's journal: `monitor: silence added` (`id`,
`alert`, `app`, `release`, `ends_at`, `created_by`, `comment`) or
`monitor: silence expired` (`id`, `expired_by`, in `created_by`'s
form). A refused or failed write leaves no line; the command says why
and exits 1. The add posts without the caller's cancel
(`context.WithoutCancel`), bounded by the client's 10 s alone, so that
a caller gone once motel has the request (Ctrl-C, a dropped `ssh`),
during the dial or while Alertmanager answers, does not cut it: the
route runs to Alertmanager's answer and writes `silence added` with the
id whether or not anyone reads the reply. Caddy cancels no admin
request at a reload; at moteld's stop it stops the apps, then waits up
to 10 s for admin requests before it exits, and systemd kills it at
`TimeoutStopSec=5s` (packaging/moteld.service): an add Alertmanager has
not answered by then writes no line ("Accepted residuals"). It is not
tied to the engine, which it does not post through: the silence is
Alertmanager's, which outlives a stop of the engine, and a cut at a
stop would lose the id this exists to keep. An add whose answer never
came (the client's 10 s, or the connection closed after the request:
`answerLost`, which judges by the error alone, so that a timeout before
the request was sent counts too) writes no `silence added` line either,
since the silence may not be there, but a warning, `monitor: silence
add unanswered: Alertmanager may hold the silence`, with the line's
fields but the id, so that a silence Alertmanager did take still has
its record; and its 502 says that Alertmanager may have taken it and
gives the command that lists what it holds, so that the operator
neither adds it twice nor believes nothing quiet. Whether Alertmanager
0.28.1 stores a silence before it answers is not measured, and the text
is true either way. A refused connection or a non-2xx answer took
nothing, and says only that.

Through the route, not from the command to the socket, because that is
how `send` and `test` write, and so one code path holds every write
motel documents: the line is motel's, in its unit's journal beside the
events, written by the process that holds the filter's known values and
the loaded config. A command that posted to `am.sock` itself would log
to its own stderr, which over `ssh root@box moteld monitor silence …` is
the laptop's terminal, not the box's journal. A later laptop client
runs these commands over ssh with `--json`, so it gets the same checks
and the same line, and never Alertmanager's raw API. root can still
post to the socket, as root can do anything; such a silence leaves no
motel line.

**No free-form matchers.** The first shape took amtool's matchers
(`label=value`, `=~`, `!~`) and held them to the output filter, which
is best effort by design: four review rounds and Copilot found ways a
channel URL or its token could pass in a regex or an arbitrary label
(escaped dots, a fragment, a label the filter never sees as a URL).
The cause was the filter used as an input validator over a regex
language. Now each value is checked against a closed set motel owns,
and motel builds only equality matchers, on the labels its own alerts
carry (`labelsFor`, `oneShotAlert`):

| Flag | Matcher | Taken |
|---|---|---|
| `--alert` | `alertname=` | one of `silenceableAlerts`: the names motel's detectors raise on any box (`raisedAlerts`: liveswap's lifecycle, cert, disk, backup, `ChannelFailed`, `person_new_network` and `person_outside_list`), and with the metrics line the store's chain events, the vmalert rules rendered for this config (`fixedRules`, the operator's `threshold` and `disk_fill` rule names) and, with a `post_deploy` rule, `post_deploy_regressed`. `TestSilenceableAlertsAreWhatMotelRaises` reads the package's emitters from its source and holds the set to them both ways |
| `--app` | `app=` | one of the apps the loaded config runs; with `--alert`, only an alert about an app, and for a rule of one app's (a `threshold` line with `app`) that app, and `post_deploy_regressed` only for an app a `post_deploy` rule judges (its own, else the every-app one: `postDeployRuleFor`, the comparator's) (`alertScope`: liveswap's, `post_deploy_regressed`, and the vmalert rules whose subject is an app's), since a box alert has no `app` label and the silence would match nothing. Alone, it silences every alert with that `app` label, an external producer's `--subject app:<name>` events among them, but the deploy mute: motel adds `alertname!=deploy_in_progress`, so that what the mute inhibits stays inhibited whatever Alertmanager does with a silenced source |
| `--release` | `release=` | one of the release names motel knows for the app (`releasesOf`, listed in the refusal): its releases on disk (`liveswap.App.Releases`, newest first), then the versions in its deploy records not on disk (`liveswap.App.RecordedVersions`, the store `GET /<app>` lists as `deploys`, newest first): a failed deploy's, whose directory is removed at once, and those release GC pruned. With `--app` and an `--alert` whose release label names one of them (`releaseAlerts`: `rollback`, `deploy_succeeded`, `deploy_failed`, `post_deploy_regressed`). One set for all four, not a set per alert: a pruned version that succeeded is still a release name motel wrote, and a rollback or regression alert raised before GC can carry it; what the set guards is that no value reaches Alertmanager or the journal unless motel wrote it, and a pair that cannot match is only a silence of nothing. A state alert (`watchdog_throttled`) carries its release as an annotation only, and a refused deploy writes no record (it comes before any phase), so a `--release` there is refused rather than stored to match nothing; it is silenced by `--alert` and `--app`. So is a failed deploy with no record: one that failed before it was accepted (liveswap records a version's history only from acceptance: its app directories, the settling of the previous deploy, the release's stat, a push's staging), one whose record write failed (logged, not fatal), and one whose record was pruned (the newest `keep` not on disk are kept) |

| Refused | Why |
|---|---|
| neither `--alert` nor `--app` | a silence of a release alone, or of nothing, is too wide |
| a pair that would match nothing by its shape: a box alert with `--app`, `--release` without `--app` or with an alert that carries no release | Alertmanager takes a silence that matches nothing, and the operator would believe the alert quiet. The release itself is held to one set for the app, not to one per alert, so `--alert rollback --release <a failed version>` is taken though no rollback to it can fire until that version is deployed again: the set guards what reaches Alertmanager and the journal, the shape what can match |
| an alert motel does not raise here, or an app the box does not run: the refusal lists the names taken, never the value given | the names listed are motel's own, safe to print; the value may be anything, and Caddy journals a route's refusal (`admin.api` `request error`), so no refusal of this route echoes input |
| `Watchdog`, `NotifyTest`, `deploy_in_progress` (`notSilenceable`) | a silenced Watchdog stops the heartbeat's pings, and the check off the box then reports the box down: the place to quiet it is that check's pause (docs/monitor.md, "Turn it off"); the other two are a test alert and the deploy mute, which reach no channel |
| a comment empty, over 200 characters, or with a control character (a newline, a tab, an escape sequence, U+2028 or U+2029) | whoever finds the silence reads why, in a terminal among others |
| a duration under 1m or over 24h (default 2h, from now; a fraction is kept, `ends_at` is RFC 3339 with nanoseconds) | a silence that outlives the incident hides the next one |
| an id that is not a UUID; an id the filter masks (the heartbeat's check UUID is a UUID); an id Alertmanager does not hold (422, `no silence by that id in Alertmanager`); one already expired (422, `silence <id> has already expired`) | Alertmanager 0.28.1 answers 200 to the expiry of an expired silence (measured in the e2e box), so the route asks its state first (`GET /api/v2/silence/{id}`) rather than journal an end that was not. A masked id is refused before Alertmanager is asked, which journals the id of a silence it cannot find; no refusal echoes the id given, a 502's path included (`/api/v2/silence/<id>`), but the expired one's: Alertmanager holds a silence by it, so it is Alertmanager's own |
| a body with unknown fields (a `matchers` list among them), a field of the wrong type, or not JSON: one fixed 400, `the body is not this route's JSON request` (`readJSON`, which every notify route uses, `send`'s and `test`'s too) | the route takes the request, never Alertmanager's shape; Go's decoder names an unknown or mistyped field, which the journalled refusal must not carry |

Not taken yet, and adding them later breaks nothing: an external
producer's own names (`moteld monitor send --name`), which no config
lists; regexes; other labels.

The comment is the one free text, and it is **masked, not refused**:
`cleanText(known, comment)`, the filter and the 200-character
cut a summary gets (the engine's `clean` is the same function over its
own known values), with `known` the union of the checked config's known values and the
engine's at that instant: a reload between the route's check and the
mask neither unmasks a secret the checked config knew nor misses one
the new config added. A refusal would echo nothing useful (it cannot
say which part without printing it) and would send the operator to
guess; a masked comment in Alertmanager shows the filter's marker where
the secret was, which reads as what it is. The bare-ntfy-topic limit of
the filter ("Owed") applies to it as to every summary.

`createdBy` is `moteld monitor: <user>`, the user being `SUDO_USER`, else
`USER`, else the uid's name: what the caller's environment says, from a
caller the admin socket already admits as root or motel, so a label for
whoever reads the silence, not an identity checked. Over `ssh root@box`
it is `root`.

`--json` prints `{"id":"…","ends_at":"…"}` for an add and `{"id":"…"}`
for an expire, and `{"error":"…"}` with exit 1 for any refusal or
failure the command reaches (cobra's own errors, an unknown flag or an
argument, stay text). Without it: `silence <id> added; it ends at
<ends_at>` and `silence <id> expired`. Both shapes are pinned by
`TestSilenceCommandOutput`, and the request Alertmanager is sent by
`TestServeSilence`.

There is no `moteld monitor silence list`: on the box, reads go to
Alertmanager's own v2 API (`GET /api/v2/silences`, `GET
/api/v2/silence/{id}`), whose shape motel would only copy ("Viewing").
A viewer of the box reads the silences that are active or wait to
start over HTTPS instead, with every alert: `GET /_motel/alerts`,
`motel monitor alerts` ("Viewing"). Who runs the commands: root (and the motel uid, which
the admin socket admits); any other user is told `the admin socket
(unix//run/motel/admin.sock) admits root and the motel user: run moteld
monitor silence as root`.

## Packaging and setup

The `.deb` **Suggests** `prometheus-alertmanager`. A `Depends` cannot
close the window in which Debian's unit starts on install (`:9093`,
gossip on `0.0.0.0:9094`): apt may configure a dependency, and run its
postinst, before motel's own scripts run, and `Pre-Depends` makes that
order certain rather than preventing it. On a box with no firewall
(hotserve-1 has none) that is the alert and silence API on the
internet. So **`moteld monitor setup`**, run by root, does it in an order
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
5. `try-reload-or-restart moteld.service`, which renders.

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
or 9094, and `moteld monitor test` delivers at once; the postinst
re-enables the service, so it then starts at boot rather than at its
socket's first connection, still on the socket alone. A reboot with motel rendering (2026-11-10, hotserve-1,
13.2 s to multi-user): `motel-am.socket` listening at 11.9 s,
Alertmanager started at 12.0 s on the rendered file left from before,
motel's render and reload at 12.8 s, moteld ready at 12.9 s; no ordering
cycle, no listener on 9093 or 9094, a heartbeat at once and `moteld
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
| `…/external.json` | 0600 | motel:prometheus (born in the setgid dir; no group bit) | motel (the engine, by rename on each change of a producer's state; removed when none stands) | motel (once per process, at notify's first Start) |
| `/etc/motel/secrets/_box.env` | 0640 | root:motel | root: the box applier from `secrets/_box/`, or by hand | motel |
| `/etc/systemd/system/motel-am.socket`, `…/prometheus-alertmanager.service.d/10-motel.conf` | 0644 | root | setup | systemd |
| `/var/lib/prometheus/alertmanager/` | 0750 (the drop-in, at every start; Debian ships 0755) | prometheus | Alertmanager (the notification log, silences) | Alertmanager |
| `<liveswap root>/<app>/alerts.json` | 0600 | motel | motel (liveswap's watchdog, on each change; removed when nothing stands) | motel (once per process, for `ReportStanding`); in no app's view |

## Threat model

New: a resident daemon motel did not write, reachable by unix-socket
permission alone, holding outbound credentials in files only it and
motel read; and five admin routes (the silence routes give the admin
socket's callers, root and motel, nothing the Alertmanager socket did
not already give them; the rules route only reads).

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
in the e2e box, 2026-11-12). On the box, root uses the `moteld monitor`
commands or `curl --unix-socket /run/motel-notify/am.sock
http://am/api/v2/alerts`. `amtool`, which the package ships, speaks TCP
only: on a box with the store it runs inside the store's namespace,
against Alertmanager's listener there (`ip netns exec motel-metrics amtool
--alertmanager.url=http://127.0.0.1:9093 alert`, measured). From root's
laptop, a unix-to-unix forward into an owner-only directory, `mkdir -m
700 -p ~/.motel && ssh -N -o StreamLocalBindMask=0177 -o
StreamLocalBindUnlink=yes -o ExitOnForwardFailure=yes -L
~/.motel/am.sock:/run/motel-notify/am.sock root@box`, then `curl
--unix-socket ~/.motel/am.sock http://am/api/v2/alerts`. Not a TCP
forward (`-L 9093:…`): that puts the unauthenticated API, silences
included, on the laptop's loopback, for any local process and for a
browser page through DNS rebinding. Measured with OpenSSH 10.0p2 (sshd
on localhost in a Debian 13 container, 2026-11-13): the socket is born
`srw-------` (0177 is OpenSSH's default mask; the option keeps it so
whatever the user's config says), another user's `curl` is refused,
and a socket file left by an ended ssh makes the next forward fail
`Address already in use` unless `StreamLocalBindUnlink=yes`. Not
measured against a real box. Writes from the laptop go through `ssh
root@box moteld monitor silence …` ("Silences"), not this forward.
Slack's title link, which Alertmanager's defaults point at its external
URL (`http://<hostname>:9093/#/alerts…`, where nothing answers), is
rendered empty, and its fallback is motel's title. A deployer
sees notifications at their channel; not Alertmanager, other apps, box
events or silences. A person with the viewer role on an app (`grant
<name> viewer app <app>`, docs/people.md) also reads, over HTTPS, that
app's alerts as Alertmanager holds them, each with whether a silence or
the deploy mute holds it and when the last such silence ends: `GET
/_motel/apps/<app>/alerts` (liveswap/access/DESIGN-access.md). Not the
silences themselves, their comments or creators, and no box event, even
one naming the app in its instance. They read, too, the rules that apply
to their app, with its numbers alone (`GET /_motel/apps/<app>/rules`: the
listing of `moteld monitor rules` without the store's socket and the
queries, DESIGN-metrics.md, "How close each alert is"); a viewer of the
box reads every rule (`GET /_motel/rules`). notify installs that source
(`rulesSource`) beside the alerts' in `provisionNotify`. motel reads them through
`alertsView` (alertsview.go): `GET /api/v2/alerts` with no parameter,
which gives active, silenced and inhibited alerts alike and no resolved
one (measured on Debian's 0.28.1 in the e2e image), and `GET
/api/v2/silences`, for the end of each alert's silences and for the
box-wide read's list; both on every fetch, at most once per 5 s for the
process, within 2 s, a failure of either kept as long and journalled
once. The silence list holds expired silences too, until Alertmanager's
retention drops them (`"expired"`, measured on 0.28.1 in a Debian 13
container, 2026-11-19); the source leaves them out. The heartbeat, `NotifyTest` and the deploy mute
are left out. liveswap chooses the app's alerts by their `app` label
after the fetch; no matcher reaches Alertmanager. A config whose monitor
block runs no notify (none, or a metrics line alone) installs no source,
and the read says `"monitor":"off"`. A person with the viewer role on
the box (`grant <name> viewer box`) reads the whole answer: every alert,
the box's own among them, with the app its label names and its
instance, and every silence that is active or waits to start, with its
matchers, maker and comment: `GET /_motel/alerts`, `motel monitor
alerts` and the client's `:alerts` (liveswap/access/DESIGN-access.md).
A viewer of the box's alone, since a comment and a maker are the
operator's own words and an instance names paths and other sites. The
same fetch serves both; no matcher, label or query of the client's
reaches Alertmanager, and nothing is written to it. The operator's
guide to all of this is docs/monitor.md.

## Decisions

| Decision | Why |
|---|---|
| Every occurrence of a one-shot is delivered, bounded only by motel's limit per key; grouping or digesting repeats is the receiving service's (whatever takes the webhook), not motel's: no monitor line groups, and none is promised | the owner's decision of 2026-11-14: an app that fails twice within two minutes is to be heard twice, and grouping is done at the service that receives the alerts. It reversed an accepted row of the same day, under which a one-shot posted again with the same labels was told once until five to six minutes after the last ("Events", `occurred_at`) |
| One block, `monitor`, for notify and the metrics store | notify, the numbers and the rules over them are one idea to the person configuring the box; `metrics` is Caddy's own global option, so the store cannot be a block of that name; one global option yields one Caddy app, so the store is a part of the monitor app (`App.Metrics`), not an app of its own. A `notify` or `metrics_store` block is refused by name, and `moteld notify` and `moteld metrics` exit 1 with the new spelling: everything here is unreleased, so there is no alias |

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
| The silence list is asked on every fetch, for the box-wide read, and a failure of it fails the whole fetch: for up to 5 s every read of alerts says `"monitor":"unanswered"`, an app viewer's `GET /_motel/apps/<app>/alerts` too, though no alert of the app is silenced and `GET /api/v2/alerts` answered | notify/alertsview.go (`heldAlerts`) | 2 | 3 | 1 | 6; red and named (the read says unanswered, the journal says why, once per 5 s), and it ends with the next fetch. The same for a list over the client's 4 MiB read (`amClient.do`), which Alertmanager's expired silences count towards until its retention drops them: some thousands of silences in five days. Keeping the alerts when the list fails is a state of its own ("the silences not known") in the source, the box-wide body and both clients: size 3. /code-review pass 1 of the box-wide alerts read (#581) |
| A silence's comment may hold Unicode format characters (bidi overrides U+202A–U+202E and U+2066–U+2069, zero-width ones, U+FEFF), which a terminal shows reordered: the comment misstates why the silence exists to whoever reads it raw | notify/silence.go (`checkComment`) | 2 | 5 | 3 | 30 by product, but second-pass hardening (the review rule stops at two passes): the comment comes from root, and the silence's matchers, which decide what is quiet, are motel's own; refusing `unicode.Cf` is size 1, risk 1 for the next change. /code-review pass 2 of the closed-set shape |
| `releaseAlerts` and `lifecycleAlerts` copy lifecycleSink's cases by hand: a new liveswap one-shot added there and not here is silenceable only once `raisedAlerts` names it (`TestSilenceableAlertsAreWhatMotelRaises` fails until it does), and never by `--release` until `releaseAlerts` does | notify/silenceable.go | 2 | 5 | 1 | 10; the scan names the missing alert loudly; `--release` refused for it is a refusal, not a silent miss. /code-review pass 2 of the closed-set shape |
| `--alert deploy_refused --release` is refused though a refusal's release can be known: the commonest refusal is a version already on disk ("version v2 already exists"), whose `deploy_refused` carries `release=v2`, so a CI pushing v2 again can be quieted only by `--alert deploy_refused --app`, every refusal of the app | notify/silenceable.go (`releaseAlerts`) | 2 | 5 | 1 | 10; refused loudly, and the wider silence works. Kept refused as the approved scope of the change that let `deploy_failed` take a recorded version said (a refusal writes no record); adding `deploy_refused` to `releaseAlerts` is size 1, risk 1, the owner's call. /code-review pass 1 on monitor/silence-release-records |
| The e2e row for `--release` posts a hand-built copy of liveswap's `deploy_failed` labels rather than failing a real deploy of v3 under the silence: a rename of `oneShotAlert`'s `release` label would leave the row green while real alerts of v3 page | e2e/notify/run.sh (notify 7a) | 2 | 1 | 3 | 6; `TestServeSilence` pins the matcher and the one-shot's labels are pinned by the engine's tests; a real redeploy adds a deploy's time and its own late-delivery race to notify 9's count. /code-review pass 2 on monitor/silence-release-records |
| A refused admin body names no field: after an upgrade, a new `moteld monitor` command posting to a moteld not yet restarted (a field the old binary does not know) reads as a plain `the body is not this route's JSON request` rather than naming the version skew | notify/admin.go (`readJSON`) | 2 | 3 | 1 | 6; refused loudly, and `systemctl restart moteld` mends it; naming the field safely would mean listing the struct's fields by reflection to tell a known name from input (size 2, risk 1). /code-review of the Copilot round 2 fixes on #444 |
| `created_by` and `expired_by` in the journal are what the caller's environment says (`SUDO_USER`, `USER`): root can name anyone | notify/silencecmd.go (`invokingUser`) | 1 | 5 | 5 | a label for whoever reads the silence, from a caller already root or motel, who could write the journal or Alertmanager directly; no identity is checked, and the doc says so. /code-review of the closed-set shape |
| Two `moteld monitor silence expire` of one silence at once, or one as the silence reaches its end: both pass the route's state check (`GET`) before either `DELETE`, which Alertmanager 0.28.1 answers 200 for an expired silence, so the journal has two `monitor: silence expired` lines, or one for a silence that ended on its own | notify/silence.go (`serveExpire`) | 2 | 3 | 3 | 18; the silence ends expired either way; only the journal's count is off, and Alertmanager's own `endsAt` says when it ended. A lock across both requests would hold the engine's lock over Alertmanager's answer, or add a lock of its own (size 3). /code-review pass 1 on monitor/silence |
| An add that Alertmanager answers 2xx with a body holding no silence id (not its API's `{"silenceID":…}`) is told as a plain failure, `silencing in Alertmanager: alertmanager answered "…"`, though Alertmanager took the silence: the operator may add it again | notify/silence.go (`postSilence`, `answerLost`) | 2 | 2 | 3 | 12; Alertmanager outside its own API, and a body cut short is a read error, which `answerLost` takes. Telling it apart needs a sentinel error from `postSilence` and a third message (size 2, risk 1). /code-review pass 1 of monitor/two-messages-say-what-happened |
| An expire whose answer is lost (the client's 10 s, after Alertmanager ended the silence) is a plain 502, `expiring in Alertmanager: … deadline exceeded`, with no `silence expired` line; the retry answers `silence <id> has already expired`, which writes none either | notify/silence.go (`serveExpire`) | 2 | 3 | 1 | 6; ends correct and named under the one fault: the silence is ended, the 502 names the timeout and Caddy journals it, and the retry says the state the operator wanted. The add's `answerLost` branch for expire is a branch and a message (size 2, risk 1). /code-review pass 1 of monitor/two-messages-say-what-happened |
| `answerLost` judges by the error, not by whether Alertmanager read the request: a deadline before the POST was written (the client's 10 s spent in the dial; the caller's cancel no longer reaches the POST) and a non-2xx answer whose body is cut off or stalls are told as `may have taken the silence`, with the `silence add unanswered` warning, though Alertmanager took nothing; and a reset after Alertmanager read the request and began its answer (killed while it writes) is told as a plain failure | notify/silence.go (`answerLost`), notify/amclient.go (`do` reads the body before it looks at the status) | 2 | 2 | 3 | 12 at most (the reset; the other two say `may` and give the command that lists the silences, which shows none: D1); each is a timeout or a kill inside a window of milliseconds, or a non-2xx with a dropped connection. A `WroteRequest` flag from `httptrace` in `postSilence` and the status read before the body (size 2, risk 2). /code-review pass 2 of monitor/two-messages-say-what-happened: recorded, the review rule stops at two passes |
| A `systemctl stop` or `restart` of moteld while an add waits on Alertmanager (socket activation starting it, or Alertmanager slow up to the client's 10 s): Caddy stops the apps, then waits for admin requests, and systemd kills moteld at `TimeoutStopSec=5s`, so an add Alertmanager answers after the kill writes neither `silence added` nor the unanswered warning, though Alertmanager may hold the silence. Likewise a reload that drops a `log` output the old config opened (a file or net sink) closes the logger the route writes its line with | notify/silence.go (`serveSilence`), packaging/moteld.service | 2 | 2 | 3 | 12; a stop and a slow Alertmanager at once, or a log output changed in the reload the add spans; the silence, if taken, is in Alertmanager's list, and a caller who stayed sees the connection drop. As before this change for a caller who stays. Holding the stop for the add would be a longer `TimeoutStopSec` for every stop (Wide), or a drain of the silence routes before Caddy stops the apps (size 3). And an add whose caller left now holds a stop issued within its 10 s the same way, to the 5 s kill, where its cancel ended it at once: systemd names the timeout (S2 O2 D1, 4). /code-review passes 1 and 2 of monitor/silence-add-outlives-its-caller |
| `TestSilenceAnswerLost` shortens the client's timeout (`silenceTimeout`) to 300 ms, which runs from the dial: a runner too slow to dial a unix socket and write the POST within it fails the test with `Alertmanager never got the request` | notify/silence_test.go | 2 | 1 | 1 | 2; a local dial and write take microseconds. Through the route, whose POST no longer takes the caller's cancel, the client's timeout is the one way to lose an answer, so it cannot start at the stub's `got` as `TestAnswerLost`'s cancel, on a client of its own, does. /code-review pass 2 of monitor/two-messages-say-what-happened: recorded, the review rule stops at two passes |
| A UUID-shaped secret this box does not hold, pasted for a silence id (another box's heartbeat check UUID, given to `moteld monitor silence expire` on this one): the filter does not know it, so it passes the masked check and reaches Alertmanager's per-id `GET`, and Alertmanager 0.28.1 journals it (`Failed to find silence`, with the id, at error level; read in `getSilenceHandler`, not observed on a box) in prometheus-alertmanager's journal, readable by group adm. motel's own refusal does not echo it | notify/silence.go (`serveExpire`) | 3 | 5 | 5 | 75: over 20, so the rule says fix; recorded instead as second-pass hardening (the review rule stops at two passes), outside what the filter promises, which is this box's own known values, and those are refused before Alertmanager is asked. S3 as for the slug-form ping key above: a ping key alone, readable where it should not be. The fix, for the owner to ask for: read `GET /api/v2/silences` and look for the id in the list, so an id Alertmanager does not hold is never sent to a per-id endpoint (a list logs nothing); size 2, risk 2. Second /code-review pass on monitor/test-no-echo |
| A public host name that resolves, or rebinds, to a private address is posted to: the check is on the URL as written | notify/config.go (`checkURL`) | 1 | 5 | 5 | a channel URL is a signer's line today, and a signer can already run any code as an app that reaches the same addresses; scored by what T6 gains (nothing). Rescore when #417 lets a deployer choose a URL |
| moteld stopped for longer than `resolve_timeout` after its last re-post (re-posts come each minute, so from about four minutes): Alertmanager resolves each firing state alert at `resolve_timeout`, and the channels get a recovery that did not happen (at moteld's start the watchdog's standing reports fire its alerts again, the producers' kept states theirs, and the pollers theirs) | notify/engine.go (no `endsAt` on re-posts) | 2 | 3 | 3 | 18; a stop that long is itself a fault the heartbeat reports (Watchdog's silence pages first); keeping Alertmanager's alerts alive across a stop would need a poster outside motel; a restart (seconds) is "Behaviour"'s, and keeps them |
| A box on the feature branch's `notify` or `metrics_store` block cannot move to the `monitor` block in one load: the new binary refuses the old blocks and the old binary the new one, so the upgrade runs with no notify between two pushes (docs/upgrading.md, "From the notify and metrics_store blocks"): no event is delivered, the heartbeat stops (the dead-man check pages), Alertmanager ends what fired at `resolve_timeout` with a recovery that did not happen, and the store's rules are retired at the first push's reload | notify/caddyfile.go (`refusedOption`) | 3 | 5 | 1 | 15; once per box, only for boxes that ran the unreleased feature branch, in an order the doc names and the refusal points at, and the dead-man check reports the gap; a transitional alias (the old names loading, warned, for one release) is a second grammar and a new state to retire (size 3, risk 2). /code-review pass 1 on the monitor block, finding 1; the user chose the documented order over a transitional alias |
| A recovery on an ntfy channel carries its alert's priority: a critical state's `[RESOLVED]` is sent at ntfy's 5 (urgent) too | notify/render.go (`ntfyPriority`) | 2 | 5 | 1 | 10; the header is fixed per receiver, and a receiver takes an alert and its end alike; the body says `resolved`. A priority by status needs a receiver per status (size 3, risk 2). #433 |
| Two producers send a `firing` and a `resolved` of one key at once: the resolve's answer (`resolved nothing`, or `sent`) is read before its report runs, so it can say the opposite of what the report did | notify/admin.go (`serveSend`) | 2 | 2 | 3 | 12; two producers racing on one key, and the state ends as the reports leave it; the answer alone is wrong, and the journal lines say what happened. The report returning what it found changes a function the pusher takes as a value (size 2, risk 2). /code-review pass 2 on #433 |
| `moteld --help` and its commands' usage say `caddy` and link caddyserver.com's docs | Caddy's root command (binary-wide) | 2 | 5 | 1 | 10; the commands themselves are motel's and work as printed under `moteld`; renaming Caddy's root command is a change to every command's help, not monitor's (size 2, risk 2). The deploy of 01b2fd5 to hotserve-1 |
| `moteld monitor test` reads the counters for 15 s; a slow receiver answers "neither yet" | notify/admin.go (`testWait`) | 2 | 2 | 1 | the answer says so; Alertmanager's retries continue |
| At a boot, an event liveswap reports before notify's Start (Caddy starts the apps in no fixed order) is dropped, the engine serving no config yet: a recovery at that instant ends its alert at `resolve_timeout`, up to five minutes late | notify/engine.go (`report`, `active == nil`) | 2 | 3 | 3 | 18; what stands is told again at notify's Start (`ReportStanding`), and an end told meanwhile is told again after it; holding events for a config not yet started adds a queue with its own lifetime (size 3). /code-review of the Owed rows |
| After the wall clock steps back by a second or more, a one-shot read at exactly the nanosecond of an earlier occurrence of the same key that Alertmanager still holds (five minutes; at most four per key) gets the same `occurred_at`, so the same labels: Alertmanager merges it into the earlier one and sends nothing. The journal has its `monitor: event` line, and the earlier, identical one was delivered | notify/engine.go (`occurrence`: a nanosecond bump only within a second of the last reading) | 2 | 2 | 5 | 20, the bar, which rule 3 would fix; rule 4 stops it. A clock step is one rare fault, and the coincidence it needs as well is counted as a second, as for vmalert's clock-step row in DESIGN-metrics.md (a coincidence is no fault, but scoring the step alone would rank it with failures that follow from the step every time). The engine reads CLOCK_REALTIME through the vDSO in nanoseconds: a reading every 41 to 42 ns on an arm64 VM's 24 MHz counter (measured 2026-11-14), every nanosecond on x86's TSC. A coarse reading needs a kernel left on the jiffies clocksource, 4 ms at Debian's HZ=250 (a machine with no working timer); none reads whole seconds. So the reading must fall in the same 42 ns tick as one of at most four live values: for a one-shot at a time independent of the clock, at most about 1.7 × 10⁻⁷ (4 × 42 ns over a re-traversed second), and about 6 × 10⁻¹⁰ over the five minutes a value lives; a producer a scheduler starts again at the same wall instant is still apart by its own start-up jitter (a process start and a post over the admin socket, tens of microseconds at least), about 10⁻³ per such re-run at most. Alert times are not pushed ahead to avoid it: a bump past a step of a second or more would. The fix is a memory of the values still alive, or a sequence where a reading revisits one: new state, size 3, risk 2. Copilot on #487 |
| The last config running notify stops (a reload that takes the block out, or moteld's shutdown) while a post is in flight: the cancel cuts the POST after Alertmanager has taken the body and before its answer, the post is still at the queue's head, and `release` gives it up: a `monitor: delivery failed` line and the counter for a post that was delivered | notify/engine.go (`release`: the queue given up after `cancel`) | 2 | 2 | 3 | 12; a stop that lands inside one POST's answer, and the alert reaches its channels; before, the same post was lost unsaid, or posted again by a later config whatever its age. Letting the last attempt finish before the cancel waits on Alertmanager inside the stop (size 2, risk 2). /code-review pass 1 of monitor/queue-no-stale-posts |
| Caddy's `finishSettingUp` fails after every app of a reload's candidate started: `run` returns the error and stops none of them, so the candidate is never released while the old config serves on. Alertmanager keeps the candidate's render and the engine its audience (`point`), the pollers the old config's sources (no `settle`); and the candidate stays in `live`, so a later Stop of the config serving falls back to it: its render is put back, and the engine keeps posting even after a reload that takes the block out | notify/engine.go (`configure`, `release`); Caddy's caddy.go (`run`, `finishSettingUp`) | 3 | 1 | 3 | 9; only `admin.identity`, `admin.remote` or `admin.config.load` can fail it, which motel's Caddyfile and tools do not write; Caddy leaks every started app of that candidate there (its listeners too), so the fix is upstream; telling the stuck candidate apart in the engine is a new state (size 2, risk 2). /code-review of the failed-reload fix |
| A restart after notify has resolved a recovery but before the next healthy probe clears `alerts.json` (with `health_interval 2m` and a 30 s hold, up to about 90 s) replays the recovery: the emptied engine posts the alert held, then resolved, and the operator sees the firing and recovery messages again. A reload does the same when it lengthens the hold (the standing report judges it with the candidate's `watchdog_grace`), and so does a push just after a restart (the watchdog's own `healthySince` is new) | liveswap/watchdog.go (`reportedAlerts` keeps `Recovering`, not when the hold began); liveswap/lifecycle.go (`reportStanding`) | 2 | 2 | 1 | 4; a duplicate the operator sees, only for a restart or reload inside one health interval after a hold ends; persisting the hold's start, and the engine honouring it, is size 3 (risk 2). Copilot round 1 on #424, widened by /code-review xhigh on #424 |
| A restart inside a recovery's hold, then a relaunch that fails for good (no instance runs after the restart): the persisted `recovering` is re-reported as a held recovery, so the engine resolves the alert of an app that is down, and the flags stay for the next restart | liveswap/lifecycle.go (`reportStanding`) | 3 | 2 | 3 | 18; two faults, a restart inside a hold and then a relaunch that fails; the journal names the failed relaunch and status shows the app down; telling it apart needs a branch on whether an instance runs after recovery (size 2, risk 2). /code-review xhigh on #424 |
| A `watchdog_*` loop that fires during its app's deploy and that the deploy's success ends: the recovery (`watchdog_recovered`, reported just before the deploy's end) is held 30 s, past the mute's end, so the alert still fires when the group's next flush comes about half the time: `[FIRING]`, then `[RESOLVED]` within a minute, for a loop the deploy had already ended | liveswap/app.go (`reportDeploy`); notify/engine.go (the hold) | 2 | 3 | 1 | 6; the pair is true (the loop was), and its recovery follows; ending the hold at once when a deploy's success ends the loop is a branch on the event (size 2, risk 2). /code-review pass 1 of the deploy mute's held recovery |
| A restart while the engine holds a `watchdog_*` recovery under the mute, or keeps a resolve to post again at its end: the table is the process's, and liveswap's `alerts.json` stops saying `recovering` at the first healthy probe after its own hold, so nothing tells it again: Alertmanager ends the alert at `resolve_timeout`, about five minutes late, or (a swallowed resolve) never | notify/engine.go (`muted`, `toldEnd`: memory only) | 2 | 2 | 3 | 12; a restart inside a deploy, after a recovery; keeping them for a restart is a file as `external.json` is (size 3, risk 2). /code-review pass 2 of the deploy mute's held recovery |
| A reload that takes the block out and one that brings it back while a deploy runs: the engine that starts ends that deploy's mute too (it forgot it with the table), so a `watchdog_*` alert that fires in the rest of the deploy is told | notify/engine.go (`configure`: `!e.running`) | 2 | 1 | 3 | 6; two reloads inside one deploy; telling it apart needs liveswap's running deploys at Start (size 2, risk 2). /code-review pass 2 of the deploy mute's held recovery |
| A channel whose deliveries fail for longer than `endWindow` after Alertmanager took a `watchdog_*` resolve, then a deploy of the app: Alertmanager keeps the resolved alert in that leaf's group while its notification fails, and retries it each flush; the mute then inhibits the retry, the pipeline succeeds on an empty list and the alert is dropped unsent, while the engine forgot the resolve at the window's end: that one channel never gets the `[RESOLVED]`, which it would have had when it came back. Read in Alertmanager 0.28.1's source (dispatch.go: the resolved alerts are deleted only when notify succeeds), not measured | notify/engine.go (`toldEnd`, `endWindow`); Alertmanager's dispatcher | 3 | 2 | 3 | 18; a channel failing for over two minutes and a deploy of that app while it does; `ChannelFailed` names the failing channel, not the lost recovery. Keeping the resolve until every leaf has delivered needs Alertmanager's delivery state per receiver (size 3, risk 2). Independent review of #496 |
| The re-post of a swallowed resolve at a mute's end, given up by the queue (Alertmanager unreachable from just before the deploy's end for five minutes), is a debug line only, though for a swallowed resolve it is the recovery's only carrier: the channels' last word stays `[FIRING]`, and the journal says so only at debug | notify/engine.go (`endMute`: its post carries no journal flag) | 3 | 2 | 3 | 18; Alertmanager down for five minutes at a deploy's end, after its mute met the resolve. The flag alone would print `monitor: delivery failed` at error level for a re-post that may be redundant (the `[RESOLVED]` already told); a true line ("the recovery's `[RESOLVED]` may not have been told") is a branch on the re-post in `gaveUp` (size 2, risk 1). Independent review of #496 |
| A `watchdog_*` resolve the full queue drops while its post is in flight, which Alertmanager takes all the same, with a mute of its app starting before that post returns: the drop removed the resolve's entry, so the mute has nothing to mark, and the entry put back when the post returns is not marked, so a flush that swallows the resolve is not followed by a second post at the mute's end | notify/engine.go (`attempt`: the entry reinstated for a post dropped while posting) | 3 | 1 | 5 | 15; it needs the queue at its 512 posts (Alertmanager not taking posts for a long while), the dropped one answered just then, and a deploy starting inside that one request: three coincidences. Marking the reinstated entry when the app's mute stands is a branch in the path every post's answer takes (size 2, risk 1), in code three review rounds each found a fault in: stopped here (rule 4). Copilot round 2 on #496 |
| The ends of the mutes an engine posts when it starts are refused by Alertmanager (a 4xx): a mute the last process left stands to `resolve_timeout`, and a `watchdog_*` recovery posted under it is dropped unsent | notify/engine.go (`configure`) | 3 | 1 | 5 | 15; the post's labels are motel's constants, and a give-up after five minutes of retries cannot leave it (the old mute's own `endsAt` has passed by then); the post given up is a `monitor: delivery failed` line at debug. Holding the queue behind it is a new state (size 3, risk 2). /code-review pass 1 of the deploy mute's held recovery |
| A restart mid-deploy into a config that no longer runs that app: the engine ends the mutes of the apps it runs, not that one's, and if the app's kept-alive `watchdog_*` alert reaches its `resolve_timeout` before the mute does, Alertmanager drops the end unsent | notify/engine.go (`configure`: `a.cfg.apps`) | 3 | 1 | 5 | 15; a restart, inside that app's deploy, into a config without it, while its watchdog fires; its alert and its mute were re-posted in the same minutes, so both end within a minute of each other. The last process's apps are not known at the start (a new state, size 3, risk 2). /code-review pass 1 of the deploy mute's held recovery |
| A failed write of `alerts.json` (EIO, a read-only remount) leaves the old file until the next change, and a restart before it reports what the file still says | liveswap/watchdog.go (`alertsChanged`) | 3 | 3 | 1 | 9; the warning at the failure names the file and the consequence; a retry needs a timer outside the transitions (size 2, risk 2). /code-review of the Owed rows |
| A failed write of `external.json` (EIO, a read-only remount) leaves the old file until the next change of a producer's state, and a restart before it tells what the file still says: a state resolved since fires again, or one fired since is lost | notify/external.go (`persistExternal`) | 3 | 3 | 1 | 9; the warning at the failure names the file and the consequence, as for `alerts.json` (the row above); a retry at each tick would repeat the warning every 5 s, or need a backoff of its own (size 2, risk 1). Scored at the change that keeps the producers' states |
| A producer's `resolved`, then moteld stopped inside the 30 s hold for longer than `resolve_timeout` after its last re-post (from about four minutes): Alertmanager has told the recovery at `resolve_timeout`, and the start tells the held state again with no `endsAt`, which Alertmanager takes as a new alert: `[FIRING]` after `group_wait`, then `[RESOLVED]` after the hold, for a condition already recovered | notify/external.go (`externalState` keeps no `clearSince`) | 2 | 2 | 3 | 12; a stop inside a hold and a stop longer than resolve_timeout (itself the accepted row "moteld stopped for more than five minutes"); liveswap's held recovery does the same. Keeping when the hold began and dropping a hold already past is a new field and a branch at the restore (size 3, risk 2). /code-review pass 1 of the producers' states |
| A restart into a config that no longer runs an app a producer's kept state names: the restore tells it as a standing firing (a journal line and a post Alertmanager merges), then resolves it as a reload would, with `why: the config no longer has its source` | notify/external.go (`restoreExternal`, then `prune`) | 2 | 3 | 1 | 6; ends correct and named, one extra `firing` line and post; resolving it without the standing report is a branch in the restore (size 2, risk 1). /code-review pass 1 of the producers' states |
| At a start without the block, a failed removal of `external.json` (EACCES, a read-only remount) is joined into `SendNothing`'s error, which the gate prints under "Alertmanager was not given the config that sends nothing", and the line saying it was given is not printed | notify/gate.go (`SendNothing`) | 2 | 3 | 1 | 6; the error names the file and the cause; a message of its own needs a second error the gate tells apart (size 2, risk 2). /code-review pass 1 of the producers' states |
| `moteld run` by hand as the motel user, the owner of both state directories (`sudo -u motel moteld run --config <a file>` on a live box), is not told from moteld.service's start. A file with no monitor block: the gate gives the running service's Alertmanager the config that sends nothing, removes the URL files and `external.json`, and empties vmalert's rules. A file with another block: Start renders its channels and rules and reloads Alertmanager onto them, and its engine reads `external.json` and posts a resolve for each live alert of an app the file does not run (a `[RESOLVED]` that did not happen); a start that then fails on the ports leaves that render. The service is not told, so until its next load every alert goes nowhere or to the file's channels. A heartbeat line on the box has its check go red off the box once the pings stop (not while the file's render pings the same check); a block without one sends the Watchdog to the blackhole, and nothing off the box sees the silence | cmd/moteld/monitorcheck.go, metricscheck.go (the gate); notify/app.go (`Start`); both through `NotOwner`, notify/state.go | 4 | 5 | 3 | 60, which the rule would fix; accepted by the owner's decision, over a probe of the admin socket at every start. Without a heartbeat line nothing off the box sees the silence, which is close to severity 5, which rule 1 fixes whatever the occurrence; the score rests on what the by-hand run itself prints, and the outcome stays the owner's recorded decision. What it prints, with a heartbeat line or without one: the gate's lines, or Start's journal lines on its stderr, say what was done to Alertmanager and the rules, though not that they were the running service's; what it writes is the motel user's, so the service's next load renders its own again. Root by hand, the case found, changes neither directory, whatever its file. /code-review of the gate at `moteld run`, and its second pass |
| A state directory no longer owned by the motel user (root chowned it, or a `chown -R` over /var/lib) while moteld.service runs as motel: each start's gate and Start leave it alone with a WARNING naming the directory, its owner and the process's user, where the write's EACCES was an ERROR; and Start starts no engine, so no event or heartbeat is posted, where before the posts went on through the render Alertmanager last loaded; and with no engine serving, an event ends before its `monitor: event` journal line (`reportLocked`) and the disk and backup polls do not run, so events leave the journal too, where before such a box journalled every event even when it could not deliver | notify/state.go (`NotOwner`), app.go (`Start`) | 4 | 1 | 1 | 4; state only root can write, found changed, and every start names it; with a heartbeat line its check goes red. Posting on while writing nothing, or an ERROR for it, needs telling the service's start from a run by hand, the probe the owner declined. Second /code-review pass of the gate at `moteld run` |
| A power loss soon after a write of `external.json` (written with no flush) can leave it empty: the next start says it could not be read, takes it as none and removes it, so every producer's state is lost, not just the last change, and each one's later `resolved` answers `resolved nothing` | notify/external.go (`writeExternalFile`) | 3 | 3 | 1 | 9; named in the journal at the start, and Alertmanager keeps no alerts across the reboot either, so no false recovery is sent; a flush puts the disk's latency under the engine's lock (pass 1 of this change removed it), and writing outside the lock needs liveswap's generation ordering (size 3, risk 2). /code-review pass 2 of the producers' states |
| A write that fails with anything but a missing state directory is not retried: on the setup run where a tick lands between setup's `mkdir` and its `chown` (EACCES), or after setup failed at its `chown`, a state sent before setup stays unwritten until it changes, and a restart then loses it | notify/external.go (`writeExternal`: only `fs.ErrNotExist` retries) | 3 | 2 | 1 | 6; a window of one system call, or a setup that failed and says so, and the warning names the file; retrying every failure with the warning said once (a flag cleared on success) is size 2, risk 1, and would retire the failed-write row above. /code-review pass 2 of the producers' states |
| An app that read a `NOTIFY_` variable from an env file in `/etc/motel/secrets/` — the box's `secrets/<app>/`, or an operator's own file kept in that directory — loses it from the app's environment at the next launch after the upgrade | liveswap/app.go (`notifyOwnedEnvKey`, `boxWritten`: a test of the directory) | 3 | 5 | 1 | 15; each launch names the withheld keys and the directory in the journal, and `make check` warns before a push; the prefix is what keeps a channel URL from an app. /code-review of the Owed rows, widened by /code-review xhigh on #424 |
| Alertmanager refuses a reload: motel puts the previous config back, but the URL files are already the new ones (a changed value rewritten, a dropped channel's removed), so the old config in memory sends a changed channel to its new URL, and Alertmanager's next start fails on a removed file until motel's next load | notify/state.go (`apply`) | 4 | 2 | 1 | 8; two faults (a render Alertmanager refuses, which the amtool tests rule out for every shape, then a restart before the next load), and Alertmanager names the missing file; a new URL is the one the signer committed; keeping the old files until a reload succeeds adds state to the write (size 2, risk 2). /code-review of slice one, passes 1 and 2 |
| A channel named after its own topic (`ops-alerts` posting to `https://ntfy.sh/ops-alerts`): motel's summaries that name the channel (`ChannelFailed`'s `failing channel: …`) carry `[redacted:partial:NOTIFY_<NAME>]` in its place, and `moteld monitor send` refuses a name, instance or source that contains the topic | notify/app.go (`knownValues`), admin.go (`serveSend`) | 2 | 5 | 1 | 10; the marker names the channel's secret, the refusal says why, and docs/monitor.md says to pick a random topic and name the channel, never the topic; keeping a channel's name whole needs the filter to tell a name from the same text as a credential, which it cannot (size 2, risk 2). /code-review of the partial mask, finding 1 |
| A heartbeat's ping URL in a slug form (Healthchecks' `hc-ping.com/<ping-key>/<slug>`, Cronitor's `/p/<key>/<monitor>`): its last segment is the slug, so the key is not masked alone (only within the whole URL) and the slug is masked instead, wherever it appears: a slug that is the box's name (Healthchecks derives it from the check's name) turns that name into `[redacted:partial:<KEY>]` in summaries, and `moteld monitor send` refuses a name, instance or source that contains it; a reader of a summary that holds the key alone could ping any check of that project. Likewise any other service's URL gives its last segment (past a segment that is a number from 0 to 255, read as Healthchecks' exit status on any host) | notify/app.go (`knownValues`, `urlCredentials`) | 3 | 1 | 5 | 15; the key going out alone gives no sign (the marker and the refusal show only the slug's masking). It needs the slug form, which docs/monitor.md ("A heartbeat") steers away from: it says to use the UUID form, masked alone, and to treat the URL as a password; taking the key needs each service's form by host (size 2, risk 1), and any other service's URL has no fixed place for its credential, as for json channels. The heartbeat's partial mask, from the owed row of the partial mask's finding 5 |
| A JSON config (or a `/load`) with `"state_dir"` or `"alertmanager"` is refused as Caddy's `json: unknown field "state_dir"`, not with the Caddyfile's sentence that names the fixed path | notify/app.go (no field; Caddy's `StrictUnmarshalJSON`) | 2 | 5 | 1 | 10; refused, naming the field, and the Caddyfile is what a box loads; the sentence in JSON needs a field kept only to refuse it, or an `UnmarshalJSON` (size 2, risk 2). /code-review of #438 |
| `moteld monitor setup` from a new binary while an old moteld still serves a config with a `state_dir` line names `/var/lib/motel-notify/alertmanager.yml`, where that moteld does not render | notify/setup.go (`loadedSays`) | 3 | 2 | 3 | 18; the install restarts moteld onto the new binary, which refuses the line, so this needs setup run from an unpacked `.deb` before the install (docs/upgrading.md suggests only `validate` there) on a box that kept the line the upgrade note says to remove; saying so needs a branch on the raw JSON (size 2, risk 2). /code-review of #438 |
| The journal lines of `applyRender` and `applyNothing`, and the metrics store's not-set-up warning, name their directory under the key `state_dir`, a line this version refuses: an operator who adds one to move it is refused at the next load | notify/app.go, metrics.go (`zap.String("state_dir", …)`) | 2 | 5 | 1 | 10; the refusal names the fixed path and says to remove the line; renaming the key changes what a journal search for it finds, and no test reads it (size 1, risk 2). /code-review of #438 |
| The first load on a release with a receiver per channel (the upgrade, and a downgrade back) gives every alert Alertmanager holds a new route and receiver, so no notification-log entry matches it: each state still firing, and each one-shot inside its 5 m life, is sent once more to each of its channels as `[FIRING]`, about 30 s after (0 s for an event). Resolved alerts are not re-sent (a group with no entry and nothing firing sends nothing); `ChannelFailed`, `NotifyTest` and the heartbeat keep their routes and receivers, and are not | notify/render.go (receiver names and routes); Alertmanager's nflog (group key and receiver) | 2 | 5 | 1 | 10; a duplicate the operator sees once per standing alert, named in docs/upgrading.md; the log's key is the receiver, so no rename keeps it (none to make). Scored before the build of #449 |
| A passing fault (one 5xx from ntfy.sh) on the only channel that `failureAudience` holds stops the heartbeat when no delivery through that channel follows motel's read of the failure within 5 minutes: no other alert to it, or the retry that delivered came before that read (Alertmanager gives a notification up at the flush's deadline and the next flush follows at once, and the read that sees the rise takes that delivery into its baseline). The dead-man check reports the box down while it runs, and stays red until the operator runs `moteld monitor test` or the next alert is delivered | notify/channelfailed.go (`untoldFor`, `watchUntold`) | 2 | 3 | 1 | 6; the page names nothing wrong with the box, but the `monitor: heartbeat held` line names the channel and the test that resumes it. Probing the channel unasked would post to it (size 3, risk 2); taking the baseline from the read before the rise keeps every receiver's last count (a new state, size 3, risk 2). Scored before the build of #449; the retry before the read, /code-review of #449 |
| Every operator channel that takes warnings fails at once (two webhooks revoked): each one's `ChannelFailed` goes to another that fails too, so each rise has someone to tell, no hold comes, and the heartbeat stays green while nothing is heard | notify/channelfailed.go (`scrapeFailed`: a hold only when no one is told) | 3 | 2 | 3 | 18; two faults, or one cause that takes the box's egress with it, when the heartbeat's own pings fail too and the check pages; holding when every told channel is itself failing reads the told channels' receivers too (size 3, risk 2). /code-review of #449 |
| A channel that refuses one message (a Slack or Discord 400 for one alert's text) and delivers the rest: each delivery ends the hold, the failing message's next retry starts another, so the heartbeat flaps between held and pinging and the dead-man check between down and up | notify/channelfailed.go (`watchUntold`: any delivery through the channel) | 2 | 3 | 1 | 6; red, though flapping, and each hold's line names the channel; telling a refused message from a refused channel needs the failing notification's own outcome, which Alertmanager's counters do not give. /code-review of #449 |
| A reload that adds an operator channel taking warnings, while a hold stands, leaves the hold until a delivery through the failed channel, and the new channel hears of the failure only at its next rise (Alertmanager's next failing re-send, while the alert lives), not at the load | notify/channelfailed.go (`keepUntold`: drops only a hold whose channel went) | 2 | 5 | 1 | 10; the check stays red and the `heartbeat held` line still names the channel; ending the hold at a rise told to someone, or posting `ChannelFailed` to the new audience at the load, is a branch (size 2, risk 2). /code-review of #449 |
| Alertmanager without `--enable-feature=receiver-name-in-metrics` (the drop-in's flag gone): a failure names no receiver, its suspects are every channel of the integration, and a failure no channel can be told of holds nothing, so with a single operator channel the heartbeat stays green while its alerts fail. A reload then resets no counter, so the load's stamp is not read (`parseAMMetrics`), and a restart of Alertmanager that brings a count back to the last read's is missed | notify/channelfailed.go (`untoldFor`: a receiver this config knows) | 3 | 1 | 3 | 9; the drop-in is setup's file, which only root writes, and `moteld validate` warns when it differs from the one this moteld would write; a hold on a guessed suspect could only end at a reload or a restart, since requests are then counted under no receiver (summing them by integration is size 2, risk 2). /code-review of #449, pass 2 |
| A channel whose first attempt hangs (it takes the connection and never answers) is named late when config-changing loads come more often than its flush deadline (a minute for events, five for the heartbeat): each load cancels the hung attempt before it fails, which counts as `contextCanceled`, a reason neither motel nor vmalert reads, and the new config's attempt hangs again | notify/ammetrics.go (`parseAMMetrics`), notify/metricsrules.go (`channelFailedRules`) | 2 | 2 | 3 | 12; two faults (a hanging channel and loads that keep coming), and late, not lost: the first flush the loads leave alone ends at its deadline as `contextDeadlineExceeded`, which is read. Counting a cancel only after a failed attempt is what Alertmanager already does; counting every cancel brings back the false `ChannelFailed` of #468 (size 2, risk 2). /code-review pass 1 of the reload cancel (#468) |
| A failure Alertmanager counts after motel's last read before moteld stops is not read: every start of moteld has Alertmanager reload (nothing is acknowledged yet in the new process, notify/state.go `apply`), which resets the counter before the new moteld's first read. It is told only if Alertmanager still holds the alert, when the new config sends it again and that failure is counted since the load | notify/channelfailed.go (`loadSince`) | 3 | 1 | 4 | 12; a failure inside the minute before a restart of moteld, of an alert gone by then (a state is re-posted while it fires; a one-shot lives five minutes); the count before the reset cannot be read after it, so telling it needs a read at moteld's stop (size 2, risk 2). What is left of the second miss the removed S5 Owed row named (a failure before motel's first read was its baseline): a first read whose load is after moteld's start now counts from 0. The config-load rows' PR |
| `moteld monitor test` across a config load: after it the baseline is 0, so what the load's cancels count on the channel's own receiver is read as the test's. A cancelled attempt of the test, or of another notification to the channel, is a failed request, and if the new config has not sent the test again by the end of the 15 s the answer is `every attempt failed so far, and Alertmanager is still retrying`; a cancel that keeps an earlier attempt's reason (a ChannelFailed to the channel retrying after a 5xx) answers `failed` with that reason at once. A test delivered just before the load and not yet read is answered `neither` | notify/admin.go (`serveTest`) | 2 | 2 | 3 | 12; a load during the test's 15 s, and then the channel's own retry or the test's last moments: the new config sends the test at once (a channel's own route waits 0 s), and a reason kept from a failed attempt is the channel's failure, if not the test's. A failed request carries no reason that would tell a cancel apart (size 2, risk 2). The config-load rows' PR, widened by its /code-review pass 1 |
| A read that lands between Alertmanager's reset of its counters and its setting of the load's stamp (within one load, microseconds) takes the reset counts with the old stamp: a failure counted in that instant is told twice, at that read (a value below the last) and at the next (a load since, from 0) | notify/channelfailed.go (`rises`) | 2 | 1 | 1 | 2; a read once a minute against a window of microseconds, and a duplicate `ChannelFailed` is the worst of it. Likewise a first read whose body was taken before a reload this process asked for and judged after it (`noteReload`; only when a reload of moteld first brings the block, within its first minute) counts that body from 0. The config-load rows' PR |
| A restart of moteld while a notification is retried after a failed attempt (one 5xx): the start's reload cancels the retry, which Alertmanager counts after the reset under the failed attempt's reason, and the new moteld's first read, counting from 0 since the load came after its start, tells a `ChannelFailed` (or records a hold) for a channel whose next attempt would have delivered | notify/channelfailed.go (`loadSince`) | 2 | 2 | 1 | 4; a restart that lands on a retry, and the attempt did fail: the same as any reload between two reads already did (a cancel after a failed attempt keeps its reason, #468), and the new config sends the notification again, so a hold ends at its delivery. Before, the first read was a baseline, which missed real failures instead. /code-review pass 1 of the config-load rows' PR |
| A box whose monitor units were written before the binary was `moteld`: their `Description=` lines name the command as it was then, so every validate, reload and start warns that the units differ and to run setup again, though nothing about how they run changed; `moteld monitor setup` restarts Alertmanager, which forgets the alerts it holds, and `--metrics` restarts the store and vmalert | notify/gate.go (`staleUnits` leaves out comments and layout, nothing else) | 2 | 5 | 1 | 10; loud and named, with the remedy: one run of `moteld monitor setup` (and `--metrics`) ends it, and the release's notes ask for it; telling a description from a setting is a new branch in the comparison (size 2, risk 2) for one release's change |
| A channel or heartbeat URL that answers with a redirect: every delivery fails, and motel's words for it are Alertmanager's reason, `other` (`moteld monitor test`: `could not deliver (webhook: other); journalctl -u prometheus-alertmanager says more`; moteld's `ChannelFailed` `through webhook (other)`, the store's no reason at all), which does not say redirect | notify/render.go (`amHTTPConfig`), docs/monitor.md | 2 | 5 | 1 | 10; red, and the journal it names says `unexpected status code 307` (measured 2026-11-20), and docs/monitor.md's `channel` line says a URL that redirects fails. Telling a 3xx apart in motel's own text needs the status, which Alertmanager's counters do not carry: a read of its journal (size 3). /code-review pass 2 of the no-redirect render |
| `TestAlertmanagerFollowsNoRedirect` picks Alertmanager's port by listening on `127.0.0.1:0` and closing it: another process that takes the port before Alertmanager binds it fails the test, red, after the 15 s the alerts' post waits, with Alertmanager's `address already in use` in its output | notify/render_test.go | 2 | 3 | 1 | 6; a flake of the metrics e2e lane that names itself. A start retried on that error is a new branch (size 2) for a window of milliseconds. /code-review pass 2 of the no-redirect render |
| moteld restarts while the backup record cannot be read, or while the credential file cannot be looked at: the new process knows nothing of what fired, its first poll gives no verdict and raises only `backup_unreadable`, and Alertmanager resolves what the old process had firing about 270 s later: `[RESOLVED] backup_stale` and the like while nothing was put right; `[FIRING]` again at the first poll that reads | notify/detectors.go (`backupPoller.poll`, `unknown`) | 3 | 2 | 3 | 18; two independent faults (the record unreadable, or the file's directory unsearchable, and a restart), and `backup_unreadable` says the verdict is not known. Measured on Alertmanager 0.28.1 in research_notes/backup-alerting-options (S7): the same on main and under every option. Remembering what fired across a restart is a state on disk, or a read of Alertmanager's alerts at start (size 3). #590 |
| A credential file the motel user cannot `Lstat` (`/etc/motel-backup` made other than root's 0755, which docs/upgrading.md tells the admin to keep): no verdict, so only `backup_unreadable` is raised, a warning, naming the file; what fired stays fired, and nothing new is raised while it lasts, so a channel that takes critical alerts alone hears nothing of a problem that starts meanwhile | notify/detectors.go (`backupPoller.poll`, `unknown`) | 2 | 5 | 1 | 10; red and named for every channel that takes warnings, as for a record that cannot be read, and the client's `motel backup status` says it cannot tell. The owner's decision of 2026-11-22 (#649): `backup_unreadable` rather than nothing at all; before it, no backup alert was raised on such a box (5 × 5 × 5) |
| `backup_record <path>` other than the default (motel-backup writes nowhere else): the poller judges that record beside the box's credential file, so a record not yet written on a box set up hours ago reads "no run yet", `backup_never_ran`, critical, and `GET /_motel/backups` reads the default record still | notify/detectors.go (`backupPoller.poll`) | 2 | 5 | 3 | 30; the rule says fix, but the line is the operator's own, and the owner decided to accept it with this row and no guard (2026-11-21, #590) |
| `moteld monitor rules` lists, beside `backup_stale`, the age of every app's last complete backup the record holds, also one status calls "snapshot gone" rather than stale: such an app can read `shop 9h` against `> 3h` while `backup_stale` never fires for it | notify/detectors.go (`backupPoller.poll`, `b.read`), notify/ruleslist.go | 2 | 3 | 1 | 6; the age is true, and `backup_snapshot_gone/shop` fires for it, critical (the owner's decision of 2026-11-22), naming the snapshot gone; taking status's own split into the readings is a branch (size 2). /code-review pass 1 of #590's rework |
| The credential file removed (`rm` of `/etc/motel-backup`, or of the file alone; the docs stop backups by disabling the timers, never so): the poll reads "not set up", as status does, so every backup alert that fired is told `[RESOLVED]` and none fires again, though the record shows backups ran and the hourly runs now fail | notify/detectors.go (`backupPoller.poll`) | 5 | 5 | 3 | 75; the rule says fix, but the cause is an admin's own act, and status itself says backups are not set up: the monitor says no more than status. Accepted by the owner's decision of 2026-11-22 (#649); the fix would have been one branch (with no file and a record that holds a run, keep what fired) |
| A new app whose first complete backup is over 1 GiB restored is `backup_drill_failed/<app>`, critical, from its first backup until the weekly drill proves it (up to a week): the run does not drill a first backup that large (`firstDrillLimit`) and records why as the app's drill, and status calls an unproven restore unhealthy | backups/engine/restore.go (`firstDrill`), backups/status/status.go | 2 | 5 | 1 | 10; red and named, and true (no restore of it is proven); as `drill_failed` did before #590. Whether status should wait for the weekly drill is the owner's question in #590's PR. /code-review pass 2 of #590's rework |
| On a box with the metrics package, a failure that begins while moteld is not running is first in the box's read as vmalert's copies addressed to each channel told (one row each, from vmalert's start); once moteld posts the read-only copy, from its own start, the read carries that copy alone, and the TUI rings a second time for the one failure | notify/alertsview.go (`foldChannelFailed`) | 2 | 2 | 3 | 12; a failure and moteld down at once, and a second bell on a row that is right. The fix would be a vmalert rule for the read-only copy, measured 2026-11-21 to ring 18 times where motel's copy alone rings once (DESIGN-metrics.md, "ChannelFailed, a second source"), unless motel also pushes a series saying a hold stands (size 3). The read-only copy's PR |
| A channel that refuses some messages and delivers others in turn, while it is the only one to tell (`failureAudience`): each delivery ends the hold's record and resolves its read-only copy, and the next failure starts another, from its own time, so the people's reads show a new `ChannelFailed` and the TUI rings again at each turn, up to once a minute | notify/channelfailed.go (`endUntold`) | 2 | 3 | 3 | 18; the row is right and named all the while, as the heartbeat's flapping beside it (the accepted row on a channel that refuses one message). Keeping the copy's start across an end and a new start in one read (a field and the ends taken after the rises: size 2, risk 2) would quiet the turns within a minute, not those a minute apart; not resolving at the end leaves a mended channel's copy up to 15 minutes. /code-review pass 2 of the read-only copy's PR |
| The notify e2e suite's check that no channel was delivered the read-only copy counts the sink's lines whose `notify_channel` field is `-`, which the sink also writes for an alert with no `notify_channel`: a `ChannelFailed` delivered without the label fails the check under the read-only copy's name | e2e/notify/run.sh (cases 6 and 15) | 2 | 2 | 1 | 4; red either way, and a `ChannelFailed` with no address is itself the regression the check is for; naming both in the message is text with no test (size 1, risk 2). /code-review of the read-only copy's PR |
| After a restart of moteld while a hold stands (the record is in memory, so the restart drops it), a channel mended within the 15 minutes the read-only copy's last post has left, and not tested with `moteld monitor test`, keeps its row in the people's reads until those minutes run out: a delivery of an alert through it ends nothing, since no record is left to end. The documented mend (the URL put right, a reload, `moteld monitor test <channel>`) ends the row at once, with a record standing or after a restart, as the test's delivery resolves the copy naming that channel alone (`endTestedCopy`). Left to their 15 minutes too: every copy on a box with the metrics package and another operator channel, where the test resolves nothing (vmalert's rules would post the addressed copies again, unfolded); and a copy naming several suspects (a counter that names no receiver). A restart alone leaves a row that is still true; a reload that takes the monitor block away answers the reads `monitor: off` | notify/channelfailed.go (`untold`, in memory; `endTestedCopy`) | 2 | 4 | 3 | 24 by product, which rule 3 would fix: one row at most 15 minutes out of date, nothing lost or hidden. Accepted: the owner's decision of 2026-11-22, with the fix for the tested mend. Taking the copies Alertmanager holds back as records at moteld's first read after a start, so that any delivery ends them, was refused: a new state (size 3, risk 2) for a row 15 minutes old at most. Resolving the copy at moteld's start was refused too: it would hide a channel still failing, the commoner case across a restart, and ring the TUI again at that channel's next failure. The read-only copy's PR (#655) and its follow-up |
| While a hold stands, a minute's read that took Alertmanager's counters just before a delivered `moteld monitor test` resolved the failure's copies posts the read-only copy again after that resolve, so the people's reads show the row again until the next read, which sees the delivery, ends the record and resolves it (a minute at most, and a bell) | notify/channelfailed.go (`watchUntold`, `endTestedCopy`) | 2 | 3 | 3 | 18; a race of milliseconds against the minute's read, and the row is right again within a minute, the hold's own end unchanged. Marking the record tested so that its re-posts stop (a field and a branch: size 2, risk 1) would end it a minute sooner. /code-review of the tested mend's PR |
| A failure Alertmanager counts in the same minute as a delivered `moteld monitor test` of its channel (the URL mended without a reload, the test run within the minute of the last give-up): that minute's read sees the rise and the delivery at once, records the rise with the delivery already in its baseline, and posts the read-only copy again after the test's resolve; the row stands, and the heartbeat is held 5 minutes later on a channel that works, until its next delivery | notify/channelfailed.go (`untoldFor`, `watchUntold`) | 2 | 3 | 1 | 6; the hold's line names the channel, and the next delivery through it (a second test) ends both. Seeding a new record from the read before (size 2, risk 2) changes the hold's baseline for every failure. Older than the tested mend: the hold's record had it before; /code-review pass 2 of the tested mend's PR |
| A delivered `moteld monitor test` is a delivery through the channel's `ch-` receiver, not through the receiver that failed: a channel that refuses one message (a Slack or Discord 400 for its size or text) and takes the test has its rows resolved while that message still fails, and the next give-up posts a new copy, from its own time, and rings; likewise another alert delivered through `ch-` in the test's wait counts as the test's | notify/admin.go (`serveTest`), notify/channelfailed.go (`endTestedCopy`) | 2 | 3 | 3 | 18; the row comes back, right and named, as the heartbeat's hold does beside it (the accepted row on a channel that refuses one message). Resolving only when the failing receiver is `ch-` needs the receiver on the copy, a label vmalert's rules and the fold would follow (size 2, risk 2). /code-review pass 2 of the tested mend's PR |

### Owed

| Owed | S | O | D | Lands in |
|---|---|---|---|---|
| A fragment of a secret that is not a whole value, nor one of its forms, nor shaped like a secret, passes the filter: an ntfy topic alone (in `send --instance ops-alerts` or a summary, or a silence's comment, where the topic is the credential) is journalled, and a silence stores its comment in Alertmanager. Pre-existing in the filter's known-value layer (whole values and their forms), which every summary goes through; found by /code-review pass 3 on monitor/silence | 3 | 5 | 5 | the filter's own change (liveswap/redact.go: a channel URL's path part as a form of its value, as a URL's password is; size 2, risk 2), not this branch: it reaches every summary motel sends. Flagged for the owner |
| ntfy's `auth` parameter is taken as a whole and form-decoded: the access token or password inside it (base64 of `Bearer tk_…` or `Basic …`) is not masked alone, and a raw `+` in it is read as a space, so the value as written is masked within the whole URL only. And a self-hosted ntfy under a sub-path with a topic named `publish`, `send` or `trigger` has its base path taken as the topic (the real topic, 7 characters or less, is under the filter's 8 anyway): that path is masked in summaries. /code-review pass 2 of the partial mask, findings 2 to 4 (second-pass hardening, rows by the review rule) | 3 | 5 | 5 | its own PR, "monitor: ntfy's auth token masked alone" (notify/app.go `urlCredentials`: the auth value from `RawQuery` as written, its base64 decoded and the scheme word cut, the suffix strip only on the public ntfy.sh host or for a topic that is a suffix word; size 2, risk 1) |
| Slack and Discord with real hooks; ntfy's formatting of Alertmanager's payload (the hotserve-1 test delivered the JSON body as the message) and its title, which needs ntfy's message templating, a server feature; Discord's colour, dark red for anything firing whatever the severity (2 5 3 = 30): Alertmanager 0.28.1's `discord_config` has no colour field (amtool refuses `color`, measured 2026-11-12), and the way round it, Discord's Slack-compatible endpoint, is untried without a real hook | — | — | — | needs the owner's hooks |
| `serveExpire` reads and deletes with the request's context: a caller gone once Alertmanager has the `DELETE` (Ctrl-C, a dropped `ssh`) cuts it, so a silence Alertmanager ended has no `monitor: silence expired` line, only Caddy's `request error` with `context canceled`. The add's change, `context.WithoutCancel(r.Context())` for the `GET` and the `DELETE`, with a test whose stub answers after the cancel (size 1, risk 1) | 2 | 2 | 3 | the expire route's next change: the Owed row monitor/silence-add-outlives-its-caller paid named the add alone. /code-review pass 1 of that branch |
| A failure all of whose attempts Alertmanager makes while moteld is not running, of an alert gone by moteld's start (a one-shot's 5 minutes, a state resolved meanwhile), is read by nothing of motel's: moteld's start reloads Alertmanager, which resets the counter before the first read, so no `ChannelFailed`, no read-only copy and no hold come. With the metrics package vmalert's rules tell the other channels; a box with one operator channel hears nothing, and its heartbeat stays green. The wider case of the accepted "a failure Alertmanager counts after motel's last read before moteld stops" | 3 | 2 | 4 | a change of its own, the owner's call: a read of the counter at moteld's stop (size 2, risk 2) does not cover a moteld that is down; a vmalert rule for the read-only copy does, but only with a series motel pushes while a hold stands, or it fights the hold's re-posts (measured, DESIGN-metrics.md, "ChannelFailed, a second source"). 24 by product. The read-only copy's PR |
| Per-app notify lines (`deploy_succeeded on` per app, app-scoped channels, reading the app's own withheld `NOTIFY_` values); the pull API and the later channel kinds, for which the `monitor: event` line is the record | — | — | — | later slices |

## e2e rows

`make e2e-notify` (e2e/notify/run.sh): a box built from e2e-motel's
image with Debian's Alertmanager installed and masked
(e2e/Dockerfile, `notify-box`), and a stub receiver (`e2e-sink`). The
suite runs `moteld monitor setup` itself, and before it asserts that
`moteld validate` warns Alertmanager is not set up, `send` exits 1 and
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
four distinct one-shots in a burst, four deliveries; one sent twice
with the same arguments, two; `send`'s
refusals, and nobody unable to send; the inhibition of the deploying
app's `watchdog_throttled` while another app's arrives after the
`group_wait`; a state from outside at +30 s and its recovery after
motel's hold and one tick; a channel answering 400 named in
`ChannelFailed` to the other operator channel, Alertmanager's failed
counter naming that channel's receivers alone; the deployer's ntfy
channel failing beside the operator's json (one integration, the
webhook) named alone and told to both operator channels; `moteld monitor test`
delivered and failed; the view probe's three keys closed from inside a
sandboxed app; no URL in either journal; a channel whose secret is
missing refused by `moteld validate` and `systemctl reload`, dropped at
`systemctl restart` with its journal line while the other channels
deliver, named dropped by `moteld monitor test`, and back after its key
is added to `_box.env` and a plain `moteld reload` of the unchanged
Caddyfile; a root:root 0600 `_box.env` refused by root's `moteld
validate`; a relaunch refused, then moteld restarted: told again,
standing, Alertmanager's alert kept alive (its `endsAt` moves) and no
recovery told, then the cause mended and a reload: the recovery after
the hold, and `alerts.json` gone; a `notify` block refused by `moteld
validate` naming `monitor`, and `moteld notify setup` exiting 1 with
`moteld monitor setup`; the block taken out by a reload while a state
fires: the state directory holds the config that sends nothing and no
URL file, the journal says so, and the alert's end, posted by root at
once rather than awaited at `resolve_timeout`, reaches no channel; a
reload that drops blog, while its refused relaunch and a producer's
state for it fire, and fails at the http app's Start (a site bound to
an address the box does not have, which `moteld validate` passes),
tried until notify's Start ran before the failure twice (the
fallback's journal line; at most sixteen tries, and fewer than two
fail the case): after each, the old
config serves with blog, the journal resolves nothing and
`external.json` keeps the state; after a minute and a quarter no
recovery has reached the operator and both alerts are active in
Alertmanager; then a reload that drops blog and serves resolves both,
with the journal line, and `external.json` lets the state go; and
with the failing channel the operator's only one, the heartbeat held
about six minutes after the failure (`monitor: heartbeat held` naming
it, `ChannelFailed` told to no channel, no `Watchdog` posted after),
and resumed once the channel is mended and `moteld monitor test`
delivers through it. The metrics suite runs the two `amtool` tests
(the rendered tree through Alertmanager's own router). Not
in e2e (unit-tested only): the
five-minute give-up of an undeliverable post, the bucket's hour.
Section 7a: `moteld monitor silence add --json --alert rollback --app
blog` answers its id and end, Alertmanager holds the silence active
(`alertname=rollback`, `app=blog`, `createdBy` `moteld monitor: root`)
and lists it, the journal has `monitor: silence added` with the id,
blog's rollback (posted by root with liveswap's labels) reaches no
channel, `--alert Watchdog` and an unknown alert are refused with the
names listed and the value not echoed, nobody is refused and told to
run it as root, `expire` leaves it expired with its journal line, the
next rollback reaches the operator, and a second `expire --json` exits
1 with `{"error":"silence <id> has already expired"}`.

The suite runs as three splits, a box each, at once (`SPLIT` in
e2e/notify/run.sh, `NOTIFY_SPLITS` in the Makefile): core (notify 3 to
10a, which count every delivery since their box began, 7a silencing 3's
failed v3), reload (11 to 14a) and heartbeat (15 alone, five minutes of
motel's hold and its resume), each after the prelude (notify 0 to 2),
which keeps the Caddyfile they put back. The boxes share the sink, which keeps
each delivery with the address it came from and answers a box's
`GET /log?mine=1` with its own, so a count is of that box's deliveries
alone. Where heartbeat runs (always on CI) the lane takes its longest
split, the heartbeat's: 581 s against 914 s for the whole suite on one box, the same 115 distinct checks
passing (2026-11-13, lima arm64 VM). CI runs all three; a local
`make e2e-notify` leaves heartbeat out unless named
(`NOTIFY_SPLITS=heartbeat`), and says so at its end, since only a change
to the hold needs it before CI. `NOTIFY_SPLITS=all` runs it whole on one
box; the suite refuses to start when a part is in no split.

## Later slices

Slice two, the metrics store (Debian's `victoria-metrics` in a private
network namespace behind a `systemd-socket-proxyd` unix socket), motel's
push, rules over its series, `metrics` in `GET /<app>`, the monitor
block's `metrics` line and `moteld monitor setup --metrics`, is
notify/DESIGN-metrics.md. Its vmalert renders a
second `ChannelFailed` with motel's labels, which Alertmanager
deduplicates; it posts no `Watchdog`, which this section once promised:
with motel's labels it would either keep the heartbeat pinging while
moteld is down or resolve motel's own (DESIGN-metrics.md, "Rules").

## History

Dated one-liners; the full text of each is in git.

- 2026-11-08 — #418 filed (on hold): notify in the moteld process, no
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
  `moteld notify send`, `test` and `setup`, the package's `Suggests`,
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
  metrics store's `metrics` line, one Caddy app; `moteld notify` is
  `moteld monitor`, and `notify` and `metrics_store` are refused by name
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
- 2026-11-13 — The two Owed rows "ntfy and json share the webhook
  integration" (2 5 3 = 30) and "an operator with no channel to hear a
  failure" (3 4 3 = 36) landed (#449): every receiver holds one channel,
  an audience being a leaf per channel under `continue` ("The route
  tree"), so a failure names the channel that failed and a working one
  of the same integration is told; and a failure no channel can be
  told of holds the heartbeat once it has stood 5 minutes with no
  delivery through the channel, until the next one ("Dead-man and
  uptime"). Runtime state, not an inhibit rule (the reasons there).
  The first load after the upgrade re-sends what fires once (an
  accepted row; docs/upgrading.md, "A receiver per channel").
- 2026-11-14 — The Owed row "an external producer's state does not
  outlive a restart" (3 4 5 = 60) landed: the engine keeps the
  producers' states in `external.json` in the state directory and tells
  them again at notify's first Start as standing reports, as liveswap
  does with `alerts.json` ("Behaviour"); docs/monitor.md no longer asks
  a producer to send `firing` again every few minutes.
- 2026-11-14 — The three Owed rows of a config load (5 2 5 = 50,
  3 4 3 = 36, 2 2 3 = 12) landed on one signal: Alertmanager's
  `alertmanager_config_last_reload_success_timestamp_seconds`, which
  0.28.1 sets after every load that resets its counters ("Failed
  delivery"). A read whose stamp is not the last read's counts
  from 0, for `ChannelFailed`, the hold's release and `moteld monitor
  test`; vmalert's rule fires on any count for five minutes after a
  load (measured in vmalert-tool: the blind spot fires 11m to 15m, a
  failure before the load does not fire again). docs/monitor.md no
  longer asks for a second test after a mend.
