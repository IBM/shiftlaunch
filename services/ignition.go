package services

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

const installConfigTemplate = `apiVersion: v1
baseDomain: {{.BaseDomain}}
compute:
{{- if .IsSNO}}
- name: worker
  replicas: 0
{{- else}}
- hyperthreading: Enabled
  name: worker
  replicas: {{.WorkerReplicas}}
  architecture: ppc64le
{{- end}}
controlPlane:
{{- if .IsSNO}}
  name: master
  replicas: 1
{{- else}}
  hyperthreading: Enabled
  name: master
  replicas: {{.MasterReplicas}}
  architecture: ppc64le
{{- end}}
metadata:
  name: {{.ClusterName}}
networking:
  clusterNetwork:
  - cidr: {{.ClusterNetworkCIDR}}
    hostPrefix: {{.ClusterNetworkHostPrefix}}
  machineNetwork:
  - cidr: {{.MachineNetwork}}
  networkType: OVNKubernetes
  serviceNetwork:
  - {{.ServiceNetwork}}
platform:
  none: {}
{{- if .IsSNO}}
bootstrapInPlace:
  installationDisk: {{.DiskDevice}}
{{- else}}
fips: false
{{- end}}
pullSecret: '{{.PullSecret}}'
sshKey: '{{.SSHKey}}'
`

// agentConfigTemplate works for both SNO and multi-node clusters
// Matches the official OpenShift Agent-based Installer format
const agentConfigTemplate = `apiVersion: v1alpha1
kind: AgentConfig
metadata:
  name: {{.ClusterName}}
rendezvousIP: {{.RendezvousIP}}
{{- if .Hosts}}
hosts:
{{- range .Hosts}}
  - hostname: {{.Hostname}}
    role: {{.Role}}
    interfaces:
      - name: env2
        macAddress: {{.MACAddress}}
    networkConfig:
      interfaces:
        - name: env2
          type: ethernet
          state: up
          ipv4:
            enabled: true
            address:
              - ip: {{.IP}}
                prefix-length: {{.PrefixLength}}
      dns-resolver:
        config:
          server:
            - {{.DNS}}
      routes:
        config:
          - destination: 0.0.0.0/0
            next-hop-address: {{.Gateway}}
            next-hop-interface: env2
{{- end}}
{{- end}}
`

func GenerateIgnition(ctx context.Context, cfg *types.AgentConfig, exec *localexec.LocalClient, workspaceDir string) error {
	// Branch based on boot method
	if cfg.Nodes.BootMethod == "iso" {
		return generateAgentISO(ctx, cfg, exec, workspaceDir)
	}

	// Existing netboot ignition generation
	return generateNetbootIgnition(ctx, cfg, exec, workspaceDir)
}

// generateNetbootIgnition handles traditional netboot ignition generation
func generateNetbootIgnition(ctx context.Context, cfg *types.AgentConfig, exec *localexec.LocalClient, workspaceDir string) error {
	// --- NEW: Define and create the install-dir target directory ---
	targetDir := filepath.Join(workspaceDir, "install-dir")
	exec.Execute(ctx,fmt.Sprintf("mkdir -p %s", targetDir))

	// 1. Generate install-config.yaml
	installConfig, err := generateInstallConfigYAML(cfg)
	if err != nil {
		return err
	}

	// Write to the new install-dir directory
	configPath := filepath.Join(targetDir, "install-config.yaml")
	if err := os.WriteFile(configPath, []byte(installConfig), 0644); err != nil {
		return fmt.Errorf("failed to write install-config.yaml: %w", err)
	}

	// Backup the config because openshift-install consumes it
	exec.Execute(ctx,fmt.Sprintf("cp %s %s.bak", configPath, configPath))

	// 2. Run openshift-install locally
	// Note: The installer binary is still in the parent workspace's tools directory
	installerPath := filepath.Join(workspaceDir, "tools", "openshift-install")

	var cmd string
	if cfg.IsSNO() {
		// cd into targetDir before running the installer
		cmd = fmt.Sprintf("cd %s && %s create single-node-ignition-config --dir=.", targetDir, installerPath)
	} else {
		// cd into targetDir before running the installer
		cmd = fmt.Sprintf("cd %s && %s create ignition-configs --dir=.", targetDir, installerPath)
	}

	if _, err := exec.Execute(ctx,cmd); err != nil {
		return fmt.Errorf("failed to create ignition configs: %w", err)
	}

/* 	// 3. Stage files for HTTP hosting
	httpDir := filepath.Join(workspaceDir, "http")
	exec.Execute(fmt.Sprintf("mkdir -p %s/ignition", httpDir))
	exec.Execute(fmt.Sprintf("cp -r %s/rhcos %s/", workspaceDir, httpDir))

	if cfg.IsSNO() {
		// Copy from targetDir
		exec.Execute(fmt.Sprintf("cp %s/bootstrap-in-place-for-live-iso.ign %s/ignition/bootstrap.ign", targetDir, httpDir))
	} else {
		// Copy from targetDir
		exec.Execute(fmt.Sprintf("cp %s/*.ign %s/ignition/", targetDir, httpDir))
	} */


	return nil
}

