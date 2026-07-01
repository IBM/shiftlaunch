package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/IBM/shiftlaunch/infra/compute"
	"github.com/IBM/shiftlaunch/infra/controller"
	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/services"
	"github.com/IBM/shiftlaunch/types"
)

// Day2NodesConfig represents the configuration for Day 2 nodes
type Day2NodesConfig struct {
	Hosts []Day2Host `yaml:"hosts"`
}

// Day2Host represents the configuration for a Day 2 host
type Day2Host struct {
	Hostname        string            `yaml:"hostname"`
	RootDeviceHints Day2DeviceHints   `yaml:"rootDeviceHints"`
	Interfaces      []Day2Interface   `yaml:"interfaces"`
	NetworkConfig   Day2NetworkConfig `yaml:"networkConfig"`
}

// Day2DeviceHints represents the hints for the root device
type Day2DeviceHints struct {
	DeviceName string `yaml:"deviceName"`
}

// Day2Interface represents the configuration for a Day 2 network interface
type Day2Interface struct {
	MacAddress string `yaml:"macAddress"`
	Name       string `yaml:"name"`
}

// Day2NetworkConfig represents the configuration for Day 2 network
type Day2NetworkConfig struct {
	Interfaces  []Day2NetInterface `yaml:"interfaces"`
	DNSResolver *Day2DNSResolver   `yaml:"dns-resolver,omitempty"`
	Routes      *Day2Routes        `yaml:"routes,omitempty"`
}

// Day2DNSResolver represents the configuration for Day 2 DNS resolver
type Day2DNSResolver struct {
	Config Day2DNSConfig `yaml:"config"`
}

// Day2DNSConfig represents the configuration for Day 2 DNS
type Day2DNSConfig struct {
	Server []string `yaml:"server"`
}

// Day2Routes represents the configuration for Day 2 routes
type Day2Routes struct {
	Config []Day2RouteConfig `yaml:"config"`
}

// Day2RouteConfig represents the configuration for Day 2 routes
type Day2RouteConfig struct {
	Destination      string `yaml:"destination"`
	NextHopAddress   string `yaml:"next-hop-address"`
	NextHopInterface string `yaml:"next-hop-interface"`
}

