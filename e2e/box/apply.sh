#!/bin/sh
# The box channel end to end (box/DESIGN-box.md), inside the e2e box:
# the e2e-motel container, systemd as PID 1, motel.service as the
# motel user, and the packaged motel-box-apply.path and .service
# with the tmpfiles exchange tree. Commits are made and SSH-signed by
# git here, bundled the way the template's workflow bundles them, and
# POSTed to box_webhook with a `deploy_trust local` token naming the
# commit; the result is polled as the workflow polls it. Every answer
# is the real handler's and every install the real applier's — nothing
# is stubbed.
#
# The box's file is the e2e config itself, made into one a box applies:
# imports inlined (a signed file has none), a `box` block, and a
# box_webhook site on a bare hostname (served with an internal-CA
# certificate on 8443: the address rules allow no scheme or port), at
# <host>/Caddyfile in the repository. Its baseline is set by `motel
# init`, run inside that directory as the operator runs it.
#
# Runs last in `make e2e` (`docker compose exec`); puts the e2e config
# back at the end.
set -u
. /lib.sh

HOST=box.e2e.test
URL=https://$HOST:8443/
BOX=/var/lib/motel-box
LIVE=/etc/motel/Caddyfile
FILE=$HOST/Caddyfile

work=$(mktemp -d)
repo=$work/repo
trap 'rm -rf "$work"' EXIT
cp "$LIVE" "$work/original"

# ── the client: what the workflow does, in shell ───────────────────

newid() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }

# tok [<sha>]: a fresh box token, naming the commit when given one.
tok() {
	if [ -n "${1:-}" ]; then
		motel deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m --claims "sha=$1"
	else
		motel deploy-token --key /shared/deploy.key --audience e2e-box --ttl 5m
	fi
}

# req <curl args...>: one request to the box's webhook; status in
# $CODE, body in $work/resp.
req() {
	CODE=$(curl -sk --http1.1 --resolve "$HOST:8443:127.0.0.1" -o "$work/resp" -w '%{http_code}' --max-time 60 "$@")
}
resp() { cat "$work/resp"; }

# bundle <sha> <baseline> [<path>]: the bundle for <sha> in
# $work/bundle.tgz — path, Caddyfile, commit, the first-parent chain
# down to (not including) <baseline> as parents/NNNN, and the trees
# along <path>. Regular files only, named one by one, so tar writes no
# directory entries. TAMPER=1 sends other Caddyfile bytes than the
# commit's.
bundle() {
	sha=$1 base=$2 path=${3:-$FILE}
	b=$work/b
	rm -rf "$b" && mkdir -p "$b/parents" "$b/trees"
	printf '%s' "$path" >"$b/path"
	git -C "$repo" cat-file commit "$sha" >"$b/commit"
	git -C "$repo" cat-file blob "$sha:$path" >"$b/Caddyfile"
	[ "${TAMPER:-}" = 1 ] && printf '\n# not what was signed\n' >>"$b/Caddyfile"
	members="path Caddyfile commit"
	n=0
	for p in $(git -C "$repo" rev-list --first-parent "$base..$sha" | tail -n +2); do
		n=$((n + 1))
		f=parents/$(printf %04d "$n")
		git -C "$repo" cat-file commit "$p" >"$b/$f"
		members="$members $f"
	done
	t=$(git -C "$repo" rev-parse "$sha^{tree}")
	git -C "$repo" cat-file tree "$t" >"$b/trees/$t"
	members="$members trees/$t"
	dir=
	rest=$path
	while [ "${rest#*/}" != "$rest" ]; do
		dir=${dir:+$dir/}${rest%%/*}
		rest=${rest#*/}
		t=$(git -C "$repo" rev-parse "$sha:$dir")
		git -C "$repo" cat-file tree "$t" >"$b/trees/$t"
		members="$members trees/$t"
	done
	# shellcheck disable=SC2086 # one member per word
	tar -czf "$work/bundle.tgz" -C "$b" $members
}

# post <claim> [<id>]: POST the bundle with a token naming <claim>
# (empty: no sha claim); the id in $ID.
post() {
	ID=${2:-$(newid)}
	req -X POST -H "Authorization: Bearer $(tok "$1")" -H "X-Box-Request-Id: $ID" \
		-H 'Content-Type: application/gzip' --data-binary "@$work/bundle.tgz" "$URL"
}

phase() { json_str "$(resp)" phase; }

# poll: GET /?result=$ID with a fresh token each time until the phase
# is terminal; 404 is "not yet" after a POST that was not answered
# terminally (Message catalogue). Bounded at 120 s, the e2e stand-in
# for the workflow's thirty minutes.
poll() {
	i=0
	while [ "$i" -lt 120 ]; do
		req -H "Authorization: Bearer $(tok)" "$URL?result=$ID"
		case "$CODE:$(phase)" in
		202:* | 404:*) ;;
		*) return ;;
		esac
		i=$((i + 1))
		sleep 1
	done
}

# push <sha> [<path>]: bundle against the baseline, POST, poll to a
# terminal phase. Final status in $CODE, phase in $PHASE.
push() {
	bundle "$1" "$BASE" "${2:-$FILE}"
	post "$1"
	case $CODE in 202 | 504) poll ;; esac
	PHASE=$(phase)
}

# settled: the applier has finished its run and left nothing behind
# (I2, the empty in/ and work/; no record; no temporary beside the
# live file).
settled() {
	i=0
	while [ "$i" -lt 60 ]; do
		[ "$(systemctl is-active motel-box-apply.service)" != activating ] \
			&& [ -z "$(ls -A "$BOX/in")$(ls -A "$BOX/work")" ] && break
		i=$((i + 1))
		sleep 0.5
	done
	if [ -n "$(ls -A "$BOX/in")$(ls -A "$BOX/work")" ]; then
		fail "$1: left in/ or work/ non-empty: $(ls -A "$BOX/in" "$BOX/work" | tr '\n' ' ')"
	elif [ -e "$BOX/txn.json" ]; then
		fail "$1: left txn.json behind"
	elif ls /etc/motel/.Caddyfile.box-* >/dev/null 2>&1; then
		fail "$1: left a .Caddyfile.box-* temporary"
	fi
}

baseline() { json_str "$(cat "$BOX/applied.json")" sha; }
served() { curl -s --max-time 5 "http://localhost:$1/"; }
status() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://localhost:$1/"; }

# refused <name> <text>: the push was answered 422 refused, carrying
# <text>; nothing on the box moved.
refused() {
	if [ "$CODE" = 422 ] && [ "$PHASE" = refused ] && resp | grep -qF -- "$2"; then
		pass "$1: refused by name"
	else
		fail "$1: expected 422 refused with '$2', got $CODE $(resp | head -c 600)"
	fi
	cmp -s "$LIVE" "$work/live" && pass "$1: the live file is unchanged" || fail "$1: the live file changed"
	[ "$(baseline)" = "$BASE" ] && pass "$1: the baseline did not move" || fail "$1: the baseline is $(baseline), not $BASE"
	settled "$1"
}

