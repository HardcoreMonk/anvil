package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlockMetadata_RoundTripPreservesDistributedFields proves the on-disk
// schema carries a routed flock's role across a Persist -> LoadFromDisk cycle:
// a hub keeps its Kind + Roster, a relay keeps its Kind + HomeAddr. This is the
// core of the D1 fix — before it, FlockMetadata had no Kind/HomeAddr/Roster, so
// a recovered hub/relay was silently downgraded to a local flock.
func TestFlockMetadata_RoundTripPreservesDistributedFields(t *testing.T) {
	// hub: Kind + Roster survive.
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	wall, err := NewTownWall("routed-hub", filepath.Join(tmp, "flocks", "routed-hub", "TOWN_WALL.log"))
	if err != nil {
		t.Fatal(err)
	}
	roster := []RosterMember{{AgentID: "researcher-1", Host: "hostB", VMID: "vm-b-1"}}
	hub := fm.RegisterHub("routed-hub", wall, roster, "relay-tok", "call-tok")
	if err := hub.Persist(tmp); err != nil {
		t.Fatalf("persist hub: %v", err)
	}

	fm2 := NewFlockManager(tmp)
	if _, _, err := fm2.LoadFromDisk(); err != nil {
		t.Fatalf("load hub: %v", err)
	}
	got, ok := fm2.Get("routed-hub")
	if !ok {
		t.Fatal("hub flock not recovered")
	}
	if got.Kind != FlockKindHub {
		t.Fatalf("recovered hub Kind = %q, want %q", got.Kind, FlockKindHub)
	}
	if len(got.Roster) != 1 || got.Roster[0].AgentID != "researcher-1" ||
		got.Roster[0].Host != "hostB" || got.Roster[0].VMID != "vm-b-1" {
		t.Fatalf("recovered hub Roster = %+v, want [researcher-1@hostB/vm-b-1]", got.Roster)
	}

	// relay: Kind + HomeAddr survive.
	tmp2 := t.TempDir()
	rfm := NewFlockManager(tmp2)
	relay := rfm.RegisterRelay("routed-relay", "http://hostA:3000", "relay-tok", "call-tok", nil)
	if err := relay.Persist(tmp2); err != nil {
		t.Fatalf("persist relay: %v", err)
	}
	rfm2 := NewFlockManager(tmp2)
	if _, _, err := rfm2.LoadFromDisk(); err != nil {
		t.Fatalf("load relay: %v", err)
	}
	gotR, ok := rfm2.Get("routed-relay")
	if !ok {
		t.Fatal("relay flock not recovered")
	}
	if gotR.Kind != FlockKindRelay {
		t.Fatalf("recovered relay Kind = %q, want %q", gotR.Kind, FlockKindRelay)
	}
	if gotR.HomeAddr != "http://hostA:3000" {
		t.Fatalf("recovered relay HomeAddr = %q, want http://hostA:3000", gotR.HomeAddr)
	}
}

// TestFlockMetadata_NeverPersistsTokens is the security sentinel: the admission
// secrets (relay/call tokens) must NEVER reach the daemon's on-disk metadata.
// It also asserts RegisterHub/RegisterRelay persist at all (the D1 fix), by
// requiring metadata.json to exist right after registration.
func TestFlockMetadata_NeverPersistsTokens(t *testing.T) {
	const relaySentinel = "RELAY-SENTINEL-9f3a2b1e"
	const callSentinel = "CALL-SENTINEL-7b2c1d4a"

	// hub
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	wall, err := NewTownWall("routed-hub", filepath.Join(tmp, "flocks", "routed-hub", "TOWN_WALL.log"))
	if err != nil {
		t.Fatal(err)
	}
	fm.RegisterHub("routed-hub", wall, []RosterMember{{AgentID: "researcher-1", Host: "hostB"}}, relaySentinel, callSentinel)
	hubBytes, err := os.ReadFile(metadataPath(tmp, "routed-hub"))
	if err != nil {
		t.Fatalf("RegisterHub did not persist metadata.json: %v", err)
	}
	if strings.Contains(string(hubBytes), relaySentinel) || strings.Contains(string(hubBytes), callSentinel) {
		t.Fatalf("hub metadata.json leaked an admission token:\n%s", hubBytes)
	}

	// relay
	tmp2 := t.TempDir()
	rfm := NewFlockManager(tmp2)
	rfm.RegisterRelay("routed-relay", "http://hostA:3000", relaySentinel, callSentinel, nil)
	relayBytes, err := os.ReadFile(metadataPath(tmp2, "routed-relay"))
	if err != nil {
		t.Fatalf("RegisterRelay did not persist metadata.json: %v", err)
	}
	if strings.Contains(string(relayBytes), relaySentinel) || strings.Contains(string(relayBytes), callSentinel) {
		t.Fatalf("relay metadata.json leaked an admission token:\n%s", relayBytes)
	}
}

// TestLoadFromDisk_LegacyMetadataLoadsAsLocal is a regression guard: a metadata
// file written before D1 (no kind/home_addr/roster keys) must still recover as a
// plain local flock with its Town Wall reopened — the additive optional fields
// must not change how legacy files load.
func TestLoadFromDisk_LegacyMetadataLoadsAsLocal(t *testing.T) {
	tmp := t.TempDir()
	legacy := `{"flock_id":"legacy-1","task":"t","max_agents":0,"agents":{},"created_at":"2026-01-01T00:00:00Z","schema_version":1}`
	dir := filepath.Join(tmp, "flocks", "legacy-1")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	fm := NewFlockManager(tmp)
	if _, _, err := fm.LoadFromDisk(); err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	f, ok := fm.Get("legacy-1")
	if !ok {
		t.Fatal("legacy flock not recovered")
	}
	if f.Kind != FlockKindLocal {
		t.Fatalf("legacy flock Kind = %q, want local (%q)", f.Kind, FlockKindLocal)
	}
	if f.HomeAddr != "" || f.Roster != nil {
		t.Fatalf("legacy flock got spurious distributed fields: HomeAddr=%q Roster=%+v", f.HomeAddr, f.Roster)
	}
	if f.TownWall == nil {
		t.Fatal("legacy (local) flock must reopen its Town Wall")
	}
}
