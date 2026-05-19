# Anvil VM Workload E2E Design

## 목표

anvil의 full KVM E2E를 VM lifecycle smoke에서 실제 workload 검증으로 확장한다.
새 테스트는 VM 내부에 서비스를 설치하고 기동한 뒤, VM 내부와 host 양쪽에서 접근성과
기초 성능을 확인한다.

1차 workload는 두 가지다.

- `nginx`: Debian package 설치, service 기동, HTTP 응답, host-to-VM 접근 검증
- Go HTTP server: 작은 HTTP server 실행, loopback 및 bridge 경유 응답, 기초 부하 측정

## 비목표

- 1차 범위에서는 guest agent에 범용 `/exec` endpoint를 추가하지 않는다.
- benchmark 결과를 제품 성능 보증치로 선언하지 않는다. 이 테스트의 목적은 회귀 감지와
  host/VM/network 경로의 상대 비교다.
- real LLM 품질 평가는 별도 `real-llm` E2E로 유지한다. workload E2E는 가능한 한
  deterministic한 script와 marker output을 중심으로 검증한다.
- 장시간 soak, multi-host scheduler, public internet exposure는 이번 범위에서 제외한다.

## 현재 제약

현재 VM 내부 실행 surface는 다음 두 endpoint다.

- `PUT/GET /vms/{vm_id}/workspace`
- `POST /vms/{vm_id}/tasks`

`/tasks`는 guest `goose-agent`가 `/usr/local/bin/goose run -i -`를 실행하는 구조다.
따라서 1차 구현은 host가 workload script를 `/workspace`로 업로드하고, task prompt는
"업로드된 script를 그대로 실행하고 marker를 출력하라"는 제한된 역할만 맡긴다.

이 방식은 raw shell API보다 보안 표면이 작고 기존 API와 호환된다. 대신 LLM/provider
상태가 task 시작 단계에 영향을 줄 수 있으므로, benchmark 수치는 script output marker와
artifact를 기준으로만 판정한다.

## 아키텍처

새 테스트는 기존 `e2e_test.sh`와 분리한다.

- `e2e_test.sh`: 기존 full KVM lifecycle, snapshot, flock smoke 유지
- `scripts/vm-workload-e2e.sh`: 서비스 설치, 기동, 성능 baseline 검증
- `scripts/workloads/nginx-smoke.sh`: VM 내부 nginx 설치와 HTTP smoke
- `scripts/workloads/go-http-server.go`: VM 내부 테스트 HTTP server
- `scripts/workloads/go-http-bench.sh`: Go server 실행과 간단 benchmark

`scripts/vm-workload-e2e.sh`는 daemon 시작, VM 생성, workload 업로드, task 실행,
host-side 검증, artifact 수집, cleanup을 소유한다.

## 실행 흐름

1. Host preflight를 수행한다.
   - root 권한
   - `/dev/kvm`
   - `curl`, `jq`
   - 선택 tool: `ab`, `wrk`, `hey`
2. `anvil-daemon`을 시작하거나 명시된 기존 daemon에 연결한다.
3. `POST /vms`로 workload VM을 생성한다.
4. workload 파일을 `/workspace/workloads/` 아래로 업로드한다.
5. `POST /vms/{vm_id}/tasks`로 VM 내부 script 실행을 요청한다.
6. VM 내부 script는 다음 marker를 출력한다.
   - `ANVIL_WORKLOAD_NGINX_READY`
   - `ANVIL_WORKLOAD_GO_HTTP_READY`
   - `ANVIL_WORKLOAD_BENCH_DONE`
7. Host는 `guest_ip`로 다음 endpoint를 직접 확인한다.
   - `http://<guest_ip>:80/`
   - `http://<guest_ip>:18080/health`
8. 가능한 경우 host-side benchmark를 실행한다.
9. 결과를 artifact directory에 저장한다.
10. VM을 삭제하고 active VM 수와 주요 stale resource를 확인한다.

Go HTTP workload는 VM 내부에 `go`가 있으면 그대로 사용한다. 없으면 `apt-get update`와
`apt-get install -y golang-go`를 수행한다. 이 단계는 package manager, DNS, outbound
network를 함께 검증한다.

## Artifact

각 실행은 `/tmp/anvil-workload-e2e-<timestamp>/` 아래에 결과를 남긴다.

- `summary.json`: pass/fail/skipped, VM ID, guest IP, timing, benchmark 요약
- `task-output.json`: `/tasks` raw response
- `nginx.log`: nginx 설치, 기동, curl 결과
- `go-http.log`: Go server build/run 결과
- `bench.txt`: VM 내부 및 host-side benchmark output
- `daemon.log`: 테스트가 daemon을 직접 시작한 경우 daemon log

Artifact에는 provider token, API key, agent token, control-plane token 값을 기록하지
않는다.

## 성능 지표

1차 pass/fail은 절대 성능 임계치보다 기능 회귀 감지에 둔다.

- nginx host-to-VM HTTP status가 `200`
- Go HTTP server host-to-VM HTTP status가 `200`
- VM 내부 loopback request 성공
- benchmark tool이 있으면 request count, duration, requests/sec 또는 latency summary 수집
- benchmark tool이 없으면 `skipped`로 기록하고 전체 테스트는 실패시키지 않음

후속 버전에서 같은 host baseline을 저장하고 p50/p95 latency 또는 RPS 변화율 기준을
추가할 수 있다.

## 오류 처리

오류 reason은 한 덩어리로 뭉치지 않고 다음 범주로 분리한다.

- `preflight_failed`
- `daemon_unavailable`
- `vm_create_failed`
- `workspace_upload_failed`
- `task_failed`
- `apt_or_dns_failed`
- `nginx_start_failed`
- `nginx_host_probe_failed`
- `go_build_failed`
- `go_server_start_failed`
- `go_host_probe_failed`
- `benchmark_failed`
- `cleanup_failed`

cleanup은 best-effort로 항상 실행한다. cleanup이 실패해도 artifact를 남기고, 어떤
resource가 남았는지 `summary.json`에 기록한다.

## 보안

- 테스트 script는 control-plane과 guest agent token 값을 출력하지 않는다.
- `summary.json`에는 token, prompt 전문, secrets 파일 내용을 저장하지 않는다.
- workload 파일은 `/workspace/workloads/` 아래 relative path로만 업로드한다.
- host에서 VM으로 접근하는 probe는 VM private bridge IP만 사용한다.
- 범용 `/exec` endpoint는 1차 범위에 포함하지 않는다.

## 테스트 전략

문서화된 pass 조건은 다음이다.

- VM 생성 성공
- workload 파일 업로드 성공
- VM 내부 nginx 설치와 기동 성공
- VM 내부 `curl localhost` 성공
- host에서 VM nginx 접근 성공
- VM 내부 Go HTTP server 실행 성공
- host에서 Go HTTP server 접근 성공
- benchmark 결과 수집 또는 명시적 skip 기록
- VM 삭제 후 active VM 0개 확인
- artifact summary 생성

`go test ./...`는 기존 unit/integration regression gate로 유지한다. workload E2E는
root/KVM/network가 필요한 host-only 검증으로 runbook에 별도 명령으로 문서화한다.

## 후속 확장

- Test-only `/exec` endpoint 또는 signed command runner 추가
- `--suite nginx|go-http|all` option
- `--reuse-daemon` option
- baseline 저장과 regression threshold
- Redis/PostgreSQL workload 추가
- 장시간 soak와 concurrent VM workload
