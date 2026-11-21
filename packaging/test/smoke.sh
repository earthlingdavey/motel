#!/bin/bash
# Package install smoke test, run as root inside the systemd container
# started by `make install-test` (dist/ mounted read-only at /dist).
#
# Fail-fast by design: each stage depends on the previous one, so the
# first broken assertion is the diagnosis. The Makefile dumps the
# moteld journal on any failure.
#
# Stage 2 is the one that earns its keep: a real liveswap deploy under
# the shipped systemd unit proves that download + extract work in
# /var/lib/liveswap as the sandboxed motel user and that the app
# comes up as a transient unit under that user's own systemd manager —
# the exact packaging interaction (ProtectSystem=full, User=motel,
# lingering, the user@<uid> drop-in) no other test layer exercises.
# Stage 3 then proves the app survives the upgrade restart, and stage
# 3c that an upgrade restart the new binary refuses is reported, and
# that an upgrade over the failed unit starts nothing and says so. Stage
# 1b runs the box applier's units as shipped, with a stub for the
# applier and then with the real one, and stages 3 to 5 follow its path
# unit through upgrade, removal and an install after removal. Stage 5b
# follows the starter config through upgrades: none asks, a stock file
# follows the starter, and no other file is touched. Stage 6 purges:
# what the package made goes, the apps' data and the key stay.
#
# Stage 2c runs the "Set it up" lines of the backups guide — read out
# of docs/backups.md itself, mounted at /docs-backups.md, so the lines
# run are the lines written — as root, with validate and status as an
# account with no privilege, against an S3 server in this container,
# and then holds the package to the table in backups/README.md: the
# timers enabled once, an administrator's disable kept across an
# upgrade, stopped and masked on remove, enabled again on reinstall,
# and purge keeping the credential file and saying why
# (expect_backup_state, a row per transition). And to the edges of
# each transition (#154): a run under way at an upgrade left alone to
# finish, the timers' stamps and a killed run's mounts gone with the
# package, and — stage 7 — an install where the account cannot be made,
# and what purge keeps said for what it is.
set -eu

# TOKEN is minted in stage 2 with a local deploy key (deploy_trust
# local), once the binary is installed.
TOKEN=""
HOOK="http://127.0.0.1:8081/demo"
PROXY="http://127.0.0.1:8080"

stage() { echo ""; echo "════ $1 ════"; }
die() { echo "FAIL: $1" >&2; exit 1; }

stage "stage 0: wait for systemd, locate the package"
# is-system-running exits non-zero for every state but "running", so
# capture its output regardless of exit code; "degraded" is normal in a
# container (units like modules-load can't work there).
i=0
while :; do
	state=$(systemctl is-system-running 2>/dev/null || true)
	case "$state" in running|degraded) break ;; esac
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		systemctl list-units --failed --no-pager || true
		die "systemd not up after 60s (state: ${state:-unknown})"
	fi
	sleep 1
done
echo "systemd is $state after ${i}s"

arch=$(dpkg --print-architecture)
set -- /dist/moteld_*_"$arch".deb
[ $# -eq 1 ] && [ -f "$1" ] || die "expected exactly one $arch .deb in /dist, found: $*"
deb=$1
echo "package under test: $deb"

# The root mount as a box systemd has booted has it: shared. In a
# container systemd leaves it as it found it, private, and what a
# shared mount does is then never seen: a recursive bind of it is its
# peer, and unmounting a disk beneath the bind unmounts the disk
# beneath what was bound [M71].
mount --make-rshared /
[ "$(findmnt -no PROPAGATION /)" = shared ] || die "the root mount is $(findmnt -no PROPAGATION /), not shared: the mount rows would prove nothing of a real box"

stage "stage 1: install, service basics, reload"
# The image ships without apt lists; refresh them so the package's
# Depends (systemd's floor, libpam-systemd, dbus, openssh-client,
# restic, sqlite3, ca-certificates) resolve the way they would on a
# real box — that resolution is part of what this stage proves. The
# image installs no recommended package (packaging/test/Dockerfile), as
# Hetzner's Debian 13 has it: what the package needs has to arrive
# there too, so it is all Depends, and the premise is checked first.
[ "$(apt-config dump APT::Install-Recommends)" = 'APT::Install-Recommends "false";' ] \
	|| die "the image's apt setting is '$(apt-config dump APT::Install-Recommends)', want 'APT::Install-Recommends \"false\";' as packaging/test/Dockerfile writes it: the install below would prove nothing of a box that installs no recommended package"
# And none of the three is here before the install, or their presence
# after it would prove nothing of what the package brings. That they
# came by Depends, and not by another package's pull, is the
# declaration check below (beside the systemd floor's): this is the
# precondition, that is the proof.
for p in restic sqlite3 ca-certificates; do
	[ "$(dpkg-query -W -f='${db:Status-Status}' "$p" 2>/dev/null)" != installed ] \
		|| die "$p is in the image already: the install below cannot show the package brings it"
done
apt-get update -qq
apt-get install -y "$deb" >/tmp/install.out 2>&1 \
	|| { cat /tmp/install.out; die "apt-get install of the package failed"; }
cat /tmp/install.out
# Not enabled yet, so postinstall owes the operator the start command.
# Root runs the package, so the commands it prints name no sudo, which a
# box may not have (#323). apt runs the scripts on a pty, which ends
# each line CR LF [measured].
tr -d '\r' </tmp/install.out | grep -A1 -x 'moteld installed. Start it with:' | grep -qx '  systemctl enable --now moteld' \
	|| die "a fresh install did not print the 'systemctl enable --now moteld' hint, with no sudo"

id motel >/dev/null || die "postinstall did not create the motel user"
# Every packaged file's mode is pinned in nfpm.yaml rather than
# inherited from the build host's umask; check the one that matters.
bmode=$(stat -c '%U:%G %a' /usr/bin/moteld)
[ "$bmode" = "root:root 755" ] \
	|| die "/usr/bin/moteld is '$bmode', want 'root:root 755' — an unpinned mode follows the builder's umask"
getent group motel >/dev/null || die "postinstall did not create the motel group"
# The package's directories are group-motel, so postinstall must
# ESTABLISH the account's membership rather than assume it — an account
# an admin or another package created keeps whatever groups it had.
id -nG motel | tr ' ' '\n' | grep -qx motel \
	|| die "the motel user is not in the motel group; postinstall must add it, not assume it"
for d in /var/lib/motel /var/lib/liveswap; do
	got=$(stat -c '%U:%G %a' "$d")
	[ "$got" = "motel:motel 750" ] || die "$d is '$got', want 'motel:motel 750'"
done
echo "user/group and data dir ownership OK"

# The starter config is no conffile, so no upgrade can ask about it:
# postinstall installs it from /usr/share and records its digest where
# only root can write (stage 5 follows it through upgrades).
STARTER=/usr/share/motel/Caddyfile
# The starter's reload line names no sudo either (#323).
grep -qx '#   systemctl reload moteld' "$STARTER" \
	|| die "the starter does not give its reload line, with no sudo"
CONFIG=/etc/motel/Caddyfile
RECORD=/var/lib/motel-package/starter.sha256
sha() { sha256sum <"$1" | cut -d' ' -f1; }
conffiles=$(dpkg-query -W -f='${Conffiles}' moteld)
[ -z "$conffiles" ] || die "the package lists conffiles, which an upgrade can ask about: $conffiles"
for f in "$STARTER" "$CONFIG" "$RECORD"; do
	got=$(stat -c '%U:%G %a %F' "$f")
	[ "$got" = "root:root 644 regular file" ] || die "$f is '$got', want 'root:root 644 regular file'"
done
cmp -s "$STARTER" "$CONFIG" || die "a fresh install's $CONFIG is not the starter"
got=$(stat -c '%U:%G %a' "${RECORD%/*}")
[ "$got" = "root:root 700" ] || die "${RECORD%/*} is '$got', want 'root:root 700'"
[ "$(cat "$RECORD")" = "$(sha "$STARTER")" ] || die "the record is not the starter's sha256: $(cat "$RECORD")"
runuser -u motel -- sh -c ": >>$RECORD" 2>/dev/null \
	&& die "the motel user can write the record: a compromised motel could make an upgrade overwrite the box's config"
runuser -u motel -- touch "${RECORD%/*}/smoke" 2>/dev/null \
	&& die "the motel user can create files beside the record"
echo "starter installed, no conffiles, the record root's alone"

# A bug report quotes this line (SECURITY.md), so it names the release
# ahead of the Caddy it was built with (the Makefile's GO_LDFLAGS). The
# package carries the same release, with nfpm's "~" for a prerelease's
# "-", which makes dpkg sort it before the release.
want="v$(dpkg-query -W -f='${Version}' moteld | tr '~' '-')"
got=$(/usr/bin/moteld version) || die "moteld version failed"
echo "$got"
case "$got" in "$want "*) : ;; *) die "moteld version printed '$got', want it to start with '$want ' (the package's release)" ;; esac
for m in liveswap http.handlers.liveswap_webhook \
	http.reverse_proxy.upstreams.liveswap http.handlers.hint_penaltybox \
	http.handlers.cache storages.cache.otter; do
	/usr/bin/moteld list-modules | grep -qx "$m" || die "module $m not linked in"
done
echo "all product modules linked"

# Type=notify, but Caddy sends READY=1 before it knows whether the
# config loaded, so enable --now returning 0 is not the readiness
# assertion — the curl below is.
timeout 300 systemctl enable --now moteld || die "systemctl enable --now moteld failed"
curl -fsS --max-time 5 http://127.0.0.1:80/ | grep -q "moteld is running" \
	|| die "starter Caddyfile not serving on :80"
echo "unit started and starter config answers on :80"

uid=$(id -u motel)
systemctl is-active --quiet "user@$uid.service" \
	|| die "motel's systemd user manager (user@$uid) is not active — postinstall linger/drop-in broken (is libpam-systemd installed?)"
[ -f /var/lib/systemd/linger/motel ] || die "lingering not enabled for motel"
[ -f /etc/systemd/system/moteld.service.d/10-user-manager.conf ] || die "user-manager drop-in missing"
[ -S "/run/user/$uid/systemd/private" ] || die "user manager private socket missing"
echo "user manager user@$uid active with lingering"

# ExecReload connects through the admin unix socket — this call IS the
# socket's functional test.
systemctl reload moteld || die "systemctl reload (ExecReload via admin unix socket) failed"
systemctl is-active --quiet moteld || die "service not active after reload"
# The admin API must NOT answer on TCP: localhost includes every
# deployed app, and an SSRF bug in one could otherwise reconfigure the
# server. The unix socket is the whole point.
curl -s --max-time 2 -o /dev/null http://127.0.0.1:2019/config/ \
	&& die "admin API answers on TCP :2019 — it must live only on the unix socket" || true
[ -S /run/motel/admin.sock ] || die "admin unix socket missing from /run/motel"
curl -fsS --max-time 5 http://127.0.0.1:80/ | grep -q "moteld is running" \
	|| die "not serving after reload"
[ "$(systemctl show -p NRestarts --value moteld)" = "0" ] \
	|| die "moteld restarted itself during install/reload (NRestarts != 0): it came up, died and systemd brought it back — see the journal"
# The unit's restart policy, read off the loaded unit: a crashed
# moteld comes back, a start it refused (exit 1) is never retried.
[ "$(systemctl show -p Restart --value moteld)" = "on-failure" ] \
	|| die "unit Restart is '$(systemctl show -p Restart --value moteld)', want on-failure: a crashed moteld would stay down"
[ "$(systemctl show -p RestartPreventExitStatus --value moteld)" = "1" ] \
	|| die "unit RestartPreventExitStatus is '$(systemctl show -p RestartPreventExitStatus --value moteld)', want 1: a refused start would loop"
[ "$(systemctl show -p RestartUSec --value moteld)" = "1s" ] \
	|| die "unit RestartSec is '$(systemctl show -p RestartUSec --value moteld)', want 1s"
[ "$(systemctl show -p TimeoutStartUSec --value moteld)" = "4min" ] \
	|| die "unit TimeoutStartSec is '$(systemctl show -p TimeoutStartUSec --value moteld)', want 4min: the start's bounded probe path (190s) must finish inside it, with margin"
# 'permission denied' / 'read-only file system' would mean the unit's
# XDG dirs point somewhere the sandboxed motel user cannot write —
# that breaks ACME cert persistence in production even though the
# service superficially runs.
journalctl -u moteld --no-pager \
	| grep -Ei 'panic|SIGSEGV|permission denied|read-only file system' \
	&& die "crash or writability error in the journal (see lines above)" || true
echo "reload OK, journal clean"

stage "stage 1b: the box applier's units and exchange tree"
# box/DESIGN-box.md, "The applier unit". box/units_test.go holds the
# files to the design; this stage holds the manager's reading of them,
# what postinstall did with them, and what the service may do inside
# its sandbox — the one lane that runs the units as the package ships
# them, first with a stub for the applier, then with `moteld box
# apply` itself; pushes that apply are the e2e lane's.
B=/var/lib/motel-box
[ "$(systemctl is-enabled motel-box-apply.path)" = enabled ] \
	|| die "postinstall did not enable motel-box-apply.path ($(systemctl is-enabled motel-box-apply.path))"
systemctl is-active --quiet motel-box-apply.path \
	|| die "postinstall did not start motel-box-apply.path: $(systemctl show -p ActiveState,Result motel-box-apply.path | tr '\n' ' ')"
[ "$(systemctl is-enabled motel-box-apply.service)" = static ] \
	|| die "motel-box-apply.service is '$(systemctl is-enabled motel-box-apply.service)', want static: the path unit is its only start"
[ "$(command -v ssh-keygen)" = /usr/bin/ssh-keygen ] || die "ssh-keygen is not installed: the openssh-client Depends did not resolve"
# The declaration, one entry per line, matched whole: a substring
# match would also accept `libpam-systemd (>= 257)`. The floor is what
# makes apt refuse an older release at install, as docs/first-deploy.md
# step 2 quotes it; Debian 13 meets it, so this image cannot show the
# refusal, and this is what keeps the quote true. The backups' programs
# and the CA store are Depends so that a provider's image that installs
# no recommended package gets them (this image is one; stage 2c runs
# the programs, and the store is read back below).
deps=$(dpkg-deb -f "$deb" Depends | tr ',' '\n' | sed 's/^ *//; s/ *$//')
for p in 'systemd (>= 257)' restic sqlite3 ca-certificates; do
	printf '%s\n' "$deps" | grep -qxF "$p" \
		|| die "the package does not depend on $p: $(printf '%s' "$deps" | tr '\n' ',')"
done
[ -s /etc/ssl/certs/ca-certificates.crt ] \
	|| die "ca-certificates did not come with the package, installed without recommended packages: restic and moteld would verify no certificate"
# The tree as the shipped tmpfiles.d file says, read back off the disk
# (the file itself is held to the Paths table by box/units_test.go).
conf=/usr/lib/tmpfiles.d/motel-box.conf
[ -f "$conf" ] || die "$conf is not installed"
n=0
while read -r typ path mode user group _; do
	case "$typ" in '' | '#'*) continue ;; esac
	want="$(printf '%o' "$((8#$mode))") $user:$group"
	got=$(stat -c '%a %U:%G' "$path") || die "$path does not exist: postinstall's systemd-tmpfiles --create did not make it"
	[ "$got" = "$want" ] || die "$path is '$got', want '$want' (from $conf)"
	n=$((n + 1))
done <"$conf"
[ "$n" -eq 7 ] || die "$conf made $n directories, want the 7 of the Paths table"
# The box's key for secrets, made by postinstall on this running system
# (box/DESIGN-box.md, "The box's key"): the identity root's alone, its
# recipient in age.pub, and `moteld box key` printing that recipient
# alone on stdout.
[ "$(stat -c '%a %U:%G' /etc/motel/age/identity)" = "600 root:root" ] \
	|| die "postinstall did not make /etc/motel/age/identity 0600 root:root: $(stat -c '%a %U:%G' /etc/motel/age/identity 2>&1)"
[ "$(stat -c '%a %U:%G' /etc/motel/age.pub)" = "644 root:root" ] \
	|| die "/etc/motel/age.pub is not 0644 root:root: $(stat -c '%a %U:%G' /etc/motel/age.pub 2>&1)"
box_key=$(moteld box key 2>/dev/null) || die "moteld box key failed"
case "$box_key" in age1*) : ;; *) die "moteld box key printed '$box_key', want the age1… recipient alone" ;; esac
[ "$box_key" = "$(cat /etc/motel/age.pub)" ] || die "age.pub is not the key moteld box key prints"
grep -qx "# public key: $box_key" /etc/motel/age/identity || die "the identity does not hold the key age.pub names"
# The manager's own reading of the units: what box/units_test.go reads
# from the files is what systemd took from them.
show() { systemctl show -p "$2" --value "$1"; }
expect_show() { # <unit> <property> <want>
	got=$(show "$1" "$2")
	[ "$got" = "$3" ] || die "$1 $2 is '$got', want '$3'"
}
svc=motel-box-apply.service
expect_show $svc Type oneshot
expect_show $svc CapabilityBoundingSet "cap_dac_override cap_fowner cap_kill cap_setgid cap_setuid"
expect_show $svc ProtectSystem strict
expect_show $svc ReadWritePaths "/etc/motel /var/lib/motel-box"
expect_show $svc PrivateDevices yes
expect_show $svc ProtectKernelTunables yes
expect_show $svc ProtectControlGroups yes
expect_show $svc DevicePolicy closed
expect_show $svc PrivateTmp yes
expect_show $svc NoNewPrivileges yes
expect_show $svc RestrictSUIDSGID yes
expect_show $svc RestrictAddressFamilies AF_UNIX
expect_show $svc SystemCallArchitectures native
# moteld.service itself: the one capability, and no way to another.
expect_show moteld.service AmbientCapabilities cap_net_bind_service
expect_show moteld.service CapabilityBoundingSet cap_net_bind_service
expect_show moteld.service NoNewPrivileges yes
expect_show moteld.service RestrictSUIDSGID yes
# An allow list (no leading ~) that still holds io_uring_enter, the
# rest of @aio, and not io_uring_setup: as for app units below.
filter=" $(show $svc SystemCallFilter) "
case "$filter" in
" ~"*) die "$svc SystemCallFilter is a deny list: $filter" ;;
*" io_uring_setup "*) die "$svc SystemCallFilter still allows io_uring_setup" ;;
*" io_uring_enter "*) ;;
*) die "$svc SystemCallFilter holds no io_uring_enter, so @aio is missing and io_uring_setup's absence proves nothing: $filter" ;;
esac
expect_show $svc TimeoutStartUSec infinity
expect_show $svc StartLimitIntervalUSec 0
expect_show motel-box-apply.path StartLimitIntervalUSec 0
expect_show $svc User ""
expect_show motel-box-apply.path TriggerLimitIntervalUSec 10s
expect_show motel-box-apply.path TriggerLimitBurst 20
case " $(show motel-box-apply.path After) " in *" moteld.service "*) : ;; *) die "the path unit is not After=moteld.service" ;; esac
# Every path the shipped file names, as the manager took it: one
# "path (Type)" line each, so a spec the manager dropped shows here.
want_paths=$(sed -n 's/^\(DirectoryNotEmpty\|PathExists\|PathExistsGlob\)=\(.*\)$/\2 (\1)/p' /lib/systemd/system/motel-box-apply.path | sort)
[ "$(printf '%s\n' "$want_paths" | wc -l)" -eq 9 ] || die "the shipped path unit names $(printf '%s\n' "$want_paths" | wc -l) paths, want 9"
got_paths=$(show motel-box-apply.path Paths | sort)
[ "$got_paths" = "$want_paths" ] || die "the manager watches
$got_paths
and the unit file names
$want_paths"
# A path unit's default Before=paths.target, beside After=moteld.service,
# is an ordering cycle the manager breaks by deleting paths.target's
# start job at boot and moteld's stop job at shutdown.
case " $(show motel-box-apply.path Before) " in *" paths.target "*) die "the path unit is Before=paths.target: with After=moteld.service that is an ordering cycle" ;; esac
cycle=$(systemd-analyze verify default.target 2>&1 | grep -i 'ordering cycle' || true)
[ -z "$cycle" ] || die "the boot transaction has an ordering cycle: $cycle"
# The motel uid's reach into the tree, path by path: its own stage/
# and nothing else it can write but in/ (below); the base, work/ and
# out/ are root's.
as_motel() { su -s /bin/sh motel -c "$1" 2>/dev/null; }
as_motel "touch $B/stage/smoke && rm $B/stage/smoke" || die "the motel uid cannot write $B/stage"
for p in "$B/smoke" "$B/out/smoke" "$B/work/smoke"; do
	as_motel "touch $p" && die "the motel uid could create $p"
