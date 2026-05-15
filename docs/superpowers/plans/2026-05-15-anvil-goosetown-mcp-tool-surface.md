# anvil Goosetown MCP Tool Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Goosetown flock/Town Wall 기능을 기존 VM/snapshot MCP 계약을 깨지 않는 additive `anvil_*` MCP tool surface로 노출한다.

**Architecture:** `cmd/anvil-mcp`와 `internal/anvilmcp`는 계속 얇은 stdio MCP adapter로 남기고, 새 tool은 ephemera daemon의 `/flocks`와 `/flocks/{id}/wall/history`, `/flocks/{id}/post` API에만 매핑한다. `session_name -> vm_id` 모델은 flock에 재사용하지 않고, flock은 명시적 `flock_id`로만 다룬다. tenant/egress/audit 정합성을 위해 daemon flock spawn에도 `tenant_id`와 `egress_policy`를 전달한다.

**Tech Stack:** Go 1.25+, `github.com/modelcontextprotocol/go-sdk/mcp`, standard `net/http`, `net/http/httptest`, ephemera daemon Goosetown API, existing `internal/anvilmcp` audit/schema helpers.

---

## 기준 상태

- 최신 구현 기준은 `origin/main`이다.
- 현재 로컬 checkout이 `origin/main`보다 뒤처져 있으면 먼저 최신 브랜치에서 feature branch를 만든다.
- 기존 dirty file은 건드리지 않는다. 현재 확인된 unrelated 변경은 `.gitignore`, `cmd/e2e-replay-server/`다.

```bash
git fetch origin main
git switch -c feature/goosetown-mcp origin/main
```

## 범위

포함:

- daemon flock create request에 optional `tenant_id`, `egress_policy` 추가
- MCP daemon client의 flock/Town Wall HTTP method 추가
- MCP tool handler 6개 추가
- IronClaw/Gemini schema compatibility 목록 갱신
- MCP smoke script에 flock mode 추가
- README, `docs/architecture/mcp-architecture.md`, `RELEASE_NOTES.md`, `CONTEXT.md` 현행화

제외:

- Town Wall SSE stream을 MCP tool로 노출
- flock snapshot/restore
- `session_name`을 flock alias로 재사용
- scheduler-aware cross-host flock placement
- HTTP MCP transport

## 파일 구조

수정:

- `internal/orchestrator/flock.go`: `Flock`에 tenant/egress metadata 추가
- `internal/orchestrator/flock_test.go`: flock metadata 보존 테스트 추가
- `cmd/goose-daemon/orchestrator_api.go`: `POST /flocks` tenant/egress validation과 VM spawn 전달
- `cmd/goose-daemon/api_test.go`: invalid flock tenant/egress가 spawn 전에 거부되는 테스트 추가
- `internal/anvilmcp/daemon_client.go`: flock/Town Wall DTO와 HTTP client method 추가
- `internal/anvilmcp/daemon_client_test.go`: daemon client request/response 테스트 추가
- `internal/anvilmcp/tools.go`: MCP input/output struct와 tool handler 추가
- `internal/anvilmcp/tools_test.go`: fake daemon 확장과 tool validation/audit 테스트 추가
- `internal/anvilmcp/ironclaw_schema.go`: 새 tool input schema 등록
- `internal/anvilmcp/ironclaw_schema_test.go`: 새 tool schema 존재 검증 추가
- `cmd/anvil-mcp/main.go`: 새 MCP tool 등록
- `cmd/anvil-mcp/main_test.go`: tool inventory 테스트 갱신
- `scripts/anvil-mcp-smoke.go`: `-mode flock` smoke flow 추가
- `scripts/anvil-mcp-e2e.sh`: `flock` wrapper mode 추가
- `README.md`: MCP tool 목록과 Goosetown 사용법 갱신
- `docs/architecture/mcp-architecture.md`: MCP Goosetown 계약 문서화
- `RELEASE_NOTES.md`: Unreleased 항목 추가
- `CONTEXT.md`: 완료 상태와 남은 후속 후보 갱신

수정 금지:

- 기존 `anvil_spawn_vm`, `anvil_run_task`, snapshot tool 이름과 입력 의미
- `agent_token` 노출 정책
- scheduler service의 `/schedule/spawn`, `/schedule/restore` 계약

## Tool 계약

추가할 MCP tools:

| MCP tool | Daemon call | 목적 |
|---|---|---|
| `anvil_spawn_flock` | `POST /flocks` | 역할 목록으로 Goosetown flock 생성 |
| `anvil_list_flocks` | `GET /flocks` | live flock 목록 조회 |
| `anvil_get_flock` | `GET /flocks/{flock_id}` | flock metadata와 agent 상태 조회 |
| `anvil_delete_flock` | `DELETE /flocks/{flock_id}` | daemon이 소유한 flock cleanup 실행 |
| `anvil_post_townwall` | `POST /flocks/{flock_id}/post` | Town Wall message append |
| `anvil_get_townwall_history` | `GET /flocks/{flock_id}/wall/history` | Town Wall history 조회 |

입력 예시:

```json
{
  "task": "release readiness review",
  "roles": ["orchestrator", "researcher", "worker", "reviewer"],
  "tenant_id": "tenant.alpha",
  "egress_policy": "profile"
}
```

## Task 1: 최신 기준 브랜치 준비

**Files:**

- Modify: none

- [x] **Step 1: 작업트리 상태를 확인한다**

```bash
git status --short --branch
```

Expected: unrelated local changes가 있으면 목록을 기록하고 건드리지 않는다.

- [x] **Step 2: 최신 `origin/main`에서 feature branch를 만든다**

```bash
git fetch origin main
git switch -c feature/goosetown-mcp origin/main
```

Expected: branch가 `origin/main` 기준으로 만들어진다.

- [x] **Step 3: Goosetown 기준 파일이 있는지 확인한다**

