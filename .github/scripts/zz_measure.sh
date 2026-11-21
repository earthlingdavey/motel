#!/bin/sh
# measure: NOT TO MERGE. What this branch's ci.yml runs to measure the CI
# deep dive's options. Every line it prints for a reader starts
# "MEASURE " and is key=value pairs, always with what=, arch= and job=,
# so one script greps them out of the job logs (the research notes'
# ci-deep-dive/measure/collect_measure.py). $COMPOSE is the workflow's.
#
#   timed k=v ... -- cmd ...   run cmd: what=timed k=v secs= rc=, and cmd's exit status
#   du k=v ...                 what=du k=v dir=gomod|gobuild|golangci kb= zst_bytes=: .cache's parts, unpacked
#                              kB and bytes as a tar|zstd archive (what actions/cache would save); dir=all their sum
#   drop <family>              du family=<family>, then .cache is removed
#   merge <family>             du family=<family>, then .cache is merged into $RUNNER_TEMP/union and removed
#   union                      the union is put at .cache; du family=union
#   moddownload                go mod download over every module in the dev container (zz_moddownload.sh),
#                              then the sums it rewrote are put back
#   pair                       make test and make test-integration at once: what=pair part=unit|integration|both
#                              secs= rc=; fails if either fails. Integration's output is held and printed after,
#                              between MEASURE-BEGIN integration-beside and MEASURE-END integration-beside.
set -u
COMPOSE=${COMPOSE:-docker compose}

now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
emit() { echo "MEASURE $* arch=$(uname -m) job=${GITHUB_JOB:-local}"; }
# The restored caches hold read-only directories (Go's module cache), and
# the containers write as root: read, merge and remove as root.
SUDO=""
if sudo -n true 2>/dev/null; then SUDO="sudo -n"; fi

du_cache() { # k=v ...
	t0=$(now)
	sum=0
	for d in gomod gobuild golangci; do
		[ -d ".cache/$d" ] || continue
		kb=$($SUDO du -sk ".cache/$d" 2>/dev/null | cut -f1)
		b=$($SUDO tar -cf - -C .cache "$d" 2>/dev/null | zstd -T0 -q | wc -c)
		sum=$((sum + b))
		emit "what=du $* dir=$d kb=${kb:-unknown} zst_bytes=$b"
	done
	emit "what=du $* dir=all zst_bytes=$sum took=$(since "$t0")"
}

case "${1:-}" in
timed)
	shift
	kv=""
	while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
		kv="$kv $1"
		shift
	done
	shift
	t0=$(now)
	"$@"
	rc=$?
	emit "what=timed$kv secs=$(since "$t0") rc=$rc"
	exit $rc
	;;

du)
	shift
	du_cache "$@"
	;;

drop)
	if [ ! -d .cache ]; then
		emit "what=du family=$2 dir=none"
		exit 0
	fi
	du_cache "family=$2"
	$SUDO rm -rf .cache
	;;

merge)
	# One family's cache as restored, measured, then added to the union.
	# Go's module cache, its build cache and golangci-lint's cache name
	# their entries by content, so the union of several is a cache.
	union="$RUNNER_TEMP/union"
	if [ ! -d .cache ]; then
		emit "what=du family=$2 dir=none"
		exit 0
	fi
	du_cache "family=$2"
	t0=$(now)
	if [ -d "$union" ]; then
		if command -v rsync >/dev/null 2>&1; then
			$SUDO rsync -a .cache/ "$union/" || exit 1
		else
			$SUDO cp -a .cache/. "$union/" || exit 1
		fi
		$SUDO rm -rf .cache
	else
		$SUDO mv .cache "$union" || exit 1
	fi
	emit "what=timed name=merge family=$2 secs=$(since "$t0") rc=0"
	;;

union)
	$SUDO rm -rf .cache
	$SUDO mv "$RUNNER_TEMP/union" .cache || exit 1
	du_cache "family=union"
	;;

moddownload)
	$COMPOSE run --rm -T -e GITHUB_JOB dev sh .github/scripts/zz_moddownload.sh
	emit "what=moddownload part=container rc=$?"
	# go mod download writes sums (go.work.sum, in workspace mode):
	# nothing after this step may see them.
	changed=$(git status --porcelain -- '*.sum' '*.mod' | awk '{print $2}' | tr '\n' ',')
	emit "what=moddownload part=tree changed=${changed:-none}"
	[ -z "$changed" ] || git checkout -- '*.sum' '*.mod'
	;;

pair)
	# make vet ran before in this job: the compose network and the golang
	# image are there before the two start.
	log=$(mktemp)
	st=$(mktemp)
	t0=$(now)
	(
		make --no-print-directory test-integration COMPOSE="$COMPOSE" >"$log" 2>&1
		echo "$? $(since "$t0")" >"$st"
	) &
	bg=$!
	make --no-print-directory test COMPOSE="$COMPOSE"
	urc=$?
	usecs=$(since "$t0")
	wait "$bg"
	both=$(since "$t0")
	irc=unknown
	isecs=unknown
	read -r irc isecs <"$st" || true
	echo "MEASURE-BEGIN integration-beside"
	cat "$log"
	echo "MEASURE-END integration-beside"
	emit "what=pair part=unit secs=$usecs rc=$urc"
	emit "what=pair part=integration secs=$isecs rc=$irc"
	emit "what=pair part=both secs=$both"
	[ "$urc" = 0 ] && [ "$irc" = 0 ]
	exit $?
	;;

*)
	echo "zz_measure.sh: unknown: $*" >&2
	exit 2
	;;
esac
exit 0
