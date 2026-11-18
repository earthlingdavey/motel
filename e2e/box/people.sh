#!/bin/sh
# People end to end (liveswap/access/DESIGN-access.md, docs/people.md),
# inside the e2e box: the e2e-motel container, systemd as PID 1,
# moteld.service as the motel user, the packaged applier units. The
# box's file declares people with keys made here by ssh-keygen; it is
# installed by `moteld init` and changed by a signed push sent with the
# box template's bin/apply, the path a box takes. Then motelctl, the
# laptop's client, built from this tree, reads the box as a laptop
# does: another process, over the box's own TLS on 443 (Caddy's
# internal CA, its root given as SSL_CERT_FILE), the box's name
# resolved by /etc/hosts. curl sends the requests whose bytes are
# compared, made as docs/people.md makes one by hand.
#
# The box sits behind nothing: the e2e config's trusted_proxies line is
# taken out of this file, so client_ip is the peer, 127.0.0.1, and an
# X-Forwarded-For the client writes is no address of its own.
#
# Runs after the box apply suite in `make e2e` (`docker compose
# exec`); puts the e2e config and /etc/hosts back at the end.
set -u
. /lib.sh

HOST=people.e2e.test
BASE=https://$HOST
LIVE=/etc/motel/Caddyfile
BOXDIR=/var/lib/motel-box
FILE=$HOST/Caddyfile
ART=http://e2e-artifacts:8080/artifacts
NS=motel-access-v1@motel.smallhours.org
CA=/var/lib/motel/caddy/pki/authorities/local/root.crt
RELEASE=v0.0.0-e2e # e2e/Dockerfile links it

work=$(mktemp -d)
repo=$work/repo
cp "$LIVE" "$work/original"
cp /etc/hosts "$work/hosts"

# restore: the e2e config back, no baseline, /etc/hosts as it was (a
# bind mount: written in place). Once, whichever way the suite ends.
restored=
restore() {
	[ -n "$restored" ] && return 0
	restored=1
	cat "$work/hosts" >/etc/hosts
	cp "$work/original" "$LIVE.e2e-tmp" && chmod 0644 "$LIVE.e2e-tmp" && mv "$LIVE.e2e-tmp" "$LIVE" \
		&& rm -f "$BOXDIR/applied.json" \
		&& { systemctl reload moteld || systemctl restart moteld; }
}
trap 'restore; rm -rf "$work"' EXIT

# moteld up, as the notify and metrics suites wait for it: run alone
# straight after `up -d`, the box may still be starting, and moteld init
# needs a running moteld. The unit alone: this e2e config's admin API is
# on TCP, so there is no admin socket to wait for.
i=0
until systemctl is-active -q moteld; do
	i=$((i + 1)); [ "$i" -ge 120 ] && { echo "FATAL: moteld did not come up"; journalctl -u moteld --no-pager -n 50; exit 1; }
	sleep 0.5
done
wait_for_token # $TOKEN: the e2e deploy token, audience e2e

# ── the keys and the box's file ────────────────────────────────────

# alice signs the commits; vera views the box; otto views demo alone;
# fran views the box from 198.51.100.0/24 only, an address this client
# does not have; nora is declared with no grant; lena views demo from
# 127.0.0.0/8, the client's own address, so that fran's refusal is the
# list's; stranger's key no line lists. Every person's key is Ed25519,
# the one kind a box takes for now; eve's is ECDSA, which no box takes
# yet, so her line is refused (people 0) and motelctl will not sign
# with it (people 1).
for k in alice vera otto fran nora lena stranger; do ssh-keygen -q -t ed25519 -N '' -C "$k" -f "$work/$k"; done
ssh-keygen -q -t ecdsa -b 256 -N '' -C eve -f "$work/eve"
key() { cut -d' ' -f1,2 "$work/$1.pub"; }
fp() { ssh-keygen -lf "$work/$1.pub" | cut -d' ' -f2; }

SIGNER="		signer alice@e2e.test $(key alice)"
VERA="		person vera@e2e.test $(key vera)
		grant vera@e2e.test viewer box"
OTTO="		person otto@e2e.test $(key otto)
		grant otto@e2e.test viewer app demo"
REST="		person fran@e2e.test $(key fran)
		grant fran@e2e.test viewer box from 198.51.100.0/24
		person nora@e2e.test $(key nora)
		person lena@e2e.test $(key lena)
		grant lena@e2e.test viewer app demo from 127.0.0.0/8"

