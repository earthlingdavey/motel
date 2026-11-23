# Giving a person access

**Experimental, since v0.8.0.** A person you name in the box's
Caddyfile can read, from their own laptop and over HTTPS, the status
and deploys of the apps you choose, and the box's own status, with
their own ssh key: no account on the box, no ssh access, and no deploy
token. With a `logs` grant they also read the apps' logs. They cannot
deploy, roll back or push, whatever you write. ssh stays root's.

This needs a `box` block and a site with `box_webhook` (the template's
deploy host), as a box with a box repository has. People reach the box
only on that site's name.

## What they need

An Ed25519 ssh key: `ssh-ed25519`, or its hardware form
`sk-ssh-ed25519@openssh.com`. That is the one kind a person may hold for
now. Not RSA, and not ECDSA yet (`ecdsa-sha2-*`,
`sk-ecdsa-sha2-nistp256@openssh.com`): a box refuses a `person` line
with either, so an agent that makes only P-256 keys, such as one that
keeps its keys in the Secure Enclave, cannot be used for now. A later
release may take ECDSA keys too. Someone without an Ed25519 key makes
one with `ssh-keygen -t ed25519`, or `ssh-keygen -t ed25519-sk` for a
hardware key. Ask them for the line `motel key
--name <their name>` prints: the `person` line for their key, ready to
paste (the `motel` command is the laptop client, experimental; without
it, the one line of their `.pub` file, with `person <name>` in front).
No release up to v0.8.0 carries the client (the
`motel_<version>_linux_<arch>.tar.gz` of v0.6.0 to v0.7.0 is the old
server): on those, they build it from a clone of this repository
(client/README.md, "Getting it").
Once you have pushed, `motel whoami` tells them who the box takes them
to be and what they may read from where they are. If their key lives in
an agent (a password manager's, say), `motel` reaches it through
`SSH_AUTH_SOCK` only, not through an `IdentityAgent` line in
`~/.ssh/config` (client/README.md, "A key an agent holds").

## Add them

In the `box` block of the box's Caddyfile, a `person` line for their key
and a `grant` line for what they may read. The name comes first on
both, so one search finds every line about a person:

```caddyfile
box {
	deploy_trust github { … }
	signer alice@example.com ssh-ed25519 AAAAC3Nz…

	person bob@example.com ssh-ed25519 AAAAC3Nz…
	grant bob@example.com viewer app blog
	grant bob@example.com logs app blog from 198.51.100.0/24
	grant alice@example.com viewer box
}
```

- `grant <name> viewer app <app>` lets them read that app: its status,
  its deploys, its alerts, the rules that apply to it with its
  numbers, and its numbers over time. `grant <name> viewer box` lets them read every app,
  the box's own status, what the box says of its backups, every
  alert and silence the box's Alertmanager holds, and every rule the box
  alerts on with every number. On a box at v0.8.0 either grant reads
  status and deploys alone: the alerts, rules, numbers over time and
  backups are on each release after it ([below](#what-they-reach)).
  `viewer` never reads logs, on an app or on the box.
- `grant <name> logs app <app>` lets them read everything `viewer`
  reads on that app, and its logs; `grant <name> logs box` everything
  `viewer` reads on the box, and every app's logs. Read [what a logs
  reader reads](#what-a-logs-reader-reads) before you give it. Where
  these pages say "a viewer of the box" or "of the app", a person with
  a `logs` grant there is one too.
- A `signer` line's ed25519 key is already that signer's key as a
  person: `alice` above needs no `person` line, only a `grant`. Once
  the block names anyone in a `person` or `grant` line, every signer
  with an ed25519 key can ask who they are here, even with no grant,
  and is told an empty list of apps.
- A second key for the same person is a second `person` line with the
  same name.
- A person holds at most one grant of each role on each scope; a
  `viewer` and a `logs` grant on the same app (or both on the box) are
  two lines, each with its own `from` list or none.
- `from` limits a grant to the addresses you list, last on the line:
  `grant bob@example.com viewer app blog from 198.51.100.0/24
  2001:db8:5::/48`. If every grant they hold has a `from` list, from
  anywhere else their key gets the same refusal as a stranger's; with
  a grant beside it that has none, they are still let in, and told
  `403` for what only the limited grant covers. bob above reads blog's
  status from anywhere, and its logs only from 198.51.100.0/24.
- A `from` list is as trustworthy as whatever tells Caddy the
  client's address. Behind a proxy of yours, set `trusted_proxies` (or
  `trusted_proxies_unix`) to it **and** `trusted_proxies_strict` in
  the global `servers` options: without `trusted_proxies_strict`
  Caddy takes the address from the left of `X-Forwarded-For`, which
  the client writes, so `moteld validate` refuses a `from` list and the
  box matches no list for a forwarded request. With it, the list holds
  only if your proxy sets the address header itself (also any other
  you name in `client_ip_headers`, such as `X-Real-IP`) rather than
  passing on the client's; and on a listener with `proxy_protocol`,
  the address is whatever the PROXY header says, so it is only as good
  as that listener's `allow` list. `moteld validate` warns of both.

Check the file, then push it as any change to the box (a signed
commit). `make check`, at the top of the box's repository, checks every
box's file; it needs a Linux `moteld`, and on a Mac the pull request's
check runs it instead. Off the box, a bare `moteld validate` refuses a
file whose `monitor` block reads a `{secret.…}`, since those values are
on the box alone; `make check` tells it their names. To see the people
lines as the box reads them, on Linux:

```sh
moteld box describe deploy.example.com/Caddyfile
```

`describe` prints a `person` line per key with its fingerprint, which
is what they will see as their key, and a `grant` line per grant. A
`warning` line names anything taken with a doubt: a person with no
grant, a grant that another grant of theirs already gives from
everywhere (a `viewer` app grant beside a `viewer` box grant, a `viewer`
grant beside a `logs` grant with no `from` list on the same app or on
the box, a `logs` app grant beside a `logs` box grant), people with no
monitor channel to tell you when their key is used from somewhere new
([below](#when-their-key-is-used-from-somewhere-new)). A covering
grant with a `from` list is not warned of: outside its list, the other
grant is all they hold.

On a box whose Caddyfile you edit at the console and do not push, the
file must still be one the box can read its people from, as the
template's is: a `box` block, exactly one site with `box_webhook`,
under one bare host name, and no `import` anywhere in the file (the
journal's line at each load says why when it is not). For people to
reach more than the box's status, that same site needs
`liveswap_webhook` too, as the template's has. Edit
`/etc/motel/Caddyfile` as root,
`moteld validate --config /etc/motel/Caddyfile`, and `systemctl reload
moteld`. The box reads who it admits from that file at each load; the
journal says `box: people admitted from the Caddyfile this machine
runs (/etc/motel/Caddyfile)`, or why no one is.

### What a logs reader reads

A `logs` grant reads far more than a `viewer` grant. An app's logs
are what it writes to its output, as the box's journal
holds them, and they hold whatever the app prints: your customers'
e-mail addresses, names and orders, the addresses they came from, and
secrets the app made or fetched while it ran (a session id, a reset
link, a token from another service). The box masks the values in the
app's `env_file` and some shapes of credential, but it cannot know what
the app was never told is secret, so assume a logs reader reads all of
it. Give `logs` only to someone you would show the app's output on
your own screen.

Put a `from` list on every `logs` grant, the addresses they read logs
from (an office, a VPN), as on bob's line above. The box does not
insist: someone who moves between networks may need none, and
`moteld validate` says nothing of a `logs` grant without one. But a
stolen key reads before anything else, and without a list it reads
the logs from anywhere, while the one thing that tells you is the
[new network it is used from](#when-their-key-is-used-from-somewhere-new),
on your channels (`moteld validate` already warns of a box with people
and no channel to tell). A `viewer` grant beside it, with no list,
still lets them read the app's status from anywhere.

## What they reach

Everything is a `GET` to the box's own name, with a token their key
signed:

| Path | What |
|---|---|
| `/_motel/whoami` | who the box takes them to be, their key, what they may read from where they are, and the box's release |
| `/_motel/apps` | the apps they may read, each with a summary: its phase, whether it runs, its version, its watchdog's state, its newest deploy and the version of a deploy in flight |
| `/<app>` | an app's status, as a deploy workflow reads it, with the deploy in flight when one runs: its version, who started it, when, and the phases so far with their times |
| `/<app>?deploy=<version>` | one deploy's record |
| `/` | the box's status, for a viewer of the box |
| `/_motel/backups` | what the box says of its backups, for a viewer of the box: `motel-backup status --json` as any account on the box sees it (every app's name, data paths and snapshot ids; nothing of the credentials). It is the box's own claim: look from a machine of your own too (docs/backups.md, "Look from your own machine") |
| `/_motel/apps/<app>/alerts` | the app's alerts, as the box's Alertmanager holds them: each one's name, severity, summary, when it began, and whether a silence holds it and until when, or a deploy of the app; and whether the box could read them (`"monitor"`: `on`, `off` when the box's monitor block sends nothing to anyone or there is none, `unanswered`). Only alerts about that app: not the box's own (disks, certificates, backups), not the silences or who wrote them. Up to 5 s old |
| `/_motel/apps/<app>/rules` | the rules the box alerts on that apply to the app: those of every app and the app's own `rule` lines, not one pinned to another app and none of the box's own checks (disks, backups, certificates). Each with what it fires on (its op, threshold, unit, `for`, and each guard's meaning) and the app's numbers alone: a check's last reading (its watchdog's restarts), and for a rule the metrics store evaluates, its value and whether vmalert holds it `pending` or `firing`, and since when; and whether the box could read the store (`"store"`: `on`, `off` with no `metrics` line, `unanswered`), and `rules_file` when the file vmalert reads is not this config's (a state is vmalert's). No query and no socket. The store's numbers are up to 30 s old, of an evaluation about a minute behind (as vmalert compared them) |
| `/_motel/rules` | every rule the box alerts on, for a viewer of the box: as for an app's, with every app's numbers, the box's own checks (each filesystem's use, each app's backup age) and rules (a `disk_fill`'s prediction for each filesystem). `moteld monitor rules --json` from a laptop, without the store's socket and the queries |
| `/_motel/apps/<app>/series[?range=1h\|24h\|7d\|14d]` | the app's numbers over the window (the hour when none is named), as the box's metrics store holds them, a point a step (a minute over the hour, 5 minutes over the day, 30 over 7 days, an hour over 14): its memory (each step's highest), CPU, and for an app behind `observe` its requests a second, 5xx share and p95; a step with no point is missing, never a zero; and whether the box could ask (`"store"`: `on`, `off` when the box's monitor block has no metrics line or there is none, `unanswered`). No panel, step or query of the reader's: the window is the one choice. Up to 30 s old. Experimental: the route and its JSON may change until a release's notes say otherwise |
| `/_motel/apps/<app>/logs[?lines=200\|50\|1000][&after=<cursor>]` | the app's own lines, as the box's journal holds them, for a reader of its logs: the newest (200 when none is named), or those after a cursor an earlier answer gave. Read [Reading an app's logs](#reading-an-apps-logs). Experimental: the route and its JSON may change until a release's notes say otherwise |
| `/_motel/alerts` | every alert the box's Alertmanager holds, for a viewer of the box: as for an app's, and with each the app it names and its instance (a path, a certificate's name, another site's); and every silence that is active or waits to start: its matchers, when it starts and ends, who made it and why (the operator's own words). Not the heartbeat, the test alert or the deploy mute itself, nor the two about a person's key (below), and no expired silence. At most 500 alerts, the newest, and 200 silences, the active ones and then the soonest to start, with `more_alerts` or `more_silences` when the box held more; past 4 MiB of what Alertmanager holds, `unanswered` ([monitor.md](monitor.md#more-alerts-than-motel-reads)). Up to 5 s old |

The last seven rows, from `/_motel/backups` on, are on each release
after v0.8.0. A box at v0.8.0 answers each of them `404`, `{"error":"not
found"}`, to a person it admits.

The `motel` command (for an M-series Mac and for Linux on amd64 and
arm64, an archive on each release after v0.8.0; elsewhere, built from a
clone of this repository: client/README.md, "Getting it") makes
the token and asks:
`motel whoami --box deploy.example.com`, `motel apps`, `motel status
<app>`, `motel deploys <app> [<version>]`, `motel box status`; on each
release after v0.8.0, `motel backup status`, `motel monitor alerts
[--app <app>]`, `motel monitor rules [--app <app>]`, `motel metrics
<app> [--range 1h|24h|7d|14d]` and `motel logs <app> [--follow]`, which
a box at v0.8.0 answers `404` (the client exits 6); and
`motel` alone, on a terminal, shows the same on screens (an app's
lines with `l` on its screen, for a holder of a `logs` grant). Without
it, this makes one by hand and asks who they are (`box` is the box's
name; the key is theirs; GNU or macOS `base64`):

```sh
box=deploy.example.com key=~/.ssh/id_ed25519
now=$(date +%s)
printf '{"aud":"%s","iat":%d,"exp":%d}' "$box" "$now" "$((now + 300))" > claims
ssh-keygen -Y sign -n motel-access-v1@motel.smallhours.org -f "$key" claims
b64url() { base64 | tr -d '\n=' | tr '+/' '-_'; }
token="m1.$(b64url < claims).$(sed '1d;$d' claims.sig | tr -d '\n' | base64 -d | b64url)"
curl -fsS -H "Authorization: Motel $token" "https://$box/_motel/whoami"
```

A token lasts at most ten minutes and is good for reads only. Every
refusal is the same `401`, whatever the cause: a key the box does not
list, an address outside a `from` list, a clock more than a minute off,
or a box whose moteld is too old for people. An app they may not read
is a `403` saying which role it needs, the same whether the app exists
or not. Too many requests (more than 60 at once, then 2 a second) is a
`429` with `Retry-After`. An app's logs without a `logs` grant on it,
or on the box, are a `403` that needs `logs`, for a viewer of the box
too.

Who reads what, by the grant that applies from where they are:

| | no grant | `viewer` of app A | `logs` of app A | `viewer` of the box | `logs` of the box |
|---|---|---|---|---|---|
| who they are, the apps they may read | yes (none) | A | A | every app | every app |
| an app's status, deploys, alerts, rules and numbers over time | no | A | A | every app | every app |
| an app's logs | no | no | A | no | every app |
| the box's status, backups, every alert and silence, every rule | no | no | no | yes | yes |
| deploy, roll back, push | no | no | no | no | no |

### Reading an app's logs

On each release after v0.8.0: `motel logs <app>` prints the app's
newest lines, and `motel logs <app> --follow` reads on every 2 s until
ctrl-c (client/README.md says what each prints and exits); `l` on an
app's screen in `motel` alone shows them, following them the same way
while the screen is open; or by hand,
as above, or from a program of yours, as below. A logs reader is sent
what the app printed, which no filter can make safe: read [what a logs
reader reads](#what-a-logs-reader-reads) before you give the grant.

`GET /_motel/apps/<app>/logs` is the app's newest lines, `lines` of
them at most (`200`, `50` or `1000`; 200 when none is named), oldest
first. With `after`, the `cursor` an earlier answer gave, it is the
lines after that one: poll with each answer's cursor to follow the
app. A cursor holds `;`, which a query must escape: `curl -G
--data-urlencode "after=$cursor"`; a raw `;` is the `422`. The answer:

```json
{"kept":"on","lines":[{"at":"2026-11-20T09:30:00.123456Z","release":"v42","text":"GET /cart 200 12ms"}],"more":false,"gap":false,"cursor":"s=…;i=…;b=…;m=…;t=…;x=…","redacted_env":["DATABASE_URL"]}
```

- `kept` is `off` for an app whose `deploy_log_lines` is `0`: it keeps
  its output on the box, and no line is ever sent.
- Each line is what the app wrote to its stdout or stderr (the journal
  does not tell them apart), with when the journal stored it and the
  release of the unit that wrote it. `release` is `""` when the journal
  did not keep the unit: a process that exited before journald read
  its line (a crash loop's lines often), or any line while journald is
  behind. A line is at most 4 KiB (a longer one is cut and ends in
  `…`; journald itself stores a line over 48 KiB as several), each run
  of bytes that are not UTF-8 is one `U+FFFD`, and control characters
  are as the app wrote them, for your terminal to be kept from.
- Each line passes the app's filter on its own: the values of its
  `env_file` in the forms the box knows, and some shapes of
  credential. `redacted_env` names the keys it replaced. The
  filter knows the values the running app was launched with for as
  long as it runs, after a moteld restart too (read back from
  systemd), though the file was changed since: an old value still in
  use stays hidden while the app uses it. Not a value no running instance was
  launched with and the app's `env_file` no longer holds, in a line
  written while it did (a logs read reaches lines of any age, and the
  filter knows the file's values now, those moteld has read since it
  started, and those of the instance it reattached to): changing a
  value and launching the app again does not hide the old one in lines
  already written, so revoke the old credential once the app runs
  with the new one. Nor, for the moments after a moteld restart before
  it has reattached to the app (longer while its user manager does not
  answer), a value the running app holds that the file no longer does.
- A logs reader who can send the app input (a request to its site, which
  most apps log) can test a guess of an `env_file` value: a line holding
  a whole value of 8 characters or more comes back as the marker that
  names its key, and a wrong guess as written. Never a piece of a value,
  and never a generated one, which cannot be guessed; a weak one (a
  word, a reused password) can be. The filter cannot close it, since any
  visible replacement confirms a match. Keep generated values in
  `env_file`, and give `logs` only to people you would trust with them:
  the `access: person read logs` line names the keys a read replaced, so
  a confirmed guess leaves the person's name and the key in the journal.
- `more` is `true` when there was more than was sent: the count, or
  64 KiB of the lines' text as the journal holds it (what is sent can be
  longer: the filter's markers, JSON's escapes). After a follow the lines left are after
  the last one sent: ask again at once with the cursor. After a tail
  (no `after`, or `gap`) they are older than the first one sent: no read
  pages back to them, and a tail of a larger `lines` may reach some.
- `cursor` is where the read got to, the `after` of the next. It is
  absent when the read took no line: keep the one you sent. Keep it in
  memory only, never on disk: it is the box's position in its journal.
  Any cursor the box gave is honoured while the journal holds its line,
  however old.
- `gap` is `true` when the `after` you sent was not honoured: a cursor
  of another app's, of a line the journal no longer holds, or one the
  box never gave. The lines are then a fresh tail, as with no `after`,
  and lines between may be missing. With no `cursor` beside it, forget
  yours and tail, or every poll is a `gap`.

At most four logs reads run at once on the box; a fifth waits a
second, then is a `503` with `Retry-After: 1`. A journal that cannot be
read is a `500`, and moteld's journal says why.

**What the box records.** Each read that sends lines is a line in
moteld's journal, `access: person read logs`, naming the person, the
app, how many lines, the cursors of the first and the last
(`first`, `last`), `gap` when there was one and the keys the filter
replaced. From it you can read on the box what was sent, as the app
wrote it, while the journal holds it: the lines from `first` to
`last`. An entry the read passed over as another unit's (moteld's
journal says when, `lines under an app's identifier from a unit that
is not the app's were not sent`) is among them, so read on to `last`:

```sh
journalctl --cursor='<first>' -o json --all SYSLOG_IDENTIFIER=motel-<app> _UID=$(id -u motel) _TRANSPORT=stdout \
  | sed '/"__CURSOR":"<last>"/q'
```

A person who follows an app writes one such line for each poll that
finds new lines: an app that writes at least once a poll, followed at
a poll every 2 s (as `motel logs --follow` and `motel`'s logs screen
poll), makes 1,800 lines an hour (1,200 at 3 s), about 0.5 to 0.8 MB of
journal an hour. A follow is 0.5 requests a second of the 2 a person is
allowed: `motel logs --follow` beside an open `motel` (1.5) takes a
person to the box's limit; `motel`'s logs screen follows within its own
1.5. They share one message, so `journalctl -u
moteld -o cat | grep -v '"access: person read logs"'` hides them. A
person who asks for an app's logs and is refused (no `logs` grant on
it, or one whose `from` list does not hold their address) is a Warn
line, `access: person refused logs`, with the address and what was
needed, at most once a minute for a person and each app of the box, and once a minute for a person and every name that is no app here, together.

**A chatty app can cost another app its lines.** Every app runs under
one systemd user manager, and journald's rate limit is per unit of the
system manager, so every app of the box shares one budget (10,000 lines
in 30 s, more on a box with disk to spare): an app that floods its
output can make journald drop the lines of every other app, its deploy
tail included, and a logs answer cannot know what was dropped.
journald says `Suppressed … messages from user@<uid>.service`, naming
no app. Issue #628 is about it.

## When their key is used from somewhere new

On each release after v0.8.0. A box at v0.8.0 tells no one of where a
key is used.

A stolen key reads before anything else, so the box tells your
monitor channels (the `operator` ones) when a person's key is used in a
way that suggests it is in someone else's hands. It tells, and lets
the read go on.

- **`person_new_network`**, a warning, the first time a key is
  accepted from a network: the request's address cut to a /24 for
  IPv4 or a /48 for IPv6. Every request counts once its token is good,
  a read included:

  ```
  bob@example.com (key SHA256:…) used this box from 203.0.113.0/24 for the first time
  bob@example.com (key SHA256:…) used this box for the first time, from 203.0.113.0/24
  ```

  The second is a key's first use the box knows of: you will see one
  for each person soon after you add them, and one for each key after
  the upgrade that brings this. A key keeps its last 16 networks; a
  network it has not used since sixteen others is told again.
- **`person_outside_list`**, a warning, when a key's signature is good
  but the address is outside every `from` list on that person's grants.
  The box refuses it, as above; this is at most once per key an hour:

  ```
  a valid signature by bob@example.com (key SHA256:…) from 203.0.113.9, outside every address list on their grants; refused
  ```

Both go to your channels alone: no one reads them over HTTPS, not even
a viewer of the box, nor a silence `moteld monitor silence add
--alert` makes of them. The message shows the key's fingerprint by its start, as `moteld box
describe` prints it on the `person` line; the alert's `instance` label
and the journal hold it whole. When one of these comes:

1. **Ask the person** whether it was them: a train, a café, a new
   phone's hotspot or a VPN is a new network.
2. **If it was not, or you cannot reach them, remove the key now**:
   take its `person` line (and, if it was their only key, their `grant`
   lines) out of the box's Caddyfile and push, as in [Remove
   them](#remove-them). Their tokens are refused from the reload on,
   and a token already made dies within ten minutes. They make a new
   key, and you add its `person` line and push again.
3. **If it was them, nothing to do.** To refuse their key from
   anywhere but the addresses you expect, put a `from` list on every
   grant they hold (a grant without one still lets them in from
   anywhere): from elsewhere their key is then refused, and you get
   `person_outside_list`. A network inside the list that the key has
   not used before is still told as `person_new_network`.

A person with a `logs` grant that has a `from` list, beside a `viewer`
grant on the same app or on the box that has none (bob above), is let
in from anywhere on the viewer grant. From outside the logs grant's
list, a network the key has not used before is told as
`person_new_network`, as always; `person_outside_list` is not told,
since a grant applies, so the key was not used outside every list. A
logs read from there is refused with a `403`, and is not told to your
channels; moteld's journal has it (`access: person refused logs`, at
most once a minute for a person and each app of the box, and once a minute for a person and every name that is no app here, together). To hear of their key used outside the office at all, put a
`from` list on the viewer grant too.

Each one-shot also has a line in moteld's journal, `access: a key used
from a new network` or `access: a valid signature from outside every
address list`, with the person, the key and the network or address:
`journalctl -u moteld -o cat | grep '"access: a'`. On a box with no
monitor channel that line is all there is (`moteld validate` warns of
such a box). A thief who keeps changing networks is told four times at
once, then once a quarter-hour per key, and the journal has every one
(`"suppressed"`). While you add many people at once, `moteld monitor
silence add --alert person_new_network --duration 1h --comment "adding
the team"` quiets their first uses; the journal still has them.

Behind a proxy of yours that `trusted_proxies` names, the box reads a
request's address only with `trusted_proxies_strict` too: without it,
every request the proxy forwards is from "an address the box could not
read", one network for all of them, so after a key's first use through
the proxy nothing more is told of it. `moteld validate` warns of that.
A proxy the box is not told of at all makes every request come from
the proxy's own network, and a listener on a unix socket makes every
request one the box could not read.

What is kept: in `/var/lib/motel/people-networks.json` (moteld's own,
`0600`), each key's fingerprint and its last 16 networks, never a time
or a whole address. A key whose lines you remove is dropped from it at
the next person's request after the reload; if you removed everyone,
it stays until a person is admitted again. The file stays when the
package is removed, `apt purge` too. To forget them, stop moteld
first (`systemctl stop moteld`; it keeps the record in memory and
writes it back), delete the file, and start it: each key's next use is
then told again as its first. The journal and your
channels keep each message (the person's name, the key's fingerprint,
the network or the whole address) as long as they keep anything.

## Remove them

Remove their `person` and `grant` lines and push. The box reloads with
the new file, and from then their key gets the `401`. A `signer` line
is removed as before, and with it that signer's key as a person.
