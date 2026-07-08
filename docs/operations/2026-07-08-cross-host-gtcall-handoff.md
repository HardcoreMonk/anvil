# Cross-host gtcall Handoff

- 작성일: 2026-07-08
- 대상 branch: `worktree-gtcall`
- 대상: `cmd/goose-daemon`(flock call dispatch, authMiddleware), `internal/anvilmcp`
  (routed flock create/reconcile/rollback), `internal/orchestrator`(FlockManager
  roster/relay), `scripts/gtcall`
- 상태: 구현·유닛(+`-race`)·KVM e2e 게이트 완료. 설계:
  [`docs/superpowers/specs/2026-07-07-cross-host-gtcall-design.md`](../superpowers/specs/2026-07-07-cross-host-gtcall-design.md)
- 선행: [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](2026-07-06-cross-host-town-wall-handoff.md)
  ("cross-host `gtcall`" follow-up — 이 슬라이스가 그 후속을 닫는다),
  [`docs/operations/2026-07-07-reconcile-loop-handoff.md`](2026-07-07-reconcile-loop-handoff.md)

## 무엇이 배포됐나

routed flock의 **어느 member든 다른 어느 member를** 호출할 수 있다(완전 parity —
단일 host flock에서 `gtcall <agent_id> <prompt>`가 어느 peer든 호출할 수 있던 것과
동형). guest는 계속 bridge-only(로컬 daemon만 접촉)로 격리된다.

### Guest 계약 전환

daemon에 신설된 **단일 endpoint** `POST /flocks/{id}/call`(body
`{agent_id, prompt}`)로 `scripts/gtcall`이 전환됐다(`41b1880`). 이전
`scripts/gtcall`은 guest가 ① `GET /flocks/{id}`로 agent_id→vm_id를 해석하고
② `POST /vms/{vm_id}/tasks`로 프롬프트를 보내는 2-step 계약이었는데, routed
flock에서는 대상 agent가 다른 host에 있을 수 있어 두 단계 모두 로컬 daemon에서
실패했다(daemon이 대상 VM의 `agent_token`을 로컬에서만 알기 때문에 원격 VM으로는
애초에 두 번째 단계가 성립하지 않는다). 새 단일 endpoint가 해석(agent→host,vm_id)과
dispatch(local/hub/relay 분기, home 경유 2-hop 포함)를 daemon 쪽으로 흡수한다.
**기존 2-step 경로(`GET /flocks/{id}` + `POST /vms/{vm_id}/tasks`)는 제거·약화
없이 하위호환으로 유지된다** — 단일 host에서 새 경로와 기존 경로의 응답 semantics는
동일하다(`TaskResult.output` 텍스트).

### 해석과 실행 (home 경유 2-hop)

- hub 등록(`POST /flocks/{id}/distributed`)의 roster member가
  `{AgentID, Host}` → `{AgentID, Host, VMID, Addr}`로 확장됐다(`435ec65`). 최초
  hub 등록은 spawn 전이라 VMID가 없으므로, spawn 완료 후 `record.Agents` 기반의
  VMID 포함 roster로 **재등록**하고 hub idempotent 경로가 재등록 시 Roster를
  실제로 갱신하도록 고쳤다(이전에는 token만 재주입하고 roster를 버렸다).
  reconcile 재등록도 동일한 VMID 포함 roster를 보낸다(`fb22f3c`).
- home의 로컬 대상 판정은 host 이름 비교가 아니라 **roster VMID가 자기 로컬 VM
  registry에 존재하는지**로 한다(daemon은 자기 control-plane host 이름을 모른다) —
  `flockAgentLocalVM`.
- home 수신 call: target이 home 자신의 host면 로컬 dispatch, 아니면 target host
  daemon의 `POST /flocks/{id}/call`로 2번째 hop(`forwardFlockCall`).
- **루프 방지**: hop 요청에 `X-Ephemera-Call-Hop: 1`을 붙이고, 이 표식이 있는
  요청은 어느 daemon도 다시 forward하지 않는다. **member→home 구간은
  unmarked, home→target 종단 hop만 marked**다(2026-07-08 최종 리뷰 C1
  수정 — home은 target 해석자이지 종단이 아니라 자신의 2번째 hop을 위해
  표식 없는 요청을 받아야 한다; 이전에는 member→home 구간에도 무조건
  표식을 붙여 home이 로컬 해석만 시도하다 대상이 자기 host에 없으면
  worker→worker 타 host 호출이 전부 404였다).