```bash
test -f cmd/goose-daemon/orchestrator_api.go
test -f internal/orchestrator/flock.go
go test ./internal/orchestrator
```

Expected: all commands pass.

## Task 2: daemon flock tenant/egress 계약 추가

**Files:**

- Modify: `internal/orchestrator/flock.go`
- Modify: `internal/orchestrator/flock_test.go`
- Modify: `cmd/goose-daemon/orchestrator_api.go`
- Modify: `cmd/goose-daemon/api_test.go`

- [x] **Step 1: failing orchestrator test를 추가한다**

`internal/orchestrator/flock_test.go`에 다음 테스트를 추가한다.

```go
func TestFlockManagerCreateStoresTenantAndEgress(t *testing.T) {
	manager := NewFlockManager(t.TempDir())
	flock, err := manager.Create("flock-1", "ship review", "tenant-1", "profile", filepath.Join(t.TempDir(), "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if flock.TenantID != "tenant-1" || flock.EgressPolicy != "profile" {
		t.Fatalf("flock tenant/egress = %q/%q, want tenant-1/profile", flock.TenantID, flock.EgressPolicy)
	}
}
```

- [x] **Step 2: test failure를 확인한다**

```bash
go test ./internal/orchestrator -run TestFlockManagerCreateStoresTenantAndEgress -count=1
```

Expected: `Create` signature mismatch로 fail.

- [x] **Step 3: `Flock` metadata와 `Create` signature를 수정한다**

`internal/orchestrator/flock.go`의 `Flock` struct와 `Create` method를 다음 형태로 바꾼다.

```go
type Flock struct {
	mu           sync.RWMutex
	ID           string                `json:"flock_id"`
	Task         string                `json:"task"`
	TenantID     string                `json:"tenant_id,omitempty"`
	EgressPolicy string                `json:"egress_policy,omitempty"`
	Agents       map[string]*AgentInfo `json:"agents"`
	TownWall     *TownWall             `json:"-"`
	CreatedAt    time.Time             `json:"created_at"`
}
```

`MarshalJSON` alias에도 같은 두 field를 포함한다.

```go
type flockJSON struct {
	ID           string                `json:"flock_id"`
	Task         string                `json:"task"`
	TenantID     string                `json:"tenant_id,omitempty"`
	EgressPolicy string                `json:"egress_policy,omitempty"`
	Agents       map[string]*AgentInfo `json:"agents"`
	CreatedAt    time.Time             `json:"created_at"`
}
```

`Create` signature와 할당은 다음처럼 바꾼다.

```go
func (fm *FlockManager) Create(flockID, task, tenantID, egressPolicy, townWallPath string) (*Flock, error) {
	tw, err := NewTownWall(flockID, townWallPath)
	if err != nil {
		return nil, err
	}
	f := &Flock{
		ID:           flockID,
		Task:         task,
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		Agents:       make(map[string]*AgentInfo),
		TownWall:     tw,
		CreatedAt:    time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	return f, nil
}
```

- [x] **Step 4: daemon request/response와 spawn forwarding을 수정한다**

`cmd/goose-daemon/orchestrator_api.go`의 request/response를 확장한다.

```go
type FlockCreateRequest struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
}
```

```go
type FlockCreateResponse struct {
	FlockID      string                    `json:"flock_id"`
	Task         string                    `json:"task"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	EgressPolicy string                    `json:"egress_policy,omitempty"`
	Agents       []*orchestrator.AgentInfo `json:"agents"`
	TownWallURL  string                    `json:"townwall_url"`
	PostURL      string                    `json:"post_url"`
}
```

`createFlock`에서 role validation 뒤에 다음 validation을 추가한다.

```go
var err error
req.TenantID, err = normalizeDaemonTenantID(req.TenantID)
if err != nil {
	writeJSONError(w, http.StatusBadRequest, err)
	return
}
req.EgressPolicy, err = normalizeDaemonEgressPolicy(req.EgressPolicy)
if err != nil {
	writeJSONError(w, http.StatusBadRequest, err)
	return
}
```

flock 생성과 spawn 호출은 다음 형태로 바꾼다.

```go
flock, err := cp.flockMgr.Create(flockID, req.Task, req.TenantID, req.EgressPolicy, townWallPath)
```

```go
vmInfo, _, err := cp.spawnVMForFlock(flockID, agentID, role, req.TenantID, req.EgressPolicy)
```

`spawnVMForFlock` signature와 options를 다음처럼 바꾼다.

```go
func (cp *ControlPlane) spawnVMForFlock(flockID, agentID, role, tenantID, egressPolicy string) (*VMInfo, string, error) {
	configPath, secretsPath, err := cp.profileConfigPaths(role)
	if err != nil {
		return nil, "", err
	}
	agentProfile := LookupProfile(role)
	return cp.spawnVMInternal(spawnVMOptions{
		Profile:      role,
		ConfigPath:   configPath,
		SecretsPath:  secretsPath,
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		SystemPrompt: cp.loadProfileSystemPrompt(agentProfile.ProfileDir),
		FlockID:      flockID,
		AgentID:      agentID,
		VcpuCount:    agentProfile.VcpuCount,
		MemSizeMib:   agentProfile.MemSizeMib,
	})
}
```

response construction에는 `TenantID`와 `EgressPolicy`를 포함한다.

- [x] **Step 5: invalid tenant/egress daemon tests를 추가한다**

`cmd/goose-daemon/api_test.go`에 다음 테스트를 추가한다.

```go
func TestCreateFlockRejectsInvalidTenantBeforeSpawn(t *testing.T) {
	cp := newTestCP(t)
	cp.flockMgr = orchestrator.NewFlockManager(cp.workDir)
	req := httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(`{"task":"x","roles":["worker"],"tenant_id":"../bad"}`))
	rr := httptest.NewRecorder()

	cp.createFlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /flocks status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	if len(cp.flockMgr.List()) != 0 {
		t.Fatalf("flocks = %+v, want none after invalid tenant", cp.flockMgr.List())
	}
}

