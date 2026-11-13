#!/bin/sh
# The metrics suite (make e2e-metrics), run inside e2e-metrics-box as root.
#
# The box is the notify suite's with Debian's victoria-metrics installed
# and masked; motel runs e2e/metrics/Caddyfile, the notify suite's
# monitor block plus a `metrics` line. The suite runs `motel monitor
# setup`, then `motel monitor setup --metrics` (the store's alone), then checks
# notify/DESIGN-metrics.md's "e2e rows":
#
#   the first --metrics restarting Alertmanager once, the second not
#   the store's TCP in its namespace only; its socket's mode and reach
#   Alertmanager's own listener there: a SIGKILL, the namespace restarted,
#   stopped and back, failing once, stopped and started by hand with motel
#   down; an upgrade from the proxy shape
#   motel's push (job="motel"), with a deployed app's motel_app_*
#   GET /<app> with the deploy token carries the app's metrics
#   observe's counts in the store and in GET /<app>?range=
#   vmalert's rules loaded; its ChannelFailed and motel's one alert
#   the store keeping of each job only what motel reads
#   the operator's rules: a threshold through vmalert, the post-deploy comparison
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
sinklog() { curl -s --max-time 5 "$SINK/log"; }
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
# nsready: Alertmanager answers on 127.0.0.1:9093 in the namespace.
nsready() { [ "$(inns curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:9093/-/ready 2>/dev/null)" = 200 ]; }
# amstarts <since>: how many times systemd started Alertmanager since then.
amstarts() { journalctl -u prometheus-alertmanager --since "$1" -o cat --no-pager | grep -c '^Started prometheus-alertmanager'; }
aminv() { systemctl show -p InvocationID --value prometheus-alertmanager; }
# val <query>: an instant query's one value.
val() { q "$1" | sed -n 's/.*"value":\[[^,]*,"\([0-9]*\)"\].*/\1/p'; }
amready() { [ "$(am -o /dev/null -w '%{http_code}' http://am/-/ready)" = 200 ]; }
ampid() { systemctl show -p MainPID --value prometheus-alertmanager; }
# held <pid>: that process holds both listeners.
held() {
	[ "$1" != 0 ] && inns ss -ltnpH 2>/dev/null | grep '127.0.0.1:9093 ' | grep -q "pid=$1," \
		&& ss -lxpH 2>/dev/null | grep -F "$AMS " | grep -q "pid=$1,"
}
PROXY=http://localhost:8080
# vmalert re-reads a new rules.yml within RELOAD_WAIT: its 5 s
# -configCheckInterval, where a minute's would mostly miss it.
RELOAD_WAIT=15

# SPLIT: which parts run after the prelude (metrics 0-2, on every box).
# `make e2e-metrics` runs each split on a box of its own, at once; unset,
# every part runs, in order, on one box. A split holds parts that share
# state: 4c reads 4b's restart of the store, 6 the probe release 5
# deploys, 8 and 9 the Caddyfiles 7 and 8 cut. Each split's box is fresh,
# so a part that moves the store, Alertmanager or the Caddyfile moves
# them for its own split alone. The shared sink is read by 10 alone, for
# lines only the rules split's deploy can make.
split_all="vm 3 3a 4 4a 4b 4c 4d 5 6 7 8 9 10 11"
split_view="vm 3 3a" split_vmalert="4 4a" split_store="4b 4c 4d 5 6" split_rules="5 7 8 9 10 11"
case "${SPLIT:=all}" in
all) parts=$split_all ;;
view) parts=$split_view ;;
vmalert) parts=$split_vmalert ;;
store) parts=$split_store ;;
rules) parts=$split_rules ;;
*) echo "FATAL: SPLIT=$SPLIT: all, view, vmalert, store or rules"; exit 1 ;;
esac
# Every part is in all and in some split, and every name in them is a
# part: `make e2e-metrics` runs the splits, so a part in all alone would
# never run there. Refused here, by name, on every box.
for d in $(sed -n 's/^m\([0-9a-z]*\)() {$/\1/p' "$0"); do
	case " $split_all " in *" $d "*) ;; *) echo "FATAL: part $d is not in all"; exit 1 ;; esac
	case " $split_view $split_vmalert $split_store $split_rules " in *" $d "*) ;; *) echo "FATAL: part $d is in no split"; exit 1 ;; esac
done
for d in $split_all $split_view $split_vmalert $split_store $split_rules; do
	grep -q "^m$d() {$" "$0" || { echo "FATAL: $d names no part (no function m$d)"; exit 1; }
done

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

echo "=== metrics 1: setup ==="
# Without --metrics, Alertmanager's setup alone; with it, on a box where
# Alertmanager is set up, the store's alone: Alertmanager's setup would
# mask and stop it, and what it holds in memory would go.
motel monitor setup >/tmp/nsetup.out 2>&1 && pass "monitor setup ran" || fail "monitor setup: $(cat /tmp/nsetup.out)"
[ ! -e /etc/systemd/system/motel-vm.socket ] && [ ! -e /etc/systemd/system/victoria-metrics.service.d/10-motel.conf ] \
	&& pass "without --metrics, nothing of the store is set up" || fail "monitor setup wrote the store's units: $(ls /etc/systemd/system)"
am -o /dev/null http://am/-/ready # started by its socket, if motel's render has not yet
am_run=$(aminv)
ns_before=$(ls -d "/run/netns/$NS" 2>/dev/null)
sleep 1 # journalctl's --since is to the second
since=$(date '+%Y-%m-%d %H:%M:%S')
if motel monitor setup --metrics >/tmp/msetup.out 2>&1; then
	pass "monitor setup --metrics ran: $(tr '\n' ';' </tmp/msetup.out | cut -c1-240)"
else
	fail "monitor setup --metrics: $(cat /tmp/msetup.out)"
fi
! grep -q '^masked prometheus-alertmanager' /tmp/msetup.out && grep -q '^masked victoria-metrics.service' /tmp/msetup.out \
	&& grep -q '^Alertmanager is set up' /tmp/msetup.out \
	&& pass "--metrics ran the store's setup alone, saying Alertmanager's setup is not run again" || fail "--metrics: $(grep -E '^(masked|Alertmanager)' /tmp/msetup.out | tr '\n' ';')"
# Alertmanager takes a socket only when it starts: the first run
# restarts it once, its socket in the namespace with it, and the
# namespace's own first start (in that restart) restarts nothing more.
i=0
until nsready; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 0.5; done
sleep 3 # a restart the namespace's start queued would be under way by now
[ -n "$am_run" ] && [ "$(aminv)" != "$am_run" ] \
	&& [ "$(amstarts "$since")" = 1 ] && grep -q '^restarted Alertmanager, once, so that it listens in the namespace' /tmp/msetup.out && nsready \
	&& pass "the first --metrics restarted Alertmanager once, and it answers on 9093 in the namespace" \
	|| fail "the first --metrics: Alertmanager's invocation '$am_run' before, '$(aminv)' after, $(amstarts "$since") start(s), 9093 $(nsready && echo answers || echo 'does not answer'); setup said: $(grep -i alertmanager /tmp/msetup.out | tr '\n' ';')"
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
# With no namespace before setup, the unit's first start takes the path a
# cold boot takes: no delete of a leftover, so no error the `-` ignores
# (`ip netns delete` of none exits 1 and logs it). Read in the journal:
# setup's `enable`, after the start, reloads systemd, which drops the
# ExecStartPre status it recorded.
nsj=$(journalctl -u motel-metrics-netns -o cat --no-pager)
[ -z "$ns_before" ] && printf '%s\n' "$nsj" | grep -q '^Finished motel-metrics-netns' && ! printf '%s\n' "$nsj" | grep -qi 'cannot remove namespace' \
	&& pass "the namespace's first start, with none before: no leftover's delete, no error logged" \
	|| fail "the namespace's first start, with '$ns_before' there before setup: $(printf '%s' "$nsj" | tail -5 | tr '\n' ';')"