- **member의 로컬 해석 데이터(2026-07-08 설계 보정, `22b7730`)**: 최초 구현(Task 3,
  `64dde70`)은 hopped 요청을 `f.Kind == FlockKindRelay`일 때 무조건 `404`로
  끝냈는데, 실제 토폴로지에서 target member daemon은 자신의 flock을 relay
  kind로 등록하고 있어(자기 outbound wall/call 트래픽을 home으로 forward하는
  용도) 이 규칙대로면 2번째 hop이 성립하지 않는다(Task 3 리뷰에서 발견). 수정:
  hopped 요청은 **kind와 무관하게 로컬 해석만** 시도한다. `POST /flocks/{id}/relay`
  요청 body에 그 host의 member 목록(`agents: [{agent_id, vm_id}]`)을 추가해
  relay flock도 host-local agent 조회 대상을 갖는다. hub 재등록과 같은 시점
  (spawn 완료 후)에 relay도 host-local agent 목록으로 재등록하고, reconcile이
  동일하게 재주입한다(`fb22f3c`). 이 목록은 로컬 해석 전용 — 다른 host의 정보는
  담지 않는다.
- **depth 가드**: `X-Ephemera-Task-Depth`를 전 hop에서 그대로 전파해 기존
  `EPHEMERA_MAX_TASK_DEPTH`(기본 5, 한계 시 `508`)가 cross-host에서도 성립한다.

## 보안 경계

- **토큰 모델(2026-07-07 재확정 — A안)**: routed guest의 주입 토큰
  (`.ephemera-cp-token`)이 `relay_token` 자체라는 사실 때문에, `relay_token`을
  **guest 능력 토큰**으로 재해석했다:
  - **`relay_token`**: 그 flock의 wall sub-path(`post|wall|wall/history`) **와
    `call` 진입**을 admit한다(`relayGuestPathFlockID`) — guest가 로컬 daemon에서
    gtwall/gtcall 모두 이 토큰으로 인증한다(단일 host flock에서 guest CP token이
    gtcall을 여는 기존 기능과 동형). 유출 blast radius는 "VM 1대 탈취"와 등가
    (이미 guest가 보유).
  - **`call_token`**: **daemon 간 call hop 전용**(member→home, home→target).
    per-flock secret, `relay_token`과 나란한 규율이지만 admit 범위는 더 좁다 —
    `callPathFlockID`가 **오직 `/flocks/{id}/call`만** 인정하고 wall sub-path는
    거부한다(control-plane bearer 승격 금지, 이 방향의 배타는
    `TestCallToken_AdmitsOnlyCallPath`로 고정). admit 시 `countAuth("call")` +
    synthetic identity `call:<flockID>` 기록(`relay`와 동일한 attribution
    패턴). `PlacementStore`에 영속(전용 `RoutedFlockCallTokens` map, `State()`가
    nil 처리해 모든 직렬화 표면에서 redaction). hub/relay 등록 요청에
    `relay_token`과 함께 배포, reconcile 재주입, rollback/delete revoke. hop
    토큰이므로 `relay_token`과 독립적으로 revoke 가능하다.
- **wall slice 잠재 결함 동반 수정**: `registerRelayFlock`이 admit 등록
  (`setRelayToken`/`setCallToken`)을 하지 않고 `FlockManager.RegisterRelay`만
  호출해 in-memory flock 구조체(member의 outbound relay hop이 읽는 자료)만
  갱신하던 기존 결함을 고쳤다 — 이 때문에 auth-on member daemon에서 routed
  guest의 gtwall(inbound)이 401이 되는 결함이 있었다. wall e2e가 이를 못 잡은
  이유는 member daemon이 **auth-off**였기 때문이다(`authMiddleware`는
  `cp.clients`가 비어 있으면 relay/call 토큰 admission 블록 자체를
  short-circuit한다). gtcall e2e는 member daemon을 **auth-on**으로 돌려 admit
  경로를 실경로에서 검증한다.
- 대상 VM의 `agent_token`은 target host daemon 로컬에서만 주입된다. wire를
  건너는 것은 `{agent_id, prompt}` + depth/hop 헤더뿐 — per-VM token, CP token,
  provider key 미노출(e2e sentinel 3종: `agent_token` 필드명, per-VM 토큰 값, CP
  토큰(=relay_token) 값 모두 부재).
