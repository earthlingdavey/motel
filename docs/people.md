# Giving a person access

**Experimental, since v0.8.0.** A person you name in the box's
Caddyfile can read, from their own laptop and over HTTPS, the status
and deploys of the apps you choose, and the box's own status, with
their own ssh key: no account on the box, no ssh access, and no deploy
token. They cannot deploy, roll back or push, whatever you write. ssh
stays root's.

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
	grant alice@example.com viewer box
}
```

- `grant <name> viewer app <app>` lets them read that app: its status,
  its deploys, its alerts, and the rules that apply to it with its
  numbers. `grant <name> viewer box` lets them read every app,
  the box's own status, what the box says of its backups, every
  alert and silence the box's Alertmanager holds, and every rule the box
  alerts on with every number.
- A `signer` line's ed25519 key is already that signer's key as a
  person: `alice` above needs no `person` line, only a `grant`. Once
  the block names anyone in a `person` or `grant` line, every signer
  with an ed25519 key can ask who they are here, even with no grant,
  and is told an empty list of apps.
- A second key for the same person is a second `person` line with the
  same name.
- `from` limits a grant to the addresses you list, last on the line:
  `grant bob@example.com viewer app blog from 198.51.100.0/24
  2001:db8:5::/48`. If every grant they hold has a `from` list, from
  anywhere else their key gets the same refusal as a stranger's; with
  a grant beside it that has none, they are still let in, and told
  `403` for what only the limited grant covers.
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
commit):

```sh
moteld validate --config Caddyfile
moteld box describe Caddyfile
```

`describe` prints a `person` line per key with its fingerprint, which
is what they will see as their key, and a `grant` line per grant. A
`warning` line names anything taken with a doubt: a person with no
grant, an app grant a box grant already covers, people with no monitor
channel to tell you when their key is used from somewhere new ([below](#when-their-key-is-used-from-somewhere-new)).

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
| `/_motel/apps/<app>/alerts` | the app's alerts, as the box's Alertmanager holds them: each one's name, severity, summary, when it began, and whether a silence holds it and until when, or a deploy of the app; and whether the box could ask (`"monitor"`: `on`, `off` when the box's monitor block sends nothing to anyone or there is none, `unanswered`). Only alerts about that app: not the box's own (disks, certificates, backups), not the silences or who wrote them. Up to 5 s old |
| `/_motel/apps/<app>/rules` | the rules the box alerts on that apply to the app: those of every app and the app's own `rule` lines, not one pinned to another app and none of the box's own checks (disks, backups, certificates). Each with what it fires on (its op, threshold, unit, `for`, and each guard's meaning) and the app's numbers alone: a check's last reading (its watchdog's restarts), and for a rule the metrics store evaluates, its value and whether vmalert holds it `pending` or `firing`, and since when; and whether the box could read the store (`"store"`: `on`, `off` with no `metrics` line, `unanswered`), and `rules_file` when the file vmalert reads is not this config's (a state is vmalert's). No query and no socket. The store's numbers are up to 30 s old, of an evaluation about a minute behind (as vmalert compared them) |
| `/_motel/rules` | every rule the box alerts on, for a viewer of the box: as for an app's, with every app's numbers, the box's own checks (each filesystem's use, each app's backup age) and rules (a `disk_fill`'s prediction for each filesystem). `moteld monitor rules --json` from a laptop, without the store's socket and the queries |
| `/_motel/alerts` | every alert the box's Alertmanager holds, for a viewer of the box: as for an app's, and with each the app it names and its instance (a path, a certificate's name, another site's); and every silence that is active or waits to start: its matchers, when it starts and ends, who made it and why (the operator's own words). Not the heartbeat, the test alert or the deploy mute itself, nor the two about a person's key (below), and no expired silence. At most 500 alerts, the newest, and 200 silences, the active ones and then the soonest to start, with `more_alerts` or `more_silences` when the box held more. Up to 5 s old |

The `motel` command (for an M-series Mac, an archive on each release
after v0.8.0; elsewhere, built from a clone of this repository:
client/README.md, "Getting it") makes
the token and asks:
`motel whoami --box deploy.example.com`, `motel apps`, `motel status
<app>`, `motel deploys <app> [<version>]`, `motel box status`, `motel backup
status`, `motel monitor alerts [--app <app>]`, `motel monitor rules [--app <app>]`; and
`motel` alone, on a terminal, shows the same on screens. Without
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
`429` with `Retry-After`.

## When their key is used from somewhere new

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
