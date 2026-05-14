# anvil Multi-Tenant Runtime Roadmap

## 상태

- 문서 목적: multi-tenant runtime으로 확장할 때의 책임 경계와 단계적 설계 기준
- 현재 기준: `feature/multi-tenant-runtime` runtime contract
- 구현 범위: MCP adapter boundary의 foundation. `internal/anvilmcp`는 tenant ID
  validation, quota decision, scheduler decision, host selection primitive, egress
  policy, runtime audit JSONL append/read/retention helper를 제공한다. daemon API는
  optional `tenant_id`와 `egress_policy`를 VM/snapshot/restore contract에 보존한다.
- 비구현 범위: tenant API, quota state persistence, scheduler host inventory/health
  polling, host packet filtering/proxy allowlist enforcement, billing, UI.

이 문서는 anvil이 IronClaw와 ephemera runtime을 multi-tenant 실행 기반으로
확장할 때 필요한 경계를 정리한다. 현재 ephemera daemon의 단일 호스트 VM
lifecycle 계약은 유효하며, multi-tenant 기능은 그 위에 추가될 별도 제어 계층과
daemon 계약 확장을 필요로 한다.

## 구현된 foundation

현재 구현된 부분은 MCP adapter와 daemon API contract까지 확장된 foundation이다.

- `NormalizeTenantID`: `tenant_id` 형식 검증
- `CheckTenantQuota`: active VM, snapshot count/bytes, concurrent tasks,
  retained audit records 기준 quota decision
- `Scheduler.Schedule`: quota를 host selection 전에 평가하고 `quota_exceeded` 또는
  `no_eligible_host` 같은 deterministic decision 반환
- `SelectRuntimeHost`: healthy host, VM capacity, snapshot bytes, egress policy를
  기준으로 첫 eligible host 선택
- `NormalizeEgressPolicy`: `deny_all`, `profile`, `allow_all` policy normalization
- `AppendRuntimeAudit`, `ReadRuntimeAudit`, `PruneRuntimeAudit`: symlink를 거부하는
  `0600` JSONL runtime audit append/read/retention helper
- `ANVIL_MCP_TENANT_ID`, `ANVIL_MCP_AUDIT_LOG`: MCP adapter의 optional
  tenant/audit 설정
- daemon VM/snapshot/restore request/response metadata의 `tenant_id`와
  `egress_policy`

이 foundation은 host network packet filtering을 아직 강제하지 않는다.

## 범위

Multi-tenant runtime의 설계 범위는 다음이다.

- tenant quota: tenant별 active VM, snapshot, task, audit 보관 한도
- scheduler: 요청을 실행할 host 선택과 host capacity 판단
- egress policy: tenant 또는 profile별 외부 네트워크 접근 정책
- audit storage: tenant별 tool/daemon operation 감사 기록 저장
- multi-host runtime: 여러 ephemera daemon host를 대상으로 한 실행 배치

이 문서는 위 구성 요소의 책임 경계를 정의한다. 현재 구현은 in-process scheduler
decision helper와 daemon tenant contract까지 포함하지만, 별도 scheduler daemon,
tenant API, host network enforcement는 포함하지 않는다.

## Tenant 식별자

Quota와 audit를 강제하려면 daemon이 명시적인 tenant identifier를 받아야 한다.
현재 MCP adapter는 `session_name` alias를 다루지만, 이 값은 사용자 친화적인
로컬 alias일 뿐 tenant identity가 아니다.

Tenant identifier는 먼저 MCP adapter boundary에서 받는다. adapter는 IronClaw 또는
상위 orchestration 계층이 제공한 `tenant_id`나 `ANVIL_MCP_TENANT_ID`를 검증한 뒤
daemon API body로 전달한다. tenant identifier를 임의 header, profile 이름,
session alias, VM ID에 끼워 넣어 quota나 audit의 근거로 사용하지 않는다.

권장 경계:

- MCP adapter: tenant identifier 수신, 기본 형식 검증, tool call context에 보존
- scheduler: tenant quota 조회와 host 선택에 tenant identifier 사용
- daemon API: VM/snapshot/restore operation에 tenant identifier 수신 및 metadata 보존
- audit storage: 모든 audit record에 tenant identifier 저장

## Quota 정책

Tenant quota는 scheduler 또는 별도 quota service가 판정하고, daemon은 선택된
host에서 VM lifecycle을 수행한다. quota 판정은 daemon 내부의 단일 host 상태만으로
끝나면 안 된다. multi-host 환경에서는 tenant가 여러 host에 분산될 수 있기
때문이다.

우선 관리할 quota 축은 다음이다.

| Quota | 의미 |
|---|---|
| active VMs | tenant가 동시에 실행할 수 있는 VM 수 |
| snapshot count | tenant가 보관할 수 있는 snapshot 개수 |
| snapshot bytes | tenant snapshot이 사용할 수 있는 총 저장 용량 |
| concurrent tasks | tenant가 동시에 실행할 수 있는 task 수 |
| retained audit records | tenant audit record 보관 개수 또는 보관 기간 |

Quota 초과 시 scheduler는 새 VM 생성, snapshot 생성, task 실행 요청을 daemon에
보내기 전에 거부해야 한다. daemon은 host-local 방어 한도를 둘 수 있지만,
tenant별 전역 quota의 원천이 되어서는 안 된다.

