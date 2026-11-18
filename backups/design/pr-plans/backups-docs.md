# PR 8 — the release-candidate docs

Start checks (2026-10-03): #166 `MERGED` (`d3213af`), #167 `MERGED`
(`100c162`); `git fetch --prune origin`; `main` 0 ahead of `backup`,
`backup` 18 ahead of `main`. Branch `backup-docs` from `origin/backup`
at `2f613e8`.

Outputs live in the scratchpad (`runs/`), pasted here as observed.

## 1. Procedures × box × output

The fresh boxes are the install-test image (`motel-install-test-debian-13`,
Debian 13, systemd 257.13-1~deb13u1, arm64, built from `packaging/test`
at `2f613e8`), started by hand with the `.deb` from `make package` at
`2f613e8` (`moteld_0.0.0~dev_arm64.deb`, 07:44 CEST), the root mount
made shared as the smoke does. Beside them:

- `hsb-docs-s3`: the e2e fixture image (`rclone serve s3`), **over TLS**
  from a private CA (`s3.docs.test`, the CA added to each box's trust
  store — a real provider's certificate needs none of that), two keys:
  the box's (`BOXKEYID0000`) and the off-box prune key (`PRUNEKEYID00`).
  Both may do anything: the fixture enforces no policy.
- `hsb-docs-laptop`: Debian 13 with Debian's restic 0.18.0 — the
  operator's own machine, off the box.

Administrators on the boxes: `alice`, made exactly as
`docs/after-first-deploy.md` step 2 makes her (`examples/box/sudoers`,
groups `adm,motel-admin`); `bob`, in Debian's `sudo` group,
`NOPASSWD: ALL`. Each line is typed at a real terminal (`script(1)`'s
pty, `e2e/backup/tty.sh`'s `converse`).

| # | Procedure | Box | As | Output (`runs/`) | Result |
|---|---|---|---|---|---|
| P1 | first-deploy.md step 2: `apt install ./moteld_…deb`, `systemctl enable --now moteld`; an app `demo` deployed, a WAL database and `uploads/` under its `shared/` | box1 | root | `P01-install.out` | as written; postinstall says the timers wait for `setup` |
| P2 | README "On a fresh box", every line | box1 | **alice** | `P02-alice.out`, `P02b-alice-pw.out` | **fails as written (F1)**: every `sudo` line but `systemctl reload moteld` is "Sorry, user alice is not allowed to execute …"; `motel-backup validate` without `sudo` works |
| P3 | `sudo motel-backup validate …`, `sudo systemctl reload moteld`, `sudo motel-backup setup s3:https://…/box1` (a bucket not there yet), `systemctl list-timers` | box1 | bob | `P03-bob-setup.out` | as written, over TLS; 13.0 s; the 10 s look gives up on the missing bucket, init makes it |
| P4 | `motel-backup status` before any run (bob, and under sudo) | box1 | bob | `P04-status-before-run.out` | `no run yet: pending first run`, exit 0 |
| P5 | the hourly timer firing, untouched: elapse 07:01:34 UTC (the box's fixed offset), journal, `list-timers`, `status` | box1 | bob | `P05-timer-fired.out` | ran 5 s, `ok`; the run's first drill proved the snapshot |
| P6 | `sudo systemctl start motel-backup.service`; `restore demo --to /root/demo-restored`; `restore demo` into place, typing `demo` | box1 | bob | `P06-run-restore.out` | as written; `backed up first: snapshot 66e09572 (restore --snapshot 66e09572 puts back what was there)` |
| P7 | the undo: `restore demo --snapshot 66e09572`; `sudo motel-backup drill`; `sudo systemctl start motel-backup-drill.service`; `status` | box1 | bob | `P07-undo-drill.out` | as written; the undo names the last ok snapshot twice (question and report, by design); check groups 40/52, 41/52 |
| P8 | off-box: `restic snapshots`, the README Retention policy `--dry-run`, then variants | laptop | the prune key | `P08a…`, `P08b…`, `P08c…` | **F2**: the README's policy removes the last complete (and last proven) backup `39021de9` and keeps the newer `pre-restore` `570b52dd` as that hour's |
| P9 | the candidate recipe for real (two `forget`s, `prune`, `check`), then a run and `status` on the box | laptop, box1 | prune key, bob | `P09-prune-recipe.out`, `P09b-prune.out`, `P09c-after-prune.out` | removes the two middle run snapshots and their records, keeps both `pre-restore`; `status` after the next run: `demo: snapshot 39021de9 is no longer in the repository (last seen there …); the restore proven was of it`, exit 0; the next drill clean |
| P10 | §7.3 on the fixture: `restic list locks --no-lock`, `restic unlock` after runs and drills | laptop | prune key | `P73-locks.out` | no locks, both silent, exit 0 |
| P11 | README Development "On a new Debian release" step 3, as written at PR 6's sizes | dev-systemd | root | `P11-new-debian.out` | as written (the heredoc through `exec -T`, `TIMEFORMAT='%Rs'`); cold, over netem: `snapshots` 1.344 s, `ls` 1.865 s, `stats` 1.845 s, `check` 1/52 2.640 s; **the wrong key: exit 1 after 895.6 s**, "Fatal: unable to open config file … Is there a repository at the following location?" (README: 864 s and 900 s); cleanup leaves nothing. `pmset`: no sleep after 06:59 UTC, the run 07:06–07:21 UTC under `caffeinate -dims` |
| P12 | a run due while the drill holds the lock (a 400 MiB app) | box2 | bob | `P12-drill-holds-lock.out` | the run fails at once, `another backup run is in progress: pid 3450, since …`, a failed unit; the drill proves and checks clean |
| P13 | README "By hand": the file written, `systemd-run … restic init`, a run, `status` | box1 | bob | `P13-by-hand.out` | as written; init 3.7 s |
| P71 | §7.1 on the fixture: `setup` with a wrong secret, three times | box2 | bob | `P71a-wrong-secret.out` | 30.6 s, exit 1, nothing written, "keep the password shown above" |
| P74 | §7.4 on the fixture: `restic forget <id>` with the box's key, by hand | box2 | bob | `P74-box-key-forget.out` | the fixture deletes the snapshot file (its key may do anything) |
| P77 | §7.7 on the fixture: box2 (`motel` uid 993; box1's 996): `setup` onto box1's repository (asks its password), `restore demo` before the first deploy, the deploy, a run, `status` | box2 | bob | `P77-rebuilt-box.out`, `P77b-deploy.out` | as written; files owned 993, the database reads 2 rows; first run `ok`, proven |

## 2. Findings from the literal runs (each to the owner before anything is fixed)

**F1 — "On a fresh box" fails as written for the administrator it names.**
`backups/README.md:44-45` says "As the administrator
(`docs/after-first-deploy.md`), with `sudo`". That administrator's grant
is `examples/box/sudoers:17-39`: `bin/push`'s eight steps and the env
files, nothing else (`docs/after-first-deploy.md:60-66` says so). Run
as `alice`, made as that page makes her (P2):

```
alice$ sudo motel-backup setup s3:https://s3.docs.test:9000/box1
[sudo] password for alice:
Sorry, user alice is not allowed to execute '/usr/bin/motel-backup setup s3:https://s3.docs.test:9000/box1' as root on box1.
```

The same for `validate` under `sudo`, `systemctl start
motel-backup.service`, both restores and `drill`. `motel-backup
validate` without `sudo` works (`a run would back up demo, under
/var/lib/liveswap`), as `bin/push` runs it; `status` needs no `sudo`.
`after-first-deploy.md:127-128` sends anything root needs to "the
provider's console". The smoke test passes because its administrator
has `ALL` (`packaging/test/smoke.sh:574-575`).

**F2 — README Retention's policy, run as written after a restore,
removes the last complete backup.** A restore's pre-backup is
`--tag pre-restore` on top of the app's own tags and path
(`engine/engine.go:1396-1402`, `engine/restore.go:330-331`), so it is in
the app's `(host, paths)` group; it is the newest of its hour, and
`--keep-hourly` keeps the newest of each hour. P8, the policy of
`backups/README.md:616-617` dry-run against box1's repository after a
restore and its undo:

```
keep 2 snapshots:
c4f0d7c1  2026-10-03 07:01:35  motel    motel,app:demo              oldest hourly snapshot …
570b52dd  2026-10-03 07:02:08  motel    motel,app:demo,pre-restore  hourly snapshot …
remove 2 snapshots:
39021de9  2026-10-03 07:01:51  motel    motel,app:demo              /backup/demo
66e09572  2026-10-03 07:01:57  motel    motel,app:demo,pre-restore  /backup/demo
```

`39021de9` was `status`'s last complete backup and its last proven
restore. The same at `--keep-daily` for a restore after a day's last
run: the day is then kept as what was restored over. Measured fixes:
- `--keep-tag pre-restore` keeps both `pre-restore` snapshots and
  **still removes `39021de9`** (the tagged one still takes the hour);
- `--group-by host,paths,tags` separates them, and makes **every
  clean-run record a group of its own** (`vouches:<id>` differs), so
  records are never thinned (README Retention: 120,000 a year at 7 apps);
- **two commands** — `forget --tag motel --group-by host,paths,tags`
  (the apps' snapshots: records carry `motel-clean`, not `motel`)
  and `forget --tag motel-clean` (the records, by `(host, paths)`) —
  keep `39021de9`'s place right: P8c, P9 (for real: the two middle run
  snapshots and their records removed, both `pre-restore` kept, `check`
  clean, the next drill clean).

**F3 — `restore --to` is root-equivalent for whoever chooses the path
(measured, `runs/F1-restore-to-root.out`).** With `files uploads
probe.conf` declared and the app's `probe.conf` holding
`[Service]\nEnvironment=HSB_PROBE=written-by-the-app`, `sudo
motel-backup restore demo --to /etc/systemd/system/hsb-probe.service.d`
passed the `--to` rule (every directory on the way root's own), and
after `daemon-reload`: `Environment=HSB_PROBE=written-by-the-app`,
`DropInPaths=/etc/systemd/system/hsb-probe.service.d/probe.conf`. An
app administrator writes both the declaration and the bytes. No hole
while restore is root's; never delegable through sudo.

## 3. The owner's answers

- **F1 (2026-10-03): A, plus one sentence.** The backup lines that need
  root are root's — the provider's console, or an administrator whose
  sudo is root's; `validate` and `status` need neither (the `sudo`
  before `validate` goes). `examples/box/sudoers` unchanged. The
  sentence: `restore --to` is root's alone and never to be granted
  through sudo (F3). Discussed first: an AI agent fits `motel-admin`
  (status, journal, validate), never the operator (`setup` shows the
  repository password on its terminal; an agent's terminal is a
  transcript); a two-tier sudoers (app administrator, backups operator
  with pinned lines) is a security-model change and a PR of its own if
  ever. A matches PLAN §1.1 (operator with sudo = TCB; `motel-admin`
  = motel's reach) and §1.6.
- **Proposed, for the owner's word:** `DESIGN-threat-model.md` gains a
  Backups section (the credential as an asset, the principals, §1.6, the
  residuals, F3) in a PR of its own after §7 and before `backup` merges
  into `main` — a merge gate in PLAN §7, as #154 was.
- **F2: two `forget` commands, docs only** (P9's recipe; README
  Retention corrected).
- **Docs home: `docs/backups.md`**, an operator guide; README the
  reference, untrimmed; the smoke's block moves to the guide (Makefile's
  mount, smoke.sh's words).
- **§7's checklist: `docs/release-candidate.md`**, reviewed with this PR.
- **Severity bar: every genuine finding.**

## 4. Claims × what holds them

`docs/backups.md` (B), `docs/release-candidate.md` (RC), the README (R).
"Pn" is a run in table 1; code lines at `2f613e8`.

| Claim | Where | Held by |
|---|---|---|
| hourly run, offset ≤ 10 min of the box's own; drill Sun 03:30–03:40, box time | B, R | `packaging/motel-backup.timer:13-16`, `-drill.timer:14-17` (no zone: local); P5 (07:01:34) |
| `sudo` lines are root's; the after-first-deploy administrator cannot | B, R, after-first-deploy | `examples/box/sudoers:17-39`; P2 |
| that administrator runs `validate`, `status`, reads the journal | B, R | P2 (validate, status); `after-first-deploy.md:63` (`adm`) |
| `setup` shows the password on its terminal; the three things to keep | B | `engine/setup.go:587-588`; P3 |
| `setup` asks for `s3:`/`b2:`; others by hand | B | README "Setup" 4, "What it refuses"; `engine/setup.go` |
| lifecycle rules by age corrupt a deduplicating repository | B | H §3.5 (reasoned, not measured) — worded as the reason |
| the smoke runs the block's lines, read from this page | B, R | `Makefile:197`, `smoke.sh:594` (after this PR) |
| setup transcript, new bucket | B, RC | P3 (URL replaced by the example; key id and password elided) |
| a wrong key asked again ×3, the password standing | B, RC | P71 |
| status: exit 0/1/3 and the rule | B | `main.go:109-111,412-414`; `status.go:15-26`; README "Status" |
| `pending first run` after setup | B | P4; `status.go:76` |
| first drill by the run up to 1 GiB | B | `restore.go:1125,1139-1141` |
| failed run = failed unit; journal lines | B | P12, P5 |
| `--to` rule; `--to` root-equivalent | B, R | README "A restore"; F3 (`runs/F1-restore-to-root.out`) |
| into place asks, backs up first, `pre-restore`, undo by `--snapshot` | B | P6, P7; `main.go:509-534,595` |
| restore needs 2× room on one filesystem; holds the run lock | B | `restore.go:848-873`; README "A restore" 1, 5; `e2e/backup/restore.sh:345,410` |
| rebuilt box: setup asks the password; restore before first deploy; another uid | B, RC | P77; `engine/setup.go:524` |
| a run before the restore: `data missing`, names the restore | B | P14; `status.go:168` |
| retire the old box's timers | B | P14 (the `disable --now` line) |
| drill: whole fetch, installs nothing, check 1/52 | B | README "The restore drill"; `check.go:93`; P7 |
| full drill ships; tiered later or never | B | PLAN §4, decided 2026-10-01 |
| drill egress ≈ 4.3× apps a month + 1/52 of the repository | B | 52/12 = 4.33; `check.go:93` |
| disk: largest app free; not proven, unhealthy | B | `restore.go:848-853`; `e2e/backup/restore.sh:397`; `status.go:201-207` |
| a run during the drill fails at once | B | P12; `engine.go:167,1879` |
| stale after 3 hours | B | `status.go:18,179` |
| prices (2026-10-03) | B | AWS price list `AWSDataTransfer` us-east-1, published 2026-09-16: $0.090/GB first 10 TB; S3 pricing page: 100 GB/month free across the account; backblaze.com/cloud-storage/pricing: free egress to 3× storage, $0.01/GB over; hetzner.com/storage/object-storage (page data): €6.49/$7.99 a month with 1 TB + 1 TB, extra egress €1/TB, extra storage €6.26/TB, before VAT |
| the box never deletes | B, R | no `forget`/`prune`/`unlock`/`repair` in non-test code (grep) |
| two snapshots an app an hour | B | README "What a snapshot holds"; P8 listing |
| the recipe, and why two | B, R | P8, P8b, P8c, P9, P9b, P9d (as written, at a tty); `engine.go:1396-1402` (tags), `check.go:272-286` (records' tags) |
| status after a prune: proven snapshot gone, said | B, RC | P9; `status.go:195-197` |
| a gone last complete backup is unhealthy | B | `status.go:177-178` |
| upload and fetch wait ≤ 2 h for a lock; the check takes none | B | `engine.go:144,1396`; `restore.go:891`; `check.go:93` |
| `restic unlock` / locks | B, RC | P10 |
| unverified list | B, RC | H §6; PLAN §7 |
| read-only key: `failed`, exit 1 wording | B, RC | `engine.go:1423-1424,1534-1535` (traced, not run) |
| fixture makes a bucket when asked | B, RC | `e2e/backup/s3.Dockerfile:5`; P3 |
| B2 hide prediction | RC | H §3.5 "Predicted, not measured" |
| §7.4 by-hand command | RC | README "By hand" form; P74 |
| uids 996/993; 400 MiB drill 7 s | RC | P77, P12 |
| a release candidate is a pushed `v*-rc` tag, CI first, a prerelease | RC | `.github/workflows/release.yml:7-20,138-143` |

## 5. Lanes

At `89dad4c` (one commit on `origin/backup` `2f613e8`), fresh containers,
`helpful/lanes.sh`: test 0 (10 s) · lint 0 (5 s) · secretscan 0 ·
test-integration 0 (229 s) · package 0 (18 s) · install-test 0 (151 s;
the smoke's lines read as `docs/backups.md: …`, `validate` without
`sudo`) · e2e-backup 0 (207 s) · e2e 0 (167 s). No sleep during the run
(`pmset`: last wake 06:59 UTC).

## 6. `/code-review xhigh` at `89dad4c`: thirteen findings, ranked before any work

| Rank | # | Finding | Verdict | Fix |
|---|---|---|---|---|
| 1 | R2 | "Give the box's key no right to delete": restic refreshes its lock every 5 min by writing a new one and **deleting** the old (`internal/restic/lock.go:309-329` at v0.18.0); a failed delete is a failed refresh, and after ~22.5 min (`internal/repository/lock.go:37,162,216-239`) the stale-lock refresh, which deletes too (`lock.go:379`), fails and restic stops: `Fatal: failed to refresh lock in time`. On S3 with `DeleteObject` denied every locking command past ~22 min stops — a large first upload, a long fetch. H §3.2 already says restic must delete its own lock files. RC check 3 runs only short commands | **genuine** (read in restic's source; not run against a provider) — could turn backups off | **to the owner** (key policy wording) |
| 2 | R1 | Step 1 edits `/etc/motel/Caddyfile` on the box; on a box run from a box repo the next `make push` removes the block, the run warns once and drops the app; a rebuilt box gets the repo's Caddyfile, without it | **genuine** — silently stops an app's backups | the block goes in the box repo's Caddyfile and `make push` (which runs `validate`); say what removing it does |
| 3 | R5 | The password is typed at a console that cannot copy out; nothing checks the stored copy until the box is gone | **genuine** — data loss | a step: from your own machine, `restic -r <url> cat config` with the stored password (run literally) |
| 4 | R3 | Retention sets only `AWS_*`; a `b2:` repository needs `B2_ACCOUNT_ID`/`B2_ACCOUNT_KEY` (`engine/setup.go`, README Setup 4) | **genuine** | the B2 pair beside it, in the snippet and RC |
| 5 | R8 | A prune during the drill leaves `damaged` until the next drill (a week); a run waiting ≤ 2 h for a prune holds the run lock, so the hours after fail | **genuine** | say both; `sudo motel-backup drill` checks again at once |
| 6 | R6 | A first backup over 1 GiB: `status` exits 1 (`restore not proven`, `restore.go:1139-1141`, `status.go:201-207`) until a drill | **genuine** | say it, and `sudo motel-backup drill` |
| 7 | R4 | Two `forget`s keep each set's newest per period; where a period's newest backup has no record (exit 3, a record not written) the record kept vouches for a backup that goes | **genuine** as an unstated limit (records and backups were separate groups before this PR too) | say it |
| 8 | R7 | `sudo systemctl reload moteld` is `alice`'s (`examples/box/sudoers:25`) — "every `sudo` line is root's" is wrong for it, here and in after-first-deploy | **genuine** | "every `sudo` line but the reload" |
| 9 | R10 | `read -rs` in a block fenced `sh`: dash has no `-s` | **genuine** | fence `bash`, say bash |
| 10 | R9 | `status` shows `c4f0d7c1` as last complete, the restore restores `39021de9`: a run in between is not shown | **genuine**, minor | say the second run |
| 11 | R12 | The `--to` warning and the two-`forget` reasoning are in both pages | **genuine**, minor | the guide says each in a sentence and links the README |
| 12 | R11 | `Documentation=`, release.yml's README.txt point at the README; `e2e/backup/tty.sh`'s comment names "the README's setup line" | tty.sh: **genuine** (stale comment). Unit files and release.yml: **not** — the README is the reference and its first paragraph links the guide; changing the units is a package change this PR does not make | tty.sh comment |
| 13 | R13 | The smoke runs the block's commands and pins none of the transcripts; dropping a line passes with ≥ 6 left | pre-existing; a transcript pin is new test code | **to the owner** |

## 7. Finding R2, investigated (the owner: "bucket-wide append-only, ransomware-resistant, easy to set up")

**Read in restic 0.18.0's source:** a lock is a file named by the hash of
its content (`internal/repository/repository.go:492`), so a refresh is a
new file and a delete of the old (`internal/restic/lock.go:309-329`),
every 5 min; a failed refresh does not count, and after
`StaleLockTimeout − 1.5 × refresh` ≈ 22.5 min the stale-lock refresh,
which deletes too (`lock.go:379`), fails and the command stops
(`internal/repository/lock.go:37,162,216-239`). S3's delete is
`RemoveObject` with no version id (`internal/backend/s3/s3.go:384`): a
delete marker on a versioned bucket. B2's tries a real delete and, on
401, hides for good (`internal/backend/b2/b2.go:266-289`); with
`deleteFiles` it deletes every version (`b2.go:272-277`). Uploads carry
`Content-MD5` (`s3.go:294`), which Object Lock asks for.

**Measured on MinIO RELEASE.2025-09-07** (a stand-in: its semantics, not
AWS's), from the laptop's restic 0.18.0 and the engine on box2:

| # | What | Output | Result |
|---|---|---|---|
| D0 | the policies: box key on a versioned bucket; no-delete key | `runs/D0-policies.out` | box key's delete = a marker; destroying a version, suspending versioning, lifecycle: `Access Denied`; no-delete key's delete: `Access Denied` |
| D1 | a short backup, then one holding its lock 30 min, on `soft` (versioned, A), `worm` (Object Lock governance 1 d, B), `nodel` (no delete, unversioned, C) | laptop `/root/lock-*` | short: A, B exit 0, 0 locks left; **C exit 0, `Remove(<lock/…>) failed: client.RemoveObject: Access Denied.`, 1 lock left**; 30 min (08:46:57–09:16:57, no sleep): **A exit 0 after 1800 s, 0 locks; B exit 0 after 1800 s, 0 locks; C `unable to refresh lock: client.RemoveObject: Access Denied.` every 5 min, at 09:09:28 (22.5 min) `failed to refresh stale lock`, then `Fatal: unable to save snapshot: context canceled`, exit 1, no snapshot, 7 locks left** — finding R2 measured |
| D2 | the engine on a versioned bucket, the box key A: setup, run, restore (pre-restore), run, drill (check clean), status | `runs/D2-softbox-setup.out` | all as written; 8 delete markers (locks) |
| D3 | the retention recipe with the prune key A (no version destroy) | `runs/D3*-softbox-retention.out` | 3 snapshots, 3 records, 10 packs removed, `check` clean; every removal a marker (49 DEL, 90 versions, 41 current); `status` says the proven snapshot is gone |
| D4 | ransomware with the box key: every object overwritten, all deleted, then `rm --versions` | `runs/D4-softbox-attack.out` | overwrites and deletes allowed (99 DEL), destroying versions `Access Denied`; the next run `there is no repository at the configured location (exit 10)`, `status` exit 1 |
| D5 | recovery by the account: `rclone copy` with `version_at` = before the attack into a new bucket; `setup` onto it | `runs/D5-softbox-recover.out` | 400 MiB copied; setup `existing, id 42a6cdc4` (the record kept); run, drill, check clean, `status` healthy |

**From the providers' docs (2026-10-03):** B2 — hide needs `writeFiles`,
destroying a version `deleteFiles` (or `bypassGovernance`); keys can be
bucket- and prefix-restricted; lifecycle "Keep prior versions for this
number of days" (`daysFromHidingToDeleting`); default keeps every
version. AWS — a `DELETE` with no version id on a versioned bucket
inserts a delete marker; only `DELETE …?versionId` removes a version;
`NoncurrentVersionExpiration` removes noncurrent versions. Hetzner —
versioning, `NoncurrentDays` lifecycle, Object Lock; every key reaches
every bucket in its project by default, narrowed by a bucket policy
naming `arn:aws:iam:::user/p<project_id>:<access_key>`.
| D6 | the attack's time from the bucket, Debian's rclone 1.60.1 (`--s3-version-at`, `--b2-version-at` both there; "a date, "2006-01-02", datetime "2006-01-02 15:04:05" or a duration … "100d" or "1h"") | `runs/D6-recover-debian-rclone.out` | `rclone lsl --s3-versions --include 'config*'`: `config` 155 B at 08:48:31 (setup), 4096 B at 08:49:51 (the attacker; restic writes `config` once); the flag on the command line makes the destination read-only too: `can't modify or delete files in --s3-version-at mode` |
| D7 | the zone, on a laptop in `TZ=Europe/Berlin` | `runs/D7-recover-tz.out` | `"2026-10-03 08:49:48"` read as local time: 0 objects; `"2026-10-03T08:49:48Z"`: 48 objects, `config` 155 B; unquoted in the inline form the colons break it |
| D8 | `rclone copy "store,version_at='2026-10-03T08:49:48Z':softbox" store:softbox-recovered2`, then restic on the copy | `runs/D8-recover-quoted.out` | exit 0; 48 objects; newest snapshot `5fe14eb2` = `status`'s last complete backup before the attack; `check --read-data` no errors |
| D9 | server-side or not | `runs/D9-recover-serverside.out` | `Copied (new)`, never "server-side copy": the whole repository through the machine running rclone (MinIO; a provider may differ) |
| D10 | a "full" backup within one repository | `runs/D10-force-backup.out` | `restic backup --force` of the same 20 MiB: `Added to the repository: 0 B`; `prune`'s repack options go by size, cacheability and compression, none by age: a full upload needs a new repository |

## 8. The owner's answers on the review (2026-10-03)

- **R2: option A now** — a versioned bucket; neither key may destroy a
  version; old versions expire after 90 days; a look from your own
  machine at least once in those 90 days; the claim as discussed
  ("nothing the box or the prune machine holds can destroy a backup; a
  deletion or overwrite with either key can be undone for 90 days, if
  someone looks from off the box within them; a stolen provider account
  can destroy everything"); Object Lock in one line, with its limit for
  restic (locks lapse N days after upload; restic's packs are reused for
  years). §7 on B2. **B with rotation (a new repository each period,
  under a compliance default retention): deferred, its benefits written
  up — maybe a GitHub issue** (draft, to the owner before it is opened).
  Discussed first, all in this session: personas, A at 30/90/no expiry,
  B's limit (AWS: retain-until = creation + duration), retention
  extension (a key that can lock you in for years), rotation
  (`--force` uploads 0 B, D10).
- **R13: the smoke holds the block to exactly its seven lines.**
- R1, R3–R12: every genuine finding fixed (the bar).

**Measured for the text option A puts in the guide:**

| # | What | Output | Result |
|---|---|---|---|
| D11 | the S3 key policy exactly as the guide prints it (`runs/guide-policy.json`), on a fresh versioned bucket with a 90-day noncurrent rule | `runs/D11-guide-policy.out` | restic init, two backups, forget, prune: exit 0, 0 locks; destroy a version, suspend versioning, touch the lifecycle rule, remove the bucket: each `Access Denied`; the rule listed `NoncurrentVersionExpiration 90` |
| D12 | the look from your own machine as the guide writes it, at a tty (prune key) | `runs/D12-look-from-own-machine.out` | right password: the newest snapshot, 09:01 (the box's last hourly run); wrong: `wrong password or no key found. Try again` ×2, then `Fatal: wrong password or no key found`, exit 12 |
| D13 | the recovery lines as the guide writes them, at a tty, on a laptop in `TZ=Europe/Berlin` (provider and endpoint MinIO's) | `runs/D13-recover-as-written.out` | copy exit 0; newest `5fe14eb2`; `check --read-data` clean. **A fourth trap:** `rclone lsl`'s time column is the laptop's zone (10:49:51) while the version name is UTC (`-v2026-10-03-084951-695`); `TZ=UTC rclone lsl …` shows 08:49:51 |

## 9. The review's fixes (one commit on 89dad4c)

| # | Fix | Seen |
|---|---|---|
| R2 | the guide's "Before you start: the bucket, and what it protects" (A: versioning, keys that cannot destroy a version — B2 capabilities, the S3 policy as measured in D11, Hetzner said not worked out — 90-day noncurrent rule, the claim, Object Lock's limit in a line); "Look from your own machine" (D12); "After an attack" (D13, the four traps); "Not yet tried" updated; RC checks 3 (a 30-min probe with the box's key) and 4 (an rclone attack with the box's key, D14, then the recovery); README Retention and repair wording | D11–D14 |
| R1 | step 1 names the box repo and `make push`; keeping the block there | — |
| R3 | B2's `B2_ACCOUNT_ID`/`B2_ACCOUNT_KEY` beside the look and retention | — |
| R4 | the two `forget`s' drift, said | — |
| R5 | "then look from your own machine" after setup's transcript | D12 |
| R6 | > 1 GiB: `status` exits 1 until `sudo motel-backup drill` | — |
| R7 | "under `sudo` but the reload", in the guide, after-first-deploy, the README | — |
| R8 | a prune holds the hours after it; `damaged` → `sudo motel-backup drill` | — |
| R9 | the second run named before the restore | — |
| R10 | fenced `bash`, "in `bash`" | — |
| R11 | `e2e/backup/tty.sh`'s comment | — |
| R12 | the guide links the README for `--to` and the two `forget`s' reasons | — |
| R13 | `smoke.sh`: the block must be exactly its seven lines | **seen failing** with `motel-backup status` dropped: `FAIL: the guide's smoke block is not the seven lines this stage runs and checks; it holds: …` (`runs/R13-pin-fails.log`) |

D14: with the box's key, `rclone delete store:<bucket>` exit 0, nothing current; `rclone delete --s3-versions` `AccessDenied` per version, 48 versions remain (`runs/D14-rclone-attack.out`). rclone 1.60.1's help: `--b2-hard-delete` "Permanently delete files on remote removal, otherwise hide files."

The rotation issue: drafted in the scratchpad (`issue-rotation.md`), to the owner before it is opened.

## 10. Copilot at `8f2542a` (2026-10-03): three findings, to the owner first

Overview "Changes recommended", 2 medium, 1 low. Each checked before the owner saw it:
1. **Expired delete markers pile up** (medium, `docs/backups.md:79`) — genuine by S3's semantics: an expiry of noncurrent versions leaves each delete marker once its versions are gone; restic leaves several an hour (locks). restic lists current objects only, so it is unaffected; version listings grow. **The owner: discuss first.**
2. **The look misses tampered data** (medium, `:217`) — genuine. D15: a version rule (a key with a current version over an older one was overwritten) named exactly the one pack the box's key overwrote, none on a clean bucket, while `restic snapshots --latest 1` looked normal. **The owner: `restic check --no-lock --read-data` from your own machine** at least once in every 90 days. D16: with the own machine's key on the A bucket, exit 0, `read all data`, `no errors were found`.
3. **`status`'s minute is not a safe recovery time** (low, `:462`) — genuine (`record/record.go:199-201`, `2006-01-02 15:04 MST`). D16: on the attacked bucket the last good write (the 08:49:38 run's record) was 08:49:40.536, the attacker's first (config) 08:49:51: `status`'s "08:49" as 08:49:00 would have lost `5fe14eb2`, and my "minute plus one", 08:50:00, would have been **after** the attack began. D17: a copy cut mid-run (08:49:39.116, after the run's pack and before its index) — restic `check --read-data` exit 0, "1 additional files were found in the repo, which likely contain duplicate data. This is non-critical, you can run `restic prune` to correct this."; the newest snapshot the previous run's. **The owner: discuss first.**

**The owner's answers on Copilot (2026-10-03):** 1 — fix: S3's rule also deletes expired delete markers; B2 removes hide markers itself (its lifecycle docs: "Every Lifecycle Rule includes an implicit rule: when the oldest version of a file is a hide marker, the marker is deleted independent of its age" — unverified); Hetzner cannot (only `NoncurrentDays`), markers pile up, harmless to restic; the RC adds a 1-day B2 lifecycle bucket looked at after the next daily run. **D5 stays** (the owner chose option 2: no B2 account in development; the RC runs B2 first). 2 — `restic check --no-lock --read-data` from your own machine at least once in every 90 days. 3 — **A, with B as an optional extra**: `status`'s minute as printed; the provider's version timeline for the exact moment (no helper: it could not live on the box, and would be new code with a parser per provider).

| # | What | Output | Result |
|---|---|---|---|
| D18 | the timeline from Debian's `awscli` 2.23.6: `aws s3api list-object-versions … --query "[Versions[].[LastModified,'write ',Key], DeleteMarkers[].[LastModified,'delete',Key]][]" \| sort` on the attacked bucket | `runs/D18-awscli-timeline.out` | every write and delete, UTC: the last good run ends `08:49:40.536 write snapshots/9db598a5…`, `08:49:40.539 delete locks/ae3ab8a0…`; the attack begins `08:49:51.695 write config` (restic writes `config` once), then overwrites of existing packs; the operator's own prune at 08:49:34–36 shows as deletes of snapshots/index/data |
| D19 | the copy at `status`'s minute (`2026-10-03T08:49:00Z`); Debian's B2 tool | `runs/D19-minute-copy-and-b2cli.out` | newest `88b3b052` (08:48:49: the runs inside 08:49 lost, as said), `check --no-lock --read-data` no errors; Debian's `backblaze-b2` 3.19.1: `ls [--long] [--versions] [-r] [B2_URI]`, `--long` shows "whether it is an uploaded file or the hiding of a file" |
| D20 | the guide's look block, extracted from the page, at a tty, own machine's key | `runs/D20-look-as-written.out` | a password prompt per command; newest snapshot 10:01:35 (box2's last hourly run); `check --no-lock --read-data` 45/45 packs, `no errors were found` |

**Copilot at `42949cc` (2026-10-03):** the three threads resolved; one "previously missed", overview only: the guide's S3 policy has no `s3:ListBucketVersions`, so RC check 4's `rclone delete --s3-versions` with the box's key fails listing versions on AWS before it tries to destroy one. **Genuine** — AWS's ListObjectVersions reference: "you must have permission to perform the `s3:ListBucketVersions` action. Be aware of the name difference." MinIO let `s3:ListBucket` list versions (D14): a stand-in difference. To the owner.

| D21 | the denial tested directly: a version id listed with the account's key, `aws s3api delete-object --version-id` with the box's (guide policy) | `runs/D21-version-delete-direct.out` | `AccessDenied`, exit 254; the version remains |

**The owner (2026-10-03): deferred** — "I don't want to create an S3 bucket just to test this." No doc change; the RC runs B2 first. Open on the PR description. Options when it is taken up: test the denial directly (D21) or grant `s3:ListBucketVersions`.

## 11. `/code-review xhigh` on #168 (the owner's, whole branch at `42949cc`): fifteen findings, ranked before any work

Measured or read first:

| # | What | Output | Result |
|---|---|---|---|
| D22 | a silent attack: the box's key overwrites box2's largest pack (11:47:29); runs; status; drill | `runs/D22-silent-attack-status.out` | the runs end `ok`: `status` "last complete backup 11:47", then **"11:48" — after the attack**; the drill: `restore not proven … restic could not restore all of it (exit 1): /var/lib/motel-backup may be out of room … or the storage stopped answering` (**misleading for tampering** — product output, not this PR's), check `damaged: … 1 damaged pack` |
| D23 | the look after a restore; forget/prune against a held lock; hidden `keys/` | `runs/D23-review2-measures.out` | `--group-by host,paths,tags` alone lists every record (each its own group); with `--tag motel` it shows the newest backup and the newest `pre-restore` apart. forget and prune: **exit 11 after 1 s** ("repository is already locked"); `forget --retry-lock 5m`: waited 35 s, exit 0. Hidden `keys/`, right password: `Fatal: wrong password or no key found`, exit 12 |

rclone 1.60's help: `--b2-versions` "no file write operations are permitted"; `--b2-version-at` exists. restic's design reference: "Changing the master key can currently only be done using the `copy` command … or by making a completely new repository."

| Rank | # | Finding | Verdict | Fix |
|---|---|---|---|---|
| 1 | 1 | "`status`'s minute is always before the attack" — false for a silent attack (D22: the minute moved past it), a lying box, a clock ahead | **genuine** — a copy that carries the attack in | **to the owner** |
| 2 | 4 | the look every ≤ 90 days = the window: no time left to recover | **genuine** | **to the owner** |
| 3 | 13 | after an attack with root on the box, the repository password and master key are the attacker's; restic cannot change the master key without a new repository | **genuine** | **to the owner** |
| 4 | 3 | RC check 4 on B2: `--b2-versions` refuses writes locally, so nothing is tried | **genuine** (rclone's help) | `rclone delete --b2-hard-delete` with the box's key — a real destroy attempt |
| 5 | 2 | `b2,version_at=…` wrong; the remote is `store` | **genuine** in part (1.60 has `--b2-version-at`) | the same `store,version_at='…'` form |
| 6 | 5 | recovery only for AWS (no endpoint) | **genuine** | `PROVIDER=Other` + `ENDPOINT`, `--endpoint-url` for others |
| 7 | 6 | RC check 1's successful setup leaves the box on an unversioned bucket for checks 2+ | **genuine** | check 1 on a bucket of its own; checks 2+ on the guide's |
| 8 | 10 | forget/prune exit 11 at once beside a box run | **genuine** (D23) | `--retry-lock 1h` |
| 9 | 11 | the look's `--latest 1` shows a `pre-restore` snapshot after a restore | **genuine** | `--tag motel --group-by host,paths,tags` (D23) |
| 10 | 8 | exit 12 is also hidden `keys/` | **genuine** (D23) | say so |
| 11 | 9 | "nothing on the box removes anything" vs the keys must delete | **genuine** (wording) | "nothing on the box forgets, prunes or repairs" |
| 12 | 14 | RC check 3 counts locks while the box's timer runs | **genuine** | stop the box's timers for the probe |
| 13 | 12 | release.yml's README.txt points at the README | **genuine** (second reviewer to raise it) | name the guide too |
| 14 | 15 | the credential lines twice (look, retention) | **genuine** (cleanup) | retention refers to the look's |
| 15 | 7 | altitude: the two `forget`s work around the engine's tags | **answered**: the owner chose docs over the engine change (F2); the reviewer's idea (no per-record `vouches:` tag) noted for the owner | — |

**The owner's answers (2026-10-03):** 1 — **the copy's `check --read-data` decides**; `status`'s minute only where the attack stopped the runs; the bucket's timeline where the look's check or the drill found it, or the box is not trusted; never "always". 4 — **old versions kept 180 days, the look at least every 90**. 3 — **say it only** (the password and master key are the attacker's after root on the box; restic changes a master key only by `copy` or a new repository). The drill's message — **issue #170** opened. The rest fixed as ranked.

| D24 | the look and the retention block extracted from the page, at a tty, own machine's key | `runs/D24-look-retention-as-written.out` | on box2's repository (D22's overwritten pack still there): the listing normal, **`check --read-data`: `Fatal: repository contains errors`, exit 1** — the look catches the silent attack; `prune` refused it (`pack size does not match calculated size from index`). On a clean copy: retention exit 0, `--retry-lock 1h` accepted |
| D25 | the recovery block in its other-provider form (`PROVIDER=Other`, an endpoint), at a tty in Berlin | `runs/D25-recovery-other-provider.out` | copy exit 0; newest backup `88b3b052` and the `pre-restore` apart; `no errors were found` |

## 12. Where it stands

`9d51949` pushed; every lane green; **Copilot at `9d51949`: no findings** (overview only: the guidance "depends on several explicitly unverified real-provider behaviors" — the RC's to settle). **CI 17 of 17 green**, automerge skipped. Open: RC check 4's S3 line on AWS (deferred by the owner); #169 (rotation, deferred); #170 (the drill's fetch message); the threat-model gate before `backup` merges into `main`. Never merged here: the owner's.

**Merged by the owner (2026-10-03 12:23 UTC) as `957a765`.** Nothing left only in the PR description (the owner's ask): #171 (S3 check 4), #172 (the threat-model gate), #173 (the single-`forget` idea), with #169 and #170; listed in a comment on #168. Kept only in these untracked plans by the owner's rule: the measurements D0–D25 and their outputs (the docs carry the results that matter).
