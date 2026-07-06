# ephemera core service parity for anvil design

## 상태

- 작성일: 2026-07-02
- 대상 branch: `main` 기준 후속 `sync/ephemera-v0.7-core-service-parity`
- upstream 기준: ephemera `v0.7.0`
- anvil 현재 runtime baseline: upstream ephemera `v0.3.6`
- 목표: ephemera core service와 operator 기능을 anvil에서 runtime-grade로 100%
  지원한다.

## 배경

anvil은 IronClaw의 tool call을 Firecracker MicroVM 실행으로 변환하는 downstream
product fork다. ephemera는 계속 발전하는 upstream runtime engine이다. 2026-07-02
기준 upstream ephemera는 `v0.7.0`까지 진행되어 있고, anvil `main`은 `v0.3.6`
baseline 위에 scheduler, tenant/egress, runtime audit, snapshot replication,
IronClaw-facing MCP adapter를 더한 상태다.

이 설계는 ephemera의 core service 기능이 upstream mainline에 진입했다는 판단을
전제로 한다. 따라서 anvil은 ephemera daemon/runtime/operator 기능을 선별 일부가
아니라 완전한 runtime service로 지원해야 한다. 단, upstream 기능을 모두
`anvil_*` MCP tool로 그대로 공개하는 것은 목표가 아니다. anvil의 보안 경계,
IronClaw 통합 계약, scheduler/tenant/egress 정책은 계속 유지한다.

## 목표

- ephemera daemon, storage, network, VM lifecycle, task/session, Web UI,
  profile/config API, monitoring, MCP Gateway, installer를 anvil 안에서
  upstream-compatible하게 지원한다.
- upstream canonical API/env/tool 이름을 유지한다.
- operator가 anvil 프로젝트 안에서 기능을 찾을 수 있도록 anvil alias와 문서를
  추가한다.
- IronClaw-facing `anvil_*` MCP adapter와 runtime/operator MCP Gateway를 분리한다.
- 각 phase를 runtime-grade gate로 검증한 뒤에만 완료로 인정한다.
- upstream `v0.7.0` 대비 parity matrix를 작성해 `adopted`, `adapted`,
  `deferred`, `excluded` 상태를 닫는다.

## 비목표

- `HardcoreMonk/anvil`을 standalone repository로 detach하지 않는다.
- upstream `EPHEMERA_*`, `goose-*`, `ephemera-ctl` 이름을 anvil 이름으로 일괄
  rename하지 않는다.
- MCP Gateway를 이번 parity 범위에서 IronClaw가 직접 호출하는 public anvil MCP
  surface로 승격하지 않는다.
- KVM 검증 없이 runtime phase 완료를 선언하지 않는다.
- Web UI를 기본 외부 공개 서비스로 만들지 않는다. 외부 노출은 TLS 종료
  reverse proxy 뒤에서만 허용한다.

## Surface 경계

anvil의 외부 표면은 두 층으로 나눈다.

| 표면 | 역할 | 포함 항목 |
|---|---|---|
| Runtime/operator surface | ephemera service를 anvil 안에서 운영하는 표면 | daemon HTTP API, `EPHEMERA_*`, `goose-*`, `ephemera-ctl`, Web UI, installer, MCP Gateway |
| IronClaw integration surface | IronClaw가 호출하는 상위 실행 계약 | `anvil_*` MCP tools, scheduler, tenant/egress, runtime audit, token redaction policy |

ephemera 기능은 runtime/operator surface에서 100% 동작해야 한다. 하지만 모든 기능을
IronClaw integration surface로 그대로 올리지는 않는다. 예를 들어 MCP Gateway는
runtime service 기능으로 채택하지만, `cmd/anvil-mcp`와 같은 역할로 취급하지 않는다.

## Branding과 alias 정책

- canonical 이름은 upstream 그대로 유지한다.
  - `EPHEMERA_*`
  - `goose-*`
  - `ephemera-ctl`
  - upstream daemon HTTP API
- anvil alias는 compatibility layer로 추가한다.
  - operator 문서와 installer/runbook에서 anvil 프로젝트 안의 위치를 찾기 쉽게 한다.
  - upstream sync 충돌을 만들 만큼 deep rename하지 않는다.
- release note는 "upstream runtime/operator support"와 "anvil product changes"를
  분리해서 작성한다.

## Phase 설계

