package anvilmcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSetSnapshotLocationDoesNotDowngradeVerifiedToObserved is the core
// provenance contract: the discovery loop re-reports every peer's listing on
// every sweep, so without a downgrade guard a peer report would erase the fact
// that this adapter itself put the snapshot there.
func TestSetSnapshotLocationDoesNotDowngradeVerifiedToObserved(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SetSnapshotLocation("snap-1", "host-a", SnapshotLocationVerified); err != nil {
		t.Fatalf("SetSnapshotLocation(verified): %v", err)
	}
	if err := store.SetSnapshotLocation("snap-1", "host-a", SnapshotLocationObserved); err != nil {
		t.Fatalf("SetSnapshotLocation(observed): %v", err)
	}
	if got := store.SnapshotLocationProvenance("snap-1", "host-a"); got != SnapshotLocationVerified {
		t.Fatalf("provenance = %q, want %q (observed must never downgrade verified)", got, SnapshotLocationVerified)
	}
}

// TestSetSnapshotLocationUpgradesTowardVerified: unknown (absent) is upgraded by
// either provenance, and observed is upgraded by verified.
func TestSetSnapshotLocationUpgradesTowardVerified(t *testing.T) {
	store := NewPlacementStore("")
	if got := store.SnapshotLocationProvenance("snap-1", "host-a"); got != "" {
		t.Fatalf("untracked provenance = %q, want \"\" (unknown)", got)
	}
	if err := store.SetSnapshotLocation("snap-1", "host-a", SnapshotLocationObserved); err != nil {
		t.Fatalf("SetSnapshotLocation(observed): %v", err)
	}
	if got := store.SnapshotLocationProvenance("snap-1", "host-a"); got != SnapshotLocationObserved {
		t.Fatalf("unknown->observed: provenance = %q, want %q", got, SnapshotLocationObserved)
	}
	if err := store.SetSnapshotLocation("snap-1", "host-a", SnapshotLocationVerified); err != nil {
		t.Fatalf("SetSnapshotLocation(verified): %v", err)
	}
	if got := store.SnapshotLocationProvenance("snap-1", "host-a"); got != SnapshotLocationVerified {
		t.Fatalf("observed->verified: provenance = %q, want %q", got, SnapshotLocationVerified)
	}

	if err := store.SetSnapshotLocation("snap-2", "host-b", SnapshotLocationVerified); err != nil {
		t.Fatalf("SetSnapshotLocation(snap-2 verified): %v", err)
	}
	if got := store.SnapshotLocationProvenance("snap-2", "host-b"); got != SnapshotLocationVerified {
		t.Fatalf("unknown->verified: provenance = %q, want %q", got, SnapshotLocationVerified)
	}
}

// TestSnapshotLocationProvenancePersistsAndPrunes: the provenance map round-trips
// through the state file and is pruned to the recorded locations on decode, so a
// hand-edited or stale entry can neither grow the file without bound nor
// resurrect a host that is not a recorded location.
func TestSnapshotLocationProvenancePersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placements.json")
	store := NewPlacementStore(path)
	if err := store.SetSnapshotLocation("snap-1", "host-a", SnapshotLocationVerified); err != nil {
		t.Fatalf("SetSnapshotLocation: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.SnapshotLocationProvenance("snap-1", "host-a"); got != SnapshotLocationVerified {
		t.Fatalf("reloaded provenance = %q, want %q", got, SnapshotLocationVerified)
	}

	stalePath := filepath.Join(t.TempDir(), "stale.json")
	writeTestPlacementFile(t, stalePath, `{
  "hosts": {},
  "vm_placements": {},
  "snapshot_locations": {"snap-1": ["host-a"]},
  "snapshot_location_provenance": {
    "snap-1": {"host-a": "verified", "host-gone": "observed"},
    "snap-deleted": {"host-a": "observed"}
  }
}`)
	stale := NewPlacementStore(stalePath)
	if err := stale.Load(); err != nil {
		t.Fatalf("Load stale: %v", err)
	}
	state := stale.State()
	if got := state.SnapshotLocationProvenances["snap-1"]["host-a"]; got != SnapshotLocationVerified {
		t.Fatalf("kept provenance = %q, want %q", got, SnapshotLocationVerified)
	}
	if _, ok := state.SnapshotLocationProvenances["snap-1"]["host-gone"]; ok {
		t.Fatalf("provenance kept a host that is not a recorded location: %+v", state.SnapshotLocationProvenances)
	}
	if _, ok := state.SnapshotLocationProvenances["snap-deleted"]; ok {
		t.Fatalf("provenance kept a snapshot with no recorded locations: %+v", state.SnapshotLocationProvenances)
	}
}

// TestSnapshotReplication_PeerOnlyGaugeCountsPeerReportedSnapshot: a snapshot
// that reached the replica factor purely on peer ListSnapshots reports is
// counted, and queue_depth is unaffected.
func TestSnapshotReplication_PeerOnlyGaugeCountsPeerReportedSnapshot(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})

	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB")); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.PeerOnlySatisfied != 1 {
		t.Fatalf("peer_only_satisfied gauge = %d, want 1", m.PeerOnlySatisfied)
	}
	if m.QueueDepth != 0 {
		t.Fatalf("queue_depth gauge = %d, want 0 (snapshot is at the replica factor)", m.QueueDepth)
	}
}

