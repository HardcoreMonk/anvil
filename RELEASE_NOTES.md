# anvil-v0.4.0 — Ephemera core service parity (upstream v0.4.0–v0.7.0)

- Tag: `anvil-v0.4.0`
- GitHub Release:
  <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.4.0> (게시 예정)
- Published: orchestrator 태그·게시 후 확정
- Target commit: 이 `docs: release anvil-v0.4.0` 커밋 (SHA는 orchestrator가 release
  handoff에 확정)

`anvil-v0.4.0`은 `anvil-v0.3.2` 이후 첫 공개 integration release이며, upstream ephemera
core service를 `v0.4.0`부터 `v0.7.0`까지 anvil main runtime/operator baseline으로
편입한다. anvil은 token redaction, tenant/egress, scheduler, audit, IronClaw MCP surface
separation 적응을 유지한 채 upstream `v0.7.0`을 지원한다 — IronClaw `anvil_*` MCP 경계는
그대로이고(`cmd/anvil-mcp` 불변), runtime MCP Gateway(`EPHEMERA_MCP_*`)와 operator Web
UI/`/config/*`/end-user installer는 runtime/operator surface로만 채택돼 `cmd/anvil-mcp`를
대체하지 않는다. parity gate 중 발견된 3개 pre-existing latent defect를 고쳤다:
restore-over-`meta.DiskPath`(`4c1c803`), golden image mount skip(`38fbedc`), proxy
keep-alive stale-connection(`64ec57c`). 검증(parity gate): full KVM e2e `334✓ / 0✗`
("All test steps passed", step 59 real-LLM smoke만 provider key 부재로 skip), MCP
`anvil-mcp-e2e` lifecycle+flock PASS, script-only workload E2E PASS, release build
(SLIM/FULL + `.sha256`) PASS. main 병합 후 e2e 재실행 결과는 orchestrator가 태그 전
확인한다. 전 태그 upstream feature 분류는
[`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)의
52-feature parity matrix(14 adopted / 30 adapted / 4 deferred / 4 excluded)에 있다.
남은 open gate는 valid provider key `semantic` run(e2e step 59)뿐이다.

아래 하위 절은 `anvil-v0.3.2` 이후 anvil-side scheduler/routed-flock 작업, upstream
`v0.4.0`-`v0.7.0` phase 편입 기록, release-gate follow-up batch, snapshot 참고를 순서대로
담는다.

> 참고: 2026-07-06에 학습·참고 전용 pre-release 스냅샷 4종이 게시됐다 —
> `anvil-v0.4.5-snapshot`(`8daf6f3`), `anvil-v0.5.5-snapshot`(`7f207a0`),
> `anvil-v0.6.4-snapshot`(`04e2a12`), `anvil-v0.7.0-snapshot`(`7b3f009`).
> 각 upstream 시리즈의 adapted baseline 시점 보존용이며 운영 배포용이 아니다.
> 각 시점의 알려진 결함(v0.4.5: `4c1c803`·`38fbedc`에서 수정, v0.5.5: `64ec57c`에서
> 수정)과 이후 hardening 내역이 각 릴리즈 노트에 명시돼 있다. 학습 주석 브랜치:
> `annotate/v0.4.5` ~ `annotate/v0.7.0`.


## 추가됨

- scheduler service에 `POST /schedule/flock` dry-run endpoint를 추가한다. 이 endpoint는
  flock roles를 host별 agent placement plan으로 계산하지만 VM을 생성하지 않는다.
- cross-host planner는 tenant quota를 roles 수 기준으로 한 번 검증하고, host capacity는
  agent slot별 reservation으로 초과하지 않게 계산한다.
- `scripts/anvil-scheduler-smoke.sh`가 `POST /schedule/flock` dry-run response의
  `agents`, `requested`, `host_status_summary` 계약을 확인한다.
- scheduler `/metrics`에 `anvil_scheduler_flock_placement_*` aggregate metrics를
  추가한다.
  - `anvil_scheduler_flock_placement_attempts_total`
  - `anvil_scheduler_flock_placement_latency_seconds`
  - `anvil_scheduler_flock_placement_last_success_timestamp_seconds`
  - `anvil_scheduler_flock_placement_last_failure_timestamp_seconds`
- `RuntimeRouter.CreateFlock` scheduler-aware path는 schedule, daemon create,
  placement save, total latency를 bounded phase histogram으로 기록한다.
- experimental MCP `anvil_create_routed_flock_members`를 추가한다.
  `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent scheduler state가 있을
  때 `POST /schedule/flock` plan을 사용해 role VM을 host daemon `POST /vms`로
  cross-host 생성하고 `cross_host_members_only` output을 반환한다.

## 보안/운영 강화

- flock placement metrics label은 `outcome`, `reason`, `phase` bounded enum만
  사용한다.
- `tenant_id`, `flock_id`, `vm_id`, host endpoint, authorization header,
  `agent_token`, daemon raw body는 metrics state와 exposition에 저장하지 않는다.
- routed members-only flock delete는 downstream registry의 member VM placement를 따라
  host별 daemon delete를 호출하고, 일부 cleanup 실패 시 성공한 VM placement만 제거한
  뒤 `failed_cleanup_pending` 상태를 남겨 수동 확인이 가능하게 한다.
- upstream ephemera `v0.7.0`의 운영 hardening 중 전체 baseline sync와 독립적인
  항목을 선별 backport한다.
  - kernel download는 Firecracker binary와 같이 pinned SHA256을 검증한 뒤
    `artifacts/vmlinux.bin`에 publish한다. mismatch나 write 실패 시 partial kernel
    file을 남기지 않는다.
  - `waitForAgent`는 per-probe HTTP timeout을 사용해 guest가 TCP connection을
    열고도 `/health` 응답을 돌려주지 않는 경우 전체 readiness loop가 묶이지 않게
    한다.
  - `EPHEMERA_HOME`으로 daemon work directory를 명시할 수 있다. unset이면 기존처럼
    process current working directory를 사용한다.
- recovery는 VM `state.json`의 `flock_id` / `agent_id`를 flock membership의
  fallback source로 사용한다. daemon crash가 VM state persistence와 flock metadata
  persistence 사이에서 발생해도, 다음 startup에서 missing flock 또는 missing agent를
  재연결하고 repaired metadata를 다시 persist한다.
- `webdev_demo.sh`는 `POST /flocks` 응답의 `agent_tokens`를 읽지 않고, orchestrator
  `vm_id`를 사용해 control-plane proxy `POST /vms/{vm_id}/tasks`로 brief를 보낸다.
- anvil main runtime baseline은 upstream ephemera `v0.7.0` adapted runtime·operator
  support를 포함한다(수정 없는 `v0.7.0`가 아니다). 2026-07-02 기준 upstream latest
  observed는 `v0.7.0`이며 anvil은 관찰 범위 전체를 병합했다 — upstream parity
  scope(`v0.4.0`-`v0.7.0`) 코드 편입이 완료돼 pending sync 후보는 없다. anvil
  runtime/operator baseline supports upstream ephemera v0.7.0 with anvil adaptations for
  token redaction, tenant/egress, scheduler, audit, and IronClaw MCP surface separation.
  전 태그별 adopted/adapted/deferred/excluded parity matrix는
  `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`에 있다.
- 2026-07-06 release-gate follow-up batch (anvil 적응 — upstream 변경 아님; commits
  `4a802f5`, `0376afa`, `613a01b`, `cd2e70b`, `de5a7aa`, `0625df5`, 상태 close
  `ccc5127`):
  - **behavior change**: stdio backend 자식 stderr를 VM-facing gateway error에서 scrub하고
    host slog에만 남긴다(`4a802f5`). backend 오동작 시 backend credential이 VM에 도달할 수
    있던 유일한 신규 경로를 차단한다.
  - **behavior change**: `credential_env`가 예약 이름 `PATH`/`HOME`/`LANG`을 쓰면 config
    validation error로 거부한다(`0376afa`).
  - **behavior change**: snapshot GC가 persisted restored-VM state를 읽지 못하면
    fail-closed한다(`613a01b`) — plan이 `Errors` entry로 abort하고 아무것도 삭제하지 않는다.
  - **behavior change**: `PUT /config/profiles/default`가 `goose.yaml` 부재 시 `404`를
    반환한다(`cd2e70b`, 이전 `500`).
  - new guard: MCP audit writer JSONL key-set sentinel(`de5a7aa`,
    `mcp_audit_writer_anvil_test.go`); KVM-free flock broadcast unit test(`de5a7aa`,
    `orchestrator_broadcast_test.go`); auth sentinel을 `/config/clients`·`/config/providers`·
    profile DELETE·`/system` verbs로 확장(real production mux, `de5a7aa`).
  - e2e/README step-label reconciliation(`0625df5`); keep-alive(`64ec57c`) upstream 제안은
    DECLINED. batch 검증: 13 pkgs ok, review Approved(0 Crit/Imp), full e2e `334✓/0✗` at
    `ccc5127`, PR #17 push.
- upstream `v0.7.0` (end-user installer + conversation transcript restore + upstream
  hardening reconcile)를 sync 한다. anvil adaptation:
  - end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/`ephemera.service.in`)와
    release workflow(`scripts/build_release.sh`)는 **runtime/operator installer
    surface**로 채택한다. systemd service는 canonical `ephemera` 이름을 유지한다
    (rule-permitted, anvil alias wrapper 없음). `uninstall.sh`는 ephemera-scoped `/tmp`
    scratch(`/tmp/goose-workspaces` 등; stale no-op `/tmp/goose-rootfs` 포함)를
    root-gated·prefix-anchored로 정리한다. 외부 Web UI 노출은 reverse proxy/TLS 또는
    private network 뒤에서만 한다.
  - conversation transcript restore는 daemon proxy `GET /vms/{id}/sessions/{name}/
    transcript`(bearer)로 노출한다. agent export는 read-only `goose session export`
    (model call 없음)이고 응답 schema `{turns:[{role,text}]}`는 auth-free여서 Web UI가
    daemon token 없이 렌더한다. 4개 transcript-safety guard(TDD): endpoint는 bearer
    없으면 `401`, payload는 provider key/CP token/`agent_token` sentinel-free, cache-hit는
    agent spawn 없이 serve, export argv는 `session export -n {name} --format json`이며
    run-token 거부.
  - release build integrity: `build_release.sh`가 다운로드한 kernel/firecracker를
    `main.go`에서 parse한 pin과 `sha256sum -c`로 검증해, runtime `EnsureKernel`이 기존
    파일을 `os.Stat`로 skip하던 FULL-tarball supply-chain gap을 닫는다.
  - upstream hardening reconcile: 사전 독립 backport 3종(kernel SHA atomic temp+rename
    무조건 검증, `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe timeout
    deadline cap)이 upstream `v0.7.0` 버전을 이기고 single definition으로 남았다(anvil이
    stricter, net Go diff는 doc-comment-only). 기존 anvil adaptation(agent-stamp mount
    skip, restore-over-`meta.DiskPath`, proxy `DisableKeepAlives`)은 하나도 rollback되지
    않았다.
- upstream `v0.6.0` (runtime MCP Gateway)를 sync 한다. `EPHEMERA_MCP_ENABLED`로 켜는
  host-resident MCP Gateway(`internal/mcpgateway`)가 backend MCP server(tools/
  resources/prompts)를 VM 내부 goose client에 중개한다. **이 gateway는 runtime/operator
  surface이고 IronClaw MCP surface가 아니며, `cmd/anvil-mcp` IronClaw adapter를
  대체하지 않는다**(`EPHEMERA_MCP_*` gateway ≠ `ANVIL_MCP_*` adapter). anvil은 경계를
  구조적으로 강제한다:
  - caller profile은 source IP를 VM registry와 대조해 server-side로 판정한다. registry에
    없는 caller는 `403`이며 guest가 identity를 주입하지 못한다.
  - backend credential은 host-side(`configs/mcp/secrets.yaml`, gitignored)에만 있고 VM에는
    gateway URL만 주입된다(`VMPrepareOptions`에 credential 필드 없음).
  - `audit/mcp.jsonl`은 고정 key set의 metadata만 기록하고 tool argument/result와 `Err`
    문자열까지 제외한다(sentinel test).
  - profile policy는 `servers.yaml`을 좁히기만 하고 넓힐 수 없다(intersection).
  - anvil boundary guard 4종: IronClaw schema/adapter가 gateway tool 제외
    (`TestToolRegistrationsExcludeGatewayTools`,
    `TestCurrentIronClawSchemasExcludeGatewayNamespacedTools`), audit metadata-only
    sentinel, `/config/mcp*` bearer 없으면 `401`, VM은 URL(credential 아님)만 받고
    policy는 widen 불가.
- upstream `v0.6.1`/`v0.6.2`/`v0.6.4`(upstream에 `v0.6.3` 없음) MCP Gateway hardening을
  sync 한다:
  - `v0.6.1`: `EPHEMERA_NET_ANTISPOOF`(기본 on, ebtables best-effort)로 guest IP 위조를
    막고, per-(VM, backend server) token-bucket rate limit(`EPHEMERA_MCP_RATE` 기본 `0`
    =unlimited, `EPHEMERA_MCP_BURST`)을 적용한다.
  - `v0.6.2`: backend resources/prompts를 aggregate하고 per-tool·per-profile policy를
    적용한다. resources/prompts는 tools와 같은 policy와 rate bucket을 공유한다(anvil
    guard). audit에 `kind` field 추가.
  - `v0.6.4`: stdio backend를 subprocess로 실행한다. child env는 `[PATH,HOME,LANG]`+
    backend `spec.Env`로 새로 구성해 daemon `EPHEMERA_*`가 새지 않고(canary test),
    credential은 `credential_env`로만 주입(argv 아님), daemon이 root면 `nobody`로
    privilege drop + `/var/lib/ephemera/mcp-stdio` scratch를 cwd·HOME으로 쓰며 shutdown이
    process group을 reap한다(pgid recycling-safe). `GET /config/mcp/servers`는
    transport/command와 `has_credential`만 노출한다(leak guard). `EPHEMERA_MCP_BIND_IP`로
    기본 bridge IP bind를 override할 수 있고 source-IP `403`이 defense-in-depth로 남는다.
- upstream `v0.5.0` (operator Web UI + `/config/profiles` + multi-turn agent
  `session` + graceful VM delete)를 sync 한다. anvil adaptation은 runtime/operator
  surface를 IronClaw MCP surface와 분리해서 유지한다:
  - embedded Web UI는 `/ui/`(정적 bundle + login page)만 auth/audit chain 밖에
    두고, `/config/profiles`를 포함한 모든 data API는 auth 설정 시 bearer 뒤에
    유지한다(guard `TestConfigProfilesRequireAuthWhenConfigured`). UI bundle은
    token/secret을 담지 않는다.
  - profile config handler는 `goose.yaml`의 `GOOSE_PROVIDER`/`GOOSE_MODEL`만
    read/write하며 `goose-secrets.yaml`은 절대 read/write하지 않는다(guard
    `TestConfigProfileSurfaceNeverReadsOrWritesSecrets`).
  - `VMInfo`는 anvil `tenant_id`/`egress_policy` propagation을 유지한 채 upstream의
    spawn-time `provider`/`model` snapshot 필드를 additive로 추가한다. `cmd/anvil-mcp`
    tool surface는 그대로다.
  - anvil `ANVIL_*` alias와 `EPHEMERA_*` canonical env 이름을 유지한다.
- upstream `v0.5.1`-`v0.5.5` (profile config API 확장, snapshot/restore UI,
  orchestration UI/Activity Feed, sizing preset, `system.md` 편집, System console)를
  sync 한다. anvil adaptation:
  - `/config/providers`는 provider별 API key 존재 여부만, `/config/clients`는
    control-plane client 이름과 만료만 반환하고 key/token 값은 노출하지 않는다
    (sentinel test).
  - profile `system.md` 편집은 `system.md`만 대상으로 하고 `64 KiB` cap을 적용하며,
    profile delete는 running VM이 그 profile을 쓰면 `409`, default profile은 예약,
    path traversal은 거부한다.
  - sizing preset과 per-VM `EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB`(struct
    `VcpuCount`/`MemSizeMib`)를 지원한다. snapshot metadata가 per-VM sizing을 기록하고
    legacy snapshot(0)은 restore 시 historical 2 vCPU / 2048 MiB로 fallback한다.
  - Town Wall author를 `SystemAuthor`로 migration하고, restore agent-wait를 30s에서
    60s로 늘린다.
- anvil sizing 결정 (KVM 근거로 승인): `v0.5.3`부터 upstream default VM sizing
  `1` vCPU / `1024` MiB를 채택한다(이전 2/2048). full e2e 3× `316✓`가 1024에서
  통과했다. flock member spawn이 per-profile `EPHEMERA_VCPU_COUNT`/
  `EPHEMERA_MEM_SIZE_MIB` override를 무시하고 `LookupProfile` default로만 sizing하는
  upstream-inherited gap은 follow-up으로 기록한다.
- Phase 2 KVM gate 중 고친 latent defect (`64ec57c`, 분류: `v0.5.x`
  `gracefulAgentStop`이 v0.2.0부터 잠재하던 upstream pooled-client 결함을 노출):
  guest IP가 VM destroy/create/restore 사이에 재활용되는데 shared keep-alive agent
  proxy client가 destroy된 VM으로 향하던 stale pooled connection을 재사용해, restored
  VM의 첫 proxied `/tasks`가 hang하거나 `502`(peer RST)로 실패했다(Phase 2 gate
  step 15 "Run task on restored VM"). fix는 proxy client의 keep-alive pooling을
  끄고(`DisableKeepAlives`) request마다 fresh dial하며 connection-reuse guard test를
  추가한다. upstream connection pooling과의 의도적 divergence이며 upstream 기여 후보다.
- upstream `v0.4.4` (streaming `/tasks`, nested-invocation depth guard,
  watchdog status route, flock broadcast, goose-agent slog migration)를 sync
  한다. anvil adaptation: buffered `POST /vms/{id}/tasks` 기본 계약(`stream=1`
  없으면 `{"output","error"}`)을 그대로 유지하고 MCP stdio tool은 이 phase에서
  buffered path만 호출한다(guard `TestRunTaskBuffered_DefaultShape`). nested task
  depth는 신설 canonical env `EPHEMERA_MAX_TASK_DEPTH`(기본 `5`, ANVIL alias
  없음)로 제한하며 한계 도달 시 `508`, `X-Ephemera-Task-Depth`를 `depth+1`로
  forwarding하고 header 부재는 `0`으로 취급한다. watchdog 상태는 read-only
  `GET /watchdog/status`(count/ID/config만)로 노출한다. flock broadcast는
  daemon API와 `ephemera-ctl flock broadcast` CLI로 채택하되 daemon-only endpoint로
  두며 `anvil_*` MCP tool로 노출하지 않는다(guard
  `TestToolRegistrationsExcludeBroadcast`,
  `TestCurrentIronClawSchemasExcludeBroadcastTool`). goose-agent는 `log/slog`로
  이전하고 `gtcall`이 depth header를 재전송한다. golden image rebake는 KVM gate에서
  확인했다.
- upstream `v0.4.5` (snapshot-restore auto-recovery)를 sync 한다. restore가
  `source_snapshot_id`(및 `tenant_id`/`egress_policy` attribution)를 담은
  `state.json`을 persist하고, daemon restart 시 `recoverRestoredVM`/
  `reRestoreMachine`이 source snapshot에서 auto-re-restore한다. restore 응답은
  `source_snapshot_id`/`tenant_id`/`egress_policy`/`profile`/`vm_id`/`guest_ip`/
  `agent_url`을 포함할 수 있고 `agent_token`/`agent_tokens`/`Authorization`/`Bearer`는
  절대 노출하지 않는다. snapshot GC와 `DELETE /snapshots/{id}`는 live·persisted
  restored VM state가 참조하는 source snapshot을 보호하며, 참조 중이면 `DELETE`가
  `409`를 반환한다. pre-v0.4.5 anvil test의 "restore leaves no recoverable state"
  assertion은 계획대로 persistence를 검증하도록 반전했고 redaction guard는 그대로
  유지한다.
- 의도적 divergence (upstream 대비): upstream e2e 46c는 live restored VM이 참조하는
  snapshot의 `DELETE`를 `200`으로 허용하고 VM을 orphan으로 둔다. anvil은 그 대신
  `409` 보호를 유지하고 VM을 먼저 삭제한 뒤 snapshot을 삭제하도록 요구한다. e2e 46c는
  이 divergence에 맞게 조정했고(commit `63df804`) `e2e_test.sh`에 divergence 주석을
  남겼다. 이 divergence는 `docs/ADR_INDEX.md`와
  `docs/operations/upstream-sync-policy.md`에 `adapted`로 기록한다.
- Phase 1 KVM gate 중 발견해 고친 두 pre-existing latent defect(이번 sync가 만든
  결함 아님).
  - `4c1c803`: restore handler가 COW device를 per-restore path 위에 bind-mount하는
    동안 Firecracker `LoadSnapshot`은 `state.bin`(`meta.DiskPath`)에 기록된 path를
    연다. source VM 삭제 후의 모든 restore가 v0.4.0 적응 이후 `500`으로 실패하던
    문제를 `meta.DiskPath` 위로 restore하도록 고쳐 upstream 및 recovery 경로와
    일치시켰다. 결함을 고정하던 v0.4.0-era test assertion 두 개도 함께 수정했다.
  - `38fbedc`: anvil 전용 `EnsureGoldenImageGooseAgent`가 시작 시 golden image를
    무조건 loop-mount해, SIGKILL crash 후 COW VM이 image를 pin한 상태에서 `EBUSY`로
    죽던 문제를, agent-stamp(source hash + image size + mtimeNs)가 current면 mount를
    건너뛰도록 고쳤다. live COW VM이 origin을 pin한 채 agent hash가 바뀌는 경우의
    loud-fail은 의도적으로 유지한다.

## 검증됨

- `go test ./internal/anvilmcp -count=1`
- `go test ./cmd/anvil-scheduler -count=1`
- `go test ./cmd/anvil-mcp -count=1`
- `go test ./scripts -count=1`
- `go test ./internal/storage -run 'TestEnsureKernel' -count=1`
- `go test ./cmd/goose-daemon -run 'TestWaitForAgentTimesOutHungHealthProbe|TestResolveWorkDir' -count=1`
- `go test ./... -count=1`
- `go build ./cmd/anvil-scheduler`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/goose-daemon`

v0.4 sync Phase 1 gate (upstream `v0.4.4`/`v0.4.5` 적응):

- CI-safe gate all green: `go build ./cmd/{goose-daemon,anvil-mcp,anvil-scheduler}`,
  `go test ./... -count=1` (EXIT=0).
- guard test: `TestRunTaskBuffered_DefaultShape`,
  `TestToolRegistrationsExcludeBroadcast`,
  `TestCurrentIronClawSchemasExcludeBroadcastTool`, proxy depth `508` guard.
- 실제 KVM host full e2e `sudo bash e2e_test.sh`: `316✓ / 0✗`
  ("All test steps passed"). step 59 real-LLM smoke만 provider key 부재로 skip.
- 전제: KVM gate는 working directory에 gitignore된 로컬 operator 파일
  `configs/goose.yaml`, `configs/goose-secrets.yaml`이 있어야 한다. 없으면
  `POST /vms`가 config injection 단계에서 `500`으로 실패한다.

v0.5 sync Phase 2 gate (upstream `v0.5.0`-`v0.5.5` operator support 적응):

- CI-safe gate all green: `git diff --check`, targeted test group, web build 재현
  가능(`cmd/goose-daemon/uidist/` drift 없음), `go test ./... -count=1` (EXIT=0),
  `go build ./cmd/{goose-daemon,anvil-mcp,anvil-scheduler}`.
- guard/sentinel test: `config_api_anvil_test.go`(`/config/profiles` auth·secret
  sentinel, `/config/providers`·`/config/clients` secret 비노출, profile in-use
  `409`), proxy connection-reuse guard.
- 실제 KVM host: `sudo bash e2e_test.sh` `316✓ / 0✗` ×3(step 59 real-LLM smoke만
  provider key 부재로 skip). `scripts/anvil-mcp-e2e.sh` lifecycle PASS·flock PASS,
  `scripts/vm-workload-e2e.sh` PASS.
- `scripts/anvil-mcp-e2e.sh semantic`은 key-free 구간(workspace copy-in/out, task
  proxy, cleanup)이 모두 `200`이고 마지막 LLM-echo substep만 로컬 Google API key가
  invalid해 실패했다(provider-key 의존, 코드 결함 아님).
- gate coverage correction: Phase 1 KVM gate는 `e2e_test.sh`만 실행하고
  `anvil-mcp-e2e.sh`(3개 모드)와 `vm-workload-e2e.sh`를 누락했다. 이 스크립트들은 현재
  HEAD(post-v0.5.5+fix)에서 처음 실행돼 두 baseline의 superset을 검증했다.

v0.6 sync Phase 3 gate (upstream `v0.6.0`-`v0.6.4` MCP gateway 적응):

- CI-safe gate all green: `git diff --check`, targeted test group(mcpgateway/boundary/
  audit/ratelimit/stdio guard 포함), web build 재현 가능(`uidist` drift 없음),
  `go test ./... -count=1`(EXIT=0), `go build ./cmd/{goose-daemon,anvil-mcp,anvil-scheduler}`.
- boundary guard: `TestToolRegistrationsExcludeGatewayTools`,
  `TestCurrentIronClawSchemasExcludeGatewayNamespacedTools`,
  `TestConfigMCPRoutesRequireAuthWhenConfigured`, `TestMCPInjectionCarriesURLNotCredential`,
  audit metadata-only sentinel, rate-limit per-(VM,server), resources/prompts shared-bucket,
  stdio env-canary/leak guards.
- 실제 KVM host `sudo bash e2e_test.sh`: `334✓ / 0✗`("All test steps passed").
  gateway step `84`-`89`가 최초 실행에서 green. provider-key skip 3건(LLM smoke,
  real tool call, real stdio tool call).
- `anvil-mcp-e2e.sh` lifecycle PASS·flock PASS — runtime gateway가 live인 상태에서도
  IronClaw adapter는 영향받지 않는다. `vm-workload-e2e.sh` PASS.
- `anvil-mcp-e2e.sh semantic`은 key-free 구간이 `200`이고 LLM-echo substep만
  known-invalid local Google key로 실패했다(provider-key 의존, Phase 2와 동일한
  release-gate item, 코드 결함 아님).

v0.7 sync Phase 4 gate (upstream `v0.7.0` installer/transcript/hardening 적응 — parity
scope 코드 편입 완료):

- CI-safe gate all green: `git diff --check`, installer `bash -n` ×3(install.sh/
  uninstall.sh/build_release.sh), targeted test group(transcript-safety guard 4종 포함),
  full glob `go test ./... -count=1` EXIT=0, web build drift 없음, 3 builds.
- 실제 KVM/installer gate: `sudo bash e2e_test.sh` `334✓ / 0✗`("All test steps passed").
  `anvil-mcp-e2e.sh` lifecycle+flock PASS, `vm-workload-e2e.sh` PASS. `anvil-mcp-e2e.sh
  semantic`은 standing provider-key caveat(key-free 구간 `200`).
- installer: `bash install.sh --help`/`bash uninstall.sh --help` OK(시스템 변경 없음).
  release build를 root로 실행 → SLIM(≈27M) + FULL(≈250M) tarball + `.sha256` checksum
  (dist/ gitignored). `build_release.sh`가 kernel/firecracker를 `sha256sum -c`로 검증.
- release-gate: 코드 항목 4종(audit-writer sentinel, stdio stderr scrub, `credential_env`
  reserved names, production-mux auth sentinel)은 2026-07-06 follow-up batch로 닫혔다
  (위 batch 항목 참조). 남은 open gate는 valid provider key로 `semantic` run(e2e step
  59, 사용자 key 교체 대기)뿐이다.

---

# v0.5.0 — Web UI

**Ephemera** v0.5.0 adds a **browser console** served by the daemon itself, replacing the script-form external client for everyday use. The existing control-plane API is unchanged and fully backward compatible; the only new server routes are `/ui/` (the embedded UI) and `/config/profiles` (profile model/provider editing). **Golden-image rebake**: `cmd/goose-agent` gains optional goose-session support, so the daemon rebuilds the golden image on first start after the change.

---

## What's New

### Embedded Web console (`/ui/`)

- The daemon serves a Svelte + Vite single-page app at **`/ui/`** on the same address as the API (`go:embed`, same origin — no CORS). The build output (`cmd/goose-daemon/uidist/`) is committed, so `go build` needs no Node toolchain; rebuild only after editing `web/` (`cd web && npm run build`). `/ui/` is mounted **outside** the auth/audit chain (the login page + JS bundle must load token-free; the bundle has no secrets), while every data call still flows through Bearer auth. `/` redirects to `/ui/`.
- Screens: token **Login** (auto-skipped when auth is disabled), **VM list** (live stats + model), **Create VM** (profile dropdown, one-time `agent_token`), **VM detail** (live stats + a cancelable streaming conversation panel), **Settings** (per-profile model/provider).

### English / Korean localization

- All UI strings are externalized to `web/src/locales/{en,ko}.json` and rendered via `svelte-i18n`. The initial language follows the browser (`ko*` → Korean, else English); a nav toggle switches and persists the choice in `localStorage`. Server-originated error text is shown verbatim.
- UI vocabulary is generic IT (display labels only — API identifiers unchanged): **Platform Agent** (in-VM goose agent), **Agent Group** (flock), **Activity Feed** (Town Wall), **Create/Delete** (spawn/destroy).

### Profile model/provider editing (`/config/profiles`)

- `GET /config/profiles` lists each profile (`default` + `configs/profiles/*`) with its `provider`/`model`; `PUT /config/profiles/{name}` rewrites `GOOSE_PROVIDER`/`GOOSE_MODEL` in place — comments and the `extensions:` block are preserved, values are validated against newline injection, and **API keys (`goose-secrets.yaml`) are never read or written** through this surface. The Settings screen drives both.
- Config is injected at spawn, so an edit applies to the **next** VM from that profile; already-running VMs keep their spawn-time model. `VMInfo` now records each VM's `provider`/`model` (returned by `GET /vms`, shown in the UI) so the distinction is visible.

### Multi-turn agent conversation

- `POST /vms/{id}/tasks` accepts an optional `session`. With it, `goose-agent` runs `goose run --output-format json -n <session> [--resume] -i -` — the first turn creates the named session, later turns `--resume` it — so the agent keeps conversation context across turns. Omitting `session` preserves the original stateless one-shot behavior (`ephemera-ctl`, `gtcall`, depth-guard tests unaffected). The control plane proxies the request body verbatim, so no control-plane change was needed.

### Graceful VM delete; "stop agent" removed

- `DELETE /vms/{id}` now best-effort asks the in-VM agent to shut down cleanly (`POST /stop`, 2 s) before force-stopping Firecracker, then frees TAP/IP/disk and deregisters. The old UI "stop agent" action — which actually halted the whole guest (the agent is the VM's init) while leaving the VM registered and the stats poller spamming the log — was removed; **Delete is the single teardown**. The stats agent-busy probe-failure log was demoted to Debug so a down/halted VM can no longer spam.

> **Caveats**: the Web UI conversation transcript is in-memory (a page reload starts a fresh `session`; the goose session persists in the VM but is not re-displayed). `scripts/build_image.sh` now removes any stale tarball before download, hardening the golden-image build against a leftover at the fixed `/tmp` path.

---

# v0.4.5 — Snapshot-Restore Auto-Recovery

**Ephemera** v0.4.5 closes a recovery gap from the Known Limitations audit. Additive — no wire format changed; no golden-image rebake (daemon/storage only).

---

## What's New

### Snapshot-restored VMs are auto-recovered across a daemon restart

- A VM created via `POST /snapshots/{id}/restore` now persists a `state.json` carrying `source_snapshot_id`. On the next daemon start, `RecoverVMs` **re-restores it from that source snapshot** (it cannot cold-boot like a spawn VM) instead of dropping it — automating what previously required a manual re-restore.
- Semantics: the VM returns to its **snapshot-time** memory and disk. Writes made *after* the original restore are not preserved across the restart (identical to a manual re-restore); the COW exception store is recreated fresh so the re-loaded snapshot memory and disk stay consistent.
- Shutdown handling: graceful shutdown discards a restored VM's dm device + transient exception store but **keeps its `state.json`** (recovery re-restores fresh). Restored VMs are excluded from the opt-in memory auto-snapshot (they re-restore from source, so an `auto/` image would never be used).
- Caveats (documented, by design): **bind-mount-fallback** restores (when dm-snapshot tooling is unavailable) are not auto-recovered; and if the **source snapshot was deleted** while the restored VM ran, recovery drops the VM and surfaces it (via `failed[]` / a flock agent marked dead) rather than silently keeping it.

### Known-Limitations refresh

- Removed the "Snapshot-restored VMs are not auto-recovered" limitation (now resolved above).
- Reworded the CP-token-rotation limitation to "CP token hot-rotation requires `_TOKENS_FILE`": the old "needs v0.3.4 VMs" clause is obsolete — the golden image auto-rebakes on any `goose-agent` change, so every current VM carries the `SET_CP_TOKEN` vsock handler. The only real requirement is sourcing tokens from a file (env tokens are fixed at exec and cannot change on SIGHUP).

---

# v0.4.4 — Feature Extensions

**Ephemera** v0.4.4 is the last single-host cycle, rounding out the operator surface: **PR-A** flock broadcast + a watchdog status route; **PR-B** streaming `/tasks`, the `goose-agent` slog migration, and a nested-invocation depth guard. Additive — no wire format changed.

---

## What's New

### Flock broadcast (PR-A)

- `POST /flocks/{id}/broadcast` `{"body":"…"}` scatters one prompt to **every** member agent's `/tasks` endpoint in parallel and gathers each agent's result (scatter-gather). The response carries a `sent`/`skipped`/`failed` tally plus a per-agent `results` map (`status` = `ok`/`busy`/`error`, with `output`/`error`). Agents already running a task answer `503` and are reported `busy` (skipped); unreachable agents are `error`. The call blocks until every agent finishes, like calling `/tasks` on each — cancellation rides on the request context.
- The broadcast is also recorded once on the Town Wall (`orchestrator` author) so observers see it happened.
- `ephemera-ctl flock broadcast <flock_id> <message>` wraps the endpoint (trailing words are joined, so the message need not be quoted).

### Watchdog status route (PR-A)

- `GET /watchdog/status` exposes the health watchdog's tunables (`interval_sec`, `timeout_sec`, `dying_threshold`, `auto_heal`) and live per-VM state (`vm_fail_counts` — VMs with a non-zero consecutive-failure count; `vm_dead_marked` — VMs the watchdog has marked dead). Read-only, behind the same auth as the other internal routes. The watchdog has run since v0.3.4 but had no status route until now; the snapshot is taken under the same lock the polling loop uses, and the returned maps are copies.

### Streaming `/tasks` (PR-B)

- `POST /vms/{id}/tasks?stream=1` streams the task as **newline-delimited JSON** over chunked transfer instead of buffering the whole result: zero or more `{"type":"progress","text":"…"}` frames (relayed from goose's stderr activity, plus a 15s heartbeat) followed by exactly one `{"type":"result","output":"…","error":"…"}` frame that mirrors the legacy `TaskResult`. The default (no `stream=1`) path is **unchanged** — full backward compatibility. The control-plane proxy now flushes per chunk (the same `http.Flusher` plumbing the Town Wall SSE stream relies on), so the stream reaches the caller incrementally.
- **Caveat**: streaming commits a `200` before goose runs, so a goose failure can no longer be a `500` — the error rides in the `result` frame's `error` field. Streaming clients must inspect `result.error`, not the status code. The buffered path keeps its `500`.

### Nested-invocation depth guard (PR-B)

- Agent→agent dispatch (`gtcall`) is now loop-guarded. The control plane reads `X-Ephemera-Task-Depth` on every proxied `/tasks` hop (absent → 0), refuses a hop at/over `EPHEMERA_MAX_TASK_DEPTH` (default 5) with **`508 Loop Detected`**, and forwards `depth+1`. `goose-agent` injects the incoming depth into the goose subprocess environment (`EPHEMERA_TASK_DEPTH`), and `gtcall` re-sends it as the header, so depth accumulates across the whole nested call tree. Distinct from the agent's own `503` busy response.

### `goose-agent` slog migration (PR-B)

- The in-VM `goose-agent` moved off the plain `log` package to `log/slog`, mirroring the host daemon's `EPHEMERA_LOG_FORMAT` (text|json) / `EPHEMERA_LOG_LEVEL` handling (default level Warn). Completes the slog migration begun in v0.3.5.

> **Golden-image rebake**: PR-B edits `cmd/goose-agent` and `scripts/gtcall`, so the daemon rebuilds the golden image on first start after the change (mtime check in `EnsureGoldenImage`).

> **anvil adaptation**: The buffered `POST /vms/{id}/tasks` contract is preserved verbatim — without `?stream=1` the daemon still returns the single `{"output","error"}` object, and anvil's MCP stdio tools keep calling the buffered path. `POST /flocks/{id}/broadcast` stays a **daemon-only** endpoint; it is **not** registered as an `anvil_*` MCP tool in this phase, so the IronClaw tool schema carries no broadcast tool.

---

# v0.4.3 — Flock Lifecycle

**Ephemera** v0.4.3 makes flocks adjustable at runtime: **PR-A** dynamic agent
membership (add/remove/role-change), **PR-B** flock pause/resume + per-flock
agent cap, **PR-C** Town Wall history filters + log rotation. Additive — no
wire format changed.

---

## What's New

### Dynamic flock agent management (PR-A)

- `POST /flocks/{id}/agents` `{"role":"worker"}` — spawn and attach a new
  agent. The `agent_id` follows the per-role `role-N` rule (max existing N + 1).
  In anvil, the daemon keeps the one-time guest token internally and omits
  `agent_token`/`agent_tokens` from this response. The 20-agent-per-flock cap is
  enforced.
- `DELETE /flocks/{id}/agents/{agent_id}` — tear down the agent's VM and remove
  it from the flock. Removing the last agent leaves an empty flock (recoverable
  via add; use `DELETE /flocks/{id}` for the whole flock).
- `PATCH /flocks/{id}/agents/{agent_id}` `{"role":"reviewer"}` — change an
  agent's role. Because role binds VM sizing + system prompt at spawn time, the
  VM is recreated under the new role (`agent_id` and `agent_token` preserved,
  like restart).
- `ephemera-ctl flock add-agent`/`rm-agent`/`set-role` wrap the three endpoints.
- Each membership change is posted to the Town Wall; the watchdog auto-discovers
  added VMs and forgets removed ones.

### Flock pause/resume + per-flock max_agents (PR-B)

- `POST /flocks/{id}/pause` / `POST /flocks/{id}/resume` — pause/resume **all**
  member VMs via Firecracker PauseVM/ResumeVM. **Runtime-only**: agent status
  flips to `paused` while paused, then resumes to the pre-pause lifecycle status;
  `Flock.Paused` toggles, but nothing is persisted (a daemon restart brings
  members back running). A partial pause failure rolls back (resumes
  already-paused members and restores their prior statuses).
- The health watchdog **skips dead-marking `paused` agents** — a paused VM
  intentionally doesn't answer `/health`, so it must not be marked dead.
- `POST /flocks` accepts `max_agents` — a per-flock agent cap (default 20),
  enforced on create **and** on `POST /flocks/{id}/agents`. Persisted in
  metadata (backward-compatible; an absent/0 value falls back to the default).
- `ephemera-ctl flock pause`/`resume`; `flock create --max-agents N`.

### Town Wall query filters + log rotation (PR-C)

- `GET /flocks/{id}/wall/history` accepts filters: `agent_id` (exact),
  `since`/`until` (RFC3339, inclusive), `contains` (body substring). Combinable;
  an all-empty filter returns the active-log history.
- The Town Wall log now **rotates by size**: past `EPHEMERA_TOWNWALL_MAX_MIB`
  (default 10) the active log shifts to `.1` (…→`EPHEMERA_TOWNWALL_KEEP`,
  default 3) and a fresh file continues. `History` reflects the active file
  (rotated backups stay on disk, mirroring the audit log).

---

# v0.4.2 — COW Spawn Support Without Default Flip

**anvil**은 upstream ephemera v0.4.2의 COW spawn rootfs와 COW+Diff snapshot 지원을
merge하지만, runtime 기본 disk mode는 아직 바꾸지 않는다. `EPHEMERA_DISK_MODE`
unset, `plain`, `full`은 기존 full byte-for-byte clone을 사용하고 COW probe도
수행하지 않는다. `EPHEMERA_DISK_MODE=cow`를 명시한 경우에만 dm-snapshot COW를
probe하고, probe 실패 시 plain clone으로 fallback한다.

Upstream ephemera v0.4.2 promotes copy-on-write spawn disks to the default, with
measured ~43% faster spawn (1.96s -> 1.12s warm) and ~0 MiB initial per-VM disk.
anvil keeps the feature opt-in for this slice so operators can adopt the kernel
device-mapper dependency explicitly. Additive — no wire format changed.

---

## What's New

### COW spawn rootfs is available explicitly

- `EPHEMERA_DISK_MODE=cow` selects COW. Unset, `plain`, and `full` select the existing full byte-for-byte copy and skip the COW probe.
- `storage.DMSnapshotAvailable()` probes the host when COW is explicitly requested (tool presence + a `dmsetup version` device-mapper round-trip + the `dm_snapshot` module); on failure the daemon logs a warning and uses a full clone. The strategy is resolved once into `ControlPlane.useCOW` instead of re-reading the env on every spawn.
- Recovery is unaffected: each VM's `DiskMode` is recorded from the actual provisioning result, so plain and COW VMs both cold-restart correctly.
- COW spawn VMs now support **Diff snapshots**: `WriteRootfsDiff` reads the rootfs size via `blockdev` when the current rootfs is a dm-snapshot block device (whose `Stat().Size()` is 0), so a COW VM's 2nd-and-later snapshot is a sparse rootfs diff. Previously this hit a size-mismatch — the COW+Diff combination was untested while COW was opt-in.

---

# v0.4.1 — Operational Interfaces

**Ephemera** v0.4.1 makes the daemon operable as a service: authenticated **client identity** threaded into request handling, a per-request **access audit log** (`GET /audit`), **per-token TTL/rotation**, and a dependency-free **operator CLI** (`ephemera-ctl`). Additive — no wire format changed; the only behavior changes are that an expired token is now rejected (401) and the in-VM CP token is the first non-expired client.

---

## What's New

### Client identity in request context (F1)

- `authMiddleware` now surfaces the authenticated caller: the matched client name is threaded to handlers via the request context and to the outer audit middleware via a request-scoped holder. Timing-safety is preserved: every token is still compared with no early-exit, and the expiry check runs after the constant-time loop.

### Access audit log — `GET /audit` (F2)

- Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl`: `{ts, client, method, path, status, duration_ms, remote_addr, bytes}`. The record never contains tokens, the `Authorization` header, request/response bodies, or the query string. Unauthenticated requests record `client="-"`; `/metrics` is excluded so scrapes do not flood the log.
- The file is size-rotated (`EPHEMERA_AUDIT_MAX_MIB`, default 100; `EPHEMERA_AUDIT_KEEP`, default 5). On by default; `EPHEMERA_AUDIT_DISABLE=true` turns it off.
- `GET /audit?limit=&client=&status=&method=` returns recent records as a JSON array, newest first, with limit default 100 and max 1000.
- `statusRecorder` captures status/bytes and forwards `http.Flusher` so the SSE Town Wall stream keeps working; the audit middleware wraps auth so it also records 401s and final status.

### Per-token TTL + rotation (F3)

- Token entries gain an optional expiry: `name:token:expires` (RFC3339 or Unix seconds). A two-field `name:token` never expires. Tokens may contain `:`; the expiry is recognized only when the trailing colon-separated field parses as a timestamp.
- Expiry is enforced per request: an expired-but-matched token returns 401 with the same body as an unknown token, distinguished only by the server log and `ephemera_auth_total{outcome="expired"}`.
- The in-VM control-plane token is now the first non-expired client. If all tokens have expired, an empty unauthenticated token is propagated with a warning.
- New metric `ephemera_auth_total{outcome=ok|denied|expired}`; startup/SIGHUP banners log expired and expiring-within-24h counts.

### Operator CLI — `ephemera-ctl` (F4)

- New `cmd/ephemera-ctl`, a stdlib-only HTTP wrapper over the control-plane API: `vm spawn/ls/rm/health/stop/task/stats/snapshot`, `flock create/ls/get/rm/post/wall/restart`, `snapshot ls/restore/rm`, `audit`, and `metrics`. Build with `go build -o ephemera-ctl ./cmd/ephemera-ctl/`.
- Reads `EPHEMERA_CTL_URL` (default `http://127.0.0.1:3000`) and a bearer from `--token`, `EPHEMERA_CTL_TOKEN`, or `EPHEMERA_API_TOKEN`. Human-readable tables are the default; `--json` returns raw output. Non-2xx responses go to stderr and exit non-zero.

### Tests

- Unit: token TTL parsing, `parseExpiry`, `firstActiveClient`, `countTokenExpiry`; `authMiddleware` outcomes and metric behavior; audit rotation/tail filters/no-secret-leak; `statusRecorder` Flusher forwarding; client-identity context; CLI client round-trip, URL/token resolution, and flag parsing.
- E2E steps 78–83 cover audit records, 401 audit entries, per-token TTL expiry, SSE through the audit wrapper, and `ephemera-ctl` spawn/list/delete plus audit access against the live daemon.

# anvil v0.3.2 — Scheduler replication and flock placement

- Tag: `anvil-v0.3.2`
- GitHub Release:
  <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.2>
- Published: 2026-06-04 14:22:49 KST
- Target commit: `18b4506204a68a8fd9e3608976727953869f94a6`

`anvil-v0.3.2`는 `anvil-v0.3.1` 이후 scheduler 기반 runtime 운영성을 확장한
historical release다. 해당 release의 upstream ephemera runtime baseline은
`v0.3.6`이며, upstream `v0.4.0` PR-A storage/recovery 변경은 포함하지 않는다.

## 추가됨

- manual cross-host snapshot replication:
  - daemon `POST /snapshots/{id}/export`는
    `application/vnd.anvil.snapshot-bundle` streamable bundle을 export한다.
  - daemon `POST /snapshots/import`는 bundle을 staging/validation 후 atomic publish로
    target snapshot directory에 반입한다.
  - MCP `anvil_replicate_snapshot`은 `snapshot_id`, `source_host`, `target_host`,
    `include_dependencies` 입력으로 source export stream을 target import로 전달한다.
  - RuntimeRouter는 replication 성공 후 target host만 scheduler
    `PlacementStoreState.SnapshotLocations`에 기록한다.
- scheduler service 전용 `GET /metrics` endpoint:
  - `anvil_scheduler_control_loop_running`
  - `anvil_scheduler_persistence_degraded`
  - `anvil_scheduler_host_status_count`
  - `anvil_scheduler_suspect_vm_placements`
  - 마지막 poll/reconcile 완료 timestamp gauge
- `cmd/anvil-scheduler` full-process integration test는 hosts file 기반 bootstrap,
  오래된 state override, fake daemon `/health`, scheduler `/control-loop/status`,
  `/schedule/spawn`, `/metrics` 경로를 검증한다.
- scheduler smoke harness가 `/metrics`를 검증한다.
- MCP `anvil_spawn_flock` scheduler-aware single-host placement:
  - scheduler router config가 있을 때 roles 수 기반 active VM quota/capacity로 host를
    선택한다.
  - 선택된 host의 기존 daemon `POST /flocks`를 호출하고, 반환된 member VM placement를
    scheduler `PlacementStore`에 기록한다.
  - daemon direct `POST /flocks` wire contract와 `agent_token` 비노출 조건은 유지한다.

## 보안/운영 강화

- replication response, audit, operator 문서는 `agent_token`, authorization header,
  daemon raw body, raw `metadata.json` body를 노출하지 않는다.
- `POST /snapshots/{id}/export` bundle의 `metadata.json`은 raw local metadata가 아니라
  token을 제거한 portable metadata를 사용한다. `disk_path`와 `vsock_path`는
  Firecracker restore 제약 때문에 safe path로 검증한 뒤 보존한다.
- 복제된 snapshot restore는 target daemon이 새 `agent_token`을 생성해 guest agent에
  vsock으로 주입하므로 source host token을 재사용하지 않는다.
- diff snapshot replication은 target host에 base full snapshot이 필요하다.
  `include_dependencies=true`이면 router가 base full을 먼저 복제하고 diff를 복제한다.
- scheduler metrics에는 `agent_token`, host endpoint, daemon raw response,
  authorization header, snapshot metadata가 들어가지 않는다.
- 실제 systemd 검증은 명시적 operator action으로 유지한다:
  `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`.

## 검증됨

- `go test ./... -count=1`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n scripts/anvil-scheduler-smoke.sh`
- `bash -n scripts/install-anvil-scheduler-systemd.sh`
- `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`

# anvil v0.3.1 — Scheduler control loop and operational roadmap

- Tag: `anvil-v0.3.1`
- GitHub Release:
  <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.1>
- Published: 2026-05-29 01:48:25 KST
- Target commit: `1f63f04bc559270ca3fa2f5b9ee80078927ead93`

`anvil-v0.3.1`은 `anvil-v0.3.0` 이후 scheduler service를 장시간 운영 가능한
control-plane process에 가깝게 확장한 release다. upstream ephemera `v0.4.0`
PR-A storage/recovery 변경은 포함하지 않는다.

## 추가됨

- scheduler control loop:
  - configured runtime host의 `/health`를 주기적으로 poll한다.
  - degraded/unhealthy host 전이를 `HostObservation`으로 저장한다.
  - daemon `GET /vms` 결과로 VM placement를 reconciliation한다.
  - host 장애 중 기존 VM placement를 `suspect_vm_placements`로 표시한다.
- scheduler bootstrap config:
  - `ANVIL_SCHEDULER_HOSTS_FILE`
  - `ANVIL_SCHEDULER_POLL_INTERVAL`
  - `ANVIL_SCHEDULER_RECONCILE_INTERVAL`
  - `ANVIL_SCHEDULER_HOST_TIMEOUT`
  - `ANVIL_SCHEDULER_FAILURE_THRESHOLD`
  - `ANVIL_SCHEDULER_API_TOKEN`
  - `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE`
- scheduler API:
  - `GET /control-loop/status`
  - 확장된 `GET /placements` state.
- persistence degraded gate:
  - `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true`이면 scheduler state 저장 장애 중 신규
    scheduling을 `503`으로 차단한다.
- scheduler smoke harness:
  - `scripts/anvil-scheduler-smoke.sh`가 `/control-loop/status`까지 확인한다.
  - fake smoke host는 `smoke_only: true`로 등록되어 일반 scheduling fallback에
    섞이지 않는다.
- 후속 개발 문서:
  - `docs/operations/2026-05-29-anvil-follow-up-development.md`.

## 보안/운영 hardening

- current daemon `/health`가 scheduler capacity fields를 생략해도 hosts file의
  `available_vms`, `available_snapshot_bytes`, `egress_policies`를 보존한다.
- `/health`의 `egress_policies`는 omitted와 explicit empty list를 구분한다.
  omitted는 기존 정책을 보존하고, `[]`는 stale policy를 명시적으로 비운다.
- config-managed host는 hosts file이 source of truth다. hosts file에서 제거된
  managed host는 scheduler state에서도 제거되고, observation/status/suspect state가
  함께 정리된다.
- `DELETE /hosts/{name}`은 config-managed host를 거부하며, runtime-added host 삭제 시
  `HostObservations`, `ControlLoopStatus.Hosts`, `ConfigManagedHosts`,
  `SuspectVMPlacements`를 정리한다.
- `agent_token`은 scheduler surface와 release artifact에 노출하지 않는다.

## 검증됨

- `go test ./... -count=1`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n scripts/anvil-scheduler-smoke.sh`
- `bash -n scripts/install-anvil-scheduler-systemd.sh`
- `bash -n scripts/vm-workload-e2e.sh`
- local scheduler binary smoke:
  - `/control-loop/status` returned `running: true`
  - `scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010` returned
    `ok: true`
- `bash scripts/install-anvil-scheduler-systemd.sh --dry-run --no-build --no-enable --verify`
- independent code review: no blocking or important issues after review fixes.

## 다음 후보

- scheduler full-process integration test.
- 실제 systemd host에서 scheduler 운영 배포 검증.
- scheduler observability metrics/alerts.
- cross-host snapshot replication.
- scheduler-aware cross-host flock placement.
- egress L7 proxy/SNI hardening.
- snapshot storage quota dashboard.
- scheduler host registration hardening.
- upstream ephemera `v0.4.0` PR-A adoption review. 이 항목은 당분간 구현 범위에서
  제외한다.

# anvil v0.3.0 — ephemera v0.3.6 runtime baseline and workload automation

- Tag: `anvil-v0.3.0`
- GitHub Release:
  <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.0>
- Published: 2026-05-28 14:57:59 KST
- Target commit: `95215e2cf85b14f82cf5d0ef7caa2b1ea77da992`

`anvil-v0.2.0` 이후 다음 runtime/product 변경을 공개 release로 게시했다.

## 추가됨

- upstream ephemera `v0.3.2`-`v0.3.6` runtime baseline을 anvil `main`에
  병합했다.
  이 변경은 anvil의 제품 정체성을 ephemera로 바꾸지 않고, Firecracker MicroVM
  runtime substrate를 최신 baseline으로 끌어올린다.
  - `v0.3.2`: live VM cold-restart와 `vms/<vm_id>/state.json`.
  - `v0.3.3`: watchdog dead persistence, per-agent restart, in-VM CP token
    auto-injection.
  - `v0.3.4`: `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out,
    watchdog tunables/auto-heal, Firecracker SIGHUP forwarding hot-fix.
  - `v0.3.5`: `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo.
  - `v0.3.6`: autonomous multi-agent webdev demo, in-VM `gtcall`,
    multi-line-safe `gtwall`, Goose JSON output parsing.
- guest `goose-agent`에 authenticated `POST /workloads/run` endpoint를 추가했다.
  이 경로는 `/workspace/workloads/*.sh` script만 실행하며 LLM provider credential과
  `/tasks` prompt 실행에 의존하지 않는다.
- daemon에 `POST /vms/{vm_id}/workloads/run` proxy route를 추가했다. 기존 agent
  token 주입 proxy 경로를 재사용하므로 외부 caller는 control-plane auth만 사용한다.
- `scripts/vm-workload-e2e.sh`는 deterministic workload 실행을 `/tasks`에서
  `/workloads/run`으로 전환했다. 결과 artifact는 `nginx-run.json`,
  `go-http-run.json`, `nginx.log`, `go-http.log`, `bench.txt`, `host-bench.txt`를
  사용한다.
- Go HTTP workload는 VM 내부에서 대형 Go toolchain을 설치하지 않고 host에서
  `linux/amd64` static binary를 build한 뒤 gzip artifact로 업로드한다.
- `webdev_demo.sh`는 orchestrator, worker, reviewer flock이 React + Vite site를
  VM 내부에서 생성하고 Town Wall을 통해 host가 수확하는 manual operator demo를
  제공한다. demo는 Gemini API key, `/dev/kvm`, 충분한 host memory가 필요하다.
- VM golden image에 `gtcall`을 추가했다. flock VM 내부 agent는 peer `agent_token`을
  알지 않고 control-plane proxy를 통해 다른 agent의 `/tasks`를 호출할 수 있다.
- `goose-agent`의 `/tasks` 실행은 Goose `--output-format json` envelope에서 assistant
  text를 추출해 반환한다. Goose startup banner와 streaming-buffer code block
  truncation 때문에 host가 온전한 긴 source output을 받지 못하던 경로를 완화한다.
- VM 내부 `gtwall`과 `gtcall`은 JSON body를 `jq -n --arg`로 구성해 newline, quote,
  backtick이 들어간 multi-line body를 안전하게 전달한다.

## 보안/운영 hardening

- workload path는 `workloads/` prefix, `.sh` suffix, regular file 조건을 만족해야
  하며 final file symlink, `workloads` root symlink, nested parent directory symlink를
  거부한다.
- workload timeout 시 `bash` process group 전체에 `SIGKILL`을 보내 background child
  process가 남지 않도록 했다.
- workload process는 parent process `os.Environ()`을 상속하지 않고 최소 env만 받는다.
- stdout/stderr는 각각 1 MiB로 cap하고 truncation flag를 JSON result에 기록한다.
- `EPHEMERA_*`, `goose-*`, `ephemera_*`는 upstream runtime compatibility namespace로
  유지한다. anvil 공개 product surface는 `anvil_*` MCP tool, scheduler,
  tenant/egress, workload automation으로 설명한다.
- `/metrics`는 upstream 기본값상 unauthenticated이다. 외부 노출 환경에서는
  `EPHEMERA_METRICS_REQUIRE_AUTH=true` 또는 network isolation을 release/runbook에
  명시해야 한다.
- `/root/.ephemera-cp-token`, `vms/<vm_id>/state.json` 안의 `agent_token`,
  API bearer token은 runtime secret으로 취급하고 MCP output, audit, metrics, trace,
  replay fixture, release artifact에 노출하지 않는다.
- `gtcall`은 peer credential을 VM 내부에 직접 노출하지 않고 control-plane proxy가
  기존 agent token injection 경계를 계속 소유한다.

## 검증됨

- `go test ./...`
- `go build -o anvil-daemon ./cmd/goose-daemon/`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n webdev_demo.sh`
- `bash -n scripts/build_image.sh`
- `bash -n scripts/workloads/nginx-smoke.sh`
- `bash -n scripts/workloads/go-http-bench.sh`
- `bash -n scripts/vm-workload-e2e.sh`
- 실제 KVM host에서 `sudo -n bash e2e_test.sh`
  - 전체 단계 통과
  - real-LLM `/tasks` smoke는 provider key가 없어 skip
  - `/metrics`, `/vms/{vm_id}/stats`, SIGHUP CP-token rotation 검증 통과
- 실제 KVM host에서 `sudo -n bash scripts/vm-workload-e2e.sh`
  - artifact: `/tmp/anvil-workload-e2e-20260526-171326`
  - `pass: true`
  - nginx host probe: `200`
  - Go HTTP host probe: `200`
  - host benchmark: `curl-loop`
- artifact secret scan:
  `GOOGLE_API_KEY|ANTHROPIC_API_KEY|OPENAI_API_KEY|Authorization: Bearer|agent_token`
  패턴 없음.
- `bash scripts/secret-scan.sh`
  - tracked tree: `PASS`
  - git history: `WARN` — 과거 secret-like fixture/history가 있어 rotate 필요
  - ignored/local files: `WARN` — local `goose-secrets.yaml` 계열 값은 출력하지 않음

scheduler service 운영 검증 자동화는 `scripts/anvil-scheduler-smoke.sh`와
`scripts/install-anvil-scheduler-systemd.sh --verify`로 포함됐다. smoke harness는
기본 host id를 `anvil-scheduler-smoke-*`로 생성하고, 같은 host id의 기존 inventory
record가 있으면 `PUT /hosts` 전에 실패해 운영 record를 덮어쓰지 않는다. 등록한 fake
host는 `DELETE /hosts/{name}`로 정리해 production placement 후보가 남지 않게 한다.
fake host는 `smoke_only: true`로 등록되어 `PreferredHosts`에 명시된 smoke 요청에서만
선택되고, smoke harness는 `PreferredHosts` 없는 추가 `/schedule/spawn`으로 일반
fallback에서 무시되는지도 확인한다.
다음 후보는 upstream ephemera `v0.4.0` PR-A storage/recovery 변경의
adoption review, cross-host snapshot replication, scheduler-aware cross-host flock
placement, L7 egress proxy/SNI hardening, snapshot storage quota dashboard다.

## 문서화됨

- script-only workload runner의 design/implementation plan, runtime/service
  architecture, runbook, workload E2E design spec을 현재 `/workloads/run` 계약에
  맞춰 갱신했다.
- upstream ephemera `v0.3.2`-`v0.3.6`을 anvil runtime baseline으로 분류하고,
  product identity와 runtime namespace 경계를 README, public release boundary,
  ADR index, upstream sync policy, release checklist에 반영했다.
- upstream ephemera `v0.3.2`와 `v0.3.3`의 병합 전 검토 근거는
  `docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`에 historical analysis로
  보존한다.
- `PUBLIC_RELEASE_BOUNDARY.md`, `ADR_INDEX.md`, `upstream-sync-policy.md`,
  `release-checklist.md`에 upstream `v0.3.2`-`v0.3.6`이 `adapted` runtime
  baseline임을 반영했다.

# anvil v0.2.0 — Runtime scheduler, Goosetown MCP, observability foundation

- Tag: `anvil-v0.2.0`
- GitHub Release:
  <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.2.0>
- Published: 2026-05-15 17:53:21 KST
- Target commit: `5b8298fab17b455a9e4e4325618d2743d9486a6c`

`anvil-v0.2.0`은 `anvil-v0.1.0` 통합 표면 위에 multi-host runtime foundation,
Goosetown operational hardening, 운영 관측성 계약을 추가한다.

## 추가됨

- `cmd/anvil-scheduler`: persistent host/quota/placement state를 읽어
  `/health`, `/hosts`, `/hosts/{name}`, `/placements`, `/reconcile`,
  `/schedule/spawn`, `/schedule/restore`를 제공하는 얇은 HTTP scheduler service.
- `internal/anvilmcp` scheduler 확장:
  - persistent `PlacementStore`
  - JSON-backed `QuotaStore`
  - snapshot locality preferred host
  - spawn/restore retry/failover
  - daemon `GET /vms` 기반 placement reconciliation
  - IronClaw/Gemini function declaration용 tool input schema compatibility 검증
- daemon 운영 endpoint:
  - `GET /health`
  - `GET /metrics`
  - `GET /metrics/vms`
  - `GET /tenants`, `GET/PUT /tenants/{tenant_id}`
  - `GET /audit/runtime`, `POST /audit/runtime/prune`
- profile egress policy:
  - `EPHEMERA_EGRESS_PROFILE_DIR` 또는 `ANVIL_EGRESS_PROFILE_DIR` 아래
    `configs/profiles/{profile}/egress.json` 형식의 allow CIDR, allow host,
    DNS server allowlist.
  - `deny_all`과 `profile` policy는 host `iptables` rule로 계획/적용한다.
- observability:
  - lifecycle counter와 duration sum/count metric
  - lifecycle queue depth
  - `/metrics/vms`의 per-VM JSON metric
  - `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` 또는 `OTEL_EXPORTER_OTLP_ENDPOINT` 기반
    optional HTTP trace export
- anvil MCP Goosetown tool surface:
  - `anvil_spawn_flock`
  - `anvil_list_flocks`
  - `anvil_get_flock`
  - `anvil_delete_flock`
  - `anvil_post_townwall`
  - `anvil_get_townwall_history`
- ephemera upstream `v0.3.1` Goosetown hardening:
  - flock member VM health watchdog
  - flock metadata persistence와 daemon restart 후 read-mostly recovery
  - Town Wall monotonic `seq`
  - stale daemon pre-flight cleanup과 bind 실패 fatal startup

## 변경됨

- ephemera upstream `v0.3.0`/`v0.3.1` 기반 runtime 변경을 mainline에 병합했다.
  Goosetown flock, Town Wall, 역할별 VM sizing, system prompt 주입, 선택적 COW
  spawn disk, flock persistence/watchdog/seq는 ephemera runtime 계약으로 유지하고,
  anvil의 tenant/egress/metrics/trace 계약과 함께 동작하도록 통합했다.
- IronClaw/Gemini schema compatibility 검증은 `roles`처럼 array input을 쓰는
  tool field에 대해 array item type metadata도 기록한다.
- daemon direct `POST /flocks`는 blank `task`, empty role, path separator가 포함된
  role을 flock registry 생성과 VM spawn 전에 `400`으로 거부한다.
- upstream `v0.3.1`의 `POST /flocks` `agent_tokens` 응답 노출은 anvil 불변 조건에
  맞춰 채택하지 않는다. `agent_token`은 계속 `POST /vms` 응답 외에는 노출하지 않는다.
- `profile` egress policy는 policy 파일이 없는 기존 profile에서는 no-op으로 남아
  기존 로컬 profile 호환성을 유지한다.
- trace export와 runtime audit API는 token, secret, daemon raw body, snapshot
  metadata, `agent_token`을 기록하지 않는 redaction 계약을 따른다.
- release/build 검증 대상에 `go build ./cmd/anvil-scheduler`를 포함한다.

## 검증됨

- `go test -count=1 ./...`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n e2e_test.sh`
- `bash -n scripts/build_image.sh`
- `bash -n scripts/anvil-mcp-e2e.sh`
- `git diff --check`
- 실제 KVM host에서 `go build -o anvil-daemon ./cmd/goose-daemon/` 후
  `sudo bash e2e_test.sh`
- daemon 실행 상태에서 `scripts/anvil-mcp-e2e.sh flock`

# ephemera v0.3.1 — Goosetown operational hardening

ephemera `v0.3.1`은 `v0.3.0`의 Goosetown flock/Town Wall 계약 위에 장시간 운영
안정성 보강을 추가한 runtime 릴리즈다. anvil downstream은 이 hardening을 병합하되
`POST /vms` 외 응답에서 `agent_token`을 노출하지 않는 기존 보안 불변 조건을
유지한다.

## 새 기능

### Flock member health watchdog

- daemon은 flock member VM의 `/health`를 주기적으로 확인한다.
- 연속 실패 임계값을 넘으면 agent status를 `dead`로 표시하고 Town Wall에
  orchestrator notice를 남긴다.
- standalone VM은 watchdog 대상이 아니다.

### Flock metadata persistence

- `POST /flocks`는 `flocks/<flock_id>/metadata.json`을 원자적으로 기록한다.
- daemon 시작 시 `metadata.json`을 scan해 flock registry와 Town Wall log를
  read-mostly 상태로 복구한다.
- 복구된 flock의 VM process 자체는 재시작하지 않는다. `/post`, `/wall`,
  `/wall/history`, `DELETE`는 계속 사용할 수 있고 live VM auto-restart는 후속
  runtime 후보로 남는다.

### Town Wall monotonic sequence

- Town Wall `Message`에 per-flock monotonic `seq`가 추가됐다.
- subscriber는 `seq` gap을 감지하고 `/wall/history`로 누락 범위를 복구할 수 있다.

## 변경됨

- flock 역할별 `agent_id`는 role별 번호를 사용한다.
  예: `researcher-1`, `researcher-2`, `worker-1`.
- daemon startup은 API listener bind 실패 시 fatal로 종료한다.
- e2e pre-flight는 stale daemon과 `flocks/flock-*` runtime directory를 정리한다.
- `goose-agent`, `micro-init`, golden image는 source/build input stale 여부를
  확인해 필요한 경우 자동 재빌드한다.

## anvil downstream 차이

- upstream `v0.3.1`은 `POST /flocks` 응답에 `agent_tokens` map을 추가했지만,
  anvil은 `POST /vms` 응답 외 `agent_token` 비노출 정책을 유지한다.
- 외부 caller는 flock member direct token 대신 control-plane `/flocks/{id}/post`,
  Town Wall history, MCP `anvil_post_townwall`/`anvil_get_townwall_history`를
  사용한다.

# ephemera v0.3.0 — Goosetown multi-agent orchestration

ephemera `v0.3.0`은 `v0.2.0`의 단일 VM lifecycle, snapshot, restore 계약을
유지하면서 역할별 MicroVM 여러 개를 하나의 flock으로 다루는 Goosetown 실행
모델을 추가한 runtime 릴리즈다. 기존 `POST /vms`, snapshot/restore, agent proxy
endpoint는 backward compatible하게 유지된다.

## 새 기능

### Multi-agent flock

- 새 endpoint:
  - `POST /flocks`
  - `GET /flocks`
  - `GET /flocks/{flock_id}`
  - `DELETE /flocks/{flock_id}`
- `POST /flocks`는 역할 목록을 받아 orchestrator, researcher, worker, reviewer
  같은 역할별 VM을 생성하고 하나의 flock ID로 묶는다.
- blank `task`, empty role, `/` 또는 `\`가 포함된 role은 flock registry 생성과
  VM spawn 전에 `400`으로 거부한다.
- 한 flock은 최대 20개 agent를 생성할 수 있다.
- flock 생성 중 일부 VM이 실패하면 이미 생성된 VM을 삭제하고 flock registry를
  제거한다.
- flock 삭제는 소속 VM을 병렬로 teardown한다.

### Town Wall

- flock별 append-only coordination log를 추가했다.
- 새 endpoint:
  - `POST /flocks/{flock_id}/post`
  - `GET /flocks/{flock_id}/wall`
  - `GET /flocks/{flock_id}/wall/history`
- `GET /flocks/{flock_id}/wall`은 기존 history를 먼저 내보낸 뒤 새 message를
  SSE stream으로 전달한다.
- Town Wall 파일은 `flocks/{flock_id}/TOWN_WALL.log`에 저장되며, flock 삭제 뒤에도
  audit artifact로 남는다.

### 역할 profile과 system prompt

- `vm.VMConfig`에 `VcpuCount`, `MemSizeMib`를 추가했다. 0 값은 기존 기본값
  2 vCPU, 2048 MiB로 해석한다.
- 기본 역할 mapping:
  - `researcher`: 1 vCPU, 512 MiB, `configs/profiles/researcher/`
  - `reviewer`: 1 vCPU, 512 MiB, `configs/profiles/reviewer/`
  - `worker`: 2 vCPU, 2048 MiB, `configs/profiles/worker/`
  - `orchestrator`: 2 vCPU, 2048 MiB, `configs/profiles/orchestrator/`
  - `builder`: 4 vCPU, 4096 MiB, `configs/profiles/worker/`
- 각 profile directory의 `system.md`를 VM 내부 `/root/.goose-system-prompt`로
  주입한다.
- `goose-agent`는 system prompt를 task prompt 앞에 붙여 역할 지시를 유지한다.

### VM 내부 flock context와 `gtwall`

- flock VM에는 `/root/.ephemera-flock`이 주입된다.
- `goose-agent`는 VM 내부 `POST /townwall/post` endpoint를 제공하고, flock context를
  읽어 host control plane의 `POST /flocks/{id}/post`로 message를 전달한다.
- `scripts/gtwall`은 guest image 안에 `/usr/local/bin/gtwall`로 설치되는 Town Wall
  post helper다.

### 선택적 COW spawn disk

- 새 환경 변수: `EPHEMERA_DISK_MODE=cow`
- 설정 시 새 VM도 golden image full copy 대신 dm-snapshot 기반 sparse COW disk를
  사용한다.
- unset 상태에서는 기존 full clone 동작을 유지한다.

### Diff restore 개선

- diff snapshot restore의 임시 merged memory file은 가능하면 `/dev/shm`에 쓴다.
- `/dev/shm`을 사용할 수 없으면 기존처럼 `{workDir}/tmp`를 사용한다.

## 변경됨

- VM 생성 공통 경로를 `spawnVMInternal`로 추출했다. 일반 `POST /vms`와 flock
  spawner가 같은 cleanup 경로를 공유한다.
- `PrepareVM`은 flock metadata와 역할 system prompt 주입을 지원한다.
- `scripts/build_image.sh`는 guest image에 `gtwall`과 관련 runtime file을 포함한다.

## 검증됨

- `e2e_test.sh`가 50단계에서 58단계로 확장됐다.
- 추가 검증 범위:
  - role profile example 준비
  - 5-agent flock 생성
  - `/vms`의 flock member 반영 확인
  - Town Wall post/history/list
  - flock 삭제와 member VM teardown
- `internal/orchestrator` unit test가 Town Wall history, subscriber delivery,
  concurrent post, flock create/get/delete, agent status update를 검증한다.

# anvil v0.1.0 — IronClaw 통합 E2E 완료

`anvil`은 IronClaw와 ephemera를 결합하는 새 프로젝트다. 이 저장소의 공개
릴리즈 `v0.1.0`, `v0.2.0`, `v0.3.0`, `v0.3.1`은 ephemera runtime 릴리즈이며,
anvil 통합 릴리즈는 `anvil-v0.1.0`, `anvil-v0.2.0`처럼 별도 tag prefix로
분리한다.

## 추가됨

- ephemera daemon `POST /snapshots/gc`: 수동 snapshot retention/GC API.
  - 기본 dry-run mode로 삭제 후보와 보호 사유를 반환한다.
  - `apply: true`일 때만 후보 snapshot directory를 삭제한다.
  - diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- `cmd/anvil-mcp`: IronClaw 연동용 Go stdio MCP 서버.
- `internal/anvilmcp`: 설정 로더, daemon HTTP client, session alias 저장소,
  MCP tool handler.
- `configs/anvil-mcp.yaml.example`: 파일 기반 MCP adapter 설정 예시.
- MCP tool:
  - `anvil_copy_in`
  - `anvil_copy_out`
  - `anvil_create_snapshot`
  - `anvil_delete_snapshot`
  - `anvil_delete_vm`
  - `anvil_get_vm_health`
  - `anvil_list_snapshots`
  - `anvil_restore_snapshot`
  - `anvil_run_task`
  - `anvil_spawn_vm`
  - `anvil_stop_vm`
- `scripts/anvil-mcp-smoke.go`: daemon 없이 MCP tool surface를 검증하는 smoke
  client.
- `docs/architecture/`: 런타임 아키텍처, 서비스 로직, MCP 아키텍처 문서.
- `docs/operations/2026-05-12-ironclaw-integration-check.md`: IronClaw 설치,
  MCP 연결, 실제 IronClaw agent E2E 검증 기록.
- `docs/operations/release-checklist.md`: ephemera runtime 릴리즈와 anvil
  integration 릴리즈를 구분하는 게시 전 확인 절차와 `anvil-v0.1.0` GitHub
  Release historical 본문.
- [docs/operations/security-policy.md](docs/operations/security-policy.md): 운영 공개
  노출, token, local secret, `agent_token` 불변 조건, snapshot metadata scrub 정책을
  구체화한 보안 정책.
- [docs/operations/runbook.md](docs/operations/runbook.md): daemon 빌드/시작, API
  확인, VM cleanup, snapshot GC dry-run/apply 운영 명령.
- [docs/operations/disaster-recovery.md](docs/operations/disaster-recovery.md): daemon
  crash/restart, stale TAP/IP, restore 실패, GC 실패, diff base 누락 대응 playbook.
- [docs/operations/observability.md](docs/operations/observability.md): daemon log,
  `/health`, `/metrics`, `/metrics/vms`, runtime audit, optional trace export 운영
  기준.
- `internal/anvilmcp` multi-tenant foundation:
  - `tenant_id` validation
  - tenant quota decision helper
  - scheduler decision service and runtime host selection primitive
  - `deny_all`/`profile`/`allow_all` egress policy enum
  - daemon tenant/egress contract forwarding for spawn, snapshot, restore
  - `ANVIL_MCP_AUDIT_LOG` 기반 runtime audit JSONL append/read/retention helper
  - daemon failure audit records with sanitized error messages

## 변경됨

- 공식 MCP Go SDK 지원을 위해 최소 Go 버전은 1.25 이상이다.
- 로컬 빌드 산출물 `anvil-daemon`이 git에 들어가지 않도록 ignore 규칙을
  정리했다.
- `ANVIL_API_*`, `ANVIL_PUBLIC_URL`, `ANVIL_DAEMON_*` 환경 변수 alias를
  지원해 IronClaw/anvil 문맥에서 daemon 설정을 읽을 수 있게 했다.
- workspace copy-in/out은 파일 크기 제한, overwrite 정책, text/base64 encoding
  검증, 표준화된 오류 본문을 적용한다.
- daemon VM/snapshot/restore contract는 optional `tenant_id`와 `egress_policy`를
  보존한다.
- `POST /snapshots/{id}/restore` 응답은 더 이상 `agent_token`을 포함하지 않는다.
- `artifacts/goose-agent`는 source hash/version stamp 기반으로 재빌드 여부를
  판단한다.
- guest golden image는 현재 linux-gnu Goose 바이너리의 glibc/runtime 의존성을
  만족하도록 Debian Trixie minbase와 `libvulkan1`을 사용한다.

## 검증됨

- `go test ./...`
- `go build -o /tmp/anvil-daemon ./cmd/goose-daemon`
- `go build -o /tmp/anvil-mcp ./cmd/anvil-mcp`
- `ironclaw mcp test anvil --no-onboard --cli-only`
- 실제 IronClaw agent 기준 anvil MCP tool call E2E

## 알려진 운영 주의사항

- IronClaw 기본 전체 tool inventory와 Gemini tool schema 조합에서는 non-anvil
  tool schema 때문에 agent 실행 전 schema 오류가 발생할 수 있다. anvil 전용 tool
  permission profile을 적용하면 anvil MCP tool call은 정상 검증된다.

# ephemera v0.2.0 — 단일 호스트 기능 완성

ephemera `v0.2.0`은 v0.1.0의 기본 VM 생성/작업 실행 모델에 snapshot, restore,
인증, proxy, profile, COW rootfs, diff snapshot을 추가한 릴리즈다.

## 새 기능

### 안전한 게스트 종료

- 새 guest PID 1인 `micro-init` 추가.
- VM 종료 시 Firecracker가 보내는 `SIGTERM`을 `micro-init`이 받아
  `goose-agent`를 종료하고 `sync` 후 `poweroff(2)`를 호출한다.
- PID 1이 그냥 종료될 때 발생할 수 있는 guest kernel panic을 제거했다.

### VM별 agent 인증

- 각 VM 생성 시 32-byte random Bearer token을 생성한다.
- token은 VM disk의 `/root/.ephemera-agent-token`에 `0600` 권한으로
  주입된다.
- `POST /tasks`와 `POST /stop`은 VM별 token을 요구한다.
- `GET /health`는 readiness probe를 위해 인증 없이 유지한다.
- `POST /vms` 응답에만 `agent_token`을 포함한다. list, snapshot, restore 응답에는
  노출하지 않는다.

### 제어 평면 API 인증

- daemon API에 per-client Bearer token 인증을 추가했다.
- 선호 설정: `EPHEMERA_API_TOKENS=alice:tok1,bob:tok2`
- 기존 단일 token 호환 설정: `EPHEMERA_API_TOKEN=tok`
- 비교는 timing-safe 방식으로 수행한다.
- 요청 로그에 인증된 client 이름을 남긴다.
- `SIGHUP`으로 token list를 hot reload할 수 있다.

### Agent proxy endpoint 추가

- 새 control-plane proxy endpoint:
  - `POST /vms/{vm_id}/tasks`
  - `GET /vms/{vm_id}/health`
  - `POST /vms/{vm_id}/stop`
- 외부 client는 VM private IP로 직접 접근하지 않아도 된다.
- daemon이 VM별 `agent_token`을 내부적으로 주입한다.

### 공개 `agent_url`

- 새 환경 변수: `EPHEMERA_PUBLIC_URL`
- 설정하면 `POST /vms` 응답의 `agent_url`이
  `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` 형태의 proxy path가 된다.
- reverse proxy/TLS 배포에서 VM private IP를 외부에 노출하지 않는다.

### VM별 LLM profile 지원

- `POST /vms`가 optional `profile` field를 받는다.
- 기본 설정:
  - `configs/goose.yaml`
  - `configs/goose-secrets.yaml`
- named profile 설정:
  - `configs/profiles/{profile}/goose.yaml`
  - `configs/profiles/{profile}/goose-secrets.yaml`
- profile 이름에는 path separator를 허용하지 않는다.
- 설정과 secret은 image rebuild 없이 provision time에 주입된다.

### Full snapshot 수명주기

- 새 endpoint:
  - `POST /vms/{vm_id}/snapshot`
  - `GET /snapshots`
  - `POST /snapshots/{id}/restore`
  - `DELETE /snapshots/{id}`
- snapshot은 다음 파일을 저장한다.
  - `memory.bin`
  - `state.bin`
  - `rootfs.ext4`
  - `metadata.json`
- `stop_after` option으로 snapshot 생성 뒤 source VM을 삭제할 수 있다.
- restore 후 새 VM ID와 새 IP를 할당한다.
- snapshot metadata는 original agent token, MAC, TAP, disk path, vsock path를
  보존한다.

### Diff memory snapshot 지원

- dirty page tracking을 사용해 memory diff snapshot을 지원한다.
- 첫 snapshot은 자동으로 `full`이다.
- 같은 VM의 이후 snapshot은 자동으로 `diff`이며 latest full snapshot을
  `base_snapshot_id`로 참조한다.
- 명시적 `type: "full"` 또는 `type: "diff"` 요청도 지원한다.
- diff restore는 base memory와 diff memory를 sparse-aware 방식으로 merge한
  뒤 Firecracker restore에 전달한다.
- diff가 참조 중인 full snapshot 삭제는 `409 Conflict`로 차단한다.

### COW rootfs restore 지원

- restore된 VM은 기본적으로 Linux `dm-snapshot` COW device를 사용한다.
- snapshot `rootfs.ext4`는 read-only base로 공유한다.
- VM별 쓰기는 `/tmp/goose-workspaces/<vm_id>.cow` sparse exception store에
  기록한다.
- restore마다 약 700 MB rootfs copy를 만들던 방식을 제거했다.
- VM 삭제 시 dm device, loop device, bind mount, `.cow` file을 정리한다.
- dm-snapshot setup이 실패하면 기존 bind-mount restore fallback으로
  동작한다.

### Restore 후 IP 재설정

- Firecracker snapshot state에는 TAP device identity와 disk path가 들어 있다.
- restore 시 original TAP name/MAC을 재생성한다.
- guest IP는 vsock `CHANGE_IP` command로 새 IP로 재설정한다.
- 같은 host에서 snapshot state와 runtime IP allocation을 분리한다.

### 통합 테스트 확장

- `e2e_test.sh`를 50단계 통합 테스트로 확장했다.
- 검증 범위:
  - daemon startup
  - VM lifecycle
  - 병렬 VM 작업
  - full snapshot create/list/restore/delete
  - concurrent restore
  - diff snapshot 자동 선택
  - diff sparse size 검증
  - diff restore
  - full/diff dependency protection
  - COW restore resource cleanup
  - agent proxy endpoints
  - `EPHEMERA_PUBLIC_URL` proxy URL behavior
  - graceful daemon shutdown

## 변경됨

- guest boot flow가 `init=/usr/local/sbin/micro-init`을 사용한다.
- provisioner는 VM별 token과 timezone data를 한 번의 mount/unmount cycle에서
  주입한다.
- Firecracker restore path는 vsock device와 original disk path 복원을
  명시적으로 처리한다.
- README를 현재 architecture와 운영 절차에 맞춰 갱신했다.

## 수정됨

- VM 종료 시 PID 1 exit kernel panic 문제를 수정했다.
- restore 후 IP 충돌과 stale private IP 의존 문제를 수정했다.
- VM 생성/restore 실패 경로의 TAP/IP cleanup을 강화했다.
- COW restore 삭제 시 kernel resource 누수를 방지했다.

## 알려진 제약

- 같은 snapshot을 동시에 두 번 restore하는 흐름은 지원하지 않는다.
  snapshot state의 original vsock UDS path 제약 때문이다.
- 서로 다른 snapshot의 concurrent restore는 지원한다.
- diff snapshot은 memory만 diff다. rootfs는 snapshot마다 full copy다.
- diff restore는 임시 merged memory file을 만들 수 있는 disk space가 필요하다.
- control-plane auth 환경 변수를 설정하지 않으면 API 인증이 비활성화된다.
- GitHub tag는 공개되어 있지만 GitHub Release page는 아직 게시하지 않았다.

# ephemera v0.1.0 — 초기 구현

ephemera `v0.1.0`은 초기 proof-of-concept 릴리즈다. 단일 host에서
Firecracker MicroVM을 만들고, 그 안에서 Goose task를 실행하는 기본
경로를 제공했다.

## 포함된 기능

- Go 기반 control-plane daemon: `cmd/goose-daemon`
- Firecracker MicroVM 생성
- Debian Bookworm minbase golden image build
- first-run bootstrap:
  - golden rootfs build
  - Firecracker binary download
  - Linux kernel download
  - guest agent build
- host bridge `goose-br0`
- private network `10.0.1.0/24`
- outbound NAT
- TAP/IP allocation and recycling
- VM별 writable rootfs clone
- Goose config와 secret injection
- guest-side `goose-agent` HTTP server
- API:
  - `POST /vms`
  - `GET /vms`
  - `DELETE /vms/{vm_id}`
  - guest direct `POST /tasks`
  - guest direct `GET /health`
- 초기 e2e smoke test

## v0.1.0 제약

- API 인증이 없었다.
- VM별 agent token이 없었다.
- 외부 client가 guest private IP에 직접 접근해야 했다.
- snapshot/restore가 없었다.
- diff memory snapshot이 없었다.
- COW rootfs restore가 없었다.
- VM별 LLM profile이 없었다.
- graceful guest shutdown이 없어서 종료 시 kernel panic이 발생할 수 있었다.
- public reverse proxy URL model이 없었다.
- MCP/IronClaw adapter가 없었다.

## 사전 요구사항

- Linux host with `/dev/kvm`
- root 또는 sudo 권한
- `curl`
- `debootstrap`
- `e2fsprogs`
- `util-linux`
- Go 1.25 이상

# Upstream ephemera v0.3.2-v0.3.6 release notes

`v0.3.2`-`v0.3.5`는 2026-05-22에 `upstream/main`에서 병합했다.
`v0.3.6`은 2026-05-26에 `v0.3.6` tag에서 병합했다.

# v0.3.6 — Autonomous Multi-Agent Webdev Demo

**Ephemera** v0.3.6 turns the multi-agent flock from a spawn primitive into a demonstrated autonomous collaboration: `webdev_demo.sh` stands up an orchestrator + worker + reviewer team that designs, generates, reviews, and publishes a complete React + Vite site — every file authored inside the VMs, with the host acting only as a passive Town Wall harvester. Getting there required three pieces of in-VM tooling, all included here. The release is additive — no wire format changed and the `/tasks` response shape is unchanged; the only behavior change to an existing endpoint is that `/tasks` output is now cleaner and no longer truncated.

---

## What's New

### `webdev_demo.sh` — autonomous 3-agent flock

- A one-shot operator demo (manual gate, like `observability_demo.sh`): preflight → swap per-role `*.webdev.{md,yaml}` overrides → spawn an `orchestrator + worker + reviewer` flock → background SSE harvester on the Town Wall → one `POST /vms/{orchestrator}/tasks` that drives the whole job → `npm install` + `vite build` → `vite preview` on `:5173` until `Ctrl-C`.
- The orchestrator drives a single Goose session through ~13 tool calls: for each of `src/App.jsx`, `src/main.jsx`, `src/index.css`, `index.html` it calls `gtcall worker-1` to generate the file, `gtwall` to publish it to the Town Wall, and a best-effort `gtcall reviewer-1` review note — then posts `<<<DONE>>>`. All four `<<<FILE:>>>` posts are authored by `orchestrator-1`; there is no host authorship.
- Per-role models: orchestrator `gemini-2.5-flash` (must drive the loop without stalling), worker + reviewer `gemini-2.5-flash-lite` (single-shot). Assumes a paid-tier Gemini key — the free tier's shared 20 RPM cap across all models cannot sustain multi-turn orchestration.
- See [Multi-Agent Webdev Demo](README.md#multi-agent-webdev-demo-v036) in the README.

### In-VM agent-to-agent dispatch — `gtcall`

- New `gtcall <agent_id> "<prompt>"` CLI baked into the golden image alongside `gtwall`. It resolves the peer's `vm_id` from `GET /flocks/{id}` and posts to the control plane's `POST /vms/{vm_id}/tasks` proxy, which injects the peer's agent token — so a calling agent never needs peer credentials.
- The request body is built with `jq -n --arg`, so arbitrary multi-line prompts (newlines, quotes, backticks) dispatch safely, replacing the fragile LLM-authored `curl`/quoting that earlier demo attempts choked on.

### goose-agent `--output-format json` — clean, untruncated task output

- `cmd/goose-agent/main.go` now runs `goose run --output-format json` and returns the assistant text extracted from the envelope (`extractGooseJSONText`): it slices from the first `{` to skip Goose's startup banner, concatenates every assistant `text` block, and falls back to raw stdout if the envelope cannot be parsed.
- This is the only working escape from goose-cli's `streaming_buffer.rs` truncation, which otherwise caps fenced code at 50 lines and spills the overflow into an in-VM `/tmp/goose-*.txt` the host caller cannot reach (neither `--debug` nor `GOOSE_SHOW_FULL_OUTPUT` disables it). The `{ "output": ..., "error": ... }` response shape is unchanged.
- 4 new unit tests cover the happy path, multi-block concatenation, non-JSON fallback, and banner-prefix skip.

### `gtwall` multi-line fix

- `gtwall` now builds its JSON body with `jq -n --arg b "$1" '{body: $b}'` instead of a hand-rolled `sed` escape. The old escape did not handle newlines, so a multi-line body (a whole source file) became invalid JSON and the in-VM agent rejected it with HTTP 400 (curl `-f` → exit 22). Single-line posts are unaffected; multi-line posts now succeed. Latent for the entire v0.3.x line because gtwall had only ever posted single-line status messages.

### Golden image: `curl` + `jq`

- `scripts/build_image.sh` now installs `curl` and `jq` into the Debian minbase golden image (~6 MiB), which `gtcall`/`gtwall` and any future in-VM scripting rely on. `scripts/gtcall` is added to `EnsureGoldenImage`'s staleness input list so editing it triggers a rebuild.

---

## Compatibility

- **Additive.** No control-plane wire format changed. `/tasks` returns the same `{ "output", "error" }` shape (the value is now cleaner). `gtwall`'s single-line behavior is unchanged.
- **Golden-image rebuild.** The in-VM changes (`goose-agent`, `build_image.sh`, `gtwall`, new `gtcall`) trigger one automatic golden-image rebuild on the next daemon start via the mtime staleness check.
- **No new daemon dependency.** `jq` / `curl` are added inside the guest image only; the host already required `jq` since v0.3.5.

---

# v0.3.5 — Observability Trio

**Ephemera** v0.3.5 lays down the observability primitives every later cycle will depend on: a Prometheus `/metrics` endpoint, a per-VM `/stats` snapshot endpoint, and a `log/slog` migration of every daemon-side log call. The release is additive — defaults preserve every v0.3.4 behavior, no wire format changed, no external dependency was added.

---

## What's New

### Prometheus `/metrics` endpoint

- New unauthenticated `GET /metrics` returns the daemon's Prometheus text exposition payload (format 0.0.4). `EPHEMERA_METRICS_REQUIRE_AUTH=true` gates the endpoint behind the same Bearer auth as the rest of the API (defaults follow the standard scrape model — network-level isolation expected).
- Exposition formatter is hand-written (`internal/metrics/registry.go`, `internal/metrics/exposition.go`) so the project keeps its zero-runtime-dependency policy. Supports counters (`Counter`, `CounterVec` with label vectors), gauges (`Gauge`, `GaugeFunc` re-evaluated on every scrape), and histograms with configurable buckets (default `{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}`). All collectors are race-safe via `sync/atomic`.
- Counters: `ephemera_vm_spawn_total{outcome}`, `ephemera_vm_destroy_total{outcome}`, `ephemera_snapshot_create_total{type}`, `ephemera_snapshot_restore_total{outcome}`, `ephemera_flock_spawn_total`, `ephemera_flock_destroy_total`, `ephemera_watchdog_dead_total`, `ephemera_watchdog_heal_total`, `ephemera_sighup_reload_total`, `ephemera_cp_token_propagated_total{outcome}`.
- Gauges (GaugeFunc — re-read on every scrape): `ephemera_vm_count`, `ephemera_flock_count`, `ephemera_snapshot_count`, `ephemera_api_clients_count`.
- Histograms: `ephemera_vm_spawn_duration_seconds`, `ephemera_snapshot_restore_duration_seconds`, `ephemera_watchdog_probe_duration_seconds` (per-probe bucket set tuned to `{0.05, 0.1, 0.25, 0.5, 1, 2, 5}`).
- The mux is now two-level: `externalMux.Handle("/metrics", …)` is registered before the catch-all `externalMux.Handle("/", authMiddleware(internalMux))`, so ServeMux's most-specific-pattern rule routes scrape traffic past the bearer check by default. Watchdog observations flow into the registry through `Watchdog.OnDead` / `OnHeal` / `OnProbeDuration` callbacks — `internal/orchestrator/` deliberately does not import `internal/metrics/`.

### Structured logging via `log/slog`

- `go.mod` bumped from `go 1.18` to `go 1.21` to access `log/slog` from the standard library. CI's `go-version-file: go.mod` clause picks the new toolchain automatically; no `.github/workflows/` edits needed.
- Every `log.Printf` / `log.Println` / `log.Fatalf` call in `cmd/goose-daemon/`, `internal/storage/`, `internal/orchestrator/`, and `internal/network/` (138 sites total) was migrated to `slog.Info` / `slog.Warn` / `slog.Error`. Messages were rewritten as short lowercase phrases (`"recovery: disk missing"`, `"snapshot: copying disk"`); inline format args became structured fields (`vm_id`, `flock_id`, `agent_id`, `snapshot_id`, `err`, …).
- Two new env knobs: `EPHEMERA_LOG_FORMAT=text|json` (default `text`) selects TextHandler or JSONHandler; `EPHEMERA_LOG_LEVEL=debug|info|warn|error` (default `warn`) sets the minimum emitted level. The default preserves the previous `log.Printf` tone — every lifecycle event is emitted at warn-or-higher so operators see it without configuration.
- `cmd/goose-agent/` (in-VM) deliberately retains its existing `log.Printf` output this cycle — touching the agent would trigger a golden-image rebuild, which is deferred to v0.4.3 (the streaming-tasks cycle that already touches in-VM code).

### Per-VM `/stats` endpoint

- New `GET /vms/{vm_id}/stats` returns `{vm_id, cpu_percent, mem_used_mib, mem_total_mib, uptime_seconds, network_rx_bytes, network_tx_bytes, agent_busy}` as a point-in-time snapshot. Authentication uses the same control-plane Bearer token as every other VM endpoint.
- `cpu_percent` is sampled over 100 ms from `/proc/<firecracker_pid>/stat` (utime + stime). The Firecracker PID is resolved by tracing the socket path through `/proc/net/unix` (path → inode) and `/proc/<pid>/fd` (inode → process) — `firecracker-go-sdk` v1.0.0 does not expose the child PID directly. The resolved PID is cached on `runningVM.fcPID` (atomic) and re-validated on every stats request via `/proc/<pid>/comm`.
- `mem_used_mib` from `VmRSS:` in `/proc/<pid>/status`; `mem_total_mib` from the per-VM spawn sizing (mirrored on `runningVM.memSizeMib`).
- `network_rx_bytes` / `network_tx_bytes` from `/sys/class/net/<tap>/statistics/{tx,rx}_bytes`, swapped to the VM's perspective (host TAP `rx_bytes` = VM `tx_bytes`).
- `uptime_seconds` from a new `runningVM.spawnedAt` (UTC) that mirrors the already-persisted `VMState.CreatedAt`. Recovered VMs inherit their original spawn time across daemon restarts.
- `agent_busy` from a 1 s `GET /health` against the in-VM agent's existing endpoint, parsed as `{status: "idle"|"busy"}`. Direct HTTP (not via the proxy) so the response body can be JSON-decoded.
- `GET /vms?stats=true` returns the full list with embedded `stats` blocks for bulk dashboards. Per-VM stats failures (PID not resolvable, `/proc` race, agent unreachable) degrade to zero values and emit a slog `Warn`; the response never partial-errors.

### Tests

- `internal/metrics/registry_test.go` adds `TestRegistry_Counter_Increments`, `TestRegistry_CounterVec_LabelsSeparate`, `TestRegistry_Gauge_SetReturnsLastValue`, `TestRegistry_GaugeFunc_CallsFunctionEachWrite`, `TestRegistry_Histogram_BucketsCumulative`, `TestRegistry_WriteTo_FormatMatchesSpec` (golden text output), `TestRegistry_WriteTo_EscapesLabelValues`, `TestRegistry_Concurrent_NoRace`, `TestRegistry_DuplicateName_Panics`, `TestRegistry_InvalidName_Panics`, `TestFormatFloat_IntegralVsFractional`.
- `cmd/goose-daemon/metrics_handler_test.go` covers content-type, method enforcement, default-unauth, counter updates, and gauge func observation.
- `cmd/goose-daemon/stats_handler_test.go` covers `/proc/<pid>/stat` parsing, `VmRSS` parsing, TAP statistics, the `/health` probe (busy / timeout), the 404 and method-not-allowed handler paths, and `?stats=true` partial-failure behaviour.
- `cmd/goose-daemon/main_test.go` validates the slog format / level handler-selection logic.
- `e2e_test.sh` step 61 (`/metrics` endpoint format) and step 62 (`/vms/{vm_id}/stats` schema + `?stats=true` inline form) live between the existing rotation cleanup (`58c.vii`) and the rotation-daemon shutdown (`60`).

---

## Upgrade Notes

- **Go 1.21 toolchain required for local builds.** CI's `setup-go@v5` step picks this up from `go.mod` automatically, but contributors building locally must have `go1.21+` installed (`go version` to check). No source changes outside of the new `log/slog` imports rely on 1.21-specific stdlib.
- **Wire compatibility is unchanged.** Every pre-existing endpoint behaves identically; the only additions are `GET /metrics` and `GET /vms/{vm_id}/stats` plus the optional `?stats=true` query on `GET /vms`.
- **Log format change is opt-in.** The default `EPHEMERA_LOG_FORMAT=text` matches the previous `log.Printf` style closely enough that existing log scrapers / journals see the same kind of output (with structured fields appended). Set `EPHEMERA_LOG_FORMAT=json` only when an aggregator expects JSON. `EPHEMERA_LOG_LEVEL` defaults to `warn` so existing baseline noise stays the same; lower it to `info` or `debug` for troubleshooting.
- **No new env var is required for default operation.** `EPHEMERA_METRICS_REQUIRE_AUTH`, `EPHEMERA_LOG_FORMAT`, `EPHEMERA_LOG_LEVEL` are all optional with sensible defaults.

---

## Changed / new files

- `go.mod` — `go 1.21` (bump). `go.sum` regenerated by `go mod tidy`.
- `internal/metrics/registry.go` — new. Registry + Counter/CounterVec/Gauge/GaugeFunc/Histogram.
- `internal/metrics/exposition.go` — new. Prometheus text format 0.0.4 encoder.
- `internal/metrics/registry_test.go` — new. 11 tests.
- `cmd/goose-daemon/metrics_handler.go` — new. `daemonMetrics` bundle + `handleMetrics`.
- `cmd/goose-daemon/metrics_handler_test.go` — new.
- `cmd/goose-daemon/stats_handler.go` — new. `VMStats` + `VMInfoWithStats` + `handleVMStats` + `?stats=true` branch helper.
- `cmd/goose-daemon/stats_handler_test.go` — new.
- `cmd/goose-daemon/stats_collector.go` — new. PID resolution, `/proc` parsing, TAP stats, agent-busy probe.
- `cmd/goose-daemon/main_test.go` — new. slog handler tests.
- `cmd/goose-daemon/main.go` — `initSlog`, full `log` → `slog` migration, `fatal` helper.
- `cmd/goose-daemon/config.go` — `metricsRequireAuth` var; `log` → `slog`.
- `cmd/goose-daemon/api.go` — `ControlPlane.metrics`, `runningVM` adds `memSizeMib` / `spawnedAt` / `fcPID`, two-mux split, `/stats` routing, `?stats=true` branch, spawn / destroy / snapshot / restore / SIGHUP / CP-token counter wiring, `log` → `slog`.
- `cmd/goose-daemon/orchestrator_api.go` — `flockSpawn` / `flockDestroy` counters, `log` → `slog`.
- `cmd/goose-daemon/recovery.go` — `log` → `slog`, `runningVM.spawnedAt` / `memSizeMib` initialized from `VMState.CreatedAt` / `MemSizeMib`.
- `internal/orchestrator/watchdog.go` — `OnDead` / `OnHeal` / `OnProbeDuration` callback fields; per-probe timer wraps `checkOne`. `log` → `slog`.
- `internal/storage/provisioner.go` — `log` → `slog` (29 sites).
- `internal/network/manager.go` — `log` → `slog` (6 sites).
- `README.md` — `EPHEMERA_METRICS_REQUIRE_AUTH` / `EPHEMERA_LOG_FORMAT` / `EPHEMERA_LOG_LEVEL` rows; Per-VM Stats and Metrics endpoint subsections under API Reference; new Observability section under Resilience; Known Limitations row for external metrics retention; Key Features row for the observability trio.
- `CONTRIBUTING.md` — three new "extra care" items: metrics counter discipline, slog message convention, per-VM stats PID resolution.

---

# v0.3.4 — Operational Convenience

**Ephemera** v0.3.4 closes the last three operator-touch corners after v0.3.3: rotating the control-plane bearer no longer requires per-VM restarts, the watchdog cadence is overridable from the environment, and an opt-in self-heal returns recovered agents to `ready` for operators who prefer liveness over the sticky-dead default. The release is additive — defaults preserve every v0.3.3 behavior, no wire format changed, and every existing env var keeps working.

---

## What's New

### CP token hot rotation via vsock

- New `EPHEMERA_API_TOKENS_FILE` env (in `cmd/goose-daemon/config.go`) names a file path that `loadAPIClients` reads on every call. Operators edit the file, send `SIGHUP`, and the daemon picks up the new contents. Env-only deployments still work as before — file precedence is `EPHEMERA_API_TOKENS_FILE` → `EPHEMERA_API_TOKENS` → `EPHEMERA_API_TOKEN`. `parseAPIClients` is factored out of the legacy parse loop and accepts both comma- and newline-separated entries.
- `ReloadClients` (`cmd/goose-daemon/api.go`) now fans the new `apiClients[0].Token` out to every running VM that has a vsock UDS path. Snapshot taken under `cp.mu.RLock()`, dispatch in parallel goroutines, per-VM 4 s budget (20 × 200 ms — see hot-fix below for why this matches `ReconfigureGuestIP`), per-VM failure logged but never propagated. A final log line summarizes ok/total counts.
- `internal/vm/machine.go` extracts a generic `vsockSendCommand` from the existing `vsockSendChangeIP`; `ReconfigureGuestIP` becomes a thin wrapper with its original 20 × 200 ms retry budget preserved. New sibling `SetGuestCPToken` uses the same 20 × 200 ms budget. `vsockSendOnce` also stat-preflights the UDS path so a dead Firecracker surfaces as "vsock UDS missing" rather than the misleading "connection refused".
- `cmd/goose-agent/main.go`'s `handleVsockConn` now dispatches on the leading verb. The new `SET_CP_TOKEN <token>` command writes `/root/.ephemera-cp-token` via tmp-and-rename (mode 0600), so a concurrent `loadCPToken` reader never observes a partial write. `/townwall/post` continues to call `loadCPToken` per request, so the next forwarder call sees the rotated bearer without any caching change.

### Env-tunable watchdog timings

- `cmd/goose-daemon/config.go` exposes three new ints: `EPHEMERA_WATCHDOG_INTERVAL_SEC` (default 5), `EPHEMERA_WATCHDOG_TIMEOUT_SEC` (default 1), `EPHEMERA_WATCHDOG_THRESHOLD` (default 3). All three reuse the existing `envInt` helper.
- `Watchdog.Configure(interval, httpTimeout, threshold, autoHeal)` (`internal/orchestrator/watchdog.go`) is the new public entry point. The struct fields remain unexported so external callers cannot bypass the `interval >= httpTimeout` clamp logic; in-package tests continue to set fields directly.
- The startup log line now reflects the resolved values: `Watchdog started (interval=5s, timeout=1s, threshold=3, auto_heal=false)`.

### Opt-in watchdog self-heal

- New `envBool` helper (case-insensitive, accepts `1/true/yes/on` and `0/false/no/off`) backs `EPHEMERA_WATCHDOG_AUTO_HEAL` (default `false`).
- When `autoHeal=true`, `Watchdog.onSuccess` clears the `deadMarked` bit on the first successful probe of a previously-dead VM, flips the agent's status back to `ready`, persists the change via `Flock.Persist`, and posts an `<orchestrator> <id> recovered - auto-healed to ready` notice to the flock's Town Wall.
- Default (`false`) preserves the v0.3.1+ sticky-dead contract — `TestWatchdog_MarksDeadAfterThreshold` and `TestWatchdog_PersistsDeadStatus` are unchanged and still pass.

### Tests

- `TestWatchdog_Configure_AppliesTunables` and `TestWatchdog_AutoHeal_ResetsDeadMark` in `internal/orchestrator/watchdog_test.go` cover the new behaviors. The auto-heal test asserts the Town Wall recovery notice, the in-memory `Status=ready`, and the on-disk metadata round-trip.
- `e2e_test.sh` step 58c (sub-steps 58c.i–58c.vii) exercises the full rotation flow against a real Firecracker VM: spawn TOKENS_FILE daemon with v1 → spawn flock → SIGHUP after editing file to v2 → assert post-rotation `/townwall/post` 200, v1 operator bearer 401, both posts in Town Wall, and the `SIGHUP: CP token propagated` log line. Pre-flight cleanup also removes a leftover `/tmp/ephemera-tokens.txt`. Step 60 is renamed to the rotation-daemon shutdown. 58c.iii also `ls -la`s the vsock UDS before SIGHUP so operators reading a future failure can tell whether the file exists, and 58c.iii's assertion parses the `propagated to N/M` count line rather than just substring-grepping for the prefix (a silent "0/1" used to pass).

### Hot-fix: SDK signal forwarding (post-release)

The initial v0.3.4 cycle merged with a critical bug that the e2e gate did not catch on the first run: `firecracker-go-sdk` v1.0.0's default `ForwardSignals` list **includes `SIGHUP`**, and the SDK installs a goroutine that forwards every received signal to its Firecracker child via `cmd.Process.Signal(sig)`. The daemon uses `SIGHUP` for its own token reload, so sending `kill -HUP <daemon_pid>` triggered the SDK's forwarder, killed every running Firecracker (exit status 156), and the vsock fan-out then failed with `connection refused` on a UDS whose listener had just died. The bug existed latently since v0.2.0 (SDK upgrade), but v0.3.4 was the first release whose post-SIGHUP code actually depended on Firecracker still being alive.

Fix in `internal/vm/machine.go`: define a package-level `forwardSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGABRT}` (SIGHUP deliberately omitted) and set `firecracker.Config.ForwardSignals: forwardSignals` in both `StartMachine` and `RestoreMachine`. Shutdown signals are still forwarded so `Ctrl-C` / `systemctl stop` propagate cleanly; only the daemon's own reload signal is intercepted before reaching Firecracker.

Two safety nets also landed alongside: `SetGuestCPToken`'s retry budget moved from 3 × 300 ms to 20 × 200 ms to match `ReconfigureGuestIP`, and `vsockSendOnce` now `os.Stat()`s the UDS path before dialing so a dead Firecracker is reported as `vsock UDS missing at <path>` instead of the more ambiguous `connection refused`.

**Operational impact for prior versions**: if you ran `kill -HUP` against any v0.2.0–v0.3.3 daemon (e.g. for the documented "token hot reload"), every Firecracker was silently killed at that moment. The v0.3.3 effect was undetectable because no post-SIGHUP code touched the children; nonetheless any VMs in flight became unreachable. Upgrading to the hot-fixed v0.3.4 is the recommended remediation.

---

## Upgrade Notes

- **Wire compatibility**: unchanged. All existing API responses, env vars, and on-disk files remain valid.
- **No default behavior change**: every new env var is default-preserving (`5s / 1s / 3` watchdog timings unchanged; `auto_heal=false` matches sticky-dead). Existing deployments observe no behavior difference until they opt in.
- **CP token hot rotation requires two prerequisites**: (1) the daemon must source tokens from `EPHEMERA_API_TOKENS_FILE`, since env vars are fixed at exec time and SIGHUP cannot change them; (2) the VMs must run a v0.3.4+ `goose-agent` (the `SET_CP_TOKEN` vsock handler did not exist before). Mixed-version fleets keep working — older VMs log a propagation failure and operators can fall back to `POST /flocks/{id}/agents/{agent_id}/restart` for those.
- **New vsock command**: `SET_CP_TOKEN <token>` joins `CHANGE_IP <cidr_ip> <gateway>` on the existing AF_VSOCK port 1234 channel. No new port, no new transport.

---

## Changed / new files

- `cmd/goose-daemon/config.go` — `EPHEMERA_API_TOKENS_FILE` source, `parseAPIClients` helper, four watchdog env vars, `envBool` helper
- `cmd/goose-daemon/api.go` — `ReloadClients` token fan-out (`propagateCPTokenToVMs`), `Configure` call after `NewWatchdog`, startup log line carries resolved tunable values
- `cmd/goose-agent/main.go` — `handleVsockConn` dispatch on verb (`CHANGE_IP` / `SET_CP_TOKEN`), `writeCPTokenAtomic`
- `internal/vm/machine.go` — `vsockSendCommand` / `vsockSendOnce` (with `os.Stat` preflight) extracted from `vsockSendChangeIP`; `ReconfigureGuestIP` becomes a thin wrapper; new `SetGuestCPToken` (20 × 200 ms retry); new `forwardSignals` package var explicitly omitting `SIGHUP`; both `StartMachine` and `RestoreMachine` set `firecracker.Config.ForwardSignals`
- `internal/orchestrator/watchdog.go` — `Watchdog.autoHeal`, `Watchdog.Configure`, opt-in `onSuccess` heal path
- `internal/orchestrator/watchdog_test.go` — `TestWatchdog_Configure_AppliesTunables`, `TestWatchdog_AutoHeal_ResetsDeadMark`
- `e2e_test.sh` — step 58c (sub-steps i–vii) for the rotation flow; step 60 renamed; pre-flight removes `/tmp/ephemera-tokens.txt`
- `README.md`, `CONTRIBUTING.md`, `RELEASE_NOTES.md`

---

# v0.3.3 — Operational Polish

**Ephemera** v0.3.3 closes the four rough edges left by the v0.3.1 / v0.3.2 hardening pass without changing any wire formats. Watchdog-detected `dead` markings now survive daemon restart and cold-restart; a single failed agent can be replaced in place without recreating the whole flock; the in-VM `/townwall/post` forwarder authenticates against an auth-on control plane without any per-VM operator setup; and the end-to-end test gained a real-LLM round-trip scenario so the `gtwall` chain stops being a code-only contract. The release is a strict superset of v0.3.2 — every existing API response, env var, and on-disk file remains valid.

---

## What's New

### Watchdog dead-status persistence

- `Flock.Persist(workDir)` is the new single entry point for flock-metadata writes (`internal/orchestrator/flock.go`). Holds a per-flock `writeMu` around `ToMetadata` + `SaveFlockMetadata`'s tmp+rename, so concurrent writers (createFlock, watchdog, recovery, per-agent restart) cannot tear each other's writes
- `Watchdog.onFailure` (`internal/orchestrator/watchdog.go`) now calls `Persist` immediately after flipping `Status` to `"dead"` — the change lands on disk before the next probe cycle
- `cmd/goose-daemon/recovery.go` persists both transitions: `markFlockAgentDead` writes the dead state when a VM can't be cold-restarted, and the success path persists `Status=ready` so a future `LoadFromDisk` does not resurrect a previously-dead agent
- `cmd/goose-daemon/orchestrator_api.go`'s `createFlock` swapped its raw `SaveFlockMetadata` call for `flock.Persist(cp.workDir)`; the raw API now carries a comment forbidding direct daemon-side calls

### Per-agent restart endpoint

- `POST /flocks/{flock_id}/agents/{agent_id}/restart` (handled by `restartAgent` in `orchestrator_api.go`) tears down one flock member's VM and respawns it with the same `agent_id`, role, and `agent_token` — callers that cached the token keep working without re-auth
- `spawnVMOptions.AgentToken` is the new optional field that drives token reuse: empty means generate fresh (standalone spawn path), non-empty means reuse verbatim (restart path)
- `Flock.UpdateAgentVM(agentID, newVMID, newAgentURL)` swaps the VM identity in place and resets `Status` to `ready`
- `Watchdog.ForgetVM(vmID)` clears cached `failCount` / `deadMarked` for a destroyed vmID so a recycled ID does not inherit stale state
- On spawn failure the agent is left `Status=dead` and persisted, so external callers see the truth without polling

### Auto-injected control-plane token

- `ControlPlane.controlPlaneTokenForVM()` returns `apiClients[0].Token` under `clientsMu` (so SIGHUP-driven `ReloadClients` is safe). Empty when auth is disabled — in that mode in-VM forwarders call CP unauthenticated, preserving the v0.3.2 dev/test flow
- `spawnVMForFlock` and `restartAgent` thread the token through `spawnVMOptions.ControlPlaneToken` → `VMPrepareOptions.ControlPlaneToken` → `injectVMFiles`, which writes it to `/root/.ephemera-cp-token` (mode 0600). Standalone `POST /vms` deliberately does NOT inject (no `/townwall/post` use case)
- `cmd/goose-agent/main.go` gains `loadCPToken()` — prefers the new file, falls back to the legacy `EPHEMERA_CONTROL_PLANE_TOKEN` env var so older golden images keep working

### Real-LLM Town Wall round-trip e2e

- `e2e_test.sh` step 59 spawns a researcher under the auth-on daemon, sends `/tasks` with an explicit `gtwall` instruction, and verifies `ROUNDTRIP_OK` reaches Town Wall via the system-prompt → Goose CLI → `gtwall` → in-VM forwarder → CP chain
- Skip-by-default: when none of `GOOGLE_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is in the environment the step reports `ok "Skipped"` so CI without LLM credentials stays green
- Secret handling: profile files are backed up to `/tmp/researcher-{secrets,goose}.bak` before sed-injection and restored via `trap EXIT`; pre-flight cleanup also restores them in case a previous run was SIGKILLed mid-step

### Refactor for testability

- `internal/storage/provisioner.go`'s file-writing logic moved into `injectVMFiles(mntDir, opts)`, decoupling it from the loop-mount lifecycle. New mount-free unit tests cover the v0.3.3 `ControlPlaneToken` injection without needing root or `/dev/kvm`

---

## Upgrade Notes

- **Wire compatibility**: unchanged. All existing API responses, env vars, and on-disk files remain valid.
- **New on-disk file inside each flock VM**: `/root/.ephemera-cp-token` (mode 0600). Only present when `EPHEMERA_API_TOKENS` is set on the host.
- **`flocks/<id>/metadata.json` is written more often** (every dead transition + every per-agent restart). Atomic tmp+rename keeps the file always parseable; expect a handful of additional writes per long-running flock, not a steady-state burden.
- **CP token rotation is still a manual operation**: `apiClients[0]` is captured at spawn time. After SIGHUP-driven token rotation, run `POST /flocks/{id}/agents/{agent_id}/restart` (or DELETE + recreate the flock) on affected VMs. See README Known Limitations.

---

## Changed / new files

- `internal/orchestrator/flock.go` — `Flock.writeMu`, `Flock.Persist`, `Flock.UpdateAgentVM`
- `internal/orchestrator/watchdog.go` — `Watchdog.onFailure` persists; new `Watchdog.ForgetVM`
- `internal/orchestrator/persistence.go` — concurrency note updated; `SaveFlockMetadata` documented as raw primitive (callers go through `Flock.Persist`)
- `internal/orchestrator/persistence_test.go` — `TestFlock_Persist_StatusRoundTrip`, `TestFlock_Persist_ConcurrentSafe`
- `internal/orchestrator/watchdog_test.go` — `TestWatchdog_PersistsDeadStatus`, `TestWatchdog_ForgetVM_ClearsState`
- `internal/orchestrator/flock_test.go` — `TestFlock_UpdateAgentVM`
- `cmd/goose-daemon/recovery.go` — both status transitions persisted
- `cmd/goose-daemon/orchestrator_api.go` — `restartAgent`, `agents/{id}/restart` route, `controlPlaneTokenForVM` plumbed into `spawnVMForFlock`
- `cmd/goose-daemon/api.go` — `controlPlaneTokenForVM`, `spawnVMOptions.AgentToken` + `.ControlPlaneToken`, route log entry for restart
- `internal/storage/provisioner.go` — `VMPrepareOptions.ControlPlaneToken`, `injectVMFiles` refactor, `/root/.ephemera-cp-token` (mode 0600)
- `internal/storage/provisioner_test.go` — `TestInjectVMFiles_ControlPlaneToken`, `TestInjectVMFiles_EmptyControlPlaneToken_SkipsFile`
- `cmd/goose-agent/main.go` — `cpTokenPath` constant, `loadCPToken`, `/townwall/post` reads via `loadCPToken`
- `e2e_test.sh` — steps 57i, 57j, 58b, 58b.i, 59a–59e, 60 (+ pre-flight LLM-secret restore)
- `README.md`, `CONTRIBUTING.md`, `RELEASE_NOTES.md`

---

# v0.3.2 — Live VM Cold-Restart

**Ephemera** v0.3.2 closes the biggest operational gap left by v0.3.1: when the daemon stops and restarts, the VMs that were running come back up automatically with the same identity. Flock metadata recovery (v0.3.1) is no longer read-mostly — every recovered flock's member VMs are cold-restarted from their existing rootfs clones, with the same `vm_id`, IP, TAP device, MAC, agent token, and `agent_url` preserved. Memory state is not snapshotted; in-flight `/tasks` work is lost, but the system surface, network identity, and audit history are all stable across the restart. v0.3.x API responses remain fully backward compatible.

---

## What's New

### Live VM cold-restart

- Every successful `POST /vms` (and every flock member) writes `vms/<vm_id>/state.json` atomically (tmp + rename) with the VM's network identity, agent token, disk path, profile, and flock association
- On daemon startup, after flock metadata is rescanned, `ControlPlane.RecoverVMs` iterates the persisted state:
  1. **Orphan cleanup** — any leftover Firecracker process bound to the persisted API socket is sent SIGTERM, then SIGKILL after a 1.5 s grace; stale socket / log FIFO / vsock UDS files are removed (`internal/storage/orphan.go`)
  2. **Network re-reservation** — the original TAP name and MAC are recreated, the original IP is re-marked in-use in the pool (`network.Manager.ReclaimAllocation`); if the IP is no longer free, the entry is dropped and the agent is marked `dead`
  3. **Cold boot** — `vm.StartMachine` is called against the same rootfs clone with the persisted vCPU / memory sizing; `goose-agent` is waited for on `/health` up to 60 s
  4. **Flock association** — for flock VMs, the agent status is flipped back to `ready` on success or `dead` (with an `<orchestrator>` Town Wall notice) on failure
- `cp.vms` is repopulated, so `/tasks`, `/health`, `/stop`, and proxy endpoints all work against recovered VMs immediately after the daemon comes back up
- `destroyVM` deletes the per-VM state file before tearing down the Firecracker process, so a crash mid-teardown does not resurrect a VM that the operator was already removing

### Graceful shutdown feeds cold-restart

`ControlPlane.DestroyAll` (deferred from `main` when the daemon receives SIGTERM/SIGINT) was rewritten to support cold-restart. Previously it called `destroyVM` per VM, which removed `state.json` and the rootfs ext4 — so the next start had nothing to recover. v0.3.2 splits the two paths:

- **Graceful daemon shutdown** stops every Firecracker process via `StopVMM`, removes per-VM transient files (API socket, log FIFO, vsock UDS), and releases TAP/IP back to the pool — but **preserves `state.json` and the rootfs ext4**. Cold-restart picks them up on the next start.
- **Explicit `DELETE /vms/{id}`** still routes through `destroyVM` and does a full cleanup (state.json removed, rootfs removed); the VM is gone permanently.
- **SIGKILL / crash** leaves everything on disk as-is; cold-restart's orphan cleanup handles any leftover Firecracker process and then proceeds identically to the graceful path.
- **COW-restored and snapshot-restored VMs** are torn down fully in `DestroyAll` (their `state.json` is also removed) because v0.3.2 does not recover them; leaving the dm-snapshot device or bind mount would leak kernel resources.

### What is preserved vs. lost

| Preserved | Lost |
|-----------|------|
| `vm_id`, `guest_ip`, `tap_device`, `mac_addr` | In-flight `/tasks` work (memory is not snapshotted) |
| `agent_token`, `agent_url`, `profile` | Goose conversation context (in-VM, in-memory) |
| Disk contents (rootfs clone is reused, not recreated) | Watchdog `status=dead` markings (revert to `ready` until next probe) |
| Flock membership, Town Wall history, `seq` numbering | COW-mode VMs and snapshot-restored VMs (out of scope for v0.3.2) |

### Out of scope for v0.3.2

- **COW-mode VMs** (`EPHEMERA_DISK_MODE=cow`) are skipped during recovery (logged on startup). dm-snapshot orphan cleanup is deferred to a later release; workaround is to re-spawn the agent.
- **Snapshot-restored VMs** (`POST /snapshots/{id}/restore`) are not auto-recovered — restore from the snapshot again after the daemon comes back.

### New / changed files

- `internal/storage/vm_state.go` — `VMState` struct, `SaveVMState` / `LoadVMState` / `DeleteVMState` / `ListVMState` (atomic tmp+rename; `schema_version: 1`)
- `internal/storage/orphan.go` — `KillStaleFirecracker(socketPath)` + `RemoveStaleVMArtifacts`
- `internal/network/manager.go` — `ReclaimAllocation(tapName, guestIP, macAddr)`: re-reserve the exact original allocation (vs. `AllocateForRestore` which picks any free IP for vsock-reconfig snapshot restore)
- `cmd/goose-daemon/recovery.go` — `ControlPlane.RecoverVMs()` and `markFlockAgentDead`
- `cmd/goose-daemon/api.go` — `spawnVMInternal` writes state at the end of every spawn; `destroyVM` deletes state at the start of teardown; `NewControlPlane` calls `RecoverVMs` after `flockMgr.LoadFromDisk`; **`DestroyAll` rewritten** to stop Firecracker without dropping state.json or rootfs ext4 (see "Graceful shutdown feeds cold-restart")
- New e2e sub-steps `57e` (cold-restart preserves VM IDs) and `57f` (recovered VM `/health` responds); existing 57e/57f renumbered to 57g/57h. Pre-flight cleans `vms/vm-*` alongside `flocks/flock-*` and `snapshots/snap-*`.
- `vms/` and `flocks/` added to `.gitignore` (both were undocumented gaps)

---

## Upgrade Notes

- **Fully backward compatible** with v0.3.x clients — `state.json` is new on-disk surface only; no API response shape changes
- **First boot after upgrade**: existing VMs spawned by a v0.3.x daemon do *not* have `state.json` on disk, so they will not be cold-restarted. The new behavior takes effect for VMs spawned by v0.3.2 or later
- **Callers that rely on at-most-once `/tasks` semantics** should idempotency-key their task IDs or re-poll for completion across a daemon restart — memory is not preserved, so any in-flight task is lost
- **`EPHEMERA_DISK_MODE=cow` users**: keep using COW for spawn-time disk savings, but a daemon restart will not recover those VMs in v0.3.2. Plain disk mode (default) is fully covered.

---

# v0.3.1 — Goosetown Operational Hardening

**Ephemera** v0.3.1 hardens the Goosetown layer introduced in v0.3.0 for long-running workloads. Three operational risks present in v0.3.0 are addressed: VM death goes from silent to actively surfaced, flock state survives daemon restarts, and Town Wall subscribers can now detect message gaps. The release also folds in Phase 5 verification follow-up that completes the in-VM `gtwall` chain validation, plus a `CONTRIBUTING.md`. All v0.3.0 API responses remain backward compatible — only additive fields and one new internal behavior surface (watchdog).

---

## What's New

### VM health watchdog

- A background goroutine polls every flock-member VM's `/health` endpoint every 5 seconds (1 s per-probe HTTP timeout)
- After 3 consecutive failures the agent's status transitions to `"dead"` in the flock registry and a notice is auto-posted to the Town Wall as `<orchestrator>`: `worker-1 unresponsive after 3 health probes - marked dead`
- A revived VM is **not** auto-marked back to `ready` — operators clear dead state by deleting the flock or the individual VM
- Standalone (non-flock) VMs are not watched (locator returns `ok=false`)
- Watchdog is stopped before the HTTP server during shutdown to prevent it from observing a half-torn-down `cp.vms`

### Flock state persistence

- `POST /flocks` writes `flocks/<flock-id>/metadata.json` atomically (tmp + rename) before returning the response
- `DELETE /flocks/{id}` removes the metadata file (the `TOWN_WALL.log` is kept as an audit artifact)
- Daemon startup scans `flocks/*/metadata.json` and re-registers every flock in memory; the active Town Wall log is reopened in append mode so active history (and `seq` numbering) continues across restarts
- Recovered flocks are read-mostly: their VM IDs no longer correspond to live Firecracker processes (those died with the previous daemon), so `/tasks` against them will fail; `/post`, `/wall`, `/wall/history`, and `DELETE` continue to work
- Schema versioned (`schema_version: 1`) for future migrations
- Live VM auto-restart is deferred to v0.3.2

### Monotonic SSE sequence numbers

- Every Town Wall `Message` now carries `seq` (uint64) starting at 1 per flock and incrementing on each post
- `seq` is preserved across daemon restarts by initializing from `len(History())` when the wall is reopened
- Subscribers detecting gaps in `seq` can fall back to `/flocks/{id}/wall/history` to recover the missing range
- The on-disk log format is unchanged (`[ts] <agent> body`); seq is wire-format-only and is reassigned 1..N from line order on each `History` read

### Phase 5 verification follow-up

These items extend the v0.3.0 work to cover what the original e2e couldn't:

- `agent_tokens` in `POST /flocks` response — additive map of `agent_id → bearer token`, lets callers authenticate to in-VM agent endpoints (matches the existing `/vms` spawn pattern)
- Per-role `agent_id` indexing — `roles=["orchestrator","researcher","researcher","worker","reviewer"]` now produces `orchestrator-1, researcher-1, researcher-2, worker-1, reviewer-1` (matches the README example; previous global indexing produced `researcher-2, researcher-3, worker-4, reviewer-5`)
- New e2e step **54b** validates the in-VM `/townwall/post` chain end-to-end (the same path `gtwall` takes), including escaped quote/backslash JSON round-trip and 401 on unauthenticated probe
- `/townwall/post` is documented as intentionally **not** proxied through the control plane — external callers should `POST /flocks/{id}/post` directly

### Daemon and tooling hardening

- mtime-based auto-rebuild for `goose-agent`, `micro-init`, and the golden image — editing in-VM Go code or `build_image.sh` no longer requires a manual `rm artifacts/...`
- Daemon `log.Fatalf` on `ListenAndServe` error so a `bind: address already in use` doesn't leave a silent zombie process
- e2e pre-flight kills any stale `ephemera-daemon` from a prior interrupted run, cleans `flocks/flock-*` workdir entries, and waits up to 600 s on cold-start (handles golden-image rebuild)
- Daemons in the e2e are started with `EPHEMERA_API_ADDR=0.0.0.0:3000` so the bridge gateway IP `10.0.1.1` accepts in-VM `/townwall/post` forwards

### Documentation

- New `CONTRIBUTING.md` focused on this project's specific gotchas (host vs in-VM binary classes, gitignored profile yaml, root/KVM e2e requirements, snapshot lifecycle, golden image bake cost, in-VM auth)
- README **Resilience** section documents the watchdog, persistence scope, and seq gap-detection pattern
- README env-var table notes `EPHEMERA_API_ADDR=0.0.0.0:3000` is required for flock /townwall/post forwarding

---

## Upgrade Notes

- **Fully backward compatible** with v0.3.0 clients — `seq`, `agent_tokens`, and the new `metadata.json` are all additive; existing endpoints unchanged
- **No image rebuild required** unless the daemon detects stale in-VM binaries — the new staleness check handles that automatically on the next start
- **First boot after upgrade**: existing `flocks/<id>/TOWN_WALL.log` files are preserved; flocks created before v0.3.1 (which have no `metadata.json`) are *not* recovered automatically. Going forward, every flock is recoverable
- **Production with `EPHEMERA_API_TOKENS` set**: the in-VM `/townwall/post` forwarder still relies on `EPHEMERA_CONTROL_PLANE_TOKEN` being set inside the VM; auto-injection is on the v0.3.2 roadmap

---

# v0.3.0 — Goosetown: Multi-Agent Orchestration

**Ephemera** now runs heterogeneous groups of role-specialized MicroVMs as a single addressable unit ("flocks"). One `POST /flocks` call spawns an orchestrator, researchers, workers, and reviewers in parallel, each sized to its role and sharing an append-only **Town Wall** log for coordination. Every v0.2.0 endpoint behaves exactly as before — the new surface is purely additive, so existing clients continue to work without changes.

---

## What's New

### Multi-Agent Flocks

- `POST /flocks` — spawn N role-specialized VMs in one call (max 20 per flock); returns flock metadata, agent records, `townwall_url`, and `post_url`
- `GET /flocks` — list all live flocks
- `GET /flocks/{id}` — describe a flock and its agents
- `DELETE /flocks/{id}` — tear down every member VM **in parallel** (~1 s for a 5-agent flock instead of ~5 s sequential) and unregister the flock
- Partial-spawn safety: if any VM in the flock fails to come up, all previously spawned VMs are torn down and the flock is removed before the error is returned

### Town Wall — Per-Flock Append-Only Log

- `POST /flocks/{id}/post` — append a message (`{agent_id, body}`) to the flock's shared log
- `GET /flocks/{id}/wall` — **SSE stream** that emits active-log history once, then forwards every new post live
- `GET /flocks/{id}/wall/history` — one-shot dump of the log as JSON
- Backed by `flocks/<flock-id>/TOWN_WALL.log` on disk (kept as an audit artifact after `DELETE`)
- Mutex-serialized writes with a buffered subscriber fan-out — slow subscribers are dropped from the current message rather than blocking the writer

### Per-Profile vCPU / Memory Sizing

- `vm.VMConfig` now accepts `VcpuCount` and `MemSizeMib` (zero falls back to the legacy 2 vCPU / 2048 MiB defaults)
- Built-in role profiles map to canonical sizing:

  | Role | vCPU | Memory (MiB) | Profile dir |
  |------|------|--------------|-------------|
  | `researcher` | 1 | 512 | `researcher/` |
  | `reviewer` | 1 | 512 | `reviewer/` |
  | `worker` | 2 | 2048 | `worker/` |
  | `orchestrator` | 2 | 2048 | `orchestrator/` |
  | `builder` | 4 | 4096 | `worker/` |

- Unknown profile names continue to spawn at the default sizing and resolve `configs/profiles/{name}/`, so the v0.2.0 profile contract is preserved

### Role System Prompts

- Each profile directory can ship a `system.md` that ships role instructions
- The control plane injects `system.md` content into the VM as `/root/.goose-system-prompt` at provision time
- In-VM `goose-agent` prepends it to every `/tasks` prompt as `[SYSTEM INSTRUCTIONS]\n...\n\n[USER TASK]\n...` so the role stays in-character even when the orchestrator dispatches plain user prompts
- Four shipped role prompts: `researcher` (read-only exploration), `worker` (implementation), `reviewer` (adversarial review), `orchestrator` (delegation + synthesis)

### In-VM Flock Context + `gtwall` CLI

- For flock members, `PrepareVM` writes `/root/.ephemera-flock` (`FLOCK_ID`, `AGENT_ID`) so the in-VM agent knows its identity
- New `goose-agent` endpoint: `POST /townwall/post` reads the flock context and forwards the message to the host control plane's `POST /flocks/{id}/post`
- New shell wrapper `scripts/gtwall` (installed at `/usr/local/bin/gtwall` in the golden image): one-liner posting from anywhere in the VM — `gtwall "Claiming src/styles/theme.css"`

### Optional COW Spawn Disks (`EPHEMERA_DISK_MODE=cow`)

- Setting `EPHEMERA_DISK_MODE=cow` provisions each new VM via `dm-snapshot` over the golden image instead of a 700 MiB full copy
- Initial extra disk usage drops from ~700 MiB to ~0 MiB per VM; writes accumulate in a sparse `.cow` exception store
- Default behavior is unchanged when the variable is unset, so it doubles as a safe rollback path
- Reuses the existing `SetupDMSnapshot` / `TeardownDMSnapshot` plumbing introduced in v0.2.0 for restore

### Faster Diff Snapshot Restore

- Memory merge during diff restore now writes the temporary 2 GiB `merged.bin` to `/dev/shm` (tmpfs) when available, avoiding disk I/O on the hot path
- Falls back to `{workDir}/tmp` when `/dev/shm` is not a writable directory (e.g. minimal containers)

### Spawning Internals — `spawnVMInternal`

- Extracted the spawn pipeline (network alloc → disk clone → config inject → Firecracker start → register → wait) into a shared `spawnVMInternal` helper
- Both the public `POST /vms` handler and the new flock spawner reuse it, so any future spawn change applies to both paths uniformly
- All cleanup paths consistently undo every resource they allocated before returning

### Configuration

- New env var: `EPHEMERA_DISK_MODE` — set to `cow` to enable the dm-snapshot-backed spawn path described above
- Inside a flock VM: `EPHEMERA_CONTROL_PLANE` (optional, default `http://10.0.1.1:3000`) — overrides the control plane URL used by `/townwall/post` forwarding for testing
- Inside a flock VM: `EPHEMERA_CONTROL_PLANE_TOKEN` (optional) — bearer token attached to the forwarded Town Wall post when the control plane runs with auth enabled

### Testing

- `e2e_test.sh` grows from 50 to **58 steps** with a new Goosetown block:
  - 51 prep `configs/profiles/*/{goose.yaml,goose-secrets.yaml}` from `.example` files
  - 52 spawn a 5-agent flock (orchestrator / researcher × 2 / worker / reviewer) and validate the returned IDs, URLs, and agent count
  - 53 confirm `/vms` reflects all flock members
  - 54 post to the Town Wall through the control plane
  - 55 assert `/flocks/{id}/wall/history` returns ≥ 2 entries
  - 56 verify `GET /flocks` lists the new flock
  - 57 `DELETE /flocks/{id}` and assert every VM is torn down and the flock unregisters
  - 58 daemon graceful shutdown (renumbered from former 50)
- New unit tests in `internal/orchestrator/`: Town Wall post/history, subscriber delivery, concurrent posting, line parsing, flock create/get/delete, agent status update, lock-safe JSON marshaling

### Documentation

- README adds an "Architecture" entry per flock endpoint, a "Flock API" section with curl examples (Spawn / Post / SSE Stream / History / List / Describe / Destroy), a built-in role profile table, and an updated Testing section showing the actual passing e2e output for steps 51–58

---

## Upgrade Notes

- **Backward compatible**: `POST /vms`, `POST /vms/{id}/snapshot`, `POST /snapshots/{id}/restore`, and the agent proxy endpoints behave exactly as in v0.2.0
- **Golden image**: rebuild to ship `gtwall` and `iproute2` inside the VM — `rm artifacts/golden-image.ext4 && sudo ./ephemera-daemon`. Existing v0.2.0 images keep working for non-flock VMs
- **Role profiles**: built-in role names ship `*.yaml.example` files only. Before spawning flocks, copy them and fill in API keys: `cp configs/profiles/<role>/goose.yaml{.example,}` and same for `goose-secrets.yaml`
- **COW spawn**: opt in with `EPHEMERA_DISK_MODE=cow`. Unset (default) keeps the v0.2.0 full-clone behavior — useful as a single-flag rollback if any platform-specific dm-snapshot issue surfaces

---

# v0.2.0 — Single-Host Feature Complete

**Ephemera** completes the single-host feature set. Every limitation noted in v0.1.0 is resolved: guests shut down gracefully, agent authentication is enforced, tokens reload without a restart, and VMs can be snapshotted and restored in seconds. A new control plane proxy makes goose-agent accessible from external clients without direct access to the private VM subnet.

---

## What's New

### Graceful Guest Shutdown

- `micro-init` (PID 1) now traps `SIGTERM` and calls `sync` + `poweroff(2)` — the guest powers off cleanly with no kernel panic
- `reboot=k` kernel argument lets Firecracker exit cleanly (exit code 0) on guest power-off

### Per-VM Agent Authentication

- Control plane generates a 32-byte random Bearer token per VM at spawn time
- Token written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk
- `POST /tasks` and `POST /stop` require `Authorization: Bearer <agent_token>`
- `GET /health` remains unauthenticated (used by the control plane health poller)
- Token returned once in `POST /vms` and `POST /snapshots/{id}/restore` responses; preserved across snapshot/restore cycles

### Per-VM LLM Profiles

- Each VM spawn can specify a named profile via `{"profile": "anthropic"}` in the request body
- Profiles stored under `configs/profiles/<name>/goose.yaml` + `goose-secrets.yaml`
- Enables running multiple VMs with different providers, models, or API keys simultaneously
- Omitting `profile` uses the default `configs/goose.yaml`

### API Token Hot Reload (SIGHUP)

- `kill -HUP <daemon_pid>` re-reads `EPHEMERA_API_TOKENS` / `EPHEMERA_API_TOKEN` and swaps the in-memory client list
- Running VMs are not affected; no downtime required to add, rotate, or revoke tokens

### MicroVM Snapshot & Restore

- `POST /vms/{vm_id}/snapshot` — freezes VM memory state and copies rootfs to `snapshots/<id>/`
- `POST /snapshots/{id}/restore` — resumes a VM from snapshot in ~5 s (vs ~60 s cold boot); preserves agent token
- `DELETE /vms/{vm_id}` with `stop_after: true` destroys the source VM immediately after snapshot
- `GET /snapshots` — list all stored snapshots
- `DELETE /snapshots/{id}` — delete snapshot files

### Diff Snapshots (Multi-Checkpoint)

- First snapshot of a VM → **Full** (2 GB `memory.bin`)
- Subsequent snapshots → **Diff** (sparse `memory.bin`, dirty pages only — typically 1–400 MB)
- Type auto-detected; `"type": "full"` or `"type": "diff"` can override
- `base_snapshot_id` links each Diff to its Full base
- Deleting a Full that has referencing Diffs returns `409 Conflict`
- Restore from Diff merges base + diff memory in-memory; temp file cleaned up immediately after Firecracker opens it

### Post-Restore IP Reconfiguration (vsock)

- Each restored VM receives a fresh IP from the pool; the original IP is freed
- Guest network stack updated in-place via vsock (`CHANGE_IP <cidr> <gw>`) — no reboot required
- `goose-agent` binds `AF_VSOCK` port 1234 inside the VM for reconfiguration commands

### Concurrent Restore

- Multiple VMs can be restored simultaneously from different snapshots
- Each restore gets its own disk COW device; bind-mount stacking ensures Firecracker opens the correct device

### COW Rootfs Restore (dm-snapshot)

- Restore no longer copies the 700 MB rootfs — instead creates a Linux dm-snapshot COW device
- Base disk (`rootfs.ext4`) is read-only and shared; per-VM writes go to a sparse exception store (~0 MB initial)
- On VM delete: dm device removed, loop devices detached, exception store deleted automatically
- Falls back to full copy if dm-snapshot is unavailable

### Control Plane Agent Proxy

- Three new proxy endpoints route agent traffic through the control plane — no direct VM subnet access needed:
  - `POST /vms/{vm_id}/tasks` → goose-agent `/tasks`
  - `GET  /vms/{vm_id}/health` → goose-agent `/health`
  - `POST /vms/{vm_id}/stop`  → goose-agent `/stop`
- Callers authenticate with the control plane Bearer token only; agent token is injected internally
- `EPHEMERA_PUBLIC_URL` env var: when set, `agent_url` in VM responses points to the proxy path (`{url}/vms/{vm_id}`) instead of the private VM IP

### End-to-End Test Suite

- `e2e_test.sh` — 50-step integration test covering the full VM and snapshot lifecycle
- Validates: VM spawn, parallel tasks, snapshot/restore, concurrent restore, diff snapshots, COW rootfs, agent proxy, `EPHEMERA_PUBLIC_URL` behavior

---

# v0.1.0 — Initial Implementation

**Ephemera** is an enterprise control plane for running ephemeral AI agents inside Firecracker MicroVMs. This first release delivers a fully working end-to-end implementation: from spinning up an isolated KVM-backed VM to executing Goose AI tasks via HTTP and cleaning up all host resources on teardown.

---

## What's New

### Control Plane API

- `POST /vms` — spawn a MicroVM; blocks until goose-agent is ready (~60 s), returns `vm_id`, `guest_ip`, `agent_url`
- `GET /vms` — list running VMs
- `DELETE /vms/{vm_id}` — stop VM and release all host resources (TAP device, disk clone, IP)

### goose-agent (in-VM HTTP agent)

- Runs inside each MicroVM as PID 1 via `micro-init`
- `POST /tasks {"prompt":"..."}` — execute a Goose task, return output
- `GET /health` — `idle` | `busy` status
- `POST /stop` — graceful agent shutdown

### Self-bootstrapping

- Firecracker v1.15.1 downloaded and SHA256-verified automatically on first run
- Linux 6.1.155 kernel downloaded automatically
- Golden image (Debian Bookworm minbase + Goose + goose-agent) built via `debootstrap` on first run
- `goose-agent` binary compiled from source before image build

### Guest OS — Minimal Debian Bookworm

- Replaced initial Ubuntu 22.04 skeleton with Debian Bookworm `--variant=minbase`
- No SSH, no init daemon, no DHCP client — `micro-init` mounts virtual filesystems and execs goose-agent directly as PID 1
- Network configured via Linux kernel `ip=` boot parameter (live before any user-space process starts)
- Host timezone mirrored into VM via `/etc/localtime` symlink injection at provisioning time

### Runtime Config Injection

- `configs/goose.yaml` (provider, model, extensions) and `configs/goose-secrets.yaml` (API keys) injected into each VM's disk at provisioning time — no image rebuild needed to change provider or model
- Supports Google, Anthropic, OpenAI, Ollama, and all other Goose-compatible providers
- Keyring-free operation (`GOOSE_DISABLE_KEYRING: true`) for headless VM environments

### Network & Storage

- Linux bridge `goose-br0` with iptables MASQUERADE for VM-to-internet connectivity
- IP pool (10.0.1.2–254) sorted and recycled across VM lifecycle
- TAP device IDs recycled via free-list after VM destruction
- All host resources guaranteed to be released on teardown

### Security

- Per-client Bearer token authentication (`EPHEMERA_API_TOKENS=alice:token1,bob:token2`)
- Timing-safe token comparison via `crypto/subtle.ConstantTimeCompare`
- Each request logged with authenticated client name for audit trail
- Control plane binds to `127.0.0.1:3000` by default (localhost only)
- TLS supported via Caddy or Nginx reverse proxy (see README)
- Single-token fallback (`EPHEMERA_API_TOKEN`) for backward compatibility

---

## Prerequisites

- Ubuntu 22.04 or 24.04 host (bare metal or nested virtualization)
- `/dev/kvm` accessible
- Go 1.18+
- `sudo apt install -y curl debootstrap e2fsprogs util-linux`
