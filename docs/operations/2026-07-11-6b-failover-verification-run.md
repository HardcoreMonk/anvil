# §6b Home 재선출 Failover — 실 2-daemon 수동 검증 수행 기록 (2026-07-11)

- 상태: **⑥b 전 단계 PASS — home 재선출 failover 실 2-daemon 검증 완료.**
  감지·재선출·kind 전환(hub→relay)·wall 손실 계약·양방향 wall/gtcall 재확인·
  redaction·정리+revoke 전부 실서버 2대에서 관측. 신규 결함 회부 없음(정리
  단계에서 **기존 D2**(delete cleanup_failed 오보고)가 재현됐으나 이는
  2026-07-10에 이미 회부된 미해결 결함이며 ⑥b failover 자체의 결함이 아니다).
- 절차 원문: [2026-07-08-cross-host-manual-verification.md](2026-07-08-cross-host-manual-verification.md) ⑥b (canonical)
- 직전 수행 기록(①~⑤, ⑥a, ⑦, ⑧): [2026-07-10-cross-host-verification-run-handoff.md](2026-07-10-cross-host-verification-run-handoff.md)
- slice handoff: [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md)
- 대상 커밋: **`0feb9fb`** (main, PR #33 `feature/home-failover` 병합 —
  `0feb9fb5398387adb28ceac79b7b4e164f9a7a8c`). 양 host 동일 소스에서 host별
  빌드 — `anvil-daemon` sha256 **`f8b7267fc848f87ad77de0132699fb677e2d9431765ade72622e3f14f3bc39c1`**
  양 host 바이트 동일.

## 환경 (계획 대비 편차 포함)

| | host-a (초기 home) | host-b (초기 member) | 워크스테이션 (control plane) |
|---|---|---|---|
| 주소 | 192.168.1.19 (PureCVisor-PROD-1) | 192.168.1.20 (PureCVisor-Prod-2) | 192.168.1.18 |
| OS | Ubuntu 24.04.4 | 동일 | — |
| 루트 fs | root-on-ZFS (rpool 128K) + `rpool/anvil-snapshots`(recordsize **4K**)→`~/anvil/snapshots` | 동일 | ext4 |
| Go | 1.26.2 (`/opt/anvil-go`) | 동일 | 1.26.2 |
| 역할 | daemon (auth-on, bind `0.0.0.0:3000`) | 동일 | adapter(`cmd/anvil-mcp`) + 검증 드라이버 |

- 배포: `rsync -a --delete`(예외: `snapshots/`, `configs/goose.yaml`,
  `configs/goose-secrets.yaml`, 그리고 root-owned 런타임 dir `artifacts/`(golden
  image·커널·fc 바이너리 보존)·`flocks/`·`audit/`·`vms/`·`tmp/`·로그) →
  host별 빌드 → `DEPLOY_RECORD.txt` 갱신. 직전(2026-07-10)의 stale `flocks/`
  (TOWN_WALL.log 잔재)는 배포 전 삭제. ZFS snapshot mountpoint 불파괴 규칙 준수.
- **adapter 구성 편차**: `ANVIL_MCP_RECONCILE_INTERVAL=10s`로 단축(기본 60s
  대신 — 절차의 감지 대기를 줄이기 위한 허용 편차). `cross_host_flock_create_mode=members_only`,
  persistent `scheduler_state_path`(placements.json), `hosts.json`=
  `[{host-a→.19}, {host-b→.20}]`(이름 정렬로 roles[0]=home→host-a 결정), 단일
  operator bearer가 양 daemon 인증. 전환 창 실측치는 interval 기준으로 환산해
  기록(아래).
- flock: `routed-flock-1783743033687100294`, roles `[home, member]`,
  `town_wall_enabled=true`, 초기 home=host-a. agent: `home-1`@host-a,
  `member-1`@host-b(각 host 실 KVM VM 1).

## 결과 매트릭스 (⑥b 세부 단계)

