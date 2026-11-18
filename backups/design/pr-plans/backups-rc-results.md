# RC-results PR — what v0.3.0-rc1 on real B2 found, brought into the docs

Branch `backup-rc-results` from `backup` at `957a765` (= `v0.3.0-rc1`; start checks passed 2026-10-05). PR into `backup`.
Never committed: `PLAN-backups.md`, `HANDOVER-backups.md`, `helpful/`, `.claude/`.
Raw outputs of this PR's own runs: `rc-runs/rc-results/` (copied from the session scratchpad); the RC's: `rc-runs/RC-*.out`.

## 1. Procedures × box × output

Fresh boxes: the install-test image (Debian 13.6, systemd booted, root mount shared), the published `moteld_0.3.0.rc1_arm64.deb` (sha256 `dc945e49…`, as `checksums.txt`), the e2e S3 server over TLS from a private CA, as `bob` (not root; `sudo` is root's) at a real terminal (`as-bob.sh`). Off-box: a `debian:13` container.

| # | Procedure | Box | As | Output | Result |
|---|---|---|---|---|---|
| L1 | first-deploy step 2 on an image with `APT::Install-Recommends "false"` | box1 | root | `L1-install-no-recommends.out` | apt lists restic, sqlite3 as "Recommended packages", installs neither; postinstall's Backups lines print as usual |
| L2 | `sudo motel-backup setup` with neither program; `status`; a start; then `sudo apt install restic sqlite3` | box1 | bob | `L2-missing-programs.out` | `motel-backup: restic is not installed at /usr/bin/restic: apt install restic`, exit 1, before any prompt (`engine/setup.go:271-277`); status exit 3 "not set up"; the service skipped (`ConditionResult=no`); the apt line brings both (24.8 MB) |
| L3 | **setup before any `backup` block**; `status`; a run | box1 | bob | `L3-setup-before-declaration.out` | `no app declares a backup yet: a run would back nothing up` … `repository ready … (new, id cff1d334)` … `next: declare a backup in an app's Caddyfile block (liveswap/README.md); the hourly timer backs it up from then on, or sudo systemctl start motel-backup.service runs one now` (`main.go:274,284`); status `no run yet: pending first run`, exit 0; after a run `no app declares a backup; nothing is backed up`, exit 0 |
| L4 | then the block edited into `/etc/motel/Caddyfile` on the box, `moteld validate`, `motel-backup validate`, `sudo systemctl reload moteld`, a run, `status`, `restore --to`, `restore` into place, `drill`, `status` | box1 | bob | `L4-declare-run-restore.out` | all as the guide's lines say; run 4.6 s `ok`, proven by the run; `backed up first: snapshot f638fb14`; drill `41/52: clean` |
| L5 | a rebuilt box with no box repo: `sudo tar -czf … -C /etc motel` on box1; on box2 (`motel` uid 993, box1's 996) `sudo tar -xzf … -C /etc`, both validates, reload, `setup` (existing), `restore demo`, `ls -ln`, a run, `status` | box1, box2 | bob | `L5-rebuilt-box-no-box-repo.out` | as written; `existing, id cff1d334`; files owned 993; run `ok`, proven |
| L6 | the off-box machine as a `debian:13` container: `apt-get install restic` alone, then with `ca-certificates`; Debian's `backblaze-b2` and `rclone` | laptop | root | `L6-offbox-container.out` | without: `/etc/ssl/certs` empty, `tls: failed to verify certificate: x509: certificate signed by unknown authority`, retried; with: answered. `backblaze-b2` 3.19.1 (no `b2` on PATH), `create-key [--bucket B] keyName [capabilities]`, `authorize-account` prompts, `ls [bucketName] [folderName]`; rclone 1.60.1. apt: `backblaze-b2` brings `ca-certificates`; `restic` and `rclone` do not |
| L7 | RC check 3's probe: as #168 wrote it, and with `read -rs RESTIC_PASSWORD && export RESTIC_PASSWORD`, the box's timers stopped first | laptop, box1 | root, bob | `L7-check3-probe.out` | as written: `Fatal: cannot read both password and data from stdin`, exit 1. Corrected, 18:06:08–18:36:09 UTC: `processed 1 files, 6 B in 30:00`, `snapshot cbd455d6 saved`, exit 0, 0 locks |
| L8 | the B2 timeline line's `sort -k3,4` on lines as `backblaze-b2 ls --long --versions` prints them (`RC-lifecycle-day0.out`) | Mac | — | inline | sorts by the date and time columns |

Needs real B2 (the owner's, or a key file by `--env-file`): the key-making recipe as the guide will print it; the timeline line with its sort as printed (bucket only); see §3.

## 2. What the RC found × where it goes

| Finding (plan, "Release candidate `v0.3.0-rc1` cut" on) | Evidence | Docs change |
|---|---|---|
| Hetzner's Debian 13 sets Install-Recommends false | the owner's box; L1, L2 | guide "Before you start"; README "On a fresh box"; D8 to the owner |
| a `debian:13` container has no CA certificates | RC step 5; L6 | the look, the recovery and the RC page's off-box machine install `restic ca-certificates` (rclone likewise) |
| B2 keys: no command given; the console has three presets | the owner's run (keys made with `create-key`); L6 for the forms | the exact commands; why the presets will not do; a new master key after |
| check 3's probe fails for a typed password | RC check 3; D27; L7 | `read -rs RESTIC_PASSWORD && export RESTIC_PASSWORD`, the box's key |
| B2 lists versions by name; `ls` with a folder in the URI fails; rclone lists no version of a hidden file | RC check 4, lifecycle day 1 (0 lines vs 179) | `\| sort -k3,4`; "never rclone's listing" *(2026-10-24, PR #356: the guide sorts by the millisecond upload stamp in each file id, `sort -t_ -k9,9`, measured as `rc-runs/rc-results/L10-timeline-sort.out`; the rclone line is cited to this record, no capture)* |
| "A rebuilt box" assumes a box repo; backups never hold `/etc/motel` | the owner; L5 | what to keep off the box, and putting it back without `make push` *(reversed by the owner, 2026-10-24, PR #356: the guide says what a backup does not hold and that the Caddyfile and secrets come back from the box repo; no recipe for a box without one)* |
| the block before the credentials read odd | the owner at 4e; L3, L4 | "Set it up": setup first, then the declaration; the smoke's seven lines in that order *(2026-10-24, PR #356: the order was kept — the smoke block's lines are the box channel's since, and the guide says in one sentence that setup first works too)* |
| a read-only key: setup accepts; runs fail after ~14½ min, drills ~14 min | `RC-check6.out` | measured on B2, in place of "traced, not run" |
| measured on B2, "unverified" comes off | checks 1, 2, 3, 4, 6, 7, 8 (timing), §7.4 | guide intro, bucket section, After an attack, the last section; RC page per check |
| still open: check 5, check 8's egress, the 1-day lifecycle read (2026-10-06) | another session writes them into the plan | folded in before review; else ask |

## 3. Decided without asking (one line each)

- "Set it up" becomes setup first, then the declaration: both orders measured (L3–L4; #168's P3), and the owner read the other as odd. *(2026-10-24, PR #356: not done — the order stayed as the box channel wrote it; one sentence says setup first works too.)*
- The guide's opening says B2 was measured on `v0.3.0-rc1` and S3 and Hetzner stay **unverified**; each claim names which.
- `/etc/motel` kept off the box as a `tar` of the directory (L5), said to hold the apps' secrets. *(Reversed by the owner, 2026-10-24, PR #356: the guide carries no recipe for a box without a box repo — main's first deploy makes the repo and the rebuilt-box recipe presumes it; L5 and the later capture `rc-runs/rc-results/L11-tar-exclude-umask.out` stay as records.)*
- Not this PR: #169–#176; any printed output; `nfpm.yaml` unless the owner says so (D8).

## 4. The owner's answers (2026-10-05)

- **D8: Depends, in this PR** — `restic`, `sqlite3`, `ca-certificates` (`packaging/nfpm.yaml`); the smoke installs `--no-install-recommends`, as Hetzner's image does, and holds the `.deb` to the three.
- **A read-only key: fixed in this PR** — `setup` writes to a repository that is there already before it takes the key.
- **Severity bar: every genuine finding.**
- **Real B2: the owner runs both** (the key-making recipe, the timeline with its sort) and pastes the output. Lines given 2026-10-05; *(output pending)*.

## 4a. The write check — measured, then built (failing tests first)

| # | What | Output | Result |
|---|---|---|---|
| M1 | restic 0.18 against a storage that reads and refuses writes (`rclone serve s3 --read-only`) | `M1-readonly-storage.out` | `cat config --no-lock` 0.6 s exit 0; `cat config` and `list keys`: `Save(<lock/…>) returned error, retrying after …` until stopped. (That server leaves empty lock files: not a fixture) |
| M2 | the locking `cat config` on a writable repository; with a non-exclusive lock held; with an exclusive lock held, live and stale | `M2-locking-read.out` | 0.7 s exit 0, no lock left; beside a backup's lock 0.7 s exit 0; exclusive: **exit 11 in 0.5 s** both times |
| M3 | motel itself as a proxy: reads passed, `PUT POST DELETE` → 403 | `M3-readonly-proxy.out` | `--no-lock` exit 0; locking: **exit 1 in 0.6 s**, `Save(<lock/…>) failed: client.PutObject: Access Denied.` (restic does not retry a 403); with 500 it retries (the e2e's second case) |
| RC 6 | B2, a console Read Only key | `RC-check6.out` | `Save(<lock/…>) returned error, retrying …: b2_get_upload_url: 401:` for 868 s |
| E1 | the e2e setup suite's new checks against the unfixed `setup` | `E1-setup-suite-before-fix.log` | **fail as they must**: `exit 0 after 1s`, "setup said the repository was ready", the working file replaced; the suite then wedged on the read-only storage to its 600 s limit |
| E2 | the same with the check | `E2-setup-suite-after-fix.log` | all pass, 218 s: refused at once on 403; at the clock (31 s in the e2e binary) on 500; working file byte-identical, no unit, no lock left |

Design, decided without asking (no new limit beyond the refusal the owner chose):
- A unit of its own, `write`, after the opening and before anything is written: `restic cat config` (no `--no-lock`), same account, same staged file.
- **Its clock is the opening's (`setupClock`, 2 min): no new time limit.** A read-only B2 key therefore waits two minutes once, with the 20 s note.
- **Exit 11 (another restic holds the repository) is not a refusal**: said, and setup goes on — it must not refuse a rebuilt box during an off-box prune.
- A repository this setup made is not asked (init wrote it).
- Pins: `TestIntegrationResticCatConfigWritesALockFileUnlessNoLock`, and exit 11 at once in `TestIntegrationALockHeldForTheWholeRetryIsExit11`. Unit: the invariant table `TestAKeyThatCannotWriteAnExistingRepositoryIsRefusedBeforeAnythingIsWritten`.

## 5. Claims × what holds them

*(filled as each is written)*

## 6. Lanes, review

*(to be filled)*
