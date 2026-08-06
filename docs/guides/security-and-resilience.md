# anvil 보안·복원력 가이드

> 보안 모델, 알려진 제약, control plane 인증, Resilience, Observability, Known Limitations를 다룹니다.
> 프로젝트 개요는 [README](../../README.md), API 참조는 [api-reference.md](api-reference.md)를 참고하세요.

## 보안 모델

- **client -> control plane**:
  `EPHEMERA_API_TOKENS`/`EPHEMERA_API_TOKEN` 또는
  `ANVIL_API_TOKENS`/`ANVIL_API_TOKEN` Bearer token을 사용한다.

- **control plane -> guest agent**:
  VM별 Bearer token을 사용한다.

- **guest task isolation**:
  Firecracker MicroVM + KVM boundary로 격리한다.

- **guest network**:
  host-only `10.0.1.0/24` network와 `goose-br0` bridge를 사용한다.

- **egress SNI 필터** (ADR-0002):
  `profile` egress policy는 CIDR/host allowlist에 더해 `allow_sni` 도메인-정밀
  :443 transparent SNI 필터를 강제한다. TCP:443은 파싱된 TLS ClientHello SNI,
  QUIC/UDP:443은 자체 구현 Initial 복호(HKDF+AES-128-GCM+header protection)로
  얻은 SNI를 같은 in-process NFQUEUE verdict 루프(queue 88)가 검사해 승인
  흐름에 connmark를 찍는다. verdict 루프가 준비되지 않은 host는 규칙을 깔지
  않고 spawn 자체를 fail-closed로 거부한다(`--queue-bypass` 배제). CIDR
  allowlist가 SNI 검사보다 상위(additive)다. 상세는
  [ADR-0002](../adr/0002-egress-sni-transparent-filter.md)를 참고한다.

- **외부 공개**:
  TLS 종료 reverse proxy 뒤에서 운영하고 운영 환경에서는 `EPHEMERA_API_TOKENS`를
  설정한다. 자세한 정책은
  [security-policy.md](../operations/security-policy.md)를 참조한다.

- **secret**:
  gitignore된 로컬 config에서 guest disk로 주입한다.

실제 API key는 문서, issue, commit, 채팅에 남기지 않는다.


## 알려진 제약

- snapshot create/restore/delete/GC lifecycle operation은 한 번에 하나만 실행된다.
- source VM이 실행 중인 동안 해당 VM의 snapshot restore는 거부된다.
- diff snapshot은 memory만 diff다. rootfs는 snapshot마다 full copy다.
- diff restore는 임시 merged memory file을 만들 disk space가 필요하다.
- daemon restart 후 spawn-path VM은 `vms/<vm_id>/state.json` 기반으로 cold-restart된다.
  같은 VM ID, IP, TAP, MAC, agent token, agent URL을 유지하지만 memory state와
  in-flight task는 보존하지 않는다.
- snapshot-restored VM(비어 있지 않은 `SourceSnapshotID`)은 daemon restart 후
  source snapshot에서 자동 re-restore된다(v0.4.5) — memory state는 보존하지 않아
  수동 re-restore와 동일하며, source snapshot이 이미 삭제됐으면 복구하지 않고
  drop한다. `EPHEMERA_DISK_MODE=cow`로 생성된 COW spawn VM도 v0.4.0부터
  `state.json`과 `.cow` exception store가 남아 있으면 dm-snapshot을 재구성해
  자동 복구된다.
- watchdog이 표시한 `dead` status는 `flocks/<flock_id>/metadata.json`에
  persist된다. per-agent restart 또는 watchdog auto-heal opt-in이 상태를 다시
  `ready`로 바꾸는 명시 경로다.
- control-plane auth가 켜진 flock VM은 host가 `/root/.ephemera-cp-token`을
  자동 주입한다. 주입되는 값은 운영자 bearer가 아니라 **그 flock의 guest 능력
  토큰**이며(ADR-0003), 해당 flock의 wall sub-path와 `call` 진입만 admit한다.
  만료가 없어 폐기 수단은 flock 삭제뿐이다. `EPHEMERA_API_TOKENS_FILE` +
  `SIGHUP` token rotation은 능력 토큰 도입 이전에 spawn된 VM에만 적용된다.
- control-plane token 환경 변수를 설정하지 않으면 API 인증이 비활성화된다.
- MCP v1은 snapshot/restore tool을 제공하지만 snapshot alias와 session alias
  영속화는 별개다. snapshot alias는 제공하지 않고, VM `session_name` alias는
  `session_store_path` 또는 `ANVIL_MCP_SESSION_STORE`가 설정된 경우에만 local JSON
  file로 영속화한다.

## Control plane API authentication

#### Per-client tokens (recommended)

```bash
ALICE_TOKEN=$(openssl rand -hex 32)
BOB_TOKEN=$(openssl rand -hex 32)

export EPHEMERA_API_TOKENS="alice:$ALICE_TOKEN,bob:$BOB_TOKEN"
sudo -E ./ephemera-daemon
```

Startup log:
```
Control plane API on 127.0.0.1:3000  (auth: Bearer token (2 client(s): alice, bob))
```

Each request is logged with the authenticated client name:
```
[alice] POST /vms
[bob]   GET  /vms
```

#### Single-token fallback

```bash
export EPHEMERA_API_TOKEN=$(openssl rand -hex 32)
sudo -E ./ephemera-daemon
```

