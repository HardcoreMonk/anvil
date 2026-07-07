# ReconcilePlacements 주기 배선 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `cmd/anvil-mcp` adapter가 `members_only` cross-host 모드일 때 `RuntimeRouter.ReconcilePlacements`를 시작 1회 + 주기(기본 60s)로 자동 실행해, daemon 재시작으로 잃은 hub/relay flock 등록·relay-token admission을 운영자 개입 없이 복구한다.

**Architecture:** `RuntimeRouter`에 `reconcileMu` 직렬화와 `StartReconcileLoop(ctx, interval, logf)` goroutine을 추가하고, adapter config에 `ANVIL_MCP_RECONCILE_INTERVAL`/`reconcile_interval`(ParseDuration, 기본 60s, `0`=off)을 신설한 뒤, `cmd/anvil-mcp/main.go`의 router 조립부에서 `members_only`일 때만 루프를 시작한다. 설계 spec: `docs/superpowers/specs/2026-07-07-reconcile-placements-wiring-design.md`.

**Tech Stack:** Go (module `ephemera`), `time.Ticker`, `sync.Mutex`, 표준 `testing` (fake `Daemon`).

## Global Constraints

- 브랜치: `feature/reconcile-loop` (local main에서 분기 — spec/plan 커밋 포함 상태).
- TDD RED→GREEN 필수: 실패 테스트 먼저 작성 → 실패 확인 → 구현 → 통과.
- 기존 테스트 제거/약화 금지. 커밋에 **git trailer 금지**.
- 로그에 relay token·daemon 주소 노출 금지 (flock/host 식별자만 — 기존 redaction 규율).
- `members_only` 모드가 아니면 interval 값과 무관하게 루프를 시작하지 않는다.
- `0` = 완전 비활성(시작 1회 포함 안 함). 음수·파싱 불가 값은 config 로드 시 에러.
- 각 task 종료 시 `go test ./cmd/... ./internal/... -count=1` 전체 실행 (부분 suite 금지 — 이 repo에서 부분 suite로 regression을 놓친 전례 있음).

---

## File Structure

- `internal/anvilmcp/config.go` — env/yaml 키 + `ReconcileInterval`(raw string) + `ReconcileIntervalParsed`(time.Duration, `yaml:"-"`) + LoadConfig 검증.
- `internal/anvilmcp/config_test.go` — 파싱/기본값/에러 테스트 추가.
- `internal/anvilmcp/runtime_router.go` — `reconcileMu` + `StartReconcileLoop`.
- `internal/anvilmcp/runtime_router_test.go` — 직렬화·루프 테스트 추가 (기존 `routerFakeDaemon` 재사용/확장).
- `cmd/anvil-mcp/main.go` — `newMCPDaemon`이 `*anvilmcp.RuntimeRouter`도 반환, `main()`에서 루프 시작.
- `cmd/anvil-mcp/main_test.go` — `newMCPDaemon` router 반환 테스트.
- 문서: `CONTEXT.md`, `README.md`, `docs/operations/runbook.md`, `docs/operations/2026-07-06-cross-host-town-wall-handoff.md`.

---

## Task 1: Config — `reconcile_interval` 파싱·검증

**Files:**
- Modify: `internal/anvilmcp/config.go`
- Test: `internal/anvilmcp/config_test.go`

**Interfaces:**
- Produces: `Config.ReconcileInterval string` (raw), `Config.ReconcileIntervalParsed time.Duration` — LoadConfig가 채움. 이후 task는 `ReconcileIntervalParsed`만 사용.

- [ ] **Step 1: 실패 테스트 작성** — `internal/anvilmcp/config_test.go`에 추가 (기존 테스트들의 `ConfigSource{Getenv: ..., ReadFile: ...}` fake 패턴을 그대로 따른다; 기존 테스트에서 사용하는 helper가 있으면 재사용):

