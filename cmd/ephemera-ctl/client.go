package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client is a thin HTTP wrapper around the Ephemera control-plane REST API.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// No global timeout: /tasks can run for minutes. Per-call context could be
		// added later; operator commands are interactive and Ctrl-C-able.
		http: &http.Client{},
	}
}

// do performs a request and returns the raw response body. A non-2xx status is
// returned as an error carrying the server's JSON error payload. body is
// JSON-encoded when non-nil; the Bearer header is set only when a token is
// configured (mirrors the in-VM gtcall/gtwall convention).
func (c *client) do(method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s → HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// ── Wire types (local mirrors of the daemon's JSON; can't import package main) ──

type vmInfo struct {
	VMID     string `json:"vm_id"`
	GuestIP  string `json:"guest_ip"`
	AgentURL string `json:"agent_url"`
	Profile  string `json:"profile,omitempty"`
}

type vmSpawnResult struct {
	vmInfo
	AgentToken string `json:"agent_token"`
}

type snapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type vmRestoreResult struct {
	vmSpawnResult
	SourceSnapshotID string `json:"source_snapshot_id"`
}

type agentInfo struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

type flockCreateResponse struct {
	FlockID     string            `json:"flock_id"`
	Task        string            `json:"task"`
	Agents      []agentInfo       `json:"agents"`
	AgentTokens map[string]string `json:"agent_tokens"`
	TownWallURL string            `json:"townwall_url"`
	PostURL     string            `json:"post_url"`
}

// flock is the GET /flocks (list) and GET /flocks/{id} shape — note agents is a
// map keyed by agent_id here (POST /flocks returns an array instead).
type flock struct {
	FlockID   string                `json:"flock_id"`
	Task      string                `json:"task"`
	Agents    map[string]*agentInfo `json:"agents"`
	CreatedAt time.Time             `json:"created_at"`
}

type townWallMessage struct {
	Seq       uint64 `json:"seq"`
	Timestamp string `json:"timestamp"`
	AgentID   string `json:"agent_id"`
	Body      string `json:"body"`
}

type auditRecord struct {
	TS         string `json:"ts"`
	Client     string `json:"client"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	RemoteAddr string `json:"remote_addr"`
	Bytes      int64  `json:"bytes"`
}