- guest는 신규 네트워크 노출 없음(로컬 daemon만). 신규 `anvil_*` MCP tool 없음 —
  기존 IronClaw schema-exclusion guard가 계속 통과한다(신규 anvil_* tool을
  추가하지 않았으므로 이 slice에 새 guard는 불필요).
- 에러 문자열은 flock/host/agent 식별자만 — daemon 주소·토큰을 bound out
  (reconcile-loop slice에서 확립한 규율과 동일).

## Gate 결과

- **유닛**: `go test ./cmd/... ./internal/... -count=1` — Task 1, 2, 4, 5에서
  전체 suite 실행, 매번 green(Task 1: 14 패키지 `ok`; Task 4: `ok`; Task 5:
  `ok`, 13 패키지). Task 3/3b/6/7은 대상 패키지(`cmd/goose-daemon`,
  `internal/anvilmcp`, `internal/orchestrator`) 타겟 실행 + 이전 task의 전체
  suite 결과로 누적 검증.
- **race**: 매 task마다 `-race` 병행 — Task 1
  (`go test ./cmd/goose-daemon ./internal/orchestrator ./internal/anvilmcp
  -count=1 -race`), Task 2(`cmd/goose-daemon` 전체 패키지 `-race` +
  `internal/orchestrator/... internal/anvilmcp/... -race` + `go test ./...
  -count=1`), Task 3/3b(타겟 테스트 `-race -v`), Task 4/5
  (`internal/anvilmcp -count=1 -race`) 전부 green.
- **빌드**: 각 task에서 `go build -o anvil-daemon ./cmd/goose-daemon/` 포함
  green(Task 7의 KVM 실행 직전 빌드로 최종 확인).
- **KVM e2e**(`scripts/anvil-cross-host-gtcall-e2e.sh`, Task 7): 실 member VM
  경로 — guest `gtcall` → in-VM agent → **auth-on** member daemon(relay
  flock) → `call_token` call hop → stub home 수신·응답 → guest stdout 왕복.
  **18/18 checks, EXIT 0, 2회 연속 실행**(한 번은 `tee` 경유, 한 번은
  standalone) 모두 통과, 좀비 프로세스 없이 클린 teardown 확인. `bash -n`
  EXIT 0. `shellcheck`은 wall e2e 템플릿과 동일한 37건 SC2317(info,
  trap-invoked `cleanup()` 함수 안 — shellcheck이 `trap ... EXIT` 도달성을
  못 따라가는 것으로, 신규 finding 없음).
- `git diff --check` clean(매 task 커밋 전 확인, 공통 관례).
- **범위 밖**: progress.md의 "Final verification gate (after all tasks)"
  블록(전체 suite + 4 binary 빌드 + `scripts/anvil-cross-host-wall-e2e.sh`
  17/17 회귀 + `scripts/anvil-mcp-e2e.sh flock` 단일 host 회귀를 한 번에
  묶어 재실행)은 Task 8(docs-only)에서 재실행하지 않았다 — 아래 Next Action
  참고.

## Known limitations / 운영 주의

- **home SPOF**: home host가 다운되면 그 flock의 wall뿐 아니라 call도 전부
  불가하다. wall과 동일하게 1차 수용된 설계 결정이며, mesh 진화 경로를
  공유한다(town wall handoff 참고).
- **`/workloads/run`의 depth env 특성**: `cmd/goose-agent/main.go`의
  `workloadEnvironment()`(`/workloads/run`이 사용하는 실행 환경)는
  `EPHEMERA_TASK_DEPTH`를 주입하지 않는다 — 실 `/tasks` 핸들러(`runTaskHandler`)만
  이 변수를 주입한다. `gtcall`은 자기 프로세스 env의 `$EPHEMERA_TASK_DEPTH`가
  있을 때만 depth 헤더를 보내므로, e2e가 depth 헤더 전파를 검증하려면 in-guest
  workload 스크립트에서 `export EPHEMERA_TASK_DEPTH=1`을 명시해야 했다(Task 7).
  이것은 버그가 아니라 `/workloads/run`이 애초에 실 task 실행 경로가 아니라는
  사실의 정직한 재현으로 판정됐다.
- **e2e 포트 공유**: `scripts/anvil-cross-host-gtcall-e2e.sh`가
  `scripts/anvil-cross-host-wall-e2e.sh`와 동일한 포트(daemon `3000`, stub home
  `3100`)를 사용한다 — 두 e2e를 동시에 실행할 수 없다(wall e2e부터 이어진 기존
  특성).