Treated as a single client named `default`.

If neither variable is set, a startup warning is logged and the API is unauthenticated — **never expose the control plane without a token**.

#### Token hot reload (SIGHUP)

API tokens can be updated without restarting the daemon or interrupting running VMs. The recommended path since v0.3.4 is a file source — env vars are captured at exec, so a SIGHUP can only observe a value change when the daemon reads from disk:

```bash
# One-time setup: point the daemon at a tokens file.
echo "alice:$ALICE_TOKEN,bob:$BOB_TOKEN" > /etc/ephemera/tokens
chmod 0600 /etc/ephemera/tokens
EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens \
    ./ephemera-daemon &

# Later: rotate by editing the file and signalling.
echo "alice:$NEW_ALICE,carol:$CAROL_TOKEN" > /etc/ephemera/tokens
kill -HUP $(pgrep ephemera-daemon)
```

`ReloadClients` re-reads the file, swaps the in-memory client list under `clientsMu`, **and (v0.3.4) fans the first non-expired client's token out over vsock to every running VM whose `/root/.ephemera-cp-token` this daemon injected with its own operator bearer** (`SET_CP_TOKEN` command, atomic rewrite of the file). The in-VM `/townwall/post` forwarder picks up the rotated bearer on the next request without any VM restart. Rotation is strictly a *re-injection* of a token the daemon already placed: a VM that was never given the daemon's bearer is never given one by SIGHUP. See [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034) for the exact eligibility rule.

| Scenario | Action |
|----------|--------|
| Adding a new client | Edit `EPHEMERA_API_TOKENS_FILE` → SIGHUP |
| Rotating the primary CP token | Edit file → SIGHUP. Since [ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md) flock members do not use this token, so rotating it does not affect them; VMs spawned before ADR-0003 still get `/root/.ephemera-cp-token` updated automatically (v0.3.4+) |
| Rotating a flock guest's credential | **Not supported as a rotation.** A flock's capability token has no expiry and no rotation path — delete the flock (revokes admission + removes the token file) or respawn the member with `POST /flocks/{id}/agents/{agent_id}/restart` |
| Emergency revocation | Edit file → SIGHUP — **no VM interruption**. Note this revokes operator bearers only; a flock guest capability token is revoked by deleting its flock |
| Legacy `EPHEMERA_API_TOKENS` env (no file) | Still works for the `cp.clients` swap, but does not see env-value changes without daemon restart. Use `_TOKENS_FILE` for live rotation. |

#### Token TTL & rotation (v0.4.1)

A client entry may carry an optional expiry as a third colon-separated field —
`name:token:expires` — where `expires` is **RFC3339** (e.g. `2026-06-01T00:00:00Z`)
or **Unix seconds**. A two-field `name:token` never expires (backward compatible).

```bash
# A short-lived CI token plus a never-expiring operator token.
printf 'ops:%s\nci:%s:2026-06-01T00:00:00Z\n' "$OPS_TOKEN" "$CI_TOKEN" > /etc/ephemera/tokens
```

- Expiry is enforced **per request**: a matched-but-expired token returns `401`
  (identical body to an unknown token; only the server-side log + the
  `ephemera_auth_total{outcome="expired"}` metric distinguish it). No background
  reaper — checking at request time is sufficient.
- Tokens may themselves contain `:`; the expiry is recognized only when the
  trailing colon-separated field parses as a timestamp, so an existing
  colon-bearing token keeps working.
- **Primary (CP) token selection:** the token injected into flock VMs is the
  **first non-expired** client (not blindly the first), so letting a primary
  token expire does not break in-VM `/townwall/post`. If every token has expired,
  an empty token is propagated (the forwarder then calls unauthenticated) and a
  warning is logged. Keep at least one never-expiring client for VM callbacks.

### Access audit log (v0.4.1)

Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl`
(on by default; set `EPHEMERA_AUDIT_DISABLE=true` to turn off). Each record is
`{ts, client, method, path, status, duration_ms, remote_addr, bytes}` — it
**never contains tokens, the `Authorization` header, request/response bodies, or
the query string**. Unauthenticated requests record `client="-"`. `/metrics` is
not audited (to avoid flooding the log with scrapes).

The file is size-rotated (`EPHEMERA_AUDIT_MAX_MIB`, default 100) keeping
`EPHEMERA_AUDIT_KEEP` (default 5) generations. Query recent entries:

```bash
curl -H "Authorization: Bearer $TOKEN" \
    "http://127.0.0.1:3000/audit?limit=100&client=alice&status=200&method=GET"
