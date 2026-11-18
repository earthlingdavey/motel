#!/bin/bash
# Inside a fresh install-test container: a rebuilt box. System users made
# first, so that `motel` gets another uid than the first box's 996; the
# RC .deb; the same Caddyfile (app demo, its backup block); nothing deployed
# and no data — "A rebuilt box" restores before the first deploy.
set -eu
mount --make-rshared /
useradd --system other1; useradd --system other2; useradd --system other3
cd /root && cp /rc/moteld_0.3.0.rc1_arm64.deb .
apt-get update -qq
apt-get install -y ./moteld_0.3.0.rc1_arm64.deb >/dev/null 2>&1
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
systemctl reload moteld
adduser --disabled-password --comment "" bob >/dev/null; usermod -aG sudo,adm bob
echo 'bob ALL=(ALL:ALL) NOPASSWD: ALL' >/etc/sudoers.d/bob && chmod 0440 /etc/sudoers.d/bob
echo "rebuilt box ready: $(dpkg -s moteld | grep '^Version'), motel uid $(id -u motel) (the first box's: 996); /var/lib/liveswap holds: $(ls /var/lib/liveswap | tr '\n' ' ')"
