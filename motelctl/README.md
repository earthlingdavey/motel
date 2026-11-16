# motelctl

The laptop client: reads a motel box over HTTPS. **Experimental**: every
command, flag, output and exit code may change until a release's notes
say otherwise. The human output is never a contract; `--json` prints the
box's own answer.

```text
motelctl version                       the client's release and protocol
motelctl key [--key <path>] [--name <you@example.com>]
                                       the key it signs with, and the person line to send the operator
motelctl whoami                        who the box takes your key to be, and its roles from here
motelctl apps                          the apps your key may view, one a line
motelctl status <app> [--range 1h|24h] an app's status
motelctl deploys <app>                 its deploys, newest first
motelctl deploys <app> <version>       one deploy's whole record
motelctl box status                    the commit the box runs, its Caddyfile's digest, its key for secrets
motelctl tui                           screens over the same reads (below)
```

Each takes `--json`. `whoami` and `apps` are asked with your own key
only: with `MOTEL_TOKEN` set, name the key with `--key` (else exit 2,
nothing sent). `<app>` is a name, or the app's URL
(`https://deploy.example.com/blog`, as `MOTEL_URL` holds it), which names
the box too.

**Which box:** an app URL as `<app>`, else `--box`, `MOTEL_BOX`, the host
of `MOTEL_URL`, or the box repository the shell is in (inside
`<host>/`, or at its top when it holds one box).

**Which credential:** a token the person's own ssh key signs when
`--key` names the key. Otherwise `MOTEL_TOKEN`, sent as `Bearer`, when it
is set: a deploy token reads its own app on any motel today, though a
5xx from a box on a release that predates the `Motel-Version` header is
reported as "could not tell" (exit 3), since motelctl cannot tell it
from a gateway's. With `MOTEL_URL` set too, the token goes to
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
an agent that makes only P-256 keys cannot be used for now. motelctl writes
nothing to disk.

