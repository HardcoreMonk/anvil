package mcpgateway

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
func (p Policy) Allows(serverID string) bool {
	return p.Servers[serverID]
}

// AllowsTool reports whether the profile may use a specific tool on a server.
// Allow-list wins: a non-empty Allow permits only its members; otherwise Deny
// hides its members; with neither, every tool on an allowed server is permitted.
// A server the profile cannot use at all is always denied.
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
type PolicyStore interface {
	For(profile string) Policy
}

// staticPolicyStore derives policy from the loaded server configs: a server is
// available to a profile if the server's Profiles list contains the profile, or
// the list is empty (meaning "all profiles"). When bindingFor is set, the result
// is additionally intersected with the profile's explicit server binding, so a
// binding can only narrow access, never widen it beyond what servers.yaml grants.
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
