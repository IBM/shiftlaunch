package services

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.ibm.com/sudeeshjohn/shiftlaunch/config"
	"github.ibm.com/sudeeshjohn/shiftlaunch/localexec"
	"github.ibm.com/sudeeshjohn/shiftlaunch/logger"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

// HTTPServerManager manages HTTP server directory structure for OpenShift installation
type HTTPServerManager struct {
	cfg        *types.AgentConfig
	daemonCfg  *config.AgentDaemonConfig // <--- THIS WAS MISSING
	exec       *localexec.LocalClient
	logger     *logger.Logger
	downloader *Downloader
}

// NewHTTPServerManager creates a new HTTP server manager
func NewHTTPServerManager(cfg *types.AgentConfig, daemonCfg *config.AgentDaemonConfig, exec *localexec.LocalClient, log *logger.Logger) *HTTPServerManager {
	return &HTTPServerManager{
		cfg:        cfg,
		daemonCfg:  daemonCfg,
		exec:       exec,
		logger:     log,
		downloader: NewDownloader(cfg, daemonCfg, exec, log),
	}
}

// Setup creates the HTTP directory structure and downloads required files
func (h *HTTPServerManager) Setup(ctx context.Context,workspaceDir string) error {
	h.logger.Info("Setting up HTTP server for deployment...", "cluster", h.cfg.OpenShift.ClusterName)

	// Create base cluster directory
	if err := h.createClusterDirectory(ctx); err != nil {
		return fmt.Errorf("failed to create cluster directory: %w", err)
	}

	// Create subdirectories
	if err := h.createSubdirectories(ctx); err != nil {
		return fmt.Errorf("failed to create subdirectories: %w", err)
	}

	// Download RHCOS images and OpenShift tools
	if err := h.downloader.DownloadAll(ctx,workspaceDir); err != nil {
		return fmt.Errorf("failed to download artifacts: %w", err)
	}

	// Set proper permissions
	if err := h.setPermissions(ctx); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Restore SELinux contexts
	if err := h.restoreSELinuxContexts(ctx); err != nil {
		return fmt.Errorf("failed to restore SELinux contexts: %w", err)
	}

	// Create helper script
	if err := h.createHelperScript(ctx); err != nil {
		return fmt.Errorf("failed to create helper script: %w", err)
	}

	h.logger.Info("HTTP server setup completed successfully")
	return nil
}

// createClusterDirectory creates the main cluster directory
func (h *HTTPServerManager) createClusterDirectory(ctx context.Context) error {
	clusterDir := h.GetClusterHTTPDir()

	// Check if directory already exists
	checkCmd := fmt.Sprintf("test -d %s && echo 'exists' || echo 'missing'", clusterDir)
	output, err := h.exec.Execute(ctx,checkCmd)

	if err == nil && strings.TrimSpace(output) == "exists" {
		h.logger.Debug("Cluster directory already exists", "dir", clusterDir)

		// Check if it contains files from a previous deployment
		listCmd := fmt.Sprintf("ls -A %s 2>/dev/null | wc -l", clusterDir)
		countOutput, _ := h.exec.Execute(ctx,listCmd)
		fileCount := strings.TrimSpace(countOutput)

		if fileCount != "0" {
			h.logger.Warn("Directory contains files from previous deployment", "count", fileCount)
			h.logger.Debug("Existing files will be overwritten during setup")
		}
		return nil
	}

	// Create directory if it doesn't exist
	cmd := fmt.Sprintf("sudo mkdir -p %s", clusterDir)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to create cluster directory %s: %w", clusterDir, err)
	}

	h.logger.Info("Created cluster directory", "dir", clusterDir)
	return nil
}

// createSubdirectories creates all required subdirectories
func (h *HTTPServerManager) createSubdirectories(ctx context.Context) error {
	clusterDir := h.GetClusterHTTPDir()

	subdirs := []string{
		"ignition", // Ignition files (bootstrap.ign, master.ign, worker.ign)
		"rhcos",    // RHCOS images (kernel, initramfs, rootfs)
		"tools",    // OpenShift installer and client tools
		"scripts",  // Helper scripts
	}

	for _, subdir := range subdirs {
		path := filepath.Join(clusterDir, subdir)

		// Check if subdirectory exists
		checkCmd := fmt.Sprintf("test -d %s && echo 'exists' || echo 'missing'", path)
		output, err := h.exec.Execute(ctx,checkCmd)

		if err == nil && strings.TrimSpace(output) == "exists" {
			h.logger.Debug("Subdirectory already exists", "path", path)
			continue
		}

		// Create subdirectory
		cmd := fmt.Sprintf("sudo mkdir -p %s", path)
		if _, err := h.exec.Execute(ctx,cmd); err != nil {
			return fmt.Errorf("failed to create subdirectory %s: %w", path, err)
		}

		h.logger.Info("Created subdirectory", "path", path)
	}

	return nil
}

