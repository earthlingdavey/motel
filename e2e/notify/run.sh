#!/bin/sh
# The notify suite (make e2e-notify), run inside e2e-notify-box as root.
#
# The box is e2e-motel's image with Debian's prometheus-alertmanager
# installed and masked; motel runs the monitor block in
# e2e/notify/Caddyfile, whose channels are paths on e2e-sink, a stub
# that logs every delivery with its time (e2e/notify/sink.py). The suite
# runs `motel monitor setup`, then reproduces the timeline measured on
# hotserve-1 against the corrected route tree
# (research: alertmanager_edge_tests.md, its addendum), through motel:
#
#   Watchdog                      → /deadman, at once
#   deploy_failed (a real deploy) → /ops and /blog, the same second
#   a box one-shot (monitor send) → /ops only, at once
#   four distinct one-shots       → four deliveries, none lost
#   watchdog_throttled blog       → nothing: inhibited by deploy_in_progress
#   watchdog_throttled shop       → /ops, after the 30 s group_wait
#   a box state (monitor send)    → /ops at +30 s; its recovery within a minute of motel's hold
#   oncall (answers 400)          → ChannelFailed to /ops, naming oncall
#   blog-deployer failing         → ChannelFailed to /ops (json beside its ntfy), naming it alone
#   oncall the only operator      → the heartbeat held after 5 m, resumed by a delivery
#
# and the socket's reach: no TCP listener, root and motel admitted,
# nobody refused, nothing of it in an app's sandbox view; the journal
# lines; no channel URL in any journal, the rendered config or
# Alertmanager's API.
. /lib.sh

HOOK=http://localhost:8081/blog
ART=http://e2e-artifacts:8080/artifacts
SINK=http://e2e-sink:8080
SOCK=/run/motel-notify/am.sock
STATE=/var/lib/motel-notify
am() { curl -s --max-time 10 --unix-socket "$SOCK" "$@"; }
now_ms() { date +%s%3N; }
# This box's deliveries alone: the sink is shared by every split's box.
sinklog() { curl -s --max-time 5 "$SINK/log?mine=1"; }
# arrivals <path> <alertname> <since ms> [alert status] [app]: the
# sink's lines for one alert at one path, from a time on.
arrivals() {
	sinklog | awk -v p="$1" -v a="$2" -v s="$3" -v st="${4:-}" -v app="${5:-}" \
		'$2==p && $4==a && $1>=s && (st=="" || $6==st) && (app=="" || $5==app)'
}
# first_arrival <path> <alertname> <since ms> <timeout s> [status] [app]:
# the ms of the first such delivery, or nothing.
first_arrival() {
	end=$(( $(date +%s) + $4 ))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(arrivals "$1" "$2" "$3" "${5:-}" "${6:-}" | head -1 | cut -d' ' -f1)
		[ -n "$t" ] && { echo "$t"; return 0; }
		sleep 0.25
	done
	return 1
}
# within <what> <arrival ms|empty> <from ms> <min s> <max s>
within() {
	if [ -z "$2" ]; then fail "$1: never delivered"; return; fi
	d=$(( $2 - $3 ))
	if [ "$d" -ge $(( $4 * 1000 )) ] && [ "$d" -le $(( $5 * 1000 )) ]; then
		pass "$1: delivered $(awk -v d="$d" 'BEGIN{printf "%.1f", d/1000}') s after the post"
	else
		fail "$1: delivered $d ms after the post, want $4 to $5 s"
	fi
}
# none <what> <path> <alertname> <since ms> [app]
none() {
	n=$(arrivals "$2" "$3" "$4" "" "${5:-}" | wc -l)
	[ "$n" -eq 0 ] && pass "$1" || fail "$1: $n deliveries"
}
post_alert() { # <labels JSON>: as root, as an admin's own tool would
	am -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
		http://am/api/v2/alerts -d "[{\"labels\":$1,\"annotations\":{\"summary\":\"e2e: posted to the socket by root\"}}]"
}
send() { motel monitor send --source e2e "$@" >/tmp/send.out 2>&1; }
# failing_receivers: the receivers Alertmanager's failed counter names.
failing_receivers() {
	am http://am/metrics | awk '/^alertmanager_notifications_failed_total\{/ && $NF+0 > 0 {
		if (match($0, /receiver_name="[^"]*"/)) print substr($0, RSTART+15, RLENGTH-16) }' | sort -u
}

# SPLIT: which parts run after the prelude (notify 0-2, on every box).
# `make e2e-notify` runs each split on a box of its own, at once, sharing
# the sink, which answers each box with its own deliveries; unset, every
# part runs, in order, on one box. A split holds parts that share state:
# 6 and 9 count every delivery since the box began, 7a silences 3's
# failed v3. 15, five minutes of a held heartbeat and its resume, is a
# split alone; 11 to 14 are another, near core's length.
split_core="3 4 5 6 6b 7 7a 8 9 10 10a" split_reload="11 12 13 14" split_heartbeat="15"
split_all="$split_core $split_reload $split_heartbeat"
case "${SPLIT:=all}" in
all) parts=$split_all ;;
core) parts=$split_core ;;
reload) parts=$split_reload ;;
heartbeat) parts=$split_heartbeat ;;
*) echo "FATAL: SPLIT=$SPLIT: all, core, reload or heartbeat"; exit 1 ;;
esac
# Every part is in all and in some split, and every name in them is a
# part: `make e2e-notify` runs the splits, so a part in all alone would
# never run there. Refused here, by name, on every box.
for d in $(sed -n 's/^case_\([0-9a-z]*\)() {$/\1/p' "$0"); do
	case " $split_all " in *" $d "*) ;; *) echo "FATAL: part $d is not in all"; exit 1 ;; esac
	case " $split_core $split_reload $split_heartbeat " in *" $d "*) ;; *) echo "FATAL: part $d is in no split"; exit 1 ;; esac
done
for d in $split_all $split_core $split_reload $split_heartbeat; do
	grep -q "^case_$d() {\$" "$0" || { echo "FATAL: $d names no part (no function case_$d)"; exit 1; }
done

echo "=== notify 0: the box ==="
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && { echo "FATAL: motel did not come up"; journalctl -u motel --no-pager -n 50; exit 1; }
	sleep 0.5
done
wait_for_token
journalctl -u motel -o cat --no-pager | grep -q 'Alertmanager is not set up on this box' \
	&& pass "before setup, motel says Alertmanager is not set up, and serves" \
	|| fail "no 'not set up' line before setup"
