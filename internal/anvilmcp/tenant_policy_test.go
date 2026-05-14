package anvilmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTenantID(t *testing.T) {
	got, err := NormalizeTenantID(" team.alpha-1 ")
	if err != nil {
		t.Fatalf("NormalizeTenantID() error = %v", err)
	}
	if got != "team.alpha-1" {
		t.Fatalf("NormalizeTenantID() = %q, want team.alpha-1", got)
	}

	for _, value := range []string{
		"",
		"../tenant",
		"tenant/name",
		"-tenant",
		strings.Repeat("a", MaxTenantIDLength+1),
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := NormalizeTenantID(value); err == nil {
				t.Fatalf("NormalizeTenantID(%q) error = nil, want error", value)
			}
		})
	}
}

func TestCheckTenantQuotaRejectsFirstExceededResource(t *testing.T) {
	decision, err := CheckTenantQuota(
		TenantQuota{
			ActiveVMs:            2,
			SnapshotCount:        10,
			SnapshotBytes:        100,
			ConcurrentTasks:      1,
			RetainedAuditRecords: 0,
		},
		TenantUsage{
			ActiveVMs:       1,
			SnapshotCount:   5,
			SnapshotBytes:   90,
			ConcurrentTasks: 1,
		},
		TenantUsage{
			ActiveVMs:       1,
			SnapshotBytes:   20,
			ConcurrentTasks: 1,
		},
	)
	if err != nil {
		t.Fatalf("CheckTenantQuota() error = %v", err)
	}
	if decision.Allowed {
		t.Fatal("CheckTenantQuota() allowed = true, want false")
	}
	if decision.Resource != "snapshot_bytes" || decision.Reason != "quota_exceeded" {
		t.Fatalf("decision = %+v, want snapshot_bytes quota_exceeded", decision)
	}
	if decision.Used != 90 || decision.Requested != 20 || decision.Limit != 100 {
		t.Fatalf("decision values = %+v, want used=90 requested=20 limit=100", decision)
	}
}

func TestCheckTenantQuotaAllowsZeroUnlimited(t *testing.T) {
	decision, err := CheckTenantQuota(
		TenantQuota{},
		TenantUsage{ActiveVMs: 100, SnapshotBytes: 1 << 40},
		TenantUsage{ActiveVMs: 1, SnapshotBytes: 1 << 40},
	)
	if err != nil {
		t.Fatalf("CheckTenantQuota() error = %v", err)
	}
	if !decision.Allowed || decision.Reason != "allowed" {
		t.Fatalf("decision = %+v, want allowed", decision)
	}
}

func TestNormalizeEgressPolicy(t *testing.T) {
	for _, value := range []string{"", "deny_all", "profile", "allow_all"} {
		if _, err := NormalizeEgressPolicy(value); err != nil {
			t.Fatalf("NormalizeEgressPolicy(%q) error = %v", value, err)
		}
	}
	if _, err := NormalizeEgressPolicy("internet"); err == nil {
		t.Fatal("NormalizeEgressPolicy(internet) error = nil, want error")
	}
}

func TestSelectRuntimeHost(t *testing.T) {
	host, err := SelectRuntimeHost([]RuntimeHost{
		{
			Name:                   "host-a",
			Endpoint:               "http://host-a:3000",
			Healthy:                true,
			AvailableVMs:           0,
			AvailableSnapshotBytes: 1 << 30,
			EgressPolicies:         []EgressPolicy{EgressPolicyProfile},
		},
		{
			Name:                   "host-b",
			Endpoint:               "http://host-b:3000",
			Healthy:                true,
			AvailableVMs:           2,
			AvailableSnapshotBytes: 64,
			EgressPolicies:         []EgressPolicy{EgressPolicyProfile, EgressPolicyAllowAll},
		},
		{
			Name:                   "host-c",
			Endpoint:               "http://host-c:3000",
			Healthy:                true,
			AvailableVMs:           1,
			AvailableSnapshotBytes: 1 << 20,
			EgressPolicies:         []EgressPolicy{EgressPolicyProfile},
		},
	}, ScheduleRequest{
		TenantID:               "tenant-1",
		RequestedSnapshotBytes: 1024,
		EgressPolicy:           EgressPolicyProfile,
	})
	if err != nil {
		t.Fatalf("SelectRuntimeHost() error = %v", err)
	}
	if host.Name != "host-c" {
		t.Fatalf("SelectRuntimeHost() = %q, want host-c", host.Name)
	}
}

