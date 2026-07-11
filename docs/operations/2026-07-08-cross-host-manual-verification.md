# Cross-host 실 2-daemon 수동 검증 절차 (wall + gtcall)

- 작성일: 2026-07-08
- 상태: **2026-07-10 수행 완료 — 전 항목 통과** (최초 실행에서 ⑥ 재시작 복구
  FAIL → D1/D1b fix(PR #30·#31) 당일 머지 후 ⑥ 재검증 PASS — 양 daemon 동시
  재시작 시나리오 포함). "실 2-daemon 통합 검증 완료". 수행 기록·증거·결함
  상세: [2026-07-10-cross-host-verification-run-handoff.md](2026-07-10-cross-host-verification-run-handoff.md).
  이 상태는 wall/gtcall/재시작 복구(①~⑤, ⑥a, ⑦, ⑧) 항목에 한정된다. ⑥b
  (home 재선출 failover)는 2026-07-11 home failover 구현 편입과 함께 신규
  추가된 시나리오로, **아직 수행되지 않았다** — 트리거는 다음 서버 세션.
  상세: [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md).
- 배경: 단일 host CI에서는 두 real daemon이 guest bridge(`goose-br0`,
  `10.0.1.0/24` — `internal/network/manager.go`에 고정)를 공유할 수 없어,
  wall/gtcall e2e는 home을 stub으로 대체한다. member 측 실경로(real VM →
  relay/call hop)는 CI가 증명하지만, **home 측 실수신·2번째 hop·양방향
  왕복은 이 수동 절차가 최종 증거**다.

## 전제

- KVM 가능한 host 2대 (host-a=home 예정, host-b=member), 상호 도달 가능한
  신뢰(private) 네트워크. control-plane 포트(기본 3000) 상호 개방.
- 각 host에 anvil 체크아웃(같은 커밋) + `go build -o anvil-daemon
  ./cmd/goose-daemon/` + golden image 전제(`configs/goose.yaml`,
  `configs/goose-secrets.yaml` — gitignored, host별 준비).
- 워크스테이션(control plane 실행 위치)에서 두 host의 :3000에 도달 가능.

### 호스트 준비 체크리스트 (Ubuntu 26.04 기준 — 타 배포판도 동일 항목)

anvil의 host 의존은 배포판 중립이다(golden image는 host 배포판과 무관하게
debootstrap으로 Debian Trixie 생성 — `scripts/build_image.sh`). 서버 세팅 시:

1. **Go ≥ 1.25.0** (`go.mod`) — 배포판 repo 버전이 미달이면 공식 tarball.
   대안: 워크스테이션에서 빌드한 `anvil-daemon`을 두 서버에 복사 — "같은
   커밋" 요구를 바이너리 배포로 대체(권장: 커밋 hash를 배포 기록에 남길 것).
2. **패키지**: `apt-get install -y iproute2 dmsetup iptables curl debootstrap
   util-linux e2fsprogs` (+ anti-spoof를 실제로 켜려면 `ebtables` — 없으면
   `EPHEMERA_NET_ANTISPOOF`는 best-effort로 조용히 degrade).
3. **iptables nft 백엔드**(26.04 기본): anvil은 iptables CLI만 호출 — 정상.
4. **네트워크/방화벽**: 두 서버+워크스테이션 간 :3000 상호 도달, 신뢰
   (private) 네트워크 전제. ufw 사용 시 private 인터페이스에서 3000 allow.
5. **아키텍처 x86_64** — 고정 kernel/firecracker 아티팩트가 x86_64.
6. **KVM**: `/dev/kvm` 존재(물리 서버 또는 nested virt 활성).
7. **본편 전 단일 host smoke**: 각 서버에서 `sudo bash e2e_test.sh`(또는
   wall e2e 1회)를 먼저 통과시켜 환경 문제를 검증 본편과 분리한다.
   host별 `configs/goose.yaml`·`configs/goose-secrets.yaml` 준비 필수 —
   누락 시 VM 생성이 500으로 조용히 실패한다(2026-07-08 세션에서 실제 발생).
8. **ZFS 루트 host 주의**: snapshot 디렉토리가 ZFS(기본 recordsize 128K) 위에
   있으면 diff snapshot 복원이 guest triple fault로 전멸한다 —
   `overlaySparseDiff`의 SEEK_DATA 4K-해상도 가정이 깨지기 때문 (2026-07-10
   RCA, D3). 해법: `zfs create -o recordsize=4k -o
   mountpoint=<repo>/snapshots rpool/anvil-snapshots` 후 smoke 실행.
   상세: [2026-07-10-cross-host-verification-run-handoff.md](2026-07-10-cross-host-verification-run-handoff.md).

## 절차

1. **daemon 기동 (양쪽, auth-on)**
   ```bash
   EPHEMERA_API_TOKENS="operator:<op-token>" \
   EPHEMERA_API_ADDR=0.0.0.0 ./anvil-daemon
   ```
   (외부 노출 금지 — private network 전제. reverse proxy 뒤면 그 정책 따름.)

2. **adapter 구성 (워크스테이션)** — `configs/anvil-mcp.yaml` 또는 env:
   `cross_host_flock_create_mode: members_only`, persistent
   `scheduler_state_path`, hosts 파일에 host-a/host-b의 name+endpoint
   (`ANVIL_API_TOKEN=<op-token>`). `ANVIL_MCP_RECONCILE_INTERVAL` 기본(60s) 유지.

3. **routed flock 생성** — `anvil_create_routed_flock_members`로 2-role flock
   (roles[0]→host-a가 home이 되도록 hosts 순서 확인). 기대: 출력
   `town_wall_enabled=true`, 두 host에 VM 각 1.

4. **wall 양방향** — 각 VM에서 `gtwall <msg>` 실행(workload runner 또는 콘솔).
   기대: home의 `TOWN_WALL.log`에 두 host 발신 메시지 모두 기록,
   `/flocks/{id}/wall/history`가 양쪽 daemon에서 동일 내용 반환(relay proxy).

5. **gtcall 4방향** — ① member(host-b) guest → home(host-a) agent,
   ② home guest → member agent (roster Addr 2nd hop),
   ③ member guest → 같은 host member agent (home 경유 후 relay local-agents
   해석), ④ 존재하지 않는 agent → `404` 진단(주소·토큰 무노출 육안 확인).
   기대: ①~③ 응답 텍스트 왕복, ③이 특히 C1 수정 경로(unmarked member→home →
   marked home→target)의 실증.

6. **장애 시나리오 스팟**
   - **⑥a. 재시작 복구** (2026-07-10 수행, PASS — D1/D1b fix 이후 재검증
     포함) — home daemon 재시작 → 60s 내(reconcile) wall/call 재동작 확인.
     member daemon 재시작 → 동일. 상세:
     [2026-07-10-cross-host-verification-run-handoff.md](2026-07-10-cross-host-verification-run-handoff.md).
   - **⑥b. home 재선출 failover** (2026-07-11 신규 추가 — **미수행**, 트리거:
     다음 서버 세션) — home daemon을 **정지**한다(재시작이 아니라 프로세스를
     죽인 채로 유지 — reconcile이 dial-계열 실패를 연속 관측해야 한다) →
     전환 창 동안 대기한다(최대 ~`homeFailureThreshold`(3) ×
     `ANVIL_MCP_RECONCILE_INTERVAL`, 기본 `60s` + 전환 시간 — 기본 설정 기준
     최대 ~3분) → adapter의 `scheduler_state_path`(placements.json)에서 해당
     flock의 `home_host`가 새 host로 바뀌었는지 확인한다 → wall 양방향과
     gtcall 4방향(④~⑤ 절차를 새 home 기준으로 재사용)이 새 home을 경유해
     재확인되는지 확인한다 → **구 home을 재기동**한다 → 구 home이 relay로
     강등됐는지 확인한다(`GET /flocks/{id}`의 `kind`가 `relay`로 바뀜; 구
     home에서 `gtwall`을 실행하면 새 home으로 forward되고 `409` 없이
     성공) → `GET /flocks/{id}/wall/history`에서 전환 이전(구 home 시절)
     메시지가 더 이상 나타나지 않음을 확인한다(wall 손실 계약의 직접 관측
     — 이 부재는 실패가 아니라 설계된 계약이다).

7. **redaction 스팟** — 두 daemon stderr 로그에 `relay_token`/`call_token`
   값·상대 daemon 주소가 없는지 grep. `GET /flocks/{id}` 응답에 토큰/Addr
   부재 확인.

8. **정리** — `anvil_delete_flock` → 양쪽 daemon에서 flock/VM 소멸 + 토큰
   revoke 확인(재호출 401).

## 판정 기준

| 시나리오 | 통과 조건 | 상태 |
|---|---|---|
| ①~③ gtcall 왕복 | 응답 텍스트 왕복, ③이 C1 수정 경로 실증 | PASS (2026-07-10) |
| wall 양방향 | home `TOWN_WALL.log`에 양 host 발신 기록, `wall/history` 양쪽 daemon 동일 | PASS (2026-07-10) |
| ⑥a 재시작 복구 | home/member daemon 재시작 후 60s 내 wall/call 재동작 | PASS (2026-07-10, D1/D1b fix 이후) |
| redaction 스팟 | 양 daemon 로그·`GET /flocks/{id}` 응답에 토큰/상대 daemon 주소 부재 | PASS (2026-07-10) |
| ⑥b home 재선출 failover | `home_host` 전환 확인 + 새 home 경유 wall/gtcall 재확인 + 구 home relay 강등(신 home으로 forward, `409` 없음) + wall history 전환-전 메시지 부재 확인 | **미수행** (트리거: 다음 서버 세션) |

①~③ 왕복 + wall 양방향 + ⑥a 재시작 복구 + redaction 스팟 전부 통과 시 "실
2-daemon 통합 검증 완료"로 이 문서 상태를 갱신하고 gtcall handoff의 MANUAL
check 항목을 CLOSED 표기한다(2026-07-10 기준 이 조건은 이미 충족됐다). ⑥b는
별도 시나리오로, 통과 시 이 표의 상태를 CLOSED로 갱신하고
[2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md)의
Follow-Up 항목을 완료로 갱신한다. 실패 시 증상·로그를 이 문서에 첨부하고
slice 결함으로 회부한다.
