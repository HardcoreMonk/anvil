package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
)

// parseProfileMCPServers extracts the optional EPHEMERA_MCP_SERVERS key (a comma-
// separated backend-server-id list) from goose.yaml bytes. A missing key returns
// (nil, false) — "no binding, use servers.yaml as-is"; a present key (even empty)
// returns (list, true), where an empty value means "this profile uses no MCP
// servers". Mirrors parseProfileBuiltins.
func parseProfileMCPServers(data []byte) ([]string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := topLevelScalar(line)
		if !ok || key != "EPHEMERA_MCP_SERVERS" {
			continue
		}
		return splitBuiltinsCSV(val), true
	}
	return nil, false
}

// mcpBindingForProfile reads a profile's MCP server binding for the gateway's
// PolicyStore. Returns (servers, true) when the profile sets EPHEMERA_MCP_SERVERS,
// else (nil, false) meaning "no binding". Best-effort: an unreadable profile
// yields no binding. The PolicyStore intersects this with servers.yaml, so a
// binding can only narrow a profile's access, never widen it.
func (cp *ControlPlane) mcpBindingForProfile(profile string) ([]string, bool) {
	path, err := cp.gooseConfigPathForProfile(profile)
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return parseProfileMCPServers(data)
}

// writeProfileMCPServers rewrites the EPHEMERA_MCP_SERVERS line in a profile's
// goose.yaml in place (line-based, preserving comments and ordering), appending
// it if absent. The write is atomic (temp+rename). Mirrors writeProfileBuiltins.
func (cp *ControlPlane) writeProfileMCPServers(name string, servers []string) error {
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	csv := strings.Join(servers, ",")
	lines := strings.Split(string(data), "\n")
	set := false
	for i, line := range lines {
		key, _, ok := topLevelScalar(line)
		if ok && key == "EPHEMERA_MCP_SERVERS" {
			lines[i] = "EPHEMERA_MCP_SERVERS: " + csv
			set = true
			break
		}
	}
	if !set {
		lines = append(lines, "EPHEMERA_MCP_SERVERS: "+csv)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalizeMCPServers trims, de-duplicates, and sorts server ids so the stored
// EPHEMERA_MCP_SERVERS line is stable regardless of request order.
func normalizeMCPServers(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// handleConfigProfileMCP serves GET/PUT /config/profiles/{name}/mcp — the
// profile's MCP server binding. The binding INTERSECTS with the servers.yaml
// `profiles:` allow-list (it can only narrow access, never widen it). GET returns
// the current binding (`servers: null`, `bound: false` when unset); PUT validates
// each id against the configured backends and persists EPHEMERA_MCP_SERVERS. The
// PolicyStore reads it per request, so edits take effect immediately for future
// tool calls; like builtins/system there is no in-use (409) guard.
func (cp *ControlPlane) handleConfigProfileMCP(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || name == ".." || strings.ContainsAny(name, "/\\") {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("profile name required"))
		return
	}
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(path)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		servers, bound := parseProfileMCPServers(data)
		if !bound {
			servers = nil
		}
		writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "bound": bound})
	case http.MethodPut:
		var body struct {
			Servers []string `json:"servers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body"))
			return
		}
		if body.Servers == nil {
			body.Servers = []string{}
		}
		// Validate each id against the configured backends when the gateway is on.
		// (Disabled gateway → no registry to check against; the binding is still
		// persisted so it applies once the gateway is enabled.)
		if cp.mcpRegistry != nil {
			for _, id := range body.Servers {
				if _, ok := cp.mcpRegistry.Backend(strings.TrimSpace(id)); !ok {
					writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unknown MCP server %q", id))
					return
				}
			}
		}
		norm := normalizeMCPServers(body.Servers)
		if err := cp.writeProfileMCPServers(name, norm); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile mcp binding updated", "profile", name, "servers", strings.Join(norm, ","))
		writeJSON(w, http.StatusOK, map[string]any{"servers": norm, "bound": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}
