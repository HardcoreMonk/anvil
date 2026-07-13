# Egress Transparent SNI Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. TDD 순서 엄수: 실패 테스트 전문 작성 → 실패 확인 → 최소 구현 → 통과 확인 → 커밋.

**Goal:** anvil host가 guest의 첫 :443 ClientHello를 in-process NFQUEUE verdict 루프로 파싱해 SNI를 profile의 `allow_sni` allowlist(default-deny, `*.` wildcard+exact)와 대조하고, 허용이면 conntrack mark 후 커널 fast-path로, 아니면 DROP+RST로 강제한다. 신뢰 워크로드의 의도된 :443 egress를 강제하고 이탈을 감사하되, in-guest 루트의 SNI-spoof/fronting/ECH 완전 봉쇄는 명시적 잔여 위험으로 계약한다.

**Architecture:** 설계 확정본 [`docs/superpowers/specs/2026-07-13-egress-sni-filter-design.md`](../specs/2026-07-13-egress-sni-filter-design.md) (df049eb, OQ1~8 전부 권고안대로 확정). SNI 층은 기존 default-deny `profile` 모델(`egress_policy.go` `planProfileEgressCommands`)에 **추가(additive)**되는 :443 전용 도메인-정밀 allow 경로다. dispatch/fast-path 규칙은 기존 iptables(-nft) exec + `commandEgressEnforcer` rollback 기계(`api.go:1938-2043`)를 그대로 재사용하고, 진짜 신규는 goose-daemon 프로세스 안 goroutine으로 도는 verdict 루프 하나다(별도 systemd unit 없음 — anvil "단일 바이너리 + in-process Manager" 패턴).

**Tech Stack:** Go 1.25 (module `ephemera`, toolchain go1.26.2). 신규 의존 **딱 하나**: `github.com/florianl/go-nfqueue` (c-binding-free pure-Go netlink NFQUEUE, MIT, `mdlayher/netlink` 경유 — `mdlayher/*`는 go.sum에 이미 부분 등장). conntrack mark는 라이브러리의 `SetVerdictWithConnMark`로 커널에 직접 기록(별도 conntrack 라이브러리·mangle CONNMARK 규칙 불필요). bash KVM e2e.

## Global Constraints

- 설계 계약 원문: 위 spec — §비목표(TLS 종단/MITM, ECH 완전대응, QUIC/UDP:443 L7, HTTP Host 검사, SNI↔cert↔IP 교차검증, native `nft` 전면 재작성, SNI 스케줄러 capability 축 신설)를 절대 침범하지 않는다.
- **default-deny + additive**: `allow_sni`는 CIDR allowlist를 **대체하지 않는다**. `allow_sni` 비면 :443 dispatch 규칙을 아예 달지 않아 기존 CIDR/DNS/base-REJECT 동작 그대로(하위호환). `allow_sni` 있으면 그 profile의 :443은 매칭 SNI만 통과, 나머지 default-deny.
- **fail-closed + preflight (OQ1)**: `--queue-bypass`를 **쓰지 않는다** — 리스너 없으면 커널이 큐 패킷을 DROP(=fail-closed). spawn 전 NFQUEUE 가용성/verdict 루프 healthy를 preflight 체크해 미지원이면 `allow_sni` profile spawn을 **거부**(조용한 under-enforce보다 낫다). verdict 루프 사망 시 :443 차단이 계약된 동작.
  - **주의(spec 내부 불일치 해소)**: spec §메커니즘 line 230 dispatch 예시는 `--queue-bypass`를 달았으나 이는 fail-OPEN이라 확정된 OQ1(fail-closed)과 모순. spec line 233이 "정확한 mark/규칙 순서는 plan에서 확정"이라 명시했으므로 이 플랜은 `--queue-bypass`를 **제거**하는 것으로 확정한다.
- **conntrack fast-path (steady-state 커널 경로)**: NFQUEUE로 가는 건 승인 전 :443 흐름의 핸드셰이크+ClientHello 세그먼트뿐. 승인 후 같은 흐름은 `-m connmark --mark <approved> -j ACCEPT` fast-path로 커널이 처리 — 대량 전송이 슬로우패스에 적체되면 안 된다(e2e 성능 스모크).
- **anti-spoof 전제**: verdict/규칙은 `-s guestIP`로 VM을 식별하므로 L2 anti-spoof(`antispoof.go`)가 source IP를 핀함을 전제한다. anti-spoof degrade 시 SNI 강제도 약화됨을 문서에 명시.
- **redaction**: SNI 도메인은 secret이 아니라 로깅 가능(CIDR/host와 동급 목적지 힌트). 그러나 토큰/authorization/Bearer는 어떤 로그·에러·감사 표면에도 절대 금지(`api.go:2593` `record.Error="[redacted]"` 선례 유지). audit는 `{ts,vmID,tenant,sni,reason:egress_sni_denied}`만.
- **신규 go.mod 의존 최소**: `go-nfqueue` 외 신규 direct 의존 금지. conntrack/RST/파서는 이 라이브러리 + 표준 라이브러리(`net`, `golang.org/x/sys` 기존 의존)로만.
- **검증**: 각 태스크 `go test -race` 포함. 순수 파서/매처/스키마/명령생성은 root 없이 유닛으로 검증. verdict 루프의 netlink 바인딩·conntrack mark·RST 주입·dispatch 규칙 실효는 root+netfilter 필요라 유닛 불가 → **KVM e2e(Task 7)가 실검**. 이 분담을 어기고 verdict 루프를 유닛에서 "통과"시키지 말 것.
- 커밋: **git trailer 금지**(anvil 브랜치 컨벤션 — Co-Authored-By 넣지 말 것). 작은 단위로 자주 커밋.
- main 직접 push 금지 — 브랜치 `feature/egress-sni-filter`(현재 체크아웃) + PR 경로. **자체 머지 금지**(머지는 사용자 승인으로만).
- 워커 파견 시 모든 Bash는 `cd /data/projects/claude-zone/anvil-wt-egress &&`로 시작, 커밋 전 `git branch --show-current` 확인(main이면 BLOCKED).

## 확정 상수 (태스크 간 공유 — 문자열 테스트가 이 값에 고정)

```go
// cmd/goose-daemon/egress_policy.go
const sniQueueNumDefault = 88          // NFQUEUE queue-num; env ANVIL_SNI_QUEUE_NUM override
const sniApprovedConnmark = "0x534e49" // conntrack mark for approved :443 flows ("SNI" in ASCII)
```

- queue-num: 단일 큐. 다중 VM은 verdict 루프가 패킷 src IP → per-VM matcher 레지스트리로 구분(spec §경계 사례 "단일 큐 + conntrack/소스 IP 매핑", 큐 고갈 방지).
- connmark: anvil은 현재 conntrack mark를 쓰지 않으므로 exact-value 매치로 충분(mask 불필요). `SetVerdictWithConnMark(id, NfAccept, 0x534e49)`가 흐름 conntrack 엔트리에 기록 → 이후 패킷이 `-m connmark --mark 0x534e49`로 fast-path.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/network/sni/matcher.go` (신규) | `allow_sni` 컴파일 매처: exact set + `*.` wildcard suffix, case-insensitive (Task 1) |
| `internal/network/sni/matcher_test.go` (신규) | 매처 유닛 (exact/wildcard/case-fold/빈 매처) |
| `internal/network/sni/parser.go` (신규) | TLS ClientHello → SNI 순수 파서 + 세그먼트 Reassembler (Task 2) |
| `internal/network/sni/parser_test.go` (신규) | 파서 유닛: 정상·SNI부재·malformed·멀티세그먼트·ECH outer (실 ClientHello 바이트) |
| `cmd/goose-daemon/egress_policy.go` (수정) | `AllowSNI` 필드, `validateEgressSNI`(wildcard-aware), `planProfileEgressCommands`에 NFQUEUE dispatch+fast-path 블록(CIDR 위/REJECT 아래), 상수 (Task 1·3) |
| `cmd/goose-daemon/egress_policy_test.go` (수정) | allow_sni 검증 회귀, dispatch/fast-path 명령 문자열, allow_sni 빈 profile은 dispatch 미생성 |
| `cmd/goose-daemon/sni_verdict.go` (신규) | in-process verdict 루프, per-VM matcher 레지스트리, `decide()` 순수 라우팅, conntrack mark, RST 주입, preflight capability, metric 훅 (Task 4·5·6) |
| `cmd/goose-daemon/sni_verdict_test.go` (신규) | 레지스트리 register/deregister, `decide()` 라우팅(핸드셰이크 passthrough/allow/deny/미등록 fail-closed), preflight 판정 — root 불필요 부분만 |
| `cmd/goose-daemon/api.go` (수정) | `applyEgressPolicy`/`ApplyWithProfile`에 tenant 전달 + SNI 레지스트리 register(apply)/deregister(cleanup), preflight 거부, daemon init에서 verdict 루프 기동 (Task 4·5·6) |
| `cmd/goose-daemon/api_test.go` (수정) | apply/cleanup 대칭에 SNI 규칙 `-I`↔`-D` 포함 |
| `cmd/goose-daemon/metrics_handler.go` (수정) | `sniVerdictTotal *metrics.CounterVec` (label: outcome=allowed\|denied), profile 단위 집계 (Task 6) |
| `internal/anvilmcp/tenant_policy.go` (수정) | `RuntimeAuditRecord`에 optional `SNI string json:"sni,omitempty"` 추가 (Task 6) |
| `internal/anvilmcp/tenant_policy_test.go` (수정) | audit SNI 필드 round-trip |
| `go.mod` / `go.sum` (수정) | `github.com/florianl/go-nfqueue` direct 의존 추가 (Task 4) |
| `scripts/anvil-egress-sni-e2e.sh` (신규) | KVM e2e: 실 VM이 allow_sni 도메인 :443 도달·미허용 차단·감사 기록 (Task 7) |
| `docs/adr/0002-egress-sni-transparent-filter.md` (신규), `docs/ADR_INDEX.md`, `docs/operations/security-policy.md`, `docs/architecture/multi-tenant-roadmap.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `CONTEXT.md`, `RELEASE_NOTES.md`, `docs/operations/runbook.md`, `docs/operations/2026-07-13-egress-sni-handoff.md` (수정/신규) | ADR 신설, 위협모델·잔여위험·계약 문서화, egress.json 스키마 갱신, 반복 이월 항목 해소 (Task 8) |

