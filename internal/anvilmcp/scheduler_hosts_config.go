package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type SchedulerHostsConfig struct {
	Hosts []RuntimeHost `json:"hosts"`
}

func LoadSchedulerHostsFile(path string) ([]RuntimeHost, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scheduler hosts file: %w", err)
	}
	var cfg SchedulerHostsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse scheduler hosts file: %w", err)
	}
	hosts := make([]RuntimeHost, 0, len(cfg.Hosts))
	seen := make(map[string]bool, len(cfg.Hosts))
	for _, host := range cfg.Hosts {
		host.Name = strings.TrimSpace(host.Name)
		host.Endpoint = strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
		if host.Name == "" {
			return nil, fmt.Errorf("scheduler host name must be non-empty")
		}
		if host.Endpoint == "" {
			return nil, fmt.Errorf("scheduler host %q endpoint must be non-empty", host.Name)
		}
		if seen[host.Name] {
			return nil, fmt.Errorf("duplicate scheduler host %q", host.Name)
		}
		seen[host.Name] = true
		if len(host.EgressPolicies) == 0 {
			host.EgressPolicies = []EgressPolicy{EgressPolicyProfile}
		}
		egressPolicies := make([]EgressPolicy, 0, len(host.EgressPolicies))
		for _, policy := range host.EgressPolicies {
			normalized, err := NormalizeEgressPolicy(string(policy))
			if err != nil {
				return nil, fmt.Errorf("scheduler host %q: %w", host.Name, err)
			}
			egressPolicies = append(egressPolicies, normalized)
		}
		hosts = append(hosts, RuntimeHost{
			Name:                   host.Name,
			Endpoint:               host.Endpoint,
			Healthy:                host.Healthy,
			AvailableVMs:           host.AvailableVMs,
			AvailableSnapshotBytes: host.AvailableSnapshotBytes,
			EgressPolicies:         egressPolicies,
			SmokeOnly:              host.SmokeOnly,
		})
	}
	return hosts, nil
}
