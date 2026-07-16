# egress SNI verdict metric — proto label 분리 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ephemera_egress_sni_verdict_total` 카운터에 `proto`(tcp|udp|unknown) label을 추가해 TCP:443과 QUIC/UDP:443 판정을 구분한다. 커널 verdict 로직은 바꾸지 않는다(metric 차원만 추가).

**Architecture:** `IncSNIVerdict`에 proto 인자를 추가하고, 공유 `recordVerdict`에 proto를 스레딩한다. TCP 경로(`applyVerdict`, hook의 pre-classify drop)는 `tcp`, QUIC 경로(`applyVerdictUDP`, `handleUDPQUIC`)는 `udp`, proto 분기 이전의 no-payload drop은 `unknown`을 전달한다. Task 1은 코드+테스트(원자적 — 시그니처 변경이 두 파일과 테스트에 동시 전파), Task 2는 문서.

**Tech Stack:** Go, `cmd/goose-daemon` daemon, custom `internal/metrics`(CounterVec, label을 **등록 순서대로** 렌더 — `formatLabels`).

## Global Constraints

- **커널 verdict behavior 변경 금지** — `SetVerdictWithConnMark`/`setDrop`/`setDropNoRST` 호출과 그 조건은 그대로. 이 작업은 metric 차원만 추가한다.
- **label 등록 순서 = `"proto", "outcome"`** — custom metrics 패키지는 등록 순서대로 렌더하므로, 노출 문자열은 `{proto="...",outcome="..."}`(proto 먼저)다. 모든 테스트 기대 문자열·문서 예시는 이 순서를 따른다.
- **proto 값**: `tcp` | `udp` | `unknown`. `unknown`은 proto 분기 이전 no-payload drop 한 곳 전용.
- **`egress_sni_incomplete` drop은 여전히 미카운트**(recordVerdict의 early return 유지) — 회귀 금지.
- **nil-safe 유지** — `IncSNIVerdict`는 nil `*daemonMetrics`/nil CounterVec에서 panic 안 함.
- Go TAB 들여쓰기, `gofmt -l` empty. 커밋 트레일러 없음.
- 브랜치: `feature/egress-sni-metric-proto-label` (이미 생성, spec `a3c44f5`).

## File Structure

| 파일 | 책임 | Task |
|------|------|------|
| `cmd/goose-daemon/metrics_handler.go` | CounterVec에 proto label + `IncSNIVerdict(proto, outcome)` | 1 |
| `cmd/goose-daemon/sni_verdict.go` | proto 상수 정의 + `recordVerdict` proto 스레딩 + emit 8곳 proto 귀속 | 1 |
| `cmd/goose-daemon/metrics_handler_test.go` | 새 arity + proto-분리 series 단언 | 1 |
| `cmd/goose-daemon/sni_verdict_test.go` | `recordVerdict` 호출 새 arity + proto 단언 | 1 |
| `docs/operations/observability.md` | metric family 목록에 egress SNI verdict(+proto) 추가 | 2 |
| `docs/operations/runbook.md` | 예시 출력 + "공유" 서술 갱신 | 2 |

---

## Task 1: proto label 추가 + 스레딩 + 테스트

**Files:**
- Modify: `cmd/goose-daemon/metrics_handler.go` (field 주석, CounterVec 등록, `IncSNIVerdict`)
- Modify: `cmd/goose-daemon/sni_verdict.go` (proto 상수, `recordVerdict`, `applyVerdict`, `applyVerdictUDP`, pre-classify drop 6곳)
- Test: `cmd/goose-daemon/metrics_handler_test.go`, `cmd/goose-daemon/sni_verdict_test.go`

**Interfaces:**
- Produces: `IncSNIVerdict(proto, outcome string)`, `recordVerdict(entry sniRegistryEntry, d sniDecision, proto string)`, 상수 `protoTCP="tcp"` / `protoUDP="udp"` / `protoUnknown="unknown"`.

**중요(비유일 라인):** `sni_verdict.go`에는 `l.metrics.IncSNIVerdict("dropped")`가 여러 곳에 있고, 그중 3곳은 뒤에 `// pre-classify infra drop (distinct from policy denial)` 주석이 붙는다. bare 문자열은 commented 라인의 substring이므로 **replace_all 금지** — 아래 각 지점을 제시된 주변 블록째로 개별 치환한다.

- [ ] **Step 1: Write the failing proto-separation test**