**Interfaces 계약 전체 요약** (각 태스크 Interfaces 블록이 원문):

```go
// internal/network/sni/matcher.go (Task 1) — package sni
type Matcher struct { /* exact map[string]struct{} + wildcard []string suffix */ }
func NewMatcher(patterns []string) (*Matcher, error) // "*." 접두 1개만 허용, 그 외 '*' 에러
func (m *Matcher) Match(serverName string) bool       // ASCII lowercase 정규화, "*." = 1개 이상 좌측 라벨
func (m *Matcher) Empty() bool

// internal/network/sni/parser.go (Task 2) — package sni
var ErrNoSNI = errors.New("clienthello has no server_name")
var ErrIncomplete = errors.New("clienthello incomplete, need more bytes")
func ParseClientHelloSNI(record []byte) (sni string, err error) // 재조립된 TLS record 바이트 → lowercased SNI
type Reassembler struct { /* bounded buffer (maxClientHelloBytes) */ }
func (r *Reassembler) Feed(segment []byte) (sni string, done bool, err error) // ErrIncomplete면 done=false 계속 누적

// cmd/goose-daemon/egress_policy.go (Task 1·3)
type egressProfile struct {
    AllowCIDRs []string `json:"allow_cidrs"`
    AllowHosts []string `json:"allow_hosts"` // legacy: -m string substring (deprecated)
    AllowSNI   []string `json:"allow_sni"`   // NEW: parsed ClientHello SNI, default-deny, opt-in
    DNSServers []string `json:"dns_servers"`
}
func validateEgressSNI(entry string) error // "*." 접두 허용 후 validateEgressHost 재사용
func planProfileEgressCommands(vmID, guestIP string, profile egressProfile) ([]egressCommand, error) // SNI 블록 추가

// cmd/goose-daemon/sni_verdict.go (Task 4·5·6)
type sniRegistryEntry struct { VMID, TenantID, Profile string; Matcher *sni.Matcher }
type sniVerdictLoop struct { /* registry map[guestIP]sniRegistryEntry (RWMutex), queue, metrics, auditPath */ }
func newSNIVerdictLoop(queueNum int, auditPath string, metrics *daemonMetrics) *sniVerdictLoop
func (l *sniVerdictLoop) Register(guestIP string, e sniRegistryEntry)
func (l *sniVerdictLoop) Deregister(guestIP string)
func (l *sniVerdictLoop) Start(ctx context.Context) error // netlink bind; 실패 시 error (preflight)
func (l *sniVerdictLoop) Ready() bool                      // preflight capability
type sniAction int // sniPassthrough, sniAcceptMark, sniDrop
type sniDecision struct { Action sniAction; SNI, Reason string }
func (l *sniVerdictLoop) decide(srcIP string, payload []byte) sniDecision // 순수: 레지스트리+파서+매처, 미등록/파싱실패=fail-closed drop

// cmd/goose-daemon/api.go (Task 5·6)
func (cp *ControlPlane) applyEgressPolicy(vmID, tapDevice, guestIP, policy, profile, tenantID string) error // tenantID 추가
func (e *commandEgressEnforcer) ApplyWithProfile(vmID, tapDevice, guestIP, policy, profileName, tenantID string) error // tenantID 추가 + SNI 레지스트리 배선 + preflight 거부

// internal/anvilmcp/tenant_policy.go (Task 6)
type RuntimeAuditRecord struct { /* 기존 필드 ... */; SNI string `json:"sni,omitempty"` }
```

---

### Task 1: `allow_sni` 스키마 + 검증 + wildcard 매처 (순수 유닛)

**Files:**
- Create: `internal/network/sni/matcher.go`, `internal/network/sni/matcher_test.go`
- Modify: `cmd/goose-daemon/egress_policy.go` (필드 + `validateEgressSNI` + `validateEgressProfile`에 loop), `cmd/goose-daemon/egress_policy_test.go`

**Interfaces:**
- Produces: `egressProfile.AllowSNI`, `validateEgressSNI`, `sni.Matcher`/`NewMatcher`/`Match`/`Empty` — Task 3(명령 생성)·Task 4(verdict decide)가 소비.
- Consumes: 기존 `validateEgressHost`(`egress_policy.go:76-91`, ASCII 영숫자/`.`/`-`) — `*.` 접두 스트립 후 재사용.
- 계약: `*.example.com`은 1개 이상 좌측 라벨(`a.example.com`, `a.b.example.com`) 매치, `example.com` 자기 자신은 **미매치**(exact 항목 별도 필요). exact 항목은 정확히 일치만. 임의 glob·중간 `*`·복수 `*` 거부(OQ5).

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/network/sni/matcher_test.go` (신규):

```go
package sni

import "testing"

