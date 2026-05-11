---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: operate
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# 운영 인계: anvil

## 릴리즈 범위

- anvil 프로젝트 정체성, 도메인 용어집, 공식 문서 계층, lifecycle 근거를
  정리하는 문서 전용 재설계.
- runtime 동작, daemon API, MCP tool 계약, config 우선순위, session alias
  의미, snapshot/restore 동작은 변경하지 않는다.

## 현재 lifecycle 단계

`operate` 단계에 진입했다. 이 문서 전용 재설계는 구현, code review, 최종
검증, release 승인을 완료했다. `operate` 진입은 2026-05-11에 승인되었다.

## 검증

- `/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign`
- `git diff --check`
- `go test ./...`
- `cmd/`, `internal/`, `go.mod`, `go.sum`에 대한 runtime diff guard

## 코드 review 결과

- 최종 branch review에서 blocking issue는 발견되지 않았다.
- 검토된 변경 범위는 문서, lifecycle 근거, README 명령과 맞추기 위한
  `e2e_test.sh` binary name 정렬로 제한되었다.
- 보호 대상 runtime path와 config 계약 path는 변경되지 않았다.

## 감사 후보

- 생성 artifact에 secret 또는 오래된 내부 참조가 없는지 확인한다.
- 공식 문서와 계산된 snapshot이 명확히 분리되어 있는지 확인한다.

## 차단 항목

- 없음.

## 경고

- scan summary는 범위가 제한되어 있고 redaction이 적용되어 있다.
- 생성 draft는 수동 완료 전까지 project-specific decision을 일부 생략할 수
  있다.

## 잔여 위험 후보

- legacy 문서가 stale 상태일 수 있다.
- scan summary는 범위가 제한되어 있고 redaction이 적용되어 있다.

## 다음 작업

- public tag, release note 게시 등 release hygiene은 별도 follow-up으로
  추적한다.

## 생명주기 gate 근거

- Stage: `operate`
- 상태: `passed`
- Approved by: `user`
- 근거: 사용자는 release 승인과 최종 검증 완료 뒤 2026-05-11에
  operate 진입을 승인했다.