# ── the repository: signer keys, the box's file, signed commits ────

for k in alice bob mallory; do ssh-keygen -q -t ed25519 -N '' -C "$k" -f "$work/$k"; done
signer() { printf '\t\tsigner %s@e2e.test %s\n' "$1" "$(cut -d' ' -f1,2 "$work/$1.pub")"; }

# The e2e config as a box's file: imports inlined, HTTPS for the one
# bare-hostname site (internal CA, port 8443, no redirect listener),
# the box block, and the webhook site last. `# signers` is where
# render puts the signer lines.
awk '
/^[ \t]*import [^ \t]+[ \t]*$/ { f = $2; while ((getline l < f) > 0) print l; close(f); next }
/^[ \t]*auto_https off[ \t]*$/ {
	print "\tlocal_certs"
	print "\tskip_install_trust"
	print "\tauto_https disable_redirects"
	print "\thttps_port 8443"
	print ""
	print "\tbox {"
	print "\t\tdeploy_trust local {"
	print "\t\t\tpublic_key /shared/deploy.pub"
	print "\t\t\taudience e2e-box"
	print "\t\t}"
	print "\t\t# signers"
	print "\t}"
	next
}
{ print }
END { print ""; print "'"$HOST"' {"; print "\tbox_webhook"; print "}" }
' "$work/original" >"$work/template"

# render <signer lines> [<extra text>]: the box's file.
render() {
	S=$1 awk '{ print } $0 == "\t\t# signers" && ENVIRON["S"] != "" { print ENVIRON["S"] }' "$work/template"
	printf '%s' "${2:-}"
}

git init -q -b main "$repo"
git -C "$repo" config user.name e2e
git -C "$repo" config user.email e2e@e2e.test
git -C "$repo" config gpg.format ssh

# commit <alice|bob|mallory|none> <file> [<path>]: commit <file> at
# <path> on top of $AT (detached), signed by the key named; prints the
# commit's id.
commit() {
	path=${3:-$FILE}
	git -C "$repo" checkout -q --detach "$AT" 2>/dev/null || true
	mkdir -p "$repo/$(dirname "$path")"
	cp "$2" "$repo/$path"
	git -C "$repo" add -A
	if [ "$1" = none ]; then
		git -C "$repo" commit -q --allow-empty --no-gpg-sign -m "unsigned"
	else
		git -C "$repo" -c user.signingkey="$work/$1" commit -q --allow-empty -S -m "by $1"
	fi
	git -C "$repo" rev-parse HEAD
}

ALICE=$(signer alice)
BOB=$(signer bob)

# The root commit: the box's file before any push.
render "$ALICE" >"$work/f0"
mkdir -p "$repo/$HOST" && cp "$work/f0" "$repo/$FILE"
git -C "$repo" add -A
git -C "$repo" -c user.signingkey="$work/alice" commit -q -S -m "$HOST: the first file"
C0=$(git -C "$repo" rev-parse HEAD)

# install_baseline <sha>: `motel init <sha>`, as root inside the
# box's directory copied from the commit (git archive, as init's help
# copies it), on a box where motel runs; its output in $work/init.out.
install_baseline() {
	rm -rf "$work/init" && mkdir -p "$work/init" \
		&& git -C "$repo" archive "$1" "$HOST" | tar -xf - -C "$work/init" \
		&& (cd "$work/init/$HOST" && motel init "$1") >"$work/init.out" 2>&1
}

# restore: the e2e config back, and no baseline.
restore() {
	cp "$work/original" "$LIVE.e2e-tmp" && chmod 0644 "$LIVE.e2e-tmp" && mv "$LIVE.e2e-tmp" "$LIVE" \
		&& rm -f "$BOX/applied.json" \
		&& { systemctl reload motel || systemctl restart motel; }
}

# ready: GET / answers 200 (the internal certificate is issued
# asynchronously after the reload that first names the host).
ready() {
	i=0
	while [ "$i" -lt 120 ]; do
		req -H "Authorization: Bearer $(tok)" "$URL"
		[ "$CODE" = 200 ] && return 0
		i=$((i + 1))
		sleep 0.5
	done
	return 1
}

echo "=== box apply 0: the units are in place and the box answers ==="
[ "$(systemctl is-active motel-box-apply.path)" = active ] \
	&& pass "motel-box-apply.path is active" || fail "the path unit is $(systemctl is-active motel-box-apply.path)"
[ "$(stat -c '%a %U %G' "$BOX" "$BOX/in" "$BOX/out" | tr '\n' ' ')" = "2750 root motel 770 root motel 2750 root motel " ] \
	&& pass "the exchange tree has the Paths table's modes" || fail "exchange tree: $(stat -c '%n %a %U %G' "$BOX" "$BOX/in" "$BOX/out" | tr '\n' ' ')"
if ! install_baseline "$C0"; then
	fail "motel init failed: $(cat "$work/init.out"); $(journalctl -u motel --no-pager -n 5 | grep -o '"msg":"[^"]*"' | tail -1)"
	restore
	exit 1
fi
grep -qx "init: reloaded. From here, this box takes config only as signed commits sent by your repository's workflow." "$work/init.out" \
	&& pass "motel init installed the baseline and reloaded" || fail "motel init printed: $(cat "$work/init.out")"
[ "$(json_str "$(cat "$BOX/applied.json")" path)" = "$FILE" ] && [ "$(json_str "$(cat "$BOX/applied.json")" signer)" = init ] \
	&& pass "applied.json records $FILE, derived from the host, signed by init" || fail "applied.json: $(cat "$BOX/applied.json")"
BASE=$C0
if ready; then
	pass "GET / answers once the baseline is installed"
else
	fail "GET / never answered 200: $CODE $(resp | head -c 300)"
fi
[ "$(json_str "$(resp)" commit)" = "$C0" ] && pass "GET / names the baseline commit" || fail "GET /: $(resp)"
[ "$(json_str "$(resp)" sha256)" = "$(sha256sum "$LIVE" | cut -d' ' -f1)" ] \
	&& pass "GET / carries the installed file's digest" || fail "GET / digest: $(resp)"
req "$URL"
[ "$CODE" = 401 ] && pass "GET / without a token is 401" || fail "GET / without a token: $CODE"

echo "=== box apply 1: a signed descendant is verified, applied, and polled to the end ==="
AT=$C0
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v1"\n}\n')" >"$work/f1"
C1=$(commit alice "$work/f1")
before=$(status 8080)
bundle "$C1" "$BASE"
post "$C1"
C1_ID=$ID
# Step 8: the POST answers by the first phase the handler sees —
# `verified` (202) unless the applier was already done within its
# first poll interval (200 applied).
case "$CODE:$(phase)" in
202:verified | 200:applied) pass "the POST answers $CODE $(phase)" ;;
*) fail "the POST answered $CODE $(resp | head -c 300)" ;;
esac
[ "$CODE" = 202 ] && poll
PHASE=$(phase)
[ "$PHASE" = applied ] && pass "the push ends applied" || fail "push of a signed descendant: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is the pushed one" || fail "the live file differs from the pushed one"
[ "$(served 8180)" = v1 ] && pass "the pushed config is running" || fail "the pushed site does not answer: '$(served 8180)'"
[ "$(baseline)" = "$C1" ] && pass "the baseline advanced to the pushed commit" || fail "the baseline is $(baseline)"
for app in demo deno-example node-example; do
	resp | grep -q "\"$app\"" || fail "the result does not list app $app: $(resp | head -c 300)"
