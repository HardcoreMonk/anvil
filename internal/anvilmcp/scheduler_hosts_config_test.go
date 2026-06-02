package anvilmcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSchedulerHostsFileParsesHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	writeTestFile(t, path, `{"hosts":[{"name":"host-a","endpoint":"http://127.0.0.1:3000","healthy":true,"available_vms":2,"available_snapshot_bytes":4096,"egress_policies":[" PROFILE ","Deny_All"],"smoke_only":true}]}`)

	hosts, err := LoadSchedulerHostsFile(path)
	if err != nil {
		t.Fatalf("LoadSchedulerHostsFile: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts len = %d, want 1", len(hosts))
	}
	host := hosts[0]
	if host.Name != "host-a" || host.Endpoint != "http://127.0.0.1:3000" || !host.SmokeOnly {
		t.Fatalf("host = %+v", host)
	}
	if !host.Healthy {
		t.Fatal("Healthy = false, want true")
	}
	if len(host.EgressPolicies) != 2 || host.EgressPolicies[0] != EgressPolicyProfile || host.EgressPolicies[1] != EgressPolicyDenyAll {
		t.Fatalf("egress policies = %+v", host.EgressPolicies)
	}
	if host.AvailableVMs != 2 || host.AvailableSnapshotBytes != 4096 {
		t.Fatalf("host capacity = %d/%d, want 2/4096", host.AvailableVMs, host.AvailableSnapshotBytes)
	}
}

func TestLoadSchedulerHostsFileRejectsInvalidHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	writeTestFile(t, path, `{"hosts":[{"name":"","endpoint":"http://127.0.0.1:3000"}]}`)

	if _, err := LoadSchedulerHostsFile(path); err == nil {
		t.Fatal("LoadSchedulerHostsFile error = nil, want invalid host error")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
