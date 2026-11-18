#!/bin/sh
# The backup suite. Runs inside an e2e-backup-box of its own as root,
# against real systemd, Debian's restic and sqlite3, and the S3 server
# at e2e-s3.
# Mostly failure paths: what a run says, and leaves behind, when
# something is wrong.
. /lib.sh
. /lib-backup.sh

REPO=s3:http://e2e-s3:9000/boxrepo

wait_for_systemd
cp "$CADDYFILE" /root/Caddyfile.base

echo "=== backup 0: before setup, a run refuses and starts nothing ==="
rm -f "$ENVFILE"
if run; then fail "a run with no $ENVFILE exited 0"; else grep -q "not set up" "$OUT" && pass "a run before setup says backups are not set up" || fail "before setup: $(cat "$OUT")"; fi
[ "$(units_left)" = 0 ] && pass "and started no unit" || fail "units were started before setup"

seed
write_env "$REPO" "$PASSWORD"
rr init -q || { echo "FATAL: could not initialise the repository at $REPO"; exit 1; }

echo "=== backup 0b: an account motel did not make stops a run, before any unit ==="
# It is the run that puts the credential in the account's environment:
# what setup refuses of the account, a run refuses too. The account's
# mark taken off stands for one made by someone else.
usermod --comment "" motel-backup
if run; then fail "a run with an account motel did not make exited 0"; else grep -q "the motel-backup account on this box was not made by motel" "$OUT" && pass "a run refuses an account motel did not make" || fail "the run's words: $(cat "$OUT")"; fi
[ "$(units_left)" = 0 ] && pass "and started no unit" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
grep -q '"error": "the motel-backup account on this box was not made by motel' "$STATUS" && pass "and the record says so, for status" || fail "the record: $(head -c 300 "$STATUS")"
usermod --comment made-by-motel motel-backup

echo "=== backup 0c: a b2: repository stops a run, before any unit ==="
# restic's own B2 backend retries a failed upload without end (#177):
# what setup refuses, a file written by hand — or set up before setup
# refused it — is refused by the run, naming the same repository at
# B2's S3-compatible endpoint. Read as the manager reads the file: in
# quotes, after an earlier line.
printf 'RESTIC_REPOSITORY=%s\nRESTIC_PASSWORD=%s\nRESTIC_REPOSITORY="b2:hs-backups:dev"\nB2_ACCOUNT_ID=K001\nB2_ACCOUNT_KEY=k\n' "$REPO" "$PASSWORD" >"$ENVFILE"
if run; then fail "a run with a b2: repository exited 0"; else grep -q "the repository is b2:, which is not supported: as root, motel-backup setup --existing 's3:https://s3.<region>.backblazeb2.com/hs-backups/dev' moves it to B2's S3 endpoint, with an application key" "$OUT" && pass "a run refuses a b2: repository, naming its S3 URL" || fail "the run's words: $(cat "$OUT")"; fi
[ "$(units_left)" = 0 ] && pass "and started no unit" || fail "units: $(systemctl list-units --all --no-legend 'motel_backup_*')"
grep -q '"error": "the repository is b2:, which is not supported' "$STATUS" && pass "and the record says so, for status" || fail "the record: $(head -c 400 "$STATUS")"
motel-backup status >"$OUT" 2>&1
grep -q "the repository is b2:, which is not supported" "$OUT" && grep -q "warning: $ENVFILE: RESTIC_REPOSITORY: b2: is not supported" "$OUT" && pass "status names it, and as root warns of the file" || fail "status: $(cat "$OUT")"
write_env "$REPO" "$PASSWORD"

