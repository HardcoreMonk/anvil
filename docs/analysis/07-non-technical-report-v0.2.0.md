# ephemera 0.2.0 비기술 보고서

## 요약

ephemera 0.2.0은 격리된 AI 작업 실행 환경을 단일 서버에서 더 안정적으로 운영하기 위한 릴리즈다. 0.1.0이 "격리된 VM을 만들고 작업을 실행할 수 있음"을 증명한 단계였다면, 0.2.0은 "그 VM의 상태를 저장하고, 다시 복원하고, 외부 시스템이 더 안전하게 사용할 수 있음"으로 발전했다. anvil 관점에서는 IronClaw와 결합할 기반 runtime이 강화된 것이다.

## 무엇이 달라졌나

### 1. VM을 더 안전하게 종료할 수 있다

0.2.0에는 VM 안에서 실행되는 작은 초기화 프로그램이 추가되었다. 이 프로그램은 VM 안의 작업 agent를 실행하고, 종료 요청이 들어오면 정리 절차를 거쳐 VM을 끈다.

사용자 관점의 의미:

- 작업 VM이 더 예측 가능하게 종료된다.
- snapshot이나 delete 전후의 상태 관리가 좋아진다.
- 향후 안정적인 세션 저장 기능의 기반이 된다.

### 2. VM마다 별도 인증값을 가진다

각 VM에는 고유한 agent token이 생긴다. 외부 사용자는 보통 이 token을 직접 다루지 않고, daemon이 내부적으로 처리한다.

사용자 관점의 의미:

- VM 내부 agent 접근이 더 안전해졌다.
- 여러 VM이 떠 있어도 인증 경계가 더 명확하다.
- 상위 시스템은 하나의 control-plane API를 기준으로 통합할 수 있다.

### 3. 외부 시스템이 VM에 직접 접근하지 않아도 된다

0.2.0에서는 daemon이 VM 내부 agent로 요청을 대신 전달한다.

이전 방식:

```text
외부 시스템 -> VM 내부 agent
```

0.2.0 방식:

```text
외부 시스템 -> daemon -> VM 내부 agent
```

사용자 관점의 의미:

- VM private IP를 직접 알 필요가 줄어든다.
- 인증과 routing을 daemon이 관리한다.
- IronClaw나 MCP adapter 같은 상위 시스템과 연결하기 쉬워진다.

### 4. VM 상태를 저장할 수 있다

0.2.0의 가장 큰 변화는 snapshot 기능이다. snapshot은 VM의 실행 상태를 저장하는 기능이다.

가능한 일:

- 현재 VM 상태 저장
- 저장된 상태 목록 조회
- 저장된 상태에서 새 VM 복원
- 더 이상 필요 없는 snapshot 삭제

사용자 관점의 의미:

- 긴 작업 중간 상태를 남길 수 있다.
- 실험 환경을 나중에 다시 열 수 있다.
- 실패한 작업을 이전 상태에서 재시도할 수 있는 기반이 생긴다.

### 5. 저장 공간을 아끼는 snapshot 방식이 들어갔다

0.2.0은 full snapshot뿐 아니라 diff snapshot도 지원한다. full snapshot은 기준 상태 전체를 저장하고, diff snapshot은 이후 변경분 중심으로 저장한다.

사용자 관점의 의미:

- 반복 저장 시 공간을 덜 쓸 수 있다.
- 세션 checkpoint를 더 자주 만들 수 있는 기반이 된다.
- 향후 UX에서 "되돌리기", "분기하기", "작업 체크포인트" 같은 기능으로 확장할 수 있다.

### 6. 복원 시 disk 전체를 매번 복사하지 않는다

0.2.0은 COW 방식의 rootfs restore를 도입했다. COW는 copy-on-write의 약자로, 처음부터 전체를 복사하지 않고 바뀌는 부분만 따로 기록하는 방식이다.

사용자 관점의 의미:

- snapshot 복원이 더 가벼워질 수 있다.
- 같은 기준 상태에서 여러 실험 VM을 만들 수 있는 기반이 된다.
- 저장 공간 사용량을 줄일 수 있다.

### 7. 설정과 테스트 체계가 좋아졌다

0.2.0에는 GitHub Actions CI와 여러 단위 테스트, e2e 테스트가 추가되었다.

사용자 관점의 의미:

