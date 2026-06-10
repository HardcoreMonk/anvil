package mcpgateway

// Policy is the resolved tool-access policy for a caller profile: the set of
// backend server ids whose tools the profile may see. Scoping the catalog per
// profile is what keeps each VM's per-request tool schema small (the token-budget
// control) — an agent only sees the tools its role needs.
type Policy struct {
	Servers map[string]bool // server ids the profile may use
}

// Allows reports whether the profile may use the given backend server.
func (p Policy) Allows(serverID string) bool {
	return p.Servers[serverID]
}

// PolicyStore resolves a caller profile to its Policy. The single-host store is
// derived from the per-server `profiles:` allow-lists in servers.yaml; a
// multi-host fork can back this with a distributed policy service unchanged.
type PolicyStore interface {
	For(profile string) Policy
}

// staticPolicyStore derives policy from the loaded server configs: a server is
// available to a profile if the server's Profiles list contains the profile, or
// the list is empty (meaning "all profiles").
type staticPolicyStore struct {
	servers []ServerConfig
}

// NewStaticPolicyStore builds a PolicyStore from server configs.
func NewStaticPolicyStore(servers []ServerConfig) PolicyStore {
	return staticPolicyStore{servers: servers}
}

func (s staticPolicyStore) For(profile string) Policy {
	allowed := map[string]bool{}
	for _, srv := range s.servers {
		if len(srv.Profiles) == 0 || contains(srv.Profiles, profile) {
			allowed[srv.ID] = true
		}
	}
	return Policy{Servers: allowed}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
