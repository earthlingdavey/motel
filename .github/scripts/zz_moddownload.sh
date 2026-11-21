#!/bin/sh
# measure: NOT TO MERGE. Option 5's cost when there is nothing to fetch:
# `go mod download` over every module, in the dev container, on the
# cache the job restored, twice (zz_measure.sh moddownload). Two ways a
# download step could be written:
#   way=each       in each module's directory with the workspace off:
#                  what that module's go.mod and go.sum name
#   way=workspace  once at the root, the workspace on
# fetches= counts the requests go made to the proxy (-x's "# get" lines):
# go mod download wants every module of the build list, more than the
# builds that filled the cache needed. Timed in here, so the image's
# pull is in no figure.
set -u
MODS=". backups box client liveswap notify penaltybox"
emit() { echo "MEASURE $* arch=$(uname -m) job=${GITHUB_JOB:-local}"; }
since() { awk -v a="$1" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }'; }
log=$(mktemp)
for pass in 1 2; do
	t0=$(date +%s.%N)
	rc=0
	: >"$log"
	for m in $MODS; do
		(cd "$m" && GOWORK=off go mod download -x) >>"$log" 2>&1 || rc=1
	done
	emit "what=moddownload way=each pass=$pass secs=$(since "$t0") fetches=$(grep -c '^# get ' "$log") rc=$rc"
	[ $rc = 0 ] || tail -20 "$log"
done
for pass in 1 2; do
	t0=$(date +%s.%N)
	go mod download -x >"$log" 2>&1
	rc=$?
	emit "what=moddownload way=workspace pass=$pass secs=$(since "$t0") fetches=$(grep -c '^# get ' "$log") rc=$rc"
	[ $rc = 0 ] || tail -20 "$log"
done
