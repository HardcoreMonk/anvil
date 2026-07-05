// [학습 주석] internal/orchestrator/watchdog.go
//
// flock 멤버 VM들의 in-guest /health를 주기적으로 찔러보고, 연속 실패가 임계치를
// 넘으면 해당 에이전트를 dead로 표시하는 백그라운드 폴러다. v0.3.5의 관찰성 훅
// (OnDead/OnHeal/OnProbeDuration)에 이어 v0.4.3에서 "paused 상태는 실패로 세지 않는다"
// 는 예외 처리가 들어왔다 — Firecracker pause는 게스트 실행 자체를 멈추므로 /health가
// 당연히 응답하지 않는데, 이를 실패로 오인하면 pauseFlock으로 잠깐 멈춘 에이전트가
// watchdog에 의해 dead로 마킹되는 오작동이 생긴다(orchestrator_api.go의 pauseFlock/
// resumeFlock과 짝을 이룬다). v0.4.4에서 GET /watchdog/status(WatchdogStatus)가
// 추가되어 운영자가 현재 실패 카운트/사망 마킹 상태를 조회할 수 있게 됐다.
//
// anvil은 이 파일 자체를 upstream 그대로 채택했다(adopted) — flock 개념이 anvil에서도
// 그대로 쓰이기 때문에 별도 적응이 필요 없었다.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AgentLocator maps a VM ID back to its flock/agent identity. Returns ok=false
// when the VM is not a flock member (standalone VM — watchdog ignores it).
type AgentLocator func(vmID string) (flockID, agentID string, ok bool)

// VMLister returns the current set of running VMs the watchdog should probe.
type VMLister func() []VMRef

// VMRef is the minimum information the watchdog needs to probe a VM's
// in-guest /health endpoint.
type VMRef struct {
	VMID    string
	GuestIP string
}

