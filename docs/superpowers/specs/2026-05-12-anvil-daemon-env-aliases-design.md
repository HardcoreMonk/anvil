# anvil daemon 환경 변수 alias 설계

작성일: 2026-05-12

## 목적

anvil 운영자가 ephemera 기반 runtime을 설정할 때 `ANVIL_*` 환경 변수 이름을
사용할 수 있게 한다. 기존 `EPHEMERA_*` 환경 변수는 runtime의 canonical 계약으로
유지하며, 새 alias는 호환성을 깨지 않는 편의 계층으로만 동작한다.

이 설계는 README에서 정리한 프로젝트 경계를 따른다. `anvil`은 IronClaw 통합
프로젝트이고, `ephemera`는 Firecracker MicroVM 기반 실행 runtime이다. 따라서
runtime 내부 구현과 기존 배포는 ephemera 이름을 계속 인식해야 한다.

## 범위

포함:

- ephemera daemon 설정에 대응되는 `ANVIL_*` alias 추가
- alias-aware 환경 변수 lookup helper 추가
- precedence 규칙 테스트
- README, service logic, context 문서 갱신

제외:

- 기존 `EPHEMERA_*` 환경 변수 제거 또는 rename
- `cmd/anvil-mcp` 설정 변경
- HTTP API route 변경
- config file format 변경
- runtime artifact 경로 변경
- release version bump

## Alias Mapping

| Canonical 변수 | Alias 변수 | 의미 |
|---|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` | control plane bind 주소 |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` | `EPHEMERA_API_ADDR`/`ANVIL_API_ADDR`가 없을 때 사용할 port |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` | named Bearer token 목록 |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` | 단일 Bearer token fallback |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` | VM 내부 `goose-agent` listen port |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` | 외부에서 접근 가능한 control plane base URL |

## Precedence

각 설정 값은 다음 순서로 결정한다.

1. Canonical `EPHEMERA_*` 값이 있으면 사용한다.
2. Canonical 값이 없고 대응되는 `ANVIL_*` alias 값이 있으면 사용한다.
3. 둘 다 없으면 기존 default 또는 empty 값을 사용한다.

예시:

| 환경 변수 상태 | 결과 |
|---|---|
| `EPHEMERA_API_ADDR=127.0.0.1:3000`, `ANVIL_API_ADDR=0.0.0.0:3000` | `127.0.0.1:3000` |
| `ANVIL_API_ADDR=0.0.0.0:3000`만 설정 | `0.0.0.0:3000` |
| 둘 다 없음 | 기존 default `127.0.0.1:3000` |

이 순서는 기존 ephemera 배포를 보호한다. 새 anvil alias를 도입해도 이미
`EPHEMERA_*`로 운영 중인 환경의 동작은 바뀌지 않는다.

## Config 동작

`cmd/goose-daemon/config.go`에 alias-aware helper를 추가한다.

개념:

```go
func envWithAlias(canonicalKey, aliasKey string) string
```

동작:

- `os.Getenv(canonicalKey)`가 비어 있지 않으면 해당 값을 반환한다.
- canonical 값이 비어 있고 `aliasKey`가 비어 있지 않으면 `os.Getenv(aliasKey)`를 반환한다.
- 둘 다 비어 있으면 빈 문자열을 반환한다.

정수 설정은 기존 `envInt`를 alias-aware 형태로 확장한다.

개념:

```go
func envIntWithAlias(canonicalKey, aliasKey string, defaultVal int) int
```

동작:

- `envWithAlias`로 raw 값을 읽는다.
- raw 값이 양의 정수이면 사용한다.
- raw 값이 비어 있거나 양의 정수가 아니면 기존 동작처럼 default를 사용한다.

## 적용 지점

`cmd/goose-daemon/config.go`에서 다음 지점을 alias-aware lookup으로 바꾼다.

| 설정 | 현재 lookup | 변경 후 lookup |
|---|---|---|
| `agentPort` | `envInt("EPHEMERA_AGENT_PORT", defaultAgentPort)` | `envIntWithAlias("EPHEMERA_AGENT_PORT", "ANVIL_AGENT_PORT", defaultAgentPort)` |
| `apiAddr` | `resolveAPIAddr()` 내부 `EPHEMERA_API_ADDR`, `EPHEMERA_API_PORT` | canonical 우선, alias fallback |
| `publicURL` | `os.Getenv("EPHEMERA_PUBLIC_URL")` | `envWithAlias("EPHEMERA_PUBLIC_URL", "ANVIL_PUBLIC_URL")` |
| multi-client token | `EPHEMERA_API_TOKENS` | `EPHEMERA_API_TOKENS`, fallback `ANVIL_API_TOKENS` |
| single token | `EPHEMERA_API_TOKEN` | `EPHEMERA_API_TOKEN`, fallback `ANVIL_API_TOKEN` |

