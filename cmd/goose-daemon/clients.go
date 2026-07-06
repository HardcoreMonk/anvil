package main

import (
	"fmt"
	"net/http"
	"time"
)

// clientView is the UI-facing view of a configured API client. The bearer Token
// is structurally absent from this struct — it never leaves the server (mirrors
// providerStatus, which exposes only an availability flag, never the API key).
type clientView struct {
	Name    string     `json:"name"`
	Expires *time.Time `json:"expires"` // nil = never expires
	Expired bool       `json:"expired"` // derived: Expires set and in the past
}

// handleConfigClients serves GET /config/clients — the configured API clients'
// name and token expiry only, for the System UI's client list. The bearer token
// value is NEVER included in the response.
func (cp *ControlPlane) handleConfigClients(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	clients := cp.getClients()
	out := make([]clientView, 0, len(clients))
	now := time.Now()
	for _, c := range clients {
		cv := clientView{Name: c.Name} // Token deliberately not copied.
		if !c.Expires.IsZero() {
			exp := c.Expires
			cv.Expires = &exp
			cv.Expired = now.After(exp)
		}
		out = append(out, cv)
	}
	writeJSON(w, http.StatusOK, out)
}
