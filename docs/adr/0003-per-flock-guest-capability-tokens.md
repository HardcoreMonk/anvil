# ADR-0003: flock guest 자격증명을 per-flock 능력 토큰으로 통일

> **상태:** accepted
> **날짜:** 2026-08-06
> **대상:** anvil downstream repository (`cmd/goose-daemon`, `internal/orchestrator`)

---

## 맥락

flock member VM 안의 goose agent는 host daemon으로 되돌아오는 호출을
`/root/.ephemera-cp-token`에 주입된 bearer로 인증한다. 그 호출 지점은 정확히
두 곳이다.

| 사용처 | 호출 경로 |
|---|---|
| in-VM townwall forwarder (`gtwall`) | `POST /flocks/{flock_id}/post` |
| `gtcall` | `POST /flocks/{flock_id}/call` |

두 flock kind가 이 자격증명을 서로 다르게 조달해 왔다.

- **routed flock**(cross-host)은 2026-07-07 이래 per-flock `relay_token`을
  쓴다. adapter가 발급하고 daemon이 `cp.relayTokens`에 등록하며,
  `authMiddleware`가 요청 경로에서 flock id를 뽑아 **그 flock의** 토큰과만
  상수시간 비교한다.
- **local flock**(단일 host)은 daemon의 **운영자 control-plane bearer 원본**을
  그대로 주입해 왔다. `APIClient`에는 scope 필드가 없으므로 이 값은
  `POST /vms`, `DELETE /vms/{id}`, `/snapshots/*`, `/tenants/*`, `/config/*`를
  포함한 **모든** control-plane 경로를 연다.

즉 local flock guest는 자신이 쓰지도 않는 전권을 보유해 왔다. 최소권한 원칙의
교과서적 위반이며, 동시에 **고칠 배관이 이미 다 있다**는 뜻이기도 하다 —
`relayGuestPathFlockID` admission 블록은 flock kind를 전혀 검사하지 않고,
wall sub-path(`post|wall|wall/history`)와 `call`을 **한 토큰으로 함께** admit
한다. 위 표의 두 사용처가 정확히 그 범위다.

`cmd/micro-init`은 권한 강등을 하지 않으므로 VM 안 코드는 root로 돈다. 따라서
guest 내부에서 파일 모드는 경계가 아니고, 주입되는 **토큰의 등급 자체**가
유일한 경계다. 이 slice는 guest 내부 root 문제를 풀지 않는다 — 별도 결정으로
남긴다.

---

## 결정

**local flock에도 per-flock guest 능력 토큰을 발급해, 두 flock kind의 토큰
모델을 하나로 통일한다.** `authMiddleware`의 admission 규칙은 **바꾸지 않는다**
— 이미 옳다. 바뀌는 것은 *무엇을 주입하는가*뿐이다.

### 메커니즘

1. **발급·등록** — flock 생성 시 `crypto/rand` 32바이트(hex 64자) 토큰을
   발급해 `cp.setRelayToken(flockID, T)`로 admission에 등록한다. routed flock이
   이미 하는 것과 같은 저장소·같은 비교 경로다.

2. **영속화는 별도 파일** — flock 디렉토리 안 `guest-token`에 atomic
   tmp+rename으로 쓰고, 모드는 **호출 지점에서 명시적 0600**이다(프로세스
   umask에 맡기지 않는다). `FlockMetadata`에 넣지 않은 이유는 그 구조체가
   "admission secret은 여기 절대 영속되지 않는다"는 **무조건 불변식**을 지고
   있고 테스트가 그것을 강제하기 때문이다. 토큰을 넣으면 불변식이 "routed
   토큰은 없지만 local 능력 토큰은 있다"는 **조건부**로 바뀌고, 가드가 토큰
   종류를 구분할 줄 알아야 성립한다. 조건부 불변식은 부패한다. 분리는
   `storage.VMState.AgentToken`과 `PlacementStore`의 토큰 맵이 이미 따르는
   선례이기도 하다.

3. **시작 시 재수화** — `cp.relayTokens`는 in-memory 전용이라 매 프로세스마다
   빈 맵이다. `FlockManager.LoadFromDisk`는 `ControlPlane`을 참조하지 못해
   admission 맵을 채울 수 없다. routed flock이 이 공백을 넘기는 유일한 이유는
   **외부 구동자**(adapter reconcile 재-POST)인데 **local flock에는 그런
   구동자가 없다.** 따라서 daemon 시작 경로에서 `LoadFromDisk` **직후**
   복원된 local flock을 순회해 영속된 토큰을 `setRelayToken`으로 되꽂는 단계를
   함께 낸다. 이 단계가 없으면 재시작 후 첫 member spawn이 **빈 토큰**을
   주입한다 — 구 모델(`cp.clients`는 매 시작 설정에서 다시 읽혀 재시작을 공짜로
   견딘다) 대비 명백한 회귀다.

