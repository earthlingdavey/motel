# Backups for motel — briefing

**What this is:** the facts, the owner's decisions, and the pitfalls that bear on
adding backups to motel. **What this is not:** a design. It names no units, no
commands, no files, no data model and no architecture. Where a fact forces a
choice, only the fact is given. The design is yours.

Everything marked *(measured)* was observed on Debian 13 (systemd 257.13,
restic 0.18.0, sqlite3 3.46.1) in a systemd-booted container, or in CI.
*(not measured)* means reasoned or taken from documentation, and should be measured
before it is relied on.

---

## 1. The goal, as the owner states it

- A solo developer running motel on one box keeps SQLite databases and uploaded
  files under each app's `shared/` dir. Nothing backs them up. A live SQLite file
  cannot be copied safely with `cp`, which is the part people get wrong.
- The bar for DX: **two/few config lines per app and the operator never thinks about
  backups again — and something says out loud whether a restore would work.**
- Restore matters equally to backup. "A backup nobody has restored is a hypothesis."

## 2. Constraints

- **Debian 13 only.** One support tier, no version detection.
- **One box per operator**; scaling is vertical. The realistic second box is a dev
  box beside production.
- restic and sqlite3 are **Debian's packages** (restic 0.18.0 — 0.19 is in
  forky/sid only). kopia, rustic and bupstash are not in Debian.
- **polkit is not installed** on a minimal Debian 13 (systemd only Suggests it).
- moteld runs as the `motel` user; every app runs as that same uid, each in
  its own sandboxed transient unit under that user's systemd manager. motel's
  admin API is a unix socket that Caddy makes writable by its owner alone.
- What an app declares is config in the Caddyfile, and the admin API serves config
  **as loaded, not as provisioned** (liveswap's default `root` and `{env.*}`
  placeholders are not resolved in what the API returns). The process answering
  that API is the first thing an attacker on the internet reaches.
- The package does not start moteld on install (`try-restart` only).
- The project's rules for any PR: doc claims are traced to code before they are
  written; comments are present tense (history lives in git); new limits that could
  fail a legitimate operation are discussed first; one PR per feature; no
  cross-subsystem cleanups.
- Prefer work to be a module e.g. in ./backups

## 3. Facts

### 3.1 SQLite

- A live database cannot be copied safely by file copy; `VACUUM INTO` of a live
  65 MB database under ~200 writes/s took 290–390 ms with integrity intact every
  time *(measured)*.
- **SQLite cannot read a WAL-mode database from a read-only mount at all** (error
  14): it has to create `-shm` beside the database. Whatever reads one needs the
  directory writable *(measured)*.
- `sqlite3` run **as root** on a WAL database it is first to open leaves
  root-owned `-wal`/`-shm`, and the app can then no longer open its own database
  *(measured)*.
