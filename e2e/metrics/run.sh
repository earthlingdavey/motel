#!/bin/sh
# The metrics suite (make e2e-metrics), run inside e2e-metrics-box as root.
#
# The box is the notify suite's with Debian's victoria-metrics installed
# and masked; motel runs e2e/metrics/Caddyfile, the notify suite's block
# plus `metrics_store`. The suite runs `motel notify setup` and `motel
# metrics setup`, then checks notify/DESIGN-metrics.md's "e2e rows":
#
#   the store's TCP in its namespace only; its socket's mode and reach
#   motel's push (job="motel"), with a deployed app's motel_app_*
#   GET /<app> with the deploy token carries the app's metrics
#   vmalert's rules loaded; its ChannelFailed and motel's one alert
#   nothing of the store in an app's sandbox view
#   the store's socket gone: GET /<app> answers at once, with the error
. /lib.sh

HOOK=http://localhost:8081/blog
ART=http://e2e-artifacts:8080/artifacts
SINK=http://e2e-sink:8080
VM=/run/motel-metrics/vm.sock
AMS=/run/motel-notify/am.sock
STATE=/var/lib/motel-metrics
NS=motel-metrics
vm() { curl -s --max-time 10 --unix-socket "$VM" "$@"; }
am() { curl -s --max-time 10 --unix-socket "$AMS" "$@"; }
# q <query>: an instant query's answer, from the store.
q() { vm -G --data-urlencode "query=$1" http://vm/api/v1/query; }
# has_result <answer>: the answer holds at least one series.
has_result() { printf '%s' "$1" | grep -q '"result":\[{'; }
# wait_result <what> <query> <timeout s>: waits for the query to return a series.
wait_result() {
	end=$(( $(date +%s) + $3 ))
	while [ "$(date +%s)" -lt "$end" ]; do
		a=$(q "$2")
		if has_result "$a"; then echo "$a"; return 0; fi
		sleep 2
	done
	return 1
}
inns() { ip netns exec "$NS" "$@"; }

echo "=== metrics 0: the box ==="
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && { echo "FATAL: motel did not come up"; journalctl -u motel --no-pager -n 50; exit 1; }
	sleep 0.5
done
wait_for_token
journalctl -u motel -o cat --no-pager | grep -q 'the metrics store is not set up on this box' \
	&& pass "before setup, motel says the store is not set up, and serves" \
	|| fail "no 'not set up' line for the store before setup"
if motel metrics setup >/tmp/msetup.out 2>&1; then
	fail "metrics setup ran before notify setup"
else
	grep -q 'run motel notify setup first' /tmp/msetup.out \
		&& pass "metrics setup refuses before notify setup" || fail "metrics setup before notify: $(cat /tmp/msetup.out)"
fi

echo "=== metrics 1: setup ==="
motel notify setup >/tmp/nsetup.out 2>&1 && pass "notify setup ran" || fail "notify setup: $(cat /tmp/nsetup.out)"
if motel metrics setup >/tmp/msetup.out 2>&1; then
	pass "metrics setup ran: $(tr '\n' ';' </tmp/msetup.out | cut -c1-240)"
else
	fail "metrics setup: $(cat /tmp/msetup.out)"
fi
i=0
until inns ss -ltnH 2>/dev/null | grep -q '127.0.0.1:8880'; do
	i=$((i + 1)); [ "$i" -ge 60 ] && break; sleep 0.5
done
host=$(ss -ltnH 2>/dev/null | awk '{print $4}' | grep -E ':(8428|8880|9093|9094)$' || true)
[ -z "$host" ] && pass "no TCP listener on 8428, 8880, 9093 or 9094 in the host's namespace" || fail "the host listens: $host"
inside=$(inns ss -ltnH 2>/dev/null | awk '{print $4}' | sort | tr '\n' ' ')
for a in 127.0.0.1:8428 127.0.0.1:8880 127.0.0.1:9093; do
	case " $inside" in *" $a "*) pass "in the namespace: $a" ;; *) fail "not in the namespace: $a (it holds: $inside)" ;; esac
done
[ "$(stat -c '%A %U %G' "$VM")" = "srw-rw---- root motel" ] \
	&& pass "the store's socket is srw-rw---- root motel" || fail "the socket is $(stat -c '%A %U %G' "$VM" 2>&1)"