func TestCreateFlockRejectsInvalidEgressBeforeSpawn(t *testing.T) {
	cp := newTestCP(t)
	cp.flockMgr = orchestrator.NewFlockManager(cp.workDir)
	req := httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(`{"task":"x","roles":["worker"],"egress_policy":"internet"}`))
	rr := httptest.NewRecorder()

	cp.createFlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /flocks status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	if len(cp.flockMgr.List()) != 0 {
		t.Fatalf("flocks = %+v, want none after invalid egress", cp.flockMgr.List())
	}
}
```

`api_test.go` import에 `ephemera/internal/orchestrator`가 없으면 추가한다.

- [x] **Step 6: daemon/orchestrator tests를 실행한다**

```bash
go test ./internal/orchestrator ./cmd/goose-daemon -run 'TestFlock|TestCreateFlock' -count=1
```

Expected: pass.

- [x] **Step 7: commit한다**

```bash
git add internal/orchestrator/flock.go internal/orchestrator/flock_test.go cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/api_test.go
git commit -m "feat: carry tenant policy through Goosetown flocks"
```

## Task 3: daemon client flock/Town Wall methods

**Files:**

- Modify: `internal/anvilmcp/daemon_client.go`
- Modify: `internal/anvilmcp/daemon_client_test.go`

- [x] **Step 1: failing daemon client test를 추가한다**

`internal/anvilmcp/daemon_client_test.go`에 다음 테스트를 추가한다.

```go
func TestDaemonClientFlockEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /flocks":
			var body FlockCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if body.Task != "ship review" || len(body.Roles) != 2 || body.Roles[0] != "researcher" || body.Roles[1] != "worker" {
				t.Fatalf("create body = %+v, want task and two roles", body)
			}
			if body.TenantID != "tenant-1" || body.EgressPolicy != "profile" {
				t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", body.TenantID, body.EgressPolicy)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"flock_id":"flock-1","task":"ship review","tenant_id":"tenant-1","egress_policy":"profile","agents":[{"agent_id":"worker-1","role":"worker","vm_id":"vm-1","agent_url":"http://10.0.1.2:8080","status":"ready"}],"townwall_url":"http://127.0.0.1:3000/flocks/flock-1/wall","post_url":"http://127.0.0.1:3000/flocks/flock-1/post"}`))
		case "GET /flocks":
			_, _ = w.Write([]byte(`[{"flock_id":"flock-1","task":"ship review","tenant_id":"tenant-1","egress_policy":"profile","agents":{"worker-1":{"agent_id":"worker-1","role":"worker","vm_id":"vm-1","agent_url":"http://10.0.1.2:8080","status":"ready"}},"created_at":"2026-05-15T00:00:00Z"}]`))
		case "GET /flocks/flock-1":
			_, _ = w.Write([]byte(`{"flock_id":"flock-1","task":"ship review","tenant_id":"tenant-1","egress_policy":"profile","agents":{"worker-1":{"agent_id":"worker-1","role":"worker","vm_id":"vm-1","agent_url":"http://10.0.1.2:8080","status":"ready"}},"created_at":"2026-05-15T00:00:00Z"}`))
		case "POST /flocks/flock-1/post":
			var body TownWallPostRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode post body: %v", err)
			}
			if body.AgentID != "orchestrator" || body.Body != "hello" {
				t.Fatalf("townwall post = %+v, want orchestrator hello", body)
			}
			_, _ = w.Write([]byte(`{"timestamp":"2026-05-15T00:00:01Z","agent_id":"orchestrator","body":"hello"}`))
		case "GET /flocks/flock-1/wall/history":
			_, _ = w.Write([]byte(`[{"timestamp":"2026-05-15T00:00:01Z","agent_id":"orchestrator","body":"hello"}]`))
		case "DELETE /flocks/flock-1":
			_, _ = w.Write([]byte(`{"status":"deleted","flock_id":"flock-1"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "test-token"}, server.Client())
	ctx := context.Background()

	created, err := client.CreateFlock(ctx, FlockCreateRequest{
		Task:         "ship review",
		Roles:        []string{"researcher", "worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if created.FlockID != "flock-1" || len(created.Agents) != 1 {
		t.Fatalf("created flock = %+v, want flock-1 with one agent", created)
	}
	if _, err := client.ListFlocks(ctx); err != nil {
		t.Fatalf("ListFlocks returned error: %v", err)
	}
	if _, err := client.GetFlock(ctx, "flock-1"); err != nil {
		t.Fatalf("GetFlock returned error: %v", err)
	}
	if _, err := client.PostTownWall(ctx, "flock-1", TownWallPostRequest{AgentID: "orchestrator", Body: "hello"}); err != nil {
		t.Fatalf("PostTownWall returned error: %v", err)
	}
	if _, err := client.TownWallHistory(ctx, "flock-1"); err != nil {
		t.Fatalf("TownWallHistory returned error: %v", err)
	}
	if _, err := client.DeleteFlock(ctx, "flock-1"); err != nil {
		t.Fatalf("DeleteFlock returned error: %v", err)
	}

	want := []string{
		"POST /flocks",
		"GET /flocks",
		"GET /flocks/flock-1",
		"POST /flocks/flock-1/post",
		"GET /flocks/flock-1/wall/history",
		"DELETE /flocks/flock-1",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}
```

- [x] **Step 2: test failure를 확인한다**

```bash
go test ./internal/anvilmcp -run TestDaemonClientFlockEndpoints -count=1
```

Expected: missing types/methods로 fail.

- [x] **Step 3: DTO와 client methods를 추가한다**

`internal/anvilmcp/daemon_client.go`에 `SnapshotInfo` 근처로 다음 type을 추가한다.

```go
type FlockAgentInfo struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

type FlockCreateRequest struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
}

type FlockCreateResponse struct {
	FlockID      string           `json:"flock_id"`
	Task         string           `json:"task"`
	TenantID     string           `json:"tenant_id,omitempty"`
	EgressPolicy string           `json:"egress_policy,omitempty"`
	Agents       []FlockAgentInfo `json:"agents"`
	TownWallURL  string           `json:"townwall_url"`
	PostURL      string           `json:"post_url"`
}

type FlockInfo struct {
	FlockID      string                    `json:"flock_id"`
	Task         string                    `json:"task"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	EgressPolicy string                    `json:"egress_policy,omitempty"`
	Agents       map[string]FlockAgentInfo `json:"agents"`
	CreatedAt    time.Time                 `json:"created_at"`
}

type TownWallPostRequest struct {
	AgentID string `json:"agent_id"`
	Body    string `json:"body"`
}

type TownWallMessage struct {
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agent_id"`
	Body      string `json:"body"`
}
```

`net/url`은 이미 import되어 있으므로 flock ID escaping에 사용한다. 다음 methods를 추가한다.

```go
func (c *DaemonClient) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/flocks", req)
	if err != nil {
		return nil, err
	}
	var resp FlockCreateResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode create flock response: %w", err)
	}
	if resp.Agents == nil {
		resp.Agents = []FlockAgentInfo{}
	}
	return &resp, nil
}

