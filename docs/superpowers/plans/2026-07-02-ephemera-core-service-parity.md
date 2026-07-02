# Ephemera Core Service Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring ephemera `v0.4.0` through `v0.7.0` core runtime/operator service functionality into anvil with runtime-grade verification and anvil's IronClaw-facing security boundary intact.

**Architecture:** Keep ephemera canonical runtime/operator surface upstream-compatible, then layer anvil aliases, scheduler, tenant/egress, audit, and `anvil_*` MCP policy on top. Merge upstream tags in chronological order with merge commits, resolve conflicts by preserving ephemera daemon/runtime behavior while enforcing anvil invariants: `agent_token` redaction, no raw daemon body in MCP output/audit, fork history preservation, and explicit runtime/operator vs IronClaw integration separation.

**Tech Stack:** Go 1.25, Go standard library HTTP tests, Firecracker/KVM, existing ephemera daemon/storage/network/orchestrator packages, Svelte/Vite Web UI, Node/npm for `web/`, Bash installer/smoke scripts, Git merge commits from upstream tags.

---

## Scope Check

This work spans four independent upstream lines: runtime stability, operator Web UI,
MCP Gateway, and installer/transcript hardening. Treat this document as the master
execution plan. Phase 1 already has a detailed sub-plan at
`docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`; use it for the
v0.4 implementation details and keep this plan as the cross-phase control plane.

## File Structure

### Phase 0 baseline and planning

- Modify: `CONTEXT.md`
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/ADR_INDEX.md`
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md`
- Modify: `docs/analysis/README.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `docs/operations/upstream-sync-policy.md`
- Read: `docs/superpowers/specs/2026-07-02-ephemera-core-service-parity-design.md`
- Read: `docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`

### Phase 1 `v0.4.0`-`v0.4.5` runtime stability

- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/config.go`
- Modify: `cmd/goose-daemon/recovery.go`
- Create/modify: `cmd/goose-daemon/audit.go`
- Create/modify: `cmd/goose-daemon/context.go`
- Create: `cmd/ephemera-ctl/*`
- Modify: `cmd/goose-agent/main.go`
- Modify: `scripts/gtcall`
- Modify: `internal/storage/diskspace.go`
- Modify: `internal/storage/orphan.go`
- Modify: `internal/storage/snapshot.go`
- Modify: `internal/storage/vm_state.go`
- Modify: `internal/network/manager.go`
- Modify: `internal/vm/machine.go`
- Modify: `internal/orchestrator/flock.go`
- Modify: `internal/orchestrator/townwall.go`
- Modify: `internal/orchestrator/watchdog.go`
- Modify: `internal/anvilmcp/*` only where daemon wire structs changed
- Test: `cmd/goose-daemon/*_test.go`
- Test: `cmd/ephemera-ctl/*_test.go`
- Test: `internal/storage/*_test.go`
- Test: `internal/network/*_test.go`
- Test: `internal/vm/*_test.go`
- Test: `internal/orchestrator/*_test.go`

### Phase 2 `v0.5.0`-`v0.5.5` operator service

- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/main_test.go`
- Modify/create: `cmd/goose-agent/session_test.go`
- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/config.go`
- Create/modify: `cmd/goose-daemon/config_api.go`
- Create/modify: `cmd/goose-daemon/config_api_test.go`
- Create/modify: `cmd/goose-daemon/providers.go`
- Create/modify: `cmd/goose-daemon/providers_test.go`
- Create/modify: `cmd/goose-daemon/presets.go`
- Create/modify: `cmd/goose-daemon/presets_test.go`
- Create/modify: `cmd/goose-daemon/ui.go`
- Create/modify: `cmd/goose-daemon/ui_test.go`
- Modify: `cmd/goose-daemon/orchestrator_api.go`
- Modify/create: `cmd/goose-daemon/orchestrator_handlers_test.go`
- Modify/create: `cmd/goose-daemon/snapshots_api_test.go`
- Modify: `cmd/goose-daemon/stats_collector.go`
- Modify: `cmd/goose-daemon/uidist/*`
- Create/modify: `web/*`
- Create/modify: `configs/webdev-demo/profiles/*`
- Modify: legacy `configs/profiles/*` examples only when the upstream merge changes
  them; preserve anvil examples that current docs or tests still reference.
- Modify: `internal/orchestrator/*`
- Modify: `internal/storage/snapshot.go`
- Modify: `internal/storage/vm_state.go`
- Modify: `internal/vm/machine.go`
- Modify: `observability_demo.sh`
- Modify: `webdev_demo.sh`
- Test: `cmd/goose-daemon/*_test.go`
- Test: `cmd/goose-agent/*_test.go`
- Test: `internal/orchestrator/*_test.go`
- Test: `internal/storage/*_test.go`
- Test: `internal/vm/*_test.go`

