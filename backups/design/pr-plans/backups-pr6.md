# PR 6 — cutting upkeep

Branch `backup-upkeep` from `origin/backup` at `4bb4e37` (checked first:
#161 `MERGED`, #154 `CLOSED`; `main` none ahead of `backup`, `backup` 12
ahead).

**The owner's answers, before anything was built (2026-10-01):** the
review loop fixes **every genuine finding** · **item 1, all of it**: the
nine new pins and the two tightened · **item 2: the lanes on a new image,
and a short written procedure, committed here,** for what no lane holds ·
**item 3: `e2e-backup`'s cache key only** · **item 4: one short id and
UTC everywhere** (the PR body lists every changed line), and
`makeAccount` with setup's context · **item 5: a report, nothing built**.

**Decided by me, said to the owner in one line each:** a fact that
decides what the code does is pinned, one that only picks words is
listed · `makeAccount` gets setup's context and no clock (a clock is a
new limit; setup is attended) · the lock holder's text, the aside's name
and restic's `--time` are left as they are · no suite split (no flake
in ~40 runs; seven boxes on four CPUs unmeasured) · item 6 waits for
real use · the two `restic init` items stay the owner's.

---

## 1. The pins table

Every third-party fact the code leans on (§0's rows, the handover's §3
entries the code relies on) × the test that fails if a bump changes it.
"Integration" is `make test-integration` (`golang:1.27-trixie`, Debian's
restic and sqlite3). "e2e/smoke" is `make e2e-backup`
(`debian:trixie-slim`) or `make install-test` (`DISTRO ?= debian:13`): a
bump there fails a whole suite, not a named test.

**Rule (mine):** a fact that decides what the code *does* — a class, a
health verdict, whether a run goes on, what data is kept — gets an
integration pin. A fact that only chooses the words is listed, not
pinned.

### restic 0.18

