# anvil 관측성 운영 메모

현재 anvil/ephemera 운영 관측성은 daemon log, top-level daemon `/health`,
Prometheus text 형식의 `/metrics`, VM/guest health endpoint, API 상태 응답,
snapshot GC audit 파일을 중심으로 한다.

## 현재 log

`goose-daemon`은 stdout/stderr에 운영 log를 출력한다. service manager를 사용한다면
해당 manager의 log 수집 설정으로 stdout/stderr를 보관한다.

시작 시 확인할 log:

- control plane listen address와 auth mode
- 등록된 endpoint 목록
- `EPHEMERA_PUBLIC_URL`이 설정된 경우 agent URL base
- bootstrap, Firecracker, network, storage warning

runtime 중 확인할 log:

- VM 생성, restore, delete 실패
- guest `/health` readiness timeout
- TAP/IP allocation 또는 cleanup warning
- dm-snapshot, loop device, bind mount, COW cleanup warning
- snapshot GC apply error

## Health endpoint

daemon 자체 상태는 top-level `/health` endpoint로 확인한다. 응답에는 `status`,
실행 중 VM 수, snapshot 수, control-plane auth 활성 여부가 들어가며 token 값은
포함하지 않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/health
```

VM health는 daemon proxy를 통해 확인한다. 공개 배포에서는 TLS reverse proxy의 외부
URL을 사용하고, 내부 host 점검에서는 localhost daemon URL을 사용할 수 있다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID/health
```

guest 내부 endpoint는 `goose-agent`의 `/health`다. 운영 client는 VM private IP에
직접 접근하지 말고 daemon의 `/vms/{id}/health` proxy를 우선 사용한다.

## Snapshot GC audit

`POST /snapshots/gc`를 `apply:true`로 호출하면 daemon은
`snapshots/gc-audit.jsonl`에 JSONL record를 append한다. dry-run은 audit record를 쓰지
않는다.

record는 count-only 성격이다.

- `timestamp`
- `applied`
- `policy.older_than_seconds`
- `policy.keep_last_per_vm`
- `policy.max_total_bytes`
- `candidates_count`
- `deleted_count`
- `errors_count`

audit 파일에는 snapshot metadata 전체, path 세부 정보, `agent_token`이 들어가지
않는다. 파일 권한은 append 시 `0600`으로 조정된다.

최근 audit 확인:

```bash
tail -n 20 snapshots/gc-audit.jsonl
```

## Runtime audit API

runtime audit JSONL은 운영 API로 조회/보관 정리할 수 있다. 응답은 record 배열만
반환하며 daemon raw body, snapshot metadata, `agent_token`을 포함하지 않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:3000/audit/runtime?tenant_id=tenant.alpha&limit=50"

curl -X POST http://127.0.0.1:3000/audit/runtime/prune \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"keep_last":1000,"max_age_seconds":2592000}'
```

## Metrics endpoint

`GET /metrics`는 Prometheus text 형식의 host-local counter를 반환한다. 현재
제공하는 주요 counter는 다음이다.

- `anvil_vm_create_total`
- `anvil_vm_restore_total`
- `anvil_vm_delete_total`
- `anvil_snapshot_create_total`
- `anvil_snapshot_delete_total`
- `anvil_snapshot_gc_total`
- `anvil_cleanup_failure_total`
- `anvil_auth_failure_total`

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/metrics
```

## 현재 없는 것

다음 기능은 아직 구현되어 있지 않다.

- 구조화된 per-VM metrics endpoint
- OpenTelemetry trace export
- daemon 내부 queue depth 또는 lifecycle duration histogram
- snapshot storage quota dashboard

현재 운영 판단은 daemon log, `/health`, `/metrics`, `GET /vms`, `GET /snapshots`,
VM health endpoint, `snapshots/gc-audit.jsonl`, runtime audit API를 조합해서 수행한다.

## 향후 metrics 후보

구현 후보 metrics:

- VM create/restore/delete duration
- guest `/health` readiness latency
- snapshot create/restore/delete/GC duration
- snapshot total bytes와 GC candidate/deleted count
- TAP/IP allocation failure count
- dm-snapshot, loop device, bind mount cleanup failure 상세 label
- proxy task request count, latency, error count

이 항목들은 현재 counter보다 더 세밀한 운영 권장 지표다.
