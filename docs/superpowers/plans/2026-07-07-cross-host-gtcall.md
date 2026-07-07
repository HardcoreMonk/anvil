# Cross-host gtcall Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** routed flock의 어느 member든 다른 어느 member를 `gtcall`로 호출할 수 있게 한다 — 새 `POST /flocks/{id}/call` 계약, per-flock `call_token`, member→home→target 2-hop.

**Architecture:** guest는 로컬 daemon의 `/flocks/{id}/call`만 호출한다(bridge-only 불변). relay daemon은 home으로 forward하고, home은 VMID/Addr가 담긴 canonical roster로 해석해 로컬 dispatch 또는 target host daemon으로 2번째 hop을 보낸다. hop은 `call_token`(relay_token과 별도 secret, call 경로만 admit)으로 인증하고 `X-Ephemera-Call-Hop: 1`로 루프를 차단하며 `X-Ephemera-Task-Depth`를 전파한다. 설계 spec: `docs/superpowers/specs/2026-07-07-cross-host-gtcall-design.md`.

**Tech Stack:** Go (module `ephemera`), `net/http`+`httptest`, sh(`scripts/gtcall`), KVM e2e 스크립트.

## Global Constraints

- 브랜치: local main에서 분기 (spec/plan 커밋 포함). TDD RED→GREEN 필수. 기존 테스트 제거/약화 금지. **git trailer 금지**.
- 각 task 종료 시 전체 suite `go test ./cmd/... ./internal/... -count=1` (부분 suite 금지).
- **토큰 모델 (A안)**: relay_token은 그 flock의 wall sub-path **+ `call` 진입**을 admit(guest 능력 토큰 — guest 주입 `.ephemera-cp-token`이 relay_token이므로). **call_token은 daemon 간 hop 전용**으로 해당 flock의 `/flocks/{id}/call`만 admit — **wall 경로 거부**(이 방향 배타는 테스트로 고정), control-plane bearer 승격 금지.
- **wall 잠재 결함 동반 수정**: `registerRelayFlock`이 `setRelayToken`을 안 해 auth-on member daemon에서 guest gtwall이 401 — 이 slice에서 수정(member 등록도 admit 등록).
- 대상 VM `agent_token`은 target host daemon 로컬에서만 주입. wire에는 `{agent_id, prompt}` + depth/hop 헤더만.
- 에러 문자열·로그에 daemon 주소·토큰 노출 금지 — flock/host/agent 식별자만 (`d5c7df0` 규율). roster의 `Addr`는 내부 용도로만 쓰고 로그·직렬화 표면에 내지 않는다.
- 신규 `anvil_*` MCP tool 없음 — `TestIronClawSchemasExcludeCrossHostWallTools` 등 exclusion guard 전부 계속 통과.
- 기존 2-step gtcall 경로(`GET /flocks/{id}` + `POST /vms/{vm_id}/tasks`)는 유지.
- timeout 계단: guest `300s`(gtcall `-m 300` 유지) > member→home forward ctx `290s` > home→target hop ctx `280s`.
- 단일 host flock(local kind) 경로의 기존 semantics 불변.

---

## File Structure

- `internal/orchestrator/flock.go` — `RosterMember` += `VMID`,`Addr`; `Flock` += `CallToken`; `RegisterHub`/`RegisterRelay`에 callToken 파라미터; hub roster 갱신 helper.
- `cmd/goose-daemon/orchestrator_api.go` — 등록 요청 struct += `call_token`; hub idempotent 경로 roster 갱신; `handleFlockItem`에 `call` case; `callFlockAgent` 핸들러(+relay forward, hub 2nd hop, 로컬 dispatch); `deleteFlock` call token revoke.
- `cmd/goose-daemon/api.go` — `callPathFlockID`; `authMiddleware`에 `callTokenFor` 파라미터 + call admission 블록; `cp.callTokens` store(set/get/remove).
- `internal/anvilmcp/daemon_client.go` — `RosterMember` += `VMID`,`Addr`; 등록 요청 타입 += `CallToken`.
- `internal/anvilmcp/placement_store.go` — `RoutedFlockCallTokens` 병렬 배관(carrier 포함 ~10개 지점).
- `internal/anvilmcp/routed_flock.go` — call token 생성·조기 영속·배포, spawn 후 VMID/Addr roster 재등록, rollback revoke.
- `internal/anvilmcp/runtime_router.go` — reconcile 재등록에 VMID/Addr + call token.
- `scripts/gtcall` — 단일 `/call` 호출로 전환.
- `scripts/anvil-cross-host-gtcall-e2e.sh` — 신규 KVM e2e.
- Tests: `cmd/goose-daemon/{orchestrator_api_test.go,auth_test.go,flock_call_test.go(신규)}`, `internal/anvilmcp/{routed_flock_test.go,runtime_router_test.go,placement_store_test.go}`, `internal/orchestrator/flock_test.go`.

---

## Task 1: roster VMID/Addr 확장 + hub 재등록 시 Roster 갱신

**Files:**
- Modify: `internal/orchestrator/flock.go` (`RosterMember`, ~line 31)
- Modify: `internal/anvilmcp/daemon_client.go` (`RosterMember`, ~line 68)
- Modify: `cmd/goose-daemon/orchestrator_api.go` (`registerDistributedFlock` idempotent 경로, ~line 1214)
- Test: `cmd/goose-daemon/orchestrator_api_test.go`

**Interfaces:**
- Produces: `orchestrator.RosterMember{AgentID, Host, VMID, Addr string}` (json `agent_id`,`host`,`vm_id`,`addr`); anvilmcp 측 동일. hub 재등록(POST `/flocks/{id}/distributed`, 기존 hub 존재) 시 `existing.Roster = req.Roster`로 갱신됨 — Task 3의 해석, Task 5의 재등록이 이에 의존.

- [ ] **Step 1: 실패 테스트** — `cmd/goose-daemon/orchestrator_api_test.go`에 추가. 기존 `TestRegisterDistributedFlock_ReAdmitsRelayTokenOnReRegister`(~:297)의 셋업 헬퍼(newTestCP 등)를 그대로 따른다:

