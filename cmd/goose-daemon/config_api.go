package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProfileConfig is the UI-facing view of a profile's editable LLM settings.
// Only provider/model are exposed; API keys (goose-secrets.yaml) stay
// server-side and are never read or written through this surface.
type ProfileConfig struct {
	Name     string `json:"name"`     // "default" for the daemon default, else the profile dir name
	Provider string `json:"provider"` // GOOSE_PROVIDER
	Model    string `json:"model"`    // GOOSE_MODEL
}

// defaultProfileName is the reserved name for the daemon's default goose.yaml
// (configs/goose.yaml). It maps to an empty profile at spawn time.
const defaultProfileName = "default"

// gooseConfigPathForProfile resolves a profile name to its goose.yaml path for
// config read/write. "default"/"" → the daemon's default config; otherwise a
// validated configs/profiles/{name}/goose.yaml. It never touches secrets.
func (cp *ControlPlane) gooseConfigPathForProfile(name string) (string, error) {
	if name == "" || name == defaultProfileName {
		return cp.gooseConfigPath, nil
	}
	// Reject path traversal before any filesystem access.
	if strings.ContainsAny(name, "/\\") || name == ".." {
		return "", fmt.Errorf("invalid profile name: %q", name)
	}
	path := filepath.Join(cp.workDir, "configs", "profiles", name, "goose.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", fmt.Errorf("profile %q not found", name)
	}
	return path, nil
}

// handleConfigProfiles serves GET /config/profiles — the list of editable
// profiles (the default config plus every configs/profiles/* with a goose.yaml).
func (cp *ControlPlane) handleConfigProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	out := []ProfileConfig{}
	// Default config first.
	if pc, err := cp.readProfileConfig(defaultProfileName); err == nil {
		out = append(out, pc)
	}
	// On-disk profiles, sorted by directory name for a stable UI order.
	dir := filepath.Join(cp.workDir, "configs", "profiles")
	entries, _ := os.ReadDir(dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name, "goose.yaml")); err != nil {
			continue
		}
		if pc, err := cp.readProfileConfig(name); err == nil {
			out = append(out, pc)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleConfigProfile serves GET/PUT /config/profiles/{name}. PUT updates the
// profile's GOOSE_PROVIDER and GOOSE_MODEL; the change applies to future VM
// spawns (config is injected into each VM's rootfs at spawn time).
func (cp *ControlPlane) handleConfigProfile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/config/profiles/")
	if name == "" || strings.Contains(name, "/") {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("profile name required"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		pc, err := cp.readProfileConfig(name)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, pc)
	case http.MethodPut:
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body"))
			return
		}
		provider := strings.TrimSpace(body.Provider)
		model := strings.TrimSpace(body.Model)
		if err := validateConfigValue("provider", provider); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := validateConfigValue("model", model); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := cp.writeProfileConfig(name, provider, model); err != nil {
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "invalid") {
				writeJSONError(w, http.StatusBadRequest, err)
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile config updated", "profile", name, "provider", provider, "model", model)
		writeJSON(w, http.StatusOK, ProfileConfig{Name: name, Provider: provider, Model: model})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}

// readProfileConfig parses GOOSE_PROVIDER and GOOSE_MODEL out of a profile's
// goose.yaml. Missing keys come back as empty strings.
func (cp *ControlPlane) readProfileConfig(name string) (ProfileConfig, error) {
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		return ProfileConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ProfileConfig{}, err
	}
	provider, model := parseGooseConfig(data)
	return ProfileConfig{Name: name, Provider: provider, Model: model}, nil
}

// parseGooseConfig extracts GOOSE_PROVIDER and GOOSE_MODEL from goose.yaml bytes.
func parseGooseConfig(data []byte) (provider, model string) {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := topLevelScalar(line)
		if !ok {
			continue
		}
		switch key {
		case "GOOSE_PROVIDER":
			provider = val
		case "GOOSE_MODEL":
			model = val
		}
	}
	return provider, model
}

// readGooseConfigFile reads provider/model from a goose.yaml path, best-effort
// (a missing file or keys yield empty strings). Used to record a VM's model at
// spawn time so the UI can show what each running VM is actually using.
func readGooseConfigFile(path string) (provider, model string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	return parseGooseConfig(data)
}

// writeProfileConfig rewrites the GOOSE_PROVIDER and GOOSE_MODEL lines in a
// profile's goose.yaml in place, preserving comments, ordering, and the nested
// extensions block. Missing keys are appended. The write is atomic (temp+rename).
func (cp *ControlPlane) writeProfileConfig(name, provider, model string) error {
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	setProvider, setModel := false, false
	for i, line := range lines {
		key, _, ok := topLevelScalar(line)
		if !ok {
			continue
		}
		switch key {
		case "GOOSE_PROVIDER":
			lines[i] = "GOOSE_PROVIDER: " + yamlScalar(provider)
			setProvider = true
		case "GOOSE_MODEL":
			lines[i] = "GOOSE_MODEL: " + yamlScalar(model)
			setModel = true
		}
	}
	if !setProvider {
		lines = append(lines, "GOOSE_PROVIDER: "+yamlScalar(provider))
	}
	if !setModel {
		lines = append(lines, "GOOSE_MODEL: "+yamlScalar(model))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// topLevelScalar parses a top-level `KEY: value` line. It skips blank lines,
// comments, and indented lines (so nested keys under `extensions:` are ignored).
// The returned value has surrounding quotes and whitespace stripped.
func topLevelScalar(line string) (key, val string, ok bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return "", "", false
	}
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return "", "", false
	}
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	val = strings.TrimSpace(line[idx+1:])
	val = strings.Trim(val, `"'`)
	return key, val, true
}

// yamlScalar renders a value safe to write after `KEY: `. Simple tokens
// (provider/model names) are written bare; anything else is double-quoted with
// backslashes and quotes escaped. Callers must reject newlines first.
func yamlScalar(v string) string {
	safe := v != ""
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' || r == '/') {
			safe = false
			break
		}
	}
	if safe {
		return v
	}
	esc := strings.ReplaceAll(v, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return `"` + esc + `"`
}

// validateConfigValue guards against empty values and YAML injection via
// embedded newlines (which could smuggle in extra keys).
func validateConfigValue(field, v string) error {
	if v == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if len(v) > 200 {
		return fmt.Errorf("%s is too long", field)
	}
	if strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("%s must be a single line", field)
	}
	return nil
}
