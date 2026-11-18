#!/bin/sh
# moteld package post-remove. dpkg runs it once the package's files
# are gone: `postrm remove`, then `postrm purge` when the removal is a
# purge, and on upgrades (`upgrade`, `failed-upgrade`) and their aborts
# (`abort-install`, `abort-upgrade`, `disappear`), which do nothing
# here: postinstall's `configure` reloads the manager.
#
# remove, and the purge that follows one: the manager drops the removed
# unit files (moteld.service, motel-box-apply.path and .service,
# motel-backup's two services and two timers), and the path unit and
# the backup timers are masked, as dh_installsystemd has it, so that
# nothing of them comes back until an install unmasks and enables them
# (postinstall.sh). The persistent timers' stamps go too: with them,
# and the credential file kept, a reinstall reads a run and a drill as
# missed and catches up inside the install [M68].
#
# purge, on top of that — what the package made and nothing else holds:
#
# - the path unit's and the timers' enable state (deb-systemd-helper
#   purge), then the masks, in debhelper's order;
# - /etc/motel/Caddyfile, when it is a regular file (a symlink is
#   never followed, and is kept, named), and postinstall's temporaries
#   beside it; /etc/motel itself only once it is empty. The box's
#   key for secrets (age/, age.pub) and the env files decrypted with
#   it (secrets/) are KEPT, and said: the key is the one way to read
#   the secrets the box's repository holds, and only the person purging
#   knows whether that is over;
# - /var/lib/motel-package/, the starter's record: root's alone,
#   0700, nothing of an app's in it;
# - /var/lib/motel-box/, the box applier's exchange tree (stage/,
#   in/, work/, out/, the transaction record, applied.json): the
#   package's own, and the Caddyfile it protected is gone by then. Not
#   where anything is mounted at or under it: an rm there would remove
#   through the mount, so the tree is kept whole, and named;
# - /etc/motel-backup/repository.env is KEPT, and said: it holds the
#   repository password, the one way to read the backups already made
#   (PLAN D6). Whoever purges the package decides about the backups;
# - /var/lib/motel-backup: the record, the repository id and the
#   listing's stderr go; staging/ and restore/ are removed with rmdir,
#   never rm -r — a directory that is not empty holds copies of an
#   app's data that a killed run left, which root does not delete
#   (backups/README.md, "Who runs as what": root never reads or removes
#   app data) — and is named and kept;
# - /var/cache/motel-backup, restic's cache, owned by the account and
#   holding nothing of an app's, goes;
# - /run/motel-backup: what a killed command left there — its units,
#   its mounts of an app's data — preremove had the engine sweep at the
#   remove, while the program was there. On purge the run directory's
#   own files go (the lock, the list of units), and its directories by
#   rmdir alone: nothing is removed where anything is mounted at or
#   under it, or where what is mounted cannot be read — an rm there
#   would remove an app's files through the mount [measured in #153].
#
# The Caddyfile and the exchange tree under root's lock,
# /var/lib/motel-box/lock: a run of the applier holds it for its
# whole run and swaps the Caddyfile under it (box/apply.go, lock), and
# preremove stops the path unit, not a run under way (box/DESIGN-box.md,
# "The applier unit"), so a run could otherwise put a Caddyfile back
# after this script removed it. The lock is taken here before either
# removal and held through both, waiting up to 30s for a run — one is
# seconds (validate, swap, reload), so the wait covers many — and where
# it is still held, both are kept, named. Nothing starts a new run by
# then: the path unit is stopped and the program is gone. The lock file
# is opened only where it exists (a tree no run has used has none) and
# never made here.
#
# Never: /var/lib/motel (Caddy's state: TLS certificates) and
# /var/lib/liveswap (the deployed apps' releases and data) — root does
# not delete what an app or an operator put there; dpkg removes either
# only when it is empty, as any directory a package shipped. The
# motel and motel-backup accounts stay: system accounts are never
# removed.
#
# Every step is best-effort: a postrm that exits non-zero leaves dpkg
# with a package it can neither configure nor remove, so each failure
# is said on stderr, naming the path, and the script exits 0.
set -e
unit=motel-box-apply.path
config=/etc/motel/Caddyfile
record_dir=/var/lib/motel-package
box=/var/lib/motel-box
timers="motel-backup.timer motel-backup-drill.timer"
run=/run/motel-backup

