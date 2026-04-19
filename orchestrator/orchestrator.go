package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sudeeshjohn/shiftlaunch/config"
	compute "github.com/sudeeshjohn/shiftlaunch/infra/compute"
	"github.com/sudeeshjohn/shiftlaunch/infra/controller"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/services"
	"github.com/sudeeshjohn/shiftlaunch/types"
	"go.yaml.in/yaml/v3"
)

// Orchestrator manages the local execution of the BYOI Boot Agent
type Orchestrator struct {
	cfg          *types.AgentConfig
	daemonCfg    *config.AgentDaemonConfig
	logger       *logger.Logger
	executor     *localexec.LocalClient
	workspaceDir string
	state        *types.DeploymentState
	stateManager *types.StateManager
	debug        bool
}

// NewOrchestrator initializes the phase engine and loads/creates the state tracker
func NewOrchestrator(cfg *types.AgentConfig, daemonCfg *config.AgentDaemonConfig, log *logger.Logger, workspaceDir string, debug bool) *Orchestrator {
	stateManager := types.NewStateManager(cfg.OpenShift.ClusterName)
	
	// Attempt to load existing state for resumes
	state, err := stateManager.LoadState()
	if err != nil || state == nil {
		// Fresh deployment
		state = &types.DeploymentState{
			ClusterName:     cfg.OpenShift.ClusterName,
			Status:          "in_progress",
			StartTime:       time.Now().Format(time.RFC3339),
			CompletedPhases: []string{},
		}
	}

	return &Orchestrator{
		cfg:          cfg,
		daemonCfg:    daemonCfg,
		logger:       log,
		executor:     localexec.NewLocalClient(log),
		workspaceDir: workspaceDir,
		state:        state,
		stateManager: stateManager,
		debug:        debug,
	}
}
// GetDebug returns the debug flag status
func (o *Orchestrator) GetDebug() bool {
    return o.debug
}

// saveState records phase completion to the local state.json file
func (o *Orchestrator) saveState(phase string) {
	o.state.CurrentPhase = phase
	if !contains(o.state.CompletedPhases, phase) {
		o.state.CompletedPhases = append(o.state.CompletedPhases, phase)
	}
	_ = o.stateManager.SaveState(o.state)
}

// startPhase records the start of a phase execution
func (o *Orchestrator) startPhase(phase string) *types.PhaseExecution {
	phaseExec := &types.PhaseExecution{
		Phase:     phase,
		StartTime: time.Now().Format(time.RFC3339),
		Status:    "in_progress",
	}
	return phaseExec
}

// endPhase records the completion of a phase execution
func (o *Orchestrator) endPhase(phaseExec *types.PhaseExecution, err error) {
	phaseExec.EndTime = time.Now().Format(time.RFC3339)
	
	// Calculate duration
	if startTime, parseErr := time.Parse(time.RFC3339, phaseExec.StartTime); parseErr == nil {
		if endTime, parseErr := time.Parse(time.RFC3339, phaseExec.EndTime); parseErr == nil {
			phaseExec.Duration = endTime.Sub(startTime).String()
		}
	}
	
	if err != nil {
		phaseExec.Status = "failed"
		phaseExec.Error = err.Error()
	} else {
		phaseExec.Status = "success"
	}
	
	o.stateManager.AddPhaseExecution(o.state, *phaseExec)
	o.stateManager.SaveState(o.state)
}