- **`AGENT_TOKEN` sentinel의 `-n` 게이트**: per-VM agent token 유출을 검증하는
  assertion이 `if [ -n "$AGENT_TOKEN" ] && printf '%s' "$CAP" | grep -qF
  "$AGENT_TOKEN"` 형태다(wall e2e 템플릿에서 상속) — `AGENT_TOKEN` 추출이 어떤
  이유로든 빈 문자열이면 이 assertion은 실제로 아무것도 검증하지 않고 조용히
  `ok`를 출력한다. 현재 KVM 실행에서는 `AGENT_TOKEN`이 항상 채워져 실효
  검증이었지만, 구조적으로는 silent-pass 가능성이 있는 잠재 약점이다(아래
  Follow-Up).

## Next Action

release-gate 관점에서 이 slice 자체(유닛·race·gtcall e2e 18/18×2)는 닫혔다. 다만
progress.md의 "Final verification gate (after all tasks)"(전체 suite green + 4
binary 빌드 + `scripts/gtcall`/`scripts/anvil-cross-host-gtcall-e2e.sh` 구문
검사 + KVM host에서 wall e2e 17/17·단일 host flock e2e 회귀 재확인)는 Task
8(docs-only) 범위 밖이라 재실행하지 않았다 — cross-host gtcall + town wall을
합쳐 릴리즈 표면으로 최종 승인하기 전에 이 결합 gate를 한 번 더 실행하는 것을
권장한다.

## Follow-Up Tasks

원장(`​.superpowers/sdd/progress.md`)의 Minor 중 이 slice 이후에도 남는 항목:

- **`UpdateHubRoster` bool 반환 미사용**(Task 1 Minor): `FlockManager.UpdateHubRoster`가
  갱신 성공 여부를 `bool`로 반환하지만 `registerDistributedFlock` 호출부는 그 값을
  버린다. TOCTOU 창(호출 사이에 flock이 사라지거나 kind가 바뀌는 경우)이 이론상
  존재 — 반환값을 확인해 실패 시 진단하도록 강화하는 것이 후속.
- ~~**`mergePersistedRoutedFlocks`의 토큰 불참여**~~(Task 4 Minor, 기존 gap) —
  **CLOSED** (`07c1e17`): `internal/anvilmcp/placement_store.go`에 신설된
  `mergePersistedRoutedFlockTokens`가 `RoutedFlockRelayTokens`/
  `RoutedFlockCallTokens`를 `mergePersistedRoutedFlocks`와 동일한 disk-권위
  full-replace 규율로 병합한다 — stale store의 generic save가 동시 writer의
  방금 영속된 토큰을 지우는 문제를 닫는다.
- **e2e 템플릿 하드닝**: `AGENT_TOKEN` sentinel의 `-n` 게이트(Known limitations
  참고)처럼 silent-pass 가능한 assertion 패턴이 `scripts/anvil-cross-host-wall-e2e.sh`와
  `scripts/anvil-cross-host-gtcall-e2e.sh` 양쪽에 공통으로 있다. 두 스크립트를
  함께 훑어 "빈 값이면 조용히 통과"하는 assertion을 "빈 값이면 명시적으로
  fail"하는 형태로 교체하는 하드닝 패스가 후속이다.
- ~~**bounded relay retry/buffer**~~ — **CLOSED** (`e94028b`, `6317a58`,
  2026-07-08 bounded relay retry slice): call hop(`forwardFlockCall`,
  relay→home과 hub→target 양쪽)이 dial-계열 transport 실패에 한해 동기 bounded
  retry(총 3 시도, 1s→2s backoff)로 순단을 흡수한다. 비동기 수락·버퍼·백그라운드
  재전송은 비범위로 남았다 — mesh/수동 multi-host 검증 이후 재평가. 상세:
  [`docs/operations/2026-07-08-bounded-relay-retry-handoff.md`](2026-07-08-bounded-relay-retry-handoff.md).

그 밖에 Task 2/3b/5의 cosmetic-level Minor(idempotent 재등록의 call-token 단언
미러 없음; relay Agents가 `GET /flocks` 직렬화에 노출 — `AgentID`/`VMID`뿐이라
비밀 아님; 빈 agents 재등록이 목록을 wipe하는 것은 재주입 설계상 의도;
create-reconcile member 그룹핑 스타일 불일치)는 기능·보안 영향이 없는 것으로
판단해 별도 follow-up으로 올리지 않는다.
