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
