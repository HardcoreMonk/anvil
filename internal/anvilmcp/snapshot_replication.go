package anvilmcp

import (
	"context"
	"fmt"
	"io"
	"strings"
)

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
		return nil, fmt.Errorf("list snapshots on source host %q: %w", sourceHostName, err)
	}
	targetSnapshots, err := target.ListSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("list snapshots on target host %q: %w", targetHostName, err)
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
	if isDiffSnapshot(requested) && !targetSnapshotIDs[snapshotID] {
		baseSnapshotID := strings.TrimSpace(requested.BaseSnapshotID)
		if baseSnapshotID == "" || !targetSnapshotIDs[baseSnapshotID] {
			if !req.IncludeDependencies {
				resp.Status = "failed"
				resp.Errors = append(resp.Errors, "diff_base_missing")
				return resp, nil
			}
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
		if targetSnapshotIDs[id] {
			if err := r.recordSnapshotLocation(id, targetHostName); err != nil {
				resp.Status = statusForFailure(resp)
				resp.Errors = append(resp.Errors, fmt.Sprintf("record snapshot location %s on %s: %v", id, targetHostName, err))
				return resp, nil
			}
			resp.Skipped = append(resp.Skipped, id)
			continue
		}

		stream, err := source.ExportSnapshot(ctx, id)
		if err != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, fmt.Sprintf("export %s: %v", id, err))
			return resp, nil
		}
		if stream == nil || stream.Body == nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, fmt.Sprintf("export %s: empty snapshot stream", id))
			return resp, nil
		}

		importResp, importErr := target.ImportSnapshot(ctx, stream.Body)
		closeErr := stream.Body.Close()
		if importErr != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, fmt.Sprintf("import %s: %v", id, importErr))
			return resp, nil
		}
		if closeErr != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, fmt.Sprintf("close export stream %s: %v", id, closeErr))
			return resp, nil
		}

		targetSnapshotIDs[id] = true
		if err := r.recordSnapshotLocation(id, targetHostName); err != nil {
			resp.Status = statusForFailure(resp)
			resp.Errors = append(resp.Errors, fmt.Sprintf("record snapshot location %s on %s: %v", id, targetHostName, err))
			return resp, nil
		}
		if importResp != nil && importResp.Status == "already_present" {
			resp.Skipped = append(resp.Skipped, id)
		} else {
			resp.Replicated = append(resp.Replicated, id)
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
