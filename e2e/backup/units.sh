#!/bin/sh
# The units suite. Runs inside an e2e-backup-box of its own as root,
# against the unit files the package ships (copied into the image where
# the .deb puts them) and real systemd: the services start a run and a
# drill with the hardening the files say and nothing more — the
# measurement of that set, PLAN M54/M55 — bind their units to
# themselves, do nothing before setup, fail as units when they fail,
# order the drill behind a run, fire from their timers as written,
# refuse in a mount namespace of their own, leave an operator's disk
# mounted, and a helper of another version does nothing.
# Nothing here enables a timer for good: a timer firing into another
# suite would take the run lock from under it.
. /lib.sh
. /lib-backup.sh

REPO=s3:http://e2e-s3:9000/unitsrepo
S=motel-backup.service
D=motel-backup-drill.service
T=motel-backup.timer
DT=motel-backup-drill.timer

wait_for_systemd
cp "$CADDYFILE" /root/Caddyfile.base
rm -f "$ENVFILE"
systemctl reset-failed 2>/dev/null

prop() { systemctl show -p "$2" --value "$1"; } # <unit> <property>
failed_units() { systemctl list-units --failed --plain --no-legend 'motel-backup*' | awk '{print $1}' | tr '\n' ' '; }
says() { grep -q -e "$1" "$OUT"; }
# until_state <unit> <ActiveState> [<seconds>]: waits for the unit to reach it
until_state() {
	i=0
	until [ "$(prop "$1" ActiveState)" = "$2" ] || [ "$i" -ge "$((${3:-120} * 10))" ]; do
		i=$((i + 1))
		sleep 0.1
	done
	[ "$(prop "$1" ActiveState)" = "$2" ]
}
big() { as_app sh -c 'head -c 50000000 /dev/urandom >/var/lib/liveswap/blog/shared/uploads/big.bin'; }

echo "=== units 0: the unit files, as the manager reads them ==="
systemd-analyze verify /lib/systemd/system/motel-backup.service /lib/systemd/system/motel-backup.timer /lib/systemd/system/motel-backup-drill.service /lib/systemd/system/motel-backup-drill.timer >"$OUT" 2>&1
[ $? = 0 ] && [ ! -s "$OUT" ] && pass "systemd-analyze verify has nothing to say of the four files" || fail "systemd-analyze verify: $(cat "$OUT")"
# What the units run as root is root's alone to write, as the package
# installs it (packaging/nfpm.yaml, 0755).
[ "$(stat -c '%a %U %G' /usr/bin/moteld /usr/bin/motel-backup | tr '\n' ' ')" = "755 root root 755 root root " ] \
	&& pass "moteld and motel-backup are 0755 root:root" || fail "binaries: $(stat -c '%n %a %U %G' /usr/bin/moteld /usr/bin/motel-backup | tr '\n' ' ')"
