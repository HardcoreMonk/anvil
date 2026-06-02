# anvil 후속 개발 목록

작성일: 2026-05-29

이 문서는 `anvil-v0.3.0` 공개 이후 `main`에 반영된 scheduler control loop 작업을
기준으로 다음 개발 후보를 정리한다. upstream ephemera `v0.4.0` PR-A storage/recovery
변경은 당분간 구현 범위에서 제외하고, 별도 adoption review 항목으로만 유지한다.

## 현재 기준

- `main`은 upstream ephemera `v0.3.6` baseline과 anvil scheduler control loop
  개선을 포함한다.
- scheduler는 persistent `PlacementStore`, host bootstrap config,
  poll/reconcile control loop, `/control-loop/status`, persistence degraded gate,
  smoke harness를 제공한다.
- 현재 daemon `/health`는 scheduler capacity 필드인 `available_vms`,
  `available_snapshot_bytes`, `egress_policies`를 항상 제공하지 않는다. scheduler는
  hosts file의 capacity를 source of truth로 유지하고, `/health`가 해당 필드를
  생략하면 기존 값을 보존한다.
- 2026-06-02 scheduler 운영 강화 범위에서 1-2번은 구현 검증 대상으로 승격됐다.
  3번은 초기 scheduler `/metrics` endpoint로 일부 대응됐지만, poll/reconcile 지연
  시간과 실패 횟수 metric은 계속 후속 후보로 남는다.
  full-process integration test, scheduler `/metrics`, smoke의 `/metrics` 확인,
  실제 systemd `--start --verify` 결과는 승인이 필요한 검증 이후
  `docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md`에 기록할
  예정이다.

## 1. Scheduler full-process integration test

### 목표

`cmd/anvil-scheduler`를 실제 process로 띄운 상태에서 hosts file 적용, fake daemon
`/health`, control loop poll, `/schedule/spawn`까지 이어지는 end-to-end 경로를
자동 검증한다.

### 필요한 이유

현재 unit test는 `PlacementStore`, `LoadSchedulerHostsFile`,
`SchedulerControlLoop`를 직접 검증한다. 하지만 실제 process startup에서 환경 변수,
state file, hosts file, HTTP server가 함께 맞물리는 경로는 별도 test로 고정되어
있지 않다. 최근 수정도 stale zero scheduler state와 current daemon `/health`
compatibility 문제였으므로 process-level 회귀 테스트가 필요하다.

### 완료 기준

- fake daemon이 current daemon shape인 `{"status":"ok","vm_count":...}`를 반환한다.
- scheduler process는 `ANVIL_SCHEDULER_HOSTS_FILE`을 읽고 configured capacity를
  유지한다.
- stale state에 `available_vms: 0`이 남아 있어도 hosts file capacity가 우선한다.
- `/control-loop/status`가 `running: true`를 반환한다.
- `/schedule/spawn`이 configured host를 정상 선택한다.

## 2. Scheduler 실제 운영 배포 검증

### 목표

실제 systemd host에서 scheduler installer, service unit, env file, state directory
permission, smoke verification을 검증한다.

### 필요한 이유

로컬 smoke와 installer dry-run은 통과했지만, 실제 systemd 환경에서는 user/group,
`/var/lib/anvil`, `/etc/anvil`, unit hardening, service restart가 함께 작동해야 한다.

### 완료 기준

- 다음 명령이 실제 운영 host에서 통과한다.