# mounted <dir>: something is mounted at or under it, or the answer
# cannot be had (then nothing is removed either). The mount table names
# a mount point by its physical path, so a directory reached through a
# symlinked ancestor (/var -> /srv/var) is looked up under both names.
# Fail closed: a table that cannot be read or resolved, or a scan that
# does not end with a plain yes or no, counts as mounted. So does a
# physical path holding a character the table escapes (space, tab,
# newline, backslash: /var -> "/srv/var data" is written \040 there),
# which the comparison below would miss; the literal path holds none.
mounted() {
	[ -r /proc/self/mountinfo ] || return 0
	real=$(readlink -f -- "$1" 2>/dev/null) || return 0
	[ -n "$real" ] || return 0
	case $real in *[[:space:]\\]*) return 0 ;; esac
	found=$(awk -v d="$1" -v r="$real" \
		'$5 == d || index($5, d "/") == 1 || $5 == r || index($5, r "/") == 1 { f = 1 } END { print (f ? "yes" : "no") }' \
		/proc/self/mountinfo 2>/dev/null) || found=
	[ "$found" = no ] || return 0
	return 1
}

# The removed unit files out of the manager, where systemd is PID 1;
# in a chroot or an image build there is no manager to reload. And
# the backup timers' stamps (header).
case "${1:-}" in
remove | purge)
	if [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null 2>&1 || true
	fi
	for u in $timers; do
		rm -f "/var/lib/systemd/timers/stamp-$u"
	done
	;;
esac

case "${1:-}" in
remove)
	# The masks land only once the unit files are gone, which they are
	# here; postinstall's unmask takes them off again.
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper mask "$unit" >/dev/null || true
		# shellcheck disable=SC2086 # two unit names
		deb-systemd-helper mask $timers >/dev/null || true
	fi
	;;
