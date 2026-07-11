package anvilmcp

import (
	"context"
	"errors"
	"net/http"
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

// TestCreate_ReRegistersHubWithVMIDRoster verifies that after all routed-flock
// members spawn, create re-registers the hub on the home daemon with a
// VMID/Addr-enriched roster (Task 1's RosterMember fields), that the member's
// relay flock is likewise re-registered with that host's local agents (the
// 2026-07-08 amendment's RelayFlockRequest.Agents), and that both the initial
// pre-spawn hub registration and the post-spawn re-registration carry a
// non-empty call token distinct from the relay token (Task 2's CallToken). The
// initial registration cannot know VM ids yet — only the post-spawn
// re-registration does — so this also proves the re-registration actually
// happens, not just that a single hub registration occurred.
func TestCreate_ReRegistersHubWithVMIDRoster(t *testing.T) {
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

	if _, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	}); err != nil {
		t.Fatal(err)
	}

	if home.distributedCalls < 2 {
		t.Fatalf("home distributed (hub) registrations = %d, want >= 2 (initial + post-spawn re-registration)", home.distributedCalls)
	}
	if len(home.distributedReqs) < 2 {
		t.Fatalf("home distributed request history len = %d, want >= 2", len(home.distributedReqs))
	}

	last := home.distributedReq
	var gotCoordinator, gotResearcher bool
	for _, m := range last.Roster {
		switch m.AgentID {
		case "coordinator-1":
			gotCoordinator = true
			if m.VMID != "vm-coordinator-1" {
				t.Fatalf("coordinator roster VMID = %q, want vm-coordinator-1", m.VMID)
			}
			if m.Addr != "http://hostA.internal:8080" {
				t.Fatalf("coordinator roster Addr = %q, want http://hostA.internal:8080", m.Addr)
			}
		case "researcher-1":
			gotResearcher = true
			if m.VMID != "vm-researcher-1" {
				t.Fatalf("researcher roster VMID = %q, want vm-researcher-1", m.VMID)
			}
			if m.Addr != "http://hostB.internal:8080" {
				t.Fatalf("researcher roster Addr = %q, want http://hostB.internal:8080", m.Addr)
			}
		default:
			t.Fatalf("unexpected roster member agent id %q", m.AgentID)
		}
	}
	if !gotCoordinator || !gotResearcher {
		t.Fatalf("re-registered roster missing member(s): %+v", last.Roster)
	}

	first := home.distributedReqs[0]
	if first.CallToken == "" {
		t.Fatalf("initial hub registration CallToken empty")
	}
	if first.CallToken == first.RelayToken {
		t.Fatalf("initial hub registration CallToken == RelayToken, want distinct secrets")
	}
	if last.CallToken == "" {
		t.Fatalf("re-registration hub CallToken empty")
	}
	if last.CallToken == last.RelayToken {
		t.Fatalf("re-registration hub CallToken == RelayToken, want distinct secrets")
	}
	if first.CallToken != last.CallToken {
		t.Fatalf("initial CallToken %q != re-registration CallToken %q, want the same per-flock secret", first.CallToken, last.CallToken)
	}

	// The member's last (post-spawn) relay registration carries only that
	// host's local agent(s), with VMID filled in.
	if len(member.relayReq.Agents) != 1 {
		t.Fatalf("member relay Agents len = %d, want 1: %+v", len(member.relayReq.Agents), member.relayReq.Agents)
	}
	if member.relayReq.Agents[0].AgentID != "researcher-1" || member.relayReq.Agents[0].VMID != "vm-researcher-1" {
		t.Fatalf("member relay Agents[0] = %+v, want researcher-1/vm-researcher-1", member.relayReq.Agents[0])
	}
}

// callTokenProbeDaemon mirrors relayTokenProbeDaemon exactly (see above), but
// for the call token: it proves the call token is durably saved BEFORE the hub
// is registered — the same crash-recovery guarantee Task 4's store extends to
// call tokens.
type callTokenProbeDaemon struct {
	*routerFakeDaemon
	store                  *PlacementStore
	tokenPresentAtRegister bool
	tokenValueAtRegister   string
}

func (d *callTokenProbeDaemon) RegisterDistributedFlock(ctx context.Context, flockID string, req DistributedFlockRequest) error {
	token, ok := d.store.RoutedFlockCallToken(flockID)
	d.tokenPresentAtRegister = ok && token != ""
	d.tokenValueAtRegister = token
	return d.routerFakeDaemon.RegisterDistributedFlock(ctx, flockID, req)
}

// TestCreate_PersistsCallTokenBeforeHubRegistration mirrors
// TestCreate_PersistsRelayTokenBeforeHubRegistration for the call token: it
// proves the call token is persisted on the first placement-store save,
// before the home daemon's hub is registered.
func TestCreate_PersistsCallTokenBeforeHubRegistration(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &callTokenProbeDaemon{
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
		t.Fatalf("call token not persisted at hub-registration time; reconcile could not recover a crashed create")
	}
	if home.tokenValueAtRegister != home.distributedReq.CallToken {
		t.Fatalf("token at register %q != hub call token %q", home.tokenValueAtRegister, home.distributedReq.CallToken)
	}
	if token, ok := store.RoutedFlockCallToken(out.FlockID); !ok || token == "" {
		t.Fatalf("call token not persisted after successful create")
	}
}

