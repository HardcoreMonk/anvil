package anvilmcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// snapshotReplicaFactor is the desired number of replicas per snapshot (원본 +
// 복제 1). Constant, not configuration (YAGNI — same stance as homeFailureThreshold).
const snapshotReplicaFactor = 2

// snapshotReplicationFailureCap is the number of CONSECUTIVE dial-class target
// failures before the sweep gives up on that (snapshot,target) pair. Constant.
const snapshotReplicationFailureCap = 3

func snapshotTargetKey(snapshotID, target string) string {
	return strings.TrimSpace(snapshotID) + "\x1f" + strings.TrimSpace(target)
}

func splitSnapshotTargetKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x1f", 2)
	if len(parts) != 2 {
		return key, ""
	}
	return parts[0], parts[1]
}

type SnapshotReplicationRequest struct {
	SnapshotID          string `json:"snapshot_id"`
	SourceHost          string `json:"source_host"`
	TargetHost          string `json:"target_host"`
	IncludeDependencies bool   `json:"include_dependencies"`
}

type SnapshotReplicationResponse struct {
	SnapshotID string   `json:"snapshot_id"`
	SourceHost string   `json:"source_host"`
	TargetHost string   `json:"target_host"`
	Status     string   `json:"status"`
	Replicated []string `json:"replicated"`
	Skipped    []string `json:"skipped"`
	Errors     []string `json:"errors"`
}

type snapshotTransferDaemon interface {
	ListSnapshots(context.Context) ([]SnapshotInfo, error)
	ExportSnapshot(context.Context, string) (*SnapshotExportStream, error)
	ImportSnapshot(context.Context, io.Reader) (*SnapshotImportResponse, error)
}