func TestMatcherExactAndWildcard(t *testing.T) {
	m, err := NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	if err != nil {
		t.Fatalf("NewMatcher error = %v", err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"api.anthropic.com", true},         // exact
		{"API.Anthropic.COM", true},         // case-insensitive
		{"anthropic.com", false},            // exact does not match parent
		{"evil-api.anthropic.com", false},   // exact is not a suffix rule
		{"a.example.com", true},             // wildcard one label
		{"a.b.example.com", true},           // wildcard multiple labels
		{"example.com", false},              // "*." requires >=1 left label
		{"notexample.com", false},           // suffix must be on a label boundary
		{"", false},
	}
	for _, c := range cases {
		if got := m.Match(c.in); got != c.want {
			t.Fatalf("Match(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMatcherEmpty(t *testing.T) {
	m, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher(nil) error = %v", err)
	}
	if !m.Empty() {
		t.Fatal("Empty() = false, want true for no patterns")
	}
	if m.Match("api.anthropic.com") {
		t.Fatal("empty matcher matched a host (must default-deny)")
	}
}

func TestNewMatcherRejectsBadWildcard(t *testing.T) {
	for _, bad := range []string{"*", "*example.com", "a.*.com", "**.example.com", "ex*mple.com"} {
		if _, err := NewMatcher([]string{bad}); err == nil {
			t.Fatalf("NewMatcher(%q) error = nil, want rejection", bad)
		}
	}
}
```

`cmd/goose-daemon/egress_policy_test.go`에 추가:

```go
func TestValidateEgressSNIAcceptsWildcardAndExact(t *testing.T) {
	for _, ok := range []string{"api.anthropic.com", "*.example.com", "sub.domain-1.io"} {
		if err := validateEgressSNI(ok); err != nil {
			t.Fatalf("validateEgressSNI(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "*", "a.*.com", "under_score.com", "space host.com", "*.*.com"} {
		if err := validateEgressSNI(bad); err == nil {
			t.Fatalf("validateEgressSNI(%q) = nil, want error", bad)
		}
	}
}

func TestLoadEgressProfileParsesAllowSNI(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "sni")
	if err := os.MkdirAll(pd, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "egress.json"), []byte(
		`{"allow_sni":["api.anthropic.com","*.example.com"],"dns_servers":["1.1.1.1"]}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	profile, ok, err := loadEgressProfile(dir, "sni")
	if err != nil || !ok {
		t.Fatalf("loadEgressProfile ok=%v err=%v", ok, err)
	}
	if len(profile.AllowSNI) != 2 || profile.AllowSNI[0] != "api.anthropic.com" {
		t.Fatalf("AllowSNI = %#v", profile.AllowSNI)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/network/sni/ 2>&1 | head        # 컴파일 실패(패키지 부재)
go test ./cmd/goose-daemon/ -run 'AllowSNI|EgressSNI' 2>&1 | head
```
Expected: 컴파일 실패(`undefined: NewMatcher` / `validateEgressSNI` / `AllowSNI`).

- [ ] **Step 3: 최소 구현**

`internal/network/sni/matcher.go` (신규):

```go
// Package sni provides a pure-Go TLS ClientHello SNI parser and an allow-list
// matcher. Nothing here touches netlink or the kernel — it is unit-testable
// without root so the fail-closed decision logic can be validated cheaply.
package sni

import (
	"fmt"
	"strings"
)

type Matcher struct {
	exact    map[string]struct{}
	wildcard []string // stored as ".example.com" (the suffix a "*." pattern must match on a label boundary)
}

func NewMatcher(patterns []string) (*Matcher, error) {
	m := &Matcher{exact: make(map[string]struct{})}
	for _, raw := range patterns {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			return nil, fmt.Errorf("empty allow_sni pattern")
		}
		if strings.HasPrefix(p, "*.") {
			rest := p[2:]
			if rest == "" || strings.ContainsRune(rest, '*') {
				return nil, fmt.Errorf("invalid wildcard allow_sni %q", raw)
			}
			m.wildcard = append(m.wildcard, "."+rest)
			continue
		}
		if strings.ContainsRune(p, '*') {
			return nil, fmt.Errorf("allow_sni %q: '*' only allowed as a leading %q label", raw, "*.")
		}
		m.exact[p] = struct{}{}
	}
	return m, nil
}

func (m *Matcher) Empty() bool { return m == nil || (len(m.exact) == 0 && len(m.wildcard) == 0) }

func (m *Matcher) Match(serverName string) bool {
	if m == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(serverName))
	if name == "" {
		return false
	}
	if _, ok := m.exact[name]; ok {
		return true
	}
	for _, suffix := range m.wildcard {
		// "*.example.com" -> suffix ".example.com"; require >=1 left label so
		// the match lands on a label boundary and "example.com" itself is excluded.
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return true
		}
	}
	return false
}
```

`cmd/goose-daemon/egress_policy.go`: `egressProfile`에 `AllowSNI []string \`json:"allow_sni"\`` 추가(주석 위 Interfaces대로), `validateEgressProfile`에 loop 추가:

```go
	for _, entry := range profile.AllowSNI {
		if err := validateEgressSNI(entry); err != nil {
			return err
		}
	}
```

그리고 `validateEgressSNI`:

```go
func validateEgressSNI(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("allow_sni entries must be non-empty")
	}
	host := entry
	if strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if strings.ContainsRune(host, '*') {
		return fmt.Errorf("allow_sni entry %q: '*' only allowed as a leading %q label", entry, "*.")
	}
	// host now has no '*'; reuse the ASCII domain-charset validator.
	return validateEgressHost(host)
}
```

(주의: `validateEgressHost` 에러 문자열이 `allow_hosts`를 언급하나 재사용해도 무방 — 별도 메시지가 필요하면 host 검증을 `validateEgressHost`에서 분리하되 이 태스크 범위 밖. 그대로 재사용하고 Self-Review에 기록.)

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./internal/network/sni/ ./cmd/goose-daemon/ -run 'Matcher|EgressSNI|AllowSNI' -v 2>&1 | tail -30
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /data/projects/claude-zone/anvil-wt-egress && git branch --show-current
git add internal/network/sni/matcher.go internal/network/sni/matcher_test.go cmd/goose-daemon/egress_policy.go cmd/goose-daemon/egress_policy_test.go
git commit -m "feat(egress): allow_sni schema, wildcard-aware validation, SNI matcher"
```

---

### Task 2: TLS ClientHello SNI 파서 + 세그먼트 재조립 (순수 유닛)

**Files:**
- Create: `internal/network/sni/parser.go`, `internal/network/sni/parser_test.go`

**Interfaces:**
- Produces: `ParseClientHelloSNI`, `Reassembler`, `ErrNoSNI`, `ErrIncomplete` — Task 4 `decide()`가 소비.
- 계약: record-layer 바이트(하나 이상의 TCP 세그먼트가 이미 이어붙은 상태)를 받아 `server_name` extension의 host_name을 lowercased 반환. 바이트 부족이면 `ErrIncomplete`(Reassembler가 계속 누적), SNI 부재/파싱불가면 `ErrNoSNI`/기타 error(호출측 fail-closed drop). ECH `encrypted_client_hello` extension이 있어도 outer(cleartext) `server_name`을 그대로 추출 — outer가 allowlist에 없으면 deny되는 것이 계약. `maxClientHelloBytes`(예: 16384) 초과 누적은 malformed로 종료(무한 버퍼 방지).

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/network/sni/parser_test.go` (신규). 실 ClientHello 바이트는 helper로 조립(컴파일 가능·유지보수 용이):

```go
package sni

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// buildClientHello assembles a minimal but wire-valid TLS 1.2/1.3 record carrying
// a ClientHello. If sni != "" a server_name extension is included. If ech is true
// a (dummy) encrypted_client_hello extension is appended alongside the cleartext SNI.
func buildClientHello(sni string, ech bool) []byte {
	ext := &bytes.Buffer{}
	if sni != "" {
		name := []byte(sni)
		sn := &bytes.Buffer{}
		sn.WriteByte(0x00) // name_type = host_name
		binary.Write(sn, binary.BigEndian, uint16(len(name)))
		sn.Write(name)
		list := &bytes.Buffer{}
		binary.Write(list, binary.BigEndian, uint16(sn.Len()))
		list.Write(sn.Bytes())
		binary.Write(ext, binary.BigEndian, uint16(0x0000)) // ext_type server_name
		binary.Write(ext, binary.BigEndian, uint16(list.Len()))
		ext.Write(list.Bytes())
	}
	if ech {
		binary.Write(ext, binary.BigEndian, uint16(0xfe0d)) // encrypted_client_hello
		binary.Write(ext, binary.BigEndian, uint16(4))
		ext.Write([]byte{0x00, 0x01, 0x02, 0x03})
	}

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})       // client_version TLS1.2
	body.Write(make([]byte, 32))         // random
	body.WriteByte(0x00)                 // session_id len 0
	body.Write([]byte{0x00, 0x02, 0x13, 0x01}) // cipher_suites: len2 + TLS_AES_128_GCM_SHA256
	body.Write([]byte{0x01, 0x00})       // compression: len1 + null
	binary.Write(body, binary.BigEndian, uint16(ext.Len()))
	body.Write(ext.Bytes())

	hs := &bytes.Buffer{}
	hs.WriteByte(0x01) // handshake type client_hello
	l := body.Len()
	hs.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)}) // uint24 length
	hs.Write(body.Bytes())

	rec := &bytes.Buffer{}
	rec.WriteByte(0x16) // content_type handshake
	rec.Write([]byte{0x03, 0x01})
	binary.Write(rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestParseClientHelloSNI(t *testing.T) {
	sni, err := ParseClientHelloSNI(buildClientHello("api.anthropic.com", false))
	if err != nil || sni != "api.anthropic.com" {
		t.Fatalf("normal: sni=%q err=%v", sni, err)
	}
	sni, err = ParseClientHelloSNI(buildClientHello("API.Example.COM", false))
	if err != nil || sni != "api.example.com" {
		t.Fatalf("case-fold: sni=%q err=%v", sni, err)
	}
}

func TestParseClientHelloNoSNI(t *testing.T) {
	if _, err := ParseClientHelloSNI(buildClientHello("", false)); !errors.Is(err, ErrNoSNI) {
		t.Fatalf("no-SNI err = %v, want ErrNoSNI", err)
	}
}

func TestParseClientHelloECHOuterExtracted(t *testing.T) {
	// ECH decoy: cleartext outer SNI still present; parser returns it so the
	// matcher can deny it. (anvil does not defeat ECH — unparseable SNI = deny.)
	sni, err := ParseClientHelloSNI(buildClientHello("cloudflare-ech.com", true))
	if err != nil || sni != "cloudflare-ech.com" {
		t.Fatalf("ech outer: sni=%q err=%v", sni, err)
	}
}

func TestParseClientHelloMalformed(t *testing.T) {
	full := buildClientHello("api.anthropic.com", false)
	// Not a handshake record.
	bad := append([]byte(nil), full...)
	bad[0] = 0x17 // application_data
	if _, err := ParseClientHelloSNI(bad); err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("non-handshake err = %v, want hard error", err)
	}
	// Truncated mid-record -> incomplete.
	if _, err := ParseClientHelloSNI(full[:12]); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("truncated err = %v, want ErrIncomplete", err)
	}
}

func TestReassemblerMultiSegment(t *testing.T) {
	full := buildClientHello("split.example.com", false)
	r := &Reassembler{}
	// Feed byte-by-byte in three chunks; only the final Feed completes.
	chunks := [][]byte{full[:5], full[5 : len(full)-3], full[len(full)-3:]}
	var got string
	for i, c := range chunks {
		sni, done, err := r.Feed(c)
		if err != nil {
			t.Fatalf("chunk %d Feed err = %v", i, err)
		}
		if done {
			got = sni
		}
	}
	if got != "split.example.com" {
		t.Fatalf("reassembled sni = %q", got)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/network/sni/ -run 'Parse|Reassembler' 2>&1 | head
```
Expected: 컴파일 실패(`undefined: ParseClientHelloSNI`).

- [ ] **Step 3: 최소 구현**

`internal/network/sni/parser.go` (신규). 모든 가변 길이 필드를 경계 검사하며 순차 파싱:

```go
package sni

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNoSNI     = errors.New("clienthello has no server_name")
	ErrIncomplete = errors.New("clienthello incomplete, need more bytes")
)

const maxClientHelloBytes = 16384 // TLS record max; bound the reassembly buffer

// ParseClientHelloSNI parses reassembled TLS record bytes and returns the
// lowercased server_name. ErrIncomplete means the caller should feed more bytes;
// any other error (including ErrNoSNI) is terminal and the caller must fail closed.
func ParseClientHelloSNI(b []byte) (string, error) {
	// TLS record header: type(1) version(2) length(2)
	if len(b) < 5 {
		return "", ErrIncomplete
	}
	if b[0] != 0x16 {
		return "", fmt.Errorf("not a handshake record (type 0x%02x)", b[0])
	}
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	if len(b) < 5+recLen {
		return "", ErrIncomplete
	}
	hs := b[5 : 5+recLen]
	// Handshake header: msg_type(1) length(3)
	if len(hs) < 4 {
		return "", ErrIncomplete
	}
	if hs[0] != 0x01 {
		return "", fmt.Errorf("not a client_hello (msg_type 0x%02x)", hs[0])
	}
	hsLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	body := hs[4:]
	if len(body) < hsLen {
		return "", ErrIncomplete
	}
	body = body[:hsLen]

	// client_version(2) random(32) then variable fields.
	p := 2 + 32
	if len(body) < p+1 {
		return "", fmt.Errorf("clienthello truncated before session_id")
	}
	sidLen := int(body[p])
	p += 1 + sidLen
	if len(body) < p+2 {
		return "", fmt.Errorf("clienthello truncated before cipher_suites")
	}
	csLen := int(binary.BigEndian.Uint16(body[p:]))
	p += 2 + csLen
	if len(body) < p+1 {
		return "", fmt.Errorf("clienthello truncated before compression")
	}
	compLen := int(body[p])
	p += 1 + compLen
	if len(body) < p+2 {
		// No extensions at all -> no SNI.
		return "", ErrNoSNI
	}
	extTotal := int(binary.BigEndian.Uint16(body[p:]))
	p += 2
	if len(body) < p+extTotal {
		return "", fmt.Errorf("clienthello truncated extensions")
	}
	ext := body[p : p+extTotal]

	for len(ext) >= 4 {
		etype := binary.BigEndian.Uint16(ext[0:2])
		elen := int(binary.BigEndian.Uint16(ext[2:4]))
		if len(ext) < 4+elen {
			return "", fmt.Errorf("truncated extension 0x%04x", etype)
		}
		data := ext[4 : 4+elen]
		if etype == 0x0000 { // server_name
			return parseServerName(data)
		}
		ext = ext[4+elen:]
	}
	return "", ErrNoSNI
}

func parseServerName(data []byte) (string, error) {
	// server_name_list: list_len(2) then entries of name_type(1) name_len(2) name.
	if len(data) < 2 {
		return "", ErrNoSNI
	}
	listLen := int(binary.BigEndian.Uint16(data[0:2]))
	list := data[2:]
	if len(list) < listLen {
		return "", fmt.Errorf("truncated server_name_list")
	}
	list = list[:listLen]
	for len(list) >= 3 {
		nameType := list[0]
		nameLen := int(binary.BigEndian.Uint16(list[1:3]))
		if len(list) < 3+nameLen {
			return "", fmt.Errorf("truncated server_name entry")
		}
		if nameType == 0x00 { // host_name
			return strings.ToLower(string(list[3 : 3+nameLen])), nil
		}
		list = list[3+nameLen:]
	}
	return "", ErrNoSNI
}

// Reassembler accumulates TCP payload segments until a full ClientHello parses.
type Reassembler struct{ buf []byte }

func (r *Reassembler) Feed(segment []byte) (string, bool, error) {
	if len(r.buf)+len(segment) > maxClientHelloBytes {
		return "", false, fmt.Errorf("clienthello exceeds %d bytes", maxClientHelloBytes)
	}
	r.buf = append(r.buf, segment...)
	sni, err := ParseClientHelloSNI(r.buf)
	if errors.Is(err, ErrIncomplete) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sni, true, nil
}
```

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./internal/network/sni/ -v 2>&1 | tail -30
```
Expected: PASS (Task 1 매처 테스트 포함 전부).

- [ ] **Step 5: Commit**

```bash
git add internal/network/sni/parser.go internal/network/sni/parser_test.go
git commit -m "feat(egress): pure TLS ClientHello SNI parser with segment reassembly"
```

---

### Task 3: NFQUEUE dispatch + fast-path 명령 생성 + rollback 대칭

**Files:**
- Modify: `cmd/goose-daemon/egress_policy.go` (`planProfileEgressCommands`에 SNI 블록 + 상수), `cmd/goose-daemon/egress_policy_test.go`, `cmd/goose-daemon/api_test.go`

**Interfaces:**
- Produces: `planProfileEgressCommands`가 `len(AllowSNI)>0`일 때 dispatch+fast-path `egressCommand` 2개를 **CIDR 블록 위(=체인 상위), base REJECT 아래**에 배치. `AllowSNI` 비면 SNI 명령 미생성(CIDR-only 하위호환).
- Consumes: 기존 `egressCommand`(`egress_policy.go:24-27`), `commandEgressEnforcer.cleanupEgressCommands`(`api.go:2028-2043`)의 `-I`↔`-D` 역순 대칭. `!`·`-m connmark`·`-j NFQUEUE` args는 `-D`로 그대로 삭제됨.
- **규칙 순서 계약(체인 top→bottom, `-I` head-insert라 slice 역순)**: `host → CIDR → sni-fastpath → sni-dispatch → DNS-allow → DNS-deny → REJECT`. 따라서 slice에는 **DNS 블록 뒤·CIDR 블록 앞**에 `[dispatch, fastpath]` 순으로 append(역순 후 fastpath가 dispatch 위). CIDR가 SNI보다 상위 → :443 CIDR 핀은 SNI를 우회(명시 IP 신뢰, additive 계약).

- [ ] **Step 1: 실패하는 테스트 작성**

`cmd/goose-daemon/egress_policy_test.go`에 추가:

```go
func TestPlanProfileEgressCommandsEmitsSNIDispatch(t *testing.T) {
	commands, err := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{
		AllowSNI: []string{"api.anthropic.com", "*.example.com"},
	})
	if err != nil {
		t.Fatalf("planProfileEgressCommands error = %v", err)
	}
	joined := joinCommands(commands)
	for _, want := range []string{
		"iptables -I FORWARD -s 10.0.1.10 -p tcp --dport 443 -m connmark --mark 0x534e49 -j ACCEPT -m comment --comment anvil-egress-vm-1-sni-fastpath",
		"iptables -I FORWARD -s 10.0.1.10 -p tcp --dport 443 -m connmark ! --mark 0x534e49 -j NFQUEUE --queue-num 88 -m comment --comment anvil-egress-vm-1-sni-nfqueue",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestPlanProfileEgressCommandsNoSNIWhenEmpty(t *testing.T) {
	commands, err := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{
		AllowCIDRs: []string{"203.0.113.10/32"},
	})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(joinCommands(commands), "NFQUEUE") {
		t.Fatal("allow_sni empty must not emit NFQUEUE dispatch rule")
	}
}

func TestPlanProfileEgressCommandsCIDRAboveSNI(t *testing.T) {
	// Ordering contract: the emitted slice must place CIDR after the SNI block so
	// that after -I head-insert reversal the CIDR rule sits ABOVE the dispatch.
	commands, err := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{
		AllowCIDRs: []string{"203.0.113.10/32"},
		AllowSNI:   []string{"api.anthropic.com"},
	})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	joined := joinCommands(commands)
	dispatchIdx := strings.Index(joined, "-sni-nfqueue")
	cidrIdx := strings.Index(joined, "-cidr-0")
	if dispatchIdx < 0 || cidrIdx < 0 || !(dispatchIdx < cidrIdx) {
		t.Fatalf("expected sni-nfqueue before cidr-0 in slice order; dispatch=%d cidr=%d\n%s", dispatchIdx, cidrIdx, joined)
	}
}
```

`cmd/goose-daemon/api_test.go`: apply/cleanup 대칭에 SNI 규칙이 `-I`↔`-D`로 역순 삭제됨을 확인하는 테스트 추가(기존 `TestCommandEgressEnforcerProfileApplyFailureRollsBackAppliedRules` 패턴). `writeEgressProfileFixture`를 allow_sni 포함 fixture로 확장하거나 별도 fixture helper 추가 후, `Cleanup`이 dispatch/fastpath를 `-D`로 지우는지 명령 캡처로 단언.

- [ ] **Step 2: 실패 확인**

```bash
go test ./cmd/goose-daemon/ -run 'SNIDispatch|NoSNIWhenEmpty|CIDRAboveSNI' 2>&1 | head
```
Expected: FAIL — `commands missing "...sni-fastpath"`.

- [ ] **Step 3: 최소 구현**

`egress_policy.go` 상단에 상수 추가(§확정 상수). queue-num은 env override:

```go
func sniQueueNum() int {
	if v := strings.TrimSpace(os.Getenv("ANVIL_SNI_QUEUE_NUM")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 65535 {
			return n
		}
	}
	return sniQueueNumDefault
}
```

`planProfileEgressCommands`: DNS 블록 append **직후, CIDR 블록 loop 직전**에 삽입:

```go
	if len(profile.AllowSNI) > 0 {
		q := strconv.Itoa(sniQueueNum())
		commands = append(commands,
			// dispatch first in slice so it lands BELOW fastpath after -I reversal.
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "tcp", "--dport", "443", "-m", "connmark", "!", "--mark", sniApprovedConnmark, "-j", "NFQUEUE", "--queue-num", q, "-m", "comment", "--comment", prefix + "-sni-nfqueue"}},
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "tcp", "--dport", "443", "-m", "connmark", "--mark", sniApprovedConnmark, "-j", "ACCEPT", "-m", "comment", "--comment", prefix + "-sni-fastpath"}},
		)
	}