```go
func TestRegisterDistributedFlock_UpdatesRosterOnReRegister(t *testing.T) {
	cp := newTestCP(t)
	first := `{"roster":[{"agent_id":"researcher-1","host":"host-a"}],"relay_token":"rt-1"}`
	rr := httptest.NewRecorder()
	cp.registerDistributedFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(first)), "routed-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("first register = %d, want 201", rr.Code)
	}

	second := `{"roster":[{"agent_id":"researcher-1","host":"host-a","vm_id":"vm-11","addr":"http://host-a:3000"}],"relay_token":"rt-1"}`
	rr = httptest.NewRecorder()
	cp.registerDistributedFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(second)), "routed-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-register = %d, want 201", rr.Code)
	}

	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f.Kind != orchestrator.FlockKindHub {
		t.Fatalf("hub flock missing after re-register")
	}
	if len(f.Roster) != 1 || f.Roster[0].VMID != "vm-11" || f.Roster[0].Addr != "http://host-a:3000" {
		t.Fatalf("roster not updated on re-register: %+v", f.Roster)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/goose-daemon -run TestRegisterDistributedFlock_UpdatesRosterOnReRegister -count=1` / Expected: FAIL (VMID/Addr 필드 없음 — 컴파일 에러도 RED).

- [ ] **Step 3: 구현**

`internal/orchestrator/flock.go`의 `RosterMember`:

```go
type RosterMember struct {
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
	// VMID/Addr let the hub resolve a call target and reach its host daemon.
	// Both are filled by the post-spawn re-registration (the initial pre-spawn
	// registration has neither). Addr is daemon-internal: never log it and
	// never emit it on a serialized surface.
	VMID string `json:"vm_id,omitempty"`
	Addr string `json:"addr,omitempty"`
}
```

`internal/anvilmcp/daemon_client.go`의 `RosterMember`에 동일 필드(`VMID string json:"vm_id,omitempty"`, `Addr string json:"addr,omitempty"`) 추가.

`cmd/goose-daemon/orchestrator_api.go` idempotent hub 경로 (~1214, 기존 409 블록 유지):

```go
	if existing, ok := cp.flockMgr.Get(flockID); ok {
		if existing.Kind != orchestrator.FlockKindHub {
			writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-hub flock", flockID))
			return
		}
		// Re-registration refreshes the roster too (not only the token): the
		// post-spawn re-POST carries the VMID/Addr-enriched roster the initial
		// pre-spawn registration could not know, and reconcile re-POSTs depend
		// on the same path after a daemon restart.
		if len(req.Roster) > 0 {
			existing.Roster = req.Roster
		}
		cp.setRelayToken(flockID, req.RelayToken)
		w.WriteHeader(http.StatusCreated)
		return
	}
```

(`existing.Roster` 대입의 동시성: `FlockManager.Get`이 포인터를 반환하므로 raw 대입은 race 소지 — `flock.go`에 mutex 잡는 setter가 있으면 사용, 없으면 `FlockManager`에 `UpdateHubRoster(flockID string, roster []RosterMember) bool`을 추가해 `fm.mu.Lock()` 아래에서 갱신하고 handler는 그것을 호출한다. **`-race`로 검증.**)

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/goose-daemon -run 'TestRegisterDistributed' -count=1 -race` / Expected: 신규+기존 전부 PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/orchestrator/flock.go internal/anvilmcp/daemon_client.go cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/orchestrator_api_test.go
git commit -m 'feat(runtime): roster carries VMID/Addr; hub re-register updates roster'
```

---

## Task 2: call_token — daemon admission·store·등록/삭제 배관

**Files:**
- Modify: `cmd/goose-daemon/api.go` (`authMiddleware`, `relayWallPathFlockID` 옆, CP struct, store 메서드)
- Modify: `cmd/goose-daemon/orchestrator_api.go` (등록 요청 struct, register 핸들러 2곳, `deleteFlock`)
- Modify: `internal/orchestrator/flock.go` (`Flock.CallToken`, `RegisterHub`/`RegisterRelay` 시그니처)
- Modify: `internal/anvilmcp/daemon_client.go` (요청 타입 += CallToken)
- Test: `cmd/goose-daemon/auth_test.go`, `cmd/goose-daemon/orchestrator_api_test.go`

**Interfaces:**
- Consumes: Task 1의 갱신된 등록 경로.
- Produces: `authMiddleware(getClients, relayTokenFor, callTokenFor func(string) string, authTotal, next)` (호출부 `api.go:609` 및 테스트 전부 갱신, nil-tolerant); `cp.setCallToken/callTokenFor/removeCallToken`; `distributedFlockRequest`/`relayFlockRequest` += `CallToken string json:"call_token"`; anvilmcp `DistributedFlockRequest`/`RelayFlockRequest` += `CallToken string json:"call_token"`; `orchestrator.Flock.CallToken`; `RegisterHub(flockID, wall, roster, relayToken, callToken string)`, `RegisterRelay(flockID, homeAddr, relayToken, callToken string)` (기존 호출부 갱신 — grep `RegisterHub(\|RegisterRelay(`); `countAuth("call")` + identity `call:<flockID>`.

- [ ] **Step 1: 실패 테스트** — `cmd/goose-daemon/orchestrator_api_test.go`에 (기존 `TestRelayToken_AdmitsOnlyWallPaths` ~:117 미러):

```go
func TestCallToken_AdmitsOnlyCallPath(t *testing.T) {
	cp := newTestCP(t)
	cp.setCallToken("routed-1", "ct-1")
	cp.setRelayToken("routed-1", "rt-1")
	handler := authMiddleware(func() []APIClient { return []APIClient{{Name: "op", Token: "op-tok"}} },
		cp.relayTokenFor, cp.callTokenFor, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

	cases := []struct {
		name, path, bearer string
		want               int
	}{
		{"call token admits call path", "/flocks/routed-1/call", "ct-1", http.StatusOK},
		{"call token rejected on wall post", "/flocks/routed-1/post", "ct-1", http.StatusUnauthorized},
		{"call token rejected on wall history", "/flocks/routed-1/wall/history", "ct-1", http.StatusUnauthorized},
		{"call token rejected on other flock", "/flocks/other/call", "ct-1", http.StatusUnauthorized},
		{"call token rejected on non-flock route", "/vms", "ct-1", http.StatusUnauthorized},
		{"relay token admits call entry (guest capability)", "/flocks/routed-1/call", "rt-1", http.StatusOK},
		{"relay token still admits wall post", "/flocks/routed-1/post", "rt-1", http.StatusOK},
		{"relay token rejected on other flock call", "/flocks/other/call", "rt-1", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.bearer)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.name, rr.Code, tc.want)
		}
	}
}
```