purge)
	if [ -x /usr/bin/deb-systemd-helper ]; then
		deb-systemd-helper purge "$unit" >/dev/null || true
		deb-systemd-helper unmask "$unit" >/dev/null || true
		# shellcheck disable=SC2086 # two unit names
		deb-systemd-helper purge $timers >/dev/null || true
		# shellcheck disable=SC2086
		deb-systemd-helper unmask $timers >/dev/null || true
	fi
	# Root's lock, where a run could hold it (header). Read-only: flock
	# needs no write, and the mode and owner of what a run made stay.
	held=
	if [ -f "$box/lock" ] && [ ! -L "$box/lock" ] && [ -r "$box/lock" ] && command -v flock >/dev/null 2>&1; then
		exec 9<"$box/lock"
		if flock -w 30 9; then held=yes; else held=no; fi
	fi
	if [ "$held" = no ]; then
		echo "motel: kept $config and $box: a run of the box applier holds $box/lock and did not finish within 30s; once it has (journalctl -u motel-box-apply.service), remove both yourself" >&2
	# The box's config: a regular file only. -f follows a symlink, so
	# the link is ruled out first; whatever else stands there is kept.
	elif [ -L "$config" ]; then
		echo "motel: kept $config: a symlink, which this script never follows; remove it yourself" >&2
	elif [ -f "$config" ]; then
		rm -f "$config" || echo "motel: could not remove $config; remove it yourself" >&2
	elif [ -e "$config" ]; then
		echo "motel: kept $config: not a regular file; remove it yourself" >&2
	fi
	# postinstall's temporaries, from a crash mid-install. Never a
	# directory by one of these names: not this script's to remove.
	rm -f /etc/motel/.Caddyfile.package-* 2>/dev/null || true
	if [ -d /etc/motel ] && ! rmdir /etc/motel 2>/dev/null; then
		echo "motel: kept /etc/motel: it still holds $(ls -A /etc/motel | tr '\n' ' ' | sed 's/ $//')" >&2
		if [ -e /etc/motel/age/identity ]; then
			echo "  age/identity is the box's key for secrets, the one way to read the secrets its repository holds; once nothing needs them: rm -r /etc/motel" >&2
		fi
	fi
	# The starter's record.
	rm -rf "$record_dir" || echo "motel: could not remove $record_dir; remove it yourself" >&2
	# The box applier's exchange tree, whole — unless a run holds the
	# lock (said above) or something is mounted at or under it.
	# --one-file-system is for a mount made between that check and the
	# rm: it is skipped, not removed through.
	if [ "$held" = no ]; then
		:
	elif [ -L "$box" ]; then
		echo "motel: kept $box: a symlink, which this script never follows; remove it yourself" >&2
	elif [ -d "$box" ]; then
		if mounted "$box"; then
			echo "motel: kept $box: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed — unmount it, then remove the directory yourself" >&2
		else
			rm -rf --one-file-system "$box" \
				|| echo "motel: could not remove all of $box; what is left is in it, named above" >&2
		fi
	fi
	rm -rf /var/cache/motel-backup || echo "motel: could not remove /var/cache/motel-backup; remove it yourself" >&2
	state=/var/lib/motel-backup
	if [ -d "$state" ]; then
		# The record, its asides, the id, the listing's stderr, and the
		# temp files their writers leave when killed mid-write: the
		# record's, and setup's of the id.
		rm -f "$state/status.json" "$state"/status.json.aside-* "$state"/.status-* "$state/repository-id" "$state/repository-id.new" "$state/listing.err" \
			|| echo "motel: could not remove all of what motel-backup recorded in $state; remove it yourself" >&2
		for d in "$state/staging" "$state/restore"; do
			[ -d "$d" ] || continue
			# Empty it of empty per-app directories, then itself; whatever
			# holds a file stays.
			for a in "$d"/*; do [ -d "$a" ] && rmdir "$a" 2>/dev/null || true; done
			rmdir "$d" 2>/dev/null \
				|| echo "motel: kept $d: not empty — copies of an app's data a backup run or a restore left there; look, then remove them yourself" >&2
		done
		# What is kept is said for what it is: the state directory stays
		# for staging/ and restore/, said above, or for a file this
		# package did not make — which is no copy of an app's data, and
		# is not called one.
		if ! rmdir "$state" 2>/dev/null; then
			others=$(ls -A "$state" | grep -v -x -e staging -e restore | tr '\n' ' ' | sed 's/ $//')
			[ -z "$others" ] || echo "motel: kept $state: it holds what this package did not make ($others)" >&2
		fi
	fi
	if [ -d "$run" ]; then
		# A command from a shell killed after the remove's sweep — which
		# found it holding the lock, and left it — leaves units no service
		# ends, an upload among them with the credential, and there is no
		# program left to sweep. Stopped here by the names it recorded,
		# and only names the engine writes (engine.go, unitNameRe; a test
		# holds the two together), where nothing holds the lock now
		# (Copilot on #155).
		# One that will not stop is said, by name, and the record of it
		# is kept (Copilot on #155).
		stuck=""
		if [ -f "$run/units" ] && [ -d /run/systemd/system ] && { ! command -v flock >/dev/null 2>&1 || flock -n "$run/lock" true 2>/dev/null; }; then
			while read -r u; do
				echo "$u" | grep -Eq '^motel_backup_[a-z]+[0-9]*(_[a-z0-9-]{1,63})?_[0-9a-f]{12}\.service$' || continue
				systemctl stop "$u" 2>/dev/null || true
				# A unit the manager has none of reads "inactive"
				# [measured]; nothing at all is a manager that did not
				# answer, which is no "gone" (Copilot on #155).
				state=$(systemctl show -p ActiveState --value "$u" 2>/dev/null) || state=""
				case "$state" in
				inactive | failed) systemctl reset-failed "$u" 2>/dev/null || true ;;
				"")
					stuck=yes
					echo "motel: $u, which a backup command left running, could not be asked about: the manager did not answer; if it runs, it holds the repository credential (systemctl stop $u)" >&2
					;;
				*)
					stuck=yes
					echo "motel: $u, which a backup command left running, would not stop (it is $state): it holds the repository credential; stop it yourself (systemctl stop $u)" >&2
					;;
				esac
			done <"$run/units"
		fi
		if [ -n "$stuck" ]; then
			echo "motel: kept $run: a unit named in $run/units would not stop, or could not be asked about, and that list is its only record" >&2
		elif mounted "$run"; then
			echo "motel: kept $run: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed — it is gone at the next boot, and is not to be rm -r'd before" >&2
		elif [ -f "$run/lock" ] && command -v flock >/dev/null 2>&1 && ! flock -n "$run/lock" true 2>/dev/null; then
			# A command that holds the run lock — a restore at its prompt
			# — keeps what is its: the lock, the list of the units it has
			# started, an init marker. Removed from under it, no sweep
			# would ever stop those units.
			echo "motel: kept $run: the run lock is held ($(head -1 "$run/lock" 2>/dev/null)); a backup command is under way, and what it keeps there is its own — it is gone at the next boot" >&2
		else
			rm -f "$run/lock" "$run/units" "$run/init-unit" || echo "motel: could not remove the run lock and unit list in $run" >&2
			for d in "$run"/*/; do [ -d "$d" ] && rmdir "$d" 2>/dev/null || true; done
			rmdir "$run" 2>/dev/null || echo "motel: kept $run: not empty ($(ls -A "$run" | tr '\n' ' ' | sed 's/ $//')); it is gone at the next boot" >&2
		fi
	fi
	if [ -e /etc/motel-backup/repository.env ]; then
		echo "motel: /etc/motel-backup/repository.env is kept: it holds the repository password, the only way to read the backups already made. Remove it yourself once those backups are not needed: rm -r /etc/motel-backup" >&2
	fi
	;;
*)
	# upgrade, failed-upgrade, abort-install, abort-upgrade, disappear.
	;;
esac
# Explicit, so no message above can decide the package's state.
exit 0
