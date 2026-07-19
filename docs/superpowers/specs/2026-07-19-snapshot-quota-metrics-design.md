# Snapshot (tenant) quota metrics — 설계

**날짜:** 2026-07-19
**상태:** 승인됨 (사용자 승인 2026-07-19)
**관련:** scheduler quota 백엔드(`internal/anvilmcp/quota_store.go`, `SchedulerQuotaStorePath`), scheduler `/metrics`(`internal/anvilmcp/scheduler_metrics.go`), 메트릭 라벨 정책(`docs/operations/observability.md` — bounded-enum only, tenant_id 제외)

## 목표

운영자가 **fleet 전역 quota 사용량 대 한도**와 **quota near/over 테넌트 수**를 resource별로 관측하고, near-overflow에 알림을 걸 수 있게 한다. 노출 경로는 scheduler의 **기존 Prometheus `/metrics`**(→ Grafana, → alert rule)다. 신규 프론트엔드·신규 API·per-tenant 라벨 없음.

## 배경

- quota 상태는 scheduler의 `QuotaStore`에 per-tenant로 존재하나(`TenantQuota`/`TenantUsage`, 각 5개 int64 필드), **현재 어디에도 노출되지 않는다** — 스케줄링 결정에만 내부적으로 읽힌다(`SchedulerInputs`).
- 운영자 web UI(`web/`)는 **데몬**(`goose-daemon` `/ui/`) 소유이고 Monitoring 탭은 이미 Grafana를 iframe으로 임베드한다. quota는 **scheduler**(별도 서비스)에 있고 scheduler는 이미 `/metrics`를 노출한다. 따라서 "대시보드"는 scheduler `/metrics`에 quota gauge를 추가해 Grafana에서 소비하는 것이 최소·기존 패턴 정합이다.
- **정책 제약**: scheduler 메트릭 라벨은 bounded-enum만 사용하며 `tenant_id`를 의도적으로 제외한다(`observability.md`). scheduler `/metrics`는 무인증(loopback/reverse-proxy 뒤에서만 scrape). 따라서 per-tenant 라벨은 채택하지 않는다(cardinality + 무인증 노출) — **aggregate + threshold 카운트**로 "테넌트별"을 조율한다.

## 스코프

**포함:** scheduler `/metrics`에 aggregate quota gauge family(resource bounded-enum 라벨) 추가. 5개 quota 차원 전부. 관련 문서.
**제외(비목표):** per-tenant 라벨 또는 per-tenant `/quotas` API(하이브리드 옵션 — 미채택; "어느 테넌트"는 host의 quota-store JSON 직접 조회로 확인, runbook에 문서화; 인증된 read API는 향후 후속 여지). 배포용 Grafana 대시보드 JSON(운영자가 `EPHEMERA_GRAFANA_URL`로 자체 Grafana 소유 — metrics + 문서화된 쿼리만 제공). 설정 가능한 임계값. 스케줄링 동작 변경.

## 메트릭 family

전부 gauge, prefix `anvil_scheduler_quota_*`, bounded `resource` 라벨.
`resource ∈ {snapshot_bytes, snapshot_count, active_vms, concurrent_tasks, retained_audit_records}` (렌더 순서 고정 = 이 순서, snapshot 우선).

| 메트릭 | 의미 |
|---|---|
| `anvil_scheduler_quota_usage_total{resource}` | 전 테넌트 usage 합(fleet usage) |
| `anvil_scheduler_quota_limit_total{resource}` | limit>0인 테넌트의 limit 합(fleet capacity) |
| `anvil_scheduler_quota_tenants_near{resource}` | `limit>0` 이고 `0.9 ≤ usage/limit ≤ 1.0` 인 테넌트 수 |
| `anvil_scheduler_quota_tenants_over{resource}` | `usage > limit`(limit>0) 인 테넌트 수 |
| `anvil_scheduler_quota_tenants_total` | 추적 중 테넌트 총수(resource 라벨 없음; 분모/맥락) |

라벨은 `resource`(bounded enum) 하나뿐 — `tenant_id`/host/PII 없음(정책 준수). `tenants_total`은 라벨 없는 단일 gauge.

## 의미론 / 엣지 케이스 (문서화 + 테스트 대상)

- `over` = `usage > limit` (strict).
- `near` = `limit > 0 && usage ≤ limit && float64(usage)/float64(limit) ≥ nearQuotaThreshold`. 정확히 100%(usage==limit)는 **near**, 100% 초과는 **over** (상호 배타).
- `limit ≤ 0`(미설정/무제한): `limit_total`·`near`·`over`에서 제외. 단 `usage_total`에는 그 테넌트 usage가 여전히 합산된다.
- `usage_total`은 limit 유무와 무관하게 모든 테넌트 usage 합.
- `nearQuotaThreshold = 0.9` 상수(설정 불가 — YAGNI, 문서화).
- 비율은 float로 계산(정수 나눗셈 금지).