done
resp | grep -q '"caddyfile_edited_out_of_band":false' && pass "the result lists the apps, no out-of-band edit" || fail "result: $(resp | head -c 300)"
[ "$(status 8080)" = "$before" ] && pass "the demo app answers as it did before the apply ($before)" || fail "the demo app answered $before, now $(status 8080)"
settled "apply"
BASE=$C1
cp "$LIVE" "$work/live"

echo "=== box apply 2: the same commit again is no_change, answered at once ==="
bundle "$C1" "$C1"
post "$C1"
[ "$CODE" = 200 ] && [ "$(phase)" = no_change ] && pass "a repeat is 200 no_change" || fail "repeat: $CODE $(resp)"
req -H "Authorization: Bearer $(tok)" "$URL?result=$ID"
[ "$CODE" = 404 ] && pass "the fast path writes nothing: its id polls 404" || fail "poll after the fast path: $CODE $(resp)"

echo "=== box apply 3: the handler's refusals ==="
# An id the box holds a result for is a retry after a lost response:
# told to poll, whatever its bundle.
post "$C1" "$C1_ID"
[ "$CODE" = 409 ] && resp | grep -qF "duplicate request id" && pass "a known id is 409 duplicate" || fail "known id: $CODE $(resp)"
post "$C1" "not-an-id"
[ "$CODE" = 400 ] && pass "a malformed request id is 400" || fail "bad id: $CODE $(resp)"
req -X POST -H "X-Box-Request-Id: $(newid)" -H 'Content-Type: application/gzip' --data-binary "@$work/bundle.tgz" "$URL"
[ "$CODE" = 401 ] && pass "a push without a token is 401" || fail "no token: $CODE $(resp)"

AT=$C1
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v2"\n}\n')" >"$work/f2"
C2=$(commit alice "$work/f2")
bundle "$C2" "$BASE"
post "$C1"
PHASE=$(phase)
refused "a token naming another commit" "the token names commit $C1; the bundle is $C2"
post ""
PHASE=$(phase)
refused "a token with no sha claim" "box tokens must name the commit (sha claim)"
TAMPER=1 bundle "$C2" "$BASE"
post "$C2"
PHASE=$(phase)
refused "a file other than the committed one" "the file sent is not $FILE in $C2"

U=$(commit none "$work/f2")
push "$U"
refused "an unsigned commit" "$U is not signed"

M=$(commit mallory "$work/f2")
push "$M"
refused "a commit by an unlisted key" "$M is signed by a key that is not a signer"

AT=$U
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v3"\n}\n')" >"$work/f3"
S=$(commit alice "$work/f3")
push "$S"
refused "an unsigned commit under a signed one" "$U, between the commit this box runs and $S, is not signed"

push "$C0"
refused "a replay of an older commit" "$C0 does not descend from the commit this box runs ($C1)"

AT=$C0
SIB=$(commit alice "$work/f2")
push "$SIB"
refused "a commit off another branch" "$SIB does not descend from the commit this box runs ($C1)"

AT=$C1
C2B=$(commit alice "$work/f2" box2/Caddyfile)
push "$C2B" box2/Caddyfile
refused "another box's file" "this box's file is $FILE; the bundle is box2/Caddyfile"

sed "s/^$HOST {/other.e2e.test {/" "$work/f2" >"$work/fhost"
H=$(commit alice "$work/fhost")
push "$H"
refused "a file for another host" "this file is for other.e2e.test; this box is $HOST"

render "" >"$work/fnosigner"
N=$(commit alice "$work/fnosigner")
push "$N"
refused "a file with no signer" "the new Caddyfile has no signer"

render "$BOB" >"$work/fbob"
D=$(commit alice "$work/fbob")
push "$D"
refused "a file that drops the key that signed it" "drops the key that signed this commit (alice@e2e.test)"

grep -v '^[[:space:]]*box_webhook$' "$work/f2" >"$work/fnohook"
W=$(commit alice "$work/fnohook")
push "$W"
refused "a file with no box_webhook" "the new Caddyfile has no site with box_webhook"

render "$ALICE" "$(printf '\n:8181 {\n\tno_such_directive\n}\n')" >"$work/fbad"
X=$(commit alice "$work/fbad")
push "$X"
refused "a config motel validate rejects" "motel validate:"

# A backup declaration is validated as every line is: one that names a
# sibling's shared dir is refused at step 6. The refusal quotes Caddy's
# error line within the 300-byte bound, which ends before the path:
# Caddy's line leads with each module it was provisioning, in an order
# that differs from run to run (its servers are a map). So the reason
# is checked on the file itself.
sed 's#files  uploads#files  ../../deno-example/shared#' "$work/f2" >"$work/fbackup"
cmp -s "$work/f2" "$work/fbackup" && fail "the box's file has no \`files  uploads\` line to break"
# Caddy logs the error as JSON off a terminal, its quotes escaped.
motel validate --adapter caddyfile --config "$work/fbackup" 2>&1 | grep -qE 'backup files \\?"\.\./\.\./deno-example/shared\\?": the path reaches outside' \
	&& pass "fixture: motel validate refuses the file for the backup path" || fail "fixture: motel validate does not refuse the file for the backup path"
BK=$(commit alice "$work/fbackup")
push "$BK"
refused "a backup path outside the app's shared dir" "motel validate:"
# A root from the environment is one a backup run, which reads the
# Caddyfile without motel's environment, cannot know: motel
# accepts it, and step 6's motel-backup validate refuses it, naming
# the variable. (Box apply 9b makes the same change to the box's file on
# a box where motel-backup is not installed.)
# shellcheck disable=SC2016 # the Caddyfile's own {$NAME}, not the shell's
sed 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' "$work/f2" >"$work/fenvroot"
grep -qF '{$LIVESWAP_ROOT:' "$work/fenvroot" || fail "the box's file has no root line to make a variable"
ER=$(commit alice "$work/fenvroot")
push "$ER"
refused "a root from the environment, for backups" "motel-backup validate: motel-backup: the liveswap root or a backup path depends on the environment variable(s) LIVESWAP_ROOT"

# The door: this box's server listens on 8443, and the push arrived
# there; a file that moves it validates and would load, and is refused
# by the compare with the loaded server's addresses.
sed 's/^\thttps_port 8443$/\thttps_port 9443/' "$work/f2" >"$work/fmoved"
grep -q 'https_port 9443' "$work/fmoved" || fail "the moved file was not made"
MV=$(commit alice "$work/fmoved")
push "$MV"
refused "a file that moves the box's server" "this push changes where the box's server listens (its listen addresses differ from the loaded config's): a push cannot move the address it arrives on; make that change at the console"

