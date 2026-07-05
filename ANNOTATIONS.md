# ANNOTATIONS.md — anvil v0.7.0 학습 주석 브랜치

이 브랜치(`annotate/v0.7.0`)는 **reference-only 학습 자료**다. 기준 커밋 `7b3f009`
("fix(runtime): adapt ephemera v0.7 installer and transcript support", 직전
merge `b2df010` "merge: sync ephemera v0.7.0")에서 갈라져 나와, 코드를 한 줄도
바꾸지 않고 한국어 학습용 주석(Go `//`, shell `#`, systemd unit `#`)만 추가했다.
이 브랜치는 다른 브랜치로 merge되지 않는다.

## 규칙

- 기존 코드 라인은 수정/삭제/재포맷하지 않는다. `git diff`는 주석 `+` 라인과 본
  파일 추가만 보여야 한다.
- INSTALL.md 등 기존 마크다운 문서는 건드리지 않는다.

## 기준 커밋이 담고 있는 v0.7.0 개요

`7b3f009`는 upstream ephemera `v0.7.0`을 anvil main runtime baseline으로
채택·적응한 결과다. 두 축으로 나뉜다.

1. **end-user installer (runtime/operator surface)** — `install.sh` /
   `uninstall.sh` / `ephemera.service.in` / `scripts/build_release.sh`.
   anvil/IronClaw product wrapper가 아니라 ephemera daemon 자체를 위한
   설치·릴리스 도구다. systemd 서비스 이름은 canonical `ephemera`를 유지한다.
2. **conversation transcript restore** — daemon proxy
   `GET /vms/{id}/sessions/{name}/transcript`(bearer 뒤, GET 전용)가 goose-agent의
   `GET /sessions/{name}/transcript`를 프록시한다. cache hit는 메모리에 있는
   `sessionInfo.transcript`를 바로 서빙하고, cache miss(콜드 재시작 등)일 때만
   `goose session export`(read-only, model 호출 없음)로 폴백한다.
3. **upstream hardening backport 화해 3종** — 사전에 anvil이 독립적으로 backport해둔
   구현이 upstream v0.7.0의 동등 기능과 합쳐지면서, **anvil 기존 구현이 그대로
   승리**했다(코드상 net diff는 doc-comment 수준). 세 가지:
   - kernel SHA256 atomic verify(`internal/storage/provisioner.go` `EnsureKernel`) —
     temp+rename으로 mismatch/write 실패 시 partial `vmlinux.bin`이 남지 않는다.
   - `resolveWorkDir()` / `EPHEMERA_HOME`(`cmd/goose-daemon/main.go`) — daemon
     workdir을 env로 명시할 수 있고, 미설정이면 `os.Getwd()`로 폴백한다.
   - `waitForAgent` per-probe timeout(`cmd/goose-daemon/api.go`) — guest가 TCP는
     붙었지만 `/health` 응답이 없을 때 전체 readiness deadline이 묶이지 않게 한다.

## transcript 안전 가드 4종 (TDD)

1. daemon 쪽 `GET /vms/{id}/sessions/{name}/transcript`는 bearer 없으면 `401`
   (`cmd/goose-daemon/api_test.go` `TestTranscriptEndpointRequiresBearer`).
2. 응답 payload는 provider key / control-plane token / `agent_token` sentinel-free
   (`cmd/goose-agent/main_test.go` `TestHandleSessionItem_PayloadOmitsAgentAuth`).
3. cache hit는 `goose session export`를 스폰하지 않는다
   (`TestHandleSessionItem_CacheHitSkipsGooseExport`).
4. export argv는 고정된 `session export -n {name} --format json`이며 실행("run")이나
   프롬프트가 끼어들 자리가 없다(`TestExportSessionTranscript_ReadOnlyNoModelCall`).

## 주석을 단 파일

| 파일 | 요약 |
|---|---|
| `install.sh` | 설치 흐름(preflight→place_files→configure_provider→setup_env→install_service→wait_ready→summary), DEST=EPHEMERA_HOME이 daemon workdir로 이어지는 지점, `-h`/`--help`의 무변이 경로 |
| `uninstall.sh` | 기본(service만 제거) vs `--purge`(DEST 전체 삭제) 분기, ephemera-스코프 `/tmp` 정리 경계(prefix-anchored, root-gated; `goose-rootfs`는 producer 없는 legacy 항목), `--help` 무변이 경로 |
| `ephemera.service.in` | `@DEST@` 치환점 3곳(WorkingDirectory/Environment/ExecStart)과 install.sh의 sed 치환, `EPHEMERA_HOME`이 `resolveWorkDir()`과 맞물리는 지점 |
| `scripts/build_release.sh` | SLIM(기본)/FULL(golden image 베이크, root 필요) 변형, kernel/firecracker 다운로드의 `sha256sum -c` 검증과 `EnsureKernel`의 skip-if-exists 사이 공급망 gap을 이 스크립트가 메우는 관계, `pin()`이 `main.go` 상수를 sed로 파싱하는 single source of truth 구조 |
| `cmd/goose-agent/main.go` | `handleSessionItem`/`exportSessionTranscript`/`TranscriptTurn` 부근 — cache-hit 시 export 미스폰, `goose session export`의 read-only 고정 argv, transcript 안전 가드 4종 이름 |
| `cmd/goose-daemon/api.go` | `GET /vms/{id}/sessions/{name}/transcript` 라우팅(bearer는 `authMiddleware`가 상위에서 이미 강제, 여기선 GET만 재검사), `waitForAgent`의 per-probe timeout(화해 3종 중 1) |
| `cmd/goose-daemon/main.go` | `resolveWorkDir()`(화해 3종 중 1, `EPHEMERA_HOME`), kernel/firecracker SHA pin 상수 블록(`build_release.sh`의 `pin()`이 파싱하는 대상) |
| `internal/storage/provisioner.go` | `EnsureKernel`(화해 3종 중 1) — temp+rename 원자 검증으로 partial kernel 파일 미잔류, upstream보다 strict한 지점과 그 한계(이미 존재하는 파일은 재검증 없이 skip → FULL tarball 배포분은 `build_release.sh` 쪽 검증에 의존) |

## 참고 자료 (읽기 전용, 이 브랜치엔 반영 안 됨)

- `RELEASE_NOTES.md` (이 워크트리 내)
- `/data/projects/codex-zone/anvil-ephemera-parity/docs/operations/2026-07-06-ephemera-v0.7-parity-sync-handoff.md`