echo "=== backup 1: a run backs up what is declared, and only that ==="
if run; then pass "the run exits 0"; else fail "the run failed: $(cat "$OUT")"; fi
expect_class blog ok "first run"
expect_class shop ok "first run"
expect_class notyet pending "an app declared and never deployed"
grep -q '^blog: ok: snapshot [0-9a-f]\{8\} holds sqlite app.db, files uploads$' "$OUT" && pass "the run names the snapshot and what it found in it" || fail "the run's own words: $(grep '^blog' "$OUT")"
grep -q "notyet: pending: /var/lib/liveswap/notyet/shared does not exist yet" "$OUT" && pass "pending says where it looked" || fail "pending: $(grep '^notyet' "$OUT")"
rr ls --no-lock latest --tag app:blog >/root/ls.blog 2>/dev/null
rr ls --no-lock --long latest --tag app:shop >/root/ls.shop 2>/dev/null
grep -q '^/backup/blog/files/uploads/a.png$' /root/ls.blog && grep -q '^/backup/blog/sqlite/app.db$' /root/ls.blog && pass "blog's snapshot holds its files and its database copy, under /backup/blog" || fail "blog's snapshot: $(cat /root/ls.blog)"
grep -q private.txt /root/ls.blog && fail "blog's snapshot holds a file that was not declared" || pass "what blog did not declare is not in its snapshot"
grep -q '/backup/blog/plan.json$' /root/ls.blog && pass "the snapshot carries its own declaration" || fail "no plan.json in blog's snapshot"
# files . takes in data/shop.db: the live file must never be what is uploaded.
if ! grep -q '/backup/shop/files/r.txt$' /root/ls.shop; then
	fail "shop's snapshot could not be listed, so nothing is known about what is in it"
elif grep -q '/backup/shop/files/data/shop.db' /root/ls.shop; then
	fail "the live database, or something in its place, is in the snapshot under files/: $(grep 'files/data/shop.db' /root/ls.shop)"
else
	pass "a database inside a files path is not uploaded there at all: only its copy, under sqlite/"
fi
rm -rf /root/restored && rr restore -q --no-lock latest --tag app:shop --target /root/restored >/dev/null 2>&1
[ "$(sqlite3 /root/restored/backup/shop/sqlite/data/shop.db 'pragma integrity_check; select count(*) from orders' 2>&1 | tr '\n' ' ')" = "ok 3 " ] && pass "the copy that comes back out of the repository is intact and has its rows" || fail "the restored copy: $(sqlite3 /root/restored/backup/shop/sqlite/data/shop.db 'pragma integrity_check; select count(*) from orders' 2>&1)"
rr snapshots --no-lock --json 2>/dev/null | grep -o '"hostname":"[^"]*"' | sort -u | grep -qx '"hostname":"motel"' && pass "every snapshot is of host 'motel', whatever the box is called ($(hostname))" || fail "snapshot hostnames: $(rr snapshots --no-lock --json | grep -o '"hostname":"[^"]*"' | sort -u)"
nothing_left "first run"
[ "$(find /var/lib/liveswap -user root | wc -l)" = 0 ] && pass "nothing root-owned was left in the apps' data" || fail "root-owned in the apps' data: $(find /var/lib/liveswap -user root)"
[ "$(stat -c %U /var/cache/motel-backup)" = motel-backup ] && pass "restic's cache belongs to the account restic runs as" || fail "cache owner: $(stat -c %U /var/cache/motel-backup)"
[ "$(stat -c '%U %a' "$STATUS")" = "root 644" ] && ! grep -q -e "$PASSWORD" -e e2e-fixture-key "$STATUS" && pass "the status record is root's, world-readable, and holds no secret" || fail "the status record: $(stat -c '%U %a' "$STATUS")"
journalctl --sync >/dev/null 2>&1
if ! journalctl --no-pager | grep -q 'motel backup: upload blog'; then
	fail "the journal does not show the run at all, so it cannot show what is not in it"
elif journalctl --no-pager | grep -q -e "$PASSWORD" -e e2e-fixture-key-not-a-secret; then
	fail "a credential is in the journal"
else
	pass "no credential is in the journal"
fi

echo "=== backup 2: two apps do not share a retention group ==="
run; run
tagged() { rr snapshots --no-lock --tag "$1" --json 2>/dev/null | grep -o '"short_id"' | wc -l; }
before=$(tagged motel)
records=$(tagged motel-clean)
rr forget --keep-last 1 >/dev/null 2>&1
blog_left=$(tagged app:blog)
shop_left=$(tagged app:shop)
[ "$before" = 6 ] && [ "$blog_left" = 1 ] && [ "$shop_left" = 1 ] && pass "after three runs, 'forget --keep-last 1' leaves each app its own latest (6 -> 1 + 1)" || fail "retention: $before snapshots before; blog $blog_left, shop $shop_left after"
# And the records that each run ended ok on an app are each app's own
# group too: the last record of each is kept, not one of both.
[ "$records" = 6 ] && [ "$(tagged motel-clean)" = 2 ] && rr snapshots --no-lock --json --tag motel-clean 2>/dev/null | grep -q '"paths":\["/motel-clean-blog"\]' && rr snapshots --no-lock --json --tag motel-clean 2>/dev/null | grep -q '"paths":\["/motel-clean-shop"\]' && pass "and leaves each app its own latest record of a clean run (6 -> 1 + 1)" || fail "records: $records before; after: $(rr snapshots --no-lock --json --tag motel-clean 2>/dev/null | grep -o '"paths":\[[^]]*\]' | tr '\n' ' ')"

echo "=== backup 3: what is not a database is reported, never opened, and stops nothing else ==="
as_app sh -c 'cd /var/lib/liveswap/blog/shared && mkfifo fifo.db && mkfifo -m 0400 rofifo.db && : >empty.db && ln -s app.db link.db && echo "not a database, only a text file" >text.db && mkfifo pipe'
sed -e 's#sqlite app.db#sqlite app.db fifo.db rofifo.db empty.db link.db text.db absent.db#' -e 's#files  uploads#files  uploads pipe#' /root/Caddyfile.base >"$CADDYFILE"
start=$(date +%s)
if run; then fail "a run with six undumpable databases exited 0"; else pass "the run exits non-zero"; fi
took=$(($(date +%s) - start))
[ "$took" -lt 90 ] && pass "and did not wait on any of them (${took}s)" || fail "the run took ${took}s: something blocked"
expect_class blog incomplete "undumpable databases"
expect_class shop ok "undumpable databases in another app"
for db in fifo.db rofifo.db empty.db link.db text.db; do
	tr -d '\n' <"$STATUS" | sed 's/  */ /g' | grep -q "\"path\": \"$db\", \"ok\": false, \"detail\": \"not a database" && pass "$db is reported as not a database" || fail "$db: $(grep -A3 "\"$db\"" "$STATUS" | tr -d '\n')"
