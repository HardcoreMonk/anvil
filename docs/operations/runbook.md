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

flock 삭제는 daemon이 소유한 member VM teardown 경로를 실행한다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID
```

Town Wall message body는 `flocks/<flock_id>/TOWN_WALL.log`와 history 응답에
남는다. provider token, API key, `agent_token` 같은 secret을 Town Wall에 게시하지
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
