# anvil 릴리즈 운영 체크리스트

## 목적

이 체크리스트는 같은 저장소에서 관리되는 두 종류의 공개 릴리즈를 구분한다.
`ephemera` runtime 릴리즈는 Firecracker MicroVM 기반 실행 엔진 자체의 source
snapshot을 공개하는 절차이고, `anvil` integration 릴리즈는 IronClaw가
`cmd/anvil-mcp`를 통해 ephemera runtime을 호출하는 통합 표면을 공개하는 절차다.

`ephemera`는 이미 릴리즈된 기반 runtime 이름이다. `anvil`은 IronClaw와
ephemera를 결합하는 통합 프로젝트 이름이다. 릴리즈 제목, tag prefix, GitHub
Release 본문에서 두 이름을 섞어 쓰지 않는다.

## 릴리즈 종류

| 종류 | Tag 예시 | 공개 대상 | 기준 내용 |
|---|---|---|---|
| ephemera runtime | `v0.2.0` | Firecracker MicroVM runtime source snapshot | `cmd/goose-daemon`, `cmd/goose-agent`, `cmd/micro-init`, `internal/storage`, `internal/network`, `internal/vm` |
| anvil integration | `anvil-v0.1.0` | IronClaw 통합 MCP adapter와 운영 계약 | `cmd/anvil-mcp`, `internal/anvilmcp`, workspace copy-in/out, snapshot MCP tools, daemon env alias, IronClaw E2E 검증 |
| anvil runtime foundation | `anvil-v0.2.0` | scheduler, network policy, observability, Goosetown MCP foundation | `cmd/anvil-scheduler`, `internal/anvilmcp`, daemon tenant/audit/metrics API, profile egress, optional trace export, Goosetown flock/Town Wall MCP tools |
| anvil core service parity(구 명칭, non-latest) | `anvil-v0.4.0` | upstream ephemera `v0.4.0`-`v0.7.0` runtime/operator 표면(storage/recovery, auth/audit, COW, flock lifecycle, streaming/depth/watchdog, snapshot-restore recovery, operator Web UI/`/config/*`, runtime MCP Gateway, end-user installer/transcript) + IronClaw `anvil_*` MCP 경계 유지 | parity matrix `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`; token redaction·tenant/egress·scheduler·audit·IronClaw MCP surface separation 적응; `cmd/anvil-mcp` 불변, `EPHEMERA_MCP_*` gateway는 `ANVIL_MCP_*` adapter를 대체하지 않음. 개발 내역으로 보존, 현행 아님 — 아래 `anvil-v0.7.0` 참고 |
| anvil ephemera-aligned current(Latest) | `anvil-v0.7.0` | 위 parity 편입에 더해 post-`anvil-v0.4.0` backlog batch와 open-gate closure까지 포함하는 가장 완전한 상태. `anvil-v0.7.0`부터 anvil 버전은 upstream ephemera 버전을 그대로 따른다(2026-07-06 정렬) | `RELEASE_NOTES.md` `# anvil-v0.7.0` 절; tag target `2f367dd`(태그 시점 main) |

## 게시 전 확인 명령

```bash
git fetch --tags origin
git tag --list 'v*' --sort=version:refname
git tag --list 'anvil-v*' --sort=version:refname
```

## Upstream sync 확인

anvil은 `steve-seungeui/ephemera` fork network를 유지한다. ephemera runtime을 새
baseline으로 올리는 release라면 먼저 upstream 상태를 확인하고 별도 sync PR로
반영한다.

```bash
git remote -v
git fetch upstream main
git ls-remote --tags upstream
```

기존 local `v*` tag와 upstream tag가 충돌할 수 있으므로 `git fetch --tags --force`로
tag를 덮어쓰지 않는다. upstream sync 절차는
[upstream-sync-policy.md](upstream-sync-policy.md)를 따른다.

### GitHub Release 상태 확인

```bash
gh release view anvil-v0.2.0 --json tagName,targetCommitish,publishedAt,url,isDraft,isPrerelease
```

