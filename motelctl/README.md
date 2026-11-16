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
only a warning quoting the box's value, and exits 3. The release and
the box's protocol are read from whoami's body alone: no header names
motel. So nothing tells a box's own 5xx from a gateway's in front of
it, and every 5xx is said as "Something at <host> answered <code>, not
known to be motel's own answer", with its `error` quoted, and exits 3:
a box still starting, which answers a deploy token 503, reads that
way too (an accepted residual, below). A passphrase-protected key file with no terminal to
ask on is refused with the remedy, `ssh-add` (exit 2).

**Only https.** motelctl never sends a request over plain http, and has
no flag or variable that would (`deploy.sh`'s `MOTEL_ALLOW_HTTP` is not
read).

**Exit codes:** 0 read; 1 a failure answered with a sentence (a 4xx
other than the 401, 403, 404 and 429 below, a 409 for one; said as the
box's: no header tells a gateway's from motel's); 2 nothing was
sent (usage, no box or key, an RSA or ECDSA key); 3 could not tell (no answer, not
motel's answer, any 5xx, a 429 that stood); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (motelctl never continues past one); 130 interrupted. 7 is held
for a box too old for people's tokens.

## How box text is kept out of the client's voice

Everything a box sends is hostile until shown otherwise, and motelctl's
own sentences (its advice, its causes, its labels) must never be ones a
box wrote. A value from a box is used in one of three ways only:

1. **A checked token**: held to the grammar the server holds it to
   (an app or version name, a phase, a deploy's status, a unit, the
   watchdog's state, the metrics' window, a
   commit, a digest, a role and its scope, whoami's name and release,
   each with the server code it mirrors cited beside its pattern in
   `internal/client/client.go`), or compared with what was asked (the
   app, the version, the host, the key, the box). A value outside its
   grammar makes the answer "not motel's answer", exit 3, and is not
   printed. One exception, which prints nothing of the value either: a
   value that carries a marker of the box's own response filter
   (`[redacted:<KEY>]`, or one of the spellings it falls back to when a
   key is named for its own value: `[REDACTED:<KEY>]`,
   `[withheld:<KEY>]`, `<<KEY> removed>`, `[#N]`; and
   `[masked, N chars]`) is one the filter replaced
   part of, as it does to a unit named after an app whose env_file
   holds the app's name (`DB_NAME=<app>`). It is shown as
   `[hidden by the box's filter]`, motelctl's words; and the app, or a
   record's version, is taken as the one asked for when the box's
   equals it everywhere but at a marker, and the one asked for is what
   is printed.
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

A body the box's response filter withheld (a 200 whose one sentence
opens `response withheld`: an env_file it could not read, say) is said
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
| A 5xx in motel's own words (a box still starting, a box status that cannot read its files) is said as not known to be motel's and exits 3, not 1: no header tells it from a gateway's, by the owner's decision that no answer names motel. Its code and its quoted sentence are still shown | 2 | 4 | 1 | 8 | 3 (a stamp on the wire again) | accepted |
| A gateway's JSON 4xx (a 403, 404, 409) is said as "the box says", as motel's would be: no header tells them apart, and none ever did for a 4xx | 2 | 5 | 3 | 30 | 1 (say a 4xx's sentence as "it says", not "the box says") | owed: older than this change, and it rewrites every 4xx sentence and its goldens |
| A labelled value under the cap keeps its runs of spaces, so on a narrow terminal a long run can wrap the rest of the value to the start of the next row, where it can look like a label (the `hostile-status` golden shows one). Log lines keep their spacing on purpose | 2 | 1 | 3 | 6 | 2 (wrap-aware printing) | accepted |

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

The module links no Caddy (`TestLinksNoCaddy`): the wire types come from
`liveswap/wire`, which imports only the standard library. The tests that
run motelctl against the real `liveswap_webhook` and `box_webhook`
handlers are in those modules (`liveswap/motelctl_test.go`,
`box/motelctl_test.go`), which may import Caddy.
