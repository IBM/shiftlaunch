package cmd

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/IBM/shiftlaunch/logger"
	"github.com/spf13/cobra"
)

var (
	genConfigType       string
	genBootMethod       string
	genOutputPath       string
	genIsolationLevel   string // Replaced genDisconnected with isolation-level enum
	genReleaseType      string
	genProxy            bool
	genExternalProxy    bool
	genRegistry         bool
	genExternalRegistry bool
)

var generateConfigCmd = &cobra.Command{
	Use:     "generate-config",
	Short:   "Create a starter config.yaml template",
	GroupID: "utils",
	Long: `Creates a starter configuration template based on topology, boot method, and network environment.

NETWORK ARCHITECTURE MATRIX:
  Standard Connected:   (No flags)
  Strict Airgap:        --disconnected (Auto-enables managed registry)
  
PROXY CONTROLS:
  Managed Squid Proxy:  --proxy
  External Corp Proxy:  --external-proxy

REGISTRY CONTROLS:
  Managed Registry:     --registry (Useful for connected caching)
  External Registry:    --external-registry`,
	RunE: runGenerateConfig,
}

func init() {
	rootCmd.AddCommand(generateConfigCmd)

	generateConfigCmd.Flags().StringVarP(&genConfigType, "type", "t", "multi", "Cluster topology: 'sno' or 'multi'")
	generateConfigCmd.Flags().StringVarP(&genBootMethod, "boot", "b", "netboot", "Boot method: 'agent' or 'netboot'")
	generateConfigCmd.Flags().StringVarP(&genOutputPath, "output", "o", "config.yaml", "Path to save the generated file")
	generateConfigCmd.Flags().StringVar(&genReleaseType, "release-type", "official", "Payload type: 'official' or 'ci'")

	// Network Boundary
	generateConfigCmd.Flags().StringVarP(&genIsolationLevel, "isolation-level", "i", "connected", "Network architecture: 'connected', 'restricted-network', or 'air-gapped'")

	// Proxy Toggles
	generateConfigCmd.Flags().BoolVarP(&genProxy, "proxy", "p", false, "Enable local managed Squid proxy")
	generateConfigCmd.Flags().BoolVar(&genExternalProxy, "external-proxy", false, "Use an external corporate proxy")

	// Registry Toggles
	generateConfigCmd.Flags().BoolVar(&genRegistry, "registry", false, "Enable local managed Podman registry")
	generateConfigCmd.Flags().BoolVar(&genExternalRegistry, "external-registry", false, "Use an external enterprise registry")
}

func runGenerateConfig(cmd *cobra.Command, args []string) error {
	configType := strings.ToLower(genConfigType)
	bootMethod := strings.ToLower(genBootMethod)

	if configType != "sno" && configType != "multi" {
		return fmt.Errorf("invalid config type: '%s'. Must be 'sno' or 'multi'", configType)
	}
	if bootMethod != "agent" && bootMethod != "netboot" {
		return fmt.Errorf("invalid boot method: '%s'. Must be 'agent' or 'netboot'", bootMethod)
	}

	// Interlock: SNO + netboot is an invalid combination
	if configType == "sno" && bootMethod == "netboot" {
		return fmt.Errorf("invalid combination: Single Node OpenShift (SNO) requires the 'agent' boot method. Netboot is not supported for SNO")
	}

	if _, err := os.Stat(genOutputPath); err == nil {
		return fmt.Errorf("file '%s' already exists. Refusing to overwrite", genOutputPath)
	}

	if genProxy && genExternalProxy {
		return fmt.Errorf("cannot specify both --proxy (managed) and --external-proxy. Choose one")
	}
	if genRegistry && genExternalRegistry {
		return fmt.Errorf("cannot specify both --registry (managed) and --external-registry. Choose one")
	}

	isolationMode := strings.ToLower(genIsolationLevel)
	if isolationMode != "connected" && isolationMode != "restricted-network" && isolationMode != "air-gapped" {
		return fmt.Errorf("invalid isolation level: '%s'. Must be 'connected', 'restricted-network', or 'air-gapped'", isolationMode)
	}

	data := TemplateData{
		IsSNO:          configType == "sno",
		BootMethod:     bootMethod,
		IsolationLevel: isolationMode,
		ManageProxy:    genProxy,
		ExtProxy:       genExternalProxy,
		ManageRegistry: genRegistry, // Removed the disconnected override - root.go handles auto-resolver
		ExtRegistry:    genExternalRegistry,
		ReleaseType:    genReleaseType,
	}

	tmpl, err := template.New("configGen").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse config template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("failed to generate template output: %w", err)
	}

	if err := os.WriteFile(genOutputPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write configuration file: %w", err)
	}

	log, _ := logger.New(false, "")
	log.Info("Successfully generated cluster template", "path", genOutputPath)

	return nil
}
// TemplateData struct
type TemplateData struct {
	IsSNO          bool
	BootMethod     string
	IsolationLevel string
	ManageProxy    bool
	ExtProxy       bool
	ManageRegistry bool
	ExtRegistry    bool
	ReleaseType    string
}

