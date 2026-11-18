#!/bin/sh
set -e
# nss <getent or id arguments>: one lookup in the account databases,
# bounded — a user directory that does not answer would otherwise hold
# dpkg's configure for as long as it likes (Copilot on #155). Exit 2 is
# "not found"; 124 is "did not answer", which is never taken for "not
# found": that would make a local account beside the directory's.
nss() {
	if command -v timeout >/dev/null 2>&1; then
		# And killed ten seconds after: a lookup that ignores the
		# SIGTERM must not hold the configure either (Copilot on #155).
		timeout -k 10 30 "$@"
	else
		"$@"
	fi
}
# absent <getent arguments>: the entry is not there, as the databases
# answered; 0 only for that.
absent() {
	st=0
	nss getent "$@" >/dev/null 2>&1 || st=$?
	[ "$st" = 2 ]
}
if command -v useradd >/dev/null 2>&1; then
	if absent group motel; then groupadd --system motel; fi
	if absent passwd motel; then useradd --system \
		--gid motel --home-dir /var/lib/motel \
		--shell /usr/sbin/nologin --comment "motel server" motel; fi
else
	# Alpine (busybox) fallback.
	addgroup -S motel 2>/dev/null || true
	adduser -S -G motel -h /var/lib/motel -s /sbin/nologin motel 2>/dev/null || true
fi
if ! uid=$(nss id -u motel 2>/dev/null); then
	echo "motel: the motel system user does not exist and could not be created" >&2
	exit 1
fi
# Best-effort: put the account IN the motel group. Both branches above
# set the group only when they create the account, so an account that
# already existed — an admin's, or one from another package — keeps
# whatever groups it had, and the state directories chowned below are
# group-motel. Not fatal: nothing the package installs is reachable
# only through the group.
in_group() { nss id -nG motel 2>/dev/null | tr ' ' '\n' | grep -qx motel; }
if ! in_group; then
	if command -v usermod >/dev/null 2>&1; then
		usermod -aG motel motel || true
	else
		addgroup motel motel 2>/dev/null || true # busybox
	fi
fi
if ! in_group; then
	echo "motel: the motel user is not a member of the motel group; add it with \`usermod -aG motel motel\` if its state directories become unreadable" >&2
fi
chown motel:motel /var/lib/motel /var/lib/liveswap
# The account motel-backup's restic units run as: the one line
# `motel-backup setup` uses (backups/engine/setup.go, held to this
# script by a test), which also makes it when it is missing. No home,
# no shell, nothing of its own but the cache the manager makes for it.
# Its comment, made-by-motel, marks it as motel's. An account of
# that name that exists is left as it is — an administrator may have
# meant it — and setup and every run use it only where it is
# motel's: in /etc/passwd, with the mark. Whoever is the account
# reads the repository credential from a running restic's
# environment. Never removed on purge (Debian policy: system accounts
# stay).
#
# Where it cannot be made — a group of its name is there without it,
# and useradd exits 9 [M66] — that is said, and the configure goes on
# (the owner, 2026-09-27): motel is installed on a box that may use
# no backups, and setup, which makes the account when it is missing,
# is where that is refused.
backup_st=0
backup_account=unknown # ok, missing or refused once looked at: the Backups: line below rests on it
nss getent passwd motel-backup >/dev/null 2>&1 || backup_st=$?
if [ "$backup_st" = 2 ]; then
	if command -v useradd >/dev/null 2>&1; then
		why=$(useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-motel motel-backup 2>&1) || true
	else
		why=$(adduser -S -H -h /nonexistent -s /sbin/nologin -g made-by-motel motel-backup 2>&1) || true # busybox
	fi
fi
if [ "$backup_st" != 0 ] && [ "$backup_st" != 2 ]; then
	case "$backup_st" in
	124 | 137) why="the user directory did not answer within 30s" ;; # 137: killed after ignoring the SIGTERM
	*) why="getent exited $backup_st" ;;
	esac
	backup_account=unknown
	echo "motel: whether the motel-backup account exists could not be told ($why); no account is made beside the directory's, and backups wait for it: motel-backup account says when it is right" >&2
elif nss getent passwd motel-backup >/dev/null 2>&1; then
	# The rule is setup's, and in setup's words: the binary this package
	# has just installed says whether the account is one setup and every
	# run accept. A warning, never the install's failure.
	if [ -x /usr/bin/motel-backup ] && ! said=$(/usr/bin/motel-backup account 2>&1); then
		echo "motel: ${said#motel-backup: }" >&2
		echo "motel: motel-backup setup, and every run, restore and drill, refuse that account until it is put right; motel-backup account says when it is" >&2
		backup_account=refused
	elif [ -x /usr/bin/motel-backup ]; then
		backup_account=ok
	fi