```

(`strconv` import 추가 필요.) CIDR/host loop은 그대로 뒤따른다.

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./cmd/goose-daemon/ -run 'Egress|SNI' -v 2>&1 | tail -40
```
Expected: PASS. 기존 `TestLoadEgressProfileAndPlanAllowlistCommands`·`...DNSAllowlist`·rollback 테스트 회귀 없음(SNI 블록은 allow_sni 없는 profile에 미영향).

- [ ] **Step 5: Commit**

```bash
git add cmd/goose-daemon/egress_policy.go cmd/goose-daemon/egress_policy_test.go cmd/goose-daemon/api_test.go
git commit -m "feat(egress): emit NFQUEUE dispatch + connmark fast-path for allow_sni profiles"
```

---

### Task 4: in-process verdict 루프 + conntrack mark + RST + fail-closed

**Files:**
- Create: `cmd/goose-daemon/sni_verdict.go`, `cmd/goose-daemon/sni_verdict_test.go`
- Modify: `go.mod`, `go.sum`

**분담 주의(브리프 필수):** NFQUEUE netlink 바인딩(`Start`)·`SetVerdictWithConnMark`·RST 원시소켓 주입·dispatch 규칙 실효는 **root+netfilter 필요라 유닛 불가 → Task 7 KVM e2e가 실검**. 이 태스크의 유닛은 **`decide()` 순수 라우팅 + 레지스트리 + fail-closed 판정**만 커버한다. `Start`/RST/커널 mark를 유닛에서 "통과"시키려 fake로 위장하지 말 것.

**Interfaces:**
- Produces: `sniVerdictLoop`(`Register`/`Deregister`/`Start`/`Ready`/`decide`), `sniRegistryEntry`, `sniDecision` — Task 5(preflight/배선)·Task 6(audit/metric)가 소비.
- Consumes: `sni.ParseClientHelloSNI`/`Reassembler`/`Matcher`(Task 1·2), `github.com/florianl/go-nfqueue`(신규), 기존 `daemonMetrics`(Task 6에서 필드 추가).
- `decide(srcIP, payload)` 계약: (a) 미등록 srcIP → `sniDrop`(fail-closed, reason `unregistered_source`); (b) payload에 TLS 애플리케이션 바이트 없음(핸드셰이크 SYN/ACK) → `sniPassthrough`(mark 없이 ACCEPT, 다음 미마크 패킷 재큐잉); (c) ClientHello 파싱 성공 & SNI ∈ matcher → `sniAcceptMark`; (d) 파싱 성공 & SNI ∉ → `sniDrop`(reason `egress_sni_denied`, SNI 기록); (e) 파싱 실패/SNI 부재/ECH-only decoy 미허용 → `sniDrop`(fail-closed).

