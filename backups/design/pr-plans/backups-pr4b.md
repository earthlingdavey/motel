# PR 4b — `motel-backup setup`, the account, the credential file

Not committed, like `PLAN-backups.md` and `pr-plans/backups-pr4.md`. Branch `backup-setup` from `origin/backup` at `fcf86c2` (#146, #147 in). Into `backup`; never merged by me, never force-pushed. Option A of the comparison page: the design as planned.

## Context

PRs 1–3, #141, 4a (#146) and #147 are in `backup`. Nothing yet makes the `motel-backup` account, writes the credential file, or initialises the repository: the e2e box does all three by hand (`e2e/backup/Dockerfile:35`, `e2e/backup/lib.sh` `write_env`, each suite's `rr init -q`), and the README's "By hand, until there is a setup command" tells an operator to. The credential file sits at `/etc/motel/backup.env`, which the example sudoers (`examples/box/sudoers:36-38`, `^/etc/motel/[A-Za-z0-9_-]+\.env$`) lets `motel-admin` `sudoedit` or empty — motel's reach reading root's credential, against §1.6. The owner decided (2026-09-20): 4b is `setup` + the account + the file's move to `/etc/motel-backup/repository.env`. Units, timers and packaging are 4c's; nothing of the engine, restore or drill changes except where this plan says.

## 1. Privilege boundaries (settled before any code)

| Principal / step | Runs as | Holds the secret | Writes where | What root consumes from it, and how |
|---|---|---|---|---|
| Operator at a terminal, `sudo motel-backup setup <repository>` | root (euid 0 required; else "needs root: sudo …") | Yes, for the command's life: what was typed (storage key, password) and the generated password, in memory only. Never in argv (the URL is argv and is not a secret; a URL with `user:pass@` is refused), never in the environment, never in the journal or on stdout beyond the one deliberate showing of the generated password. | `/etc/motel-backup/` (root, `0755`); `repository.env.staged` then `repository.env` (root, `0600`, written whole, fsynced, renamed); `/etc/passwd` etc. via `/usr/sbin/useradd` (argv constant); `/var/lib/motel-backup` (`0755`), `staging/`, `restore/`, `/run/motel-backup` (`0700`) as `begin` does; `status.json` moved aside to `status.json.aside-<time>`; `repository-id` (root, `0644`, written whole and fsynced, once the credential file is in place) beside it; the run lock. | The operator's typed answers (TCB). The previous `repository.env` (root's own file, one writer) — only whether it is there is looked at; what it names is not read. `repository-id` (root's own, one writer) — read to compare with the id restic answered. |
| PID 1 | root | Reads `EnvironmentFile=` before dropping to `User=` | the unit's environment | — |
| Unit **P** plan (`motel_backup_plan_<nonce>`) | `motel-backup`, user+PID namespaces, no capability, no network, no env file | none | stdout → root-owned `plan.json` in the run dir | Its JSON, `plan.Decode` strict + re-validated — exactly as `run` does today (`engine.plan`). |
| Unit **I** init (`motel_backup_init_<nonce>`): `restic init --json` | `motel-backup`, no capability, network, `EnvironmentFile=` the **temp** file, `CacheDirectory=motel-backup` | yes, in its environment | the repository (bucket); the cache; stdout/stderr → root-owned files in the run dir | Exit status + `Result` from the manager; stdout parsed strictly for the `initialized` JSON line (id 64-hex); stderr matched against one known phrase ("already initialized") and otherwise shown cleaned (`record.Text`), never kept. |
| Unit **Q** probe (`motel_backup_probe_<nonce>`): `restic cat config --no-lock` | as I | yes | cache; stdout/stderr → root files | Exit status only (0 / 12 / 10 / 1 classes as `resticFailure`); the config JSON's `id` (64-hex) for the final line; stderr shown cleaned. |
| `motel-backup status` (anyone) | caller | no (`0600`) | nothing | `Lstat` of the file (existence, mtime) through a `0755` directory; as root only, reads it to lint with systemd's rules — warnings printed, health unchanged. |
| `motel-backup run` / `restore` / `drill` | root | never reads it: passes the path as `EnvironmentFile=` | as today | as today; the message when the file is missing names the new path and, if `/etc/motel/backup.env` exists, says it is not read. |
| uid `motel`, apps (T1/T5), `motel-admin` | — | cannot read the file (`0600` root) or the environment of I/Q (own uid, [M3]); can see the file's name and mtime (`0755` dir); the sudoers regex does not reach `/etc/motel-backup/`. | nothing new | nothing |

Rules 1–6 of `PLAN-backups.md` §1.2 hold: root reads no app data and nothing of uid `motel`'s; no operator string reaches argv but the repository URL, which goes into the file, not to a unit; every unit is `ExecStartEx` + `no-env-expand` through `unit.Runner`.

## 2. Decided by me, the owner's to overturn (each goes in the PR body under "For the owner", and in PLAN-backups.md)

1. **`/etc/motel-backup` is `0755` root, the file `0600` root.** `status` needs no root (owner, 4a) and decides "set up" by `Lstat` of the file; a `0700` directory would make every unprivileged `status` exit 3. Cost: any account sees the file's name and mtime, not its bytes.
2. **The old path is named, never read.** With the new file missing and `/etc/motel/backup.env` present, `run`/`restore`/`drill`/`status` say so and name `setup`. No fallback: nothing released reads the old path, and reading it would keep the sudoers hole open. A box set up by hand on this branch stops until `setup` is run — the one thing that works today and would not.
3. **The repository URL is `setup`'s one argument**; the secrets are asked on `/dev/tty` with echo off; with no terminal `setup` refuses before anything is touched. No `--password-file` / `--credentials-file` in 4b (for the owner: a non-interactive path for provisioning tools).
4. **Schemes.** `s3:`, `b2:`, `azure:`, `rest:` are asked for by their two variables (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `B2_ACCOUNT_ID`/`B2_ACCOUNT_KEY`, `AZURE_ACCOUNT_NAME`/`AZURE_ACCOUNT_KEY`, `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD` — measure that 0.18 reads the last pair; else `rest:` asks nothing and a URL with credentials in it is refused). `gs:` and `swift:` (a credentials *file*, or a dozen `OS_*` variables): `setup` refuses, saying to write the file by hand; the run accepts them as today. `sftp:` (D2), `local:`, `rclone:` (owner), a bare path: refused, before any prompt. Only `s3:` is measured against the fixture.
5. **The generated password**: 32 bytes from `crypto/rand`, base32 lower-case without padding (52 characters); shown once; the operator types `stored` to go on (as a restore takes the app's name — not `y`). Anything else is a no, and nothing has been written.
6. **A clock of 2 minutes on each of setup's two restic units** (I, Q), with "still waiting for <url> (Ctrl-C is safe: nothing has been written)" after 30 s. [M13]: `init` answers in 2–3 s either way; a host that does not resolve is what the clock is for. When it ends the unit is stopped and confirmed gone, the temp file removed, the working file untouched. A new bound, on a new command only.
7. **A working setup is replaced only after the repository answered**: the new file is written beside it under a temp name and renamed over at the end; on any failure the temp file goes and the old file is byte-identical. Failure mode 4 holds: the password is shown and confirmed stored *before* the temp file exists, so an interrupt anywhere leaves either nothing at the final path or a file holding a password that was shown. A SIGKILL between init and the rename leaves the temp file (root-only, in a root-only place); the next `setup` removes any `repository.env.*` first and says so.
8. **The record is put aside** (`status.json` → `status.json.aside-<RFC3339>`) when the record was written against another repository — known by its **id**, which setup keeps beside the record in `/var/lib/motel-backup/repository-id` (root, `0644`, written and fsynced with the aside, put back with it) and compares with the id restic answered — or when there was no old file and a record exists, or when no id is recorded beside it (a file written by hand), or when this setup made the repository. The same repository, by whatever URL: kept. (Was: by URL, with no id in the record — Copilot round 9, the owner: "no fallback; no one is using this code yet".)
9. **`setup` takes the run lock for its whole life** (a run, restore or drill in progress says who holds it; a run that comes due while an operator is at the prompts says busy — the restore prompt's accepted cost). Each prompt is bounded by `answerWithin` (10 min), then a no.
10. **The account**: `setup` makes `motel-backup` when it is missing (`useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin`), and leaves one that exists alone (shell and home not checked: 4c's postinstall is the owner of that line). The e2e box no longer makes it by hand: the setup suite runs first and makes it.
11. **Values `setup` writes**: plain `KEY=value`, never quoted; a value with a newline, a control character, leading/trailing whitespace, or that begins with `"` or `'` is refused at the prompt, naming the character (systemd would read it as something else). Generated passwords are always plain.
12. **`status` as root lints the file** (failure mode 3): key whitespace, a key set twice (last wins), a missing `RESTIC_REPOSITORY` / `RESTIC_PASSWORD`, an unknown scheme — printed as `warning:` lines; the exit status is unchanged (the run's own failure shows the effect).

## 3. Measured first (each becomes a pinning test; numbers continue PLAN-backups.md's M-series)

| # | Question | How | Bears on |
|---|---|---|---|
| M39 | `restic init` 0.18 as `motel-backup` in a real unit: with `--json`, what is printed (line shape, `id`), exit 0; on an existing repository: exit and exact stderr wording; wrong S3 secret: exit, time; a host that does not resolve: does it retry, how long | `restic_integration_test.go` (local repo, wording and JSON) + e2e (fixture: new bucket, existing, wrong secret, `nowhere.invalid`) | init's verdict classes; the clock |
| M40 | `restic cat config --no-lock` on an existing repository: right password (exit 0, JSON with `id`), wrong password (exit 12, at once), no repository (exit 10) | integration (local) + e2e | the probe after an existing repository's password is typed |
| M41 | How PID 1 reads `EnvironmentFile=`: `KEY = v` (key trimmed), `KEY="v"` / `'v'` (quotes stripped), a duplicate (last wins), `# comment`, `;`, a trailing backslash, `\` inside an unquoted value, trailing whitespace, `#` inside a value — a unit running `/usr/bin/env` with `StdoutFile` | `unit_integration_test.go` | the writer (what it refuses), the linter (reads as systemd does) |
| M42 | `useradd --system …` in the container: works, the uid range, `id`; `useradd` on an existing name: exit 9 | e2e + integration `ensureUser` | preflight's account step |
| M43 | `script -qec` is in the e2e image (`bsdutils`); a pty echoes what arrives before echo is off (so the secret must be sent after its prompt) | e2e image, first run | the tty scenarios' validity |
| M44 | restic 0.18 reads `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD` | `strings /usr/bin/restic` in the image + docs | decision 4 |
| M45 | `/dev/tty` opened by a process under `sudo` from `script`: readable, echo off/on by `TCGETS`/`TCSETS` (`golang.org/x/sys/unix`, already a dependency — no `x/term`) | e2e scenario 6 | the terminal code |

## 4. Tests first, each seen failing before the fix

Before the command exists, `motel-backup setup x` exits 2 with the usage text; every negative check is paired with one a missing command cannot pass (the refusal's own words, the positive effect) — PR 3's rule.

### 4.1 `backups/envfile/envfile_test.go` (pure, new package)
- `Parse` reads as systemd does (from M41): trimmed keys, quotes stripped, last assignment wins, comments and blanks skipped; returns values **and** findings (a key with whitespace around it, a key set twice, a line that is not `KEY=value`).
- `Write` refuses: a newline, a control character, leading/trailing whitespace, a leading quote, an empty key; a key not matching `^[A-Za-z_][A-Za-z0-9_]*$`.
- Round trip: what `Write` writes, `Parse` reads back byte-for-byte equal values; and (integration, M41) a real unit's `env` agrees.
- `Lint(values, findings)`: missing `RESTIC_REPOSITORY`/`RESTIC_PASSWORD`; a scheme outside the six; each finding worded for a terminal.

### 4.2 `backups/engine/setup_test.go` (scripted `box`, as `engine_test.go`)
Events are recorded in order (`terminal` fake: prompts asked, what was shown; `box`: units started; the filesystem: when the temp file and the final file appear). Rows, each with what must never happen:

| Row | Must | Never |
|---|---|---|
| not root (main.go, not engine) | names `sudo motel-backup setup` | — |
| `/usr/bin/restic` or `/usr/bin/sqlite3` or `/usr/bin/moteld` missing | names the package / program, exit 1 | a prompt asked; a unit started |
| manager version < 257 | says which systemd is needed | a prompt |
| scheme `sftp:`, `local:`, `/srv/x`, `rclone:`, `gs:`, `swift:`; a URL with `user:pass@` | refused with the reason, before any prompt | a prompt; the file touched |
| the plan unit fails | the adapter's reason (cleaned), exit 1 | a prompt |
| the plan holds no app | said ("no app declares a backup yet"), continues | refused |
| a run holds the lock | `ErrBusy` with the holder | a unit |
| leftover `repository.env.*` temp files | removed first, said | left |
| fresh: key id, secret, password shown, `stored` typed | order: show → `stored` → temp file exists → I → rename; the final file holds exactly the four keys, `0600`; I's spec: `User: motel-backup`, `EnvironmentFile: <temp>`, network, no capability, `Argv = [restic, init, --json]` | the temp file before `stored`; the final file before I exited 0; the password in any unit's argv/environment/Spec |
| `stored` not typed (anything else / empty / ctx cancelled / 10 min) | stops, nothing written, says so | a unit |
| I exits 0 with an `initialized` line | "repository ready", the id shown short | — |
| I exits 1, stderr "already initialized" | asks for the repository's password (echo off), rewrites the temp file with it, Q runs; Q exit 0 → rename; the generated password is nowhere | the generated password in the final file; a second generated password |
| Q exits 12 | "this password cannot open it", temp file removed, old file byte-identical (or still absent) | the final file changed |
| I exits 1 (other: wrong secret, wording cleaned) / 10 / 11 | class named, temp removed, old file byte-identical, no unit left | "no repository" for an exit 1 [M15] |
| I never ends (box holds) | after the clock: `Stop` called, confirmed gone, said; temp removed | a rename |
| ctx cancelled during I | `Stop` called, temp removed | — |
| `repository-id` beside the record ≠ the id restic answered (or none recorded) | `status.json` moved to `status.json.aside-<t>`, said | the record kept |
| old file same URL | record kept | moved |
| no old file, record present | moved | kept |
| account missing | `useradd` argv exactly as decision 10, said "made" | `useradd` when present |
| the secret typed with a newline/leading space | refused at the prompt naming the character, asked again (up to 3 times) | written |
| dirs | `/etc/motel-backup` `0755` root made; state/run dirs as `begin` | `0700` on the etc dir |

### 4.3 `backups/engine/*_integration_test.go`, `backups/unit/unit_integration_test.go` (real systemd, root)
- M39/M40 pins: `TestIntegrationResticInitSaysWhenTheRepositoryExists` (exit 1 + the exact phrase; `--json` `initialized` line; `cat config` 12/10/0).
- M41 pin: `TestIntegrationSystemdReadsAnEnvFileAsEnvfileDoes` — one file with every shape, a unit running `/usr/bin/env`, `envfile.Parse` of the same bytes agrees with what the unit saw.
- The whole `Setup` against a real manager needs a repository; `setup` refuses `local:` and gets no test-only hook, so the real-manager path is e2e's.
- `ensureUser` gains `--home-dir /nonexistent` (matches decision 10; M42).

### 4.4 `e2e/backup/setup.sh` — a fourth suite, run **first** (setup → run → status → restore)
Own bucket `setuprepo`; `write_env` gains `mkdir -p /etc/motel-backup`; `ENVFILE=/etc/motel-backup/repository.env` in `lib.sh`. `tty() { script -qec "$*" /dev/null; }` (M43). Ends `ALL SETUP SCENARIOS PASSED`.

| # | Scenario | Pairing / guard |
|---|---|---|
| 0 | usage: `setup`, `setup a b` → exit 2, usage text | — |
| 1 | as `nobody`: refused, names `sudo`, nothing made | guard: `/etc/motel-backup` absent after |
| 2 | no terminal (`compose exec -T`): refused, says a terminal is needed; nothing written, no unit | output holds "terminal", not the usage text |
| 3 | account absent (the Dockerfile no longer makes it: `! id motel-backup` asserted first); `restic` moved aside → names the package, no prompt; `restic` back | guard: the prompt text absent from the capture |
| 4 | `sftp:`, `/srv/backups`, `local:/x`, `rclone:r:b`, `gs:b`, `s3:http://u:p@h/b` → each refused before any prompt; nothing created at `/srv/backups` | the refusal names the scheme |
| 5 | the Caddyfile with `root {$LIVESWAP_ROOT}` (no default): refused, names the variable, no prompt; Caddyfile restored | guard: `moteld validate` accepts that file |
| 6 | **fresh setup at a tty**: key id (echo on), secret (sent 3 s after), `stored`; capture shows the key id and not the secret; the password line is shown; the account exists (`getent`, shell `nologin`, home `/nonexistent`); `/etc/motel-backup` `755 root`, the file `600 root`, exactly four keys; `rr snapshots` answers (repository exists); no temp file; no unit left; `status` → `pending first run`, exit 0; `run` → both apps `ok` | guard: the key id shows (else the echo check is vacuous) |
| 7 | old-path file: write a `/etc/motel/backup.env`, remove the new → `run` and `status` say not set up, name the new path **and** say the old one is not read; remove it | — |
| 8 | **mistakes over a working setup** (file's sha256 before): wrong secret on a new bucket → answers < 60 s, class said, file byte-identical, no `repository.env.*`, no unit; wrong password on the existing repository (rebuilt-box path: same URL, `stored` never asked) → "cannot open it", same invariants | guard: the capture holds "already exists"/asks for the password |
| 9 | **rebuilt box**: `rm` the file; setup on the same URL → asks for the repository's password → the saved one → file holds it (`grep ^RESTIC_PASSWORD=`), "repository ready"; no second password was generated (the `stored` prompt absent) | — |
| 10 | **interrupted**: setup started under `script` in the background, input stops after the password is shown (no `stored`); `kill -INT` → nothing at the final path, no temp, echo restored (`stty -a` on the same pty shows `echo`) | guard: the password line was shown before the kill |
| 11 | **killed mid-init**: `hold_restic init` (`kill -STOP`), `kill -KILL` setup → the unit runs on (no `BindsTo` from a shell) → the next `setup` sweeps it (units file) and removes the temp file, says so; `nothing_left` | guard: `units_running != 0` before the second setup |
| 12 | **a host that does not resolve** (`s3:http://nowhere.invalid:9/x`): "still waiting" after ~30 s, then at 2 min "did not answer", unit gone, file byte-identical (≈2 min of lane; keep if the job stays well under 30 min, else Ctrl-C at 35 s) | guard: the unit existed while waiting |
| 13 | **switch of repository** after `run` + `drill` on `setuprepo`: setup onto `setuprepo2` → `status.json.aside-*` exists, `status` → `pending first run`; `run` → drills into the new one (`restore proven` names a snapshot `rr snapshots` on repo 2 holds) | guard: the proof id is not in repo 1 |
| 14 | `status` as root on a hand-edited file (` RESTIC_REPOSITORY = x` + a duplicate key): two `warning:` lines, exit unchanged; as `nobody`: no warning, same exit | — |
| 15 | setup while a run holds the lock (`kill -STOP` a run's restic): "another backup run is in progress", exit 1, no prompt; released | — |

Deliberate breaks in one build (lesson 5), each caught by a named check: the temp file written before `stored`; the rename before I's verdict; the generated password kept on "already initialized"; the clock not stopping the unit; the record not put aside; the old path read as a fallback; echo left off.

## 5. Then the code

- **`backups/envfile/`** (new, stdlib): `Parse([]byte) (Values, []Finding)`, `Write(dir, name string, kv []Pair) (tmpPath, error)` → temp in the same dir, `0600`, fsync; `Commit(tmp, final)` rename + dir fsync; `Refuse(value) error`; `Lint(Values, []Finding) []string`. Modelled on `record.Write` (temp, chmod, fsync, rename, dir fsync).
- **`backups/engine/setup.go`** (new): `type SetupOptions{Repository string; Terminal Terminal}`, `type Terminal interface{ Ask(ctx, prompt string, secret bool) (string, error); Show(string) }`, `func Setup(ctx, cfg Config, r Runner, o SetupOptions) (*SetupReport, error)`. Steps: preflight (programs, manager version via a new `unit.ManagerVersion`, scheme, account) → `beginSetup` (`begin` without the env-file check: split `begin` into the check + the rest) → sweep `repository.env.*` → plan (`x.plan`) → prompts → generate → show → `stored` → temp file → I (`x.start` with role `init`, clock via `context.WithTimeout`, the 30-s line through `Terminal.Show`) → verdict → (existing: ask password → rewrite temp → Q) → record aside → rename → report. Roles `init`, `probe` fit `unitNameRe`. One `resticSpec(role, envFile, out, err)` for I and Q; the three existing restic Spec literals are **not** refactored (one feature, no cleanups) — only the env-file path they take changes, through `Config`.
- **`backups/engine/engine.go`**: `Config.OldEnvFile` (named in `begin`'s message, never read); `begin` split so setup can skip the check.
- **`backups/unit/`**: `ManagerVersion(ctx) (int, error)` (the manager's `Version` property; unprivileged).
- **`backups/cmd/motel-backup/main.go`**: `setup <repository>` (root check as `runner()`), `usage`, `arguments`; `config()` → `EnvFile: "/etc/motel-backup/repository.env"`, `OldEnvFile: "/etc/motel/backup.env"`; `tty.go`: `/dev/tty` opened `O_RDWR`, echo off/on by termios (`x/sys/unix`), restored on every path (defer + ctx), a read goroutine selected against ctx and `answerWithin`; no tty → error before anything. `showStatus`: when euid 0, read + `envfile.Lint` → `warning:` lines.
- **e2e**: `e2e/backup/setup.sh`; `lib.sh` (path, `write_env` mkdir, `tty`); `docker-compose.yml` mount `/suite-setup.sh`; `Makefile` `e2e-backup` runs it first; `e2e/backup/Dockerfile` drops the hand `useradd` (setup makes the account; the comment says so).
- **Docs**: `backups/README.md` — intro (`setup` exists; the new path everywhere), a `## Setup` section (what it asks, the order, what it refuses, the clock, the rebuilt box, switching repository, the `stored` step, interrupts), "Who runs as what" gains init/probe rows, "What it refuses" gains the setup refusals, "By hand" shrinks to the file's format (for `gs:`/`swift:` and provisioning tools) and loses the `useradd`; `examples/box/README.md`: one line that the backup credential is not under `/etc/motel` and not the administrator's to edit. Every claim traced to a line of code before it is written.
- **`PLAN-backups.md`**: M39–M45 in §0; §1.4/1.5 rows for the new path and setup's units; "PR 4b — what the owner decided, and where it stands" with decisions 1–12, the tests, the lanes, "For the owner"; D6's path updated to the new one.

## 5b. What the measurements changed (2026-09-26)

- M39: `restic init` **does not retry** — a wrong key, a host that does not resolve, a closed port, HTTPS to HTTP each answer at once; a black hole after 30 s. The clock stays (2 min, the note at 20 s) for a storage that takes the connection and never answers, which the e2e stages with `systemd-socket-activate -l 127.0.0.1:9999 /bin/sleep 3600`; `init` is what tells that a repository exists, whatever the password, so a rebuilt box's setup shows a password it then does not use (decision 13 in PLAN-backups.md).
- M44: Debian's restic has **no `azure:`** — refused by name. `rest:` takes `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD`.
- Decision 8 as written makes "no file before, same URL" put the record aside; e2e 7 (the rebuilt box) asserts that, and runs again to have a record for e2e 9 to put aside on the change of repository.
- The plan unit's stderr goes to a file for setup only (`planWith`), so the adapter's reason is shown at the terminal; a run keeps it in the journal (the adapter quotes the Caddyfile, and the record is everyone's to read).
- The e2e suite has 13 scenarios (the plan's 16 folded: the switch of repository is 9 + 10; the clock and Ctrl-C are 11).

## 6. Verification, in order

1. Every new test seen failing first (no command: usage; then the real failures), the deliberate breaks, then green: `make test` · `make test-integration` · `make e2e-backup` (four suites; job time noted against `timeout-minutes: 30`) · `make e2e` (box-push) · `make lint` (backups) · `make secretscan` — fresh containers, the way CI does.
2. `/code-review xhigh` on the branch; each finding checked against the code; the real ones fixed with a test seen failing first; lanes again.
3. Push `backup-setup`, open the PR into `backup` in #146's style: what it does · measured (M39–M45, with the pinning tests) · new limits · decided · **For the owner** (decisions 1–12 that could bite: the old path not read; the `0755` directory; the clock; the scheme set; no non-interactive path) · open · how it was tested · lanes at the head commit. Copilot requested.


## Invariants, each a table test (2026-09-26, the owner's "sort it out")

Fourteen review rounds fixed findings at the site pointed at, each with a test of that one case, and every round's fixes seeded the next round's findings. The invariants behind them are now tables in `backups/engine/setup_invariants_test.go`, and the rule is: **a finding that fits a table is a row added, never a test of its own.**

| Invariant | Table | Rows |
|---|---|---|
| Whatever leaves init running leaves it what it needs: not stopped, recorded, staged file and run directory kept, "keep" said | `TestWhateverLeavesInitRunningLeavesItWhatItNeeds` | its clock · an interrupt · a runner that lost sight of it · a stop it could not confirm |
| Every lock holder waits for an init left running, then sweeps its staged file, run directory and marker | `TestEveryLockHolderWaitsForAnInitLeftRunningThenSweeps` | setup, a run, a drill × a box with a credential file, one with the staged file alone |
| A power cut at any point of the commit leaves a state the next setup reads rightly: only a directory's sync makes its writes durable; a record written against the old repository is never kept for the new | `TestAPowerCutAtAnyPointOfTheCommitLeavesAStateTheNextSetupReads` | every durable state the hooks see, each on a fresh box, a run's record between |
| Every exit after the showing says one of three fates, from what setup knows: made (certain), ran (the rest), never used (certain) | `TestEveryExitAfterTheShowingSaysOneOfThreeFates` | each failure point × made / ran / existed-and-own-password |

**Before a push that touches unit lifecycle, the commit sequence, or the fate:** the tables (`make test`), lint, `make test-integration` when a pin changes, and **`make e2e-backup` locally** — CI's arm lane found the staged-file race that the amd64 lane and the unit lane could not. Review listings are paginated: read every page.