# The request limits: a read_header timeout under the floor on the box
# server validates and would load, and is refused by name before any
# route (the usual hardening values are the unit tests' to prove).
sed 's/^\t\ttrusted_proxies static private_ranges$/&\n\t\ttimeouts {\n\t\t\tread_header 1s\n\t\t}/' "$work/f2" >"$work/fshort"
grep -q 'read_header 1s' "$work/fshort" || fail "the short-timeout file was not made"
SH=$(commit alice "$work/fshort")
push "$SH"
refused "a file with a read_header timeout under the floor" "the new Caddyfile does not reach box_webhook on /: the server's read_header_timeout is under 5s, the floor for a push's handshake and headers"

# request_body ahead of box_webhook (Caddy's default order puts it
# there): a max_size under the bundle's cap is refused by name.
sed 's/^\tbox_webhook$/\trequest_body {\n\t\tmax_size 1MB\n\t}\n\tbox_webhook/' "$work/f2" >"$work/fcapped"
grep -q 'max_size 1MB' "$work/fcapped" || fail "the capped file was not made"
CP=$(commit alice "$work/fcapped")
push "$CP"
refused "a file that caps the body under the bundle's" "the new Caddyfile does not reach box_webhook on /: request_body's max_size caps the body under 16 MiB first"

# timeouts ahead of box_webhook (the directive, which the default
# order puts there too): a read_timeout under the floor is refused by
# name.
sed 's/^\tbox_webhook$/\ttimeouts {\n\t\tread_timeout 1s\n\t}\n\tbox_webhook/' "$work/f2" >"$work/fcut"
grep -q 'read_timeout 1s' "$work/fcut" || fail "the cut file was not made"
CT=$(commit alice "$work/fcut")
push "$CT"
refused "a file whose timeouts would cut an upload" "the new Caddyfile does not reach box_webhook on /: timeouts's read_timeout cuts an upload that stalls under 10s first"

echo "=== box apply 3b: a trickled upload gives the push slot back at the handler's bound ==="
# Caddy's idle reader re-arms the connection's read deadline before
# every body read (read_body_idle, a minute by default), so a body that
# keeps arriving is never idle, and the deadline the handler set bounds
# nothing past the first byte. The handler's own bound on the body ends
# the push at 120 s: the read in flight then returns with the next byte
# (within 20 s here) and the slot is given back; the answer follows
# once the server has given up draining the rest, at the idle deadline
# that read armed when it began, before the bound — so by 180 s (Caps,
# "bundle upload"). The client: openssl's s_client, sending a byte
# every 20 s of a body it declared as 1000 bytes, 15 bytes in all, under
# a 400 s timeout: before the bound, the slot was held until the bytes
# stopped, and the answer then never left — the idle deadline's error
# made Caddy's reader clear the deadline, and net/http's drain of the
# rest before the answer had none (measured: hours, until the client
# was killed). The probe: a push whose body is no bundle, 409 while the
# slot is held and 422 once it is free, nothing written either way.
probe() {
	req -X POST -H "Authorization: Bearer $(tok)" -H "X-Box-Request-Id: $(newid)" -H 'Content-Type: application/gzip' --data-binary 'not a bundle' "$URL"
}
trickle_start=$(date +%s)
{
	printf 'POST / HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Box-Request-Id: %s\r\nContent-Type: application/gzip\r\nContent-Length: 1000\r\n\r\n' "$HOST" "$(tok)" "$(newid)"
	i=0
	while [ "$i" -lt 15 ]; do
		sleep 20
		printf x
		i=$((i + 1))
	done
} 2>/dev/null | timeout 400 openssl s_client -quiet -connect 127.0.0.1:8443 -servername "$HOST" >"$work/trickle" 2>&1 &
trickle_pid=$!
sleep 100
probe
[ "$CODE" = 409 ] && resp | grep -qF "a push is being admitted" && pass "100 s in, the trickled upload holds the slot: another push is 409" || fail "probe at 100 s: $CODE $(resp)"
wait "$trickle_pid"
trickle_took=$(($(date +%s) - trickle_start))
grep -q '^HTTP/1.1 400' "$work/trickle" && grep -qF 'could not read the bundle: the upload outlasted 2m0s' "$work/trickle" \
	&& pass "the trickled upload is answered 400 at the bound" || fail "trickle: $(tail -c 400 "$work/trickle")"
[ "$trickle_took" -ge 120 ] && [ "$trickle_took" -le 195 ] \
	&& pass "answered after ${trickle_took}s: the bound and at most one idle window" || fail "answered after ${trickle_took}s"
journalctl -u motel --since "@$trickle_start" --no-pager -o cat | grep -q 'box push upload failed' \
	&& pass "the journal has the upload failed line" || fail "no 'box push upload failed' line in the journal"
probe
[ "$CODE" = 422 ] && pass "the slot is free: the next push is read and refused" || fail "probe after the bound: $CODE $(resp)"
settled "trickle"

echo "=== box apply 4: a config that validates but does not load is rolled back ==="
# Validation binds no listener; loading does, and 192.0.2.1 (TEST-NET-1)
# is on no interface here.
render "$ALICE" "$(printf '\n:8180 {\n\trespond "v1"\n}\n\nhttp://192.0.2.1:8182 {\n\tbind 192.0.2.1\n\trespond "unreachable"\n}\n')" >"$work/funload"
R=$(commit alice "$work/funload")
push "$R"
[ "$PHASE" = rolled_back ] && resp | grep -qF "the previous Caddyfile is back and running" \
	&& pass "a failed reload is rolled_back" || fail "unloadable: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/live" && pass "the live file is the running config again" || fail "the live file is not the previous one"
[ "$(served 8180)" = v1 ] && pass "the previous config kept serving" || fail "after the rollback: '$(served 8180)'"
[ "$(baseline)" = "$C1" ] && pass "the baseline did not move" || fail "the baseline is $(baseline)"
settled "rollback"

echo "=== box apply 5: a console edit is reported, and the next push reinstalls the commit ==="
# A console edit can import what a push cannot. Here the demo app's
# backup block comes from a snippet defined outside /etc/motel, which
# a backup run's view does not hold, so that app would not be backed up
# while every run said ok: motel reload refuses it, and the running
# config goes on. A start is never refused for it — every site would
# be down — and says why.
mkdir -p /srv/outside
printf '(bk) {\n\tbackup {\n\t\tsqlite demo.db\n\t\tfiles  uploads\n\t}\n}\n' >/srv/outside/bk.caddy
chmod 0644 /srv/outside/bk.caddy
{ echo "import /srv/outside/bk.caddy"; awk '/^			backup \{$/ { print "			import bk"; skip = 1; next } skip && /^			\}$/ { skip = 0; next } !skip' "$work/f1"; } >"$work/foutside"
grep -q "import bk" "$work/foutside" || fail "the box's file has no backup block to move"
cat "$work/foutside" >"$LIVE"
since=$(date +%s)
systemctl reload motel >"$work/out" 2>&1 && fail "systemctl reload took a backup declared outside /etc/motel" || pass "a reload of it is refused"
journalctl -u motel --since "@$since" --no-pager -o cat | grep -qF "read from /srv/outside/bk.caddy, outside /etc/motel" \
	&& pass "and the journal says why" || fail "the journal does not say why the reload was refused"
