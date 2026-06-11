package cmd

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.ibm.com/sudeeshjohn/shiftlaunch/infra/compute"
	"github.ibm.com/sudeeshjohn/shiftlaunch/infra/controller"
	"github.ibm.com/sudeeshjohn/shiftlaunch/localexec"
	"github.ibm.com/sudeeshjohn/shiftlaunch/services"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

type Day2NodesConfig struct {
	Hosts []Day2Host `yaml:"hosts"`
}

type Day2Host struct {
	Hostname        string            `yaml:"hostname"`
	RootDeviceHints Day2DeviceHints   `yaml:"rootDeviceHints"`
	Interfaces      []Day2Interface   `yaml:"interfaces"`
	NetworkConfig   Day2NetworkConfig `yaml:"networkConfig"`
}

type Day2DeviceHints struct {
	DeviceName string `yaml:"deviceName"`
}

type Day2Interface struct {
	MacAddress string `yaml:"macAddress"`
	Name       string `yaml:"name"`
}

type Day2NetworkConfig struct {
	Interfaces  []Day2NetInterface `yaml:"interfaces"`
	DNSResolver *Day2DNSResolver   `yaml:"dns-resolver,omitempty"`
	Routes      *Day2Routes        `yaml:"routes,omitempty"`
}

type Day2DNSResolver struct {
	Config Day2DNSConfig `yaml:"config"`
}

type Day2DNSConfig struct {
	Server []string `yaml:"server"`
}

type Day2Routes struct {
	Config []Day2RouteConfig `yaml:"config"`
}

type Day2RouteConfig struct {
	Destination      string `yaml:"destination"`
	NextHopAddress   string `yaml:"next-hop-address"`
	NextHopInterface string `yaml:"next-hop-interface"`
}

type Day2NetInterface struct {
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`
	State      string   `yaml:"state"`
	MacAddress string   `yaml:"mac-address"`
	Ipv4       Day2Ipv4 `yaml:"ipv4"`
	Ipv6       Day2Ipv6 `yaml:"ipv6"`
}

type Day2Ipv6 struct {
	Enabled bool `yaml:"enabled"`
}

type Day2Ipv4 struct {
	Enabled bool       `yaml:"enabled"`
	Address []Day2Addr `yaml:"address"`
	Dhcp    bool       `yaml:"dhcp"`
}

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

func init() {
	rootCmd.AddCommand(scaleCmd)
}

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

	updatedCfg.Controller.IP = cfg.Controller.IP

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
				log.Info("Successfully archived original cluster config.yaml backup", "path", configBackupPath)
				
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

	// 3. THE SMART RESUME FIX: Differentiate between nodes needing Boot vs Monitoring
	var scaleTargets []types.NodeConfig
	var pendingBoot []types.NodeConfig
	
	localExec := localexec.NewLocalClient(log)
	ocPath := filepath.Join(workspaceDir, "tools", "oc")
	kubeconfigPath := filepath.Join(installDir, "auth", "kubeconfig")

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

		scaleTargets = append(scaleTargets, worker)

		// If it hasn't been powered on by the HMC yet, it needs a boot
		bootMarker := "booted_" + worker.Hostname
		if !contains(state.CompletedPhases, bootMarker) {
			pendingBoot = append(pendingBoot, worker)
		}
	}

	if len(scaleTargets) == 0 {
		log.Debug("No pending worker nodes detected. All nodes in config have already successfully joined the cluster.")
		return nil
	}

	log.Debug(fmt.Sprintf("Detected %d worker(s) scaling up. %d require ISO/booting, %d require monitoring.", len(scaleTargets), len(pendingBoot), len(scaleTargets)-len(pendingBoot)))

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
		
		log.Info("Regenerating DNS routing rules locally...")
		dnsmasq := services.NewDNSmasqManager(cfg, daemonCfg, localExec)
		if err := dnsmasq.SetupDNS(ctx); err != nil {
			log.EndPhase(false, "Failed to regenerate DNS")
			return fmt.Errorf("failed to regenerate DNS configurations: %w", err)
		}

		log.Info("Regenerating HAProxy backend pools locally...")
		if err := services.SetupHAProxy(ctx, cfg, localExec); err != nil {
			log.EndPhase(false, "Failed to regenerate HAProxy")
			return fmt.Errorf("failed to regenerate HAProxy configurations: %w", err)
		}

		_ = localExec.SystemctlRestart(ctx, "dnsmasq")
		log.EndPhase(true, "Local network core tables cleanly regenerated to support new compute infrastructure")

		// 6. Guarantee Local API Resolution
		log.Debug("Ensuring OpenShift API is reachable from controller...")
		netMgr := controller.NewNetworkManager(localExec, debug, log)
		_ = netMgr.AddHostsEntry(ctx, cfg.OpenShift.ClusterName, cfg.OpenShift.BaseDomain, cfg.Network.LoadBalancerIP)

		// 7. Build the temporary Red Hat spec nodes-config.yaml file programmatically
		log.StartPhase("Compiling transient nodes-config.yaml manifest matching native Red Hat spec...")
		_, ipNet, err := net.ParseCIDR(cfg.Network.MachineCIDR)
		prefixLen := 24
		if err == nil {
			prefixLen, _ = ipNet.Mask.Size()
		}

		dnsServer := cfg.Network.Nameserver
		if cfg.ManagedServices.DNS {
			dnsServer = cfg.Controller.IP
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
		
		// THE FIX: Use the updated pull secret for Day-2 ISO compilation so it can authenticate to the local registry!
		pullSecretPath := cfg.OpenShift.PullSecretFile
		if updatedCfg.DisconnectedConfig.Enabled && updatedCfg.ManagedServices.Registry {
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
			return fmt.Errorf("node image compilation failed: %w\nOutput: %s\n\n[!] CRITICAL HINT: The initial KUBECONFIG created during deployment expires after 24 hours. If your cluster is older than a day, you must replace '%s' with a fresh cluster-admin kubeconfig to scale!", err, string(out), kubeconfigPath)
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
	log.Info("Day-2 infrastructure initialized. Intercepting core cluster registration handshakes...")

	go orch.AutoApproveCSRs(ctx)

	var ipList []string
	for _, w := range scaleTargets {
		ipList = append(ipList, w.IP)
	}
	ips := strings.Join(ipList, ",")

	log.StartPhase(fmt.Sprintf("Monitoring worker nodes [%s] for cluster join...", ips))

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

	// SUCCESS: Safely mark all monitored targets as formally Ready!
	state, _ = stateManager.LoadState()
	for _, w := range scaleTargets {
		readyMarker := "ready_" + w.Hostname
		if !contains(state.CompletedPhases, readyMarker) {
			state.CompletedPhases = append(state.CompletedPhases, readyMarker)
		}
	}
	_ = stateManager.SaveState(state)

	log.EndPhase(true, "All Day-2 worker nodes have successfully joined the cluster.")
	log.Info("=== Scale Operation Completed Successfully ===")
	return nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}