(주의: 기존 `authMiddleware` 401 상태코드가 `http.StatusUnauthorized`가 아니라 다른 것을 쓰면 기존 `TestRelayToken_AdmitsOnlyWallPaths`의 기대값을 따른다. `authTotal`이 nil 허용이 아니면 기존 테스트가 쓰는 metric fixture를 그대로 사용.)

`cmd/goose-daemon/auth_test.go`에 (기존 `TestAuthMiddleware_RelayTokenRecordsIdentityAndMetric` ~:75 미러 — 그 테스트의 metric/holder 검증 방식을 그대로 복사하고 label만 `call`, identity만 `call:routed-1`로):

```go
func TestAuthMiddleware_CallTokenRecordsIdentityAndMetric(t *testing.T) {
	// 기존 TestAuthMiddleware_RelayTokenRecordsIdentityAndMetric의 본문을
	// 복사해 다음만 바꾼다: setRelayToken→setCallToken, 요청 path를
	// /flocks/routed-1/call로, 기대 identity를 "call:routed-1"로, 기대
	// metric outcome label을 "call"로, relayTokenFor 자리는 nil이 아니라
	// cp.relayTokenFor 그대로 두고 callTokenFor를 추가로 전달.
}
```

(구현자는 위 주석 지시대로 실제 본문을 작성한다 — 기존 테스트가 진실원천.)

`TestDeleteFlock_RevokesRelayToken`(~:264) 미러로 `TestDeleteFlock_RevokesCallToken`도 추가(셋업 복사, setCallToken 후 deleteFlock → `cp.callTokenFor(flockID) == ""` 단언).

**wall 결함 수정 RED** — `cmd/goose-daemon/orchestrator_api_test.go`:

```go
func TestRegisterRelayFlock_AdmitsRelayAndCallTokens(t *testing.T) {
	cp := newTestCP(t)
	body := `{"home_addr":"http://home:3000","relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.registerRelayFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(body)), "routed-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("register relay = %d, want 201", rr.Code)
	}
	// member daemon도 guest의 relay token을 inbound admit해야 한다 — 이게
	// 없으면 auth-on member에서 guest gtwall/gtcall이 401 (wall slice 잠재 결함).
	if got := cp.relayTokenFor("routed-1"); got != "rt-1" {
		t.Fatalf("relayTokenFor = %q, want rt-1 (member daemon must admit guest token)", got)
	}
	if got := cp.callTokenFor("routed-1"); got != "ct-1" {
		t.Fatalf("callTokenFor = %q, want ct-1", got)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/goose-daemon -run 'TestCallToken_AdmitsOnlyCallPath|TestAuthMiddleware_CallTokenRecords|TestDeleteFlock_RevokesCallToken' -count=1` / Expected: FAIL (컴파일: setCallToken/callTokenFor 미정의).

- [ ] **Step 3: 구현**

`cmd/goose-daemon/api.go`:

```go
// callPathFlockID admits ONLY the call sub-path — the call token must never
// open wall paths (and relayWallPathFlockID never returns "call"), keeping
// the two per-flock secrets' blast radii disjoint.
func callPathFlockID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/flocks/")
	if !ok {
		return "", false
	}
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		return "", false
	}
	if sub == "call" {
		return id, true
	}
	return "", false
}
```

`authMiddleware` 시그니처에 `callTokenFor func(flockID string) string`를 `relayTokenFor` 다음 인자로 추가하고, relay admission 블록(api.go:79-99) **바로 아래**에 동형 블록 추가 (countAuth `"call"`, identity `"call:" + flockID`, `callPathFlockID` 사용 — relay 블록과 나란한 코드, slog 라인 포함). 호출부 `api.go:609`를 `cp.callTokenFor` 전달로 갱신, 기존 auth 테스트들의 `authMiddleware(...)` 호출도 전부 인자 추가(대부분 `nil` 허용).

**relay admission 확장(A안)**: `relayWallPathFlockID`의 switch에 `"call"`을 추가하고 함수명을 `relayGuestPathFlockID`로 rename(호출부·주석 갱신 — relay_token은 guest 능력 토큰으로서 wall+call 진입을 연다. call_token 쪽 `callPathFlockID`는 call만 — wall 방향 배타 유지).

**wall 결함 수정**: `registerRelayFlock`에 `cp.setRelayToken(flockID, req.RelayToken)` + `cp.setCallToken(flockID, req.CallToken)` 추가(hub 등록과 동형 — member daemon이 guest 토큰과 hop 토큰을 inbound admit).

CP struct(~api.go:353)에 `callTokens map[string]string` + `callTokensMu sync.RWMutex`, 초기화(~:472), `setCallToken`/`callTokenFor`/`removeCallToken` 메서드는 relay 3종(~:631-651)을 그대로 미러.

`internal/orchestrator/flock.go`: `Flock`에 `CallToken string json:"-"` (RelayToken 옆). `RegisterHub`(:440)와 `RegisterRelay`(:458)에 `callToken string` 파라미터 추가, struct 대입 포함. 기존 호출부 전부 갱신(`grep -rn 'RegisterHub(\|RegisterRelay(' cmd/ internal/` — 테스트 포함; recovery.go의 `NewUnregistered` 계열은 token을 다루지 않으면 무변경).

`cmd/goose-daemon/orchestrator_api.go`: `distributedFlockRequest`/`relayFlockRequest`에 `CallToken string json:"call_token"`. `registerDistributedFlock`: fresh 경로 `RegisterHub(..., req.RelayToken, req.CallToken)` + `cp.setCallToken(flockID, req.CallToken)`; idempotent 경로에도 `cp.setCallToken(flockID, req.CallToken)` (Task 1의 roster 갱신 옆). `registerRelayFlock`: `RegisterRelay(flockID, req.HomeAddr, req.RelayToken, req.CallToken)`. 빈 CallToken은 admit map에 넣지 않는다(`setCallToken`이 빈 문자열이면 no-op — relay 미러가 그렇게 안 되어 있으면 handler에서 `if req.CallToken != ""` 가드). `deleteFlock`(~:317): `cp.removeRelayToken` 옆에 `cp.removeCallToken(flockID)`.

`internal/anvilmcp/daemon_client.go`: `DistributedFlockRequest`/`RelayFlockRequest`에 `CallToken string json:"call_token"` (Task 5가 채움 — 이 task에서는 필드만).

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/goose-daemon -count=1 -race` / Expected: 신규 3종 + 기존 auth/register 테스트 전부 PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add cmd/goose-daemon internal/orchestrator/flock.go internal/anvilmcp/daemon_client.go
git commit -m 'feat(runtime): per-flock call_token — call-path-only admission, parallel to relay_token'
```

---

## Task 3: `POST /flocks/{id}/call` 핸들러 (해석·dispatch·forward·hop 가드·depth)

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go` (`handleFlockItem` switch + 신규 핸들러들)
- Test: Create `cmd/goose-daemon/flock_call_test.go`