`cmd/goose-daemon/metrics_handler_test.go`의 `TestMetrics_HandlerExposesSNIVerdictTotal` 바로 뒤(현재 L143 `}` 다음)에 신규 테스트를 추가한다. 이 테스트는 proto가 series를 분리함을 증명한다(이 작업의 핵심):

```go
func TestMetrics_SNIVerdictSeparatesByProto(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.IncSNIVerdict(protoTCP, "denied")
	cp.metrics.IncSNIVerdict(protoUDP, "denied")
	cp.metrics.IncSNIVerdict(protoUDP, "denied")
	cp.metrics.IncSNIVerdict(protoUnknown, "dropped")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	// proto is registered first, so it renders first: {proto="...",outcome="..."}.
	wantLines := []string{
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 1`,
		`ephemera_egress_sni_verdict_total{proto="udp",outcome="denied"} 2`,
		`ephemera_egress_sni_verdict_total{proto="unknown",outcome="dropped"} 1`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails (compile error)**

Run: `go test ./cmd/goose-daemon/ -run TestMetrics_SNIVerdictSeparatesByProto`
Expected: FAIL — compile error `undefined: protoTCP` (and `IncSNIVerdict` arity mismatch once constants exist).

- [ ] **Step 3: Define proto constants in `sni_verdict.go`**

`cmd/goose-daemon/sni_verdict.go`에서 `const sniReassemblerMaxFlows = 4096`(현재 L40 부근) 정의 바로 아래에 추가한다:

```go
// proto label values for the egress SNI verdict metric
// (ephemera_egress_sni_verdict_total). tcp/udp mark the two dispatch branches;
// unknown is only the pre-branch no-payload drop, where the transport cannot be
// determined (NFQUEUE delivered no packet bytes).
const (
	protoTCP     = "tcp"
	protoUDP     = "udp"
	protoUnknown = "unknown"
)
```

- [ ] **Step 4: Add the proto label to the CounterVec and `IncSNIVerdict`**

In `cmd/goose-daemon/metrics_handler.go`:

(a) field 주석 (현재 L40) 교체:

old:
```go
	sniVerdictTotal   *metrics.CounterVec // outcome=allowed|denied|dropped (egress SNI filter, Task 6)
```
new:
```go
	sniVerdictTotal   *metrics.CounterVec // proto=tcp|udp|unknown, outcome=allowed|denied|dropped (egress SNI filter)
```

(b) 등록 (현재 L117-121) 교체:

old:
```go
		sniVerdictTotal: r.NewCounterVec(
			"ephemera_egress_sni_verdict_total",
			"Total :443 SNI verdicts by outcome (allowed|denied|dropped; dropped = pre-classify infra fail-closed).",
			"outcome",
		),
```
new:
```go
		sniVerdictTotal: r.NewCounterVec(
			"ephemera_egress_sni_verdict_total",
			"Total :443 SNI verdicts by proto (tcp|udp|unknown) and outcome (allowed|denied|dropped; dropped = pre-classify infra fail-closed; unknown proto = no-payload drop before the tcp/udp branch).",
			"proto", "outcome",
		),
```

(c) `IncSNIVerdict` (현재 L229-244) 교체 — doc comment 첫 문장 + 시그니처 + body:

old:
```go
// IncSNIVerdict records one :443 SNI verdict by outcome. The classify path emits
// "allowed" (accept+mark) and "denied" (policy deny) via recordVerdict; the Start
// hook's pre-classify fail-closed sites (no payload, unparsable packet,
// unregistered source) emit "dropped" so the counter reflects every kernel
// verdict, not just policy decisions. Called regardless of tenant availability or
// audit-append success/failure — the counter is a content-free signal (outcome
// only, never SNI/VMID/tenant) so it carries no redaction risk and must never be
// gated on the audit write.
func (m *daemonMetrics) IncSNIVerdict(outcome string) {
	if m == nil {
		return
	}
	if m.sniVerdictTotal != nil {
		m.sniVerdictTotal.WithLabelValues(outcome).Inc()
	}
}
```
new:
```go
// IncSNIVerdict records one :443 SNI verdict by proto (tcp|udp|unknown) and
// outcome. The classify path emits "allowed" (accept+mark) and "denied" (policy
// deny) via recordVerdict; the Start hook's pre-classify fail-closed sites (no
// payload, unparsable packet, unregistered source) emit "dropped" so the counter
// reflects every kernel verdict, not just policy decisions. proto is "unknown"
// only for the no-payload drop that precedes the tcp/udp branch. Called
// regardless of tenant availability or audit-append success/failure — the counter
// is a content-free signal (proto/outcome only, never SNI/VMID/tenant) so it
// carries no redaction risk and must never be gated on the audit write.
func (m *daemonMetrics) IncSNIVerdict(proto, outcome string) {
	if m == nil {
		return
	}
	if m.sniVerdictTotal != nil {
		m.sniVerdictTotal.WithLabelValues(proto, outcome).Inc()
	}
}
```

