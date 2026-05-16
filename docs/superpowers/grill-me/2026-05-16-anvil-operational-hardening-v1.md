---
lifecycle_run: 2026-05-16-anvil-operational-hardening-v1
lifecycle_stage: grill-me
lifecycle_status: passed
generated_by: codex
generated_at: 2026-05-16T00:00:00+09:00
redaction_applied: true
---
# Grill-Me 승인: anvil operational hardening v1

이 문서는 purecvisor에서 유용한 운영 패턴 1-5를 anvil에 반영하기 전의
grill-me gate다. 질문은 기능 경계, API 호환성, 보안 경계, 검증 기준을 먼저
고정하기 위한 것이다.

현재 implementation plan 초안은
`docs/superpowers/plans/2026-05-16-anvil-operational-hardening-v1.md`에 있지만,
이 grill-me가 `passed`로 갱신되었으므로 실행 대상으로 취급할 수 있다.

---

## Q1. Sync API 호환성과 job 추적 방식

**맥락:** purecvisor는 긴 작업을 `accepted + job_id`로 추적한다. anvil/ephemera의
현재 daemon API는 `POST /vms`, snapshot, restore가 동기 응답 body를 반환한다.
이 body를 갑자기 `202 Accepted` 중심으로 바꾸면 기존 MCP adapter와 E2E가 깨질 수
있다.

**질문:** v1에서 기존 sync API body를 유지하면서 `X-Anvil-Job-ID`, `/jobs`,
`/audit`을 추가하는 호환 확장으로 시작할 것인가?

**권장 답변:** 예. v1은 기존 response body와 status code를 유지한다. `job_id`는
header와 `/jobs` 조회로 추가하고, `Prefer: respond-async`나 `?async=true` 기반
비동기 전환은 별도 ADR에서 후속으로 결정한다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. v1은 기존 sync response body와 status code를 유지하고
`X-Anvil-Job-ID`, `/jobs`, `/audit`을 호환 확장으로 추가한다.

**Plan 반영:** Task 1, Task 2의 job/audit foundation은 sync compatibility를
전제로 작성되어 있다.

---

## Q2. Audit에 남길 수 있는 데이터 경계

**맥락:** anvil은 agent prompt, workspace content, daemon token, guest
`agent_token` 같은 민감 데이터를 다룬다. audit이 운영 추적에는 필요하지만, 민감
데이터 저장소가 되면 안 된다.

**질문:** v1 audit event에는 action, target, owner/client, role, result, job_id,
error summary만 남기고 prompt, workspace content, Bearer token, `agent_token`은
절대 남기지 않는 정책으로 둘 것인가?

**권장 답변:** 예. v1 audit은 in-memory ring으로 시작하고 민감 body는 기록하지
않는다. persistent audit store는 retention/암호화/삭제 정책이 정리될 때까지
`deferred`로 둔다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. v1 audit은 action, target, owner/client, role, result,
job_id, error summary만 기록하고 prompt, workspace content, Bearer token,
`agent_token`은 기록하지 않는다.

**Plan 반영:** Task 1, Task 2, Task 7의 token 미노출 검증에 반영되어 있다.

---

## Q3. `/health`와 `/metrics` 공개 방식

**맥락:** purecvisor는 health/metrics를 운영 표면으로 강하게 제공한다. 하지만
anvil은 Firecracker runtime과 VM 내부 agent 정보를 다루므로, unauth metrics가
초기에 노출되면 내부 상태가 새어나갈 수 있다.

**질문:** v1에서는 daemon `/health`와 `/metrics`를 기존 control-plane auth 뒤에
두고, 인증 없는 readiness endpoint는 추가하지 않는 것으로 시작할 것인가?

**권장 답변:** 예. v1은 운영자용 auth-protected endpoint로 시작한다. reverse
proxy 또는 systemd watchdog용 unauth readiness가 필요해지면 별도 ADR로 범위를
정한다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. v1 `/health`와 `/metrics`는 기존 control-plane auth 뒤에
둔다. 인증 없는 readiness endpoint는 이번 범위에서 제외한다.

**Plan 반영:** Task 4 risk control에 반영되어 있다.

---

## Q4. RBAC/owner-scope 도입 방식

**맥락:** 기존 `EPHEMERA_API_TOKENS=name:token` 형식은 이미 운영 계약이다.
owner-scope를 도입하면서 기존 배포를 깨면 안 된다.

**질문:** 기존 `name:token`은 `admin`으로 해석해 호환성을 유지하고, 새
`name:role:token` 형식으로 `operator`, `viewer`를 추가하는 방식으로 갈 것인가?

**권장 답변:** 예. v1에서는 `admin`, `operator`, `viewer`만 허용한다.
`operator`와 `viewer`는 owner가 일치하는 VM/snapshot/job/audit만 볼 수 있고,
mutation은 role에 따라 제한한다. no-token local dev mode는 기존처럼 admin
호환으로 유지한다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. 기존 `name:token` 형식은 `admin` 호환으로 유지하고,
새 `name:role:token` 형식으로 `admin`, `operator`, `viewer`를 지원한다.

**Plan 반영:** Task 5에 반영되어 있다.

---

## Q5. Network cleanup의 자동화 수준

**맥락:** anvil은 TAP/IP, dm-snapshot, loop device, bind mount, sparse COW file
cleanup이 중요하다. 하지만 자동 network cleanup을 잘못 실행하면 살아 있는 VM의
TAP을 지울 수 있다.

**질문:** v1에서는 network status와 cleanup dry-run plan만 제공하고, 실제 자동
삭제는 후속 ADR과 E2E 검증 전까지 제외할 것인가?

**권장 답변:** 예. `/network/status`와 `/network/cleanup-plan`만 추가한다.
실제 cleanup 실행 API는 v1에서 제외한다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. v1은 `/network/status`와 `/network/cleanup-plan`만
추가하고, 실제 cleanup 실행 API는 후속 ADR 전까지 제외한다.

**Plan 반영:** Task 6과 Risk Controls에 반영되어 있다.

---

## Q6. Release/operate blocker

**맥락:** 운영 기능은 문서가 맞아도 full KVM E2E가 없으면 실제 안전성을 보장하기
어렵다.

**질문:** 이 기능 묶음의 완료 기준을 unit test, daemon build, MCP build,
full KVM E2E, `agent_token` 미노출 검사, cleanup 최종 상태 확인까지로 둘 것인가?

**권장 답변:** 예. 특히 `/jobs`, `/audit`, `/metrics`, `/network/status`,
MCP output, replay output에 `agent_token`이 나오면 release blocker로 본다.

**사용자 답변:** 승인. 권장 답변대로 진행한다.

**결정:** accepted. 완료 기준은 unit test, daemon build, MCP build,
full KVM E2E, `agent_token` 미노출 검사, cleanup 최종 상태 확인까지로 둔다.

**Plan 반영:** Task 7과 Final Verification에 반영되어 있다.

---

## Gate 상태

- 현재 상태: `passed`
- 승인 근거: 사용자가 2026-05-16에 Q1-Q6 권장 답변 진행을 승인했다.
- 다음 단계: `docs/superpowers/plans/2026-05-16-anvil-operational-hardening-v1.md`
  기준으로 구현을 시작할 수 있다.
