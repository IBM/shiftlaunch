package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"github.ibm.com/sudeeshjohn/shiftlaunch/localexec"
	"github.ibm.com/sudeeshjohn/shiftlaunch/logger"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
	"gopkg.in/yaml.v3"
)

// The template translates the ansible jinja2 loops into static rules tailored for the cluster
const squidConfTemplate = `# ============================================
# Squid Proxy Configuration for ShiftLaunch
# Cluster: {{.ClusterName}}
# Allowed Network: {{.MachineCIDR}}
# ============================================

# ACLS
acl localnet src {{.MachineCIDR}}
acl localnet src {{.ControllerIP}}/32   # THE FIX: Whitelist the Controller node!

acl SSL_ports port 443
acl SSL_ports port 5000         # Local Registry
acl SSL_ports port 6443         # OpenShift API
acl SSL_ports port 22623        # OpenShift Machine Config

acl Safe_ports port 80          # http
acl Safe_ports port 21          # ftp
acl Safe_ports port 443         # https
acl Safe_ports port 70          # gopher
acl Safe_ports port 210         # wais
acl Safe_ports port 1025-65535  # unregistered ports
acl Safe_ports port 280         # http-mgmt
acl Safe_ports port 488         # gss-http
acl Safe_ports port 591         # filemaker
acl Safe_ports port 777         # multiling http
acl CONNECT method CONNECT

# HTTP ACCESS
http_access deny !Safe_ports
http_access deny CONNECT !SSL_ports
http_access allow localhost manager
http_access deny manager

http_access allow localnet
http_access allow localhost
http_access deny all

http_port 3128

coredump_dir /var/spool/squid
refresh_pattern ^ftp:           1440    20%     10080
refresh_pattern ^gopher:        1440    0%      1440
refresh_pattern -i (/cgi-bin/|\?) 0     0%      0
refresh_pattern .               0       20%     4320
`

// The systemd drop-in configuration from your Ansible files/restart.conf
const squidRestartConf = `[Service]
Restart=always
RestartSec=30
`

// SquidManager handles Squid proxy server configuration for controlled internet access
type SquidManager struct {
	cfg          *types.AgentConfig
	executor     *localexec.LocalClient
	logger       *logger.Logger
	workspaceDir string
}

// NewSquidManager initializes the proxy configurator
func NewSquidManager(cfg *types.AgentConfig, exec *localexec.LocalClient, log *logger.Logger, workspaceDir string) *SquidManager {
	return &SquidManager{
		cfg:          cfg,
		executor:     exec,
		logger:       log,
		workspaceDir: workspaceDir,
	}
}

