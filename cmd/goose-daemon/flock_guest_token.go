package main

import (
	"log/slog"

	"ephemera/internal/orchestrator"
)

// Per-flock guest capability tokens for LOCAL flocks.
//
// A flock member guest needs exactly two control-plane entry points: its flock's
// Town Wall (post / wall / wall/history) and its flock's /call. Both are already
// admitted by the per-flock relay-token store that routed flocks use, and that
// admission path (authMiddleware's relayGuestPathFlockID block) never inspects
// the flock kind — it extracts the flock id from the request path and compares
// the bearer against THAT flock's registered token. Registering a local flock's
// token in the same store therefore puts both flock kinds on one token model
// without touching the admission rules.
//
// The daemon's own operator bearer is not used for this: it opens every
// control-plane route, and a flock guest needs none of them.

// authEnabled reports whether API auth is on. authMiddleware short-circuits and
// admits every request unconditionally when the client list is empty, so with
// auth disabled no token authenticates anything and issuing one would be
// meaningless bookkeeping.
func (cp *ControlPlane) authEnabled() bool {
	cp.clientsMu.RLock()
	defer cp.clientsMu.RUnlock()
	return len(cp.clients) > 0
}

// ensureLocalFlockGuestToken returns the guest capability token for a LOCAL
// flock, minting, persisting and registering one if the flock has none yet.
// Callers MUST have established that flockID names a local flock — a routed
// flock's relay token is supplied by its adapter and must never be overwritten
// here.
//
// Returns "" when auth is disabled, matching what controlPlaneTokenForVM()
// returned in that mode: the provisioner then writes no cp-token file, exactly as
// before this change.
//
// Minting is lazy rather than create-time-only on purpose. A flock recovered from
// a release that predates this file has no token, and its next member spawn
// (add-agent, restart, role change) would otherwise inject an empty token and
// strand that member on 401. Every call site holds the flock's mutation lock, so
// two spawns for the same flock cannot mint competing tokens.
func (cp *ControlPlane) ensureLocalFlockGuestToken(flockID string) string {
	if !cp.authEnabled() {
		return ""
	}
	if tok := cp.relayTokenFor(flockID); tok != "" {
		return tok
	}
	tok, err := generateAgentToken()
	if err != nil {
		slog.Error("flock: guest capability token generation failed; members will spawn without a control-plane credential", "flock_id", flockID, "err", err)
		return ""
	}
	// Persist before registering so a crash in between leaves a token that a
	// restart can rehydrate, rather than admission for a token nobody can find.
	// A write failure is not fatal: admission still works for this daemon's
	// lifetime and the next restart re-mints.
	if err := orchestrator.SaveFlockGuestToken(cp.workDir, flockID, tok); err != nil {
		slog.Warn("flock: persist guest capability token failed (admission holds until daemon restart)", "flock_id", flockID, "err", err)
	}
	cp.setRelayToken(flockID, tok)
	slog.Warn("flock: issued guest capability token", "flock_id", flockID)
	return tok
}

// ensureFlockGuestTokenFor is the flock-typed entry point used by the member
// spawn paths, which all hold a *Flock already. Routed flocks (hub/relay) are
// skipped: their relay token is registered by the distributed/relay registration
// handlers from the value their adapter supplies.
func (cp *ControlPlane) ensureFlockGuestTokenFor(f *orchestrator.Flock) {
	if f == nil || f.Kind != orchestrator.FlockKindLocal {
		return
	}
	cp.ensureLocalFlockGuestToken(f.ID)
}

// revokeFlockGuestToken drops a flock's capability token from admission AND from
// disk. Both halves are required: the in-memory removal stops the token
// authenticating anything in this process, and the file removal stops the next
// daemon start rehydrating admission for a flock that no longer exists. The flock
// directory itself survives deletion (its Town Wall is kept as an audit
// artifact), so the token file cannot be left to a directory removal.
func (cp *ControlPlane) revokeFlockGuestToken(flockID string) {
	cp.removeRelayToken(flockID)
	if err := orchestrator.DeleteFlockGuestToken(cp.workDir, flockID); err != nil {
		slog.Warn("flock: remove persisted guest capability token failed", "flock_id", flockID, "err", err)
	}
}

// flockGuestToken returns the capability token to inject into a member guest of
// flockID. It NEVER falls back to the operator bearer: injecting the broad
// credential is the exact failure this design removes, so a missing token yields
// an empty one (the provisioner writes no cp-token file — degraded but safe) and
// a loud log line instead.
//
// Routed flocks resolve through the same store, since their registration
// handlers call setRelayToken with the adapter-supplied token.
func (cp *ControlPlane) flockGuestToken(flockID string) string {
	tok := cp.relayTokenFor(flockID)
	if tok == "" && cp.authEnabled() {
		slog.Error("flock: no guest capability token registered; member spawns without a control-plane credential and its wall/call calls will be rejected",
			"flock_id", flockID)
	}
	return tok
}

// rehydrateFlockGuestTokens re-registers the persisted capability token of every
// recovered LOCAL flock. It runs immediately after FlockManager.LoadFromDisk in
// the daemon boot sequence.
//
// This step is not optional. cp.relayTokens is in-memory only and starts empty in
// every process, and LoadFromDisk cannot fill it — it is a FlockManager method
// with no reference to the ControlPlane. Routed flocks survive that gap because
// an external driver (the adapter's reconcile re-POST) re-registers their token;
// a local flock has no such driver. Without this walk, the first member spawned
// after any restart would be injected an empty token.
func (cp *ControlPlane) rehydrateFlockGuestTokens() {
	restored, missing := 0, 0
	for _, f := range cp.flockMgr.List() {
		if f.Kind != orchestrator.FlockKindLocal {
			continue
		}
		tok, err := orchestrator.LoadFlockGuestToken(cp.workDir, f.ID)
		if err != nil || tok == "" {
			// Expected for a flock created before capability tokens existed; the
			// next member spawn mints one lazily.
			missing++
			continue
		}
		cp.setRelayToken(f.ID, tok)
		restored++
	}
	if restored > 0 || missing > 0 {
		slog.Warn("flock: guest capability token admission restored", "restored", restored, "without_token", missing)
	}
}
