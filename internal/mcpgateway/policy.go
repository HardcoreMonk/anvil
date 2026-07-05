package mcpgateway

// 학습 주석 개요: 이 파일은 gateway.go 요청 흐름의 2단계인 policy 확인을 구현한다.
// 핵심 불변식은 "profile policy 는 servers.yaml 이 이미 허용한 집합을 교집합으로
// 좁히기만 하고, 절대 넓히지 않는다"는 것이다(anvil boundary guard, v0.6.0).
// staticPolicyStore.For 를 보면 servers.yaml 의 `profiles:` 로 1차 allowed 집합을
// 만들고, bindingFor(EPHEMERA_MCP_SERVERS)가 있으면 그 집합과의 교집합만 남긴다 —
// bindingFor 가 allowed 에 없는 서버를 추가하는 코드 경로 자체가 없다.

// Policy is the resolved access policy for a caller profile: which backend
// servers the profile may use, and any per-tool allow/deny within each. Scoping
// the catalog per profile is what keeps each VM's per-request tool schema small
// (the token-budget control) — an agent only sees the tools its role needs.
type Policy struct {
	Servers map[string]bool       // server ids the profile may use
	Tools   map[string]ToolPolicy // per-tool filter, by server id (only for allowed servers)
}

// ToolPolicy is the per-tool filter for one backend server.
type ToolPolicy struct {
	Allow []string // if non-empty, only these tool names are permitted
	Deny  []string // tool names denied (ignored when Allow is set)
}

// Allows reports whether the profile may use the given backend server.
// 학습 주석: gateway.go 의 handleToolsList/handleResourcesList/handlePromptsList
// 가 카탈로그를 모을 때 이 함수로 서버 단위 1차 필터링을 한다.
func (p Policy) Allows(serverID string) bool {
	return p.Servers[serverID]
}

// AllowsTool reports whether the profile may use a specific tool on a server.
// Allow-list wins: a non-empty Allow permits only its members; otherwise Deny
// hides its members; with neither, every tool on an allowed server is permitted.
// A server the profile cannot use at all is always denied.
// 학습 주석: handleToolsCall 이 실제 호출 직전에 재확인하는 함수. 서버가 막히면
// tool 목록(Tools map)을 볼 필요도 없이 즉시 false — allow-list 우선순위가
// deny-list 보다 먼저 검사되는 순서에 주의.
func (p Policy) AllowsTool(serverID, tool string) bool {
	if !p.Servers[serverID] {
		return false
	}
	tp, ok := p.Tools[serverID]
	if !ok {
		return true
	}
	if len(tp.Allow) > 0 {
		return contains(tp.Allow, tool)
	}
	return !contains(tp.Deny, tool)
}

// PolicyStore resolves a caller profile to its Policy. The single-host store is
// derived from the per-server `profiles:` allow-lists in servers.yaml (optionally
// intersected with each profile's explicit binding); a multi-host fork can back
// this with a distributed policy service unchanged.
// 학습 주석: gateway.New 는 opts.Policy 가 nil 이면 NewStaticPolicyStore 로
// 기본값을 채운다(gateway.go 참고) — 즉 policy 를 명시하지 않으면 servers.yaml
// 이 허용하는 모든 서버가 그대로 허용된다(policy 미설정은 개방 기본값).
type PolicyStore interface {
	For(profile string) Policy
}

// staticPolicyStore derives policy from the loaded server configs: a server is
// available to a profile if the server's Profiles list contains the profile, or
// the list is empty (meaning "all profiles"). When bindingFor is set, the result
// is additionally intersected with the profile's explicit server binding, so a
// binding can only narrow access, never widen it beyond what servers.yaml grants.
// 학습 주석: bindingFor 가 nil 이면 servers.yaml 의 `profiles:` 만으로 access 가
// 결정된다 — EPHEMERA_MCP_SERVERS 바인딩은 optional 한 추가 축소 레이어일 뿐이다.
type staticPolicyStore struct {
	servers    []ServerConfig
	bindingFor func(profile string) ([]string, bool)
}

// NewStaticPolicyStore builds a PolicyStore from server configs, with no
// per-profile binding (servers.yaml `profiles:` is the only gate).
func NewStaticPolicyStore(servers []ServerConfig) PolicyStore {
	return staticPolicyStore{servers: servers}
}

// NewStaticPolicyStoreWithBinding builds a PolicyStore that intersects the
// servers.yaml result with each profile's explicit binding. bindingFor returns
// (servers, true) when the profile sets a binding (EPHEMERA_MCP_SERVERS), else
// (nil, false) meaning "no binding — use servers.yaml as-is".
func NewStaticPolicyStoreWithBinding(servers []ServerConfig, bindingFor func(profile string) ([]string, bool)) PolicyStore {
	return staticPolicyStore{servers: servers, bindingFor: bindingFor}
}

// 학습 주석: 교집합-축소가 코드로 드러나는 지점. allowed 는 servers.yaml 의
// `profiles:` 만으로 먼저 완성되고, bindingFor 블록은 그 집합에서 delete 만
// 수행한다(boundSet 에 없는 id 를 allowed 에 추가하는 코드가 없다) — policy 를
// 아무리 잘못 설정해도 servers.yaml 보다 넓은 접근을 만들 수 없는 구조.
func (s staticPolicyStore) For(profile string) Policy {
	allowed := map[string]bool{}
	tools := map[string]ToolPolicy{}
	for _, srv := range s.servers {
		if len(srv.Profiles) == 0 || contains(srv.Profiles, profile) {
			allowed[srv.ID] = true
			if len(srv.ToolsAllow) > 0 || len(srv.ToolsDeny) > 0 {
				tools[srv.ID] = ToolPolicy{Allow: srv.ToolsAllow, Deny: srv.ToolsDeny}
			}
		}
	}
	// Profile binding: intersect with the profile's explicit server list, if any.
	// This can only remove servers (narrow), never add (widen) — the intersection
	// is taken against the set servers.yaml already granted above.
	if s.bindingFor != nil {
		if bound, ok := s.bindingFor(profile); ok {
			boundSet := map[string]bool{}
			for _, id := range bound {
				boundSet[id] = true
			}
			for id := range allowed {
				if !boundSet[id] {
					delete(allowed, id)
					delete(tools, id)
				}
			}
		}
	}
	return Policy{Servers: allowed, Tools: tools}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
