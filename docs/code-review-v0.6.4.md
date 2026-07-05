# Ephemera 전체 코드 리뷰 — v0.6.4

> 리뷰 일자: 2026-06-12 · 대상 커밋: `30eca75` (main) · 범위: Go 전체(15.3K LOC) + web/Svelte + scripts
> 방식: 서브시스템별 정독 리뷰(6개 차원: 보안/권한, 동시성/레이스, 리소스 생명주기/누수, 에러 처리/복원력, 정확성/Go idiom, 입력 검증)

## 요약 (Verdict)

Ephemera는 **root 권한으로 KVM·네트워크·디스크를 직접 조작하는 제어 평면**임에도, 코드 품질이 예외적으로 높습니다. **치명적(P0) 결함은 발견되지 않았습니다.** 특히 다음 영역이 견고합니다:

- **Teardown 완결성** — `spawnVMInternal`의 LIFO rollback 스택, snapshot restore의 실패 분기별 정확한 정리, dm-snapshot setup의 단계별 cleanup 클로저까지 에러 경로에서 리소스 누수가 거의 없음.
- **동시성** — 모든 공유 상태(IP/TAP 풀, flock 맵, rate-limit 버킷, metrics)가 일관되게 mutex/atomic으로 보호되고, `Snapshot()`은 defensive copy를 반환.
- **보안 기본기** — timing-safe 토큰 비교(`subtle.ConstantTimeCompare`), stdio 서브프로세스의 권한 드롭+rlimit+minimal env, path traversal 검증, MCP 크리덴셜의 호스트-only 경계.

| 심각도 | 건수 | 처리 |
|---|---|---|
| **P0** (치명) | 0 | — |
| **P1** (보안 약화/명확한 수정) | 1 | **수정 적용** |
| **P2** (조건부 영향/누수) | 6 | 1건 수정, 5건 백로그 |
| **P3** (견고성/일관성/단순화) | ~13 | 백로그 |

---

## P1 — 수정 적용됨

### P1-1. 커널 다운로드에 무결성(SHA256) 검증 부재
- **위치**: `internal/storage/provisioner.go:509` `EnsureKernel` / `cmd/goose-daemon/main.go:73-77,98`
- **문제**: `EnsureFirecracker`는 핀된 SHA256으로 다운로드 바이너리를 검증하지만, `EnsureKernel`은 검증 없이 S3에서 커널을 받아 그대로 씀. 커널은 모든 VM의 신뢰 기반(`init=`로 micro-init을 부팅)이므로, S3 버킷 변조·firecracker CI 침해·전송 경로 변조 시 **변조된 커널이 검증 없이 모든 게스트에 적용**됨. HTTPS가 전송 보안 일부를 주지만 firecracker와 동일한 무결성 핀이 빠진 명백한 비대칭.
- **수정**: `EnsureKernel`에 `expectedSHA256` 파라미터를 추가해 `EnsureFirecracker`와 동일한 스트리밍 SHA256 검증 + 불일치 시 부분 다운로드 제거. `main.go`에 현재 검증된 커널의 SHA256(`e20e46d0…d4f2`)을 상수로 핀. 빈 문자열을 넘기면 검증을 건너뛰어(하위호환) 기존 호출자/테스트가 깨지지 않음.

---

## P2 — 조건부 영향 (1건 수정, 5건 백로그)

### P2-1. `waitForAgent`의 타임아웃 없는 `http.Get` — 수정 적용됨
- **위치**: `cmd/goose-daemon/api.go:1321`
- **문제**: `http.Get`은 기본 타임아웃이 없음. 60초 deadline 루프 안에서 호출되지만, 게스트가 TCP 연결은 수락하고 HTTP 응답을 주지 않는 half-open 상태(커널은 떴으나 goose-agent 미동작)면 **단일 `http.Get`이 무기한 블록**되어 deadline 체크에 도달하지 못함. spawn/restore/recovery 세 경로가 모두 이 함수에 의존.
- **수정**: probe별 타임아웃(2초)을 가진 `http.Client`로 교체. deadline(60초)보다 짧아 half-open 시 빠르게 다음 시도로 넘어감.

