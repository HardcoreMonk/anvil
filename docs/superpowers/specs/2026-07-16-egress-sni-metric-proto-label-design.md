# egress SNI verdict metric — proto label 분리 설계

**날짜:** 2026-07-16
**상태:** 승인됨 (사용자 승인 2026-07-16)
**관련:** [ADR-0002](../../adr/0002-egress-sni-transparent-filter.md), egress SNI 필터 TCP(PR #59)/QUIC(PR #66)

## 목표

egress SNI 필터의 판정 카운터 `ephemera_egress_sni_verdict_total`에 **`proto` label**을 추가해, 운영자가 TCP:443 판정과 QUIC/UDP:443 판정을 구분할 수 있게 한다. 현재는 두 경로가 같은 카운터(`outcome` label만)로 집계돼 "denied가 TCP인지 QUIC인지" 구분이 불가능하다.

## 배경

`cmd/goose-daemon/metrics_handler.go`의 `sniVerdictTotal`은 label `outcome`(allowed|denied|dropped)만 갖는다. `IncSNIVerdict(outcome)`를 TCP hook 경로와 QUIC `handleUDPQUIC` 경로, 그리고 공유 `recordVerdict`가 모두 호출한다 — proto 정보가 유실된다. behavior(커널 verdict)는 정확하나 관측 차원이 부족하다.

## 스코프

**포함**: `proto` label 추가 + emit 지점 proto 전달 + HELP/문서 마이그레이션 노트 + 유닛 테스트.
**제외(비목표)**: 커널 verdict 로직 변경(없음 — metric 차원만 추가), outcome label 값 변경, QUIC 필터 자체 변경, 별도 신규 metric 신설(label 추가로 충족).

## metric 계약 변경

- `ephemera_egress_sni_verdict_total` label set: `{outcome}` → **`{proto, outcome}`**.
- **`proto`** ∈ `tcp` | `udp` | `unknown`.
  - `tcp`: TCP:443 경로의 모든 판정.
  - `udp`: QUIC/UDP:443 경로의 모든 판정.
  - `unknown`: proto 분기(`sni_verdict.go`의 `pkt[9]==IPPROTO_UDP` 검사) **이전**의 no-payload fail-closed drop 한 곳(`a.Payload == nil`) 전용. NFQUEUE가 payload 없이 준 degenerate 케이스라 tcp/udp를 판별할 수 없다. 실무상 거의 발화하지 않으나 "모든 커널 verdict을 센다"는 카운터 불변을 지키기 위해 정직하게 별도 값으로 센다.
- **`outcome`** 불변: `allowed` | `denied` | `dropped`.
- HELP 문자열에 proto 차원과 `unknown` 의미를 반영.

## 코드 변경 (`cmd/goose-daemon/`)

### `metrics_handler.go`
- `sniVerdictTotal` `NewCounterVec` 등록에 label `"proto"` 추가 → `"proto", "outcome"` 순.
- `IncSNIVerdict(outcome string)` → **`IncSNIVerdict(proto, outcome string)`**; 본문 `WithLabelValues(proto, outcome)`.
- proto 상수 정의(매직 스트링 제거): `protoTCP = "tcp"`, `protoUDP = "udp"`, `protoUnknown = "unknown"`.
- doc comment/HELP 갱신(proto 차원 + unknown 설명 + 마이그레이션 노트).

### `sni_verdict.go`
- `recordVerdict(entry, d)` → **`recordVerdict(entry, d, proto string)`**; 내부 `IncSNIVerdict("allowed"/"denied")` 두 곳이 `IncSNIVerdict(proto, ...)`로.
- 호출자 proto 전달:
  - `applyVerdict`(TCP, `:636`) → `recordVerdict(entry, d, protoTCP)`.
  - `applyVerdictUDP`(QUIC, `:662`) → `recordVerdict(entry, d, protoUDP)`.
- pre-classify drop 지점 proto 전달:
  - `:488`(no-payload, proto 분기 이전) → `IncSNIVerdict(protoUnknown, "dropped")`.
  - `:502`, `:512`(TCP hook) → `IncSNIVerdict(protoTCP, "dropped")`.
  - `:590`, `:598`, `:607`(handleUDPQUIC) → `IncSNIVerdict(protoUDP, "dropped")`.

(주의: 위 라인 번호는 착수 시점 기준 앵커일 뿐, 구현은 내용 앵커로 편집한다.)

## 하위호환 / 마이그레이션

- **additive label**: `sum without(proto)(ephemera_egress_sni_verdict_total)` 는 기존 total과 동일. `sum()` 계열 쿼리는 무영향.
- **영향**: 단일 outcome series를 그리던 대시보드 패널은 이제 `{proto="tcp"}`+`{proto="udp"}`(+드물게 `unknown`)로 분리된다 → 단일 series를 기대하던 패널은 `sum by(outcome)` 또는 proto 선택 필요.
- 마이그레이션 노트를 metric HELP + `docs/operations/observability.md` metric 목록에 1줄 반영.

## 테스트

- `metrics_handler_test.go`: `IncSNIVerdict`의 새 arity(proto, outcome)로 갱신. proto별 series가 독립 증가하는지(`tcp`/`udp`/`unknown` × outcome) 단언.
- `sni_verdict_test.go`: `recordVerdict`가 proto를 받아 올바른 series를 올리는지(tcp allowed/denied, udp allowed/denied) + pre-classify drop 경로의 proto 귀속(가능한 유닛 범위에서). 기존 metric 단언을 새 label로 갱신.
- `egress_sni_incomplete` drop이 여전히 카운트되지 않는(early return) 회귀 유지.

## 검증

- `go test ./cmd/goose-daemon/... -race` — 신규/갱신 테스트 green.
- `go build ./... && go vet ./...`, `gofmt -l` empty.
- 커널 verdict behavior 무변경(metric 차원만 추가) — 최종 리뷰에서 재확인.

## 잔여 / 후속

- `unknown` 값은 degenerate NFQUEUE 케이스 전용 — 발화 빈도가 유의미하면 원인(payload copy 설정) 조사. 소소.
