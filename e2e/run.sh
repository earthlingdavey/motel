#!/bin/sh
# Orchestrates the e2e runner's two suites, liveswap's and
# penaltybox's, against the one moteld binary. Each suite prints its own
# PASS/FAIL lines and summary; this script's exit code fails if either
# fails. `make e2e` runs it, then the systemd, recovery, box and people
# suites (the Makefile's e2e target).
set -u

TOTAL=0

echo "════ liveswap suite ════"
sh /suite-liveswap.sh
TOTAL=$((TOTAL + $?))

echo ""
echo "════ penaltybox suite ════"
sh /suite-penaltybox.sh
TOTAL=$((TOTAL + $?))

echo ""
if [ "$TOTAL" -eq 0 ]; then
	echo "ALL SUITES PASSED"
	exit 0
fi
echo "ONE OR MORE SUITES FAILED"
exit 1
