package mcpgateway

// 학습 주석 개요: 이 파일은 HTTP backend 전용 credential 주입 seam이다. stdio
// backend 의 credential 주입 경로(child 환경변수, credential_env)는 여기 없고
// backend_stdio.go/registry.go 에 있다 — HTTP 는 Authorization 헤더, stdio 는
// 프로세스 환경변수로 서로 다른 물리적 채널을 쓰기 때문에 seam 이 분리돼 있다.
// 두 경로 모두 공통점은 같다: credential 원본은 host 의 secrets.yaml 에만 있고
// VM 에는 어떤 형태로도 전달되지 않는다.

import "net/http"

// CredentialProvider injects a backend's authentication into an outbound request
// just before the gateway forwards a call to it. Credentials live only on the
// host (configs/mcp/secrets.yaml) and are never sent to a VM. A multi-host fork
// can back this with a central vault behind the same interface.
// 학습 주석: HTTPBackend.roundTrip(backend.go)이 매 요청 직전에 Inject 를
// 호출한다 — credential 은 gateway core(gateway.go)를 절대 거치지 않는다.
type CredentialProvider interface {
	// Inject adds the backend's auth header(s) to h. A backend with no configured
	// credential is a no-op (the remote server is unauthenticated or public).
	Inject(serverID string, h http.Header)
}

// mapCredentialProvider injects "Authorization: Bearer <token>" for each server
// that has a token. Built from servers.yaml (server → credential key) plus
// secrets.yaml (credential key → token).
// 학습 주석: tokens 는 NewRegistry 가 조립한 httpTokens(server id -> token)를
// 그대로 받는다 — servers.yaml 의 credential 키 이름은 여기까지 오지 않는다.
type mapCredentialProvider struct {
	// tokens maps server id → bearer token.
	tokens map[string]string
}

// NewMapCredentialProvider builds a provider from a server-id→token map.
func NewMapCredentialProvider(tokens map[string]string) CredentialProvider {
	return mapCredentialProvider{tokens: tokens}
}

// 학습 주석: credential 이 없는 서버(공개 backend)는 그냥 아무 헤더도 추가하지
// 않는다 — "무credential = 무인증 backend"를 명시적 분기 없이 자연스럽게 표현.
func (m mapCredentialProvider) Inject(serverID string, h http.Header) {
	if tok := m.tokens[serverID]; tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
}
