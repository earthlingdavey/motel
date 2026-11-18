# What the operator guide said about the providers

Moved from `docs/backups.md` on 2026-11-03, when the guide was cut to
what an operator needs: `docs/` is for the operator, and the evidence
lives here. The text is as the guide had it at v0.5.0 (commit
68745f6), each piece under the section it came from, with only its
links rewritten for this directory; a path quoted in the text is the
one the guide used then. The checks' own answers are in
[release-candidate.md](../release-candidate.md), the measurements
(M…) are in [plan.md](plan.md), and the raw outputs are under
[rc-runs/](rc-runs/).

## From the introduction

Every transcript below was observed on a fresh Debian 13 box with the
package installed: against the e2e suites' S3 server over TLS, and,
where a bucket's versions matter, against MinIO standing in for a
provider. On your box the URL, the ids, the times and the password
differ. On Backblaze B2 the release candidate `v0.3.0-rc1` then ran
seven of the eight [release-candidate checks](../release-candidate.md)
(2026-10-04 to 10-06), through B2's native API — restic's own `b2:`
backend, which the box no longer uses, and rclone's and
`backblaze-b2`'s B2 code — and `v0.5.0-rc1` ran all twelve through
B2's S3-compatible endpoint, the door the box uses (2026-11-02). What
B2 showed is said where it bears,
[one hang included](#against-a-real-provider), and what is still to be
read there says **unverified**. On AWS S3 and Hetzner nothing has been
run: what depends on them says **unverified**, and
[the last section](#against-a-real-provider) lists it all.

## From "Before you start: the bucket, and what it protects"

On MinIO standing in for S3, an attack made with the box's own key
was refused every destruction and [undone](../../docs/backups.md#after-an-attack). On B2,
through B2's own API on `v0.3.0-rc1`
([RC-check4-attack.out](rc-runs/RC-check4-attack.out)),
the box's key was refused every destruction too, and a copy made with
that key into the same bucket recovered the backups. Through B2's S3
endpoint, on `v0.5.0-rc1`
([check 4](../release-candidate.md#4-an-attack-with-the-boxs-key-and-the-recovery)),
the box's key was refused every destruction through both of B2's
APIs, and a copy into a new bucket recovered the last backup, made
with a read-only key of the attacked bucket and a key of the new one.
The account's key, which the guide's recovery uses, was not; that is
**unverified**. On AWS S3 and Hetzner all of it is **unverified**.

restic's own `b2:` backend is refused, by `setup` and by every run,
restore and drill: the B2 library Debian 13's restic is built with
retries a failed upload without end, and held a run for 27 hours with
nothing said ([#177](https://github.com/smallhoursorg/motel/issues/177),
[#354](https://github.com/smallhoursorg/motel/issues/354)). restic's
own documentation points B2 users to the S3 endpoint "due to issues
with error handling in the current B2 library that restic uses"
([Backblaze B2](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#backblaze-b2),
`doc/030_preparing_a_new_repo.rst`).

The web console offers three presets and no single capabilities —
Read and Write, which may delete, Read Only and Write Only — so make
the two keys once from the command line, with Debian 13's
`backblaze-b2` (3.19; a `debian:13` container will do: apt brings the
CA certificates with it, which it does not with `restic` or `rclone`
[measured: the `apt-get install -s` and `apt-cache depends` lines of
[`docs/design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out`](rc-runs/rc-results/L11-tar-exclude-umask.out)]):

```sh
apt-get update && apt-get install -y backblaze-b2
backblaze-b2 authorize-account     # asks for the master key's id, then the key
backblaze-b2 create-key --bucket my-backups motel-box listBuckets,listFiles,readFiles,writeFiles
backblaze-b2 create-key --bucket my-backups motel-laptop listBuckets,listFiles,readFiles,writeFiles
backblaze-b2 clear-account         # forgets the master key authorize-account stored on this machine
```

Each `create-key` prints the new key's id and the key itself, once.
(The keys of `v0.5.0-rc1`'s [checks](../release-candidate.md) were made
with these lines, `--duration` added so that they expire.)
The master key has now been typed at a terminal: generate a new one in
the console, which ends the old.

B2 lets such a key delete by name — restic's lock removal and `prune`
both do — and not delete a version, through the S3 endpoint
[measured, `v0.5.0-rc1`,
[check 3](../release-candidate.md#3-keys-that-cannot-destroy-a-version)]:
a delete by name answered `"DeleteMarker": true` and left the version
under its marker, and a delete of that version by its id `AccessDenied
… not entitled`. Backblaze's own page says as much in prose (a delete
by name needs `writeFiles`, a delete of a version `deleteFiles`), while
its table lists Delete Object under `deleteFiles` alone. A backup that held its
lock for 30 minutes ended `snapshot … saved`, no lock left, through
the S3 endpoint as through B2's own API on `v0.3.0-rc1`, and a key
with `deleteFiles` added only hid what restic removed too.

In the bucket's Lifecycle Settings, "Keep prior versions for this
number of days": 180. B2 then removes a hide marker itself once the
versions under it are gone — its
[lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules),
and seen for a hide made through B2's own API on a bucket whose rule
keeps them a day: a file hidden on 2026-10-04 and its marker were both
gone two days on
([RC-lifecycle-day2.out](rc-runs/RC-lifecycle-day2.out)); a hide made through the S3
endpoint is the same marker by Backblaze's documentation, **unverified**
until 2026-11-04, when `v0.5.0-rc1`'s is read (check 3).
restic's documentation suggests "Keep only the last version" instead,
which frees the space a day after a hide; here the 180 days are the
protection, and that rule would make a stolen key's deletions final
after one.

On MinIO that policy let restic do all it does and refused to destroy
a version, suspend versioning, change the lifecycle rule or remove the
bucket. **On Hetzner** versioning and the rule for old versions are
there, but not the one for delete markers, which pile up (restic lists
only current files, so it does not mind); and every key reaches every
bucket of its project, so the same denials have to be a bucket policy
naming each key — not worked out here.

## From "Set it up"

A wrong key is asked for again, up to three times, the password shown
standing — on B2 a wrong key was refused at once (`401`): on
`v0.3.0-rc1`, after a look that said it had timed out, the password
shown, then the init refused; the current look refuses it there and
shows none (through B2's S3 endpoint, `v0.5.0-rc1`: `Stat: Access
Denied`, three askings in 0.66 s); a setup that ends early says
whether to keep that password or discard it
([backups/README.md](../README.md#setup)).

## From "Look from your own machine"

The box's `status` is only as honest as the box. Right after `setup`,
and at least once in every 90 days — half the time old versions are
kept, so that the other half is left to recover — look from a machine
of your own with Debian's restic (0.18), in `bash`, with your own
machine's key and the repository's password (in a `debian:13`
container, `apt-get install -y restic ca-certificates`: restic alone
brings no CA certificates, and fails `x509: certificate signed by
unknown authority` [measured: the failure in
[`docs/design/backups/rc-runs/rc-results/L6-offbox-container.out`](rc-runs/rc-results/L6-offbox-container.out),
the dependency in the `apt-cache depends restic` line of
[`docs/design/backups/rc-runs/rc-results/L11-tar-exclude-umask.out`](rc-runs/rc-results/L11-tar-exclude-umask.out)]):
`restic snapshots` for
whether the backups are fresh, and `restic check --read-data` for
whether anything under them was hidden, overwritten or damaged, which a
fresh-looking listing does not show.

## From "The weekly drill, and what it costs"

The release ships this full drill. A lighter weekly one, which would
not fetch every app whole, is for later or never: what the full one
costs on a real provider decides it. On B2 (EU Central, from an arm64
Hetzner VPS) with 500 MB of new uploads, the run that uploaded them
took 24 s and the drill that fetched them back 14 s, its check included
[measured].

All **unverified** against a bill: one real drill's egress is
[release-candidate check 8](../release-candidate.md#8-one-drill-timed-and-its-egress).

## From "Retention, off the box"

Each needs the repository to itself, and without `--retry-lock` gives up
at once while a run of the box's holds a lock: `repository is already
locked`, exit 11 [measured]; with it, it waits. A lock no command
holds any more is waited on all the same: restic never passes a stale
lock by itself [M75]. Two things leave one, each measured against the
e2e S3 server behind a proxy that injects the fault:

- **A storage outage that begins during a box's backup** leaves its
  lock, and the box's next backup goes on [measured]. A `prune` then
  waits out its hour and exits 11: restic refuses an exclusive lock
  over any lock there, never asking whether it is stale (restic 0.18.0,
  `internal/restic/lock.go:188-189`; M75, measured for a backup).
- **A `prune` that fails or is stopped** leaves its own, which no
  other command may share — after 35 minutes with every delete
  refused, three of them. Every run and drill of the box's then waits
  for it — two hours by its `--retry-lock`, of which 40 minutes were
  watched, nothing said meanwhile — exits 11, and says the repository
  stayed locked; `status`
  calls the backups stale from the third hour. So read `prune`'s exit
  status, not its words: the one whose every delete was refused
  printed `2 snapshots have been removed` and removed none. On B2 a
  key without `deleteFiles` may delete by name, and `prune` hid what
  it removed [measured, `v0.5.0-rc1`, release-candidate check 5].

On the e2e S3 server, after four runs, a restore
and its undo in one hour, the first kept both `pre-restore` snapshots
and the first and last backups (restic keeps the oldest too) and
removed the two in between, the second removed those two's records,
and `restic check` then found no errors. On MinIO, with a key that
cannot destroy a version, the two removed what the policy said and
destroyed nothing: every removal stayed an old version. On B2, a
`forget` with a key without `deleteFiles` hid the snapshot's file and
left its version [measured].

## From "After an attack"

With the box's key, or your own machine's, someone can hide, overwrite
or delete every file, and still destroy nothing for 180 days. On MinIO
an attack that did all three, and then tried to destroy the versions
(`Access Denied`; on B2 `401` for every file), made the next run fail
and `status` exit 1:

```
demo: failed: there is no repository at the configured location (exit 10); …
```

A quieter one — a single pack overwritten — left the hourly runs ending
`ok`, `status` moving its "last complete backup" on past the attack,
and only the next drill saying so (`restore not proven`, and its check
`damaged`) [measured]; the [look's](../../docs/backups.md#look-from-your-own-machine)
`restic check --read-data` finds the same.

On another S3 provider, `RCLONE_CONFIG_STORE_PROVIDER=Other` and
`RCLONE_CONFIG_STORE_ENDPOINT=https://<its endpoint>` in place of the
region; on B2, `RCLONE_CONFIG_STORE_TYPE=b2` with `…_ACCOUNT` and
`…_KEY` (a key's keyID and applicationKey), and the same
`store,version_at='…'`: rclone's own B2 code, which speaks B2's own API
rather than the S3 endpoint the box uses, and which takes an
application key or the account's master key
([rclone's B2 page](https://rclone.org/b2/)). There a copy at
`status`'s minute and one from the bucket's history both checked
clean, and a box set up onto the copy (through restic's own `b2:`
backend) ran, drilled and was healthy [measured — with the box's own
key, which on B2 reads old versions too (`listFiles` and `readFiles`),
and into a folder of the attacked bucket
([RC-check4-recover.out](rc-runs/RC-check4-recover.out))].

That way the copy from the bucket's history held the last backup and
checked clean, and a box set up onto it ran, drilled and was healthy
[measured, `v0.5.0-rc1`,
[check 4](../release-candidate.md#4-an-attack-with-the-boxs-key-and-the-recovery)].
The account's key, as written above, is **unverified**. On AWS S3 and
Hetzner all of this is **unverified**.

On the RC's own listing a hide and its upload in one second came
out upload first, 346 ms apart [measured,
`rc-runs/rc-results/L10-timeline-sort.out` in the design record]. Not
rclone's `lsl --b2-versions`: of a bucket whose every file was
hidden it listed nothing (the RC's record of 2026-10-05, in the
plan; no capture).

## Against a real provider

**On Backblaze B2**, two release candidates ran the checks of
[release-candidate.md](../release-candidate.md):
- **`v0.3.0-rc1`,** from 2026-10-04 to 10-06, all but retention,
  through B2's native API: restic's own `b2:` backend, which the box no
  longer uses, and rclone's and `backblaze-b2`'s B2 code. It ran from an
  arm64 Hetzner VPS and from fresh test boxes.
- **`v0.5.0-rc1`,** on 2026-11-02, all twelve, through B2's
  S3-compatible endpoint, the one the box uses, from the same VPS
  ([its record](rc-runs/v0.5.0-rc1.md)).

Each line below says which run it is. What is still to be read says
**unverified**.

- **TLS**: `setup`, runs, restores, the drill and the look from another
  machine, over B2's own certificate, through both doors.
- **The bucket above**:
  - **Through the S3 endpoint:** with keys without `deleteFiles`, the
    backups, retention, the checks, restores and drills all ran and
    left no lock, a backup holding its lock for 30 minutes included. A
    delete by name hides, a delete of a version is refused
    (`AccessDenied … not entitled`), and a key with `deleteFiles` added
    only hid what restic removed too.
  - **An attack with the box's key** destroyed nothing through either
    API: `401` for every file on the native one, `AccessDenied` on the
    S3 one. It then overwrote `config` and hid everything, and the next
    run failed `exit 10`. A copy from the bucket's history into a new
    bucket, made with a read-only key of the attacked bucket and a key
    of the new one, held the last backup and checked clean, and `setup`
    onto it was healthy.
  - **Through the native API,** `v0.3.0-rc1`: the same, with 17 files.
    B2 removed a hidden file's version and its hide marker within two
    days of a 1-day rule. Through the S3 endpoint that is read on
    2026-11-04, **unverified** until then.
- **Retention off the box**: through the S3 endpoint, `prune` hid 10
  files over versions that stay, the check after it was clean, and the
  box's next run ended `ok`.
- **A bucket that does not exist**: through the S3 endpoint the look
  says at once that no repository is there, a password is shown, and
  init is refused, `client.MakeBucket: not entitled`. `v0.5.0-rc1` then
  asked for the key twice more (1.3 s, nothing written); from #388 on,
  `setup` ends at once, naming the bucket, **unverified** on B2 until
  the next candidate. Through the native API, three askings in 7.5 s
  (`NewBucket: b2_list_buckets: 401:`).
- **A wrong secret**: through the S3 endpoint it is refused at the look
  itself (`Stat: Access Denied`), with no password shown: three askings
  in 0.66 s. Through the native API, three in 3.8 s
  (`b2.NewClient: b2_authorize_account: 401:`). B2 answers at once
  where the e2e server kept restic retrying.
- **A read-only key**: `setup` opens a repository that is there
  already taking its lock, which writes a lock file and removes it. A
  key that cannot write a lock file ends setup on one neutral line:
  - at once where the storage says so plainly, with what restic said,
    without its retry lines, cut at 300 characters;
  - at its two-minute clock where restic retries, with the last line
    restic said, which is then a retry line.

  A key that wrote the lock file and could not remove it is refused by
  restic's own word that it could not, `error while unlocking:`
  ([M4](rc-runs/rc-results/M4-unlock.out), cases A and
  B). All three were seen on the e2e server, behind a proxy that
  refuses writes. Through the S3 endpoint, `v0.5.0-rc1` refused a key
  with `listBuckets`, `listFiles` and `readFiles` at once, in 2.7 s
  (`client.PutObject: not entitled`), nothing written. Through the
  native API, `v0.3.0-rc1`'s `setup`, which only read, took a console
  Read Only key, and every run then failed after 14½ minutes of retries
  (`b2_get_upload_url: 401:`, from restic's own `b2:` backend), as did
  the drill after it, after 14 minutes.
- **A rebuilt box** with another uid and gid for `motel`: through
  the S3 endpoint, the guide's way back — `moteld init` from the box
  repo's file, `setup` on the same URL, and the restore before the
  first deploy — put back 501 MiB in 17 s, every file the new ids'. The
  first run and the drill after the deploy were `ok` and proven.
  Through the native API, `v0.3.0-rc1`, a box with no box repo, its
  Caddyfile copied over, restored in 10 s
  ([check 7](../release-candidate.md#7-restore-onto-a-second-box-with-another-uid-for-motel)).
- **A box moved off `b2:`**: the candidate refused its runs, restores
  and drills in the words above, and `setup` on the S3 URL found the
  same repository, its id unchanged, and went on from the last backup
  (check 9).
- **One drill, timed**: through the S3 endpoint, 501 MiB, the drill
  7.8 s, its check included; through the native API, with 500 MB of new
  uploads, 14 s. Its egress on a bill: **unverified**.
- **A lock removed in the second it was written**: B2 warns that a file
  hidden within the same second it was uploaded may not be hidden
  ([calling the S3-compatible API](https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api)),
  which a restic command shorter than a second does to its own lock
  file. Through the S3 endpoint, 100 such commands left no lock. Through
  the native API, an upload and its hide in the same second held
  ([RC-lifecycle-day0.out](rc-runs/RC-lifecycle-day0.out)).
- **A key for a name prefix**: works inside its prefix, and is refused
  outside it in 0.22 s (`Stat: Access Denied`).
- **The region**: without it, B2 refuses the bucket's location to these
  keys (`403`) and takes every request after it, so the region only
  spares a request.
- **A run's upload hung**, on the owner's box, for 27 hours with no
  output, and no later run was scheduled meanwhile: `status` said
  `running: upload of dev, since …` and, after 3 hours, exited 1 with
  the backups stale. `kill -QUIT` on the unit's restic ended it, and
  the timer caught up at once. That is why the box no longer uses
  restic's own `b2:` backend (#177, #354): the B2 library Debian 13's
  restic is built with retries a failed upload without limit, below
  restic's own retries; which failure it was is not known. Over S3:
  - **A sustained fault that answers** ended the command, exit 1, some
    15 minutes into the operation it failed, in every case measured
    [M87].
  - **A storage that takes the request and never answers** ends the
    box's upload, fetch and check, since they make one try at a request
    where restic would make ten and took 100 minutes [M92]. They end
    after 15 minutes at the opening, 21–36 mid-upload and 35–51
    mid-fetch, exit 1 (or 130, restic giving up its lock) [M93].
  - **The check,** which takes no lock, holds a quarter of an hour
    mid-read for each five packs it has left [M94].
  - **A storage that answers a byte at a time** is never given up on;
    `status` is red from the third hour, naming the unit still running.

  These last ones are accepted
  ([#177](https://github.com/smallhoursorg/motel/issues/177),
  closed with their scores).

**On AWS S3 and Hetzner** none of this has been run, and every line
above is **unverified** there. Over TLS to the e2e S3 server, from a
private CA added to the box's trust store, `setup`, runs, restores and
the drill all worked; on MinIO the bucket, an attack and its recovery
did. Not known on any provider: whether the recovery copy can be made
there without coming down to your machine.