4. **주입** — member spawn 3개 지점(`spawnVMForFlock` · `restartAgent` ·
   `changeFlockAgentRole`)이 운영자 bearer 대신 그 flock의 능력 토큰을 넣는다.
   `spawnVMForFlock`이 create와 add-agent 양쪽을 서비스하므로 "생성 경로만
   고치면 된다"는 오해가 쉽지만, 셋을 다 덮지 않으면 재시작·역할변경된 member가
   조용히 운영자 bearer로 되돌아간다.

5. **`ControlPlaneTokenManaged=false`** — 이 플래그는 "이 guest의 CP 토큰은
   daemon 자신의 운영자 bearer이므로 SIGHUP이 회전시켜도 된다"는 **출처 기록**
   이다. 능력 토큰은 그것이 아니다. `true`로 두면 다음 SIGHUP이 능력 토큰을
   운영자 bearer로 덮어써 이 결정이 조용히 무효화된다.

6. **폐기** — flock 삭제 시 admission에서 제거하고 토큰 파일도 지운다. flock
   디렉토리는 Town Wall을 감사 산출물로 남기려고 **삭제되지 않으므로**, 토큰
   파일 제거를 디렉토리 제거에 맡길 수 없다.

7. **빈 토큰은 fail-closed** — 주입 시점에 토큰이 없으면 운영자 bearer로
   **폴백하지 않는다**. Error 로그를 남기고 빈 값을 주입한다(provisioner가
   파일을 아예 쓰지 않는다 — 저하되지만 안전). auth 비활성 모드에서는 구
   모델도 빈 문자열을 반환했으므로 관측 가능한 동작이 동일하다.

### 무중단 업그레이드

이 변경 **이전에** spawn된 VM은 운영자 bearer를 갖고 있고 `CPTokenManaged=true`가
`state.json`에 남아 있으므로 계속 SIGHUP 회전을 받는다. 이후 spawn되는 VM은
`false`라 받지 않는다. 운영자 bearer는 `cp.clients` 경로로 **계속 admit되므로**
업그레이드 도중에도 구세대 guest가 깨지지 않는다. 회전 대상 집합은 VM이 교체
되면서 자연히 말라붙는다 — 마이그레이션도, 버전 게이트도 없다.

`propagateCPTokenToVMs`와 vsock `SET_CP_TOKEN`은 구세대 VM이 남아 있는 동안
유지하고, 제거는 별도 slice로 미룬다.

---

## 기각 대안: member마다 별도 토큰

add-agent가 member마다 새 능력 토큰을 발급하는 안을 검토했다. **이 안은 위
3번(재수화) 요구를 소멸시킨다** — 토큰이 spawn 시점에 만들어지므로 재시작 후
복원할 대상이 애초에 없다. 그 점은 사실이고, 이 설계의 가장 성가신 부분을
없애는 것도 맞다.

기각 이유는 admission 모델이 함께 바뀌기 때문이다.

- 지금 admission은 "flock id → 토큰 하나"의 **동등 비교**다. member별 토큰은
  이것을 **집합 소속 검사**로 만든다.
- `removeFlockAgent`에 **per-member 폐기 경로**가 새로 필요하다. 빠뜨리면
  제거된 member의 토큰이 flock 수명 내내 유효하게 남는다 — 즉 폐기 표면이
  하나에서 member 수만큼으로 늘어난다.

재수화는 시작 경로의 순회 하나로 끝나고 테스트로 고정할 수 있는 반면, 폐기
누락은 조용히 실패한다. 국소적이고 관측 가능한 비용을 택했다.

---

## 잔여 위험 계약

| 항목 | 구 모델 | 이 결정 이후 |
|---|---|---|
| flock guest가 보유하는 권한 | 운영자 control plane 전권(전 tenant의 VM/snapshot/config/tenant/audit 표면) | 그 flock의 wall `post\|wall\|wall/history` + `call` |
| 다른 flock에 대한 영향 | 전부 | 없음 — flock id exact match |
| 자격증명 수명 | 운영자 토큰의 `Expires`를 따름(만료·회전 가능) | **만료 없음** — flock 삭제까지 |

**이 결정은 순수 개선이 아니라 거래다.** 만료되는 넓은 자격증명을 **만료되지
않는 좁은 것**으로 바꾼다. flock에는 TTL도, reaper도, GC도 없으므로 flock
토큰의 수명은 그 flock의 수명과 같고 사실상 무제한이다. 개별 회전 경로도 두지
않았다(routed flock 선례가 이미 회전 없이 운영된다).

그럼에도 이 거래를 택하는 근거는 **지배적 항이 범위이지 수명이 아니기**
때문이다. 운영자 bearer는 만료되더라도 유효한 동안 제어평면 전체를 열고, 그
폐기는 fleet 전체에 영향을 주는 회전뿐이다. 능력 토큰은 만료되지 않지만 한
flock의 게시판 sub-path 두 종류밖에 열지 않고, flock 삭제라는 **자연스럽고
국소적인 폐기 지점**을 갖는다.