- 변경 시 기본 빌드와 테스트를 자동 확인할 수 있다.
- 릴리즈 품질을 관리할 기준이 생겼다.
- snapshot, restore, proxy 같은 핵심 흐름을 반복 검증할 수 있다.

## 제품 관점에서의 의미

0.2.0은 ephemera를 "AI coding agent용 격리 실행 환경"으로 만들기 위한 중요한 전환점이다.

0.1.0에서 가능했던 것:

- 격리 VM 실행
- agent task 실행
- 기본 lifecycle 관리

0.2.0에서 가능해진 것:

- VM별 인증
- daemon 중심 통합 API
- profile별 VM 실행
- snapshot 저장
- snapshot 복원
- diff 기반 저장 최적화
- COW 기반 복원 최적화
- API key hot reload
- 자동 테스트

즉, 0.2.0은 PoC에서 single-host product foundation으로 넘어가는 릴리즈다.

## IronClaw 통합 관점

사용자가 처음 제공한 0.1 문서와 아키텍처에서 anvil은 IronClaw의 volatile execution plane 역할을 한다.

0.2.0은 이 통합 가능성을 더 높인다.

### 좋아진 점

- IronClaw가 VM 내부 IP를 직접 다루지 않아도 된다.
- daemon API만 호출해 task를 실행할 수 있다.
- 작업 중간 상태를 snapshot으로 저장할 수 있다.
- 복원된 VM에서 이어서 작업하는 UX를 만들 수 있다.
- profile을 통해 작업 유형별 LLM 설정을 분리할 수 있다.

### 아직 필요한 점

- IronClaw workspace와 VM snapshot의 lifecycle 연결
- 사용자별 quota와 권한 모델
- 네트워크 egress 제한
- audit log
- 장기 snapshot 보관 정책
- snapshot에서 secret을 어떻게 취급할지에 대한 정책

## 운영 리스크

### snapshot에는 민감 정보가 포함될 수 있다

snapshot metadata에는 agent token이 포함된다. 파일 권한은 제한되어 있지만, snapshot이 backup, export, 장기 보관될 경우 민감 정보 관리 정책이 필요하다.

### 모든 production 기능이 완성된 것은 아니다

0.2.0은 single-host feature complete에 가깝지만 multi-tenant production platform은 아니다.

아직 필요한 기능:

- 사용자별 리소스 제한
- 네트워크 정책
- 장기 보관 정책
- 비용 관리
- 운영 대시보드
- 장애 복구 절차

### 같은 snapshot을 동시에 여러 번 복원하는 시나리오는 주의가 필요하다

0.2.0은 서로 다른 snapshot의 병렬 복원은 테스트하지만, 같은 snapshot의 동시 복원은 아직 명시적으로 지원하지 않는다. 제품 UX에서 이 제약을 숨기거나 제한해야 한다.

## 릴리즈 판단

0.2.0은 내부 PoC나 단일 호스트 기반 실험 환경으로는 의미 있는 진전이다. 특히 snapshot/restore가 들어갔기 때문에 "작업 상태를 저장할 수 있는 AI execution workspace"라는 제품 방향을 설명하기 쉬워졌다.

하지만 외부 사용자에게 self-hosted production 제품으로 제공하려면 아직 다음이 필요하다.

- 보안 정책 문서화
- 운영 가이드
- snapshot 보관과 삭제 정책
- 장애 복구 시나리오
- 리소스 제한
- 관찰 가능성
- API 계약 안정화

## 0.3.0에 권장되는 제품 과제

우선순위가 높은 과제는 다음과 같다.

- snapshot lifecycle policy
- workspace와 snapshot의 연결 모델
- tenant별 quota
- egress 제한
- audit log
- API 문서와 예제
- MCP adapter와의 실제 통합 테스트
- restore 실패 시 사용자에게 보여줄 상태 모델

## 결론

ephemera 0.2.0은 0.1.0보다 훨씬 제품에 가까워졌다. 가장 큰 변화는 snapshot과 restore다. 이제 ephemera는 단순히 격리 VM을 띄우는 시스템이 아니라, AI 작업 세션의 상태를 저장하고 다시 불러올 수 있는 실행 기반으로 발전했다.

다만 0.2.0은 "단일 서버에서 기능이 갖춰진 단계"로 보는 것이 정확하다. multi-user production 운영을 위해서는 보안, 정책, 감사, quota, lifecycle 관리가 다음 단계에서 보강되어야 한다.
