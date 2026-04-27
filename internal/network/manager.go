package network

import (
	"fmt"
	"log"
	"os/exec"
	"sync"
)

type Manager struct {
	mu         sync.Mutex
	ipPool     map[string]bool
	gatewayIP  string
	subnet     string
	nextTapID  int
	bridgeName string // Added bridge name
}

func NewManager(subnet string, gatewayIP string) *Manager {
	pool := make(map[string]bool)
	for i := 2; i <= 254; i++ {
		ip := fmt.Sprintf("%s%d", subnet, i)
		pool[ip] = false
	}

	m := &Manager{
		ipPool:     pool,
		gatewayIP:  gatewayIP,
		subnet:     subnet,
		nextTapID:  1,
		bridgeName: "goose-br0", // Unified virtual switch
	}

	// Host OS에 가상 스위치(Bridge)를 생성하고 게이트웨이 IP를 부여합니다.
	if err := m.setupBridge(); err != nil {
		log.Printf("Warning: Bridge setup note (might already exist): %v", err)
	}

	return m
}

func (m *Manager) setupBridge() error {
	exec.Command("ip", "link", "add", "name", m.bridgeName, "type", "bridge").Run()
	exec.Command("ip", "addr", "add", m.gatewayIP+"/24", "dev", m.bridgeName).Run()
	return exec.Command("ip", "link", "set", "dev", m.bridgeName, "up").Run()
}

func (m *Manager) Allocate() (tapDevice string, guestIP string, macAddr string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	guestIP = ""
	for ip, inUse := range m.ipPool {
		if !inUse {
			guestIP = ip
			m.ipPool[ip] = true
			break
		}
	}

	if guestIP == "" {
		return "", "", "", fmt.Errorf("no available IP addresses")
	}

	tapDevice = fmt.Sprintf("tap%d", m.nextTapID)
	macAddr = fmt.Sprintf("AA:FC:00:00:%02X:%02X", m.nextTapID/256, m.nextTapID%256)
	m.nextTapID++

	log.Printf("Creating TAP device: %s for IP: %s...", tapDevice, guestIP)
	if err := m.createTapDevice(tapDevice); err != nil {
		m.ipPool[guestIP] = false
		return "", "", "", fmt.Errorf("failed to create TAP device: %w", err)
	}

	return tapDevice, guestIP, macAddr, nil
}

func (m *Manager) Release(tapDevice string, guestIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.deleteTapDevice(tapDevice); err != nil {
		log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, err)
	}
	if _, exists := m.ipPool[guestIP]; exists {
		m.ipPool[guestIP] = false
	}
	return nil
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