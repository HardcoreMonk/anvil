package mcpgateway

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

// CallerResolver maps an inbound gateway request to a Caller. The single-host
// implementation resolves by source IP via the VM registry (zero injection into
// the VM). A multi-host fork swaps this for a token- or overlay-based resolver
// without touching the gateway core or the VM-side contract.
type CallerResolver interface {
	Resolve(r *http.Request) (Caller, error)
}

// VMLookup resolves a VM's guest IP to its id and Ephemera profile. The daemon
// supplies this as a closure over its running-VM registry, keeping mcpgateway
// decoupled from the control-plane types.
type VMLookup func(ip string) (vmID, profile string, ok bool)

// ipCallerResolver identifies a caller by the source IP of the request, looked up
// in the VM registry. On the single host every VM has a unique, daemon-assigned
// 10.0.1.x address on the trusted bridge, so the source IP is a sound identity.
type ipCallerResolver struct {
	lookup VMLookup
}

// NewIPCallerResolver returns a CallerResolver that maps request source IPs to
// VMs via the supplied lookup.
func NewIPCallerResolver(lookup VMLookup) CallerResolver {
	return ipCallerResolver{lookup: lookup}
}

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
