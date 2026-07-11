# Home 재선출 Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** routed flock의 home host가 죽으면 adapter reconcile 루프가 연속 dial-실패 3회를 감지해 생존 member host를 결정적으로 새 home으로 재선출하고, 기존 relay/call token 그대로 hub/relay 토폴로지를 재구성한다 (wall 과거 기록 손실은 명시 수용).

**Architecture:** 설계 확정본 [`docs/superpowers/specs/2026-07-08-home-failover-design.md`](../specs/2026-07-08-home-failover-design.md). 감지·선출·전환은 전부 adapter(`internal/anvilmcp`)의 기존 reconcile 루프 안에서 일어난다(새 프로세스 없음). daemon 쪽은 kind 전환 두 경로(relay→hub 승격, hub→relay 강등)만 연다 — **spec의 "기존 배관 재사용" 가정과 달리 D1 fix(PR #30)의 kind 충돌 409 가드 때문에 이 daemon 변경 없이는 전환이 성립하지 않는다** (2026-07-11 코드 조사로 확정된 spec 보정, 아래 "Spec 보정 사항").

**Tech Stack:** Go (기존 의존성만, 신규 dependency 금지), bash KVM e2e.

## Global Constraints

- 설계 계약 원문: `docs/superpowers/specs/2026-07-08-home-failover-design.md` — 비목표(wall 복제·병합, 자동 fail-back, 다중 adapter 분산 선출, 임계 설정화)를 절대 침범하지 않는다.
- `homeFailureThreshold = 3` — **상수. 환경 변수/설정화 금지** (spec: bounded retry와 동일 YAGNI 방침).
- dial-계열 실패만 감지 대상: `net.OpError.Op == "dial"` (bounded relay retry의 `isDialError`와 동일 판정). HTTP 응답·reset/EOF·ctx 취소는 카운트하지 않는다.
- **토큰 불변**: relay/call token은 flock 단위 그대로 재사용 — guest 주입 토큰이 바뀌지 않으므로 guest 무중단. 토큰은 daemon 디스크에 절대 영속 금지(`FlockMetadata`에 토큰 필드 추가 금지 — 기존 sentinel test가 고정), adapter 쪽은 `PlacementStore`의 전용 redacted map만.
- **redaction 규율**: 에러 문자열·로그 라인은 flock/host 식별자만. daemon 주소(`Endpoint`/`HomeAddr`)와 토큰은 어떤 로그/에러/직렬화 표면에도 금지.
- **hub Agents 불변식(신규, 이 slice가 도입)**: hub kind flock의 `f.Agents`는 항상 빈 map — `deleteFlock`이 `f.Agents` 기반으로 VM을 파괴하므로, 이 불변식이 깨지면 구 home stale hub의 best-effort DELETE가 로컬 member VM을 죽인다. 승격 경로도 이 불변식을 유지해야 한다.
- 커밋: **git trailer 금지** (anvil 브랜치 컨벤션 — Co-Authored-By 넣지 말 것). 작은 단위로 자주 커밋.
- 검증: 각 태스크에서 `go test -race` 포함 (D1b가 -race로 검증된 계열의 코드를 만진다).
- main 직접 push 금지 — 브랜치 `feature/home-failover`(main 팁에 이미 존재, 재사용) + PR 경로. 자체 머지 금지.
- 워커 파견 시 모든 Bash는 `cd <worktree> &&`로 시작, 커밋 전 `git branch --show-current` 확인(main이면 BLOCKED).

## Spec 보정 사항 (2026-07-11 코드 조사 결과 — 리뷰어 주의)

spec §전환 절차는 "기존 배관 재사용(reconcile 재등록 코드 그대로)"을 전제했으나, spec 작성(07-08) 이후 머지된 D1 fix(PR #30, 07-10)가 두 등록 핸들러에 kind 충돌 409 가드를 넣었다:

- `registerDistributedFlock`: 기존 flock의 kind가 hub가 아니면 409 (`cmd/goose-daemon/orchestrator_api.go:1509`).
- `registerRelayFlock`: 기존 flock의 kind가 relay가 아니면 409 (`cmd/goose-daemon/orchestrator_api.go:1567`).

failover의 새 home은 **반드시 기존 relay flock을 가진 member host**이므로, 승격 hub 등록이 무조건 409 → 전환이 영구 불성립. 또한 구 home 부활 시(D1 recovery가 hub kind 복원) relay 재등록도 409 → 영구 heal 실패. 따라서 이 slice는 daemon 쪽에 다음 두 전환을 연다 (Task 1, 2):

- **relay→hub 승격** (`POST /flocks/{id}/distributed` 위에): CP bearer 인증 요청만 이 endpoint에 도달 가능(relay/call token admit 목록에 등록 endpoint 없음). local flock은 계속 409.
- **hub→relay 강등** (`POST /flocks/{id}/relay` 위에): 동일 인증 경계. local flock은 계속 409. wall log 파일은 디스크에 남는다(기존 `DeleteFlockMetadata`의 audit artifact 관례와 동일).

권위 논거: 두 endpoint 모두 full control-plane bearer 뒤에 있고, 같은 bearer는 이미 flock DELETE(더 파괴적)를 허용한다. elector가 단일 control plane이라는 spec의 전제 그대로다.

추가 선행 보정 (Task 3): 현재 `ReconcilePlacements`는 **어느 한 host의 `ListVMs`가 실패하면 전체를 조기 반환**해 다른 host의 wall heal까지 모두 중단된다 (`internal/anvilmcp/runtime_router.go:283-286`). home이 죽어 있는 동안 reconcile이 아무것도 못 하므로 failover 감지 자체가 불가능 — per-host 격리로 바꾼다 (죽은 host의 기존 placement는 보존).

## File Structure

| 파일 | 책임 |
|---|---|
| `cmd/goose-daemon/orchestrator_api.go` (수정) | 등록 핸들러 2곳의 kind 전환 허용 (승격/강등) |
| `cmd/goose-daemon/orchestrator_api_test.go` (수정) | 승격/강등 핸들러 테스트 |
| `internal/anvilmcp/runtime_router.go` (수정) | `ReconcilePlacements` per-host 격리 + probe 수집, `reconcileRoutedFlockWalls` 시그니처 변경 + 재등록 helper 추출 + failover 감지 훅, `StartReconcileLoop`의 logf 보관 |
| `internal/anvilmcp/home_failover.go` (신규) | dial 분류, `homeFailureThreshold`, 결정적 선출(`electNewHome`), 전환(`failoverRoutedFlock`) |
| `internal/anvilmcp/home_failover_test.go` (신규) | 감지·선출·전환 유닛 테스트 (fake daemon) |
| `internal/anvilmcp/runtime_router_test.go` (수정) | fake에 `listVMErr` 추가, 격리 테스트, 기존 reconcile 테스트 시그니처 정합 |
| `scripts/anvil-cross-host-failover-e2e.sh` (신규) | 2-phase KVM e2e (stub 재선출 + real daemon 승격) |
| `docs/ADR_INDEX.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `CONTEXT.md`, `docs/operations/runbook.md`, `docs/operations/2026-07-08-cross-host-manual-verification.md`, `docs/operations/2026-07-11-home-failover-handoff.md` (수정/신규) | SPOF 서술 갱신, wall 손실 계약 명시, 수동 검증 §6 확장, handoff |

**Interfaces 계약 전체 요약** (태스크 간 공유 — 각 태스크의 Interfaces 블록이 원문):

```go
// internal/anvilmcp/home_failover.go (Task 4)
const homeFailureThreshold = 3
func isDialError(err error) bool
func (r *RuntimeRouter) electNewHome(record RoutedFlockRecord, probes map[string]hostProbe) (string, bool)
func (r *RuntimeRouter) failoverRoutedFlock(ctx context.Context, record RoutedFlockRecord, newHome, relayToken, callToken string) (switched bool, err error)

// internal/anvilmcp/runtime_router.go (Task 3)
type hostProbe struct{ reachable, dialFailed bool }
func (r *RuntimeRouter) reconcileRoutedFlockWalls(ctx context.Context, probes map[string]hostProbe) error
func (r *RuntimeRouter) registerRoutedHub(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) error
func (r *RuntimeRouter) registerRoutedRelays(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) []error
func (r *RuntimeRouter) logf(format string, args ...any) // nil-safe; StartReconcileLoop가 r.reconcileLogf 설정
// RuntimeRouter 신규 필드: homeFailures map[string]int (reconcileMu 하에서만 접근), reconcileLogf func(string, ...any)
```

---

### Task 1: daemon — relay→hub 승격 (`registerDistributedFlock`)

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go:1504-1539` (registerDistributedFlock의 기존-flock 분기)
- Test: `cmd/goose-daemon/orchestrator_api_test.go`

**Interfaces:**
- Consumes: 기존 `orchestrator.FlockManager.RegisterHub` / `Get` / `NewTownWall`, `cp.setRelayToken` / `cp.setCallToken` (변경 없음)
- Produces: `POST /flocks/{id}/distributed`가 relay kind 기존 flock 위에서 201로 승격 (Task 4의 adapter 전환과 Task 5 e2e가 의존). local flock 409 계약은 불변.

- [ ] **Step 1: 실패하는 테스트 작성**

`cmd/goose-daemon/orchestrator_api_test.go`에 추가 (기존 `TestRegisterDistributedAndRelayFlock` 아래):

```go
// TestRegisterDistributedFlock_PromotesRelayToHub proves the failover promotion
// path: a member daemon that holds a RELAY flock for this id accepts a hub
// registration from the control plane (the adapter re-elected this host as the
// new home) instead of 409ing. The promoted hub follows RegisterHub semantics:
// fresh wall, roster from the request, EMPTY Agents map (the deleteFlock
// VM-safety invariant), kind persisted as hub for restart recovery.
func TestRegisterDistributedFlock_PromotesRelayToHub(t *testing.T) {
	cp := newTestCP(t)

	// Seed: this daemon is a member — relay flock with local agents.
	relayBody := `{"home_addr":"http://old-home:3000","relay_token":"rt-1","call_token":"ct-1","agents":[{"agent_id":"researcher-1","vm_id":"vm-r1"}]}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(relayBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed relay register = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	// Failover: the adapter promotes this host to home. SAME tokens (guest-transparent).
	hubBody := `{"roster":[{"agent_id":"coordinator-1","host":"hostA","vm_id":"vm-c1"},{"agent_id":"researcher-1","host":"hostB","vm_id":"vm-r1"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr2 := httptest.NewRecorder()
	cp.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(hubBody)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("promote relay->hub = %d, want 201 (%s)", rr2.Code, rr2.Body.String())
	}

	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f.Kind != orchestrator.FlockKindHub {
		t.Fatalf("flock not promoted to hub: %+v", f)
	}
	if f.TownWall == nil {
		t.Fatal("promoted hub has no town wall")
	}
	if len(f.RosterSnapshot()) != 2 {
		t.Fatalf("promoted hub roster = %d members, want 2", len(f.RosterSnapshot()))
	}
	// deleteFlock VM-safety invariant: hub Agents stays EMPTY (local resolution
	// uses roster VMID + cp.vms presence, same as a first-generation home).
	if len(f.Snapshot()) != 0 {
		t.Fatalf("promoted hub Agents = %d, want 0 (deleteFlock VM-safety invariant)", len(f.Snapshot()))
	}
	// Tokens admitted for inbound guest/hop auth.
	if cp.relayTokenFor("routed-1") != "rt-1" || cp.callTokenFor("routed-1") != "ct-1" {
		t.Fatalf("promotion did not admit tokens: relay=%q call=%q", cp.relayTokenFor("routed-1"), cp.callTokenFor("routed-1"))
	}
	// Kind persisted so a restart recovers a hub, not a relay.
	meta, err := orchestrator.LoadFlockMetadata(cp.workDir, "routed-1")
	if err != nil {
		t.Fatalf("load persisted metadata: %v", err)
	}
	if meta.Kind != orchestrator.FlockKindHub {
		t.Fatalf("persisted kind = %q, want hub", meta.Kind)
	}
}
```

주의: `f.RosterSnapshot()`/`f.Snapshot()`은 D1b가 도입한 locked accessor다. 만약 `Snapshot()` 이름이 다르면(agents snapshot 계열) `internal/orchestrator/flock.go`에서 실제 이름을 확인해 맞춘다 — raw `f.Agents`/`f.Roster` 접근은 금지(-race 위반 계열).

- [ ] **Step 2: 실패 확인**

```bash
go test ./cmd/goose-daemon/ -run TestRegisterDistributedFlock_PromotesRelayToHub -v
```
Expected: FAIL — `promote relay->hub = 409, want 201`

- [ ] **Step 3: 최소 구현**

`registerDistributedFlock`의 기존-flock 분기를 재구성한다. 현재 코드:

```go
	if existing, ok := cp.flockMgr.Get(flockID); ok {
		// A non-hub flock already owns this id ...
		if existing.Kind != orchestrator.FlockKindHub {
			writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-hub flock", flockID))
			return
		}
		... (멱등 재등록 경로) ...
		return
	}
```

변경 후:

```go
	if existing, ok := cp.flockMgr.Get(flockID); ok {
		switch existing.Kind {
		case orchestrator.FlockKindHub:
			// (기존 멱등 재등록 경로 본문 그대로 — roster/token 재주입 + 201 return)
			...
			return
		case orchestrator.FlockKindRelay:
			// Failover promotion: the adapter (the sole control plane; this
			// endpoint sits behind the full CP bearer — relay/call tokens are
			// never admitted here) re-elected this member host as the flock's
			// new home. Fall through to the fresh hub registration below:
			// RegisterHub replaces the relay stub, local agent resolution moves
			// to the VMID-enriched roster + cp.vms presence (identical to a
			// first-generation home), and the hub's Agents map stays EMPTY so a
			// later best-effort DELETE of a stale failed-over hub can never
			// destroy this host's member VMs (deleteFlock destroys f.Agents).
		default:
			// A local flock owns this id: an id collision, never a failover.
			writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-hub flock", flockID))
			return
		}
	}
	wallPath := filepath.Join(cp.workDir, "flocks", flockID, "TOWN_WALL.log")
	... (기존 fresh 등록 경로 그대로: NewTownWall + RegisterHub + setRelayToken/setCallToken + 201)
```

참고: `NewTownWall`은 append-open이므로 첫 승격 시 새 빈 log가 생긴다(spec의 wall 손실 semantics). 같은 host가 과거에 home이었다가 재승격되면 남아 있던 log를 이어 쓴다 — seq 단조성 유지, 허용 동작.

- [ ] **Step 4: 통과 확인 + 기존 가드 회귀 확인**

```bash
go test ./cmd/goose-daemon/ -run 'TestRegisterDistributedFlock' -v
```
Expected: PASS — 신규 테스트 + `TestRegisterDistributedFlock_RejectsDuplicateNonHubID`(local 409 불변) + `_ReAdmitsRelayTokenOnReRegister` + `_UpdatesRosterOnReRegister` 전부 green.

- [ ] **Step 5: 패키지 전체 + race**

```bash
go test -race ./cmd/goose-daemon/ ./internal/orchestrator/
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/orchestrator_api_test.go
git commit -m "feat(runtime): promote relay flock to hub on distributed re-registration"
```

---

### Task 2: daemon — hub→relay 강등 (`registerRelayFlock`)

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go:1564-1570` (registerRelayFlock의 kind 가드)
- Test: `cmd/goose-daemon/orchestrator_api_test.go`

**Interfaces:**
- Consumes: 기존 `FlockManager.RegisterRelay` (변경 없음)
- Produces: `POST /flocks/{id}/relay`가 hub kind 기존 flock 위에서 201로 강등. 구 home 부활 후 reconcile heal이 이 경로로 수렴 (Task 4 테스트 시나리오와 Task 5 e2e가 의존). local flock 409 계약 불변.

- [ ] **Step 1: 실패하는 테스트 작성**

```go
// TestRegisterRelayFlock_DemotesHubToRelay proves the failover demotion path: a
// revived old home still holding the stale HUB flock (restart recovery restores
// kind=hub) accepts a relay registration from the control plane — the adapter's
// record now points at the NEW home — instead of 409ing forever. The wall log
// file stays on disk (audit artifact, same convention as DeleteFlockMetadata);
// the demoted flock forwards to the new home and resolves its own local agents.
func TestRegisterRelayFlock_DemotesHubToRelay(t *testing.T) {
	cp := newTestCP(t)

	// Seed: this daemon WAS the home — hub flock with a canonical wall.
	hubBody := `{"roster":[{"agent_id":"coordinator-1","host":"hostA","vm_id":"vm-c1"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(hubBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed hub register = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	wallPath := filepath.Join(cp.workDir, "flocks", "routed-1", "TOWN_WALL.log")

	// Failover happened elsewhere; reconcile now demotes this host to a member.
	relayBody := `{"home_addr":"http://new-home:3000","relay_token":"rt-1","call_token":"ct-1","agents":[{"agent_id":"coordinator-1","vm_id":"vm-c1"}]}`
	rr2 := httptest.NewRecorder()
	cp.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(relayBody)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("demote hub->relay = %d, want 201 (%s)", rr2.Code, rr2.Body.String())
	}

	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f.Kind != orchestrator.FlockKindRelay {
		t.Fatalf("flock not demoted to relay: %+v", f)
	}
	if f.HomeAddr != "http://new-home:3000" {
		t.Fatalf("demoted relay HomeAddr = %q, want new home", f.HomeAddr)
	}
	// Local agents resolvable for hopped calls.
	if len(f.Snapshot()) != 1 {
		t.Fatalf("demoted relay Agents = %d, want 1", len(f.Snapshot()))
	}
	// Old wall history remains on disk (spec: 이전 기록은 구 home 디스크에 남는다).
	if _, err := os.Stat(wallPath); err != nil {
		t.Fatalf("old wall log removed by demotion: %v", err)
	}
	// Kind persisted so a restart recovers a relay, not a hub.
	meta, err := orchestrator.LoadFlockMetadata(cp.workDir, "routed-1")
	if err != nil {
		t.Fatalf("load persisted metadata: %v", err)
	}
	if meta.Kind != orchestrator.FlockKindRelay || meta.HomeAddr != "http://new-home:3000" {
		t.Fatalf("persisted kind/home_addr = %q/%q, want relay/new-home", meta.Kind, meta.HomeAddr)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./cmd/goose-daemon/ -run TestRegisterRelayFlock_DemotesHubToRelay -v
```
Expected: FAIL — `demote hub->relay = 409, want 201`

- [ ] **Step 3: 최소 구현**

현재 가드:

```go
	if existing, ok := cp.flockMgr.Get(flockID); ok && existing.Kind != orchestrator.FlockKindRelay {
		writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-relay flock", flockID))
		return
	}
```

변경 후:

```go
	// A LOCAL flock owning this id is an id collision — refuse (unchanged). A
	// hub-kind occupant is the failover demotion path: the control plane's
	// record now names another host as home, so this (revived old-home) daemon
	// converts its stale hub into a relay stub. RegisterRelay below overwrites
	// the registry entry and persists kind=relay; the old TOWN_WALL.log stays
	// on disk as an audit artifact (spec: wall history is lost, not merged).
	// Relay->relay re-register remains the reconcile heal overwrite.
	if existing, ok := cp.flockMgr.Get(flockID); ok && existing.Kind == orchestrator.FlockKindLocal {
		writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-relay flock", flockID))
		return
	}
```

- [ ] **Step 4: 통과 + 회귀 확인**

```bash
go test ./cmd/goose-daemon/ -run 'TestRegisterRelayFlock' -v
```
Expected: PASS — 신규 + `_RejectsDuplicateNonRelayID`(local 409 + relay→relay heal 불변) + `_AdmitsRelayAndCallTokens` + `_StoresLocalAgents` 전부 green.

- [ ] **Step 5: 패키지 전체 + race**

```bash
go test -race ./cmd/goose-daemon/ ./internal/orchestrator/
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/orchestrator_api_test.go
git commit -m "feat(runtime): demote stale hub to relay on relay re-registration"
```

---

### Task 3: adapter — `ReconcilePlacements` per-host 격리 + probe + 재등록 helper 추출

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go:269-306` (ReconcilePlacements), `:356-437` (reconcileRoutedFlockWalls)
- Test: `internal/anvilmcp/runtime_router_test.go` (fake에 `listVMErr` 추가 + 격리 테스트)

**Interfaces:**
- Consumes: 기존 `PlacementStore.ReplaceVMPlacements/Save/ListRoutedFlocks/RoutedFlock{Relay,Call}Token`, `Daemon.Register{Distributed,Relay}Flock`
- Produces (Task 4가 사용):
  - `type hostProbe struct{ reachable, dialFailed bool }` — reconcile 1-pass의 host 도달성 관측. `reachable`: `ListVMs` 성공. `dialFailed`: `ListVMs`가 dial-계열로 실패.
  - `func (r *RuntimeRouter) reconcileRoutedFlockWalls(ctx context.Context, probes map[string]hostProbe) error`
  - `func (r *RuntimeRouter) registerRoutedHub(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) error` — VMID/Addr-enriched roster 구성 + home daemon에 `RegisterDistributedFlock`. home daemon client 부재도 error로 반환.
  - `func (r *RuntimeRouter) registerRoutedRelays(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) []error` — home 제외 각 member host에 host-local agents 포함 `RegisterRelayFlock`. 에러는 host 단위 수집(식별자만).
- 주의: `isDialError`는 Task 4에서 정의된다. Task 3 시점에는 probe의 `dialFailed`를 채우기 위해 Task 3 안에서 함께 정의해도 된다(파일은 `internal/anvilmcp/home_failover.go`, 아래 Step 3 코드 참조) — Task 4는 그것을 재사용한다.

- [ ] **Step 1: fake 확장 + 실패하는 테스트 작성**

`internal/anvilmcp/runtime_router_test.go`의 `routerFakeDaemon` struct에 필드 추가:

```go
	listVMErr              error
```

`ListVMs` 수정:

```go
func (f *routerFakeDaemon) ListVMs(context.Context) ([]VMInfo, error) {
	if f.listVMErr != nil {
		return nil, f.listVMErr
	}
	return f.listVMResp, nil
}
```

테스트 추가 (기존 `TestReconcile_ReregistersSharedTownWall` 패턴 재사용 — 같은 scheduler/host 세팅):

```go
// TestReconcilePlacements_IsolatesUnreachableHost proves one dead daemon no
// longer aborts the whole reconcile pass: the reachable host's placements are
// rebuilt, the dead host's existing placements are carried over (not wiped),
// wall healing still runs for the reachable side, and the pass reports the
// failure in its joined error instead of returning early.
func TestReconcilePlacements_IsolatesUnreachableHost(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	member := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-researcher-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil, nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)
	if _, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task: "smoke", Roles: []string{"coordinator", "researcher"},
		TenantID: "tenant-1", EgressPolicy: "profile",
	}); err != nil {
		t.Fatal(err)
	}

	// hostA dies (dial failure), hostB stays reachable with its VM listed.
	home.listVMErr = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	member.listVMResp = []VMInfo{{VMID: "vm-researcher-1"}}
	member.relayCalls = 0

	err := router.ReconcilePlacements(context.Background())
	if err == nil {
		t.Fatal("reconcile with a dead host must surface the failure in its error")
	}
	// Dead host's placement carried over, reachable host's rebuilt.
	if host, ok := router.Placement("vm-coordinator-1"); !ok || host != "hostA" {
		t.Fatalf("dead host placement wiped: %q %v", host, ok)
	}
	if host, ok := router.Placement("vm-researcher-1"); !ok || host != "hostB" {
		t.Fatalf("reachable host placement lost: %q %v", host, ok)
	}
	// Wall healing still ran for the reachable member (home hub POST fails —
	// collected, not fatal).
	if member.relayCalls != 1 {
		t.Fatalf("member relay re-registrations = %d, want 1 (healing must survive a dead host)", member.relayCalls)
	}
	// Error strings stay identifier-only: no endpoint/address leak.
	if strings.Contains(err.Error(), "internal:8080") {
		t.Fatalf("reconcile error leaked a daemon address: %v", err)
	}
}
```

import에 `"net"` 추가.

주의: 기존 fake의 `RegisterDistributedFlock`은 `registerDistributedErr`가 nil이면 성공하므로, 이 테스트에서 home hub POST는 성공한다(daemon client는 살아있는 fake). 그래서 relay heal 1회를 단언할 수 있다 — probe 실패(ListVMs dial)와 등록 성공이 공존하는 케이스는 Task 4의 감지 로직 테스트에서 다룬다(감지는 probe dial-fail만으로 counting).

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run TestReconcilePlacements_IsolatesUnreachableHost -v
```
Expected: FAIL — `dead host placement wiped` 또는 `member relay re-registrations = 0` (현재는 조기 반환)

- [ ] **Step 3: 구현**

(a) `internal/anvilmcp/home_failover.go` 신규 생성 — 이 태스크에서는 dial 분류만 담는다:

```go
package anvilmcp

import (
	"errors"
	"net"
)

// isDialError reports whether err is a dial-phase transport failure (the
// connection was never established). Mirrors the daemon-side relay-retry
// classification (cmd/goose-daemon/relay_retry.go): only dial-class failures
// mark a host as down for reconcile probes and home-failover detection —
// HTTP responses, reset/EOF, and ctx cancellation never do.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}
```

(b) `runtime_router.go`의 `ReconcilePlacements` 재구성:

```go
// hostProbe records one reconcile pass's reachability observation for a host.
// reachable is true only when the host's daemon answered ListVMs this pass;
// dialFailed is true when ListVMs failed with a dial-class transport error
// (host down), the only failure class that counts toward home failover.
type hostProbe struct {
	reachable  bool
	dialFailed bool
}

func (r *RuntimeRouter) ReconcilePlacements(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	next := make(map[string]string)
	probes := make(map[string]hostProbe)
	var errs []error
	for hostName, daemon := range r.daemons {
		if daemon == nil {
			continue
		}
		lister, ok := daemon.(interface {
			ListVMs(context.Context) ([]VMInfo, error)
		})
		if !ok {
			continue
		}
		vms, err := lister.ListVMs(ctx)
		if err != nil {
			// Per-host fault isolation: one unreachable daemon must not abort
			// placement reconciliation and wall healing for every other host
			// (failover detection depends on reconcile continuing while the
			// home is down). Carry the failed host's existing placements over
			// unchanged — replacing them from a partial view would orphan its
			// VMs until the host returns.
			probes[hostName] = hostProbe{dialFailed: isDialError(err)}
			errs = append(errs, fmt.Errorf("list vms on runtime host %q failed", hostName))
			r.mu.RLock()
			for vmID, host := range r.placement {
				if host == hostName {
					next[vmID] = host
				}
			}
			r.mu.RUnlock()
			continue
		}
		probes[hostName] = hostProbe{reachable: true}
		for _, vm := range vms {
			if vmID := strings.TrimSpace(vm.VMID); vmID != "" {
				next[vmID] = hostName
			}
		}
	}
	r.mu.Lock()
	r.placement = next
	r.mu.Unlock()
	if r.placementStore != nil {
		if err := r.placementStore.ReplaceVMPlacements(next); err != nil {
			errs = append(errs, err)
		} else if err := r.placementStore.Save(); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, r.reconcileRoutedFlockWalls(ctx, probes))
	return errors.Join(errs...)
}
```

(c) `reconcileRoutedFlockWalls`에서 hub/relay 등록 본문을 helper 2개로 추출 (behavior-preserving — Task 3에서 probes 파라미터는 시그니처만 받고 아직 사용하지 않는다):

```go
// registerRoutedHub re-issues the hub registration for record on its current
// HomeHost: the VMID/Addr-enriched roster from the record's agents plus the
// persisted relay/call tokens. Error strings carry flock/host identifiers only.
func (r *RuntimeRouter) registerRoutedHub(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) error {
	flockID := strings.TrimSpace(record.FlockID)
	homeHost := strings.TrimSpace(record.HomeHost)
	homeDaemon, ok := r.daemons[homeHost]
	if !ok || homeDaemon == nil {
		return fmt.Errorf("reconcile routed flock %q: home host %q has no daemon client", flockID, homeHost)
	}
	roster := make([]RosterMember, 0, len(record.Agents))
	for _, a := range record.Agents {
		roster = append(roster, RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			Host:    strings.TrimSpace(a.Host),
			VMID:    strings.TrimSpace(a.VMID),
			Addr:    r.daemonAddr(strings.TrimSpace(a.Host)),
		})
	}
	return homeDaemon.RegisterDistributedFlock(ctx, flockID, DistributedFlockRequest{Roster: roster, RelayToken: relayToken, CallToken: callToken})
}

// registerRoutedRelays re-issues relay registrations on every member host that
// is not the record's current HomeHost, each carrying that host's own local
// agents so a hopped /call resolves locally. Failures are collected per host
// (identifiers only) so one unreachable member cannot block the rest.
func (r *RuntimeRouter) registerRoutedRelays(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) []error {
	flockID := strings.TrimSpace(record.FlockID)
	homeHost := strings.TrimSpace(record.HomeHost)
	homeAddr := r.daemonAddr(homeHost)
	memberAgents := make(map[string][]RosterMember)
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || host == homeHost {
			continue
		}
		memberAgents[host] = append(memberAgents[host], RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			VMID:    strings.TrimSpace(a.VMID),
		})
	}
	var errs []error
	relayed := map[string]bool{homeHost: true}
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || relayed[host] {
			continue
		}
		relayed[host] = true
		daemon, ok := r.daemons[host]
		if !ok || daemon == nil {
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: member host %q has no daemon client", flockID, host))
			continue
		}
		if err := daemon.RegisterRelayFlock(ctx, flockID, RelayFlockRequest{
			HomeAddr:   homeAddr,
			RelayToken: relayToken,
			CallToken:  callToken,
			Agents:     memberAgents[host],
		}); err != nil {
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: relay re-registration on member host %q failed", flockID, host))
		}
	}
	return errs
}
```

`reconcileRoutedFlockWalls`는 시그니처를 `(ctx context.Context, probes map[string]hostProbe) error`로 바꾸고, per-flock 본문을 다음으로 축약한다 (skip 조건과 token 조회는 기존 그대로):

```go
		if err := r.registerRoutedHub(ctx, record, relayToken, callToken); err != nil {
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: hub re-registration on home host %q failed", flockID, homeHost))
			continue
		}
		errs = append(errs, r.registerRoutedRelays(ctx, record, relayToken, callToken)...)
```

주의: `registerRoutedHub`가 반환하는 원시 에러(DaemonClient wrap)는 dial 분류를 위해 Task 4가 원형 그대로 필요하다 — `reconcileRoutedFlockWalls`에서 wrap해 수집하되 helper 자체는 원형을 반환한다(위 코드가 이미 그렇게 되어 있다).

- [ ] **Step 4: 통과 + 기존 테스트 정합**

```bash
go test ./internal/anvilmcp/ -v -run 'TestReconcile'
go test -race ./internal/anvilmcp/
```
Expected: PASS. 기존 테스트 중 `reconcileRoutedFlockWalls`를 직접 호출하거나 조기 반환 semantics를 단언하는 것이 있으면 새 계약(격리 + joined error)에 맞춰 수정한다 — 수정 시 반드시 해당 테스트의 의도(주석)를 보존.

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/home_failover.go internal/anvilmcp/runtime_router_test.go
git commit -m "fix(anvilmcp): isolate per-host reconcile failures and extract routed re-registration helpers"
```

---

### Task 4: adapter — 감지 카운터 + 결정적 선출 + 전환

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go` (RuntimeRouter 필드 2개, `StartReconcileLoop`, `reconcileRoutedFlockWalls` 감지 훅)
- Modify: `internal/anvilmcp/home_failover.go` (선출·전환)
- Test: `internal/anvilmcp/home_failover_test.go` (신규)

**Interfaces:**
- Consumes: Task 3의 `hostProbe`/`registerRoutedHub`/`registerRoutedRelays`/`isDialError`, 기존 `PlacementStore.SaveRoutedFlockAndPlacements`(token-carrier 없는 재저장은 영속 토큰 보존 — `applyRoutedFlockAndPlacements` 확인 완료), `Daemon.DeleteFlock`
- Produces:
  - `const homeFailureThreshold = 3`
  - `func (r *RuntimeRouter) electNewHome(record RoutedFlockRecord, probes map[string]hostProbe) (string, bool)`
  - `func (r *RuntimeRouter) failoverRoutedFlock(ctx, record, newHome, relayToken, callToken) (switched bool, err error)` — `switched`는 HomeHost 영속(원자 전환점) 성공 여부. caller는 switched일 때만 카운터 리셋.
  - `RuntimeRouter.homeFailures map[string]int` — flock 단위 연속 dial-실패 카운터. **reconcileMu 하에서만 접근**(reconcile 전체가 reconcileMu로 직렬화되므로 추가 락 불요 — 필드 주석에 명시).
  - `RuntimeRouter.reconcileLogf func(format string, args ...any)` + nil-safe `logf` 메서드. `StartReconcileLoop`가 설정. 테스트는 같은 패키지에서 필드 직접 주입.

- [ ] **Step 1: 실패하는 테스트 작성 — 핵심 발화 시나리오**

`internal/anvilmcp/home_failover_test.go` 신규. 공용 헬퍼 + 시나리오 테스트:

```go
package anvilmcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// dialErr fabricates the one transport failure class that counts toward home
// failover detection.
func dialErr() error {
	return &net.OpError{Op: "dial", Err: errors.New("connection refused")}
}

// newFailoverHarness builds a 3-host routed flock: coordinator on hostA (home),
// helper on hostB, researcher on hostC. Election order is agent order, so hostB
// is always the deterministic first candidate.
func newFailoverHarness(t *testing.T) (*RuntimeRouter, *PlacementStore, string, map[string]*routerFakeDaemon) {
	t.Helper()
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	daemons := map[string]*routerFakeDaemon{
		"hostA": {spawnResponses: []*SpawnVMResponse{{VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile"}}},
		"hostB": {spawnResponses: []*SpawnVMResponse{{VMID: "vm-helper-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile"}}},
		"hostC": {spawnResponses: []*SpawnVMResponse{{VMID: "vm-researcher-1", GuestIP: "10.0.3.10", AgentURL: "http://10.0.3.10:8080", TenantID: "tenant-1", EgressPolicy: "profile"}}},
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostC", Endpoint: "http://hostC.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil, nil,
		),
		map[string]Daemon{"hostA": daemons["hostA"], "hostB": daemons["hostB"], "hostC": daemons["hostC"]},
		RuntimeRouterOptions{PlacementStore: store},
	)
	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task: "smoke", Roles: []string{"coordinator", "helper", "researcher"},
		TenantID: "tenant-1", EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep reachable hosts listing their VMs so probes mark them reachable.
	daemons["hostA"].listVMResp = []VMInfo{{VMID: "vm-coordinator-1"}}
	daemons["hostB"].listVMResp = []VMInfo{{VMID: "vm-helper-1"}}
	daemons["hostC"].listVMResp = []VMInfo{{VMID: "vm-researcher-1"}}
	return router, store, out.FlockID, daemons
}

// killHost makes a host dial-dead for both the probe and any direct POST.
func killHost(d *routerFakeDaemon) {
	d.listVMErr = dialErr()
	d.registerDistributedErr = dialErr()
	d.registerRelayErr = dialErr()
	d.deregisterErr = dialErr()
}

func reviveHost(d *routerFakeDaemon) {
	d.listVMErr = nil
	d.registerDistributedErr = nil
	d.registerRelayErr = nil
	d.deregisterErr = nil
}

func reconcileN(t *testing.T, router *RuntimeRouter, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_ = router.ReconcilePlacements(context.Background())
	}
}

// TestFailover_FiresAfterConsecutiveDialFailures is the spec's core contract:
// homeFailureThreshold consecutive dial-class home failures re-elect the first
// reachable member host in agent order, persist the new HomeHost first (atomic
// transition point), promote the new home with the SAME tokens, retarget every
// other member's relay, and attempt a best-effort stale-hub delete on the old
// home.
func TestFailover_FiresAfterConsecutiveDialFailures(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	relayToken, _ := store.RoutedFlockRelayToken(flockID)
	killHost(daemons["hostA"])
	daemons["hostB"].distributedCalls = 0
	daemons["hostC"].relayCalls = 0

	reconcileN(t, router, homeFailureThreshold)

	rec, ok := store.RoutedFlock(flockID)
	if !ok || rec.HomeHost != "hostB" {
		t.Fatalf("HomeHost = %q, want hostB (deterministic first candidate)", rec.HomeHost)
	}
	if daemons["hostB"].distributedCalls == 0 {
		t.Fatal("new home never received the hub (promotion) registration")
	}
	if got := daemons["hostB"].distributedReq.RelayToken; got != relayToken {
		t.Fatalf("failover changed the relay token: %q != %q (guest-transparency broken)", got, relayToken)
	}
	if len(daemons["hostB"].distributedReq.Roster) != 3 {
		t.Fatalf("promoted hub roster = %d, want 3 (membership is unchanged by failover)", len(daemons["hostB"].distributedReq.Roster))
	}
	if daemons["hostC"].relayCalls == 0 || daemons["hostC"].relayReq.HomeAddr != "http://hostB.internal:8080" {
		t.Fatalf("member relay not retargeted to the new home: %+v", daemons["hostC"].relayReq)
	}
	if daemons["hostA"].deregisterCalls == 0 {
		t.Fatal("stale hub delete on the old home was never attempted (best-effort)")
	}
	// Token survives the HomeHost re-save (token-less carrier must preserve it).
	if tok, ok := store.RoutedFlockRelayToken(flockID); !ok || tok != relayToken {
		t.Fatalf("relay token lost across failover persist: %q", tok)
	}
}

// TestFailover_BelowThresholdIsNoop: K-1 consecutive failures must not fire.
func TestFailover_BelowThresholdIsNoop(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	killHost(daemons["hostA"])
	daemons["hostB"].distributedCalls = 0

	reconcileN(t, router, homeFailureThreshold-1)

	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostA" {
		t.Fatalf("HomeHost switched below threshold: %q", rec.HomeHost)
	}
	if daemons["hostB"].distributedCalls != 0 {
		t.Fatal("premature promotion below threshold")
	}
}

// TestFailover_SuccessResetsCounter: an intervening successful home pass resets
// the consecutive counter, so fail,fail,ok,fail,fail never fires.
func TestFailover_SuccessResetsCounter(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	killHost(daemons["hostA"])
	reconcileN(t, router, homeFailureThreshold-1)
	reviveHost(daemons["hostA"])
	reconcileN(t, router, 1) // success resets
	killHost(daemons["hostA"])
	reconcileN(t, router, homeFailureThreshold-1)

	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostA" {
		t.Fatalf("counter did not reset on success: HomeHost = %q", rec.HomeHost)
	}
}

// TestFailover_NonDialErrorsDoNotCount: an HTTP-level registration failure
// means the host is alive — it must not advance the dial-failure counter.
func TestFailover_NonDialErrorsDoNotCount(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	daemons["hostA"].registerDistributedErr = fmt.Errorf("500 internal") // reachable, erroring

	reconcileN(t, router, homeFailureThreshold+2)

	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostA" {
		t.Fatalf("non-dial errors advanced the counter: HomeHost = %q", rec.HomeHost)
	}
}

// TestFailover_NoCandidateIsNoop: with every member host down too (or a
// single-host flock), election finds no candidate and the pass is a no-op —
// re-evaluated next cycle (spec: 현행 502 지속). When a member later revives,
// the already-saturated counter fires immediately on the next pass.
func TestFailover_NoCandidateIsNoop(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	killHost(daemons["hostA"])
	killHost(daemons["hostB"])
	killHost(daemons["hostC"])

	reconcileN(t, router, homeFailureThreshold+1)
	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostA" {
		t.Fatalf("failover fired with zero reachable candidates: %q", rec.HomeHost)
	}

	reviveHost(daemons["hostB"])
	daemons["hostB"].listVMResp = []VMInfo{{VMID: "vm-helper-1"}}
	reconcileN(t, router, 1)
	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostB" {
		t.Fatalf("saturated counter did not fire once a candidate revived: %q", rec.HomeHost)
	}
}

// TestFailover_PartialTransitionConverges: the new home's promotion POST fails
// at transition time, but HomeHost was already persisted (step 1 = the atomic
// transition point) — the next ordinary reconcile pass heals the hub on the
// new home. No second election happens.
func TestFailover_PartialTransitionConverges(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	killHost(daemons["hostA"])
	daemons["hostB"].registerDistributedErr = dialErr() // promotion fails transiently

	reconcileN(t, router, homeFailureThreshold)
	rec, _ := store.RoutedFlock(flockID)
	if rec.HomeHost != "hostB" {
		t.Fatalf("HomeHost not persisted before promotion attempt: %q", rec.HomeHost)
	}

	daemons["hostB"].registerDistributedErr = nil
	daemons["hostB"].distributedCalls = 0
	reconcileN(t, router, 1)
	if daemons["hostB"].distributedCalls == 0 {
		t.Fatal("next pass did not heal the promotion on the persisted new home")
	}
	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostB" {
		t.Fatalf("HomeHost drifted after partial transition: %q", rec.HomeHost)
	}
}

// TestFailover_RevivedOldHomeBecomesRelay: after failover the revived old home
// is healed as a MEMBER (relay registration towards the new home) and never
// re-receives a hub registration (no automatic fail-back).
func TestFailover_RevivedOldHomeBecomesRelay(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	killHost(daemons["hostA"])
	reconcileN(t, router, homeFailureThreshold)
	if rec, _ := store.RoutedFlock(flockID); rec.HomeHost != "hostB" {
		t.Fatal("precondition: failover did not fire")
	}

	reviveHost(daemons["hostA"])
	daemons["hostA"].listVMResp = []VMInfo{{VMID: "vm-coordinator-1"}}
	daemons["hostA"].distributedCalls = 0
	daemons["hostA"].relayCalls = 0
	reconcileN(t, router, 1)

	if daemons["hostA"].distributedCalls != 0 {
		t.Fatal("revived old home re-received a hub registration (fail-back must be manual)")
	}
	if daemons["hostA"].relayCalls == 0 || daemons["hostA"].relayReq.HomeAddr != "http://hostB.internal:8080" {
		t.Fatalf("revived old home not healed as a relay towards the new home: %+v", daemons["hostA"].relayReq)
	}
	_ = flockID
}

// TestFailover_LogsCarryIdentifiersOnly: the failover event log line names
// flock and hosts but never a daemon endpoint or token.
func TestFailover_LogsCarryIdentifiersOnly(t *testing.T) {
	router, store, flockID, daemons := newFailoverHarness(t)
	var lines []string
	router.reconcileLogf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	relayToken, _ := store.RoutedFlockRelayToken(flockID)
	callToken, _ := store.RoutedFlockCallToken(flockID)
	killHost(daemons["hostA"])
	reconcileN(t, router, homeFailureThreshold)

	if len(lines) == 0 {
		t.Fatal("failover produced no operator-visible log line")
	}
	joined := strings.Join(lines, "\n")
	for _, forbidden := range []string{"internal:8080", relayToken, callToken} {
		if forbidden != "" && strings.Contains(joined, forbidden) {
			t.Fatalf("failover log leaked %q: %s", forbidden, joined)
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run TestFailover -v
```
Expected: FAIL (컴파일 에러 — `homeFailureThreshold`/`reconcileLogf` 미정의 — 도 실패로 간주하고 진행)

- [ ] **Step 3: 구현**

(a) `internal/anvilmcp/home_failover.go`에 추가:

```go
// homeFailureThreshold is the number of CONSECUTIVE reconcile passes on which
// a routed flock's home daemon must fail with a dial-class error before
// re-election fires. Deliberately a constant, not configuration (YAGNI — the
// same fixed-policy stance as the daemon's bounded relay retry).
const homeFailureThreshold = 3

// electNewHome returns the deterministic failover target for record: the first
// host in record.Agents order that is not the failed home, has a daemon
// client, and was observed reachable by this reconcile pass. Same input, same
// answer — the elector is the single control plane, so determinism (not
// consensus) is what prevents split-brain. ok=false when no candidate
// survives (single-host flock or all members down): the caller no-ops and the
// saturated counter re-evaluates next pass.
func (r *RuntimeRouter) electNewHome(record RoutedFlockRecord, probes map[string]hostProbe) (string, bool) {
	homeHost := strings.TrimSpace(record.HomeHost)
	seen := map[string]bool{}
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || host == homeHost || seen[host] {
			continue
		}
		seen[host] = true
		if daemon, ok := r.daemons[host]; !ok || daemon == nil {
			continue
		}
		if probes[host].reachable {
			return host, true
		}
	}
	return "", false
}

// failoverRoutedFlock re-points record at newHome and rebuilds the hub/relay
// topology (spec §전환 절차). Ordering is the contract:
//  1. Persist HomeHost FIRST — the atomic transition point. From here every
//     later reconcile pass heals toward the new home even if the steps below
//     all fail right now. A token-less re-save preserves the persisted
//     relay/call tokens (applyRoutedFlockAndPlacements carrier rule).
//  2. Hub registration on the new home (daemon-side relay→hub promotion).
//  3. Relay re-registration on every other member host, INCLUDING the old
//     home (normally still down — heals into a relay on revival, which is
//     also what prevents automatic fail-back).
//  4. Best-effort stale-hub DELETE on the old home. VM-safe by construction:
//     a hub flock's Agents map is always empty (RegisterHub/promotion
//     invariant), so the daemon's deleteFlock destroys no member VMs.
// Tokens are reused unchanged — the guest-injected token never changes, so
// guests ride through the failover untouched. switched reports whether step 1
// committed; the caller resets the failure counter only then. All log/error
// text carries flock/host identifiers only.
func (r *RuntimeRouter) failoverRoutedFlock(ctx context.Context, record RoutedFlockRecord, newHome, relayToken, callToken string) (bool, error) {
	flockID := strings.TrimSpace(record.FlockID)
	oldHome := strings.TrimSpace(record.HomeHost)
	record.HomeHost = newHome
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		return false, fmt.Errorf("failover routed flock %q: persisting new home host %q failed: %w", flockID, newHome, err)
	}
	r.logf("anvil-mcp: routed flock %q home failover %q -> %q (canonical wall restarts empty on the new home)", flockID, oldHome, newHome)
	var errs []error
	if err := r.registerRoutedHub(ctx, record, relayToken, callToken); err != nil {
		errs = append(errs, fmt.Errorf("failover routed flock %q: hub promotion on new home %q failed", flockID, newHome))
	}
	errs = append(errs, r.registerRoutedRelays(ctx, record, relayToken, callToken)...)
	if daemon, ok := r.daemons[oldHome]; ok && daemon != nil {
		if _, err := daemon.DeleteFlock(ctx, flockID); err != nil {
			r.logf("anvil-mcp: routed flock %q: stale hub delete on old home %q failed (best-effort, skipped)", flockID, oldHome)
		}
	}
	return true, errors.Join(errs...)
}
```

`home_failover.go` import에 `"context"`, `"fmt"`, `"strings"`, `"time"` 추가.

(b) `runtime_router.go` — RuntimeRouter 필드와 logf:

```go
	// homeFailures counts CONSECUTIVE dial-class home failures per routed
	// flock id for failover detection. Guarded by reconcileMu: it is only
	// ever touched inside ReconcilePlacements, which reconcileMu serializes.
	homeFailures map[string]int

	// reconcileLogf reports reconcile/failover events (flock/host identifiers
	// only — never tokens or daemon addresses). Set by StartReconcileLoop;
	// nil-safe via logf.
	reconcileLogf func(format string, args ...any)
```

`NewRuntimeRouterWithOptions`의 생성자 리터럴에 `homeFailures: make(map[string]int),` 추가. `StartReconcileLoop`에서 `logf` 기본값 처리 직후 `r.reconcileLogf = logf` 설정(고루틴 시작 전). nil-safe 메서드:

```go
func (r *RuntimeRouter) logf(format string, args ...any) {
	if r.reconcileLogf != nil {
		r.reconcileLogf(format, args...)
	}
}
```

(c) `reconcileRoutedFlockWalls`의 per-flock 본문에 감지 훅 (Task 3의 축약 본문을 대체):

```go
		live[flockID] = true

		// ── home failover detection (2026-07-08 spec) ─────────────────────
		// Only dial-class failures count: the probe's ListVMs dial failure
		// short-circuits (a doomed hub POST adds nothing but latency), and a
		// hub POST that fails with a dial error counts the same way. Any
		// successful hub registration resets the consecutive counter. HTTP
		// errors mean the host is alive: collected as heal errors, counter
		// untouched.
		homeDown := probes[homeHost].dialFailed
		if !homeDown {
			hubErr := r.registerRoutedHub(ctx, record, relayToken, callToken)
			switch {
			case hubErr == nil:
				r.homeFailures[flockID] = 0
			case isDialError(hubErr):
				homeDown = true
			default:
				errs = append(errs, fmt.Errorf("reconcile routed flock %q: hub re-registration on home host %q failed", flockID, homeHost))
				continue
			}
		}
		if homeDown {
			r.homeFailures[flockID]++
			if r.homeFailures[flockID] >= homeFailureThreshold && record.Status == RoutedFlockStatusReady {
				if newHome, ok := r.electNewHome(record, probes); ok {
					switched, err := r.failoverRoutedFlock(ctx, record, newHome, relayToken, callToken)
					if switched {
						r.homeFailures[flockID] = 0
					}
					if err != nil {
						errs = append(errs, err)
					}
					continue
				}
				// No reachable candidate: no-op, counter stays saturated so the
				// next pass with a revived member fires immediately (spec).
			}
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: home host %q unreachable", flockID, homeHost))
			continue
		}
		errs = append(errs, r.registerRoutedRelays(ctx, record, relayToken, callToken)...)
```

함수 도입부에 `live := make(map[string]bool)`를 두고(위 코드가 채움 — skip 조건 통과 전, `homeHost != ""`이고 deleted/deleting이 아닌 record마다), 루프 종료 후 카운터 sweep:

```go
	// Sweep counters for flocks that no longer exist (deleted/removed) so the
	// map cannot grow unboundedly across flock lifecycles.
	for id := range r.homeFailures {
		if !live[id] {
			delete(r.homeFailures, id)
		}
	}
```

주의 — 기존 skip 조건과의 상호작용: relay token이 영속되지 않은 record(ready 전 실패)는 기존대로 skip이며 감지에도 참여하지 않는다(토큰 없이 전환 불가). `record.Status == RoutedFlockStatusReady` 게이트는 **failover 발화에만** 적용 — creating/failed_cleanup_pending record의 heal 동작은 기존 그대로다.

- [ ] **Step 4: 통과 확인**

```bash
go test ./internal/anvilmcp/ -run TestFailover -v
go test -race ./internal/anvilmcp/
```
Expected: 전부 PASS (기존 reconcile/routed flock 테스트 포함 회귀 없음)

- [ ] **Step 5: 전체 Go 테스트**

```bash
go build ./... && go test -race ./internal/... ./cmd/...
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/anvilmcp/home_failover.go internal/anvilmcp/home_failover_test.go internal/anvilmcp/runtime_router.go
git commit -m "feat(anvilmcp): re-elect routed flock home after consecutive dial failures"
```

---

### Task 5: KVM e2e — `scripts/anvil-cross-host-failover-e2e.sh`

**Files:**
- Create: `scripts/anvil-cross-host-failover-e2e.sh`

**Interfaces:**
- Consumes: Task 1-4 전부 (real daemon 승격 + adapter 전환), 기존 스크립트 패턴 — stub/VM/gtwall 패턴은 `scripts/anvil-cross-host-wall-e2e.sh`, adapter members_only 구성은 `docs/operations/2026-07-08-cross-host-manual-verification.md` §2와 `cmd/anvil-mcp` config (`cross_host_flock_create_mode: members_only`, `scheduler_state_path`, `reconcile_interval`).
- Produces: 단독 실행 KVM e2e 게이트 (root + KVM 필요, 기존 cross-host e2e 2종과 동일하게 메인 `e2e_test.sh`와 별도).

**토폴로지** (단일 물리 host — 기존 wall e2e와 동일한 이유로 real daemon은 1개만):

- real anvil-daemon (member, auth-on, real KVM VM 1개 — researcher)
- stub A (초기 home, python3 recorder, `127.0.0.1:3100`) — coordinator "배치"
- stub B (선출 후보 1순위, `127.0.0.1:3101`) — helper "배치"
- adapter: `cmd/anvil-mcp`를 MCP stdio로 구동 (`scripts/anvil-mcp-e2e.sh`의 JSON-RPC 구동 패턴), `reconcile_interval: 2s`
- stub은 wall e2e의 recorder를 확장: `GET /vms` → `[]` 또는 자기 VM 목록(JSON), `POST /vms` → 고정 VMID/guest_ip JSON, `POST /flocks/{id}/distributed|/relay|/post` → 기록 후 200/201, `DELETE /flocks/{id}` → 기록 후 200. hosts 배치는 검증 runbook처럼 host 용량(AvailableVMs 1씩)으로 고정 — roles 순서 [coordinator@stubA, helper@stubB, researcher@member]가 곧 선출 순서다.

- [ ] **Step 1: 스크립트 골격 작성** — wall e2e의 헤더 주석 스타일(무엇을 증명하고 무엇을 단일-host 범위에서 제외하는지 명시), `set -euo pipefail`, `step()/ok()/fail()` 헬퍼, artifact 디렉토리, cleanup trap(스텁 kill + flock/VM teardown + adapter 종료) 재사용.

- [ ] **Step 2: Phase 0 — 셋업 단언.** adapter로 `anvil_spawn_flock` (roles 3개) → placements.json에서 `home_host == stubA-host` 확인, stub A capture에 `POST /flocks/{id}/distributed` 존재, real daemon에 relay flock 등록(`GET /flocks/{id}` 200), guest gtwall 1회 성공(post가 stub A capture에 도달 — bearer == relay token).

- [ ] **Step 3: Phase 1 — stub 재선출.** stub A kill → placements.json의 `home_host`가 stub B host로 바뀔 때까지 poll (timeout 60s; `reconcile_interval 2s × threshold 3`이면 ~10s 내). 단언:
  - stub B capture에 `POST /flocks/{id}/distributed` (승격 hub 등록) — roster 3명 + **동일 relay token**
  - real member daemon이 relay 재등록을 받아 guest gtwall 재시도 성공, post가 **stub B** capture에 도달 (relay 갱신 wire 증거)
  - adapter stderr에 failover 로그 라인 존재하되 `127.0.0.1`(stub 주소)·token 문자열 부재 (redaction spot check)

- [ ] **Step 4: Phase 2 — real daemon 승격.** stub B kill → poll로 `home_host == member-host` 확인. 단언:
  - real daemon의 flock kind가 hub로 승격: `GET /flocks/{id}/wall/history` (CP bearer)가 200이고 **빈 배열에서 시작** (phase 0/1 메시지 부재 — wall 손실 계약의 관측 증거)
  - guest gtwall 성공 → history에 phase 2 메시지만 존재
  - daemon stderr에 token/stub 주소 무출력 (기존 e2e의 redaction grep 패턴)

- [ ] **Step 5: Phase 3 — 구 home 부활 강등 (stub로는 kind 상태를 가질 수 없으므로 real daemon 기준 역방향은 수동 검증 §6에 위임).** 스크립트에서는 stub B를 재기동해 relay 재등록 POST가 도달하는지(capture)만 확인하고, "hub→relay 강등의 실 daemon 검증은 manual verification §6" 주석을 남긴다.

- [ ] **Step 6: 실행 검증**

```bash
sudo bash scripts/anvil-cross-host-failover-e2e.sh; echo "exit=$?"
```
Expected: `exit=0` + 마지막 라인 `All failover e2e steps passed ✓`. **판정은 exit code와 "passed" 라인만으로** — tail의 cleanup 출력은 실패 시에도 동일하게 나온다. 실행 워크트리에는 gitignored `configs/goose.yaml`·`goose-secrets.yaml`을 메인 checkout에서 복사해야 VM 생성이 500으로 조용히 실패하지 않는다. sudo 실행 후 root 소유 잔재(vms/, artifacts/)는 sudo rm으로 정리.

- [ ] **Step 7: Commit**

```bash
git add scripts/anvil-cross-host-failover-e2e.sh
git commit -m "test(e2e): cross-host home failover KVM e2e (stub election + real daemon promotion)"
```

---

### Task 6: 문서 — ADR row, 경계 문서, runbook, 수동 검증 §6, handoff

**Files:**
- Modify: `docs/ADR_INDEX.md` §3 표, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `CONTEXT.md`, `docs/operations/runbook.md`, `docs/operations/2026-07-08-cross-host-manual-verification.md`, `docs/operations/2026-07-08-routed-flock-stack-handoff.md`
- Create: `docs/operations/2026-07-11-home-failover-handoff.md`

**Interfaces:**
- Consumes: Task 1-5 완료 상태 (구현 사실 기술)
- Produces: release 단계 zone 인벤토리 동기화의 입력 (handoff의 zone 연동 절)

- [ ] **Step 1: ADR_INDEX §3에 row 추가.** 관례는 기존 cross-host 결정 row와 동일 — design spec을 결정 원문으로 삼고 별도 `docs/adr/*.md` 없음. 내용에 반드시 포함: 재선출 failover(복제 없음), `homeFailureThreshold=3` 상수·dial-계열 한정, 결정적 선출(agent 순서 첫 생존 host, 구 home 제외), **wall 손실 명시 계약**(새 home 빈 log에서 seq 재시작, 이전 기록은 구 home 디스크 잔존·병합 없음), **토큰 불변**(guest 무중단), 자동 fail-back 없음, **kind 전환 결정**(spec 보정: relay→hub 승격/hub→relay 강등은 CP bearer 뒤 등록 endpoint에서만, local flock 409 보호 불변, hub Agents-empty 불변식과 deleteFlock VM-safety 논거), 상태 링크(`superpowers/specs/2026-07-08-home-failover-design.md`, handoff).

- [ ] **Step 2: PUBLIC_RELEASE_BOUNDARY.md** — Cross-host Town Wall/gtcall row의 "home SPOF는 1차 수용" 서술을 "재선출 failover로 해소(wall 과거 기록 손실 수용 계약)"로 갱신하고 전환 창(최대 ~`threshold×reconcile_interval` + 전환 시간, 기본 ~3분) 동안 502 + bounded retry가 기존 동작임을 명시.

- [ ] **Step 3: CONTEXT.md** — "최근 후속 완료 상태"에 failover 항목 추가 (kind 전환·감지/선출/전환·e2e 요약, wall 손실 계약 명시), "남은 후속 후보"에서 "home SPOF 제거 — 재선출 failover 설계 확정(...구현은 수동 multi-host 검증 통과 후)" 항목을 구현 완료로 제거/치환. "비동기 relay buffer(failover 이후 재평가)"는 이제 재평가 가능 상태로 문구 갱신.

- [ ] **Step 4: runbook.md** — 운영 절차 추가: (a) failover 관측법(adapter 로그 라인, placements.json `home_host`), (b) 전환 창 계산(threshold 3 × `ANVIL_MCP_RECONCILE_INTERVAL` 기본 60s → 최대 ~3분 + 전환 시간), (c) **수동 fail-back 절차**: 자동 fail-back 없음 — adapter 중지 → `scheduler_state` placements.json의 해당 flock `home_host`를 구 home으로 수정 → adapter 재기동 → reconcile이 hub 승격/강등을 자동 수행(전환과 동일 배관), wall은 다시 빈 log에서 시작됨을 경고. (d) wall 손실 계약을 flock 운영 관점에서 명시(agent에게 과거 메시지가 사라진 것으로 보임).

- [ ] **Step 5: 수동 검증 runbook §6 확장** (`2026-07-08-cross-host-manual-verification.md`) — failover 시나리오 추가: home daemon 정지 → 전환 창 대기 → placements.json 전환 확인 → wall 양방향·gtcall 재확인(새 home 경유) → **구 home 재기동 → relay 강등 확인(gtwall이 새 home으로 forward, 409 없음)** → wall history에 전환 전 메시지 부재 확인. 판정 기준 표에 행 추가.

- [ ] **Step 6: handoff 신규 작성** (`2026-07-11-home-failover-handoff.md`) — 기존 slice handoff 형식(무엇이 main에 있나 / 검증 증거 / Follow-Up). Follow-Up에 반드시: 실 2-daemon 수동 검증 §6 failover 시나리오 수행(트리거: 다음 서버 세션), zone `docs/FOLLOWUP.md` P3-09 갱신, stack handoff(`2026-07-08-routed-flock-stack-handoff.md`)의 Next Action 3번을 완료로 갱신.

- [ ] **Step 7: 문서 상호 링크 검증 + Commit**

```bash
grep -rn "home-failover" docs/ CONTEXT.md | grep -v Binary  # 링크 오타 눈으로 확인
git add docs/ CONTEXT.md
git commit -m "docs: home failover — ADR row, boundary/runbook updates, manual-verification §6, handoff"
```

---

## 최종 검증 (전체 슬라이스)

- [ ] `go build ./... && go vet ./... && gofmt -l . | grep -v '^web/' ; go test -race ./internal/... ./cmd/...` — 전부 clean/PASS
- [ ] `sudo bash scripts/anvil-cross-host-failover-e2e.sh` — exit 0 + "passed ✓"
- [ ] 기존 cross-host e2e 회귀: `sudo bash scripts/anvil-cross-host-wall-e2e.sh`와 `sudo bash scripts/anvil-cross-host-gtcall-e2e.sh` — exit 0 (등록 핸들러를 만졌으므로 필수)
- [ ] 전체 KVM 게이트 `sudo bash e2e_test.sh` — exit code 판정 (단일 host flock 경로 회귀 확인)
- [ ] secret-scan: `bash scripts/secret-scan.sh` (있는 그대로의 사용법 확인 후) — 신규 코드/로그에 토큰 유출 없음
- [ ] PR 생성 (`feature/home-failover` → main). **자체 머지 금지** — 머지는 사용자 승인으로만.

## Self-Review 기록 (플랜 작성 시점)

- Spec coverage: 감지(§감지와 선출)→Task 4, 선출→Task 4, 전환 4단계(§전환 절차)→Task 4 (+kind 전환 전제 Task 1·2), wall 손실 semantics→Task 1(승격 빈 wall)+Task 5 Phase 2 단언+Task 6 문서, 경계 사례 4종→Task 4 테스트(후보 0/fail-back 없음/부분 실패 수렴/구 home 부활), 유닛 테스트 요구(§테스트)→Task 4의 8개 테스트가 spec 열거 항목 전부 대응, KVM e2e→Task 5, 수동 검증 §6 확장→Task 6, 문서 반영(§문서 반영)→Task 6. 비목표 침범 없음.
- Spec 보정 2건(daemon kind 전환 필요, reconcile per-host 격리 필요)은 상단 "Spec 보정 사항"에 근거와 함께 명시 — 리뷰어(사용자)가 기각 가능하도록 분리 기술.
- Type consistency: `hostProbe`/`registerRoutedHub`/`registerRoutedRelays`/`electNewHome`/`failoverRoutedFlock` 시그니처가 Task 3↔4↔테스트 코드에서 동일함을 재확인. fake 필드명(`listVMErr`, `registerDistributedErr`, `registerRelayErr`, `deregisterErr`, `deregisterCalls`, `distributedReq`, `relayReq`)은 기존 `runtime_router_test.go`/`tools_test.go` 실물과 대조 완료.
- 알려진 리스크: `f.Snapshot()`/`RosterSnapshot()` accessor 이름(Task 1 Step 1 주의 참조), 기존 reconcile 테스트의 시그니처 정합(Task 3 Step 4). 워커가 컴파일 에러로 즉시 발견 가능한 부류다.
