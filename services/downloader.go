package services

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sudeeshjohn/shiftlaunch/config"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

// Downloader handles downloading RHCOS images and OpenShift tools
type Downloader struct {
	cfg       *types.AgentConfig
	daemonCfg *config.AgentDaemonConfig
	exec      *localexec.LocalClient
	logger    *logger.Logger
}

// NewDownloader creates a new downloader for local execution
func NewDownloader(cfg *types.AgentConfig, daemonCfg *config.AgentDaemonConfig, exec *localexec.LocalClient, log *logger.Logger) *Downloader {
	return &Downloader{
		cfg:       cfg,
		daemonCfg: daemonCfg,
		exec:      exec,
		logger:    log,
	}
}

// DownloadAll downloads all required artifacts into the local workspace
func (d *Downloader) DownloadAll(ctx context.Context,workspaceDir string) error {
	if err := d.DownloadRHCOSImages(ctx,workspaceDir); err != nil {
		return fmt.Errorf("failed to download RHCOS images: %w", err)
	}
	if err := d.DownloadOpenShiftTools(ctx,workspaceDir); err != nil {
		return fmt.Errorf("failed to download OpenShift tools: %w", err)
	}
	return nil
}
// DownloadRHCOSImages downloads RHCOS images with optional checksum validation
func (d *Downloader) DownloadRHCOSImages(ctx context.Context,workspaceDir string) error {
	d.logger.Info("Downloading RHCOS images to local workspace...")

	rhcosDir := filepath.Join(workspaceDir, "rhcos")
	d.exec.Execute(ctx,fmt.Sprintf("mkdir -p %s", rhcosDir))

	urls := d.cfg.OpenShift.RHCOSImages
	manifestPath := filepath.Join(rhcosDir, "sha256sum.txt")
	timeout := d.daemonCfg.Timeouts.DownloadTimeoutSec // Get timeout from config

	// 1. Fetch global manifest ONLY if checksum_url is provided
	if urls.ChecksumURL != "" {
		d.logger.Info("Integrity Mode: Downloading checksum manifest", "url", urls.ChecksumURL)

		// Apply timeout to manifest download as well
		dlManifestCmd := fmt.Sprintf("curl -sSL --max-time %d -o %s '%s'", timeout, manifestPath, urls.ChecksumURL)
		if _, err := d.exec.Execute(ctx,dlManifestCmd); err != nil {
			return fmt.Errorf("failed to fetch checksum manifest: %w", err)
		}

		d.logger.Info("Checksum manifest downloaded")
	}

	images := []struct {
		url          string
		filename     string
		desc         string
		specificCSUM string // Individual hash from YAML
	}{
		{urls.KernelURL, "kernel", "RHCOS kernel", urls.KernelCSUM},
		{urls.InitramfsURL, "initramfs.img", "RHCOS initramfs", urls.InitramfsCSUM},
		{urls.RootfsURL, "rootfs.img", "RHCOS rootfs", urls.RootfsCSUM},
	}

	for _, img := range images {
		if img.url == "" {
			return fmt.Errorf("%s URL not provided in configuration", img.desc)
		}
		destPath := filepath.Join(rhcosDir, img.filename)
		expectedHash := ""

		// 2. Determine Expected Hash (Hierarchy of Truth)
		if img.specificCSUM != "" {
			expectedHash = img.specificCSUM
			d.logger.Debug("Using individual checksum", "image", img.desc)
		} else if urls.ChecksumURL != "" {
			hash, err := d.extractHashFromManifest(ctx,img.url, manifestPath)
			if err == nil && hash != "" {
				expectedHash = hash
				d.logger.Debug("Using manifest checksum", "image", img.desc)
			}
		}

		// 3. Conditional Flow based on checksum availability and force_ocp_download flag
		forceDownload := d.cfg.OpenShift.ForceOCPDownload

		if forceDownload {
			d.logger.Info("Force download requested. Wiping existing file...", "file", destPath)
			d.exec.Execute(ctx,fmt.Sprintf("rm -f %s", destPath))
		} else {
			if expectedHash != "" {
				existsCmd := fmt.Sprintf("test -f %s", destPath)
				if _, err := d.exec.Execute(ctx,existsCmd); err == nil {
					if d.verifyFileHash(ctx,destPath, expectedHash) {
						d.logger.Info("Checksum matches, skipping download", "image", img.desc)
						continue
					}
					d.logger.Warn("Checksum mismatch. Wiping corrupted file and re-downloading...", "image", img.desc)
					d.exec.Execute(ctx,fmt.Sprintf("rm -f %s", destPath))
				}
			} else {
				checkCmd := fmt.Sprintf("test -s %s", destPath)
				if _, err := d.exec.Execute(ctx,checkCmd); err == nil {
					d.logger.Info("File already exists, skipping download (no checksum validation)", "image", img.desc)
					continue
				}
			}
		}

		// 4. Download the file
		d.logger.Info("Downloading image...", "image", img.desc)

		// Use the dynamic timeout here!
		downloadCmd := fmt.Sprintf("curl -sSL -C - --retry 3 --retry-delay 5 --max-time %d -o %s '%s'", timeout, destPath, img.url)
		if _, err := d.exec.Execute(ctx,downloadCmd); err != nil {
			return fmt.Errorf("failed to download %s from %s: %w", img.desc, img.url, err)
		}

		// 5. Final Verification (if checksum is available)
		if expectedHash != "" {
			if !d.verifyFileHash(ctx,destPath, expectedHash) {
				return fmt.Errorf("FATAL: %s checksum mismatch after download", img.desc)
			}
			d.logger.Info("Downloaded and verified", "image", img.desc)
		} else {
			d.logger.Info("Downloaded", "image", img.desc)
		}
	}

	return nil
}

