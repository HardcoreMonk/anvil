package vm

import "testing"

func TestResolveMachineSizeDefaultsNonPositiveValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  VMConfig
	}{
		{name: "zero", cfg: VMConfig{}},
		{name: "negative", cfg: VMConfig{VcpuCount: -1, MemSizeMib: -512}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vcpu, mem := resolveMachineSize(tc.cfg)
			if vcpu != defaultVcpuCount || mem != defaultMemSizeMib {
				t.Fatalf("resolveMachineSize() = (%d, %d), want (%d, %d)", vcpu, mem, defaultVcpuCount, defaultMemSizeMib)
			}
		})
	}
}

func TestResolveMachineSizePreservesPositiveValues(t *testing.T) {
	vcpu, mem := resolveMachineSize(VMConfig{VcpuCount: 4, MemSizeMib: 4096})
	if vcpu != 4 || mem != 4096 {
		t.Fatalf("resolveMachineSize() = (%d, %d), want (4, 4096)", vcpu, mem)
	}
}