// Watchdog polls every registered VM's in-guest /health endpoint on a fixed
// interval. After dyingThreshold consecutive failures the agent is marked
// dead in the flock registry and a notice is posted to that flock's Town
// Wall. By default a revived VM is NOT auto-marked back to ready — operators
// decide when to clear the dead state. Setting autoHeal=true via Configure
// flips this to opt-in self-healing.
type Watchdog struct {
	interval       time.Duration
	httpTimeout    time.Duration
	dyingThreshold int
	agentPort      int
	autoHeal       bool

	flockMgr *FlockManager
	locator  AgentLocator
	lister   VMLister
	client   *http.Client

	mu         sync.Mutex
	failCount  map[string]int
	deadMarked map[string]bool

	// Observability hooks (v0.3.5). Nil-safe — the watchdog package does not
	// depend on cmd/goose-daemon's metrics registry. The daemon wires these
	// after constructing the watchdog so package orchestrator stays free of
	// metrics imports.
	//
	// OnDead fires once per agent at the moment the dyingThreshold is crossed.
	// OnHeal fires once when autoHeal returns a dead agent to ready.
	// OnProbeDuration fires after every /health probe, regardless of outcome.
	OnDead          func(flockID, agentID, vmID string)
	OnHeal          func(flockID, agentID, vmID string)
	OnProbeDuration func(d time.Duration)

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewWatchdog wires the watchdog with all dependencies it needs from the
// control plane. Default tunables are 5s interval / 1s timeout / 3 fails.
// Use Configure (from external packages) or set the unexported fields
// directly in in-package tests to override before Start is called.
func NewWatchdog(fm *FlockManager, locator AgentLocator, lister VMLister, agentPort int) *Watchdog {
	return &Watchdog{
		interval:       5 * time.Second,
		httpTimeout:    1 * time.Second,
		dyingThreshold: 3,
		agentPort:      agentPort,
		flockMgr:       fm,
		locator:        locator,
		lister:         lister,
		client:         &http.Client{Timeout: 1 * time.Second},
		failCount:      make(map[string]int),
		deadMarked:     make(map[string]bool),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

// Configure overrides watchdog tunables. Must be called BEFORE Start.
// Zero or negative interval/httpTimeout/threshold values are ignored
// (defaults preserved). When interval < httpTimeout, interval is clamped up
// to httpTimeout and a warning is logged — otherwise a probe could outlast
// its own tick window.
//
// autoHeal=true enables opt-in self-healing: a VM previously marked dead
// that starts responding will be returned to AgentStatusReady, the new
// status persisted, and a recovery notice posted to the flock's Town Wall.
// Default (false) preserves the sticky-dead policy.
func (wd *Watchdog) Configure(interval, httpTimeout time.Duration, threshold int, autoHeal bool) {
	if interval > 0 {
		wd.interval = interval
	}
	if httpTimeout > 0 {
		wd.httpTimeout = httpTimeout
		wd.client.Timeout = httpTimeout
	}
	if threshold > 0 {
		wd.dyingThreshold = threshold
	}
	if wd.interval < wd.httpTimeout {
		slog.Warn("watchdog: clamping interval up to http timeout", "interval", wd.interval, "http_timeout", wd.httpTimeout)
		wd.interval = wd.httpTimeout
	}
	wd.autoHeal = autoHeal
}

// Start launches the polling goroutine. Safe to call exactly once per
// Watchdog instance. The returned Watchdog must be stopped via Stop before
// the process exits to release the goroutine.
func (wd *Watchdog) Start() {
	go wd.loop()
}

// Stop signals the polling goroutine to exit and blocks until it has.
// Stop must be called BEFORE shutting down the HTTP server that owns the
// VMLister callback's data — otherwise the watchdog can observe a partly
// torn-down vms map and read stale GuestIP values.
func (wd *Watchdog) Stop() {
	close(wd.stopCh)
	<-wd.doneCh
}

func (wd *Watchdog) loop() {
	defer close(wd.doneCh)
	ticker := time.NewTicker(wd.interval)
	defer ticker.Stop()
	for {
		select {
		case <-wd.stopCh:
			return
		case <-ticker.C:
			wd.tick()
		}
	}
}

// tick probes every currently-registered VM in parallel. Bounded by the
// VMLister snapshot size; HTTP timeout caps the worst-case duration.
func (wd *Watchdog) tick() {
	vms := wd.lister()
	if len(vms) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, v := range vms {
		wg.Add(1)
		go func(v VMRef) {
			defer wg.Done()
			wd.checkOne(v)
		}(v)
	}
	wg.Wait()
}

func (wd *Watchdog) checkOne(v VMRef) {
	url := fmt.Sprintf("http://%s:%d/health", v.GuestIP, wd.agentPort)
	start := time.Now()
	defer func() {
		if wd.OnProbeDuration != nil {
			wd.OnProbeDuration(time.Since(start))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), wd.httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		wd.onFailure(v)
		return
	}
	resp, err := wd.client.Do(req)
	if err != nil {
		wd.onFailure(v)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		wd.onSuccess(v.VMID)
		return
	}
	wd.onFailure(v)
}

// onSuccess clears any accumulated fail count. By default, deadMarked is
// preserved — a flapping VM that revived briefly should not be auto-cleared.
// With Configure(..., autoHeal=true), a previously-dead VM that responds
// successfully is returned to AgentStatusReady, the change persisted, and
// a recovery notice posted to the flock's Town Wall.
func (wd *Watchdog) onSuccess(vmID string) {
	wd.mu.Lock()
	if wd.failCount[vmID] > 0 {
		delete(wd.failCount, vmID)
	}
	wasDead := wd.deadMarked[vmID]
	if !wd.autoHeal || !wasDead {
		wd.mu.Unlock()
		return
	}
	delete(wd.deadMarked, vmID)
	wd.mu.Unlock()

	flockID, agentID, ok := wd.locator(vmID)
	if !ok {
		return
	}
	flock, ok := wd.flockMgr.Get(flockID)
	if !ok {
		return
	}
	flock.UpdateAgentStatus(agentID, AgentStatusReady)
	if err := flock.Persist(wd.flockMgr.WorkDir()); err != nil {
		slog.Warn("watchdog: persist auto-heal failed", "agent_id", agentID, "err", err)
	}
	if _, err := flock.TownWall.Post(
		"orchestrator",
		fmt.Sprintf("%s recovered - auto-healed to ready", agentID),
	); err != nil {
		slog.Warn("watchdog: post auto-heal notice failed", "agent_id", agentID, "err", err)
	}
	if wd.OnHeal != nil {
		wd.OnHeal(flockID, agentID, vmID)
	}
}

// onFailure increments the fail counter and, on the threshold transition,
// updates the agent status and posts a Town Wall notice exactly once. Paused
// agents are skipped before counting because Firecracker pause intentionally
// makes /health unavailable.
// [학습] paused 예외 처리가 이 함수 맨 위, failCount를 늘리기도 전에 걸린다는 점이
// 핵심이다 — paused 상태로 확인되면 실패 카운트를 아예 리셋하고 즉시 반환하므로,
// pause 도중에는 dyingThreshold를 향한 카운트가 절대 누적되지 않는다. flock이
// 삭제된 뒤 남은 goroutine이 이 함수를 호출하는 race는 "flockMgr.Get 실패 시 조용히
// 리턴"으로 허용한다(race-tolerant라고 주석에 명시).
func (wd *Watchdog) onFailure(v VMRef) {
	flockID, agentID, ok := wd.locator(v.VMID)
	if !ok {
		// Not a flock member — standalone VMs aren't watchdog targets.
		return
	}
	flock, ok := wd.flockMgr.Get(flockID)
	if !ok {
		// Flock was deleted between the lister snapshot and now. Race-tolerant.
		return
	}
	if flock.AgentStatus(agentID) == AgentStatusPaused {
		// A paused flock member intentionally doesn't answer /health (v0.4.3);
		// don't count this as a failure or mark it dead.
		wd.mu.Lock()
		delete(wd.failCount, v.VMID)
		wd.mu.Unlock()
		return
	}

	wd.mu.Lock()
	wd.failCount[v.VMID]++
	count := wd.failCount[v.VMID]
	alreadyMarked := wd.deadMarked[v.VMID]
	wd.mu.Unlock()

	if alreadyMarked || count < wd.dyingThreshold {
		return
	}

	if !flock.MarkAgentDeadIfNotPaused(agentID) {
		wd.mu.Lock()
		delete(wd.failCount, v.VMID)
		wd.mu.Unlock()
		return
	}
	if err := flock.Persist(wd.flockMgr.WorkDir()); err != nil {
		// The in-memory mark already took effect; a missed disk write means
		// the dead state will be lost on the next daemon restart, which the
		// next probe will re-detect. Logged for operator visibility.
		slog.Warn("watchdog: persist dead status failed", "agent_id", agentID, "err", err)
	}
	if _, err := flock.TownWall.Post(
		"orchestrator",
		fmt.Sprintf("%s unresponsive after %d health probes - marked dead",
			agentID, wd.dyingThreshold),
	); err != nil {
		slog.Warn("watchdog: post dead notice failed", "agent_id", agentID, "err", err)
	}

	wd.mu.Lock()
	wd.deadMarked[v.VMID] = true
	wd.mu.Unlock()

	if wd.OnDead != nil {
		wd.OnDead(flockID, agentID, v.VMID)
	}
}

// ForgetVM clears any cached failure state for vmID. Call after a VM is
// destroyed (per-agent restart, DELETE) so a recycled vmID — or a future
// probe of a long-dead one — does not inherit the previous run's
// deadMarked bit. Safe to call concurrently with the polling loop.
// [학습] 함정: vmID는 daemon이 "vm-<UnixNano>" 형태로 매번 새로 발급하므로 실제
// 충돌 가능성은 희박하지만, restart/removeFlockAgent처럼 "옛 VM을 지우고 같은
// agent_id에 새 VM을 붙이는" 흐름에서 ForgetVM을 호출하지 않으면 새 VM이 옛 vmID의
// failCount/deadMarked 상태를 전혀 상속하지 않는 게 아니라(vmID 자체가 다르므로),
// 오히려 "옛 vmID 엔트리가 map에 영원히 남아 메모리를 누수"하는 문제가 생긴다 —
// 그래서 restartAgent/removeFlockAgent/changeFlockAgentRole이 destroyVM 직후 반드시
// ForgetVM을 호출한다.
func (wd *Watchdog) ForgetVM(vmID string) {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	delete(wd.failCount, vmID)
	delete(wd.deadMarked, vmID)
}

// WatchdogStatus is a point-in-time snapshot of the watchdog's tunables and
// per-VM health state, served by GET /watchdog/status (v0.4.4). VMFailCounts
// holds only VMs with a non-zero consecutive-failure count; VMDeadMarked lists
// VMs the watchdog has marked dead. Both are empty when every agent is healthy.
type WatchdogStatus struct {
	IntervalSec    int            `json:"interval_sec"`
	TimeoutSec     int            `json:"timeout_sec"`
	DyingThreshold int            `json:"dying_threshold"`
	AutoHeal       bool           `json:"auto_heal"`
	VMFailCounts   map[string]int `json:"vm_fail_counts"`
	VMDeadMarked   []string       `json:"vm_dead_marked"`
}

// Status returns a deep-copied snapshot of the watchdog's current configuration
// and per-VM health state under the same lock the polling loop uses, so a
// concurrent tick can never observe a half-built response. Safe to call any
// time after construction.
func (wd *Watchdog) Status() WatchdogStatus {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	failCounts := make(map[string]int, len(wd.failCount))
	for vmID, n := range wd.failCount {
		failCounts[vmID] = n
	}
	dead := make([]string, 0, len(wd.deadMarked))
	for vmID, marked := range wd.deadMarked {
		if marked {
			dead = append(dead, vmID)
		}
	}
	return WatchdogStatus{
		IntervalSec:    int(wd.interval.Seconds()),
		TimeoutSec:     int(wd.httpTimeout.Seconds()),
		DyingThreshold: wd.dyingThreshold,
		AutoHeal:       wd.autoHeal,
		VMFailCounts:   failCounts,
		VMDeadMarked:   dead,
	}
}