// DownloadOpenShiftTools downloads and extracts installer/client tools with optional checksum validation
func (d *Downloader) DownloadOpenShiftTools(ctx context.Context,workspaceDir string) error {
	d.logger.Info("Downloading OpenShift tools...")

	toolsDir := filepath.Join(workspaceDir, "tools")
	d.exec.Execute(ctx,fmt.Sprintf("mkdir -p %s", toolsDir))

	ocpConfig := d.cfg.OpenShift.OCPClientConfig
	manifestPath := filepath.Join(toolsDir, "sha256sum.txt")
	timeout := d.daemonCfg.Timeouts.DownloadTimeoutSec // Get timeout from config

	// 1. Fetch global manifest ONLY if checksum_url is provided
	if ocpConfig.ChecksumURL != "" {
		d.logger.Info("Integrity Mode: Downloading checksum manifest", "url", ocpConfig.ChecksumURL)

		dlManifestCmd := fmt.Sprintf("curl -sSL --max-time %d -o %s '%s'", timeout, manifestPath, ocpConfig.ChecksumURL)
		if _, err := d.exec.Execute(ctx,dlManifestCmd); err != nil {
			d.logger.Warn("Failed to fetch checksum manifest", "error", err)
		} else {
			d.logger.Info("Checksum manifest downloaded")
		}
	}

	tools := []struct {
		url          string
		filename     string
		desc         string
		specificCSUM string
	}{
		{ocpConfig.Installer, "openshift-install-linux.tar.gz", "OpenShift installer", ocpConfig.InstallerCSUM},
		{ocpConfig.Client, "openshift-client-linux.tar.gz", "OpenShift client", ocpConfig.ClientCSUM},
	}

	for _, tool := range tools {
		if tool.url == "" {
			continue
		}
		destPath := filepath.Join(toolsDir, tool.filename)
		expectedHash := ""

		// 2. Determine Expected Hash
		if tool.specificCSUM != "" {
			expectedHash = tool.specificCSUM
			d.logger.Debug("Using individual checksum", "tool", tool.desc)
		} else if ocpConfig.ChecksumURL != "" {
			hash, err := d.extractHashFromManifest(ctx,tool.url, manifestPath)
			if err == nil && hash != "" {
				expectedHash = hash
				d.logger.Debug("Using manifest checksum", "tool", tool.desc)
			}
		}

		forceDownload := d.cfg.OpenShift.ForceOCPDownload

		if forceDownload {
			d.logger.Info("Force download requested. Wiping existing file...", "file", destPath)
			d.exec.Execute(ctx,fmt.Sprintf("rm -f %s", destPath))
		} else {
			if expectedHash != "" {
				existsCmd := fmt.Sprintf("test -f %s", destPath)
				if _, err := d.exec.Execute(ctx,existsCmd); err == nil {
					if d.verifyFileHash(ctx,destPath, expectedHash) {
						d.logger.Info("Matches checksum, skipping download", "tool", tool.desc)
						continue
					}
					d.logger.Warn("Checksum mismatch. Wiping corrupted file and re-downloading...", "tool", tool.desc)
					d.exec.Execute(ctx,fmt.Sprintf("rm -f %s", destPath))
				}
			} else {
				checkCmd := fmt.Sprintf("test -s %s", destPath)
				if _, err := d.exec.Execute(ctx,checkCmd); err == nil {
					d.logger.Info("File already exists, skipping download (no checksum validation)", "tool", tool.desc)
					continue
				}
			}
		}

		// 4. Download
		d.logger.Info("Downloading tool...", "tool", tool.desc)

		// Use the dynamic timeout here!
		downloadCmd := fmt.Sprintf("curl -sSL -C - --retry 3 --retry-delay 5 --max-time %d -o %s '%s'", timeout, destPath, tool.url)
		if _, err := d.exec.Execute(ctx,downloadCmd); err != nil {
			d.logger.Warn("Failed to download tool", "tool", tool.desc, "error", err)
			continue
		}

		// 5. Final Verification
		if expectedHash != "" {
			if !d.verifyFileHash(ctx,destPath, expectedHash) {
				d.logger.Warn("Checksum mismatch after download", "tool", tool.desc)
				continue
			}
			d.logger.Info("Downloaded and verified", "tool", tool.desc)
		} else {
			d.logger.Info("Downloaded", "tool", tool.desc)
		}
	}

	return d.extractOpenShiftTools(ctx,toolsDir)
}

