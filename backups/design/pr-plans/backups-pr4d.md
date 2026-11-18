# PR 4d — the package's lifecycle edges and the account's lookups

Not committed, like `PLAN-backups.md`, `HANDOVER-backups.md`, `helpful/` and the three plans before it. Branch `backup-lifecycle` from `origin/backup` at `d0b213b` (#153 in, checked `MERGED` first). Into `backup`; closes #154. Never merged by me, never force-pushed. Worked in this folder, no worktree.

Order: this plan and its two closed tables → the tables and failure-path tests, each seen failing → the code → every lane → `/code-review xhigh`, scored and ranked → push, PR in #153's style, Copilot. The review loop ends inside the PR.

**The two tables below are closed.** Every cell is either *checked*, with the row that holds it, or *not checked*, with why. A finding that fits a cell is answered by pointing at the cell; a finding that names a cell not here is a new row of the table, and the table was wrong.

## 0. The owner's answers (2026-09-27, before anything was built)

| # | Question | Answer |
|---|---|---|
| 1 | The uid check and a directory that does not enumerate | **Each source on the passwd line is asked by uid**, and any holder but the account is refused by name. The README says what is left. (The owner: no box runs this yet, so "nothing that works today is refused" is no argument for the weaker check. Re-measured on that word: the issue's "no local check can do more" did not hold, M65.) |
| 2 | postinstall when the account cannot be made | **Warn and go on.** The configure completes; setup makes the account or refuses. The `useradd` line stays as it is in its four places. |
| 3 | A backup unit that will not stop at an upgrade | **Said, and the upgrade goes on.** |
| 4 | The verification gap | **The listing's inode and owner are held against the pinned directory's own** (a third way, found by M63). |
| 5 | The raw-binary tarball | **Gains `motel-backup` and the four unit files**, and `README.txt` says what the package would have done. |
| 6 | The drill's timer | **The hourly's fixed spread**: `RandomizedDelaySec=10min`, `FixedRandomDelay=true`, `AccuracySec=1s`. |

Writing the tables found cells the six did not cover. Two were put to the owner as multiple-choice questions before any code, and two were withdrawn on the owner's word (§0.1).

### 0.1 The owner's answers on the tables' open cells

The owner, on the ssh-key cell: an administrator who sets up their own server so that their own account is reachable is no attacker — "it doesn't make sense" as a threat. It does not. Root and the operator with sudo are the TCB (§1.1 of `PLAN-backups.md`), and none of the attackers there (app code, the serving process, `motel-admin`, a stolen storage key) can change sshd's configuration, set a password or add an account to a group.

**What the account checks are for, written down:** a guard against an account named `motel-backup` that is already on the box for another reason, which someone who is not root has been given a way to use. Setup would otherwise adopt it by name and hand that someone the repository password.

| Cell | Answer |
|---|---|
| A18, a password | **Kept: refused unless locked.** It fits the guard — an account someone knows the password to — and is one lookup. (I had called it a hole; the measured route needs sshd set to `internal-sftp`, which is not Debian's default. Corrected before the owner answered.) |
| A20, an ssh key outside the home | **Not checked; withdrawn as a question.** |
| A13, any other group | **Not checked; withdrawn.** It stays as the owner decided in #153. |
| B5, a command run from a shell while the package changes | **Said, and the change goes on.** |

### 0.2 The owner's answers on the review's findings (`/code-review xhigh`, 2026-09-27)

Fifteen findings, scored and ranked before any work (§7). Thirteen genuine and fixed here, each as a row seen failing first. Two were the owner's:

| Finding | Answer |
|---|---|
| The listing of the whole passwd database is held to the ten seconds of a lookup by a key, and a box joined to a large directory would refuse every run | **Kept, with a longer bound: a minute for that one lookup.** |
| The wrong-directory defect is found after the upload, so a misconfigured service still writes a snapshot of empty directories per app per hour, and those become the newest | **A run refuses up front**, before anything is uploaded, where its mount namespace is not the manager's. The inode and owner check stays behind it. |

## 1. Table A — the account invariant

**Nobody but the manager can be, or act as, the `motel-backup` account.** From the threat model (`PLAN-backups.md` §1.1, §1.2 rule 3): the credential exists in a root-only file and in the environment of a restic process running as `motel-backup`. Whoever is that uid reads that environment; whoever shares its groups reads what restic reads. Root and the operator with sudo are the TCB: they read the file itself.

**Scope.** The account as the account databases define it (passwd, group, shadow, as NSS answers) and the ways to log in as it. What else on a box runs a command as a named account — a sudoers rule, a crontab, a unit with `User=` — is root's own act naming the account, and is cell A21.

Checked in one place, `accountUsable` behind `accountReady`, asked by setup, by every run, restore and drill (`begin`), and by `motel-backup account` (postinstall's warning). Rows are in `TestAnAccountMadeWrongIsRefusedNotNormalised` (setup) and `TestARunRefusesWhatSetupRefusesWithSetupsWords` (run, drill, restore) unless said.

| # | By | Cell | Checked, and its row — or not, and why |
|---|---|---|---|
| A1 | uid | root's uid | **Checked.** Row "uid 0". |
| A2 | uid | A uid shared with an account the box enumerates | **Checked**: `getent passwd` enumerated, every holder named. Rows "the motel user's uid", "a uid shared with two accounts"; run row "a shared uid"; e2e setup 2. |
| A3 | uid | A uid shared with an account a directory holds and does not enumerate | **Checked, new (answer 1)**: each source on `nsswitch.conf`'s passwd line is asked for the uid (`getent -s <source> passwd <uid>`, M65). Rows "a uid a directory holds and does not enumerate", "…held by the second of three sources"; `TestThePasswdLineIsReadAsGlibcReadsIt`. |
| A4 | uid | A uid held in a source that does not answer (the daemon down, the module not installed) | **Not checked.** `getent -s` exits 2 for a source that does not answer, as it does for absence (M65): no local lookup tells the two apart. Every run asks again, so a directory that answers later is caught then. Said in the README. |
| A5 | uid | A second holder of the uid inside one source that does not enumerate | **Not checked.** A lookup by uid answers with one account. Where that one is not the account, it is refused (A3); where a directory holds the account itself and another at its uid, nothing local sees the other. Said in the README with A4. |
| A6 | uid | The uid given to another account once the account is removed | **Checked**: an account that is not there is refused. Row "the account". |
| A7 | uid | A process already running as the uid outside any unit of a run | **Not checked.** Only root starts a process as an account that has no login (A14–A17), and root is the manager. |
| A8 | group | root's group as its own | **Checked.** Row "gid 0". |
| A9 | group | root's group among its groups | **Checked.** Row "root's group among its groups". |
| A10 | group | The `motel` group, as its own or among its groups | **Checked.** Rows "the motel group among its groups", "…as its own", "…the motel user's primary group being another". |
| A11 | group | The `motel` user's primary group, where that is not the group named `motel` | **Accepted, deliberately**: the apps' files belong to the group *named* `motel`. Row "the motel user's primary group, which is not the motel group". |
| A12 | group | A group of another name with root's or `motel`'s gid | **Checked**: the comparison is by number, from `id -G`. The rows of A9 and A10 are numbers. |
| A13 | group | Any other group among its groups (`disk`, `shadow`, `docker`, `sudo`, `adm`) | **Not checked.** Only root puts an account in a group, and root is the manager. The owner, #153: root's group and the `motel` group are refused, because the separation the account exists for is from those two. |
| A14 | group | Others in the account's own group | **Not checked.** A group reads no process's environment. What is the account's own: the cache (`/var/cache/motel-backup`, ciphertext as the repository holds it) and a fetch directory that is `0700` under a parent only root can reach (`restore/`, `0700` root). The owner, #153: gid uniqueness is not asked. |
| A15 | login | A login shell | **Checked**: the four exact paths that refuse a login. Rows "a login shell", "a shell named nologin elsewhere". |
| A16 | login | No shell set | **Checked.** Row "no shell at all, which login reads as /bin/sh". |
| A17 | login | A home that exists (`~/.ssh/authorized_keys`, a shell's rc, a user manager's units) | **Checked**; `/nonexistent` exempt. Rows "a home that exists", "both", "/nonexistent, which exists on this box". |
| A18 | login | A password | **Checked, new (§0.1)**: the shadow entry is read (`getent shadow`), and a password that is not locked (`!` or `*` first, as `useradd --system` leaves it) is refused, the remedy `passwd -l`. Without root the entry cannot be read, and `motel-backup account` says it could not tell. Rows "a password", "a password locked", "no shadow entry", "an empty password". Measured both ways: `su`, and `su -s` by anyone but root, run the account's shell and are refused (M67); sshd with `Subsystem sftp internal-sftp` runs no shell, and with the password read `RESTIC_PASSWORD` out of `/proc/<pid>/environ` of a unit's process (M70). |
| A18b | login | A password a directory holds for the account | **Not checked.** The account's passwd line from a directory holds a mark (`*`), and the password is the directory's to know: no lookup on the box reads it. Row "a mark in the passwd line itself" pins what is done: the mark is taken as no local password. |
| A18c | login | The check asked by someone who is not root | **Checked**: the shadow database is root's to read, and asked by anyone else `getent` says "not found" of an entry that is there. What can be seen is said, with what was not looked at beside it; an account with nothing else wrong is not called right. `TestTheAccountAskedWithoutRoot`; e2e setup 2. |
| A18d | login | A shadow database root cannot read (a security module, a root that is one in name) | **Checked (the review, 5)**: `getent shadow` says "not found" of it as of absence, so "not found" is believed only where `/etc/shadow` could be opened; else refused. No file at all is no database. Row in `TestALookupThatFailsRefuses`. |
| A19 | login | An ssh key under the home | **Checked by A17**: no home, no `~/.ssh/authorized_keys`. |
| A20 | login | An ssh key where sshd's configuration looks outside the home (`AuthorizedKeysFile /etc/ssh/keys/%u`, `AuthorizedKeysCommand`) | **Not checked (§0.1).** It takes a changed sshd configuration and a key placed for this account's name: root's own act, twice. |
| A21 | login | Whatever else runs a command as a named account: a sudoers rule, a crontab or `at` job, a unit with `User=`, lingering | **Not checked.** Each is root's own act, naming the account; root reads the credential file itself. (A crontab of the account's own making needs a login: A15–A18.) |
| A22 | lookup | What NSS answers for the name | **Checked**: the account is read by `getent`, through the NSS the manager resolves `User=` through. **New:** what a fetch gives its directory to is the account the check looked at, kept on the run — one lookup as the command begins, no second one for a directory to answer otherwise (the review, 14) — where `os/user` read `/etc/passwd` alone. The `motel` account is looked up by `getent` too (the review, 7). Rows `TestTheAccountIsOneAnswer`, `TestTheAccountCheckRunsOnItsCommandsContext`. |
| A23 | lookup | The answer changing between the check and a unit's start | **Not checked.** The manager resolves the name at each unit's start, and no local check binds the two lookups. Every run checks again. |
| A24 | lookup | How long a lookup takes | **Checked, new**: every lookup runs under `exec.CommandContext`, bounded — ten seconds for a lookup by a key, **a minute for the listing of the whole database** (the owner, §0.2) — the context threaded from `begin` and setup through `accountReady`. Rows in `TestALookupIsBoundedAndEndsWithItsCommand`: a lookup that never answers ends at the bound and the refusal names it; a cancel ends the child and is an interrupt. |
| A25 | lookup | A lookup that fails (an exit that is neither 0 nor 2, a line that is no passwd line) | **Checked**: refused with the lookup's own error, never accepted. Rows in `TestALookupThatFailsRefuses`. |
| A26 | lookup | What it cannot enumerate | A3, A4, A5. |
| A27 | lookup | The account's groups, where a directory holds memberships | **Checked**: `id -G` is `initgroups`, which asks each source for the account's groups; it is what the manager gives a unit. Rows of A9, A10. |
| A28 | lookup | The passwd line itself: no `nsswitch.conf`, no passwd line, actions in brackets, a source named twice | **Checked**: read as glibc reads it (absent: `files`). A file that cannot be read refuses. `TestThePasswdLineIsReadAsGlibcReadsIt`. |

## 2. Table B — the package's transitions × what the package owns

Transitions: **install** (fresh) · **upgrade** (`prerm upgrade` → unpack → `postinst configure <old>`) · **remove** · **reinstall** after remove · **purge**. Rows are in `packaging/test/smoke.sh` unless said. "Kept" means untouched.

| # | Owned | install | upgrade | remove | reinstall | purge |
|---|---|---|---|---|---|---|
| B1 | The four unit files, the binary | dpkg's: `root:root 644` (row "after install") | replaced | gone (row "after remove") | back | gone |
| B2 | Enable state: the `wants` symlinks, the helper's state, the masks | enabled once | an administrator's disable kept (rows "after an upgrade, disabled before it", "…enabled before it") | masked | unmasked, enabled | the helper's state and the masks gone |
| B3 | The timers' active state | started | stopped by preremove, started again if enabled | stopped | started | — |
| B4 | A run or a drill under way as the service, and its units | none: nothing starts before setup | **New:** stopped by `systemctl stop` itself and checked inactive; one that will not stop is named, and the upgrade goes on (answer 3). `deb-systemd-invoke` skipped the stop under a `policy-rc.d` and said 0 (M69). What preremove stopped — written down by it, the timers that were running (the review, 4) — postinstall starts again, itself, where the timer is enabled: under a `policy-rc.d` `deb-systemd-invoke` starts nothing, and the upgrade would leave the timers off. Rows "an upgrade under a policy-rc.d ends the run", "a unit that will not stop is said, and the upgrade goes on" | the same stop, the same check | — | — |
| B4b | Units a command killed from a shell left running (it has no service to bind them to), recorded in `units` | — | kept: the next run's sweep stops them | **New (the review, 3):** stopped by the exact names recorded, as the sweep does, where nothing holds the lock; a line that is no name the engine writes is not passed to the manager; one that will not stop is named. `init-unit` is left to end, as a run leaves it | swept by the first run | as remove, before `units` is removed. Row "purge stops what a killed command left running" |
| B5 | A command run from a shell while the package changes (`sudo motel-backup run`, a restore at its prompt) | — | **New (§0.1):** preremove tests the run lock; held, it names the pid, says a command is under way and that its binary is about to change, and goes on. Row "a command from a shell is said, and the upgrade goes on" | the same, "about to go"; its mounts are left alone (B7) | — | the same as remove |
| B6 | The persistent timers' stamps | written at the first activation, nothing fires (M61) | kept: a catch-up after an upgrade is the timer's own | **New: removed** | the first activation writes them and fires nothing. Row "reinstalled after remove: the stamps are new" (staged eight days old before the remove) | **New: removed** (a remove by an older version left them) |
| B7 | `/run/motel-backup`: a killed run's bind mounts | — | kept: the next run's sweep | **New:** made private, then detached, where nothing holds the lock. Private first (the review, 1; M71): a recursive bind of a shared root is its peer, and unmounting a disk beneath it unmounts the disk beneath the app's own directory. Rows "a disk inside the app's data stays mounted", with one of a name mountinfo escapes | swept by the first run | **New:** unmounted as at remove; then the run directories by `rmdir`, never `rm -r` — and nothing beneath the run directory at all while anything is still mounted there (the review, 8), which is named. Rows "purge unmounts what a killed run left", "a mount that is none of the engine's" |
| B8 | `/run/motel-backup`: `lock`, `units`, `init-unit`, the run directories' files | made by the first command | kept | kept: the next run reads `units` and `init-unit` | read and swept | removed, file by file; a directory by `rmdir` |
| B9 | The state directory's files: `status.json`, `status.json.aside-*`, `.status-*`, `repository-id`, **`repository-id.new`**, `listing.err` | none | kept | kept | kept | removed. **New:** `repository-id.new`. Row "purge is not defeated by setup's temp file" |
| B10 | `staging/`, `restore/` | none | kept | kept | swept by the first run | `rmdir` only; one that holds a file is named and kept |
| B11 | Anything else in the state directory | — | kept | kept | kept | kept, and **new:** the "kept" line says what it is — files the package did not make — and blames copies of app data only for `staging/` and `restore/`. Row "the kept line says what is kept" |
| B12 | `/var/cache/motel-backup` | none | kept | kept | kept | removed |
| B13 | The account and its group | made with setup's line. **New (answer 2):** where it cannot be made the configure completes with a warning. Row "a group of the account's name: installed, and said" | left as it is; warned of when setup would refuse it (row "in the motel group") | kept | kept | kept (Debian policy) |
| B14 | `/etc/motel-backup/`: the credential file, a staged file a killed setup left | never made by the package | byte-identical | kept | byte-identical | kept, and said why |
| B15 | `/etc/motel/backup.env`, from before this version | not the package's | kept | kept | kept | kept: the operator's |
| B16 | The journal | not the package's to remove | | | | |

**Not rows of their own:** `abort-upgrade`, `abort-remove` and `failed-upgrade`. postinstall takes one path whatever its first argument (`$2` alone chooses start or restart), and preremove takes `failed-upgrade` as `upgrade`. moteld's own service, drop-ins and lingering are the package's from before backups, and their rows are the smoke's stages 1–4.

## 3. Measured first (numbers continue `PLAN-backups.md` §0)

| # | Question | Result |
|---|---|---|
| M63 | What restic 0.18 answers about a `files` item's directory, and at what cost | `ls --json <id> <parent>` prints each child's `inode`, `uid`, `gid` (no `device_id`). Through a bind mount the inode is the bound directory's own: `/real/uploads` ino 2363271 → node inode 2363271; a bare mount point bound in its place → uid 0, another inode. Looking inside costs a line per direct child (5003 for 5000 files), and an empty directory and a swapped one are both 2 lines. The snapshot's `summary.total_files_processed` is restic's own count of the same view. |
| M64 | Debian 13's passwd line | `files` in the bare image; `files systemd` once `libnss-systemd` is installed. |
| M65 | One source asked by uid | `libnss-extrausers` holding `alice` at the account's uid: `getent passwd 999` → `motel-backup` (the first source wins); `getent -s extrausers passwd 999` → `alice`, exit 0; a uid the source does not hold → exit 2; a source whose module is not installed → exit 2. sssd 2.10.1 with `enumerate = false` (proxy provider): `alice` absent from `getent passwd`, `getent -s sss passwd 999` → `alice`. Before sssd runs: exit 2. After it stops it still answered, from its memory cache. |
| M66 | `useradd` with a group of the account's name | exit 9. With `--gid motel-backup`: exit 0; exit 6 when the group is not there. |
| M67 | A password on an account whose shell is `nologin` | `nologin` is not in `/etc/shells`. `su motel-backup -c id` with the password: "This account is currently not available", exit 1. `su -s /bin/sh` as anyone but root: "using restricted shell /usr/sbin/nologin", exit 1. |
| M68 | The stamps across remove, reinstall, purge | Both stamps stay after remove and after purge. Reinstalled with the stamps eight days old and the credential file kept: `motel-backup-drill.service` started in the same second as dpkg's `configure`. The hourly's catch-up is subject to `RandomizedDelaySec=`, so it comes within ten minutes, not inside the configure. |
| M69 | `deb-systemd-invoke stop` under a `policy-rc.d` that exits 101 | "not running 'stop motel-backup.timer'", exit 0, the timer still active. |
| M71 | A recursive bind of an app's directory taken away, with a disk mounted inside the original, on a shared root | Private root (a container's): the disk stays. **Shared root (a booted box's): the disk beneath the app's own directory was unmounted with the bind** — by `umount -l` of the bind, and by unmounting deepest first. With the bind made private first (`--make-rprivate`): the disk stays, either way. systemd makes the root shared at boot and leaves a container's as it found it, which is why no lane had seen it. |
| M72 | How a command knows the manager sees its mounts | `/proc/1/ns/mnt` cannot be read under the service's hardening (its capabilities are not PID 1's). **The manager keeps a mount unit for every mount of its own namespace**: a mount made from a shell, and from a unit hardened as the shipped one is, was `active` at the first asking; from a unit with `PrivateNetwork=yes`, `PrivateMounts=yes` or `ProtectSystem=strict`: no such unit, after thirty askings. |
| M70 | sshd, a password, and a unit's environment | Debian's default `Subsystem sftp /usr/lib/openssh/sftp-server` runs through the shell and is refused. With `Subsystem sftp internal-sftp`: `get /proc/<pid>/environ` as the account fetched `RESTIC_PASSWORD=the-secret` from a unit running as it. A command over ssh is refused both ways. With the password locked (`usermod -L`): "Permission denied". |

## 4. Tests first, each seen failing before the code

### 4.1 `backups/engine` (scripted `box`)

- **`TestAnAccountMadeWrongIsRefusedNotNormalised`** — rows A3 (two) and A18 (four).
- **`TestARunRefusesWhatSetupRefusesWithSetupsWords`** — a row each for A3 and A18, so that the run holds them.
- **`TestThePasswdLineIsReadAsGlibcReadsIt`** (A28) — pure: the text of an `nsswitch.conf` → the sources. Rows: `files`; `files systemd`; `files sss [NOTFOUND=return] ldap`; a source twice; tabs and a comment; no passwd line; no file.
- **`TestALookupIsBoundedAndEndsWithItsCommand`** (A24) — the real `lookup` against a real child (`/bin/sleep`): ended at the bound, the error naming the command and the bound; ended by a cancel, the error the context's.
- **`TestALookupThatFailsRefuses`** (A25) — `account`, `holders`, `groupsOf`, `groupNamed` over a scripted `lookup`: exit 1, a line of five fields, a uid that is no number, an empty answer with exit 0.
- **`TestTheAccountIsOneAnswer`** (A22) — `backupOwner` answers what `account` answered.
- **`TestADrillStoppedWhileItWaitsIsNoVerdict`** — a row: interrupted during the plan unit, the last drill's verdict stays. The guard moves inside `couldNotBegin`.
- **The verification** (`engine_test.go`, the listing's table) — rows: the node's inode is the pin's → ok; another inode → "is not the directory that was given to the backup"; the same inode and another owner → the same; an empty directory with the pin's inode → ok; a file item the same.

### 4.2 `backups/cmd/motel-backup/units_test.go`

- The forbidden keys are looked for in every section. Seen failing: `OnFailure=` written into `[Unit]` of a copy passes the old loop and fails the new.
- The drill's timer: `RandomizedDelaySec=10min`, `FixedRandomDelay=true`, `AccuracySec=1s`.

### 4.3 `packaging/test/smoke.sh` — Table B's new rows

B4 (two), B5, B6, B7, B9, B11, B13. Each seen failing against the package as `origin/backup` builds it.

### 4.4 `e2e/backup`

- `units.sh`: the drill timer's spread read off the loaded timer, as the hourly's is, and its offset the same after a restart; units 6: a run under a drop-in that gives the service a mount namespace (`PrivateNetwork=yes`, the M54 cause) says of its `files` item that it is not the directory given, by both inodes and owners, and a declared directory that is empty is `ok`.
- `setup.sh`: scenario 2 gains the account with a password (`chpasswd`): refused before any prompt, naming the `usermod --lock`; as nobody, what was not looked at is said; locked, accepted. And a uid a directory holds without enumerating it: sssd (proxy provider, `enumerate = false`) in the e2e box, started by the scenario alone — refused, naming the holder; with sssd stopped and its cache gone, the source says what absence says and the account passes (A4, pinned as what happens).

## 5. Then the code

- `backups/engine/setup.go` — `lookup` (one `exec.CommandContext`, bounded); `account`, `holders`, `groupsOf`, `groupNamed` over it, each taking the context; `passwdSources`; `holders` asks each source; the exit-3 fallback gone; `accountReady(ctx)`; `AccountReady(ctx)`.
- `backups/engine/engine.go` — `begin` passes its context; `backupOwner` from `account`; `lsNode` gains `Inode`, `UID`, `GID`; `view` keeps each item's pin identity; `verify` holds the node against it.
- `backups/engine/pin.go` — `identity()`.
- `backups/engine/restore.go` — `couldNotBegin`.
- `backups/cmd/motel-backup/main.go` — `account` with a context.
- `packaging/postinstall.sh`, `preremove.sh`, `postremove.sh` — Table B.
- `packaging/motel-backup-drill.timer` — the spread.
- `.github/workflows/release.yml` — the tarball.
- `backups/README.md` — what the uid check covers and what it cannot; the transitions table; the drill's time; the tarball.
- `PLAN-backups.md` (uncommitted) — M63–M70; "PR 4d — what the owner decided, and where it stands".

## 5.0 What the review changed in the engine

- **`bindMount` makes what it has bound private; `unmountDetach` makes it private again before it detaches** (M71). The engine's own, since #139: on a booted box the end of every run unmounted a disk mounted inside an app's data, from under the live app. `TestIntegrationTakingABindAwayLeavesTheAppsDiskMounted` (seen failing against the calls as they were); e2e units 7; the smoke's stage 0 makes the root shared.
- **`begin` refuses where the manager does not see the command's mounts** (M72): one mount of its own, of nothing, and the manager asked for its mount unit (`unit.Runner.Sees`). Rows in `TestARunRefusesWhatSetupRefusesWithSetupsWords`; `TestIntegrationSeesAMountOfTheManagersNamespace`; e2e units 6.

## 5.1 Found while building

- **The timers' own start limit** (the smoke): four upgrades inside ten seconds start each timer five times, and the manager refuses the sixth (`start-limit-hit`, the timer `failed`). Nobody upgrades at that pace; the smoke resets the count before each staged upgrade, and says why.
- **With the old package and the stamps eight days old, the run after a reinstall met the catch-up's lock** ("another backup run is in progress"): M68's consequence, seen in the smoke's keep-going run against the package as `origin/backup` builds it.
- **`as_nobody motel-backup account`** (e2e setup 2, "it needs no root") would have lost its fault behind "root's to read" with the password check returning first: the check gathers what it can see (A18c).
- **`apt-get purge` of an installed package is a remove and then a purge**, so postremove runs twice and a command holding the lock is said three times (preremove, and postremove twice). Left so: each script says what it found.

## 7. The review's findings, scored and ranked before any work

| Rank | Finding | Verdict |
|---|---|---|
| 1 | Unmounting the bind unmounts the app's disk under shared propagation | Genuine, measured (M71); the engine's own unmount the same. Fixed in both |
| 2 | Purge removes the record of units a killed command left running, and leaves them running | Genuine: a row Table B lacked (B4b) |
| 3 | The password check passes where root cannot read the shadow database | Genuine (A18d) |
| 4 | Purge removes through a mount sitting on a run directory | Genuine (B7) |
| 5 | The enumeration under the ten-second bound | The owner's (§0.2) |
| 6 | The wrong directory is found after the upload | The owner's (§0.2) |
| 7 | postinstall starts timers no upgrade stopped | Genuine (B4) |
| 8 | The `motel` account through `os/user` | Genuine (A22) |
| 9 | A second lookup of the account at every fetch | Genuine (A22) |
| 10 | The smoke's drill-time check holds a literal `3` | Genuine |
| 11 | A warning of a mount the next unmount takes away | Genuine |
| 12 | postremove calls a service that would not stop "a command from a shell" | Genuine |
| 13 | The remedy names the shell and the home where the fault is the password | Genuine |
| 14 | The tarball README's `useradd` line is held by no test | Genuine |
| 15 | The units suite leaves a directory for the suites after it | Genuine |

## 6. Verification, in order

1. Every new row seen failing first; then green.
2. `make test` · `make lint` · `make secretscan` · `make test-integration` · `make package` and `make install-test` · `make e2e-backup` · `make e2e`.
3. `/code-review xhigh`; findings scored and ranked before any work.
4. Push `backup-lifecycle`; the PR into `backup` in #153's style, "Closes #154"; Copilot requested.
5. The review loop, to its end, here.