# The e2e config as this box's file: imports inlined (a signed file
# has none), no trusted_proxies, HTTPS on 443 for the one bare-hostname
# site (internal CA, no redirect listener), the box block, and the
# deploy host last, as the template's: both webhooks, and an access
# log, whose proto field says which HTTP the client spoke.
awk -v host="$HOST" '
/^[ \t]*import [^ \t]+[ \t]*$/ { f = $2; while ((getline l < f) > 0) print l; close(f); next }
/^[ \t]*trusted_proxies / { next }
/^[ \t]*auto_https off[ \t]*$/ {
	print "\tlocal_certs"
	print "\tskip_install_trust"
	print "\tauto_https disable_redirects"
	print ""
	print "\tbox {"
	print "\t\tdeploy_trust local {"
	print "\t\t\tpublic_key /shared/deploy.pub"
	print "\t\t\taudience e2e-box"
	print "\t\t}"
	print "\t\t# lines"
	print "\t}"
	next
}
{ print }
END { print ""; print host " {"; print "\tlog"; print "\tliveswap_webhook"; print "\tbox_webhook"; print "}" }
' "$work/original" >"$work/template"
grep -q 'trusted_proxies' "$work/template" && echo "FATAL: the box's file still trusts proxies" && exit 1

# render <lines>: the box's file with <lines> in its box block.
render() {
	L=$1 awk '{ print } $0 == "\t\t# lines" { print ENVIRON["L"] }' "$work/template"
}
render "$SIGNER
$VERA
$OTTO
$REST" >"$work/f0"
render "$SIGNER
$VERA
$REST" >"$work/f1"
render "$SIGNER
$VERA
		person eve@e2e.test $(key eve)
		grant eve@e2e.test viewer box" >"$work/fe"

git init -q -b main "$repo"
git -C "$repo" config user.name e2e
git -C "$repo" config user.email e2e@e2e.test
git -C "$repo" config gpg.format ssh
mkdir -p "$repo/$HOST" "$repo/bin"
cp /template/bin/apply "$repo/bin/apply"
# commit <file>: <file> as the box's file on top of HEAD, signed by
# alice; prints the commit's id.
commit() {
	cp "$1" "$repo/$FILE"
	git -C "$repo" add -A
	git -C "$repo" -c user.signingkey="$work/alice" commit -q -S -m "people: $(basename "$1")"
	git -C "$repo" rev-parse HEAD
}

# ── the clients ────────────────────────────────────────────────────

# boxtok [<sha>]: a token for the box block's deploy_trust.
boxtok() {
	if [ -n "${1:-}" ]; then
		moteld deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m --claims "sha=$1"
	else
		moteld deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m
	fi
}

# ptok <who> [<aud>]: a person's token signed with <who>'s key, made as
# docs/people.md makes one by hand.
b64url() { base64 | tr -d '\n=' | tr '+/' '-_'; }
ptok() {
	now=$(date +%s)
	printf '{"aud":"%s","iat":%d,"exp":%d}' "${2:-$HOST}" "$now" "$((now + 300))" >"$work/claims"
	rm -f "$work/claims.sig"
	ssh-keygen -q -Y sign -n "$NS" -f "$work/$1" "$work/claims" || return 1
	printf 'm1.%s.%s' "$(b64url <"$work/claims")" "$(sed '1d;$d' "$work/claims.sig" | tr -d '\n' | base64 -d | b64url)"
}

