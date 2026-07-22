package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/IBM/shiftlaunch/config"
	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
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
func (d *Downloader) DownloadAll(ctx context.Context, workspaceDir string) error {
	if d.cfg.Nodes.BootMethod == "agent" {
		d.logger.Info("Skipping RHCOS image downloads (Agent ISO handles payload dynamically)")
	} else {
		if err := d.DownloadRHCOSImages(ctx, workspaceDir); err != nil {
			return fmt.Errorf("failed to download RHCOS images: %w", err)
		}
	}

	// We ALWAYS need the OpenShift tools (openshift-install, oc, kubectl)
	if err := d.DownloadOpenShiftTools(ctx, workspaceDir); err != nil {
		return fmt.Errorf("failed to download OpenShift tools: %w", err)
	}

	return nil
}

// DownloadRHCOSImages downloads RHCOS images with optional checksum validation
func (d *Downloader) DownloadRHCOSImages(ctx context.Context, workspaceDir string) error {
	d.logger.Info("Downloading RHCOS images to local workspace...")

	rhcosDir := filepath.Join(workspaceDir, "rhcos")
	if err := os.MkdirAll(rhcosDir, 0o755); err != nil {
		return fmt.Errorf("failed to create RHCOS directory %s: %w", rhcosDir, err)
	}

	urls := d.cfg.OpenShift.RHCOSImages
	timeout := d.daemonCfg.Timeouts.DownloadTimeoutSec

	manifestPath := filepath.Join(rhcosDir, "sha256sum.txt")

	// Fetch the checksum manifest if a URL is available (auto-resolved by root.go
	// when rhcos_images is omitted, or explicitly set by the user).
	if urls.ChecksumURL != "" {
		d.logger.Info("Integrity Mode: Fetching fresh RHCOS checksum manifest", "url", urls.ChecksumURL)
		_ = os.Remove(manifestPath)
		dlManifestCmd := fmt.Sprintf("curl -sSL --fail --max-time %d -o %s -- %s", timeout, shellQuote(manifestPath), shellQuote(urls.ChecksumURL))
		if _, err := d.exec.Execute(ctx, dlManifestCmd); err != nil {
			d.logger.Warn("Failed to fetch RHCOS checksum manifest", "error", err)
		} else {
			d.logger.Info("RHCOS checksum manifest downloaded")
		}
	}

	images := []struct {
		url      string
		filename string
		desc     string
	}{
		{urls.KernelURL, "kernel", "RHCOS kernel"},
		{urls.InitramfsURL, "initramfs.img", "RHCOS initramfs"},
		{urls.RootfsURL, "rootfs.img", "RHCOS rootfs"},
	}

	forceDownload := d.cfg.OpenShift.ForceOCPDownload

	for _, img := range images {
		if img.url == "" {
			return fmt.Errorf("%s URL not provided in configuration", img.desc)
		}
		destPath := filepath.Join(rhcosDir, img.filename)

		// Attempt to resolve the expected hash from the manifest; fall back gracefully.
		expectedHash, _ := d.extractHashFromManifest(ctx, img.url, manifestPath)

		skip, wipe := d.resolveDownloadAction(ctx, destPath, expectedHash, forceDownload)
		if skip {
			continue
		}
		if wipe {
			if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
				d.logger.Warn("Failed to remove stale file", "file", destPath, "error", err)
			}
		}

		d.logger.Info("Downloading image...", "image", img.desc)
		downloadCmd := fmt.Sprintf("curl -sSL -C - --retry 3 --retry-delay 5 --max-time %d -o %s -- %s", timeout, shellQuote(destPath), shellQuote(img.url))
		if _, err := d.exec.Execute(ctx, downloadCmd); err != nil {
			return fmt.Errorf("failed to download %s from %s: %w", img.desc, img.url, err)
		}

		if expectedHash != "" {
			if !d.verifyFileHash(ctx, destPath, expectedHash) {
				return fmt.Errorf("%s checksum mismatch after download", img.desc)
			}
			d.logger.Info("Downloaded and verified", "image", img.desc)
		} else {
			d.logger.Info("Downloaded", "image", img.desc)
		}
	}

	return nil
}

