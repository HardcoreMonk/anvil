package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEgressProfileAndPlanAllowlistCommands(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "anthropic")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatalf("mkdir profile dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "egress.json"), []byte(`{
  "allow_cidrs": ["203.0.113.10/32"],
  "allow_hosts": ["api.anthropic.com"],
  "dns_servers": ["1.1.1.1"]
}`), 0600); err != nil {
		t.Fatalf("write egress profile: %v", err)
	}

	profile, ok, err := loadEgressProfile(dir, "anthropic")
	if err != nil {
		t.Fatalf("loadEgressProfile error = %v", err)
	}
	if !ok {
		t.Fatal("loadEgressProfile ok = false, want true")
	}
	commands, err := planProfileEgressCommands("vm-1", "10.0.1.10", profile)
	if err != nil {
		t.Fatalf("planProfileEgressCommands error = %v", err)
	}
	joined := joinCommands(commands)
	for _, want := range []string{
		"iptables -I FORWARD -s 10.0.1.10 -j REJECT -m comment --comment anvil-egress-vm-1-default",
		"iptables -I FORWARD -s 10.0.1.10 -d 203.0.113.10/32 -j ACCEPT -m comment --comment anvil-egress-vm-1-cidr-0",
		"iptables -I FORWARD -s 10.0.1.10 -m string --string api.anthropic.com --algo bm -j ACCEPT -m comment --comment anvil-egress-vm-1-host-0",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestPlanProfileEgressCommandsEnforcesDNSAllowlist(t *testing.T) {
	commands, err := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{
		DNSServers: []string{"1.1.1.1"},
	})
	if err != nil {
		t.Fatalf("planProfileEgressCommands error = %v", err)
	}
	joined := joinCommands(commands)
	for _, want := range []string{
		"iptables -I FORWARD -s 10.0.1.10 -p udp --dport 53 -j REJECT -m comment --comment anvil-egress-vm-1-dns-deny-udp",
		"iptables -I FORWARD -s 10.0.1.10 -p tcp --dport 53 -j REJECT -m comment --comment anvil-egress-vm-1-dns-deny-tcp",
		"iptables -I FORWARD -s 10.0.1.10 -d 1.1.1.1 -p udp --dport 53 -j ACCEPT -m comment --comment anvil-egress-vm-1-dns-0-udp",
		"iptables -I FORWARD -s 10.0.1.10 -d 1.1.1.1 -p tcp --dport 53 -j ACCEPT -m comment --comment anvil-egress-vm-1-dns-0-tcp",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func joinCommands(commands []egressCommand) string {
	var lines []string
	for _, command := range commands {
		lines = append(lines, command.Name+" "+strings.Join(command.Args, " "))
	}
	return strings.Join(lines, "\n")
}