```go
func TestLoadConfigReconcileIntervalDefault(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv:   func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 60*time.Second {
		t.Fatalf("default ReconcileIntervalParsed = %v, want 60s", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalEnvOverridesYAML(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
				return "5m"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) {
			return []byte("reconcile_interval: 30s\n"), nil
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 5*time.Minute {
		t.Fatalf("ReconcileIntervalParsed = %v, want 5m (env가 yaml을 override)", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalZeroDisables(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
				return "0"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 0 {
		t.Fatalf("ReconcileIntervalParsed = %v, want 0 (off)", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"-30s", "banana"} {
		_, err := LoadConfig(ConfigSource{
			Getenv: func(key string) string {
				if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
					return bad
				}
				return ""
			},
			ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		})
		if err == nil {
			t.Fatalf("LoadConfig(%q): want error, got nil", bad)
		}
	}
}
```

주의: 기존 config_test.go가 `os.ErrNotExist` 대신 다른 not-found 처리를 쓰면 그 패턴을 따른다. import에 `time` 추가.

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/anvilmcp -run TestLoadConfigReconcileInterval -count=1 -v` / Expected: FAIL (`cfg.ReconcileIntervalParsed` undefined — 컴파일 에러도 RED로 인정).

- [ ] **Step 3: 구현** — `internal/anvilmcp/config.go`:

env const 블록에 추가:

```go
	envReconcileInterval = "ANVIL_MCP_RECONCILE_INTERVAL"
```

`Config` struct에 추가 (`CrossHostFlockCreateMode` 아래):

```go
	// ReconcileInterval is the raw reconcile-loop interval (time.ParseDuration
	// format). ReconcileIntervalParsed is the validated value LoadConfig fills:
	// 60s default when unset, 0 disables the loop entirely.
	ReconcileInterval       string        `yaml:"reconcile_interval"`
	ReconcileIntervalParsed time.Duration `yaml:"-"`
```

LoadConfig의 env override 구간(다른 `if v := ...getenv` 블록들 옆)에 추가:

```go
	if v := strings.TrimSpace(getenv(envReconcileInterval)); v != "" {
		cfg.ReconcileInterval = v
	}