[ "$(stat -c '%A %U %G' "$STATE")" = "drwxr-s--- motel _victoria-metrics" ] \
	&& pass "$STATE is drwxr-s--- motel _victoria-metrics" || fail "$STATE is $(stat -c '%A %U %G' "$STATE" 2>&1)"
# Debian ships it 0755: the drop-in sets the mode at every start (setup
# started the store), so no other uid reads the series past the socket's
# group.
[ "$(stat -c '%A %U %G' /var/lib/victoria-metrics)" = "drwxr-x--- _victoria-metrics _victoria-metrics" ] \
	&& pass "the store's data is drwxr-x--- _victoria-metrics _victoria-metrics" \
	|| fail "the store's data is $(stat -c '%A %U %G' /var/lib/victoria-metrics 2>&1)"
[ "$(vm http://vm/health)" = OK ] && pass "root reaches the store" || fail "root: $(vm http://vm/health)"
[ "$(runuser -u motel -- curl -s --max-time 5 --unix-socket "$VM" http://vm/health)" = OK ] \
	&& pass "the motel user reaches it" || fail "the motel user is refused"
if runuser -u nobody -- curl -s -o /dev/null --max-time 5 --unix-socket "$VM" http://vm/health; then
	fail "nobody reached the store's socket"
else
	pass "nobody is refused the store's socket (curl exit $?)"
fi
curl -s -o /dev/null --max-time 3 http://127.0.0.1:8428/health && fail "the store answers on the host's 127.0.0.1:8428" \
	|| pass "the host's 127.0.0.1:8428 is refused"
[ "$(systemctl show -p MemoryMax --value victoria-metrics)" = 268435456 ] && pass "the store's MemoryMax is 256M" \
	|| fail "the store's MemoryMax is $(systemctl show -p MemoryMax --value victoria-metrics)"
[ "$(systemctl show -p MemoryMax --value motel-vmalert)" = 134217728 ] && pass "vmalert's MemoryMax is 128M" \
	|| fail "vmalert's MemoryMax is $(systemctl show -p MemoryMax --value motel-vmalert)"
i=0
until grep -q 'motel-channel-failed' "$STATE/rules.yml" 2>/dev/null && grep -q 'notify_channel: ops' "$STATE/rules.yml"; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.25
done
grep -q 'notify_channel: ops' "$STATE/rules.yml" \
	&& pass "motel rendered the rules, with ChannelFailed for the operator's channels" || fail "rules.yml: $(head -c 400 "$STATE/rules.yml")"
vmalert -dryRun -rule="$STATE/rules.yml" >/tmp/dry.out 2>&1 \
	&& pass "vmalert -dryRun passes the rendered rules" || fail "vmalert -dryRun: $(tail -3 /tmp/dry.out)"

echo "=== metrics 2: the push ==="
code=$(deploy demo-v1.tar.gz v1)
[ "$code" = 200 ] && pass "deploy v1" || fail "deploy v1 returned $code: $(cat /tmp/deploy-body)"
a=$(wait_result push 'process_start_time_seconds{job="motel"}' 120) \
	&& pass "motel's push arrived (job=\"motel\")" || fail "no series of motel's in the store after 120 s"
a=$(wait_result up 'motel_app_up{app="blog"} == 1' 90) \
	&& pass "motel_app_up{app=\"blog\"} is 1" || fail "no motel_app_up for blog: $(q 'motel_app_up')"
a=$(wait_result release 'motel_app_release_info{app="blog",release="v1"}' 30) \
	&& pass "motel_app_release_info names v1" || fail "no release_info for blog v1"
a=$(wait_result cpu 'motel_app_cpu_seconds_total{app="blog"} > 0' 60) \
	&& pass "motel_app_cpu_seconds_total for blog" || fail "no CPU for blog: $(q 'motel_app_cpu_seconds_total')"
# The unit's memory is its cgroup's memory.current, which exists only
# where the memory controller reaches the user manager's units: Debian 13
# delegates it, a container's host may not. The suite asserts it where
# the file is there, and says so where it is not.
pid=$(json_num "$(status)" pid)
cg=$(sed -n 's/^0:://p' "/proc/$pid/cgroup" 2>/dev/null)
MEMORY=0
[ -n "$cg" ] && [ -f "/sys/fs/cgroup$cg/memory.current" ] && MEMORY=1
if [ "$MEMORY" = 1 ]; then
	a=$(wait_result memory 'motel_app_memory_bytes{app="blog"} > 0' 60) \
		&& pass "motel_app_memory_bytes for blog" || fail "no memory for blog: $(q 'motel_app_memory_bytes')"
