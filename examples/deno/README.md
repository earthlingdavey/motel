# A Deno app on motel

A small app to copy as the start of your own. It serves on the unix
socket motel hands it, answers a health check, runs a migration
before each new version starts, finishes its requests on shutdown, and
deploys from GitHub Actions with no stored secret.
[motel](https://github.com/smallhoursorg/motel)'s e2e suite
builds and deploys these files, from its `examples/deno`, on every
change to motel, so what is here works. Each stable release
publishes them as the template repository
[motel-example-deno](https://github.com/smallhoursorg/motel-example-deno): **Use this template** there makes a
repository of your own, which deploys with no file edited.

| File | What it is |
|---|---|
| `main.ts` | The app: `/` and `/health`, on `$SOCKET` (or 127.0.0.1:8000 locally) |
| `migrate.ts` | The `pre_start` hook: runs before each new version starts |
| `deno.json` | Dependencies and the `dev`, `bundle` and `deploy` tasks |
| `scripts/bundle.sh` | Builds `app.tar.gz`, with the module cache inside it |
| `scripts/deploy.sh` | Tells the box to deploy a release URL (or pushes a local tarball), or to roll back |
| `.github/workflows/deploy.yml` | Build, publish a release, deploy — on every push to `main`; `Run workflow` rolls back. The box's address is the repository variable `MOTEL_URL` |
| `motel.caddy` | What the app needs from the box: the lines for its `app` block |
| `AGENTS.md` | The rules the box enforces, for you and your coding agent |

## Run it locally

```sh
deno task dev          # http://127.0.0.1:8000
```

## Put it on a box

You need a Debian 13 server with motel installed and a DNS name
pointing at it. [Your first deploy](https://github.com/smallhoursorg/motel/blob/main/docs/first-deploy.md) is the
whole path from a fresh server, with this app as its one fork; the
three steps below are its second half.

**1. Install Deno on the box, under `/usr/local`**, as root. The app's
sandbox contains `/usr` and nothing else of the host's software, so a
Deno in a home directory would not exist
for it. Use the version in `.deno-version`, which is what the workflow
builds with:

```sh
v=v$(cat .deno-version)               # from a checkout; on the box, type what the file says
f=deno-$(uname -m)-unknown-linux-gnu.zip
curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f"
curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f.sha256sum"
sha256sum -c "$f.sha256sum" &&
  apt install -y unzip && unzip -o "$f" deno -d /usr/local/bin
```

**2. Add the app to the box's Caddyfile**, with the lines from
`motel.caddy` inside its `app` block. In the box's
[repo](https://github.com/smallhoursorg/motel/tree/main/examples/box#change-the-config)
that is `<host>/Caddyfile`, committed signed and pushed to `main`; the
box template ships the Node app's block, and this app's `command`,
`pre_start` and `env` lines replace those. On a box without one, it is
`/etc/motel/Caddyfile`, as root: edit, `moteld validate --config`
it, `systemctl reload moteld`. Either way the app's part is the
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
				claim repository   your-org/example
				claim ref          refs/heads/main
				claim workflow_ref your-org/example/.github/workflows/deploy.yml@refs/heads/main
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
Then push to `main`. A run without `MOTEL_URL` stops at its first
step, saying so, and publishes nothing. The URL must be `https://`:
the request carries the run's token, so `scripts/deploy.sh` refuses
any other scheme before it mints one (`MOTEL_ALLOW_HTTP=1` lifts
that, for a local test box only). The workflow publishes the
tarball as a GitHub release and the box fetches it — so every deployed
version stays on GitHub. The box fetches the asset by its API URL with
the deploy job's own token, so a private repo deploys the same way as
a public one, and the box's
`artifact_allowlist api.github.com/repos/your-org/` is what admits it.
That job's token only reads the repository: the release is published
by a job of its own, the one whose token can write, and the build by
another that cannot. The deploy job also sends the tarball's digest,
taken in the build job before the upload, and the box refuses the
asset unless it hashes to the same.
The deploy step prints the app's status when the new version is live,
or why it was refused; a refused deploy leaves the old version
serving. A 401 means the box's `deploy_trust` for this app does not
accept the run — most often `claim repository`, `claim ref` or
`claim workflow_ref` names another repository, branch or workflow
file, or its `audience` is not what the job
asks for (`motel`, unless the job's `env:` sets `MOTEL_AUDIENCE`
beside `MOTEL_URL`, where the rollback step gets it too) — or that
`MOTEL_URL` names an app
the box does not know, which answers the same 401. The step prints
the values this run minted its token with; the box's journal
(`journalctl -u moteld`) names the app asked for and the check that
refused it, within the box's budget for failed authentications: ten a
minute from one address and a hundred a minute in all; past either,
a failure is not written until the minute passes, and the answer
stays the same 401 — except that the box failing to consult the
token's issuer is named once a minute whatever the budget, and a
re-run once the issuer is back goes through. Before reporting a 401
the step sends the request once more, 3 seconds later: the box
refuses the first token under GitHub's new signing key while it
fetches that key, and the retry goes through — unless the box had
fetched the keys in the minute before, when the journal's refusal says
from when a re-run goes through.
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
  app's own output — the migration's line, then whatever `main.ts`
  prints. `alice` is the administrator from
  [Hardening](https://github.com/smallhoursorg/motel/blob/main/docs/hardening.md#3-an-administrator-if-you-want-one),
  whose `adm` group is what reads it; root reads it too.

A re-run of every job in the same workflow run rebuilds the tarball
and replaces the release's asset with it, and the box refuses it with
a 422: versions are immutable on the box, that commit's version is
already there, and the rebuilt bytes are not the ones it runs (the
box relaunches a running version only for the very same artifact). A
re-run of the deploy job alone sends the run's last build again,
which the box relaunches only if that build is the one it runs. Push
a new commit to deploy again, or **Run workflow** with the running
version as the rollback target to relaunch it as it is.

## When the code needs a new permission

A new env var, file path or network host is a new Deno flag, and the
flags live on the box, so that a compromised build cannot grant itself
more. Change them on the box *before* you deploy code that needs them:
the running version does not mind an extra permission, and the new
one will not start without it. Remove a flag after the code that
needed it is gone. Keep `motel.caddy` here in step, so the repo
says what the code expects.

A reload never restarts the running app; the new flags apply at its
next launch (a deploy, or a restart after a crash). A rollback runs
the older code with the flags on the box today.

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
It runs the older code with the Deno flags on the box today (see
above). Roll forward the same way you deploy: push a commit.

## Deploying without CI

Given a local tarball instead of a URL, `scripts/deploy.sh` pushes it
in the request body, so a laptop build needs no release. The token
comes from `moteld deploy-token`, and motel's release binaries
are Linux only, so this is a path for a Linux machine that is not the
box (the signing key must stay off the box). With a `deploy_trust local` block
on the box whose `audience` is the box's deploy host,
`deploy.example.com` here (see
[Deploy authentication](https://github.com/smallhoursorg/motel/blob/main/liveswap/README.md#deploy-authentication-deploy_trust)):

```sh
deno task bundle
MOTEL_URL=https://deploy.example.com/example \
MOTEL_TOKEN=$(moteld deploy-token --key deploy.key --audience deploy.example.com) \
deno task deploy
```

The version defaults to the commit, and versions are immutable on the
box, so that deploys once per commit. While tracked files have
uncommitted changes the script refuses the default rather than label
them with the commit (it checks the checkout when it runs, not the
build); set a version for an uncommitted build
(`VERSION=wip-3 deno task deploy`). A version is 1 to 64 letters,
digits, `.`, `_` and `-`, not starting with `.`.
