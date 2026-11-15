# motelctl

The laptop client: reads a motel box over HTTPS. **Experimental**: every
command, flag, output and exit code may change until a release's notes
say otherwise. The human output is never a contract; `--json` prints the
box's own answer.

```text
motelctl version                       the client's release and protocol
motelctl key [--key <path>] [--name <you@example.com>]
                                       the key it signs with, and the person line to send the operator
motelctl status <app> [--range 1h|24h] an app's status
motelctl deploys <app>                 its deploys, newest first
motelctl deploys <app> <version>       one deploy's whole record
motelctl box status                    the commit the box runs, its Caddyfile's digest, its key for secrets
```

Each takes `--json`. `<app>` is a name, or the app's URL
(`https://deploy.example.com/blog`, as `MOTEL_URL` holds it), which names
the box too.

**Which box:** an app URL as `<app>`, else `--box`, `MOTEL_BOX`, the host
of `MOTEL_URL`, or the box repository the shell is in (inside
`<host>/`, or at its top when it holds one box).

**Which credential:** `MOTEL_TOKEN`, sent as `Bearer`, when it is set: a
deploy token reads its own app on any motel today. Otherwise a token the
person's own ssh key signs, made by the system `ssh-keygen -Y sign` in
the namespace `motel-access-v1@motel.smallhours.org`. The key is
`--key`, then `MOTEL_KEY`, then git's `user.signingkey` inside a box
repository, then ssh's default files, then the agent's key when it holds
exactly one. One key per command, never several; RSA keys are refused
before anything is signed. motelctl writes nothing to disk.

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