func (c *DaemonClient) ListFlocks(ctx context.Context) ([]FlockInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/flocks", nil)
	if err != nil {
		return nil, err
	}
	var resp []FlockInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode list flocks response: %w", err)
	}
	if resp == nil {
		resp = []FlockInfo{}
	}
	return resp, nil
}

func (c *DaemonClient) GetFlock(ctx context.Context, flockID string) (*FlockInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/flocks/"+url.PathEscape(flockID), nil)
	if err != nil {
		return nil, err
	}
	var resp FlockInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode flock response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodDelete, "/flocks/"+url.PathEscape(flockID), nil)
}

func (c *DaemonClient) PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/flocks/"+url.PathEscape(flockID)+"/post", req)
	if err != nil {
		return nil, err
	}
	var resp TownWallMessage
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode town wall post response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/flocks/"+url.PathEscape(flockID)+"/wall/history", nil)
	if err != nil {
		return nil, err
	}
	var resp []TownWallMessage
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode town wall history response: %w", err)
	}
	if resp == nil {
		resp = []TownWallMessage{}
	}
	return resp, nil
}
```

- [x] **Step 4: daemon client tests를 실행한다**

```bash
go test ./internal/anvilmcp -run TestDaemonClientFlockEndpoints -count=1
```

Expected: pass.

- [x] **Step 5: commit한다**

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go
git commit -m "feat: add Goosetown daemon client methods"
```

## Task 4: MCP tool handlers 추가

**Files:**

- Modify: `internal/anvilmcp/tools.go`
- Modify: `internal/anvilmcp/tools_test.go`

- [x] **Step 1: `Daemon` interface와 `fakeDaemon` compile failure test를 만든다**

`internal/anvilmcp/tools.go`의 `Daemon` interface에 다음 method를 추가한다.

```go
	CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error)
	ListFlocks(ctx context.Context) ([]FlockInfo, error)
	GetFlock(ctx context.Context, flockID string) (*FlockInfo, error)
	DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error)
	PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error)
	TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error)
```

이 상태에서 test를 실행해 `fakeDaemon`이 새 interface를 만족하지 못하는지 확인한다.

```bash
go test ./internal/anvilmcp -run TestToolsSpawnFlock -count=1
```

Expected: `*fakeDaemon does not implement Daemon` compile failure.

- [x] **Step 2: input/output struct를 추가한다**

`internal/anvilmcp/tools.go`의 input struct 영역에 다음을 추가한다.

```go
type SpawnFlockInput struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
}

type SpawnFlockOutput struct {
	FlockID      string           `json:"flock_id"`
	Task         string           `json:"task"`
	TenantID     string           `json:"tenant_id,omitempty"`
	EgressPolicy string           `json:"egress_policy,omitempty"`
	Agents       []FlockAgentInfo `json:"agents"`
	TownWallURL  string           `json:"townwall_url"`
	PostURL      string           `json:"post_url"`
}

type ListFlocksInput struct {
	TenantID string `json:"tenant_id,omitempty"`
}

type ListFlocksOutput struct {
	Flocks []FlockInfo `json:"flocks"`
}

type FlockIdentityInput struct {
	FlockID  string `json:"flock_id"`
	TenantID string `json:"tenant_id,omitempty"`
}

type TownWallPostInput struct {
	FlockID  string `json:"flock_id"`
	AgentID  string `json:"agent_id"`
	Body     string `json:"body"`
	TenantID string `json:"tenant_id,omitempty"`
}

type TownWallHistoryOutput struct {
	Messages []TownWallMessage `json:"messages"`
}
```

- [x] **Step 3: validation helpers를 추가한다**

`internal/anvilmcp/tools.go`에 다음 helper를 추가한다.

```go
const maxFlockRoles = 20

func normalizeFlockRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		return nil, fmt.Errorf("roles must contain at least one role")
	}
	if len(roles) > maxFlockRoles {
		return nil, fmt.Errorf("roles must contain at most %d roles", maxFlockRoles)
	}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			return nil, fmt.Errorf("roles must not contain empty role")
		}
		if strings.ContainsAny(role, "/\\") {
			return nil, fmt.Errorf("roles must not contain path separators")
		}
		out = append(out, role)
	}
	return out, nil
}

func requireFlockID(value string) (string, error) {
	flockID := strings.TrimSpace(value)
	if flockID == "" {
		return "", fmt.Errorf("flock_id is required")
	}
	if strings.ContainsAny(flockID, "/\\") {
		return "", fmt.Errorf("flock_id must not contain path separators")
	}
	return flockID, nil
}
```

