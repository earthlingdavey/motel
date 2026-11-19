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

Each but `tui` takes `--json`. `whoami` and `apps` are asked with your own key
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
box's: no header tells a gateway's from motel's); 2 nothing was
sent (usage, no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not
moteld's answer, any 5xx, a 429 that stood); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (the client never continues past one); 130 interrupted. 7 is held
for a box too old for people's tokens.

## Getting it

No release ships the client as a binary yet, and
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
| `door.Line` does not hold its own tags against the text beside them (it asks tview where a row begins with the tags left out, and tview steps otherwise at a tag): box free text put directly after a dressed (coloured or styled) segment could begin a row, keep a style open, or be drawn wider than `Width` says; no screen puts box free text there today (it stands only after plain or label segments, which carry no tag), no test would fail if one did, and a comment on `door.Line` says so | 3 | 1 | 3 | 9 | 3 | accepted |
| The cap cuts through the client's own quotation of box text: a failure's sentence is capped (200 characters in an apps row's reason, 400 in a "the last read failed" sentence, 800 alone on a screen) after the box's value is quoted, so a quotation that runs past the cap loses its closing quote before `…[cut]`. On the apps screen, cut to the window on one row, it shows only in a window over about 230 columns; who am I, the box screen and an app's "the last read failed" lay the sentence on rows, so there it shows at any width when the quoted value is long in escapes (200 bidi overrides, each written as `\u202e`, make 1200) | 2 | 2 | 3 | 12 | 2 | accepted |
| ctrl-z on the `:` line after an answer about another key and `r` brings back what was typed there before (the input field's own undo; the stop empties the line but not its history), which can be an app's name the dropped box gave | 2 | 2 | 3 | 12 | 2 | accepted |
| whoami's name and release may spell one of the title bar's own words (`"name":"PAUSED"`, `"version":"0.0.0-SIGNED-OUT"`): within their grammars; no space, `·`, `│` or `:` can be forged | 2 | 2 | 3 | 12 | 2 | accepted |
| Who am I closed within a second of a 403 that asked whoami again puts that whoami off to its 5-minute turn: closing who am I sets whoami's next read to its last answer and 5 minutes (`Engine.MeOpen`), over the read the 403 made due now, if that read has not yet been taken | 2 | 2 | 3 | 12 | 2 | accepted |
| `c` and `y` judge whether a command line is drawn as it runs from tcell's simulation screen (`door.AsIs`): a real terminal may draw a character otherwise than tcell's tables say, and nothing here can see that | 2 | 2 | 3 | 12 | 3 | accepted |
| Two things whose reads each get no answer for reasons of their own (two routes each failing, while the box answers the rest) begin a session-wide run of no answer when their reads come back to back, none answered between: every read backs off and the title bar says slow, then STALE, until the next answer to any other read says back and asks whoami again. A thing's first read in a row to get none makes it likelier, since it brings forward the read due next, which may be the other failing one | 2 | 2 | 3 | 12 | 2 | accepted |

Owed, from the reviews of the TUI's first part (#558; scored there), of
their fixes, and of its second part; the others are fixed:

| Owed | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| A record's and an app's details are cut to the window, with no way to scroll them | 2 | 5 | 3 | 30 | 3 | owed, the owner's call: the second part's `y` shows the whole answer; a header that scrolls is new screen behaviour |
| Two names over 20 characters that differ only in their middle are shown longer until they differ, which can take the room of VERSION and LAST DEPLOY on every row. Where the width cannot hold both whole beside the widest STATE (with `running · 12s`, from 44 characters at 60 columns, none at 80; with `not deployed · stale`, from 37 at 60, 57 at 80) they are shown alike, and enter on one may open the other (at 80 columns its crumb then names it; at 60 the crumb is cut before the place they differ) | 2 | 5 | 3 | 30 | 3 | the owner's call: over the bar, and no cut within the width tells every such pair apart |
| The title bar's name, roles and release are whoami's last answer, shown beside the freshness of the newest answer to any read: while whoami answers (every 5 minutes) they can be up to 5 minutes older than that word says, unmarked. While its newest read has failed, the title bar says so and the answer's age before it (below) | 2 | 5 | 3 | 30 | 2 | the owner's call: over the bar; whoami's schedule is the owner's, and who am I shows the answer under its own age |

## The TUI

`motel tui` is a view over the reads above, in the same process. Its
packages see the box through `model.Client`, one method per command,
which `internal/cli/tui.go` alone builds over the client's reads; no
package of the TUI reaches the client, a transport or a program
(`TestTUIImports`). Every read goes through the table `model.Actions`,
each entry naming the command whose read it makes
(`TestActionsAreCommands`). It only reads. The apps screen is sorted by
what needs a look, with a strip that says why; a person with one app
opens on it, with the list under it. A name longer than 20 characters is
cut in its middle, in the table and the strip, its start and its end
kept; two names that would be cut alike are shown longer, alike, until
they differ, as far as the screen leaves room for STATE beside them;
the whole name is what is opened, searched and printed. An app's
screen shows its state and deploys; a deploy's record is laid out for
reading a failure, its log tail last. The title bar, the crumb, the
apps strip, an app's fields, a record's head and who am I's and the box screen's lines holding a box's value are fitted to the
window in the screen's cells, as the door draws them (a wide character
takes two, a combining mark none, and a character the door writes as
an escape as many as the escape has): a line too wide is cut, `…` its
last cell, and the title bar's and the crumb's right side is cut only
when it alone takes the whole window or more (the apps' summary of hundreds of apps at 60 columns), the same way. `:` goes to a view in the command words (`:status blog`,
`:deploys blog <version>`, `:me`, `:box`); what is typed is held to the client's grammars, and a version to the box's own (at most 64 characters; `motel deploys` sends a longer one for the box to refuse), before anything is asked, and
what is wrong is said on the prompt's line; Enter runs what is typed,
Tab takes a completion. `?` lists the keys of the screen below it,
written from the table the keys are bound from, its rows wrapped to the window, and scrolls, as its footer says. On a screen that reads
the box (not help or a stop screen), `c` shows the `motel` line for it
(a record whose version the box's filter hid has none, and `c` and `y`
say so, and `P` prints nothing there), every argument quoted for a
POSIX shell, `--box` naming the port when it is not 443 and `--key` the
key when it was named with `--key` (on the apps screen, a row's line
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
names: the command line, or the answer. Into a pipe or a file the
answer goes as the command's `--json` prints it: the box's JSON, with
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
puts the age of its last answer, and that the read failed, before it
(`21m old, failed: v0.7.0 · alice@…`), and who am I says why (the
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
operator, which `P` prints.

- **The rate.** A box allows a person 2 requests a second with a burst
  of 60. The TUI keeps to 1.5 a second and 30 at once. The open app is
  read every 5 s; every other app takes its turn in the rest of the
  budget, every 30 s, or longer with more apps than that covers (about
  55 s for 60 apps, 3 minutes for 200), each at its own place in the
  cycle. A 429 is waited out for every read, by its `Retry-After`, or
  30 s with none (the time the box's whole burst takes to refill), and
  slows the rest of the session; no answer backs off from 2 s to 60 s:
  that read alone, at its own interval or the backoff when longer (whoami at the backoff), while
  the box answers others (a 5xx on one route); every read, the title bar
  saying slow, then STALE, once reads of two different things got none,
  none answered between (a thing's first read in a row to get none brings the read of another thing next due forward to then, once, so a box that answers nothing is seen within seconds),
  until any answer from the box ends that run, the
  title bar says back, and whoami is asked again
  (a read the client fails without sending, such as a record whose
  version the box's filter hid, ends nothing).
  A 401, a token not signed, or a certificate that did not verify stops
  every read and says why; `r` asks once more. A 403 or a 404 stops
  that read alone until `ctrl-r`. A deploy's record is read each time it
  is opened, and by `ctrl-r`, at most once in 5 s once the box has
  answered with it (asked sooner, it is read when the 5 s have passed,
  if it is still open); a read of it that fails, a 5xx too, is not asked again by
  itself, and the record read before stays on screen beside a failure
  that is not a refusal: a 403 or a 404 is said alone, as on an app's
  screen and the box screen, and drops the status, record or box's status held (not who am I's: the title bar and who am I say its age and that it failed), so `y` and `P` have no answer of it to give (`y` says the box refused the read, or answered it 404). Ten idle minutes pause it.
- **The token** lasts 600 s, the longest a box takes, and is signed
  again a minute before it ends: a hardware key is touched about every
  nine minutes, and the title bar says when. The TUI never asks for a
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
  own. `door` asks tview's own splitter where it would begin a row
  inside a text and writes the character before each such break as an
  escape (`\U0001f3fd`), as `termtext` writes one it will not pass; the
  input field's text that the person typed is put back as it was.
  `TestOneRow` has tview draw every code point, every
  Extended_Pictographic character with each modifier, and a modifier
  after a combining mark, in every widget kind used, each on one row.
  `TestOneDoor` fails if the screens hand tview text or make a widget
  off a short list, and `TestSafeBytes` draws the hostile fixture on a
  real terminfo screen and finds none of its attacks in the bytes.

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
