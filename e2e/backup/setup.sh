#!/bin/sh
# The setup suite. Runs inside an e2e-backup-box of its own as root,
# against real systemd, Debian's restic and the S3 server at e2e-s3. It
# holds `motel-backup setup <repository>` to its word: everything
# knowable is refused before a secret is asked for; the secrets are
# typed at a terminal, the secret ones with echo off; a new repository's
# password is shown, and confirmed stored, before it takes effect
# anywhere; a mistake over a working setup leaves the working file byte
# for byte; and the account restic runs as is made here, by setup — the
# image does not make it.
#
# Until the command exists it exits 2 with the usage text, so no check
# below is content with a non-zero exit alone.
. /lib.sh
. /lib-backup.sh
. /tty.sh

S3=s3:http://e2e-s3:9000
REPO=$S3/setuprepo
KEYID=AKIDE2EFIXTURE
SECRET=e2e-fixture-key-not-a-secret
ETC=$(dirname "$ENVFILE")
OLD=/etc/motel/backup.env

wait_for_systemd
cp "$CADDYFILE" /root/Caddyfile.base
seed

# at_tty, converse and the prompts: /tty.sh.
setup() { motel-backup setup "$@" >"$OUT" 2>&1; }
says() { grep -q -e "$1" "$OUT"; }
sum() { sha256sum "$ENVFILE" 2>/dev/null | cut -d' ' -f1; }
temps() { ls -A "$ETC" 2>/dev/null | grep -c '^\.\{0,1\}repository\.env\.staged'; }
shown_password() { grep -o 'Repository password (new): [a-z2-7]\{52\}' "$1" | cut -d' ' -f4; }
as_nobody() { setpriv --reuid nobody --regid nogroup --clear-groups "$@"; }
until_units() { # <n> <pattern>: waits up to a minute for that many units of the pattern to be running
	i=0
	while [ "$(systemctl list-units --plain --no-legend --state=active,activating "$2" | wc -l)" != "$1" ] && [ "$i" -lt 600 ]; do
		i=$((i + 1))
		sleep 0.1
	done
}

echo "=== setup 0: the usage text ==="
setup && fail "setup with no repository exited 0" || { says "^usage: motel-backup setup \[--existing\] <repository>" && pass "setup with no repository is the usage text" || fail "said: $(cat "$OUT")"; }
setup a b && fail "setup with two arguments exited 0" || { says "^usage:" && pass "and so is setup with two" || fail "said: $(cat "$OUT")"; }
setup --existing && fail "setup --existing with no repository exited 0" || { says "^usage:" && pass "and so is --existing alone" || fail "said: $(cat "$OUT")"; }
setup --exists "$REPO" && fail "setup with a flag it does not have exited 0" || { says "^usage:" && pass "and so is a flag setup does not have" || fail "said: $(cat "$OUT")"; }

echo "=== setup 1: not root, no terminal: refused before anything is touched ==="
as_nobody motel-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup as nobody exited 0" || { says "needs root: run the same command again as root" && ! says "$REPO" && pass "setup as nobody says to repeat the command as root, and renders none of it" || fail "as nobody: $(cat "$OUT")"; }
setup "$REPO" </dev/null && fail "setup with no terminal exited 0" || { says "asks for secrets at a terminal, and there is none here; from a provisioning tool, write $ENVFILE by hand" && ! says "^usage" && ! says "no such device" && pass "setup with no terminal says so, and where to go" || fail "no terminal: $(cat "$OUT")"; }
[ ! -e "$ETC" ] && pass "and nothing was made under /etc" || fail "$ETC exists: $(ls -la "$ETC")"
[ "$(units_left)" = 0 ] && pass "and no unit was started" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"

echo "=== setup 2: what is knowable is refused before any prompt, and before the account is made ==="
id motel-backup >/dev/null 2>&1 && fail "fixture: the motel-backup account exists before setup ran, so nothing below can show setup making it" || pass "fixture: no motel-backup account before setup"
mv /usr/bin/restic /usr/bin/restic.aside
at_tty motel-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup without restic exited 0" || { says "restic, the backup program motel-backup runs, is not installed at /usr/bin/restic: apt install restic" && pass "without restic, setup says what restic is and names the package" || fail "without restic: $(cat "$OUT")"; }
says "Storage key id" && fail "a prompt was asked before the preflight passed" || pass "and asked nothing"
id motel-backup >/dev/null 2>&1 && fail "the account was made before the preflight passed" || pass "and made no account"
mv /usr/bin/restic.aside /usr/bin/restic
# An account of that name that motel did not make: every restic
# unit runs as it, so motel uses only one it made itself, marked as
# its own, and refuses any other by name — before any prompt, leaving
# it as it is (the owner, 2026-09-27). Made here as an administrator
# would, with the same line and no mark: a nologin shell and no home do
# not make it motel's.
useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin motel-backup
at_tty motel-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup with an account motel did not make exited 0" || { says "the motel-backup account on this box was not made by motel" && says "remove it (userdel motel-backup)" && says "useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --comment made-by-motel motel-backup" && pass "an account motel did not make is refused, naming the remedy" || fail "the account not made by motel: $(cat "$OUT")"; }
says "Storage key id" && fail "a prompt was asked with an account motel did not make" || pass "and asked nothing"
[ -z "$(getent passwd motel-backup | cut -d: -f5)" ] && pass "and the account was left as it was" || fail "the account was changed: $(getent passwd motel-backup)"
# The same rule, asked by itself: what the package's postinstall asks,
# and an administrator after mending the account. It needs no root.
as_nobody motel-backup account >"$OUT" 2>&1 && fail "account exited 0 of an account motel did not make" || { says "was not made by motel" && pass "motel-backup account says the same, as nobody" || fail "account: $(cat "$OUT")"; }
userdel motel-backup 2>/dev/null
[ ! -e "$ETC" ] && pass "and nothing was made under /etc" || fail "$ETC exists"
for r in sftp:user@host:/srv/backups /srv/backups local:/srv/backups rclone:remote:bucket azure:container:path gs:bucket:path swift:container:/path rest:https://host:8000/ "s3:http://user:pass@e2e-s3:9000/box" "s3:user:pass@e2e-s3:9000/box" "s3://user:pass@e2e-s3:9000/box" "ftp://host/x" "s3:"; do
	if at_tty motel-backup setup "$r" >"$OUT" 2>&1; then
		fail "setup $r exited 0"
	elif says "Storage key id"; then
		fail "setup $r asked before refusing"
	elif says "^usage"; then
		fail "setup $r: the usage text"
	else
		pass "refused before any prompt: $r — $(grep motel-backup: "$OUT" | head -1 | cut -c1-90)"
	fi