# → JSON array, newest first
```

`GET /audit` is itself authenticated (and audited). Filters `client`, `status`,
`method` are optional; `limit` defaults to 100, capped at 1000.

---

### goose-agent authentication

Each VM's agent is protected by a unique 32-byte random Bearer token generated at spawn time and written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk. The token is returned only in the `POST /vms` response; snapshot restore responses do not expose it.

- `POST /tasks` and `POST /stop` require `Authorization: Bearer <agent_token>`
- `GET /health` is always open (used by the control plane's internal health poller)
- The token is tied to the VM's disk and persists across snapshot/restore cycles

---

### TLS and network exposure

By default the control plane binds to `127.0.0.1:3000` (localhost only). Place a TLS-terminating reverse proxy in front for external access.

#### Step 1 — allow external binding

```bash
export EPHEMERA_API_ADDR=0.0.0.0:3000
sudo -E ./ephemera-daemon
```

#### Step 2 — configure a reverse proxy

**Caddy** (automatic HTTPS via Let's Encrypt — recommended):

`/etc/caddy/Caddyfile`:
```
api.example.com {
    reverse_proxy localhost:3000
}
```

```bash
sudo apt-get install -y caddy
sudo systemctl restart caddy
```

**Nginx** (manual certificate):

`/etc/nginx/sites-available/ephemera`:
```nginx
server {
    listen 443 ssl;
    server_name api.example.com;

    ssl_certificate     /etc/ssl/certs/ephemera.crt;
    ssl_certificate_key /etc/ssl/private/ephemera.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    location / {
        proxy_pass         http://127.0.0.1:3000;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_read_timeout 300s;   # POST /vms/*/snapshot can take several minutes
    }
}