# get <name> <curl args...>: one request over the box's TLS, the root
# verified. Status in $CODE, the HTTP version curl spoke in $PROTO; the
# headers in $work/<name>.h (Date dropped, the one line that differs
# by the second), the body in $work/<name>.b, and both in $work/<name>.
get() {
	o=$work/$1
	shift
	r=$(curl -sS --cacert "$work/root.crt" -D "$o.raw" -o "$o.b" -w '%{http_code} %{http_version}' --max-time "${MAXT:-10}" "$@")
	CODE=${r% *} PROTO=${r#* }
	tr -d '\r' <"$o.raw" | grep -iv '^date:' >"$o.h"
	cat "$o.h" "$o.b" >"$o"
}
motel_headers() { grep -i '^motel-' "$work/$1.h" | tr -d '\n' | tr 'A-Z' 'a-z'; }
# no_release <name>: no header of the answer saved as <name> holds the
# release, which only whoami's body names.
no_release() { ! grep -qF "$RELEASE" "$work/$1.h"; }

# mc <motelctl args...>: motelctl as a laptop runs it, with the box's
# private root as SSL_CERT_FILE and nothing else of the environment's
# motel variables. Exit status in $RC, stdout in $work/out, stderr in
# $work/err.
mc() {
	env -u MOTEL_TOKEN -u MOTEL_URL -u MOTEL_KEY -u MOTEL_BOX SSL_CERT_FILE="$work/root.crt" \
		motelctl "$@" >"$work/out" 2>"$work/err"
	RC=$?
}
# expect <label> <exit>: the last mc exited <exit>.
expect() {
	if [ "$RC" = "$2" ]; then
		pass "$1: exit $2"
	else
		fail "$1: expected exit $2, got $RC; stderr: $(head -c 400 "$work/err"); stdout: $(head -c 300 "$work/out")"
	fi
}
# has <label> <file> <fixed text>
has() {
	if grep -qF -- "$3" "$2"; then pass "$1"; else fail "$1: no '$3' in: $(head -c 500 "$2")"; fi
}
# jget <file> <expression over j>: a field of a JSON answer, parsed by
# a JSON parser (the box's Deno; the image has no jq).
jget() {
	F=$1 deno eval --quiet "const j = JSON.parse(Deno.readTextFileSync(Deno.env.get('F'))); console.log($2)" 2>&1
}
# app_names <file>: the names in GET /_motel/apps's body (wire.Apps),
# one space between; empty for none. The one place that reads its shape.
app_names() { jget "$1" 'j.apps.map(a => a.name).join(" ")'; }
# same <label> <a> <b>: two responses are byte for byte the same.
same() {
	if cmp -s "$work/$2" "$work/$3"; then
		pass "$1"
	else
		fail "$1: they differ: $(diff "$work/$2" "$work/$3" | head -c 600)"
	fi
}

echo "=== people 0: the box's file installed, the apps deployed through the deploy host ==="
moteld validate --adapter caddyfile --config "$work/f0" >"$work/validate" 2>&1 \
	&& pass "moteld validate takes the file with people" || { fail "moteld validate: $(tail -5 "$work/validate")"; exit 1; }
if moteld validate --adapter caddyfile --config "$work/fe" >"$work/validate-ecdsa" 2>&1; then
	fail "moteld validate took a person line with an ECDSA key"
else
	has "moteld validate refuses a person's ECDSA key by name, for now, with the remedy" "$work/validate-ecdsa" \
		"person eve@e2e.test: a person's key is an Ed25519 key for now, and this release does not take ecdsa-sha2-nistp256 yet (ssh-keygen -t ed25519, or -t ed25519-sk for a hardware key)"
fi
C0=$(commit "$work/f0")
rm -rf "$work/init" && mkdir -p "$work/init" && git -C "$repo" archive "$C0" "$HOST" | tar -xf - -C "$work/init"
printf '127.0.0.1 %s\n' "$HOST" >>/etc/hosts
since=$(date +%s)
if (cd "$work/init/$HOST" && moteld init "$C0") >"$work/init.out" 2>&1; then
	pass "moteld init installed the file and reloaded"
else
	fail "moteld init: $(cat "$work/init.out")"
	exit 1
fi
# The internal certificate is issued after the reload that first names
# the host: until it is, the handshake fails.
i=0
until [ "$(curl -sk -o /dev/null -w '%{http_code}' --max-time 5 -H "Authorization: Bearer $(boxtok)" "$BASE/")" = 200 ]; do
	i=$((i + 1))
	[ "$i" -ge 120 ] && { fail "GET / never answered 200 over HTTPS"; exit 1; }
	sleep 0.5
done
cp "$CA" "$work/root.crt"
journalctl -u moteld --since "@$since" --no-pager -o cat | grep -qF "people admitted from the Caddyfile this machine runs (/etc/motel/Caddyfile)" \
	&& pass "the journal says people are admitted from the installed file" \
	|| fail "the journal does not say people are admitted: $(journalctl -u moteld --since "@$since" --no-pager -o cat | grep -F 'box:' | tail -3)"
# bin/apply's curl takes the same root.
mkdir -p "$work/curl"
printf 'cacert = "%s"\n' "$work/root.crt" >"$work/curl/.curlrc"

# Two apps, deployed with the e2e deploy token through the deploy host:
# what a deploy workflow does, beside the people.
DV=pd$(date +%s) NV=pn$(date +%s)
MAXT=120 get dep -X POST -H "Authorization: Bearer $TOKEN" -d "{\"url\":\"$ART/demo-v1.tar.gz\",\"version\":\"$DV\"}" "$BASE/demo"
[ "$CODE" = 200 ] && pass "a deploy token deploys demo $DV over the deploy host" || fail "deploy demo: $CODE $(head -c 600 "$work/dep.b")"
MAXT=120 get dep -X POST -H "Authorization: Bearer $TOKEN" -d "{\"url\":\"$ART/node-example.tar.gz\",\"version\":\"$NV\"}" "$BASE/node-example"
[ "$CODE" = 200 ] && pass "a deploy token deploys node-example $NV over the deploy host" || fail "deploy node-example: $CODE $(head -c 600 "$work/dep.b")"
case "$(curl -s --max-time 5 http://localhost:8080/)" in
"hello v1"*) pass "demo serves the release deployed" ;;
*) fail "demo serves: $(curl -s --max-time 5 http://localhost:8080/ | head -c 200)" ;;
esac

