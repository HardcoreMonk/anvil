package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlockGuestToken_RoundTrip pins the basic persistence contract: a saved
// per-flock guest capability token reads back byte-identical, and a flock that
// never had one reads back as absent (not as an empty-but-present token).
func TestFlockGuestToken_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	const tok = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	if err := SaveFlockGuestToken(tmp, "flock-rt", tok); err != nil {
		t.Fatalf("SaveFlockGuestToken: %v", err)
	}
	got, err := LoadFlockGuestToken(tmp, "flock-rt")
	if err != nil {
		t.Fatalf("LoadFlockGuestToken: %v", err)
	}
	if got != tok {
		t.Fatalf("LoadFlockGuestToken = %q, want %q", got, tok)
	}

	// A flock with no token file must report absence via an error, so callers can
	// distinguish "pre-upgrade flock, mint one" from "token is the empty string".
	if _, err := LoadFlockGuestToken(tmp, "flock-never-had-one"); err == nil {
		t.Fatal("LoadFlockGuestToken on a flock with no token file returned nil error, want an error")
	}
}

// TestFlockGuestToken_Mode0600RegardlessOfParentDirMode is the load-bearing
// file-mode guard. The token file is a secret, so its mode must be pinned at the
// call site rather than inherited from the process umask or from however
// permissive the enclosing flock directory happens to be. A 0600 file inside a
// world-readable directory is still unreadable to other users; this test proves
// the 0600 is not an accident of the environment.
func TestFlockGuestToken_Mode0600RegardlessOfParentDirMode(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "flocks", "flock-mode")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Deliberately loosen the parent directory (MkdirAll applies the umask, so set
	// it explicitly) — the file mode must not follow it.
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	if err := SaveFlockGuestToken(tmp, "flock-mode", "sentinel-token"); err != nil {
		t.Fatalf("SaveFlockGuestToken: %v", err)
	}
	fi, err := os.Stat(FlockGuestTokenPath(tmp, "flock-mode"))
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("token file mode = %04o, want 0600 (parent dir is 0777)", perm)
	}

	// An overwrite must not relax the mode either (a rewrite goes through the same
	// tmp+rename, so a stale tmp file's mode must never leak into the destination).
	if err := SaveFlockGuestToken(tmp, "flock-mode", "sentinel-token-2"); err != nil {
		t.Fatalf("SaveFlockGuestToken (overwrite): %v", err)
	}
	fi, err = os.Stat(FlockGuestTokenPath(tmp, "flock-mode"))
	if err != nil {
		t.Fatalf("stat token file after overwrite: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("token file mode after overwrite = %04o, want 0600", perm)
	}
}

// TestDeleteFlockGuestToken_RemovesFile proves revocation reaches disk. The flock
// directory itself deliberately survives deletion (TOWN_WALL.log is kept as an
// audit artifact — see DeleteFlockMetadata), so the token file must be removed
// explicitly; it cannot ride along on a directory removal that never happens.
func TestDeleteFlockGuestToken_RemovesFile(t *testing.T) {
	tmp := t.TempDir()
	if err := SaveFlockGuestToken(tmp, "flock-del", "to-be-revoked"); err != nil {
		t.Fatalf("SaveFlockGuestToken: %v", err)
	}
	if err := DeleteFlockGuestToken(tmp, "flock-del"); err != nil {
		t.Fatalf("DeleteFlockGuestToken: %v", err)
	}
	if _, err := os.Stat(FlockGuestTokenPath(tmp, "flock-del")); !os.IsNotExist(err) {
		t.Fatalf("token file still present after delete (stat err = %v)", err)
	}
	// Idempotent: deleting a flock that never had a token is not an error.
	if err := DeleteFlockGuestToken(tmp, "flock-del"); err != nil {
		t.Fatalf("DeleteFlockGuestToken (second call): %v", err)
	}
}

// TestFlockGuestToken_NeverReachesMetadataJSON keeps the guest capability token on
// the correct side of FlockMetadata's blanket no-secrets invariant. The token
// lives in its own file precisely so that invariant stays unconditional: a guard
// that had to tell "routed token" from "local capability token" apart would rot.
func TestFlockGuestToken_NeverReachesMetadataJSON(t *testing.T) {
	const sentinel = "GUEST-TOKEN-SENTINEL-4c1f0a97"
	tmp := t.TempDir()

	if err := SaveFlockMetadata(tmp, FlockMetadata{FlockID: "flock-inv", Task: "t"}); err != nil {
		t.Fatalf("SaveFlockMetadata: %v", err)
	}
	if err := SaveFlockGuestToken(tmp, "flock-inv", sentinel); err != nil {
		t.Fatalf("SaveFlockGuestToken: %v", err)
	}
	raw, err := os.ReadFile(metadataPath(tmp, "flock-inv"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("metadata.json leaked the guest capability token:\n%s", raw)
	}
	// And the reverse: the token file must not be mistaken for metadata by the
	// recovery scan (which keys off metadata.json only).
	metas, err := ListFlockMetadata(tmp)
	if err != nil {
		t.Fatalf("ListFlockMetadata: %v", err)
	}
	if len(metas) != 1 || metas[0].FlockID != "flock-inv" {
		t.Fatalf("ListFlockMetadata = %+v, want exactly flock-inv", metas)
	}
}