// Deploy executes the linear installation pipeline
func (o *Orchestrator) Deploy(ctx context.Context,resume bool) (err error) {
	// Check if cluster was previously deleted
	if o.stateManager.IsDeleted() && !resume {
		o.logger.Info("⚠️  Cluster was previously deleted. Clearing deleted marker for new deployment...", "cluster", o.cfg.OpenShift.ClusterName)
		if err := o.stateManager.ClearDeleted(); err != nil {
			return fmt.Errorf("failed to clear deleted marker: %w", err)
		}
		// Reset state for fresh deployment
		o.state = &types.DeploymentState{
			ClusterName:     o.cfg.OpenShift.ClusterName,
			Status:          "in_progress",
			StartTime:       time.Now().Format(time.RFC3339),
			CompletedPhases: []string{},
		}
	}

	// Acquire lock to prevent concurrent deployments
	if err := o.stateManager.AcquireLock(); err != nil {
		return fmt.Errorf("failed to acquire cluster lock: %w", err)
	}

	// Ensure lock is released and correct state markers are applied, even on panic/error
	defer func() {
		if err != nil {
			o.state.Status = "failed"
			o.stateManager.MarkFailed()
		} else {
			o.state.Status = "completed"
			o.state.CurrentPhase = "done"
			o.state.EndTime = time.Now().Format(time.RFC3339)
			o.stateManager.MarkManaged()
			o.stateManager.ClearFailed()
		}
		_ = o.stateManager.SaveState(o.state)
		
		if lockErr := o.stateManager.ReleaseLock(); lockErr != nil {
			o.logger.Warn("Failed to release lock", "error", lockErr)
		}
	}()

	o.logger.Info("🚀 Starting ShiftLaunch Local Agent Orchestration...", "cluster", o.cfg.OpenShift.ClusterName)

	// --- PHASE 0: VALIDATION (Only for fresh deployments) ---
	if !resume || len(o.state.CompletedPhases) == 0 {
		o.logger.Info("\n[Phase 0] Pre-Deployment Validation")
		
		// Validate VIP is not in use
		if o.cfg.ManagedServices.LoadBalancer && o.cfg.Network.LoadBalancerIP != "" {
			o.logger.Info("Validating VIP availability...", "vip", o.cfg.Network.LoadBalancerIP)
			
			// Check if VIP is configured on interface
			iface := o.cfg.Controller.NetworkInterface
			if iface != "" {
				output, err := o.executor.Execute(ctx,fmt.Sprintf("ip addr show %s", iface))
				if err == nil && strings.Contains(output, o.cfg.Network.LoadBalancerIP+"/") {
					// VIP is configured - check which cluster is using it
					conflictingCluster := o.findClusterUsingVIP(o.cfg.Network.LoadBalancerIP)
					if conflictingCluster != "" {
						return fmt.Errorf("VIP %s is already in use by cluster '%s'. Please choose a different loadbalancer_ip or delete the conflicting cluster first",
							o.cfg.Network.LoadBalancerIP, conflictingCluster)
					}
					return fmt.Errorf("VIP %s is already configured on interface %s. Please remove the VIP alias manually or choose a different loadbalancer_ip",
						o.cfg.Network.LoadBalancerIP, iface)
				}
			}
			
			// Check if VIP is defined in another cluster's config
			conflictingCluster := o.findClusterUsingVIP(o.cfg.Network.LoadBalancerIP)
			if conflictingCluster != "" {
				return fmt.Errorf("VIP %s is already configured for cluster '%s'. Please choose a different loadbalancer_ip",
					o.cfg.Network.LoadBalancerIP, conflictingCluster)
			}
			
			o.logger.Info("✓ VIP is available", "vip", o.cfg.Network.LoadBalancerIP)
		}
	}

// --- PHASE 1: DISCOVERY ---
	if !resume || !contains(o.state.CompletedPhases, "discovery") {
		phaseExec := o.startPhase("discovery")
		o.logger.Info("\n[Phase 1] Pre-Flight & HMC Discovery")
		
		var phaseErr error
		func() {
			provider, err := compute.NewProvider(o.cfg, o.logger, o.debug)
			if err != nil {
				phaseErr = fmt.Errorf("failed to initialize compute provider: %w", err)
				return
			}
			defer func() {
				if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
					hmcProvider.Cleanup()
				}
			}()
			
			if err := provider.DiscoverMetadata(context.Background()); err != nil {
				phaseErr = fmt.Errorf("failed to discover LPAR metadata from HMC: %w", err)
				return
			}
		}()
		
		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("discovery")
	}
	// --- PHASE 2: DOWNLOADS ---
	if !resume || !contains(o.state.CompletedPhases, "downloads") {
		phaseExec := o.startPhase("downloads")
		o.logger.Info("\n[Phase 2] Downloading OpenShift Artifacts")
		
		downloader := services.NewDownloader(o.cfg, o.daemonCfg, o.executor, o.logger)
		phaseErr := downloader.DownloadAll(ctx,o.workspaceDir)
		
		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("downloads")
	}

	// --- PHASE 3: MANAGED SERVICES ---
	if !resume || !contains(o.state.CompletedPhases, "services") {
		phaseExec := o.startPhase("services")
		o.logger.Info("\n[Phase 3] Configuring Managed Infrastructure Services")

		var phaseErr error
		func() {
			setup := services.NewControllerSetup(o.cfg, o.daemonCfg, o.executor, o.logger)
			if err := setup.InstallPackages(ctx); err != nil {
				phaseErr = fmt.Errorf("failed to setup controller dependencies: %w", err)
				return
			}
			if err := setup.ConfigureFirewall(ctx); err != nil {
				phaseErr = fmt.Errorf("failed to configure local firewall: %w", err)
				return
			}

			if o.cfg.ManagedServices.LoadBalancer {
				o.logger.Info(" -> Configuring Local HAProxy...")

				// 1. Allow HAProxy to bind to the VIP immediately (fixes the systemd crash)
				o.executor.Execute(ctx,"sudo sysctl -w net.ipv4.ip_nonlocal_bind=1")
				
				// 2. Allow HAProxy to route OpenShift's custom ports through SELinux
				o.executor.Execute(ctx,"sudo setsebool -P haproxy_connect_any 1")

				// 3. Bind the VIP to the controller's physical network interface
				netMgr := controller.NewNetworkManager(o.executor, o.debug, o.logger)
				vip := o.cfg.Network.LoadBalancerIP
				iface := o.cfg.Controller.NetworkInterface
				cidr := o.cfg.Network.MachineCIDR
				
				if err := netMgr.AddVIPAlias(ctx,iface, vip, cidr); err != nil {
					o.logger.Warn("Failed to configure VIP via nmcli. HAProxy will start, but routing may fail.", "error", err)
				}

				// 4. Generate config and start the service
				if err := services.SetupHAProxy(ctx,o.cfg, o.executor); err != nil {
					phaseErr = err
					return
				}
			} else {
				o.logger.Info(" -> Skipping HAProxy (User Managed)")
			}

			dnsmasq := services.NewDNSmasqManager(o.cfg, o.daemonCfg, o.executor)

			if o.cfg.ManagedServices.DNS {
				o.logger.Info(" -> Configuring Local DNS...")
				if err := dnsmasq.SetupDNS(ctx); err != nil {
					phaseErr = err
					return
				}
			} else {
				o.logger.Info(" -> Skipping DNS (User Managed)")
			}

			if o.cfg.ManagedServices.DHCP {
				o.logger.Info(" -> Configuring Local DHCP...")
				if err := dnsmasq.SetupDHCP(ctx); err != nil {
					phaseErr = err
					return
				}
			} else {
				o.logger.Info(" -> Skipping DHCP (User Managed)")
			}

			if o.cfg.ManagedServices.PXE {
				o.logger.Info(" -> Configuring Local PXE Service...")
				if err := dnsmasq.SetupPXEService(ctx); err != nil {
					phaseErr = err
					return
				}
				o.logger.Info(" -> Staging PXE Artifacts...")
				if err := dnsmasq.ConfigurePXEBoot(ctx,o.workspaceDir); err != nil {
					phaseErr = err
					return
				}
			} else {
				o.logger.Info(" -> Skipping PXE (User Managed)")
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("services")
	}

	// --- PHASE 4: IGNITION GENERATION ---
	if !resume || !contains(o.state.CompletedPhases, "ignition") {
		phaseExec := o.startPhase("ignition")
		o.logger.Info("\n[Phase 4] Generating OpenShift Ignition Payload")
		
		var phaseErr error
		func() {
			if err := services.GenerateIgnition(ctx,o.cfg, o.executor, o.workspaceDir); err != nil {
				phaseErr = err
				return
			}

			o.logger.Info(" -> Setting up Local HTTP Server (Port 8080)...")
			
			// --- NEW: Configure and Start Apache HTTPD ---
			if err := services.ConfigureHTTPD(ctx,o.executor, o.daemonCfg.Network.HTTPPort); err != nil {
				phaseErr = err
				return
			}

			httpServer := services.NewHTTPServerManager(o.cfg, o.daemonCfg, o.executor, o.logger)
			if err := httpServer.Setup(ctx,o.workspaceDir); err != nil {
				phaseErr = err
				return
			}
			
			o.logger.Info(" -> Staging files to HTTP Server...")
			if err := httpServer.StageFiles(ctx,o.workspaceDir); err != nil {
				phaseErr = err
				return
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("ignition")
	}

	// --- PHASE 5: BOOT ---
	if !resume || !contains(o.state.CompletedPhases, "boot") {
		phaseExec := o.startPhase("boot")
		o.logger.Info("\n[Phase 5] Initiating Cluster Boot")
		
		var phaseErr error
		func() {
			provider, err := compute.NewProvider(o.cfg, o.logger, o.debug)
			if err != nil {
				phaseErr = err
				return
			}
			// Ensure HMC session is cleaned up after boot phase
			defer func() {
				if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
					hmcProvider.Cleanup()
				}
			}()

			// Re-discover metadata if resuming (UUIDs may not be populated)
			if resume {
				o.logger.Info("Re-discovering LPAR metadata for resume...")
				if err := provider.DiscoverMetadata(context.Background()); err != nil {
					phaseErr = fmt.Errorf("failed to re-discover LPAR metadata: %w", err)
					return
				}
			}

			// Iterate through nodes and check individual boot states
			for _, node := range o.cfg.GetAllNodes() {
				bootMarker := "booted_" + node.Hostname
				
				// Skip this specific node if it was successfully booted in a previous run
				if resume && contains(o.state.CompletedPhases, bootMarker) {
					o.logger.Info("Skipping already booted node", "node", node.Hostname)
					continue
				}

				if err := provider.BootNode(context.Background(), node); err != nil {
					phaseErr = fmt.Errorf("HMC boot sequence failed for %s: %w", node.Hostname, err)
					return
				}
				
				// Mark this specific node as successfully booted in state.json
				o.saveState(bootMarker)
			}
		}()
		
		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		// Mark the entire phase as fully complete
		o.saveState("boot")
	}

	// --- PHASE 6: WAIT FOR INSTALLATION ---
	if !resume || !contains(o.state.CompletedPhases, "wait") {
		phaseExec := o.startPhase("wait")
		o.logger.Info("\n[Phase 6] Waiting for OpenShift Installation")
		
		var phaseErr error
		func() {
			if err := o.WaitForBootstrap(context.Background()); err != nil {
				phaseErr = err
				return
			}

			if err := o.WaitForInstall(context.Background()); err != nil {
				phaseErr = err
				return
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("wait")
	}

	// Deployment Complete! Update State.
/* 	o.state.Status = "completed"
	o.state.CurrentPhase = "done"
	o.state.EndTime = time.Now().Format(time.RFC3339)
	_ = o.stateManager.SaveState(o.state) */

	o.logger.Info("\n🎉 ShiftLaunch Agent Execution Complete! OpenShift is ready.")
	return nil
}


// GetClusterStatus returns a formatted string of the current deployment state
func (o *Orchestrator) GetClusterStatus() string {
	state := o.state
	
	status := fmt.Sprintf(`Cluster: %s
Status: %s
Current Phase: %s
Completed Phases: %v
Started: %s
`, state.ClusterName, state.Status, state.CurrentPhase, state.CompletedPhases, state.StartTime)

	if state.EndTime != "" {
		status += fmt.Sprintf("Ended: %s\n", state.EndTime)
	}

	if state.Status == "completed" {
		status += "\n=== Service Endpoints ===\n"
		baseDomain := o.cfg.OpenShift.BaseDomain
		clusterDomain := fmt.Sprintf("%s.%s", state.ClusterName, baseDomain)
		
		status += fmt.Sprintf("API Server:       https://api.%s:6443\n", clusterDomain)
		status += fmt.Sprintf("Web Console:      https://console-openshift-console.apps.%s\n", clusterDomain)
		
		// Add kubeadmin credentials if available locally
		kubeconfigPath := filepath.Join(o.workspaceDir, "install-dir", "auth", "kubeconfig")
		pwPath := filepath.Join(o.workspaceDir, "install-dir", "auth", "kubeadmin-password")
		
		if _, err := os.Stat(kubeconfigPath); err == nil {
			status += fmt.Sprintf("\nKubeconfig:       %s\n", kubeconfigPath)
		}
		if pwData, err := os.ReadFile(pwPath); err == nil {
			status += fmt.Sprintf("Password:         %s\n", string(pwData))
		}
	}

	return status
}

// DumpConfigs outputs required configuration records for Enterprise Admins
func (o *Orchestrator) DumpConfigs() error {
	o.logger.Info("Dumping configuration requirements for unmanaged services...")

	if !o.cfg.ManagedServices.DNS {
		fmt.Printf("\n--- REQUIRED DNS RECORDS ---\n")
		fmt.Printf("%s\tapi.%s.%s\n", o.cfg.Network.LoadBalancerIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
		fmt.Printf("%s\tapi-int.%s.%s\n", o.cfg.Network.LoadBalancerIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
		fmt.Printf("%s\t*.apps.%s.%s\n", o.cfg.Network.LoadBalancerIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
	}

	if !o.cfg.ManagedServices.LoadBalancer {
		fmt.Printf("\n--- REQUIRED LOAD BALANCER POOLS (Target: %s) ---\n", o.cfg.Network.LoadBalancerIP)
		fmt.Println("Port 6443 (API)   -> Masters & Bootstrap")
		fmt.Println("Port 22623 (MCS)  -> Masters & Bootstrap")
		fmt.Println("Port 80/443 (App) -> Workers (or Masters if SNO/Compact)")
	}

	if !o.cfg.ManagedServices.DHCP {
		fmt.Printf("\n--- REQUIRED DHCP OPTIONS ---\n")
		fmt.Printf("Option 66 (Next-Server): %s\n", o.cfg.Controller.IP)
		fmt.Printf("Option 67 (Bootfile):    %s/core.elf\n", o.cfg.OpenShift.ClusterName)
	}

	return nil
}

// Helper to check if a string exists in a slice
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// findClusterUsingVIP searches all managed clusters to find if any is using the given VIP
func (o *Orchestrator) findClusterUsingVIP(vip string) string {
	// Get workspace parent directory
	workspaceParent := filepath.Dir(o.workspaceDir)
	
	// List all directories in workspace
	entries, err := os.ReadDir(workspaceParent)
	if err != nil {
		return "" // Can't check, return empty
	}
	
	currentCluster := o.cfg.OpenShift.ClusterName
	
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		
		clusterName := entry.Name()
		
		// Skip current cluster
		if clusterName == currentCluster {
			continue
		}
		
		// Check if cluster is managed OR failed (both mean the cluster occupies the VIP)
		managedMarker := filepath.Join(workspaceParent, clusterName, ".managed")
		failedMarker := filepath.Join(workspaceParent, clusterName, ".failed")
		
		if _, err1 := os.Stat(managedMarker); os.IsNotExist(err1) {
			if _, err2 := os.Stat(failedMarker); os.IsNotExist(err2) {
				continue // Not a managed or failed cluster
			}
		}	
		
		// Check if cluster is deleted
		deletedMarker := filepath.Join(workspaceParent, clusterName, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			continue // Cluster is deleted, skip
		}
		
		// Read the cluster's config
		configPath := filepath.Join(workspaceParent, clusterName, "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue // Can't read config
		}
		
		// Safely parse the YAML to guarantee an exact value match
		var tempCfg types.AgentConfig
		if err := yaml.Unmarshal(data, &tempCfg); err == nil {
			if tempCfg.Network.LoadBalancerIP == vip {
				return clusterName
			}
		}
	}
	
	return "" // No conflict found
}
// GetLogger returns the orchestrator's logger instance
func (o *Orchestrator) GetLogger() *logger.Logger {
	return o.logger
}
// GetClusterName returns the cluster name from the configuration
func (o *Orchestrator) GetClusterName() string {
	return o.cfg.OpenShift.ClusterName
}

