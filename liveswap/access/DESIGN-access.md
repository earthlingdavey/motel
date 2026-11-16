# access: people, the token they sign, and who may take which action

`access` is how a person, not CI, reaches a box from a laptop. A person
is declared once in the `box` block of the Caddyfile with their ssh key
(`person` and `grant` lines, box/DESIGN-box.md "People and grants"), is
given the `viewer` role on one app or on the whole box, and signs a
short token with their own key, which the client sends over HTTPS to
the box's own name. ssh stays root's. One function, `Decide`, says on
every request either webhook serves whether the caller may take the
route's action.

This release gives people reads only: an app's status and deploys, the
box's status, who they are here, and which apps they may view. No
person can deploy, roll back or push, whatever the lines say.

- The person who runs a box is the operator.
- The words `Motel` (the `Authorization` scheme) and `m1.` (the
  token's envelope) are proposals the owner has not confirmed; each is
  one constant in the code (`access.TokenScheme`,
  `access.TokenEnvelope`). The namespace
  `motel-access-v1@motel.smallhours.org` and the prefix `/_motel/` are
  chosen and one-way.

## The shape

| Piece | What | Where |
|---|---|---|
| The lines | `ParsePerson`, `ParseGrant`, `CheckLines`: one line's arguments, and the rules across lines, shared by the module's parser and the walk | `lines.go`; box's `caddyfile.go` and `tokens.go` call them |
| The registry | each key a person may sign with by its fingerprint, the name it belongs to, that name's grants, and the box's identity (the `box_webhook` host) | `registry.go`; built by the `box` app (`box/app.go`, `loadRegistry`) |
| The token checker | `CheckToken`: checks 1 to 13 below; writes nothing, journals nothing, knows no person | `token.go` |
| `Decide` | the cells of who may take which action, narrowed for a person by their grants | `access.go` |
| The rate | a token bucket per person name, for the process | `rate.go` |
| The way in | `AdmitPerson`: the checker with the registry as its lookup, the address list, the failure budgets, the rate | `liveswap/people.go`, called by both webhooks |
| The routes | liveswap's route table and its rows under `/_motel/`; box's table on `/` | `liveswap/handler.go`, `liveswap/motel_routes.go`, `box/handler.go` |

`access` is a leaf package in the liveswap module: it imports nothing
of liveswap's and no Caddy package. `box` imports liveswap, never the
reverse; liveswap's webhook reaches the registry the `box` app builds
through `ctx.AppIfConfigured("box")` and the interface `access.Source`.
A config with no `box` block has no registry, and no person is
admitted.

## The registry

**Read from the installed file, at each load, by the walk.** The `box`
app reads `/etc/motel/Caddyfile` as the walk reads it (box/DESIGN-box.md,
"Reading the signed file"), so root and motel read the same signed
bytes, and a config posted to the admin socket by hand cannot add a
person. The walk also gives the box's identity, which every token's
`aud` and the request's `Host` must equal and which no module sees at
load.

**Built at provision, before any app starts.** Caddy may start the
http app before liveswap, and liveswap installs each app at its
`Start`, some tens of milliseconds later. A deploy token in that moment
is checked against the sources the config built at provision and, once
it verifies, waits for the install, at most 5 s (issue #473). An
issuer's token whose keys the process has not fetched yet is refused
then as at any time, since no token waits on an issuer. The
registry is built in the `box` app's `Provision`, and a person's way
in reads nothing liveswap's `Start` installs, so a person's token is
answered from the first request and never waits.
`TestPersonNotRefusedAfterAStart` provisions a whole config, starts
nothing, and shows a deploy token held and then told to come back, a
stranger refused at once, and a person's token answered at that
moment.

**What admits no one.** A config with no `box` block, which has no
`box` app and so no registry, and says nothing; and, with a `box`
block, no installed file, a file that cannot be read, one the walk
refuses, and one whose `box` block has no `person` and no `grant` line
(its signers are not logins). With a `box` block, each but the last is
said in the journal at each load (`box: no person is admitted from the
Caddyfile this machine runs (/etc/motel/Caddyfile): …`): a warning when
the config declares people, information otherwise, and only debug for
a config that declares no one on a machine with no installed file.
Every line the registry writes names the file this machine runs: at
`motel validate` of a candidate, it is still this machine's. Deploys
are not touched by any of it.

**A person's key is an Ed25519 key, for now**: `ssh-ed25519`, or
`sk-ssh-ed25519@openssh.com` for a hardware key (the owner's decision
of 2026-11-15). ECDSA is not accepted yet: a `person` line with an
`ecdsa-sha2-nistp256`, `-nistp384`, `-nistp521` or
`sk-ecdsa-sha2-nistp256@openssh.com` key is refused by name, by both
readers, with the remedy (`ssh-keygen -t ed25519`, or `-t ed25519-sk`
for a hardware key), and a token signed with one is refused as
`key-type` before any lookup, so no ECDSA signature is ever verified
for a person. So an agent that makes only P-256 keys (the Secure
Enclave's, Secretive's) cannot be used for now. The reason is the cost
a stranger can spend: presenting a listed person's public key buys one
signature check of its type, about 0.04 ms for Ed25519 and 0.5 to
1.3 ms for the larger ECDSA curves. Admitting a type later breaks no
one; taking one away would. RSA stays refused, whatever its size.

**Only keys a person may sign with are listed**: a `person` line's, and
a `signer` line's ed25519 key once the block declares people (a
`signer` line takes no ECDSA key: box/proof refuses one).
An `ssh-rsa` signer key, and an Ed25519 key of small order, are no
person's key. The lookup never returns a nil key as listed.

**A reload reads the file again.** Removing a person is removing their
lines; the push's reload builds a registry without them, and from then
their token is the flat 401 (`TestRemovedPersonRefusedAfterReload`).
A console edit of the file is seen at the next reload, as for any other
line.

**Nothing keys on the token's bytes**: the same claims signed again
under the other hash, or by an `sk-` key with another counter, are
another token that verifies alike. Anything that must recognise a token keys
on its verified claims and the key's fingerprint.

## The token

```text
Authorization: Motel m1.<base64url claims>.<base64url SSHSIG>
```

The claims are exactly `{"aud":"<box>","iat":<n>,"exp":<n>}`, in that
order and that spelling, signed with `ssh-keygen -Y sign -n
motel-access-v1@motel.smallhours.org`; the SSHSIG structure travels
de-armored. A token is good on a `GET` only, for at most 600 s from
`iat`, with 60 s of clock skew each way. A claim this form does not know
(a scope, an act) is refused: a later form raises the protocol number.

`CheckToken` makes the checks in this order, and no claim is read before
the signature verifies:

| # | Check | Reason in the journal |
|---|---|---|
| 1 | a header in the `Motel` scheme; none: not a person's request | — |
| 2 | exactly one `Authorization` header | `authorization-not-one-header` |
| 3 | at most 8 KiB, before any parse | `too-long` |
| 4 | `m1.`, two more parts, strict unpadded base64url, claims at most 1 KiB | `envelope-…`, `claims-…`, `sig-encoding` |
| 5 | the SSHSIG structure: magic, version 1, nothing after it | `blob-…` |
| 6 | the key's type is a person's (Ed25519 in either form: no RSA, and no ECDSA yet), then its fingerprint is listed | `key-type`, `key-not-listed` |
| 7 | namespace, empty reserved field, `sha256` or `sha512`, the `sk-` trailer and touch flag, nothing after the signature, the signature | `signature-…` |
| 8 | the claims in their one spelling, no unknown key | `claims-json`, `claims-not-canonical` |
| 9 | `aud` is the box's identity | `aud` |
| 10 | the request's `Host` (port dropped, ASCII lowered) is the box's identity | `host` |
| 11 | the times, with the skew | `times`, `iat-in-future`, `expired` |
| 13 | the lifetime, then the method | `life`, `method` |

A request is a person's only by its scheme: such a request never meets
a deploy token's sources, and a deploy token never meets `CheckToken`.
A `Motel` header beside a `Bearer` one is refused at check 2.

## How a request is judged

On either webhook, a request in the `Motel` scheme goes through
`AdmitPerson`, in this order:

1. `CheckToken`, with the registry as its lookup. A refusal is the flat
   401 below, through the same failure budgets as a deploy token's
   (`Limiter.Refuse`); the reason, and the key and its person once the
   key was found listed (and the host received, for `host`), go to the
   journal only.
2. **The address list.** Each grant whose `from` list does not hold the
   request's address does not exist for this request. The address is
   Caddy's `client_ip`, whole: the peer, or the address a proxy in the
   server's `trusted_proxies` forwarded for. A request a trusted proxy
   forwarded, on a server without `trusted_proxies_strict`, matches no
   list: Caddy then takes the left-most address in `X-Forwarded-For`,
   which the client writes. A person who holds grants
   and has none left from here gets the flat 401, and the journal names
   the key, the person, the address and the peer. A person with no
   grant at all goes on: they may ask who they are.
3. The address's failures are forgotten, as for an accepted deploy
   token.
4. **The rate**: 429 past the person's budget.
5. `Decide`, then the route's function.

Any other request takes the path it always took, untouched.

## Routes that are not an app

liveswap's webhook takes the app from the path's last segment, so
`/blog`, `/x/y/blog` and, before this release, `/_motel/blog` all named
`blog`. It now checks the first segment first: a path under `/_motel/`
names no app (`_` is outside the app grammar).

| Route | Action | Who |
|---|---|---|
| `GET /_motel/whoami` | `box.whoami` | any declared person, granted or not |
| `GET /_motel/apps` | `box.apps`, and `app.status` for each row's summary | any declared person: the apps they may view |
| any other path, method or query under `/_motel/` | none | 404 once authenticated, the flat 401 before |

A deploy token under `/_motel/` is judged against the global sources,
as for a name that is no app: a global source's is told the 403 that
names a declared key, or the 404; an app's source's is the flat 401.
`box_webhook` answers on `/` alone and is unchanged.

## Decide

### The principals

| Principal | How it is known | What `Decide` is given |
|---|---|---|
| a deploy source of an app | a `Bearer` token one of that app's `deploy_trust` sources accepted | `AppDeployer`, the app in the path |
| a global deploy source | a `Bearer` token a global liveswap source accepted | `AppDeployer` of no app (404 on an app's path, before the table) |
| the box channel's source | a `Bearer` token the `box` block's `deploy_trust` accepted | `BoxChannel` |
| a person | a `Motel` token `CheckToken` accepted | `Person`: the name, and the views the grants that apply from here give (`Views`: the box, or a list of apps) |

### The table

| Action | Route | Deploy source of the app | Box channel | Person |
|---|---|---|---|---|
| `app.status` | `GET /<app>[?range=1h\|24h]` | its own app | — | a viewer of that app or of the box |
| `app.deploy_record` | `GET /<app>?deploy=<v>` | its own app | — | a viewer of that app or of the box |
| `app.deploy` | `POST /<app>`, a gzip or JSON body | its own app | — | never (403 naming a deploy token of this app; a person's token is refused off a `GET` before it) |
| `app.rollback` | `POST /<app>?rollback=<v>` | its own app | — | never, as above |
| `box.status` | `GET /` on `box_webhook` | — | yes | a viewer of the box |
| `box.push_result` | `GET /?result=<id>` | — | yes | never (403 naming a deploy token of the box) |
| `box.push` | `POST /` | — | yes | never |
| `box.whoami` | `GET /_motel/whoami` | 403, needs a declared key (a global source's token: an app's own is the flat 401 under `/_motel/`) | the flat 401: liveswap's webhook, which serves `/_motel/`, does not know the box channel's source (the 403 only when it is a global liveswap source too) | any person |
| `box.apps` | `GET /_motel/apps` | as for `box.whoami` | as for `box.whoami` | any person |

A box grant covers every app. A pair the table does not hold is
refused. Three things are structural: no person's cell holds an act; no
deploy source's cell holds a person's route; and a person's token is
refused off a `GET` by the checker, before `Decide`.

The tests: `TestDecideTable` writes every cell out for every kind,
people included; `TestAllowedCells` pins the table both ways;
`TestEveryRouteHasAnAction`, `TestEveryActionHasARoute` and
`TestEveryMotelRouteHasAnAction` hold the route tables and the actions
to each other; `FuzzNoRouteNoAnswer`, in each webhook, drives any
method, path and query for every kind of caller, people included.

### Scoping as structure

A read cannot be made without a handle to one app: each route's
function takes an `appHandle`, which only `allowApp` makes, after
`Decide` allowed the action on that app, and each reads that app alone,
through that app's response filter. `TestOneHandleMaker` holds the
package to that one maker. `TestTwoAppsTwoPeople` runs two deployed
apps, each with a canary value in its env_file, and for every row of
both route tables and every kind of caller asserts the table's status
and that no 200 holds the name, canary or socket directory of an app
the caller may not view; a row whose shape it does not know fails it.
Its half on `box_webhook` does the same for that table.

`GET /_motel/apps` is the one body about several apps, and it is made
of rows that each keep to one app. The names are those `Views` covers;
each row's summary is read through the handle `allowApp` makes for
`app.status`, the action `GET /<app>` is asked about, from that app's
status, and passes that app's filter alone, before the list puts it
in place. So no filter is given two apps' values, and a row whose
filter withholds (its env_file unreadable) becomes the name and the
filter's sentence while the other rows and the 200 stand. The
two-apps test gives each app a newest deploy whose version and
`deployed_by` hold its canary, and asserts for every caller that each
row holds its own canary only as its filter's marker and nothing of the
other app.

## The answers

| Answer | When | Body |
|---|---|---|
| 401 | every refusal of a token, a person's or a deploy token's, whatever the reason; a person outside every address list | the flat 401, byte for byte, headers included: `{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}`. No `Motel-*` header |
| 403 | a verified caller `Decide` refuses | `wire.Forbidden`: `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}`; `needs` is `viewer`, `deployer` or `key` (a token a declared key signed), `scope` is `app` or `box`. It names no app and echoes nothing of the request |
| 404 | a viewer of the box asking for a name that is no app (`unknown app "<name>"`, as a global deploy source is told); a path under `/_motel/` with no row (`not found`) | `wire.Error` |
| 429 | a person past their rate | `{"error":"too many requests; retry after <n> s"}`, with `Retry-After` |
| 503 | a deploy token that verified for an app liveswap's `Start` has not installed within 5 s, or whose request ended first (#473) | `{"error":"motel is starting and has not loaded this app yet; retry after 1 s"}`, with `Retry-After: 1` |
| 200 | `GET /<app>` | `wire.Status`, the body a deploy token reads too. `deploying` is the deploy in flight: `version`, `deployed_by`, `started_at` (when the box took it: for a push, before its upload) and `phases`, those entered so far in order, the last one's `seconds` running to the answer; absent when none runs. It is set when the box takes a deploy (a rollback and a relaunch are deploys; the watchdog's restarts are not) and cleared in the step that sets `last_deploy`, so a read shows one or the other. It sits before `deploys` and `last_deploy`, and holds no `"phase"` key: the templates' `deploy.sh` reads the failing phase as the last `"phase"` in a failure body |
| 200 | `GET /_motel/whoami` | `wire.WhoAmI`: `name`, `key` (its fingerprint), `roles` (the grants that apply from here, no address list shown), `box`, `version`, `protocol`, `token_expires_at` |
| 200 | `GET /_motel/apps` | `wire.Apps`: the apps this config runs that the person may view, sorted by name, each an object, `wire.AppEntry`: `name`, then `summary` (`phase`, `running`, `current_version`, `watchdog`, the watchdog's state, `newest_deploy`, the status's first `deploys` entry, and `deploying`, the version of a deploy in flight), each value as `GET /<app>` gives it at the same moment; or, for a row the app's filter withheld, `error` in place of `summary`. A row the filter changed has its own `redacted_env`. No socket, pid, unit or command. A client reads an absent key as "not known" |

No answer says "person": a later kind of declared key changes no field.

**Unknown names.** A stranger asking for any name gets the flat 401. A
person whose grants are on some apps only gets the same 403 for an app
that exists and one that does not: `Decide` comes before the app is
looked for, so a person of one app learns nothing of the others. A
viewer of the box, who may list the apps, gets the 404.

## The version on the wire

No answer of either webhook carries a header that names motel, to any
caller, verified or not. The release is what picks a known flaw: a
header on every verified answer would show it to every token holder
and to anything that reads the responses, where whoami's `version`
shows it only to a declared person who asks. whoami's `protocol` is
the number of the token forms the box takes, and moves only with them
(1: this form). The costs, both said by the client: a box too old for
people and a key the box does not list both look like the flat 401;
and nothing tells motel's own 5xx from a gateway's, so motelctl says
every 5xx as not known to be motel's (motelctl/README.md).

## Safeguards

**Keys by fingerprint.** One parse of the structure, one hash, one map
lookup, then one signature check: a refusal costs the same whatever the
number of keys. A stranger can put a listed public key in the structure
and buy one signature check, as a JWT under a known `kid` does; the
failure budgets bound the journal, not the work. A token whose key is
not listed, but is of a type some listed key has, is checked against a
dummy key of that type (made at start, listed nowhere) before it is
refused, so a stranger holding someone's published public key does not
learn from the time whether this box lists it. A key of a type no
listed key has is refused before any verification: it cannot be
mistaken for a listed key, and checking it would let anyone, with no
key at all, make every box (one with no people too) spend a signature
check per request (and the cost of that check is why a person's key is
Ed25519 for now: a P-521 one was over a millisecond). So a stranger can make
the box do at most one verification, of a type some listed key has,
per request, which presenting a listed person's public key already
buys. What timing still tells a stranger is which key **types** are
listed, not which keys; and the refusals do not take constant time,
only the signature check is matched.

**The rate per person**: 2 requests a second sustained, a burst of 60,
per name, for the process, so a reload does not refill it; only a
verified token reaches it, so it holds declared names alone. 429 with
`Retry-After`; one journal line a minute per person while throttled
(`access: person throttled`). It is apart from the failure budgets,
which count refusals by address.

**The address list** is held to `client_ip`. Behind a proxy the
operator runs, `trusted_proxies` naming it and `trusted_proxies_strict`
make the list work: Caddy then takes the right-most address in
`X-Forwarded-For` outside the ranges, which the operator's proxy
appended. Without `trusted_proxies_strict` it takes the left-most,
which the client writes, so a client could name any address: `motel
motel's gate refuses a grant's `from` list on a box with a server that
trusts proxies (`trusted_proxies`, or `trusted_proxies_unix`) without
it, and the serving process matches no list for a request such a proxy
forwarded. With it, `motel validate` warns, naming the headers the
server reads the address from (`client_ip_headers`; `X-Forwarded-For`
by default): the list holds only if the operator's proxy sets each of
them itself, never passing on one the client sent. A server with a
`proxy_protocol` listener wrapper is warned of too: the address is
whatever the PROXY header names, so the list is as good as that
wrapper's `allow` list, and a `fallback_policy` that takes the header
from any peer lets a client name its own. In short, a `from` list is as
trustworthy as whatever tells Caddy the client's address. The module
cannot refuse this at load itself: Caddy loads a server's
`trusted_proxies` and clears the raw field before it provisions the
handlers, and the `box` app cannot ask the `http` app without a
provisioning cycle. Where each way of loading a config stands:

| How the config arrives | A `from` list behind proxies trusted without `trusted_proxies_strict` |
|---|---|
| `motel validate` | refused: an `Error`, exit 1 |
| `motel reload` (and `systemctl reload motel`) | refused before it reaches the server: nothing is loaded, the running config stays |
| a signed push | refused at the handler's validate (it runs `motel validate`): the push's run is red, nothing installed |
| `motel run` (a start, a boot) | an `ERROR` line, and served all the same, as for the gate's other refusals; the handler fails closed: no list holds a forwarded request |
| the admin socket, by hand | no gate: loaded, and the handler fails closed as above |

**A box with people and no monitor channel** is allowed, with a warning
from `motel validate` and in the journal at each load: in this release
nothing a person does is announced anyway; the announcements of later
steps would reach no one.

## Upgrade and downgrade

An upgrade needs nothing: a file without `person` or `grant` lines
loads as before and admits no one. Going back to a release without
them needs the lines taken out first (docs/upgrading.md): such a
release refuses the file (`unknown box subdirective "person"`).

## Triage

Findings are triaged by the triage skill on its general scales.
access's examples:

| Axis | Examples |
|---|---|
| **Severity 5** | a person reads an app they hold no grant on; a person's token acts, deploys or pushes; a token for one box accepted at another; a stranger's request answered as a person's; a key no line lists accepted |
| **Severity 4** | every person locked out until the console, with no line saying why |
| **Severity 3** | a refusal that tells a stranger something (a key is listed, a name is an app); a grant that grants more or less than `describe` prints; a person told the wrong role in a 403 |
| **Severity 2** | a 401 for a valid token, retried and named; a 429 to a person inside their budget |
| **Severity 1** | a journal line's wording |

### The changes of this release, scored

| # | Change | Failure it could cause | S | O | D | Outcome | What holds it |
|---|---|---|---|---|---|---|---|
| 1 | people's reads, by app | a route shows one app's data to another app's person | 5 | 5 | 5 | fix | the app handle only `allowApp` makes; each app's own filter; `TestTwoAppsTwoPeople` over every row and caller |
| 2 | the person token | a viewer's token accepted for an act | 5 | 5 | 5 | fix | a person's token is good on a `GET` alone (check 13); no person's cell acts |
| 3 | the person token | a token for box A accepted at box B | 5 | 5 | 5 | fix | `aud` and `Host` both equal to the identity (checks 9, 10) |
| 4 | the token beside deploy tokens | one kind taken as the other | 5 | 5 | 5 | fix | the scheme picks the checker; two headers refused; no person's cell holds `app.deploy` |
| 5 | the `Motel` scheme in both webhooks | an existing answer changes | 3 | 5 | 1 | fix | any other request takes today's path; `handler-bodies.txt` passes with additions only; every refusal is the flat 401 byte for byte (`TestPersonRefusalsAreTheFlat401`, both webhooks) |
| 6 | the version on the wire | the release told to strangers, or to every token holder | 2 | 5 | 5 | fix | no header names motel, its protocol or its release, only whoami's body (`TestNoMotelHeaderOnVerifiedAnswers` and `TestReleaseInWhoamiBodyAlone` in liveswap, `TestNoMotelHeaderOnVerifiedAnswers` and `TestNoMotelHeaderOnPersonAnswers` in box) |
| 7 | two readers of the lines | `validate` and the serving process disagree on who is admitted | 3 | 5 | 3 | fix | one parser for both; the registry from the walk alone; a grant on an app the config does not run now fails the load too |
| 8 | the registry from the installed file | the walk refuses it, so no person is admitted | 2 | 5 | 1 | fix | a journal line at each load naming why |
| 9 | the registry after a start | a person refused in #473's window | 2 | 4 | 1 | fix | built at provision; `TestPersonNotRefusedAfterAStart` |
| 10 | the address list on `client_ip` | a client names its own address in `X-Forwarded-For` behind a trusted proxy (Caddy's default, non-strict mode takes the left-most), or too wide a `trusted_proxies` lets a peer set it | 3 | 3 | 5 | fix | without `trusted_proxies_strict`: refused by the gate, and no list holds a forwarded request; with it, `motel validate` warns that the ranges decide; `TestFromBehindATrustedProxy` drives a forged header through a provisioned server in each mode |
| 15 | the cost of a refusal by lookup | a stranger tells by timing whether a published key is listed | 2 | 2 | 5 | fix | a signature check against a dummy key of the type before `key-not-listed`, for a type some listed key has (`TestUnlistedKeyCostsASignatureCheck`) |
| 16 | that dummy check | a stranger with no key makes any box verify a P-521 signature per request (1.3 ms against 3 µs) | 3 | 3 | 5 | fix | no dummy check for a type no listed key has, nor with no registry (`TestUnlistedTypeCostsNothing`); since 2026-11-15 no ECDSA type at all, refused as `key-type` before any lookup (`TestECDSANeverVerified`) |
| 11 | people with no channel | what they do is told to no one | 3 | 5 | 5 | fix by naming | warned by `validate` and at load |
| 12 | the 403 to people | a 403 for one app tells a person of another that the name exists | 3 | 5 | 5 | fix | one 403 for an app that exists and one that does not |
| 13 | `Host` equal to the identity | a proxy that rewrites `Host` refuses every person | 2 | 5 | 1 | fix | the journal's line names the host received |
| 14 | the `/_motel/` prefix | a deploy to `POST /_motel/<app>`, which deployed `<app>`, gets 401 or 404 | 2 | 5 | 1 | accept | no template or doc posts there; loud |
| 17 | a deploy token after a start (#473) | a valid deploy token refused as a stranger's for some tens of milliseconds after a start, when Caddy starts the http app before liveswap (measured: 10 of 20 restarts) | 2 | 4 | 3 | fix: size 3, risk 1; **Wide** (every deploy and status request runs through it) | until a `Start` installs the app, the token is checked against the config's own sources; a verified one waits for the install, at most 5 s and the request's life, then 503 with `Retry-After`, which `deploy.sh` sends once more; nothing waits before a token verified. Covered: a `local` key's token, and an issuer's whose keys are fetched; the first token under an issuer's key after a start can still be refused once, while the key is fetched, as before (#306's row in DESIGN-liveswap.md), and the script's resend of a 401 covers it. `TestDeployTokenNotRefusedBeforeStart`, `TestDeployTokenBeforeStartPastTheBound` (on a fake clock; `TestInstallWaitIs5s` pins the bound), `TestTrustIsOneRead`, `TestStrangerBeforeStartIsTheFlat401`, `TestInstallWaitEndsWithTheRequest`, `TestReloadBeforeStartAnswersAtOnce`, `TestPersonBeforeStartDoesNotWait`; through a provisioned config, `TestPersonNotRefusedAfterAStart` |
| 18 | the apps list's summary | one app's env value, or its name or socket, in another app's row | 5 | 5 | 5 | fix | a filter per row, never one over the list (`redact.go`, rule 2); each row through `allowApp` for `app.status`; `TestTwoAppsTwoPeople` over each row with a canary in the summary's fields; `TestAppsRowWithheld` |
| 19 | the apps list's summary | the list and `GET /<app>` disagree about an app | 3 | 5 | 3 | fix | the summary is cut from the same status (`summaryOf`), never computed; `TestAppsSummaryIsTheStatus` for an app that serves, one never deployed, one whose newest deploy failed and one throttled |
| 20 | the apps list's summary | a person at the rate on a box of many apps spends the box's CPU. A row reads the app's state in memory and, of the disk, only the newest deploy record (`summaryStatus`), never the release listing or the other records, which `GET /<app>` reads. Measured (go1.27.3, arm64, one core, five records an app, the token's check included): 8.6 KB and 4.8 ms a list for 30 apps, 57 KB and 31 ms for 200, against 0.8 ms for one `GET /<app>`; of a row's time about 15% is the disk, 20% building the app's filter and the rest running it. At 2 a second that is 1% of a core for 30 apps, 6% for 200; a burst of 60 at 200 apps is 1.9 s of a core | 2 | 5 | 3 | fix | the newest record alone (`TestNewestDeploySummaryIsTheListsFirst` holds it to the full list's first); the rate per person; the list is as long as the config's apps. What a cache would add is an accepted residual below |
| 21 | the apps list, `GET /<app>` | an app whose env_file holds its own name (`DB_NAME=<app>`) is told withheld in the list, and `GET /<app>` answers `"app":"[redacted:DB_NAME]"`, which a client checking the answer names the app refuses | 3 | 5 | 3 | fix | the row's summary alone passes the filter and the name is put beside it after; the filter's rule 5 leaves a value that is the name as written in a JSON body's own `"app"` pair, and replaces it everywhere else, in plain text too; another value's encoding equal to the name is replaced in the pair as well (`holdOwnName`); `TestAppOwnNameInClear`, `TestHoldOwnName` |
| 22 | `deploying` in `GET /<app>` | placed after `last_deploy`, or holding a `"phase"`, so `deploy.sh` reports the wrong phase for a failed deploy | 3 | 5 | 3 | fix | before `deploys` in `wire.Status`; a failure body is written after it is cleared; `TestFailureBodyPhaseWithADeployInFlight` runs `deploy.sh`'s own `field` on a real failure body, with a deploy in flight and without; `status-every-field.json` |
| 23 | `deploying` in `GET /<app>` | a read during a deploy's end shows neither the deploy nor its outcome, or both, or a deploy that ended | 3 | 5 | 3 | fix | the outcome, the phases and `deploying` change in one step under the app's lock; a push refused before its upload is never shown, and one whose upload is not staged is cleared before its answer; `endDeploy` clears what is left before it lets go of the lock, never after, when it would clear the next deploy's; no answer of a deploy holds its own `deploying`; `TestDeployingOneStepToItsOutcome`, `TestDeployingGoneWhenTheEndIsTold`, `TestDeployingGoneFromAPushsResponses`, `TestDeployingGoneBeforeTheLockIsLetGo`, `TestDeployingBeginIsUnderTheLock`, `TestDeployingUnderConcurrentReads` under `-race`, `TestDeployingAPushFromItsUpload`, `TestDeployingASecondDeployRefused` |
| 24 | `deploying` in `GET /<app>` | a value in it unfiltered (a version or deployer holding an env value), or a git-SHA version masked as a token | 5 | 5 | 5 | fix | it is in the status, so the app's filter; the version is a safe string as the others the status names; `TestDeployingFiltered` |

### Accepted residuals

| Residual | S | O | D | Size, risk of a fix | Why accepted |
|---|---|---|---|---|---|
| A removed person reads until the reload that applies the push removing their lines | 2 | 5 | 1 | — | the reload follows the push; a deny list would be state for that window |
| A person's lines edited at the console are seen at the next reload, not at once | 2 | 3 | 3 | 1, 1 | as for every other line of the file; the operator reloads after an edit |
| A grant on an app named through a placeholder loads (the module sees the name resolved), and leaves an installed file the walk refuses: no person is admitted and every push is refused until root mends it, with a journal line at each load saying why | 2 | 2 | 1 | 2, 2 | the walk names the line; refusing placeholders in the `liveswap` block's app names at load is a change to liveswap's grammar, not this one's |
| A deploy token of one app (not a global source) under `/_motel/` gets the flat 401, not the 403 naming a declared key | 1 | 1 | 1 | 2, 2 | judging it against every app's sources would let a stranger buy one verification per app; no CI asks who anyone is |
| `whoami` lists the grants, not a `signer` line's role | 1 | 2 | 1 | 1, 1 | a signer is not a login here; adding a value to `roles` later breaks no client |
| `motel validate` on a machine with no installed file, of a config with people, warns that no person is admitted from the Caddyfile this machine runs | 1 | 5 | 1 | 1, 1 | true of that machine, and the line says it is about that machine's file |
| A refusal by lookup and one past it still differ by the work around the signature check (a hash, the key's parse) | 1 | 2 | 5 | 2, 2 | a stranger learns at most that difference, not a signature check's; constant time would need the whole path padded |
| The time of a refusal tells a stranger which key types the box lists (a type no listed key has costs no verification) | 1 | 3 | 5 | — | it says nothing of who; hiding it would mean verifying every type for anyone, which is the cost row 16 removes |
| A box too old for people and a key the box does not list both answer the flat 401 | 2 | 5 | 3 | — | the owner's decision: no answer names motel in a header; the client's message names both |
| A legitimate burst over 60 requests is told to wait | 2 | 3 | 1 | — | `Retry-After` names the wait |
| The rate table holds more than its 2,048 names while more than that have each asked in the last 30 s: only buckets that have refilled are dropped | 1 | 5 | 5 | 2, 2 | a load declares at most 1,024 people and only a verified token of a declared name adds a bucket, so it takes loads that replace every person in use, twice in 30 s; dropping a bucket in use would refill a throttled person. A bucket can be dropped once 30 s have passed since its name last asked |
| A person whose only key is ECDSA (for example from an agent that makes only P-256 keys) cannot be declared until a later release admits that type | 2 | 2 | 1 | — | the owner's decision of 2026-11-15: a stranger presenting a listed public key buys one check of its type, 0.04 ms for Ed25519 against 0.5 to 1.3 ms for the larger ECDSA curves, and widening later breaks no one where narrowing would; the line's refusal names the remedy |
| A token sent by hand over plain HTTP, to a site the operator serves in clear under the box's name, is answered like any other; no webhook checks that a request came over TLS | 3 | 1 | 3 | 2, 2 | the owner's decision: a hand-made plain-HTTP request is not catered for. `motelctl` speaks https only and follows no redirect; `deploy.sh` refuses an `http://` address unless `MOTEL_ALLOW_HTTP=1`, for a test box; the template's deploy host is served over https |
| On a box with some 200 apps, a person at the rate can spend several percent of a core on `GET /_motel/apps` (6% measured, row 20); a short-lived cache of each app's filtered row would cut it | 1 | 5 | 3 | 3, 2 | the answers are right and nothing else waits on them: a cost, bounded by the rate per person. A cache is a new state to keep in step with deploys and the filter's known values; not built until a box that size exists (the owner's decision of 2026-11-16) |
| A list row's `newest_deploy` can name the deploy before the newest for one read, when a record is rewritten between the row's stat of the records and its read (a deploy or a rollback under way) | 1 | 5 | 3 | 2, 2 | the row is a moment's reading, as two requests a moment apart are; a record is replaced whole, so what is read is one record, never a mix, and the next read (a client polls) orders it rightly. Ordering by the time read from the open file means opening every record, which `newestDeploySummary` exists not to do (row 20) |
| Between a push taking the deploy lock and being shown in flight (`beginDeploy`, after the app's dirs pass `writeRefusal`, a few file checks), a status read shows no deploy in flight while a second deploy is already told 409 | 1 | 5 | 3 | 1, 2 | it lasts microseconds and misleads nobody for longer: the next read shows the push. Showing it before `writeRefusal` would need that refusal's answer to clear it first, one more way out to keep in step for no reader's benefit |
| A deploy by URL or a rollback whose app directories fail `writeRefusal` is shown in flight from its start to that refusal (a few file checks), where a push refused the same way is never shown: `deployLocked` marks the deploy in flight where it logs `deploy started` and sends the lifecycle's start, before its first check | 1 | 1 | 3 | 1, 2 | `deploying` says what the journal and the lifecycle say, that the deploy started; it is cleared before the refusal is answered, so no answer of the deploy holds it. Marking it after the check would have the status deny a deploy the journal has already announced. It takes app directories that are not as motel made them |
| A status read at `GET /_motel/<app>` with the app's deploy token, which named `<app>` before this release as any last path segment does, gets the flat 401 (a global source's token: 404), as row 14's `POST` there does; and the box channel's token under `/_motel/` gets the flat 401, not a 403 that names a declared key (the 403 only when its source is a global liveswap source too) | 2 | 5 | 1 | 2, 2 | as row 14: no template, doc or client asks there, and the answer is loud. Judging a token under `/_motel/` against every app's sources, or the box's, would let a stranger buy a verification per source |

## Not in this release

Any act by a person; a second role for people; every box-level read but
the box's status, who am I and the list of apps; the nonce store; the
announcements of a key's first use from a new network and of a valid
signature from outside every address list; people on a box with no
`box` block or no `box_webhook` site, or on any name but the box's
main one; RSA keys for people; a push's result for a viewer of the box.