func (d *Downloader) extractOpenShiftTools(ctx context.Context,toolsDir string) error {
	tools := []string{"openshift-install-linux.tar.gz", "openshift-client-linux.tar.gz"}
	for _, tool := range tools {
		tarPath := filepath.Join(toolsDir, tool)
		if _, err := d.exec.Execute(ctx,fmt.Sprintf("test -s %s", tarPath)); err != nil {
			continue
		}
		extractCmd := fmt.Sprintf("cd %s && tar -xzf %s", toolsDir, tool)
		if _, err := d.exec.Execute(ctx,extractCmd); err != nil {
			return fmt.Errorf("failed to extract %s: %w", tool, err)
		}
	}
	makeExecCmd := fmt.Sprintf("cd %s && chmod +x openshift-install oc kubectl 2>/dev/null || true", toolsDir)
	_, err := d.exec.Execute(ctx,makeExecCmd)
	return err
}

// extractHashFromManifest parses sha256sum.txt for a specific filename
// Uses precise grep pattern to avoid partial matches (e.g., "kernel" vs "my-kernel")
func (d *Downloader) extractHashFromManifest(ctx context.Context,originalURL, manifestPath string) (string, error) {
	filename := filepath.Base(originalURL)
	// Use [[:space:]] to match whitespace and $ to anchor end of line
	// This prevents matching "my-kernel" when looking for "kernel"
	extractCmd := fmt.Sprintf("grep -E '[[:space:]]%s$' %s | awk '{print $1}'", filename, manifestPath)
	hash, err := d.exec.Execute(ctx,extractCmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(hash), nil
}

// verifyFileHash calculates SHA256 hash of a file and compares it to expected hash
func (d *Downloader) verifyFileHash(ctx context.Context,filePath, expectedHash string) bool {
	calcCmd := fmt.Sprintf("sha256sum %s | awk '{print $1}'", filePath)
	actualHash, err := d.exec.Execute(ctx,calcCmd)
	if err != nil {
		return false
	}
	actual := strings.TrimSpace(actualHash)
	expected := strings.TrimSpace(expectedHash)
	return actual == expected
}