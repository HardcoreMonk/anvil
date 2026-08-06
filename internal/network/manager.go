package network

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
)

type Manager struct {
	mu                    sync.Mutex
	ipList                []string // sorted slice for deterministic allocation order
	ipInUse               map[string]bool
	gatewayIP             string
	subnet                string
	nextTapID             int
	freeTapIDs            []int // recycled tap IDs — prefer these over nextTapID
	bridgeName            string
	runCommand            func(name string, args ...string) error
	antiSpoof             bool // pin each TAP to its assigned MAC/IP via ebtables
	conntrackFlushEnabled bool // conntrack binary found on PATH at startup (M15 teardown flush)
}

func NewManager(subnet string, gatewayIP string) *Manager {
	// Build a sorted IP list so allocation order is deterministic.
	// A map's iteration order is randomized in Go; a slice is not.
	ips := make([]string, 0, 253)
	for i := 2; i <= 254; i++ {
		ips = append(ips, fmt.Sprintf("%s%d", subnet, i))
	}
	sort.Strings(ips)

	inUse := make(map[string]bool, len(ips))
	for _, ip := range ips {
		inUse[ip] = false
	}

	m := &Manager{
		ipList:     ips,
		ipInUse:    inUse,
		gatewayIP:  gatewayIP,
		subnet:     subnet,
		nextTapID:  1,
		freeTapIDs: nil,
		bridgeName: "goose-br0",
		runCommand: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
	}

	if err := m.setupBridge(); err != nil {
		slog.Warn("bridge setup note (might already exist)", "err", err)
	}

	// Anti-spoof pins each TAP to its assigned MAC/IP at L2. It runs before
	// RecoverVMs re-adds per-TAP rules, so the chain starts clean.
	m.antiSpoof = antiSpoofEnabledFromEnv()
	if m.antiSpoof && !ebtablesAvailable() {
		slog.Warn("anti-spoof requested but ebtables not found on PATH; continuing with anti-spoof disabled")
		m.antiSpoof = false
	}
	if m.antiSpoof {
		m.setupAntiSpoofChain()
		slog.Warn("per-TAP anti-spoof enabled", "chain", antiSpoofChain)
	}

	// M15: probe once at startup rather than on every VM teardown, so a host
	// without conntrack logs a single warning instead of spamming one per
	// Release() call.
	m.conntrackFlushEnabled = conntrackAvailable()
	if !m.conntrackFlushEnabled {
		slog.Warn("conntrack not found on PATH; stale conntrack entries will not be flushed on VM teardown (best-effort hygiene skipped)")
	}

	return m
}

// conntrackAvailable reports whether the conntrack binary is on PATH. It is a
// pure PATH probe (no command execution), called once by NewManager to decide
// whether Release() attempts the M15 teardown flush.
func conntrackAvailable() bool {
	_, err := exec.LookPath("conntrack")
	return err == nil
}