for u in $S $D; do
	for row in "Type=oneshot" "PrivateMounts=no" "ProtectSystem=no" "ProtectHome=no" "PrivateTmp=no" "PrivateNetwork=no" "ProtectKernelModules=no" "RestrictAddressFamilies=AF_UNIX" "NoNewPrivileges=yes" "RestrictSUIDSGID=yes" "CapabilityBoundingSet=cap_chown cap_dac_read_search cap_fowner cap_sys_admin" "Environment=MOTEL_BACKUP_UNIT=$u" "TimeoutStartUSec=infinity" "TimeoutStopUSec=3min" "Result=success" "ActiveState=inactive"; do
		k=${row%%=*}
		want=${row#*=}
		got=$(prop "$u" "$k")
		[ "$got" = "$want" ] && pass "$u: $k is '$want'" || fail "$u: $k is '$got', want '$want'"
	done
	# The manager prints its Conditions= property as [unprintable]; the
	# file it loaded is the word, and units 1 is the proof.
	grep -qx "ConditionPathExists=$ENVFILE" "/lib/systemd/system/$u" && pass "$u: conditioned on $ENVFILE" || fail "$u: no ConditionPathExists= line for $ENVFILE"
	prop "$u" After | tr ' ' '\n' | grep -qx network-online.target && pass "$u: after the network is up" || fail "$u: After=$(prop "$u" After)"
done
prop $D After | tr ' ' '\n' | grep -qx "$S" && pass "the drill is After= the run" || fail "the drill's After=$(prop $D After)"
prop $S After | tr ' ' '\n' | grep -qx "$D" && fail "the run is After= the drill too: an ordering cycle" || pass "the run is not After= the drill"
prop $T TimersCalendar | grep -q '\*-\*-\* \*:00:00' && pass "the hourly timer is on the hour" || fail "TimersCalendar=$(prop $T TimersCalendar)"
[ "$(prop $T RandomizedDelayUSec)" = 10min ] && [ "$(prop $T FixedRandomDelay)" = yes ] && [ "$(prop $T Persistent)" = yes ] && pass "with a fixed delay of up to ten minutes, and catching up a missed one" || fail "the hourly timer: $(systemctl show -p RandomizedDelayUSec,FixedRandomDelay,Persistent $T | tr '\n' ' ')"
[ "$(prop $T AccuracyUSec)" = 1s ] && pass "and an accuracy of a second, so that ten minutes is the bound" || fail "AccuracyUSec=$(prop $T AccuracyUSec): the manager's default minute is added after the delay"
prop $DT TimersCalendar | grep -q 'Sun \*-\*-\* 03:30:00' && [ "$(prop $DT Persistent)" = yes ] && pass "the drill's timer is Sunday 03:30, catching up a missed one" || fail "the drill's timer: $(systemctl show -p TimersCalendar,Persistent $DT | tr '\n' ' ')"
[ "$(prop $DT RandomizedDelayUSec)" = 10min ] && [ "$(prop $DT FixedRandomDelay)" = yes ] && [ "$(prop $DT AccuracyUSec)" = 1s ] && pass "with the hourly's spread: a fixed delay of up to ten minutes, to the second (the owner, 2026-09-27)" || fail "the drill's timer: $(systemctl show -p RandomizedDelayUSec,FixedRandomDelay,AccuracyUSec $DT | tr '\n' ' ')"
[ "$(prop $T Unit)" = "$S" ] && [ "$(prop $DT Unit)" = "$D" ] && pass "each timer starts the service of its name" || fail "Unit=: $(prop $T Unit), $(prop $DT Unit)"
for f in $S $T $D $DT; do
	[ "$(stat -c '%U:%G %a' "/lib/systemd/system/$f")" = "root:root 644" ] && pass "$f is root's, 0644" || fail "$f: $(stat -c '%U:%G %a' "/lib/systemd/system/$f")"
done

echo "=== units 1: nothing starts before setup: a start with no credential file is skipped, not failed ==="
for u in $S $D; do
	systemctl start "$u" >"$OUT" 2>&1 && pass "$u: systemctl start exits 0 with no $ENVFILE" || fail "$u: systemctl start with no file exits $?: $(cat "$OUT")"
	[ "$(prop "$u" ConditionResult)" = no ] && pass "$u: the condition was what stopped it" || fail "$u: ConditionResult=$(prop "$u" ConditionResult): the row proves nothing"
	[ "$(prop "$u" ActiveState)" = inactive ] && [ "$(prop "$u" Result)" = success ] && pass "$u: inactive, not failed" || fail "$u: $(systemctl show -p ActiveState,Result "$u" | tr '\n' ' ')"
	journalctl --sync >/dev/null 2>&1
	journalctl -u "$u" --no-pager | grep -q "unmet condition" && pass "$u: the journal says the condition was not met" || fail "$u: the journal: $(journalctl -u "$u" --no-pager | tail -3)"
done
[ -z "$(failed_units)" ] && pass "no motel-backup unit is failed" || fail "failed: $(failed_units)"
[ "$(units_left)" = 0 ] && pass "and no unit of a run was started" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"

echo "=== units 2: a run as the shipped service works under its hardening, and its units are bound to it ==="
seed
write_env "$REPO" "$PASSWORD"
rr init -q || { echo "FATAL: could not initialise the repository at $REPO"; exit 1; }
big
systemctl start --no-block $S
if hold_restic "restic backup"; then
	[ "$(prop $S ActiveState)" = activating ] && pass "the service is activating while its run works" || fail "the service is $(prop $S ActiveState) with its restic held"
	upload=$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_upload_*' | awk '{print $1}' | head -1)
	[ -n "$upload" ] && [ "$(prop "$upload" BindsTo)" = "$S" ] && pass "the upload unit is BindsTo= the service ($upload)" || fail "the upload unit's BindsTo=: '$(prop "$upload" BindsTo)' ($upload)"
	systemctl kill --signal=SIGKILL $S
	i=0
	while [ "$(units_running)" != 0 ] && [ "$i" -lt 30 ]; do
		i=$((i + 1))
		sleep 1
	done
	[ "$i" -lt 30 ] && pass "SIGKILLed mid-upload, the service's units were ended by systemd within ${i}s" || fail "units still running 30s after the service was killed: $(systemctl list-units --plain --no-legend 'motel_backup_*')"
	pgrep -x restic >/dev/null && fail "restic is still running" || pass "and restic is gone"
	until_state $S failed 10 && [ "$(prop $S Result)" = signal ] && pass "the service is failed, by the signal" || fail "the killed service: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ')"
	systemctl reset-failed $S
else
	fail "no restic appeared under the service: is the run refused under its hardening? $(journalctl -u $S --no-pager | tail -5)"
	systemctl stop $S 2>/dev/null
	systemctl reset-failed $S 2>/dev/null
fi
# Stopped, not killed — what an upgrade and a remove do: the run ends
# its units and removes its plaintext itself, though its service has a
# stop job queued by then, which is why the units that remove
# plaintext are bound to nothing [M62].
systemctl start --no-block $S
if hold_restic "restic backup"; then
	[ "$(staged)" != 0 ] && pass "fixture: a copy is in staging while the upload is held" || fail "fixture: nothing is staged: the stop below proves nothing of plaintext"
	systemctl stop $S
	[ "$(units_running)" = 0 ] && pass "stopped mid-upload, the service's units are gone" || fail "units still running after the stop: $(systemctl list-units --plain --no-legend 'motel_backup_*')"
	[ "$(staged)" = 0 ] && pass "and the run removed its plaintext on its way out, with no later run to tidy up" || fail "left in staging by a stopped run: $(find /var/lib/motel-backup/staging -mindepth 2) — $(journalctl -u $S --no-pager | tail -4)"
	pgrep -x restic >/dev/null && fail "restic is still running" || pass "and restic is gone"
else
	fail "no restic appeared under the service: the stop row proves nothing"
	systemctl stop $S 2>/dev/null
fi
systemctl reset-failed $S 2>/dev/null
as_app rm -f /var/lib/liveswap/blog/shared/uploads/big.bin
t0=$(date +%s)
if systemctl start $S >"$OUT" 2>&1; then pass "a run as the service exits 0 [M54/M55: the hardening set is what a run needs]"; else fail "a run as the service failed: $(cat "$OUT") — $(journalctl -u $S --no-pager | tail -20)"; fi
[ "$(prop $S Result)" = success ] && pass "and the service ended success" || fail "Result=$(prop $S Result)"
expect_class blog ok "as the service"
expect_class shop ok "as the service"
[ "$(stat -c %Y "$STATUS")" -ge "$t0" ] && pass "the record was written by that run" || fail "the record is older than the run"
[ -n "$(proven blog)" ] && pass "the first drill proved blog's restore, under the same hardening" || fail "no proof of blog after its first run as the service: $(app_json blog)"
nothing_left "as the service"
[ "$(find /run/motel-backup -mindepth 1 -type d 2>/dev/null | wc -l)" = 0 ] && pass "no mount point of the run is left under /run/motel-backup" || fail "left under /run/motel-backup: $(find /run/motel-backup -mindepth 1)"

echo "=== units 3: a run or a drill that fails is a failed unit, and the journal says why ==="
mv /usr/bin/restic /usr/bin/restic.aside
systemctl start $S >"$OUT" 2>&1 && fail "the service exited 0 without restic" || pass "the service exits non-zero without restic"
[ "$(prop $S Result)" = exit-code ] && [ "$(failed_units)" = "$S " ] && pass "the run's service is failed, in systemctl --failed" || fail "after a run without restic: $(systemctl show -p ActiveState,Result $S | tr '\n' ' '); failed: $(failed_units)"
journalctl --sync >/dev/null 2>&1
journalctl -u $S --no-pager | grep -q "restic, the backup program motel-backup runs, is not installed at /usr/bin/restic: apt install restic" && pass "and the journal names the package to install, in setup's words" || fail "the journal: $(journalctl -u $S --no-pager | tail -5)"
mv /usr/bin/restic.aside /usr/bin/restic
systemctl reset-failed $S
[ "$(units_left)" = 0 ] && pass "no unit was started for it" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
mkdir -p /root/bare && echo x >/root/bare/f
crafted=$(craft blog /root/bare)
[ -n "$crafted" ] && pass "fixture: blog's newest snapshot is one with no plan in it" || fail "fixture: nothing was crafted: the drill's row proves nothing"
systemctl start $D >"$OUT" 2>&1 && fail "the drill's service exited 0 with a snapshot it cannot prove" || pass "the drill's service exits non-zero when it proved nothing"
[ "$(prop $D Result)" = exit-code ] && [ "$(failed_units)" = "$D " ] && pass "the drill's service is failed, in systemctl --failed [the owner, 2026-09-26]" || fail "after a drill that proved nothing: $(systemctl show -p ActiveState,Result $D | tr '\n' ' '); failed: $(failed_units)"
journalctl --sync >/dev/null 2>&1
journalctl -u $D --no-pager | grep -q "blog: restore not proven" && pass "and the journal says which app, and why" || fail "the journal: $(journalctl -u $D --no-pager | tail -5)"
forget "$crafted"
systemctl reset-failed $D
if systemctl start $D >"$OUT" 2>&1; then pass "a drill as the service exits 0 once the newest snapshot is a run's"; else fail "the drill's service: $(cat "$OUT") — $(journalctl -u $D --no-pager | tail -20)"; fi
[ "$(prop $D Result)" = success ] && [ -z "$(failed_units)" ] && pass "and it is not failed any more" || fail "after a drill that proved: $(systemctl show -p ActiveState,Result $D | tr '\n' ' '); failed: $(failed_units)"
nothing_left "after the drills"

echo "=== units 4: the drill waits for a run under way; a run meeting a drill says so ==="
systemctl reset-failed 2>/dev/null
big
systemctl start --no-block $S
if hold_restic "restic backup"; then
	systemctl start --no-block $D
	sleep 2
	systemctl list-jobs --plain --no-legend >"$OUT" 2>&1
	grep -q "$D *start *waiting" "$OUT" && pass "the drill's start job waits behind the run's [M57]" || fail "jobs: $(cat "$OUT"); the drill: $(systemctl show -p ActiveState,Result $D | tr '\n' ' ')"
	[ "$(prop $D ActiveState)" = inactive ] && pass "and the drill has not started" || fail "the drill is $(prop $D ActiveState) while the run's restic is held"
	kill -CONT "$pid"
	until_state $S inactive 180 && [ "$(prop $S Result)" = success ] && pass "the run ended success once released" || fail "the run: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ')"
	until_state $D inactive 180 && [ "$(prop $D Result)" = success ] && pass "and the drill ran after it, and ended success" || fail "the drill: $(systemctl show -p ActiveState,Result $D | tr '\n' ' ') — $(journalctl -u $D --no-pager | tail -5)"
	journalctl --sync >/dev/null 2>&1
	journalctl -u $D --no-pager | grep -q "another backup run is in progress" && fail "the drill met the run's lock instead of waiting" || pass "the drill never met the run's lock"
else
	fail "no restic appeared under the service: the row proves nothing"
	systemctl stop $S $D 2>/dev/null
fi
as_app rm -f /var/lib/liveswap/blog/shared/uploads/big.bin
systemctl reset-failed 2>/dev/null
systemctl start --no-block $D
if hold_restic "restic restore"; then
	systemctl start $S >"$OUT" 2>&1 && fail "a run's service exited 0 while a drill held the lock" || pass "a run's service that meets a drill exits non-zero"
	journalctl --sync >/dev/null 2>&1
	journalctl -u $S --no-pager | grep -q "another backup run is in progress" && [ "$(prop $S Result)" = exit-code ] && pass "saying so; the unit is failed for that hour (the owner, 2026-09-26: a red unit, not a wait)" || fail "the run meeting a drill: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ') — $(journalctl -u $S --no-pager | tail -3)"
	kill -CONT "$pid"
	until_state $D inactive 180 && pass "the drill went on to its end" || fail "the drill: $(systemctl show -p ActiveState,Result $D | tr '\n' ' ')"
	systemctl reset-failed $S
else
	fail "no restic restore appeared under the drill's service: the reverse row proves nothing"
	systemctl stop $S $D 2>/dev/null
	systemctl reset-failed 2>/dev/null
fi
nothing_left "after the two orders"

echo "=== units 5: the timers as written, and one firing ==="
[ ! -f "/var/lib/systemd/timers/stamp-$T" ] && pass "fixture: the hourly timer has no stamp yet" || fail "fixture: the timer has a stamp already: its first activation below proves nothing"
started0=$(prop $S ExecMainStartTimestampMonotonic)
systemctl start $T $DT
sleep 2
# A timer with Persistent=true and no stamp writes its stamp at its
# first activation and fires nothing [M61]: a later activation catches
# up from that stamp only if an elapse was missed meanwhile. The
# manager then shows the stamp's time as LastTriggerUSec, which is no
# firing: what says the service ran is its main process's start time.
[ -f "/var/lib/systemd/timers/stamp-$T" ] && [ "$(prop $S ExecMainStartTimestampMonotonic)" = "$started0" ] && pass "a persistent timer's first activation writes its stamp and fires nothing" || fail "the first activation: stamp $([ -f /var/lib/systemd/timers/stamp-$T ] && echo yes || echo no), the service $(systemctl show -p ActiveState,ExecMainStartTimestampMonotonic $S | tr '\n' ' ')"
next=$(prop $T NextElapseUSecRealtime)
next_s=$(date -d "$next" +%s 2>/dev/null)
now=$(date +%s)
[ -n "$next_s" ] && [ "$((next_s - now))" -le 4200 ] && [ "$((next_s - now))" -gt 0 ] && pass "the hourly timer's next elapse is within the hour and ten minutes ($next)" || fail "next elapse: '$next' (now $(date))"
[ "$(date -d "$next" +%M)" -le 10 ] && pass "and at most ten minutes past the hour" || fail "next elapse minute: $(date -d "$next" +%M)"
systemctl daemon-reload
systemctl restart $T
# By its minute and second: across the top of an hour the next elapse
# moves an hour, and the offset is what stays.
[ "$(date -d "$(prop $T NextElapseUSecRealtime)" +%M:%S)" = "$(date -d "$next" +%M:%S)" ] && pass "the offset is this box's own, the same after a reload and a restart [M60]" || fail "the offset moved: $next → $(prop $T NextElapseUSecRealtime)"
until_state $S inactive 180 || fail "a run the restart caught up did not end: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ')"
dnext=$(prop $DT NextElapseUSecRealtime)
dpast=$(($(date -d "$dnext" +%s) - $(date -d "$(date -d "$dnext" '+%Y-%m-%d') 03:30:00" +%s)))
[ "$(date -d "$dnext" +%a)" = Sun ] && [ "$dpast" -ge 0 ] && [ "$dpast" -le 600 ] && pass "the drill's next elapse is a Sunday, between 03:30 and 03:40 ($dnext)" || fail "the drill's next elapse: '$dnext' (${dpast}s past 03:30)"
systemctl restart $DT
[ "$(prop $DT NextElapseUSecRealtime)" = "$dnext" ] && pass "and its offset is this box's own, the same after a reload and a restart" || fail "the drill's offset moved: $dnext → $(prop $DT NextElapseUSecRealtime)"
# One real firing on the calendar, from a drop-in that makes the timer
# a minute's; the service it starts is the shipped one, with the
# credential file there.
mkdir -p /run/systemd/system/$T.d
printf '[Timer]\nOnCalendar=\nOnCalendar=*-*-* *:*:00\nRandomizedDelaySec=0\n' >/run/systemd/system/$T.d/10-units-suite.conf
systemctl daemon-reload
t0=$(date +%s)
started0=$(prop $S ExecMainStartTimestampMonotonic)
systemctl restart $T
i=0
until [ "$(prop $S ExecMainStartTimestampMonotonic)" != "$started0" ] || [ "$i" -ge 750 ]; do
	i=$((i + 1))
	sleep 0.1
done
[ "$i" -lt 750 ] && pass "the timer fired within $((i / 10))s, and the service started [M61]" || fail "no firing within 75s: $(systemctl show -p LastTriggerUSec,NextElapseUSecRealtime,ActiveState $T | tr '\n' ' '); the service $(systemctl show -p ActiveState,ExecMainStartTimestampMonotonic $S | tr '\n' ' ')"
until_state $S inactive 180 && [ "$(prop $S Result)" = success ] && pass "and the run it started ended success" || fail "the run the timer started: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ') — $(journalctl -u $S --no-pager | tail -5)"
[ "$(stat -c %Y "$STATUS")" -ge "$t0" ] && pass "and wrote the record" || fail "the record is older than the firing"
[ -f /var/lib/systemd/timers/stamp-$T ] && pass "the timer keeps a stamp, so a missed elapse is caught up at boot" || fail "no stamp file under /var/lib/systemd/timers"
rm -rf /run/systemd/system/$T.d
systemctl stop $T $DT
systemctl daemon-reload
[ "$(prop $T ActiveState)" = inactive ] && [ "$(prop $DT ActiveState)" = inactive ] && pass "the timers are stopped again for the suites after this one" || fail "timers: $(systemctl show -p ActiveState $T $DT | tr '\n' ' ')"
nothing_left "after the firing"

echo "=== units 6: a run in a mount namespace of its own refuses, before any upload ==="
# What the unit files must never give the service is a mount namespace
# of its own: the manager then cannot see the run's mounts, and binds
# the bare mount point — root's, empty — into the units in place of the
# app's data [M54]. The unit table holds the files to that; a drop-in
# is an administrator's to write. The run makes one mount of its own,
# asks the manager whether it sees it, and refuses where it does not
# [M72]: nothing is uploaded, so no snapshot of empty directories
# becomes the newest one.
before=$(snapshots)
mkdir -p /run/systemd/system/$S.d
printf '[Service]\nPrivateNetwork=yes\n' >/run/systemd/system/$S.d/10-units-suite.conf
systemctl daemon-reload
[ "$(prop $S PrivateNetwork)" = yes ] && pass "fixture: the service has a namespace of its own" || fail "fixture: PrivateNetwork=$(prop $S PrivateNetwork): the row proves nothing"
systemctl start $S >"$OUT" 2>&1 && fail "a run in a mount namespace of its own exited 0" || pass "a run in a mount namespace of its own exits non-zero"
journalctl --sync >/dev/null 2>&1
journalctl -u $S --no-pager | grep -q "this command runs in a mount namespace of its own: the manager does not see the mounts it makes" && pass "saying that the manager does not see its mounts" || fail "the journal: $(journalctl -u $S --no-pager | tail -5)"
journalctl -u $S --no-pager | grep -q "$S is given one by a property it must not have .* systemctl cat $S shows it" && pass "and where to look: systemctl cat, by the unit's name" || fail "the journal names no remedy: $(journalctl -u $S --no-pager | tail -3)"
[ "$(snapshots)" = "$before" ] && pass "nothing was uploaded: the repository holds the $before snapshots it held" || fail "the repository held $before snapshots and holds $(snapshots): a run that refused uploaded"
grep -q "runs in a mount namespace of its own" "$STATUS" && pass "and the record says why the run ended" || fail "the record: $(tr -d '\n' <"$STATUS" | cut -c1-300)"
[ "$(units_left)" = 0 ] && pass "no unit was started for it" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
grep -q " /run/motel-backup/" /proc/self/mountinfo && fail "the run left its own mount behind: $(grep ' /run/motel-backup/' /proc/self/mountinfo)" || pass "and the mount it asked about is gone"
rm -rf /run/systemd/system/$S.d
systemctl daemon-reload
systemctl reset-failed $S 2>/dev/null
if systemctl start $S >"$OUT" 2>&1; then pass "without the namespace the run works again"; else fail "the run after the drop-in went: $(journalctl -u $S --no-pager | tail -10)"; fi
expect_class blog ok "without the namespace"
# Behind that refusal the snapshot is held to the file that was given,
# by its inode and owner [M63] (the engine's table, and the
# integration lane against restic); what that must not cost: a
# declared directory that is empty is the one given, and is backed up.
as_app sh -c 'mkdir /var/lib/liveswap/blog/shared/empty'
sed -i 's|^\(\t*\)files  *uploads$|&\n\1files empty|' "$CADDYFILE"
if grep -q 'files empty' "$CADDYFILE"; then
	systemctl start $S >"$OUT" 2>&1 && pass "a run with a declared directory that is empty exits 0" || fail "a run with an empty directory declared: $(journalctl -u $S --no-pager | tail -10)"
	expect_class blog ok "with an empty directory, which is the one given"
else
	fail "fixture: no 'files uploads' line in $CADDYFILE to declare an empty directory beside"
fi
cp /root/Caddyfile.base "$CADDYFILE"
as_app rmdir /var/lib/liveswap/blog/shared/empty
nothing_left "after the namespace"

echo "=== units 7: a disk inside an app's data is backed up with it, and stays where the operator mounted it ==="
# On a box systemd has booted the root mount is shared; in a container
# it is left private, and what a shared mount does is never seen. A
# run binds an app's data recursively, so that a disk mounted inside
# comes along — and a recursive bind of a shared mount is its peer:
# taken away as it was, it took the operator's disk from under the
# live app with it, at the end of every run [M71].
mount --make-rshared /
[ "$(findmnt -no PROPAGATION /)" = shared ] && pass "fixture: the root mount is shared, as a booted box has it" || fail "fixture: the root mount is $(findmnt -no PROPAGATION /)"
mkdir -p /root/disk
echo on-the-disk >/root/disk/kept.txt
chown -R motel:motel /root/disk
as_app mkdir /var/lib/liveswap/blog/shared/uploads/disk
mount --bind /root/disk /var/lib/liveswap/blog/shared/uploads/disk
mountpoint -q /var/lib/liveswap/blog/shared/uploads/disk && pass "fixture: a disk is mounted inside blog's uploads" || fail "fixture: no disk is mounted: the row proves nothing"
if systemctl start $S >"$OUT" 2>&1; then pass "a run with a disk inside an app's data exits 0"; else fail "the run: $(journalctl -u $S --no-pager | tail -10)"; fi
expect_class blog ok "with a disk inside its data"
mountpoint -q /var/lib/liveswap/blog/shared/uploads/disk && [ -f /var/lib/liveswap/blog/shared/uploads/disk/kept.txt ] && pass "the disk is still mounted where the operator put it" || fail "the run unmounted the operator's disk from under the app: $(grep liveswap/blog /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')"
rr ls --no-lock "$(newest blog)" 2>/dev/null | grep -q '^/backup/blog/files/uploads/disk/kept.txt$' && pass "and what is on it is in the snapshot" || fail "the snapshot does not hold the disk's file: $(rr ls --no-lock "$(newest blog)" 2>&1 | grep uploads | head -5)"
grep -q " /run/motel-backup/" /proc/self/mountinfo && fail "mounts of the run are left: $(grep ' /run/motel-backup/' /proc/self/mountinfo | awk '{print $5}' | tr '\n' ' ')" || pass "and nothing of the run's is left mounted"
umount /var/lib/liveswap/blog/shared/uploads/disk
as_app rmdir /var/lib/liveswap/blog/shared/uploads/disk
rm -rf /root/disk
nothing_left "after the disk"

echo "=== units 8: a helper of another version does nothing for a run under way ==="
# An upgrade leaves a run alone (the owner, 2026-09-27), and the
# helpers it starts from then on are the new program's: each is told
# which program started it, and one that is another does nothing and
# says so. Staged as dpkg replaces a file — written beside, renamed
# over — with a program that differs by one byte, while blog's upload
# is held: blog's upload finishes and its plaintext is removed (the
# clean helper works for any version); shop's dump, the next helper,
# is the other program's.
seed
big
systemctl reset-failed $S 2>/dev/null
systemctl start --no-block $S
if hold_restic "restic backup"; then
	cp /usr/bin/motel-backup /usr/bin/motel-backup.suite-aside
	cp /usr/bin/motel-backup /usr/bin/.motel-backup.new
	printf x >>/usr/bin/.motel-backup.new
	mv /usr/bin/.motel-backup.new /usr/bin/motel-backup
	kill -CONT "$pid"
	until_state $S failed 240 && pass "the run whose helper was another version's is a failed unit" || fail "the run: $(systemctl show -p ActiveState,Result $S | tr '\n' ' ') — $(journalctl -u $S --no-pager | tail -5)"
	expect_class blog ok "with its upload under way at the upgrade"
	expect_class shop failed "whose helper was another version's"
	app_json shop | grep -q "the package was upgraded while this command was under way: its dump helper is another version's, and did nothing" && pass "shop says the package was upgraded mid-run" || fail "shop: $(app_json shop)"
	journalctl -u "motel_backup_dump_shop_*" --no-pager 2>/dev/null | grep -q "this helper is not the version of the command that started it" && pass "and the helper said so in its own journal" || fail "the dump helper's journal: $(journalctl --no-pager | grep -i 'dump_shop' | tail -3)"
	mv /usr/bin/motel-backup.suite-aside /usr/bin/motel-backup
	systemctl reset-failed $S
	if systemctl start $S >"$OUT" 2>&1; then pass "the next run, of one version, works"; else fail "the next run: $(journalctl -u $S --no-pager | tail -10)"; fi
	expect_class shop ok "the next run"
else
	fail "no restic appeared under the service: the row proves nothing"
	systemctl stop $S 2>/dev/null
fi
as_app rm -f /var/lib/liveswap/blog/shared/uploads/big.bin
nothing_left "after the other version"

rm -rf /root/bare
systemctl reset-failed 2>/dev/null
cp /root/Caddyfile.base "$CADDYFILE"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL UNITS SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES UNITS ASSERTION(S) FAILED"
exit 1
