# PR 4 — setup, status, packaging (motel backups)

Not committed, like `PLAN-backups.md` and `HANDOVER-backups.md` (`.claude/` is not git-ignored here).

## Context

PRs 1–3 and #141 are in `backup`: the declaration, the engine, restore, the drill.
Nothing yet makes the `motel-backup` account, writes the credential file, ships
units or timers, or says out loud whether a restore would work — the e2e box does it
all by hand (`e2e/backup/lib.sh` `write_env`, `e2e/backup/Dockerfile:35`). PR 4 closes
that, as **three PRs into `backup`**. Sources: `PLAN-backups.md` "Brief for PR 4",
`HANDOVER-backups.md`, and the `backup` branch.

## Decided by the owner, 2026-09-20

| Question | Decision |
|---|---|
| Naming | `motel-backup` only; no `motel backup …` shim. |
| Weekly drill | `OnCalendar=Sun *-*-* 03:30`, `Persistent=true`; follows the hourly timer (`Wants=`/`PartOf=`). |
| `status`: a proven restore is "old" | after **8 days** (a drill period + a day). |
| `SystemCallFilter` on the run's service | measure in a real unit; ship only if every lane passes; M38 either way (M37 went to the listing). |
| The credential file | **moves to `/etc/motel-backup/repository.env`** (root-owned directory of its own): the example sudoers' `^/etc/motel/[A-Za-z0-9_-]+\.env$` lets `motel-admin` `sudoedit` `backup.env`, or empty it to `0640 root:motel` (`examples/box/sudoers:37-38`). Closed by construction, old sudoers copies included. |
| The Caddyfile command | **`motel-backup validate <file>`**; the in-unit `check` (`main.go:81`) is not renamed; `check` stays free for PR 5's repository check. |
| Failure mode 38 | **one repository listing per run**; the record gains `seen`; `status` stays a reader of `status.json`. |
| The cut | **4a** `status` + `validate` + `bin/push` · **4b** `setup` + the account + the credential file · **4c** units, timers, packaging, docs. |

**Decided by me, the owner's to overturn** (each goes in the PR body):
1. A backup is **stale after 3 hours** without a complete snapshot (two missed hourly runs and grace) — a new number. Judged from `last_ok`, whatever is in progress (D4).
2. `status` needs **no root**: `status.json` is `0644` by design (`record.go:145`), and what is running is asked of systemd over the bus, read-only.
3. A **proven** snapshot that has since been pruned is *said*, and does not fail `status` (the restore was proven); a vanished **`last_ok`** fails it (pitfall: "fresh" from a snapshot that no longer exists).
4. The state dir stays `0755` as `engine.go:120-127` makes it, with its reason; the brief's `0711` described code that does not exist.

