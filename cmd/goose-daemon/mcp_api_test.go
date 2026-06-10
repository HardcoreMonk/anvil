package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleConfigMCP_Disabled(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigMCP(rr, httptest.NewRequest(http.MethodGet, "/config/mcp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false when gateway off", got["enabled"])
	}
	if got["server_count"] != float64(0) {
		t.Fatalf("server_count = %v, want 0", got["server_count"])
	}
}

func TestHandleConfigMCPServers_DisabledEmpty(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigMCPServers(rr, httptest.NewRequest(http.MethodGet, "/config/mcp/servers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var list []any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty server list when gateway off, got %d", len(list))
	}
}

func TestHandleConfigMCP_MethodGuards(t *testing.T) {
	cp := newTestCP(t)
	for _, h := range []http.HandlerFunc{cp.handleConfigMCP, cp.handleConfigMCPServers} {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodPost, "/config/mcp", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d, want 405", rr.Code)
		}
	}
}

func TestLookupVMByIP(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-a"] = &runningVM{VMInfo: VMInfo{VMID: "vm-a", GuestIP: "10.0.1.5", Profile: "leader"}}

	if id, profile, ok := cp.lookupVMByIP("10.0.1.5"); !ok || id != "vm-a" || profile != "leader" {
		t.Fatalf("lookup = %q/%q/%v, want vm-a/leader/true", id, profile, ok)
	}
	if _, _, ok := cp.lookupVMByIP("10.0.1.99"); ok {
		t.Fatal("unknown IP should not resolve")
	}
}
