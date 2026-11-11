#!/bin/sh
# The notify suite (make e2e-notify), run inside e2e-notify-box as root.
#
# The box is e2e-motel's image with Debian's prometheus-alertmanager
# installed and masked; motel runs the notify block in
# e2e/notify/Caddyfile, whose channels are paths on e2e-sink, a stub
# that logs every delivery with its time (e2e/notify/sink.py). The suite
# runs `motel notify setup`, then reproduces the timeline measured on
# hotserve-1 against the corrected route tree
# (research: alertmanager_edge_tests.md, its addendum), through motel:
#
#   Watchdog                      → /deadman, at once
#   deploy_failed (a real deploy) → /ops and /blog, the same second
#   a box one-shot (notify send)  → /ops only, at once
#   four distinct one-shots       → four deliveries, none lost
#   watchdog_throttled blog       → nothing: inhibited by deploy_in_progress
#   watchdog_throttled shop       → /ops, after the 30 s group_wait
#   a box state (notify send)     → /ops at +30 s; its recovery within a minute of motel's hold
#   oncall (answers 400)          → ChannelFailed to /ops, naming oncall
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
sinklog() { curl -s --max-time 5 "$SINK/log"; }
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
send() { motel notify send --source e2e "$@" >/tmp/send.out 2>&1; }

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

echo "=== notify 1: motel notify setup ==="
t_setup=$(now_ms)
if motel notify setup >/tmp/setup.out 2>&1; then
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
	&& pass "motel rendered the notify block into $STATE/alertmanager.yml" || fail "the config is not motel's: $(head -3 "$STATE/alertmanager.yml")"
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

echo "=== notify 2: the heartbeat ==="
t=$(first_arrival /deadman Watchdog "$t_setup" 90 firing)
within "Watchdog to the heartbeat" "$t" "$t_setup" 0 90

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
none "deploy_succeeded is off by default" /ops deploy_succeeded "$t_v1"
none "deploy_in_progress reaches nobody (operator)" /ops deploy_in_progress 0
none "deploy_in_progress reaches nobody (deployer)" /blog deploy_in_progress 0
journalctl -u motel -o cat --no-pager | grep 'notify: event' | grep -q 'liveswap/app:blog/deploy_failed' \
	&& pass "journal: notify: event liveswap/app:blog/deploy_failed" || fail "no notify: event line for the failed deploy"

echo "=== notify 4: one-shots from outside ==="
t_box=$(now_ms)
send --name nightly_job_failed --severity warning --summary "e2e: the nightly job failed" \
	&& pass "notify send: $(cat /tmp/send.out)" || fail "notify send: $(cat /tmp/send.out)"
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
if runuser -u nobody -- motel notify send --source e2e --name x_failed --summary e2e >/tmp/nobody.out 2>&1; then
	fail "nobody posted through the admin socket"
else
	pass "nobody cannot reach motel's admin socket to send"
fi

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
journalctl -u motel -o cat --no-pager | grep 'notify: event' | grep 'external:e2e/box/disk_check' | grep -q '"state":"resolved"' \
	&& pass "journal: the resolve's notify: event line" || fail "no resolved line for disk_check"

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
journalctl -u motel -o cat --no-pager | grep 'notify: channel failed' | grep -q 'oncall' \
	&& pass "journal: notify: channel failed names oncall" || fail "no channel failed line naming oncall"

echo "=== notify 7: motel notify test ==="
out=$(motel notify test ops 2>&1) && echo "$out" | grep -q 'delivered to ops' \
	&& pass "notify test ops: $out" || fail "notify test ops: $out"
out=$(motel notify test oncall 2>&1) && fail "notify test oncall succeeded: $out" || pass "notify test oncall fails: $(echo "$out" | tail -1)"

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

echo "=== notify 10: no channel URL in any journal ==="
n=$(journalctl -u motel -u prometheus-alertmanager -o cat --no-pager | grep -c 'e2e-sink' || true)
[ "$n" -eq 0 ] && pass "no channel URL in motel's or Alertmanager's journal" || fail "$n journal lines hold a channel URL"

echo "=== notify 11: a secret missing at boot drops its channel; validate and reload refuse ==="
# A channel line whose secret _box.env does not hold: the box's
# consistency check refuses such a push, so this is a hand edit, or a
# file lost. A boot must not take every site down for it.
cp /etc/motel/Caddyfile /tmp/Caddyfile.before
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
journalctl -u motel --since "@$t_boot" -o cat --no-pager | grep 'notify: a channel is dropped from this load' | grep -q 'channel spare: secret NOTIFY_SPARE_URL' \
	&& pass "journal: the dropped channel, by line and key" || fail "no journal line names the dropped channel"
out=$(motel notify test ops 2>&1) && echo "$out" | grep -q 'delivered to ops' \
	&& pass "the other channels deliver: $out" || fail "notify test ops after the boot: $out"
cp /tmp/Caddyfile.before /etc/motel/Caddyfile
systemctl restart motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done

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
until journalctl -u motel -o cat --no-pager | grep 'notify: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"state":"firing"'; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done
journalctl -u motel -o cat --no-pager | grep 'notify: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"state":"firing"' \
	&& pass "the refused relaunch fires relaunch_refused" || fail "no relaunch_refused after the kill (pid '$pid')"
grep -q '"refused":true' /var/lib/liveswap/blog/alerts.json 2>/dev/null \
	&& pass "alerts.json keeps it for a restart" || fail "alerts.json: $(cat /var/lib/liveswap/blog/alerts.json 2>&1)"
t=$(first_arrival /ops relaunch_refused "$t_rr" 60 firing blog)
within "relaunch_refused to the operator (group_wait)" "$t" "$t_rr" 0 45
e0=$(rr_ends)
t_restart=$(date +%s)
systemctl restart motel
i=0
until systemctl is-active -q motel && [ -S /run/motel/admin.sock ]; do
	i=$((i + 1)); [ "$i" -ge 120 ] && break; sleep 0.5
done
i=0
until journalctl -u motel --since "@$t_restart" -o cat --no-pager | grep 'notify: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"standing":true'; do
	i=$((i + 1)); [ "$i" -ge 40 ] && break; sleep 0.5
done
journalctl -u motel --since "@$t_restart" -o cat --no-pager | grep 'notify: event' | grep 'liveswap/app:blog/relaunch_refused' | grep -q '"standing":true' \
	&& pass "after the restart, motel tells it again, standing" || fail "no standing relaunch_refused after the restart"
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

echo
if [ "$FAILURES" -ne 0 ]; then
	echo "notify suite: $FAILURES failure(s)"
	echo "--- sink log"; sinklog | tail -60
	echo "--- alertmanager"; journalctl -u prometheus-alertmanager -o cat --no-pager | tail -30
	exit 1
fi
echo "notify suite: all passed"