### P2-2. MCP 게이트웨이 세션 스토어 누수 (백로그)
- **위치**: `internal/mcpgateway/session.go` + `gateway.go:85,152`
- **문제**: `handleInitialize`가 매 `initialize`마다 `sessions.Create`로 엔트리를 추가하지만, **`SessionStore.Get`은 코드 어디에서도 호출되지 않음**(caller는 매 요청 source IP로 재해석). 세션은 goose가 명시적 `DELETE`를 보낼 때만 정리되는데, VM이 그냥 파괴되면(`destroyVM`) 정리 훅이 없어 **엔트리가 영구 잔존**. ephemeral VM이 다수 생성/파괴되면 맵이 무한 성장.
- **권고**: (a) `destroyVM`/flock teardown에서 해당 VM의 세션을 정리하는 훅 추가, 또는 (b) rate-limit 버킷처럼 idle sweep 추가, 또는 (c) `Get`이 실제로 안 쓰이므로 세션 스토어를 경량화. MCP 스펙상 `Mcp-Session-Id` 발급은 유지해야 하므로 (a)/(b)가 적절.

### P2-3. micro-init(PID 1)의 좀비 reaping 부재 (백로그)
- **위치**: `cmd/micro-init/main.go`
- **문제**: micro-init은 PID 1이지만 `goose-agent`만 `cmd.Wait()`하고, `SIGCHLD` 기반 reap 루프가 없음. goose가 도구 실행으로 만든 서브프로세스가 고아가 되면 PID 1로 재부모화되어 **좀비가 누적**. ephemeral(짧은 수명) VM은 영향이 작지만, persistent 모드 장수 VM에서는 PID/메모리 누수.
- **권고**: PID 1에 `signal.Notify(SIGCHLD)` + `syscall.Wait4(-1, ..., WNOHANG, nil)` 루프를 추가해 재부모화된 좀비를 reap. goose-agent의 정상 종료 감지(`doneCh`)와 충돌하지 않도록 자식 PID를 구분.

### P2-4. `time.Now().UnixNano()` 기반 ID 충돌 가능성 (백로그)
- **위치**: `api.go:854,1515,1709`(`vm-`/`snap-`), `orchestrator_api.go:166`(`flock-`)
- **문제**: VM/스냅샷/flock ID가 `fmt.Sprintf("vm-%d", time.Now().UnixNano())` 패턴. 동시 요청 두 개가 같은 나노초를 받으면(시계 해상도가 낮은 호스트, 또는 동시 spawn 부하) **ID 충돌 → `cp.vms`/`cp.snapshots` 맵에서 덮어쓰기**로 이전 항목의 machine/TAP/disk 추적을 상실(리소스 누수). 실제 충돌 확률은 낮으나 동시성 부하에서 가능.
- **권고**: ID에 짧은 랜덤 접미사 추가(`crypto/rand` 4바이트 hex) 또는 atomic counter 병행.

### P2-5. anti-spoof가 ebtables 부재 시 조용히 비활성 → caller-identity 스푸핑 (백로그/설계)
- **위치**: `internal/network/manager.go:55-58` + `internal/mcpgateway/identity.go:43`
- **문제**: MCP 게이트웨이는 caller를 **source IP로 식별**(`ipCallerResolver`)하고, 그 무결성은 per-TAP ebtables anti-spoof 핀에 의존. anti-spoof는 기본 활성이지만 `ebtables`가 PATH에 없으면 경고만 남기고 비활성화됨(`never fatal`). 이 경우 한 VM이 다른 VM의 10.0.1.x source IP를 스푸핑해 **그 프로파일의 정책·rate budget·툴 카탈로그를 가로챌** 수 있음.
- **현황**: CLAUDE.md에 문서화된 의도적 trade-off(가용성 우선). 단 보안 영향이 있으므로, MCP 게이트웨이가 켜진 상태(`EPHEMERA_MCP_ENABLED`)에서 anti-spoof가 꺼져 있으면 **startup 경고를 승격**하거나, MCP-on + ebtables-absent 조합을 명시적으로 막는 옵션을 권고.