- [ ] **Step 1: 실패하는 테스트 작성**

`cmd/goose-daemon/sni_verdict_test.go` (신규) — `decide()`만 검증(netlink 불필요). `buildClientHello`는 sni 패키지 테스트 helper라 접근 불가하므로, 여기서는 sni 패키지의 exported 파서로 만든 최소 바이트를 재사용하되, 간결하게 로컬 helper를 둔다(또는 `internal/network/sni`에 `BuildClientHelloForTest`를 export). **결정: sni 패키지에 test-only가 아닌 얇은 exported helper 대신, `sni_verdict_test.go`가 Task 2의 wire 포맷을 로컬 helper로 최소 재현**(파서와 결합 낮춤). 아래는 라우팅만 보므로 유효 ClientHello 하나면 충분:

```go
package main

import (
	"testing"

	"ephemera/internal/network/sni"
)

func mustHello(t *testing.T, name string) []byte {
	t.Helper()
	// Reuse the parser's own acceptance as an oracle: build via a tiny inline
	// encoder mirroring Task 2's buildClientHello (kept local to avoid exporting
	// test helpers). Implementation copies the byte layout from parser_test.go.
	b := encodeClientHelloSNI(name) // small local encoder in this _test.go file
	if got, err := sni.ParseClientHelloSNI(b); err != nil || got != name {
		t.Fatalf("oracle: built hello for %q parsed as %q err=%v", name, got, err)
	}
	return b
}

func TestSNIDecideRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p", Matcher: m})

	// allowed exact
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed exact -> %v (%s)", d.Action, d.Reason)
	}
	// allowed wildcard
	if d := l.decide("10.0.1.10", mustHello(t, "cdn.example.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed wildcard -> %v", d.Action)
	}
	// denied
	d := l.decide("10.0.1.10", mustHello(t, "evil.test"))
	if d.Action != sniDrop || d.Reason != "egress_sni_denied" || d.SNI != "evil.test" {
		t.Fatalf("denied -> action=%v reason=%q sni=%q", d.Action, d.Reason, d.SNI)
	}
	// handshake / no application payload -> passthrough
	if d := l.decide("10.0.1.10", []byte{}); d.Action != sniPassthrough {
		t.Fatalf("empty payload -> %v, want passthrough", d.Action)
	}
	// unregistered source -> fail-closed drop
	if d := l.decide("10.0.1.99", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatalf("unregistered -> %v, want drop", d.Action)
	}
	// malformed application payload -> fail-closed drop
	if d := l.decide("10.0.1.10", []byte{0x16, 0x03, 0x01, 0xff, 0xff, 0x01}); d.Action != sniDrop {
		t.Fatalf("malformed -> %v, want drop", d.Action)
	}
}

func TestSNIDeregisterFailsClosed(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})
	l.Deregister("10.0.1.10")
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatal("deregistered source must fail closed")
	}
}
```

(`encodeClientHelloSNI`는 이 `_test.go` 안에 Task 2 `buildClientHello`의 SNI-only 축약본으로 작성. handshake 패킷을 "payload 없음"으로 모델링하는 것은 단순화 — 실제 verdict 루프는 TCP payload 오프셋을 계산하지만, `decide`는 이미 payload 슬라이스를 받으므로 빈 슬라이스=핸드셰이크로 충분.)

- [ ] **Step 2: 실패 확인**

```bash
go test ./cmd/goose-daemon/ -run 'SNIDecide|SNIDeregister' 2>&1 | head
```
Expected: 컴파일 실패(`undefined: newSNIVerdictLoop`).

- [ ] **Step 3: 최소 구현**

먼저 의존 추가:

```bash
go get github.com/florianl/go-nfqueue@latest
go mod tidy
```
(pure-Go/MIT 확인. `go.mod` require에 direct로 올라오고 `mdlayher/netlink`가 transitive로 채워짐. `go mod tidy` 후 `git diff go.mod go.sum`으로 신규 direct 의존이 `go-nfqueue` 하나뿐임을 확인 — 아니면 BLOCKED 후 보고.)

`cmd/goose-daemon/sni_verdict.go` (신규). 순수 `decide` + netlink 글루 분리:

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"ephemera/internal/network/sni"

	nfqueue "github.com/florianl/go-nfqueue"
)

type sniAction int

const (
	sniPassthrough sniAction = iota // ACCEPT, no conntrack mark (handshake/non-ClientHello)
	sniAcceptMark                   // ACCEPT + set approved conntrack mark
	sniDrop                         // DROP (+ best-effort RST) — fail-closed
)

type sniDecision struct {
	Action sniAction
	SNI    string
	Reason string
}

type sniRegistryEntry struct {
	VMID     string
	TenantID string
	Profile  string
	Matcher  *sni.Matcher
}

type sniVerdictLoop struct {
	queueNum  int
	auditPath string
	metrics   *daemonMetrics

	mu       sync.RWMutex
	registry map[string]sniRegistryEntry // key: guest source IP
	ready    bool
}

func newSNIVerdictLoop(queueNum int, auditPath string, metrics *daemonMetrics) *sniVerdictLoop {
	return &sniVerdictLoop{queueNum: queueNum, auditPath: auditPath, metrics: metrics, registry: map[string]sniRegistryEntry{}}
}

func (l *sniVerdictLoop) Register(guestIP string, e sniRegistryEntry) {
	l.mu.Lock(); defer l.mu.Unlock()
	l.registry[guestIP] = e
}
func (l *sniVerdictLoop) Deregister(guestIP string) {
	l.mu.Lock(); defer l.mu.Unlock()
	delete(l.registry, guestIP)
}
func (l *sniVerdictLoop) Ready() bool { l.mu.RLock(); defer l.mu.RUnlock(); return l.ready }

// decide is the pure routing core (unit-tested without root). payload is the TCP
// application payload of one queued packet; srcIP is the packet source address.
func (l *sniVerdictLoop) decide(srcIP string, payload []byte) sniDecision {
	l.mu.RLock()
	entry, ok := l.registry[srcIP]
	l.mu.RUnlock()
	if !ok {
		return sniDecision{Action: sniDrop, Reason: "unregistered_source"}
	}
	if len(payload) == 0 {
		return sniDecision{Action: sniPassthrough} // handshake packet, let it through unmarked
	}
	name, err := sni.ParseClientHelloSNI(payload)
	if errors.Is(err, sni.ErrIncomplete) {
		// Single-segment path: incomplete ClientHello. Passthrough so the next
		// (still-unmarked) segment re-queues; Reassembler handling lives in Start.
		return sniDecision{Action: sniPassthrough}
	}
	if err != nil {
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed", SNI: ""} // fail-closed
	}
	if entry.Matcher.Match(name) {
		return sniDecision{Action: sniAcceptMark, SNI: name, Reason: "egress_sni_allowed"}
	}
	return sniDecision{Action: sniDrop, SNI: name, Reason: "egress_sni_denied"}
}
```

`Start(ctx)`: `nfqueue.Open(&nfqueue.Config{NfQueue: uint16(l.queueNum), Copymode: nfqueue.NfQnlCopyPacket, Flags: nfqueue.NfQaCfgFlagConntrack, ...})` → 실패 시 `l.ready=false` + error 반환(**preflight**). 성공 시 `l.ready=true`, `nf.RegisterWithErrorFunc(ctx, hook, errHook)` 등록. hook:
1. 패킷에서 IPv4 src, TCP payload 오프셋 추출(IHL·data offset). payload가 있으면 `sni.Reassembler`를 흐름별로 유지하거나(단일 패킷 ClientHello가 절대다수라 v1은 per-packet + `ErrIncomplete`→passthrough 재큐잉으로 처리, 멀티세그먼트 재조립은 Reassembler를 흐름키(src:sport)로 캐시 — 구현 시 bounded LRU). 
2. `d := l.decide(srcIP, payload)`:
   - `sniPassthrough` → `nf.SetVerdict(id, nfqueue.NfAccept)`
   - `sniAcceptMark` → `nf.SetVerdictWithConnMark(id, nfqueue.NfAccept, mustParseConnmark(sniApprovedConnmark))`; metric allowed++.
   - `sniDrop` → `nf.SetVerdict(id, nfqueue.NfDrop)`; best-effort `injectRST(packet)`; audit(Task 6) + metric denied++.
3. errHook: 로그(토큰 없음), 계속.

RST 주입 `injectRST`: 원시 IPv4 소켓(`unix.Socket(AF_INET, SOCK_RAW, IPPROTO_TCP)` + `IP_HDRINCL`)으로, src=원 목적지(server) IP:443, dst=guestIP:sport, seq=ClientHello TCP seg의 ack_seq, flags=RST 패킷을 guest로 주입해 빠른 실패 유도(OQ6). **best-effort** — 실패하면 조용한 DROP으로 degrade(guest는 타임아웃). 이 fallback을 코드 주석·문서에 명시.

fail-closed 확인: `Config`에 `--queue-bypass` 미설정(라이브러리 기본이 bypass 아님)이라 리스너 부재 시 커널이 DROP. `Start` 실패(=preflight 실패)면 `Ready()==false`.

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./cmd/goose-daemon/ -run 'SNIDecide|SNIDeregister' -v 2>&1 | tail -20
go build ./cmd/goose-daemon/   # netlink 글루 컴파일 확인
```
Expected: PASS + build OK. (`Start`/RST/커널 경로는 여기서 검증 못 함 — Task 7.)

- [ ] **Step 5: Commit**

```bash
git add cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/sni_verdict_test.go go.mod go.sum
git commit -m "feat(egress): in-process NFQUEUE SNI verdict loop (decide core, conntrack mark, RST, fail-closed)"
```

