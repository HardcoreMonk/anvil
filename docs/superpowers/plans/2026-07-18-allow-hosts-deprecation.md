# allow_hosts deprecation cycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 레거시 `allow_hosts` egress 필드에 런타임 deprecation 경고를 추가하고(현재 deprecation은 문서에만), 다음 tagged anvil 릴리즈 제거를 결정 기록한다. 실제 필드 제거는 이번 스코프 밖.

**Architecture:** `loadEgressProfile`가 `allow_hosts`가 있는 profile을 로드할 때 `slog.Warn` deprecation 경고를 1회 남긴다(동작 불변 — `allow_hosts`는 여전히 적용). 나머지는 마이그레이션·결정 문서.

**Tech Stack:** Go 1.25, `cmd/goose-daemon` daemon, 표준 `log/slog`.

## Global Constraints

- **동작 불변** — `allow_hosts`는 여전히 파싱·적용된다. 이 작업은 경고(side-effect) + 문서만 추가한다. 필드/apply/validate/cleanup 제거는 **비목표**(다음 릴리즈).
- **경고는 content-free** — profile 이름 + allow_hosts 개수만. host 값·token 없음.
- **제거 시점 = 다음 tagged anvil 릴리즈**(이 경고가 실린 릴리즈의 다음). 제거 시 loud fail-closed(`DisallowUnknownFields`) — 별도 작업.
- Go TAB 들여쓰기, `gofmt -l` empty. 커밋 트레일러 없음.
- 브랜치: `feature/allow-hosts-deprecation-warning` (이미 생성, spec `834c035`).

## File Structure

| 파일 | 책임 | Task |
|------|------|------|
| `cmd/goose-daemon/egress_policy.go` | `loadEgressProfile`에 allow_hosts 런타임 deprecation 경고 + `log/slog` import | 1 |
| `cmd/goose-daemon/egress_policy_test.go` | 경고 발화/미발화 + allow_hosts 동작 불변 회귀 | 1 |
| `docs/operations/runbook.md` | allow_hosts deprecation + 마이그레이션(allow_sni/allow_cidrs) | 2 |
| `docs/adr/0002-egress-sni-transparent-filter.md` | OQ8 deprecation cycle 결정 기록 | 2 |
| `CONTEXT.md` | 백로그 allow_hosts 항목 종결 | 2 |
| `docs/PUBLIC_RELEASE_BOUNDARY.md` | allow_hosts 행 갱신(경고 + 제거 예정) | 2 |

---

## Task 1: 런타임 deprecation 경고 + 테스트

**Files:**
- Modify: `cmd/goose-daemon/egress_policy.go` (`log/slog` import, `loadEgressProfile` 경고)
- Test: `cmd/goose-daemon/egress_policy_test.go` (2 신규 테스트 + import)

**Interfaces:**
- Consumes: `loadEgressProfile(baseDir, profile string) (egressProfile, bool, error)`; `egressProfile{ AllowHosts []string ... }` (변경 없음).
- Produces: `allow_hosts`>0 profile 로드 시 `slog.Warn(...)` 1회(동작 불변).

- [ ] **Step 1: Write the failing tests**

`cmd/goose-daemon/egress_policy_test.go`의 import를 교체한다:

old:
```go
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```
new:
```go
import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)
```

그리고 파일 끝(또는 `TestLoadEgressProfileAndPlanAllowlistCommands` 뒤)에 두 테스트를 추가한다:

