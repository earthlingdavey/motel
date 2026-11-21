#!/bin/sh
# The Go an e2e lane's images take, built in the dev container, where
# Go's caches are the volumes every other target's builds share (in CI,
# ./.cache under actions/cache): a build inside the image starts from
# nothing on a builder that kept no cache, and CI's builder never has
# one. Run from the repo root, in the dev container:
#
#   sh e2e/bin.sh <lane> [notify-tests]
#
# and it leaves, in build/<lane>, what e2e/Dockerfile and
# e2e/liveswap/artifacts.Dockerfile copy from their `bin` context
# (docker-compose.yml, ./build/${E2E_LANE}): moteld, linked as a release
# is (Caddy's CustomVersion; the people suite reads it), motel-backup,
# the laptop client motel, the test app for this machine and for the
# other one, and the artifacts server; with notify-tests, notify's test
# binary too, which the metrics box alone runs. The make targets
# (e2e, e2e-notify, e2e-metrics, soak) and e2e/bench/observe.sh run it
# right before they build the images. An image built any other way
# takes whatever the lane's last run left there.
#
# A directory a lane, so that two lanes started at once in one checkout
# write no file the other's image reads. It is emptied first: a build
# that fails leaves no binary of an earlier source for an image to take.
#
# moteld first and alone, the rest at once after it: they compile
# little that moteld's build has not left in the cache. -trimpath on
# every build, so that they are the same compile actions, and e2e-backup's
# and `make build`'s too.
#
# Each build says how many seconds it took, and the script the whole:
# where a lane's start goes is read off its log.
set -eu
lane=${1:?usage: sh e2e/bin.sh <lane> [notify-tests]}
out=build/$lane
export CGO_ENABLED=0

build() { # <name> <command ...>
	name=$1
	shift
	s=$(date +%s)
	"$@" || return 1
	echo "e2e/bin.sh: $name in $(($(date +%s) - s)) s"
}
t0=$(date +%s)

rm -rf "$out"
mkdir -p "$out"

build moteld go build -trimpath -ldflags "-X github.com/caddyserver/caddy/v2.CustomVersion=v0.0.0-e2e" -o "$out/moteld" ./cmd/moteld

other=amd64
[ "$(go env GOARCH)" = amd64 ] && other=arm64
pids=""
build motel-backup go build -C backups -trimpath -o "../$out/motel-backup" ./cmd/motel-backup & pids="$pids $!"
build motel go build -C client -trimpath -o "../$out/motel" ./cmd/motel & pids="$pids $!"
build server go build -trimpath -o "$out/server" ./e2e/liveswap/testapp & pids="$pids $!"
build server-other env GOARCH=$other go build -trimpath -o "$out/server-other" ./e2e/liveswap/testapp & pids="$pids $!"
build artifacts-server go build -trimpath -o "$out/artifacts-server" ./e2e/liveswap/artifacts & pids="$pids $!"
if [ "${2:-}" = notify-tests ]; then
	build notify.test go test -C notify -c -trimpath -o "../$out/notify.test" . & pids="$pids $!"
fi
status=0
for p in $pids; do
	wait "$p" || status=1
done
# The container is root: without this the host's user could not remove
# build/ (make build's chmod says the same).
chmod -R a+rwX build
echo "e2e/bin.sh: $out in $(($(date +%s) - t0)) s"
exit $status
