#!/bin/sh
# Runs INSIDE the e2e-motel container (systemd as PID 1), after the
# main suites and before the recovery suite: the assertions that need
# systemctl, journalctl and the process tree, which the runner
# container on the compose network cannot see. Proves the systemd
# runner's contract end to end: the app is a transient unit under the
# motel user's manager, it survives motel restarts and motel's
# own deaths — which the packaged unit undoes on its own, while a start
# motel refuses stays down — stopping a version takes its whole
# process tree, a crash leaves nothing behind, and app output is in
# the journal.
set -u

PROXY="http://127.0.0.1:8080"
HOOK="http://127.0.0.1:8081/demo"
ART="http://e2e-artifacts:8080/artifacts"
. /lib.sh
wait_for_token

uid=$(id -u motel)
as_motel() { su -s /bin/sh motel -c "$*"; }
# systemctl --user for a nologin system user: motel's own trick —
# its manager's private socket under /run/user/<uid>.
user_systemctl() { as_motel "XDG_RUNTIME_DIR=/run/user/$uid systemctl --user $*"; }

wait_hook() { # -> 0 once the webhook answers 200, 1 after 30s
	i=0
	until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 -H "Authorization: Bearer $TOKEN" "$HOOK")" = "200" ]; do
		i=$((i + 1))
		[ "$i" -ge 30 ] && return 1
		sleep 1
	done
}

# motel.service itself, as systemd sees it. The crash rows below
# read the unit's own record — Result, ExecMainCode/Status, NRestarts —
# never a systemctl command's exit status: Caddy sends READY=1 before
# it knows whether the config loaded, so `systemctl start` can return
# 0 for a start that then exits 1.
hs_prop() { systemctl show -p "$1" --value motel; }
hs_shape() { systemctl show -p ActiveState,SubState,Result,NRestarts motel | tr '\n' ' '; }
wait_hs_substate() { # <glob> -> 0 once SubState matches (10s)
	i=0
	until case "$(hs_prop SubState)" in $1) true ;; *) false ;; esac; do
		i=$((i + 1)); [ "$i" -ge 100 ] && return 1; sleep 0.1
	done
}
wait_hs_up() { # <old ExecMainPID> -> 0 once active/running on another main pid (60s)
	i=0
	until [ "$(hs_prop ActiveState)" = "active" ] && [ "$(hs_prop SubState)" = "running" ] \
		&& [ "$(hs_prop ExecMainPID)" != "$1" ] && [ "$(hs_prop ExecMainPID)" != "0" ]; do
		i=$((i + 1)); [ "$i" -ge 120 ] && return 1; sleep 0.5
	done
}
# crash_prelude gives the row its own start budget (the default start
# limit counts every start, and reset-failed also zeroes NRestarts) and
# the baseline every assertion is measured against — relative, never
# absolute.
crash_prelude() {
	systemctl reset-failed motel
	N0=$(hs_prop NRestarts); HP0=$(hs_prop ExecMainPID); T0=$(date +%s)
}
# failure_shape reads the death's record inside the RestartSec window,
# in one show call so the three values are one snapshot. <Result> may
# be two alternatives separated by |. ExecMainCode is the SIGCHLD code
# as `show` prints it: 1 = exited (status is the exit code), 2 = killed
# (status is the signal).
failure_shape() { # <label> <Result> <ExecMainCode> <ExecMainStatus>
	wait_hs_substate 'auto-restart*' \
		|| { fail "$1: not in auto-restart within 10s: $(hs_shape)"; return 1; }
	Result= ExecMainCode= ExecMainStatus=
	eval "$(systemctl show -p Result,ExecMainCode,ExecMainStatus motel)"
	case "|$2|" in *"|$Result|"*) rok=1 ;; *) rok=0 ;; esac
	[ "$rok" = 1 ] && [ "$ExecMainCode" = "$3" ] && [ "$ExecMainStatus" = "$4" ] \
		&& pass "$1: Result=$Result ExecMainCode=$ExecMainCode ExecMainStatus=$ExecMainStatus" \
		|| fail "$1: Result=$Result ExecMainCode=$ExecMainCode ExecMainStatus=$ExecMainStatus, want $2/$3/$4"
}
# came_back: systemd brought motel back exactly once, and it
# reattached to the instance section 1 recorded in $pid/$unit.
came_back() { # <label>
	wait_hs_up "$HP0" && pass "$1: motel back on its own (main pid $HP0 -> $(hs_prop ExecMainPID))" \
		|| fail "$1: motel not back within 60s: $(hs_shape)"
	n1=$(hs_prop NRestarts)
	[ "$n1" = "$((N0 + 1))" ] && pass "$1: NRestarts $N0 -> $n1 (one automatic restart, no loop)" \
		|| fail "$1: NRestarts $N0 -> $n1, want $((N0 + 1))"
	wait_hook || fail "$1: webhook not back within 30s"
	s=$(status)
	[ "$(json_num "$s" pid)" = "$pid" ] && [ "$(json_str "$s" unit)" = "$unit" ] \
		&& pass "$1: same app pid and unit (reattached, not relaunched)" || fail "$1: instance changed: $s"
	case "$s" in *'"running":true'*) pass "$1: reattached app running" ;; *) fail "$1: app not running: $s" ;; esac
}

echo "=== systemd 1: the app is a transient unit under user@$uid ==="
s=$(status)
unit=$(json_str "$s" unit)
pid=$(json_num "$s" pid)
if [ -n "$unit" ] && [ -n "$pid" ]; then
	pass "status names the unit ($unit, pid $pid)"