done
as_motel "ls $B/work" && die "the motel uid can list $B/work"
# The units run: the service's ExecStart swapped for a stub by a runtime
# drop-in, everything else as shipped. In the first run the stub probes
# the sandbox — reloads moteld, verifies a signature as uid 65534
# under its PrivateTmp, kills a child of that uid, writes where it may
# and not elsewhere; in every run it moves whatever is in in/ to work/,
# empties work/ and removes the record, as the applier does.
sig=/root/box-smoke
rm -rf "$sig"; mkdir -p "$sig"
ssh-keygen -q -t ed25519 -N '' -C smoke -f "$sig/key"
printf 'box smoke payload\n' >"$sig/payload"
ssh-keygen -q -Y sign -f "$sig/key" -n git "$sig/payload" || die "could not sign the smoke payload"
printf 'smoke namespaces="git" %s\n' "$(cut -d' ' -f1,2 "$sig/key.pub")" >"$sig/allowed"
cat >/run/motel-box-smoke.sh <<'EOF'
#!/bin/sh
B=/var/lib/motel-box
out=$B/smoke-result
: >"$out"
r() { echo "$1=$2" >>"$out"; }
if [ -e /run/motel-box-smoke.probe ]; then
	systemctl reload moteld; r reload $?
	d=$(mktemp -d /tmp/box-verify-smoke.XXXXXX)
	cp /root/box-smoke/payload.sig /root/box-smoke/allowed "$d/" && chmod 755 "$d" && chmod 644 "$d"/*
	setpriv --reuid=65534 --regid=65534 --clear-groups env -i PATH=/usr/bin:/bin \
		ssh-keygen -Y verify -f "$d/allowed" -I smoke -n git -s "$d/payload.sig" </root/box-smoke/payload >/dev/null 2>&1
	r verify $?
	rm -rf "$d"
	setpriv --reuid=65534 --regid=65534 --clear-groups sleep 30 &
	child=$!
	# Killed only once it is uid 65534: before setpriv's switch it is
	# root's own, and the kill would pass without CAP_KILL. A kill that
	# fails is not waited for: the manager ends the child with the unit.
	i=0
	until [ "$(awk '/^Uid:/ {print $2}' /proc/$child/status 2>/dev/null)" = 65534 ]; do
		i=$((i + 1)); [ "$i" -ge 100 ] && break
		sleep 0.05
	done
	r childuid "$(awk '/^Uid:/ {print $2}' /proc/$child/status 2>/dev/null)"
	if kill "$child"; then r kill 0; wait "$child"; else r kill 1; fi
	touch /etc/motel/.smoke-box && rm /etc/motel/.smoke-box; r etc $?
	touch /var/lib/motel/.smoke-box 2>/dev/null; r varlibmotel $?
	r blockdevs "$(find /dev -type b 2>/dev/null | wc -l)"
	v=$(cat /proc/sys/kernel/printk_ratelimit)
	(echo "$v" >/proc/sys/kernel/printk_ratelimit) 2>/dev/null; r sysctl $?
	(: 1<>/sys/fs/cgroup/cgroup.procs) 2>/dev/null; r cgroup $?
fi
find "$B/in" -mindepth 1 -maxdepth 1 -exec mv -t "$B/work/" {} + || r move 1
find "$B/work" -mindepth 1 -delete; r clear $?
rm -f "$B/txn.json"
r left "$(find "$B/in" "$B/work" -mindepth 1 | wc -l)"
EOF
mkdir -p /run/systemd/system/$svc.d
printf '[Service]\nExecStart=\nExecStart=/bin/sh /run/motel-box-smoke.sh\n' >/run/systemd/system/$svc.d/smoke.conf
systemctl daemon-reload
res() { sed -n "s/^$1=//p" "$B/smoke-result"; }
# One round: an entry made, a run that clears it — judged from inside
# the unit and from outside — and the path unit waiting again. A round
# "stopped" stops the path unit while the entry is made (it would fire
# at the entry's first step) and starts it again, so it finds the entry
# at once; any other round makes the entry while the path unit waits,
# so its watch is what fires.
box_round() { # <what> <maker: motel|root> <command> <stopped|waiting>
	[ "$4" = stopped ] && systemctl stop motel-box-apply.path
	rm -f "$B/smoke-result"
	if [ "$2" = root ]; then sh -c "$3"; else as_motel "$3"; fi || die "could not make $1"
	if [ "$4" = stopped ]; then
		systemctl start motel-box-apply.path || die "$1: the path unit did not start: $(systemctl show -p ActiveState,Result motel-box-apply.path | tr '\n' ' ')"
	fi
	i=0
	until [ -s "$B/smoke-result" ] && grep -q '^left=' "$B/smoke-result" && [ "$(show $svc ActiveState)" = inactive ]; do
		i=$((i + 1))
		[ "$i" -ge 90 ] && die "$1: no run of the service within 45s: path $(systemctl show -p ActiveState,Result motel-box-apply.path | tr '\n' ' '), service $(systemctl show -p ActiveState,Result $svc | tr '\n' ' ')"
		sleep 0.5
	done
	[ -z "$(res move)" ] && [ "$(res clear)" = 0 ] && [ "$(res left)" = 0 ] \
		&& [ -z "$(find "$B/in" "$B/work" -mindepth 1)" ] && [ ! -e "$B/txn.json" ] \
		|| die "$1: the run did not clear it: $(tr '\n' ' ' <"$B/smoke-result") left: $(find "$B/in" "$B/work" "$B/txn.json" -mindepth 0 2>/dev/null | tr '\n' ' ')"
	wait_path_waiting "$1"
	echo "$1: started a run, which cleared it"
}
# The path unit back to waiting, its last trigger a success.
wait_path_waiting() { # <what>
	i=0
	until [ "$(show motel-box-apply.path SubState)" = waiting ] && [ "$(show motel-box-apply.path Result)" = success ]; do
		i=$((i + 1))
		[ "$i" -ge 20 ] && die "$1: the path unit is not waiting after the run: $(systemctl show -p ActiveState,SubState,Result motel-box-apply.path | tr '\n' ' ')"
		sleep 0.5
	done
}
# The probe, and the two capabilities that let a run clear what the
# motel uid makes: a 000 directory (CAP_DAC_OVERRIDE) and a sticky
# directory holding its file (CAP_FOWNER).
: >/run/motel-box-smoke.probe
box_round "a 000 directory in in/" motel "mkdir $B/in/x000 && touch $B/in/x000/f && chmod 000 $B/in/x000" stopped
rm -f /run/motel-box-smoke.probe
echo "inside the unit:"; sed 's/^/  /' "$B/smoke-result"
[ "$(res reload)" = 0 ] || die "systemctl reload moteld failed inside the applier's unit"
[ "$(res verify)" = 0 ] || die "ssh-keygen -Y verify as uid 65534 under the unit's PrivateTmp failed"
[ "$(res childuid)" = 65534 ] || die "the child to kill was uid '$(res childuid)', not 65534: the kill check would prove nothing"
[ "$(res kill)" = 0 ] || die "the unit could not kill a uid-65534 child (CAP_KILL)"
[ "$(res etc)" = 0 ] || die "the unit cannot write /etc/motel"
[ "$(res varlibmotel)" != 0 ] || die "the unit could write /var/lib/motel: ProtectSystem=strict is not in force"
# The host's block devices, which the unit must not see: without one
# out here, "none in there" would prove nothing.
[ -n "$(find /dev -type b 2>/dev/null | head -1)" ] || die "this host has no block device under /dev; the PrivateDevices check would prove nothing"
[ "$(res blockdevs)" = 0 ] || die "the unit sees $(res blockdevs) block devices: PrivateDevices=yes is not in force, and root opens a device it owns with no capability"
[ "$(res sysctl)" != 0 ] || die "the unit could write a sysctl: ProtectKernelTunables=yes is not in force"
[ "$(res cgroup)" != 0 ] || die "the unit could open the cgroup tree for writing: ProtectControlGroups=yes is not in force"
[ "$(stat -c %G "$B/smoke-result")" = motel ] || die "a file root made in $B is not group motel: the setgid bit is missing"
box_round "a sticky directory in in/" motel "mkdir $B/in/sticky && chmod 1777 $B/in/sticky && touch $B/in/sticky/f" stopped
# Then one round for every path the manager says the path unit watches,
# its entry renamed into place while the path unit waits, as the
# handler and the applier write: for each glob, a name no other line of
# its directory counts (systemd's DirectoryNotEmpty= skips names it takes
# for hidden or backup files); the record itself; and a plain name for
# DirectoryNotEmpty= — which <dir>/* counts too, so those two rounds
# show the pair, not the line alone. in/ is filled by the motel uid
# through stage/, the rest by root.
n=0
while read -r spec kind; do
	dir=$spec
	case "$kind" in
	"(DirectoryNotEmpty)") entry=$dir/plain ;;
	"(PathExists)") entry=$spec dir=$(dirname "$spec") ;;
	"(PathExistsGlob)")
		dir=$(dirname "$spec")
		case "$(basename "$spec")" in
		'*') entry=$dir/stale~ ;;
		'.[!.]*') entry=$dir/.hidden ;;
		'..?*') entry=$dir/..hidden ;;
		*) die "no sample name for the glob $spec: add one here" ;;
		esac ;;
	*) die "a path spec of a kind this stage has no round for: $spec $kind" ;;
	esac
	maker=root tmp=$B/.smoke-entry
	[ "$dir" = "$B/in" ] && maker=motel tmp=$B/stage/smoke-entry
	box_round "$spec $kind" "$maker" "touch '$tmp' && mv '$tmp' '$entry'" waiting
	n=$((n + 1))
done <<<"$got_paths"
[ "$n" -eq 9 ] || die "$n rounds, want one for each of the path unit's 9 paths"
systemctl is-active --quiet moteld || die "moteld is not active after the reload inside the applier's unit"
rm -f /run/systemd/system/$svc.d/smoke.conf "$B/smoke-result" /run/motel-box-smoke.sh
rmdir /run/systemd/system/$svc.d
systemctl daemon-reload
wait_path_waiting "the path unit, its stub removed"
echo "box applier: path unit enabled and waiting; tree as tmpfiles.d says; the unit reloads moteld, verifies as 65534, kills its child; every path the unit watches starts a run that clears it"
# The real applier in the unit as shipped, no stub: what the motel
# uid drops that is not a bundle — a hidden 000 directory, a sticky
# directory holding its file, a plain file — is taken and removed with
# an error line and no result; an empty file named as a bundle is
# refused when its bundle is read, its result written in out/ as the
# Paths table says. The run exits 0, and the path unit waits again.
bad_id=0123456789abcdef0123456789abcdef
real_journal() { journalctl -u $svc --since "$since" --no-pager -o cat; }
systemctl stop motel-box-apply.path
since=$(date '+%Y-%m-%d %H:%M:%S')
as_motel "mkdir $B/in/.x000 && touch $B/in/.x000/f && chmod 000 $B/in/.x000 && mkdir $B/in/sticky && chmod 1777 $B/in/sticky && touch $B/in/sticky/f $B/in/junk $B/in/$bad_id.tar" \
	|| die "could not drop the real applier's entries into $B/in"
systemctl start motel-box-apply.path || die "the path unit did not start: $(systemctl show -p ActiveState,Result motel-box-apply.path | tr '\n' ' ')"
# Done when the tree is clear and the service has stopped, either way:
# a failed run is reported below, not as a timeout. The journal is
# synced before it is counted, so a line still on its way is not missed.
i=0
until [ -z "$(find "$B/in" "$B/work" -mindepth 1)" ] && [ -e "$B/out/$bad_id.json" ] \
	&& case "$(show $svc ActiveState)" in inactive | failed) true ;; *) false ;; esac; do
	i=$((i + 1))
	[ "$i" -ge 120 ] && die "the real applier did not clear in/ within 60s: in/work: $(find "$B/in" "$B/work" -mindepth 1 | tr '\n' ' ') service $(systemctl show -p ActiveState,Result,ExecMainStatus $svc | tr '\n' ' '); $(real_journal | tail -20)"
	sleep 0.5
done
[ "$(show $svc Result)" = success ] && [ "$(show $svc ExecMainStatus)" = 0 ] \
	|| die "the real applier's run did not end clean: $(systemctl show -p Result,ExecMainStatus $svc | tr '\n' ' '); $(real_journal | tail -20)"
journalctl --sync
removed=$(real_journal | grep -c 'removing an entry that is not a bundle' || true)
[ "$removed" -eq 3 ] || die "the real applier named $removed entries that are not bundles, want 3: $(real_journal | tail -20)"
[ "$(ls -A "$B/out")" = "$bad_id.json" ] || die "out/ holds $(ls -A "$B/out" | tr '\n' ' '), want only $bad_id.json: an entry that is not a bundle gets no result"
got=$(stat -c '%a %U:%G' "$B/out/$bad_id.json")
[ "$got" = "640 root:motel" ] || die "the real applier's result is '$got', want '640 root:motel' (Paths table)"
grep -Eq '"phase": *"(failed|refused)"' "$B/out/$bad_id.json" || die "the result of a bundle that is not one is not terminal: $(cat "$B/out/$bad_id.json")"
echo "  its result: $(cat "$B/out/$bad_id.json")"
wait_path_waiting "the real applier's run"
rm -f "$B/out/$bad_id.json"
echo "the real applier, in the unit as shipped: three entries that are not bundles removed with no result, one bad bundle settled in out/ (640 root:motel), exit 0"

# An unreadable record, with the real applier: each run journals it,
# takes nothing, ends with the left-on-disk line (the restart remedy)
# and exits 0, so the path unit starts the next at once
# and ends at its trigger limit; removing the record does not start it
# again, and `systemctl restart motel-box-apply.path`, once the
# limit's ten seconds have passed, does (DESIGN-box.md, "Why the path
# unit cannot loop"). The record is written the way root writes one, by
# rename.
since=$(date '+%Y-%m-%d %H:%M:%S')
t0=$(date +%s)
printf 'not a record\n' >"$B/.smoke-txn" && mv "$B/.smoke-txn" "$B/txn.json"
i=0
until [ "$(show motel-box-apply.path Result)" = trigger-limit-hit ]; do
	i=$((i + 1))
	[ "$i" -ge 120 ] && die "an unreadable record did not end at the trigger limit within 60s: path $(systemctl show -p ActiveState,SubState,Result motel-box-apply.path | tr '\n' ' '); $(real_journal | tail -5)"
	sleep 0.5
done
t1=$(date +%s)
journalctl --sync
runs=$(real_journal | grep -c 'txn.json cannot be read' || true)
[ "$runs" -ge 1 ] || die "the path unit hit its trigger limit, but no run journaled the unreadable record"
real_journal | grep 'box apply leaves work on disk' | grep -qF '"left":["txn.json"]' || die "no run ended with the left-on-disk line counting the record"
real_journal | grep -q 'once the console has cleared the cause, systemctl restart motel-box-apply.path, and again ten seconds later if that fails' || die "the left-on-disk line does not name the restart the console needs, and when"
echo "an unreadable record: $runs runs, then trigger-limit-hit, within $((t1 - t0 + 1))s"
rm -f "$B/txn.json"
sleep 2
[ "$(show motel-box-apply.path ActiveState)" = failed ] \
	|| die "the path unit left trigger-limit-hit with no restart: $(systemctl show -p ActiveState,SubState,Result motel-box-apply.path | tr '\n' ' ')"
while [ "$(date +%s)" -le $((t1 + 11)) ]; do sleep 1; done
systemctl restart motel-box-apply.path || die "systemctl restart motel-box-apply.path failed past the limit's ten seconds"
wait_path_waiting "the path unit, restarted after the record was removed"
echo "removing the record left the path unit failed; systemctl restart started it again"

stage "stage 2: liveswap deploy under the systemd sandbox"
# Generate a local deploy keypair; the app trusts the public half, and
# we mint the deploy bearer with the private half. The service (User=
# motel) reads the 0644 public key; the 0600 private key stays with
# root here, standing in for the operator's token-minting machine.
moteld deploy-keygen --out /etc/motel/deploy.key

# The user manager's PID, handed to the app so it can try the routes
# the sandbox is supposed to close. Captured before the config is
# written; the manager has been running since stage 1.
mgr_pid=$(systemctl show -p MainPID --value "user@$uid.service")
[ -n "$mgr_pid" ] && [ "$mgr_pid" != "0" ] || die "no MainPID for user@$uid.service"

# Overwriting the starter makes this a configured box, whose file
# stages 3 and 5 prove no upgrade touches.
cat > /etc/motel/Caddyfile <<'EOF'
{
	admin unix//run/motel/admin.sock
	auto_https off

	liveswap {
		root /var/lib/liveswap
		allow_insecure_http
		artifact_allowlist 127.0.0.1:8200
		deploy_trust local {
			public_key /etc/motel/deploy.key.pub
			audience smoke
		}

		app demo {
			command ./server
			env MGR_PID __MGR_PID__
			env MOTEL_UID __MOTEL_UID__
			health_interval 250ms
			health_timeout 1s
			soak 1s
			deadline 20s
		}
	}
}

:8080 {
	reverse_proxy {
		dynamic liveswap demo
	}
}

:8081 {
	liveswap_webhook
}
EOF

# The heredoc above is quoted — it has to be, it carries Caddy's own
# {...} placeholders — so the two runtime values are substituted here.
# Without this the app receives the literal strings and every /proc
# probe below tests a path that cannot exist, passing vacuously.
sed -i "s|__MGR_PID__|$mgr_pid|; s|__MOTEL_UID__|$uid|" /etc/motel/Caddyfile
grep -q '__MGR_PID__\|__MOTEL_UID__' /etc/motel/Caddyfile \
	&& die "placeholder substitution failed; the sandbox probes would test nothing"

systemctl daemon-reload
timeout 300 systemctl restart moteld || die "restart with liveswap config failed"

# Mint the deploy bearer with the private key (audience must match the
# deploy_trust block).
TOKEN=$(moteld deploy-token --key /etc/motel/deploy.key --audience smoke --ttl 10m) \
	|| die "deploy-token failed"

i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "webhook status endpoint not ready within 30s"
	sleep 1
done

# Build a deployable artifact on the fly: the "app" is a wrapper around
# `moteld respond`, honoring liveswap's SOCKET contract and answering
# 200 on every path (which satisfies the health gate).
workdir=$(mktemp -d)
# Besides the NOFILE contract the wrapper records what its sandbox
# looks like from inside, by sourcing the shared view probe
# (liveswap/testdata/sandbox-view.sh — one asset with liveswap's integration test
# and e2e/liveswap/systemd.sh, mounted into this container by
# `make install-test`). Sourced, not run as a child, so the probe
# reports $$ as the unit's main pid; the NOFILE values below are
# inherited either way.
cp /sandbox-view.sh "$workdir/sandbox-view.sh" \
	|| die "the view probe is not mounted at /sandbox-view.sh; run this through 'make install-test'"
cat > "$workdir/server" <<'EOF'
#!/bin/sh
. ./sandbox-view.sh
echo "smoke app starting on $SOCKET"
# Caddy's unix//abs/path spelling: "unix/" + the absolute socket path.
exec /usr/bin/moteld respond --listen "unix/$SOCKET" "hello smoke"
EOF
chmod +x "$workdir/server"
mkdir -p /srv/art
tar -czf /srv/art/demo.tar.gz -C "$workdir" server sandbox-view.sh

/usr/bin/moteld file-server --listen 127.0.0.1:8200 --root /srv/art \
	>/tmp/artserver.log 2>&1 &
ART_PID=$!
S3_PID=""
trap 'kill $ART_PID $S3_PID 2>/dev/null || true' EXIT
i=0
until curl -fs -o /dev/null http://127.0.0.1:8200/demo.tar.gz; do
	i=$((i + 1))
	[ "$i" -ge 15 ] && die "artifact file-server not up within 15s"
	sleep 1
done

# A second app on disk, so root_listing below is a real sibling check.
# With only demo there, binding the WHOLE liveswap root into the unit
# would still list exactly "demo" and pass. This one is deliberately
# not in the config: it exists to be invisible, and its secret is what
# a leaking view would expose.
mkdir -p /var/lib/liveswap/other/shared
: > /var/lib/liveswap/other/shared/secret
chown -R motel:motel /var/lib/liveswap/other

# Deploy via Authorization: Bearer — Caddy access logs redact it
# automatically — exercised here under the real unit.
code=$(curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 90 \
	-X POST -H "Authorization: Bearer $TOKEN" \
	-d '{"url":"http://127.0.0.1:8200/demo.tar.gz","version":"s1"}' "$HOOK")
[ "$code" = "200" ] || {
	echo "deploy response: $(cat /tmp/deploy-body)"
	echo "--- app journal (motel-demo):"
	journalctl --no-pager -t motel-demo 2>/dev/null | tail -40
	die "deploy under systemd sandbox failed with HTTP $code — a deny-by-default view fails with ENOENT (or the sibling shell's 'exec: … not found', exit status 127, for the command itself), not a permission error, so if the journal shows a missing path suspect the unit's base view rather than file modes"
}
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" \
	|| die "proxy does not serve the deployed app"
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
case "$status" in
*'"current_version":"s1"'*) : ;;
*) die "status missing current_version s1: $status" ;;
esac
case "$status" in
*'"running":true'*) : ;;
*) die "status missing running:true: $status" ;;
esac
unit=$(printf '%s' "$status" | sed -n 's/.*"unit":"\([^"]*\)".*/\1/p')
[ -n "$unit" ] || die "status missing the app's systemd unit: $status"
# systemctl --user for a nologin system user: same trick motel
# itself uses — its own manager's private socket under /run/user/<uid>.
user_systemctl() { su -s /bin/sh motel -c "XDG_RUNTIME_DIR=/run/user/$uid systemctl --user $*"; }
user_systemctl is-active --quiet "$unit" \
	|| die "app unit $unit is not active under motel's user manager"
