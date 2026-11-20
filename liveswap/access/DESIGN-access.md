# access: people, the token they sign, and who may take which action

`access` is how a person, not CI, reaches a box from a laptop. A person
is declared once in the `box` block of the Caddyfile with their ssh key
(`person` and `grant` lines, box/DESIGN-box.md "People and grants"), is
given the `viewer` role on one app or on the whole box, and signs a
short token with their own key, which the client sends over HTTPS to
the box's own name. ssh stays root's. One function, `Decide`, says on
every request either webhook serves whether the caller may take the
route's action.

This release gives people reads only: an app's status, deploys and
alerts, the box's status, backups, and every alert and silence, who
they are here, and which apps they may view. No
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
| The way in | `AdmitPerson`: the checker with the registry as its lookup, the address list, the failure budgets, the record of networks, the rate | `liveswap/people.go`, called by both webhooks |
| The record of networks | the networks each key was used from, kept in `/var/lib/motel/people-networks.json`, and the two announcements, through `Announcer`, which notify installs | `liveswap/people_networks.go` ("A key used from somewhere new") |
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
never refused in that moment. What it reads may be: an app's status,
its deploys and its row in the list come from the app `Start` installs,
and until then the pooled app holds only its name. So a read `Decide`
allowed waits for the install as a deploy token does, under the same
5 s (one bound for all the apps a list names), and past it is told the
same 503; who am I, the box's status and every refusal are answered at
once. `TestPersonNotRefusedAfterAStart` provisions a whole config,
starts nothing, and shows a deploy token and a person's read of an app
held and then told to come back, a stranger refused at once, and a
person's whoami answered at that moment.

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
`moteld validate` of a candidate, it is still this machine's. Deploys
are not touched by any of it. On a box that admits no one (a registry
that lists no key, or none) neither webhook looks for the `Motel`
scheme: a request in it takes the deploy token's path, and a `Motel`
header alone is answered and journalled as a request with no bearer
token ("How a request is judged"). So on a box whose config declares
people and that admits no one, a person's request is journalled `no
bearer token in Authorization header`, not as a person's refusal; why
no one is admitted is the line the load writes, in every case above
but the last. In the last, when the running config declares people and
the installed file declares no one (a config loaded other than from
that file), the load writes no line.

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
An `ssh-rsa` signer key is no person's key. No line takes an Ed25519
key of small order: `ParsePerson` refuses one on a `person` line,
box/proof's `ParseSigner` on a `signer` line, with the same check
(`UnusableEd25519`). The lookup never returns a nil key as listed.

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

On a box that admits someone, a request is a person's only by its
scheme: such a request never meets a deploy token's sources, and a
deploy token never meets `CheckToken`. A `Motel` header beside a
`Bearer` one is refused at check 2. On a box that admits no one the
scheme is not looked for, and the deploy token's path reads the first
`Authorization` header alone, as it always did: a `Bearer` one first
is judged as that deploy token, and a `Motel` one first is no deploy
token, the flat 401.

## How a request is judged

A box that admits no one (its registry lists no key: "What admits no
one") does not look for the scheme at all. On either webhook a request
in the `Motel` scheme there takes the deploy token's path, which reads
the first `Authorization` header alone (under the token table): a
`Motel` header alone gets the answer and the journal reason of a
request with no bearer token.

On a box that admits someone, a request in the `Motel` scheme goes
through `AdmitPerson`, in this order:

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
   the key, the person, the address and the peer; the operator's
   channels are told, at most once an hour per key
   (`person_outside_list`, "A key used from somewhere new"). A person
   with no grant at all goes on: they may ask who they are.
3. The address's failures are forgotten, as for an accepted deploy
   token.
4. **The network**: the request's network is recorded as one this key
   was used from, and told to the operator's channels when it is new
   (`person_new_network`). It never refuses or waits.
5. **The rate**: 429 past the person's budget.
6. `Decide`, then the route's function.

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
| `GET /_motel/backups` | `box.backups` | a viewer of the box |
| `GET /_motel/apps/<app>/alerts` | `app.alerts`, on `<app>` | a viewer of that app or of the box |
| `GET /_motel/alerts` | `box.alerts`, and `app.alerts` for each row of an app this config runs | a viewer of the box |
| `GET /_motel/apps/<app>/rules` | `app.rules`, on `<app>` | a viewer of that app or of the box |
| `GET /_motel/rules` | `box.rules` | a viewer of the box |
| any other path, method or query under `/_motel/` | none | 404 once authenticated, the flat 401 before; but a query on an app-scoped row is its 422, once `Decide` allowed it and the app is one here |

**App-scoped rows.** A row of the form `GET /_motel/apps/<app>/<read>`
(`motel_approutes.go`) is a read about one app. The path is taken as
sent and must be exactly that: the app segment is held to the app
grammar before anything else, and a segment more, less, empty or
dotted is no row, the 404. The row's action is an `app.*` action on
the app the path names, and its function is given the app's handle
from `allowApp`, never the name: `Decide` comes before the app is
looked for, so a person whose grants do not cover the name is told the
same 403 whether it is an app here or not, and only a viewer of the
box is told 404 for a name that is no app, as on `GET /<app>`. Then the
query: no row takes one yet, so any query (an empty `?` and one that
does not parse among them) is a 422, `GET /_motel/apps/{app}/<read>
takes no query`, which echoes nothing of the request. A row that comes
to take a key declares it beside its read, with its closed set or
grammar, and the query is then parsed strictly against them, an unknown
key still the 422, naming the keys the row takes. Then the read waits
for the app's install as `GET /<app>` does. A global deploy source's
token is told the 403 naming a declared key (`scope` `app`); an app's
own deploy token and the box channel's are the flat 401, as on every
row under `/_motel/`.

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
| `box.whoami` | `GET /_motel/whoami` | 403, needs a declared key, for a token a global source accepts (one that only an app's own sources accept is the flat 401 under `/_motel/`) | the flat 401: liveswap's webhook, which serves `/_motel/`, does not know the box channel's source (the 403 only when it is a global liveswap source too) | any person |
| `box.apps` | `GET /_motel/apps` | as for `box.whoami` | as for `box.whoami` | any person |
| `box.backups` | `GET /_motel/backups` | as for `box.whoami` | as for `box.whoami` | a viewer of the box |
| `box.alerts` | `GET /_motel/alerts` | as for `box.whoami` | as for `box.whoami` | a viewer of the box |
| `app.alerts` | `GET /_motel/apps/<app>/alerts` | 403, needs a declared key, scope `app`, for a token a global source accepts; the flat 401 for one only an app's own sources accept, as under every row of `/_motel/` | as for `box.whoami` | a viewer of that app or of the box |
| `box.rules` | `GET /_motel/rules` | as for `box.whoami` | as for `box.whoami` | a viewer of the box |
| `app.rules` | `GET /_motel/apps/<app>/rules` | as for `app.alerts` | as for `box.whoami` | a viewer of that app or of the box |

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

A box-wide read is made through a handle to the box: `GET
/_motel/backups`'s function reads the backups only through a
`boxHandle`, which only `allowBox` makes, after `Decide` allowed the
action on the box (`TestOneBoxHandleMaker`). Its body names every
app, and is the box's, not one app's: it passes the filter with no
app's values (as the list's frame does: no filter may see two apps'
values), and its shape and entropy layers.

`GET /_motel/alerts` reads notify's source through the box handle too
(`allowBox`, `box.alerts`): every alert Alertmanager holds and every
silence that has not ended, the alerts newest first, the active
silences newest first and then the pending ones soonest first, each
list cut to its cap
(500 alerts, 200 silences, `more_alerts` and `more_silences` saying it
was cut). It is a viewer of the box's alone, for what it holds of the
operator's: a silence's comment and maker are the operator's free
words, and an instance names paths and other sites' hostnames. The
body is about every app, so it is made of rows, as the list is: an
alert whose `app` label names an app this config runs is read through
that app's handle (`allowApp`, `app.alerts`; the box viewer's grant
covers it) and passes that app's filter alone, with the versions its
status names safe, so an env_file value in its summary is masked as on
`GET /_motel/apps/<app>/alerts`, and a row the filter changed has its
own `redacted_env`. Its times (`starts_at`, `silenced_until`) are
Alertmanager's, never the app's bytes: they pass no filter and are put
back after its last pass, so an env_file value that is a piece of one
(a date) cannot leave a time no client reads; every alert row then has
its keys sorted. Every other alert (the box's own, and one whose
label names an app this config does not run) and every silence pass a
filter with no app's values, with the app names, silence ids and
release names they hold safe. A row the filter withholds keeps what
holds the alert and its times, and its app only when the app's own
filter withheld it; its summary is the filter's sentence; it loses its
name and instance. A silence the filter withholds keeps its state, its
times and an id Alertmanager made (a UUID), the filter's sentence as
its comment, and loses its matchers and maker. An app this config runs
whose handle the box viewer is refused (the table and the grants
falling apart) has its rows withheld, never filtered without its
values. The list then adds only its
punctuation and the box's own words (`monitor`, `more_*`).
`TestTwoAppsTwoPeople` holds the row to 403 for every person but the
box's viewer, and to holding every alert, the box's among them, for
that viewer.

`GET /_motel/apps/<app>/alerts` reads the box's whole answer from
Alertmanager (notify's source, below) and keeps the alerts whose `app`
label is the handle's app, in Go, after the fetch: no matcher is built
or sent. The body is about that app alone and passes its filter, as
`GET /<app>` does. `TestTwoAppsTwoPeople` gives each app an alert whose
summary holds a canary of its own, and Alertmanager a box event, which
has no `app` label, whose summary names the first app (as
`backup_stale/<app>`'s does; that such an event carries no label is
notify's `TestHeldAlerts`), and asserts for every caller that no answer holds another app's
alert or the box's, and that each 200 holds its own app's.

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

The rules (`motel_rules.go`) are notify's listing, the one `moteld
monitor rules --json` prints (`wire.RulesListing`), with each store
rule's numbers from the process's one evaluation (notify/DESIGN-metrics.md,
"How close each alert is"), installed as `RulesSource` beside the
alerts' source. `GET /_motel/rules` reads it through the box handle
(`allowBox`, `box.rules`): every rule, with every app's numbers, the
disks' paths and the box's own checks, a viewer of the box's alone. `GET
/_motel/apps/<app>/rules` reads it through the app's handle (`allowApp`,
`app.rules`) and keeps, in Go, the alerts whose subject is an app, every
app's or the handle's own (a rule pinned to another app is left out),
each with the readings of the handle's app alone: no box check and no
other app's number; its `rules_file` word too, since a reading's state is
vmalert's and a file that is not this config's qualifies it. Neither answer holds the
store's socket or a query (`value_query`, a guard's `query` and `match`):
the box's alone. A rules body holds no byte of an app's: the rule names
and thresholds are the Caddyfile's (in the box's repository, not a
secret by policy), the words motel's, the numbers the store's and the
detectors', the times the box's. So both pass a filter with no app's
values, as the backups body does, its shape and entropy layers running
and the alert and app names safe: an env_file value shaped like a date,
a number or a word of the body masks nothing in it, and so cannot leave
an answer no client reads (`TestRulesPassNoAppValue`).
`TestTwoAppsTwoPeople` gives the rules source a rule of every app with
each app's number, a rule pinned to each app and a box rule, and asserts
that an app's read holds its own number and pinned rule alone and the
box's read holds them all.

## The answers

| Answer | When | Body |
|---|---|---|
| 401 | every refusal of a token, a person's or a deploy token's, whatever the reason; a person outside every address list | the flat 401, byte for byte, headers included: `{"error":"invalid or missing deploy token (Authorization: Bearer <jwt>)"}`. No `Motel-*` header |
| 403 | a verified caller `Decide` refuses | `wire.Forbidden`: `{"error":"this needs the viewer role on this app","needs":"viewer","scope":"app"}`; `needs` is `viewer`, `deployer` or `key` (a token a declared key signed), `scope` is `app` or `box`. It names no app and echoes nothing of the request |
| 404 | a viewer of the box asking for a name that is no app (`unknown app "<name>"`, as a global deploy source is told); a path under `/_motel/` with no row (`not found`) | `wire.Error` |
| 429 | a person past their rate | `{"error":"too many requests; retry after <n> s"}`, with `Retry-After` |
| 503 | a deploy token that verified for an app liveswap's `Start` has not installed within 5 s, or whose request ended first (#473); a person's read of such an app, or a list that names one | `{"error":"motel is starting and has not loaded this app yet; retry after 1 s"}`, with `Retry-After: 1` |
| 200 | `GET /<app>` | `wire.Status`, the body a deploy token reads too. `deploying` is the deploy in flight: `version`, `deployed_by`, `started_at` (when the box took it: for a push, before its upload) and `phases`, those entered so far in order, the last one's `seconds` running to the answer; absent when none runs. It is set when the box takes a deploy (a rollback and a relaunch are deploys; the watchdog's restarts are not) and cleared in the step that sets `last_deploy`, so a read shows one or the other. It sits before `deploys` and `last_deploy`, and holds no `"phase"` key: the templates' `deploy.sh` reads the failing phase as the last `"phase"` in a failure body |
| 200 | `GET /_motel/whoami` | `wire.WhoAmI`: `name`, `key` (its fingerprint), `roles` (the grants that apply from here, no address list shown), `box`, `version`, `protocol`, `token_expires_at` |
| 200 | `GET /_motel/backups` | `wire.Backups`, which is `backups/status`'s `Result`: what `motel-backup status --json` says as any account on the box looks, the motel user's here (no warnings of the credential file, which only root's look reads), whether the backups are healthy, not, or could not be told (`could_not_tell`: on a box without backups, `backups are not set up: … is not there`; on a server that is not moteld, that it does not read them). One look every 10 s at most, shared by every person: a request in that time is given the answer kept, whose `at` says when it was decided. The box's own claim (docs/backups.md, "Look from your own machine") |
| 200 | `GET /_motel/alerts` | `wire.BoxAlerts`: `monitor` (as for an app's), `alerts`, newest first, each a `wire.BoxAlert`: an app's alert's fields, then `app` (its `app` label; none for a box event, which names an app, if any, in its instance) and `instance`; `silences`, the active ones newest first, then the pending ones soonest first, each a `wire.Silence`: `id`, `state` (`active` or `pending`; never an expired one), `matchers` (`name`, `op`, one of `=`, `!=`, `=~`, `!~` spelled from Alertmanager's `isRegex` and `isEqual`, `value`), `starts_at`, `ends_at`, `created_by` and `comment`, as Alertmanager holds them; and `more_alerts`, `more_silences` when a list was cut at its cap (500, 200). With `off` or `unanswered`, both lists are `[]`. Every alert row has its keys sorted, and an app's its own `redacted_env` when its filter changed it; its times are Alertmanager's, put back after the filter's last pass, so an env_file value that is a piece of a time (a date) cannot leave the list unread. Not the heartbeat, the test alert, the deploy mute, nor `person_new_network` and `person_outside_list` ("A key used from somewhere new"), which name people's networks and addresses and go to the operator's channels alone. From the same fetch as an app's, up to 5 s old |
| 200 | `GET /_motel/apps/<app>/alerts` | `wire.Alerts`: `monitor`, then `alerts` (a body the app's filter changed comes with its keys sorted and its own `redacted_env`, as `GET /<app>`'s does), the alerts newest first, each a `wire.Alert`: `name` (its alertname), `severity`, `summary` (motel's sentence, through the app's filter), `starts_at`, `state` (Alertmanager's: `active`, `suppressed`, `unprocessed`), `silenced_until` (when the last silence that suppresses it ends, when the box knows it) and `inhibited` (the deploy mute holds it). `monitor` is `on` (Alertmanager answered), `off` (the config's monitor block runs no notify, or there is none: nothing is raised) or `unanswered` (Alertmanager did not answer within 2 s), the last two with `[]`. Not the silences, their comments or creators; not the box's own alerts, even one naming the app in its instance; not the heartbeat, the test alert or the deploy mute. One fetch of Alertmanager every 5 s at most for the process, shared by every person: an answer can be up to 5 s old |
| 200 | `GET /_motel/rules` | `wire.Rules`: `store`, `on` (the store answered the box's evaluation: each store rule's `readings` are its numbers, maybe none), `off` (no `metrics` line: no store rule is listed; with no monitor block that raises anything, no rule at all) or `unanswered` (no store rule has readings); `rules_file` as `moteld monitor rules --json` has it; `alerts`, every alert motel can raise, each a `wire.AlertListing` as the command lists it but with no `value_query` and no guard `query` or `match`: `name`, `source`, `severity`, `subject`, `app`, `kind`, `op`, `threshold`, `unit`, `for_seconds`, `guards` (`op`, `value`, `meaning`), `levels`, `readings` (`instance` or `app`, `value`, `threshold`, `window_seconds`, `level`, `measured_at`, and a store rule's `state`, `inactive`, `pending` or `firing`, and `active_since`). A client reads a `store` or `state` word it does not know as not knowing. The store's numbers from one evaluation every 30 s at most for the process: up to 30 s old, of the time vmalert compared (about a minute behind). Experimental |
| 200 | `GET /_motel/apps/<app>/rules` | `wire.Rules`, as above, holding the alerts whose subject is an app, every app's or this one's, each with this app's readings alone, and `rules_file` as the box's read has it |
| 500 | `GET /_motel/rules`, `GET /_motel/apps/<app>/rules`, when the box could not make its listing (vmalert's unit unreadable) | `{"error":"the box could not list its rules: moteld's journal says why"}` |
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
and nothing tells moteld's own 5xx from a gateway's, so the client
says every 5xx as not known to be moteld's (client/README.md).

## Safeguards

**Keys by fingerprint.** One parse of the structure, one hash, one map
lookup, then one signature check: a refusal costs the same whatever the
number of keys. On a box that admits no one a stranger's token costs
what junk of its length in another scheme costs: the scheme is not
looked for and the token is not parsed. A stranger can put a listed public key in the structure and buy one
signature check, as a JWT under a known `kid` does; the
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
which the client writes, so a client could name any address:
motel's gate refuses a grant's `from` list on a box with a server that
trusts proxies (`trusted_proxies`, or `trusted_proxies_unix`) without
it, and the serving process matches no list for a request such a proxy
forwarded. With it, `moteld validate` warns, naming the headers the
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
| `moteld validate` | refused: an `Error`, exit 1 |
| `moteld reload` (and `systemctl reload moteld`) | refused before it reaches the server: nothing is loaded, the running config stays |
| a signed push | refused at the handler's validate (it runs `moteld validate`): the push's run is red, nothing installed |
| `moteld run` (a start, a boot) | an `ERROR` line, and served all the same, as for the gate's other refusals; the handler fails closed: no list holds a forwarded request |
| the admin socket, by hand | no gate: loaded, and the handler fails closed as above |

**A box with people and no monitor channel** is allowed, with a warning
from `moteld validate` and in the journal at each load: a key used from
a new network, or from outside every address list, is then a journal
line only ("A key used from somewhere new").

## A key used from somewhere new

Two one-shot events tell the operator's channels that a person's key is
in someone's hands where it was not before, the sign of a key stolen
(T7, T8 in DESIGN-threat-model.md). They announce and never gate: a
read goes on as before, whatever the record says or fails at (the
owner's decision; no waiting period, no 403 of its own).

| | |
|---|---|
| A network | the request's address as the address lists read it (`clientAddr`: Caddy's `client_ip`, IPv4 written as IPv6 taken as IPv4, no zone), cut to a /24 for IPv4 or a /48 for IPv6. An address the box could not read (a request a proxy in `trusted_proxies` forwarded, on a server without `trusted_proxies_strict`, or one that does not parse) is one network of its own, told as "an address the box could not read" |
| New | the key was never accepted from that network, as far as the record goes. A key's first use the box knows of is a new network, told as its first |
| Counted | only a request whose token verified (checks 1 to 13) and that the address list let through: after `Limiter.Admit` and before the rate, so a throttled request counts as a use. Reads count: a thief reads first. Nothing a stranger sends reaches the record (`TestOnlyAVerifiedTokenCounts`; `FuzzNoRouteNoAnswer` holds every input to it) |
| `person_new_network` | a one-shot box event at `warning`: `alice@example.com (key SHA256:…) used this box from 203.0.113.0/24 for the first time`; a key's first use `… used this box for the first time, from 203.0.113.0/24` |
| `person_outside_list` | a one-shot box event at `warning`, for a valid signature from outside every address list on the person's grants (the flat 401): `a valid signature by alice@example.com (key SHA256:…) from 203.0.113.9, outside every address list on their grants; refused`. At most once per key an hour, kept in memory alone |
| How | `liveswap.Announcer`, installed by notify's Provision (`SetAnnouncer`) for the process, as the record is the process's. notify's `peopleSink` raises each as an event of source `access`, its instance the key's fingerprint (`access/box/person_new_network/<fingerprint>`), so notify's limit per key (four at once, then one a quarter-hour) bounds a thief who cycles networks; each held back is a `monitor: event` line with `suppressed`. The summary shows the fingerprint's first 12 characters after `SHA256:`, and the name cut to 48 bytes (on a character's edge), so the longest sentence (an outside-list's, with an IPv6 address) stays under the channels' 200; the alert's `instance` and the journal's line hold both whole. Both, as shown, are safe strings to the summary's filter (`Announcement.Safe`): it masks a fingerprint, which looks generated, otherwise. No person's read over HTTPS holds them: the people's alerts reads leave them out with the heartbeat and the mute (notify's `personAlerts`), so a viewer of the box reads no other person's networks or addresses, and a thief holding a box viewer's key does not learn it was seen. A message held back by the limit is not counted into the next (the notes' "and N more networks since"): the journal has each |
| The journal | every announcement is also a line of its own, whatever notify does: `access: a key used from a new network` (`person`, `key`, `network`, `first_use`, `summary`), `access: a valid signature from outside every address list (told at most once an hour per key)` (`person`, `key`, `address`, `summary`). On a box with no monitor block, or no channel, that line is all there is |
| The record | per key fingerprint, its last 16 networks, the most recently used first; a seventeenth drops the one used longest ago. No time is kept: the order is the record. Keys the newest registry does not list are dropped at the first person's request under it (a registry's `Generation` orders the loads, so a reload's old handlers drop nothing, and a request they still serve for a removed key is neither recorded nor told); a person removed is forgotten, and told as a first use if added back. A load that admits no one leaves the record as it is, until a person is admitted again |
| The file | `/var/lib/motel/people-networks.json`, motel's own, 0600, written by rename (`writeOwnFile`). `{"version":1,"keys":{"SHA256:…":["203.0.113.0/24","2001:db8:5::/48","unknown"]}}`. A new network or a key dropped is written within a second of the last write (so a thief cycling networks has it written once a second at most), in the background, one write at a time; a change of order alone at most once a minute; and liveswap's `Stop` and the box app's (a stop, an upgrade's restart, a reload's old config; the box app's for a config with no liveswap block) write what is pending, waiting for it at most 2 s (`FlushPeopleNetworks`). Named by moteld's main (`KeepPeopleNetworksIn`), so a test's handlers keep the record in memory alone |

**The record never touches the disk on a request's path.** The file is read in
the background at the first person's request a process admits. Until it
has been read, or for 30 s if the read does not come back, uses are
recorded and their announcements held (up to 64 networks a key, not
16, so a burst after a start is told whole; at the load each key keeps
16 again, and one told then and cut is told again at its next use),
then told for each network the file did not hold. A write runs on its timer's goroutine, outside
the lock a request takes. (A request from a new network hands its
announcement to notify's engine, under the engine's lock, which notify
also holds while it writes its producers' file: an accepted residual
below.) Each fault ends right, or said:

| Fault | What happens |
|---|---|
| no file (the first start with people, the first after the upgrade) | `access: no record yet …` at information; each key's next use is told, as its first |
| a file that does not parse (one a power loss emptied: there is no fsync), of a newer version, of none, a link at the name, larger than 1 MiB, or that cannot be read | one warning naming the file and why; moteld starts afresh and writes over it; each key's next use is told again as its first |
| a read that does not come back within 30 s | the same warning (`it was not read within 30s`); the held uses are told, and the file is written over with what memory holds then: the read that comes back later is dropped, and each other key's next use is told again as its first |
| a write that fails (a full disk, `EIO` at the rename) | one warning until a write succeeds again, then an information line; the record stays right in memory and is tried again each minute, no sooner for a new network, nor for a stop, which then waits for nothing; a restart before it is written tells again the networks used since |
| a stop, a restart, a crash | a stop writes what is pending first, and waits for a read of the file under way (so the uses it holds are told and written), at most 2 s in all; a crash loses what was due: a change of order (the next use moves it again), a new network of the last second, told again at its next use, and the uses held by a read under way, told at their next use |
| two requests at once, on either webhook | one lock; each network told once (`TestNetworksConcurrent`, under `-race`) |
| the clock stepping | nothing on disk holds a time; the hour of `person_outside_list` is on the monotonic clock |

**The cost on every admitted request**, measured (go1.27.3, arm64,
one core, `BenchmarkPersonGate`, `BenchmarkNetworksSeen`, two runs of
three on a shared machine): for a network the key was used from, 18 to
21 ns and no allocation, against 51 to 54 µs for `AdmitPerson` as a
whole (the token's signature check is most of it) and about 0.8 ms for
a status read; a request from a new network adds 0.5 to 1.3 µs here,
and about 34 µs more in notify's engine, whose filter runs over the
summary (a scratch benchmark of `peopleSink`, not kept).

**Behind proxies** trusted without `trusted_proxies_strict`, every
request such a proxy forwards is the one network the box could not
read: after a key's first use through it, its use through the proxy
from anywhere is not told. `moteld validate` says so for each such
server when people are declared (`ForwardedNetworksWarning`), and the
announcement itself names an address the box could not read. A proxy
the box is not told of (no `trusted_proxies` at all) is one network
too, the proxy's, which motel cannot see; so is a listener on a unix
socket, whose peer has no address (the box cannot read one).

**What is kept, where, for how long.** The file holds, per key
fingerprint, up to 16 networks: never a time, never an address whole.
It stays until the key's lines are removed and a person's next request
comes under that load (with no person left at all, until one is again),
and survives `apt purge`, which keeps
`/var/lib/motel`. The journal and the operator's channels hold, per
announcement, the person's name, the key's fingerprint, and the network
or the whole address; they keep them as long as the journal and the
channels keep anything.

## Upgrade

An upgrade needs nothing: a file without `person` or `grant` lines
loads as before and admits no one. There is no going back to a release
without them: each one is from before the package was `moteld`, and no
`.deb` goes back across that move (docs/upgrading.md).

## Triage

Findings are triaged by the triage skill on its general scales.
access's examples:

| Axis | Examples |
|---|---|
| **Severity 5** | a person reads an app they hold no grant on; a person's token acts, deploys or pushes; a token for one box accepted at another; a stranger's request answered as a person's; a key no line lists accepted |
| **Severity 4** | every person locked out until the console, with no line saying why |
| **Severity 3** | a refusal that tells a stranger something (a key is listed, a name is an app); a grant that grants more or less than `describe` prints; a person told the wrong role in a 403; an announcement that does not come |
| **Severity 2** | a 401 for a valid token, retried and named; a 429 to a person inside their budget; a duplicate announcement |
| **Severity 1** | a journal line's wording |

### The changes of this release, scored

| # | Change | Failure it could cause | S | O | D | Outcome | What holds it |
|---|---|---|---|---|---|---|---|
| 1 | people's reads, by app | a route shows one app's data to another app's person | 5 | 5 | 5 | fix | the app handle only `allowApp` makes; each app's own filter; `TestTwoAppsTwoPeople` over every row and caller |
| 2 | the person token | a viewer's token accepted for an act | 5 | 5 | 5 | fix | a person's token is good on a `GET` alone (check 13); no person's cell acts |
| 3 | the person token | a token for box A accepted at box B | 5 | 5 | 5 | fix | `aud` and `Host` both equal to the identity (checks 9, 10) |
| 4 | the token beside deploy tokens | one kind taken as the other | 5 | 5 | 5 | fix | the scheme picks the checker; two headers refused, on a box that admits someone (on one that admits no one the scheme is not looked for, and the deploy token's path judges the first header alone: row 27); no person's cell holds `app.deploy` |
| 5 | the `Motel` scheme in both webhooks | an existing answer changes | 3 | 5 | 1 | fix | any other request takes today's path; `handler-bodies.txt` passes with additions only; every refusal is the flat 401 byte for byte (`TestPersonRefusalsAreTheFlat401`, both webhooks) |
| 6 | the version on the wire | the release told to strangers, or to every token holder | 2 | 5 | 5 | fix | no header names motel, its protocol or its release, only whoami's body (`TestNoMotelHeaderOnVerifiedAnswers` and `TestReleaseInWhoamiBodyAlone` in liveswap, `TestNoMotelHeaderOnVerifiedAnswers` and `TestNoMotelHeaderOnPersonAnswers` in box) |
| 7 | two readers of the lines | `validate` and the serving process disagree on who is admitted | 3 | 5 | 3 | fix | one parser for both; the registry from the walk alone; a grant on an app the config does not run now fails the load too |
| 8 | the registry from the installed file | the walk refuses it, so no person is admitted | 2 | 5 | 1 | fix | a journal line at each load naming why |
| 9 | the registry after a start | a person refused in #473's window | 2 | 4 | 1 | fix | built at provision; `TestPersonNotRefusedAfterAStart` |
| 10 | the address list on `client_ip` | a client names its own address in `X-Forwarded-For` behind a trusted proxy (Caddy's default, non-strict mode takes the left-most), or too wide a `trusted_proxies` lets a peer set it | 3 | 3 | 5 | fix | without `trusted_proxies_strict`: refused by the gate, and no list holds a forwarded request; with it, `moteld validate` warns that the ranges decide; `TestFromBehindATrustedProxy` drives a forged header through a provisioned server in each mode |
| 15 | the cost of a refusal by lookup | a stranger tells by timing whether a published key is listed | 2 | 2 | 5 | fix | a signature check against a dummy key of the type before `key-not-listed`, for a type some listed key has (`TestUnlistedKeyCostsASignatureCheck`) |
| 16 | that dummy check | a stranger with no key makes any box verify a P-521 signature per request (1.3 ms against 3 µs) | 3 | 3 | 5 | fix | no dummy check for a type no listed key has, nor with no registry (`TestUnlistedTypeCostsNothing`); since 2026-11-15 no ECDSA type at all, refused as `key-type` before any lookup (`TestECDSANeverVerified`) |
| 27 | a person's token on a box that admits no one | the person's path ran on such a box too: a well-formed token was refused 20 to 45 µs later than no token or junk of its length (z 3.0 to 4.4 at 300 samples over loopback, measured twice on a real box), and any request in the `Motel` scheme took a path a release without people does not have, so a stranger tells a release that reads people's tokens from one that does not, on a box with no people | 2 | 5 | 5 | fix: size 1, risk 1; **Wide** (every request to either webhook passes the gate) | both webhooks open a person's path only when the registry lists a key (`Registry.Keys`); otherwise the scheme is not looked for and the request takes the deploy token's path. With no registry and with one listing no key, a well-formed token, junk of its length and short junk get the answer and the journal reason of no `Authorization` header; a deploy token sent first beside a `Motel` header is served as it is alone, and on a box with people the pair is still the flat 401 and an unlisted key still `key-not-listed` (`TestNoOneAdmittedTakesTheDeployPath`, both webhooks) |
| 11 | people with no channel | what they do is told to no one | 3 | 5 | 5 | fix by naming | warned by `validate` and at load |
| 12 | the 403 to people | a 403 for one app tells a person of another that the name exists | 3 | 5 | 5 | fix | one 403 for an app that exists and one that does not |
| 13 | `Host` equal to the identity | a proxy that rewrites `Host` refuses every person | 2 | 5 | 1 | fix | the journal's line names the host received |
| 14 | the `/_motel/` prefix | a deploy to `POST /_motel/<app>`, which deployed `<app>`, gets 401 or 404 | 2 | 5 | 1 | accept | no template or doc posts there; loud |
| 17 | a deploy token after a start (#473) | a valid deploy token refused as a stranger's for some tens of milliseconds after a start, when Caddy starts the http app before liveswap (measured: 10 of 20 restarts) | 2 | 4 | 3 | fix: size 3, risk 1; **Wide** (every deploy and status request runs through it) | until a `Start` installs the app, the token is checked against the config's own sources; a verified one waits for the install, at most 5 s and the request's life, then 503 with `Retry-After`, which `deploy.sh` sends once more; nothing waits before a token verified. Covered: a `local` key's token, and an issuer's whose keys are fetched; the first token under an issuer's key after a start can still be refused once, while the key is fetched, as before (#306's row in DESIGN-liveswap.md), and the script's resend of a 401 covers it. `TestDeployTokenNotRefusedBeforeStart`, `TestDeployTokenBeforeStartPastTheBound` (on a fake clock; `TestInstallWaitIs5s` pins the bound), `TestTrustIsOneRead`, `TestStrangerBeforeStartIsTheFlat401`, `TestInstallWaitEndsWithTheRequest`, `TestReloadBeforeStartAnswersAtOnce`; through a provisioned config, `TestPersonNotRefusedAfterAStart` |
| 18 | the apps list's summary | one app's env value, or its name or socket, in another app's row | 5 | 5 | 5 | fix | a filter per row, never one over the list (`redact.go`, rule 2); each row through `allowApp` for `app.status`; `TestTwoAppsTwoPeople` over each row with a canary in the summary's fields; `TestAppsRowWithheld` |
| 19 | the apps list's summary | the list and `GET /<app>` disagree about an app | 3 | 5 | 3 | fix | the summary is cut from the same status (`summaryOf`), never computed; `TestAppsSummaryIsTheStatus` for an app that serves, one never deployed, one whose newest deploy failed and one throttled |
| 20 | the apps list's summary | a person at the rate on a box of many apps spends the box's CPU. A row reads the app's state in memory and, of the disk, only the newest deploy record (`summaryStatus`), never the release listing or the other records, which `GET /<app>` reads. Measured (go1.27.3, arm64, one core, five records an app, the token's check included): 8.6 KB and 4.8 ms a list for 30 apps, 57 KB and 31 ms for 200, against 0.8 ms for one `GET /<app>`; of a row's time about 15% is the disk, 20% building the app's filter and the rest running it. At 2 a second that is 1% of a core for 30 apps, 6% for 200; a burst of 60 at 200 apps is 1.9 s of a core | 2 | 5 | 3 | fix | the newest record alone (`TestNewestDeploySummaryIsTheListsFirst` holds it to the full list's first); the rate per person; the list is as long as the config's apps. What a cache would add is an accepted residual below |
| 21 | the apps list, `GET /<app>` | an app whose env_file holds its own name (`DB_NAME=<app>`) is told withheld in the list, and `GET /<app>` answers `"app":"[redacted:DB_NAME]"`, which a client checking the answer names the app refuses | 3 | 5 | 3 | fix | the row's summary alone passes the filter and the name is put beside it after; the filter's rule 5 leaves a value that is the name as written in a JSON body's own `"app"` pair, and replaces it everywhere else, in plain text too; another value's encoding equal to the name is replaced in the pair as well (`holdOwnName`); `TestAppOwnNameInClear`, `TestHoldOwnName` |
| 22 | `deploying` in `GET /<app>` | placed after `last_deploy`, or holding a `"phase"`, so `deploy.sh` reports the wrong phase for a failed deploy | 3 | 5 | 3 | fix | before `deploys` in `wire.Status`; a failure body is written after it is cleared; `TestFailureBodyPhaseWithADeployInFlight` runs `deploy.sh`'s own `field` on a real failure body, with a deploy in flight and without; `status-every-field.json` |
| 23 | `deploying` in `GET /<app>` | a read during a deploy's end shows neither the deploy nor its outcome, or both, or a deploy that ended | 3 | 5 | 3 | fix | the outcome, the phases and `deploying` change in one step under the app's lock; a push refused before its upload is never shown, and one whose upload is not staged is cleared before its answer; `endDeploy` clears what is left before it lets go of the lock, never after, when it would clear the next deploy's; no answer of a deploy holds its own `deploying`; `TestDeployingOneStepToItsOutcome`, `TestDeployingGoneWhenTheEndIsTold`, `TestDeployingGoneFromAPushsResponses`, `TestDeployingGoneBeforeTheLockIsLetGo`, `TestDeployingBeginIsUnderTheLock`, `TestDeployingUnderConcurrentReads` under `-race`, `TestDeployingAPushFromItsUpload`, `TestDeployingASecondDeployRefused` |
| 24 | `deploying` in `GET /<app>` | a value in it unfiltered (a version or deployer holding an env value), or a git-SHA version masked as a token | 5 | 5 | 5 | fix | it is in the status, so the app's filter; the version is a safe string as the others the status names; `TestDeployingFiltered` |
| 25 | a person's read after a start | in the moments before liveswap's `Start` installs an app, a person's `GET /<app>` answers 200 from the pooled app, which holds only its name: `idle`, not running, no release, no deploy, for an app that has them; the list's rows say the same, and a deploy's record is a 503 with no `Retry-After`. A deploy token on the same route waits (row 17). Found reviewing the PRs together (#538) | 3 | 4 | 3 | fix: size 2, risk 1 | a read `Decide` allowed waits for the install, one bound of 5 s for every app a list names, then the 503 with `Retry-After` (`awaitRead`); nothing waits before `Decide`, and whoami reads no app. `TestPersonBeforeStartReadsTheInstalledApp`, `TestPersonBeforeStartPastTheBound`, `TestAppsListWaitsOnceForEveryApp`, `TestPersonNotAllowedBeforeStartDoesNotWait`; through a provisioned config, `TestPersonNotRefusedAfterAStart` |
| 26 | a person's read of a deploy's record | the 404 of `GET /<app>?deploy=<version>` named the version asked for through the app's filter, which replaces an `env_file` value wherever a body holds it and names the key: a guess that held a value (as written, inside a longer string, in base64 or hex, a URL value's password) came back `[redacted:DB_PASSWORD]`, and a viewer could try the app's secrets at the person's rate, 2 a second. The 500 of the same read, whose error names the record's file, did the same while a record's file could not be read. A deploy token has met both answers since the route was added (#104); a person who may only read reaching them is this release's. Found answering the owner's question on #538, 2026-11-17 | 5 | 3 | 5 | fix: size 1, risk 1; **Wide** (deploy tokens get these answers too. The 404 changes for them in three cases: a version that holds one of the app's values is shown as sent; a value that is one of the sentence's own words no longer stands as a marker in it; and it is not withheld while the app's `env_file` does not read. The 500 no longer carries the system's error, which is in the journal, and is not withheld then either) | the 404 is the handler's words and the request's own text, nothing of the app's, and passes a filter that knows no value, as the 422 of a query that does not parse does (`redact.go`, rule 6; the owner's choice: what a caller sent is never run past the app's values, as a body's rule and never a string's). The 500 says one fixed sentence: with its error, which may name the app's directory, and a value may equal that, it was no such body. `TestRecordNotReadTellsNothingOfTheEnv`: each form of guess, for a viewer and a deploy token, answered in the 404 as an app with none of the values answers it; the 500 the same bytes whatever is asked, and its error in the journal; a version an earlier deploy stored still replaced in the status, the record and a deploy's refusal. `TestRecordNotFoundIsThePlainSentence`: a value that is a word of the sentence, a version shaped like a token, and, for the 404 and the 422, an `env_file` that does not read |
| 28 | the app-scoped rows under `/_motel/apps/<app>/` | a person reads an app their grants do not cover there, or learns from the answer that a name is an app | 5 | 5 | 5 | fix | the app segment held to the grammar first; `Decide` before the app is looked for, the same 403 for an app and a name that is no app, the 404 to a viewer of the box alone; the function given the handle `allowApp` makes; `TestAlertsRoute`, `TestAlertsRoutePaths`, `TestTwoAppsTwoPeople`, `FuzzNoRouteNoAnswer` |
| 29 | an app's alerts | another app's alert, or a box event that names the app in its instance, in an app's answer | 5 | 5 | 5 | fix | the box's whole answer chosen by the handle's app against each alert's `app` label, in Go, after the fetch; no matcher sent; `TestTwoAppsTwoPeople` with a canary alert per app and a box event naming the first; `TestAlertsChosenByTheHandle` |
| 30 | an app's alerts | one of the app's env values in a summary reaching its viewer | 5 | 5 | 5 | fix | the body passes the app's own filter, the versions its status names safe (`TestAlertsRouteBody`) |
| 31 | an app's alerts | an Alertmanager that does not answer holds the read, or each read asks it | 2 | 4 | 1 | fix | one fetch per 5 s for the process, a failure kept as long, a 2 s budget a caller going away does not cut, `"monitor":"unanswered"` (`TestAlertsView`) |
| 32 | an app's alerts | a query taken loosely, or echoed | 3 | 5 | 3 | fix | no row takes one: any query, an empty `?` and one that does not parse among them, is the 422 in the route's own words (`TestAlertsRouteTakesNoQuery`) |
| 33 | an app's alerts | an `env_file` value that is a piece of a time (a date, `API_VERSION=2026-11-19`) replaced inside an alert's `starts_at` or `silenced_until`, so no client reads the answer; the TUI kept the last answer ("none held") beside "the last read failed". Found by the alerts-screen PR's review | 3 | 5 | 3 | fix: size 2, risk 1 | the times pass no filter: taken out of each alert before the app's filter and put back after its last pass (`filterAlerts`; the alerts-screen PR's box-wide rows do the same), and a body the filter withheld or whose structure it changed (a value spanning two alerts) sent whole through the filter as before (`TestAlertsFilterFallsBack`); the app's screen says why a read failed beside the answer it keeps (`TestAlertsTimesPassNoFilter`, `TestAlertLines`). `GET /<app>`'s `last_deploy`, `deploying`, `watchdog` and `metrics` times go through its filter as they always have |
| 34 | the rules reads | a rule pinned to another app, another app's number, or a box check (a disk's path, the backups' ages naming every app) in an app's answer | 5 | 5 | 5 | fix | the listing chosen in Go by the handle's app (`appHandle.rules`): subject an app, every app's or the handle's (an event pinned to apps, `post_deploy_regressed` of pinned `post_deploy` lines, kept for those apps alone), readings of the handle's app alone; `TestAppRulesBody`, `TestRulesChosenByTheHandle`, `TestTwoAppsTwoPeople` with a number and a pinned rule per app and a box rule |
| 35 | the rules reads | the store's socket or MetricsQL reaching a person | 2 | 5 | 5 | fix | `peopleRules` leaves out `store`'s object and every `value_query`, guard `query` and `match`, in a type (`wire.Rules`) with no field for the socket; `TestRulesBody`, `TestAppRulesBody` |
| 36 | the rules reads | an env_file value shaped like a date, a number or a word of the body (`API_VERSION=2026-11-13`, `RATIO=0.0612346`, `STATE=inactive`) replaced inside it, so no client reads the answer (row 33's failure) | 3 | 5 | 3 | fix | the body holds no byte of an app's, so it passes a filter with no app's values (`respondRules`); `TestRulesPassNoAppValue` |
| 37 | the rules reads | the store loaded by evaluation at the rate, or a read held on a store that does not answer | 2 | 5 | 3 | fix | one evaluation per 30 s for the process, shared by the admin route and every person, four queries at once, within 2 s a reader going away does not cut; a failure kept as long; `TestRulesViewShared`, `TestRulesViewBudget`, `TestEvaluateFailsWhole` |
| 38 | the rules reads | a query taken loosely, or a path trick reaching the row | 3 | 5 | 3 | fix | the rows of #581's alerts: any query on the app row is the 422, on the box row the 404; `TestRulesRoute`, `TestAppRulesRoute`, `FuzzNoRouteNoAnswer` |
| 39 | the announcements | a request without a valid token records a network or is told: a stranger fills a key's 16 networks, or has the operator's channel name a person | 3 | 5 | 5 | fix: size 1, risk 1 | the record is reached after the token's checks and the address list alone; `TestOnlyAVerifiedTokenCounts` (every kind of refusal, from many addresses, and a person's `POST`), `FuzzNoRouteNoAnswer` (only a person's `GET` is counted, whatever the method, path and query) |
| 40 | the announcements | the bookkeeping holds or refuses a person's read: a read of the file under the lock a request takes, and a disk that hangs holds every person | 4 | 3 | 5 | fix: size 3, risk 2; **Wide** (every admitted request) | no I/O on a request's path: the file read in the background at the first person's request, the uses held until it is read or 30 s have passed, the writes on their timer's goroutine; `TestNetworksFileHangs` (fifty requests while the read hangs, the held use told past the wait, the late read dropped) |
| 41 | the record on disk | a crash or a restart inside the minute tells a network again "for the first time"; and a stop (an upgrade) before a due write does the same (found in round 3 of the self-review) | 2 | 4 | 3 | fix: size 2, risk 1 | a new network or a key dropped is written within a second; a change of order alone waits its minute (`TestNetworksReorderWithinTheMinute`); liveswap's `Stop` writes what is pending (`TestNetworksFlushAtStop`); e2e people 7b, ten restarts and each key told once |
| 42 | the record on disk | a write fails (a full disk, `EIO` at the rename): the file falls behind, and a restart tells again what it missed | 2 | 3 | 1 | fix (rule 2): size 2, risk 1 | one warning until a write succeeds, an information line then; memory stays right; tried again each minute and no sooner, so a full disk is not written to at every new network (`TestNetworksWriteFails`) |
| 43 | the record on disk | no file (the first start, the upgrade): every key's next use is told as its first | 2 | 5 | 1 | fix by naming: size 1, risk 1 | an information line; docs/upgrading.md says it before the install (`TestNetworksFileMissing`) |
| 44 | the record on disk | a file that does not parse, empty after a power loss, of a newer version or none, a link, too large, or unreadable | 2 | 4 | 1 | fix (rule 2): size 1, risk 1 | one warning naming why; started afresh and written over by rename (a link is replaced, never followed); `TestNetworksFileUnusable` |
| 45 | the record on disk | a file's content that the reader does not expect panics the read's goroutine, and moteld exits at the first person's request after every start | 4 | 4 | 1 | fix (rule 2): size 1, risk 1 | every entry checked (a network as this release writes it, at most 16 a key, each once); `FuzzNetworksFile` |
| 46 | the record | two requests at once (either webhook, or both) lose a network or tell it twice | 3 | 5 | 5 | fix: size 1, risk 1 | one lock; the announcement made after the change, outside it; `TestNetworksConcurrent` under `-race` |
| 47 | the record | the clock stepping reorders the networks or tells one again | 2 | 3 | 3 | fix: size 1, risk 1 | no time is kept on disk: the order is the record; the hour of `person_outside_list` on the monotonic clock |
| 48 | the announcements | a thief cycling networks floods the operator's channels | 2 | 3 | 1 | fix: size 1, risk 1 | the event's key per fingerprint, under notify's limit per key: four, then one a quarter-hour, each held back a journal line (`TestPeopleSinkRaisesBoxEvents`) |
| 49 | the announcements | behind proxies trusted without `trusted_proxies_strict`, every forwarded request is one network: a stolen key used through the proxy is not told after its first use | 3 | 5 | 3 | fix by naming: size 1, risk 1 | `moteld validate` warns for each such server (`TestWarnPeopleNetworks`); the announcement names "an address the box could not read" (`TestForwardedNotStrictIsUnknown`); docs/people.md |
| 50 | the announcements | an IPv4 address written as IPv6, or an address with a zone, is another network: a duplicate, or a key's 16 filled with one network's spellings | 2 | 5 | 3 | fix: size 1, risk 1 | `networkOf` unmaps and drops the zone, as the address lists compare (`TestNetworkOf`) |
| 51 | the announcements | the summary's filter masks the key's fingerprint (measured: `SHA256:[masked, 43 chars]`), so the operator cannot tell which key; or, with a name cut in the summary, masks part of the name, the uncut one being the safe string. The second found in the self-review | 2 | 5 | 3 | fix: size 1, risk 1 | the name and the fingerprint as the summary shows them are its safe strings (`Announcement.Safe`, notify's `Event.Safe`); `TestAnnouncementSummary`, `TestPeopleSinkRaisesBoxEvents` with an 84-rune name of hex |
| 52 | the announcements | a long name, or an outside-list's IPv6 address, pushes the sentence past the channels' 200 runes and cuts its meaning (found in the self-review: 240 runes) | 2 | 5 | 3 | fix: size 1, risk 1 | the name cut to 48 bytes and the fingerprint to its first 12 characters in the summary, both whole in the journal's line and the fingerprint in the alert's `instance` (`TestAnnouncementSummary`: every sentence, a 256-byte name, the longest address, under 200 with its end whole) |
| 53 | the record | a person removed keeps their networks on disk, and is not told as a first use when added back; or a request a reload's old handlers still serve for the removed key puts it back, told "for the first time" (found in the self-review) | 2 | 5 | 5 | fix: size 2, risk 1 | keys a newer registry does not list are dropped, from memory and the file; under an older registry, a key the newest does not list is neither recorded nor told, and nothing is dropped (`TestNetworksRemovedKeyDropped`, `TestNetworksFileDropsUnlisted`; e2e people 7b) |
| 54 | the announcements | on a box with no monitor block or no channel, or while notify runs no config, an announcement reaches no one | 3 | 5 | 1 | fix by naming: size 1, risk 1 | the journal's own line for each announcement, always; the warning at load and from `moteld validate` (row 11) |
| 55 | the announcements | uses held while the file is read are cut at 16 a key, so a burst from more networks after a start drops some untold | 3 | 2 | 5 | fix: size 1, risk 1 | the cut is 64 a key until the file is read, 16 after (`TestNetworksHeldUntilRead`) |
| 57 | the announcements | every viewer of the box reads both through `GET /_motel/alerts` and the client's alerts screen: other people's names, fingerprints, networks and whole addresses, and a thief holding a box viewer's key learns within seconds that it was seen. Found in round 2 of the self-review, by both reviewers | 3 | 5 | 5 | fix: size 1, risk 1 | left out of every person's read with the heartbeat, the test alert and the mute (notify's `personAlerts`, `heldAlerts`; `TestHeldAlerts`'s answer holds both) |
| 58 | the announcements | with people and no `from` list, a server that trusts proxies strictly, or takes the PROXY protocol, gives the network a thief can name through a proxy that passes on a client's own header, or a `fallback_policy` that takes the header from any peer; `moteld validate` said so only for a `from` list. Found in round 2 of the self-review | 3 | 5 | 5 | fix by naming: size 2, risk 1 | a warning for each such server when people are declared and no grant has a list (`NetworksFromHeadersWarning`, `NetworksFromProxyProtocolWarning`; `TestWarnPeopleNetworks`) |
| 59 | the record on disk | a thief cycling networks, throttled or not, has the file written once a request (found in round 3 of the self-review) | 2 | 3 | 1 | fix: size 1, risk 1 | urgent writes at most once a second (`networksUrgentEvery`; `TestNetworksReorderWithinTheMinute`) |
| 60 | the announcements | an announcement made while no config runs notify (a start's first seconds: Caddy starts its apps in no fixed order, and notify's Start renders before it takes events) is dropped by notify's engine with no line of its own, while the record counts the network as told: the first uses after an upgrade fall there. Found in round 3 of the self-review | 3 | 3 | 3 | fix: size 2, risk 1 | the engine keeps up to 64 of them (source `access`) until a config runs notify, then tells them (`engine.early`; `TestPeopleSinkBeforeNotifyRuns`) |
| 61 | the announcements | a silence of either (its matchers, the key's fingerprint among them, and its comment: "bob travelling") is read by every viewer of the box. Found in round 3 of the self-review | 2 | 5 | 5 | fix: size 1, risk 1 | a silence with an equality matcher on `alertname` naming either is left out of the people's reads (`ofPersonAlert`; `TestHeldAlerts`, `TestHeldSilences`) |
| 62 | `moteld validate` | the forwarded-networks warning beside the refusal of a `from` list behind the same server, naming the same remedy | 1 | 5 | 1 | fix: size 1, risk 1 | given only with no `from` list (`TestWarnPeopleNetworks`) |
| 63 | the record on disk | with writes failing, a stop retried the write every 5 ms for its 2 s, so each reload's old config took 2 s to stop. Found in round 4 of the self-review | 2 | 3 | 3 | fix: size 1, risk 1 | while writes fail, a stop leaves the minute's retry as it is (`TestNetworksFlushAtStop`) |
| 64 | the announcements | notify's engine kept people's announcements whenever no config ran notify, not only at a start, and told days-old ones as now when a monitor block came back. Found in round 4 of the self-review | 2 | 2 | 3 | fix: size 2, risk 1 | each kept at most a minute (`earlyFor`); an older one has its journal line alone (`TestPeopleSinkBeforeNotifyRuns`) |
| 65 | the record on disk | a stop while the file is being read left the uses it held neither told nor written until their next use after the restart. Found by Copilot on #610 | 3 | 2 | 5 | fix: size 1, risk 1 | a stop waits for the read too, within its 2 s (`TestNetworksFlushWaitsForTheRead`) |
| 66 | the announcements | old announcements kept by notify's engine filled its 64 places, so a fresh one in the minute before a config ran notify again was dropped. Found by Copilot on #610 | 3 | 2 | 3 | fix: size 1, risk 1 | the old ones are dropped before the bound is checked (`TestPeopleSinkBeforeNotifyRuns`) |
| 67 | the record on disk | a config with a box block and no liveswap block admits people on `box_webhook`, but only liveswap's `Stop` wrote what was pending. Found by Copilot on #610 | 2 | 4 | 3 | fix: size 1, risk 1 | the box app's `Stop` does it too (`TestStopFlushesPeopleNetworks`) |
| 68 | the announcements | one key cycling networks while no config runs notify filled the 64 places kept, so another key's first use then was dropped from the channels. Found by Copilot on #610 | 3 | 2 | 3 | fix: size 1, risk 1 | at most four kept per key, the limit per key's own bucket (`TestPeopleSinkBeforeNotifyRuns`) |
| 56 | the record on disk | the removal of a temp file a crash left, run beside the read, removes the temp file of a write begun after the wait, which fails and is said (found in the self-review) | 2 | 2 | 1 | fix: size 1, risk 1 | leftovers removed by the writer, before its own temp file, where no other write of this process is under way (`TestNetworksFile`) |

### Accepted residuals

| Residual | S | O | D | Size, risk of a fix | Why accepted |
|---|---|---|---|---|---|
| `GET /_motel/alerts` passes a filter with no app's values over every silence and every box event (an alert with no `app` label, or one naming an app this config does not run), so an app's env_file value in a silence's comment, or in a box event's summary, would reach a viewer of the box unmasked, where an app's own alert masks it | 3 | 1 | 5 | 2, 2 (a silence whose matchers name one app here through that app's filter; a box event cannot be given one) | a comment posted through `moteld monitor silence add` is masked with every value the config knows when it is written (notify, "Silences"), and liveswap masks its events' summaries with the app's filter when it raises them; what is left is text root writes past motel (to Alertmanager's socket, as a producer), and the audience is a viewer of the box, who reads every app. Found in the self-review of the box-wide alerts read (#581) |
| A removed person reads until the reload that applies the push removing their lines | 2 | 5 | 1 | — | the reload follows the push; a deny list would be state for that window |
| A person's lines edited at the console are seen at the next reload, not at once | 2 | 3 | 3 | 1, 1 | as for every other line of the file; the operator reloads after an edit |
| A grant on an app named through a placeholder loads (the module sees the name resolved), and leaves an installed file the walk refuses: no person is admitted and every push is refused until root mends it, with a journal line at each load saying why | 2 | 2 | 1 | 2, 2 | the walk names the line; refusing placeholders in the `liveswap` block's app names at load is a change to liveswap's grammar, not this one's |
| A deploy token of one app (not a global source) under `/_motel/` gets the flat 401, not the 403 naming a declared key | 1 | 1 | 1 | 2, 2 | judging it against every app's sources would let a stranger buy one verification per app; no CI asks who anyone is |
| `whoami` lists the grants, not a `signer` line's role | 1 | 2 | 1 | 1, 1 | a signer is not a login here; adding a value to `roles` later breaks no client |
| `moteld validate` on a machine with no installed file, of a config with people, warns that no person is admitted from the Caddyfile this machine runs | 1 | 5 | 1 | 1, 1 | true of that machine, and the line says it is about that machine's file |
| A refusal by lookup and one past it still differ by the work around the signature check (a hash, the key's parse) | 1 | 2 | 5 | 2, 2 | a stranger learns at most that difference, not a signature check's; constant time would need the whole path padded |
| The time of a refusal tells a stranger which key types the box lists (a type no listed key has costs no verification) | 1 | 3 | 5 | — | it says nothing of who; hiding it would mean verifying every type for anyone, which is the cost row 16 removes |
| A box too old for people and a key the box does not list both answer the flat 401 | 2 | 5 | 3 | — | the owner's decision: no answer names motel in a header; the client's message names both |
| A legitimate burst over 60 requests is told to wait | 2 | 3 | 1 | — | `Retry-After` names the wait |
| A crash in the second between a new network's record and its write tells it again after the restart | 2 | 4 | 3 | 2, 2 (the announcement held until the write) | a single fault, which rule 2 would fix, but the two orders trade a duplicate for a miss: told only after the write, a crash between the write and the telling leaves a network recorded and never told (severity 3), in the journal as well as the channels. The duplicate errs towards telling, and the crash is in the journal beside it. A stop, unlike a crash, writes first (row 41) |
| A key's `person_outside_list` can be told again within the hour after a restart: the hour is kept in memory alone | 2 | 4 | 1 | 3, 2 (the hours in the file) | the second message is true: a valid signature came from outside the lists again. One more line in the file's form, and a time on disk the clock can step, for a rate bound |
| A read of the file slower than 30 s (a disk that all but hangs) is taken as no file: the held uses are told and the file is written over with what memory holds, so every other key's next use is told again as its first. Found in the self-review, rounds 1 and 3; the wait was 5 s, and is 30 s since round 3 (a read of at most a megabyte, on a disk merely busy at boot, comes well within it) | 2 | 2 | 1 | 2, 2 (no write until the late read comes back, then a merge) | it ends red and named: the warning says the record could not be used and that each key's next use is told as new. A deferred write is one more state the next fault (the read never coming back) can reach |
| A request from a new network can wait on notify's engine lock while notify writes its producers' file under it (`external.json`, on a producer's state or a tick), so a disk that hangs there holds that request; a request from a known network does not. Found in the self-review, by both reviewers | 2 | 2 | 3 | 2, 2 (the announcement handed to the engine through a queue its goroutine drains) | it takes two faults: a disk that hangs under notify's state directory, and a key's first request from a network at that moment; that request alone waits, and its retry, from a network now recorded, does not. liveswap's own events (a deploy's) wait on the same lock already |
| With every person's lines removed, the file keeps the fingerprints and their networks until a person is admitted again, since only an admitted request drops keys; and a key removed and added back with no person's request between is told as a new network rather than as its first use. Found in the self-review | 2 | 5 | 1 | 3, 2 (a hook from the box app's load in the serving process alone; the box app has no `Start`, and `moteld validate` provisions it too) | docs/people.md says it, and how to forget them (delete the file); the file is motel's own, 0600, and holds no time or whole address |
| A reload whose http app served a person and then failed (another app's `Start`) leaves the newest registry seen as the failed one: under the restored config, a key that only the failed file removed is neither recorded nor told until the next load that serves. Found in round 2 of the self-review | 3 | 1 | 5 | 2, 2 (the newest registry taken from a serving config, not from a request; or row 53's drop undone, which tells a removed key's last request as its first use) | it takes a load that fails after serving, a person only that load removed, and their use from a new network meanwhile; the failed push or reload is red and named. Row 53's drop is the more common case: every reload has an old config serving beside the new |
| A key's networks told at a load, past the 16 it keeps then (a burst of more than 16 networks before the file was read), are told again at their next use | 2 | 1 | 3 | 2, 2 (a key kept above 16 until those are used again) | it takes a restart and a burst from more than 16 networks of one key in the moment the file is read; a duplicate errs towards telling |
| A write that hangs (a rename that never returns) is never said, no later change is written for the life of the process, and every stop waits its 2 s. Found in round 4 of the self-review | 2 | 1 | 5 | 2, 2 (a deadline on the write, and a line) | a disk in that state holds much else of moteld's (Caddy's certificates, the deploys) before this; the record stays right in memory |
| A thief cycling networks writes two journal lines for each new network (liveswap's, and notify's `suppressed`), at the rate they can send, throttled or not, which journald's limit per service then bounds, dropping moteld's other lines too. Found in round 4 of the self-review | 2 | 2 | 1 | 2, 2 (a limit per key on the lines) | it takes a stolen key and many networks (a large IPv6 allocation, or many IPv4 /24s); the lines are the record the operator wants, and the channels are bounded (row 48) |
| A silence of either alert made by hand with a regular expression on `alertname` (`amtool`, or Alertmanager's API), or with no `alertname`, is listed in the box-wide read with its matchers and comment | 2 | 5 | 5 | 1, 1 | the owner's rule: no guard for a by-hand command no doc asks for. `moteld monitor silence add --alert`, which docs/people.md names, makes the equality matcher the filter leaves out |
| Before the file is read, a key used from A, then B, then A again has B told as its first use, and A as a new network: the held order is the order of last use. Found by Copilot on #610 | 2 | 2 | 3 | 2, 2 (the first held network kept apart from the order) | it takes a key's first uses from two networks in the moment the file is read after a start; both are told, and only which one says "first" differs |
| A stop in the moment a file read is under way, right after a start, in which Caddy stops notify before liveswap: the read's held uses are written but their channel messages go to notify's engine after it stopped, and are lost at exit; the next process takes them as told. Found by Copilot on #610 | 3 | 1 | 3 | 3, 2 (undelivered announcements kept with the record, or the stops ordered) | it takes a stop within the read's moment after a start, and that order of stops; the `access:` journal line of each is written before the announcer is told |
| More than 64 networks of one key in the first moments after a start, before the file is read (normally milliseconds, at most 30 s), drop the earliest untold | 3 | 1 | 5 | 2, 2 | it takes a stolen key, a burst of that size from that many networks, and a start, at once; the 64 networks told still say it |
| The rate table holds more than its 2,048 names while more than that have each asked in the last 30 s: only buckets that have refilled are dropped | 1 | 5 | 5 | 2, 2 | a load declares at most 1,024 people and only a verified token of a declared name adds a bucket, so it takes loads that replace every person in use, twice in 30 s; dropping a bucket in use would refill a throttled person. A bucket can be dropped once 30 s have passed since its name last asked |
| A person whose only key is ECDSA (for example from an agent that makes only P-256 keys) cannot be declared until a later release admits that type | 2 | 2 | 1 | — | the owner's decision of 2026-11-15: a stranger presenting a listed public key buys one check of its type, 0.04 ms for Ed25519 against 0.5 to 1.3 ms for the larger ECDSA curves, and widening later breaks no one where narrowing would; the line's refusal names the remedy |
| A token sent by hand over plain HTTP, to a site the operator serves in clear under the box's name, is answered like any other; no webhook checks that a request came over TLS | 3 | 1 | 3 | 2, 2 | the owner's decision: a hand-made plain-HTTP request is not catered for. The client, `motel`, speaks https only and follows no redirect; `deploy.sh` refuses an `http://` address unless `MOTEL_ALLOW_HTTP=1`, for a test box; the template's deploy host is served over https |
| On a box with some 200 apps, a person at the rate can spend several percent of a core on `GET /_motel/apps` (6% measured, row 20); a short-lived cache of each app's filtered row would cut it | 1 | 5 | 3 | 3, 2 | the answers are right and nothing else waits on them: a cost, bounded by the rate per person. A cache is a new state to keep in step with deploys and the filter's known values; not built until a box that size exists (the owner's decision of 2026-11-16) |
| `GET /_motel/backups` passes the filter with no app's values, so a short value of an app's env_file that the record's text held would reach a viewer of the box unmasked, where `GET /<app>` masks it. The record holds no secret by contract (`backups/record`): what of an app is in it is the paths the Caddyfile declares (not secret by policy) and sqlite3's words about its databases; restic's stay root's. Found in the self-review of the backups read (#581) | 3 | 1 | 5 | 3, 2 (each app's lines through that app's filter, as the list's rows, and a run's own lines through none: a body of rows) | it takes a record holding an app's secret, which only root writes and which by contract holds none; the audience is a viewer of the box, who reads every app |
| The alerts' words pass the app's filter without the times between them, so the filter sees other adjacencies than the bytes sent: an `env_file` value made of JSON's own punctuation and the end of a time (`Z","state":"active`) stands in the answer as written, and one that spans words the times no longer separate (`x","state":"active`) is replaced across a field's edge, leaving an alert with no state, which the client refuses as not moteld's answer. Found reviewing the fix of row 33 | 2 | 5 | 1 | 2, 2 (the known-value pass again over the bytes sent, with the times blanked out) | the first discloses nothing (the value is the answer's own structure and Alertmanager's time) and the second is refused loudly; both take a value written to match JSON's punctuation |
| The box-wide rows of `GET /_motel/alerts` meet the same residual in their own way. Each of an app's alerts passes its app's filter alone, without its times (`alertRow`), and is marshalled with its keys sorted, so the times stood between `severity` and `state`. An `env_file` value made of JSON's own punctuation and the end of a time (`Z","state":"active`) stands in the row as written. One that spans the severity and the state the times no longer separate (`g","state":"active`) is replaced across a field's edge: the row is left with no `state` and with the marker in its severity (`warnin[redacted:K]`), and the client shows it with its state not known and that severity as the box's text, reading the rest of the list. One that breaks the row's structure (`g","state`) has the row sent withheld, its summary the filter's sentence. A row is filtered alone, so no replacement crosses into the next row; a value made of the punctuation the list puts between rows after their last pass (`"},{"app":"blog`) stands as written, as the first does. Only an app's own alerts are concerned: every other row passes a filter that knows no value. Found checking the row above against the box-wide rows | 2 | 5 | 1 | 2, 2 (the known-value pass again over each row as sent, with the times blanked out) | as the row above: the first discloses nothing, and the others show what they are, an alert whose state is not known beside a marker that names the key, or a row withheld with the filter's sentence; all of them take a value written to match JSON's punctuation |
| A summary of an app's alert can name a release as the filter's marker (`[masked, N chars]`) in `GET /_motel/apps/<app>/alerts` where Alertmanager's own copy names it whole: the body passes the app's filter with the versions its status names as safe strings, and a release the alert names (a one-shot's `deploy of <v> failed`, held five minutes) that has left the status by then (no longer on disk, the current one, the last deploy or among the newest records) and is shaped like a token (a 40-hex commit) is masked. Found in the self-review of the alerts read | 2 | 2 | 1 | 2, 2 (the alert's `release` label carried to the route and named safe) | it takes the release to leave every list the status keeps within the alert's five minutes; the marker reads as what it is, and the alert's name and time still say what happened |
| A list row's `newest_deploy` can name the deploy before the newest for one read, when a record is rewritten between the row's stat of the records and its read (a deploy or a rollback under way) | 1 | 5 | 3 | 2, 2 | the row is a moment's reading, as two requests a moment apart are; a record is replaced whole, so what is read is one record, never a mix, and the next read (a client polls) orders it rightly. Ordering by the time read from the open file means opening every record, which `newestDeploySummary` exists not to do (row 20) |
| Between a push taking the deploy lock and being shown in flight (`beginDeploy`, after the app's dirs pass `writeRefusal`, a few file checks), a status read shows no deploy in flight while a second deploy is already told 409 | 1 | 5 | 3 | 1, 2 | it lasts microseconds and misleads nobody for longer: the next read shows the push. Showing it before `writeRefusal` would need that refusal's answer to clear it first, one more way out to keep in step for no reader's benefit |
| A deploy by URL or a rollback whose app directories fail `writeRefusal` is shown in flight from its start to that refusal (a few file checks), where a push refused the same way is never shown: `deployLocked` marks the deploy in flight where it logs `deploy started` and sends the lifecycle's start, before its first check | 1 | 1 | 3 | 1, 2 | `deploying` says what the journal and the lifecycle say, that the deploy started; it is cleared before the refusal is answered, so no answer of the deploy holds it. Marking it after the check would have the status deny a deploy the journal has already announced. It takes app directories that are not as motel made them |
| A status read at `GET /_motel/<app>` with the app's deploy token, which named `<app>` before this release as any last path segment does, gets the flat 401 (a global source's token: 404), as row 14's `POST` there does; and the box channel's token under `/_motel/` gets the flat 401, not a 403 that names a declared key (the 403 only when its source is a global liveswap source too) | 2 | 5 | 1 | 2, 2 | as row 14: no template, doc or client asks there, and the answer is loud. Judging a token under `/_motel/` against every app's sources, or the box's, would let a stranger buy a verification per source |
| A stranger who knows an app's name tells this release from an older one by the time to the 401 for a junk `Bearer` JWT at `/_motel/<app>`, on a box where `<app>` has `deploy_trust` sources of its own, people or not (an app that takes the global sources is judged by the same ones on both): here the path names no app and the token is judged against the global sources (with none, refused before any parse: `no deploy_trust source resolves for this app`); on `main` (0.7.0 and before) `path.Base` makes it `<app>`, whose own sources parse it and, for a JWT that gets that far, check its signature. Measured for this release only (hotserve-1, a box with no global source, at 30f5f12f, 2026-11-17, over loopback, 300 samples a case): against no token, a junk RS256 JWT cost +130 µs (z 10.2) at `/example`, the path `main` gives `/_motel/example`, and nothing measurable at `/_motel/example` (+0 µs, z −0.19) or `/nosuchapp` (−10 µs, z −1.21). 0.7.0's side is still read from the code (`ServeHTTP` on `main`, `authorize`, `deploytrust.Build`): measuring it takes a downgrade | 2 | 5 | 5 | 2, 2; **Wide** (where no global source resolves, a check against a dummy source, on every box's deploy path; where global sources differ from the app's own, more than that) | accepted by the owner's decision (2026-11-17), over the bar: the fix changes the deploy path of a box with no person or grant line. The channel is the timing residual DESIGN-threat-model.md already accepts for the deploy path ("Webhook endpoint", #306): the time to the 401 differs by the local work a name's sources do. What is new is that it tells the release |

## Not in this release

Any act by a person; a second role for people; every box-level read but
the box's status, who am I and the list of apps; the nonce store; a
read held back from a new network (the announcements tell, and never
gate); people on a box with no
`box` block or no `box_webhook` site, or on any name but the box's
main one; RSA keys for people; a push's result for a viewer of the box.