// Day2NetInterface represents the configuration for Day 2 network interface
type Day2NetInterface struct {
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`
	State      string   `yaml:"state"`
	MacAddress string   `yaml:"mac-address"`
	Ipv4       Day2Ipv4 `yaml:"ipv4"`
	Ipv6       Day2Ipv6 `yaml:"ipv6"`
}

// Day2Ipv6 represents the configuration for Day 2 IPv6
type Day2Ipv6 struct {
	Enabled bool `yaml:"enabled"`
}

// Day2Ipv4 represents the configuration for Day 2 IPv4
type Day2Ipv4 struct {
	Enabled bool       `yaml:"enabled"`
	Address []Day2Addr `yaml:"address"`
	Dhcp    bool       `yaml:"dhcp"`
}

// Day2Addr represents the configuration for Day 2 address
type Day2Addr struct {
	IP           string `yaml:"ip"`
	PrefixLength int    `yaml:"prefix-length"`
}

var scaleCmd = &cobra.Command{
	Use:     "scale",
	Short:   "Scale an existing cluster by adding new worker nodes appended to config.yaml",
	GroupID: "core",
	RunE:    runScale,
}

// init scales the cluster by adding new worker nodes to the cluster.
func init() {
	rootCmd.AddCommand(scaleCmd)
}

// runScale scales the cluster by adding new worker nodes to the cluster.
func runScale(cmd *cobra.Command, args []string) error {
	// 1. Load active cluster configuration
	cfg, daemonCfg, orch, err := loadConfig(true)
	if err != nil {
		return err
	}
	defer orch.GetLogger().Close()

	ctx := GetContext()
	log := orch.GetLogger()
	workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, cfg.OpenShift.ClusterName)
	installDir := filepath.Join(workspaceDir, "install-dir")

	stateManager := types.NewStateManager(cfg.OpenShift.ClusterName)
	state, err := stateManager.LoadState()
	if err != nil || state.Status != "completed" {
		return fmt.Errorf("cannot scale cluster '%s': cluster must be in 'completed' status", cfg.OpenShift.ClusterName)
	}

	log.Info(fmt.Sprintf("=== Starting Day-2 Scale Operation: %s ===", cfg.OpenShift.ClusterName))

	// 2. Read the NEW config file
	updatedYamlData, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to read updated config file '%s': %w", configFile, err)
	}

	var updatedCfg types.AgentConfig
	if err := yaml.Unmarshal(updatedYamlData, &updatedCfg); err != nil {
		return fmt.Errorf("failed to parse updated config YAML: %w", err)
	}

	// NEW: Catch copy-paste topology bugs (like adding workers to SNO) immediately!
	if err := updatedCfg.Validate(); err != nil {
		return fmt.Errorf("invalid scale configuration detected: %w", err)
	}

	updatedCfg.Network.ControllerIP = cfg.Network.ControllerIP

	// ========================================================================
	// LOAD BALANCER ZERO-CONFIG AUTO-RESOLVER (For Day-2 Updated Config)
	// ========================================================================
	if updatedCfg.Services.LoadBalancer == nil {
		updatedCfg.Services.LoadBalancer = &types.ServiceLoadBalancer{}
	}
	if updatedCfg.Services.LoadBalancer.ExternalLoadBalancer != "" {
		updatedCfg.Services.LoadBalancer.VIP = updatedCfg.Services.LoadBalancer.ExternalLoadBalancer
	}
	if updatedCfg.Services.LoadBalancer.VIP == "" {
		updatedCfg.Services.LoadBalancer.VIP = updatedCfg.Network.ControllerIP
	}

	// --- SMART BACKUP ENGINE ---
	workspaceConfigPath := filepath.Join(workspaceDir, "config.yaml")
	if existingConfigData, readErr := os.ReadFile(workspaceConfigPath); readErr == nil {
		// Only backup and overwrite if the new config is ACTUALLY different (Idempotent)
		if !bytes.Equal(existingConfigData, updatedYamlData) {
			timestamp := time.Now().Format("20060102-150405")
			configBackupPath := filepath.Join(workspaceDir, fmt.Sprintf("config.yaml.backup.%s", timestamp))

			if backupErr := os.WriteFile(configBackupPath, existingConfigData, 0644); backupErr != nil {
				log.Warn("Failed to create a historical backup of the original config.yaml", "error", backupErr)
			} else {
				log.Debug("Successfully archived original cluster config.yaml backup", "path", configBackupPath)

				// Track the backup formally in state.json
				stateManager.AddConfigBackup(state, configBackupPath)
				_ = stateManager.SaveState(state)
			}

			// Overwrite the workspace config
			if err := os.WriteFile(workspaceConfigPath, updatedYamlData, 0644); err != nil {
				log.Warn("Failed to update workspace config.yaml", "error", err)
			}
		} else {
			log.Debug("Config file is unchanged. Skipping backup.")
		}
	}
	cfg = &updatedCfg

	// =========================================================================
	// DIFF ENGINE: Identify Scale-Down and Scale-Up Targets
	// =========================================================================
	var scaleDownTargets []types.NodeConfig
	var scaleUpTargets []types.NodeConfig
	var pendingBoot []types.NodeConfig

	localExec := localexec.NewLocalClient(log)
	ocPath := filepath.Join(workspaceDir, "tools", "oc")
	kubeconfigPath := filepath.Join(installDir, "auth", "kubeconfig")

	// 1. Identify removed workers (Scale-Down)
	// We use state.DiscoveredNodes as the absolute source of truth for deployed infrastructure
	for _, deployedNode := range state.DiscoveredNodes {
		// We only ever scale down workers. Never touch masters or bootstrap nodes!
		if deployedNode.Role != "worker" {
			continue
		}

		foundInNewConfig := false
		for _, newWorker := range updatedCfg.Nodes.Workers {
			if deployedNode.Hostname == newWorker.Hostname {
				foundInNewConfig = true
				break
			}
		}

		// If the node is in the deployed state, but missing from the new config, nuke it.
		if !foundInNewConfig {
			scaleDownTargets = append(scaleDownTargets, types.NodeConfig{
				Hostname:         deployedNode.Hostname,
				IP:               deployedNode.IP,
				ExistingLPARName: deployedNode.LPARName,
				SystemName:       deployedNode.SystemName,
				MACAddress:       deployedNode.MACAddress,
			})
		}
	}
	if len(scaleDownTargets) > 0 {
		log.EndPhase(true, fmt.Sprintf("Found %d workers to be removed from the cluster", len(scaleDownTargets)))
	}
	// 2. Identify added workers (Scale-Up)
	for _, worker := range updatedCfg.Nodes.Workers {
		readyMarker := "ready_" + worker.Hostname
		if contains(state.CompletedPhases, readyMarker) {
			continue
		}

		// SELF-HEALING API CHECK: If state missed the ready marker (e.g., Day-1 nodes), ask the OpenShift API directly!
		checkCmd := fmt.Sprintf("KUBECONFIG=%s %s get node %s -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null", kubeconfigPath, ocPath, worker.Hostname)
		if out, err := localExec.Execute(ctx, checkCmd); err == nil && strings.TrimSpace(out) == "True" {
			log.Debug("Node is already natively Ready in OpenShift API. Backfilling state marker.", "node", worker.Hostname)
			state.CompletedPhases = append(state.CompletedPhases, readyMarker)
			_ = stateManager.SaveState(state)
			continue
		}

		scaleUpTargets = append(scaleUpTargets, worker)

		// If it hasn't been powered on by the HMC yet, it needs a boot
		if !contains(state.CompletedPhases, "booted_"+worker.Hostname) {
			pendingBoot = append(pendingBoot, worker)
		}
	}
	if len(scaleUpTargets) > 0 {
		log.EndPhase(true, fmt.Sprintf("Found %d workers to be added to the cluster", len(scaleUpTargets)))
	}

	if len(scaleDownTargets) == 0 && len(scaleUpTargets) == 0 {
		log.Info("Configuration diff matches current state. No scaling operations required.")
		return nil
	}

	// =========================================================================
	// EXECUTE SCALE DOWN (Node Removal & HMC Teardown)
	// =========================================================================
	if len(scaleDownTargets) > 0 {
		log.Debug(fmt.Sprintf("Detected %d worker(s) scheduled for removal. Initiating scale-down sequence...", len(scaleDownTargets)))

		// We need HMC access to power off and unmap the removed nodes
		log.StartPhase("Connecting to HMC platform to process node removal...")
		provider, err := compute.NewProviderWithState(cfg, log, debug, stateManager)
		if err != nil {
			log.EndPhase(false, "Failed to connect to HMC")
			return fmt.Errorf("failed to connect to HMC for scale-down: %w", err)
		}
		hmcProvider, ok := provider.(*compute.HMCProvider)
		if !ok {
			return fmt.Errorf("failed to cast compute provider")
		}
		log.EndPhase(true, "HMC connected successfully")

		for _, target := range scaleDownTargets {
			log.StartPhase(fmt.Sprintf("Evicting and removing %s from OpenShift...", target.Hostname))

			// 1. Cordon the node
			cordonCmd := fmt.Sprintf("KUBECONFIG=%s %s adm cordon %s", kubeconfigPath, ocPath, target.Hostname)
			localExec.Execute(ctx, cordonCmd)

			// 2. Drain the node gracefully
			drainCmd := fmt.Sprintf("KUBECONFIG=%s %s adm drain %s --force --delete-emptydir-data --ignore-daemonsets --timeout=5m", kubeconfigPath, ocPath, target.Hostname)
			if _, err := localExec.Execute(ctx, drainCmd); err != nil {
				log.Warn("Drain encountered warnings (PDBs or DaemonSets) - proceeding with deletion anyway", "node", target.Hostname)
			}

			// 3. Delete from cluster
			deleteCmd := fmt.Sprintf("KUBECONFIG=%s %s delete node %s", kubeconfigPath, ocPath, target.Hostname)
			localExec.Execute(ctx, deleteCmd)

			log.EndPhase(true, fmt.Sprintf("Node %s cleanly removed from OpenShift", target.Hostname))

			// 4. Clean up HMC infrastructure (Power off & ISO removal)
			log.StartPhase(fmt.Sprintf("Powering off LPAR and releasing ISO media for %s...", target.Hostname))

			var targetUUID string
			for _, dNode := range state.DiscoveredNodes {
				if dNode.Hostname == target.Hostname {
					targetUUID = dNode.UUID
					break
				}
			}

			if targetUUID != "" {
				// Power off LPAR immediately without deleting it
				_, err := hmcProvider.GetHMCClient().PowerOffPartition(ctx, targetUUID, "Immediate", false)
				if err != nil && !strings.Contains(strings.ToLower(err.Error()), "unavailable in the current partition state") {
					log.Warn("Failed to power off LPAR", "error", err)
				}

				// If Agent Boot, unmap and delete the ISO from the VIOS
				if cfg.Nodes.BootMethod == "agent" {
					for i, mapping := range state.ISOMappings {
						if mapping.NodeName == target.Hostname {
							// Resolve System UUID required for unmapping
							sysUUID, _, _ := hmcProvider.GetHMCClient().GetManagedSystemByName(ctx, mapping.SystemName)

							if sysUUID != "" {
								// Unmap ISO from LPAR
								_, err := hmcProvider.GetHMCClient().DeleteVirtualOpticalMaps(ctx, sysUUID, mapping.VIOSUUID, targetUUID, []string{mapping.MediaName})
								if err != nil {
									log.Warn("Failed to unmap ISO from LPAR", "error", err)
								} else {
									time.Sleep(3 * time.Second) // Let VIOS digest the unmap
									// Delete ISO from repository
									err = hmcProvider.GetHMCClient().DeleteVirtualOpticalMedia(ctx, mapping.SystemName, mapping.VIOSName, mapping.MediaName)
									if err != nil && !strings.Contains(err.Error(), "not found") {
										log.Warn("Failed to delete ISO from VIOS repository", "error", err)
									}
								}
							}

							// Remove mapping from state array
							state.ISOMappings = append(state.ISOMappings[:i], state.ISOMappings[i+1:]...)
							break
						}
					}
				}
			} else {
				log.Warn("LPAR UUID not found in state file; bypassing HMC power off", "node", target.Hostname)
			}
			log.EndPhase(true, fmt.Sprintf("Hardware released for %s", target.Hostname))

			// 5. Purge the node from the local state markers
			var newDiscovered []types.DiscoveredNode
			for _, dNode := range state.DiscoveredNodes {
				if dNode.Hostname != target.Hostname {
					newDiscovered = append(newDiscovered, dNode)
				}
			}
			state.DiscoveredNodes = newDiscovered

			var newPhases []string
			for _, phase := range state.CompletedPhases {
				if phase != "booted_"+target.Hostname && phase != "ready_"+target.Hostname {
					newPhases = append(newPhases, phase)
				}
			}
			state.CompletedPhases = newPhases
			_ = stateManager.SaveState(state)
		}

		hmcProvider.Cleanup() // Close session before moving to scale up
	}

	// =========================================================================
	// EXECUTE SCALE UP (Day-2 Node Addition)
	// =========================================================================
	if len(scaleUpTargets) == 0 {
		log.Info("=== Scale Operation Completed Successfully ===")
		return nil
	}

	log.Debug(fmt.Sprintf("Detected %d worker(s) scaling up. %d require ISO/booting, %d require monitoring.", len(scaleUpTargets), len(pendingBoot), len(scaleUpTargets)-len(pendingBoot)))

	// =========================================================================
	// PRE-FLIGHT VALIDATION FOR NEW DAY-2 NODES
	// =========================================================================
	log.StartPhase("Validating network parameters for new Day-2 scale targets...")

	_, ipNet, parseErr := net.ParseCIDR(cfg.Network.MachineCIDR)
	if parseErr != nil {
		log.EndPhase(false, "Network validation failed")
		return fmt.Errorf("invalid machine network CIDR in config: %w", parseErr)
	}

	for _, target := range scaleUpTargets {
		// 1. CIDR Boundary Check: Ensure the new IP actually belongs to the cluster subnet
		nodeIP := net.ParseIP(target.IP)
		if nodeIP == nil || !ipNet.Contains(nodeIP) {
			log.EndPhase(false, "Network validation failed")
			return fmt.Errorf("Day-2 validation failed: IP '%s' for new node '%s' is invalid or outside the machine_network_cidr '%s'", target.IP, target.Hostname, cfg.Network.MachineCIDR)
		}

		// 2. Ping Conflict Check: Ensure no other device on the network is actively using this IP!
		// If the command SUCCEEDS (exit code 0), it means the IP answered our ping, which is a collision!
		pingCmd := fmt.Sprintf("ping -c 2 -W 2 %s >/dev/null 2>&1", target.IP)
		if _, err := localExec.Execute(ctx, pingCmd); err == nil {
			log.EndPhase(false, "Network validation failed")
			return fmt.Errorf("Day-2 validation failed: IP CONFLICT! The IP '%s' designated for new node '%s' is already actively responding on the network", target.IP, target.Hostname)
		}
	}

	log.EndPhase(true, "Day-2 scale targets passed network isolation and availability checks")

	// =========================================================================
	// EXECUTE AGENT ISO GENERATION & BOOT (ONLY IF NODES NEED IT)
	// =========================================================================
	if len(pendingBoot) > 0 {
		// 4. Connect to HMC and discover metadata
		log.StartPhase("Connecting to HMC platform infrastructure to resolve target LPAR nodes...")
		provider, err := compute.NewProviderWithState(cfg, log, debug, stateManager)
		if err != nil {
			return err
		}
		hmcProvider, ok := provider.(*compute.HMCProvider)
		if !ok {
			return fmt.Errorf("failed to cast compute provider to HMC provider context")
		}
		defer hmcProvider.Cleanup()
		log.EndPhase(true, "Hypervisor management engine linked successfully")

		log.StartPhase("Querying HMC environment to discover MAC and profile parameters for new LPARs...")
		if err := hmcProvider.DiscoverMetadata(ctx); err != nil {
			log.EndPhase(false, "Failed to resolve hypervisor hardware properties")
			return fmt.Errorf("scale infrastructure discovery failed: %w", err)
		}

		state, _ = stateManager.LoadState()
		log.EndPhase(true, "Day-2 target LPAR hardware profiles successfully mapped from HMC")

		// 5. Dynamic Networking & Load Balancer configuration scaling
		log.StartPhase("Reconciling infrastructure services for additional node paths...")

		dnsmasq := services.NewDNSmasqManager(cfg, daemonCfg, localExec)
		restartDnsmasq := false

		if cfg.Services.DNS.IsManaged() {
			log.Info("Regenerating DNS routing rules locally...")
			if err := dnsmasq.SetupDNS(ctx); err != nil {
				log.EndPhase(false, "Failed to regenerate DNS")
				return fmt.Errorf("failed to regenerate DNS configurations: %w", err)
			}
			restartDnsmasq = true
		} else {
			log.Debug("Skipping DNS regeneration (User Managed)")
		}

		if cfg.Nodes.BootMethod == "netboot" {
			if cfg.Services.DHCP.IsManaged() {
				log.Info("Regenerating DHCP reservations locally...")
				if err := dnsmasq.SetupDHCP(ctx); err != nil {
					log.EndPhase(false, "Failed to regenerate DHCP")
					return fmt.Errorf("failed to regenerate DHCP configurations: %w", err)
				}
				restartDnsmasq = true
			}
			if cfg.Services.PXE.IsManaged() {
				log.Info("Regenerating PXE/TFTP bootloader configurations locally...")
				if err := dnsmasq.SetupPXEService(ctx); err != nil {
					log.EndPhase(false, "Failed to regenerate PXE service")
					return fmt.Errorf("failed to regenerate PXE configurations: %w", err)
				}
				if err := dnsmasq.ConfigurePXEBoot(ctx, workspaceDir); err != nil {
					log.EndPhase(false, "Failed to stage PXE MAC configurations")
					return fmt.Errorf("failed to stage PXE configurations: %w", err)
				}
				restartDnsmasq = true
			}
		}

		if restartDnsmasq {
			_ = localExec.SystemctlRestart(ctx, "dnsmasq")
		}

		if cfg.Services.LoadBalancer.IsManaged() {
			log.Info("Regenerating HAProxy backend pools locally...")
			if err := services.SetupHAProxy(ctx, cfg, localExec); err != nil {
				log.EndPhase(false, "Failed to regenerate HAProxy")
				return fmt.Errorf("failed to regenerate HAProxy configurations: %w", err)
			}
		} else {
			log.Debug("Skipping HAProxy regeneration (User Managed)")
		}

		log.EndPhase(true, "Local network core tables reconciled successfully")

		// 6. Guarantee Local API Resolution
		log.Debug("Ensuring OpenShift API is reachable from controller...")
		netMgr := controller.NewNetworkManager(localExec, debug, log)
		_ = netMgr.AddHostsEntry(ctx, cfg.OpenShift.ClusterName, cfg.OpenShift.BaseDomain, cfg.Services.LoadBalancer.GetVIP())

		// 7. Build the temporary Red Hat spec nodes-config.yaml file programmatically
		if cfg.Nodes.BootMethod == "agent" {
			log.StartPhase("Compiling transient nodes-config.yaml manifest matching native Red Hat spec...")
			_, ipNet, err := net.ParseCIDR(cfg.Network.MachineCIDR)
		prefixLen := 24
		if err == nil {
			prefixLen, _ = ipNet.Mask.Size()
		}

		dnsServer := cfg.Services.DNS.GetExternal()
		if cfg.Services.DNS.IsManaged() {
			dnsServer = cfg.Network.ControllerIP
		}

		var day2Config Day2NodesConfig
		for _, worker := range pendingBoot {
			nodeMac := ""
			for _, discovered := range state.DiscoveredNodes {
				if discovered.Hostname == worker.Hostname {
					nodeMac = discovered.MACAddress
					break
				}
			}

			hostEntry := Day2Host{
				Hostname: worker.Hostname,
				RootDeviceHints: Day2DeviceHints{
					DeviceName: daemonCfg.Paths.InstallDevice,
				},
			}

			netInterface := Day2NetInterface{
				Name:       "env2",
				Type:       "ethernet",
				State:      "up",
				MacAddress: nodeMac,
			}
			netInterface.Ipv4 = Day2Ipv4{
				Enabled: true,
				Dhcp:    false,
				Address: []Day2Addr{
					{IP: worker.IP, PrefixLength: prefixLen},
				},
			}
			netInterface.Ipv6 = Day2Ipv6{
				Enabled: false,
			}

			hostEntry.Interfaces = []Day2Interface{
				{MacAddress: nodeMac, Name: "env2"},
			}
			hostEntry.NetworkConfig.Interfaces = []Day2NetInterface{netInterface}

			if dnsServer != "" {
				hostEntry.NetworkConfig.DNSResolver = &Day2DNSResolver{
					Config: Day2DNSConfig{
						Server: []string{dnsServer},
					},
				}
			}

			if cfg.Network.Gateway != "" {
				hostEntry.NetworkConfig.Routes = &Day2Routes{
					Config: []Day2RouteConfig{
						{
							Destination:      "0.0.0.0/0",
							NextHopAddress:   cfg.Network.Gateway,
							NextHopInterface: "env2",
						},
					},
				}
			}

			day2Config.Hosts = append(day2Config.Hosts, hostEntry)
		}

		tempYAMLPath := filepath.Join(installDir, "nodes-config.yaml")
		yamlBytes, err := yaml.Marshal(day2Config)
		if err != nil {
			log.EndPhase(false, "YAML serialization failed")
			return fmt.Errorf("failed to compile transient nodes-config manifest: %w", err)
		}
		if err := os.WriteFile(tempYAMLPath, yamlBytes, 0644); err != nil {
			log.EndPhase(false, "File system write operation failed")
			return fmt.Errorf("failed to save transient nodes-config manifest to disk: %w", err)
		}

		timestamp := time.Now().Format("20060102-150405")
		yamlBackupPath := filepath.Join(installDir, fmt.Sprintf("nodes-config.yaml.backup.%s", timestamp))
		if writeErr := os.WriteFile(yamlBackupPath, yamlBytes, 0644); writeErr != nil {
			log.Warn("Failed to create a historical backup of nodes-config.yaml", "error", writeErr)
		} else {
			log.Info("Successfully archived nodes-config manifest backup", "path", yamlBackupPath)
		}
		log.EndPhase(true, "Transient scaling manifest generated and archived successfully")

		// 8. Invoke native OpenShift CLI tool to generate the custom Day-2 ISO payload
		existingIsoPath := filepath.Join(installDir, "agent.ppc64le.iso")
		if _, err := os.Stat(existingIsoPath); err == nil {
			isoBackupPath := filepath.Join(installDir, fmt.Sprintf("agent.ppc64le.iso.backup.%s", timestamp))
			log.Debug("Backing up previous deployment/scale ISO asset...", "path", isoBackupPath)
			if renameErr := os.Rename(existingIsoPath, isoBackupPath); renameErr != nil {
				log.Warn("Failed to safely backup existing ISO. Proceeding will overwrite it.", "error", renameErr)
			}
		}

		log.StartPhase("Invoking native OpenShift CLI to compile specialized node installer ISO...")

		//  Use the updated pull secret for Day-2 ISO compilation so it can authenticate to the local registry!
		// Fix: Use the original deployment 'cfg' to guarantee we catch the registry state
		pullSecretPath := cfg.OpenShift.PullSecretFile
		if cfg.Network.IsolationLevel == "air-gapped" && cfg.Services.Registry.IsManaged() {
			updatedSecretPath := filepath.Join(workspaceDir, "pull-secret-updated.json")
			if _, err := os.Stat(updatedSecretPath); err == nil {
				pullSecretPath = updatedSecretPath
				log.Debug("Airgap mode detected: Using updated pull secret for local registry authentication")
			}
		}

		isoCmd := exec.CommandContext(ctx, ocPath, "adm", "node-image", "create", "--dir", installDir, "--registry-config", pullSecretPath)
		isoCmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)

		if out, err := isoCmd.CombinedOutput(); err != nil {
			log.EndPhase(false, "OpenShift node image compilation threw an unexpected exception")
			return fmt.Errorf("node image compilation failed: %w\noutput: %s\n\ncritical hint: the initial KUBECONFIG created during deployment expires after 24 hours; if your cluster is older than a day, you must replace '%s' with a fresh cluster-admin kubeconfig to scale", err, string(out), kubeconfigPath)
		}

		generatedIso := filepath.Join(installDir, "node.ppc64le.iso")
		runtimeIso := filepath.Join(installDir, "agent.ppc64le.iso")
		if _, err := os.Stat(generatedIso); err == nil {
			if _, cpErr := localExec.Execute(ctx, fmt.Sprintf("sudo mv %s %s", generatedIso, runtimeIso)); cpErr != nil {
				log.Warn("Failed to rename node ISO to agent ISO", "error", cpErr)
			}
		}

			_ = os.Remove(tempYAMLPath)
			log.EndPhase(true, "Target configuration nodes injected directly into custom asset ISO payload")
		} else {
			log.Debug("Netboot architecture detected. Skipping ISO payload compilation.")

			// THE 24-HOUR CERTIFICATE FIX
			log.StartPhase("Extracting fresh worker ignition payload from live cluster API...")

			// Use jsonpath and base64 decode to guarantee pure JSON output without 'oc extract' CLI headers
			extractCmd := fmt.Sprintf("KUBECONFIG=%s %s get secret worker-user-data -n openshift-machine-api -o jsonpath='{.data.userData}' | base64 -d", kubeconfigPath, ocPath)

			freshIgnition, err := localExec.Execute(ctx, extractCmd)
			if err != nil || strings.TrimSpace(freshIgnition) == "" {
				log.EndPhase(false, "Failed to extract fresh worker ignition from live cluster")
				return fmt.Errorf("failed to fetch updated worker ignition (is the cluster API accessible?): %v\nOutput: %s", err, freshIgnition)
			}

			// 1. Overwrite the file in the workspace so the user sees the updated timestamp locally!
			workspaceIgnPath := filepath.Join(installDir, "worker.ign")
			if err := localExec.WriteFile(ctx, workspaceIgnPath, []byte(freshIgnition), 0644); err != nil {
				log.Warn("Failed to update worker.ign in local workspace", "error", err)
			}

			// 2. Overwrite the file on the HTTP server so the LPAR can boot
			httpIgnPath := fmt.Sprintf("/var/www/html/%s/ignition/worker.ign", cfg.OpenShift.ClusterName)
			if err := localExec.WriteFile(ctx, httpIgnPath, []byte(freshIgnition), 0644); err != nil {
				log.EndPhase(false, "Failed to stage fresh worker ignition to HTTP server")
				return fmt.Errorf("failed to overwrite stale worker.ign: %w", err)
			}

			// 3. Fix permissions so Apache doesn't throw a 403 Forbidden!
			localExec.Execute(ctx, fmt.Sprintf("sudo chown apache:apache %s 2>/dev/null || sudo chown httpd:httpd %s 2>/dev/null || true", httpIgnPath, httpIgnPath))

			log.EndPhase(true, "Fresh worker ignition staged to HTTP server successfully")

			// Ensure the HTTP server is running to serve the fresh payload
			_ = localExec.SystemctlRestart(ctx, "httpd")
		}

		// 9. Parallel LPAR provisioning and hardware boot distribution pipelines
		log.StartPhase("Distributing customized node assets to IBM Power hardware blocks...")

		// Leverage the optimized bulk parallel boot engine (auto-skips already booted nodes)
		if err := hmcProvider.BootNodes(ctx); err != nil {
			log.EndPhase(false, "Parallel hypervisor boot sequence failed")
			return err
		}

		log.EndPhase(true, "All target LPAR instances successfully power cycled with specialized assets")
	} else {
		log.Info("Bypassing ISO generation and HMC boot phases (Nodes already booted).")
	}

	// =========================================================================
	// 10. NATIVE NODE MONITORING & CSR AUTO-APPROVAL
	// =========================================================================
	log.Debug("Day-2 infrastructure initialized. Intercepting core cluster registration handshakes...")

	go orch.AutoApproveCSRs(ctx)

	var ipList []string
	for _, w := range scaleUpTargets {
		ipList = append(ipList, w.IP)
	}
	ips := strings.Join(ipList, ",")

	if cfg.Nodes.BootMethod == "agent" {
		log.StartPhase(fmt.Sprintf("Monitoring worker nodes [%s] for cluster join via Agent...", ips))

		monitorCmd := exec.CommandContext(ctx, ocPath, "adm", "node-image", "monitor", "--ip-addresses", ips)
		monitorCmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)

		var outBuf bytes.Buffer
		monitorCmd.Stdout = &outBuf
		monitorCmd.Stderr = &outBuf

		cmdErr := monitorCmd.Run()
		output := outBuf.Bytes()

		log.FileOnly().Write([]byte("\n=== NODE MONITOR OUTPUT ===\n"))
		log.FileOnly().Write(output)
		log.FileOnly().Write([]byte("\n===========================\n"))

		if cmdErr != nil {
			log.EndPhase(false, "Node monitoring failed or was interrupted")
			return fmt.Errorf("node monitoring failed: %w", cmdErr)
		}
	} else {
		log.StartPhase(fmt.Sprintf("Monitoring netboot worker nodes [%s] for API registration and readiness...", ips))

		timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		defer cancel()

		allReady := false
		for !allReady {
			select {
			case <-timeoutCtx.Done():
				log.EndPhase(false, "Timeout waiting for netboot nodes to join the cluster")
				return fmt.Errorf("timeout waiting for netboot nodes to join")
			default:
				readyCount := 0
				for _, w := range scaleUpTargets {
					checkCmd := fmt.Sprintf("KUBECONFIG=%s %s get node %s -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null", kubeconfigPath, ocPath, w.Hostname)
					if out, err := localExec.Execute(timeoutCtx, checkCmd); err == nil && strings.TrimSpace(out) == "True" {
						readyCount++
					}
				}

				if readyCount == len(scaleUpTargets) {
					allReady = true
					break
				}

				time.Sleep(30 * time.Second)
			}
		}
	}

	// SUCCESS: Safely mark all monitored targets as formally Ready!
	state, _ = stateManager.LoadState()
	for _, w := range scaleUpTargets {
		readyMarker := "ready_" + w.Hostname
		if !contains(state.CompletedPhases, readyMarker) {
			state.CompletedPhases = append(state.CompletedPhases, readyMarker)
		}
	}
	_ = stateManager.SaveState(state)

	log.EndPhase(true, fmt.Sprintf("All %d Day-2 worker nodes have successfully joined the cluster.", len(scaleUpTargets)))
	log.Info("=== Scale Operation Completed Successfully ===")
	return nil
}

// contains function is for search a string in a slice of strings
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
