# anvil 기여 가이드

이 저장소는 `HardcoreMonk/anvil`이며 `steve-seungeui/ephemera` fork network를
유지한다. ephemera는 Firecracker MicroVM runtime engine이고, anvil은 그 runtime을
IronClaw MCP 실행 계층으로 통합하는 downstream product fork다.

기여 전에 [README.md](README.md), [CONTEXT.md](CONTEXT.md),
[AGENTS.md](AGENTS.md)를 먼저 읽는다. 문서가 충돌하면 `CONTEXT.md`를 우선한다.

## 로컬 개발 환경

필수 조건:

- Linux host. KVM E2E는 `/dev/kvm`이 필요하다.
- root 또는 `sudo` 권한.
- `curl`, `debootstrap`, `e2fsprogs`, `util-linux`, `jq`, `dmsetup`.
- Go toolchain. 현재 MCP SDK와 프로젝트 검증은 Go 1.25 이상을 기준으로 한다.

기본 빌드:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

KVM E2E:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

`e2e_test.sh`는 실제 Firecracker VM을 부팅한다. `configs/goose-secrets.yaml` 또는
profile별 secret file에는 로컬 LLM API key가 필요할 수 있으며, 이 파일들은 절대
커밋하지 않는다.

## 빌드 산출물

daemon은 첫 실행 시 다음 runtime artifact를 준비한다.

- `artifacts/golden-image.ext4`
- `artifacts/vmlinux.bin`
- `artifacts/firecracker`
- `artifacts/goose-agent`
- `artifacts/micro-init`

`goose-agent`와 `micro-init`은 VM 내부에서 실행되는 Linux amd64 binary다. 수동으로
빌드할 때 host 기본값으로 빌드하지 않는다. daemon의 bootstrap helper가 필요한
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 설정을 적용한다.

ephemera `v0.3.1` 기준으로 daemon은 `goose-agent`, `micro-init`, golden image의
stale input을 감지해 필요한 경우 자동 재빌드한다. 그래도 runtime artifact는
gitignored 상태로 유지한다.

## 설정과 secret

커밋 가능한 예시:

- `configs/goose.yaml.example`
- `configs/goose-secrets.yaml.example`
- `configs/profiles/*/goose.yaml.example`
- `configs/profiles/*/goose-secrets.yaml.example`
- `configs/profiles/*/system.md`

커밋 금지:

- `configs/goose.yaml`
- `configs/goose-secrets.yaml`
- `configs/profiles/*/goose.yaml`
- `configs/profiles/*/goose-secrets.yaml`
- 실제 provider API key, Bearer token, 고객 데이터

채팅, issue, 문서, commit message에도 실제 key를 남기지 않는다.

## 보안 불변 조건

- `POST /vms` 응답 외에는 `agent_token`을 노출하지 않는다.
- daemon restore 응답, flock 응답, MCP output, runtime audit, metrics, trace,
  snapshot GC audit에는 `agent_token`이 없어야 한다.
- upstream ephemera `v0.3.1`의 `POST /flocks` `agent_tokens` 응답 추가는 anvil에서
  채택하지 않는다.
- Town Wall message body는 `flocks/<flock_id>/TOWN_WALL.log`와 history 응답에
  남으므로 secret 전달 채널로 쓰지 않는다.

관련 변경을 하면 최소한 다음 검색을 수행한다.

```bash
rg -n "agent_token|agent_tokens|Authorization|Bearer|secret" .
```

검색 결과는 정책상 허용된 위치인지 직접 확인한다.

## 테스트 기준

일반 변경:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n e2e_test.sh
bash -n scripts/anvil-mcp-e2e.sh
git diff --check
```

다음 경로를 건드리면 KVM E2E를 실행한다.

- `cmd/goose-daemon/`
- `cmd/goose-agent/`
- `cmd/micro-init/`
- `internal/network/`
- `internal/storage/`
- `internal/vm/`
- `internal/orchestrator/`
- `scripts/build_image.sh`
- `scripts/gtwall`

MCP adapter만 변경한 경우에도 daemon-backed smoke가 필요한 변경이면 다음을
확인한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

## 주의가 필요한 영역

KVM/Firecracker:
VM cold spawn, snapshot create, restore는 서로 다른 경로다. 하나를 고치면서 다른
경로의 cleanup을 깨뜨리기 쉽다. 실패 경로에서도 TAP/IP, dm-snapshot, loop device,
bind mount, sparse COW file을 정리해야 한다.

Snapshot:
실행 중인 원본 VM의 snapshot은 restore하지 않는다. diff snapshot이 참조 중인 full
snapshot은 삭제하지 않는다.

Goosetown:
flock metadata persistence는 daemon restart 뒤 read-mostly registry와 Town Wall
history를 복구한다. 이전 daemon process와 함께 종료된 Firecracker VM은 자동
재시작하지 않는다. watchdog의 `dead` 상태는 live probe 결과다.

MCP:
`cmd/anvil-mcp`와 `internal/anvilmcp`는 얇은 stdio adapter다. VM lifecycle 의미는
ephemera daemon API가 소유한다. MCP tool이 host-local cleanup 의미를 재해석하지
않는다.

## PR 기준

- 한 PR에는 하나의 논리 변경만 담는다.
- 사용자/운영자 문서는 한국어로 작성한다. API 경로, env var, command, file path,
  code identifier는 원문을 유지한다.
- 로컬 secret과 runtime artifact를 포함하지 않는다.
- upstream ephemera sync는 merge commit으로 수행하고 rebase/history rewrite를 하지
  않는다.
- release note와 release checklist는 release prep 작업에서 함께 정리한다.

보안 이슈는 공개 issue 대신 GitHub Security Advisory 등 비공개 경로로 보고한다.
