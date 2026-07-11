# anvil 운영 Runbook

이 문서는 단일 host 운영자가 anvil/ephemera daemon을 빌드, 시작, 점검, 정리할 때
사용하는 절차다. 명령의 token 값은 실제 값을 문서에 남기지 말고 shell 환경 변수로만
전달한다.

## 빌드

```bash
go build -o anvil-daemon ./cmd/goose-daemon
go build -o anvil-mcp ./cmd/anvil-mcp
go build -o anvil-scheduler ./cmd/anvil-scheduler
```

## 운영 시작

운영 환경은 control-plane token을 설정해서 시작한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN" ./anvil-daemon
```

client 이름을 분리해야 하면 쉼표 구분 형식을 사용한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN,ci:$CI_TOKEN" ./anvil-daemon
```

공개 노출은 TLS를 종료하는 reverse proxy 뒤에서만 수행한다. daemon을 인터넷에 직접
공개하지 않는다.

runtime scheduler service를 별도 process로 운영하는 경우 state path를 명시한다.

```bash
ANVIL_SCHEDULER_ADDR=127.0.0.1:3010 \
ANVIL_SCHEDULER_STATE=/var/lib/anvil/scheduler.json \
ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/tenants.json \
./anvil-scheduler
```

systemd로 운영할 host에서는 설치 스크립트를 먼저 dry-run으로 확인한다. 기본 unit은
`127.0.0.1:3010`에 bind하고 `/var/lib/anvil/{scheduler.json,tenants.json}`을
state로 사용한다. `--verify`와 standalone smoke harness는 host에 `curl`,
`python3`가 있어야 실행된다.

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이미 실행 중인 service만 재검증할 때는 다음 명령을 사용한다.

```bash
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```

smoke harness는 기본 host id를 `anvil-scheduler-smoke-*`로 생성하고,
`GET /hosts`로 같은 host id의 기존 inventory record가 없는지 먼저 확인한다.
충돌이 있으면 `PUT /hosts` 전에 실패하므로 운영 host record를 덮어쓰지 않는다.
검증 중 등록한 fake host는 `DELETE /hosts/{name}`로 정리한다. 본 검증은 cleanup
실패를 성공으로 취급하지 않는다. 검증용 fake host는 `smoke_only: true`로 등록되며,
smoke 요청의 `PreferredHosts`에 host id가 명시된 경우에만 선택된다. smoke harness는
`PreferredHosts`가 없는 추가 `/schedule/spawn`도 실행해 일반 fallback scheduling이
smoke-only host를 무시하는지 확인한다.

설치 후 설정을 바꿔야 하면 `/etc/anvil/anvil-scheduler.env`를 수정한 뒤
`sudo systemctl restart anvil-scheduler.service`를 실행한다. scheduler service는
자체 인증 계층을 두지 않으므로 loopback/private network 또는 reverse proxy policy
뒤에서만 노출한다.

`--verify`가 실패하면 먼저 `sudo systemctl status anvil-scheduler.service`와
`journalctl -u anvil-scheduler.service -n 100 --no-pager`를 확인한다.
`health_failed`는 service bind/start 문제, `host_put_failed`는 state path 권한 또는
JSON body 처리 문제, `schedule_spawn_failed`는 host inventory나 quota/usage 입력
문제를 우선 의심한다. `metrics_failed`는 scheduler service가 `/metrics`를 제공하지
않거나 smoke가 `anvil_scheduler_control_loop_running` line을 찾지 못한 상태다.

## Daemon API 확인

daemon process 상태와 API 인증 경로는 top-level `/health` endpoint로 확인한다.

로컬 인증이 꺼진 개발 모드:

```bash
curl http://127.0.0.1:3000/health
```

운영 token이 필요한 환경:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/health
```

VM guest agent health는 daemon proxy를 통해 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID/health
```