else
	fail "status lacks unit/pid: $s"
	echo "$FAILURES SYSTEMD ASSERTION(S) FAILED"
	exit 1
fi
systemctl is-active --quiet "user@$uid.service" && pass "user@$uid is active" || fail "user@$uid not active"
user_systemctl is-active --quiet "$unit" && pass "unit is active under the user manager" || fail "unit $unit not active"
[ "$(user_systemctl show -p MainPID --value "$unit")" = "$pid" ] \
	&& pass "unit MainPID matches the status pid" || fail "MainPID differs from status pid $pid"
# The command status reports is read back out of the manager's own
# ExecStart, so it is what the unit is running — not what the config
# says now, which a reload can change without restarting anything.
cmd=$(json_arr "$s" command)
cmd0=${cmd%% *}
if [ -z "$cmd0" ]; then
	fail "status reports no command: $s"
else
	case "$(user_systemctl show -p ExecStart --value "$unit")" in
	*"$cmd0"*) pass "status command is the unit's ExecStart ($cmd)" ;;
	*) fail "status command '$cmd' is not the unit's ExecStart" ;;
	esac
fi
[ "$(user_systemctl show -p Restart --value "$unit")" = "no" ] \
	&& pass "unit has Restart=no (apps are restarted by the watchdog, not systemd)" || fail "unit Restart is not 'no'"
[ "$(user_systemctl show -p KillMode --value "$unit")" = "control-group" ] \
	&& pass "unit has KillMode=control-group" || fail "unit KillMode is not control-group"
# motel's own unit is the other half of that rule: systemd restarts
# it after a crash, and never after an exit 1 (a start it refused).
[ "$(hs_prop Restart)" = "on-failure" ] \
	&& pass "motel.service has Restart=on-failure" || fail "motel.service Restart is '$(hs_prop Restart)'"
[ "$(hs_prop RestartPreventExitStatus)" = "1" ] \
	&& pass "motel.service never retries an exit 1" || fail "motel.service RestartPreventExitStatus is '$(hs_prop RestartPreventExitStatus)'"

echo "=== systemd 2: systemctl restart motel keeps the app running ==="
systemctl restart motel || fail "systemctl restart motel failed"
wait_hook || fail "webhook not back within 30s of restart"
s=$(status)
if [ "$(json_num "$s" pid)" = "$pid" ] && [ "$(json_str "$s" unit)" = "$unit" ]; then
	pass "same pid and unit after restart (reattached, not relaunched)"
else
	fail "instance changed across restart: $s"
fi
# A reattached unit has no launch in the new process to remember, so
# the command can only be read back from the manager — the case where
# a status that reported what motel started would go blank.
[ "$(json_arr "$s" command)" = "$cmd" ] \
	&& pass "the reattached instance still reports its command" \
	|| fail "command lost across reattach: $s"
case "$(body)" in "hello "*) pass "proxy serves after restart" ;; *) fail "proxy not serving after restart" ;; esac

echo "=== systemd 3: SIGKILL of motel: systemd brings it back, and it reattaches ==="
# No `systemctl start` here: one inside the RestartSec window would
# pre-empt the automatic restart and the row would prove nothing.
crash_prelude
systemctl kill --kill-whom=main -s SIGKILL motel
failure_shape "SIGKILL" signal 2 9
came_back "SIGKILL"
journalctl -u motel --no-pager --since "@$T0" | grep -q "code=killed, status=9/KILL" \
	&& pass "SIGKILL: the journal records the unclean death" || fail "SIGKILL: no killed/9 line in the journal since $T0"

echo "=== systemd 3b: motel exits 2 (SIGQUIT is Caddy's force-quit, a panic's code): systemd brings it back ==="
# The row that pins "a panic is restarted": Restart=on-abnormal would
# leave an unclean exit code down.
crash_prelude
systemctl kill --kill-whom=main -s SIGQUIT motel
failure_shape "exit 2" exit-code 1 2
came_back "exit 2"

echo "=== systemd 3c: motel OOM-killed: systemd brings it back ==="
# The limit is written into the unit's live cgroup, not set as a
# runtime property: a property would survive the restart and kill the
# next process too. systemd prunes the empty cgroup on death and makes
# a fresh one for the restart, so the trigger should clear itself; it
# is also lifted by hand once the death is on record, so a kernel or
# systemd that kept the cgroup cannot turn this row into a restart
# loop — which is why the last check asserts only that the restarted
# unit runs unlimited, not who lifted the limit. Swap is closed first:
# a runner with a swapfile would thrash instead of dying.
crash_prelude
hcg=$(hs_prop ControlGroup)
if [ -w "/sys/fs/cgroup$hcg/memory.max" ]; then
	echo 0 > "/sys/fs/cgroup$hcg/memory.swap.max" 2>/dev/null || true
	echo 8M > "/sys/fs/cgroup$hcg/memory.max"
	# Whether systemd files it as oom-kill (it saw the cgroup's
	# memory.events first) or as the SIGKILL the kernel delivered is a
	# race it is entitled to; the journal line below is the OOM proof.
	failure_shape "OOM" "oom-kill|signal" 2 9
	if [ -e "/sys/fs/cgroup$hcg/memory.max" ]; then
		echo max > "/sys/fs/cgroup$hcg/memory.max" 2>/dev/null
		echo max > "/sys/fs/cgroup$hcg/memory.swap.max" 2>/dev/null
	fi
	came_back "OOM"
	journalctl -u motel --no-pager --since "@$T0" | grep -qi "oom" \
		&& pass "OOM: the journal records the OOM kill" || fail "OOM: no oom line in the journal since $T0"
	[ "$(cat "/sys/fs/cgroup$(hs_prop ControlGroup)/memory.max")" = "max" ] \
		&& pass "OOM: the restarted unit runs unlimited" \
		|| fail "OOM: memory.max is still $(cat "/sys/fs/cgroup$(hs_prop ControlGroup)/memory.max") after the restart"