**Interfaces:**
- Consumes: Task 1 roster(VMID/Addr), Task 2 `Flock.CallToken`/store.
- Produces: `POST /flocks/{id}/call` body `{"agent_id":"...","prompt":"..."}` → 성공 시 대상 agent의 `/tasks` 응답 바디를 그대로 반환(200), 미존재 agent `404`, upstream 불가 `502`, depth 초과 `508`. 헤더 계약: `X-Ephemera-Task-Depth` 전파·누적, `X-Ephemera-Call-Hop: 1`이 있으면 어떤 daemon도 재-forward하지 않음. Task 6의 gtcall, Task 7의 e2e가 이 계약을 사용.

- [ ] **Step 1: 실패 테스트** — Create `cmd/goose-daemon/flock_call_test.go`. 기존 `townwall_relay_test.go`의 relay flock 셋업과 `orchestrator_api_test.go`의 로컬 flock 셋업 헬퍼를 재사용한다:

```go
package main

// 다음 시나리오를 각각 독립 테스트로 작성한다. 셋업은 기존
// townwall_relay_test.go(relay 셋업: RegisterRelay + httptest home)와
// orchestrator_api_test.go(newTestCP, 로컬 flock/VM fixture)를 따른다.

// 1) TestCallFlockAgent_LocalDispatchesToAgent
//    local flock에 agent researcher-1(vm-1, httptest agent 서버가 /tasks에서
//    {"output":"pong"} 반환, Authorization: Bearer <agent_token> 캡처) 등록.
//    POST /flocks/{id}/call {"agent_id":"researcher-1","prompt":"ping"}
//    → 200, body에 "pong"; agent가 받은 Authorization == 그 VM의 agent token;
//    agent가 받은 X-Ephemera-Task-Depth == "1" (요청에 depth 없음 → 0+1).

// 2) TestCallFlockAgent_UnknownAgent404
//    같은 셋업, agent_id="ghost" → 404, body에 daemon 주소·토큰 문자열 없음.

// 3) TestCallFlockAgent_DepthLimit508
//    요청 헤더 X-Ephemera-Task-Depth: <maxTaskDepth> → 508, agent 서버 호출 0회.

// 4) TestCallFlockAgent_RelayForwardsToHome
//    relay flock(HomeAddr=httptest home, CallToken="ct-1") 등록. home 핸들러는
//    path/Authorization/X-Ephemera-Call-Hop/X-Ephemera-Task-Depth/body를 캡처하고
//    200 {"output":"remote-pong"} 반환.
//    POST /flocks/{id}/call (X-Ephemera-Task-Depth: 2 포함)
//    → 200 "remote-pong"; home이 받은 것: path == /flocks/{id}/call,
//    Authorization == "Bearer ct-1"(relay token 아님!), X-Ephemera-Call-Hop == "1",
//    X-Ephemera-Task-Depth == "2"(전파, 미누적 — 누적은 최종 로컬 dispatch에서만),
//    body == {"agent_id":...,"prompt":...} 그 외 필드 없음.

// 5) TestCallFlockAgent_RelayHonorsCallerContext
//    townwall_relay_test.go의 cancelled-ctx 패턴 미러 → 즉시 502.

// 6) TestCallFlockAgent_HubSecondHopUsesRosterAddr
//    hub flock 등록(roster: [{agent_id:"remote-1",host:"host-b",vm_id:"vm-9",
//    addr:<httptest target daemon URL>}], call_token="ct-1"). target daemon
//    핸들러는 /flocks/{id}/call 수신을 캡처하고 200 {"output":"hop2"} 반환.
//    hub daemon에 POST /flocks/{id}/call {"agent_id":"remote-1",...} (hop 헤더 없음)
//    → 200 "hop2"; target이 받은 Authorization == "Bearer ct-1",
//    X-Ephemera-Call-Hop == "1".

// 7) TestCallFlockAgent_HopGuardNeverReforwards
//    같은 hub 셋업에서 요청에 X-Ephemera-Call-Hop: 1을 붙이고 remote-1을 호출
//    → target daemon 호출 0회, 404 (로컬 해석 실패 시 즉시 에러 — 재전달 금지).

// 8) TestCallFlockAgent_HubLocalTargetByVMRegistry
//    hub flock + roster에 로컬 VM(vm-local, cp의 VM registry에 존재, httptest
//    agent)과 addr 없는 항목 → 로컬 dispatch로 200 (Addr 불필요 — 로컬 판정은
//    VM registry 존재 여부).
```

각 테스트의 단언은 위 주석 계약을 코드로 그대로 옮긴다(캡처 struct + httptest). 셋업이 기존 헬퍼와 어긋나면 기존 헬퍼를 진실원천으로 조정하되 계약(응답 코드·헤더·바디·호출 횟수)은 유지.

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/goose-daemon -run TestCallFlockAgent -count=1` / Expected: FAIL (핸들러 미존재 → 404/컴파일 에러).

- [ ] **Step 3: 구현** — `cmd/goose-daemon/orchestrator_api.go`:

`handleFlockItem` switch(~:106-156)에 추가:

```go
	case sub == "call" && r.Method == http.MethodPost:
		cp.callFlockAgent(w, r, flockID)
```

핸들러 (postToTownWall relay 분기·dispatchBroadcastTask·proxyAgentEndpoint의 depth 로직을 재조합):

```go
type FlockCallRequest struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

const callHopHeader = "X-Ephemera-Call-Hop"
const taskDepthHeader = "X-Ephemera-Task-Depth"