server {
    listen 80;
    server_name api.example.com;
    return 301 https://$host$request_uri;
}
```

```bash
sudo ln -s /etc/nginx/sites-available/ephemera /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl restart nginx
```

#### Step 3 — call via HTTPS

```bash
curl -X POST https://api.example.com/vms \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json"
```

### VM isolation

- Each VM runs in a separate KVM hardware boundary.
- Each VM gets a **cloned** rootfs — no shared filesystem state between VMs.
- Goose config and API keys are injected at provision time and exist only inside the ephemeral VM disk.
- On teardown: `micro-init` calls `poweroff(2)`, TAP device is deleted, disk is wiped, IP is returned to pool.

---

## Resilience

v0.3.1 hardened Goosetown for long-running flock workloads (health watchdog, flock metadata persistence, monotonic `seq`). v0.3.2 extends this with **live VM cold-restart**: any VM that was running when the daemon stops is automatically brought back up on the next daemon start, with the same IP, TAP, MAC, agent token, and `vm_id`. All features preserve v0.3.0 wire compatibility (`seq` is a new JSON field; watchdog is server-side only; persistence touches only `flocks/<id>/metadata.json` and `vms/<vm_id>/state.json`).

### Live VM cold-restart (v0.3.2)

Every successful spawn writes `vms/<vm_id>/state.json` (atomic tmp + rename) capturing the network identity, disk path, agent token, profile, and flock association. On daemon startup, after flock metadata is rescanned, each persisted VM is automatically brought back up:

1. **Orphan cleanup** — any leftover Firecracker process bound to the persisted API socket is sent SIGTERM, then SIGKILL after a 1.5 s grace. Stale socket / log FIFO / vsock UDS files are removed. (After a graceful shutdown this is a no-op because the previous daemon already stopped them; after a SIGKILL / crash it does the actual cleanup.)
2. **Network re-reservation** — the original TAP device is recreated with the same name and MAC, and the original IP is re-marked as in-use in the pool.
3. **Cold boot** — Firecracker is restarted against the same rootfs clone; `goose-agent` is waited for on `/health` up to 60 s.
4. **Flock association** — VM이 flock에 속해 있었다면 agent status를 `"ready"`로 되돌린다. daemon이 VM state persistence와 flock metadata persistence 사이에서 crash되어 flock metadata가 없거나 해당 agent가 빠져 있으면, recovery는 `state.json`의 `flock_id` / `agent_id`를 기준으로 member를 재연결하고 repaired flock metadata를 다시 persist한다. recovery가 실패하면 agent를 `"dead"`로 표시하고 Town Wall에 `<orchestrator>` notice를 남긴다.

The daemon-side shutdown path is designed to feed cold-restart:

- **Graceful shutdown (SIGTERM/SIGINT)** — `ControlPlane.DestroyAll` stops every Firecracker process via `StopVMM`, releases TAP/IP/vsock/socket, and **preserves each VM's rootfs ext4 and `state.json`**. The next daemon start cold-restarts them.
- **Explicit `DELETE /vms/{id}`** — routes through `destroyVM`, which does a full cleanup (deletes `state.json`, removes the rootfs ext4, releases all resources). The VM is gone and is not cold-restarted.
- **SIGKILL / crash** — defers don't run. `state.json` + rootfs survive on disk; on the next start, cold-restart picks them up exactly as for graceful shutdown.
- **COW spawn VMs** (`EPHEMERA_DISK_MODE=cow`) — `DestroyAll` releases the dm-snapshot kernel objects but **preserves the sparse exception store + `state.json`** (`TeardownDMSnapshotKeepStore`); the next start re-layers the store over the golden image and cold-restarts them (v0.4.0).
- **Snapshot-restored VMs** (`POST /snapshots/{id}/restore`, dm-snapshot path) — `DestroyAll` drops the dm device + transient exception store but **keeps `state.json`** (which carries `source_snapshot_id`, plus anvil `tenant_id` / `egress_policy`). The next start **re-restores** the VM from that source snapshot via `RecoverVMs` → `recoverRestoredVM` (back to snapshot-time memory+disk; the store is recreated fresh), v0.4.5. The legacy **bind-mount fallback** path (dm-snapshot tooling unavailable) still persists no `state.json` and is not auto-recovered.

**Memory auto-snapshot (v0.4.0, opt-in).** With `EPHEMERA_AUTOSNAPSHOT=true`, `DestroyAll` additionally snapshots each recoverable VM's live memory+state into `vms/<id>/auto/{memory.bin,state.bin}` *before* stopping it (graceful shutdown only — a SIGKILL cannot run it). On the next start, `RecoverVMs` **warm-restores** from that snapshot via `vm.RestoreMachine` (memory preserved, same `vm_id`/IP/TAP/MAC/token), so in-flight `/tasks` work survives a daemon bounce. Snapshot-restored VMs are excluded (they re-restore from their source snapshot). The snapshot is **one-shot** (deleted after the attempt, so a later bounce never rolls the VM back to a stale image) and **best-effort**: any failure — snapshot write, restore, or agent handshake — logs and falls back to the cold boot above. This is why `forwardSignals` omits `SIGTERM`/`SIGINT` (v0.4.0): the daemon owns graceful teardown, and a forwarded SIGTERM would kill Firecracker mid-snapshot.

What this preserves:

| Preserved | Lost |
|-----------|------|
| `vm_id`, `guest_ip`, `tap_device`, `mac_addr` | In-flight `/tasks` work (memory is not snapshotted) |
| `agent_token`, `agent_url` | Goose conversation context (in-VM, in-memory) |
| Disk contents (the rootfs clone, or COW spawn exception store, is reused) | Post-restore writes on a re-restored VM (dm-snapshot restores re-restore from source, v0.4.5; legacy bind-mount restores are not auto-recovered) |
| Flock membership, Town Wall history | (none) |
| Watchdog `status=dead` markings (v0.3.3 — persisted to `metadata.json`) | |

Callers that need at-most-once semantics across daemon restarts should idempotency-key their `/tasks` calls or poll for completion before retrying.

**Snapshot-restored VM recovery (v0.4.5):** dm-snapshot restored VMs **are** auto-recovered — they persist a `state.json` with `source_snapshot_id` (plus anvil `tenant_id` / `egress_policy`), and `RecoverVMs` re-restores them from that snapshot on the next start (the VM returns to its snapshot-time memory+disk; writes since the restore are not preserved, same as a manual re-restore). Still **out of scope**: the legacy bind-mount-fallback restore path (no `state.json`), and a restored VM whose **source snapshot was deleted** while it ran — recovery cannot re-restore it, so it is dropped and surfaced (not silently kept). Snapshot GC never deletes a source snapshot still referenced by a live or persisted restored VM.

> COW *spawn* VMs (`EPHEMERA_DISK_MODE=cow`) **are** auto-recovered as of v0.4.0: `DestroyAll` preserves the exception store and `RecoverVMs` re-layers it over the golden image. Orphan dm-snapshot devices left by a crashed run (no surviving `state.json`) are reclaimed on the next start via `RemoveOrphanCOWDevices`.

### Watchdog dead-status persistence (v0.3.3)

The watchdog's `dead` marking is now durable. `Watchdog.onFailure` calls `Flock.Persist(workDir)` immediately after flipping `AgentInfo.Status` to `"dead"`, so the change lands in `flocks/<id>/metadata.json` before the next probe cycle. `Flock.Persist` holds a per-flock `writeMu` around `ToMetadata` + `SaveFlockMetadata`'s `tmp + rename`, so concurrent writers (`createFlock`, `watchdog.onFailure`, `recovery.markFlockAgentDead`, the new per-agent restart) never tear each other's writes.

Recovery's status transitions are persisted on the same path: successful cold-restart flips status back to `ready` and persists, while a VM that cannot be cold-restarted is marked `dead` (with an `<orchestrator>` Town Wall notice) and persisted. The net effect is that the on-disk metadata always reflects the freshest known liveness, and a daemon restart can never silently revive an agent that was already dead.

> **Operational note**: a recovered `dead` agent stays dead — even if the watchdog later sees the VM respond, the dead marking is intentionally not auto-cleared. Use `POST /flocks/{id}/agents/{agent_id}/restart` (below) to replace the VM and reset the status to `ready`.

### Per-agent restart (v0.3.3)

`POST /flocks/{flock_id}/agents/{agent_id}/restart` is the surgical alternative to recreating a whole flock when one member dies. The handler:

1. Looks up the agent in the flock (404 if either is missing).
2. Captures the existing `agent_token` from `cp.vms` before teardown.
3. Tears down the dead VM via `destroyVM` and calls `Watchdog.ForgetVM(oldVMID)` to drop cached failure state.
4. Calls `spawnVMInternal` with the same role/profile/flockID/agentID **plus the captured `AgentToken`**; an empty token would trigger fresh generation, but here we reuse so callers' cached tokens keep working.
5. Calls `Flock.UpdateAgentVM(agentID, newVMID, newAgentURL)`, which swaps the VM identity in place and resets `Status` to `ready`.
6. Persists the updated metadata.

On spawn failure the agent is left in `Status=dead` (and persisted) so external callers see the truth — they can retry restart or `DELETE` the flock entirely.

```bash
curl -X POST "$API/flocks/$FLOCK_ID/agents/reviewer-1/restart"
# {"vm_id":"vm-1715...","guest_ip":"10.0.1.7","agent_url":"http://10.0.1.7:8080","profile":"reviewer"}
```

### Auto-injected control-plane token (v0.3.3, narrowed by ADR-0003)

When the control plane runs with `EPHEMERA_API_TOKENS` set, the in-VM `/townwall/post` forwarder needs a Bearer to authenticate against `/flocks/{id}/post`. v0.3.3 plumbs that token automatically. Since [ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md) the value it plumbs is a **per-flock guest capability token**, not the operator bearer:

- `cp.ensureLocalFlockGuestToken` mints a 32-byte random token for a local flock, registers it with `cp.setRelayToken` (the same admission store routed flocks use) and persists it at `flocks/<flock_id>/guest-token`, mode **0600**, written atomically. Minting is skipped entirely when auth is disabled, so the injected value is `""` exactly as before.
- `spawnVMForFlock` (plus `restartAgent` and `changeFlockAgentRole`) pass that token through `spawnVMOptions.ControlPlaneToken` → `VMPrepareOptions.ControlPlaneToken` → `injectVMFiles`, which writes it to `/root/.ephemera-cp-token` at mode 0600. All three set `spawnVMOptions.ControlPlaneTokenManaged` to **false**: a capability token is not the daemon's bearer, so SIGHUP must not overwrite it.
- Admission scope: `authMiddleware` extracts the flock id from the request path and compares against **that flock's** token, so the guest can reach only `/flocks/<its own id>/(post|wall|wall/history|call)`. Another flock's identical paths and every control-plane route (`/vms`, `/config/*`, `/tenants`, `/snapshots`) return `401`.
- Lifetime: there is **no expiry and no individual rotation**. Deleting the flock revokes admission and removes the token file — that is the only revocation path. See ADR-0003's residual-risk contract.
- Restart: `cp.relayTokens` is in-memory only, so the daemon re-reads each recovered local flock's `guest-token` immediately after `LoadFromDisk` and re-registers it. A flock recovered from before ADR-0003 has no such file and mints one on its next member spawn.
- `ControlPlane.controlPlaneTokenForVM()` still returns the first **non-expired** client's token under `clientsMu`, but is no longer wired into any spawn path.
- A standalone `POST /vms` with no `control_plane_token` in the body injects **nothing**: `injectVMFiles` skips the file entirely for an empty token, so a plain workload VM has no control-plane bearer on disk at all.
- A `POST /vms` that *does* carry `control_plane_token` (how a routed flock member is spawned — the value is that flock's scoped relay token) injects the caller's token verbatim and leaves it unmanaged: it is the caller's credential, and the home daemon expects to keep seeing exactly it.
- `goose-agent`'s `loadCPToken` prefers the file and falls back to the legacy `EPHEMERA_CONTROL_PLANE_TOKEN` env var for older golden images. Guest-side code is unchanged by ADR-0003 — it forwards whatever the file holds and never inspects it.

This removes the per-VM operator burden documented in earlier releases. v0.3.4 adds true hot rotation on top — see [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034) below, which after ADR-0003 applies only to VMs spawned before it.

### CP token rotation via vsock (v0.3.4)

When you want to rotate the control-plane bearer without restarting either the daemon or any VMs:

1. Run the daemon with `EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens` (one `name:token[:expires]` entry per line — comma-separated also works; the optional `:expires` is the v0.4.1 per-token TTL). The file source takes precedence over `EPHEMERA_API_TOKENS` env when set; both legacy env paths remain as fallback.
2. Edit the file (operator action).
3. `pkill -HUP ephemera-daemon`. `ReloadClients` re-reads the file (env values are fixed at exec, the file is not), hot-swaps `cp.clients` under `clientsMu`, and fans the first **non-expired** client's token (v0.4.1; was `apiClients[0]`) out over the existing vsock channel to the eligible VMs.

**Eligibility** — `propagateCPTokenToVMs` targets a running VM only when both hold:

| Condition | Why |
|-----------|-----|
| The VM has a live vsock UDS path | There is no other channel to reach the guest. |
| `runningVM.cpTokenManaged` is true | The daemon itself injected its own operator bearer into that guest at spawn. |

Concretely:

| VM kind | Where its CP token came from | On SIGHUP |
|---------|------------------------------|-----------|
| Plain `POST /vms` (no `control_plane_token`) | Nothing — no file is written | **Skipped** |
| Local flock member spawned **after** [ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md) | That flock's **guest capability token** | **Skipped** |
| Local flock member spawned **before** ADR-0003 | `controlPlaneTokenForVM()` — the daemon's own operator bearer | **Rotated** |
| Routed flock member (`POST /vms` with `control_plane_token`) | Caller-supplied per-flock **scoped relay token** | **Skipped** |

The rule is recorded explicitly at spawn time rather than inferred from `flock_id` or flock kind, because a flock member also carries a `flock_id` yet must keep its narrower token: the daemon rotates only the credential it owns. The bit is persisted in `state.json` as `cp_token_managed`, which is what keeps pre-ADR-0003 VMs rotating across a daemon restart — that is the whole upgrade path, and it needs no migration or version gate. Since no current spawn path sets the bit, the rotation set only shrinks: it empties as those VMs are replaced. A `state.json` written before the field existed decodes as unmanaged; use the `POST /flocks/{id}/agents/{agent_id}/restart` fallback below, which respawns the member under the current (capability-token) model.

In-VM side, `goose-agent`'s vsock listener now dispatches both `CHANGE_IP` (used since v0.2.0 for snapshot-restore IP plumbing) and the new `SET_CP_TOKEN <token>` command, which atomically rewrites `/root/.ephemera-cp-token` (tmp + rename, mode 0600). The `/townwall/post` handler re-reads the file on every request, so the next forwarder call sees the new bearer.

The fan-out is **best-effort**: each VM gets ~4 s (20 attempts × 200 ms, matching the existing `ReconfigureGuestIP` budget) and any per-VM failure is logged but never propagated. The SIGHUP path therefore completes in bounded time regardless of unresponsive VMs. A final log line summarizes results:

```
SIGHUP: token reload complete — 1 client(s): alice
SIGHUP: CP token propagated to 3/3 VM(s)
```

**SDK signal forwarding** — `firecracker-go-sdk` v1.0.0 defaults to forwarding `SIGINT/SIGQUIT/SIGTERM/SIGHUP/SIGABRT` from the daemon to every Firecracker child (see `internal/vm/machine.go`'s `setupSignals` reference). The daemon explicitly narrows `firecracker.Config.ForwardSignals` to `SIGQUIT` and `SIGABRT` only. `SIGHUP` is owned by the token-reload + vsock fan-out flow described here; forwarding it would kill every running Firecracker and the fan-out would immediately get `connection refused`. `SIGINT` and `SIGTERM` are also daemon-owned: `Ctrl-C` / `systemctl stop` enter the daemon's graceful teardown and auto-snapshot path, which then stops each child explicitly. `SIGQUIT` and `SIGABRT` stay forwarded because the daemon does not trap those abnormal exits, so forwarding them reduces orphaned Firecracker children.

**Caveat**: only VMs spawned by a v0.3.4 (or newer) daemon implement the `SET_CP_TOKEN` handler. VMs whose `goose-agent` was baked from an older golden image will log a per-VM "unknown command" failure during fan-out; for those, the v0.3.3 fallback (`POST /flocks/{id}/agents/{agent_id}/restart`) is still the rotation path.

### Health watchdog

A background goroutine polls every flock-member VM's `/health` endpoint every 5 seconds (1 s HTTP timeout). After 3 consecutive failures the agent's status transitions to `"dead"` and a notice is auto-posted to the Town Wall:

```
[2026-05-15T14:33:12Z] <orchestrator> worker-1 unresponsive after 3 health probes - marked dead
```

Subscribers on the SSE stream see this in real time. The dead agent is **not** auto-revived even if it transiently recovers — operators decide when to reset by deleting the flock or the individual VM. Standalone (non-flock) VMs are not watched.

**Env-tunable since v0.3.4.** All three thresholds are overridable at startup:

| Variable | Default | Purpose |
|----------|---------|---------|
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` | `5` | Poll cadence |
| `EPHEMERA_WATCHDOG_TIMEOUT_SEC` | `1` | Per-probe HTTP timeout (clamped: `interval ≥ timeout`) |
| `EPHEMERA_WATCHDOG_THRESHOLD` | `3` | Consecutive fails before marking dead |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `false` | When `true`, a dead agent that resumes responding is auto-marked `ready` and a recovery notice (`"<id> recovered - auto-healed to ready"`) is posted to the Town Wall. Default off preserves the sticky-dead contract. |

Tunables apply once at daemon startup and land via `Watchdog.Configure` before `Start`. The startup log line confirms the resolved values:

```
Watchdog started (interval=5s, timeout=1s, threshold=3, auto_heal=false)
```

### Flock state persistence

`POST /flocks` writes `flocks/<flock-id>/metadata.json` atomically (tmp + rename) before returning the response. On daemon startup the file is rescanned and every flock is re-registered in memory. The active Town Wall log is reopened in append mode; when v0.4.3 size rotation is enabled, rotated backups remain on disk but `/wall/history` reads the active `TOWN_WALL.log` only. `seq` numbering continues monotonically within the active log.

> **Recovery scope (v0.3.2)**: flock metadata is restored here; the live VMs are brought back via the cold-restart path described above. After daemon restart, recovered flocks are fully interactive (`/tasks`, `/stop`, `/post`, `/wall`, `DELETE` all work), with the caveat that in-VM memory state is lost — agents resume from a fresh boot, not from where they left off.

### Monotonic message sequence numbers

Each Town Wall `Message` carries a `seq` field starting at 1 per flock. A subscriber that reconnects after a network blip can compare its last received `seq` against the newest message it sees and detect any gap; missing active-log entries can be fetched from `/flocks/{id}/wall/history` and filtered client-side by `seq`. After size rotation, `/wall/history` does not scan rotated backups.

```bash
LAST_SEQ=42
curl -N "$API/flocks/$FLOCK_ID/wall" | while read -r line; do
    case "$line" in
        data:*)
            msg="${line#data: }"
            seq=$(echo "$msg" | jq -r .seq)
            if [ "$seq" -gt "$((LAST_SEQ + 1))" ]; then
                # gap — recover from history
                curl -s "$API/flocks/$FLOCK_ID/wall/history" | \
                    jq --argjson last "$LAST_SEQ" --argjson seen "$seq" \
                        '.[] | select(.seq > $last and .seq < $seen)'
            fi
            LAST_SEQ=$seq
            ;;
    esac
done
```

`seq` is reassigned 1..N from the on-disk log each time `History` is read, so it is stable across daemon restarts (the file format itself does not store seq — it is the canonical assignment from line order).

---

## Observability (v0.3.5)

### Prometheus metrics

The control plane exposes a counter / gauge / histogram catalogue at `GET /metrics`. Defaults follow the standard scrape model (unauthenticated, text format 0.0.4); see the [Metrics endpoint](api-reference.md#metrics-v035) under API Reference for the full catalogue and `EPHEMERA_METRICS_REQUIRE_AUTH` to gate it behind Bearer auth. The exposition formatter is self-implemented (`internal/metrics/`) — the project keeps its zero-runtime-dependency policy.

### Structured logging (`log/slog`)

Every daemon-side log call (control plane, recovery, watchdog, network, storage) was migrated from `log.Printf` to `log/slog`. Two env knobs control output:

- `EPHEMERA_LOG_FORMAT=text` (default) — `key=value` lines from slog's TextHandler.
- `EPHEMERA_LOG_FORMAT=json` — slog's JSONHandler, suitable for log-aggregation pipelines.
- `EPHEMERA_LOG_LEVEL=debug|info|warn|error` (default `warn`) — minimum level emitted.

Context fields are attached as structured pairs (`vm_id`, `flock_id`, `agent_id`, `err`, …) rather than embedded in the message string. The in-VM `goose-agent` keeps its existing `log.Printf` output unchanged this cycle to avoid touching the golden-image bake budget; revisit in v0.4.3.

### Per-VM stats endpoint

`GET /vms/{vm_id}/stats` returns a JSON snapshot of cpu/mem/network/uptime/agent_busy (see [Per-VM Stats](api-reference.md#per-vm-stats-v035) under API Reference). The endpoint is a point-in-time snapshot — repeated polling is the intended scrape pattern; streaming is on the v0.4.3 roadmap.

### Try the demo (`observability_demo.sh`)

`sudo bash observability_demo.sh` spins up the daemon, downloads + launches Prometheus and Grafana (cached under `artifacts/`), then runs an automatic workload that exercises every metric family (VM spawn/destroy, snapshot create, flock spawn, SIGHUP reload). After ~2 minutes a banner prints the URLs:

| Service | URL | Notes |
|---------|-----|-------|
| Daemon API + `/metrics` | http://localhost:3000 | Bearer `demo-token-v035` for API calls; `/metrics` is unauthenticated |
| Prometheus | http://localhost:9090 | 5-second scrape interval (demo-only) |
| Grafana | http://localhost:3001 | `admin` / `admin`, dashboard "Ephemera Overview" pre-provisioned |

The daemon, Prometheus, and Grafana remain running until you press `Ctrl-C`; the trap then shuts down all three and removes the per-run TSDB / data dir under `/tmp/observability-demo-*`. Targets Prometheus 2.51.x and Grafana 10.4.x (versions + SHA256 are pinned in the script).

---

## Known Limitations

| Limitation | Detail |
|------------|--------|
| **Single-host VM runtime** | VM 실행 자체는 host-local daemon이 소유한다. Cross-host snapshot replication은 MCP router와 scheduler state를 통해 지원한다 — 수동 경로(`anvil_replicate_snapshot`)에 더해 선언적 자동 복제 sweep(adapter reconcile, replica factor N=2, best-effort eventual). |
| **Same-snapshot concurrent restores not supported** | The guest IP is reconfigured via vsock after restore, so different-snapshot concurrent restores each get a fresh IP. However, two VMs from the *same* snapshot would still collide on the Firecracker vsock UDS path (which is fixed in `state.bin`), so same-snapshot concurrent restores are not supported. |
| **Cross-machine restore** | `anvil_replicate_snapshot`으로 target host에 snapshot bundle을 import한 뒤 `POST /snapshots/{id}/restore`를 호출한다. diff snapshot은 target에 base full snapshot이 필요하며 `include_dependencies=true`가 base를 먼저 복제한다. |
| **Cold-restart loses in-VM memory by default** (v0.3.2; mitigated v0.4.0) | Live VM auto-restart re-boots each VM from its rootfs clone — the guest kernel and `goose-agent` start fresh, and any `/tasks` request in flight at the moment of daemon shutdown is dropped. Set `EPHEMERA_AUTOSNAPSHOT=true` to warm-restore in-VM memory across a *graceful* shutdown (v0.4.0). A SIGKILL/crash still cold-boots, so callers should still idempotency-key tasks or re-poll for completion across an ungraceful restart. |
| **CP token hot-rotation requires `_TOKENS_FILE`** (v0.3.4; scope narrowed by ADR-0003) | On SIGHUP the daemon hot-propagates the new control-plane token (the first non-expired client since v0.4.1; `apiClients[0]` before) to the running VMs it injected that token into (see [eligibility](#cp-token-rotation-via-vsock-v034)) via the `SET_CP_TOKEN` vsock command. This requires sourcing tokens from `EPHEMERA_API_TOKENS_FILE` — env-supplied tokens are fixed at exec and cannot change on SIGHUP. (The in-VM `SET_CP_TOKEN` handler ships in every current golden image, which auto-rebakes on any `goose-agent` change.) When tokens come from the env, the `POST /flocks/{id}/agents/{agent_id}/restart` fallback re-injects the current token. **Since [ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md) this limitation applies only to VMs spawned before it**: flock members now hold a per-flock capability token that operator-token rotation neither touches nor invalidates, so the eligible set drains as those VMs are replaced. The machinery is retained until it does. |
| **A flock guest capability token does not expire** (ADR-0003) | The credential injected into a flock member admits only that flock's `post`/`wall`/`wall/history`/`call`, but it has no TTL and no individual rotation path, and flocks have no reaper or GC — so its lifetime equals the flock's. **Deleting the flock is the only revocation** (it drops admission and removes `flocks/<id>/guest-token` together). This is a deliberate trade recorded in ADR-0003's residual-risk contract: an expiring broad credential was exchanged for a non-expiring narrow one, justified by the blast-radius reduction. Introducing a flock TTL/reaper would close it. |
| **Metrics retention is external** (v0.3.5) | `/metrics` exposes raw counters and gauges only — the daemon does not aggregate, store, or rotate history. Operators are expected to wire an external Prometheus (or any text-exposition-compatible) scraper. |
| **Web UI conversation is in-memory** (v0.5.0) | The conversation panel holds its transcript in the browser tab; a page reload starts a fresh `session`, so prior turns are no longer shown (the underlying goose session persists in the VM but is not re-loaded into the UI). Snapshot-restored / cold-recovered VMs may also show an empty model, since `provider`/`model` is recorded only at spawn time. |
| **COW diff-restore guest panic under heavy ZFS load** (D4 — concluded, upstream-tracked; opt-in COW only) | With `EPHEMERA_DISK_MODE=cow` (opt-in; the default disk mode is **plain**), restoring a **diff snapshot** onto a COW-spawned VM can panic the guest kernel — a general-protection fault in `inet_bind2_bucket_find` shortly after resume — but only on ZFS hosts under concurrent full-gate load; ext4 hosts and full-snapshot restores pass. Four rounds of investigation traced the root cause to a KVM/Firecracker **resume-race outside anvil**: every anvil-side storage/restore lever (fsync, global sync, direct-I/O, merged-artifact path, unlink audit, memory-file immutability) came back negative, and both hosts reproduced it with a byte-identical daemon, so it is a general defect rather than host-specific hardware. Firecracker **v1.16.1** is the strongest mitigation, cutting the failure rate from ~100% to ~15–25% (its v1.16.0 vsock RX-race fix #5882 is the main contributor) but does not eliminate it; a pre-resume quiescence delay only lowers the probability (no fixed value cleared n≥2 on both hosts). **Status: concluded on 2026-07-13 as an upstream-tracked known limitation** — the default disk mode stays **plain**, COW remains opt-in, and the flip is deferred indefinitely (reopened only if upstream resolves the resume-race). Operators using opt-in COW on ZFS under heavy load should expect ~15–25% diff-restore failures and run Firecracker ≥ v1.16.1. Detail: `docs/operations/2026-07-11-cow-burnin-run.md` (rounds 1–4) and the upstream report `docs/operations/2026-07-13-d4-firecracker-upstream-report.md`. |
| **Egress SNI filter sees only the outer (ECH) SNI** (ADR-0002, PR #73) | anvil's transparent SNI filter observes only the ClientHello's **outer/cover SNI** — it skips the ECH `encrypted_client_hello` extension rather than decrypting it. If the outer name is missing or an unlisted decoy, the flow fail-closed DROPs. But if the outer name **is** on `allow_sni`, the flow is allowed and the encrypted **inner** destination stays hidden — the same trust level as a guest-asserted SNI (an allowlisted outer can tunnel an arbitrary inner destination). anvil does not defeat ECH (the inner name is not decryptable without the server's key); the only mitigation is a **CIDR pin** on the endpoint (an outer-SNI allowlist is not itself a trust anchor). As of 2026-07-18 this residual case is at least observable: `ephemera_egress_sni_ech_observed_total{proto}` increments (plus a content-free `slog.Info`) whenever an allowed flow's ClientHello carries ECH — observe-only, the allow/deny verdict is unchanged. Detail: the "잔여 위험 계약" table in [`docs/adr/0002-egress-sni-transparent-filter.md`](../adr/0002-egress-sni-transparent-filter.md). |
| **Restored guests reseed through VMGenID, which requires Firecracker ≥ v1.16.1** (2026-08-06) | A snapshot captures the guest CSPRNG state, so every restore of the same snapshot would resume from identical entropy if nothing reseeded it. Firecracker creates a **VMGenID** device unconditionally on x86_64 — there is no API or SDK setting for it, so its absence from anvil's machine config is expected, not a gap — and mints a fresh 128-bit ID on every restore; the guest kernel (6.1.155 carries `vmgenid`, `add_vmfork_randomness`, `crng_reseed`) reseeds off the resulting interrupt. Two residuals. First, upstream documents a **race window between vCPU resume and the reseed**: anvil's restore path runs a host-side vsock round trip (guest IP reconfigure, then agent token) before any guest-originated TLS is reachable, and the agent and control-plane tokens are generated with host entropy and injected rather than drawn from the guest, so the window is narrow here — but it is not closed by anvil. Second, the guarantee **only holds from Firecracker v1.16.1**: v1.15.1 advertises the device as `FCVMGID` while the kernel matches `VMGENCTR` (upstream fixed this in v1.16.0), so a host still running v1.15.1 may get no reseed at all. Until 2026-08-06 such a host could persist silently, because `EnsureFirecracker` returned as soon as the binary existed and never compared it against the pin; it now verifies what is already installed (see `docs/guides/runtime-usage.md`, "Pinned artifacts verified on every start"). The same version floor is already recommended by the D4 row above, for an unrelated reason. |
