#!/bin/sh
# Deploys a release through motel's webhook and prints the result.
#
#   scripts/deploy.sh <artifact URL>     the box fetches it (CI: a release asset)
#   scripts/deploy.sh <app.tar.gz>       a local file: pushed in the request body
#   scripts/deploy.sh --rollback <ver>   relaunches a version still on the box's disk
#
#   MOTEL_URL          the app's webhook, e.g. https://deploy.example.com/example  (required;
#                         https:// only, since the request carries the token)
#   MOTEL_ALLOW_HTTP   1 (exactly) lets MOTEL_URL be http://, sending the
#                         token in the clear: for a local test box only
#   VERSION               the release's version: 1 to 64 letters, digits,
#                         . _ -, not starting with . (the box's alphabet).
#                         Defaults to the commit (12 hex chars), and
#                         versions are immutable on the box, so the default
#                         deploys once per commit. For a pushed file it is
#                         refused while tracked files have uncommitted
#                         changes when the script runs, rather than put the
#                         commit's name on them: set VERSION for an
#                         uncommitted build (VERSION=wip-3, say). Not used
#                         by --rollback: that names its version itself
#   MOTEL_TOKEN        a deploy token. Not needed in GitHub Actions: with
#                         `permissions: id-token: write` one is minted per run.
#   MOTEL_AUDIENCE     the audience the box's deploy_trust expects (default: motel)
#   ARTIFACT_AUTH_HEADER  sent by the box as Authorization when it fetches the
#                         URL: "token <github token>" reads a release asset by
#                         its API URL, private repo or not (the workflow sends
#                         the job's own token)
#   ARTIFACT_SHA256       the artifact's digest as `sha256sum app.tar.gz` prints
#                         it: the box refuses a download that hashes to anything
#                         else, so a host serving other bytes than this build
#                         cannot deploy them (the workflow sends it). A pushed
#                         file is the request itself and a rollback fetches
#                         nothing: neither takes a pin, and one set is refused
#
# The deploy (or rollback: the same start, health gate and cutover,
# from a release already on disk) is streamed as it happens: one JSON
# line per phase, then the outcome — the app's status when the new
# version is live, or an error saying why it was refused (the old
# version keeps serving) — with the status code the request would
# have had in `http_status`. Every line is printed as it arrives, and
# a failure exits non-zero. A 401 — the box would not accept the
# token — is sent once more 3s later (send), and so is the box's 503
# while it is starting, after the seconds its Retry-After names; a 401
# that stands is followed by what the box must
# trust for this run, since
# the box's own answer deliberately does not say (the reason is in its
# journal, for its operator).
#
# In GitHub Actions the same output is also dressed for the job page:
# the request's output in a collapsible group, a failure as an error
# annotation naming the phase and the cause, and one line in the job
# summary. A release the box refuses as built for another architecture
# is followed by the repository variable that picks the runner. Nothing
# else is sent or printed; a run from a laptop sees none of it.
set -eu

# The box's version alphabet (liveswap/names.go), checked for a deploy's
# version and a rollback's alike, so a stray character is refused here
# rather than mangling the request: in the query a `#` would drop the
# rest of it and a `%31` would arrive as `1`, so the box would take
# another version than the one printed. The box's 64-character limit is
# left to the box, whose 422 says so. printf, not echo: the version is
# the caller's, and dash's echo would read its backslashes.
valid_version() {
	case $1 in
	''|.*|*[!A-Za-z0-9._-]*) printf "deploy.sh: '%s' is not a version (letters, digits, . _ -; not starting with .)\n" "$1" >&2; return 1 ;;
	esac
}

rollback=
if [ "${1:-}" = --rollback ]; then
	rollback=${2:?--rollback needs the version to roll back to}
	shift 2
	valid_version "$rollback" || exit 1
else
	artifact=${1:?artifact URL or file, or --rollback <version>}
	shift