---

### Task 5: preflight capability 체크 + spawn 거부 배선

**Files:**
- Modify: `cmd/goose-daemon/api.go` (verdict 루프 init/기동, `applyEgressPolicy`/`ApplyWithProfile` tenant 인자 + 레지스트리 배선 + preflight 거부, Cleanup deregister), `cmd/goose-daemon/sni_verdict.go`(필요 시 helper), `cmd/goose-daemon/api_test.go`

**Interfaces:**
- Consumes: Task 4 `sniVerdictLoop`, 기존 `commandEgressEnforcer`, `applyEgressPolicy`(`api.go:2384`, 인터페이스 assertion `api.go:2388`), spawn(`api.go:1367`)·restore(`api.go:3490,3670`) 호출부.
- Produces: `allow_sni` profile spawn 시 verdict 루프 미준비면 error 반환(spawn 실패, fail-closed OQ1/OQ2). 준비되면 apply가 `Register(guestIP, entry)`, Cleanup이 `Deregister`.
- **배선**: `applyEgressPolicy`에 `tenantID` 인자 추가 → 3개 호출부 갱신(spawn `opts.TenantID`, restore ×2 `meta.TenantID` — 확인: `opts.TenantID`는 `api.go:1447/1503`, `meta.TenantID`는 `api.go:2109`에 존재). 인터페이스 assertion 시그니처(`api.go:2388-2390`)도 갱신. `ApplyWithProfile`이 profile 로드 후 `len(profile.AllowSNI)>0`이면 preflight.

- [ ] **Step 1: 실패하는 테스트 작성**

`cmd/goose-daemon/api_test.go`:

```go
func TestApplyWithProfileRefusesSNIWhenLoopNotReady(t *testing.T) {
	profileDir := t.TempDir()
	writeEgressSNIProfileFixture(t, profileDir, "sni") // {"allow_sni":["api.anthropic.com"]}
	loop := newSNIVerdictLoop(88, "", nil) // Ready()==false (never Start()ed)
	enforcer := &commandEgressEnforcer{
		profileDir: profileDir,
		sniLoop:    loop,
		run:        func(name string, args ...string) error { return nil },
	}
	err := enforcer.ApplyWithProfile("vm-1", "tap", "10.0.1.10", "profile", "sni", "t1")
	if err == nil {
		t.Fatal("ApplyWithProfile with allow_sni and no ready verdict loop = nil, want fail-closed refusal")
	}
	if !strings.Contains(err.Error(), "sni") {
		t.Fatalf("err = %v, want SNI capability refusal", err)
	}
	if _, ok := enforcer.rules["vm-1"]; ok {
		t.Fatal("refused spawn must not leave egress rule state")
	}
}

func TestApplyWithProfileRegistersAndDeregistersSNI(t *testing.T) {
	profileDir := t.TempDir()
	writeEgressSNIProfileFixture(t, profileDir, "sni")
	loop := newSNIVerdictLoop(88, "", nil)
	loop.ready = true // simulate a started loop for the registry wiring test
	enforcer := &commandEgressEnforcer{profileDir: profileDir, sniLoop: loop, run: func(string, ...string) error { return nil }}
	if err := enforcer.ApplyWithProfile("vm-1", "tap", "10.0.1.10", "profile", "sni", "t1"); err != nil {
		t.Fatalf("apply err = %v", err)
	}
	if d := loop.decide("10.0.1.10", nil); d.Action == sniDrop && d.Reason == "unregistered_source" {
		t.Fatal("apply did not register guest IP in verdict loop")
	}
	if err := enforcer.Cleanup("vm-1"); err != nil {
		t.Fatalf("cleanup err = %v", err)
	}
	if d := loop.decide("10.0.1.10", nil); !(d.Action == sniDrop && d.Reason == "unregistered_source") {
		t.Fatal("cleanup did not deregister guest IP")
	}
}
```

(`writeEgressSNIProfileFixture`는 기존 `writeEgressProfileFixture` 옆에 추가. `decide(ip, nil)`는 등록돼 있으면 `sniPassthrough`, 미등록이면 `sniDrop/unregistered_source` — 등록 여부의 관측 훅으로 사용.)

- [ ] **Step 2: 실패 확인**

```bash
go test ./cmd/goose-daemon/ -run 'RefusesSNI|RegistersAndDeregisters' 2>&1 | head
```
Expected: 컴파일 실패(`enforcer.sniLoop` 필드 부재).

- [ ] **Step 3: 최소 구현**

- `commandEgressEnforcer`에 `sniLoop *sniVerdictLoop` 필드 추가, `newCommandEgressEnforcer`가 세팅(daemon init에서 루프 생성·`Start(ctx)` 기동 후 주입 — `Start` 실패해도 데몬은 뜨되 `Ready()==false`).
- `ApplyWithProfile(vmID, tapDevice, guestIP, policy, profileName, tenantID string)`:
  - profile 로드 후 `if len(profile.AllowSNI) > 0`:
    - `if e.sniLoop == nil || !e.sniLoop.Ready() { return fmt.Errorf("egress profile %q requires SNI verdict loop but host lacks NFQUEUE capability (fail-closed)", profileName) }` — 규칙 적용 전에 거부(상태 미변경).
  - 규칙 적용 성공 후: `if len(profile.AllowSNI) > 0 { matcher, err := sni.NewMatcher(profile.AllowSNI); ...; e.sniLoop.Register(guestIP, sniRegistryEntry{VMID: vmID, TenantID: tenantID, Profile: profileName, Matcher: matcher}) }`. 등록 실패 시 이미 적용한 규칙 롤백(기존 cleanup 경로 재사용).
- `Cleanup(vmID)`: 기존 로직 + `rule.GuestIP`가 있으면 `e.sniLoop.Deregister(rule.GuestIP)`(sniLoop nil 가드).
- `Apply`(2-인자 wrapper)와 `applyEgressPolicy`, 인터페이스 assertion(`api.go:2388`)에 `tenantID` 추가. 3개 호출부(1367 spawn, 3490·3670 restore) 갱신.
- daemon 기동부: `newSNIVerdictLoop(sniQueueNum(), <audit path>, cp.metrics)` 생성 → goroutine `loop.Start(cp.ctx)`(또는 동기 bind 후 consume goroutine) → enforcer에 주입. audit path는 기존 데몬 audit 경로 규약을 따름(Task 6에서 확정).

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./cmd/goose-daemon/ -run 'Egress|SNI|Apply' -v 2>&1 | tail -40
go build ./...
```
Expected: PASS + build. 기존 enforcer 테스트(deny_all/allow_all/rollback) 회귀 없음 — `tenantID` 인자 추가에 맞춰 기존 테스트 호출부도 갱신 필요(컴파일러가 즉시 지적).

- [ ] **Step 5: Commit**

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/api_test.go
git commit -m "feat(egress): preflight fail-closed refusal + per-VM SNI registry wiring on apply/cleanup"
```

---

### Task 6: 감사(RuntimeAuditRecord SNI) + metric

**Files:**
- Modify: `internal/anvilmcp/tenant_policy.go`(`RuntimeAuditRecord`에 `SNI`), `internal/anvilmcp/tenant_policy_test.go`, `cmd/goose-daemon/metrics_handler.go`(`sniVerdictTotal`), `cmd/goose-daemon/sni_verdict.go`(deny 시 audit+metric emit)

**Interfaces:**
- Consumes: 기존 `AppendRuntimeAudit`(`tenant_policy.go:222`, tenant 필수 검증)·`RuntimeAuditRecord`, 기존 `daemonMetrics`(`metrics_handler.go:18`)·`metrics.CounterVec`.
- Produces: DROP 시 `{ts, vmID, tenant, sni, reason:egress_sni_denied}` 구조화 레코드(`ToolName="egress_sni_filter"`, `DaemonOperation="egress_sni_denied"`, `ResultCode="denied"`, `SNI=<domain>`) + `sniVerdictTotal` profile-단위 allowed/denied 카운터.
- **redaction 계약**: SNI 도메인만 기록. tenant 상관은 기존 규율(토큰/authorization 절대 금지). profile×domain cross-product 카운터 금지 — label은 `outcome`(+ profile)만.
- **주의(spec↔code seam)**: `RuntimeAuditRecord`는 adapter(`anvilmcp/tools.go`)가 쓰던 tenant-scoped 레코드이고 `AppendRuntimeAudit`는 **tenant 비어 있으면 검증 실패**한다. verdict 루프는 daemon 프로세스에서 돌고 tenant는 Task 5가 레지스트리로 스레드한다. tenant가 빈 VM이면 `AppendRuntimeAudit`가 거부하므로, **tenant 있을 때만 감사 파일 append, 없으면 redaction-safe `slog` 라인만** 남기고 metric은 항상 증가. 이 분기를 코드·문서에 명시(spec은 "패턴 재사용"만 요구, 정확한 tenant 가용성은 이 seam에서 해소).

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/anvilmcp/tenant_policy_test.go`:

```go
func TestRuntimeAuditRecordSNIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.jsonl")
	if err := AppendRuntimeAudit(p, RuntimeAuditRecord{
		TenantID: "t1", ToolName: "egress_sni_filter",
		DaemonOperation: "egress_sni_denied", ResultCode: "denied",
		VMID: "vm-1", SNI: "evil.test",
	}); err != nil {
		t.Fatalf("append err = %v", err)
	}
	recs, err := ReadRuntimeAudit(p)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read err=%v n=%d", err, len(recs))
	}
	if recs[0].SNI != "evil.test" || recs[0].DaemonOperation != "egress_sni_denied" {
		t.Fatalf("record = %+v", recs[0])
	}
}
```

metric: `metrics_handler.go`의 기존 collector 테스트가 있으면 그 스타일로 `sniVerdictTotal` 노출 회귀 추가(없으면 `/metrics` 렌더에 `ephemera_egress_sni_verdict_total` 라인이 나오는지 최소 단언).

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run SNIRoundTrip 2>&1 | head
```
Expected: 컴파일 실패(`RuntimeAuditRecord.SNI` 부재).

