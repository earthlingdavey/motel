#!/bin/sh
# Orchestrates both modules' e2e suites against the one moteld
# binary. Each suite prints its own PASS/FAIL lines and summary; this
# script's exit code (the gate for `make e2e`) fails if either fails.
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
