# Builds the exact artifact an operator ships: the motel binary from
# cmd/motel (caddy standard + liveswap + penaltybox + Souin + Otter).
# The e2e-motel container then plays the production VPS: systemd as
# PID 1 running the packaged motel.service as the motel user,
# with deployed apps as transient units under that user's own systemd
# manager — exactly the layout the .deb's postinstall produces.
# (e2e-upstream reuses the image as a plain `motel run` container.)

# The Deno runtime for the examples/deno lane, installed under
# /usr/local, inside every app's sandbox view. A FROM, so Dependabot
# watches it; keep it the same version as the build image in
# e2e/liveswap/artifacts.Dockerfile and the example's workflow.
FROM denoland/deno:bin-2.9.8 AS deno

# The source is bind-mounted, not copied, and Go's module and build
# caches are BuildKit cache mounts, kept by the builder between builds
# and shared by every worktree's: a one-line change compiles one package
# and links, where `COPY . .` and a cold build downloaded every module
# and compiled everything again after any edit to the context, and left
# both caches in the layer [measured: 3.4 GB of build cache a rebuild].
# The mount is writable because go may write go.work.sum; what it writes
# is dropped with the step. The cache ids are motel's, shared by its
# worktrees and no other build. A builder pruned, or CI's, starts cold.
FROM golang:1.27-trixie AS builder
WORKDIR /src
RUN --mount=type=bind,target=/src,rw \
	--mount=type=cache,id=motel-gomod,target=/go/pkg/mod \
	--mount=type=cache,id=motel-gobuild,target=/root/.cache/go-build \
	CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /motel ./cmd/motel \
	&& cd backups && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /motel-backup ./cmd/motel-backup

# notify's tests that run vmalert and vmalert-tool over the rendered
# rules, compiled for the metrics box, which has both (Debian's
# victoria-metrics); the dev image has neither, so `make test` skips
# them. Built only for the metrics box, from the toolchain alone, so
# BuildKit builds it beside the builder stage.
FROM golang:1.27-trixie AS notify-tests
WORKDIR /src
RUN --mount=type=bind,target=/src,rw \
	--mount=type=cache,id=motel-gomod,target=/go/pkg/mod \
	--mount=type=cache,id=motel-gobuild,target=/root/.cache/go-build \
	cd notify && CGO_ENABLED=0 GOFLAGS=-buildvcs=false go test -c -o /notify.test .

# The box. A named stage, because the notify suite's box below builds on
# it: docker-compose.yml names this one as e2e-motel's and e2e-upstream's
# target, or they would get the last stage.
FROM debian:trixie-slim AS box
ENV DEBIAN_FRONTEND=noninteractive
# libpam-systemd: pam_systemd gives user@<uid>.service its runtime dir;
# dbus: logind; procps + curl: the in-container systemd suite;
# libatomic1: what a Node single executable needs on arm64 (the one
# package examples/node asks the box for); openssh-client: ssh-keygen,
# which the box applier verifies signatures with (the package Depends
# on it); git: the box suite makes and signs its commits on the box;
# age and make: the box template's secrets commands, which the suite
# runs as a laptop would.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus libpam-systemd procps curl ca-certificates libatomic1 \
		openssh-client git age make \
	&& rm -rf /var/lib/apt/lists/*
RUN systemctl mask \
	systemd-udevd.service \
	systemd-udevd-kernel.socket \
	systemd-udevd-control.socket \
	systemd-modules-load.service \
	getty.target \
	console-getty.service \
	|| true

COPY --from=builder /motel /usr/bin/motel
# The box channel's step 6 asks it whether a backup run could plan
# from the Caddyfile being pushed; nothing here runs a backup.
COPY --from=builder /motel-backup /usr/bin/motel-backup
COPY --from=deno /deno /usr/local/bin/deno
# The example pins the Deno its workflow builds with in .deno-version;
# the FROM above is Dependabot's to bump. Refuse to build when the two
# differ, so a bump cannot leave the example testing one version and
# deploying another.
COPY examples/deno/.deno-version /tmp/.deno-version
RUN [ "$(deno --version | sed -n 's/^deno \([0-9.]*\).*/\1/p')" = "$(cat /tmp/.deno-version)" ] \
	|| { echo "e2e/Dockerfile pins Deno $(deno --version | head -1) but examples/deno/.deno-version says $(cat /tmp/.deno-version)" >&2; exit 1; }
# Fail the build outright if any module didn't link in. (Otter is
# additionally proven at startup: the `otter` cache option makes config
# load fail if the storage module is absent.)
RUN motel list-modules | grep -qx liveswap \
	&& motel list-modules | grep -qx http.handlers.liveswap_webhook \
	&& motel list-modules | grep -qx http.reverse_proxy.upstreams.liveswap \
	&& motel list-modules | grep -qx http.handlers.hint_penaltybox \
	&& motel list-modules | grep -qx http.handlers.cache \
	&& motel list-modules | grep -qx storages.cache.otter