현재 `CheckTenantQuota`와 `Scheduler.Schedule`은 위 축에 대한 deterministic
decision을 제공한다. `Schedule`은 quota를 host selection보다 먼저 평가해 quota
초과 요청이 daemon host 선택이나 runtime mutation으로 내려가지 않게 한다. 다만
tenant별 usage/quota의 영속 저장소와 tenant API는 아직 구현하지 않았다.

## Scheduler 책임

Scheduler는 host selection을 소유한다. 요청의 tenant, profile, quota 상태, host
capacity, egress policy, snapshot 위치를 바탕으로 어느 ephemera daemon host에
작업을 보낼지 결정한다.

Daemon은 선택된 host에서 VM lifecycle을 소유한다. 즉 `POST /vms`,
`DELETE /vms/{vm_id}`, snapshot 생성/복원, guest agent proxy, host-local resource
cleanup은 daemon 책임이다.

MCP tool은 host-specific cleanup 의미를 인코딩하지 않는다. MCP adapter는
상위 계층이 선택한 daemon endpoint 또는 scheduler endpoint를 호출할 수 있지만,
TAP, IP, dm-snapshot, loop device, bind mount, sparse COW file 같은 host-local
정리 의미를 tool contract에 노출하지 않는다.

현재 `Scheduler.Schedule`은 host 목록 중 healthy, capacity, requested snapshot
bytes, egress policy 조건을 만족하는 첫 host를 고른다. 실제 host inventory,
health polling, snapshot locality, retry/failover는 아직 scheduler service 책임으로
남아 있다.

## Egress 정책

Egress policy는 host network policy에서 먼저 강제되어야 한다. VM profile은 어떤
policy를 선택할지에 대한 hint를 제공할 수 있지만, profile config만이 유일한
강제 계층이어서는 안 된다.

권장 경계:

- scheduler: tenant와 requested profile을 보고 허용 가능한 egress policy 선택
- daemon: 선택된 policy를 host-local network 설정에 연결
- host network policy: 실제 외부 접근 허용/차단 강제
- VM profile: 이미지, resource, tool 설정과 함께 policy hint 제공

Profile 파일을 수정하거나 우회하는 것만으로 egress 제한이 사라지는 구조는
허용하지 않는다.

현재 `EgressPolicy` 값은 policy 선택, daemon metadata, audit/debug를 위한 enum이다.
daemon은 선택된 policy를 VM/snapshot/restore metadata에 보존하지만 실제 packet
filtering, proxy allowlist, DNS policy 같은 host network enforcement는 아직
구현하지 않았다.

## 감사 저장소

Audit storage는 append-only record를 저장해야 한다. record는 사후 조사와 quota
집계에 필요한 최소 정보를 담되, VM 내부 인증 비밀이나 guest agent 접근 token을
포함하지 않는다.

기본 필드:

| Field | 의미 |
|---|---|
| tenant ID | 요청을 소유한 tenant identifier |
| VM ID | 대상 VM identifier. VM이 없으면 비워 둘 수 있음 |
| session alias | MCP `session_name` 같은 사용자 친화 alias |
| tool name | 호출된 MCP tool 이름 |
| daemon operation | 매핑된 daemon API operation 또는 내부 operation 이름 |
| result code | 성공/실패/거부/timeout 등 표준화된 결과 코드 |
| timestamp | event 발생 시각 |

Audit record에는 `agent_token`을 저장하지 않는다. `POST /vms` 응답 외에는
`agent_token`을 노출하지 않는 기존 불변 조건을 audit 설계에도 적용한다.

현재 `ANVIL_MCP_AUDIT_LOG`를 설정하면 성공/실패 MCP tool call에 대해 위 필드의
부분집합을 JSONL로 append한다. 실패 record의 `error`는 daemon status 또는
sanitized message만 담으며 daemon raw response body, snapshot metadata,
`agent_token`을 포함하지 않는다. `ReadRuntimeAudit`와 `PruneRuntimeAudit`은 조회와
보존 정책 적용을 제공하지만, 외부 운영 API와 metrics 연결은 후속 작업이다.

## 호환성/경계

Single-host runtime은 계속 유효하다. Multi-tenant 확장은 현재 ephemera daemon이
소유한 VM lifecycle을 대체하지 않고, host 선택과 tenant 정책 판정을 그 앞단에
추가하는 방향이어야 한다.

기존 계약도 rename하지 않는다.

- `EPHEMERA_*` 환경 변수는 ephemera runtime의 canonical 계약으로 유지한다.
- `goose-*` binary, bridge, workspace, runtime artifact 이름은 현재 코드 계약을
  따른다.
- `ANVIL_*` alias와 MCP adapter 환경 변수는 문서화된 의미를 유지한다.
- ephemera 릴리즈 분석 문서 제목을 anvil 제목으로 바꾸지 않는다.

restore 경로의 direct token exposure는 제거됐다. 새로운 audit record, roadmap
예제, multi-tenant 계약은 `POST /vms` 외 응답에서 `agent_token`을 노출하지 않는
불변 조건을 따른다.

## 비목표

이 roadmap의 non-goals는 다음이다.

- 완전한 multi-tenant runtime 즉시 구현
- 별도 scheduler daemon과 host inventory service 구현
- quota service 또는 tenant API 구현
- host egress packet filtering/proxy allowlist 구현
- billing
- UI
- OpenClaw compatibility layer
- background GC
