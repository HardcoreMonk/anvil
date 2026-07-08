# Bounded relay retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** daemon-to-daemon wall/call hop이 dial-계열 실패(요청이 상대에 도달하지 않았음이 보장되는 실패)에 한해 guest 요청 창 안에서 최대 2회 자동 재시도한다 — 전달 semantics 불변.

**Architecture:** `cmd/goose-daemon/orchestrator_api.go`에 dial-에러 판별 `isDialError`와 요청-재생성 재시도 helper `doWithDialRetry`를 추가하고, home/target-bound hop 3곳(`relayTownWallPost`, `forwardFlockCall`, `townWallHistory` relay GET)이 이를 경유한다. HTTP 응답·연결 후 실패·ctx 만료는 절대 재시도하지 않는다(wall 중복·call 이중 실행 배제). 설계 spec: `docs/superpowers/specs/2026-07-08-bounded-relay-retry-design.md`.

**Tech Stack:** Go (module `ephemera`), `net`/`net/http`, `httptest`, fake `http.RoundTripper`.

## Global Constraints

- 브랜치: local main(스펙 커밋 `342f70c` 포함)에서 분기. TDD RED→GREEN. 기존 테스트 제거/약화 금지. **git trailer 금지**.
- 각 task 종료 시 전체 suite `go test ./cmd/... ./internal/... -count=1` (부분 suite 금지).
- **재시도는 dial-계열 transport 에러에만** (`*net.OpError.Op == "dial"`, `url.Error` 체인 unwrap). HTTP 응답(상태 무관)·reset/EOF·ctx 취소/만료는 재시도 금지.
- 정책 고정: 최대 2회 재시도(총 3 시도), backoff `1s → 2s`, backoff는 ctx-aware(`ctx.Done()` 시 즉시 중단). 설정 env 신설 금지.
- 재시도 로그는 flock/host 식별자만 — daemon 주소·토큰 금지 (`d5c7df0` 규율).
- SSE(`streamTownWallRelay`)는 비범위 — 건드리지 않는다.
- 전달 semantics 불변: 성공 응답 = 상대 daemon ack. 기존 relay/call 테스트는 무변경 통과해야 한다.

---

## File Structure

- `cmd/goose-daemon/relay_retry.go` — Create: `isDialError`, `doWithDialRetry`, 정책 상수 (한 파일, 한 책임).
- `cmd/goose-daemon/relay_retry_test.go` — Create: helper 단위 테스트.
- `cmd/goose-daemon/orchestrator_api.go` — Modify: hop 3곳이 helper 경유 (`relayTownWallPost` :368-377, `townWallHistory` relay 분기 :449-455, `forwardFlockCall` :1339-1354 — 줄번호는 근사, 함수명이 기준).
- `cmd/goose-daemon/flock_call_test.go` / `townwall_relay_test.go` — Modify: 통합 테스트 추가 (기존 것 무변경).
- 문서: runbook, wall/gtcall handoff Follow-Up CLOSED, slice handoff 신설.

---

## Task 1: `isDialError` + `doWithDialRetry` helper (TDD)

**Files:**
- Create: `cmd/goose-daemon/relay_retry.go`
- Test: Create `cmd/goose-daemon/relay_retry_test.go`

**Interfaces:**
- Produces (Task 2가 사용):
  - `func isDialError(err error) bool`
  - `func doWithDialRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error)` — 시도마다 `build()`로 요청 재생성; dial-에러면 backoff(1s→2s, ctx-aware) 후 재시도, 총 3 시도; 그 외 에러/모든 HTTP 응답은 즉시 반환.
  - 상수 `relayRetryAttempts = 3`, `relayRetryBackoff = []time.Duration{time.Second, 2 * time.Second}`.
  - 테스트 훅: backoff sleep은 패키지 변수 `relayRetrySleep = sleepCtx` 형태로 두어 테스트가 fake로 교체 가능하게 (`sleepCtx(ctx, d)`는 `select { <-ctx.Done() / <-time.After(d) }`).