[ "$(user_systemctl show -p Restart --value "$unit")" = "no" ] \
	|| die "app unit must have Restart=no (apps are restarted by the liveswap watchdog, not systemd)"
journalctl --no-pager -t motel-demo | grep -q "smoke app starting" \
	|| die "app stdout did not reach the journal under identifier motel-demo"
# The runner's contract: every unit's NOFILE (soft and hard) is the
# user manager's DefaultLimitNOFILE — the very property it reads
# (liveswap/systemd_dbus.go). Assert against that, not a constant: a
# constant that happened to match Docker's PID 1 limit passed
# vacuously. Where the manager is systemd >= 256 the postinstall
# drop-in is what sets that ceiling, so pin it there; below 256 the
# drop-in does not reach the manager and the value is whatever PID 1
# had (#37).
mgr_nofile=$(user_systemctl show -p DefaultLimitNOFILE --value)
# The view the app recorded from inside its unit. The shared probe
# writes it into the app's shared dir — the one writable, persistent
# path in the view — and smoke runs as root, so it is readable here.
# A file, not a journal line: the keys are the ones e2e and the
# integration lane assert too, and reading them back by name beats
# fifteen greps over one flat line.
view=/var/lib/liveswap/demo/shared/view.txt
i=0
until grep -q '^done=1' "$view" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 20 ] && die "view.txt not written by the sandboxed app"
	sleep 0.5
done
probe_val() { sed -n "s/^$1=//p" "$view" | head -1; }
expect_probe() { # <key> <want>
	got=$(probe_val "$1")
	[ "$got" = "$2" ] || die "inside the unit: $1=$got, want $2"
}
# The evidence behind every in-unit assertion below, printed whether or
# not one fails: a cell that dies on one field is much easier to read
# next to the whole view the app actually saw.
echo "in-unit view:"
sed 's/^/  /' "$view"
app_soft=$(probe_val nofile_soft)
app_hard=$(probe_val nofile_hard)
[ -n "$mgr_nofile" ] && [ "$app_soft" = "$mgr_nofile" ] && [ "$app_hard" = "$mgr_nofile" ] \
	|| die "app NOFILE soft=$app_soft hard=$app_hard is not the user manager's DefaultLimitNOFILE ($mgr_nofile) on both"
app_nofile=$app_soft
sd_version=$(systemctl --version | awk 'NR==1 {print $2}')
# The user@<uid> drop-in only reaches the manager from systemd 256 (#37);
# the package requires 257, so this is unconditional.
[ "${sd_version%%[!0-9]*}" -ge 256 ] \
	|| die "systemd $sd_version is below 256, where the user@ drop-in does not reach the manager (#37)"
[ "$mgr_nofile" = "1048576" ] \
	|| die "systemd $sd_version: user@$uid DefaultLimitNOFILE is $mgr_nofile, not the drop-in's 1048576"
echo "app NOFILE $app_nofile = user@$uid DefaultLimitNOFILE = the drop-in's 1048576 (systemd $sd_version)"
# The manager runs under no-new-privileges (the drop-in's flag), and so
# does every unit it starts: the running process's own flag, not only
# the unit's property, since a manager born before the drop-in would
# show the property and lack the flag.
expect_show "user@$uid.service" NoNewPrivileges yes
grep -q '^NoNewPrivs:[[:space:]]*1$' "/proc/$mgr_pid/status" \
	|| die "user@$uid (pid $mgr_pid) runs without NoNewPrivs: $(grep '^NoNewPrivs:' "/proc/$mgr_pid/status")"
echo "user@$uid runs under NoNewPrivileges (the drop-in's)"
# No core dump of a crashed app: LimitCORE=0 on both limits
# (liveswap/systemd_dbus.go; the why is in DESIGN-threat-model.md,
# "Where env_file values sit"). The unit's property, then the limit
# the app itself saw from inside.
[ "$(user_systemctl show -p LimitCORE --value "$unit")" = "0" ] \
	|| die "app unit LimitCORE is '$(user_systemctl show -p LimitCORE --value "$unit")', want 0: a crash could write the app's memory to disk"
[ "$(user_systemctl show -p LimitCORESoft --value "$unit")" = "0" ] \
	|| die "app unit LimitCORESoft is '$(user_systemctl show -p LimitCORESoft --value "$unit")', want 0"
expect_probe core_soft 0
expect_probe core_hard 0
echo "app core-dump limit 0/0: a crash writes no dump"
echo "deployed as unit $unit under user@$uid; app output in the journal"

# The sandbox tier: full — a PID namespace on top of the user namespace
# and the mount set. Debian 13 is systemd 257, so there is one tier and
# the app's own view is the assertion (there is no status field: every
# unit gets the sandbox, so there is nothing to report).
# The host kernel's stance on unprivileged user namespaces is printed
# for the record: it lives in the host kernel, not the container image,
# so a CI runner that restricts them fails this cell even though Debian
# itself does not restrict them.
echo "host: $(uname -r); apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent); virt=$(systemd-detect-virt 2>/dev/null || echo unknown)"
app_uidmap=$(probe_val uidmap)
app_nprocs=$(probe_val nprocs)
expect_probe pid 1
[ -n "$app_nprocs" ] && [ "$app_nprocs" -le 8 ] || die "/proc shows $app_nprocs pids inside the unit"
[ "$app_uidmap" != "4294967295" ] && [ -n "$app_uidmap" ] || die "no user namespace inside the unit (uid_map range $app_uidmap)"
# Sibling IPC (#311), under the real package and the motel user: the
# unit mounted its own mqueuefs (its own IPC namespace), can write its
# own /dev/shm, and nothing written there reaches the host's.
[ "$(probe_val mqueue_owner)" = self ] || die "the unit's /dev/mqueue is not its own (owner $(probe_val mqueue_owner)): no IPC namespace of its own"
[ "$(probe_val shm_write)" = ok ] || die "the unit cannot write its own /dev/shm"
[ ! -e /dev/shm/sandbox-view-canary ] || die "/dev/shm/sandbox-view-canary written inside the unit is on the host: /dev/shm is shared"
expect_probe motel_lib closed
# These are the acceptance paths from DESIGN-sandbox.md. They are
# closed by the user namespace alone — the PID namespace on top is what
# additionally hides and protects sibling processes — so they are the
# assertions that must hold even if a host ever delivers less.
# The probes are only worth anything if the app was handed the real
# values: a literal "$mgr_pid" would make every /proc check below pass
# by testing a path that cannot exist. The probe echoes back what it
# was handed for exactly this comparison.
saw_pid=$(probe_val saw_mgr_pid)
saw_uid=$(probe_val saw_uid)
[ "$saw_pid" = "$mgr_pid" ] \
	|| die "the app was given MGR_PID='$saw_pid', not the manager's real pid $mgr_pid — the /proc probes would be vacuous"
[ "$saw_uid" = "$uid" ] \
	|| die "the app was given MOTEL_UID='$saw_uid', not $uid — the manager-socket probe would be vacuous"
# And prove the routes are reachable at all from outside the sandbox,
# so "closed" means the sandbox closed it rather than the path being
# absent on this host.
[ -r "/proc/$mgr_pid/environ" ] || die "/proc/$mgr_pid/environ is unreadable even to root; the in-unit probe would prove nothing"
[ -e "/run/user/$uid/systemd/private" ] || die "the manager socket does not exist; the in-unit probe would prove nothing"
[ -e /run/motel/admin.sock ] || die "the admin socket does not exist; the in-unit probe would prove nothing"
expect_probe sslprivate absent
for route in mgr_root mgr_environ mgr_socket admin_socket state etc_motel run_motel apptmp current; do
	got=$(probe_val "$route")
	[ "$got" = "closed" ] \
		|| die "$route is '$got' inside the unit: a route the design says is closed is open"
done
echo "in-unit routes closed: manager /proc root+environ, manager socket, admin socket, state.json, /etc/motel, /run/motel, the app's tmp and current"
# The app's own dirs are the other side of that: exactly its release
# (writable), its shared dir as $HOME, and a liveswap root that names
# no other app.
# Guarded: "demo " only means the sandbox hid the sibling if the
# sibling is actually on the host.
[ -d /var/lib/liveswap/other ] \
	|| die "the sibling app fixture is missing; root_listing would prove nothing"
expect_probe root_listing "demo "
expect_probe release writable
expect_probe root readonly
expect_probe home /var/lib/liveswap/demo/shared
expect_probe xdg_runtime unset
expect_probe cgroup readonly
expect_probe tmp writable
# Deny-by-default: nothing bound these, so they do not exist inside the
# unit — absent, not present-but-unreadable.
for d in opt srv home root mnt media etcliveswap; do
	expect_probe "abs_$d" absent
done
# /var/lib is the exception: the liveswap root lives under it, so it is
# present, and varlib_listing below is the real check.
expect_probe abs_varlib present
# The other half of the deny-by-default claim, and the one that keeps
# "closed" from being satisfied by an empty unit: the base view carries
# an OS the app can execute in, /etc is only the named entries (never
# the whole tree, which would hand every app every other app's
# env_file), and /var/lib holds nothing but the path to this app's own
# directories — motel's TLS keys sit next door on the host.
for present in binsh usrbinenv hsbin etcssl resolvconf; do
	got=$(probe_val "$present")
	[ "$got" = "ok" ] \
		|| die "$present is '$got' inside the unit: the base view does not carry a runnable OS, so the closed routes above prove nothing"
done
etclist=$(probe_val etc_listing)
# A non-empty guard in its own right: the case below has only failure
# branches, so an empty listing — /etc gone entirely rather than merely
# narrow — would match nothing and report success on the opposite
# regression to the one it is here to catch.
[ -n "$etclist" ] \
	|| die "/etc inside the unit lists nothing at all: the base view is empty, not narrow"
case " $etclist" in
*" motel "* | *" liveswap "* | *" shadow "*)
	die "/etc inside the unit carries more than the named entries ($etclist): every app would see every other app's env_file" ;;
esac
varliblist=$(probe_val varlib_listing)
[ "$varliblist" = "liveswap " ] \
	|| die "/var/lib inside the unit is '$varliblist', want only 'liveswap': anything else is a path nothing named"
echo "in-unit base view: /bin/sh, /usr/bin/env, /usr/bin/moteld, /etc/ssl/certs, /etc/resolv.conf; /etc=$etclist /var/lib=$varliblist"
[ "$(user_systemctl show -p PrivateUsers --value "$unit")" = "yes" ] || die "unit lacks PrivateUsers=yes"
[ "$(user_systemctl show -p SystemCallArchitectures --value "$unit")" = "native" ] \
	|| die "unit lacks SystemCallArchitectures=native: on amd64 the x86-32 ABI's socketcall() opens any address family"
# The syscall filter the manager built: an allow list (no leading ~)
# that still holds io_uring_enter, the rest of @aio, and not
# io_uring_setup, whose ring would open sockets RestrictAddressFamilies=
# never sees.
filter=" $(user_systemctl show -p SystemCallFilter --value "$unit") " \
	|| die "could not read the unit's SystemCallFilter"
case "$filter" in
" ~"*) die "SystemCallFilter is a deny list: $filter" ;;
*" io_uring_setup "*) die "SystemCallFilter still allows io_uring_setup" ;;
*" io_uring_enter "*) ;;
*) die "SystemCallFilter holds no io_uring_enter, so @aio is missing and io_uring_setup's absence proves nothing: $filter" ;;
esac
# And the filter is in force inside the unit, not only set on it: on a
# kernel without seccomp the manager skips it and the read-back above
# still passes. Inside, the filter mode and io_uring_setup refused;
# from outside, as the same user, the call must reach the kernel, or
# the refusal inside proves nothing about the unit's own filter.
expect_probe seccomp 2
expect_probe io_uring_setup refused
outside=$(as_motel "perl -e 'syscall(425, 1, 0); print \$!{EPERM} ? q(refused) : q(reached)'") || outside="unmeasured (su or perl failed)"
[ "$outside" = "reached" ] \
	|| die "io_uring_setup outside the unit is '$outside', want reached: io_uring_setup=refused inside proves nothing (kernel.io_uring_disabled=$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo absent); anything but 0 can refuse it outside too)"
