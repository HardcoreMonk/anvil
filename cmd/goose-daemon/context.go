package main

import "context"

// ctxKey is a private type for the context keys defined in this package, so the
// values stored here cannot collide with keys set by any other package.
type ctxKey int

const (
	clientNameKey ctxKey = iota
	clientHolderKey
)

// withClientName returns a context carrying the authenticated caller's client
// name. Handlers read it with clientNameFromContext (v0.4.1).
func withClientName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, clientNameKey, name)
}

// clientNameFromContext returns the authenticated client name, or "" when the
// request was unauthenticated (auth disabled, /metrics, or a rejected request).
func clientNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(clientNameKey).(string)
	return name
}

// clientHolder is a request-scoped mailbox the audit middleware installs before
// authentication runs, letting the OUTER audit middleware read the client name
// the INNER auth middleware resolves (a request's context mutation via
// WithContext is not visible to an outer wrapper, so a shared pointer bridges
// the two). It is written and read within a single request goroutine, so it
// needs no synchronization.
type clientHolder struct {
	name string
}

func withClientHolder(ctx context.Context, h *clientHolder) context.Context {
	return context.WithValue(ctx, clientHolderKey, h)
}

func clientHolderFromContext(ctx context.Context) *clientHolder {
	h, _ := ctx.Value(clientHolderKey).(*clientHolder)
	return h
}