done
tr -d '\n' <"$STATUS" | sed 's/  */ /g' | grep -q '"path": "absent.db", "ok": false, "detail": "missing' && pass "absent.db is reported as missing" || fail "absent.db: $(grep -A3 '"absent.db"' "$STATUS" | tr -d '\n')"
rr ls --no-lock latest --tag app:blog >/root/ls.blog 2>/dev/null
grep -q '^/backup/blog/sqlite/app.db$' /root/ls.blog && pass "the one real database was still copied and uploaded" || fail "app.db is not in the snapshot"
# A backup keeps files and directories: a FIFO declared as files is
# nothing a restore could put back.
tr -d '\n' <"$STATUS" | sed 's/  */ /g' | grep -q '"path": "pipe", "ok": false, "detail": "it is a fifo' && pass "a FIFO declared as files is refused, in words" || fail "pipe: $(grep -A3 '"pipe"' "$STATUS" | tr -d '\n')"
grep -q '^/backup/blog/files/uploads/a.png$' /root/ls.blog && ! grep -q 'files/pipe' /root/ls.blog && pass "and is not in the snapshot, while the files beside it are" || fail "the snapshot's files: $(grep files/ /root/ls.blog)"
[ ! -e /var/lib/liveswap/blog/shared/absent.db ] && pass "the missing database was not created in the app's directory" || fail "absent.db now exists in the app's directory"
nothing_left "undumpable databases"
as_app sh -c 'cd /var/lib/liveswap/blog/shared && rm -f fifo.db rofifo.db empty.db link.db text.db pipe'

echo "=== backup 4: a declared files path that is not there ==="
sed 's#files  uploads#files  uploads avatars#' /root/Caddyfile.base >"$CADDYFILE"
run && fail "a run with a missing declared path exited 0" || pass "the run exits non-zero"
expect_class blog incomplete "a missing files path"
tr -d '\n' <"$STATUS" | sed 's/  */ /g' | grep -q '"path": "avatars", "ok": false, "detail": "missing' && pass "avatars is reported as missing, and uploads was still backed up" || fail "avatars: $(grep -A3 '"avatars"' "$STATUS" | tr -d '\n')"

