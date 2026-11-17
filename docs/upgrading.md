# Upgrading motel

A release is a new `.deb`; installing it over the old one is the
upgrade. Everything on this page runs as root on the box
(`ssh root@box.example.com`). This fetches the `.deb` for the box's
architecture from the latest of the
[releases](https://github.com/smallhoursorg/motel/releases), beside
the one you are upgrading from, checks it, and installs it:

```sh
v=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/smallhoursorg/motel/releases/latest)
v=${v##*/v}                         # the latest release's tag, without the v
arch=$(dpkg --print-architecture)   # amd64 or arm64
base=https://github.com/smallhoursorg/motel/releases/download/v$v
cd /var/local/motel              # next to the release you are upgrading from
curl -fsSLO "$base/motel_${v}_${arch}.deb" && curl -fsSLO "$base/checksums.txt" &&
  sha256sum -c --ignore-missing checksums.txt &&
  apt install ./motel_${v}_${arch}.deb
```

The `&&`s stop the block at the first step that fails. Without them, a
`checksums.txt` that did not download would leave the last release's
in place, which checks the old `.deb`, passes, and lets the new one
install unchecked.

<details>
<summary><b>To upgrade to a release candidate</b></summary>

A release candidate's `.deb` has a dot where its tag has a hyphen —
`motel_X.Y.Z.rcN_arm64.deb` under `vX.Y.Z-rcN` — so the block above
does not fit it as it stands. This one does, with `<tag>` replaced by
the tag the maintainer announced, without its `v`:

```sh
v='<tag>'                           # the announced tag, without the v
arch=$(dpkg --print-architecture)   # amd64 or arm64
deb=motel_$(echo "$v" | tr - .)_${arch}.deb
base=https://github.com/smallhoursorg/motel/releases/download/v$v
cd /var/local/motel              # next to the release you are upgrading from
curl -fsSLO "$base/$deb" && curl -fsSLO "$base/checksums.txt" &&
  sha256sum -c --ignore-missing checksums.txt &&
  apt install "./$deb"
```

A candidate sorts below the release it leads to, so if `apt` says it
will downgrade motel, answer `n` and stop here: the box already runs
a version that sorts above the candidate.

The box's repository then needs its Actions variable
`MOTEL_VERSION` set to the same `<tag>`: its workflow
checks with the latest stable release unless told otherwise, and a
release candidate is never that. On the laptop:
`gh variable set MOTEL_VERSION -R your-org/boxes --body <tag>`.
Once the release is out, upgrade to it with the first block on this
page, and delete the variable so the workflow follows the latest
stable release again: `gh variable delete MOTEL_VERSION -R your-org/boxes`.

</details>

The package keeps your `/etc/motel/Caddyfile` and restarts motel
into the new binary. An upgrade never asks about that file. The
package installs its starter config there only when nothing is there,
and replaces it only while it is byte-for-byte the starter the package
itself last wrote. Any other file — one you edited, or one `motel
init` or a push wrote — is never touched, and neither is a symlink.
The test is the bytes, not who wrote them: a copy of the starter you
put there yourself counts as the starter. A fresh copy of the starter
is always at `/usr/share/motel/Caddyfile`. Your apps are not restarted: they run under
motel's user manager rather than inside motel, so the new process
finds each one in `state.json` and picks it up as it is — same process,
no relaunch, no redeploy.

**The restart itself is not zero-downtime.** New visitors get errors
for about a tenth of a second, or longer while a slow request finishes:
the old process gets up to 5 seconds to stop before the new one starts.
So upgrade at a quiet moment. A
config change does not need a restart: a reload — what an applied push
from the box's repository does, or `systemctl reload motel` after a
hand edit — swaps the config inside the running process, drops no requests, and
never restarts a running app (an edited app definition applies the
next time the app starts: its next deploy or rollback, or a relaunch
after a crash, a sustained health failure, or a reboot).

<details>
<summary><b>What visitors see during the restart</b> — and how to pick a quiet moment</summary>

Deploys are zero-downtime; upgrading motel is not, because the
process listening on 80 and 443 is the one being replaced. On stop it
closes its listeners at once and lets the requests already in flight
finish, and the new process cannot listen until the old one has
exited. New visitors get connection refused for that whole window
(or, as the old process stops, a connection closed with no answer),
sometimes followed by a few dozen milliseconds of 503s while the new
process picks the apps back up. A deploy sent in those first moments
waits for motel to load its app, at most 5 seconds; past that it is
answered 503 with `Retry-After: 1`, and the deploy script sends it once
more. That holds for a token a `local` key signed, and for an issuer's
token whose signing keys the new process has already fetched. It
fetches them in the background as it starts and no token waits on that,
so the first issuer's token after a restart (a GitHub Actions deploy's)
can still be answered 401 once; the script sends it again 3 seconds
later, which goes through once the keys are in.
That is this release's script. A `scripts/deploy.sh` copied from a
template of v0.4.0 to v0.7.0 sends a request once more only after a
401, and one from an earlier template never, so after the 503 that
run fails. `diff` the copy against this release's
`examples/node/scripts/deploy.sh` (or `examples/deno/`'s): with no
change of your own in it, copy the template's over it; with some,
carry across `boxstarting()` and the `elif` branch of `send()` that
calls it, or, in a copy with no `send()` (before v0.4.0), make your
changes again in the template's.
A crash has a window of its own: whatever was in flight is cut off,
systemd waits a second
(`RestartSec=1s`) and starts motel again, and the apps are picked
back up the same way — a little over a second, plus the start itself.
How long a planned restart's window lasts:

- **With only short requests in flight:** about a tenth of a second
  (measured on Debian 13 in the package test container, plain HTTP).
- **With a slow one in flight** — a large download, a long poll — as
  long as that request takes, because the old process waits for it.
- **The old process gets at most 5 seconds to stop** (`TimeoutStopSec=5s`
  in the unit). systemd then kills it, which cuts off whatever was still
  running, and logs `Failed with result 'timeout'`; the restart still
  goes ahead, and the apps are untouched. The new process starts after
  that, so the window is those 5 seconds plus its startup (5.3 seconds
  in all, in the test container) — and a new process that cannot start
  is not retried by systemd (it retries a crash, never a refused
  start): the site stays down until you fix the config or go back,
  which is what the config check below is for.

This is how Caddy itself upgrades: its official Debian package restarts
the service the same way, with the same 5-second stop timeout, and the
shutdown is Caddy's own. The 503s are the one part that is motel's —
the moment before liveswap has picked your apps back up.

To check for a quiet moment, add `metrics` to the Caddyfile's global
options (a push from the box's repository, which reloads), and read
how many requests are in flight:

```sh
curl -fsS --unix-socket /run/motel/admin.sock http://localhost/metrics \
  | grep '^caddy_http_requests_in_flight'
```

Every line at 0 means nothing is mid-request. No lines means the check
is not working yet, not that the server is idle: `metrics` is not on,
or nothing has been served since the reload (load a page, then look
again). It is only a snapshot, so a request can still start the moment
after, but it catches the slow downloads that would stretch the window.

</details>

Before you upgrade:

- **Check the new version accepts your config.** The `.tar.gz` on the
  release page holds the bare binary, and
  `./motel validate --config /etc/motel/Caddyfile` runs its
  config check without touching anything that is running. A config it
  rejects would stop the upgraded motel from starting, and the site
  would stay down until you fixed the config or went back (systemd
  retries a crash, not a refusal). A build with a `.deb` and no
  `.tar.gz`, such as one of a feature branch, has no `checksums.txt`
  either. Take its sha256 where it was built
  (`sha256sum motel_<version>_<arch>.deb`, `<arch>` what
  `dpkg --print-architecture` prints on the box), copy it to
  `/var/local/motel`, and check it there before anything else:
  `echo '<that sha256>  motel_<version>_<arch>.deb' | sha256sum -c`
  must say `OK`. Its config is checked the same way, from the `.deb`,
  unpacked and not installed, into a directory that is not there yet
  (`dpkg-deb -x` adds to one an earlier upgrade left, so
  `rm -rf /root/motel-new` first):
  `dpkg-deb -x motel_<version>_$(dpkg --print-architecture).deb /root/motel-new`,
  then `/root/motel-new/usr/bin/motel validate --config /etc/motel/Caddyfile`.
  It installs from `/var/local/motel` with
  `apt install ./motel_<version>_<arch>.deb`. dpkg sorts a `~` below
  everything, the version's end included, and two builds of one
  version by what follows it, so `0.7.1~clientapp2` is an
  upgrade over `0.7.0` and `0.7.1~clientapp1`, and a downgrade over
  `0.7.1` and `0.7.1~rc1`. apt asks before a downgrade unless given
  `--allow-downgrades`: answer `n` and stop, since taking a box below
  what it runs is a decision of its own; if it is the one you mean,
  the bullet on the way back below installs with that flag. The way
  back is the `.deb` the box was installed from, whose version
  `dpkg-query -W -f='${Version}\n' motel` prints before the install (a
  candidate's file has a `.` where that has a `~`:
  `motel_0.7.1.rc1_<arch>.deb`): keep it in `/var/local/motel`, and the
  bullet on it below puts it back.
  To see why an upgraded motel will not start, read `journalctl -u motel`
  and run `motel validate --config /etc/motel/Caddyfile`; do not run
  `motel run` by hand on the box: while motel runs, a second one takes
  its admin socket and can disturb its apps.
  motel refuses root's run only when the Caddyfile's liveswap root
  exists and is not root's, as on a packaged box (it is motel's);
  otherwise nothing stops it, so do not rely on the refusal.
- **Check motel.service runs as the motel user.** That refusal cannot
  tell root's run by hand from a unit edited to run motel as root, and
  the install makes `/var/lib/liveswap` motel's again. So such a box,
  with the liveswap root the package makes, is refused at every start,
  the install's own included, and serves nothing until someone at the
  console mends it. `systemctl show -p User --value motel` prints
  `motel` on a packaged box; anything else (`root`, `0` or nothing at
  all: to run as root) means the unit was edited. `systemctl cat motel`
  shows what is there: each file in play under a `# <path>` line, the
  packaged unit `/usr/lib/systemd/system/motel.service` and the
  package's own drop-in `motel.service.d/10-user-manager.conf` among
  them. Then, just before the install:
  - **The `show` prints `motel`, and nothing of yours is there but
    drop-ins in `/etc/systemd/system/motel.service.d/`:** nothing to do.
  - **A full copy of the unit, `/etc/systemd/system/motel.service`, is
    there**, whatever its `User=`: it hides every later release's
    changes to the unit. `rm /etc/systemd/system/motel.service` removes
    it and leaves your drop-ins.
  - **A drop-in in `motel.service.d/` sets `User=` or `Group=`** to
    anything but `motel`: set both to `motel` in it, or delete it.

  `systemctl revert motel` does the last two at once, and takes more
  with them: it deletes `motel.service.d/` and every file in it under
  `/etc/systemd/system` and `/run/systemd/system` (every drop-in of
  yours, and `10-user-manager.conf`, which the install writes back) and
  any copy of the unit there, and unmasks the unit; the packaged unit
  stays. After any of these, run `systemctl daemon-reload`; the `show`
  must then print `motel`, and `systemctl is-enabled motel` `enabled`.
  A drop-in in `/etc/systemd/system/service.d/`, which every service
  reads, can set `User=` too; `systemctl cat` shows it, and the revert
  leaves it. A box that ran motel as root may also
  hold files of root's where motel writes, Caddy's certificates in
  `/var/lib/motel/caddy` among them: before the install,
  `find /var/lib/motel /var/lib/liveswap -user root` lists them, and
  each one motel wrote wants `chown motel:motel`.
- **A box with an ECDSA signer rotates it first** (upgrading from
  v0.6.0 or earlier). A `signer` line with an `ecdsa-sha2-nistp256`,
  `-384`, `-521` or `sk-ecdsa-sha2-nistp256@openssh.com` key is refused
  from this release on, since an ECDSA signature has two forms that
  verify alike and one signed commit would have two ids; the
  `validate` above names such a line. While the box still runs the old
  release: that signer makes an ed25519 key (`ssh-keygen -t ed25519`,
  or `-t ed25519-sk` for a hardware key), `make signer` adds it, one
  push applies, and a second push removes the old line. Upgrading
  first instead stops the site: the install restarts motel onto the
  new binary, which refuses to start on a Caddyfile with that line,
  naming it, until root edits the line out of `/etc/motel/Caddyfile`
  at the console and runs `systemctl start motel` (the next push then
  reports the file as edited out of band and applies over it).
- **The `.deb` you are upgrading from is still in `/var/local/motel`.**
  `apt install --allow-downgrades ./motel_<old>_$(dpkg --print-architecture).deb`
  puts it back — the same restart, in reverse, and nothing to download
  first. On a VPS, a snapshot taken first is the fuller way back.
- **Going back to v0.6.1 or earlier from a release with monitor**
  needs the Caddyfile emptied of it first. Those releases know none of
  monitor's two pieces: neither the `monitor` block (its `metrics` and
  `rule` lines go with it) nor the response observer's `observe <app>`
  lines in the sites. They refuse the file, and `validate` with 0.6.1's
  binary says `unrecognized global option: monitor` or
  `unrecognized directive: observe`. The install's restart then fails
  (`motel: restarting onto the new version FAILED; motel.service is now
  failed.`) and nothing is served until the file is mended. Their box
  channel also refuses an `applied.json` that lists the box's own
  `secrets/_box/`, so every push after the downgrade would fail before
  it began. While the box still runs the newer release:
  1. **Pause the heartbeat's check, then stop Alertmanager**, as
     [Monitor](monitor.md#turn-it-off-and-remove-it)'s steps 1 and 3
     say (on a box set up with `--metrics`, vmalert and Alertmanager's
     socket in the store's namespace with it). The older release
     neither uses nor stops them.
  2. **Take out the `monitor` block and every `observe` line.** On a box
     that takes pushes, push a commit without them and without
     `secrets/_box/`. It removes `/etc/motel/secrets/_box.env` and takes
     `_box` out of `applied.json`. That holds for a block edited in by
     hand, too: the push replaces the file. On a box with no push
     channel, root edits them out of `/etc/motel/Caddyfile` and runs
     `systemctl reload motel`. 0.6.1 starts with `_box.env` still in
     place and reads nothing from it; delete it if you are not coming
     back.
  3. **Go back** with the `.deb`, as above.

  The store keeps running after this, and so do Alertmanager and
  vmalert if you skip step 1, as do any of Monitor's recipe units you
  installed (they fail once `motel monitor send` is gone). A reboot
  starts vmalert again with the store, whose drop-in starts it
  (`ExecStartPost=`), though step 1 disabled it; Alertmanager and its socket in the store's
  namespace stay down, so it reaches no Alertmanager and nothing is
  sent. They are
  setup's or yours, not the package's, and the older release neither
  uses nor stops them. [Monitor](monitor.md#turn-it-off-and-remove-it)
  stops and removes them. Coming back to a release with monitor later
  means its setup again: step 1 left Alertmanager disabled, so once
  the block and its URLs are back, root runs
  `motel monitor setup [--metrics]` ("To turn it back on", there). On
  a box already back whose motel refused to start, root edits
  the block and the `observe` lines out of `/etc/motel/Caddyfile` and
  runs `systemctl restart motel`. On a box that takes pushes, root also
  removes the `"_box"` entry from the `secrets` map in
  `/var/lib/motel-box/applied.json` and deletes
  `/etc/motel/secrets/_box.env`, and the next push (without the block,
  the `observe` lines and the directory) applies.
- **Going back to a release without people** (0.7.0, or an earlier one
  with a `box` block, 0.3.0 on) needs the Caddyfile emptied of them
  first. Those releases refuse a `box` block with a `person` or `grant`
  line: `validate` with the old binary says `unknown box subdirective
  "person"` (or `"grant"`), and the install's restart fails with nothing
  served until the file is mended. While the box still runs the newer
  release:
  1. **Take out every `person` and `grant` line.** On a box that takes
     pushes, push a commit without them. On a box with no push channel,
     root edits them out of `/etc/motel/Caddyfile` and runs `systemctl
     reload motel`. People's clients get the flat 401 from then on.
  2. **Go back** with the `.deb`, as above.

  `signer` lines are not affected: they mean what they meant, and the
  box channel works across the downgrade. On a box already back whose
  motel refused to start, root edits the lines out of
  `/etc/motel/Caddyfile` and runs `systemctl restart motel`; the next
  push, without them, applies.
- **Upgrade motel and the host separately.** motel measures what
  the host can sandbox at every start and refuses to start on a host
  that cannot deliver it ([liveswap/README.md](../liveswap/README.md#sandbox)),
  so a kernel update or reboot bundled with an upgrade leaves you
  guessing which change did it.

After an upgrade from v0.6.0 or earlier, one more step at a quiet
moment: the package now runs the motel user's systemd manager, which
starts every app, under `NoNewPrivileges` (a setuid binary on the host
cannot raise what runs under it to root), and a manager already running
keeps its old flags until it next starts. The install does not restart
it, since that stops every app at once. Either a reboot or

```sh
systemctl restart "user@$(id -u motel).service"
```

does it; each app comes back as motel's watchdog relaunches it, a few
seconds apart from the others. `grep NoNewPrivs /proc/$(systemctl show
-p MainPID --value "user@$(id -u motel).service")/status` shows `1`
once it has.

After an upgrade to v0.7.0 or later from a release between v0.3.1 and
v0.6.1 (both included), on a box that takes pushes: those releases
named, in each push's output, the variables of an env file in
`/etc/motel/secrets/` that the push replaced or removed when someone had made or edited it by hand, and a secret
pasted alone on a line of it (base64 ending in `=`) read as a
variable's name. If a push ever took over such a file, look through
the box repository's old apply runs (logs and job summaries) and
the applier's journal for a name after `removed` that is no
variable's, and give any value found that way a new one ([Change a
value](secrets.md#change-a-value)). What you are looking for may
hold a secret, so read the old runs only where they are, and never
paste a line of them or of the journal into a chat, an issue or a bug
report. The runs name every push's removals; the journal names them
only for a push whose result could not be written, so count before
you read it:
`journalctl -u motel-box-apply -u hotserve-box-apply | grep -c 'secrets_removed": *\["'`
(`hotserve-box-apply` is the applier's name before v0.6.0) prints 0
when the journal names no removal, which says nothing of the runs.
Read any lines it counts on the box itself. `make secrets-import` had the same
gap: copy this release's `examples/box/bin/secrets` and
`examples/box/bin/apply` into the box repository's `bin/` (the second
says a push left a hand-edited file's own variables unnamed), and a
name in `make secrets` that is a value is one to change too, since the
old name stays in the repository's history.

The box repository's workflow checks config with the latest stable
release, so it needs nothing after an upgrade — unless its
`MOTEL_VERSION` repository variable pins a release, which then
wants the new one. One made from the v0.6.0 or v0.6.1 template that
takes a `monitor` block also wants this release's `examples/box/bin/`
in its `bin/` ([Monitor](monitor.md)).

There is no APT repository yet, so nothing upgrades motel behind
your back; the hosted repository on the [roadmap](../README.md#roadmap) is what
will change that.

## A producer's state across the install

A box built from the notify-and-metrics feature branch before motel
kept the states your producers send (`motel monitor send --state
firing`; v0.6.1 and earlier have no monitor block) holds them
in memory only.
The install's restart loses them, so about five minutes later
Alertmanager tells their channels `[RESOLVED]`, although nothing
recovered. After the install, send each state that still fires once
more. From then on motel keeps them across a restart
([Monitor](monitor.md)).

## A receiver per channel: one repeat of what fires

A box built from the notify-and-metrics feature branch before each
channel had its own receivers (v0.6.1 and earlier have no
monitor block) gets
a new Alertmanager config at the first load after the install: every
alert keeps its channels, under new routes and receiver names. Nothing
is dropped, and nothing to do; but Alertmanager's notification log is
keyed by route and receiver, so it has no record of what the old
receivers sent: **each state still firing, and each one-shot sent in
the last five minutes, reaches its channels once more as `[FIRING]`**,
about 30 seconds after the restart. A recovery already told is not
told again, and the heartbeat, `ChannelFailed` and `motel monitor test`
keep their receivers. Going back to the older binary does the same
once in the other direction. What will repeat is what fires before
the install ([Monitor](monitor.md#what-is-firing-with-amtool) reads
it).

## The disk rules' new expressions: a resolve, then firing again

On a box built from the notify-and-metrics feature branch, the first
load after the install changes the expression of
`metrics_store_disk_fill` (it waits for 5 hours of the store's
history) and of each `disk_fill` rule whose `within` is under a day
(its windows and `for` are scaled to `within`). vmalert knows a rule
by its expression, so an alert of either that fires at the install is
no longer sent: its channels can get `[RESOLVED]` while the disk still
fills, then `[FIRING]` once the new rule's `for` has passed (30 m for
the store's, from 5 m for yours). Nothing to do.

## From a state_dir or alertmanager line

A box built from the notify-and-metrics feature branch (no release
has either line) may have a `state_dir` or an `alertmanager` line in
its `monitor` block, or in the older `notify` block. The new binary
refuses both, naming the line and the path it fixes, so the install's
restart onto a file that still has one stops every site. **Take the
line out before the install.** Setup only ever made
`/var/lib/motel-notify` and `/run/motel-notify/am.sock`, so a line
naming either changed nothing, and one naming another path never
reached Alertmanager. Push the file without it, or on a box with no
push channel edit it out and run `systemctl reload motel`: the old
binary takes it. Then upgrade. The new binary's `motel validate`,
from the `.deb` as above, names any line left. On a box with a
`notify` block, the next section's steps leave these lines out of
step 3's `monitor` block.

## From the notify and metrics_store blocks

A box built from the notify-and-metrics feature branch before the
`monitor` block (no release has either) runs a `notify` block and
perhaps a `metrics_store` block. The new binary refuses both, naming
`monitor`, and the old one refuses `monitor`, so no Caddyfile loads on
both: upgrading with the old blocks in place stops every site, since
the install restarts motel onto a binary that refuses to start on the
file. Do it in this order:

1. **Push the Caddyfile without the `notify` and `metrics_store`
   blocks**, and without `secrets/_box/`: the old box channel refuses
   that directory beside a file with no `notify` block, and the push
   removes `/etc/motel/secrets/_box.env` (the encrypted files stay in
   the repository's history). The old motel validates and reloads it.
   On a box with no push channel, root edits the blocks out of
   `/etc/motel/Caddyfile` and runs `systemctl reload motel`. So does a
   box that takes pushes but whose blocks were edited into the file by
   hand and never committed. Its next push replaces the file with the
   repository's, so before that push commit the `monitor` block, and
   with it each secret the block reads
   (`make secret APP=_box VAR=NOTIFY_<NAME>`): a `_box.env` written by
   hand does not count, and a push of the block without
   `secrets/_box/` is refused, naming the secret.
2. **Upgrade the package**, as above. The new motel starts on that
   Caddyfile, which has nothing for it to refuse.
3. **Push the Caddyfile with `monitor { … }`**: the notify block's
   lines unchanged under the new name (less any `state_dir` or
   `alertmanager` line, whose paths are fixed: the section above), and
   a `metrics` line inside it
   in place of the `metrics_store` block: `metrics` for a bare
   `metrics_store`, `metrics <path>` for one whose `store <path>` named
   a socket. Put `secrets/_box/` back, restored from the commit before
   step 1's (`git checkout <that commit> -- <host>/secrets/_box`), so no
   value is typed again. On a box with no push channel, root writes the
   block into the file, puts `_box.env` back if it went
   (root:motel 0640, [Monitor](monitor.md#the-secrets-file)), and runs
   `systemctl reload motel`.

The new binary can check step 3's file before you install it, from the
`.deb` as above (`/root/motel-new/usr/bin/motel validate --config
<the new file>`). It reads `_box.env` to check each secret, and on a
box that takes pushes step 1's push deletes that file, so run the
check before step 1. Writing the `monitor` block into the file before the
install, so that the install's restart reads it, would skip the gap
below. It is untested, and it leaves a file the running binary refuses
until the install: a reload in between fails, and a restart stops every
site. On hotserve-1 the three steps left 23 seconds with no monitor,
and the site answered throughout (2026-11-12).

**No alert is delivered between step 1's reload and step 3's.** motel
posts nothing to Alertmanager: no event, no `Watchdog`, so the
heartbeat stops pinging and the dead-man check pages once its own
period is out (pause it first, or expect it). The old binary's reload
at step 1 leaves Alertmanager the last config motel rendered, which
would end what was firing at its `resolve_timeout`, five minutes on,
with a recovery to the channels that did not happen. The install's
restart onto step 1's file prevents that: the new binary, starting a
config with no monitor block, gives Alertmanager the config that sends
nothing (`monitor: this config has no monitor block with channels: …`
on stderr, in the journal), so what was firing ends without a message,
unless more than five minutes pass between step 1 and the install.
Step 1's reload also retires the store's rules:
the reload to a config with no `metrics_store` block writes
`/var/lib/motel-metrics/rules.yml` with its groups empty
(`grep -c 'alert:'` on it says 0), so vmalert evaluates nothing, and
motel stops pushing, so the store keeps no motel series for the gap
(its own scrapes of itself, vmalert and Alertmanager go on). The
store, vmalert and Alertmanager keep running throughout: their units
are setup's. Step 3's load renders Alertmanager's config, writes the
rules again and resumes the push. The journal says the rules were
retired twice, in two spellings. The old binary says it at step 1's
reload (`metrics_store: the block is gone: vmalert is given no rule;
…`), and the new one says it again when the install restarts it onto
step 1's file (`monitor metrics: the metrics line is gone: vmalert is
given no rule; …`).

**Run setup again if the box was built before b7754fc** (#426,
2026-11-11): both setups on a box that had a `metrics_store` block,
and `motel monitor setup` alone on one that had only `notify`
(`--metrics` there would install the store and start it). Their units
have changed how they run since then: the two
socket proxies run as root until the store's units are rewritten
(`ps -eo user,cmd | grep socket-proxyd` shows `root`), and
Alertmanager's and the store's data directories stay 0755, readable by
every local user, until both drop-ins are rewritten. After step 3, on
a box with the store, root runs `motel monitor setup --metrics`. It
rewrites the store's units and
restarts vmalert and the store (a push may fail meanwhile). It
also retires the proxy that carried vmalert's posts to Alertmanager
(`retired motel-am-proxy.socket …`) and restarts Alertmanager once,
which loses what it holds in memory, so that Alertmanager holds that
listener in the store's namespace itself (`restarted Alertmanager,
once, …`). Then, at a quiet moment, root runs
`motel monitor setup`. It rewrites Alertmanager's units and restarts
it, which loses what it holds in memory: motel posts each firing state
alert again within a minute, and a one-shot waiting in a group is
lost. On a box built at b7754fc or later whose store still reaches
Alertmanager through a proxy (`/etc/systemd/system/motel-am-proxy.socket`
is there), `motel monitor setup --metrics` is due, as above, and
`motel monitor setup` is not. Going back to such a build later, root first runs `systemctl disable --now motel-am-metrics.socket`, removes it and `prometheus-alertmanager.service.d/20-motel-metrics.conf` from `/etc/systemd/system`, and runs `systemctl daemon-reload`: that build's `setup --metrics` cannot otherwise bind its proxy's 9093. Otherwise, on a box built at b7754fc or later, the units differ from the
old setups' in their comments, in the store's clocks (below), and in the `Description=` lines,
which name the old commands (`systemctl status` shows
`(motel notify setup)` until they are rewritten). Running both setups
again then renames them and sets the store's clocks. Either way, motel says when it is due:
from this version, every load (`motel validate`, `motel reload`, and
the restart onto the new binary, in the journal) warns `monitor: the
units motel monitor setup wrote differ from this motel's (…)` (for the store's, `monitor: the files motel monitor setup --metrics wrote differ …`) for as
long as a unit differs from the one this binary would write, comments
aside, and names the setup to run. While the proxy's units are there,
the store's warning says `the proxy an older setup wrote for
Alertmanager (…) is still in place` (after the files that differ, or
alone when none does), and that setup retires it and, unless
Alertmanager listens in the store's namespace already, restarts (or
starts) it once.

**One run of `motel monitor setup --metrics` covers every paragraph
below that fits a box**, however many do, and it is the same run as
the `--metrics` one above: each run writes all of its units and
`scrape.yml` as this motel ships them, and restarts the store and
vmalert. The warning each paragraph names is the store's (`the files
motel monitor setup --metrics wrote differ …`). None of them needs
`motel monitor setup` without `--metrics`, which is due only as the
paragraph above says, or while a load warns `monitor: the units motel
monitor setup wrote differ …`.
The store is down for part of the run, so a push that comes then
fails; motel pushes every 30 seconds, and hotserve-1's two re-runs
(2026-11-14) took under 2.5 seconds each, with no push failed.

**Run `motel monitor setup --metrics` again on a box whose store was set
up before the 5 s clocks** (the warning above names `10-motel.conf` and
`motel-vmalert.service`). Until it is, vmalert re-reads a new
`rules.yml` only once a minute, and the store keeps
VictoriaMetrics' 30 s `-search.latencyOffset`, so every alert and
`GET /<app>` waits the 30 s as before. Post-deploy checks are right
either way: they ask the store for the 5 s themselves. The re-run
rewrites the units and restarts the store and vmalert; a push may
fail meanwhile.

**Run `motel monitor setup --metrics` again on a box whose store was set
up before its restarts backed off** (each load's warning names
`10-motel.conf`). Until it is, a store that
fails at its start (a full disk, say) is restarted about 60 times a
second for as long as the fault lasts, about a fifth of a core and some
400 journal lines a second. The re-run writes the drop-in that backs
the restarts off from 1 s to a minute; it restarts the store and
vmalert, so a push may fail meanwhile.

**The same re-run trims what the store keeps of its own scrapes.** From
this version the store keeps, of its scrapes of itself, vmalert and
Alertmanager, only the series motel reads ([Monitor](monitor.md#promql-over-the-socket-as-root)):
about 80 where it kept over 2,000. Only setup writes
`/var/lib/motel-metrics/scrape.yml`, so a store set up before goes on
keeping every series, and each load's warning names `scrape.yml`, until
root runs `motel monitor setup --metrics` (the re-run above does it).
The store is restarted onto the new file; the series it no longer
keeps stop there and age out over the 14 days' retention. Nothing
motel runs reads them, so no rule, check or `GET /<app>` changes. A
query of your own over one of them finds its samples from before the
re-run until the retention ends them, and none after.

**Run `motel monitor setup --metrics` again on a box whose store was set
up before its socket outlived a failed start** (each load's warning
names `motel-vm.socket` and `motel-vm.service`). Until it is, one push
that comes while the store or its namespace fails to start ends
`/run/motel-metrics/vm.sock` for good (`motel-vm.socket` failed with
`trigger-limit-hit`), and the store stays down after the cause is gone
until root runs `systemctl start motel-vm.socket`; so do a few
pushes or reads a few seconds apart while the store keeps exiting
after its start. The re-run writes the socket unit's `FlushPending=yes`
and `TriggerLimitIntervalSec=0` and the proxy's `StartLimitIntervalSec=0`,
which its `daemon-reload` applies to the running units with no restart,
and its `enable --now` starts a socket that had already failed.

**Run `motel monitor setup --metrics` again on a box whose store was set
up before it kept Alertmanager's last config load** (each load's
warning names `scrape.yml`). Until it is, the store's `ChannelFailed`
reads the failed counter alone, and misses a failure counted just after
a config load that brings the counter back to the value the scrape
before the load read; motel's own `ChannelFailed` tells of it either
way. The re-run rewrites `scrape.yml` and restarts the store and
vmalert; a push may fail meanwhile.

## Patching the host

Apps run on the box's own `/usr`, so `apt upgrade` reaches an app at
its next launch — its next deploy or rollback, or a relaunch after a
crash, a sustained health failure, or a reboot — never the one
running, which keeps the runtime and libraries it has already loaded.
Upgrading motel does not relaunch apps either: it picks them up as
they are.

What a patch reaches depends on where the app's runtime came from:

- **`command node server.js` with Node from apt:** the new Node and the
  new system libraries.
- **The Deno under `/usr/local/bin`**, or **the Node example's
  executable**, which carries its own Node: the new system libraries
  only. A newer Deno is a new file there, and a newer Node a new build;
  either still waits for the next launch.

So after patching, relaunch each app. In its repository, Actions →
deploy → **Run workflow**, with the running version as the version to
roll back to: a rollback to the running version relaunches it from
the box's disk, through the same health gate and cutover as any
deploy, with no build. Or push a commit — an empty one is enough —
which deploys a new version the same way:

```sh
git commit --allow-empty -m "Relaunch after patching" && git push
```

Or reboot, which relaunches every app at once but takes the sites
down while it does.