| 단계 | 결과 | 증거 요지 |
|---|---|---|
| 배포 (양 host 동일 소스, host별 빌드) | **PASS** | `anvil-daemon` sha256 양 host 동일, `DEPLOY_RECORD` commit=`0feb9fb` |
| ① daemon 기동 (auth-on 양쪽) | **PASS** | 무토큰 `/vms` 401, op-token 200, watchdog started (양 host, 워크스테이션→LAN :3000 도달) |
| ③ routed flock 생성 | **PASS** | `home_host=host-a`, `town_wall_enabled=true`, `home-1`@host-a·`member-1`@host-b, 각 host VM 1 |
| ④ wall 양방향 (sanity) | **PASS** | home `TOWN_WALL.log`에 seq1(`home-1`)+seq2(`member-1`), `wall/history` 양 daemon 바이트 동일, relay(host-b) 로컬 wall 없음 |
| ⑤ gtcall (sanity) | **PASS** | `member-1`→`home-1` relay hop 왕복 `PONG6B` (exit 0) |
| ⑥b-1 감지·재선출 | **PASS** | host-a daemon **kill**(정지 유지) → `placements.json home_host` **host-a→host-b 전환**. adapter stderr: `home failover "host-a" -> "host-b"` (+ 선행 "home host host-a unreachable" ×2) |
| ⑥b-2 새 home 서빙 + wall 손실 | **PASS** | 전환 직후 host-b `wall/history` **빈 배열**(구 seq1/2 소멸) → `member-1` gtwall이 **seq 1**로 재시작 (새 canonical wall) |
| ⑥b-3 구 home relay 강등 | **PASS** | host-a **재기동** → on-disk `metadata.json` `kind:"relay"` (`home_addr:"http://192.168.1.20:3000"`=새 home), `GET /flocks/{id}` 200. access audit가 host-a에서 `/distributed`(hub)→(kill 공백)→`/relay`(강등) 전환을 보여줌 |
| ⑥b-4 구 home guest forward, 409 없음 | **PASS** | `home-1`(host-a=relay) gtwall → 새 home host-b로 forward, **seq 2**로 착지, exit 0, **409 없음** |
| ⑥b-5 gtcall 새 home 경유 재확인 | **PASS** | `home-1`→`member-1` `PONGA`(relay hop), `member-1`→`home-1` `PONGB`(새 home 2nd hop) — 양방향 왕복 |
| ⑥b-6 wall parity + 전환-전 메시지 부재 | **PASS** | host-a(relay)≡host-b(hub) `wall/history` 바이트 동일(seq1/2=전환 후 메시지). 전환 전 `WALL_FROM_HOME_A_p4`/`WALL_FROM_MEMBER_B_p4` **부재**(wall 손실 계약 — 부재가 정답) |
| ⑦ redaction 스팟 | **PASS** | 양 daemon stderr(host-a 정지·재기동 로그 + host-b) + adapter stderr에 relay/call token 값 0·64-hex 0·상대 daemon 주소(192.168.1.19/20) 0건. `GET /flocks/{id}` 응답은 `kind`/`home_addr`/token 전부 미노출(`json:"-"`) |
| ⑧ 정리 + revoke | **PASS** (D2 재현 동반) | `anvil_delete_flock` 후 양 host `GET /flocks/{id}` **404** + `/vms` **0** + relay/call token `POST /post`·`/call` **401**(revoke). 단 delete 도구가 `is_error:true "cleanup_failed"` 오보고 — **기존 D2**(아래) |

## 전환 창 실측

- **감지→재선출(`home_host` 전환)**: kill 시점부터 `placements.json`이 host-b로
  바뀔 때까지 **~27s**(2s 폴링 granularity). adapter 로그 delta로도 kill
  ~13:15:18 KST → failover 라인 13:15:44 KST = **~26s**. reconcile_interval=10s
  기준 `homeFailureThreshold`(3)×10s=30s 상한 안 — 관측된 3회 연속 dial-실패
  누적 후 발화와 일치.