**What a box tells a key it does not take.** Nothing: every refusal is
the same 401, with no header that names motel. So after its one retry
motelctl names every cause it cannot tell apart (no line lists the key,
the address is outside every `from` list, the name is not the box's main
one, a motel too old for people's tokens), and the clock first when the
box's `Date` says it is off. A 403 whose `needs` and `scope` are words
of their grammar (`needs` a lowercase word, `scope` `app` or `box`) is
said from those two alone, and names no app; any other 403 is said as a
refusal that did not say what it needs, with the box's `error`, if any,
quoted as the box's. The box's words are never motelctl's own: a value
it prints in a sentence is held to the grammar the box holds it to, or
quoted by the one quoting function (`termtext.Quote`: cut at 200
characters, made safe to print, `"` and `\` escaped, so the closing
quote is motelctl's own), and no message carries a decoder's error,
which would quote the body. `whoami` takes an answer only when its name,
release, roles and scopes are of the box's grammars and no required
field is missing or `null`; it shows the key this command signed with
and the box it asked, and an answer naming another key or box prints
only a warning quoting the box's value, and exits 3. `Motel-Protocol` comes only on answers
to a token the box verified, and counts only as one header of 1 to 9
digits; a higher number than this client's is said on stderr, and the
command carries on. A passphrase-protected key file with no terminal to
ask on is refused with the remedy, `ssh-add` (exit 2).

**Only https.** motelctl never sends a request over plain http, and has
no flag or variable that would (`deploy.sh`'s `MOTEL_ALLOW_HTTP` is not
read).

**Exit codes:** 0 read; 1 the box answered with a failure; 2 nothing was
sent (usage, no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not
motel's answer, a 429 that stood); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (motelctl never continues past one); 130 interrupted. 7 is held
for a box too old for people's tokens.

## How box text is kept out of the client's voice

Everything a box sends is hostile until shown otherwise, and motelctl's
own sentences (its advice, its causes, its labels) must never be ones a
box wrote. A value from a box is used in one of three ways only:

1. **A checked token**: held to the grammar the server holds it to
   (an app or version name, a phase, a deploy's status, a unit, a
   commit, a digest, a role and its scope, whoami's name and release,
   each with the server code it mirrors cited beside its pattern in
   `internal/client/client.go`), or compared with what was asked (the
   app, the version, the host, the key, the box). A value outside its
   grammar makes the answer "not motel's answer", exit 3, and is not
   printed.
2. **A quotation through `termtext.Quote`**: white space collapsed,
   cut at 200 characters, every control, invisible character and
   look-alike quote mark made visible, `"` and `\` escaped, so the
   closing quote is motelctl's own; attributed ("the box says: …").
   A box-written backtick command inside a quotation is shown as the
   box's words, by design: it is inside the box's quotation.
3. **A labelled, capped value**: a free-text field (a deploy's error, a
   note, a log tail line) on a line whose label is motelctl's, made safe
   by `termtext.From` and cut at 200 characters (`…[cut]`).

No message carries a transport's or a decoder's error text once a byte
came from the peer: a fixed reason of motelctl's own is said instead.

This is held by review, the hostile fixtures and goldens
(`internal/cli/testdata/hostile-*.golden`), and one partial source test,
`TestNoHandMadeQuoteMarks`, which finds only a few shapes of a quotation
made by hand. Accepted:

| Residual | S | O | D | Score | Size of a fix | Outcome |
|---|---|---|---|---|---|---|
| A new message could splice box text without a test failing | 2 | 2 | 3 | 12 | 3 (typed message parts) | accepted |
| The TUI's marks (`▲ ✖ ⟳`) and box-drawing characters may be double width in some terminals, and misalign its columns (a plain-character mode is a later addition) | 1 | 2 | 1 | 2 | 2 | accepted |
| The client's one retry of a 401 is a request the TUI's engine does not count against its rate | 1 | 1 | 3 | 3 | 2 | accepted |
| A box string could reach a tview widget by a writer (`io.Copy`, `WriteTo` into a text view) or a method value of a setter without a test failing; held by review and the hostile goldens | 2 | 1 | 3 | 6 | 2 | accepted |
| The TUI's `post` may leave a few short-lived goroutines after quit | 1 | 2 | 3 | 6 | 1 | accepted |
| A `Retry-After` of an hour makes the TUI wait an hour: the box's word; the title bar says until when | 1 | 1 | 1 | 1 | 1 | accepted |
| A labelled value under the cap keeps its runs of spaces, so on a narrow terminal a long run can wrap the rest of the value to the start of the next row, where it can look like a label (the `hostile-status` golden shows one). Log lines keep their spacing on purpose | 2 | 1 | 3 | 6 | 2 (wrap-aware printing) | accepted |

## The TUI

`motelctl tui` is a view over the reads above, in the same process. Its
packages see the box through `model.Client`, one method per command,
which `internal/cli/tui.go` alone builds over the client's reads; no
package of the TUI reaches the client, a transport or a program
(`TestTUIImports`). Every read goes through the table `model.Actions`,
each entry naming the command whose read it makes
(`TestActionsAreCommands`). It only reads. The apps screen is sorted by
what needs a look, with a strip that says why; a person with one app
opens on it. An app's screen shows its state and deploys; a deploy's
record is laid out for reading a failure, its log tail last.

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
  bytes) is refused before the screen opens, exit 2. The commands may
  still ask on a terminal. Nothing is written to disk.
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

`motelctl tui --demo` (hidden) shows the test fixtures, marked SAMPLE
DATA in the title bar, with no box.

## Layout

| Package | What |
|---|---|
| `.` | `main`: the process's environment, signals, exit |
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

Release builds are made with the workspace off (`make build-motelctl`,
`GOWORK=off`), so the binary links exactly the versions this go.mod
records, the ones Dependabot, govulncheck and a reader go by: in the
workspace Go takes each module at the highest version any workspace
module requires, which can be another. `TestWorkspaceAgrees` fails, naming
the module, when a build in the workspace would link a version this
go.mod does not record.

The module links no Caddy (`TestLinksNoCaddy`): the wire types come from
`liveswap/wire`, which imports only the standard library. The tests that
run motelctl against the real `liveswap_webhook` and `box_webhook`
handlers are in those modules (`liveswap/motelctl_test.go`,
`box/motelctl_test.go`), which may import Caddy.
