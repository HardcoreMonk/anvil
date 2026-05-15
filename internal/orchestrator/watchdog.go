package orchestrator

import (
	"context"
	"fmt"
	"log"
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
// Wall. A revived VM is not auto-marked back to ready — operators decide
// when to clear the dead state (typically by deleting and respawning).
type Watchdog struct {
	interval       time.Duration
	httpTimeout    time.Duration
	dyingThreshold int
	agentPort      int

	flockMgr *FlockManager
	locator  AgentLocator
	lister   VMLister
	client   *http.Client

	mu         sync.Mutex
	failCount  map[string]int
	deadMarked map[string]bool

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewWatchdog wires the watchdog with all dependencies it needs from the
// control plane. Tunables (interval, httpTimeout, dyingThreshold) can be
// overridden on the returned struct before Start is called — useful for tests
// that need a faster cadence.
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

// onSuccess clears any accumulated fail count. deadMarked is preserved on
// purpose — a flapping VM that revived briefly should not be auto-cleared.
func (wd *Watchdog) onSuccess(vmID string) {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	if wd.failCount[vmID] > 0 {
		delete(wd.failCount, vmID)
	}
}

// onFailure increments the fail counter and, on the threshold transition,
// updates the agent status and posts a Town Wall notice exactly once.
func (wd *Watchdog) onFailure(v VMRef) {
	wd.mu.Lock()
	wd.failCount[v.VMID]++
	count := wd.failCount[v.VMID]
	alreadyMarked := wd.deadMarked[v.VMID]
	wd.mu.Unlock()

	if alreadyMarked || count < wd.dyingThreshold {
		return
	}

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
	flock.UpdateAgentStatus(agentID, AgentStatusDead)
	if _, err := flock.TownWall.Post(
		"orchestrator",
		fmt.Sprintf("%s unresponsive after %d health probes - marked dead",
			agentID, wd.dyingThreshold),
	); err != nil {
		log.Printf("Watchdog: failed to post dead notice for %s: %v", agentID, err)
	}

	wd.mu.Lock()
	wd.deadMarked[v.VMID] = true
	wd.mu.Unlock()
}
