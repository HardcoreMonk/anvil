package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const MaxTenantIDLength = 64

type TenantQuota struct {
	ActiveVMs            int64 `json:"active_vms"`
	SnapshotCount        int64 `json:"snapshot_count"`
	SnapshotBytes        int64 `json:"snapshot_bytes"`
	ConcurrentTasks      int64 `json:"concurrent_tasks"`
	RetainedAuditRecords int64 `json:"retained_audit_records"`
}

type TenantUsage struct {
	ActiveVMs            int64 `json:"active_vms"`
	SnapshotCount        int64 `json:"snapshot_count"`
	SnapshotBytes        int64 `json:"snapshot_bytes"`
	ConcurrentTasks      int64 `json:"concurrent_tasks"`
	RetainedAuditRecords int64 `json:"retained_audit_records"`
}

type QuotaDecision struct {
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason"`
	Resource  string `json:"resource,omitempty"`
	Limit     int64  `json:"limit,omitempty"`
	Used      int64  `json:"used,omitempty"`
	Requested int64  `json:"requested,omitempty"`
}

type EgressPolicy string

const (
	EgressPolicyDenyAll  EgressPolicy = "deny_all"
	EgressPolicyProfile  EgressPolicy = "profile"
	EgressPolicyAllowAll EgressPolicy = "allow_all"
)

type RuntimeHost struct {
	Name                   string         `json:"name"`
	Endpoint               string         `json:"endpoint"`
	Healthy                bool           `json:"healthy"`
	AvailableVMs           int64          `json:"available_vms"`
	AvailableSnapshotBytes int64          `json:"available_snapshot_bytes"`
	EgressPolicies         []EgressPolicy `json:"egress_policies"`
}

type ScheduleRequest struct {
	TenantID               string       `json:"tenant_id"`
	Profile                string       `json:"profile,omitempty"`
	RequestedSnapshotBytes int64        `json:"requested_snapshot_bytes,omitempty"`
	EgressPolicy           EgressPolicy `json:"egress_policy"`
}

type RuntimeAuditRecord struct {
	Timestamp       time.Time `json:"timestamp"`
	TenantID        string    `json:"tenant_id"`
	VMID            string    `json:"vm_id,omitempty"`
	SessionAlias    string    `json:"session_alias,omitempty"`
	ToolName        string    `json:"tool_name"`
	DaemonOperation string    `json:"daemon_operation"`
	ResultCode      string    `json:"result_code"`
}

func NormalizeTenantID(value string) (string, error) {
	tenantID := strings.TrimSpace(value)
	if tenantID == "" {
		return "", fmt.Errorf("tenant_id must be non-empty")
	}
	if len(tenantID) > MaxTenantIDLength {
		return "", fmt.Errorf("tenant_id must be <= %d bytes", MaxTenantIDLength)
	}
	for _, r := range tenantID {
		if r > 127 {
			return "", fmt.Errorf("tenant_id must use ASCII letters, digits, dot, underscore, or hyphen")
		}
		if isTenantIDChar(byte(r)) {
			continue
		}
		return "", fmt.Errorf("tenant_id must use ASCII letters, digits, dot, underscore, or hyphen")
	}
	if !isTenantIDAlphaNum(tenantID[0]) {
		return "", fmt.Errorf("tenant_id must start with an ASCII letter or digit")
	}
	if strings.Contains(tenantID, "..") {
		return "", fmt.Errorf("tenant_id must not contain path traversal")
	}
	return tenantID, nil
}

func CheckTenantQuota(limit TenantQuota, used TenantUsage, requested TenantUsage) (QuotaDecision, error) {
	if err := validateQuotaValues(limit, used, requested); err != nil {
		return QuotaDecision{}, err
	}
	for _, item := range []struct {
		resource  string
		limit     int64
		used      int64
		requested int64
	}{
		{"active_vms", limit.ActiveVMs, used.ActiveVMs, requested.ActiveVMs},
		{"snapshot_count", limit.SnapshotCount, used.SnapshotCount, requested.SnapshotCount},
		{"snapshot_bytes", limit.SnapshotBytes, used.SnapshotBytes, requested.SnapshotBytes},
		{"concurrent_tasks", limit.ConcurrentTasks, used.ConcurrentTasks, requested.ConcurrentTasks},
		{"retained_audit_records", limit.RetainedAuditRecords, used.RetainedAuditRecords, requested.RetainedAuditRecords},
	} {
		if item.limit > 0 && item.used+item.requested > item.limit {
			return QuotaDecision{
				Allowed:   false,
				Reason:    "quota_exceeded",
				Resource:  item.resource,
				Limit:     item.limit,
				Used:      item.used,
				Requested: item.requested,
			}, nil
		}
	}
	return QuotaDecision{Allowed: true, Reason: "allowed"}, nil
}