else
	fail "OOM: no writable memory controller at /sys/fs/cgroup$hcg — the row would prove nothing"
fi

echo "=== systemd 4: the app runs in its sandbox — the view from inside ==="
# Host-side facts first (never assertions): they explain a refused
# start on a kernel that restricts unprivileged user namespaces.
# Debian 13 does not restrict them, but the e2e container runs on
# whatever kernel the CI runner has, so record what that kernel allows.
echo "host: $(uname -r); apparmor_restrict_unprivileged_userns=$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)"
echo "userns: max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || echo absent)"
# The probe release's ./server sources the shared view probe
# (liveswap/testdata/sandbox-view.sh, one asset with liveswap's integration test and
# packaging/test/smoke.sh) to record its view into its shared dir, then
# becomes the app. The image is trixie
# (systemd 257) — the only release in the support matrix — so the
# sandbox is the whole set: a PID namespace on top of the user
# namespace and the mount set. The view from inside is the assertion;
# there is no status field to consult, because there is nothing to
# report — every unit gets this.
# A second app on disk, so root_listing below is a real sibling check.
# With only demo there, binding the WHOLE liveswap root into the unit
# would still list exactly "demo" and pass. This one is deliberately
# not in the config: it exists to be invisible, and its secret is what
# a leaking view would expose.
mkdir -p /var/lib/liveswap/other/shared
: > /var/lib/liveswap/other/shared/secret
chown -R motel:motel /var/lib/liveswap/other

code=$(deploy demo-probe.tar.gz sb1)
[ "$code" = "200" ] || fail "deploy of the probe release returned $code: $(body)"
probe=/var/lib/liveswap/demo/shared/view.txt
i=0
until grep -q '^done=1' "$probe" 2>/dev/null; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5
done
grep -q '^done=1' "$probe" || fail "view.txt not written by the sandboxed app"
probe_val() { sed -n "s/^$1=//p" "$probe" | head -1; }
expect_probe() { # <key> <want>
	got=$(probe_val "$1")
	[ "$got" = "$2" ] && pass "inside the sandbox: $1=$2" || fail "inside the sandbox: $1=$got, want $2"
}
expect_probe pid 1
[ "$(probe_val uidmap)" != "4294967295" ] \
	&& pass "inside the sandbox: own user namespace (uid_map $(probe_val uidmap))" \
	|| fail "no user namespace: uid_map covers the whole id space"
n=$(probe_val nprocs)
[ -n "$n" ] && [ "$n" -le 8 ] \
	&& pass "inside the sandbox: /proc lists $n pids (own PID namespace)" \
	|| fail "inside the sandbox: /proc lists $n pids — the host's, not a PID namespace"
[ -d /var/lib/liveswap/other ] \
	|| fail "the sibling app fixture is missing; root_listing would prove nothing"
[ "$(probe_val root_listing)" = "demo " ] \
	&& pass "inside the sandbox: the liveswap root shows only this app" \
	|| fail "inside the sandbox: root listing is '$(probe_val root_listing)', want only 'demo'"
expect_probe state closed
expect_probe apptmp closed
expect_probe current closed
expect_probe release writable
expect_probe root readonly
expect_probe motel_lib closed
# Real here, unlike the admin socket below: the packaged unit creates
# /run/motel through RuntimeDirectory=. Guarded so "closed" cannot
# come from the directory simply not existing.
[ -d /run/motel ] \
	|| fail "/run/motel does not exist on the host; the in-unit probe would prove nothing"
expect_probe run_motel closed
# The manager socket is only meaningfully "closed" if the probe looked
# at the real path: under a deny-by-default view /run/user/<any uid> is
# absent, so a wrong uid would pass this. Pin the uid the probe used,
# and prove the socket is there to be hidden.
expect_probe uid "$uid"
[ -e "/run/user/$uid/systemd/private" ] \
	|| fail "the manager socket does not exist on the host; the in-unit probe would prove nothing"
expect_probe mgr_socket closed
expect_probe etc_motel closed
# admin_socket is deliberately NOT asserted here. This config puts the
# admin API on TCP (e2e/Caddyfile: test-only, so the runner can drive
# /load over the network), so /run/motel/admin.sock is never
# created and "closed" would pass on a path that never existed.
# run_motel above already proves the whole directory is out of the
# view, and install-test asserts the socket itself against a real one.
# The manager's /proc is not checked from in here: this lane has no way
# to hand the probe a runtime pid (its Caddyfile is baked at image
# build), so the probe reports "skipped" rather than a vacuous "closed".
# Those two routes are covered from outside the unit further down
# (denied_to_motel), and from inside by the integration and smoke
# lanes, which can pass MGR_PID.
expect_probe mgr_root skipped
expect_probe mgr_environ skipped
expect_probe cgroup readonly
# The demo app's caps (e2e/Caddyfile: memory_max 256MB, tasks_max 256,
# cpu_quota 150%), as the kernel holds them in the unit's own cgroup,
# read from inside it — where, per the row above, they cannot be
# changed. memory_max is 256MB in SI (humanize), so 256000000 bytes.
expect_probe cgroup_memory_max present
expect_probe cgroup_memory_max_value 256000000
expect_probe cgroup_pids_max 256
expect_probe cgroup_cpu_max "150000 100000"
expect_probe tmp writable
expect_probe home /var/lib/liveswap/demo/shared
expect_probe xdg_runtime unset
expect_probe acme_token absent
expect_probe dns ok
# The base view: an OS the app can run on. Asserted first, because
# without it every "absent" below would pass on an empty unit.
expect_probe binsh ok
expect_probe usrbinenv ok
expect_probe etcssl ok
expect_probe sslprivate absent
expect_probe resolvconf ok
# Deny-by-default: nothing bound these, so they do not exist inside the
# unit — not "present but inaccessible", which is what the hidden-set
# model left behind and what could go stale between deploys.
for d in opt srv home root mnt media etcliveswap; do
	expect_probe "abs_$d" absent