am_run=$(aminv)
sleep 1
since=$(date '+%Y-%m-%d %H:%M:%S')
if motel monitor setup --metrics >/tmp/msetup2.out 2>&1; then
	sleep 3
	[ "$(aminv)" = "$am_run" ] && [ "$(amstarts "$since")" = 0 ] \
		&& grep -q '^Alertmanager listens in the namespace already (motel-am-metrics.socket): left as it runs' /tmp/msetup2.out \
		&& pass "a second --metrics leaves the running Alertmanager as it is (the same invocation), and says so" \
		|| fail "the second --metrics: Alertmanager's invocation '$am_run' before, '$(aminv)' after, $(amstarts "$since") start(s); setup said: $(grep -i alertmanager /tmp/msetup2.out | tr '\n' ';')"
else
	fail "the second monitor setup --metrics: $(cat /tmp/msetup2.out)"
fi
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
# notify's tests over vmalert, vmalert-tool and the store's binary, which `make test` skips
# for want of them: each must run here, none skip. The list is every
# test that calls vmTool (TestE2EMetricsListsEveryVMAlertTest).
mvm() {
echo "=== metrics 1 (vm): notify's vmalert tests ==="
vmtests='^(TestRenderedRulesPassVMAlertDryRun|TestRenderedRulesFireAsMotelsAlerts|TestOperatorRulesPassVMAlertDryRun|TestOperatorRulesFire|TestOperatorRulesCountAfterAGap|TestOperatorRulesHistogramIsReadWhole|TestDiskFillHistoryOverAGap|TestDiskFillStepIsNoTrend|TestOperatorRulesCountOnAFreshStore|TestOperatorRulesReadThePushedApps|TestMetricsViewCountsOverTheStore|TestPostDeployARelaunchReadsPastTheStoppedInstance|TestScrapeConfigPassesTheStoresCheck)$'
nvm=$(printf '%s\n' "$vmtests" | tr '|' '\n' | wc -l)
(cd /tmp && notify.test -test.run "$vmtests" -test.v -test.count=1) >/tmp/vmtests.out 2>&1
[ $? -eq 0 ] && [ "$(grep -c '^--- PASS' /tmp/vmtests.out)" = "$nvm" ] && ! grep -q -- '--- SKIP' /tmp/vmtests.out \
	&& pass "notify's $nvm vmalert tests run and pass against the box's vmalert, vmalert-tool and victoria-metrics" \
	|| fail "notify's vmalert tests: $(grep -E -- '^(--- |FAIL|ok)|SKIP' /tmp/vmtests.out | head -10)"
}
case " $parts " in *" vm "*) mvm ;; esac
# The operator's group: the threshold and disk_fill lines, motel's own
# expressions, each counter through the guard (increase_pure for a
# series new to the store since the push's first sample, else
# increase); post_deploy is motel's.
op=$(sed -n '/name: motel-operator/,$p' "$STATE/rules.yml")
for r in errors_high blog_slow disk_filling; do
	printf '%s' "$op" | grep -q "alert: $r\$" || fail "the motel-operator group has no $r: $(printf '%s' "$op" | head -c 300)"
done
printf '%s' "$op" | grep -q 'increase_pure(motel_app_requests_total' \
	&& printf '%s' "$op" | grep -q 'unless (present_over_time(motel_app_requests_total' \
	&& printf '%s' "$op" | grep -q 'and (tfirst_over_time(motel_app_requests_total' \
	&& printf '%s' "$op" | grep -q 'or increase(motel_app_requests_total' \
	&& printf '%s' "$op" | grep -qF 'and on (app) (max by (app) (tlast_over_time(motel_app_observed[75s])) > scalar(max(tlast_over_time(process_start_time_seconds{job="motel"}[75s]))) - 15)' \
	&& ! grep -q 'blog_regressed' "$STATE/rules.yml" && printf '%s' "$op" | grep -q 'key: rules/app:blog/blog_slow' \
	&& pass "the motel-operator group holds the threshold and disk_fill rules, through the counter guard, keyed rules/<subject>/<name>" \
	|| fail "the motel-operator group: $(printf '%s' "$op" | head -c 600)"

echo "=== metrics 2: the push ==="
code=$(deploy demo-v1.tar.gz v1)
[ "$code" = 200 ] && pass "deploy v1" || fail "deploy v1 returned $code: $(cat /tmp/deploy-body)"
a=$(wait_result push 'process_start_time_seconds{job="motel"}' 120) \
	&& pass "motel's push arrived (job=\"motel\")" || fail "no series of motel's in the store after 120 s"
a=$(wait_result up 'motel_app_up{app="blog"} == 1' 90) \
	&& pass "motel_app_up{app=\"blog\"} is 1" || fail "no motel_app_up for blog: $(q 'motel_app_up')"
a=$(wait_result release 'motel_app_release_info{app="blog",release="v1"}' 30) \
	&& pass "motel_app_release_info names v1" || fail "no release_info for blog v1"
# Observed, and no request through the proxy yet (metrics 3a sends the
# first): the current release's counts are pushed at zero, so that a
# request_rate rule's `<` holds for an app no one has reached.
a=$(wait_result zero 'motel_app_requests_total{app="blog",release="v1",class="2xx"} == 0' 60) \
	&& pass "an observed release with no response yet is pushed at zero" \
	|| fail "no zero count for blog v1 before its first request: $(q 'motel_app_requests_total')"
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

