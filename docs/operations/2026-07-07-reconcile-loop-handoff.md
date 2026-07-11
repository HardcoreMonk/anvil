# ReconcilePlacements 주기 배선 Handoff (reconcile-loop slice)

- 작성일: 2026-07-07
- 대상 branch: `feature/reconcile-loop` (PR #21)
- 상태: 구현·리뷰·게이트 완료. 설계:
  [`docs/superpowers/specs/2026-07-07-reconcile-placements-wiring-design.md`](../superpowers/specs/2026-07-07-reconcile-placements-wiring-design.md),
  계획: [`docs/superpowers/plans/2026-07-07-reconcile-placements-wiring.md`](../superpowers/plans/2026-07-07-reconcile-placements-wiring.md)
- 선행: [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](2026-07-06-cross-host-town-wall-handoff.md)
  Follow-Up "`ReconcilePlacements` 주기적 control loop 배선" — 이 slice로 CLOSED.

## 무엇이 배포됐나

`members_only` cross-host 모드의 `cmd/anvil-mcp` adapter가
`RuntimeRouter.ReconcilePlacements`를 **시작 직후 1회 + 주기(기본 60s)**로 자동
실행한다. daemon process restart로 사라진 town wall hub/relay flock 등록과
relay-token admission이 운영자 개입 없이 복구된다.

- config: `reconcile_interval`(yaml) / `ANVIL_MCP_RECONCILE_INTERVAL`(env,
  yaml보다 우선). `time.ParseDuration` 형식, 기본 `60s`, `0`=완전 비활성,
  음수·파싱 불가는 기동 시 거부.
- `ReconcilePlacements`는 전용 `reconcileMu`로 end-to-end 직렬화 — 주기 실행과
  수동 호출이 절대 겹치지 않는다 (`-race` 검증).
- `StartReconcileLoop(ctx, interval, logf)`: ctx 취소로 종료, 실패는 로그만
  남기고 루프 지속(adapter tool 동작 무차단), `interval <= 0`은 완전 no-op.
- 배선 게이트 `shouldStartReconcileLoop` = `members_only` && router && interval>0
  (테이블 테스트). 루프 ctx는 `server.Run`과 공유.
- 로그는 stderr(`log.Printf`) — stdout은 MCP stdio transport라 오염 금지.

## 보안/redaction

최종 whole-branch 리뷰가 잡은 결함: `ListVMs` 실패 시 `%w` 체인이
`*url.Error`(daemon 전체 URL)를 reconcile 로그로 누출 — redaction 하드 제약
위반이고 이 기능의 주 트리거(daemon 재시작/unreachable) 시나리오에서 발화.
`d5c7df0`이 `reconcileRoutedFlockWalls`와 동일 규율로 에러를 host 식별자로
bound하고, 테스트를 "원인 에러가 로그에 없어야 한다"는 redaction guard로
승격했다. relay token은 이 경로에 애초에 노출되지 않는다.

## Gate 결과

- 전체 유닛 suite `go test ./cmd/... ./internal/... -count=1` green (13 pkgs).
- 동시성 테스트 `-race` green. 4 빌드(goose-daemon/anvil-mcp/anvil-scheduler/
  ephemera-ctl) green. `git diff --check` clean. 전 커밋 trailer 없음.
- KVM e2e는 이 slice가 daemon 경로를 바꾸지 않아 비필수 (기존 게이트 유효).

## Known limitations / 운영 주의

- create 진행 중 주기 reconcile이 겹치면 spawn 직후 VM placement 항목이
  일시적으로 빠질 수 있다 — create의 후속 save가 재기록(자가 치유, spec 명시
  수용).
- 다중 adapter 인스턴스의 store save는 여전히 last-writer-wins (기존 성질,
  범위 밖).
- adapter가 꺼져 있거나 `0`으로 비활성화된 구성에서는 기존처럼 수동
  재등록(runbook 참조)이 필요하다.

## Next Action

- PR #21 머지 후 zone 인벤토리 동기화(release 단계).
- 다음 capability: cross-host `gtcall` 설계 착수.

## Follow-Up Tasks

- 직렬화 테스트 negative branch 100ms 휴리스틱은 설계 수용(false-negative
  방향) — 재설계 필요 없음, 기록만 유지.
- ~~bounded relay retry/buffer~~ — **CLOSED** (PR #23, 2026-07-08 bounded
  relay retry slice; 비동기 buffer는 비목표로 남아 mesh/수동 검증 이후
  재평가, 상세:
  [`docs/operations/2026-07-08-bounded-relay-retry-handoff.md`](2026-07-08-bounded-relay-retry-handoff.md)).
  home SPOF mesh 진화는 재선출 failover로 설계 확정 후 ~~구현은 수동
  multi-host 검증 통과 후로 보류~~ — **구현 완료** (2026-07-11,
  `feature/home-failover`, PR #33 → main `0feb9fb`). 실 2-daemon 수동
  검증(§6b)은 2026-07-11 수행 완료 — 전 세부 단계 PASS. 상세:
  [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md),
  [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
  SSE non-200 polish는 town wall handoff의 기존 follow-up 유지.