## Token Precedence

Token 설정은 기존 multi-client 우선 규칙을 유지한다.

1. `EPHEMERA_API_TOKENS`
2. `ANVIL_API_TOKENS`
3. `EPHEMERA_API_TOKEN`
4. `ANVIL_API_TOKEN`
5. token 없음, 인증 비활성화

`EPHEMERA_API_TOKENS`와 `ANVIL_API_TOKEN`이 함께 있으면 multi-client canonical
값을 사용한다. `ANVIL_API_TOKENS`와 `EPHEMERA_API_TOKEN`이 함께 있으면
multi-client alias가 single-token canonical보다 우선한다. 이 선택은 기존
함수의 “multi-client 설정이 single-token fallback보다 우선”이라는 의미를
alias까지 확장한 것이다.

## 오류 처리

기존 daemon config는 잘못된 정수 값을 error로 반환하지 않고 default를 사용한다.
이번 alias도 같은 정책을 유지한다.

- `ANVIL_API_PORT=abc`이면 default port를 사용한다.
- `EPHEMERA_API_PORT=abc`와 `ANVIL_API_PORT=4000`이 함께 있으면 canonical 값이
  설정된 것으로 보고 default port를 사용한다. alias fallback은 canonical 값이
  비어 있을 때만 적용한다.
- malformed `ANVIL_API_TOKENS` entry는 기존 `EPHEMERA_API_TOKENS`처럼 skip한다.

## 문서 갱신

README:

- anvil 운영자는 `ANVIL_*` alias를 사용할 수 있음을 설정 섹션에 추가한다.
- `EPHEMERA_*`가 canonical runtime 변수이며 우선순위가 더 높다는 점을 명시한다.

`docs/architecture/service-logic.md`:

- 제어 평면 인증 설명에 `ANVIL_API_TOKENS`, `ANVIL_API_TOKEN` fallback을 추가한다.
- 설정 precedence를 runtime invariant로 명시한다.

`CONTEXT.md`:

- 고정 런타임 계약에 alias 관계를 추가한다.
- `EPHEMERA_*` 제거가 아니라 `ANVIL_*` fallback 지원임을 명시한다.

## 테스트 전략

`cmd/goose-daemon/config_test.go` 또는 기존 config test에 다음 unit test를 추가한다.

- `ANVIL_API_ADDR`만 설정하면 `resolveAPIAddr()`가 alias 값을 사용한다.
- `EPHEMERA_API_ADDR`와 `ANVIL_API_ADDR`가 함께 있으면 canonical 값을 사용한다.
- `ANVIL_API_PORT`만 설정하면 default bind host `127.0.0.1`과 alias port를 사용한다.
- `EPHEMERA_API_PORT`와 `ANVIL_API_PORT`가 함께 있으면 canonical port를 사용한다.
- `ANVIL_PUBLIC_URL`만 설정하면 trailing slash가 제거된다.
- `EPHEMERA_PUBLIC_URL`와 `ANVIL_PUBLIC_URL`가 함께 있으면 canonical 값을 사용한다.
- `ANVIL_API_TOKENS`만 설정하면 named client 목록을 만든다.
- `EPHEMERA_API_TOKENS`와 `ANVIL_API_TOKENS`가 함께 있으면 canonical multi-client 값을 사용한다.
- `ANVIL_API_TOKEN`만 설정하면 default client를 만든다.
- `ANVIL_API_TOKENS`와 `EPHEMERA_API_TOKEN`이 함께 있으면 alias multi-client가 single-token canonical보다 우선한다.
- `ANVIL_AGENT_PORT`만 설정하면 agent port alias 값을 사용한다.

검증 명령:

```bash
go test ./cmd/goose-daemon -count=1
go test ./... -count=1
```

## 수용 기준

- 기존 `EPHEMERA_*` 설정만 사용하는 환경의 동작이 바뀌지 않는다.
- 신규 `ANVIL_*` alias만 사용하는 환경에서 daemon 설정이 적용된다.
- canonical과 alias가 동시에 설정되면 canonical 값이 우선한다.
- token 설정은 multi-client 우선 규칙을 유지한다.
- README와 architecture/context 문서가 alias와 precedence를 설명한다.
- `go test ./... -count=1`이 통과한다.