m3() {
echo "=== metrics 3: GET /<app> ==="
# The view's field shows once the store answers the view's own range
# query (notify/metricsview.go: start now-1h, end now, step 60 s), and
# then once the view's 30 s cache of its last answer, empty or not, has
# passed. The store aligns the range's end down to a whole minute, so a
# rate's first point is the minute after the second push: up to 89 s
# after the first push, measured on 1.112.0 (PR #445). The suite waits
# for that point in the store (a cap, not a timing), then gives the view
# its cache and a margin.
rq='sum(rate(motel_app_cpu_seconds_total{app="blog"}[2m]))'
[ "$MEMORY" = 1 ] && rq='{__name__=~"motel_app_memory_bytes|motel_app_memory_max_bytes",app="blog"}'
end=$(( $(date +%s) + 180 ))
while [ "$(date +%s)" -lt "$end" ]; do
	now=$(date +%s)
	r=$(vm -G --data-urlencode "query=$rq" --data-urlencode "start=$((now - 3600))" --data-urlencode "end=$now" \
		--data-urlencode step=60 http://vm/api/v1/query_range)
	has_result "$r" && break
	sleep 2
done
# The view is checked only once the store has the point: with none,
# the store's failure is the one to read.
if has_result "$r"; then
	pass "the store answers the view's range query with a point"
	m=""
	end=$(( $(date +%s) + 40 ))
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
		|| fail "GET /blog's metrics, 40 s after the store had the point: $(printf '%s' "$m" | grep -o '"metrics":.*' | cut -c1-300)"
else
	fail "the store has no point for the view's range query after 180 s (GET /blog's metrics not checked): $(printf '%s' "$r" | head -c 300)"
fi
code=$(curl -s -o /tmp/get-noauth -w '%{http_code}' --max-time 5 "$HOOK")
[ "$code" = 401 ] && ! grep -q metrics /tmp/get-noauth && pass "GET /blog without a token: 401, no metrics" || fail "GET /blog without a token: $code"

}

m3a() {
echo "=== metrics 3a: the response observer ==="
# Twenty misses (the query is in the cache's key), then the same path
# twice: the second is a hit. shop has no release: its 503s are 5xx.
i=1
while [ "$i" -le 20 ]; do curl -s -o /dev/null --max-time 5 "$PROXY/?n=$i"; i=$((i + 1)); done
curl -s -o /dev/null --max-time 5 "$PROXY/same"
hit=$(curl -s -o /dev/null -D - --max-time 5 "$PROXY/same" | tr -d '\r' | sed -n 's/^[Cc]ache-[Ss]tatus: //p')
case "$hit" in *"; hit"*) pass "the cache answered the repeat: $hit" ;; *) fail "the repeat was no hit: '$hit'" ;; esac
for i in 1 2 3; do curl -s -o /dev/null --max-time 5 http://localhost:8085/; done
a=$(wait_result requests 'motel_app_requests_total{app="blog",release="v1",class="2xx"} >= 22' 90) \
	&& pass "motel_app_requests_total counts blog v1's 2xx" || fail "no 2xx count for blog v1: $(q 'motel_app_requests_total')"
a=$(wait_result cachehit 'motel_app_cache_responses_total{app="blog",release="v1",result="hit"} >= 1' 30) \
	&& pass "motel_app_cache_responses_total counts the hit" || fail "no cache hit counted: $(q 'motel_app_cache_responses_total')"
a=$(wait_result cachemiss 'motel_app_cache_responses_total{app="blog",release="v1",result="miss"} >= 21' 30) \
	&& pass "motel_app_cache_responses_total counts the misses" || fail "no cache misses counted: $(q 'motel_app_cache_responses_total')"
a=$(wait_result latency 'motel_app_response_seconds_count{app="blog",release="v1"} >= 21' 30) \
	&& pass "motel_app_response_seconds times the app's own responses" || fail "no latency histogram: $(q 'motel_app_response_seconds_count')"
# Every proxied response so far is in the store (the row above): the
# timed count stands still while the refusals and the abort go by.
lat0=$(q 'motel_app_response_seconds_count{app="blog"}' | sed -n 's/.*"value":\[[^,]*,"\([0-9]*\)"\].*/\1/p')
for i in 1 2 3 4 5; do curl -s -o /dev/null --max-time 5 "$PROXY/refused"; done
curl -s -o /dev/null --max-time 5 "$PROXY/abort"
a=$(wait_result shop5xx 'motel_app_requests_total{app="shop",class="5xx"} >= 3' 30) \
	&& pass "an app with nothing running: its 503s count as 5xx" || fail "no 5xx for shop: $(q 'motel_app_requests_total{app="shop"}')"
a=$(wait_result refused 'motel_app_requests_total{app="blog",class="4xx"} >= 5' 120) \
	&& pass "a refusal inside the observer counts as 4xx" || fail "no 4xx for blog's refusals: $(q 'motel_app_requests_total{app="blog"}')"
lat1=$(q 'motel_app_response_seconds_count{app="blog"}' | sed -n 's/.*"value":\[[^,]*,"\([0-9]*\)"\].*/\1/p')
[ -n "$lat0" ] && [ "$lat0" = "$lat1" ] && pass "the refusals are not timed as the release's ($lat0 timed before and after)" \
	|| fail "the refusals changed the release's timed count: $lat0 -> $lat1"
a=$(wait_result aborted 'motel_app_requests_total{app="blog",class="aborted"} >= 1' 30) \
	&& pass "an aborted request counts as aborted" || fail "no aborted count: $(q 'motel_app_requests_total{app="blog"}')"
m=""
end=$(( $(date +%s) + 120 ))
while [ "$(date +%s)" -lt "$end" ]; do
	m=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK?range=24h")
	printf '%s' "$m" | grep -q '"requests":{"total":[1-9]' && break
	sleep 5
done
printf '%s' "$m" | grep -q '"window":"24h"' && printf '%s' "$m" | grep -q '"requests":{"total":[1-9][0-9]*,"per_second":[0-9.]*,"by_class":{"2xx":[1-9]' \
	&& printf '%s' "$m" | grep -q '"cache_hit_share":0\.[0-9]' \
	&& pass "GET /blog?range=24h carries requests: $(printf '%s' "$m" | grep -o '"requests":{[^}]*}[^}]*}' | cut -c1-240)" \
	|| fail "GET /blog?range=24h's requests: $(printf '%s' "$m" | grep -o '"metrics":.*' | cut -c1-400)"