- [ ] **Step 3: 최소 구현**

- `tenant_policy.go` `RuntimeAuditRecord`에 `SNI string \`json:"sni,omitempty"\`` 추가(다른 필드 뒤). `AppendRuntimeAudit`는 SNI를 특별 검증하지 않음(optional). `ReadRuntimeAudit`는 JSON 언마샬이라 자동 지원.
- `metrics_handler.go`: `daemonMetrics`에 `sniVerdictTotal *metrics.CounterVec // outcome=allowed|denied`, `newDaemonMetrics`에서 `r.NewCounterVec("ephemera_egress_sni_verdict_total", "Total :443 SNI verdicts by outcome.", "outcome")`. `IncSNIVerdict(outcome string)` helper.
- `sni_verdict.go`: `sniDrop` & reason==`egress_sni_denied`일 때 → tenant 있으면 `anvilmcp.AppendRuntimeAudit(l.auditPath, RuntimeAuditRecord{TenantID: entry.TenantID, VMID: entry.VMID, ToolName: "egress_sni_filter", DaemonOperation: "egress_sni_denied", ResultCode: "denied", SNI: d.SNI})`, 없으면 `slog.Warn("egress sni denied", "vm_id", entry.VMID, "sni", d.SNI)`. metric: allowed→`IncSNIVerdict("allowed")`, denied/unparsed→`IncSNIVerdict("denied")`. (import cycle 주의: `cmd/goose-daemon`는 이미 `anvilmcp`에 의존하므로 신규 사이클 없음.)

- [ ] **Step 4: 통과 확인**

```bash
go test -race ./internal/anvilmcp/ ./cmd/goose-daemon/ -run 'SNI|Audit|Metric' -v 2>&1 | tail -30
go build ./...
```
Expected: PASS + build.

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/tenant_policy.go internal/anvilmcp/tenant_policy_test.go cmd/goose-daemon/metrics_handler.go cmd/goose-daemon/sni_verdict.go
git commit -m "feat(egress): SNI deny audit record + profile-level verdict metric"
```

---

### Task 7: KVM e2e (실 VM, 실 도메인)

**Files:**
- Create: `scripts/anvil-egress-sni-e2e.sh`

**Interfaces:**
- Consumes: Task 1-6 전체(빌드된 daemon), 기존 e2e 하니스 패턴(`scripts/vm-workload-e2e.sh` 헤더/`set -Eeuo pipefail`/`step()`/`ok()`/`fail()`/artifact dir/cleanup trap/exit-code 판정), `configs/profiles/` 아래 임시 allow_sni profile 주입(env `ANVIL_EGRESS_PROFILE_DIR`).
- Produces: 단독 KVM e2e 게이트(root+KVM 필요, 메인 `e2e_test.sh`와 별도). **판정은 exit code + 마지막 "passed" 라인만**(anvil e2e 규율).
- **실검 범위(브리프 분담)**: verdict 루프 netlink 바인딩·conntrack mark·RST·dispatch 규칙 실효를 실 커널에서 검증 — 유닛이 못 한 부분 전부.

- [ ] **Step 1: 스크립트 골격** — vm-workload-e2e 헤더 스타일(무엇을 증명/제외하는지 주석), `set -Eeuo pipefail`, helper, artifact dir, cleanup trap(임시 profile dir 삭제 + VM teardown + daemon 종료 + `iptables -S FORWARD | grep anvil-egress` 잔재 정리). 안정적 공개 TLS 엔드포인트 선정: 허용 `api.anthropic.com`(or `cloudflare.com`), 비허용 `example.org`. env: `ANVIL_SNI_E2E_ALLOW_DOMAIN`, `ANVIL_SNI_E2E_DENY_DOMAIN` override.

- [ ] **Step 2: Phase 0 — 셋업 단언.** 임시 profile dir에 `egress.json`(`{"allow_sni":["<allow>"],"dns_servers":["1.1.1.1","8.8.8.8"]}` — golden image resolv.conf `8.8.8.8`/`1.1.1.1` baked-in 정합, spec §Q5) 작성. `ANVIL_EGRESS_PROFILE_DIR` 지정하고 daemon 기동(verdict 루프 `Ready()` 확인 — `/metrics`에 `ephemera_egress_sni_verdict_total` 존재 또는 로그). allow_sni profile로 VM spawn 성공(preflight 통과 = NFQUEUE 가용 host).

- [ ] **Step 3: Phase 1 — 허용 도메인 도달.** guest에서 `<allow>:443` TLS 핸드셰이크 성공(vm-workload 방식으로 guest 안 `curl -sS --max-time 15 https://<allow>/` 또는 `openssl s_client -connect <allow>:443 -servername <allow>`). exit 0/핸드셰이크 성공 단언. `iptables -S FORWARD`에 해당 VM의 `-sni-fastpath`/`-sni-nfqueue`/`connmark 0x534e49` 규칙 존재 확인.

- [ ] **Step 4: Phase 2 — 비허용 도메인 차단.** guest에서 `<deny>:443` 시도 → **실패**(RST면 빠른 실패, DROP이면 타임아웃 — 둘 다 non-zero exit 허용). 판정: 연결 성립 안 됨(`curl` exit != 0 / `s_client` 핸드셰이크 미완).

- [ ] **Step 5: Phase 3 — 감사 기록.** daemon audit 파일(또는 `slog` 캡처)에 `egress_sni_denied` + `sni=<deny>` 레코드 존재 확인. **redaction spot-check**: audit/로그에 Bearer/token 문자열 부재(기존 e2e의 redaction grep 패턴 재사용).

- [ ] **Step 6: (선택) 성능 스모크.** 허용 도메인으로 수 MB 다운로드가 슬로우패스 적체 없이 완료(승인 후 fast-path 커널 경로 확인) — `ephemera_egress_sni_verdict_total{outcome=allowed}`가 흐름당 1 부근(패킷마다 아님)임을 관측.

- [ ] **Step 7: 실행 검증**

```bash
sudo -n bash scripts/anvil-egress-sni-e2e.sh; echo "exit=$?"
```
Expected: `exit=0` + 마지막 라인 `All egress SNI e2e steps passed ✓`. 실행 워크트리에 gitignored `configs/goose.yaml`·`goose-secrets.yaml`을 메인 checkout에서 복사(VM 생성 500 방지). sudo 실행 후 root 소유 잔재는 sudo rm 정리.

- [ ] **Step 8: Commit**

```bash
git add scripts/anvil-egress-sni-e2e.sh
git commit -m "test(e2e): egress SNI filter KVM e2e (allow reaches :443, deny blocked, audit recorded)"
```

---

### Task 8: 문서 — ADR 신설 + 계약/경계 문서 + 반복 이월 해소

