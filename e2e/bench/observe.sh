#!/bin/sh
# The request path's cost, before and with liveswap_observe
# (notify/DESIGN-metrics.md, "The response observer"; the triage rule's
# measurement before a fix on every app's path). Run from the repo root:
#
#   sh e2e/bench/observe.sh [<name>=<url> ...]
#
# Brings up the e2e stack, deploys demo-v1 to `demo`, then runs Debian's
# ab from a throwaway debian:13 container on the stack's network
# (e2e/bench/ab.sh): keep-alive GETs at each concurrency, three measured
# runs each after a warm-up. Prints every run and, per target and
# concurrency, the median run by req/s. The load average is printed
# before and after: other stacks on the same machine skew the numbers.
#
# Targets default to the bare app site (e2e/Caddyfile :8080). Env: N,
# CONCS, RUNS, WARM (ab.sh), KEEP=1 to leave the stack up.
set -eu
COMPOSE="${COMPOSE:-docker compose}"
project=$(basename "$(pwd)")
[ "$#" -gt 0 ] || set -- "bare=http://e2e-motel:8080/"

$COMPOSE up --build -d e2e-motel e2e-upstream e2e-artifacts >/dev/null
$COMPOSE run --rm -T --entrypoint /bin/sh e2e-runner -c '
	HOOK=http://e2e-motel:8081/demo ART=http://e2e-artifacts:8080/artifacts
	. /lib.sh
	wait_for_token
	i=0
	until [ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$HOOK")" = 200 ]; do
		i=$((i + 1)); [ "$i" -ge 60 ] && { echo "FATAL: webhook not ready"; exit 1; }; sleep 1
	done
	c=$(deploy demo-v1.tar.gz bench-1)
	[ "$c" = 200 ] || { echo "FATAL: deploy $c: $(cat /tmp/deploy-body)"; exit 1; }
	echo "deployed demo bench-1"'

echo "load before: $(cut -d" " -f1-3 /proc/loadavg)"
echo "commit: $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (dirty)')"
out=$(docker run --rm --network "${project}_default" \
	-e N -e CONCS -e RUNS -e WARM \
	-v "$(pwd)/e2e/bench/ab.sh:/ab.sh:ro" debian:13 /bin/sh /ab.sh "$@")
echo "load after: $(cut -d" " -f1-3 /proc/loadavg)"
printf '%s\n' "$out"
echo "--- median run by req/s, per target and concurrency ---"
printf '%s\n' "$out" | awk '{print $1, $2}' | sort -u | while read -r name c; do
	printf '%s\n' "$out" | awk -v n="$name" -v c="$c" '$1 == n && $2 == c' |
		sort -t= -k4 -n | awk '{a[NR] = $0} END {print a[int((NR + 1) / 2)]}'
done

[ "${KEEP:-}" = 1 ] || $COMPOSE down --remove-orphans >/dev/null