done
[ ! -e /srv/backups ] && pass "nothing was made at the refused path" || fail "/srv/backups exists"
[ ! -e "$ETC" ] && pass "and nothing under /etc" || fail "$ETC exists"
# b2: in every form restic reads, refused naming the same repository
# at B2's S3-compatible endpoint: restic's own B2 backend retries a
# failed upload without end (#177).
for r in b2:hs-backups:dev b2:hs-backups b2:; do
	if at_tty motel-backup setup "$r" >"$OUT" 2>&1; then
		fail "setup $r exited 0"
	elif says "Storage key id"; then
		fail "setup $r asked before refusing"
	elif ! says "b2: is not supported: B2 is reached at its S3-compatible endpoint, s3:https://s3.<region>.backblazeb2.com/"; then
		fail "setup $r: $(cat "$OUT")"
	else
		pass "refused before any prompt: $r — $(grep -o 's3:https://[^,]*' "$OUT" | head -1)"
	fi
done
says "s3:https://s3.<region>.backblazeb2.com/<bucket>, with an application key, not the master key" && pass "b2: with no bucket names the bucket as a placeholder" || fail "b2: with no bucket: $(cat "$OUT")"
id motel-backup >/dev/null 2>&1 && fail "setup b2: made the account" || pass "and made no account"
[ ! -e "$ETC" ] && pass "and nothing under /etc" || fail "$ETC exists"

echo "=== setup 3: a fresh setup at a terminal ==="
t0=$(date +%s)
converse "motel-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" = 0 ] && says "repository ready: $REPO (new, id [0-9a-f]\{8\})" && pass "setup made the repository (${took}s)" || fail "setup: exit $rc after ${took}s: $(cat "$OUT")"
if ! says "$KEYID"; then
	fail "the key id did not show as it was typed, so the echo check proves nothing: $(cat "$OUT")"
elif ! says "Storage secret key"; then
	fail "the secret was never asked for, so the echo check proves nothing: $(cat "$OUT")"
elif says "$SECRET"; then
	fail "the secret was shown as it was typed"
else
	pass "the key id showed as it was typed; the secret did not"
fi
pw=$(shown_password "$OUT")
[ -n "$pw" ] && pass "a password was shown" || fail "no password was shown: $(cat "$OUT")"
says "Store it off the box now, with the repository URL, $REPO, and the storage key" && says "Type stored to go on" && pass "and had to be confirmed stored, with what to store beside it" || fail "the stored step: $(cat "$OUT")"
says "account motel-backup: made" && getent passwd motel-backup | grep -q ':/nonexistent:/usr/sbin/nologin$' && pass "the account was made: no home, no shell" || fail "the account: $(getent passwd motel-backup) — $(grep account "$OUT")"
motel-backup account >/dev/null 2>&1 && pass "and motel-backup account accepts it" || fail "account: $(motel-backup account 2>&1)"
[ "$(stat -c '%U %a' "$ETC")" = "root 755" ] && [ "$(stat -c '%U %a' "$ENVFILE")" = "root 600" ] && pass "the directory is root's and open to look into; the file is root's alone" || fail "$ETC: $(stat -c '%U %a' "$ETC"), $ENVFILE: $(stat -c '%U %a' "$ENVFILE")"
[ "$(grep -c '^' "$ENVFILE")" = 4 ] && grep -q "^RESTIC_REPOSITORY=$REPO\$" "$ENVFILE" && grep -q "^RESTIC_PASSWORD=$pw\$" "$ENVFILE" && grep -q "^AWS_ACCESS_KEY_ID=$KEYID\$" "$ENVFILE" && grep -q "^AWS_SECRET_ACCESS_KEY=$SECRET\$" "$ENVFILE" && pass "the file holds the four settings, the password among them, and nothing else" || fail "the file: $(sed 's/PASSWORD=.*/PASSWORD=…/' "$ENVFILE")"
[ "$(temps)" = 0 ] && pass "nothing was left beside it" || fail "left beside it: $(ls "$ETC")"
[ "$(units_left)" = 0 ] && pass "no unit is left" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
rr cat config --no-lock >/dev/null 2>&1 && pass "the repository answers the credential the file holds" || fail "the repository does not answer: $(rr cat config --no-lock 2>&1)"
says "a run would back up blog, notyet, shop, under /var/lib/liveswap" && pass "the plan was read first, and said" || fail "the plan: $(cat "$OUT")"
says "^looking for a repository at $REPO" && pass "the repository was looked for before a password was made" || fail "the look: $(cat "$OUT")"
# This box's timer is not active (the image undoes the package's
# enabling): setup says so, and how to have it; the package smoke sees
# the other wording, with the timer active.
says "^next: nothing runs it on a schedule: systemctl enable --now motel-backup.timer, or run motel-backup run hourly from a timer or cron entry of your own; then motel-backup status" && pass "and what to do next, from the timer being inactive here" || fail "next: $(cat "$OUT")"
journalctl --sync >/dev/null 2>&1
if ! journalctl --no-pager | grep -q "motel backup: init the repository"; then
	fail "the journal does not show the init unit, so it cannot show what is not in it"
