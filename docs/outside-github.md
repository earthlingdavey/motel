# Deploying from outside GitHub

[Your first deploy](first-deploy.md) deploys from GitHub Actions, with
a token GitHub mints for each run and nothing stored anywhere. The box
takes more than that: a token from any other CI's OpenID Connect
(OIDC) issuer, GitLab's among them, or a token you mint yourself with
a key, from a runner that has no OIDC at all, or from a laptop. This
page walks through those: choosing the trust, a key end to end, an
app's deploy and the box's own config from another runner, and
minting a token where there is no Linux `moteld` to mint it with.

Everything here is the box's `deploy_trust` block, whose reference is
in [liveswap/README.md](../liveswap/README.md#deploy-authentication-deploy_trust).
Names are the ones from the first page.

## 1. Choosing the trust

A deploy, and a push of the box's config, carries one bearer token,
and the box accepts it if a `deploy_trust` block for that app (or the
`box` block, for config) verifies it. Four presets:

| Preset | Who mints the token | What the box stores | What it needs pinned |
|---|---|---|---|
| `github` | GitHub Actions, per run | nothing | `audience`, and `claim repository` with `claim ref` |
| `gitlab` | GitLab CI, per job (`id_tokens:`); `issuer https://gitlab.example.com` for a self-hosted instance, `https://gitlab.com` otherwise | nothing | `audience`, and `claim project_path` with `claim ref_path` |
| `oidc` | any other OIDC issuer: `issuer <url>` | nothing | `audience`, `issuer`, and `subject` (the issuer's own form of `sub`) |
| `local` | you, with `moteld deploy-token` and a private key | the public half of your key, a file on the box | `public_key`, and `audience` (the box's name) |

What each costs:

- **OIDC** stores nothing. The box fetches the issuer's public keys
  over https and checks each token against them. A token is minted for
  the job and lives for as long as the issuer says, on the issuer's
  clock: ending or cancelling the job revokes nothing, and a token that
  leaks from a log deploys until that expiry. An `audience` alone is refused when
  the config loads: every project on the issuer can mint a token for
  any audience, so the block must also pin who, with an identity claim
  (`repository`, `project_path`, or `sub` for the generic preset). Pin
  the branch too, or a workflow edited on any branch deploys; for
  `github` and `gitlab` the box warns at load about a block that does
  not. The generic `oidc` preset gets no such warning, because the
  shape of its `sub` is the issuer's: check what the subject you pin
  scopes to, and when it names a project but no branch, pin a branch
  claim of the issuer's own beside it.
- **A key** is a stored secret: the private half sits in the runner's
  secret store, or on a laptop, and whoever reads it can deploy until
  you remove the public half from the box. For an app, the key is the
  whole deploy credential. Pinning a `claim` on a `local` block adds
  nothing to that: the key's holder writes the claims when they mint,
  so a pin only says what the token must contain, not who holds the
  key. For the box's config the key is weaker than it looks: a push
  applies only when a key on the box's own `signer` list signed every
  commit, so a stolen deploy key sends config but cannot land any.

Use OIDC where the runner has it. A key is for a runner that does
not, and for a laptop.

## 2. A key, end to end

On a Linux machine with `moteld` on it, or in a container
([section 5](#5-minting-without-a-linux-moteld)):

```sh
moteld deploy-keygen --out example.key
```

```
wrote private key example.key (0600) and public key example.key.pub
```

An ed25519 pair: the private key as PKCS#8 PEM, the public key as
PKIX PEM. The command never overwrites: a name that exists is refused,
so a second key gets a second `--out`.

**Where each half goes.** The public key is a file the box reads, so
it goes to the box over SSH, as root, before the config that names it:

```sh
scp example.key.pub root@box.example.com:/etc/motel/example.pub
```

Then, in the box's repository, the app's block trusts it:

```caddyfile
app example {
	# …
	deploy_trust local {
		public_key /etc/motel/example.pub
		audience deploy.example.com
	}
}
```

Commit, signed, and push: the box validates the file (the key must
already be there, or the push is refused naming the path), installs
it and reloads. The `audience` names this box: a token minted for
another box that trusts the same key is refused here, and the box
warns at load about a `local` block with none.

The private key goes into the runner's secret store (a GitLab CI
variable of type *File*, say), or stays in a directory of your own on
the laptop. Nowhere else: not in the app's repository, not on the
box.

**The check job.** The box repository's `make check` runs `moteld
validate` on each box's file as it stands in the checkout, and
validating loads every `public_key` from the path the file names. On
the runner that path is empty, and the check fails naming it. The
public key is public, so commit a copy beside the file and put it in
place in the check job before `make check`:

```sh
sudo install -D -m 644 deploy.example.com/example.pub /etc/motel/example.pub
```

(A GitLab job on the Docker executor runs as root in an image with no
`sudo`: drop the word there.)

**Minting.** One token per run, as late as the step that uses it:

```sh
MOTEL_TOKEN=$(moteld deploy-token --key example.key --audience deploy.example.com)
```

The token lives for `--ttl` from now, five minutes unless you say
otherwise, and whoever reads it can deploy until then. The box refuses
one minted to live longer than its block's `max_token_ttl`, 24 hours
unless the block sets one, and the command warns on stderr above an
hour. `--subject <name>` puts a `sub` claim in it, which the deploy record
and the app's status show as `deployed_by`, and the journal's `deploy
authorized` line as `via`; `--claims k=v,k2=v2` adds any other claim.

**One key per app, and one for the box.** A key is the credential for
what trusts it, so give each app its own, and the box's `box` block
another. A key removed from one block then stops one thing.

**Rotation.** Make the new pair, copy its public half to the box under
a new name, add a second `deploy_trust local` block beside the old one,
pointing at it, and a second `install` line in the check job for the
new `.pub`. Push that. Swap the runner's secret for the new private
key. Then remove the old block, push again, and delete the old
`.pub` from the box. Two blocks accept either token in between, so no
run is refused on the way. A key you believe stolen skips the overlap:
remove its block and push.

## 3. An app's deploy from another runner

The app templates deploy with `scripts/deploy.sh`, which runs anywhere
there is `sh`, `curl` and git. Outside GitHub
Actions it reads the token from `MOTEL_TOKEN` and otherwise behaves
as the first page describes. What it takes:

| Variable | What it is |
|---|---|
| `MOTEL_URL` | the app's webhook, `https://deploy.example.com/example`; `https://` only, since the request carries the token |
| `MOTEL_TOKEN` | the token (section 2's, or the CI's own OIDC token) |
| `VERSION` | the release's version; the commit's first 12 characters unless set. Versions are immutable on the box, so the default deploys once per commit |

There are two ways to hand the box the release:

- **Push the tarball in the request** — `scripts/deploy.sh app.tar.gz`
  with a file. No release host, and no `artifact_allowlist` entry has
  to match it: the bytes come straight from the authenticated caller,
  capped at the app's `max_artifact_size`. The directive itself stays
  required, since an app with no allowlist at all is refused when the
  config loads; keep the template's entry. This is the path for a runner that
  publishes nothing, and for a laptop. The tarball itself must be
  built on Linux, for the box's architecture: the Node template's
  executable is the builder's own Node, and the box refuses a
  mismatch at deploy time (its message names a GitHub runner label,
  which is the hint for the GitHub workflow; elsewhere, pick the
  runner yourself). A Mac pushes a tarball built elsewhere. The
  checkout must be clean of uncommitted changes to tracked files, or
  the script refuses to label the build with the commit's name: set
  `VERSION` for an uncommitted build.
- **Give the box a URL** — `scripts/deploy.sh https://…/app.tar.gz`.
  The box fetches it, so the URL's host must be in the box's
  `artifact_allowlist`, path pinned on a shared host. Two more
  variables then help: `ARTIFACT_AUTH_HEADER`, sent verbatim as
  `Authorization` on the fetch, for a private host; and
  `ARTIFACT_SHA256`, the tarball's digest, the first field of
  `sha256sum`'s output (64 hex characters, nothing else), so the box
  refuses a download that hashes to anything else. The GitHub workflow
  sends both.

A job on **GitLab CI**, with the Node template's build and GitLab's
own token (`id_tokens:` sets the variable it names to a fresh OIDC
token for the job, with the audience given). The `tags:` line picks a
runner of the box's architecture, here gitlab.com's hosted arm64
runner for the first page's default box; gitlab.com's default runners
are amd64, and an amd64 box needs no line:

```yaml
deploy:
  stage: deploy
  image: node:26
  tags: [saas-linux-small-arm64]
  # One deploy at a time, as the template's workflow has it: the box
  # answers a second deploy of the same app 409 while one is running.
  resource_group: deploy
  rules:
    - if: $CI_COMMIT_BRANCH == "main"
  id_tokens:
    MOTEL_TOKEN:
      aud: motel
  variables:
    MOTEL_URL: https://deploy.example.com/example
  script:
    - npm run bundle
    - scripts/deploy.sh app.tar.gz
```

and the app's block on the box:

```caddyfile
deploy_trust gitlab {
	audience motel
	claim project_path your-group/example
	claim ref_path refs/heads/main
}
```

That shape is written from GitLab's documentation of its ID tokens,
its claims and its runners, and from the box's code. It has not been
run against a real GitLab project; if you do, say so in
[Q&A](https://github.com/smallhoursorg/motel/discussions/new?category=q-a). The
same job publishing the tarball to GitLab's package registry for the
box to fetch, with `curl` in place of the script, is in
[liveswap/README.md](../liveswap/README.md#gitlab-ci).

Any **other OIDC issuer** (CircleCI, Buildkite, a Kubernetes cluster)
is the `oidc` preset: its `issuer`, the `audience` you asked the
provider to mint for, and the `subject` the provider puts in `sub`,
whose form is the provider's own. The box requires the subject: with
only an audience, every workload on the issuer could deploy.

```caddyfile
deploy_trust oidc {
	issuer https://oidc.circleci.com/org/<org-id>
	audience motel
	subject <the provider's sub for this project>
}
```

With a **key** on a runner without OIDC, the job mints its own token
and the rest is the same:

```sh
MOTEL_TOKEN=$(moteld deploy-token --key "$DEPLOY_KEY" --audience deploy.example.com) \
MOTEL_URL=https://deploy.example.com/example scripts/deploy.sh app.tar.gz
```

where `DEPLOY_KEY` is the path the secret store wrote the private key
to, and `moteld` is on the runner's `PATH`
([section 5](#5-minting-without-a-linux-moteld) has how it gets there).
One token serves the one request, so the default five minutes is
enough: the box checks it when the request arrives, however long the
deploy then takes.

A **401** from any of these means the box's block for the app does
not accept the token, or the URL names an app the box does not have (a
typo in `MOTEL_URL` answers the same 401). The script says what the
box holds the token against: the key, the audience, the subject and
claims the block pins. Outside GitHub Actions it cannot print the
values the token carries, which are the issuer's, or whatever
`deploy-token` was given, so hold those against the block yourself.
`MOTEL_AUDIENCE` changes none of this: it names the audience only
for the token the script mints in GitHub Actions, and here the token
already has one, from `--audience` or a GitLab job's `aud:`.
With an issuer's token there is a third cause, and a key's token never
meets it: the box could not consult the token's issuer (an outage
there), or had fetched its keys in the minute before the issuer
changed them, and no token waits on an issuer, so the first OIDC token
after a moteld restart or reload can reach a box that holds no keys
yet. The box's journal says which, and a re-run goes through. The
box's own answer says no more, by design; its journal names the check
that refused it (`journalctl -u moteld`, the `webhook auth failed`
lines).

## 4. The box's config from another runner

Only the workflow file in the box's repository is GitHub's. `bin/apply`
is what talks to each box, and any runner that gives it what the
Actions job does can run it:

- **A checkout with its full history.** The box checks every commit
  between the one it runs and the one being sent, and `bin/apply`
  bundles them from the checkout; a shallow clone is refused with a
  message saying so. On GitLab, that is the project's *Git shallow
  clone* setting, or `GIT_DEPTH` set to `0` on the job.
- **`moteld` on `PATH`.** `bin/apply` runs `moteld box webhook` to
  read each box's address from its file. `bin/install-moteld
  default` installs the latest release's binary on a Linux runner with
  `curl`, `sha256sum`, `tar` and `sudo` on it; a GitLab job on the
  Docker executor has no `sudo`, so `apt-get install -y sudo` first,
  or take the release tarball yourself as
  [section 5](#5-minting-without-a-linux-moteld) does and put the
  binary on `PATH`. `MOTELD=/path/to/moteld` names it when it is
  not on `PATH`.
- **`MOTEL_TOKEN`**, a token the `box` block's `deploy_trust`
  accepts, carrying a `sha` claim that names the commit being sent.
  The box refuses a bundle for any other commit. `bin/apply` sends
  this one token with every request, the result polls included, and a
  push can be polled for up to 30 minutes, so the token must outlive
  the run: `--ttl 1h`.

With a key, the `box` block trusts it like an app's would, and the
job mints:

```sh
MOTEL_TOKEN=$(moteld deploy-token --key "$BOX_KEY" --audience motel --ttl 1h --claims sha=$(git rev-parse HEAD)) bin/apply
```

The audience here is not one box's name, as an app's is in
[section 2](#2-a-key-end-to-end): `bin/apply` sends its one token to
every box in the repository, and `deploy-token` writes one audience,
so the `box` blocks of those boxes share one name. A token for one
box is then accepted by the others that trust the key, which is what
the run is for.

motel's own end-to-end suite runs `bin/apply`, unchanged, this
way: a `deploy_trust local` block on the box, a token naming the
commit, every answer the real box's.

With **GitLab's token** the `sha` claim comes for free: GitLab's ID
tokens carry the pipeline's commit as `sha`, so a `box` block of

```caddyfile
box {
	deploy_trust gitlab {
		audience motel
		claim project_path your-group/boxes
		claim ref_path refs/heads/main
	}
	# signers
}
```

and a job with `id_tokens: { MOTEL_TOKEN: { aud: motel } }`, a
full clone, `moteld` installed, and `bin/apply` as its script, is
the Actions job on GitLab. One more line it needs: GitLab sets the
token's expiry to the job's `timeout:`, or to five minutes when the
job sets none ([GitLab's ID token documentation](https://docs.gitlab.com/ci/secrets/id_token_authentication/)),
and `bin/apply` polls a push with that one token for up to 30
minutes, so give the job a `timeout:` above that. Untested, as
above.

Whatever mints the token, the rules of the box's repository hold: a
commit no listed key signed is refused, and so is a merge button that
rewrites one. The runner sends config; the signatures decide whether
it lands.

## 5. Minting without a Linux `moteld`

moteld is built for Linux on amd64 and arm64, and nothing else:
`moteld deploy-token` does not run on a Mac. Two ways round it.

**A Linux container.** Each release's bare binary is on its releases
page as `moteld_<version>_linux_<arch>.tar.gz`, beside a
`checksums.txt`, and runs in any Debian 13 container. To mint, with
the private key in the current directory, which the container sees
read-only at `/w` and writes nothing into:

```sh
docker run --rm -v "$PWD:/w:ro" -w /tmp debian:13-slim sh -c '
  apt-get -qq update && apt-get -qq install -y curl ca-certificates >/dev/null &&
  v=X.Y.Z && arch=$(dpkg --print-architecture) &&
  curl -fsSLO https://github.com/smallhoursorg/motel/releases/download/v$v/moteld_${v}_linux_$arch.tar.gz &&
  curl -fsSLO https://github.com/smallhoursorg/motel/releases/download/v$v/checksums.txt &&
  sha256sum -c --ignore-missing checksums.txt >/dev/null &&
  tar -xzf moteld_${v}_linux_$arch.tar.gz moteld &&
  ./moteld deploy-token --key /w/example.key --audience deploy.example.com'
```

Set `v` to the release the box runs, from its releases page. The same
command makes a key: mount the directory writable, `-v "$PWD:/w"`, and
end with `./moteld deploy-keygen --out /w/example.key` in place of
the last line; the pair lands in the current directory, the private
half mode 0600. Each run downloads the release again; a token a day is
fine that way, a token an hour wants the binary kept in an image of
your own. The same tarball is how a runner that is not Debian gets its
`moteld`: unpack it and put the binary on `PATH`.

**Any JWT library.** A `local` token is a plain JSON Web Token, so
anything that signs one can mint it, in any language on any machine:

- Signed with the private key from `deploy-keygen` (ed25519, PKCS#8
  PEM), algorithm `EdDSA`. The box accepts no other algorithm on a
  `local` block.
- Header `{"alg":"EdDSA","typ":"JWT"}`.
- Claims the box checks:
  - `aud`: the block's `audience`, as a string or in an array;
  - `iat` and `exp`, both required, in seconds; `exp` at most
    `max_token_ttl` after `iat`; `exp` not yet past and `iat` not in
    the future, each with 60 seconds of leeway for clock skew;
  - every `claim` the block pins, as a string that matches exactly
    (`sub` for `subject`), and for the box's config `sha`, the full
    commit id, which the handler compares with the bundle's.
- An `nbf`, if you add one, is checked with the same leeway. Anything
  else is carried, not checked. `deploy-token` adds a random `jti` and
  the `--subject` and `--claims` you give it.

A token minted this way is accepted exactly as `deploy-token`'s is, and
refused for the same reasons, which the box's journal names.
