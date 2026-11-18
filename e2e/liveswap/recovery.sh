#!/bin/sh
# Recovery suite, run by `make e2e` LAST — after the main suites and
# after the in-container systemd suite has killed moteld three ways
# (SIGKILL, exit 2, OOM) and let systemd bring it back, and refused it
# a start once. This is the runner's view of that: the app that was
# serving (sd-final, demo-v1 content, the last deploy the systemd
# suite made) is still there without any webhook call — reattached,
# not relaunched — and the deploy machinery isn't wedged by the
# unclean deaths.
set -u

PROXY="http://e2e-motel:8080"
HOOK="http://e2e-motel:8081/demo"
ART="http://e2e-artifacts:8080/artifacts"
. /lib.sh
wait_for_token

echo "=== recovery 1: the app is still served after moteld's crashes and restarts ==="
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$PROXY/")" = "200" ]; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		fail "proxy not serving within 60s of restart"
		echo "$FAILURES RECOVERY ASSERTION(S) FAILED"
		exit 1
	fi
	sleep 1
done
pass "proxy serving again ${i}s after restart"

b=$(body)
case "$b" in
"hello v1"*) pass "reattached app serves the last deployed release: '$b'" ;;
*) fail "expected sd-final's content ('hello v1 ...'), got '$b'" ;;
esac

echo "=== recovery 2: status endpoint reports the recovered deploy ==="
s=$(status)
case "$s" in
*'"current_version":"sd-final"'*'"running":true'*|*'"running":true'*'"current_version":"sd-final"'*)
	pass "status reports sd-final running after the unclean death" ;;
*) fail "unexpected status after crash: $s" ;;
esac

echo "=== recovery 3: deploys still work after the unclean death ==="
c=$(deploy demo-v2.tar.gz v6)
[ "$c" = "200" ] && pass "post-recovery deploy accepted" \
	|| fail "post-recovery deploy: expected 200, got $c ($(cat /tmp/deploy-body))"
b=$(body)
case "$b" in
"hello v2"*) pass "post-recovery deploy serves: '$b'" ;;
*) fail "expected 'hello v2 ...', got '$b'" ;;
esac

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL RECOVERY SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES RECOVERY ASSERTION(S) FAILED"
exit 1
