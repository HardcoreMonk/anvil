#!/usr/bin/env bash
set -Eeuo pipefail

result_dir="/workspace/workloads/results"
log_file="$result_dir/go-http.log"
bench_file="$result_dir/bench.txt"
server_out="$result_dir/go-http-server.out"
pid_file="$result_dir/go-http-server.pid"
server_bin="/workspace/workloads/go-http-server"
server_src="/workspace/workloads/go-http-server.go"
requests="${ANVIL_WORKLOAD_REQUESTS:-50}"

mkdir -p "$result_dir"
exec > >(tee "$log_file") 2>&1

export DEBIAN_FRONTEND=noninteractive

echo "[go-http] starting Go HTTP workload"
echo "[go-http] kernel: $(uname -a)"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "[go-http] apt-get is required inside the VM" >&2
  exit 30
fi

if ! command -v curl >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends curl ca-certificates
fi

if ! command -v go >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends golang-go
fi

go version
go build -o "$server_bin" "$server_src"

if pgrep -f "$server_bin" >/dev/null 2>&1; then
  pkill -f "$server_bin" || true
  sleep 1
fi

nohup "$server_bin" >"$server_out" 2>&1 &
server_pid=$!
printf '%s\n' "$server_pid" >"$pid_file"
echo "[go-http] started server pid=$server_pid"

for attempt in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:18080/health | grep -q '"status":"ok"'; then
    echo "ANVIL_WORKLOAD_GO_HTTP_READY"
    break
  fi
  echo "[go-http] waiting for localhost response ($attempt/30)"
  sleep 1
done

if ! curl -fsS http://127.0.0.1:18080/health | grep -q '"status":"ok"'; then
  echo "[go-http] server did not become ready" >&2
  tail -50 "$server_out" >&2 || true
  exit 31
fi

ok_count=0
fail_count=0
start_ns="$(date +%s%N)"
for _ in $(seq 1 "$requests"); do
  if curl -fsS http://127.0.0.1:18080/ >/dev/null; then
    ok_count=$((ok_count + 1))
  else
    fail_count=$((fail_count + 1))
  fi
done
end_ns="$(date +%s%N)"
duration_ms=$(((end_ns - start_ns) / 1000000))
if [ "$duration_ms" -le 0 ]; then
  duration_ms=1
fi

{
  printf 'tool=vm-curl-loop\n'
  printf 'requests=%s\n' "$requests"
  printf 'ok=%s\n' "$ok_count"
  printf 'failed=%s\n' "$fail_count"
  printf 'duration_ms=%s\n' "$duration_ms"
} | tee "$bench_file"

if [ "$fail_count" -ne 0 ]; then
  echo "[go-http] loopback benchmark had failed requests" >&2
  exit 32
fi

echo "ANVIL_WORKLOAD_BENCH_DONE"
