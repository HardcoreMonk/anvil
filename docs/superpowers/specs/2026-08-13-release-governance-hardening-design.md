# Release governance hardening 설계

**날짜:** 2026-08-13

**상태:** 승인됨 — 사용자의 후속 작업 3·5·6·7 전체 실행 승인

**topic:** `release-governance-hardening`

## 목표

Web dependency advisory, 누락된 Web/secret CI, 무보호 `main`, 다음 version 결정 규칙을
하나의 release governance contract로 닫는다. 모든 gate가 닫히기 전에는 tag를 만들지
않는다.

## 포함 범위

1. `web/package-lock.json`의 npm High/Moderate advisory 0건 처리
2. CI에 Web check/build/audit와 tracked-tree secret scan 추가
3. merge 후 `main` branch protection에 required status/review/conversation resolution 적용
4. next anvil version selection rule과 현재 번호 미할당 blocker 명문화
5. exact SHA CI와 operation handoff

## 비목표

- Svelte UI/runes migration 또는 warning 10건 전면 수정
- upstream tag 생성 요청이나 upstream release 일정 추측
- current release tag/GitHub Release 생성
- 실제 secret 또는 credential 출력

## Domain Architecture

- **Dependency boundary:** `web/package.json` direct dependencies와 npm overrides,
  `package-lock.json` resolved graph
- **Verification boundary:** GitHub Actions의 `build-and-test`, `web-and-security`,
  `secret-scan` 독립 job
- **Merge governance boundary:** GitHub `main` branch protection
- **Version boundary:** upstream ephemera version → downstream `anvil-` tag transform

runtime process, HTTP/MCP schema, UI behavior는 바뀌지 않는다. workflow/release contract
변경이므로 full lifecycle을 사용한다.

## 설계 결정

### D1. 취약 transitive를 override하고 audit를 moderate부터 차단한다

- `svelte-i18n@4.0.1`의 `esbuild@0.19.12`는 Vite graph와 공유 가능한 patched
  `0.28.2`로 override한다.
- Vite graph의 `postcss`와 `nanoid`는 현재 compatible patched release로 override한다.
- `npm audit --audit-level=moderate`가 0건이어야 CI green이다.
- `npm ci`, `npm run check`, `npm run build`로 override compatibility를 검증한다.

direct `svelte-i18n`을 npm fixer 제안대로 3.x로 downgrade하지 않는다. 현재 Svelte 5 app의
direct major를 되돌리는 것보다 build-tested transitive patch가 작다.

### D2. Web과 secret을 독립 CI job으로 둔다

- `web-and-security`: Node 22, npm cache, `npm ci`, check, build, moderate audit
- `secret-scan`: least-privilege checkout 뒤 `bash scripts/secret-scan.sh`

두 job을 분리해 dependency failure와 source-secret failure를 구분한다.

### D3. branch protection은 strict required checks + 1 approval이다

`main`에 세 CI context와 CodeRabbit review status, 1 approving review, stale review dismiss,
last-push approval, conversation resolution, force-push/deletion 금지를 적용하고 admin에도
enforce한다.

현재 collaborator는 repository owner 한 명뿐이라 작성자가 self-approve할 수 없다. 이
설정 이후 merge하려면 두 번째 eligible reviewer를 collaborator로 추가해야 한다. 이는
의도된 강한 gate이며 handoff의 immediate follow-up이다.

### D4. 다음 version은 규칙을 확정하되 번호를 발명하지 않는다

결정 규칙:

```text
next_upstream_tag = upstream의 첫 post-v0.7.0 정식 tag
next_anvil_tag    = "anvil-" + next_upstream_tag
```

downstream-only `anvil-v0.7.1`이나 추정 `anvil-v0.8.0`은 만들지 않는다. 조사 시점 upstream
latest/main은 `v0.7.0`이므로 next version number는 **미할당(blocked)**이다. 이는 모호한
TBD가 아니라 upstream event를 입력으로 하는 결정적 policy와 명시적 release gate다.

### D5. tag는 금지한다

이번 workflow, dependency, protection 작업은 tag 생성 권한으로 확대되지 않는다.
host security operation과 version input이 닫힐 때까지 current head에는 tag가 없어야 한다.

## Plan Design Review

- CI job 이름은 branch protection context와 정확히 일치한다.
- Web audit와 source-secret 실패가 별도 job으로 보여 triage가 명확하다.
- version 문서는 “예상 번호” 대신 입력/변환/gate를 보여 operator 오판을 막는다.
- strict review가 sole-owner repository를 잠그는 결과를 사전에 명시한다.
- UI 변경 없음. workflow IA와 gate discoverability review로 대체했고 blocker 없음.

## 수용 기준

1. `npm audit --audit-level=moderate`가 vulnerability 0, exit 0이다.
2. `npm ci`, `npm run check`, `npm run build`가 통과한다.
3. CI에 `web-and-security`, `secret-scan` job이 있고 least-privilege다.
4. branch exact SHA에서 Go/Web/secret jobs가 green이다.
5. `main` protection이 strict checks, CodeRabbit, 1 approval, stale/last-push,
   conversation resolution, admin enforce, force-push/delete 금지를 반영한다.
6. next version transform rule과 current unassigned blocker가 canonical 문서에 있다.
7. current/upstream remote tag inventory에 새 tag가 없다.
8. handoff가 sole-owner merge consequence와 reviewer 추가 필요를 기록한다.

## Spec Freeze Snapshot

- **Objective:** npm advisory 0, Web/secret required CI, strict main governance, version rule
- **Dependency decision:** patched transitive overrides + compatibility build
- **CI contexts:** `build-and-test`, `web-and-security`, `secret-scan`, `CodeRabbit`
- **Review decision:** admin-enforced 1 approval; second collaborator required
- **Version decision:** next upstream tag를 exact transform, current number unassigned
- **Publish decision:** no tag/release
- **Open questions:** 없음
