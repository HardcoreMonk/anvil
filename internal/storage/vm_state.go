// [학습 주석] internal/storage/vm_state.go
//
// 이 파일은 "콜드 리스타트 복구 계약"의 핵심이다: 데몬이 죽었다 다시 뜰 때 어떤 VM을
// 어떤 정체성(IP/TAP/MAC/agent token)으로 되살릴지를 결정하는 유일한 소스가
// workDir/vms/<vm_id>/state.json이다. upstream v0.4.0에서 DiskMode(plain|cow)와
// 메모리 auto-snapshot(AutoSnapshotDir 계열)이 들어왔고, v0.4.5에서 SourceSnapshotID가
// 추가되어 "스냅샷에서 restore된 VM"도 재시작 후 복구 대상에 포함되게 됐다.
// anvil은 여기에 TenantID/EgressPolicy 필드를 얹어서, 데몬 재시작 후에도 tenant 격리와
// egress 정책이 사라지지 않도록 적응했다(원본 upstream state.json에는 없는 필드).
//
// 함정: SourceSnapshotID가 있는 VM은 DiskPath가 "영속적인 디스크"가 아니라 매번
// graceful shutdown 때 버려지고 재기동 때 새로 만들어지는 임시 exception store다
// (recovery.go의 recoverRestoredVM 참고) — 이 상태를 spawn VM과 똑같이 취급하면
// snapshot GC가 아직 살아있는 restored VM의 원본 스냅샷을 지워버릴 수 있다.
//
// 관련 가드 테스트: TestRestoreSnapshotPersistsRecoverableVMState.
package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// vmStateSchemaVersion is the current on-disk format version for per-VM state.
// Bump when the wire layout changes in a way that needs migration logic.
const vmStateSchemaVersion = 1

// Disk mode tags recorded in VMState.DiskMode. Cold-restart recovery handles both:
// "plain" boots the full rootfs clone, "cow" re-layers the preserved dm-snapshot
// exception store over the golden image (v0.4.0).
// [학습] 이 두 값은 recovery.go의 RecoverVMs가 분기하는 기준이다: "plain"이면 rootfs
// 클론 파일을 그대로 재부팅하고, "cow"면 provisioner.CloneDiskCOW가 만든 것과 같은
// 방식으로 exception store를 golden image 위에 다시 얹는다(SetupDMSnapshot 재호출).
const (
	DiskModePlain = "plain"
	DiskModeCOW   = "cow"
)

// VMState is the on-disk snapshot of everything cold-restart needs to bring a
// previously-running VM back after a daemon restart (graceful or crash).
// Persisted per-VM at workDir/vms/<vm_id>/state.json.
//
// Memory state is NOT preserved in state.json; cold-restart boots the VM from
// its rootfs clone and re-attaches the same network identity and agent token.
// (Opt-in EPHEMERA_AUTOSNAPSHOT additionally writes a separate memory snapshot
// under auto/ for warm restore — see AutoSnapshotDir.)
type VMState struct {
	SchemaVersion int    `json:"schema_version"`
	VMID          string `json:"vm_id"`
	GuestIP       string `json:"guest_ip"`
	TapDevice     string `json:"tap_device"`
	MacAddr       string `json:"mac_addr"`
	VsockPath     string `json:"vsock_path"`
	SocketPath    string `json:"socket_path"`
	AgentToken    string `json:"agent_token"`
	DiskPath      string `json:"disk_path"`
	DiskMode      string `json:"disk_mode"` // "plain" | "cow"
	Profile       string `json:"profile,omitempty"`
	TenantID      string `json:"tenant_id,omitempty"`
	EgressPolicy  string `json:"egress_policy,omitempty"`
	// SourceSnapshotID, when non-empty, marks this VM as snapshot-derived
	// (v0.4.5): recovery re-restores it from that snapshot instead of cold-
	// booting from a rootfs clone. The exception store in DiskPath is transient
	// (discarded on shutdown, recreated fresh on re-restore), so the recoverable
	// artifact is the source snapshot, not the disk. Empty for spawn-path VMs.
	// Snapshot GC must keep this source while the restored VM state is live.
	// [학습] 이 필드가 비어있지 않으면 recovery.go의 RecoverVMs는 이 VM을 일반
	// spawn-path cold-restart 분기가 아니라 recoverRestoredVM 분기로 보낸다 — 그
	// 분기는 디스크를 그대로 재사용하는 게 아니라 SourceSnapshotID가 가리키는
	// snapshot에서 메모리+상태를 다시 통째로 불러온다("재복구"이지 "이어붙이기"가 아님).
	SourceSnapshotID string    `json:"source_snapshot_id,omitempty"`
	VcpuCount        int64     `json:"vcpu_count"`
	MemSizeMib       int64     `json:"mem_size_mib"`
	FlockID          string    `json:"flock_id,omitempty"`
	AgentID          string    `json:"agent_id,omitempty"`
	AgentURL         string    `json:"agent_url"`
	CreatedAt        time.Time `json:"created_at"`
}