```

LoadConfig의 validation 구간(다른 검증들 옆, return 전)에 추가:

```go
	cfg.ReconcileInterval = strings.TrimSpace(cfg.ReconcileInterval)
	if cfg.ReconcileInterval == "" {
		cfg.ReconcileIntervalParsed = 60 * time.Second
	} else {
		d, err := time.ParseDuration(cfg.ReconcileInterval)
		if err != nil {
			return Config{}, fmt.Errorf("reconcile_interval must be a duration like 60s (got %q): %w", cfg.ReconcileInterval, err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("reconcile_interval must not be negative (got %q)", cfg.ReconcileInterval)
		}
		cfg.ReconcileIntervalParsed = d
	}
```

import에 `time` 추가.

- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/anvilmcp -run TestLoadConfigReconcileInterval -count=1 -v` / Expected: PASS ×4.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/anvilmcp/config.go internal/anvilmcp/config_test.go
git commit -m 'feat(mcp): add reconcile_interval adapter config (60s default, 0=off)'
```

---

## Task 2: `ReconcilePlacements` 실행 직렬화 (`reconcileMu`)

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go` (`RuntimeRouter` struct, `ReconcilePlacements`)
- Test: `internal/anvilmcp/runtime_router_test.go`

**Interfaces:**
- Produces: `ReconcilePlacements`는 이제 내부적으로 `r.reconcileMu`로 전체 실행을 직렬화한다. 시그니처 무변경 — 기존 호출부/테스트 영향 없음.

- [ ] **Step 1: 실패 테스트 작성** — `internal/anvilmcp/runtime_router_test.go`에 추가:

```go
// blockingListDaemon은 ListVMs 진입을 entered로 알리고 release까지 블록한다.
// Daemon embed는 nil — ReconcilePlacements는 ListVMs만 type-assert해 호출한다.
type blockingListDaemon struct {
	Daemon
	entered  chan struct{}
	release  chan struct{}
	inflight atomic.Int32
	maxSeen  atomic.Int32
}

func (d *blockingListDaemon) ListVMs(ctx context.Context) ([]VMInfo, error) {
	cur := d.inflight.Add(1)
	for {
		max := d.maxSeen.Load()
		if cur <= max || d.maxSeen.CompareAndSwap(max, cur) {
			break
		}
	}
	d.entered <- struct{}{}
	<-d.release
	d.inflight.Add(-1)
	return nil, nil
}

func TestReconcilePlacementsSerializesConcurrentCalls(t *testing.T) {
	daemon := &blockingListDaemon{
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{},
	)

	done := make(chan error, 2)
	go func() { done <- router.ReconcilePlacements(context.Background()) }()
	<-daemon.entered // 첫 호출이 ListVMs 안에서 블록 중
	go func() { done <- router.ReconcilePlacements(context.Background()) }()

	// 직렬화되면 두 번째 호출은 release 전에 ListVMs에 진입하지 못한다.
	select {
	case <-daemon.entered:
		t.Fatal("second ReconcilePlacements entered ListVMs while first still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(daemon.release)
	<-daemon.entered // 두 번째 호출 진입
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("ReconcilePlacements: %v", err)
		}
	}
	if got := daemon.maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent ListVMs = %d, want 1", got)
	}
}
```

import에 `sync/atomic`(미사용 시)과 `time` 확인. `atomic.Int32`는 Go 1.19+ — `go.mod`의 Go 버전이 더 낮으면 `int32` + `atomic.AddInt32` 계열로 동일 로직을 쓴다.

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/anvilmcp -run TestReconcilePlacementsSerializesConcurrentCalls -count=1 -race` / Expected: FAIL ("second ReconcilePlacements entered ListVMs...").

- [ ] **Step 3: 구현** — `internal/anvilmcp/runtime_router.go`:

`RuntimeRouter` struct에 추가 (`mu sync.RWMutex` 아래):

```go
	// reconcileMu serializes ReconcilePlacements end-to-end so the periodic
	// loop and manual calls never run concurrently (placement replace + wall
	// re-registration is not safe to interleave with itself).
	reconcileMu sync.Mutex
```

`ReconcilePlacements` 함수 첫 줄에 추가:

```go
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
```

- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/anvilmcp -run TestReconcilePlacementsSerializesConcurrentCalls -count=1 -race` / Expected: PASS. 기존 reconcile 테스트도: `go test ./internal/anvilmcp -run TestRuntimeRouterReconcile -count=1` PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go
git commit -m 'feat(mcp): serialize ReconcilePlacements end-to-end'
```

---

## Task 3: `StartReconcileLoop` — 시작 1회 + 주기 실행

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go`
- Test: `internal/anvilmcp/runtime_router_test.go`

**Interfaces:**
- Consumes: Task 2의 직렬화된 `ReconcilePlacements`.
- Produces: `func (r *RuntimeRouter) StartReconcileLoop(ctx context.Context, interval time.Duration, logf func(format string, args ...any))` — Task 4의 main 배선이 호출. `interval <= 0`이거나 `r == nil`이면 no-op. `logf`는 nil 허용(무로그).

- [ ] **Step 1: 실패 테스트 작성** — `internal/anvilmcp/runtime_router_test.go`에 추가:

```go
// countingListDaemon은 ListVMs 호출 횟수만 센다. err가 설정되면 항상 그 에러를 반환한다.
type countingListDaemon struct {
	Daemon
	calls atomic.Int32
	err   error
}

func (d *countingListDaemon) ListVMs(ctx context.Context) ([]VMInfo, error) {
	d.calls.Add(1)
	return nil, d.err
}

func waitForCalls(t *testing.T, d *countingListDaemon, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ListVMs calls = %d, want >= %d within 3s", d.calls.Load(), want)
}

func TestStartReconcileLoopRunsImmediatelyThenPeriodically(t *testing.T) {
	daemon := &countingListDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router.StartReconcileLoop(ctx, 10*time.Millisecond, nil)
	waitForCalls(t, daemon, 3) // 시작 1회 + 주기 최소 2회
}

func TestStartReconcileLoopStopsOnContextCancel(t *testing.T) {
	daemon := &countingListDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	router.StartReconcileLoop(ctx, 10*time.Millisecond, nil)
	waitForCalls(t, daemon, 2)
	cancel()

	time.Sleep(30 * time.Millisecond) // 취소 전 시작된 in-flight 실행 소진
	settled := daemon.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := daemon.calls.Load(); got != settled {
		t.Fatalf("ListVMs calls grew after cancel: %d -> %d", settled, got)
	}
}

func TestStartReconcileLoopContinuesAfterErrorAndLogs(t *testing.T) {
	daemon := &countingListDaemon{err: errors.New("boom")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var logged []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	router.StartReconcileLoop(ctx, 10*time.Millisecond, logf)
	waitForCalls(t, daemon, 3) // 에러에도 루프 지속

	mu.Lock()
	defer mu.Unlock()
	if len(logged) == 0 {
		t.Fatal("reconcile errors were not logged")
	}
	if !strings.Contains(logged[0], "boom") || !strings.Contains(logged[0], "host-a") {
		t.Fatalf("logged[0] = %q, want the host-scoped error (contains boom and host-a)", logged[0])
	}
}

func TestStartReconcileLoopNoopWhenDisabled(t *testing.T) {
	daemon := &countingListDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router.StartReconcileLoop(ctx, 0, nil)
	time.Sleep(50 * time.Millisecond)
	if got := daemon.calls.Load(); got != 0 {
		t.Fatalf("ListVMs calls = %d, want 0 (interval 0 = off)", got)
	}
}
```

import에 `errors`, `fmt`, `strings`, `sync` 확인 (기존 파일에 이미 있으면 생략).

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/anvilmcp -run TestStartReconcileLoop -count=1 -race` / Expected: FAIL (`StartReconcileLoop` undefined).

- [ ] **Step 3: 구현** — `internal/anvilmcp/runtime_router.go`의 `ReconcilePlacements` 아래에 추가:

```go
// StartReconcileLoop runs ReconcilePlacements once immediately and then every
// interval until ctx is cancelled. interval <= 0 disables the loop entirely
// (including the immediate run). Failures are logged through logf (flock/host
// identifiers only — relay tokens and daemon addresses never appear) and the
// loop keeps running: reconcile must never block or kill the adapter.
func (r *RuntimeRouter) StartReconcileLoop(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	if r == nil || interval <= 0 {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	run := func() {
		if err := r.ReconcilePlacements(ctx); err != nil {
			logf("anvil-mcp: reconcile placements: %v", err)
		}
	}
	go func() {
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
```

- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/anvilmcp -run TestStartReconcileLoop -count=1 -race` / Expected: PASS ×4.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go
git commit -m 'feat(mcp): StartReconcileLoop — startup-once + periodic reconcile'
```

---

## Task 4: adapter main 배선 (`members_only` 게이트)

**Files:**
- Modify: `cmd/anvil-mcp/main.go` (`newMCPDaemon` 시그니처, `main()`)
- Test: `cmd/anvil-mcp/main_test.go`

**Interfaces:**
- Consumes: Task 1 `cfg.ReconcileIntervalParsed`, Task 3 `StartReconcileLoop`.
- Produces: `newMCPDaemon(cfg anvilmcp.Config, httpClient *http.Client) (anvilmcp.Daemon, *anvilmcp.RuntimeRouter, error)` — router는 router config가 있을 때 non-nil. 루프 시작 게이트는 테스트 가능한 `shouldStartReconcileLoop(cfg anvilmcp.Config, router *anvilmcp.RuntimeRouter) bool` 함수로 추출: `cfg.CrossHostFlockCreateMode == "members_only" && router != nil && cfg.ReconcileIntervalParsed > 0`.

- [ ] **Step 1: 실패 테스트 작성** — `cmd/anvil-mcp/main_test.go`에 추가. 기존 테스트가 `newMCPDaemon`을 호출하는 패턴(임시 state 파일 경로, hosts fixture)을 찾아 동일하게 구성한다. 기존 호출부가 2-값 반환을 가정하면 이 task에서 함께 3-값으로 갱신한다:

```go
func TestNewMCPDaemonReturnsRouterForMembersOnly(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "scheduler.json")
	hostsPath := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(hostsPath, []byte("hosts:\n  - name: host-a\n    endpoint: http://127.0.0.1:3000\n    max_vms: 4\n"), 0o600); err != nil {
		t.Fatalf("write hosts file: %v", err)
	}
	cfg := anvilmcp.Config{
		DaemonURL:                anvilmcp.DefaultDaemonURL,
		DefaultTimeoutSeconds:    anvilmcp.DefaultTimeoutSeconds,
		SchedulerStatePath:       statePath,
		SchedulerHostsFile:       hostsPath,
		CrossHostFlockCreateMode: "members_only",
	}
	daemon, router, err := newMCPDaemon(cfg, http.DefaultClient)
	if err != nil {
		t.Fatalf("newMCPDaemon: %v", err)
	}
	if daemon == nil {
		t.Fatal("daemon is nil")
	}
	if router == nil {
		t.Fatal("router is nil — reconcile loop wiring needs it in members_only mode")
	}
}

func TestShouldStartReconcileLoopGates(t *testing.T) {
	router := &anvilmcp.RuntimeRouter{} // 게이트 판정에는 nil 여부만 쓰인다
	cases := []struct {
		name   string
		mode   string
		router *anvilmcp.RuntimeRouter
		ivl    time.Duration
		want   bool
	}{
		{"members_only on", "members_only", router, 60 * time.Second, true},
		{"mode empty", "", router, 60 * time.Second, false},
		{"router nil", "members_only", nil, 60 * time.Second, false},
		{"interval zero", "members_only", router, 0, false},
	}
	for _, tc := range cases {
		cfg := anvilmcp.Config{
			CrossHostFlockCreateMode: tc.mode,
			ReconcileIntervalParsed:  tc.ivl,
		}
		if got := shouldStartReconcileLoop(cfg, tc.router); got != tc.want {
			t.Fatalf("%s: shouldStartReconcileLoop = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

주의: `&anvilmcp.RuntimeRouter{}` 직접 생성이 unexported 필드 때문에 불가하면 `NewRuntimeRouter(anvilmcp.NewScheduler(nil, nil, nil), nil)`로 대체한다.

주의: hosts yaml 스키마는 `internal/anvilmcp`의 `LoadSchedulerHostsFile` 테스트/fixture에서 실제 필드명을 확인해 맞춘다(위 `max_vms` 등이 다르면 fixture를 그 스키마로 교체).

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/anvil-mcp -run TestNewMCPDaemonReturnsRouterForMembersOnly -count=1` / Expected: FAIL (2-값 반환 함수에 3-값 대입 — 컴파일 에러 RED).

- [ ] **Step 3: 구현** — `cmd/anvil-mcp/main.go`:

`newMCPDaemon` 시그니처와 return 3곳 수정:

```go
func newMCPDaemon(cfg anvilmcp.Config, httpClient *http.Client) (anvilmcp.Daemon, *anvilmcp.RuntimeRouter, error) {
```

- 에러 return들: `return nil, nil, err` 형태로.
- router 없이 base만 쓰는 경로: `return base, nil, nil`.
- router 생성 후: `members_only` 분기는 `return anvilmcp.NewReplicatingDaemonWithOptions(...), router, nil`, 일반 분기는 `return anvilmcp.NewReplicatingDaemon(base, router), router, nil`.

`main()` 수정 — 호출부와 루프 시작 + 프로세스 ctx:

```go
	daemon, router, err := newMCPDaemon(cfg, http.DefaultClient)
	if err != nil {
		log.Fatalf("configure daemon: %v", err)
	}
```

`server.Run` 호출을 ctx 공유로 교체:

```go
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if shouldStartReconcileLoop(cfg, router) {
		router.StartReconcileLoop(ctx, cfg.ReconcileIntervalParsed, log.Printf)
	}

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
```

게이트 함수 (main.go, `newMCPDaemon` 근처):

```go
// shouldStartReconcileLoop gates the periodic reconcile loop: members_only
// cross-host mode only (other configurations have no routed flock registry),
// a router must exist, and interval 0 disables the loop entirely.
func shouldStartReconcileLoop(cfg anvilmcp.Config, router *anvilmcp.RuntimeRouter) bool {
	return cfg.CrossHostFlockCreateMode == "members_only" && router != nil && cfg.ReconcileIntervalParsed > 0
}
```

기존 `newMCPDaemon` 호출부가 테스트에 더 있으면 전부 3-값으로 갱신한다 (`grep -n 'newMCPDaemon(' cmd/anvil-mcp/*.go`).

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/anvil-mcp -count=1` / Expected: PASS (신규 + 기존 전부).

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
go build ./cmd/anvil-mcp
git add cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go
git commit -m 'feat(mcp): wire periodic reconcile loop into adapter (members_only)'
```

---

## Task 5: 문서 반영

**Files:**
- Modify: `CONTEXT.md` (고정된 런타임 계약 목록)
- Modify: `README.md` (adapter 환경 변수 표/목록 — `ANVIL_MCP_AUDIT_LOG` 항목 옆)
- Modify: `docs/operations/runbook.md` (flock/wall 운영 절차 부근)
- Modify: `docs/operations/2026-07-06-cross-host-town-wall-handoff.md` (Follow-Up Tasks)

**Interfaces:** 없음 (docs-only).

- [ ] **Step 1: CONTEXT.md** — `## 고정된 런타임 계약` 목록의 MCP adapter env 항목들(`ANVIL_MCP_AUDIT_LOG` 부근)에 추가:

```markdown
- MCP adapter reconcile 주기 환경 변수: `ANVIL_MCP_RECONCILE_INTERVAL`
  (`time.ParseDuration` 형식, 기본 `60s`, `0`=off. `members_only` cross-host
  모드에서만 루프가 돌며 daemon 재시작 후 hub/relay wall 등록을 자동 복구)
```

- [ ] **Step 2: README.md** — adapter 환경 변수 나열부(`ANVIL_MCP_SCHEDULER_STATE` 등이 설명되는 곳)에 같은 내용 1-2줄 추가. 표 형식이면 표 행으로, 목록이면 목록 항목으로 — 주변 형식을 따른다.

- [ ] **Step 3: runbook.md** — cross-host wall 절차(flock 삭제/e2e 부근)에 추가:

```markdown
daemon 재시작으로 hub/relay flock 등록과 relay-token admission이 사라진 경우,
`members_only` 모드 adapter가 `ANVIL_MCP_RECONCILE_INTERVAL`(기본 60s) 주기로
자동 재등록한다. 수동 개입은 adapter가 꺼져 있거나 `0`으로 비활성화된 경우에만
필요하다.
```

- [ ] **Step 4: handoff Follow-Up CLOSED 표기** — `docs/operations/2026-07-06-cross-host-town-wall-handoff.md`의 "`ReconcilePlacements` 주기적 control loop 배선" 항목을 다른 CLOSED 항목과 같은 형식으로 취소선 + CLOSED(구현 커밋 hash, 2026-07-07 reconcile-loop slice) 처리.

- [ ] **Step 5: 커밋**

```bash
git add CONTEXT.md README.md docs/operations/runbook.md docs/operations/2026-07-06-cross-host-town-wall-handoff.md
git commit -m 'docs: document adapter reconcile loop (ANVIL_MCP_RECONCILE_INTERVAL)'
```

---

## Final verification gate (after all tasks)

```bash
go test ./cmd/... ./internal/... -count=1
go build ./cmd/goose-daemon ./cmd/anvil-mcp ./cmd/anvil-scheduler ./cmd/ephemera-ctl
git diff --check
```

Expected: 전체 suite green, 4 builds 성공, whitespace clean. KVM e2e는 이 슬라이스가 기존 daemon 경로를 바꾸지 않으므로 필수 아님 — 단, `scripts/anvil-mcp-e2e.sh lifecycle`을 돌릴 수 있는 환경이면 smoke로 1회 확인.
