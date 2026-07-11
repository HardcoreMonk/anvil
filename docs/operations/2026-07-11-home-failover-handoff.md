# Home 재선출 Failover Handoff

- 작성일: 2026-07-11
- 대상 branch: `feature/home-failover`
- 대상: `cmd/goose-daemon`(`orchestrator_api.go` — hub 승격/relay 강등),
  `internal/anvilmcp`(`home_failover.go` 신설, `runtime_router.go` reconcile
  per-host 격리 + failover 발화), `scripts/anvil-cross-host-failover-e2e.sh`
- 상태: 구현·유닛(+`-race`)·KVM e2e 게이트 완료. **실 2-daemon 수동 검증
  (§6b) 2026-07-11 수행 완료 — 전 세부 단계 PASS** (감지·재선출·kind hub→relay
  강등·wall 손실 계약·양방향 wall/gtcall 재확인·redaction·정리+revoke; 전환
  창 실측 ~27s @`reconcile 10s`; 신규 결함 없음). 기록:
  [`docs/operations/2026-07-11-6b-failover-verification-run.md`](2026-07-11-6b-failover-verification-run.md).
  설계:
  [`docs/superpowers/specs/2026-07-08-home-failover-design.md`](../superpowers/specs/2026-07-08-home-failover-design.md)
- 선행: [`docs/operations/2026-07-08-routed-flock-stack-handoff.md`](2026-07-08-routed-flock-stack-handoff.md)
  (stack 전체 상태·Next Action), [`docs/operations/2026-07-10-cross-host-verification-run-handoff.md`](2026-07-10-cross-host-verification-run-handoff.md)
  (D1/D1b fix로 이 slice 착수 게이트가 열림)

## 무엇이 main에 있나

이 slice는 아직 `feature/home-failover` branch에만 있다 — main 병합은
"최종 검증(전체 슬라이스)" 통과와 PR 승인 이후에 이뤄진다(자체 머지 금지).
이 handoff는 branch 상태를 기록한다.

routed flock의 home host(hub) SPOF를 hub 복제 없이 **재선출 failover**로
해소한다.

### kind 전환 (daemon, spec 보정)

