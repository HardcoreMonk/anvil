package mcpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// auditOfKind returns the records of one Kind, so assertions about call records
// are not disturbed by the "list" records the */list handlers emit.
func auditOfKind(recs []AuditRecord, kind string) []AuditRecord {
	var out []AuditRecord
	for _, r := range recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// TestGateway_ListsAreAudited is the M6 guard for "the */list fan-out is
// auditable." tools/list, resources/list and prompts/list each reach every
// backend the caller's profile allows, using host-side credentials the VM never
// sees. Before this, those legs were the only backend traffic the Observe hook
// never saw — a compromised VM could enumerate every backend catalog leaving no
// trace in audit/mcp.jsonl. Each backend leg must produce one metadata-only
// record naming the backend it contacted.
func TestGateway_ListsAreAudited(t *testing.T) {
	for _, tc := range []struct {
		method string
	}{
		{"tools/list"},
		{"resources/list"},
		{"prompts/list"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			m := newMockMCP(t, false)
			reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
			var audited []AuditRecord
			g := New(Options{
				Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
				Registry: reg,
				Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
			})

			if r := doRPC(t, g, tc.method, map[string]any{}); r.Error != nil {
				t.Fatalf("%s returned an error: %v", tc.method, r.Error)
			}

			lists := auditOfKind(audited, "list")
			if len(lists) != 1 {
				t.Fatalf("%s produced %d list audit records, want 1 (one per backend leg); all records: %+v", tc.method, len(lists), audited)
			}
			rec := lists[0]
			if rec.VMID != "vm1" || rec.Profile != "leader" || rec.Server != "mock" || rec.Tool != tc.method || !rec.OK {
				t.Fatalf("%s list audit record wrong: %+v", tc.method, rec)
			}
		})
	}
}

// TestListAuditRecordIsMetadataOnly extends the audit privacy invariant to the
// new "list" records: a catalog is response payload, and the audit log must stay
// metadata-only. The backend below stuffs a sentinel into every tool name,
// resource URI and prompt name it advertises; the sentinel round-trips into the
// gateway's response, so its absence from the serialized audit record is a
// meaningful check.
func TestListAuditRecordIsMetadataOnly(t *testing.T) {
	const sentinel = "SENTINEL-CATALOG-DO-NOT-AUDIT-91bd"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sentinel-session")
			writeJSONResp(w, req.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "sentinel", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeJSONResp(w, req.ID, map[string]any{"tools": []map[string]any{
				{"name": sentinel, "description": "desc " + sentinel},
			}})
		case "resources/list":
			writeJSONResp(w, req.ID, map[string]any{"resources": []map[string]any{
				{"uri": "file:///" + sentinel, "name": sentinel},
			}})
		case "prompts/list":
			writeJSONResp(w, req.ID, map[string]any{"prompts": []map[string]any{
				{"name": sentinel, "description": sentinel},
			}})
		default:
			writeJSONResp(w, req.ID, map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)

	for _, method := range []string{"tools/list", "resources/list", "prompts/list"} {
		t.Run(method, func(t *testing.T) {
			reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: srv.URL, Profiles: []string{"leader"}}, nil)
			var audited []AuditRecord
			g := New(Options{
				Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
				Registry: reg,
				Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
			})

			resp := doRPC(t, g, method, map[string]any{})
			if resp.Error != nil {
				t.Fatalf("%s returned an error: %v", method, resp.Error)
			}
			// Precondition: the sentinel really is in the catalog the gateway
			// returned, so its absence below is not vacuous.
			if !strings.Contains(string(resp.Result), sentinel) {
				t.Fatalf("precondition failed: %s result did not carry the sentinel: %s", method, resp.Result)
			}

			lists := auditOfKind(audited, "list")
			if len(lists) != 1 {
				t.Fatalf("%s produced %d list audit records, want 1", method, len(lists))
			}
			blob, err := json.Marshal(lists[0])
			if err != nil {
				t.Fatalf("marshal audit record: %v", err)
			}
			if strings.Contains(string(blob), sentinel) {
				t.Fatalf("%s list audit record leaked catalog contents: %s", method, blob)
			}
		})
	}
}

// TestGateway_ListsConsultLimiter pins that a */list backend leg draws from the
// same per-(VM, server) token bucket as tools/call, resources/read and
// prompts/get. A list is an amplifier — one guest request becomes one request to
// every allowed backend, on host credentials — so leaving it unmetered would let
// a compromised VM sidestep the budget the limiter exists to enforce, exactly the
// bypass TestGateway_ResourcesAndPromptsShareRateLimit already forbids for reads
// and prompt fetches. A throttled backend is skipped (and audited), the same
// degradation an unreachable backend already produces.
func TestGateway_ListsConsultLimiter(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)

	lim := NewTokenBucketLimiter(1, 1) // one token, frozen clock → no refill
	clock := time.Unix(1000, 0)
	lim.now = func() time.Time { return clock }

	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Limiter:  lim,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})

	first := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	_ = json.Unmarshal(first.Result, &lr)
	if len(lr.Tools) != 2 {
		t.Fatalf("first tools/list returned %d tools, want 2", len(lr.Tools))
	}

	second := doRPC(t, g, "tools/list", map[string]any{})
	var lr2 toolsListResult
	_ = json.Unmarshal(second.Result, &lr2)
	if len(lr2.Tools) != 0 {
		t.Fatalf("second tools/list returned %d tools; the (vm1, mock) bucket was drained by the first, so the backend leg must be skipped", len(lr2.Tools))
	}

	last := audited[len(audited)-1]
	if last.Kind != "list" || last.OK || last.Err != "rate limited" || last.Server != "mock" {
		t.Fatalf("rate-limited list audit record wrong: %+v", last)
	}
}