- [ ] **Step 5: Thread proto through `recordVerdict` and its two callers**

In `cmd/goose-daemon/sni_verdict.go`:

(a) `recordVerdict` 시그니처 (현재 L678):

old:
```go
func (l *sniVerdictLoop) recordVerdict(entry sniRegistryEntry, d sniDecision) {
```
new:
```go
func (l *sniVerdictLoop) recordVerdict(entry sniRegistryEntry, d sniDecision, proto string) {
```

(b) `recordVerdict` 내부 emit 두 곳:

old:
```go
	case sniAcceptMark:
		l.metrics.IncSNIVerdict("allowed")
```
new:
```go
	case sniAcceptMark:
		l.metrics.IncSNIVerdict(proto, "allowed")
```

old:
```go
		l.metrics.IncSNIVerdict("denied")
		if d.Reason == "egress_sni_denied" {
```
new:
```go
		l.metrics.IncSNIVerdict(proto, "denied")
		if d.Reason == "egress_sni_denied" {
```

(c) TCP 호출자 `applyVerdict` (현재 L636):

old:
```go
		l.setDrop(nf, id, t)
	}
	l.recordVerdict(entry, d)
}

// applyVerdictUDP is applyVerdict's UDP/QUIC counterpart:
```
new:
```go
		l.setDrop(nf, id, t)
	}
	l.recordVerdict(entry, d, protoTCP)
}

// applyVerdictUDP is applyVerdict's UDP/QUIC counterpart:
```

(d) QUIC 호출자 `applyVerdictUDP` (현재 L662):

old:
```go
	case sniDrop:
		l.setDropNoRST(nf, id)
	}
	l.recordVerdict(entry, d)
}
```
new:
```go
	case sniDrop:
		l.setDropNoRST(nf, id)
	}
	l.recordVerdict(entry, d, protoUDP)
}
```

- [ ] **Step 6: Attribute proto at the 6 pre-classify drop sites (edit each block individually — NOT replace_all)**

In `cmd/goose-daemon/sni_verdict.go`, replace each block:

**(1) no-payload → `protoUnknown`:**

old:
```go
		if a.Payload == nil {
			// No packet bytes to inspect -> cannot verify -> fail closed.
			l.setDrop(nf, id, ipv4TCP{})
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```
new:
```go
		if a.Payload == nil {
			// No packet bytes to inspect -> cannot verify -> fail closed. Proto is
			// unknown here: this precedes the tcp/udp branch, so we cannot tell which.
			l.setDrop(nf, id, ipv4TCP{})
			l.metrics.IncSNIVerdict(protoUnknown, "dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```

**(2) unparsable TCP → `protoTCP`:**

old:
```go
		t, perr := parseIPv4TCP(pkt)
		if perr != nil {
			slog.Debug("sni verdict: unparsable packet, fail-closed drop", "err", perr)
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```
new:
```go
		t, perr := parseIPv4TCP(pkt)
		if perr != nil {
			slog.Debug("sni verdict: unparsable packet, fail-closed drop", "err", perr)
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict(protoTCP, "dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```

**(3) unregistered TCP → `protoTCP`:**

old:
```go
		entry, ok := l.resolveEntry(srcIP)
		if !ok {
			// Defense in depth: the iptables dispatch rule is already scoped by
			// -s guestIP, but an unregistered source still fails closed here.
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict("dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```
new:
```go
		entry, ok := l.resolveEntry(srcIP)
		if !ok {
			// Defense in depth: the iptables dispatch rule is already scoped by
			// -s guestIP, but an unregistered source still fails closed here.
			l.setDrop(nf, id, t)
			l.metrics.IncSNIVerdict(protoTCP, "dropped") // pre-classify infra drop (distinct from policy denial)
			return 0
		}
```

**(4) unparsable UDP → `protoUDP`:**

