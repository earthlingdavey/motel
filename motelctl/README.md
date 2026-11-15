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
is set: a deploy token reads its own app on any motel today, though a
5xx from a box on a release that predates the `Motel-Version` header is
reported as "could not tell" (exit 3), since motelctl cannot tell it
from a gateway's. With `MOTEL_URL` set too, the token goes to
`MOTEL_URL`'s box only; a command that names another box sends nothing
(exit 2). Otherwise a
token the person's own ssh key signs, made by the system `ssh-keygen -q -Y sign` in
the namespace `motel-access-v1@motel.smallhours.org`. The key is
`MOTEL_KEY`, then git's `user.signingkey` inside a box
repository, then ssh's default files, then the agent's key when it holds
exactly one. One key per command, never several; RSA keys are refused
before anything is signed. motelctl writes nothing to disk.

**What a box tells a key it does not take.** Nothing: every refusal is
the same 401, with no header that names motel. So after its one retry
motelctl names every cause it cannot tell apart (no line lists the key,
the address is outside every `from` list, the name is not the box's main
one, a motel too old for people's tokens), and the clock first when the
box's `Date` says it is off. A 403 whose `needs` and `scope` are words
of their grammar (`needs` a lowercase word, `scope` `app` or `box`) is
said from those two alone, and names no app; any other 403 is said as a
refusal that did not say what it needs, with the box's `error`, if any,
quoted as the box's (made safe to print, cut at 200 characters). The
box's words are never motelctl's own advice. `whoami` shows the key this
command signed with and the box it asked; an answer naming another key
or box is warned of and exits 3. `Motel-Protocol` comes only on answers
to a token the box verified, and counts only as one header of 1 to 9
digits; a higher number than this client's is said on stderr, and the
command carries on. A passphrase-protected key file with no terminal to
ask on is refused with the remedy, `ssh-add` (exit 2).

**Only https.** motelctl never sends a request over plain http, and has
no flag or variable that would (`deploy.sh`'s `MOTEL_ALLOW_HTTP` is not
read).

**Exit codes:** 0 read; 1 the box answered with a failure; 2 nothing was
sent (usage, no box or key, an RSA key); 3 could not tell (no answer, not
motel's answer, a 429 that stood); 4 not accepted (a 401 after one
retry); 5 not allowed (403); 6 not found (404); 8 the certificate did not
verify (motelctl never continues past one); 130 interrupted. 7 is held
for a box too old for people's tokens.

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
