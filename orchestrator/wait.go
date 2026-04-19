package orchestrator

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pterm/pterm"
)

// WaitForBootstrap waits for the OpenShift bootstrap process to complete
func (o *Orchestrator) WaitForBootstrap(cancelCtx context.Context) error {
	timeoutSecs := 1800 // Default 30 minutes

	spinnerText := fmt.Sprintf("Waiting for bootstrap to complete (%d min timeout, may take 20-30 minutes)...", timeoutSecs/60)
	spinner, _ := pterm.DefaultSpinner.WithWriter(o.logger.TerminalOnly()).Start(spinnerText)
	defer spinner.Stop()

	o.logger.Info(fmt.Sprintf("Timeout: %d seconds (%d minutes)", timeoutSecs, timeoutSecs/60))
	o.logger.Info("Executing: openshift-install wait-for bootstrap-complete")

	timeoutCtx, cancel := context.WithTimeout(cancelCtx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	installerPath := filepath.Join(o.workspaceDir, "tools", "openshift-install")
	targetDir := filepath.Join(o.workspaceDir, "install-dir") // <--- FIX: Point to the new directory

	cmd := exec.CommandContext(timeoutCtx, installerPath, "wait-for", "bootstrap-complete", "--dir", targetDir, "--log-level=info")

	output, cmdErr := cmd.CombinedOutput()

	if cmdErr != nil {
		spinner.Fail("Bootstrap failed!")
		o.logger.Error(fmt.Sprintf("\n❌ Bootstrap failed:\n%s\n", string(output)))
		return fmt.Errorf("bootstrap completion failed: %w", cmdErr)
	}

	spinner.Success("Bootstrap Complete!")
	o.logger.Info(fmt.Sprintf("\n✓ Bootstrap Complete!\n%s\n", string(output)))

	if !o.cfg.IsSNO() {
		pterm.Info.WithWriter(o.logger.TerminalOnly()).Println("Bootstrap node can now be powered off. The cluster will continue installation without it.")
	}

	return nil
}

// WaitForInstall waits for installation to complete and auto-approves worker CSRs
func (o *Orchestrator) WaitForInstall(cancelCtx context.Context) error {
	timeoutSecs := 3600 // Default 60 minutes

	var timeEstimate string
	if o.cfg.IsSNO() {
		timeEstimate = "30-45 minutes"
	} else {
		timeEstimate = "30-60 minutes"
	}

	spinnerText := fmt.Sprintf("Waiting for OpenShift installation to complete (%d min timeout, may take %s)...", timeoutSecs/60, timeEstimate)
	spinner, _ := pterm.DefaultSpinner.WithWriter(o.logger.TerminalOnly()).Start(spinnerText)
	defer spinner.Stop()

	o.logger.Info(fmt.Sprintf("Timeout: %d seconds (%d minutes)", timeoutSecs, timeoutSecs/60))
	o.logger.Info("Executing: openshift-install wait-for install-complete")

	timeoutCtx, cancel := context.WithTimeout(cancelCtx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	// Start the background CSR approver for Multi-Node clusters
	if !o.cfg.IsSNO() {
		go o.autoApproveCSRs(timeoutCtx)
	}

	installerPath := filepath.Join(o.workspaceDir, "tools", "openshift-install")
	targetDir := filepath.Join(o.workspaceDir, "install-dir") // <--- FIX: Point to the new directory

	cmd := exec.CommandContext(timeoutCtx, installerPath, "wait-for", "install-complete", "--dir", targetDir, "--log-level=info")

	output, cmdErr := cmd.CombinedOutput()

	if cmdErr != nil {
		spinner.Fail("Installation failed!")
		o.logger.Error(fmt.Sprintf("Installation failed:\n%s", string(output)))
		return fmt.Errorf("installation completion failed: %w", cmdErr)
	}

	spinner.Success("Installation Complete!")
	return nil
}

// autoApproveCSRs runs in the background and automatically approves worker CSRs locally
func (o *Orchestrator) autoApproveCSRs(ctx context.Context) {
	o.logger.Debug("  [CSR Auto-Approver] Background thread started...")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// <--- FIX: Point to the install-dir auth directory
	kubeconfigPath := filepath.Join(o.workspaceDir, "install-dir", "auth", "kubeconfig") 
	ocPath := filepath.Join(o.workspaceDir, "tools", "oc")

	// Local pipeline execution: oc get csr | grep pending | xargs oc adm certificate approve
	approveCmd := fmt.Sprintf(
		"export KUBECONFIG=%s && "+
			"%s get csr -o go-template='{{range .items}}{{if not .status}}{{.metadata.name}}{{\"\\n\"}}{{end}}{{end}}' | "+
			"xargs --no-run-if-empty %s adm certificate approve",
		kubeconfigPath, ocPath, ocPath,
	)

	for {
		select {
		case <-ctx.Done():
			o.logger.Debug("  [CSR Auto-Approver] Background thread stopped.")
			return
		case <-ticker.C:
			cmd := exec.CommandContext(ctx, "bash", "-c", approveCmd)
			output, err := cmd.CombinedOutput()
			
			if err == nil && strings.Contains(string(output), "approved") {
				// Safely print above the spinner on the terminal
				pterm.Info.WithWriter(o.logger.TerminalOnly()).Println("Approved pending worker CSRs")
				
				// Send the raw output to the debug log file silently
				o.logger.Debug(fmt.Sprintf("CSR Auto-Approver details:\n%s", string(output)))
			}
		}
	}
}