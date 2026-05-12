package anvilmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type DaemonClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type DaemonError struct {
	StatusCode int
	Body       string
}

func (e *DaemonError) Error() string {
	return fmt.Sprintf("daemon returned status %d: %s", e.StatusCode, e.Body)
}

type SpawnVMResponse struct {
	VMID       string `json:"vm_id"`
	GuestIP    string `json:"guest_ip"`
	AgentURL   string `json:"agent_url"`
	Profile    string `json:"profile,omitempty"`
	AgentToken string `json:"agent_token,omitempty"`
}

type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type CreateSnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"`
}

type RestoreSnapshotResponse struct {
	VMID             string `json:"vm_id"`
	GuestIP          string `json:"guest_ip"`
	AgentURL         string `json:"agent_url"`
	Profile          string `json:"profile,omitempty"`
	AgentToken       string `json:"agent_token,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id"`
}

type RawDaemonResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func NewDaemonClient(cfg Config, httpClient *http.Client) *DaemonClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &DaemonClient{
		baseURL: cfg.DaemonURL,
		token:   cfg.APIToken,
		http:    httpClient,
	}
}

func (c *DaemonClient) SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error) {
	payload := map[string]string{}
	if profile != "" {
		payload["profile"] = profile
	}

	_, body, err := c.do(ctx, http.MethodPost, "/vms", payload)
	if err != nil {
		return nil, err
	}

	var resp SpawnVMResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode spawn vm response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodPost, "/vms/"+vmID+"/tasks", map[string]string{"prompt": prompt})
}

func (c *DaemonClient) CopyIn(ctx context.Context, vmID, workspacePath, content string) (*RawDaemonResponse, error) {
	return c.rawBody(
		ctx,
		http.MethodPut,
		"/vms/"+vmID+"/workspace?path="+url.QueryEscape(workspacePath),
		strings.NewReader(content),
		"application/octet-stream",
	)
}

func (c *DaemonClient) CopyOut(ctx context.Context, vmID, workspacePath string) (string, error) {
	_, body, err := c.doRaw(ctx, http.MethodGet, "/vms/"+vmID+"/workspace?path="+url.QueryEscape(workspacePath), nil, "")
	if err != nil {
		return "", err
	}
	return body, nil
}

func (c *DaemonClient) Health(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodGet, "/vms/"+vmID+"/health", nil)
}

func (c *DaemonClient) Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodPost, "/vms/"+vmID+"/stop", nil)
}

func (c *DaemonClient) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodDelete, "/vms/"+vmID, nil)
}

func (c *DaemonClient) CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/vms/"+vmID+"/snapshot", req)
	if err != nil {
		return nil, err
	}

	var resp SnapshotInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode create snapshot response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/snapshots", nil)
	if err != nil {
		return nil, err
	}

	var resp []SnapshotInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode list snapshots response: %w", err)
	}
	return resp, nil
}

func (c *DaemonClient) RestoreSnapshot(ctx context.Context, snapshotID string) (*RestoreSnapshotResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/snapshots/"+snapshotID+"/restore", nil)
	if err != nil {
		return nil, err
	}

	var resp RestoreSnapshotResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode restore snapshot response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodDelete, "/snapshots/"+snapshotID, nil)
}

func (c *DaemonClient) raw(ctx context.Context, method, path string, payload any) (*RawDaemonResponse, error) {
	statusCode, body, err := c.do(ctx, method, path, payload)
	if err != nil {
		return nil, err
	}
	return &RawDaemonResponse{
		StatusCode: statusCode,
		Body:       body,
	}, nil
}

func (c *DaemonClient) rawBody(ctx context.Context, method, path string, body io.Reader, contentType string) (*RawDaemonResponse, error) {
	statusCode, responseBody, err := c.doRaw(ctx, method, path, body, contentType)
	if err != nil {
		return nil, err
	}
	return &RawDaemonResponse{
		StatusCode: statusCode,
		Body:       responseBody,
	}, nil
}

func (c *DaemonClient) do(ctx context.Context, method, path string, payload any) (int, string, error) {
	var body io.Reader
	hasBody := payload != nil
	if hasBody {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, "", fmt.Errorf("encode daemon request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, "", fmt.Errorf("create daemon request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.send(req)
}

func (c *DaemonClient) doRaw(ctx context.Context, method, path string, body io.Reader, contentType string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, "", fmt.Errorf("create daemon request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return c.send(req)
}

func (c *DaemonClient) send(req *http.Request) (int, string, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("send daemon request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("read daemon response: %w", err)
	}
	responseBody := string(data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", &DaemonError{
			StatusCode: resp.StatusCode,
			Body:       responseBody,
		}
	}
	return resp.StatusCode, responseBody, nil
}
