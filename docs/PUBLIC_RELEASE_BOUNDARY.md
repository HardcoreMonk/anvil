# anvil 공개 릴리즈 경계

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-07-02
> **목적:** anvil이 공개적으로 책임지는 기능 표면과, upstream ephemera에서 가져오더라도 anvil 정책상 수정하거나 제외해야 하는 표면을 구분한다.

---

## 1. 읽는 규칙

이 문서는 anvil의 공개 릴리즈 경계를 판단하는 기준이다. 문서가 충돌하면
`CONTEXT.md`가 우선하고, 이 문서는 README보다 먼저 공개 표면을 판단한다.

- `CONTEXT.md`: 프로젝트 경계, 변경 불가 계약, 진실 기준
- `docs/PUBLIC_RELEASE_BOUNDARY.md`: 공개 릴리즈 포함/제외 표면
- `docs/ADR_INDEX.md`: 장기 설계 결정의 현재 적용 상태
- `README.md`: 사용자 진입점과 현재 사용법
- `RELEASE_NOTES.md`: 릴리즈별 변경 기록

`ephemera` upstream 릴리즈 문서는 기반 runtime 분석으로 유지한다. ephemera
릴리즈 분석 문서의 제목이나 태그 이름을 anvil로 바꾸지 않는다.

---

## 2. 공개 포함 표면

현재 anvil 공개 표면은 다음 범위를 책임진다.

| 영역 | 공개 표면 | 구현/문서 위치 |
|---|---|---|
| MCP adapter | IronClaw가 호출하는 `anvil_*` stdio MCP tool | `cmd/anvil-mcp`, `internal/anvilmcp`, `docs/architecture/mcp-architecture.md` |
| VM lifecycle | VM 생성, task 실행(`POST /vms/{id}/tasks?stream=1` optional NDJSON streaming, 기본은 buffered `{"output","error"}` 계약), health, stop, delete | ephemera daemon API + anvil MCP wrapper |
| Snapshot lifecycle | full/diff snapshot 생성, 목록, restore, 삭제 | `internal/storage`, `anvil_create_snapshot` 계열 MCP tool |
| Runtime boundary | Firecracker MicroVM, TAP/IP, rootfs, guest agent proxy | `cmd/goose-daemon`, `internal/vm`, `internal/network`, `internal/storage` |
| Runtime observability | `/metrics`, `/metrics/vms`, `/vms/{vm_id}/stats`, `GET /watchdog/status`(read-only count/ID/config), structured daemon logs | upstream ephemera runtime namespace + anvil 운영 문서 |
| Workload automation | script-only `POST /vms/{vm_id}/workloads/run` 계약 | `cmd/goose-agent`, `cmd/goose-daemon`, `scripts/vm-workload-e2e.sh` |
| Goosetown in-VM helpers | `gtwall`, `gtcall`, `webdev_demo.sh` operator demo | upstream ephemera runtime namespace + anvil 보안 경계 문서 |
| Token policy | daemon token과 guest `agent_token` 분리, MCP output token redaction | `CONTEXT.md`, `README.md`, MCP adapter |
| IronClaw integration | IronClaw 전용 실행 layer 계약 | `CONTEXT.md`, `README.md` |
| 문서 계약 | 한국어 운영 문서, 실제 API/env/file 이름 보존 | `AGENTS.md`, `CONTEXT.md` |

공개 표면에 들어간 기능은 README, RELEASE_NOTES, 관련 architecture 문서 중
영향을 받는 문서와 함께 갱신해야 한다.

---

## 3. 조건부 포함 표면

다음 표면은 runtime에는 존재할 수 있지만, anvil product 공개 표면으로 승격할 때
별도 검토가 필요하다.

| 영역 | 조건 |
|---|---|
| upstream ephemera 새 API | token 노출, lifecycle 의미, cleanup 계약이 anvil 정책과 충돌하지 않아야 한다. |
| Goosetown/flock 기능 | IronClaw MCP tool surface로 승격할 때 `agent_token` 노출 없이 Town Wall/API 경유 방식으로 노출해야 한다. |
| multi-host/scheduler/quota | runtime 안정성, 보안 경계, 운영 문서, full KVM E2E 기준이 먼저 정리되어야 한다. |
| audit/metrics/job store | public API 계약과 retention/보안 정책이 함께 정의되어야 한다. |
| replay/player 산출물 | 검증 근거와 운영 URL을 문서화한 경우에만 공개 문서 표면으로 취급한다. |
| upstream runtime namespace | `EPHEMERA_*`, `goose-*`, `ephemera_*` 이름은 runtime 호환성으로 유지한다. anvil product 표면으로 rename하려면 별도 ADR이 필요하다. |
| upstream ephemera low-level API | anvil MCP tool로 노출하려면 token redaction, tenant/egress, cleanup, audit 의미를 먼저 정의한다. |