fi
# `deno task deploy --rollback v` / `npm run deploy -- --rollback v`
# would arrive here with app.tar.gz already in front of the flag and
# push a stale tarball; refuse anything after the one operand.
[ $# -eq 0 ] || { echo "deploy.sh: unexpected argument '$1' (--rollback goes first, without a tarball)" >&2; exit 1; }
# A pin only a URL deploy can carry. With a pushed file (the request
# itself) or a rollback (no fetch) it would be dropped, and a dropped
# pin is one the operator believes is checked; set but empty, the step
# that computes it produced nothing. Refused here, before any output
# or the token mint, like every other refusal of the invocation.
if [ -n "${ARTIFACT_SHA256+set}" ]; then
	[ -n "$ARTIFACT_SHA256" ] || { echo "deploy.sh: ARTIFACT_SHA256 is set but empty (the step that computes the digest produced nothing); unset it to deploy unpinned" >&2; exit 1; }
	if [ -n "$rollback" ] || [ -f "$artifact" ]; then
		echo "deploy.sh: ARTIFACT_SHA256 pins a URL deploy; a pushed file is the request itself and a rollback fetches nothing, so unset it" >&2
		exit 1
	fi
fi
url=${MOTEL_URL:?set MOTEL_URL to the app webhook, e.g. https://deploy.example.com/example}
# The request carries the deploy token, and from the workflow
# ARTIFACT_AUTH_HEADER (the job's GITHUB_TOKEN) too: over http both
# would cross the wire in the clear before anything in front of the
# box could redirect it, and curl guesses a plaintext scheme (http,
# mostly) for a schemeless URL. So https:// only — the scheme in any
# case, nothing before it — refused before the mint; the one way past
# is MOTEL_ALLOW_HTTP=1 exactly, for a test box. printf, not echo:
# the URL is the caller's, and dash's echo would read its backslashes.
case $url in
[Hh][Tt][Tt][Pp][Ss]://*) ;;
*)
	[ "${MOTEL_ALLOW_HTTP:-}" = 1 ] || { printf "deploy.sh: MOTEL_URL '%s' is not https://, and the request carries the deploy token; set it to the app webhook, e.g. https://deploy.example.com/example (MOTEL_ALLOW_HTTP=1 sends it anyway, for local testing only)\n" "$url" >&2; exit 1; }
	;;
esac
# The app is the URL's last path segment; the box accepts a trailing
# slash there, so drop any before taking it.
app=$url
while [ "${app%/}" != "$app" ]; do app=${app%/}; done
app=${app##*/}
if [ -z "$rollback" ]; then
	if [ -n "${VERSION:-}" ]; then
		version=$VERSION
	else
		version=$(git rev-parse --short=12 HEAD 2>/dev/null || true)
		[ -n "$version" ] || { echo "deploy.sh: not in a git checkout; set VERSION" >&2; exit 1; }
		# The default names the commit, and the box keeps that name for
		# good: uncommitted changes pushed under it would be what
		# `--rollback <commit>` relaunches, and the commit's own build
		# would then be refused as a version that already exists. So a
		# pushed file (built here) is refused the default while the
		# checkout has uncommitted changes: every tracked file in the
		# repository against HEAD, staged or not (the version names the
		# repository's commit, not the app directory's; --no-relative, so
		# a diff.relative config cannot narrow it to the directory the
		# script runs in). This looks at the checkout now, not at the
		# build: it keeps the commit's name off uncommitted changes, and
		# cannot prove the tarball was built from the commit. A URL's
		# artifact was built elsewhere, and edits here say nothing about
		# it, so it is not checked. Untracked files do not count, since
		# deploy.key or a .env may sit in the checkout. `git diff`
		# refreshes the index first, so a file that was only touched is
		# clean. Its 1 is caught, not left to set -e; any other status is
		# git failing, and its own stderr says why.
		if [ -f "$artifact" ]; then
			dirty=0
			git diff --quiet --no-relative HEAD -- || dirty=$?
			case $dirty in
			0) ;;
			1) echo "deploy.sh: tracked files have uncommitted changes, and the default version would label them commit $version; commit them, or set VERSION (e.g. VERSION=wip-3) to deploy uncommitted changes" >&2; exit 1 ;;
			*) echo "deploy.sh: git could not compare the checkout with commit $version (above); set VERSION" >&2; exit 1 ;;
			esac
		fi
	fi
	valid_version "$version" || exit 1
fi

if [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]; then
	# GitHub Actions OIDC. The token stays in this process: handing it
	# to another step as an output would print it in the job log, and it
	# can deploy until it expires.
	token=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
		"$ACTIONS_ID_TOKEN_REQUEST_URL&audience=${MOTEL_AUDIENCE:-motel}" |
		sed -n 's/.*"value" *: *"\([^"]*\)".*/\1/p')
	[ -n "$token" ] || { echo "deploy.sh: could not mint an OIDC token" >&2; exit 1; }
	printf '::add-mask::%s\n' "$token"
	minted=1
