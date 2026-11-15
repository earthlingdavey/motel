# Giving a person access

**Experimental.** A person you name in the box's Caddyfile can read,
from their own laptop and over HTTPS, the status and deploys of the apps
you choose, and the box's own status, with their own ssh key: no
account on the box, no ssh access, and no deploy token. They cannot
deploy, roll back or push, whatever you write. ssh stays root's.

This needs a box with a box repository: a `box` block and a site with
`box_webhook` (the template's deploy host). People reach the box only on
that site's name.

## What they need

An ssh key of a type a person may hold: `ssh-ed25519`, `ecdsa-sha2-*`,
or the hardware forms of either (`sk-ssh-ed25519@openssh.com`,
`sk-ecdsa-sha2-nistp256@openssh.com`). Not RSA: someone with only an RSA
key makes one with `ssh-keygen -t ed25519`, or `ssh-keygen -t
ed25519-sk` for a hardware key. They send you the public half, the one
line of their `.pub` file.

## Add them

In the `box` block of the box's Caddyfile, a `person` line for their key
and a `grant` line for what they may read. The name comes first on
both, so one search finds every line about a person:

```caddyfile
box {
	deploy_trust github { … }
	signer alice@example.com ssh-ed25519 AAAAC3Nz…

	person bob@example.com ecdsa-sha2-nistp256 AAAAE2Vj…
	grant bob@example.com viewer app blog
	grant alice@example.com viewer box
}
```

- `grant <name> viewer app <app>` lets them read that app: its status
  and its deploys. `grant <name> viewer box` lets them read every app
  and the box's own status.
- A `signer` line's ed25519 key is already that signer's key as a
  person: `alice` above needs no `person` line, only a `grant`.
- A second key for the same person is a second `person` line with the
  same name.
- `from` limits a grant to the addresses you list, last on the line:
  `grant bob@example.com viewer app blog from 198.51.100.0/24
  2001:db8:5::/48`. From anywhere else their key gets the same refusal
  as a stranger's. If your server has `trusted_proxies`, the address is
  the one those proxies forward, so the list is only as good as those
  ranges: `motel validate` says so.

Check the file, then push it as any change to the box (a signed
commit):

```sh
motel validate --config Caddyfile
motel box describe Caddyfile
```

`describe` prints a `person` line per key with its fingerprint, which
is what they will see as their key, and a `grant` line per grant. A
`warning` line names anything taken with a doubt: a person with no
grant, an app grant a box grant already covers, people with no monitor channel.

On a box with no box repository, edit `/etc/motel/Caddyfile` as root,
`motel validate --config /etc/motel/Caddyfile`, and `systemctl reload
motel`. The box reads who it admits from that file at each load; the
journal says `box: people admitted`, or why no one is.

## What they reach

Everything is a `GET` to the box's own name, with a token their key
signed:

| Path | What |
|---|---|
| `/_motel/whoami` | who the box takes them to be, their key, what they may read from where they are, and the box's release |
| `/_motel/apps` | the apps they may read |
| `/<app>` | an app's status, as a deploy workflow reads it |
| `/<app>?deploy=<version>` | one deploy's record |
| `/` | the box's status, for a viewer of the box |

A client that makes the token is to come. Until then, this makes one
by hand and asks who they are (`box` is the box's name; the key is
theirs; GNU or macOS `base64`):

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
or a box whose motel is too old for people. An app they may not read
is a `403` saying which role it needs, the same whether the app exists
or not. Too many requests (more than 60 at once, then 2 a second) is a
`429` with `Retry-After`.

## Remove them

Remove their `person` and `grant` lines and push. The box reloads with
the new file, and from then their key gets the `401`. A `signer` line
is removed as before, and with it that signer's key as a person.

## Going back to an older release

A release without people refuses a Caddyfile with these lines: take
them out first ([Upgrading](upgrading.md)).