# What the journal says, the commands say too.
motel validate --adapter caddyfile --config /etc/motel/Caddyfile >/tmp/validate-0.out 2>&1
grep -q 'WARNING: monitor: Alertmanager is not set up on this box' /tmp/validate-0.out \
	&& pass "before setup, motel validate warns that Alertmanager is not set up" || fail "validate before setup: $(tail -3 /tmp/validate-0.out)"
if send --name before_setup_probe --summary "e2e: sent before setup"; then
	fail "monitor send before setup exits 0: $(cat /tmp/send.out)"
else
	grep -q 'delivered nowhere.*run motel monitor setup as root' /tmp/send.out \
		&& pass "monitor send before setup exits 1, naming setup" || fail "monitor send before setup: $(cat /tmp/send.out)"
fi
out=$(motel monitor test ops 2>&1) && fail "monitor test before setup succeeded: $out" || {
	echo "$out" | grep -q 'run motel monitor setup as root' \
		&& pass "monitor test before setup names setup" || fail "monitor test before setup: $out"
}

echo "=== notify 1: motel monitor setup ==="
t_setup=$(now_ms)
if motel monitor setup >/tmp/setup.out 2>&1; then
	pass "setup ran: $(tr '\n' ';' </tmp/setup.out | cut -c1-200)"
else
	fail "setup: $(cat /tmp/setup.out)"
fi
listeners=$(ss -ltnH 2>/dev/null | awk '{print $4}' | grep -E ':(9093|9094)$' || true)
[ -z "$listeners" ] && pass "no TCP listener on 9093 or 9094" || fail "listening: $listeners"
[ "$(stat -c '%A %U %G' "$SOCK")" = "srw-rw---- root motel" ] \
	&& pass "the socket is srw-rw---- root motel" || fail "the socket is $(stat -c '%A %U %G' "$SOCK" 2>&1)"
[ "$(stat -c '%A %U %G' "$STATE")" = "drwxr-s--- motel prometheus" ] \
	&& pass "$STATE is drwxr-s--- motel prometheus" || fail "$STATE is $(stat -c '%A %U %G' "$STATE" 2>&1)"
# Debian ships it 0755: the drop-in sets the mode at every start, so no
# other uid reads the notification log or the silences. A connection to
# the socket starts Alertmanager, if motel's render has not yet.
curl -s -o /dev/null --max-time 10 --unix-socket "$SOCK" http://am/-/ready
[ "$(stat -c '%A %U %G' /var/lib/prometheus/alertmanager)" = "drwxr-x--- prometheus prometheus" ] \
	&& pass "Alertmanager's storage is drwxr-x--- prometheus prometheus" \
	|| fail "Alertmanager's storage is $(stat -c '%A %U %G' /var/lib/prometheus/alertmanager 2>&1)"
i=0
until grep -q 'Rendered by motel' "$STATE/alertmanager.yml" 2>/dev/null; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.25
done
grep -q 'Rendered by motel' "$STATE/alertmanager.yml" \
	&& pass "motel rendered the monitor block into $STATE/alertmanager.yml" || fail "the config is not motel's: $(head -3 "$STATE/alertmanager.yml")"
bad=$(find "$STATE" -name 'url-*' ! -perm 0640 -o -name 'url-*' ! -group prometheus | head -1)
[ -z "$bad" ] && [ -n "$(find "$STATE" -name 'url-*')" ] && pass "the URL files are 0640, group prometheus" || fail "a URL file: $bad"
amtool check-config "$STATE/alertmanager.yml" 2>&1 | grep -q SUCCESS \
	&& pass "amtool check-config passes the rendered config" || fail "amtool: $(amtool check-config "$STATE/alertmanager.yml" 2>&1 | tail -3)"
[ "$(am -o /dev/null -w '%{http_code}' http://am/-/ready)" = 200 ] && pass "root reaches Alertmanager on the socket" || fail "root: not ready"
[ "$(runuser -u motel -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 --unix-socket "$SOCK" http://am/-/ready)" = 200 ] \
	&& pass "the motel user reaches it" || fail "the motel user is refused"
if runuser -u nobody -- curl -s -o /dev/null --max-time 5 --unix-socket "$SOCK" http://am/-/ready; then
	fail "nobody reached Alertmanager's socket"
else
	pass "nobody is refused the socket (curl exit $?)"
fi
for f in "$STATE/alertmanager.yml"; do
	grep -q 'e2e-sink' "$f" && fail "a channel URL is in $f" || pass "no channel URL in $f"
done
am http://am/api/v2/status | grep -q 'e2e-sink' && fail "Alertmanager's status echoes a channel URL" || pass "Alertmanager's status echoes no channel URL"

# The Caddyfile as the box began, which 11, 14 and 15 put back: kept
# here, before any part edits it, for every split.
cp /etc/motel/Caddyfile /tmp/Caddyfile.before

echo "=== notify 2: the heartbeat ==="
t=$(first_arrival /deadman Watchdog "$t_setup" 90 firing)
within "Watchdog to the heartbeat" "$t" "$t_setup" 0 90

case_3() {
echo "=== notify 3: a real deploy fails ==="
code=$(deploy demo-v1.tar.gz v1)
[ "$code" = 200 ] && pass "deploy v1" || fail "deploy v1 returned $code: $(cat /tmp/deploy-body)"
t_v1=$(now_ms)
code=$(deploy demo-v3-broken.tar.gz v3)
t_v3=$(now_ms)
[ "$code" = 500 ] && pass "deploy v3 fails (its health check never passes)" || fail "deploy v3 returned $code"
t1=$(first_arrival /ops deploy_failed "$t_v1" 15 "" blog)
t2=$(first_arrival /blog deploy_failed "$t_v1" 15 "" blog)
within "deploy_failed to the operator" "$t1" "$t_v3" -2 3
within "deploy_failed to blog's deployer" "$t2" "$t_v3" -2 3
arrivals /blog deploy_failed "$t_v1" | head -1 | grep -q ' 4$' \
	&& pass "to the deployer's ntfy channel at ntfy's high priority (a warning)" \
	|| fail "deploy_failed to ntfy without X-Priority 4: $(arrivals /blog deploy_failed "$t_v1" | head -1)"
none "deploy_succeeded is off by default" /ops deploy_succeeded "$t_v1"
none "deploy_in_progress reaches nobody (operator)" /ops deploy_in_progress 0
none "deploy_in_progress reaches nobody (deployer)" /blog deploy_in_progress 0
journalctl -u motel -o cat --no-pager | grep 'monitor: event' | grep -q 'liveswap/app:blog/deploy_failed' \
	&& pass "journal: monitor: event liveswap/app:blog/deploy_failed" || fail "no monitor: event line for the failed deploy"

}

