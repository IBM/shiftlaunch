package cmd

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.ibm.com/sudeeshjohn/shiftlaunch/logger"
)

var (
	genConfigType string
	genBootMethod string
	genOutputPath string
)

var generateConfigCmd = &cobra.Command{
	Use:   "create-template",
	Short: "Create a starter config.yaml template",
	GroupID: "utils",
	Long: `Creates a starter configuration template based on topology and boot method.

The create-template command creates:
- A cluster configuration file (config.yaml)
- An agent daemon configuration file (agent.yaml) if it doesn't exist

Supported topologies:
- sno: Single Node OpenShift
- multi: Multi-node cluster (3 masters + workers)

Supported boot methods:
- agent: Agent-based installer
- netboot: Network boot (PXE/TFTP)`,
	RunE: runGenerateConfig,
}

func init() {
	rootCmd.AddCommand(generateConfigCmd)

	generateConfigCmd.Flags().StringVarP(&genConfigType, "type", "t", "sno", "Cluster topology: 'sno' or 'multi'")
	generateConfigCmd.Flags().StringVarP(&genBootMethod, "boot", "b", "agent", "Boot method: 'agent' or 'netboot'")
	generateConfigCmd.Flags().StringVarP(&genOutputPath, "output", "o", "config.yaml", "Path to save the generated file")
}

func runGenerateConfig(cmd *cobra.Command, args []string) error {
	return GenerateConfig(genConfigType, genBootMethod, genOutputPath)
}

const configTemplate = `# =============================================================================
# ShiftLaunch Agent Configuration Template
# Topology: {{if .IsSNO}}SNO (Single Node OpenShift){{else}}Multi-Node Cluster{{end}}
# Boot Method: {{if eq .BootMethod "agent"}}Agent Installer{{else}}Network Boot (PXE){{end}}
# =============================================================================
# INSTRUCTIONS:
# 1. Review and modify the values below to match your infrastructure.
# 2. Ensure the controller node has the specified 'network_interface' active.
# 3. Ensure the LPARs specified under 'existing_lpar_name' are already 
#    created on the HMC.
# =============================================================================

# -----------------------------------------------------------------------------
# 1. MANAGED SERVICES (The "Who")
# Tell the Agent which services to install and manage locally on this machine.
# -----------------------------------------------------------------------------
managed_services:
  # Setup local dnsmasq to answer for the cluster domain (api, *.apps, etc.)
  dns: true
  
  # Setup local DHCP to assign static IPs to LPARs based on MAC addresses
  dhcp: {{if eq .BootMethod "agent"}}false{{else}}true{{end}}
  
  # Setup local TFTP server (Required for Netboot, ignored for Agent boot)
  pxe: {{if eq .BootMethod "netboot"}}true{{else}}false{{end}}
  
  # Setup local HAProxy to route traffic for the loadbalancer_ip (VIP)
  load_balancer: true
  
  # Setup local NFS server to host Agent images to the VIOS (Required for Agent boot)
  nfs: {{if eq .BootMethod "agent"}}true{{else}}false{{end}}

# -----------------------------------------------------------------------------
# 2. CONTROLLER NODE (The "Where")
# -----------------------------------------------------------------------------
controller:
  # The physical network interface on this machine where the VIP will be bound
  # Example: "eth0", "enP1p1s0f0", "env2"
  network_interface: "eth0"

# -----------------------------------------------------------------------------
# 3. HMC CREDENTIALS
# -----------------------------------------------------------------------------
hmc:
  ip: "10.20.x.x"
  username: "YOUR_HMC_USERNAME"
  password: "password"

# -----------------------------------------------------------------------------
# 4. NETWORK CONFIGURATION
# -----------------------------------------------------------------------------
network:
  # The Virtual IP (VIP) for the cluster. If managed_services.load_balancer is
  # true, ShiftLaunch will automatically alias this IP to the controller interface.
  loadbalancer_ip: "10.20.x.y"
  
  # The subnet where the OpenShift nodes reside
  machine_network_cidr: "10.20.x.0/24" 
  
  # The gateway for the machine network
  gateway: "10.20.x.1"
  
  # Upstream DNS server. Leave empty ("") if managed_services.dns is true,
  # as ShiftLaunch will act as the primary nameserver.
  nameserver: ""
  
  # External DNS servers for resolving public domains (e.g., quay.io)
  dns_forwarders:
    - "198.51.100.1"
    - "198.51.100.2"

# -----------------------------------------------------------------------------
# 5. OPENSHIFT CLUSTER SETTINGS
# -----------------------------------------------------------------------------
openshift:
  cluster_name: "<Cluster Name>"
  version: "<Cluster Version>"
  
  # The base domain for the cluster.
  # The cluster will be accessible at: https://api.<cluster_name>.<base_domain>:6443
  base_domain: "example.local"
  
  # SDN Configuration (OVNKubernetes default)
  cluster_network_cidr: "10.128.0.0/14"
  cluster_network_host_prefix: 23
  service_network: "172.30.0.0/16"
  
  # Path to your Red Hat pull secret (download from console.redhat.com)
  pull_secret_file: "./pull-secret.json"
  
  # Path to the SSH public key injected into the nodes (for 'core' user access)
  ssh_public_key_file: "~/.ssh/id_rsa.pub"
{{if eq .BootMethod "netboot"}}
  # RHCOS Images used for building the payloads (Required for Netboot/PXE)
  rhcos_images:
    kernel_url: "<URL>/rhcos-live-kernel.ppc64le"
    initramfs_url: "<URL>/rhcos-live-initramfs.ppc64le.img"
    rootfs_url: "<URL>/rhcos-live-rootfs.ppc64le.img"
{{end}}
  # OpenShift Install binaries
  ocp_client_config:
    ocp_client: "<URL>/openshift-client-linux.tar.gz"
    ocp_installer: "<URL>/openshift-install-linux.tar.gz"

# -----------------------------------------------------------------------------
# 6. NODE TOPOLOGY (HMC Target LPARs)
# -----------------------------------------------------------------------------
nodes:
  # Boot method: "agent" (Agent Installer) or "netboot" (Standard PXE UPI)
  boot_method: "{{.BootMethod}}"

{{if .IsSNO}}
  sno:
    - name: "sno-0"
      ip: "10.20.x.10"
      existing_lpar_name: "<SNO-LPARNAME>"
      system_name: "SYSTEM-NAME"
{{else}}
{{- if eq .BootMethod "netboot"}}
  # Netboot requires a dedicated bootstrap node to initialize the control plane
  bootstrap:
    - name: "bootstrap"
      ip: "10.20.x.11"
      existing_lpar_name: "<BOOTSTRAP-LPARNAME>"
      system_name: "SYSTEM-NAME"
{{- end}}
  
  # Master nodes run the control plane (API, etcd). Minimum 3 required for HA.
  masters:
    - name: "master-0"
      ip: "10.20.x.12"
      existing_lpar_name: "<MASTER0-LPARNAME>"
      system_name: "SYSTEM-NAME"
    - name: "master-1"
      ip: "10.20.x.13"
      existing_lpar_name: "<MASTER1-LPARNAME>"
      system_name: "SYSTEM-NAME"
    - name: "master-2"
      ip: "10.20.x.14"
      existing_lpar_name: "<MASTER2-LPARNAME>"
      system_name: "SYSTEM-NAME"

  # Worker nodes run the application workloads. Optional.
  workers:
    - name: "worker-0"
      ip: "10.20.x.15"
      existing_lpar_name: "<WORKER0-LPARNAME>"
      system_name: "SYSTEM-NAME"
    - name: "worker-1"
      ip: "10.20.x.16"
      existing_lpar_name: "<WORKER1-LPARNAME>"
      system_name: "SYSTEM-NAME"
  
{{end}}`