func NormalizeEgressPolicy(value string) (EgressPolicy, error) {
	policy := EgressPolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case "":
		return EgressPolicyProfile, nil
	case EgressPolicyDenyAll, EgressPolicyProfile, EgressPolicyAllowAll:
		return policy, nil
	default:
		return "", fmt.Errorf("egress_policy must be empty, deny_all, profile, or allow_all")
	}
}

func SelectRuntimeHost(hosts []RuntimeHost, req ScheduleRequest) (RuntimeHost, error) {
	if _, err := NormalizeTenantID(req.TenantID); err != nil {
		return RuntimeHost{}, err
	}
	if req.RequestedSnapshotBytes < 0 {
		return RuntimeHost{}, fmt.Errorf("requested_snapshot_bytes must be non-negative")
	}
	policy, err := NormalizeEgressPolicy(string(req.EgressPolicy))
	if err != nil {
		return RuntimeHost{}, err
	}

	for _, host := range hosts {
		if strings.TrimSpace(host.Name) == "" || strings.TrimSpace(host.Endpoint) == "" {
			continue
		}
		if !host.Healthy || host.AvailableVMs <= 0 {
			continue
		}
		if req.RequestedSnapshotBytes > 0 && host.AvailableSnapshotBytes < req.RequestedSnapshotBytes {
			continue
		}
		if !hostSupportsEgress(host, policy) {
			continue
		}
		return host, nil
	}
	return RuntimeHost{}, fmt.Errorf("no eligible runtime host for tenant %q", req.TenantID)
}

func AppendRuntimeAudit(auditPath string, record RuntimeAuditRecord) error {
	auditPath = strings.TrimSpace(auditPath)
	if auditPath == "" {
		return fmt.Errorf("audit path must be non-empty")
	}

	tenantID, err := NormalizeTenantID(record.TenantID)
	if err != nil {
		return err
	}
	record.TenantID = tenantID
	record.VMID = strings.TrimSpace(record.VMID)
	record.SessionAlias = strings.TrimSpace(record.SessionAlias)
	record.ToolName = strings.TrimSpace(record.ToolName)
	record.DaemonOperation = strings.TrimSpace(record.DaemonOperation)
	record.ResultCode = strings.TrimSpace(record.ResultCode)
	if record.ToolName == "" {
		return fmt.Errorf("tool_name must be non-empty")
	}
	if record.DaemonOperation == "" {
		return fmt.Errorf("daemon_operation must be non-empty")
	}
	if record.ResultCode == "" {
		return fmt.Errorf("result_code must be non-empty")
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	} else {
		record.Timestamp = record.Timestamp.UTC()
	}

	dir := filepath.Dir(auditPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create runtime audit dir: %w", err)
		}
	}
	if info, err := os.Lstat(auditPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime audit file is a symlink")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime audit file is not regular")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat runtime audit file: %w", err)
	}

	fd, err := unix.Open(auditPath, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("open runtime audit file: %w", err)
	}
	f := os.NewFile(uintptr(fd), auditPath)
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat opened runtime audit file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opened runtime audit file is not regular")
	}
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("chmod runtime audit file before write: %w", err)
	}
	if err := json.NewEncoder(f).Encode(record); err != nil {
		return fmt.Errorf("write runtime audit record: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("chmod runtime audit file after append: %w", err)
	}
	return nil
}

func validateQuotaValues(values ...interface{}) error {
	for _, value := range values {
		switch v := value.(type) {
		case TenantQuota:
			if v.ActiveVMs < 0 || v.SnapshotCount < 0 || v.SnapshotBytes < 0 || v.ConcurrentTasks < 0 || v.RetainedAuditRecords < 0 {
				return fmt.Errorf("quota values must be non-negative")
			}
		case TenantUsage:
			if v.ActiveVMs < 0 || v.SnapshotCount < 0 || v.SnapshotBytes < 0 || v.ConcurrentTasks < 0 || v.RetainedAuditRecords < 0 {
				return fmt.Errorf("usage values must be non-negative")
			}
		}
	}
	return nil
}

func hostSupportsEgress(host RuntimeHost, policy EgressPolicy) bool {
	for _, allowed := range host.EgressPolicies {
		if allowed == policy {
			return true
		}
	}
	return false
}

func isTenantIDChar(b byte) bool {
	return isTenantIDAlphaNum(b) || b == '.' || b == '_' || b == '-'
}

func isTenantIDAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