case_4() {
echo "=== notify 4: one-shots from outside ==="
t_box=$(now_ms)
send --name nightly_job_failed --severity warning --summary "e2e: the nightly job failed" \
	&& pass "monitor send: $(cat /tmp/send.out)" || fail "monitor send: $(cat /tmp/send.out)"
t=$(first_arrival /ops nightly_job_failed "$t_box" 10)
within "a box one-shot to the operator" "$t" "$t_box" 0 2
sleep 1
none "a box one-shot reaches no deployer" /blog nightly_job_failed "$t_box"
t_four=$(now_ms)
for n in 1 2 3 4; do send --name batch_item_failed --instance "item-$n" --summary "e2e: item $n failed"; done
sleep 3
n=$(arrivals /ops batch_item_failed "$t_four" | wc -l)
[ "$n" -eq 4 ] && pass "four distinct one-shots in a burst: four deliveries, none lost" || fail "four one-shots: $n deliveries"
send --subject app:nosuch --name x_failed --summary "e2e" && fail "a subject naming no app was accepted" || pass "refused: $(tail -1 /tmp/send.out)"
send --name deploy_in_progress --subject app:blog --summary "e2e" && fail "the mute's name was accepted" || pass "refused: $(tail -1 /tmp/send.out)"
send --name watchdog_throttled --subject app:blog --summary "e2e" && fail "a watchdog name was accepted" || pass "refused: $(tail -1 /tmp/send.out)"
if runuser -u nobody -- motel monitor send --source e2e --name x_failed --summary e2e >/tmp/nobody.out 2>&1; then
	fail "nobody posted through the admin socket"
else
	pass "nobody cannot reach motel's admin socket to send"
fi

}

case_5() {
echo "=== notify 5: state alerts, inhibition, recovery ==="
[ "$(post_alert '{"alertname":"deploy_in_progress","kind":"mute","app":"blog","severity":"none"}')" = 200 ] || fail "posting the mute"
t_state=$(now_ms)
post_alert '{"alertname":"watchdog_throttled","kind":"state","app":"blog","subject":"app:blog","severity":"critical"}' >/dev/null
post_alert '{"alertname":"watchdog_throttled","kind":"state","app":"shop","subject":"app:shop","severity":"critical"}' >/dev/null
send --name disk_check --state firing --severity warning --summary "e2e: a state from outside" || fail "send firing: $(cat /tmp/send.out)"
t=$(first_arrival /ops watchdog_throttled "$t_state" 60 firing shop)
within "watchdog_throttled shop to the operator (group_wait)" "$t" "$t_state" 28 45
t=$(first_arrival /ops disk_check "$t_state" 60 firing)
within "a box state to the operator (group_wait)" "$t" "$t_state" 28 45
none "watchdog_throttled blog: inhibited, to the deployer" /blog watchdog_throttled "$t_state"
none "watchdog_throttled blog: inhibited, to the operator" /ops watchdog_throttled "$t_state" blog
am 'http://am/api/v2/alerts?filter=alertname%3D%22watchdog_throttled%22&filter=app%3D%22blog%22' | grep -q '"state":"suppressed"' \
	&& pass "Alertmanager holds watchdog_throttled blog suppressed (inhibitedBy deploy_in_progress)" || fail "watchdog_throttled blog is not suppressed"
t_res=$(now_ms)
send --name disk_check --state resolved --severity warning --summary "e2e: a state from outside" || fail "send resolved: $(cat /tmp/send.out)"
t=$(first_arrival /ops disk_check "$t_res" 120 resolved)
within "the recovery, after motel's 30 s hold and a one-minute tick" "$t" "$t_res" 29 100
journalctl -u motel -o cat --no-pager | grep 'monitor: event' | grep 'external:e2e/box/disk_check' | grep -q '"state":"resolved"' \
	&& pass "journal: the resolve's monitor: event line" || fail "no resolved line for disk_check"

}

case_6() {
echo "=== notify 6: a failing channel ==="
n=$(arrivals /broken - 0 | wc -l)
[ "$n" -gt 0 ] && pass "oncall was tried $n times and refused (400)" || fail "oncall was never tried"
t=$(first_arrival /ops ChannelFailed 0 90)
if [ -n "$t" ]; then
	pass "ChannelFailed reached the operator's other channel"
	arrivals /ops ChannelFailed 0 | head -1 | grep -q ' ops$' && pass "addressed to channel ops alone" || fail "ChannelFailed addressed elsewhere: $(arrivals /ops ChannelFailed 0 | head -1)"
else
	fail "no ChannelFailed within 90 s"
fi
journalctl -u motel -o cat --no-pager | grep 'monitor: channel failed' | grep -q 'oncall' \
	&& pass "journal: monitor: channel failed names oncall" || fail "no channel failed line naming oncall"
# A receiver per channel: Alertmanager's failed counter names oncall's
# receivers alone, and the alert names oncall alone.
r=$(failing_receivers)
[ -n "$r" ] && ! printf '%s\n' "$r" | grep -qv -- '-oncall$' \
	&& pass "the failed counter names oncall's receivers alone: $(echo $r)" || fail "failing receivers: $(echo $r)"
am 'http://am/api/v2/alerts?filter=alertname%3D%22ChannelFailed%22&filter=notify_channel%3D%22ops%22' | grep -q '"suspects":"oncall"' \
	&& pass "ChannelFailed to ops names oncall alone (suspects=oncall)" || fail "ChannelFailed: $(am 'http://am/api/v2/alerts?filter=alertname%3D%22ChannelFailed%22' | head -c 400)"

}

