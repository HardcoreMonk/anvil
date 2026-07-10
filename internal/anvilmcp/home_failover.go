package anvilmcp

import (
	"errors"
	"net"
)

// isDialError reports whether err is a dial-phase transport failure (the
// connection was never established). Mirrors the daemon-side relay-retry
// classification (cmd/goose-daemon/relay_retry.go): only dial-class failures
// mark a host as down for reconcile probes and home-failover detection —
// HTTP responses, reset/EOF, and ctx cancellation never do.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}
