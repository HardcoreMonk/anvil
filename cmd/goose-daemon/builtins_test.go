package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseProfileBuiltins(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		// A missing key returns nil — callers treat that as "use the default".
		{"missing key", "GOOSE_PROVIDER: groq\n", nil},
		{"single", "EPHEMERA_BUILTINS: developer\n", []string{"developer"}},
		{"multiple", "EPHEMERA_BUILTINS: developer,memory\n", []string{"developer", "memory"}},
		{"spaces trimmed", "EPHEMERA_BUILTINS: developer , memory \n", []string{"developer", "memory"}},
		// An explicit empty value is a non-nil empty slice: "no builtins".
		{"explicit empty", "EPHEMERA_BUILTINS:\n", []string{}},
		// Indented (nested) keys must be ignored, like the extensions block.
		{"ignores nested", "extensions:\n  EPHEMERA_BUILTINS: nope\n", nil},
	}
	for _, c := range cases {
		got := parseProfileBuiltins([]byte(c.yaml))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parseProfileBuiltins = %#v, want %#v", c.name, got, c.want)
		}
	}
}

func TestValidateBuiltins(t *testing.T) {
	if err := validateBuiltins([]string{"developer", "memory"}); err != nil {
		t.Errorf("known builtins rejected: %v", err)
	}
	if err := validateBuiltins([]string{}); err != nil {
		t.Errorf("empty list should be valid: %v", err)
	}
	if err := validateBuiltins([]string{"developer", "rm -rf /"}); err == nil {
		t.Error("unknown builtin should be rejected")
	}
}

func TestNormalizeBuiltins(t *testing.T) {
	// De-duplicates and re-orders to the registry's canonical order.
	got := normalizeBuiltins([]string{"memory", "developer", "memory"})
	if !reflect.DeepEqual(got, []string{"developer", "memory"}) {
		t.Errorf("normalizeBuiltins = %#v, want [developer memory]", got)
	}
}

func TestRenderGooseProfileYAML_Builtins(t *testing.T) {
	out := renderGooseProfileYAML("groq", "llama", 0, 0, []string{"developer", "memory"})
	if !strings.Contains(out, "EPHEMERA_BUILTINS: developer,memory") {
		t.Errorf("EPHEMERA_BUILTINS line missing:\n%s", out)
	}
	// The inert extensions block mirrors the selected builtins.
	for _, want := range []string{"  developer:", "  memory:", "type: builtin"} {
		if !strings.Contains(out, want) {
			t.Errorf("extensions block missing %q:\n%s", want, out)
		}
	}
	// No builtins → no dangling extensions block.
	none := renderGooseProfileYAML("groq", "llama", 0, 0, []string{})
	if !strings.Contains(none, "EPHEMERA_BUILTINS: \n") && !strings.HasSuffix(none, "EPHEMERA_BUILTINS: \n") {
		t.Errorf("empty builtins should still write the (empty) key:\n%s", none)
	}
	if strings.Contains(none, "extensions:") {
		t.Errorf("empty builtins should omit the extensions block:\n%s", none)
	}
}

func TestReadProfileConfig_BuiltinsEffectiveDefault(t *testing.T) {
	cp := newTestCP(t)
	// A profile with no EPHEMERA_BUILTINS key reports the effective runtime default.
	writeProfileFixture(t, cp, "legacy", "GOOSE_PROVIDER: groq\nGOOSE_MODEL: llama\n")
	pc, err := cp.readProfileConfig("legacy")
	if err != nil {
		t.Fatalf("readProfileConfig: %v", err)
	}
	if !reflect.DeepEqual(pc.Builtins, []string{"developer"}) {
		t.Fatalf("Builtins = %#v, want [developer] (effective default)", pc.Builtins)
	}

	// An explicit selection is reported verbatim (canonical order).
	writeProfileFixture(t, cp, "rich", "GOOSE_PROVIDER: groq\nGOOSE_MODEL: llama\nEPHEMERA_BUILTINS: memory,developer\n")
	pc, _ = cp.readProfileConfig("rich")
	if !reflect.DeepEqual(pc.Builtins, []string{"memory", "developer"}) {
		t.Fatalf("Builtins = %#v, want [memory developer]", pc.Builtins)
	}
}

func TestHandleConfigBuiltins_Shape(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigBuiltins(rr, httptest.NewRequest(http.MethodGet, "/config/builtins", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var list []BuiltinInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != len(builtinRegistry) {
		t.Fatalf("got %d builtins, want %d", len(list), len(builtinRegistry))
	}
	if list[0].ID != "developer" || !list[0].Default {
		t.Errorf("expected developer to be first and Default: %+v", list[0])
	}

	// Method guard.
	rr = httptest.NewRecorder()
	cp.handleConfigBuiltins(rr, httptest.NewRequest(http.MethodPost, "/config/builtins", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rr.Code)
	}
}

func TestHandleConfigProfileBuiltins_PutThenGet(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	// PUT a new selection through the router so the /builtins suffix dispatch runs.
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/builtins", strings.NewReader(`{"builtins":["developer","memory"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, path)
	if !strings.Contains(out, "EPHEMERA_BUILTINS: developer,memory") {
		t.Fatalf("EPHEMERA_BUILTINS not written:\n%s", out)
	}
	// The original comments/keys/extensions block must survive the line-based edit.
	for _, want := range []string{"# Goose config", "GOOSE_PROVIDER: google", "extensions:"} {
		if !strings.Contains(out, want) {
			t.Errorf("write dropped %q:\n%s", want, out)
		}
	}

	// GET reflects it.
	rr = httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker/builtins", nil))
	var resp map[string][]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(resp["builtins"], []string{"developer", "memory"}) {
		t.Fatalf("GET builtins = %#v, want [developer memory]", resp["builtins"])
	}
}

func TestHandleConfigProfileBuiltins_RejectsUnknown(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/builtins", strings.NewReader(`{"builtins":["developer","evil"]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(mustRead(t, path), "EPHEMERA_BUILTINS") {
		t.Fatalf("a rejected PUT must not write the key")
	}
}

func TestHandleConfigProfileBuiltins_EmptyMeansNone(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/builtins", strings.NewReader(`{"builtins":[]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(mustRead(t, path), "EPHEMERA_BUILTINS:") {
		t.Fatalf("explicit empty selection should write an empty EPHEMERA_BUILTINS key")
	}
}

func TestHandleConfigProfileBuiltins_PathTraversal(t *testing.T) {
	cp := newTestCP(t)
	for _, p := range []string{"/config/profiles/../evil/builtins", "/config/profiles/a/b/builtins"} {
		rr := httptest.NewRecorder()
		cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, p, strings.NewReader(`{"builtins":["developer"]}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, rr.Code)
		}
	}
}

func TestCreateProfile_WithBuiltins(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles",
		strings.NewReader(`{"name":"mem","provider":"google","builtins":["developer","memory"]}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, cp.workDir+"/configs/profiles/mem/goose.yaml")
	if !strings.Contains(out, "EPHEMERA_BUILTINS: developer,memory") {
		t.Fatalf("builtins not written:\n%s", out)
	}
}

func TestCreateProfile_DefaultsBuiltinsToDeveloper(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	// No builtins field → defaults to the registry default (developer).
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles",
		strings.NewReader(`{"name":"plain","provider":"google"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, cp.workDir+"/configs/profiles/plain/goose.yaml")
	if !strings.Contains(out, "EPHEMERA_BUILTINS: developer") {
		t.Fatalf("expected developer default:\n%s", out)
	}
}