// callFlockAgent dispatches a prompt to a named flock member. Local/hub
// flocks resolve locally (hub falls back to its VMID/Addr roster for members
// on other hosts); relay flocks forward to the home daemon. The per-flock
// call token authenticates daemon-to-daemon hops; X-Ephemera-Call-Hop marks a
// forwarded request so no daemon ever re-forwards (loop guard). Only
// {agent_id, prompt} plus the depth/hop headers cross the wire — the target
// VM's agent token is injected by the target host daemon alone. Errors carry
// flock/host/agent identifiers only.
func (cp *ControlPlane) callFlockAgent(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	var req FlockCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" || strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent_id and prompt required"))
		return
	}
	hopped := r.Header.Get(callHopHeader) != ""

	// Relay flock: forward to home (never when this request is itself a hop).
	if f.Kind == orchestrator.FlockKindRelay {
		if hopped {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %q not resolvable on relay host", req.AgentID))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 290*time.Second)
		defer cancel()
		status, body, err := forwardFlockCall(ctx, f.HomeAddr, f.CallToken, flockID, req, r.Header.Get(taskDepthHeader))
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("call relay to home failed for flock %q", flockID))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	// Local/hub: local VM registry first (works for local flocks and for hub
	// members living on the home host — the daemon does not know its own
	// control-plane host name, so locality == VM presence).
	if vmID, ok := cp.flockAgentLocalVM(f, req.AgentID); ok {
		cp.dispatchFlockCall(w, r, vmID, req.Prompt)
		return
	}
	// Hub: remote member via VMID/Addr roster — one hop only.
	if f.Kind == orchestrator.FlockKindHub && !hopped {
		if member, ok := rosterMember(f, req.AgentID); ok && member.Addr != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 280*time.Second)
			defer cancel()
			status, body, err := forwardFlockCall(ctx, member.Addr, f.CallToken, flockID, req, r.Header.Get(taskDepthHeader))
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, fmt.Errorf("call hop to member host %q failed", member.Host))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %q not found in flock %q", req.AgentID, flockID))
}

// forwardFlockCall sends {agent_id, prompt} to addr's /flocks/{id}/call with
// the per-flock call token, marking the request as a hop and propagating the
// caller's task depth verbatim (accumulation happens only at the final local
// dispatch).
func forwardFlockCall(ctx context.Context, addr, callToken, flockID string, call FlockCallRequest, depth string) (int, []byte, error) {
	payload, _ := json.Marshal(call)
	url := strings.TrimRight(addr, "/") + "/flocks/" + flockID + "/call"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callToken)
	req.Header.Set(callHopHeader, "1")
	if depth != "" {
		req.Header.Set(taskDepthHeader, depth)
	}
	resp, err := newAgentHTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}
```

보조 함수:
- `flockAgentLocalVM(f *orchestrator.Flock, agentID string) (string, bool)` — `f.Agents[agentID]`의 VMID가 cp의 로컬 VM registry에 존재하면 그것을, 아니면 hub roster에서 agentID의 VMID를 찾아 로컬 registry에 있으면 그것을 반환 (로컬 registry 조회는 기존 proxyAgentEndpoint가 vmID→VM을 찾는 방식을 재사용 — grep으로 확인해 동일 접근자 사용).
- `rosterMember(f *orchestrator.Flock, agentID string) (orchestrator.RosterMember, bool)` — 단순 순회.
- `dispatchFlockCall(w, r, vmID, prompt)` — **depth 누적+508 가드 필수**(Trap #6): `proxyAgentEndpoint`의 depth 블록(api.go:1038-1052)과 동일 로직으로 `taskDepthHeader` 누적 후, `{"prompt": prompt}` 바디로 대상 VM agent `/tasks`에 agent token 주입 dispatch(`dispatchBroadcastTask` ~:1146-1185의 buffered 형태 재사용/추출). 응답 바디를 그대로 반환. 기존 함수를 추출·공유할 수 있으면 추출(중복 금지), 시그니처가 안 맞으면 최소 헬퍼로.

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/goose-daemon -run TestCallFlockAgent -count=1 -race` / Expected: 8종 전부 PASS. 이어서 `go test ./cmd/goose-daemon -count=1` (기존 wall/broadcast/proxy 테스트 회귀 없음).

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/flock_call_test.go
git commit -m 'feat(runtime): POST /flocks/{id}/call — resolve, dispatch, 2-hop forward with loop guard'
```

---

## Task 4: placement store — call_token 병렬 배관

**Files:**
- Modify: `internal/anvilmcp/placement_store.go`
- Test: `internal/anvilmcp/placement_store_test.go`

**Interfaces:**
- Consumes: 없음 (독립).
- Produces: `RoutedFlockRecord.callToken`(unexported carrier), `PlacementStoreState.RoutedFlockCallTokens map[string]string json:"routed_flock_call_tokens,omitempty"`, `RoutedFlockCallToken(flockID) (string, bool)` accessor, `removeRoutedFlockCallToken(flockID) error` — Task 5가 사용. **relay 배관의 10개 지점 전부 미러** (하나라도 빠지면 유출/소실/panic — 아래 체크리스트).

- [ ] **Step 1: 실패 테스트** — `internal/anvilmcp/placement_store_test.go`에 relay-token 관련 기존 테스트(grep `RoutedFlockRelayToken`)를 미러:

```go
func TestPlacementStoreCallTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := NewPlacementStore(filepath.Join(dir, "state.json"))
	rec := RoutedFlockRecord{FlockID: "routed-1", Status: RoutedFlockStatusCreating}
	rec.callToken = "ct-secret"
	if err := store.SaveRoutedFlockAndPlacements(rec, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 영속·carrier scrub 후에도 조회 가능
	if tok, ok := store.RoutedFlockCallToken("routed-1"); !ok || tok != "ct-secret" {
		t.Fatalf("call token = %q,%v want ct-secret,true", tok, ok)
	}
	// State() redaction
	if store.State().RoutedFlockCallTokens != nil {
		t.Fatal("State() must redact RoutedFlockCallTokens")
	}
	// 빈 carrier 재저장이 기존 entry를 지우지 않음
	rec2 := RoutedFlockRecord{FlockID: "routed-1", Status: RoutedFlockStatusReady}
	if err := store.SaveRoutedFlockAndPlacements(rec2, nil); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if tok, _ := store.RoutedFlockCallToken("routed-1"); tok != "ct-secret" {
		t.Fatalf("token lost on token-less re-save: %q", tok)
	}
	// 디스크 reload 후 생존 (clone/normalize 경로 검증)
	store2 := NewPlacementStore(filepath.Join(dir, "state.json"))
	if err := store2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if tok, ok := store2.RoutedFlockCallToken("routed-1"); !ok || tok != "ct-secret" {
		t.Fatalf("reloaded call token = %q,%v", tok, ok)
	}
	// revoke
	if err := store2.removeRoutedFlockCallToken("routed-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := store2.RoutedFlockCallToken("routed-1"); ok {
		t.Fatal("call token survived removal")
	}
}
```

(기존 relay 미러 테스트의 정확한 accessor 반환 형태 — `(string, bool)` vs `string` — 를 grep으로 확인해 relay와 동형으로 맞춘다. 위 코드는 `(string, bool)` 가정; relay가 `RoutedFlockRelayToken(flockID)` 단일 반환이면 그 형태를 따르고 테스트를 조정.)

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/anvilmcp -run TestPlacementStoreCallTokenLifecycle -count=1` / Expected: FAIL (컴파일).