- [x] **Step 4: tool methods를 추가한다**

`internal/anvilmcp/tools.go`에 다음 methods를 추가한다.

```go
func (t *Tools) SpawnFlock(ctx context.Context, input SpawnFlockInput) (*SpawnFlockOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	egressPolicy, err := NormalizeEgressPolicy(input.EgressPolicy)
	if err != nil {
		return nil, err
	}
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return nil, fmt.Errorf("task must be non-empty")
	}
	roles, err := normalizeFlockRoles(input.Roles)
	if err != nil {
		return nil, err
	}
	resp, err := t.daemon.CreateFlock(ctx, FlockCreateRequest{
		Task:         task,
		Roles:        roles,
		TenantID:     tenantID,
		EgressPolicy: string(egressPolicy),
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_spawn_flock", "POST /flocks", err)
	}
	out := &SpawnFlockOutput{
		FlockID:      resp.FlockID,
		Task:         resp.Task,
		TenantID:     resp.TenantID,
		EgressPolicy: resp.EgressPolicy,
		Agents:       resp.Agents,
		TownWallURL:  resp.TownWallURL,
		PostURL:      resp.PostURL,
	}
	if out.Agents == nil {
		out.Agents = []FlockAgentInfo{}
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_spawn_flock", "POST /flocks"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPSpawnFlock(ctx context.Context, req *mcp.CallToolRequest, input SpawnFlockInput) (*mcp.CallToolResult, SpawnFlockOutput, error) {
	out, err := t.SpawnFlock(ctx, input)
	if err != nil || out == nil {
		return nil, SpawnFlockOutput{}, err
	}
	return nil, *out, nil
}
```

같은 패턴으로 다음 methods를 추가한다.

```go
func (t *Tools) ListFlocks(ctx context.Context, input ListFlocksInput) (*ListFlocksOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flocks, err := t.daemon.ListFlocks(ctx)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_list_flocks", "GET /flocks", err)
	}
	if flocks == nil {
		flocks = []FlockInfo{}
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_list_flocks", "GET /flocks"); err != nil {
		return nil, err
	}
	return &ListFlocksOutput{Flocks: flocks}, nil
}

func (t *Tools) MCPListFlocks(ctx context.Context, req *mcp.CallToolRequest, input ListFlocksInput) (*mcp.CallToolResult, ListFlocksOutput, error) {
	out, err := t.ListFlocks(ctx, input)
	if err != nil || out == nil {
		return nil, ListFlocksOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) GetFlock(ctx context.Context, input FlockIdentityInput) (*FlockInfo, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flockID, err := requireFlockID(input.FlockID)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.GetFlock(ctx, flockID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_get_flock", "GET /flocks/{flock_id}", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_get_flock", "GET /flocks/{flock_id}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPGetFlock(ctx context.Context, req *mcp.CallToolRequest, input FlockIdentityInput) (*mcp.CallToolResult, FlockInfo, error) {
	out, err := t.GetFlock(ctx, input)
	if err != nil || out == nil {
		return nil, FlockInfo{}, err
	}
	return nil, *out, nil
}
```

`DeleteFlock`, `PostTownWall`, `TownWallHistory`도 같은 방식으로 구현한다. 반드시 다음 validation을 넣는다.

```go
agentID := strings.TrimSpace(input.AgentID)
body := strings.TrimSpace(input.Body)
if agentID == "" || body == "" {
	return nil, fmt.Errorf("agent_id and body must be non-empty")
}
```

`TownWallHistory`는 daemon slice가 nil이면 `[]TownWallMessage{}`로 바꾸고 `{ "messages": [] }` wrapper를 반환한다.

- [x] **Step 5: `fakeDaemon`을 확장하고 tests를 추가한다**

`internal/anvilmcp/tools_test.go`의 `fakeDaemon`에 call counters와 methods를 추가한다.

```go
createFlockCalls int
createFlockReq   FlockCreateRequest
createFlockResp  *FlockCreateResponse
createFlockErr   error

listFlockCalls     int
listFlockResp      []FlockInfo
listFlockReturnNil bool
listFlockErr       error

getFlockCalls int
getFlockID    string
getFlockResp  *FlockInfo
getFlockErr   error

deleteFlockCalls int
deleteFlockID    string
deleteFlockResp  *RawDaemonResponse
deleteFlockErr   error

postTownWallCalls int
postTownWallID    string
postTownWallReq   TownWallPostRequest
postTownWallResp  *TownWallMessage
postTownWallErr   error

townWallHistoryCalls     int
townWallHistoryID        string
townWallHistoryResp      []TownWallMessage
townWallHistoryReturnNil bool
townWallHistoryErr       error
```

methods는 existing fake methods와 같은 style로 구현한다. 기본 response는 deterministic하게 둔다.

```go
func (f *fakeDaemon) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockErr != nil {
		return nil, f.createFlockErr
	}
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{
		FlockID:      "flock-1",
		Task:         req.Task,
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
		Agents:       []FlockAgentInfo{{AgentID: "worker-1", Role: "worker", VMID: "vm-1", AgentURL: "http://10.0.1.2:8080", Status: "ready"}},
		TownWallURL:  "http://127.0.0.1:3000/flocks/flock-1/wall",
		PostURL:      "http://127.0.0.1:3000/flocks/flock-1/post",
	}, nil
}
```

추가 tests:

```go
func TestToolsSpawnFlockForwardsTenantEgressAndRoles(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	out, err := tools.SpawnFlock(context.Background(), SpawnFlockInput{
		Task:         "ship review",
		Roles:        []string{" researcher ", "worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("SpawnFlock returned error: %v", err)
	}
	if daemon.createFlockCalls != 1 {
		t.Fatalf("CreateFlock calls = %d, want 1", daemon.createFlockCalls)
	}
	if daemon.createFlockReq.Task != "ship review" {
		t.Fatalf("task = %q, want ship review", daemon.createFlockReq.Task)
	}
	if got := strings.Join(daemon.createFlockReq.Roles, ","); got != "researcher,worker" {
		t.Fatalf("roles = %q, want researcher,worker", got)
	}
	if daemon.createFlockReq.TenantID != "tenant-1" || daemon.createFlockReq.EgressPolicy != "profile" {
		t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", daemon.createFlockReq.TenantID, daemon.createFlockReq.EgressPolicy)
	}
	if out.FlockID != "flock-1" || len(out.Agents) != 1 {
		t.Fatalf("output = %+v, want flock-1 with one agent", out)
	}
}

func TestToolsSpawnFlockRejectsInvalidInputBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)
	for _, input := range []SpawnFlockInput{
		{Task: "", Roles: []string{"worker"}},
		{Task: "x", Roles: nil},
		{Task: "x", Roles: []string{""}},
		{Task: "x", Roles: []string{"../worker"}},
		{Task: "x", Roles: []string{"worker"}, EgressPolicy: "internet"},
	} {
		if _, err := tools.SpawnFlock(context.Background(), input); err == nil {
			t.Fatalf("SpawnFlock(%+v) error = nil, want validation error", input)
		}
	}
	if daemon.createFlockCalls != 0 {
		t.Fatalf("CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}
}
```

나머지 tools에는 `DeleteFlock` empty `flock_id`, `PostTownWall` empty `agent_id/body`, `TownWallHistory` nil slice wrapper, audit default tenant success test를 추가한다.

- [x] **Step 6: tool tests를 실행한다**

```bash
go test ./internal/anvilmcp -run 'TestTools.*Flock|TestTools.*TownWall' -count=1
```

Expected: pass.

- [x] **Step 7: commit한다**

```bash
git add internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go
git commit -m "feat: expose Goosetown MCP tool handlers"
```

## Task 5: MCP inventory와 IronClaw schema 갱신

**Files:**

- Modify: `cmd/anvil-mcp/main.go`
- Modify: `cmd/anvil-mcp/main_test.go`
- Modify: `internal/anvilmcp/ironclaw_schema.go`
- Modify: `internal/anvilmcp/ironclaw_schema_test.go`

- [x] **Step 1: tool registration test의 want map을 먼저 갱신한다**

`cmd/anvil-mcp/main_test.go`의 `want` map에 다음 항목을 추가한다.

```go
"anvil_spawn_flock":          "Create a Goosetown flock of ephemera VMs and return its Town Wall endpoints.",
"anvil_list_flocks":          "List live Goosetown flocks known to the ephemera daemon.",
"anvil_get_flock":            "Return a Goosetown flock and its agent status by flock_id.",
"anvil_delete_flock":         "Delete a Goosetown flock and let the daemon tear down member VMs.",
"anvil_post_townwall":        "Append a message to a Goosetown flock Town Wall.",
"anvil_get_townwall_history": "Return the full Town Wall history for a Goosetown flock.",
```

- [x] **Step 2: failure를 확인한다**

```bash
go test ./cmd/anvil-mcp -run TestToolRegistrationsIncludeSnapshotTools -count=1
```

Expected: missing tool registration fail.

- [x] **Step 3: registrations를 추가한다**

`cmd/anvil-mcp/main.go`의 `toolRegistrations()`에 snapshot tools 뒤로 다음 entries를 추가한다.

```go
{
	name:        "anvil_spawn_flock",
	description: "Create a Goosetown flock of ephemera VMs and return its Town Wall endpoints.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPSpawnFlock)
	},
},
{
	name:        "anvil_list_flocks",
	description: "List live Goosetown flocks known to the ephemera daemon.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPListFlocks)
	},
},
{
	name:        "anvil_get_flock",
	description: "Return a Goosetown flock and its agent status by flock_id.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPGetFlock)
	},
},
{
	name:        "anvil_delete_flock",
	description: "Delete a Goosetown flock and let the daemon tear down member VMs.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPDeleteFlock)
	},
},
{
	name:        "anvil_post_townwall",
	description: "Append a message to a Goosetown flock Town Wall.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPPostTownWall)
	},
},
{
	name:        "anvil_get_townwall_history",
	description: "Return the full Town Wall history for a Goosetown flock.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPTownWallHistory)
	},
},
```

- [x] **Step 4: IronClaw schema 목록을 갱신한다**

`internal/anvilmcp/ironclaw_schema.go`의 `CurrentIronClawToolInputSchemas()`에 다음을 추가한다.

```go
toolInputSchemaFromStruct("anvil_spawn_flock", SpawnFlockInput{}),
toolInputSchemaFromStruct("anvil_list_flocks", ListFlocksInput{}),
toolInputSchemaFromStruct("anvil_get_flock", FlockIdentityInput{}),
toolInputSchemaFromStruct("anvil_delete_flock", FlockIdentityInput{}),
toolInputSchemaFromStruct("anvil_post_townwall", TownWallPostInput{}),
toolInputSchemaFromStruct("anvil_get_townwall_history", FlockIdentityInput{}),
```

`internal/anvilmcp/ironclaw_schema_test.go`에 tool name presence test를 추가한다.

```go
func TestCurrentIronClawSchemasIncludeGoosetownTools(t *testing.T) {
	schemas := CurrentIronClawToolInputSchemas()
	seen := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		seen[schema.ToolName] = true
	}
	for _, name := range []string{
		"anvil_spawn_flock",
		"anvil_list_flocks",
		"anvil_get_flock",
		"anvil_delete_flock",
		"anvil_post_townwall",
		"anvil_get_townwall_history",
	} {
		if !seen[name] {
			t.Fatalf("missing schema for %s", name)
		}
	}
}
```

