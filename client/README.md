# motel

`motel`, the laptop client: reads a box over HTTPS. **Experimental**: every
command, flag, output and exit code may change until a release's notes
say otherwise. The human output is never a contract; `--json` prints the
box's own answer to a read (for `deploys <app>`, the status's `deploys`
array alone, `[]` when it has none): the same JSON value, and byte for
byte unless the body holds something a terminal acts on. A server's
encoder writes a C1 control, DEL, a bidi control and the line and
paragraph separators raw in a string: each is printed as JSON's own `\u`
escape, a byte that is not UTF-8 as `\ufffd` (what a JSON reader makes
of it), and a carriage return between values is dropped. `jq` reads the
same value, and a terminal does nothing. `version --json` and
`key --json` print the client's own object.

On a failure, stdout is the box's own body, made safe the same way, on
exit 1 or 6, and on exit 5 when the 403's body was JSON. On any other
failure (exit 2, 3, 4 or 8, or a 5 whose body was not JSON) it is
`{"error":"…"}` holding the client's own sentence, the one stderr's
last line prints after `motel: `. For exit 1, 2, 3, 4, 6 and 8, the
exit code says which of the two stdout is. For a 5, go by the object:
the client's own has `error` alone, the sentence on stderr; the box's
access check writes `needs` and `scope` beside its `error` (a plainer
403 of the box's is `{"error":"forbidden"}`). An interrupted command
(130) prints nothing on stdout.

```text
motel version                       the client's release and protocol
motel key [--key <path>] [--name <you@example.com>]
                                    the key it signs with, and the person line to send the operator
motel whoami                        who the box takes your key to be, and its roles from here
motel apps                          the apps your key may view, one a line
motel status <app> [--range 1h|24h] an app's status
motel deploys <app>                 its deploys, newest first
motel deploys <app> <version>       one deploy's whole record
motel box status                    the commit the box runs, its Caddyfile's digest, its key for secrets
motel tui                           screens over the same reads (below)
```

Each takes `--json`. `whoami` and `apps` are asked with your own key
only: with `MOTEL_TOKEN` set, name the key with `--key` (else exit 2,
nothing sent). `<app>` is a name, or the app's URL
(`https://deploy.example.com/blog`, as `MOTEL_URL` holds it), which names
the box too.

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
words a box writes (`needs` one of `viewer`, `deployer`, `key`; `scope`
`app` or `box`) is said from those two alone, and names no app; any
other 403, one naming a role a later motel adds among them, is said as
a refusal that did not say what it needs, with the box's `error`, if
any, quoted as the box's. `whoami` lists `viewer` as a word, and any
other role a box names quoted, as the box's word. The box's words are never the client's own: a value
it prints in a sentence is held to the grammar the box holds it to, or
quoted by the one quoting function (`termtext.Quote`: cut at 200
characters, made safe to print, `"` and `\` escaped, so the closing
quote is the client's own), and no message carries a decoder's error,
which would quote the body. `whoami` takes an answer only when its name,
release, roles and scopes are of the box's grammars and no required
field is missing or `null`; it shows the key this command signed with
and the box it asked, and an answer naming another key or box prints
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
box's: no header tells a gateway's from motel's); 2 nothing was
sent (usage, no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not
moteld's answer, any 5xx, a 429 that stood); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (the client never continues past one); 130 interrupted. 7 is held
for a box too old for people's tokens.

## Getting it

No release carries the client yet, and
`go install …/client/cmd/motel@<version>` does not work (the module
names its sibling `liveswap` by a local `replace`, which `go install`
refuses). Build it from a clone of this repository, with Go 1.27.1 or
later:

```sh
git clone https://github.com/smallhoursorg/motel && cd motel/client
go build -o motel ./cmd/motel
./motel version
```

A build made this way carries no release: `version` prints `(devel)`,
or a version Go derives from the commit.

## How box text is kept out of the client's voice

Everything a box sends is hostile until shown otherwise, and the
client's own sentences (its advice, its causes, its labels) must never
be ones a box wrote. A value from a box is used in one of three ways
only:

1. **A checked token**: held to the grammar the server holds it to
   (an app or version name, a phase, a deploy's status, a unit, the
   watchdog's state, the metrics' window, a
   commit, a digest, a role and its scope, whoami's name and release,
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
   note, a log tail line) on a line whose label is the client's, made safe
   by `termtext.From` and cut at 200 characters (`…[cut]`).

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
| The TUI's marks (`▲ ✖ ⟳`) and box-drawing characters may be double width in some terminals, and misalign its columns (a plain-character mode is a later addition) | 1 | 2 | 1 | 2 | 2 | accepted |
| The client's one retry of a 401 is a request the TUI's engine does not count against its rate | 1 | 1 | 3 | 3 | 2 | accepted |
| A box string could reach a tview widget by a writer (`io.Copy`, `WriteTo` into a text view), by reflection, through a type alias, or by a function or method value of a setter, and the TUI's structural tests (`TestOneDoor`, `TestTUIImports`, `TestActionsAreCommands`) be got round the same ways, without a test failing; held by review and the hostile goldens | 2 | 1 | 3 | 6 | 2 | accepted |
| A hardware key that requires its PIN cannot be used in the TUI: the TUI never asks for one, and the agent does not keep it | 2 | 1 | 1 | 2 | 3 (a PIN prompt outside the screen) | accepted |
| The TUI's `post` may leave a few short-lived goroutines after quit | 1 | 2 | 3 | 6 | 1 | accepted |
| A `Retry-After` of an hour makes the TUI wait an hour: the box's word; the title bar says until when. A longer one is cut to an hour: the client's cap, and meant (moteld's 429 says 1 s) | 1 | 1 | 1 | 1 | 1 | accepted |
| A labelled value under the cap keeps its runs of spaces, so on a narrow terminal a long run can wrap the rest of the value to the start of the next row, where it can look like a label (the `hostile-status` golden shows one). Log lines keep their spacing on purpose | 2 | 1 | 3 | 6 | 2 (wrap-aware printing) | accepted |
| An apps-list read that got no answer is asked again at the list's own interval, 60 s, not at the 2 s backoff a status gets; while no read is answered the title bar says STALE and the time of the last answer | 2 | 4 | 1 | 8 | 2 | accepted |

Owed, from the two reviews of the TUI's first part (#558; scored there). The fix PR goes
into `tui` after the TUI's second part, and `tui` does not go to `main` before it:

| Owed | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| A deploy's record is read once and kept for the session: after a rollback or a redeploy rewrites it on the box, the TUI can still show FAILED for a version that succeeded | 3 | 5 | 3 | 45 | 2 | owed: the fix PR |
| A failed ctrl-r on a record replaces the record on screen with the failure, though the session is meant to keep the last answer | 2 | 4 | 1 | 8 | 2 | owed: the fix PR, with the row above: once a record is read at each open, a lost connection would blank it without ctrl-r |
| Once the apps list is held, a later failure of its read is not shown, and the list shows no age | 2 | 4 | 5 | 40 | 2 | owed: the fix PR |
| A running app whose row is stale is counted as not running: the strip can say "2 of 2 apps are not deployed or not yours to view" | 2 | 5 | 3 | 30 | 1 | owed: the fix PR |
| The deploys table keeps its cursor by row number: when a new deploy lands, enter opens the deploy above the one meant | 2 | 5 | 3 | 30 | 2 | owed: the fix PR |
| A person with one app starts on its screen with nothing under it: the footer says `esc back`, and esc does nothing (`:apps` goes to the list) | 2 | 5 | 3 | 30 | 1 | owed: the fix PR |
| A whoami that fails without stopping the session (a release string that is not `N.N.N`, the row in the table above) leaves the title bar at "not signed in" and `live` for a key the box accepts; the reason is held and not shown | 2 | 5 | 3 | 30 | 2 | owed: the fix PR |
| An app whose first deploy failed opens to "Nothing to show", though the box sends its failed deploys | 2 | 5 | 3 | 30 | 2 | owed: the fix PR |
| A long app name (63 characters are allowed) pushes STATE, and `stale` with it, off a 60- or 80-column screen | 2 | 5 | 3 | 30 | 2 | owed: the fix PR |
| Under about 91 columns the apps table drops AGE first, so a row shows no age there, against "Every value shows its age" below | 2 | 5 | 3 | 30 | 2 | owed: the fix PR (a short age in the state cell), or that sentence, by the owner's call |
| After the wall clock steps back, nothing is read for the length of the step, and the title bar says `live` | 3 | 3 | 3 | 27 | 2 | owed: the fix PR |
| Stepping to the next or the previous record does not drop the read of the one left: a slow one holds the new screen up to 30 s | 2 | 4 | 3 | 24 | 1 | owed: the fix PR |
| A record whose read gets a 5xx is asked again ahead of every other read after each backoff: nothing else is read while its screen is open, and the title bar says STALE for a box that answers | 2 | 3 | 3 | 18 | 2 | owed: the fix PR |
| A role the box sends is shown in the title bar unquoted, as the client's own word; `motel whoami` quotes every role but `viewer` | 2 | 2 | 3 | 12 | 1 | owed: the fix PR |
| After an outage that ends in refusals (a 403, a 404) the title bar stays STALE: only a 200 clears the count of misses | 2 | 2 | 3 | 12 | 1 | owed: the fix PR |
| With a key only the agent holds, the advice after a failed signature reads "ssh-add the agent's key" | 2 | 3 | 1 | 6 | 1 | owed: the fix PR |
| A wake from sleep during a 429 wait refills the bucket from that moment, so a burst follows the wait (the wait must pass 30 s; moteld's 429 says 1 s) | 1 | 1 | 5 | 5 | 1 | owed: the fix PR |
| A negative `memory_bytes.now` (moteld never sends one) panics the memory bar: the TUI ends, exit 3, the terminal given back | 2 | 2 | 1 | 4 | 1 | owed: the fix PR |
| A record's and an app's details are cut to the window, with no way to scroll them | 2 | 5 | 3 | 30 | 3 | owed, the owner's call: the second part's `y` shows the whole answer; a header that scrolls is new screen behaviour |

## The TUI

`motel tui` is a view over the reads above, in the same process. Its
packages see the box through `model.Client`, one method per command,
which `internal/cli/tui.go` alone builds over the client's reads; no
package of the TUI reaches the client, a transport or a program
(`TestTUIImports`). Every read goes through the table `model.Actions`,
each entry naming the command whose read it makes
(`TestActionsAreCommands`). It only reads. The apps screen is sorted by
what needs a look, with a strip that says why; a person with one app
opens on it. An app's screen shows its state and deploys; a deploy's
record is laid out for reading a failure, its log tail last. `:` goes
to a view in the command words (`:status blog`, `:deploys blog
<version>`, `:me`, `:box`); what is typed is held to the client's
grammars before anything is asked, and what is wrong is said on the
prompt's line; Enter runs what is typed, Tab takes a completion. `?`
lists the keys of the screen below it, written from the table the keys
are bound from. On a screen that reads the box (not help or a stop
screen), `c` shows the `motel` line for it, every argument quoted
for a POSIX shell and `--box` naming the port when it is not 443, and
`y` the box's answer, indented, its strings made safe to show; long
lines and long answers are cut for display, and the overlay says so.
`P` quits and prints what the overlay names: the command line, or the
answer. Into a pipe or a file the answer goes byte for byte as the box
sent it, what the command's `--json` prints; to a terminal it goes
indented and made safe, every line whole (JSON may carry C1 controls
and bidi overrides, which a terminal acts on), and stderr says so.
ctrl-r asks again for what the screen shows, and nothing else; held
down, it reads the list at most once in 5 s, and the statuses keep
their turn. whoami is read every 5 minutes, every minute while who am I
is open, and soon after a 403, so a removed grant shows in the title
bar. An answer about another key or box (the `whoami` command's rule)
stops every read, like a refusal, and the session drops everything the
box said, before it too (who you are, the list, every status and
record, the box's answer): a box that does not answer about this key
gives no grounds to show any of its answers as yours. After `r`, the
session fills again from empty. A stop screen takes `r`, `?` and `q`
alone, and `P` on a refused key's; help over it takes esc, `r` and `q`
alone; nothing opens from either. A refused key's screen names every
cause the client cannot tell apart and the person line to send the
operator, which `P` prints.

- **The rate.** A box allows a person 2 requests a second with a burst
  of 60. The TUI keeps to 1.5 a second and 30 at once. The open app is
  read every 5 s; every other app takes its turn in the rest of the
  budget, every 30 s, or longer with more apps than that covers (about
  55 s for 60 apps, 3 minutes for 200), each at its own place in the
  cycle. A 429 is waited out for every read, by its `Retry-After`, or
  30 s with none (the time the box's whole burst takes to refill), and
  slows the rest of the session; no answer backs off from 2 s to 60 s.
  A 401, a token not signed, or a certificate that did not verify stops
  every read and says why; `r` asks once more. A 403 or a 404 stops
  that read alone until `ctrl-r`. Ten idle minutes pause it.
- **The token** lasts 600 s, the longest a box takes, and is signed
  again a minute before it ends: a hardware key is touched about every
  nine minutes, and the title bar says when. The TUI never asks for a
  passphrase or a PIN: every signature runs ssh-keygen in a session of
  its own, with no controlling terminal, and with `SSH_ASKPASS_REQUIRE=force`
  and an `SSH_ASKPASS` that fails, so a key that needs one fails at
  once. That stops the reads with the advice to add the key to the agent
  (`ssh-add <path>`); `r` signs again, afresh, at most once in 5 s. A
  key file with a passphrase the agent does not hold (matched by its
  bytes) is refused before the screen opens, exit 2. A hardware key
  that requires its PIN cannot be used in the TUI for now: the agent
  does not stop the PIN being asked for each signature. The commands
  may still ask on a terminal. Nothing is written to disk.
- **Every value shows its age**, and a row whose last three reads, by
  its own interval, failed or did not come says `stale` in its state.
  Colour only repeats a mark (`✖ ▲ ⟳ ? ·`) or a word; `NO_COLOR`
  turns it off.
- **Box text on tview.** tview reads `[...]` as tags in table cells,
  titles and text views, and a URL tag makes a real OSC 8 link.
  `tview.Escape` does not stop that (its pattern skips a tag holding a
  `/`), so every string reaches a widget through one package,
  `internal/tui/door`, which puts a zero-width space after each `[` on
  top of `termtext`. Its setters take `door.Shown`, a struct only it
  can make. That rests on two tview internals, held by
  `TestEscapeHolds` in every widget kind used; `TestOneDoor` fails if
  the screens hand tview text or make a widget off a short list, and
  `TestSafeBytes` draws the hostile fixture on a real terminfo screen
  and finds none of its attacks in the bytes.

`motel tui --demo` (hidden) shows the test fixtures, marked SAMPLE
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
| `internal/tui/model` | what the screens read and when (the refresh engine), and what they show, with no terminal library |
| `internal/tui/demo` | a box made of fixtures: `--demo`, and the TUI's tests |
| `internal/tui/hostile` | test support: the hostile fixture's attacks, and a terminal that records bytes |

A release build must be made with the workspace off (`make
build-client`, `GOWORK=off`; nothing in the release workflow builds
the client yet), so the binary links exactly the versions this go.mod
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