case_6b() {
echo "=== notify 6b: two channels of one integration: the failing one alone is named ==="
# ops (json) and blog-deployer (ntfy) are both Alertmanager's webhook
# integration, and both hear blog's events. Under a receiver they
# shared, blog-deployer's failure named both, and only oncall (which
# fails) was told; with a receiver each it names blog-deployer, and ops
# hears it.
cp -p /etc/motel/secrets/_box.env /tmp/_box.env.6b
sed -i 's#^NOTIFY_BLOG_URL=.*#NOTIFY_BLOG_URL="http://e2e-sink:8080/broken"#' /etc/motel/secrets/_box.env
motel reload --config /etc/motel/Caddyfile >/tmp/reload-6b.out 2>&1 || fail "the reload with blog-deployer failing: $(tail -3 /tmp/reload-6b.out)"
t_6b=$(now_ms)
s_6b=$(date +%s)
send --subject app:blog --name probe_6b_failed --severity warning --summary "e2e: to ops and blog-deployer" \
	|| fail "send: $(cat /tmp/send.out)"
t=$(first_arrival /ops probe_6b_failed "$t_6b" 10)
[ -n "$t" ] && pass "the event reached ops" || fail "the event never reached ops"
i=0
until am 'http://am/api/v2/alerts?filter=alertname%3D%22ChannelFailed%22&filter=suspects%3D%22blog-deployer%22' | grep -q '"notify_channel":"ops"'; do
	i=$((i + 1)); [ "$i" -ge 100 ] && break; sleep 1
done
am 'http://am/api/v2/alerts?filter=alertname%3D%22ChannelFailed%22&filter=suspects%3D%22blog-deployer%22' | grep -q '"notify_channel":"ops"' \
	&& pass "ChannelFailed to ops names blog-deployer alone, $(( $(date +%s) - s_6b )) s after" \
	|| fail "no ChannelFailed to ops naming blog-deployer alone: $(am 'http://am/api/v2/alerts?filter=alertname%3D%22ChannelFailed%22' | head -c 600)"
t=$(first_arrival /ops ChannelFailed "$t_6b" 30)
[ -n "$t" ] && pass "and ops, the other webhook channel, was delivered it" || fail "no ChannelFailed reached ops after the probe"
journalctl -u motel --since "@$s_6b" -o cat --no-pager | grep 'monitor: channel failed' | grep '"suspects":\["blog-deployer"\]' | grep -q '"told":\["ops","oncall"\]' \
	&& pass "journal: monitor: channel failed, suspects [blog-deployer], told [ops oncall]" \
	|| fail "journal: $(journalctl -u motel --since "@$s_6b" -o cat --no-pager | grep 'monitor: channel failed' | tail -2)"
printf '%s\n' "$(failing_receivers)" | grep -q '^event-warning-blog-deployer$' \
	&& pass "the failed counter names event-warning-blog-deployer" || fail "failing receivers: $(echo $(failing_receivers))"
cp -p /tmp/_box.env.6b /etc/motel/secrets/_box.env
motel reload --config /etc/motel/Caddyfile >/tmp/reload-6b.out 2>&1 || fail "the reload that mends blog-deployer: $(tail -3 /tmp/reload-6b.out)"

}

case_7() {
echo "=== notify 7: motel monitor test ==="
out=$(motel monitor test ops 2>&1) && echo "$out" | grep -q 'delivered to ops' \
	&& pass "monitor test ops: $out" || fail "monitor test ops: $out"
out=$(motel monitor test oncall 2>&1) && fail "monitor test oncall succeeded: $out" || pass "monitor test oncall fails: $(echo "$out" | tail -1)"

}