조건부 표면을 공개 표면으로 올릴 때는 ADR을 추가하거나 기존 ADR 적용 상태를
갱신한다.

---

## 4. 공개 제외 표면

다음은 현재 anvil 공개 릴리즈 범위가 아니다.

- OpenClaw compatibility layer, shared gateway, shared runtime contract
- IronClaw가 ephemera low-level HTTP API를 직접 다루는 결합 구조
- `POST /vms` 응답 외부의 `agent_token` 노출
- upstream ephemera의 flock `agent_tokens` 응답을 anvil public API로 그대로 노출
- flock broadcast(`POST /flocks/{id}/broadcast`, `ephemera-ctl flock broadcast`)를
  `anvil_*` MCP tool로 노출하는 것. 이 phase에서 broadcast는 daemon-only runtime
  operator 표면으로만 두며, MCP tool 노출은 tenant/rate/audit 설계 전까지
  deferred다(`TestToolRegistrationsExcludeBroadcast`,
  `TestCurrentIronClawSchemasExcludeBroadcastTool`로 고정).
- 실제 구현 계약 없이 `EPHEMERA_*`, `goose-*` API/env/path를 anvil 이름으로
  일괄 rename하는 변경
- purecvisor의 libvirt/QEMU, LXC, ZFS zvol clone, multi-node HA, live migration,
  cluster evacuation, OVN multi-node automation
- 공개 검증 없이 runtime 산출물, local secret, profile별 secret을 릴리즈 표면에
  포함하는 변경

제외 표면은 코드에 존재하거나 upstream에 존재해도 anvil 공개 기능으로 설명하지
않는다.

---

## 5. upstream ephemera 변경 채택 분류

upstream ephemera 변경을 병합할 때는 다음 상태 중 하나로 분류한다.

| 상태 | 의미 | 예시 |
|---|---|---|
| `adopted` | anvil 정책과 충돌하지 않아 그대로 채택 | watchdog, metadata persistence, stale artifact rebuild |
| `adapted` | runtime 가치는 채택하되 public/API 보안 표면은 수정 | restore 응답 token redaction, env alias 병행 |
| `excluded` | anvil 공개 경계와 충돌해 제외 | flock `agent_tokens` public response |
| `deferred` | 가치가 있으나 현재 release scope 밖 | multi-host scheduler, persistent audit store, quota |
| `historical` | 과거 분석 근거로만 유지 | ephemera 0.1.0/0.2.0 분석 문서 |

`adapted`, `excluded`, `deferred` 판단은 README나 RELEASE_NOTES만으로 처리하지
말고 ADR 또는 ADR_INDEX에 남긴다.

현재 sync branch의 upstream runtime baseline 채택 상태:

| upstream tag | 주요 변경 | 현재 anvil 분류 |
|---|---|---|
| `v0.3.2` | live VM cold-restart, `vms/<vm_id>/state.json`, same-identity recovery | `adapted` — runtime recovery는 채택, token persistence는 보안/문서 redaction 정책으로 관리 |
| `v0.3.3` | watchdog dead persistence, per-agent restart, in-VM CP token auto-injection, real-LLM e2e | `adapted` — daemon 기능은 채택, MCP/public output token 노출은 금지 |
| `v0.3.4` | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP forwarding hot-fix | `adapted` — hot rotation은 채택, file permission/VM version caveat를 운영 문서에 명시 |
| `v0.3.5` | Prometheus `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo | `adapted` — runtime observability는 채택, `ephemera_*` metric namespace와 `/metrics` auth 정책을 compatibility로 설명 |
| `v0.3.6` | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing | `adapted` — demo/helper는 runtime operator 표면으로 채택, peer `agent_token`은 계속 control-plane proxy가 주입하며 직접 노출하지 않음 |
| `v0.4.0` | memory auto-snapshot, diff/COW rootfs, spawn-path cold-restart | `adapted` — storage/recovery 채택, `EPHEMERA_AUTOSNAPSHOT=true`는 opt-in·disk-expensive로 두고 public support로 승격하지 않음 |
| `v0.4.1` | client identity, `GET /audit`, per-token TTL, `ephemera-ctl` | `adapted` — daemon access audit/CLI 채택, `ephemera-ctl`은 runtime operator CLI(IronClaw MCP 대체 아님) |
| `v0.4.2` | COW spawn probe/fallback, COW+Diff snapshot | `adapted`, default cow deferred — `EPHEMERA_DISK_MODE=cow`는 명시적 opt-in, default 전환은 KVM burn-in 뒤 |
| `v0.4.3` | dynamic flock membership, pause/resume, `max_agents`, Town Wall filter/rotation | `adapted` — single-host flock lifecycle 채택, routed cross-host flock에는 미적용 |
| `v0.4.4` | streaming `/tasks`, depth guard, `/watchdog/status`, flock broadcast, slog | `adapted`, broadcast MCP exposure deferred — streaming은 buffered 기본 계약 유지, `GET /watchdog/status`는 read-only 공개 표면, flock broadcast는 daemon-only(MCP tool 노출 deferred) |
| `v0.4.5` | snapshot-restore auto-recovery | `adapted` — restore state persist + token redaction 유지. live·persisted restored VM이 참조하는 source snapshot `DELETE`는 `409`로 보호(upstream e2e 46c의 `200` orphan과 의도적 divergent) |

2026-07-02 기준 upstream `main`과 최신 upstream tag는 `v0.7.0`이다. `v0.4.0`-`v0.4.5`는
현재 sync branch에 병합·적응되어 full KVM gate로 검증한 runtime baseline이며(위 표),
`v0.5.0`-`v0.7.0`은 별도 adoption review 전까지 조건부/제외 표면 후보로 둔다.

| upstream tag 범위 | 공개 경계 판단 |
|---|---|
| `v0.5.0`-`v0.5.5` | product/operator Web UI 계열로 보인다. IronClaw 전용 execution layer 경계와 맞는지 검토하기 전까지 anvil 공개 표면으로 설명하지 않는다. |
| `v0.6.0`-`v0.6.4` | MCP Gateway 계열로 보인다. anvil MCP adapter와 권한 모델을 우회하거나 중복할 수 있으므로 별도 ADR/adoption review 전까지 공개 표면으로 승격하지 않는다. |
| `v0.7.0` | installer/transcript/hardening 계열로 보인다. kernel SHA 검증, `waitForAgent` per-probe timeout, `EPHEMERA_HOME`은 선별 backport됐지만 tag 전체는 미채택이다. |

`v0.3.2`/`v0.3.3` 병합 전 검토 근거는
`docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`에 historical analysis로
보존한다.

## 6. 이름/브랜드 경계

anvil은 ephemera를 이름만 바꾼 프로젝트가 아니다. 공개 문서에서는 다음 규칙을
유지한다.

- `anvil`: IronClaw MCP 실행 계층, scheduler, tenant/egress governance, workload
  automation, product release 경계.
- `ephemera`: Firecracker MicroVM runtime baseline, daemon HTTP API, guest agent,
  upstream runtime release tag.
- `EPHEMERA_*`, `goose-*`, `ephemera_*`는 runtime compatibility namespace다.
  호환성 때문에 보존하며 anvil product identity의 약화로 해석하지 않는다.
- anvil release note는 "upstream runtime baseline"과 "anvil product changes"를
  분리해 작성한다.

---

## 7. 릴리즈 전 경계 확인

공개 릴리즈 또는 push 전에는 변경 성격에 맞게 다음을 확인한다.

- 공개 기능이 늘었으면 이 문서의 포함/조건부/제외 표면을 갱신한다.
- 장기 설계 결정이면 `docs/ADR_INDEX.md`와 `docs/adr/*.md`를 갱신한다.
- upstream ephemera 변경을 병합했으면 채택 상태를 명시한다.
- `agent_token` 노출 경로가 `POST /vms` 응답 외부로 늘지 않았는지 확인한다.
- runtime lifecycle 변경이면 full KVM E2E 또는 그에 준하는 실환경 검증 근거를
  남긴다.
- 로컬 secret, runtime artifact, snapshot, profile secret은 커밋하지 않는다.