echo "=== people 1: motelctl for each of them ==="
# matrix <who> <8 exits>: whoami, apps, status demo, status
# node-example, deploys demo, deploys node-example, deploys demo <DV>,
# box status. Each command's output is kept as $work/<who>.<n>.out/.err.
matrix() {
	who=$1
	shift
	n=0
	for cmd in whoami apps "status demo" "status node-example" "deploys demo" "deploys node-example" "deploys demo $DV" "box status"; do
		# shellcheck disable=SC2086 # the command's words
		mc $cmd --box "$HOST" --key "$work/$who"
		expect "$who: motelctl $cmd" "$1"
		cp "$work/out" "$work/$who.$n.out"
		cp "$work/err" "$work/$who.$n.err"
		n=$((n + 1))
		shift
	done
}
t_matrix=$(date +%s)
matrix vera 0 0 0 0 0 0 0 0
has "vera: whoami names her" "$work/vera.0.out" "name     vera@e2e.test"
has "vera: whoami names the box and its release" "$work/vera.0.out" "box      $HOST, motel $RELEASE, protocol 1"
has "vera: whoami shows the key that signed" "$work/vera.0.out" "$(fp vera)"
has "vera: whoami gives the box's viewer role" "$work/vera.0.out" "roles    viewer of the box"
[ "$(sort "$work/vera.1.out" | tr '\n' ' ')" = "demo deno-example node-example " ] \
	&& pass "vera: apps lists every app the box runs" || fail "vera: apps: $(cat "$work/vera.1.out")"
has "vera: status demo names the version deployed" "$work/vera.2.out" "$DV"
has "vera: status node-example names the version deployed" "$work/vera.3.out" "$NV"
has "vera: deploys demo lists the deploy" "$work/vera.4.out" "$DV"
has "vera: deploys demo <v> is that deploy's record" "$work/vera.6.out" "demo $DV  succeeded"
has "vera: box status names the commit the box runs" "$work/vera.7.out" "commit           $C0"

matrix otto 0 0 0 5 0 5 0 5
has "otto: whoami gives demo's viewer role alone" "$work/otto.0.out" "roles    viewer of app demo"
[ "$(cat "$work/otto.1.out")" = demo ] && pass "otto: apps lists demo alone" || fail "otto: apps: $(cat "$work/otto.1.out")"
has "otto: status demo names the version deployed" "$work/otto.2.out" "$DV"
mc status no-such-app --box "$HOST" --key "$work/otto"
expect "otto: an app that does not exist is the same 403" 5
sed "s/no-such-app/node-example/g" "$work/err" >"$work/otto.none.err"
cmp -s "$work/otto.none.err" "$work/otto.3.err" \
	&& pass "otto: the refusal for an app that does not exist reads as for one that does" \
	|| fail "otto: the two 403s differ: $(diff "$work/otto.none.err" "$work/otto.3.err" | head -c 400)"

matrix nora 0 0 5 5 5 5 5 5
has "nora: whoami names her" "$work/nora.0.out" "name     nora@e2e.test"
has "nora: whoami says no role" "$work/nora.0.out" "roles    none from this address"
[ ! -s "$work/nora.1.out" ] && pass "nora: apps lists nothing" || fail "nora: apps: $(cat "$work/nora.1.out")"

matrix fran 4 4 4 4 4 4 4 4
matrix stranger 4 4 4 4 4 4 4 4
# The same message for the two, once each one's key is set aside.
# unkeyed <who> <file>: <file> with <who>'s key path and fingerprint
# replaced, in $work/<who>.unkeyed.
unkeyed() { sed -e "s#$work/$1#<key>#g" -e "s#$(fp "$1")#<fp>#g" "$2" >"$work/$1.unkeyed"; }
n=0 differ=
while [ "$n" -lt 8 ]; do
	unkeyed fran "$work/fran.$n.err"
	unkeyed stranger "$work/stranger.$n.err"
	cmp -s "$work/fran.unkeyed" "$work/stranger.unkeyed" \
		|| differ="$differ ($n) $(diff "$work/fran.unkeyed" "$work/stranger.unkeyed" | head -c 400)"
	n=$((n + 1))
done
[ -z "$differ" ] && pass "fran, outside her from list, is told what the unlisted key is told: $(head -c 200 "$work/fran.unkeyed")" \
	|| fail "fran and stranger are told different things:$differ"

mc whoami --box "$HOST" --key "$work/lena"
expect "lena, inside her from list: whoami" 0
has "lena: whoami gives demo's viewer role from here" "$work/out" "roles    viewer of app demo"