const agentConfigTemplate = `# =============================================================================
# ShiftLaunch Internal Daemon Configuration (agent.yaml)
# =============================================================================
# ShiftLaunch uses sensible internal defaults. If you need to override timeouts,
# ports, or paths, modify this file. The binary will automatically detect it
# if it is placed in the same directory from where the command is run.
# =============================================================================

network:
  http_port: 8080

paths:
  workspace_dir: "/opt/shiftlaunch/clusters"
  dnsmasq_conf_dir: "/etc/dnsmasq.d"
  haproxy_conf_dir: "/etc/haproxy/conf.d"
  httpd_doc_root: "/var/www/html"
  tftp_root: "/var/lib/tftpboot"
  install_device: "/dev/sda"

timeouts:
  hmc_api_retries: 3
  download_timeout_sec: 1800
`

type TemplateData struct {
	IsSNO      bool
	BootMethod string
}

// GenerateConfig writes a dynamically generated starter config.yaml based on the user's topology and boot preferences
func GenerateConfig(configType, bootMethod, outputPath string) error {
	configType = strings.ToLower(configType)
	bootMethod = strings.ToLower(bootMethod)

	// Validate inputs
	if configType != "sno" && configType != "multi" {
		return fmt.Errorf("invalid config type: '%s'. Must be 'sno' or 'multi'", configType)
	}
	if bootMethod != "agent" && bootMethod != "netboot" {
		return fmt.Errorf("invalid boot method: '%s'. Must be 'agent' or 'netboot'", bootMethod)
	}

	// Safety check: Don't accidentally overwrite an existing cluster config
	if _, err := os.Stat(outputPath); err == nil {
		return fmt.Errorf("file '%s' already exists. Refusing to overwrite", outputPath)
	}

	data := TemplateData{
		IsSNO:      configType == "sno",
		BootMethod: bootMethod,
	}

	// 1. Generate the Cluster Config (config.yaml)
	tmpl, err := template.New("configGen").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse config template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to generate template output: %w", err)
	}

	if err := os.WriteFile(outputPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write configuration file: %w", err)
	}

	// Initialize console-only logger
	log, _ := logger.New(false, "")

	log.Info("Successfully generated cluster template", "topology", configType, "boot_method", bootMethod, "path", outputPath)

	// 2. Generate the Daemon Config (agent.yaml) if it doesn't already exist
	agentPath := "agent.yaml"
	if _, err := os.Stat(agentPath); os.IsNotExist(err) {
		if err := os.WriteFile(agentPath, []byte(agentConfigTemplate), 0644); err != nil {
			log.Warn("Failed to generate agent.yaml", "error", err)
		} else {
			log.Info("Successfully generated internal daemon config template", "path", agentPath)
		}
	} else {
		log.Info("Found existing agent.yaml in current directory, skipping generation")
	}

	log.Info("Please edit these files with your specific infrastructure details before running the 'create' command.")
	return nil
}