// DownloadOpenShiftTools downloads and extracts installer/client tools with optional checksum validation
func (d *Downloader) DownloadOpenShiftTools(ctx context.Context, workspaceDir string) error {
	d.logger.Info("Downloading OpenShift tools...")

	toolsDir := filepath.Join(workspaceDir, "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create tools directory %s: %w", toolsDir, err)
	}

	// Check if the extracted binaries are already here (Airgap mode safety)
	installerPath := filepath.Join(toolsDir, "openshift-install")
	ocPath := filepath.Join(toolsDir, "oc")

	if _, err1 := os.Stat(installerPath); err1 == nil {
		if _, err2 := os.Stat(ocPath); err2 == nil {
			d.logger.Info("Airgap Mode: OpenShift tools already pre-staged in workspace. Skipping download framework.")
			return nil
		}
	}

	ocpConfig := d.cfg.OpenShift.OCPClientConfig
	manifestPath := filepath.Join(toolsDir, "sha256sum.txt")
	timeout := d.daemonCfg.Timeouts.DownloadTimeoutSec // Get timeout from config

	// 1. Fetch global manifest ONLY if checksum_url is provided
	if ocpConfig.ChecksumURL != "" {
		d.logger.Info("Integrity Mode: Fetching fresh checksum manifest", "url", ocpConfig.ChecksumURL)

		// Force wipe any stale manifest to guarantee we get the latest.
		_ = os.Remove(manifestPath)

		dlManifestCmd := fmt.Sprintf("curl -sSL --fail --max-time %d -o %s -- %s", timeout, shellQuote(manifestPath), shellQuote(ocpConfig.ChecksumURL))
		if _, err := d.exec.Execute(ctx, dlManifestCmd); err != nil {
			d.logger.Warn("Failed to fetch checksum manifest", "error", err)
		} else {
			d.logger.Info("Checksum manifest downloaded")
		}
	}

	tools := []struct {
		url      string
		filename string
		desc     string
	}{
		{ocpConfig.Installer, "openshift-install-linux.tar.gz", "OpenShift installer"},
		{ocpConfig.Client, "openshift-client-linux.tar.gz", "OpenShift client"},
		{ocpConfig.MirrorClient, "oc-mirror.tar.gz", "OpenShift mirror plugin"},
	}

	forceDownload := d.cfg.OpenShift.ForceOCPDownload

	for _, tool := range tools {
		if tool.url == "" {
			continue
		}
		destPath := filepath.Join(toolsDir, tool.filename)

		// Attempt to resolve the expected hash from the manifest downloaded above.
		expectedHash, _ := d.extractHashFromManifest(ctx, tool.url, manifestPath)

		skip, wipe := d.resolveDownloadAction(ctx, destPath, expectedHash, forceDownload)
		if skip {
			continue
		}
		if wipe {
			if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
				d.logger.Warn("Failed to remove stale file", "file", destPath, "error", err)
			}
		}

		d.logger.Info("Downloading tool...", "tool", tool.desc)
		downloadCmd := fmt.Sprintf("curl -sSL -C - --retry 3 --retry-delay 5 --max-time %d -o %s -- %s", timeout, shellQuote(destPath), shellQuote(tool.url))
		if _, err := d.exec.Execute(ctx, downloadCmd); err != nil {
			d.logger.Warn("Failed to download tool", "tool", tool.desc, "error", err)
			continue
		}

		if expectedHash != "" {
			if !d.verifyFileHash(ctx, destPath, expectedHash) {
				d.logger.Warn("Checksum mismatch after download", "tool", tool.desc)
				continue
			}
			d.logger.Info("Downloaded and verified", "tool", tool.desc)
		} else {
			d.logger.Info("Downloaded", "tool", tool.desc)
		}
	}

	return d.extractOpenShiftTools(ctx, toolsDir)
}