- [ ] **Step 1: 실패 테스트 작성** — `cmd/goose-daemon/relay_retry_test.go`:

```go
package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRT returns scripted results per attempt and counts calls.
type fakeRT struct {
	calls   atomic.Int32
	results []func() (*http.Response, error)
}

func (f *fakeRT) RoundTrip(*http.Request) (*http.Response, error) {
	n := int(f.calls.Add(1)) - 1
	if n >= len(f.results) {
		n = len(f.results) - 1
	}
	return f.results[n]()
}

func okResp() (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func dialErr() (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
}

func resetErr() (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}}
}

func buildReq(t *testing.T, ctx context.Context) func() (*http.Request, error) {
	t.Helper()
	return func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, "http://home.invalid/flocks/f/post", strings.NewReader(`{}`))
	}
}

func noSleep(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func TestIsDialError(t *testing.T) {
	if _, err := dialErr(); !isDialError(err) {
		t.Fatal("wrapped dial OpError must classify as dial error")
	}
	if _, err := resetErr(); isDialError(err) {
		t.Fatal("read/reset OpError must NOT classify as dial error")
	}
	if isDialError(nil) || isDialError(errors.New("boom")) {
		t.Fatal("nil/plain errors must not classify as dial error")
	}
}

func TestDoWithDialRetry_RetriesDialThenSucceeds(t *testing.T) {
	old := relayRetrySleep
	relayRetrySleep = noSleep
	defer func() { relayRetrySleep = old }()

	rt := &fakeRT{results: []func() (*http.Response, error){dialErr, okResp}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	resp, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v, want 200 after one retry", resp, err)
	}
	if got := rt.calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDoWithDialRetry_StopsAtAttemptCap(t *testing.T) {
	old := relayRetrySleep
	relayRetrySleep = noSleep
	defer func() { relayRetrySleep = old }()

	rt := &fakeRT{results: []func() (*http.Response, error){dialErr}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if got := rt.calls.Load(); got != int32(relayRetryAttempts) {
		t.Fatalf("attempts = %d, want %d", got, relayRetryAttempts)
	}
}

func TestDoWithDialRetry_NoRetryOnHTTPResponse(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){func() (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	}}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	resp, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err != nil || resp.StatusCode != 500 {
		t.Fatalf("resp=%v err=%v, want the 500 passed through", resp, err)
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (HTTP responses never retry)", got)
	}
}

func TestDoWithDialRetry_NoRetryOnResetError(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){resetErr}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want the reset error surfaced")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (post-connect failures never retry)", got)
	}
}

func TestDoWithDialRetry_CtxCancelAbortsBackoff(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){dialErr}}
	client := &http.Client{Transport: rt}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 이미 취소된 ctx — 첫 dial 실패 후 backoff에서 즉시 중단해야 함
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want error")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (cancelled ctx must abort backoff)", got)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/goose-daemon -run 'TestIsDialError|TestDoWithDialRetry' -count=1` / Expected: FAIL (undefined: isDialError 등 — 컴파일 에러 RED).

- [ ] **Step 3: 구현** — Create `cmd/goose-daemon/relay_retry.go`:

```go
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Bounded synchronous retry for daemon-to-daemon relay hops (wall post/history
// relay, call forwards). Retries ONLY dial-class transport failures — the one
// case where the request provably never reached the peer — so a retry can
// never duplicate a wall post or double-execute a call prompt. HTTP responses
// (any status) are the peer's answer and are returned as-is; post-connect
// failures (reset/EOF) may have been processed and are surfaced immediately.
// Policy is fixed (no env): 3 total attempts, 1s then 2s ctx-aware backoff —
// well inside the guest 300s > member 290s > hub 280s timeout ladder.
const relayRetryAttempts = 3

var relayRetryBackoff = []time.Duration{time.Second, 2 * time.Second}

// relayRetrySleep is swappable in tests.
var relayRetrySleep = sleepCtx

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// isDialError reports whether err is a dial-phase failure (connection never
// established). It unwraps url.Error/OpError chains.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// doWithDialRetry executes build()+Do up to relayRetryAttempts times,
// rebuilding the request each attempt (bodies are single-use readers).
func doWithDialRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < relayRetryAttempts; attempt++ {
		if attempt > 0 {
			if err := relayRetrySleep(ctx, relayRetryBackoff[attempt-1]); err != nil {
				return nil, lastErr
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}
```

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/goose-daemon -run 'TestIsDialError|TestDoWithDialRetry' -count=1 -race` / Expected: 6종 PASS.

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add cmd/goose-daemon/relay_retry.go cmd/goose-daemon/relay_retry_test.go
git commit -m 'feat(runtime): dial-failure-only bounded retry helper for relay hops'
```

---

## Task 2: hop 3곳 적용 + 재시도 로그 + 통합 테스트

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go` (`relayTownWallPost` ~:368, `townWallHistory` relay 분기 ~:449, `forwardFlockCall` ~:1339)
- Test: `cmd/goose-daemon/townwall_relay_test.go`, `cmd/goose-daemon/flock_call_test.go` (추가만)

**Interfaces:**
- Consumes: Task 1의 `doWithDialRetry`, `relayRetrySleep`(테스트 훅).
- Produces: 세 hop이 dial-실패 시 자동 재시도. 외부 시그니처 무변경.

- [ ] **Step 1: 실패 테스트 작성** — 두 테스트 파일에 추가 (기존 셋업 헬퍼 재사용):

`townwall_relay_test.go`:

```go
// TestPostToTownWall_RelayRetriesDialFailure: relay flock의 HomeAddr를
// 먼저 닫아둔 리스너 포트로 → 첫 시도 dial 실패 → (테스트 훅 relayRetrySleep을
// noSleep으로 교체한 상태에서) 재시도 전에 실제 httptest 서버를 그 포트에
// 기동하는 대신, 결정론을 위해 다음 방식 사용:
//   net.Listen으로 포트 확보 → 주소 기록 → Close (첫 dial 실패 보장)
//   relayRetrySleep 훅 안에서(첫 backoff 시점) 같은 주소로 httptest-스타일
//   서버를 다시 Listen+serve — 두 번째 시도가 성공.
// 단언: 응답 200, home 서버가 정확히 1회 요청 수신(중복 post 없음).
```

`flock_call_test.go`:

```go
// TestCallFlockAgent_RelayRetryExhausted502: relay flock HomeAddr를 닫힌
// 포트로, relayRetrySleep=noSleep → 3 시도 소진 후 502, 에러 바디에 daemon
// 주소·토큰 문자열 부재(redaction), 벽시계 < 5s.
```

두 주석 시나리오를 실제 테스트 코드로 작성한다. 포트 재사용 레이스가 CI에서 불안하면 첫 번째 테스트는 fakeRT 주입이 불가한 통합 계층 대신 — `relayRetrySleep` 훅에서 서버 기동이 실패할 경우를 대비해 `t.Skip` 없이 재시도 로직 자체는 Task 1 단위 테스트가 보증함을 전제로, "닫힌 포트 → 3 시도 → 502 + home 0회 수신" 형태(두 테스트 모두 소진 시나리오)로 통일해도 된다 — 단 그 경우 두 번째-시도-성공 경로는 Task 1 단위 테스트가 유일한 증거임을 report에 명시.

- [ ] **Step 2: 실패 확인** — Run: `go test ./cmd/goose-daemon -run 'RelayRetries|RelayRetryExhausted' -count=1` / Expected: FAIL (현행은 재시도 없이 1 시도 즉시 502 — 시도 수/성공 단언 실패).

- [ ] **Step 3: 구현** — 세 지점을 helper 경유로 교체. 패턴 (relayTownWallPost — 나머지 두 곳 동일 구조):

```go
	// 기존:
	// resp, err := newAgentHTTPClient().Do(req)  (req는 위에서 1회 생성)
	// 교체: 요청 생성을 build 클로저로 옮기고 doWithDialRetry 경유
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+relayToken)
		return req, nil
	}
	resp, err := doWithDialRetry(ctx, newAgentHTTPClient(), build)