done
# /var/lib is the exception: the liveswap root lives under it, so it is
# present and varlib_listing below is the real check. motel's own
# state dir next door is motel_lib above.
expect_probe abs_varlib present
# /etc is named entry by entry, never bound whole: an app that could
# list all of /etc would see every other app's env_file.
[ -n "$(probe_val etc_listing)" ] \
	|| fail "inside the sandbox: /etc lists nothing at all — the base view is empty, not narrow"
case " $(probe_val etc_listing)" in
*" motel "* | *" liveswap "* | *" shadow "*)
	fail "inside the sandbox: /etc carries more than the named entries: $(probe_val etc_listing)" ;;
*) pass "inside the sandbox: /etc is only the named entries ($(probe_val etc_listing))" ;;
esac
# And /var/lib holds nothing but the path to this app's own dirs — the
# TLS keys next door on the host are not in the view at all.
[ "$(probe_val varlib_listing)" = "liveswap " ] \
	&& pass "inside the sandbox: /var/lib shows only the liveswap root" \
	|| fail "inside the sandbox: /var/lib listing is '$(probe_val varlib_listing)', want only 'liveswap'"
# Sibling IPC (#311): a mqueuefs the unit mounted itself (its own IPC
# namespace), and a /dev/shm of its own at the kernel's half of RAM —
# systemd's own default for a TemporaryFileSystem= would be 10% — that
# nothing written inside reaches on the host.
expect_probe mqueue_owner self
expect_probe shm_write ok
shm_kb=$(probe_val shm_size_kb)
mem_kb=$(probe_val memtotal_kb)
if [ -n "$shm_kb" ] && [ -n "$mem_kb" ] && [ "$mem_kb" -gt 0 ] \
	&& [ $((shm_kb * 100 / mem_kb)) -ge 49 ] && [ $((shm_kb * 100 / mem_kb)) -le 50 ]; then
	pass "inside the sandbox: /dev/shm is ${shm_kb} KiB of ${mem_kb} KiB (the host's 50%, not systemd's 10%)"
else
	fail "inside the sandbox: /dev/shm is '${shm_kb}' KiB of '${mem_kb}' KiB, want 50%"
fi
[ ! -e /dev/shm/sandbox-view-canary ] \
	&& pass "a file written to /dev/shm inside the unit is not on the host" \
	|| fail "/dev/shm/sandbox-view-canary written inside the unit is on the host: /dev/shm is shared"
# The sandboxed app still serves through motel like any other.
curl -fsS --max-time 5 http://127.0.0.1:8080/ | grep -q "probe" \
	&& pass "the sandboxed release serves through the proxy" \
	|| fail "the sandboxed release does not serve"
# And the unit carries the properties the runner set.
sunit=$(json_str "$(status)" unit)
props=$(user_systemctl show "$sunit" -p PrivatePIDs,PrivateUsers,PrivateIPC,ProtectSystem,ProtectControlGroups,TemporaryFileSystem,InaccessiblePaths,LimitCORE,LimitCORESoft,SystemCallArchitectures)
for want in "PrivatePIDs=yes" "PrivateUsers=yes" "PrivateIPC=yes" "ProtectControlGroups=yes" "TemporaryFileSystem=/:ro" "/dev/shm:mode=1777,size=50%" "LimitCORE=0" "LimitCORESoft=0" "SystemCallArchitectures=native"; do
	case "$props" in
	*"$want"*) pass "unit property $want" ;;
	*) fail "unit lacks $want: $props" ;;
	esac
done
# And the caps as the manager holds them (the kernel's view is the
# probe's rows above): memory_max brings no swap with it, and status
# reports the three in the directives' own units.
caps=$(user_systemctl show "$sunit" -p MemoryMax,MemorySwapMax,TasksMax)
for want in "MemoryMax=256000000" "MemorySwapMax=0" "TasksMax=256"; do
	case "$caps" in
	*"$want"*) pass "unit property $want" ;;
	*) fail "unit lacks $want: $caps" ;;
	esac
done
s=$(status)
if [ "$(json_num "$s" memory_max)" = "256000000" ] && [ "$(json_num "$s" tasks_max)" = "256" ] && [ "$(json_num "$s" cpu_quota)" = "150" ]; then
	pass "status reports the caps (memory_max, tasks_max, cpu_quota)"
else
	fail "status does not report the caps: $s"