else
	n=$(q 'motel_app_memory_bytes{app="blog"}')
	has_result "$n" && fail "memory for blog with no memory.current to read: $n" \
		|| pass "no memory controller in this host's user units (no $cg/memory.current): motel_app_memory_bytes absent, as designed"
fi

echo "=== metrics 3: GET /<app> ==="
m=""
end=$(( $(date +%s) + 90 ))
while [ "$(date +%s)" -lt "$end" ]; do
	m=$(status)
	if [ "$MEMORY" = 1 ]; then
		printf '%s' "$m" | grep -q '"metrics":{[^}]*"memory_bytes":{"now":[1-9]' && break
	else
		printf '%s' "$m" | grep -q '"metrics":{[^}]*"cpu_cores":{"avg_5m":' && break
	fi
	sleep 3
done
want='"cpu_cores":{"avg_5m":'
[ "$MEMORY" = 1 ] && want='"memory_bytes":{"now":[1-9]'
printf '%s' "$m" | grep -q '"metrics":{"window":"1h","at":"' && printf '%s' "$m" | grep -q "$want" \
	&& pass "GET /blog with the deploy token carries its metrics: $(printf '%s' "$m" | grep -o '"metrics":{.*' | cut -c1-200)" \
	|| fail "GET /blog's metrics: $(printf '%s' "$m" | grep -o '"metrics":.*' | cut -c1-300)"
code=$(curl -s -o /tmp/get-noauth -w '%{http_code}' --max-time 5 "$HOOK")
[ "$code" = 401 ] && ! grep -q metrics /tmp/get-noauth && pass "GET /blog without a token: 401, no metrics" || fail "GET /blog without a token: $code"

echo "=== metrics 4: vmalert ==="
# Setup's first rules.yml is the empty groups and motel writes the real
# one right after; vmalert re-reads the file once a minute
# (-configCheckInterval), so wait out one re-read.
i=0
until rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules) \
	&& printf '%s' "$rules" | grep -q '"name":"ChannelFailed"'; do
	i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2
done
for r in ChannelFailed app_memory_high metrics_store_disk_fill; do
	printf '%s' "$rules" | grep -q "\"name\":\"$r\"" && pass "vmalert loaded $r" || fail "vmalert has no $r: $(printf '%s' "$rules" | cut -c1-200)"
done
# What metrics 7 counts after the notify block goes: present now, so its
# absence then is not a field the API never had.
printf '%s' "$rules" | grep -q '"type":"alerting"' && pass "vmalert's API marks its rules \"type\":\"alerting\"" \
	|| fail "vmalert's API has no \"type\":\"alerting\": $(printf '%s' "$rules" | cut -c1-300)"
a=$(wait_result vmalert-up 'up{job="vmalert"} == 1' 90) && pass "the store scrapes vmalert" || fail "the store has no up{job=\"vmalert\"}"
a=$(wait_result am-up 'up{job="alertmanager"} == 1' 90) && pass "the store scrapes Alertmanager through the in-namespace proxy" \
	|| fail "the store has no up{job=\"alertmanager\"}: $(q 'up')"
# A box event reaches ops and oncall; oncall answers 400, Alertmanager
# counts the failure on event-box-warning's discord, and both motel and
# vmalert post ChannelFailed to ops with suspects=oncall.
motel notify send --source e2e --name nightly_job_failed --severity warning --summary "e2e: the nightly job failed" >/tmp/send.out 2>&1 \
	|| fail "notify send: $(cat /tmp/send.out)"
sel='ALERTS{alertname="ChannelFailed",alertstate="firing",notify_channel="ops",integration="discord",suspects="oncall",severity="warning",subject="box"}'
a=$(wait_result channel-failed "$sel" 300) \
	&& pass "vmalert's ChannelFailed fires for ops, naming oncall, with motel's labels" \
	|| fail "no vmalert ChannelFailed after 300 s: $(q 'ALERTS')"
