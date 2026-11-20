# Your first deploy

From a fresh server to an app you deploy by pushing to `main`, in four
steps. At the end you have motel serving one app over HTTPS, the
box's config in a repository of its own that the box applies when you
push a signed commit, a GitHub Actions workflow that deploys every
push of the app with no stored secret, and the shape you copy for
every app after it.

Two repositories feed the box, and the box trusts them differently:

| Repository | What a push there changes | The box applies it when |
|---|---|---|
| The box's | The config: which apps run, who may deploy each, their permission flags and secrets | a key on the box's own list signed the commit; those keys stay on the signers' laptops |
| Each app's | That app's code | the app's workflow proves, with a token GitHub mints, that the push came from that repository's `main` |

So a stolen GitHub login or a compromised workflow can ship a new
version of an app, but cannot change the config with anything a signer
did not sign. GitHub's merge button
cannot land a change to the config either: a reviewed pull request is
landed from a signer's laptop ([the rules](../examples/box/README.md#the-rules)).

You work as root on the box only to install software and, once, to
tell the box which repository its config comes from (step 3);
everything else happens on your laptop and on GitHub.
[After the first deploy](after-first-deploy.md) covers what comes
next — changing the config, secrets — and
[Hardening the box](hardening.md) is recommended, none of it needed to
get here. Deploying from a CI that is not GitHub Actions, or from a
laptop with a key, is [Deploying from outside GitHub](outside-github.md),
once this page's box exists.

What the laptop needs, on this page and the ones after it:

- git 2.34 or later (it signs commits with an SSH key), `make`, `ssh`
  and `curl`, and the SSH key the box is created with.
