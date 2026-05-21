package main

import (
	"encoding/json"
	"net/http"
)

// VMStats is the response body for GET /vms/{vm_id}/stats and the embedded
// `stats` field on GET /vms?stats=true. Values are a point-in-time snapshot —
// callers wanting streaming should poll.
//
// network_rx_bytes / network_tx_bytes are reported from the VM's perspective
// (received / transmitted by the guest) even though the host TAP reports the
// inverse. collectVMStats applies the swap before returning.
type VMStats struct {
	VMID           string  `json:"vm_id"`
	CPUPercent     float64 `json:"cpu_percent"`
	MemUsedMib     int64   `json:"mem_used_mib"`
	MemTotalMib    int64   `json:"mem_total_mib"`
	UptimeSeconds  int64   `json:"uptime_seconds"`
	NetworkRxBytes int64   `json:"network_rx_bytes"`
	NetworkTxBytes int64   `json:"network_tx_bytes"`
	AgentBusy      bool    `json:"agent_busy"`
}

// VMInfoWithStats embeds VMInfo and the per-VM stats block for the ?stats=true
// branch of GET /vms. The stats field is always populated (zeroes for failed
// collections so dashboards don't see a missing key).
type VMInfoWithStats struct {
	VMInfo
	Stats VMStats `json:"stats"`
}

// handleVMStats returns a snapshot of cpu/mem/net/uptime/agent_busy for one VM.
// GET only. Returns 404 if the vm_id is not currently registered.
func (cp *ControlPlane) handleVMStats(w http.ResponseWriter, r *http.Request, vmID string) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
		return
	}
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"vm not found"}`, http.StatusNotFound)
		return
	}
	stats := cp.collectVMStats(r.Context(), vmID, v)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}
