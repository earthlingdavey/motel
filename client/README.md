# motel

`motel`, the laptop client: reads a box over HTTPS. **Experimental**: every
command, flag, output and exit code may change until a release's notes
say otherwise. The human output is never a contract; `--json` prints the
box's own answer to a read (for `deploys <app>`, the status's `deploys`
array alone, `[]` when it has none; for `logs <app> --follow`, each line
of the app's as the box sent it, one JSON object a line): the same JSON
value, and byte for byte unless the body holds something a terminal acts
on (a line of `logs --follow` loses the white space between its tokens). A server's
encoder writes a C1 control, DEL, a bidi control and the line and
paragraph separators raw in a string: each is printed as JSON's own `\u`
escape, a byte that is not UTF-8 as `\ufffd` (what a JSON reader makes
of it), and a carriage return between values is dropped. `jq` reads the
same value, and a terminal does nothing. `version --json` and
`key --json` print the client's own object.

On a failure, stdout is the box's own body, made safe the same way, on
exit 1 or 6, on exit 5 when the 403's body was JSON, and on exit 9 (the
last status `status --follow` read). On any other
failure (exit 2, 3, 4 or 8, or a 5 whose body was not JSON) it is
`{"error":"…"}` holding the client's own sentence, the one stderr's
last line prints after `motel: `. For exit 1, 2, 3, 4, 6, 8 and 9, the
exit code says which of the two stdout is. For a 5, go by the object:
the client's own has `error` alone, the sentence on stderr; the box's
access check writes `needs` and `scope` beside its `error` (a plainer
403 of the box's is `{"error":"forbidden"}`). An interrupted command
(130) prints nothing on stdout, and nor does a failure of `logs
--follow --json` once the command runs: its stdout holds the app's
lines alone (a refusal by the command line's parser, before the command
runs, still prints its object: the accepted row below).

```text
motel version                       the client's release and protocol
motel key [--key <path>] [--name <you@example.com>]
                                    the key it signs with, and the person line to send the operator
motel whoami                        who the box takes your key to be, and its roles from here
motel apps                          the apps your key may view, one a line
motel status <app> [--range 1h|24h] an app's status
motel status <app> --follow         the deploy in flight, read until it ends; exits by its outcome
motel deploys <app>                 its deploys, newest first
motel deploys <app> <version>       one deploy's whole record
motel logs <app> [--lines 50|200|1000]
                                    an app's own lines, the newest, as the box's journal holds them
motel logs <app> --follow           then the lines after them, read every 2 s until ctrl-c
motel box status                    the commit the box runs, its Caddyfile's digest, its key for secrets,
                                    and whether that Caddyfile is the one that commit installed
motel backup status                 what the box says of its backups, for a viewer of the box: the box's own claim
motel monitor alerts --app <app>    an app's alerts, as the box's Alertmanager holds them
motel monitor alerts                what the box's Alertmanager holds, for a viewer of the box: every alert, and the silences
motel monitor rules --app <app>     the rules that apply to an app, and how close each is, with its numbers alone
motel monitor rules                 every rule the box alerts on, and how close each is, for a viewer of the box
motel metrics <app> [--range 1h|24h|7d|14d]
                                    an app's numbers over time: memory, cpu, requests, 5xx share, p95
motel                               on a terminal, screens over the same reads (below)
```

Later than v0.8.0, and not in a client built from v0.8.0's source:
`status --follow` and following a deploy on an app's screen, which
follow a box at v0.8.0 too; `box status`'s word that the Caddyfile is
not the one its commit installed, which a box at v0.8.0 never sends,
so nothing is said either way; and `backup status`, `monitor alerts`,
`monitor rules`, `metrics` and `logs`, with the box screen's backups line and
the screens' `:backups`, `:alerts`, `:rules` and `:metrics` (`g` on an
app's screen), whose reads a box has on each release after v0.8.0
only. A later client asking a box at v0.8.0 for one of those reads is
answered `404`: the command exits 6, and the screen says the box
answered 404.

Each command takes `--json`; `motel` alone, the screens, does not. `whoami`, `apps`, `backup status`, `monitor alerts`, `monitor rules`, `metrics` and `logs` are asked with your own key
only: with `MOTEL_TOKEN` set, name the key with `--key` (else exit 2,
nothing sent). `<app>` is a name, or the app's URL
(`https://deploy.example.com/blog`, as `MOTEL_URL` holds it), which names
the box too.

`status` says the app's state first (`running`, `throttled`, `stopped`,
`backoff`, `not deployed`) and the version that serves, and while a
deploy is in flight a `deploying` line under it: the version in flight,
its phase once it has one, when the deploy started and by whom
(`deploying    4c2d9e7b1a05 soaking, started 2026-11-19 14:12 UTC  by …`);
its versions line marks the first version with that state when it is
the one serving (`7c1e55d0a913 (running)`).

`status --follow` prints the status, and with a deploy in flight reads
it again every 5 s until the box no longer shows that deploy (the same
version, taken at the same time), printing each phase once as the box
lists it, with the time the box says it began (`14:12:03 UTC  soaking`):
the box lists every phase so far, so none is missed between two reads,
and the last ones come from the outcome's own list. Then it prints the
outcome's record, as `deploys <app> <version>` does (a failure's error,
exit, probe and log tail among it), and what serves (`now     7c1e55d0a913
(running)`), and exits 0 when the deploy succeeded, 9 when it failed,
and 3 when the box's last deploy is not that deploy's: of its version, and
beginning with the phases it was seen in, each begun at the time the
box gave (a push whose upload was never staged leaves none; so does
another deploy that ended after it before the read). With no deploy in
flight it prints the status and says so on stderr, exit 0; a box older
than v0.8.0 in a deploy's phase, which names no deploy in flight,
exits 3. A read that fails ends it, with that read's exit and its
sentence saying the deploy may still run. ctrl-c stops following, not
the deploy, exit 130. With `--json` it prints only the last status
read, the box's own answer, on exit 0 and 9, and on exit 3 the
client's `{"error":"…"}`, as every exit 3 does.

`backup status` prints the box's verdict on its backups first, as the
box's (`<host> says its backups are not healthy (at … UTC)`), then a
row an app (its last run's result, its last complete backup, a restore
proven), the box's own lines, each after its level (`info`, `warning`,
`problem`) and capped, and last that it is the box's own claim, only
as honest as the box: `docs/backups.md` sends the operator to look
from a machine of their own with restic. Its exit is the box's
verdict, as `motel-backup status`'s is: 0 healthy, 1 not (stdout the
box's body under `--json`), 3 could not tell (`could_not_tell`, quoted
in the client's sentence: on a box without backups, `backups are not
set up`).

`monitor alerts --app <app>` reads `GET /_motel/apps/<app>/alerts`, for
a viewer of the app or of the box: the alerts the box's Alertmanager
holds about the app, newest first, each on an `alert` line (its name,
severity, when it began, its state, and when the last silence that holds
it ends or that a deploy of the app holds it) with its summary, the
box's free text, on a `summary` line under it. Not the box's own alerts,
and not the silences. The box asks Alertmanager at most every 5 s, so
an answer can be that old. It exits 0 when it read them, alerts or none,
and when the box has no monitor (nothing is raised there); 3 when the
box could not ask its Alertmanager (`"monitor":"unanswered"`) or names
its monitor in a word the client does not know: `--json` then prints
the client's own sentence. It is asked with your own key only, as
`whoami` is. An alert's name is held to `^[A-Za-z_][A-Za-z0-9_:.-]{0,63}$` (an app's
screen shows one outside motel's own shape, `^[A-Za-z][A-Za-z0-9_]{0,63}$`,
as the box's text)
and its severity, its state and the monitor to `^[a-z][a-z0-9_]{0,31}$`; a
severity or a state the client does not know is shown as the box's
word, not refused.

`monitor alerts` without `--app` reads `GET /_motel/alerts`, for a viewer
of the box, and says how many alerts and silences the box's
Alertmanager holds, then each alert: its severity, what holds it (a
silence, until when; the deploy mute, while a deploy of its app runs)
or that it fires, and since when, in the client's words, then the box's
words on labelled lines of their own (its name, the app its `app` label
names, its instance and its summary), each capped; then each silence
that is active or waits to start: its state and times, its matchers
(each label's name, `=`, `!=`, `=~` or `!~`, the value quoted), who
made it, its comment and its id. An expired silence is not listed, nor
the heartbeat, the test alert or the deploy mute itself, nor the two
alerts about a person's key, which go to the operator's channels alone. Silences are
added and expired as root on the box (`moteld monitor silence`,
docs/monitor.md). It exits 0 when the box answered, whatever fires or
is silenced; on a box whose monitor block sends nothing to anyone, or
that has none, it says the box raises no alerts, exit 0; when the box's Alertmanager did not answer the box in
time (`"monitor":"unanswered"`), or the box names a monitor word this
client does not know, 3; a monitor that is not a lowercase word is not
moteld's answer, 3. A silence's state the client does not know is said
as not known, the box's word quoted, and an operator it does not know
as `?`. The box sends at most 500 alerts, the newest, and 200 silences,
the active ones first and then the soonest to start, and says when it
held more: the first line then says so.

`monitor rules` reads `GET /_motel/rules`, for a viewer of the box, the
remote twin of `moteld monitor rules` without the store's socket and
queries, which are the box's alone; `--app
<app>` reads `GET /_motel/apps/<app>/rules`, for a viewer of the app or
of the box: the rules that apply to it (every app's, and its own `rule`
lines) with its numbers alone, never the box's own checks. The first
line says the box's word for its store (`on`; `off` with no `metrics`
line, or with no monitor that raises anything), and the file vmalert
reads when it is not the config's. Then each rule: a head in the
client's words (its mark, which repeats only a state or a level the box
said: `✖` firing and critical, `▲` firing and a warning, or pending,
`?` a state or a severity the client does not know; its severity, its
kind, its scope: every app, an app, the apps a pinned alert names, or
the box), then on labelled lines under it its name (`rule`, the box's
words), what it fires on (`fires`, the client's words: the op, the
threshold in the rule's unit, `for`), and the box's words (`severity`,
one not motel's; `guard`, each guard's op, value and meaning; `now`,
each number: what it is of, an app or a
filesystem, the number with its age, and for a store rule its state,
`inactive`, `pending` or `firing`, and since when). A share is said as
a percentage, seconds as a duration, bytes in binary units, a unit the
client does not know quoted beside the number; a kind, an op or a
scope it does not know as not known or `?`. The box reads the store's
numbers at most every 30 s for everyone, as vmalert compared them, so a
store rule's number is a minute old or more. It exits 0 when it read
them, with the store `on` or `off`; 3 when the box's store did not
answer it (`"store":"unanswered"`), or the box names its store in a
word the client does not know: the sentence says that the readings of
motel's own checks, which do not need the store, are not printed (the
screens show any: `:rules`, or `:rules <app>` on `--app`), and `--json`
prints that sentence; a store that is not a lowercase word, an app that is not of
the app grammar, and on `--app` a rule or a number of another app, are
not moteld's answer, 3. It is asked with your own key only, as
`whoami` is.

`metrics <app> [--range 1h|24h|7d|14d]` reads `GET
/_motel/apps/<app>/series`, for a viewer of the app or of the box: the
app's numbers over the window (the box's default, the hour, when none is
named), a point a step (a minute over the hour, 5 minutes over the day,
30 over 7 days, an hour over 14), the newest of the last whole step. It
prints the box's window, step and end, then a line a panel it knows
(`memory`, `cpu`, `requests`, `5xx share`, `p95`): its lowest, highest
and newest value, each with its time (the lowest and highest written
alike until they differ, the newest no coarser), and how many of the window's steps have a
point (a rate under 0.001/s written an hour's, `1.0/h`); for the 5xx
share and the p95, beside a requests panel, how
many had requests (a rate above 0), since a step with none has no
point of theirs, and how many have a point when the two differ.
Every word and number is the client's own, written from the box's
numbers. A panel it does not know is named as the box's,
a word of its grammar, and not drawn; the request panels' absence says
the box counts them only for an app behind `observe`. `--json` prints
the points. It exits 0 when it read them, and when the box keeps no
numbers (`"store":"off"`: no metrics line); 3 when the box could not
ask its store (`"store":"unanswered"`) or names it in a word the client
does not know, `--json` then printing the client's own sentence. An
answer is held to its grammars before anything of it is used: the
store's word and each panel's name and unit to `^[a-z0-9][a-z0-9_]{0,31}$`
(the store's `^[a-z][a-z0-9_]{0,31}$`), the window to the client's set
and the one asked, a step and an end with the store's answer, each name
once, no more points than the window has steps, each inside the window,
oldest first, at a whole second; anything else is not moteld's answer,
exit 3. The box asks its store at most every 30 s for an app and window,
so an answer can be that old. The spec's command (motelctl spec row 18,
`motelctl metrics <app> --range <r>`).

`logs <app> [--lines 50|200|1000]` reads `GET /_motel/apps/<app>/logs`,
for a person with a `logs` grant on the app or the box (docs/people.md,
"Reading an app's logs"): the app's newest lines, oldest first, 200
when `--lines` names no other count (`wire.LogsLines`). Each line is
printed as its time, UTC to the second, and its text, whole as the box
sent it (at most 4 KiB, cut by the box), made safe: a tab as spaces to
the next stop of 8, counted in the text's own columns from its start
(each character one column, an escape as its characters, not a
terminal's cells: a wide character before a tab puts what follows it a
cell past the stop, a combining or zero-width one a cell before; an
accepted row), and every other
control character and colour code the app wrote shown as an escape,
never passed to the terminal (`termtext.Tabbed`, through
`model.LogsLines`, which a screen of the lines uses too). The release is not on every line:
a divider before the first line, and wherever the release or the day
changes, names both (`-- 2026-11-20 UTC, release v42 --`; a line whose
unit the journal did not keep, `release not kept by the journal`). A
read sends at most the count or 64 KiB of lines: when lines older than
the first were left, a divider before them says so (stderr, with
`--json`), and that `logs` does not page back (`--lines 1000`
may reach further, when it was not the count asked and the count, not
the 64 KiB, stopped the read). An answer is held to its grammars: `kept` a word, each
release a version or empty, the cursor `wire.LogsCursor` (the box's own
grammar for it, one copy); anything else is not moteld's answer, exit
3. It exits 0 when it read, and when the box says the app keeps its
output on the box (`kept: off`: `deploy_log_lines 0`), which stderr
says, as it says when the journal holds no line of the app (with
`--json` too, after the box's answer); 3 when the
box names what it keeps in a word the client does not know. A box
running as many logs reads as it runs at once (four) answers `503` with
`Retry-After: 1`: one read is not asked again, exit 3. `--json` prints
the box's answer, its cursor among it: the box's position in its
journal, which the box asks a reader to keep in memory only.

`logs <app> --follow` reads the newest lines, then every 2 s
(`model.LogsEvery`) the lines after the last one read, by the cursor
the last answer gave, kept in memory and never written anywhere; a read
that stopped with lines left after the last one sent is followed by
the next at once. When its first read brings no line, stderr says the
journal holds none, and it waits for one. An answer with no cursor (no
line taken) keeps the one sent. When the box did not honour the cursor (`gap`: its line is no
longer in the journal), a divider says lines since may be missing, and
the lines are its newest; with no cursor beside the gap, the next read
is a tail, or every read would be one. A `429` that stands after the
client's own wait, with a `Retry-After` of at most a minute or none, and
a `503` whose JSON says why with a `Retry-After` of at most a minute (the
box's when its four read slots are taken, or while moteld starts, 1 s),
are waited out (the box's `Retry-After`, or 30 s for a `429` with none),
stderr saying so once in a run of them, with the read's own sentence and
the box's words quoted in it, and the follow goes on; any other read
that fails, a `429` asking for longer among them, ends it, with
that read's exit and its sentence saying following stopped. The box
saying the app keeps its output on the box ends it, exit 0. ctrl-c ends
it, 130. With `--json` stdout is the app's lines alone, each as the box
sent it, one JSON object a line, as each read brings them (the
cursor is not printed); what is not a line (a gap, older lines not
sent, an app that keeps its output on the box) is a sentence on stderr,
and a failure once it runs prints no object on stdout either: the exit
says it. A
follow is 0.5 reads a second, and each read that sends lines is an
`access: person read logs` line in the box's journal, naming the person
("The rate", below).

**Which box:** an app URL as `<app>`, else `--box`, `MOTEL_BOX`, the host
of `MOTEL_URL`, or the box repository the shell is in (inside
`<host>/`, or at its top when it holds one box). `--box` and `MOTEL_BOX`
take the host name, and `:port` when it is not 443 (digits alone, read
as a number from 1 to 65535, so `:08443` is `:8443` here and in a URL);
the token is for the host, as with an app URL that names a port.

**Which credential:** a token the person's own ssh key signs when
`--key` names the key. Otherwise `MOTEL_TOKEN`, sent as `Bearer`, when it
is set: a deploy token reads its own app on any motel today. With `MOTEL_URL` set too, the token goes to
`MOTEL_URL`'s box only; a command that names another box sends nothing
(exit 2). Otherwise a
token the person's own ssh key signs, made by the system `ssh-keygen -q -Y sign` in
the namespace `motel-access-v1@motel.smallhours.org`. The key is
`MOTEL_KEY`, then git's `user.signingkey` inside a box
repository, then ssh's default Ed25519 files (`~/.ssh/id_ed25519`, then
`~/.ssh/id_ed25519_sk`), then the agent's key when it holds exactly one.
One key per command, never several. A box takes Ed25519 keys
(`ssh-ed25519`, or `sk-ssh-ed25519@openssh.com` for a hardware key) for
now, so the default files looked at are only those: `id_ecdsa` and
`id_rsa` are not read. An RSA or ECDSA key that is named (`--key`,
`MOTEL_KEY`, git's `user.signingkey`) or is the agent's only key is
refused before anything is signed, with a message that names the key
and the remedy (`ssh-keygen -t ed25519`); ECDSA is not accepted yet, so
an agent that makes only P-256 keys cannot be used for now. The client
writes nothing to disk.

**A key an agent holds** (`ssh-agent`, a password manager's agent, a
hardware token's): the client signs with the system `ssh-keygen` and lists
an agent's keys with `ssh-add`, and both reach an agent through
`SSH_AUTH_SOCK` alone. Neither reads `IdentityAgent` from
`~/.ssh/config`, so an agent named only there works for `ssh` and not
for `motel`: set `SSH_AUTH_SOCK` to the agent's socket. Without it a
command that signs stops before anything is sent (exit 2), saying to
name a key, or that `ssh-keygen` could not sign with the one named;
`motel key`, which signs nothing, still prints the line of a key
that is named or has a file. git's
`user.signingkey` may be a path or the key itself, as `key::<line>` or
as a bare line beginning `ssh-`, which git reads the same way; the
agent then holds the key.

**What a box tells a key it does not take.** Nothing: every refusal is
the same 401, with no header that names motel. So after its one retry
the client names every cause it cannot tell apart (no line lists the key,
the address is outside every `from` list, the name is not the box's main
one, a moteld too old for people's tokens), and the clock first when the
box's `Date` says it is off. A 403 whose `needs` and `scope` are the
words a box writes (`needs` one of `viewer`, `logs`, `deployer`, `key`; `scope`
`app` or `box`) is said from those two alone, and names no app; any
other 403, one naming a role a later motel adds among them, is said as
a refusal that did not say what it needs, with the box's `error`, if
any, quoted as the box's. `whoami` lists `viewer` and `logs` as words,
and any other role a box names quoted, as the box's word. The box's words are never the client's own: a value
it prints in a sentence is held to the grammar the box holds it to, or
quoted by the one quoting function (`termtext.Quote`: cut at 200
characters, made safe to print, `"` and `\` escaped, so the closing
quote is the client's own), and no message carries a decoder's error,
which would quote the body. `whoami` takes an answer only when its name,
release, roles and scopes are of the box's grammars and no required
field is missing or `null`; it shows the key this command signed with
and the box it asked, and an answer naming another key or box, whatever else it holds (the other of the two missing, `null` or not a string among it), prints
only a warning quoting the box's value, and exits 3. The release and
the box's protocol are read from whoami's body alone: no header names
motel. So nothing tells a box's own 5xx from a gateway's in front of
it, and every 5xx is said as "Something at <host> answered <code>, not
known to be moteld's own answer", with its `error` quoted, and exits 3:
a box still starting, which answers a deploy token 503, reads that
way too (an accepted residual, below). A passphrase-protected key file with no terminal to
ask on is refused with the remedy, `ssh-add` (exit 2).

