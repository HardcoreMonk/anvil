package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVMRestoreResultRedactionContract locks the v0.4.5 restore-response contract
// (brief Step 2): a restore response MAY carry source_snapshot_id, tenant_id,
// egress_policy, profile, vm_id, guest_ip, agent_url — and MUST NOT carry any
// agent token / bearer credential. VMRestoreResult embeds VMInfo (which has no
// token field) plus SourceSnapshotID, so this guards against a future field that
// would re-leak a token even though the recovery record persists one privately.
func TestVMRestoreResultRedactionContract(t *testing.T) {
	data, err := json.Marshal(VMRestoreResult{
		VMInfo: VMInfo{
			VMID:         "vm-restored",
			GuestIP:      "10.0.1.9",
			AgentURL:     "http://10.0.1.9:8080",
			Profile:      "dev",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		},
		SourceSnapshotID: "snap-1",
	})
	if err != nil {
		t.Fatalf("marshal restore result: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`"source_snapshot_id":"snap-1"`,
		`"tenant_id":"tenant-1"`,
		`"egress_policy":"profile"`,
		`"profile":"dev"`,
		`"vm_id":"vm-restored"`,
		`"guest_ip":"10.0.1.9"`,
		`"agent_url":"http://10.0.1.9:8080"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("restore result missing %s: %s", want, body)
		}
	}
	for _, forbidden := range []string{"agent_token", "agent_tokens", "Authorization", "Bearer"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("restore result leaks %q: %s", forbidden, body)
		}
	}
}
