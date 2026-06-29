# ephemera 분석 문서 색인

## 기준 정보

- 분석 대상: `ephemera` runtime
- anvil 관점: ephemera runtime은 IronClaw 결합 프로젝트의 기반 실행 계층
- 공식 저장소: `https://github.com/HardcoreMonk/anvil/`
- 0.1.0 기준 커밋: `157753fb5234679ca7cbebb6658e431c6a748ef6`
- 0.2.0 기준 커밋: `abcaa86`
- anvil 현재 sync branch runtime baseline: upstream ephemera `v0.3.6` 병합분
- 다음 upstream 후보: ephemera `v0.4.0`-`v0.4.5` runtime 안정화 변경

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

## 권장 읽기 순서

1. `04-v0.2.0-diff-from-v0.1.0.md`
2. `07-non-technical-report-v0.2.0.md`
3. `06-junior-developer-report-v0.2.0.md`
4. `05-source-line-analysis-v0.2.0.md`
5. upstream sync 검토가 목적이면
   `08-v0.3.2-v0.3.3-upstream-change-review.md`와
   `09-v0.3.6-upstream-change-review.md`,
   `10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`

빠른 의사결정이 목적이면 4번 비교 문서와 7번 비기술 보고서를 먼저 보면 된다. 구현에 투입될 개발자는 6번 보고서를 읽은 뒤 5번 소스 분석으로 들어가는 편이 좋다.