# eve's ECDSA key: motelctl refuses it before it signs or sends, and
# `motelctl key` prints no person line the box would refuse.
for cmd in "whoami --box $HOST" key; do
	# shellcheck disable=SC2086 # the command's words
	mc $cmd --key "$work/eve.pub"
	expect "eve's ECDSA key: motelctl ${cmd%% *} refuses it" 2
	has "eve: motelctl ${cmd%% *} says why, and the remedy" "$work/err" "is an ecdsa-sha2-nistp256 key. A box takes Ed25519 keys for now, not ECDSA yet: make one with ssh-keygen -t ed25519"
	[ ! -s "$work/out" ] && pass "eve: motelctl ${cmd%% *} prints nothing on stdout" || fail "eve: motelctl ${cmd%% *} printed: $(head -c 300 "$work/out")"
done

echo "=== people 2: --json is the box's answer ==="
mc whoami --json --box "$HOST" --key "$work/vera"
expect "vera: whoami --json" 0
[ "$(jget "$work/out" 'j.name + " " + j.box + " " + j.key + " " + j.version + " " + j.protocol + " " + j.roles.map(r => r.role + ":" + r.scope).join(",")')" = "vera@e2e.test $HOST $(fp vera) $RELEASE 1 viewer:box" ] \
	&& pass "vera: whoami --json parses and holds her name, the box, her key, the release and her role" \
	|| fail "vera: whoami --json: $(jget "$work/out" 'JSON.stringify(j)')"
mc apps --json --box "$HOST" --key "$work/otto"
expect "otto: apps --json" 0
[ "$(app_names "$work/out")" = demo ] && pass "otto: apps --json parses and holds demo alone" \
	|| fail "otto: apps --json: $(cat "$work/out")"
mc apps --json --box "$HOST" --key "$work/nora"
expect "nora: apps --json" 0
[ "$(app_names "$work/out")" = "" ] && jget "$work/out" 'Array.isArray(j.apps)' | grep -qx true && pass "nora: apps --json parses and holds an empty list" \
	|| fail "nora: apps --json: $(cat "$work/out")"
mc whoami --json --box "$HOST" --key "$work/nora"
[ "$(jget "$work/out" 'j.name + " " + JSON.stringify(j.roles)')" = "nora@e2e.test []" ] && pass "nora: whoami --json holds her name and no role" \
	|| fail "nora: whoami --json: $(cat "$work/out")"

echo "=== people 3: every refusal is the flat 401, byte for byte, and names no motel ==="
moteld deploy-keygen --out "$work/other.key" >/dev/null 2>&1
OTHER=$(moteld deploy-token --key "$work/other.key" --audience e2e --ttl 5m)
OTHERBOX=$(moteld deploy-token --key "$work/other.key" --audience e2e-box --ttl 5m)
P_STRANGER=$(ptok stranger) P_FRAN=$(ptok fran) P_VERA=$(ptok vera) P_OTHERBOX=$(ptok vera other.e2e.test)
for path in /demo /_motel/whoami /; do
	tag=$(printf '%s' "$path" | tr -c 'a-z' '_')
	ref=$OTHER
	[ "$path" = / ] && ref=$OTHERBOX
	get "d$tag" -H "Authorization: Bearer $ref" "$BASE$path"
	[ "$CODE" = 401 ] || fail "$path: a refused deploy token: $CODE"
	get "n$tag" "$BASE$path"
	get "s$tag" -H "Authorization: Motel $P_STRANGER" "$BASE$path"
	get "f$tag" -H "Authorization: Motel $P_FRAN" "$BASE$path"
	get "x$tag" -H "Authorization: Motel $P_FRAN" -H "X-Forwarded-For: 198.51.100.7" "$BASE$path"
	get "a$tag" -H "Authorization: Motel $P_OTHERBOX" "$BASE$path"
	same "$path: the unlisted key's answer is the refused deploy token's" "d$tag" "s$tag"
	same "$path: fran's, outside her from list, is the refused deploy token's" "d$tag" "f$tag"
	same "$path: fran's with an X-Forwarded-For naming her list is the same refusal" "d$tag" "x$tag"
	same "$path: a token for another box is the same refusal" "d$tag" "a$tag"
	same "$path: no token at all is the same refusal" "d$tag" "n$tag"
	for k in d n s f x a; do
		[ -z "$(motel_headers "$k$tag")" ] || fail "$path: an answer to a caller not accepted names motel: $(motel_headers "$k$tag")"
		no_release "$k$tag" || fail "$path: a refusal names the release in a header: $(grep -F "$RELEASE" "$work/$k$tag.h")"
	done
	pass "$path: no refusal carries a Motel- header or names the release ($(head -1 "$work/d$tag.h"), $(cat "$work/d$tag.b"))"
	get "v$tag" -H "Authorization: Motel $P_VERA" "$BASE$path"
	[ "$CODE" = 200 ] && [ -z "$(motel_headers "v$tag")" ] && no_release "v$tag" \
		&& pass "$path: vera's answer is 200 with no Motel- header and no header naming $RELEASE" \
		|| fail "$path: vera: $CODE, headers '$(motel_headers "v$tag")', release in a header: $(grep -F "$RELEASE" "$work/v$tag.h")"