```

`forwardFlockCall`은 기존 헤더 세트(call token, hop 표식 markHop 조건, depth)를 build 클로저 안으로 그대로 이동 — **markHop/depth semantics 무변경**. `townWallHistory` relay 분기도 동일(GET, 헤더 Authorization만).

재시도 발생 시 로그: `doWithDialRetry`는 범용이므로 로그는 넣지 않고, 호출부가 아니라 helper에 slog를 넣으면 flock/host 컨텍스트가 없다 — **helper에 optional logf 파라미터를 추가하는 대신** (시그니처 비대 방지) helper가 재시도 횟수를 알 수 있게 하지 말고, 단순히 helper 안에서 `slog.Warn("relay hop dial failed, retrying", "attempt", attempt+1)` 1줄만 (URL·주소 미포함 — attempt 번호만. flock/host 식별은 최종 실패 시 기존 호출부 에러 메시지가 담당). 이 로그 라인에 주소/토큰이 없음을 코드 리뷰 포인트로.

- [ ] **Step 4: 통과 확인** — Run: `go test ./cmd/goose-daemon -count=1 -race` / Expected: 신규 + 기존 relay/call 테스트 전부 PASS (기존 무변경 — semantics 불변 증거).

- [ ] **Step 5: 전체 확인 + 커밋**

```bash
go test ./cmd/... ./internal/... -count=1
git add cmd/goose-daemon
git commit -m 'feat(runtime): relay hops retry dial failures within the caller window'
```

---

## Task 3: 문서 반영

**Files:**
- Modify: `docs/operations/runbook.md` (순단 대응 1-2줄 — cross-host 절차 부근)
- Modify: `docs/operations/2026-07-06-cross-host-town-wall-handoff.md`, `docs/operations/2026-07-08-cross-host-gtcall-handoff.md` (Follow-Up "bounded relay retry/buffer" CLOSED — 기존 CLOSED 형식, 커밋 해시, "비동기 버퍼는 mesh/수동 검증 이후 재평가" 명시)
- Create: `docs/operations/2026-07-08-bounded-relay-retry-handoff.md` (선행 handoff 형식: 무엇이 배포됐나/재시도 규칙과 안전 논거/Gate 결과/Known limitations(dial-만, SSE 비범위, 비동기 버퍼 없음)/Next Action(수동 multi-host 검증, mesh 설계)/Follow-Up)

**Interfaces:** 없음 (docs-only).

- [ ] **Step 1: 세 문서 갱신 + handoff 작성** — 사실 원천은 구현 코드와 spec. 각 문서 기존 형식 유지.
- [ ] **Step 2: 검증** — `git diff --check` clean, 서술-코드 교차 확인(시도 수 3, backoff 1s/2s, dial-만).
- [ ] **Step 3: 커밋**

```bash
git add docs/
git commit -m 'docs: document bounded relay retry (dial-failure-only, 3 attempts)'
```

---

## Final verification gate (after all tasks)

```bash
go test ./cmd/... ./internal/... -count=1
go build ./cmd/goose-daemon ./cmd/anvil-mcp ./cmd/anvil-scheduler ./cmd/ephemera-ctl
git diff --check
# KVM host (semantics 불변 회귀 확인):
go build -o anvil-daemon ./cmd/goose-daemon/
sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh
sudo -n bash scripts/anvil-cross-host-wall-e2e.sh
```

Expected: 전체 suite green; 빌드 4종; 두 e2e 기존과 동일 통과(재시도는 e2e 정상 경로에서 발화하지 않음 — 회귀 없음의 증거).