printf '%s' "$a" | grep -q '"alertgroup"' && fail "vmalert's alert carries alertgroup: $a" || pass "no alertgroup label on vmalert's alert"
amq='filter=alertname%3D%22ChannelFailed%22&filter=notify_channel%3D%22ops%22&filter=suspects%3D%22oncall%22&filter=integration%3D%22discord%22'
n=$(am "http://am/api/v2/alerts?$amq" | grep -o '"fingerprint"' | wc -l)
[ "$n" -eq 1 ] && pass "Alertmanager holds one ChannelFailed for ops naming oncall: the two sources are one alert" \
	|| fail "Alertmanager holds $n such alerts: $(am "http://am/api/v2/alerts?$amq" | cut -c1-400)"
# The merged alert carries the generatorURL of whichever source posted
# last: motel reposts every minute and vmalert re-sends at each 1m
# evaluation, so wait out one re-send for vmalert's to be the latest.
i=0
until am "http://am/api/v2/alerts?$amq" | grep -q '"generatorURL":"http'; do
	i=$((i + 1)); [ "$i" -ge 38 ] && break; sleep 2
done
[ "$i" -lt 38 ] && pass "the one alert was posted by vmalert too (its generatorURL)" \
	|| fail "no vmalert post merged into the alert in 75 s: $(am "http://am/api/v2/alerts?$amq" | cut -c1-400)"

echo "=== metrics 4a: the store stopped by hand comes back with vmalert ==="
# A stop of the store stops vmalert and the proxy (both Require it).
# motel's next push connects to the socket, whose proxy starts the store
# again; the store's Wants= brings vmalert back with it.
systemctl stop victoria-metrics
systemctl is-active -q motel-vmalert && fail "vmalert still runs with the store stopped" || pass "stopping the store stops vmalert"
i=0
until systemctl is-active -q victoria-metrics && systemctl is-active -q motel-vmalert; do
	i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2
done
[ "$i" -lt 45 ] && pass "motel's push started the store again, and vmalert with it" \
	|| fail "after 90 s: the store $(systemctl is-active victoria-metrics), vmalert $(systemctl is-active motel-vmalert)"
a=$(wait_result vmalert-up-again 'up{job="vmalert"} == 1' 90) && pass "the store scrapes vmalert again" || fail "no up{job=\"vmalert\"} after the restart"

echo "=== metrics 4b: a read-only store is told by motel, as itself ==="
# The store's free-space floor set above any disk: it is read-only from
# its start, refuses motel's imports and its own scrapes, and no rule
# can see the flag. motel reads it from the store's /metrics.
dropin=/etc/systemd/system/victoria-metrics.service.d
{
	echo '[Service]'
	echo 'ExecStart='
	echo "$(grep '^ExecStart=/' "$dropin/10-motel.conf") -storage.minFreeDiskSpaceBytes=1000000000000000"
} >"$dropin/20-e2e-read-only.conf"
systemctl daemon-reload && systemctl restart victoria-metrics
ro_at=$(date +%s)
i=0
until vm http://vm/metrics | grep -q '^vm_storage_is_read_only.* 1$'; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 2; done
[ "$i" -lt 30 ] && pass "the store says it is read-only: $(vm http://vm/metrics | grep '^vm_storage_is_read_only')" \
	|| fail "the store is not read-only: $(vm http://vm/metrics | grep '^vm_storage_is_read_only')"
roq='filter=alertname%3D%22metrics_store_read_only%22'
i=0
until am "http://am/api/v2/alerts?$roq" | grep -q '"severity":"critical"'; do i=$((i + 1)); [ "$i" -ge 60 ] && break; sleep 2; done
[ "$i" -lt 60 ] && pass "motel posted metrics_store_read_only, critical" \
	|| fail "no metrics_store_read_only in Alertmanager after 120 s: $(am "http://am/api/v2/alerts?$roq" | cut -c1-300)"
# Two of the store's 30 s self-scrapes have come and gone by now: had it
# kept one, a rule would see the flag.
while [ $(( $(date +%s) - ro_at )) -lt 75 ]; do sleep 2; done
a=$(q 'max(max_over_time(vm_storage_is_read_only[5m])) > 0')
printf '%s' "$a" | grep -q '"status":"success"' && ! has_result "$a" \
	&& pass "75 s read-only and no series carries the flag: the store refused its own scrapes" \
	|| fail "a rule could have seen the flag after all, or the store did not answer: $(printf '%s' "$a" | cut -c1-200)"
