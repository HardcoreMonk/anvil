package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ProfileConfig is the UI-facing view of a profile's editable LLM settings.
// Only provider/model are exposed; API keys (goose-secrets.yaml) stay
// server-side and are never read or written through this surface.
type ProfileConfig struct {
	Name       string `json:"name"`         // "default" for the daemon default, else the profile dir name
	Provider   string `json:"provider"`     // GOOSE_PROVIDER
	Model      string `json:"model"`        // GOOSE_MODEL
	VcpuCount  int64  `json:"vcpu_count"`   // EPHEMERA_VCPU_COUNT; 0 → default sizing (1)
	MemSizeMib int64  `json:"mem_size_mib"` // EPHEMERA_MEM_SIZE_MIB; 0 → default sizing (1024)
}

// defaultProfileName is the reserved name for the daemon's default goose.yaml
// (configs/goose.yaml). It maps to an empty profile at spawn time.
const defaultProfileName = "default"

// maxSystemPromptBytes caps a profile's system.md. Shipped reference prompts are
// ~1-2.5 KB; 64 KiB is generous headroom while bounding what gets baked into a
// guest rootfs at spawn.
const maxSystemPromptBytes = 64 * 1024

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

// handleConfigProfiles serves GET /config/profiles (list the default config plus
// every configs/profiles/* with a goose.yaml) and POST /config/profiles (create a
// new user-defined profile).
func (cp *ControlPlane) handleConfigProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cp.listProfiles(w, r)
	case http.MethodPost:
		cp.createProfile(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}

// listProfiles writes the editable profile list: the default config first, then
// each on-disk profile sorted by directory name for a stable UI order.
func (cp *ControlPlane) listProfiles(w http.ResponseWriter, r *http.Request) {
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
	// Sub-route /config/profiles/{name}/system manages the profile's system.md.
	// The prefix mux can't express path variables, so peel the suffix here (the
	// handleVM dispatch does the same for /stats, /snapshot, /tasks). Name
	// validation lives inside the sub-handler, mirroring the guard below.
	if strings.HasSuffix(name, "/system") {
		cp.handleConfigProfileSystem(w, r, strings.TrimSuffix(name, "/system"))
		return
	}
	// Reject empty and any path-traversal form before touching the filesystem.
	if name == "" || name == ".." || strings.ContainsAny(name, "/\\") {
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
			Provider   string `json:"provider"`
			Model      string `json:"model"`
			VcpuCount  int64  `json:"vcpu_count"`
			MemSizeMib int64  `json:"mem_size_mib"`
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
		// Provider must be a known one whose API key is present in the keychain.
		if err := cp.validateProviderAvailable(provider); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := validateSizing(body.VcpuCount, body.MemSizeMib); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := cp.writeProfileConfig(name, provider, model, body.VcpuCount, body.MemSizeMib); err != nil {
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "invalid") {
				writeJSONError(w, http.StatusBadRequest, err)
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile config updated", "profile", name, "provider", provider, "model", model, "vcpu", body.VcpuCount, "mem", body.MemSizeMib)
		writeJSON(w, http.StatusOK, ProfileConfig{Name: name, Provider: provider, Model: model, VcpuCount: body.VcpuCount, MemSizeMib: body.MemSizeMib})
	case http.MethodDelete:
		if name == defaultProfileName {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("the default profile cannot be deleted"))
			return
		}
		dir := filepath.Join(cp.workDir, "configs", "profiles", name)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, fmt.Errorf("profile %q not found", name))
			return
		}
		// Refuse to delete a profile that running VMs were spawned from — otherwise a
		// restart / change-role of those agents would silently fall back to the default
		// config. The caller must remove or re-profile those agents first.
		if inUse := cp.vmsUsingProfile(name); len(inUse) > 0 {
			writeJSONError(w, http.StatusConflict, fmt.Errorf("profile %q is in use by %d running VM(s) (%s); remove or change those agents before deleting", name, len(inUse), strings.Join(inUse, ", ")))
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile deleted", "profile", name)
		writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}