- [ ] **Step 3: 구현** — relay 배관 10개 지점을 grep으로 열거하며 하나씩 미러 (**체크리스트 — 각 항목에 relay 쪽 참조 줄**):

1. `RoutedFlockRecord`에 `callToken string` carrier + 주석 (relay: :56-74)
2. `PlacementStoreState.RoutedFlockCallTokens map[string]string json:"routed_flock_call_tokens,omitempty"` (relay: :108-112)
3. 생성자 map init (relay: :136)
4. `State()` redaction `state.RoutedFlockCallTokens = nil` (relay: :696)
5. accessor `RoutedFlockCallToken(flockID)` (relay: :703-709)
6. `removeRoutedFlockCallToken(flockID)` (relay: :718-749)
7. `applyRoutedFlockAndPlacements` carrier 복사+scrub (relay: :909-912)
8. `normalizePlacementStoreState` nil-map guard (relay: :790-792)
9. `clonePlacementStoreState` map alloc (relay: :831)
10. `clonePlacementStoreState` copy loop (relay: :854-856)

- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/anvilmcp -run 'TestPlacementStore' -count=1` / Expected: 신규+기존 PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/anvilmcp/placement_store.go internal/anvilmcp/placement_store_test.go
git commit -m 'feat(mcp): persist per-flock call tokens (redacted, parallel to relay tokens)'
```

---

## Task 5: routed flock create/reconcile/rollback 배선 (call token 배포 + VMID/Addr roster 재등록)

**Files:**
- Modify: `internal/anvilmcp/routed_flock.go`
- Modify: `internal/anvilmcp/runtime_router.go` (`reconcileRoutedFlockWalls`)
- Test: `internal/anvilmcp/routed_flock_test.go`, `internal/anvilmcp/runtime_router_test.go`

**Interfaces:**
- Consumes: Task 1 anvilmcp `RosterMember{VMID,Addr}`, Task 2 요청 타입 `CallToken`, Task 4 store 배관.
- Produces: create가 relay/hub 등록에 `CallToken` 포함 + spawn 완료 후 `record.Agents` 기반 `{AgentID,Host,VMID,Addr}` roster로 hub **재등록**; **같은 시점에 각 member host의 relay flock도 그 host의 local agents(`Agents: [{AgentID, VMID}]` — record.Agents를 host별 필터)로 재등록** (Task 3b가 만든 `RelayFlockRequest.Agents` 사용 — target member가 hopped call을 로컬 해석하는 데 필수); reconcile 재등록도 hub roster와 relay local-agents를 동일하게 재주입; rollback/delete가 call token revoke.
- 테스트 추가 요구(3b 연동): create 성공 시 각 member fake daemon의 마지막 `relayReq.Agents`가 그 host의 agent만 담고 VMID가 채워졌는지 단언; reconcile 테스트도 동일 단언.

- [ ] **Step 1: 실패 테스트** — `internal/anvilmcp/routed_flock_test.go`에 (기존 fake `routerFakeDaemon`의 `distributedCalls/distributedReq` 카운터 활용):

```go
// 1) TestCreate_ReRegistersHubWithVMIDRoster
//    기존 create 성공 테스트(TestCreateRoutedFlockMembers_WiresSharedTownWall
//    류)의 셋업 복사. 단언:
//    - home fake의 distributedCalls >= 2 (spawn 전 최초 + spawn 후 재등록)
//    - 마지막 distributedReq.Roster의 각 member에 VMID(=fake spawn이 돌려준
//      vm id)와 Addr(=r.daemonAddr(host) 값) 존재
//    - 최초·재등록 요청 모두 CallToken 비어있지 않고 RelayToken과 다름
// 2) TestCreate_PersistsCallTokenBeforeHubRegistration
//    기존 TestCreate_PersistsRelayTokenBeforeHubRegistration(:100)과
//    relayTokenProbeDaemon(:81-93) 미러 — probe가 RegisterDistributedFlock
//    시점에 store.RoutedFlockCallToken이 이미 영속됐음을 단언.
// 3) TestRollback_RevokesCallToken
//    기존 TestRollback_DeregistersSharedTownWall(:17) 셋업에서 rollback 후
//    store.RoutedFlockCallToken(flockID)가 없음을 단언.
// 4) TestDeleteRoutedFlock_RevokesCallToken
//    TestDeleteRoutedFlock_DeregistersSharedTownWall(:244) 미러.
```

`internal/anvilmcp/runtime_router_test.go`에:

```go
// 5) TestReconcileReRegistersCallTokenAndVMIDRoster
//    기존 reconcile wall 테스트 셋업 복사(레코드에 Agents(VMID 보유) + 저장된
//    relay/call token). 단언: 재등록 distributedReq.Roster에 VMID/Addr 포함,
//    CallToken == 저장값, relay 재등록 요청에도 CallToken 포함.
```

