# After the first deploy

[Your first deploy](first-deploy.md) leaves a working box whose config
is in git. This page has what comes next, running it: changing the
config, secrets, the way back from an upgrade, day to day, and what to
do at 3am. [Hardening the box](hardening.md) — SSH by key only, the
SSH port shut to everyone but you, and, if you want them, an
administrator who is not root, Debian's updates installed on their
own, and seven kernel network modules no app needs refused — has a
page of its own.

Names are the ones from the first page.

## 1. Change the config

Edit `deploy.example.com/Caddyfile` in the box's repository, commit —
signed, as every commit from that checkout is — and push to `main`.
The run prints what the box did:

```
apply: deploy.example.com/Caddyfile → https://deploy.example.com/: the box runs 4f1c2a9; sending c6d0e81 (1 commit), push 018f3a…
apply: deploy.example.com at c6d0e81, signed by alice@example.com; verified; installing
--- installed
+++ pushed
…
apply: deploy.example.com at c6d0e81: loaded; running apps are not restarted; apps example
```

The box applied it because a key it lists signed the commit and every
commit since the one it ran, the file is the one in that commit, and
motel validates it; then it swapped the file in and reloaded. A
reload that fails puts the previous file back. A push it refuses
changes nothing, and the run says why.

A reload never restarts a running app: a changed `app` block applies
at the app's next launch — its next deploy or rollback, or a relaunch
after a crash. A rollback to the running version gives it one now
([When a value reaches the app](secrets.md#when-a-value-reaches-the-app)).

<details>
<summary><b>If the app is a Deno app</b></summary>

Its permission flags live in this file, and a deploy launches with the
flags the box has. So widen them *before* deploying code that needs
them, and narrow them only after the code that needed them is gone.

</details>

A change someone else should read first goes on a branch and a pull
request, where the workflow checks it, and lands from a signer's
laptop — never with GitHub's merge button, which cannot make a commit
the box applies:

```sh
git switch main && git pull --ff-only
git merge --ff-only the-branch     # a branch of signed commits, rebased onto main
git push origin main
```

Undoing a change is `git revert` and a push. The box README has the
rest: [the rules](../examples/box/README.md#the-rules) that follow
from what the box checks, [adding a signer](../examples/box/README.md#adding-a-signer),
and [what to do when a push is refused](../examples/box/README.md#when-a-push-is-refused).

## 2. Secrets, if the app has any

Skip this if it does not; the examples do not. A secret is a commit,
like the config. Each value is one small file in the box's repository,
encrypted on your laptop with [age](https://age-encryption.org) to two
kinds of key: the box's own, which motel made when it was
installed, and the SSH key you sign commits with. GitHub and the
workflow only ever hold ciphertext, and nothing is typed onto the box.

Once, on the laptop: `brew install age` (Debian: `apt install age`).
The first secret also needs the box's key, which root prints:

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

The value goes from the prompt straight into age, so it is never
written to the laptop's disk or your shell's history. The prompt takes
the line you paste or type exactly as it is, however long, up to the
box's cap; since
typing is not shown it takes no edits, so a Backspace or an arrow key
refuses the value and you enter it again. A value you would rather not
type comes in through a pipe instead: `pbpaste | make secret
APP=example VAR=…` (on Linux, `xclip -o` or `wl-paste`). Add that
`env_file` line to the app's block (the template's Caddyfile has it
ready, commented out: uncomment it), then commit and push:

```sh
git commit -am "example: database" && git push
```

```
apply: deploy.example.com at 9be0d44, signed by alice@example.com; verified; installing
--- installed
+++ pushed
…
apply: deploy.example.com secrets: example: added DATABASE_URL
apply: deploy.example.com at 9be0d44: loaded; running apps are not restarted; apps example
apply: example reads its secrets at its next launch; a deploy of a new version launches it now
```

The box decrypts the value with its own key, only after it has checked
the commit's signature, and writes `/etc/motel/secrets/example.env`.
motel reads that file each time the app launches, and no app can
see it. A pull request shows which variables changed and never what
they are, and the workflow checks the names before anything is
merged.

The same command changes a value; once the box has a secret it needs
no `BOX_KEY`. A push that changes only secrets reloads nothing:

```
apply: deploy.example.com secrets: example: changed DATABASE_URL
apply: deploy.example.com at 3c1d0e8: secrets updated; the Caddyfile is unchanged, nothing reloaded; apps example
apply: example reads its secrets at its next launch; a deploy of a new version launches it now
```

Who can read a value: the box, and every key in
`deploy.example.com/secrets/recipients`, which `make signer` keeps in
step with the signers. Removing a person stops them reading new
values, not the ones they could already read, so change those. A Deno
app also needs `--allow-env=` to name each variable it reads.
[Secrets](secrets.md) has the rest: [the commands](secrets.md#tasks)
to see the names, read a value back, remove one, bring in an env file
made by hand, or let a new signer read them; a signing key age cannot
use, a rebuilt box, a lost laptop, who can read what, and what GitHub
sees.

## 3. Know the way back

The `.deb` you installed from is still in `/var/local/motel`, and
`apt install --allow-downgrades` of it, as root, is the way back from
an upgrade that does not suit you; download each later release into
the same directory. [Upgrading](upgrading.md) has the rest: checking a
new version accepts your config before you install it, what visitors
see during the restart, and how to pick a quiet moment for it.

## Day to day

- **Deploy** by pushing to `main`; **roll back** from Actions → deploy →
  **Run workflow**, with "Use workflow from" left on `main` and the
  version to go back to: a commit's first 12 characters, as the
  releases page lists them, and one of the five the box deployed most
  recently (a rollback counts), which is what it still holds
  ([examples/node](../examples/node/README.md#rolling-back)).
- **Change the box** — an app's flags, a second app, a new site, a new
  signer, a secret — by a signed push to the box's repository (steps 1
  and 2). The run's output is what changed.
- **Logs:** `journalctl -t motel-example` for the app's own output,
  `journalctl -u motel` for motel's, `journalctl -u
  motel-box-apply` for what the box did with a push — as root, or as
  the administrator, if there is one
  ([Hardening](hardening.md#3-an-administrator-if-you-want-one)),
  without sudo.
- **If motel crashes** — a SIGKILL, out of memory, a bug — systemd
  starts it again a second later and it picks the running apps back up.
  A plain `kill` (SIGTERM) is a clean stop, like `systemctl stop`, and
  stays stopped; so does a start it *refuses*: `journalctl -u motel
  -n 50 --no-pager` says why. Fix it, then, as root, `systemctl start
  motel` (after "start request repeated too quickly", `systemctl
  reset-failed motel` first). A stopped box takes no pushes, so a fix to the
  config is made on the box too ([At 3am](#at-3am)). A restart loads
  `/etc/motel/Caddyfile` as it stands: a pushed file that did not
  load was put back, but a hand edit that was not reloaded goes live
  then — or, if it does not load, keeps motel down.
- **A second app:** another `app` block and site in the box's
  Caddyfile, pushed; another copy of the example, with its own
  `MOTEL_URL`. The Caddyfile, and the app's secrets if it has any
  (step 2), are all that change on the box.
- **A second box:** another directory in the box's repository, named
  for that box's deploy host, and `motel init` from it once as root
  on the new box. Every push goes to every box
  ([More than one box](../examples/box/README.md#more-than-one-box)).
- **Patching the box** (`apt upgrade`, as root, or on its own:
  [Hardening](hardening.md#4-debians-updates-on-their-own-if-you-want-them),
  step 4) reaches an app only at its next launch; after patching, push
  a commit to each app or reboot
  ([Upgrading](upgrading.md#patching-the-host)).
- **Backups:** the package ships `motel-backup` and its timers;
  setting them up, checking on them and restoring are
  [Backups](backups.md), which says first
  [who runs which](backups.md#who-runs-these).
- **Hearing when something goes wrong** (experimental, not in a
  release yet): the `monitor` block, `motel monitor setup` and your
  channels are [Monitor](monitor.md).
- **Everything else:** [liveswap/README.md](../liveswap/README.md) is the
  reference for every option, status code and edge.

## At 3am

If a pushed change broke the deploy host itself, the next push cannot
reach the box. The way back is root over SSH (the provider's console
only if SSH itself is down), in one of two ways:

- Edit `/etc/motel/Caddyfile` and `systemctl reload motel`, then
  make the same fix a commit and push it. The next signed push
  overwrites a hand edit — its run warns that it did — because the
  repository is the truth and the edit is a bridge back to it.
- Or commit the fix first, and run `motel init` with that commit
  from its directory, copied over as the first page did it.

A change that loads but misbehaves is reverted like any commit: `git
revert`, push.

A secret that has leaked is changed like any other (`make secret`, a
push), and reaches the app at its next launch: roll back to the
running version, or deploy a new one ([When a value reaches the
app](secrets.md#when-a-value-reaches-the-app)). A hand edit of an
env file under `/etc/motel/secrets/` is overwritten by the next
push, as a hand edit of the Caddyfile is.
