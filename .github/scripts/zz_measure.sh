#!/bin/sh
# NOT TO MERGE. A one-off measurement, run in place of `make test`:
# go test as make test runs it, with each test's time, for main as it
# is, main without #637, main without #637 and #634, and main again,
# one after the other on the same runner.
# usage: zz_measure.sh '<the compose command>'
COMPOSE=$1
here=.github/scripts

measure() {
	tree=$1
	# shellcheck disable=SC2086
	$COMPOSE run --rm -T dev go test -race -cover -timeout 45m -json \
		./... ./backups/... ./box/... ./client/... ./liveswap/... ./notify/... ./penaltybox/... \
		>"/tmp/test-$tree.json" 2>"/tmp/test-$tree.err"
	echo "go test exit for $tree: $?"
	tail -3 "/tmp/test-$tree.err"
	python3 "$here/zz_times.py" "/tmp/test-$tree.json" "$tree" "$(uname -m)" 1
}

nproc
lscpu | grep -E 'Model name|^CPU\(s\)|MHz' || true

measure main-first
git apply "$here/zz-revert-637.patch" && measure without-637
git apply "$here/zz-revert-634.patch" && measure without-637-634
git apply -R "$here/zz-revert-634.patch" && git apply -R "$here/zz-revert-637.patch" && measure main-last
git status --short
exit 0