case_7a() {
echo "=== notify 7a: motel monitor silence ==="
# rollback for blog, as liveswap's one-shot carries it, posted by root
# so that the suite need not roll a release back.
rollback() { post_alert "{\"alertname\":\"rollback\",\"kind\":\"event\",\"app\":\"blog\",\"subject\":\"app:blog\",\"severity\":\"warning\",\"source\":\"liveswap\",\"key\":\"liveswap/app:blog/rollback/$1\"}" >/dev/null; }
t_sil=$(date +%s)
out=$(motel monitor silence add --json --comment "e2e: a silence through motel" --duration 10m --alert rollback --app blog 2>&1)
id=$(echo "$out" | sed -n 's/^{"id":"\([0-9a-f-]\{36\}\)","ends_at":"[0-9T:.Z-]*"}$/\1/p')
[ -n "$id" ] && pass "silence add --json: $out" || fail "silence add --json: $out"
sil=$(am "http://am/api/v2/silence/$id")
echo "$sil" | grep -q '"state":"active"' && echo "$sil" | grep -q '"createdBy":"motel monitor: root"' \
	&& echo "$sil" | grep -q '"name":"alertname","value":"rollback"' && echo "$sil" | grep -q '"name":"app","value":"blog"' \
	&& pass "Alertmanager holds it active, alertname=rollback app=blog, created by motel monitor: root" || fail "Alertmanager's silence: $sil"
am http://am/api/v2/silences | grep -q "\"id\":\"$id\"" && pass "and lists it in /api/v2/silences" || fail "not in /api/v2/silences"
journalctl -u motel --since "@$t_sil" -o cat --no-pager | grep 'monitor: silence added' | grep -qF "$id" \
	&& pass "journal: monitor: silence added, with its id" || fail "no silence added line for $id"
t=$(now_ms)
rollback e2e-silenced
sleep 10
none "blog's rollback under the silence reaches no channel" /ops rollback "$t" blog
out=$(motel monitor silence add --comment "e2e: the heartbeat" --alert Watchdog 2>&1) \
	&& fail "a silence of Watchdog was added: $out" \
	|| { echo "$out" | grep -q -- '--alert names none of the alerts motel raises on this box; they are .*disk_low' \
		&& ! echo "$out" | grep -q 'Watchdog' && pass "a silence of Watchdog is refused, the valid names listed" || fail "Watchdog: $out"; }
out=$(motel monitor silence add --comment "e2e: unknown" --alert no_such_alert 2>&1) \
	&& fail "an unknown alert was silenced: $out" \
	|| { echo "$out" | grep -q -- '--alert names none' && ! echo "$out" | grep -q no_such_alert \
		&& pass "an unknown alert is refused, its name not echoed" || fail "unknown alert: $out"; }
out=$(motel monitor silence add --comment "e2e: a box alert by app" --alert disk_low --app blog 2>&1) \
	&& fail "a box alert was silenced by app: $out" \
	|| { echo "$out" | grep -q 'disk_low is about the box, not an app' && pass "a box alert with --app is refused: it would match nothing" || fail "disk_low --app: $out"; }
out=$(runuser -u nobody -- motel monitor silence add --comment x --alert disk_low 2>&1) \
	&& fail "nobody added a silence: $out" \
	|| { echo "$out" | grep -q 'admits root and the motel user' && pass "nobody is refused, told to run it as root" || fail "nobody: $out"; }
out=$(motel monitor silence expire "$id" 2>&1)
[ "$out" = "silence $id expired" ] && pass "silence expire: $out" || fail "silence expire: $out"
am "http://am/api/v2/silence/$id" | grep -q '"state":"expired"' && pass "Alertmanager has it expired" || fail "after expire: $(am "http://am/api/v2/silence/$id")"
journalctl -u motel --since "@$t_sil" -o cat --no-pager | grep 'monitor: silence expired' | grep -qF "$id" \
	&& pass "journal: monitor: silence expired, with its id" || fail "no silence expired line for $id"
t=$(now_ms)
rollback e2e-after
t_arr=$(first_arrival /ops rollback "$t" 30 "" blog)
[ -n "$t_arr" ] && pass "after expire, blog's rollback reaches the operator again" || fail "no rollback after the expire"
out=$(motel monitor silence expire --json "$id" 2>&1) && fail "a second expire succeeded: $out" \
	|| { echo "$out" | grep -qF '{"error":"silence '"$id"' has already expired"}' && pass "a second expire exits 1: $out" || fail "a second expire: $out"; }
# v3's deploy failed in notify 3: its release was removed, its record
# kept, so --release takes it with deploy_failed. The silence is left to
# outlive the alert posted under it (resolve_timeout, 5m): expired
# earlier, the alert would be delivered late and skew notify 9's count.
out=$(motel monitor silence add --json --comment "e2e: v3 is known broken" --duration 15m --alert deploy_failed --app blog --release v3 2>&1)
fid=$(echo "$out" | sed -n 's/^{"id":"\([0-9a-f-]\{36\}\)","ends_at":"[0-9T:.Z-]*"}$/\1/p')
[ -n "$fid" ] && am "http://am/api/v2/silence/$fid" | grep -q '"name":"release","value":"v3"' \
	&& pass "a failed deploy's recorded release is silenced by --release v3" || fail "deploy_failed --release v3: $out"
# blog's deploy_failed for v3, as liveswap's one-shot carries it.
t=$(now_ms)
code=$(post_alert '{"alertname":"deploy_failed","kind":"event","app":"blog","release":"v3","subject":"app:blog","severity":"warning","source":"liveswap","key":"liveswap/app:blog/deploy_failed/e2e-silenced"}')
[ "$code" = 200 ] || fail "posting blog's deploy_failed of v3 returned $code"
sleep 10
none "blog's deploy_failed of v3 under the silence reaches no operator" /ops deploy_failed "$t" blog
none "nor blog's deployer" /blog deploy_failed "$t" blog
out=$(motel monitor silence add --comment "e2e: refused" --alert deploy_refused --app blog --release v3 2>&1) \
	&& fail "deploy_refused was silenced by --release: $out" \
	|| { echo "$out" | grep -q "a refusal writes no record" && pass "deploy_refused with --release is refused" || fail "deploy_refused --release: $out"; }
out=$(motel monitor silence add --comment "e2e: unknown" --alert deploy_failed --app blog --release v9 2>&1) \
	&& fail "a release neither on disk nor recorded was silenced: $out" \
	|| { echo "$out" | grep -q -- '--release names no release of blog on this box; they are .*v3' && ! echo "$out" | grep -q v9 \
		&& pass "a release neither on disk nor recorded is refused, the known ones listed" || fail "--release v9: $out"; }

}

case_8() {
echo "=== notify 8: an app's sandbox has none of it in view ==="
code=$(deploy demo-probe.tar.gz sb1)
[ "$code" = 200 ] || fail "deploy of the probe release returned $code: $(cat /tmp/deploy-body)"
probe=/var/lib/liveswap/blog/shared/view.txt
i=0
until grep -q '^done=1' "$probe" 2>/dev/null; do i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5; done
[ -S "$SOCK" ] && [ -d "$STATE" ] || fail "the host has no socket or state dir; the probe would prove nothing"
for k in notify_run notify_socket notify_state; do
	v=$(sed -n "s/^$k=//p" "$probe" | head -1)
	[ "$v" = closed ] && pass "inside the sandbox: $k=closed" || fail "inside the sandbox: $k=$v"
done

}

case_9() {
echo "=== notify 9: what was delivered once stays delivered once ==="
# Alertmanager retried the failing oncall at every tick while each alert
# lived; the operator's own channel, delivered, must have had each alert
# once only.
for a in nightly_job_failed deploy_failed; do
	n=$(arrivals /ops "$a" 0 | wc -l)
	want=1; [ "$a" = deploy_failed ] && want=$(arrivals /blog deploy_failed 0 | wc -l)
	[ "$n" -eq "$want" ] && pass "$a reached the operator $n time(s), as many as it was posted, beside a failing channel's retries" \
		|| fail "$a reached the operator $n times, want $want"
done

}

case_10() {
echo "=== notify 10: no channel URL in any journal ==="
n=$(journalctl -u motel -u prometheus-alertmanager -o cat --no-pager | grep -c 'e2e-sink' || true)
[ "$n" -eq 0 ] && pass "no channel URL in motel's or Alertmanager's journal" || fail "$n journal lines hold a channel URL"

}

case_10a() {
echo "=== notify 10a: no heartbeat line, warned ==="
sed '/^[[:space:]]*heartbeat /d' /etc/motel/Caddyfile >/tmp/Caddyfile.nohb
grep -q 'heartbeat ' /tmp/Caddyfile.nohb && fail "fixture: could not cut the heartbeat line"
motel validate --adapter caddyfile --config /tmp/Caddyfile.nohb >/tmp/validate-nohb.out 2>&1 \
	&& grep -q 'monitor: no heartbeat in this load' /tmp/validate-nohb.out \
	&& pass "motel validate takes a block with no heartbeat line, and warns" \
	|| fail "validate without a heartbeat: $(tail -3 /tmp/validate-nohb.out)"
motel validate --adapter caddyfile --config /etc/motel/Caddyfile >/tmp/validate-hb.out 2>&1
grep -q 'no heartbeat' /tmp/validate-hb.out && fail "validate warns of no heartbeat with the line in place" \
	|| pass "and with the line, no such warning"

}