echo "=== backup 4b: an app cannot aim its backup at a sibling's data ==="
# What a compromised blog can do: its declared path is its own to
# replace. The unit that uploads can read any file it is shown, so what
# it is shown must never be decided by a link the app made.
cp /root/Caddyfile.base "$CADDYFILE"
as_app sh -c 'cd /var/lib/liveswap/blog/shared && mv uploads uploads.real && ln -s ../../shop/shared uploads'
run && fail "a run exited 0 with blog's declared path a link to shop's data" || pass "the run exits non-zero"
expect_class blog incomplete "a declared path that is a link"
tr -d '\n' <"$STATUS" | sed 's/  */ /g' | grep -q '"path": "uploads", "ok": false, "detail": "[^"]*symbolic link' && pass "and says the path is a symbolic link" || fail "uploads: $(grep -A3 '"uploads"' "$STATUS" | tr -d '\n')"
if ! rr ls --no-lock latest --tag app:blog >/root/ls.blog 2>/dev/null || ! grep -q '^/backup/blog/sqlite/app.db$' /root/ls.blog; then
	fail "blog's snapshot could not be listed, so nothing is known about what is in it"
elif grep -q -e r.txt -e 'files/uploads' /root/ls.blog; then
	fail "shop's files are in blog's snapshot: $(grep files/ /root/ls.blog)"
else
	pass "nothing of shop's is in blog's snapshot"
fi
expect_class shop ok "a sibling aiming at its data"
as_app sh -c 'cd /var/lib/liveswap/blog/shared && rm uploads && mv uploads.real uploads'

echo "=== backup 5: a root the server and the backup would read differently ==="
n=$(snapshots)
sed 's#root /var/lib/liveswap#root {$LIVESWAP_ROOT:/var/lib/liveswap}#' /root/Caddyfile.base >"$CADDYFILE"
if run; then fail "a run exited 0 with the root written as {\$LIVESWAP_ROOT:...}"; else pass "the run refuses"; fi
journalctl --sync >/dev/null 2>&1
journalctl --no-pager -o cat | grep -q "depends on the environment variable(s) LIVESWAP_ROOT" && pass "naming the variable" || fail "the plan unit said: $(journalctl --no-pager -o cat | grep -i 'motel-backup:' | tail -2)"
[ "$(snapshots)" = "$n" ] && pass "and uploads nothing" || fail "snapshots went from $n to $(snapshots)"
tr -d '\n' <"$STATUS" | grep -q '"last_ok"' && pass "what was known about each app is kept in the record" || fail "the record lost the apps: $(cat "$STATUS")"

echo "=== backup 5b: an ordinary production Caddyfile: variables with no default ==="
# The server has these set; a backup does not have the server's
# environment, and unset they are parse errors. It adapts with made-up
# values of the kind each place takes, and the plan is what it would be.
sed -e 's#admin off#admin off\n\temail {$ACME_EMAIL}#' -e 's#^:8080 {#:{$METRICS_PORT} {\n\trespond metrics\n}\n\n{$SITE_DOMAIN} {#' /root/Caddyfile.base >"$CADDYFILE"
grep -q '{$ACME_EMAIL}' "$CADDYFILE" && grep -q '^:{$METRICS_PORT} {' "$CADDYFILE" && grep -q '^{$SITE_DOMAIN} {' "$CADDYFILE" || fail "the scenario's Caddyfile does not hold the three variables"
env -i /usr/bin/moteld adapt --adapter caddyfile --config "$CADDYFILE" >/dev/null 2>&1 && fail "this Caddyfile adapts with an empty environment: the scenario proves nothing" || pass "with an empty environment this Caddyfile does not adapt"
run && pass "the run exits 0 all the same" || fail "the run failed: $(cat "$OUT"; journalctl --no-pager -o cat | grep 'motel-backup:' | tail -2)"
expect_class blog ok "variables with no default"
expect_class shop ok "variables with no default"
cp /root/Caddyfile.base "$CADDYFILE"

