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
//  5. Sweeps in-memory counters for snapshots no longer observed (deleted).
//
// All log/error text carries snapshot id + host NAME only (never a daemon address
// or token).
func (r *RuntimeRouter) reconcileSnapshotReplication(ctx context.Context, probes map[string]hostProbe) error {
	if r == nil || r.placementStore == nil || r.scheduler == nil {
		return nil
	}
	var errs []error

	// 1. Discover.
	liveSnapshots := make(map[string]bool)
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
			liveSnapshots[id] = true
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

	// 5. Sweep counters for snapshots no longer observed (deleted/removed).
	for key := range r.replicationDialFailures {
		if s, _ := splitSnapshotTargetKey(key); !liveSnapshots[s] {
			delete(r.replicationDialFailures, key)
		}
	}
	for key := range r.replicationGivingUp {
		if s, _ := splitSnapshotTargetKey(key); !liveSnapshots[s] {
			delete(r.replicationGivingUp, key)
		}
	}
	return errors.Join(errs...)
}

// excludedReplicationTargets lists the in-memory targets to keep out of selection
// for a snapshot. Task 2: giving-up targets only. (Task 3 also adds terminal
// targets.)
func (r *RuntimeRouter) excludedReplicationTargets(snapshotID string) []string {
	var out []string
	for key := range r.replicationGivingUp {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	return out
}

// classifySnapshotReplication records the metric outcome for one replication
// attempt and updates the dial counter. Task 2: success (reset) vs error
// (collected, retried next pass). Task 3 splits terminal out of error.
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
	// Reachable target but the transfer did not succeed. Task 2 collects it as a
	// generic error and retries next pass (Task 3 refines the non-error case into a
	// terminal exclusion).
	r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
		Outcome:   SnapshotReplicationOutcomeError,
		Reason:    SnapshotReplicationReasonTransferError,
		Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
		At:        at,
	})
	return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q failed", id, target))
}

func (r *RuntimeRouter) recordSnapshotReplicationMetric(obs SnapshotReplicationMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordSnapshotReplicationMetrics(obs)
}