```go
func TestLoadEgressProfileWarnsOnDeprecatedAllowHosts(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(pd, 0700); err != nil {
		t.Fatalf("mkdir profile dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "egress.json"),
		[]byte(`{"allow_hosts": ["api.anthropic.com"]}`), 0600); err != nil {
		t.Fatalf("write egress profile: %v", err)
	}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	profile, ok, err := loadEgressProfile(dir, "legacy")
	if err != nil || !ok {
		t.Fatalf("loadEgressProfile ok=%v err=%v", ok, err)
	}
	// Behavior unchanged: allow_hosts is still parsed/applied.
	if len(profile.AllowHosts) != 1 || profile.AllowHosts[0] != "api.anthropic.com" {
		t.Fatalf("allow_hosts must still be parsed unchanged, got %+v", profile.AllowHosts)
	}
	if out := buf.String(); !strings.Contains(out, "deprecated allow_hosts") || !strings.Contains(out, "allow_sni") {
		t.Fatalf("expected a deprecation warning naming the allow_sni migration, got: %q", out)
	}
}

func TestLoadEgressProfileNoWarnWithoutAllowHosts(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "sni")
	if err := os.MkdirAll(pd, 0700); err != nil {
		t.Fatalf("mkdir profile dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pd, "egress.json"),
		[]byte(`{"allow_sni": ["api.anthropic.com"]}`), 0600); err != nil {
		t.Fatalf("write egress profile: %v", err)
	}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(old)

	if _, ok, err := loadEgressProfile(dir, "sni"); err != nil || !ok {
		t.Fatalf("loadEgressProfile ok=%v err=%v", ok, err)
	}
	if strings.Contains(buf.String(), "deprecated allow_hosts") {
		t.Fatalf("no allow_hosts warning expected for an allow_sni-only profile, got: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify the Warns test fails**

Run: `go test ./cmd/goose-daemon/ -run 'TestLoadEgressProfileWarnsOnDeprecatedAllowHosts|TestLoadEgressProfileNoWarnWithoutAllowHosts' -v`
Expected: `TestLoadEgressProfileWarnsOnDeprecatedAllowHosts` FAILs ("expected a deprecation warning ..., got: \"\"" — no warning emitted yet). `TestLoadEgressProfileNoWarnWithoutAllowHosts` PASSes (no warning either way yet).

- [ ] **Step 3: Add the `log/slog` import to egress_policy.go**

`cmd/goose-daemon/egress_policy.go`의 import 블록을 교체한다:

old:
```go
import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)
```
new:
```go
import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)
```

- [ ] **Step 4: Emit the deprecation warning in loadEgressProfile**

`cmd/goose-daemon/egress_policy.go`의 `loadEgressProfile` 끝부분을 교체한다:

old:
```go
	if err := validateEgressProfile(profileConfig); err != nil {
		return egressProfile{}, false, err
	}
	return profileConfig, true, nil
}
```
new:
```go
	if err := validateEgressProfile(profileConfig); err != nil {
		return egressProfile{}, false, err
	}
	if len(profileConfig.AllowHosts) > 0 {
		// allow_hosts is a deprecated coarse substring matcher superseded by
		// allow_sni (parsed ClientHello SNI) + allow_cidrs. Warn on every load so
		// the docs-only deprecation becomes a runtime signal; the field is still
		// applied (behavior unchanged) and is scheduled for removal in the next
		// tagged anvil release (ADR-0002 OQ8). Content-free: profile name + count
		// only, never the host values.
		slog.Warn("egress profile uses deprecated allow_hosts (coarse packet substring match, fragmentation-evadable); migrate to allow_sni (parsed ClientHello SNI) for domains + allow_cidrs for IPs — allow_hosts will be removed in the next tagged anvil release",
			"profile", profile, "allow_hosts_count", len(profileConfig.AllowHosts))
	}
	return profileConfig, true, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./cmd/goose-daemon/ -run 'TestLoadEgressProfileWarnsOnDeprecatedAllowHosts|TestLoadEgressProfileNoWarnWithoutAllowHosts' -v`
Expected: 둘 다 PASS.

- [ ] **Step 6: Run the full daemon suite + build/vet/gofmt**

Run: `go test ./cmd/goose-daemon/... -race && go build ./... && go vet ./cmd/goose-daemon/... && gofmt -l cmd/goose-daemon/egress_policy.go cmd/goose-daemon/egress_policy_test.go`
Expected: test PASS(기존 `TestLoadEgressProfileAndPlanAllowlistCommands`는 allow_hosts profile을 로드하므로 이제 경고 1줄을 남기지만 — 기대 동작, 단언 대상 아님 — 통과 유지), build/vet ok, `gofmt -l` 빈 출력.

- [ ] **Step 7: Commit**

```bash
git add cmd/goose-daemon/egress_policy.go cmd/goose-daemon/egress_policy_test.go
git commit -m "feat(egress): runtime deprecation warning for legacy allow_hosts (removal next release)"
```

---

## Task 2: 마이그레이션 + 결정 문서

**Files:**
- Modify: `docs/operations/runbook.md`, `docs/adr/0002-egress-sni-transparent-filter.md`, `CONTEXT.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`

**Interfaces:** 없음(문서만). Task 1의 결정(경고 지금 + 다음 릴리즈 제거·loud fail-closed)을 반영.

- [ ] **Step 1: runbook — fallback 문구에서 allow_hosts 대신 allow_cidrs 권장 + 마이그레이션 노트**

`docs/operations/runbook.md`에서:

old:
```
복구: `nfnetlink_queue` 커널 모듈을 로드하거나(`modprobe nfnetlink_queue`),
`allow_sni` 없이 `allow_cidrs`/`allow_hosts`만 쓰는 profile로 전환한다.
```
new:
```
복구: `nfnetlink_queue` 커널 모듈을 로드하거나(`modprobe nfnetlink_queue`),
`allow_sni` 없이 `allow_cidrs`만 쓰는 profile로 전환한다(`allow_hosts`는 deprecated —
아래).
```

그리고 그 문단(`...사전 확인해야 한다.`)의 뒤에 마이그레이션 노트를 추가한다:

old:
```
profile을 쓰는 tenant는 대상 host 전체가 NFQUEUE를 지원하는지 운영자가
사전 확인해야 한다.
```
new:
```
profile을 쓰는 tenant는 대상 host 전체가 NFQUEUE를 지원하는지 운영자가
사전 확인해야 한다.

