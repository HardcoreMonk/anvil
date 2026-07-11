# Routed flock cross-host stack — 작업 묶음 handoff (PR #19~#26)

- 작성일: 2026-07-08
- 상태: **stack 완성·머지 완료. 대기 게이트 0건 — §6b 실 2-daemon 수동 검증까지 2026-07-11 PASS(전 항목 통과).**
- 목적: 2026-07-06~08에 걸친 cross-host stack 작업 전체의 현재 상태와 **명시적
  후속 작업 큐**를 한 문서로 고정한다. 세부는 각 slice handoff가 원문이다.

## 무엇이 main에 있나 (slice별 handoff 링크)

| PR | 내용 | handoff |
|---|---|---|
| #19 | cross-host shared Town Wall (home hub + relay) | [2026-07-06-cross-host-town-wall-handoff.md](2026-07-06-cross-host-town-wall-handoff.md) |
| #21 | adapter 주기 reconcile loop (daemon 재시작 자가 치유) | [2026-07-07-reconcile-loop-handoff.md](2026-07-07-reconcile-loop-handoff.md) |
| #22 | cross-host gtcall (2-hop + call_token, wall admit 결함 수정) | [2026-07-08-cross-host-gtcall-handoff.md](2026-07-08-cross-host-gtcall-handoff.md) |
| #23 | bounded relay retry (dial-실패 한정 3시도) | [2026-07-08-bounded-relay-retry-handoff.md](2026-07-08-bounded-relay-retry-handoff.md) |
| #24 | wall relay 에러 redaction 정합 + retry 하드닝 | (retry handoff Follow-Up CLOSED 항목 참조) |
| #20/#25 | 문서 currency sweep 1·2차 | — |
| #26 | 수동 검증 runbook 호스트 준비 체크리스트 | [2026-07-08-cross-host-manual-verification.md](2026-07-08-cross-host-manual-verification.md) |

**구현 완료(branch `feature/home-failover`, main 병합·PR 대기)**: **home
재선출 failover** —
[`docs/superpowers/specs/2026-07-08-home-failover-design.md`](../superpowers/specs/2026-07-08-home-failover-design.md)
(복제 없음, 단일 elector, wall 손실 수용 계약), 상세:
[2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md).
유닛 8종 + `-race` + KVM e2e(`scripts/anvil-cross-host-failover-e2e.sh`)
게이트 통과. 실 2-daemon 수동 검증(§6b)은 2026-07-11 수행 완료 — 전 세부 단계
PASS(기록: [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md)).

## Next Action (순서 고정 — 2026-07-10 갱신)

~~1. 서버 2대 세팅~~ / ~~2. 실 2-daemon 수동 검증 수행~~ — **완료 (2026-07-10)**.
결과: **부분 통과** — wall 양방향·gtcall 4방향·redaction·정리/revoke 전부 PASS,
**재시작 복구 FAIL** (결함 D1 회부). gtcall handoff MANUAL check는 CLOSED.
수행 기록: [2026-07-10-cross-host-verification-run-handoff.md](2026-07-10-cross-host-verification-run-handoff.md).

갱신된 순서:

1. ~~D1 fix slice~~ — **완료** (PR #30 + 잔여 D1b PR #31, 2026-07-10 당일).
2. ~~⑥ 재시작 복구 재검증~~ — **PASS** (양 daemon 동시 재시작 포함 전 방향).
   **검증 전체가 이로써 완전 통과 — "실 2-daemon 통합 검증 완료".**
3. ~~failover 구현 slice 착수 승인 요청~~ — **완료 (2026-07-11)**. 구현·
   유닛(8종)·`-race`·KVM e2e 게이트 통과, `feature/home-failover` branch에
   편입(main 병합은 §6b 통과 후). 상세:
   [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md).
4. ~~**실 2-daemon 수동 검증 §6b(home 재선출 failover) 수행**~~ — **완료
   (2026-07-11, 전 세부 단계 PASS)**. 서버 2대(192.168.1.19/.20, `0feb9fb`).
   기록: [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
   → **stack 대기 게이트 전부 CLOSED.**

## Follow-Up Tasks (전체 큐 — 우선순위순, 2026-07-11 갱신)

1. ~~수동 multi-host 검증~~ — **수행 완료** (부분 통과, 위 Next Action).
   신규 회부: **D1** 재시작 복구 결함 (주요, 새 1순위), **D2** delete
   cleanup_failed 오보고 (부차), **D3** ZFS diff-snapshot merge 오염 — 코드
   완화 필요 (부차, 운영 완화는 적용됨). 상세: run handoff.
2. ~~failover 구현 slice~~ — **완료** (2026-07-11, D1 수정·⑥ 재검증 통과 후
   승인). 상세:
   [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md).
   ~~신규 회부(새 1순위): 실 2-daemon 수동 검증 §6b failover 시나리오~~ —
   **완료 (2026-07-11, PASS)**. 신규 결함 없음(정리 단계에서 기존 D2 재현만).
3. ~~SSE relay non-200 content-type polish~~ — **CLOSED** (`b1c8c8c`,
   2026-07-10): wall handoff Follow-Up 참조.
4. cross-host broadcast fan-out — 계속 비목표 (필요 대두 시 별도 설계).
5. 소소한 잔여 (각 handoff Follow-Up 기록): e2e 포트 공유(wall/gtcall/failover
   동시 실행 불가 — 기존 특성), 비동기 relay buffer 재평가(failover 구현
   완료로 재평가 가능 — §6b 수동 검증 통과 후 우선순위 판단), cross-flock
   isolation 커버리지 테스트(Option B, home failover handoff Follow-Up 4번
   참고).

## zone 연동

- zone 후속 작업 단일 소스: `~/projects/claude-zone/docs/FOLLOWUP.md`의
  **P3-09** (트리거: 서버 2대 세팅 완료).
- zone 인벤토리: `ops/projects.yaml` anvil 주석, `wiki/entities/anvil-project.md`
  — PR별 동기화 완료 상태.