**Only https.** The client never sends a request over plain http, and has
no flag or variable that would (`deploy.sh`'s `MOTEL_ALLOW_HTTP` is not
read).

**Exit codes:** 0 read; 1 a failure answered with a sentence (a 4xx
other than the 401, 403, 404 and 429 below, a 409 for one; said as the
box's: no header tells a gateway's from motel's; and `backup status`'s
not healthy); 2 nothing was
sent (usage, no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not
moteld's answer, any 5xx, a 429 that stood, the box's could not tell of `backup status`, an Alertmanager that did not answer the box, for `monitor alerts`, a metrics store that did not answer it, for `monitor rules`, and a store that did not, for `metrics`); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (the client never continues past one); 9 the deploy `status
--follow` followed failed; 130 interrupted. 7 is held for a box too old
for people's tokens.

## Getting it

Each release after v0.8.0 carries the client for an M-series Mac and
for Linux on amd64 and arm64, `motel_<version>_darwin_arm64.tar.gz`,
`motel_<version>_linux_amd64.tar.gz` and
`motel_<version>_linux_arm64.tar.gz`, on the same
[releases](https://github.com/smallhoursorg/motel/releases) page as
the server and in the same `checksums.txt`, with the release's own
number: `motel version` prints `motel v<version>`. Releases up to
v0.8.0 carry no client. In a terminal, this picks the archive for the
machine it runs on, fetches the latest release's, checks it, and puts
`motel` in `~/.local/bin`:

```sh
case $(uname -s)/$(uname -m) in
  Darwin/arm64) p=darwin_arm64 ;;
  Linux/x86_64) p=linux_amd64 ;;
  Linux/aarch64) p=linux_arm64 ;;
  *) p=; echo "no release carries motel for $(uname -s) $(uname -m): build it from a clone" >&2 ;;
esac
v=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/smallhoursorg/motel/releases/latest)
v=${v##*/v}
f=motel_${v}_$p.tar.gz
base=https://github.com/smallhoursorg/motel/releases/download/v$v
[ -n "$p" ] && cd "$(mktemp -d)" &&
  curl -fsSLO "$base/$f" && curl -fsSLO "$base/checksums.txt" &&
  grep " $f\$" checksums.txt |
  if [ "$p" = darwin_arm64 ]; then shasum -a 256 -c -; else sha256sum -c -; fi &&
  tar -xzf "$f" motel && mkdir -p ~/.local/bin && mv motel ~/.local/bin/ &&
  ~/.local/bin/motel version
```

The `case` names the archive from `uname`; on any other machine (an
Intel Mac, Linux on another architecture, Windows) it says so, and the
block stops before it downloads an archive. The two lines after it take
the latest release's tag without its `v`; for a release candidate, or
any other release after v0.8.0, write the tag in their place instead,
without its `v` and with its hyphen: `v=0.8.1-rc1`. Not an earlier
one: v0.6.0 to v0.7.0 have a `motel_<version>_linux_<arch>.tar.gz`
too, which is the old server, and on Linux the block would install
that as `motel`. The `&&`s stop the block at
the first step that fails, and the checksum step checks the one line
of `checksums.txt` that names the archive, with macOS's `shasum -a
256` or Linux's `sha256sum` (GNU's, or BusyBox's on Alpine): with no
such line either one fails, rather than passing with nothing checked.
When `motel` alone is then not found, `~/.local/bin` is not on your
PATH: `echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile` on
a Mac, or `~/.profile` on Linux, and open a new terminal. On a Mac,
unpack the archive with `tar`, as above: a browser's download unpacked
by a double-click in Finder gives `motel` macOS's quarantine mark, and
macOS refuses to run it until `xattr -d com.apple.quarantine
~/.local/bin/motel` clears the mark.

On an Intel Mac, or any other machine no release carries a client
for, build it from a clone of this repository, with Go 1.27.1 or
later. `go
install …/client/cmd/motel@<version>` does not work: the module names
its sibling `liveswap` by a local `replace`, which `go install`
refuses.

```sh
git clone https://github.com/smallhoursorg/motel && cd motel/client
go build -o motel ./cmd/motel
./motel version
```

A build made this way carries no release: `version` prints `(devel)`,
or a version Go derives from the commit.

Accepted, for the release's archive and the block above:

| Residual | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| Until the first release after v0.8.0 is out, the block reads v0.8.0 as the latest, whose release has no client, and `curl` stops it at a 404 that names only the address; the paragraph above it says releases up to v0.8.0 carry none | 2 | 5 | 1 | 10 | 1 (text), risk 2 | accepted: it lasts until the next release |
| The archive's line in `checksums.txt` is picked by `grep`, where each `.` of the name matches any character: an asset whose name differed from the archive's only there would be picked too, and `shasum` or `sha256sum` would fail on the file not downloaded | 2 | 2 | 1 | 4 | 1 (`awk`'s exact match), risk 2 | accepted: no release has such an asset; it fails red, naming the file |
| That `tar` in Terminal leaves the unpacked `motel` without the quarantine mark of a browser's download rests on Apple's forum answer (the 2026-11-16 research), not a run on a Mac: were it wrong, macOS refuses `motel` and the `xattr` line above clears it | 2 | 5 | 1 | 10 | 1 (text), risk 2 | accepted: the block's own path, `curl` then `tar`, is not concerned; to be tried on a Mac with the first release that has the archive |
| The `case` knows each archive by one `uname -m` spelling: a Linux that prints `arm64` or `amd64`, or a shell under Rosetta on an M-series Mac (`x86_64`), is told no release carries `motel` and to build it from a clone, though an archive would run there | 2 | 5 | 1 | 10 | 1 (text: more patterns), risk 2 | accepted: refused before any archive is downloaded, naming the system and machine it saw, and the build from a clone works there |
| On Linux, the PATH line added to `~/.profile` reaches a new login, not a new terminal window, whose shell does not read `~/.profile` (and bash skips it where `~/.bash_profile` exists, and zsh never reads it): `motel` alone is still not found until the next login, or until `. ~/.profile`; with zsh, not at all, until the line is in `~/.zprofile` | 2 | 5 | 1 | 10 | 1 (text), risk 2 | accepted: `command not found`, after the block has run `~/.local/bin/motel version` by its full path; Debian's and Ubuntu's own `~/.profile` add `~/.local/bin` at login once it exists |
| The block needs `curl`, which some minimal systems lack (Alpine's base image, named above for its BusyBox `sha256sum`): there the first `curl` is not found, `v` is empty, and the download fails | 2 | 5 | 1 | 10 | 1 (text), risk 2 | accepted: red at the first `curl`, `not found` |

## How box text is kept out of the client's voice

Everything a box sends is hostile until shown otherwise, and the
client's own sentences (its advice, its causes, its labels) must never
be ones a box wrote. A value from a box is used in one of three ways
only:

1. **A checked token**: held to the grammar the server holds it to
   (an app or version name, a phase, a deploy's status, a unit, the
   watchdog's state, the metrics' window, a
   commit, a digest, a role and its scope, whoami's name and release,
   a backup's app, the shape of its result and a line's level, the
   env_file keys an answer's `redacted_env` names in the sentence for
   a value that did not read (one outside the grammar or past 128
   characters is counted there, not printed, and the answer stands),
   each with the server code it mirrors cited beside its pattern in
   `internal/client/client.go`), or compared with what was asked (the
   app, the version, the host, the key, the box). A value outside its
   grammar makes the answer "not moteld's answer", exit 3, and is not
   printed. One exception, which prints nothing of the value either: a
   value that carries a marker of the box's own response filter
   (`[redacted:<KEY>]`, or one of the spellings it falls back to when a
   key is named for its own value: `[REDACTED:<KEY>]`,
   `[withheld:<KEY>]`, `<<KEY> removed>`, `[#N]`; and
   `[masked, N chars]`; what stands inside a marker may itself be
   rewritten there, to any depth, `[redacted:[redacted:aws-key]]`: a
   marker runs to the bracket, or the angle, that closes it) is one the filter replaced
   part of, as it does to a unit named after an app whose env_file
   holds the app's name (`DB_NAME=<app>`). It is shown as
   `[hidden by the box's filter]`, the client's words; and the app, or a
   record's version, is taken as the one asked for when the box's
   equals it everywhere but at a marker, and the one asked for is what
   is printed.
2. **A quotation through `termtext.Quote`**: white space collapsed,
   cut at 200 characters, every control made visible, `"` and `\`
   escaped, and each `'` and `` ` `` that stands beside another (two
   read as one double quote); outside ASCII only the two dashes and
   the ellipsis are written as they are, and every other character as
   a `\u` (or, above U+FFFF, `\U`) escape, whatever it is: a quote mark of any script, a
   letter that looks like one, a blank or invisible character. So the
   closing quote is the client's own; attributed ("the box says: …").
   A box-written backtick command inside a quotation is shown as the
   box's words, by design: it is inside the box's quotation.
