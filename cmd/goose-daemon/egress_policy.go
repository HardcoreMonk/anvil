package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	envEphemeraEgressProfileDir = "EPHEMERA_EGRESS_PROFILE_DIR"
	envAnvilEgressProfileDir    = "ANVIL_EGRESS_PROFILE_DIR"
	defaultEgressProfileDir     = "configs/profiles"

	// sniApprovedConnmark is the connmark iptables/conntrack applies once the
	// NFQUEUE-side SNI inspector approves a ClientHello for a connection. ASCII "SNI".
	sniApprovedConnmark = "0x534e49"
	// sniQueueNumDefault is the NFQUEUE queue number the SNI inspector listens on
	// unless overridden by ANVIL_SNI_QUEUE_NUM.
	sniQueueNumDefault = 88
)

// sniQueueNum returns the NFQUEUE queue number to dispatch unapproved TLS
// connections to, honoring an ANVIL_SNI_QUEUE_NUM override when it parses as
// a valid 0..65535 queue number.
func sniQueueNum() int {
	if v := strings.TrimSpace(os.Getenv("ANVIL_SNI_QUEUE_NUM")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 65535 {
			return n
		}
	}
	return sniQueueNumDefault
}

type egressProfile struct {
	AllowCIDRs []string `json:"allow_cidrs"`
	AllowHosts []string `json:"allow_hosts"`
	AllowSNI   []string `json:"allow_sni"`
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
	for _, entry := range profile.AllowSNI {
		if err := validateEgressSNI(entry); err != nil {
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
	return validateDomainCharset("allow_hosts", host)
}

// validateDomainCharset enforces the shared ASCII domain charset (letters,
// digits, '.', '-') on an already-trimmed, non-empty host. field names the JSON
// key the host came from ("allow_hosts" or "allow_sni") so the error points at
// the entry the operator actually wrote — validateEgressHost and
// validateEgressSNI both reuse this so the rule lives in one place.
func validateDomainCharset(field, host string) error {
	for _, r := range host {
		if r > 127 {
			return fmt.Errorf("%s entry %q must be ASCII", field, host)
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			continue
		}
		return fmt.Errorf("%s entry %q contains unsupported character %q", field, host, r)
	}
	return nil
}

func validateEgressSNI(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("allow_sni entries must be non-empty")
	}
	host := entry
	if strings.HasPrefix(host, "*.") {
		host = host[2:]
	}
	if strings.ContainsRune(host, '*') {
		return fmt.Errorf("allow_sni entry %q: '*' only allowed as a leading %q label", entry, "*.")
	}
	if host == "" {
		// A bare "*." carries no domain label to validate; reject it as empty
		// (preserves the pre-refactor behavior when this fell through to
		// validateEgressHost's non-empty guard).
		return fmt.Errorf("allow_sni entries must be non-empty")
	}
	// host now has no '*'; reuse the ASCII domain-charset validator with the
	// allow_sni field so the message names the right JSON key.
	return validateDomainCharset("allow_sni", host)
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
	if len(profile.AllowSNI) > 0 {
		q := strconv.Itoa(sniQueueNum())
		commands = append(commands,
			// dispatch first in slice so it lands BELOW fastpath after -I reversal.
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "tcp", "--dport", "443", "-m", "connmark", "!", "--mark", sniApprovedConnmark, "-j", "NFQUEUE", "--queue-num", q, "-m", "comment", "--comment", prefix + "-sni-nfqueue"}},
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "tcp", "--dport", "443", "-m", "connmark", "--mark", sniApprovedConnmark, "-j", "ACCEPT", "-m", "comment", "--comment", prefix + "-sni-fastpath"}},
			// UDP:443 (QUIC) mirrors the TCP dispatch above: same queue, same
			// connmark, dispatch first in slice so fastpath lands above it too.
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "udp", "--dport", "443", "-m", "connmark", "!", "--mark", sniApprovedConnmark, "-j", "NFQUEUE", "--queue-num", q, "-m", "comment", "--comment", prefix + "-sni-udp-nfqueue"}},
			egressCommand{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", guestIP, "-p", "udp", "--dport", "443", "-m", "connmark", "--mark", sniApprovedConnmark, "-j", "ACCEPT", "-m", "comment", "--comment", prefix + "-sni-udp-fastpath"}},
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
