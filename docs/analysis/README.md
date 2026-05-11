# anvil 분석 문서 색인

## 기준 정보

- 공식 프로젝트 명칭: `anvil`
- 공식 저장소: `https://github.com/HardcoreMonk/ephemera/`
- 0.1.0 기준 커밋: `157753fb5234679ca7cbebb6658e431c6a748ef6`
- 0.2.0 기준 커밋: `abcaa86`

## 0.1.0 문서

0.1.0 문서는 초기 소스 분석 결과다. 현재 문서 제목과 설명은 공식 제품명 `anvil`로 정리했으며, 코드 경로와 모듈명에는 당시 코드베이스 명칭인 `ephemera`가 남아 있을 수 있다.

- `01-source-line-analysis.md`: 0.1.0 소스 구조와 파일별 분석
- `02-junior-developer-report.md`: 주니어 개발자용 진입 보고서
- `03-non-technical-report.md`: 비기술 독자용 설명 보고서

## 0.2.0 문서

0.2.0 문서는 0.1.0 문서 구조를 기준으로, 릴리즈 diff와 신규 기능을 반영해 작성했다.

- `04-v0.2.0-diff-from-v0.1.0.md`: 0.1.0 대비 0.2.0 변경 분석
- `05-source-line-analysis-v0.2.0.md`: 0.2.0 소스 구조와 모듈별 분석
- `06-junior-developer-report-v0.2.0.md`: 주니어 개발자용 0.2.0 보고서
- `07-non-technical-report-v0.2.0.md`: 비기술 독자용 0.2.0 보고서

## 권장 읽기 순서

1. `04-v0.2.0-diff-from-v0.1.0.md`
2. `07-non-technical-report-v0.2.0.md`
3. `06-junior-developer-report-v0.2.0.md`
4. `05-source-line-analysis-v0.2.0.md`

빠른 의사결정이 목적이면 4번 비교 문서와 7번 비기술 보고서를 먼저 보면 된다. 구현에 투입될 개발자는 6번 보고서를 읽은 뒤 5번 소스 분석으로 들어가는 편이 좋다.
