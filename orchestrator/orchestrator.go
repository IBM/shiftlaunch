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
			StateVersion:    2, // Current version with granular tracking
			ClusterName:     cfg.OpenShift.ClusterName,
			Status:          "in_progress",
			StartTime:       time.Now().Format(time.RFC3339),
			CompletedPhases: []string{},
			CompletedEvents: []string{},
			DownloadProgress: make(map[string]types.DownloadProgress),
			NodeBootStatus:   make(map[string]types.NodeBootStatus),
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
	// --- CRITICAL FIX: Sync completely instead of manually copying specific fields ---
	if currentState, err := o.stateManager.LoadState(); err == nil && currentState != nil {
		o.state = currentState 
	}
	
	o.state.CurrentPhase = phase
	if !contains(o.state.CompletedPhases, phase) {
		o.state.CompletedPhases = append(o.state.CompletedPhases, phase)
	}
	_ = o.stateManager.SaveState(o.state)
}

// trackServiceStart records the start of a service configuration
func (o *Orchestrator) trackServiceStart(name, serviceType string, managed bool) *types.ConfiguredService {
	return &types.ConfiguredService{
		Name:      name,
		Type:      serviceType,
		Status:    "configuring",
		Managed:   managed,
		StartedAt: time.Now().Format(time.RFC3339),
	}
}

// trackServiceEnd records the completion of a service configuration
func (o *Orchestrator) trackServiceEnd(svc *types.ConfiguredService, err error, details string) {
	svc.CompletedAt = time.Now().Format(time.RFC3339)
	
	// Calculate duration
	if startTime, parseErr := time.Parse(time.RFC3339, svc.StartedAt); parseErr == nil {
		if endTime, parseErr := time.Parse(time.RFC3339, svc.CompletedAt); parseErr == nil {
			duration := endTime.Sub(startTime)
			svc.Duration = duration.Round(time.Millisecond).String()
		}
	}
	
	if err != nil {
		svc.Status = "failed"
		svc.Error = err.Error()
	} else {
		svc.Status = "completed"
	}
	
	if details != "" {
		svc.Details = details
	}
	
	// Add to state
	o.state.ConfiguredServices = append(o.state.ConfiguredServices, *svc)
	
	// Save state immediately to persist service tracking
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
	
	// --- CRITICAL FIX: Sync in-memory state with the disk to preserve provider updates ---
	if currentState, loadErr := o.stateManager.LoadState(); loadErr == nil && currentState != nil {
		o.state = currentState 
	}
	
	o.stateManager.AddPhaseExecution(o.state, *phaseExec)
	o.stateManager.SaveState(o.state)
}