[ "$(served 8180)" = v1 ] && pass "and the running config goes on serving" || fail "after the refused reload: '$(served 8180)'"
# (Meanwhile the box's GET / answers 500: its walk refuses the import in
# the file it runs. So the start is watched on the demo site, and the
# box is asked again once the file is mended.)
systemctl restart motel
i=0
until [ "$(served 8180)" = v1 ] || [ "$i" -ge 60 ]; do i=$((i + 1)); sleep 0.5; done
[ "$(served 8180)" = v1 ] && pass "a start with it serves all the same" || fail "after a start: '$(served 8180)'"
journalctl -u motel --since "@$since" --no-pager -o cat | grep -q "served all the same" \
	&& pass "and says why it is wrong, in the journal" || fail "the start said nothing of the backup"
cat "$work/f1" >"$LIVE"
systemctl reload motel && pass "with the backup back in the file, a reload is taken" || fail "the reload after mending it"
ready && pass "and the box answers again" || fail "the box does not answer after the mend: $CODE $(resp | head -c 300)"
rm -rf /srv/outside
printf '\n# edited at the console\n' >>"$LIVE"
systemctl reload motel
bundle "$C1" "$C1"
post "$C1"
case $CODE in 202 | 504) poll ;; esac
PHASE=$(phase)
[ "$PHASE" = applied ] && resp | grep -q '"caddyfile_edited_out_of_band":true' \
	&& pass "a replay after a console edit applies, reporting the edit" || fail "replay after an edit: $CODE $(resp | head -c 600)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is the commit's again" || fail "the console edit survived"
settled "out-of-band"

echo "=== box apply 6: with motel stopped, the applier refuses and changes nothing ==="
# Nothing can POST with motel down, so the bundle is dropped as the
# handler drops it: assembled in stage/ by the motel uid, renamed
# into in/.
AT=$C1
C3=$(commit alice "$work/f2")
bundle "$C3" "$C1"
systemctl stop motel
ID=$(newid)
install -o motel -g motel -m 0644 "$work/bundle.tgz" "$BOX/stage/.e2e-$ID"
mv "$BOX/stage/.e2e-$ID" "$BOX/in/$ID.tar"
i=0
while [ ! -s "$BOX/out/$ID.json" ] && [ "$i" -lt 60 ]; do
	i=$((i + 1))
	sleep 0.5
done
[ "$(json_str "$(cat "$BOX/out/$ID.json" 2>/dev/null)" phase)" = refused ] && grep -qF "motel is not running; nothing applied" "$BOX/out/$ID.json" \
	&& pass "the applier refuses while motel is stopped" || fail "with motel stopped: $(cat "$BOX/out/$ID.json" 2>/dev/null)"
cmp -s "$LIVE" "$work/f1" && pass "the live file is unchanged" || fail "the live file changed with motel stopped"
settled "stopped"
systemctl start motel
ready && pass "motel is back" || fail "motel did not come back: $CODE"

echo "=== box apply 7: the template's bin/apply, unchanged, against this box ==="
# examples/box/bin/apply is what a box repository's workflow runs. It
# posts to the address the committed file names, https://$HOST/ on
# 443; curl's own config file routes that to 8443 and takes the
# internal CA's certificate (the script has no test hooks). The
# laptop's token: one, naming the commit, via MOTEL_TOKEN.
mkdir -p "$repo/bin" "$work/curl"
cp /template/bin/apply "$repo/bin/apply"
printf 'connect-to = %s:443:127.0.0.1:8443\ninsecure\nhttp1.1\n' "$HOST" >"$work/curl/.curlrc"
template_apply() { # <sha> → its output in $work/tout, exit status returned
	CURL_HOME=$work/curl MOTEL_TOKEN=$(tok "$1") "$repo/bin/apply" "$HOST" >"$work/tout" 2>&1
}
AT=$C1
C4=$(commit alice "$work/f2")
if template_apply "$C4" && grep -qF "loaded; running apps are not restarted" "$work/tout"; then
	pass "bin/apply applied a signed commit"
else
	fail "bin/apply: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$C4" ] && pass "the baseline is bin/apply's commit" || fail "the baseline is $(baseline), not $C4"
cmp -s "$LIVE" "$work/f2" && pass "the live file is the commit's" || fail "the live file is not the commit's"
settled "template apply"
template_apply "$C4" && grep -qF "the box already runs this Caddyfile" "$work/tout" \
	&& pass "bin/apply again: no_change" || fail "bin/apply again: $(cat "$work/tout")"
# An unsigned commit under a signed one: refused, naming the commit to
# rebase out — the real handler's words, unmasked and unescaped.
AT=$C4
C5=$(commit none "$work/f1")
AT=$C5
C6=$(commit alice "$work/f2")
if template_apply "$C6"; then
	fail "bin/apply exited 0 for an unsigned commit under a signed one"
elif grep -qF "$C5, between the commit this box runs and $C6, is not signed" "$work/tout"; then
	pass "bin/apply reports the unsigned commit by name"
else
	fail "bin/apply on an unsigned commit: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$C4" ] && pass "the baseline did not move" || fail "the baseline is $(baseline)"
settled "template refused"

echo "=== box apply 8: secrets, set with the template's own commands and sent by its bin/apply ==="
# The laptop's side is the template's Makefile and bin/secrets, run in
# the repository as an operator runs them (age from Debian's package);
# the box's is the real handler and applier. The app is demo, whose
# block moves from its hand-made env file to the one the box writes.
cp /template/Makefile "$repo/Makefile"
cp /template/bin/secrets "$repo/bin/secrets"
ENV=/etc/motel/secrets/demo.env
KEY=$(cat /etc/motel/age.pub 2>/dev/null)
grep -qxF "init: secrets for this box encrypt to $KEY; motel box key prints it again" "$work/init.out" \
	&& pass "motel init made the box's key and printed it" || fail "init's key line, against age.pub '$KEY': $(cat "$work/init.out")"
[ "$(motel box key 2>/dev/null)" = "$KEY" ] && pass "motel box key prints that key alone" || fail "motel box key: $(motel box key 2>&1)"
[ "$(stat -c '%a %U %G' /etc/motel/age /etc/motel/age/identity /etc/motel/age.pub /etc/motel/secrets | tr '\n' ' ')" = "700 root root 600 root root 644 root root 2750 root motel " ] \
	&& pass "the key and the env files' directory have the Paths table's modes" \
	|| fail "modes: $(stat -c '%n %a %U %G' /etc/motel/age /etc/motel/age/identity /etc/motel/age.pub /etc/motel/secrets | tr '\n' ' ')"
ready
[ "$(json_str "$(resp)" recipient)" = "$KEY" ] && pass "GET / names the box's key" || fail "GET /: $(resp)"