elif journalctl --no-pager | grep -q -e "$pw" -e "$SECRET"; then
	fail "a credential is in the journal"
else
	pass "no credential is in the journal"
fi
if motel-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run, exit 0" || fail "status said: $(cat "$OUT")"; else fail "status after setup exits $?: $(cat "$OUT")"; fi
run && pass "a run works with what setup wrote" || fail "the run: $(cat "$OUT")"
expect_class blog ok "after setup"
expect_class shop ok "after setup"

echo "=== setup 4: a Caddyfile a run could not plan from is refused before any prompt ==="
# shellcheck disable=SC2016 # the Caddyfile's own {$NAME}, not the shell's
sed 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' /root/Caddyfile.base >"$CADDYFILE"
moteld validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1 && pass "fixture: motel accepts the file" || fail "fixture: motel rejects the file: the scenario proves nothing"
before=$(sum)
at_tty motel-backup setup "$S3/other" >"$OUT" 2>&1 && fail "setup exited 0 with a Caddyfile a run cannot plan from" || { says "could not be turned into a plan (exit 1): .*LIVESWAP_ROOT" && ! says "Storage key id" && pass "refused with the adapter's reason, naming the variable, before any prompt" || fail "said: $(cat "$OUT")"; }
[ "$(sum)" = "$before" ] && pass "the working file is as it was" || fail "the working file changed"
cp /root/Caddyfile.base "$CADDYFILE"

echo "=== setup 5: a file at the old path is named, and not read ==="
mv "$ENVFILE" /root/env.keep
printf 'RESTIC_PASSWORD=old\n' >"$OLD"
chmod 0600 "$OLD"
run && fail "a run with only the old file exited 0" || { says "not set up: $ENVFILE is not there" && says "$OLD is from before this version and is not read: run \`motel-backup setup --existing <its RESTIC_REPOSITORY>\` as root, which asks for its RESTIC_PASSWORD; then remove it" && ! says "lstat" && pass "a run names the new path, and says what to do with the old one" || fail "the run said: $(cat "$OUT")"; }
[ "$(units_left)" = 0 ] && pass "and started nothing" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
motel-backup status >"$OUT" 2>&1
[ $? = 3 ] && says "$OLD is from before this version" && pass "status exits 3 and says the same" || fail "status: $(cat "$OUT")"
# A setup that goes through with the old file still there says so at
# the end: that file holds a copy of the credential.
mv /root/env.keep "$ENVFILE"
converse "motel-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
[ $? = 0 ] && says "$OLD is still there, from before this version, and is not read; it holds a copy of the credential: remove it (rm $OLD)" && [ -f "$OLD" ] && pass "a setup with the old file still there says to remove it, and does not itself" || fail "the old file after a setup: $(grep "$OLD" "$OUT"; ls -la "$OLD" 2>&1)"
rm -f "$OLD"