// vmStatePath returns the per-VM state.json location under workDir.
func vmStatePath(workDir, vmID string) string {
	return filepath.Join(workDir, "vms", vmID, "state.json")
}

// AutoSnapshotDir returns the per-VM memory auto-snapshot directory under
// workDir — a subdirectory of the VM's state directory (v0.4.0). When
// EPHEMERA_AUTOSNAPSHOT is enabled, the graceful-shutdown path writes a
// memory+state snapshot here and RecoverVMs warm-restores from it.
func AutoSnapshotDir(workDir, vmID string) string {
	return filepath.Join(workDir, "vms", vmID, "auto")
}

// AutoSnapshotPaths returns the memory and state file paths inside a VM's
// auto-snapshot directory.
func AutoSnapshotPaths(workDir, vmID string) (memPath, statPath string) {
	dir := AutoSnapshotDir(workDir, vmID)
	return filepath.Join(dir, "memory.bin"), filepath.Join(dir, "state.bin")
}

// AutoSnapshotExists reports whether a usable memory auto-snapshot is present
// for vmID (the memory file is the load-bearing artifact for a warm restore).
func AutoSnapshotExists(workDir, vmID string) bool {
	memPath, _ := AutoSnapshotPaths(workDir, vmID)
	_, err := os.Stat(memPath)
	return err == nil
}

// RemoveAutoSnapshot deletes a VM's auto-snapshot directory (best-effort).
// Auto-snapshots are one-shot: consumed on a restore attempt and rewritten by
// the next graceful shutdown, so a stale image is never reused. Call this
// whenever a VM's state.json is dropped so an orphaned auto/ does not linger.
// [학습] anvil은 EPHEMERA_AUTOSNAPSHOT 기본값을 off로 유지한다(docs/analysis 10번
// 문서: "memory auto-snapshot deferred") — 전체 메모리 이미지를 디스크에 매번 쓰는
// 비용과 in-flight task 손실 정책이 아직 운영 문서화되지 않았기 때문이다. 코드 경로
// 자체는 살아있지만 public 지원 대상은 아니다.
func RemoveAutoSnapshot(workDir, vmID string) {
	os.RemoveAll(AutoSnapshotDir(workDir, vmID))
}

// [학습] tmp+rename 패턴이 여기서도 반복된다: state.json을 쓰는 도중 데몬이 죽어도
// 절반만 쓰인 상태 파일이 남지 않는다(rename은 원자적). 파일 모드가 0600인 이유는
// AgentToken 필드가 이 JSON 안에 평문으로 들어있기 때문 — state.json 자체가 하나의
// 시크릿 저장소라는 뜻이다.
// SaveVMState writes state atomically (tmp + rename). Not safe for concurrent
// writes to the same VM; the daemon's call sites (spawnVMInternal once,
// destroyVM once) never overlap.
func SaveVMState(workDir string, state VMState) error {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = vmStateSchemaVersion
	}
	dst := vmStatePath(workDir, state.VMID)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("vm_state: create dir: %w", err)
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("vm_state: marshal: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return fmt.Errorf("vm_state: write tmp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("vm_state: rename: %w", err)
	}
	return nil
}

// LoadVMState reads a single VM's state.json.
func LoadVMState(workDir, vmID string) (VMState, error) {
	b, err := os.ReadFile(vmStatePath(workDir, vmID))
	if err != nil {
		return VMState{}, fmt.Errorf("vm_state: read: %w", err)
	}
	var s VMState
	if err := json.Unmarshal(b, &s); err != nil {
		return VMState{}, fmt.Errorf("vm_state: unmarshal: %w", err)
	}
	return s, nil
}

// VMStateExists reports whether a persisted state.json is present for vmID. Only
// spawn VMs persist state (SaveVMState), so a present file means RecoverVMs will
// bring the VM back — the graceful-shutdown path uses this to decide whether to
// preserve a COW exception store or tear it down.
func VMStateExists(workDir, vmID string) bool {
	_, err := os.Stat(vmStatePath(workDir, vmID))
	return err == nil
}

// DeleteVMState removes a VM's state.json and its parent directory if empty.
// Missing entries are not an error.
func DeleteVMState(workDir, vmID string) error {
	dir := filepath.Dir(vmStatePath(workDir, vmID))
	err := os.Remove(vmStatePath(workDir, vmID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vm_state: delete: %w", err)
	}
	// Best-effort directory removal; os.Remove only succeeds on empty dirs.
	os.Remove(dir)
	return nil
}

// ListVMState enumerates every recoverable VM under workDir/vms/.
// Directories without a parseable state.json are skipped. Results are sorted by
// VMID so recovery order is deterministic.
func ListVMState(workDir string) ([]VMState, error) {
	base := filepath.Join(workDir, "vms")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vm_state: list dir: %w", err)
	}
	var out []VMState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := LoadVMState(workDir, e.Name())
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].VMID < out[j].VMID
	})
	return out, nil
}
