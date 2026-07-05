package mcpgateway

// 학습 주석 개요: 이 파일은 gateway.go 요청 흐름의 첫 단계인 caller 신원 해석을
// 담당한다. v0.6.0 core 설계의 핵심 불변식은 "caller profile 은 request 의
// source IP 를 host 가 소유한 VM registry 와 대조해 server-side 로만 판정한다"는
// 것이다 — VM 이 스스로 자신의 profile 을 주장(요청 바디/헤더/세션)할 방법이 없다.
// registry 에 없는 source IP 는 곧 미등록 caller 이므로 gateway.ServeHTTP 에서
// 403 으로 거부된다. 이 구조 덕분에 세션(Mcp-Session-Id)은 신원의 근거가 아니라
// 이미 해석된 Caller 를 담는 그릇일 뿐이다(session.go 의 SessionStore 참고).

import (
	"fmt"
	"net"
	"net/http"
)

// Caller identifies the agent making a gateway request, resolved to its Ephemera
// profile so policy can scope the tool catalog. Profile drives which backend MCP
// servers (and thus tools) the caller may see.
type Caller struct {
	VMID    string
	Profile string
}

// 학습 주석: Caller 는 이미 신원 해석이 끝난 결과값이다. Profile 필드가 policy.go
// PolicyStore.For(profile) 의 입력이 되어 tool/resource/prompt 가시성을 좁힌다.

// CallerResolver maps an inbound gateway request to a Caller. The single-host
// implementation resolves by source IP via the VM registry (zero injection into
// the VM). A multi-host fork swaps this for a token- or overlay-based resolver
// without touching the gateway core or the VM-side contract.
// 학습 주석: gateway core(gateway.go)는 이 interface 만 알고, IP 기반 구현
// 세부사항(ipCallerResolver)에는 의존하지 않는다 — 교체 가능한 seam.
type CallerResolver interface {
	Resolve(r *http.Request) (Caller, error)
}

// VMLookup resolves a VM's guest IP to its id and Ephemera profile. The daemon
// supplies this as a closure over its running-VM registry, keeping mcpgateway
// decoupled from the control-plane types.
// 학습 주석: 실제 구현은 cmd/goose-daemon/mcp_gateway.go 의 lookupVMByIP —
// cp.vms 를 순회해 GuestIP 일치 여부로 VMID/Profile 을 돌려준다(RLock 스냅샷).
type VMLookup func(ip string) (vmID, profile string, ok bool)

// ipCallerResolver identifies a caller by the source IP of the request, looked up
// in the VM registry. On the single host every VM has a unique, daemon-assigned
// 10.0.1.x address on the trusted bridge, so the source IP is a sound identity.
// 학습 주석: "신뢰"의 근거는 daemon 이 그 10.0.1.x 주소를 직접 할당했다는 점과,
// internal/network/antispoof.go 의 ebtables 규칙이 그 주소를 스푸핑하지 못하게
// TAP 포트에 MAC+IP 를 고정한다는 점이다(defense-in-depth, best-effort).
type ipCallerResolver struct {
	lookup VMLookup
}

// NewIPCallerResolver returns a CallerResolver that maps request source IPs to
// VMs via the supplied lookup.
// 학습 주석: cmd/goose-daemon/mcp_gateway.go 의 initMCPGateway 가 이 생성자를
// cp.lookupVMByIP 와 함께 호출해 Options.Resolver 를 채운다.
func NewIPCallerResolver(lookup VMLookup) CallerResolver {
	return ipCallerResolver{lookup: lookup}
}

// 학습 주석: 이 함수가 identity.go 의 유일한 신원 판정 지점이다. r.lookup 이
// false 를 돌려주면(등록 안 된 IP) gateway.ServeHTTP 는 403 으로 응답하고 이후
// policy/rate/backend 단계로 전혀 진행하지 않는다.
func (r ipCallerResolver) Resolve(req *http.Request) (Caller, error) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr // RemoteAddr without a port (rare); use as-is
	}
	vmID, profile, ok := r.lookup(host)
	if !ok {
		return Caller{}, fmt.Errorf("unknown caller %s", host)
	}
	return Caller{VMID: vmID, Profile: profile}, nil
}