**`allow_hosts` deprecation(마이그레이션)**: `allow_hosts`(packet substring match)는
deprecated다 — profile 로드 시 daemon이 런타임 경고를 남기고(`loadEgressProfile`),
**다음 tagged anvil 릴리즈에서 제거**된다(제거 후 잔존 `allow_hosts` profile은 loud
fail-closed로 거부, ADR-0002 OQ8). 마이그레이션: 도메인(`api.example.com`) →
`allow_sni`(exact 또는 `*.` wildcard — 파싱된 ClientHello SNI 강제), 고정 IP/CIDR
백엔드 → `allow_cidrs`. substring match와 달리 `allow_sni`는 정확 SNI 매치라 부분
문자열 매치가 없으니 도메인 목록을 명시화한다.
```

- [ ] **Step 2: ADR-0002 — OQ8 deprecation cycle 결정**

`docs/adr/0002-egress-sni-transparent-filter.md`에서:

old:
```
- **OQ8 (`allow_hosts` 폐기 시점)**: 즉시 제거하지 않는다. **deprecated
  표기 후 유지**하고 신규 profile은 `allow_sni`로 유도한다. 고정 런타임
  계약 표면(`docs/operations/runbook.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`)이라
  제거는 별도 결정이 필요하다.
```
new:
```
- **OQ8 (`allow_hosts` 폐기 시점)**: **deprecation cycle 확정(2026-07-18).**
  release N(지금): profile 로드 시 daemon이 런타임 deprecation 경고를 남긴다
  (`loadEgressProfile` — 그동안 deprecation은 문서에만 있었다). release N+1(다음
  tagged anvil 릴리즈): 필드·apply·validate·cleanup·test 제거 + `egressProfile`
  unmarshal에 `DisallowUnknownFields`(또는 명시 거부)로 잔존 `allow_hosts` profile을
  loud fail-closed 거부(조용한 drop 방지). 마이그레이션: 도메인 → `allow_sni`,
  IP → `allow_cidrs`(`docs/operations/runbook.md`).
```

- [ ] **Step 3: CONTEXT.md — 백로그 종결**

`CONTEXT.md`에서:

old:
```
- egress SNI 필터 후속(ADR-0002 잔여 위험/설계 한계에서 파생, 미착수):
  `allow_hosts`(legacy substring) 제거 시점 재검토(OQ8, 고정 런타임 계약
  표면이라 즉시 제거하지 않음).
```
new:
```
- egress SNI 필터 후속(ADR-0002 잔여 위험/설계 한계에서 파생):
  `allow_hosts` 제거 시점은 **2026-07-18 deprecation cycle 확정**(release N 런타임
  경고 추가 + release N+1(다음 tagged anvil 릴리즈) 제거·loud fail-closed — OQ8).
```

- [ ] **Step 4: PUBLIC_RELEASE_BOUNDARY — allow_hosts 행 갱신**

`docs/PUBLIC_RELEASE_BOUNDARY.md`에서(L35 Egress SNI filter 행 내부):

old:
```
`allow_hosts`(packet substring match)는 **legacy/deprecated**로 표기되며 유지된다(제거 시점은 별도 결정).
```
new:
```
`allow_hosts`(packet substring match)는 **legacy/deprecated**다 — profile 로드 시 daemon이 런타임 경고를 남기고 **다음 tagged anvil 릴리즈에서 제거** 예정이다(제거 후 잔존 profile은 loud fail-closed 거부; 마이그레이션 도메인→`allow_sni`/IP→`allow_cidrs`, ADR-0002 OQ8).
```

- [ ] **Step 5: 잔여 스테일 확인 + Commit**

Run:
```bash
grep -rn "제거 시점은 별도 결정\|즉시 제거하지 않는다" docs/adr/0002-egress-sni-transparent-filter.md docs/PUBLIC_RELEASE_BOUNDARY.md CONTEXT.md
```
Expected: 빈 출력(옛 "별도 결정/즉시 제거하지 않음" 문구가 모두 deprecation-cycle 결정으로 갱신됨).

```bash
git add docs/operations/runbook.md docs/adr/0002-egress-sni-transparent-filter.md CONTEXT.md docs/PUBLIC_RELEASE_BOUNDARY.md
git commit -m "docs(egress): record allow_hosts deprecation cycle (OQ8) + migration to allow_sni/allow_cidrs"
```

---

## 최종 검증 (모든 태스크 후)

- [ ] `go test ./cmd/goose-daemon/... -race` — green(신규 경고 테스트 포함).
- [ ] `go build ./... && go vet ./...` — clean. `gofmt -l cmd/goose-daemon/` — empty.
- [ ] Step 5 grep — 옛 문구 0.
- [ ] 최종 whole-branch 리뷰: 동작 불변(allow_hosts 여전히 적용), 경고 content-free(host 값 미노출), 결정 문구 일관(release N 경고 / N+1 제거·loud fail-closed).