tmpfs=$(user_systemctl show -p TemporaryFileSystem --value "$unit")
[ "$tmpfs" = "/:ro /dev/shm:mode=1777,size=50%" ] || die "unit's TemporaryFileSystem= is '$tmpfs', want '/:ro /dev/shm:mode=1777,size=50%': the empty root the view starts from, and a /dev/shm of the unit's own (#311)"
# The retired half of the old model, read back off the live unit. Both
# properties still exist on every service, so test the value rather
# than its presence: what must be gone is the setting, not the row.
[ "$(user_systemctl show -p ProtectSystem --value "$unit")" != "strict" ] || die "unit still sets ProtectSystem=strict; the view names what exists rather than making the rest read-only"
case "$(user_systemctl show -p InaccessiblePaths --value "$unit")" in
*/*) die "unit still masks a list with InaccessiblePaths=" ;;
esac
echo "sandbox (systemd $sd_version): uid_map range $app_uidmap, pid $(probe_val pid), $app_nprocs pids visible, native syscall ABI only, seccomp in force (io_uring_setup refused inside, reached outside), /var/lib/motel $(probe_val motel_lib)"

# `moteld validate` provisions and cleans up without starting: run
# as root (no user manager for uid 0) and as the motel user (the
# live manager) against the serving config, and the app must be
# untouched either way.
pid_live=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
moteld validate --config /etc/motel/Caddyfile >/dev/null 2>&1 \
	|| die "moteld validate as root failed against a valid config"
su -s /bin/sh motel -c "moteld validate --config /etc/motel/Caddyfile" >/dev/null 2>&1 \
	|| die "moteld validate as the motel user failed against a valid config"
sleep 1
kill -0 "$pid_live" 2>/dev/null || die "moteld validate stopped the running app (pid $pid_live)"
user_systemctl is-active --quiet "$unit" || die "moteld validate left the app unit inactive"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after validate"
echo "validate (root and motel) left the running app alone"

# The deploy token must never reach the journal: not via access logs
# (Authorization is redacted), not via any error path above.
journalctl -u moteld --no-pager | grep -q "$TOKEN" \
	&& die "deploy token leaked into the journal" || true
echo "journal is free of the deploy token"

stage "stage 2b: a SIGKILLed moteld is back within seconds, reattached"
# Stage 3's upgrade restart is a clean stop and start, which never
# exercises Restart=; this is the one lane that runs the installed
# .deb's unit, so the crash path is proven here too. The unit's own
# record is the assertion, never a systemctl exit status.
hp0=$(systemctl show -p ExecMainPID --value moteld)
systemctl kill --kill-whom=main -s SIGKILL moteld
i=0
until [ "$(systemctl show -p ActiveState --value moteld)" = "active" ] \
	&& [ "$(systemctl show -p ExecMainPID --value moteld)" != "$hp0" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && die "moteld not back within 30s of SIGKILL: $(systemctl show -p ActiveState,SubState,Result,NRestarts moteld | tr '\n' ' ')"
	sleep 0.5
done
[ "$(systemctl show -p NRestarts --value moteld)" = "1" ] \
	|| die "NRestarts is $(systemctl show -p NRestarts --value moteld), want 1: more than one automatic restart after a single kill"
deadline=$(($(date +%s) + 30))
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	[ "$(date +%s)" -ge "$deadline" ] && die "webhook not back within 30s of the automatic restart"
	sleep 1
done
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
[ "$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')" = "$pid_live" ] \
	|| die "app pid changed across moteld's SIGKILL restart: reattach failed: $status"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after moteld's automatic restart"
echo "SIGKILL: systemd restarted moteld (NRestarts=1); app pid $pid_live reattached"

stage "stage 2c: backups — the guide's 'Set it up' lines, as root, and validate and status as a user with no privilege"
# What the package set up, before any line of the guide runs: the
# programs it depends on (installed in stage 1 without recommended
# packages, so they came by Depends alone), the account as setup makes
# it, the four unit files, the timers enabled
# and running and their services untouched, and no credential file —
# that is setup's, and postinstall says so.
S3REPO=s3:http://127.0.0.1:9000/motel
S3KEY=AKIDSMOKEFIXTURE
S3SECRET=smoke-fixture-key-not-a-secret
CRED=/etc/motel-backup/repository.env
TIMERS="motel-backup.timer motel-backup-drill.timer"
# expect_backup_state <step> <is-enabled> <is-active> <absent|sha256>:
# one row of the table, for both timers and the credential file.
expect_backup_state() {
	local u got
	for u in $TIMERS; do
		got=$(systemctl is-enabled "$u" 2>/dev/null || true)
		[ "$got" = "$2" ] || die "$1: $u is-enabled '$got', want '$2'"
		got=$(systemctl is-active "$u" 2>/dev/null || true)
		[ "$got" = "$3" ] || die "$1: $u is-active '$got', want '$3' ($(systemctl show -p Result "$u"); $(journalctl -u "$u" -u "${u%.timer}.service" --no-pager | tail -12 | tr '\n' '|'))"
	done
	if [ "$4" = absent ]; then
		[ ! -e "$CRED" ] || die "$1: $CRED exists, and the package must not make it"
	else
		got=$(sha256sum "$CRED" | cut -d' ' -f1)
		[ "$got" = "$4" ] || die "$1: $CRED changed (was $4, is $got)"
	fi
	echo "$1: timers $2, $3; credential file $4 — as the table says"
}
[ "$(command -v restic)" = /usr/bin/restic ] && [ "$(command -v sqlite3)" = /usr/bin/sqlite3 ] \
	|| die "restic and sqlite3 did not come with the package, installed without recommended packages: they have to be Depends (restic '$(command -v restic)', sqlite3 '$(command -v sqlite3)')"
getent passwd motel-backup | grep -q ':/nonexistent:/usr/sbin/nologin$' \
	|| die "postinstall did not make the motel-backup account as setup does: $(getent passwd motel-backup || echo none)"
for u in motel-backup.service motel-backup-drill.service $TIMERS; do
	got=$(stat -c '%U:%G %a' "/lib/systemd/system/$u")
	[ "$got" = "root:root 644" ] || die "/lib/systemd/system/$u is '$got', want 'root:root 644'"
done
expect_backup_state "after install" enabled active absent
[ ! -e /etc/motel-backup ] || die "the package made /etc/motel-backup; that is setup's"
for u in motel-backup.service motel-backup-drill.service; do
	[ "$(systemctl show -p ActiveState --value "$u")" = inactive ] && [ "$(systemctl show -p Result --value "$u")" = success ] \
		|| die "$u after install: $(systemctl show -p ActiveState,Result "$u" | tr '\n' ' ') — nothing may start before setup"
done
# The whole line, so no sudo stands before it: apt runs the maintainer
# scripts on a pty, and each line ends in \r, taken off first.
tr -d '\r' </tmp/install.out | grep -qx "  motel-backup setup <repository>" \
	|| die "postinstall did not say that the timers wait for setup"
grep -q "enabled and set up" /tmp/install.out \
	&& die "postinstall called the backups set up on a box with no credential file: $(grep Backups: /tmp/install.out)" || true
grep -q "refuse that account" /tmp/install.out \
	&& die "postinstall warned of the account it had just made: $(grep motel-backup /tmp/install.out)" || true
motel-backup account >/dev/null || die "the account postinstall made is not one setup accepts: $(motel-backup account 2>&1)"
echo "the package's own: restic, sqlite3 and ca-certificates by Depends, the account, the units, the timers enabled and waiting"

# The repository the guide's lines are pointed at: rclone serve s3
# here, on the loopback, with a key that is no secret.
mkdir -p /srv/s3
rclone serve s3 --addr 127.0.0.1:9000 --auth-key "$S3KEY,$S3SECRET" /srv/s3 >/tmp/s3.log 2>&1 &
S3_PID=$!
i=0
until curl -s -o /dev/null http://127.0.0.1:9000/; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "rclone serve s3 not up within 15s: $(cat /tmp/s3.log)"
	sleep 0.5
done
# Root runs the guide's lines, as written, but validate and status,
# which need no privilege: an account in no group at all runs those,
# with no sudo (the image has none) — less than the administrator
# Hardening makes, who has adm besides.
useradd -m admin
as_admin() { runuser -u admin -- sh -c "$1"; }
# The app's data, as the app itself would make it: a database and an
# uploads directory under its shared/, the motel user's.
su -s /bin/sh motel -c 'cd /var/lib/liveswap/demo/shared && sqlite3 app.db "create table t(n); insert into t values (1),(2);" && mkdir -p uploads && echo pic >uploads/a.png' \
	|| die "could not seed the demo app's data"
# Step 1 of the guide, the declaration in the app's block, put there
# the way an administrator would edit it in.
sed -i 's|^\t\tapp demo {$|&\n\t\t\tbackup {\n\t\t\t\tsqlite app.db\n\t\t\t\tfiles  uploads\n\t\t\t}|' /etc/motel/Caddyfile
[ "$(grep -c 'sqlite app.db' /etc/motel/Caddyfile)" = 1 ] || die "the backup block did not go into the demo app's block"

# The guide's lines, between its smoke markers; the example
# repository URL is this box's, and the app is demo, as written.
OUT=/tmp/docs.out
. /tty.sh
mapfile -t docs < <(awk '/<!-- smoke: begin -->/{on=1; next} /<!-- smoke: end -->/{on=0} on' /docs-backups.md | grep -v '^```' | grep -v '^#' | grep -v '^$' | sed "s|s3:https://s3.example.com/my-backups|$S3REPO|")
# Exactly the lines this stage runs and checks: a line dropped from the
# guide, or one added that nothing here checks, fails rather than
# passing as "ran as written".
want="motel-backup validate /etc/motel/Caddyfile
systemctl reload moteld
motel-backup setup $S3REPO
systemctl start motel-backup.service
motel-backup status
motel-backup restore demo --to /root/demo-restored
motel-backup restore demo"
[ "$(printf '%s\n' "${docs[@]}")" = "$want" ] \
	|| die "the guide's smoke block is not the seven lines this stage runs and checks; it holds: $(printf '%s | ' "${docs[@]}")"
for line in "${docs[@]}"; do
	echo "docs/backups.md: $line"
	case "$line" in
	"motel-backup setup "*)
		converse "$line" "$P_KEY" "$S3KEY" "$P_SECRET" "$S3SECRET" "$P_STORED" stored \
			|| die "setup as root failed: $(cat "$OUT")"
		grep -q "account motel-backup: present" "$OUT" || die "setup did not find the package's account: $(cat "$OUT")"
		grep -q "repository ready: $S3REPO (new, id " "$OUT" || die "setup did not make the repository: $(cat "$OUT")"
		grep -q "^next: the first backup runs within the hour and ten minutes (systemctl list-timers motel-backup.timer)" "$OUT" \
			|| die "setup's closing line does not name the timer: $(grep next: "$OUT")"
		grep -q "$S3SECRET" "$OUT" && die "the storage secret was echoed at the terminal" || true
		[ "$(stat -c '%U %a' "$CRED")" = "root 600" ] || die "$CRED is '$(stat -c '%U %a' "$CRED")', want 'root 600'"
		CRED_SHA=$(sha256sum "$CRED" | cut -d' ' -f1)
		expect_backup_state "after setup" enabled active "$CRED_SHA"
		# The guide's next claim: the first backup comes from the timer,
		# within the hour and ten minutes. Not waited an hour for: a
		# drop-in makes the timer a minute's, one firing is watched, the
		# drop-in goes. The service it starts is the shipped one.
		# A persistent timer writes its stamp at its first activation —
		# postinstall's start — and fires nothing; the service has not
		# run yet, and what will say it ran is its main process's start
		# time (LastTriggerUSec shows the stamp's time after a restart,
		# which is no firing) [M61].
		[ -f /var/lib/systemd/timers/stamp-motel-backup.timer ] || die "the timer's first activation at install wrote no stamp"
		started0=$(systemctl show -p ExecMainStartTimestampMonotonic --value motel-backup.service)
		[ "$started0" = 0 ] || die "the backup service ran before anything asked it to (main process started at $started0)"
		mkdir -p /run/systemd/system/motel-backup.timer.d
		printf '[Timer]\nOnCalendar=\nOnCalendar=*-*-* *:*:00\nRandomizedDelaySec=0\n' >/run/systemd/system/motel-backup.timer.d/10-smoke.conf
		systemctl daemon-reload
		systemctl restart motel-backup.timer
		i=0
		until [ "$(systemctl show -p ExecMainStartTimestampMonotonic --value motel-backup.service)" != "$started0" ] || [ "$i" -ge 150 ]; do
			i=$((i + 1))
			sleep 0.5
		done
		[ "$i" -lt 150 ] || die "the hourly timer, made a minute's, did not start the service within 75s: $(systemctl show -p LastTriggerUSec,NextElapseUSecRealtime,ActiveState motel-backup.timer | tr '\n' ' '); $(systemctl show -p ActiveState,Result,ConditionResult motel-backup.service | tr '\n' ' ')"
		i=0
		until [ "$(systemctl show -p ActiveState --value motel-backup.service)" = inactive ] || [ "$i" -ge 240 ]; do
			i=$((i + 1))
			sleep 0.5
		done
		rm -rf /run/systemd/system/motel-backup.timer.d
		systemctl daemon-reload
		systemctl restart motel-backup.timer
		[ "$(systemctl show -p Result --value motel-backup.service)" = success ] \
			|| die "the run the timer fired ended '$(systemctl show -p Result --value motel-backup.service)': $(journalctl -u motel-backup.service --no-pager | tail -20)"
		[ -f /var/lib/motel-backup/status.json ] || die "the timer's run wrote no record"
		echo "the timer fired ($((i / 2))s after the minute), and its run ended success under the unit's hardening"
		;;
	"motel-backup restore demo")
		printf 'demo\n' | sh -c "$line" >"$OUT" 2>&1 || die "the restore into place failed: $(cat "$OUT")"
		grep -q "demo: backed up first: snapshot [0-9a-f]\{8\} (restore --snapshot" "$OUT" || die "the restore into place did not back the app up first: $(cat "$OUT")"
		grep -q "demo: restored from snapshot" "$OUT" || die "the restore into place restored nothing: $(cat "$OUT")"
		;;
	"motel-backup validate "*)
		as_admin "$line" >"$OUT" 2>&1 || die "validate with no privilege exits $?: $(cat "$OUT")"
		grep -q "^a run would back up demo, under " "$OUT" || die "validate with no privilege did not see demo's declaration: $(cat "$OUT")"
		;;
	"motel-backup status")
		as_admin "$line" >"$OUT" 2>&1 || die "status with no privilege exits $?: $(cat "$OUT")"
		grep -q "^demo: ok: " "$OUT" || die "status does not say demo is ok: $(cat "$OUT")"
		grep -q "restore last proven" "$OUT" || die "status does not say demo's restore is proven: $(cat "$OUT")"
		;;
	*)
		sh -c "$line" >"$OUT" 2>&1 || die "'$line' as root exits $?: $(cat "$OUT")"
		;;
	esac
done
[ -f /root/demo-restored/uploads/a.png ] && [ "$(sqlite3 /root/demo-restored/app.db 'select count(*) from t')" = 2 ] \
	|| die "the restore --to put nothing there: $(find /root/demo-restored 2>&1 | head)"
# Not in the guide's lines, and true of them: the --to rule, and the
# pre-restore snapshot the restore into place named.
motel-backup restore demo --to /tmp/demo-restored </dev/null >"$OUT" 2>&1 && die "a restore --to /tmp was accepted" || true
grep -q "not /tmp — /root, /srv, /var/backups, or a root-owned directory of your own" "$OUT" || die "the --to rule's words are missing: $(cat "$OUT")"
[ ! -e /tmp/demo-restored ] || die "a refused --to made its directory"
snaps=$(systemd-run --quiet --pipe --wait --collect -p User=motel-backup -p EnvironmentFile=$CRED -p CacheDirectory=motel-backup -E RESTIC_CACHE_DIR=/var/cache/motel-backup -E HOME=/nonexistent /usr/bin/restic snapshots --no-lock --json --tag pre-restore 2>/dev/null | grep -o '"short_id"' | wc -l)
[ "$snaps" = 1 ] || die "want one pre-restore snapshot, the repository holds $snaps"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "the app is not served after the backups"
echo "the guide's lines ran as written, as root, and validate and status with no privilege: validate, reload, setup, a run, status, a restore --to, a restore into place with its pre-restore snapshot"

stage "stage 3: reinstall — upgrade path, the box's config kept, app survival"
pid_before=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ -n "$pid_before" ] || die "status missing pid: $status"
# The account already exists by definition on a reinstall, which is the
# path that used to set the group only when it CREATED the user. Strip
# the membership first, the way a motel account made by an admin (or
# another package) would arrive: postinstall must establish it rather
# than assume it.
# postinstall's useradd makes motel the account's PRIMARY group, so
# it cannot simply be removed; move the account to another primary
# group instead, which is exactly the shape a pre-existing account has.
groupadd --system smoketest-other 2>/dev/null || true
usermod -g smoketest-other motel \
	|| die "could not move the motel account off its primary group; the reinstall assertion below would be vacuous"
id -nG motel | tr ' ' '\n' | grep -qx motel \
	&& die "the motel account is still in the motel group; the reinstall assertion below would be vacuous"
# The box applier across the upgrade: an administrator's disable is kept
# (deb-systemd-helper's was-enabled), and a tree that drifted is put
# back by postinstall's systemd-tmpfiles.
systemctl disable --now motel-box-apply.path
chmod 0777 "$B/in"
# Five digits: GNU chmod keeps a directory's setgid bit for a
# four-digit mode, and the bit is what tmpfiles.d must put back.
chmod 00755 "$B"
chown motel "$B/out"
cp "$CONFIG" /tmp/Caddyfile.configured
# stdin from /dev/null and no --force-conf*: a question would end at
# EOF and fail the install.
dpkg -i "$deb" </dev/null >/tmp/reinstall.out 2>/tmp/reinstall.err \
	|| { cat /tmp/reinstall.out /tmp/reinstall.err; die "reinstall (dpkg -i) failed"; }
cat /tmp/reinstall.out /tmp/reinstall.err
# moteld is enabled and running: the start hint would be noise, and a
# restart that went fine must not read as one that failed.
grep -q 'enable --now' /tmp/reinstall.out /tmp/reinstall.err \
	&& die "reinstall over an enabled moteld printed the 'enable --now' start hint"
grep -q 'journalctl -u moteld' /tmp/reinstall.err \
	&& die "reinstall printed the failed-restart hint although the restart succeeded"
grep -qx 'moteld restarted.' /tmp/reinstall.out \
	|| die "reinstall over a running moteld did not say it restarted"
# Stage 2c ran setup: the credential file is in place, so the backups
# are set up, and the notice must say so rather than send the operator
# back to setup (#403).
grep -qx 'Backups: the hourly timer and the Sunday restore drill are enabled and set up; motel-backup status says where they stand' /tmp/reinstall.out \
	|| die "reinstall over a set-up box did not say the backups are set up: $(grep Backups: /tmp/reinstall.out /tmp/reinstall.err || echo 'no Backups: line')"
tr -d '\r' </tmp/reinstall.out | grep -qx "  motel-backup setup <repository>" \
	&& die "reinstall over a set-up box told the operator to run setup again" || true
id -nG motel | tr ' ' '\n' | grep -qx motel \
	|| die "reinstall did not restore the motel user's group membership: the package's group-owned directories would be unreachable"
cmp -s /tmp/Caddyfile.configured "$CONFIG" \
	|| die "reinstall changed the configured $CONFIG"
[ "$(cat "$RECORD")" = "$(sha "$STARTER")" ] \
	|| die "reinstall over a configured box changed the record"
[ "$(systemctl is-enabled motel-box-apply.path)" = disabled ] \
	|| die "an upgrade re-enabled the box applier's path unit an administrator disabled"
systemctl is-active --quiet motel-box-apply.path \
	&& die "an upgrade started the box applier's path unit an administrator disabled"
for d in "$B 2750 root:motel" "$B/in 770 root:motel" "$B/out 2750 root:motel"; do
	read -r p want_mode want_owner <<<"$d"
	got=$(stat -c '%a %U:%G' "$p")
	[ "$got" = "$want_mode $want_owner" ] || die "$p is '$got' after the upgrade, want '$want_mode $want_owner': postinstall's systemd-tmpfiles did not put it back"
done
systemctl enable --now motel-box-apply.path
# The upgrade's postinstall ran moteld box key again: the key is the
# one the install made, never replaced.
[ "$(moteld box key 2>/dev/null)" = "$box_key" ] || die "an upgrade replaced the box's key for secrets"
[ "$(cat /etc/motel/age.pub)" = "$box_key" ] || die "an upgrade rewrote age.pub with another key"
systemctl is-active --quiet moteld \
	|| die "service not active after reinstall — an upgrade must not leave the server down (preremove stop / missing postinstall restart)"
id motel >/dev/null || die "motel user gone after reinstall (postinstall not idempotent)"
# postinstall's try-restart swapped moteld onto the new binary. The
# deployed app is a unit under the user manager, not a child of
# moteld: it must still be the SAME process, reattached from
# state.json, and serving throughout.
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "webhook not back within 30s of the upgrade restart"
	sleep 1
done
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
pid_after=$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')
[ "$pid_after" = "$pid_before" ] \
	|| die "app pid changed across the upgrade restart ($pid_before -> $pid_after): reattach failed, the app was relaunched (or is gone): $status"
case "$status" in *'"running":true'*) : ;; *) die "reattached app not running: $status" ;; esac
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "reattached app not served"
echo "reinstall preserved config and user; moteld restarted and reattached to the running app (pid $pid_after)"
# The table's upgrade rows: as installed, the timers stay enabled and
# running and the credential file is not touched; disabled by the
# administrator, an upgrade leaves them disabled (was-enabled is false,
# and deb-systemd-invoke starts nothing disabled); enabled again, an
# upgrade keeps them so.
expect_backup_state "after an upgrade" enabled active "$CRED_SHA"
systemctl disable --now motel-backup.timer motel-backup-drill.timer
expect_backup_state "disabled by the administrator" disabled inactive "$CRED_SHA"
dpkg -i "$deb" >/dev/null
expect_backup_state "after an upgrade, disabled before it" disabled inactive "$CRED_SHA"
systemctl enable --now motel-backup.timer motel-backup-drill.timer
dpkg -i "$deb" >/dev/null
expect_backup_state "after an upgrade, enabled before it" enabled active "$CRED_SHA"
# An administrator's enable --runtime (enabled until the next boot) is
# enabled for what runs today: the upgrade keeps it (the helper has no
# persistent enable to restore), and the Backups: line says set up, not
# disabled (Copilot on #404).
systemctl disable --now motel-backup.timer motel-backup-drill.timer
systemctl enable --runtime --now motel-backup.timer motel-backup-drill.timer
dpkg -i "$deb" >/tmp/upgrade-runtime.log 2>&1 || { cat /tmp/upgrade-runtime.log; die "the upgrade with the timers enabled --runtime failed"; }
expect_backup_state "after an upgrade, enabled --runtime before it" enabled-runtime active "$CRED_SHA"
grep -qx 'Backups: the hourly timer and the Sunday restore drill are enabled and set up; motel-backup status says where they stand' /tmp/upgrade-runtime.log \
	|| die "the upgrade over timers enabled --runtime did not call them enabled and set up: $(grep Backups: /tmp/upgrade-runtime.log || echo 'no Backups: line')"
systemctl disable --now motel-backup.timer motel-backup-drill.timer
systemctl enable --now motel-backup.timer motel-backup-drill.timer
expect_backup_state "enabled again after the --runtime staging" enabled active "$CRED_SHA"
# The timers as dh_installsystemd has them (the owner, 2026-09-27): an
# upgrade restarts a timer that is enabled, one an administrator
# stopped among them — the README says so, and to disable it to keep
# it off — and leaves one the administrator masked masked.
systemctl stop motel-backup.timer
systemctl mask --now motel-backup-drill.timer
dpkg -i "$deb" >/tmp/upgrade-mask.log 2>&1 || { cat /tmp/upgrade-mask.log; die "the upgrade with one timer stopped and one masked failed"; }
# The Backups: line says of each timer what is so, and does not call a
# box with the drill masked set up for both (Copilot on #404).
grep -qx 'Backups: motel-backup.timer is enabled and motel-backup-drill.timer is masked, as they were left; systemctl enable --now turns on one that is disabled, after systemctl unmask on one that is masked' /tmp/upgrade-mask.log \
	|| die "the upgrade over a masked drill timer did not name each timer's state: $(grep Backups: /tmp/upgrade-mask.log || echo 'no Backups: line')"
grep -q "enabled and set up" /tmp/upgrade-mask.log \
	&& die "the upgrade over a masked drill timer called both timers enabled and set up" || true
grep -qx '  and the backups are set up; motel-backup status says where they stand' /tmp/upgrade-mask.log \
	|| die "the upgrade over a masked drill timer did not say the backups are set up: $(grep -A1 Backups: /tmp/upgrade-mask.log)"
# The same box before setup: the line keeps the setup pointer the one
# enabled timer needs (a review of #404).
# This stage now restarts moteld more times inside ten seconds than
# any box does (each dpkg -i is one), and systemd's default start
# limit, five in ten seconds, would hold the next one and leave moteld
# failed: the counter is reset first, which is the test's doing, not
# the package's.
systemctl reset-failed moteld
mv "$CRED" "$CRED.aside"
dpkg -i "$deb" >/tmp/upgrade-mask-nofile.log 2>&1 || { cat /tmp/upgrade-mask-nofile.log; mv "$CRED.aside" "$CRED"; die "the upgrade with one timer masked and no credential file failed"; }
mv "$CRED.aside" "$CRED"
grep -qx '  and the one that is enabled runs nothing until: motel-backup setup <repository>' /tmp/upgrade-mask-nofile.log \
	|| die "the upgrade over a masked drill timer with no credential file did not point at setup: $(grep -A1 Backups: /tmp/upgrade-mask-nofile.log)"
# Both masked: named as masked, with unmask before enable, not called
# disabled with an enable --now that a mask refuses (a review of #404).
systemctl mask --now motel-backup.timer
dpkg -i "$deb" >/tmp/upgrade-masked-both.log 2>&1 || { cat /tmp/upgrade-masked-both.log; die "the upgrade with both timers masked failed"; }
grep -qx 'Backups: motel-backup.timer is masked and motel-backup-drill.timer is masked, as they were left; systemctl enable --now motel-backup.timer motel-backup-drill.timer turns them on, after systemctl unmask on one that is masked' /tmp/upgrade-masked-both.log \
	|| die "the upgrade over two masked timers did not name them masked: $(grep Backups: /tmp/upgrade-masked-both.log || echo 'no Backups: line')"
systemctl unmask motel-backup.timer
systemctl reset-failed motel-backup.timer 2>/dev/null || true
systemctl start motel-backup.timer
[ "$(systemctl is-active motel-backup.timer || true)" = active ] \
	|| die "after an upgrade, a timer stopped and left enabled is $(systemctl is-active motel-backup.timer || true): dh_installsystemd restarts it"
# (Debian's helper asks the manager to restart it all the same, which
# a mask refuses: masked, and failed rather than inactive, and not
# running either way.)
[ "$(systemctl is-enabled motel-backup-drill.timer || true)" = masked ] && [ "$(systemctl is-active motel-backup-drill.timer || true)" != active ] \
	|| die "after an upgrade, the administrator's mask of motel-backup-drill.timer is $(systemctl is-enabled motel-backup-drill.timer || true), $(systemctl is-active motel-backup-drill.timer || true)"
systemctl unmask motel-backup-drill.timer
systemctl reset-failed motel-backup-drill.timer 2>/dev/null || true
systemctl start motel-backup-drill.timer
expect_backup_state "after an upgrade, one timer stopped and one masked, and the mask taken off" enabled active "$CRED_SHA"
systemctl is-active --quiet moteld || die "moteld not active after the upgrade cycle"
# A user directory that does not answer: postinstall's lookups of the
# accounts are bounded, and one that does not answer is "could not
# tell" — never "absent", which would make a local account beside the
# directory's — and the configure completes (Copilot on #155: dpkg
# hung). Staged with a getent ahead of the real one on dpkg's PATH that
# never answers for the backup account.
# It ignores the SIGTERM a timeout sends first, as a lookup stuck in
# the kernel does: only the kill that follows ends it.
printf '#!/bin/sh\ncase "$*" in *motel-backup*) trap "" TERM; sleep 600; exit 0 ;; esac\nexec /usr/bin/getent "$@"\n' >/usr/local/sbin/getent
chmod 755 /usr/local/sbin/getent
t0=$(date +%s)
timeout 300 dpkg -i "$deb" >/tmp/upgrade-nss.log 2>&1 || { cat /tmp/upgrade-nss.log; rm -f /usr/local/sbin/getent; die "the upgrade with a directory that does not answer did not complete within 300s"; }
took=$(($(date +%s) - t0))
rm -f /usr/local/sbin/getent
[ "$took" -le 150 ] || die "the upgrade with a directory that does not answer took ${took}s"
grep -q "motel: whether the motel-backup account exists could not be told (the user directory did not answer within 30s)" /tmp/upgrade-nss.log \
	|| die "postinstall did not say that the directory did not answer: $(cat /tmp/upgrade-nss.log)"
# The credential file is in place and the account could not be told:
# the Backups: line says neither set up nor run setup (Copilot on #404).
grep -qx "Backups: the hourly timer and the Sunday restore drill are enabled, and the credential file is in place; the runs wait on the motel-backup account (above): motel-backup account says when it is right" /tmp/upgrade-nss.log \
	|| die "the upgrade with a directory that does not answer did not say the runs wait on the account: $(grep Backups: /tmp/upgrade-nss.log || echo 'no Backups: line')"
grep -q "enabled and set up" /tmp/upgrade-nss.log \
	&& die "the upgrade with a directory that does not answer called the backups set up" || true
[ "$(getent passwd motel-backup | wc -l)" = 1 ] || die "a second motel-backup account was made: $(getent passwd | grep motel-backup)"
dpkg -s moteld | grep -q '^Status: install ok installed$' || die "moteld is not configured after the upgrade with a directory that does not answer"
echo "a directory that does not answer: postinstall said so within ${took}s, made no account, and the configure completed"
# An account of the name that motel did not make (its comment is not
# made-by-motel): said on stderr, and the Backups: line sends the
# operator to motel-backup account, not to setup, which refuses it too
# (a review of #404).
usermod --comment someone-elses motel-backup
dpkg -i "$deb" >/tmp/upgrade-refused.log 2>&1 || { cat /tmp/upgrade-refused.log; usermod --comment made-by-motel motel-backup; die "the upgrade with an account motel did not make failed: a warning must not fail an install"; }
usermod --comment made-by-motel motel-backup
grep -q "refuse that account until it is put right" /tmp/upgrade-refused.log \
	|| die "postinstall did not warn of the account motel did not make: $(cat /tmp/upgrade-refused.log)"
grep -qx "Backups: the hourly timer and the Sunday restore drill are enabled, and the credential file is in place; the runs wait on the motel-backup account (above): motel-backup account says when it is right" /tmp/upgrade-refused.log \
	|| die "the upgrade with a refused account did not say the runs wait on it: $(grep Backups: /tmp/upgrade-refused.log || echo 'no Backups: line')"
grep -q "enabled and set up" /tmp/upgrade-refused.log \
	&& die "the upgrade with a refused account called the backups set up" || true
motel-backup account >/dev/null || die "the account is not accepted again after its comment was put back: $(motel-backup account 2>&1)"
# An upgrade from a release that had no backup timers — every box that
# first gets this feature: the helper has no state for them, and they
# are enabled and started as on an install, not left stopped until a
# reboot (the owner's review of #155). Staged as that release leaves a
# box: no enable state, no stamps, the timers stopped.
systemctl disable --now motel-backup.timer motel-backup-drill.timer >/dev/null 2>&1
rm -f /var/lib/systemd/deb-systemd-helper-enabled/motel-backup*.dsh-also /var/lib/systemd/deb-systemd-helper-enabled/timers.target.wants/motel-backup* /var/lib/systemd/timers/stamp-motel-backup*
systemctl reset-failed $TIMERS 2>/dev/null || true
dpkg -i "$deb" >/tmp/upgrade-first.log 2>&1 || { cat /tmp/upgrade-first.log; die "the upgrade from a release without the timers failed"; }
expect_backup_state "after an upgrade from a release without the timers" enabled active "$CRED_SHA"
# An upgrade leaves a backup under way alone (the owner, 2026-09-27):
# its upload, which is restic's, finishes, and the run goes on as the
# program it was started as — here the same program, so its helpers
# work for it. (A helper of another version refusing is the units
# suite's row and the engine's table.) Staged with a run held at its
# upload, under a service and from a shell.
# hold_upload <how a run is started, in the background>: a run held at
# its upload — $up its upload unit, $rpid that unit's restic, stopped.
hold_upload() {
	# The rows upgrade the package faster than anyone does, and each
	# upgrade restarts the timers: past five starts in ten seconds the
	# manager refuses the next (start-limit-hit). Counted from here.
	# shellcheck disable=SC2086 # two unit names
	systemctl reset-failed $TIMERS 2>/dev/null || true
	su -s /bin/sh motel -c 'head -c 50000000 /dev/urandom >/var/lib/liveswap/demo/shared/uploads/big.bin'
	eval "$1"
	i=0
	until up=$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_upload_*' | awk '{print $1}' | head -1) && [ -n "$up" ]; do
		i=$((i + 1))
		[ "$i" -ge 300 ] && die "no upload unit appeared ($1): the row would prove nothing"
		sleep 0.1
	done
	rpid=$(systemctl show -p ExecMainPID --value "$up")
	[ -n "$rpid" ] && [ "$rpid" != 0 ] && kill -STOP "$rpid" || die "could not hold the upload's restic (pid '$rpid')"
}
# survived <step> <log>: the run held at its upload is still under way
# after the upgrade, said nothing of, and ends ok once let go.
survived() {
	[ "$(systemctl is-active "$up" || true)" = activating ] && kill -0 "$rpid" 2>/dev/null \
		|| die "$1: the upgrade ended the upload under way ($up is $(systemctl is-active "$up" || true))"
	# An upgrade stops and sweeps nothing: none of what a stop or a
	# sweep says is in its output (the owner's review of #155: this
	# looked for words no script prints any more).
	grep -q "not swept\|motel-backup sweep\|Stopping\|Stopped" "$2" && die "$1: the upgrade stopped or swept something of the backups: $(cat "$2")" || true
	kill -CONT "$rpid"
}
hold_upload "systemctl start --no-block motel-backup.service"
dpkg -i "$deb" >/tmp/upgrade-run.log 2>&1 || { cat /tmp/upgrade-run.log; die "the upgrade with a run under way failed"; }
survived "an upgrade with a run under way" /tmp/upgrade-run.log
i=0
until [ "$(systemctl show -p ActiveState --value motel-backup.service)" != activating ] || [ "$i" -ge 240 ]; do
	i=$((i + 1))
	sleep 0.5
done
# Ended, and not still under way: a hung run keeps the Result= of the
# run before it (Copilot on #155).
[ "$(systemctl show -p ActiveState --value motel-backup.service)" != activating ] \
	|| die "the run under way at the upgrade had not ended 120s after it was let go: $(journalctl -u motel-backup.service --no-pager | tail -10)"
[ "$(systemctl show -p Result --value motel-backup.service)" = success ] \
	|| die "the run under way at the upgrade ended '$(systemctl show -p Result --value motel-backup.service)': $(journalctl -u motel-backup.service --no-pager | tail -10)"
grep -q '"demo": {' /var/lib/motel-backup/status.json && tr -d '\n' </var/lib/motel-backup/status.json | grep -q '"demo": { *"class": "ok"' \
	|| die "the run under way at the upgrade did not back demo up: $(tr -d '\n' </var/lib/motel-backup/status.json | cut -c1-400)"
su -s /bin/sh motel -c 'rm -f /var/lib/liveswap/demo/shared/uploads/big.bin'
expect_backup_state "after an upgrade with a run under way" enabled active "$CRED_SHA"
echo "an upgrade left the run under way alone: its upload finished, and it ended ok"
# The same from a shell.
hold_upload "setsid motel-backup run >/tmp/shell-run.log 2>&1 &"
shell_pid=$(sed -n 's/^pid \([0-9][0-9]*\),.*/\1/p' /run/motel-backup/lock)
[ -n "$shell_pid" ] && kill -0 "$shell_pid" || die "the run from a shell is not the lock's holder ('$(cat /run/motel-backup/lock)'): the row proves nothing"
dpkg -i "$deb" >/tmp/upgrade-shell.log 2>&1 || { cat /tmp/upgrade-shell.log; die "the upgrade with a command under way from a shell failed"; }
survived "an upgrade with a command under way from a shell" /tmp/upgrade-shell.log
i=0
while kill -0 "$shell_pid" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 240 ] && die "the run from a shell did not end within 120s of its upload going on: $(cat /tmp/shell-run.log)"
	sleep 0.5