# commit_index <signer> <message>: what the work tree holds, committed
# on top of HEAD, signed; prints the commit's id.
commit_index() {
	git -C "$repo" add -A
	git -C "$repo" -c user.signingkey="$work/$1" commit -q -S -m "$2"
	git -C "$repo" rev-parse HEAD
}
# secret <var> <value> [<make args>]: make secret for demo, the value
# piped in as from a password manager; its output in $work/sout.
secret() {
	v=$1 val=$2
	shift 2
	(cd "$repo" && printf '%s' "$val" | make -s secret APP=demo VAR="$v" "$@") >"$work/sout" 2>&1
}
# reloads_since <epoch>: how many times motel.service was reloaded.
reloads_since() { journalctl -u motel.service --since "@$1" --no-pager -o cat | grep -c '^Reloading' || true; }

git -C "$repo" checkout -q --detach "$C4"
V1=e2e-value-from-the-repository-1
if secret SECRET "$V1" BOX_KEY="$KEY" && grep -qF "env_file /etc/motel/secrets/demo.env" "$work/sout"; then
	pass "make secret encrypted the value and named the env_file line to add"
else
	fail "make secret: $(cat "$work/sout")"
fi
sed -i 's#env_file /etc/motel/demo.env#env_file /etc/motel/secrets/demo.env#' "$repo/$FILE"
S1=$(commit_index alice "demo: SECRET from the repository")
t0=$(date +%s)
if template_apply "$S1" && grep -qxF "apply: $HOST secrets: demo: added SECRET" "$work/tout" \
	&& grep -qF "at $(git -C "$repo" rev-parse --short=7 "$S1"): loaded; running apps are not restarted" "$work/tout" \
	&& grep -qxF "apply: demo reads its secrets at its next launch; a deploy of a new version launches it now" "$work/tout"; then
	pass "bin/apply sent the secret and named it"
else
	fail "bin/apply with a secret: $(cat "$work/tout")"
fi
[ "$(stat -c '%a %U %G' "$ENV" 2>/dev/null)" = "640 root motel" ] && pass "the env file is 0640 root:motel" || fail "the env file: $(stat -c '%a %U %G' "$ENV" 2>&1)"
grep -qxF "SECRET=\"$V1\"" "$ENV" && pass "the env file holds the value, as motel reads it" || fail "the env file holds: $(cat "$ENV" 2>&1)"
[ "$(reloads_since "$t0")" -ge 1 ] && pass "a push that changed the Caddyfile reloaded motel" || fail "no reload in the journal: the reload check below proves nothing"
[ "$(baseline)" = "$S1" ] && pass "the baseline is the commit with the secret" || fail "the baseline is $(baseline), not $S1"
settled "secret added"

# A launch reads it: the crash release prints SECRET and exits, and the
# deploy's answer names the variable it redacted, never the value. The
# hand-made demo.env sets SECRET too, so it is moved aside for this
# deploy: a launch that still read it would fail, not pass.
mv /etc/motel/demo.env /etc/motel/demo.env.aside
code=$(curl -s -o "$work/deploy" -w '%{http_code}' --max-time 60 -X POST -H "Authorization: Bearer $(cat /shared/deploy.token)" \
	-d '{"url":"http://e2e-artifacts:8080/artifacts/demo-crash.tar.gz","version":"box-secrets-crash"}' http://localhost:8081/demo)
mv /etc/motel/demo.env.aside /etc/motel/demo.env
if grep -q '"redacted_env":\["SECRET"\]' "$work/deploy" && ! grep -qF "$V1" "$work/deploy"; then
	pass "a launch read the box's env file: the release printed SECRET, and its answer redacted it"
else
	fail "deploy with the box's env file: $code $(head -c 600 "$work/deploy")"
fi

V2=e2e-value-from-the-repository-2
secret SECRET "$V2" || fail "make secret, again: $(cat "$work/sout")"
S2=$(commit_index alice "demo: rotate SECRET")
t0=$(date +%s)
if template_apply "$S2" && grep -qxF "apply: $HOST secrets: demo: changed SECRET" "$work/tout" \
	&& grep -qF ": secrets updated; the Caddyfile is unchanged, nothing reloaded" "$work/tout"; then
	pass "a rotated value is sent alone"
else
	fail "a rotation: $(cat "$work/tout")"
fi
grep -qxF "SECRET=\"$V2\"" "$ENV" && pass "the env file holds the new value" || fail "after the rotation: $(cat "$ENV" 2>&1)"
[ "$(reloads_since "$t0")" = 0 ] && pass "a push of secrets alone did not reload motel" || fail "motel was reloaded for secrets alone"
settled "secret rotated"
template_apply "$S2" && grep -qF "the box already runs this Caddyfile" "$work/tout" \
	&& pass "the same commit again is no_change" || fail "a replay: $(cat "$work/tout")"

# An edit at the console: the replay of HEAD writes the commit's
# value back and says so.
printf 'SECRET="edited at the console"\n' >"$ENV"
if template_apply "$S2" && grep -qF "demo's env file on the box had been edited there since the last apply" "$work/tout" \
	&& grep -qxF "SECRET=\"$V2\"" "$ENV"; then
	pass "a replay after a console edit wrote the value back, and said so"
else
	fail "after a console edit: $(cat "$work/tout"); the file holds $(cat "$ENV" 2>&1)"
fi
settled "env file edited"

# A push that does not load is rolled back, the env file with it.
secret SECRET e2e-value-never-installed || fail "make secret: $(cat "$work/sout")"
cp "$repo/$FILE" "$work/fgood"
printf '\nhttp://192.0.2.1:8182 {\n\tbind 192.0.2.1\n\trespond "unreachable"\n}\n' >>"$repo/$FILE"
R2=$(commit_index alice "a secret, and a file that does not load")
if ! template_apply "$R2" && grep -qF "rolled_back" "$work/tout" && grep -qxF "SECRET=\"$V2\"" "$ENV"; then
	pass "a rolled-back push put the env file back too"
else
	fail "a rollback with a secret: $(cat "$work/tout"); the file holds $(cat "$ENV" 2>&1)"
fi
[ "$(baseline)" = "$S2" ] && pass "the baseline did not move" || fail "the baseline is $(baseline), not $S2"
settled "rollback with secrets"
cp "$work/fgood" "$repo/$FILE"

# A value encrypted to another key: the box cannot open it.
age-keygen -o "$work/other.key" 2>/dev/null
printf 'not for this box' | age -r "$(age-keygen -y "$work/other.key")" -o "$repo/$HOST/secrets/demo/OTHER.age"
N2=$(commit_index alice "a value encrypted to another key")
if ! template_apply "$N2" && grep -qF "/secrets/demo/OTHER.age is not encrypted to this box's key ($KEY)" "$work/tout"; then
	pass "a value not encrypted to the box is refused by name"
else
	fail "a value for another key: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$S2" ] && grep -qxF "SECRET=\"$V2\"" "$ENV" && pass "nothing moved" || fail "the baseline is $(baseline); the file holds $(cat "$ENV" 2>&1)"
settled "not for this box"
rm "$repo/$HOST/secrets/demo/OTHER.age"

