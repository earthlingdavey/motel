#!/bin/sh
# Runs once before moteld inside the e2e-motel container
# (e2e-token.service). The offline compose network has no CI OIDC
# provider, so the liveswap deploy path is exercised through the
# `local` trust source: mint a keypair + a long-lived token into the
# shared volume (idempotent, so a restart keeps the same token). The
# e2e Caddyfile trusts /shared/deploy.pub; the runner reads
# /shared/deploy.token.
set -e

if [ -d /shared ] && [ ! -f /shared/deploy.token ]; then
	# Clear any partial state (deploy-keygen refuses to overwrite), mint a
	# far-future token so a persisted volume never serves a stale one, and
	# publish the token atomically so the runner can't read a half-written
	# file. motel (User=motel) and the runner read the 0644 halves.
	rm -f /shared/deploy.key /shared/deploy.key.pub /shared/deploy.pub
	moteld deploy-keygen --out /shared/deploy.key >/dev/null 2>&1
	cp /shared/deploy.key.pub /shared/deploy.pub
	moteld deploy-token --key /shared/deploy.key --audience e2e --ttl 87600h >/shared/deploy.token.tmp
	chmod 644 /shared/deploy.pub /shared/deploy.token.tmp
	mv /shared/deploy.token.tmp /shared/deploy.token
fi