done
grep -q "^demo: ok" /tmp/shell-run.log || die "the run from a shell, under way at the upgrade, did not back demo up: $(cat /tmp/shell-run.log)"
su -s /bin/sh motel -c 'rm -f /var/lib/liveswap/demo/shared/uploads/big.bin'
expect_backup_state "after an upgrade with a command under way from a shell" enabled active "$CRED_SHA"
echo "a command from a shell, under way at the upgrade: left alone, and it ended ok"
# And under a policy-rc.d, which forbids a package every start and
# stop: an upgrade stops nothing of the backups, so the policy leaves
# the timers as they were, running.
printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
chmod 755 /usr/sbin/policy-rc.d
dpkg -i "$deb" >/tmp/upgrade-policy.log 2>&1 || { cat /tmp/upgrade-policy.log; rm -f /usr/sbin/policy-rc.d; die "the upgrade under a policy-rc.d failed"; }
rm -f /usr/sbin/policy-rc.d
expect_backup_state "after an upgrade under a policy-rc.d" enabled active "$CRED_SHA"
echo "an upgrade under a policy-rc.d left the timers running"
# The account is every run's to check, and an upgrade says so in
# setup's own words: motel uses only an account it made, marked as
# its own; with the mark gone — one made by someone else stands in —
# it is warned of at the upgrade, refused by a run, and accepted again
# once it is motel's.
usermod --comment "made by someone else" motel-backup
dpkg -i "$deb" >/tmp/upgrade-mark.log 2>&1 || { cat /tmp/upgrade-mark.log; die "the upgrade with an account motel did not make failed: a warning must not fail an install"; }
grep -q "was not made by motel" /tmp/upgrade-mark.log && grep -q "refuse that account" /tmp/upgrade-mark.log \
	|| die "postinstall did not warn of an account motel did not make: $(cat /tmp/upgrade-mark.log)"
systemctl start motel-backup.service && die "a run with an account motel did not make exited 0" || true
journalctl -u motel-backup.service --no-pager | grep -q "was not made by motel" \
	|| die "the run did not say why it refused: $(journalctl -u motel-backup.service --no-pager | tail -5)"
usermod --comment made-by-motel motel-backup
systemctl reset-failed motel-backup.service 2>/dev/null || true
motel-backup account >/dev/null || die "motel's account is not accepted: $(motel-backup account 2>&1)"
systemctl start motel-backup.service || die "a run after the account was motel's again failed: $(journalctl -u motel-backup.service --no-pager | tail -20)"
echo "an account motel did not make: warned of at the upgrade in setup's words, refused by a run, accepted once it is motel's"

stage "stage 3b: prerm deconfigure is not removal"
# dpkg runs `prerm deconfigure` when a dependency (dbus, libpam-systemd)
# is swapped out mid-dist-upgrade; the package stays installed and no
# later maintainer script re-enables the service. Calling the installed
# script directly is the only way to reach that path without a real
# conflicting package.
prerm=/var/lib/dpkg/info/moteld.prerm
[ -x "$prerm" ] || die "installed prerm not found at $prerm"
main_before=$(systemctl show -P MainPID moteld)
"$prerm" deconfigure in-favour smoketest-dep 1.0 removing dbus 1.0 \
	|| die "prerm deconfigure exited non-zero"
systemctl is-active --quiet moteld || die "prerm deconfigure stopped moteld"
systemctl is-enabled --quiet moteld || die "prerm deconfigure disabled moteld"
[ "$(systemctl show -P MainPID moteld)" = "$main_before" ] || die "prerm deconfigure restarted moteld"
systemctl is-active --quiet "user@$uid.service" || die "prerm deconfigure stopped the user manager (and every app)"
[ -e /etc/systemd/system/moteld.service.d/10-user-manager.conf ] || die "prerm deconfigure removed the user-manager drop-in"
[ -e "/etc/systemd/system/user@$uid.service.d/10-motel.conf" ] || die "prerm deconfigure removed the user@ limits drop-in"
[ -e /var/lib/systemd/linger/motel ] || die "prerm deconfigure disabled lingering"
kill -0 "$pid_after" 2>/dev/null || die "deployed app (pid $pid_after) did not survive prerm deconfigure"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after prerm deconfigure"
echo "prerm deconfigure left the service, the user manager and the app (pid $pid_after) running"

stage "stage 3c: an upgrade restart the new version refuses is reported; an upgrade over the failed unit"
# An upgrade whose config the new binary refuses stops the old one and
# fails to start the new: the site is down. postinstall must say so on
# stderr — and still exit 0, or the package is left half-configured.
# A subdirective no version accepts stands in for one an upgrade stops
# accepting; reinstalling the same .deb runs the same try-restart.
cp /etc/motel/Caddyfile /tmp/Caddyfile.good
sed -i '/^[[:space:]]*liveswap {$/a smoke_retired_option on' /etc/motel/Caddyfile
moteld adapt --config /etc/motel/Caddyfile >/dev/null 2>&1 \
	&& die "the broken Caddyfile adapts; the failed-restart assertions below would be vacuous"
dpkg -i "$deb" >/tmp/refused.out 2>/tmp/refused.err \
	|| { cat /tmp/refused.out /tmp/refused.err; die "dpkg -i exited non-zero over a refused config: a failed restart must not fail the package"; }
cat /tmp/refused.out /tmp/refused.err
[ "$(dpkg-query -W -f='${Status}' moteld)" = "install ok installed" ] \
	|| die "package state is '$(dpkg-query -W -f='${Status}' moteld)' after a refused restart, want 'install ok installed'"
systemctl is-active --quiet moteld \
	&& die "moteld is active after a restart over a refused config; this stage tests nothing"
grep -q 'journalctl -u moteld -e' /tmp/refused.err \
	|| die "a failed upgrade restart printed no journalctl hint on stderr (postinstall swallowed it)"
grep -q 'systemctl status moteld' /tmp/refused.err \
	|| die "a failed upgrade restart printed no systemctl status hint on stderr"
grep -qx '  then fix it and run: systemctl restart moteld' /tmp/refused.err \
	|| die "a failed upgrade restart did not give the restart line, with no sudo"
grep -q 'enable --now' /tmp/refused.out /tmp/refused.err \
	&& die "a failed upgrade restart printed the 'enable --now' start hint, as if all were well"
grep -q 'restarted\.' /tmp/refused.out \
	&& die "a failed upgrade restart claimed moteld restarted"
kill -0 "$pid_after" 2>/dev/null || die "deployed app (pid $pid_after) did not survive moteld's refused start"
# The box applier's path unit, enabled again after stage 3, is restarted
# by the upgrade whatever became of moteld.
[ "$(systemctl is-enabled motel-box-apply.path)" = enabled ] && systemctl is-active --quiet motel-box-apply.path \
	|| die "the box applier's path unit is not enabled and active after an upgrade: $(systemctl show -p ActiveState,Result motel-box-apply.path | tr '\n' ' ')"
