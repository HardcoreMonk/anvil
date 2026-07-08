# Home 재선출 failover 설계 (복제 없음, wall 손실 수용)

- 작성일: 2026-07-08
- 상태: **설계 확정 — 구현 보류** (수동 multi-host 검증
  [`docs/operations/2026-07-08-cross-host-manual-verification.md`](../../operations/2026-07-08-cross-host-manual-verification.md)
  통과 후 별도 승인으로 구현 착수)
- 선행: cross-host wall/gtcall/reconcile-loop/bounded-retry 슬라이스
  (home SPOF 1차 수용의 해소 경로)
- 관련 코드: `internal/anvilmcp/runtime_router.go`(`reconcileRoutedFlockWalls`),
  `internal/anvilmcp/routed_flock.go`, `internal/anvilmcp/placement_store.go`

## 문제

routed flock의 wall과 cross-host call은 home host(hub)가 단일 장애점이다.
home daemon이 죽으면 wall 게시/조회와 cross-host 호출이 전부 502가 되고,
회복은 home 프로세스 부활에만 의존한다(reconcile이 등록은 힐링하지만 host
자체가 죽으면 무력).

## 결정 (사용자 확정)

**hub 복제(mesh)가 아니라 재선출 failover.** wall 과거 기록의 손실을
명시적으로 수용하는 대신, 복제 일관성 문제(seq 단조성, 이중 쓰기, 복제 지연,
분산 합의)를 전부 제거한다. elector가 단일 control plane(adapter)이므로
split-brain이 구조적으로 불가능하다.

## 감지와 선출

- **주체**: adapter의 기존 reconcile 루프(`ANVIL_MCP_RECONCILE_INTERVAL`,
  기본 60s). 새 프로세스·데몬 없음 — PlacementStore, daemon 주소록, 토큰을
  이미 소유한 유일한 지점.
- **감지**: reconcile의 home 대상 관측(등록 re-POST 등)이 **연속
  `homeFailureThreshold`회(상수, 기본 3)** dial-계열로 실패하면 failover
  트리거. 카운터는 flock 단위, 성공 시 리셋. 임계 상수는 설정화하지 않는다
  (YAGNI — bounded retry와 동일 방침).
- **선출(결정적)**: 생존 host(직전 reconcile에서 도달 가능 관측) 중
  `record.Agents` 순서상 첫 host, 구 home 제외. 같은 입력 → 같은 결론.
  후보가 없으면 no-op(현행 502 지속, 다음 주기 재평가).

## 전환 절차 (기존 배관 재사용)

1. `record.HomeHost = 새 host` 갱신 + `SaveRoutedFlockAndPlacements` 영속 —
   **원자적 전환점** (이후 모든 reconcile 재등록의 기준이 바뀜).
2. 새 home에 `RegisterDistributedFlock` — VMID/Addr roster + relay/call token
   (reconcile 재등록 코드 그대로; 새 home에 새 빈 `TOWN_WALL.log` 생성됨).
3. 전 member(새 home 제외)에 `RegisterRelayFlock` — 새 HomeAddr + 두 토큰 +
   host-local agents.
4. 구 home에 best-effort `DELETE /flocks/{id}` (도달 불가면 skip — 어차피
   다운; 부활 시 stale hub는 아무도 참조하지 않고, reconcile은 새 HomeHost
   기준으로만 재주입).
- **토큰 불변**: relay/call token은 flock 단위 그대로 재사용 — guest 주입
  토큰(`.ephemera-cp-token`)이 바뀌지 않으므로 **guest 무중단·무개입**.
- **부분 실패 수렴**: 어느 단계가 실패해도 다음 reconcile 주기가 idempotent
  재시도(기존 heal 규율). 전환 중간 상태도 HomeHost 영속값 기준으로 수렴.

## Wall 손실 semantics (명시 계약)

- failover 후 wall은 새 home의 빈 log에서 seq를 재시작한다. **이전 기록은
  구 home 디스크에 남지만 새 wall로 병합되지 않는다.**
- agent 관점: `gtwall`/history는 계속 동작하되 과거 메시지가 사라진 것으로
  보인다 — flock 운영 문서에 이 계약을 명시한다.

## 경계 사례

- 생존 후보 0 (모든 member host 다운 또는 단일-host flock) → no-op.
- 자동 fail-back 없음 — 구 home이 부활해도 새 home 유지 (수동 개입으로만
  재편). flap 방지.
- 다중 adapter 인스턴스: store는 last-writer-wins(기존 성질)이나 선출 규칙이
  결정적이라 동일 결론으로 수렴; 토큰 map은 disk-권위 merge(PR #23 slice)로
  보호됨.
- call 경로: hub roster가 새 home으로 이동(재등록)하므로 2-hop 해석 즉시
  복원. 전환 창(최대 ~threshold×interval + 전환 시간) 동안의 call/post는
  기존대로 502 + bounded retry.

## 테스트 (구현 시, TDD)

- 유닛(fake daemon): 연속 K회 dial-실패 → 재선출 발화 — 새 home
  `distributedCalls` + 전 member `relayReq.HomeAddr` 갱신 + HomeHost 영속
  단언; K-1회는 no-op; 성공 개재 시 카운터 리셋; 후보 0 no-op; 전환 부분
  실패 → 다음 주기 수렴; 구 home 부활 시 hub 미재주입.
- KVM e2e: stub home kill → 두 번째 stub으로 전환 관측(멤버 daemon의 relay
  갱신 wire 캡처), guest gtwall이 전환 후 성공.
- 수동 multi-host 검증 절차에 failover 시나리오 추가(§6 확장).

## 문서 반영 (구현 시)

- ADR_INDEX/PUBLIC_RELEASE_BOUNDARY의 SPOF 서술을 failover로 갱신,
  wall-손실 계약 명시. CONTEXT 완료 목록, runbook(전환 창·수동 fail-back
  절차), slice handoff.

## 비목표

- wall log 복제·무손실 failover, wall 병합, 자동 fail-back.
- 다중 adapter 간 분산 선출(단일 control plane 전제 유지).
- 감지 임계·선출 규칙의 설정화.