else
	backup_account=missing
	echo "motel: the motel-backup account could not be made (${why:-no reason was given}); backups wait for it, and nothing else of motel does: motel-backup setup <repository> makes it, or says why it cannot" >&2
fi
# The box applier's exchange tree, /var/lib/motel-box: made now that
# the motel group exists, and again at every boot, each time with
# the modes and owners box/DESIGN-box.md's Paths table gives, put back
# where they drifted. Named, as debhelper does, so no other package's
# tmpfiles.d entries run from here.
if command -v systemd-tmpfiles >/dev/null 2>&1; then
	systemd-tmpfiles --create motel-box.conf \
		|| echo "motel: systemd-tmpfiles --create motel-box.conf failed; the box applier has no /var/lib/motel-box until it succeeds" >&2
fi
# The starter config (nfpm.yaml says why it is not a conffile). Install
# it where nothing stands at /etc/motel/Caddyfile, and replace it
# only while it is byte-identical to the starter this script last wrote
# there, which the record says. Leave anything else alone, silently: an
# edit, a file `moteld init` or a push wrote, a symlink (never
# followed), anything not a regular file, a file with no record. Each
# write lands whole, by rename, before the try-restart below loads it. A
# failure never fails the install; the file stays as it was.
starter=/usr/share/motel/Caddyfile
config=/etc/motel/Caddyfile
record=/var/lib/motel-package/starter.sha256
sha256() { sha256sum <"$1" | cut -d' ' -f1; }
# Records $want as the starter on disk. /etc/motel is synced first,
# so the record never names a rename a power cut could still undo,
# which would make a stock file look edited for good; then the record's
# own rename, and its directory.
starter_record() {
	record_tmp="$record.$$"
	if sync /etc/motel && printf '%s\n' "$want" >"$record_tmp" \
		&& chmod 0644 "$record_tmp" && sync "$record_tmp" \
		&& mv -fT "$record_tmp" "$record" && sync "${record%/*}"; then
		return 0
	fi
	rm -f "$record_tmp" || true
	echo "motel: could not write $record durably; until it is, upgrades may leave $config as it is" >&2
}
starter_put() {
	config_tmp="/etc/motel/.Caddyfile.package-$$"
	# -T: a directory standing at that name is refused, never written into.
	if cp -T "$starter" "$config_tmp" && chown root:root "$config_tmp" \
		&& chmod 0644 "$config_tmp" && sync "$config_tmp" \
		&& mv -fT "$config_tmp" "$config"; then
		starter_record
		return 0
	fi
	rm -f "$config_tmp" || true
	echo "motel: could not install the starter config at $config; it is at $starter" >&2
}
# A crash's temporaries: only this script makes them. Never fatal: a
# directory by one of these names is not this script's to remove.
rm -f /etc/motel/.Caddyfile.package-* "$record".* 2>/dev/null || true
if want=$(sha256 "$starter") && [ ${#want} -eq 64 ]; then
	if [ ! -e "$config" ] && [ ! -L "$config" ]; then
		starter_put
	elif [ -f "$config" ] && [ ! -L "$config" ]; then
		have=$(sha256 "$config") || have=
		recorded=
		if [ -f "$record" ] && [ ! -L "$record" ]; then
			recorded=$(head -n 1 "$record") || recorded=
		fi
		if [ "$have" = "$want" ]; then
			# Already the starter: a crash between the rename and the
			# record, or a box that has never had a record. Without
			# this it would look edited for good.
			[ "$recorded" = "$want" ] || starter_record
		elif [ -n "$have" ] && [ "$have" = "$recorded" ]; then
			starter_put
		fi
	fi
else
	echo "motel: could not read $starter; $config is left as it is" >&2
fi
# The box's key for secrets (box/DESIGN-box.md, "The box's key"), made
# once and never replaced, so that a box has one with no step. Only on
# a running system: an image built without systemd as PID 1 would
# otherwise bake one key into every box made from it. A failure never
# fails the install; init, or the console, makes it later.
if [ -d /run/systemd/system ]; then
	/usr/bin/moteld box key >/dev/null 2>&1 \
		|| echo "motel: could not make the box's key for secrets; run moteld box key as root" >&2
fi
if command -v systemctl >/dev/null 2>&1; then
	# liveswap runs deployed apps as transient units under the motel
	# user's own systemd manager (user@$uid.service), so they outlive
	# moteld restarts and upgrades. Lingering keeps that manager
	# running with nobody logged in; the drop-in starts it alongside
	# moteld and orders moteld after it. No polkit and no root path:
	# motel talks to its own manager over /run/user/$uid.
	mkdir -p /etc/systemd/system/moteld.service.d
	cat > /etc/systemd/system/moteld.service.d/10-user-manager.conf <<EOF
# Generated by the moteld package (postinstall); do not edit.
# liveswap's app units run under the motel user's manager.
[Unit]
Wants=user@$uid.service
After=user@$uid.service
EOF
	# The user manager's own limits are what its units inherit; match
	# moteld.service so apps keep the file-descriptor headroom they
	# had as its children. (Effective from systemd 256; the package
	# requires 257 — see nfpm.yaml.) And its no-new-privileges flag,
	# which every unit it starts inherits and none can drop: motel
	# talks to this manager, and a unit asked of it without the
	# sandbox's settings would otherwise run a setuid binary on the
	# host to root, the route moteld.service's own NoNewPrivileges
	# closes for moteld's children (packaging/DESIGN-packaging.md).
	mkdir -p "/etc/systemd/system/user@$uid.service.d"
	cat > "/etc/systemd/system/user@$uid.service.d/10-motel.conf" <<EOF
# Generated by the moteld package (postinstall); do not edit.
[Service]
LimitNOFILE=1048576
NoNewPrivileges=yes
EOF
	systemctl daemon-reload || true
	# Lingering starts user@$uid.service at once, so it comes after the
	# drop-ins above: the manager must be born with those settings. (An
	# already-running manager keeps its own until it next starts —
	# restarting it would stop every app, which an upgrade must not do;
	# until then the sandbox probe reports what the running manager can
	# do, and docs/upgrading.md names the restart for the flag.)
	loginctl enable-linger motel 2>/dev/null || {
		mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/motel
	}
	# An upgrade restarts a running moteld onto the new binary;
	# try-restart leaves a stopped one stopped. Only where systemd is
	# PID 1: in a chroot or an image build systemctl fails for reasons
	# that say nothing about motel, and there the start hint below is
	# the whole story.
	restart=skipped
	if [ -d /run/systemd/system ]; then
		if systemctl try-restart moteld 2>/dev/null; then
			restart=ok
		else
			restart=failed
		fi
	fi
	if [ "$restart" = failed ]; then
		# The new binary refused to start — most often a Caddyfile it
		# rejects — and the old one is already stopped, so the site is
		# down. Say so, on stderr. The script still exits 0: a non-zero
		# postinst leaves the package half-configured and fails this
		# apt run, and every later one that retries the configure,
		# until it succeeds. The package is installed correctly; what
		# needs fixing is the config, which dpkg cannot do.
		state=$(systemctl is-active moteld 2>/dev/null) || true
		echo "motel: restarting onto the new version FAILED; moteld.service is now ${state:-in an unknown state}." >&2
		if [ "$state" != active ]; then
			echo "  Nothing is served until it starts (deployed apps keep running, unreachable)." >&2
		fi
		echo "  The likely cause is something in /etc/motel/Caddyfile this version refuses. See why with:" >&2
		echo "    journalctl -u moteld -e" >&2
		echo "    systemctl status moteld" >&2
		echo "  then fix it and run: systemctl restart moteld" >&2
	elif [ "$restart" = ok ] && systemctl is-enabled --quiet moteld 2>/dev/null; then
		# Already set up: no start hint. If is-enabled itself fails,
		# the hint below prints, which is harmless on an enabled unit.
		if systemctl is-active --quiet moteld 2>/dev/null; then
			echo "moteld restarted."
		else
			# try-restart does not start a stopped or failed unit — an
			# upgrade that fixes an earlier failed start included.
			echo "moteld is enabled but not running. Start it with:"
			echo "  systemctl start moteld"
		fi
	else
		echo "moteld installed. Start it with:"
		echo "  systemctl enable --now moteld"
	fi
fi
# The box applier's path unit, the way dh_installsystemd would have it:
# enabled on first installation; an administrator's `disable` kept
# across upgrades (was-enabled is false then, and update-state only
# tidies the symlinks); started, or restarted on an upgrade ($2 is the
# version upgraded from), by deb-systemd-invoke, which starts nothing
# disabled or masked. The service it starts has no [Install]: the path
# unit is its only start (box/DESIGN-box.md, "The applier unit").
if [ -x /usr/bin/deb-systemd-helper ]; then
	deb-systemd-helper unmask motel-box-apply.path >/dev/null || true
	if deb-systemd-helper --quiet was-enabled motel-box-apply.path; then
		deb-systemd-helper enable motel-box-apply.path >/dev/null || true
	else
		deb-systemd-helper update-state motel-box-apply.path >/dev/null || true
	fi
fi
# The daemon-reload above has loaded the unpacked units.
if [ -d /run/systemd/system ]; then
	if [ -n "${2:-}" ]; then action=restart; else action=start; fi
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke "$action" motel-box-apply.path >/dev/null || true
	else
		# No helper (a systemd host that is not Debian): enabled here,
		# and started, with no memory of an administrator's disable.
		systemctl enable motel-box-apply.path 2>/dev/null || true
		systemctl "$action" motel-box-apply.path 2>/dev/null || true
	fi
fi
# The backup timers, the way dh_installsystemd would have it: enabled on
# first installation; an administrator's `disable` kept across upgrades
# (was-enabled is false then, and update-state only tidies the
# symlinks); started, or restarted on an upgrade ($2 is the version
# upgraded from), by deb-systemd-invoke, which starts nothing that is
# disabled or masked. The services they start do nothing until
# `motel-backup setup` has written the credential file
# (ConditionPathExists= in the unit files): an install runs no backup.
if [ -x /usr/bin/deb-systemd-helper ]; then
	for u in motel-backup.timer motel-backup-drill.timer; do
		deb-systemd-helper unmask "$u" >/dev/null || true
		if deb-systemd-helper --quiet was-enabled "$u"; then
			deb-systemd-helper enable "$u" >/dev/null || true
		else
			deb-systemd-helper update-state "$u" >/dev/null || true
		fi
	done
fi
# Started on an install, restarted on an upgrade ($2 is the version
# upgraded from), exactly as dh_installsystemd does it (the owner,
# 2026-09-27): deb-systemd-invoke starts nothing disabled or masked,
# and obeys a policy-rc.d. So an upgrade starts again a timer an
# administrator stopped and left enabled — disable it to keep it off.
# Nothing of the backups was stopped for an upgrade: a run under way
# is left to finish. The daemon-reload above has loaded their units.
if [ -d /run/systemd/system ]; then
	if [ -n "${2:-}" ]; then action=restart; else action=start; fi
	if [ -x /usr/bin/deb-systemd-invoke ]; then
		deb-systemd-invoke "$action" motel-backup.timer motel-backup-drill.timer >/dev/null || true
	else
		# No helper (a systemd host that is not Debian): enabled here,
		# and started, with no memory of an administrator's disable.
		systemctl enable motel-backup.timer motel-backup-drill.timer 2>/dev/null || true
		systemctl "$action" motel-backup.timer motel-backup-drill.timer 2>/dev/null || true
	fi
	# Each timer's own state, so the line says of both only what is so:
	# an administrator's disable or mask of one is kept across an
	# upgrade, and enabled-runtime (their enable --runtime, gone at the
	# next boot) is enabled for what runs today. Then what the backups
	# are: the credential file is what setup writes and the units wait
	# on (ConditionPathExists=, the same test as -e), so without it
	# nothing runs until setup; with it and an account the runs accept
	# they are set up, and telling that box's operator to run setup
	# again would be wrong (#403); with it and an account that is
	# missing, refused or not told (said above), the runs the timers
	# start fail on the account, and setup is not what puts it right.
	hourly=$(systemctl is-enabled motel-backup.timer 2>/dev/null) || true
	drill=$(systemctl is-enabled motel-backup-drill.timer 2>/dev/null) || true
	: "${hourly:=unknown}" "${drill:=unknown}"
	if [ ! -e /etc/motel-backup/repository.env ]; then
		backups=setup
	elif [ "$backup_account" = ok ]; then
		backups=ready
	else
		backups=account
	fi
	case "$hourly/$drill" in
	enabled*/enabled*)
		case "$backups" in
		setup)
			echo "Backups: the hourly timer and the Sunday restore drill are enabled, and run nothing until:"
			echo "  motel-backup setup <repository>"
			;;
		ready) echo "Backups: the hourly timer and the Sunday restore drill are enabled and set up; motel-backup status says where they stand" ;;
		*) echo "Backups: the hourly timer and the Sunday restore drill are enabled, and the credential file is in place; the runs wait on the motel-backup account (above): motel-backup account says when it is right" ;;
		esac
		;;
	enabled*/* | */enabled*)
		echo "Backups: motel-backup.timer is $hourly and motel-backup-drill.timer is $drill, as they were left; systemctl enable --now turns on one that is disabled, after systemctl unmask on one that is masked"
		case "$backups" in
		setup) echo "  and the one that is enabled runs nothing until: motel-backup setup <repository>" ;;
		ready) echo "  and the backups are set up; motel-backup status says where they stand" ;;
		*) echo "  and the credential file is in place; the runs wait on the motel-backup account (above): motel-backup account says when it is right" ;;
		esac
		;;
	*)
		echo "Backups: motel-backup.timer is $hourly and motel-backup-drill.timer is $drill, as they were left; systemctl enable --now motel-backup.timer motel-backup-drill.timer turns them on, after systemctl unmask on one that is masked"
		;;
	esac
fi
# Explicit, so no message above can decide the package's state.
exit 0