func (d *Downloader) extractOpenShiftTools(ctx context.Context, toolsDir string) error {
	shieldedCtx := context.WithoutCancel(ctx)

	// Extract each archive that is present and non-empty.
	tools := []string{"openshift-install-linux.tar.gz", "openshift-client-linux.tar.gz", "oc-mirror.tar.gz"}
	for _, tool := range tools {
		tarPath := filepath.Join(toolsDir, tool)
		if _, err := d.exec.Execute(shieldedCtx, "test -s "+shellQuote(tarPath)); err != nil {
			continue
		}
		extractCmd := fmt.Sprintf("tar -xzf %s -C %s", shellQuote(tarPath), shellQuote(toolsDir))
		if _, err := d.exec.Execute(shieldedCtx, extractCmd); err != nil {
			return fmt.Errorf("failed to extract %s: %w", tool, err)
		}
	}

	// Make all extracted binaries executable.
	makeExecCmd := fmt.Sprintf(
		"chmod +x %s %s %s %s 2>/dev/null || true",
		shellQuote(filepath.Join(toolsDir, "openshift-install")),
		shellQuote(filepath.Join(toolsDir, "oc")),
		shellQuote(filepath.Join(toolsDir, "kubectl")),
		shellQuote(filepath.Join(toolsDir, "oc-mirror")),
	)
	_, err := d.exec.Execute(shieldedCtx, makeExecCmd)
	return err
}

// extractHashFromManifest parses sha256sum.txt for a specific filename
// Uses precise grep pattern to avoid partial matches (e.g., "kernel" vs "my-kernel")
func (d *Downloader) extractHashFromManifest(_ context.Context, originalURL, manifestPath string) (string, error) {
	// Strip query parameters (e.g. signed S3 URLs) before extracting the basename.
	cleanURL := strings.SplitN(originalURL, "?", 2)[0]
	filename := filepath.Base(cleanURL)

	if _, err := os.Stat(manifestPath); err != nil {
		return "", fmt.Errorf("manifest not found: %s", manifestPath)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest %s: %w", manifestPath, err)
	}

	// Each sha256sum line: "<hash>  <filename>" — match last field to avoid
	// partial name collisions (e.g. "kernel" matching "my-kernel").
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[len(fields)-1] == filename {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("filename %q not found in manifest", filename)
}

// resolveDownloadAction inspects the file at destPath and returns the action
// the caller should take before attempting a download, based on the force flag,
// checksum availability, and the file's current state on disk.
//
// Returns:
//   - skip=true  → file is present and verified; the caller should skip the download.
//   - wipe=true  → file is stale or corrupt; the caller must remove it before downloading.
func (d *Downloader) resolveDownloadAction(ctx context.Context, destPath, expectedHash string, forceDownload bool) (skip, wipe bool) {
	if forceDownload {
		d.logger.Info("Force download requested. Wiping existing file...", "file", destPath)
		return false, true
	}

	fi, err := os.Stat(destPath)
	fileExists := err == nil

	if expectedHash != "" {
		if !fileExists {
			return false, false // nothing on disk yet; proceed to download
		}
		if d.verifyFileHash(ctx, destPath, expectedHash) {
			d.logger.Info("Checksum matches, skipping download", "file", destPath)
			return true, false
		}
		d.logger.Warn("Checksum mismatch. Wiping corrupted file and re-downloading...", "file", destPath)
		return false, true
	}

	// No checksum available. Guard against S3-backed mirrors that return HTTP 200
	// on a Range request, causing curl -C - to silently re-download the whole file.
	// Skip only if the file is already non-empty; a truncated partial can be
	// recovered with ForceOCPDownload=true.
	if fileExists && fi.Size() > 0 {
		d.logger.Info("File exists, no checksum configured — skipping re-download", "file", destPath)
		return true, false
	}

	return false, false
}

// verifyFileHash computes the SHA-256 digest of filePath in pure Go and
// compares it against expectedHash (lowercase hex). Returns false on any error.
func (d *Downloader) verifyFileHash(_ context.Context, filePath, expectedHash string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}

	actual := hex.EncodeToString(h.Sum(nil))
	return actual == strings.TrimSpace(expectedHash)
}

