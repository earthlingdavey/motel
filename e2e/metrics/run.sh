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
for r in ChannelFailed app_memory_high metrics_store_read_only metrics_store_disk_fill; do
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

echo
if [ "$FAILURES" -ne 0 ]; then
	echo "metrics suite: $FAILURES failure(s)"
	echo "--- vmalert"; journalctl -u motel-vmalert -o cat --no-pager | tail -30
	echo "--- store"; journalctl -u victoria-metrics -o cat --no-pager | tail -20
	exit 1
fi
echo "metrics suite: all passed"
