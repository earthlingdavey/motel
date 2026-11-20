# Monitor

The `monitor` block in the box's Caddyfile tells you when something on
the box goes wrong: an app stuck restarting, a deploy that failed or
was refused, a rollback, a backup gone stale, a certificate that did not
renew, a disk filling up. motel spots each of these and hands it to
Debian's Alertmanager, which sends it to your channels: ntfy, Slack,
Discord, or any URL that takes Alertmanager's JSON. The block's
`metrics` line also keeps the box's numbers and each app's in Debian's
VictoriaMetrics for 14 days, and its `rule` lines alert on those
numbers. This page is how to set it up, check that it works, add your
own events, read what it sends, look at the numbers, and turn it off
again. How it works, and everything it refuses, is in
[notify/DESIGN-notify.md](../notify/DESIGN-notify.md) and
[notify/DESIGN-metrics.md](../notify/DESIGN-metrics.md).

**Experimental** since v0.7.0. Its block, its commands and the files
it writes may change in a later release.

The transcripts below come from motel's metrics e2e box at commit
01b2fd5 (Debian 13, with `prometheus-alertmanager` 0.28.1 and
`victoria-metrics` 1.112.0 from Debian). Its channels were a stub
receiver on the compose network over plain HTTP, which only the test
rig's `allow_insecure_channels` line allows, and the example blocks
here were validated with https values. Nothing on this page has been
tried against real Slack or Discord. A real ntfy topic and a reboot
with monitor set up were tried only on another box, at an earlier
commit.
[The last section](#what-has-been-verified-where) says what was
checked where.

## What it runs

| Piece | What it is | Listens on |
|---|---|---|
| motel | spots the events, and writes a `monitor: event` line to the journal for each one before delivering it | its admin socket, `/run/motel/admin.sock` |
| Alertmanager | Debian's `prometheus-alertmanager`. It routes and sends, using a config motel writes from the block | `/run/motel-notify/am.sock` only (0660 root:motel) |
| the store (with `metrics`) | Debian's `victoria-metrics`: the numbers, kept 14 days, and vmalert, which checks the rules motel writes | `/run/motel-metrics/vm.sock` only (0660 root:motel). Its TCP stays inside a network namespace of its own, `motel-metrics` |

Neither Alertmanager nor the store opens a TCP port on the box, and
neither ships a web UI.
Debian's Alertmanager page at `/` says "The Debian package of the
alertmanager does not include a web application", and the store's
`/vmui/` answers `404 page not found`. You look at things with `curl`
over the sockets and with `amtool` ([Look at the
numbers](#look-at-the-numbers)).

## Who runs these

Setup, every `systemctl` line, and edits under `/etc/motel` and
`/etc/systemd` are root's: run them over SSH as root
(`ssh root@box.example.com`), or with `sudo` in front if your
administrator's `sudo` is root's. `moteld monitor send`,
`moteld monitor test` and `moteld monitor silence` talk to motel's admin
socket, which root and the `motel` user can reach and no app can. The journal is readable by the
`adm` group.

## Set it up

As root on the box:

```sh
# 1. Debian's Alertmanager and, with --metrics, its VictoriaMetrics.
#    Each unit is masked before its package installs, so nothing ever
#    listens on Debian's ports (9093, 9094, 8428). Leave out --metrics
#    to get notifications only.
moteld monitor setup --metrics
# 2. Each channel's URL goes in the box's own secrets file, root:motel 0640.
#    With a box repo (examples/box), run on your machine
#        make secret APP=_box VAR=NOTIFY_OPS_URL
#    and push it with step 3: the box writes the file. Without one,
#    make the file (once: install empties a file that is there) and add
#    a line per URL. On public ntfy.sh the topic is the password: pick
#    a long random one (openssl rand -hex 16), and in a summary name
#    the channel (ops), never its topic.
install -m 0640 -o root -g motel /dev/null /etc/motel/secrets/_box.env
printf '%s\n' 'NOTIFY_OPS_URL=https://ntfy.sh/<a topic of your own>' >>/etc/motel/secrets/_box.env
# 3. The monitor block, in the Caddyfile's global options (below). With
#    a box repo, commit and push it: the box validates it and reloads
#    motel itself, so skip step 4. Without one, edit /etc/motel/Caddyfile.
# 4. Check it, then load it:
moteld validate --config /etc/motel/Caddyfile
systemctl reload moteld
# 5. Send one test notification to each channel:
moteld monitor test ops
```

The smallest block, with one channel and the numbers:

```
{
	admin unix//run/motel/admin.sock

	liveswap {
		…
	}

	monitor {
		channel ops ntfy {secret.NOTIFY_OPS_URL}
		operator ops
		metrics
	}
}
```

The box-repo path needs a box repository on this release's template.
[Upgrading](upgrading.md) says how one made from an earlier release's
template takes it, in its paragraph on the box repository's workflow.

On a box with nothing installed yet, setup says:

```
$ moteld monitor setup --metrics
masked prometheus-alertmanager.service, so the install starts nothing
installed prometheus-alertmanager
wrote /etc/systemd/system/prometheus-alertmanager.service.d/10-motel.conf
wrote /etc/systemd/system/motel-am.socket
wrote /var/lib/motel-notify/alertmanager.yml, a config that sends nothing, until motel renders the monitor block's channels
Alertmanager listens on /run/motel-notify/am.sock only, started by its first connection
reloaded moteld, whose config has no monitor block with channels: Alertmanager sends nothing until there is one (docs/monitor.md); add it, then systemctl reload moteld
masked victoria-metrics.service, so the install starts nothing
iproute2 is installed
installed victoria-metrics
wrote /etc/systemd/system/victoria-metrics.service.d/10-motel.conf
wrote /etc/systemd/system/motel-metrics-netns.service
wrote /etc/systemd/system/motel-vm.socket
wrote /etc/systemd/system/motel-vm.service
wrote /etc/systemd/system/motel-am-metrics.socket
wrote /etc/systemd/system/prometheus-alertmanager.service.d/20-motel-metrics.conf
wrote /etc/systemd/system/motel-vmalert.service
wrote /var/lib/motel-metrics/scrape.yml
wrote /var/lib/motel-metrics/rules.yml
started Alertmanager, once, so that it listens in the namespace (motel-am-metrics.socket) beside its unix socket
started the store and vmalert: the store listens on /run/motel-metrics/vm.sock only, its TCP in the motel-metrics namespace
reloaded moteld, whose config has no monitor block with a metrics line: nothing is pushed until there is one (docs/monitor.md); add it, then systemctl reload moteld
```

The two `reloaded moteld` lines say what the loaded config does with
what setup made: here it has no monitor block yet, so nothing is
rendered and nothing is pushed until step 4 loads one. With the block
loaded, they say `which renders the monitor block's channels into …`
and `which renders the store's rules into … and pushes`. Run again later, setup puts
its units back as the binary ships them. Of each unit and `scrape.yml`
it says `wrote …` when it changed the content and `…: content
unchanged` when it did not. `alertmanager.yml` and `rules.yml`, which
motel renders, it writes only when they are missing, and otherwise
leaves with no line. `--metrics` says `restarted the store and
vmalert` when both were running, and `started` for one that was not. `moteld monitor setup` (without
`--metrics`) restarts Alertmanager, which forgets the alerts it holds,
so run it at a quiet moment. `--metrics` on a box where Alertmanager is
already set up redoes only the store. The first time, and after an
upgrade from a build whose store reached Alertmanager through a proxy
(setup says `retired motel-am-proxy.socket …`), it restarts
Alertmanager once (`started`, here, where nothing had started it yet), so that Alertmanager holds its own listener in the
store's namespace; later runs leave it running.

Each test answers within about 15 seconds:

```
$ moteld monitor test ops
delivered to ops
$ moteld monitor test oncall
Error: oncall: Alertmanager could not deliver (discord: other); journalctl -u prometheus-alertmanager says more
```

It exits 1 unless the message was delivered, and it never prints the
URL, not even one given in place of the channel's name: a name the
block does not have is refused with the block's channel names, and
without the name given, since moteld's journal keeps the refusal. A
test goes to its channel whatever that channel's `severity`
floor is.

### The secrets file

`/etc/motel/secrets/_box.env` holds one `NOTIFY_<NAME>=<url>` line per
URL. The block names each value as `{secret.NOTIFY_<NAME>}`, because a
Slack or Discord hook URL works as a password by itself and the
Caddyfile is not secret. motel reads the file and no app is given it.
Every value in it is masked in what motel sends and journals
(`[redacted:NOTIFY_<NAME>]`), and so is the credential of an ntfy,
Slack or Discord URL on its own (the topic, the hook's token, ntfy's
`auth` parameter as a whole) and of a heartbeat's ping URL (its last
path segment, past a `/start`, `/fail` or `/log` and past a number
from 0 to 255, on any host: Healthchecks' ping endpoints, so its check
UUID), as `[redacted:partial:NOTIFY_<NAME>]`, when it is 8 characters
or more. A shorter one is not masked; one that is an ordinary word (a
topic like `monitoring`, a ping URL ending `/api/heartbeat`) is masked
wherever that word appears.
It must be **root:motel 0640**. A file made elsewhere and moved in with
`mv` keeps its owner, group and mode: one made in `/root` with
`install -m 600`, or by an editor under umask 077, is root:root 0600.
One made in place gets the directory's group, motel, but keeps its
mode, and 0600 shuts motel out all the same. `validate`, run as root,
refuses such a file, though root can read it:

```
$ ls -l /etc/motel/secrets/_box.env
-rw------- 1 root root 269 Nov 12 17:46 /etc/motel/secrets/_box.env
$ moteld validate --config /etc/motel/Caddyfile
… monitor: the motel user cannot read the box's secrets file /etc/motel/secrets/_box.env (root:root 0600): a reload would be refused and a restart would drop every channel; chown root:motel /etc/motel/secrets/_box.env && chmod 0640 /etc/motel/secrets/_box.env
```

A reload is refused too, and the journal says why (`monitor: reading
the box's secrets: open /etc/motel/secrets/_box.env: permission
denied`). A restart or a reboot loads the config without a single
channel, and the journal says so with `monitor: the box's secrets file
cannot be used: every channel that reads a secret is dropped from this
load` and `monitor: no operator channel is left in this load: box
events are journal lines only`. Fix it with:

```sh
chown root:motel /etc/motel/secrets/_box.env && chmod 0640 /etc/motel/secrets/_box.env
systemctl reload moteld
```

`moteld reload --config /etc/motel/Caddyfile` does the same: with a
monitor block in the file it loads it even when the Caddyfile has not
changed, as `systemctl reload` does, since the block reads
`_box.env`, which the Caddyfile does not hold.

A key that is missing is refused by `validate` and by a reload, and so
is one set to nothing (`NOTIFY_OPS_URL=`):

```
monitor: channel phone: secret NOTIFY_PHONE_URL is not in the box's secrets (/etc/motel/secrets/_box.env)
monitor: channel ops: secret NOTIFY_OPS_URL is empty in the box's secrets (/etc/motel/secrets/_box.env): give it the channel's URL
```

`moteld monitor test <channel>` on a channel a boot dropped says so
(`channel phone was dropped from this load: its secret could not be
used …`) and names the mend.

### The block's lines

```
monitor {
	channel <name> json|ntfy|slack|discord {secret.NOTIFY_<NAME>} [severity critical|warning|info]
	operator <channel>...           # who hears every event: the box's, and every app's
	bind <identity> <channel>       # an app's deployer: hears the alerts labelled with an app that trusts the identity (below)
	heartbeat {secret.NOTIFY_<NAME>} # a dead-man check's ping URL (below)
	deploy_succeeded on             # off by default
	metrics                         # keep the numbers (needs setup --metrics)
	rule <name> …                   # your alerts on the numbers (needs metrics)
}
```

- **`channel`**: `severity warning` means a channel hears warnings and
  criticals but no info. A URL has to be https, to a public address and
  host name; anything else is refused by name.
- **`operator`**: required. It names the channels that hear everything.
- **`bind`**: `<identity>` starts as a deploy's `deployed_by` does in
  `GET /<app>`, and is not written as the `deploy_trust` block:
  `local:/etc/motel/alice.pub`, or
  `"oidc:<issuer> repository=<org>/<repo>"`, quoted because an oidc
  identity always carries a claim after a space. Give only claims the
  app's `deploy_trust` pins. `deployed_by` also shows what the token
  carried (`ref=…`, `actor=…`, `sub=…`), and a bind with one of those
  is refused unless the block pins it. A bind to an identity that no
  app trusts is refused in the same words (`no app's deploy_trust
  trusts this identity`), so revoking a key also means removing its
  bind. A bound channel hears the alerts that carry the app's `app`
  label: liveswap's events about the app (its deploys and rollbacks,
  `watchdog_throttled`, `relaunch_refused`), the store's rules and
  your `rule` lines about it, `post_deploy_regressed`, and
  `moteld monitor send --subject app:<name>`. The box's own alerts
  about the app, such as `backup_stale` and `drill_failed` for its
  backups and `cert_failed` for its domains, carry no `app` label and
  go to the operator alone.
- **`heartbeat`**: without it, every load warns `monitor: no heartbeat
  in this load: nothing outside the box hears that it, moteld or
  Alertmanager stopped`.

Every line, and the less common ones (`secrets_file`,
`backup_record`), is in DESIGN-notify.md's [The monitor
block](../notify/DESIGN-notify.md#the-monitor-block). Alertmanager's
paths are not lines: its state directory is fixed at
`/var/lib/motel-notify` and its socket at `/run/motel-notify/am.sock`,
the ones setup makes.

## A heartbeat: hear that the box stopped

Nothing on a box can report that the box itself is down. A dead-man
check off the box can: it expects a ping on a schedule and alerts you
when one is missed. motel pings it every five minutes for as long as
moteld and Alertmanager both run. A restart of moteld, or a reload that
changes Alertmanager's config, starts the five minutes again, so the
ping after one can come later: 7 minutes 20 seconds after the last was
measured, and about nine is the most. The Grace below covers it.

1. Make a check at [Healthchecks](https://healthchecks.io) (or another
   service that takes a plain ping URL) with **Period 5 minutes** and a
   Grace of 10 minutes. It then reports the box down about 15 minutes
   after the last ping.
2. Put its ping URL in the secrets file as `NOTIFY_HC_URL`: the UUID
   form, `https://hc-ping.com/<uuid>` (Healthchecks' default), not the
   slug form `https://hc-ping.com/<ping-key>/<slug>`. motel masks the
   UUID on its own too; in the slug form it can mask only the slug,
   which is then masked wherever it appears, your box's name if the
   check is named after it, and `moteld monitor send` refuses a name,
   instance or source that contains it.
   Treat the URL as a password: anyone who has it can keep the check
   green while the box is down. Add the
   line `heartbeat {secret.NOTIFY_HC_URL}` to the block. With a box
   repo, do both there (`make secret APP=_box VAR=NOTIFY_HC_URL`, and
   the line in the repository's Caddyfile) and push: a line or a secret
   edited in on the box is gone at the next push, which replaces both
   files.
3. Without a box repo, `moteld validate --config /etc/motel/Caddyfile`,
   then `systemctl reload moteld`. The first ping goes out within the
   minute.

The ping is Alertmanager's JSON for an alert named `Watchdog`, POSTed
to the URL: `"summary":"moteld is running and can reach Alertmanager"`.
Healthchecks treats any request to the URL as a ping. **Pause the check
before you stop moteld or Alertmanager on purpose**, or it will page you.

To see when the pings went out, or how long a gap was, read the check's
log at Healthchecks. The box holds no history of them: the store does
not keep `alertmanager_notifications_total` (below, "PromQL over the
socket, as root"), and Alertmanager starts each of its notification
counters (`alertmanager_notifications_*` and
`alertmanager_notification_requests_*`) again from 0 whenever it loads
its config: when it starts, and whenever motel has it reload, which is
at every start of moteld and at every reload of moteld that changes what
motel renders for it (a channel's URL included) or follows one it did
not answer. Read live, the counts since the last load are
`curl -s --unix-socket /run/motel-notify/am.sock http://am/metrics | grep 'receiver_name="heartbeat"'`.
motel and the store's `ChannelFailed` rule read the time of that load
beside the counts (`alertmanager_config_last_reload_success_timestamp_seconds`):
after a load every count is one since it, so a counter that fell is no
failure, and a failure or a delivery just after a load is counted even
when it brings a counter back to where it stood before. A delivery in flight when
Alertmanager loads its config is cancelled and counted in
`alertmanager_notifications_failed_total` under
`reason="contextCanceled"`; the new config sends it again, and neither
motel nor the rule reads that reason, so a count there after a load is
no failed channel.

`alertmanager_notifications_total` is no count of messages.
Alertmanager raises it once for each pass at sending a group's
notification to a receiver, before the pass's first attempt (its
retries are inside the pass). A pass whose attempts all fail counts,
and the group tries again at its next flush, which counts again. On a
receiver that sends no recovery (`heartbeat`, and each channel's
`event-` receivers and its `ch-` receiver, which `moteld monitor test`
goes to), the pass for an alert's end counts too, though nothing is
sent; an alert never delivered has no such pass. So a
`moteld monitor test ops` whose first pass delivers counts 2 on
`ch-ops`, unless a config load between restarts the count: one at the
delivery, and one when the test alert ends five minutes later (the 2
measured on hotserve-1, 2026-11-14; when, from Alertmanager's
source). A one-shot event counts the same way on each `event-`
receiver, each time it is sent: each is its own alert.
`heartbeat`'s rises once with no ping when motel stops posting the
`Watchdog` alert (the heartbeat held, or moteld stopped while
Alertmanager runs) and the alert ends. The messages that went are
`alertmanager_notification_requests_total` less
`alertmanager_notification_requests_failed_total`, each attempt (a
retry too) being a request, which is how `moteld monitor test` reads a
delivery.

The check also pages when a channel failed and no other channel could
be told: motel stops the pings then, so a dead channel is not hidden
behind a green check. If it pages while the box is up, look for
`monitor: heartbeat held` in the journal (below, "In the journal").

## Uptime: hear that a site stopped answering

The heartbeat tells you moteld runs. It does not tell you that a site
answers its visitors. For that, use an external uptime monitor (a
keyword check) against each site: it fetches a page and looks for a
word that only the app's own page contains, so an error page reads as
down. A cached copy would still read as up, so check a page the cache
does not hold, such as the app's health path if the site routes it to
the app. penaltybox does not box such a monitor by default: it counts
only responses an app labels level 2 or 3, and an ordinary page is
level 1 ([penaltybox](../penaltybox/README.md#the-wire-contract)). A
site set to `min_level 1` counts every response, the monitor's too, so
check a path that site does not penalise.

## Your own events: `moteld monitor send`

Anything that can run a command can send an event: cron, a systemd
unit, a script. `moteld monitor send` posts it over the admin socket.
Each event becomes a `monitor: event` line in the journal and goes to
the operator's channels (with `--subject app:<name>`, to that app's
deployers too). Two things keep an event from a channel. A channel
with a `severity` floor hears nothing below it, and an event is a
warning unless `--severity` says otherwise. And one key (the source,
subject, name and instance together) is posted four times at once, then
one more every 15 minutes: past that, an event is a journal line only,
marked `"suppressed":"more than four an hour for this key"`.

An event is either a one-shot (the default) or, with `--state firing`,
a state that holds until a `--state resolved` with the same `--source`,
`--subject`, `--name` and `--instance`. A `resolved` that differs in
any of the four, or one sent when nothing is firing, does nothing, and
answers `resolved nothing: no state fires under <key> …` (exit 0, so a
producer may resolve whatever it last said). On a box where
`moteld monitor setup` has not run, `send` exits 1 with `journalled as
<key>, and delivered nowhere: …`. `--summary` is always needed: one line, at
most 200 characters. When a state resolves, the message repeats the
summary it fired with, not the one you give.

Every one-shot you send is delivered, the same one again too: a
failure sent twice, two minutes apart and with the same arguments,
reaches your channels twice, up to the limit above. Each carries its
own `occurred_at` label (in a json or ntfy channel's payload), the
time motel took it. motel does not group or mute repeats; the service
that receives them is the place for that. Group there on the alert's
`labels.key` (with `labels.severity`), the same for every occurrence;
`fingerprint`, `groupKey` and `groupLabels` differ for each, since
they are made from every label. `--instance` is for telling
things apart (the unit that failed, the disk, the job): it is part of
the key, so each instance has a limit of its own, and an instance new
at every send (a timestamp, a UUID) leaves no limit to hold back a
storm. An instance the filter would take for a secret is refused: a
40-hex git commit id is, a numeric run id or a lowercase UUID is not.

A state outlives a restart of moteld (an upgrade of the package restarts
it): motel keeps what is firing in `/var/lib/motel-notify/external.json`
and tells it again when it starts, and the later `--state resolved`
ends it. Alertmanager ends an alert five minutes after motel last
told it, which motel does about every minute: a restart back within
three minutes sends no second message, and a stop of five minutes or
more sends your channels `[RESOLVED]` while moteld is down, then a new
`[FIRING]` when it starts (in between, either can happen).
A reload that takes the monitor block out, or a start without it,
forgets the states. A repeat of
`--state firing` while the state fires sends no second message, so a
producer may send it as often as it checks.

The names motel uses itself are refused:

```
$ moteld monitor send --source reboot --name watchdog_x --summary x
Error: name is one of motel's own events, or starts with watchdog_ as theirs do; name yours something else
```

### A reboot that waits

Debian's unattended upgrades leave `/run/reboot-required` when an update
needs a reboot ([Hardening](hardening.md)), and never reboot by
themselves. This timer reminds you once a day for as long as the file
is there. A reboot both clears the file and empties the memory of
moteld and Alertmanager, so the reminder is a one-shot: a state would
never get its recovery.

`/etc/systemd/system/motel-reboot-required.service`:

```ini
[Unit]
Description=Tell the operator a reboot waits (moteld monitor send)
ConditionPathExists=/run/reboot-required

[Service]
Type=oneshot
Environment=HOME=/root
ExecStart=/usr/bin/moteld monitor send --source reboot --name reboot_required --summary "a reboot waits: an update needs it (cat /run/reboot-required.pkgs)"
```

`/etc/systemd/system/motel-reboot-required.timer`:

```ini
[Unit]
Description=Once a day, tell the operator a reboot waits

[Timer]
OnCalendar=09:00
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
systemctl daemon-reload
systemctl enable --now motel-reboot-required.timer
systemctl start motel-reboot-required.service   # once now: sends only if a reboot waits
```

With no reboot waiting, the run does nothing (`systemctl status` says
`ConditionPathExists=/run/reboot-required was not met`). With one
waiting, its journal says `sent external:reboot/box/reboot_required`.
`Environment=HOME=/root` stops moteld from warning that it cannot find
its config directory each time.

### A unit that failed: `OnFailure=`

To hear about a job of your own failing (a nightly report, an import),
write one template unit and point the job's `OnFailure=` at it.
`/etc/systemd/system/motel-send-failed@.service`:

```ini
[Unit]
Description=Tell the operator that %i failed (moteld monitor send)

[Service]
Type=oneshot
Environment=HOME=/root
ExecStart=/usr/bin/moteld monitor send --source systemd --name unit_failed --instance %i --summary "%i failed: see journalctl -u %i"
```

`--instance` takes at most 128 letters, digits and `._:/@-`, and the
summary at most 200 characters, so a unit name over about 90
characters, or one with an escape in it (a template instance such as
`srv\x2ddata`), is refused (`Error: instance is at most 128 letters,
digits and ._:/@-`). The notifier unit then fails, and no channel
hears that the job failed: name the units you point here plainly.

Then, in the job's unit (or in a drop-in made with
`systemctl edit nightly-report.service`):

```ini
[Unit]
OnFailure=motel-send-failed@%n.service
```

After `systemctl daemon-reload`, a failing `nightly-report.service`
reaches the operator's channels as `[FIRING] unit_failed`, with the
summary naming the unit. It is a warning, so a channel whose floor is
`severity critical` does not hear it, and a unit that fails more often
than the four-then-one-a-quarter-hour above is a journal line only for
the rest. motel already reports its own failures (backups and the
drill among them), so there is no need to point `motel-backup.service`
at this.

## Read what it sends

### In the journal

Every event is written to moteld's journal before it is delivered, so the
journal is the record even when a channel is down. These two are from
a test of `--state firing`, then `--state resolved` (the reboot
recipe's one-shot says `"state":"oneshot"`):

```
$ journalctl -u moteld -o cat | grep '"monitor: event"'
{"level":"info","ts":1794506376.5473716,"logger":"monitor","msg":"monitor: event","key":"external:reboot/box/reboot_required","state":"firing","severity":"warning","summary":"a kernel update waits for a reboot","source":"external:reboot"}
{"level":"info","ts":1794506464.4814832,"logger":"monitor","msg":"monitor: event","key":"external:reboot/box/reboot_required","state":"resolved","severity":"warning","summary":"a kernel update waits for a reboot","source":"external:reboot"}
```

The `resolved` line comes 30 to 35 seconds after the `--state
resolved` (33 on hotserve-1), not at once: a producer's state that
clears is held 30 seconds before its recovery is told, and one that
fires again within them tells none. Until the line, Alertmanager still
lists the alert as active ([`amtool`](#what-is-firing-with-amtool)),
and `/var/lib/motel-notify/external.json` still holds the state, with
`"firing":false`. At the line the state goes from the file (the file
with it, when no other state stands), and the channels get its
`[RESOLVED]` within a minute after. A key that has spent its four
transitions (a producer that flaps; above) gets the line at the same
time with `"held":"more than four transitions an hour for this key;
posted when the bucket allows"`, and the alert and the file's entry
stay until the bucket allows, up to 15 minutes later, when a second
`resolved` line comes.

`state` is `firing`, `resolved` or `oneshot`. The `key` says what the
event is about: `<source>/<subject>/<name>[/<instance>]`. Other lines
start the same way, with `monitor:` or `monitor metrics:` (a warning
or an error says what to do). If the block was loaded before setup ran,
you will see
`monitor: Alertmanager is not set up on this box (run moteld monitor setup as root); events are journal lines only until it is`,
and `moteld validate` and `moteld reload` say it too, as a `WARNING:`
line. They also warn when the units a setup wrote (or the store's `scrape.yml`) differ from the ones
this moteld would write (after an upgrade that changed them), and name
the setup to run again.
When Alertmanager cannot deliver, `journalctl -u prometheus-alertmanager`
gives its reason, with the receiver and the channel's kind but never
the URL:

```
time=2026-11-12T17:45:33.575Z level=ERROR source=dispatch.go:360 msg="Notify for alerts failed" component=dispatcher num_alerts=1 err="state-warning-oncall/discord[0]: notify retry canceled due to unrecoverable error after 1 attempts: unexpected status code 400"
```

Every load of a config rendered from a monitor block with channels, with
a `heartbeat` line or without, also logs one warning, for the
heartbeat's route (without the line, the route leads nowhere). Setup's
config that sends nothing, which motel also puts back when no block
with channels is loaded, has no such route and logs no such warning:

```
time=2026-11-14T08:17:49.812Z level=WARN source=main.go:510 msg="repeat_interval is less than group_interval. Notifications will not repeat until the next group_interval." component=configuration repeat_interval=4m0s group_interval=5m0s route="{}/{alertname=\"Watchdog\"}"
```

That one is expected: the heartbeat's route repeats every 4 minutes
under a 5-minute interval on purpose, which keeps the pings 5 minutes
apart and not 10 (`notify/render.go`).

motel reads Alertmanager's failure counters, and tells the operator's
other channels about a failure as `ChannelFailed` (DESIGN-notify.md,
"Failed delivery"). `ChannelFailed` is a warning, so a channel with
`severity critical` never hears it. Each channel has receivers of its
own in Alertmanager's config (`ch-ops`, `event-warning-ops`,
`state-critical-ops`, `fallback-ops`, …), so a failure names the one
channel that failed, and every other operator channel that takes
warnings hears it, whatever its kind.

With no other channel to tell (a single operator channel, or the
others set to `severity critical`), motel **holds the heartbeat**
instead: once the failure has stood 5 minutes with nothing delivered
through that channel since, it stops pinging, and the heartbeat's
check off the box reports the box down through its own channels (its
email, say). The journal says why:

```
{"level":"warn","logger":"monitor","msg":"monitor: heartbeat held: a failed delivery no channel was told of; the heartbeat's check off the box will report this box down","suspects":["ops"],"failed_at":"…","resumes":"at the next delivery through the channel: mend it, then moteld monitor test ops"}
```

Mend the channel, then `moteld monitor test ops`: the pings start again
within a minute (`monitor: heartbeat resumed`), whether or not mending
the URL reloaded Alertmanager. A reload that takes the channel out, or a
restart of moteld, resumes them too. A second
operator channel that takes warnings is still the better answer: it
names the failure, where the dead-man check can only say the box is
down.

A state is told twice: once when it fires (after 30 seconds, so a burst
arrives as one message) and once when it resolves. While it holds, a
reminder comes every 4 hours (every 24 for a disk). A deploy in
progress mutes its own app's watchdog alerts (`watchdog_*`): one that
fires during the deploy is told within a minute after it ends if it
has not recovered by then (a recovery counts once the app has stayed
healthy for 30 seconds, or its `watchdog_grace` if longer), a recovery
that comes during the deploy is told once the deploy has ended,
whatever it came to, and a loop that begins and has recovered within
one deploy is not told. The events motel reports
itself, and when each resolves, are in DESIGN-notify.md's
[Events](../notify/DESIGN-notify.md#events) table.

### What each channel kind shows

Here is what each kind received for a firing `disk_low` and its
recovery (from the stub receiver; a real app's rendering is
untested):

| Kind | What is posted | Firing | Resolved |
|---|---|---|---|
| `json` | Alertmanager's webhook JSON as it is: `status`, `receiver`, and for each alert its `labels` (`alertname`, `severity`, `key`, `app` or `instance`) and `annotations.summary` | `"status":"firing"` | `"status":"resolved"` |
| `ntfy` | the same JSON, POSTed to the topic URL with no title. ntfy shows the JSON text itself as the message. A critical is sent at ntfy's priority 5 (urgent) and a warning at 4 (high), as an `X-Priority` header, `ChannelFailed` and `moteld monitor test` at 4 too; info at the default | — | — (a recovery has its alert's priority: the header is fixed per severity) |
| `slack` | an attachment: title `[FIRING] disk_low` (with the app's name after it for an app event), text the summary, one line per alert; no title link, and the title as the fallback text | colour by severity: `danger` (red) for a critical, `warning` (yellow) for a warning, blue for info | title and each line prefixed `[RESOLVED]`, colour `good` |
| `discord` | an embed: the same title, the summary as its description | colour 10038562 (dark red), whatever the severity: Alertmanager 0.28 has no setting for it | `[RESOLVED]`, colour 3066993 (green) |

## Look at the numbers

With the `metrics` line, motel pushes every 30 seconds. The store keeps
each series for 14 days. There is no dashboard. You read the numbers in
three ways.

### `GET /<app>`, as a deployer

The deploy status that `GET /<app>` returns (with the deploy token, as
a deploy uses it: [liveswap/README.md](../liveswap/README.md)) gains a
`metrics` object over the last hour, or over the day with `?range=24h`:

```json
"metrics":{"window":"1h","at":"2026-11-12T17:50:00Z","memory_bytes":{"now":2830336,"max":2830336},"cpu_cores":{"avg_5m":0.003289,"avg":0.003289},"requests":{"total":41,"per_second":0.011389,"by_class":{"2xx":40,"4xx":1},"5xx_share":0,"p50_ms":2.5,"p95_ms":4.8,"cache_hit_share":0.975}}
```

Any other range is refused: `{"error":"range must be one of 1h, 24h"}`
(HTTP 422). `requests` appears only for an app whose site has
`observe <app>` in front of it ([liveswap/README.md](../liveswap/README.md)).
`memory_bytes.max`, `cpu_cores.avg` and `requests` are over the range.
`memory_bytes.now`, `memory_max_bytes` (the memory cap) and
`cpu_cores.avg_5m` are the present, whatever the range: the app's last
push within 75 seconds, and for `avg_5m` the mean of its CPU over the
five minutes up to its newest minute, read a minute at a time. `now`
and `memory_max_bytes` are left out when no push of the app came in
those 75 seconds (it stopped, or its pushes are failing), and `avg_5m`
when its newest minute is over two minutes old, about three minutes
after the last push; so a missing `memory_max_bytes` means no cap only
while the app runs. `at` is the time of the range's newest point, which the
view reads a minute apart over the hour and five minutes apart over
the day, not the time of those three. A number with nothing behind it
in the range is left out. When the store does
not answer the view's queries, `metrics` is `{"error":"the metrics
store did not answer"}`, and moteld's journal has the cause as a
warning, `monitor metrics: GET /<app> answered without metrics: the
metrics store did not answer the view's queries`, with the app, the
range and the error (at most one per app and range every 30 seconds).

### PromQL over the socket, as root

The store answers Prometheus's query API on its socket:

```sh
curl -s --unix-socket /run/motel-metrics/vm.sock -G \
  --data-urlencode 'query=motel_app_up' http://vm/api/v1/query
```

```
{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"motel_app_up","app":"blog","job":"moteld"},"value":[1794505816,"1"]},{"metric":{"__name__":"motel_app_up","app":"shop","job":"moteld"},"value":[1794505816,"0"]}]},…}
```

The series motel pushes are `motel_app_up`, `motel_app_release_info`,
`motel_app_memory_bytes` (and `motel_app_memory_max_bytes` for a capped
app), `motel_app_cpu_seconds_total`, `motel_app_observed` (an app
with `observe`), `motel_app_requests_total` (by `class`, with
`observe`), `motel_app_response_seconds` (a histogram),
`motel_app_cache_responses_total`, `motel_disk_avail_bytes`,
`motel_metrics_push_failed_total` and `motel_notify_delivery_failed_total`,
along with Caddy's own `go_*`, `process_*` and `caddy_*`. To list what
is there:

```sh
curl -s --unix-socket /run/motel-metrics/vm.sock -G \
  --data-urlencode 'match[]={job="moteld"}' http://vm/api/v1/label/__name__/values
```

`query_range` (`/api/v1/query_range` with `start`, `end` and `step`)
works the same way. What each series means is in DESIGN-metrics.md's
[What motel pushes](../notify/DESIGN-metrics.md#what-motel-pushes).

The store also scrapes itself, vmalert and Alertmanager, and keeps
only the series motel reads (`/var/lib/motel-metrics/scrape.yml`):
`vm_free_disk_space_bytes`, which the store's disk rule reads,
`alertmanager_notifications_failed_total` and
`alertmanager_config_last_reload_success_timestamp_seconds`, which vmalert's
`ChannelFailed` reads, and each job's `up` and `scrape_*` (motel reads
`up{job="vmalert"}` to tell vmalert down). The rest would cost disk
and memory with nothing reading it, and a series motel comes to read
later is added to the list then, which breaks nothing
([What the store scrapes](../notify/DESIGN-metrics.md#what-the-store-scrapes)).
Each one's whole page is still there to read live, the store's over
its socket and the other two inside its namespace:

```sh
curl -s --unix-socket /run/motel-metrics/vm.sock http://vm/metrics
ip netns exec motel-metrics curl -s http://127.0.0.1:8880/metrics
ip netns exec motel-metrics curl -s http://127.0.0.1:9093/metrics
```

A box whose store was set up before motel kept only these goes on
keeping every series until root runs `moteld monitor setup --metrics`
again (each load warns that `scrape.yml` differs). After that, the
series it no longer keeps age out over the 14 days, and nothing that
reads the store reads them.

### What is firing, with `amtool`

Debian's Alertmanager package includes `amtool`. It speaks TCP only, so
on a box set up with `--metrics` you run it inside the store's
namespace, where Alertmanager itself listens on `127.0.0.1:9093`
(`motel-am-metrics.socket`, beside its unix socket):

```
$ ip netns exec motel-metrics amtool --alertmanager.url=http://127.0.0.1:9093 alert
Alertname        Starts At                Summary                                                                                                   State
Watchdog         2026-11-12 17:44:03 UTC  moteld is running and can reach Alertmanager                                                              active
disk_low         2026-11-12 17:44:03 UTC  the filesystem holding /var/lib/liveswap is 88% full                                                      active
reboot_required  2026-11-12 17:45:17 UTC  a kernel update waits for a reboot                                                                        active
```

vmalert answers in the same namespace, and lists the rules it
evaluates:

```sh
ip netns exec motel-metrics curl -s http://127.0.0.1:8880/api/v1/rules
```

There are three groups: `motel-store` (motel's fixed rules),
`motel-channel-failed` and `motel-operator` (your `threshold` and
`disk_fill` rules). A `post_deploy` rule is never listed here: motel
evaluates it, not vmalert. Each `rules` list is empty while there is
nothing to evaluate.

On a box with notifications only, ask the API over its socket:
`curl -s --unix-socket /run/motel-notify/am.sock http://am/api/v2/alerts`.

A person with the viewer role on the box (docs/people.md) reads what
is firing over HTTPS, with no ssh and no forward: `motel monitor
alerts` lists every alert (since when, and whether a silence or a
deploy holds it, until when) and the silences, and `:alerts` in
`motel` shows the same. A viewer of one app reads that app's alone:
`motel monitor alerts --app <app>`. Neither can add or end a silence.

From your laptop, forward the socket to a socket of your own, in a
directory only you can open, and ask that:

```sh
mkdir -m 700 -p ~/.motel
ssh -fN -o ExitOnForwardFailure=yes \
  -o StreamLocalBindMask=0177 -o StreamLocalBindUnlink=yes \
  -L ~/.motel/am.sock:/run/motel-notify/am.sock root@box.example.com
curl -s --unix-socket ~/.motel/am.sock http://am/api/v2/alerts
```

`-f` puts ssh in the background only once the forward is listening
(with `ExitOnForwardFailure=yes`; measured), so the `curl` after it
finds the socket; a failed forward is ssh's exit status, not a
background job's. End it with `pkill -f motel/am.sock`.

Do not forward it to a TCP port (`ssh -L 9093:…`). Alertmanager's API
has no login, so a port on your laptop's loopback gives it, silences
included, to every program on the laptop and to any web page that
rebinds a DNS name to 127.0.0.1. The socket file is created
`srw-------`, and `StreamLocalBindUnlink=yes` replaces the file an
earlier ssh left, which otherwise fails the forward with `Address
already in use`. This was measured with OpenSSH 10.0 against an sshd on
localhost, not against a real box. `amtool` cannot use a socket, so run
it on the box (`ssh root@box.example.com ip netns exec motel-metrics
amtool …`, on a box set up with `--metrics`). The store's socket
forwards the same way (`~/.motel/vm.sock:/run/motel-metrics/vm.sock`).

## Silence an alert

While you fix something you already know about, silence its alert so
that the channels stop repeating it. Run it as root:

```
$ moteld monitor silence add --comment "resizing the volume" --duration 4h --alert disk_low
silence 22e1715e-25d1-4986-bec3-1ecf8bdb376f added; it ends at 2026-11-13T21:37:22Z
$ moteld monitor silence expire 22e1715e-25d1-4986-bec3-1ecf8bdb376f
silence 22e1715e-25d1-4986-bec3-1ecf8bdb376f expired
```

(The output's shape is the notify e2e box's; that run's silence was 10
minutes.)

You name what to silence with up to three flags, and motel builds the
silence from them:

- `--alert <name>`: one of the alerts motel raises on this box, such as
  `disk_low`, `deploy_failed`, `watchdog_throttled`, `cert_failed`,
  `backup_stale`, `ChannelFailed`, or one of your `rule` lines' names;
- `--app <name>`: one of the apps the box runs: every alert with its
  `app` label, your own `moteld monitor send --subject app:<name>`
  events among them, but not the box's alerts about it, such as
  `backup_stale`, `drill_failed` or `cert_failed`;
- `--release <version>`: one release of the app, as its directory under
  `/var/lib/liveswap/<app>/releases/` names it or as the app's deploy
  records do (the `deploys` list of `GET /<app>`, a failed deploy's
  version among them), so that the next release's alerts still reach
  you. It needs `--app` and one of the alerts that carry a release:
  `rollback`, `deploy_succeeded`, `deploy_failed` or
  `post_deploy_regressed`. A refused deploy writes no record, so
  silence `deploy_refused` by `--alert` and `--app`; so too a failed
  deploy with no record: one that failed before it began (`GET
  /<app>?deploy=<version>` answers `404`), one whose record could not
  be written (moteld's journal says `deploy record: not written`), and
  one whose record has been pruned (the newest `keep` versions not on
  disk keep theirs).

Give `--alert`, `--app` or both. Each is an exact match. A name motel
does not know is refused with the list of the names it takes, and so
is a pair that would match nothing, such as `--alert disk_low --app
shop` (`disk_low` is about the box, not an app):

```
$ moteld monitor silence add --comment x --alert disk_lo
Error: --alert names none of the alerts motel raises on this box; they are ChannelFailed, backup_run_failed, …
```

`Watchdog`, the heartbeat, is not in that list: silencing it would stop
the pings, and your check off the box would report the box down. To
quiet the heartbeat, pause that check instead. Your own events from
`moteld monitor send` cannot be silenced by their name yet, only by
`--app` for an app's.

The comment is required, one line of plain text (no tab or escape
sequence), at most 200 characters; anything in it that looks like a
secret is masked, as in a notification's summary.
The duration is from `1m` to `24h`, `2h` if you leave it out, from now.

Each silence added or expired is a line in moteld's journal
(`monitor: silence added` with its id, alert, app, release, end,
comment and `created_by`, which is `moteld monitor: <your user>`;
`monitor: silence expired` with its id and `expired_by`). With
`--json`, `add` prints `{"id":"…","ends_at":"…"}`, `expire` prints
`{"id":"…"}`, and an error is `{"error":"…"}` with exit 1; a second
expire of one silence is `silence <id> has already expired`. An add
that Alertmanager did not answer within 10 seconds says that
Alertmanager may have taken the silence all the same: list its
silences (below) before you add it again. The journal then has
`monitor: silence add unanswered: Alertmanager may hold the silence`,
with what was asked but no id, and no `silence added` line. Ctrl-C
does not undo an add that has reached motel, nor does a dropped `ssh`:
it runs to Alertmanager's answer, and the journal has its line, with
the id if Alertmanager gave one, though the command printed none. Read
the id there, or list the silences, before you add it again; expire it
by that id if you did not mean it. Another
user than root is told `the admin socket … admits root and the motel
user: run moteld monitor silence as root`.

There is no `moteld monitor silence list`. Ask Alertmanager:
`curl -s --unix-socket /run/motel-notify/am.sock http://am/api/v2/silences`.
A person with the viewer role on the box reads the silences that are
active or wait to start from their laptop, with every alert: `motel
monitor alerts`, or `:alerts` in `motel` (docs/people.md).
Silences you post to the socket yourself work too, but motel checks
nothing of them and leaves no line in its journal.

## Your own alerts: `rule` lines

With `metrics`, the block also takes rules of your own. Each is one of
motel's kinds, and you set only its numbers:

```
monitor {
	channel ops ntfy {secret.NOTIFY_OPS_URL}
	operator ops
	metrics
	rule errors_high threshold 5xx_share > 0.5 for 1m min_requests 3
	rule disk_filling disk_fill within 1h
	rule blog_regressed post_deploy app blog {
		window 4m
		min_requests 10
		max_5xx_share 0.1
		failures 1
	}
}
```

- **`threshold`**: a metric (`5xx_share`, `p50`, `p95`, `request_rate`,
  `cache_hit_share`, `memory_share`, `cpu_cores`), `>` or `<` a value,
  held `for` a time, for every app or for `app <name>`.
  `memory_share` is an app's working set over its `memory_max`, and
  motel pushes the cap to the store only while the app runs and its
  status (`GET /<app>`) lists `memory_max` under `caps`, which it does
  when the host enforces the cap: an every-app rule watches those apps
  alone, a rule naming an app with no `memory_max` is refused, and one
  naming an app whose cap status lists under `caps_unenforced`, or
  not at all after a caps change that failed, loads and does not fire
  while it is so. A cap status listed under `caps` until then (a caps
  change that failed on a running app) is still read from the store
  for a few minutes after its last push: through them an every-app
  rule still watches the app, and either rule can fire on the old cap.
- **`disk_fill`**: a filesystem that will be full `within` the time
  given, at the rate it is filling. The rate is read over the last
  quarter of `within` (15 minutes at `1h`, 6 hours at `1d`), so a
  steady fill is told a little less than `within` ahead: at `1h`, 55
  minutes ahead of a fill that takes two hours or more, 44 ahead of
  one that takes the hour. A single write, even one that takes a few
  minutes, fires only when it took about half of what was free. The
  rule cannot fire, nor start its `for`, until the store holds most of
  that quarter of history (10 minutes at `1h`, 5 hours at `1d`, 150
  hours at `30d`) after its setup, a wipe of its data, or a
  filesystem's first measurement; the store's own disk rule,
  `metrics_store_disk_fill`, waits 5 hours the same way. `disk_low`
  watches the disks meanwhile.
- **`post_deploy`**: after each deploy or rollback of the app, the new
  release compared with the one it replaced. It sends
  `post_deploy_regressed` once, and it never rolls back. Each minute
  of the `window` (30 m by default), the new release's responses since
  the cutover are checked, and `failures` (3) failing checks in a row
  send it. A check fails only when the new release is over
  `max_5xx_share` or `max_p95` and worse than the old one (a p95 above
  1.2 times the old one's; a share over the bound where the old one's
  was within it, or above 1.2 times it and beyond chance where both
  are over). With no baseline the bound alone counts: after a first
  deploy, when the old release's instance was not running at the
  cutover, when the old release sent fewer than `min_requests`
  responses, and for a p95 when it timed fewer. A check is skipped
  until the new release has sent `min_requests` (100) responses, and
  its p95 is judged only once it has timed that many (a cache hit is
  not timed), so on a quiet or well-cached app the window can end
  having judged nothing, which looks the same as a release that held.
  A deploy or rollback to the release already serving starts no
  comparison; a relaunch of a release whose instance had stopped (a
  rollback to one that crashed) is held to the bound alone, from 40 s
  after the cutover, and told as a relaunch. A restart of moteld inside
  the window ends the comparison, and nothing compares that deploy
  again; so does a reload whose config has no rule for the app, and a
  store that has not answered 10 minutes past the window's end
  (`ended unfinished` in the journal). A deploy or rollback to another
  release ends it and starts its own.

A `post_deploy` comparison that holds logs nothing at the default
level, Info, and no command shows that one ran. A regression's
`post_deploy_regressed` is sent and journaled (`monitor: event`);
`ended unfinished` is a journal warning, never sent. At Debug the
`monitor` logger says `monitor rules: the post-deploy comparison
ended with its window`, with the app and the release, when the
window ends (a window that judged nothing says it too), and
`monitor rules: a new deploy ends the post-deploy comparison in
flight` when a deploy does; its other ends log no such line. In the
Caddyfile's global options, this writes the `monitor` logger's lines
to the journal from Debug up:

```
log monitor_debug {
	level DEBUG
	include monitor
}
```

The request metrics need `observe <app>` in the app's site, and a rule
on them reads only the apps motel's last push says are observed: an
app whose `observe` line is taken out drops out at the next push, and
every app does while no push reaches the store for over 75 s, so an
alert firing then resolves. That is moteld down, or the store refusing
the pushes: `metrics_store_read_only` says so for a read-only store,
and `metrics_store_down` once they have failed for five minutes.
`request_rate` is over the time the store's samples cover, so a moteld
restart in the window barely moves it. It has no value until the
store holds a push from the five minutes before its window, and for an
app until that push observed it, so a `request_rate` rule fires no
sooner than five minutes and its `for` after setup, or after a gap of
over five minutes in the pushes, and for an app after its `observe`
line comes, or comes back after over five minutes out (its counts
start from zero then). An `observe` line out for less than five
minutes reads low by the gap's share until five minutes after it was
taken out, then has no value for as long as it was out, so an alert
firing for the app resolves and fires again only after its `for`.
`memory_share` and `cpu_cores` read the app's running unit alone. An
app that stopped is read for at most 75 s after its last push, which
vmalert sees about a minute later (`query_offset_seconds`, below), so
an alert on a stopped app ends up to about three and a half minutes
after the stop. A unit a deploy, a rollback or a restart replaced is
read by `cpu_cores` in the new unit's place, never beside it, until
the new one has a rate, which it has from its second push: within
about a minute of the cutover, and past the 75 s when that push is
lost, for as long as some unit of the app is pushed. Every app drops
out of them, and of `app_memory_high`, while no push reaches the store
for over 75 s, as for the request metrics. The
rule goes to the operator's channels, and to the app's bound channels
when it is about one app. Every parameter and every refusal is in
DESIGN-metrics.md's [Operator rules](../notify/DESIGN-metrics.md#operator-rules).

Editing a `threshold` or `disk_fill` rule can restart its alert.
vmalert knows a rule by its expression, name and labels, so a new
metric, comparison, value, `within`, `min_requests`, `app`,
`severity` or name makes it a new rule. An alert the old one was
firing is no longer sent, and the new one fires only after its `for`
(a `disk_fill`'s is set by its `within`; `moteld monitor rules --json`
gives it as `for_seconds`), so your channels can get a `[RESOLVED]`
while the condition still holds, then a new `[FIRING]`.

## How close is each alert

`moteld monitor rules`, as root, lists every alert motel can raise on
this box: motel's own checks, the store's rules and your `rule` lines.
For each it shows what it fires on and, for a check motel makes itself,
the number it last read:

```
$ moteld monitor rules
NAME                     SOURCE       KIND       SEVERITY  SCOPE      CONDITION                                                            NOW
app_memory_high          builtin      threshold  warning   every app  > 0.9 share for 10m                                                  (the store)
backup_stale             motel check  threshold  critical  box        > 3h                                                                 blog 58m, shop 3h59m30s
disk_low                 motel check  threshold  warning   box        >= 80% warning, >= 90% critical, each clears 5 points under          /srv/motel 79.9% (warning), /var/lib/caddy 41.2%
errors_high              operator     guarded    warning   every app  > 0.05 share for 10m, 2 guards                                       (the store)
watchdog_throttled       motel check  threshold  critical  every app  a further restart needed while >= its budget in its window is spent  shop 2 of 5 in 10m
…
```

(The shape is a test's, not a box's; the full table has every alert,
deploys and other events among them, whose condition is `-`.)

`KIND` is `threshold`, `guarded` (a threshold that also needs its
guards: a share or a latency needs at least `min_requests` responses
in five minutes, and below that it cannot fire whatever the share is) or
`event` (a deploy, a failure, a state: nothing to be close to).
Without the `metrics` line only motel's own checks are listed.

With `--json` each store rule comes with the MetricsQL of its value,
`value_query`, beside `op`, `threshold`, `unit`, `for_seconds` and
`guards`; you run it on the store's socket yourself. vmalert evaluates
data up to `query_offset_seconds` (65 s) old, so query at that offset
to see about what it compared:

```sh
q=$(moteld monitor rules --json | jq -r '.alerts[] | select(.name=="app_memory_high") | .value_query')
curl -s --unix-socket /run/motel-metrics/vm.sock -G --data-urlencode "query=$q" \
  --data-urlencode "time=$(( $(date +%s) - 65 ))" http://vm/api/v1/query
```

(`jq` is not part of motel; `apt install jq`, or copy the query by
hand.) Whether an alert is pending or firing, and since when, is in the
store too: `last_over_time(ALERTS[2m])` and
`last_over_time(ALERTS_FOR_STATE[2m])` (the listing's `state_query`
and `active_since_query`; the value of the second is when it became
pending, so pending turns firing at that plus `for_seconds`, about a
minute later in practice). Look back two minutes, not less: vmalert
writes both once a minute, and a shorter lookback misses them now and
then. The lookback has a price: an alert that resolved within it still
shows, and at the minute it turns firing both its `pending` and its
`firing` series show; take firing over pending, and the newest sample
(`tlast_over_time`) over an older one.

The listing is the loaded config's, and vmalert evaluates
`/var/lib/motel-metrics/rules.yml`, which motel writes at each load. A
load whose write failed (`EIO`, a read-only `/var/lib`: the journal
has `monitor metrics: the rules were not written`), or a reload that
took the `metrics` line out and could not empty the file (`… its rules
were not retired: vmalert still evaluates them`), still succeeds, so
the store's rules listed would not be the ones vmalert runs. The table
then ends with a line that says so, and never what the file holds:

```
WARNING: /var/lib/motel-metrics/rules.yml, the file vmalert reads, is not this config's render: the store's rules listed above are not the ones in it (the journal says why).
```

With `--json` it is `rules_file`: `differs`, `missing` (a `metrics`
line and no file: `moteld monitor setup --metrics` makes it) or
`unreadable`; absent when the file is this config's, or, without the
`metrics` line, the empty one this moteld's retire leaves, or none. The
command still exits 0: test `rules_file` in a script. Once the cause is
mended, `systemctl reload moteld` writes the file again for a config
with the `metrics` line; without it a reload writes nothing there, and
`systemctl restart moteld` empties it. The line speaks for the file
alone: vmalert re-reads it within 5 s of a write, and whether
vmalert accepted it is not checked.

The listing holds motel's own query text, rule, app and path names:
no channel URL, no secret, no summary. Another user than root is told
`the admin socket … admits root and the motel user: run moteld monitor
rules as root`. What each field means is in DESIGN-metrics.md's
[How close each alert is](../notify/DESIGN-metrics.md#how-close-each-alert-is).

## Turn it off, and remove it

A reload that takes the block out gives Alertmanager the config that
sends nothing (setup's own), removes the channel URLs motel copied for
it (`/var/lib/motel-notify/url-*`) and the states your producers sent
(`external.json`), and retires vmalert's rules. What
Alertmanager still holds then ends with no message, rather than as a
`[RESOLVED]` that did not happen; the journal says
`monitor: no loaded config has the monitor block's channels: Alertmanager was given the config that sends nothing …`.
A restart or a boot into the Caddyfile without the block does the same
(cmd/moteld's gate at `moteld run`), saying `monitor: this config has no
monitor block with channels: Alertmanager was given the config that
sends nothing …`. As root:

```sh
# 1. Pause the heartbeat's check off the box: from here on the pings stop.
# 2. Take the monitor block out of the Caddyfile (with a box repo: push
#    it without the block and without secrets/_box/), then:
systemctl reload moteld
# 3. Stop Alertmanager. A connection to either of its sockets starts it
#    again, so they go in one command; on a box set up with --metrics,
#    vmalert and the socket in the store's namespace with them (that
#    socket's stop alone stops Alertmanager, and Alertmanager's start or
#    vmalert's starts it again):
systemctl disable --now motel-vmalert.service motel-am-metrics.socket motel-am.socket prometheus-alertmanager.service   # --metrics
systemctl disable --now motel-am.socket prometheus-alertmanager.service   # without --metrics
# 4. The rest of the store, if setup --metrics ran:
systemctl disable --now motel-vm.socket victoria-metrics.service motel-metrics-netns.service
systemctl stop motel-vm.service
```

To stop here is to turn it off. The packages stay installed, and so do
the units, the config that sends nothing and the stored numbers. To
turn it back on, put the URLs back (with a box repo,
`git checkout <the commit before step 2's> -- <host>/secrets/_box`;
without one, `_box.env` is still in place) and the block with them,
then run `moteld monitor setup [--metrics]` again, which enables
everything again. A push of the block without `secrets/_box/` is
refused, since its channels name secrets the push does not carry.

To remove it, carry on:

```sh
# 5. The packages, and what they keep:
apt purge prometheus-alertmanager victoria-metrics
# 6. What setup wrote, which no package owns:
rm -f /etc/systemd/system/motel-am.socket /etc/systemd/system/motel-metrics-netns.service \
  /etc/systemd/system/motel-vm.socket /etc/systemd/system/motel-vm.service \
  /etc/systemd/system/motel-am-metrics.socket /etc/systemd/system/motel-vmalert.service
rm -rf /etc/systemd/system/prometheus-alertmanager.service.d /etc/systemd/system/victoria-metrics.service.d
systemctl daemon-reload
systemctl reset-failed
# 7. motel's state for them:
rm -rf /var/lib/motel-notify /var/lib/motel-metrics /run/motel-notify /run/motel-metrics
# 8. The URLs themselves: delete /etc/motel/secrets/_box.env (with a box
#    repo, the push without secrets/_box/ in step 2 already removed it).
rm -f /etc/motel/secrets/_box.env
# 9. This page's recipes, if you installed them: they fail once there is
#    no block to send to. Take OnFailure=motel-send-failed@%n.service
#    out of each job that has it too.
systemctl disable --now motel-reboot-required.timer
rm -f /etc/systemd/system/motel-reboot-required.timer /etc/systemd/system/motel-reboot-required.service \
  /etc/systemd/system/motel-send-failed@.service
systemctl daemon-reload
```

`apt purge` removes `/var/lib/prometheus`, `/var/lib/victoria-metrics`
(the stored numbers), `/etc/prometheus` and both files in
`/etc/default`. It does not remove what setup wrote: the six units
and the three drop-ins in step 6, and the state in step 7. It also
leaves the `prometheus` and `_victoria-metrics` system users and their
groups, which `userdel prometheus` and `userdel _victoria-metrics`
remove. Purging before steps 3 and 4 leaves five of setup's units
enabled with no package behind them (both of Alertmanager's sockets
and the store's, the namespace and vmalert), and the next connection to
Alertmanager's socket fails it. If you purged first, run the
`systemctl disable --now` lines of steps 3 and 4 anyway. systemctl
names the two packages' units as missing and exits 5, and still stops
and disables the rest. Then do steps 6 and 7.

From a `moteld` release there is no going back to a release without
monitor: each one (v0.6.1 and earlier) is from before the package was
`moteld`, and no `.deb` goes back across that move
([Upgrading](upgrading.md)).

## What has been verified where

| What | Where | Result |
|---|---|---|
| Every command and message on this page | motel's metrics e2e box (`make e2e-metrics`'s image), Debian 13, at 01b2fd5, 2026-11-12 | as shown |
| Setup from nothing, with Debian's packages downloaded | the same box, after `apt purge` | as shown |
| The false recovery after the block alone was removed, before the reload gave Alertmanager the config that sends nothing | the same box at 01b2fd5: block out at 17:52:17, `[RESOLVED]` for a firing `disk_low` and `reboot_required` at 17:56:57–17:57:03, the disk still full | what step 2's reload now prevents |
| The block taken out by a reload, a state firing | motel's notify e2e box (`make e2e-notify`, section 14), Debian 13 | the state directory holds the config that sends nothing and no URL file; the alert's end, posted by root at once rather than awaited, reaches no channel |
| `apt purge` with nothing disabled first | the same box. Its `policy-rc.d` refuses the packages' own stop at removal, as Docker images do, so a real box's prerm may stop more | five units left enabled, as above |
| The removal, with Alertmanager stopped before the block went (this page's order before the reload sent nothing), and `apt purge` first, where the packages' own stops run | the same image at c979404 with `policy-rc.d` removed, 2026-11-12 | steps 2 to 9 leave nothing but the two system users. Purged first, the same five units stay enabled, both unix sockets fail at their next connection, and steps 2, 4, 6 and 7 clear them |
| `moteld monitor silence add` and `expire`, and a one-shot under the silence reaching no channel | motel's notify e2e box (`make e2e-notify`, section 7a), Debian 13, 2026-11-13 | as shown; not tried on a real box |
| The laptop's unix-socket forward | OpenSSH 10.0p2, ssh to an sshd on localhost in a Debian 13 container, 2026-11-13 | the socket `srw-------`, another user refused, a stale file failing the forward without `StreamLocalBindUnlink=yes`; not tried against a real box |
| Real Slack, Discord and ntfy servers | not tried. On hotserve-1, an ntfy topic showed the JSON body as the message | owed (DESIGN-notify.md, "Owed") |
| The heartbeat's pings | the same image at c979404, at the stub receiver | 299 s and 300 s apart undisturbed; 439 s across a restart of moteld 140 s after a ping |
| A `--state firing` across a restart of moteld | motel's notify e2e box (`make e2e-notify`, section 12), Debian 13, 2026-11-14 | kept in `external.json`, told again at the start (`"standing":true`), and found by the `--state resolved` after it. Before motel kept it (the metrics box at c979404): `[RESOLVED]` five minutes after the restart, nothing having resolved it |
| A reboot with monitor set up | hotserve-1 (Debian 13, at 9228935), three cold boots; again at a8038b4, with Alertmanager's own socket in the store's namespace, 2026-11-13 | the store and its sockets come back. At a8038b4 every unit of the stack is up within about a second of `sysinit.target`, Alertmanager starts once, by socket activation, nothing is started by hand and nothing fails; the heartbeat's next ping comes 6 min 50 s after its last before the reboot, the downtime included |
| Setup's output with Alertmanager's socket in the store's namespace (first run, second run, upgrade from the proxy), and step 2's `--metrics` line | the metrics e2e box, 2026-11-12, with the change that replaced the proxy; the upgrade from the proxy again on hotserve-1 at a8038b4, 2026-11-13 | as shown; after step 2, Alertmanager, both its sockets and vmalert stay down for 70 s while the store scrapes 9093 and motel reads `am.sock`. On hotserve-1 the upgrade took 6 s, with the site answering throughout; its cold boot is the row above |
| What the store keeps of its own scrapes, and the store's and vmalert's pages read live | the metrics e2e box at 59df185, 2026-11-13 (metrics 4) | of `victoria-metrics` `up` and `vm_free_disk_space_bytes`, of `vmalert` `up`, of `alertmanager` `up` and `alertmanager_notifications_failed_total`, each with `scrape_*`; Alertmanager's page is the store's scrape of it. Since then `scrape.yml` keeps `alertmanager_config_last_reload_success_timestamp_seconds` too (the store's `ChannelFailed` reads it), which e2e metrics 4 checks |
| The branch on a real box: the install and setup's `--metrics` re-run, `moteld monitor test`, a `--state firing` across `systemctl restart moteld` and its `--state resolved`, and three restarts under a `request_rate` rule each way | hotserve-1 (Debian 13), at bf6f88a, 2026-11-14 | the re-run said `wrote` for the four files that changed and `…: content unchanged` for the other four, `restarted the store and vmalert`, and left Alertmanager as it ran, in 2.31 s with no push failed; each test delivered in 0.3–0.6 s, `ch-ops`'s `alertmanager_notifications_total` rising by 2 for it; the state was kept in `external.json` and told again (`"standing":true`), with no notification counted for the 426 s watched; five seconds after the resolve the file held it as `"firing":false`, the `resolved` line came about 33 s after the resolve, the alert was gone from Alertmanager within 65 s, and `state-info-ops` counted two notifications in all; `request_rate` fell 0.13 to 0.28 % at each restart, and neither rule (about 5 % under it and 5 % over) went pending |
