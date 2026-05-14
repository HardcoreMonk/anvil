# anvil 재해 복구 Playbook

이 문서는 단일 host 운영 중 daemon crash, VM 삭제 실패, restore 실패, GC 실패,
snapshot 의존성 문제를 다룰 때의 안전한 절차다. 원칙은 먼저 daemon API와 상태 조회로
확인하고, 수동 삭제는 최후 수단으로 inspect 이후에만 수행하는 것이다.

## 공통 원칙

- 실제 secret, `agent_token`, provider API key를 terminal 공유, 문서, ticket에 남기지
  않는다.
- 공개 운영 환경은 TLS reverse proxy와 `EPHEMERA_API_TOKENS`를 유지한다.
- `rm -rf`로 runtime directory를 직접 지우지 않는다.
- VM 삭제, snapshot 삭제, snapshot GC는 daemon API를 우선 사용한다.
- 명령 예시의 `$TOKEN`, `$VM_ID`, `$SNAPSHOT_ID`는 placeholder다.

## daemon crash 또는 restart

1. daemon process를 다시 시작한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN" ./anvil-daemon
```

2. daemon API 응답을 확인한다. 현재 top-level `/health` endpoint는 없으므로 목록
   endpoint로 process와 인증 경로를 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
```

3. daemon이 알고 있는 VM과 snapshot 목록을 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/snapshots
```

4. host network 상태를 조회한다.

```bash
ip -brief link
ip -brief addr
```

5. VM별 health를 proxy로 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID/health
```

daemon 재시작 후 API 목록과 host 상태가 불일치하면 수동 파일 삭제를 하지 말고 먼저
해당 VM에 `DELETE /vms/{vm_id}`를 호출해 daemon cleanup path를 실행한다.

## VM 삭제 실패 후 stale TAP/IP

1. VM 목록에서 대상 VM이 아직 남아 있는지 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
```

2. daemon cleanup을 재시도한다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID
```

3. network device와 address 상태를 inspect한다.

```bash
ip -brief link
ip -brief addr
```

4. daemon log에서 TAP, IP, dm-snapshot, loop device, bind mount, COW cleanup 오류를
확인한다.

수동 정리가 필요해 보이면 먼저 device 이름, mount, loop, dm 상태가 해당 VM의
resource인지 확인한다. 운영 표준 절차는 daemon API 재시도이며, 임의 파일 삭제나
device 제거를 자동화하지 않는다.

## restore 실패

1. source VM이 실행 중인지 확인한다. 실행 중인 원본 VM의 snapshot은 restore하지
   않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
```

2. snapshot 목록에서 대상 snapshot과 base 관계를 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/snapshots
```

3. restore를 다시 시도하기 전에 daemon log에서 실패 단계가 memory merge, COW rootfs,
   TAP/IP allocation, vsock IP reset, guest `/health` 중 어디인지 확인한다.

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/snapshots/$SNAPSHOT_ID/restore
```

4. restore가 VM 생성 이후 health 대기에서 실패했다면 VM 목록에 partial VM이 남았는지
   확인하고, 남아 있으면 daemon API로 삭제한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID
```

restore 응답은 `agent_token`을 노출하지 않는다. 과거 로그나 오래된 fixture를
공유할 때는 legacy restore body에 `agent_token`이 남아 있지 않은지 먼저 확인한다.

## GC apply 실패

1. 같은 policy로 dry-run을 다시 실행해 현재 후보와 보호 대상을 확인한다.

```bash
curl -X POST http://127.0.0.1:3000/snapshots/gc \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240}'
```

2. 이전 apply의 audit count를 확인한다. audit은 count-only record이며 metadata나
   `agent_token`을 포함하지 않는다.

```bash
tail -n 20 snapshots/gc-audit.jsonl
```

3. apply를 재시도한다.

```bash
curl -X POST http://127.0.0.1:3000/snapshots/gc \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240,"apply":true}'
```

4. 응답의 `errors`가 특정 snapshot을 가리키면 해당 snapshot을 개별 삭제하기 전에
   `GET /snapshots`로 diff dependency를 다시 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/snapshots
```

## diff base snapshot 누락

1. `GET /snapshots`에서 diff snapshot의 `base_snapshot_id`가 목록에 있는지 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/snapshots
```

2. base가 없으면 해당 diff snapshot은 restore 가능한 상태가 아니다. 운영자가 임의로
   새 base를 연결하지 않는다.

3. 같은 source VM 또는 같은 작업에서 생성된 최신 full snapshot이 있으면 그 full
   snapshot으로 restore를 시도한다.

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/snapshots/$FULL_SNAPSHOT_ID/restore
```

4. base 누락 diff snapshot을 삭제해야 하면 먼저 dry-run GC 또는 개별 delete 응답으로
   보호 상태를 확인한다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/snapshots/$SNAPSHOT_ID
```

삭제가 거부되면 응답의 dependency 이유를 따른다. snapshot directory를 직접 삭제하지
않는다.
