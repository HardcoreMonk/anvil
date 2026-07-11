package anvilmcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// TestSnapshotReplication_TerminalRejectionIsNotRetried: a reachable target that
// EXPLICITLY refuses the import — an HTTP 4xx DaemonError from
// POST /snapshots/import, the shape ImportSnapshot always returns for a real
// daemon-side rejection (invalid bundle / diff base missing on the target /
// conflicting snapshot / D3-equivalent validation) — is terminal: recorded
// once, excluded from re-selection, never retried against the same target,
// and never counted toward the dial cap (Follow-Up 0: only this explicit-4xx
// signal is terminal-worthy; see TestSnapshotReplication_AmbiguousImportFailureIsNotTerminal
// for the conservative non-terminal counterpart).
func TestSnapshotReplication_TerminalRejectionIsNotRetried(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{importErrForBody: map[string]error{
		"bundle:snap-1": &DaemonError{StatusCode: 409, Body: "diff_base_missing"},
	}}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	probes := allReachable("hostA", "hostB")

	_ = router.reconcileSnapshotReplication(context.Background(), probes) // first attempt → terminal
	if len(hostB.importCalls) != 1 {
		t.Fatalf("first pass import calls = %d, want 1", len(hostB.importCalls))
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeTerminalRejected, SnapshotReplicationReasonRejected)] != 1 {
		t.Fatalf("terminal_rejected not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable)]; got != 0 {
		t.Fatalf("terminal failure wrongly counted as dial: %d", got)
	}

	_ = router.reconcileSnapshotReplication(context.Background(), probes) // second pass → excluded, not retried
	if len(hostB.importCalls) != 1 {
		t.Fatalf("terminal target retried: import calls = %d, want still 1", len(hostB.importCalls))
	}
}

// TestSnapshotReplication_ExportFailureDoesNotTerminalTarget pins Follow-Up
// 0's core bug fix: ReplicateSnapshot returns the SAME (resp, nil)
// non-"replicated" shape for a SOURCE-side export failure as for a genuine
// target refusal. The old classifier treated every such response as a target
// rejection, so a transient source problem could terminal-mark an innocent
// target for the process lifetime — and, swept pass after pass, poison every
// candidate target in turn until the snapshot stalled at no_candidate. The
// target must stay eligible and, once the source recovers, receive the very
// same replica on the next pass.
func TestSnapshotReplication_ExportFailureDoesNotTerminalTarget(t *testing.T) {
	hostA := &routerFakeDaemon{
		snapshotList: []SnapshotInfo{snapInfo("snap-1")},
		exportErr:    errors.New("simulated source export failure"),
	}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	probes := allReachable("hostA", "hostB")

	_ = router.reconcileSnapshotReplication(context.Background(), probes) // pass 1: export fails
	if len(hostB.importCalls) != 0 {
		t.Fatalf("import attempted toward target despite a source export failure: %d", len(hostB.importCalls))
	}
	key := snapshotTargetKey("snap-1", "hostB")
	if router.replicationTerminal[key] {
		t.Fatal("source-side export failure wrongly terminal-marked the target")
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeError, SnapshotReplicationReasonExportFailed)] != 1 {
		t.Fatalf("export_failed not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeTerminalRejected, SnapshotReplicationReasonRejected)]; got != 0 {
		t.Fatalf("source export failure wrongly counted as terminal_rejected: %d", got)
	}

	hostA.exportErr = nil                                                 // source problem resolved
	_ = router.reconcileSnapshotReplication(context.Background(), probes) // pass 2: same target reselected
	if len(hostB.importCalls) != 1 {
		t.Fatalf("previously-poisoned target was not reselected/retried after the source recovered: imports=%d", len(hostB.importCalls))
	}
	if hosts := store.SnapshotHosts("snap-1"); !slices.Contains(hosts, "hostB") {
		t.Fatalf("SnapshotHosts(snap-1) = %v, want hostB present after the retry succeeded", hosts)
	}
}

// TestSnapshotReplication_AmbiguousImportFailureIsNotTerminal: a target-side
// import failure that is NOT a genuine content rejection is conservatively
// retryable, never terminal ("import-측 5xx/불명도 retryable error로 —
// terminal은 확실한 거부만", Follow-Up 0). The same target is retried and
// succeeds once its failure clears. Covers, alongside the plain 5xx case,
// two concrete 4xx cases a reviewer identified as reachable in production
// and NOT a content rejection despite being 4xx (isExplicitImportRejection
// only treats {400,409} as an explicit rejection — see its doc):
//   - 401: a token expiring/rotating mid-pass, after ListSnapshots already
//     cleared auth but before ImportSnapshot fires.
//   - 404: an older target daemon predating the /snapshots/import route
//     (version skew) — resolves once the target upgrades, but would stick
//     as a phantom terminal exclude until the adapter restarts if
//     mis-treated as a rejection.
func TestSnapshotReplication_AmbiguousImportFailureIsNotTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"internal_server_error_5xx", &DaemonError{StatusCode: 500, Body: "snapshot_import_failed"}},
		{"unauthorized_401_token_rotation_race", &DaemonError{StatusCode: 401, Body: "unauthorized"}},
		{"not_found_404_version_skew", &DaemonError{StatusCode: 404, Body: "not found"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
			hostB := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-1": tc.err}}
			router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
				map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
			probes := allReachable("hostA", "hostB")

			_ = router.reconcileSnapshotReplication(context.Background(), probes) // pass 1: import fails
			key := snapshotTargetKey("snap-1", "hostB")
			if router.replicationTerminal[key] {
				t.Fatalf("ambiguous import failure (%s) wrongly terminal-marked the target", tc.name)
			}
			m := store.State().SnapshotReplicationMetrics
			if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeError, SnapshotReplicationReasonImportFailed)] != 1 {
				t.Fatalf("import_failed not recorded: %+v", m.AttemptsByOutcomeReason)
			}
			if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeTerminalRejected, SnapshotReplicationReasonRejected)]; got != 0 {
				t.Fatalf("ambiguous import failure (%s) wrongly counted as terminal_rejected: %d", tc.name, got)
			}

			hostB.importErrForBody = nil                                          // target-side failure resolved
			_ = router.reconcileSnapshotReplication(context.Background(), probes) // pass 2: same target retried, not excluded
			if len(hostB.importCalls) != 2 {                                      // 1 failed attempt + 1 successful retry
				t.Fatalf("target was not retried after its failure cleared: import calls = %d, want 2", len(hostB.importCalls))
			}
			if hosts := store.SnapshotHosts("snap-1"); !slices.Contains(hosts, "hostB") {
				t.Fatalf("SnapshotHosts(snap-1) = %v, want hostB present after the retry succeeded", hosts)
			}
		})
	}
}