- The [GitHub CLI](https://cli.github.com/) (`gh`), if you want it:
  where a step shows a `gh` command, GitHub's web pages do the same,
  and the step says where.
- `age`, once the box has secrets ([Secrets](secrets.md)).
- To run the box repository's `make check` yourself, Linux, with the
  release's `moteld` on `PATH`, from the tarball on its releases page:
  there is no macOS build. From a Mac, the repository's
  workflow runs it on every push and pull request.
- The laptop client, `motel`, only to read the box over HTTPS
  ([Giving a person access](people.md)). No release up to v0.8.0
  carries it; [client/README.md](../client/README.md#getting-it) says
  how to get it.

Names used throughout — replace them with yours as you go:

| Name | What it is |
|---|---|
| `example.com` | The app's address |
| `deploy.example.com` | The deploy webhook's address, on the same box: the app's deploys and the box's config both arrive there |
| `box.example.com` | The box's own name, for SSH: any name that resolves to it, `example.com` included, or its address |
| `your-org` | Your GitHub organization, or your username if the repositories are in your own account |
| `your-org/example` | The GitHub repository the app deploys from |
| `your-org/boxes` | The private GitHub repository that holds the box's config |
| `example` | The app's name on the box |

## 1. A box, two names, open ports

You need a real virtual machine, not a container. Providers call the
kind you want **KVM**; at Hetzner, say, the cheapest KVM tier is enough
for a few small apps. Offers described as LXC or "container VPS" will
not work: each app runs in its own sandbox, which those hosts cannot
provide, and motel refuses to run your apps without it. If you are
unsure what you have, step 2 checks for the common cases within a
minute. At the provider:

1. Create a server with **Debian 13** as the image and your SSH public
   key. Architecture is your choice; note it, since the release `.deb`
   and the Node example's executable are per-architecture.
2. **TCP 80 and 443** must be reachable from the internet, and
   **UDP 443** too for HTTP/3. If the provider has a firewall, allow
   all three from anywhere; on one where each rule names a protocol,
   such as Hetzner Cloud's, that is a rule for each. Allow 22 too
   ([Hardening the box](hardening.md) narrows it to your own address),
   and deny the rest: motel listens on no other port, but an app may
   open one of its own, and the provider's firewall is what keeps it
   shut. On a provider with none, only a firewall on the box itself
   would, which these pages do not set up.
   Leaving UDP 443 closed is a fine choice: everything works, and
   browsers fall back to HTTP/2 over TCP without saying so. The end of
   step 3 checks it.
3. Point two DNS names at the server's address: `example.com` and
   `deploy.example.com`. motel gets certificates for both itself,
   once the names resolve and the ports are open.

## 2. Install motel

Logged in as root, fetch the `.deb` for the box's architecture from
the latest of the [releases](https://github.com/smallhoursorg/motel/releases),
check it, and install it:

```sh
v=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/smallhoursorg/motel/releases/latest)
v=${v##*/v}                         # the latest release's tag, without the v
arch=$(dpkg --print-architecture)   # amd64 or arm64
base=https://github.com/smallhoursorg/motel/releases/download/v$v
mkdir -p /var/local/motel && cd /var/local/motel
curl -fsSLO "$base/moteld_${v}_${arch}.deb" && curl -fsSLO "$base/checksums.txt" &&
  sha256sum -c --ignore-missing checksums.txt &&
  apt install ./moteld_${v}_${arch}.deb
systemctl enable --now moteld
```

<details>
<summary><b>To install a release candidate</b></summary>

A release candidate's `.deb` has a dot where its tag has a hyphen —
`moteld_X.Y.Z.rcN_arm64.deb` under `vX.Y.Z-rcN` — so the block above
does not fit it as it stands. This one does, with `<tag>` replaced by
the tag the maintainer announced, without its `v`:

```sh
v='<tag>'                           # the announced tag, without the v
arch=$(dpkg --print-architecture)   # amd64 or arm64
deb=moteld_$(echo "$v" | tr - .)_${arch}.deb
base=https://github.com/smallhoursorg/motel/releases/download/v$v
mkdir -p /var/local/motel && cd /var/local/motel
curl -fsSLO "$base/$deb" && curl -fsSLO "$base/checksums.txt" &&
  sha256sum -c --ignore-missing checksums.txt &&
  apt install "./$deb"
systemctl enable --now moteld
```

The box's repository (step 3) then needs its Actions variable
`MOTEL_VERSION` set to the same `<tag>`: its workflow
checks with the latest stable release unless told otherwise, and a
release candidate is never that. (With more than one box, a
`MOTEL_VERSION` file beside this box's Caddyfile pins this box
alone: the template's README, "A box on another release".)

</details>

(The directory is where the `.deb` stays, so the way back from a later
upgrade is on the box already; `/tmp` would not survive a reboot.
`enable --now` prints only `Created symlink …`; that is success.)

On a release older than Debian 13, `apt install` stops with
`moteld : Depends: systemd (>= 257)`, and the `systemctl` line after
it finds no moteld unit. Each app's sandbox is built on systemd 257,
so the fix is a Debian 13 image, not a setting. That is the first half
of the step 1 check.

Check it:

```sh
systemctl status moteld          # active (running); press q to exit
curl -s localhost                  # "moteld is running. To serve your apps: moteld init …"
systemd-detect-virt --container    # none
```

If it is not running, `journalctl -u moteld -n 50 --no-pager` says
why.

The block's last line, `systemd-detect-virt`, is the other half of the
step 1 check, the part apt cannot see. `none` means no container. A
name such as `lxc` or `openvz` means a container, the kind of host
step 1 warns about: most cannot give the sandbox, and step 3 is where
motel finds out for sure.

(The deploy side, liveswap, is not running yet: the starter config
leaves it commented out. It starts, and says so in the journal, in
step 3, when the box's own config replaces the starter. That start is
where the sandbox is first tried for real.)

## 3. The box's config, in git

The box's config is one file, `/etc/motel/Caddyfile`, and it is
where the box's policy lives: which repository may deploy each app,
each Deno app's permission flags, and who may change the file itself.
You write it in a private repository of its own, separate from the
app's, and it reaches the box one way only: you push a signed commit
to that repository's `main`, its workflow sends the commit to the box,
and the box applies the file only if a key on its own list signed the
commit. Nothing on the box is edited by hand.

**Make the repository** from the box template,
[motel-example-box](https://github.com/smallhoursorg/motel-example-box):
press **Use this template** → **Create a new repository**, make it
private, and clone it. With the GitHub CLI that is one line:

```sh
gh repo create your-org/boxes --template smallhoursorg/motel-example-box --private --clone
cd boxes
```

A repository made from the template runs its workflow as soon as it
exists, and that run is red: the template lists no signer, and a file
without one is refused. Nothing reached any box.

Or copy the template's files yourself, into a new private repository
anywhere — another GitHub account or organisation, or a host that is
not GitHub: the template's **Code** → **Download ZIP**, unpacked into
an empty directory, then `git init -b main` and `git remote add origin
<the new repository's address>` there. Keep the hidden `.github/`
directory: it holds the workflow that sends the config to the box, and
a repository without it starts no run when pushed and reports no
error. The ZIP and `git archive` keep it; Finder hides it, `cp dir/*`
skips it, and GitHub's upload page leaves it out. The workflow that
sends the config to the box is GitHub Actions; on another host,
[Deploying from outside GitHub](outside-github.md#4-the-boxs-config-from-another-runner)
says what a CI job of your own needs, and the rest of this page reads
as if the repository is on GitHub.

**Add your key as the first signer:**

```sh
make signer        # ~/.ssh/id_ed25519.pub; or KEY=path/to/key.pub
```

```
signer: added alice@example.com (ssh-ed25519) to deploy.example.com/Caddyfile
signer: commits from this checkout are signed with /home/alice/.ssh/id_ed25519
```

It adds a `signer` line to the `box` block, named for your git email,
and makes this checkout — this one only — sign every commit with that
key. If it also says the file is not staged, ignore that today, since
the commit below takes the file as it stands. The box README's
[Set it up](../examples/box/README.md#set-it-up) says why a key that
only signs is better than the one you push to GitHub with, and how to
use a hardware key.

**Give GitHub the same key**, so it shows your commits as Verified
rather than Unverified: on GitHub, Settings → SSH and GPG keys →
**New SSH key**, Key type **Signing Key**, and paste the public key
`make signer` used. Use the web page: the GitHub CLI's `gh ssh-key add
--type signing` needs a permission `gh` is not given by default.
GitHub shows Verified only when the commit's email, your git email, is
one it has verified for your account.

**Make the file yours.** `deploy.example.com/Caddyfile` is the box's
whole config: who may change it, the app it will run and who may
deploy that app, the app's site, and the deploy host. The app is
motel's example Node app, named `example`. A box runs only the apps
its config names, so the app is named here, before anything deploys
it; step 4 makes its repository and deploys it. The file's comments
say what each line does. Replace `example.com`, `deploy.example.com`
and `your-org` (your organization, or your username) — the last one
three times: the `box` block's `claim repository your-org/boxes` names
this repository (the box takes config from its workflow alone, so a
placeholder left there refuses the first push), `artifact_allowlist`
pins the organization or account, and the app's `deploy_trust` its
repository. This lists the five lines, comments aside, and prints
nothing once all five are changed:

```sh
grep -En '^([^#]*[^#A-Za-z0-9-])?(your-org|example\.com)([^A-Za-z0-9.-]|$)' */Caddyfile
```

What is new beside the app is at the top and the bottom:

```caddyfile
{
	admin unix//run/motel/admin.sock

	box {
		deploy_trust github {
			audience motel
			claim repository your-org/boxes
			claim ref refs/heads/main
		}
		# signers
		signer alice@example.com ssh-ed25519 AAAAC3Nz…
	}

	liveswap {
		# … the app, from examples/node/motel.caddy, and who may deploy it
	}
}

# … example.com's site

deploy.example.com {
	liveswap_webhook
	box_webhook
}
```

`box_webhook` is where the workflow sends the box its config. The
workflow asks motel which site carries it, so there is no address
to copy anywhere.

<details>
<summary><b>If you would rather deploy the Deno example</b></summary>

The Deno example runs `deno run` with a Deno installed on the box, and
the runtime's permission flags live in this Caddyfile — where a
compromised build cannot widen them. That is what it buys over the
single executable; the cost is one runtime to install. Two changes to
the above:

1. Install Deno under `/usr/local`, as root on the box. The app's
   sandbox contains `/usr` and nothing else of the host's software, so
   a Deno anywhere else would not exist for it. Use the version in the
   example's `.deno-version`, which is what its workflow builds with:

   ```sh
   v=v$(cat .deno-version)               # from a checkout; on the box, type what the file says
   f=deno-$(uname -m)-unknown-linux-gnu.zip
   curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f"
   curl -fsSLO "https://github.com/denoland/deno/releases/download/$v/$f.sha256sum"
   sha256sum -c "$f.sha256sum" &&
     apt install -y unzip && unzip -o "$f" deno -d /usr/local/bin
   ```

2. In the `app example` block, replace the four lines from the Node
   example with the ones in
   [examples/deno/motel.caddy](../examples/deno/motel.caddy):

   ```caddyfile
   command deno run --cached-only \
   	--allow-net=unix:{socket} \
   	--allow-read={shared_dir},{socket} \
   	--allow-write={socket} \
   	--allow-env=SOCKET,APP_VERSION,DATA_DIR \
   	main.ts
   pre_start deno run --cached-only \
   	--allow-read={shared_dir} \
   	--allow-write={shared_dir} \
   	--allow-env=DATA_DIR \
   	migrate.ts
   env DENO_DIR {release_dir}/.deno
   env DENO_NO_UPDATE_CHECK 1
   env APP_VERSION {version}
   env DATA_DIR {shared_dir}
   ```

When you reach step 4, make the app's repository from
[motel-example-deno](https://github.com/smallhoursorg/motel-example-deno) instead — the same **Use this
template** — and leave out the `MOTEL_RUNS_ON` variable that step
asks an amd64 box for: the tarball has no native code, so any runner
builds it.

</details>

Name the directory for the deploy host — the box records
`<host>/Caddyfile` as its file — and keep the name in `host` for the
commands below:

```sh
host=deploy.yourdomain.com           # your deploy host: the site that carries box_webhook
git mv deploy.example.com $host
```

Then commit. The commit is signed, and it is the one the box starts
from:

```sh
git add -A && git commit -m "$host: the box's config"
```

**Tell the box which repository it belongs to.** Copy the box's
directory to it, as the commit has it, and run `moteld init` there
as root with that commit. The first two lines check the commit: that
it has `$host/Caddyfile`, and then the placeholder check above. While
one is left it prints its line and sends nothing, so change it,
commit, and run the four lines again.

```sh
git cat-file -e HEAD:${host%/}/Caddyfile &&
  ! git show HEAD:${host%/}/Caddyfile | grep -En '^([^#]*[^#A-Za-z0-9-])?(your-org|example\.com)([^A-Za-z0-9.-]|$)' &&
  git archive HEAD $host | ssh root@box.example.com tar -xf - &&
  ssh root@box.example.com "cd $host && moteld init $(git rev-parse HEAD)"
```

```
init: box_webhook on deploy.example.com; 1 app (example); 1 signer (alice@example.com)
init: this box's file is deploy.example.com/Caddyfile in the repository
init: installed /etc/motel/Caddyfile
init: this box applies commits descending from 4f1c2a9
init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow.
init: secrets for this box encrypt to age1…; moteld box key prints it again
```

It refuses, writing nothing, a file with no `box` block, no signer or
no site carrying `box_webhook` — any of them would leave the box
unreachable by its own channel — and a directory not named for that
site. Otherwise it installs the file over the starter config and
reloads moteld. A file that does not load is put back, the starter
keeps serving, and `init` says so; `journalctl -u moteld -n 50
--no-pager` on the box says why. Fix it, commit, and run the four
lines again. The reload is also liveswap's first start, which tries the
sandbox: on a host that cannot give one, the journal names what is
missing, and the fix is the host's, not the file's
([liveswap/README.md](../liveswap/README.md#sandbox) has what a host
has to provide).

**Push:**

```sh
git push origin main
```

The workflow checks the file with the latest motel release, and
sends the commit to the box, which checks the run's token and that the file is
the one in that commit, and answers that it already runs it:

```
apply: deploy.example.com/Caddyfile → https://deploy.example.com/: the box runs 4f1c2a9; sending 4f1c2a9 (1 commit), push 7d3e…
apply: deploy.example.com at 4f1c2a9: the box already runs this Caddyfile
```

No run for this push on the repository's **Actions** tab? First check
that the repository has `.github/workflows/apply.yml`: a copy that
missed the hidden directory has none, so copy it in from the template,
commit, and push. If the file is there, Actions is turned off: the
repository's Settings → Actions → General, or its organization's,
turns it on.

From here on, a change to the box's config is a signed commit pushed
to `main`: [After the first deploy](after-first-deploy.md#1-change-the-config)
has how. With nothing deployed yet, a correct box looks like this:

- `https://example.com` answers 503 with `example is not running:
  nothing has been deployed yet, …`, in a browser or with `curl -i`,
  once the certificate is issued, which takes a few seconds after
  `init`'s reload with 80 and 443 open.
- `https://deploy.example.com/example` answers 401 with
  `{"error":"invalid or missing deploy token …"}`, in a browser or with
  `curl -i`. That is the webhook working: a deploy carries a token, and
  nothing without one gets further than this.
- If UDP 443 is open (step 1) and the Caddyfile leaves HTTP/3 on (it
  is on unless a `protocols` line in the `servers` option, a `bind` or
  `default_bind` leaves out `h3`; the one in `tls` is TLS versions),
  this, run from outside the box, prints the same status as the first
  item, over HTTP/3:

  ```sh
  curl --http3-only --max-time 10 -sS -o /dev/null -w '%{http_code}\n' https://example.com/
  ```

  An error instead, such as a timeout (`curl: (28)`) or `Connection
  refused` (`curl: (7)`), means no HTTP/3 answer came back. Check the
  UDP 443 rule at the provider's firewall first; on the box,
  `ss -ulnp 'sport = :443'` shows whether moteld listens on UDP 443.
  The check needs a curl built with HTTP/3, which `curl -V` lists as
  `HTTP3` under `Features`; without one, skip it.
- `journalctl -u moteld | grep 'liveswap started'`, on the box, gains
  a line with `"apps":1,"app_names":["example"]`.
- The workflow run is green.

## 4. The app

The Node app is published as a template repository,
[motel-example-node](https://github.com/smallhoursorg/motel-example-node) — the `examples/node` directory of
this repo, as of its latest stable release. Open it, press **Use this
template** → **Create a new repository**, and name it
`your-org/example`. Private is fine: the box fetches the release asset
by its API URL with the workflow's own token, and the
`artifact_allowlist` in the box's Caddyfile admits it. With the GitHub
CLI that is:

```sh
gh repo create your-org/example --template smallhoursorg/motel-example-node --private
```

The new repository runs the deploy workflow as soon as it is created,
and that run stops red at its first step: it does not know your box
yet. Nothing was published, and telling it the box is the rest of this
step — no file is edited, ever.

In the new repository's Settings → Secrets and variables → Actions →
Variables, add:

- `MOTEL_URL`, set to `https://deploy.example.com/example`. A
  run without it stops at its first step, saying so, and publishes
  nothing. It must be `https://`: the deploy request carries the
  run's token, so the deploy step refuses any other scheme before it
  mints one.
- On an amd64 box, `MOTEL_RUNS_ON`, set to `ubuntu-24.04`. The
  workflow builds on an arm64 runner otherwise, and the executable is
  the runner's own Node binary, so this has to match the box; a
  mismatch is refused at deploy time with a message naming the right
  value.

Or from a laptop:
`gh variable set MOTEL_URL -R your-org/example --body https://deploy.example.com/example`.

**On an arm64 box, one more command as root.** The Node example's
executable is a copy of Node, and Node's arm64 build links one library
a stock Debian 13 does not ship; without it the app fails to start
with exit status 127. amd64 needs nothing, and nor does the Deno
example.

```sh
ssh root@box.example.com apt install -y libatomic1
```

Then re-run the red run — Actions → the failed run → **Re-run jobs** —
which picks up the variable you just set. (A push does the same, and
is how every later deploy starts.) The workflow builds the
executables, publishes them as a GitHub release tagged with the
commit's first 12 characters, mints an OIDC token, and asks the box to
deploy. The box fetches the release, runs the migration, starts the
new version on its own socket, health checks it for fifteen seconds,
and moves traffic over. The request returns when that is done, so the
deploy step takes about half a minute.

What a first deploy that worked looks like:

- The deploy step's output ends with the app's status JSON, holding
  `"current_version":"<the commit's first 12 characters>"`.
- `curl https://example.com/` answers `hello from <that version>
  (schema v1)`; the 503 from step 3 is gone.
- On the box, `journalctl -t motel-example` shows the app's own
  output — the migration's line, then whatever `server.js` prints.

If the deploy step is red, its output says which stage refused it and
why — a token the box would not accept, a URL outside the allowlist, a
health check that never passed. A 401 means the box's `deploy_trust`
for this app does not accept the run: most often the `claim
repository` or `claim ref` names another repository or branch, or the
audience differs: the job asks for `motel`, so a block with another
`audience` needs the same name as `MOTEL_AUDIENCE` in the job's
`env:`, beside `MOTEL_URL`, so the rollback step gets it too (a
repository variable alone does not reach the script). A `box` block's
goes in the `env:` at the top of `apply.yml`.
It also means an app the box does not know — a
typo in `MOTEL_URL` — since an unknown app answers the same 401
unless a global `deploy_trust` accepts the token. The step prints the
values the run minted its token with, so you can hold them against
the app block; the box's answer deliberately says no more, and
`journalctl -u moteld` on the box names the app that was asked for
and the check that refused it — within the box's budget for failed
authentications: ten a minute from one address and a hundred a minute
in all; past either, the answer is the same 401 and the failure is
not written to the journal until the minute passes. The same 401
also covers the box failing to consult the token's issuer (an outage
at github.com): the journal names that once a minute whatever the
budget, and the first valid token once the issuer is back is admitted
even from an address the retries throttled. For a migration that
failed or an app that exited on start, the same output carries the
app's side under
`detail`: the exit status, what the health endpoint answered, and the
last lines the app wrote (40 by default; `deploy_log_lines` in the
app block changes that, `0` keeps them on the box). The full journal
is `journalctl -t motel-example` on the box. Nothing was serving
before, so nothing changed.

A re-run of the same workflow run rebuilds the tarball, and the box
refuses it with a 422: versions are immutable on the box, that
commit's version is already there, and the rebuilt bytes are not the
ones it runs (the box relaunches a running version only for the very
same artifact). Push a new commit to deploy again, or **Run workflow**
with the running version as the rollback target to relaunch it as it
is.

## What you have

An app at `example.com` that deploys on every push to its `main`, and
a box whose config changes on every signed push to `your-org/boxes`'
`main` — with no SSH key, password or token stored anywhere in CI.
Rollback is a button:
Actions → deploy → **Run workflow**, with the version to go back to. A
version is the first 12 characters of a commit on `main`, as the
releases page lists them; the box keeps the five it deployed most
recently (a rollback counts), so those are the ones it can relaunch. Leave "Use workflow from" on `main`: the
workflow runs from there whichever version it relaunches. The
example's README says [more](../examples/node/README.md#rolling-back).
The next app is a commit to the box's repository — another `app`
block and site in its Caddyfile, signed and pushed — then step 4
again: another repository from the template, with its own
`MOTEL_URL`.

[After the first deploy](after-first-deploy.md) is next: changing the
config and adding secrets. [Hardening the box](hardening.md) is for
when you are ready to. Each takes less time than this did.