### P2-6. watchdog `failCount` 맵의 standalone VM 미정리 (백로그)
- **위치**: `internal/orchestrator/watchdog.go:286` `ForgetVM`
- **문제**: `ForgetVM`은 flock 경로(`removeFlockAgent`/`restartAgent`/`changeFlockAgentRole`)에서만 호출됨. 일반 `destroyVM`(DELETE /vms/{id})은 호출하지 않으므로, probe에 한 번이라도 실패한 적 있는 standalone VM의 `failCount` 엔트리가 파괴 후에도 잔존. 작은 누수지만 standalone VM을 반복 생성/파괴하면 축적.
- **권고**: `destroyVM`에서도 `cp.watchdog.ForgetVM(vmID)` 호출(현재 일부 경로만).

---

## P3 — 견고성·일관성·단순화 (백로그)

| # | 위치 | 내용 |
|---|---|---|
| P3-1 | `storage/snapshot.go:364` | `teardownDMSnapshot`이 `exec.Command("sleep","0.2")`로 외부 프로세스를 fork — `time.Sleep(200*time.Millisecond)`로 교체(불필요한 프로세스 생성, `sleep` 바이너리 부재 시 무대기). |
| P3-2 | `storage/snapshot.go:400,590` | `overlaySparseDiff`/`sparseCopyFile`이 `defer out.Close()`만 하고 Close 에러를 미체크 — `WriteRootfsDiff`는 명시적으로 체크함(비대칭). 머지 파일 flush 실패가 조용히 누락될 수 있음. |
| P3-3 | `storage/provisioner.go:383` | `copyFile`이 `io.Copy` 실패 시 부분 `dst`를 제거하지 않음(다른 헬퍼는 제거). |
| P3-4 | `storage/provisioner.go:265` | `copyFile`이 secrets.yaml 모드를 보존하지 않아 VM 내 0644(게스트 root-only라 영향은 낮음). |
| P3-5 | `network/manager.go:276` | `Release`가 free-list에 무조건 `append` — 이중 Release 방어 부재(현재 호출 경로는 1회 보장이라 이론적). |
| P3-6 | `mcpgateway/gateway.go:293,366` | `resources/read`·`prompts/get`은 `Allows(server)`만 체크하고 tool-level allow/deny를 적용하지 않음(`tools/call`은 적용). resource/prompt 세밀 필터 불가(일관성). |
| P3-7 | `mcpgateway/registry.go:338` | `HealthAll`이 backend를 순차 probe(각 5초) — 백엔드가 많으면 `GET /config/mcp/servers` 응답 지연. 병렬화 가능. |
| P3-8 | `config_api.go:432 vs 46/125/219` | `validateProfileName`(엄격한 슬러그 정규식)은 create에만 적용, update/delete는 느슨한 `ContainsAny`만(traversal은 양쪽 차단되나 검증 비대칭). |
| P3-9 | `goose-agent/main.go:405` | vsock `CHANGE_IP`/`SET_CP_TOKEN`에 인증 없음 — vsock은 host↔guest 전용이라 위험은 낮으나, 게스트 내 loopback CID 접근 가능 커널에서는 자기 VM 한정 영향. |
| P3-10 | `goose-agent/main.go:838` | `handleTownWallPost` 주석이 "forward without bearer auth (v1)"라 outdated — 코드는 `loadCPToken`으로 토큰 주입함. |
| P3-11 | `webdev_demo.sh`, `observability_demo.sh` | `set -euo pipefail` 부재(데모 스크립트라 영향은 작음). |
| P3-12 | `web/src/lib/api.js:13` | "remember me" 시 토큰을 `localStorage`에 저장 — XSS sink는 0건이라 현재 안전하나, XSS 발생 시 탈취 가능(표준 SPA 위험). |
| P3-13 | `recovery.go:301,370` | `recoverRestoredVM`/`reRestoreMachine`이 `cp.snapshots`를 락 없이 접근 — `RecoverVMs`가 HTTP 서버 시작 전 초기화 단계에서만 호출되므로 현재 안전(방어적으로 락 권장). |

