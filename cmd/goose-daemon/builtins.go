package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// BuiltinInfo describes a goose builtin extension a profile can enable. The id is
// the exact name passed to `goose run --with-builtin`; Label/Description are
// display-only. This registry is the single source of truth for the builtin
// extensions the profile editor offers — it must stay in sync with the goose
// build baked into the golden image (verify the ids against the valid
// `goose mcp <SERVER>` names of that build).
type BuiltinInfo struct {
	ID          string `json:"id"`          // exact --with-builtin name
	Label       string `json:"label"`       // human-facing name
	Description string `json:"description"` // one-line summary for the UI
	Default     bool   `json:"default"`     // pre-selected for new profiles
}

// builtinRegistry is intentionally a slice (not a map) so the UI renders the
// extensions in a stable, curated order. The ids below are verified present in
// the bundled goose 1.37.0 build (aaif-goose stable). To add one, append here —
// /config/builtins and validateBuiltins pick it up automatically.
//
// Only "developer" is Default: it preserves the historical hardcoded behaviour
// (shell + file editing) as a sensible — but now removable — starting point, so
// removing the old `--with-builtin developer` hardcode does not silently strip
// every agent's tools. Enabling more builtins grows the per-request tool schema
// (~7.5k tokens for goose's full default set), which can exceed a provider's
// per-minute token budget (e.g. Groq free-tier 6000 TPM) — the UI warns of this.
var builtinRegistry = []BuiltinInfo{
	{ID: "developer", Label: "Developer", Description: "Shell commands and file/text editing.", Default: true},
	{ID: "memory", Label: "Memory", Description: "Persistent memory the agent reads and writes across turns.", Default: false},
	{ID: "tutorial", Label: "Tutorial", Description: "Interactive goose tutorials.", Default: false},
}

// defaultBuiltins returns the builtin ids pre-selected for a new profile (those
// flagged Default in the registry) — currently just "developer".
func defaultBuiltins() []string {
	out := []string{}
	for _, b := range builtinRegistry {
		if b.Default {
			out = append(out, b.ID)
		}
	}
	return out
}

// knownBuiltin reports whether id is a builtin in the registry.
func knownBuiltin(id string) bool {
	for _, b := range builtinRegistry {
		if b.ID == id {
			return true
		}
	}
	return false
}

// validateBuiltins rejects any name not in the registry. Whitelisting the names
// (rather than only guarding characters) keeps unknown/garbage values out of the
// `--with-builtin` argv the in-VM agent builds, and doubles as injection defense.
// An empty list is valid: a profile may deliberately run with no builtin tools.
func validateBuiltins(names []string) error {
	for _, n := range names {
		if !knownBuiltin(strings.TrimSpace(n)) {
			return fmt.Errorf("unknown builtin extension %q", n)
		}
	}
	return nil
}

// normalizeBuiltins trims, de-duplicates, and re-orders a requested set to the
// registry's canonical order, so the stored EPHEMERA_BUILTINS line is stable
// regardless of the order the UI sends. Call validateBuiltins first.
func normalizeBuiltins(names []string) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	out := []string{}
	for _, b := range builtinRegistry {
		if want[b.ID] {
			out = append(out, b.ID)
		}
	}
	return out
}

// handleConfigBuiltins serves GET /config/builtins — the registry of goose builtin
// extensions the profile editor offers as a checkbox group. Advisory display data;
// the authoritative gate is validateBuiltins on write.
func (cp *ControlPlane) handleConfigBuiltins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	writeJSON(w, http.StatusOK, builtinRegistry)
}

// parseProfileBuiltins extracts the optional EPHEMERA_BUILTINS key (a comma-
// separated builtin list) from goose.yaml bytes. A missing key returns nil, which
// callers treat as "use the default builtin"; an explicit empty value returns an
// empty, non-nil slice meaning "no builtins". Mirrors the in-VM agent's
// loadBuiltins so the UI shows exactly what the agent will run.
func parseProfileBuiltins(data []byte) []string {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := topLevelScalar(line)
		if !ok || key != "EPHEMERA_BUILTINS" {
			continue
		}
		return splitBuiltinsCSV(val)
	}
	return nil
}

// splitBuiltinsCSV splits a comma-separated builtin list, trimming spaces and
// dropping empties. "" → an empty (non-nil) slice.
func splitBuiltinsCSV(val string) []string {
	out := []string{}
	for _, p := range strings.Split(val, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// writeProfileBuiltins rewrites the EPHEMERA_BUILTINS line in a profile's
// goose.yaml in place (line-based, preserving comments, ordering, and the nested
// extensions block), appending it if absent. The write is atomic (temp+rename).
// An empty list writes an empty value, meaning the profile runs with no builtin
// tools.
func (cp *ControlPlane) writeProfileBuiltins(name string, builtins []string) error {
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	csv := strings.Join(builtins, ",")
	lines := strings.Split(string(data), "\n")
	set := false
	for i, line := range lines {
		key, _, ok := topLevelScalar(line)
		if ok && key == "EPHEMERA_BUILTINS" {
			lines[i] = "EPHEMERA_BUILTINS: " + csv
			set = true
			break
		}
	}
	if !set {
		lines = append(lines, "EPHEMERA_BUILTINS: "+csv)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handleConfigProfileBuiltins serves GET/PUT /config/profiles/{name}/builtins —
// the profile's enabled goose builtin extensions, injected into every VM spawned
// from the profile (the agent reads EPHEMERA_BUILTINS from its goose config at
// run time). Like system.md, editing affects only FUTURE spawns, so there is no
// in-use (409) guard. Mirrors handleConfigProfileSystem.
func (cp *ControlPlane) handleConfigProfileBuiltins(w http.ResponseWriter, r *http.Request, name string) {
	// Reject empty and any path-traversal form before touching the filesystem
	// (same guard as handleConfigProfile; see validProfileNameSyntax for why
	// "." must be caught here rather than left to the looser inline check
	// this used to have).
	if !validProfileNameSyntax(name) {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("profile name required"))
		return
	}
	// A profile exists iff its goose.yaml does; the resolver also handles
	// "default" → the daemon config and guards path traversal.
	if _, err := cp.gooseConfigPathForProfile(name); err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		pc, err := cp.readProfileConfig(name)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string][]string{"builtins": pc.Builtins})
	case http.MethodPut:
		var body struct {
			Builtins []string `json:"builtins"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body"))
			return
		}
		if body.Builtins == nil {
			body.Builtins = []string{}
		}
		if err := validateBuiltins(body.Builtins); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		norm := normalizeBuiltins(body.Builtins)
		if err := cp.writeProfileBuiltins(name, norm); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile builtins updated", "profile", name, "builtins", strings.Join(norm, ","))
		writeJSON(w, http.StatusOK, map[string][]string{"builtins": norm})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}
