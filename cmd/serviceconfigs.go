package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.ibm.com/sudeeshjohn/shiftlaunch/orchestrator"
	"github.ibm.com/sudeeshjohn/shiftlaunch/services"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

var dumpConfigCmd = &cobra.Command{
	Use:   "service-configs",
	Short: "Generate configuration files for unmanaged external services",
	GroupID: "utils",
	Long: `Generates DNS/DHCP/HAProxy configuration files for network administrators if you
disabled managed_services in YAML.

The service-configs command outputs:
- DNS records (A records for API, apps, nodes)
- DHCP configuration (ISC DHCP format)
- PXE/TFTP configuration (GRUB2 configs)
- Load Balancer configuration (HAProxy format)`,
	RunE: runDumpConfig,
}

func init() {
	rootCmd.AddCommand(dumpConfigCmd)
}

func runDumpConfig(cmd *cobra.Command, args []string) error {
	cfg, _, orch, err := loadConfig(true)
	if err != nil {
		return err
	}
	
	// Ensure logger file descriptor is closed when command completes
	defer orch.GetLogger().Close()

	return dumpConfig(orch, cfg)
}

// dumpConfig dumps the required external service configurations for a cluster
func dumpConfig(orch *orchestrator.Orchestrator, cfg *types.AgentConfig) error {
	clusterName := cfg.OpenShift.ClusterName
	vip := cfg.Network.LoadBalancerIP

	// Check if any external services are configured (Inverted logic for new ManagedServices block)
	// Boot method aware: DHCP/PXE only matter for netboot, NFS only matters for Agent ISO
	hasExternalServices := !cfg.ManagedServices.DNS || !cfg.ManagedServices.LoadBalancer
	
	// Add boot-method-specific checks
	if cfg.Nodes.BootMethod != "agent" {
		hasExternalServices = hasExternalServices || !cfg.ManagedServices.DHCP || !cfg.ManagedServices.PXE
	}
	if cfg.Nodes.BootMethod == "agent" {
		hasExternalServices = hasExternalServices || !cfg.ManagedServices.NFS
	}

	if !hasExternalServices {
		orch.GetLogger().Info("Cluster does not use any external services. All services will be managed by ShiftLaunch.", "cluster", clusterName)
		return nil
	}

	orch.GetLogger().Info("Generating external service configuration dump", "cluster", clusterName)

	// Print header
	fmt.Println("================================================================================")
	fmt.Printf("External Service Configuration Requirements for Cluster: %s\n", clusterName)
	fmt.Println("================================================================================")
	fmt.Println()

	// Get all nodes as pointers to match the interface
	nodes := cfg.GetAllNodes()

	// Dump DNS configuration
	if !cfg.ManagedServices.DNS {
		dumpDNSConfig(cfg)
	}

	// Dump DHCP configuration (only relevant for netboot)
	if !cfg.ManagedServices.DHCP && cfg.Nodes.BootMethod != "agent" {
		if err := dumpDHCPConfig(cfg); err != nil {
			return fmt.Errorf("failed to generate DHCP config: %w", err)
		}
	}

	// Dump PXE configuration using template (only relevant for netboot)
	if !cfg.ManagedServices.PXE && cfg.Nodes.BootMethod != "agent" {
		if err := dumpPXEConfigFromTemplate(clusterName, cfg, nodes, cfg.Controller.IP); err != nil {
			return fmt.Errorf("failed to generate PXE config: %w", err)
		}
	}
	
	// Dump NFS configuration (only relevant for Agent boot)
	if !cfg.ManagedServices.NFS && cfg.Nodes.BootMethod == "agent" {
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Println("NFS Server Configuration (Required for Agent ISO)")
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Println("You must configure an NFS server to host the Agent ISO files.")
		fmt.Printf("The VIOS will mount the ISO from: nfs://<your-nfs-server>/<export-path>\n")
		fmt.Println()
	}

	// Dump Load Balancer configuration
	if !cfg.ManagedServices.LoadBalancer {
		dumpLoadBalancerConfig(clusterName, vip, cfg)
	}

	orch.GetLogger().Info("External configuration dump complete", "cluster", clusterName)
	return nil
}