- `.restore` (SQLite's backup API) into a **live** database works with a writer
  running, as one transaction; the app's writes wait *(measured)*. In
  rollback-journal mode (SQLite's default) a reader and a writer exclude each
  other, so a copy and a restore both wait on the busy timeout *(measured)*.
- `sqlite3` defaults to failing the moment a database is busy; it needs an
  explicit timeout *(measured)*.
- `PRAGMA integrity_check` reports a damaged copy in two ways: prints what is
  wrong and exits 0, or — a page it cannot read at all — says "malformed" on
  stderr and exits 1 *(measured)*.
- A `sqlite3` that is **killed** leaves `-wal`/`-shm` behind until some connection
  next closes cleanly. **`select 1` does not open the database file**, so it
  clears nothing; a query that reads a table does *(measured)*.
- `sqlite3` opening a **named pipe** blocks waiting for a writer *(not measured —
  standard FIFO behaviour)*.
- `VACUUM INTO` refuses to write to a file that exists, and needs free disk space
  for a full copy; it fails with "database or disk is full" otherwise.

### 3.2 restic 0.18

- Hourly full copies of a 65 MB database deduplicate to ~1 MB each; peak RSS
  ~77 MB *(measured)*.
- **restic writes a snapshot even when it exits non-zero** (exit 3: some sources
  unreadable). A snapshot existing is not evidence a backup worked *(measured)*.
- restic stores a **symlink as a symlink** — it has no option to follow one. A
  path that is a link to a data disk backs up the link and none of the data,
  successfully *(measured)*.
- restic records **absolute paths**, so a snapshot taken under one liveswap root
  does not line up with a box using another *(measured)*.
- `restic backup --json` prints a summary with the `snapshot_id`. "The newest
  snapshot" is not the same thing whenever the clock has stepped or another run
  interleaved.
- `restic ls <id> <dir>` lists **direct children only**; listing a directory
  itself returns one line per entry in it, listing its parent returns a handful
  *(measured: 2 lines vs 501)*.
- `restic restore <id>:<path>` **refuses a single file** ("not a directory");
  `dump` works for one *(measured)*.
- **Exit statuses** *(measured; `systemd-run --wait` passes them through)*:
  10 = no repository there, 11 = repository is locked, 12 = wrong password,
  3 = incomplete backup. **1 means "anything else"** — `check` exits 1 both for
  damage it found **and** for a repository it could not reach (a host that does
  not resolve). `init` on an existing repository exits 1 with backend-specific
  wording.
- **`restic forget` exits 0 when the storage refuses the delete**, and says so only
  on stderr *(measured against an append-only rest-server)*. Whether a delete
  happened can only be read from the repository.
- Asked to open a repository in a **bucket that does not exist**, restic retries
  "The specified bucket does not exist" for ~15 minutes; a **wrong S3 secret** is
  also retried for many minutes (still running at 10). `restic init` answers in
  ~3 s either way *(measured)*.
- **Locks** *(measured)*: `check` takes the repository **exclusively**. `backup`,
  `ls`, `snapshots`, `restore`, `dump` take a non-exclusive lock unless
  `--no-lock`. restic **does not wait** for a lock: the second of a check and a
  backup fails at once with exit 11. `--retry-lock <duration>` makes it wait and
  succeed. **restic 0.18 does not read this from the environment**
  (`RESTIC_RETRY_LOCK` is ignored). With `--quiet`/`--json` it logs nothing while
  it waits. No stale locks were left in any test.
- **`restic forget` applies a policy per group of (host, paths).** Snapshots
  written with `--stdin-filename X` all have path `/X`, so every snapshot sharing
  that name is one group *(measured: two apps' worth, four hours, one shared name
  — the commonly documented `--keep-hourly 24 --keep-daily 30 --keep-monthly 12`
  removed three of the first's four and none of the second's)*. `forget` keeps the
  **last** snapshot of a period regardless of how its run ended.
- `--stdin-filename` **with a slash in it fails** ("open /x: no such file")
  *(measured)*. `--stdin-from-command -- echo …` makes a snapshot without writing
  a file anywhere.
- `restic snapshots --latest N` groups by (host, paths) too.
- restic needs a **writable cache dir** or it warns on every command and re-reads
  the repository index each run; run as another user with root's `HOME` (what
  `sudo` leaves) it prints "unable to open cache" and runs uncached *(measured)*.
- restic 0.18 **restores owners by number**. Inside a user namespace that maps
  only root and one user, `lchown` to an unmapped uid fails with **EINVAL**, which
  restic does not overlook the way it overlooks EPERM: it exits 1 over files it
  did restore. That is every file, on a box rebuilt with a different uid for the
  `motel` user. restic also reports a file it **never wrote** as an `lchown`
  error (another errno), and its closing summary counts that file as restored
  *(measured)*. restic 0.19 has `--ownership-by-name`.
- **restic is not lock-free**, so bucket-wide S3 Object Lock breaks it (it must be
  able to delete its own lock files). Kopia supports Object Lock natively.
- restic has **no hooks** (only `--stdin-from-command`).
- ssh takes its key and `known_hosts` from files in a home directory, and rclone
  its remotes from a config file: neither backend takes its credentials from the
  environment. `s3:`, `b2:`, `rest:`, `azure:`, `gs:` and `swift:` do.

### 3.3 systemd 257

- A property set that drops parts of a full sandbox (the base read-only view, the
  private namespaces) makes systemd **fail to set the user at all** (217/USER)
  rather than run with a weaker sandbox *(measured)*.
- `PrivateUsers=yes` alone denies a same-uid process outside the unit access to the
  unit's `/proc/<pid>/environ` and `/proc/<pid>/root`. **Without it, the
  `motel` uid can read the environment — and any credentials in it — of any
  unit running as `motel`** *(measured both ways)*.
- `PrivateUsers=identity` does not parse on 257.13.
- **`systemd-run` expands `$VAR` / `${VAR}` in the command's arguments from the
  unit's environment** by default — including values from `EnvironmentFile=`.
  `--expand-environment=no` turns it off *(measured: an argument of
  `${RESTIC_PASSWORD}` reached the process as the password)*. systemd also
  collapses `$$` to `$` in `ExecStart=`.
- `EnvironmentFile=`: systemd strips surrounding quotes, **trims whitespace around
  the key**, and takes the **last** assignment *(measured: `KEY = second` after
  `KEY=first` yields `second`)*. It is read by PID 1 as root before dropping to
  `User=`, so the file can stay root-only. systemd has no writer for the format.
- `BindPaths=` / `BindReadOnlyPaths=` values are split on **whitespace** and
  **colons** (`source:destination`). A `-` prefix skips a missing source; the unit
  still starts and the path is simply absent *(measured)*.
- **`StateDirectory=` / `RuntimeDirectory=`** work inside a full sandbox; systemd
  makes the directory, gives it to `User=`, and **refuses to start a unit whose
  state directory is a symlink**, leaving the target untouched. It does **not**
  re-chown the inside of a state directory whose top-level owner already matches
  *(measured)*.
- `PrivateNetwork=yes` works inside the full sandbox *(measured)*.
- **`Type=oneshot` units are `activating` for as long as their command runs**;
  `systemctl is-active` exits 3 for them throughout *(measured)*.
- `systemd-run --wait` returns the unit's exit status — **unless
  `SuccessExitStatus=` lists it, in which case it returns 0** *(measured)*. A
  non-zero exit is logged by systemd as the unit failing; with `--collect` the
  failed transient unit is garbage-collected.
- `systemd-run` resolves a **relative command on the caller's PATH**, before the
  unit exists; a unit's own PATH is systemd's default
  (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin`).
- **`ExecStopPost=` runs after every exit, a clean one included** *(measured)*.
- **A context timeout or Ctrl-C on `systemd-run --wait --pipe` kills only the
  client.** The unit belongs to PID 1 and runs on; through `--pipe` it holds the
  caller's stdout/stderr, so a caller waiting on those pipes waits for the unit —
  and any cleanup placed after that wait never runs *(measured: a command hung
  >10 minutes, SIGTERM included)*. Go's `exec.Cmd.WaitDelay` bounds the pipe wait.
- In Go, a `select` over "the work finished" and `ctx.Done()` may take either when
  both are ready — and a cancelled context is also what ends the work.
- Transient units are outside the cgroup of whatever started them: stopping the
  starter does not stop them. Two transient units cannot share a name at once.
- **The right to create a transient system unit is root-equivalent**: a root
  process with an empty capability set, `ProtectSystem=strict` and
  `PrivateNetwork=yes` can still start a uid-0 unit that reads `/etc/shadow`
  *(measured)*. A root process with no capabilities can still talk to systemd.
- The `motel` uid gets "Access denied" starting or stopping system units.
- A unit with `ProtectHome=yes` and `CAP_DAC_OVERRIDE` sees a directory under
  `/home` as **"No such file or directory"**, not as inaccessible *(measured)*.
- Connecting to motel's admin socket as root needs `CAP_DAC_OVERRIDE` (the
  socket is owner-write-only, in a directory that is `motel`'s) *(measured)*.
- A file masked by `InaccessiblePaths=` still exists to `stat` (regular, mode 0,
  empty), reads as nothing even with `CAP_DAC_OVERRIDE`, and a unit started *from
  inside* such a unit still receives the real `EnvironmentFile=` values
  *(measured)*.
- A timer with no `[Install]` section can follow another via `Wants=` on that
  timer plus `PartOf=` on itself: started and stopped with it *(measured)*.
- System users log to the **system** journal; reading it needs
  `SupplementaryGroups=systemd-journal`.

### 3.4 Debian packaging

- `dh_installsystemd`'s own lines (`deb-systemd-helper`
  unmask/was-enabled/enable/update-state, mask on remove, purge+unmask on purge)
  carry a unit's enabled state correctly through upgrade, disable, remove,
  reinstall and purge. `deb-systemd-invoke start` does not start a disabled unit.
- In a Docker image, the container's policy holds back the start that goes with
  enabling at install *(measured in the package smoke test)*.
- `Recommends:` packages are left out by `apt install --no-install-recommends`.
- dpkg replaces a binary in place: a process already running keeps the old one;
  anything started afterwards by path gets the new one.
- `apt purge` removing a file that holds the only copy of a secret is silent
  unless the maintainer script says something.
- `make install-test` reuses a stale `dist/` — run `make package` first.

### 3.5 Storage providers

- Backblaze B2: removing a file by name only **hides** it; destroying a version
  needs `deleteFiles`. A key with `listBuckets, listFiles, readFiles, writeFiles`
  is what restic's append-only support for B2 is built on (restic#2398). Keys can
  be scoped to a bucket and a file-name prefix. B2's web console offers presets,
  not individual capabilities; the `b2` CLI sets them. *(From documentation and
  memory; nothing here has been run against real B2.)*
- Hetzner Object Storage: bucket policies yes, Object Lock governance-only, keys
  project-wide by default *(from documentation)*.
- **Predicted, not measured:** with a B2 no-delete key, a `forget` that succeeds
  by *hiding* would make any "can this key delete?" probe answer "yes" for exactly
  the key one would recommend.
- Bucket lifecycle rules that expire objects by age corrupt a deduplicating
  repository (a pack judged old can hold the only copy of a current chunk).

### 3.6 Test fixtures and CI

- **An S3 fixture that works:** `rclone serve s3`, the static binary copied from
  rclone's image (pinned by digest) into `FROM scratch`; verified with restic
  including multipart and `check --read-data`. One key, which may do anything; it
  creates a bucket when asked; tmpfs data counts against its memory limit.
  Unsuitable: MinIO (community edition archived, unpatched CVE), SeaweedFS
  (accepts a Deny policy and does not enforce it), moto, Debian's rclone 1.60 (no
  `serve s3`). The Debian archive has no S3 server.
- A rejected S3 request cannot be asserted with restic (it retries for minutes);
  `curl --aws-sigv4` answers at once.
- `docker cp` into `/tmp` of a systemd container lands under the tmpfs and is
  invisible; copy to `/root`.
- The e2e job on main takes ~230 s (amd64), about half of it building the stack. A
  suite appended to it costs its own length; a suite run as a parallel job costs
  its length plus the build.
- gitleaks reads every commit: a fixture that looks like a credential needs an
  allowlist entry by text, and `make secretscan` scans nothing in a git worktree.
- The Claude Code tools unescape `\u` sequences in any input, even in Go raw
  strings.

## 4. Decisions the owner has made (and why)

- **No local-filesystem repositories.** A backup on the box it protects does not
  survive losing the box; and a repository on disk would have to be reachable,
  writable, from whatever backs the apps up.
- **No `rclone:` backend.**
- **Hourly, not continuous (for now).** Litestream was evaluated: run inside an app's release
  lifecycle it **corrupted its replica** during deploy overlap (two instances);
  as a separate host unit it was clean. A possible later tier, not this feature.
- **Retention is not the box's job.** `forget` deletes, and the box's key should
  not be able to; pruning is done off the box with a second key.
- **No time or memory cap on a backup.** Measured: a first backup killed at
  240 MB of 763 MB re-sent all 763 MB on the next run, so a cap means a large
  first backup never completes.
- **No per-app manifest and no second config format**; what an app declares lives
  in its Caddyfile block.
- **Never part of the serving process**; nothing of a backup may be able to touch
  traffic.
- **Shell out to the distribution's restic and sqlite3**; never link them.
- **One repository per box, in its own bucket, with its own key and password.**
  No multi-box safeguards in code: the operator runs one box. Paths inside a
  shared bucket are deliberately not recommended (untested, and quietly wrong
  with a bucket-wide key).
- **Bucket-policy enforcement is the provider's job** and is not tested.
- **Per-file or per-job extraction caps are rejected** elsewhere in the project
  (#126) — do not propose them.
- **Monitoring and alerting are out of scope** for this feature.
- **The repository itself is to be verified periodically**: its structure plus one
  fifty-second of the data per week(?) (the whole repository once a year, ~2% of its
  size in downloads weekly).
- **No follow-ups**: everything the feature needs lands with it.

## 5. Pitfalls

### Process

- **Settle every privilege boundary before any code**: who runs as what, who holds
  the credential, who may write where, what root consumes and from whom. Written
  as a table, it is a review tool; decided one finding at a time, it is rework.
- **Measure third-party behaviour before relying on it.** Nearly every entry in §3
  contradicts a reasonable assumption.
- **Do not let review be the design process.** A fix written between review rounds
  gets one round of scrutiny, and the next round finds what it introduced.
- **Write the failure-path end-to-end tests first.** Happy-path suites pass over
  timeouts that never fire and states that are never observed.
- **Keep the PR small.** Declaration, backup, setup, status, restore, packaging and
  verification are several features.
- **Review the whole change, not the latest commits**, with named focus areas.
- **Show every new end-to-end check failing once**, against the broken behaviour,
  before trusting it.
- **Never push a fix on a plausible diagnosis**; measure, then fix.
- **A fake that returns `ctx.Err()` proves nothing about a timeout**; only a real
  process that outlives its context does.
- **Run every documented procedure literally**, as the person it is written for
  (fresh box, non-root shell), before it is written down.

### Technical

- Anything that reaches a unit's argv can be expanded by systemd from that unit's
  environment, where credentials may be.
- Stopping units by name pattern when a scheduled run ends also stops whatever an
  operator started under a matching name — and `ExecStopPost=` runs on success.
- A restore that finds nothing to restore, and exits 0.
- A fixed unit name that a legitimately named app can also have.
- A sandbox that makes a path look nonexistent, read as "there is no data yet".
- A generated secret that is shown only at the end of a sequence that can be
  interrupted after the secret has taken effect.
- A unit left running, retrying, after the command that started it was cancelled.
- Records of different apps sharing one retention group.
- A process holding credentials in its environment, running as the `motel` uid
  without a user namespace.
- "The data directory is missing" treated as "never deployed" for an app that has
  been backed up before (deleted data, a volume that did not mount).
- A settings key with whitespace around it overriding the intended value.
- Root running a step by hand and leaving root-owned files where an unprivileged
  job must later write.
- One exit status standing for both "damaged" and "unreachable".
- A read-only listing that takes a lock, and fails while something else holds the
  repository exclusively.
- Plaintext copies of app data left on disk after they have served their purpose.
- "Fresh" computed from a record whose underlying snapshot no longer exists.
- Any error looking at data treated as "the data is not there".
- Special files (FIFO, socket, device) accepted as state.
- A status report that flags a brand-new setup as failing before its first run.
- An error that asks for a secret to be typed and then fails for an unrelated,
  knowable-in-advance reason (a program not installed).
- A log line that says "backed up" when nothing was copied.

## 6. Unverified

- Anything against a **real provider**: TLS, a provider-enforced no-delete key, the
  B2 hide-versus-delete prediction, lock removal with a key that cannot delete,
  a read-only key, a missing bucket.
- `systemd-ask-password` as a prompt mechanism: one spike, inconclusive.
- systemd credentials (`LoadCredential=`) as a way to hand a secret to a unit:
  not measured.
- Behaviour with repositories of realistic size (tens of GB): every timing above is
  from a repository under 1 GB on a local network.