done
echo "INFO: the flat 401 as sent: $(tr -d '\r' <"$work/d_demo.raw" | tr '\n' '|')$(cat "$work/d_demo.b")"

echo "=== people 4: deploy tokens as before, on the same box ==="
get st -H "Authorization: Bearer $TOKEN" "$BASE/demo"
[ "$CODE" = 200 ] && [ "$(json_str "$(cat "$work/st.b")" current_version)" = "$DV" ] \
	&& pass "a deploy token's GET /demo answers 200 with the version deployed" || fail "deploy token status: $CODE $(head -c 400 "$work/st.b")"
[ -z "$(motel_headers st)" ] && no_release st && pass "and, a token the box verified, its answer carries no Motel- header and no header naming $RELEASE" \
	|| fail "a deploy token's answer: headers '$(motel_headers st)', release in a header: $(grep -F "$RELEASE" "$work/st.h")"
get st -H "Authorization: Bearer $TOKEN" "$BASE/_motel/whoami"
[ "$CODE" = 403 ] && pass "a global deploy source's token asking who it is: 403 ($(cat "$work/st.b"))" || fail "deploy token on whoami: $CODE $(cat "$work/st.b")"
DV2=${DV}b
MAXT=120 get dep -X POST -H "Authorization: Bearer $TOKEN" -d "{\"url\":\"$ART/demo-v2.tar.gz\",\"version\":\"$DV2\"}" "$BASE/demo"
[ "$CODE" = 200 ] && pass "a deploy token deploys again, with people on the box" || fail "second deploy: $CODE $(head -c 600 "$work/dep.b")"
case "$(curl -s --max-time 5 http://localhost:8080/)" in
"hello v2"*) pass "demo serves the new release" ;;
*) fail "demo after the second deploy: $(curl -s --max-time 5 http://localhost:8080/ | head -c 200)" ;;
esac
get st -X POST -H "Authorization: Motel $P_VERA" -d "{\"url\":\"$ART/demo-v1.tar.gz\",\"version\":\"${DV}c\"}" "$BASE/demo"
[ "$CODE" = 401 ] && same "a person's token on a POST is the flat 401" d_demo st || fail "vera's POST: $CODE $(cat "$work/st.b")"

echo "=== people 5: HTTP/2, and the box's name in Host ==="
journalctl -u moteld --since "@$t_matrix" --no-pager -o cat | grep '"logger":"http.log.access' | grep -F '"uri":"/_motel/whoami"' | grep -F motelctl >"$work/access"
# The request's proto, beside client_ip (the TLS block has one too).
grep -o '"client_ip":"[^"]*","proto":"[^"]*"' "$work/access" | sed 's/.*"proto":"\([^"]*\)"/\1/' >"$work/protos"
protos=$(sort "$work/protos" | uniq -c | tr -s ' ' | tr '\n' ' ')
[ -s "$work/protos" ] && [ "$(wc -l <"$work/protos")" = "$(wc -l <"$work/access")" ] && ! grep -qvx 'HTTP/2.0' "$work/protos" \
	&& pass "motelctl spoke HTTP/2 to the box: the access log's whoami lines say $protos" \
	|| fail "motelctl's protocol in the access log: '$protos' ($(head -c 300 "$work/access"))"
get h2 --http2 -H "Authorization: Motel $P_VERA" "$BASE/_motel/whoami"
[ "$CODE" = 200 ] && [ "$PROTO" = 2 ] && pass "curl --http2 is answered over HTTP/2" || fail "curl --http2: $CODE over $PROTO"
get h2up --http2 -H "Authorization: Motel $P_VERA" -H "Host: PEOPLE.E2E.TEST:443" "$BASE/_motel/whoami"
[ "$CODE" = 200 ] && pass ":authority with the port and in capitals is the box's name (HTTP/$PROTO)" || fail ":authority PEOPLE.E2E.TEST:443: $CODE $(cat "$work/h2up.b")"
get h2other --http2 -H "Authorization: Motel $P_VERA" -H "Host: other.e2e.test" "$BASE/_motel/whoami"
if ! grep -qF vera "$work/h2other.b" && [ -z "$(motel_headers h2other)" ]; then
	pass "another :authority on the box's address and certificate is not answered as a person: Caddy's site matcher answers $CODE over HTTP/$PROTO with $(wc -c <"$work/h2other.b") bytes"
