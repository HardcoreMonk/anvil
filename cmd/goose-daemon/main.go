package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	// Replace "ephemera" with your actual module name if different
	"ephemera/internal/network"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

func main() {
	log.Println("Starting Ephemera Control Plane (End-to-End Test)...")

	// 1. Initialize graceful shutdown context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Resolve Absolute Paths dynamically
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Fatal error getting current working directory: %v", err)
	}
	
	goldenImagePath := filepath.Join(cwd, "artifacts/ubuntu-22.04-goose.ext4")
	buildScriptPath := filepath.Join(cwd, "scripts/build_image.sh")
	kernelPath := filepath.Join(cwd, "artifacts/vmlinux-5.10.bin")

	// 2. Initialize Core Modules with Absolute Paths
	log.Println("Initializing Storage Provisioner...")
	provisioner, err := storage.NewProvisioner(
		goldenImagePath,
		"/tmp/goose-workspaces",
		buildScriptPath,
	)
	if err != nil {
		log.Fatalf("Fatal error initializing storage: %v", err)
	}

	log.Println("Initializing Network Manager...")
	netManager := network.NewManager("10.0.1.", "10.0.1.1")

	// ==========================================
	// [Simulation] Provisioning a new Agent Request
	// ==========================================
	vmID := "agent-dynamic-01"
	log.Printf("Received request to spawn new MicroVM: [%s]", vmID)

	// 3. Allocate Network Resources
	tapDevice, guestIP, macAddr, err := netManager.Allocate()
	if err != nil {
		log.Fatalf("Failed to allocate network: %v", err)
	}
	// Defer network cleanup to ensure resources are returned on exit
	defer netManager.Release(tapDevice, guestIP)

	// 4. Provision Storage (Clone Golden Image)
	diskPath, err := provisioner.CloneDisk(vmID)
	if err != nil {
		log.Fatalf("Failed to provision disk: %v", err)
	}
	// Defer disk cleanup to ensure file is deleted on exit
	defer provisioner.CleanupDisk(vmID)

	// 5. Start MicroVM with dynamically allocated resources
	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", vmID)
	
	// Clean up stale socket if exists
	os.Remove(socketPath)

	cfg := vm.VMConfig{
		VMID:       vmID,
		SocketPath: socketPath,
		KernelPath: kernelPath, // Updated to use absolute path
		RootfsPath: diskPath,
		TapDevice:  tapDevice,
		MacAddress: macAddr,
		GuestIP:    guestIP,
		GatewayIP:  "10.0.1.1",
	}

	log.Printf("Booting MicroVM [%s] with IP: %s, TAP: %s...", vmID, guestIP, tapDevice)
	machine, err := vm.StartMachine(ctx, cfg)
	if err != nil {
		log.Fatalf("Fatal error starting MicroVM: %v", err)
	}
	
	// Defer VMM shutdown and socket cleanup
	defer func() {
		machine.StopVMM()
		os.Remove(socketPath)
	}()

	log.Printf("MicroVM [%s] is fully operational at %s. Press Ctrl+C to destroy.", vmID, guestIP)

	// 6. Wait for termination signal (Ctrl+C)
	<-sigChan
	log.Println("\nReceived termination signal. Executing graceful teardown sequence...")
}