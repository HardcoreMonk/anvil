package main

import (
	"fmt"
	"net/http"
)

// SizingPreset describes a named VM resource tier offered in the Settings UI's
// profile editor. The id is a stable slug; Label is the display name. VcpuCount
// and MemSizeMib are the values applied to a profile when the preset is chosen.
// This registry is the single source of truth for the sizing tiers the UI offers.
//
// The "standard" tier intentionally mirrors the daemon's default sizing
// (vm.defaultVcpuCount / vm.defaultMemSizeMib): a profile with no explicit sizing
// spawns at these values, so "Standard" and "unsized" are the same VM.
type SizingPreset struct {
	ID         string `json:"id"`    // "light" | "standard" | "advanced"
	Label      string `json:"label"` // human-facing name
	VcpuCount  int64  `json:"vcpu_count"`
	MemSizeMib int64  `json:"mem_size_mib"`
}

// sizingPresetRegistry is intentionally a slice (not a map) so the UI renders the
// tiers in a stable, curated order (lightest first). To add a tier, append here —
// the /config/presets endpoint picks it up automatically.
//
// Rationale for the tiers: Goose runs LLM inference remotely, so the VM mostly
// waits on network IO and runs agent tools. Light suits chat/file-edit agents;
// Standard absorbs light tool execution; Advanced is for build/test-heavy roles
// (parallel compiles, node_modules). All values stay within validateSizing bounds.
var sizingPresetRegistry = []SizingPreset{
	{ID: "light", Label: "Light", VcpuCount: 1, MemSizeMib: 512},
	{ID: "standard", Label: "Standard", VcpuCount: 1, MemSizeMib: 1024},
	{ID: "advanced", Label: "Advanced", VcpuCount: 2, MemSizeMib: 2048},
}

// handleConfigPresets serves GET /config/presets — the registry of named VM sizing
// tiers (Light/Standard/Advanced) the profile editor offers as quick presets. The
// values are advisory: a profile may still carry any sizing within validateSizing's
// bounds. The "standard" tier matches the daemon's default sizing.
func (cp *ControlPlane) handleConfigPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	writeJSON(w, http.StatusOK, sizingPresetRegistry)
}