# The production layout, produced by the package's own postinstall
# (user, data dirs, lingering, the user@<uid> drop-ins) so the e2e
# container is what a .deb install is — never a re-implementation.
# systemd is not running at build time; postinstall tolerates that
# (linger-file fallback, daemon-reload/try-restart ignored).
COPY packaging/motel.service /etc/systemd/system/motel.service
# The box applier's units and exchange tree, where the package puts
# them (packaging/nfpm.yaml); postinstall makes the tree.
COPY packaging/motel-box-apply.path packaging/motel-box-apply.service /lib/systemd/system/
COPY packaging/motel-box.tmpfiles /usr/lib/tmpfiles.d/motel-box.conf
# The starter and the directories the package ships for it; postinstall
# installs it and its record (e2e/Caddyfile replaces it below).
COPY packaging/Caddyfile /usr/share/motel/Caddyfile
COPY packaging/postinstall.sh /tmp/postinstall.sh
RUN mkdir -p /var/lib/motel /var/lib/liveswap /etc/motel /var/lib/motel-package \
	&& chmod 750 /var/lib/motel /var/lib/liveswap \
	&& chmod 755 /etc/motel && chmod 700 /var/lib/motel-package \
	&& sh /tmp/postinstall.sh && rm /tmp/postinstall.sh
COPY e2e/systemd/e2e.conf /etc/systemd/system/motel.service.d/e2e.conf
COPY e2e/systemd/e2e-token.service /etc/systemd/system/e2e-token.service
# postinstall enables the path unit only through deb-systemd-helper,
# which needs a dpkg install to remember; enabled here as it would be.
RUN systemctl enable motel.service e2e-token.service motel-box-apply.path
COPY e2e/Caddyfile /etc/motel/Caddyfile
# The demo app's env file, as the box README says to make one: root
# and the motel user can read it, nothing else. The crash scenario
# deploys a release that prints SECRET and exits.
RUN printf 'SECRET=e2e-secret-value-1234\n' > /etc/motel/demo.env \
	&& chown root:motel /etc/motel/demo.env && chmod 0640 /etc/motel/demo.env
# The Deno example's app-block lines, imported by e2e/Caddyfile: the
# lines its README says to copy onto the box are the lines tested.
COPY examples/deno/motel.caddy /etc/motel/deno-example.caddy
COPY examples/node/motel.caddy /etc/motel/node-example.caddy
COPY e2e/mint-token.sh /usr/local/bin/mint-token.sh
RUN chmod +x /usr/local/bin/mint-token.sh

# An administrator who is not root, in the adm group alone, for the
# journal. The box suite checks that this user reads the logs, and no
# env file and not the box's key.
RUN useradd -m -G adm admin

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]

# The notify suite's box (make e2e-notify, e2e/notify/run.sh): the same
# box with Debian's Alertmanager installed as `motel monitor setup` would
# install it, masked first so that nothing it enables starts; the suite
# runs setup itself, which finds the package installed and does the
# rest. iproute2: the suite's `ss`, for what listens.
FROM box AS notify-box
RUN systemctl mask prometheus-alertmanager.service \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends prometheus-alertmanager iproute2 \
	&& rm -rf /var/lib/apt/lists/*
# shop's deploy key, which nothing binds a channel to; /shared is where
# e2e-token.service mints blog's (no volume: this box shares it with no
# runner).
RUN mkdir -p /shared \
	&& motel deploy-keygen --out /etc/motel/shop.key >/dev/null 2>&1 \
	&& cp /etc/motel/shop.key.pub /etc/motel/shop.pub && chmod 644 /etc/motel/shop.pub
COPY e2e/notify/Caddyfile /etc/motel/Caddyfile
# The box tier's secrets, as the applier would write secrets/_box/: one
# URL per channel, each a path on the stub receiver.
RUN install -d -m 2750 -o root -g motel /etc/motel/secrets \
	&& printf '%s\n' \
		'NOTIFY_OPS_URL="http://e2e-sink:8080/ops"' \
		'NOTIFY_ONCALL_URL="http://e2e-sink:8080/broken"' \
		'NOTIFY_BLOG_URL="http://e2e-sink:8080/blog"' \
		'NOTIFY_HC_URL="http://e2e-sink:8080/deadman"' \
		> /etc/motel/secrets/_box.env \
	&& chown root:motel /etc/motel/secrets/_box.env && chmod 0640 /etc/motel/secrets/_box.env

# The metrics suite's box (make e2e-metrics, e2e/metrics/run.sh): the
# notify suite's box with Debian's victoria-metrics (the store and
# vmalert) installed as `motel monitor setup --metrics` would install
# it, masked first so that its unit, which listens on 0.0.0.0:8428, never
# starts; the suite runs `motel monitor setup`, then `motel monitor
# setup --metrics`, the store's setup, which finds the package installed
# and does the rest.
FROM notify-box AS metrics-box
RUN systemctl mask victoria-metrics.service \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends victoria-metrics \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=notify-tests /notify.test /usr/local/bin/notify.test
COPY e2e/metrics/Caddyfile /etc/motel/Caddyfile