echo "=== backup 6: a symlinked root, and a name systemd would expand ==="
ln -sfn /var/lib/liveswap /srv/liveswap
as_app sh -c 'cd /var/lib/liveswap/blog/shared && mkdir -p "up \$RESTIC_PASSWORD %h" && echo x >"up \$RESTIC_PASSWORD %h/f"'
sed -e 's#root /var/lib/liveswap#root /srv/liveswap#' -e 's#files  uploads#files  uploads "up $RESTIC_PASSWORD %h"#' /root/Caddyfile.base >"$CADDYFILE"
run && pass "the run exits 0" || fail "the run failed: $(cat "$OUT")"
rr ls --no-lock latest --tag app:blog >/root/ls.blog 2>/dev/null
grep -q '^/backup/blog/files/uploads/a.png$' /root/ls.blog && pass "through a symlinked root the snapshot holds the data, not the link, at the same paths" || fail "through a symlinked root: $(cat /root/ls.blog)"
grep -q '^/backup/blog/files/up \$RESTIC_PASSWORD %h/f$' /root/ls.blog && pass "a path spelled \$RESTIC_PASSWORD %h is backed up under exactly that name" || fail "the awkward path: $(grep 'up ' /root/ls.blog)"
grep -q "$PASSWORD" /root/ls.blog && fail "the repository password was expanded into a path" || pass "and the password is nowhere in the snapshot's paths"
as_app rm -rf '/var/lib/liveswap/blog/shared/up $RESTIC_PASSWORD %h'
cp /root/Caddyfile.base "$CADDYFILE"

echo "=== backup 7: data that was backed up and is gone is never 'not deployed yet' ==="
run
mv /var/lib/liveswap/shop /root/shop.away
run && fail "a run exited 0 with shop's data gone" || pass "the run exits non-zero"
expect_class shop "data missing" "data gone after a good backup"
grep -q "shop: data missing: .*was last backed up on .*snapshot [0-9a-f]\{8\}" "$OUT" && pass "and says when it was last backed up, and to which snapshot" || fail "data missing: $(grep '^shop' "$OUT")"
expect_class blog ok "another app's data going"
# A rebuilt box: the record is gone as well as the data. The repository
# remembers what the box does not, so this is still not "pending",
# which a run exits 0 on.
rm -f "$STATUS"
run && fail "a run exited 0 with shop's data gone and no record of it" || pass "with the record gone too, the run still exits non-zero"
expect_class shop "data missing" "data gone, and no record"
grep -q "shop: data missing: .*snapshot [0-9a-f]\{8\}" "$OUT" && pass "the snapshot it names came from the repository" || fail "data missing, no record: $(grep '^shop' "$OUT")"
expect_class notyet pending "an app the repository has never seen"
mv /root/shop.away /var/lib/liveswap/shop

echo "=== backup 8: the wrong repository password ==="
write_env "$REPO" not-the-password
run && fail "a run with the wrong password exited 0" || pass "the run exits non-zero"
expect_class blog failed "wrong password"
grep -q "blog: failed: the repository password is wrong (exit 12)" "$OUT" && pass "and says so" || fail "wrong password: $(grep '^blog' "$OUT")"
expect_class shop "not attempted" "wrong password"
nothing_left "wrong password"

echo "=== backup 9: no repository where the settings point ==="
write_env "$REPO/nothing-here" "$PASSWORD"
run && fail "a run with no repository exited 0" || pass "the run exits non-zero"
# Whichever app meets the repository first says so — an app that failed
# last run goes last, and one with no data asks the repository about
# itself — and the rest are not attempted.
grep -q "there is no repository at the configured location (exit 10)" "$OUT" && pass "and says there is no repository" || fail "no repository: $(cat "$OUT")"
grep -q "no repository" "$OUT" && ! grep -q ": ok: " "$OUT" && pass "and no app is reported ok" || fail "with no repository: $(cat "$OUT")"
rr cat config --no-lock >/dev/null 2>&1
[ $? = 10 ] && pass "and did not create one (restic still exits 10 there)" || fail "restic no longer says 'no repository' there: the run created one, or the check cannot tell"
nothing_left "no repository"
write_env "$REPO" "$PASSWORD"

