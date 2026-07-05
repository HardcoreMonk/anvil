package mcpgateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestAuditRecordExcludesArgsAndResults is the Phase 3 (v0.6.0) boundary guard for
// "Gateway audit/mcp.jsonl stores metadata only, never args or results." It drives
// a real tools/call through the gateway with a sentinel argument against a backend
// (mockMCP) that echoes both the call name AND the arguments into its result, then
// captures the AuditRecord the gateway hands to the Observe hook. The record —
// serialized in full — must contain neither the sentinel argument nor the
// backend's (sentinel-bearing) result payload.
func TestAuditRecordExcludesArgsAndResults(t *testing.T) {
	const sentinelArg = "SENTINEL-ARG-DO-NOT-AUDIT-7f3a"

	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL}, nil)

	var captured []AuditRecord
	g := New(Options{
		Resolver: stubResolver{c: Caller{VMID: "vm-1", Profile: "worker"}},
		Registry: reg,
		Observe:  func(r AuditRecord) { captured = append(captured, r) },
	})

	// A successful namespaced tools/call carrying the sentinel in its arguments.
	resp := doRPC(t, g, "tools/call", toolsCallParams{
		Name:      "mock__echo",
		Arguments: json.RawMessage(`{"secret":"` + sentinelArg + `"}`),
	})
	// Sanity: the backend really did echo the sentinel back in the RESULT, so the
	// absence check below is meaningful (the sentinel round-tripped through both
	// args and result, yet must not appear in the audit record).
	if !strings.Contains(string(resp.Result), sentinelArg) {
		t.Fatalf("precondition failed: backend result did not echo the sentinel: %s", resp.Result)
	}

	if len(captured) != 1 {
		t.Fatalf("Observe called %d times, want 1", len(captured))
	}
	rec := captured[0]
	if rec.Tool != "echo" || rec.Server != "mock" || rec.VMID != "vm-1" || rec.Profile != "worker" || !rec.OK {
		t.Fatalf("audit record metadata wrong: %+v", rec)
	}

	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal audit record: %v", err)
	}
	if strings.Contains(string(blob), sentinelArg) {
		t.Fatalf("audit record leaked the tool-call argument/result: %s", blob)
	}
}

// TestAuditRecordHasNoArgOrResultFields is a structural lock: the AuditRecord type
// must never grow a field that could carry tool-call arguments, results, params,
// or response payloads. Adding such a field fails this guard, forcing a review of
// the metadata-only invariant.
func TestAuditRecordHasNoArgOrResultFields(t *testing.T) {
	forbidden := []string{"arg", "result", "param", "response", "payload", "content", "output", "input", "body"}
	rt := reflect.TypeOf(AuditRecord{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("AuditRecord field %q may carry payload data (%q); the gateway audit record must stay metadata-only", rt.Field(i).Name, bad)
			}
		}
	}
}