// setPermissions sets proper permissions on HTTP directories
func (h *HTTPServerManager) setPermissions(ctx context.Context) error {
	clusterDir := h.GetClusterHTTPDir()

	// Set directory permissions to 755 (rwxr-xr-x)
	cmd := fmt.Sprintf("sudo chmod -R 755 %s", clusterDir)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// Set ownership to apache user (typically apache:apache or httpd:httpd)
	cmd = fmt.Sprintf("sudo chown -R apache:apache %s 2>/dev/null || sudo chown -R httpd:httpd %s 2>/dev/null || true",
		clusterDir, clusterDir)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		h.logger.Warn("Could not set apache/httpd ownership", "error", err)
	}

	h.logger.Info("Set permissions on directory", "dir", clusterDir)
	return nil
}

// restoreSELinuxContexts restores SELinux contexts for HTTP directories
func (h *HTTPServerManager) restoreSELinuxContexts(ctx context.Context) error {
	clusterDir := h.GetClusterHTTPDir()

	if _, err := h.exec.Execute(ctx,fmt.Sprintf("sudo restorecon -R -v %s", clusterDir)); err != nil {
		h.logger.Warn("Could not restore SELinux contexts", "error", err)
		return nil
	}

	h.logger.Info("Restored SELinux contexts for directory", "dir", clusterDir)
	return nil
}

// createHelperScript creates the install helper script
func (h *HTTPServerManager) createHelperScript(ctx context.Context) error {
	script := GenerateHelperScript(h.cfg, h.daemonCfg.Network.HTTPPort)

	scriptPath := filepath.Join(h.GetClusterHTTPDir(), "scripts", "install-helper.sh")

	if err := h.exec.WriteFile(ctx,scriptPath, []byte(script), 0644); err != nil {
		return fmt.Errorf("failed to upload helper script: %w", err)
	}

	// Make executable
	cmd := fmt.Sprintf("sudo chmod 755 %s", scriptPath)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to make script executable: %w", err)
	}

	h.logger.Info("Created install helper script")
	return nil
}

// UploadIgnitionFile uploads an ignition file to the cluster's ignition directory
func (h *HTTPServerManager) UploadIgnitionFile(ctx context.Context,filename string, content []byte) error {
	destPath := filepath.Join(h.GetClusterHTTPDir(), "ignition", filename)

	if err := h.exec.WriteFile(ctx,destPath, content, 0644); err != nil {
		return fmt.Errorf("failed to upload ignition file %s: %w", filename, err)
	}

	// Set proper permissions
	cmd := fmt.Sprintf("sudo chmod 644 %s", destPath)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to set permissions on %s: %w", destPath, err)
	}

	h.logger.Info("Uploaded ignition file", "file", filename)
	return nil
}

// GetIgnitionURL returns the HTTP URL for an ignition file
func (h *HTTPServerManager) GetIgnitionURL(filename string) string {
	return fmt.Sprintf("http://%s:%d/%s/ignition/%s",
		h.cfg.Controller.IP,
		h.daemonCfg.Network.HTTPPort,
		h.cfg.OpenShift.ClusterName,
		filename)
}

// GetRHCOSImageURL returns the HTTP URL for an RHCOS image
func (h *HTTPServerManager) GetRHCOSImageURL(filename string) string {
	return fmt.Sprintf("http://%s:%d/%s/rhcos/%s",
		h.cfg.Controller.IP,
		h.daemonCfg.Network.HTTPPort,
		h.cfg.OpenShift.ClusterName,
		filename)
}

// GetKernelURL returns the URL for the RHCOS kernel
func (h *HTTPServerManager) GetKernelURL() string {
	return h.GetRHCOSImageURL("rhcos-live-kernel-ppc64le")
}

// GetInitramfsURL returns the URL for the RHCOS initramfs
func (h *HTTPServerManager) GetInitramfsURL() string {
	return h.GetRHCOSImageURL("rhcos-live-initramfs.ppc64le.img")
}

// GetRootfsURL returns the URL for the RHCOS rootfs
func (h *HTTPServerManager) GetRootfsURL() string {
	return h.GetRHCOSImageURL("rhcos-live-rootfs.ppc64le.img")
}

// GetOpenShiftInstallPath returns the path to openshift-install binary
func (h *HTTPServerManager) GetOpenShiftInstallPath() string {
	return filepath.Join(h.GetClusterHTTPDir(), "tools", "openshift-install")
}