3. **A labelled, capped value**: a free-text field (a deploy's error, a
   note, a log tail or probe body line) on a line whose label is the
   client's, made safe by `termtext.From` (an app's own output by
   `termtext.Tabbed`, below) and cut at 200 characters (`…[cut]`; a
   record's screen cuts a tail line at 2000). An app's own output, a
   record's log tail and probe body lines and a line of `motel logs`,
   is made safe by `termtext.Tabbed`:
   `From`, but a tab as spaces to the next stop of 8, counted in the
   text's own columns, so that what the app laid out with tabs (a stack
   frame, a tab-separated log) lines up; a cut is of the text as shown.
   A line of `motel logs`, after the client's time, is not cut: it is
   what a logs reader asked for, and the box cuts it at 4 KiB.

No message carries a transport's or a decoder's error text once a byte
came from the peer: a fixed reason of the client's own is said instead.

A body the box's response filter withheld (a 200 whose one sentence
opens `response withheld`, or with a marker where the filter replaced
part of those words: an env_file it could not read, say) is said
as that, with the box's sentence quoted, exit 1: the sentence names
what the operator has to mend. (The filter's last resort, a body of
`[#0]` alone, is said the same way and names nothing.)

This is held by review, the hostile fixtures and goldens
(`internal/cli/testdata/hostile-*.golden`), and one partial source test,
`TestNoHandMadeQuoteMarks`, which finds only a few shapes of a quotation
made by hand. Accepted:

