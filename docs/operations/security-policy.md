# anvil 보안 정책

## 공개 노출

`goose-daemon`은 TLS를 종료하는 reverse proxy 뒤에서만 공개 운영한다. 운영 환경은
`EPHEMERA_API_TOKENS`를 설정해야 하며, control-plane token이 없는 인증 비활성
로컬 전용 모드는 공개 노출 용도가 아니다.

## 제어 평면 token 정책

control-plane token의 기준 설정은 `EPHEMERA_API_TOKENS`다. 호환 alias로
`ANVIL_API_TOKENS`, `EPHEMERA_API_TOKEN`, `ANVIL_API_TOKEN`을 인식한다. 실제 token
값은 문서, 채팅, 커밋 메시지, 테스트 fixture에 남기지 않는다.

## 게스트 agent token 정책

guest agent token은 VM마다 생성된다. daemon은 이 token을 guest disk에 주입하고,
control plane proxy가 guest agent 호출에 내부적으로 사용한다. 외부 client는 guest
agent token이 아니라 control-plane token으로 daemon에 인증한다. 보안 불변 조건상
`agent_token`은 `POST /vms` 응답 외에는 노출하지 않아야 한다. 현재 daemon의 restore
응답은 기존 호환성 때문에 `agent_token`을 포함할 수 있지만, 이는 제거 대상 구현
부채다. MCP output은 restore `agent_token`을 노출하지 않는다.

## 로컬 secret

`configs/goose-secrets.yaml`과 profile별 secrets 파일은 local secret이다. 이 파일은
커밋하지 않는다.

## Snapshot metadata 반출 정책

snapshot `metadata.json`에는 `agent_token`이 들어 있다. metadata 반출 또는 백업
산출물이 신뢰된 host 경계 밖으로 나가기 전에는 아래 scrubber로 token을 제거한다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-.../metadata.json > metadata.scrubbed.json
```

snapshot GC audit은 metadata 전체나 `agent_token`을 기록하지 않는다.

## 제한 사항

이미 생성된 snapshot의 token 회전은 아직 구현되어 있지 않다.