---

## 서브시스템별 코멘트

- **`cmd/goose-daemon` (api.go 등)** — 가장 큰 표면이지만 품질 최상. spawn rollback, snapshot 전 분기 teardown, timing-safe 인증, 2-mux(metrics 인증 분리) 모두 견고. 발견은 대부분 P2-1(타임아웃)·P2-4(ID).
- **`internal/storage`** — dm-snapshot/COW/diff 로직이 정교하고 실패 경로 정리가 완결적. P3 위주(sleep fork, Close 체크).
- **`internal/network` / `internal/vm`** — IP/TAP 풀 동시성·anti-spoof·forwardSignals 설계가 신중. `vm`은 thin-test(21줄)지만 코드 자체는 견고.
- **`internal/mcpgateway`** — 보안 설계의 백미. stdio 서브프로세스(setuid·rlimit·minimal env·scratch dir Lstat 가드)는 거의 production-grade. P2-2(세션 누수)가 유일한 실질 이슈.
- **`internal/orchestrator`** — flock/townwall/watchdog 동시성 모범적. SSE의 subscribe-before-flush, watchdog의 context-timeout probe·goroutine 정리.
- **`cmd/goose-agent`** — goose를 shell 없이 exec(인젝션 없음), session 이름 검증, NDJSON 동시쓰기 직렬화.
- **web / scripts / ctl** — web XSS sink 0건(Svelte 자동 escape), ctl은 순수 HTTP 클라이언트(exec 없음), 핵심 scripts는 `set -` 적용.

## 테스트 커버리지 갭

LOC 대비 테스트가 얇은 영역(보고서 계획 단계 측정):
- `internal/network` (405 src / **37 test**) — IP/TAP 풀 동시성, ebtables 규칙 구성, 복구 reclaim 경로.
- `internal/vm` (381 src / **21 test**) — KernelArgs 구성, vsock 명령 핸드셰이크, restore 핸들러 옵션.
- `cmd/ephemera-ctl` (978 src / **128 test**) — CLI 인자 파싱 엣지 케이스.
- 쉘 스크립트(~3.6K, root 실행)·Svelte UI(3.3K)는 자동 테스트 없음.

→ 권고: network 풀 동시성(`-race`)과 vm KernelArgs/vsock에 단위 테스트를 보강하면 향후 멀티호스트 포크 시 회귀 안전망이 됨.

## 권고 (우선순위)

1. **즉시(수정 적용됨)**: P1-1 커널 SHA 검증, P2-1 waitForAgent 타임아웃.
2. **다음 유지보수 사이클**: P2-2 세션 누수(정리 훅), P2-3 PID 1 좀비 reaping, P2-6 watchdog 정리 — 모두 ephemeral 환경에서 누적되는 누수.
3. **보안 강화**: P2-5 MCP-on + anti-spoof-off 조합 경고 승격.
4. **정리(선택)**: P3 항목들은 동작 영향이 없으므로 여유 시 일괄 정리(특히 P3-1 sleep fork, P3-10 outdated 주석).

> **검증 경계**: 이 리뷰의 수정(P1-1, P2-1)은 `go build`/`go vet`/`go test`로 로컬 검증함. **커밋·PR 전 사용자의 수동 `sudo bash e2e_test.sh` 통과가 필요**하며(CI가 e2e 게이트를 실행하지 못함), Go 수정 후 `go build -o ephemera-daemon ./cmd/goose-daemon/` 재빌드가 선행되어야 함.