rm "$dropin/20-e2e-read-only.conf"
systemctl daemon-reload && systemctl restart victoria-metrics
back_at=$(date +%s)
i=0
until vm http://vm/metrics | grep -q '^vm_storage_is_read_only.* 0$'; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 2; done
[ "$i" -lt 30 ] && pass "the store is writable again" || fail "the store is still read-only: $(vm http://vm/metrics | grep '^vm_storage_is_read_only')"
# A sample from after the restart, not one kept from before the floor.
a=$(wait_result vmalert-up-writable "(timestamp(up{job=\"vmalert\"}) > $back_at) and (up{job=\"vmalert\"} == 1)" 120) && pass "and scrapes vmalert again" \
	|| fail "no fresh up{job=\"vmalert\"} after the store came back: $(q 'timestamp(up{job="vmalert"})')"

echo "=== metrics 4c: the two proxies run as no root ==="
# Each runs while it carries traffic (motel's push through vm.sock every
# 30 s, the store's scrape of Alertmanager through the in-namespace port);
# 4b restarted the store, which stops the vm proxy until the next push.
for u in motel-vm.service motel-am-proxy.service; do
	i=0
	until [ "$(systemctl show -p MainPID --value "$u")" != 0 ]; do i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2; done
	pid=$(systemctl show -p MainPID --value "$u")
	uid=$(stat -c %u "/proc/$pid" 2>/dev/null)
	bnd=$(sed -n 's/^CapBnd:[[:space:]]*//p' "/proc/$pid/status" 2>/dev/null)
	[ "$pid" != 0 ] && [ -n "$uid" ] && [ "$uid" != 0 ] && [ "$bnd" = 0000000000000000 ] \
		&& pass "$u runs as uid $uid ($(ps -o user= -p "$pid")), no capability" \
		|| fail "$u: pid $pid, uid '$uid', CapBnd '$bnd'"
done
# The am proxy holds the motel group for Alertmanager's socket, so the
# group's paths must be out of its view, and the socket in it; and the
# group is its primary one, so the store's uid cannot act as it.
pid=$(systemctl show -p MainPID --value motel-am-proxy.service)
motel_gid=$(getent group motel | cut -d: -f3)
[ "$(ps -o user= -p "$pid")" = _victoria-metrics ] && [ "$(sed -n 's/^Gid:[[:space:]]*\([0-9]*\).*/\1/p' "/proc/$pid/status")" = "$motel_gid" ] \
	&& [ "$(sed -n 's/^Groups:[[:space:]]*//p' "/proc/$pid/status" | tr -d ' \t')" = "$motel_gid" ] \
	&& pass "the am proxy is _victoria-metrics, gid motel and no other group" \
	|| fail "the am proxy: $(ps -o user= -p "$pid"), $(grep -E '^(Gid|Groups)' "/proc/$pid/status" | tr '\n' ' ')"
# Yama does not gate a /proc read, so this one holds on any host.
if runuser -u _victoria-metrics -- cat "/proc/$pid/environ" >/dev/null 2>&1; then
	fail "the store's uid reads the am proxy's /proc: it could attach to it and act with the motel group"
else
	pass "the store's uid is refused the am proxy's /proc"
fi
# An attach, with a control: the store's uid attaching to a sleeper of
# its own uid and gid. Where that is refused too (ptrace_scope 1 or more,
# or a seccomp that denies ptrace), the refusal proves nothing of the
# unit, and the /proc check above stands for it. An attach that gets
# through is detached at once, so a stopped proxy cannot fail the
# sections after this one.
nr=117; [ "$(uname -m)" = x86_64 ] && nr=101
attach() {
	runuser -u _victoria-metrics -- perl -e '$p='"$1"'; $r=syscall('"$nr"',16,$p,0,0); if ($r==0) { waitpid($p,0); syscall('"$nr"',17,$p,0,0); print "attached" } else { print "refused" }'
}
runuser -u _victoria-metrics -- sleep 60 &
i=0
until sleeper=$(pgrep -u _victoria-metrics -x sleep | head -1) && [ -n "$sleeper" ]; do
	i=$((i + 1)); [ "$i" -ge 50 ] && break; sleep 0.2
