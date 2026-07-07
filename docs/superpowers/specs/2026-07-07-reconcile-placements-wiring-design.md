# ReconcilePlacements 주기적 배선 설계 (adapter 내 reconcile loop)

- 작성일: 2026-07-07
- 상태: 설계 확정 (구현 전)
- 선행: [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](../../operations/2026-07-06-cross-host-town-wall-handoff.md)
  Follow-Up "`ReconcilePlacements` 주기적 control loop 배선"
- 관련 코드: `internal/anvilmcp/runtime_router.go` (`ReconcilePlacements`,
  `reconcileRoutedFlockWalls`), `cmd/anvil-mcp/main.go` (router 조립부),
  `internal/anvilmcp/config.go`

## 문제

`RuntimeRouter.ReconcilePlacements`는 daemon별 `ListVMs` 관측으로 VM placement를
재구성하고, cross-host shared Town Wall의 hub/relay flock 등록과 relay-token
admission을 재수행(`reconcileRoutedFlockWalls`)한다. daemon이 process restart로
in-memory flock 등록과 relay-token admission을 잃었을 때 이 함수가 유일한 자동
복구 경로다. 그러나 현재 production 호출자가 없어(유닛 테스트만 exercise) daemon
재시작 후 routed flock의 공유 wall은 운영자가 개입하기 전까지 깨진 채 남는다.

## 결정

**reconcile 루프는 `cmd/anvil-mcp` adapter 프로세스 안에 둔다.** adapter가
`members_only` cross-host 모드 + persistent `PlacementStore`로 기동할 때, 시작
직후 1회 + 주기(기본 60초)로 `ReconcilePlacements`를 실행하는 background
goroutine을 시작한다.

근거:

- wall 등록·relay token·daemon client·`PlacementStore` 전부 adapter 프로세스의
  소유물이다. 프로세스 경계를 넘지 않고 코드 추가가 최소다.
- 운영 전제 확인 결과 adapter는 사실상 상시 실행이다(IronClaw 세션이 거의 항상
  떠 있음) — 힐링 공백이 없다.
- hub/relay 등록 endpoint는 idempotent라(중복 id는 `409`로 보호) 반복 재등록이
  안전하고, 다중 adapter 인스턴스가 동시에 돌아도 daemon 쪽 상태는 수렴한다.

기각한 대안:

- **scheduler 서비스 control loop 편입**: 장기 systemd 서비스라는 장점은 있으나
  scheduler 프로세스에 wall 재등록·relay token 접근을 이식해야 해 관심사가
  침범되고, adapter와 state 파일을 두 프로세스가 쓰게 되어 잠금 없는 store의
  last-writer-wins race를 악화시킨다.
- **시작 시 1회 + lazy heal만**: 세션 도중 daemon이 재시작하면 다음 adapter
  재시작/작업 전까지 wall이 깨진 채 남는다. 채택안이 시작 1회 실행을 포함해 이
  한계를 흡수한다.

## 아키텍처

- `RuntimeRouter.StartReconcileLoop(ctx context.Context, interval time.Duration)`
  신설 (`internal/anvilmcp/runtime_router.go`):
  - goroutine 하나. 시작 직후 1회 `ReconcilePlacements(ctx)` 실행, 이후
    `time.Ticker(interval)` 주기 실행. `ctx` 취소 시 ticker 정리 후 종료.
  - 별도 `reconcileMu sync.Mutex`로 reconcile 실행 자체를 직렬화한다 — 주기
    실행과 수동/테스트 호출이 겹쳐도 동시 실행이 없다. (`r.mu`는 placement map
    보호용으로 그대로 둔다.)
- 배선: `cmd/anvil-mcp/main.go`의 router 조립부(`members_only` + persistent
  state 조건이 이미 판정되는 지점)에서 `interval > 0`이면 루프를 시작한다.
  프로세스 수명 signal ctx에 연결해 종료 시 goroutine이 정리된다.

## 구성 계약