func (r *RuntimeRouter) ReplicateSnapshot(ctx context.Context, req SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	snapshotID := strings.TrimSpace(req.SnapshotID)
	sourceHostName := strings.TrimSpace(req.SourceHost)
	targetHostName := strings.TrimSpace(req.TargetHost)

	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	if sourceHostName == "" {
		return nil, fmt.Errorf("source_host is required")
	}
	if targetHostName == "" {
		return nil, fmt.Errorf("target_host is required")
	}
	if sourceHostName == targetHostName {
		return nil, fmt.Errorf("same_host")
	}
	if r == nil || r.scheduler == nil {
		return nil, fmt.Errorf("runtime router scheduler is nil")
	}

	sourceHost, ok := r.scheduler.RuntimeHost(sourceHostName)
	if !ok {
		return nil, fmt.Errorf("source_host_not_found")
	}
	targetHost, ok := r.scheduler.RuntimeHost(targetHostName)
	if !ok {
		return nil, fmt.Errorf("target_host_not_found")
	}
	if !sourceHost.Healthy {
		return nil, fmt.Errorf("source_host_unavailable")
	}
	if !targetHost.Healthy {
		return nil, fmt.Errorf("target_host_unavailable")
	}

	source, ok := r.daemons[sourceHostName].(snapshotTransferDaemon)
	if !ok || source == nil {
		return nil, fmt.Errorf("runtime host %q does not support snapshot replication", sourceHostName)
	}
	target, ok := r.daemons[targetHostName].(snapshotTransferDaemon)
	if !ok || target == nil {
		return nil, fmt.Errorf("runtime host %q does not support snapshot replication", targetHostName)
	}

	resp := &SnapshotReplicationResponse{
		SnapshotID: snapshotID,
		SourceHost: sourceHostName,
		TargetHost: targetHostName,
	}

	sourceSnapshots, err := source.ListSnapshots(ctx)
	if err != nil {
		return nil, errors.New(safeReplicationError("source_list_failed", "", "source", sourceHostName, err))
	}
	targetSnapshots, err := target.ListSnapshots(ctx)
	if err != nil {
		return nil, errors.New(safeReplicationError("target_list_failed", "", "target", targetHostName, err))
	}

	requested, ok := snapshotInfoByID(sourceSnapshots, snapshotID)
	if !ok {
		return nil, fmt.Errorf("snapshot_not_found")
	}

	targetSnapshotIDs := make(map[string]bool, len(targetSnapshots))
	for _, snapshot := range targetSnapshots {
		id := strings.TrimSpace(snapshot.SnapshotID)
		if id != "" {
			targetSnapshotIDs[id] = true
		}
	}

	var transferOrder []SnapshotInfo
	if isDiffSnapshot(requested) {
		baseSnapshotID := strings.TrimSpace(requested.BaseSnapshotID)
		if !req.IncludeDependencies {
			if baseSnapshotID == "" || !targetSnapshotIDs[baseSnapshotID] {
				resp.Status = "failed"
				resp.Errors = append(resp.Errors, "diff_base_missing")
				return resp, nil
			}
		} else {
			base, ok := snapshotInfoByID(sourceSnapshots, baseSnapshotID)
			if baseSnapshotID == "" || !ok {
				resp.Status = "failed"
				resp.Errors = append(resp.Errors, "diff_base_missing")
				return resp, nil
			}
			transferOrder = append(transferOrder, base)
		}
	}
	transferOrder = append(transferOrder, requested)

	for _, snapshot := range transferOrder {
		id := strings.TrimSpace(snapshot.SnapshotID)
		if id == "" {
			continue
		}

		stream, err := source.ExportSnapshot(ctx, id)
		if err != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, safeReplicationError("export_failed", id, "source", sourceHostName, err))
			return resp, nil
		}
		if stream == nil || stream.Body == nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, safeReplicationError("export_failed", id, "source", sourceHostName, nil))
			return resp, nil
		}

		importResp, importErr := target.ImportSnapshot(ctx, stream.Body)
		closeErr := stream.Body.Close()
		if importErr != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, safeReplicationError("import_failed", id, "target", targetHostName, importErr))
			return resp, nil
		}
		if closeErr != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, safeReplicationError("close_export_stream_failed", id, "source", sourceHostName, closeErr))
			return resp, nil
		}

		targetSnapshotIDs[id] = true
		if importResp != nil && importResp.Status == "already_present" {
			resp.Skipped = append(resp.Skipped, id)
		} else {
			resp.Replicated = append(resp.Replicated, id)
		}
		if err := r.recordSnapshotLocation(id, targetHostName); err != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, safeReplicationError("record_location_failed", id, "target", targetHostName, err))
			return resp, nil
		}
	}

	resp.Status = "replicated"
	return resp, nil
}

func (r *RuntimeRouter) recordSnapshotLocation(snapshotID, targetHost string) error {
	if r.placementStore == nil {
		return nil
	}
	if err := r.placementStore.SetSnapshotLocation(snapshotID, targetHost); err != nil {
		return err
	}
	if err := r.placementStore.Save(); err != nil {
		return err
	}
	return nil
}

func statusForFailure(resp *SnapshotReplicationResponse) string {
	if resp != nil && (len(resp.Replicated) > 0 || len(resp.Skipped) > 0) {
		return "partial"
	}
	return "failed"
}

func safeReplicationError(operation, snapshotID, hostRole, hostName string, err error) string {
	parts := []string{operation}
	if snapshotID = strings.TrimSpace(snapshotID); snapshotID != "" {
		parts = append(parts, "snapshot_id="+snapshotID)
	}
	if hostRole = strings.TrimSpace(hostRole); hostRole != "" {
		parts = append(parts, "host_role="+hostRole)
	}
	if hostName = strings.TrimSpace(hostName); hostName != "" {
		parts = append(parts, "host="+hostName)
	}
	var daemonErr *DaemonError
	if errors.As(err, &daemonErr) {
		parts = append(parts, fmt.Sprintf("status_code=%d", daemonErr.StatusCode))
	}
	return strings.Join(parts, " ")
}

