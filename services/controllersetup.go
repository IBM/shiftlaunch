package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/sudeeshjohn/shiftlaunch/config"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

// ControllerSetup manages the local packages and firewalls on the machine running the agent
type ControllerSetup struct {
	cfg       *types.AgentConfig
	daemonCfg *config.AgentDaemonConfig
	executor  *localexec.LocalClient
	logger    *logger.Logger
}

func NewControllerSetup(cfg *types.AgentConfig, daemonCfg *config.AgentDaemonConfig, executor *localexec.LocalClient, log *logger.Logger) *ControllerSetup {
	return &ControllerSetup{
		cfg:       cfg,
		daemonCfg: daemonCfg,
		executor:  executor,
		logger:    log,
	}
}

// getRequiredPackages figures out what dnf packages we need based on the YAML toggles
func (c *ControllerSetup) getRequiredPackages() []string {
	var pkgs []string
	
	// We always need firewalld for port management
	pkgs = append(pkgs, "firewalld", "policycoreutils-python-utils")

	// HTTPD is only needed for netboot staging
	if c.cfg.Nodes.BootMethod != "iso" {
		pkgs = append(pkgs, "httpd")
	}

	needsDHCP := c.cfg.ManagedServices.DHCP && c.cfg.Nodes.BootMethod != "iso"
	needsPXE := c.cfg.ManagedServices.PXE && c.cfg.Nodes.BootMethod != "iso"

	if c.cfg.ManagedServices.DNS || needsDHCP || needsPXE {
		pkgs = append(pkgs, "dnsmasq")
	}
	if needsPXE {
		pkgs = append(pkgs, "tftp-server", "syslinux-tftpboot", "grub2-tools-extra")
	}
	if c.cfg.ManagedServices.LoadBalancer {
		pkgs = append(pkgs, "haproxy")
	}
	if c.cfg.Nodes.BootMethod == "iso" && c.cfg.ManagedServices.NFS {
		pkgs = append(pkgs, "nfs-utils")
	}
	// nmstate is required for Agent ISO with static networking validation
	if c.cfg.Nodes.BootMethod == "iso" {
		pkgs = append(pkgs, "nmstate")
	}

	return pkgs
}

// InstallPackages uses localexec to run dnf install
func (c *ControllerSetup) InstallPackages(ctx context.Context) error {
	pkgs := c.getRequiredPackages()
	c.logger.Info("Installing required local packages...", "packages", strings.Join(pkgs, ", "))

	installCmd := fmt.Sprintf("sudo dnf install -y %s", strings.Join(pkgs, " "))
	if _, err := c.executor.Execute(ctx,installCmd); err != nil {
		return fmt.Errorf("failed to install local packages: %w", err)
	}

	c.logger.Info("Packages installed successfully")
	return nil
}

// ConfigureFirewall opens the required ports locally based on the YAML toggles
func (c *ControllerSetup) ConfigureFirewall(ctx context.Context) error {
	c.logger.Info("Configuring local firewall...")

	if _, err := c.executor.Execute(ctx, "sudo systemctl enable --now firewalld"); err != nil {
		return fmt.Errorf("failed to start firewalld: %w", err)
	}

	var ports []string
	var services []string
	
	// HTTP for Ignition is only needed for netboot
	if c.cfg.Nodes.BootMethod != "iso" {
		ports = append(ports, fmt.Sprintf("%d/tcp", c.daemonCfg.Network.HTTPPort))
	}

	if c.cfg.ManagedServices.DNS {
		ports = append(ports, "53/tcp", "53/udp")
	}
	if c.cfg.ManagedServices.DHCP && c.cfg.Nodes.BootMethod != "iso" {
		ports = append(ports, "67/udp")
	}
	if c.cfg.ManagedServices.PXE && c.cfg.Nodes.BootMethod != "iso" {
		ports = append(ports, "69/udp")
	}
	if c.cfg.ManagedServices.LoadBalancer {
		ports = append(ports, "6443/tcp", "22623/tcp", "80/tcp", "443/tcp")
	}
	if c.cfg.Nodes.BootMethod == "iso" && c.cfg.ManagedServices.NFS {
		services = append(services, "nfs", "rpc-bind", "mountd")
	}

	// --- FIX: Apply Ports and Services in separate isolated commands ---

	// 1. Apply Ports
	if len(ports) > 0 {
		portArgs := ""
		for _, port := range ports {
			portArgs += fmt.Sprintf(" --add-port=%s", port)
		}
		if _, err := c.executor.Execute(ctx, "sudo firewall-cmd --permanent"+portArgs); err != nil {
			return fmt.Errorf("failed to add firewall port rules: %w", err)
		}
	}

	// 2. Apply Services
	if len(services) > 0 {
		svcArgs := ""
		for _, svc := range services {
			svcArgs += fmt.Sprintf(" --add-service=%s", svc)
		}
		if _, err := c.executor.Execute(ctx, "sudo firewall-cmd --permanent"+svcArgs); err != nil {
			return fmt.Errorf("failed to add firewall service rules: %w", err)
		}
	}

	// 3. Reload to apply changes
	if _, err := c.executor.Execute(ctx, "sudo firewall-cmd --reload"); err != nil {
		return fmt.Errorf("failed to reload firewall: %w", err)
	}

	c.logger.Info("Local firewall configured successfully")
	return nil
}