func generateInstallConfigYAML(cfg *types.AgentConfig) (string, error) {
	tmpl, err := template.New("installConfig").Parse(installConfigTemplate)
	if err != nil {
		return "", err
	}

	pullSecret, _ := os.ReadFile(cfg.OpenShift.PullSecretFile)
	sshKey, _ := os.ReadFile(os.ExpandEnv(strings.ReplaceAll(cfg.OpenShift.SSHPublicKeyFile, "~", "$HOME")))

	data := struct {
		BaseDomain               string
		WorkerReplicas           int
		MasterReplicas           int
		ClusterName              string
		ClusterNetworkCIDR       string
		ClusterNetworkHostPrefix int
		ServiceNetwork           string
		MachineNetwork           string
		IsSNO                    bool
		DiskDevice               string
		PullSecret               string
		SSHKey                   string
	}{
		BaseDomain:               cfg.OpenShift.BaseDomain,
		WorkerReplicas:           len(cfg.Nodes.Workers),
		MasterReplicas:           len(cfg.Nodes.Masters),
		ClusterName:              cfg.OpenShift.ClusterName,
		ClusterNetworkCIDR:       cfg.OpenShift.ClusterNetworkCIDR,
		ClusterNetworkHostPrefix: cfg.OpenShift.HostPrefix,
		ServiceNetwork:           cfg.OpenShift.ServiceNetwork,
		MachineNetwork:           cfg.Network.MachineCIDR,
		IsSNO:                    cfg.IsSNO(),
		DiskDevice:               "/dev/sda", // Default disk device for SNO
		PullSecret:               strings.TrimSpace(string(pullSecret)),
		SSHKey:                   strings.TrimSpace(string(sshKey)),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
// generateAgentISO creates an Agent-based Installer ISO for both SNO and multi-node clusters
func generateAgentISO(ctx context.Context, cfg *types.AgentConfig, exec *localexec.LocalClient, workspaceDir string) error {
	targetDir := filepath.Join(workspaceDir, "install-dir")
	exec.Execute(ctx, fmt.Sprintf("mkdir -p %s", targetDir))

	// 1. Generate install-config.yaml
	installConfig, err := generateInstallConfigYAML(cfg)
	if err != nil {
		return err
	}

	configPath := filepath.Join(targetDir, "install-config.yaml")
	if err := os.WriteFile(configPath, []byte(installConfig), 0644); err != nil {
		return fmt.Errorf("failed to write install-config.yaml: %w", err)
	}

	exec.Execute(ctx, fmt.Sprintf("cp %s %s.bak", configPath, configPath))

	// 2. Generate agent-config.yaml
	agentConfig, err := generateAgentConfigYAML(cfg)
	if err != nil {
		return err
	}

	agentConfigPath := filepath.Join(targetDir, "agent-config.yaml")
	if err := os.WriteFile(agentConfigPath, []byte(agentConfig), 0644); err != nil {
		return fmt.Errorf("failed to write agent-config.yaml: %w", err)
	}

	// 3. Run openshift-install agent create image
	toolsDir := filepath.Join(workspaceDir, "tools")
	installerPath := filepath.Join(toolsDir, "openshift-install")
	
	// Prepend the tools directory to the PATH just for this command execution
	cmd := fmt.Sprintf("export PATH=%s:$PATH && cd %s && %s agent create image --dir=. --log-level=info", toolsDir, targetDir, installerPath)

	if _, err := exec.Execute(ctx, cmd); err != nil {
		return fmt.Errorf("failed to create agent ISO: %w", err)
	}

	return nil
}

// generateAgentConfigYAML creates the agent-config.yaml for Agent-based Installer
// Works for both SNO and multi-node clusters
func generateAgentConfigYAML(cfg *types.AgentConfig) (string, error) {
	tmpl, err := template.New("agentConfig").Parse(agentConfigTemplate)
	if err != nil {
		return "", err
	}

	// Calculate prefix length from CIDR
	_, ipNet, err := net.ParseCIDR(cfg.Network.MachineCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid machine CIDR: %w", err)
	}
	prefixLen, _ := ipNet.Mask.Size()

	// Get DNS server based on managed services configuration
	// If DNS is managed by shiftlaunch, use controller IP as DNS resolver
	// Otherwise, use the configured nameserver
	dnsServer := cfg.Network.Nameserver
	if cfg.ManagedServices.DNS {
		dnsServer = cfg.Controller.IP
	}

	// Build host configurations for all nodes (SNO or multi-node)
	type HostConfig struct {
		Hostname     string
		Role         string
		MACAddress   string
		IP           string
		PrefixLength int
		Gateway      string
		DNS          string
	}

	var hosts []HostConfig
	for _, node := range cfg.GetAllNodes() {
		role := "master"
		if node.Role == "worker" {
			role = "worker"
		}
		
		hosts = append(hosts, HostConfig{
			Hostname:     node.Hostname,
			Role:         role,
			MACAddress:   node.MACAddress,
			IP:           node.IP,
			PrefixLength: prefixLen,
			Gateway:      cfg.Network.Gateway,
			DNS:          dnsServer,
		})
	}

	// Get rendezvous IP (first master/SNO node)
	rendezvousIP := ""
	if len(hosts) > 0 {
		rendezvousIP = hosts[0].IP
	}

	data := struct {
		ClusterName  string
		RendezvousIP string
		Hosts        []HostConfig
	}{
		ClusterName:  cfg.OpenShift.ClusterName,
		RendezvousIP: rendezvousIP,
		Hosts:        hosts,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}