// dumpDNSConfig dumps required DNS records
func dumpDNSConfig(cfg *types.AgentConfig) {
	fmt.Println("┌────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ DNS SERVER CONFIGURATION                                                   │")
	fmt.Println("└────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Println("Required DNS A Records:")
	fmt.Println("-----------------------")
	fmt.Printf("api.%s.%s             IN A %s\n", cfg.OpenShift.ClusterName, cfg.OpenShift.BaseDomain, cfg.Network.LoadBalancerIP)
	fmt.Printf("api-int.%s.%s         IN A %s\n", cfg.OpenShift.ClusterName, cfg.OpenShift.BaseDomain, cfg.Network.LoadBalancerIP)
	fmt.Printf("*.apps.%s.%s          IN A %s\n", cfg.OpenShift.ClusterName, cfg.OpenShift.BaseDomain, cfg.Network.LoadBalancerIP)
	fmt.Println()

	fmt.Println("Node A Records:")
	fmt.Println("---------------")
	for _, node := range cfg.GetAllNodes() {
		fmt.Printf("%-30s IN A %s\n", node.Hostname, node.IP)
	}
	fmt.Println()
}

// dumpDHCPConfig dumps DHCP configuration using ISC DHCP Template
func dumpDHCPConfig(cfg *types.AgentConfig) error {
	fmt.Println("┌────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ DHCP SERVER CONFIGURATION                                                  │")
	fmt.Println("└────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()

	// Generate ISC DHCP configuration using the existing service template
	dhcpdData := services.PrepareISCDHCPData(cfg)
	iscDHCPConfig, err := services.GenerateISCDHCPConfig(dhcpdData)
	if err != nil {
		return fmt.Errorf("failed to generate ISC DHCP config: %w", err)
	}

	fmt.Println("ISC DHCP Configuration (dhcpd.conf):")
	fmt.Println("--------------------------------------")
	fmt.Println(iscDHCPConfig)

	return nil
}

// dumpPXEConfigFromTemplate dumps PXE/TFTP configuration strictly preserving original text
func dumpPXEConfigFromTemplate(clusterName string, cfg *types.AgentConfig, nodes []*types.NodeConfig, helperIP string) error {
	fmt.Println("┌────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ PXE/TFTP SERVER CONFIGURATION                                              │")
	fmt.Println("└────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Println("Required Directory Structure:")
	fmt.Println("-----------------------------")
	fmt.Println("/var/lib/tftpboot/")
	fmt.Printf("├── %s/\n", clusterName)
	fmt.Println("│   ├── core.elf                 # PowerPC bootloader")
	fmt.Println("│   ├── rhcos/")
	fmt.Println("│   │   ├── kernel               # RHCOS kernel")
	fmt.Println("│   │   ├── initramfs.img        # RHCOS initramfs")
	fmt.Println("│   │   └── rootfs.img           # RHCOS rootfs (served via HTTP)")
	fmt.Println("│   └── grub.cfg-<mac>           # GRUB2 config per MAC (e.g., grub.cfg-01-92-17-df-55-ed-03)")
	fmt.Println()

	fmt.Println("RHCOS Image URLs:")
	fmt.Println("-----------------")
	fmt.Printf("Kernel: %s\n", cfg.OpenShift.RHCOSImages.KernelURL)
	fmt.Printf("Initramfs: %s\n", cfg.OpenShift.RHCOSImages.InitramfsURL)
	fmt.Printf("Rootfs: %s\n", cfg.OpenShift.RHCOSImages.RootfsURL)
	fmt.Println()

	fmt.Println("GRUB2 Configuration Template (per node):")
	fmt.Println("-----------------------------------------")
	fmt.Printf("# Example: grub.cfg-01-92-17-df-55-ed-03 for bootstrap node\n")
	fmt.Println("set default=0")
	fmt.Println("set timeout=10")
	fmt.Println()
	fmt.Println("menuentry 'Install Node' {")
	fmt.Printf("  linux %s/rhcos/kernel initrd=%s/rhcos/initramfs.img \\\n", clusterName, clusterName)
	fmt.Println("        nomodeset rd.neednet=1 ip=dhcp coreos.inst=yes \\")
	fmt.Println("        coreos.inst.install_dev=/dev/sda \\")
	fmt.Printf("        coreos.live.rootfs_url=http://%s:8080/%s/rhcos/rootfs.img \\\n", helperIP, clusterName)
	fmt.Printf("        coreos.inst.ignition_url=http://%s:8080/%s/ignition/<role>.ign\n", helperIP, clusterName)
	fmt.Printf("  initrd %s/rhcos/initramfs.img\n", clusterName)
	fmt.Println("}")
	fmt.Println()

	fmt.Println("Per-Node Ignition URLs:")
	fmt.Println("-----------------------")
	for _, node := range nodes {
		role := "master"
		if strings.Contains(strings.ToLower(node.Hostname), "worker") {
			role = "worker"
		} else if strings.Contains(strings.ToLower(node.Hostname), "bootstrap") {
			role = "bootstrap"
		} else if cfg.IsSNO() {
			role = "bootstrap" // SNO uses bootstrap.ign
		}
		fmt.Printf("%s: http://%s:8080/%s/ignition/%s.ign\n", node.Hostname, helperIP, clusterName, role)
	}
	fmt.Println()

	fmt.Println("Note: Ignition files are served by ShiftLaunch controller on port 8080")
	fmt.Println()

	return nil
}

// dumpLoadBalancerConfig dumps load balancer configuration requirements strictly preserving original text
func dumpLoadBalancerConfig(clusterName, vip string, cfg *types.AgentConfig) {
	fmt.Println("┌────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ LOAD BALANCER CONFIGURATION                                                │")
	fmt.Println("└────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()

	fmt.Printf("Load Balancer Frontend IP: %s\n", vip)
	fmt.Println()

	// Separate nodes by role using the new flattened arrays
	bootstrap := cfg.Nodes.Bootstrap
	masters := cfg.Nodes.Masters
	workers := cfg.Nodes.Workers

	if cfg.IsSNO() {
		masters = cfg.Nodes.SNO
	}

	// Kubernetes API (Port 6443)
	fmt.Println("1. Kubernetes API Backend (Port 6443)")
	fmt.Println("   -----------------------------------")
	fmt.Printf("   Frontend: %s:6443\n", vip)
	fmt.Println("   Protocol: TCP")
	fmt.Println("   Health Check: TCP port 6443")
	fmt.Println("   Backend Servers:")
	if len(bootstrap) > 0 {
		for _, node := range bootstrap {
			fmt.Printf("     - %s:6443 (bootstrap - REMOVE after bootstrap complete)\n", node.IP)
		}
	}
	for _, node := range masters {
		fmt.Printf("     - %s:6443\n", node.IP)
	}
	fmt.Println()

	// Machine Config Server (Port 22623)
	fmt.Println("2. Machine Config Server Backend (Port 22623)")
	fmt.Println("   -------------------------------------------")
	fmt.Printf("   Frontend: %s:22623\n", vip)
	fmt.Println("   Protocol: TCP")
	fmt.Println("   Health Check: TCP port 22623")
	fmt.Println("   Backend Servers:")
	if len(bootstrap) > 0 {
		for _, node := range bootstrap {
			fmt.Printf("     - %s:22623 (bootstrap - REMOVE after bootstrap complete)\n", node.IP)
		}
	}
	for _, node := range masters {
		fmt.Printf("     - %s:22623\n", node.IP)
	}
	fmt.Println()

	// Ingress HTTPS (Port 443)
	fmt.Println("3. Ingress HTTPS Backend (Port 443)")
	fmt.Println("   ---------------------------------")
	fmt.Printf("   Frontend: %s:443\n", vip)
	fmt.Println("   Protocol: TCP")
	fmt.Println("   Health Check: TCP port 443")
	fmt.Println("   Backend Servers:")
	if len(workers) > 0 {
		for _, node := range workers {
			fmt.Printf("     - %s:443\n", node.IP)
		}
	} else {
		// Use masters if no workers
		for _, node := range masters {
			fmt.Printf("     - %s:443\n", node.IP)
		}
	}
	fmt.Println()

	// Ingress HTTP (Port 80)
	fmt.Println("4. Ingress HTTP Backend (Port 80)")
	fmt.Println("   -------------------------------")
	fmt.Printf("   Frontend: %s:80\n", vip)
	fmt.Println("   Protocol: TCP")
	fmt.Println("   Health Check: TCP port 80")
	fmt.Println("   Backend Servers:")
	if len(workers) > 0 {
		for _, node := range workers {
			fmt.Printf("     - %s:80\n", node.IP)
		}
	} else {
		// Use masters if no workers
		for _, node := range masters {
			fmt.Printf("     - %s:80\n", node.IP)
		}
	}
	fmt.Println()

	// HAProxy example
	fmt.Println("Example HAProxy Configuration:")
	fmt.Println("------------------------------")
	fmt.Println("frontend api")
	fmt.Printf("  bind %s:6443\n", vip)
	fmt.Println("  default_backend api_backend")
	fmt.Println("  mode tcp")
	fmt.Println("  option tcplog")
	fmt.Println()
	fmt.Println("backend api_backend")
	fmt.Println("  balance roundrobin")
	fmt.Println("  mode tcp")
	if len(bootstrap) > 0 {
		for _, node := range bootstrap {
			fmt.Printf("  server %s %s:6443 check  # REMOVE after bootstrap\n", node.Hostname, node.IP)
		}
	}
	for _, node := range masters {
		fmt.Printf("  server %s %s:6443 check\n", node.Hostname, node.IP)
	}
	fmt.Println()

	fmt.Println("frontend machine_config")
	fmt.Printf("  bind %s:22623\n", vip)
	fmt.Println("  default_backend machine_config_backend")
	fmt.Println("  mode tcp")
	fmt.Println("  option tcplog")
	fmt.Println()
	fmt.Println("backend machine_config_backend")
	fmt.Println("  balance roundrobin")
	fmt.Println("  mode tcp")
	if len(bootstrap) > 0 {
		for _, node := range bootstrap {
			fmt.Printf("  server %s %s:22623 check  # REMOVE after bootstrap\n", node.Hostname, node.IP)
		}
	}
	for _, node := range masters {
		fmt.Printf("  server %s %s:22623 check\n", node.Hostname, node.IP)
	}
	fmt.Println()

	fmt.Println("frontend ingress_https")
	fmt.Printf("  bind %s:443\n", vip)
	fmt.Println("  default_backend ingress_https_backend")
	fmt.Println("  mode tcp")
	fmt.Println("  option tcplog")
	fmt.Println()
	fmt.Println("backend ingress_https_backend")
	fmt.Println("  balance roundrobin")
	fmt.Println("  mode tcp")
	if len(workers) > 0 {
		for _, node := range workers {
			fmt.Printf("  server %s %s:443 check\n", node.Hostname, node.IP)
		}
	} else {
		for _, node := range masters {
			fmt.Printf("  server %s %s:443 check\n", node.Hostname, node.IP)
		}
	}
	fmt.Println()

	fmt.Println("frontend ingress_http")
	fmt.Printf("  bind %s:80\n", vip)
	fmt.Println("  default_backend ingress_http_backend")
	fmt.Println("  mode tcp")
	fmt.Println("  option tcplog")
	fmt.Println()
	fmt.Println("backend ingress_http_backend")
	fmt.Println("  balance roundrobin")
	fmt.Println("  mode tcp")
	if len(workers) > 0 {
		for _, node := range workers {
			fmt.Printf("  server %s %s:80 check\n", node.Hostname, node.IP)
		}
	} else {
		for _, node := range masters {
			fmt.Printf("  server %s %s:80 check\n", node.Hostname, node.IP)
		}
	}
	fmt.Println()
}