done
control=refused
[ -n "$sleeper" ] && control=$(attach "$sleeper")
kill "$sleeper" 2>/dev/null
wait 2>/dev/null
att=$(attach "$pid")
[ -n "$sleeper" ] || fail "fixture: no sleeper of the store's uid after 10 s, so no control for the attach"
case "$control,$att" in
attached,refused) pass "the store's uid cannot attach to the am proxy (and can attach to a sleeper of its own)" ;;
refused,refused) pass "this host refuses the store's uid any attach (ptrace_scope $(cat /proc/sys/kernel/yama/ptrace_scope 2>/dev/null)): the /proc check above stands for the unit" ;;
*) fail "the store's uid attached to the am proxy (control: $control)"; kill -CONT "$pid" 2>/dev/null ;;
esac
# Every path an app's view hides (liveswap/sandbox.go, sandboxMotelPaths,
# which TestE2EProxyViewHidesTheMotelPaths holds this list to) and the
# apps' root: the motel group reaches them on a packaged box, and only
# Alertmanager's socket directory is bound back.
seen=""
for p in /var/lib/motel /run/motel /etc/motel /etc/liveswap /var/lib/motel-box /var/lib/motel-notify \
	/run/motel-metrics /var/lib/motel-metrics /var/lib/liveswap; do
	[ -e "/proc/$pid/root$p" ] && seen="$seen $p"
done
[ -z "$seen" ] && pass "the am proxy sees none of motel's paths but Alertmanager's socket directory" \
	|| fail "the am proxy sees:$seen"
[ -S "/proc/$pid/root$AMS" ] && pass "and sees Alertmanager's socket" || fail "the am proxy has no $AMS in its view"

echo "=== metrics 5: an app's sandbox has none of it in view ==="
code=$(deploy demo-probe.tar.gz sb1)
[ "$code" = 200 ] || fail "deploy of the probe release returned $code: $(cat /tmp/deploy-body)"
probe=/var/lib/liveswap/blog/shared/view.txt
i=0
until grep -q '^done=1' "$probe" 2>/dev/null; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
[ -S "$VM" ] && [ -d "$STATE" ] || fail "the host has no socket or state dir; the probe would prove nothing"
for k in metrics_run metrics_socket metrics_state; do
	v=$(sed -n "s/^$k=//p" "$probe" | head -1)
	[ "$v" = closed ] && pass "inside the sandbox: $k=closed" || fail "inside the sandbox: $k=$v"
done

echo "=== metrics 6: the store's socket gone ==="
systemctl stop motel-vm.socket motel-vm.service
sleep 31 # past the view's cache
t0=$(date +%s%3N)
m=$(status)
t1=$(date +%s%3N)
printf '%s' "$m" | grep -q '"metrics":{"error":"the metrics store did not answer"}' && [ $((t1 - t0)) -lt 3000 ] \
	&& pass "GET /blog answers in $((t1 - t0)) ms, metrics an error, the rest as before" \
	|| fail "GET /blog with the store gone ($((t1 - t0)) ms): $(printf '%s' "$m" | cut -c1-300)"
json_str "$m" current_version | grep -q sb1 && pass "the status is otherwise whole" || fail "the status: $(printf '%s' "$m" | cut -c1-200)"
i=0
until journalctl -u motel -o cat --no-pager | grep -q 'metrics_store: push failed'; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 1
done
journalctl -u motel -o cat --no-pager | grep -q 'metrics_store: push failed' \
	&& pass "journal: metrics_store: push failed" || fail "no push failed line"
systemctl start motel-vm.socket

echo "=== metrics 7: the notify block removed, metrics_store kept ==="
# Alertmanager keeps the last config notify rendered, so vmalert must be
# given no rule that could still reach those channels.
cp /etc/motel/Caddyfile /tmp/Caddyfile.both
sed '/^\tnotify {$/,/^\t}$/d' /etc/motel/Caddyfile >/tmp/Caddyfile.nonotify
if grep -q 'notify {' /tmp/Caddyfile.nonotify || ! grep -q 'metrics_store' /tmp/Caddyfile.nonotify; then
	fail "could not cut the notify block out of the Caddyfile"
