# motel

**Deploy from `git push` to a single cheap server, with zero downtime.**
One binary that is your reverse proxy, your deploy pipeline, your rate
limiter, and your page cache, with its config and secrets kept in git
— for indie hackers, solo devs, and small
businesses running real apps on cheap servers. No Docker, no
Kubernetes, no SSH keys in CI. Powered by
[Caddy](https://caddyserver.com).

```
git push  →  CI builds app.tar.gz  →  webhook  →  motel swaps it live. Zero downtime.
```

Built in:

| Module | What it does |
|---|---|
| **[liveswap](liveswap/)** | Zero-downtime app deploys: webhook from CI, artifact download, migrations, health-gated start, atomic traffic cutover, graceful stop, versioned releases with rollback. Your apps run as systemd units under the motel user's own service manager — Node.js, Go, anything that can listen on a unix socket — surviving motel restarts and upgrades, with a continuous watchdog that restarts them on crash or sustained health failure (a restart budget and backoff bound the rate; it throttles rather than giving up on anything a retry can fix). |
| **[penaltybox](penaltybox/)** | Rate limiting driven by your app's `X-Rate-Limit-Level` hint headers — weighted sliding-window budgets, tiers, and a penalty box for clients that cross them. |
| **[box](examples/box/)** | Config from git: the box's Caddyfile lives in a repository of its own, and each signed push to `main` reaches the box, which checks the commit's signature, validates the file, installs it as root, and rolls back a reload that fails. The apps' secrets ride along, encrypted to the box's own key. CI holds no SSH key. |
| **cache** | HTTP page caching via [Souin](https://github.com/darkweak/souin) with in-memory [Otter](https://github.com/darkweak/storages) storage. |
| everything Caddy has | Automatic HTTPS, HTTP/2 + HTTP/3, the Caddyfile, the admin API — motel *is* Caddy underneath, with the modules above compiled in. |

## Install

A `.deb` for amd64 and arm64 on the
[releases](https://github.com/smallhoursorg/motel/releases) page.
[Your first deploy](docs/first-deploy.md) installs it and deploys an
app, in four steps; [Upgrading](docs/upgrading.md) is every release
after that. The package gives you `/usr/bin/motel`, a systemd
service running as the `motel` user, a starter config at
`/etc/motel/Caddyfile`, a systemd instance for the `motel`
user that stays up without anyone logging in — the thing your apps
will run under — and, for a box whose config lives in a repository,
the root applier that installs each signed push and the box's key
for secrets.

**Supported: Debian 13, on a real virtual machine.** The package
refuses to install where systemd is older than 257 (Debian 12 and
earlier, Ubuntu 24.04): each app's sandbox needs it. It installs on other Debian-based
distributions with systemd 257 or newer, but nothing else is tested.
On a host that cannot deliver the per-app sandbox — many LXC-based and
other container-style servers — motel refuses a config that runs
apps rather than serving something weaker; [liveswap/README.md](liveswap/README.md#sandbox)
has what a host has to provide.

The package also depends on `libpam-systemd`, `dbus` and
`openssh-client`, all present on a stock Debian server: liveswap runs
your apps as systemd units under the `motel` user's own service
manager, which needs `pam_systemd` to start and `loginctl` to stay
alive without a login, and the box checks each config commit's SSH
signature with `ssh-keygen`. And on `restic` (23 MB), `sqlite3` and
`ca-certificates`, for [backups](backups/README.md): a box that uses
none carries the two programs too, so that one whose image installs no
recommended package (Hetzner's Debian 13) gets them; the CA store is
what motel and restic verify every certificate against.

<details>
<summary><b>Without the package</b> — the raw binary, for other systemd hosts</summary>

The `motel_<version>_linux_<arch>.tar.gz` archives on the same page
hold the **raw binary** (plus LICENSE and a README; and
`motel-backup` with its unit files, for [backups](backups/README.md)),
for other systemd
hosts you wire up yourself — a NixOS-style distro, say. Prefer the
packages where you can: going this way you take on what the package
does for you, all of it in [packaging/](packaging/) (`nfpm.yaml` and
`postinstall.sh` are the whole list): a dedicated `motel` user, the
`Type=notify` unit `motel.service`, `loginctl enable-linger
motel` (with `libpam-systemd` installed) so the user manager exists
for your apps, the config at `/etc/motel/Caddyfile`, and, for config
pushed from a box repository, the root applier's units
(`motel-box-apply.path` and `.service`), its directories
(`motel-box.tmpfiles`) and the box's key (`motel box key`, as
root). The sandbox applies here too, so the host
needs systemd 257 or newer and has to deliver both namespaces (many
container and LXC hosts cannot) and seccomp. motel checks both from
inside a test unit at start and refuses to start rather than run your
apps without them. One case passes that check: a container host whose
existing seccomp rules leave the kernel no room for the app units'
system-call filter
([liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md), "What this
does NOT stop").

**systemd is not optional.** liveswap runs your apps as transient
units on your user's systemd manager and there is no fallback runner,
so this is not a path onto a non-systemd host. motel would still
serve there — Caddy, the cache, penaltybox — but a Caddyfile defining
any `app` refuses to start. That is also why there is no `.apk`:
Alpine runs OpenRC, and an Alpine package would install a motel
without the feature it exists for.

</details>

(A hosted APT repository with automatic updates is on the roadmap.)

**Installing and upgrading need root; everyday administering does
not.** Checking a config
(`motel validate`) needs no privilege, and reading the journal —
motel's and every app's — needs the `adm` group. The config changes
only by a signed push from the box's repository
([examples/box](examples/box)), and so do the apps' secrets kept there,
as values encrypted to the box's own key
([docs/secrets.md](docs/secrets.md)); motel itself runs
unprivileged. One thing reaches wider: `adm` reads the
whole system journal, not only motel's lines, so it is a trust
grant in its own right. The e2e suite administers its box that way.

## Getting started

[Your first deploy](docs/first-deploy.md) takes a fresh Debian 13
server to an app that deploys on every push to `main`, in four steps,
with the box's config in a repository of its own that the box applies
on each signed push.
[After the first deploy](docs/after-first-deploy.md) then covers
changing the config, secrets (whose reference is
[Secrets](docs/secrets.md)) and the way back, and
[Hardening the box](docs/hardening.md) covers SSH by key only, the SSH
port shut, and, optionally, an administrator who is not root,
Debian's updates installed on their own, and seven kernel network
modules no app needs refused.
The first two use the three directories below, which the e2e suite
builds and deploys on every change, so what they say works:

- **[examples/node](examples/node)** — a single executable (Node's own
  build), so nothing is installed on the box. Largest tarball;
  simplest box. The tutorial's default. Start your own from the
  template it is published as, [motel-example-node](https://github.com/smallhoursorg/motel-example-node).
- **[examples/deno](examples/deno)** — `deno run` with a Deno installed
  under `/usr` on the box, and the runtime's permission flags held in
  the box's Caddyfile, where a compromised build cannot widen them.
  Its template is [motel-example-deno](https://github.com/smallhoursorg/motel-example-deno).
- **[examples/box](examples/box)** — the box's config, kept in a
  private repo of its own: a Caddyfile per box (which app, which repo
  may deploy it, where the deploy webhook answers, who may sign a
  change), applied by the workflow on each signed push to `main`; the
  box checks the signatures and keeps the file it had if the new one
  does not load. The apps' secrets live there too, encrypted, one
  file per value ([docs/secrets.md](docs/secrets.md)). Its template is
  [motel-example-box](https://github.com/smallhoursorg/motel-example-box).

Both apps serve on the socket motel hands them, answer `/health`,
migrate before each version starts, stop cleanly, and deploy from
GitHub Actions with no stored secret.

[Backups](docs/backups.md) copies each app's databases and files into a
bucket off the box every hour, proves weekly that a restore would
work, and puts an app back.


## What motel is (and isn't)

- **A server product, distributed like Caddy.** Same CLI, same
  Caddyfile, same admin API — `motel run`, `motel reload`,
  `motel validate` all behave exactly as Caddy's do. The config is
  a Caddyfile, though: a JSON config (a `.json` file, or JSON posted
  to the admin API) is unsupported, because Caddy deletes every
  `"@id"` key from JSON before motel's checks can see it.
- **Made for one cheap server.** Apps run as systemd units under
  motel's own user manager, reached over unix sockets; no container
  runtime anywhere. That's why there's deliberately no Docker image.
- **Not a cluster.** Single-node by design. If you outgrow one server,
  you've outgrown motel — a good problem.

## Security

The threat model is [DESIGN-threat-model.md](DESIGN-threat-model.md);
to report something, [SECURITY.md](SECURITY.md). Four properties it
rests on:

- **The admin API is not on localhost.** It listens on a unix socket
  rather than TCP: "localhost-only" would include every app you run,
  and one SSRF bug in an app could otherwise reconfigure the server.
  Apps themselves run unprivileged, with `NoNewPrivileges`, and write
  no core dump (`LimitCORE=0`), so a crash cannot leave an app's
  memory on disk.
- **Every app is sandboxed, with no opt-out.** Apps share the
  `motel` user, so each runs in its own systemd sandbox: it sees
  its own release, its `shared/` data, its instance's socket directory
  (`run/<nonce>/`), a private `/tmp`, and the parts of the OS it needs. Nothing else on the host exists in its view —
  motel's keys and every sibling app are *absent* from its
  filesystem and invisible in its process table, not merely
  unreadable. A host that cannot deliver that refuses to start rather
  than run something weaker. What is shared by design is the network,
  for outbound calls; nothing motel runs listens on a port.
  Details: [liveswap/README.md](liveswap/README.md#sandbox).
- **Deploys are authenticated without a shared secret.** A deploy, and
  a config push, carries a short-lived token — an OIDC token from CI,
  verified against the provider's public keys, or one signed by a
  local key whose public half the box holds. Nothing an attacker can
  steal off the box lets them deploy.
- **motel cannot change its own config.** The Caddyfile is the box's
  policy — which repository may deploy each app, each runtime's
  permission flags, who may sign the next change — and only root
  writes it. A push from the box's repository reaches root through
  motel, and root installs the file only if the commit descends
  from the one the box runs and is signed by a key the running file
  names. Details: [box/DESIGN-box.md](box/DESIGN-box.md).

## Roadmap

Per-app sandboxing is shipped and unconditional — see **Security**
above for what an app can reach,
[liveswap/README.md](liveswap/README.md#sandbox) for the unit's own
settings, and [DESIGN-threat-model.md](DESIGN-threat-model.md) for why
each one is there. What is still ahead:

- **Per-app UIDs.** Apps share the unprivileged `motel` user; giving
  each its own would need a small privileged helper, so it stays a
  later milestone.
- **A hosted APT repository**, with package signing and auto-updates.

Not committed to, but worth naming: a metrics/alerts module alongside
liveswap and penaltybox. The first thing it would earn its keep on is
alerting when the watchdog is stuck in a restart loop — the watchdog
retries a crash or a health failure forever by design (only a release
that cannot run at all is refused once), so the loop itself is the
signal that a release is broken.

## Questions, bugs and ideas

They go in [Discussions](https://github.com/smallhoursorg/motel/discussions);
issues and pull requests are the maintainer's. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## Development

No local Go toolchain needed — everything runs in Docker:

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real deploys through caddytest, under a real systemd user manager
make e2e               # full stack: the liveswap and penaltybox suites against the shipped binary under systemd, restart survival + crash recovery, then signed box pushes
make lint vet tidy     # golangci-lint (gofmt-gated), go vet, go mod tidy
make tidy-check        # fails if go mod tidy would change a module's go.mod/go.sum; CI runs it on every PR
make vulncheck         # govulncheck, all modules (tool dep in go.mod — Dependabot-bumped)
make secretscan        # gitleaks secret scan: full history, or the working tree in a git worktree (same image as the CI gate)
make usage             # what building this costs (Claude Code, Actions minutes, Copilot) → usage/ CSVs + docs/footprint.md; needs gh auth login
                       #   and, once per machine, `git config --global motel.usage-machine <label>`: each machine's sessions get their own usage/claude/<label>.csv, summed on the page
make usage-claude      # the Claude Code half alone (no token), to run on each machine the sessions live on
make usage-check       # usage.py's tests, then fails if docs/footprint.md no longer matches the usage/ CSVs; CI runs it on every PR
make pin-watch-check   # pin-watch.py's manifest rules (dismissals, go.mod-held pins), in the same python image; usage-check runs it first
make fuzz              # fuzz the untrusted-input surfaces (FUZZTIME=2m per target)
make fuzz-list         # what fuzz will run (discovered per module, not listed by hand); CI runs it on every PR
make soak              # ~20min leak hunt: deploy/reload churn, goroutine/fd assertions; CI runs it per merge to main and weekly
make build             # cross-compile linux amd64/arm64
make package           # .deb via nfpm, into dist/
make install-test      # install dist/'s .deb under real systemd (Debian 13);
                       #   run `make package` first — dist/ is not rebuilt
```

`test-integration`, `e2e` and `install-test` boot systemd inside a
privileged container, which needs a cgroup-v2 Docker host — Docker
Desktop (macOS/Windows) or Linux with systemd both qualify; the targets
check and say so up front. That is also how liveswap's systemd runner
is developed and tested on a Mac: nothing here needs a Linux VM.

What building motel costs — Claude Code sessions, runner minutes,
Copilot reviews, in money, kWh and CO2 — is a committed ledger:
[docs/footprint.md](docs/footprint.md).

The repo is a Go multi-module workspace: `liveswap/` and `penaltybox/`
are lean, independently usable Caddy modules
(`xcaddy build --with github.com/smallhoursorg/motel/liveswap`).
`box/` is a module of its own but part of the product only: it imports
liveswap through a local `replace`, so it builds only from this repo,
and it runs only with the package's applier units
([box/DESIGN-box.md](box/DESIGN-box.md)). The root module
builds the product binary from `cmd/motel`, and `packaging/` turns
it into the `.deb`.

### Dependency policy

Dependabot opens weekly grouped PRs per ecosystem with a 7-day
cooldown (security updates bypass it); patch/minor bumps auto-merge
once the full CI graph is green. Two escape hatches, both declared in
[`.github/pin-watch.yml`](.github/pin-watch.yml) and enforced by the
weekly `pin-watch` workflow:

- **Pins.** Every pin must have a `watches` entry declaring the
  machine-checkable condition under which it is lifted (e.g. "caddy's
  latest release requires cel-go ≥ 0.29"): an `ignore` in
  `dependabot.yml`, or a require go.mod holds itself ahead of the last
  tag, which the entry then names as its `min_version`. The workflow
  opens an issue when a condition is met, and flags any ignore↔watch
  drift and any go.mod↔watch drift — a pin can never rot silently.
- **Alert triage.** `alert_dismissals` entries auto-dismiss Dependabot
  alerts by GHSA id, and `code_scanning_dismissals` entries do the same
  for CodeQL alerts by rule + file (with an `expect` cap so a new flow
  in the same file is never silently swallowed) — each with a reviewed
  reason and evidence comment (280 characters, what GitHub keeps; a
  longer one is refused, not posted cut), so triage decisions are
  versioned instead of buried in the UI. Unlisted alerts stay open and
  notify as normal. An entry whose alerts have all been fixed is
  flagged as stale, and an alert open again in a go.mod and package
  that had been fixed is a reintroduction, named rather than
  dismissed. An `alert_dismissals` entry may also
  name the advisory's Go vulndb ID (`govulncheck:`); `make vulncheck`
  then lets that one ID through, and fails once nothing calls it any
  more. This needs the
  `DEPENDABOT_ALERTS_TOKEN` Actions secret — a fine-grained PAT with
  **Dependabot alerts: read-write** on this repository only — because
  the Actions `GITHUB_TOKEN` cannot access the alerts API.

## Dependencies

You're being asked to install this on a production server, so
here's exactly what's in the binary and what it drags in. Module counts
are measured from `go mod graph` (deduplicated by module path); the
"pulls in" column shows what each dependency adds *beyond* what's
already in the tree above it, because the trees overlap almost
entirely. (Counts exclude the `govulncheck` tool dependency — a
`tool` directive is never compiled into the product and never
inherited by importers; it exists in go.mod so Dependabot can bump
the scanner.)

| Dependency | Pulls in (~modules) | motel | liveswap | penaltybox | box | Notes |
|---|---|:-:|:-:|:-:|:-:|---|
| [Caddy](https://github.com/caddyserver/caddy) | ~565 | ✓ | ✓ | ✓ | ✓ | The foundation — motel *is* a Caddy distribution. Commercially sponsored, strong security track record. Nearly the entire dependency tree is Caddy's. |
| [Souin](https://github.com/darkweak/souin) (`cache`) | +24 | ✓ | — | — | — | HTTP cache (RFC 7234). Solo-maintained; the e2e suite exercises it directly so drift is caught in CI. The riskiest dependency here, and still better than an in-house HTTP cache. |
| [darkweak/storages/otter](https://github.com/darkweak/storages) | +6 | ✓ | — | — | — | In-memory storage backend for Souin, wrapping [maypok86/otter](https://github.com/maypok86/otter). |
| [zap](https://github.com/uber-go/zap) | 0 (already in Caddy's tree) | ✓ | ✓ | ✓ | ✓ | Caddy's module logging API is zap; not optional for a Caddy module. |
| [go-humanize](https://github.com/dustin/go-humanize) | 0 (already in Caddy's tree) | ✓ | ✓ | — | — | A few formatting helpers in liveswap. |
| [go-systemd](https://github.com/coreos/go-systemd) (`dbus`) + [godbus](https://github.com/godbus/dbus) | +1 | ✓ | ✓ | — | — | liveswap's runner: apps are transient units created over systemd's D-Bus API on the motel user's own manager (no polkit, no root). go-systemd was already in the workspace graph; godbus is its one dependency, pure Go. |
| [go-oidc](https://github.com/coreos/go-oidc) + [go-jose](https://github.com/go-jose/go-jose) | 0 (already in Caddy's tree) | ✓ | ✓ | — | — | Deploy authentication (`deploy_trust`): OIDC discovery/JWKS verification and JWT signing/verification. Both were already transitive dependencies of Caddy; liveswap now requires them directly, and box's pushes use the same verifier through liveswap. Vetted and widely used — the deliberate alternative to hand-rolling JWT crypto. |
| [age](https://github.com/FiloSottile/age) (`filippo.io/age`) | +3 | ✓ | — | — | ✓ | Secrets: each value in the box repository is an age file encrypted to the box's own key, which root decrypts on the box into the app's env file. Chosen over SOPS, whose library measured 80 more modules in a binary root runs ([box/DESIGN-box.md](box/DESIGN-box.md), "Decisions and rejected alternatives"). |

A ✓ in the liveswap, penaltybox and box columns means that module's own
code requires the dependency. A "—" there is not "absent from that
module's graph" — the rows marked "already in Caddy's tree" arrive via
Caddy either way. The motel column is ✓ throughout: the product
binary contains every row.

Build and CI tooling never ships to users and is pinned by image tag
in `docker-compose.yml`: `golang` (toolchain), `golangci-lint`,
`gitleaks` (secret scan), `nfpm` (deb packaging) and `curl` (the e2e
runner). GitHub Actions are pinned to commit SHAs.

What keeps this honest: `govulncheck` gates every PR and runs weekly
against the fresh vulnerability database (reachable-code analysis, all
modules; the only exceptions are the reviewed `govulncheck:` entries
in `pin-watch.yml`, each with its evidence), every release is blocked until the full test matrix passes —
including installing the actual `.deb` under systemd on Debian 13, on
both architectures — and any dependency bump has to survive all of the
above before it merges.

## License

Apache-2.0. motel is powered by [Caddy](https://caddyserver.com),
[Souin](https://github.com/darkweak/souin),
[Otter](https://github.com/darkweak/storages), and
[age](https://github.com/FiloSottile/age).