case_11() {
echo "=== notify 11: a secret missing at boot drops its channel; validate and reload refuse ==="
# A channel line whose secret _box.env does not hold: the box's
# consistency check refuses such a push, so this is a hand edit, or a
# file lost. A boot must not take every site down for it. The Caddyfile
# to put back is the prelude's copy.
sed -i 's#^\(\t*\)operator ops oncall#\1channel spare json {secret.NOTIFY_SPARE_URL}\n\1operator ops oncall spare#' /etc/motel/Caddyfile
grep -q 'NOTIFY_SPARE_URL' /etc/motel/Caddyfile || fail "fixture: the spare channel line is not in the Caddyfile"
if motel validate --adapter caddyfile --config /etc/motel/Caddyfile >/tmp/validate.out 2>&1; then
	fail "motel validate took a channel whose secret is missing"
else
	grep -q 'channel spare: secret NOTIFY_SPARE_URL is not in the box' /tmp/validate.out \
		&& pass "motel validate refuses it, naming the channel and the key" || fail "validate: $(tail -2 /tmp/validate.out)"
fi
if systemctl reload motel 2>/dev/null; then
	fail "a reload took a channel whose secret is missing"
else
	systemctl is-active -q motel && pass "a reload refuses it, and the running config serves on" || fail "motel is not active after the refused reload"
fi
t_boot=$(date +%s)
systemctl restart motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done
systemctl is-active -q motel && pass "motel starts on it: the boot is not refused" || fail "motel did not start: $(journalctl -u motel -o cat --no-pager -n 5)"
journalctl -u motel --since "@$t_boot" -o cat --no-pager | grep 'monitor: a channel is dropped from this load' | grep -q 'channel spare: secret NOTIFY_SPARE_URL' \
	&& pass "journal: the dropped channel, by line and key" || fail "no journal line names the dropped channel"
out=$(motel monitor test ops 2>&1) && echo "$out" | grep -q 'delivered to ops' \
	&& pass "the other channels deliver: $out" || fail "monitor test ops after the boot: $out"
out=$(motel monitor test spare 2>&1) && fail "monitor test of the dropped channel succeeded: $out" || {
	echo "$out" | grep -q 'channel spare was dropped from this load' \
		&& pass "monitor test names the channel as dropped, and the mend" || fail "monitor test spare: $out"
}
# A file motel cannot read: root's validate refuses it, as the reload would.
cp -p /etc/motel/secrets/_box.env /tmp/_box.env.keep
chown root:root /etc/motel/secrets/_box.env && chmod 0600 /etc/motel/secrets/_box.env
if motel validate --adapter caddyfile --config /tmp/Caddyfile.before >/tmp/validate-0600.out 2>&1; then
	fail "motel validate, as root, took a secrets file motel cannot read"
else
	grep -q 'the motel user cannot read the box.s secrets file /etc/motel/secrets/_box.env (root:root 0600)' /tmp/validate-0600.out \
		&& pass "motel validate refuses a root:root 0600 secrets file, naming the mend" || fail "validate of 0600: $(tail -2 /tmp/validate-0600.out)"
fi
cp -p /tmp/_box.env.keep /etc/motel/secrets/_box.env
# The mend: the key added to the secrets file, the Caddyfile unchanged.
# A plain motel reload (no --force) loads it, as systemctl reload does.
printf '%s\n' 'NOTIFY_SPARE_URL="http://e2e-sink:8080/spare"' >>/etc/motel/secrets/_box.env
motel reload --config /etc/motel/Caddyfile >/tmp/reload-mend.out 2>&1 || fail "motel reload after the mend: $(tail -3 /tmp/reload-mend.out)"
out=$(motel monitor test spare 2>&1) && echo "$out" | grep -q 'delivered to spare' \
	&& pass "after the mend, a plain motel reload of the unchanged Caddyfile brings the channel back: $out" \
	|| fail "monitor test spare after motel reload: $out"
cp -p /tmp/_box.env.keep /etc/motel/secrets/_box.env
cp /tmp/Caddyfile.before /etc/motel/Caddyfile
systemctl restart motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done

}