**Files:**
- Create: `docs/adr/0002-egress-sni-transparent-filter.md`, `docs/operations/2026-07-13-egress-sni-handoff.md`
- Modify: `docs/ADR_INDEX.md`, `docs/operations/security-policy.md`, `docs/architecture/multi-tenant-roadmap.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `CONTEXT.md`, `RELEASE_NOTES.md`, `docs/operations/runbook.md`

**Interfaces:**
- Consumes: Task 1-7 완료 상태(구현 사실 기술).
- Produces: release 단계 zone 인벤토리 동기화 입력(handoff의 zone 연동 절).
- **신규 ADR 판단(브리프 요구)**: **필요 = YES.** `docs/adr/`에 현재 0001 1건뿐이고 egress/network 전용 ADR 부재. 이 slice는 substring→SNI semantics 전환·위협 모델·fail-closed 결정이라 behavior/contract 변경 → ADR-tier. 신규 `0002` 파일로 박고 `ADR_INDEX.md`에 row 추가.

- [ ] **Step 1: ADR 0002 신설.** 반드시 포함: (a) substring `-m string` → parsed SNI 전환 근거(spec §문제), (b) **위협 모델**(신뢰 golden-image 워크로드, in-scope=의도 egress 강제+감사 / out-of-scope=적대 in-guest 루트의 spoof/fronting/ECH 완전봉쇄), (c) **잔여 위험 계약 표**(ECH fail-closed deny, non-TLS는 CIDR만, UDP/QUIC:443 default-deny, SNI guest-asserted, domain-fronting 미탐지 — spec §Q3), (d) **fail-closed + preflight**(OQ1) 결정과 `--queue-bypass` 배제 근거, (e) in-process verdict(OQ3)·iptables-exec 재사용(OQ4)·`*.` wildcard(OQ5)·RST(OQ6)·ECH CIDR-fallback-only(OQ7)·allow_hosts deprecated-유지(OQ8), (f) additive 계약(CIDR 대체 아님, CIDR가 :443 SNI보다 상위). 상태 링크: spec + handoff.
- [ ] **Step 2: ADR_INDEX.md** — 0002 row 추가(관례 확인 후).
- [ ] **Step 3: security-policy.md:107-123** — Egress policy 절 갱신. `allow_hosts`를 **legacy/deprecated(substring, coarse)**로 표기하고 `allow_sni`(parsed ClientHello, default-deny, `*.` wildcard) 신설을 계약으로 기술. 위협 모델·잔여 위험(ECH/fronting/spoofing)·fail-closed·anti-spoof 전제 명시. "SNI gateway를 대체하지 않는다" 문장을 "SNI 필터가 도입됐으며 그 계약과 한계는 ADR-0002"로 갱신.
- [ ] **Step 4: multi-tenant-roadmap.md:294-300** 비목표 절 — SNI는 이제 in-scope, 단 full HTTP CONNECT/forward proxy·TLS 종단·QUIC L7는 여전히 비목표로 조정.
- [ ] **Step 5: PUBLIC_RELEASE_BOUNDARY.md** — egress 표면(`egress.json` 스키마, `EPHEMERA_/ANVIL_EGRESS_PROFILE_DIR` 고정 계약)에 `allow_sni` 필드 + `ANVIL_SNI_QUEUE_NUM` env + connmark/queue 상수 추가. `allow_hosts` deprecated 표기. NFQUEUE baseline 요구(OQ2) 명시.
- [ ] **Step 6: CONTEXT.md:478** — 반복 이월된 "egress L7/SNI hardening" 후속 후보를 구현 완료로 치환(kind·메커니즘·e2e·잔여위험 계약 요약).
- [ ] **Step 7: RELEASE_NOTES.md** — `:747`/`:860` 부근 반복 이월 항목 해소, 신규 기능 엔트리(allow_sni, fail-closed, ADR-0002 링크).
- [ ] **Step 8: runbook.md** — 운영 절차: (a) allow_sni profile 작성법(`*.` wildcard, dns_servers에 `8.8.8.8`/`1.1.1.1` 포함 필수 — golden image resolv.conf 정합), (b) NFQUEUE 미지원 host에서 spawn 거부(preflight) 관측·복구, (c) verdict 루프 사망 시 :443 차단(fail-closed) 진단, (d) deny 감사/metric 확인법(`ephemera_egress_sni_verdict_total`, audit `egress_sni_denied`), (e) ECH 엔드포인트는 CIDR fallback opt-in.
- [ ] **Step 9: handoff 신규 작성.** 무엇이 main에 있나 / 검증 증거(유닛+e2e) / Follow-Up(QUIC/UDP:443 SNI, allow_hosts 제거 시점 재검토, multi-queue per-VM 재검토, ECH inner 대응 불가 재확인). zone 연동: `docs/FOLLOWUP.md`·`ops/units.yaml`/`ops/projects.yaml` 갱신 필요 항목 명시(release 단계 입력).
- [ ] **Step 10: 상호 링크 검증 + Commit**

```bash
grep -rn "egress-sni\|allow_sni\|0002-egress" docs/ CONTEXT.md RELEASE_NOTES.md | grep -v Binary
git add docs/ CONTEXT.md RELEASE_NOTES.md
git commit -m "docs(egress): ADR-0002, security-policy/boundary/runbook contracts, allow_sni schema, handoff"
```

---

## 최종 검증 (전체 슬라이스)

- [ ] `go build ./... && go vet ./... && gofmt -l . | grep -v '^web/'` — clean
- [ ] `go test -race ./internal/... ./cmd/...` — 전부 PASS (특히 `internal/network/sni`, `cmd/goose-daemon`)
- [ ] `git diff main -- go.mod` — 신규 direct 의존이 `github.com/florianl/go-nfqueue` **하나뿐**임을 확인(그 외면 보고)
- [ ] `sudo -n bash scripts/anvil-egress-sni-e2e.sh` — exit 0 + "passed ✓"
- [ ] 기존 egress 회귀: `go test ./cmd/goose-daemon/ -run Egress` + 전체 KVM 게이트 `sudo bash e2e_test.sh` — exit code 판정(egress apply 경로를 만졌으므로 필수)
- [ ] secret-scan: `bash scripts/secret-scan.sh`(있는 그대로의 사용법 확인 후) — 신규 코드/로그/audit에 토큰 유출 없음, SNI 도메인만
- [ ] PR 생성(`feature/egress-sni-filter` → main). **자체 머지 금지** — 머지는 사용자 승인으로만.

## Self-Review 기록 (플랜 작성 시점)

**Spec 커버리지 (경계 사례 → 태스크 테스트 매핑, spec §경계 사례·§테스트 열거 대응):**
- Q1 SNI 추출 = userspace 파서(c) → Task 2 파서 유닛(정상/부재/malformed/멀티세그먼트/ECH outer).
- Q2 additive/default-deny → Task 3 `NoSNIWhenEmpty`(allow_sni 없으면 CIDR-only 유지) + `CIDRAboveSNI`(CIDR가 SNI 우회).
- Q3 실패·우회 계약 → Task 8 ADR-0002 잔여 위험 표(ECH/non-TLS/QUIC/spoof/fronting). 코드 강제 가능한 것만: ECH-only decoy 미허용 → Task 2 `ECHOuterExtracted`+Task 4 deny; 파싱불가 fail-closed → Task 2 malformed + Task 4 `malformed→drop`.
- Q4 스키마 `allow_sni` 신규 필드(재해석 아님)·wildcard·validateEgressHost 재사용 → Task 1 검증/매처 유닛 + `LoadEgressProfileParsesAllowSNI`.
- Q5 anti-spoof 전제/DNS 정합 → Task 8 문서(코드 강제 아님, 전제 계약) + e2e dns_servers `8.8.8.8`/`1.1.1.1`.
- Q6 감사/metric·redaction → Task 6 `SNIRoundTrip` + metric + Task 7 redaction spot-check.
- 경계 사례: allow_sni 빈+CIDR만 → Task 3 `NoSNIWhenEmpty`; ClientHello 없는 :443/0-RTT → Task 2 no-SNI + Task 4 fail-closed drop; verdict 루프 crash/부재 → Task 5 preflight 거부 유닛 + fail-closed(커널 DROP) e2e; restore idempotent → 3개 apply 호출부(spawn+restore×2)가 동일 `planProfileEgressCommands` 경유(Task 3·5); 다중 VM 단일 큐+src IP → Task 4 `decide` src-IP 라우팅 유닛.
- 테스트 §유닛 4항목: allow_sni 파싱/검증(Task1), planProfileEgressCommands 문자열+빈 profile 미생성(Task3), verdict TLS 파서 5종(Task2), enforcer apply/cleanup 대칭(Task3 api_test). §KVM e2e 3항목(허용/비허용/감사) → Task 7 Phase 1-3. §성능 스모크 → Task 7 Step 6.
- 문서 반영 5항목(security-policy/roadmap/egress.json 스키마/전용 ADR/CONTEXT·RELEASE 이월) → Task 8 전부.
- 비목표 침범 없음: TLS 종단/MITM·ECH 완전대응·QUIC L7·HTTP Host·SNI↔cert↔IP·native nft 재작성·SNI 스케줄러 capability 축 — 모두 미구현, ADR/문서에 비목표로 재확인.

**유닛 vs e2e 분담(브리프 필수):** 순수 파서(Task2)·매처(Task1)·스키마검증(Task1)·명령생성(Task3)·`decide` 라우팅+fail-closed(Task4)·preflight/레지스트리 배선(Task5)·audit/metric(Task6)은 root 없이 유닛으로 실검. verdict 루프 netlink 바인딩·`SetVerdictWithConnMark` 커널 mark·RST 원시소켓 주입·dispatch 규칙 실효·steady-state fast-path는 root+netfilter 필요 → Task 7 KVM e2e가 유일 실검. 플랜은 이 경계를 Task 4·7 헤더에 명시하고, verdict 글루를 유닛에서 위장 통과시키지 말라고 못 박음.

**Spec 보정/주의(리뷰어가 기각 가능하도록 분리):**
1. **`--queue-bypass` 배제**: spec §메커니즘 line 230은 `--queue-bypass`를 달았으나 이는 fail-OPEN이라 확정 OQ1(fail-closed)과 모순. line 233 "mark/규칙 순서 plan에서 확정" 위임에 근거해 제거로 확정.
2. **conntrack mark 수단**: spec은 "conntrack mark"만 언급. `go-nfqueue`의 `SetVerdictWithConnMark`가 흐름 conntrack 엔트리에 직접 mark → 별도 conntrack 라이브러리·mangle CONNMARK save/restore 규칙 불필요(신규 의존 최소 준수). 커널 실효는 e2e 검증.
3. **audit tenant seam**: `RuntimeAuditRecord`/`AppendRuntimeAudit`는 tenant 필수. verdict 루프는 daemon-side라 tenant를 Task 5가 레지스트리로 스레드하되, tenant 빈 VM은 append 거부 → slog fallback + metric은 항상 증가. spec은 "패턴 재사용"만 요구, 정확한 tenant 가용성은 이 seam에서 해소.
4. **CIDR vs SNI 순서**: spec은 additive만 규정, :443 CIDR와 SNI 공존 시 우선순위 미명시 → 플랜이 "CIDR가 SNI 위(명시 IP 신뢰 우회)"로 확정(Task 3 `CIDRAboveSNI` 테스트로 고정).
5. **`validateEgressHost` 에러 메시지**: 재사용 시 문자열이 `allow_hosts`를 언급(부정확). 재사용 유지, 필요 시 후속 정리(범위 밖).

**Type consistency:** `sniRegistryEntry`/`sniDecision`/`sniAction`/`newSNIVerdictLoop`/`decide`/`Register`/`Deregister`/`Ready` 시그니처가 Task4↔5↔6 및 테스트에서 동일. `applyEgressPolicy`/`ApplyWithProfile`의 `tenantID` 추가가 인터페이스 assertion(`api.go:2388`)+3개 호출부(1367/3490/3670)+기존 enforcer 테스트 호출부에 전파됨을 확인(컴파일러가 누락 즉시 지적). `sni.Matcher`/`ParseClientHelloSNI` 시그니처가 Task1·2 정의와 Task4 소비에서 일치.

**알려진 리스크:** (a) `go-nfqueue` 버전별 `SetVerdictWithConnMark`/`Config` 필드명·`NfQaCfgFlagConntrack` 상수명 차이 — `go get` 후 실제 API로 맞춤(Task4 Step3, e2e가 실검). (b) RST 원시소켓 주입은 host NAT 경로에서 fiddly — best-effort, 실패 시 조용한 DROP degrade(문서화). (c) 멀티세그먼트 ClientHello 재조립을 흐름키 캐시로 할지 per-packet `ErrIncomplete`→passthrough 재큐잉으로 할지 — v1은 후자(단일 세그먼트가 절대다수), Reassembler는 파서 계약으로 준비만. (d) e2e 실 공개 도메인(`api.anthropic.com`/`example.org`)의 가용성 — env override 제공.