// Deploy executes the linear installation pipeline
func (o *Orchestrator) Deploy(ctx context.Context, resume bool) (err error) {
	// Check if cluster was previously deleted
	if o.stateManager.IsDeleted() && !resume {
		o.logger.Info(" Cluster was previously deleted. Clearing deleted marker for new deployment...", "cluster", o.cfg.OpenShift.ClusterName)
		if err := o.stateManager.ClearDeleted(); err != nil {
			return fmt.Errorf("failed to clear deleted marker: %w", err)
		}
		// Reset state for fresh deployment
		o.state = &types.DeploymentState{
			StateVersion:     2, // Current version with granular tracking
			ClusterName:      o.cfg.OpenShift.ClusterName,
			Status:           "in_progress",
			StartTime:        time.Now().Format(time.RFC3339),
			CompletedPhases:  []string{},
			CompletedEvents:  []string{},
			DownloadProgress: make(map[string]types.DownloadProgress),
			NodeBootStatus:   make(map[string]types.NodeBootStatus),
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

	o.logger.Info("Starting ShiftLaunch Local Agent Orchestration...", "cluster", o.cfg.OpenShift.ClusterName)

	// --- PHASE 0: VALIDATION (Only for fresh deployments) ---
	if !resume || len(o.state.CompletedPhases) == 0 {
		o.logger.Phase("[Phase 0] Pre-Deployment Validation")
		
		// Validate VIP is not in use
		if o.cfg.ManagedServices.LoadBalancer && o.cfg.Network.LoadBalancerIP != "" {
			o.logger.Info("Validating VIP availability...", "vip", o.cfg.Network.LoadBalancerIP)
			
			// Check if VIP is configured on interface
			iface := o.cfg.Controller.NetworkInterface
			if iface != "" {
				output, err := o.executor.Execute(ctx, fmt.Sprintf("ip addr show %s", iface))
				if err == nil && strings.Contains(output, o.cfg.Network.LoadBalancerIP+"/") {
					// VIP is configured - check which cluster is using it
					conflictingCluster := o.findClusterUsingVIP(o.cfg.Network.LoadBalancerIP)
					if conflictingCluster != "" {
						o.logger.Error("VIP is already in use by another cluster",
							"vip", o.cfg.Network.LoadBalancerIP,
							"cluster", conflictingCluster)
						return fmt.Errorf("VIP %s is already in use by cluster '%s'. Please choose a different loadbalancer_ip or delete the conflicting cluster first",
							o.cfg.Network.LoadBalancerIP, conflictingCluster)
					}
					o.logger.Error("VIP is already configured on interface",
						"vip", o.cfg.Network.LoadBalancerIP,
						"interface", iface)
					return fmt.Errorf("VIP %s is already configured on interface %s. Please remove the VIP alias manually or choose a different loadbalancer_ip",
						o.cfg.Network.LoadBalancerIP, iface)
				}
			}
			
			// Check if VIP is defined in another cluster's config
			conflictingCluster := o.findClusterUsingVIP(o.cfg.Network.LoadBalancerIP)
			if conflictingCluster != "" {
				o.logger.Error("VIP is already configured for another cluster",
					"vip", o.cfg.Network.LoadBalancerIP,
					"cluster", conflictingCluster)
				return fmt.Errorf("VIP %s is already configured for cluster '%s'. Please choose a different loadbalancer_ip",
					o.cfg.Network.LoadBalancerIP, conflictingCluster)
			}
			
			o.logger.Info("VIP is available", "vip", o.cfg.Network.LoadBalancerIP)
		}
	}
	// --- RESTORE IN-MEMORY STATE FOR RESUMES ---
	if resume && len(o.state.DiscoveredNodes) > 0 {
		o.logger.Info("Restoring discovered node metadata from state file...")
		for _, discovered := range o.state.DiscoveredNodes {
			for _, node := range o.cfg.GetAllNodes() {
				if node.Hostname == discovered.Hostname {
					node.MACAddress = discovered.MACAddress
					node.UUID = discovered.UUID
					node.ProfileUUID = discovered.ProfileUUID
					node.LocationCode = discovered.LocationCode
				}
			}
		}
	}

	// --- PHASE 1: DISCOVERY ---
	if !resume || !contains(o.state.CompletedPhases, "discovery") {
		phaseExec := o.startPhase("discovery")
		o.logger.Phase("[Phase 1] Pre-Flight & HMC Discovery")
		
		var phaseErr error
		func() {
			provider, err := compute.NewProviderWithState(o.cfg, o.logger, o.debug, o.stateManager)
			if err != nil {
				phaseErr = fmt.Errorf("failed to initialize compute provider: %w", err)
				return
			}
			defer func() {
				if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
					hmcProvider.Cleanup()
				}
			}()
			
			if err := provider.DiscoverMetadata(ctx); err != nil {
				o.logger.Error("Failed to discover LPAR metadata from HMC", "error", err)
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
	needsDownloads := !resume || !contains(o.state.CompletedPhases, "downloads")

	// --- FIX: LBYL Safety Check for Missing Binaries ---
	if !needsDownloads {
		installerPath := filepath.Join(o.workspaceDir, "tools", "openshift-install")
		if _, err := os.Stat(installerPath); os.IsNotExist(err) {
			o.logger.Warn(" Missing required OpenShift binaries in workspace. Forcing download phase recovery...")
			needsDownloads = true
		}
	}

	if needsDownloads {
		phaseExec := o.startPhase("downloads")
		o.logger.Phase("[Phase 2] Downloading OpenShift Artifacts")
		
		downloader := services.NewDownloader(o.cfg, o.daemonCfg, o.executor, o.logger)
		phaseErr := downloader.DownloadAll(ctx, o.workspaceDir)
		
		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("downloads")
	}

	// --- PHASE 3: MANAGED SERVICES ---
	if !resume || !contains(o.state.CompletedPhases, "services") {
		phaseExec := o.startPhase("services")
		o.logger.Phase("[Phase 3] Configuring Managed Infrastructure Services")

		var phaseErr error
		func() {
			// Track package installation
			pkgSvc := o.trackServiceStart("controller-packages", "packages", true)
			setup := services.NewControllerSetup(o.cfg, o.daemonCfg, o.executor, o.logger)
			if err := setup.InstallPackages(ctx); err != nil {
				o.trackServiceEnd(pkgSvc, err, "")
				phaseErr = fmt.Errorf("failed to setup controller dependencies: %w", err)
				return
			}
			o.trackServiceEnd(pkgSvc, nil, "Installed required packages")
			
			// Track firewall configuration
			fwSvc := o.trackServiceStart("firewall", "firewall", true)
			if err := setup.ConfigureFirewall(ctx); err != nil {
				o.trackServiceEnd(fwSvc, err, "")
				phaseErr = fmt.Errorf("failed to configure local firewall: %w", err)
				return
			}
			o.trackServiceEnd(fwSvc, nil, "Configured firewall rules")

			// Track HAProxy configuration
			if o.cfg.ManagedServices.LoadBalancer {
				haproxySvc := o.trackServiceStart("haproxy", "load-balancer", true)
				haproxySvc.ServiceName = "haproxy"
				haproxySvc.ConfigFile = "/etc/haproxy/haproxy.cfg"
				
				o.logger.Info("Configuring Local HAProxy...")

				o.executor.Execute(ctx, "sudo sysctl -w net.ipv4.ip_nonlocal_bind=1")
				o.executor.Execute(ctx, "sudo setsebool -P haproxy_connect_any 1")
				o.executor.Execute(ctx, "sudo semanage port -a -t http_port_t -p tcp 6443 2>/dev/null || true")
				o.executor.Execute(ctx, "sudo semanage port -m -t http_port_t -p tcp 6443 2>/dev/null || true")
				o.executor.Execute(ctx, "sudo semanage port -a -t http_port_t -p tcp 22623 2>/dev/null || true")
				o.executor.Execute(ctx, "sudo semanage port -m -t http_port_t -p tcp 22623 2>/dev/null || true")

				netMgr := controller.NewNetworkManager(o.executor, o.debug, o.logger)
				vip := o.cfg.Network.LoadBalancerIP
				iface := o.cfg.Controller.NetworkInterface
				cidr := o.cfg.Network.MachineCIDR
				
				if err := netMgr.AddVIPAlias(ctx, iface, vip, cidr); err != nil {
					o.trackServiceEnd(haproxySvc, err, "")
					phaseErr = fmt.Errorf("FATAL: Failed to configure VIP alias '%s' on interface '%s': %w", vip, iface, err)
					return
				}

				if err := services.SetupHAProxy(ctx, o.cfg, o.executor); err != nil {
					o.trackServiceEnd(haproxySvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(haproxySvc, nil, fmt.Sprintf("Load balancer configured on %s", vip))
			} else {
				o.logger.Info("Skipping HAProxy (User Managed)")
				skipSvc := o.trackServiceStart("haproxy", "load-balancer", false)
				o.trackServiceEnd(skipSvc, nil, "User managed")
			}

			dnsmasq := services.NewDNSmasqManager(o.cfg, o.daemonCfg, o.executor)

			if o.cfg.ManagedServices.DNS {
				dnsSvc := o.trackServiceStart("dns", "dns", true)
				dnsSvc.ServiceName = "dnsmasq"
				dnsSvc.ConfigFile = "/etc/dnsmasq.d/dns.conf"
				
				o.logger.Info("Configuring Local DNS...")
				if err := dnsmasq.SetupDNS(ctx); err != nil {
					o.trackServiceEnd(dnsSvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(dnsSvc, nil, fmt.Sprintf("DNS configured for %s.%s", o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain))
			} else {
				o.logger.Info("Skipping DNS (User Managed)")
				skipSvc := o.trackServiceStart("dns", "dns", false)
				o.trackServiceEnd(skipSvc, nil, "User managed")
			}

			if o.cfg.ManagedServices.DHCP && o.cfg.Nodes.BootMethod != "iso" {
				dhcpSvc := o.trackServiceStart("dhcp", "dhcp", true)
				dhcpSvc.ServiceName = "dnsmasq"
				dhcpSvc.ConfigFile = "/etc/dnsmasq.d/dhcp.conf"
				
				o.logger.Info("Configuring Local DHCP...")
				if err := dnsmasq.SetupDHCP(ctx); err != nil {
					o.trackServiceEnd(dhcpSvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(dhcpSvc, nil, "DHCP configured for cluster nodes")
			} else {
				o.logger.Info("Skipping DHCP (Not required for Agent ISO or User Managed)")
				skipSvc := o.trackServiceStart("dhcp", "dhcp", false)
				skipReason := "Not required for Agent ISO"
				if !o.cfg.ManagedServices.DHCP {
					skipReason = "User managed"
				}
				o.trackServiceEnd(skipSvc, nil, skipReason)
			}

			if o.cfg.ManagedServices.PXE && o.cfg.Nodes.BootMethod != "iso" {
				pxeSvc := o.trackServiceStart("pxe", "pxe", true)
				pxeSvc.ServiceName = "dnsmasq"
				pxeSvc.ConfigFile = "/etc/dnsmasq.d/tftp.conf"
				
				o.logger.Info("Configuring Local PXE Service...")
				if err := dnsmasq.SetupPXEService(ctx); err != nil {
					o.trackServiceEnd(pxeSvc, err, "")
					phaseErr = err
					return
				}
				o.logger.Info("Staging PXE Artifacts...")
				if err := dnsmasq.ConfigurePXEBoot(ctx, o.workspaceDir); err != nil {
					o.trackServiceEnd(pxeSvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(pxeSvc, nil, "PXE boot configured with TFTP")
			} else {
				o.logger.Info("Skipping PXE (Not required for Agent ISO or User Managed)")
				skipSvc := o.trackServiceStart("pxe", "pxe", false)
				skipReason := "Not required for Agent ISO"
				if !o.cfg.ManagedServices.PXE {
					skipReason = "User managed"
				}
				o.trackServiceEnd(skipSvc, nil, skipReason)
			}

			needsDHCP := o.cfg.ManagedServices.DHCP && o.cfg.Nodes.BootMethod != "iso"
			needsPXE := o.cfg.ManagedServices.PXE && o.cfg.Nodes.BootMethod != "iso"

			if o.cfg.ManagedServices.DNS || needsDHCP || needsPXE {
				restartSvc := o.trackServiceStart("dnsmasq-restart", "service-restart", true)
				restartSvc.ServiceName = "dnsmasq"
				
				o.logger.Info("Restarting DNSmasq service...")
				if err := dnsmasq.Restart(ctx); err != nil {
					o.trackServiceEnd(restartSvc, err, "")
					phaseErr = fmt.Errorf("failed to start dnsmasq: %w", err)
					return
				}
				o.trackServiceEnd(restartSvc, nil, "DNSmasq service restarted successfully")
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("services")
	}

	// --- PHASE 4: IGNITION GENERATION ---
	needsIgnition := !resume || !contains(o.state.CompletedPhases, "ignition")

	if !needsIgnition {
		if _, err := os.Stat(filepath.Join(o.workspaceDir, "install-dir")); os.IsNotExist(err) {
			o.logger.Warn("Missing installation artifacts in workspace. Forcing ignition generation phase recovery...")
			needsIgnition = true
		}
	}

	if needsIgnition {
		phaseExec := o.startPhase("ignition")
		o.logger.Phase("[Phase 4] Generating OpenShift Ignition Payload")
		
		var phaseErr error
		func() {
			ignSvc := o.trackServiceStart("ignition-generation", "ignition", true)
			if err := services.GenerateIgnition(ctx, o.cfg, o.executor, o.workspaceDir); err != nil {
				o.trackServiceEnd(ignSvc, err, "")
				phaseErr = err
				return
			}
			o.trackServiceEnd(ignSvc, nil, "Generated ignition configs for all nodes")

			if o.cfg.Nodes.BootMethod != "iso" {
				httpSvc := o.trackServiceStart("http-server", "http", true)
				httpSvc.ServiceName = "httpd"
				httpSvc.ConfigFile = "/etc/httpd/conf.d/shiftlaunch.conf"
				
				o.logger.Info("Setting up Local HTTP Server (Port 8080)...")
				
				if err := services.ConfigureHTTPD(ctx, o.executor, o.daemonCfg.Network.HTTPPort); err != nil {
					o.trackServiceEnd(httpSvc, err, "")
					phaseErr = err
					return
				}

				httpServer := services.NewHTTPServerManager(o.cfg, o.daemonCfg, o.executor, o.logger)
				if err := httpServer.Setup(ctx, o.workspaceDir); err != nil {
					o.trackServiceEnd(httpSvc, err, "")
					phaseErr = err
					return
				}
				
				o.logger.Info("Staging files to HTTP Server...")
				if err := httpServer.StageFiles(ctx, o.workspaceDir); err != nil {
					o.trackServiceEnd(httpSvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(httpSvc, nil, fmt.Sprintf("HTTP server configured on port %d", o.daemonCfg.Network.HTTPPort))
			} else {
				o.logger.Info("Skipping HTTP Server setup (Not required for Agent ISO)")
				skipSvc := o.trackServiceStart("http-server", "http", false)
				o.trackServiceEnd(skipSvc, nil, "Not required for Agent ISO")
				
				if o.cfg.ManagedServices.NFS {
					nfsSvc := o.trackServiceStart("nfs-server", "nfs", true)
					nfsSvc.ServiceName = "nfs-server"
					nfsSvc.ConfigFile = "/etc/exports"
					
					o.logger.Info("Setting up NFS Server for Agent ISO...")
					nfsMgr := services.NewNFSManager(o.cfg, o.executor, o.logger, o.workspaceDir)
					if err := nfsMgr.Setup(ctx); err != nil {
						o.trackServiceEnd(nfsSvc, err, "")
						phaseErr = fmt.Errorf("failed to setup NFS server: %w", err)
						return
					}
					exportPath := filepath.Join(o.workspaceDir, fmt.Sprintf("%s-iso", o.cfg.OpenShift.ClusterName))
					o.trackServiceEnd(nfsSvc, nil, fmt.Sprintf("NFS export configured: %s", exportPath))
				} else {
					o.logger.Info("Skipping NFS Server setup (User Managed)")
					skipSvc := o.trackServiceStart("nfs-server", "nfs", false)
					o.trackServiceEnd(skipSvc, nil, "User managed")
				}
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
		o.logger.Phase("[Phase 5] Initiating Cluster Boot")
		
		var phaseErr error
		func() {
			provider, err := compute.NewProviderWithState(o.cfg, o.logger, o.debug, o.stateManager)
			if err != nil {
				phaseErr = err
				return
			}
			defer func() {
				if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
					hmcProvider.Cleanup()
				}
			}()

			if resume {
				o.logger.Info("Re-discovering LPAR metadata for resume...")
				if err := provider.DiscoverMetadata(ctx); err != nil {
					phaseErr = fmt.Errorf("failed to re-discover LPAR metadata: %w", err)
					return
				}
			}

			for _, node := range o.cfg.GetAllNodes() {
				bootMarker := "booted_" + node.Hostname
				
				if resume && contains(o.state.CompletedPhases, bootMarker) {
					o.logger.Info("Skipping already booted node", "node", node.Hostname)
					continue
				}

				if err := provider.BootNode(ctx, node); err != nil {
					phaseErr = fmt.Errorf("HMC boot sequence failed for %s: %w", node.Hostname, err)
					return
				}
				
				o.saveState(bootMarker)
			}
		}()
		
		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			return phaseErr
		}
		o.saveState("boot")
	}

	// --- PHASE 6: WAIT FOR INSTALLATION ---
	if !resume || !contains(o.state.CompletedPhases, "wait") {
		phaseExec := o.startPhase("wait")
		o.logger.Phase("[Phase 6] Waiting for OpenShift Installation")
		
		var phaseErr error
		func() {
			if err := o.waitForBootstrapComplete(ctx); err != nil {
				phaseErr = err
				return
			}

			if err := o.waitForInstallComplete(ctx); err != nil {
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

	// --- PHASE 7: POST-INSTALL CLEANUP ---
	if o.cfg.Nodes.BootMethod == "iso" && (!resume || !contains(o.state.CompletedPhases, "iso_cleanup")) {
		phaseExec := o.startPhase("iso_cleanup")
		o.logger.Phase("[Phase 7] Cleaning up VIOS ISO Mappings")
		
		var phaseErr error
		func() {
			provider, err := compute.NewProviderWithState(o.cfg, o.logger, o.debug, o.stateManager)
			if err != nil {
				phaseErr = err
				return
			}
			defer func() {
				if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
					hmcProvider.Cleanup()
				}
			}()

			if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
				if err := hmcProvider.CleanupISOMappings(ctx); err != nil {
					o.logger.Warn("Failed to clean up ISO mappings, manual VIOS cleanup may be required", "error", err)
				}
			}

			if o.cfg.ManagedServices.NFS {
				o.logger.Info("Cleaning up local NFS configuration on controller...")
				nfsMgr := services.NewNFSManager(o.cfg, o.executor, o.logger, o.workspaceDir)
				if err := nfsMgr.Cleanup(ctx); err != nil {
					o.logger.Warn("Failed to clean up local NFS exports", "error", err)
				}
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		o.saveState("iso_cleanup") 
	}

	o.logger.Info("\nShiftLaunch Agent Execution Complete! OpenShift is ready.")
	return nil
}


// GetClusterStatus returns a formatted string of the current deployment state
func (o *Orchestrator) GetClusterStatus(ctx context.Context) string {
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

	// Add cluster nodes information
	if len(state.DiscoveredNodes) > 0 {
		status += "\n=== Cluster Nodes ===\n"
		for _, node := range state.DiscoveredNodes {
			status += fmt.Sprintf("%-15s %-10s %s\n", node.Hostname, node.Role, node.IP)
		}
	}

	if state.Status == "completed" {
		status += "\n=== Service Endpoints ===\n"
		baseDomain := o.cfg.OpenShift.BaseDomain
		clusterDomain := fmt.Sprintf("%s.%s", state.ClusterName, baseDomain)
		
		status += fmt.Sprintf("API Server:       https://api.%s:6443\n", clusterDomain)
		status += fmt.Sprintf("Web Console:      https://console-openshift-console.apps.%s\n", clusterDomain)
		status += fmt.Sprintf("OAuth Server:     https://oauth-openshift.apps.%s\n", clusterDomain)
		status += fmt.Sprintf("Prometheus:       https://prometheus-k8s-openshift-monitoring.apps.%s\n", clusterDomain)
		status += fmt.Sprintf("Grafana:          https://grafana-openshift-monitoring.apps.%s\n", clusterDomain)

		// Add /etc/hosts entry (single line format)
		status += "\n=== /etc/hosts Entry ===\n"
		status += fmt.Sprintf("%s api.%s console-openshift-console.apps.%s integrated-oauth-server-openshift-authentication.apps.%s oauth-openshift.apps.%s prometheus-k8s-openshift-monitoring.apps.%s grafana-openshift-monitoring.apps.%s\n",
			o.cfg.Network.LoadBalancerIP, clusterDomain, clusterDomain, clusterDomain, clusterDomain, clusterDomain, clusterDomain)
		
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
func (o *Orchestrator) DumpConfigs(ctx context.Context) error {
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