- **default 60s 환산**: 같은 3-pass 발화 정책이므로 기본 설정에서는
  ~3×60s + 전환 ≈ **~2.7분**(runbook 문서의 "최대 ~3분" 상한 안).
- **구 home relay 강등 소요**: host-a 재기동(API ~4s 복귀) 후 on-disk kind가
  `relay`로 확정될 때까지 **~20s**(reconcile 2 pass). 이후 gtwall forward·gtcall
  즉시 동작.

## 관측 방법 편차 (결함 아님 — runbook 문언 정합 필요)

절차 ⑥b는 "`GET /flocks/{id}`의 `kind`가 `relay`로 바뀜"으로 강등을 관측하라고
쓰여 있으나, daemon의 `orchestrator.Flock.Kind`는 `json:"-"`로 **HTTP 응답에서
의도적으로 제외**된다(redaction 계약 ⑦와 정합 — token/HomeAddr와 함께 미노출).
따라서 실제 관측은 다음 3중으로 대체·강화했다:

1. **on-disk `~/anvil/flocks/{id}/metadata.json`** `kind:"relay"` + `home_addr`=새 home (token은 여전히 부재).
2. **행위 관측**: 구 home guest gtwall이 새 home으로 forward되어 성공(409 없음) — relay만이 보이는 동작.
3. **access audit**: host-a가 `/distributed`(hub) 수신 → kill 공백 → `/relay`(강등) 수신으로 전환.

→ 이는 **runbook 문언의 부정확**이지 product 결함이 아니다. `kind`의 HTTP
비노출은 redaction 설계상 올바른 동작. (후속: 절차 ⑥b 문언을 위 관측 방법으로
보정 권장 — 이번 세션에서 절차 문서 상태만 PASS로 갱신, 관측 방법 note 추가.)

## 결함 회부

### 신규: 없음

⑥b failover 경로(감지/선출/kind 전환/wall 손실/양방향 재확인/redaction)에서
신규 결함 없음.

### D2 (기존, 재현) — routed flock delete의 `cleanup_failed` 오보고

- 2026-07-10에 이미 회부된 미해결 결함(run handoff 결함 회부 D2, stack handoff
  Follow-Up). 이번에도 `anvil_delete_flock`이 `is_error:true`
  (`"routed flock delete cleanup pending: ... reason=cleanup_failed"`)를
  반환했으나 **실제 teardown은 전부 성공**(양 host flock 404 + VM 0 + token
  revoke 401 확인). 보고/멱등성 결함, ⑥b와 무관. fix는 stack Follow-Up 큐에
  잔존(우선순위 유지).

## 후속 작업

1. **(문서, 소소)** 절차 ⑥b 문언의 "`GET /flocks/{id}` kind" 관측을 on-disk
   metadata + 행위(forward) + audit 3중 관측으로 보정 — 이번 갱신에 note로
   반영, 정식 문언 정리는 별도.
2. **D2 fix** — delete cleanup 보고 정합(기존 Follow-Up 유지, ⑥b로 재확인됨).
3. **비동기 relay buffer 재평가** — home failover 실검증 완료로 이제 우선순위
   판단 가능(stack/handoff Follow-Up 참고).
4. **zone `~/projects/claude-zone/docs/FOLLOWUP.md` P3-09** — ⑥b 수행·PASS로
   갱신(zone repo는 이 branch 밖 — 트리거만 기록).

## zone 연동

- 검증 인프라 사실(서버 접속·Go 경로·ZFS 4K dataset·op-token 방식)은 세션
  메모리 `anvil-test-servers`/`anvil-session-workflow`와 정합.
- 서버는 배포본(`0feb9fb`) 유지 상태로 남김(daemon 정지, flock/VM 소멸,
  snapshots mount·configs 보존).