// GetClusterHTTPDir returns the cluster's HTTP directory path
func (h *HTTPServerManager) GetClusterHTTPDir() string {
	return filepath.Join("/var/www/html", h.cfg.OpenShift.ClusterName)
}

// Cleanup removes the cluster's HTTP directory
func (h *HTTPServerManager) Cleanup(ctx context.Context) error {
	h.logger.Info("Cleaning up HTTP directories for cluster...", "cluster", h.cfg.OpenShift.ClusterName)

	clusterDir := h.GetClusterHTTPDir()

	cmd := fmt.Sprintf("sudo rm -rf %s", clusterDir)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to remove cluster directory: %w", err)
	}

	h.logger.Info("HTTP directories removed successfully")
	return nil
}

// VerifySetup verifies that the HTTP directory structure is correct
func (h *HTTPServerManager) VerifySetup(ctx context.Context) error {
	h.logger.Info("Verifying HTTP server setup for deployment...", "cluster", h.cfg.OpenShift.ClusterName)

	clusterDir := h.GetClusterHTTPDir()

	// Check main directory
	cmd := fmt.Sprintf("test -d %s", clusterDir)
	if _, err := h.exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("cluster directory does not exist: %s", clusterDir)
	}

	// Check subdirectories
	subdirs := []string{"ignition", "rhcos", "tools", "scripts"}
	for _, subdir := range subdirs {
		path := filepath.Join(clusterDir, subdir)
		cmd := fmt.Sprintf("test -d %s", path)
		if _, err := h.exec.Execute(ctx,cmd); err != nil {
			return fmt.Errorf("subdirectory does not exist: %s", path)
		}
	}

	// Check RHCOS images
	rhcosImages := []string{
		"kernel",
		"initramfs.img",
		"rootfs.img",
	}
	for _, image := range rhcosImages {
		path := filepath.Join(clusterDir, "rhcos", image)
		cmd := fmt.Sprintf("test -f %s", path)
		if _, err := h.exec.Execute(ctx,cmd); err != nil {
			return fmt.Errorf("RHCOS image missing: %s", image)
		}
	}

	h.logger.Info("HTTP server setup verified successfully")
	return nil
}

// GetDiskUsage returns the disk usage of the cluster's HTTP directory
func (h *HTTPServerManager) GetDiskUsage(ctx context.Context) (string, error) {
	clusterDir := h.GetClusterHTTPDir()

	cmd := fmt.Sprintf("sudo du -sh %s 2>/dev/null | awk '{print $1}'", clusterDir)
	output, err := h.exec.Execute(ctx,cmd)
	if err != nil {
		return "", fmt.Errorf("failed to get disk usage: %w", err)
	}

	return output, nil
}
// Inside services/httpserver.go

// StageFiles copies the downloaded images and generated ignition configs into the HTTP root
func (h *HTTPServerManager) StageFiles(ctx context.Context,workspaceDir string) error {
	h.logger.Info("Staging artifacts to HTTP directory...", "cluster", h.cfg.OpenShift.ClusterName)
	httpDir := h.GetClusterHTTPDir()

	// Shield from cancellation! Truncated rootfs or ignition files
	// hosted by Apache will cause catastrophic OpenShift boot failures!
	shieldedCtx := context.WithoutCancel(ctx)

	// 1. Copy RHCOS images from workspace to HTTP directory
	copyRhcos := fmt.Sprintf("sudo cp -r %s/rhcos/* %s/rhcos/ 2>/dev/null || true", workspaceDir, httpDir)
	if _, err := h.exec.Execute(shieldedCtx, copyRhcos); err != nil {
		h.logger.Warn("Failed to stage some RHCOS images", "error", err)
	}

	// 2. Copy Ignition files from the install-dir directory based on topology
	targetDir := filepath.Join(workspaceDir, "install-dir")
	var copyIgnCmd string
	
	if h.cfg.IsSNO() {
		copyIgnCmd = fmt.Sprintf("sudo cp %s/bootstrap-in-place-for-live-iso.ign %s/ignition/bootstrap.ign", targetDir, httpDir)
	} else {
		copyIgnCmd = fmt.Sprintf("sudo cp %s/*.ign %s/ignition/", targetDir, httpDir)
	}

	if _, err := h.exec.Execute(shieldedCtx, copyIgnCmd); err != nil {
		return fmt.Errorf("failed to stage ignition files: %w", err)
	}

	// 3. Fix permissions and SELinux contexts for the HTTP server
	h.exec.Execute(shieldedCtx, fmt.Sprintf("sudo chmod -R 755 %s", httpDir))
	h.exec.Execute(shieldedCtx, fmt.Sprintf("sudo restorecon -Rv %s", httpDir))

	h.logger.Info("Artifacts staged successfully")
	return nil
}