fi
# And names none as unenforced: this host delegates every controller
# the three caps need (the probe's rows above read the kernel's values).
case "$s" in
*caps_unenforced*) fail "status names a cap the host does not enforce: $s" ;;
*) pass "status names no unenforced cap: every controller reaches the unit" ;;
esac
# The syscall filter the manager built: an allow list (no leading ~)
# that still holds io_uring_enter, the rest of @aio, and not
# io_uring_setup, whose ring would open sockets RestrictAddressFamilies=
# never sees. Every app in this suite — Node, Deno, the demo — has been
# starting under it, and under SystemCallArchitectures=native above.
filter=" $(user_systemctl show -p SystemCallFilter --value "$sunit") "
case "$filter" in
" ~"*) fail "SystemCallFilter is a deny list: $filter" ;;
*" io_uring_setup "*) fail "SystemCallFilter still allows io_uring_setup" ;;
*" io_uring_enter "*) pass "SystemCallFilter is @system-service less io_uring_setup" ;;
*) fail "SystemCallFilter holds no io_uring_enter, so @aio is missing and io_uring_setup's absence proves nothing: $filter" ;;
esac
# And the filter is in force inside the unit, not only set on it: on a
# kernel without seccomp the manager skips it and the read-back above
# still passes. Inside, the filter mode and io_uring_setup refused;
# from outside, as the same user, the call must reach the kernel, or
# the refusal inside proves nothing about the unit's own filter.
expect_probe seccomp 2
expect_probe io_uring_setup refused
outside=$(as_motel "perl -e 'syscall(425, 1, 0); print \$!{EPERM} ? q(refused) : q(reached)'")
[ "$outside" = "reached" ] \
	&& pass "outside the sandbox: io_uring_setup reaches the kernel" \
	|| fail "io_uring_setup outside the unit is '$outside', want reached: io_uring_setup=refused inside proves nothing (kernel.io_uring_disabled=$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo absent); anything but 0 can refuse it outside too)"
# And the app saw the core-dump limit as 0/0 from inside.
expect_probe core_soft 0
expect_probe core_hard 0
# The retired half of the old model, read back off the live unit: the
# view names what exists rather than masking a list of names.
for gone in "ProtectSystem=strict" "InaccessiblePaths=/"; do
	case "$props" in
	*"$gone"*) fail "unit still carries $gone: $props" ;;
	*) pass "unit no longer carries $gone" ;;
	esac
done

echo "=== systemd 5: stopping a version takes its whole process tree ==="
c=$(deploy demo-workers.tar.gz w1)
[ "$c" = "200" ] || fail "deploy workers: expected 200, got $c ($(cat /tmp/deploy-body))"
case "$(body)" in "hello workers"*) pass "worker-tree release serves" ;; *) fail "worker-tree release not serving" ;; esac
s=$(status)
wunit=$(json_str "$s" unit)
# Host pids from the unit's cgroup: inside its PID namespace the app
# sees itself as pid 1, so the pids.txt the leader writes cannot name
# host processes; the cgroup is the manager's own ledger of them.
wcg=$(user_systemctl show -p ControlGroup --value "$wunit")
tree=$(cat "/sys/fs/cgroup$wcg/cgroup.procs" 2>/dev/null | tr '\n' ' ')
[ "$(echo "$tree" | wc -w)" -ge 2 ] && pass "leader + worker in the unit's cgroup ($tree)" || fail "cgroup $wcg lists fewer than 2 pids: '$tree'"
c=$(deploy demo-v1.tar.gz sd-final)
[ "$c" = "200" ] || fail "deploy sd-final: expected 200, got $c ($(cat /tmp/deploy-body))"
survivors=""
for p in $tree; do kill -0 "$p" 2>/dev/null && survivors="$survivors $p"; done
[ -z "$survivors" ] && pass "every process of the old unit is gone" || fail "processes survived the stop:$survivors"
[ "$(user_systemctl show -p LoadState --value "$wunit")" = "not-found" ] \
	&& pass "old unit unloaded" || fail "old unit $wunit still loaded"

