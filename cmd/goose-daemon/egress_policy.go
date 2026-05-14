package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	envEphemeraEgressProfileDir = "EPHEMERA_EGRESS_PROFILE_DIR"
	envAnvilEgressProfileDir    = "ANVIL_EGRESS_PROFILE_DIR"
	defaultEgressProfileDir     = "configs/profiles"
)

type egressProfile struct {
	AllowCIDRs []string `json:"allow_cidrs"`
	AllowHosts []string `json:"allow_hosts"`
	DNSServers []string `json:"dns_servers"`
}

type egressCommand struct {
	Name string
	Args []string
}

func loadEgressProfile(baseDir, profile string) (egressProfile, bool, error) {
	baseDir = strings.TrimSpace(baseDir)
	profile = strings.TrimSpace(profile)
	if baseDir == "" || profile == "" {
		return egressProfile{}, false, nil
	}
	cleanProfile := filepath.Clean(profile)
	if cleanProfile == "." || cleanProfile != profile || strings.Contains(cleanProfile, string(filepath.Separator)) {
		return egressProfile{}, false, fmt.Errorf("invalid egress profile name %q", profile)
	}
	path := filepath.Join(baseDir, cleanProfile, "egress.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return egressProfile{}, false, nil
		}
		return egressProfile{}, false, fmt.Errorf("read egress profile: %w", err)
	}
	var profileConfig egressProfile
	if err := json.Unmarshal(data, &profileConfig); err != nil {
		return egressProfile{}, false, fmt.Errorf("parse egress profile: %w", err)
	}
	if err := validateEgressProfile(profileConfig); err != nil {
		return egressProfile{}, false, err
	}
	return profileConfig, true, nil
}

func validateEgressProfile(profile egressProfile) error {
	for _, cidr := range profile.AllowCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("invalid allow_cidrs entry %q: %w", cidr, err)
		}
	}
	for _, host := range profile.AllowHosts {
		if err := validateEgressHost(host); err != nil {
			return err
		}
	}
	for _, server := range profile.DNSServers {
		if ip := net.ParseIP(strings.TrimSpace(server)); ip == nil {
			return fmt.Errorf("invalid dns_servers entry %q", server)
		}
	}
	return nil
}

func validateEgressHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("allow_hosts entries must be non-empty")
	}
	for _, r := range host {
		if r > 127 {
			return fmt.Errorf("allow_hosts entry %q must be ASCII", host)
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			continue
		}
		return fmt.Errorf("allow_hosts entry %q contains unsupported character %q", host, r)
	}
	return nil
}

func planProfileEgressCommands(vmID, guestIP string, profile egressProfile) ([]egressCommand, error) {
	if strings.TrimSpace(vmID) == "" {
		return nil, fmt.Errorf("vm_id must be non-empty")
	}
	if ip := net.ParseIP(strings.TrimSpace(guestIP)); ip == nil {
		return nil, fmt.Errorf("guest_ip must be a valid IP address")
	}
	if err := validateEgressProfile(profile); err != nil {
		return nil, err
	}
	prefix := "anvil-egress-" + vmID
	commands := []egressCommand{{
		Name: "iptables",
		Args: []string{"-I", "FORWARD", "-s", guestIP, "-j", "REJECT", "-m", "comment", "--comment", prefix + "-default"},
	}}
	if len(profile.DNSServers) > 0 {
		commands = append(commands,
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "udp", "--dport", "53", "-j", "REJECT", "-m", "comment", "--comment", prefix + "-dns-deny-udp"}},
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "tcp", "--dport", "53", "-j", "REJECT", "-m", "comment", "--comment", prefix + "-dns-deny-tcp"}},
		)
	}
	for idx, server := range profile.DNSServers {
		server = strings.TrimSpace(server)
		commands = append(commands,
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-d", server, "-p", "udp", "--dport", "53", "-j", "ACCEPT", "-m", "comment", "--comment", fmt.Sprintf("%s-dns-%d-udp", prefix, idx)}},
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-d", server, "-p", "tcp", "--dport", "53", "-j", "ACCEPT", "-m", "comment", "--comment", fmt.Sprintf("%s-dns-%d-tcp", prefix, idx)}},
		)
	}
	for idx, cidr := range profile.AllowCIDRs {
		commands = append(commands, egressCommand{
			Name: "iptables",
			Args: []string{"-I", "FORWARD", "-s", guestIP, "-d", strings.TrimSpace(cidr), "-j", "ACCEPT", "-m", "comment", "--comment", fmt.Sprintf("%s-cidr-%d", prefix, idx)},
		})
	}
	for idx, host := range profile.AllowHosts {
		commands = append(commands, egressCommand{
			Name: "iptables",
			Args: []string{"-I", "FORWARD", "-s", guestIP, "-m", "string", "--string", strings.TrimSpace(host), "--algo", "bm", "-j", "ACCEPT", "-m", "comment", "--comment", fmt.Sprintf("%s-host-%d", prefix, idx)},
		})
	}
	return commands, nil
}

func egressProfileDir() string {
	if value := os.Getenv(envEphemeraEgressProfileDir); strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if value := os.Getenv(envAnvilEgressProfileDir); strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return defaultEgressProfileDir
}
