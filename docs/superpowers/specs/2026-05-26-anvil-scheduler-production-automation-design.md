# Anvil Scheduler Production Automation Design

## 목표

anvil runtime scheduler를 `anvil-v0.3.0` release candidate에서 운영자가 재현 가능하게
설치, 기동, 검증할 수 있도록 production automation을 보강한다. runtime baseline은
ephemera `v0.3.6` tag로 유지하며, upstream `main`의 `v0.4.0 PR-A` 저장소 변경은 이번
범위에 포함하지 않는다.

이번 작업의 산출물은 scheduler core scheduling algorithm이 아니라 운영 자동화다.
구체적으로는 systemd installer, HTTP smoke harness, 문서화된 runbook/checklist가 같은
계약을 공유하도록 만든다.

## 현재 상태

현재 scheduler binary는 `cmd/anvil-scheduler`에 있으며 다음 환경 변수를 읽는다.

- `ANVIL_SCHEDULER_ADDR` 기본값: `127.0.0.1:3010`
- `ANVIL_SCHEDULER_STATE`
- `ANVIL_SCHEDULER_QUOTA_STORE`

HTTP service는 `internal/anvilmcp.SchedulerService`가 소유한다.

- `GET /health`
- `GET/PUT /hosts`
- `GET /placements`
- `POST /reconcile`
- `POST /schedule/spawn`
- `POST /schedule/restore`

systemd 설치 surface는 이미 존재한다.

- `deploy/systemd/anvil-scheduler.service`
- `deploy/systemd/anvil-scheduler.env.example`
- `scripts/install-anvil-scheduler-systemd.sh`

하지만 현재 installer는 설치와 선택적 start까지만 수행한다. 운영자가 새 host에서
설치가 실제로 성공했는지 확인하려면 curl request를 수동으로 조합해야 하며, release
checklist와 runbook도 이 검증 절차를 명시적으로 닫지 못한다.

## 비목표

- ephemera upstream `main`의 `v0.4.0 PR-A` 변경을 merge하지 않는다.
- scheduler algorithm, quota model, placement data model을 바꾸지 않는다.
- cross-host VM migration, snapshot replication, scheduler-aware flock placement를
  추가하지 않는다.
- multi-node HA, leader election, database-backed state store를 추가하지 않는다.
- public internet exposure를 기본값으로 만들지 않는다. scheduler는 loopback bind와
  private network 또는 reverse proxy 뒤 운영을 기본 전제로 유지한다.
- systemd가 없는 환경을 위한 별도 process supervisor를 추가하지 않는다.

## 설계

### 1. Scheduler Smoke Harness

새 script `scripts/anvil-scheduler-smoke.sh`를 추가한다. 이 script는 실행 중인 scheduler
HTTP endpoint를 대상으로 최소 운영 계약을 검증한다.

기본값:

```bash
ANVIL_SCHEDULER_BASE_URL=http://127.0.0.1:3010
```

지원 옵션:

- `--base-url URL`: 대상 scheduler URL
- `--host-id ID`: smoke용 host id, 기본값은 `anvil-scheduler-smoke-*` 자동 생성
- `--json-out PATH`: 결과 summary JSON 저장

검증 순서:

1. `GET /health`가 HTTP `200`과 `{"status":"ok"}`를 반환한다.
2. `PUT /hosts`로 smoke host를 등록한다.
3. `GET /hosts`에서 등록한 host id가 보인다.
4. `POST /schedule/spawn`이 등록 host를 선택하는 schedule decision을 반환한다.
5. `PreferredHosts` 없는 `POST /schedule/spawn`이 등록한 smoke-only host를 선택하지
   않는다.
6. `GET /placements`가 JSON으로 반환된다.

smoke는 scheduler API에 이미 존재하는 기능만 사용한다. 실패 시 단계 이름, HTTP status,
response body 일부를 stderr에 남기고 non-zero로 종료한다. token, provider key,
`agent_token`은 입력받거나 출력하지 않는다.

### 2. Installer Verification Mode

`scripts/install-anvil-scheduler-systemd.sh`에 `--verify` option을 추가한다.

동작:

- `--verify`는 installer 마지막 단계에서 `scripts/anvil-scheduler-smoke.sh`를 실행한다.
- `--verify`만 지정되고 `--start`가 없으면 기존에 실행 중인 service를 대상으로 검증한다.
- `--start --verify`는 service restart 후 smoke를 실행한다.
- `--dry-run --verify`는 실제 curl을 실행하지 않고 어떤 smoke command가 실행될지
  출력한다.