원래 spec(`2026-07-08-home-failover-design.md`)은 "기존 배관 재사용"을
전제했지만, D1 fix(PR #30)가 도입한 kind 충돌 `409` 가드 때문에 daemon
쪽에 새 kind 전환 경로가 필요했다 — 이것은 spec 보정 사항으로, 리뷰어가
기각 가능하도록 별도로 남긴다:

- `POST /flocks/{id}/distributed`가 **relay 점유 id 위에서 hub로 승격**한다
  (`201`) — 승격된 hub의 `Agents` map은 빈 map으로 시작한다(`deleteFlock`의
  VM-safety 불변식: Agents가 있으면 삭제를 거부하므로, 승격 경로로 만들어진
  hub도 신규 hub와 동일하게 비어 있어야 그 불변식이 유지된다).
- `POST /flocks/{id}/relay`가 **hub 점유 id 위에서 relay로 강등**한다
  (`201`) — 강등 시 구 `TOWN_WALL.log`는 디스크에 남는다(정리하지 않음,
  wall 손실 계약의 일부).
- **local flock은 양쪽 endpoint 모두에서 `409` 불변으로 보호된다**(기존
  가드 유지).
- 두 endpoint 모두 **CP bearer 전용**이다 — `relay_token`/`call_token`은 이
  승격·강등 요청을 admit하지 않는다(guest나 relay hop이 kind를 스스로 바꿀
  수 없다).

### reconcile per-host 격리 (adapter)

`ReconcilePlacements`가 **host 단위로 격리**되도록 고쳤다 — 죽은 host의
placement가 다른 host의 reconcile 결과에 영향을 주지 않고 carry-over되며,
wall heal도 계속 진행된다. `hostProbe`가 각 host의 도달성을 관측해
failover 감지·선출의 입력이 된다.

### 감지 · 선출 · 전환

- **감지**: adapter reconcile 루프가 flock 단위로 **연속
  `homeFailureThreshold`회(상수, 기본 3)** dial-계열 home 실패를 관측하면
  발화한다. 카운터는 성공 시 리셋되고, 후보가 없으면(모든 member 다운 또는
  단일-host flock) 카운터는 포화 상태를 유지한 채 no-op으로 다음 주기
  재평가한다.
- **선출(결정적)**: 직전 reconcile에서 생존 관측된 host 중
  `record.Agents` 순서상 첫 host, 구 home 제외. 같은 입력은 항상 같은
  결론이다 — elector가 단일 control plane(adapter)이라 split-brain이
  구조적으로 불가능하다.
- **전환**: (1) `record.HomeHost` 영속 — **원자적 전환점**, 이후 모든
  reconcile 재등록의 기준이 바뀐다. (2) 새 home에 hub 승격 등록
  (VMID/Addr roster + relay/call token). (3) 구 home을 포함한 전 member에
  relay 재등록(새 HomeAddr). (4) 구 home에 best-effort `DELETE`(도달 불가면
  skip — 부활 시 stale hub는 아무도 참조하지 않고, 다음 reconcile이
  relay로 강등해 heal한다). 어느 단계가 실패해도 다음 reconcile 주기가
  idempotent 수렴한다.
- **토큰 불변**: relay/call token은 flock 단위로 그대로 재사용된다 — guest
  주입 토큰(`.ephemera-cp-token`)이 바뀌지 않으므로 guest는 무중단·무개입.
- **자동 fail-back 없음**: 구 home이 부활해도 새 home을 유지한다(flap
  방지) — 구 home은 다음 reconcile에서 relay로 강등돼 heal된다.
- 발화는 `record.Status == RoutedFlockStatusReady`에만 국한된다. 로그·
  에러 문자열은 flock/host 식별자만 남긴다(daemon 주소·토큰 미노출).

### Wall 손실 semantics (명시 계약)

failover 후 새 home은 빈 `TOWN_WALL.log`에서 seq를 재시작한다. **이전
기록은 구 home 디스크에 남지만 새 wall로 병합되지 않는다.** agent
관점에서는 `gtwall`/history가 계속 동작하되 과거 메시지가 사라진 것으로
보인다.

## 보안 경계

- kind 전환 두 endpoint(`/flocks/{id}/distributed`, `/flocks/{id}/relay`)는
  CP bearer 전용이다 — guest 능력 토큰(`relay_token`)이나 hop 전용
  (`call_token`)은 admit되지 않는다. local flock 보호(`409`)는 승격/강등
  양쪽에서 불변이다.
- 승격 hub의 `Agents` map이 항상 빈 map으로 시작한다는 불변식은
  `deleteFlock`의 VM-safety 가드(Agents가 있으면 삭제 거부)와 정합한다 —
  승격 경로가 이 불변식을 깨면 승격된 hub를 삭제할 때 존재하지 않는 VM을
  teardown하려는 시도가 생길 수 있었다.
- relay/call token은 failover 전후로 값이 바뀌지 않는다 — 토큰 재발급/회전
  경로 없음, guest 관점에서 무중단.
- 로그·에러 문자열은 flock/host 식별자만 남긴다(reconcile-loop/gtcall
  slice에서 확립한 규율과 동일 — daemon 주소·토큰 미노출). KVM e2e가
  redaction을 관측 단언한다.

## Gate 결과

- **유닛**: `internal/anvilmcp/home_failover_test.go`에 `TestFailover_*`
  8종 — 연속 임계 발화, 임계 미만 no-op, 성공 개재 카운터 리셋, non-dial
  에러 미카운트, 후보 0 no-op, 부분 전환 수렴, 구 home 부활 시 relay
  유지(자동 fail-back 없음), 로그 redaction. 전부 green.
- **race**: `-race` 병행, 전체 `internal/... cmd/...` suite green.
- **빌드**: `go build ./... && go vet ./... && gofmt -l .`(`web/` 제외)
  clean.
- **KVM e2e**(`scripts/anvil-cross-host-failover-e2e.sh`): stub A → stub
  B로 재선출(멤버 daemon relay 갱신 wire 캡처) + real daemon relay→hub
  승격 경로, wall 손실 계약 관측, redaction 검증. 3회 연속 green.
- **회귀**: 기존 cross-host e2e(`anvil-cross-host-wall-e2e.sh`,
  `anvil-cross-host-gtcall-e2e.sh`) — 등록 handler를 만졌으므로 회귀
  필수(등록 endpoint 승격/강등 로직 추가가 기존 hub/relay 최초 등록
  경로를 깨지 않는지 확인).
- **범위 밖(이 handoff 작성 시점 기준)**: 전체 KVM 게이트(`e2e_test.sh`),
  secret-scan, PR 생성은 "최종 검증(전체 슬라이스)" 단계에서 별도 실행 —
  이 handoff는 문서 작업(Task 6) 완료 시점 기록이다.

## Known limitations / 운영 주의

- **home SPOF는 완전히 사라지지 않는다 — 전환 창이 있다.** 감지에
  `homeFailureThreshold`(3) × `ANVIL_MCP_RECONCILE_INTERVAL`(기본 `60s`)회
  reconcile pass가 필요하고 여기에 전환 자체 소요 시간이 더해진다 — 기본
  설정 기준 최대 ~3분. 이 창 동안 wall/call은 기존과 동일하게 502 +
  bounded retry로 관측된다.
- **wall 과거 기록은 복구되지 않는다.** 복제가 없으므로 이것은 설계된
  trade-off이지 버그가 아니다 — 운영자는 이 계약을 flock 사용자에게
  사전 공지해야 한다.
- **자동 fail-back 없음.** 구 home이 완전히 복구돼도 adapter가 자동으로
  되돌리지 않는다 — 원래 host로 되돌리려면 runbook의 수동 fail-back
  절차를 따른다(adapter 중지 → placements.json `home_host` 수정 → adapter
  재기동).
- ~~**실 2-daemon 수동 검증 미수행.**~~ **2026-07-11 수행 완료 — 전 세부
  단계 PASS.** 실 2-daemon(192.168.1.19/.20, `0feb9fb`)에서 전환 창 실측
  ~27s(`reconcile 10s`; 기본 60s 환산 ~2.7분), 새 home 경유 wall/gtcall
  양방향 재확인, 구 home hub→relay 강등(on-disk metadata `kind:"relay"` +
  구 home guest gtwall forward `409` 없음 + audit `/distributed`→`/relay`),
  wall 손실 계약 관측 확인. 기록:
  [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
- 단일-host flock과 생존 후보가 0인 경우 failover는 no-op이다(현행 502
  지속) — 이것은 spec의 명시 비목표(경계 사례)이지 결함이 아니다.

## Next Action

1. ~~**실 2-daemon 수동 검증 §6b 수행**~~ — **완료 (2026-07-11, PASS)**.
   기록: [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
2. ~~§6b 통과 후 "최종 검증(전체 슬라이스)" + PR 생성~~ — **완료**: slice는
   PR #33로 main 병합됨(`0feb9fb`). §6b는 그 병합본에 대한 최종 field 게이트로
   수행돼 PASS — slice 전 게이트 CLOSED.

## Follow-Up Tasks

1. ~~**실 2-daemon 수동 검증 §6b failover 시나리오 수행**~~ — **완료
   (2026-07-11, 전 세부 단계 PASS)**. home daemon 정지 → `home_host`
   host-a→host-b 전환(실측 ~27s @`reconcile 10s`) → 새 home 경유 wall
   양방향·gtcall 재확인 → 구 home 재기동 → hub→relay 강등(forward `409`
   없음) → wall 손실 계약(전환 전 메시지 부재) 확인. 신규 결함 없음(정리
   단계에서 기존 D2 재현만). 기록:
   [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
   절차: [2026-07-08-cross-host-manual-verification.md](2026-07-08-cross-host-manual-verification.md) ⑥b.
2. **zone `~/projects/claude-zone/docs/FOLLOWUP.md` P3-09 갱신** — zone
   repo는 이 anvil branch 밖이므로 이 handoff에는 트리거만 기록한다. 갱신
   내용: home failover 구현 완료, 대기 게이트는 §6b 실 2-daemon 수동 검증.
3. **stack handoff Next Action 갱신** — 완료
   ([`2026-07-08-routed-flock-stack-handoff.md`](2026-07-08-routed-flock-stack-handoff.md)의
   Next Action 3번을 이 handoff와 같은 커밋에서 완료로 직접 갱신했다).
4. **cross-flock isolation 커버리지 테스트(Option B) 후속 후보** — Task 4
   리뷰(`.superpowers/sdd/task-4-report.md`)에서 파생된 minor.
   `TestReconcilePlacements_IsolatesUnreachableHost`는 Option A(신규
   semantics에 맞춘 단언 갱신)로 해소됐지만, "죽은 daemon 하나가 전체
   pass를 중단시키지 않는다"는 원래 isolation 의도를 완전히 보존하려면
   두 개의 routed flock(한 flock의 home은 dead, 다른 flock의 member relay
   healing은 계속 동작)으로 재구성한 진짜 cross-**flock** isolation 테스트가
   필요하다 — product 코드 변경 없이 `runtime_router_test.go` 단일 파일
   추가로 가능. 별도 승인 후 착수.
5. ~~비동기 relay buffer 재평가~~ — **기각 확정 (2026-07-11 사용자 결정)**:
   failover의 불능 창 유계화(§6b 실측 ~27s@10s interval)로 명분 소멸 + wall
   손실 계약과 충돌하는 부분-부활 semantics·전달 보장 약화·seq/중복 복잡성
   재도입. 대안(guest-side `gtwall` 지수 backoff 재시도)은 필요 대두 시 별도
   등재.
6. **(최종 whole-branch 리뷰 파생, minor)** 감지 카운터의 "연속" 정밀화 —
   home이 probe에는 도달 가능하나 hub 재등록이 비-dial 에러로 실패하는
   pass는 카운터를 리셋하지 않아, flapping home이 이론상 최대 2 pass 이르게
   발화할 수 있다(스펙 문언 "성공 시 리셋"에는 합치, 동작상 무해).
   원하면 `probes[homeHost].reachable`일 때도 리셋하도록 한 줄 보강.
7. **(최종 whole-branch 리뷰 파생, minor·테스트 위생)** failover e2e의
   cleanup이 `RESEARCHER_VMID` 미포착 시 VM DELETE를 생략하는 좁은 누수
   창, 그리고 adapter 로그 grep이 리터럴 "home failover" 문구에 결합된
   취약성 — 스크립트 후속 폴리시 후보.