func snapshotInfoByID(snapshots []SnapshotInfo, snapshotID string) (SnapshotInfo, bool) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return SnapshotInfo{}, false
	}
	for _, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.SnapshotID) == snapshotID {
			return snapshot, true
		}
	}
	return SnapshotInfo{}, false
}

func isDiffSnapshot(snapshot SnapshotInfo) bool {
	return strings.EqualFold(strings.TrimSpace(snapshot.SnapshotType), "diff")
}

// reconcileSnapshotReplication converges every observed snapshot toward
// snapshotReplicaFactor replicas. It is a sibling of reconcileRoutedFlockWalls and
// runs inside ReconcilePlacements (reconcileMu-serialized), so its in-memory
// counters need no extra lock. Each pass:
//  1. Discovers actual replica locations + snapshot tenant/egress from every
//     probe-reachable daemon's ListSnapshots (add-only union into SnapshotLocations
//     — replica GC is a non-goal).
//  2. Resets the dial counter + giving-up mark for any target observed reachable
//     again (spec D-2 보강: giving-up is not permanent).
//  3. For each snapshot below the replica factor with a reachable source: selects
//     an eligible target (SelectRuntimeHost, tenant/egress carried), and either
//     counts a dial failure (target unreachable at probe — short-circuit, no doomed
//     transfer) or fires one ReplicateSnapshot and classifies the outcome.
//  4. Republishes the queue_depth / giving_up gauges.
//  5. Sweeps the dial-failure, giving-up, and terminal counters using a
//     positive-evidence deletion rule: a (snapshot,target) pair's counters are
//     GC'd only when the snapshot's recorded locations (SnapshotHosts) include
//     a host that was probe-reachable THIS PASS and did not list the snapshot
//     in its ListSnapshots — deletion positively observed, not merely absent
//     from this pass's union of listings. If every recorded host was
//     unreachable this pass, the pass is uninformative and the counters are
//     left untouched (Task 2 review: a naive "not observed this pass" rule
//     wipes a saturated giving-up mark whenever a snapshot's recorded hosts go
//     unreachable together, reviving dead-target reselection and stalling
//     convergence under source flapping).
//
// All log/error text carries snapshot id + host NAME only (never a daemon address
// or token).
func (r *RuntimeRouter) reconcileSnapshotReplication(ctx context.Context, probes map[string]hostProbe) error {
	if r == nil || r.placementStore == nil || r.scheduler == nil {
		return nil
	}
	var errs []error

	// 1. Discover.
	observedHosts := make(map[string]map[string]bool)
	infoByID := make(map[string]SnapshotInfo)
	discovered := false
	for hostName, daemon := range r.daemons {
		if daemon == nil || !probes[hostName].reachable {
			continue
		}
		lister, ok := daemon.(snapshotTransferDaemon)
		if !ok {
			continue
		}
		snaps, err := lister.ListSnapshots(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile snapshot replication: list snapshots on runtime host %q failed", hostName))
			continue
		}
		for _, s := range snaps {
			id := strings.TrimSpace(s.SnapshotID)
			if id == "" {
				continue
			}
			if observedHosts[id] == nil {
				observedHosts[id] = make(map[string]bool)
			}
			observedHosts[id][hostName] = true
			if _, seen := infoByID[id]; !seen {
				infoByID[id] = s
			}
			if err := r.placementStore.SetSnapshotLocation(id, hostName); err == nil {
				discovered = true
			}
		}
	}
	if discovered {
		if err := r.placementStore.Save(); err != nil {
			errs = append(errs, fmt.Errorf("reconcile snapshot replication: persisting discovered locations failed"))
		}
	}

	// 2. Revival reset.
	for key := range r.replicationDialFailures {
		if _, target := splitSnapshotTargetKey(key); probes[target].reachable {
			delete(r.replicationDialFailures, key)
			delete(r.replicationGivingUp, key)
		}
	}

	// 3. Heal drift.
	locations := r.placementStore.State().SnapshotLocations
	ids := make([]string, 0, len(locations))
	for id := range locations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now().UTC()
	var queueDepth int64
	for _, id := range ids {
		hosts := locations[id] // sorted by SetSnapshotLocation
		if len(hosts) >= snapshotReplicaFactor {
			continue
		}
		queueDepth++
		source := ""
		for _, h := range hosts {
			if probes[h].reachable && observedHosts[id][h] {
				source = h
				break
			}
		}
		if source == "" {
			continue // source unreachable/absent this pass; still counted in queue_depth
		}
		info := infoByID[id]
		excluded := append([]string(nil), hosts...)
		excluded = append(excluded, r.excludedReplicationTargets(id)...)
		target, err := SelectRuntimeHost(r.scheduler.hosts, ScheduleRequest{
			TenantID:      info.TenantID,
			EgressPolicy:  EgressPolicy(info.EgressPolicy),
			ExcludedHosts: excluded,
		})
		if err != nil {
			r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
				Outcome: SnapshotReplicationOutcomeNoCandidate,
				Reason:  SnapshotReplicationReasonNoEligibleHost,
				At:      now,
			})
			continue
		}
		tgtName := strings.TrimSpace(target.Name)
		key := snapshotTargetKey(id, tgtName)
		if !probes[tgtName].reachable {
			r.replicationDialFailures[key]++
			r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
				Outcome: SnapshotReplicationOutcomeDialFailed,
				Reason:  SnapshotReplicationReasonTargetUnreachable,
				At:      now,
			})
			if r.replicationDialFailures[key] >= snapshotReplicationFailureCap {
				r.replicationGivingUp[key] = true
				r.logf("anvil-mcp: snapshot replication for %q giving up on target host %q after %d dial failures", id, tgtName, r.replicationDialFailures[key])
			}
			errs = append(errs, fmt.Errorf("reconcile snapshot replication for %q: target host %q unreachable", id, tgtName))
			continue
		}
		start := time.Now()
		resp, repErr := r.ReplicateSnapshot(ctx, SnapshotReplicationRequest{
			SnapshotID:          id,
			SourceHost:          source,
			TargetHost:          tgtName,
			IncludeDependencies: true,
		})
		errs = r.classifySnapshotReplication(id, tgtName, key, resp, repErr, time.Since(start), now, errs)
	}

	// 4. Gauges.
	givingUpSnaps := make(map[string]bool)
	for key := range r.replicationGivingUp {
		if s, _ := splitSnapshotTargetKey(key); s != "" {
			givingUpSnaps[s] = true
		}
	}
	if err := r.placementStore.RecordSnapshotReplicationGauges(queueDepth, int64(len(givingUpSnaps))); err != nil {
		errs = append(errs, fmt.Errorf("reconcile snapshot replication: recording gauges failed"))
	}

	// 5. Sweep counters using the positive-evidence deletion rule (see function
	// doc). snapshotDeletionConfirmed is the single source of truth so the three
	// counter maps can never disagree on whether a pair's evidence is in.
	for key := range r.replicationDialFailures {
		if s, _ := splitSnapshotTargetKey(key); r.snapshotDeletionConfirmed(s, probes, observedHosts) {
			delete(r.replicationDialFailures, key)
		}
	}
	for key := range r.replicationGivingUp {
		if s, _ := splitSnapshotTargetKey(key); r.snapshotDeletionConfirmed(s, probes, observedHosts) {
			delete(r.replicationGivingUp, key)
		}
	}
	for key := range r.replicationTerminal {
		if s, _ := splitSnapshotTargetKey(key); r.snapshotDeletionConfirmed(s, probes, observedHosts) {
			delete(r.replicationTerminal, key)
		}
	}
	return errors.Join(errs...)
}