// handleConfigProfileSystem serves GET/PUT/DELETE /config/profiles/{name}/system —
// the profile's system.md, injected into every VM spawned from the profile as
// /root/.goose-system-prompt (loadProfileSystemPrompt). Editing it affects only
// FUTURE spawns: a running VM already has the prompt baked into its rootfs, so
// unlike a whole-profile delete this needs no in-use (409) guard.
func (cp *ControlPlane) handleConfigProfileSystem(w http.ResponseWriter, r *http.Request, name string) {
	// Reject empty and any path-traversal form before touching the filesystem
	// (same guard as handleConfigProfile). The default profile has no directory.
	if name == "" || name == ".." || strings.ContainsAny(name, "/\\") {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("profile name required"))
		return
	}
	if name == defaultProfileName {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("the default profile has no system prompt"))
		return
	}
	// A profile exists iff its goose.yaml does; reuse the resolver for the
	// existence check (and its path-traversal guard) before touching system.md.
	if _, err := cp.gooseConfigPathForProfile(name); err != nil {
		writeJSONError(w, http.StatusNotFound, err)
		return
	}
	path := filepath.Join(cp.workDir, "configs", "profiles", name, "system.md")

	switch r.Method {
	case http.MethodGet:
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// No system.md yet — empty, matching loadProfileSystemPrompt.
				writeJSON(w, http.StatusOK, map[string]string{"system_md": ""})
				return
			}
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"system_md": string(b)})
	case http.MethodPut:
		var body struct {
			SystemMd string `json:"system_md"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body"))
			return
		}
		if len(body.SystemMd) > maxSystemPromptBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("system prompt too large (max %d bytes)", maxSystemPromptBytes))
			return
		}
		// Atomic write (temp+rename) so a concurrent spawn never reads a torn file.
		// No newline/YAML-escape check: system.md is a free-form markdown file body,
		// not a value interpolated into a YAML scalar (unlike provider/model).
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(body.SystemMd), 0o644); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile system prompt updated", "profile", name, "bytes", len(body.SystemMd))
		writeJSON(w, http.StatusOK, map[string]string{"system_md": body.SystemMd})
	case http.MethodDelete:
		// Idempotent clear: an already-absent system.md is the desired end state.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		slog.Info("profile system prompt cleared", "profile", name)
		writeJSON(w, http.StatusOK, map[string]string{"deleted": "system.md"})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
	}
}

// vmsUsingProfile returns the IDs of running VMs spawned from the named profile.
// Used to block deletion of an in-use profile (deleting it would leave those agents
// silently falling back to the default config on restart / change-role).
func (cp *ControlPlane) vmsUsingProfile(name string) []string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	var ids []string
	for id, v := range cp.vms {
		if v.Profile == name {
			ids = append(ids, id)
		}
	}
	return ids
}

// providerStatus is one element of the GET /config/providers response: a known
// provider annotated with whether its API key is present in the global keychain.
// Secret values never leave the server — only the availability flag is exposed.
type providerStatus struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Available       bool     `json:"available"`
	DefaultModel    string   `json:"default_model"`
	SuggestedModels []string `json:"suggested_models"`
}

// handleConfigProviders serves GET /config/providers — the registry of known LLM
// providers, each flagged available iff its API key is set (uncommented and not a
// placeholder) in the single global keychain configs/goose-secrets.yaml. The UI
// uses this to restrict the Provider dropdown to providers that can actually run.
func (cp *ControlPlane) handleConfigProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	data, _ := os.ReadFile(cp.gooseSecretsPath) // best-effort: a missing file → all unavailable
	out := make([]providerStatus, 0, len(providerRegistry))
	for _, p := range providerRegistry {
		out = append(out, providerStatus{
			ID:              p.ID,
			Label:           p.Label,
			Available:       secretKeyPresent(data, p.SecretEnv),
			DefaultModel:    p.DefaultModel,
			SuggestedModels: p.SuggestedModels,
		})
	}
	writeJSON(w, http.StatusOK, out)
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

// secretKeyPresent reports whether goose-secrets.yaml content carries a usable
// value for envKey: an uncommented top-level `envKey: value` whose value is not a
// shipped placeholder. topLevelScalar already skips comments and indented lines.
func secretKeyPresent(data []byte, envKey string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := topLevelScalar(line)
		if !ok || key != envKey {
			continue
		}
		return !isPlaceholderSecret(val)
	}
	return false
}

// isPlaceholderSecret recognizes the placeholder values shipped in the
// goose-secrets.yaml.example templates ("your-key-here", "sk-ant-your-key-here",
// "sk-your-key-here") so an unfilled key counts as "not set".
func isPlaceholderSecret(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	low := strings.ToLower(v)
	if strings.Contains(low, "your-key-here") {
		return true
	}
	for _, prefix := range []string{"your-", "sk-your", "sk-ant-your"} {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

// profileNameRe constrains user-defined profile names to a filesystem- and
// URL-safe slug; it also rejects "/", "\", "." and "..".
var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// validateProfileName enforces the slug format and reserves "default".
func validateProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("profile name required")
	}
	if name == defaultProfileName {
		return fmt.Errorf("%q is a reserved profile name", defaultProfileName)
	}
	if len(name) > 64 {
		return fmt.Errorf("profile name too long (max 64)")
	}
	if !profileNameRe.MatchString(name) {
		return fmt.Errorf("profile name must be lowercase letters, digits, '-' or '_'")
	}
	return nil
}

// validateProviderAvailable ensures a provider is known and its API key is present
// in the global keychain. Shared by profile create and update.
func (cp *ControlPlane) validateProviderAvailable(provider string) error {
	def, ok := providerByID(provider)
	if !ok {
		return fmt.Errorf("unknown provider %q", provider)
	}
	data, _ := os.ReadFile(cp.gooseSecretsPath)
	if !secretKeyPresent(data, def.SecretEnv) {
		return fmt.Errorf("provider %q has no API key in goose-secrets.yaml", provider)
	}
	return nil
}

// createProfile handles POST /config/profiles. It creates a new user-defined
// profile directory holding a goose.yaml with the chosen provider/model. Secrets
// are NOT stored per profile — every VM reads the global keychain at spawn time.
// validateSizing checks optional per-profile VM sizing. 0 means "use the default";
// otherwise vCPU is capped at 8 and memory is bounded to a sane 256–16384 MiB.
func validateSizing(vcpu, mem int64) error {
	if vcpu < 0 || vcpu > 8 {
		return fmt.Errorf("vcpu_count out of range (0 = default, max 8)")
	}
	if mem < 0 || mem > 16384 || (mem > 0 && mem < 256) {
		return fmt.Errorf("mem_size_mib out of range (0 = default, otherwise 256–16384)")
	}
	return nil
}

func (cp *ControlPlane) createProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		VcpuCount  int64  `json:"vcpu_count"`
		MemSizeMib int64  `json:"mem_size_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body"))
		return
	}
	name := strings.TrimSpace(body.Name)
	provider := strings.TrimSpace(body.Provider)
	model := strings.TrimSpace(body.Model)
	if err := validateProfileName(name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := cp.validateProviderAvailable(provider); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	// An empty model defaults to the provider's recommended model.
	if model == "" {
		def, _ := providerByID(provider)
		model = def.DefaultModel
	}
	if err := validateConfigValue("model", model); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateSizing(body.VcpuCount, body.MemSizeMib); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	dir := filepath.Join(cp.workDir, "configs", "profiles", name)
	if _, err := os.Stat(dir); err == nil {
		writeJSONError(w, http.StatusConflict, fmt.Errorf("profile %q already exists", name))
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "goose.yaml"), []byte(renderGooseProfileYAML(provider, model, body.VcpuCount, body.MemSizeMib)), 0o644); err != nil {
		os.RemoveAll(dir) // don't leave a half-created profile behind
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	slog.Info("profile created", "profile", name, "provider", provider, "model", model, "vcpu", body.VcpuCount, "mem", body.MemSizeMib)
	writeJSON(w, http.StatusCreated, ProfileConfig{Name: name, Provider: provider, Model: model, VcpuCount: body.VcpuCount, MemSizeMib: body.MemSizeMib})
}

