package main

import "testing"

func TestIsPlaceholderSecret(t *testing.T) {
	cases := map[string]bool{
		"":                     true,
		"   ":                  true,
		"your-key-here":        true,
		"sk-ant-your-key-here": true,
		"sk-your-key-here":     true,
		"AIzaSyRealKey123":     false,
		"sk-ant-abc123def":     false,
		"gsk_realgroqkey":      false,
	}
	for in, want := range cases {
		if got := isPlaceholderSecret(in); got != want {
			t.Errorf("isPlaceholderSecret(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSecretKeyPresent(t *testing.T) {
	data := []byte("GOOGLE_API_KEY: \"AIzaReal\"\n# ANTHROPIC_API_KEY: \"sk-ant-your-key-here\"\nGROQ_API_KEY: \"gsk_real\"\n")
	if !secretKeyPresent(data, "GOOGLE_API_KEY") {
		t.Error("GOOGLE_API_KEY should be present")
	}
	if secretKeyPresent(data, "ANTHROPIC_API_KEY") {
		t.Error("commented ANTHROPIC_API_KEY should be absent")
	}
	if !secretKeyPresent(data, "GROQ_API_KEY") {
		t.Error("GROQ_API_KEY should be present")
	}
	if secretKeyPresent(data, "OPENAI_API_KEY") {
		t.Error("missing OPENAI_API_KEY should be absent")
	}
}

func TestProviderRegistry_Groq(t *testing.T) {
	p, ok := providerByID("groq")
	if !ok {
		t.Fatal("groq not in registry")
	}
	if p.DefaultModel != "openai/gpt-oss-120b" {
		t.Errorf("groq default model = %q, want openai/gpt-oss-120b", p.DefaultModel)
	}
	if p.SecretEnv != "GROQ_API_KEY" {
		t.Errorf("groq secret env = %q, want GROQ_API_KEY", p.SecretEnv)
	}
	if len(p.SuggestedModels) == 0 {
		t.Error("groq should suggest at least one model")
	}
}

func TestValidateProfileName(t *testing.T) {
	valid := []string{"myprofile", "a", "fast-worker", "team_1", "x9"}
	for _, n := range valid {
		if err := validateProfileName(n); err != nil {
			t.Errorf("validateProfileName(%q) = %v, want nil", n, err)
		}
	}
	invalid := []string{"", "default", "..", "../evil", "a/b", `a\b`, "Has Space", "UPPER", "-leading"}
	for _, n := range invalid {
		if err := validateProfileName(n); err == nil {
			t.Errorf("validateProfileName(%q) = nil, want error", n)
		}
	}
}
