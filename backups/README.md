# backups

Backs up what motel's apps declare: the SQLite databases and the
files under each app's `shared/` that its `backup` block names
([liveswap/README.md](../liveswap/README.md#declaring-backups)), into
one [restic](https://restic.net) repository off the box. restic is a
backup program Debian ships; `motel-backup` runs it and never links
it.

`motel-backup` is a program of its own, and not part of the
server: motel validates a declaration and does nothing else with it,
and nothing here talks to motel. The two share a declaration format
(`liveswap/backupdecl`) and nothing more — `motel-backup` links no
Caddy.

**This release carries setup, the engine, restore, the restore drill,
the repository check, what says how they are doing, and the
package.** [docs/backups.md](../docs/backups.md) is the operator's
guide to them, every transcript in it observed on a fresh box; this
page is how they work. `motel-backup setup
<repository>` makes the box ready for one repository (below),
`motel-backup run` does one backup run, `motel-backup restore
<app>` puts a snapshot back, `motel-backup drill` proves that a
restore would work without doing one and checks the repository itself,
`motel-backup status` says whether each app's backup is fresh and
its restore proven, and
`motel-backup validate <Caddyfile>` says whether a run could plan
from a Caddyfile before it goes live. The `motel` package carries
the program, an hourly timer for the run and a Sunday one for the
drill ("On a fresh box", below), and depends on Debian 13's `restic`,
`sqlite3` and `ca-certificates` (the CA store restic verifies the
storage's certificate against); `setup` and every run need the
programs, and systemd 257 (`PrivatePIDs=`), which is Debian 13's, and
say so before anything else.

## On a fresh box

The package (`docs/first-deploy.md`) sets up: `motel-backup`; the
`motel-backup` account restic runs as, with setup's own `useradd`
line; `motel-backup.timer`, hourly — on the hour plus an offset of
up to ten minutes that is this box's own and stays the same — and
`motel-backup-drill.timer`, Sunday between 03:30 and 03:40 — an
offset of the same kind — both enabled, both catching
up a missed elapse at boot; and `restic`, `sqlite3` and
`ca-certificates` as packages it depends on (`apt install
./motel_….deb` brings them, with or without recommended packages:
Hetzner's Debian 13 image installs none). Nothing runs until `setup`
has written the credential file: each service is conditioned on
`/etc/motel-backup/repository.env`, and a start before that is
skipped, not failed. The lines that set it
up — the declaration, `validate`, `setup`, a first run, `status`, a
restore — are under "Set it up" in
[docs/backups.md](../docs/backups.md#set-it-up). All but `validate`
and `status` are root's, run as root over SSH. The administrator
`docs/hardening.md` makes is in the `adm` group alone, with no `sudo`:
it runs `validate` and `status`, which need no root.

The package's smoke test (`make install-test`) runs exactly those lines,
read out of that page, as root, on a fresh Debian 13 with no `sudo`,
the `.deb` just installed and an S3 server beside it; watches the
timer fire; runs `validate` and `status` as an account with no
privilege; refuses `--to /tmp/…` (every directory on the way has to
be root's own, "A restore"); finds the `pre-restore` snapshot; and
takes the package through upgrade, remove, reinstall and purge,
holding it to this table:

| Transition | The timers | The credential file |
|---|---|---|
| install | enabled, running; their services untouched until setup | not made — setup's |
| upgrade | restarted where enabled, as `dh_installsystemd` does it: an administrator's `disable` or `mask` is kept, a `stop` is not — disable a timer to keep it off. A run or a drill under way is left alone: its upload finishes. The helpers it starts afterwards are the new version's, and each does nothing for a command of another version — that app fails for the hour, said as the upgrade, and the next run is whole | byte for byte |
| remove | stopped and masked; the unit files gone, and the timers' stamps with them | kept, with `/var/lib/motel-backup` |
| reinstall after remove | enabled and running again; nothing is caught up, since nothing was missed | byte for byte; a run works |
| purge | their enable state gone with the masks | **kept, and said why**: it holds the repository password, the one way to read the backups already made. The record, the repository id, the listing's stderr and restic's cache go; `staging/` and `restore/` go only when empty — a directory with something in it holds copies of an app's data a killed run left, which root does not remove, and is named |

The account stays through remove and purge, as system accounts do.

At the edges of those:

- **Where the account cannot be made** — a group named
  `motel-backup` is there without it, and `useradd` refuses — the
  install says so and completes: motel is installed, and `setup`,
  which makes the account, is where backups wait.
- **At a remove** the timers and the services are stopped as
  `dh_installsystemd` stops them, and then, while the program is still
  there, `motel-backup sweep` takes away what a killed command left:
  the units it had started, by the exact names it recorded, and what it
  left mounted under `/run/motel-backup` — as the next run would
  have, since there will be none. A command that holds the run lock —
  `motel-backup run` from a shell, a restore at its prompt — is
  named by what the lock says and left to run, with its program gone
  from under it. Let it end first.
- **A command and its helpers are one version.** A run starts its
  helpers — the plan, the dump, a restore's check and install — from
  `/usr/bin/motel-backup` by path, and after an upgrade that is the
  new program. Each is told which program started it (by the hash of
  the file) and one that is another does nothing, exits 75, and says
  so; the run records the upgrade as that app's failure and tries no
  app after it. An app whose backup needs no helper of this program —
  files alone, which restic uploads — is backed up by the run as it
  was started: no versions mix. The helper that removes plaintext copies works for any
  version.
- **Purge removes nothing under `/run/motel-backup`** where anything
  is mounted at or under it, or where what is mounted cannot be read,
  and says so; otherwise its own files, and its directories by `rmdir`
  alone. Never `rm -r /run/motel-backup` while anything is mounted
  under it (`grep /run/motel-backup /proc/self/mountinfo`): that
  removes the app's files through the mount.
- **A disk mounted inside an app's data stays mounted.** A run binds
  the app's data recursively, so that such a disk is backed up with
  it, and makes what it has bound private before it does anything
  else with it, and again before it takes it away; so does the sweep
  at a remove. On a box systemd has booted the root mount is shared, and a
  bind of it that is taken away as it was made takes the disk beneath
  the app's own directory with it [measured; a container's root is
  private, and shows nothing of this].
- **What purge keeps in `/var/lib/motel-backup`** it names for what
  it is: `staging/` and `restore/` for copies of an app's data, the
  directory itself for a file the package did not make.

A run under the timer that fails — an app not backed up, restic not
installed, another run holding the lock — is a failed unit,
`motel-backup.service` in `systemctl --failed`, until the next run
succeeds; a drill that proved nothing, could not begin, or whose check
of the repository was not `clean`, the same for
`motel-backup-drill.service`. The record and `status` say the same
thing; the unit is for whatever watches units. Nothing acts on it. A
drill that comes due while a run is under way waits for it (`After=`);
a run that comes due while the drill holds the lock says so and is that
hour's failed unit.

A run's units are bound to its service, so that they end with it
however it dies — but the two that remove plaintext. A service that is
stopped (`systemctl stop`, an upgrade, a remove) has a stop job queued,
and the manager starts nothing that is bound to it [measured]; unbound,
a stopped run removes the copies it had made on its way out, and a
killed one leaves them to the next run, which empties staging first.

The services run as root with `CAP_SYS_ADMIN`, `CAP_DAC_READ_SEARCH`,
`CAP_CHOWN` and `CAP_FOWNER` and nothing else of root's, reach nothing
but the manager's unix socket (restic runs in units of its own), and
carry the hardening systemd offers a root service that has to make
mounts the manager can see: none of `ProtectSystem=`, `ProtectHome=`,
`PrivateTmp=`, `ProtectKernelTunables=` and their kin, nor
`PrivateNetwork=` or `ProtectKernelModules=` — each of these puts the
service in a mount namespace of its own, and the manager then binds the
bare mount point, root's and empty, into the units instead of the app's
data [measured: every database copy failed "permission denied", and
the files uploaded were an empty directory's]. The unit files say at
each line why it is there or not; `cmd/motel-backup/units_test.go`
holds the two services to one set, and the e2e units suite starts a
run and a drill from the shipped files.

## A run

`motel-backup run`, as root:

1. takes the run lock — one run at a time; a second says who holds it;
2. stops, by exact name, any unit an earlier run recorded and did not
   live to stop, and takes away any mount it left;
3. has an unprivileged unit adapt `/etc/motel/Caddyfile` and print
   the plan — the liveswap root and each app's declaration — and takes
   that plan strictly, validating every field again;
4. empties and removes the staging directory of any app that no longer
   declares a backup, and empties whatever a restore or a drill that was
   killed had fetched;
5. for each app that declares a backup — in name order, but one whose
   last run `failed` goes last:
   - empties the app's staging directory, before anything else;
   - looks for `<root>/<app>/shared` on the real filesystem, by opening
     it. Only "no such file" is absence: `pending` if nothing has ever
     been backed up of the app, `data missing` if something has. The
     record says which; where it holds no snapshot of the app — a
     rebuilt box, a new disk, or an app that really is new — the
     repository is asked, on every run that finds it so (an answer of
     "none" is not kept: it could go stale), and if it cannot be asked
     the app is `failed`, not `pending`: `pending` is the one absence a
     run exits 0 on. The directory must belong to the
     `motel` user;
   - copies each declared database into staging with `VACUUM INTO`, and
     keeps a copy only if `integrity_check` says exactly `ok`;
   - uploads with `restic backup`, making one try at each request to an
     S3 storage where restic would make ten (`-o s3.retries=1`, as the
     fetch and the check do): a storage that takes the request and never
     answers ends it, exit 1, after 15 minutes at the opening and 21–36
     mid-upload, where ten tries took 100 and 106 [M92, M93];
   - lists the snapshot that upload made — by the id in its own summary
     — and looks for every declared item in it, because restic leaves
     out a path that vanishes while it runs, with exit 0; and holds
     each file or directory it finds to the one that was given to the
     upload, by its inode and its owner, which the listing prints: a
     directory of the right name that is another directory — the
     upload shown an empty one in its place — is `incomplete`, and the
     record says both inodes. A declared directory that is empty is
     backed up, empty;
   - empties staging again, whatever happened;
   - if the app is `ok`, writes into the repository the record that
     this run ended `ok` on it ("What a snapshot holds", below): what a
     rebuilt box, which has no record, has to go on;
   - and, if the app is `ok` and no drill of it is on record — proven
     or failed — drills the snapshot it has just made (below): an app's
     first good backup, once; after that it is the drill's. That is the
     whole app fetched back, in plaintext, on a timer, so above 1 GiB
     restored the run leaves it to `motel-backup drill`, and says so;
     and where the repository has just not answered — the app's record
     given up at its clock — it drills nothing, records no verdict, and
     the next run drills it;
6. lists the repository, once, for every app (`restic snapshots
   --no-lock --tag motel`, which leaves the records of clean runs
   out), and writes beside each snapshot the record names when
   it was last there (`seen`), and when the listing was answered
   (`listed`). Not after the repository has refused the run, and not
   with no snapshot on record to look for. A listing that fails is a
   warning: it changes no app's result, and makes no snapshot look
   gone. So is one restic had anything to say beside, on stderr: it
   leaves a snapshot it cannot load out of the listing and exits 0
   [measured, with a cold cache]. The listing unit's stderr goes to a
   file, not to the journal; what restic said of a listing that was
   not believed is kept in `/var/lib/motel-backup/listing.err`,
   root's to read, until a listing is answered — the record, which is
   everyone's to read, holds none of restic's words but the id of a
   snapshot it could not load — and the record says since when
   listings have gone unanswered (`unlisted`). That is meant, and
   strict: anything at all on restic's stderr beside a listing makes
   it not believed, so a restic that one day prints a harmless notice
   on every command leaves every box unlisted, and after 3 hours
   unhealthy — `listing.err` says what it printed. A snapshot this run itself made, or
   fetched for its first drill, is judged by the next run's listing,
   not by this one's;
7. writes `/var/lib/motel-backup/status.json` — also when the run
   ended early, with why, and the apps it did not reach as `not run`.

An app that has left the plan with snapshots in the repository — a
block deleted, or an imported file removed while its glob still matches
others — is a warning of the
run that finds it gone, and of `status` until the next run: once. The
record then drops it, as its operator may have meant.

It exits 0 only if every app is `ok` or `pending`.

## Who runs as what

Every step is a transient systemd unit with a deny-by-default view: an
empty read-only root, the parts of `/usr` and `/etc` a program needs to
run, and what the step is given.

| Unit | Runs as | Network | Credential | Sees |
|---|---|---|---|---|
| the run itself | root | — | reads which repository it names, to refuse a `b2:` one; passes the file's path to the manager for the rest | its own state and run dirs |
| setup | root | — | writes it; sees whether one was there before, and reads none | its own state and run dirs, and the terminal |
| plan | `motel-backup`, own user+PID namespaces | no | no | `/etc/motel`, read-only |
| probe, init, open (setup's) | `motel-backup`, no capability | yes | yes, the file setup is about to put in place | nothing of the app |
| dump, clean | `motel`, own user+PID namespaces | no | no | that app's `shared/` (dump only) and staging |
| upload | `motel-backup`, `CAP_DAC_READ_SEARCH` | yes | yes | that app's declared paths and staged copies, read-only |
| verify | `motel-backup`, no capability | yes | yes | nothing of the app |
| size | `motel-backup`, no capability | yes | yes | nothing of the app |
| fetch | `motel-backup`, no capability | yes | yes | one empty directory, writable |
| hand-over | root, `CAP_CHOWN` and `CAP_DAC_READ_SEARCH` | no | no | that directory and nothing else |
| install, check | `motel`, own user+PID namespaces | no | no | what was fetched — writable: it is this run's scratch, and a copy closed to its owner is opened to be read — and (install) that app's `shared/` |
| unstage | `motel`, own user+PID namespaces | no | no | what was fetched, to remove it |
| mkshared | `motel`, own user+PID namespaces | no | no | the liveswap root, to make `<app>/shared` on a rebuilt box |

The run itself needs root — `CAP_SYS_ADMIN`, `CAP_DAC_READ_SEARCH`,
`CAP_CHOWN` and `CAP_FOWNER` of it, the unit file being the one list —
and the host's own mount and PID namespaces: it
makes bind mounts that the manager then has to see. Under the timer it
is `motel-backup.service` ("On a fresh box"), which says so line by
line.

- **What parses the app's bytes holds nothing.** `sqlite3` opens files
  the app chose, so it runs as the app's own uid in the app's own kind
  of sandbox, with no network and no credential, seeing one app.
- **The credential is never under the `motel` uid.** `restic` runs
  as an account of its own, so the server and every app can neither
  read its environment nor signal it. The one capability lets it read
  files it does not own, in a view that holds one app and nothing else.
  The run passes the *path* of `repository.env` to systemd, which reads
  it as root for each restic unit; the run itself reads it only to
  refuse, before any unit, a file the manager would refuse whole, a
  `RESTIC_REPOSITORY_FILE` and a `b2:` repository (`repositoryInUse`) —
  not every repository `setup` refuses (Accepted residuals). `setup` is
  its one writer, and `status`, as root, its one other reader.
- **What an operator or an app wrote stays off command lines**, with
  two exceptions. Units get the app's declaration as a root-written file
  bound at a fixed path, and declared paths as bind sources. Every
  property is sent to systemd typed, over D-Bus, and commands as
  `ExecStartEx` with `no-env-expand`: a directory called
  `$RESTIC_PASSWORD %h` is backed up under that name. The exceptions are
  the listing: the directory part of a nested declared path
  (`media` of `media/uploads`) is an argument to `restic ls`, after
  `--`, as `/backup/<app>/files/media`; and the dump: a declared
  sqlite path, percent-escaped into a `file:` URI and quoted as a SQL
  literal, is in the statements on `sqlite3`'s command line (`execute`
  and `RestoreOver`, backups/dump/dump.go), readable in
  `/proc/<pid>/cmdline` as the paths in `status.json` already are.
- **Root never reads what an app wrote.** It opens `shared/` and each
  declared path as `O_PATH` — a descriptor that names a file and cannot
  read it — makes each app's staging directory, makes mounts, and
  starts units.
- **An app cannot aim its backup at anything else.** A declared path is
  the app's own to replace, while it runs, with a link to a sibling's
  data; a bind source is a path systemd resolves as root, following
  links; and the upload unit reads any file it is shown. So the run
  opens each path itself, refusing any symbolic link on the way, and
  binds what it opened — `mount(2)` from `/proc/self/fd/<n>`, which the
  kernel follows to that very directory — onto a mount point in its own
  root-only directory. systemd is given the mount point. (Given
  `/proc/<pid>/fd/<n>` itself, systemd reads the link's text and walks
  the path again, and an app flipping the name wins that race now and
  then.)
- **The hardening is systemd's, on a kernel with seccomp.** Debian 13's
  has it; on one without, `SystemCallFilter=`, `RestrictSUIDSGID=` and
  `LockPersonality=` are warnings in the journal, not refusals, and the
  view and the accounts are what is left.
- **What a unit says is not believed because a unit said it.** The
  plan is decoded strictly and validated again; a dump result has to
  answer what was asked, in a class the run knows; and words that came
  from `sqlite3` — which hold names the app chose — lose their control
  characters and their length before they are kept or printed.

## A restore

`motel-backup restore <app> [--snapshot <id>] [--to <dir>]
[--no-pre-backup] [--yes]`, as root. It works from the repository and
the Caddyfile alone: a rebuilt box has no record, and needs none. What
that costs is below, under "what a restore cannot tell".

1. The lock, the leftovers and the plan, as for a run. The app has to
   declare a backup: the liveswap root is taken from the plan. The lock
   is the run's own, and is held from here to the end — the question
   below included — so a backup run that comes due meanwhile says a run
   is in progress, and does nothing. The question waits ten minutes for
   its answer, and no answer is a no; the backup made first, the fetch
   and the install are not bounded, so a large restore is hours in
   which no app is backed up. On one box that is the accepted cost: a
   restore is the one thing the box is doing.
2. The repository is asked for the app's snapshots (host `motel`, tag
   `app:<name>`). The newest that a backup run made is restored, or the
   one `--snapshot` names by its hex id, or by eight or more characters
   from the start of it — never `latest` — and only if it is among the
   app's. "Newest" is by the time restic recorded. Where the record
   says the last snapshot a run ended `ok` on is another one, the
   question and the report say so, and name it.
3. It asks, naming the snapshot and when it was made, and takes the
   app's name typed back — not "y", which a hand types on its own — for
   a yes. `--yes` answers; with nobody to answer it refuses. Nothing has
   happened yet.
4. **The app is backed up first**, by the steps of a run, so the restore
   can be undone: the snapshot is tagged `pre-restore` and named in what
   the restore prints, also when the restore then fails. It holds what
   was being restored over — the damage, as often as not — so it is
   restored only by name: it is never "the newest", to a restore or to a
   drill. If that backup does not end `ok` — the live database being the
   damaged thing, often — the restore stops and says so;
   `--no-pre-backup` restores without one. Where there is no
   `<root>/<app>/shared` there is nothing to back up or overwrite: a
   unit running as `motel` makes it, mode 0750 — and takes it away
   again if the restore then puts nothing in it, since an hourly run
   would take an empty directory for the app's data.
5. **fetch**: `restic restore -o s3.retries=1 <id>:/backup/<app>` into an empty directory
   under `/var/lib/motel-backup/restore`, as `motel-backup` with
   no capability. Not in a user namespace: there every `chown` restic
   tries fails in a way it does not overlook. Always `<id>:<path>`: a
   path the snapshot does not hold is then exit 1, where `--include`
   matching nothing is exit 0. Exit 0 is still not a restore: restic's
   summary has to say it restored something, and everything. **A fetch
   is the whole app, in plaintext, under `/var/lib/motel-backup`, and
   needs that much room there** — a drill too. So the repository is
   asked first how large the snapshot is restored (`restic stats --mode
   restore-size`), and a fetch that is larger than what is free there is
   not begun: the restore, or that app's drill, says how much it needs
   and how much there is. An install then writes the app a second time
   while the fetch is still there, and on the usual box that is the
   same disk — the one the live apps write to — so a restore into place
   or `--to` needs **twice** the snapshot free on one filesystem, or
   the snapshot on each of two. A drill, which installs nothing, needs
   the fetch alone.
6. **hand-over**: `chown -hR motel:` of that directory, by root, in a
   view that holds nothing else, with no network. By name, so a restore
   does not depend on the uid the snapshot was made under. It needs
   `CAP_DAC_READ_SEARCH` beside `CAP_CHOWN`: root without it cannot read
   a directory that is another account's and closed.
7. **install**, as `motel` in its own namespaces with no network and
   no credential, given what was fetched (writable: it is scratch, and
   opened to be read where the app kept it closed) and the app's
   `shared/` — pinned and bound by the run itself, as for an upload.
   **Every check comes before any change**:
   - the snapshot's own `plan.json` says which paths are databases — not
     the box's declaration, which may have changed. It is read strictly
     and validated as a declaration is anywhere;
   - each database copy has to be a SQLite file of which
     `integrity_check` says exactly `ok`;
   - every declared item has to be in the snapshot;
   - nothing may be in the way: a symbolic link at `shared/`, at a
     declared directory or on the way to it, or at a database, is
     refused; and so is a file that has a `-wal` beside it in place — a
     live database that this snapshot holds only as a file, which put
     back under the app would be read with a log that is not its own.
     Nothing here can stop the app: restore before it is first deployed,
     or `--to` a directory and move the file into place with the app
     stopped.

   If anything fails, nothing is installed — the files neither. Then a
   database is restored over the live one by SQLite's own backup, as one
   transaction, with the app running: a writer waits and carries on.
   (`sqlite3` is started as for a dump, and has the same five minutes to
   *open* the live database; in rollback-journal mode, where a reader
   and a writer exclude each other, a restore that stays locked out says
   so.) Where there is no live database the copy is put there, and any
   `-wal`, `-shm` or `-journal` left beside its name removed. Nothing
   here can stop the app, and one that re-creates its database meanwhile
   keeps writing to a file that is no longer there: with no database in
   place, restore before the app is first deployed. A copy is in
   rollback-journal mode; an app that wants WAL sets it when it opens. Files are written beside their names and renamed onto
   them, in directories opened without following a link: a link where a
   file goes is replaced, never written through — a file a killed restore
   left half written under its temporary name is listed as left in place,
   never removed: a name is not a reason — and a symbolic link the
   snapshot holds comes back as the same link, its text stored and never
   followed; a file with several names comes back as one file with those
   names. Not put back: setuid, setgid and sticky bits (the unit cannot
   set them), extended attributes and ACLs, and directories' times. A
   copy the app kept closed to its own owner is read all the
   same — what was fetched is this run's scratch, under a directory no
   app can enter — and the target gets the mode the snapshot held.
   Directories the restore makes get the modes they had, and one that is
   closed to writing in place is opened while it is filled. If something fails
   once installing has begun, the restore says the data is partly
   restored — never that nothing was changed — and the same restore, run
   again, goes over it.
8. **Nothing of the app's is ever removed.** What is in place under a declared path
   and not in the snapshot is left, and listed. What is in the snapshot
   and could not be app data and could not be put back — a FIFO, a
   socket, a device — is listed and not installed, and the restore is
   none the worse for it. A declared database is never installed from
   `files/`, whatever stands there.
9. What was fetched is removed, first and last, by a unit an interrupt
   does not reach; a run, a restore and a drill each remove what a
   killed one left.

`--to <dir>` does the same into a directory it makes (it must not
exist), owned by `motel` and laid out as `shared/` is. Every
directory on the way to it has to be root's own and writable by nobody
else — `/root`, `/srv`, `/var/backups`, a root-owned directory of your
own; not `/tmp`, and nothing an app's user owns — since the restore is
made where the name leads, and anyone who could write a directory on
the way could have put a link there first. That rule does not keep
root's own directories out of reach, so `--to` is root's alone and is
never to be granted through `sudo`: whoever chooses the directory
chooses where root puts bytes the app wrote — restored into
`/etc/systemd/system/<unit>.d`, an app's `files` became that unit's
settings at the next `daemon-reload` [measured]. Nothing is
asked and nothing backed up, since nothing is overwritten; and what is
sound lands even when something else is not — a damaged copy is never
handed out as a database — with a non-zero exit. It is how to look into
an `incomplete` snapshot, which cannot be restored into place.

### What a restore cannot tell

A run that ends `incomplete` because restic exited 3 — a file
unreadable, or gone while it was read, which is ordinary for a live
uploads directory — leaves a snapshot with files left out of a
directory it did read. Every check at install passes on it, and a
restore of it puts back fewer files than the previous `ok` one would,
and says it restored. On a box with a record the restore says when the
snapshot is not the last one a run ended `ok` on, and names that one.
On a box whose record has no run of the app that ended `ok` — a
rebuilt box, most often — the repository says: a run that ends `ok` on
an app writes a record of it there (below, "What a snapshot holds"),
and the restore asks for those records and says the same, naming the
newest snapshot one vouches for. It restores what it would have
restored either way; the question comes first. What it still cannot
tell: a snapshot whose run's record could not be written (the run said
so, as a warning), or whose record was forgotten off the box — then it
says that nothing in the repository vouches for any snapshot of the
app, or that the records could not be asked, and why.

## The restore drill

`motel-backup drill`, as root: for every app the repository holds a
snapshot of, the newest is fetched, handed over and checked exactly as a
restore checks it — and nothing is installed; no unit of a drill sees
any app's data. The record then says, per app, `restore_proven` (which
snapshot, and when it was proven) and, until the next drill that proves
one, `restore_drill` (which snapshot proved nothing, when, and why). A
drill that fails never replaces what was proven, a drill that is
interrupted records what it finished and nothing of the app it was
interrupted on, and a backup run carries both. It exits 0
if no drill failed; an app the repository holds no snapshot of has
nothing to prove. A `run` that drills — an app's first good backup —
says what the drill found on a line of its own. The package runs it
every Sunday between 03:30 and 03:40, at an offset that is this box's
own and stays the same (`motel-backup-drill.timer`, catching up a
missed one at boot); a drill that proved nothing leaves
`motel-backup-drill.service` failed until the next one proves.

### The repository check

After its apps, whatever became of them — and where the Caddyfile
cannot be turned into a plan, without them: the repository needs none —
a drill checks the repository itself: `restic check --no-lock -o s3.retries=1 --read-data-subset=n/52`, as the backup
account — its structure, and one fifty-second of its data, read back
and verified. n is the group `check_next` in the record names, and the
ISO week's in UTC (`((week − 1) mod 52) + 1`) for a first check. A
`clean` verdict sets `check_next` to the group after; any other verdict
leaves it on the same group — so a check that finds damage, a first one
included, is followed by one that reads the same group again, and says
`damaged` until a check of it is clean (the owner, 2026-09-30) — and an
interrupt, which is no verdict, leaves it as it was. Which group a pack
falls in is fixed by its id, so
fifty-two clean checks read every pack once, whatever weeks a box was
off or a check came to nothing — the whole repository a year,
about 2% of it in downloads each week. On 28.5 GB over a link of 25 ms each way and 50 Mbit/s it read
about 600 MiB in three to four minutes, holding the run lock
throughout: an hourly run that comes due meanwhile says so and is that
hour's failed unit, as with the drill.

restic's exit 1 means both "damaged" and "could not reach the
storage", and its summary counts an error where it could not open the
repository at all. So `restic cat config --no-lock` is asked first,
under a clock of 30 seconds — it answers in about a second where the
storage answers at all, and retries for a quarter of an hour where it
does not, or refuses the key — and again after a check that exited 1.
The verdict, in `status.json` as `last_check` (when, which group, and
why):

| verdict | what it means |
|---|---|
| `clean` | restic read the structure and the group and found nothing wrong |
| `damaged` | restic found errors, and the repository answered before and after it: the count, the damaged packs, that a prune run off the box or the storage failing during the check looks the same and the next check reads the group again, and `journalctl -u motel_backup_repocheck_…` for restic's own words |
| `unreachable` | the probe was not answered in 30 seconds, or restic exited 1 on it: the storage could not be reached, or refused the key — or the check lost the storage half way ("could not finish") |
| `no repository` | the storage answered, and holds none there (10) |
| `wrong password` | the password does not open it (12) |
| `failed` | anything else: a unit systemd could not set up, a check ended by a signal, a summary that could not be read or did not add up |

An interrupted drill comes to no verdict and leaves the last one, and
so does a check or a probe whose unit is ended from outside — stopped
by someone, or with the drill's own service, maybe before the drill
sees its own stop. One exception: a check that exited 1 having counted
errors, stopped before the repository could be asked again, is
recorded `failed` — "restic check counted N errors, and was stopped
before damage could be told from a storage that went away" — never
dropped, and its group is read again. The check takes no lock: one killed hard with a lock written leaves it, and
restic 0.18 never passes a stale lock by itself, so every backup after
would fail until someone ran `restic unlock` [measured]. What that
costs: a `prune` run off the box at the same moment can make packs
vanish under it, and the verdict say `damaged` though nothing
is; the next check reads the same group again, and says otherwise. A check killed hard leaves its
temporary cache, some tens of MiB, in `/var/cache/motel-backup`;
the probe before a later check removes one that is a month old
(`restic cat config --cleanup-cache`: a check works in a temporary
cache of its own, and cleans nothing else [measured]).

Damage is said — by the drill, which exits 1, by
`motel-backup-drill.service` failed, and by `status` — and nothing
more: backups go on (a new snapshot of what changed is still worth
having), and the box never repairs, since `restic repair` removes data
and nothing on the box forgets, prunes or repairs. What restic's words name is
run off the box, with a key of its own, and the next drill says
whether it worked.

## What a snapshot holds

One snapshot per app per run, host `motel`, tagged `motel` and
`app:<name>`, of `/backup/<app>`:

```
/backup/blog/plan.json          the declaration this snapshot was made from
/backup/blog/sqlite/app.db      the consistent copy
/backup/blog/files/uploads/…    the declared files
```

The paths are the same on every box, whatever its liveswap root, and
through a symlinked root the snapshot holds the data, not the link. Each
app is its own `(host, paths)` group, so a `forget` policy applies to
each app on its own — and to the snapshot a restore makes of what it
restores over, which is in that group too, tagged `pre-restore` besides
("Retention", below).

A run that ends `ok` on an app then writes one more snapshot, the
record that it did: host `motel`, tagged `motel-clean` and
`vouches:<the app's snapshot id>`, of one file,
`/motel-clean-<app>`, which holds that id — some four hundred bytes
(`restic backup --stdin-from-command -- /usr/bin/echo <id>`; a record
of nothing is restic's exit 3) — at the snapshot's own time, to the
second, as the listing that checked the snapshot gives it: a record a
few seconds later could fall in the next day, where a `--keep-daily`
policy keeps the snapshot and not its record. It is the one thing in the repository
that says how a run ended: restic writes a snapshot of a run that
failed half way too. Each app's records are a `(host, paths)` group of
their own, beside the app's snapshots, so a `forget` policy thins them
as a group of their own, and no app's history lists them. A record
not written is a warning of the run, and the app stays `ok`: its
snapshot was verified before the record was begun. The warning says
which: "could not be written", where restic itself said it failed;
"may not have been written", where restic may have saved it first —
the run stopped while it wrote (Ctrl-C, `systemctl stop`, a shutdown),
its clock, its unit ended from outside or by a signal, an exit 0 that
named nothing; and "was not written", where the run was stopped before
it was begun.

Every time is stored in UTC. restic stores a snapshot's time in the
zone it runs in, every unit sees the box's `/etc/localtime`, and
`forget` sorts each snapshot into its hours, days and months by the
zone it was stored in [measured]: a box in Berlin stored its app
snapshots at `+02:00` and their records at `Z`, and `--keep-daily`
kept a 23:30 snapshot and removed its record. So every restic unit is
given `TZ=UTC`, and a `forget` policy's days and weeks turn at UTC
midnight, whatever the box's zone.

### Retention

Nothing on the box forgets, prunes or repairs: `forget` and `prune` are
run off the box, with a key of their own — on the bucket
[docs/backups.md](../docs/backups.md#before-you-start-the-bucket-and-what-it-protects)
describes, one that cannot destroy a version either, so what they
remove stays an old version for 180 days. Without them every
hourly run adds a snapshot of each app and a record of each that ended
`ok` — two snapshots an app an hour — and every listing of the
repository reads every snapshot file, on a rebuilt box from an empty
cache: 1,506 snapshots took 19.6 s over 25 ms each way and 50 Mbit/s
[measured]. At 7 apps a year of that is some 120,000, and at that rate
a first listing of some 26 minutes [extrapolated, not measured] —
close to the 30 minutes at which a run or a drill gives a listing up;
a restore waits it out. A policy such as `--keep-hourly 24 --keep-daily
30 --keep-monthly 12` keeps a listing to a few hundred — run as two
`forget`s, not one ([docs/backups.md](../docs/backups.md#retention-off-the-box)).
In the default grouping a restore's `pre-restore` snapshot can be the
newest of its hour in the app's group, and then `--keep-hourly` keeps it
and removes that hour's backup: dry-run after a restore, the policy
would have removed the last complete backup, which was also the last
proven, and with `--keep-tag pre-restore` it kept the restore's
snapshot and would still have removed the backup [measured]. Grouped by tags as well, the app's backups and
its `pre-restore` snapshots are thinned apart; but each record's
`vouches:` tag is its own, so the records are thinned by `(host,
paths)` in a `forget` of their own (`--tag motel-clean`), and the
backups in the other (`--tag motel`, which no record carries).

A declared database that sits inside a declared `files` path is not
uploaded there, nor its `-wal`, `-shm` and `-journal`: its contents are
under `sqlite/`. Two things see to that. Each is masked in the upload
unit's view — an empty, unreadable file where it was, made by systemd
when the unit starts, so the live bytes are not there to read. And each
is named, exactly, in a root-written restic exclude file, because a mask
covers only what existed when the unit started: a database the app
creates, or puts back under its name, while restic walks is excluded all
the same. (restic reads an exclude line as a pattern and expands `$VAR`
in it, so every pattern character is escaped and every dollar doubled:
`app*.db` excludes itself, and not `appX.db`.)

## Databases

A declared path is handed to `sqlite3` only if it is, right now, a
regular file with a SQLite header, reached without following a symbolic
link anywhere on the way: a FIFO, a device, a directory, a link, a path
through a link, an empty file and a missing file are each reported and
never opened.

`sqlite3` is started on an in-memory database and `ATTACH`es the real
one as `file:<path>?mode=rw`. Given the path directly, the `sqlite3`
shell opens it read-only itself before SQLite does, and that open waits
for ever on a FIFO — which an app can put in place of its database
after the checks. By `ATTACH` a FIFO, a socket or a directory is an
error within milliseconds, and a missing file is not created. It runs
with `-init /dev/null`, because it otherwise reads `~/.sqliterc` and an
app's `HOME` is a directory the app writes, and with a 30-second busy
timeout.

**The one time bound.** A FIFO the app has made mode 0400 still blocks:
the read-write open is refused and SQLite retries read-only. So
`sqlite3` has five minutes to *open* a database — for the copy's target
file to appear, which takes some ten milliseconds whatever the
database's size, after a wait of at most the busy timeout — and is
killed if it has not. Once the target exists nothing bounds the copy,
and nothing bounds an upload.

## What a run says

`status.json`, per app: `ok`, `incomplete` (a snapshot exists and
something declared is not in it, or restic exited 3), `pending`, `data
missing`, `failed`, `not attempted` (in the words of why: a
repository that did not answer, or refused an earlier app's backup for
a reason every app shares, or a helper of another version), `not run`;
each declared item with
whether it was found in the snapshot; the snapshot's id; the last run
that was `ok`, and the last that made a snapshot at all — each with
`seen`, when the repository was last found to hold it. An app a run
did not reach is `not run`, with those two dates and nothing else:
never the last run's result under this run's date.

restic's exit statuses are reported as measured on 0.18: 10 is "no
repository", 11 "locked" (after `--retry-lock 2h`), 12 "wrong
password". 1 is "the storage could not be reached, or refused the key,
or something else" — never "no repository": a wrong storage key ends,
after about fifteen minutes of restic's own retrying, in exit 1 and a
message about a missing repository. After any of those four the run
does not try the remaining apps, which would each wait as long. An
exit 1 that is really about one app costs the apps after it that run
and no more: an app whose last run failed goes last.

How a copy ended is read from `sqlite3`'s exit status, which is
SQLite's result code (5 and 6 busy, 13 full), never from its words:
they hold the database's path, and `busy.db` is not busy.

## Status

`motel-backup status [--json]`, as anyone: `status.json` is world-readable
and holds no secret, and the manager tells any user which units are
running. It prints when the last run started — and that no run has
finished for more than 3 hours, where that is so: the run itself is
aged, or a record whose apps are all `pending` would read "no data
yet" for as long as no run replaced it — since when the repository
has gone unlisted, where it has, when a drill last ran, or why the last
one could not begin, what the last check of the repository found and
when, and then per app:

- how the last run ended, where it looked for the app's data, and the
  last **complete** backup — the last run that ended `ok` — with its
  snapshot. An app that is `pending` has nothing else said of it;
- `stale` when that backup is more than 3 hours old: two hourly runs
  missed, and some grace. Freshness is the last complete backup's,
  whatever is running now and whatever a later `incomplete` snapshot
  holds;
- `snapshot … is no longer in the repository` when a listing answered
  since the snapshot was last seen there did not hold it — pruned or
  forgotten off the box, lost, or rewritten under a new id (`restic
  rewrite`, `restic tag`). A last complete backup that is gone is not a
  backup; a *proven* snapshot that is gone is said, and what was proven
  of it stays proven. With no listing answered since, nothing is said
  either way: "in the repository" is never claimed, only "gone";
- `restore last proven: <when>, snapshot <id>`, and `: old` when that
  is more than 8 days ago — the weekly drill's period and a day;
- `restore not proven: <why>` when the last drill proved nothing —
  also beside an older proof: the newest snapshot is the one a restore
  reaches for — or when no drill has run;
- and what is running: each unit of a run, a restore, a drill or a
  setup that is running, starting or stopping, by what it does, to
  which app, since when. A unit whose command is still running is
  `activating` to systemd, which is not failed. The command that
  starts those units is not one of them: whenever it is at work itself
  — between two units, and for the whole of a restore's question —
  `no backup unit running now` is what is said, though it holds the
  run lock and a second run, drill or restore is refused. Where the
  manager cannot be asked, that is said instead.

It exits 0 only if the last run finished within 3 hours and did not end
early, the repository has not gone unlisted for more than 3 hours, the
last drill could begin, the last check of the repository was `clean`
and is no more than 8 days old — or, with none yet, the first drill
that checks ran no more than 8 days ago (`check_since` in the record),
or, before any has, backups were set up or a drill ran no more than 8
days ago — and every app that is not `pending`
was backed up by the last run (or not reached by it), has a complete
backup that is fresh and that no listing since has missed, and a
restore proven in the last 8 days with no failed drill since. With no
app declaring a backup there is nothing to fail. A box set up less than
3 hours ago and not yet run is `pending first run`, exit 0; after that
it is not. Anything else of those exits 1. What kept `status` from
looking at all — no `/etc/motel-backup/repository.env` (backups are
not set up), a record that cannot be read — exits 3: not the same news as
backups that are unhealthy. (2 is the usage text's. These are
`status`'s own: its 3 has nothing to do with restic's exit 3, an
incomplete backup, which a run records as `incomplete`.)

That anyone may run it is meant. `status.json` is readable by every
account on the box, and so are the app names, data paths, snapshot ids
and cleaned error text in it; nothing of the credentials is, and of
the repository's location only what a run's refusal of it names: the
bucket and path of a `b2:` repository, in the S3 URL that moves it. Run as root, `status` also reads the credential file
and says, as `warning:` lines, where a hand-edited line is not what the
manager reads (a key with whitespace around it, a key set twice — the
last wins — a line that is not `KEY=value` or whose name the manager
does not take, a quote never closed, which takes the rest of the file,
a NUL anywhere, or a byte that is not UTF-8 in a key or a value, either
of which makes the manager refuse the whole file, though such a byte
in a comment does not [M91]), where `RESTIC_REPOSITORY` or `RESTIC_PASSWORD` is not set or
set to nothing, where the repository is one the box cannot use or a
value setup would refuse, and where the file is there and cannot be
read at all; its exit status does not change for it.

### As JSON

`motel-backup status --json` says the same as one JSON object on
stdout, for a program to read — a client over `ssh box motel-backup
status --json` — and leaves with the same exit status. Nothing else
goes to stdout, and what goes to stderr is what goes there without the
flag. A stdout that takes none of it (a full disk) is named on stderr,
with or without the flag: a report its reader never got exits 1, not
0, and could not tell stays 3. The text and the JSON are printed from one `status.Result`
(`backups/status/status.go`): the text is its `lines`, one per line.
This object is the contract; `status.json`, the record it is read
from, is not, and its fields may change in any release. The shape is
pinned whole by `backups/cmd/motel-backup/testdata/status-*.json`:
a field may be added; one removed, renamed or changed in meaning breaks
its readers.

```json
{
  "at": "2026-09-20T15:00:00Z",
  "healthy": false,
  "apps": [
    {
      "name": "shop",
      "healthy": false,
      "result": "failed",
      "last_complete_backup": "2026-09-19T09:00:00Z",
      "fresh": false,
      "restore_proven": "2026-09-18T15:00:00Z",
      "restore_proven_lately": false
    }
  ],
  "running": [
    {"role": "upload", "app": "blog", "state": "activating", "since": "2026-09-20T14:58:00Z"}
  ],
  "unrecognised_units": ["motel_backup_zzz.service"],
  "lines": [
    {"level": "warning", "text": "warning: /etc/motel-backup/repository.env: …"},
    {"app": "shop", "level": "problem", "text": "shop: stale: the last complete backup is from …"}
  ]
}
```

- `at`: when status looked, which every age is reckoned from (an age
  is `at` less the time, not the reader's clock less it). Every time is
  RFC 3339, in UTC, to the second; `fresh` and
  `restore_proven_lately` are reckoned from the times as the record
  holds them, so at the boundary an age read off the seconds may
  differ from them by under a second, and they are the verdict.
- `healthy`: exit 0. `could_not_tell`, present only on exit 3, is why
  it could not look (the words stderr begins with); `apps`, `running`
  and `unrecognised_units` are then empty, and `lines` holds only the
  `warning:` lines found before it stopped.
- `apps`, sorted by name: `result` is how the last run ended for it
  (the classes under [What a run says](#what-a-run-says); empty where
  the record holds no result for it); `last_complete_backup` (null:
  none), and `fresh`: no more than 3 hours old and missed by no listing
  since; `restore_proven` (null: never), and `restore_proven_lately`:
  no more than 8 days ago with no failed drill since; `healthy`: no
  line of the app is a problem. A `pending` app is healthy and neither
  fresh nor proven.
- `running`: each unit of a run, a restore, a drill or a setup the
  manager holds — `role` (`upload`, `fetch`, …), `app` (absent for a
  unit of the whole run), systemd's `state`, `since` (absent when the
  unit ended while the manager was asked). `unrecognised_units`: the
  `motel_backup_*` units whose name says nothing here.
- `lines`: the text, in its order: `app` (absent for the run, the
  repository and a unit of no app), `level` — `problem` for a line that
  makes status unhealthy, `warning` for a `warning:` line (the
  credential file's, as root, and the last run's), `info` for any
  other — and `text`, as printed.

It holds what the text holds and nothing more: no value of the
credential file, the repository only as a warning names it, and every
string through `record.Text` or `record.Clean` as the text's are (a
test feeds the credential file's checks a password, a key id and a
secret, in well-formed and hand-edited lines and in the URL, and finds
none of them in either output).

## Before a Caddyfile goes live

`motel-backup validate <Caddyfile>`, as anyone: what a run's plan
step does to the live file, done to this one by whoever asks — it
adapts it with `/usr/bin/motel adapt` and no environment (below),
and refuses what that step would refuse, in the same words. It is not
run as the plan unit's account nor inside its view, so an import from
outside `/etc/motel`, or one the `motel-backup` account may not
read, can pass here and fail the run: those are for `motel validate`
to refuse, where the adapter itself says which file each `backup` block
comes from.

A copy checked anywhere but `/etc/motel` — a checkout, a home
directory — is judged where it is: an import glob relative to it has to
match there, and only the file itself is read for `{$NAME}`.

Keep `/etc/motel` to config. A run reads every file there that
others may read, whole, every hour, for `{$NAME}` — however large: a
variable in a large imported file moves a root as well as one in a
small one. A tarball or a package parked there is read into the plan
unit's memory each time, and anything `{$X}`-shaped inside any file
there becomes a variable name, tried with made-up values and named in
a refusal (its name, never a value).

It names the apps a run would back up, and each app that declares no
backup — which is said, not refused; one named through a `{$NAME}` is
said by the variable, since what the server calls it is not known here. It starts no unit, takes no lock
and changes nothing.

The box channel's step 6 (`box/validate.go`) runs it on the staged
copy of a push after `motel validate`, as the motel user, where
`/usr/bin/motel-backup` can be run, so a Caddyfile that would break
backups is refused before it goes live and not by the next hourly run.
It needs no privilege.

## What it refuses

- A liveswap `root`, an app's name or a backup path that depends on a
  Caddyfile `{$NAME}` — the root only where some app declares a backup:
  where none does, a run backs nothing up and the root is not its
  business, so `validate` says "no app declares a backup" and a run
  plans nothing, whatever the root says. (motel itself refuses
  `root {env.X}` together with a `backup` block; `{$NAME}` it never
  sees.) See below.
- A variable in an import, `import sites/{$ENV:prod}/*.caddy`: with any
  other value it names a file that is not there, and what the server
  reads when it is set cannot be known here. Write import paths
  literally. A variable that no made-up value adapts with is refused
  for this where some value failed at an import — Caddy's parser meets
  an import before any directive sees an argument — and where the
  adapter's words are neither an import's nor a directive's own. One
  that every value failed at a directive's check of its argument's form
  (`protocols {$TLS_MIN:tls1.2}`, an `lb_policy`, a `redir` code) is
  not refused for that: an import path, a root, an app's name and a
  backup path each take any token, so a value a directive refused was
  in none of them (#394) — but the trials say nothing of where else the
  variable is written, so each occurrence is tried on its own, in a
  copy of the Caddyfile's directory with that one occurrence made a
  value of its own and the others their defaults. One the adapter
  refuses, quoting the value and not through an import, is the
  directive's (quoted from a snippet or an imported file — which an
  import with arguments may expand again, carrying the value to a path
  — the value is refused as not knowable); one standing where a
  directive's or a
  subdirective's name is read (`{$KIND:files} app.db`) is refused as
  not knowable before any is tried, since a name is checked by form as
  an argument is, and another value names another directive; one that
  adapts and
  moves the root, an app's name or a backup path is refused as any
  variable there is; and the copy's environment gives the variable a
  second value of its own, so an occurrence read past the copy — a file
  imported by its absolute path — shows, and is refused as not knowable
  here. Caddy substitutes `{$NAME}` in the text before it reads a token,
  by the same shape of match, so what is tried is what it substitutes,
  whatever line, heredoc or quoted string it is in.
- An import glob that matches no file. The adapter takes it for no
  error; but inside the plan unit's view, which holds `/etc/motel`
  and nothing else, an import from outside it matches nothing — and so
  does one through a directory the `motel-backup` account may not
  list, or through a link that leads out — and the run would plan
  without the apps declared there. The adapter's own warning is what is
  read, after its snippets, heredocs and `{$NAME}`. A restore and a
  drill plan the same way, and are refused with the run. Not seen: a
  wildcard directory, `apps/*/Caddyfile`, where a link out sits beside
  real directories — the glob still matches the real ones.
- A symbolic link anywhere in a declared path, or at `<app>/shared`:
  declare the real path, and put data on another disk with a bind
  mount, as liveswap itself asks. (The liveswap root may be a link.)
- An `<app>/shared` that does not belong to the `motel` user.
- A declared `files` path that is a FIFO, a socket or a device: a backup
  keeps files and directories, and a restore could not put anything
  else back. (restic does record such a node where it finds one
  *inside* a declared directory; nothing reads it.)
- A run's leftover unit that will not stop within two minutes: the run
  is refused, and the record says why.
- A run directory, `/run/motel-backup`, that is itself a mount
  point: nothing of motel-backup's is mounted there, and what it
  writes and removes there would go through the mount. Every command
  refuses, and names the `umount`.
- By hand, after a command was killed: `motel-backup sweep`
  takes away what it left — the units it recorded, and its mounts
  under `/run/motel-backup`, made private first — as the next run
  would. Never unmount or `rm -r` there by hand.
- A run, a restore or a drill in a mount namespace of its own: the
  manager binds what it sees into a unit, and where it does not see
  the mounts a command makes it binds the bare mount point — root's,
  empty — in the place of an app's data, and the upload is a snapshot
  of empty directories. A service has such a namespace from any of
  `PrivateMounts=`, `ProtectSystem=`, `PrivateTmp=`, `PrivateNetwork=`
  and their kin, which is why the shipped unit files have none of
  them and a drop-in must add none; a command from a shell, where the
  shell has one. Each command makes one mount of its own, of nothing,
  asks the manager for the mount unit it keeps for every mount it
  sees, and refuses where there is none, naming the unit to look at
  (`systemctl cat`): before any unit is started, and before anything
  is uploaded.
- A restore into place of a snapshot that lacks something its own
  `plan.json` declares, or holds a damaged copy: restore it `--to` a
  directory, or restore another snapshot.
- A snapshot whose `plan.json` is missing, is not a valid declaration, or
  holds a field this version does not know.
- `--to` a directory that exists, one under `/var/lib/motel-backup`
  or `/run/motel-backup`, one any directory on the way to which is
  not root's own or can be written by others, and `--to` or
  `--snapshot` given with nothing in it.
- A file that would land on a live database (above).
- A restore or a drill of a snapshot larger than what is free under
  `/var/lib/motel-backup`, and a restore of one larger than half of
  it where the app's data is on the same filesystem.
- A run's own first drill of a snapshot above 1 GiB restored: that one
  is `motel-backup drill`'s.
- A unit whose state cannot be read ten looks running (five minutes) is
  stopped, and that step fails.
- In a run or a drill, a read of the repository — an app's snapshots,
  the run's listing, the look into a snapshot after its upload, the
  size of a snapshot before its fetch — and the writing of a record,
  not answered in 30 minutes: stopped, and said as the repository not
  answering, which it will not do for the next app either, so no other
  app is asked. The run's listing is then a warning, the app's backup
  `incomplete`, a drill's app not proven, a record not written the
  run's warning (the app still `ok`). The slowest measured: a listing
  24.6 s (1,506 snapshots, 28.5 GB, a cold cache, 25 ms each way and
  50 Mbit/s), a snapshot's size 6.4 s (200,203 files, the same link);
  restic itself gives up on a wrong key after about a quarter of an
  hour. It is there for a storage that takes connections and never
  answers, which held the run lock, and every hourly run, for as long
  as restic kept trying. A restore's reads have none (the owner,
  2026-09-30): someone started it and can stop it, and a rebuilt box's
  first listing, from an empty cache, is the longest a box makes
  ("Retention", above). An upload, a fetch and the check itself are not
  bounded: their length is the data's.
- The repository check, where `restic cat config` has not answered in
  30 seconds: that check's verdict is `unreachable`, and the check is not
  made.
- `setup --existing <url>` where the storage takes the key and says
  there is no repository (exit 10 at the look):
  "there is no repository at <url> (exit 10): setup --existing opens a
  repository from an earlier box and makes none; …" — the URL names a
  bucket or path the earlier box's repository is not at, or the
  repository is gone, and on the operator's word that one is there
  nothing is made; the working file is as it was.
- `setup`, before it asks for anything: a repository that is a path on
  this box (`/srv/backups`, `local:`), `sftp:` (ssh takes its key from a
  home directory, which no unit has), `rclone:` (a config file, the
  same), `azure:` (not in Debian's restic), `b2:` (restic's own B2
  backend: the next item), `rest:`, `gs:` and `swift:`
  (not set up by this command: write the file by hand, below), an
  `s3:` URL with an `@` anywhere in it — `user:pass@` before the host,
  with or without a scheme, and a secret holding a `/`, `?` or `#`
  that ends the host before its `@` (a command line and a shell
  history are no place for a secret); no bucket name holds an `@`, so
  what this costs is a path with one, which the refusal says — B2's
  S3-compatible endpoint given as `s3:http://` (B2 takes HTTPS alone),
  an `s3:` URL whose `http`/`https` scheme does not begin in lower case
  (`s3:HTTPS://…`: restic reads it as a host named `HTTPS:` and no
  bucket, and retries without end, `Bucket name cannot be empty`
  [measured, restic 0.18.0]), and a scheme restic does not
  know; restic, sqlite3 or motel not installed (a run, a restore and
  a drill refuse the same, in the same words, before any unit); a
  systemd older than 257; a `motel-backup` account that motel
  did not make — motel makes it with its mark, the comment
  `made-by-motel`, and uses no other, since every restic unit runs
  as it and whoever is it reads the repository credential from a
  running restic's environment: an account of that name made by hand,
  by another package, or by this branch before the mark, is refused,
  never changed, and the remedy is to remove it (`userdel
  motel-backup`) and run setup, which makes it — or, for one you
  made for motel yourself, to mark it (`usermod --comment
  made-by-motel motel-backup`), which keeps the credential file.
  Motel's account is local as well as marked: the one in
  `/etc/passwd`, which only root writes, and the one the box resolves
  by that name, line for line; a directory's account of that name
  (LDAP, sssd) answered first is refused, whatever its comment says. A
  directory the box is joined to is otherwise trusted as root is (the
  owner, 2026-09-27): its administrator can give any account of its own
  the backup account's uid, and whoever holds that uid is the restic
  process. No check on the box stops that; join backups' box only to a
  directory you trust with its data; a Caddyfile a run
  could not plan from; another run, restore or drill under way. **A
  run, a restore and a drill ask the same of the account** before any
  unit — it is the run that puts the credential in that account's
  environment — and refuse one that is not there. `motel-backup
  account` asks it by itself: the package's postinstall does, and warns
  in those words. What root does to motel's account after it is
  made — a shell, a password, a group — is root's: root reads the
  credential file itself. The lookup is given ten seconds; one that
  does not answer refuses the run, and says so.
- A `b2:` repository, restic's own B2 backend, whose library in Debian
  13's restic (blazer 0.6.1) retries a failed upload without end (#177,
  #354): a connection that fails before any answer is made a retry
  (`base/base.go:317-320`, `b2err{retry: 1}`; `Action`, `:70-71`), and
  `withBackoff` (`b2/backend.go:749-767`, under the upload at `:505`)
  tries it again on the same upload URL with no limit. `setup`, before it asks for anything: "b2: is not supported:
  B2 is reached at its S3-compatible endpoint,
  s3:https://s3.<region>.backblazeb2.com/<bucket>/<path>, with an
  application key, not the master key; restic's own B2 backend retries
  a failed upload without end" — the bucket and path filled in as
  restic reads them from the `b2:` URL (the path cleaned, its leading
  slash dropped, and left out when there is none; a bucket that is not
  lower-case letters, digits and hyphens, as B2's S3 endpoint names one,
  left as `<bucket>`; each path
  segment URL-escaped, an `@` as `%40`, so that restic, which
  `url.Parse`s an `s3:https` URL, reads back the path B2 holds and not
  a shorter one at a `#` or a `?`, and setup takes it; a path that
  still climbs after cleaning, `../x`, is left as `<path>`, since a
  server on the way may resolve an S3 request's dot segments), the
  region left for the operator, since a `b2:` URL does not
  hold it. A run, a
  restore and a drill read the credential file's `RESTIC_REPOSITORY`
  as the manager would (the last assignment) and refuse a `b2:` one
  before any unit, the run recording it as its error: "the repository
  is b2:, which is not supported: as root, motel-backup setup --existing
  's3:https://s3.<region>.backblazeb2.com/<bucket>/<path>' moves it to
  B2's S3 endpoint, with an application key; restic's own
  B2 backend retries a failed upload without end" — the URL in single
  quotes, so that the command pasted as it is reaches setup whole, the
  escaping leaving no `'` in it, and short enough that the record's
  300 runes keep all of it for a bucket and path of up to 58
  characters as escaped. A credential file they cannot read is refused there
  too, "the credential file <path> could not be read: <why>", where
  every unit would otherwise fail to start — one that is not a
  regular file among them, "it is not a regular file (p---------)"
  for a FIFO, which is opened without waiting on it — or that the
  manager refuses whole, "the credential file <path>: line <n> holds a
  NUL byte: …" or "…: line <n>: the value of <KEY> holds a byte that is
  not UTF-8: …" (named by its key, from the line the key is on, the
  byte perhaps lines further on; a key that is not UTF-8 is "a key";
  in a comment the manager takes such a byte, and so does the run
  [M91]); and so is
  one that sets `RESTIC_REPOSITORY_FILE`, from which restic would take
  a repository no refusal here reads: "the credential file <path>
  names the repository in RESTIC_REPOSITORY_FILE, which is not
  supported: name it in RESTIC_REPOSITORY". `status` as
  root says setup's line as a warning, `warning: <file>:
  RESTIC_REPOSITORY: b2: is not supported: …`, its exit status
  unchanged; and of a file written by hand for B2's S3-compatible
  endpoint with no region, `warning: <file>: the repository is at
  B2's S3-compatible endpoint and AWS_DEFAULT_REGION is not set, or set
  to nothing: add
  AWS_DEFAULT_REGION=<region>, as setup writes it`, or with another
  region than the endpoint's name holds, `… in <region>, and
  AWS_DEFAULT_REGION is <other>: set AWS_DEFAULT_REGION=<region>, as
  setup writes it` — neither of a URL setup refuses, which status has
  already said.
- `setup`, at a prompt: an empty value, one with a line break or a
  control character, or one that is not UTF-8 — the file cannot hold
  it — three times; and a password not confirmed `stored`.
- `setup`, after two minutes with no answer from the repository (ten
  seconds for the look that comes first): the look or the opening is
  stopped, and nothing of setup's has been written (an opening stopped
  between writing restic's lock file and removing it leaves it, and
  restic never passes a stale lock by itself [M75], which the
  opening's clock line says: `restic unlock`, with a key that may
  delete, removes it from the box at once once setup has ended, given
  that key's credentials, if no other machine with this hostname runs
  restic against the repository — restic takes a same-host lock whose
  pid is gone for stale [M4 cases C, D], and a live lock that a second
  machine of the same hostname wrote too [M4 case E] — otherwise from
  another machine that holds it once it is half an hour old, or with
  `--remove-all` when nothing else holds the repository); init is left
  running, said, and waited for by whatever comes next.
- `setup`, for a repository that is there already, or one it has just
  made: a key that cannot write a lock file to it, or a storage that
  refuses the write, on one neutral line with what restic said; and a key
  that wrote the lock file and could not remove it, by restic's own
  word that it could not, `error while unlocking:` [M4 cases A, B]
  ("Setup", 7).

## The Caddyfile is read without the server's environment

The plan comes from `/etc/motel/Caddyfile`, which is root's, and not
from the running server: what gets backed up is not for the process the
internet talks to to say. The cost is `{$NAME}`, which Caddy fills in,
in the text, from the environment of whoever adapts the file — and a
run does not have the server's environment, where its secrets are.

So a run adapts the file with made-up values. A variable written
somewhere with no default would otherwise leave nothing where its value
goes — `email {$ACME_EMAIL}`, unset, is a parse error — so each gets a
placeholder of its own, and where the adapter refuses one (it quotes the
value: `invalid port 'hsb-placeholder-metrics-port.invalid'`) the next
kind is tried there: a number, a duration, a path. Each made-up value
is one token, so what is adapted has the structure the Caddyfile has on
the page, and none of those values is anywhere a plan looks.

Then every variable is given a second value. If the plan comes out
different — the root, an app's name or a backup path depends on it — the
run is refused, naming the variable: the server and the backup would
otherwise look in different places, and the backup would say "no data
yet" for ever. Write those three things literally; a per-box `import` of
a file with a literal `root` does what a variable would.

If no value tried adapts, the run fails with the adapter's own error,
which names the file and line; a default that adapts
(`{$NAME:value}`) settles it — unless the error is the parser's about
an import, in which case the run says so: a default moves the import
to the default's file here and the server's elsewhere, so the variable
would be refused as an import's next, and only a literal path settles
it. A variable with a default that no other value adapts with is read
by what the trials failed at, and then one occurrence at a time
(above, "What it refuses").

**What this cannot see.** Caddy substitutes `{$NAME}` into the text
before it reads a single token, so a value is not bound to be one token:
with a space in it `command {$CMD}` is two arguments, and with a newline
in it, it is more lines of Caddyfile — `CMD` set to `./server`, a
newline, and a `backup` block gives the server's `blog` a declaration
that is nowhere in the file [measured]. One-token values cannot show
that no value would do that, and the values the server has are not
known here. So: **a declaration that exists only through the server's
environment is not backed up, and nothing says so.** That environment
is root's to write (`motel.service`), like the Caddyfile's own
directory; keep what it holds to values, and what is declared in the
Caddyfile.

## Setup

`motel-backup setup [--existing] <repository>`, as root, at a terminal. The
repository is a restic URL — `s3:https://s3.example.com/bucket`, or
on B2 its S3-compatible endpoint, `s3:https://s3.<region>.backblazeb2.com/bucket`
— and not a secret; the
secrets are typed at the terminal, never given on the command line.
`--existing`, before or after the URL, is the operator's word that a
repository is there already — a rebuilt box's (the guide, "A rebuilt
box") — and changes steps 5 and 6: no password is made or shown on any
path, the repository's own is asked for, and a storage that says there
is none ends setup. What can be known to fail is refused before
anything is asked for ("What it refuses"). In order:

1. the `motel-backup` account is made if it is not there
   (`useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-motel motel-backup`
   — the package's postinstall makes it with the same line, so on a
   `.deb` box it is there already), and one that is there is used
   only where motel made it, which its comment says ("What it
   refuses"; every run asks the same);
2. the run lock is taken and held to the end — a backup run that comes
   due meanwhile says who holds it — the state and run directories are
   made as a run makes them, `/etc/motel-backup` is made (root,
   `0755`: anyone may see that the file is there and when it was
   written, which is how `status` tells a fresh setup from one that
   never ran; nobody but root what is in it), and a file an interrupted
   setup left beside the credential file (`repository.env.staged`, or
   the dotfile `envfile` makes on the way to it — the two shapes
   `envfile` itself names) is removed and said — a copy an operator
   keeps there under another name is left alone;
3. the plan is read from `/etc/motel/Caddyfile` as a run reads it,
   and said — the apps a run would back up, or that no app declares a
   backup yet;
4. the storage key id is asked for, with echo on, and the secret key
   with echo off — on `/dev/tty`, whatever stdin and stdout are, so
   that nothing stdout or stderr is sent to holds them; everything
   setup says goes there too, the password above all (what records the
   terminal itself — scrollback, `sudo`'s I/O log, `script` — holds
   what was shown). The error a setup ends on goes to stderr, as every
   command's does. They are `AWS_ACCESS_KEY_ID` and
   `AWS_SECRET_ACCESS_KEY` — on B2, an application key's keyID and
   applicationKey. For a host that is `s3.<region>.backblazeb2.com`,
   the region two letters, a word and three digits (`eu-central-003`) —
   in ASCII, in any case, read past a port or a trailing dot (the
   region is the name's; whether B2 takes a host written so is not
   measured), but never a longer name ending so — the file also gets
   `AWS_DEFAULT_REGION=<region>` (in lower case), not asked, and setup says "B2's
   region, <region>, from the endpoint's name, goes in the file as
   AWS_DEFAULT_REGION"; any other host gets nothing more. With it
   restic never asks B2 where the bucket is (its S3 library, minio-go,
   asks only when no region is set). The guide's B2 keys may not ask
   it — B2 puts Get Bucket Location under `readBuckets`, and on the
   refusal minio-go signs every request for `us-east-1` — and minio-go
   does not retry it on a 5xx with no S3 error code in it, which is how
   B2 answers one: "Codes in the 5xx range … include JSON diagnostics",
   `{"code":"internal_error",…}` (Backblaze,
   [Introduction to the S3-Compatible API](https://www.backblaze.com/apidocs/introduction-to-the-s3-compatible-api)).
   So one 500 there puts restic's two retry lines beside a listing,
   which is then not believed; with the region set the request is not
   made, and the same 500 elsewhere is absorbed in silence [M89: that
   documented shape injected through a proxy, not an answer of B2's].
   Nothing else is set up by this
   command (`rest:`, `gs:` and `swift:` are written by hand, below). A
   value the file
   cannot hold (a line break, a control character) is asked for again,
   up to three times; one the plain form would not carry — whitespace
   at either end, a leading quote — is written double-quoted, as the
   manager reads it [measured];
5. the file is written beside the working one — `repository.env.staged`
   (one name: setup holds the run lock), root-only, with a throwaway
   password — and the repository is
   **looked for**: `restic cat config` as `motel-backup`, under a
   ten-second clock (two minutes with `--existing`, below), said as
   "looking for a repository at …". With a
   right key restic says at once whether there is no repository (exit
   10) or one this password does not open (exit 12) [measured]. A
   storage that **refuses at once** — restic's exit 1 within the clock:
   B2's own API answered a wrong key or a bucket the key may not reach
   with `401` through restic's own `b2:` backend [measured, on real B2,
   RC check 1 of `v0.3.0-rc1`];
   S3 an `AccessDenied`, which restic 0.18.0's
   retry wrapper treats as permanent under its default feature set
   [read in its source, `internal/backend/s3`; measured on B2's S3
   endpoint, which the box uses: a wrong secret, `Stat: Access
   Denied`, three askings in 0.66 s, RC check 1 of `v0.5.0-rc1`] — is said
   in restic's own words ("restic could not open the repository (exit
   1): …"; any other exit 1 within the clock is said the same, with
   restic's words, see the residuals), and
   the key id and secret are asked for again, up to three times in all
   with init's refusals below, before any password is made: init's
   first step is the same opening, and would be refused the same. Where
   restic is still **retrying at the clock** — S3 on a bucket not there
   yet, a wrong secret or a host that does not resolve [measured, on
   rclone's S3 server; B2's S3 endpoint says a bucket not there at once,
   as none (exit 10), and init meets it, below: RC check 1] — the
   look is given up on, said as such ("no repository answered within
   10s": the three look alike there), quoting what restic said of the
   storage before the stop — its last line that is not the stop's own
   (the signal, the `Fatal: … context canceled` the stop makes of the
   opening, and the "Is there a repository at the following location?"
   and URL lines after every failed opening, which say nothing of the
   storage), which is its last retry line where it retried: "(restic
   said: Load(<config/>, 0, 0) returned error, retrying after …: …)"
   [measured: a wrong secret on the e2e's S3 server, "The request
   signature we calculated does not match"; a host that does not
   resolve, "no such host"] — since nothing else keeps it (the run
   directory goes with setup; RC check 7's slow look left no trace),
   and init answers instead. A look
   that failed on its own account — a unit the manager could not set
   up, one that could not be started — is said as that, before any
   password is made. **With `--existing`**, the look has setup's
   two-minute clock, not ten seconds — the operator's word is that a
   repository is there, so a storage slow to answer is waited for, and
   the question is never init's — and its answers are taken
   differently: one found (exit 12) is opened with its own password, as
   below; none (exit 10) ends setup — "there is no repository at <url>
   (exit 10): setup --existing opens a repository from an earlier box
   and makes none; check the URL — the bucket and the path — against
   the earlier box's, or run setup without --existing to make a new
   repository there" — with nothing written; a key refused at once is
   asked for again, as without the flag (the third refusal's ending
   adds the way out below); and a look given up on at
   that clock is said ("no repository answered within 2m0s (restic
   said: …): a bucket not made yet, a wrong key and a wrong host look
   alike here; with --existing no password is made") and the key asked
   for again, in the clock's own words ("the key id and secret again: a
   wrong key is one of the three; the same key, if the storage was only
   slow to answer, has the look run again"), up to three times in all
   with refusals; the third ends setup with the same words and the way
   out ("; check the URL and the key against the earlier box's, and run
   setup again; nothing of setup's has been written"; the plain path is
   not offered: on a bucket mistyped, with a key that may make one, init
   there makes a new repository, the outcome the flag exists to keep a
   rebuilt box from) — the look only reads, so no
   lock is in question; restic's retry line is quoted where it wrote
   one before the stop [measured: a host that does not resolve, e2e,
   "no such host" where the resolver answered at once].
   Init never runs with the flag. The flag
   is for the one case that met the clock in the field: B2's S3
   endpoint slow to answer a rebuilt box's look, once in four [RC check
   7], which without it shows a password that is then not needed and
   asks for the repository's own after (#389);
6. where none was found — and `--existing` was not given — a repository password is made — 32 random
   bytes, base32, 52 characters — and **shown**, once, with what to
   store beside it (the repository URL and the storage key: with those
   three `restic -r <url>` reads every backup from any machine;
   without the password nothing can); where the look could not tell,
   it is to be stored beside any password already kept for that
   repository, never in its place, since a repository may be there
   after all (a rebuilt box's, on a storage slow to answer: B2's S3
   endpoint, the look stopped at its clock once in four [measured, RC
   check 7]) and the password kept is then the
   only way to read it; the operator types `stored` to
   go on, and gets one more asking for any other word. Then
   `restic init --json` runs with that password, under a two-minute
   clock; after 20 seconds a person waiting is told what for, and what
   Ctrl-C would do — end the waiting, not the unit, which may be making
   the repository with the password shown, so that password is to be
   kept (for the look and the opening, which only read, Ctrl-C stops
   the unit, and is safe). Init is never stopped, by Ctrl-C or by the
   clock: stopped half way it would leave a key file and no config
   (restic 0.18.0 writes the key first, then the config), which nothing
   opens and a later init will not write over, where left to its few
   seconds it makes the repository with the password that was shown —
   what the next setup asks for. It is recorded instead
   (`/run/motel-backup/init-unit`) for the next run or setup to
   wait for, up to three minutes, saying which unit, and the message
   names it and the `systemctl stop` that ends it if it must be. restic answers at once whatever is
   wrong with the key, the host or the port [measured], and setup then asks for the
   key id and secret again, up to three times, the one password
   standing: it has taken effect nowhere until the repository is made
   with it. When setup ends after the showing, the error it ends on
   says what became of the password, from what restic said: before
   `restic init` has been started with it (a refusal at the `stored`
   prompt, Ctrl-C there, a file that could not be written), "never
   used: discard it"; the same where every init started with it was
   refused at the opening — restic's "unable to open repository at …",
   its word for a backend it could not open, which comes before anything
   of a repository is written (restic 0.18.0, `cmd/restic/global.go`):
   a key refused, a bucket out of reach, a host that does not resolve.
   One of those ends setup at once, with no asking for the key again: a
   **bucket not there** that the key may not make, restic's S3 backend's
   `client.MakeBucket: …` after its echo of the URL — on B2's S3
   endpoint `not entitled`, after a look that said none at once (exit
   10), so the password has been shown [measured, RC check 1] — since
   the guide's keys never may make a bucket and a key typed again is
   refused the same; setup names the bucket, to check its name or make
   it first. Once an init has got past the opening — "create key in repository
   at … failed", Ctrl-C or the clock during init, any failure after —
   "keep the password shown above: restic init ran with it, and may
   have made the repository", since an init still running, or one that
   failed past the opening, may have written some or all of one, and
   the next setup looks first and asks for it if the repository is
   there (one such init keeps the password through any later refusal
   at the opening); and where init itself answers that the repository
   exists, it made nothing with the password, and "discard it" is said
   again. Where init answers that the storage holds **key files or
   snapshots and no config** ("repository already contains keys", or
   "… snapshots": a repository whose config was removed — a lifecycle
   rule, a deletion — or an init that ended between its two writes),
   nothing opens them as they stand and init will not write over them,
   so setup ends at once — no asking for the key again — says what is
   there and the way out, recovery before removal: if a backup was ever
   made there, its earlier state is recovered from the storage's kept
   versions first ([After an attack](../docs/backups.md#after-an-attack))
   and nothing removed; only an unfinished repository that never held
   one is removed at the storage; or setup is given another path. Setup
   cannot tell the two apart, so it never removes anything. And
   "discard it": whatever an earlier init of this setup may have
   written there, nothing opens it. The clock is
   for a storage that takes the connection and never answers: the
   waiting ends, and the unit runs on, recorded as above, with the
   staged file it reads and the run directory it writes to left in
   place for it — the manager opens both in the unit's first moments,
   which the clock or Ctrl-C may come before — and the next run or
   setup, having waited for it, removes both and says so;
7. a repository that **exists already** — a rebuilt box, a bucket
   reused — is asked for its own password (echo off), and nothing is
   made or shown; setup opens it with `restic cat config` **taking the
   repository's lock** — the one unit proves the password, the id, and
   that the key may write and delete a lock file, since one is written
   and removed on the way (0.7 s where the key may write [measured]).
   That is the repository's locks/ alone: a policy that allows locks/
   and denies data/, index/ or snapshots/ passes, and the first run
   fails on it, named ("Accepted residuals"). The look alone reads
   without a lock. A wrong password is
   refused (exit 12) and asked for again, up to three times. What
   restic said is read, and nothing is inferred beyond it:
   - exit 0 with restic's own `error while unlocking:` line on stderr:
     it wrote its lock file and could not remove it [M4 case A, three
     times out of three; case B, after a minute of retries]. Refused,
     quoting restic's `Remove(<lock/…>) failed:` line where it wrote
     one, which names the lock; where the DELETE failed and was retried
     rather than refused, restic writes no such line [M4 case B], and
     its last `Remove(<lock/…>) returned error, retrying after …` line,
     which holds the storage's answer, is quoted with its `error while
     unlocking:` one. A key that may write
     and not delete (narrower than the guide's policies, which grant
     the delete — on B2 restic's S3 backend deletes by name, which
     leaves a hide marker, which `writeFiles` covers without
     `deleteFiles` [measured, B2's S3 endpoint, RC check 3 of
     `v0.5.0-rc1`], as Backblaze's own page says in prose, though its
     table lists Delete Object under `deleteFiles`)
     would leave one more lock with every run after, which restic
     never passes by itself [M75]: the first prune, or a check from
     elsewhere, would exit 11 for good. The refusal names both causes
     that look alike from the box, a key that may not delete and a
     storage that refuses or does not answer deletes, and where the
     lock goes: `restic unlock`, with a key that may delete — not the
     one refused, and not one the box holds — removes it from this box
     at once once setup has ended, given that key's credentials, if no
     other machine with this hostname runs restic against the
     repository (restic takes a same-host lock whose pid is gone for
     stale [M4 cases C, D], and so strips a live lock that a second
     machine of the same hostname wrote, an old box still running
     beside its rebuild, with exit 0 and nothing said [M4 case E]: a
     prune from elsewhere could then delete what that machine's backup
     is writing); otherwise from another machine that holds it once it
     is half an hour old, or `restic unlock --remove-all` sooner, when
     nothing else holds the repository;
   - exit 1 — a write refused (S3's 403 [M3]), a storage gone, a read
     that failed — or the clock, where restic retries (B2's own API
     answered such a key `401`, which restic's own `b2:` backend
     retried for a quarter of an hour [measured, RC check 6 of
     `v0.3.0-rc1`]; B2's S3 endpoint answers it `client.PutObject: not
     entitled`, at once [measured, RC check 6 of `v0.5.0-rc1`]): one neutral line. Exit 1 is "restic could not open the repository with
     its lock (exit 1): …" with all restic said, cleaned — restic
     prints hint lines last [M4 case G], and the cause is before them —
     less restic's retry lines ("… returned error, retrying after …")
     wherever another line remains, the last of them kept where they are
     all it said, and then cut at 300 characters; the clock is "restic had not finished
     opening the repository with its lock within 2m0s" with the last
     line restic said, and that a lock file restic was writing may
     stay, with where `restic unlock` clears it, as the interrupt says.
     No cause is named, since a key replaced for a storage that
     throttled reads is the wrong one; a run with a key that cannot
     write fails that long after it starts, every hour. An exit 1 whose
     stderr also holds restic's `error while unlocking:` line — it wrote
     its lock, then failed, then could not remove it — says so after the
     neutral line, with restic's words and where `restic unlock` clears
     the lock;
   - exit 11, another restic holding the repository (a prune or a check
     from elsewhere): restic looks for other locks before it writes its
     own, and leaves nothing [M4 case G]. The opening is read again
     without the lock, which proves the password and the id; the holder
     is said as restic named it, with "whether this key can write to
     it, or remove a lock file, was not checked" and that restic never
     passes a lock nothing holds by itself — if the holder is gone,
     `restic unlock` clears it from the holder's own host at once, from
     elsewhere once it is half an hour old, or with `--remove-all` —
     and setup goes on.

   A stop or Ctrl-C between the lock file's writing and its removal
   leaves it, which restic never passes by itself [M75]; the waiting
   note and the interrupt say so. Running `restic unlock` after the
   opening, in place of reading restic's line, was measured and set
   aside: from this box it removes, at once and with exit 0, a live
   lock written under this hostname by a process the box does not run
   [M4 case E] — a second machine of the same hostname, an old box
   still running beside its rebuild. A repository setup itself made was
   written by its init, which proved the key may write and deleted
   nothing: it is opened with the lock once, so that restic's word on a
   lock file it could not remove refuses a key that may not delete on a
   first setup as on a rebuilt box. One an earlier attempt of this
   setup made, opened with the password shown, was made with that
   attempt's key, and the key asked for since is held the same. Where the look could not tell and init finds the repository
   there after all ("already initialized", whatever the password), the
   same follows, and the password just shown is said not to be the
   one;
8. once the repository has answered, the record (`status.json`) is
   put aside as `status.json.aside-<time>`, and why is said, if it was
   written against another repository, or there was no file before, or
   this setup made the repository — a repository just made holds none
   of the record's snapshots, whatever its URL: a run then drills what
   it backs up into the repository now in use, and `status` does not
   say "proven" of a snapshot this repository does not hold. A
   repository is known by its id, not its URL — a bucket emptied and
   made again is another repository at the same URL, and one reached
   by another URL is the same — and setup keeps the id of the one the
   file names beside the record (`/var/lib/motel-backup/repository-id`,
   for everyone to read, as the record is); a record with no id beside
   it (runs from a file written by hand) goes aside too. The same
   repository, opened, keeps its record;
9. then the file takes the working one's place — whole, root `0600`.
   The record goes aside first, and comes back if the file cannot take
   its place; the id is written only once the file is in place and on
   the disk, so that a power cut leaves the new id beside the old file
   never, and the old id beside the new file at worst — which puts the
   record aside once more; each step is on the disk before the next,
   so that nothing that ends setup between them leaves a credential
   file with another repository's record beside it. Whatever fails
   after the file is in place is said with where the record went, and
   what became of the password. Then setup names the
   repository, whether it was made or opened, and its id — and, when
   `/etc/motel/backup.env` from before this version is still there,
   that it is, and to remove it: `/etc/motel` is the box channel's
   (below).

Whatever ends setup before the last step — Ctrl-C ("interrupted:
nothing has been written" before the opening; "interrupted: nothing of
setup's has been written; a lock file restic was writing may stay …"
from it on), a refusal, a kill — leaves the working file
byte for byte as it was, or absent as it was, and no copy of a
credential beside it — but for a `restic init` left running, which
keeps its staged file; a `kill -9` at any point after the storage key
was typed leaves the file under its staged name the same, root-only,
holding that key and — after `restic init` began — the password. The
next setup removes it first, and says so. A password that has
taken effect anywhere has been shown, and confirmed stored, before it
did: a setup killed after `restic init` made the repository is followed
by one that says the repository exists and asks for the password that
was shown.

`/etc/motel-backup/repository.env` is not under `/etc/motel`:
that is the box channel's — its Caddyfile and, under `secrets/`, each
app's env file, which the motel user reads — and the repository
credential is root's alone.
A `/etc/motel/backup.env` from an earlier version of this branch is
not read; a run and `status` say so, and what to do: run setup with
its `RESTIC_REPOSITORY`, type its `RESTIC_PASSWORD` when asked, then
remove it. With no credential file at the new path, setup cannot tie
the record to the repository and puts it aside; the next run drills
what it backs up.

A box whose file names a `b2:` repository is refused every run until
it is moved, by the one `setup` the refusal names — the bucket's S3
URL, the region filled in from the bucket's endpoint — with the same
application key (not the account's master key, which B2's own API
takes and its S3 endpoint does not) and the repository's own
password: the look finds the repository, it is opened and held to
writing (step 7), and, its id unchanged, the record is kept (step 8);
the file is written whole, without the old `B2_ACCOUNT_*` lines. The
URL names the same objects: restic's two backends lay a repository out
alike under the bucket and path, which the refusal carries over as
restic's b2 backend reads them (`b2AsS3`, `backups/engine/setup.go`).
A real bucket answered both with the one id [measured, B2, RC check 9
of `v0.5.0-rc1`: `existing, id a1ec8733`, as before the move].

With the package, the first backup runs from `motel-backup.timer`
within the hour and ten minutes of setup ("On a fresh box"), or now
with `systemctl start motel-backup.service`; setup's last line
says so. Without it, run `motel-backup run` hourly from a timer or
cron entry of your own, or `status` goes unhealthy after 3 hours.

### By hand

For `rest:`, `gs:` and `swift:`, or a provisioning tool, the file setup would
have written — one `KEY=value` a line, a backslash doubled, a value
with whitespace at an end or a leading quote in double quotes with `\`
and `"` escaped, as systemd reads it:

```
# /etc/motel-backup/repository.env — root:root 0600, in a root 0755 directory
RESTIC_REPOSITORY=s3:https://…/bucket
RESTIC_PASSWORD=…
AWS_ACCESS_KEY_ID=…
AWS_SECRET_ACCESS_KEY=…
```

plus the account, as setup makes it (above; the package's postinstall
has made it already on a `.deb` box), and `restic init` the way
a run's units get the file: read by the manager, as root, and handed
to restic running as the account — never sourced by a shell, whose
`$`, backticks and `;` are not systemd's, and never with a secret on a
command line — with the cache where the units keep it, owned by the
account:

```sh
systemd-run --quiet --pipe --wait --collect \
  -p User=motel-backup -p EnvironmentFile=/etc/motel-backup/repository.env \
  -p CacheDirectory=motel-backup -E RESTIC_CACHE_DIR=/var/cache/motel-backup -E HOME=/nonexistent \
  /usr/bin/restic init
```

On B2 the URL is its S3-compatible endpoint and the file has one line
more, `AWS_DEFAULT_REGION=<region>`, the endpoint's second part, as
setup writes it; a file that names a `b2:` repository is refused by
every run, restore and drill before any unit ("What it refuses").

Pointing the box at **another repository** this way leaves
`status.json` speaking of the old one, and `repository-id` beside it
naming the old one: remove both with the change, as setup does — an
id left behind would tie a later record to the wrong repository.

## Accepted residuals

Findings scored by the repository's triage rule
(`.claude/skills/triage/SKILL.md`) and accepted; the scores are
severity × occurrence × detection, then the fix's size and risk.

| Failure | Where | S | O | D | Product | Fix | Why accepted |
|---|---|---|---|---|---|---|---|
| `status` whose report could not be printed (stdout on a full disk; a closed pipe where SIGPIPE is ignored) exits 1, the code of unhealthy backups, so a caller cannot tell the two apart by the code; and an unhealthy box's own error is replaced by the print's. | `showStatus` (`cmd/motel-backup/main.go`) | 2 | 3 | 1 | 6 | size 2, risk 2 (an exit status of its own, documented, for a print that failed) | Rule 2 holds: the command ends red and names the cause on stderr ("the report could not be printed"), and a reader got no report to trust (2026-11-13, from the self-review of `--json`). Exit 0 there would be green with nothing behind it, which the first fix did and the second pass found. Could not tell stays 3. No test reaches the report path's print failure: the paths are the box's own, and on a dev machine with none `status` never gets past could not tell. |
| With `--existing`, a host that does not resolve, or a bucket not there on an S3 server that retries it, is waited for to setup's two-minute clock, up to three times with the key asked for again between, though restic's retry line, which the clock's message quotes, names the cause ("no such host"; "The specified bucket does not exist"). | `Setup`, `look` (`engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 2 (a reading of the retry line's words, as `backendRefused` reads init's, to end the look at once) | Under rule 3 the product is under 20 and the fix is a branch on restic's wording (2026-11-06, from the self-review of #389's fix). The message is right and names the cause; the cost is the wait, bounded, with Ctrl-C said safe after 20 s. The plain path meets the same two cases at once, through init. |
| A look that exits 0 — the throwaway password opened the repository, or restic printed a config it does not read — is said as "no repository answered within <the look's clock>" — 10s, or 2m0s with `--existing`, where the key is then asked for again — when restic answered at once. | `look`, `Setup` (`engine/setup.go`) | 2 | 1 | 3 | 6 | size 2, risk 1 (a word of its own for exit 0) | Under rule 3 the product is under 20 and the fix is a branch (2026-11-06, from the self-review of #389's fix). A 52-character random password opening a repository, or restic 0.18.0 printing a config without an `id`, is nothing measured; and what follows is right either way: init, or with the flag the opening, answers, or with the flag setup ends as at the clock). |
| A candidate validated in a directory others may not enter, which imports a file beside it, is refused at `validate` for that directory ("which others may not enter"), though a run reads the file installed at `/etc/motel/Caddyfile`. | `checkBackups` (`cmd/motel/backupcheck.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (stop a candidate's walk at its own directory) | Rule 2 holds (refused, the directory named); under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-18). A box push carries no import, so step 6 never meets it; and a candidate's relative imports resolve beside the candidate, not where it will be installed, so a candidate validated away from `/etc/motel` is asked about other files than a run would read anyway. |
| The box suite's push of a backup path into a sibling's `shared/` (e2e box apply 3) holds the refusal to `motel validate:` alone, so a push refused at step 6 for another reason would pass it too. | e2e/box/apply.sh, box apply 3 | 3 | 2 | 3 | 18 | size 1, risk 2 (a substring of the bounded refusal: tried, and it flaked, since Caddy's module chain before the reason follows its servers' map order) | Under rule 3 the product is under 20 (2026-10-18). The same file is shown to fail `motel validate` for the path just before the push; the refusal can name the path once step 6's bound keeps the reason rather than the head of Caddy's line (owed on main, in box). |
| On a box with a box repository, an operator who follows the guide's "without one" branch edits `/etc/motel/Caddyfile` in place and reloads; the next push finds the edit and puts the commit's file back, the edit's backup block with it unless it was also committed. | docs/backups.md, "Set it up", step 1 | 3 | 5 | 1 | 15 | size 1, risk 2 (a sentence no test checks) | Rule 2 holds: the push ends applied, the box correct by its own contract (the signed file is the whole config), and it names the out-of-band edit (`caddyfile_edited_out_of_band`, a warning in the template's `bin/apply`). If that leaves an app with no backup block, the next run says once that it no longer declares a backup, naming its last snapshot — only where the record before held one: an app never backed up leaves in silence (`engine.go`, the run's `apps`; `TestAnAppThatLeftThePlanIsSaidOnce`). A path dropped from a block still declared is not said either. Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-18). The step names the box repo first and says to skip the two lines there. |
| With a key that may read but not write — one that lost writing after `setup` (a bucket policy, a key replaced by a narrower one), or one `setup` took because another restic held the repository at the opening (exit 11, said) — every drill fetch and every `restore` spends restic's quarter of an hour retrying the lock's `Save` before exit 1 (`restore` fetches the same way, and on a rebuilt box nothing is backed up first), and a fetch's exit 1 is that app's own, so a drill of N apps holds the run lock for about 15 N minutes, hourly runs refused meanwhile; the line, "restic could not restore all of it (exit 1)", is said of a fetch that restored nothing. | `fetch` (`backups/engine/restore.go`, the exit-1 branch: `wide` false; `drillApp` returns `errors.As(err, new(repositoryWideError))`; `Drill` sets `refused` on a wide error alone; `restore` reaches `fetch` through `bring`) | 2 | 1 | 1 | 2 | size 2, risk 2 (a branch on the fetch's `--json` summary, absent when the lock was never written; and a decision on `wide`, where a snapshot path not held also exits 1 with nothing restored; restic's stderr is not to hand — the unit keeps stdout alone, stderr goes to the journal, and its other lines carry an app's file names — and `--no-lock` on the fetch would drop the lock that keeps an off-box prune away from it, unmeasured) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-22, #170, PR #346; narrowed 2026-10-23 when the opening was made to take the repository's lock, writing a lock file before the key is taken, RC check 6 — the key the row first described is refused at setup, so what is left is one that could write then and cannot now). Red and named by the drill's own line: it exits 1 with "restore not proven", names no wrong cause, and gives the exact `journalctl -u …fetch…` command, where restic's `Save(<lock/…>)` lines name the key. On the timers the hourly runs have been red for days before a Sunday drill (RC check 6, on `v0.3.0-rc1`, whose setup did not check, through restic's own `b2:` backend: the run 868 s, the drill 832 s); a drill or a `restore` run by hand before any run has no run line to lean on, and `status`'s `running:` line shows the fetch and since when without flagging it. Exit 1 stays app-own by design: one app's failure never skips the next (the 2026-09-19 review, where it once did). The owner chose the one neutral line over a wording per cause. |
| At the opening's clock, the line appended is the last restic said before the stop's own — known by restic 0.18.0's wording ("context canceled", "signal terminated received") — so a restic that rewords its stop would have its stop line quoted in place of the storage's. | `lastSaid` (`backups/engine/setup.go`) | 2 | 2 | 3 | 12 | size 2, risk 2 (cut at the stop's moment instead of its words: the unit's stderr carries no timestamps) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-23, the write-check PR). The e2e's clock case asserts the retry line is the one appended, so a rewording goes red there before it ships. |
| An exit 1 at the opening whose words end in "context canceled" are quoted as restic said them, among the storage's own. | `withoutRetries` | 2 | 1 | 3 | 6 | size 1, risk 2 | restic's stuck-request watchdog is five minutes and the opening's clock two, so within the clock the only cancel is setup's own, which is passed over; accepted (2026-10-23, the write-check PR). |
| At the opening's clock, a stop of the unit the manager could not confirm ends setup in the runner's words ("the unit could not be confirmed gone: motel_backup_open_…"), with no word of the key, the lock file that may stay, or `restic unlock`; and the reopen's clock (after exit 11) ends in the runner's bare clock line, with no word that the exit-11 opening may have left a lock. | `openRepository` and `reopen` (`backups/engine/setup.go`), the `err != nil` arms; the look's clock ends the same way | 2 | 2 | 1 | 4 | size 2, risk 2 (a wording for `unit.ErrNotConfirmedGone` in the opening, as `check.go`'s probe has) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-23, the write-check PR, scored three times). The line names the unit, and `systemctl status` of it shows restic still at its retries; the operator's remedy is the one the clock refusal gives. |
| A Ctrl-C at the opening that lands before restic has started (between the password's prompt and the manager taking the unit) ends with the interrupt's word that a lock file restic was writing may stay, though nothing ran. | `openRepository` sets `writing` before the unit; `err` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 2 (set the flag from the runner once the unit has started) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-23, the write-check PR). "May stay" holds either way, and `restic list locks` from your own machine answers it; the reopen after an exit 11 runs on the same context, and an interrupt there says the same, which is right of it: the lock-taking opening has run. |
| After a successful init, the opening with the lock that follows it (for restic's word on a lock file it could not remove) meets a storage slow to show the new repository (exit 10, or 12), or a transient exit 1 or its clock, and setup ends — "does not open yet … run setup again", or the neutral exit-1 or clock line — after the password was shown and stored. | the init path of `Setup` (`backups/engine/setup.go`) | 2 | 3 | 1 | 6 | size 1, risk 2 (skip the opening after init, which leaves a key that cannot delete unchecked on a first setup) | Rule 2 holds: red, named, with "keep it" for the password and the remedy — setup again looks first, finds the repository, asks for the password shown, and opens it with the lock. Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-24, the write-check PR; extended to the transient exit 1 and the clock 2026-10-25). |
| The locking opening proves only that the key may write and delete under the repository's locks/: a policy that allows locks/ and denies data/ (or index/, snapshots/) passes `setup`, and `repository ready` is said. | `openRepository` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 3, risk 2 (write and remove a test object under data/: a new state in the repository, and an object a prune must know to pass) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #352's redesign). Red and named: the first run, within the hour, fails loudly with restic's own refusal in its line and journal; the guide's policies grant the prefixes together. |
| The locking opening reads every lock file in the repository's locks/ (restic's look for other locks, before and after its own write), so a repository holding hundreds of leftover locks can run past `setup`'s two-minute clock, and a right key ends on "restic had not finished opening the repository with its lock within 2m0s". | `openRepository` (`backups/engine/setup.go`) | 2 | 2 | 3 | 12 | size 2, risk 2 (count the locks before the opening, and say so where they are many: a unit and a branch) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #352's redesign). Occurrence 2: hundreds of leftover locks need a key that could not delete them, which the guide's policies never make, run against the repository for a long while; nothing is installed. The remedy: `restic unlock --remove-all` with a key that may delete, then `setup` again. |
| After an exit 11, a later opening that exits 12 (a wrong password typed at the next asking) clears `writing`, so a Ctrl-C at the asking after it says "nothing has been written", though the race at exit 11 may have left a lock of the opening's. | `openRepository`'s exit-12 arm, after `reopen` (`backups/engine/setup.go`) | 2 | 1 | 3 | 6 | size 1, risk 2 (`writing` made sticky once an exit 11 has been seen) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #352's redesign). Occurrence 1: the race at exit 11, a key that may not delete, and a wrong password after it. `restic list locks` from your own machine shows the lock if there is one. |
| A restic that rewords its `error while unlocking:` line would let a key that may write and not delete through `setup`: the opening exits 0, says nothing setup reads, `repository ready` is said, and every run after leaves one more lock, which restic never passes by itself [M75], until the first prune, or a check from elsewhere, exits 11 for good, naming the locks. | `unlockFailed` (`backups/engine/setup.go`) | 3 | 2 | 3 | 18 | size 3, risk 2 (lock listings before and after the opening, read and matched: the machinery the redesign removed, each part of it with hazards of its own; `restic unlock` after the opening instead removes a live lock of a same-hostname machine [M4 case E]) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #352's redesign). Occurrence 2: a rewording in the restic a box runs, and motel shipping for it without its e2e — the support matrix is Debian 13 alone, which ships restic 0.18.0 and writes the line [M4 case A, three times out of three; case B], and the e2e's DELETE-refused case asserts the refusal, so a rewording goes red there before it ships. The key is refused in the end by restic itself, at the first prune, named. |
| The race at exit 11: an exclusive lock that appears between restic's look for other locks and its write of its own — restic writes its lock, finds the other on its second look and removes its own, which a key that may not delete cannot do — leaves that lock, and setup goes on with the key's deleting unchecked. | `reopen` (`backups/engine/setup.go`) | 3 | 2 | 1 | 6 | size 2, risk 2 (read restic's own line on the exit-11 stderr too, and refuse on it; no test can make the race) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #352's redesign). Occurrence 2: an exclusive prune or check from elsewhere starting within that moment of setup, and a key that may not delete. A plain exit 11 leaves nothing of the opening's own: restic looked before it wrote [M4 case G]. Named: the exit-11 line says "whether this key can write to it, or remove a lock file, was not checked". |
| On the tryShown path after an init that exited 1 past the opening (`initRan`, not `made`), an interrupt during the opening says "nothing of setup's has been written; a lock file … may stay" and, on the same line, that init "may have made the repository". | `err` and `lockLeftLine` (`backups/engine/setup.go`); `TestAnInterruptDuringTheOpeningNamesTheLockItMayLeave` pins the line verbatim, as a pin of this accepted wording | 2 | 3 | 1 | 6 | size 2, risk 2 (`lockLeftLine` keyed on `initRan` too) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-24, the write-check PR). Both halves are true of their own things — nothing of this setup's opening is in the repository, and the earlier init may have written — and the fate names what to do. |
| `setup`'s look (`restic cat config`) exiting 1 within its clock for a reason other than the storage refusing — a config file restic cannot read, a TLS trust failure — is said as every exit 1 is, "restic could not open the repository (exit 1): <restic's words>" then "the storage refused the key, or could not be reached: the key id and secret again", and setup ends after three askings with restic's words; before #175 the look was given up on and init said the same words after a password had been shown. | `look` (`backups/engine/setup.go`), `lookRefused` | 2 | 3 | 1 | 6 | size 1, risk 2 (reading the look's exit 1 by restic's wording, as init's is, would send a storage that refuses at once in another shape — S3's `AccessDenied` comes through the config's Stat, not the opening — back to the password-first path) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-23, #175). Rule 2 holds: red, with restic's own words on the line, and nothing written. |
| A storage outage that begins during a backup leaves the command's lock file in the repository: the backup ends exit 1 after 20–35 minutes, its lock still there [M87], which restic never passes by itself [M75]. The box's own commands go on (a backup, a fetch and setup's write check take shared locks, the drill's check none; the next backup measured ok); an off-box `prune` waits out its `--retry-lock 1h` and exits 11, since restic refuses an exclusive lock over any lock and never asks whether it is stale (restic 0.18.0, `internal/restic/lock.go:188-189`; M75, measured for a backup). | restic, not this code; docs/backups.md, "Retention, off the box" | 2 | 3 | 1 | 6 | size 2, risk 2 (a `restic unlock` unit after a run the storage failed would delete in the repository on every such run, and would remove a lock another machine's restic still held) | Rule 2 holds: red and named where it bites — `prune` says the repository is locked (exit 11) — and the guide gives `restic list locks --no-lock` and the unlock. Under rule 3 the product is under 20 (2026-10-23, #354). |
| An off-box `prune` that is stopped (killed, its machine asleep or cut off) or whose deletes all fail leaves an exclusive lock, which restic does not take for stale with its process gone [M90]: every run and drill of the box's then waits its `--retry-lock 2h` for it, holding the run lock, nothing said meanwhile, and ends "the repository stayed locked by something else for 2h (exit 11)"; backups stop until `restic unlock` from your own machine. | restic, not this code; `resticFailure` (`backups/engine/engine.go`); docs/backups.md, "Retention, off the box" | 4 | 4 | 1 | 16 | size 3, risk 3 (the box removing another machine's lock would end a prune that is still running, whose lock is all that keeps a backup from writing beside its deletions) | Rule 2 holds: red and named — every run fails naming the lock, `status` is stale from the third hour — and the guide gives the remedy and says to read `prune`'s exit status, not its words (one whose every delete failed printed "2 snapshots have been removed", M90). Under rule 3 the product is under 20 (2026-10-23, #354). Whether `restic unlock` without `--remove-all` removes such a lock is not measured; the guide gives `--remove-all` with the box's timers stopped. |
| A storage that answers a byte at a time is never given up on by restic — each byte read resets its stuck-request watchdog, at any timeout [M92] — so an upload, a fetch or the check runs on, holding the run lock, and no later run starts. One that takes connections and never answers, or is paused, is no longer this row: those three units make one try at a request (`oneTry`) and end after 15 minutes at the opening, 21–36 mid-upload and 35–51 mid-fetch, exit 1, or 130 where restic's lock monitor gives up first [M93] (the check mid-read, the next row), where ten tries took 100–106 minutes with nothing on stderr until the end [M92]. | the upload, fetch and check units (`backups/engine/engine.go`, `restore.go`, `check.go`); restic's stuck-request watchdog, kicked by every byte | 4 | 3 | 1 | 12 | size 3, risk 3 (a no-progress watchdog that ends a unit, the option #177 weighed: a new state per unit, and a cut that must never end a slow upload that is moving — restic's `bytes_done` stands still while the last packs are sent, and the bytes a unit writes grow in a stall, as restic sends a pack again every five minutes [M92]) | Rule 2 holds: `status` exits 1 from the third hour — "no run has finished since …", the app stale, and `running: upload of <app>, since …` naming the stuck unit. Under rule 3 the product is under 20 (2026-10-23, #354; narrowed 2026-10-29, #177). The switch to the S3 endpoint removed the hang #177 found (M87: every fault that answers ends), and one try a request the one that never answers [M93]; this one is accepted, and #177 closed with these scores (2026-10-29): the watchdog and a dead man's switch it lists are features, not fixes owed. |
| A storage that stops answering while the drill's check reads its data group holds the check a quarter of an hour for each five packs of the group not yet read — 45 minutes for 14 packs [M94] — since the check takes no lock, and so has no lock monitor to end it as a fetch's does [M93]: a group of 1,200 packs, a 1 TB repository's fifty-second in restic's 16 MiB packs, some 60 hours, the run lock held and every hourly run refused through it. It ends exit 1, and the probe after it says `unreachable` (`checkRepository`). | `checkArgv` (`backups/engine/check.go`: `--no-lock`, no clock, D4); restic 0.18's five connections and its quarter of an hour of retries a pack | 4 | 3 | 1 | 12 | size 3, risk 3 (a clock on the check from its group's size, which the drill does not know before the check, and which must never cut a slow link that is reading; or the lock, which the owner refused for the check, since one killed hard leaves an exclusive lock restic never passes [M75]; or #177's no-progress watchdog) | Rule 2 holds: `status` exits 1 from the third hour, "no run has finished since …", and its `running:` line names the check and since when; the check ends by itself, `unreachable` and named, and the next drill reads the group again. One try a request made it ten times shorter: ten took 100 minutes for each five packs [M92]. Under rule 3 the product is under 20 (2026-10-29; #177 closed with these scores). |
| A drill whose fetch the storage stops answering gives that fetch up, `restore not proven` and that app's own — 15 minutes at the fetch's opening, exit 1; 35 and 51 minutes from mid-download, exit 1 and exit 130 — then asks the next app's snapshots, which waits out the listing's 30-minute clock before nothing more is asked: 46 minutes in all from the opening, 67 and 82 from mid-download, the run lock held and the hourly runs refused through it [M93: the fetch 904, 2108 and 3040 s after the fault; the drill 2736, 3999 and 4901 s]. A storage that stops answering before the drill ends it at the first listing's clock instead. | `Drill` (`backups/engine/restore.go`: a fetch's exit 1 is app-own, and `refused` is set by a wide error or `x.unanswered` alone); `listClock` (`check.go`) | 2 | 3 | 1 | 6 | size 2, risk 2 (take a fetch's exit 1 after a request timeout as the repository's: restic's words are in the journal, not the unit's stdout, as in the read-only key's row above; or give the listings one try too, which puts a single 500's retry lines beside a listing that is then not believed [M92]) | Rule 2 holds: the drill exits 1, each app named, `restore not proven` or that the repository did not answer, and the bound does not grow with the apps or the data: one fetch, ended by restic's lock monitor where its packs are not all asked first, one listing's half hour and the check's 30-second probe, however many there are. Under rule 3 the product is under 20 (2026-10-29, #177). |
| Where no region is set (a hand-written file for an S3 host other than AWS's regional ones and B2's), a storage that answers a request with a region error (`AuthorizationHeaderMalformed`, `InvalidRegion`, `AccessDenied`) naming another region than its own bucket-location answer gave is no longer retried by minio-go inside the try, since the upload, fetch and check make one (`oneTry`): restic's retry layer tries again, a retry line in the journal, and the region minio-go cached by then answers; an `AccessDenied` restic takes for permanent, and that unit ends exit 1. | `oneTry` (`backups/engine/engine.go`); minio-go 7.0.89 `executeMethod` in `api.go` (the region retry counts against `MaxRetry`, after `bucketLocCache.Set`); restic 0.18 `IsPermanentError` in `internal/backend/s3/s3.go` | 2 | 3 | 1 | 6 | size 2, risk 2 (leave out `oneTry` where no region is set, a branch on the credential file that brings back the 100-minute hang there) | Read from the sources, not measured: rclone, the e2e storage, answers no region error. It takes a storage whose two answers disagree; the run warns of a B2 endpoint with no region ("What it refuses"), and on AWS minio-go reads the region from a regional host. Rule 2 holds: the unit's exit 1 is said for its app, restic's line in its journal. Under rule 3 the product is under 20 (2026-10-29, #177). |
| On an S3 host other than AWS's and B2's, for which setup writes no region, every restic command's first request is minio-go's bucket-location lookup, which minio-go does not retry on a 5xx that carries no S3 error code: one such 500 puts restic's two retry lines on stderr beside a listing, exit 0, and the listing is not believed [M89: B2's documented JSON 5xx body, "Codes in the 5xx range … include JSON diagnostics" (Backblaze, Introduction to the S3-Compatible API), injected through a proxy]. An S3 XML 500 there is retried by minio-go in silence. | `besides` (`backups/engine/engine.go`); minio-go 7.0.89 `bucket-cache.go`, `executeMethod` in `api.go` | 2 | 3 | 1 | 6 | size 2, risk 3 (a region for every host would have to be asked for, and a wrong one is refused; reading restic's retry lines as harmless would believe a listing beside a line that means a snapshot was left out) | Rule 2 holds: the listing is a warning of the run, said as restic having something to say beside it, and the next run lists again. On AWS minio-go knows the region from a regional host alone (minio-go `pkg/s3utils/utils.go`, `GetRegionFromURL`, gives none for `s3.amazonaws.com`); there the guide's policy grants `s3:GetBucketLocation`, and AWS's errors are XML, which minio-go retries. On B2 setup writes it ("Setup", 4), and with it the same fault is silent (M89). Under rule 3 the product is under 20 (2026-10-23, #354). |
| `status` as root says nothing of a credential file that sets `RESTIC_REPOSITORY_FILE`, which every run, restore and drill refuses; with no `RESTIC_REPOSITORY` beside it, its lint says that one is not set. | `showStatus` (`backups/cmd/motel-backup/main.go`); `repositoryInUse` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (one more lint line in status) | Rule 2 holds: the run refuses before any unit in words that name the key and the remedy, and `status` shows that as the last run's error. Under rule 3 the product is under 20 (2026-10-24, #357 review round 2). |
| A run, a restore and a drill refuse a `b2:` repository and a `RESTIC_REPOSITORY_FILE`, not every repository setup refuses: a hand-written `sftp:`, `rclone:`, local path or URL with credentials in it starts its units, which fail. | `repositoryInUse` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 2 (calling `RepositoryUsable` here too: a by-hand `rest:`, `gs:` or `swift:` file must still run) | Rule 2 holds: each fails at its first unit in restic's own words, the run red, and `status` as root warns of the file in setup's words. A `b2:` one is refused because it does not fail: it can hang (#177). Under rule 3 the product is under 20 (2026-10-24, #357 review round 2; the plan's "refuse everything setup refuses", declined as wider than #354). |
| The run's `b2:` refusal has 126 runes before its URL's bucket and 116 after its path, and the record keeps 300 (`record.Text`): a bucket and path (escaped, with the slash between them) over 58 characters lose the end of the reason, and over 173 the end of the command to paste, its closing quote first, cut with a "…" (59 and 185 before #323's `as root`; 184 before #389's `--existing`, eleven runes moved from after the path to before the bucket). | `b2RunRefused` (`backups/engine/setup.go`); `record.Text` | 3 | 5 | 1 | 15 | size 2, risk 2 (a wording for the record alone, or no URL in it past the cut; the wording was shortened in round 4 of #357's review, a fix to text a test checks) | Rule 2 holds: the cut is marked, and the journal keeps the whole line (`journalctl -u motel-backup.service`), since the error goes to stderr whole. A cut URL whose quote is closed and run points `setup --existing` at a shorter path, where it finds no repository and refuses it, nothing written (#389; before it, setup there showed a new one's password (`Repository password (new)`) and asked that it be stored, where a move asks for the repository's own, put the record aside, and ended `repository ready: … (new, …)`); `status` as root names the whole URL in its warning (severity 3 from round 13 of #357's review). The remedy is first by design, so the reason goes before it does. Under rule 3 the product is under 20 (2026-10-24, #357 review round 2; the bounds re-measured for #323's `as root`, four runes longer, scores unchanged, 2026-10-28). |
| The run's remedy, pasted without its placeholder filled in (`<region>`, or `<bucket>` for a bare `b2:`), is taken by setup, which asks for the key and secret before restic refuses the host, and asks again up to its limit. | `backendOf` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (a refusal of a URL holding `<` or `>` before any prompt) | Rule 2 holds: each look is refused at once in restic's words, which name the host with its `<region>` in it, and nothing is written. Under rule 3 the product is under 20 (2026-10-24, #357 review round 4). |
| `status` as root reads the credential file with a plain read: a FIFO in its place, which the run refuses, holds `status` until interrupted. | `showStatus` (`backups/cmd/motel-backup/main.go`) | 2 | 1 | 5 | 10 | size 1, risk 2 (the run's `readRegular`, exported; no test of status's own read) | Only root makes the file anything but setup's regular one, and `status` is run at a terminal, where the wait is seen and interrupted; the run, which no one watches, refuses it. Under rule 3 the product is under 20 (2026-10-24, #357 review round 5). |
| The run opens what the credential file's path leads to, a link followed as the manager follows it, before it asks whether it is a regular file: a device node there is opened by root (`O_RDONLY\|O_NONBLOCK`) before it is refused. | `readRegular` (`backups/engine/setup.go`) | 2 | 1 | 5 | 10 | size 2, risk 1 (a stat before the open, as `box/envfiles.go` reads its files) | Only root puts anything but setup's regular file there, and the open reads nothing; the run then refuses it, named. Under rule 3 the product is under 20 (2026-10-24, #357 review round 8). |
| The run reads the credential file whole, with no cap on its size: a link at its path to a large regular file is read into root's memory and parsed on every run, restore and drill before it is refused or used. | `readRegular` (`backups/engine/setup.go`) | 2 | 1 | 3 | 6 | size 2, risk 2 (a cap on `fi.Size()`, a new limit, which is discussed before it is set) | Only root puts anything but setup's few-line file there, and the run still ends, reading it. Under rule 3 the product is under 20 (2026-10-24, #357 review round 9). |
| `status` as root warns of a hand-written `s3:` repository whose object path holds an `@` — "the repository URL holds an @, which is refused: … a path with one is not supported" — though restic, and the run, use it. | `backendOf` through `RepositoryUsable` (`backups/engine/setup.go`) | 2 | 5 | 1 | 10 | size 2, risk 2 (a reading of the authority again, the shape three rounds of #357's review found ways past) | The warning says what it is about, its exit status unchanged, and setup's refusal of any `@` is what keeps a key and secret off a command line in every spelling. Under rule 3 the product is under 20 (2026-10-24, #357 review round 10). |
| A run, restore or drill reads the credential file once, in `begin`; the manager reads it again for every unit after. A hand edit back to `b2:` while one is under way reaches that command's later units, restic's own B2 backend with them. | `repositoryInUse` through `begin` (`backups/engine/engine.go`) | 4 | 3 | 1 | 12 | size 3, risk 2 (the units given the values read at `begin`, not the file's path) | The edit is the operator's input, its timing with it; what hangs is restic's own B2 backend meeting a dead upload pod, one rare fault. Rule 2 holds: `status` exits 1 from the third hour naming the stuck unit, and as root warns of the `b2:` file; the next command refuses the file. Under rule 3 the product is under 20 (2026-10-24, #357 review round 12; occurrence corrected from 2 in the re-triage after round 13). |
| A box on a `b2:` repository whose bucket or key is from before 4 May 2020, which B2's S3-compatible endpoint does not take [Backblaze's archived help, cited in the guide], has no motel-backup path to restore: the run, restore and drill refuse the `b2:` file, and the S3 URL they name cannot reach that bucket with that key. | `repositoryInUse` (`backups/engine/setup.go`), through `begin` | 4 | 5 | 1 | 20 | size 2, risk 2 (let `restore` alone run on a `b2:` file, said, since an operator watches a restore and can stop it where a hang is the run's) | **Owed to the owner's call, not fixed here:** the product clears rule 3, but the fix reverses the owner's choice that a restore refuses `b2:` too (2026-10-23, #354). Until then the remedy is restic by hand ("By hand", above), with the file's own `b2:` URL and keys, or copying the bucket to one made since. Only a box set up on `b2:` before #354 can meet it. |
| At the opening's exit 1 after restic wrote its lock and could not remove it, restic's `Remove(<lock/…>) failed:` and `error while unlocking:` lines stay in the words quoted (`withoutRetries` drops retry lines alone) and are appended again after them, so where a read was retried until restic gave up — no `Load(…) failed:` line before them — the 300-character cut can fall inside the `Fatal` line, before its cause; a B2 endpoint's URL, longer than most, makes that likelier. | `openRepository`'s exit-1 arm, `withoutRetries`, `unlockFailed` (`backups/engine/setup.go`) | 2 | 3 | 3 | 18 | size 2, risk 2 (the unlock lines left out of the exit-1 arm's quoting, since they are appended: one more case in the filter, and the invariant rows that quote it change) | Rule 2 holds: setup ends refused, and the `Remove(<lock/…>) failed:` line it quotes first carries the storage's answer to the same outage, with the lock left and where `restic unlock` clears it. Under rule 3 the product is under 20 (2026-10-25, #357 review round 14, on #352's opening). |
| RC check 5's snapshot for `prune` to remove (`head -c 8388608 /dev/urandom \| restic backup --stdin --tag rc-retention`) runs after check 3 has unset `RESTIC_PASSWORD`, so taken in order it ends `Fatal: cannot read both password and data from stdin` and the prune has nothing to remove. | docs/release-candidate.md, check 5 | 2 | 5 | 1 | 10 | size 1, risk 2 (text no test checks: the password read and exported before the pipeline, unset after the forget) | Rule 2 holds: restic refuses at once and names the cause, and check 3 shows the `read -rs RESTIC_PASSWORD` line that answers it; nothing is written. Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, #357, Copilot on 59aa393). |
| The guide's B2 timeline sort, `sort -t_ -k9,9` ("After an attack"), assumes the ninth `_` field of every B2 file id is the millisecond upload stamp: generalised from the lines measured in `rc-runs/rc-results/L10-timeline-sort.out`, while Backblaze calls file ids opaque. An id of another shape mis-sorts the listing and gives a wrong copy moment. And the timeline gives the moment to the second, while the guide has no step for a sub-second `version_at` when the last run's write and the attack's first fall in one second. | docs/backups.md, "After an attack", the timeline line | 3 | 2 | 3 | 18 | size 1, risk 2 (text no test checks) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-10-25, PR #356; D re-scored from 1 the same day). A moment too late is loud when the copy's `restic check --no-lock --read-data` finds errors, but not when the attack began by hiding the newest snapshot: the copy after that hide checks clean and is short of it. The guide's step for that, beside the check: for a moment from the bucket's history, each app's newest snapshot in the copy must be the one `motel-backup status` named before the attack (`snapshot <id>` on its `last complete backup` line, `backups/status/status.go`); one missing means an earlier moment. D is 3, not 1: the step is loud only where the reader holds that id from before the attack, and without it the copy is a plausible, clean, older backup. |
| The tutorial's backups bullet (`docs/after-first-deploy.md`) names no command as root's: it links to "Who runs these". An administrator in `adm` alone who goes straight to "Set it up" runs `setup` as themselves. | `docs/after-first-deploy.md`, "Backups" | 2 | 5 | 1 | 10 | size 1, risk 2 (a list of root's commands in the bullet, which round 2 of #323's self-review found narrower than the guide's) | Rule 2 holds: `setup` refuses before anything else, "a setup starts system units, which needs root: run the same command again as root", and "Set it up" opens "As root on the box". Under rule 3 the product is under 20 (2026-10-28, #323 self-review round 3). |
| A drill refused before any unit — a `b2:` repository, say — still prints each app's last proof from the record in a drill's own words, `example: restore proven: snapshot …, on <its date>`, before the refusal and exit 1 [RC check 9, `v0.5.0-rc1`]. | `drill`, `reportDrill` (cmd/motel-backup) | 2 | 5 | 1 | 10 | size 2, risk 2 | The refusal follows at once, the exit is 1, and the line carries the proof's own date, not the drill's. The report is wanted for a drill that ran and failed part way; telling that from a refusal before any unit needs the engine to say which, one more state for a message. |
| A trial adapt that fails with no words from the adapter — the plan step's deadline reached mid-way, `/usr/bin/motel` replaced under a package upgrade, its output not JSON — is read as a variable no value adapts with, and every variable still to try is refused as opaque, the real error gone with it. | `Inspect` (`backups/plan/make.go`), the trial loop's `continue` | 2 | 3 | 3 | 18 | size 2, risk 1 (return an error with no adapter words at once) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-06, #394 review). The trial's error was discarded the same way before #394; the refusal is red and names the variables, and the next hourly run meets no deadline. |
| A Caddyfile validated as a copy away from `/etc/motel` — a checkout, the box channel's staged file — is read alone, so a variable every made-up value fails at a directive's check of, written as well in a file beside the copy that the copy imports, is refused at `validate` as not knowable (the probe sees the file read past its copy), where the run on the box, which reads the files beside the live Caddyfile, tries that occurrence and may let it through. | `Inspect`, `envNames` with `beside` false, `probe` (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (read the copy's directory too, as the run's) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-06, #394 review). Red where green was due, naming the variable; a box push carries no import (the first row of this table), so step 6's verdict is the run's. |
| A variable with no default written both where a directive checks its form and where a plan reads — `protocols {$X}` and `root /srv/{$X}` — is refused twice over: first with the hint that a default settles it, then, with the default written, as a root from the environment. | `Inspect`, the base loop's hint (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (probe before the hint, with no base plan to hold the copies against) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-06, #394 review). Each refusal is red and names the variable and the remedy, and the second is the one the first would have given with the default written. |
| A value a directive checks by form, written in a snippet or an imported file, or carried to one by its argument — `protocols {$X:tls1.2}` in `sites/tls.caddy`; `import tlsmin {$X:tls1.2}` with `protocols {args[0]}` in the snippet — is refused as not knowable: the adapter refuses the first expansion it meets, with the import chain after the line, and an import with arguments may expand the same occurrence again where a plan reads (`{args[0]} {$X:first}` imported once as `sqlite` and once as `lb_policy`), which its words cannot tell from an import expanded once. | `probe`, `throughAnImport` (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 3, risk 2 (counting an import's expansions is reading syntax; a second value the check accepts is not to be had in general) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-06, #394 review; widened 2026-11-07, #412 review). Red where green was due, naming the variable and the cause; the remedy is the value written where the directive is, outside the import, or the argument named for what it is. |
| A value a directive checks by form written first on a line of a quoted string or a heredoc that spans lines is read as a directive's name, and refused as not knowable where it is inert. | `probe`, `leadsItsLine` (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (follow the lexer's quoted and heredoc state) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-06, #394 review; narrowed 2026-11-07, #412 review, when the line a refusal is placed at stopped being read). A protocol name, a policy or a redirect code is one token on one line; one spanning lines is refused, red and named. |
| A variable written only in a file that a symlink beside the Caddyfile leads to, outside its directory, is not found at `validate` on a checkout: the walk passes links over (a link to a file inside is walked at the file's own path), so the variable is neither tried nor probed and `validate` is green; the run's view holds no link out, so the run is refused there by the adapter, red, naming the import. | `envNames` (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (stat a link, read a regular target) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-07, #412 review). Green at the checkout, red at the run, which names the file. |
| The files the probe copies are read again after `envNames` read them, so a Caddyfile rewritten in between — a push landing during the hourly run — is probed as it now is against a base adapted as it was: the control differs from the base, and the variable is refused as not knowable for that run; the next reads one file. | `probe` (`backups/plan/make.go`) | 2 | 2 | 1 | 4 | size 2, risk 1 (have `envNames` hand on the bytes it read) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-07, #412 review). Red and named for one run, on the safe side of the race. |
| A `validate` whose Caddyfile's directory, or `TMPDIR`, is on a filesystem that refuses symlinks (some SMB, exFAT and synced folders) ends in the copy's error, named ("copying the Caddyfile's directory to try {$X} in: …"), where the variable is inert and the run on the box, in `PrivateTmp`, lets it through. | `probe`, `mirror` (`backups/plan/make.go`) | 2 | 3 | 1 | 6 | size 1, risk 2 (return `unknown` with no error, and lose the cause) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-07, #412 review). The error names the filesystem's refusal, which the opaque refusal would not. |
| A variable with no default written both in an import glob and where a directive checks its form — `import sites/{$ENV}/*.caddy` and `protocols {$ENV}` — is refused as a variable no made-up value adapts with, as it has to be, but the hint says a default settles it: the glob's empty-pattern warning sits above the directive's error, and the base loop reads the last line alone. With a default it is refused as an import's, named. | `Inspect`, the base loop (`backups/plan/make.go`) | 2 | 5 | 1 | 10 | size 2, risk 1 (in the `!moved` branch, an empty glob holding the variable's value picks the import's hint) | Under rule 3 the product is under 20 and the fix is not size 1, risk 1 (2026-11-07, #412 Copilot's third review). Red and named either way: the operator who adds the default meets the import's refusal, which names it. |

## Development

- `make test` — everything that needs neither systemd nor sqlite3: the
  plan, the record, the run's flow and failure classes against a
  scripted runner, the file inspection.
- `make test-integration` — as root against a real system manager and
  Debian's `sqlite3`: what a unit's view holds for every way of saying
  who runs it, that arguments are never expanded, a cancelled run's
  unit confirmed gone, a unit ended with its SIGKILLed orchestrator;
  and sqlite3 itself — a live WAL database, what is swapped in after
  the checks, a read-only FIFO killed at the open bound, a planted
  `.sqliterc`.
- `make test-integration` also races an app flipping a declared path
  between its directory and a link to a sibling's, as fast as it can,
  against real units starting with the read capability.
- and an app flipping the directory restored into, and the database
  restored over, between the real thing and a link to a sibling's while
  real install units run; `restic restore`'s summary and exit statuses;
  a restore over a live database under a writer, and over one with
  damaged pages; a runner made with the context that is then cancelled;
  what the manager lists as running, and since when.
- and what the manager makes of each shape of line in the credential
  file, held against what `envfile` reads; what `restic init` and
  `cat config` say of a repository that exists, and with a wrong
  password; that `cat config` writes a lock file unless told not to,
  and exits 11 at once where another restic holds the repository.
- and every other behaviour of restic, sqlite3, systemd and Caddy that
  the code leans on, each a test of its own whose comment names what
  rests on it: restic's exit statuses (3 with the snapshot still named,
  11 for a lock held through `--retry-lock`), its JSON, its default for
  a stuck request, a restore as a user who cannot give files their
  owners, the exclude file's escaping; sqlite3 at the path limit; the
  filter on `open_by_handle_at`; Caddy's `{$NAME}`.
- `make e2e-backup` — a box with systemd, restic and sqlite3, and an S3
  server (`rclone serve s3`): the setup suite (at a real terminal,
  `script(1)`'s), the backup suite, the units suite (the shipped unit
  files: a run and a drill under their hardening, their units bound to
  them, nothing before setup, a failed run a failed unit, the drill
  ordered behind a run, the timers as written and one firing, an
  upload shown another directory than the one given), the
  status suite (`status` and `validate`) and the restore suite, mostly
  failure paths. The box image runs the package's postinstall and then
  undoes two things of it: the `motel-backup` account, which the
  setup suite shows `setup` making, and the timers' enable state, so
  that no timer fires into a scenario.
- `make package` and `make install-test` — the `.deb` built and
  installed under real systemd on a fresh Debian 13: the "Set it up"
  lines of `docs/backups.md` as root (`validate` and `status` as an
  account with no privilege), and the package's transitions table with
  its edges.

### On a new Debian release

restic, sqlite3 and systemd are Debian's, so a new release is new
programs under the same code.

1. Name the release where the backup lanes' images are:
   `test/systemd/Dockerfile` (`golang:<version>-<codename>`),
   `e2e/backup/Dockerfile` (`debian:<codename>-slim`), and the package
   lanes' `DISTRO` — the `Makefile`, `ci.yml`'s install-test matrix,
   `packaging/test/Dockerfile`.
2. `make test-integration`, `make e2e-backup`, `make package
   install-test`. A behaviour that changed fails the test that holds
   it, by name: the integration lane runs every test afresh
   (`-count=1`), since Go's test cache does not know that a program
   under a test has changed.
3. What no lane can hold — it takes a large repository, or a quarter
   of an hour — is measured by hand, against the suites' S3 server:
   - the reads at scale, from a cold cache over a slow link. The
     30-minute backstop on a listing (`listClock`) is seventy times the
     slowest measured with restic 0.18 on 28.5 GB and 1,506 snapshots,
     25 ms each way and 50 Mbit/s: `snapshots` 19.6 s, `ls` of a
     200-entry directory 24.6 s, `stats` of a 200,000-file snapshot
     6.4 s. The check of one fifty-second has no backstop: 155–217 s.
   - that a wrong storage key ends by itself: restic 0.18 gives up after
     a quarter of an hour (864 s and 900 s measured), exit 1.
     `listClock` is twice that, and an upload, a fetch and the check,
     which have no backstop, end by it where the storage answers (next).
   - how a storage that fails ends, through restic's S3 backend, with
     the suites' S3 server (rclone, `e2e/backup/s3.Dockerfile`) behind
     a proxy that injects the fault, plain HTTP, no region, and the
     upload's own command [M87–M90]: minio-go 7.0.89, which Debian's
     restic is built with, tries a request ten times, 0–1 s apart, and
     restic's own layer retries the call, then exit 1 after 14.3–14.9
     minutes an operation — a 500 or 503 every time, a connection
     refused, a host that does not resolve; 20.5 and 34.7 minutes for
     an outage from mid-backup. Up to three 500s in a row say nothing;
     ten, two lines on stderr; random faults, resets and 503s that
     stop, exit 0. A storage that takes connections and never answers
     costs each try five minutes, restic's stuck-request watchdog: with
     ten tries a backup ended after 100 minutes at the opening and 106
     mid-upload (a paused one had not ended at 25, #346), and a SIGTERM
     did not end it sooner [M92]. With one — the upload, the fetch and
     the check — after 15 at the opening, 21–36 mid-upload and 35–51
     mid-fetch, exit 1 (130 once, restic's lock monitor first), named
     in the record; a silence of ten minutes still ends exit 0
     [M92, M93]. The check mid-read, which takes no lock, a quarter of
     an hour for each five packs of its group left [M94]. A storage that answers a byte at a time is never given
     up on, each byte resetting the watchdog: no backstop ends that (D4,
     #177).
     A listing beside one B2-shaped 500 on the location lookup, with
     no region, is not believed (M89). A `prune` whose deletes all
     fail ends after ~35 minutes and leaves exclusive locks that hold
     every run for two hours (M90).

```sh
docker build -t hsb-s3 -f e2e/backup/s3.Dockerfile .
docker compose up --build -d dev-systemd
# On a disk volume: the server's own tmpfs is too small. In the
# client's network, so that one qdisc on lo is the link both ways.
docker run -d --name hsb-scale-s3 --network container:$(docker compose ps -q dev-systemd) \
	-v hsb-scale:/data hsb-s3 --auth-key AKIDE2EFIXTURE,e2e-fixture-key-not-a-secret
docker compose exec dev-systemd bash
```

```sh
export RESTIC_REPOSITORY=s3:http://127.0.0.1:9000/scale RESTIC_PASSWORD=pw \
	AWS_ACCESS_KEY_ID=AKIDE2EFIXTURE AWS_SECRET_ACCESS_KEY=e2e-fixture-key-not-a-secret \
	RESTIC_CACHE_DIR=/var/tmp/scale-cache
restic init
# As measured: five 5 GiB streams, 200 directories of 1,000 files, and
# 500 runs of three small apps.
for i in 1 2 3 4 5; do head -c 5G /dev/urandom | restic backup -q --stdin --stdin-filename stream-$i; done
for d in $(seq 200); do mkdir -p /var/tmp/tree/$d; for f in $(seq 1000); do echo "$d/$f" >/var/tmp/tree/$d/$f; done; done
restic backup -q --host motel --tag app:tree /var/tmp/tree
for n in $(seq 500); do for a in a b c; do head -c 64k /dev/urandom | restic backup -q --host motel --tag app:$a --stdin --stdin-filename $a.db; done; done
id=$(restic snapshots --json --no-lock --tag app:tree | grep -o '"id":"[0-9a-f]*"' | head -1 | cut -d'"' -f4)
apt-get update && apt-get install -y iproute2
tc qdisc add dev lo root netem delay 25ms rate 50mbit
# Each read from an empty cache, as each was measured.
for r in "snapshots --no-lock --json" "ls --no-lock --json $id /var/tmp/tree" \
	"stats --no-lock --quiet --json --mode restore-size $id" "check --no-lock --read-data-subset=1/52"; do
	rm -rf "$RESTIC_CACHE_DIR"
	echo "restic $r"; time restic $r >/dev/null
done
tc qdisc del dev lo root
time AWS_SECRET_ACCESS_KEY=wrong restic cat config --no-lock
exit
```

```sh
docker rm -f hsb-scale-s3 && docker volume rm hsb-scale
docker compose rm -sf dev-systemd
```