// renderGooseProfileYAML produces a goose.yaml body for a new profile, mirroring
// the structure of the default configs/goose.yaml (telemetry off, keyring off, the
// bundled developer extension). Provider/model are rendered via yamlScalar.
func renderGooseProfileYAML(provider, model string, vcpu, mem int64) string {
	s := "# Goose config for this profile. Provider/model only — API keys live in\n" +
		"# the global configs/goose-secrets.yaml keychain.\n" +
		"GOOSE_PROVIDER: " + yamlScalar(provider) + "\n" +
		"GOOSE_MODEL: " + yamlScalar(model) + "\n" +
		"GOOSE_TELEMETRY_ENABLED: false\n" +
		"GOOSE_DISABLE_KEYRING: true\n"
	// Per-profile VM sizing. goose ignores these unknown keys; the spawn path reads
	// them back (parseProfileSizing) to size the MicroVM. Omitted when 0 (default).
	if vcpu > 0 {
		s += fmt.Sprintf("EPHEMERA_VCPU_COUNT: %d\n", vcpu)
	}
	if mem > 0 {
		s += fmt.Sprintf("EPHEMERA_MEM_SIZE_MIB: %d\n", mem)
	}
	return s + "\n" +
		"extensions:\n" +
		"  developer:\n" +
		"    bundled: true\n" +
		"    enabled: true\n" +
		"    name: developer\n" +
		"    timeout: 300\n" +
		"    type: builtin\n"
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
	vcpu, mem := parseProfileSizing(data)
	return ProfileConfig{Name: name, Provider: provider, Model: model, VcpuCount: vcpu, MemSizeMib: mem}, nil
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

// parseProfileSizing extracts the optional per-profile VM sizing keys from
// goose.yaml bytes. Missing or non-numeric values come back as 0, which the
// spawn path treats as "use default sizing".
func parseProfileSizing(data []byte) (vcpu, mem int64) {
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := topLevelScalar(line)
		if !ok {
			continue
		}
		switch key {
		case "EPHEMERA_VCPU_COUNT":
			vcpu, _ = strconv.ParseInt(val, 10, 64)
		case "EPHEMERA_MEM_SIZE_MIB":
			mem, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	return vcpu, mem
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
func (cp *ControlPlane) writeProfileConfig(name, provider, model string, vcpu, mem int64) error {
	path, err := cp.gooseConfigPathForProfile(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	setProvider, setModel, setVcpu, setMem := false, false, false, false
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
		case "EPHEMERA_VCPU_COUNT":
			// vcpu == 0 means "unspecified": keep the existing line so an update
			// that doesn't carry sizing (e.g. provider/model edit) doesn't reset it.
			if vcpu > 0 {
				lines[i] = fmt.Sprintf("EPHEMERA_VCPU_COUNT: %d", vcpu)
			}
			setVcpu = true
		case "EPHEMERA_MEM_SIZE_MIB":
			if mem > 0 {
				lines[i] = fmt.Sprintf("EPHEMERA_MEM_SIZE_MIB: %d", mem)
			}
			setMem = true
		}
	}
	if !setProvider {
		lines = append(lines, "GOOSE_PROVIDER: "+yamlScalar(provider))
	}
	if !setModel {
		lines = append(lines, "GOOSE_MODEL: "+yamlScalar(model))
	}
	if !setVcpu && vcpu > 0 {
		lines = append(lines, fmt.Sprintf("EPHEMERA_VCPU_COUNT: %d", vcpu))
	}
	if !setMem && mem > 0 {
		lines = append(lines, fmt.Sprintf("EPHEMERA_MEM_SIZE_MIB: %d", mem))
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
