# A Node.js app on motel, as one executable

A small app to copy as the start of your own. It ships as a single
executable — a copy of Node with the app inside — so the box needs no
runtime installed. It serves on the unix socket motel hands it,
answers a health check, runs a migration before each new version
starts, finishes its requests on shutdown, and deploys from GitHub
Actions with no stored secret.
[motel](https://github.com/smallhoursorg/motel)'s e2e suite
builds and deploys these files, from its `examples/node`, on every
change to motel, so what is here works. Each stable release
publishes them as the template repository
[motel-example-node](https://github.com/smallhoursorg/motel-example-node): **Use this template** there makes a
repository of your own, which deploys with no file edited.

| File | What it is |
|---|---|
| `server.js` | The app: `/` and `/health`, on `$SOCKET` (or 127.0.0.1:8000 locally) |
| `migrate.js` | The `pre_start` hook: runs before each new version starts |
| `package.json` | The `dev`, `bundle` and `deploy` scripts (no dependencies) |
| `scripts/bundle.sh` | Builds `app.tar.gz`: `server` and `migrate` as executables |
| `scripts/deploy.sh` | Tells the box to deploy a release URL (or pushes a local tarball), or to roll back |
| `.github/workflows/deploy.yml` | Build, publish a release, deploy — on every push to `main`; `Run workflow` rolls back. The box's address is the repository variable `MOTEL_URL` |
| `motel.caddy` | What the app needs from the box: the lines for its `app` block |
| `AGENTS.md` | The rules the box enforces, for you and your coding agent |

## Run it locally

```sh
npm run dev            # http://127.0.0.1:8000
```

## Put it on a box

You need a Debian 13 server with motel installed and a DNS name
pointing at it. [Your first deploy](https://github.com/smallhoursorg/motel/blob/main/docs/first-deploy.md) is the
whole path from a fresh server, with this app as its example; the
three steps below are its second half.

**1. On an arm64 box, install `libatomic1`**, as root. The tarball
carries its own Node, and Node's arm64 build links one library a
stock Debian 13 does not ship; without it
the app fails to start with exit status 127 (`error while loading
shared libraries: libatomic.so.1`). amd64 needs nothing.

```sh
apt install libatomic1
```

**2. Add the app to the box's Caddyfile**, with the lines from
`motel.caddy` inside its `app` block. In the box's
[repo](https://github.com/smallhoursorg/motel/tree/main/examples/box#change-the-config)
that is `<host>/Caddyfile`, committed signed and pushed to `main`; the
box template ships this app's block already. On a box without one, it
is `/etc/motel/Caddyfile`, as root: edit, `moteld validate
--config` it, `systemctl reload moteld`. Either way the app's part is the
same, and a box repo's file keeps its `box` block and the deploy
host's `box_webhook` line as they are. Without those two, the file is:

```caddyfile
{
	admin unix//run/motel/admin.sock

	liveswap {
		artifact_allowlist api.github.com/repos/your-org/

		app example {
			# the lines from motel.caddy go here

			deploy_trust github {
				audience motel
				claim repository your-org/example
				claim ref        refs/heads/main
			}
		}
	}
}

example.com {
	reverse_proxy {
		dynamic liveswap example
	}
}

deploy.example.com {
	liveswap_webhook
}
```

The copy on the box is the one that counts: motel never reads
`motel.caddy` from a deploy. (motel's e2e suite instead saves
the file on its box as `/etc/motel/<app>.caddy` and writes `import`
of that path in the block.)

**3. Point the workflow at the box.** No file needs editing: in your
copy's GitHub repository, add the repository variable `MOTEL_URL`,
set to `https://deploy.example.com/example` (Settings → Secrets and
variables → Actions → Variables, or
`gh variable set MOTEL_URL -R your-org/example --body https://deploy.example.com/example`).
The workflow builds on an arm64 runner, since the executable is the
runner's own Node binary; for an amd64 box, also add
`MOTEL_RUNS_ON` set to `ubuntu-24.04`. Then push to `main`. A run
without `MOTEL_URL` stops at its first step, saying so, and
publishes nothing. The URL must be `https://`: the request carries
the run's token, so `scripts/deploy.sh` refuses any other scheme
before it mints one (`MOTEL_ALLOW_HTTP=1` lifts that, for a local
test box only). The workflow publishes the tarball as a GitHub
release and the box fetches it — so every deployed version stays on
GitHub. The box fetches the asset by its API URL with the job's own
token, so a private repo deploys the same way as a public one, and the
box's `artifact_allowlist api.github.com/repos/your-org/` is what
admits it. The job also sends the tarball's digest, taken before the
upload, and the box refuses the asset unless it hashes to the same.
The deploy step prints the app's status when the new
version is live, or why it was refused; a refused deploy leaves the
old version serving. A 401 means the box's `deploy_trust` for this
app does not accept the run — most often `claim repository` or `claim
ref` names another repository or branch, or its `audience` is not what
the job asks for (`motel`, unless the job's `env:` sets
`MOTEL_AUDIENCE` beside `MOTEL_URL`, where the rollback step gets
it too) — or that `MOTEL_URL`
names an app the box does not know, which answers the same 401. The
step prints the values this run minted its token with; the box's
journal (`journalctl -u moteld`) names the app asked for and the
check that refused it, within the box's budget for failed
authentications: ten a minute from one address and a hundred a
minute in all; past either, a failure is not written until the
minute passes, and the answer stays the same 401 — except that the
box failing to consult the token's issuer is named once a minute
whatever the budget, and a re-run once the issuer is back goes
through. Before reporting a 401 the step sends the request once more,
3 seconds later: the box refuses the first token under GitHub's new
signing key while it fetches that key, and the retry goes through —
unless the box had fetched the keys in the minute before, when the
journal's refusal says from when a re-run goes through.
A box that has just started and not yet loaded the app answers a
token it accepts with a 503 that says so, after waiting up to 5
seconds; the step sends that request once more too, after the seconds
its `Retry-After` names.

What a first deploy that worked looks like, from the laptop:

- The deploy step's output ends with the app's status JSON, holding
  `"current_version":"<the commit's first 12 characters>"`.
- `curl https://example.com/` answers `hello from <that version>
  (schema v1)`; the 503 from before the deploy is gone.
- `ssh alice@box.example.com journalctl -t motel-example` shows the
  app's own output — the migration's line, then whatever `server.js`
  prints. `alice` is the administrator from
  [Hardening](https://github.com/smallhoursorg/motel/blob/main/docs/hardening.md#3-an-administrator-if-you-want-one),
  whose `adm` group is what reads it; root reads it too.

A re-run of the same workflow run rebuilds the tarball, and the box
refuses it with a 422: versions are immutable on the box, that
commit's version is already there, and the rebuilt bytes are not the
ones it runs (the box relaunches a running version only for the very
same artifact). Push a new commit to deploy again, or **Run workflow**
with the running version as the rollback target to relaunch it as it
is.

## What the executable is, and isn't

`npm run bundle` runs Node's own single-executable build
(`node --build-sea`, Node 25.5+): each of `server.js` and `migrate.js`
is embedded in a copy of the Node binary. The tarball is about 45 MB
compressed and 300 MB unpacked, within motel's default caps, and it
runs on any Debian 13 box of the same architecture with nothing
installed beyond `libatomic1` on arm64. Inside the executable there is no `node_modules`: keep the
app dependency-free, or bundle it into one file (esbuild, say) before
the build. To run a Node that is installed on the box instead, see
the [Deno example](https://github.com/smallhoursorg/motel/tree/main/examples/deno) for the shape — `command node server.js`
with the runtime under `/usr` — which is smaller per deploy and lets
the box's Caddyfile hold the runtime's flags.

## Rolling back

Actions → deploy → **Run workflow**, with a version from the releases
page (the tag: a commit's first 12 characters) and "Use workflow from"
left on `main` — the box's `deploy_trust` pins that ref, so a run from
a tag or another branch is refused with a 401. The box relaunches
that release from its disk — the same start, health gate and cutover
as a deploy, and no build — so it is live in about twenty seconds, or
the run is red with the reason and nothing changed. A version the box
no longer holds is a 422: it keeps the five releases it deployed most
recently, the running one among them (a rollback counts as a deploy).
Roll forward the same way you deploy: push a commit.

## Deploying without CI

Given a local tarball instead of a URL, `scripts/deploy.sh` pushes it
in the request body, so a laptop build needs no release — as long as
the laptop is the box's architecture and OS (a Linux arm64 build, for
an arm64 box). The token comes from `moteld deploy-token`, and
motel's release binaries are Linux only, so this is a path for a
Linux machine that is not the box (the signing key must stay off the
box).
With a `deploy_trust local` block on the box whose `audience` is the
box's deploy host, `deploy.example.com` here (see
[Deploy authentication](https://github.com/smallhoursorg/motel/blob/main/liveswap/README.md#deploy-authentication-deploy_trust)):

```sh
npm run bundle
MOTEL_URL=https://deploy.example.com/example \
MOTEL_TOKEN=$(moteld deploy-token --key deploy.key --audience deploy.example.com) \
npm run deploy
```

The version defaults to the commit, and versions are immutable on the
box, so that deploys once per commit. While tracked files have
uncommitted changes the script refuses the default rather than label
them with the commit (it checks the checkout when it runs, not the
build); set a version for an uncommitted build
(`VERSION=wip-3 npm run deploy`). A version is 1 to 64 letters,
digits, `.`, `_` and `-`, not starting with `.`.