// Setup installs, configures, and starts the Squid proxy securely
func (s *SquidManager) Setup(ctx context.Context) error {
	s.logger.Info("Setting up local Squid proxy server...")
	
	// CRITICAL: Shield from cancellation to prevent broken RPM databases or half-written firewall rules
	shieldedCtx := context.WithoutCancel(ctx)

	// 1. Install Squid (Ansible: Install Squid package)
	s.logger.Debug("Installing squid package via dnf...")
	if _, err := s.executor.Execute(shieldedCtx, "sudo dnf install -y squid"); err != nil {
		return fmt.Errorf("failed to install squid: %w", err)
	}

	// 2. Generate Configuration (Ansible: Configure Squid)
	s.logger.Debug("Generating squid.conf restricted to cluster CIDR...")
	tmpl, err := template.New("squid").Parse(squidConfTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse squid template: %w", err)
	}

	data := struct {
		ClusterName  string
		MachineCIDR  string
		ControllerIP string
	}{
		ClusterName:  s.cfg.OpenShift.ClusterName,
		MachineCIDR:  s.cfg.Network.MachineCIDR,
		ControllerIP: s.cfg.Controller.IP, // THE FIX: Map the IP to the template
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to execute squid template: %w", err)
	}

	if err := s.executor.WriteFile(shieldedCtx, "/etc/squid/squid.conf", buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write squid.conf: %w", err)
	}

	// 3. Systemd Drop-in (Ansible: Create dropin directory & Copy restart conf)
	s.logger.Debug("Configuring systemd restart drop-in for Squid...")
	if _, err := s.executor.Execute(shieldedCtx, "sudo mkdir -p /etc/systemd/system/squid.service.d"); err != nil {
		return fmt.Errorf("failed to create systemd drop-in directory: %w", err)
	}
	if err := s.executor.WriteFile(shieldedCtx, "/etc/systemd/system/squid.service.d/restart.conf", []byte(squidRestartConf), 0644); err != nil {
		return fmt.Errorf("failed to write systemd restart drop-in: %w", err)
	}

	// 4. Configure Firewall & SELinux (Ansible: Add Squid to firewall)
	s.logger.Debug("Configuring firewall and SELinux for Squid proxy...")
	
	if _, err := s.executor.Execute(shieldedCtx, "sudo firewall-cmd --permanent --add-service=squid"); err != nil {
		return fmt.Errorf("failed to add firewall rule for squid: %w", err)
	}
	if _, err := s.executor.Execute(shieldedCtx, "sudo firewall-cmd --reload"); err != nil {
		return fmt.Errorf("failed to reload firewall: %w", err)
	}

	// CRITICAL: Allow Squid to proxy traffic to OpenShift's non-standard ports (6443, 22623, 5000)
	if _, err := s.executor.Execute(shieldedCtx, "sudo setsebool -P squid_connect_any 1"); err != nil {
		s.logger.Warn("Failed to set SELinux boolean squid_connect_any. Proxying to OpenShift API ports may fail.", "error", err)
	}

	// 5. Enable and Start Service
	s.logger.Debug("Reloading systemd and restarting squid service...")
	if _, err := s.executor.Execute(shieldedCtx, "sudo systemctl daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w", err)
	}
	if err := s.executor.SystemctlEnable(shieldedCtx, "squid"); err != nil {
		return fmt.Errorf("failed to enable squid service: %w", err)
	}
	if err := s.executor.SystemctlRestart(shieldedCtx, "squid"); err != nil {
		return fmt.Errorf("failed to restart squid service: %w", err)
	}

	s.logger.Info("Squid proxy server configured successfully on port 3128")
	return nil
}

// Cleanup removes firewall rules and stops the proxy during teardown
func (s *SquidManager) Cleanup(ctx context.Context) error {
	// THE FIX: Check for multi-tenancy before knocking the proxy server offline!
	if s.isProxyShared() {
		s.logger.Info("Local Squid proxy is actively being used by other managed clusters. Bypassing proxy teardown.")
		return nil
	}

	s.logger.Info("Cleaning up local Squid proxy...")
	shieldedCtx := context.WithoutCancel(ctx)

	// Remove firewall rule
	s.executor.Execute(shieldedCtx, "sudo firewall-cmd --permanent --remove-service=squid")
	s.executor.Execute(shieldedCtx, "sudo firewall-cmd --reload")

	// Stop and disable service
	s.executor.Execute(shieldedCtx, "sudo systemctl stop squid")
	s.executor.Execute(shieldedCtx, "sudo systemctl disable squid")

	s.logger.Info("Squid proxy cleanup complete")
	return nil
}

// isProxyShared checks if other managed clusters are currently using the local Squid proxy
func (s *SquidManager) isProxyShared() bool {
	workspaceParent := filepath.Dir(s.workspaceDir)
	
	entries, err := os.ReadDir(workspaceParent)
	if err != nil {
		return false
	}

	activeCount := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		
		clusterName := entry.Name()
		if clusterName == s.cfg.OpenShift.ClusterName {
			continue // Skip our own cluster
		}

		// A cluster is active if it has a config file and is NOT marked as deleted
		deletedMarker := filepath.Join(workspaceParent, clusterName, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			continue
		}

		configPath := filepath.Join(workspaceParent, clusterName, "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		// Parse the config to see if it relies on the managed proxy gateway
		var tmpCfg types.AgentConfig
		if err := yaml.Unmarshal(data, &tmpCfg); err == nil {
			if tmpCfg.ManagedServices.Proxy {
				activeCount++
			}
		}
	}

	return activeCount > 0
}