# Fix the config, as the operator would. An upgrade now, over the unit
# the refused restart left failed, starts nothing (try-restart), though
# the config would load, and says how to start it.
cp /tmp/Caddyfile.good /etc/motel/Caddyfile
st=$(systemctl show -P ActiveState moteld)
[ "$st" = failed ] || die "moteld is '$st' after the refused restart, not failed: the upgrade below would prove nothing"
dpkg -i "$deb" </dev/null >/tmp/stopped.out 2>&1 || { cat /tmp/stopped.out; die "dpkg -i over a failed moteld exited non-zero"; }
cat /tmp/stopped.out
st=$(systemctl show -P ActiveState moteld)
[ "$st" = failed ] || die "an upgrade over a failed moteld left it '$st', not failed: it started it, or cleared the failure"
grep -A1 -x 'moteld is enabled but not running. Start it with:' /tmp/stopped.out | grep -qx '  systemctl start moteld' \
	|| die "an upgrade over a failed moteld did not give the start line, with no sudo"
grep -q -e '^moteld restarted\.$' -e 'restarting onto the new version FAILED' /tmp/stopped.out \
	&& die "an upgrade over a failed moteld also said it restarted, or that the restart failed"
# Recover the way the failed restart's hint says, so stage 4 starts
# from a serving box. reset-failed first: stages 3 to 3c started the
# unit often enough in a few seconds to reach systemd's start limit
# (5 in 10 s), which an operator fixing the config by hand would have
# long outlived.
systemctl reset-failed moteld
timeout 300 systemctl restart moteld || die "moteld did not start again on the restored config"
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
	i=$((i + 1))
	[ "$i" -ge 30 ] && die "webhook not back within 30s of the recovery restart"
	sleep 1
done
status=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK")
[ "$(printf '%s' "$status" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p')" = "$pid_after" ] \
	|| die "app pid changed across the refused restart and recovery: reattach failed: $status"
curl -fsS --max-time 5 "$PROXY/" | grep -q "hello smoke" || die "app not served after recovery"
echo "refused restart: dpkg exit 0, hint on stderr, no start hint; a second upgrade started nothing and gave the start hint; recovered and reattached to pid $pid_after"

stage "stage 4: removal"
# What remove has to take with it, staged: the persistent timers'
# stamps, eight days old — a reinstall beside the credential file
# would read them as a run and a drill missed, and catch up [M68] —
# and a bind mount of an app's data that a killed run left, which after
# a remove no next run sweeps.
for u in $TIMERS; do
	[ -f "/var/lib/systemd/timers/stamp-$u" ] || die "no stamp for $u before the remove: the row would prove nothing"
	touch -d '8 days ago' "/var/lib/systemd/timers/stamp-$u"
done
# As a run binds it: recursively, with the disks an operator has
# mounted inside the app's data — one of a name with a space in it,
# which mountinfo escapes. Through a bind taken away, each disk stays
# mounted where the operator put it.
su -s /bin/sh motel -c 'mkdir -p /var/lib/liveswap/demo/shared/uploads/disk "/var/lib/liveswap/demo/shared/uploads/my disk"'
mkdir -p /run/motel-backup/dead00000001/mount-1 /tmp/disk "/tmp/my disk"
echo on-the-disk >/tmp/disk/kept.txt
echo on-my-disk >"/tmp/my disk/kept.txt"
mount --bind /tmp/disk /var/lib/liveswap/demo/shared/uploads/disk || die "could not stage a disk inside the app's data"
mount --bind "/tmp/my disk" "/var/lib/liveswap/demo/shared/uploads/my disk" || die "could not stage a disk of a name with a space inside the app's data"
disks_mounted() { # <when>
	mountpoint -q /var/lib/liveswap/demo/shared/uploads/disk && mountpoint -q "/var/lib/liveswap/demo/shared/uploads/my disk" \
		&& [ -f /var/lib/liveswap/demo/shared/uploads/disk/kept.txt ] && [ -f "/var/lib/liveswap/demo/shared/uploads/my disk/kept.txt" ] \
		|| die "$1: a disk inside the app's data was unmounted from under the app: $(grep liveswap/demo /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')"
}
mount --rbind /var/lib/liveswap/demo/shared /run/motel-backup/dead00000001/mount-1 || die "could not stage a leftover bind mount"
[ -f /run/motel-backup/dead00000001/mount-1/app.db ] && [ -f "/run/motel-backup/dead00000001/mount-1/uploads/my disk/kept.txt" ] \
	|| die "the staged mount does not show the app's data: the remove row would prove nothing"
disks_mounted "before the remove"
apt-get remove -y moteld >/tmp/remove.log 2>&1 || { cat /tmp/remove.log; die "apt-get remove failed"; }
cat /tmp/remove.log
disks_mounted "after remove"
grep -q "unmount .*yourself" /tmp/remove.log \
	&& die "remove told the administrator to unmount what it had unmounted: $(grep yourself /tmp/remove.log)" || true
for u in $TIMERS; do
	[ ! -e "/var/lib/systemd/timers/stamp-$u" ] || die "remove left the stamp of $u: a reinstall would catch up a run nobody missed"
done
grep -q " /run/motel-backup/" /proc/self/mountinfo \
	&& die "remove left a killed run's mount: $(grep ' /run/motel-backup/' /proc/self/mountinfo)" || true
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] \
	|| die "remove took the app's data with the mount"
[ ! -e /run/motel-backup/dead00000001 ] || die "remove left a killed run's directory, which the engine's sweep takes: $(ls -la /run/motel-backup/dead00000001)"
echo "remove took the timers' stamps, and the engine's sweep a killed run's mount and its directory, and nothing of the app's"
systemctl is-active --quiet moteld && die "service still active after remove (preremove did not stop it)" || true
[ ! -e /usr/bin/moteld ] || die "/usr/bin/moteld still present after remove"
cmp -s /tmp/Caddyfile.configured "$CONFIG" || die "remove changed or deleted $CONFIG"
[ -f "$RECORD" ] || die "remove deleted the record"
systemctl is-active --quiet "user@$uid.service" && die "user manager still running after remove (apps would linger)" || true
[ ! -e /etc/systemd/system/moteld.service.d/10-user-manager.conf ] || die "user-manager drop-in left behind"
[ ! -e "/etc/systemd/system/user@$uid.service.d/10-motel.conf" ] || die "user@ limits drop-in left behind"
[ ! -e /var/lib/systemd/linger/motel ] || die "lingering left enabled"
kill -0 "$pid_after" 2>/dev/null && die "deployed app (pid $pid_after) survived package removal" || true
systemctl is-active --quiet motel-box-apply.path && die "the box applier's path unit is still active after remove (preremove did not stop it)" || true
[ ! -e /lib/systemd/system/motel-box-apply.path ] && [ ! -e /lib/systemd/system/motel-box-apply.service ] \
	|| die "the box applier's units are still installed after remove"
# postremove's daemon-reload, run after dpkg removed the unit files:
# the manager no longer loads them. The path unit is masked, as
# dh_installsystemd has it, until an install unmasks it (stage 5 finds
# it enabled and active again); the mask is a /dev/null link where the
# unit file was, and deb-systemd-helper's note of it is what lets
# postinstall take it off without undoing an administrator's own mask.
for u in moteld.service motel-box-apply.service; do
	[ "$(systemctl show -p LoadState --value "$u")" = not-found ] \
		|| die "$u is still loaded after remove ($(systemctl show -p LoadState,FragmentPath --value "$u" | tr '\n' ' ')): postremove did not reload the manager"
	[ -z "$(systemctl list-unit-files --no-legend "$u" 2>/dev/null)" ] \
		|| die "$u is still a unit file after remove: $(systemctl list-unit-files --no-legend "$u")"
done
[ "$(systemctl is-enabled motel-box-apply.path 2>/dev/null)" = masked ] \
	|| die "motel-box-apply.path is '$(systemctl is-enabled motel-box-apply.path 2>&1)' after remove, want masked (postremove's deb-systemd-helper mask)"
[ "$(readlink /etc/systemd/system/motel-box-apply.path)" = /dev/null ] \
	|| die "the mask of motel-box-apply.path is not a /dev/null link in /etc/systemd/system"
[ -e /var/lib/systemd/deb-systemd-helper-masked/motel-box-apply.path ] \
	|| die "deb-systemd-helper did not note the mask: postinstall's unmask would leave the path unit masked"
# The table's remove row: the timers stopped and masked, the unit files
# gone, the credential file and the account kept.
expect_backup_state "after remove" masked inactive "$CRED_SHA"
for u in motel-backup.service motel-backup-drill.service $TIMERS; do
	[ ! -e "/lib/systemd/system/$u" ] || die "/lib/systemd/system/$u still present after remove"
done
systemctl is-active --quiet motel-backup.service && die "a backup run is still active after remove" || true
getent passwd motel-backup >/dev/null || die "the motel-backup account was removed with the package"
[ ! -e /usr/bin/motel-backup ] || die "/usr/bin/motel-backup still present after remove"
echo "removal stopped the service, the apps and the box applier's path unit, kept the config and the record; the units unloaded, the path unit masked; the backup timers masked, the credential file and the account kept"

stage "stage 5: install again after removal, a backup run, and purge"
# preremove stops the path unit and does not disable it, so an install
# after a removal finds it enabled and starts it; a disable there would
# have left the applier off for good.
: >/tmp/before-reinstall
apt-get install -y "$deb" >/tmp/again.out 2>&1 || { cat /tmp/again.out; die "install after removal failed"; }
[ "$(systemctl is-enabled motel-box-apply.path)" = enabled ] && systemctl is-active --quiet motel-box-apply.path \
	|| die "after remove and install, the box applier's path unit is '$(systemctl is-enabled motel-box-apply.path)', $(systemctl is-active motel-box-apply.path); want enabled and active"
cmp -s /tmp/Caddyfile.configured "$CONFIG" || die "an install after removal changed the configured $CONFIG"
echo "install after removal: the path unit enabled and active again; the configured $CONFIG kept"
expect_backup_state "reinstalled after remove" enabled active "$CRED_SHA"
# The stamps are new: written by the timers' first activation, which
# fires nothing [M61]. With the old ones the drill's next elapse is the
# catch-up's, minutes away; with new ones it is a Sunday at 03:30 and
# its offset.
for u in $TIMERS; do
	[ "/var/lib/systemd/timers/stamp-$u" -nt /tmp/before-reinstall ] \
		|| die "reinstalled after remove: the stamp of $u is not this install's: $(ls -la --time-style=full-iso /var/lib/systemd/timers/)"
done
# A drill the timer fired in these seconds (a Sunday 03:30 and its
# offset) is running still, and its timer has no next elapse until it
# ends [M86]: read the elapse once there is one.
# A timer `running` has a run going; one `waiting` has a next elapse,
# so the next read gets it (a run ending between two reads); any
# other state is not waiting, and the wait ends at once.
not_timing() { case $(systemctl show -p SubState --value "$1") in running|waiting) return 1 ;; *) return 0 ;; esac; }
i=0
until dnext=$(systemctl show -p NextElapseUSecRealtime --value motel-backup-drill.timer) && [ -n "$dnext" ] || not_timing motel-backup-drill.timer || [ "$i" -ge 600 ]; do
	i=$((i + 1))
	sleep 1
done
[ -n "$dnext" ] || die "reinstalled after remove: motel-backup-drill.timer has no next elapse and is '$(systemctl show -p SubState --value motel-backup-drill.timer)' (running: the drill it fired is still going after 600s; anything else: the timer is not waiting): $(journalctl -u motel-backup-drill.timer -u motel-backup-drill.service --no-pager | tail -5)"
dpast=$(($(date -d "$dnext" +%s) - $(date -d "$(date -d "$dnext" '+%Y-%m-%d') 03:30:00" +%s)))
[ "$(date -d "$dnext" +%a)" = Sun ] && [ "$dpast" -ge 0 ] && [ "$dpast" -le 600 ] \
	|| die "reinstalled after remove: the drill's next elapse is '$dnext' (${dpast}s past 03:30 of its day), not a Sunday between 03:30 and 03:40: a catch-up"
# A firing is the service's main process starting [M61]. One inside the
# install is a catch-up — unless the timer's own elapse fell in these
# seconds (the hour and this box's offset, or a Sunday 03:30 and its
# offset): then the trigger is that elapse, a period before the next
# one, to the second or within AccuracySec (1s) after it — up to 60s
# is taken, for a manager late under load: a catch-up's trigger is
# anywhere in the period, and one can only come of a stamp the remove
# left, which stage 4 checks for itself. A catch-up's trigger is
# off the calendar: the manager's start and the offset [M68], or the
# timer's activation [M86]. The next elapse is empty while the fired
# run lasts: it is read once the run has ended.
for u in motel-backup.service motel-backup-drill.service; do
	t=${u%.service}.timer
	# The period is the unit's OnCalendar= by hand: held to the unit, on
	# every run, fired or not.
	case $t in motel-backup.timer) period=3600; cal='\*-\*-\* \*:00:00' ;; *) period=604800; cal='Sun \*-\*-\* 03:30:00' ;; esac
	systemctl show -p TimersCalendar --value "$t" | grep -q "OnCalendar=$cal ;" \
		|| die "reinstalled after remove: $t's calendar is '$(systemctl show -p TimersCalendar --value "$t")', not the ${period}s period this check takes"
	[ "$(systemctl show -p ExecMainStartTimestampMonotonic --value "$u")" = 0 ] && continue
	i=0
	until next=$(systemctl show -p NextElapseUSecRealtime --value "$t") && [ -n "$next" ] || not_timing "$t" || [ "$i" -ge 600 ]; do
		i=$((i + 1))
		sleep 1
	done
	[ -n "$next" ] || die "reinstalled after remove: $u ran inside the install, and $t has no next elapse and is '$(systemctl show -p SubState --value "$t")' (running: that run is still going after 600s; anything else: the timer is not waiting): $(journalctl -u "$t" -u "$u" --no-pager | tail -5)"
	last=$(systemctl show -p LastTriggerUSec --value "$t")
	[ -n "$last" ] || die "reinstalled after remove: $u ran inside the install, and not from its timer: $(journalctl -u "$u" --no-pager | tail -5)"
	late=$(( $(date -d "$last" +%s) - $(date -d "$next" +%s) + period ))
	[ "$late" -ge 0 ] && [ "$late" -le 60 ] \
		|| die "reinstalled after remove: $u ran inside the install, and not at its timer's elapse (last trigger '$last', next elapse '$next', ${late}s after the elapse a period before the next): a catch-up"
	echo "reinstalled after remove: $u ran inside the install at its timer's elapse (last trigger $last, ${late}s after it): a scheduled run, not a catch-up"
done
echo "reinstalled after remove: the stamps are new, and nothing caught up"
timeout 300 systemctl enable --now moteld || die "moteld did not start after the reinstall"
# A timer firing from here to the run below would put a drill, which
# holds the run lock (motel-backup-drill.service), or a run of its
# own in the way of the stage's run: the timers are held while it runs,
# and one fired since the check above is let end first. Before they
# are started again their stamps are touched: a start reads the
# stamp, and an elapse that passed while they were held would
# otherwise be caught up at once [M61, M86], into the purge's staging.
systemctl stop $TIMERS || die "reinstalled after remove: the timers could not be stopped: $(systemctl show -p ActiveState,Result $TIMERS | tr '\n' ' ')"
# Going: activating, or deactivating — a stop's grace still holds the
# run lock.
going() { case $(systemctl show -p ActiveState --value "$1") in activating|deactivating) return 0 ;; *) return 1 ;; esac; }
i=0
until ! going motel-backup.service && ! going motel-backup-drill.service || [ "$i" -ge 600 ]; do
	i=$((i + 1))
	sleep 1
done
if [ "$i" -ge 600 ] && { going motel-backup.service || going motel-backup-drill.service; }; then
	die "reinstalled after remove: a run or drill the timer fired is still running after 600s ($(systemctl show -p ActiveState --value motel-backup.service), $(systemctl show -p ActiveState --value motel-backup-drill.service)): $(journalctl -u motel-backup.service -u motel-backup-drill.service --no-pager | tail -5)"
fi
systemctl start motel-backup.service || die "a run after the reinstall failed: $(journalctl -u motel-backup.service --no-pager | tail -20)"
[ "$(systemctl show -p Result --value motel-backup.service)" = success ] || die "the run after the reinstall ended '$(systemctl show -p Result --value motel-backup.service)'"
echo "reinstalled after remove: the same credential file, a run works; the timers are held until the purge"
# What a killed run can leave behind: a bind mount of an app's data
# under the run directory, and the record writer's temp file. Purge
# must remove nothing of the app's through the first, and not be
# defeated by the second.
# A directory and a file, as a run binds them, a mount inside a mount,
# and the files a run keeps beside them; and the temp file setup's
# writer of the repository id leaves when it is killed mid-write.
su -s /bin/sh motel -c 'echo inner >/var/lib/liveswap/demo/shared/uploads/inner.txt'
mkdir -p /run/motel-backup/dead00000001/mount-1
: >/run/motel-backup/dead00000001/mount-2
disks_mounted "before the purge"
mount --rbind /var/lib/liveswap/demo/shared /run/motel-backup/dead00000001/mount-1 || die "could not stage a leftover bind mount"
mount --bind /var/lib/liveswap/demo/shared/app.db /run/motel-backup/dead00000001/mount-2 || die "could not stage a leftover bind mount of a file"
[ -f /run/motel-backup/dead00000001/mount-1/app.db ] && [ -f /run/motel-backup/dead00000001/mount-1/uploads/disk/kept.txt ] \
	|| die "the staged mounts do not show the app's data: the purge check would prove nothing"
# The mount a run makes of nothing, to ask the manager whether it is
# seen: a run killed right then leaves it.
mkdir -p /run/motel-backup/dead00000001/seen
mount --bind /run/motel-backup/dead00000001/seen /run/motel-backup/dead00000001/seen || die "could not stage a run's own probe mount"
echo '{}' >/run/motel-backup/dead00000001/plan.json
: >/run/motel-backup/dead00000001/.listing.err
# And the units a command killed from a shell left running — it has no
# service for the manager to end them with — recorded by their exact
# names, as a run records each before it starts it. A line that is no
# name the engine writes is nothing to stop.
LEFT=motel_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" -p User=motel-backup /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
systemd-run --quiet --unit=smoke-bystander.service /usr/bin/sleep 600 || die "could not stage a unit that is none of the engine's"
# And one of the engine's prefix that is no name the engine writes: no
# role and app, no run's twelve hex digits (Copilot on #155).
systemd-run --quiet --unit=motel_backup_bystander.service /usr/bin/sleep 600 || die "could not stage a unit of the engine's prefix"
printf '%s\nsmoke-bystander.service\nmotel_backup_bystander.service\n' "$LEFT" >/run/motel-backup/units
[ "$(systemctl is-active "$LEFT" || true)" = active ] || die "the staged unit is not running: the purge row would prove nothing"
: >/var/lib/motel-backup/.status-crash
echo 0123 >/var/lib/motel-backup/repository-id.new
# The timers, held since the stage's own run so that no run of theirs
# sweeps what was staged above, are started again here, their stamps
# touched first (a start reads the stamp, and an elapse that passed
# meanwhile would be caught up at once [M86]); the purge's preremove
# stops them, as the table says.
for u in $TIMERS; do touch "/var/lib/systemd/timers/stamp-$u"; done
systemctl start $TIMERS || die "reinstalled after remove: the timers could not be started again: $(systemctl show -p ActiveState,Result $TIMERS | tr '\n' ' ')"
expect_backup_state "before the purge" enabled active "$CRED_SHA"
apt-get purge -y moteld >/tmp/purge.log 2>&1 || { cat /tmp/purge.log; die "apt-get purge failed"; }
cat /tmp/purge.log
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] && [ -f /var/lib/liveswap/demo/shared/uploads/inner.txt ] && [ -f /tmp/disk/kept.txt ] \
	|| die "purge removed the app's data through a mount a killed run left under /run/motel-backup"
