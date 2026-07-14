# 2026-07-11 scheduler-ops-deploy handoff

anvil-scheduler 실운영 systemd 배포 + host inventory polling 상주화, end-user
installer 실 systemd host 검증, runtime MCP Gateway 운영 정책/dry-run 세 트랙의 배포·검증
기록. branch `feature/scheduler-ops-deploy`.

## 서버 상태 (검증 종료 시점)

| host | 주소 | 역할 | 상태 |
|------|------|------|------|
| host-a | 192.168.1.19 | anvil-scheduler 실운영 | **상주 유지** — `anvil-scheduler.service` active+enabled, control loop running |
| host-b | 192.168.1.20 | polling 대상 / installer 검증 대상 | 원복 — `~/anvil` 수동 배포본(0feb9fb) 유지, daemon 정지, ephemera 설치본 purge 완료 |

- 두 host의 ZFS `rpool/anvil-snapshots` mount(`~/anvil/snapshots`) 검증 전/후 불변. host-b `goose-br0`·purecvisor 데이터셋 불변.
- **신규 상주 유닛**: host-a `anvil-scheduler.service` (이 트랙에서 처음 systemd로 상주). zone `ops/units.yaml` 반영은 controller 몫.

## Sub-track (i) — scheduler 실배포 + polling 상주화: PASS

코드 상태: control loop는 이미 service에 상주(`loop.Start`, `cmd/anvil-scheduler/main.go`),
`cmd/anvil-scheduler`가 전체 `ANVIL_SCHEDULER_*` env를 이미 read(테스트
`TestLoadSchedulerConfigDefaultsAndEnv`). 유일한 배선 공백은 설치 스크립트가 env 파일에
ADDR/STATE/QUOTA_STORE 3개만 기록해 systemd 배포 scheduler가 poll할 host가 없던 것.

변경: `scripts/install-anvil-scheduler-systemd.sh`가 operator가 설정한 polling knob
(`HOSTS_FILE`, `POLL_INTERVAL`, `RECONCILE_INTERVAL`, `HOST_TIMEOUT`,
`FAILURE_THRESHOLD`, `API_TOKEN`, `REQUIRE_PERSISTENCE`)만 env 파일에 기록하고,
`ANVIL_SCHEDULER_HOSTS_SRC`로 hosts JSON을 `ANVIL_SCHEDULER_HOSTS_FILE`
(기본 `/etc/anvil/scheduler-hosts.json`)에 설치. API token은 env 파일(0640 root:group)에만
기록되고 dry-run preview에서는 `<redacted>`. TDD: `scripts/install_anvil_scheduler_systemd_test.go`.

배포: host-a에 static `anvil-scheduler` 빌드본 + 설치 스크립트 rsync →
`sudo env ANVIL_SCHEDULER_HOSTS_SRC=... POLL_INTERVAL=5s RECONCILE_INTERVAL=10s
HOST_TIMEOUT=2s FAILURE_THRESHOLD=3 bash scripts/install-anvil-scheduler-systemd.sh
--no-build --start` → active+enabled.

검증 증거 (host-a `127.0.0.1:3010`):
- `/health` → `{"status":"ok"}`; `/metrics` → `anvil_scheduler_control_loop_running 1`.
- `/control-loop/status` → `running:true`, poll=5s, reconcile=10s. host-b가 healthy 관측(`failure_count:0`, `last_success_at` 갱신) — polling이 host-b `/health`,`/vms`를 실제로 읽음.
- **toggle**: host-b daemon 정지 → `failure_count` 증가 후 `unhealthy`, `/hosts` `healthy:false`, `anvil_scheduler_host_status_count{status="unhealthy"} 1`. host-b daemon 재기동 → `healthy`, `failure_count:0`, `last_success_at` 갱신.
- **restart 지속성**(reboot proxy): `systemctl restart anvil-scheduler` 후 active+enabled, control loop running, hosts 파일에서 host-b 재로드 후 healthy 재관측.
- **redaction**: `journalctl -u anvil-scheduler`는 bind 주소(`anvil scheduler service on 127.0.0.1:3010`)만 남김. token·host endpoint 없음. host endpoint는 `/control-loop/status`의 observation error(예: `dial tcp 192.168.1.20:3000: connection refused`)에 진단용으로만 노출되고 `/metrics`에는 없음.
- standalone smoke: `bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010` → passed.

CONTEXT drift: `CONTEXT.md` '고정된 런타임 계약'이 scheduler env를 ADDR/STATE/QUOTA_STORE로만
기재했으나 코드는 이미 전체 `ANVIL_SCHEDULER_*` set을 read. 이번에 전체 set을 계약에 반영.

## Sub-track (ii) — installer 실 systemd 검증: STOP-and-record (구조적 결함)

FULL variant tarball 로컬 빌드(`sudo bash scripts/build_release.sh
anvil-v0.7.0-schedops-verify`) — kernel/firecracker sha256 검증 통과, golden image
bake, FULL(252M)+SLIM(27M)+`.sha256` 산출. host-b로 옮겨 `install.sh` 실행.

**작동한 부분**: `install.sh` 파일 배치(`/opt/ephemera`), provider/model/key config 작성,
token 프롬프트(unauthenticated 선택), systemd `ephemera.service` 등록. `uninstall.sh`
default(service+symlink 제거, `/opt/ephemera` 보존, `/tmp` scratch 정리)와 `--purge`
(`/opt/ephemera` 완전 삭제) 모두 정상. 격리 완벽 — `~/anvil` 수동 배포본·snapshots
mount·goose-br0·purecvisor 전부 불변.

