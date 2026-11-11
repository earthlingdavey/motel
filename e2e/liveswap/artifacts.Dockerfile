# Builds the demo app once, packs its release tarballs (v1, v2, a v3
# whose health check always fails, a "workers" release whose ./server
# is a shell leader that forks a worker before exec'ing the app — the
# process-tree shape the systemd suite kills — a "probe" release whose
# ./server records its sandbox view before exec'ing the app, and a
# "crash" release whose ./server prints its secret and exits 3, a
# "wrongarch" release whose ./server was built for the other machine —
# the tarball a workflow on the wrong runs-on produces — and an
# "errors" release, healthy, whose every other path answers 500: the
# metrics suite's post-deploy comparison), and serves
# them over HTTP — the stand-in for a GitHub/GitLab release
# asset URL.
#
# The build context is the repo root, not this directory (see
# docker-compose.yml), which is why every COPY path is repo-relative:
# the probe release carries liveswap/testdata/sandbox-view.sh, one view probe shared
# with liveswap's integration test and packaging/test/smoke.sh (#52).
FROM golang:1.27-trixie AS build
# The box's architecture, from BuildKit; the wrongarch release is
# built for the other one.
ARG TARGETARCH
WORKDIR /build
COPY e2e/liveswap/testapp/main.go e2e/liveswap/workers.sh e2e/liveswap/probe-server.sh e2e/liveswap/crash.sh liveswap/testdata/sandbox-view.sh ./
RUN CGO_ENABLED=0 go build -o server main.go
COPY e2e/liveswap/artifacts/main.go ./artifacts/
RUN CGO_ENABLED=0 go build -o artifacts-server ./artifacts/main.go
RUN other=amd64; [ "$TARGETARCH" = amd64 ] && other=arm64; CGO_ENABLED=0 GOARCH=$other go build -o server-other main.go
RUN mkdir /out \
	&& for v in v1 v2; do \
		mkdir /tmp/stage-$v \
		&& cp /build/server /tmp/stage-$v/ \
		&& echo $v > /tmp/stage-$v/version.txt \
		&& tar -czf /out/demo-$v.tar.gz -C /tmp/stage-$v .; \
	done \
	&& mkdir /tmp/stage-v3 \
	&& cp /build/server /tmp/stage-v3/ \
	&& echo v3 > /tmp/stage-v3/version.txt \
	&& touch /tmp/stage-v3/broken \
	&& tar -czf /out/demo-v3-broken.tar.gz -C /tmp/stage-v3 . \
	&& mkdir /tmp/stage-errors \
	&& cp /build/server /tmp/stage-errors/ \
	&& echo errors > /tmp/stage-errors/version.txt \
	&& touch /tmp/stage-errors/errors \
	&& tar -czf /out/demo-errors.tar.gz -C /tmp/stage-errors . \
	&& mkdir /tmp/stage-workers \
	&& cp /build/server /tmp/stage-workers/server-bin \
	&& cp /build/workers.sh /tmp/stage-workers/server \
	&& chmod +x /tmp/stage-workers/server \
	&& echo workers > /tmp/stage-workers/version.txt \
	&& tar -czf /out/demo-workers.tar.gz -C /tmp/stage-workers . \
	&& mkdir /tmp/stage-probe \
	&& cp /build/server /tmp/stage-probe/server-bin \
	&& cp /build/probe-server.sh /tmp/stage-probe/server \
	&& cp /build/sandbox-view.sh /tmp/stage-probe/ \
	&& chmod +x /tmp/stage-probe/server \
	&& echo probe > /tmp/stage-probe/version.txt \
	&& tar -czf /out/demo-probe.tar.gz -C /tmp/stage-probe . \
	&& mkdir /tmp/stage-crash \
	&& cp /build/crash.sh /tmp/stage-crash/server \
	&& chmod +x /tmp/stage-crash/server \
	&& echo crash > /tmp/stage-crash/version.txt \
	&& tar -czf /out/demo-crash.tar.gz -C /tmp/stage-crash . \
	&& mkdir /tmp/stage-wrongarch \
	&& cp /build/server-other /tmp/stage-wrongarch/server \
	&& echo wrongarch > /tmp/stage-wrongarch/version.txt \
	&& tar -czf /out/demo-wrongarch.tar.gz -C /tmp/stage-wrongarch .

# The Deno example, built by its own scripts/bundle.sh with the same
# Deno version e2e/Dockerfile installs on the box.
FROM denoland/deno:2.9.8 AS deno-build
WORKDIR /example
COPY examples/deno/ ./
# Same pin as the example's .deno-version (and e2e/Dockerfile), or fail.
RUN [ "$(deno --version | sed -n 's/^deno \([0-9.]*\).*/\1/p')" = "$(cat .deno-version)" ] \
	|| { echo "artifacts.Dockerfile builds with Deno $(deno --version | head -1) but .deno-version says $(cat .deno-version)" >&2; exit 1; }
RUN sh scripts/bundle.sh

# The Node example, built by its own scripts/bundle.sh: Node's
# single-executable build, so the e2e box needs no Node installed.
FROM node:26-trixie-slim AS node-build
WORKDIR /example
COPY examples/node/ ./
RUN sh scripts/bundle.sh

# The server is e2e/liveswap/artifacts/main.go, a static binary, so the
# image is nothing but it and the tarballs.
FROM scratch
COPY --from=build /build/artifacts-server /artifacts-server
COPY --from=build /out /srv/artifacts
COPY --from=deno-build /example/app.tar.gz /srv/artifacts/deno-example.tar.gz
COPY --from=node-build /example/app.tar.gz /srv/artifacts/node-example.tar.gz
ENTRYPOINT ["/artifacts-server"]