echo "=== systemd 6: a crash leaves no failed unit behind ==="
s=$(status)
cunit=$(json_str "$s" unit)
cpid=$(json_num "$s" pid)
curl -s -o /dev/null --max-time 2 "$PROXY/boom"
i=0
until [ "$(json_num "$(status)" pid)" != "$cpid" ] && [ "$(json_num "$(status)" pid)" != "" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && break
	sleep 0.5
done
s=$(status)
[ "$(json_num "$s" pid)" != "$cpid" ] && pass "watchdog relaunched after the crash" || fail "no relaunch after /boom: $s"
sleep 1
[ "$(user_systemctl show -p LoadState --value "$cunit")" = "not-found" ] \
	&& pass "crashed unit was reset and unloaded" || fail "crashed unit $cunit still loaded"
# One unit per app, and nothing else under the prefix: the examples'
# pre_start oneshots and the sandbox probe's RunOnce unit must all
# have unloaded, so a leak of any of them shows up in the total.
for app in demo deno-example node-example; do
	n=$(user_systemctl list-units --all --no-legend --plain "'motel-$app.*'" | wc -l)
	[ "$n" = "1" ] && pass "exactly one motel-$app unit loaded" || fail "expected 1 loaded motel-$app unit, found $n"
done
n=$(user_systemctl list-units --all --no-legend --plain "'motel-*'" | wc -l)
[ "$n" = "3" ] && pass "no other motel-* unit is loaded (pre_start and probe units unloaded)" || fail "expected 3 loaded motel-* units, found $n"

echo "=== systemd 7: a unit gone behind motel's back is relaunched on start ==="
# The cold path recovery takes when the recorded unit no longer exists
# (a reboot, a manual stop — and a v0.1.0 state.json with no unit at
# all): not reattach, but launch. Stop the unit as the operator might,
# restart motel, and expect the same version back on a new pid.
s=$(status)
gunit=$(json_str "$s" unit)
gpid=$(json_num "$s" pid)
user_systemctl stop "$gunit"
systemctl restart motel || fail "systemctl restart motel failed"
wait_hook || fail "webhook not back within 30s of restart"
i=0
until [ -n "$(json_num "$(status)" pid)" ] && [ "$(json_num "$(status)" pid)" != "$gpid" ]; do
	i=$((i + 1))
	[ "$i" -ge 60 ] && break
	sleep 0.5
done
s=$(status)
if [ -n "$(json_num "$s" pid)" ] && [ "$(json_num "$s" pid)" != "$gpid" ] && [ "$(json_str "$s" unit)" != "$gunit" ]; then
	pass "recovery relaunched the recorded version as a new unit (pid $gpid -> $(json_num "$s" pid))"
else
	fail "no relaunch after the unit was stopped behind motel's back: $s"
fi
case "$s" in *'"current_version":"sd-final"'*) pass "the relaunched instance is the recorded version" ;; *) fail "unexpected version after relaunch: $s" ;; esac
# Recovery publishes the socket as soon as the unit starts (no health
# gate on a relaunch); give the sandboxed app a moment to bind.
i=0
until case "$(body)" in "hello v1"*) true ;; *) false ;; esac; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5
done
case "$(body)" in "hello v1"*) pass "proxy serves the relaunched instance" ;; *) fail "proxy not serving after relaunch" ;; esac

# removal_config writes a Caddyfile without any app — and without the
# sites that proxy to them (:8080, :8082, :8083) and the :8081 webhook
# site, since liveswap_webhook refuses a config that defines no apps.
# The liveswap global block itself stays, so the module is still
# loaded (that is the shape of "an operator removed their last app").
removal_config() { # <out>
	awk '
		/^\t\tapp [a-z-]+ \{/ { skip = 1 }
		/^:80(8[0-3]|9[0-2]) \{/ { skip = 2 }  # the app sites, and e2e/bench/observe.sh ones
		skip == 0 { print }
		skip == 1 && /^\t\t}$/ { skip = 0 }
		skip == 2 && /^}$/     { skip = 0 }
	' /etc/motel/Caddyfile > "$1"
	grep -q '^\t\tapp ' "$1" && die "removal config still names an app"
	grep -q 'dynamic liveswap\|liveswap_observe' "$1" && die "removal config still has a site of an app"
	grep -q 'liveswap {' "$1" || die "removal config lost the liveswap block"
}
# load_config POSTs a Caddyfile to the admin API. Caddy flushes adapter
# warnings before it loads, so the HTTP status can read 200 for a
# rejected config — the body's "error" is the truth.
load_config() { # <file> -> 0 on success
	curl -s -o /tmp/load-body -w '%{http_code}' -X POST -H "Content-Type: text/caddyfile" --data-binary @"$1" "$ADMIN/load" >/tmp/load-code
	[ "$(cat /tmp/load-code)" = "200" ] && ! grep -q '"error"' /tmp/load-body
}
die() { echo "FATAL: $1"; exit 1; }
ADMIN="http://127.0.0.1:2019"
unit_gone() { # <unit> -> 0 once the manager no longer loads it (waits up to 20s)
	i=0
	until [ "$(user_systemctl show -p LoadState --value "$1")" = "not-found" ]; do
		i=$((i + 1))
		[ "$i" -ge 40 ] && return 1
		sleep 0.5
	done
}
# The examples are in the config too, so a removal must stop all three
# apps and a restore must bring all three back — not just demo, which
# status() and the proxy checks watch.
EXAMPLES="deno-example node-example"
ex_status() { curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "${HOOK%/demo}/$1"; }
snapshot_examples() { # records unit and pid per example into EX_<app-with-dashes-removed>_UNIT/_PID
	for app in $EXAMPLES; do
		es=$(ex_status "$app")
		eval "EX_$(echo "$app" | tr -d -)_UNIT='$(json_str "$es" unit)'; EX_$(echo "$app" | tr -d -)_PID='$(json_num "$es" pid)'"
	done
}
assert_examples_gone() { # <label>
	for app in $EXAMPLES; do
		eval "u=\$EX_$(echo "$app" | tr -d -)_UNIT; p=\$EX_$(echo "$app" | tr -d -)_PID"
		unit_gone "$u" && pass "$1: $app's unit stopped and unloaded" || fail "$1: unit $u of $app survived"
		kill -0 "$p" 2>/dev/null && fail "$1: $app's process $p still alive" || pass "$1: $app's process is gone"
	done
}
assert_examples_back() { # <label>
	for app in $EXAMPLES; do
		eval "old=\$EX_$(echo "$app" | tr -d -)_PID"
		i=0; np=""
		while [ "$i" -lt 60 ]; do
			np=$(json_num "$(ex_status "$app")" pid)
			[ -n "$np" ] && [ "$np" != "$old" ] && break
			i=$((i + 1)); sleep 0.5
		done
		[ -n "$np" ] && [ "$np" != "$old" ] && pass "$1: $app relaunched (pid $old -> $np)" || fail "$1: $app did not come back: $(ex_status "$app")"
	done
}
wait_new_pid() { # <old pid> -> sets NEWPID or fails after 30s
	i=0
	NEWPID=""
	while [ "$i" -lt 60 ]; do
		p=$(json_num "$(status)" pid)
		if [ -n "$p" ] && [ "$p" != "$1" ] && [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/")" = "200" ]; then
			NEWPID=$p
			return 0
		fi
		i=$((i + 1))
		sleep 0.5
	done
	return 1
}

