cat >/root/ro.Caddyfile <<'C'
{
	admin off
	auto_https off
}
http://127.0.0.1:9100 {
	@write method PUT POST DELETE
	respond @write 403
	reverse_proxy https://s3.docs.test:9000 {
		header_up Host {hostport}
	}
}
C
systemd-run --quiet --unit=ro-proxy /usr/bin/moteld run --config /root/ro.Caddyfile --adapter caddyfile
sleep 2; systemctl is-active ro-proxy
export RESTIC_REPOSITORY=s3:http://127.0.0.1:9100/box1 AWS_ACCESS_KEY_ID=BOXKEYID0000 AWS_SECRET_ACCESS_KEY=box-secret-not-a-secret RESTIC_PASSWORD=$(cat /root/repo.pw) RESTIC_CACHE_DIR=/root/suite-cache
t() { s=$(date +%s.%N); timeout "$1" "${@:2}" 2>&1 | head -3; echo "[exit ${PIPESTATUS[0]}, $(awk "BEGIN{printf \"%.1f\", $(date +%s.%N)-$s}")s] ${*:2}"; }
t 20 restic cat config --no-lock
t 40 restic cat config
systemctl stop ro-proxy