```bash
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

- `systemctl status anvil-scheduler`가 active 상태다.
- `/health`, `/placements`, `/control-loop/status`가 정상 응답한다.
- smoke harness가 등록한 임시 host를 정리한다.
- scheduler state file과 quota store file permission이 운영 정책과 맞는다.

## 3. Scheduler observability 확장

### 목표

control loop 상태를 `/control-loop/status`뿐 아니라 metrics/alert-friendly surface로
노출한다.

### 필요한 이유

운영자는 degraded/unhealthy host, poll failure, reconcile failure, persistence
degraded 상태를 사람이 curl로 확인하기 전에 알림으로 받아야 한다.

### 후보 metrics

- control loop running flag
- host status count: healthy/degraded/unhealthy/unknown
- poll latency와 reconcile latency
- poll/reconcile failure count
- `persistence_degraded` gauge
- suspect placement count
- last successful poll/reconcile timestamp

### 완료 기준

- Prometheus scrape 또는 scheduler metrics endpoint에서 위 상태를 확인할 수 있다.
- 기존 daemon `/metrics` 정책과 충돌하지 않는다.
- token, raw daemon response, authorization header는 metrics에 노출하지 않는다.

## 4. Cross-host snapshot replication

### 목표

snapshot locality 정보를 실제 cross-host snapshot replication과 연결한다.

### 필요한 이유

scheduler는 snapshot locality preference를 고려할 준비가 되어 있지만, snapshot이
특정 host에만 존재하면 restore scheduling 선택지가 제한된다. host 장애나 부하 상황에서
다른 host로 restore하려면 snapshot 복제가 필요하다.

### 설계 쟁점

- full snapshot과 diff snapshot dependency를 host 간 어떻게 보존할지
- 복제 중 partial artifact를 어떻게 숨길지
- replication audit record와 retry 정책
- source host가 degraded/unhealthy일 때 replication을 중단할지
- snapshot GC와 replication 상태의 우선순위

### 완료 기준

- snapshot metadata가 host별 location을 표현한다.
- replication 성공/실패가 audit/state에 남는다.
- restore scheduler가 복제된 host location을 선택에 반영한다.
- diff snapshot이 참조하는 full snapshot은 복제/삭제 순서에서 보호된다.

## 5. Scheduler-aware cross-host flock placement

### 목표

Goosetown flock member VM을 scheduler decision에 따라 여러 runtime host에 배치한다.

### 필요한 이유

현재 flock runtime은 VM/snapshot tool 계약과 별도 additive surface로 존재하지만,
cross-host placement 정책은 없다. 장기 실행 flock은 특정 host 장애나 quota 한계에
취약하므로 role별 또는 tenant별 placement 정책이 필요하다.

### 설계 쟁점

- role별 preferred/excluded host 지원 여부
- flock 단위 anti-affinity 또는 co-location 정책
- Town Wall 통신과 cross-host network reachability
- 일부 member spawn 실패 시 rollback 또는 partial flock 허용 여부
- placement state와 runtime audit record의 flock id 연결

### 완료 기준

- `anvil_spawn_flock` 또는 daemon flock 생성 경로가 scheduler decision을 사용한다.
- member VM별 host placement가 state에 기록된다.
- scheduler failure 시 token이나 partial VM 정보가 노출되지 않는다.
- 기존 single-host flock API wire compatibility를 깨지 않는다.

## 6. Egress hardening

### 목표

현재 `deny_all`, `profile`, `allow_all` 및 profile allowlist 기반 host rule을 더 강한
운영 정책으로 확장한다.

### 필요한 이유

IP/DNS allowlist만으로는 L7 목적지, SNI, tenant별 outbound audit를 세밀하게 통제하기
어렵다. IronClaw가 실행하는 agent workload는 외부 네트워크 접근을 더 명확히 제한해야
한다.

### 후보

- L7 proxy 기반 egress enforcement
- SNI allowlist
- DNS policy 검증과 DNS egress audit
- per-tenant egress audit record
- profile별 deny reason reporting

### 완료 기준

- profile policy가 실제 outbound 목적지를 더 정밀하게 제한한다.
- `deny_all`과 기존 profile no-op compatibility를 깨지 않는다.
- egress decision과 위반 기록이 audit 가능한 형태로 남는다.

## 7. Snapshot storage quota dashboard

### 목표

snapshot usage, quota, GC candidate를 운영자가 한눈에 볼 수 있는 report/API를 제공한다.

### 필요한 이유

snapshot GC와 quota helper는 있지만 운영자는 현재 사용량, 보호 중인 full snapshot,
diff dependency, 삭제 후보를 직접 추적해야 한다. 운영 중 디스크 압박을 줄이려면
read-only report가 필요하다.

### 후보 출력

- total snapshot bytes
- full/diff snapshot count
- protected full snapshot 목록
- GC candidate 목록과 삭제 예상 bytes
- tenant/profile별 usage grouping
- stale artifact 후보

### 완료 기준

- JSON 또는 dashboard-friendly 형태로 usage report를 반환한다.
- dry-run GC 결과와 실제 GC audit record가 같은 기준을 사용한다.
- diff snapshot이 참조 중인 full snapshot은 삭제 후보에서 제외된다.

## 8. Scheduler host registration hardening

### 목표

hosts file과 `/hosts` API 중심의 host inventory 운영을 더 안전한 registration model로
확장한다.

### 필요한 이유

현재 scheduler는 loopback/private network 또는 reverse proxy 뒤 운영을 전제로 한다.
장기적으로 여러 daemon host를 운영하려면 임의 host 등록을 제한하고, 승인된 daemon만
scheduler inventory에 들어오게 해야 한다.

### 후보

- daemon self-registration
- host별 scheduler token
- config-managed host와 runtime-added host의 권한 차등
- host identity fingerprint
- registration audit record

### 완료 기준

- 승인되지 않은 host는 scheduler inventory에 등록되지 않는다.
- config-managed host는 hosts file이 source of truth임을 유지한다.
- host registration 실패와 삭제가 audit 가능하다.

## 9. upstream ephemera `v0.4.0` PR-A adoption review

### 상태

당분간 구현 범위에서 제외한다.

### 목표

나중에 별도 `sync/ephemera-*` branch에서 upstream storage/recovery 변경을 검토하고
anvil 정책 기준으로 분류한다.

### 필요한 이유

`v0.4.0` PR-A는 storage/recovery 의미를 바꾸는 변경으로 보이며, anvil의 snapshot,
restore, cleanup, token exposure 불변 조건과 충돌할 수 있다. 따라서 일반 feature
작업과 섞지 않는다.

### 완료 기준

- `git ls-remote --tags upstream`으로 upstream tag를 확인한다.
- sync branch에서 merge commit으로 upstream 이력을 보존한다.
- 변경을 `adopted`, `adapted`, `deferred`, `excluded`로 분류한다.
- `docs/ADR_INDEX.md`, 관련 ADR, `CONTEXT.md`, `RELEASE_NOTES.md`를 갱신한다.

## 권장 개발 순서

1. Scheduler full-process integration test
2. Scheduler 실제 운영 배포 검증
3. Scheduler observability 확장
4. Cross-host snapshot replication
5. Scheduler-aware cross-host flock placement
6. Egress hardening
7. Snapshot storage quota dashboard
8. Scheduler host registration hardening
9. upstream ephemera `v0.4.0` PR-A adoption review

첫 세 항목은 최근 scheduler control loop 작업을 운영 품질로 굳히는 데 직접 연결된다.
그 다음부터는 snapshot, flock, network policy처럼 설계 폭이 큰 기능으로 확장한다.
