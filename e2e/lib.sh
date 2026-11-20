#!/bin/sh
# Shared helpers for the e2e suites (sourced, not run). Callers set
# PROXY / HOOK / ART first; wait_for_token then sets TOKEN from the
# deploy token the e2e-motel container mints into the shared
# volume (e2e/mint-token.sh) — the `deploy_trust local` path standing
# in for a CI OIDC provider the offline compose network cannot reach.
FAILURES=0
TOKEN_FILE="${DEPLOY_TOKEN_FILE:-/shared/deploy.token}"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILURES=$((FAILURES + 1)); }

wait_for_token() {
	i=0
	until [ -s "$TOKEN_FILE" ]; do
		i=$((i + 1))
		if [ "$i" -ge 60 ]; then
			echo "FATAL: no deploy token at $TOKEN_FILE within 60s"
			exit 1
		fi
		sleep 1
	done
	TOKEN=$(cat "$TOKEN_FILE")
}

# deploy <artifact-file> <version> -> prints HTTP status code; body in /tmp/deploy-body
deploy() {
	curl -s -o /tmp/deploy-body -w '%{http_code}' --max-time 60 \
		-X POST -H "Authorization: Bearer $TOKEN" \
		-d "{\"url\":\"$ART/$1\",\"version\":\"$2\"}" "$HOOK"
}

body() { curl -s --max-time 5 "$PROXY/"; }

# person_token <key file> <aud>: a person's token signed with the ssh
# key at <key file> for the box <aud>, good for five minutes, made as
# docs/people.md makes one by hand (the people and notify suites).
b64url() { base64 | tr -d '\n=' | tr '+/' '-_'; }
person_token() {
	pt=$(mktemp -d) || return 1
	now=$(date +%s)
	printf '{"aud":"%s","iat":%d,"exp":%d}' "$2" "$now" "$((now + 300))" >"$pt/claims"
	if ! ssh-keygen -q -Y sign -n motel-access-v1@motel.smallhours.org -f "$1" "$pt/claims"; then
		rm -rf "$pt"
		return 1
	fi
	printf 'm1.%s.%s' "$(b64url <"$pt/claims")" "$(sed '1d;$d' "$pt/claims.sig" | tr -d '\n' | base64 -d | b64url)"
	rm -rf "$pt"
}

status() { curl -s --max-time 5 -H "Authorization: Bearer $TOKEN" "$HOOK"; }

# json_str <json> <key> / json_num <json> <key>: one top-level scalar,
# enough for the status endpoint's flat fields (no jq in the images).
json_str() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"; }
json_num() { printf '%s' "$1" | sed -n "s/.*\"$2\":\([0-9][0-9]*\).*/\1/p"; }
# json_arr <json> <key>: a top-level array of strings as one space-
# separated line, which is all `command` needs to be compared.
json_arr() { printf '%s' "$1" | sed -n "s/.*\"$2\":\[\([^]]*\)\].*/\1/p" | tr -d '"' | tr ',' ' '; }
