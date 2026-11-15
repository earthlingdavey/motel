#!/bin/sh
# Runs inside a throwaway debian:13 container on the e2e network (see
# observe.sh): installs Debian's ab, then for each target, each
# concurrency and each run, a warm-up and a measured pass. One line per
# measured pass:
#   <name> c=<conc> run=<n> rps=<req/s> mean_ms=<ms> p50=<ms> p95=<ms> p99=<ms> failed=<n>
# Percentiles are ab's -e table (fractional milliseconds).
#
# Args: <name>=<url> ...   Env: N (requests), CONCS, RUNS, WARM.
set -eu
N="${N:-20000}"
CONCS="${CONCS:-1 16}"
RUNS="${RUNS:-3}"
WARM="${WARM:-2000}"

apt-get -qq update >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get -qq install -y --no-install-recommends apache2-utils >/dev/null

pct() { # <csv> <percent> -> ms
	awk -F, -v p="$2" '$1 == p {print $2; exit}' "$1"
}

for target in "$@"; do
	name=${target%%=*}
	url=${target#*=}
	for c in $CONCS; do
		ab -q -k -n "$WARM" -c "$c" "$url" >/dev/null 2>&1 || true
		r=1
		while [ "$r" -le "$RUNS" ]; do
			out=$(ab -q -k -n "$N" -c "$c" -e /tmp/pct.csv "$url" 2>&1)
			rps=$(printf '%s\n' "$out" | awk '/^Requests per second:/ {print $4}')
			mean=$(printf '%s\n' "$out" | awk '/^Time per request:/ && /\(mean\)$/ {print $4; exit}')
			failed=$(printf '%s\n' "$out" | awk '/^Failed requests:/ {print $3}')
			non2xx=$(printf '%s\n' "$out" | awk '/^Non-2xx responses:/ {print $3}')
			echo "$name c=$c run=$r rps=$rps mean_ms=$mean p50=$(pct /tmp/pct.csv 50) p95=$(pct /tmp/pct.csv 95) p99=$(pct /tmp/pct.csv 99) failed=$failed non2xx=${non2xx:-0}"
			r=$((r + 1))
		done
	done
done