| Residual | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| A new message could splice box text without a test failing | 2 | 2 | 3 | 12 | 3 (typed message parts) | accepted |
| An env_file value that is a piece of a marker's own punctuation (`:aws-key]`, `d:[redac`), replaced by the filter's last pass across a marker's edge, leaves the box's text with brackets or angles that do not pair, so no marker is found whole where the app's name or a record's version stood: `status` says "the status of another app", exit 3 (a value held to a grammar is still shown as hidden) | 2 | 5 | 3 | 30 | 3, and not the client's to make: the box's filter would have to keep its later layers off the markers its first wrote | accepted at the third review round and by the owner on 2026-11-17, the fix in the box's filter held as #553: over the bar because an operator's input scores occurrence 5 however unlikely; it takes a credential-shaped key's name and a value made of marker punctuation |
| A 200 from something that is not moteld, lacking one of the answer's fields or more, whose `error` either opens `response withheld` and then ends or goes on with a colon or a marker, or opens with any start of those words (none of them, too) and then something shaped like one of the filter's markers (`[#404] not found`, `<content removed>`, `response [#504] gateway timeout`), is said as the box withholding its answer, exit 1, not as "not moteld's answer", exit 3: a marker may stand for any piece of the filter's opening words, so what follows it cannot be held to them | 2 | 2 | 3 | 12 | 2 | accepted: it takes another server at the box's name answering 200 with such an error; the sentence is still quoted as its sender's |
| A 5xx in moteld's own words (a box still starting, a box status that cannot read its files) is said as not known to be moteld's and exits 3, not 1: no header tells it from a gateway's, by the owner's decision that no answer names motel. Its code and its quoted sentence are still shown | 2 | 4 | 1 | 8 | 3 (a stamp on the wire again) | accepted |
| A gateway's JSON 4xx (a 403, 404, 409) is said as "the box says", as motel's would be: no header tells them apart, and none ever did for a 4xx | 2 | 5 | 3 | 30 | 1 (say a 4xx's sentence as "it says", not "the box says") | owed: older than this change, and it rewrites every 4xx sentence and its goldens |
| A box built with a release string that is not `N.N.N` with an optional suffix (`make build VERSION=<another shape>`: the Makefile's own default and every tag fit) has every `whoami` called "not moteld's answer: its version is not a release", exit 3 | 2 | 5 | 3 | 30 | 1 (print another shape quoted) | accepted: over the bar, and no doc asks for such a build; the client holds the release to the shape the Makefile links in so that it can print it unquoted |
| The TUI's marks (`▲ ✖ ⟳`, and the `→` of a version in flight) and box-drawing characters may be double width in some terminals, and misalign its columns (a plain-character mode is a later addition) | 1 | 2 | 1 | 2 | 2 | accepted |
| The client's one retry of a 401 is a request the TUI's engine does not count against its rate | 1 | 1 | 3 | 3 | 2 | accepted |
| A box string could reach a tview widget by a writer (`io.Copy`, `WriteTo` into a text view), by reflection, through a type alias, or by a function or method value of a setter, and the TUI's structural tests (`TestOneDoor`, `TestTUIImports`, `TestActionsAreCommands`) be got round the same ways, without a test failing; held by review and the hostile goldens | 2 | 1 | 3 | 6 | 2 | accepted |
| A hardware key that requires its PIN cannot be used in the TUI: the TUI never asks for one, and the agent does not keep it | 2 | 1 | 1 | 2 | 3 (a PIN prompt outside the screen) | accepted |
| The TUI's `post` may leave a few short-lived goroutines after quit | 1 | 2 | 3 | 6 | 1 | accepted |
| A `Retry-After` of an hour makes the TUI wait an hour: the box's word; the title bar says until when. A longer one is cut to an hour: the client's cap, and meant (moteld's 429 says 1 s) | 1 | 1 | 1 | 1 | 1 | accepted |
| A labelled value under the cap keeps its runs of spaces, so on a narrow terminal a long run can wrap the rest of the value to the start of the next row, where it can look like a label (the `hostile-status` golden shows one). Log lines keep their spacing on purpose | 2 | 1 | 3 | 6 | 2 (wrap-aware printing) | accepted |
| An apps-list read that got no answer is asked again at the list's own interval, 60 s: every read that gets none is asked again at its own interval or its backoff (2 s, doubling, to 60 s) when that is longer (whoami at its backoff), and the backoff is never longer than the list's, so the list is not asked sooner for having failed (ctrl-r asks it, at most once in 5 s); while no read is answered the title bar says STALE and the time of the last answer | 2 | 4 | 1 | 8 | 2 | accepted |
| Enter and esc held on a record no read of which has yet ended (a slow box) send a read at each enter and cancel it at each esc: leaving a record that never answered forgets it. The reads stay within the engine's rate, and it was so before the review's fixes | 2 | 4 | 1 | 8 | 3 | accepted |
| A record every read of which the box refused (a 403, a 404, a 409) is read again by ctrl-r, esc, enter in turn, a read for every three keys: leaving it after ctrl-r forgets it, as the row above. The reads stay within the engine's rate | 2 | 5 | 1 | 10 | 2 | accepted |
| A step of the wall clock back that lands between two readings handed to the engine out of order (two goroutines, microseconds apart) moves the engine's times twice: the title bar can say PAUSED until a key, and a 429's wait can end early by the step | 2 | 2 | 3 | 12 | 3 | accepted |
| An answer's time is taken when it is applied on the UI's goroutine: after a terminal that stalled its output, the one answer that waited shows an age short by the stall, until that value is next read | 2 | 3 | 3 | 18 | 2 | accepted |
| The crumb cuts a long app name from its end, so two names that share a long start read alike there on a narrow screen: of a 63-character name it keeps the first 24 characters at 60 columns and 44 at 80 (fewer when the age beside it is longer), and a record's crumb loses its version | 2 | 5 | 1 | 10 | 1 | accepted |
| After this machine's clock is set back and is then wrong by more than a token can bridge (2 minutes: the client sends a token until a minute before its exp on its own clock, and the box takes it until a minute after on the box's), the box refuses the token within its ten minutes, every read stops, and the refusal screen names the clock first ("This machine's clock differs from the box's by 3600 s (behind it; …"). Until then who am I's token line says the token has the step's length more left than it has. A token signed afresh is stamped on the same clock (`r`'s too: past about 10.5 minutes it is refused at once). A clock that was fast and is set right loses nothing | 2 | 3 | 1 | 6 | 3 (none on this side: the clock is wrong) | accepted |
| After an answer about another key or box and `r`, the title bar's word can say `refused`, or `live` with the age of an answer from before the drop, until whoami answers, as it could before the review's fixes: the session drops what the box said, but not when it last answered or whether it refused | 2 | 2 | 3 | 12 | 1 | accepted |
| The `--key` of a printed command (`c`, `P`) is the key's path as the TUI shows it (`Key.Shown`: the home directory as `~`, made safe by `termtext`), so a path holding a control character or bytes that are not UTF-8 prints as a path naming another file, and a relative path works only from the directory the TUI was started in; running the line then fails naming the path | 2 | 5 | 1 | 10 | 2 | accepted: the line fails, and says which path, rather than reading another key |
| After an answer about another key and `r`, a whoami that comes back refused (a 403, or a release that is not `N.N.N`, as in the row on such a build above) leaves the stop screen up, saying `stopped` and the old reason until another read is answered, then the whoami's refusal, and offering `r`, while the reads run behind it (the list and every status; the title bar says `live`); `r` does nothing there, since no read is stopped; `q` leaves | 3 | 2 | 3 | 18 | 2 | accepted: it takes a box that answers about another key and then refuses whoami, and the screen still says what was refused |
| Opening and closing who am I again and again (`?` then esc: each esc opens it again) reads whoami at each opening while its last answer is over a minute old, or it has never answered, and its newest read failed without being held (a release that is not `N.N.N`, as in the row on such a build above): 41 reads in 10 s were measured, within the engine's rate | 2 | 5 | 1 | 10 | 2 | accepted |
| Per-frame work has no bound in what a box sends: every table and text view is rebuilt at each frame (one a second, and one a key), so a hostile box's list of tens of thousands of names, or a record with hundreds of thousands of log lines, all within the client's 4 MiB cap on an answer, makes every key take seconds (the review measured 55,000 names at 6.2 s a frame, 550,000 log lines at 2.0 s a frame, with the fixture client); an honest box cannot | 2 | 2 | 1 | 4 | 3 | accepted |
| `door.Line` draws each grapheme cluster in the style of the segment it begins in, since tview reads a tag only between two clusters: a character that joins the one before it across two segments (a combining or spacing mark, a Hangul vowel or final, a flag's second half, a pictograph after a zero width joiner) is drawn in the earlier segment's style. So a line of the box's on the backups screen that begins with such a mark shows the mark in its level's colour, as the start of a key's path does after `not asking: `; and text ending in a Prepend character (U+0600, U+0D4E), a Hangul leading consonant, half a flag, or a pictograph and joiner, directly before a dressed segment, would draw that segment's first character in its own style (no screen puts a box's or a person's text directly before a tag: a survey of the model's lines at `58bbd74a`, which read most of them) | 2 | 2 | 3 | 12 | 2 | accepted: one row, the width, and styles closed at a line's end are held for every line (`FuzzLine`, `TestClassesBesideATag`, `TestPairsBesideATag`); a tag inside a cluster is what drew the cluster twice |
| With wrap on, tview does not draw the cluster that overflows a row where a word longer than the row is split, when a grapheme Control follows it directly: the line feed that ends the line, or the zero-width space `door` writes after a `[`. So in a record's tail after `w`, the last character of a long token that ends its line one cell into a new row is not drawn (`abcdefgh` at width 7 draws `abcdefg` and an empty row), nor is a `[` at such a split; on the help and stop pages it takes a word of the client's own wider than the row. It was so before #637, and plain tview does the same. Since #637 it also holds for a Prepend character (U+0600, U+0D4E) that ends a dressed segment at such a split, where `door` writes a zero-width space before the tag: of the wrapped views only the refused screen draws dressed lines, and no dressed segment ends in a box's or a person's text | 3 | 5 | 5 | 75 | 3 (the door wraps a wrapped view itself, by `Width`, and hands tview its rows; or tview sizes a new row's first cluster from the state before it) | owed: a change of its own; found by the review of #637 |
| The cap cuts through the client's own quotation of box text: a failure's sentence is capped (200 characters in an apps row's reason, 400 in a "the last read failed" sentence, 800 alone on a screen) after the box's value is quoted, so a quotation that runs past the cap loses its closing quote before `…[cut]`. On the apps screen, cut to the window on one row, it shows only in a window over about 230 columns; who am I, the box screen and an app's "the last read failed" lay the sentence on rows, so there it shows at any width when the quoted value is long in escapes (200 bidi overrides, each written as `\u202e`, make 1200), on an app's screen only while the sentence fits in half the window (the row on that sentence below) | 2 | 2 | 3 | 12 | 2 | accepted |
| ctrl-z on the `:` line after an answer about another key and `r` brings back what was typed there before (the input field's own undo; the stop empties the line but not its history), which can be an app's name the dropped box gave | 2 | 2 | 3 | 12 | 2 | accepted |
| whoami's name and release may spell one of the title bar's own words (`"name":"PAUSED"`, `"version":"0.0.0-SIGNED-OUT"`): within their grammars; no space, `·`, `│` or `:` can be forged | 2 | 2 | 3 | 12 | 2 | accepted |
| Who am I closed within a second of a 403 that asked whoami again puts that whoami off to its 5-minute turn: closing who am I sets whoami's next read to its last answer and 5 minutes (`Engine.MeOpen`), over the read the 403 made due now, if that read has not yet been taken | 2 | 2 | 3 | 12 | 2 | accepted |
| `c` and `y` judge whether a command line is drawn as it runs from tcell's simulation screen (`door.AsIs`): a real terminal may draw a character otherwise than tcell's tables say, and nothing here can see that | 2 | 2 | 3 | 12 | 3 | accepted |
| Two things whose reads each get no answer for reasons of their own (two routes each failing, while the box answers the rest) begin a session-wide run of no answer when their reads come back to back, none answered between: every read backs off and the title bar says slow, then STALE, until the next answer to any other read says back and asks whoami again. A thing's first read in a row to get none makes it likelier, since it brings forward the read due next, which may be the other failing one. Measured with two of the twelve fixture apps (`api`, `billing`, next to each other in the cycle) answering 500 and the rest answered, over 9 minutes: 6 such runs, 32 s within them (the first 12 s, the rest 4 s each), every read held 4 s each time (once 8 s), the title bar saying slow 24 s, STALE 8 s and back 30 s, and whoami read 7 times where its turn gives 2; the same with a run's retry going first to a read whose last read was answered, since each run ends at the first retry such a read takes | 2 | 2 | 3 | 12 | 2 | accepted |
| After a 403 or a 404 to the apps list, the list read before is kept, not dropped as a status, a record or the box's status is: the apps screen shows its names under "list as of … the last read failed: …", their statuses go on being read (each then answered, or dropped and held on its own refusal), and `y` there offers the old list, said only as fetched at its time; the list is read again only by ctrl-r | 2 | 3 | 1 | 6 | 2 | accepted: the list is said to be old and its last read failed, and no name keeps a status the box has refused |
| A read whose last read was refused without being held (a 409, or an answer the client cannot use) and whose next read gets no answer is not counted as its first in a row to get none: its own count is already 1, so it brings no read of another thing forward and its backoff starts at 4 s, not 2. Measured with one app open, its status answered 409 once and then the box answering nothing: the title bar first says slow 56 s after the box stopped answering (the list's turn), not 1 s after; with 3 apps 6 s. And in a run of no answer such a read counts as failed: it goes after every due read none of whose reads has failed since its last answer, as one that got none does, though the box would answer it | 2 | 2 | 3 | 12 | 2 | accepted |
| With every route but one failing (the box answering one read alone, an app's status), that read waits behind the others: in a run of no answer a read never tried in the run keeps its old due time and goes ahead of it, one at each retry, the retries up to 60 s apart. Measured on the fake clock with the box answering only the last app's status: 12 apps, its first read 9 minutes after, then at most 1 minute apart; 60 apps, 28 minutes, then gaps up to 27; 300 apps, 1 h 42 min, then gaps up to 3 h 13 min. The title bar says STALE throughout, but for the seconds after each answer (back, then slow) | 2 | 1 | 1 | 2 | 3 | accepted |
| A wake into a run of no answer keeps the run's count (by design: the box did not answer before the sleep), so the title bar says at once what it said before the sleep (slow, or STALE), and when the first read after waking gets no answer because the network is not back yet, the next waits out the run's backoff: 60 s once the run had five reads or more before the sleep (measured on the fake clock: 60 s after a 5-minute outage, 32 s after 30 s, 8 s after 10 s), the title bar saying STALE for that long on a network that then works | 2 | 2 | 1 | 4 | 2 | accepted |
| At 60x16 the `y` overlay's own words (what the answer is to, how it is shown and cut, and that `P` prints it whole) take 7 of its 11 rows over the apps screen, 9 and a blank over an app's, and all 11 over a record whose command line is cut, so the answer shows on three rows, one, or none until it is scrolled; the footer says `↑↓ scroll` | 2 | 5 | 1 | 10 | 2 | accepted |
| An app's screen gives its alerts' command (`all: motel monitor alerts --box <host> [--key <path>] --app <app>`) on one row, cut at the window's edge like any line: at 60 columns, or with a long box or app name, its end is lost (`…`), and the screen's `c`, `y` and `P` give the status's command and answer, not the alerts'. Found by Copilot on #602, which asked for a way to the alerts the window cuts | 2 | 5 | 1 | 10 | 3 (the alerts in `c`'s overlay, or a screen of their own) | accepted: the cut is marked, the command is the status's with `monitor alerts` and `--app`. A screen of the alerts' own is a viewer of the box's only (the alerts-screen PR's `:alerts`), so a viewer of one app has the command alone |
| A refusal said alone on a record's screen or an app's (a 403, a 404) is one row, cut at the window's edge with `…`: a long one (a 403 that did not say what it needs, or one quoting a value long in escapes) loses the box's words past the edge and the client's own after them (`motel whoami` lists the roles you hold from here.). Laid on rows instead (62d41e87, reverted), it took at most half the window's height and lost its end with no mark | 2 | 4 | 1 | 8 | 2 | accepted |
| An app's "the last read failed" sentence is laid on rows within half the window's height, the fields under it: at 60x16 one quoting a value long in escapes (a 409 quoting 200 bidi overrides) fills those 8 rows, the last of them saying how many lines are not shown (`… 10 more lines: y shows the answer; then P prints it whole`), and its end (its cap's `…[cut]`) and every field under it, the state among them, are not on the screen. `y` shows the answer held, fetched before the failure, with the fields' values, but not the sentence's end, which nothing then shows; the deploys stay below it | 2 | 2 | 3 | 12 | 2 (a notice worded by whether the sentence is among the lines cut) | accepted |
| A record's and an app's details are cut to the window, and cannot be scrolled: each line at the window's width, marked by its `…`, and the block to half the window's height, its last row then saying how many lines are not shown, that row's own among them (`… 4 more lines: y shows the answer; then P prints it whole`, an app at 60x16, where the cut was unmarked before #566). The half holds on an app with no deploys too, the rows under it then blank. `y` shows the box's answer as JSON, not laid out as the screen lays it, and itself cut past 2000 lines, said there, where `P` prints it whole | 2 | 5 | 1 | 10 | 3 (a header that scrolls is new screen behaviour) | accepted, by the owner's choice in #566 of the first of its options: the cut is said, the layout left |
| With `--key` in it (whenever the key was named by `--key` or by `MOTEL_KEY`), `c`'s command lines are wider than the overlay at 80 columns for an ordinary key path and box name: `motel status --box deploy.smallhours.org --key '~/.ssh/id_ed25519.pub' -- example` is 81 characters and its `--json` line 88, where the overlay leaves a command line 76 cells (78 inside its border, 2 of indent). Both are cut at the edge and marked, and the overlay says not to copy them and that `P` prints the first whole; with a longer key path (`'~/tui1-trial/keys/trial.pub'`) both are cut before they differ, so they look alike as far as they show | 2 | 5 | 1 | 10 | 3 (a command laid on rows cannot be copied as one line) | accepted |
| A mistyped `:` command is said on the crumb's row, cut where the apps screen's summary beside it begins: at 60 columns `? lists the : views;…`, the remedy whole and what was wrong cut away, the cut marked; at 80 `? lists the : views; no command "bogus" …`, what was wrong but its last word | 2 | 5 | 1 | 10 | 2 | accepted |
| A hardware key's touch on the read made after one that waited 5 s or more and got no answer is counted in `waiting`'s seconds: the count goes on from the read that got none until the box answers, and the time a token is signed is left out only of the read in flight's own count, so after a 10 s touch the title bar says the box has been waited for 10 s longer than it has been asked. The word is true (the box has not answered); the number is long by the touch. Starting the count again at the signature would put `live` back for 5 s in an outage | 2 | 2 | 3 | 12 | 2 | accepted |
| `motel` alone on a terminal, with a box and a key found (in a box repository, git's `user.signingkey` among them), signs a token before the screen opens, as every command that asks the box does: a person with a hardware key who typed `motel` to see its commands is asked for a touch, then gets the screen (`motel -h` prints the commands and signs nothing) | 2 | 5 | 1 | 10 | 3 (a screen that opens before its first reads, or a key press before the first signature at every start) | accepted, by the owner's decision of 2026-11-19 that `motel` alone is the screen |
| `motel` alone waits for its first reads before the screen opens ("The TUI" below): where the box's host does not answer, a person who typed `motel` gets one line on stderr after 3 s and the screen within a minute, where the help came at once before `motel` alone was the screen; ctrl-c quits, and `motel -h` asks nothing | 2 | 4 | 1 | 8 | 3 | accepted |
| `motel tui`, the screen's command before `motel` alone opened it, is refused as any word that is no command (`unknown command "tui" for "motel"`, exit 2), and nothing says that `motel` alone is the screen | 2 | 5 | 1 | 10 | 2 (a hidden command that says so) | accepted, by the owner's decision of 2026-11-19: no release said `motel tui`, and a pointer would keep the name |
| `motel \| jq`, or `motel > file`, meant for the screen and its `P`, prints the help into the pipe or the file, exit 0: with no arguments at all, stdout not being a terminal is how a script's `motel` is told from a person's, and a pipe meant for `P` looks the same. `jq` then fails on the help, whose own words say why only to someone who reads them. With a flag the screen opens (`motel --box <host> \| jq`, "The TUI" below) | 2 | 5 | 3 | 30 | 3 (none within the rule: any other test of a pipe gives a script the screen) | accepted over the bar, by the owner's decision of 2026-11-19 on what `motel` alone does |
| `motel --json` with no command is refused, exit 2 (`motel alone has no --json: it draws screens, and every command takes --json`, and `{"error":"…"}` on stdout), where it printed the help, exit 0, before `motel` alone was the screen: a flag asks for the screen, and the screen prints no JSON | 2 | 5 | 1 | 10 | 2 | accepted |
| What says a screen can be drawn (`cli.CanDraw`) asks only for a terminfo entry for `TERM` that can move the cursor, a locale that names no character set, or one under a name tcell knows for UTF-8 or ASCII (tcell's own rule, which `TestCharsetIsTcells` holds the client's copy to), and `/dev/tty`: in a terminal that draws otherwise than its entry says, or under a locale that names no character set and is not UTF-8 (`LANG=en_US`, which glibc reads as ISO-8859-1), `motel` alone opens a screen drawn wrongly, where it printed its help before `motel` alone was the screen; and ASCII under a name tcell does not know (`LANG=en_US.ANSI_X3.4-1968`) is refused as a character set not supported, before anything is signed. `motel -h` still prints the help | 2 | 2 | 1 | 4 | 3 | accepted |
| `motel 2>/dev/null` alone, on a terminal where no box or key is found: the help, exit 2, and the line that says what was not found is lost with stderr, though the help says a line under it says why | 2 | 5 | 1 | 10 | 2 (the line on stdout too) | accepted |
| `motel help tui`, as `motel help` with any word that is no command, says ``Unknown help topic [`tui`]`` and prints the usage, exit 0, where `motel tui` is refused, exit 2: cobra's own answer, and older than `motel` alone as the screen | 2 | 5 | 1 | 10 | 2 | accepted |
| `motel` alone, where no box or key is found, interrupted (ctrl-c, the terminal closed) in the moment its help is being written: the help is on stdout, the line under it is not said, and the exit is 130, where an interrupted command prints nothing on stdout. The interrupt is looked for before the help is written (`TestSignalBeforeTheScreen`), not during: about 3.5 kB to a terminal, a few milliseconds unless the terminal is held (ctrl-s) | 2 | 2 | 3 | 12 | 2 (the help built whole, then written only if not interrupted) | accepted |
| A client with #577's change reading a box on v0.8.0 or earlier: that box's `memory_bytes.now` is the range's newest point, kept while it is at most three steps old, so under `--range 24h` up to 15 minutes old (three 5-minute steps; 3 minutes at the hour), and its `avg_5m` and `memory_max_bytes` alike. The app's screen shows it as `now` beside `metrics     over 24h` with no age, where the client before this change said `newest point 14:00:10 UTC (12m ago)` under it | 2 | 5 | 3 | 30 | 2 | accepted over the bar, pending the owner (the skill's rule 4): it ends when the box takes this release, and telling the two kinds of box apart needs the client to judge the box's release |
| Following a deploy (#580): a deploy seen only before its first phase (a push's upload) is matched to its outcome by its version and its end alone, so a later deploy of the same version that begins and ends before the next read (5 s on an app's screen and for `status --follow`, 30 s or more for another app) is taken for its outcome: `status --follow` exits by that one, and the app's screen says it; so does a step of the box's clock back between the two. Once the deploy was seen in a phase, its outcome must begin with the phases seen, each at the box's time, which no other deploy's does | 3 | 2 | 3 | 18 | 3 (the box would name each deploy, in `deploying` and `last_deploy`) | accepted: it takes two deploys of one version within one read, the first not yet past its upload |
| `status --follow` ends at the first read that fails (no answer, a 5xx, a 429 that stood after its wait, a moteld restarting), with that read's exit and its sentence saying the deploy may still run, though it may end well a moment later; it is not asked again | 2 | 3 | 1 | 6 | 2 (a retry at its turn, and a count) | accepted: it ends red and named, and run again it follows the same deploy if it still runs |
| `status --follow` signs its tokens as every command does, each lasting 300 s, so a follow of more than about 3.5 minutes with a hardware key asks for a touch on the terminal mid-follow, the reads waiting until it is given; the TUI's tokens last 600 s (`TestForTUI` holds the longer life to the TUI's client) | 2 | 3 | 1 | 6 | 1, risk 2 (a captured token then lasts twice as long) | accepted: a deploy's usual length is under a minute |
| `logs --follow` signs its tokens as every command does, each lasting 300 s, so with a hardware key it asks for a touch on the terminal about every 4 minutes for as long as it follows, its reads waiting until it is given | 2 | 5 | 1 | 10 | 1, risk 2 (the TUI's 600 s: a touch every 9 minutes, and a captured token that lasts twice as long) | accepted: the touch is asked for on the terminal the follow runs in, and nothing is missed while it waits: the next read takes up at the cursor |
| `logs --follow`, which runs for as long as a person leaves it, ends at the first read that fails other than by a 429 or the box's busy 503: no answer in 30 s, a connection closed while moteld restarts (its upgrade), a gateway's 5xx, the box's own 500 when journalctl could not read the journal in time; with that read's exit and its sentence saying following stopped, and nothing after it is read until it is run again | 2 | 4 | 1 | 8 | 2 (a retry at the next turn, and a count of them) | accepted: red and named, as `status --follow`'s row above; run again, it tails, and the lines between are in the box's journal; found by the self-review of `motel logs` |
| A 429 during `logs --follow` is waited out twice before the follow says so: the client's own read waits the box's `Retry-After` once, silently, as every command's does, and when the 429 stands the follow says it and waits it again, so the lines stop for twice the box's wait (60 s for two 429s of 30 s), the first half unsaid | 2 | 5 | 1 | 10 | 2, risk 2 (the follow's reads made with the client's own wait off, which `TestForTUI` holds to the TUI's client alone) | accepted: the second wait is said, nothing is lost, the next read taking up at the cursor; found by the self-review of `motel logs` |
| The plain layout's divider comes wherever a line's release differs from the line before, and the journal keeps no unit for some lines (a child that exited before journald read its line, a crash loop's lines often): lines of one release mixed with lines of none print a divider before nearly every line | 2 | 5 | 1 | 10 | 2 (a line of no known release marked on its own row, the divider left as it was) | accepted: each divider is true, and `--json` has the release on every line; for the owner to confirm with the layout |
| An answer with lines and no cursor is taken (`hold` asks a cursor only to be of its grammar), so a follow's first read, a tail, is followed by another tail every 2 s, and the same lines print again without end. No moteld sends it: a read that sends lines took them, and gives the last one's cursor | 2 | 1 | 3 | 6 | 2, risk 1 (lines with no cursor refused as not moteld's answer) | accepted: it takes another server, or a box's bug to come, and a new branch for it is what the rule's stop is for; found by the coordinator's review of `motel logs` |
| `termtext.Tabbed` counts a column a rune, not a terminal's cells, so on a line with a character that is not one cell wide before a tab the columns after it do not line up: a wide one (CJK, an emoji) puts what follows a cell past the stop, a combining or zero-width one a cell before it, and on the TUI's screen a character the door rewrites as an escape further off | 2 | 5 | 1 | 10 | 2, risk 2 (count cells with a width function the caller passes: the door's on the screen) | accepted: the text is whole and every other character as before; only the alignment after such a character is off. Found by the coordinator's review of the tabs change |
| `motel logs --follow --json` refused by the command line's parser before the command runs (no `<app>`, a flag that does not read, `--lines` with no value) prints its `{"error":…}` on stdout, where the mode's stdout is the app's lines alone; a refusal the command makes itself (a count not the box's) prints none | 2 | 5 | 1 | 10 | 2, risk 1 (Main reads `logs`, `--follow` and `--json` from the arguments, as it reads `--json`) | accepted: exit 2, the sentence on stderr, and nothing was sent; found by the self-review of `motel logs` |
| A follow of an app that writes more than a read sends every 2 s (200 lines, or 64 KiB) reads again at once after each read (the owner's rule for a follow's `more`), so beside an open TUI it takes a person past the box's 2 requests a second: the box answers 429, the follow waits it out (stderr says so once) and the TUI slows | 2 | 5 | 1 | 10 | 2, risk 2 (a floor between reads, which the owner's rule does not have) | accepted, by the owner's decision that a follow's `more` is read at once: each 429 is waited out and said, and no line is lost, the next read taking up at the cursor |
| `motel logs` does not say an answer's `redacted_env` (the keys whose values the box's filter replaced) in the plain layout, nor beside the lines of `--follow --json`; one read's `--json` prints it in the box's answer | 2 | 5 | 1 | 10 | 2, risk 1 (a note naming the keys, from the client's grammar for them) | accepted: each replaced value stands in its line as the filter's marker, which names its key but in its last spelling, `[#N]` |
| A phase a newer box names that this client does not know, in the deploy in flight's phases, makes the whole status "not moteld's answer", exit 3 (on the screens the app is `unknown`), for the rest of that deploy, as one in `last_deploy` already did until the next deploy and one in the status's phase while it lasts: since #580 the phases are printed, and so held to the outcome vocabulary | 2 | 5 | 1 | 10 | 2 (an unknown phase shown in the client's words) | accepted: it is said, and a client of the box's release knows its phases |
| A TUI left without a key for 10 minutes pauses every read but the alerts the title bar counts (the title bar says PAUSED), so a person waiting on an app's screen for CI to reach its deploy sees nothing of a deploy that begins after that until a key; `status --follow` follows only a deploy already in flight when it starts | 2 | 5 | 1 | 10 | 2 (no pause while an app's screen is open: a new rule for the rate and a hardware key's touches) | accepted: the pause is the owner's rule for an unattended TUI, and says itself |
| An app's `ended` line puts its key hint last (`· d opens its record`), and its `phases` line the failed phase's `✖`: at 80 columns the line of a failure in soaking with 12-character versions is 85 cells, so the hint is cut (`…`), and a failed phase of 100 s or more cuts its `✖`; the line still says `failed in <phase>`, and help lists `d` | 2 | 5 | 1 | 10 | 1 (text), risk 2 | accepted: what is cut is a hint and a repeated mark |
| `motel backup status` whose stdout takes none of its human output (a full disk, a closed pipe) exits 2 with the write's error, not the box's verdict (1, 3): every command's human output fails so | 2 | 3 | 1 | 6 | 2 | accepted: red and named; `--json` and the verdict's exit are what a script reads |
| On the backups screen in a window 14 columns wide or less, far below the smallest the screens are laid for (60), a could not tell's quoted why is laid one character a row, each past its 14 columns of indent, so the rows show blank | 2 | 5 | 1 | 10 | 2 | accepted: no screen is laid for such a width; `y` shows the answer |
| After a 404 to the backups (a moteld older than the read) the read is held until ctrl-r, and the screen does not say ctrl-r asks again, as an app's screen does not after a 404: once the box is upgraded it says the 404 until ctrl-r or `r` | 2 | 5 | 1 | 10 | 1 | accepted: the sentence names the cause, and ctrl-r is among the screen's keys (`?`) |
| A rate of 100k/s or more that is not round (123456.7/s) has no compact writing in a plot label's seven cells (`123.5k/s` is eight), so its axis label is cut, marked: `123.5k~`. The head above the plot writes the value whole, and main did the same | 2 | 5 | 1 | 10 | 2 (a writing of its own for rates past 100k/s) | accepted: the cut is marked and the head has the value |
| The metrics screen's plots are braille dots (U+2801 to U+28FF), which every East Asian width table gives one cell, and its axes and marks ASCII; but a terminal whose font has no braille draws each as its fallback glyph, and the plot reads as a row of boxes. The numbers above each plot (newest, highest, lowest, each with its time) still say what it shows | 2 | 2 | 1 | 4 | 2 (an ASCII plot, `.` and `'`, at half the height) | accepted: `motel metrics` prints the same numbers in words; the plots are checked on tcell's simulation screen alone, not yet in a real terminal or on a Mac |
| A series point the box sends as other than a pair of numbers (`[t]`, `[t, null]`, `[t, v, x]`) is decoded as `[t, 0]` or its first two (Go's decoding into `[2]float64`), and drawn or printed as such, a zero where moteld would have sent no pair. moteld sends pairs of numbers alone | 2 | 1 | 3 | 6 | 2 (the points decoded as lists of any length and refused unless pairs) | accepted: it takes a box that is not moteld's, which the answer's other grammars mostly catch; found in the series read's first self-review |
| After the box refuses an app's status and then answers it again, a window of the app's series shown again within 30 s of its last answer says "asking…" until that answer is 30 s old, when it is read: the refusal dropped the answer held, and the engine waits out the answer's age | 3 | 2 | 3 | 18 | 2 (the engine told the answer was dropped) | accepted: it takes a refusal and a window shown again within 30 s; the read comes within 30 s. Found in the series read's second self-review |
| A deploy mark on the metrics screen is a column of the window (28 minutes at 60 columns over a day, almost 5 hours over 14 days): two deploys in one column are one mark, and a mark says the deploy was within that column, not at its start. The legend line says how long a column is | 2 | 5 | 1 | 10 | 2 | accepted: the record's own time is on the app's screen (enter on its row) |
| The metrics screen marks the latest deploy of each version the box keeps a record of, from the app's status: a version deployed twice is marked at its last deploy alone, a deploy older than the oldest record the box keeps is not marked, and a failed deploy never is. The legend says "a version's latest deploy that succeeded", and "records from <time>" when the oldest is inside the window | 2 | 5 | 1 | 10 | 3 (a read of every deploy, which the owner's M2 decision rules out) | accepted, by the owner's decision of 2026-11-20 that marks need no new read |
| The terminal's bell is the client's own BEL, written by tcell's `Beep`: the first byte the client itself sends that draws nothing (tcell's own mode sequences aside). A box's text cannot ring it, as `termtext` makes every control visible; `hostile.Scan`'s bare-BEL signature holds that in `TestSafeBytes`, and `TestSafeBytesAlertArrives` finds the one BEL of an alert made of the hostile fixture's attacks | 2 | 5 | 1 | 10 | 1 | accepted: the bell is the decision (#576) |
| The bell is not heard where the terminal is silent (Ghostty's, Alacritty's and VS Code's defaults, a terminal whose bell was turned off), and is not forwarded by a tmux with `visual-bell on` or a screen with `vbell on` (Debian's `/etc/screenrc` sets it), so a person away from the screen is not called there | 2 | 5 | 3 | 30 | 3 (a desktop notification, OSC 9 or 777, per terminal and wrapped for tmux) | accepted over the bar, by the owner's decisions of 2026-11-20 (#576): the bell alone, no OSC 9/777/99; the mark is the channel, the bell its call, and the mark stays reversed until a key |
| A bell rung into a detached tmux session is not replayed when it is attached (tested on tmux 3.5a) | 2 | 5 | 1 | 10 | 3 | accepted: the mark is held reversed, with how many are new, until a key |
| The mark's glyph is the worst alert's: a critical one beside warnings shows `✖` before the count of them all, which does not say how many are critical | 2 | 5 | 1 | 10 | 2 | accepted: `:alerts` lists each, marked by its severity |
| After ten idle minutes the alerts the title bar counts are read every minute, not every 15 s: an alert that fires while the TUI is paused is counted and rung up to a minute after the box holds it (and the box's own 5 s hold); for a person with app grants only each app's at its own interval, longer with many apps (63.5 s at 40) | 2 | 5 | 1 | 10 | 1 (a shorter interval: more reads all night) | accepted: the owner's decision of 2026-11-20 (#576: 60 s while paused); the bar says the alerts are still read |
| A key that cannot sign with nobody there is asked about every nine minutes while the TUI reads through the pause (the alerts, and whoami even with no grant to watch): a hardware key (an `sk-` type, whose touch the box requires: the title bar says `TOUCH YOUR KEY to sign` and the key blinks), and a key in an agent that asks before each signature (`ssh-add -c`; a password manager's agent, such as 1Password's with Touch ID, once it has locked itself; the common setup on a Mac. A Secure Enclave key, Secretive's, is ECDSA, which the client does not take yet: the title bar says `waiting 12s for the key to sign` from 5 s on). No one answers, the signature fails (the authenticator's own timeout, or the TUI's minute), and every read stops, so the person comes back to `NOT SIGNED: r signs again`, the stop line saying why, not to PAUSED, and the alerts were watched only until then, at most about nine minutes into the pause; the mark says its count is old. What such an agent does with a prompt nobody answers (waits for ever, times out, stacks the prompts), and how long an authenticator waits for a touch, are unverified; the client cannot tell an agent that asks from one that signs at once (`ssh-add -L` shows no constraint) | 2 | 5 | 1 | 10 | 2 (a signature that fails while paused stops the watch until a key, rather than every read until `r`) or 3 (a case of its own for a hardware key) | accepted: the stop is said and named, and `r` signs again; the owner's decision of 2026-11-21 is that hardware keys get no case of their own now (`TestPausedHardwareKeyAsAnyKey`) |
| At the pause's first moment an answer applied just after it is judged by the paused minute, though its next read was scheduled 15 s on: if that read then hangs, the mark says its count is old 65 s after it, not 20 s | 2 | 2 | 3 | 12 | 1, risk 2 (the interval each read was scheduled at kept on its entry) | accepted: it takes a read that hangs in the second the pause begins |
| A signature that takes longer than 60 s at the keys (a person slow to answer an agent's prompt, or to touch a hardware key) fails, and every read stops until `r`, where before the reads waited for it | 2 | 3 | 1 | 6 | 1 (a longer deadline) | accepted: a person at the keys answers within a minute, and the stop says why |
| An alert that fires and ends within a gap of the reads (the pause, STALE, a sleep) is never counted nor rung: no read gives what fired before it | 2 | 5 | 5 | 50 | 3 (a read of what fired since, on the box) | accepted over the bar, by the owner's decision of 2026-11-20 (#576: no history of a gap); notify's channels still told it, and the mark says its count was old |
| Against a moteld older than the alerts read (a 404), the title bar says `alerts not known` (`alerts ?`) in the warning colour on every screen until ctrl-r, though the box refused nothing | 2 | 5 | 1 | 10 | 2 (a dim word of its own for a box that cannot say) | accepted: the decision of 2026-11-20 (#576, READS.md section 3): the box cannot say, and the mark never reads as none firing; `:alerts` says the 404 and why |
| While the box says it holds more alerts than it sends (`more_alerts`, past 500), every alert the watch has counted and the box no longer sends is kept as maybe firing until an answer without it, so the keys kept grow for as long as that lasts, and one not yet seen stays counted as new until a key | 2 | 2 | 3 | 12 | 2 | accepted: it takes an Alertmanager holding over 500 alerts, far past what motel posts; the count says `+` |
| For a person with app grants only, two alerts of one app that differ only by instance and begin at one evaluation (an app's rule over two paths) are one in the title bar's count and ring once: an app's alerts carry no instance, so their keys are the same. A viewer of the box, whose read carries it, counts two | 2 | 3 | 3 | 18 | 3 (the instance on an app's alerts: a wire field) | accepted: it takes two instances crossing at one evaluation; the app's screen lists both |
| For a person with app grants only, one app whose alerts read is refused, unanswered by its Alertmanager or not yet read (an app added or removed on the box, until the list is read again, at most a minute) turns the whole mark to `alerts not known` (or `alerts…`), hiding the other apps' known count, critical ones too | 2 | 3 | 3 | 18 | 2 (the known sum shown as old beside an app not known) | accepted: the mark never reads as a count it does not know; the apps' own screens show theirs |
| `ChannelFailed` told to several channels is one alert in the title bar's count, but several identical rows on `:alerts` and in the box screen's count of what fires (the wire does not carry which channel each is), from a box whose moteld is older than `ChannelFailed`'s read-only copy, or from a newer one before that copy is held (vmalert's rules alone, while moteld was not running when the failure began) | 2 | 3 | 3 | 18 | 2 (the channel on the wire, a notify change) | accepted: the owner's decision of 2026-11-20 (#576); a newer moteld sends the read-only copy alone in their place (notify/DESIGN-notify.md, "Failed delivery"), which is what is left: an older box until it is upgraded, and the minutes before moteld's copy is held |
| On a box with the metrics package whose moteld was not running when a failure began, the TUI rings twice for it: once for vmalert's addressed rows, and again when moteld's read-only copy, from a start of its own, takes their place | 2 | 2 | 3 | 12 | 3 (a vmalert rule for the read-only copy, with a series motel pushes while a hold stands: notify/DESIGN-metrics.md, "ChannelFailed, a second source") | accepted: notify/DESIGN-notify.md's row; the row the read ends on is right |
| From a box older than `ChannelFailed`'s read-only copy, with the metrics package, one `ChannelFailed` often rings twice: vmalert's post carries the minute of its evaluation as its start, earlier than motel's, and Alertmanager moves the start it holds to it, a new alert to the watch (measured 2026-11-21) | 2 | 4 | 3 | 24 | — (the box's) | fixed on a newer box: its read carries the read-only copy, which has no vmalert rule, so its start never moves; an older box keeps it until upgraded |
| The box screen's alerts line counts an alert in a state the client does not know (none Alertmanager 0.28 has: `active`, `suppressed`, `unprocessed`) as firing, and marks the line as one that needs a look | 2 | 1 | 3 | 6 | 2 | accepted: `:alerts` says the state as not known, the box's word quoted; counting such a state apart is a fourth count on the line for a state no Alertmanager sends |
| A silence's matcher's label name is the box's text, unquoted, so a name that holds `" = "x", `, which Alertmanager 0.28 takes in a UTF-8 label name, reads on the `matches` row as one more matcher | 2 | 1 | 3 | 6 | 2 | accepted: only root (or the motel user) writes a silence past motel's own route, which takes no label name; the row is labelled and cut, and `y` shows the answer |
| When the box sends its alerts or silences cut at the cap (500, 200), the box screen's line counts those sent and does not say the box holds more; `:alerts` and `motel monitor alerts` do | 2 | 3 | 3 | 18 | 2 | accepted: the line points at `:alerts`, which says so; a box holding 500 alerts at once is far past what motel posts |
| The alerts screen's "the last read failed: …" sentence, which quotes the box's words in the client's sentence, is laid on rows as every screen's failure sentence is, so a quotation in it may go on at column 2 of a row, where an alert's head without a mark begins | 2 | 1 | 3 | 6 | 2 | accepted: as on every screen; the quotation is the client's, its runs of spaces collapsed, and the failure is the client's own sentence (a refusal's, a 5xx's) |
| The backups screen's lines of the box's are cut at the window's edge, the cut marked `…` (an advice at a line's end, `` `motel-backup drill`, as root, … ``, is cut at 80 columns): the screen scrolls up and down, not sideways | 2 | 5 | 1 | 10 | 3 | accepted: the cut is marked, and `y` shows the whole answer |
| A row stopped, throttled or in backoff with a deploy in flight can have the state's word cut from its strip at 80 columns, the deploy before it taking the room: when the row is stale (`1m old, stale: ` before it), its name is near 20 characters, or its phase is a long one (`stopping_old`). Its mark and STATE still say it, on the row below the strip | 2 | 5 | 1 | 10 | 2 | accepted |
| Such a row's VERSION says only the version in flight (`stopped  → 9a1b2c3d4e5f`), so the table does not name the version that is stopped, throttled or in backoff, and the row can be read as the new version being the stopped one; the `→` marks it as on its way ("The TUI", below), and the strip and the app's screen name the stopped one | 2 | 4 | 3 | 24 | 1 (keep the running version in VERSION on these rows) | accepted over the bar, pending the owner (the skill's rule 4): VERSION said as a row in flight says it was the owner's chosen fix for #584's item 4 |
| While whoami lists a `logs` grant on an app and the box still refuses its logs read (403), the read held by that 403 is asked again at each whoami answer that follows it (whoami is asked again by the 403 itself, at most once a minute, else every 5 minutes): a refused read, and a Warn line in the box's journal (which the box writes at most once a minute for a person and an app), each time, for as long as the screen is open. It takes whoami and the logs route disagreeing about one key from one address, which neither does by design: whoami lists the grants that apply from the request's address | 2 | 2 | 1 | 4 | 2 (keep, for each refused app, whether whoami has since not listed the grant, and ask again only after that) | accepted, found by the fourth self-review round of the logs screen: the screen says the refusal and that ctrl-r asks again |
| A logs read the box answered busy (a `503` with its wait, `FailBusy`) counts as a failed read of its own (its misses), as a refusal does: a box that then stops answering is seen by that read's backoff from the count it reached (4 s, 8 s… instead of 2 s), and the read of another thing is not brought forward for it | 2 | 2 | 1 | 4 | 2 (a branch in `Done` for a busy answer) | accepted: the other reads see the outage at their own pace, and the title bar says it |
| The logs screen's footer is worked out at its frame, and each logs key when it is pressed, both from the session as it is then. A head says `ctrl-r asks again.` while the reads are neither stopped nor slowed by a 429 (`Engine.CanAskAgain`), so by the clock alone the hint can only appear, when a 429's wait ends: on a follow the box stopped with lines held, the head then grows by a row, and if that takes the lines' last row a key the frame listed does nothing until the next frame (a second at most). The other way, a key not listed that acts once, takes a 429 or a stop recorded between the frame and the key | 2 | 2 | 3 | 12 | 2, 2 (the frame's answer kept for the keys: a stored fact, as #662's third attempt was) | accepted: it takes a 429 on a follow the box stopped, a head one row from the page's foot, and a key in the second the hint changes; the next frame's footer is right |
| While every line held fits on the page, the logs screen's footer still lists `↑↓ scroll` and the scroll keys move nothing, as on a record's tail and the other screens when all fits | 2 | 5 | 1 | 10 | 2, 2 (the rows laid out counted against the page's; the full footer and its golden files change) | accepted: every line is in view, so nothing hidden is offered |
| While the logs screen follows its lines and holds no view, its footer still lists `End follow`, and End changes nothing: the rule above the lines already says `following` | 2 | 5 | 1 | 10 | 1, 2 (End offered only for a hold; the full footer and its golden files change) | accepted: End keeps the follow the rule names, and the full footer stays as #660 made it |
| Below the TUI's smallest window (60x16) the frame says only `window too small`, and the logs screen's keys still act on what it would show: k, ↑, PgUp and Home hold the view where nobody sees it, and `w` turns the wrap | 2 | 5 | 1 | 10 | 2 (a branch in every logs key for a window the TUI does not draw in) | accepted: the frame says the window is too small and what it needs; once it is large enough the rule says `held`, and End follows |
| After a read that stopped every read and `r`, a failure the logs screen said before the stop is said again until the next logs answer, 2 s or the read's wait | 2 | 2 | 3 | 12 | 1, 2 (forgetting it on a stop also cleared the failure of another opening's read) | accepted |
| The logs screen's tail after a skip (a follow more than the 5,000 lines it holds behind, `lines skipped`) leaves out each of its lines at or before the time of the newest line held, so a line of the journal's with that same microsecond, after the last one held, is not shown, nor one stamped earlier after a step of the box's clock back | 2 | 5 | 1 | 10 | 2 (keep the last line's text too, and match on both) | accepted: the skip's note says the lines between were not read, which these are among |

Owed, from the reviews of the TUI's first part (#558; scored there), of
their fixes, and of its second part; the others are fixed:

| Owed | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| Two names over 20 characters that differ only in their middle are shown longer until they differ, which can take the room of VERSION and LAST DEPLOY on every row. Where the width cannot hold both whole beside the widest STATE (with `running · 12s`, from 44 characters at 60 columns, none at 80; with `not deployed · stale`, from 37 at 60, 57 at 80) they are shown alike, and enter on one may open the other (at 80 columns its crumb then names it; at 60 the crumb is cut before the place they differ) | 2 | 5 | 3 | 30 | 3 | the owner's call: over the bar, and no cut within the width tells every such pair apart |
| The title bar's name, roles and release are whoami's last answer, shown beside the freshness of the newest answer to any read: while whoami answers (every 5 minutes) they can be up to 5 minutes older than that word says, unmarked. While its newest read has failed, the title bar says so and the answer's age before it (below) | 2 | 5 | 3 | 30 | 2 | the owner's call: over the bar; whoami's schedule is the owner's, and who am I shows the answer under its own age |
| Between reads nothing is waited for, so after a box stops answering the title bar says `live`, its age growing, until 5 s into the first read made after it (from the end of its signature, when it signs a token; then `waiting`, as "The rate" under "## The TUI" below says): for the time to the next read due and 5 s more, as long as between two reads of a box that answers and 5 s. Measured on the fake clock: 7 s on an app's screen (its read every 5 s: at most 10 s), 14 s on the apps screen with one app and refusals that come 10 s late (its app read every 30 s: at most 35 s), at most 65 s on an apps screen with no app (the list every 60 s); with more apps their reads are spread over the cycle and it is shorter | 2 | 4 | 3 | 24 | 3 (a read made only to see that the box answers, when nothing else is due: a new schedule, and requests against the rate) | the owner's call: over the bar, and closing it costs requests, which this change was held not to add; the age beside the word is never more than 5 s past what the same screen shows of a box that answers |
| `motel logs` prints each line whole after its time, and a terminal wraps a line wider than itself: an app that writes a line padded to the reader's width, with a time and text after the padding (a request it logs, with what a client sent in it), has that text begin a row of its own, where it reads as a line of its own, or as one of the client's dividers (`-- 2026-11-21 UTC, release v43 --`, a gap). The same mechanism as the accepted row on a labelled value's runs of spaces, scored there at occurrence 1; scored here at 5, since an app's line holds what anyone who reaches it may send | 2 | 5 | 3 | 30 | 3 (a layout that knows the terminal's width and indents a line's continuation rows past the time, so that no row of the app's begins at the left edge) | the owner's call: over the bar, and the fix is a new layout; in a pipe or a file no line wraps, and `--json` holds each line whole. Found while building `motel logs` |

## The TUI

`motel` alone, on a terminal, is a view over the reads above, in the same process. With no arguments at all it prints its help instead when stdout is not a terminal (a pipe, a file, `$(…)`), exit 0, and, with a line under it saying why, exit 2, when it finds no box or key to read with or no screen to draw on; either way nothing is signed or sent. A flag (`--box`, `--key`) asks for the screen, which then fails as a command does. A word that is no command, after `--` or empty (`motel "$CMD"` with `CMD` unset) too, is refused, exit 2, whatever stdout is (after `--` with no nearest command named; with `-h` beside an empty word the help is still printed). Its
packages see the box through `model.Client`, one method per command,
which `internal/cli/tui.go` alone builds over the client's reads; no
package of the TUI reaches the client, a transport or a program
(`TestTUIImports`). Every read goes through the table `model.Actions`,
each entry naming the command whose read it makes
(`TestActionsAreCommands`). It only reads. The apps screen is sorted by
what needs a look, with a strip that says why; a person with one app
opens on it, with the list under it. Its first reads, whoami and the list, are made before the screen opens: once they have waited 3 s for the box (a signature, a hardware key's touch among them, and the client's own pause before it asks again after a 401 not counted), one line on stderr says what is asked and that the screen opens when the box answers, or within a minute; an ordinary start, about four round trips of the box, says nothing. Where no screen can be drawn (no terminal, as in a CI job, a `TERM` with no entry or none that moves the cursor, such as `dumb`, or a locale that names a character set other than UTF-8 or ASCII, as `LANG=en_US.ISO-8859-1` does; one that names none, `LANG=en_US`, is taken for UTF-8, which is tcell's rule) it stops before anything is signed or sent, exit 2.
While a deploy is in flight an
app's row (one not stopped, throttled or in backoff) is marked `⟳`,
whatever its newest record says (a failed one
stays in LAST DEPLOY, `d` still opens it, and the row still counts as
needing a look), STATE says the deploy's phase (`deploying` while its
upload is received), VERSION the version in flight, as on its way
(`→ 4c2d9e7b1a05`), and the strip `deploying 4c2d9e7b1a05: soaking`; a
box that names no version in flight (one older than `deploying`) has
VERSION keep the running one and the strip say `deploying: soaking`.
A row stopped, throttled or in backoff keeps that state and its mark
(`✖`, `✖`, `▲`), and sorts by it, while a deploy that may mend it is
in flight; VERSION says the version in flight as above, and the strip
says the deploy before the row's reason (`deploying 4c2d9e7b1a05:
soaking; stopped: 3b8812aa0c4f is not running`): the strip is cut at
the screen's width, and a reason after which it came would leave it
out at 80 columns. Its app's screen says the same
state, with the `deploying` line under it.
A name longer than 20 characters is
cut in its middle, in the table and the strip, its start and its end
kept; two names that would be cut alike are shown longer, alike, until
they differ, as far as the screen leaves room for STATE beside them;
the whole name is what is opened, searched and printed. An app's
screen shows its state and deploys, and under its state (and under
the deploy's lines below, when it has them; for an app with no
version yet, after its `deployed` line) its alerts,
read while the screen is open (under their count the command that
lists them whole, `motel monitor alerts --box <host> --app <app>`,
since the screen's `y` and `P` give the status; then the first three,
each with what holds it, when it began and, on a `summary` line, its
summary, and the rest counted; the client's own words when the box has no monitor or
could not ask its Alertmanager, and on a 404 the box's own (a box older
than the read, or no app of that name now); while a deploy is in flight its
state names the current version as running (one stopped, throttled or
in backoff by its row's word), and a `deploying` line
under it the version in flight, its phase, when the deploy started and
by whom (`4c2d9e7b1a05 soaking · started 25s ago (14:12 UTC) by …`;
under "has no version yet: a deploy of it is in flight." before a first
version serves), and a `phases` line under it the phases it has entered
so far with the box's seconds, the last 4 (`… extracting 0.7s ›
preparing 3.9s › starting 1.0s › soaking 18.0s`; a record lists them
all). Following a deploy is staying on that screen: when a read no
longer shows a deploy the session saw in flight (on any screen), an
`ended` line in its place says how it ended and what serves
(`4c2d9e7b1a05 failed in soaking · 3b8812aa0c4f serves · d opens its
record`; `… succeeded · it serves`; `… ended with no outcome from the
box`, by `status --follow`'s rule), and, while no other deploy runs, a
`phases` line of the last 4 phases it passed through, one `… ` before
them standing for any earlier, the one it failed in marked `✖` (`d`
opens the record, which lists them all). It stays while the next deploy runs, when one was already
in flight at the read that saw it end, and goes when a deploy is seen
to begin after it, or the box's last deploy is another's (one begun and
ended between two reads, which the `deployed` line says). `d` there
opens the newest deploy's record, which for a deploy that failed is its
own (the line says so only then: a relaunch writes no record, nor does
a deploy refused before it began). A deploy's
record is laid out for reading a failure, its log tail last. `g` on an app's screen opens its
metrics (`motel metrics`): the window over the hour at first, `t` the
next (24h, 7d, 14d, then the hour again), esc back to the app; `g`
again opens the window last shown, and `:metrics <app> [<window>]` goes
there from anywhere. It says the window, its step and its end with
that end's age, and what its deploy marks are; then a plot a panel the
box sent and the client knows, as tall as lets the first be whole on
the page, three rows at the least (at 60x16, six rows of four dots
each), above each its
newest value with its time and age and, on a row of its own, its
highest and lowest with their times, those two again at the top and
the bottom of its axis, written alike to more places until they differ
to places fine enough that neither rounds away from its value by
more than the two are apart, both in the highest's unit (cores too
small for the label's seven cells in millicores, `0.04m`; rates in an
hour's, `1.0/h`; a pair no writing fits alike each in its own, so no
label a quiet app's numbers give is cut);
every point of one value is said once (`every point 0%`) and labelled
once, on its line's row, the bottom one at zero (values no label tells
apart, a page of memory, are drawn so too); and under it the time axis with a `^` in each column that holds a
version's latest deploy that succeeded (from the app's status, which
the screen goes on reading), broken (a space) in each column whose span
holds part of a step with no point (a deploy's `^` stands over a
break), and under it the window's two ends and how many of its steps
have a point. Points a step apart are joined; a step
with no point is a gap, never a zero or a line across it, and where a
column holds several steps the axis's break says it. A point joined to
neither is drawn across its step, two dots wide at the least, so it
shows and is never one dot of a bridge. The 5xx share and the
p95 have no point at a step with no request (nor the p95 at one of
cache hits alone): in those two, a step the requests panel has a point
at is no break, the count says how many steps had requests (a rate
above 0) and, when that differs, how many have a point, and two points
with only such steps between them are bridged by a dotted line, a dot
every second cell (with no requests panel, every step with no point is
a gap). A panel absent says why (no point in the window; the request
panels only for an app behind `observe`), and one the client does not
know is named as the box's. The box with no metrics line, a store that
did not answer, a word for the store the client does not know, and a
moteld older than the read (a 404, which the title bar does not call a
refusal) are each said in the client's words. `l` on an app's screen
opens its own lines (`motel logs`), and `:logs <app>` goes there from
anywhere, esc back to the app, for a person whose whoami lists a `logs`
grant on the app or on the box. Without one the screen says which
grant is needed and asks the box nothing, since a refused logs read is
a line in the box's journal; before whoami has answered about this key
it asks nothing either, and while whoami's newest read has failed it
goes by the answer held (a grant removed since is a 403, which asks
whoami again). It opens on the newest lines (200, the box's default),
read afresh at each opening, and follows them every 2 s (`LogsEvery`)
while it is in front: help, the `:` line and an overlay over it keep its
lines and its cursor, and it reads on from the cursor once it is in
front again, as after the idle pause. A read that stopped with lines
left after the last it sent is followed by the next at once; a run of
those that has brought 5,000 lines goes on from a tail, and a note says
the lines between were not read. The lines are laid out as `motel logs`
prints them, from the same pieces (`model.LogsDivider`,
`LogsLine.Clock`, the notes' sentences): each its time, UTC to the
second, and its text, every control character and colour code an
escape; a divider before the first line held, after a note, and
wherever the release or the day changes; the client's notes (older
lines not sent, a gap, lines skipped) as dividers among them. A line
wider than the window is cut at its edge, `…` its last cell; `w` wraps
it, as on a record (one setting for both: turned on here, a record's
tail is wrapped too), the rest on rows of its own indented past the time,
so no part of a line begins a row where a line or a divider does: the
screen lays out the rows itself, by the door's widths (never inside one
of termtext's escapes), and tview wraps nothing. A rule above them says
it follows; a row or a page up (↑, k, PgUp, Home) holds the view where it
is while lines arrive, the rule counting them (`held: 37 new lines since
· End follows`); End, or scrolling down to the newest row, follows
again. Of these keys and `w`, the footer lists only those that apply
to the frame, and one it does not list does nothing. While the page
draws the lines' rule and a row under it, the scroll keys are listed
when a line is held, End when the screen follows the lines (a follow
with no line yet too, not one the box stopped) or holds a view, and
`w`. While it draws no rule (the screen says why it shows no line, or
its head leaves the lines no row), End is listed only to let go a view
held there, which nobody sees (the grant gone, or the head filling the
page), and `w` only while the first read is asked, so the lines that
then arrive come as `w` left them.
It holds the newest 5,000 lines (`LogsKeep`), and at most 16 MiB
of their text as shown (`LogsKeepBytes`: a line of 4 KiB from the box
can be four times that made safe, eight with tabs), and says how many
older ones it dropped. The box saying the app keeps its output on the
box (`kept: off`), or naming what it keeps in a word the client does not
know, is said in the client's words, and nothing more is read until
ctrl-r. A 403 or a 404 drops the lines and the cursor, as a refused
status is dropped. `c` gives `motel logs` for the app, the tail the
screen opens on (with `--follow` it reads on as the screen does), and
its `--json` form; `y` and `P` the newest answer that brought lines
(the newest, when none has or the box said it keeps them on the box),
as `motel logs --json` prints one, its cursor
among it: the box's position in its journal, which the client holds in
memory only and prints only when `P` is pressed. The box screen shows the commit
the box runs, its Caddyfile's digest and its key for secrets, and, when
the box says the Caddyfile is not the one that commit installed, a line
under the digest that says so, as `motel box status`'s first line does.
The box says it when the installed file's digest is not the one it
recorded for the commit: an edit at the console since (the usual case),
or a push whose record could not be written (its result said `unknown`
and named `moteld box baseline`), which the box cannot tell apart;
after `moteld box baseline`, the file is compared with the one the
baseline found. A box older than that answer says nothing of it, and
nor does the screen. The box screen has a line of the box's backups
too, read beside its status: the box's verdict and the answer's age.
`:backups` (a viewer of the box) shows what the box says of them: its
verdict, when it decided (its answer is kept 10 s on the box), a row an
app with its result, its last complete backup and a restore proven,
each time with its age, the box's own lines after their levels, and
that it is the box's own claim, only as honest as the box (look from a
machine of your own: docs/backups.md). The box screen has a line of
the box's alerts as well, read beside its status: the answer's age, how
many fire (those `:alerts` marks), how many a silence or the deploy
mute holds quiet, how many of severity `info` fire when any do, and how
many silences there are. `:alerts` (a viewer of the box) shows what the
box's Alertmanager holds, as `monitor alerts` says it: each alert's
head in the client's words, marked by its severity while it fires (`✖`
critical, `▲` warning, `?` a severity or a state the client does not
know) and unmarked while held quiet, and the box's
words under it, each on a row of its own after the client's label and
cut at the window's edge; then the silences the same way, and that
adding and ending one is root's on the box. An alert's or a
silence's state and a monitor word the client does not know are said
as not known, the box's word quoted (a monitor that is not a lowercase
word is not moteld's answer); a severity that is not motel's is
`other` in the head and the box's word on a `severity` line; an
operator the client does not know is `?`. Against a moteld older than
the read (a 404) the screen and the box screen's line say so, and the
title bar does not call it a refusal. `:rules` (a viewer of the box)
and `:rules <app>` (a viewer of the app or of the box) show the rules
as `monitor rules` says them, the box's or the app's, under the
store's line: each rule's head in the client's words and the box's
words under it, each on a row of its own after the client's label and
cut at the window's edge. A rule is never marked by the client's own
reading of a number against its threshold: only a state or a level the
box said. Against a moteld older than the read (a 404) the screen says
so (an app's: or no app of that name now), and the title bar does not
call it a refusal. An app's screen does not list its rules: a box's
listing is ten to thirty rules, more than its details have room for,
and reading them would add a read to every app screen opened. The apps screen's marks and its
strip are judged from the statuses alone, as before. The title bar, the crumb, the
apps strip, an app's fields, a record's head and its log tail (while `w` is off) and who am I's and the box screen's lines holding a box's value are fitted to the
window in the screen's cells, as the door draws them (a wide character
takes two, a combining mark none, and a character the door writes as
an escape as many as the escape has): a line too wide is cut, `…` its
last cell, and the title bar's and the crumb's right side is cut only
when it alone takes the whole window or more (the apps' summary of hundreds of apps at 60 columns), the same way. `:` goes to a view in the command words (`:status blog`,
`:deploys blog <version>`, `:me`, `:box`, `:backups`, `:alerts`, `:rules`, `:rules blog`, `:metrics blog 24h`, `:logs blog`); what is typed is held to the client's grammars, and a version to the box's own (at most 64 characters; `motel deploys` sends a longer one for the box to refuse), before anything is asked, and
what is wrong is said on the prompt's line; Enter runs what is typed,
Tab takes a completion. `?` lists the keys of the screen below it,
written from the table the keys are bound from, its rows wrapped to the window (an entry's words under its words), and scrolls, as its footer says. On a screen that reads
the box (not help or a stop screen), `c` shows the `motel` line for it
(a record whose version the box's filter hid has none, and `c` and `y`
say so, and `P` prints nothing there), every argument quoted for a
POSIX shell, `--box` naming the port when it is not 443 and `--key` the
key when it was named with `--key` or `MOTEL_KEY` (on the apps screen, a row's line
too, `<app>` for the app). A command line wider than the window, in the
screen's cells, is cut at its edge (it is not wrapped: a line drawn on
two rows copies with a line feed in it), the cut marked `…`, and the
overlay says not to copy it and what prints it whole: `P` for the
screen's command (its `--json` line is that with `--json` added), `c`
then `P` on an app's screen for a row's. A command line not drawn as
it runs (a character the door writes as an escape, as below, or one of
no width, which tview draws as nothing: a soft hyphen, a zero-width
space; a `[` is drawn as it is) copies as another
command: the overlay says so, and not to copy it, and what prints it,
in the same way; it offers selecting the lines only when every one is
drawn as it is. The overlays' own words are laid on more rows to fit
the window. `y` shows the box's answer under
its command, which is cut as `c` cuts it, which it says not to copy, as
`c` does, when it is cut or not drawn as it runs, and which `c` then
`P` prints whole; the answer is indented (at most 10 levels deep, and the overlay says when a line is deeper), its strings made safe to show; its long
lines and long answers are cut for display (a line wider than the
window at its edge, as there is no scrolling sideways; lines over 2000
characters and answers over 2000 lines here), and the overlay says so
and that `P` prints it whole. `P` quits and prints what the overlay
names: the command line, or the answer. Into a pipe or a file (the
screen started with a flag, `motel --box <host> | jq`, since `motel`
alone prints its help there) the answer goes as the command's `--json` prints it: the box's JSON, with
the characters a terminal acts on that JSON lets a string carry raw (C1
controls, bidi overrides, DEL) written as JSON's own escapes, since a
pipe may end at a terminal; to a terminal it goes indented (as on the overlay) and made
safe, every line whole, and stderr says so. ctrl-r asks again for what
the screen shows, and nothing else; held down, it reads the list, a
record, whoami or the box at most once in 5 s, and the other reads keep
their turn. whoami is read every 5 minutes, every minute while who am I
is open, and soon after a 403, so a removed grant shows in the title
bar; one that got no answer is asked again at its backoff (below), not at its turn, until it answers. While the session runs and whoami's newest read has failed (an
answer the client cannot use, a 403, a 409, no answer), the title bar
puts that the read failed, and the age of its last answer, before it
(`whoami failed; 21m old: v0.7.0 · alice@…`), and who am I says why (the
client's own lines there and on the box screen, a failure's sentence among them, are laid on rows to fit the window). An
answer about another key or box (the `whoami` command's rule) stops
every read, like a refusal, and the session drops everything the
box said, before it too (who you are, the list, every status and
record, the box's answer): a box that does not answer about this key
gives no grounds to show any of its answers as yours. After `r`, the
session fills again from empty. A stop screen takes `r`, `?`, `q` and
the keys that scroll it alone, and `P` on a refused key's; help over it takes esc, `r`, `q` and the keys that
scroll it alone; nothing opens from either. A refused key's screen names every
cause the client cannot tell apart and the person line to send the
operator, which `P` prints. ctrl-c is a key while the screen holds the terminal, and quits as `q` does, exit 0; a
SIGINT, SIGTERM or SIGHUP sent to the process, or the terminal closed under the screen, ends it as an interrupt, 130, nothing said.

- **The rate.** A box allows a person 2 requests a second with a burst
  of 60. The TUI keeps to 1.5 a second and 30 at once. The box's status,
  its backups and its alerts are read every 30 s, each only while a
  screen that shows it is open (the box screen shows all three); but
  a viewer of the box reads its alerts every 15 s whatever screen is
  open, for the title bar's count (below: the same read the alerts
  screen and the box screen make, then every 15 s too), and a person
  with app grants only reads each app's alerts beside its status, at
  the status's interval and never under 30 s. Both are counted in the
  budget the other apps share: they keep 30 s to about 12 apps, and at
  40 a status comes every 33 s for a viewer of the box and every 63.5 s
  for a person with app grants only, whose alerts then come as often
  (measured on the fake clock, `TestWatchRates`); and
  the rules, the box's or one app's, every 30 s while their screen is
  open (the box evaluates the store's numbers at most every 30 s). The
  metrics screen's window is read every 30 s while it is shown
  (`SeriesEvery`, the box's own 30 s per app and window), and the app's
  status every 5 s beside it, for the deploy marks; another window is
  read in its place, the first not. The logs screen's lines are read
  every 2 s while it is in front (`LogsEvery`), 0.5 a second, counted in
  the budget the other apps share as the open app's reads are (its app
  is not the open one meanwhile: its status is read in the background);
  a read made at once for lines left takes its turn among the other
  apps' reads, by when each is due, so a chatty app's run of them
  starves none. With it open, a viewer of the box's other apps keep 30 s
  to about 23 apps (the engine's own sum, `background`). A logs `503`
  with the box's wait (its four read slots taken, by anyone, or moteld
  still starting after a restart, which the client cannot tell apart)
  waits that read alone, for the wait or 2 s when that is longer, and
  slows nothing else; it is not counted as an answer from the box, so a
  run of no answer goes on through it; its screen says the last read
  failed, and the title bar says nothing of it. The
  open app is read every 5 s, and its alerts every 15 s while its screen
  is open (`AlertsEvery`, after the box's own 5 s of Alertmanager's
  answer). Following a deploy costs no read of its own: it is the open
  app's. `status --follow` reads once in 5 s, 0.2 a second, so one
  beside a TUI keeps a person to 1.7; each person's reads count against
  their own 2, so several people following one deploy cost the box 0.2
  a second each. `logs --follow` reads once in 2 s (`LogsEvery`), 0.5 a
  second, so one beside a TUI brings a person to the box's 2, and the
  reads it makes at once after a read that stopped with lines left go
  past it, until a 429 is waited out; and each
  of its reads that sends lines is an `access: person read logs` line in
  the box's journal (docs/people.md, "What the box records"). Every other app takes its turn in the rest of the
  budget, every 30 s, or longer with more apps than that covers (about
  55 s for 60 apps, 3 minutes for 200; 57 s and a little over 3
  minutes while an app's screen is open, its alerts read too), each at
  its own place in the
  cycle. A 429 is waited out for every read, by its `Retry-After`, or
  30 s with none (the time the box's whole burst takes to refill), and
  slows the rest of the session; no answer backs off from 2 s to 60 s:
  that read alone, at its own interval or the backoff when longer (whoami at the backoff), while
  the box answers others (a 5xx on one route); every read, the title bar
  saying slow, then STALE, once reads of two different things got none,
  none answered between (a thing's first read in a row to get none brings the read of another thing next due forward to then, once, so a box whose reads fail at once, a connection refused or a 5xx, is seen within seconds: slow about 1 s after it stops answering, STALE 5 to 31 s after, the most with one app; a host that does not answer at all, each read waiting out the client's 30 s timeout, one read at a time, is seen 5 s into the first read made after it stops: from 5 s into any read not yet answered (an answer from a box that answers takes well under a second; a 429's wait, a read cancelled by leaving its screen and a stop are not waited for, nor is the box while a token is signed, a hardware key's touch among them, or the machine sleeps: a read is waited for from the end of either) the title bar says `waiting` and how long the box has been waited for, the last answer's age after it (`waiting 12s for an answer · 15s ago`), and goes on saying it after a read so waited for gets none, through the read that brings forward, until an answer or a wake; slow about 60 s after the outage begins, STALE from about 94 s, up to 120 s with one app, measured on the fake clock with up to 300 apps; until that first read nothing is asked, and the title bar says `live`, its age growing, the owed row above),
  one read at each retry: of the reads due, one of a thing none of whose reads has failed (got no answer, or was refused) since its last answer or `r`, or never read (a 429 or a read cancelled leaves the count as it was), or, when none is, the one tried longest ago, so a thing whose own route fails does not take every retry,
  until any answer from the box ends that run, the
  title bar says back, and whoami is asked again
  (a read the client fails without sending, such as a record whose
  version the box's filter hid, ends nothing).
  A 401, a token not signed, or a certificate that did not verify stops
  every read and says why; `r` asks once more, as a new start: no read before it counts as one in a row after it (for the backoff, the read brought forward, or `stale`), and one that failed before it is due at once, after whoami, not at the end of its backoff. A 403 or a 404 stops
  that read alone until `ctrl-r`. A deploy's record is read each time it
  is opened, and by `ctrl-r`, at most once in 5 s once the box has
  answered with it (asked sooner, it is read when the 5 s have passed,
  if it is still open); a read of it that fails, a 5xx too, is not asked again by
  itself, and the record read before stays on screen beside a failure
  that is not a refusal: a 403 or a 404 is said alone, as on an app's
  screen and the box screen, and drops the status, record or box's status held (not who am I's: the title bar and who am I say its age and that it failed; nor the apps list: the apps screen shows it as of its time, says the last read failed, and `y` there still gives it, which is bearable since it is said to be old and failed and each name's status is still asked, and dropped on its own refusal), so `y` and `P` have no answer of it to give (`y` says the box refused the read, or answered it 404). Ten idle minutes pause it (the logs screen's follow too, its rule then saying paused, and it reads on from its cursor at the key that resumes): every read stops but the alerts the title bar counts, which go on at a minute or more (a viewer of the box's every minute, 470 reads in the 7 h 50 min after the pause of 8 hours away and about 54 signatures, as at 15 s; a person with app grants only, each app's at the interval it had at the keys and never under a minute: 0.017 a second with 1 app, 0.083 with 5, 0.2 with 12, 0.63 with 40, measured on the fake clock, `TestAppWatchThroughThePause`; with the apps list every minute and whoami every five, which say which apps are watched, so one added or granted meanwhile is; whoami is read through the pause, whatever is watched, so a grant removed by a push and given back while away is seen, and a read a 403 held is asked again; a grant limited by `from` that drops out on another network is seen too while another grant still lets the key in, but with none left the box refuses the key (401), every read stops, and only `r` asks again), and the title bar says `PAUSED (idle 10m), alerts still read: any key resumes` (while their newest reads answered: one that failed has it say `PAUSED (idle 10m): any key resumes`, and the mark its count old; one waited for, `waiting`); an alert new meanwhile moves the mark and rings the bell as at the keys, and the mark stays reversed until the key that resumes. Every key is read for so, a hardware key too: one that cannot sign with nobody there stops the reads at its next signature (the accepted row on a key that cannot sign with nobody there).
- **What fires.** The title bar's first cells, on every screen, count
  the alerts that fire that this person may see, after the glyph of the
  worst (`✖ 3 firing`; `▲ 1 firing` when none is critical; `✖ 3` and
  `✖3` where the window is narrower, and the glyph alone where nothing
  else fits beside the freshness word, which is never cut for it). A
  viewer of the box counts the box's alerts (`GET /_motel/alerts`), a
  person with app grants only each app's (`GET
  /_motel/apps/<app>/alerts`), summed; which follows whoami's roles as
  they change. Counted is what `:alerts` marks (critical, warning, a
  severity or a state the client does not know), each once however
  many identical rows the box sends (a box whose moteld predates
  `ChannelFailed`'s read-only copy sends one a channel told; a newer
  one sends that copy alone in their place, notify/DESIGN-notify.md,
  "Failed delivery"), and not what a silence or the deploy mute holds, nor an alert
  of severity `info`; `✖ 500+` when the box held more than it sent.
  With none firing nothing is drawn; with a box that has no monitor, a
  dim `no monitor` where the long form fits (once that answer is old,
  as an old count of none: a monitor may have been added since); before the first answer
  `alerts…`. Where the count is not known (its read refused: a 403 is
  asked again when whoami next lists the role, a 404 by ctrl-r on a
  screen that shows the read (`:alerts`, the box screen, an app's
  screen for its alerts); the
  box's Alertmanager did not answer the box; a monitor word the client
  does not know; a read that failed before any answer; an answer about
  another key or box) it says `alerts not known` (`alerts ?`, `?`),
  never a zero, and the title bar's word is not turned to `refused` by
  it. Where the count held is old (the reads stopped; its read's last
  try failed, a 429, or its answer older than its interval
  and 5 s: a read or a signature that waits, a sleep) it is shown with
  `?` (`✖ 3?`, `✖ 3 firing as of 10m ago`, `0?` for none). An alert is
  new when it is counted and was not at the last answer that said what
  fires, told apart by its name, app, instance, severity and start, so
  a refire, a silence ending, the deploy mute ending and a change of
  severity are each new; nothing is new at the first answer, and after
  a gap (the pause, STALE, a sleep, an Alertmanager that did not answer
  the box) what is new against the last answer known is new once.
  While something is new and not seen the long form says how many
  (`✖ 3 firing, 1 new`) and the mark is drawn reversed, which reads
  without colour: on alternate seconds of the clock for 5 s, then still
  until a key is pressed a second or more after it arrived (any key,
  the one that resumes from PAUSED among them); the count stays while
  they fire. A read that brings something new rings the terminal's
  bell, a lone BEL (tcell's `Beep`), once however many it brings; never
  at the first answer, on a resolve, or while the count is not known;
  and at most once a minute: a bell due sooner is held, and rung once
  when the minute ends, unless by then what it was for has been seen,
  has resolved, or is no longer watched (an app no longer listed), and
  held on while the count is not known. `MOTEL_BELL=off` in the
  environment (or `no`, `false`, `0`, in any case) turns the bell off,
  and the mark is the same. What the terminal does with a
  bell is its own setting (a sound, a flash, a badge on its tab or its
  Dock icon). No desktop notification and no window title is written.
  The mark and the bell carry only the client's own words and numbers:
  no text of an alert's reaches them. `motel --demo` fires a critical
  alert 20 s after it starts and a warning one at 70 s, and resolves
  the first at 130 s, each shown at the next read.
- **The token** lasts 600 s, the longest a box takes, and is signed
  again a minute before it ends: a hardware key is touched about every
  nine minutes, and the title bar says when. It is signed for the
  alerts read through the pause too, about as often, with nobody there
  to touch a hardware key (the row on a key that cannot sign with nobody
  there). A signature
  waited for 5 s or more has the title bar say so (`waiting 12s for the
  key to sign`, an agent that asks first, say), and one that takes over
  a minute fails: every read stops, the screen says why, and `r` signs
  again. The TUI never asks for a
  passphrase or a PIN: every signature runs ssh-keygen in a session of
  its own, with no controlling terminal, and with
  `SSH_ASKPASS_REQUIRE=force` and an `SSH_ASKPASS` that fails, so a key
  that needs one fails at once. That stops the reads with the advice to
  add the key to the agent (`ssh-add <path>`; a key the agent alone
  holds is given no such advice); `r` signs again, afresh, at most once
  in 5 s. A key file with a passphrase the agent does not hold (matched
  by its bytes) is refused before the screen opens, exit 2. A hardware
  key that requires its PIN cannot be used in the TUI for now: the agent
  does not stop the PIN being asked for each signature. The commands may
  still ask on a terminal. Nothing is written to disk.
- **Every value shows its age** (on a screen too narrow for the AGE
  column, in its state: `running · 12s`), and a row whose last three
  reads, by its own interval, failed or did not come says `stale` in
  its state; the apps screen's crumb counts the stale rows, the strip
  says a stale row's reason as old and stale (`1h old, stale: stopped: …`)
  and counts them when no row needs a look, and does not say every
  app runs beside one. Ages are on the wall clock, so a laptop's sleep
  counts. A step of the clock back is seen at the first time read after
  it, and every time kept of the reads moves back by it, to within the
  second between two readings; a sleep, or a step back past 30 s, makes
  every row that is still being read stale until read again (one held
  by a 403 or a 404 waits for `ctrl-r`). A step of the clock forward cannot be told from a short sleep: one that leaves no more than 30 s between two of the clock's ticks (one a second) is counted as time passed, so the bucket fills by the step and a 429's wait or a backoff ends early by it; a longer one is a wake.
  Colour only repeats a mark (`✖ ▲ ⟳ ? ·`) or a word; `NO_COLOR`
  turns it off.
- **Box text on tview.** tview reads `[...]` as tags in table cells,
  titles and text views, and a URL tag makes a real OSC 8 link.
  `tview.Escape` does not stop that (its pattern skips a tag holding a
  `/`), so every string reaches a widget through one package,
  `internal/tui/door`, which puts a zero-width space after each `[` on
  top of `termtext`. Its setters take `door.Shown`, a struct only it
  can make. That rests on two tview internals, held by
  `TestEscapeHolds` in every widget kind used. tview also begins a row
  of a text view, or of an input field, wherever uniseg reports a
  mandatory line break, wrapping or not; `termtext` escapes every
  character that is one, but uniseg v0.4.7 reports one too after an
  emoji modifier that ends a cluster begun by an emoji base (a
  thumbs-up with a skin tone), so box text could begin a row of its
  own. `door` finds where tview would begin a row inside a text and
  writes the character before each such break as an escape
  (`\U0001f3fd`), as `termtext` writes one it will not pass; the input
  field's text that the person typed is put back as it was. A tag
  changes where: tview judges the break after a cluster a tag follows
  by the one character after the tag, where uniseg would read two (no
  break before a hyphen a digit follows), so a thumbs-up with a skin
  tone before `-0` began a row once a tag stood after it or after the
  hyphen. So `door` asks uniseg, of each cluster, what tview asks with
  a tag after it (`rowbreak.At`, a package of its own only for the
  time `make test` takes: its comment says why): that finds every break tview begins with no tag as
  well, since uniseg reads the second character only to take a break
  away, and the drawn tests below hold it. The escape is written in
  every text, tagged or not, so that the width measured is the width
  drawn. uniseg also takes no break before a hyphen a digit
  follows, the mandatory one after a line feed among them, so a line
  that began `-1` was drawn on the row above it; `door` writes a
  word joiner (U+2060), which tview draws nowhere, after each line
  feed it joins lines with (`Lines`, `Texts`, `LitLines`): a
  zero-width space there let a wrapped line break straight after its
  indent, and `TestJoinedLinesWrapAsAlone` holds a joined line to wrap
  as it does alone.
  `TestOneRow` has tview draw every code point, every
  Extended_Pictographic character with each modifier, and a modifier
  after a combining mark, in every widget kind used, each on one row.
  `TestClassesOnOneRow` draws one character of each Unicode line
  breaking and grapheme cluster class beside each other in pairs (and
  in triples in the widgets that can begin a row or are measured), on
  one row, and as wide as `door.Width` says in the widgets that hold
  the box's text; `FuzzInert` holds any string so in the same widgets,
  and that no tag in it is read; and `FuzzLine` and
  `TestClassesBesideATag` hold whole lines: one row, each cell in its
  segment's style, styles closed at the line's end, and as wide as
  `Width` says. `TestPairsBesideATag` holds one row with two
  characters after a tag, the tag before the two and between them,
  for each cluster of two or three code points before it after which
  uniseg can report a mandatory break (the others are left to
  `FuzzLine`). `TestLinesEachOnARow` holds each joined line to a row
  of its own, whatever begins the line under it, and `FuzzLine` draws
  its line first and last with `-0` between, and between two lines
  through `Texts` and `LitLines`. `FuzzOverlay` and `FuzzPrintedCommand` hold the
  overlays and the printed command to the rules above, the second
  reading the line back as a POSIX shell does.
  `TestOneDoor` fails if the screens hand tview text or make a widget
  off a short list, and `TestSafeBytes` draws the hostile fixture on a
  real terminfo screen and finds none of its attacks in the bytes.

`motel --demo` (hidden) shows the test fixtures, marked SAMPLE
DATA in the title bar, with no box.

## Layout

| Package | What |
|---|---|
| `cmd/motel` | `main`: the process's environment, signals, exit |
| `internal/cli` | the command tree (cobra), and the one place a box's strings become views |
| `internal/client` | the box and the credential, one GET with the retry rules, what each answer means |
| `internal/sshkey` | which key, the person's token, signing with `ssh-keygen` |
| `internal/render` | the human output; it takes views of `termtext.Text` and never a wire type |
| `internal/termtext` | `Text`: a string a box sent, made safe to print |
| `internal/local` | the programs it runs and the box repository it may be in |
| `internal/tui` | the TUI's screens, on tview |
| `internal/tui/door` | the one way text reaches tview |
| `internal/tui/door/rowbreak` | where tview would begin a row inside a text, for the door to escape |
| `internal/tui/model` | what the screens read and when (the refresh engine), and what they show, with no terminal library |
| `internal/tui/demo` | a box made of fixtures: `--demo`, and the TUI's tests |
| `internal/tui/hostile` | test support: the hostile fixture's attacks, and a terminal that records bytes |

A release build must be made with the workspace off (`make
build-client`, `GOWORK=off`, which `make package` runs for each
release's archive), so the binary links exactly the versions this go.mod
records, the ones Dependabot, govulncheck and a reader go by: in the
workspace Go takes each module at the highest version any workspace
module requires, which can be another. `TestWorkspaceAgrees` fails, naming
the module, when a build in the workspace would link a version this
go.mod does not record.

The module links no Caddy (`TestLinksNoCaddy`): the wire types come from
`liveswap/wire`, which imports only the standard library. The tests that
run the client against the real `liveswap_webhook` and `box_webhook`
handlers are in those modules (`liveswap/client_test.go`,
`box/client_test.go`), which may import Caddy.