const configTemplate = `# =============================================================================
# ShiftLaunch Configuration Reference
# Topology: {{if .IsSNO}}SNO (Single Node OpenShift){{else}}Multi-Node Cluster{{end}}
# Boot Method: {{if eq .BootMethod "agent"}}Agent Installer{{else}}Network Boot (PXE){{end}}
# =============================================================================

# -----------------------------------------------------------------------------
# 1. INFRASTRUCTURE SERVICES
# CONVENTION OVER CONFIGURATION:
# Core infrastructure services (DNS, Load Balancer, NFS) are automatically
# managed locally on the controller node if left commented out.
# To Bring Your Own Infrastructure (BYOI), simply uncomment the block
# and fill in your network target.
# -----------------------------------------------------------------------------
services:
  # --- CORE INFRASTRUCTURE (Managed locally by default if commented out) ---
  
  # dns:
    # external_nameserver: "192.168.100.5"  # Uncomment to bypass local dnsmasq and point to enterprise DNS

  # load_balancer:
    # vip: "192.168.100.50"                 # Uncomment to use a dedicated floating IP for the cluster
                                            # or if planning to host multiple clusters on this controller
    # external_lb_ip: "192.168.100.2"       # Uncomment to bypass local HAProxy and route to an external F5/Big-IP

{{if eq .BootMethod "agent"}}
  # nfs:
    # external_nfs_server: "192.168.100.3"  # Uncomment to use an external enterprise NFS appliance for Agent ISOs
    # external_nfs_path: "/vol/shiftlaunch" # Required if external_nfs_server is used
{{else}}
  # nfs:
    # external_nfs_server: "192.168.100.3"  # NFS not required for netboot environments
{{end}}

  # --- BOOT METHOD SPECIFIC SERVICES ---
{{if eq .BootMethod "netboot"}}
  # dhcp:
    # external_dhcp_server: "192.168.100.1" # Uncomment to bypass local DHCP management
      
  # pxe:
    # external_pxe_server: "192.168.100.1"  # Uncomment to bypass local PXE/TFTP management
{{else}}
  # dhcp:
    # external_dhcp_server: "192.168.100.1" # DHCP is bypassed/not required for Agent ISO boot
      
  # pxe:
    # external_pxe_server: "192.168.100.1"  # PXE is bypassed/not required for Agent ISO boot
{{end}}

{{if or (ne .IsolationLevel "connected") .ExtProxy .ManageProxy .ExtRegistry .ManageRegistry}}

  # --- BOUNDARY SERVICES (Strictly Opt-In) ---
{{- end}}

{{- if or (eq .IsolationLevel "restricted-network") .ExtProxy .ManageProxy}}
{{if or .ExtProxy .ManageProxy}}
  proxy:
{{- else}}
  # proxy:
{{- end}}
{{- if .ExtProxy}}
    external_http_proxy: "http://proxy.mycompany.com:8080"
    external_https_proxy: "http://proxy.mycompany.com:8080"
    no_proxy: "127.0.0.1,localhost,.example.local,192.168.100.0/24"
{{- else if .ManageProxy}}
    # ShiftLaunch will build a local Squid Proxy gateway instance automatically
{{- else}}
    # external_http_proxy: "http://proxy.mycompany.com:8080"
    # external_https_proxy: "http://proxy.mycompany.com:8080"
    # no_proxy: "127.0.0.1,localhost,.example.local,192.168.100.0/24"
    # Note: To build a locally managed Squid proxy, simply use 'proxy: {}'
{{- end}}
{{- end}}

{{- if or (eq .IsolationLevel "air-gapped") .ExtRegistry .ManageRegistry}}
{{if or .ExtRegistry .ManageRegistry}}
  registry:
{{- else}}
  # registry:
{{- end}}
{{- if .ExtRegistry}}
    external_registry_server: "registry.mycompany.com"
    username: "admin"
    password: "admin"
    ca_cert_file: "/path/ca.crt"
{{- else if .ManageRegistry}}
    # ShiftLaunch will build a local Podman registry automatically.
    # Default credentials (admin:admin) will be injected and synced automatically.
{{- else}}
    # external_registry_server: "registry.mycompany.com"
    # username: "admin"
    # password: "admin"
    # ca_cert_file: "/path/ca.crt"
    # Note: If isolation_level is set to 'air-gapped', ShiftLaunch will automatically
    # build a local Podman registry if this block is left commented out.
{{- end}}
{{- end}}

# -----------------------------------------------------------------------------
# 2. CORE NETWORK & ARCHITECTURE MODE
# -----------------------------------------------------------------------------
network:
  # ARCHITECTURE TARGETS:
  #   "connected"         -> Connected Installation (or Standard Connected Deployment). Nodes have direct outbound internet access.
  #   "restricted-network" -> Proxy-Connected or Restricted Network deployment. Nodes are isolated but access internet via Proxy gateway.
  #   "air-gapped"-> True Disconnected or Air-Gapped. Nodes are strictly airgapped. Enforces local Registry data.
  isolation_level: "{{.IsolationLevel}}"
  
  controller_interface: "eth0"             # Physical interface on the controller to bind the VIP and services to
  machine_network_cidr: "192.168.100.0/24" # The primary subnet the OpenShift nodes reside on
  gateway: "192.168.100.1"                 # The default gateway for the cluster nodes
  
  dns_forwarders:                          # Network DNS servers used by the local dnsmasq service
    - "8.8.8.8"
    - "1.1.1.1"

# -----------------------------------------------------------------------------
# 3. OPENSHIFT CLUSTER SETTINGS
# -----------------------------------------------------------------------------
openshift:
  cluster_name: "my-cluster"               # The unique identifier for your OpenShift cluster
  version: "4.21.18"                       # Target OpenShift Container Platform version
  release_type: "{{.ReleaseType}}"         # "official" for stable releases, "ci" for raw nightly builds             
  base_domain: "example.local"             # The base DNS zone (Cluster FQDN will be api.<cluster_name>.<base_domain>)
  pull_secret_file: "~/pull-secret.json"   # Path to your Red Hat pull secret (from console.redhat.com)
  ssh_public_key_file: "~/.ssh/id_rsa.pub" # SSH public key for 'core' user access to the OpenShift nodes
  force_ocp_download: false                # Set to true to force re-download of existing OpenShift artifacts

# -----------------------------------------------------------------------------
# 4. HMC CREDENTIALS (IBM Hardware Management Console)
# -----------------------------------------------------------------------------
hmc:
  ip: "192.168.100.10"                     # The IP address or hostname of your HMC appliance
  username: "hscroot"                      # HMC user with hmcsuperadmin privileges
  password: "YOUR_HMC_PASSWORD"            # Password for the HMC user

# -----------------------------------------------------------------------------
# 5. NODE TOPOLOGY (HMC Target LPARs)
# -----------------------------------------------------------------------------
nodes:
  boot_method: "{{.BootMethod}}"           # "agent" (ISO) or "netboot" (PXE)
{{if .IsSNO}}
  masters:
    - name: "sno-0"                        # Internal Kubernetes node name
      ip: "192.168.100.12"                 # Static IP to assign to this node
      existing_lpar_name: "my-sno-lpar"    # EXACT partition name as it appears on the HMC
      system_name: "Power-System-1080-HEX" # EXACT Managed System name hosting this LPAR
{{else}}
{{- if eq .BootMethod "netboot"}}
  bootstrap:
    - name: "bootstrap"                    # Transient node used only during installation
      ip: "192.168.100.11"                 # Static IP for the bootstrap node
      existing_lpar_name: "my-bootstrap"   # EXACT partition name as it appears on the HMC
      system_name: "Power-System-1080-HEX" # EXACT Managed System name hosting this LPAR
{{- end}}
  masters:
    - name: "master-0"                     # Control plane node 0
      ip: "192.168.100.12"                 # Static IP for master-0
      existing_lpar_name: "my-master-0"    # EXACT partition name on the HMC
      system_name: "Power-System-1080-HEX" # EXACT Managed System name hosting this LPAR
    - name: "master-1"                     # Control plane node 1
      ip: "192.168.100.13"                 # Static IP for master-1
      existing_lpar_name: "my-master-1"    # EXACT partition name on the HMC
      system_name: "Power-System-1080-HEX" # EXACT Managed System name hosting this LPAR
    - name: "master-2"                     # Control plane node 2
      ip: "192.168.100.14"                 # Static IP for master-2
      existing_lpar_name: "my-master-2"    # EXACT partition name on the HMC
      system_name: "Power-System-1080-HEX" # EXACT Managed System name hosting this LPAR

  workers:
    - name: "worker-0"                     # Compute node 0
      ip: "192.168.100.15"                 # Static IP for worker-0
      existing_lpar_name: "my-worker-0"    # EXACT partition name on the HMC
      system_name: "Power-System-1022-HEX" # EXACT Managed System name hosting this LPAR
    - name: "worker-1"                     # Compute node 1
      ip: "192.168.100.16"                 # Static IP for worker-1
      existing_lpar_name: "my-worker-1"    # EXACT partition name on the HMC
      system_name: "Power-System-1022-HEX" # EXACT Managed System name hosting this LPAR
{{end}}`