echo "=== systemd 7b: a reload's changed cap reaches the running instance, no restart ==="
# A Caddyfile reload keeps apps running, so a changed cap is set on the
# running unit through the manager (syncCaps) rather than waiting for
# the next launch: the kernel's cpu.max follows, the pid does not
# change, and status shows the new value. Restoring the file lifts it
# back the same way. The unit is read fresh: scenario 7 relaunched it.
s=$(status)
pid0=$(json_num "$s" pid)
scg=$(user_systemctl show -p ControlGroup --value "$(json_str "$s" unit)")
cpu_max() { cat "/sys/fs/cgroup$scg/cpu.max" 2>/dev/null; }
await_cpu_max() { # <want> -> 0 once the kernel holds it (waits up to 5s)
	i=0
	until [ "$(cpu_max)" = "$1" ]; do
		i=$((i + 1)); [ "$i" -ge 20 ] && return 1; sleep 0.25
	done
}
[ "$(cpu_max)" = "150000 100000" ] && pass "before the reload: the relaunched unit carries cpu_quota 150%" \
	|| fail "before the reload: cpu.max is '$(cpu_max)', want 150000 100000"
sed 's/^\(\t*\)cpu_quota 150%$/\1cpu_quota 100%/' /etc/motel/Caddyfile > /tmp/caps.Caddyfile
grep -q 'cpu_quota 100%' /tmp/caps.Caddyfile || die "the cpu_quota edit did not take"
if load_config /tmp/caps.Caddyfile; then
	await_cpu_max "100000 100000" && pass "reload: the running unit's cpu.max follows cpu_quota 100%" \
		|| fail "reload: cpu.max is '$(cpu_max)' after cpu_quota 100%, want 100000 100000"
	s=$(status)
	[ "$(json_num "$s" pid)" = "$pid0" ] && pass "reload: same pid, the cap changed without a restart" \
		|| fail "reload: pid changed from $pid0 to $(json_num "$s" pid)"
	[ "$(json_num "$s" cpu_quota)" = "100" ] && pass "reload: status reports cpu_quota 100" \
		|| fail "reload: status reports cpu_quota '$(json_num "$s" cpu_quota)', want 100"
else
	fail "reload with cpu_quota 100% rejected: $(cat /tmp/load-body)"
fi
load_config /etc/motel/Caddyfile || fail "reload restoring cpu_quota 150% rejected: $(cat /tmp/load-body)"
await_cpu_max "150000 100000" && pass "restore: the running unit's cpu.max is back at cpu_quota 150%" \
	|| fail "restore: cpu.max is '$(cpu_max)', want 150000 100000"

echo "=== systemd 8: removing the app via reload stops its units; adding it back relaunches ==="
s=$(status)
runit=$(json_str "$s" unit)
rpid=$(json_num "$s" pid)
snapshot_examples
removal_config /tmp/without-demo.Caddyfile
load_config /tmp/without-demo.Caddyfile && pass "reload without the apps accepted" \
	|| fail "reload without the apps rejected: $(cat /tmp/load-code) $(cat /tmp/load-body)"
unit_gone "$runit" && pass "removed app's unit stopped and unloaded" || fail "unit $runit survived the app's removal"
kill -0 "$rpid" 2>/dev/null && fail "removed app's process $rpid still alive" || pass "removed app's process is gone"
assert_examples_gone "reload"
load_config /etc/motel/Caddyfile || fail "reload restoring the apps rejected: $(cat /tmp/load-body)"
wait_new_pid "$rpid" && pass "re-added app relaunched from state.json (pid $rpid -> $NEWPID)" \
	|| fail "re-added app did not come back: $(status)"
assert_examples_back "re-add"
case "$(status)" in *'"current_version":"sd-final"'*) pass "relaunched the recorded version" ;; *) fail "unexpected version after re-add: $(status)" ;; esac

echo "=== systemd 9: an app removed while motel was down is swept on the next start ==="
s=$(status)
dunit=$(json_str "$s" unit)
dpid=$(json_num "$s" pid)
snapshot_examples
cp /etc/motel/Caddyfile /tmp/with-demo.Caddyfile
systemctl stop motel
kill -0 "$dpid" 2>/dev/null && pass "app survives motel stop (pid $dpid)" || fail "app died with motel stop"
cp /tmp/without-demo.Caddyfile /etc/motel/Caddyfile
systemctl start motel || fail "start without the apps failed"
unit_gone "$dunit" && pass "start-time sweep stopped the unit of an app no config names" \
	|| fail "unit $dunit of a removed app survived the restart"
assert_examples_gone "start-time sweep"
cp /tmp/with-demo.Caddyfile /etc/motel/Caddyfile
systemctl restart motel || fail "restart with the app failed"
wait_hook || fail "webhook not back after restoring the app"
wait_new_pid "$dpid" && pass "restored app relaunched from state.json (pid $dpid -> $NEWPID)" \
	|| fail "restored app did not come back: $(status)"
assert_examples_back "restore"

