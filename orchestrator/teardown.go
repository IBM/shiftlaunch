package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	compute "github.ibm.com/sudeeshjohn/shiftlaunch/infra/compute"
	"github.ibm.com/sudeeshjohn/shiftlaunch/infra/controller"
	"github.ibm.com/sudeeshjohn/shiftlaunch/services"
)

// Teardown safely powers off LPARs, removes local services, and marks workspace as deleted
func (o *Orchestrator) Teardown(ctx context.Context) error {
	// Check if already deleted
	if o.stateManager.IsDeleted() {
		o.logger.Info("Cluster is already marked as deleted. Skipping teardown.", "cluster", o.cfg.OpenShift.ClusterName)
		return nil
	}

	// Acquire lock to prevent concurrent operations
	if err := o.stateManager.AcquireLock(); err != nil {
		o.logger.Error("Failed to start teardown", "error", err)
		return err
	}
	// Ensure lock is always released, even on error
	defer func() {
		if err := o.stateManager.ReleaseLock(); err != nil {
			o.logger.Warn("Failed to release lock", "error", err)
		}
	}()

	// Downgraded to Debug to keep the terminal clean
	o.logger.Debug("Initiating Soft Teardown", "cluster", o.cfg.OpenShift.ClusterName)

	// ---Centralized HMC Connection Phase ---
	o.logger.StartPhase("Connecting to HMC...")
	provider, err := compute.NewProviderWithState(o.cfg, o.logger, o.debug, o.stateManager)
	if err != nil {
		o.logger.EndPhase(false, "Failed to connect to HMC")
		return fmt.Errorf("failed to initialize compute provider: %w", err)
	}
	o.logger.EndPhase(true, "Connected to HMC")

	defer func() {
		if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
			hmcProvider.Cleanup()
		}
	}()

	// Phase 1: Power off LPARs (MUST happen before unmapping ISO)
	phaseExec := o.startPhase("teardown_poweroff")
	o.logger.StartPhase("Powering off cluster LPARs...")
	var phaseErr error
	func() {
		// We no longer initialize the provider here!
		shieldedCtx := context.WithoutCancel(ctx)
		if err := provider.PowerOffNodes(shieldedCtx); err != nil {
			o.logger.Warn("Failed to power off some LPARs", "error", err)
			phaseErr = err
		}
	}()
	o.logger.EndPhase(phaseErr == nil, "Cluster LPARs powered off")
	o.endPhase(phaseExec, phaseErr)

	// Phase 2: Clean up Agent mappings (AFTER LPARs are powered off)
	if o.cfg.Nodes.BootMethod == "agent" {
		phaseExec = o.startPhase("teardown_agent_cleanup")
		o.logger.StartPhase("Cleaning up VIOS ISO Mappings...")
		phaseErr = nil
		func() {
			// We no longer initialize the provider here either!
			if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
				if err := hmcProvider.CleanupISOMappings(ctx); err != nil {
					o.logger.Warn("Failed to clean up some Agent ISO mappings", "error", err)
					phaseErr = err
				}
			}
		}()
		o.logger.EndPhase(phaseErr == nil, "VIOS Agent ISO Mappings cleaned up")
		o.endPhase(phaseExec, phaseErr)
	}

	// Phase 3: Clean up local services
	phaseExec = o.startPhase("teardown_services")
	o.logger.StartPhase("Removing local network and service configurations...")
	phaseErr = nil
	func() {
		// Shield local network cleanup from cancellation to prevent orphaned VIPs and NFS exports!
		shieldedCtx := context.WithoutCancel(ctx)

		if o.cfg.ManagedServices.DNS || o.cfg.ManagedServices.DHCP || o.cfg.ManagedServices.PXE {
			dnsmasq := services.NewDNSmasqManager(o.cfg, o.daemonCfg, o.executor)
			dnsmasq.Cleanup(shieldedCtx)
		}

		if o.cfg.ManagedServices.LoadBalancer {
			o.executor.Execute(shieldedCtx, fmt.Sprintf("sudo rm -f /etc/haproxy/conf.d/10-%s.cfg", o.cfg.OpenShift.ClusterName))
			o.executor.SystemctlRestart(shieldedCtx, "haproxy")

			netMgr := controller.NewNetworkManager(o.executor, o.debug, o.logger)
			vip := o.cfg.Network.LoadBalancerIP
			iface := o.cfg.Controller.NetworkInterface
			cidr := o.cfg.Network.MachineCIDR
			ctrlIP := o.cfg.Controller.IP

			if err := netMgr.RemoveVIPAlias(shieldedCtx, iface, vip, cidr, ctrlIP); err != nil {
				o.logger.Warn("Failed to cleanly remove VIP alias via nmcli", "error", err)
				
				prefix := controller.ExtractCIDRPrefix(cidr)
				o.executor.Execute(shieldedCtx, fmt.Sprintf("sudo ip addr del %s/%s dev %s", vip, prefix, iface))
			}
		}

		if o.cfg.ManagedServices.Proxy {
			squidMgr := services.NewSquidManager(o.cfg, o.executor, o.logger, o.workspaceDir)
			if err := squidMgr.Cleanup(shieldedCtx); err != nil {
				o.logger.Warn("Failed to clean up Squid proxy", "error", err)
			}
		}

		//  Clean up the Podman registry, firewall rules, and certs!
		if o.cfg.DisconnectedConfig.Enabled && o.cfg.ManagedServices.Registry {
			registryMgr := services.NewRegistryManager(o.cfg, o.executor, o.logger, o.stateManager, o.state, o.workspaceDir)
			if err := registryMgr.Cleanup(shieldedCtx); err != nil {
				o.logger.Warn("Failed to clean up Local Registry", "error", err)
			}
		}
		
		if !o.stateManager.IsServiceRemoved(o.state, "local-hosts") {
			netMgr := controller.NewNetworkManager(o.executor, o.debug, o.logger)
			if err := netMgr.RemoveHostsEntry(shieldedCtx, o.cfg.OpenShift.ClusterName); err != nil {
				o.logger.Warn("Failed to clean up /etc/hosts entry", "error", err)
			} else {
				_ = o.stateManager.RecordServiceRemoved(o.state, "local-hosts")
			}
		}
		
		if o.cfg.Nodes.BootMethod != "agent" {
			httpServer := services.NewHTTPServerManager(o.cfg, o.daemonCfg, o.executor, o.logger)
			httpServer.Cleanup(shieldedCtx)
		}
		if o.cfg.Nodes.BootMethod == "agent" && o.cfg.ManagedServices.NFS {
			nfsMgr := services.NewNFSManager(o.cfg, o.executor, o.logger, o.workspaceDir)
			nfsMgr.Cleanup(shieldedCtx)
		}
	}()
	o.logger.EndPhase(phaseErr == nil, "Local network and services removed")
	o.endPhase(phaseExec, phaseErr)

	// Phase 4: Mark workspace as deleted
	phaseExec = o.startPhase("teardown_finalize")
	o.logger.StartPhase("Archiving cluster workspace...")
	phaseErr = nil
	func() {
		if err := o.stateManager.MarkDeleted(); err != nil {
			o.logger.Warn("Failed to create .deleted marker", "error", err)
			phaseErr = err
		}
		
		managedMarkerPath := filepath.Join(o.workspaceDir, ".managed")
		if err := os.Remove(managedMarkerPath); err != nil && !os.IsNotExist(err) {
			o.logger.Warn("Failed to remove .managed marker", "error", err)
		}
		
		failedMarkerPath := filepath.Join(o.workspaceDir, ".failed")
		if err := os.Remove(failedMarkerPath); err != nil && !os.IsNotExist(err) {
			o.logger.Warn("Failed to remove .failed marker", "error", err)
		}
	}()
	o.logger.EndPhase(phaseErr == nil, fmt.Sprintf("Workspace retained at %s", o.workspaceDir))
	o.endPhase(phaseExec, phaseErr)

	// Update State
	o.state.Status = "deleted"
	o.state.CurrentPhase = "deleted"
	o.state.EndTime = time.Now().Format(time.RFC3339)
	_ = o.stateManager.SaveState(o.state)

	return nil
}

// IsDeleted checks if the cluster has already been torn down
func (o *Orchestrator) IsDeleted() bool {
	return o.stateManager.IsDeleted()
}