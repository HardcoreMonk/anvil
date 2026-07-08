# Routed flock cross-host stack — 작업 묶음 handoff (PR #19~#26)

- 작성일: 2026-07-08
- 상태: **stack 완성·머지 완료. 대기 게이트 1건 — 실 2-daemon 수동 검증.**
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

설계만 확정(구현 보류): **home 재선출 failover** —
[`docs/superpowers/specs/2026-07-08-home-failover-design.md`](../superpowers/specs/2026-07-08-home-failover-design.md)
(복제 없음, 단일 elector, wall 손실 수용 계약).

## Next Action (순서 고정)

1. **Ubuntu 26.04 서버 2대 세팅** — 검증 runbook의
   "호스트 준비 체크리스트" 절차대로 (Go/패키지/네트워크/KVM + **서버별
   단일 host smoke 필수**).
2. **실 2-daemon 수동 검증 수행** — runbook ①~⑧ (wall 양방향, gtcall 4방향,
   재시작 복구, redaction 스팟). 이것이 home 측 실수신·2번째 hop의 최종
   증거이자 failover 구현의 게이트다.
3. 판정에 따라:
   - **통과** → runbook 상태 갱신 + gtcall handoff MANUAL check CLOSED +
     **failover 구현 slice 착수 승인 요청** (spec 확정본 기준 writing-plans →
     SDD).
   - **실패** → 증상·로그를 runbook에 첨부, slice 결함으로 회부.

## Follow-Up Tasks (전체 큐 — 우선순위순)

1. 수동 multi-host 검증 (위 Next Action — 유일한 차단 게이트).
2. failover 구현 slice (검증 통과 후, 별도 승인).
3. SSE relay non-200 content-type polish (wall handoff Follow-Up 잔존 —
   cosmetic).
4. cross-host broadcast fan-out — 계속 비목표 (필요 대두 시 별도 설계).
5. 소소한 잔여 (각 handoff Follow-Up 기록): e2e 포트 공유(wall/gtcall 동시
   실행 불가 — 기존 특성), 비동기 relay buffer 재평가(failover 이후).

## zone 연동

- zone 후속 작업 단일 소스: `~/projects/claude-zone/docs/FOLLOWUP.md`의
  **P3-09** (트리거: 서버 2대 세팅 완료).
- zone 인벤토리: `ops/projects.yaml` anvil 주석, `wiki/entities/anvil-project.md`
  — PR별 동기화 완료 상태.