echo "=== systemd 9b: a start motel refuses stays down: systemd does not retry an exit 1 ==="
# motel's own refusal, not a syntax error: liveswap_webhook with no
# apps is refused while the config loads, after Caddy has already sent
# READY=1 — the shape where `systemctl restart` can return 0 and a test
# that trusted it would pass vacuously.
s=$(status)
fpid=$(json_num "$s" pid)
funit=$(json_str "$s" unit)
cp /etc/motel/Caddyfile /tmp/good.Caddyfile
removal_config /tmp/refused.Caddyfile
printf ':8081 {\n\tliveswap_webhook\n}\n' >> /tmp/refused.Caddyfile
cp /tmp/refused.Caddyfile /etc/motel/Caddyfile
systemctl reset-failed motel
N0=$(hs_prop NRestarts) # the baseline, taken before the start that will be refused
T0=$(date +%s)
systemctl restart motel >/dev/null 2>&1 # status ignored on purpose, see above
i=0
until [ "$(hs_prop ActiveState)" = "failed" ]; do i=$((i + 1)); [ "$i" -ge 100 ] && break; sleep 0.1; done
[ "$(hs_prop ActiveState)" = "failed" ] && pass "refused config: the unit failed" || fail "unit did not fail on the refused config: $(hs_shape)"
Result= ExecMainCode= ExecMainStatus=
eval "$(systemctl show -p Result,ExecMainCode,ExecMainStatus motel)"
[ "$Result" = "exit-code" ] && [ "$ExecMainCode" = "1" ] && [ "$ExecMainStatus" = "1" ] \
	&& pass "the refusal is exit-code 1 (Caddy's failed startup)" \
	|| fail "refusal shape Result=$Result ExecMainCode=$ExecMainCode ExecMainStatus=$ExecMainStatus"
sleep 3 # > RestartSec: had systemd retried, the unit would be activating or active by now
[ "$(hs_prop ActiveState)" = "failed" ] && [ "$(hs_prop SubState)" = "failed" ] && [ "$(hs_prop NRestarts)" = "$N0" ] \
	&& pass "still failed 3s later, NRestarts unchanged ($N0): RestartPreventExitStatus=1 held" \
	|| fail "systemd retried the refused start: $(hs_shape)"
journalctl -u motel --no-pager --since "@$T0" | grep -q "no apps are defined" \
	&& pass "the journal carries the refusal's reason" || fail "refusal reason not in the journal since $T0"
kill -0 "$fpid" 2>/dev/null && pass "the app kept running through the refused start (pid $fpid)" || fail "app $fpid died with the refused start"
cp /tmp/good.Caddyfile /etc/motel/Caddyfile
systemctl start motel || fail "start after restoring the config failed"
wait_hook || fail "webhook not back after restoring the config"
s=$(status)
[ "$(json_num "$s" pid)" = "$fpid" ] && [ "$(json_str "$s" unit)" = "$funit" ] \
	&& pass "reattached after the refused start" || fail "instance changed across the refused start: $s"

echo "=== systemd 10: app output reaches the journal ==="
journalctl --no-pager -t motel-demo | grep -q "workers up" \
	&& pass "app stdout is in the journal under identifier motel-demo" \
	|| fail "no app output in the journal for -t motel-demo"

echo "=== systemd 11: motel is non-dumpable — its /proc is closed to its own UID ==="
# Apps run as the motel user. The kernel opens /proc/<pid>/environ
# and /proc/<pid>/root to any same-UID reader of a dumpable process —
# whatever sandbox that reader sits in — so without this floor every
# app could read the supervisor's environment (ACME DNS tokens) or walk
# the host filesystem through /proc/<motel>/root. liveswap/harden's
# init marks the process non-dumpable before main; this proves it
# under the packaged unit.
hpid=$(systemctl show -p MainPID --value motel)
[ -n "$hpid" ] && [ "$hpid" != "0" ] || fail "no MainPID for motel.service"
# (The /proc/<pid> directory itself keeps the task's uid so `ps` can
# stat it; the per-process files inside are what turn root-owned.)
[ "$(stat -c %U "/proc/$hpid/environ")" = "root" ] \
	&& pass "/proc/$hpid/environ is root-owned (motel is non-dumpable)" \
	|| fail "/proc/$hpid/environ is owned by $(stat -c %U "/proc/$hpid/environ"): motel is dumpable"
# Each denial is asserted by its reason, not by a non-zero exit: a
# broken `su` would otherwise pass these vacuously.
denied_to_motel() { # <what> <command...> -> pass only if the command fails AND reports EACCES
	what=$1; shift
	if out=$(as_motel "$*" 2>&1); then
		fail "$what is readable to the motel user (command succeeded): $out"
		return
	fi
	case "$out" in
	*"Permission denied"*) pass "$what is closed to the motel user" ;;
	*) fail "expected EACCES: $what as the motel user, got: $out" ;;
	esac
}
denied_to_motel "motel's environ" cat "/proc/$hpid/environ"
denied_to_motel "/proc/$hpid/root" ls "/proc/$hpid/root/"
# Control: root can read the app's environ, so the denials above are
# the non-dumpable floor, not a broken /proc. (The motel uid can no
# longer read a sandboxed app's environ either: the app sits in its
# own user namespace, which closes /proc to every process outside it.)
apid=$(json_num "$(status)" pid)
cat "/proc/$apid/environ" >/dev/null 2>&1 \
	&& pass "control: root can read the app's environ (pid $apid)" \
	|| fail "control: the app's environ (pid $apid) is unreadable even to root"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL SYSTEMD SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES SYSTEMD ASSERTION(S) FAILED"
exit 1
