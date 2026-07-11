package anvilmcp

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func replicationHosts(names ...string) []RuntimeHost {
	hosts := make([]RuntimeHost, 0, len(names))
	for _, n := range names {
		hosts = append(hosts, RuntimeHost{
			Name: n, Endpoint: "http://" + n + ".internal:8080",
			Healthy: true, AvailableVMs: 1,
			EgressPolicies: []EgressPolicy{EgressPolicyProfile},
		})
	}
	return hosts
}

func newReplicationRouter(t *testing.T, hosts []RuntimeHost, daemons map[string]*routerFakeDaemon) (*RuntimeRouter, *PlacementStore) {
	t.Helper()
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	dm := make(map[string]Daemon, len(daemons))
	for name, d := range daemons {
		dm[name] = d
	}
	router := NewRuntimeRouterWithOptions(NewScheduler(hosts, nil, nil), dm, RuntimeRouterOptions{PlacementStore: store})
	return router, store
}

func allReachable(names ...string) map[string]hostProbe {
	probes := make(map[string]hostProbe, len(names))
	for _, n := range names {
		probes[n] = hostProbe{reachable: true}
	}
	return probes
}

func snapInfo(id string) SnapshotInfo {
	return SnapshotInfo{SnapshotID: id, TenantID: "tenant-1", EgressPolicy: "profile", SnapshotType: "full"}
}

// TestSnapshotReplication_ReplicatesUnderReplicatedSnapshot is the core contract:
// a snapshot present on only one reachable host is replicated to one eligible
// target, its location is recorded, queue_depth reflects the pre-heal drift, and
// a replicated attempt is counted.
func TestSnapshotReplication_ReplicatesUnderReplicatedSnapshot(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})

	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB")); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	if len(hostB.importCalls) != 1 {
		t.Fatalf("target imports = %d, want 1", len(hostB.importCalls))
	}
	if hosts := store.SnapshotHosts("snap-1"); len(hosts) != 2 || !slices.Contains(hosts, "hostB") {
		t.Fatalf("SnapshotHosts(snap-1) = %v, want [hostA hostB]", hosts)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled)] != 1 {
		t.Fatalf("replicated attempt not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if m.QueueDepth != 1 {
		t.Fatalf("queue_depth gauge = %d, want 1", m.QueueDepth)
	}
}

// TestSnapshotReplication_GivesUpAfterDialFailureCap: a target that is Healthy in
// inventory but unreachable at probe time advances the dial counter each pass and
// hits giving-up exactly at the cap; K-1 passes never give up and never attempt an
// import toward the down target (probe short-circuit).
func TestSnapshotReplication_GivesUpAfterDialFailureCap(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}

	for i := 0; i < snapshotReplicationFailureCap-1; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 0 {
		t.Fatalf("gave up before cap: giving_up=%d", store.State().SnapshotReplicationMetrics.GivingUp)
	}
	if len(hostB.importCalls) != 0 {
		t.Fatalf("import attempted toward an unreachable target: %d", len(hostB.importCalls))
	}
	_ = router.reconcileSnapshotReplication(context.Background(), down) // cap-th failure
	m := store.State().SnapshotReplicationMetrics
	if m.GivingUp != 1 {
		t.Fatalf("giving_up gauge = %d, want 1 after %d dial failures", m.GivingUp, snapshotReplicationFailureCap)
	}
	if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable)]; got != int64(snapshotReplicationFailureCap) {
		t.Fatalf("dial_failed attempts = %d, want %d", got, snapshotReplicationFailureCap)
	}
}

// TestSnapshotReplication_DialCounterResetsWhenTargetRevives: once the target is
// observed reachable again the saturated counter resets and the snapshot is
// replicated (giving-up is not permanent — spec D-2 보강).
func TestSnapshotReplication_DialCounterResetsWhenTargetRevives(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}
	for i := 0; i < snapshotReplicationFailureCap; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 1 {
		t.Fatal("precondition: sweep did not give up")
	}

	_ = router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB"))
	if len(hostB.importCalls) != 1 {
		t.Fatalf("revived target did not receive the retried replication: imports=%d", len(hostB.importCalls))
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 0 {
		t.Fatalf("giving_up gauge did not reset after revival: %d", store.State().SnapshotReplicationMetrics.GivingUp)
	}
}

// TestSnapshotReplication_NoCandidateSurfacesQueueDepth: a single-host cluster
// (no eligible target) is a no-op — surfaced via queue_depth + a no_candidate
// attempt, never an export.
func TestSnapshotReplication_NoCandidateSurfacesQueueDepth(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	router, store := newReplicationRouter(t, replicationHosts("hostA"),
		map[string]*routerFakeDaemon{"hostA": hostA})
	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA")); err != nil {
		t.Fatalf("no-candidate sweep should be a no-op, got error: %v", err)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.QueueDepth != 1 {
		t.Fatalf("queue_depth = %d, want 1", m.QueueDepth)
	}
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeNoCandidate, SnapshotReplicationReasonNoEligibleHost)] != 1 {
		t.Fatalf("no_candidate not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if len(hostA.exportCalls) != 0 {
		t.Fatalf("export attempted with no candidate: %d", len(hostA.exportCalls))
	}
}

// TestSnapshotReplication_RestartResetsInMemoryCounters: a fresh router over the
// same store starts with empty in-memory counters, so a previously given-up
// target is retried once reachable (spec 경계: 재시작 후 수렴, in-memory 카운터만
// 리셋).
func TestSnapshotReplication_RestartResetsInMemoryCounters(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	hosts := replicationHosts("hostA", "hostB")
	router, store := newReplicationRouter(t, hosts,
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}
	for i := 0; i < snapshotReplicationFailureCap; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}

	restarted := NewRuntimeRouterWithOptions(NewScheduler(hosts, nil, nil),
		map[string]Daemon{"hostA": hostA, "hostB": hostB},
		RuntimeRouterOptions{PlacementStore: store})
	_ = restarted.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB"))
	if len(hostB.importCalls) != 1 {
		t.Fatalf("restart did not re-attempt a previously given-up target: imports=%d", len(hostB.importCalls))
	}
}

// TestSnapshotReplication_RespectsHostEligibility: target selection reuses
// SelectRuntimeHost eligibility — a SmokeOnly host is skipped (not preferred), the
// eligible host receives the replica. Proves tenant/egress-aware selection.
func TestSnapshotReplication_RespectsHostEligibility(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	hostC := &routerFakeDaemon{}
	hosts := replicationHosts("hostA", "hostB", "hostC")
	hosts[1].SmokeOnly = true // hostB ineligible unless preferred
	router, _ := newReplicationRouter(t, hosts,
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB, "hostC": hostC})
	_ = router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB", "hostC"))
	if len(hostB.importCalls) != 0 {
		t.Fatalf("replicated to an ineligible SmokeOnly host: %d", len(hostB.importCalls))
	}
	if len(hostC.importCalls) != 1 {
		t.Fatalf("did not replicate to the eligible host: hostC imports=%d", len(hostC.importCalls))
	}
}