echo "=== backup 10: one run at a time; a killed run's unit is stopped by the next, by name ==="
as_app sh -c 'head -c 50000000 /dev/urandom >/var/lib/liveswap/blog/shared/uploads/big.bin'
motel-backup run >/root/first.out 2>&1 &
first=$!
i=0
until pid=$(pgrep -x restic); do
	i=$((i + 1))
	[ "$i" -ge 300 ] && break
	sleep 0.1
done
if [ -n "${pid:-}" ] && kill -STOP "$pid"; then
	pass "a run is under way, its restic held still"
	run && fail "a second run exited 0 while the first was under way" || { grep -q "another backup run is in progress: pid $first" "$OUT" && pass "a second run refuses, and says who holds the lock" || fail "the second run: $(cat "$OUT")"; }
	kill -KILL "$first"
	wait "$first" 2>/dev/null
	left=$(systemctl list-units --plain --no-legend --state=activating 'motel_backup_upload_*' | awk '{print $1}')
	[ -n "$left" ] && pass "the killed run left its upload unit running ($left)" || fail "no unit was left running to sweep: the scenario proved nothing"
	run && pass "the next run exits 0" || fail "the run after a killed run: $(cat "$OUT")"
	systemctl list-units --all --plain --no-legend "$left" | grep -q . && fail "$left is still there" || pass "and stopped the unit the killed run left"
	nothing_left "after a killed run"
else
	fail "no restic process appeared to hold still"
fi

echo "=== backup 10b: Ctrl-C stops the run's own unit, and removes its plaintext ==="
motel-backup run >/root/first.out 2>&1 &
first=$!
i=0
until pid=$(pgrep -x restic); do
	i=$((i + 1))
	[ "$i" -ge 300 ] && break
	sleep 0.1
done
if [ -n "${pid:-}" ] && kill -STOP "$pid"; then
	kill -INT "$first"
	wait "$first"
	[ $? != 0 ] && pass "interrupted, a run exits non-zero" || fail "an interrupted run exited 0"
	[ "$(units_running)" = 0 ] && pass "having stopped its own upload unit" || fail "units still running after Ctrl-C: $(systemctl list-units --plain --no-legend 'motel_backup_*') — $(cat /root/first.out)"
	pgrep -x restic >/dev/null && fail "restic is still running" || pass "and restic is gone"
	nothing_left "after Ctrl-C, with no later run to tidy up"
else
	fail "no restic process appeared to hold still"
fi

echo "=== backup 11: a run that is a service takes its units with it, however it dies ==="
systemd-run --quiet --unit motel-backup-suite.service -E MOTEL_BACKUP_UNIT=motel-backup-suite.service /usr/bin/motel-backup run
i=0
until pid=$(pgrep -x restic); do
	i=$((i + 1))
	[ "$i" -ge 300 ] && break
	sleep 0.1
done
if [ -n "${pid:-}" ] && kill -STOP "$pid"; then
	i=0
	[ "$(units_running)" != 0 ] && pass "its upload unit is running when the service is killed" || fail "nothing was running when the service was killed: the scenario proved nothing"
	systemctl kill --signal=SIGKILL motel-backup-suite.service
	while [ "$(units_running)" != 0 ] && [ "$i" -lt 30 ]; do
		i=$((i + 1))
		sleep 1
	done
	[ "$i" -lt 30 ] && pass "SIGKILLed mid-upload, its units were ended by systemd itself within ${i}s" || fail "units still running 30s after the service was killed: $(systemctl list-units --plain --no-legend 'motel_backup_*')"
	pgrep -x restic >/dev/null && fail "restic is still running" || pass "and restic is gone"
else
	fail "no restic process appeared to hold still"
fi
systemctl reset-failed motel-backup-suite.service 2>/dev/null
as_app rm -f /var/lib/liveswap/blog/shared/uploads/big.bin
run && pass "the run after it exits 0" || fail "the run after a killed service: $(cat "$OUT")"
nothing_left "after a killed service"

echo ""
if [ "$FAILURES" -eq 0 ]; then
	echo "ALL BACKUP SCENARIOS PASSED"
	exit 0
fi
echo "$FAILURES BACKUP ASSERTION(S) FAILED"
exit 1
