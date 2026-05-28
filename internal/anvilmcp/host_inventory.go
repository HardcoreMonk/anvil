package anvilmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultHostInventoryTimeout = 3 * time.Second

type HostInventoryOptions struct {
	HTTPClient *http.Client
	APIToken   string
	Timeout    time.Duration
}

type HostInventory struct {
	mu      sync.RWMutex
	hosts   []RuntimeHost
	token   string
	timeout time.Duration
	http    *http.Client
}

type hostHealthResponse struct {
	Status                 string          `json:"status"`
	AvailableVMs           *int64          `json:"available_vms,omitempty"`
	AvailableSnapshotBytes *int64          `json:"available_snapshot_bytes,omitempty"`
	EgressPolicies         *[]EgressPolicy `json:"egress_policies,omitempty"`
}

func NewHostInventory(hosts []RuntimeHost, opts HostInventoryOptions) *HostInventory {
	hostCopy := make([]RuntimeHost, 0, len(hosts))
	for _, host := range hosts {
		hostCopy = append(hostCopy, cloneRuntimeHost(host))
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHostInventoryTimeout
	}
	return &HostInventory{
		hosts:   hostCopy,
		token:   strings.TrimSpace(opts.APIToken),
		timeout: timeout,
		http:    httpClient,
	}
}

func (i *HostInventory) RuntimeHosts() []RuntimeHost {
	i.mu.RLock()
	defer i.mu.RUnlock()
	hosts := make([]RuntimeHost, 0, len(i.hosts))
	for _, host := range i.hosts {
		hosts = append(hosts, cloneRuntimeHost(host))
	}
	return hosts
}

func (i *HostInventory) PollOnce(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	for idx := range i.hosts {
		i.hosts[idx] = i.pollHost(ctx, i.hosts[idx])
	}
	return nil
}

func (i *HostInventory) pollHost(ctx context.Context, host RuntimeHost) RuntimeHost {
	endpoint := strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
	if endpoint == "" {
		host.Healthy = false
		return host
	}
	reqCtx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		host.Healthy = false
		return host
	}
	if i.token != "" {
		req.Header.Set("Authorization", "Bearer "+i.token)
	}

	resp, err := i.http.Do(req)
	if err != nil {
		host.Healthy = false
		return host
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		host.Healthy = false
		return host
	}

	var health hostHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		host.Healthy = false
		return host
	}
	if strings.ToLower(strings.TrimSpace(health.Status)) != "ok" {
		host.Healthy = false
		return host
	}

	return applyHealthResponseToHost(host, health)
}

func (i *HostInventory) Scheduler(quotas map[string]TenantQuota, usage map[string]TenantUsage) *Scheduler {
	return NewScheduler(i.RuntimeHosts(), quotas, usage)
}

func (i *HostInventory) HostByName(name string) (RuntimeHost, error) {
	normalized := strings.TrimSpace(name)
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, host := range i.hosts {
		if host.Name == normalized {
			return host, nil
		}
	}
	return RuntimeHost{}, fmt.Errorf("runtime host %q not found", normalized)
}

func applyHealthResponseToHost(host RuntimeHost, health hostHealthResponse) RuntimeHost {
	host.Healthy = true
	if health.AvailableVMs != nil {
		host.AvailableVMs = *health.AvailableVMs
	}
	if health.AvailableSnapshotBytes != nil {
		host.AvailableSnapshotBytes = *health.AvailableSnapshotBytes
	}
	if health.EgressPolicies != nil {
		host.EgressPolicies = append([]EgressPolicy(nil), (*health.EgressPolicies)...)
	}
	return host
}