func (m *Manager) setupBridge() error {
	m.command("ip", "link", "add", "name", m.bridgeName, "type", "bridge")
	m.command("ip", "addr", "add", m.gatewayIP+"/24", "dev", m.bridgeName)
	if err := m.command("ip", "link", "set", "dev", m.bridgeName, "up"); err != nil {
		return err
	}

	// Enable IP forwarding so VM packets can reach the internet via the host.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		slog.Warn("enable ip_forward failed", "err", err)
	}

	// NAT masquerade: rewrite source IP of VM packets leaving the internal subnet
	// so replies are routed back through the host.
	// -C checks for an existing identical rule; only add with -A when absent.
	masqArgs := []string{
		"POSTROUTING", "-s", m.subnet + "0/24", "!", "-d", m.subnet + "0/24", "-j", "MASQUERADE",
	}
	if m.command("iptables", append([]string{"-t", "nat", "-C"}, masqArgs...)...) != nil {
		if err := m.command("iptables", append([]string{"-t", "nat", "-A"}, masqArgs...)...); err != nil {
			slog.Warn("failed to add NAT masquerade rule", "err", err)
		}
	}

	forwardRules := [][]string{
		{"FORWARD", "-i", m.bridgeName, "-s", m.subnet + "0/24", "-j", "ACCEPT"},
		{"FORWARD", "-o", m.bridgeName, "-d", m.subnet + "0/24", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, rule := range forwardRules {
		if m.command("iptables", append([]string{"-C"}, rule...)...) == nil {
			continue
		}
		if err := m.command("iptables", append([]string{"-I"}, rule...)...); err != nil {
			slog.Warn("failed to add forwarding rule", "rule", rule, "err", err)
		}
	}

	return nil
}

// AllowBridgeHostPort permits VM-to-host callbacks from the private bridge to
// a specific TCP port on the bridge gateway. This is required on hosts whose
// INPUT policy is DROP; FORWARD/NAT rules alone do not allow packets addressed
// to the host itself, such as in-VM Town Wall posts to 10.0.1.1:3000.
func (m *Manager) AllowBridgeHostPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid host port %d", port)
	}
	rule := []string{
		"INPUT",
		"-i", m.bridgeName,
		"-s", m.subnet + "0/24",
		"-d", m.gatewayIP,
		"-p", "tcp",
		"--dport", strconv.Itoa(port),
		"-j", "ACCEPT",
		"-m", "comment", "--comment", "anvil-cp-callback",
	}
	if m.command("iptables", append([]string{"-C"}, rule...)...) == nil {
		return nil
	}
	if err := m.command("iptables", append([]string{"-I"}, rule...)...); err != nil {
		return fmt.Errorf("add bridge host input rule: %w", err)
	}
	return nil
}

func (m *Manager) command(name string, args ...string) error {
	if m.runCommand != nil {
		return m.runCommand(name, args...)
	}
	return exec.Command(name, args...).Run()
}

func (m *Manager) Allocate() (tapDevice string, guestIP string, macAddr string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Pick the first available IP from the sorted list for deterministic ordering.
	guestIP = ""
	for _, ip := range m.ipList {
		if !m.ipInUse[ip] {
			guestIP = ip
			m.ipInUse[ip] = true
			break
		}
	}
	if guestIP == "" {
		return "", "", "", fmt.Errorf("no available IP addresses")
	}

	// Prefer recycled tap IDs over allocating a new one.
	var tapID int
	if len(m.freeTapIDs) > 0 {
		tapID = m.freeTapIDs[0]
		m.freeTapIDs = m.freeTapIDs[1:]
	} else {
		tapID = m.nextTapID
		m.nextTapID++
	}

	tapDevice = fmt.Sprintf("tap%d", tapID)
	macAddr = fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)

	slog.Info("creating tap device", "tap", tapDevice, "guest_ip", guestIP)
	if err := m.createTapDevice(tapDevice); err != nil {
		m.ipInUse[guestIP] = false
		m.freeTapIDs = append([]int{tapID}, m.freeTapIDs...) // return ID to free-list
		return "", "", "", fmt.Errorf("failed to create TAP device: %w", err)
	}

	m.addAntiSpoofRules(tapDevice, guestIP, macAddr)
	return tapDevice, guestIP, macAddr, nil
}