게시된 GitHub Release의 body를 갱신하는 작업도
외부 상태를 바꾸는 작업이다. 따라서 `gh release edit`을 실행하기 전에 tag name,
target commit, release body source를 먼저 보여 주고 사용자의 명시적 승인을
받아야 한다.

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n e2e_test.sh
bash -n scripts/anvil-mcp-e2e.sh
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/vm-workload-e2e.sh
bash -n scripts/anvil-cross-host-wall-e2e.sh
```

`anvil-v0.1.0` Release 본문은 이미 게시된 첫 통합 release의 historical body다.
`anvil-v0.2.0` Release는 scheduler, profile egress, `/metrics/vms`, optional
trace export, ephemera `v0.3.1` Goosetown hardening, Goosetown MCP tool surface를
포함하는 두 번째 integration release로 게시됐다. KVM host가 준비된 release
candidate에서는 58단계 `sudo bash e2e_test.sh`와 `scripts/anvil-mcp-e2e.sh flock`을
함께 확인한다.

script-only workload runner를 포함하는 release candidate에서는 KVM host에서
`sudo -n bash scripts/vm-workload-e2e.sh`도 확인한다. 이 검증은
`/vms/{vm_id}/workloads/run`을 사용하므로 LLM provider key 없이 nginx 설치/기동,
Go HTTP server 기동, VM 내부 benchmark, host-to-VM probe를 확인한다. 결과 artifact는
`summary.json`, `nginx-run.json`, `go-http-run.json`, `nginx.log`, `go-http.log`,
`bench.txt`, `host-bench.txt`를 포함해야 하며 provider token, API key,
control-plane token, agent token을 포함하지 않아야 한다.

scheduler production automation을 포함하는 release candidate에서는 systemd host에서
다음 검증을 수행한다. `--verify`는 host에 `curl`, `python3`가 있어야 실행된다.

```bash
go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
curl http://127.0.0.1:3010/metrics
```

`--verify`는 `GET /health`, `PUT/GET /hosts`, `POST /schedule/spawn`,
`POST /schedule/flock`, `GET /placements`, `GET /control-loop/status`,
`DELETE /hosts/{name}` cleanup을
확인한다. smoke host는 `smoke_only: true`로 등록되어 `PreferredHosts`에 명시된 smoke
요청에서만 선택되고, `PreferredHosts` 없는 추가 `/schedule/spawn`에서는 제외되는지
검증된다. `POST /schedule/flock`은 dry-run planner response 계약만 확인하며 VM을
생성하지 않는다. scheduler는 기본적으로 `127.0.0.1:3010`에 bind하며, 외부 노출은
private network 또는 TLS 종료 reverse proxy policy 뒤에서만 수행한다.

Scheduler release candidate는 `/control-loop/status`와 `/metrics`를 모두 smoke로
검증해야 한다. `/metrics`에는 `agent_token`, daemon raw body, host endpoint가 나오면
안 된다.

### Routed flock members create release-candidate gate

members-only routed flock create slice를 포함하는 release candidate에서는 기본
`anvil_spawn_flock`이 계속 scheduler-aware single-host path이며,
`anvil_create_routed_flock_members`는 opt-in experimental tool임을 확인한다.
활성화 조건은 `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent
`scheduler_state_path`, runtime host inventory다. host inventory는
`scheduler_hosts_file` / `ANVIL_MCP_SCHEDULER_HOSTS_FILE` 또는 이미 persistent state에
저장된 runtime host list로 제공되어야 한다.

필수 검증:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
go test ./cmd/goose-daemon -count=1
go test ./... -count=1
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/goose-daemon
git diff --check
```

KVM, daemon, LLM API key 등 flock smoke 전제 조건이 있는 host에서는 다음을 추가로
실행해 default single-host flock 동작이 바뀌지 않았는지 확인한다.

```bash
scripts/anvil-mcp-e2e.sh flock
```

### 현재 upstream runtime baseline

anvil main runtime baseline은 upstream ephemera `v0.7.0`까지 병합·적응한 runtime을
포함한다. `v0.3.2`-`v0.3.6`은 이전 release가 채택한 baseline이고, `v0.4.0`-`v0.4.5`는
v0.4 sync, `v0.5.0`-`v0.5.5`는 v0.5 operator sync, `v0.6.0`-`v0.6.4`는 v0.6 MCP gateway
sync, `v0.7.0`은 v0.7 parity sync에서 병합·적응해 full KVM gate로 검증했다 — upstream
parity scope(`v0.4.0`-`v0.7.0`) 코드 편입이 완료됐다. anvil runtime/operator baseline
supports upstream ephemera v0.7.0 with anvil adaptations for token redaction,
tenant/egress, scheduler, audit, and IronClaw MCP surface separation. 전 태그별 parity
matrix는 [`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](../analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)에
있다. upstream `main`과 최신 upstream tag는 `v0.7.0`까지 진행되어 있다. 새 anvil release
후보가 이 baseline을 포함한다면 release 본문에는 upstream runtime 변경과 anvil product
변경을 분리해서 적는다.

- upstream `v0.3.2`: live VM cold-restart, `vms/<vm_id>/state.json`, orphan cleanup,
  기존 TAP/IP/MAC 재예약, graceful daemon shutdown 시 rootfs/state 보존.
- upstream `v0.3.3`: watchdog dead-status persistence, single-agent restart
  endpoint, flock VM 내부 `/root/.ephemera-cp-token` auto-injection, real-LLM Town
  Wall round-trip e2e.
