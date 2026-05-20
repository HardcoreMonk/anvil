#!/usr/bin/env bash
set -Eeuo pipefail

result_dir="/workspace/workloads/results"
log_file="$result_dir/nginx.log"
mkdir -p "$result_dir"
exec > >(tee "$log_file") 2>&1

export DEBIAN_FRONTEND=noninteractive

echo "[nginx] starting nginx workload smoke"
echo "[nginx] kernel: $(uname -a)"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "[nginx] apt-get is required inside the VM" >&2
  exit 20
fi

apt-get update
apt-get install -y --no-install-recommends nginx curl ca-certificates

mkdir -p /var/www/html
printf '%s\n' 'anvil-nginx-ok' >/var/www/html/index.html

if command -v service >/dev/null 2>&1; then
  service nginx restart || true
fi

if ! pgrep -x nginx >/dev/null 2>&1; then
  nginx
fi

for attempt in $(seq 1 30); do
  if curl -fsS http://127.0.0.1/ | grep -q 'anvil-nginx-ok'; then
    echo "ANVIL_WORKLOAD_NGINX_READY"
    exit 0
  fi
  echo "[nginx] waiting for localhost response ($attempt/30)"
  sleep 1
done

echo "[nginx] failed to serve expected marker on localhost" >&2
if [ -f /var/log/nginx/error.log ]; then
  tail -50 /var/log/nginx/error.log >&2 || true
fi
exit 21