각 주석 시나리오를 실제 테스트 코드로 작성한다 — 기존 미러 대상 테스트의 본문이 진실원천.

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/anvilmcp -run 'TestCreate_ReRegistersHub|TestCreate_PersistsCallToken|TestRollback_RevokesCallToken|TestDeleteRoutedFlock_RevokesCallToken|TestReconcileReRegistersCallToken' -count=1` / Expected: FAIL.

- [ ] **Step 3: 구현** — `internal/anvilmcp/routed_flock.go`:

1. `newCallToken()` — `newRelayToken`(:313-320)과 동일 구현(32 rand bytes hex). 공용 helper로 추출해도 좋다(`newFlockSecret()` 하나를 둘이 호출).
2. create 흐름(:103-133): `callToken, err := newCallToken()` 생성, `record.callToken = callToken`을 relay carrier 옆에서 설정 → 첫 save가 둘 다 영속 → 둘 다 scrub.
3. hub 등록(:151)과 relay 등록(:171) 요청에 `CallToken: callToken` 추가. spawn 요청의 `ControlPlaneToken`은 **기존대로 relayToken** (guest gtwall 경로 불변 — gtcall의 guest CP token도 같은 것을 쓰며, `/call` admission은 daemon hop용 call_token과 별개로 guest는 기존 CP bearer로 로컬 daemon에 인증한다. 변경 없음).
4. **spawn 루프 완료 후**(:268 부근, `record.Status = RoutedFlockStatusReady` 직전) VMID/Addr roster 재등록:

```go
	// Re-register the hub with the VMID/Addr-enriched roster: the initial
	// pre-spawn registration cannot know VM ids, and the hub needs them (plus
	// each member host daemon's address) to resolve and hop cross-host calls.
	// The daemon's idempotent hub path updates the roster in place (Task 1).
	enriched := make([]RosterMember, 0, len(record.Agents))
	for _, a := range record.Agents {
		enriched = append(enriched, RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			Host:    strings.TrimSpace(a.Host),
			VMID:    strings.TrimSpace(a.VMID),
			Addr:    r.daemonAddr(strings.TrimSpace(a.Host)),
		})
	}
	if err := homeDaemon.RegisterDistributedFlock(ctx, record.FlockID, DistributedFlockRequest{Roster: enriched, RelayToken: relayToken, CallToken: callToken}); err != nil {
		return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
			Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
			Reason:              FlockPlacementReasonDaemonCreateFailed,
			PlanLatency:         planLatency,
			AgentSpawnLatency:   agentSpawnLatency,
			RegistrySaveLatency: registrySaveLatency,
			TotalStart:          totalStart,
		}, relayHosts...)
	}
```

5. `deregisterRoutedFlockWall`(:403-405)에 `_ = r.placementStore.removeRoutedFlockCallToken(flockID)` 추가.

`internal/anvilmcp/runtime_router.go` `reconcileRoutedFlockWalls`:
- roster 빌드(:378-381)에 `VMID: strings.TrimSpace(a.VMID)`, `Addr: r.daemonAddr(strings.TrimSpace(a.Host))` 추가.
- 저장된 call token 조회(`RoutedFlockCallToken`) — 없으면 record skip은 **하지 말고** relay token만으로 wall 재등록은 계속(하위호환: call token 없는 구 레코드), call token 있으면 hub/relay 요청에 포함.

- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/anvilmcp -count=1 -race` / Expected: 신규 5종 + 기존 routed/reconcile 전부 PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/anvilmcp
git commit -m 'feat(mcp): distribute call tokens and VMID/Addr roster through create/reconcile/rollback'
```

---

## Task 6: scripts/gtcall 전환 (단일 /call 호출)

**Files:**
- Modify: `scripts/gtcall`
- Test: `bash -n scripts/gtcall` + Task 7 e2e가 실경로 검증

**Interfaces:**
- Consumes: Task 3의 `/flocks/{id}/call` 계약.
- Produces: guest `gtcall <agent_id> <prompt>`가 step1(flock 조회)+step3(vm task) 대신 단일 `POST $CP_URL/flocks/$FLOCK_ID/call`을 호출. `DEPTH_HEADER`·`CP_TOKEN`·`-m 300`·`jq -r '.output // empty'` 파싱 유지. jq의 flock JSON 파싱 의존 제거.

- [ ] **Step 1: 구현** — `scripts/gtcall`의 Step 1(:64-86 flock 조회+vm_id 해석)과 Step 3(:88-112 /vms/$VM_ID/tasks 호출)을 다음 단일 호출로 교체 (헤더·주석·에러 exit code 체계는 기존 스타일 유지):

```sh
# Dispatch the prompt to the named agent through the local control plane's
# flock call endpoint. The daemon resolves the agent (locally or via the
# routed-flock home daemon) and injects the target VM's agent token itself —
# the calling agent never learns peer credentials or peer addresses.
BODY="$(jq -n --arg id "$AGENT_ID" --arg p "$PROMPT" '{agent_id: $id, prompt: $p}')"

if [ -n "$CP_TOKEN" ]; then
    RESP="$(curl -sf -m 300 \
        -H "Authorization: Bearer $CP_TOKEN" \
        -H 'Content-Type: application/json' \
        ${DEPTH_HEADER:+ -H "$DEPTH_HEADER"} \
        -X POST -d "$BODY" \
        "$CP_URL/flocks/$FLOCK_ID/call")" || {
        echo "gtcall: call to $AGENT_ID failed" >&2
        exit 5
    }
else
    RESP="$(curl -sf -m 300 \
        -H 'Content-Type: application/json' \
        ${DEPTH_HEADER:+ -H "$DEPTH_HEADER"} \
        -X POST -d "$BODY" \
        "$CP_URL/flocks/$FLOCK_ID/call")" || {
        echo "gtcall: call to $AGENT_ID failed" >&2
        exit 5
    }
