#!/bin/bash
# Inside a fresh install-test container (Debian 13, systemd), as root: the
# v0.3.0-rc1 arm64 .deb (mounted at /rc), an app `demo` with a WAL
# database and uploads under its shared/, and `bob`, an administrator
# whose sudo is root's. For the RC checks against real B2.
set -eu
mount --make-rshared /
cd /root && cp /rc/moteld_0.3.0.rc1_arm64.deb .
apt-get update -qq
apt-get install -y ./moteld_0.3.0.rc1_arm64.deb 2>&1 | grep -E "Setting up (moteld|restic|sqlite3)|Backups:|motel-backup setup"
systemctl enable --now moteld >/dev/null 2>&1
moteld deploy-keygen --out /etc/motel/deploy.key >/dev/null
cat >/etc/motel/Caddyfile <<'EOF'
{
	admin unix//run/motel/admin.sock
	auto_https off
	liveswap {
		root /var/lib/liveswap
		allow_insecure_http
		artifact_allowlist 127.0.0.1:8200
		deploy_trust local {
			public_key /etc/motel/deploy.key.pub
			audience rc
		}
		app demo {
			command ./server
			backup {
				sqlite app.db
				files  uploads
			}
		}
	}
}
:8080 {
	reverse_proxy {
		dynamic liveswap demo
	}
}
:8081 {
	liveswap_webhook
}
EOF
systemctl restart moteld
w=$(mktemp -d)
printf '#!/bin/sh\nexec /usr/bin/moteld respond --listen "unix/$SOCKET" "hello rc"\n' >"$w/server"; chmod +x "$w/server"
mkdir -p /srv/art && tar -czf /srv/art/demo.tar.gz -C "$w" server
nohup /usr/bin/moteld file-server --listen 127.0.0.1:8200 --root /srv/art >/var/log/artserver.log 2>&1 &
sleep 2
TOKEN=$(moteld deploy-token --key /etc/motel/deploy.key --audience rc --ttl 10m)
i=0; until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8081/demo)" = 200 ]; do i=$((i+1)); [ $i -ge 30 ] && exit 1; sleep 1; done
curl -s -o /dev/null -w 'deploy: %{http_code}\n' --max-time 90 -X POST -H "Authorization: Bearer $TOKEN" \
	-d '{"url":"http://127.0.0.1:8200/demo.tar.gz","version":"r1"}' http://127.0.0.1:8081/demo
S=/var/lib/liveswap/demo/shared
su -s /bin/sh motel -c "sqlite3 $S/app.db \"pragma journal_mode=wal; create table notes(id integer primary key, body text); insert into notes(body) values ('first'),('second');\" >/dev/null && mkdir -p $S/uploads && echo 'hello rc' > $S/uploads/hello.txt && head -c 65536 /dev/urandom > $S/uploads/random.bin"
adduser --disabled-password --comment "" bob >/dev/null; usermod -aG sudo,adm bob
echo 'bob ALL=(ALL:ALL) NOPASSWD: ALL' >/etc/sudoers.d/bob && chmod 0440 /etc/sudoers.d/bob
motel-backup validate /etc/motel/Caddyfile
echo "box ready: $(dpkg -s moteld | grep '^Version'), restic $(restic version | awk '{print $2}'), motel uid $(id -u motel)"
