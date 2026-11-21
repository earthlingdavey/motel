# DESIGN — packaging

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- packaging/DESIGN-packaging.md`);
the "History" section at the end holds only dated one-liners.

The `.deb`'s design is stated beside the code it governs:
`packaging/nfpm.yaml`'s comments (what ships, at which mode, and why
the starter config is not a conffile) and `packaging/postinstall.sh`'s.
This document holds what has no place there: packaging's triage.

## Triage

Findings are triaged by the triage skill (`.claude/skills/triage/SKILL.md`):
its scales, the fix's cost, and the rule. The maintainer scripts
(`packaging/postinstall.sh`, `packaging/preremove.sh`,
`packaging/postremove.sh`) run as root on every install, upgrade,
removal and purge. These are packaging's examples for
the scales:

| Axis | Examples |
|---|---|
| **Severity 5** | the package puts bytes it did not ship where root trusts them, or opens root-only state (the starter's record, the box's key) to another user |
| **Severity 4** | the box's own `/etc/motel/Caddyfile` replaced by the starter, so no push reaches the box; dpkg left half-configured; moteld left down after an install — each until the console |
| **Occurrence 1** | root's own entry in a directory only root can write, where the scripts expect none |
| **Detection 1** | the install fails, or the script says so on stderr, naming the path |

**Accepted residuals:**

| Residual | Where stated | S | O | D | Why accepted |
|---|---|---|---|---|---|
| README.md's "Without the package" says the client in the old archive name "refuses `run` and every other server command", but it answers `version`, which the server has too: a raw-binary host's own update script that checks its install with `motel version` passes on the client | README.md, "Without the package" | 2 | 5 | 1 | 10; the unit's `run` is refused, `unknown command "run"`, at the next start |
| `moteld init` running while `apt install` configures a package whose starter changed, on a box still on the stock starter: postinstall hashes the stock file, init renames the box's file in, and postinstall's rename puts the starter over it | packaging/postinstall.sh (no lock) | 4 | 2 | 3 | 24, kept by rule 4: closing it means postinstall taking the box's lock (size 3, risk 3) for two root acts at the same moment, and a lock it waits on would let a long apply hold dpkg |
| A push that installs bytes identical to the starter postinstall last wrote is replaced by the next starter | packaging/postinstall.sh | 2 | 1 | 3 | 6; a starter has no `box` block, so no push reaches such a box anyway |
| `apt purge` finds something mounted at or under `/var/lib/motel-box`, by either its literal or its physical path (or cannot read `/proc/self/mountinfo`, resolve the path, or finish the scan, or the physical path holds a space, tab, newline or backslash, which the table escapes): the exchange tree is kept whole, so a purge and a reinstall come up on the old `applied.json` and record | packaging/postremove.sh (`mounted`) | 2 | 1 | 1 | 2; root's own mount in a directory the package owns; named on stderr with the remedy (unmount, then remove); the alternative, an rm through the mount, would remove what is not the package's |
| `apt purge` finds a symlink, or anything but a regular file, at `/etc/motel/Caddyfile`: kept, so a reinstall comes up on what it points to | packaging/postremove.sh | 2 | 5 | 1 | 10; an operator's own path (not a fault, so occurrence 5 and detection carries it): named on stderr, and postinstall leaves a symlink alone too, so the box comes up on the file the operator chose |
| `apt purge` while a run of the box applier holds root's lock for longer than the purge waits (30s; a run is seconds): the config and the exchange tree are both kept, so a reinstall comes up on them | packaging/postremove.sh (the lock) | 2 | 1 | 1 | 2; a run that long is a fault of its own (a disk that will not sync), and the purge names both paths and the journal to read; removing under a run would let it put a Caddyfile back after the purge (Copilot on #255, S3 O2 D5 = 30, fixed by taking the lock) |
| The install's `Backups:` line reads the credential file's presence as setup's work, the units' own test (`ConditionPathExists=`): a directory or an empty file root put at `/etc/motel-backup/repository.env` by hand is called set up, and the timers start runs that fail on it | packaging/postinstall.sh (the `Backups:` line, #403) | 2 | 1 | 1 | 2; root's own entry where the scripts expect setup's file; each run names the file it cannot read, and the line itself sends the operator to `motel-backup status`, which says so too; `-f` would print the setup line instead, no more right about timers that run |
| An upgrade of a box with a credential file at the path an earlier version used (`/etc/motel/backup.env`) and none at today's prints the plain setup line, without the note `motel-backup status` adds (which repository to give setup, and to remove the old file) | packaging/postinstall.sh (the `Backups:` line, #403) | 2 | 2 | 1 | 4; a box from before the move, which no release made; the first run and `status` both name the old file and what to do with it; postinstall knowing the old path too is one more copy of what the binary holds |
| `apt purge moteld` keeps the backup credential file as the way to read the backups already made, and a following `apt autoremove` takes `restic` (and `sqlite3`, and `ca-certificates` where only the package brought it) off the box, so reading them needs `apt install restic` first | packaging/nfpm.yaml (Depends, #351); the purge's lines in packaging/postremove.sh name the kept file and not `apt install restic` | 2 | 2 | 1 | 4; two acts of root's, and autoremove lists what it removes before doing so; the same held when the two programs were Recommends (auto-installed alike); `apt-mark manual` from postinstall would leave three packages nobody asked for marked as asked for |
| A `User=` for every service, from a drop-in in `/etc/systemd/system/service.d/` (or `/run/…/service.d/`), survives `systemctl revert moteld`: the upgrade page's check that motel runs as the motel user then still prints `User=root` until root adds the drop-in the page names, one in `moteld.service.d/` whose name sorts after it (systemd.unit(5): drop-ins of different names apply in lexicographic order whichever directory holds them, and a `type.d/` file yields to an equally named one in the unit's own directory) | docs/upgrading.md ("Check moteld.service runs as the motel user") | 2 | 5 | 1 | 10; the check runs before the install and says so, and the file is root's own box-wide edit, which root decides on. /code-review pass 2 on #505 |
| Between the unit check's `systemctl daemon-reload` and the install's restart, a unit edited to run motel as root has `User=motel` while the running process is still root's, so a `systemctl reload moteld` in between (a push whose files need a reload applies with one: `box/systemd.go`, `reloadSwapped` in `box/txn.go`) runs `ExecReload=` (`moteld reload --config /etc/motel/Caddyfile --force`) as motel against the admin socket the root process made (`/run/motel/admin.sock`, in the `RuntimeDirectory=` made for the user the unit had at its start), and may fail, failing the push red. Read from the unit and the code, not run | docs/upgrading.md ("Check moteld.service runs as the motel user") | 2 | 2 | 1 | 4; an edited unit and a push in the minutes between two steps, and the page puts the step just before the install, whose restart ends it |
| A box on a release from before the package was `moteld` (the package `motel`, v0.7.0 and earlier) cannot upgrade in place: `apt install` of the `moteld` `.deb` stops at unpack, naming a file both packages ship. The move is `apt remove motel`, which stops the `motel` user's manager and so every app, then the install, which leaves `moteld.service` disabled until `systemctl enable --now moteld`. The removed package stays in dpkg's `rc` state with its purge script: an `apt purge motel` after that removes `/etc/motel/Caddyfile`, `/var/lib/motel-box`, the starter's record in `/var/lib/motel-package`, and motel-backup's records and cache, and disables `motel-box-apply.path` and both backup timers, which the new package enabled under the same names; the running moteld serves on until its next reload, which fails | packaging/nfpm.yaml (`name:`; no `Replaces`, `Conflicts` or transitional package) | 4 | 5 | 3 | 60, kept by the owner's decision (2026-11-17): the only box on the earlier package is the owner's, so the package carries no code for it; the release's notes carry the move, with the step that drops the earlier package's record without running its purge (both rehearsed in a container from the v0.7.0 `.deb`, 2026-11-17: the move keeps the box's Caddyfile, its drop-ins once their directory is renamed, the timers and the applier; the purge does what this row says). One-way: no `.deb` goes back across the name |

**Owed** (over the bar, not yet fixed): none.

**Kept on purpose, not scored** — `apt purge` leaves `/etc/motel/age/`
(the box's key for secrets), `age.pub` and `/etc/motel/secrets/`,
and names them on stderr, with the `rm -r` that finishes the job
where the key is among them.
Deleting the key is one-way: it is the one way to read the secrets
the box's repository holds, and a purge cannot know whether that is
over. `/var/lib/motel` (Caddy's state) and `/var/lib/liveswap` (the
apps' releases and data) are never removed by a maintainer script;
dpkg removes either only when it is empty, as any directory a package
shipped. The account stays.

## History

Dated one-liners; the full text of each is in git.

- 2026-10-14 — Packaging gets a design doc for its triage: its examples
  for the scales, #239's residuals (the starter config out of the
  conffile, #237), and the purge it owes.
- 2026-10-16 — The owed purge (scored 45) is paid by
  `packaging/postremove.sh` (PR #255): a purge removes the box's
  config, the starter's record and the box applier's exchange tree,
  and masks the path unit on remove the dh_installsystemd way, under
  the applier's own lock so a run under way cannot put the Caddyfile
  back. Three residuals of that script accepted; the key kept on
  purpose. The
  `backup` branch folds its timers' handling into the same file.
- 2026-10-17 — No box from before v0.3.1 exists but the owner's, which
  runs v0.3.1: postinstall stops removing the AppArmor profile packages
  before the Debian-13-only matrix left, and the two residuals of
  taking a box to or from a release with the starter as a conffile go.
- 2026-11-06 — #403: the install's `Backups:` line says of each timer
  only what is so (enabled, enabled-runtime, masked, disabled; the two
  named apart where they differ) and then what the backups are: not
  set up without the credential file, so what is enabled runs nothing
  until setup; set up with the file and an account the runs accept;
  waiting on the account, with the file and an account that is
  missing, refused or not told, since the runs the timers start fail
  on it and setup is not what puts it right. The smoke test stages
  each. Two residuals of the line accepted (a hand-made entry at the
  path; the old path's note).
- 2026-11-06 — A security sweep: moteld.service gets
  `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`, `NoNewPrivileges=yes`
  and `RestrictSUIDSGID=yes`, as every other shipped unit had, so a
  setuid binary on the host is no route from the motel uid to root
  (scored 5×2×5); the smoke test holds the unit to them. Copilot's
  review of #407: the motel user's manager (`user@<uid>.service`) is
  the same route around them — motel asks it for units, and one asked
  without the sandbox's settings ran a setuid binary unbound — so
  postinstall's drop-in sets `NoNewPrivileges=yes` on the manager,
  which every unit it starts inherits and none can drop (5×2×5); the
  smoke test holds the manager's own process to the flag, not only the
  unit. A manager running through the upgrade keeps running without it
  until it next starts, as it keeps its limits: postinstall does not
  restart it, since that stops every app, and `docs/upgrading.md`
  names the restart for a quiet moment (a doc step: size 1, risk 2).
- 2026-11-20 — The 2026-11-06 line's "as every other shipped unit
  had" was not so for `motel-box-apply.service`, which had no
  `RestrictSUIDSGID=` until now (box/DESIGN-box.md, "The applier
  unit"); the smoke test holds it to the line.