// ReclaimAllocation re-reserves the exact original IP, TAP name, and MAC for a
// VM being recovered after a daemon restart. Unlike AllocateForRestore (which
// picks any free IP because the guest is reconfigured via vsock post-snapshot),
// cold-restart needs to reuse the prior IP so callers and the agent_url stay
// stable across the restart.
//
// Returns an error if the requested IP or TAP ID is no longer free (which
// indicates a stale state.json — the recovery loop should drop that entry).
func (m *Manager) ReclaimAllocation(tapDeviceName, guestIP, macAddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, known := m.ipInUse[guestIP]; !known {
		return fmt.Errorf("IP %q is not in this manager's subnet", guestIP)
	}
	if m.ipInUse[guestIP] {
		return fmt.Errorf("IP %q is already in use", guestIP)
	}

	var tapID int
	if _, err := fmt.Sscanf(tapDeviceName, "tap%d", &tapID); err != nil {
		return fmt.Errorf("invalid tap device name %q: %w", tapDeviceName, err)
	}

	// Reserve the IP and align the tap-ID bookkeeping so future Allocate calls
	// won't collide with this reclaimed device.
	m.ipInUse[guestIP] = true
	for i, id := range m.freeTapIDs {
		if id == tapID {
			m.freeTapIDs = append(m.freeTapIDs[:i], m.freeTapIDs[i+1:]...)
			break
		}
	}
	if tapID >= m.nextTapID {
		m.nextTapID = tapID + 1
	}

	slog.Warn("reclaiming tap device (recovery)", "tap", tapDeviceName, "guest_ip", guestIP, "mac", macAddr)
	if err := m.createTapDeviceWithMAC(tapDeviceName, macAddr); err != nil {
		m.ipInUse[guestIP] = false
		return fmt.Errorf("failed to recreate TAP device for recovery: %w", err)
	}
	m.addAntiSpoofRules(tapDeviceName, guestIP, macAddr)
	return nil
}

// AllocateForRestore recreates a TAP device with the exact original name and MAC (required by
// Firecracker's snapshot state.bin) and allocates any available IP from the pool.
// The guest IP is reconfigured post-restore via vsock, so any free IP works.
func (m *Manager) AllocateForRestore(tapDeviceName, macAddr string) (tapDevice string, guestIP string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Pick any available IP — the guest will be told its new IP via vsock after restore.
	for _, ip := range m.ipList {
		if !m.ipInUse[ip] {
			guestIP = ip
			m.ipInUse[ip] = true
			break
		}
	}
	if guestIP == "" {
		return "", "", fmt.Errorf("no available IP addresses for restore")
	}

	// Parse tap ID to keep the pool consistent (prevent future collisions).
	var tapID int
	if _, scanErr := fmt.Sscanf(tapDeviceName, "tap%d", &tapID); scanErr != nil {
		m.ipInUse[guestIP] = false
		return "", "", fmt.Errorf("invalid tap device name %q: %w", tapDeviceName, scanErr)
	}
	for i, id := range m.freeTapIDs {
		if id == tapID {
			m.freeTapIDs = append(m.freeTapIDs[:i], m.freeTapIDs[i+1:]...)
			break
		}
	}
	if tapID >= m.nextTapID {
		m.nextTapID = tapID + 1
	}

	slog.Warn("creating tap device (restore)", "tap", tapDeviceName, "guest_ip", guestIP, "mac", macAddr)
	if err := m.createTapDeviceWithMAC(tapDeviceName, macAddr); err != nil {
		m.ipInUse[guestIP] = false
		return "", "", fmt.Errorf("failed to create TAP device for restore: %w", err)
	}

	m.addAntiSpoofRules(tapDeviceName, guestIP, macAddr)
	return tapDeviceName, guestIP, nil
}

// createTapDeviceWithMAC creates a TAP device and sets its MAC address explicitly,
// overriding the kernel-assigned default. Used for snapshot restoration where the
// guest's eth0 already has the original MAC baked into its memory state.
func (m *Manager) createTapDeviceWithMAC(tapName, macAddr string) error {
	exec.Command("ip", "link", "delete", tapName).Run()

	if err := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap").Run(); err != nil {
		return fmt.Errorf("failed to add tuntap %s: %w", tapName, err)
	}

	if err := exec.Command("ip", "link", "set", "dev", tapName, "address", macAddr).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to set MAC %s on %s: %w", macAddr, tapName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "master", m.bridgeName).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to attach %s to bridge %s: %w", tapName, m.bridgeName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "up").Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to bring up %s: %w", tapName, err)
	}

	return nil
}