### Phase 0: Baseline 정리

현재 문서 현행화 변경을 먼저 commit한다. 기준 문서가 clean해야 후속 sync branch에서
upstream adoption 판단을 재현할 수 있다.

완료 기준:

- 현재 baseline 문서가 `v0.3.6` runtime baseline과 upstream latest observed
  `v0.7.0`을 정확히 구분한다.
- `v0.4.0`-`v0.4.5`는 planned sync 후보로 기록한다.
- `v0.5.0`-`v0.7.0`은 core/operator parity backlog로 기록한다.
- `git diff --check`가 통과한다.

### Phase 1: `v0.4.0`-`v0.4.5` runtime stability

storage/recovery, disk preflight, COW, auth/audit, token TTL, flock lifecycle,
streaming task, restored VM recovery를 병합한다.

채택 원칙:

- COW recovery, rootfs diff snapshot, disk-space preflight, orphan reclaim,
  rollback cleanup은 core runtime으로 채택한다.
- default `EPHEMERA_DISK_MODE=cow` 전환은 KVM burn-in 전까지 deferred다.
- memory auto-snapshot public support는 opt-in/off 상태로 둔다.
- flock broadcast는 daemon runtime path는 채택 가능하지만 `anvil_*` MCP tool
  노출은 후속 설계로 둔다.
- restored VM recovery는 채택하되 source snapshot dependency를 GC가 보호해야 한다.

### Phase 2: `v0.5.0`-`v0.5.5` operator service

embedded Web UI, profile config API, multi-turn sessions, snapshot/restore UI
backend, sizing presets, system prompt API, audit/watchdog/client/monitoring console을
병합한다.

채택 원칙:

- Web UI는 anvil operator surface로 포함한다.
- Web UI를 IronClaw workflow의 필수 경로로 만들지 않는다.
- profile/config API는 daemon core service로 취급한다.
- system prompt editor와 single-agent send task는 operator 기능으로 지원한다.
- monitoring console은 token, provider key, raw daemon body를 노출하지 않아야 한다.

### Phase 3: `v0.6.0`-`v0.6.4` MCP Gateway

builtin extensions, MCP Gateway, catalog resources/prompts, per-tool/per-profile
policy, rate limit, anti-spoof, stdio backend를 병합한다.

채택 원칙:

- MCP Gateway는 ephemera runtime service 기능으로 100% 지원한다.
- `cmd/anvil-mcp`는 IronClaw-facing adapter로 유지한다.
- Gateway policy는 tenant/egress/audit 정책과 충돌하지 않아야 한다.
- stdio backend는 run-as user, scratch base, shutdown reap, resource limits를
  검증해야 한다.
- per-TAP anti-spoof는 기본 on 상태를 유지하되 host compatibility failure mode를
  문서화한다.

### Phase 4: `v0.7.0` installer/transcript/hardening

end-user installer, service unit, transcript restore, code-review hardening을
병합한다. 이미 backport된 kernel SHA 검증, `waitForAgent` per-probe timeout,
`EPHEMERA_HOME`은 upstream 전체 구현과 충돌 없이 정렬한다.

채택 원칙:

- installer는 upstream canonical을 유지하되 anvil alias/runbook을 추가한다.
- Web UI 기본 노출은 `127.0.0.1` operator local로 둔다.
- 외부 노출은 private network 또는 TLS 종료 reverse proxy 뒤에서만 문서화한다.
- transcript restore는 secret redaction과 release artifact policy를 통과해야 한다.

### Phase 5: Parity audit

upstream `v0.7.0` 대비 누락 기능, adapted 기능, deferred 기능을 표로 닫는다.

완료 산출물:

- parity matrix
- release candidate checklist
- updated `CONTEXT.md`
- updated `README.md`
- updated `RELEASE_NOTES.md`
- updated `docs/PUBLIC_RELEASE_BOUNDARY.md`
- updated `docs/ADR_INDEX.md`
- updated architecture/operations docs

## Branch와 commit 운영

- 기준 branch: `main`
- sync branch: `sync/ephemera-v0.7-core-service-parity`
- upstream sync는 tag 순서대로 merge commit을 사용한다.
- rebase/history rewrite는 하지 않는다.
- `git fetch --tags --force`는 사용하지 않는다.
- tag 확인은 `git ls-remote --tags upstream`으로 한다.

권장 commit 구조:

```text
docs: update current ephemera baseline
docs: design ephemera core service parity
merge: sync ephemera v0.4.0
fix(runtime): adapt ephemera v0.4.0 for anvil
docs: document ephemera v0.4.0 support in anvil
...
merge: sync ephemera v0.7.0
fix(runtime): adapt ephemera v0.7.0 for anvil
docs: document ephemera v0.7.0 parity in anvil
```

phase가 여러 tag를 포함하더라도 tag별 merge commit은 유지한다. adaptation commit은
충돌 규모에 따라 tag별 또는 phase별로 나눈다.

## Verification gate

각 phase는 다음 gate를 통과해야 완료로 본다.

CI-safe gate:

```bash
git diff --check
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

phase별 추가 gate:

```bash
# v0.4.x
go build ./cmd/ephemera-ctl

# v0.5.x
npm --prefix web run build

# v0.6.x
go test ./internal/mcpgateway ./cmd/goose-daemon -count=1

# v0.7.0
bash -n install.sh
bash -n uninstall.sh
bash -n scripts/build_release.sh
```

KVM host gate:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
scripts/vm-workload-e2e.sh
```

Security gate:

- `agent_token`은 `POST /vms` 응답 외부에서 노출되면 실패한다.
- Authorization header, daemon raw body, profile secret, provider key가 audit,
  metrics, Web UI logs, release artifacts에 남으면 실패한다.
- MCP Gateway가 anvil MCP adapter 권한 경계를 우회하면 실패한다.
- Web UI와 installer가 외부 bind/TLS/reverse proxy 정책을 흐리면 실패한다.
- local secret, runtime artifact, snapshot, profile secret은 commit하지 않는다.

Docs gate:

- `CONTEXT.md`
- `README.md`
- `RELEASE_NOTES.md`
- `docs/PUBLIC_RELEASE_BOUNDARY.md`
- `docs/ADR_INDEX.md`
- `docs/architecture/*.md`
- `docs/operations/*.md`

historical release body는 고치지 않는다.

## Risk

| Risk | 영향 | 완화 |
|---|---|---|
| MCP 경계 충돌 | MCP Gateway와 `cmd/anvil-mcp` 권한 모델 혼동 | runtime/operator surface와 IronClaw integration surface를 문서와 코드에서 분리 |
| Web UI 보안 표면 확대 | operator UI 외부 노출 시 token/API 위험 | 기본 local bind, reverse proxy/TLS 문서, audit/token redaction 검증 |
| token 노출 회귀 | anvil invariant 위반 | `agent_token` redaction regression test와 audit/metrics artifact scan |
| storage/COW/restore 회귀 | snapshot replication, GC, scheduler locality 손상 | source snapshot dependency guard, KVM restore/GC/replication smoke |
| installer ownership 충돌 | ephemera service와 anvil operator alias 혼동 | upstream canonical 유지, anvil alias는 wrapper/docs 수준으로 제한 |

## Open decisions

결정됨:

- Web UI 기본 노출은 `127.0.0.1` local operator 전용으로 둔다.
- 외부 노출은 reverse proxy/TLS 뒤에서만 허용한다.
- MCP Gateway의 IronClaw 직접 사용은 이번 parity 범위에서 제외하고 후속 ADR로
  검토한다.

후속 phase에서 닫을 결정:

- `EPHEMERA_DISK_MODE=cow` default flip 여부
- auto-snapshot public support 범위
- flock broadcast의 `anvil_*` MCP tool 승격 여부
- Web UI anvil alias의 정확한 command/service 이름
- `anvil-v0.4.0` 또는 별도 parity release tag 기준

## Acceptance criteria

이 설계는 다음 상태가 되면 완료된 것으로 본다.

- upstream ephemera `v0.7.0`의 daemon/runtime/operator 기능이 anvil에서 동작한다.
- intentionally adapted/deferred/excluded 항목이 parity matrix에 명시되어 있다.
- `agent_token` 노출 불변 조건이 유지된다.
- `EPHEMERA_*` canonical과 anvil alias가 함께 문서화되어 있다.
- Web UI, installer, MCP Gateway가 operator/runtime surface로 문서화되어 있다.
- IronClaw-facing `anvil_*` MCP adapter가 기존 계약을 유지한다.
- CI-safe gate와 KVM host gate가 통과한다.
- release candidate checklist가 업데이트되어 있다.