- [x] **Step 5: inventory/schema tests를 실행한다**

```bash
go test ./internal/anvilmcp ./cmd/anvil-mcp -run 'Test.*IronClaw|Test.*ToolRegistration' -count=1
```

Expected: pass.

- [x] **Step 6: commit한다**

```bash
git add cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go internal/anvilmcp/ironclaw_schema.go internal/anvilmcp/ironclaw_schema_test.go
git commit -m "feat: register Goosetown MCP tool surface"
```

## Task 6: MCP smoke flock mode 추가

**Files:**

- Modify: `scripts/anvil-mcp-smoke.go`
- Modify: `scripts/anvil-mcp-e2e.sh`

- [x] **Step 1: smoke mode flag와 flock flow test plan을 추가한다**

`scripts/anvil-mcp-smoke.go`에 `mode` flag를 추가한다.

```go
mode := flag.String("mode", "lifecycle", "smoke mode: lifecycle, semantic, or flock")
```

`flag.Parse()` 뒤에 flock mode branch를 추가한다.

```go
if *mode == "flock" {
	return runFlockSmoke(ctx, clientSession)
}
```

- [x] **Step 2: flock smoke helper를 추가한다**

`scripts/anvil-mcp-smoke.go`에 다음 output structs와 helper를 추가한다.

```go
type flockAgentOutput struct {
	AgentID string `json:"agent_id"`
	Role    string `json:"role"`
	VMID    string `json:"vm_id"`
	Status  string `json:"status"`
}

type spawnFlockOutput struct {
	FlockID string             `json:"flock_id"`
	Agents  []flockAgentOutput `json:"agents"`
}

type listFlocksOutput struct {
	Flocks []struct {
		FlockID string `json:"flock_id"`
	} `json:"flocks"`
}

type townWallHistoryOutput struct {
	Messages []struct {
		AgentID string `json:"agent_id"`
		Body    string `json:"body"`
	} `json:"messages"`
}
```

```go
func runFlockSmoke(ctx context.Context, session *mcp.ClientSession) error {
	var spawned spawnFlockOutput
	if err := callStructured(ctx, session, "anvil_spawn_flock", map[string]any{
		"task":  "anvil flock smoke",
		"roles": []string{"orchestrator", "worker"},
	}, &spawned); err != nil {
		return err
	}
	if spawned.FlockID == "" || len(spawned.Agents) != 2 {
		return fmt.Errorf("spawned flock = %+v, want flock_id and two agents", spawned)
	}
	cleanup := true
	defer func() {
		if cleanup && spawned.FlockID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			var deleted rawOutput
			if err := callStructured(cleanupCtx, session, "anvil_delete_flock", map[string]any{"flock_id": spawned.FlockID}, &deleted); err != nil {
				log.Printf("cleanup delete flock failed for %s: %v", spawned.FlockID, err)
			}
		}
	}()

	var listed listFlocksOutput
	if err := callStructured(ctx, session, "anvil_list_flocks", map[string]any{}, &listed); err != nil {
		return err
	}

	var msg map[string]any
	if err := callStructured(ctx, session, "anvil_post_townwall", map[string]any{
		"flock_id": spawned.FlockID,
		"agent_id": "orchestrator",
		"body":     "anvil-flock-smoke-ok",
	}, &msg); err != nil {
		return err
	}

	var history townWallHistoryOutput
	if err := callStructured(ctx, session, "anvil_get_townwall_history", map[string]any{
		"flock_id": spawned.FlockID,
	}, &history); err != nil {
		return err
	}
	found := false
	for _, m := range history.Messages {
		if m.Body == "anvil-flock-smoke-ok" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("town wall history did not include smoke marker: %+v", history.Messages)
	}

	var deleted rawOutput
	if err := callStructured(ctx, session, "anvil_delete_flock", map[string]any{"flock_id": spawned.FlockID}, &deleted); err != nil {
		return err
	}
	cleanup = false
	fmt.Println("anvil MCP flock smoke test passed")
	return nil
}
```

- [x] **Step 3: wrapper에 `flock` mode를 추가한다**

`scripts/anvil-mcp-e2e.sh`가 mode case를 가지고 있으면 `flock`을 허용하고 내부 smoke command에 다음 flag를 전달한다.

```bash
go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -mode flock
```

기존 `lifecycle`, `semantic` 동작은 그대로 둔다.

- [x] **Step 4: script build 검증을 실행한다**

```bash
go run ./scripts/anvil-mcp-smoke.go -h
bash -n scripts/anvil-mcp-e2e.sh
```

Expected: pass.

- [x] **Step 5: commit한다**

```bash
git add scripts/anvil-mcp-smoke.go scripts/anvil-mcp-e2e.sh
git commit -m "test: add Goosetown MCP smoke mode"
```

## Task 7: 문서 현행화

**Files:**

- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `CONTEXT.md`

- [x] **Step 1: `docs/architecture/mcp-architecture.md`의 상태와 tool table을 갱신한다**

상태 section의 현재 확장 목록에 다음을 추가한다.

```markdown
- 현재 확장: optional tenant/egress contract, runtime audit, IronClaw/Gemini tool
  input schema compatibility 검증, Goosetown flock/Town Wall MCP tool surface
```

도구 계약 표에 다음 rows를 추가한다.

```markdown
| `anvil_spawn_flock` | `POST /flocks` | 역할 목록으로 Goosetown flock 생성 |
| `anvil_list_flocks` | `GET /flocks` | live flock 목록 조회 |
| `anvil_get_flock` | `GET /flocks/{flock_id}` | flock metadata와 agent 상태 조회 |
| `anvil_delete_flock` | `DELETE /flocks/{flock_id}` | flock 소속 VM 삭제를 daemon에 위임 |
| `anvil_post_townwall` | `POST /flocks/{flock_id}/post` | Town Wall message append |
| `anvil_get_townwall_history` | `GET /flocks/{flock_id}/wall/history` | Town Wall history 조회 |
```

