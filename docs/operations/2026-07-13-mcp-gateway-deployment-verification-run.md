# runtime MCP Gateway — 실 backend 배포 검증 수행 기록 (2026-07-13)

PR #40 트랙 C가 남긴 유일 잔여였던 **실 backend를 붙인 operator 배포 검증**을 수행한 기록.
backend는 **DeepWiki**(`https://mcp.deepwiki.com/mcp`, public Streamable HTTP MCP, credential
불필요) — 코드에서 유일하게 실왕복 검증된 후보(`internal/mcpgateway/live_test.go` `TestLive_DeepWiki`).
PR #40 dry-run(가짜 URL, `up:false`, leak guard)과의 핵심 대비는 이번엔 **실 backend가 살아
tools/list·tools/call을 왕복하고 VM 내부 caller가 gateway 경유로 실제 tool을 호출**한다는 점이다.

판정: **PASS** (실왕복 성공 + 경계 3종 성립, 신규 결함 없음).

## 환경

| | 값 |
|---|---|
| host | host-b `192.168.1.20` (PureCVisor-Prod-2), Ubuntu 24.04.4, 16c/32G, `/dev/kvm` 있음 |
| 루트 fs | root-on-ZFS (rpool 128K) + `rpool/anvil-snapshots`(recordsize **4K**) → `~/anvil/snapshots` (불가침, 유지) |
| Go | 1.26.2 (`/opt/anvil-go`) |
| 소스 | branch `docs/mcp-gateway-deepwiki-verification` (`59ff27b`; go code == main `e2725e3`, 이 브랜치 diff는 docs-only) — 워크스테이션 worktree에서 rsync(`--delete`, snapshots/·configs/goose*.yaml·runtime dir 제외), host 빌드 |
| daemon | auth-on(`EPHEMERA_API_TOKEN`), `EPHEMERA_API_ADDR=127.0.0.1:3000`, `EPHEMERA_MCP_ENABLED=1`, 그 외 MCP env 기본값 |
| gateway | bind `10.0.1.1:3001`(bridge IP, 기본), VM-facing `http://ephemera-gw:3001/mcp` |
| backend config | `configs/mcp/servers.yaml`(gitignored, 임시): `deepwiki` http, **credential 없음**, `profiles: [worker]`. `secrets.yaml` 부재(credential 없어 불요) |

DeepWiki는 no-auth라 VM은 gateway URL만 받고 credential은 애초에 없다. credential 있는 backend였다면
`secrets.yaml`은 host-only로 두고 VM엔 URL만 주입되는 규율(runbook §credential)이 그대로 적용된다.

## 결과 매트릭스