old:
```go
	u, perr := parseIPv4UDP(pkt)
	if perr != nil {
		slog.Debug("sni verdict: unparsable udp packet, fail-closed drop", "err", perr)
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
```
new:
```go
	u, perr := parseIPv4UDP(pkt)
	if perr != nil {
		slog.Debug("sni verdict: unparsable udp packet, fail-closed drop", "err", perr)
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict(protoUDP, "dropped")
		return
	}
```

**(5) wrong dport UDP → `protoUDP`:**

old:
```go
	if u.dport != 443 {
		// Defense in depth: the iptables dispatch rule is expected to already
		// scope this queue to dport 443, but fail closed here too rather than
		// trust that blindly.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
```
new:
```go
	if u.dport != 443 {
		// Defense in depth: the iptables dispatch rule is expected to already
		// scope this queue to dport 443, but fail closed here too rather than
		// trust that blindly.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict(protoUDP, "dropped")
		return
	}
```

**(6) unregistered UDP → `protoUDP`:**

old:
```go
	srcIP := u.srcIP.String()
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		// Defense in depth: the iptables dispatch rule is already scoped by
		// -s guestIP, but an unregistered source still fails closed here.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict("dropped")
		return
	}
```
new:
```go
	srcIP := u.srcIP.String()
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		// Defense in depth: the iptables dispatch rule is already scoped by
		// -s guestIP, but an unregistered source still fails closed here.
		l.setDropNoRST(nf, id)
		l.metrics.IncSNIVerdict(protoUDP, "dropped")
		return
	}
```

- [ ] **Step 7: Update existing tests to the new arity + label order**

In `cmd/goose-daemon/metrics_handler_test.go`, `TestMetrics_HandlerExposesSNIVerdictTotal`:

old:
```go
	cp.metrics.IncSNIVerdict("allowed")
	cp.metrics.IncSNIVerdict("allowed")
	cp.metrics.IncSNIVerdict("denied")
```
new:
```go
	cp.metrics.IncSNIVerdict(protoTCP, "allowed")
	cp.metrics.IncSNIVerdict(protoTCP, "allowed")
	cp.metrics.IncSNIVerdict(protoTCP, "denied")
```

old:
```go
	wantLines := []string{
		`ephemera_egress_sni_verdict_total{outcome="allowed"} 2`,
		`ephemera_egress_sni_verdict_total{outcome="denied"} 1`,
	}
```
new:
```go
	wantLines := []string{
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} 2`,
		`ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 1`,
	}
```

In `TestMetrics_IncSNIVerdictNilMetricsSafe`:

old:
```go
	m.IncSNIVerdict("allowed") // must not panic on nil receiver
```
new:
```go
	m.IncSNIVerdict(protoTCP, "allowed") // must not panic on nil receiver
```

In `cmd/goose-daemon/sni_verdict_test.go`, add `protoTCP` as the third arg to every `recordVerdict(...)` call in these tests (9 call sites at the block starting `// --- Task 6: recordVerdict / auditDeny`): each `l.recordVerdict(entry, sniDecision{...})` → `l.recordVerdict(entry, sniDecision{...}, protoTCP)`, and the three `l.recordVerdict(sniRegistryEntry{...}, sniDecision{...})` in `TestSNIRecordVerdictMetricAlwaysIncrements` → append `, protoTCP`. Then update that test's assertions:

old:
```go
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{outcome="denied"} 2`) {
		t.Fatalf("expected denied=2 regardless of tenant/audit outcome, got:\n%s", out)
	}
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{outcome="allowed"} 1`) {
		t.Fatalf("expected allowed=1, got:\n%s", out)
	}
```
new:
```go
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 2`) {
		t.Fatalf("expected denied=2 regardless of tenant/audit outcome, got:\n%s", out)
	}
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} 1`) {
		t.Fatalf("expected allowed=1, got:\n%s", out)
	}
```

(The `TestSNIRecordVerdict*` tests that pass `nil` metrics or only check audit still just need the extra `protoTCP` arg to compile; their audit assertions are unchanged.)

- [ ] **Step 8: Run the full daemon test suite under race**

Run: `go test ./cmd/goose-daemon/... -race`
Expected: PASS — `TestMetrics_SNIVerdictSeparatesByProto` and all updated tests green. The `egress_sni_incomplete` early-return behavior is unchanged (no counter increment) — existing coverage stays green.

- [ ] **Step 9: build + vet + gofmt**

