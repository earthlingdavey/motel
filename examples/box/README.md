# A box repo for motel

The server's config, kept in git: one `Caddyfile` per box, reviewed
like code, signed by the people who may change it, and applied to the
box by this repo's workflow. The box checks every change itself before
it applies it. Keep this repo **private** and separate from your apps'
repos.

It is a separate repo because the Caddyfile is where the box's policy
lives: which repo may deploy each app, each Deno app's permission
flags, and who may change the file itself. An app repo can be
compromised through a dependency or a leaked token; it should not also
be able to grant itself `--allow-net`. So an app that needs a new
permission asks for it here, and the change is a diff someone reads
and signs.

| File | What it is |
|---|---|
| `deploy.example.com/Caddyfile` | One box's whole config; becomes `/etc/motel/Caddyfile` on the box whose deploy host names the directory |
| `deploy.example.com/MOTEL_VERSION` | Optional: the release that box runs, one line (`0.3.1`, `v0.3.1-rc1`), when it is not the one the workflow checks with ([A box on another release](#a-box-on-another-release)); never sent to the box |
| `.github/workflows/apply.yml` | `make check` on every push and pull request, with the latest stable motel (a `MOTEL_VERSION` repository variable pins another, such as a prerelease, for every box; a box's own `MOTEL_VERSION` file pins that box's); on `main`, sends the commit to each box (`bin/apply`) |
| `Makefile` | `make signer`, `make check`, `make wip`; for secrets, `make secret`, `make secrets`, `make secret-show`, `make rekey`, `make secrets-import` |
| `bin/` | What the Makefile and the workflow run |

## How a change reaches the box

A push to `main` runs the workflow. It checks every box's file (the
same check runs on pull requests), and posts each box its file from
that commit, with the commit and the
commits before it back to the one the box runs. The box applies the
file only if:

- the workflow's token says it ran for this repo's `main` (the box
  block's `deploy_trust`);
- a key the box's own Caddyfile lists signed that commit and every
  commit between it and the one the box runs (`signer` lines);
- the file is the one in that commit, for this box's host and path;
- motel validates it, and so does `motel-backup validate` on a
  box with backups installed.

Then the box swaps the file in and reloads. A reload that fails puts
the previous file back. The run prints what happened:

```
apply: deploy.example.com/Caddyfile → https://deploy.example.com/: the box runs 4f1c2a9; sending c6d0e81 (1 commit), push 018f3a…
apply: deploy.example.com at c6d0e81, signed by alice@example.com; verified; installing
--- installed
+++ pushed
…
apply: deploy.example.com at c6d0e81: loaded; running apps are not restarted; apps example
```

No SSH key and no secret lives in CI: the workflow's OIDC token says
which repo and branch, and the commit's signature says which person.

## Set it up

These steps start from a box with motel installed and running, and
root still able to log in: step 2 of
[Your first deploy](https://github.com/smallhoursorg/motel/blob/main/docs/first-deploy.md),
whose step 3 walks a fresh box through them. A box that already serves
a Caddyfile written by hand takes them too; step 3 says how to keep
what it runs.

1. Make a private repo from this template ("Use this template" on
   [smallhoursorg/motel-example-box](https://github.com/smallhoursorg/motel-example-box)),
   and clone it. Or copy its files into a new private repo anywhere —
   another account, or a host that is not GitHub (see
   [Outside GitHub](#outside-github)) — with `git init -b main` and the
   new repo as `origin`. Copy the hidden `.github/` directory too: it
   holds the workflow, and a repo without it starts no run when pushed
   and reports no error. Download ZIP and `git archive` keep it;
   Finder hides it, `cp dir/*` skips it, and GitHub's upload page
   leaves it out.
2. Add your SSH key as the first signer:

   ```sh
   make signer        # ~/.ssh/id_ed25519.pub; or KEY=path/to/key.pub
   ```

   It adds a `signer` line under `# signers` in every box's Caddyfile,
   and makes this checkout sign every commit with that key (its own git
   config: `gpg.format ssh`, `user.signingkey`, `commit.gpgsign`). No
   key yet: `ssh-keygen -t ed25519`, or `-t ed25519-sk` for one held by
   a hardware key, which signs only with a touch. Better still, a key
   that only signs: if the key you push to GitHub with also signs, the
   two checks a box makes (the push came from `main`, a signer made the
   commit) fall to one stolen key.

   Then add the same public key to your GitHub account as a **Signing
   Key** (Settings → SSH and GPG keys → New SSH key, Key type: Signing
   Key), so GitHub shows your commits as Verified. Use the web page:
   `gh ssh-key add --type signing` needs a permission `gh` is not given
   by default. That badge is GitHub's check, and a box does not read
   it: a box checks each commit against the `signer` lines in the
   Caddyfile it runs. So a commit GitHub calls Unverified still applies
   when a listed key signed it — the key is not on GitHub, or the
   commit's email is not one GitHub has verified for the account — and
   one GitHub calls Verified is still refused unless a listed key
   signed it: a squash from the merge button, which GitHub signs with
   its own key.
3. Make the Caddyfile your box's. On a box that already serves its
   own Caddyfile, start from that file whole — its global options,
   sites and apps — so nothing edited there is lost, and keep the
   template's beside it:

   ```sh
   mv deploy.example.com/Caddyfile template.caddy
   scp root@box.example.com:/etc/motel/Caddyfile deploy.example.com/Caddyfile
   ```

   Then copy the template's two box pieces into it: the `box { … }`
   block, with the signer line `make signer` added, into the global
   options, and `box_webhook` into the deploy host's site. Inline
   anything it `import`s, since a box refuses a file that imports, and
   delete `template.caddy`. On a fresh box there is nothing to bring
   over. Either way, replace `your-org/boxes`, in the `box`
   block's `claim repository`, with this repo's `owner/name`: the box
   takes pushes from that repository's workflow alone, so a placeholder
   left there refuses the first one. On a fresh box, also replace
   `example.com`, `deploy.example.com` and `your-org/example`
   with yours — `your-org` (your organization, or your username if the
   repos are in your own account) twice more: the `artifact_allowlist`
   entry pins the organization or account alone, and the app's
   `deploy_trust` its repo. This lists the lines left to change,
   comments aside, and prints nothing once all are changed:

   ```sh
   grep -En '^([^#]*[^#A-Za-z0-9-])?(your-org|example\.com)([^A-Za-z0-9.-]|$)' */Caddyfile
   ```

   The `app example` block is motel's examples/node; a Deno app's
   lines are in its examples/deno.
4. Name the directory for the box's deploy host, the site that carries
   `box_webhook`:

   ```sh
   git mv deploy.example.com deploy.yourdomain.com
   ```

   The box records `<host>/Caddyfile` as its file, and `make check`
   refuses any other layout.
5. Commit. It is signed, and it is the commit the box starts from:

   ```sh
   git add -A && git commit -m "deploy.yourdomain.com: the box's config"
   ```

6. Tell the box which repo it belongs to. Copy the box's directory to
   it, as the commit has it rather than as your checkout does, and run
   `motel init` there, as root, with this commit. The first two
   lines check the commit: that it has the directory's `Caddyfile`, and
   then step 3's check. While a placeholder is left it prints its line
   and sends nothing, so change it, commit, and run the four lines
   again.

   ```sh
   git cat-file -e HEAD:deploy.yourdomain.com/Caddyfile &&
     ! git show HEAD:deploy.yourdomain.com/Caddyfile | grep -En '^([^#]*[^#A-Za-z0-9-])?(your-org|example\.com)([^A-Za-z0-9.-]|$)' &&
     git archive HEAD deploy.yourdomain.com | ssh root@box.example.com tar -xf - &&
     ssh root@box.example.com "cd deploy.yourdomain.com && motel init $(git rev-parse HEAD)"
   ```

   It installs the file, reloads motel, and records the commit; the
   box then applies only commits that descend from it. It refuses when
   motel is not running, and says how to start it.
7. Push:

   ```sh
   git push origin main
   ```

   The workflow's first run answers that the box already runs this
   Caddyfile. From here on, config changes are pushes to `main`. With
   nothing deployed yet, a correct box looks like this:

   - `https://example.com` answers 503 with `example is not running:
     nothing has been deployed yet, …`, in a browser or with
     `curl -i`, once the certificate is issued, which takes a few
     seconds after the reload with 80 and 443 open.
   - `https://deploy.example.com/example` answers 401 with
     `{"error":"invalid or missing deploy token …"}`, in a browser or
     with `curl -i`. That is the webhook working: a deploy carries a
     token, and nothing without one gets further than this.

An app's secrets are commits too, encrypted so that only the box
and its readers can read them: see [Secrets](#secrets). Hardening the box — SSH by key only, the
SSH port shut, and, optionally, an administrator who is not root and
Debian's updates installed on their own — is on
[Hardening the box](https://github.com/smallhoursorg/motel/blob/main/docs/hardening.md).

## Change the config

Edit the Caddyfile, commit (signed), and push to `main`. A pull
request gets `make check` too. Land it on a signer's laptop, never with
GitHub's merge button (see [The rules](#the-rules)):

```sh
git switch main && git pull --ff-only
git merge --ff-only the-branch     # a branch of signed commits, rebased onto main
git push origin main
```

A reload never restarts a running app. A changed `app` block applies
at the app's next launch: its next deploy, or a relaunch after a crash
or a reboot.

**Widening an app's permissions:** push the new flag *before* the app
deploys code that needs it; the running version does not mind an extra
permission, and the new one will not start without it. Narrow after
the code that needed it is gone. A rollback runs the older code with
the flags pushed today.

**Undoing a change:** `git revert` it and push. The box applies the
revert like any commit.

## The rules

Each follows from what the box checks: every commit between the one it
runs and the one it is sent must be signed by a key it lists, and must
sit on `main`'s first-parent line.

| Rule | Because |
|---|---|
| Every commit on `main` is signed by a listed key. A branch from someone who is not a signer is landed by a signer: `git merge --squash` and one signed commit. | The box checks every commit it has not run. |
| Never press GitHub's merge button. | A squash is signed by GitHub's key, a rebase lands unsigned, a merge commit is GitHub's: the box refuses the push, and every push after it, until the commit is rebased out of `main` and force-pushed. |
| Rebase a branch onto `main`; never merge `main` into a branch. | A merge puts the commit the box runs off the first-parent line, and the box refuses the fast-forward, naming the case. |
| Config not yet meant to run is committed unsigned (`make wip`) or kept in a fork. | A push credential cannot sign, but it can fast-forward a branch of signed commits onto `main`, and the box applies what `main` says. A branch with an unsigned commit in it is refused whoever lands it. |
| Every box in this repo lists the same signers (`make signer` adds to all; `make check` holds it). | Every commit is on every box's history, so a key one box lacks stops that box's pushes. |
| Change a signer in two pushes: add the new key, let it apply, then remove the old one. | The box refuses a file that drops the key that signed it. |
| Recommended, in the repo's rulesets: require signed commits, and block force pushes to `main`. | The first stops the rebase button and unsigned pushes (not the squash button: GitHub's signature satisfies GitHub, not the box); it needs each signer's key added to their GitHub account as a *signing* key, or GitHub refuses their pushes. The second keeps a stale signed commit from being put back under a `main` that has moved — and so blocks the force-push that undoes a bad commit (below): lift it for that push. The box depends on neither; they shorten red runs. |
| After a credential leak: rotate it, then one applied push to `main`. | A signed commit someone fetched lands only while the box still runs the commit it forked from; one applied push puts it below. |

## Adding a signer

A box checks a commit against the signers it *runs*, so the commit that
adds Bob is signed by someone already listed:

1. Bob, in his clone, on a branch: `make signer` (it stages the line
   it adds), then `make wip` — that commit, unsigned — and pushes the
   branch.
2. Alice, a signer: `git merge --squash bob-signer && git commit` —
   one commit, signed by her — and pushes `main`.
3. Once it applies, Bob's own signed commits go through.
4. If a box has secrets, Bob's `make signer` also made him a reader
   ([Secrets](#secrets)): once a reader runs `make rekey` and pushes,
   he can read what was there before.

Removing someone is a commit that deletes their line, signed by
someone else, since a box refuses a file that drops the key that
signed it.

## When a push is refused

The run prints the box's reason; it changed nothing. The common ones:

- **A commit is not signed**, or is signed by GitHub (the merge
  button): rebase it out of `main` — re-sign it, or squash it into a
  signed one — and force-push (lifting the ruleset that blocks force
  pushes, if you set one). Nothing on the box.
- **Signed by a key the box did not list:** the commit that adds the
  key has to apply first. Put `main` back to that commit, let it apply,
  then push the rest.
- **Does not descend from the commit the box runs:** a branch that
  merged `main` into itself — rebase it onto `main` and force-push. If
  `main` itself was rewritten below that commit, `motel box baseline
  <sha>`, as root on the box, resets trust to `<sha>` (the box will
  never examine its ancestors).
- **Longer than 500 commits** since the one the box runs: `motel box
  baseline <sha>` as root on the box.
- **A run says "superseded"**: a later run already brought the box
  past this commit. Nothing is wrong.
- **`motel-backup validate:`**, on a box with backups installed: the
  file declares something a backup run could not plan from, such as a
  `root` or a backup path written with a `{$NAME}`, and the reason
  names it. Change the file and push again.

## At 3am

If a change broke the deploy host itself, the next push cannot reach
the box. The way back is root on the box, over SSH (the provider's
console only if SSH itself is down): edit `/etc/motel/Caddyfile`
and `systemctl reload motel`, or copy the box's directory over as
step 6 did and `motel init` again from it. The next
signed push overwrites a hand edit — the run warns that it did — which
is the point: the repo is the truth and the edit is a bridge back to
it, so a fix that must outlive the night is a commit.

The same path moves where the deploy host's server listens
(`https_port`, a `bind` on its site): a push cannot move the address
it arrives on, so the run that would is refused by name and the box
keeps serving. Commit the change, make it on the box as above (and
move whatever forwards `https://<host>/` there), then let the next run
push the commit, which now matches.

## Outside GitHub

Only the workflow file is GitHub's. `bin/apply` is what talks to the
boxes, and any CI that gives it what the Actions job does can run it:

- a checkout of the commit being pushed, with its full history;
- a `motel` on `PATH`, and a `motel-<version>` for each box whose
  `MOTEL_VERSION` file names one (`bin/install-motel` installs
  them, with `sudo`; `make check` needs them too);
- `MOTEL_TOKEN`: a token that the `box` block's `deploy_trust`
  accepts and whose `sha` claim names that commit. `bin/apply` sends
  that one token with every request, polls included, so it must
  outlive the run.
  - From a CI's OIDC: on GitLab, an `id_tokens:` entry with
    `aud: motel`, and a box block with a `deploy_trust gitlab` holding
    `audience motel`, `claim project_path your-org/boxes` and
    `claim ref_path refs/heads/main`, a line each.
    Not tested.
  - From a runner without OIDC: a key the box block trusts with a
    `deploy_trust local` holding `public_key …` and `audience motel`,
    a line each
    (`motel deploy-keygen` makes the pair), and a token minted each
    run with `motel deploy-token --key … --audience motel --ttl 1h --claims sha=<commit>`.
    motel's own end-to-end tests run `bin/apply` this way. The
    private key is then a stored secret, which OIDC avoids; the
    signatures still bound what it can push. `motel` is one name
    the boxes' blocks share, not a box's own: the one token goes to
    every box in the repository.

The rules above hold on any host: a merge button that squashes,
rebases or writes its own merge commit lands a commit no listed key
signed. motel's
[Deploying from outside GitHub](https://github.com/smallhoursorg/motel/blob/main/docs/outside-github.md)
walks through both ways, and an app's deploy from the same runner.

## More than one box

One directory per box, each named for its deploy host. Every push goes
to every box: each one's history must keep up, even when its own file
did not change, and then the box answers that it already runs that
Caddyfile. Each box is set up with `motel init` from its own
directory.

### A box on another release

The workflow checks every box's files with one motel: the latest
stable release, or the one the `MOTEL_VERSION` repository variable
names. Each box validates what it is sent with the release it runs, so
while one box tries a release candidate and the rest stay on the stable
release, that one check tells the wrong story: a pull request passes
with config the stable boxes refuse, and the push to `main` goes red on
them, named, with nothing changed. A file beside the box's Caddyfile
pins that box to its own release:

```
deploy.example.com/MOTEL_VERSION     one line: 0.3.1-rc1, or v0.3.1-rc1
```

`bin/install-motel` then installs that release too, as
`motel-0.3.1-rc1`, verified against its `checksums.txt` like the
rest, and `make check` checks that box's files with it, saying so in
its line:

```
deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer), checked with motel-0.3.1-rc1
```

For each box, in this order: its file; else the `MOTEL_VERSION`
repository variable; else the latest stable release. An empty file, one
that does not name a release, or one that names a release with nothing
to download, never means "the latest": `bin/install-motel` says so,
naming the file, and skips it, so the other boxes are still checked;
`make check` then refuses that box, naming the file again. The file is
this repo's alone: `bin/apply` never sends it, and the box has no use
for it. It tells the early check which release the box runs; it does
not install one, and delete it when the box is back on the release the
others run.

## Secrets

An app's secrets are commits too. Each value is a file of its own in
this repo, encrypted on your laptop with [age](https://age-encryption.org)
to the box's own key and to every reader's SSH key. The box decrypts
them when it applies the push, and writes the app's env file. GitHub
and the workflow only ever hold ciphertext, and nothing is typed onto
the box.

```
deploy.example.com/secrets/recipients                who can read them: the box's key first, then each reader's
deploy.example.com/secrets/example/DATABASE_URL.age  one value, encrypted to all of them
```

Once, on the laptop: `brew install age` (or `apt install age`). The
box's first secret also needs the box's own key, which root prints on
the box:

```sh
make secret APP=example VAR=DATABASE_URL BOX_KEY=$(ssh root@box.example.com motel box key)
```

```
value for example DATABASE_URL (typing is not shown):
secret: made deploy.example.com/secrets/recipients: this box (age1qpzr…7k2d), alice@example.com
secret: deploy.example.com/secrets/example/DATABASE_URL.age, encrypted to 2 keys, added to the commit
secret: example's block does not read its secrets yet; add this line to it:
secret:     env_file /etc/motel/secrets/example.env
```

The value goes from the prompt, or a pipe (`pbpaste | make secret …`),
straight into age, never onto the laptop's disk. The prompt takes what
you paste or type exactly as it is: typing is not shown, so a
Backspace or an arrow key refuses the value, and you enter it again.
Add the `env_file` line to the app's block, commit (signed) and push.
The run names what the box did, never a value:

```
apply: deploy.example.com secrets: example: added DATABASE_URL
apply: deploy.example.com at 9be0d44: loaded; running apps are not restarted; apps example
apply: example reads its secrets at its next launch; a deploy of a new version launches it now
```

The box writes `/etc/motel/secrets/example.env`, which only root
and motel can read and no app can see. motel reads it each time
the app launches, so a value reaches the app at its next launch: its
next deploy of a new version, or a relaunch after a crash or a reboot.

| To | Run |
|---|---|
| Set a value, or change one | `make secret APP=example VAR=DATABASE_URL` |
| See the names, and who can read them | `make secrets` |
| Read a value back | `make secret-show APP=example VAR=DATABASE_URL` |
| Remove one | `git rm deploy.example.com/secrets/example/DATABASE_URL.age` |
| Bring in an env file made on the box | `ssh root@box.example.com cat /etc/motel/example.env \| make secrets-import APP=example` |
| Let a new reader read what is there, or move it all to a rebuilt box's key | `make rekey`, or `make rekey BOX_KEY=…` |
| Set a notify channel's URL, the box's own (its `notify` block reads it as `{secret.NOTIFY_OPS_URL}`) | `make secret APP=_box VAR=NOTIFY_OPS_URL` |

A box's first secret, by `make secret` or `make secrets-import`, also
takes `BOX_KEY=$(ssh root@box.example.com motel box key)`: it makes
the recipients file. With more than one box, name the one you mean:
`BOX=deploy.example.com`.
`make check`, on every push and pull request, holds the secrets to
what the box takes, and warns as one nears a cap. A Deno app also needs
`--allow-env=` to name each variable it reads.

The box's own secrets, `APP=_box`, are notify's channel URLs: the box
writes them to `/etc/motel/secrets/_box.env`, which no app is given,
and reloads motel when they change, so a new URL is in use once the
push is applied (motel's [Secrets](https://github.com/smallhoursorg/motel/blob/main/docs/secrets.md#a-notify-channels-url)
page).

Whoever can get a signed commit onto `main` can read these files
anyway: one site in the Caddyfile serves any file the `motel` user
can read, env files included. A signer's key is the Caddyfile, and the
Caddyfile is every app's secrets.

The backup repository's credential is not one of these and never rides
this channel: `motel-backup setup` keeps it at
`/etc/motel-backup/repository.env`, root's alone, which the
`motel` user cannot read.

The rest is on motel's [Secrets](https://github.com/smallhoursorg/motel/blob/main/docs/secrets.md)
page: who can read a value, adding or removing a person (removing one
does not take back what they could read: change those values), a
rebuilt box, a signing key age cannot use, rotating a value, and what
GitHub and CI see.

### An env file on the box, by hand

An app's block can still name a file made on the box, outside this
repo, as root, and it keeps working as it always has:

```sh
install -m 0640 -o root -g motel -T /dev/null /etc/motel/example.env   # once: empty, root:motel
editor /etc/motel/example.env      # DATABASE_URL=postgres://…
```

motel reads the file each time the app launches, so a missing or
unreadable one fails that launch, and a changed one applies at the
next.