disks_mounted "after purge"
[ "$(systemctl is-active "$LEFT" || true)" != active ] \
	|| die "purge left a unit a killed command had left running, and removed the one record of it: $LEFT is still active, with the credential"
for u in smoke-bystander.service motel_backup_bystander.service; do
	[ "$(systemctl is-active "$u" || true)" = active ] \
		|| die "purge stopped $u, which is no name the engine writes, for being written in the units file"
done
systemctl stop smoke-bystander.service motel_backup_bystander.service
grep -q " /run/motel-backup" /proc/self/mountinfo \
	&& die "purge left a killed run's mounts, with no next run to sweep them: $(grep ' /run/motel-backup' /proc/self/mountinfo)" || true
[ ! -e /run/motel-backup ] || die "purge left /run/motel-backup: $(find /run/motel-backup)"
grep -q "still mounted\|/run/motel-backup" /tmp/purge.log \
	&& die "purge spoke of a run directory it removed whole: $(grep 'motel-backup' /tmp/purge.log)" || true
umount /var/lib/liveswap/demo/shared/uploads/disk "/var/lib/liveswap/demo/shared/uploads/my disk"
[ ! -e /var/lib/motel-backup/repository-id.new ] || die "purge left setup's temp file, repository-id.new"
grep -q "kept.*/var/lib/motel-backup" /tmp/purge.log \
	&& die "purge kept the state directory, which held nothing but the package's own: $(grep kept /tmp/purge.log)" || true
grep -q "$CRED is kept: it holds the repository password, the only way to read the backups already made" /tmp/purge.log \
	|| die "purge did not say that the credential file is kept, and why"
[ "$(sha256sum "$CRED" | cut -d' ' -f1)" = "$CRED_SHA" ] || die "purge changed or removed $CRED"
[ ! -e /var/lib/motel-backup/status.json ] && [ ! -e /var/lib/motel-backup/repository-id ] || die "purge left the record or the repository id"
[ ! -e /var/lib/motel-backup ] || die "purge left /var/lib/motel-backup, though nothing of an app's was in it: $(find /var/lib/motel-backup)"
[ ! -e /var/cache/motel-backup ] || die "purge left restic's cache"
getent passwd motel-backup >/dev/null || die "purge removed the motel-backup account"
for u in $TIMERS; do
	[ ! -e "/etc/systemd/system/$u" ] || die "purge left $u masked"
	[ ! -e "/etc/systemd/system/timers.target.wants/$u" ] || die "purge left $u enabled"
	[ ! -e "/var/lib/systemd/deb-systemd-helper-enabled/$u.dsh-also" ] || die "purge left the helper's state for $u"
done
[ ! -f "$CONFIG" ] || die "purge left $CONFIG"
for u in $TIMERS; do
	[ ! -e "/var/lib/systemd/timers/stamp-$u" ] || die "purge left the stamp of $u"
done
echo "purge kept the credential file and said why, removed the package's own state, the cache, the stamps and a killed run's mounts, kept the account"
# Installed again, so that stage 5b's first case is an upgrade over an
# installed package: the install writes the starter and its record, and
# moteld stays disabled and stopped, as the purge left it.
apt-get install -y "$deb" >/tmp/after-purge.out 2>&1 || { cat /tmp/after-purge.out; die "install after the purge failed"; }

stage "stage 5b: the starter across upgrades"
# Upgrades never ask about $CONFIG: postinstall installs the starter
# where nothing stands, replaces it only while it is byte-identical to
# the starter it last wrote (the record), and leaves anything else
# alone. moteld is disabled and stopped since stage 5's purge, so no
# postinstall here restarts it.
systemctl is-active --quiet moteld && die "moteld is running; stage 5b expects it stopped" || true
rm -f "$CONFIG"
dpkg -i "$deb" </dev/null >/tmp/starter.out 2>&1 || { cat /tmp/starter.out; die "reinstall over a missing $CONFIG failed"; }
cmp -s "$STARTER" "$CONFIG" || die "a missing $CONFIG was not restored from the starter"
[ "$(cat "$RECORD")" = "$(sha "$STARTER")" ] || die "restoring the starter did not record it"
echo "a missing $CONFIG: the starter restored and recorded"

# A newer package whose starter changed: this one, repacked.
cp "$STARTER" /tmp/starter.prev
rm -rf /tmp/repack
dpkg-deb -R "$deb" /tmp/repack
printf '# smoke: a changed starter\n' >>/tmp/repack/usr/share/motel/Caddyfile
sed -i 's/^Version: .*/&+smoke1/' /tmp/repack/DEBIAN/control
(cd /tmp/repack && find . -path ./DEBIAN -prune -o -type f -printf '%P\0' | xargs -0 md5sum >DEBIAN/md5sums)
dpkg-deb -b /tmp/repack /tmp/motel-smoke1.deb >/dev/null || die "could not repack the .deb with a changed starter"
dpkg -i /tmp/motel-smoke1.deb </dev/null >/tmp/starter.out 2>&1 \
	|| { cat /tmp/starter.out; die "the upgrade to a changed starter failed (a question at EOF fails it)"; }
grep -q 'Configuration file' /tmp/starter.out && { cat /tmp/starter.out; die "the upgrade to a changed starter asked about a config file"; }
tail -n 1 "$STARTER" | grep -qx '# smoke: a changed starter' || die "the repacked .deb did not ship the changed starter"
cmp -s "$STARTER" "$CONFIG" || die "a stock $CONFIG was not replaced by the changed starter"
got=$(stat -c '%U:%G %a %F' "$CONFIG")
[ "$got" = "root:root 644 regular file" ] || die "the replaced $CONFIG is '$got', want 'root:root 644 regular file'"
[ "$(cat "$RECORD")" = "$(sha "$STARTER")" ] || die "the record does not name the starter just written"
echo "a stock $CONFIG: replaced by the changed starter, with no question, and recorded"

# Each case below runs the installed postinst directly, the way stage
# 3b runs prerm: each is the state an upgrade would find.
ver=$(dpkg-query -W -f='${Version}' moteld)
postinst() {
	/var/lib/dpkg/info/moteld.postinst configure "$ver" </dev/null >/tmp/postinst.out 2>&1 \
		|| { cat /tmp/postinst.out; die "postinst exited non-zero over $1"; }
}
want=$(sha "$STARTER")

printf '# an operator was here\n' >>"$CONFIG"
cp "$CONFIG" /tmp/edited
postinst "an edited $CONFIG"
cmp -s /tmp/edited "$CONFIG" || die "postinst changed an edited $CONFIG"
[ "$(cat "$RECORD")" = "$want" ] || die "postinst changed the record over an edited $CONFIG"
echo "an edited $CONFIG: untouched"

# A crash between the rename and the record: the starter on disk, the
# record still the previous one's. The next run heals the record, or
# the box would look edited for good.
cp "$STARTER" "$CONFIG"
sha /tmp/starter.prev >"$RECORD"
ino=$(stat -c %i "$CONFIG")
postinst "a crash between the rename and the record"
[ "$(cat "$RECORD")" = "$want" ] || die "postinst did not heal a record left behind by a crash"
[ "$(stat -c %i "$CONFIG")" = "$ino" ] && cmp -s "$STARTER" "$CONFIG" || die "healing the record rewrote $CONFIG"
echo "a crash between the rename and the record: the record healed, $CONFIG not rewritten"

# A symlink is never followed, even to bytes the record names: here a
# followed link would be replaced.
rm -f "$CONFIG"
cp /tmp/starter.prev /tmp/linked
sha /tmp/linked >"$RECORD"
ln -s /tmp/linked "$CONFIG"
postinst "a symlink"
[ "$(readlink "$CONFIG")" = /tmp/linked ] || die "postinst replaced a symlinked $CONFIG"
cmp -s /tmp/starter.prev /tmp/linked || die "postinst wrote through a symlinked $CONFIG"
rm -f "$CONFIG"
ln -s /nonexistent/Caddyfile "$CONFIG"
postinst "a dangling symlink"
[ "$(readlink "$CONFIG")" = /nonexistent/Caddyfile ] && [ ! -e /nonexistent ] \
	|| die "postinst replaced or followed a dangling symlink at $CONFIG"
rm -f "$CONFIG"
mkdir "$CONFIG"
postinst "a directory"
[ -d "$CONFIG" ] && [ -z "$(ls -A "$CONFIG")" ] || die "postinst changed a directory at $CONFIG"
rmdir "$CONFIG"
echo "a symlink, a dangling symlink, a directory: untouched"

# Without a record that names it, a file that is not the starter is
# left alone: a garbled record, and none at all.
cp /tmp/starter.prev "$CONFIG"
echo garbage >"$RECORD"
postinst "a garbled record"
cmp -s /tmp/starter.prev "$CONFIG" || die "postinst replaced $CONFIG on a garbled record"
[ "$(cat "$RECORD")" = garbage ] || die "postinst rewrote a garbled record although it wrote no starter"
rm -f "$RECORD"
postinst "no record"
cmp -s /tmp/starter.prev "$CONFIG" || die "postinst replaced $CONFIG with no record"
[ ! -e "$RECORD" ] || die "postinst wrote a record although it wrote no starter"
echo "a garbled record, no record: $CONFIG untouched"
ls -A /etc/motel | grep -q '^\.Caddyfile\.package-' && die "postinst left a temporary in /etc/motel" || true

# postinst sweeps a crash's temporaries; a directory by one of their
# names is not its to remove, and must not fail the install.
mkdir /etc/motel/.Caddyfile.package-smoke "$RECORD.smoke"
postinst "directories named like its temporaries"
[ -d /etc/motel/.Caddyfile.package-smoke ] && [ -d "$RECORD.smoke" ] \
	|| die "postinst removed a directory named like its temporaries"
rmdir /etc/motel/.Caddyfile.package-smoke "$RECORD.smoke"
echo "directories named like its temporaries: left, and postinst exits 0"

# A directory at a temporary's exact name: made under the PID postinst
# will run as (exec keeps the shell's, and the script's $$ is it). The
# install goes on, saying why, and nothing is written into the
# directory, over it or in its place.
postinst_colliding() {
	sh -c 'mkdir -m 0755 "$1$$" && exec "$2" configure "$3"' sh "$1" /var/lib/dpkg/info/moteld.postinst "$ver" \
		</dev/null >/tmp/postinst.out 2>&1 \
		|| { cat /tmp/postinst.out; die "postinst exited non-zero with a directory at its temporary $1<pid>"; }
	collided=$(ls -d "$1"[0-9]*) || die "no directory at $1<pid>: the collision tested nothing"
	[ "$(stat -c '%a %F' "$collided")" = "755 directory" ] && [ -z "$(ls -A "$collided")" ] \
		|| die "postinst changed the directory at $collided: $(stat -c '%a %F' "$collided"); holds: $(ls -A "$collided")"
	rmdir "$collided"
}
cp /tmp/starter.prev "$CONFIG"
sha /tmp/starter.prev >"$RECORD"
postinst_colliding /etc/motel/.Caddyfile.package-
cmp -s /tmp/starter.prev "$CONFIG" || die "postinst changed a stock $CONFIG although its temporary was a directory"
[ "$(cat "$RECORD")" = "$(sha /tmp/starter.prev)" ] || die "postinst recorded a starter it did not install"
grep -q 'could not install the starter config' /tmp/postinst.out || die "postinst did not say it could not install the starter"
rm -f "$CONFIG"
postinst_colliding /etc/motel/.Caddyfile.package-
[ ! -e "$CONFIG" ] && [ ! -L "$CONFIG" ] \
	|| die "postinst put a $(stat -c %F "$CONFIG") at a missing $CONFIG although its temporary was a directory"
[ "$(cat "$RECORD")" = "$(sha /tmp/starter.prev)" ] || die "postinst recorded a starter it did not install"
grep -q 'could not install the starter config' /tmp/postinst.out || die "postinst did not say it could not install the starter"
# The starter on disk and a stale record: postinst writes only the record.
cp "$STARTER" "$CONFIG"
postinst_colliding "$RECORD."
[ "$(cat "$RECORD")" = "$(sha /tmp/starter.prev)" ] || die "the record changed although its temporary was a directory"
cmp -s "$STARTER" "$CONFIG" || die "postinst changed $CONFIG while failing to write the record"
grep -q "could not write $RECORD durably" /tmp/postinst.out || die "postinst did not say it could not write the record"
echo "a directory at a temporary's exact name: left as it was, $CONFIG and the record unchanged, postinst exits 0 and says why"

apt-get remove -y moteld >/dev/null
systemctl is-active --quiet motel-box-apply.path && die "the second remove left the path unit active" || true
echo "stage 5b: removed again; the path unit stopped"

stage "stage 6: purge"
# postremove on purge removes what the package made and nothing else
# holds — the box's config, the starter's record, the box applier's
# exchange tree, the path unit's enable state and its mask — and never
# Caddy's state, the deployed apps' data or the account. The box's key
# for secrets stays in /etc/motel, and the purge says so on stderr.
# Each path is checked to be there first, so that "gone" proves the
# purge removed it; the installed postrm is copied for the mount case
# below, since dpkg removes the script with the purge.
cp /var/lib/dpkg/info/moteld.postrm /tmp/motel.postrm
: >/var/lib/motel/smoke-state
for p in "$CONFIG" "$RECORD" "$B/in" "$B/out" /etc/motel/age/identity "$view" \
	/var/lib/liveswap/other/shared/secret /var/lib/systemd/deb-systemd-helper-enabled/motel-box-apply.path.dsh-also \
	/var/lib/systemd/deb-systemd-helper-masked/motel-box-apply.path; do
	[ -e "$p" ] || die "the purge would prove nothing: $p is already gone"
done
apt-get purge -y moteld >/tmp/purge.out 2>&1 || { cat /tmp/purge.out; die "apt-get purge failed"; }
cat /tmp/purge.out
case "$(dpkg-query -W -f='${Status}' moteld 2>/dev/null)" in
'' | *not-installed) : ;;
*) die "moteld is still '$(dpkg-query -W -f='${Status}' moteld)' after purge" ;;
esac
[ ! -e "$CONFIG" ] && [ ! -L "$CONFIG" ] || die "purge left $CONFIG"
[ ! -e "${RECORD%/*}" ] || die "purge left ${RECORD%/*}"
[ ! -e "$B" ] || die "purge left $B: $(find "$B" | tr '\n' ' ')"
[ -f /var/lib/motel/smoke-state ] || die "purge removed /var/lib/motel, Caddy's state"
[ -f "$view" ] && [ -f /var/lib/liveswap/other/shared/secret ] || die "purge removed the apps' data under /var/lib/liveswap"
id motel >/dev/null || die "purge removed the motel account"
[ -f /etc/motel/age/identity ] && [ "$(cat /etc/motel/age.pub)" = "$box_key" ] \
	|| die "purge removed the box's key for secrets, the one way to read the secrets its repository holds"
grep -q 'kept /etc/motel: it still holds' /tmp/purge.out || die "the purge did not say it kept /etc/motel"
tr -d '\r' </tmp/purge.out | grep -q 'age/identity is the box.s key for secrets.*once nothing needs them: rm -r /etc/motel$' || die "the purge did not name the key it kept, and the command that finishes the job, with no sudo"
# The path unit's enable state and mask: gone, so a later install
# starts from nothing (postinstall's was-enabled is true with no state).
st=$(systemctl is-enabled motel-box-apply.path 2>&1) && die "motel-box-apply.path is still '$st' after purge" || true
[ "$st" != masked ] || die "purge left motel-box-apply.path masked"
[ "$(systemctl show -p LoadState --value motel-box-apply.path)" = not-found ] \
	|| die "motel-box-apply.path is '$(systemctl show -p LoadState --value motel-box-apply.path)' after purge, want not-found"
for p in /etc/systemd/system/motel-box-apply.path /etc/systemd/system/multi-user.target.wants/motel-box-apply.path \
	/var/lib/systemd/deb-systemd-helper-enabled/motel-box-apply.path.dsh-also /var/lib/systemd/deb-systemd-helper-masked/motel-box-apply.path; do
	[ ! -e "$p" ] && [ ! -L "$p" ] || die "purge left $p"
done
echo "purge: config, record and exchange tree gone; the key, Caddy's state, the apps' data and the account kept; the path unit's state and mask gone"

# A mount at or under the exchange tree: nothing is removed through
# it, the tree is kept whole and named, and the script still exits 0.
# The tree is made again by hand and the saved postrm run as dpkg
# runs it.
mkdir -p "$B/work"
mount -t tmpfs -o size=1m smoke "$B/work" || die "could not mount a tmpfs under $B; the mount case would prove nothing"
: >"$B/work/through-the-mount"
: >"$B/beside-the-mount"
sh /tmp/motel.postrm purge >/tmp/postrm.out 2>&1 || { cat /tmp/postrm.out; die "postrm purge exited non-zero with a mount under $B"; }
[ -f "$B/work/through-the-mount" ] && [ -f "$B/beside-the-mount" ] \
	|| die "postrm purge removed through or beside a mount under $B; left: $(find "$B" | tr '\n' ' ')"
grep -q "kept $B: something is mounted at or under it" /tmp/postrm.out \
	|| die "postrm purge did not say it kept $B: $(cat /tmp/postrm.out)"
umount "$B/work"
rm -rf "$B"
echo "a mount under $B: postrm purge kept the tree whole, said so, exit 0"

# The mount table names a mount point by its physical path. A tree
# reached through a symlinked ancestor (/var -> /srv/var) must still be
# seen as mounted: the function is taken out of the saved postrm and
# asked about a stand-in reached through a link, since /var itself
# cannot be made a link here.
sed -n '/^mounted() {/,/^}/p' /tmp/motel.postrm >/tmp/mounted.sh
grep -q '^mounted() {' /tmp/mounted.sh || die "could not take mounted() out of the postrm for the symlink case"
mkdir -p /tmp/real-var/tree/work
ln -sfn /tmp/real-var /tmp/link-var
mount -t tmpfs -o size=1m smoke /tmp/real-var/tree/work || die "could not mount a tmpfs for the symlink case"
if (. /tmp/mounted.sh && mounted /tmp/link-var/tree); then :; else
	die "mounted() missed a tmpfs under a tree reached through a symlinked ancestor (/tmp/link-var -> /tmp/real-var)"
fi
if (. /tmp/mounted.sh && mounted /tmp/link-var/other); then
	die "mounted() said a tree with nothing mounted under it is mounted (symlink case)"
fi
umount /tmp/real-var/tree/work
rm -rf /tmp/real-var /tmp/link-var
echo "a mount under a tree reached through a symlinked ancestor: mounted() saw it"

# A physical path the mount table escapes (a space is written \040):
# the comparison cannot see a mount under it, so the function fails
# closed and says mounted whether or not anything is.
mkdir -p "/tmp/real var/tree/work"
ln -sfn "/tmp/real var" /tmp/link-spaced
mount -t tmpfs -o size=1m smoke "/tmp/real var/tree/work" || die "could not mount a tmpfs for the escaped-path case"
if (. /tmp/mounted.sh && mounted /tmp/link-spaced/tree); then :; else
	die "mounted() missed a tmpfs under a tree whose physical path holds a space (written \\040 in the mount table)"
fi
umount "/tmp/real var/tree/work"
if (. /tmp/mounted.sh && mounted /tmp/link-spaced/tree); then :; else
	die "mounted() did not fail closed on a physical path holding a space"
fi
rm -rf "/tmp/real var" /tmp/link-spaced
echo "a physical path the mount table escapes: mounted() fails closed, mount or not"