// snapshotDeletionConfirmed reports whether this pass has positive evidence
// that snapshot id is genuinely gone, for step 5's counter GC (dial failures,
// giving-up, terminal). Evidence requires at least one of the snapshot's
// recorded locations (SnapshotHosts) to have been probe-reachable this pass —
// if none were reachable, the pass is uninformative and GC must hold (Task 2
// review: "not observed this pass" alone conflates genuine deletion with every
// recorded host being transiently unreachable together). Among the reachable
// recorded hosts, if none reported the snapshot in this pass's ListSnapshots
// (observedHosts), deletion is confirmed.
func (r *RuntimeRouter) snapshotDeletionConfirmed(id string, probes map[string]hostProbe, observedHosts map[string]map[string]bool) bool {
	sawReachableRecordedHost := false
	for _, host := range r.placementStore.SnapshotHosts(id) {
		if !probes[host].reachable {
			continue
		}
		sawReachableRecordedHost = true
		if observedHosts[id][host] {
			return false
		}
	}
	return sawReachableRecordedHost
}

// excludedReplicationTargets lists the in-memory targets to keep out of
// selection for a snapshot: dial-saturated giving-up targets (Task 2) and
// terminal-rejected targets (Task 3) — both reconcileMu-guarded, checked at
// selection time only.
func (r *RuntimeRouter) excludedReplicationTargets(snapshotID string) []string {
	var out []string
	for key := range r.replicationGivingUp {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	for key := range r.replicationTerminal {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	return out
}

// classifySnapshotReplication records the metric outcome for one replication
// attempt and updates the dial counter. Success clears the dial/giving-up
// counters for the pair. Reachable-target non-success splits into two cases —
// see the inline comment below.
func (r *RuntimeRouter) classifySnapshotReplication(id, target, key string, resp *SnapshotReplicationResponse, repErr error, total time.Duration, at time.Time, errs []error) []error {
	if resp != nil && repErr == nil && resp.Status == "replicated" {
		delete(r.replicationDialFailures, key)
		delete(r.replicationGivingUp, key)
		outcome, reason := SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled
		if len(resp.Replicated) == 0 { // every item was already present on the target
			outcome, reason = SnapshotReplicationOutcomeAlreadyPresent, SnapshotReplicationReasonIdempotent
		}
		r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
			Outcome:   outcome,
			Reason:    reason,
			Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
			At:        at,
		})
		r.logf("anvil-mcp: snapshot %q replicated to host %q (%d desired replicas)", id, target, snapshotReplicaFactor)
		return errs
	}
	// Reachable target, transfer did not succeed. Two cases:
	//  - repErr != nil: ReplicateSnapshot failed BEFORE the transfer completed
	//    (source/target ListSnapshots, transient). The target answered the probe,
	//    so this is not a dial failure; surface it and retry next pass. Do NOT
	//    exclude the target — it may well succeed next time.
	//  - (resp, nil) with a non-"replicated" status: the reachable target refused
	//    the content — D3 coarse-fs overlay refusal, tenant/validation mismatch, or
	//    a missing diff base. TERMINAL (spec D-6): retrying the same target is
	//    futile, so exclude it in-memory (reset only on restart), record a terminal
	//    reason, and never count it toward the dial cap.
	if repErr != nil {
		r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
			Outcome:   SnapshotReplicationOutcomeError,
			Reason:    SnapshotReplicationReasonTransferError,
			Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
			At:        at,
		})
		return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q failed", id, target))
	}
	r.replicationTerminal[key] = true
	r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
		Outcome:   SnapshotReplicationOutcomeTerminalRejected,
		Reason:    SnapshotReplicationReasonRejected,
		Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
		At:        at,
	})
	return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q rejected (terminal)", id, target))
}

func (r *RuntimeRouter) recordSnapshotReplicationMetric(obs SnapshotReplicationMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordSnapshotReplicationMetrics(obs)
}