case_12() {
echo "=== notify 12: motel restarted while a state alert fires: it is told again, and still resolves ==="
# A relaunch refused: blog's command gone from its release, then its
# process killed. Nothing re-detects that after a restart (recovery's
# own refusal reports nothing), so without liveswap's alerts.json and
# its standing report the alert would end at resolve_timeout, and a
# recovery be told for an app that is down.
rr_alert() { am 'http://am/api/v2/alerts?filter=alertname%3D%22relaunch_refused%22&filter=app%3D%22blog%22'; }
rr_ends() { rr_alert | sed -n 's/.*"endsAt":"\([^"]*\)".*/\1/p' | head -1; }
code=$(deploy demo-v1.tar.gz rr1)
[ "$code" = 200 ] && pass "deploy rr1" || fail "deploy rr1 returned $code: $(cat /tmp/deploy-body)"
REL=/var/lib/liveswap/blog/releases/rr1
cp "$REL/server" /tmp/server.rr1
pid=$(json_num "$(status)" pid)
t_rr=$(now_ms)
rm "$REL/server"
[ -n "$pid" ] && kill -9 "$pid"
i=0
until journalctl -u motel -o cat --no-pager | grep 'monitor: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"state":"firing"'; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done
journalctl -u motel -o cat --no-pager | grep 'monitor: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"state":"firing"' \
	&& pass "the refused relaunch fires relaunch_refused" || fail "no relaunch_refused after the kill (pid '$pid')"
grep -q '"refused":true' /var/lib/liveswap/blog/alerts.json 2>/dev/null \
	&& pass "alerts.json keeps it for a restart" || fail "alerts.json: $(cat /var/lib/liveswap/blog/alerts.json 2>&1)"
t=$(first_arrival /ops relaunch_refused "$t_rr" 60 firing blog)
within "relaunch_refused to the operator (group_wait)" "$t" "$t_rr" 0 45
# A producer's state across the same restart: no detector of motel's
# tells it again, so the engine keeps it in external.json.
send --name restart_probe --state firing --summary "e2e: firing across a restart" || fail "send firing: $(cat /tmp/send.out)"
grep -q '"name":"restart_probe"' "$STATE/external.json" 2>/dev/null \
	&& pass "external.json keeps the producer's state for a restart" || fail "external.json: $(cat "$STATE/external.json" 2>&1)"
x_ends() { am 'http://am/api/v2/alerts?filter=alertname%3D%22restart_probe%22' | sed -n 's/.*"endsAt":"\([^"]*\)".*/\1/p' | head -1; }
i=0
until [ -n "$(x_ends)" ]; do
	i=$((i + 1)); [ "$i" -ge 20 ] && break; sleep 0.5
done
x0=$(x_ends)
e0=$(rr_ends)
t_restart=$(date +%s)
systemctl restart motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done
i=0
until journalctl -u motel --since "@$t_restart" -o cat --no-pager | grep 'monitor: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"standing":true'; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5
done
journalctl -u motel --since "@$t_restart" -o cat --no-pager | grep 'monitor: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"standing":true' \
	&& pass "after the restart, motel tells it again, standing" || fail "no standing relaunch_refused after the restart"
journalctl -u motel --since "@$t_restart" -o cat --no-pager | grep 'monitor: event' | grep 'external:e2e/box/restart_probe' | grep -q '"standing":true' \
	&& pass "and the producer's state, standing" || fail "no standing restart_probe after the restart"
i=0
until [ -n "$x0" ] && [ -n "$(x_ends)" ] && expr "$(x_ends)" \> "$x0" >/dev/null; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5
done
x1=$(x_ends)
[ -n "$x0" ] && [ -n "$x1" ] && expr "$x1" \> "$x0" >/dev/null \
	&& pass "and Alertmanager's alert for it is kept alive (endsAt $x0 → $x1)" || fail "restart_probe's endsAt did not move: '$x0' → '$x1'"
n=$(am 'http://am/api/v2/alerts?filter=alertname%3D%22restart_probe%22' | grep -o '"fingerprint"' | wc -l)
[ "$n" -eq 1 ] && pass "one alert for it: the standing report has the original's labels" || fail "$n restart_probe alerts in Alertmanager"
send --name restart_probe --state resolved --summary "e2e: firing across a restart" || fail "send resolved: $(cat /tmp/send.out)"
grep -q '^sent ' /tmp/send.out && pass "the producer's resolve after the restart finds its state" || fail "the resolve after the restart: $(cat /tmp/send.out)"
i=0
until [ -n "$e0" ] && [ -n "$(rr_ends)" ] && expr "$(rr_ends)" \> "$e0" >/dev/null; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5
done
e1=$(rr_ends)
[ -n "$e0" ] && [ -n "$e1" ] && expr "$e1" \> "$e0" >/dev/null \
	&& pass "Alertmanager's alert is kept alive across the restart (endsAt $e0 → $e1)" || fail "the alert's endsAt did not move: '$e0' → '$e1'"
rr_alert | grep -q '"state":"active"' && pass "the alert is still active" || fail "the alert: $(rr_alert | head -c 300)"
n=$(arrivals /ops relaunch_refused "$t_rr" resolved blog | wc -l)
[ "$n" -eq 0 ] && pass "no recovery reached the operator across the restart" || fail "$n recoveries while blog is down"
# The cause mended and a reload: recovery launches it, and the standing
# alert resolves after motel's hold.
cp /tmp/server.rr1 "$REL/server"
chown --reference="$REL" "$REL/server" 2>/dev/null || true
t_fix=$(now_ms)
systemctl reload motel || fail "the reload after the mend"
t=$(first_arrival /ops relaunch_refused "$t_fix" 150 resolved blog)
within "the recovery once an instance runs again (hold, then a tick)" "$t" "$t_fix" 25 150
[ ! -e /var/lib/liveswap/blog/alerts.json ] && pass "alerts.json is gone once nothing stands" || fail "alerts.json: $(cat /var/lib/liveswap/blog/alerts.json 2>&1)"

}

case_13() {
echo "=== notify 13: the old spellings are refused, naming the new ==="
sed 's/^\tmonitor {$/\tnotify {/' /etc/motel/Caddyfile >/tmp/Caddyfile.old
grep -q '^	notify {$' /tmp/Caddyfile.old || fail "fixture: could not rename the monitor block"
if motel validate --adapter caddyfile --config /tmp/Caddyfile.old >/tmp/validate-old.out 2>&1; then
	fail "motel validate took a notify block"
else
	grep -qF 'the notify block is now `monitor { … }`' /tmp/validate-old.out \
		&& pass "motel validate refuses the notify block, naming monitor" || fail "validate of a notify block: $(tail -2 /tmp/validate-old.out)"
fi
if motel notify setup >/tmp/old.out 2>&1; then
	fail "motel notify setup ran"
else
	grep -qF 'motel notify is now motel monitor: motel monitor setup' /tmp/old.out \
		&& pass "motel notify setup exits non-zero, naming motel monitor setup" || fail "motel notify setup: $(cat /tmp/old.out)"
fi

}

case_14() {
echo "=== notify 14: the block taken out by a reload: Alertmanager sends nothing ==="
# Alertmanager keeps the last config it loaded, and would end what it
# holds at resolve_timeout through it: a recovery that did not happen,
# on every channel. The reload gives it the config that sends nothing.
send --name removal_probe --state firing --summary "e2e: firing when the block goes" || fail "send firing: $(cat /tmp/send.out)"
t_rm=$(now_ms)
t=$(first_arrival /ops removal_probe "$t_rm" 60 firing)
[ -n "$t" ] && pass "a state fires before the block goes" || fail "removal_probe never fired"
grep -q '"name":"removal_probe"' "$STATE/external.json" 2>/dev/null || fail "fixture: external.json does not hold removal_probe"
awk 'BEGIN{skip=0} /^\tmonitor \{$/{skip=1} skip&&/^\t\}$/{skip=0; next} !skip' /etc/motel/Caddyfile >/tmp/Caddyfile.noblock
grep -q 'monitor {' /tmp/Caddyfile.noblock && fail "fixture: could not cut the monitor block"
cp /tmp/Caddyfile.noblock /etc/motel/Caddyfile
t_reload=$(date +%s)
systemctl reload motel || fail "the reload without the block"
grep -q '^# Written by `motel monitor setup`' "$STATE/alertmanager.yml" && grep -q '^  receiver: blackhole$' "$STATE/alertmanager.yml" \
	&& ! grep -q 'Rendered by motel' "$STATE/alertmanager.yml" \
	&& pass "the state directory holds the config that sends nothing" || fail "after the reload: $(head -5 "$STATE/alertmanager.yml")"
[ -z "$(find "$STATE" -name 'url-*')" ] && pass "and no URL file" || fail "URL files left: $(find "$STATE" -name 'url-*')"
[ ! -e "$STATE/external.json" ] && pass "and no producer's state kept for a restart" || fail "external.json left: $(cat "$STATE/external.json")"
am http://am/api/v2/status | grep -q 'e2e-sink\|receiver: ops\|ch-ops' && fail "Alertmanager still runs a config with channels" \
	|| pass "Alertmanager runs it: no receiver of motel's channels"
journalctl -u motel --since "@$t_reload" -o cat --no-pager | grep -q 'Alertmanager was given the config that sends nothing' \
	&& pass "journal: Alertmanager was given the config that sends nothing" || fail "no journal line for the removal"
# The end Alertmanager would reach at resolve_timeout, brought forward:
# root posts the alert's own labels with endsAt now. Under the last
# render that is a [RESOLVED] to ops at the group's next tick; under the
# config that sends nothing, nothing.
labels=$(am 'http://am/api/v2/alerts?filter=alertname%3D%22removal_probe%22' | sed -n 's/.*"labels":\({[^}]*}\).*/\1/p' | head -1)
t_end=$(now_ms)
if [ -n "$labels" ]; then
	am -o /dev/null -X POST -H 'Content-Type: application/json' http://am/api/v2/alerts \
		-d "[{\"labels\":$labels,\"annotations\":{\"summary\":\"e2e: firing when the block goes\"},\"endsAt\":\"$(date -u +%Y-%m-%dT%H:%M:%S.000Z)\"}]"
	sleep 75
	none "its end reaches no channel: no recovery that did not happen" /ops removal_probe "$t_end"
else
	fail "Alertmanager holds no removal_probe to end"
fi
cp /tmp/Caddyfile.before /etc/motel/Caddyfile
systemctl reload motel || fail "the reload that puts the block back"

}