else
	token=${MOTEL_TOKEN:?set MOTEL_TOKEN (mint one with: moteld deploy-token) or run in GitHub Actions with id-token: write}
	minted=
fi

# The Actions dressing. `field` reads one string field out of the
# response (no jq here: this also runs from laptops): its last
# occurrence, with the escapes `unjson` names undone so a quote in the
# cause does not cut it short; `lead` reads it only as the body's first
# key. `unjson` undoes \" and \\, the \u003c, \u003e and \u0026 Go's
# JSON writes for <, > and &, and makes \n, \r and \t a space; an
# escaped backslash stands aside (as a newline, which sed's line holds
# no other of) while the rest are undone, so a JSON \\u003c reads \u003c.
# `failed` reads a failure body with them. `prop` and `msg` escape what a
# workflow command's property and message may not contain, and `md`
# what the job summary, Markdown that GitHub renders with its HTML,
# would take as markup. The
# commands are written with printf, never echo: dash's echo turns a
# JSON-escaped \n in a cause into a real newline and splits the line.
actions=${GITHUB_ACTIONS:-}
unjson() {
	sed 's/\\\\/\
/g; s/\\"/"/g; s/\\u003c/</g; s/\\u003e/>/g; s/\\u0026/\&/g; s/\\[nrt]/ /g; s/\n/\\/g'
}
field() {
	printf '%s' "$1" | sed -n 's/.*"'"$2"'":"\(\([^\\"]*\\.\)*[^\\"]*\)".*/\1/p' | unjson
}
lead() {
	printf '%s' "$1" | sed -n 's/^{"'"$2"'":"\(\([^\\"]*\\.\)*[^\\"]*\)".*/\1/p' | unjson
}
# failed <body> <version sent>: sets why, the body's own error (the
# box puts it first, and a filtered body's sorted keys keep it first),
# and phase, where the deploy stopped: last_deploy's, only when that
# is a deploy of the version sent. last_deploy's first key is its
# version, and no "phase" after its opening is another's. A body about
# a deploy of another version, or a failure before the first phase,
# gives no phase (#533).
failed() {
	why=$(lead "$1" error)
	phase= rest=$(printf '%s' "$1" | sed -n 's/.*"last_deploy":{//p')
	case $rest in "\"version\":\"$2\","*) phase=$(field "$rest" phase) ;; esac
}
msg() { printf '%s' "$1" | sed 's/%/%25/g' | tr '\r\n' '  '; }
prop() { msg "$1" | sed 's/:/%3A/g; s/,/%2C/g'; }
md() { printf '%s' "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g'; }
began=$(date +%s)
# One temp directory, trapped before anything is put in it.
tmpd=$(mktemp -d)
trap 'rm -rf "$tmpd"' EXIT
body=$tmpd/body
# The stream: curl prints each line as it arrives and tee keeps a
# copy (curl's own errors go to stderr, printed but kept out of the
# copy); the response headers and curl's own exit status are kept in
# files of their own — a pipeline's status is tee's. The outcome is
# the last line's http_status — a stream is 200 from its first byte,
# so the status line says nothing. A body with no such line and no
# phase line did not stream: a box without stream support answered
# the single response, and when curl brought it whole and its status
# line is the 200 a completed deploy answers, that is the outcome: a
# 3xx from an intermediary is not a deploy, and neither is any other
# 2xx (a 202 from a queue in front of the box, say). A phase line
# with no terminal line is a stream cut short, and a single response
# curl could not finish is no outcome: failures both.
hdrs=$tmpd/headers
rcfile=$tmpd/curl-status
stream() { # <curl args...>: runs the request, prints it, keeps it
	# `|| rc=$?` keeps a failing curl from ending the group under
	# set -e before its status is written.
	{
		rc=0
		# --max-time: the deploy's worst case with the box's defaults and
		# a pre_start, plus the artifact's transfer. The pre_start's
		# deadline (5m), the health gate's deadline (5m) and its overrun
		# of one health_interval and two health_timeouts (9s), drain (5s)
		# and grace (10s) come to 10m24s; 900 leaves 4m36s for the
		# transfer. Raise it with any of those settings. Giving up before
		# the cutover fails the deploy with the old version still
		# serving; a running pre_start is not stopped but runs to its
		# end, and a retry gets 409 until then (stopping its .prestart
		# unit on the box ends it sooner). Giving up after the cutover
		# reports a deploy the box completed as failed.
		curl --fail-with-body --silent --show-error --no-buffer --max-time 900 \
			-H "Authorization: Bearer $token" -H "Accept: application/x-ndjson" \
			-D "$hdrs" "$@" || rc=$?
		echo "$rc" >"$rcfile"
	} | tee "$body"
}
httpstatus() { sed -n 's/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$hdrs" 2>/dev/null | tail -n 1; }
# The box's own 401, by its sentence: a 401 from something in front of
# it (a wrong MOTEL_URL, say) is not about deploy_trust.
boxrefused() {
	[ "$(httpstatus)" = 401 ] || return 1
	case $(field "$(tail -n 1 "$body")" error) in 'invalid or missing deploy token'*) return 0 ;; esac
	return 1
}
# boxstarting prints the seconds to wait when the answer is the box's
# own 503 while it is starting, by its sentence and its Retry-After in
# seconds (held to 1 to 10), and fails for any other answer: a 503
# from something in front of the box, or one that names no wait, is
# not this.
boxstarting() {
	[ "$(httpstatus)" = 503 ] || return 1
	case $(field "$(tail -n 1 "$body")" error) in 'motel is starting'*) ;; *) return 1 ;; esac
	secs=$(tr 'A-Z' 'a-z' <"$hdrs" | sed -n 's/^retry-after: *\([0-9][0-9]*\).*/\1/p' | tail -n 1)
	[ -n "$secs" ] || return 1
	case $secs in ?|??) ;; *) secs=10 ;; esac
	[ "$secs" -ge 1 ] || secs=1
	[ "$secs" -le 10 ] || secs=10
	echo "$secs"
}
# send is stream, sent once more 3s after the box refuses the token.
# The first token under an issuer's new signing key is refused while
# the box fetches that key, without waiting on the fetch (a wait would
# tell anyone with a token which apps exist), and the retry finds it.
# That holds for a MOTEL_TOKEN too when it is an issuer's (a GitLab
# id_token, say). The box refuses before it does anything else, so
# sending the request again repeats nothing. A box that has just
# started answers a token it accepts 503 when it has not loaded the app
# within 5s; nothing ran on the box for that answer either, so the
# request is sent once more after the Retry-After it names. Either way
# the second answer is the run's.
send() { # <curl args...>
	stream "$@"
	if boxrefused; then
		printf '\nthe box refused the token; sending it once more in 3s (a box refuses the first token under a new signing key while it fetches the key)\n'
		sleep 3
		stream "$@"
	elif secs=$(boxstarting); then
		printf '\nthe box is starting; sending the request once more in %ss, as it asks\n' "$secs"
		sleep "$secs"
		stream "$@"
	fi
}
outcome() { # the http_status of a complete last line; else 200 for a whole single 200; else 0
	# The whole terminal suffix, brace included: a connection cut after
	# the digits must not read as an outcome.
	code=$(tail -n 1 "$body" | sed -n 's/.*,"event":"done","http_status":\([0-9][0-9]*\)}$/\1/p')
	if [ -n "$code" ]; then echo "$code"
	elif grep -q '"event":"phase"' "$body"; then echo 0
	elif [ "$(cat "$rcfile" 2>/dev/null)" != 0 ]; then echo 0
	elif [ "$(httpstatus)" = 200 ]; then echo 200
	else echo 0
	fi
}
# refused says what the box must trust for a token it turned away.
# The box's 401 is the same flat sentence whatever the reason — an
# answer naming the check that failed would tell anyone who can mint a
# token (every GitHub repository can) which apps exist and what each
# pins — so this side is built from what the run already has, and the
# reason itself is in the box's journal. Most often, not always: the
# journal also holds the case where the box could not reach the
# issuer to check the token at all.
refused() {
	echo "the box refused this token. Most often its deploy_trust for this app does not accept:"
	if [ -n "$minted" ]; then
		printf '  %-19s%s\n' 'audience' "${MOTEL_AUDIENCE:-motel}" 'claim repository' "${GITHUB_REPOSITORY:-?}" 'claim ref' "${GITHUB_REF:-?}" 'claim workflow_ref' "${GITHUB_WORKFLOW_REF:-?}"
	else
		echo "  the key this token was minted with (deploy_trust local { public_key ... })"
		echo "  the audience, subject or claim it pins, against what moteld deploy-token was given"
	fi
	echo "Or the URL's app name is not one the box knows: an unknown app answers the same 401."
	echo "Or the box could not consult the token's issuer (an outage there), or had fetched its keys in the"
	echo "minute before the issuer changed them: its journal says which, and when a re-run goes through."
	echo "Check the app's deploy_trust block in the box's Caddyfile. The box's journal"
	echo "(journalctl -u moteld, 'webhook auth failed') names the app asked for and the"
	echo "check that refused it, unless the box's budget for logging failed authentications is spent."
}
finish() { # <what> <version sent>: dresses the outcome, exits on failure
	what=$1
	echo
	[ -n "$actions" ] && printf '::endgroup::\n'
	took="$(( $(date +%s) - began ))s"
	code=$(outcome)
	case $code in
	2??)
		[ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '**motel:** %s live, %s\n\n' "$what" "$took" >>"$GITHUB_STEP_SUMMARY"
		return 0 ;;
	esac
	failed "$(tail -n 1 "$body")" "$2"
	hint=
	! boxrefused || hint=$(refused)
	[ -z "$hint" ] || printf '%s\n' "$hint"
	# The box's pre-flight names the runner a release for its
	# architecture is built on (liveswap/elf.go); the workflow's build
	# job takes its runner from a repository variable, which the box
	# cannot know of.
	if [ -n "$actions" ] && [ "$code" = 422 ]; then
		runner=$(printf '%s' "$why" | sed -n 's/.*(runs-on: \([A-Za-z0-9._-]*\)).*/\1/p')
		[ -z "$runner" ] || echo "The build job's runner is the repository variable MOTEL_RUNS_ON: gh variable set MOTEL_RUNS_ON -R ${GITHUB_REPOSITORY:-<owner/repo>} --body $runner (or Settings → Secrets and variables → Actions → Variables), then build again: push, or Re-run all jobs (a re-run of the deploy job alone sends this build again)."
	fi
	[ -n "$actions" ] && printf '::error title=%s::%s\n' "$(prop "motel: $what failed")" "$(msg "${phase:+in $phase: }${why:-see the response above}${hint:+; the log says what the box must trust}")"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		printf '**motel:** %s failed%s, %s\n\n%s\n\n' "$what" "${phase:+ in \`$phase\`}" "$took" "$(md "${why:-see the log}")" >>"$GITHUB_STEP_SUMMARY"
		# Tildes: a git ref or repository name cannot hold one, so the
		# run's own values cannot end the fence early.
		[ -z "$hint" ] || printf '~~~\n%s\n~~~\n\n' "$hint" >>"$GITHUB_STEP_SUMMARY"
	fi
	exit 1
}