| 단계 | 결과 | 증거 요지 |
|---|---|---|
| ① 로컬 유닛 실왕복 (`TestLive_DeepWiki`, 워크스테이션) | **PASS** | `EPHEMERA_MCP_LIVE_TEST=1 go test ./internal/mcpgateway/ -run TestLive_DeepWiki -v` → `PASS (1.03s)`. ListTools=3(`read_wiki_structure`, `read_wiki_contents`, `ask_question`), CallTool `read_wiki_structure` non-empty |
| ② 배포·기동 (auth-on, MCP on) | **PASS** | 로그 `mcp gateway configured endpoint=http://ephemera-gw:3001/mcp bind=10.0.1.1:3001 servers=1` |
| ③ `GET /config/mcp` (bearer) | **PASS** | `{"enabled":true,"endpoint":"http://ephemera-gw:3001/mcp","server_count":1}` |
| ④ `GET /config/mcp/servers` (bearer) | **PASS** | `deepwiki` url 노출, **`up:true`**(실 backend 헬스 up — PR #40 fake `up:false`와 대비), `has_credential:false`, `command:""`, **token/args 필드 없음** |
| ⑤ VM spawn (`profile=worker`) | **PASS** | `201`, `vm-…332006`@`10.0.1.10`. 게스트 `/etc/hosts`에 `10.0.1.1 ephemera-gw` 주입, `/root/.ephemera-mcp` = `http://ephemera-gw:3001/mcp` |
| ⑥ **in-guest tools/list** (VM→gateway) | **PASS** | 게스트 workload가 `POST http://ephemera-gw:3001/mcp` → namespaced 3 tool(`deepwiki__ask_question`, `deepwiki__read_wiki_contents`, `deepwiki__read_wiki_structure`). worker profile policy가 backend 허용 |
| ⑥ **in-guest tools/call** (VM→gateway→DeepWiki) | **PASS** | 게스트가 `deepwiki__read_wiki_structure {"repoName":"modelcontextprotocol/servers"}` 호출 → **실 DeepWiki 문서 목차 수신**, workload `exit_code=0 timed_out=false` |
| ⑦ 경계 (a) source-IP 미등록 → 403 | **PASS** | host-origin(src `10.0.1.1`) `POST …/mcp`(tools/call·tools/list 각각) → **HTTP 403 `forbidden`**. daemon slog `mcp gateway: unresolved caller remote=10.0.1.1:… err="unknown caller 10.0.1.1"` |
| ⑦ 경계 (b) audit metadata-only | **PASS** | `audit/mcp.jsonl` 실물에 tool **이름**은 있으나 arguments/results **없음**(아래 발췌) |
| ⑦ 경계 (c) `/config` leak guard | **PASS** | ④와 동일 — `has_credential`(bool)만, 실 token 문자열·`args` 0. (이번 backend는 credential 없어 http backend 필드 노출만) |

## 왕복 증거 (in-guest, VM 내부 caller → gateway → DeepWiki)

게스트 workload 스크립트(`workloads/mcp-probe.sh`, `curl`+게스트 기본 도구)를
`PUT /vms/{id}/workspace` 업로드 후 `POST /vms/{id}/workloads/run`으로 VM 안에서 실행 —
control plane이 아니라 **VM 내부에서** gateway로 MCP 요청을 넣어 왕복을 실증했다.

게스트 stdout 발췌:

```
== /etc/hosts ephemera-gw ==
10.0.1.1 ephemera-gw
== injected mcp url ==
http://ephemera-gw:3001/mcp
== tools/list via gateway ==
{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"deepwiki__ask_question", …},
  {"name":"deepwiki__read_wiki_contents", …},{"name":"deepwiki__read_wiki_structure", …}]}}
== tools/call deepwiki__read_wiki_structure via gateway ==
{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"Available pages for
  modelcontextprotocol/servers:\n\n- 1 Introduction to Model Context Protocol Servers\n …"}],
  "isError":false}}
```

VM은 URL만 받았고 credential은 존재하지 않는다(DeepWiki no-auth). tool 이름은 backend
namespace로 접두(`deepwiki__…`)돼 aggregated catalog로 노출된다.

## 경계 실증 발췌

(a) 미등록 source-IP:

```
POST tools/call from host -> gateway: HTTP 403   body: forbidden
POST tools/list from host -> gateway: HTTP 403   body: forbidden
daemon slog: msg="mcp gateway: unresolved caller" remote=10.0.1.1:41386 err="unknown caller 10.0.1.1"
```

(b) `audit/mcp.jsonl` (실 왕복 1건 — metadata only, arguments/results 부재):

```json
{"kind":"tool","ms":238,"outcome":"ok","profile":"worker","server":"deepwiki","tool":"read_wiki_structure","ts":"2026-07-13T14:03:24Z","vm":"vm-1783951343492332006"}
```

`tool`은 이름만 있고 `repoName` 인자값도, 반환된 문서 본문도 기록되지 않는다(access-log privacy
invariant). tools/list와 403 거부는 audit에 남지 않는다(observe는 tool/resource/prompt **호출**만 기록).

## 결함 회부

### 신규(gateway): 없음

gateway 배선·정책·audit·경계는 실 backend·실 VM에 대해 기대대로 동작. `/etc/hosts` 주입,
source-IP caller 판정, namespacing, metadata-only audit, leak guard 모두 성립.

### 관측 (결함 아님 — anvil MCP gateway 범위 밖, 이 세션 인공물)

이 세션 중 첫 VM spawn이 `VM preparation: failed to open /etc/hosts …` `500`으로 한 번 실패했다.
근인은 **부분 golden image**였다: 시작 디버그로 daemon을 foreground 12s로 띄웠다가 죽이면서
debootstrap을 절단 → 부분 rootfs(top-level `debootstrap/ lost+found/ usr/ var/`, `/etc` 없음)가 남았고,
이어진 정식 기동의 golden-image staleness 체크(mtime 기반)가 이 부분 이미지를 "up to date"로
**재사용**했다. 부분 이미지를 삭제하고 정식 재빌드하니(full Debian rootfs, `/etc/hosts` =
`127.0.0.1 localhost goose-agent`) spawn·왕복이 모두 PASS.

이것은 **gateway 결함이 아니다**: `scripts/build_image.sh`(line 117)가 `/etc/hosts`를 생성하고,
provisioner의 hosts 주입(`O_APPEND|O_WRONLY`, `internal/storage/provisioner.go:331`)은 완전한 이미지에서
정상 성립한다. 다만 "kill/crash로 남은 부분 golden image를 staleness 체크가 감지 못하고 재사용"은
gateway와 무관한 별개 robustness 관찰이며, 이 세션의 조작 인공물이라 별도 회부는 하지 않고 여기 기록만 한다.

## 정리

- 검증 후 임시 `configs/mcp/servers.yaml`(gitignored) 제거, 테스트 VM 삭제(`/vms` = `[]`), daemon 정지.
- `~/anvil/snapshots` 4K ZFS 마운트·`configs/goose.yaml`·goose-secrets 불가침 확인. host 배포 트리는 유지.
- `~/anvil/DEPLOY_RECORD.txt`에 이 run 기록.

## 후속 작업

- 없음 (실 backend 배포 검증 완료로 PR #40 트랙 C의 마지막 잔여 종결).