- env `ANVIL_MCP_RECONCILE_INTERVAL`, config yaml `reconcile_interval`
  (기존 `internal/anvilmcp/config.go` 패턴 동일 — env가 yaml을 override).
- 값은 `time.ParseDuration` 형식(`60s`, `5m` 등).
- **기본값 `60s`** — `members_only` 모드면 별도 설정 없이 켜진다.
- **`0` = 완전 비활성**(시작 1회 실행 포함 안 함, 현행 동작 보존 스위치).
- 음수·파싱 불가 값은 기동 시 설정 오류로 거부(기존 config validation 패턴).
- `members_only` 모드가 아니면 interval 값과 무관하게 루프를 시작하지 않는다
  (routed flock registry가 없는 구성에서는 대상이 없다).

## 에러 처리·보안

- reconcile 실패는 stderr 경고 로그로만 남기고(adapter의 기존 로깅 관례를 따름)
  루프는 계속 돈다 — adapter의 MCP tool 동작을 절대 차단하지 않는다.
- 로그에는 flock/host 식별자만 남긴다. relay token과 daemon 주소는 노출하지
  않는다 — `reconcileRoutedFlockWalls`가 이미 에러 문자열을 이 규율로 bound하고
  있으며, 루프가 추가하는 로그도 같은 규율을 따른다.
- 신규 네트워크 표면 없음: 루프는 기존 daemon client 호출(`ListVMs`,
  `RegisterDistributedFlock`, `RegisterRelayFlock`)만 주기화한다.

## 알려진 한계

- `ReconcilePlacements`의 `ReplaceVMPlacements`는 daemon 관측값으로 VM placement를
  통째 교체한다. flock create 진행 중 주기 reconcile이 겹치면 spawn 직후·daemon
  목록 반영 전 VM의 placement 항목이 일시적으로 빠질 수 있다. create가 진행되며
  `SaveRoutedFlockAndPlacements`가 재기록하므로 자가 치유되고, 이 성질은 기존
  수동 호출에도 동일하다(이번 슬라이스는 호출 주기화만 한다). create-reconcile
  직렬화는 create가 VM spawn으로 장시간 걸리는 만큼 루프를 과도하게 블록하므로
  채택하지 않는다.
- 다중 adapter 인스턴스가 같은 state 파일로 동시에 돌면 store save는 여전히
  last-writer-wins다(기존 성질, 이번 슬라이스 범위 밖).

## 테스트 (TDD, RED→GREEN)

- 설정: 기본 `60s` / `0` 비활성 / 커스텀 값 / 형식 오류·음수 거부.
- 루프: counting fake daemon으로 —
  - 시작 직후 1회 + ticker 주기 실행을 관측한다(짧은 interval).
  - `ctx` 취소 시 루프가 정지한다.
  - reconcile이 에러를 반환해도 루프가 계속 돈다.
  - `reconcileMu`로 동시 실행이 없다(수동 호출과 겹침 시나리오).
- `members_only`가 아닌 구성에서 루프가 시작되지 않는다.
- 기존 `ReconcilePlacements`/`reconcileRoutedFlockWalls` 테스트는 무변경 유지.

## 문서 반영 (구현 PR에 포함)

- `CONTEXT.md` 고정 런타임 계약에 `ANVIL_MCP_RECONCILE_INTERVAL` 1행.
- `README.md` adapter env 표에 동일 항목.
- `docs/operations/runbook.md`에 daemon 재시작 후 자동 힐링 언급 1-2줄.
- `docs/operations/2026-07-06-cross-host-town-wall-handoff.md` Follow-Up 항목은
  구현 완료 시 CLOSED 표기.

## 비목표

- scheduler 서비스로의 reconcile 이식, store 파일 cross-process 잠금.
- reconcile 결과의 metrics/audit 표면 신설(현행 slog로 충분 — 필요해지면 별도
  슬라이스).
- backoff/jitter(고정 주기 60s로 충분 — daemon 부하는 host당 `ListVMs` 1회 +
  routed flock당 등록 재발행 수준).