preflight:

- `curl`이 없으면 명확한 오류로 실패한다.
- `jq`는 있으면 JSON 검사에 사용하고, 없으면 Go/POSIX fallback이 아니라 script 내부의
  최소 string 검증으로 동작한다.
- `--verify` 실패는 installer 전체 실패로 처리한다.

### 3. State Path와 권한 검증

installer는 systemd service와 env file이 기대하는 write path가 일치하는지 사전에
확인한다.

- `ANVIL_SCHEDULER_STATE`와 `ANVIL_SCHEDULER_QUOTA_STORE`의 directory를 생성한다.
- directory owner는 scheduler service user/group이다.
- env file은 root 소유, scheduler group read 가능, mode `0640`을 유지한다.
- systemd unit은 `/var/lib/anvil` write path를 유지한다.

1차 범위에서는 service unit의 `ReadWritePaths=/var/lib/anvil` 계약을 유지한다. 운영자가
state path를 `/var/lib/anvil` 밖으로 override하는 경우에는 installer가 경고와 함께
해당 path가 unit hardening과 맞지 않을 수 있음을 알린다. unit template 자동 수정은 이번
범위에 넣지 않는다.

### 4. 문서와 Release Checklist

운영 문서를 다음 기준으로 갱신한다.

- README는 scheduler production automation이 존재한다는 짧은 entry point만 둔다.
- `docs/operations/runbook.md` 또는 scheduler runbook에는 install, start, verify,
  failure triage를 명령어 단위로 적는다.
- `docs/operations/release-checklist.md`는 `anvil-v0.3.0` release candidate 검증 항목에
  scheduler smoke를 포함한다.
- `docs/operations/2026-05-26-anvil-v0.3.0-release-candidate-handoff.md`는 scheduler
  automation excluded scope 문구를 실제 구현 상태로 갱신한다.

## Error Handling

smoke harness는 실패를 한 가지 exit code로만 뭉치지 않고 사람이 읽을 수 있는 단계명을
출력한다.

- `health_failed`
- `host_put_failed`
- `host_list_failed`
- `schedule_spawn_failed`
- `smoke_only_failed`
- `placements_failed`
- `json_write_failed`

summary JSON을 요청받은 경우에는 다음 구조를 쓴다.

```json
{
  "ok": false,
  "base_url": "http://127.0.0.1:3010",
  "failed_step": "schedule_spawn_failed",
  "host_id": "anvil-scheduler-smoke-12345"
}
```

성공 시:

```json
{
  "ok": true,
  "base_url": "http://127.0.0.1:3010",
  "host_id": "anvil-scheduler-smoke-12345",
  "selected_host_id": "anvil-scheduler-smoke-12345"
}
```

## 보안

- scheduler는 기본적으로 loopback address에 bind한다.
- smoke script는 scheduler endpoint 외부에 VM, guest agent, provider secret을 전달하지
  않는다.
- installer는 env file에 scheduler path와 bind address만 기록한다.
- `agent_token`은 scheduler surface에 등장하지 않으며, 이번 automation에서도 출력하지
  않는다.
- 공개 노출이 필요한 경우에는 기존 보안 지침처럼 TLS 종료 reverse proxy와 private network
  policy 뒤에서 수행한다.

## 테스트 전략

TDD 순서는 다음과 같이 유지한다.

1. `scripts/anvil-scheduler-smoke.sh`의 fake scheduler 대상 실패/성공 테스트를 먼저
   작성한다.
2. smoke harness가 없어서 테스트가 실패하는 것을 확인한다.
3. smoke harness를 최소 구현하고 테스트를 통과시킨다.
4. installer `--verify` dry-run과 command construction 테스트를 추가한다.
5. installer를 수정하고 테스트를 통과시킨다.
6. scheduler binary를 실제로 띄워 smoke script를 실행한다.

필수 검증:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/install-anvil-scheduler-systemd.sh
```

실제 service smoke는 root/systemd host에서 다음 명령으로 확인한다.

```bash
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

## 완료 기준

- feature branch는 ephemera `v0.3.6` sync commit 위에 유지된다.
- upstream `main`의 `v0.4.0 PR-A` 코드는 포함되지 않는다.
- scheduler smoke script가 standalone으로 실행 가능하다.
- installer가 `--verify`와 `--start --verify`를 지원한다.
- dry-run output이 실제 변경 없이 검증 command를 보여준다.
- runbook, release checklist, handoff 문서가 구현 상태와 일치한다.
- 일반 Go build/test와 shell syntax check가 통과한다.