# The store is minutes old, so either window holds every count: the
# view reads the store's own 2xx count, and the misses (a series that
# came after the push's first sample, at 21) are counted from zero, a
# hit share near 0.05; the store's increase alone dropped them, a share
# of 1 (DESIGN-metrics, "metrics in GET /<app>"). The store's count is
# read again beside each view, over the view's window, so a response
# that arrives meanwhile or a series gone stale moves both.
for r in 1h 24h; do
	m="" raw2xx=""
	end=$(( $(date +%s) + 60 ))
	while [ "$(date +%s)" -lt "$end" ]; do
		raw2xx=$(val "sum(last_over_time(motel_app_requests_total{app=\"blog\",class=\"2xx\"}[$r]))")
		m=$(curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK?range=$r")
		[ -n "$raw2xx" ] && printf '%s' "$m" | grep -q "\"by_class\":{\"2xx\":$raw2xx[,}]" && printf '%s' "$m" | grep -q '"cache_hit_share":0\.0[0-9]*[,}]' && break
		sleep 5
	done
	[ -n "$raw2xx" ] && printf '%s' "$m" | grep -q "\"by_class\":{\"2xx\":$raw2xx[,}]" && printf '%s' "$m" | grep -q '"cache_hit_share":0\.0[0-9]*[,}]' \
		&& pass "GET /blog?range=$r on a fresh store reads the store's $raw2xx 2xx and counts the misses: $(printf '%s' "$m" | grep -o '"cache_hit_share":[0-9.]*')" \
		|| fail "GET /blog?range=$r on a fresh store (the store's 2xx: $raw2xx): $(printf '%s' "$m" | grep -o '"requests":{[^}]*}[^}]*}' | cut -c1-300)"
done
code=$(curl -s -o /tmp/range7d -w '%{http_code}' --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK?range=7d")
[ "$code" = 422 ] && pass "?range=7d is a 422" || fail "?range=7d: $code $(cat /tmp/range7d)"

}

m4() {
echo "=== metrics 4: vmalert ==="
# Setup's first rules.yml is the empty groups and motel writes the real
# one right after, which vmalert re-reads within 5 s (-configCheckInterval).
i=0
until rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules) \
	&& printf '%s' "$rules" | grep -q '"name":"ChannelFailed"'; do
	i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2
done
for r in ChannelFailed app_memory_high metrics_store_disk_fill errors_high blog_slow disk_filling; do
	printf '%s' "$rules" | grep -q "\"name\":\"$r\"" && pass "vmalert loaded $r" || fail "vmalert has no $r: $(printf '%s' "$rules" | cut -c1-200)"
done
# What metrics 7 counts after the channels go: present now, so its
# absence then is not a field the API never had.
printf '%s' "$rules" | grep -q '"type":"alerting"' && pass "vmalert's API marks its rules \"type\":\"alerting\"" \
	|| fail "vmalert's API has no \"type\":\"alerting\": $(printf '%s' "$rules" | cut -c1-300)"
a=$(wait_result vmalert-up 'up{job="vmalert"} == 1' 90) && pass "the store scrapes vmalert" || fail "the store has no up{job=\"vmalert\"}"
a=$(wait_result am-up 'up{job="alertmanager"} == 1' 90) && pass "the store scrapes Alertmanager at its listener in the namespace" \
	|| fail "the store has no up{job=\"alertmanager\"}: $(q 'up')"
# A box event reaches ops and oncall; oncall answers 400, Alertmanager
# counts the failure on event-box-warning's discord, and both motel and
# vmalert post ChannelFailed to ops with suspects=oncall.
motel monitor send --source e2e --name nightly_job_failed --severity warning --summary "e2e: the nightly job failed" >/tmp/send.out 2>&1 \
	|| fail "monitor send: $(cat /tmp/send.out)"
sel='ALERTS{alertname="ChannelFailed",alertstate="firing",notify_channel="ops",integration="discord",suspects="oncall",severity="warning",subject="box"}'
a=$(wait_result channel-failed "$sel" 300) \
	&& pass "vmalert's ChannelFailed fires for ops, naming oncall, with motel's labels" \
	|| fail "no vmalert ChannelFailed after 300 s: $(q 'ALERTS')"
printf '%s' "$a" | grep -q '"alertgroup"' && fail "vmalert's alert carries alertgroup: $a" || pass "no alertgroup label on vmalert's alert"
amq='filter=alertname%3D%22ChannelFailed%22&filter=notify_channel%3D%22ops%22&filter=suspects%3D%22oncall%22&filter=integration%3D%22discord%22'
n=$(am "http://am/api/v2/alerts?$amq" | grep -o '"fingerprint"' | wc -l)
[ "$n" -eq 1 ] && pass "Alertmanager holds one ChannelFailed for ops naming oncall: the two sources are one alert" \
	|| fail "Alertmanager holds $n such alerts: $(am "http://am/api/v2/alerts?$amq" | cut -c1-400)"
# The store keeps of each job only what motel reads (scrape.yml): the
# disk rule's free space, the counter vmalert's ChannelFailed just read,
# and of vmalert nothing but `up`. up and scrape_* are the scraper's own,
# which every job keeps; scrape_* is left out here.
kept() {
	vm -G --data-urlencode "match[]={job=\"$1\"}" http://vm/api/v1/label/__name__/values \
		| sed -n 's/.*"data":\[\([^]]*\)\].*/\1/p' | tr -d '"' | tr ',' '\n' | grep -v '^scrape_' | sort | tr '\n' ' '
}
for jk in 'victoria-metrics:up vm_free_disk_space_bytes ' 'vmalert:up ' 'alertmanager:alertmanager_notifications_failed_total up '; do
	j=${jk%%:*} want=${jk#*:}
	got=$(kept "$j")
	[ "$got" = "$want" ] && pass "the store keeps of job $j: ${want% }" || fail "the store keeps of job $j: '$got', not '$want'"
done
a=$(q 'vm_free_disk_space_bytes{job="victoria-metrics"} > 0')
has_result "$a" && pass "the store reads its own free space live, which metrics_store_disk_fill reads" \
	|| fail "no live vm_free_disk_space_bytes: $(printf '%s' "$a" | cut -c1-200)"
# vmalert's post is counted where it is sent, not read from the merged
# alert: that carries the generatorURL of whichever source posted last,
# and each posts once a minute at a phase of its own (motel after its read
# of Alertmanager's counter; vmalert at its group's, fixed by the group's
# id), so when motel's follows vmalert's by less than a poll, vmalert's is
# never seen (x86 CI at f23c74f: 1 s apart). vmalert sends a firing alert
# at each evaluation of its group (no -rule.resendDelay) and counts a
# failed send before it counts the evaluation. An evaluation may be under
# way at the first reading, so the second one after it, which began after
# that reading, is wholly inside the wait: two evaluations of the group
# (two 1 m intervals and a margin) with no send error while its
# ChannelFailed fires and one alert in Alertmanager is its post taken
# into motel's alert. The sent count is every group's, so it only
# corroborates. A reading must hold the error counter's line: one that
# lacks it (a renamed counter) would read as no error.
evals='vmalert_iteration_duration_seconds_count{group="motel-channel-failed"'
vmsnap() {
	snap=$(inns curl -sf --max-time 5 http://127.0.0.1:8880/metrics) && printf '%s\n' "$snap" | grep -qF "$evals" \
		&& printf '%s\n' "$snap" | grep -q '^vmalert_alerts_send_errors_total{'
}
vmx() { printf '%s\n' "$snap" | awk -v m="$1" 'index($0, m) == 1 {s += $NF} END {print s + 0}'; }
i=0
until vmsnap; do i=$((i + 1)); [ "$i" -ge 5 ] && break; sleep 2; done
ok0=$i ev0=$(vmx "$evals") sent0=$(vmx vmalert_alerts_sent_total) err0=$(vmx vmalert_alerts_send_errors_total)
i=0
until vmsnap && [ "$(vmx "$evals")" -ge $((ev0 + 2)) ]; do
	i=$((i + 1)); [ "$i" -ge 68 ] && break; sleep 2
done
ev=$(vmx "$evals") sent=$(vmx vmalert_alerts_sent_total) err=$(vmx vmalert_alerts_send_errors_total)
firing=$(vmx 'vmalert_alerts_firing{alertname="ChannelFailed"')
body=$(am "http://am/api/v2/alerts?$amq")
n=$(printf '%s' "$body" | grep -o '"fingerprint"' | wc -l)
last=motel
printf '%s' "$body" | grep -q '"generatorURL":"http' && last=vmalert
[ "$ok0" -lt 5 ] && [ "$i" -lt 68 ] && [ "$sent" -gt "$sent0" ] && [ "$err" = "$err0" ] && [ "$firing" -ge 1 ] && [ "$n" -eq 1 ] \
	&& pass "vmalert posted its ChannelFailed and Alertmanager still holds one alert (the latest post: $last's)" \
	|| fail "vmalert's post into the one alert is not shown (the first reading: try $ok0, 5 is none; two evaluations: poll $i, 68 is 135 s and none): evaluations $ev0 -> $ev, sent $sent0 -> $sent, errors $err0 -> $err, firing $firing, $n alerts: $(printf '%s' "$body" | cut -c1-400)"

}

m4a() {
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

}

m4b() {
echo "=== metrics 4b: a read-only store is told by motel, as itself ==="
# The store's free-space floor set above any disk: it is read-only from
# its start, refuses motel's imports and its own scrapes, and no rule
# can see the flag. motel reads it from the store's /metrics.
# The check below reads a sample of the store's free space from before
# the floor: the store split comes here straight from the prelude, which
# may end before the store's first self-scrape.
a=$(wait_result free-space 'vm_free_disk_space_bytes{job="victoria-metrics"}' 60) \
	|| fail "no sample of the store's free space in 60 s, before its read-only start"
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
# Two of the store's 30 s self-scrapes have come and gone by now, and
# neither was kept: the free space the disk rule reads stops with every
# other write. (The flag itself is no series the store keeps, scrape.yml.)
while [ $(( $(date +%s) - ro_at )) -lt 75 ]; do sleep 2; done
before=$(q 'tlast_over_time(vm_free_disk_space_bytes[15m])')
a=$(q "tlast_over_time(vm_free_disk_space_bytes[15m]) > $ro_at")
has_result "$before" && printf '%s' "$a" | grep -q '"status":"success"' && ! has_result "$a" \
	&& pass "75 s read-only and no sample of the store's free space since, only before: the store refused its own scrapes" \
	|| fail "a scrape was kept while read-only, or none before it, or the store did not answer: $(printf '%s' "$before" | cut -c1-200) / $(printf '%s' "$a" | cut -c1-200)"
rm "$dropin/20-e2e-read-only.conf"
systemctl daemon-reload && systemctl restart victoria-metrics
back_at=$(date +%s)
i=0
until vm http://vm/metrics | grep -q '^vm_storage_is_read_only.* 0$'; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 2; done
[ "$i" -lt 30 ] && pass "the store is writable again" || fail "the store is still read-only: $(vm http://vm/metrics | grep '^vm_storage_is_read_only')"
# A sample from after the restart, not one kept from before the floor.
a=$(wait_result vmalert-up-writable "(timestamp(up{job=\"vmalert\"}) > $back_at) and (up{job=\"vmalert\"} == 1)" 120) && pass "and scrapes vmalert again" \
	|| fail "no fresh up{job=\"vmalert\"} after the store came back: $(q 'timestamp(up{job="vmalert"})')"

}

m4c() {
echo "=== metrics 4c: the store's proxy runs as no root ==="
# It runs while it carries traffic (motel's push through vm.sock every
# 30 s); 4b restarted the store, which stops it until the next push.
u=motel-vm.service
i=0
until [ "$(systemctl show -p MainPID --value "$u")" != 0 ]; do i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2; done
pid=$(systemctl show -p MainPID --value "$u")
uid=$(stat -c %u "/proc/$pid" 2>/dev/null)
bnd=$(sed -n 's/^CapBnd:[[:space:]]*//p' "/proc/$pid/status" 2>/dev/null)
[ "$pid" != 0 ] && [ -n "$uid" ] && [ "$uid" != 0 ] && [ "$bnd" = 0000000000000000 ] \
	&& pass "$u runs as uid $uid ($(ps -o user= -p "$pid")), no capability" \
	|| fail "$u: pid $pid, uid '$uid', CapBnd '$bnd'"

}

m4d() {
echo "=== metrics 4d: Alertmanager's own listener in the namespace ==="
# One Alertmanager holds both: am.sock in the host's namespace and
# 127.0.0.1:9093 in the store's, each passed by systemd at its start.
amready
pid=$(ampid)
nsready && held "$pid" && pass "one Alertmanager (pid $pid) holds am.sock and the namespace's 127.0.0.1:9093, and answers on both" \
	|| fail "Alertmanager's listeners (pid $pid): $(inns ss -ltnpH | grep 9093) / $(ss -lxpH | grep -F "$AMS")"
# Killed, it is started again by the next connection to either socket,
# both still systemd's.
systemctl kill -s SIGKILL prometheus-alertmanager
i=0
until [ "$(ampid)" != "$pid" ]; do i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5; done
i=0
until nsready; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 1; done
pid1=$(ampid)
amready && nsready && [ "$pid1" != "$pid" ] && held "$pid1" \
	&& pass "a SIGKILL of Alertmanager: the next connection starts it again (pid $pid1), on both listeners" \
	|| fail "after a SIGKILL of pid $pid: pid '$pid1', 9093 $(nsready && echo answers || echo 'does not answer'), am.sock $(amready && echo answers || echo 'does not answer'), $(systemctl is-active prometheus-alertmanager motel-am-metrics.socket motel-am.socket | tr '\n' ' ')"
# A restart of the namespace restarts the socket in it and, PartOf=,
# Alertmanager: once, the namespace's own restart of it folded in.
inv=$(aminv)
sleep 1
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart motel-metrics-netns
i=0
until nsready; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 1; done
sleep 3
[ "$(aminv)" != "$inv" ] && [ "$(amstarts "$since")" = 1 ] && nsready && held "$(ampid)" \
	&& pass "a restart of the namespace restarts Alertmanager once, which answers on 9093 in the new namespace" \
	|| fail "after a restart of the namespace: $(amstarts "$since") start(s) of Alertmanager, 9093 $(nsready && echo answers || echo 'does not answer'), $(systemctl is-active motel-metrics-netns motel-am-metrics.socket prometheus-alertmanager | tr '\n' ' ')"
# The restart reaches the socket through its Requires= (systemd restarts
# a unit that requires one restarted), not left in the old namespace.
r_at=$(date +%s)
a=$(wait_result am-up-restart "(timestamp(up{job=\"alertmanager\"}) > $r_at) and (up{job=\"alertmanager\"} == 1)" 90) \
	&& pass "and the store, restarted with it, scrapes Alertmanager in the new namespace" \
	|| fail "no fresh up{job=\"alertmanager\"} after the namespace's restart: $(q 'up{job="alertmanager"}')"
# Stopped, the namespace takes Alertmanager down with its socket there;
# am.sock listens on. motel's push brings the store and the namespace
# back, and its read of Alertmanager's counters (a minute) or a post
# starts Alertmanager, whose start brings its socket: nothing by hand.
# The proxy's socket this replaced stayed down here, so vmalert's posts
# and the store's scrape of Alertmanager went nowhere.
t0=$(date +%s)
systemctl stop motel-metrics-netns
! systemctl is-active -q prometheus-alertmanager && ! systemctl is-active -q motel-am-metrics.socket && systemctl is-active -q motel-am.socket \
	&& pass "a stop of the namespace stops Alertmanager and its socket there; am.sock listens on" \
	|| fail "after a stop of the namespace: $(systemctl is-active prometheus-alertmanager motel-am-metrics.socket motel-am.socket | tr '\n' ' ')"
i=0
until systemctl is-active -q motel-metrics-netns && nsready; do i=$((i + 1)); [ "$i" -ge 75 ] && break; sleep 2; done
back=$(date +%s)
[ "$i" -lt 75 ] && pass "the namespace stopped and came back with nothing done by hand: Alertmanager answers on 9093 in it after $((back - t0)) s" \
	|| fail "150 s after a stop of the namespace: $(systemctl is-active motel-metrics-netns victoria-metrics prometheus-alertmanager motel-am-metrics.socket | tr '\n' ' ')"
a=$(wait_result am-up-back "(timestamp(up{job=\"alertmanager\"}) > $back) and (up{job=\"alertmanager\"} == 1)" 90) \
	&& pass "and the store scrapes Alertmanager there again" || fail "no fresh up{job=\"alertmanager\"} after the namespace came back: $(q 'up{job="alertmanager"}')"
# The namespace fails to start (one fault): Alertmanager, started on
# am.sock, comes up on it alone, its socket here failed with the
# namespace, and no later start of that socket could hand it over. The
# namespace's next start, which motel's push brings once the fault is
# gone, restarts it (ExecStartPost=).
NSD=/etc/systemd/system/motel-metrics-netns.service.d
mkdir -p "$NSD"
printf '[Service]\nExecStartPre=/bin/false\n' >"$NSD/99-e2e-fail.conf"
systemctl daemon-reload
systemctl stop motel-metrics-netns
amready # what motel's next post or read does
sleep 1
systemctl is-active -q prometheus-alertmanager && ! systemctl is-active -q motel-am-metrics.socket && ! systemctl is-active -q motel-metrics-netns && amready \
	&& pass "the namespace failing: Alertmanager starts on am.sock alone, and notify is unharmed" \
	|| fail "the namespace failing: $(systemctl is-active prometheus-alertmanager motel-am-metrics.socket motel-metrics-netns | tr '\n' ' '), am.sock $(amready && echo answers || echo 'does not answer')"
rm -f "$NSD/99-e2e-fail.conf"
rmdir "$NSD"
systemctl daemon-reload
inv=$(aminv)
t0=$(date +%s)
i=0
until nsready; do i=$((i + 1)); [ "$i" -ge 60 ] && break; sleep 2; done
[ "$i" -lt 60 ] && [ "$(aminv)" != "$inv" ] && held "$(ampid)" \
	&& pass "the fault gone, the namespace's start (motel's push) restarted Alertmanager, which answers on 9093 again after $(( $(date +%s) - t0 )) s" \
	|| fail "120 s after the namespace's fault went: $(systemctl is-active motel-metrics-netns victoria-metrics motel-vm.socket prometheus-alertmanager motel-am-metrics.socket | tr '\n' ' '), the same Alertmanager: $([ "$(aminv)" = "$inv" ] && echo yes || echo no)"
# With motel down, the namespace stopped and started by hand, then the
# store: nothing reads am.sock, so only vmalert's start (its Wants=)
# starts the socket in the namespace again, and the store's scrape
# starts Alertmanager through it. Without that line the socket and
# Alertmanager stayed inactive until motel came back (150 s measured),
# and vmalert's posts were refused.
systemctl stop motel
systemctl stop motel-metrics-netns
systemctl start motel-metrics-netns victoria-metrics
t0=$(date +%s)
i=0
until systemctl is-active -q motel-am-metrics.socket; do i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5; done
systemctl is-active -q motel-vmalert && systemctl is-active -q motel-am-metrics.socket \
	&& pass "motel down, the namespace stopped and started by hand, the store started: vmalert's start started Alertmanager's socket there" \
	|| fail "motel down, the namespace and the store started by hand: $(systemctl is-active motel-vmalert motel-am-metrics.socket prometheus-alertmanager | tr '\n' ' ')"
a=$(wait_result am-up-nomotel "(timestamp(up{job=\"alertmanager\"}) > $t0) and (up{job=\"alertmanager\"} == 1)" 90) \
	&& systemctl is-active -q prometheus-alertmanager && held "$(ampid)" \
	&& pass "and the store's scrape started Alertmanager through it, with motel still down, on both listeners" \
	|| fail "motel down: no fresh up{job=\"alertmanager\"}, or Alertmanager not on both listeners: $(systemctl is-active motel-am-metrics.socket prometheus-alertmanager | tr '\n' ' '), $(q 'up{job="alertmanager"}')"
systemctl start motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5; done
systemctl is-active -q motel || fail "motel did not start again: $(systemctl is-active motel)"

}

m5() {
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

}

m6() {
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
until journalctl -u motel -o cat --no-pager | grep -q 'monitor metrics: push failed'; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 1
done
journalctl -u motel -o cat --no-pager | grep -q 'monitor metrics: push failed' \
	&& pass "journal: monitor metrics: push failed" || fail "no push failed line"
systemctl start motel-vm.socket

}

m7() {
echo "=== metrics 7: the channels removed, the metrics line kept ==="
# Alertmanager keeps the last config notify rendered, so vmalert must be
# given no rule that could still reach those channels. Every line of the
# monitor block but metrics is cut: a metrics line alone runs no notify.
# vmalert has loaded the alerting rules (at once after metrics 4; a split
# without it can come here first), so their zero below is a re-read.
i=0
until inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules | grep -q '"type":"alerting"'; do
	i=$((i + 1)); [ "$i" -ge 45 ] && break; sleep 2
done
[ "$i" -lt 45 ] && pass "vmalert has loaded the alerting rules before they are cut" \
	|| fail "vmalert loaded no alerting rule in 90 s, so a zero after the cut would prove nothing"
cp /etc/motel/Caddyfile /tmp/Caddyfile.both
awk '/^\tmonitor \{$/ { inb = 1; print; next } inb && /^\t\}$/ { inb = 0; print; next } inb && !/^\t\tmetrics( |$)/ { next } { print }' \
	/etc/motel/Caddyfile >/tmp/Caddyfile.nonotify
if grep -qE '^[[:space:]]*(channel|operator|bind|heartbeat|allow_insecure_channels)( |$)' /tmp/Caddyfile.nonotify \
	|| ! grep -q '^[[:space:]]*monitor {$' /tmp/Caddyfile.nonotify || ! grep -qE '^[[:space:]]*metrics( |$)' /tmp/Caddyfile.nonotify; then
	fail "could not cut the monitor block down to its metrics line: $(sed -n '/monitor {/,/^\t}/p' /tmp/Caddyfile.nonotify)"
fi
cp /tmp/Caddyfile.nonotify /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded with the metrics line alone" || fail "the reload with the metrics line alone failed"
i=0
until ! grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && fail "rules.yml still has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "motel rendered rules.yml with no alerting rule"
end=$(( $(date +%s) + RELOAD_WAIT ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 1
done
printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' \
	&& pass "vmalert re-read the rules: zero alerting rules loaded" \
	|| fail "vmalert still loads alerting rules: $(printf '%s' "$rules" | grep -o '"name":"[^"]*","type":"alerting"' | head -5 | tr '\n' ' ')"
journalctl -u motel -o cat --no-pager | grep -q 'monitor metrics: the monitor block has no channel, only its metrics line: vmalert is given no rule' \
	&& pass "journal: no channel, no rule" || fail "no 'the monitor block has no channel' line"

}

m8() {
echo "=== metrics 8: motel's shutdown keeps the rules; the metrics line removed retires them ==="
# A shutdown leaves the rules, so the store's alerts reach the operator
# while motel is down; a reload to a config with no metrics line writes
# the groups empty, as vmalert and the store still run.
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded with the channels and the metrics line again" || fail "the reload with the whole monitor block failed"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "the whole block again: rules.yml has alerting rules" || fail "rules.yml has no alerting rule with the whole block"
systemctl stop motel
grep -q 'alert:' "$STATE/rules.yml" && pass "motel stopped: rules.yml keeps its alerting rules" \
	|| fail "motel's shutdown retired the rules: $(head -c 300 "$STATE/rules.yml")"
systemctl start motel && pass "motel started again" || fail "motel did not start again"
# The rule lines go with it: they need the metrics line.
awk '/^\t\tmetrics( |$)/ { next } /^\t\trule / { if ($0 ~ /\{$/) inr = 1; next } inr { if ($0 ~ /^\t\t\}$/) inr = 0; next } { print }' \
	/tmp/Caddyfile.both >/tmp/Caddyfile.nostore
if grep -qE '^[[:space:]]*(metrics|rule|window|max_5xx_share)( |$)' /tmp/Caddyfile.nostore || ! grep -q '^[[:space:]]*monitor {$' /tmp/Caddyfile.nostore; then
	fail "could not cut the metrics and rule lines out of the Caddyfile: $(sed -n '/monitor {/,/^\t}/p' /tmp/Caddyfile.nostore)"
fi
cp /tmp/Caddyfile.nostore /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded without the metrics line" || fail "the reload without the metrics line failed"
i=0
until ! grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && fail "the metrics line is gone, yet rules.yml has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "the metrics line is gone: rules.yml has no alerting rule"
journalctl -u motel -o cat --no-pager | grep -q 'monitor metrics: the metrics line is gone: vmalert is given no rule' \
	&& pass "journal: the metrics line is gone, no rule" || fail "no 'the metrics line is gone' line"

}

m9() {
echo "=== metrics 9: the metrics line removed, then a restart, not a reload ==="
# A config with no metrics line builds no MetricsStore, so the boot's
# gate retires the rules a config with one wrote.
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl reload motel && pass "reloaded with the whole monitor block again" || fail "the reload with the whole monitor block failed"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "the whole block again: rules.yml has alerting rules" || fail "rules.yml has no alerting rule with the whole block"
# vmalert loads them before the restart, so the zero after it is its
# re-read of the retired file, not a load it never made.
end=$(( $(date +%s) + RELOAD_WAIT ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 1
done
printf '%s' "$rules" | grep -q '"type":"alerting"' && pass "vmalert loads alerting rules again" || fail "vmalert never loaded the rules again: $(printf '%s' "$rules" | cut -c1-300)"
cp /tmp/Caddyfile.nostore /etc/motel/Caddyfile
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl restart motel && pass "restarted without the metrics line" || fail "the restart without the metrics line failed"
grep -q 'alert:' "$STATE/rules.yml" && fail "restarted with no metrics line, yet rules.yml has alerting rules: $(grep -c 'alert:' "$STATE/rules.yml")" \
	|| pass "restarted with no metrics line: rules.yml has no alerting rule"
journalctl -u motel --since "$since" -o cat --no-pager | grep -q 'monitor metrics: the metrics line is gone: vmalert is given no rule' \
	&& pass "journal: the boot retired the rules" || fail "no 'the metrics line is gone' line at the boot"
end=$(( $(date +%s) + RELOAD_WAIT ))
while [ "$(date +%s)" -lt "$end" ]; do
	rules=$(inns curl -s --max-time 5 http://127.0.0.1:8880/api/v1/rules)
	printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' && break
	sleep 1
done
printf '%s' "$rules" | grep -q '"status":"success"' && ! printf '%s' "$rules" | grep -q '"type":"alerting"' \
	&& pass "vmalert re-read the rules after the restart: zero alerting rules loaded" \
	|| fail "vmalert still loads alerting rules after the restart: $(printf '%s' "$rules" | grep -o '"name":"[^"]*","type":"alerting"' | head -5 | tr '\n' ' ')"
cp /tmp/Caddyfile.both /etc/motel/Caddyfile
systemctl restart motel && pass "restored: the whole monitor block, restarted" || fail "the restart with the whole monitor block failed"
# Post-deploy's queryAt asks for its own offset (latency_offset): a time
# 3 s back is answered as given at 1 s, where the unit's 5 s would move it.
t3=$(( $(date +%s) - 3 ))
a=$(vm -G --data-urlencode 'query=time()' --data-urlencode "time=$t3" --data-urlencode 'latency_offset=1s' http://vm/api/v1/query)
printf '%s' "$a" | grep -q "\"value\":\[$t3," && pass "the store takes a query's own latency_offset" \
	|| fail "latency_offset=1s at $t3 was not answered as given: $a"
i=0
until grep -q 'alert:' "$STATE/rules.yml"; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
grep -q 'alert:' "$STATE/rules.yml" && pass "a boot with the metrics line writes its rules" || fail "a boot with the metrics line wrote no alerting rule"

}

m10() {
echo "=== metrics 10: the operator's rules ==="
# errors_high, a threshold over every app, fires for shop, whose site
# answers 503 with no release; blog_regressed compares blog's next
# release, whose every page answers 500, with the probe release before
# it. Both reach Alertmanager as the operator's rules: source rules,
# the app's label, so route 6 sends them to the app's channels too.
t10=$(date +%s)
i=1
while [ "$i" -le 30 ]; do curl -s -o /dev/null --max-time 5 "$PROXY/?pd=$i"; i=$((i + 1)); done
# No wait for a push: the old release's window ends a push past the
# cutover, so the first push after it carries these answers in time.
code=$(deploy demo-errors.tar.gz errs1)
[ "$code" = 200 ] && pass "deploy errs1, a release whose pages answer 500" || fail "deploy errs1 returned $code: $(cat /tmp/deploy-body)"
c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PROXY/?pe=0")
[ "$c" = 500 ] || fail "fixture: errs1 answers $c, not 500"
amf() { am "http://am/api/v2/alerts?filter=alertname%3D%22$1%22&filter=app%3D%22$2%22"; }
reg="" thr=""
end=$(( $(date +%s) + 300 ))
i=1
while [ "$(date +%s)" -lt "$end" ]; do
	for j in 1 2 3 4 5; do curl -s -o /dev/null --max-time 5 "$PROXY/?pe=$i-$j"; done
	curl -s -o /dev/null --max-time 5 http://localhost:8085/
	i=$((i + 1))
	[ -z "$reg" ] && amf post_deploy_regressed blog | grep -q '"fingerprint"' && reg=$(amf post_deploy_regressed blog)
	[ -z "$thr" ] && amf errors_high shop | grep -q '"fingerprint"' && thr=$(amf errors_high shop)
	[ -n "$reg" ] && [ -n "$thr" ] && break
	sleep 3
done
for l in '"source":"rules"' '"kind":"event"' '"subject":"app:blog"' '"key":"rules/app:blog/post_deploy_regressed"' '"release":"errs1"' '"severity":"warning"'; do
	printf '%s' "$reg" | grep -q "$l" || { fail "post_deploy_regressed in Alertmanager lacks $l: $(printf '%s' "$reg" | cut -c1-500)"; reg=bad; break; }
done
[ -n "$reg" ] && [ "$reg" != bad ] && printf '%s' "$reg" | grep -q '"summary":"blog errs1 regressed [0-9]*m after its deploy, over [0-9]* responses: 5xx share 1 against 0 before it' \
	&& pass "post_deploy_regressed reached Alertmanager in $(( $(date +%s) - t10 )) s, the app's, source rules, both releases' numbers: $(printf '%s' "$reg" | grep -o '"summary":"[^"]*"')" \
	|| fail "no post_deploy_regressed for blog as wanted: $(printf '%s' "$reg" | cut -c1-500)"
for l in '"source":"rules"' '"kind":"state"' '"subject":"app:shop"' '"key":"rules/app:shop/errors_high"' '"generatorURL":"http'; do
	printf '%s' "$thr" | grep -q "$l" || { fail "errors_high in Alertmanager lacks $l: $(printf '%s' "$thr" | cut -c1-500)"; thr=bad; break; }
done
[ -n "$thr" ] && [ "$thr" != bad ] \
	&& pass "vmalert's errors_high fired for shop and reached Alertmanager: source rules, app shop, its key" \
	|| fail "no errors_high for shop as wanted: $(printf '%s' "$thr" | cut -c1-500)"
# The request thresholds' guard (pushedNow) over the pushes the store
# stamped as they came: at every second of the last two minutes, each
# app motel_app_observed named in the last 75 s was in it, its sample
# within 15 s of the push's, and blog and shop were in it throughout
# (115 of the 120 seconds, the subquery's edges aside).
present='max by (app) (tlast_over_time(motel_app_observed[75s]))'
pushed="$present > scalar(max(tlast_over_time(process_start_time_seconds{job=\"motel\"}[75s]))) - 15"
out=$(q "(count_over_time(($present)[2m:1s]) - (count_over_time(($pushed)[2m:1s]) or count_over_time(($present)[2m:1s]) * 0)) > 0")
inq=$(q "count_over_time(($pushed)[2m:1s]) >= 115")
printf '%s' "$out" | grep -q '"result":\[\]' && printf '%s' "$inq" | grep -q '"app":"blog"' && printf '%s' "$inq" | grep -q '"app":"shop"' \
	&& pass "every observed app was in the request thresholds' guard at each second of two minutes, blog and shop throughout" \
	|| fail "the guard left out a pushing app: $(printf '%s' "$out" | cut -c1-300); in it: $(printf '%s' "$inq" | cut -c1-300)"
i=0
until sinklog | grep -q '/blog [a-z]* post_deploy_regressed blog '; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 1
done
sinklog | grep -q '/blog [a-z]* post_deploy_regressed blog ' && sinklog | grep -q '/ops [a-z]* post_deploy_regressed blog ' \
	&& pass "post_deploy_regressed reached the operator's channel and blog's deployer's" \
	|| fail "the sink: $(sinklog | grep post_deploy | tail -5 | tr '\n' ';')"
n=$(journalctl -u motel -o cat --no-pager | grep -c 'rules/app:blog/post_deploy_regressed' || true)
[ "$n" -eq 1 ] && pass "one post_deploy_regressed, journalled once" || fail "post_deploy_regressed journalled $n times"

}

m11() {
echo "=== metrics 11: an upgrade from the proxy shape ==="
# What an earlier setup left: a socket on the namespace's 9093 and a
# proxy from it to Alertmanager's unix socket (their lines that matter
# here), and Alertmanager on am.sock alone. The store's setup, run again,
# retires both and restarts Alertmanager once to take the port.
U=/etc/systemd/system
systemctl disable --now motel-am-metrics.socket # stops Alertmanager too (PartOf=)
rm -f "$U/motel-am-metrics.socket" "$U/prometheus-alertmanager.service.d/20-motel-metrics.conf"
printf '%s\n' '[Unit]' 'DefaultDependencies=no' 'Requires=motel-metrics-netns.service' 'After=sysinit.target motel-metrics-netns.service' \
	'Conflicts=shutdown.target' 'Before=shutdown.target' '[Socket]' 'ListenStream=127.0.0.1:9093' 'NetworkNamespacePath=/run/netns/motel-metrics' \
	'[Install]' 'WantedBy=multi-user.target' >"$U/motel-am-proxy.socket"
printf '%s\n' '[Unit]' 'Requires=motel-am-proxy.socket motel-am.socket' 'After=motel-am-proxy.socket motel-am.socket' '[Service]' 'Type=notify' \
	'NetworkNamespacePath=/run/netns/motel-metrics' "ExecStart=/usr/lib/systemd/systemd-socket-proxyd --exit-idle-time=5min $AMS" >"$U/motel-am-proxy.service"
systemctl daemon-reload
systemctl enable --now motel-am-proxy.socket
amready
nsready && ! held "$(ampid)" && systemctl is-active -q motel-am-proxy.service \
	|| fail "fixture: the proxy shape does not answer on 9093 through its proxy: $(inns ss -ltnpH | grep 9093)"
inv=$(aminv)
sleep 1
since=$(date '+%Y-%m-%d %H:%M:%S')
motel monitor setup --metrics >/tmp/msetup3.out 2>&1 || fail "monitor setup --metrics over the proxy shape: $(cat /tmp/msetup3.out)"
grep -q '^retired motel-am-proxy.socket and motel-am-proxy.service: Alertmanager listens in the namespace itself' /tmp/msetup3.out \
	&& [ ! -e "$U/motel-am-proxy.socket" ] && [ ! -e "$U/motel-am-proxy.service" ] \
	&& ! systemctl is-active -q motel-am-proxy.socket && ! systemctl is-active -q motel-am-proxy.service \
	&& [ ! -e "$U/multi-user.target.wants/motel-am-proxy.socket" ] \
	&& pass "--metrics retired the proxy's socket and service: stopped, disabled and gone, and said so" \
	|| fail "the proxy after --metrics: $(ls "$U" | grep am-proxy | tr '\n' ' ') $(systemctl is-active motel-am-proxy.socket motel-am-proxy.service | tr '\n' ' '); setup said: $(grep -E '^(retired|restarted)' /tmp/msetup3.out)"
i=0
until nsready; do i=$((i + 1)); [ "$i" -ge 30 ] && break; sleep 0.5; done
sleep 3
[ "$(aminv)" != "$inv" ] && [ "$(amstarts "$since")" = 1 ] && nsready && held "$(ampid)" && grep -q '^restarted Alertmanager, once' /tmp/msetup3.out \
	&& pass "and restarted Alertmanager once, which holds 9093 in the namespace itself" \
	|| fail "after the upgrade: $(amstarts "$since") start(s) of Alertmanager, 9093 $(nsready && echo answers || echo 'does not answer'): $(inns ss -ltnpH | grep 9093)"
}

for p in $parts; do [ "$p" = vm ] || "m$p"; done

echo
if [ "$FAILURES" -ne 0 ]; then
	echo "metrics suite ($SPLIT): $FAILURES failure(s)"
	echo "--- vmalert"; journalctl -u motel-vmalert -o cat --no-pager | tail -30
	echo "--- store"; journalctl -u victoria-metrics -o cat --no-pager | tail -20
	echo "--- Alertmanager and its sockets"; journalctl -u prometheus-alertmanager -u motel-am-metrics.socket -u motel-am.socket -u motel-metrics-netns --no-pager | tail -40
	exit 1
fi
echo "metrics suite ($SPLIT): all passed"
