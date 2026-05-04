package services

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

// NFSManager handles the local NFS server configuration for ISO boot mode
type NFSManager struct {
	cfg          *types.AgentConfig
	executor     *localexec.LocalClient
	logger       *logger.Logger
	workspaceDir string
}

// NewNFSManager creates a new NFS manager
func NewNFSManager(cfg *types.AgentConfig, exec *localexec.LocalClient, log *logger.Logger, workspaceDir string) *NFSManager {
	return &NFSManager{
		cfg:          cfg,
		executor:     exec,
		logger:       log,
		workspaceDir: workspaceDir,
	}
}

// Setup configures and starts the NFS service for the cluster's install directory
func (n *NFSManager) Setup(ctx context.Context) error {
	n.logger.Info("Setting up local NFS server for ISO boot...", "cluster", n.cfg.OpenShift.ClusterName)

	// 1. Ensure the install-dir exists so exportfs doesn't fail
	installDir := filepath.Join(n.workspaceDir, "install-dir")
	if _, err := n.executor.Execute(ctx, fmt.Sprintf("sudo mkdir -p %s", installDir)); err != nil {
		return fmt.Errorf("failed to create install-dir for NFS export: %w", err)
	}

	// 2. Ensure /etc/exports.d exists (standard on RHEL/CentOS, but safe to guarantee)
	if _, err := n.executor.Execute(ctx, "sudo mkdir -p /etc/exports.d"); err != nil {
		return fmt.Errorf("failed to create /etc/exports.d directory: %w", err)
	}

	// 3. Create the export configuration
	// We use a cluster-specific drop-in file to avoid touching the main /etc/exports
	exportPath := fmt.Sprintf("/etc/exports.d/shiftlaunch-%s.exports", n.cfg.OpenShift.ClusterName)
	
	// Exporting as read-only to all hosts (*). The options ensure stable, safe reads for the VIOS.
	exportContent := fmt.Sprintf("%s *(rw,sync,insecure,no_root_squash)\n", installDir)

	n.logger.Debug("Writing NFS export file", "file", exportPath)
	if err := n.executor.WriteFile(ctx, exportPath, []byte(exportContent), 0644); err != nil {
		return fmt.Errorf("failed to write NFS export file: %w", err)
	}

	// 4. Ensure NFS server is enabled and started
	n.logger.Debug("Enabling and starting nfs-server service")
	if err := n.executor.SystemctlEnable(ctx, "nfs-server"); err != nil {
		return fmt.Errorf("failed to enable nfs-server: %w", err)
	}
	if _, err := n.executor.Execute(ctx, "sudo systemctl start nfs-server"); err != nil {
		return fmt.Errorf("failed to start nfs-server: %w", err)
	}

	// 5. Apply the exports dynamically without restarting the daemon
	n.logger.Debug("Applying NFS exports")
	if _, err := n.executor.Execute(ctx, "sudo exportfs -arv"); err != nil {
		return fmt.Errorf("failed to apply NFS exports: %w", err)
	}

	n.logger.Info("NFS server configured and directory exported", "dir", installDir)
	return nil
}

// Cleanup removes the cluster's NFS export and dynamically unpublishes the directory
func (n *NFSManager) Cleanup(ctx context.Context) error {
	n.logger.Info("Cleaning up NFS configuration...", "cluster", n.cfg.OpenShift.ClusterName)

	exportPath := fmt.Sprintf("/etc/exports.d/shiftlaunch-%s.exports", n.cfg.OpenShift.ClusterName)

	// Remove the specific drop-in export file
	if _, err := n.executor.Execute(ctx, fmt.Sprintf("sudo rm -f %s", exportPath)); err != nil {
		n.logger.Warn("Failed to remove NFS export file", "file", exportPath, "error", err)
	}

	// Re-apply exports to unpublish the directory immediately from the kernel
	if _, err := n.executor.Execute(ctx, "sudo exportfs -arv"); err != nil {
		n.logger.Warn("Failed to reload NFS exports during cleanup", "error", err)
		return err
	}

	n.logger.Info("NFS configuration cleaned up")
	return nil
}