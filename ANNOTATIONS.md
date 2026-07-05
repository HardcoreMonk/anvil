# ANNOTATIONS.md — v0.5.x 학습용 주석 브랜치

## 브랜치 목적

이 브랜치(`annotate/v0.5.5`)는 **참고 전용(reference-only)** 학습 자료다. anvil 코드베이스가
upstream ephemera `v0.5.0`–`v0.5.5`("Web UI / operator console" 시리즈)를 어떻게 채택·적응했는지
한국어 주석으로 풀어 설명하기 위해 만들었다. **main으로 merge되지 않는다.** 기존 코드 라인은
단 한 줄도 수정/삭제/재포맷하지 않았고, 오직 새 주석 줄(Go `//`)과 이 문서만 추가했다.

## 기준 커밋

`7f207a0` — anvil이 upstream ephemera `v0.5.0`부터 `v0.5.5`까지를 merge·적응 완료한 시점
(`sync/ephemera-v0.7-core-service-parity` 계열 작업의 Phase 2 산출물). 상세 채택 근거와 검증
기록은 이 워크트리의 `RELEASE_NOTES.md` v0.5.0 섹션, 그리고 최종 브랜치
`anvil-ephemera-parity`의 `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md` /
`docs/operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md`를 참고했다(둘 다 읽기 전용
참고 자료로만 사용, 이 브랜치에는 포함되지 않음).

## 주석을 단 파일

| 파일 | 한 줄 요약 |
|---|---|
| `cmd/goose-daemon/ui.go` | 임베디드 Web UI(go:embed) 서빙, `/ui/`가 auth 밖인 이유(정적+로그인만), SPA fallback, path traversal 이중 방어 |
| `cmd/goose-daemon/config_api.go` | `/config/profiles` CRUD(provider/model/sizing), `/config/providers`(키 존재 여부만), `/config/clients`(이름+만료만), `/config/presets`, `system.md` 편집기(64 KiB 캡), `goose-secrets.yaml` 비접근 계약, traversal/`default` 예약 가드 |
| `cmd/goose-daemon/api.go` | 이중 mux(internalMux/externalMux) 배선과 auth 경계, per-VM sizing(`VcpuCount`/`MemSizeMib`) 흐름과 flock 경로의 비대칭, graceful VM delete(`gracefulAgentStop` → `StopVMM`) |
| `cmd/goose-daemon/config.go` | `LookupProfile`과 sizing 기본값(1 vCPU/1024 MiB) 채택 배경, `EPHEMERA_VCPU_COUNT`/`MEM` override가 `POST /vms`에만 적용되는 upstream-inherited 비대칭 |
| `cmd/goose-agent/main.go` | multi-turn goose 세션(`--resume`, 세션 이름 검증), 버퍼드 기본 계약 불변, `extractGooseJSONText`의 최신 턴만 추출하는 로직 |
| `web/src/lib/api.js` | 클라이언트 토큰 취급(sessionStorage/localStorage, 번들에 비밀 없음), `apiFetch`가 사실상 클라이언트 쪽 auth 게이트 역할을 하는 구조 |

**브리프 대비 확인 사항**: 브리프는 `web/src/api.js`를 지정했지만 실제 파일은
`web/src/lib/api.js`에 있다(다른 모든 소스 모듈도 `web/src/lib/` 아래 위치). 실제 경로에
주석을 달았다.

## web/src 구조 개요

`web/`는 Svelte + Vite로 만든 SPA로, `cmd/goose-daemon/uidist/`에 빌드 산출물이 커밋되어
Node 툴체인 없이도 `go build`가 완결된다. 엔트리는 `main.js`(→ `App.svelte`)이고, 상태는
`src/lib/store.js`의 두 writable(`auth`: 준비/비활성/토큰, `view`: 클라이언트 사이드 라우터
상태)로 관리한다. API 통신은 전부 `src/lib/api.js`의 `apiFetch`/`apiJSON`을 거치며, 화면별
컴포넌트는 `src/components/`에 평면적으로 나열돼 있다(`VMList`, `VMDetail`, `Flocks`,
`FlockDetail`, `Settings`, `System`, `Snapshots`, `ActivityFeed`, 각종 Modal류). 다국어는
`src/lib/i18n.js` + `src/locales/{en,ko}.json`(svelte-i18n)로 처리되며 브라우저 언어를
초기값으로 쓰고 `localStorage`에 선택을 남긴다.

## 시리즈 개요 (v0.5.0 → v0.5.5)

upstream ephemera는 v0.5.x 시리즈 전체를 통해 "스크립트 기반 외부 클라이언트"에서
"daemon이 직접 내는 브라우저 콘솔"로 운영 인터페이스를 옮겼다. anvil은 이 전체를
runtime/operator surface로 채택했으며, IronClaw MCP 표면(`cmd/anvil-mcp`)은 이 시리즈로
변경되지 않았다.

- **v0.5.0**: 임베디드 Web console(`/ui/`, EN/KO), `/config/profiles`(provider/model 편집),
  multi-turn goose 세션, graceful VM delete("stop agent" 액션 제거).
- **v0.5.1**: `/config/providers`(API 키 존재 여부만), snapshot/restore UI, per-profile
  sizing UI.
- **v0.5.2**: orchestration 콘솔 + 실시간 Activity Feed(SSE).
- **v0.5.3**: sizing 프리셋 + per-VM `VcpuCount`/`MemSizeMib`, 프로필 가드(in-use `409`,
  `default` 예약, traversal 거부), **기본 VM sizing을 1 vCPU/1024 MiB로 전환**.
- **v0.5.4**: `system.md` 전용 prompt 편집기(64 KiB 캡), 제어면 Town Wall 작성자
  (`SystemAuthor`) + 에이전트 수 복수형 처리.
- **v0.5.5**: System & Monitoring 콘솔(임베디드 Grafana), `/config/clients`(이름+만료만),
  `API_TOKEN` 경고, restore agent-wait 30s → 60s.

## 사이징 결정: 2 vCPU/2048 MiB → 1 vCPU/1024 MiB

v0.5.3부터 anvil은 upstream 기본 VM sizing인 1 vCPU / 1024 MiB("Standard" 프리셋)를
채택했다(그 이전엔 2 vCPU / 2048 MiB가 기본이었다). 이 전환은 KVM 실환경에서 full e2e를
3회 반복(`316✓ / 0✗` ×3)해 1024 MiB에서 문제없이 통과한 것을 근거로 승인됐다. 레거시
snapshot(sizing 값 0으로 기록된 것)은 restore 시 2/2048로 fallback하도록 남겨 뒀다
(`cmd/goose-daemon/api.go`의 restore 경로 참고).

**flock 사이징 갭**: `POST /vms`(standalone spawn, `cmd/goose-daemon/api.go`의 `spawnVM`
핸들러)는 `LookupProfile`의 기본값(1/1024) 위에 해당 프로필의 `goose.yaml`에 저장된
`EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB`(`readProfileConfig` 경유)를 override로
얹는다. 반면 flock 멤버 spawn 경로(`internal/orchestrator`가 채우는 `spawnVMOptions`)는
이 override 단계를 거치지 않고 `LookupProfile` 기본값만 그대로 사용한다. 즉 **flock으로
만든 에이전트는 아직 프로필별 커스텀 sizing을 받지 못하고 항상 기본 sizing으로만
뜬다** — 이는 upstream에서 물려받은 알려진 제약으로, 향후 sizing 경로 정리가 필요한
follow-up으로 문서화되어 있다(`docs/operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md`
의 "Known upstream-inherited gap").