func (m *Manager) Release(tapDevice string, guestIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// The tap ID drives both anti-spoof rule removal (MAC is deterministic from
	// it, since Release does not receive the MAC) and the free-list recycle.
	var tapID int
	tapParsed := false
	if _, err := fmt.Sscanf(tapDevice, "tap%d", &tapID); err == nil {
		tapParsed = true
		mac := fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)
		m.removeAntiSpoofRules(tapDevice, guestIP, mac)
	}

	if err := m.deleteTapDevice(tapDevice); err != nil {
		slog.Warn("delete tap device failed", "tap", tapDevice, "err", err)
	}

	// M15: flush this guest IP's conntrack entries before it re-enters the
	// pool. Must run after the iptables/ebtables rules are gone (above) and
	// the TAP is deregistered, but strictly before the IP is freed below —
	// otherwise a racing Allocate() could hand the IP to a new VM while a
	// stale entry still carries the egress SNI fastpath mark (0x534e49, see
	// cmd/goose-daemon/sni_verdict.go). That mark lets the fastpath iptables
	// rule ACCEPT a matching flow without a second SNI inspection; a stale
	// entry surviving teardown could let a new tenant on the reused IP ride
	// an old, unrelated approval.
	//
	// Confidence note (could not be verified without root in this audit): a
	// fresh TCP SYN against an existing conntrack entry causes the kernel to
	// tear down and recreate it, which drops the mark — so TCP is likely safe
	// regardless. The realistic exposure is UDP/QUIC, where no handshake
	// forces recreation and a stale entry can persist until timeout (also not
	// independently verified here). Flushing before IP reuse is cheap
	// hygiene either way, so it is done unconditionally rather than gated on
	// a transport we can't confirm.
	m.flushConntrack(guestIP)

	// Return IP to the pool.
	if _, exists := m.ipInUse[guestIP]; exists {
		m.ipInUse[guestIP] = false
	}

	// Return tap ID to the free-list for reuse.
	if tapParsed {
		m.freeTapIDs = append(m.freeTapIDs, tapID)
	}
	return nil
}

// flushConntrack best-effort deletes kernel conntrack entries sourced from
// guestIP (M15). No-op when the conntrack binary was absent at startup
// (conntrackFlushEnabled, warned once in NewManager) — hosts without the tool
// simply skip this hygiene step rather than failing teardown. guestIP is
// validated with net.ParseIP before it reaches exec.Command's argv (never a
// shell) so a corrupted caller-supplied value cannot inject extra arguments.
// Any failure (including the common case of "no matching entries") is
// logged, never returned — this must never fail VM teardown.
func (m *Manager) flushConntrack(guestIP string) {
	if !m.conntrackFlushEnabled {
		return
	}
	if net.ParseIP(guestIP) == nil {
		slog.Warn("skipping conntrack flush: guest IP failed validation", "guest_ip", guestIP)
		return
	}
	if err := m.command("conntrack", "-D", "-s", guestIP); err != nil {
		// Debug, not Warn. Measured on conntrack-tools v1.4.9: `-D` exits 1 with
		// "0 flow entries have been deleted." whenever the filter matched nothing,
		// which is the ordinary outcome for a VM that never opened a tracked flow.
		// Warning there would fire on routine teardowns until operators stopped
		// reading it — the worst place to spend a security-adjacent log line.
		//
		// Exit 1 is also what a non-root caller gets, so the status cannot separate
		// the two, and m.command reports only the error. Neither variant is worth a
		// warning here: the daemon runs as root, and the case that does deserve
		// attention — conntrack absent entirely — is caught once at startup by
		// conntrackAvailable(). A failure that leaves stale entries costs the
		// recycled IP its flush, which the fastpath rule alone never turns into an
		// approval; it only forgoes the extra hygiene.
		slog.Debug("conntrack flush did not delete entries (best-effort, teardown continues)", "guest_ip", guestIP, "err", err)
	}
}

func (m *Manager) createTapDevice(tapName string) error {
	exec.Command("ip", "link", "delete", tapName).Run()

	if err := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap").Run(); err != nil {
		return fmt.Errorf("failed to add tuntap %s: %w", tapName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "master", m.bridgeName).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to attach %s to bridge %s: %w", tapName, m.bridgeName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "up").Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to bring up %s: %w", tapName, err)
	}

	return nil
}

func (m *Manager) deleteTapDevice(tapName string) error {
	return exec.Command("ip", "link", "delete", tapName).Run()
}