### Phase 3 `v0.6.0`-`v0.6.4` MCP Gateway

- Create: `internal/mcpgateway/backend.go`
- Create: `internal/mcpgateway/backend_stdio.go`
- Create: `internal/mcpgateway/gateway.go`
- Create: `internal/mcpgateway/identity.go`
- Create: `internal/mcpgateway/jsonrpc.go`
- Create: `internal/mcpgateway/policy.go`
- Create: `internal/mcpgateway/ratelimit.go`
- Create: `internal/mcpgateway/registry.go`
- Create: `internal/mcpgateway/secrets.go`
- Create: `internal/mcpgateway/session.go`
- Create: `internal/mcpgateway/types.go`
- Create/modify: `internal/mcpgateway/*_test.go`
- Create: `cmd/goose-daemon/builtins.go`
- Create: `cmd/goose-daemon/builtins_test.go`
- Create/modify: `cmd/goose-daemon/mcp_api.go`
- Create/modify: `cmd/goose-daemon/mcp_api_test.go`
- Create/modify: `cmd/goose-daemon/mcp_binding.go`
- Create/modify: `cmd/goose-daemon/mcp_binding_test.go`
- Create/modify: `cmd/goose-daemon/mcp_gateway.go`
- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/session_test.go`
- Modify: `cmd/goose-daemon/config_api.go`
- Modify: `cmd/goose-daemon/metrics_handler.go`
- Modify: `cmd/goose-daemon/uidist/*`
- Create: `configs/mcp/servers.yaml.example`
- Create: `configs/mcp/secrets.yaml.example`
- Create: `internal/network/antispoof.go`
- Modify: `internal/network/manager.go`
- Modify: `internal/storage/provisioner.go`
- Create: `internal/storage/mcp_inject_test.go`
- Modify: `e2e_test.sh`
- Modify: `web/src/components/MCPGateway.svelte`
- Modify: `web/src/components/BuiltinPicker.svelte`
- Modify: `web/src/components/BuiltinsModal.svelte`
- Modify: `web/src/components/ProfileModal.svelte`
- Modify: `web/src/components/Settings.svelte`
- Modify: `web/src/components/System.svelte`
- Modify: `web/src/locales/en.json`
- Modify: `web/src/locales/ko.json`

### Phase 4 `v0.7.0` installer/transcript/hardening

- Create: `.github/workflows/release.yml`
- Create: `INSTALL.md`
- Create: `ephemera.service.in`
- Create: `install.sh`
- Create: `uninstall.sh`
- Create/modify: `scripts/build_release.sh`
- Create: `docs/code-review-v0.6.4.md`
- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/main_test.go`
- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/main.go`
- Modify: `cmd/goose-daemon/uidist/*`
- Modify: `internal/storage/provisioner.go`
- Modify: `web/src/components/TaskPanel.svelte`
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`

### Cross-phase anvil documents

- Modify: `CONTEXT.md`
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/ADR_INDEX.md`
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md`
- Modify: `docs/architecture/*.md`
- Modify: `docs/operations/*.md`
- Create: `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`

## Global Invariants

- `POST /vms` is the only response that may expose `agent_token`.
- Daemon restore, snapshot, flock, Web UI, MCP Gateway, metrics, audit, and installer
  surfaces must not expose `agent_token`, Authorization headers, provider keys, profile
  secrets, raw daemon bodies, or local secret file contents.
- `EPHEMERA_*`, `goose-*`, and `ephemera-ctl` remain canonical upstream names.
- anvil aliases are wrappers/docs/config compatibility, not deep upstream renames.
- `cmd/anvil-mcp` remains the IronClaw-facing adapter.
- upstream MCP Gateway remains runtime/operator surface during this parity project.
- Web UI default exposure remains local operator access. External exposure requires TLS
  termination reverse proxy or private network policy.
- Rebase/history rewrite is forbidden. Use merge commits.
- Do not use `git fetch --tags --force`.

---

### Task 1: Prepare Branch, Worktree, and Baseline

**Files:**
- Read: `docs/superpowers/specs/2026-07-02-ephemera-core-service-parity-design.md`
- Read: `docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`
- Modify only if needed: no source files in this task

- [ ] **Step 1: Confirm current branch and pushed baseline**

Run:

```bash
git status --short --branch
git log --oneline -3
```

Expected:

```text
## main...origin/main [ahead N]
<plan commit> docs: plan ephemera core service parity
4c17327 docs: design ephemera core service parity
e69bbfc docs: update current ephemera baseline
```

If the documentation commits are not on the remote and remote backup is approved before
sync work, run:

```bash
git push origin main
```

Expected: push succeeds and `git status --short --branch` no longer reports local
ahead commits.

- [ ] **Step 2: Confirm upstream tags without force-fetching**

Run:

```bash
git ls-remote --tags upstream | rg 'refs/tags/v0\.(4|5|6|7)\.'
```

Expected: output includes `v0.4.0` through `v0.4.5`, `v0.5.0` through `v0.5.5`,
`v0.6.0`, `v0.6.1`, `v0.6.2`, `v0.6.4`, and `v0.7.0`.

- [ ] **Step 3: Fetch only required tag objects**

Run:

```bash
git fetch upstream main tag v0.4.0 tag v0.4.1 tag v0.4.2 tag v0.4.3 tag v0.4.4 tag v0.4.5 tag v0.5.0 tag v0.5.1 tag v0.5.2 tag v0.5.3 tag v0.5.4 tag v0.5.5 tag v0.6.0 tag v0.6.1 tag v0.6.2 tag v0.6.4 tag v0.7.0
```

Expected: no forced tag update warnings.

- [ ] **Step 4: Create dedicated worktree**

Run from `/data/projects/codex-zone/anvil`:

```bash
git worktree add ../anvil-ephemera-parity -b sync/ephemera-v0.7-core-service-parity main
```

Expected: new worktree exists at `/data/projects/codex-zone/anvil-ephemera-parity`
on branch `sync/ephemera-v0.7-core-service-parity`.

- [ ] **Step 5: Run baseline verification in the worktree**

Run:

```bash
cd /data/projects/codex-zone/anvil-ephemera-parity
git diff --check
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands pass before upstream merge work begins.

- [ ] **Step 6: Record branch start**

Run:

```bash
git status --short --branch
git merge-base HEAD upstream/main
git describe --tags --exact-match "$(git merge-base HEAD upstream/main)" || true
```

Expected: branch is clean. Merge base should resolve to the current anvil/upstream
common baseline, expected `v0.3.6` before sync.

---

### Task 2: Phase 1 Merge `v0.4.0`-`v0.4.5` Runtime Stability

**Files:**
- Use detailed plan: `docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`
- Modify: files listed under Phase 1 in this plan
- Test: daemon/storage/network/vm/orchestrator tests

- [ ] **Step 1: Execute existing v0.4 detailed plan**

Open and follow:

```bash
sed -n '1,260p' docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md
```

Expected: the implementation follows tag order `v0.4.0`, `v0.4.1`, `v0.4.2`,
`v0.4.3`, `v0.4.4`, `v0.4.5`.

- [ ] **Step 2: Merge each upstream tag with a merge commit**

Run these commands one tag at a time, resolving conflicts and committing adaptation
work between tags:

```bash
git merge --no-ff v0.4.0 -m "merge: sync ephemera v0.4.0"
git merge --no-ff v0.4.1 -m "merge: sync ephemera v0.4.1"
git merge --no-ff v0.4.2 -m "merge: sync ephemera v0.4.2"
git merge --no-ff v0.4.3 -m "merge: sync ephemera v0.4.3"
git merge --no-ff v0.4.4 -m "merge: sync ephemera v0.4.4"
git merge --no-ff v0.4.5 -m "merge: sync ephemera v0.4.5"
```

Expected: each tag has a merge commit. If a merge conflicts, resolve conflicts,
run the targeted tests in the detailed v0.4 plan, then continue.

- [ ] **Step 3: Preserve v0.4 anvil adaptation rules**

For every conflict, apply these choices:

```text
Keep upstream storage/recovery logic.
Keep anvil tenant_id and egress_policy fields in VM/snapshot/restore paths.
Keep anvil snapshot export/import scrubbing.
Keep anvil scheduler placement and routed flock state separated from daemon flock state.
Keep EPHEMERA_* canonical env names and ANVIL_* aliases.
Keep EPHEMERA_DISK_MODE default plain until KVM burn-in decides otherwise.
Keep auto-snapshot opt-in/off.
Redact all agent_token fields outside POST /vms.
Do not expose flock broadcast as an anvil_* MCP tool in this phase.
```

Expected: conflicts resolve without removing existing anvil tests.

- [ ] **Step 4: Run Phase 1 CI-safe gate**

Run:

```bash
git diff --check
go test ./internal/storage ./internal/network ./internal/vm ./internal/orchestrator -count=1
go test ./cmd/goose-daemon ./cmd/goose-agent ./cmd/ephemera-ctl -count=1
go test ./internal/anvilmcp ./cmd/anvil-mcp ./cmd/anvil-scheduler -count=1
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/ephemera-ctl
```

Expected: all commands pass.

- [ ] **Step 5: Run Phase 1 KVM gate**

Run on a KVM-capable host:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
scripts/vm-workload-e2e.sh
```

Expected: all smoke paths pass. If provider keys are missing, record which
provider-key-dependent substeps were skipped and keep key-free lifecycle checks green.

- [ ] **Step 6: Commit Phase 1 docs**

Update at least:

```text
CONTEXT.md
README.md
RELEASE_NOTES.md
docs/ADR_INDEX.md
docs/PUBLIC_RELEASE_BOUNDARY.md
docs/operations/release-checklist.md
docs/operations/upstream-sync-policy.md
docs/architecture/runtime-architecture.md
docs/architecture/service-logic.md
```

Then run:

```bash
git add CONTEXT.md README.md RELEASE_NOTES.md docs/ADR_INDEX.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/release-checklist.md docs/operations/upstream-sync-policy.md docs/architecture/runtime-architecture.md docs/architecture/service-logic.md
git commit -m "docs: document ephemera v0.4 runtime support"
```

Expected: docs say `v0.4.0`-`v0.4.5` is supported/adapted, not just planned.

---

### Task 3: Phase 2 Merge `v0.5.0` Web UI and Profile Config API

**Files:**
- Modify: files listed under Phase 2
- Test: `cmd/goose-daemon/config_api_test.go`
- Test: `cmd/goose-daemon/ui_test.go`
- Test: `cmd/goose-agent/session_test.go`
- Test: `web/` build

- [ ] **Step 1: Merge upstream `v0.5.0`**

Run:

```bash
git merge --no-ff v0.5.0 -m "merge: sync ephemera v0.5.0"
```

Expected: conflicts, if any, are concentrated in daemon API/config, README,
release notes, `cmd/goose-agent/main.go`, and Web UI assets.

- [ ] **Step 2: Resolve v0.5.0 with anvil surface policy**

Apply these conflict decisions:

```text
Keep daemon /ui/ and /config/profiles endpoints.
Keep Web UI outside auth only for static bundle and login page.
Keep every data API behind bearer auth when auth is configured.
Keep profile config handlers from reading or writing goose-secrets.yaml.
Keep anvil ANVIL_* aliases and EPHEMERA_* canonical env names.
Keep anvil tenant_id and egress_policy propagation in spawn/snapshot/restore paths.
Keep cmd/anvil-mcp behavior unchanged unless daemon wire structs require an additive field.
Keep Web UI operator surface separate from IronClaw MCP surface.
```

Expected: `/ui/`, `/config/profiles`, multi-turn `session`, and graceful delete
are present without weakening anvil auth/token rules.

- [ ] **Step 3: Verify v0.5.0 KVM-free behavior**

Run:

```bash
go test ./cmd/goose-agent -run 'Test.*Session|Test.*Task|Test.*Transcript' -count=1
go test ./cmd/goose-daemon -run 'Test.*UI|Test.*Config|Test.*Profile|Test.*Delete|Test.*Token' -count=1
go test ./cmd/goose-daemon ./cmd/goose-agent -count=1
```

Expected: daemon and agent tests pass. If no transcript tests exist until v0.7,
the first command should still pass with the available session/task tests.

- [ ] **Step 4: Build Web UI and embedded assets**

Run:

```bash
npm --prefix web install
npm --prefix web run build
go test ./cmd/goose-daemon -run 'Test.*UI' -count=1
```

Expected: `web` build succeeds and embedded `cmd/goose-daemon/uidist` tests pass.

- [ ] **Step 5: Commit v0.5.0 adaptation**

Run:

```bash
git status --short
git add cmd/goose-agent cmd/goose-daemon web README.md RELEASE_NOTES.md CONTRIBUTING.md scripts/build_image.sh
git commit -m "fix(runtime): adapt ephemera v0.5.0 web UI for anvil"
```

Expected: one adaptation commit after the `v0.5.0` merge commit.

---

### Task 4: Phase 2 Merge `v0.5.1`-`v0.5.5` Operator Service

**Files:**
- Modify: files listed under Phase 2
- Test: daemon config/profile/orchestrator/snapshot tests
- Test: Web UI build

- [ ] **Step 1: Merge upstream tags in order**

Run each merge after the previous tag's adaptation commit is clean:

```bash
git merge --no-ff v0.5.1 -m "merge: sync ephemera v0.5.1"
git merge --no-ff v0.5.2 -m "merge: sync ephemera v0.5.2"
git merge --no-ff v0.5.3 -m "merge: sync ephemera v0.5.3"
git merge --no-ff v0.5.4 -m "merge: sync ephemera v0.5.4"
git merge --no-ff v0.5.5 -m "merge: sync ephemera v0.5.5"
```

Expected: merge commits preserve upstream history.

- [ ] **Step 2: Preserve profile and secret safety**

For config/profile conflicts, preserve these rules:

```text
GET /config/providers exposes key availability only, never key values.
GET /config/clients exposes client name and expiry only, never token values.
GET/PUT/DELETE /config/profiles/{name}/system uses system.md only and caps body size at 64 KiB.
Profile delete refuses running-use profiles with 409.
Profile edits affect future spawns only.
Default profile remains reserved.
Path traversal through profile names is rejected.
```

Expected: tests cover provider secret non-exposure, client token non-exposure,
profile path traversal, default profile rejection, and profile-in-use guard.

- [ ] **Step 3: Preserve flock profile workflow and scheduler separation**

For orchestrator/flock conflicts, preserve these rules:

```text
Flock role label and profile are separate.
AgentInfo.Profile survives restart, add-agent, and role-change.
Daemon single-host flock lifecycle does not manage routed members-only cross-host flock state.
Town Wall control-plane messages use control-plane author.
Operator-authored Web UI feed posts use operator author.
No agent_token is exposed in flock list/get/broadcast/add-agent Web UI responses.
```

Expected: existing routed flock tests and upstream single-host flock tests both pass.

- [ ] **Step 4: Preserve snapshot sizing and restored VM dependency**

For snapshot conflicts, preserve these rules:

```text
Snapshot metadata records vcpu_count and mem_size_mib when upstream provides it.
Legacy snapshots without sizing fall back to historical sizing.
SourceSnapshotID for restored VMs is protected from GC while a restored VM references it.
anvil snapshot export/import continues to scrub local/token fields.
```

Expected: snapshot metadata round-trip, restored sizing, and GC dependency tests pass.

- [ ] **Step 5: Run Phase 2 CI-safe gate**

Run:

```bash
git diff --check
go test ./cmd/goose-daemon -run 'Test.*(Config|Provider|Preset|Profile|System|Client|Monitoring|UI|Flock|Snapshot|Restore|Token)' -count=1
go test ./cmd/goose-agent -count=1
go test ./internal/orchestrator ./internal/storage ./internal/vm -count=1
go test ./internal/anvilmcp ./cmd/anvil-mcp ./cmd/anvil-scheduler -count=1
npm --prefix web run build
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands pass.

- [ ] **Step 6: Run Phase 2 KVM gate**

Run:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
scripts/vm-workload-e2e.sh
```

Expected: lifecycle, snapshot/restore, flock, workload, and Web UI-backed daemon
paths remain healthy.

- [ ] **Step 7: Commit v0.5 adaptation and docs**

Run:

```bash
git add cmd/goose-agent cmd/goose-daemon internal/orchestrator internal/storage internal/vm web configs webdev_demo.sh observability_demo.sh e2e_test.sh README.md RELEASE_NOTES.md CONTRIBUTING.md
git commit -m "fix(runtime): adapt ephemera v0.5 operator service for anvil"
git add CONTEXT.md docs/ADR_INDEX.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/release-checklist.md docs/architecture docs/operations
git commit -m "docs: document ephemera v0.5 operator support"
```

Expected: docs describe Web UI/profile/config/monitoring as runtime/operator surface.

---

### Task 5: Phase 3 Merge `v0.6.0` MCP Gateway Core

**Files:**
- Modify/create: files listed under Phase 3
- Test: `internal/mcpgateway/*_test.go`
- Test: `cmd/goose-daemon/mcp_*_test.go`
- Test: `internal/storage/mcp_inject_test.go`

- [ ] **Step 1: Merge upstream `v0.6.0`**

Run:

```bash
git merge --no-ff v0.6.0 -m "merge: sync ephemera v0.6.0"
```

Expected: new `internal/mcpgateway` package, daemon MCP config handlers, builtins
handlers, `configs/mcp/*.example`, and Web UI MCP components are introduced.

- [ ] **Step 2: Preserve MCP boundary**

Apply these conflict decisions:

```text
MCP Gateway is runtime/operator surface.
cmd/anvil-mcp remains IronClaw-facing adapter.
Gateway audit/mcp.jsonl stores metadata only, never args or results.
Gateway backend credentials stay host-side in configs/mcp/secrets.yaml.
VM receives only gateway URL/config, never backend credentials.
Profile policy cannot widen access beyond servers.yaml.
ANVIL_MCP_* env for cmd/anvil-mcp keeps its current meaning.
EPHEMERA_MCP_* env is for upstream runtime gateway.
```

Expected: docs and code do not describe MCP Gateway as a replacement for `cmd/anvil-mcp`.

- [ ] **Step 3: Run v0.6.0 targeted tests**

Run:

```bash
go test ./internal/mcpgateway -count=1
go test ./cmd/goose-daemon -run 'Test.*(MCP|Builtin|Profile|Config)' -count=1
go test ./internal/storage -run 'Test.*MCP|Test.*Inject' -count=1
go test ./cmd/goose-agent -count=1
```

Expected: gateway, builtins, profile binding, and VM injection tests pass.

- [ ] **Step 4: Build Web UI and daemon**

Run:

```bash
npm --prefix web run build
go build ./cmd/goose-daemon
```

Expected: embedded System MCP Gateway UI builds.

- [ ] **Step 5: Commit v0.6.0 adaptation**

Run:

```bash
git add internal/mcpgateway internal/network internal/storage cmd/goose-daemon cmd/goose-agent configs/mcp web e2e_test.sh README.md RELEASE_NOTES.md
git commit -m "fix(runtime): adapt ephemera v0.6.0 MCP gateway for anvil"
```

Expected: one adaptation commit after the `v0.6.0` merge.

---

### Task 6: Phase 3 Merge `v0.6.1`-`v0.6.4` Gateway Hardening

**Files:**
- Modify/create: files listed under Phase 3
- Test: `internal/mcpgateway/backend_stdio_test.go`
- Test: `internal/mcpgateway/gateway_test.go`
- Test: `cmd/goose-daemon/mcp_api_test.go`
- Test: `internal/network/*_test.go`

- [ ] **Step 1: Merge upstream tags in order**

Run:

```bash
git merge --no-ff v0.6.1 -m "merge: sync ephemera v0.6.1"
git merge --no-ff v0.6.2 -m "merge: sync ephemera v0.6.2"
git merge --no-ff v0.6.4 -m "merge: sync ephemera v0.6.4"
```

Expected: anti-spoof, rate limit, resources/prompts, granular policy, stdio backend,
and shutdown reap support are merged.

- [ ] **Step 2: Preserve gateway hardening**

For conflicts, enforce:

```text
EPHEMERA_NET_ANTISPOOF default on, best-effort disable if ebtables missing.
EPHEMERA_MCP_RATE default 0 means unlimited.
Rate limit key is per VM and backend server.
resources/prompts calls share policy and rate limit with tool calls.
audit/mcp.jsonl includes kind for tool/resource/prompt.
GET /config/mcp/servers exposes transport and command but never args or credentials.
Stdio backend child env is rebuilt from scratch.
credential_env is required with credential for stdio secrets.
Stdio child does not inherit daemon EPHEMERA_* environment.
Shutdown reaps stdio process groups.
```

Expected: security properties from upstream remain intact and are documented as
runtime/operator surface.

- [ ] **Step 3: Run Phase 3 CI-safe gate**

Run:

```bash
git diff --check
go test ./internal/mcpgateway -count=1
go test ./cmd/goose-daemon -run 'Test.*(MCP|Builtin|Config|Gateway|Policy|Rate|Stdio|Token)' -count=1
go test ./internal/network -count=1
go test ./internal/storage -run 'Test.*MCP|Test.*Inject' -count=1
go test ./cmd/goose-agent -count=1
npm --prefix web run build
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands pass.

- [ ] **Step 4: Run Phase 3 KVM/MCP Gateway gate**

Run:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

Expected: `e2e_test.sh` gateway steps pass or explicitly skip only network/provider-key
dependent checks. `anvil-mcp` smoke remains unaffected by runtime MCP Gateway.

- [ ] **Step 5: Commit v0.6 adaptation and docs**

Run:

```bash
git add internal/mcpgateway internal/network internal/storage cmd/goose-daemon cmd/goose-agent configs/mcp web e2e_test.sh README.md RELEASE_NOTES.md go.mod
git commit -m "fix(runtime): adapt ephemera v0.6 MCP gateway for anvil"
git add CONTEXT.md docs/ADR_INDEX.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/release-checklist.md docs/architecture docs/operations
git commit -m "docs: document ephemera v0.6 MCP gateway support"
```

Expected: docs clearly separate `EPHEMERA_MCP_*` runtime Gateway from `ANVIL_MCP_*`
IronClaw adapter configuration.

---

### Task 7: Phase 4 Merge `v0.7.0` Installer, Transcript, and Hardening

**Files:**
- Modify/create: files listed under Phase 4
- Test: `cmd/goose-agent/main_test.go`
- Test: `cmd/goose-daemon/api_test.go`
- Test: installer shell syntax

- [ ] **Step 1: Merge upstream `v0.7.0`**

Run:

```bash
git merge --no-ff v0.7.0 -m "merge: sync ephemera v0.7.0"
```

Expected: installer files, release workflow, transcript restore, and upstream hardening
are merged.

- [ ] **Step 2: Reconcile already-backported hardening**

For conflicts in kernel verification, `waitForAgent`, and `EPHEMERA_HOME`, keep the
implementation that satisfies all of:

```text
Kernel download verifies pinned SHA256 before publishing artifacts/vmlinux.bin.
Partial kernel file is not left behind on mismatch or write failure.
waitForAgent uses per-probe HTTP timeout inside the overall readiness deadline.
EPHEMERA_HOME controls daemon work directory.
Unset EPHEMERA_HOME preserves current working directory behavior.
Tests for kernel SHA, waitForAgent hung probe, and workdir resolution pass.
```

Expected: no duplicate logic and no regression from current anvil backports.

- [ ] **Step 3: Preserve anvil installer alias policy**

For installer/service conflicts, enforce:

```text
Upstream ephemera canonical install remains usable.
anvil docs explain this as runtime/operator installer support.
Systemd service may remain ephemera canonical unless alias wrapper is implemented and documented.
Configs do not commit real secrets.
EPHEMERA_HOME is used for service workdir.
External Web UI exposure is documented only behind reverse proxy/TLS or private network.
```

Expected: installer works for upstream-compatible ephemera service while anvil docs
tell operators how it fits in anvil.

- [ ] **Step 4: Preserve transcript safety**

For transcript restore conflicts, enforce:

```text
GET /vms/{id}/sessions/{name}/transcript is bearer-protected through daemon proxy.
goose-agent transcript export is read-only and does not call a model.
Transcript cache does not include provider key, control-plane token, or agent_token.
Transcript UI renders prior turns without exposing daemon auth.
```

Expected: transcript restore is visible in Web UI and does not create a new secret
exposure path.

- [ ] **Step 5: Run Phase 4 CI-safe gate**

Run:

```bash
git diff --check
go test ./cmd/goose-agent -run 'Test.*(Session|Transcript|Task)' -count=1
go test ./cmd/goose-daemon -run 'Test.*(WaitForAgent|ResolveWorkDir|Kernel|Transcript|Session|Token)' -count=1
go test ./internal/storage -run 'TestEnsureKernel|Test.*Kernel' -count=1
bash -n install.sh
bash -n uninstall.sh
bash -n scripts/build_release.sh
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
npm --prefix web run build
```

Expected: all commands pass.

- [ ] **Step 6: Run Phase 4 KVM and installer gate**

Run on a KVM-capable systemd host:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
scripts/vm-workload-e2e.sh
bash scripts/build_release.sh
bash install.sh --help
bash uninstall.sh --help
```

Expected: runtime smoke passes. Release build creates expected tarballs and checksums.
Installer help paths pass without mutating system state.

- [ ] **Step 7: Commit v0.7 adaptation and docs**

Run:

```bash
git add .github/workflows/release.yml INSTALL.md ephemera.service.in install.sh uninstall.sh scripts/build_release.sh docs/code-review-v0.6.4.md cmd/goose-agent cmd/goose-daemon internal/storage web README.md RELEASE_NOTES.md
git commit -m "fix(runtime): adapt ephemera v0.7 installer and transcript support"
git add CONTEXT.md docs/ADR_INDEX.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/release-checklist.md docs/architecture docs/operations
git commit -m "docs: document ephemera v0.7 core service parity"
```

Expected: docs mark upstream `v0.7.0` as supported/adapted, with remaining deferred
items listed explicitly.

---

### Task 8: Cross-Phase Parity Matrix and Release Candidate Gate

**Files:**
- Create: `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`
- Modify: `CONTEXT.md`
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/ADR_INDEX.md`
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `docs/operations/upstream-sync-policy.md`

- [ ] **Step 1: Generate upstream feature inventory**

Run:

```bash
git log --oneline v0.3.6..v0.7.0
git diff --name-status v0.3.6..v0.7.0
git tag --list 'v0.*' --sort=version:refname
```

Expected: inventory covers all upstream tags from `v0.4.0` to `v0.7.0`.

- [ ] **Step 2: Write parity review document**

Create `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md` with this
structure:

```markdown
# ephemera v0.5.0-v0.7.0 core service parity review

## 상태

- 검토일: 2026-07-02 이후 sync branch 검증일
- upstream 범위: `v0.4.5..v0.7.0`
- anvil branch: `sync/ephemera-v0.7-core-service-parity`
- 기준: ephemera core service 100% runtime/operator support

## 요약

| 범위 | 주제 | anvil 상태 | 근거 |
|---|---|---|---|
| `v0.5.0`-`v0.5.5` | Web UI/profile/operator console | adapted | operator surface로 지원, IronClaw 필수 경로 아님 |
| `v0.6.0`-`v0.6.4` | MCP Gateway | adapted | runtime/operator Gateway로 지원, `cmd/anvil-mcp`와 분리 |
| `v0.7.0` | installer/transcript/hardening | adapted | installer는 upstream canonical 유지 + anvil docs/alias |

## Feature parity matrix

| upstream feature | tag | anvil status | evidence | notes |
|---|---|---|---|---|
```

Then fill every upstream feature with one of `adopted`, `adapted`, `deferred`, or
`excluded`. Do not leave blank rows.

- [ ] **Step 3: Update truth docs**

Update:

```text
CONTEXT.md
README.md
RELEASE_NOTES.md
docs/ADR_INDEX.md
docs/PUBLIC_RELEASE_BOUNDARY.md
docs/operations/release-checklist.md
docs/operations/upstream-sync-policy.md
```

Required final wording:

```text
anvil runtime/operator baseline supports upstream ephemera v0.7.0 with anvil
adaptations for token redaction, tenant/egress, scheduler, audit, and IronClaw MCP
surface separation.
```

Expected: no document still says `v0.5.0`-`v0.7.0` are unreviewed backlog after the
parity branch is complete.

- [ ] **Step 4: Run complete verification gate**

Run:

```bash
git diff --check
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/ephemera-ctl
npm --prefix web run build
bash -n install.sh
bash -n uninstall.sh
bash -n scripts/build_release.sh
```

Expected: all CI-safe gates pass.

- [ ] **Step 5: Run complete KVM gate**

Run:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
scripts/vm-workload-e2e.sh
```

Expected: all KVM gates pass or provider-key-dependent substeps are explicitly
recorded as skipped. KVM-independent parts must pass.

- [ ] **Step 6: Run secret/token artifact scan**

Run:

```bash
rg -n 'agent_token|Authorization: Bearer|goose-secrets|API_KEY|ANTHROPIC_API_KEY|OPENAI_API_KEY|GOOGLE_API_KEY' README.md RELEASE_NOTES.md docs cmd/goose-daemon/uidist web/src configs/*.example configs/mcp/*.example || true
```

Expected: matches are either policy text, example variable names, redaction tests,
or sample/example files. No real token, bearer value, provider key, or raw
secret appears.

- [ ] **Step 7: Commit parity review**

Run:

```bash
git add docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md CONTEXT.md README.md RELEASE_NOTES.md docs/ADR_INDEX.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/release-checklist.md docs/operations/upstream-sync-policy.md docs/architecture docs/operations
git commit -m "docs: record ephemera v0.7 core service parity"
```

Expected: final docs commit closes the parity matrix and release candidate criteria.

---

### Task 9: Final Review, Push, and PR Preparation

**Files:**
- Read: all changed files from `git diff --name-only main...HEAD`
- Modify: only docs/tests if review finds factual issues

- [ ] **Step 1: Show final branch summary**

Run:

```bash
git status --short --branch
git log --oneline main..HEAD
git diff --stat main...HEAD
git diff --name-only main...HEAD
```

Expected: branch is clean. Commit history shows upstream merge commits and adaptation
commits in chronological phase order.

- [ ] **Step 2: Run final verification**

Run:

```bash
git diff --check
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
npm --prefix web run build
bash -n install.sh
bash -n uninstall.sh
bash -n scripts/build_release.sh
```

Expected: all final verification commands pass.

- [ ] **Step 3: Run code review before push**

Run the repository's preferred review workflow. If using gstack review:

```bash
git diff main...HEAD --stat
```

Then inspect manually for:

```text
token exposure
auth bypass
raw secret in Web UI bundle
MCP Gateway vs anvil MCP confusion
snapshot GC dependency regression
installer destructive behavior
unexpected deletion of anvil scheduler/tenant/egress paths
```

Expected: no blocking issues remain.

- [ ] **Step 4: Push sync branch**

Run:

```bash
git push -u origin sync/ephemera-v0.7-core-service-parity
```

Expected: branch is available remotely for CI and review.

- [ ] **Step 5: PR body checklist**

Use this PR body skeleton:

```markdown
## Summary

- Syncs ephemera core runtime/operator service through upstream `v0.7.0`.
- Preserves anvil IronClaw-facing `anvil_*` MCP boundary.
- Supports Web UI, profile/config API, monitoring console, MCP Gateway, installer,
  transcript restore, and hardening as runtime/operator surface.

## Security boundaries

- `agent_token` remains exposed only by `POST /vms`.
- MCP Gateway is runtime/operator surface, not a replacement for `cmd/anvil-mcp`.
- Web UI default exposure remains local/operator; external exposure requires reverse
  proxy/TLS or private network policy.
- Secrets stay out of audit, metrics, Web UI bundle, docs, and release artifacts.

## Verification

- [ ] `git diff --check`
- [ ] `go test ./... -count=1`
- [ ] `go build ./cmd/goose-daemon`
- [ ] `go build ./cmd/anvil-mcp`
- [ ] `go build ./cmd/anvil-scheduler`
- [ ] `go build ./cmd/ephemera-ctl`
- [ ] `npm --prefix web run build`
- [ ] `bash -n install.sh`
- [ ] `bash -n uninstall.sh`
- [ ] `bash -n scripts/build_release.sh`
- [ ] `sudo bash e2e_test.sh`
- [ ] `scripts/anvil-mcp-e2e.sh lifecycle`
- [ ] `scripts/anvil-mcp-e2e.sh semantic`
- [ ] `scripts/anvil-mcp-e2e.sh flock`
- [ ] `scripts/vm-workload-e2e.sh`

## Parity matrix

See `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`.
```

Expected: PR reviewers can verify scope, security boundaries, and test evidence without
digging through the whole branch first.
