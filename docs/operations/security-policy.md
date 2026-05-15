# anvil 보안 정책

## 공개 노출

`goose-daemon`은 TLS를 종료하는 reverse proxy 뒤에서만 공개 운영한다. daemon
자체는 기본적으로 HTTP control plane을 제공하므로, 인터넷 또는 팀 공용 network에
직접 bind하지 않는다.

운영 배포의 외부 경계는 다음 구조를 기준으로 한다.

```text
client
  -> HTTPS reverse proxy
  -> HTTP 127.0.0.1:3000 또는 private host network의 goose-daemon
```

reverse proxy는 TLS 인증서, 외부 access log, allowlist/rate limit 같은 공개 노출
정책을 담당한다. daemon은 VM lifecycle, snapshot lifecycle, guest agent proxy,
control-plane Bearer token 검증을 담당한다.

운영 환경은 `EPHEMERA_API_TOKENS`를 설정해야 한다. control-plane token이 없는 인증
비활성 로컬 전용 모드는 개발과 host-local smoke test 용도이며 공개 노출 용도가
아니다.

## 제어 평면 token 정책

control-plane token의 기준 설정은 `EPHEMERA_API_TOKENS`다. 호환 alias로
`ANVIL_API_TOKENS`, `EPHEMERA_API_TOKEN`, `ANVIL_API_TOKEN`을 인식한다. 실제 token
값은 문서, 채팅, 커밋 메시지, 테스트 fixture, release note에 남기지 않는다.

운영에서는 client 이름이 있는 다중 token 형식을 우선한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN,ci:$CI_TOKEN" ./anvil-daemon
```

단일 token alias는 기존 배포 호환을 위한 경로다. 새 운영 설정은
`EPHEMERA_API_TOKENS`를 사용한다.

## 게스트 agent token 정책

guest agent token은 VM마다 생성된다. daemon은 이 token을 guest disk에 주입하고,
control plane proxy가 guest agent 호출에 내부적으로 사용한다. 외부 client는 guest
agent token이 아니라 control-plane token으로 daemon에 인증한다. 보안 불변 조건상
`agent_token`은 `POST /vms` 응답 외에는 노출하지 않아야 한다.

다음 출력에는 정책상 `agent_token`이 나오면 안 된다.

- snapshot 생성, 목록, restore, delete 응답
- snapshot GC dry-run/apply 응답
- `snapshots/gc-audit.jsonl`
- MCP tool output
- 문서, audit log, test fixture

daemon의 restore 응답과 MCP output은 `agent_token`을 노출하지 않는다. 운영자가
과거 로그나 오래된 test fixture를 공유해야 할 때는 legacy restore body에 token이
남아 있지 않은지 확인한다.

## 로컬 secret

`configs/goose-secrets.yaml`과 profile별 secrets 파일은 local secret이다. 예시는
커밋할 수 있지만 실제 secret 파일은 커밋하지 않는다.

커밋 금지 대상:

- `configs/goose-secrets.yaml`
- `configs/profiles/*/goose-secrets.yaml`
- 실제 LLM API key 또는 provider token이 들어간 임시 fixture
- 실제 token 값을 포함한 shell history, terminal transcript, release note, issue,
  PR description, commit message

운영 절차 문서에는 `$TOKEN`, `$CI_TOKEN`, `$SNAPSHOT_ID`, `$VM_ID` 같은 placeholder만
사용한다.

## Snapshot metadata 반출 정책

snapshot `metadata.json`에는 `agent_token`이 들어 있다. metadata 반출 또는 백업
산출물이 신뢰된 host 경계 밖으로 나가기 전에는 반드시 scrubber로 token을 제거한다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-.../metadata.json > metadata.scrubbed.json
```

신뢰된 host 경계 밖에는 off-host backup, support bundle, object storage, 외부 ticket,
채팅 업로드, release artifact가 포함된다. 원본 snapshot directory 전체를 외부로
복사해야 하는 운영 절차는 아직 승인된 표준 절차가 아니다. 필요한 경우 먼저
`metadata.json`을 scrub한 별도 산출물을 만들고, 원본 metadata가 포함되지 않았는지
검사한다.

snapshot GC audit은 metadata 전체나 `agent_token`을 기록하지 않는다.

## Town Wall message 정책

Goosetown Town Wall은 flock별 coordination log다. `POST /flocks/{flock_id}/post`,
`GET /flocks/{flock_id}/wall/history`, SSE stream, VM 내부 `gtwall` helper가 같은
append-only log를 사용한다.

Town Wall message body는 사용자/agent가 제공한 내용이며
`flocks/<flock_id>/TOWN_WALL.log`와 history 응답에 남는다. 따라서 provider API key,
Bearer token, `agent_token`, 내부 credential, 고객 PII를 게시하지 않는다.

MCP runtime audit은 `anvil_post_townwall` 호출 사실과 daemon operation만 기록하고
Town Wall body는 저장하지 않는다. 하지만 Town Wall 자체는 body를 보존하므로 audit
redaction을 secret 저장소로 오해하지 않는다.

## Egress policy

`egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나다. daemon은 선택된
policy를 VM/snapshot/restore metadata에 보존하고, host-local network rule 적용에
사용한다.

- `deny_all`: guest IP 기준 `iptables FORWARD` reject rule을 적용한다.
- `profile`: `configs/profiles/{profile}/egress.json`,
  `EPHEMERA_EGRESS_PROFILE_DIR`, `ANVIL_EGRESS_PROFILE_DIR` 아래의 profile별
  `egress.json`이 있으면 allow CIDR, allow host string match, DNS server allowlist와
  default reject rule을 적용한다.
- `allow_all`: 기존 NAT outbound 동작을 유지한다.

`egress.json`은 secret 저장소가 아니다. provider API key, Bearer token, 내부
credential을 넣지 않는다. `allow_hosts` rule은 packet string match 기반의 coarse
host allowlist이며, L7 proxy 또는 SNI gateway를 대체하지 않는다. policy 파일이 없는
`profile`은 기존 profile 호환성을 위해 no-op이다.

## Audit, metrics, trace redaction

runtime audit API, snapshot GC audit, `/metrics`, `/metrics/vms`, optional trace
export는 daemon raw body, snapshot metadata 전체, secret, `agent_token`을 기록하지
않는다. trace exporter는 attribute key/value에서 token, secret, authorization 계열
값을 제거한 뒤 `{endpoint}/v1/traces`로 전송한다.

## 운영 점검 기준

- 공개 endpoint는 TLS reverse proxy 뒤에 있는가.
- 운영 daemon에 `EPHEMERA_API_TOKENS`가 설정되어 있는가.
- `POST /vms` 외 응답과 MCP output에 `agent_token`이 없는가.
- `deny_all` 또는 `profile` egress policy를 쓰는 profile의 `egress.json`이 의도한
  CIDR, host, DNS server만 허용하는가.
- runtime audit, metrics, trace export에 token/secret/metadata raw body가 없는가.
- Town Wall message에 token, provider secret, 고객 PII가 들어가지 않았는가.
- snapshot metadata를 host 밖으로 내보내기 전에 scrub했는가.
- `configs/goose-secrets.yaml`과 profile secrets가 git에 들어가지 않았는가.

## 제한 사항

이미 생성된 snapshot의 token 회전은 아직 구현되어 있지 않다.