// TestSnapshotReplication_PeerOnlyGaugeIgnoresVerifiedLocation: one
// adapter-verified location disqualifies the snapshot, and the verified mark
// survives the discovery loop's peer report in the same pass.
func TestSnapshotReplication_PeerOnlyGaugeIgnoresVerifiedLocation(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	if err := store.SetSnapshotLocation("snap-1", "hostA", SnapshotLocationVerified); err != nil {
		t.Fatalf("seed verified location: %v", err)
	}

	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB")); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	if got := store.SnapshotLocationProvenance("snap-1", "hostA"); got != SnapshotLocationVerified {
		t.Fatalf("discovery downgraded a verified location to %q", got)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.PeerOnlySatisfied != 0 {
		t.Fatalf("peer_only_satisfied gauge = %d, want 0 (one location is adapter-verified)", m.PeerOnlySatisfied)
	}
	if m.QueueDepth != 0 {
		t.Fatalf("queue_depth gauge = %d, want 0", m.QueueDepth)
	}
}

// TestSnapshotReplication_PeerOnlyGaugeIgnoresLegacyStateWithoutProvenance:
// locations written before provenance tracking existed decode as unknown -- a
// third state, not "observed" -- so upgrading an existing deployment must not
// produce a spike of peer-only alerts about data that was in fact fine.
func TestSnapshotReplication_PeerOnlyGaugeIgnoresLegacyStateWithoutProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	writeTestPlacementFile(t, path, `{
  "hosts": {},
  "vm_placements": {},
  "snapshot_locations": {"snap-legacy": ["hostA", "hostB"]}
}`)
	store := NewPlacementStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(replicationHosts("hostA", "hostB"), nil, nil),
		map[string]Daemon{"hostA": &routerFakeDaemon{}, "hostB": &routerFakeDaemon{}},
		RuntimeRouterOptions{PlacementStore: store},
	)

	// No host is probe-reachable this pass, so no discovery runs and every
	// recorded location stays unknown.
	if err := router.reconcileSnapshotReplication(context.Background(), map[string]hostProbe{}); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.PeerOnlySatisfied != 0 {
		t.Fatalf("peer_only_satisfied gauge = %d, want 0 (all-unknown legacy locations)", m.PeerOnlySatisfied)
	}
	if m.QueueDepth != 0 {
		t.Fatalf("queue_depth gauge = %d, want 0", m.QueueDepth)
	}
}

// TestSnapshotReplication_QueueDepthUnchangedByPeerOnlyGauge asserts the
// regression this change is most exposed to: queue_depth must still count only
// snapshots below the replica factor, independent of the new gauge.
func TestSnapshotReplication_QueueDepthUnchangedByPeerOnlyGauge(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-under"), snapInfo("snap-peer")}}
	hostB := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-peer")}}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})

	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB")); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.QueueDepth != 1 {
		t.Fatalf("queue_depth gauge = %d, want 1 (only snap-under is below the replica factor)", m.QueueDepth)
	}
	if m.PeerOnlySatisfied != 1 {
		t.Fatalf("peer_only_satisfied gauge = %d, want 1 (only snap-peer is peer-only satisfied)", m.PeerOnlySatisfied)
	}
}

// TestRenderSchedulerMetricsIncludesPeerOnlySatisfiedGauge pins the exposition
// name alongside the existing queue_depth / giving_up gauges.
func TestRenderSchedulerMetricsIncludesPeerOnlySatisfiedGauge(t *testing.T) {
	state := PlacementStoreState{
		SnapshotReplicationMetrics: SnapshotReplicationMetricsState{
			QueueDepth:        4,
			GivingUp:          1,
			PeerOnlySatisfied: 2,
		},
	}
	output := RenderSchedulerMetrics(state)
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_queue_depth 4")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_giving_up 1")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_peer_only_satisfied 2")
}

// TestPlacementStoreRecordsPeerOnlySatisfiedGauge: the new gauge persists with
// the other two.
func TestPlacementStoreRecordsPeerOnlySatisfiedGauge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placements.json")
	store := NewPlacementStore(path)
	if err := store.RecordSnapshotReplicationGauges(4, 1, 2); err != nil {
		t.Fatalf("RecordSnapshotReplicationGauges() error = %v", err)
	}
	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	state := reloaded.State().SnapshotReplicationMetrics
	if state.QueueDepth != 4 || state.GivingUp != 1 || state.PeerOnlySatisfied != 2 {
		t.Fatalf("gauges = queue_depth %d giving_up %d peer_only_satisfied %d, want 4/1/2",
			state.QueueDepth, state.GivingUp, state.PeerOnlySatisfied)
	}
}

func writeTestPlacementFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write placement file: %v", err)
	}
}