// TestRollback_RevokesCallToken mirrors TestRollback_DeregistersSharedTownWall
// for the call token: rollback must leave no stale call secret persisted.
func TestRollback_RevokesCallToken(t *testing.T) {
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

	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	flockID := records[0].FlockID
	if token, ok := store.RoutedFlockCallToken(flockID); ok || token != "" {
		t.Fatalf("call token still persisted after rollback: %q,%v", token, ok)
	}
}

// TestDeleteRoutedFlock_RevokesCallToken mirrors
// TestDeleteRoutedFlock_DeregistersSharedTownWall for the call token: delete
// must leave no stale call secret persisted.
func TestDeleteRoutedFlock_RevokesCallToken(t *testing.T) {
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
	if token, ok := store.RoutedFlockCallToken(out.FlockID); !ok || token == "" {
		t.Fatalf("call token not persisted on create for flock %q", out.FlockID)
	}

	if _, err := router.DeleteRoutedFlock(context.Background(), out.FlockID); err != nil {
		t.Fatalf("DeleteRoutedFlock: %v", err)
	}
	if token, ok := store.RoutedFlockCallToken(out.FlockID); ok || token != "" {
		t.Fatalf("call token still persisted after delete: %q,%v", token, ok)
	}
}

// TestDeleteRoutedFlock_AlreadyGoneVMsReportSuccess reproduces defect D2: the
// routed-flock delete deregisters the shared wall first (DeleteFlock), which on
// the real daemon cascades a teardown of that flock's member VMs. The subsequent
// per-VM DELETE /vms/{id} then hits an already-absent VM and the daemon answers
// 404. A 404 here is the success end-state of teardown, so the delete must
// report success — not "cleanup pending: reason=cleanup_failed". Both hosts'
// VMs return 404, mirroring the cross-host verification run where teardown fully
// succeeded (flock 404 / VM 0 / token 401) yet the tool reported is_error.
func TestDeleteRoutedFlock_AlreadyGoneVMsReportSuccess(t *testing.T) {
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

	// Deregistering the shared wall already tore the VMs down on the daemon, so the
	// per-VM DELETE finds nothing and 404s. Model that end-state on both hosts.
	home.deleteErr = &DaemonError{StatusCode: http.StatusNotFound, Body: "vm not found"}
	member.deleteErr = &DaemonError{StatusCode: http.StatusNotFound, Body: "vm not found"}

	resp, err := router.DeleteRoutedFlock(context.Background(), out.FlockID)
	if err != nil {
		t.Fatalf("DeleteRoutedFlock reported failure for a fully-torn-down flock: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("DeleteRoutedFlock response = %+v, want status 200", resp)
	}
	record, ok := store.RoutedFlock(out.FlockID)
	if !ok {
		t.Fatalf("flock %q missing from store after delete", out.FlockID)
	}
	if record.Status != RoutedFlockStatusDeleted {
		t.Fatalf("flock status = %q, want %q", record.Status, RoutedFlockStatusDeleted)
	}
	if len(record.Agents) != 0 {
		t.Fatalf("deleted flock retains agents %+v, want none", record.Agents)
	}
}

// TestDeleteRoutedFlock_GenuineTeardownFailureStaysCleanupPending pins the
// D2-fix boundary: a genuine, non-404 delete failure (e.g. the daemon is
// unreachable) must still surface as is_error "cleanup pending:
// reason=cleanup_failed", persist the flock as failed_cleanup_pending, and keep
// the un-torn-down agent marked cleanup_pending so the retry path survives. Only
// "already gone" (404) is reclassified as success — real failures are not.
func TestDeleteRoutedFlock_GenuineTeardownFailureStaysCleanupPending(t *testing.T) {
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

	// Home VM tears down cleanly; the member daemon is unreachable — a genuine,
	// non-404 failure that leaves that VM standing.
	member.deleteErr = errors.New("dial tcp: connection refused")

	_, err = router.DeleteRoutedFlock(context.Background(), out.FlockID)
	if err == nil {
		t.Fatal("DeleteRoutedFlock error = nil, want cleanup-pending on genuine teardown failure")
	}
	if msg := err.Error(); !strings.Contains(msg, "cleanup pending") || !strings.Contains(msg, routedFlockReasonCleanupFailed) {
		t.Fatalf("DeleteRoutedFlock error = %q, want cleanup-pending/%s", msg, routedFlockReasonCleanupFailed)
	}
	// The error string must not leak the daemon-side dial detail (redaction: only
	// flock/host/vm identifiers, never addresses).
	if strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "dial") {
		t.Fatalf("DeleteRoutedFlock error leaked daemon detail: %q", err.Error())
	}
	record, ok := store.RoutedFlock(out.FlockID)
	if !ok {
		t.Fatalf("flock %q missing from store after failed delete", out.FlockID)
	}
	if record.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("flock status = %q, want %q", record.Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(record.Agents) != 1 {
		t.Fatalf("failed-cleanup flock retains %d agents, want 1 (the un-torn-down member)", len(record.Agents))
	}
	if got := record.Agents[0]; got.VMID != "vm-researcher-1" || got.Status != routedFlockAgentStatusCleanupPending {
		t.Fatalf("pending agent = %+v, want vm-researcher-1 status=%s", got, routedFlockAgentStatusCleanupPending)
	}
}
