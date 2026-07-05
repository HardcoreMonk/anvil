package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInjectVMFiles_MCPGateway verifies the v0.6.0 MCP injection: the gateway URL
// lands at /root/.ephemera-mcp and the hosts entry is APPENDED to /etc/hosts
// without clobbering the image's existing localhost line. Mount-free.
func TestInjectVMFiles_MCPGateway(t *testing.T) {
	mnt := t.TempDir()
	srcCfg := filepath.Join(mnt, "cfg.yaml")
	srcSec := filepath.Join(mnt, "sec.yaml")
	if err := os.WriteFile(srcCfg, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcSec, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	// Mimic the golden image's pre-existing /etc/hosts.
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "etc", "hosts"), []byte("127.0.0.1\tlocalhost goose-agent\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := injectVMFiles(mnt, VMPrepareOptions{
		HostConfigPath:       srcCfg,
		HostSecretsPath:      srcSec,
		MCPGatewayURL:        "http://ephemera-gw:3001/mcp",
		MCPGatewayHostsEntry: "10.0.1.1 ephemera-gw",
	}); err != nil {
		t.Fatalf("injectVMFiles: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(mnt, "root", ".ephemera-mcp"))
	if err != nil {
		t.Fatalf("read .ephemera-mcp: %v", err)
	}
	if string(got) != "http://ephemera-gw:3001/mcp" {
		t.Errorf(".ephemera-mcp = %q", string(got))
	}

	hosts, err := os.ReadFile(filepath.Join(mnt, "etc", "hosts"))
	if err != nil {
		t.Fatalf("read /etc/hosts: %v", err)
	}
	if !strings.Contains(string(hosts), "127.0.0.1\tlocalhost goose-agent") {
		t.Errorf("/etc/hosts lost the localhost line:\n%s", hosts)
	}
	if !strings.Contains(string(hosts), "10.0.1.1 ephemera-gw") {
		t.Errorf("/etc/hosts missing gateway entry:\n%s", hosts)
	}
}

// TestInjectVMFiles_NoMCPWhenEmpty keeps the gateway-off path clean: no
// .ephemera-mcp file is written when the URL is empty.
func TestInjectVMFiles_NoMCPWhenEmpty(t *testing.T) {
	mnt := t.TempDir()
	srcCfg := filepath.Join(mnt, "cfg.yaml")
	srcSec := filepath.Join(mnt, "sec.yaml")
	if err := os.WriteFile(srcCfg, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcSec, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := injectVMFiles(mnt, VMPrepareOptions{HostConfigPath: srcCfg, HostSecretsPath: srcSec}); err != nil {
		t.Fatalf("injectVMFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mnt, "root", ".ephemera-mcp")); !os.IsNotExist(err) {
		t.Errorf("expected no .ephemera-mcp file, stat err = %v", err)
	}
}
