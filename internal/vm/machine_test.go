package vm

import "testing"

// TestResolveMachineSize_Defaults pins the fallback behavior: a VMConfig with zero
// sizing fields gets the package defaults (the "Standard" preset: 1 vCPU / 1024 MiB),
// while explicit values pass through and each field falls back independently.
func TestResolveMachineSize_Defaults(t *testing.T) {
	if v, m := resolveMachineSize(VMConfig{}); v != 1 || m != 1024 {
		t.Errorf("default = %d/%d, want 1/1024", v, m)
	}
	if v, m := resolveMachineSize(VMConfig{VcpuCount: 2, MemSizeMib: 2048}); v != 2 || m != 2048 {
		t.Errorf("explicit = %d/%d, want 2/2048", v, m)
	}
	if v, m := resolveMachineSize(VMConfig{VcpuCount: 4}); v != 4 || m != 1024 {
		t.Errorf("mixed (mem unset) = %d/%d, want 4/1024", v, m)
	}
	if v, m := resolveMachineSize(VMConfig{MemSizeMib: 512}); v != 1 || m != 512 {
		t.Errorf("mixed (vcpu unset) = %d/%d, want 1/512", v, m)
	}
}