fi
cp /tmp/Caddyfile.nonotify /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded without the notify block" || fail "the reload without the notify block failed"
i=0
until ! grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && fail "rules.yml still has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "motel rendered rules.yml with no alerting rule"
end=$(( $(date +%s) + 100 ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 5
done
printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' \
	&& pass "vmalert re-read the rules: zero alerting rules loaded" \
	|| fail "vmalert still loads alerting rules: $(printf '%s' "$rules" | grep -o '"name":"[^"]*","type":"alerting"' | head -5 | tr '\n' ' ')"
journalctl -u motel -o cat --no-pager | grep -q 'metrics_store: no notify block: vmalert is given no rule' \
	&& pass "journal: no notify block, no rule" || fail "no 'no notify block' line"

echo "=== metrics 8: motel's shutdown keeps the rules; the metrics_store block removed retires them ==="
# A shutdown leaves the rules, so the store's alerts reach the operator
# while motel is down; a reload to a config with no metrics_store block
# writes the groups empty, as vmalert and the store still run.
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded with both blocks again" || fail "the reload with both blocks failed"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "both blocks again: rules.yml has alerting rules" || fail "rules.yml has no alerting rule with both blocks"
systemctl stop motel
grep -q 'alert:' "$STATE/rules.yml" && pass "motel stopped: rules.yml keeps its alerting rules" \
	|| fail "motel's shutdown retired the rules: $(head -c 300 "$STATE/rules.yml")"
systemctl start motel && pass "motel started again" || fail "motel did not start again"
sed '/^\tmetrics_store$/d' /tmp/Caddyfile.both >/tmp/Caddyfile.nostore
if grep -q '^[[:space:]]*metrics_store' /tmp/Caddyfile.nostore || ! grep -q 'notify {' /tmp/Caddyfile.nostore; then
	fail "could not cut the metrics_store block out of the Caddyfile"
fi
cp /tmp/Caddyfile.nostore /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded without the metrics_store block" || fail "the reload without the metrics_store block failed"
i=0
until ! grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && fail "the metrics_store block is gone, yet rules.yml has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "the metrics_store block is gone: rules.yml has no alerting rule"
journalctl -u motel -o cat --no-pager | grep -q 'metrics_store: the block is gone: vmalert is given no rule' \
	&& pass "journal: the block is gone, no rule" || fail "no 'the block is gone' line"

echo "=== metrics 9: the metrics_store block removed, then a restart, not a reload ==="
# A config with no block builds no MetricsStore, so the boot's gate
# retires the rules a config with one wrote.
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded with both blocks again" || fail "the reload with both blocks failed"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "both blocks again: rules.yml has alerting rules" || fail "rules.yml has no alerting rule with both blocks"
# vmalert loads them before the restart, so the zero after it is its
# re-read of the retired file, not a load it never made.
end=$(( $(date +%s) + 100 ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 5
done
printf '%s' "$rules" | grep -q '"type":"alerting"' && pass "vmalert loads alerting rules again" || fail "vmalert never loaded the rules again: $(printf '%s' "$rules" | cut -c1-300)"
cp /tmp/Caddyfile.nostore /etc/motel/Caddyfile
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart motel && pass "restarted without the metrics_store block" || fail "the restart without the metrics_store block failed"
grep -q 'alert:' "$STATE/rules.yml" && fail "restarted with no block, yet rules.yml has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "restarted with no block: rules.yml has no alerting rule"
journalctl -u motel --since "$since" -o cat --no-pager | grep -q 'metrics_store: the block is gone: vmalert is given no rule' \
	&& pass "journal: the boot retired the rules" || fail "no 'the block is gone' line at the boot"
end=$(( $(date +%s) + 100 ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 5
done
printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' \
	&& pass "vmalert re-read the rules after the restart: zero alerting rules loaded" \
	|| fail "vmalert still loads alerting rules after the restart: $(printf '%s' "$rules" | grep -o '"name":"[^"]*","type":"alerting"' | head -5 | tr '\n' ' ')"
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl restart motel && pass "restored: both blocks, restarted" || fail "the restart with both blocks failed"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "a boot with the block writes its rules" || fail "a boot with the block wrote no alerting rule"

echo
if [ "$FAILURES" -ne 0 ]; then
	echo "metrics suite: $FAILURES failure(s)"
	echo "--- vmalert"; journalctl -u motel-vmalert -o cat --no-pager | tail -30
	echo "--- store"; journalctl -u victoria-metrics -o cat --no-pager | tail -20
	exit 1
fi
echo "metrics suite: all passed"
