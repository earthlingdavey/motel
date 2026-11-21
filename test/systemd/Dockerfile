# Go toolchain + systemd, for the integration suite (`make
# test-integration`). liveswap's runner drives a real systemd user
# manager over D-Bus, so the tests that exercise it — the runner's own
# suite and the caddytest scenarios that deploy a real app — need
# systemd as PID 1. Same recipe as packaging/test/Dockerfile
# (--privileged, --cgroupns=host, cgroup mount), on the golang image
# the plain `dev` service uses, so the module and build caches are
# shared between the two.
#
# libpam-systemd: pam_systemd is what hands user@<uid>.service its
# XDG_RUNTIME_DIR; without it no user manager can start. dbus: logind.
# procps: pgrep/ps for the process-tree assertions. sqlite3 and restic:
# Debian's, because what the backups module's integration tests
# measure is how those two programs behave.
# The Go is docker-compose.yml's dev image, by the same tag and digest:
# the Go that builds what ships.
FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		systemd systemd-sysv dbus libpam-systemd procps sqlite3 restic \
	&& rm -rf /var/lib/apt/lists/*

RUN systemctl mask \
	systemd-udevd.service \
	systemd-udevd-kernel.socket \
	systemd-udevd-control.socket \
	systemd-modules-load.service \
	getty.target \
	console-getty.service \
	|| true

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