if [ -n "$rollback" ]; then
	what="$app rollback to $rollback"
	printf '%srolling %s back to %s\n' "${actions:+::group::}" "$url" "$rollback"
	send -X POST "$url?rollback=$rollback"
	finish "$what" "$rollback"
	exit 0
fi

# Never print a URL's query string: that is where presigned-URL
# credentials live (the box redacts it from its logs for the same
# reason), and Actions output is readable by anyone who can see the job.
what="$app $version"
printf '%sdeploying %s as %s to %s\n' "${actions:+::group::}" "${artifact%%\?*}" "$version" "$url"
if [ -f "$artifact" ]; then
	send -X POST -H "Content-Type: application/gzip" --data-binary @"$artifact" "$url?version=$version"
else
	# JSON-escape what goes into the body (a quote or backslash in a
	# header value would otherwise make it malformed).
	json() { printf '%s' "$1" | sed 's/[\\"]/\\&/g'; }
	auth=${ARTIFACT_AUTH_HEADER:+,\"auth_header\":\"$(json "$ARTIFACT_AUTH_HEADER")\"}
	sha=${ARTIFACT_SHA256:+,\"sha256\":\"$(json "$ARTIFACT_SHA256")\"}
	send -X POST -H "Content-Type: application/json" \
		-d "{\"url\":\"$(json "$artifact")\",\"version\":\"$(json "$version")\"$auth$sha}" "$url"
fi
finish "$what" "$version"