# A scan that fails (awk exiting 2 with no answer) must count as
# mounted: the tree is kept and named, not removed.
mkdir -p /tmp/fakebin "$B/in"
printf '#!/bin/sh\nexit 2\n' >/tmp/fakebin/awk
chmod 0755 /tmp/fakebin/awk
: >"$B/in/entry"
PATH="/tmp/fakebin:$PATH" sh /tmp/motel.postrm purge >/tmp/postrm.out 2>&1 || { cat /tmp/postrm.out; die "postrm purge exited non-zero with a failing awk"; }
[ -f "$B/in/entry" ] || die "postrm purge removed $B although the mount scan failed; output: $(cat /tmp/postrm.out)"
grep -q "kept $B: something is mounted at or under it" /tmp/postrm.out \
	|| die "postrm purge did not say it kept $B when the scan failed: $(cat /tmp/postrm.out)"
rm -rf /tmp/fakebin "$B"
echo "a failing mount scan: postrm purge kept the tree, said so, exit 0"

# A run of the applier under way holds root's lock for its whole run
# and swaps the Caddyfile under it; preremove leaves a run alone. The
# purge takes that lock first: it waits for a run to finish, then
# removes the config and the tree; one that holds the lock past 30s
# keeps both, named, and the script still exits 0. A flock on the lock
# file stands in for the run, started and then seen held.
hold_lock() { # <seconds>
	mkdir -p "$B/in"
	: >"$B/lock"
	flock "$B/lock" sleep "$1" &
	holder=$!
	until ! flock -n "$B/lock" true 2>/dev/null; do sleep 0.1; done
}
cp /tmp/starter.prev "$CONFIG"
hold_lock 4
t0=$(date +%s)
sh /tmp/motel.postrm purge >/tmp/postrm.out 2>&1 || { cat /tmp/postrm.out; die "postrm purge exited non-zero while a run held the lock"; }
t1=$(date +%s)
wait "$holder" || true
[ $((t1 - t0)) -ge 3 ] || die "postrm purge finished in $((t1 - t0))s with the lock held for 4s: it did not wait for the run"
[ ! -e "$CONFIG" ] && [ ! -e "$B" ] || die "postrm purge waited for the lock but left $CONFIG or $B"
echo "a run holding the lock for 4s: postrm purge waited $((t1 - t0))s, then removed the config and the tree"
cp /tmp/starter.prev "$CONFIG"
hold_lock 120
t0=$(date +%s)
sh /tmp/motel.postrm purge >/tmp/postrm.out 2>&1 || { cat /tmp/postrm.out; die "postrm purge exited non-zero with the lock held past its wait"; }
t1=$(date +%s)
kill "$holder" 2>/dev/null || true
[ $((t1 - t0)) -ge 29 ] || die "postrm purge gave up on the lock after $((t1 - t0))s, want about 30"
[ -f "$CONFIG" ] && [ -d "$B/in" ] || die "postrm purge removed $CONFIG or $B although a run held the lock"
grep -q "kept $CONFIG and $B: a run of the box applier holds $B/lock" /tmp/postrm.out \
	|| die "postrm purge did not say it kept both for the run: $(cat /tmp/postrm.out)"
rm -rf "$B" "$CONFIG"
echo "a run holding the lock past 30s: postrm purge kept the config and the tree, said so, exit 0"

stage "stage 7: an account that cannot be made, a command under way at purge, and what purge keeps"
# A group of the account's name and no account: useradd exits 9 [M66].
# The configure completes — motel is installed, on a box that may
# use no backups — and says what setup will have to do (the owner,
# 2026-09-27).
userdel motel-backup || die "could not remove the account to stage one that cannot be made"
getent group motel-backup >/dev/null || groupadd --system motel-backup
apt-get install -y "$deb" >/tmp/install-noaccount.log 2>&1 \
	|| { cat /tmp/install-noaccount.log; die "the install failed where the motel-backup account could not be made: motel is left half-configured"; }
dpkg -s moteld | grep -q '^Status: install ok installed$' || die "moteld is not configured: $(dpkg -s moteld | grep ^Status)"
getent passwd motel-backup >/dev/null && die "the account was made: the row proves nothing" || true
grep -q "motel: the motel-backup account could not be made (useradd: group motel-backup exists" /tmp/install-noaccount.log \
	|| die "postinstall did not say that the account could not be made, and why: $(cat /tmp/install-noaccount.log)"
grep -q "nothing else of motel does: motel-backup setup <repository> makes it, or says why it cannot" /tmp/install-noaccount.log \
	|| die "postinstall did not say what makes the account: $(cat /tmp/install-noaccount.log)"
# The credential file is there (purge kept it) and the account is not:
# the Backups: line must not call that set up, in the breath after
# saying the account is missing, nor say the timers run nothing (the
# units' condition is the file, so each run starts and fails on the
# account, as the lines below prove) (#403).
grep -q "enabled and set up" /tmp/install-noaccount.log \
	&& die "postinstall called the backups set up where it had just said the account could not be made: $(grep Backups: /tmp/install-noaccount.log)" || true
tr -d '\r' </tmp/install-noaccount.log | grep -qx "  motel-backup setup <repository>" \
	&& die "postinstall said the timers run nothing until setup on a box whose runs start and fail on the account" || true
tr -d '\r' </tmp/install-noaccount.log | grep -qx "Backups: the hourly timer and the Sunday restore drill are enabled, and the credential file is in place; the runs wait on the motel-backup account (above): motel-backup account says when it is right" \
	|| die "postinstall did not say that the runs wait on the account: $(grep Backups: /tmp/install-noaccount.log || echo 'no Backups: line')"
timeout 300 systemctl enable --now moteld || die "moteld did not start on the box where the account could not be made"
expect_backup_state "installed where the account cannot be made" enabled active "$CRED_SHA"
systemctl start motel-backup.service && die "a run with no account exited 0" || true
journalctl -u motel-backup.service --no-pager | grep -q "the motel-backup account is not there" \
	|| die "the run did not say that the account is not there: $(journalctl -u motel-backup.service --no-pager | tail -5)"
systemctl reset-failed motel-backup.service 2>/dev/null || true
groupdel motel-backup
echo "a group of the account's name: installed, and said; a run says the account is not there"
# Purge with a command holding the run lock: said, its mounts left to
# it. And what purge keeps, said for what it is: copies of an app's
# data in staging, and a file in the state directory that the package
# did not make, which is no app's data.
mkdir -p /run/motel-backup/11fe00000001/mount-1 /var/lib/motel-backup/staging/demo
mount --bind /var/lib/liveswap/demo/shared /run/motel-backup/11fe00000001/mount-1 || die "could not stage a mount of a run under way"
flock /run/motel-backup/lock -c 'echo "pid $$, since 2026-09-27T00:00:00Z" >/run/motel-backup/lock; exec sleep 300' &
holder=$!
i=0
until ! flock -n /run/motel-backup/lock true 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 50 ] && die "could not hold the run lock: the row would prove nothing"
	sleep 0.1
done
echo plaintext >/var/lib/motel-backup/staging/demo/app.db
echo mine >/var/lib/motel-backup/notes.txt
apt-get purge -y moteld >/tmp/purge-held.log 2>&1 || { cat /tmp/purge-held.log; die "apt-get purge with a command under way failed"; }
cat /tmp/purge-held.log
grep -q "motel: what a backup command left was not swept, and is left: another backup run is in progress: pid [0-9]*, since 2026-09-27T00:00:00Z" /tmp/purge-held.log \
	|| die "preremove did not say that a command holds the run lock"
grep -q "motel: kept /run/motel-backup: something is mounted at or under it" /tmp/purge-held.log \
	|| die "postremove did not say why it kept the run directory"
[ -f /run/motel-backup/11fe00000001/mount-1/app.db ] || die "purge unmounted under a command that holds the run lock"
grep -q "motel: kept /var/lib/motel-backup/staging: not empty — copies of an app's data" /tmp/purge-held.log \
	|| die "purge did not say that staging is kept, and what is in it"
grep -q "motel: kept /var/lib/motel-backup: it holds what this package did not make (notes.txt)" /tmp/purge-held.log \
	|| die "purge did not say what the state directory is kept for"
grep "kept /var/lib/motel-backup:" /tmp/purge-held.log | grep -q "copies of an app's data" \
	&& die "purge blamed copies of app data for a file that is none" || true
[ -f /var/lib/motel-backup/staging/demo/app.db ] && [ -f /var/lib/motel-backup/notes.txt ] || die "purge removed what it said it kept"
pkill -P "$holder" 2>/dev/null || true
kill "$holder" 2>/dev/null || true
umount /run/motel-backup/11fe00000001/mount-1
rm -rf /run/motel-backup /var/lib/motel-backup
echo "purge under a command's lock: said, its mount left; what is kept is said for what it is"
# Mounts under the run directory of names the engine does not make:
# the directory is root's alone, 0700, and nothing but the engine
# mounts there, so its sweep takes every mount under it — made
# private first, so that what was beneath the app's own directory
# stays — and the app's files stay (the owner, 2026-09-27: the
# engine's sweep, not a shell copy with its own rules).
apt-get install -y "$deb" >/tmp/install-again.log 2>&1 || { cat /tmp/install-again.log; die "the install before the last purge failed"; }
ODD="/run/motel-backup/odd /run/motel-backup/dead00000001/other /run/motel-backup/notarun/mount-1"
mkdir -p "/run/motel-backup/odd dir" /run/motel-backup/dead00000001/mount-1 $ODD
echo '{}' >/run/motel-backup/dead00000001/plan.json
for m in "/run/motel-backup/odd dir" $ODD /run/motel-backup/dead00000001/mount-1; do
	mount --bind /var/lib/liveswap/demo/shared "$m" || die "could not stage a mount at $m"
done
cp /var/lib/dpkg/info/moteld.postrm /tmp/motel.postrm
apt-get purge -y moteld >/tmp/purge-odd.log 2>&1 || { cat /tmp/purge-odd.log; die "apt-get purge with mounts of other names failed"; }
cat /tmp/purge-odd.log
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] \
	|| die "purge removed the app's files through a mount under the run directory"
grep -q " /run/motel-backup" /proc/self/mountinfo \
	&& die "purge left mounts under the run directory: $(grep ' /run/motel-backup' /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')" || true
[ ! -e /run/motel-backup ] || die "purge left /run/motel-backup: $(find /run/motel-backup)"
echo "mounts of other names under the run directory: taken away by the engine's sweep, and nothing of the app's with them"

stage "stage 8: purge where the mount table cannot be read"
# In a chroot or a namespace with no /proc, postremove cannot see what
# is mounted under the run directory: it removes nothing there, and
# says so, rather than taking an empty mount table for none (Copilot
# on #155). Run in a mount namespace of its own with /proc gone, the
# script as the package shipped it, against a bind of the app's data
# sitting right on a run directory.
[ -f /tmp/motel.postrm ] || die "the package's postremove was not kept for this stage"
unshare --mount --propagation private sh -c '
	set -e
	# Right on a run directory, whose files purge removes one by one
	# once it believes nothing is mounted there.
	mkdir -p /run/motel-backup/0123456789ab
	mount --bind /var/lib/liveswap/demo/shared /run/motel-backup/0123456789ab
	[ -f /run/motel-backup/0123456789ab/app.db ] || { echo "FIXTURE: the bind shows nothing"; exit 3; }
	umount -l /proc
	[ ! -r /proc/self/mountinfo ] || { echo "FIXTURE: /proc is still there"; exit 3; }
	sh /tmp/motel.postrm purge >/tmp/purge-noproc.log 2>&1 || true
	[ -f /run/motel-backup/0123456789ab/app.db ] || { echo "GONE"; exit 4; }
' ; rc=$?
[ "$rc" != 3 ] || die "the mount table could still be read: the row proves nothing"
[ "$rc" = 0 ] || die "purge with no mount table removed the app's files through a mount under the run directory (exit $rc): $(cat /tmp/purge-noproc.log)"
[ -f /var/lib/liveswap/demo/shared/app.db ] && [ -f /var/lib/liveswap/demo/shared/uploads/a.png ] || die "purge with no mount table removed the app's files"
grep -q "motel: kept /run/motel-backup: something is mounted at or under it, or what is mounted cannot be read; nothing under it is removed" /tmp/purge-noproc.log \
	|| die "postremove did not say that it could not read the mount table: $(cat /tmp/purge-noproc.log)"
rm -rf /run/motel-backup
echo "no mount table: purge removed nothing under the run directory, and said why"
# And a mount on the run directory itself, with /proc there: purge
# removes the run directory's own files by name, and through such a
# mount those would be the app's (Copilot on #155).
mkdir -p /run/motel-backup /tmp/at-run
echo "the app's" >/tmp/at-run/lock
echo "the app's" >/tmp/at-run/units
mount --bind /tmp/at-run /run/motel-backup
sh /tmp/motel.postrm purge >/tmp/purge-atrun.log 2>&1 || true
umount /run/motel-backup
[ -f /tmp/at-run/lock ] && [ -f /tmp/at-run/units ] || die "purge removed files through a mount on the run directory itself: $(ls -la /tmp/at-run)"
grep -q "motel: kept /run/motel-backup: something is mounted at or under it" /tmp/purge-atrun.log \
	|| die "postremove did not say why it kept the run directory: $(cat /tmp/purge-atrun.log)"
rm -rf /run/motel-backup /tmp/at-run
echo "a mount on the run directory itself: purge removed nothing through it, and said why"
# And a command that holds the run lock with nothing mounted — a
# restore waiting at its prompt: purge leaves the lock, the list of
# its units and the init marker to it, and says so (the owner's
# review of #155: they were removed from under it, and the units it
# had started had no record left for any sweep).
mkdir -p /run/motel-backup
chmod 700 /run/motel-backup
echo motel_backup_upload_demo_0123456789ab.service >/run/motel-backup/units
echo motel_backup_init_0123456789ab.service >/run/motel-backup/init-unit
flock /run/motel-backup/lock -c 'echo "pid $$, since 2026-09-27T00:00:00Z" >/run/motel-backup/lock; exec sleep 300' &
holder=$!
i=0
until ! flock -n /run/motel-backup/lock true 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 50 ] && die "could not hold the run lock: the row would prove nothing"
	sleep 0.1
done
sh /tmp/motel.postrm purge >/tmp/purge-locked.log 2>&1 || true
pkill -P "$holder" 2>/dev/null || true
kill "$holder" 2>/dev/null || true
[ -f /run/motel-backup/units ] && [ -f /run/motel-backup/init-unit ] && [ -f /run/motel-backup/lock ] \
	|| die "purge removed the run directory's files from under a command that holds the run lock: $(ls -la /run/motel-backup 2>&1)"
grep -q "motel: kept /run/motel-backup: the run lock is held (pid [0-9]*, since 2026-09-27T00:00:00Z)" /tmp/purge-locked.log \
	|| die "postremove did not say that the run lock is held: $(cat /tmp/purge-locked.log)"
rm -rf /run/motel-backup
echo "a command holding the run lock: purge left its files, and said why"
# A command from a shell, killed between a remove and a purge: its
# upload unit, which no service ends, still runs with the credential,
# and its mounts are there. The program is gone, so no sweep: purge
# stops the units the command recorded, by the engine's grammar, and
# leaves the mounts, as it says (Copilot on #155).
LEFT=motel_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
systemd-run --quiet --unit=smoke-bystander.service /usr/bin/sleep 600 || die "could not stage a unit that is none of the engine's"
mkdir -p /run/motel-backup/0123456789ab/mount-1
chmod 700 /run/motel-backup
mount --bind /var/lib/liveswap/demo/shared /run/motel-backup/0123456789ab/mount-1 || die "could not stage a killed command's mount"
printf '%s\nsmoke-bystander.service\n' "$LEFT" >/run/motel-backup/units
sh /tmp/motel.postrm purge >/tmp/purge-killed.log 2>&1 || true
[ "$(systemctl is-active "$LEFT" || true)" != active ] \
	|| die "purge left a killed command's upload unit running, with the credential: $(cat /tmp/purge-killed.log)"
[ "$(systemctl is-active smoke-bystander.service || true)" = active ] || die "purge stopped a unit that is none of the engine's"
systemctl stop smoke-bystander.service
mountpoint -q /run/motel-backup/0123456789ab/mount-1 || die "purge took a mount away without the engine's sweep"
[ -f /var/lib/liveswap/demo/shared/app.db ] || die "purge removed the app's files"
grep -q "motel: kept /run/motel-backup: something is mounted at or under it" /tmp/purge-killed.log || die "purge did not say why it kept the run directory"
umount /run/motel-backup/0123456789ab/mount-1
rm -rf /run/motel-backup
echo "a killed command's units after a remove: stopped by purge, by their recorded names; its mount left, and said"
# And one that will not stop: said, by name, and its record kept, so
# that the name is not lost with the credential still in its
# environment (Copilot on #155).
STUCK=motel_backup_upload_demo_0123456789ab.service
mkdir -p "/run/systemd/system/$STUCK.d"
printf '[Unit]\nRefuseManualStop=yes\n' >"/run/systemd/system/$STUCK.d/10-smoke.conf"
systemd-run --quiet --unit="$STUCK" /usr/bin/sleep 600 || die "could not stage a unit that will not stop"
systemctl daemon-reload
mkdir -p /run/motel-backup
chmod 700 /run/motel-backup
echo "$STUCK" >/run/motel-backup/units
sh /tmp/motel.postrm purge >/tmp/purge-stuck.log 2>&1 || true
[ "$(systemctl is-active "$STUCK" || true)" = active ] || die "the unit staged as one that will not stop stopped: the row proves nothing"
grep -q "motel: $STUCK, which a backup command left running, would not stop" /tmp/purge-stuck.log \
	|| die "purge did not say that a unit would not stop: $(cat /tmp/purge-stuck.log)"
grep -qx "$STUCK" /run/motel-backup/units 2>/dev/null || die "purge removed the record of a unit that would not stop"
rm -rf "/run/systemd/system/$STUCK.d"
systemctl daemon-reload
systemctl stop "$STUCK" 2>/dev/null || true
rm -rf /run/motel-backup
echo "a unit that would not stop: said by name, and its record kept"
# And the manager unreachable: its state cannot be asked, which is no
# "gone" — a missing unit reads "inactive" [measured] — so it is said,
# and the record kept (Copilot on #155).
LEFT=motel_backup_upload_demo_0123456789ab.service
systemd-run --quiet --unit="$LEFT" /usr/bin/sleep 600 || die "could not stage a unit a killed command left running"
mkdir -p /run/motel-backup
chmod 700 /run/motel-backup
echo "$LEFT" >/run/motel-backup/units
mv /run/systemd/private /run/systemd/private.smoke-aside
systemctl show -p ActiveState --value "$LEFT" >/dev/null 2>&1 && { mv /run/systemd/private.smoke-aside /run/systemd/private; die "the manager still answered: the row proves nothing"; }
sh /tmp/motel.postrm purge >/tmp/purge-nobus.log 2>&1 || true
mv /run/systemd/private.smoke-aside /run/systemd/private
grep -qx "$LEFT" /run/motel-backup/units 2>/dev/null || die "purge removed the record of a unit whose state could not be asked: $(cat /tmp/purge-nobus.log)"
grep -q "motel: $LEFT, which a backup command left running, could not be asked about" /tmp/purge-nobus.log \
	|| die "purge did not say that it could not ask about a unit: $(cat /tmp/purge-nobus.log)"
systemctl stop "$LEFT" 2>/dev/null || true
rm -rf /run/motel-backup
echo "the manager unreachable: said, and the record kept"

echo ""
echo "ALL PACKAGE SMOKE STAGES PASSED ($deb on $(. /etc/os-release && echo "$PRETTY_NAME"))"
