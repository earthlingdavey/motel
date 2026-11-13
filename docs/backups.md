# Backups

The package ships `motel-backup`: an hourly backup of what each
app's Caddyfile block declares — its SQLite databases, copied safely
while the app runs, and the files under its `shared/` — into one
[restic](https://restic.net) repository off the box. restic is a backup
program Debian ships; `motel-backup` runs it. A weekly drill proves
a restore would work: it fetches every app's newest snapshot, checks it
as a restore would, and installs nothing. This page is how to set
backups up, check on them, restore, rebuild a box, keep the repository
small, and recover from an attack. How `motel-backup` works, and
everything it refuses, is [backups/README.md](../backups/README.md).

The transcripts below come from a fresh Debian 13 box with the package
installed; on your box the URL, the ids, the times and the password
differ. The release was checked end to end on Backblaze B2 through its
S3-compatible endpoint, the door the box uses, and nothing has been run
on AWS S3 or Hetzner: [the last section](#what-has-been-verified-where)
says exactly what that covers.

## Before you start: the bucket, and what it protects

A backup that an attacker can delete is no backup. The bucket below
keeps every version of every file, and neither key you make for it can
destroy one: what a key deletes or overwrites stays an old version for
180 days, and only the bucket's own rule removes it after that. So
nothing the box or your own machine holds can destroy a backup, and a
deletion or overwrite made with either key can be undone for 180 days —
**if someone [looks from off the box](#look-from-your-own-machine) at
least once every 90 days**, which leaves another 90 to recover; whoever
has the box can make its own `status` say anything. A stolen login to
the provider account can destroy everything: give it a second factor,
and never put its keys on the box.

Three things are needed before `setup`:

- **A bucket of its own**, one per box, on Backblaze B2, AWS S3 or
  another S3 provider, with versioning on and one lifecycle rule: old
  (noncurrent) versions are removed after 180 days. Never a rule that
  expires current objects by age: a pack judged old can hold the only
  copy of data every backup uses. `setup` asks for an `s3:` URL, B2's
  included; `rest:`, `gs:` and `swift:` repositories are written
  [by hand](../backups/README.md#by-hand), and restic's own `b2:`
  backend is refused: its B2 library retries a failed upload without
  end, and held a run for 27 hours with nothing said.
- **Two keys**, neither able to destroy a version: the box's, which
  `setup` asks for, and your own machine's, for
  [retention](#retention-off-the-box) and the look. Both need to
  *delete*: restic replaces its lock file every five minutes, and a
  command run with a key that may delete nothing is stopped some twenty
  minutes in. On a versioned bucket a delete only hides a file.
- **Somewhere off the box**, such as a password manager, for the
  repository URL, the box's key, and the password `setup` shows once.
  With those, `restic` reads every backup from any machine; without
  the password nothing can.

Object Lock is not part of this. A bucket's default retention locks
each file for a period counted from its upload, and restic keeps the
files of a first backup in use for years: the data every backup shares
would be the first to fall out of the lock.

### On Backblaze B2

The easiest: every bucket keeps versions.

1. **The repository URL** is the bucket's S3 endpoint, the one the
   bucket's page shows, with the bucket after it:
   `s3:https://s3.<region>.backblazeb2.com/my-backups` (HTTPS and
   Signature v4 only, Backblaze's
   [S3-compatible API](https://www.backblaze.com/apidocs/introduction-to-the-s3-compatible-api)).
   `setup` reads the region from the endpoint's name, its second part
   (`eu-central-003` in `s3.eu-central-003.backblazeb2.com`), and
   writes it beside the key, so that restic never asks B2 where the
   bucket is, a question the keys below may not ask. A `b2:` URL is
   refused before anything is asked, naming the S3 form to use instead.
2. **Two application keys**, each for the bucket alone, with the
   capabilities `listBuckets`, `listFiles`, `readFiles` and
   `writeFiles` — **not `deleteFiles`**, the capability that deletes a
   version. restic only ever deletes by name, which on B2 leaves a hide
   marker over the old version, so `deleteFiles` would add nothing but
   a stolen key's power to destroy history. The web console offers
   three presets and no single capabilities — Read and Write, which may
   delete, Read Only and Write Only — so make the two keys once from
   the command line, with Debian 13's `backblaze-b2` (a `debian:13`
   container will do):

   ```sh
   apt-get update && apt-get install -y backblaze-b2
   backblaze-b2 authorize-account     # asks for the master key's id, then the key
   backblaze-b2 create-key --bucket my-backups motel-box listBuckets,listFiles,readFiles,writeFiles
   backblaze-b2 create-key --bucket my-backups motel-laptop listBuckets,listFiles,readFiles,writeFiles
   backblaze-b2 clear-account         # forgets the master key authorize-account stored on this machine
   ```

   Each `create-key` prints the new key's id and the key itself, once:
   the keyID and applicationKey are what `setup` asks for as the
   storage key id and secret. The master key has now been typed at a
   terminal: generate a new one in the console, which ends the old.
   Never give `setup` the master key, which the S3 endpoint does not
   take, nor a key or bucket made before 4 May 2020, which are not
   S3-compatible (Backblaze's help,
   [archived](https://web.archive.org/web/20230925042836/https://help.backblaze.com/hc/en-us/articles/360047425453)).
3. **The lifecycle rule**: in the bucket's Lifecycle Settings, "Keep
   prior versions for this number of days": 180. B2 then removes a
   hide marker itself once the versions under it are gone
   ([lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules)).
   Not "Keep only the last version", which restic's documentation
   suggests: it frees the space a day after a hide, and would make a
   stolen key's deletions final after one. Here the 180 days are the
   protection.

### On AWS S3

Turn Versioning on, add a lifecycle rule that permanently deletes
noncurrent versions after 180 days and deletes expired object delete
markers — restic leaves about two markers a command, and S3 keeps each
one after its versions are gone, unless told — and make each key a user
of its own with this policy (`my-backups` is the bucket):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-backups/*" },
    { "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::my-backups" },
    { "Effect": "Deny",
      "Action": ["s3:DeleteObjectVersion", "s3:PutBucketVersioning",
                 "s3:PutLifecycleConfiguration", "s3:PutBucketPolicy",
                 "s3:DeleteBucketPolicy", "s3:DeleteBucket"],
      "Resource": ["arn:aws:s3:::my-backups", "arn:aws:s3:::my-backups/*"] }
  ]
}
```

The policy lets restic do all it does and refuses to destroy a version,
suspend versioning, change the lifecycle rule or remove the bucket. It
has been checked against MinIO standing in for S3, not against AWS
itself.

### Elsewhere

On Hetzner Object Storage versioning and the rule for old versions are
there, but not the one for delete markers, which pile up (restic lists
only current files, so it does not mind); and every key reaches every
bucket of its project, so the same denials have to be a bucket policy
naming each key — not worked out here.

## Who runs these

On the box, `motel-backup setup`, `restore`, `drill`, `run` and
`sweep`, and every `systemctl` line that starts, stops, reloads,
enables or disables a unit, are root's: run them as root over SSH
(`ssh root@box.example.com`, the way back [Hardening](hardening.md)
keeps). An administrator of your own whose `sudo` is root's puts the
word before them. What only reads needs no privilege:
`motel-backup validate` and `status`, `systemctl list-timers` and
`systemctl --failed`, and the journal through the `adm` group. The
administrator Hardening makes is in that group alone, with no `sudo`:
they — or an agent working as one — run those. Run as root, `status`
also checks the credential file, which only root can read. The
Caddyfile reaches the box by a signed push from its repository, and the
box runs `motel-backup validate` on each one before it is applied
([examples/box](../examples/box/README.md)). Keep `setup` for a person:
it shows the repository password on the terminal it runs in, and it
writes the one credential that reads every backup ever made.

## Set it up

As root on the box:

<!-- smoke: begin -->
```sh
# 1. In the app's block of the Caddyfile, what to back up:
#        backup {
#            sqlite app.db
#            files  uploads
#        }
#    With a box repo (examples/box), commit and push it: the box asks
#    motel-backup validate before it applies the push, and reloads
#    itself, so skip the next two lines. Without one, edit
#    /etc/motel/Caddyfile, then, before it goes live, ask whether a
#    run could plan from it:
motel-backup validate /etc/motel/Caddyfile
systemctl reload motel
# 2. The repository, once: the storage key is asked for at the terminal,
#    and a new repository's password is shown once, to store elsewhere.
motel-backup setup s3:https://s3.example.com/my-backups
# 3. The first backup runs within the hour and ten minutes
#    (systemctl list-timers motel-backup.timer), or now:
systemctl start motel-backup.service
motel-backup status
# 4. A restore: into a directory of root's own to look at, or into
#    place — which backs the app up first, tagged pre-restore, and asks.
motel-backup restore demo --to /root/demo-restored
motel-backup restore demo
```
<!-- smoke: end -->

The package's smoke test (`make install-test`) runs exactly those
lines, read out of this page, as root — `validate` and `status` as an
account with no privilege — on a fresh Debian 13 with the `.deb` just
installed and no `sudo`. Keep the block in the box repo's Caddyfile:
a push of a file without it stops that app's backups, and a run says
so once. The other order works too: `setup` before any block says
that a run would back nothing up.

Have your password manager open: a new repository's password is shown
once. `setup`, for a bucket with no repository in it yet:

```
$ motel-backup setup s3:https://s3.example.com/my-backups
account motel-backup: present
a run would back up demo, under /var/lib/liveswap
Storage key id (AWS_ACCESS_KEY_ID): …
Storage secret key (AWS_SECRET_ACCESS_KEY):
looking for a repository at s3:https://s3.example.com/my-backups (up to 10s)
no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here; a new repository will be made with the password shown next
Repository password (new): <52 characters>
Store it off the box now, with the repository URL, s3:https://s3.example.com/my-backups, and the storage key: with those three, `restic -r <url>` reads every backup from any machine; without the password nothing can.
The look could not tell whether a repository is there: store this password beside any you already keep for this repository, never in its place; if one is there, setup says so next and asks for its own.
Type stored to go on: stored
repository ready: s3:https://s3.example.com/my-backups (new, id e9157010)
credentials: /etc/motel-backup/repository.env (root, 0600)
next: the first backup runs within the hour and ten minutes (systemctl list-timers motel-backup.timer); to run one now, systemctl start motel-backup.service; then motel-backup status
```

What can stop `setup`, and what it says:

- **A wrong key** is asked for again, up to three times. A storage
  that refuses the key plainly, as B2 does (`Stat: Access Denied`),
  is refused at the look, and no password is shown. Where nothing
  answers the look within 10 s — a bucket not made yet, a wrong key
  and a wrong host look alike there — a password is shown first and
  stands through the askings, and a setup that ends without a
  repository says whether to keep that password or discard it
  ([Setup](../backups/README.md#setup)).
- **A bucket that is not there**, which the key may not make — a key
  made as [above](#before-you-start-the-bucket-and-what-it-protects)
  never may — ends `setup` at once, naming the bucket: check its name
  in the URL, or make it first.
- **A key that cannot write.** `setup` opens a repository that is
  there already by taking its lock, which writes a file and removes
  it, so a read-only key is refused at `setup`, and not a quarter of
  an hour into every run.

Then [look from your own machine](#look-from-your-own-machine) with the
URL, the key and the password as you stored them: a password copied
wrong is otherwise found only when the box is gone.

## Is it working

`motel-backup status`, as anyone, exits 0 when all is well — every
app backed up within 3 hours, a restore of it proven within 8 days, the
repository's last check clean ([the whole rule](../backups/README.md#status))
— 1 when not, and 3 when it could not look. Right after `setup` it is
`no run yet: pending first run`, exit 0. After the timer's first run:

```
$ motel-backup status
last run 2026-10-03 07:01 UTC
the repository has not been checked yet: the weekly drill checks it
demo: ok: last complete backup 2026-10-03 07:01 UTC, snapshot c4f0d7c1 (data at /var/lib/liveswap/demo/shared)
demo: restore last proven: 2026-10-03 07:01 UTC, snapshot c4f0d7c1
no backup unit running now
```

An app's first good backup, up to 1 GiB restored, is drilled by the
run that made it, as here. A larger one is left to the weekly drill,
and until then `status` says `restore not proven` and exits 1:
`motel-backup drill` proves it now. A run that fails is a
failed unit until the next one succeeds — `systemctl --failed` lists
`motel-backup.service` — and `journalctl -u motel-backup.service`
(the `adm` group reads it) has one line per app:

```
motel-backup[9377]: demo: ok: snapshot c4f0d7c1 holds sqlite app.db, files uploads
motel-backup[9377]: demo: restore proven: snapshot c4f0d7c1 was fetched and checked whole
```

What every line of `status` means is under
[Status](../backups/README.md#status). `motel-backup status --json` says the
same as one JSON object, with the same exit status, for a program to
read ([As JSON](../backups/README.md#as-json)).

### Look from your own machine

The box's `status` is only as honest as the box. Right after `setup`,
and at least once in every 90 days — half the time old versions are
kept, so that the other half is left to recover — look from a machine
of your own with Debian's restic (0.18), in `bash`, with your own
machine's key and the repository's password. In a `debian:13`
container, `apt-get update && apt-get install -y restic ca-certificates`:
restic alone brings no CA certificates, and fails `x509: certificate
signed by unknown authority`. `restic snapshots` says whether the
backups are fresh, and `restic check --read-data` whether anything
under them was hidden, overwritten or damaged, which a fresh-looking
listing does not show.

```bash
export RESTIC_REPOSITORY=s3:https://s3.example.com/my-backups
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # your own machine's key
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY
# restic asks for the repository's password, for each.
restic snapshots --latest 1 --tag motel --group-by host,paths,tags
restic check --no-lock --read-data
```

On B2 the URL is the bucket's S3 endpoint, the two key lines carry
your own key's keyID and applicationKey, and the region goes with them,
as `setup` writes it into the box's file, so that restic does not ask
B2 for it:

```bash
export RESTIC_REPOSITORY=s3:https://s3.eu-central-003.backblazeb2.com/my-backups
export AWS_DEFAULT_REGION=eu-central-003                         # the endpoint's second part
read -r AWS_ACCESS_KEY_ID && export AWS_ACCESS_KEY_ID          # your own key's keyID
read -rs AWS_SECRET_ACCESS_KEY && export AWS_SECRET_ACCESS_KEY # its applicationKey
```

The newest backup of each app should be within the hour; a snapshot a
restore made of what it restored over (tagged `pre-restore`) is listed
apart, and is not a backup:

```
snapshots for (host [motel], tags [app:demo, motel], paths [/backup/demo]):
ID        Time                 Host        Tags               Paths         Size
---------------------------------------------------------------------------------------
66724c5e  2026-10-03 11:48:35  motel    app:demo,motel  /backup/demo  400.008 MiB
---------------------------------------------------------------------------------------
1 snapshots
snapshots for (host [motel], tags [app:demo, motel, pre-restore], paths [/backup/demo]):
ID        Time                 Host        Tags                           Paths         Size
---------------------------------------------------------------------------------------------------
6efa7a46  2026-10-03 08:48:42  motel    app:demo,motel,pre-restore  /backup/demo  400.008 MiB
---------------------------------------------------------------------------------------------------
1 snapshots
```

The check should end `no errors were found`. It reads the whole
repository back: the repository's size in egress each time (S3: $0.09/GB
past the free 100 GB a month; B2: free up to three times what is
stored; Hetzner: within its 1 TB). `--no-lock` keeps the box's runs
from waiting on it; run it when you are not pruning, which could make
it report damage that is not there. `Fatal: wrong password or no key
found`, exit 12, after the third asking, is a password typed wrong — or,
where the stored password is right, key files hidden by an attack.
That, nothing there, a check that finds errors, or no repository at
all: [after an attack](#after-an-attack).

## Restore

Into a new directory first, to look before anything changes:

```
$ motel-backup restore demo --to /root/demo-restored
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /root/demo-restored: sqlite app.db, files uploads
```

Every directory on the way has to be root's own and writable by nobody
else: `/root`, `/srv`, `/var/backups`, not `/tmp`. `--to` is root's
alone, and never to be granted through `sudo`
([why](../backups/README.md#a-restore)).

Into place, with the app running. It asks, takes the app's name typed
back for a yes, and backs the app up first:

```
$ motel-backup restore demo
demo: restore snapshot 39021de9, made 2026-10-03 07:01 UTC, into /var/lib/liveswap/demo/shared?
What is there is backed up first. Its databases are then replaced and its files overwritten; what the snapshot does not hold is left.
Type the app's name, demo, to go on: demo
demo: backed up first: snapshot 66e09572 (restore --snapshot 66e09572 puts back what was there)
demo: restored from snapshot 39021de9 of 2026-10-03 07:01 UTC into /var/lib/liveswap/demo/shared: sqlite app.db, files uploads
```

`motel-backup restore demo --snapshot 66e09572` undoes it: the
snapshot tagged `pre-restore` holds what was there. A restore needs the
snapshot's size free twice over where the fetch and the app share a
disk, and holds the run lock throughout: an hourly run that comes due
meanwhile fails, and says why.

## A rebuilt box

A new box, or the same one reinstalled; the `motel` user may get
another uid, which a restore does not mind:

1. Install the package ([Your first deploy](first-deploy.md), step 2)
   and give it the box's Caddyfile with `motel init` from the box
   repository, as the first deploy did (step 3; with secrets,
   [A rebuilt box](secrets.md#a-rebuilt-box)).
2. `motel-backup setup --existing <the same URL>`, with the box's
   key. `--existing` is your word that the repository is there: setup
   makes no password and shows none, whatever the storage's first
   answer, and asks for the repository's own:

   ```
   looking for a repository at s3:https://s3.example.com/my-backups (up to 2m0s)
   the repository exists; its password is needed
   Repository password:
   repository ready: s3:https://s3.example.com/my-backups (existing, id e9157010)
   ```

   With the flag the look has two minutes, not ten seconds, so a
   storage slow to answer is waited for; one that has not answered by
   then is said, naming what looks alike there (a bucket not made yet,
   a wrong key, a wrong host), and the key is asked for again; the
   third asking in all ends setup, with nothing written. If the
   storage says there is no repository at that URL, setup ends and says
   so: check the URL — the bucket and the path — against the old
   box's. (Without
   the flag, a slow first look is taken for a new repository: a
   password is shown, to be stored, and only then is the repository
   found and its own password asked for.)

3. Before each app's first deploy, `motel-backup restore <app>`.
   With nothing in place there is nothing to back up first:

   ```
   demo: restore snapshot 391c887b, made 2026-10-03 07:03 UTC, into /var/lib/liveswap/demo/shared?
   Its databases are replaced and its files overwritten, with no backup first; what the snapshot does not hold is left.
   Type the app's name, demo, to go on: demo
   demo: restored from snapshot 391c887b of 2026-10-03 07:03 UTC into /var/lib/liveswap/demo/shared: sqlite app.db, files uploads
   ```

4. Deploy each app. The next hourly run backs it up as before.

An hourly run that comes between 2 and 3 finds the data missing, and
`status` says so and names the restore. A repository is one box's: if
the old box still runs, turn its timers off first
(`systemctl disable --now motel-backup.timer motel-backup-drill.timer`).

**A backup holds what the apps declare under `shared/`, and nothing of
`/etc/motel`**: not the Caddyfile, whose `backup` blocks a restore
needs before it can run, no env file, and no `deploy_trust local`
public key file. The Caddyfile and the secrets come back from the box
repo, which the first deploy makes
([examples/box](../examples/box/README.md)): the Caddyfile with
`motel init` in step 1; the secrets with `make rekey`, pushed and
applied before step 4, since until then the box has no env file for
the app ([A rebuilt box](secrets.md#a-rebuilt-box)). A `deploy_trust
local` public key is a file the Caddyfile names, not a commit, and
`motel init` refuses a Caddyfile whose key file is missing: in
step 1, after the install and before `motel init`, put the `.pub`
back from where the pair is kept. Or make a new pair with `motel
deploy-keygen`: its private key then replaces the old one wherever
tokens are minted — the CI secret, or your own machine — and only the
`.pub` goes to the box. Until the private key is replaced, a deploy
minted with the old one is refused: a flat 401, and the journal's
`webhook auth failed` line carries, in its `refused` field,
`local:<path>: go-jose/go-jose: error in cryptographic primitive`
([liveswap/README.md](../liveswap/README.md)).

Every other `env_file` the Caddyfile names comes back only if it is in
the box repo. One made by hand, such as `/etc/motel/demo.env`, is in
neither the backup nor the repo, and a launch that reads a missing env
file fails. Move a hand-made one in by
[Move a hand-made env file in](secrets.md#move-a-hand-made-env-file-in)
while the old box still runs: until then, nothing this guide covers
restores it.

## The weekly drill, and what it costs

Every Sunday between 03:30 and 03:40, the box's time — an offset of
this box's own — the drill fetches the newest snapshot of every app
whole, checks it as a restore would, and installs nothing; then it
checks the repository's structure and one fifty-second of its data.
`motel-backup drill` runs it now:

```
$ motel-backup drill
demo: restore proven: snapshot 39021de9, on 2026-10-03 07:02 UTC
repository: its structure, and data group 40/52: clean
```

On B2 (EU Central, from an arm64 Hetzner VPS) with 500 MB of new
uploads, the run that uploaded them took 24 s and the drill that
fetched them back 14 s, its check included.

- **Egress:** each week, the apps' size, plus 1/52 of the repository.
  About 4.3 times the apps' size a month — less for data that
  compresses, which restic does.
- **Disk:** the largest app's restored size, free under
  `/var/lib/motel-backup`. An app that does not fit is not drilled,
  `status` says `restore not proven` with how much it needs, and stays
  unhealthy until there is room.
- **Time:** the drill holds the run lock. An hourly run that comes due
  meanwhile fails at once, and is that hour's failed unit:

  ```
  motel-backup[3611]: motel-backup: another backup run is in progress: pid 3450, since 2026-10-03T07:06:41Z
  systemd[1]: motel-backup.service: Failed with result 'exit-code'.
  ```

  On a slow link a long drill fails every run it overlaps, and past 3
  hours `status` calls the backups stale.

The drill's egress at the providers' prices, fetched 2026-10-03, per
month, by the apps' total size (the check's share left out):

| Apps | Drill egress | S3, us-east-1 | B2 | Hetzner Object Storage |
|---|---|---|---|---|
| 1 GB | 4.3 GB | $0 | ~$0.01 | €0 |
| 10 GB | 43 GB | $0 | ~$0.13 | €0 |
| 30 GB | 130 GB | ~$2.70 | ~$0.40 | €0 |
| 100 GB | 433 GB | ~$30 | ~$1.33 | €0 |

- **S3:** $0.09/GB out past 100 GB a month free, which is shared by
  the whole AWS account; with that free tier spent elsewhere, $0.39 a
  month per GB of apps.
- **B2:** egress free up to three times the average stored, then
  $0.01/GB; the figures take the repository as the apps' size, and a
  larger repository makes them lower. The S3 endpoint is priced as B2's
  own API, egress and calls alike
  ([pricing](https://www.backblaze.com/cloud-storage/pricing),
  [transactions](https://www.backblaze.com/cloud-storage/transaction-pricing)).
- **Hetzner:** €6.49 a month includes 1 TB stored and 1 TB out, then
  €1/TB (before VAT): nothing more up to about 230 GB of apps.

These are list prices: no bill has been read against a real drill yet.

## Retention, off the box

Nothing on the box forgets, prunes or repairs. Without retention every
hourly run adds two snapshots an app — the backup, and a small record
that the run ended ok — and listings slow as they grow
([Retention](../backups/README.md#retention)). Thin them from your own
machine, after the first three lines of
[the look](#look-from-your-own-machine) (on B2, its four):

```bash
# restic asks for the repository's password, for each.
restic forget --retry-lock 1h --tag motel --group-by host,paths,tags \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic forget --retry-lock 1h --tag motel-clean \
  --keep-hourly 24 --keep-daily 30 --keep-monthly 12
restic prune --retry-lock 1h
```

The first `forget` thins each app's backups, and the snapshots a
restore made of what it restored over (tagged `pre-restore`) apart
from them; the second, the records that runs ended ok. One `forget`
would keep a restore's snapshot in place of that hour's backup
([why](../backups/README.md#retention)). Each keeps the newest of every
hour, day and month on its own, and restic keeps the oldest too. Add
`--dry-run` to either to see first. What they remove stays an old
version for 180 days, so the space comes back then.

**Locks.** Each needs the repository to itself, and without
`--retry-lock` gives up at once while a run of the box's holds a lock
(`repository is already locked`, exit 11); with it, it waits. A lock no
command holds any more is waited on all the same: restic never passes
a stale lock by itself. Two things leave one:

- **A storage outage that begins during a box's backup** leaves its
  lock, and the box's next backup goes on. A `prune` then waits out
  its hour and exits 11.
- **A `prune` that fails or is stopped** leaves its own, which no
  other command may share. Every run and drill of the box's then waits
  two hours for it, nothing said meanwhile, exits 11, and says the
  repository stayed locked; `status` calls the backups stale from the
  third hour. So read `prune`'s exit status, not its words: one whose
  every delete was refused printed `2 snapshots have been removed` and
  removed none.

`restic list locks --no-lock` lists what is there. `restic unlock`
removes a lock restic judges stale. `restic unlock --remove-all`
removes every lock, a running command's too, so first stop the box's
timers and let any run end (`systemctl stop motel-backup.timer
motel-backup-drill.timer`; `motel-backup status` says whether a
unit is running), and have no command of your own running; then start
the timers again, and prune.

**When to prune.** `prune` takes the repository for itself. A run's
upload and a drill's fetch that come due wait for it, up to two hours,
holding the run lock meanwhile, so the hours after fail too. The
drill's check takes no lock, and a prune under way can make it say
`damaged`; `motel-backup drill` checks again at once, where the next
drill is a week away. So prune at a quiet hour, away from the drill
(Sunday from 03:30, the box's time).

**What the box says afterwards.** The next run's listing notices. A
snapshot that was the last proven restore is said, and is still proven:

```
demo: restore last proven: 2026-10-03 07:02 UTC, snapshot 39021de9
demo: snapshot 39021de9 is no longer in the repository (last seen there 2026-10-03 07:03 UTC); the restore proven was of it
```

The next drill proves the newest. A last complete backup that is gone
makes `status` unhealthy until a run makes another. Where a period's
newest backup has no record — a run that ended `incomplete`, or whose
record was not written — the record kept is of a backup that goes, and
a restore on a rebuilt box then says that nothing vouches for what
remains of that period.

## After an attack

With the box's key, or your own machine's, someone can hide, overwrite
or delete every file, and still destroy nothing for 180 days. An attack
that does all three makes the next run fail and `status` exit 1:

```
demo: failed: there is no repository at the configured location (exit 10); …
```

A quieter one — a single pack overwritten — leaves the hourly runs
ending `ok`, `status` moving its "last complete backup" on past the
attack, and only the next drill saying so (`restore not proven`, and
its check `damaged`); the [look's](#look-from-your-own-machine)
`restic check --read-data` finds the same.

Recovery is a copy of the bucket as it was before the attack, into a
new bucket made as
[above](#before-you-start-the-bucket-and-what-it-protects), with keys
the attacker never had, using Debian's rclone (1.60; in a container,
with `ca-certificates`), in `bash`.

**On B2**, a read-only key for the attacked bucket, one the box never
held (`listBuckets,listFiles,readFiles`), reads its old versions, and a
key for the new bucket writes them, as two remotes over B2's own API
(rclone's `b2` backend takes an application key):

```bash
export RCLONE_CONFIG_SRC_TYPE=b2 RCLONE_CONFIG_DST_TYPE=b2
read -r RCLONE_CONFIG_SRC_ACCOUNT && export RCLONE_CONFIG_SRC_ACCOUNT    # the attacked bucket's read-only key
read -rs RCLONE_CONFIG_SRC_KEY && export RCLONE_CONFIG_SRC_KEY
read -r RCLONE_CONFIG_DST_ACCOUNT && export RCLONE_CONFIG_DST_ACCOUNT    # the new bucket's key
read -rs RCLONE_CONFIG_DST_KEY && export RCLONE_CONFIG_DST_KEY
rclone copy "src,version_at='2026-10-03T08:49:00Z':my-backups" dst:my-backups-recovered
```

**On AWS S3 and other S3 providers**, the account's own key, which may
read old versions, as one remote:

```bash
export RCLONE_CONFIG_STORE_TYPE=s3 RCLONE_CONFIG_STORE_PROVIDER=AWS RCLONE_CONFIG_STORE_REGION=<the bucket's region>
read -r RCLONE_CONFIG_STORE_ACCESS_KEY_ID && export RCLONE_CONFIG_STORE_ACCESS_KEY_ID          # the account's key
read -rs RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY && export RCLONE_CONFIG_STORE_SECRET_ACCESS_KEY
rclone copy "store,version_at='2026-10-03T08:49:00Z':my-backups" store:my-backups-recovered
```

On another S3 provider, `RCLONE_CONFIG_STORE_PROVIDER=Other` and
`RCLONE_CONFIG_STORE_ENDPOINT=https://<its endpoint>` in place of the
region. Three things the copy needs, each seen going wrong without it:

- the time in UTC, with its `T` and `Z`: `2026-10-03 08:49:48` is read
  in your machine's zone, and in Berlin found nothing;
- `version_at` on the source alone: as `--s3-version-at` it makes the
  new bucket read-only too, `can't modify or delete files in
  --s3-version-at mode`;
- the single quotes, without which the time's colons cut it short:
  `couldn't parse config item "version_at" = "2026-10-03T08"`.

**The moment** has to be before the attack's first change, and the copy
decides whether it was: with `RESTIC_REPOSITORY` set to the new bucket,
and a key of the new bucket's own — made now, for it alone, as
[above](#before-you-start-the-bucket-and-what-it-protects); on B2 an
application key, since the S3 endpoint does not take the master key —
`restic snapshots` and `restic check --no-lock --read-data`. A check
that finds errors means the moment was too late — make another copy,
earlier. A check can also come back clean on a moment too late: an
attack that first hides the newest snapshot leaves a copy after that
hide whole, and short of it. So for a moment taken from the bucket's
history, the newest snapshot `restic snapshots` lists for each app must
be the one `motel-backup status` named before the attack — the
`snapshot <id>` on its `last complete backup` line; one missing means
the moment fell after the attack began, and an earlier one is needed.
(A copy at `status`'s own minute holds the backup before that one, as
below.) The copy never changes the attacked bucket, so trying again
costs only the copy. Earlier is safe: a moment in the middle of a run
leaves that run's unfinished files, which restic calls "non-critical"
(`1 additional files were found in the repo, which likely contain
duplicate data`), and the check finds no errors. Where to
start:

- **The attack stopped the runs** (the next run failed, as above):
  the minute `status` gave for the last complete backup, as printed —
  `last complete backup 2026-10-03 08:49 UTC` is `2026-10-03T08:49:00Z`.
  Never that minute rounded up, which can fall after the attack began.
- **The look's check or the drill found it, or the box is not to be
  trusted** — the runs went on, so `status` is no guide: the moment from
  the bucket's history, every write and delete with its time in UTC,
  from the provider's own tool — Debian's `awscli`, with the account's
  key in `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (and
  `--endpoint-url https://<its endpoint>` on another S3 provider):

  ```bash
  aws s3api list-object-versions --bucket my-backups --output text \
    --query "[Versions[].[LastModified,'write ',Key], DeleteMarkers[].[LastModified,'delete',Key]][]" | sort
  ```

  ```
  2026-10-03T08:49:40.290000+00:00  write   locks/ae3ab8a0…
  2026-10-03T08:49:40.526000+00:00  write   data/2b/2b3113e7…
  2026-10-03T08:49:40.532000+00:00  write   index/4050462a…
  2026-10-03T08:49:40.536000+00:00  write   snapshots/9db598a5…
  2026-10-03T08:49:40.539000+00:00  delete  locks/ae3ab8a0…
  2026-10-03T08:49:51.695000+00:00  write   config
  2026-10-03T08:49:52.027000+00:00  write   data/16/16b7af23…
  ```

  A run ends by deleting its lock. The attack begins at the first thing
  restic never does: a write to a name already there — restic writes
  every file once, `config` at `init` — or a delete of `config` or
  `keys/…`. Any moment between the two will do: `2026-10-03T08:49:45Z`
  here. A deletion of snapshots and data alone looks like a `prune`:
  compare it with when you last pruned. On B2, a run ends by hiding
  its lock, and the provider's own tool, Debian's `backblaze-b2`,
  which speaks B2's own API with any key that may list files, lists
  each upload and hide with its time to the second — by name, so sort
  it by the upload stamp inside each file id, milliseconds since the
  epoch in the ninth `_` field:

  ```bash
  backblaze-b2 authorize-account     # the attacked bucket's read-only key: its id, then the key
  backblaze-b2 ls --long --versions -r b2://my-backups | sort -t_ -k9,9
  ```

  Not rclone's `lsl --b2-versions`: of a bucket whose every file was
  hidden it listed nothing.

Once the copy checks clean, give the new bucket new keys (the old ones
are the attacker's) and follow [A rebuilt box](#a-rebuilt-box) with its
URL. The copy went through the machine running rclone, every object:
the whole repository comes down and goes up again, at the provider's
egress price.

New keys stop the attacker writing; they do not unknow the repository's
password. Whoever had root on the box had the credential file, and so
the password and, through the key files, the master key that encrypts
every backup — and restic changes a master key only by moving the data
into a new repository (`restic copy`) or starting a new one. Anyone who
later reads the bucket with them can read the backups.

Whoever had root had the apps' secrets too: every env file the
Caddyfile names, in clear — those under `/etc/motel/secrets` and
any made by hand — and the box's key with them. Then every value of
every one of them changes — database passwords, third-party keys,
hand-made files' included — on the rebuilt box in this order: `make
rekey` first, since `recipients` still names the old box's key and a
`make secret` before it would encrypt to that key and the workflow
refuse the push ([A rebuilt box](secrets.md#a-rebuilt-box)); then each
value set anew with `make secret` ([Add a secret](secrets.md#add-a-secret)),
a hand-made file's too, with the app's block changed to read
`/etc/motel/secrets/<app>.env` — not imported by
[the door](secrets.md#move-a-hand-made-env-file-in), which would carry
the attacker's values in; then the first deploy. `make rekey` alone
re-encrypts the values the attacker holds.

## What has been verified where

The release candidate `v0.5.0-rc1` ran the twelve checks of
[backups/release-candidate.md](../backups/release-candidate.md) on
Backblaze B2 through its S3-compatible endpoint on 2026-11-02,
following this guide as written, from a fresh box and from an off-box
machine; `v0.3.0-rc1` had run most of them through B2's native API,
which the box no longer uses. Each check there says what B2 showed,
and the design record under [backups/design](../backups/design/README.md)
holds the raw outputs and
[what this guide said about the providers](../backups/design/provider-results.md)
before those results moved there. The test suites run every command
against an S3 server of their own over TLS, and the bucket's
protections against MinIO standing in for a provider.

| | Backblaze B2, S3 endpoint | AWS S3 | Hetzner |
|---|---|---|---|
| `setup`, runs, restores, the drill and the look, over the provider's TLS | yes | not run | not run |
| Keys that cannot destroy a version; a delete by name only hides | yes | the policy above, on MinIO only | not worked out |
| An attack with the box's key destroys nothing; recovery from the bucket's history | yes, with the two B2 keys above | on MinIO only; the account's-key copy not run anywhere | not run |
| Retention off the box, and the box's next run after it | yes | on MinIO only | not run |
| A rebuilt box, with another uid for `motel` | yes | not run | not run |
| The lifecycle rule removes a hidden version and its marker after the period | through B2's native API; through the S3 endpoint still to be read | not run | not run |
| The drill's egress on a bill | not yet | not run | not run |

`setup` ending at once on a bucket that is not there is newer than the
candidate and has not been run on B2 yet.
