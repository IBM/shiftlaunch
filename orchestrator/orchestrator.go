package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pterm/pterm"
	"github.com/IBM/shiftlaunch/config"
	compute "github.com/IBM/shiftlaunch/infra/compute"
	"github.com/IBM/shiftlaunch/infra/controller"
	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/services"
	"github.com/IBM/shiftlaunch/types"
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
			StateVersion:     2, // Current version with granular tracking
			ClusterName:      cfg.OpenShift.ClusterName,
			Status:           "in_progress",
			StartTime:        time.Now().Format(time.RFC3339),
			CompletedPhases:  []string{},
			CompletedEvents:  []string{},
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
	// --- CRITICAL  Sync completely instead of manually copying specific fields ---
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

	// --- CRITICAL  Sync in-memory state with the disk to preserve provider updates ---
	if currentState, loadErr := o.stateManager.LoadState(); loadErr == nil && currentState != nil {
		o.state = currentState
	}

	// Deduplicate: If this phase was executed in a previous failed run, overwrite it instead of bloating the array
	updated := false
	for i, existing := range o.state.PhaseHistory {
		if existing.Phase == phaseExec.Phase {
			// Inherit the attempt count and increment it
			phaseExec.Attempts = existing.Attempts + 1
			o.state.PhaseHistory[i] = *phaseExec
			updated = true
			break
		}
	}

	if !updated {
		phaseExec.Attempts = 1
		o.stateManager.AddPhaseExecution(o.state, *phaseExec)
	}

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
		o.logger.Error("Failed to start deployment", "error", err)
		return err
	}

	// Ensure lock is released and correct state markers are applied, even on panic/error
	defer func() {
		if err != nil {
			o.logger.Error("Deployment failed", "error", err)
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
	o.logger.Debug("Starting ShiftLaunch Local Agent Orchestration...", "cluster", o.cfg.OpenShift.ClusterName)

	// --- PHASE 0: VALIDATION (Only for fresh deployments) ---
	if !resume || len(o.state.CompletedPhases) == 0 {
		o.logger.StartPhase("[Phase 0/6] Pre-Deployment Validation")

		// Validate VIP is not in use
		if o.cfg.Services.LoadBalancer.IsManaged() && o.cfg.Services.LoadBalancer.GetVIP() != "" {
			o.logger.Info("Validating VIP availability...", "vip", o.cfg.Services.LoadBalancer.GetVIP())

			// Check if VIP is configured on interface
			iface := o.cfg.Network.ControllerInterface
			ctrlIP := o.cfg.Network.ControllerIP

			// ZERO-CONFIG SHIELD: If VIP is the Controller IP, it belongs on the interface natively!
			if iface != "" && o.cfg.Services.LoadBalancer.GetVIP() != ctrlIP {
				output, err := o.executor.Execute(ctx, fmt.Sprintf("ip addr show %s", iface))
				if err == nil && strings.Contains(output, o.cfg.Services.LoadBalancer.GetVIP()+"/") {
					// VIP is configured - check which cluster is using it
					conflictingCluster := o.findClusterUsingVIP(o.cfg.Services.LoadBalancer.GetVIP())
					if conflictingCluster != "" {
						o.logger.Error("VIP is already in use by another cluster",
							"vip", o.cfg.Services.LoadBalancer.GetVIP(),
							"cluster", conflictingCluster)
						return fmt.Errorf("VIP %s is already in use by cluster '%s'. Please choose a different VIP or delete the conflicting cluster first",
							o.cfg.Services.LoadBalancer.GetVIP(), conflictingCluster)
					}
					o.logger.Error("VIP is already configured on interface",
						"vip", o.cfg.Services.LoadBalancer.GetVIP(),
						"interface", iface)
					return fmt.Errorf("VIP %s is already configured on interface %s. Please remove the VIP alias manually or choose a different VIP",
						o.cfg.Services.LoadBalancer.GetVIP(), iface)
				}
			}

			// Check if VIP is defined in another cluster's config
			conflictingCluster := o.findClusterUsingVIP(o.cfg.Services.LoadBalancer.GetVIP())
			if conflictingCluster != "" {
				o.logger.Error("VIP is already configured for another cluster",
					"vip", o.cfg.Services.LoadBalancer.GetVIP(),
					"cluster", conflictingCluster)
				return fmt.Errorf("VIP %s is already configured for cluster '%s'. Please choose a different VIP",
					o.cfg.Services.LoadBalancer.GetVIP(), conflictingCluster)
			}
	
			o.logger.Info("VIP is available", "vip", o.cfg.Services.LoadBalancer.GetVIP())
		}

		o.logger.EndPhase(true, "[Phase 0/6] Pre-Deployment Validation Complete")
	}
	// --- RESTORE IN-MEMORY STATE FOR RESUMES ---
	if resume && len(o.state.DiscoveredNodes) > 0 {
		o.logger.Debug("Restoring discovered node metadata from state file...")
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
		o.logger.StartPhase("[Phase 1/6] Pre-Flight & HMC Discovery")

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
			o.logger.EndPhase(false, "[Phase 1/6] Pre-Flight & HMC Discovery Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 1/6] Pre-Flight & HMC Discovery Complete")
		o.saveState("discovery")
	}

	// --- PHASE 2: DOWNLOADS ---
	needsDownloads := !resume || !contains(o.state.CompletedPhases, "downloads")

	// ---  LBYL Safety Check for Missing Binaries ---
	if !needsDownloads {
		installerPath := filepath.Join(o.workspaceDir, "tools", "openshift-install")
		if _, err := os.Stat(installerPath); os.IsNotExist(err) {
			o.logger.Warn(" Missing required OpenShift binaries in workspace. Forcing download phase recovery...")
			needsDownloads = true
		}
	}

	if needsDownloads {
		phaseExec := o.startPhase("downloads")
		o.logger.StartPhase("[Phase 2/6] Downloading OpenShift Artifacts")

		downloader := services.NewDownloader(o.cfg, o.daemonCfg, o.executor, o.logger)
		phaseErr := downloader.DownloadAll(ctx, o.workspaceDir)

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			o.logger.EndPhase(false, "[Phase 2/6] Downloading OpenShift Artifacts Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 2/6] Downloading OpenShift Artifacts Complete")
		o.saveState("downloads")
	}

	// --- PHASE 3: MANAGED SERVICES ---
	if !resume || !contains(o.state.CompletedPhases, "services") {
		phaseExec := o.startPhase("services")
		o.logger.StartPhase("[Phase 3/6] Configuring Managed Infrastructure Services")

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
			if o.cfg.Services.LoadBalancer.IsManaged() {
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
				vip := o.cfg.Services.LoadBalancer.VIP
				iface := o.cfg.Network.ControllerInterface
				cidr := o.cfg.Network.MachineCIDR
				ctrlIP := o.cfg.Network.ControllerIP

				// Only add a secondary network alias if they requested a dedicated floating VIP
				if vip != ctrlIP {
					if err := netMgr.AddVIPAlias(ctx, iface, vip, cidr); err != nil {
						o.trackServiceEnd(haproxySvc, err, "")
						phaseErr = fmt.Errorf("FATAL: Failed to configure VIP alias '%s' on interface '%s': %w", vip, iface, err)
						return
					}
				} else {
					o.logger.Debug("VIP matches Controller IP. Skipping secondary alias creation.")
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

			// Track Squid Proxy configuration
			if o.cfg.Services.Proxy.IsManaged() {
				squidSvc := o.trackServiceStart("squid-proxy", "proxy", true)
				squidSvc.ServiceName = "squid"
				squidSvc.ConfigFile = "/etc/squid/squid.conf"

				o.logger.Info("Configuring Local Squid Proxy...")
				squidMgr := services.NewSquidManager(o.cfg, o.executor, o.logger, o.workspaceDir)

				if err := squidMgr.Setup(ctx); err != nil {
					o.trackServiceEnd(squidSvc, err, "")
					phaseErr = err
					return
				}
				o.trackServiceEnd(squidSvc, nil, "Squid proxy configured on port 3128")
			} else {
				o.logger.Info("Skipping Squid Proxy (User Managed)")
				skipSvc := o.trackServiceStart("squid-proxy", "proxy", false)
				o.trackServiceEnd(skipSvc, nil, "User managed")
			}

			// Track Local Registry configuration for disconnected deployments
			if o.cfg.Network.IsolationLevel == "air-gapped" && o.cfg.Services.Registry.IsManaged() {
				registrySvc := o.trackServiceStart("local-registry", "registry", true)
				registrySvc.ServiceName = "local-registry"
				registrySvc.ConfigFile = "/etc/systemd/system/local-registry.service"

				o.logger.Info("Configuring Local Container Registry for Disconnected Deployment...")
				o.logger.Info("This will mirror OpenShift images and may take 15-30 minutes...")

				registryMgr := services.NewRegistryManager(o.cfg, o.executor, o.logger, o.stateManager, o.state, o.workspaceDir)

				if err := registryMgr.Setup(ctx, o.workspaceDir); err != nil {
					o.trackServiceEnd(registrySvc, err, "")
					phaseErr = fmt.Errorf("failed to setup local registry: %w", err)
					return
				}

				registryURL := registryMgr.GetRegistryURL()
				o.trackServiceEnd(registrySvc, nil, fmt.Sprintf("Local registry configured at https://%s", registryURL))
			} else if o.cfg.Network.IsolationLevel == "air-gapped" {
				o.logger.Info("Skipping Local Registry (User Managed)")
				skipSvc := o.trackServiceStart("local-registry", "registry", false)
				o.trackServiceEnd(skipSvc, nil, "User managed")
			}

			dnsmasq := services.NewDNSmasqManager(o.cfg, o.daemonCfg, o.executor)

			if o.cfg.Services.DNS.IsManaged() {
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

			if o.cfg.Services.DHCP.IsManaged() && o.cfg.Nodes.BootMethod != "agent" {
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
				if !o.cfg.Services.DHCP.IsManaged() {
					skipReason = "User managed"
				}
				o.trackServiceEnd(skipSvc, nil, skipReason)
			}

			if o.cfg.Services.PXE.IsManaged() && o.cfg.Nodes.BootMethod != "agent" {
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
				if !o.cfg.Services.PXE.IsManaged() {
					skipReason = "User managed"
				}
				o.trackServiceEnd(skipSvc, nil, skipReason)
			}

			needsDHCP := o.cfg.Services.DHCP.IsManaged() && o.cfg.Nodes.BootMethod != "agent"
			needsPXE := o.cfg.Services.PXE.IsManaged() && o.cfg.Nodes.BootMethod != "agent"

			if o.cfg.Services.DNS.IsManaged() || needsDHCP || needsPXE {
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

			// ========================================================================
			// LOCAL DNS OVERRIDE (Tracked in State)
			// ========================================================================
			hostsSvc := o.trackServiceStart("local-hosts", "dns-override", true)
			hostsSvc.ConfigFile = "/etc/hosts"

			o.logger.Info("Updating local /etc/hosts for installer resolution...")
			netMgr := controller.NewNetworkManager(o.executor, o.debug, o.logger)

			if err := netMgr.AddHostsEntry(ctx, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain, o.cfg.Services.LoadBalancer.VIP); err != nil {
				o.trackServiceEnd(hostsSvc, err, "")
				o.logger.Warn("Failed to update /etc/hosts (installer wait phase may fail)", "error", err)
			} else {
				o.trackServiceEnd(hostsSvc, nil, "Added cluster API to /etc/hosts for local resolution")

				//  Force Squid to flush its cache and learn the new OpenShift API routes!
				if o.cfg.Services.Proxy.IsManaged() {
					_ = o.executor.SystemctlRestart(ctx, "squid")
					o.logger.Debug("Reloaded Squid proxy to register new API routing")
				}
			}
			// ========================================================================
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			o.logger.EndPhase(false, "[Phase 3/6] Configuring Managed Infrastructure Services Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 3/6] Configuring Managed Infrastructure Services Complete")
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
		o.logger.StartPhase("[Phase 4/6] Generating OpenShift Ignition Payload")

		var phaseErr error
		func() {
			ignSvc := o.trackServiceStart("ignition-generation", "ignition", true)
			if err := services.GenerateIgnition(ctx, o.cfg, o.executor, o.workspaceDir); err != nil {
				o.trackServiceEnd(ignSvc, err, "")
				phaseErr = err
				return
			}
			o.trackServiceEnd(ignSvc, nil, "Generated ignition configs for all nodes")

			if o.cfg.Nodes.BootMethod != "agent" {
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

				if o.cfg.Services.NFS.IsManaged() {
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
					//  Point the log message to the actual exported directory
					exportPath := filepath.Join(o.workspaceDir, "install-dir")
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
			o.logger.EndPhase(false, "[Phase 4/6] Generating OpenShift Ignition Payload Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 4/6] Generating OpenShift Ignition Payload Complete")
		o.saveState("ignition")
	}

	// --- PHASE 5: BOOT ---
	if !resume || !contains(o.state.CompletedPhases, "boot") {
		phaseExec := o.startPhase("boot")
		o.logger.StartPhase("[Phase 5/6] Initiating Cluster Boot")

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

			// Hand over the entire topology to the Provider to support bulk operations!
			if err := provider.BootNodes(ctx); err != nil {
				phaseErr = err
				return
			}
		}()

		o.endPhase(phaseExec, phaseErr)
		if phaseErr != nil {
			o.logger.EndPhase(false, "[Phase 5/6] Initiating Cluster Boot Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 5/6] Initiating Cluster Boot Complete")
		o.saveState("boot")
	}

	// --- PHASE 6: WAIT FOR INSTALLATION ---
	if !resume || !contains(o.state.CompletedPhases, "wait") {
		phaseExec := o.startPhase("wait")
		o.logger.StartPhase("[Phase 6/6] Waiting for OpenShift Installation")

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
			o.logger.EndPhase(false, "[Phase 6/6] Waiting for OpenShift Installation Failed")
			return phaseErr
		}

		o.logger.EndPhase(true, "[Phase 6/6] Waiting for OpenShift Installation Complete")
		o.saveState("wait")
	}

	// Phase 7 (POST-INSTALL CLEANUP) has been removed - cleanup now handled by 'shiftlaunch delete' command
	// This allows the cluster to remain operational with ISOs attached until explicit teardown

	o.logger.Debug("ShiftLaunch Agent Execution Complete! OpenShift is ready.")
	return nil
}

// GetClusterStatus returns a beautifully formatted, Docker-style summary of the deployment
func (o *Orchestrator) GetClusterStatus(ctx context.Context) string {
	state := o.state
	var sb strings.Builder

	// --- 1. CLUSTER SUMMARY ---
	sb.WriteString(pterm.Cyan("\n◉ CLUSTER SUMMARY\n"))

	// Dynamic status color
	statusColor := pterm.FgLightYellow
	if state.Status == "completed" {
		statusColor = pterm.FgLightGreen
	} else if state.Status == "failed" {
		statusColor = pterm.FgLightRed
	}

	summaryData := pterm.TableData{
		{"Name:", pterm.Bold.Sprint(state.ClusterName)},
		{"Status:", statusColor.Sprint(strings.ToUpper(state.Status))},
		{"Started:", state.StartTime},
	}
	if state.EndTime != "" {
		summaryData = append(summaryData, []string{"Ended:", state.EndTime})
	}

	summaryTable, _ := pterm.DefaultTable.WithData(summaryData).Srender()
	sb.WriteString(summaryTable)

	// ---INFRASTRUCTURE & SERVICES ---
	sb.WriteString(pterm.Cyan("\n◉ INFRASTRUCTURE & SERVICES\n"))

	// Format managed services list (Boot-Method Aware)
	var activeServices []string
	if o.cfg.Services.DNS.IsManaged() {
		activeServices = append(activeServices, "DNS")
	}
	if o.cfg.Services.DHCP.IsManaged() && o.cfg.Nodes.BootMethod != "agent" {
		activeServices = append(activeServices, "DHCP")
	}
	if o.cfg.Services.PXE.IsManaged() && o.cfg.Nodes.BootMethod != "agent" {
		activeServices = append(activeServices, "PXE")
	}
	if o.cfg.Services.LoadBalancer.IsManaged() {
		activeServices = append(activeServices, "HAProxy")
	}
	if o.cfg.Services.NFS.IsManaged() && o.cfg.Nodes.BootMethod == "agent" {
		activeServices = append(activeServices, "NFS")
	}

	managedStr := "None"
	if len(activeServices) > 0 {
		managedStr = strings.Join(activeServices, ", ")
	}

	// Format Proxy
	proxyStr := "None"
	if o.cfg.Services.Proxy.IsManaged() {
		proxyStr = fmt.Sprintf("Managed Squid Proxy (http://%s:3128)", o.cfg.Network.ControllerIP)
	} else if o.cfg.Services.Proxy.GetHTTP() != "" {
		proxyStr = fmt.Sprintf("External (%s)", o.cfg.Services.Proxy.GetHTTP())
	}

	// Format Registry
	registryStr := "Official Red Hat Upstream"
	if o.cfg.Services.Registry.IsManaged() {
		registryStr = fmt.Sprintf("Managed Local Registry (%s:5000)", o.cfg.Network.ControllerIP)
	} else if o.cfg.Network.IsolationLevel == "air-gapped" {
		registryStr = fmt.Sprintf("External Airgap Registry (%s)", o.cfg.Services.Registry.GetExternal())
	}

	infraData := pterm.TableData{
		{"Controller Managed:", managedStr},
		{"Proxy Gateway:", proxyStr},
		{"Container Registry:", registryStr},
	}
	infraTable, _ := pterm.DefaultTable.WithData(infraData).Srender()
	sb.WriteString(infraTable)

	// --- 2. CLUSTER NODES ---
	if len(state.DiscoveredNodes) > 0 {
		sb.WriteString(pterm.Cyan("\n◉ CLUSTER NODES\n"))
		nodeData := pterm.TableData{{"HOSTNAME", "ROLE", "IP ADDRESS", "MAC ADDRESS"}}

		for _, node := range state.DiscoveredNodes {
			nodeData = append(nodeData, []string{node.Hostname, node.Role, node.IP, node.MACAddress})
		}

		nodeTable, _ := pterm.DefaultTable.
			WithHasHeader().
			WithHeaderStyle(pterm.NewStyle(pterm.FgCyan, pterm.Bold)).
			WithData(nodeData).
			Srender()
		sb.WriteString(nodeTable)
	}

	// --- 3. ENDPOINTS & CREDENTIALS (Only if completed) ---
	if state.Status == "completed" {
		sb.WriteString(pterm.Cyan("\n◉ SERVICE ENDPOINTS\n"))
		baseDomain := o.cfg.OpenShift.BaseDomain
		clusterDomain := fmt.Sprintf("%s.%s", state.ClusterName, baseDomain)

		endpointData := pterm.TableData{
			{"API Server", fmt.Sprintf("https://api.%s:6443", clusterDomain)},
			{"Web Console", fmt.Sprintf("https://console-openshift-console.apps.%s", clusterDomain)},
			{"OAuth Server", fmt.Sprintf("https://oauth-openshift.apps.%s", clusterDomain)},
			{"Prometheus", fmt.Sprintf("https://prometheus-k8s-openshift-monitoring.apps.%s", clusterDomain)},
			{"Grafana", fmt.Sprintf("https://grafana-openshift-monitoring.apps.%s", clusterDomain)},
		}
		epTable, _ := pterm.DefaultTable.WithData(endpointData).Srender()
		sb.WriteString(epTable)

		sb.WriteString(pterm.Cyan("\n◉ ACCESS CREDENTIALS\n"))
		credData := pterm.TableData{}
		//kubeconfigPath := filepath.Join(o.workspaceDir, "install-dir", "auth", "kubeconfig")
		pwPath := filepath.Join(o.workspaceDir, "install-dir", "auth", "kubeadmin-password")

		//if _, err := os.Stat(kubeconfigPath); err == nil {
		//	credData = append(credData, []string{"KUBECONFIG:", kubeconfigPath})
		//}
		if pwData, err := os.ReadFile(pwPath); err == nil {
			credData = append(credData, []string{"Password:", string(pwData)})
		}

		if len(credData) > 0 {
			credTable, _ := pterm.DefaultTable.WithData(credData).Srender()
			sb.WriteString(credTable)
		}

		sb.WriteString(pterm.Cyan("\n◉ /etc/hosts ENTRY (Controller Override)\n"))
		hostsEntry := fmt.Sprintf("%s api.%s console-openshift-console.apps.%s oauth-openshift.apps.%s prometheus-k8s-openshift-monitoring.apps.%s grafana-openshift-monitoring.apps.%s",
			o.cfg.Services.LoadBalancer.VIP, clusterDomain, clusterDomain, clusterDomain, clusterDomain, clusterDomain)
		sb.WriteString(pterm.Gray(hostsEntry) + "\n\n")
	}

	return sb.String()
}

// DumpConfigs outputs required configuration records for Enterprise Admins
func (o *Orchestrator) DumpConfigs(ctx context.Context) error {
	o.logger.Info("Dumping configuration requirements for unmanaged services...")

	if !o.cfg.Services.DNS.IsManaged() {
		fmt.Printf("\n--- REQUIRED DNS RECORDS ---\n")
		fmt.Printf("%s\tapi.%s.%s\n", o.cfg.Services.LoadBalancer.VIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
		fmt.Printf("%s\tapi-int.%s.%s\n", o.cfg.Services.LoadBalancer.VIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
		fmt.Printf("%s\t*.apps.%s.%s\n", o.cfg.Services.LoadBalancer.VIP, o.cfg.OpenShift.ClusterName, o.cfg.OpenShift.BaseDomain)
	}

	if !o.cfg.Services.LoadBalancer.IsManaged() {
		fmt.Printf("\n--- REQUIRED LOAD BALANCER POOLS (Target: %s) ---\n", o.cfg.Services.LoadBalancer.GetVIP())
		fmt.Println("Port 6443 (API)   -> Masters & Bootstrap")
		fmt.Println("Port 22623 (MCS)  -> Masters & Bootstrap")
		fmt.Println("Port 80/443 (App) -> Workers (or Masters if SNO/Compact)")
	}

	if !o.cfg.Services.DHCP.IsManaged() {
		fmt.Printf("\n--- REQUIRED DHCP OPTIONS ---\n")
		fmt.Printf("Option 66 (Next-Server): %s\n", o.cfg.Network.ControllerIP)
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
			if tempCfg.Services.LoadBalancer != nil && tempCfg.Services.LoadBalancer.GetVIP() == vip {
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
