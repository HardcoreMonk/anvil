package storage

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// Provisioner handles the disk lifecycle for MicroVMs.
type Provisioner struct {
	GoldenImagePath string // e.g., "artifacts/ubuntu-22.04-goose.ext4"
	WorkspaceDir    string // e.g., "/tmp/goose-workspaces"
	BuildScriptPath string // e.g., "scripts/build_image.sh"
}

// NewProvisioner initializes a new Storage Provisioner and ensures the golden image exists.
func NewProvisioner(goldenImagePath, workspaceDir, buildScriptPath string) (*Provisioner, error) {
	// 1. Ensure the workspace directory exists
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	p := &Provisioner{
		GoldenImagePath: goldenImagePath,
		WorkspaceDir:    workspaceDir,
		BuildScriptPath: buildScriptPath,
	}

	// 2. Self-bootstrap: Check and build golden image if missing
	if err := p.EnsureGoldenImage(); err != nil {
		return nil, fmt.Errorf("failed to ensure golden image: %w", err)
	}

	return p, nil
}

// EnsureGoldenImage checks if the golden image exists, and if not, builds it from scratch.
func (p *Provisioner) EnsureGoldenImage() error {
	_, err := os.Stat(p.GoldenImagePath)
	if err == nil {
		log.Printf("Golden image found at %s. Skipping build.", p.GoldenImagePath)
		return nil
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf("error checking golden image: %w", err)
	}

	log.Printf("Golden image not found at %s. Starting automated build process...", p.GoldenImagePath)
	log.Printf("This may take a few minutes. Please wait.")

	// Execute the build script
	cmd := exec.Command("bash", p.BuildScriptPath)
	
	// Pipe the script's output to the daemon's standard output for visibility
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute build script: %w", err)
	}

	// Verify the image was actually created by the script
	if _, err := os.Stat(p.GoldenImagePath); os.IsNotExist(err) {
		return fmt.Errorf("build script completed, but golden image was not found at expected path")
	}

	log.Printf("Golden image successfully built and verified at %s.", p.GoldenImagePath)
	return nil
}

// CloneDisk creates an isolated copy of the golden image for a specific VM.
func (p *Provisioner) CloneDisk(vmID string) (string, error) {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	log.Printf("Cloning golden image to %s...", destPath)

	srcFile, err := os.Open(p.GoldenImagePath)
	if err != nil {
		return "", fmt.Errorf("failed to open golden image: %w", err)
	}
	defer srcFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return "", fmt.Errorf("failed to copy disk contents: %w", err)
	}

	if err := destFile.Sync(); err != nil {
		return "", fmt.Errorf("failed to sync data to disk: %w", err)
	}

	log.Printf("Successfully provisioned isolated disk for MicroVM [%s]", vmID)
	return destPath, nil
}

// CleanupDisk safely removes the disk image after the VM is destroyed.
func (p *Provisioner) CleanupDisk(vmID string) error {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	log.Printf("Cleaning up disk image: %s", destPath)

	if err := os.Remove(destPath); err != nil {
		if os.IsNotExist(err) {
			log.Printf("Disk image %s already deleted or does not exist.", destPath)
			return nil
		}
		return fmt.Errorf("failed to delete disk file: %w", err)
	}

	log.Printf("Successfully cleaned up storage resources for MicroVM [%s]", vmID)
	return nil
}