case_15() {
echo "=== notify 15: a failure no channel can hear holds the heartbeat ==="
# oncall alone is the operator's, and it answers 400: there is no other
# channel to tell of its failure. Once the failure has stood 5 minutes
# with no delivery through oncall, motel stops posting Watchdog, so the
# heartbeat's check off the box reports the box down; a delivery through
# oncall, once it is mended, resumes it.
wd_ends() { am 'http://am/api/v2/alerts?filter=alertname%3D%22Watchdog%22' | sed -n 's/.*"endsAt":"\([^"]*\)".*/\1/p' | head -1; }
sed 's/^\(\t*\)operator ops oncall$/\1operator oncall/' /tmp/Caddyfile.before >/etc/motel/Caddyfile
grep -q '^[[:space:]]*operator oncall$' /etc/motel/Caddyfile || fail "fixture: could not make oncall the only operator channel"
systemctl reload motel || fail "the reload with oncall alone"
s_15=$(date +%s)
send --name probe_15_failed --severity warning --summary "e2e: to oncall alone" || fail "send: $(cat /tmp/send.out)"
i=0
until journalctl -u motel --since "@$s_15" -o cat --no-pager | grep -q 'monitor: heartbeat held'; do
	i=$((i + 1)); [ "$i" -ge 540 ] && break; sleep 1
done
held=$(journalctl -u motel --since "@$s_15" -o cat --no-pager | grep 'monitor: heartbeat held' | head -1)
if [ -n "$held" ]; then
	pass "the heartbeat is held $(( $(date +%s) - s_15 )) s after the failure (5 minutes, then the minute's read)"
	echo "$held" | grep -q '"suspects":\["oncall"\]' && pass "journal: monitor: heartbeat held names oncall" || fail "held: $held"
else
	fail "no monitor: heartbeat held within 9 minutes"
fi
journalctl -u motel --since "@$s_15" -o cat --no-pager | grep 'monitor: channel failed' | grep '"suspects":\["oncall"\]' | grep -q '"told":\[\]' \
	&& pass "journal: monitor: channel failed told no channel" || fail "channel failed: $(journalctl -u motel --since "@$s_15" -o cat --no-pager | grep 'monitor: channel failed' | tail -1)"
e0=$(wd_ends)
sleep 75
e1=$(wd_ends)
[ -z "$e1" ] || [ "$e1" = "$e0" ] \
	&& pass "no Watchdog posted while held (endsAt '$e0', then '$e1')" || fail "Watchdog posted while held: endsAt $e0 → $e1"
cp -p /etc/motel/secrets/_box.env /tmp/_box.env.15
sed -i 's#^NOTIFY_ONCALL_URL=.*#NOTIFY_ONCALL_URL="http://e2e-sink:8080/oncall"#' /etc/motel/secrets/_box.env
s_mend=$(date +%s)
motel reload --config /etc/motel/Caddyfile >/tmp/reload-15.out 2>&1 || fail "the reload that mends oncall: $(tail -3 /tmp/reload-15.out)"
out=$(motel monitor test oncall 2>&1) && echo "$out" | grep -q 'delivered to oncall' \
	&& pass "mended, monitor test oncall: $out" || fail "monitor test oncall after the mend: $out"
i=0
until journalctl -u motel --since "@$s_mend" -o cat --no-pager | grep -q 'monitor: heartbeat resumed'; do
	i=$((i + 1)); [ "$i" -ge 90 ] && break; sleep 1
done
journalctl -u motel --since "@$s_mend" -o cat --no-pager | grep -q 'monitor: heartbeat resumed' \
	&& pass "journal: monitor: heartbeat resumed, $(( $(date +%s) - s_mend )) s after the mend" || fail "no monitor: heartbeat resumed within 90 s"
i=0
until [ -n "$(wd_ends)" ] && { [ -z "$e1" ] || expr "$(wd_ends)" \> "$e1" >/dev/null; }; do
	i=$((i + 1)); [ "$i" -ge 75 ] && break; sleep 1
done
e2=$(wd_ends)
[ -n "$e2" ] && { [ -z "$e1" ] || expr "$e2" \> "$e1" >/dev/null; } \
	&& pass "Watchdog is posted again (endsAt $e2)" || fail "no Watchdog after the hold ended: '$e1' → '$e2'"
cp -p /tmp/_box.env.15 /etc/motel/secrets/_box.env
cp /tmp/Caddyfile.before /etc/motel/Caddyfile
systemctl reload motel || fail "the reload that puts the fixture back"
}

for p in $parts; do "case_$p"; done

echo
if [ "$FAILURES" -ne 0 ]; then
	echo "notify suite ($SPLIT): $FAILURES failure(s)"
	echo "--- sink log"; sinklog | tail -60
	echo "--- alertmanager"; journalctl -u prometheus-alertmanager -o cat --no-pager | tail -30
	exit 1
fi
echo "notify suite ($SPLIT): all passed"