비목표 section에 다음을 명시한다.

```markdown
- Town Wall SSE stream의 MCP tool 노출
- flock snapshot/restore
- flock alias 또는 `session_name` 재사용
- scheduler-aware cross-host flock placement
```

- [x] **Step 2: README MCP adapter tool 목록을 갱신한다**

IronClaw MCP 어댑터 section의 tool 목록에 다음 bullets를 추가한다.

```markdown
- `anvil_spawn_flock`:
  Goosetown 역할 목록으로 여러 ephemera VM을 하나의 flock으로 생성한다.

- `anvil_list_flocks` / `anvil_get_flock` / `anvil_delete_flock`:
  daemon이 소유한 flock registry와 member VM cleanup 계약을 MCP에서 호출한다.

- `anvil_post_townwall` / `anvil_get_townwall_history`:
  flock별 Town Wall coordination log에 message를 남기고 history를 조회한다.
```

Smoke test section에 다음 command를 추가한다.

```bash
scripts/anvil-mcp-e2e.sh flock
```

- [x] **Step 3: RELEASE_NOTES와 CONTEXT를 갱신한다**

`RELEASE_NOTES.md` Unreleased `추가됨`에 다음 항목을 넣는다.

```markdown
- anvil MCP Goosetown tool surface:
  - `anvil_spawn_flock`
  - `anvil_list_flocks`
  - `anvil_get_flock`
  - `anvil_delete_flock`
  - `anvil_post_townwall`
  - `anvil_get_townwall_history`
```

`CONTEXT.md`의 최근 후속 완료 상태에 다음 bullet을 추가한다.

```markdown
- Goosetown flock/Town Wall runtime API는 `anvil_*` MCP tool surface로 노출되며,
  기존 VM/snapshot tool 계약을 대체하지 않는 additive extension으로 취급한다.
```

- [x] **Step 4: 문서 검색 검증을 실행한다**

```bash
rg -n "anvil_spawn_flock|anvil_post_townwall|Goosetown.*MCP|flock snapshot|SSE" README.md docs/architecture/mcp-architecture.md RELEASE_NOTES.md CONTEXT.md
```

Expected: 새 tool과 비목표 문구가 확인된다.

- [x] **Step 5: commit한다**

```bash
git add README.md docs/architecture/mcp-architecture.md RELEASE_NOTES.md CONTEXT.md
git commit -m "docs: document Goosetown MCP surface"
```

## Task 8: 최종 검증

**Files:**

- Modify: none

- [x] **Step 1: focused test를 실행한다**

```bash
go test ./internal/orchestrator ./internal/anvilmcp ./cmd/anvil-mcp ./cmd/goose-daemon -run 'Test.*Flock|Test.*TownWall|Test.*IronClaw|Test.*ToolRegistration|TestDaemonClientFlockEndpoints' -count=1
```

Expected: pass.

- [x] **Step 2: 전체 CI-safe 검증을 실행한다**

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
git diff --check
```

Expected: all pass.

- [x] **Step 3: KVM host에서 daemon e2e를 실행한다**

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

Expected: 58단계 e2e pass. 특히 51-57 Goosetown flock 단계가 pass해야 한다.

- [x] **Step 4: MCP flock smoke를 실행한다**

Daemon이 이미 실행 중인 상태에서:

```bash
scripts/anvil-mcp-e2e.sh flock
```

Expected: `anvil MCP flock smoke test passed`.

- [x] **Step 5: 최종 상태를 확인한다**

```bash
git status --short
git log --oneline --max-count=8
```

Expected: 작업 변경만 남아 있고 unrelated `.gitignore`, `cmd/e2e-replay-server/`는 작업 commit에 포함되지 않는다.

## Self-Review

Spec coverage:

- Goosetown을 MCP tool surface로 승격한다: Tasks 3-5
- 기존 VM/snapshot tool 계약을 깨지 않는다: Task 5는 additive registration만 수행
- tenant/egress/audit 정합성을 유지한다: Tasks 2, 4
- scheduler/cross-host 범위 확장을 피한다: 범위 제외와 문서 Task 7
- KVM/e2e 검증 경로를 남긴다: Task 8

Placeholder scan:

- 미정 항목이나 나중 구현으로 남긴 항목 없음
- 각 코드 변경 task는 구체적인 type, method, test, command를 포함한다

Type consistency:

- `FlockCreateRequest`, `FlockCreateResponse`, `FlockInfo`, `TownWallPostRequest`, `TownWallMessage` 이름은 daemon client, tool handler, tests에서 동일하게 사용한다
- tool names는 registration, schema, docs에서 동일하다

## Post-Implementation Gate

2026-05-15 후속 grill-me 결과로 다음 보강을 완료했다.

- daemon direct `POST /flocks`도 blank `task`, empty role, `/` 또는 `\`가 포함된
  role을 flock registry 생성과 VM spawn 전에 `400`으로 거부한다.
- `cmd/goose-daemon/api_test.go`에 spawn 전 validation regression test를 추가했다.
- 이 계획 문서를 feature branch에 포함하고 실행 체크박스를 완료 상태로 현행화했다.
- 전체 KVM E2E를 재실행해 `sudo bash e2e_test.sh` 58단계가 pass했다.

Fresh verification:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n e2e_test.sh
bash -n scripts/anvil-mcp-e2e.sh
git diff --check
sudo bash e2e_test.sh
```

MCP flock smoke는 daemon 실행 상태에서 다음 명령으로 검증한다.

```bash
scripts/anvil-mcp-e2e.sh flock
```
