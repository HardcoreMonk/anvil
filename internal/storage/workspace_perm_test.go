package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewProvisionerCreatesWorkspaceDirOwnerOnly pins the workspace directory —
// the one that holds every per-VM rootfs image and COW exception store — to 0700.
//
// The rootfs images carry 0600 secrets *inside* them (LLM provider API keys, the
// guest agent token, the operator control-plane bearer). A world-traversable,
// world-readable workspace hands all of those to any unprivileged local account
// via a single `strings vm-*.ext4`, so the directory mode is the boundary.
func TestNewProvisionerCreatesWorkspaceDirOwnerOnly(t *testing.T) {
	tmp := t.TempDir()
	golden := filepath.Join(tmp, "artifacts", "golden-image.ext4")
	if err := os.MkdirAll(filepath.Dir(golden), 0700); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	// An existing golden image short-circuits EnsureGoldenImage (none of the build
	// inputs exist, so nothing is newer than it) — this test stays about the
	// workspace directory mode and never shells out to the image build script.
	if err := os.WriteFile(golden, []byte("golden"), 0600); err != nil {
		t.Fatalf("write golden image: %v", err)
	}
	workspace := filepath.Join(tmp, "goose-workspaces")

	if _, err := NewProvisioner(golden, workspace, filepath.Join(tmp, "scripts", "build_image.sh")); err != nil {
		t.Fatalf("NewProvisioner: %v", err)
	}

	fi, err := os.Stat(workspace)
	if err != nil {
		t.Fatalf("stat workspace dir: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0700 {
		t.Fatalf("workspace dir mode = %#o, want 0700 (VM rootfs images inside it hold 0600 secrets)", got)
	}
}