Run: `go build ./... && go vet ./cmd/goose-daemon/... && gofmt -l cmd/goose-daemon/`
Expected: build OK, vet clean, `gofmt -l` prints nothing.

- [ ] **Step 10: Commit**

```bash
git add cmd/goose-daemon/metrics_handler.go cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/metrics_handler_test.go cmd/goose-daemon/sni_verdict_test.go
git commit -m "feat(metrics): split egress SNI verdict counter by proto (tcp/udp/unknown)"
```

---

## Task 2: 문서 갱신 (observability.md + runbook.md)

**Files:**
- Modify: `docs/operations/observability.md` (metric family 목록)
- Modify: `docs/operations/runbook.md` (예시 출력 + "공유" 서술)

**Interfaces:** 없음(문서만). Task 1의 계약(proto label, 렌더 순서 proto,outcome)을 반영.

- [ ] **Step 1: observability.md — metric family 목록에 항목 추가**

`docs/operations/observability.md`의 metric family 목록에서 `ephemera_cp_token_propagated_total` 줄(현재 L212) 바로 아래에 한 줄 추가한다.

old:
```
- `ephemera_cp_token_propagated_total{outcome="ok|fail"}`
- `ephemera_cleanup_failure_total`
```
new:
```
- `ephemera_cp_token_propagated_total{outcome="ok|fail"}`
- `ephemera_egress_sni_verdict_total{proto="tcp|udp|unknown",outcome="allowed|denied|dropped"}` —
  egress SNI 필터의 :443 판정. `proto`로 TCP:443과 QUIC/UDP:443을 구분한다(`unknown`은
  proto 분기 이전 no-payload drop 전용). additive label이라 `sum without(proto)(...)`가
  기존 total과 같다 — 단일 outcome series를 그리던 패널은 이제 proto별로 분리된다.
- `ephemera_cleanup_failure_total`
```

- [ ] **Step 2: runbook.md — 예시 출력 갱신**

`docs/operations/runbook.md`의 metric 예시(현재 L310-312):

old:
```
curl -s http://127.0.0.1:3000/metrics | grep ephemera_egress_sni_verdict_total
# ephemera_egress_sni_verdict_total{outcome="allowed"} N
# ephemera_egress_sni_verdict_total{outcome="denied"} N
```
new:
```
curl -s http://127.0.0.1:3000/metrics | grep ephemera_egress_sni_verdict_total
# ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} N
# ephemera_egress_sni_verdict_total{proto="udp",outcome="denied"} N
```

- [ ] **Step 3: runbook.md — "공유" 서술 갱신**

`docs/operations/runbook.md`의 metric/audit 서술(현재 L384-386):

old:
```
**metric/audit**: TCP/UDP는 `ephemera_egress_sni_verdict_total`을
공유한다(proto별 label 분리는 v1 비목표, 후속 후보). `egress_sni_denied`
audit도 TCP/UDP 동일 스키마를 재사용한다.
```
new:
```
**metric/audit**: TCP/UDP는 `ephemera_egress_sni_verdict_total`을
`proto="tcp|udp"` label로 구분한다(`unknown`은 proto 판별 전 no-payload
drop 전용). `egress_sni_denied` audit도 TCP/UDP 동일 스키마를 재사용한다.
```

- [ ] **Step 4: 잔여 스테일 확인**

Run:
```bash
grep -rn "proto별 label 분리는 v1 비목표\|verdict_total{outcome=\"" docs/operations/runbook.md docs/operations/observability.md
```
Expected: 빈 출력(옛 "공유/후속 후보" 서술과 proto 없는 예시가 모두 갱신됨).

- [ ] **Step 5: Commit**

```bash
git add docs/operations/observability.md docs/operations/runbook.md
git commit -m "docs(metrics): document egress SNI verdict proto label + migration note"
```

---

## 최종 검증 (모든 태스크 후)

- [ ] `go test ./cmd/goose-daemon/... -race` — green.
- [ ] `go build ./... && go vet ./...` — clean. `gofmt -l cmd/goose-daemon/` — empty.
- [ ] Task 2 Step 4 grep — 빈 출력.
- [ ] 최종 whole-branch 리뷰: 커널 verdict 로직 무변경(metric 차원만), proto 귀속 정확(tcp/udp/unknown), 렌더 순서 proto,outcome, `egress_sni_incomplete` 미카운트 유지, nil-safe 유지.
