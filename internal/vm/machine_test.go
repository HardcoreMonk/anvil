package vm

import (
	"os"
	"syscall"
	"testing"
)

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

func TestForwardSignalsRestrictedToAbnormalExitSignals(t *testing.T) {
	counts := make(map[os.Signal]int)
	for _, sig := range forwardSignals {
		counts[sig]++
	}

	for _, sig := range []os.Signal{syscall.SIGQUIT, syscall.SIGABRT} {
		if counts[sig] != 1 {
			t.Fatalf("forwardSignals contains %v %d times, want exactly once; signals=%v", sig, counts[sig], forwardSignals)
		}
	}
	for _, sig := range []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM} {
		if counts[sig] != 0 {
			t.Fatalf("forwardSignals contains forbidden %v; signals=%v", sig, forwardSignals)
		}
	}
	for sig, count := range counts {
		if count > 1 {
			t.Fatalf("forwardSignals contains duplicate %v; signals=%v", sig, forwardSignals)
		}
	}
}