## 아키텍처 / 데이터 흐름

기존 imperative 렌더 스타일(`RenderSchedulerMetrics`: `# HELP`/`# TYPE`/`Fprintf`, `writeSchedulerGauge` 헬퍼)을 따른다. 3개 단위:

1. **`QuotaStore.QuotaAggregate() QuotaAggregate`** (`quota_store.go`) — `RLock` 1회 패스로 테넌트를 순회하며 resource별 `{UsageTotal, LimitTotal, Near, Over}` int64 4종 + `TenantsTotal` int 집계. 순수 함수, 테이블 테스트 용이. `QuotaAggregate` 구조는 resource→집계 map 또는 resource별 필드 struct(구현 재량, resource enum은 고정 순서 슬라이스로 표현).

2. **`RenderQuotaMetrics(agg QuotaAggregate) string`** (신규 `internal/anvilmcp/quota_metrics.go`) — `RenderSchedulerMetrics`와 동형의 imperative 렌더. family별 `# HELP`/`# TYPE` 1줄씩 + resource 고정 순서로 gauge 라인. `tenants_total`은 라벨 없는 단일 라인.

3. **`handleMetrics` 배선** (`scheduler_service.go`) — 기존 `RenderSchedulerMetrics(state)` 출력 뒤에 `RenderQuotaMetrics(s.quotas.QuotaAggregate())`를 append. (핸들러가 이미 `s.quotas` 보유.)

경계: `QuotaAggregate`는 `QuotaStore` 내부 상태만 읽는 순수 집계(락 규율은 store 소유). `RenderQuotaMetrics`는 집계→문자열 순수 변환(스토어/락 무관). 배선만 `handleMetrics`.

## 알림 / 쿼리 (문서 예시)

- fleet 활용률: `anvil_scheduler_quota_usage_total{resource="snapshot_bytes"} / anvil_scheduler_quota_limit_total{resource="snapshot_bytes"}`.
- critical alert: `anvil_scheduler_quota_tenants_over{resource="snapshot_bytes"} > 0`.
- warning alert: `anvil_scheduler_quota_tenants_near{resource="snapshot_bytes"} > 0`.
- "어느 테넌트?"는 metrics에 없다(aggregate 설계) → alert 후 host의 quota-store JSON(`SchedulerQuotaStorePath`) 직접 조회. runbook에 절차 명시.

## 검증

- **`QuotaAggregate` 유닛(테이블):** 빈 스토어 / under(50%) / near(90%, 100%) / over(>100%) / 무제한(limit=0, usage>0) / 다중 테넌트 혼합 — 5개 resource 각각에서 `UsageTotal/LimitTotal/Near/Over/TenantsTotal` 정확. limit=0 테넌트가 near/over/limit_total에서 제외되고 usage_total엔 포함되는지 회귀. 100% 경계가 near(over 아님)인지 회귀.
- **`RenderQuotaMetrics` 유닛:** 알려진 집계 → 정확한 `# HELP`/`# TYPE` + resource 고정 순서 gauge 라인 문자열 assert. `tenants_total` 단일 라인. bounded resource enum만 출력(tenant/PII 라벨 부재 회귀).
- **`handleMetrics` 통합:** `/metrics` 응답이 기존 `anvil_scheduler_*` 라인과 함께 신규 quota 라인을 포함. content-type 불변.
- **게이트:** `go test ./internal/anvilmcp/... -race`, `go build ./...`, `go vet ./...`, `gofmt -l .`(신규 CI gofmt 게이트 준수 — 비어야 함).

## 문서

- `docs/operations/observability.md`: scheduler metric family 목록에 5개 gauge 추가(의미·resource enum·예시 PromQL/alert). tenant 식별은 quota-store JSON 조회임을 명시.
- `docs/operations/runbook.md`: near/over quota 알림 대응 절차(어느 테넌트인지 quota-store JSON에서 확인).
- `CONTEXT.md`: 백로그 "snapshot storage quota dashboard" 항목을 종결(aggregate scheduler metrics로 구현, per-tenant는 비목표).

## 비목표 (재확인)

per-tenant 라벨·`/quotas` API·Grafana 대시보드 JSON 아티팩트·설정 가능 임계값·스케줄링 동작 변경. web/(데몬 UI) 변경 없음.