echo "=== setup 6: mistakes over a working setup leave it as it was ==="
before=$(sum)
t0=$(date +%s)
converse "motel-backup setup $S3/setupwrong" "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret "$P_STORED" stored "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret "$P_KEY" "$KEYID" "$P_SECRET" wrong-secret
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "restic could not make or open the repository (exit 1): .*signature" && [ "$took" -lt 90 ] && pass "a wrong storage key is refused by the storage (${took}s), and said" || fail "wrong key: exit $rc after ${took}s: $(cat "$OUT")"
says "there is no repository at the configured location" && fail "an exit 1 was called 'no repository'" || pass "and not called 'no repository'"
[ "$(grep -c 'the key id and secret again (the password shown above still applies)' "$OUT")" = 2 ] && [ "$(grep -c 'Storage key id' "$OUT")" = 3 ] && pass "the key was asked for again, twice, the one password standing" || fail "the retries: $(grep -c 'Storage key id' "$OUT") askings, $(grep -c 'again' "$OUT") agains"
[ "$(grep -c 'Repository password (new)' "$OUT")" = 1 ] && says "the password shown above was never used: discard it" && pass "one password was shown, and said to be dead at the end: init never opened the storage" || fail "the password's fate: $(grep -c 'Repository password (new)' "$OUT") shown; $(tail -1 "$OUT")"
says "no repository answered within 10s: a bucket not made yet, a wrong key and a wrong host look alike here" && pass "the silent look was explained" || fail "the look: $(grep looking -A1 "$OUT" | head -3)"
says "look alike here; a new repository will be made with the password shown next (restic said: .*returned error, retrying after .*signature" && pass "and quoted the storage's answer from the look's last retry line" || fail "the look quoted: $(grep -o 'restic said: .*' "$OUT" | head -3)"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a wrong key: same=$([ "$(sum)" = "$before" ] && echo yes || echo NO), temps=$(temps), units=$(units_left)"
# A host that does not resolve: the look retries and is given up on,
# init says so at once [measured], and the key is asked for again.
t0=$(date +%s)
converse "motel-backup setup s3:http://nowhere.invalid:9/nothere" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "no such host" && says "the key id and secret again" && [ "$took" -lt 40 ] && pass "a host that does not resolve is said at once after the look (${took}s)" || fail "no such host: exit $rc after ${took}s: $(tail -4 "$OUT")"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "and left the working setup as it was" || fail "after a bad host: temps=$(temps), units=$(units_left)"
converse "motel-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" not-the-password "$P_PW" not-that-one "$P_PW" nor-this
rc=$?
[ "$rc" != 0 ] && says "the repository exists; its password is needed" && says "this password cannot open the repository (exit 12)" && pass "a wrong password for the repository that exists is refused, and said" || fail "wrong password: exit $rc: $(cat "$OUT")"
[ "$(grep -c 'Repository password: ' "$OUT")" = 3 ] && [ "$(grep -c 'cannot open the repository (exit 12): again' "$OUT")" = 2 ] && pass "and asked for again, twice, before giving up" || fail "the password retries: $(grep -c 'Repository password: ' "$OUT") askings"
says "Repository password (new)" && fail "a password was made for a repository that exists" || pass "no password was made for a repository that exists"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a wrong password: temps=$(temps), units=$(units_left)"
# A key that may read and not write. The storage through a proxy of the
# box's own that passes every read and refuses every write: plainly
# (403), where restic exits 1 at once, and then in a way restic retries
# (500; the Internal Error of `rclone serve s3 --read-only` is retried
# alike [measured: rc-runs/rc-results/M1-readonly-storage.out], and B2's
# native API's 401 was, for a quarter of an hour [RC check 6]),
# which the clock ends — 30 s in this box's binary. The opening once
# only read, so both were taken, and every run after failed; it takes
# the repository's lock now, and each ends on one neutral line with
# what restic said, without its retry lines. Then a key that may write
# and not delete (the
# proxy refuses DELETE alone): restic writes the lock file, cannot
# remove it, says so on stderr ("error while unlocking:") and exits 0
# [M4 case A] — the opening reads that line, and refuses the key.
readonly_storage() { # <the status a write gets> [<the methods refused: PUT POST DELETE>]
	printf '{\n\tadmin off\n\tauto_https off\n}\nhttp://127.0.0.1:9100 {\n\t@write method %s\n\trespond @write %s\n\treverse_proxy e2e-s3:9000\n}\n' "${2:-PUT POST DELETE}" "$1" >/root/readonly.Caddyfile
	# The one name each time, collected whatever it ended as, so that
	# a proxy that ended failed does not keep the second from starting;
	# and the start is checked, so that the second case never runs
	# against the first proxy.
	systemctl stop readonly-storage.service 2>/dev/null
	systemctl reset-failed readonly-storage.service 2>/dev/null
	systemd-run --quiet --collect --unit=readonly-storage.service /usr/bin/moteld run --config /root/readonly.Caddyfile --adapter caddyfile || { fail "the read-only proxy ($1) could not be started"; return 1; }
	i=0
	until curl -s -o /dev/null http://127.0.0.1:9100/ || [ "$i" -ge 100 ]; do
		i=$((i + 1))
		sleep 0.1
	done
	systemctl is-active --quiet readonly-storage.service || { fail "the read-only proxy ($1) is not running: $(systemctl status readonly-storage.service 2>&1 | tail -5)"; return 1; }
}
RO=s3:http://127.0.0.1:9100/setuprepo
# Each case runs only against its own proxy (fail counts and returns).
if readonly_storage 403; then
t0=$(date +%s)
converse "motel-backup setup $RO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "the repository exists; its password is needed" && says "restic could not open the repository with its lock (exit 1): Save(<lock/[0-9a-f]\{10\}>) failed: client.PutObject: Access Denied. unable to create lock in backend: client.PutObject: Access Denied" && [ "$took" -lt 20 ] && pass "a key the storage refuses every write, plainly, is refused at the opening, at once (${took}s), in restic's words" || fail "a read-only key, refused plainly: exit $rc after ${took}s: $(cat "$OUT")"
says "repository ready" && fail "setup said the repository was ready" || pass "and not called ready"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a read-only key: same=$([ "$(sum)" = "$before" ] && echo yes || echo NO), temps=$(temps), units=$(units_left)"
fi
if readonly_storage 500; then
t0=$(date +%s)
converse "motel-backup setup $RO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "still waiting for $RO (Ctrl-C is safe: nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it)" && says "restic had not finished opening the repository with its lock within 30s (restic said: Save(<lock/.*returned error, retrying after.*); nothing of setup's has been written; a lock file restic was writing may stay, and hold off a prune, or a check from elsewhere, until restic unlock removes it, with a key that may delete: from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise" && [ "$took" -ge 30 ] && [ "$took" -lt 90 ] && pass "a write restic retries is given up on at the clock (${took}s), restic's retry line appended" || fail "a read-only key, retried: exit $rc after ${took}s: $(cat "$OUT")"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "and left the working setup as it was, its unit stopped" || fail "after the clock: same=$([ "$(sum)" = "$before" ] && echo yes || echo NO), temps=$(temps), units=$(units_left)"
fi
if readonly_storage 403 DELETE; then
t0=$(date +%s)
converse "motel-backup setup $RO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "restic opened the repository and could not remove the lock file it wrote (restic said: Remove(<lock/[0-9a-f]\{10\}>) failed: client.RemoveObject: Access Denied.): a key that may not delete, or a storage that refuses or does not answer deletes, ends here; the lock stays until restic unlock removes it, with a key that may delete — not this one: from this box at once, once setup has ended, given that key's credentials, if no other machine with this hostname runs restic against the repository; otherwise from another machine that holds it once the lock is half an hour old; or with restic unlock --remove-all sooner, when nothing else holds the repository" && [ "$took" -lt 20 ] && pass "a key that may write and not delete is refused by restic's own word on the lock it could not remove (${took}s)" || fail "a key that may not delete: exit $rc after ${took}s: $(cat "$OUT"); locks: $(rr list locks --no-lock 2>&1 | while read -r id; do rr cat lock "$id" --no-lock 2>&1; done)"
says "repository ready" && fail "setup said the repository was ready" || pass "and not called ready"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "the working file is byte-identical, no copy of a credential is left, no unit is running" || fail "after a key that may not delete: same=$([ "$(sum)" = "$before" ] && echo yes || echo NO), temps=$(temps), units=$(units_left)"
# The lock that key left is cleared as the refusal says: restic unlock
# from this box at once once setup has ended, with a key that may
# delete, where no other machine of this hostname runs restic against
# the repository (none does here) — the suite's own [M4 case C].
[ "$(rr list locks --no-lock 2>/dev/null | wc -l)" = 1 ] && pass "the one lock it left is listed" || fail "locks after the refusal: $(rr list locks --no-lock 2>&1)"
rr unlock >/dev/null 2>&1 && [ "$(rr list locks --no-lock 2>/dev/null | wc -l)" = 0 ] && pass "and restic unlock with a key that may delete clears it" || fail "restic unlock: $(rr unlock 2>&1)"
fi
systemctl stop readonly-storage.service 2>/dev/null
# The listing must answer, not merely print nothing: a listing that
# failed would also leave stdout empty.
locks=$(rr list locks --no-lock 2>&1) && [ -z "$locks" ] && pass "no lock file is left in the repository" || fail "locks left, or not listed: $locks"

echo "=== setup 7: a rebuilt box opens the repository it already has, with its own password ==="
rm -f "$ENVFILE"
# --existing on a path with no repository: refused at once with the
# storage's word (exit 10 at the look: the bucket is there, the path holds
# nothing; a bucket not there is retried by the fixture, and met by the
# clock), no password made, nothing written.
t0=$(date +%s)
converse "motel-backup setup --existing $REPO/nothere" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "there is no repository at $REPO/nothere (exit 10): setup --existing opens a repository from an earlier box and makes none" && says "run setup without --existing to make a new repository there" && [ "$took" -lt 30 ] && pass "--existing on a path with no repository is refused with the storage's word (${took}s)" || fail "--existing, nothing there: exit $rc after ${took}s: $(cat "$OUT")"
! says "Repository password" && ! says "stored" && pass "and no password was made or asked for" || fail "--existing, nothing there, said: $(grep -i password "$OUT")"
[ ! -e "$ENVFILE" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "and nothing was written" || fail "after --existing on nothing: file=$([ -e "$ENVFILE" ] && echo yes || echo no), temps=$(temps), units=$(units_left)"
# --existing on a host that does not resolve: the look, under the long
# clock (30 s in the e2e build), retries to it: said, naming the look's
# three, and the key is asked for again — no answer is typed, so the
# conversation ends there: no password made or asked for, nothing
# written. restic's retry line is quoted where it wrote one before the
# clock (a resolver that answers at once does; one that times out may
# not), so the quote is not required.
t0=$(date +%s)
converse "motel-backup setup --existing s3:http://nowhere.invalid:9/nothere" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "looking for a repository at s3:http://nowhere.invalid:9/nothere (up to 30s)" && says "no repository answered within 30s\( (restic said: .*)\)\?: a bucket not made yet, a wrong key and a wrong host look alike here; with --existing no password is made" && says "the key id and secret again: a wrong key is one of the three; the same key, if the storage was only slow to answer, has the look run again" && ! says "refused the key" && [ "$(grep -c 'Storage key id' "$OUT")" = 2 ] && { says "restic said: .*no such host" || echo "note: no retry line was quoted (a resolver that times out)"; } && ! says "opening" && [ "$took" -ge 30 ] && [ "$took" -lt 100 ] && pass "--existing on a host that does not resolve is looked for under the long clock, said, and the key asked for again (${took}s)" || fail "--existing, no such host: exit $rc after ${took}s: $(cat "$OUT")"
! says "Repository password" && ! says "stored" && [ ! -e "$ENVFILE" ] && [ "$(temps)" = 0 ] && [ "$(units_left)" = 0 ] && pass "and made no password, asked for none, and wrote nothing" || fail "after --existing on no such host: $(grep -i password "$OUT"); file=$([ -e "$ENVFILE" ] && echo yes || echo no), temps=$(temps), units=$(units_left)"
# Without the flag first, as before #389: no file on the box, the look
# under its short clock finds the repository and asks for its own
# password; the record, which no file tied to this repository, is put
# aside. Then the record is put back and the file removed for the
# guide's own step.
converse "motel-backup setup $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
[ "$rc" = 0 ] && says "repository ready: $REPO (existing, id [0-9a-f]\{8\})" && says "(up to 10s)" && says "the repository exists; its password is needed" && ! says "Repository password (new)" && says "the record was put aside (no credential file was there to tie it to this repository)" && [ "$(ls "$STATUS".aside-* | wc -l)" = 1 ] && pass "setup without the flag found the existing repository under its short clock, asked for its own password, and put the record aside" || fail "the existing repository, no flag: exit $rc: $(cat "$OUT")"
mv "$(ls "$STATUS".aside-* | head -1)" "$STATUS" || fail "the record could not be put back: $(ls /var/lib/motel-backup)"
rm -f "$ENVFILE"
# The guide's rebuilt box: --existing, the same URL, the box's key.
t0=$(date +%s)
converse "motel-backup setup --existing $REPO" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw"
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" = 0 ] && says "repository ready: $REPO (existing, id [0-9a-f]\{8\})" && pass "setup --existing opened the existing repository (${took}s)" || fail "rebuilt box: exit $rc after ${took}s: $(cat "$OUT")"
grep -q "^RESTIC_PASSWORD=$pw\$" "$ENVFILE" && pass "the file holds the repository's own password" || fail "the file holds: $(grep PASSWORD "$ENVFILE" | sed 's/=.*/=…/')"
says "$pw" && fail "the repository's password was shown as it was typed" || pass "the repository's password was not shown as it was typed"
says "the repository exists; its password is needed" && says "(up to 30s)" && ! says "Repository password (new)" && ! says "stored" && pass "a rebuilt box is asked for the password it has, under the long clock, and shown none" || fail "the rebuilt box was shown: $(grep -i 'password\|looking' "$OUT")"
rr cat config --no-lock >/dev/null 2>&1 && pass "the repository answers" || fail "the repository does not answer"
# With no file before, the record cannot be tied to this repository: it
# is put aside, and the next run drills what it backs up here.
says "the record was put aside (no credential file was there to tie it to this repository): $STATUS.aside-.*; the next run drills what it backs up" && [ "$(ls "$STATUS".aside-* | wc -l)" = 1 ] && pass "with no file before, the record was put aside, and why was said" || fail "the record: $(ls /var/lib/motel-backup) — $(grep aside "$OUT")"
if motel-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run" || fail "status: $(cat "$OUT")"; else fail "status exits $?: $(cat "$OUT")"; fi
rm -f "$STATUS".aside-*
run && pass "a run makes a new record on it" || fail "the run: $(cat "$OUT")"

echo "=== setup 8: Ctrl-C at the secret's prompt writes nothing, and puts echo back ==="
before=$(sum)
rm -f /root/in
mkfifo /root/in
script -qec "motel-backup setup $S3/setupint; stty -a" /dev/null </root/in >/root/int.out 2>&1 &
sp=$!
exec 3>/root/in
printf '%s\n' "$KEYID" >&3
i=0
until grep -q "Storage secret key" /root/int.out || [ "$i" -ge 100 ]; do
	i=$((i + 1))
	sleep 0.1
done
grep -q "Storage secret key" /root/int.out && pass "setup is at the secret's prompt" || fail "setup never asked for the secret: $(cat /root/int.out)"
kill -INT "$(pgrep -x motel-backup)" 2>/dev/null
exec 3>&-
wait "$sp"
grep -q ' echo ' /root/int.out && pass "echo is on again after the interrupt" || fail "the terminal after the interrupt: $(grep -o '[-]*echo ' /root/int.out | head -1)"
grep -q "interrupted: nothing has been written" /root/int.out && ! grep -q "context canceled" /root/int.out && pass "the interrupt is said in the operator's words" || fail "after Ctrl-C it said: $(grep motel-backup: /root/int.out)"
[ "$(sum)" = "$before" ] && [ "$(temps)" = 0 ] && pass "nothing was written" || fail "after Ctrl-C: temps=$(temps)"

echo "=== setup 9: killed while the repository is being made: the next setup sweeps, and the shown password opens it ==="
OUT=/root/kill.out converse "motel-backup setup $S3/setupkill" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored &
sp=$!
if hold_restic "restic init"; then
	kill -KILL "$(pgrep -x motel-backup)"
	wait "$sp" 2>/dev/null
	pw1=$(shown_password /root/kill.out)
	[ -n "$pw1" ] && pass "the killed setup had shown its password" || fail "no password in $(cat /root/kill.out)"
	[ "$(temps)" = 1 ] && [ "$(stat -c '%U %a' "$ETC"/repository.env.*)" = "root 600" ] && pass "it left its file beside the working one, root's alone" || fail "left: $(ls -la "$ETC")"
	cp "$ENVFILE" "$ENVFILE.bak"
	[ "$(sum)" = "$before" ] && pass "and the working file as it was" || fail "the working file changed"
	[ "$(units_running)" = 1 ] && pass "and its init unit running" || fail "units running: $(units_running)"
	kill -CONT "$pid"
	until_units 0 'motel_backup_*'
	converse "motel-backup setup $S3/setupkill" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw1"
	rc=$?
	says "removed a file an interrupted setup left: $ENVFILE\." && [ "$(temps)" = 0 ] && pass "the next setup removed it, and said so" || fail "the sweep: $(cat "$OUT") — $(ls "$ETC")"
	[ -f "$ENVFILE.bak" ] && ! says "$ENVFILE.bak" && pass "and left the operator's own copy beside it alone" || fail "the operator's copy: $(ls "$ETC") — $(grep bak "$OUT")"
	rm -f "$ENVFILE.bak"
	[ "$rc" = 0 ] && says "the repository exists; its password is needed" && says "repository ready: $S3/setupkill (existing" && pass "the password the killed setup showed opens the repository it made" || fail "exit $rc: $(cat "$OUT")"
	says "the record was put aside (it was written against another repository (id [0-9a-f]\{8\})): /var/lib/motel-backup/status.json.aside-" && [ "$(ls /var/lib/motel-backup/status.json.aside-* | wc -l)" = 1 ] && pass "the record of the other repository was put aside, and why was said" || fail "the record: $(ls /var/lib/motel-backup) — $(grep aside "$OUT")"
	[ "$(stat -c '%U %a' /var/lib/motel-backup/status.json.aside-*)" = "root 644" ] && pass "the aside is as the record was" || fail "the aside: $(stat -c '%U %a' /var/lib/motel-backup/status.json.aside-*)"
	nothing_left "after the sweep"
else
	fail "no restic init appeared to hold still"
fi

echo "=== setup 10: with the record aside, the box is a fresh setup on the new repository ==="
if motel-backup status >"$OUT" 2>&1; then says "pending first run" && pass "status: pending first run" || fail "status: $(cat "$OUT")"; else fail "status exits $?: $(cat "$OUT")"; fi
run && pass "the run into the new repository" || fail "the run: $(cat "$OUT")"
proof=$(proven blog)
[ -n "$proof" ] && rr snapshots --no-lock --json 2>/dev/null | grep -q "\"id\":\"$proof\"" && pass "and it drilled the snapshot it made there: restore proven on the repository in use" || fail "proof $proof is not in $S3/setupkill: $(cat "$OUT")"
# The same repository set up again is known by the id beside the
# record, and keeps it: the proof stands.
ID_FILE=/var/lib/motel-backup/repository-id
converse "motel-backup setup $S3/setupkill" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_PW" "$pw1"
rc=$?
[ "$(stat -c '%U %a' "$ID_FILE")" = "root 644" ] && says "repository ready: $S3/setupkill (existing, id $(cut -c1-8 "$ID_FILE"))" && pass "the repository's id is kept beside the record, for everyone to read" || fail "the id file: $(ls -l "$ID_FILE" 2>&1) $(cat "$ID_FILE" 2>&1)"
[ "$rc" = 0 ] && ! says "put aside" && [ "$(proven blog)" = "$proof" ] && [ "$(ls /var/lib/motel-backup/status.json.aside-* | wc -l)" = 1 ] && pass "the same repository set up again keeps its record" || fail "set up again: exit $rc, proof $(proven blog), $(grep aside "$OUT")"

echo "=== setup 11: a repository that does not answer: told what is waited for, then given up on, init left running and recorded ==="
systemd-socket-activate -l 127.0.0.1:9999 /bin/sleep 3600 >/dev/null 2>&1 &
bh=$!
sleep 1
before=$(sum)
t0=$(date +%s)
converse "motel-backup setup s3:http://127.0.0.1:9999/blackhole" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
took=$(($(date +%s) - t0))
[ "$rc" != 0 ] && says "still waiting for s3:http://127.0.0.1:9999/blackhole (Ctrl-C leaves restic init running, and the next setup waits for it; it may be making the repository with the password shown: keep it)" && pass "a person waiting on init is told what for, and what Ctrl-C would do" || fail "the wait: exit $rc after ${took}s: $(cat "$OUT")"
# The box's binary is built with -tags e2e, whose clock is 30 s where
# the package's is two minutes (backups/engine/setupclock_e2e.go; the
# shipped value is held by a unit test).
says "the repository did not answer within 30s; restic init is left running as motel_backup_init_[0-9a-f]\{12\}.service, and the next run or setup waits for it (if it must be ended: systemctl stop motel_backup_init_" && [ "$took" -ge 40 ] && [ "$took" -lt 100 ] && pass "given up on after the look's 10 s and the clock's 30 s (${took}s): the waiting, not the unit" || fail "the clock: exit $rc after ${took}s: $(tail -3 "$OUT")"
init_unit=$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_init_*' | awk '{print $1}')
[ -n "$init_unit" ] && [ -f /run/motel-backup/init-unit ] && [ "$(cat /run/motel-backup/init-unit)" = "$init_unit" ] && pass "init is still running, and recorded for the next lock holder" || fail "after the clock: init unit '$init_unit', marker $(cat /run/motel-backup/init-unit 2>&1)"
# The staged file and the run directory stay for the unit — the manager
# opens both in the unit's first moments, which the clock or Ctrl-C may
# come before — and the next setup removes them after its wait.
[ "$(temps)" = 1 ] && [ "$(sum)" = "$before" ] && pass "the staged file is left for init, and the working file is as it was" || fail "after the clock: temps=$(temps)"
# A setup that comes now waits for that init, up to three minutes, and
# says so; the operator ends it the way the message says.
converse "motel-backup setup $S3/afterclock" "$P_KEY" "$KEYID" &
sp=$!
i=0; until grep -q "waiting for the restic init a setup that did not finish left running: $init_unit" "$OUT" || [ "$i" -ge 300 ]; do i=$((i + 1)); sleep 0.1; done
grep -q "waiting for the restic init a setup that did not finish left running: $init_unit" "$OUT" && pass "the next setup waits for the init that was left running, and says which" || fail "the next setup: $(cat "$OUT")"
systemctl stop "$init_unit"
wait "$sp"
[ $? != 0 ] && grep -q "$P_KEY" "$OUT" && says "removed a file an interrupted setup left: $ENVFILE.staged" && [ "$(temps)" = 0 ] && pass "once it ended, the next setup removed the staged file it left, and went on to its own prompts" || fail "after the init ended: temps=$(temps): $(tail -3 "$OUT")"
until_units 0 'motel_backup_*'
[ "$(units_left)" = 0 ] && [ ! -f /run/motel-backup/init-unit ] && pass "the unit is gone, and its record with it" || fail "after stopping by hand: units=$(units_left), marker $(ls /run/motel-backup/)"
converse "motel-backup setup s3:http://127.0.0.1:9999/blackhole" "$P_KEY" "$KEYID" "$P_SECRET" "$SECRET" "$P_STORED" stored &
sp=$!
until_units 1 'motel_backup_init_*'
[ "$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_init_*' | wc -l)" = 1 ] && pass "the init unit is waiting on the repository" || fail "no init unit is running"
kill -INT "$(pgrep -x motel-backup)"
wait "$sp"
[ $? != 0 ] && grep -q "interrupted: nothing has been written; keep the password shown above: restic init ran with it" "$OUT" && pass "interrupted during init, setup exits non-zero and says to keep the shown password" || fail "interrupted setup: $(tail -2 "$OUT")"
init_unit=$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_init_*' | awk '{print $1}')
[ -n "$init_unit" ] && [ "$(temps)" = 1 ] && [ "$(sum)" = "$before" ] && pass "Ctrl-C left init running, with its staged file, and the working file as it was" || fail "after Ctrl-C: init '$init_unit' temps=$(temps)"
systemctl stop "$init_unit" 2>/dev/null
until_units 0 'motel_backup_*'
[ "$(units_running)" = 0 ] && pass "ended by hand, as the message says" || fail "units still running: $(units_running)"
kill "$bh" 2>/dev/null
rm -f "$ENVFILE.staged"

echo "=== setup 12: status as root says where the file is not read as written; as nobody it cannot look ==="
cp "$ENVFILE" /root/env.keep
motel-backup status >"$OUT" 2>&1
rc_before=$?
printf ' RESTIC_REPOSITORY = %s\n' "$S3/setupkill" >>"$ENVFILE"
motel-backup status >"$OUT" 2>&1
rc_root=$?
says "warning: $ENVFILE: line 5: the key is written with whitespace around it; the manager reads it as RESTIC_REPOSITORY" && says "warning: $ENVFILE: line 5: RESTIC_REPOSITORY is set again; the manager takes this one" && pass "status as root says what the manager makes of a hand-edited line" || fail "status as root: $(cat "$OUT")"
[ "$rc_root" = "$rc_before" ] && pass "and its verdict is unchanged by it" || fail "status exited $rc_root, $rc_before before the edit"
as_nobody cat "$ENVFILE" >/dev/null 2>&1 && fail "fixture: nobody can read the credential file" || pass "fixture: nobody cannot read the credential file"
# A file that is there and cannot be read — a directory in its place —
# is said, not passed over.
cp /root/env.keep "$ENVFILE"
mv "$ENVFILE" /root/env.keep2 && mkdir "$ENVFILE"
motel-backup status >"$OUT" 2>&1
says "warning: $ENVFILE: could not be read: " && pass "status as root says when the file cannot be read" || fail "a directory in the file's place: $(cat "$OUT")"
rmdir "$ENVFILE" && mv /root/env.keep2 "$ENVFILE"
as_nobody motel-backup status >"$OUT" 2>&1
[ $? = "$rc_root" ] && ! says "warning: $ENVFILE" && pass "as nobody, the same verdict and no word of the file" || fail "as nobody: $(cat "$OUT")"
# A b2: repository written by hand — or set up before setup refused
# it — is said by status as root in setup's words; the verdict is the
# record's, until a run refuses it in its own.
cp /root/env.keep "$ENVFILE"
printf 'RESTIC_REPOSITORY=b2:hs-backups:dev\n' >>"$ENVFILE"
motel-backup status >"$OUT" 2>&1
rc_b2=$?
says "warning: $ENVFILE: RESTIC_REPOSITORY: b2: is not supported: B2 is reached at its S3-compatible endpoint, s3:https://s3.<region>.backblazeb2.com/hs-backups/dev, with an application key, not the master key" && pass "status as root says a b2: repository is not supported, naming its S3 URL" || fail "status of a b2: file: $(cat "$OUT")"
[ "$rc_b2" = "$rc_before" ] && pass "and its verdict is unchanged by it" || fail "status exited $rc_b2, $rc_before before the edit"
cp /root/env.keep "$ENVFILE"

echo "=== setup 13: the file written by hand, and the documented init that never sources it ==="
# The README's by-hand path: the file as setup would write it, and
# restic init through the manager's own reading of it — never a shell's.
write_env "$S3/byhand" 'p#a$s;s`w"x'
by_hand() { systemd-run --quiet --pipe --wait --collect -p User=motel-backup -p EnvironmentFile="$ENVFILE" -p CacheDirectory=motel-backup -E RESTIC_CACHE_DIR=/var/cache/motel-backup -E HOME=/nonexistent /usr/bin/restic "$@"; }
# The suite's own rr sources the file through the shell, which is the
# very thing the scenario is about: the repository is read back the
# way the manager reads the file.
by_hand init >"$OUT" 2>&1 && by_hand cat config --no-lock >/root/byhand.cfg 2>&1 && grep -q '"id"' /root/byhand.cfg && pass "the documented by-hand init makes a repository the file opens, with a password a shell would have mangled" || fail "by hand: $(cat "$OUT" /root/byhand.cfg 2>&1 | tail -3)"
[ "$(stat -c %U /var/cache/motel-backup)" = motel-backup ] && pass "and leaves the cache the account's" || fail "cache owner: $(stat -c %U /var/cache/motel-backup)"
cp /root/env.keep "$ENVFILE"

echo "=== setup 14: setup while a run holds the lock ==="
motel-backup run >/root/first.out 2>&1 &
first=$!
if hold_restic "restic backup"; then
	at_tty motel-backup setup "$REPO" >"$OUT" 2>&1 && fail "setup exited 0 while a run held the lock" || { says "another backup run is in progress: pid $first" && ! says "Storage key id" && pass "setup says who holds the lock, and asks nothing" || fail "said: $(cat "$OUT")"; }
	kill -CONT "$pid"
	wait "$first"
else
	fail "no restic backup appeared to hold still"
fi
nothing_left "at the end"

echo "=== setup 15: a secret typed ahead of its prompt is not taken ==="
# The key and a line after it in one paste, at the key's prompt: the
# line discipline echoes the second line as it arrives, and the flush
# that turns echo off discards it — the secret is asked for, and typed,
# afresh. Without the flush the pasted line is the secret, and setup
# goes on with a key the storage refuses.
converse "motel-backup setup $S3/setupahead" "$P_KEY" "$KEYID
typed-ahead-not-the-secret" "$P_SECRET" "$SECRET" "$P_STORED" stored
rc=$?
[ "$rc" = 0 ] && says "repository ready: $S3/setupahead (new" && grep -q "^AWS_SECRET_ACCESS_KEY=$SECRET\$" "$ENVFILE" && ! grep -q typed-ahead "$ENVFILE" && pass "the line typed ahead was discarded, and the secret typed at its prompt is the one in the file" || fail "typed ahead: exit $rc: $(grep -c 'Storage secret key' "$OUT") askings; $(tail -3 "$OUT")"
cp /root/env.keep "$ENVFILE"

# The other suites start from a record of their own.
rm -f "$STATUS" /var/lib/motel-backup/status.json.aside-* /var/lib/motel-backup/repository-id

if [ "$FAILURES" = 0 ]; then
	echo "ALL SETUP SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES SETUP ASSERTION(S) FAILED"
exit 1
