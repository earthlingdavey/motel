# Systemd-in-docker image for the package install smoke test
# (`make install-test`). BASE_IMAGE is debian:13 — the support matrix is
# one release (systemd 257), so the smoke test asserts the full sandbox
# tier unconditionally. BASE_IMAGE stays a build arg so a candidate
# release can be tried against a newer Debian without editing this file.
#
# networkd/resolved are deliberately NOT installed: with no wait-online
# unit enabled, network-online.target (which motel.service Requires=)
# is reached trivially inside the container. dbus is here because
# logind (loginctl enable-linger in postinstall) needs the system bus;
# libpam-systemd is NOT — it is a package Depends, and the smoke test
# proves apt resolves it.
# (Declared before the first FROM, so that every stage sees it.)
ARG BASE_IMAGE=debian:13

# rclone: the S3 server the backups README's lines are run against —
# `rclone serve s3` in this same container, the static binary out of
# rclone's own image as e2e/backup/s3.Dockerfile takes it (Debian's
# rclone has no `serve s3`). The same pin as there; Dependabot bumps
# both.
FROM rclone/rclone:1.76@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466 AS rclone

FROM ${BASE_IMAGE}

# No recommended package, ever, as a provider's image may have it:
# Hetzner's Debian 13 sets this. Every install made here — the image's
# own below, the smoke's first, each reinstall after a remove or a
# purge — then gets only what is depended on, and a Recommends entry
# the package came to lean on would fail there rather than pass by
# apt's default.
RUN printf 'APT::Install-Recommends "false";\n' > /etc/apt/apt.conf.d/99no-recommends
# No sudo: the backups guide's lines are root's, and run as root, the
# way they are written. restic, sqlite3 and ca-certificates are NOT
# here: the package depends on them, and the smoke test proves
# apt brings all three to an image that installs no recommended
# package (nothing here speaks TLS; the smoke's servers are loopback
# http).
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
	&& apt-get install -y \
		systemd systemd-sysv dbus curl \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=rclone /usr/local/bin/rclone /usr/local/bin/rclone
# The Debian image forbids maintainer scripts to start services
# (/usr/sbin/policy-rc.d exits 101, which deb-systemd-invoke obeys). A
# real box has no such file, and postinstall starting the backup timers
# is part of what the smoke test proves.
RUN rm -f /usr/sbin/policy-rc.d

# The Debian image forbids maintainer scripts to start services
# (/usr/sbin/policy-rc.d exits 101, which deb-systemd-invoke obeys). A
# real box has no such file, and postinstall starting the box applier's
# path unit is part of what the smoke test proves.
RUN rm -f /usr/sbin/policy-rc.d

# Units that cannot work in a container and would otherwise leave the
# boot stuck or the system needlessly degraded.
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
