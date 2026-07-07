package anvilmcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestRollback_DeregistersSharedTownWall verifies that when a routed-flock create
// fails partway (after hub/relay registration) the rollback best-effort
// deregisters the cross-host shared Town Wall: it issues a flock-delete
// (DELETE /flocks/{id}) on the home daemon that owns the hub and leaves no stale
// relay token persisted in the store. Deregistration failures must never mask the
// original create failure.
func TestRollback_DeregistersSharedTownWall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	member := &routerFakeDaemon{spawnErr: errors.New("daemon http://hostB.internal/secret-endpoint failed: agent_token=secret-token")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want member spawn failure")
	}
	if out != nil {
		t.Fatalf("CreateRoutedFlockMembers output = %+v, want nil on rollback", out)
	}

	// The hub lives on the home daemon: rollback must deregister it.
	if home.deregisterCalls == 0 {
		t.Fatalf("rollback did not deregister the hub flock on the home daemon")
	}
	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	flockID := records[0].FlockID
	for _, id := range home.deregisterFlockIDs {
		if strings.TrimSpace(id) != flockID {
			t.Fatalf("home deregister flock id = %q, want %q", id, flockID)
		}
	}
	// No stale relay token may linger for a rolled-back flock.
	if token, ok := store.RoutedFlockRelayToken(flockID); ok || token != "" {
		t.Fatalf("relay token still persisted after rollback: %q,%v", token, ok)
	}

	// Best-effort discipline: the returned error still reports the original create
	// failure (daemon_create_failed), not a deregistration error.
	if !strings.Contains(err.Error(), FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("rollback error = %q, want original daemon_create_failed reason", err.Error())
	}
}

// relayTokenProbeDaemon wraps routerFakeDaemon and, at RegisterDistributedFlock
// time, records whether the flock's relay token is already persisted in the
// placement store. It proves the token is durably saved BEFORE the hub is
// registered, so a crash between hub registration and the full-success save
// leaves the store able to reconcile (a live token on the daemon with none in
// the store makes ReconcilePlacements skip the record).
type relayTokenProbeDaemon struct {
	*routerFakeDaemon
	store                  *PlacementStore
	tokenPresentAtRegister bool
	tokenValueAtRegister   string
}

func (d *relayTokenProbeDaemon) RegisterDistributedFlock(ctx context.Context, flockID string, req DistributedFlockRequest) error {
	token, ok := d.store.RoutedFlockRelayToken(flockID)
	d.tokenPresentAtRegister = ok && token != ""
	d.tokenValueAtRegister = token
	return d.routerFakeDaemon.RegisterDistributedFlock(ctx, flockID, req)
}

// TestCreate_PersistsRelayTokenBeforeHubRegistration proves the relay token is
// persisted on the first placement-store save (alongside HomeHost), before the
// home daemon's hub is registered — not only on the final full-success save.
// Otherwise a crash mid-create would leave a live token registered on the daemon
// with none in the store, and reconcile would be unable to recover it.
func TestCreate_PersistsRelayTokenBeforeHubRegistration(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &relayTokenProbeDaemon{
		routerFakeDaemon: &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
			VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
		}}},
		store: store,
	}
	member := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-researcher-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers: %v", err)
	}
	if !home.tokenPresentAtRegister {
		t.Fatalf("relay token not persisted at hub-registration time; reconcile could not recover a crashed create")
	}
	// The token seen at registration must match the one the hub was registered with.
	if home.tokenValueAtRegister != home.distributedReq.RelayToken {
		t.Fatalf("token at register %q != hub relay token %q", home.tokenValueAtRegister, home.distributedReq.RelayToken)
	}
	// And it must still be persisted after full success.
	if token, ok := store.RoutedFlockRelayToken(out.FlockID); !ok || token == "" {
		t.Fatalf("relay token not persisted after successful create")
	}
}

// TestRollback_DeregistersOrphanedMemberRelay proves that when a member's relay
// flock is registered but that same member's SpawnVM then fails, rollback still
// deregisters the member's relay flock. Before the fix, deregisterRoutedFlockWall
// derived hosts only from record.HomeHost + record.Agents, and a spawn-failed
// member never entered record.Agents, so its relay registration leaked on the
// member daemon.
func TestRollback_DeregistersOrphanedMemberRelay(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	// member: its relay flock registers cleanly, then its SpawnVM fails.
	member := &routerFakeDaemon{spawnErr: errors.New("daemon http://hostB.internal/secret-endpoint failed")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want member spawn failure")
	}
	// Precondition: the member's relay flock was actually registered before spawn.
	if member.relayCalls == 0 {
		t.Fatalf("member relay flock was never registered; test setup invalid")
	}
	// The member's relay flock must be deregistered on rollback even though the
	// member never entered record.Agents (spawn failed before the append).
	if member.deregisterCalls == 0 {
		t.Fatalf("rollback did not deregister the spawn-failed member's relay flock")
	}
}

// TestRollback_DeregisterFailureDoesNotMaskOriginalError verifies best-effort
// semantics: a deregistration (flock-delete) failure during rollback must not
// change the create outcome — the original create failure is still surfaced and
// the flock still rolls back to a terminal state.
func TestRollback_DeregisterFailureDoesNotMaskOriginalError(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{
			VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
		}},
		deregisterErr: errors.New("deregister failed: agent_token=secret-token"),
	}
	member := &routerFakeDaemon{spawnErr: errors.New("daemon http://hostB.internal/secret-endpoint failed")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want member spawn failure")
	}
	if home.deregisterCalls == 0 {
		t.Fatalf("rollback did not attempt hub deregistration")
	}
	if !strings.Contains(err.Error(), FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("rollback error = %q, want original daemon_create_failed reason", err.Error())
	}
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("rollback error leaked forbidden marker %q: %q", forbidden, err.Error())
		}
	}
}

// TestDeleteRoutedFlock_DeregistersSharedTownWall verifies the routed-flock DELETE
// path tears down the cross-host shared Town Wall: it deregisters the hub on the
// home daemon and the relay on each member host, and removes the persisted relay
// token so no stale secret lingers on disk after delete.
func TestDeleteRoutedFlock_DeregistersSharedTownWall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	member := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-researcher-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers: %v", err)
	}
	if token, ok := store.RoutedFlockRelayToken(out.FlockID); !ok || token == "" {
		t.Fatalf("relay token not persisted on create for flock %q", out.FlockID)
	}
	// Create itself must not deregister anything.
	if home.deregisterCalls != 0 || member.deregisterCalls != 0 {
		t.Fatalf("create issued deregister calls home/member = %d/%d, want 0/0", home.deregisterCalls, member.deregisterCalls)
	}

	if _, err := router.DeleteRoutedFlock(context.Background(), out.FlockID); err != nil {
		t.Fatalf("DeleteRoutedFlock: %v", err)
	}
	if home.deregisterCalls == 0 {
		t.Fatalf("delete did not deregister the hub flock on the home daemon")
	}
	if member.deregisterCalls == 0 {
		t.Fatalf("delete did not deregister the relay flock on the member daemon")
	}
	if token, ok := store.RoutedFlockRelayToken(out.FlockID); ok || token != "" {
		t.Fatalf("relay token still persisted after delete: %q,%v", token, ok)
	}
}
