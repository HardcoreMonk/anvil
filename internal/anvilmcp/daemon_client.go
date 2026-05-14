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
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"`
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
	AgentToken   string `json:"agent_token,omitempty"`
}

type SpawnVMRequest struct {
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	TenantID       string    `json:"tenant_id,omitempty"`
	Profile        string    `json:"profile,omitempty"`
	EgressPolicy   string    `json:"egress_policy,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type CreateSnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
}

type RestoreSnapshotRequest struct {
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type RestoreSnapshotResponse struct {
	VMID             string `json:"vm_id"`
	GuestIP          string `json:"guest_ip"`
	AgentURL         string `json:"agent_url"`
	Profile          string `json:"profile,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	EgressPolicy     string `json:"egress_policy,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id"`
}

type FlockAgentInfo struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

type FlockCreateRequest struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
}

type FlockCreateResponse struct {
	FlockID      string           `json:"flock_id"`
	Task         string           `json:"task"`
	TenantID     string           `json:"tenant_id,omitempty"`
	EgressPolicy string           `json:"egress_policy,omitempty"`
	Agents       []FlockAgentInfo `json:"agents"`
	TownWallURL  string           `json:"town_wall_url"`
	PostURL      string           `json:"post_url"`
}

type FlockInfo struct {
	FlockID      string                    `json:"flock_id"`
	Task         string                    `json:"task"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	EgressPolicy string                    `json:"egress_policy,omitempty"`
	Agents       map[string]FlockAgentInfo `json:"agents"`
	CreatedAt    time.Time                 `json:"created_at"`
}

type TownWallPostRequest struct {
	AgentID string `json:"agent_id"`
	Body    string `json:"body"`
}

type TownWallMessage struct {
	Timestamp time.Time `json:"timestamp"`
	AgentID   string    `json:"agent_id"`
	Body      string    `json:"body"`
}

type RawDaemonResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

type RuntimeAuditListResponse struct {
	Records []RuntimeAuditRecord `json:"records"`
}

type DaemonHealthResponse struct {
	Status        string `json:"status"`
	VMCount       int    `json:"vm_count"`
	SnapshotCount int    `json:"snapshot_count"`
	AuthEnabled   bool   `json:"auth_enabled"`
}

type TenantUpsertRequest struct {
	Quota TenantQuota `json:"quota"`
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

func (c *DaemonClient) SpawnVM(ctx context.Context, req SpawnVMRequest) (*SpawnVMResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/vms", req)
	if err != nil {
		return nil, err
	}

	var resp SpawnVMResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode spawn vm response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) ListVMs(ctx context.Context) ([]VMInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/vms", nil)
	if err != nil {
		return nil, err
	}
	var resp []VMInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode list vms response: %w", err)
	}
	return resp, nil
}

func (c *DaemonClient) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodPost, "/vms/"+vmID+"/tasks", map[string]string{"prompt": prompt})
}

func (c *DaemonClient) CopyIn(ctx context.Context, vmID, workspacePath, content string, overwrite bool) (*RawDaemonResponse, error) {
	query := url.Values{}
	query.Set("path", workspacePath)
	if overwrite {
		query.Set("overwrite", "true")
	}
	return c.rawBody(
		ctx,
		http.MethodPut,
		"/vms/"+vmID+"/workspace?"+query.Encode(),
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

func (c *DaemonClient) RestoreSnapshot(ctx context.Context, snapshotID string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/snapshots/"+snapshotID+"/restore", req)
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

func (c *DaemonClient) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/flocks", req)
	if err != nil {
		return nil, err
	}

	var resp FlockCreateResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode create flock response: %w", err)
	}
	if resp.Agents == nil {
		resp.Agents = []FlockAgentInfo{}
	}
	return &resp, nil
}

func (c *DaemonClient) ListFlocks(ctx context.Context) ([]FlockInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/flocks", nil)
	if err != nil {
		return nil, err
	}

	var resp []FlockInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode list flocks response: %w", err)
	}
	if resp == nil {
		resp = []FlockInfo{}
	}
	return resp, nil
}

func (c *DaemonClient) GetFlock(ctx context.Context, flockID string) (*FlockInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, flockPath(flockID), nil)
	if err != nil {
		return nil, err
	}

	var resp FlockInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode flock response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodDelete, flockPath(flockID), nil)
}

func (c *DaemonClient) PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	_, body, err := c.do(ctx, http.MethodPost, flockPath(flockID)+"/post", req)
	if err != nil {
		return nil, err
	}

	var resp TownWallMessage
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode town wall post response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	_, body, err := c.do(ctx, http.MethodGet, flockPath(flockID)+"/wall/history", nil)
	if err != nil {
		return nil, err
	}

	var resp []TownWallMessage
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode town wall history response: %w", err)
	}
	if resp == nil {
		resp = []TownWallMessage{}
	}
	return resp, nil
}

func (c *DaemonClient) DaemonHealth(ctx context.Context) (*DaemonHealthResponse, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return nil, err
	}
	var resp DaemonHealthResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode daemon health response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) Metrics(ctx context.Context) (string, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/metrics", nil)
	if err != nil {
		return "", err
	}
	return body, nil
}

func (c *DaemonClient) ListTenants(ctx context.Context) ([]TenantRecord, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/tenants", nil)
	if err != nil {
		return nil, err
	}
	var resp []TenantRecord
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode tenant list response: %w", err)
	}
	return resp, nil
}

func (c *DaemonClient) GetTenant(ctx context.Context, tenantID string) (*TenantRecord, error) {
	path, err := tenantPath(tenantID)
	if err != nil {
		return nil, err
	}
	_, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp TenantRecord
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode tenant response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) UpsertTenant(ctx context.Context, tenantID string, quota TenantQuota) (*TenantRecord, error) {
	path, err := tenantPath(tenantID)
	if err != nil {
		return nil, err
	}
	_, body, err := c.do(ctx, http.MethodPut, path, TenantUpsertRequest{Quota: quota})
	if err != nil {
		return nil, err
	}
	var resp TenantRecord
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode tenant upsert response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) ListRuntimeAudit(ctx context.Context, tenantID string, limit int) ([]RuntimeAuditRecord, error) {
	query := url.Values{}
	if strings.TrimSpace(tenantID) != "" {
		query.Set("tenant_id", strings.TrimSpace(tenantID))
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/audit/runtime"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	_, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp RuntimeAuditListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode runtime audit response: %w", err)
	}
	return resp.Records, nil
}

func (c *DaemonClient) PruneRuntimeAudit(ctx context.Context, policy RuntimeAuditRetention) ([]RuntimeAuditRecord, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/audit/runtime/prune", policy)
	if err != nil {
		return nil, err
	}
	var resp RuntimeAuditListResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode runtime audit prune response: %w", err)
	}
	return resp.Records, nil
}

func tenantPath(tenantID string) (string, error) {
	tenantID, err := NormalizeTenantID(tenantID)
	if err != nil {
		return "", err
	}
	return "/tenants/" + url.PathEscape(tenantID), nil
}

func flockPath(flockID string) string {
	return "/flocks/" + url.PathEscape(flockID)
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