| # | Fact | Where the code leans on it | Integration | e2e / smoke | Verdict |
|---|---|---|---|---|---|
| R1 | `backup --quiet --json`: one line, `message_type: summary`, `snapshot_id` [M12] | `upload`, `vouch` (`summaryID`) | `TestIntegrationACleanRunRecordIsASnapshotOfItsOwn` (summaryID on real output) | every run | held |
| R2 | `backup` that exits 3 still prints a summary with its snapshot's id [H §3.2] | `upload`: `incomplete` with the id, recorded | **none** | none (the suites' `incomplete` is the engine's own) | **pin** |
| R3 | `--stdin-from-command` record: its own group, `--time` honoured, one of nothing exits 3 [M76] | `vouchArgv` | `ACleanRunRecordIsASnapshotOfItsOwn` | check 1, 7 | held |
| R4 | A snapshot's time is stored in the zone restic runs in [M81] | `resticUnit` `TZ=UTC` | `AResticUnitStoresItsTimesInUTC` | check (Berlin box) | held |
| R5 | Exit 10 no repository, 12 wrong password [H, M40] | `resticFailure`, `checkClass`, setup's look | `ResticInitSaysWhenTheRepositoryExists` (cat config), `TheCheckFindsARottenPackOnlyInItsGroup` (check 10) | backup 8, 9; check 4 | held |
| R6 | Exit 1 is anything else; `check` exits 1 for damage and for a storage it cannot reach [H, M73] | the probe after a check | `TheCheckFindsARottenPack…` (damage) | check 3; check 5 (unreachable) | held |
| R7 | Exit 11: a lock met for the whole `--retry-lock` [H, M75] | `resticFailure`: repository-wide, so the run stops asking; `vouch`'s words | **none** | none | **pin** — renumbered, a run would wait `retryLock` (2 h) once per app instead of stopping |
| R8 | `--stuck-request-timeout` defaults to 5 min; a wrong key ends by itself after 864 s [M10, M15] | D4: upload, fetch and the check have no backstop because restic bounds its own waiting | **none** | none | **pin** the default (from `--help`, text to be measured first); the 864 s ending → §2 |
| R9 | `ls <id> <dir>` lists direct children only [H] | `verify`: one listing of every parent | `ResticLsOfADirectoryIsNotRecursive` | — | held |
| R10 | `ls --json` node: `inode`, `uid`, `gid` through a bind; the snapshot line's `time` [M63] | `verify` holds a files item to the pin; the record's `--time` | `ResticLsSaysWhichFileANodeIs` (through `lsNodes`) | every run | held |
| R11 | `ls --json` node: `type` (`file`/`dir`) and `size` | `verify`: a sqlite item is a `file` of more than 0 bytes | **not asserted** (the same test reads them, checks neither) | every run | **tighten** R10's test |
| R12 | `snapshots --json`: `id`, `time`, `tags`; `--host`/`--tag` filter [H] | `snapshots`, `history`, the records | `ACleanRunRecord…`, `AResticUnitStoresItsTimesInUTC` (structs mirrored) | status 4, check 7 | held |
| R13 | `snapshots` leaves out a snapshot it cannot load, exits 0, says so on stderr alone [measured] | `besides`: anything on stderr → the listing is not believed | `ResticSnapshotsLeavesOutWhatItCannotLoadAndExitsZero` | — | held; its `Ignoring "<id>"` wording picks only the message (`ignoringRe`) — **tighten** the test to that regex (one line) |
| R14 | `restore <id>:<absent>` exits 1; `--include` of nothing exits 0; the summary counts every entry; a zero field is left out [M7, M25] | `fetch`, `fetchedAll` | `ResticRestoreSaysWhatItRestored` | restore 0 | held |
| R15 | Restore as an unprivileged uid: `lchown` EPERM overlooked, exit 0, every entry counted, files the restorer's [M6] | every fetch (restore, drill) runs as `motel-backup` with no `CAP_CHOWN` | **none** | restore 8b, 15, every drill | **pin** — the Debian-14 risk (restic 0.19 reworks ownership) |
| R16 | `stats --mode restore-size --json`: `total_size`, `snapshots_count`; an absent id exits 0 with 0 snapshots [M33] | `snapshotSize` → `room` before every fetch | **none** | restore 13b, 13c | **pin** |
| R17 | `cat config --no-lock`: 0 and the config's `id`, 10, 12; `init --json` one line with the id; init on an existing repository exits 1 in one of two wordings [M39, M40, M47] | setup's look and init, the probe | `ResticInitSaysWhenTheRepositoryExists` (local wording) | setup 3, 7, 9 (S3 wording) | held |
| R18 | `check --json`: `num_errors`, `broken_packs`; exit 10 with an error counted; the group is the pack id's first byte mod 52, plus one [M73, M74] | `checkRepository` | `TheCheckFindsARottenPackOnlyInItsGroup` | check 2, 3 | held |
| R19 | `check --no-lock` writes no lock [M75] | the check never blocks a backup | `TheCheckWritesNoLock` | — | held |
| R20 | `--cleanup-cache` on `cat config` removes a month-old check cache [M82] | the probe | none | none | housekeeping only (≤ 47 MiB per hard kill): listed |
| R21 | Exclude file: `\` escapes `* ? [ \`, `$VAR` is expanded, `$$` is `$` [M24] | `excludeFile`: a declared database inside a `files` path is left out, its look-alikes kept | **none** | restore 7 (a plain name only) | **pin** — wrong, a look-alike file is silently not backed up |
| R22 | A symlink is stored as a link, a FIFO as a node [H, M5] | restore 8 | none | restore 8 | held (e2e) |
| R23 | `forget` groups by (host, paths) [H] | paths `/backup/<app>`, `/motel-clean-<app>` | none | backup 2; check's retention | held (e2e); the box never runs `forget` |
| R24 | A file that vanishes mid-backup: exit 0, nothing said [M11] | not leaned on: `verify` looks regardless | — | — | n/a |

### sqlite3 3.46

| # | Fact | Where | Integration | Verdict |
|---|---|---|---|---|
| S1 | A plain open of a FIFO blocks; `ATTACH 'file:…?mode=rw'` fails at once, but a mode-0400 FIFO still blocks [M1, M18, M19] | `dump.run`, `RestoreOver`, `openWithin` | `WhatIsSwappedInAfterTheChecksFailsAtOnce`, `AReadOnlyFifoIsKilledAtTheOpenBound`, `RestoreOverAReadOnlyFIFOIsKilledAtTheBound` | held |
| S2 | A lock is waited for before the copy's target exists [M20] | `openWithin ≥ 2 × busyTimeout` | `ALockedDatabaseIsWaitedForBeforeTheTargetExists` | held |
| S3 | `VACUUM INTO` of a live WAL database under a writer: committed rows, no sidecars [H, M27] | `dump.one` | `DumpsALiveWALDatabase` | held |
| S4 | `VACUUM INTO` refuses a target that exists [H] | leftovers removed first | `ASecondRunReplacesTheCopy` | held |
| S5 | `integrity_check`: `ok` and exit 0; damage in words with exit 0, or "malformed" with exit 1 [H] | only exactly `ok\n` and 0 is sound | `ADamagedDatabaseIsNotAnOKCopy`; restore's `ADamagedCopyIsNeverRestored` (both shapes) | held |
| S6 | The exit status is SQLite's result code: 5 busy | `classify` | `ALockedDatabaseIsBusyByExitStatus` | held |
| S7 | … 13 full | `classify` → `disk full` | none | words only (`disk full` or `failed`, both an item not ok): listed |
| S8 | `.restore`: under a writer, over damaged pages; a lock it meets is exit 1, "Error: source database is busy" [M27, M28, M32] | `RestoreOver` | restore's `ALiveDatabaseIsRestoredOverUnderAWriter`, `…WithDamagedPagesIsRestoredOver`; dump's `RestoreOverALockedDatabaseIsBusy` | held |
| S9 | `-init /dev/null` and `HOME` keep `.sqliterc` out [H] | `execute` | `APlantedSqlitercIsNotRun` | held |
| S10 | `mode=rw` never creates [M18] | dump, restore | `WhatIsSwappedIn…` ("nothing") | held |
| S11 | Past the unix VFS's 512 bytes, `ATTACH` opens an empty temporary database [M36]; the code refuses past 480 | `pathLimit` | **none** | **pin** — a database at a 480-byte path is dumped as itself (a later SQLite with a lower limit would make it a dump of nothing that checks out) |

### systemd 257

| # | Fact | Integration | Verdict |
|---|---|---|---|
| Y1 | `ExecStartEx` + `no-env-expand`; typed binds never split or expanded [M9] | `ArgvIsNeverExpanded`, `BindSourceIsAPathAndNothingElse` | held |
| Y2 | The view: `TemporaryFileSystem=/:ro` + what is named, for every way a Spec says who runs [M16, M17] | `ViewIsWhatIsNamed` | held |
| Y3 | `PrivateNetwork=`: no routes [M17] | `NoNetworkUnlessAsked` | held |
| Y4 | `InaccessiblePaths=`: read empty with the capability, `-` skips a missing one [M14] | `MaskedPathsReadEmptyAndAMissingOneIsSkipped` | held |
| Y5 | Ambient `CAP_DAC_READ_SEARCH` reads, writes nothing [M3] | `CapabilityReadsAnotherUsersFileAndWritesNothing` | held |
| Y6 | `PrivateUsers=` + `PrivatePIDs=` [H, M16] | `SameUIDNamespaces` | held |
| Y7 | `SystemCallFilter=~open_by_handle_at` answers before the kernel where the capability is held [M34] | **none** (`TestOpenByHandleAtIsDeniedExactlyWhereTheCapabilityIsHeld` checks the property is sent, not that it bites; e2e: restic works under it) | **pin** |
| Y8 | `BindsTo=` ends a unit whose orchestrator is SIGKILLed [M8] | `BindsToEndsTheUnitWhenItsOrchestratorIsKilled` | held |
| Y9 | Job result, `Result`, `ExecMainStatus`; a oneshot is `activating` while it runs [H] | `ExitStatusIsTheOutcomeNotAnError`, `ListActiveSaysWhatIsRunningAndSinceWhen` | held |
| Y10 | A stop confirmed; the connection outlives the signal's context [M30] | `CancelStopsTheUnitAndConfirmsItGone`, `ARunnerOutlivesTheContextItWasMadeWith` | held |
| Y11 | `EnvironmentFile=` as PID 1 reads it [M41, M46] | `SystemdReadsAnEnvFileAsParseDoes` | held |
| Y12 | `StandardOutput/ErrorFileToTruncate` | `StderrReachesTheFileNamed` | held |
| Y13 | A mount unit for every mount of the manager's own namespace, none for another's [M72] | `SeesAMountOfTheManagersNamespace` | held |
| Y14 | `mount(2)` from `/proc/self/fd/<n>` binds what was opened [M22] | `WhatIsBoundIsWhatWasPinnedWhateverTheAppDoesToTheName` | held |
| Y15 | A shared root: a bind made private before it is taken away [M71] | `TakingABindAwayLeavesTheAppsDiskMounted`, `ABindThatCannotBeMadePrivateIsNotDetached` | held |
| Y16 | `RestrictSUIDSGID=` makes `openat2` ENOSYS [M23] | — | avoided by construction (`nofollow` uses `openat`) |
| Y17 | `CacheDirectory=` made and owned by `User=` | none | e2e, every restic unit; a failure is restic's "unable to open cache" on stderr, which a listing refuses: loud |
| Y18 | The shipped units: capabilities and hardening, `ConditionPathExists=`, `After=`, the timers [M54–M57, M60, M61] | none | e2e units 0–5, smoke |
| Y19 | `deb-systemd-helper`/`-invoke`, `policy-rc.d` [M58, M68, M69] | none | smoke |

### Caddy (this repository's `moteld adapt`), and the box's own programs

| # | Fact | Integration | Verdict |
|---|---|---|---|
| C1 | An import glob matching nothing: the JSON warning `No files matching import glob pattern` on stderr, exit 0 | `TheAdapterWarnsOfAnEmptyGlobAsTheRefusalReadsIt` | held |
| C2 | `{$NAME}` is substituted in the text before any token; unset with no default is an empty value; `{$NAME:default}` | **none** (e2e backup 5, 5b) | **pin** — in the same test, against the adapter it already builds: a root written `{$X}` is refused, naming X |
| C3 | The adapted JSON: `apps.liveswap.root`, `apps.liveswap.apps.*.backup` | liveswap's own schema | n/a (ours) |
| O1 | `getent` exit 2 for "not there"; `-s files` [M64, M65] | none | e2e setup 3, backup 0b; smoke |
| O2 | `useradd` line [M42, M66] | none | e2e setup 3; smoke (postinstall) |
| O3 | `chown -hR` [M26] | none | e2e restore |
| O4 | `/usr/bin/echo` (coreutils, Essential) | `ACleanRunRecord…` | held |

**New pins: 9** (R2, R7, R8, R15, R16, R21, S11, Y7, C2) and **two
tightened** (R11, R13). Each seen failing against a mutated expectation
before it is trusted.

### What no lane can hold (item 2)

M77, M78, M80 (timings at 28.5 GB, local and over a 25 ms / 50 Mbit/s
link), M15 (a wrong key ends after 864 s), M75 (a stale lock outlives
`--retry-lock 40m`), M39 (30 s on a host that swallows packets), M10's
5-minute stuck request as behaviour (R8 pins only the default).

## 2. Findings while scoping (measured)

- **e2e-backup on CI** (18 runs on #161's branch, all green): 233–323 s
  a job. Per suite, arm64, the last three: setup 181–184 s, restore
  174–217 s, check 124–150 s, units 117–152 s, backup 73–91 s, status
  74–92 s. One red in ~40 runs since #152 (2026-09-26, `6e4b9a0`, setup's
  "after Ctrl-C: init ''", an intermediate commit of #152), not seen
  since. #152's "slow unit stop": not seen again.
- **The Actions cache is full: 10.99 GB of GitHub's 10 GB, 19 entries,
  every one from `refs/pull/161/merge`** — eight of them `e2e-backup` at
  ~540 MB each (its key carries `github.sha`, so every push saves one per
  arch). No cache from `main` survives. CI runs on push to `main` and on
  PRs only, so no `backup`-scoped cache ever exists.
- **Times as printed**: every time the record holds is written in UTC,
  and since #161 every restic unit runs in UTC, so `main`'s
  `Format("2006-01-02 15:04 MST")` in the time's own zone and `status`'s
  `UTC().Format(…)` print the same for everything this release writes.
  The one line that differs: a run's `data missing` detail, in RFC3339.
- **Caddy coupling** (look and report): see §5.

### Measured for the pins (2026-10-01; restic 0.18.0, sqlite3 3.46.1, systemd 257.13; the integration image, arm64)

| # | Question | Result |
|---|---|---|
| R7 | A way to hold restic's exclusive lock that is not a race | An index file swapped for a FIFO: a locking `restic check` takes its lock and blocks opening the FIFO. `backup --retry-lock 2s` then: **exit 11 after 2.57 s**, "repository is already locked exclusively by PID …". SIGTERM does not end the blocked check; SIGKILL does. |
| R8 | How restic states its stuck-request default | `restic backup --help`: `--stuck-request-timeout duration   duration after which to retry stuck requests (default 5m0s)` |
| R15 | `restore` as uid 65534 of files owned by 4242:4243 | exit 0; `{"message_type":"summary","total_files":4,"files_restored":4,…}`; every entry owned 65534:65534 |
| R16 | `stats --quiet --json --no-lock --mode restore-size` | `{"total_size":4,"total_file_count":6,"snapshots_count":1}`; an absent id: `{"total_size":0,"snapshots_count":0}`, **exit 0**, `Ignoring "<id>": failed to load snapshot …` on stderr |
| R2 | `backup --quiet --json` as uid 65534 over a root-only 0600 file | an `error` line, `{"message_type":"exit_error","code":3,…}`, **then** the summary with `snapshot_id`; exit 3 |
| R21 | M24's five names through the exclude file | Patterns are matched against the path **on disk**: a relative target under a temp dir matches none of `/backup/…` — the pin builds its tree at a real `/backup/<app>`. There: escaped, exactly the five go, `appX.db`, `data/x.db`, `leaked.db`, `qZ.db` stay. Unescaped (contrast): `app*.db` takes `appX.db` with it; `$HOME_SECRET.db` expands and takes `leaked.db`, and keeps itself. |
| S11 / **M83** | sqlite3 and a path near the unix VFS's limit, in the dump's own shapes (**correction to M36**) | Up to **504 bytes** everything works. From **505** (504 + `-journal` = 512) `ATTACH 'file:…?mode=rw'` is **exit 14, "unable to open database"** — `integrity_check`, `VACUUM INTO`, `pragma_database_list` alike; the direct open exit 1. **M36's empty temporary database at 513 bytes is not reproduced**: today's sqlite3 fails loudly there. `pathLimit` (480) stands either way; `dump.go`'s comment is corrected. |
| Y7 | `open_by_handle_at` of a bogus handle, `O_RDONLY` mount fd | root outside a unit: **ESTALE (116)**; a unit, `CAP_DAC_READ_SEARCH` effective (`CapEff 0x4`), no filter: **ESTALE**; the same with `SystemCallFilter=~open_by_handle_at`, `SystemCallErrorNumber=EPERM`: **EPERM (1)**; a unit without the capability: EPERM (the kernel's). So EPERM with the capability held is the filter's alone. |

### Measured for item 3 (the dev container, arm64, 6 CPUs; e2e-backup's two builds as the Makefile runs them, a private `GOCACHE`)

| Build | Time |
|---|---|
| cold (empty build cache) | 34.5 s |
| warm, the same commit again | 0.4 s |
| HEAD from the cache `a594aea` left (all of PR 5 behind, `go.sum` the same) | **0.9 s** |
| HEAD from the cache `5dd5604` left (across a `main` merge: 26 files of liveswap/cmd, `go.sum` changed — a new key in any case) | 16.8 s |

Go's build cache is keyed by content: a cache from an older commit
recompiles only what changed. Keyed by `go.sum` alone, a PR saves one
`e2e-backup` cache per arch, at its first push, and later pushes pay
about half a second.

## 3. A failure row for every new call to another program

Product code gains no call to another program. **After the review
(§4c, the owner's (A)), none changes either**: useradd runs as before,
to its end, and its comment says why. The row below was the first
build's, kept for the record:

| Call | Fails how | What is done |
|---|---|---|
| `useradd …` (setup's `makeAccount`), now with setup's context | exits non-zero | as today: "making the motel-backup account: …" with useradd's words |
| | the context ends (Ctrl-C, SIGTERM to setup) | **interrupted** (SIGINT, what Ctrl-C at the terminal already sends it — useradd writes the account databases, and what it does about an interrupt is its own), killed only if still there 10 s on (`WaitDelay`); setup ends with the context's error — the next setup looks first, as today. A useradd ended half way can leave its group made and its account not (M66: useradd then exits 9, in its own words) — as Ctrl-C at the terminal already could |

## 4. Tests first — each seen failing, the message written down

### The pins (item 1)

A pin holds behaviour that is already there, so it passes at once
against today's programs. Each was then run against a mutation — of its
expectation, or of the code it guards — and seen failing for its reason;
the files were put back byte for byte (copies in the scratchpad).

| Pin | Test | Mutation | Seen failing with |
|---|---|---|---|
| R2 | `TestIntegrationResticNamesTheSnapshotOfAnIncompleteBackup` | expect exit 0 | `a backup that could not read s.txt: exit 3, snapshot "5f422bce…", want exit 3 and the snapshot it made: {"message_type":"summary",…` |
| R7 | `TestIntegrationALockHeldForTheWholeRetryIsExit11` | expect exit 12 | `… exit 11 after 2.522s, want 11 after 2s: {"message_type":"exit_error","code":11,"message":"unable to create lock in backend: repository is already locked exclusively by PID 1691 …` |
| R8 | `TestIntegrationResticRetriesAStuckRequestByItself` | expect `10m0s` | `restic backup --help (exit 0) does not give --stuck-request-timeout a default of 5m0s:` |
| R11 | `TestIntegrationResticLsSaysWhichFileANodeIs` (tightened) | expect `"directory"`, and 4 bytes | `/backup/blog/files/uploads: the listing says a "dir" of 0 bytes, want a "directory" of 0` · `…uploads-a.png: the listing says a "file" of 3 bytes, want a "file" of 4` |
| R13 | `TestIntegrationResticSnapshotsLeavesOutWhatItCannotLoadAndExitsZero` (tightened: `ignoringRe`) | expect the other snapshot's id | `stderr: "Ignoring \"1f67fde0…\": failed to load snapshot 1f67fde0: LoadRaw(<snapshot/1f67fde0ad>): invalid data returned\n"` |
| R15 | `TestIntegrationResticRestoresAsAnUnprivilegedUserWhateverTheOwners` | expect the snapshot's owners, 4242:4243 | `…/restore/files/uploads/a.png is owned 65534:65534, not by the user who restored it` (every entry) |
| R16 | `TestIntegrationResticSaysHowLargeASnapshotIsOnceRestored` | expect 1000 · `restoreSize` taking a count of 0 | `a snapshot of 1024 bytes: 1024, <nil>` · `an id the repository does not hold was taken for an answer: 0` |
| R21 | `TestIntegrationTheExcludeFileLeavesOutTheDeclaredDatabasesAndNothingElse` | **the code**: `excludeEscaper` made a no-op | `da[t]a/x.db is in the snapshot …` · `$HOME_SECRET.db is in the snapshot …` · `back\slash.db …` · `appX.db is not in the snapshot: the exclude file left out what only looks like a declared database` · `appX.db-wal`, `data/x.db`, `leaked.db`, `qZ.db` the same |
| S11 | `TestIntegrationADatabaseAtThePathLimitIsDumpedAsItself` | **the code**: `pathLimit` 520, the database at 505 bytes | `a database at 505 bytes: {… Class:failed Detail:Error: stepping, unable to open database: file:<505 bytes>…}` (first run: the fixture's own `sqlite3 <path>` could not make it there — now made elsewhere and renamed in) |
| Y7 | `TestIntegrationTheFilterDeniesOpenByHandleAtWhereTheCapabilityIsHeld` | expect ESTALE, the kernel's | `open_by_handle_at in a unit that holds CAP_DAC_READ_SEARCH: operation not permitted, want EPERM, the filter's` |
| C2 | `TestIntegrationTheAdapterSubstitutesVariablesAsMakeReadsThem` | expect `SITE_DOMAIN` named | `Make of a root written {$LIVESWAP_ROOT:…}: the liveswap root or a backup path depends on the environment variable(s) LIVESWAP_ROOT through the Caddyfile's {$NAME}; …` |

Product code moved for the pins: `restoreSize` out of `snapshotSize`
(the pin reads restic's answer as the engine does); `dump.go`'s and
`backupdecl.go`'s comments on the path limit corrected to M83.

### Item 4 — the consolidations

`record.Short(id)` (eight characters, through `Clean`) and
`record.When(t)` (UTC, to the minute, `status`'s own form) take the place
of the engine's `short`, `status`'s closures and constant, and every
`%.8s` and `Format("2006-01-02 15:04 MST")` in `main.go`; the drill's
lines moved, unchanged, into `reportDrill` beside `reportRestore`, so a
test can read them. `makeAccount` takes setup's context, no clock.

| Test | Seen failing with |
|---|---|
| `TestASnapshotAndATimeArePrintedOneWay` (record; against stubs) | `Short("5f422bce…") = "", want "5f422bce"` · `When(2026-10-01 14:34:56 +0200 +0200) = "", want 2026-10-01 12:34 UTC` (a third row of mine, a time in `time.Local` built backwards, was wrong — `16:34` — and taken out: the `+02:00` row is the case) |
| `TestEveryCommandPrintsASnapshotAndATimeOneWay` (main; stdout read through a pipe; four rows: a run's report, a restore's question, a restore's report, a drill's report) | `a run's report … blog: not run (last ok 2026-10-01 14:34 +0200, snapshot 5f422bce)` · `a restore's question … made 2026-10-01 14:34 +0200` · `a restore's report … restored from snapshot 5f422bce of 2026-10-01 14:34 +0200` · `a drill's report … (last proven: snapshot 5f422bce, on 2026-10-01 14:34 +0200)` |
| `TestWithNoRecordTheRepositoryIsAskedWhetherTheAppWasEverBackedUp`, its first row (the newest snapshot stored at `+02:00`) | `… this app was last backed up on 2026-09-18T12:00:00+02:00 (snapshot 22222222) …` |
| `TestMakingTheAccountEndsWithItsCommand` (account invariants, beside the lookups' bound; against the signature taking the context and not using it) | `a useradd whose command was stopped: err = <nil>` after 5.02 s |

**Printed lines that change**, for the PR body: on a box running this
release, one — a run's `data missing` detail:
`… was last backed up on 2026-09-18T10:00:00Z (snapshot 22222222) …` →
`… was last backed up on 2026-09-18 10:00 UTC (snapshot 22222222) …`.
`main`'s times print in UTC where a time was read in another zone — a
snapshot stored by a restic run in the box's own zone (by hand); every
time this release writes is UTC already. Left as they are: the run
lock's holder text (RFC3339, written into the lock file; the smoke
writes the same), the record's aside name, restic's `--time`.

### Item 2 — the procedure, run as written

The README's "On a new Debian release" commands, run verbatim but for
the sizes (5G → 64M, 1,000 files → 20, 500 runs → 10) and a `timeout
60` on the wrong-key line: the fixture image tagged `hsb-s3`, run in the
client's network namespace on a volume; over `tc … netem delay 25ms rate
50mbit` on `lo`, cold — `snapshots` 1.46 s, `ls` 1.85 s, `stats` 1.01 s,
`check` of a group 2.76 s; the wrong key retrying "signature … does not
match", ended by the timeout at 60 s; the cleanup lines leave nothing.
The wrong key, the whole way, in the first topology (two containers on
the compose network): **exit 1 after 900 s**, "Is there a repository at
the following location?" — M15 had 864 s; the README says both. A first
draft named the compose network and image by this checkout's folder
(`motel-backups-v2_default`), which another checkout does not have:
the image is tagged, and the fixture shares the client's namespace.

### Item 3 — the cache key

`e2e-backup`'s key without `github.sha`; restore-keys the prefix. The
comment carries the measurement. Checked on CI after the push: the
cache listing, and the compile step on a second push.

## 4b. Lanes

At `06294d3` (three commits: `94a77f6` the pins · `4671f53` the
consolidations · `06294d3` the cache key and the procedure), one after
another, fresh containers: `make test` 0 · `make lint` 0 (and, by hand,
`golangci-lint --build-tags integration`: my two findings fixed — a map
literal's alignment, an unused `//nolint:gosec`; three older ones in
files this PR does not touch) · `make secretscan` 0, 264 commits, no
leaks · `make test-integration` 0, 229 s, 89 passed, none skipped, the
eleven pins among them (they add some 34 s) · `make package` 0 ·
`make install-test` 0, 160 s · `make e2e-backup` 0, 215 s, six suites ·
`make e2e` 0, 180 s. Each commit's tree built on its own (the first
checked with `git archive` of its index).

## 4c. `/code-review xhigh` at `06294d3` (2026-10-01): twelve findings, ranked before any work

Each checked against the code; one measured (useradd's order). The
owner's answer, on the one that went back on an approved option:
**(A) useradd runs to its end** — no context, as before this PR.

| Rank | Finding | Verdict | Done | Seen failing with |
|---|---|---|---|---|
| 1 | `makeAccount` interrupts useradd when setup's context ends | genuine; the scenario corrected — **measured: useradd renames passwd, shadow, group, gshadow in that order**, so a stop part way leaves the account without its group (not a group with no account). Before this PR a SIGTERM to setup alone let it finish | (A): no context; the comment says why; `TestMakingTheAccountRunsToItsEnd` (setup's context ended 100 ms into a 1 s useradd stand-in, through `Setup`) | `setup's context, ended, stopped useradd part way: stat …/made: no such file or directory` |
| 2 | the ctx branch says "context canceled" of a useradd that finished | genuine while the context stayed | gone with 1 | — |
| 3 | `e2e-backup`'s key on go.sum alone is never saved again after a Go change; the dev image `golang:1.27-trixie` follows patch releases | genuine | the dev image's `go env GOVERSION` (a step before the cache, base compose file, no `.cache` mounts) in the key; restore-keys: the same Go first, any after (the module cache is any Go's) | CI only: checked after the push |
| 4 | `WaitDelay` on success: a helper holding the pipe makes a made account a failure | genuine while the context stayed | gone with 1 | — |
| 5 | nothing pins that no restic argv sets `--stuck-request-timeout` | genuine | a row in `TestWhatEveryResticUnitIsGiven` (was `…RunsInUTC`): every restic unit of a run, a drill, a restore | `--stuck-request-timeout 0` in the upload's argv: `motel_backup_upload_blog_….service sets restic's own bound on a stuck request` |
| 6 | `validate`'s refusal still said sqlite3 "opens nothing … and does not say so" | genuine | "sqlite3 does not open a path past its own limit as itself", there and in liveswap's README; `TestValidateRejects`'s row holds the reason | `want error containing "at most: sqlite3 does not open a path past its own limit as itself", got …` |
| 7 | `reportDrill` (and the run's `report`) print record fields uncleaned, where status cleans | genuine by the code's own rule (each field is `record.Text`ed where it is made: no path known) | `say` — a line of a record's words through `record.Clean` — in both; `TestNothingOfARecordReachesTheTerminalUnlookedAt` | `a run's report printed a control character ('\x1b')` · `a drill's report printed a control character ('\x1b')` |
| 8 | the exclude pin removes a `/backup` it did not make | genuine (test) | removed only where the test made it | test only |
| 9 | `printed` leaks its read ends; process-wide stdout | genuine (test) | every end closed in `t.Cleanup`, the reader's channel buffered, the comment says no test here runs in parallel; no `io.Writer` refactor (`confirm` reads stdin itself) | test only |
| 10 | four older restic tests build their own copy of `resticIn`'s environment | genuine | `resticOK` beside `resticIn`; the four use them (the init test keeps its own: it varies the password and the repository) | test only |
| 11 | the adapter built twice a package run | genuine (~2.5 s) | once, by the first test that asks (`sync.Once`), removed in `TestMain` | test only |
| 12 | the README procedure leaves out the dev image | **not genuine**: both builds are `CGO_ENABLED=0`, and `make test` needs neither restic nor sqlite3 — the dev image's release reaches nothing a lane holds | answered | — |

A printed line changes (6): `moteld validate`'s refusal of a path
over 400 bytes — for the PR body.

## 4d. PR #162, and Copilot

Pushed at `415b2ce`, PR #162 into `backup`, Copilot requested. CI green
on both arches. The first `e2e-backup` restored nothing (a new PR's
run reads only its own ref's, `backup`'s and `main`'s caches, none of
which has one) and saved `e2e-backup-Linux-{X64,ARM64}-go1.27.1-…` —
CI's floating `golang:1.27-trixie` was go1.27.1, the local go1.27.0.

**Copilot at `415b2ce`** (one finding, "high"): the version step runs
Compose before the restore and makes the CI file's `./.cache` bind
sources as root. Not so — plain `docker compose`, the base file, named
volumes (the run's log), and the save worked. Discussed with the owner:
**answer, and a comment saying why the step must not use `$COMPOSE`**
(`2ea227d`). Every lane again; `make test-integration` came from Go's
test cache (no Go input changed) and was run again with `-count=1`:
once **`TestIntegrationSystemdJournalTail` (liveswap, untouched) failed**
— "journal tail … [], <nil>" after its 5 s — and a second run in a fresh
container passed 89/89. Not diagnosed; noted in the PR body. (A
`-count=10` of that test alone fails for another reason, the runs
reading each other's lines within one second: an artefact of the
count, not the failure.) Pushed, the thread answered and left for the
owner, Copilot requested again.

**Copilot at `2ea227d`:** "Findings: None"; its earlier finding listed
as resolved. CI green on both arches. The cache answered the finding
itself: both arches "Cache restored from key:
e2e-backup-…-go1.27.1-…", then "Cache hit occurred on the primary key"
— nothing saved again. `e2e-backup` warm: 245–248 s a job (the make
step 211–218 s), against 290–313 s (262–279 s) cold on the first run.
The repository's cache: 10.33 GB, 20 entries.

## 4e. Go's test cache and the integration lane (fixed here)

**Measured** (2026-10-01): Go's test cache replays an integration
test's last result, a pass included, after the program it runs has
changed, given the same test binary and flags — a test that `os.Stat`s
the program first included (toy module, `golang:1.27-trixie`). On the
real pin R8, in `dev-systemd`, with the Makefile's own `go test` flags
narrowed to `./backups/engine/` and a private `GOCACHE`: first run
77.3 s, second `(cached)`; restic wrapped so that its `--help` says
`(default 10m0s)`; third run `--- PASS … (cached)`. CI's `test` job
restores the Go build cache, which holds these results: of the eight
latest CI runs, seven print `liveswap (cached)`; the `make
test-integration` step took 16–92 s cached, 245 s run (ubuntu-latest).

**Fix (the owner, 2026-10-01: in #162):** `go test -count=1` in
`make test-integration`, the Makefile's comment saying why; the README
procedure's step 2 says what its claim rests on. Seen working: with the
fixed line, the second run is a run (72.8 s), and the changed restic
fails R8 (`--- FAIL: TestIntegrationResticRetriesAStuckRequestByItself`).
Cost: CI's `test` job runs the integration lane every time (~245 s an
arch against 16–92 s).

`245e9f7`: every lane green — `make test-integration` ran every
package, none cached, 89 passed, none skipped, 225 s; secretscan 267
commits, no leaks; e2e-backup's six suites, e2e. Pushed, the PR body
says so (and lists §7's two flakes as found, for a separate PR),
Copilot requested again.

## 4f. Copilot at `245e9f7`: the procedure's cold reads

Copilot's overview, "previously missed", low: the README procedure
clears restic's cache once, before its loop, so the reads after the
first are not from an empty cache. Discussed with the owner: **fix**.

- **Seen failing** (the README's block as written, sizes scaled down,
  a probe of the cache's size before each read): `snapshots` from
  nothing; **`ls` with 308 KiB cached, `stats` and `check` with 992
  KiB**. The figures the procedure is compared with (M78 `snapshots`
  19.6 s and `ls` 24.6 s cold, M80 `stats` 6.4 s cold) were each
  measured from an empty cache. The small-scale timings §4 records for
  `ls` (1.85 s) and `stats` (1.01 s) were from that same partly filled
  cache, not cold.
- **Fixed** (`28779ca`, local): the cache cleared before each read. The
  same probe on the fixed block: empty before every read.
- **Two "15-minute" reads on the way — not restic.** In those runs
  `stats` took 15 m 2.8 s and, a round later, `check` 909.2 s, exit 0,
  no retry on stderr, under a `timeout 120` that did not fire. The Mac's
  power log: clamshell sleep at 22:10:52 ("902 secs"), then maintenance
  sleeps of 903, 904, 907 s with 45 s DarkWakes between, on battery
  (16 %). The machine was fully awake from 15:35:34 to 22:10:52, so
  every result recorded before 22:10 here — both lane runs, the journal
  tail failure (~19:35), the spike rounds, the lanes at `245e9f7` —
  stands; the timings taken after 22:10 do not.
- **2026-10-02, the machine awake (fully from 18:26:08, no sleep
  after):** every lane green at `28779ca` — the integration lane run
  whole (none cached, 89 passed, none skipped), secretscan 268 commits,
  no leaks, e2e-backup's six suites, e2e. The README's three blocks run
  as written but for the sizes and a `timeout 60` on the wrong-key line
  (18:44–18:46): `snapshots` 1.44 s, `ls` 1.89 s, `stats` 1.88 s,
  `check` of a group 2.56 s, each from an empty cache, over the link;
  the wrong key retrying "signature … does not match" until the
  timeout (exit 124, the timeout's); the cleanup leaves nothing.
  Pushed; the finding answered in a PR comment (it was in Copilot's
  overview, no thread); the body updated; Copilot requested again.

## 4g. CI and Copilot at `28779ca` (2026-10-02)

- **CI red on `govulncheck` alone** (`ci-ok` with it): a new advisory,
  **GO-2026-6615** — "BatchProcessor can busy-spin when export buffer
  is full", `go.opentelemetry.io/otel/sdk/log` v0.19.0, fixed in
  v0.21.0 — reached in the root module and in penaltybox (`backups`:
  none); `.github/pin-watch.yml` does not accept it. `main`'s last CI,
  2026-09-28, was green: published since, so every branch's next run
  meets it. Its sibling GO-2026-6508 (`otlploggrpc`) is accepted there
  as `not_used` (the otel log modules arrive only through caddy's
  autoexport, which builds no log exporter). Dependabot's open bumps:
  #160 (`sdk/log` 0.19.0 → 0.21.0, penaltybox), #143 and #144
  (`otlploggrpc` 0.19.0 → 0.21.0). **The owner: a separate PR**; #162
  stays red on it until then.
- **Copilot's overview, "previously missed"**: after a Go patch bump
  the arch-only restore key restores the previous Go's cache, whose
  build half the new Go cannot use, and the new key — saved once, never
  again — keeps it. **Measured** (`e2e-backup`'s two builds from empty
  caches, unpacked): modules 1,042 MiB in 25,293 files, build cache
  721 MiB. **The owner: drop that key** (`8531471`): only the same Go's
  cache is restored; a new Go starts cold once an arch (about +60 s,
  #159's figure). CI-only; the Go-bump case shows on the first run
  after one.
- `8531471`: every lane green (the integration lane run whole: none
  cached, 89 passed, none skipped; the Mac awake throughout). Pushed;
  both answered in one PR comment; the body updated; Copilot requested
  again.

## 4h. `main`'s caddy v2.11.6 bump (#163, `a0969ad`): a trial cherry-pick, then reset (2026-10-02)

At the owner's word, `a0969ad` was cherry-picked onto `8531471`; then
the owner chose instead to merge `main` into `backup`, and `backup`
into this branch. The trial (`cbf6997`, `718b6f0`; in the reflog) was
never pushed, and the branch was reset to `origin/backup-upkeep`
(`8531471`). What it met, for that merge:

- **`go.mod` conflict** (the `spf13` block): `backup` made `cobra` and
  `pflag` direct (#147), at the versions `main` keeps indirect; `main`
  moved `cast` v1.9.2 → v1.10.0. Resolved by keeping them direct and
  taking `cast` v1.10.0; every module then tidy and building but
  `backups`.
- **`backups/go.mod`** requires liveswap (`replace ../liveswap`): `go
  mod tidy` there moves `golang.org/x/tools` to v0.50.0 and
  `golang.org/x/vuln` to v1.8.0, and drops `packagestest`. `go.work.sum`
  needs nothing: the workspace builds and vets without the lines `go
  mod tidy` and `go list -m all` write into it.
- **`make vulncheck`**: no vulnerabilities in any of the four modules
  (GO-2026-6615 cleared).
- **Lanes:** seven green; **`make e2e` failed** in the box suite,
  `e2e/box-push.sh:126` (from #146): `grep -q -F 'backup files
  "../../deno-example/shared": the path reaches outside'` against
  push's output, which under caddy v2.11.6 is a JSON log line —
  `{"level":"error",…,"msg":"loading liveswap app module: provision
  liveswap: app demo: backup files \"../../deno-example/shared\": the
  path reaches outside the app's shared dir"}` — its quotes escaped.
  The refusal is right; the check's fixed string does not match it.

**Then (2026-10-02):** the owner merged `main` into `backup` (`ecfe910`:
#163 caddy v2.11.6, #164 golangci-lint v2.14.0, #156 the dockerfiles
group, #165 pin-watch), with `f646a65` (lint: four Caddyfile stats
named) and `54f6e84` (the box check matching the logged error too);
`backups/go.mod` tidied there. Merged into this branch at the owner's
word: **`27db308`**, no conflict (the one file both changed,
`backups/cmd/motel-backup/main.go`, merged by itself). `make
vulncheck` clean in all four modules; every lane green — the
integration lane run whole (89 passed, none skipped), secretscan 271
commits, no leaks, e2e-backup's six suites, e2e's box suite passing;
the Mac awake throughout. Pushed; the body's govulncheck paragraph
rewritten; Copilot requested again.

## 7. Found during PR 6, for a separate PR (the owner, 2026-10-01) — facts only

Both are flakes of the integration lane, in code this PR does not
change. The owner: a separate PR.

### 7.1 liveswap: `TestIntegrationSystemdJournalTail`, and a failed deploy's log tail

- **Seen:** once, `make test-integration` run uncached at `2ea227d`:
  `runner_systemd_integration_test.go:386: journal tail of
  motel-itest.journaltail.0a1b2c3d0a1b2c3d.prestart.service: [],
  <nil>`, after 5.13 s. Seven full lane runs that day: one failure. CI's
  record of it is mostly replayed results (§4e).
- **The test:** a transient unit on root's user manager runs `echo
  first line; echo second line >&2; exit 2`; it then calls
  `journalctlReader.tail` — `journalctl --user --no-pager --quiet -o cat
  -n 41 --since @<start − 1 s> _SYSTEMD_USER_UNIT=<unit>` — every
  100 ms until both lines are there or 5 s have passed.
- **Reproduced (measured):** the unmodified test, with
  `systemd-journald` stopped (SIGSTOP) for its first 2 s, fails with the
  same message after 5.16 s; the control passes. The journal holds both
  lines, with `MESSAGE`, `PRIORITY`, `SYSLOG_FACILITY`,
  `SYSLOG_IDENTIFIER=motel-itest`, `_GID`, `_HOSTNAME`, `_PID`,
  `_RUNTIME_SCOPE`, `_STREAM_ID`, `_TRANSPORT=stdout`, `_UID`, and
  **without** `_SYSTEMD_USER_UNIT`, `_SYSTEMD_UNIT`, `_SYSTEMD_CGROUP`,
  `_SYSTEMD_INVOCATION_ID`, `_SYSTEMD_SLICE`, `_SYSTEMD_USER_SLICE`,
  `_SYSTEMD_OWNER_UID`, `_COMM`, `_CAP_EFFECTIVE`, which a line read
  while its process lives carries. No unit field of any name is set.
- **Natural rate (measured, one fresh `dev-systemd`, the test binary
  alone, 2 s apart):** idle 0/20; four `logger` loops flooding the
  journal 0/20; twelve busy loops on 6 CPUs 0/20; three loops writing
  and fsyncing 512 MiB 1/20, then 0/40. The natural failure's lines
  were stored the same way, without unit fields (`_PID` 511574). In that
  container: 122 runs, 120 lines attributed, 2 not (one forced, one
  natural). A `-test.count` above 1 fails another way — the runs read
  each other's lines within one second — and is no measure of this.
- **The product path:** `failureDetail` (`liveswap/app.go`) reads the
  same tail for a failed deploy — the pre_start oneshot's unit and the
  app's, by `unitName`, `_SYSTEMD_USER_UNIT=` each, up to five attempts
  within `journalTailTimeout` (5 s). With the lines stored without unit
  fields, `LogTail` is empty and `LogTailError` is empty: the response
  carries none of the app's output, and no word of why.
- **Constraints:** the units' `SyslogIdentifier` is `motel-<app>`
  (`runner_systemd.go`), one for every unit of an app — the live
  instance writes under it during a deploy too; operators are told to
  read an app's output with `journalctl -t motel-<app>`
  (`liveswap/README.md`, `docs/first-deploy.md`,
  `docs/after-first-deploy.md`). liveswap reads a unit's `MainPID`
  (`statusFromProps`), not `ExecMainPID`.
- **Unmeasured:** what the manager reports as `MainPID` / `ExecMainPID`
  for a failed transient oneshot after it has exited; whether
  `_STREAM_ID` can be known to the caller; the rate on CI's runners.

### 7.2 backups: `TestIntegrationWhatIsBoundIsWhatWasPinnedWhateverTheAppDoesToTheName`

- **Seen:** once, the spike's round 1 (a fresh `dev-systemd`, the whole
  lane uncached): `pin_integration_test.go:113: 31 units shown the
  directory, 32680 flips (5969 pins refused mid-flip): not enough of a
  race to mean anything`, the test 1.74 s. Seven full lane runs that
  day: one failure.
- **The test:** a goroutine flips `blog/shared/uploads` with no pause —
  rename it aside, symlink the sibling at its name, remove the link,
  rename it back. The loop `for i := 0; shown < units && i <
  100*units; i++` (`units` 60, `PIN_RACE_UNITS` overrides) pins
  `uploads` by `sharedPin.beneath` each attempt — refused when caught
  mid-flip — and starts a unit for each pin that holds; at most 6,000
  attempts. In the failure 5,969 of 6,000 were refused (99.5 %), 31
  units were shown, and the test fails on `shown < units`.
- **Not the same as its CI failure** (run `35465884405`,
  `backup-engine`, 2026-09-19, both arches): `run 0: {ExitStatus:217
  Result:exit-code}` — the account, during PR 2.
- **Its sibling** (`TestIntegrationARestoreNeverLandsInASiblingsData…`,
  `engine/restore_integration_test.go`) pins once and runs a fixed 40
  units; it has not failed.
- **Unmeasured:** the refusal rate idle and under load; whether the
  failure reproduces on demand.

## 5. Caddy coupling — report, nothing built

Where backups lean on Caddy, what each buys, what holds it:

| Coupling | What it buys | Held by | Verdict |
|---|---|---|---|
| The plan is `moteld adapt --adapter caddyfile`: Caddy's own reading of imports, snippets, heredocs and `{$NAME}`, from any depth (`plan.Inspect`) | Exactness. The import reader it replaced (#146) was where that PR's findings clustered | the adapted JSON is liveswap's own schema under `apps.liveswap` (ours); e2e every run | keep |
| Caddy's empty-glob warning, as JSON on stderr (`emptyGlobs`) | Refusing an import a run's view cannot see — the silent shrink of the plan | C1 | keep |
| `{$NAME}` substituted in the text before any token; unset with no default is empty; `:default` | Refusing a root or backup path that depends on the server's environment; adapting an ordinary production Caddyfile with no environment | C2 (new), e2e backup 5, 5b | keep |
| The placeholder trials: one adapt per variable, after the base, every run; the next kind of value chosen by whether Caddy's error quotes the last | as above | Caddy's error quoting a value only speeds the choice — correctness does not rest on it | **measured: ~25 ms an adapt (63 ms the first, cold)**, so (1 + N) × 25 ms a run. Costs nothing worth removing. The one reduction — D10(b), the plan unit given motel's own `EnvironmentFile=` so that no trial is needed — puts the server's secrets in that unit's memory: **a change to the security model, the owner's** |
| `cmd/moteld`'s check at `validate`/`reload` (#147): `caddycmd.Commands()[name].CobraFunc`, `caddycmd.LoadConfig`, tokens' `File()` | Refusing, before it goes live, a declaration a run would not see | its own tests (the hooks are there), e2e status 8, 8b | keep |
| liveswap's package-level record of where each `backup` and `root` came from | as above | decided to stay (2026-09-25) | keep |

**Nothing found that buys nothing.** The coupling that remains is
pinned (C1, C2) or ours.

## 6. Item 6, the re-estimate

Not this PR: it needs real use, and there is none yet. §7's run
against a real bucket is the first evidence.
