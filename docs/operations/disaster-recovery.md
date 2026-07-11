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

2. daemon API 응답을 확인한다. top-level `/health` endpoint로 process와 인증
   경로를 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/health
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

runtime scheduler service를 별도로 운영한다면 scheduler도 확인한다.

```bash
curl http://127.0.0.1:3010/health
curl http://127.0.0.1:3010/placements
```

`ANVIL_SCHEDULER_STATE`와 `ANVIL_SCHEDULER_QUOTA_STORE` 파일을 직접 편집하지
않는다. placement가 stale이면 먼저 daemon별 `GET /vms` 결과를 확인하고
`RuntimeRouter.ReconcilePlacements` 경로 또는 운영자가 승인한 scheduler 재기동
절차로 정리한다.

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

## flock 삭제 실패 또는 member VM 잔존

1. live flock과 member VM 목록을 확인한다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks/$FLOCK_ID
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/vms
```

2. 먼저 daemon의 flock delete 경로를 재시도한다. 이 경로는 flock registry에서 제거한
   뒤 member VM들을 병렬로 `destroyVM` 처리한다.

```bash
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID
```

3. flock은 삭제됐지만 member VM이 남아 있으면 각 VM에 대해 일반
   `DELETE /vms/{vm_id}` 절차를 따른다. stale TAP/IP, dm-snapshot, COW 파일은 먼저
   daemon cleanup path를 재시도하고, 수동 정리는 inspect 이후 최후 수단으로만 수행한다.

4. Town Wall log는 `flocks/<flock_id>/TOWN_WALL.log`에 남을 수 있고,
   `metadata.json`이 있으면 daemon restart 뒤 read-mostly flock registry가 복구될 수
   있다. 장애 분석에는 사용할 수 있지만 secret 포함 여부를 확인한 뒤 공유한다.

5. cross-host routed flock이면 home daemon의 hub delete가 `relay_token` admission을
   revoke하지만, 나머지 member host의 relay flock 등록은 별도 정리가 필요할 수 있다.
   각 member host에서 같은 `DELETE /flocks/$FLOCK_ID`를 실행해 relay 등록을 해제한다.

## home host 다운(routed flock)

routed flock의 home host(hub)가 다운되면 그 flock의 Town Wall 게시/조회와
cross-host `gtcall`이 전부 502를 반환한다.

1. 증상을 확인한다. member host에서 wall post/history와 cross-host call이
   일관되게 502를 반환하면 home host 다운을 의심한다.

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID/post \
  -d '{"agent_id":"researcher-1","body":"health check"}'
```

2. 짧은 네트워크 순단이면 별도 조치가 필요 없다. daemon-to-daemon relay hop은
   dial-실패에 한정해 동기 bounded retry(총 3시도, backoff 1s/2s)로 순단을
   자동 흡수한다.

3. home daemon 프로세스만 죽었다면 재기동한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN" ./anvil-daemon
```

4. adapter의 reconcile 루프가 기본 `60s`(`ANVIL_MCP_RECONCILE_INTERVAL`) 안에
   hub/relay 등록과 relay-token/call-token admission을 자동 재주입한다.
   즉시 반영이 필요하면 운영자가 승인한 수동 재등록 절차를 사용한다.

5. home host 자체가 장기간 복구되지 않으면(프로세스가 아니라 host 다운),
   adapter reconcile 루프가 연속 `homeFailureThreshold`회(상수, 기본 3)
   dial-계열 실패를 관측한 뒤 자동으로 재선출 failover를 발화한다 —
   `record.Agents` 순서상 첫 생존 member host를 새 home으로 결정적으로
   승격하고 전 member를 그 host로 재등록한다. 전환 창은 최대
   ~`homeFailureThreshold`(3) × `ANVIL_MCP_RECONCILE_INTERVAL`(기본 `60s`)
   + 전환 시간이며, 기본 설정 기준 최대 ~3분이다. 이 창 동안 wall/call은
   계속 502 + bounded retry로 관측된다. **failover는 wall 과거 기록 손실을
   명시적으로 수용하는 계약이다** — 새 home은 빈 log에서 seq를 재시작하고,
   구 home 디스크의 기록은 병합되지 않는다. 자동 fail-back은 없다(구 host가
   부활해도 새 home을 유지하고, 다음 reconcile이 구 host를 relay로 강등해
   heal한다). 관측 방법(adapter 로그 라인, placements.json `home_host`)과
   원래 host로 되돌리는 수동 fail-back 절차는 `runbook.md`의 "Home 재선출
   failover 관측과 수동 fail-back" 절을 참고한다. 실 2-daemon 환경에서의
   failover 시나리오 수동 검증은 아직 수행되지 않았다 — 상세:
   [2026-07-08-cross-host-manual-verification.md](2026-07-08-cross-host-manual-verification.md)
   ⑥b. 설계 원문:
   [`docs/superpowers/specs/2026-07-08-home-failover-design.md`](../superpowers/specs/2026-07-08-home-failover-design.md).

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

## coarse-hole 파일시스템의 diff snapshot 오염 (D3)

증상: diff snapshot restore 직후 guest가 즉사한다 (fc log의
`Unexpected exit reason on vcpu run: Shutdown` = triple fault). 원인은 ZFS 등
recordsize>4K 파일시스템에서 sparse diff의 hole이 record 단위로만 보고되어 미기록
padding이 base 메모리를 덮어쓰는 것이다 (RCA:
`2026-07-10-cross-host-verification-run-handoff.md` §D3).

1. daemon은 이제 이를 코드로 막는다. 로그에서 아래를 확인한다.
   - 창설측: `coarse filesystem hole granularity detected ...` warning이 있으면 해당
     daemon은 diff 대신 **full snapshot**을 만든 것이다 (`GET /snapshots`의
     `snapshot_type`이 `full`). 데이터 손실 없음.
   - 판독측: restore가 `refusing overlay to avoid guest memory corruption (see D3)`로
     실패하면, 그 diff는 coarse 파일시스템에서 생성됐거나 복제된 것이다. 해당 diff는
     restore하지 않는다.

2. 거부된 diff의 base full snapshot이 있으면 그 full로 restore한다 (위 "diff base
   snapshot 누락" 3항과 동일 절차).

3. 재발 방지: snapshot 디렉토리를 recordsize=4K dataset에 올린다 (runbook "Diff
   snapshot 안전성" 절 참조). 4K dataset 위에서는 diff가 정상 생성·restore된다.

4. 손상된 것으로 의심되는 이미 생성된 diff snapshot을 삭제해야 하면 dry-run GC 또는
   개별 delete 응답으로 보호 상태를 먼저 확인한다. snapshot directory를 직접 삭제하지
   않는다.