이 거래를 뒤집을 만한 후속 후보를 명시해 둔다: flock TTL/reaper가 도입되면
능력 토큰의 수명이 그에 종속되므로 위 표의 마지막 행이 닫힌다. 그때까지는
**flock을 지우는 것이 유일한 폐기 수단**이라는 사실을 운영 계약으로 둔다.

범위 밖으로 남기는 것:

- **`micro-init` 권한 강등** — 별도 결정. 이 slice는 guest 내부 root 문제를
  풀지 않고 주입되는 토큰의 등급만 낮춘다. goose/workload가 root를 전제하므로
  단독 적용은 위험하다.
- **`call_token`과의 통합** — daemon-to-daemon hop 전용이며 범위가 다르다.
- **guest 측 코드** — `goose-agent`/`gtwall`/`gtcall`은 파일에서 읽은 값을
  그대로 Bearer로 보낸다. 값이 무엇인지 모르므로 변경이 필요 없다.

---

## 결과

긍정적 결과:

- 두 flock kind가 **하나의 토큰 모델**을 공유한다. admission 코드에 kind 분기가
  없고, routed flock에서 축적된 패턴·테스트·운영 경험이 그대로 적용된다.
- 신규 배관이 작다 — 토큰 파일 하나와 시작 시 재수화 순회 하나. admission
  로직, guest 코드, `call_token` 규율은 손대지 않았다.
- 폐기가 flock 삭제에 자연히 붙는다(구 모델에서는 운영자 토큰 회전이 유일한
  폐기 수단이었고 그것은 fleet 전체에 영향을 준다).

비용/잔여 위험:

- 위 표의 마지막 행 — **만료되지 않는 자격증명**이 flock 수명만큼 존재한다.
- 재수화 단계가 시작 경로의 **암묵적 요구사항**이 됐다. 빠지면 재시작 후 첫
  member spawn이 조용히 빈 토큰을 주입한다. 회귀 테스트로 고정했다.
- `controlPlaneTokenForVM()`이 production 코드에서 더 이상 참조되지 않는다
  (운영자 bearer 접근자로 남아 있다). 제거는 `propagateCPTokenToVMs` 정리와
  같은 slice로 미룬다 — 구세대 VM이 남아 있는 동안은 그 배관 전체가 필요하다.

---

## 검증 기준

- 유닛(`cmd/goose-daemon`): 능력 토큰이 자기 flock의 `post`/`wall`/
  `wall/history`/`call`을 admit하고 **다른 flock의 같은 경로는 401**;
  `POST /vms`·`DELETE /vms/{id}`·`/config/*`·`/tenants`·`/snapshots`에서
  **거부**(핵심 회귀 — 이 slice의 존재 이유이며, 같은 경로를 운영자 bearer로도
  probe해 "경로가 사라져서 통과한 것"이 아님을 함께 고정); flock 삭제가
  admission과 토큰 파일을 함께 무효화; 재시작 후 재수화가 admission을 복구하고
  `addFlockAgent`가 **빈 토큰이 아닌** 능력 토큰을 주입; 기존 운영자 bearer도
  계속 admit; SIGHUP이 능력 토큰을 덮어쓰지 않음; `CPTokenManaged=true`가 남은
  구세대 VM은 **계속 회전을 받음**; auth 비활성 모드는 구 동작과 동일(토큰 미발급·
  파일 미기록).
- 유닛(`internal/orchestrator`): 토큰 파일 round-trip, **상위 디렉토리가 0777
  이어도 파일 모드가 0600**(덮어쓰기 후에도), 삭제 멱등, `metadata.json`에
  토큰이 없음 + 기존 never-persists-token 가드가 **무수정으로** 통과.
- `go build ./...`, `go vet ./...`, `gofmt -l .`(비어야 함), `go test ./...`,
  `go test -race ./cmd/goose-daemon/ ./internal/orchestrator/` 전부 통과.
- 주입 지점 회귀 가드: `grep -n "ControlPlaneToken: cp.controlPlaneTokenForVM(),"
  cmd/goose-daemon/orchestrator_api.go`가 **0건**이어야 한다(변경 전 3건).
- KVM e2e: 기존 `scripts/anvil-mcp-e2e.sh flock`과 cross-host wall/gtcall
  스크립트가 그대로 통과해야 한다.

---

## 관련 문서

- [`CONTEXT.md`](../../CONTEXT.md) — 고정 런타임 계약의 토큰 절이 두 flock kind가
  같은 능력 토큰 모델을 쓴다는 사실을 기록한다.
- [`docs/operations/security-policy.md`](../operations/security-policy.md) —
  guest agent token 정책 절이 CP token 주입을 운영 관점에서 요약한다.
- [`docs/PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md) — flock 표면에
  per-flock guest 능력 토큰을 기록한다.
- [`docs/ADR_INDEX.md`](../ADR_INDEX.md) — 이 ADR의 적용 상태.
