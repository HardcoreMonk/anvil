# allow_hosts deprecation cycle — 설계

**날짜:** 2026-07-18
**상태:** 승인됨 (사용자 승인 2026-07-18)
**관련:** [ADR-0002](../../adr/0002-egress-sni-transparent-filter.md) OQ8, egress SNI 필터(allow_sni, PR #59)

## 목표

레거시 egress 필드 `allow_hosts`(coarse packet substring match)의 제거를 표준 deprecation cycle로 확정한다: **지금** 런타임 deprecation 경고를 추가하고(현재 deprecation은 문서에만 있고 런타임 경고가 없다), **다음 tagged anvil 릴리즈**에서 제거한다.

## 배경

`allow_hosts`는 iptables substring-match(`-m string`) 기반의 coarse egress 통제로, fragmentation-evadable footgun이다(operator가 안전하다 오인 가능). PR #59가 `allow_sni`(파싱된 ClientHello SNI) + 기존 `allow_cidrs`(IP)로 대체했다. 탐색 결과:

- repo의 어떤 profile/config/example도 `allow_hosts`를 쓰지 않는다(docs·test만 참조).
- `allow_hosts` 사용 시 **런타임 경고가 없다** — "deprecated"는 문서에만 있다.
- `json.Unmarshal`은 unknown 필드를 무시하므로, 필드를 그냥 지우면 기존 `allow_hosts` profile은 **조용히 규칙을 잃는다**(→ 더 제한적, 사용자 모르게 파손).

따라서 즉시 제거가 아니라 deprecation cycle: 경고(release N) → 제거(release N+1).

## 스코프

**포함(이번)**: 런타임 deprecation 경고 + 마이그레이션 문서 + 제거-버전 결정 기록.
**제외(다음 릴리즈)**: 실제 필드/코드 제거(별도 작업 — 제거 시 `DisallowUnknownFields` 또는 명시 거부로 loud fail-closed).

## 변경 상세

### 1. 런타임 deprecation 경고 (`cmd/goose-daemon/egress_policy.go`)

`loadEgressProfile`에서 `validateEgressProfile`가 통과한 뒤(현재 L72-75), profile 반환 직전에:

```go
if len(profileConfig.AllowHosts) > 0 {
    slog.Warn("egress profile uses deprecated allow_hosts (coarse packet substring match, fragmentation-evadable); migrate to allow_sni (parsed ClientHello SNI) for domains + allow_cidrs for IPs — allow_hosts will be removed in the next tagged anvil release",
        "profile", profile, "allow_hosts_count", len(profileConfig.AllowHosts))
}
```

- `log/slog` import 추가(현재 egress_policy.go 미import).
- profile 로드(= VM spawn / egress 재적용)마다 1회 발화. **동작 불변** — `allow_hosts`는 여전히 적용된다(경고 side-effect만). content-free(profile 이름 + count만, host 값·token 없음).

### 2. 제거 시점 (결정 기록)

**다음 tagged anvil 릴리즈**(이 경고가 실린 릴리즈의 다음)에서 `allow_hosts` 필드·apply·validate·cleanup·test를 제거한다. 제거 시 `egressProfile` unmarshal에 `DisallowUnknownFields`(또는 명시 거부)를 적용해 잔존 `allow_hosts` profile이 **조용히 drop되지 않고 loud fail-closed**로 거부되게 한다.

### 3. 마이그레이션 안내 (`docs/operations/runbook.md`)

`allow_hosts` → 대체 대응표 추가:
- 도메인(예: `api.example.com`) → `allow_sni`(exact 또는 `*.` wildcard, 파싱된 SNI 강제).
- 고정 IP/CIDR 백엔드 → `allow_cidrs`.
- 주의: substring match와 달리 `allow_sni`는 정확 SNI 매치라 semantics가 더 엄격(부분 문자열 매치 불가) — 마이그레이션 시 도메인 목록을 명시화한다.

### 4. 결정 기록 (문서)

- `docs/adr/0002-egress-sni-transparent-filter.md` **OQ8**: deprecation cycle 확정(런타임 경고 지금 + 다음 릴리즈 제거 + loud fail-closed) 반영.
- `CONTEXT.md`: 백로그에서 `allow_hosts` 제거 항목을 "deprecation cycle 확정" 노트로 종결.
- `docs/PUBLIC_RELEASE_BOUNDARY.md`: `allow_hosts` 행에 런타임 경고 + 다음 릴리즈 제거 예정 반영(현재 "legacy/deprecated로 표기되며 유지").

## 검증

- `cmd/goose-daemon/egress_policy_test.go`: allow_hosts 있는 profile 로드 시 경고 발화(로그 캡처 또는 slog handler 주입), allow_hosts 없으면 미발화, allow_hosts 여전히 규칙 생성(동작 불변) 회귀.
- `go test ./cmd/goose-daemon/... -race`, `go build ./...`, `go vet ./...`, `gofmt -l`.

## 비목표

- `allow_hosts` 실제 제거(다음 릴리즈). allow_sni/allow_cidrs 동작 변경. 자동 마이그레이션 도구(수동 profile 편집으로 충분, YAGNI).