func TestSelectRuntimeHostRejectsUnsupportedEgress(t *testing.T) {
	_, err := SelectRuntimeHost([]RuntimeHost{
		{
			Name:                   "host-a",
			Endpoint:               "http://host-a:3000",
			Healthy:                true,
			AvailableVMs:           1,
			AvailableSnapshotBytes: 1 << 20,
			EgressPolicies:         []EgressPolicy{EgressPolicyDenyAll},
		},
	}, ScheduleRequest{
		TenantID:               "tenant-1",
		RequestedSnapshotBytes: 1024,
		EgressPolicy:           EgressPolicyProfile,
	})
	if err == nil {
		t.Fatal("SelectRuntimeHost() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "no eligible runtime host") {
		t.Fatalf("SelectRuntimeHost() error = %q, want no eligible runtime host", err)
	}
}

func TestAppendRuntimeAuditWritesJSONLWithoutAgentToken(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit", "runtime.jsonl")
	record := RuntimeAuditRecord{
		Timestamp:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		TenantID:        "tenant-1",
		VMID:            "vm-1",
		SessionAlias:    "session-a",
		ToolName:        "anvil_run_task",
		DaemonOperation: "POST /vms/{vm_id}/tasks",
		ResultCode:      "success",
	}

	if err := AppendRuntimeAudit(auditPath, record); err != nil {
		t.Fatalf("AppendRuntimeAudit() error = %v", err)
	}
	if err := AppendRuntimeAudit(auditPath, record); err != nil {
		t.Fatalf("AppendRuntimeAudit() second error = %v", err)
	}

	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit path: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("audit mode = %v, want 0600", mode)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit path: %v", err)
	}
	if strings.Contains(string(data), "agent_token") {
		t.Fatalf("audit log includes agent_token: %q", string(data))
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2: %q", len(lines), string(data))
	}
	var got RuntimeAuditRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("parse audit line: %v", err)
	}
	if got.TenantID != record.TenantID || got.ToolName != record.ToolName || got.ResultCode != record.ResultCode {
		t.Fatalf("audit record = %+v, want tenant/tool/result from %+v", got, record)
	}
}

func TestAppendRuntimeAuditRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	targetPath := filepath.Join(tmp, "target.jsonl")
	if err := os.WriteFile(targetPath, []byte{}, 0600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	auditPath := filepath.Join(tmp, "audit.jsonl")
	if err := os.Symlink(targetPath, auditPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	err := AppendRuntimeAudit(auditPath, RuntimeAuditRecord{
		Timestamp:       time.Now().UTC(),
		TenantID:        "tenant-1",
		ToolName:        "anvil_run_task",
		DaemonOperation: "POST /vms/{vm_id}/tasks",
		ResultCode:      "success",
	})
	if err == nil {
		t.Fatal("AppendRuntimeAudit() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("AppendRuntimeAudit() error = %q, want symlink", err)
	}
}

func TestReadAndPruneRuntimeAudit(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit", "runtime.jsonl")
	base := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	for _, record := range []RuntimeAuditRecord{
		{
			Timestamp:       base.Add(-3 * time.Hour),
			TenantID:        "tenant-1",
			ToolName:        "anvil_spawn_vm",
			DaemonOperation: "POST /vms",
			ResultCode:      "success",
		},
		{
			Timestamp:       base.Add(-2 * time.Hour),
			TenantID:        "tenant-1",
			VMID:            "vm-1",
			ToolName:        "anvil_run_task",
			DaemonOperation: "POST /vms/{vm_id}/tasks",
			ResultCode:      "error",
			Error:           "daemon returned status 502",
		},
		{
			Timestamp:       base.Add(-1 * time.Hour),
			TenantID:        "tenant-1",
			ToolName:        "anvil_list_snapshots",
			DaemonOperation: "GET /snapshots",
			ResultCode:      "success",
		},
	} {
		if err := AppendRuntimeAudit(auditPath, record); err != nil {
			t.Fatalf("AppendRuntimeAudit() error = %v", err)
		}
	}

	records, err := ReadRuntimeAudit(auditPath)
	if err != nil {
		t.Fatalf("ReadRuntimeAudit() error = %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("record count = %d, want 3", len(records))
	}
	if records[1].ResultCode != "error" || records[1].Error != "daemon returned status 502" {
		t.Fatalf("failure record = %+v, want sanitized error", records[1])
	}

	if err := PruneRuntimeAudit(auditPath, RuntimeAuditRetention{KeepLast: 2}); err != nil {
		t.Fatalf("PruneRuntimeAudit() error = %v", err)
	}
	records, err = ReadRuntimeAudit(auditPath)
	if err != nil {
		t.Fatalf("ReadRuntimeAudit() after prune error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count after prune = %d, want 2", len(records))
	}
	if records[0].ToolName != "anvil_run_task" || records[1].ToolName != "anvil_list_snapshots" {
		t.Fatalf("records after prune = %+v, want last two records", records)
	}
}