**Found, not PR 4's, for the owner:** D9's warning (an undeclared SQLite file inside a `files` path: loud in the run's log *and* in `status`) has no trace in `backups/` — nothing detects one. `status` cannot show what no run records. A PR of its own against `backup`, or 4a grows a detection step in the dump unit: owner's call before `backup` merges.

## First: `git pull` on `backup` (#141 merged 13:25Z; local is `2c3cf83`), branch `backup-status`.

---

## PR 4a — `status`, `validate <file>`, `bin/push`

### Tests first — all written, and seen failing, before any of the code below

Before the commands exist `motel-backup status` exits 2 with the usage text (`main.go:59`). So every negative check is paired with one a missing command cannot pass (the refusal's own words, or the positive effect) — PR 3's rule.

**1. `backups/status/status_test.go`** — new pure package: `Report(st *record.Status, now time.Time, running []Running, runningErr error) (lines []string, healthy bool)`. One table, modelled on `TestHowAnUploadEnds` (`engine_test.go:394`): each row asserts the exact line, the verdict, **and a negative**.

| Row | Must say / verdict | Must never |
|---|---|---|
| no record yet; and an app `pending` with no snapshot | `pending first run` · healthy (39) | call it failing |
| `ok`, 10 min old, proven 2 days ago | `restore last proven: <when>, snapshot <8 hex>` · healthy | — |
| `last_ok` 3h01m old / 2h59m old | stale, names when / healthy (boundary) | — |
| `incomplete` now, `last_snapshot` new, `last_ok` old | stale: judged from `last_ok` | say fresh from `last_snapshot` |
| `not run`, `last_ok` fresh | healthy, says the run did not reach it | print last run's class as this run's |
| never proven, `restore_drill` says why | not proven, the why, `motel-backup drill` · unhealthy (41) | say proven |
| proven 8d+1m / 7d23h ago | old · unhealthy / healthy | — |
| `last_ok.seen` before `listed` | `no longer in the repository`, last seen when · unhealthy (38) | count it as fresh |
| never listed (`listed` nil), or listing older than `seen` | nothing about vanishing | call a snapshot vanished without a listing that missed it |
| proven snapshot vanished, `last_ok` present | said · healthy | — |
| `data missing`, `failed` | the record's detail · unhealthy | — |
| an upload unit `activating` for 3 h | `running: upload of blog, since <when>` (30) | call an `activating` oneshot failed |
| `runningErr` set | `could not ask systemd what is running: …` | say nothing is running |
| `Status.Error`, `Status.Warning` | both printed | — |
| every app | the full path looked at (`App.Looked`) | — |

**2. `backups/engine/engine_test.go`** — the scripted `box` gains a `listing` role and `b.listing` output.
- a run lists once, after the last app (`b.roles()` ends `… listing`); ids the listing holds get `seen = now` on `last_ok`, `last_snapshot`, `restore_proven.snapshot`; ids it lacks keep their earlier `seen`; `Status.Listed = now`.
- the listing exits 1 / 10 / 12, or names a malformed id: every app's class and the run's exit are as without it, `seen` and `listed` untouched, `Status.Warning` says so — a listing can never turn a good backup bad, nor a missing answer into "vanished".
- `Restore` and `Drill` carry `seen`/`listed` through unchanged (beside `carryLastOK`, `engine.go:235`).
- `record`: round trip with the new fields; a record written before them reads.

**3. `backups/plan/plan_test.go`** — with `fakeMotel` (`plan_test.go:121`): `Inspect` returns the apps that declare **no** `backup` (`extract`, `plan.go:98`, drops them today) and the import files it followed; `Make`'s answers unchanged.

**4. `e2e/backup/status.sh`** — a third suite: own bucket `statusrepo`, run **between** `run.sh` and `restore.sh` (restore 15 renumbers the account, so nothing runs after it); starts from `rm -f "$STATUS"; seed`; ends `ALL STATUS SCENARIOS PASSED`. `craft`, `forget`, `hold_restic` move from `restore.sh:36-91` into `e2e/backup/lib.sh` for both.

| # | Scenario | Pairing / fixture guard |
|---|---|---|
| 0 | no credential file: non-zero, says not set up, names `setup` | output holds `setup`, not the usage text |
| 1 | credential written, no run yet: `pending first run`, exit 0 | — |
| 2 | after a run + drill: exit 0; the id printed is the one `rr snapshots` holds | — |
| 3 | the same as `nobody` (`setpriv`): same lines, exit 0 | guard: that uid cannot read the credential file |
| 4 | `rr forget` blog's `last_ok`, blog's next run made `incomplete`, run: `no longer in the repository`, non-zero; shop healthy | guard: `rr snapshots` no longer lists the id, "or the scenario proves nothing" |
| 5 | `last_ok` set 4 h back, then `restore_proven` 9 days back, in `status.json` (no clock to move in a container; boundaries are the unit table's): stale, then old; the other app untouched | guard: the edit took (`grep` the new time) |
| 6 | a run held mid-upload (`kill -STOP`, `run.sh:186` idiom): `running: upload of blog, since`; exit 0 while `last_ok` is fresh; never "failed" | guard: `units_running != 0` |
| 7 | blog's newest snapshot crafted damaged, `drill`: blog not proven and why, non-zero; shop proven | — |
| 8 | `validate`: good file → 0; an app with no `backup` block → named, exit 0; `root {$LIVESWAP_ROOT:/var/lib/liveswap}` with a `backup` → 1, names the variable; as `nobody` → works; a file that is not there → 1, names it; an import from outside `/etc/motel` → 1 | guard: `moteld validate` **accepts** the `{$LIVESWAP_ROOT}` file; never "no app declares a backup" for a missing file |
| 9 | `status x`, `validate`, `validate a b`, and `check /x` → usage, exit 2 | — |

**5. `e2e/box-push.sh`** (the box suite; `e2e/Dockerfile` also builds `motel-backup`): a Caddyfile `moteld validate` accepts and `validate` refuses → push exits non-zero, says the box rejected it **for backups**, live Caddyfile byte-identical, no `.new` left; with the binary moved aside, push works as before (boxes without backups).

**Shown failing once, several breaks in one run** (lesson 5): freshness read from `last_snapshot`; the `seen` stamp applied when the listing failed; `activating` treated as failed; the 8-day test off by a day; `validate` ignoring the second-value trial.

### Then the code

- `backups/record/record.go` — `Snapshot.Seen *time.Time` (`seen,omitempty`), `Status.Listed *time.Time` (`listed,omitempty`). Snapshots' own `plan.json` is a `plan.Plan`, untouched, so PR 3's strict-decode limit is not widened.
- `backups/engine/engine.go` — `listing`: `history` (`engine.go:993`) without `--tag`, tags `app:<name>` read from the answer; one unit per run, after the apps; failure → `Warning` only. Carry through `restore.go`'s record writes.
- `backups/status/` — `Report` as above; `Running` filled from the bus: `ListUnitsByPatterns("motel_backup_*", "motel-backup*.service")` + `StateChangeTimestamp`, role and app parsed from the name (`x.name`, `engine.go:284`). Reuse `notLastOK` and the `%.8s` / `2006-01-02 15:04 MST` conventions from `main.go:138-177, 270`.
- `backups/plan/` — `Inspect(ctx, file)`; `Make` calls it.
- `backups/cmd/motel-backup/main.go` — `status` (no args, exit 1 with one summary line when unhealthy — the `run`/`drill` pattern, `main.go:129, 365`), `validate <file>` (no `runner()`: no root, no bus, no lock), `usage`, the arity guard at `:59`.
- `examples/box/bin/push` — after `moteld validate` (line 54): `on_box test -x /usr/bin/motel-backup` then `on_box motel-backup validate "$live.new"`; unprivileged, **no new sudoers line**. `examples/box/README.md` says so.
- `e2e/backup/lib.sh`, `docker-compose.yml:153-158` (mount), `Makefile:221` (one `exec` line), `backups/README.md` (`status`, `validate`, the record's new fields, "What a run says").

## PR 4b — `setup`, the account, the credential file (own plan when 4a is merged)

`backups/envfile` (one writer, one linter, systemd's rules: key whitespace, last assignment, no newline) · the move to `/etc/motel-backup/repository.env` (`main.go:99`, `lib.sh:6`, `engine.go:114`'s message; **only `ENOENT` means not set up** — an unprivileged `status` that gets `EACCES` says it cannot tell) · preflight before the first prompt · secrets from `/dev/tty`, echo off (e2e through `systemd-run --pty`; `compose exec -T` has no tty) · show → confirm stored → write → `restic init`, **killed between each step** (`systemd-run --unit` + `systemctl kill`, `run.sh:231-254`) · the plan run first · `sftp:` refused · `status` lints the file when root. Tests first: failure modes 3, 4, 5, 22.

**From a coworker's suite for another design (`helpful/e2e-backup-run.sh`, read 2026-09-20; its `motel backup …` shim, admin-API plan and `restic --` passthrough are not ours) — for 4b's tests:**
- **A switch of repository.** The record speaks of the old one: `restore proven` of a snapshot the new repository does not hold (4a's `status` says it is gone, and does not fail). `setup` onto a different repository puts the record aside, so the next run drills against the repository in use. Test: their `another-repository` — not healthy until a clean run *and* a proof land in the new one.
- **A rebuilt box**: a repository that exists is opened with *its* password (asked for, or from a file — sudo does not carry `RESTIC_PASSWORD`); `setup` never generates a second password for a repository that exists.
- **Mistakes over a working setup** (wrong storage key, wrong password, a local path, `local:`): each answers in seconds, leaves the working file byte-identical, leaves no copy of a credential anywhere, and leaves no unit running — the look for an existing repository is what restic retries for minutes after `init` has failed at once, so it gets a clock and its unit is stopped with it.
- **Quoted values**: systemd strips surrounding quotes from `EnvironmentFile=` values; the linter has to read a quoted `RESTIC_REPOSITORY` as systemd does, and only a real unit shows the two agree.
- **The terminal**: `script -qec '<cmd>' /dev/null` gives a real tty (check `bsdextrautils` is in the image, or add it). Send the secret a moment after its prompt — a pty echoes what arrives before echo is off — and type something with echo *on* first, so "the secret was not shown" cannot pass on a capture that shows no typed input at all.

## PR 4c — units, timers, packaging, docs (own plan when 4b is merged)

`motel-backup.service` (`CAP_SYS_ADMIN`, host mount + PID namespaces, `Environment=MOTEL_BACKUP_UNIT=%n`, `RestrictSUIDSGID=`) + hourly timer; drill service + the Sunday timer · M37 · `nfpm.yaml`: second binary (`Makefile:133-157`, a module of its own: `cd backups`), `recommends: [restic, sqlite3]`, hand-written `deb-systemd-helper` lines (nfpm generates none; no precedent in the repo), a new `postremove` — **purge keeps `/etc/motel-backup/` and says where and why** · `smoke.sh`: is-enabled after install, service not started, disabled survives reinstall, a purge stage, `--no-install-recommends` + preflight · `e2e/backup/Dockerfile:35` and `ensureUser` give way to the real postinstall · `release.yml:42-86` tarballs · docs run literally as a non-root admin; §7 checklist with exact commands; the `forget` recipe and `pre-restore` snapshots.

## Verification (4a)

Fresh containers, the way CI does: `make test` · `make test-integration` · `make e2e-backup` (three suites; check the job still fits `timeout-minutes: 30`, `ci.yml:126`) · `make e2e` (box-push). Every new e2e check seen failing first, then the deliberate breaks above. Then an independent review by focus area (what `status` claims vs what the record proves; the listing's failure paths; `validate` vs what unit P would do; push's rejection path) and `/code-review high`, before pushing; Copilot until clean. `PLAN-backups.md` gets "PR 4 — what the owner decided, and where it stands".