Prometheus text metrics와 VM별 JSON metrics:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/metrics
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/metrics/vms
```

runtime scheduler service 상태:

```bash
curl http://127.0.0.1:3010/health
curl http://127.0.0.1:3010/placements
```

runtime scheduler control loop 상태:

```bash
curl http://127.0.0.1:3010/control-loop/status
```

runtime scheduler metrics:

```bash
curl http://127.0.0.1:3010/metrics
```

`anvil_scheduler_persistence_degraded 1`이면 state file 저장 경로 권한과 disk 상태를
먼저 확인한다. `anvil_scheduler_host_status_count{status="unhealthy"}`가 0보다 크면
`/control-loop/status`의 host observation과 daemon host `/health`를 함께 확인한다.

`degraded`/`unhealthy` host는 신규 placement에서 제외되며, 기존 VM placement는
`suspect_vm_placements`로 남는다. host가 다시 응답하면 reconciliation이 daemon
`GET /vms` 결과로 stale placement를 정리한다.

## Tenant, egress, audit 확인

tenant quota/usage state:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/tenants
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/tenants/$TENANT_ID
```

profile egress policy를 사용할 때는 profile별 `egress.json`을 먼저 확인한다. 기본
위치는 `configs/profiles/{profile}/egress.json`이고, 운영 배포에서는
`EPHEMERA_EGRESS_PROFILE_DIR` 또는 `ANVIL_EGRESS_PROFILE_DIR`로 별도 directory를
지정할 수 있다.

```bash
sed -n '1,120p' configs/profiles/$PROFILE/egress.json
```

runtime audit 조회:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:3000/audit/runtime?tenant_id=$TENANT_ID&limit=50"
```

## Goosetown flock 점검

live flock 목록과 단일 flock 상태:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks/$FLOCK_ID
```

Town Wall history 조회:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID/wall/history
```

flock 삭제는 daemon이 소유한 member VM teardown 경로를 실행한다. cross-host routed
flock은 home daemon의 hub delete가 relay_token admission도 함께 revoke하므로, hub와
각 member host의 relay flock을 모두 해제해야 완전히 정리된다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID
```

Town Wall message body는 `flocks/<flock_id>/TOWN_WALL.log`와 history 응답에
남는다. provider token, API key, `agent_token` 같은 secret을 Town Wall에 게시하지
않는다.

`metadata.json`이 있는 flock은 daemon restart 뒤 registry와 Town Wall log가 복구된다.
spawn-path member VM은 `vms/<vm_id>/state.json` 기반으로 cold-restart되어 같은 VM ID,
IP, TAP, MAC, agent token, agent URL을 유지한다. memory state와 진행 중인 task는
보존되지 않는다. COW-mode VM과 snapshot-restored VM은 자동 복구 대상이 아니다.
agent `status=dead`는 watchdog이 연속 health probe 실패를 감지했을 때 표시된다.

daemon 재시작으로 hub/relay flock 등록과 relay-token admission이 사라진 경우,
`members_only` 모드 adapter가 `ANVIL_MCP_RECONCILE_INTERVAL`(기본 60s) 주기로
자동 재등록한다. 수동 개입은 adapter가 꺼져 있거나 `0`으로 비활성화된 경우에만
필요하다.

wall/gtcall relay hop(member→home, home→target)은 dial 단계 실패(connection
refused/no route 등 — 요청이 상대에 도달하지 않았음이 보장되는 경우)에 한해
총 3회 시도(1s→2s backoff, 최악 +3s)로 짧은 네트워크 순단을 자동 흡수한다.
그 외 실패(HTTP 응답, reset/EOF, ctx 만료)는 재시도 없이 즉시 반환하므로
agent 쪽 재시도에 맡긴다.

### Home 재선출 failover 관측과 수동 fail-back

routed flock의 home host가 연속 `homeFailureThreshold`회(상수, 기본 3)
dial-계열로 응답하지 않으면 adapter(`cmd/anvil-mcp`)의 reconcile 루프가 자동
재선출한다. 새 home은 `record.Agents` 순서상 첫 생존 host(구 home 제외)이고,
생존 후보가 없으면 no-op으로 다음 reconcile 주기에 재평가한다.