# Secrets for an app the file does not declare: refused at the door.
mkdir -p "$repo/$HOST/secrets/ghost"
printf 'x' | age -R "$repo/$HOST/secrets/recipients" -o "$repo/$HOST/secrets/ghost/X.age"
G=$(commit_index alice "secrets for no app")
if ! template_apply "$G" && grep -qF "/secrets/ghost/ names no app in the new Caddyfile" "$work/tout"; then
	pass "secrets for an undeclared app are refused (422)"
else
	fail "secrets for no app: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$S2" ] && pass "the baseline did not move" || fail "the baseline is $(baseline), not $S2"
settled "undeclared app"
rm -r "$repo/$HOST/secrets/ghost"

# A rebuilt box has a new key: bin/apply stops before sending, naming
# it, and a reader's make rekey moves every value to it.
mv /etc/motel/age/identity "$work/identity.old"
KEY2=$(motel box key 2>/dev/null)
[ -n "$KEY2" ] && [ "$KEY2" != "$KEY" ] && [ "$(cat /etc/motel/age.pub)" = "$KEY2" ] \
	&& pass "a key moved aside is made again, and age.pub with it" || fail "the new key '$KEY2', age.pub $(cat /etc/motel/age.pub)"
K=$(commit_index alice "values for the old key")
if ! template_apply "$K" && grep -qF "the box's key is $KEY2, but the first recipient in $HOST/secrets/recipients is $KEY" "$work/tout" \
	&& grep -qF "make rekey BOX_KEY=$KEY2" "$work/tout" && grep -qF "nothing was sent" "$work/tout"; then
	pass "bin/apply stops at a box key the recipients file does not name, and names it"
else
	fail "a changed box key: $(cat "$work/tout")"
fi
if (cd "$repo" && make -s rekey BOX_KEY="$KEY2" IDENTITY="$work/alice") >"$work/rout" 2>&1; then
	pass "make rekey encrypted every value to the new key"
else
	fail "make rekey: $(cat "$work/rout")"
fi
K2=$(commit_index alice "rekeyed to the box's new key")
if template_apply "$K2" && grep -qxF "apply: $HOST secrets: demo: changed SECRET" "$work/tout" \
	&& grep -qxF "SECRET=\"e2e-value-never-installed\"" "$ENV"; then
	pass "after make rekey the box opens the values again"
else
	fail "after the rekey: $(cat "$work/tout"); the file holds $(cat "$ENV" 2>&1)"
fi
settled "rekey"

# A hand-made env file brought in: the values the box writes are the
# ones motel read from it.
if (cd "$repo" && make -s secrets-import APP=demo </etc/motel/demo.env) >"$work/iout" 2>&1; then
	pass "make secrets-import read the hand-made env file"
else
	fail "make secrets-import: $(cat "$work/iout")"
fi
I=$(commit_index alice "demo: the hand-made env file, brought in")
template_apply "$I" || fail "bin/apply of the import: $(cat "$work/tout")"
[ "$(grep -v '^#' "$ENV" | sed 's/^SECRET="\(.*\)"$/SECRET=\1/')" = "$(cat /etc/motel/demo.env)" ] \
	&& pass "the env file the box wrote holds the hand-made file's values" || fail "imported: $(cat "$ENV" 2>&1)"
settled "import"

# Removed from the repository, demo's file goes; a file the box never
# wrote stays.
printf 'X=1\n' >/etc/motel/secrets/node-example.env
git -C "$repo" rm -r -q "$HOST/secrets/demo"
sed -i 's#env_file /etc/motel/secrets/demo.env#env_file /etc/motel/demo.env#' "$repo/$FILE"
RM=$(commit_index alice "demo: back to its hand-made env file")
if template_apply "$RM" && grep -qxF "apply: $HOST secrets: demo: removed SECRET" "$work/tout" && [ ! -e "$ENV" ] \
	&& [ -e /etc/motel/secrets/node-example.env ]; then
	pass "a removed app's env file is gone, and one the box never wrote is left alone"
else
	fail "removal: $(cat "$work/tout"); $(ls -la /etc/motel/secrets 2>&1)"
fi
rm -f /etc/motel/secrets/node-example.env
settled "secrets removed"

echo "=== box apply 8b: the box's own secrets, a notify channel's URL ==="
# secrets/_box/ into /etc/motel/secrets/_box.env, which notify reads at a
# config load: validate is told the name the push carries (the value is
# not on the box yet), and a push of it alone reloads motel. No
# Alertmanager on this box: notify says so and serves.
BOXENV=/etc/motel/secrets/_box.env
notify_secret() { # <var> <value>: make secret APP=_box; output in $work/sout
	(cd "$repo" && printf '%s' "$2" | make -s secret APP=_box VAR="$1") >"$work/sout" 2>&1
}
cp "$repo/$FILE" "$work/fnonotify"
notify_secret NOTIFY_OPS_URL https://ntfy.sh/e2e-box-ops-1 || fail "make secret APP=_box: $(cat "$work/sout")"
grep -qF "monitor block reads NOTIFY_OPS_URL yet" "$work/sout" \
	&& pass "make secret APP=_box says no notify line reads it yet" || fail "make secret APP=_box: $(cat "$work/sout")"
awk '{ print } !done && /^\{/ { print "\tmonitor {\n\t\tchannel ops ntfy {secret.NOTIFY_OPS_URL}\n\t\toperator ops\n\t}"; done = 1 }' \
	"$work/fnonotify" >"$repo/$FILE"
N1=$(commit_index alice "notify: the operator's channel, its URL in secrets/_box/")
t0=$(date +%s)
if template_apply "$N1" && grep -qxF "apply: $HOST secrets: _box: added NOTIFY_OPS_URL" "$work/tout" \
	&& ! grep -qF "_box reads its secrets" "$work/tout"; then
	pass "a monitor block and its secret in secrets/_box/ pass validate and apply"
else
	fail "a push of the box's own secret: $(cat "$work/tout")"
fi
[ "$(stat -c '%a %U %G' "$BOXENV" 2>/dev/null)" = "640 root motel" ] && grep -qxF 'NOTIFY_OPS_URL="https://ntfy.sh/e2e-box-ops-1"' "$BOXENV" \
	&& pass "_box.env is 0640 root:motel, holding the value" || fail "_box.env: $(stat -c '%a %U %G' "$BOXENV" 2>&1) $(cat "$BOXENV" 2>&1)"
journalctl -u motel --since "@$t0" --no-pager -o cat | grep -q 'Alertmanager is not set up on this box' \
	&& pass "the reload loaded the monitor block" || fail "no monitor line after the reload"
settled "box secret added"
# A channel line whose secret the push does not carry: refused at the
# door, since a boot would drop the channel (notify's "Secrets at load").
cp "$repo/$FILE" "$work/fnotify"
sed -i 's#^\(\t*\)operator ops$#\1channel pager slack {secret.NOTIFY_PAGER_URL}\n\1operator ops pager#' "$repo/$FILE"
NP=$(commit_index alice "notify: a channel whose secret is not in secrets/_box/")
if ! template_apply "$NP" && grep -qF "the monitor block reads secret NOTIFY_PAGER_URL, and $HOST/secrets/_box/ does not hold it: make secret APP=_box VAR=NOTIFY_PAGER_URL" "$work/tout"; then
	pass "a monitor block naming a secret the push does not carry is refused, by name"
