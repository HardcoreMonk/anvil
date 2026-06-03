# Scheduler 운영 강화 운영 인계

작성일: 2026-06-02

## 릴리즈 범위 (Release Scope)

- scheduler service에 `GET /metrics` endpoint를 추가했다.
- scheduler smoke harness가 `/metrics`와
  `anvil_scheduler_control_loop_running` line을 검증한다.
- `cmd/anvil-scheduler` full-process integration test를 추가해 hosts file bootstrap,
  stale state override, fake daemon `/health`, scheduler `/control-loop/status`,
  `/schedule/spawn`, `/metrics` 경로를 검증한다.
- scheduler 운영 문서와 release note를 scheduler `/metrics`와 실제 systemd 검증
  상태에 맞춰 갱신했다.

## 검증 (Verification)

- `go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1`: PASS
- `go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1`: PASS
- `go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1`: PASS
- `go test ./... -count=1`: PASS
- `go build ./cmd/goose-daemon`: PASS
- `go build ./cmd/anvil-mcp`: PASS
- `go build ./cmd/anvil-scheduler`: PASS
- `bash -n scripts/anvil-scheduler-smoke.sh`: PASS
- `bash -n scripts/install-anvil-scheduler-systemd.sh`: PASS
- `git diff --check`: PASS
- `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`: PASS

실제 systemd 검증 결과:

- `anvil-scheduler.service`: active running
- scheduler bind address: `127.0.0.1:3010`
- smoke result: `anvil scheduler smoke passed`
- installed env path: `/etc/anvil/anvil-scheduler.env`
- installed unit path: `/etc/systemd/system/anvil-scheduler.service`
- installed binary path: `/usr/local/bin/anvil-scheduler`

## 감사 (Audit)

- scheduler metric namespace는 `anvil_scheduler_*`다.
- scheduler metrics에는 `agent_token`, daemon raw response, authorization header,
  host endpoint, snapshot metadata를 넣지 않는다.
- scheduler service는 이번 변경에서도 자체 인증 계층을 추가하지 않았다. 운영 노출은
  loopback/private network 또는 reverse proxy policy 뒤에서 수행한다.
- 이번 작업은 scheduler 운영 surface와 검증 경로만 다룬다. VM lifecycle,
  Firecracker/KVM runtime, snapshot/restore storage semantics, flock placement는
  변경하지 않았다.

## 차단 요소 (Blockers)

- 없음.

## 경고 (Warnings)

- 실제 systemd 검증은 `/etc/anvil`, `/var/lib/anvil`,
  `/usr/local/bin/anvil-scheduler`, `anvil-scheduler.service`를 변경했다.
- `cmd/anvil-scheduler` process integration test는 test 전용 loopback port를 예약한 뒤
  닫고 child process가 bind한다. 다른 process가 그 사이 같은 port를 잡는 극히 작은
  race가 남아 있지만, code quality review에서 local Go integration test의 허용 가능한
  residual risk로 분류했다.

## 잔여 위험 (Residual Risk)

- Scheduler `/metrics`는 scheduler service 자체와 동일하게 unauthenticated다. 외부
  노출 환경에서는 loopback/private network 또는 reverse proxy policy로 보호해야 한다.
- Scheduler `/metrics`는 초기 alert surface다. poll/reconcile latency와 failure count
  metric은 아직 구현되지 않았고 후속 후보로 남아 있다.
- Process integration test는 KVM, Firecracker, real VM lifecycle을 실행하지 않는다.

## 현재 lifecycle 단계 (Current Lifecycle Stage)

검증과 review를 마친 뒤 scheduler 운영 강화 작업은 operate 단계에 진입했다.

## 다음 작업 (Next Action)

- 후속 item 4인 cross-host snapshot replication은 별도 design spec에서 시작한다.
- upstream ephemera `v0.4.0`-`v0.5.0` adoption review는 scheduler operations
  hardening과 섞지 않고 별도 sync/adoption 작업으로 진행한다.

## 후속 작업 (Follow-Up Tasks)

- scheduler poll/reconcile latency metric 추가 여부 결정
- scheduler poll/reconcile failure count metric 추가 여부 결정
- cross-host snapshot replication design spec 작성
- scheduler-aware cross-host flock placement design spec 작성