else
	fail "another :authority was answered: $CODE $(motel_headers h2other) $(head -c 300 "$work/h2other.b")"
fi
get h1other --http1.1 -H "Authorization: Motel $P_VERA" -H "Host: other.e2e.test" "$BASE/_motel/whoami"
if ! grep -qF vera "$work/h1other.b" && [ -z "$(motel_headers h1other)" ]; then
	pass "another Host over HTTP/1.1 is not answered as a person ($CODE)"
else
	fail "another Host over HTTP/1.1 was answered: $CODE $(head -c 300 "$work/h1other.b")"
fi
# The e2e config's plain-http webhook (:8081) is another site serving
# liveswap_webhook: there the Host is the check that refuses.
c=$(curl -s -o "$work/plain.b" -w '%{http_code}' --max-time 5 -H "Authorization: Motel $P_VERA" http://127.0.0.1:8081/_motel/whoami)
[ "$c" = 401 ] && cmp -s "$work/plain.b" "$work/d_demo.b" \
	&& pass "on another site with the webhook, Host 127.0.0.1:8081 is refused with the flat 401" || fail "plain :8081: $c $(cat "$work/plain.b")"
c=$(curl -s -o "$work/plain.b" -w '%{http_code}' --max-time 5 -H "Authorization: Motel $P_VERA" -H "Host: $HOST" http://127.0.0.1:8081/_motel/whoami)
echo "INFO: vera's token over plain http to :8081 with Host $HOST: $c $(head -c 200 "$work/plain.b")"

echo "=== people 6: a push that removes a person ==="
C1=$(commit "$work/f1")
if CURL_HOME=$work/curl MOTEL_TOKEN=$(boxtok "$C1") "$repo/bin/apply" "$HOST" >"$work/tout" 2>&1 \
	&& grep -qF "loaded; running apps are not restarted" "$work/tout"; then
	pass "bin/apply pushed the file without otto's lines, and the box reloaded"
else
	fail "bin/apply: $(cat "$work/tout")"
fi
cmp -s "$LIVE" "$work/f1" && pass "the installed file is the pushed one" || fail "the installed file is not the pushed one"
mc whoami --box "$HOST" --key "$work/otto"
expect "otto, removed: whoami" 4
unkeyed otto "$work/err"
unkeyed stranger "$work/stranger.0.err"
cmp -s "$work/otto.unkeyed" "$work/stranger.unkeyed" && pass "otto is told what the unlisted key is told" \
	|| fail "otto removed: $(diff "$work/otto.unkeyed" "$work/stranger.unkeyed" | head -c 400)"
get rm -H "Authorization: Motel $(ptok otto)" "$BASE/demo"
same "otto's next request is the flat 401" d_demo rm
mc status demo --box "$HOST" --key "$work/vera"
expect "vera, still declared, after the push" 0

echo "=== people 7: the first second after a restart ==="
# A person's token and a deploy token in turn on GET /demo, from before
# a restart until a second and a half after the new process first
# answers; each line of $work/loop: kind, time sent (ms), status (000:
# no connection). The person's requests alternate vera's and lena's
# tokens, so that neither name spends much of its burst
# (access.RateBurst, 60) in that second. Caddy starts its apps in no
# fixed order, so issue #473's window for deploy tokens is in some
# starts only, those whose first answer comes at about +55 ms rather
# than +140 ms: five restarts for each way in [measured, 2026-11-15,
# ten restarts each: over 443, deploy tokens refused in 3, 16 of 641
# requests, up to +81 ms after the first answer; on :8081, in 7, 57 of
# 1028; persons' tokens 0 of 644 and 0 of 1030]. Since #524 a valid
# deploy token there waits for liveswap's Start, so both kinds are held
# to the same: no 401 in the first second. The e2e config's plain-http
# webhook (:8081) answers a person whose Host names the box (people 5),
# so it is asked too.
P_VERA=$(ptok vera) P_LENA=$(ptok lena)
# in_first <kind> [<status>]: how many <kind> requests, of <status> when
# given, the new process's first second holds.
in_first() { awk -v k="$1" -v s="${2:-}" -v f="$first" '$1 == k && $2 >= f && $2 < f + 1000 && (s == "" || $3 == s) { n++ } END { print n + 0 }' "$work/loop"; }
# ask <kind> <token> <https|plain>: one GET /demo; prints its line.
ask() {
	t=$(date +%s%3N)
	if [ "$3" = https ]; then
		c=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$work/root.crt" --max-time 1 -H "Authorization: $2" "$BASE/demo")
	else
		c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 -H "Authorization: $2" -H "Host: $HOST" http://127.0.0.1:8081/demo)
	fi
	echo "$1 $t $c"
}
# restarts <https|plain>: five restarts under the loop, each said in an
# INFO line; a person's token or a deploy token refused in a first
# second fails, and so does any answer there that is not a 200: a 5xx,
# a 403 or 404, or no answer (000) is as much a box that is not serving
# a valid token. A 429 alone is let by: it is the person's own rate,
# which a faster runner could reach, and not the restart's doing. The
# sums in $p_all, $p_401, $p_not200, $d_all, $d_401.
restarts() {
	p_all=0 p_401=0 p_not200=0 d_all=0 d_401=0 r=1
	while [ "$r" -le 5 ]; do
		rm -f "$work/stop"
		(
			while [ ! -e "$work/stop" ]; do
				ask person "Motel $P_VERA" "$1"
				ask deploy "Bearer $TOKEN" "$1"
				ask person "Motel $P_LENA" "$1"
				ask deploy "Bearer $TOKEN" "$1"
			done >"$work/loop"
		) &
		loop=$!
		sleep 0.3
		t_restart=$(date +%s%3N)
		systemctl restart moteld
		t_ready=$(date +%s%3N)
		# The new process's first answer: the first after a 000 that
		# followed the restart.
		first=
		i=0
		while [ -z "$first" ] && [ "$i" -lt 200 ]; do
			first=$(awk -v t0="$t_restart" '$2 >= t0 && $3 == "000" { gap = 1; next } gap && $3 != "000" { print $2; exit }' "$work/loop")
			i=$((i + 1))
			sleep 0.1
		done
		sleep 1.5
		touch "$work/stop"
		wait "$loop"
		if [ -z "$first" ]; then
			fail "$1 restart $r: no request was answered after it, or none failed to connect: $(tail -5 "$work/loop" | tr '\n' ' ')"
		else
			codes() { awk -v k="$1" -v f="$first" '$1 == k && $2 >= f && $2 < f + 1000 { c[$3]++ } END { for (s in c) printf " %s x%d", s, c[s] }' "$work/loop"; }
			d401=$(awk -v f="$first" '$1 == "deploy" && $2 >= f && $2 < f + 1000 && $3 == "401" { if (a == "") a = $2 - f; b = $2 - f } END { if (a != "") printf ", refused from +%d to +%d ms", a, b }' "$work/loop")
			echo "INFO: $1 restart $r: systemctl restart returned after $((t_ready - t_restart)) ms, first answer at +$((first - t_restart)) ms; first second: person$(codes person); deploy$(codes deploy)$d401"
			p_all=$((p_all + $(in_first person))) p_401=$((p_401 + $(in_first person 401)))
			p_not200=$((p_not200 + $(in_first person) - $(in_first person 200)))
			d_all=$((d_all + $(in_first deploy))) d_401=$((d_401 + $(in_first deploy 401)))
			[ "$(in_first person 401)" -eq 0 ] \
				|| fail "$1 restart $r: a person's token was refused in the first second: $(awk -v f="$first" '$1 == "person" && $2 >= f && $2 < f + 1000 { printf "+%d:%s ", $2 - f, $3 }' "$work/loop")"
			[ "$(in_first deploy 401)" -eq 0 ] \
				|| fail "$1 restart $r: a valid deploy token was refused in the first second (#473)$d401"
			other=$(awk -v f="$first" '$2 >= f && $2 < f + 1000 && $3 != "200" && $3 != "401" && $3 != "429" { printf "%s+%d:%s ", $1, $2 - f, $3 }' "$work/loop")
			[ -z "$other" ] \
				|| fail "$1 restart $r: a valid token got neither a 200 nor a refusal in the first second: $other"
		fi
		r=$((r + 1))
	done
	echo "INFO: $1, five restarts' first seconds: person $p_all sent, $p_401 refused, $p_not200 not 200; deploy token $d_all sent, $d_401 refused"
	if [ "$p_all" -lt 25 ]; then
		fail "$1: only $p_all person requests in the five first seconds"
	elif [ "$p_401" -eq 0 ]; then
		pass "$1: no person's token was refused in the first second after any of five restarts ($p_all sent)"
	fi
	if [ "$d_all" -lt 25 ]; then
		fail "$1: only $d_all deploy token requests in the five first seconds"
	elif [ "$d_401" -eq 0 ]; then
		pass "$1: no valid deploy token was refused in the first second after any of five restarts ($d_all sent; #473)"
	fi
}
restarts https
restarts plain

echo "=== people 8: put the e2e config back ==="
restore && pass "restored the original config" || fail "could not load the original config"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL PEOPLE SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES PEOPLE ASSERTION(S) FAILED"
exit 1