fi
```

파일 상단 주석의 흐름 설명도 새 계약으로 갱신. `FLOCK_ID` 파싱·`AGENT_ID`/`PROMPT` 검증·출력 파싱(`jq -r '.output // empty'`)은 유지.

- [ ] **Step 2: 검증** — Run: `bash -n scripts/gtcall && shellcheck scripts/gtcall || true` (shellcheck 미설치면 bash -n만). 기존에 없던 새 SC 지적을 만들지 않는다.

- [ ] **Step 3: 커밋**

```bash
git add scripts/gtcall
git commit -m 'feat(guest): gtcall uses the single /flocks/{id}/call contract'
```

(provisioner `EnsureGoldenImage`가 scripts/gtcall 변경을 감지해 golden image를 자동 rebuild한다 — 별도 조치 불요, e2e에서 실경로 확인.)

---

## Task 7: KVM e2e — cross-host gtcall (real member + stub home 왕복)

**Files:**
- Create: `scripts/anvil-cross-host-gtcall-e2e.sh`
- 검증: `bash -n`, KVM host에서 `sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh`

**Interfaces:**
- Consumes: Tasks 1-6 전부.
- Produces: 검사 항목 — real member VM의 `gtcall remote-agent "<prompt>"`가 member daemon(relay)→stub home으로 전달되고, stub 응답이 guest stdout까지 왕복.

- [ ] **Step 1: 스크립트 작성** — `scripts/anvil-cross-host-wall-e2e.sh`를 복사·개작한다 (헬퍼 `step/ok/fail/require_cmd/cleanup`, `CURL_AUTH_ARGS`, `CAPTURE_FILE`, relay 등록, VM 생성, workload 업로드 구조 전부 재사용):

핵심 차이:
0. **member daemon을 auth-on으로 기동** (`EPHEMERA_API_TOKENS="operator:$OP_TOKEN"` 설정, 컨트롤러 curl은 그 토큰 사용) — guest의 relay-token admission 경로를 실경로로 검증한다(wall e2e가 auth-off라 놓친 결함의 재발 방지). guest gtcall은 주입된 `.ephemera-cp-token`(=relay token)으로 member daemon `/call`을 통과해야 한다.
1. relay 등록 요청에 `"call_token":"ct-e2e"` 추가 (`{home_addr, relay_token, call_token}`).
2. stub home의 `do_POST`가 `/flocks/$FLOCK_ID/call` 수신 시 기록 후 `200 {"output":"CROSSHOST_REPLY_OK"}` 반환 (wall e2e의 기록-only stub에 응답 바디 추가).
3. in-guest workload가 `gtwall` 대신:

```sh
OUT="$(gtcall remote-researcher "ping from member")" || exit 1
[ "$OUT" = "CROSSHOST_REPLY_OK" ] || { echo "unexpected reply: $OUT" >&2; exit 1; }
echo "GTCALL_ROUNDTRIP_OK"
```

4. 단언(wall e2e :364-408 패턴):
   - capture에 `"path": "/flocks/$FLOCK_ID/call"` 존재
   - body가 `{"agent_id":"remote-researcher","prompt":"ping from member"}` 정확히 (그 외 필드 없음)
   - `headers.authorization == "Bearer ct-e2e"` (relay token `rt-e2e` **아님** — 별도 단언으로 rt-e2e 부재 확인)
   - `x-ephemera-call-hop == "1"`, `x-ephemera-task-depth` 존재
   - capture 전체에 `agent_token` 문자열·실제 per-VM agent token 값·CP token 값 부재 (sentinel)
   - workload 출력에 `GTCALL_ROUNDTRIP_OK` (왕복 성립)
5. 성공 echo에 "two-daemon full integration remains a MANUAL multi-host check" 주석 유지(wall e2e와 동일 사유 — 단일 host에서 두 real daemon은 guest bridge 충돌).

- [ ] **Step 2: 정적 검증** — Run: `bash -n scripts/anvil-cross-host-gtcall-e2e.sh` / Expected: EXIT 0.

- [ ] **Step 3: KVM 실행** — Run: `sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh` / Expected: 전 검사 ✓, EXIT 0. (KVM 전제 조건은 wall e2e와 동일: gitignored `configs/goose.yaml`·`goose-secrets.yaml`. sudo 불가 환경이면 이 step만 보류하고 보고에 명시.)

- [ ] **Step 4: 커밋**

```bash
git add scripts/anvil-cross-host-gtcall-e2e.sh
git commit -m 'test(runtime): cross-host gtcall e2e — real member relays call to stub home and returns reply'
```

---

## Task 8: 문서 반영

**Files:**
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md`, `docs/ADR_INDEX.md` (call_token/call 경로 행 — cross-host wall 행 옆, 동일 형식)
- Modify: `CONTEXT.md` (용어집 call_token 행 — relay_token 행 미러; 완료 상태 목록에 cross-host gtcall 항목; "남은 후속 후보"에서 cross-host gtcall 제거)
- Modify: `README.md` (routed flock 서술의 "cross-host gtcall 비목표" 문구 갱신 + `/flocks/{id}/call` endpoint 목록 추가), `docs/architecture/service-logic.md` (endpoint 표 + authMiddleware call admission 1-2줄), `docs/architecture/mcp-architecture.md` (비목표에서 gtcall 제거, broadcast fan-out만 유지)
- Modify: `docs/operations/runbook.md` (e2e 목록에 gtcall e2e), `docs/operations/2026-07-06-cross-host-town-wall-handoff.md` (Follow-Up "cross-host gtcall" CLOSED)
- Create: `docs/operations/2026-07-07-cross-host-gtcall-handoff.md` (wall/reconcile handoff 형식: 무엇이 배포됐나/보안 경계/Gate 결과/Known limitations/Next Action/Follow-Up)

**Interfaces:** 없음 (docs-only).

- [ ] **Step 1: 각 파일 갱신** — 사실 원천은 구현된 코드와 spec. call_token 서술은 반드시 "해당 flock의 `/flocks/{id}/call`만 admit, wall 경로·CP bearer 불가, 전 표면 redaction"을 포함. 각 문서의 기존 어조·형식(표/목록/취소선 CLOSED) 유지.
- [ ] **Step 2: 검증** — `git diff --check` clean; 갱신한 서술이 코드 사실과 일치하는지 교차 확인(특히 endpoint 표의 메서드/경로, 용어집의 admit 규칙).
- [ ] **Step 3: 커밋**

```bash
git add CONTEXT.md README.md docs/
git commit -m 'docs: document cross-host gtcall (call_token, /flocks/{id}/call)'
```

---

## Final verification gate (after all tasks)

```bash
go test ./cmd/... ./internal/... -count=1
go build ./cmd/goose-daemon ./cmd/anvil-mcp ./cmd/anvil-scheduler ./cmd/ephemera-ctl
git diff --check
bash -n scripts/gtcall scripts/anvil-cross-host-gtcall-e2e.sh
# KVM host:
go build -o anvil-daemon ./cmd/goose-daemon/
sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh
sudo -n bash scripts/anvil-cross-host-wall-e2e.sh   # wall 회귀 없음
scripts/anvil-mcp-e2e.sh flock                       # 단일 host flock 회귀 없음
```

Expected: 전체 유닛 suite green; 4 builds; gtcall e2e 왕복 green; wall e2e 17/17 유지; IronClaw schema-exclusion·token-redaction guard 전부 pass (신규 anvil_* tool 없음).