**구조적 결함 (VM smoke BLOCKED)**: FULL release 설치본으로 `ephemera` daemon이
기동하지 못함. `ephemera-daemon`이 startup step 1에서
`storage.EnsureGooseAgent(gooseAgentPath, cwd)` 호출(`cmd/goose-daemon/main.go:98`) →
`GooseAgentSourceHash`가 `cmd/goose-agent` **소스 트리를 WalkDir**
(`internal/storage/provisioner.go:487,508`) → release 설치본(`/opt/ephemera`)에 소스가
없어 `fatal: ensure goose-agent err="walk goose-agent sources: lstat
/opt/ephemera/cmd/goose-agent: no such file or directory"` → status 1로 죽고
`Restart=on-failure` 루프.

- 대조: `EnsureMicroInit`은 mtime 기반이라 소스 부재를 tolerate("micro-init up to date"), `EnsureGooseAgent`는 source-hash 기반이라 소스가 없으면 stamp 확인 전에 실패.
- 계약 위반: `INSTALL.md`는 release 설치가 "no Go toolchain or source checkout needed"라고 명시하나 `EnsureGooseAgent`가 소스 트리를 하드 요구. `build_release.sh`는 `artifacts/goose-agent`는 담지만 `.sha256` stamp도 소스도 담지 않음.
- daemon이 step 1에서 죽어 network manager/bridge/VM 셋업에 도달하지 못함 → host-b 부작용 없음(격리가 유지된 이유).
- **범위 밖**: 이 트랙은 검증 트랙이므로 수정하지 않고 기록만. fix는 별도 사이클 필요(예: `EnsureGooseAgent`가 소스 부재 + shipped `artifacts/goose-agent`(+stamp) 존재 시 prebuilt를 신뢰하도록, 그리고 `build_release.sh`가 stamp를 동봉하도록).
- **FIX SLICE (branch `fix/release-daemon-goose-agent`, 머지 전)**: `EnsureGooseAgent`가 `cmd/goose-agent` 소스 부재 시 shipped `artifacts/goose-agent`+`.sha256` stamp를 current로 수용(재빌드 skip, INFO 1줄), 부재 시 진단 에러. `build_release.sh`가 daemon 헬퍼 `print-goose-agent-source-hash`로 stamp 동봉. host-b(192.168.1.20) FULL 재검증: daemon 기동 성공 + VM spawn(`{"status":"idle"}`)/rm smoke + `uninstall.sh --purge` 원복(격리 유지). 개발 경로 불변. 상세: `docs/operations/release-checklist.md` "Release-install 기동 게이트".

## Sub-track (iii) — runtime MCP Gateway 운영 정책 + dry-run: PASS (문서 + dry-run)

> **Update 2026-07-13**: 여기서 남긴 "실 backend operator 배포 검증 잔여"는 **종결**. DeepWiki
> (public no-auth http MCP)로 host-b 실 배포 검증 PASS — 실 backend `up:true`, VM 내부 왕복,
> 경계 3종(미등록 source-IP `403`, audit metadata-only, `/config` leak guard). 신규 gateway
> 결함 없음. 상세: [`docs/operations/2026-07-13-mcp-gateway-deployment-verification-run.md`](2026-07-13-mcp-gateway-deployment-verification-run.md).

- 문서: `runbook.md`에 "runtime MCP Gateway 운영 정책" 절 추가(backend→profile 바인딩 기준, rate-limit/burst 권고, secrets.yaml 운영 규율, 배포 체크리스트).
- 테스트 레벨 leak guard: `go test ./cmd/goose-daemon -run 'TestHandleConfigMCPServers_NeverLeaksArgsOrCredential|TestConfigMCP...'` 6개 PASS.
- **live dry-run** (host-b `~/anvil` 수동 daemon, 소스 존재로 기동 가능): 임시 `configs/mcp/servers.yaml`(credential 있는 http backend) + `secrets.yaml` 배치 후 `EPHEMERA_MCP_ENABLED=1`로 기동 → 로그 `mcp gateway configured endpoint=http://ephemera-gw:3001/mcp bind=10.0.1.1:3001 servers=1`, `/config/mcp` → `{"enabled":true,"server_count":1}`, `/config/mcp/servers` → `has_credential:true`·url 노출·`up:false`(fake backend), **credential 토큰 문자열 응답에 0회 등장**. dry-run 후 임시 config·log 제거, daemon 정지.

## Next Action / Follow-Up Tasks

1. (controller) zone `ops/units.yaml`·`ops/projects.yaml`에 host-a `anvil-scheduler.service` 신규 상주 유닛 반영, `wiki/entities/` 동기화.
2. (별도 사이클) FULL release daemon 기동 결함 수정: `EnsureGooseAgent` release-layout tolerance + `build_release.sh`의 goose-agent stamp 동봉. 수정 후 host-b installer 재검증(VM 1개 생성/삭제 smoke 포함). → **완료(branch `fix/release-daemon-goose-agent`, 머지 대기)**: 위 결함 회부 절 FIX SLICE 참조. controller가 diff/gate 검토 후 머지.
3. (운영 판단) host-a scheduler `/etc/anvil/scheduler-hosts.json`는 현재 host-b만 가리킴(검증 fixture). 실운영 poll 대상 host 인벤토리를 controller가 확정해 갱신.
4. host-a `~/schedops/`는 설치 소스 스테이징(binary+installer+hosts json). 재현용으로 보존; 불필요 시 controller가 정리.
