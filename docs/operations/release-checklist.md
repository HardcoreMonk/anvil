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

anvil main runtime baseline은 upstream ephemera `v0.4.5`까지 병합·적응한 runtime을
포함한다. `v0.3.2`-`v0.3.6`은 이전 release가 채택한 baseline이고, `v0.4.0`-`v0.4.5`는
이 v0.4 sync에서 병합·적응해 full KVM gate로 검증했다. upstream `main`과 최신
upstream tag는 `v0.7.0`까지 진행되어 있으나 `v0.5.0`-`v0.7.0`은 아직 anvil baseline으로
병합하지 않았다. 새 anvil release 후보가 이 baseline을 포함한다면 release 본문에는
upstream runtime 변경과 anvil product 변경을 분리해서 적는다.

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

미병합 upstream review 상태:

- `v0.5.0`-`v0.5.5`: product/operator Web UI 계열로 별도 공개 경계 검토 전까지
  anvil release baseline에 포함하지 않는다.
- `v0.6.0`-`v0.6.4`: MCP Gateway 계열로 anvil MCP adapter, IronClaw 통합 경계,
  권한 모델과 충돌하거나 중복될 수 있어 별도 설계 review가 필요하다.
- `v0.7.0`: installer/transcript/hardening 계열로 보인다. kernel SHA 검증,
  `waitForAgent` per-probe timeout, `EPHEMERA_HOME`은 선별 backport됐지만 tag 전체를
  채택한 것은 아니다.

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
