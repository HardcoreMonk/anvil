package anvilmcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// isDialError reports whether err is a dial-phase transport failure (the
// connection was never established). Mirrors the daemon-side relay-retry
// classification (cmd/goose-daemon/relay_retry.go): only dial-class failures
// mark a host as down for reconcile probes and home-failover detection —
// HTTP responses, reset/EOF, and ctx cancellation never do.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// homeFailureThreshold is the number of CONSECUTIVE reconcile passes on which
// a routed flock's home daemon must fail with a dial-class error before
// re-election fires. Deliberately a constant, not configuration (YAGNI — the
// same fixed-policy stance as the daemon's bounded relay retry).
const homeFailureThreshold = 3

// electNewHome returns the deterministic failover target for record: the first
// host in record.Agents order that is not the failed home, has a daemon
// client, and was observed reachable by this reconcile pass. Same input, same
// answer — the elector is the single control plane, so determinism (not
// consensus) is what prevents split-brain. ok=false when no candidate
// survives (single-host flock or all members down): the caller no-ops and the
// saturated counter re-evaluates next pass.
func (r *RuntimeRouter) electNewHome(record RoutedFlockRecord, probes map[string]hostProbe) (string, bool) {
	homeHost := strings.TrimSpace(record.HomeHost)
	seen := map[string]bool{}
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || host == homeHost || seen[host] {
			continue
		}
		seen[host] = true
		if daemon, ok := r.daemons[host]; !ok || daemon == nil {
			continue
		}
		if probes[host].reachable {
			return host, true
		}
	}
	return "", false
}

// failoverRoutedFlock re-points record at newHome and rebuilds the hub/relay
// topology (spec §전환 절차). Ordering is the contract:
//  1. Persist HomeHost FIRST — the atomic transition point. From here every
//     later reconcile pass heals toward the new home even if the steps below
//     all fail right now. A token-less re-save preserves the persisted
//     relay/call tokens (applyRoutedFlockAndPlacements carrier rule).
//  2. Hub registration on the new home (daemon-side relay→hub promotion).
//  3. Relay re-registration on every other member host, INCLUDING the old
//     home (normally still down — heals into a relay on revival, which is
//     also what prevents automatic fail-back).
//  4. Best-effort stale-hub DELETE on the old home. VM-safe by construction:
//     a hub flock's Agents map is always empty (RegisterHub/promotion
//     invariant), so the daemon's deleteFlock destroys no member VMs.
// Tokens are reused unchanged — the guest-injected token never changes, so
// guests ride through the failover untouched. switched reports whether step 1
// committed; the caller resets the failure counter only then. All log/error
// text carries flock/host identifiers only.
func (r *RuntimeRouter) failoverRoutedFlock(ctx context.Context, record RoutedFlockRecord, newHome, relayToken, callToken string) (bool, error) {
	flockID := strings.TrimSpace(record.FlockID)
	oldHome := strings.TrimSpace(record.HomeHost)
	record.HomeHost = newHome
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		return false, fmt.Errorf("failover routed flock %q: persisting new home host %q failed: %w", flockID, newHome, err)
	}
	r.logf("anvil-mcp: routed flock %q home failover %q -> %q (canonical wall restarts empty on the new home)", flockID, oldHome, newHome)
	var errs []error
	if err := r.registerRoutedHub(ctx, record, relayToken, callToken); err != nil {
		errs = append(errs, fmt.Errorf("failover routed flock %q: hub promotion on new home %q failed", flockID, newHome))
	}
	errs = append(errs, r.registerRoutedRelays(ctx, record, relayToken, callToken)...)
	if daemon, ok := r.daemons[oldHome]; ok && daemon != nil {
		if _, err := daemon.DeleteFlock(ctx, flockID); err != nil {
			r.logf("anvil-mcp: routed flock %q: stale hub delete on old home %q failed (best-effort, skipped)", flockID, oldHome)
		}
	}
	return true, errors.Join(errs...)
}