**관측**: adapter stderr 로그에서 다음 형태의 라인을 확인한다(flock/host
식별자만 남고 daemon 주소·토큰은 노출되지 않는다).

```
anvil-mcp: routed flock "<flock_id>" home failover "<old_host>" -> "<new_host>" (canonical wall restarts empty on the new home)
```

adapter가 `members_only` 모드로 쓰는 `scheduler_state_path`(설정
`scheduler_state_path`, 즉 placements.json)에서 해당 flock의 `home_host`가
새 host로 바뀌었는지 직접 확인할 수도 있다.

```bash
jq '.routed_flocks["<flock_id>"].home_host' "$SCHEDULER_STATE_PATH"
```

**전환 창**: 최대 ~`homeFailureThreshold`(3) × `ANVIL_MCP_RECONCILE_INTERVAL`
(기본 `60s`) + 전환에 걸리는 시간이다 — 기본 설정 기준 최대 ~3분. 이 창 동안
wall post/조회와 gtcall은 기존과 동일하게 502 + bounded retry(dial-실패 한정
총 3회 시도)로 관측된다.

**wall 손실 계약**: 전환 후 새 home은 빈 `TOWN_WALL.log`에서 seq를
재시작한다. 구 home 디스크의 기록은 지워지지 않지만 새 wall로 병합되지
않는다 — flock을 운영하는 사람 관점에서는 agent에게 전환 시점 이전 wall
메시지가 사라진 것으로 보인다. 전환 전후로 wall history를 비교해 달라는
문의를 받으면 이 계약을 먼저 설명한다.

**자동 fail-back은 없다.** 구 home이 부활해도 adapter는 새 home을 계속
유지한다(flap 방지) — 구 home은 다음 reconcile에서 relay로 강등돼 자동
heal된다(stale hub는 어차피 아무도 참조하지 않는다). 원래 host로 되돌리려면
다음 수동 절차를 따른다.

1. adapter를 중지한다(reconcile 루프가 되돌린 설정을 다시 자동 전환하지
   않도록).
2. `scheduler_state_path`(placements.json)에서 해당 flock의 `home_host`를
   원하는 host로 직접 수정한다.
3. adapter를 재기동한다.
4. 다음 reconcile 주기가 hub 승격/relay 강등을 자동 수행한다 — failover와
   동일한 배관(`RegisterDistributedFlock`/`RegisterRelayFlock`)을 탄다.

경고: 이 수동 fail-back도 재선출과 동일하게 wall 손실 계약을 따른다 —
되돌아간 host의 wall은 다시 빈 log에서 시작된다. 이전 host로 복귀한다고
해서 그 host가 과거에 쌓았던 `TOWN_WALL.log` 내용이 자동으로 복원되지
않는다.

## 일반 검증

문서와 code path가 함께 맞는지 보는 기본 검증:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

전체 host smoke는 일반 문서 검증에 필요하지 않다. 다음 조건을 갖춘 host에서
Firecracker/KVM 통합 경로를 확인할 때만 실행한다.

