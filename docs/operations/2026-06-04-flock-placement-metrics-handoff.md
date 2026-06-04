# Scheduler flock placement metrics 운영 인계

작성일: 2026-06-04

## 릴리즈 범위

- scheduler-aware `RuntimeRouter.CreateFlock` path에 flock placement aggregate metrics를
  추가했다.
- scheduler `/metrics`에 `anvil_scheduler_flock_placement_*` metrics를 노출한다.
- latency histogram은 `schedule`, `daemon_create`, `placement_save`, `total` phase를
  기록한다.
- metrics 기록은 best-effort다. metrics persistence 실패는 flock 생성의 user-facing
  결과를 바꾸지 않는다.

## 보안 경계

- metrics labels는 bounded enum인 `outcome`, `reason`, `phase`만 사용한다.
- `tenant_id`, `flock_id`, `vm_id`, host endpoint, daemon raw body, authorization
  header, `agent_token`은 metrics state와 output에 저장하지 않는다.
- loaded scheduler state의 histogram bucket key도 fixed bucket set과 `+Inf`로
  정규화한다.

## 검증

- `go test ./internal/anvilmcp -run 'TestPlacementStore.*FlockPlacementMetrics' -count=1`: PASS
- `go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1`: PASS
- `go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlockRecords.*Metrics|TestRuntimeRouterCreateFlockSchedulesByRoleCountAndRecordsPlacements|TestRuntimeRouterCreateFlockRejectsQuotaBeforeDaemonCall|TestRuntimeRouterCreateFlockReportsPlacementSaveFailureWithoutSecrets' -count=1`: PASS
- `go test ./internal/anvilmcp -count=1`: PASS
- `go test -race ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlockRecords.*Metrics|TestPlacementStore.*FlockPlacement' -count=1`: PASS
- `go test ./cmd/anvil-scheduler -count=1`: PASS
- `go test ./cmd/anvil-mcp -count=1`: PASS
- `go test ./... -count=1`: PASS
- `go build ./cmd/anvil-scheduler`: PASS
- `go build ./cmd/anvil-mcp`: PASS
- `go build ./cmd/goose-daemon`: PASS
- `git diff --check`: PASS

## 잔여 위험

- metrics aggregate는 `PlacementStoreState` JSON file에 저장된다. 기존 placement store의
  multi-process write 모델은 이번 작업에서 바꾸지 않았다.
- placement save failure metric은 같은 failing `PlacementStore`에 best-effort로 기록된다.
  persistence 자체가 degraded 상태면 해당 failure counter도 저장되지 않을 수 있다.
- host별 placement 편향은 아직 직접 노출하지 않는다.
- true cross-host flock placement는 별도 설계 범위다.