- upstream `v0.3.4`: `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out,
  watchdog env tunables/auto-heal, Firecracker `ForwardSignals`에서 `SIGHUP` 제외.
- upstream `v0.3.5`: Prometheus `/metrics`, per-VM `/vms/{vm_id}/stats`,
  `GET /vms?stats=true`, `log/slog`, `EPHEMERA_LOG_FORMAT`, `EPHEMERA_LOG_LEVEL`,
  `EPHEMERA_METRICS_REQUIRE_AUTH`, observability demo/Grafana assets.
- upstream `v0.3.6`: autonomous webdev demo, in-VM `gtcall`, multi-line-safe
  `gtwall`, Goose `--output-format json` assistant text extraction, golden image
  `curl`/`jq` dependency.
- upstream `v0.4.0`-`v0.4.3`: memory auto-snapshot(opt-in `EPHEMERA_AUTOSNAPSHOT`),
  diff/COW rootfs, client identity + `GET /audit` + per-token TTL + `ephemera-ctl`,
  COW spawn probe/fallback(`EPHEMERA_DISK_MODE=cow` 명시적 opt-in), dynamic flock
  membership/pause/resume/`max_agents`/Town Wall filter·rotation.
- upstream `v0.4.4`: streaming `POST /vms/{id}/tasks?stream=1`(NDJSON, buffered
  기본 계약 유지), nested task depth guard `EPHEMERA_MAX_TASK_DEPTH`/`508`,
  read-only `GET /watchdog/status`, flock broadcast(daemon API/`ephemera-ctl` only,
  `anvil_*` MCP tool 미노출), goose-agent slog.
- upstream `v0.4.5`: snapshot-restore auto-recovery(daemon restart 시 source
  snapshot에서 re-restore). anvil은 live·persisted restored VM이 참조하는 source
  snapshot `DELETE`를 `409`로 막아 upstream e2e 46c의 `200` orphan과 divergent하다.
- upstream `v0.5.0`: operator Web UI(`/ui/`, embedded `cmd/goose-daemon/uidist/`) +
  `/config/profiles` + multi-turn goose session + graceful VM delete. Web UI는
  runtime/operator surface(IronClaw MCP 아님), `/ui/`만 auth 밖·data API는 bearer 뒤,
  `/config/profiles`는 `goose-secrets.yaml` 비노출.
- upstream `v0.5.1`-`v0.5.5`: `/config/providers`·`/config/clients`(secret 비노출),
  `system.md` 편집(64 KiB), profile guard(in-use `409`/default 예약/traversal 거부),
  sizing preset + per-VM `VcpuCount`/`MemSizeMib`, `SystemAuthor`, restore wait 60s.
- upstream `v0.6.0`: runtime MCP Gateway(`internal/mcpgateway`, daemon `/config/mcp*`,
  `configs/mcp/*.example`, Web UI MCP console). runtime/operator surface(IronClaw MCP
  아님, `cmd/anvil-mcp` 대체 아님). source IP로 caller profile 판정(unknown `403`),
  backend credential host-side only(VM엔 gateway URL만), `audit/mcp.jsonl` metadata-only,
  `/config/mcp*`는 bearer 뒤, profile policy는 widen 불가.
- upstream `v0.6.1`/`v0.6.2`/`v0.6.4`(upstream `v0.6.3` 없음): `EPHEMERA_NET_ANTISPOOF`
  기본 on, per-(VM,server) rate limit(`EPHEMERA_MCP_RATE`/`BURST`, 기본 `0`=unlimited),
  resources/prompts policy·rate 공유, `GET /config/mcp/servers`는 `has_credential`만,
  stdio backend(`nobody`/`/var/lib/ephemera/mcp-stdio` scratch, `credential_env`,
  child env 재구성, process-group reap).
- upstream `v0.7.0`: end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/
  `ephemera.service.in`)와 release workflow(`scripts/build_release.sh`), conversation
  transcript restore, upstream hardening reconcile. installer는 runtime/operator surface,
  systemd는 canonical `ephemera`(alias wrapper 없음). transcript는 daemon proxy
  `GET /vms/{id}/sessions/{name}/transcript`(bearer), agent export read-only(model call
  없음), 응답 `{turns:[{role,text}]}` auth-free. `uninstall.sh`는 ephemera-scoped `/tmp`
  scratch(`/tmp/goose-workspaces` 등, stale no-op `/tmp/goose-rootfs` 포함)를 root-gated·
  prefix-anchored로 정리한다(의도된 cleanup).
- anvil transcript-safety guard 4종: bearer 없으면 `401`, payload는 provider key/CP
  token/`agent_token` sentinel-free, cache-hit는 agent spawn 없이 serve, export argv는
  `session export -n {name} --format json`이며 run-token 거부.
- anvil backport reconciliation: 사전 backport 3종(kernel SHA atomic temp+rename,
  `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe)이 v0.7.0 reconcile에서
  single definition으로 남았고(anvil stricter, net Go diff doc-comment-only) 기존 anvil
  adaptation(agent-stamp skip, restore-over-`meta.DiskPath`, `DisableKeepAlives`)은
  rollback 없음. release build integrity: `build_release.sh`가 kernel/firecracker를
  `main.go` pin과 `sha256sum -c`로 검증(FULL-tarball supply-chain gap 차단).
- anvil sizing 결정: default VM sizing `1` vCPU / `1024` MiB(v0.5.3 이전 2/2048,
  KVM 근거로 승인). flock member spawn의 per-profile sizing override 미존중 gap은
  follow-up으로 기록한다.
- anvil keep-alive divergence(`64ec57c`): proxy agent client가 request마다 fresh
  dial(`DisableKeepAlives`)한다. `v0.5.x` `gracefulAgentStop`이 드러낸 v0.2.0-since
  pooled-client 결함(guest IP 재활용 시 stale connection → restored VM `/tasks`
  hang/`502`)을 막는 upstream pooling divergence다. 2026-07-06 결정으로 anvil-side에서
  유지하고 upstream 제안(기여)은 하지 않는다.
- anvil adaptation: `agent_token`과 control-plane token이 MCP output, audit, metrics,
  trace, replay fixture, release artifact에 노출되지 않도록 수정 또는 검증한 내용.
  `ephemera_*` metric namespace와 `EPHEMERA_*` env는 runtime compatibility로
  설명하고 anvil product rename으로 처리하지 않는다.

v0.4 sync Phase 1 KVM gate 결과와 전제:

- CI-safe gate all green: `go build ./cmd/{goose-daemon,anvil-mcp,anvil-scheduler}`,
  `go test ./... -count=1`(EXIT=0). broadcast 비노출·buffered 기본 계약·depth `508`
  guard test 포함.
- 실제 KVM host `sudo bash e2e_test.sh`: `316✓ / 0✗`("All test steps passed").
  step 59 real-LLM smoke만 provider key 부재로 skip.
- KVM gate 전제: working directory에 gitignore된 로컬 operator 파일
  `configs/goose.yaml`, `configs/goose-secrets.yaml`이 있어야 한다. 없으면
  `POST /vms`가 config injection 단계에서 `500`으로 실패한다.

v0.5 sync Phase 2 KVM gate — 필수 스크립트(재발 방지):

KVM runtime/operator 변경 release 후보는 아래 스크립트를 **모두** 실행한다. Phase 1은
`e2e_test.sh`만 실행하고 `anvil-mcp-e2e.sh`(3개 모드)와 `vm-workload-e2e.sh`를 누락했다.
이 목록은 그 누락이 재발하지 않도록 게시 전 필수 항목으로 고정한다.

```bash
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
sudo -n bash scripts/vm-workload-e2e.sh
sudo bash scripts/anvil-cross-host-wall-e2e.sh       # cross-host town wall relay (real member VM + stub home)
bash install.sh --help && bash uninstall.sh --help   # installer help, no system mutation (v0.7.0)
sudo bash scripts/build_release.sh v0.7.0             # FULL+SLIM tarball + .sha256 (dist/ gitignored, v0.7.0)
```

Cross-host shared Town Wall를 건드리는 release 후보는 KVM host에서
`sudo bash scripts/anvil-cross-host-wall-e2e.sh`도 확인한다. 이 검증은 real 멤버
`anvil-daemon` + 하나의 real flock member VM을 띄우고, guest `gtwall` post가
guest → in-VM agent → member daemon → relay hop → stub home으로 흐르는지 확인한다.
home은 loopback 127.0.0.1:3100 python3 stub이 recorder 역할만 하며, relay가 home에
넘긴 request가 `{"agent_id":"researcher-1","body":"ROUNDTRIP_OK"}` body와
`Authorization: Bearer rt-e2e` header를 갖고 `agent_token`(필드명·값 모두)을 노출하지
않는지 assert한다. LLM provider key 없이 통과한다(post에 model call 없음). 멤버
daemon은 guest가 bridge gateway IP(`http://10.0.1.1:3000`)로 forward하므로
`EPHEMERA_API_ADDR=0.0.0.0:3000`으로 bind한다(host는 `127.0.0.1:3000`으로 접근).
**full two-daemon cross-host integration(두 번째 host의 real home daemon)은 단일 host
bridge/IP 충돌 때문에 MANUAL multi-host 검증**이며 이 single-host gate 범위 밖이다.

Phase 2 결과:

- CI-safe gate all green: `git diff --check`, targeted test group, web build 재현
  가능(`cmd/goose-daemon/uidist/` drift 없음), `go test ./... -count=1`(EXIT=0),
  `go build ./cmd/{goose-daemon,anvil-mcp,anvil-scheduler}`.
- KVM: `sudo bash e2e_test.sh` `316✓ / 0✗` ×3(step 59 real-LLM smoke만 provider-key
  부재로 skip). `anvil-mcp-e2e.sh` lifecycle PASS·flock PASS, `vm-workload-e2e.sh`
  PASS.
- `anvil-mcp-e2e.sh semantic`은 key-free 구간(workspace copy-in/out, task proxy,
  cleanup)이 모두 `200`이고 마지막 LLM-echo substep만 로컬 Google API key가 invalid해
  실패했다. provider-key 의존 실패이며 코드 결함이 아니다(계획대로 기록).

v0.6 sync Phase 3 KVM gate — gateway steps:

- `e2e_test.sh`는 이제 MCP gateway step `84`-`89`를 포함한다. gateway를 건드리는 release
  후보는 이 step들이 green인지 확인한다(real tool call / real stdio tool call step은
  provider key가 필요할 수 있다).
- Phase 3 결과: CI-safe gate all green(`git diff --check`, targeted group, web build
  drift 없음, `go test ./... -count=1` EXIT=0, 3 builds). KVM `sudo bash e2e_test.sh`
  `334✓ / 0✗`("All test steps passed") — gateway step 84-89 최초 실행 green, provider-key
  skip 3건(LLM smoke, real tool call, real stdio tool call). `anvil-mcp-e2e.sh`
  lifecycle PASS·flock PASS(runtime gateway live 상태에서 adapter 영향 없음),
  `vm-workload-e2e.sh` PASS. `anvil-mcp-e2e.sh semantic`은 key-free 구간 `200`, LLM-echo
  substep만 known-invalid local Google key로 실패(provider-key 의존, Phase 2와 동일 기록).
- stdio backend 운영 주의: `EPHEMERA_MCP_STDIO_USER`(기본 `nobody`)와
  `/var/lib/ephemera/mcp-stdio` scratch default를 확인한다. `EPHEMERA_MCP_BIND_IP`는
  기본 안전한 bridge IP bind를 override할 수 있고, source-IP `403`이 defense-in-depth로
  남는다.

v0.7 sync Phase 4 KVM/installer gate:

- CI-safe gate all green: `git diff --check`, installer `bash -n` ×3(install.sh/
  uninstall.sh/build_release.sh), targeted test group, full glob EXIT=0, web build drift
  없음, 3 builds.
- KVM: `sudo bash e2e_test.sh` `334✓ / 0✗`. `anvil-mcp-e2e.sh` lifecycle+flock PASS,
  `vm-workload-e2e.sh` PASS. `anvil-mcp-e2e.sh semantic`은 standing provider-key caveat
  (key-free 구간 `200`, LLM-echo substep만 known-invalid local Google key로 실패).
- installer/release build: `bash install.sh --help`/`bash uninstall.sh --help` OK(시스템
  변경 없음). release build를 root로 실행 → SLIM(≈27M) + FULL(≈250M) tarball + `.sha256`
  checksum(dist/ gitignored). `build_release.sh`가 kernel/firecracker를 `sha256sum -c`로
  검증한다.
- 외부 Web UI 노출은 reverse proxy/TLS 또는 private network 뒤에서만 한다.

upstream parity scope 채택 완료:

- `v0.4.0`-`v0.7.0`은 모두 anvil main baseline으로 병합·적응됐다. 2026-07-02 기준 관찰된
  upstream 최신 tag는 `v0.7.0`이며 pending sync 후보는 없다. `v0.7.0` 이후 새 tag가
  관찰되면 별도 sync/adoption review로 처리한다.
- release-gate: 코드 항목 4종(audit-writer sentinel, stdio stderr scrub, `credential_env`
  reserved names, production-mux auth sentinel)은 2026-07-06 follow-up batch(`4a802f5`,
  `0376afa`, `613a01b`, `cd2e70b`, `de5a7aa`, `0625df5`)로 닫혔다. 마지막 open gate
  (valid provider key `semantic` run, e2e step 59)도 `18c7559`에서 OpenAI `gpt-4o`로
  닫혔다(`scripts/anvil-mcp-e2e.sh semantic` PASS + full e2e `343✓/0✗`, step 59 실행).
  남은 release-gate open 항목은 없다.

현재 baseline 기반 upstream E2E는 `/metrics`, `/stats`, streaming/depth/watchdog,
snapshot-restore recovery, real-LLM smoke, in-VM helper 경로를 포함할 수 있다. provider key가 있는 환경에서는 `GOOGLE_API_KEY`,
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY` 값이 문서와 fixture에 남지 않았는지 별도로
확인한다. `/metrics`는 기본 unauthenticated이므로 외부 노출 release 후보에서는
`EPHEMERA_METRICS_REQUIRE_AUTH=true` 또는 network isolation 정책을 함께 적는다.
`webdev_demo.sh` 검증은 paid-tier Gemini key와 충분한 host memory가 필요하므로
release blocker가 아니라 operator demo 검증으로 별도 기록한다.

## `anvil-v0.3.2` GitHub Release 게시 기록과 본문

- Tag: `anvil-v0.3.2`
- Target commit:
  `18b4506204a68a8fd9e3608976727953869f94a6`
- Published: 2026-06-04 14:22:49 KST
- URL: <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.2>
- Release source: scheduler snapshot replication, scheduler metrics,
  scheduler-aware flock placement

```markdown
# anvil-v0.3.2 - Scheduler replication and flock placement

`anvil-v0.3.2`는 `anvil-v0.3.1` 이후 scheduler 기반 runtime 운영성을 확장한
release다. upstream ephemera runtime baseline은 계속 `v0.3.6`이다. upstream
`v0.4.0` PR-A storage/recovery 변경은 포함하지 않는다.

## 포함 내용

- manual cross-host snapshot replication:
  - daemon `POST /snapshots/{id}/export`가 streamable snapshot bundle을 export
  - daemon `POST /snapshots/import`가 bundle을 staging/validation 후 atomic publish
  - MCP `anvil_replicate_snapshot`이 source export stream을 target import로 전달
  - RuntimeRouter가 replication 성공 후 scheduler `SnapshotLocations`를 갱신
- scheduler service metrics:
  - `GET /metrics`
  - `anvil_scheduler_control_loop_running`
  - `anvil_scheduler_persistence_degraded`
  - `anvil_scheduler_host_status_count`
  - `anvil_scheduler_suspect_vm_placements`
  - 마지막 poll/reconcile 완료 timestamp gauge
- scheduler process integration coverage:
  - hosts file bootstrap
  - stale state override
  - fake daemon `/health`
  - `/control-loop/status`, `/schedule/spawn`, `/metrics`
- MCP `anvil_spawn_flock` scheduler-aware single-host placement:
  - scheduler router config가 있을 때 roles 수 기반 active VM quota/capacity로 host 선택
  - 선택된 host의 기존 daemon `POST /flocks` 호출
  - 반환된 member VM placement를 scheduler `PlacementStore`에 기록
  - daemon direct `POST /flocks` wire contract 유지

## 보안/운영 주의

- `agent_token`, authorization header, daemon raw response, raw `metadata.json` body는
  replication response, audit, metrics, operator 문서에 노출하지 않는다.
- 복제된 snapshot restore는 target daemon이 새 `agent_token`을 생성해 guest agent에
  vsock으로 주입하므로 source host token을 재사용하지 않는다.
- diff snapshot replication은 target host에 base full snapshot이 필요하다.
  `include_dependencies=true`이면 router가 base full을 먼저 복제한다.
- scheduler metrics에는 token, host endpoint, daemon raw response, snapshot metadata를
  넣지 않는다.
- scheduler-aware flock placement는 이번 release에서 single-host placement다.
  true cross-host member placement는 후속 설계 범위다.

## 검증

- `go test ./... -count=1`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n scripts/anvil-scheduler-smoke.sh`
- `bash -n scripts/install-anvil-scheduler-systemd.sh`
- `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`
- GitHub Actions `CI` on `main`:
  - run `26879303599`
  - commit `18b4506204a68a8fd9e3608976727953869f94a6`
  - conclusion `success`
- 2026-06-04 KST local scheduler smoke:
  - `scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010 --json-out /tmp/anvil-scheduler-smoke-20260604.json`
  - result: pass
- 2026-06-04 KST daemon-backed MCP flock smoke:
  - `scripts/anvil-mcp-e2e.sh flock`
  - result: pass
- 2026-06-04 KST scheduler-router MCP flock smoke:
  - `ANVIL_MCP_TENANT_ID=tenant-1`
  - `ANVIL_MCP_SCHEDULER_HOSTS_FILE=/tmp/anvil-mcp-router-hosts-20260604.json`
  - `ANVIL_MCP_SCHEDULER_STATE=/tmp/anvil-mcp-router-state-20260604.json`
  - result: pass
  - placement state recorded two flock member VMs on `host-a`
  - daemon cleanup verified with empty `/vms` and `/flocks`
```

## `anvil-v0.3.1` GitHub Release 게시 기록과 본문

- Tag: `anvil-v0.3.1`
- Target commit: `1f63f04bc559270ca3fa2f5b9ee80078927ead93`
- Published: 2026-05-29 01:48:25 KST
- URL: <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.1>
- Release source: anvil scheduler control loop + operational follow-up roadmap

```markdown
# anvil-v0.3.1 - Scheduler control loop and operational roadmap

`anvil-v0.3.1`은 `anvil-v0.3.0` 이후 scheduler service를 장시간 운영 가능한
control-plane process에 가깝게 확장한 release다. upstream ephemera `v0.4.0`
PR-A storage/recovery 변경은 포함하지 않는다.

## 포함 내용

- scheduler control loop:
  - configured runtime host의 `/health`를 주기적으로 poll
  - degraded/unhealthy host observation 저장
  - daemon `GET /vms` 기반 VM placement reconciliation
  - 장애 host의 기존 VM placement를 `suspect_vm_placements`로 표시
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
  - 확장된 `GET /placements`
- persistence degraded gate:
  - `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true`이면 scheduler state 저장 장애 중 신규
    scheduling을 `503`으로 차단
- smoke/runtime verification:
  - `scripts/anvil-scheduler-smoke.sh`가 `/control-loop/status`까지 확인
  - `smoke_only: true` host가 일반 scheduling fallback에 섞이지 않는지 확인
- 운영 문서:
  - `docs/operations/2026-05-29-anvil-follow-up-development.md`

## 보안/운영 주의

- current daemon `/health`가 scheduler capacity fields를 생략해도 hosts file의
  `available_vms`, `available_snapshot_bytes`, `egress_policies`를 보존한다.
- `/health`의 `egress_policies`는 omitted와 explicit `[]`를 구분한다.
- config-managed host는 hosts file이 source of truth다.
- hosts file에서 제거된 managed host와 runtime-added host 삭제 경로는
  observation/status/suspect state를 함께 정리한다.
- `agent_token`은 scheduler surface와 release artifact에 노출하지 않는다.

## 검증

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
- independent code review: no blocking or important issues after review fixes

## 다음 후보

- scheduler full-process integration test
- 실제 systemd host에서 scheduler 운영 배포 검증
- scheduler observability metrics/alerts
- cross-host snapshot replication
- scheduler-aware cross-host flock placement
- egress L7 proxy/SNI hardening
- snapshot storage quota dashboard
- scheduler host registration hardening
- upstream ephemera `v0.4.0` PR-A adoption review
```

## `anvil-v0.3.0` GitHub Release 게시 기록과 본문

- Tag: `anvil-v0.3.0`
- Target commit: `95215e2cf85b14f82cf5d0ef7caa2b1ea77da992`
- Published: 2026-05-28 14:57:59 KST
- URL: <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.3.0>
- Release source: ephemera `v0.3.6` runtime baseline + anvil workload/scheduler
  automation

```markdown
# anvil-v0.3.0 - ephemera v0.3.6 runtime baseline and workload automation

`anvil-v0.3.0`은 `anvil-v0.2.0` 이후 ephemera `v0.3.2`-`v0.3.6` runtime
baseline을 anvil downstream에 통합하고, IronClaw 운영자가 deterministic workload와
scheduler service를 더 재현 가능하게 검증할 수 있게 만든 release다.

이 release는 anvil의 제품 정체성을 ephemera로 바꾸지 않는다. `EPHEMERA_*`,
`goose-*`, `ephemera_*` namespace는 기반 Firecracker runtime compatibility surface로
유지하고, anvil 공개 product surface는 `anvil_*` MCP tools, scheduler, tenant/egress,
workload automation으로 설명한다.

## 포함 내용

- upstream ephemera `v0.3.2`-`v0.3.6` runtime baseline:
  - live VM cold-restart와 `vms/<vm_id>/state.json`
  - watchdog dead persistence, per-agent restart, in-VM control-plane token injection
  - `EPHEMERA_API_TOKENS_FILE`, SIGHUP token rotation fan-out
  - Prometheus `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo
  - autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`
  - Goose `--output-format json` assistant text extraction
- script-only workload runner:
  - guest `POST /workloads/run`
  - daemon `POST /vms/{vm_id}/workloads/run` proxy
  - deterministic nginx and Go HTTP workload E2E
  - workload stdout/stderr cap, timeout process-group cleanup, symlink/path hardening
- Goosetown/webdev operator tooling:
  - `webdev_demo.sh`
  - orchestrator/worker/reviewer webdev profiles
  - Vite template assets harvested through Town Wall
- scheduler production automation:
  - `scripts/anvil-scheduler-smoke.sh`
  - `scripts/install-anvil-scheduler-systemd.sh --verify`
  - smoke host isolation with `smoke_only: true`
  - host inventory collision check before `PUT /hosts`
  - cleanup verification through `DELETE /hosts/{name}`
- release/operations documentation:
  - v0.3.0 release candidate handoff
  - scheduler runbook and release checklist gates
  - upstream `v0.3.6` adoption review

## 보안/운영 주의

- `agent_token`은 계속 `POST /vms` 응답 외에는 노출하지 않는다.
- `gtcall`은 peer credential을 VM 내부에 직접 노출하지 않고 daemon proxy token
  injection 경계를 유지한다.
- `/metrics`는 upstream 기본값상 unauthenticated이다. 외부 노출 환경에서는
  `EPHEMERA_METRICS_REQUIRE_AUTH=true` 또는 network isolation을 사용한다.
- workload runner는 `/workspace/workloads/*.sh` regular file만 실행하며 final file,
  workload root, nested parent symlink를 거부한다.
- local `configs/goose-secrets.yaml`과 profile별 secrets는 release artifact에 포함하지
  않는다.

## 검증

- `go test ./...`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n webdev_demo.sh`
- `bash -n scripts/build_image.sh`
- `bash -n scripts/anvil-scheduler-smoke.sh`
- `bash -n scripts/install-anvil-scheduler-systemd.sh`
- `bash -n scripts/vm-workload-e2e.sh`
- `bash scripts/install-anvil-scheduler-systemd.sh --dry-run --no-build --no-enable --verify`
- local scheduler binary smoke:
  `bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010`
- `bash scripts/secret-scan.sh`
  - tracked tree: `PASS`
  - git history: `WARN` for historical secret-like fixture/history
  - ignored/local files: `WARN` for local `goose-secrets.yaml` files, values not printed
- KVM host:
  - `sudo -n bash e2e_test.sh`
  - `sudo -n bash scripts/vm-workload-e2e.sh`

## 다음 후보

- upstream ephemera `v0.4.0` PR-A storage/recovery adoption review
- cross-host snapshot replication
- scheduler-aware cross-host flock placement
- L7 egress proxy/SNI hardening
- snapshot storage quota dashboard
```

## `anvil-v0.2.0` GitHub Release 게시 기록과 본문

- Tag: `anvil-v0.2.0`
- Target commit: `5b8298fab17b455a9e4e4325618d2743d9486a6c`
- Published: 2026-05-15 17:53:21 KST
- URL: <https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.2.0>

```markdown
# anvil-v0.2.0 - Runtime scheduler, Goosetown MCP, and observability foundation

`anvil-v0.2.0`은 `anvil-v0.1.0`의 IronClaw MCP integration 위에 runtime scheduler,
tenant/egress/audit foundation, observability endpoint, ephemera `v0.3.1`
Goosetown hardening, Goosetown MCP tool surface를 추가한다.

## 포함 내용

- `cmd/anvil-scheduler`: host/quota/placement state 기반 schedule decision service.
- `internal/anvilmcp` scheduler/runtime foundation:
  - `PlacementStore`
  - `QuotaStore`
  - snapshot locality preferred host
  - spawn/restore retry/failover
  - daemon `GET /vms` 기반 placement reconciliation
  - IronClaw/Gemini tool input schema compatibility 검증
- daemon control-plane foundation:
  - `GET /health`
  - `GET /metrics`
  - `GET /metrics/vms`
  - `GET/PUT /tenants/{tenant_id}`
  - `GET /audit/runtime`
  - `POST /audit/runtime/prune`
- profile egress policy:
  - `deny_all`
  - `profile`
  - `allow_all`
  - profile별 `egress.json` allow CIDR/host/DNS rule
- observability:
  - lifecycle counter와 duration sum/count
  - queue depth
  - per-VM JSON metrics
  - optional OTLP-compatible HTTP trace export
- Goosetown MCP tools:
  - `anvil_spawn_flock`
  - `anvil_list_flocks`
  - `anvil_get_flock`
  - `anvil_delete_flock`
  - `anvil_post_townwall`
  - `anvil_get_townwall_history`
- ephemera `v0.3.1` Goosetown hardening:
  - flock member watchdog
  - flock metadata persistence
  - daemon restart 후 read-mostly flock recovery
  - Town Wall monotonic `seq`
  - fatal bind startup

## 보안/호환성

- `POST /vms` 외 응답과 MCP output은 `agent_token`을 노출하지 않는다.
- upstream ephemera `v0.3.1`의 `POST /flocks` `agent_tokens` 응답 추가는 anvil
  downstream에서 채택하지 않는다.
- `EPHEMERA_*` runtime 환경 변수와 `goose-*` binary/API 이름은 호환성을 위해
  유지한다.
- `anvil_*` MCP tool name과 기존 input field 의미는 유지된다.

## 검증

- `go test -count=1 ./...`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n e2e_test.sh`
- `bash -n scripts/build_image.sh`
- `bash -n scripts/anvil-mcp-e2e.sh`
- `git diff --check`
- KVM host에서 `go build -o anvil-daemon ./cmd/goose-daemon/` 후
  `sudo bash e2e_test.sh`
- daemon 실행 상태에서 `scripts/anvil-mcp-e2e.sh flock`
```

## `anvil-v0.1.0` GitHub Release historical 본문

아래 본문은 `anvil-v0.1.0` GitHub Release 게시 당시 사용한 historical body다.

```markdown
# anvil-v0.1.0 - IronClaw integration over ephemera runtime v0.2.0

`anvil-v0.1.0`은 IronClaw가 ephemera Firecracker runtime을 MCP tool로 호출할 수
있게 하는 첫 통합 릴리즈다. 이 릴리즈는 ephemera runtime `v0.2.0`을 기반으로
하며, VM lifecycle 의미는 ephemera daemon API가 소유하고 anvil은 얇은 stdio MCP
adapter로 그 기능을 IronClaw에 노출한다.

## 포함 내용

- `cmd/anvil-mcp`: IronClaw용 stdio MCP server entrypoint.
- `internal/anvilmcp`: 설정 로더, daemon HTTP client, session alias 저장소,
  MCP tool handler.
- VM lifecycle MCP tools:
  - `anvil_spawn_vm`
  - `anvil_run_task`
  - `anvil_get_vm_health`
  - `anvil_stop_vm`
  - `anvil_delete_vm`
- Workspace copy-in/out MCP tools:
  - `anvil_copy_in`
  - `anvil_copy_out`
  - VM 내부 `/workspace` 기준 단일 file copy를 지원한다.
  - text와 base64 encoding, overwrite 정책, 4 MiB 단일 file 제한을 적용한다.
- Snapshot MCP tools:
  - `anvil_create_snapshot`
  - `anvil_list_snapshots`
  - `anvil_restore_snapshot`
  - `anvil_delete_snapshot`
- daemon 환경 변수 alias:
  - `ANVIL_API_ADDR`, `ANVIL_API_PORT`
  - `ANVIL_API_TOKENS`, `ANVIL_API_TOKEN`
  - `ANVIL_PUBLIC_URL`, `ANVIL_AGENT_PORT`
  - canonical `EPHEMERA_*` 환경 변수와 호환된다.
- MCP adapter 환경 변수:
  - `ANVIL_DAEMON_URL`
  - `ANVIL_API_TOKEN`
  - `ANVIL_MCP_DEFAULT_TIMEOUT`
- 보안 경계:
  - daemon 직접 API의 `POST /vms`만 VM 접근에 필요한 `agent_token`을 반환한다.
  - restore 응답과 MCP output은 `agent_token`을 노출하지 않는다.
  - 외부 client는 daemon proxy와 control-plane token을 기준으로 통합한다.

## 검증

- `go test ./...`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `ironclaw mcp test anvil --no-onboard --cli-only`
- 실제 IronClaw agent가 `anvil_spawn_vm`, `anvil_copy_in`,
  `anvil_copy_out`, `anvil_get_vm_health`, `anvil_stop_vm`,
  `anvil_delete_vm`을 호출하는 E2E 검증.

## 운영 주의사항

- `anvil`은 IronClaw 통합 project이고, `ephemera`는 기반 Firecracker runtime이다.
- ephemera runtime release tag는 `v*` 형식을 사용하고, anvil integration release
  tag는 `anvil-v*` 형식을 사용한다.
- 공개 운영은 TLS 종료 reverse proxy 뒤에서 수행한다.
- 운영 모드에서는 `EPHEMERA_API_TOKENS` 또는 `ANVIL_API_TOKENS`를 설정한다.
- 로컬 `configs/goose-secrets.yaml`과 profile별 secret file은 release artifact에
  포함하지 않는다.
```

## 외부 효과 승인

다음 작업은 repository 외부 상태를 바꾸므로 사용자의 명시적 승인을 받은 뒤에만
수행한다.

- Git tag 생성
- Git tag push
- GitHub Release publish

승인 요청 전에는 반드시 다음 값을 먼저 보여 준다.

- tag name
- target commit
- release body source: `docs/operations/release-checklist.md` 안의 해당
  `anvil-v*` GitHub Release 본문 fenced section

`gh release --notes-file`에는 전체 체크리스트 파일을 넘기지 않는다. 게시 전 fenced
본문만 별도의 검토된 notes file로 추출한 뒤 그 파일을 사용한다.

승인 없이 tag를 만들거나, tag를 push하거나, GitHub Release를 게시하지 않는다.