- `/dev/kvm` 접근 가능
- root 권한
- Firecracker 실행 가능
- 로컬 `configs/goose-secrets.yaml`에 LLM secret 준비

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
go build -o anvil-scheduler ./cmd/anvil-scheduler
sudo bash e2e_test.sh
```

cross-host shared Town Wall 경로는 별도 KVM e2e로 확인한다. 실제 member VM의
`gtwall` post가 in-VM agent → member daemon(relay flock) → relay hop을 거쳐
전달되는지 검증하며, home daemon은 두 real daemon이 guest bridge subnet에서
충돌하는 것을 피하기 위해 stub HTTP recorder로 대체한다.

```bash
sudo -n bash scripts/anvil-cross-host-wall-e2e.sh
```

cross-host gtcall 경로도 별도 KVM e2e로 확인한다. 실제 member VM의 `gtcall`이
in-VM agent → member daemon(relay flock) → call hop(`call_token`)을 거쳐 home으로
전달되고 응답이 guest까지 왕복하는지 검증한다. 이 스크립트는 member daemon을
**auth-on**(`EPHEMERA_API_TOKENS`)으로 띄운다 — guest의 `relay_token` 기반 call
진입 admission(guest 능력 토큰이 그 flock의 wall과 `call`을 모두 admit)은
`authMiddleware`가 `cp.clients`를 확인하는 경로이므로, auth-off daemon에서는 그
확인 자체가 short-circuit되어 실경로 검증이 안 되기 때문이다(wall e2e가 auth-off로
돌다 놓쳤던 결함 class와 동일 — `docs/operations/2026-07-08-cross-host-gtcall-handoff.md`
참고). home daemon은 wall e2e와 동일하게 stub HTTP recorder로 대체한다.

```bash
sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh
```

### VM workload E2E

VM 내부 서비스 설치, 기동, host-to-VM 접근, 기초 성능 artifact를 확인할 때는
workload E2E를 실행한다. 이 검증은 root/KVM/network 조건이 필요하며, VM 내부에서
`apt-get`을 사용하므로 outbound와 DNS 경로도 함께 검증한다.
이 workload 경로는 `/workloads/run`을 사용하므로 Gemini/Goose provider key 없이도
서비스 설치와 host-to-VM probe를 검증한다. real LLM 검증은 기존 `/tasks` 기반
suite에서 별도로 수행한다.

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo -n bash scripts/vm-workload-e2e.sh
```

이미 daemon을 직접 띄운 상태에서 재사용하려면 다음처럼 실행한다.

```bash
ANVIL_WORKLOAD_REUSE_DAEMON=1 \
ANVIL_WORKLOAD_API=http://127.0.0.1:3000 \
bash scripts/vm-workload-e2e.sh
```

재사용하는 daemon이 인증을 요구하면 `ANVIL_WORKLOAD_API_TOKEN=$TOKEN`을 함께
전달한다.

결과 artifact는 기본적으로 `/tmp/anvil-workload-e2e-<timestamp>/` 아래에 남는다.
핵심 파일은 `summary.json`, `nginx-run.json`, `go-http-run.json`, `nginx.log`,
`go-http.log`, `bench.txt`, `host-bench.txt`이다. 이 파일에는 provider token, API key,
control-plane token, agent token을 남기지 않는다.

daemon이 이미 실행 중이면 MCP adapter smoke도 별도로 확인할 수 있다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

`flock` 모드는 `anvil_spawn_flock`, `anvil_list_flocks`, `anvil_post_townwall`,
`anvil_get_townwall_history`, `anvil_delete_flock`을 실제 daemon-backed MCP tool
call로 검증한다.

## VM 목록과 정리

실행 중인 VM 목록:

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
```

VM 삭제와 host resource 정리는 daemon API를 우선 사용한다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID
```

삭제 실패 후에도 수동 파일 삭제부터 하지 않는다. 먼저 VM 목록, daemon log, network
상태를 확인하고 같은 `DELETE /vms/{vm_id}`를 재시도한다. stale TAP/IP 대응 절차는
[disaster-recovery.md](disaster-recovery.md)를 따른다.

## Snapshot GC dry-run

GC는 기본이 dry-run이다. 아래 명령은 삭제 후보와 보호 이유만 계산한다.

```bash
curl -X POST http://127.0.0.1:3000/snapshots/gc \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240}'
```

주요 policy:

- `older_than_seconds`: 지정한 초보다 오래된 snapshot을 후보로 본다.
- `keep_last_per_vm`: `source_vm_id`별 최신 N개 snapshot을 보호한다.
- `max_total_bytes`: 전체 snapshot size 상한이다. `0`이면 비활성화된다.
- diff snapshot이 참조 중인 full snapshot은 삭제 후보에서 보호된다.

## Snapshot GC apply