// TestReconcilePlacements_RunsSnapshotReplicationSweep proves the sweep is wired
// into ReconcilePlacements (probes built from ListVMs) and that its logs leak no
// daemon address.
func TestReconcilePlacements_RunsSnapshotReplicationSweep(t *testing.T) {
	hostA := &routerFakeDaemon{
		snapshotList: []SnapshotInfo{snapInfo("snap-1")},
		listVMResp:   []VMInfo{{VMID: "vm-a"}},
	}
	hostB := &routerFakeDaemon{listVMResp: []VMInfo{}}
	router, _ := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	var logs []string
	router.reconcileLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	if err := router.ReconcilePlacements(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if len(hostB.importCalls) != 1 {
		t.Fatal("snapshot replication sweep did not run within ReconcilePlacements")
	}
	joined := strings.Join(logs, "\n")
	for _, forbidden := range []string{"hostA.internal:8080", "hostB.internal:8080"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sweep log leaked a daemon address: %s", joined)
		}
	}
}

// TestSnapshotReplication_CounterGCRequiresPositiveEvidence: step 5's counter GC
// must not fire on mere "not observed this pass" — that conflates a snapshot's
// genuine deletion with its recorded hosts being transiently unreachable
// together, which would revive a saturated giving-up mark under source flapping
// and stall convergence (Task 2 review). GC fires only when a recorded location
// (SnapshotHosts) was reachable this pass and did not list the snapshot
// (positive evidence of deletion); if every recorded host was unreachable this
// pass, the counters are left untouched (uncertainty preserved).
func TestSnapshotReplication_CounterGCRequiresPositiveEvidence(t *testing.T) {
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
	key := snapshotTargetKey("snap-1", "hostB")
	if !router.replicationGivingUp[key] {
		t.Fatal("precondition: giving-up map missing key")
	}

	// Every recorded host of snap-1 (just hostA — it never replicated to hostB)
	// goes unreachable together with hostB: no recorded host was observed
	// reachable this pass, so there is no positive evidence either way. GC must
	// hold.
	allDown := map[string]hostProbe{"hostA": {dialFailed: true}, "hostB": {dialFailed: true}}
	_ = router.reconcileSnapshotReplication(context.Background(), allDown)
	if !router.replicationGivingUp[key] {
		t.Fatal("giving-up mark GC'd with no positive evidence (all recorded hosts unreachable this pass)")
	}
	if router.replicationDialFailures[key] < snapshotReplicationFailureCap {
		t.Fatalf("dial counter reset with no positive evidence: %d", router.replicationDialFailures[key])
	}

	// snap-1 is genuinely deleted: hostA (its only recorded host) is reachable
	// again this pass and no longer lists it — positive evidence. GC must fire.
	hostA.snapshotList = nil
	stillDown := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}
	_ = router.reconcileSnapshotReplication(context.Background(), stillDown)
	if router.replicationGivingUp[key] {
		t.Fatal("giving-up mark survived genuine deletion with positive evidence")
	}
	if _, ok := router.replicationDialFailures[key]; ok {
		t.Fatal("dial counter survived genuine deletion with positive evidence")
	}
}

// TestClassifySnapshotReplication_AlreadyPresentDeletesCounters calls
// classifySnapshotReplication directly (no sweep plumbing) to pin the
// already_present/idempotent branch: every transferred item was already on the
// target (Replicated empty, Skipped non-empty), and the dial/giving-up counters
// for the pair are cleared exactly like a full "replicated" success.
func TestClassifySnapshotReplication_AlreadyPresentDeletesCounters(t *testing.T) {
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"), nil)
	key := snapshotTargetKey("snap-1", "hostB")
	router.replicationDialFailures[key] = 2
	router.replicationGivingUp[key] = true

	resp := &SnapshotReplicationResponse{
		SnapshotID: "snap-1",
		TargetHost: "hostB",
		Status:     "replicated",
		Skipped:    []string{"snap-1"},
	}
	errs := router.classifySnapshotReplication("snap-1", "hostB", key, resp, nil, time.Millisecond, time.Now().UTC(), nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %v", errs)
	}
	if _, ok := router.replicationDialFailures[key]; ok {
		t.Fatal("dial counter survived an already_present classification")
	}
	if router.replicationGivingUp[key] {
		t.Fatal("giving-up mark survived an already_present classification")
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeAlreadyPresent, SnapshotReplicationReasonIdempotent)] != 1 {
		t.Fatalf("already_present not recorded: %+v", m.AttemptsByOutcomeReason)
	}
}
