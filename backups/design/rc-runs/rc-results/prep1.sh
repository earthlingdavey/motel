#!/bin/bash
# box1, as root: a Debian 13 image that sets Install-Recommends false (as
# Hetzner's does), first-deploy.md step 2 with the rc1 .deb, an app `demo`
# deployed with data and NO backup block yet, and bob (sudo is root's).
set -eu
echo 'APT::Install-Recommends "false";' >/etc/apt/apt.conf.d/99-no-recommends
echo "### apt-config: $(apt-config dump | grep -i Install-Recommends)"
mkdir -p /var/local/motel && cd /var/local/motel
cp /rc/moteld_0.3.0.rc1_arm64.deb .
apt-get update -qq
echo "### apt install ./moteld_0.3.0.rc1_arm64.deb"
apt install -y ./moteld_0.3.0.rc1_arm64.deb 2>&1 | grep -E "Setting up|Backups:|motel-backup setup|Recommended|restic|sqlite3" || true
echo "### restic: $(command -v restic || echo none); sqlite3: $(command -v sqlite3 || echo none)"
systemctl enable --now moteld >/dev/null 2>&1
cp /ca.crt /usr/local/share/ca-certificates/hsb-docs.crt && update-ca-certificates >/dev/null 2>&1
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
			audience docs
		}

		app demo {
			command ./server
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
printf '#!/bin/sh\nexec /usr/bin/moteld respond --listen "unix/$SOCKET" "hello docs"\n' >"$w/server"; chmod +x "$w/server"
mkdir -p /srv/art && tar -czf /srv/art/demo.tar.gz -C "$w" server
nohup /usr/bin/moteld file-server --listen 127.0.0.1:8200 --root /srv/art >/var/log/artserver.log 2>&1 &
sleep 2
TOKEN=$(moteld deploy-token --key /etc/motel/deploy.key --audience docs --ttl 10m)
i=0; until [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8081/demo)" = 200 ]; do i=$((i+1)); [ $i -ge 30 ] && exit 1; sleep 1; done
curl -s -o /dev/null -w 'deploy: %{http_code}\n' --max-time 90 -X POST -H "Authorization: Bearer $TOKEN" -d '{"url":"http://127.0.0.1:8200/demo.tar.gz","version":"d1"}' http://127.0.0.1:8081/demo
adduser --disabled-password --comment "" bob >/dev/null; usermod -aG sudo,adm bob
echo 'bob ALL=(ALL:ALL) NOPASSWD: ALL' >/etc/sudoers.d/bob && chmod 0440 /etc/sudoers.d/bob
echo "box ready: $(dpkg -s moteld | grep '^Version'), motel uid $(id -u motel)"