실제 삭제는 `apply:true`가 있을 때만 수행된다. 먼저 dry-run 응답의 `candidates`,
`protected`, `errors`를 확인한 뒤 apply한다.

```bash
curl -X POST http://127.0.0.1:3000/snapshots/gc \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240,"apply":true}'
```

`apply:true` 호출은 `snapshots/gc-audit.jsonl`에 count-only audit record를 append한다.
이 audit record에는 snapshot metadata 전체나 `agent_token`이 들어가지 않는다.

## Snapshot cross-host replication

복제 전 source/target daemon과 scheduler 상태를 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://host-a:3000/health
curl -H "Authorization: Bearer $TOKEN" http://host-b:3000/health
curl http://127.0.0.1:3010/placements
```

diff snapshot은 target host에 base full snapshot이 있어야 restore할 수 있다. 운영자가
base 존재를 확신하지 못하면 `include_dependencies=true`를 사용한다.

```bash
anvil_replicate_snapshot \
  snapshot_id=snap-1 \
  source_host=host-a \
  target_host=host-b \
  include_dependencies=true
```

성공 후 scheduler `/placements` 또는 state file의 `snapshot_locations`에서 target host가
기록됐는지 확인한다.

```bash
curl http://127.0.0.1:3010/placements
```

target import가 실패하면 daemon log를 확인하고 target snapshot directory 아래
`snapshots/.import-*` staging directory가 남아 있지 않은지 점검한다. 남아 있다면 즉시
수동 삭제하기 전에 같은 import를 재시도할지, daemon이 해당 staging path를 사용 중인지
확인한다. replication response, audit, 운영 기록에는 `agent_token`, authorization
header, daemon raw body, raw `metadata.json` body를 남기지 않는다.

## Diff snapshot 안전성 — coarse-hole 파일시스템 가드 (D3)

sparse diff snapshot(memory.bin diff + rootfs.diff)은 파일시스템이 **hole을 4KiB
단위로** 보고한다고 가정한다. ZFS는 hole을 recordsize(기본 128K) 단위로만 보고하므로,
diff가 실제로 기록한 dirty page 바깥의 미기록 record padding까지 "기록된 영역"으로
부풀려져 restore 시 base 메모리의 유효 데이터를 제로로 덮어쓴다 → guest triple fault.

daemon은 두 지점에서 이를 코드로 막는다 (운영자 조치 불필요):

- **창설측 강등**: snapshot 생성 시 `{workDir}/snapshots` 파일시스템의 hole
  granularity를 daemon 수명당 1회 probe한다. >4KiB면 diff 요청을 **full snapshot으로
  강등**하고 metadata·API 응답의 `snapshot_type`을 정직하게 `full`로 기록한다. 강등
  시 daemon log에 `coarse filesystem hole granularity detected ...` warning이 1회 남는다
  (관측 granularity 값 포함, 비밀 없음).
- **판독측 거부**: restore/merge가 diff를 overlay하기 직전 diff 디렉토리를 probe한다.
  coarse면 `refusing overlay to avoid guest memory corruption (see D3)` 에러로
  거부한다 — 복제/임포트로 유입된 coarse diff까지 방어한다.

**운영 완화(권장)**: 강등은 안전하지만 diff 효율(작은 delta)을 잃는다. diff snapshot
효율이 필요한 호스트에서는 snapshot 디렉토리를 **recordsize=4K dataset**에 올린다.

```bash
sudo zfs create -o recordsize=4k -o mountpoint=/home/$USER/anvil/snapshots rpool/anvil-snapshots
```

4K dataset 위에서는 probe가 fine(4096)으로 관측되어 강등/거부가 발생하지 않고 diff가
정상 생성·restore된다. ext4/xfs 등은 기본이 4KiB이므로 조치가 필요 없다. daemon log에
위 warning이 반복 없이 1회만 보이는지, `GET /snapshots`에서 의도한 host의
`snapshot_type`이 `diff`인지로 상태를 확인한다.
