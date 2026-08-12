# ephemera 및 anvil 분석 문서 색인

## 기준 정보

- 분석 대상: `ephemera` runtime 및 이를 통합하는 `anvil` product
- anvil 관점: ephemera runtime은 IronClaw 결합 프로젝트의 기반 실행 계층
- 공식 저장소: `https://github.com/HardcoreMonk/anvil/`
- 0.1.0 기준 커밋: `157753fb5234679ca7cbebb6658e431c6a748ef6`
- 0.2.0 기준 커밋: `abcaa86`
- anvil 현재 runtime baseline: upstream ephemera `v0.7.0` 병합·적응 완료
- upstream latest observed: ephemera `v0.7.0` (2026-08-13 확인, pending sync 없음)
- 최신 anvil 공개 release: `anvil-v0.7.0`

## 0.1.0 문서

0.1.0 문서는 ephemera 초기 소스 분석 결과다. 코드 경로와 모듈명에는
`ephemera`가 그대로 남아 있으며, 이 표현은 anvil로 바꾸지 않는다.

- `01-source-line-analysis.md`: 0.1.0 소스 구조와 파일별 분석
- `02-junior-developer-report.md`: 주니어 개발자용 진입 보고서
- `03-non-technical-report.md`: 비기술 독자용 설명 보고서

## 0.2.0 문서

0.2.0 문서는 0.1.0 문서 구조를 기준으로, 릴리즈 diff와 신규 기능을 반영해 작성했다.

- `04-v0.2.0-diff-from-v0.1.0.md`: 0.1.0 대비 0.2.0 변경 분석
- `05-source-line-analysis-v0.2.0.md`: 0.2.0 소스 구조와 모듈별 분석
- `06-junior-developer-report-v0.2.0.md`: 주니어 개발자용 0.2.0 보고서
- `07-non-technical-report-v0.2.0.md`: 비기술 독자용 0.2.0 보고서

## 0.3.x upstream 검토 문서

0.3.x 문서는 anvil에 이미 병합된 runtime baseline과 upstream에 새로 나온 후보를
구분해 검토한다. ephemera release tag는 anvil product tag가 아니며, anvil 채택
여부는 별도 sync branch와 공개 경계 검토를 거쳐 확정한다.

- `08-v0.3.2-v0.3.3-upstream-change-review.md`: upstream ephemera `v0.3.2`,
  `v0.3.3` 변경 요약, 태그/commit/diff 근거, anvil 채택 검토 포인트
- `09-v0.3.6-upstream-change-review.md`: upstream ephemera `v0.3.6` webdev demo,
  `gtcall`, `gtwall`, Goose JSON output 변경의 anvil 채택 검토 포인트

## 0.4.x upstream 검토 문서

0.4.x 문서는 아직 anvil baseline으로 병합되지 않은 ephemera runtime 안정화 후보를
분류한다. 분류는 pre-sync adoption review이며, 실제 채택은 `sync/ephemera-*`
브랜치의 merge commit과 KVM 검증 뒤 확정한다.

- `10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`: upstream ephemera
  `v0.4.0`-`v0.4.5` storage/recovery, auth/audit, COW default, flock lifecycle,
  streaming task, restored VM recovery 변경의 anvil 예비 분류

## 0.5.x-0.7.x upstream 검토 문서

upstream ephemera `v0.5.x`-`v0.7.0`은 anvil의 runtime/operator baseline으로
병합·적응됐다. ephemera release 제목과 anvil product release는 계속 구분한다.

- `11-v0.5.0-v0.7.0-core-service-parity-review.md`: upstream ephemera
  `v0.5.0`-`v0.7.0`과 cross-phase parity를 `adopted`/`adapted`/`deferred`/`excluded`로
  분류하고 anvil 경계를 검증한 최종 review

## anvil 프로젝트 공정 분석

- `12-anvil-project-process-status-review-2026-08-13.md`: Git/fork/upstream,
  lifecycle 산출물, CI, 로컬 Go/Web 검증, secret gate, 운영 residual risk를 교차검증한
  현재 공정 상태 보고서

## 권장 읽기 순서

1. `04-v0.2.0-diff-from-v0.1.0.md`
2. `07-non-technical-report-v0.2.0.md`
3. `06-junior-developer-report-v0.2.0.md`
4. `05-source-line-analysis-v0.2.0.md`
5. upstream sync 검토가 목적이면
   `08-v0.3.2-v0.3.3-upstream-change-review.md`와
   `09-v0.3.6-upstream-change-review.md`,
   `10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`,
   `11-v0.5.0-v0.7.0-core-service-parity-review.md`
6. 현재 프로젝트 공정·release gate 판단이 목적이면
   `12-anvil-project-process-status-review-2026-08-13.md`

초기 runtime 이해가 목적이면 4번 비교 문서와 7번 비기술 보고서를 먼저 본다. 현재
release 의사결정은 12번 공정 보고서에서 시작하고, runtime 채택 근거가 필요할 때 11번
parity review로 내려간다.