else
	fail "an uncarried notify secret: $(cat "$work/tout")"
fi
[ "$(baseline)" = "$N1" ] && pass "the baseline did not move" || fail "the baseline is $(baseline), not $N1"
settled "uncarried notify secret"
cp "$work/fnotify" "$repo/$FILE"
notify_secret NOTIFY_OPS_URL https://ntfy.sh/e2e-box-ops-2 || fail "make secret APP=_box, again: $(cat "$work/sout")"
N2=$(commit_index alice "notify: rotate the operator's channel")
t0=$(date +%s)
if template_apply "$N2" && grep -qxF "apply: $HOST secrets: _box: changed NOTIFY_OPS_URL" "$work/tout" \
	&& grep -qF ": the box's notify secrets updated and loaded; the Caddyfile is unchanged" "$work/tout"; then
	pass "a rotated notify URL is sent alone"
else
	fail "a notify rotation: $(cat "$work/tout")"
fi
[ "$(reloads_since "$t0")" -ge 1 ] && grep -qxF 'NOTIFY_OPS_URL="https://ntfy.sh/e2e-box-ops-2"' "$BOXENV" \
	&& pass "the box's own secrets alone reloaded motel" || fail "after the rotation: $(reloads_since "$t0") reloads; $(cat "$BOXENV" 2>&1)"
settled "box secret rotated"
git -C "$repo" rm -r -q "$HOST/secrets/_box"
cp "$work/fnonotify" "$repo/$FILE"
N3=$(commit_index alice "notify: gone")
if template_apply "$N3" && grep -qxF "apply: $HOST secrets: _box: removed NOTIFY_OPS_URL" "$work/tout" && [ ! -e "$BOXENV" ]; then
	pass "the box's own secrets removed with the monitor block: _box.env is gone"
else
	fail "removal of the box's own secrets: $(cat "$work/tout"); $(ls -la /etc/motel/secrets 2>&1)"
fi
settled "box secrets removed"
# The box's first key back, and age.pub from it.
mv "$work/identity.old" /etc/motel/age/identity && [ "$(motel box key 2>/dev/null)" = "$KEY" ] \
	&& pass "the box's first key is back" || fail "could not put the box's first key back"

echo "=== box apply 9: an administrator who is not root reads the logs, and nothing secret ==="
# As docs/hardening.md makes one: the adm group alone (e2e/Dockerfile).
# journalctl exits 0 on an empty journal too: look for known lines.
runuser -u admin -- journalctl -u motel --no-pager 2>&1 | grep -q '"liveswap started"' \
	&& pass "adm reads motel's journal without privilege" || fail "admin cannot read motel's journal"
runuser -u admin -- journalctl -t motel-demo --no-pager 2>&1 | grep -q 'motel-demo' \
	&& pass "adm reads an app's journal" || fail "admin cannot read the app's journal"
for f in /etc/motel/demo.env /etc/motel/age/identity; do
	runuser -u admin -- cat "$f" >/dev/null 2>&1 && fail "admin can read $f" || pass "admin cannot read $f"
done
for d in /etc/motel/secrets /etc/motel/age /var/lib/motel; do
	runuser -u admin -- ls "$d" >/dev/null 2>&1 && fail "admin can list $d" || pass "admin cannot list $d"
done
# The template's own Caddyfile, with a signer (it ships with none, and
# a box refuses one without), is a box's file this build takes.
ssh-keygen -q -t ed25519 -N "" -C e2e -f "$work/example-signer"
ekey=$(cut -d' ' -f1,2 "$work/example-signer.pub")
awk -v line="signer e2e@example.com $ekey" '{ print } /^[[:space:]]*# signers[[:space:]]*$/ { print "\t\t" line }' \
	/template/deploy.example.com/Caddyfile >"$work/example"
runuser -u admin -- motel validate --adapter caddyfile --config /dev/stdin <"$work/example" >"$work/out" 2>&1 \
	&& pass "the template's Caddyfile is a valid config" || fail "the template's Caddyfile does not validate: $(tail -3 "$work/out")"
if runuser -u admin -- motel box describe /dev/stdin <"$work/example" >"$work/out" 2>&1 \
	&& [ "$(cat "$work/out")" = "$(printf 'host deploy.example.com\napp example\nsigner e2e@example.com %s' "$ekey")" ]; then
	pass "the box reads the template as the deploy.example.com box, one app, one signer"
else
	fail "motel box describe on the template: $(cat "$work/out")"
fi

echo "=== box apply 9b: where motel-backup is not installed, step 6 does not ask it ==="
# Box apply 3's root from the environment, which motel-backup
# validate refused: with no motel-backup on the box, the same change
# is applied (access(X_OK)'s "not installed").
# shellcheck disable=SC2016 # the Caddyfile's own {$NAME}, not the shell's
sed -i 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' "$repo/$FILE"
grep -qF '{$LIVESWAP_ROOT:' "$repo/$FILE" || fail "the box's file has no root line to make a variable"
# A root from the environment is refused only where an app declares a
# backup: with none, this would apply whatever step 6 asked. So the
# file is first held to what step 6 would ask, with the binary there.
/usr/bin/motel-backup validate "$repo/$FILE" >"$work/out" 2>&1 \
	&& fail "fixture: motel-backup validate takes the file, so 9b would prove nothing" \
	|| { grep -q 'LIVESWAP_ROOT' "$work/out" && pass "fixture: motel-backup validate refuses the file, naming the variable" || fail "fixture: motel-backup validate refused the file for another reason: $(cat "$work/out")"; }
NB=$(commit_index alice "root from the environment, on a box without backups")
mv /usr/bin/motel-backup /usr/bin/motel-backup.aside
template_apply "$NB"
applied=$?
mv /usr/bin/motel-backup.aside /usr/bin/motel-backup
[ "$applied" = 0 ] && [ "$(baseline)" = "$NB" ] \
	&& pass "with no motel-backup on the box, the push is applied" || fail "push without motel-backup: $(cat "$work/tout")"
settled "no backups"

echo "=== box apply 10: the path unit never spun ==="
[ "$(systemctl is-active motel-box-apply.path)" = active ] \
	&& pass "the path unit is still active" || fail "the path unit is $(systemctl is-active motel-box-apply.path)"
if journalctl -u motel-box-apply.path -u motel-box-apply.service --no-pager | grep -qiE 'trigger limit|start request repeated|start-limit'; then
	fail "the journal has a trigger or start limit line"
else
	pass "no trigger or start limit was hit"
fi

echo "=== box apply 11: put the e2e config back ==="
restore && pass "restored the original config" || fail "could not load the original config"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL BOX APPLY SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES BOX APPLY ASSERTION(S) FAILED"
exit 1
