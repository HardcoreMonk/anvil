package main

import (
	"fmt"
	"net/http"
	"os"
)

// ProviderDef describes a known LLM provider that Goose can target. The id is the
// value written to GOOSE_PROVIDER; SecretEnv is the key the provider's API token
// is stored under in the global keychain (configs/goose-secrets.yaml). DefaultModel
// and SuggestedModels drive the Settings UI's model field (datalist + the value
// pre-filled when a provider is chosen). This registry is the single source of
// truth for which providers the UI may offer and for validating profile writes.
type ProviderDef struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	SecretEnv       string   `json:"-"` // never serialized; identifies the keychain entry only
	DefaultModel    string   `json:"default_model"`
	SuggestedModels []string `json:"suggested_models"`
}

// providerRegistry is intentionally a slice (not a map) so the UI renders
// providers in a stable, curated order. To add a provider, append here — the
// /config/providers endpoint and profile create/update validation pick it up
// automatically.
var providerRegistry = []ProviderDef{
	{
		ID:              "google",
		Label:           "Google",
		SecretEnv:       "GOOGLE_API_KEY",
		DefaultModel:    "gemini-2.5-flash",
		SuggestedModels: []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash"},
	},
	{
		ID:              "anthropic",
		Label:           "Anthropic",
		SecretEnv:       "ANTHROPIC_API_KEY",
		DefaultModel:    "claude-sonnet-4-6",
		SuggestedModels: []string{"claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5"},
	},
	{
		ID:              "openai",
		Label:           "OpenAI",
		SecretEnv:       "OPENAI_API_KEY",
		DefaultModel:    "gpt-4o",
		SuggestedModels: []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1"},
	},
	{
		ID:        "groq",
		Label:     "Groq",
		SecretEnv: "GROQ_API_KEY",
		// gpt-oss (OpenAI open models) emit standard OpenAI-format tool calls Groq
		// accepts reliably. The Llama models' non-standard tool-call format makes Groq
		// return "Failed to call a function" on tool-heavy agents, so they are offered
		// but not the default.
		DefaultModel:    "openai/gpt-oss-120b",
		SuggestedModels: []string{"openai/gpt-oss-120b", "openai/gpt-oss-20b", "llama-3.3-70b-versatile", "llama-3.1-8b-instant"},
	},
}

// providerByID returns the registry entry for a provider id, if known.
func providerByID(id string) (ProviderDef, bool) {
	for _, p := range providerRegistry {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderDef{}, false
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
