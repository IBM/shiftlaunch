package compute

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	hmc "github.com/IBM/infra-go-sdk/phmc"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
)

// apiTrafficMutex protects the HMC session token from data races during parallel operations
// Uses RWMutex to allow concurrent reads but exclusive writes during re-authentication
var apiTrafficMutex sync.RWMutex

// HMCProvider implements the compute provider interface for IBM Hardware Management Console (HMC)
type HMCProvider struct {
	cfg          *types.AgentConfig
	hmcClient    *hmc.RestClient
	logger       *logger.Logger
	debug        bool
	isoMappings  []types.ISOMapping
	stateManager *types.StateManager
	// Track mount points per VIOS to enable sharing across nodes
	viosMountPoints map[string]string // key: viosUUID, value: mountPoint
	viosMounted     map[string]bool   // key: viosUUID, value: true if already mounted
	// Store selected VIOS per system to support multi-system clusters
	systemVIOSUUIDs map[string]string // key: SystemName
	systemVIOSNames map[string]string // key: SystemName
}

// GetHMCClient returns the underlying HMC REST client for external use (e.g., validation)
func (h *HMCProvider) GetHMCClient() *hmc.RestClient {
	return h.hmcClient
}

// DiscoverMetadata loops through your nodes and queries the HMC for network adapter details
func (h *HMCProvider) DiscoverMetadata(ctx context.Context) error {
	h.logger.Info("Discovering LPAR metadata from HMC in parallel (Bounded & Context-Aware)...")

	nodes := h.cfg.GetAllNodes()

	// 1. PRE-FETCH SYSTEM DATA
	type sysCache struct {
		sysUUID string
		lpars   []hmc.LogicalPartitionQuick
	}
	systemData := make(map[string]sysCache)

	for _, node := range nodes {
		if _, exists := systemData[node.SystemName]; !exists {
			apiTrafficMutex.RLock()
			sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName)
			apiTrafficMutex.RUnlock()
			if err != nil {
				return fmt.Errorf("failed to find system %s: %w", node.SystemName, err)
			}
	
			apiTrafficMutex.RLock()
			lpars, err := h.hmcClient.GetLogicalPartitionsQuickAll(ctx, sysUUID)
			apiTrafficMutex.RUnlock()
			if err != nil {
				return err
			}
			systemData[node.SystemName] = sysCache{sysUUID: sysUUID, lpars: lpars}
		}
	}

	// 2. PARALLELIZE WITH SEMAPHORE AND CONTEXT AWARENESS
	var wg sync.WaitGroup
	errCh := make(chan error, len(nodes))
	var memMu sync.Mutex
	var discoveredData []types.DiscoveredNode

	concurrencyLimit := 3
	semaphore := make(chan struct{}, concurrencyLimit)

	for _, n := range nodes {
		wg.Add(1)
		go func(node *types.NodeConfig) {
			defer wg.Done()

			// Respect Context Cancellation before acquiring semaphore
			select {
			case <-ctx.Done():
				errCh <- fmt.Errorf("discovery aborted for %s: %w", node.Hostname, ctx.Err())
				return
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			}

			sysUUID := systemData[node.SystemName].sysUUID
			lpars := systemData[node.SystemName].lpars

			for _, l := range lpars {
				if l.PartitionName == node.ExistingLPARName {
					node.UUID = l.UUID
					break
				}
			}
			if node.UUID == "" {
				errCh <- fmt.Errorf("LPAR '%s' not found on system %s", node.ExistingLPARName, node.SystemName)
				return
			}

			// Implement Lock Upgrade Pattern Helper
			apiCallWithRetry := func(call func(context.Context) error) error {
				for i := 0; i < 3; i++ {
					if ctx.Err() != nil {
						return ctx.Err() // Abort if cancelled mid-flight
					}

					apiTrafficMutex.RLock()
					err := call(ctx)
					apiTrafficMutex.RUnlock()

					if err != nil && strings.Contains(err.Error(), "406") {
						// 🛡️ SHIELDED LOCK UPGRADE: If user hits Ctrl+C right now,
						// the login MUST complete to prevent session corruption!
						shieldedCtx := context.WithoutCancel(ctx)

						apiTrafficMutex.Lock()
						testErr := call(shieldedCtx)
						if testErr != nil && strings.Contains(testErr.Error(), "406") {
							_ = h.hmcClient.Logoff(shieldedCtx)
							_ = h.hmcClient.Login(shieldedCtx, h.cfg.HMC.Username, h.cfg.HMC.Password)
						}
						apiTrafficMutex.Unlock()
						continue
					}
					return err
				}
				return fmt.Errorf("API failed after max re-auth attempts")
			}

			var profiles []hmc.LogicalPartitionProfile
			err := apiCallWithRetry(func(c context.Context) error {
				var e error
				profiles, e = h.hmcClient.GetLogicalPartitionProfiles(c, node.UUID)
				return e
			})
			if err != nil || len(profiles) == 0 {
				errCh <- fmt.Errorf("no profile found for LPAR %s", node.ExistingLPARName)
				return
			}
			node.ProfileUUID = profiles[0].UUID

			var adapters []hmc.ClientNetworkAdapter
			err = apiCallWithRetry(func(c context.Context) error {
				var e error
				adapters, e = h.hmcClient.GetClientNetworkAdapters(c, sysUUID, node.UUID)
				return e
			})
			if err != nil || len(adapters) == 0 {
				errCh <- fmt.Errorf("no network adapter found on LPAR %s", node.ExistingLPARName)
				return
			}
			node.MACAddress = hmc.FormatMACAddress(adapters[0].MACAddress)
			node.LocationCode = adapters[0].LocationCode

			var volumes []hmc.StorageMap
			_ = apiCallWithRetry(func(c context.Context) error {
				var e error
				volumes, e = h.hmcClient.GetAttachedVolumes(c, sysUUID, node.UUID)
				return e
			})
			if len(volumes) == 0 {
				h.logger.Warn("No vSCSI volumes found on LPAR. If using NPIV/SAN or dedicated HBAs, this is expected.", "lpar", node.ExistingLPARName)
			}

			h.logger.Info("Discovered", "lpar", node.ExistingLPARName, "mac", node.MACAddress, "uuid", node.UUID)

			memMu.Lock()
			discoveredData = append(discoveredData, types.DiscoveredNode{
				Hostname:     node.Hostname,
				Role:         node.Role,
				IP:           node.IP,
				MACAddress:   node.MACAddress,
				UUID:         node.UUID,
				ProfileUUID:  node.ProfileUUID,
				LocationCode: node.LocationCode,
				SystemName:   node.SystemName,
				LPARName:     node.ExistingLPARName,
				DiscoveredAt: time.Now().Format(time.RFC3339),
			})
			memMu.Unlock()

		}(n)
	}

	wg.Wait()
	close(errCh)

	// 🛡️ SAVE ON ABORT: We process the state save BEFORE evaluating errors.
	// This guarantees that any nodes successfully discovered before the cancellation
	// are safely written to state.json and won't be lost!
	if h.stateManager != nil && len(discoveredData) > 0 {
		state, err := h.stateManager.LoadState()

		if err != nil || state == nil {
			state = &types.DeploymentState{
				StateVersion:     2,
				ClusterName:      h.cfg.OpenShift.ClusterName,
				Status:           "in_progress",
				StartTime:        time.Now().Format(time.RFC3339),
				CompletedPhases:  []string{},
				CompletedEvents:  []string{},
				DownloadProgress: make(map[string]types.DownloadProgress),
				NodeBootStatus:   make(map[string]types.NodeBootStatus),
			}
		}

		for _, dNode := range discoveredData {
			nodeExists := false
			for i, existing := range state.DiscoveredNodes {
				if existing.Hostname == dNode.Hostname {
					state.DiscoveredNodes[i] = dNode
					nodeExists = true
					break
				}
			}
			if !nodeExists {
				state.DiscoveredNodes = append(state.DiscoveredNodes, dNode)
			}
		}

		if err := h.stateManager.SaveState(state); err != nil {
			h.logger.Warn("Failed to save discovered nodes to state", "error", err)
		} else {
			h.logger.Info("Saved discovered nodes to state file", "count", len(state.DiscoveredNodes))
		}
	}

	// 3. FINALLY, RETURN THE ERRORS
	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("metadata discovery aborted or failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

// BootNode routes to the appropriate boot method (netboot or Agent ISO)
func (h *HMCProvider) BootNode(ctx context.Context, node *types.NodeConfig) error {
	// Route based on boot method
	if h.cfg.Nodes.BootMethod == "agent" {
		return h.bootNodeWithISO(ctx, node)
	}

	// Default to netboot
	return h.networkBootLpar(ctx, node)
}

// BootNodes boots all nodes using the configured boot method
func (h *HMCProvider) BootNodes(ctx context.Context) error {
	if h.cfg.Nodes.BootMethod == "agent" {
		return h.bootNodesWithISOBulk(ctx)
	}

	// Default to netboot - boot all nodes in parallel
	h.logger.Info("Initiating parallel network boot sequence...")

	state, _ := h.stateManager.LoadState()
	allNodes := h.cfg.GetAllNodes()

	var wg sync.WaitGroup
	var stateMu sync.Mutex
	errCh := make(chan error, len(allNodes))

	for _, n := range allNodes {
		node := n // Prevent loop variable capture bug

		bootMarker := "booted_" + node.Hostname
		if state != nil && containsPhase(state.CompletedPhases, bootMarker) {
			h.logger.Info("Skipping already booted node", "node", node.Hostname)
			continue
		}

		wg.Add(1)
		go func(targetNode *types.NodeConfig) {
			defer wg.Done()

			if err := h.networkBootLpar(ctx, targetNode); err != nil {
				errCh <- fmt.Errorf("HMC boot sequence failed for %s: %w", targetNode.Hostname, err)
				return
			}

			if state != nil {
				stateMu.Lock()
				state.CompletedPhases = append(state.CompletedPhases, bootMarker)
				_ = h.stateManager.SaveState(state)
				stateMu.Unlock()
			}
		}(node)
	}

	wg.Wait()
	close(errCh)

	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("parallel boot encountered errors: %s", strings.Join(errs, "; "))
	}

	return nil
}

// networkBootLpar executes the lpar_netboot command via REST API for a single node
func (h *HMCProvider) networkBootLpar(ctx context.Context, node *types.NodeConfig) error {
	h.logger.Info("Processing LPAR for netboot", "lpar", node.ExistingLPARName)

	var lparDetailed *hmc.LogicalPartitionDetailed
	var err error
	maxRetries := 3

	// Retry loop for 406/Intermittent HMC errors with re-authentication
	for i := 0; i < maxRetries; i++ {
		lparDetailed, err = h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID)
		if err == nil {
			break
		}

		// Check if error is 406 (session issue) and re-authenticate
		if strings.Contains(err.Error(), "406") && i < maxRetries-1 {
			h.logger.Warn(fmt.Sprintf("HMC returned 406 error (attempt %d/%d). Attempting safe re-auth...", i+1, maxRetries), "node", node.Hostname)

			// 🛡️ Exclusive Lock: Pause ALL other goroutines from making API calls
			apiTrafficMutex.Lock()

			// Do a quick test to see if another thread already fixed the token while we were waiting in line!
			_, testErr := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID)
			if testErr != nil && strings.Contains(testErr.Error(), "406") {
				// Logout from old session first
				if logoutErr := h.hmcClient.Logoff(ctx); logoutErr != nil {
					h.logger.Debug("Logout failed (session may already be invalid)", "error", logoutErr)
				}

				// Wait a moment for HMC to clean up
				time.Sleep(2 * time.Second)

				// Re-authenticate with fresh session
				if loginErr := h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password); loginErr != nil {
					h.logger.Warn("Re-authentication failed", "error", loginErr)
				} else {
					h.logger.Info("Successfully re-authenticated with HMC")
				}
			}

			apiTrafficMutex.Unlock()
			time.Sleep(5 * time.Second)
		} else {
			h.logger.Warn(fmt.Sprintf("Failed to get LPAR details (attempt %d/%d). Retrying in 5s...", i+1, maxRetries), "error", err)
			time.Sleep(5 * time.Second)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to get detailed LPAR information for %s after %d attempts: %w", node.ExistingLPARName, maxRetries, err)
	}

	// Power off LPAR if it's in any active state
	if lparDetailed.PartitionState == "running" || lparDetailed.PartitionState == "open firmware" {
		h.logger.Info("LPAR is active. Powering off before network boot...", "state", lparDetailed.PartitionState)

		h.logger.Info("Closing virtual terminal...")
		_ = h.hmcClient.CloseVirtualTerminalViaSSH(
			h.cfg.HMC.IP,
			h.cfg.HMC.Username,
			h.cfg.HMC.Password,
			node.SystemName,
			node.ExistingLPARName,
		)

		_, err = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false)
		if err != nil {
			return fmt.Errorf("failed to power off LPAR: %w", err)
		}
		h.logger.Info("LPAR powered off successfully")
		time.Sleep(5 * time.Second)
	}

	profileHref := lparDetailed.AssociatedPartitionProfile.Href
	if profileHref == "" {
		return fmt.Errorf("no associated partition profile found for LPAR %s", node.ExistingLPARName)
	}
	profileUUID := profileHref[len(profileHref)-36:]

	// =========================================================================
	// STEP 1: Power cycle LPAR to make adapters visible to firmware
	// =========================================================================
	h.logger.Info("Power cycling LPAR to register adapters with firmware...")
	_, err = h.hmcClient.PowerOnPartition(ctx, node.UUID, &hmc.PowerOnOptions{
		ProfileUUID: profileUUID,
		BootMode:    "of", // Boot to Open Firmware
	})
	if err != nil {
		return fmt.Errorf("failed to power on LPAR for adapter registration: %w", err)
	}

	h.logger.Info("Waiting 20 seconds for LPAR to reach Open Firmware and register adapters...")
	time.Sleep(20 * time.Second)

	h.logger.Info("Powering off LPAR for profile query...")
	_, err = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false)
	if err != nil {
		return fmt.Errorf("failed to power off LPAR: %w", err)
	}

	h.logger.Info("Waiting 10 seconds for LPAR to fully power off...")
	time.Sleep(10 * time.Second)

	_ = h.hmcClient.CloseVirtualTerminalViaSSH(
		h.cfg.HMC.IP,
		h.cfg.HMC.Username,
		h.cfg.HMC.Password,
		node.SystemName,
		node.ExistingLPARName,
	)

	// =========================================================================
	// STEP 2: Get authoritative location code from profile
	// =========================================================================
	h.logger.Info("Retrieving network boot device information...")

	var bootDevices []hmc.NetworkBootDevice
	for i := 0; i < maxRetries; i++ {
		bootDevices, err = h.hmcClient.GetNetworkBootDevicesForLpar(ctx, node.UUID, profileUUID)
		if err == nil && len(bootDevices) > 0 {
			break
		}
		h.logger.Warn(fmt.Sprintf("Failed to get boot devices (attempt %d/%d). Retrying in 5s...", i+1, maxRetries), "error", err)
		time.Sleep(5 * time.Second)
	}

	if err != nil || len(bootDevices) == 0 {
		return fmt.Errorf("failed to get network boot devices from profile for %s (ensure adapter exists): %v", node.ExistingLPARName, err)
	}

	authoritativeLocationCode := bootDevices[0].LocationCode
	h.logger.Info("Authoritative location code found", "location", authoritativeLocationCode)

	// =========================================================================
	// STEP 3: Network boot using authoritative location code
	// =========================================================================
	h.logger.Info("Waiting 5 seconds for LPAR to initialize before initiating netboot...")
	time.Sleep(5 * time.Second)

	h.logger.Info("Ensuring virtual terminal is closed before netboot...")
	_ = h.hmcClient.CloseVirtualTerminalViaSSH(
		h.cfg.HMC.IP,
		h.cfg.HMC.Username,
		h.cfg.HMC.Password,
		node.SystemName,
		node.ExistingLPARName,
	)
	time.Sleep(3 * time.Second) // Give the HMC SSH daemon a moment to drop the connection

	h.logger.Info("Initiating network boot with location code...", "location", authoritativeLocationCode)

	options := &hmc.PowerOnOptions{
		ProfileUUID:  profileUUID,
		BootMode:     "netboot",
		LocationCode: authoritativeLocationCode,
		ClientIP:     "0.0.0.0",
		ServerIP:     "0.0.0.0",
		Gateway:      "0.0.0.0",
		Netmask:      "0.0.0.0",
	}

	status, err := h.hmcClient.PowerOnPartition(ctx, node.UUID, options)
	if err != nil {
		return fmt.Errorf("failed to execute network boot: %w", err)
	}

	h.logger.Info("Network boot initiated successfully", "lpar", node.ExistingLPARName, "status", status)

	h.logger.Info("Saving profile to persist configuration...")
	// Shield from cancellation to prevent profile corruption
	_ = h.hmcClient.SaveCurrentLparConfig(context.WithoutCancel(ctx), node.UUID, "default_profile", true)

	return nil
}

// PowerOffNodes powers off all nodes managed by the HMC provider
func (h *HMCProvider) PowerOffNodes(ctx context.Context) error {
	h.logger.Info("Fetching LPAR UUIDs for teardown...")

	nodes := h.cfg.GetAllNodes()
	h.logger.Info("Found nodes to power off", "count", len(nodes))

	// =========================================================================
	// FAST PATH: Try to populate UUIDs from the local state file first
	// =========================================================================
	if h.stateManager != nil {
		if state, err := h.stateManager.LoadState(); err == nil && state != nil {
			h.logger.Debug("Checking state file for known LPAR UUIDs...")
			for _, node := range nodes {
				for _, discovered := range state.DiscoveredNodes {
					if node.Hostname == discovered.Hostname && discovered.UUID != "" {
						node.UUID = discovered.UUID
						h.logger.Debug("Found UUID in state file", "node", node.Hostname)
						break
					}
				}
			}
		}
	}

	// =========================================================================
	// SLOW PATH / FALLBACK: Lightweight HMC query for UUIDs not found in state
	// =========================================================================
	for _, node := range nodes {
		if node.UUID != "" {
			continue // Skip! We already got it from the state file.
		}

		h.logger.Info("UUID not found in state file, querying HMC fallback...", "lpar", node.ExistingLPARName)

		// 1. Get System UUID quietly
		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName)
		if err != nil {
			h.logger.Debug("Could not resolve system UUID during teardown", "system", node.SystemName)
			continue
		}

		// 2. Get LPARs quietly
		lpars, err := h.hmcClient.GetLogicalPartitionsQuickAll(ctx, sysUUID)
		if err != nil {
			continue
		}

		// 3. Match LPAR name to UUID
		for _, l := range lpars {
			if l.PartitionName == node.ExistingLPARName {
				node.UUID = l.UUID
				break
			}
		}
	}

	h.logger.Info("Sending shutdown signals to all managed LPARs...")
	powerOffCount := 0
	skippedCount := 0

	// --- ADD THIS LINE TO OVERWRITE THE SPINNER TEXT ---
	h.logger.Info("Processing power-off transitions on HMC...")

	for _, node := range nodes {
		if node.UUID == "" {
			h.logger.Warn("Skipping power off for LPAR (UUID not found)", "lpar", node.ExistingLPARName)
			skippedCount++
			continue
		}

		h.logger.Info("Attempting to power off LPAR", "lpar", node.ExistingLPARName, "uuid", node.UUID)

		// Send the immediate power off signal.
		_, err := h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false)
		if err != nil {
			// Extract just the first line of the error for cleaner logging
			errMsg := strings.Split(err.Error(), "\n")[0]

			// If the HMC complains the LPAR is already off, feed it cleanly to the spinner!
			if strings.Contains(errMsg, "HSCL1558") || strings.Contains(strings.ToLower(errMsg), "unavailable in the current partition state") {
				h.logger.Info("LPAR already powered off", "lpar", node.ExistingLPARName)
			} else {
				// Only print an actual terminal WARNING if it's a real unexpected failure
				h.logger.Warn("LPAR power off returned an unexpected error", "lpar", node.ExistingLPARName, "details", errMsg)
			}
		} else {
			h.logger.Info("Power off signal accepted", "lpar", node.ExistingLPARName)
			powerOffCount++
		}
	}

	h.logger.Info("Power off signals sent", "powered_off", powerOffCount, "skipped", skippedCount, "total", len(nodes))

	if powerOffCount > 0 {
		h.logger.Info("Waiting for LPARs to fully transition to powered-off state (this may take a minute)...")

		maxRetries := 36 // Up to 3 minutes (36 * 5 seconds)
		for i := 0; i < maxRetries; i++ {
			allPoweredOff := true

			for _, node := range nodes {
				if node.UUID == "" {
					continue
				}

				// Natively query the lightweight JSON endpoint for the exact LPAR state
				lpar, err := h.hmcClient.GetLogicalPartitionQuick(node.UUID)
				if err == nil && lpar != nil {
					state := strings.ToLower(lpar.PartitionState)
					if state != "not activated" {
						allPoweredOff = false
						break // At least one LPAR is still powering down, no need to check the rest yet
					}
				}
			}

			if allPoweredOff {
				h.logger.Info("All LPARs successfully powered off and hardware locks released!")
				break
			}

			if i == maxRetries-1 {
				h.logger.Warn("Timeout waiting for some LPARs to power off. ISO cleanup may fail.")
			} else {
				time.Sleep(5 * time.Second)
			}
		}
	}

	return nil
}

// Cleanup logs out from HMC and cleans up resources
func (h *HMCProvider) Cleanup() error {
	h.logger.Debug("Logging out from HMC session...")
	if err := h.hmcClient.Logoff(context.Background()); err != nil {
		h.logger.Debug("HMC logout failed (session may already be invalid)", "error", err)
		return err
	}
	h.logger.Debug("Successfully logged out from HMC")
	return nil
}

// bootNodeWithISO boots an LPAR using Agent ISO via NFS mount
// Creates unique optical media for each node from a shared NFS mount
func (h *HMCProvider) bootNodeWithISO(ctx context.Context, node *types.NodeConfig) error {
	h.logger.Info("Booting node with Agent ISO via NFS", "node", node.ExistingLPARName)

	// Step 1: Validate LPAR UUID (populated by DiscoverMetadata)
	if node.UUID == "" {
		return fmt.Errorf("LPAR UUID not found for %s", node.ExistingLPARName)
	}
	// ========================================================================
	// STEP 1.5: ENSURE LPAR IS POWERED OFF
	// ========================================================================
	lparDetails, err := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID)
	if err == nil && (lparDetails.PartitionState == "running" || lparDetails.PartitionState == "open firmware") {
		h.logger.Info("LPAR is active. Powering off before ISO boot...", "state", lparDetails.PartitionState)
		_, _ = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false)
		h.logger.Info("Waiting 15 seconds for LPAR to fully power off...")
		time.Sleep(15 * time.Second)
	}

	// Step 2: Ensure viosadmin user exists (required for VIOS operations)
	h.logger.Info("Checking viosadmin user on HMC")
	// Shield from cancellation to prevent partially created VIOS admin account
	viosUsername, viosPassword, viosUserCreated, err := h.hmcClient.EnsureVIOSAdminUser(context.WithoutCancel(ctx), h.cfg.HMC.Username, h.cfg.HMC.Password)
	if err != nil {
		return fmt.Errorf("failed to ensure viosadmin user: %w", err)
	}
	if viosUserCreated {
		h.logger.Info("viosadmin user created", "username", viosUsername)
	} else {
		h.logger.Info("viosadmin user already exists", "username", viosUsername)
	}

	// Step 3: Get or select active VIOS (per physical system)
	if h.systemVIOSUUIDs == nil {
		h.systemVIOSUUIDs = make(map[string]string)
		h.systemVIOSNames = make(map[string]string)
	}

	var viosUUID, viosName string
	if id, exists := h.systemVIOSUUIDs[node.SystemName]; exists {
		// Reuse previously selected VIOS for this specific system
		viosUUID = id
		viosName = h.systemVIOSNames[node.SystemName]
		h.logger.Info("Reusing selected VIOS for system", "system", node.SystemName, "vios", viosName)
	} else {
		// First node on this system: discover and store VIOS selection
		var err error
		viosUUID, viosName, err = h.getActiveVIOS(ctx, node.SystemName)
		if err != nil {
			return fmt.Errorf("failed to get active VIOS for system %s: %w", node.SystemName, err)
		}
		h.systemVIOSUUIDs[node.SystemName] = viosUUID
		h.systemVIOSNames[node.SystemName] = viosName
		h.logger.Info("Selected VIOS for system", "system", node.SystemName, "vios", viosName)
	}

	// Step 4: Get system UUID
	sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName)
	if err != nil {
		return fmt.Errorf("failed to get system UUID: %w", err)
	}

	// ========================================================================
	// STEP 5: DETERMINE MOUNT POINT AND MEDIA NAME (WITH RESUME SUPPORT)
	// ========================================================================
	var mountPoint, mediaName string

	// Look before you leap: Check if state file already has a mapping for this node
	if h.stateManager != nil {
		if state, err := h.stateManager.LoadState(); err == nil && state != nil {
			for _, m := range state.ISOMappings {
				if m.NodeName == node.Hostname {
					mountPoint = m.MountPoint
					mediaName = m.MediaName
					h.logger.Info("Resume detected: Reusing existing ISO configuration", "media", mediaName, "mount", mountPoint)
					break
				}
			}
		}
	}

	// If no existing media name found, generate a new one
	if mediaName == "" {
		randomStr := fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFFFFF)
		mediaName = fmt.Sprintf("%s-iso", randomStr)
	}

	// If no existing mount point found, generate or reuse the shared VIOS mount point
	if h.viosMountPoints == nil {
		h.viosMountPoints = make(map[string]string)
	}
	if h.viosMounted == nil {
		h.viosMounted = make(map[string]bool)
	}

	if mountPoint == "" {
		if mp, exists := h.viosMountPoints[viosUUID]; exists {
			mountPoint = mp
			h.logger.Info("Reusing existing mount point for VIOS", "vios", viosName, "mount", mountPoint)
		} else {
			randomStr := fmt.Sprintf("%d", time.Now().Unix())
			mountPoint = fmt.Sprintf("/mnt/%s-%s", h.cfg.OpenShift.ClusterName, randomStr)
			h.viosMountPoints[viosUUID] = mountPoint
			h.logger.Info("Generated new mount point for VIOS", "vios", viosName, "mount", mountPoint)
		}
	}

	h.logger.Info("Mount point and media configuration", "mount", mountPoint, "media", mediaName, "node", node.Hostname)

	// ========================================================================
	// STEP 6: MOUNT NFS (External BYO-NFS or Local Dynamic Resolution)
	// ========================================================================
	nfsServer := h.cfg.Network.ControllerIP
	exportPath := fmt.Sprintf("/opt/shiftlaunch/clusters/%s/install-dir", h.cfg.OpenShift.ClusterName)

	if !h.cfg.Services.NFS.IsManaged() && h.cfg.Services.NFS.GetExternal() != "" {
		// External "Bring Your Own" NFS
		nfsServer = h.cfg.Services.NFS.GetExternal()
		// If they provided a custom mount path, use it! Otherwise, fall back safely.
		if h.cfg.Services.NFS.GetExternalPath() != "" {
			exportPath = h.cfg.Services.NFS.GetExternalPath()
		}
	} else {
		// Local Managed NFS: Dynamically discover the Management IP that can route to the VIOS
		if conn, err := net.Dial("udp", h.cfg.HMC.IP+":443"); err == nil {
			nfsServer = conn.LocalAddr().(*net.UDPAddr).IP.String()
			conn.Close()
		}
	}

	// Check if we've already mounted NFS for this VIOS
	if !h.viosMounted[viosUUID] {
		h.logger.Info("Creating mount directory on VIOS", "path", mountPoint)

		// Create mount directory
		mkdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "mkdir -p %s" --admin`, node.SystemName, viosName, mountPoint)
		if _, err := hmc.CliRunnerViaSSH(h.cfg.HMC.IP, viosUsername, viosPassword, mkdirCmd); err != nil {
			return fmt.Errorf("failed to create mount directory: %w", err)
		}

		h.logger.Info("Mounting NFS on VIOS", "server", nfsServer, "export", exportPath, "mount", mountPoint)

		// Mount NFS with retry logic
		var mountErr error
		maxRetries := 3
		for i := 0; i < maxRetries; i++ {
			// Shield from cancellation to prevent locked VIOS mount daemon
			_, mountErr = hmc.MountNFS(context.WithoutCancel(ctx), h.hmcClient, node.SystemName, viosName, nfsServer, exportPath, mountPoint, "3")
			if mountErr == nil || strings.Contains(mountErr.Error(), "already mounted") {
				mountErr = nil
				break
			}
			if (strings.Contains(mountErr.Error(), "500") || strings.Contains(mountErr.Error(), "session is null")) && i < maxRetries-1 {
				h.logger.Warn(fmt.Sprintf("HMC session corrupted during NFS mount (attempt %d/%d). Re-authenticating...", i+1, maxRetries))
				_ = h.hmcClient.Logoff(ctx)
				time.Sleep(2 * time.Second)
				_ = h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password)
				time.Sleep(3 * time.Second)
				continue
			}
			break
		}
		if mountErr != nil {
			return fmt.Errorf("failed to mount NFS: %w", mountErr)
		}

		// Mark this VIOS as mounted
		h.viosMounted[viosUUID] = true
		h.logger.Info("NFS mounted successfully")

		// Save NFS mount to state file
		if h.stateManager != nil {
			state, err := h.stateManager.LoadState()
			if err != nil || state == nil {
				state = &types.DeploymentState{
					ClusterName: h.cfg.OpenShift.ClusterName,
					Status:      "in_progress",
					StartTime:   time.Now().Format(time.RFC3339),
				}
			}

			// Add NFS mount record
			nfsMount := types.NFSMount{
				VIOSUUID:   viosUUID,
				VIOSName:   viosName,
				SystemName: node.SystemName,
				MountPoint: mountPoint,
				NFSServer:  nfsServer,
				ExportPath: exportPath,
				MountedAt:  time.Now().Format(time.RFC3339),
			}

			// Check if mount already exists
			mountExists := false
			for _, existing := range state.NFSMounts {
				if existing.VIOSUUID == nfsMount.VIOSUUID && existing.MountPoint == nfsMount.MountPoint {
					mountExists = true
					break
				}
			}

			if !mountExists {
				state.NFSMounts = append(state.NFSMounts, nfsMount)
				if err := h.stateManager.SaveState(state); err != nil {
					h.logger.Warn("Failed to save NFS mount to state", "error", err)
				} else {
					h.logger.Info("Saved NFS mount to state file", "mount", mountPoint)
				}
			}
		}
	} else {
		h.logger.Info("NFS already mounted on this VIOS, skipping mount")
	}

	// ========================================================================
	// STEP 6.5: ENSURE MEDIA REPOSITORY EXISTS
	// ========================================================================
	if err := h.ensureMediaRepository(ctx, node.SystemName, viosUUID, viosName); err != nil {
		return fmt.Errorf("failed to ensure media repository exists on VIOS '%s' (System: '%s'): %w", viosName, node.SystemName, err)
	}

	// ========================================================================
	// STEP 7: CREATE UNIQUE OPTICAL MEDIA FOR THIS NODE
	// ========================================================================
	isoPath := fmt.Sprintf("%s/agent.ppc64le.iso", mountPoint)
	h.logger.Info("Creating optical media for node", "media", mediaName, "iso", isoPath)
	h.logger.Info("Uploading ISO to VIOS repository (this copies ~1GB and may take a few minutes)...", "iso", isoPath)

	// Refresh HMC session before long-running operation to prevent timeout
	h.logger.Info("Refreshing HMC session before ISO upload...")
	_ = h.hmcClient.Logoff(ctx)
	time.Sleep(2 * time.Second)
	if err := h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password); err != nil {
		return fmt.Errorf("failed to refresh HMC session: %w", err)
	}
	time.Sleep(3 * time.Second)

	// Use CreateVirtualOpticalMedia with read-only flag
	// Shield from cancellation - 1GB ISO transfer must complete to prevent VIOS corruption
	err = h.hmcClient.CreateVirtualOpticalMedia(
		context.WithoutCancel(ctx), // ctx - shielded from cancellation
		node.SystemName,            // sysName
		viosUUID,                   // viosUUID
		viosName,                   // viosName
		mediaName,                  // mediaName
		isoPath,                    // sourceFile (path to ISO on NFS mount)
		0,                          // sizeMB (not used when sourceFile is provided)
		true,                       // readOnly (create with -ro flag)
		false,                      // nfsLink (MUST be false to allow concurrent node booting - VIOS copies ISO locally)
	)
	if err != nil {
		return fmt.Errorf("failed to create optical media on VIOS '%s' (System: '%s'): %w", viosName, node.SystemName, err)
	}
	h.logger.Info("Optical media created successfully", "media", mediaName)

	// ========================================================================
	// STEP 8: MAP OPTICAL MEDIA TO LPAR (With LBYL Check)
	// ========================================================================
	h.logger.Info("Checking if optical media is already mapped to LPAR...", "lpar", node.ExistingLPARName, "media", mediaName)

	alreadyMapped := false
	var mediaToUnmap []string

	mappings, mapCheckErr := h.hmcClient.GetViosSCSIMappings(ctx, viosUUID)
	if mapCheckErr != nil {
		h.logger.Warn("Failed to fetch VIOS mappings for verification, proceeding with map attempt", "error", mapCheckErr)
	} else {
		targetLparLower := strings.ToLower(node.UUID)
		for _, mapping := range mappings {
			// Check if mapping belongs to our LPAR
			if strings.HasSuffix(strings.ToLower(mapping.AssociatedLogicalPartition.Href), targetLparLower) {
				mappedMedia := mapping.Storage.VirtualOpticalMedia.MediaName
				if mappedMedia == mediaName {
					alreadyMapped = true
				} else if mappedMedia != "" {
					// We found a different ISO stuck in the drive from a previous failure. Tag it for unmapping.
					mediaToUnmap = append(mediaToUnmap, mappedMedia)
				}
			}
		}
	}

	// Clean up any stale media mapped to this LPAR before inserting the new one
	if len(mediaToUnmap) > 0 {
		h.logger.Info("Found stale optical media mapped to LPAR. Unmapping...", "lpar", node.ExistingLPARName, "stale_media", mediaToUnmap)
		// Shield from cancellation to prevent orphaned vSCSI adapters
		_, err = h.hmcClient.DeleteVirtualOpticalMaps(context.WithoutCancel(ctx), sysUUID, viosUUID, node.UUID, mediaToUnmap)
		if err != nil {
			h.logger.Warn("Failed to unmap stale media. The mapping step may fail.", "error", err)
		} else {
			h.logger.Info("Successfully unmapped stale media.")
			time.Sleep(3 * time.Second) // Give HMC a moment to process the unmap
		}
	}

	if alreadyMapped {
		h.logger.Info("Optical media is already mapped to LPAR. Skipping mapping step.")
	} else {
		h.logger.Info("Mapping optical media to LPAR", "lpar", node.ExistingLPARName, "media", mediaName)

		// Shield from cancellation to prevent orphaned vSCSI adapters
		_, err = h.hmcClient.CreateVirtualOpticalMaps(context.WithoutCancel(ctx), sysUUID, viosUUID, node.UUID, []string{mediaName})
		if err != nil {
			return fmt.Errorf("failed to map optical media: %w", err)
		}

		h.logger.Info("Optical media mapped successfully")
	}

	// ========================================================================
	// STEP 8.5: SAVE STATE IMMEDIATELY AFTER MAPPING
	// ========================================================================
	if h.isoMappings == nil {
		h.isoMappings = []types.ISOMapping{}
	}
	mapping := types.ISOMapping{
		NodeName:   node.Hostname,
		MediaName:  mediaName,
		VIOSUUID:   viosUUID,
		VIOSName:   viosName,
		LparUUID:   node.UUID,
		SystemName: node.SystemName,
		MountPoint: mountPoint,
		MappedAt:   time.Now().Format(time.RFC3339),
	}

	if h.stateManager != nil {
		state, err := h.stateManager.LoadState()
		if err != nil || state == nil {
			state = &types.DeploymentState{
				ClusterName: h.cfg.OpenShift.ClusterName,
				Status:      "in_progress",
				StartTime:   time.Now().Format(time.RFC3339),
			}
		}

		mappingExists := false
		for _, existing := range state.ISOMappings {
			if existing.NodeName == mapping.NodeName {
				mappingExists = true
				break
			}
		}

		if !mappingExists {
			state.ISOMappings = append(state.ISOMappings, mapping)
		}

		state.VIOSAdminUsername = viosUsername
		state.VIOSAdminPassword = viosPassword
		state.VIOSAdminCreated = viosUserCreated
		state.VIOSAdminCheckedAt = time.Now().Format(time.RFC3339)

		if err := h.stateManager.SaveState(state); err != nil {
			h.logger.Warn("Failed to save ISO mappings to state", "error", err)
		} else {
			h.logger.Info("Saved ISO mappings to state file", "count", len(state.ISOMappings))
		}
	}

	// ========================================================================
	// STEP 9: SAVE PARTITION PROFILE
	// ========================================================================
	profileName := "default_profile"
	h.logger.Info("Saving partition profile", "profile", profileName)

	// Shield from cancellation to prevent profile corruption
	err = h.hmcClient.SaveCurrentLparConfig(context.WithoutCancel(ctx), node.UUID, profileName, true)
	if err != nil {
		return fmt.Errorf("failed to save partition profile: %w", err)
	}

	// ========================================================================
	// STEP 9.5: SET BOOT STRING TO PRIORITIZE ISO
	// ========================================================================
	h.logger.Info("Setting Pending Boot String to 'cd/dvd-all'...")

	// Shield from cancellation to prevent boot definition corruption
	err = h.hmcClient.SetPartitionBootString(context.WithoutCancel(ctx), node.UUID, "cd/dvd-all")
	if err != nil {
		h.logger.Warn("Failed to set boot string (may require manual SMS boot)", "error", err)
	} else {
		h.logger.Info("Boot string set to 'cd/dvd-all'")
	}

	// ========================================================================
	// STEP 10: GET PROFILE UUID AND POWER ON
	// ========================================================================
	lparDetails2, err2 := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID)
	if err2 != nil {
		return fmt.Errorf("failed to get LPAR details: %w", err2)
	}

	profileHref2 := lparDetails2.AssociatedPartitionProfile.Href
	if profileHref2 == "" {
		return fmt.Errorf("no associated partition profile found")
	}

	// Extract UUID from href (last 36 characters)
	profileUUID2 := profileHref2[len(profileHref2)-36:]

	h.logger.Info("Powering on LPAR with ISO boot", "lpar", node.ExistingLPARName)

	powerOnOpts := &hmc.PowerOnOptions{
		ProfileUUID: profileUUID2,
		BootMode:    "norm",
		Keylock:     "normal",
	}

	_, err = h.hmcClient.PowerOnPartition(ctx, node.UUID, powerOnOpts)
	if err != nil {
		if strings.Contains(err.Error(), "already running") {
			h.logger.Info("LPAR already running")
		} else {
			return fmt.Errorf("failed to power on LPAR: %w", err)
		}
	}

	h.logger.Info("LPAR powered on successfully with Agent ISO")

	return nil
}

// getActiveVIOS discovers and returns the first active VIOS on the system
func (h *HMCProvider) getActiveVIOS(ctx context.Context, systemName string) (uuid, name string, err error) {
	sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, systemName)
	if err != nil {
		return "", "", err
	}

	viosList, err := h.hmcClient.GetVirtualIOServersQuick(ctx, sysUUID)
	if err != nil {
		return "", "", err
	}

	if len(viosList) == 0 {
		return "", "", fmt.Errorf("no VIOS servers found on system %s", systemName)
	}

	viosUUIDs := make([]string, len(viosList))
	for i, v := range viosList {
		viosUUIDs[i] = v.UUID
	}

	activeVIOSMap, err := h.hmcClient.GetActiveVIOSServers(ctx, sysUUID, viosUUIDs)
	if err != nil {
		return "", "", err
	}

	for uuid, details := range activeVIOSMap {
		return uuid, details.PartitionName, nil
	}

	return "", "", fmt.Errorf("no active VIOS found on system %s", systemName)
}

// containsPhase checks if a phase exists in the completed phases list
func containsPhase(phases []string, phase string) bool {
	for _, p := range phases {
		if p == phase {
			return true
		}
	}
	return false
}

// bootNodesWithISOBulk aggregates ISO mappings and triggers them atomically per VIOS
func (h *HMCProvider) bootNodesWithISOBulk(ctx context.Context) error {
	h.logger.Info("Bulk provisioning Agent ISOs via NFS for all nodes")

	state, err := h.stateManager.LoadState()
	if err != nil || state == nil {
		state = &types.DeploymentState{
			ClusterName: h.cfg.OpenShift.ClusterName,
			Status:      "in_progress",
		}
	}

	// 1. Identify which nodes actually need booting
	var nodesToProcess []*types.NodeConfig
	for _, node := range h.cfg.GetAllNodes() {
		bootMarker := "booted_" + node.Hostname
		if containsPhase(state.CompletedPhases, bootMarker) {
			h.logger.Info("Skipping already booted node", "node", node.Hostname)
			continue
		}
		if node.UUID == "" {
			return fmt.Errorf("LPAR UUID not found for %s", node.ExistingLPARName)
		}
		nodesToProcess = append(nodesToProcess, node)
	}

	if len(nodesToProcess) == 0 {
		return nil
	}

	h.logger.Info("Checking viosadmin user on HMC")
	viosUsername, viosPassword, viosUserCreated, err := h.hmcClient.EnsureVIOSAdminUser(context.WithoutCancel(ctx), h.cfg.HMC.Username, h.cfg.HMC.Password)
	if err != nil {
		return fmt.Errorf("failed to ensure viosadmin user: %w", err)
	}

	// Initialize tracking maps
	if h.systemVIOSUUIDs == nil {
		h.systemVIOSUUIDs = make(map[string]string)
		h.systemVIOSNames = make(map[string]string)
	}
	if h.viosMountPoints == nil {
		h.viosMountPoints = make(map[string]string)
	}
	if h.viosMounted == nil {
		h.viosMounted = make(map[string]bool)
	}

	// 🛡️Track ISO creation to share a single media file across all LPARs!
	viosMediaCreated := make(map[string]string)
	viosMediaUploaded := make(map[string]bool)

	// ---  Generate a unique batch hash so Day-2 scaling doesn't collide with Day-1 ISOs! ---
	batchHash := fmt.Sprintf("%x", time.Now().Unix()%0xFFFFF)

	// bulkMap Structure: map[sysUUID]map[viosUUID]map[lparUUID][]string
	bulkMap := make(map[string]map[string]map[string][]string)

	// Keep track of metadata for the power-on sequence later
	type nodeMeta struct {
		sysUUID    string
		viosUUID   string
		viosName   string
		mediaName  string
		mountPoint string
	}
	metaTracker := make(map[string]nodeMeta)

	for _, node := range nodesToProcess {
		h.logger.Info("Preparing node for ISO boot...", "node", node.ExistingLPARName)

		// Ensure LPAR is powered off
		lparDetails, err := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID)
		if err == nil && (lparDetails.PartitionState == "running" || lparDetails.PartitionState == "open firmware") {
			h.logger.Info("LPAR is active. Powering off before ISO boot...", "state", lparDetails.PartitionState)
			_, _ = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false)
			time.Sleep(15 * time.Second)
		}

		// Get or select active VIOS
		var viosUUID, viosName string
		if id, exists := h.systemVIOSUUIDs[node.SystemName]; exists {
			viosUUID = id
			viosName = h.systemVIOSNames[node.SystemName]
		} else {
			viosUUID, viosName, err = h.getActiveVIOS(ctx, node.SystemName)
			if err != nil {
				return fmt.Errorf("failed to get active VIOS for system %s: %w", node.SystemName, err)
			}
			h.systemVIOSUUIDs[node.SystemName] = viosUUID
			h.systemVIOSNames[node.SystemName] = viosName
		}

		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName)
		if err != nil {
			return fmt.Errorf("failed to get system UUID: %w", err)
		}

		// Calculate Media Name & Mount Point (with resume support)
		var mountPoint, mediaName string
		for _, m := range state.ISOMappings {
			if m.NodeName == node.Hostname {
				mountPoint = m.MountPoint
				mediaName = m.MediaName
				break
			}
		}

		// ---  Apply the unique batch hash to the ISO name ---
		if mediaName == "" {
			if existingMedia, ok := viosMediaCreated[viosUUID]; ok {
				mediaName = existingMedia
			} else {
				// Ensure name is safe for VIOS (max 37 chars, alphanumeric, dashes)
				mediaName = fmt.Sprintf("%s-%s", h.cfg.OpenShift.ClusterName, batchHash)
				mediaName = strings.ReplaceAll(mediaName, "_", "-")
				if len(mediaName) > 30 {
					mediaName = mediaName[:30]
				}
				viosMediaCreated[viosUUID] = mediaName
			}
		} else {
			viosMediaCreated[viosUUID] = mediaName
		}

		if mountPoint == "" {
			if mp, exists := h.viosMountPoints[viosUUID]; exists {
				mountPoint = mp
			} else {
				mountPoint = fmt.Sprintf("/mnt/%s-%d", h.cfg.OpenShift.ClusterName, time.Now().Unix())
				h.viosMountPoints[viosUUID] = mountPoint
			}
		}

		// Ensure NFS is Mounted on VIOS (External BYO-NFS or Local Dynamic Resolution)
		if !h.viosMounted[viosUUID] {
			nfsServer := h.cfg.Network.ControllerIP
			exportPath := fmt.Sprintf("/opt/shiftlaunch/clusters/%s/install-dir", h.cfg.OpenShift.ClusterName)

			if !h.cfg.Services.NFS.IsManaged() && h.cfg.Services.NFS.GetExternal() != "" {
				// External "Bring Your Own" NFS
				nfsServer = h.cfg.Services.NFS.GetExternal()
				// Allow enterprise users to specify custom export path
				if customPath := h.cfg.Services.NFS.GetExternalPath(); customPath != "" {
					exportPath = customPath
				}
			} else {
				// Local Managed NFS: Dynamically discover the Management IP that can route to the VIOS
				if conn, err := net.Dial("udp", h.cfg.HMC.IP+":443"); err == nil {
					nfsServer = conn.LocalAddr().(*net.UDPAddr).IP.String()
					conn.Close()
				}
			}

			h.logger.Info("Creating mount directory on VIOS", "path", mountPoint)
			mkdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "mkdir -p %s" --admin`, node.SystemName, viosName, mountPoint)
			hmc.CliRunnerViaSSH(h.cfg.HMC.IP, viosUsername, viosPassword, mkdirCmd)
	
			h.logger.Info("Mounting NFS on VIOS", "server", nfsServer, "export", exportPath)
			_, err = hmc.MountNFS(context.WithoutCancel(ctx), h.hmcClient, node.SystemName, viosName, nfsServer, exportPath, mountPoint, "3")
			if err != nil && !strings.Contains(err.Error(), "already mounted") {
				return fmt.Errorf("failed to mount NFS: %w", err)
			}
			h.viosMounted[viosUUID] = true
		}

		// Ensure Media Repository Exists
		if err := h.ensureMediaRepository(ctx, node.SystemName, viosUUID, viosName); err != nil {
			return fmt.Errorf("failed to ensure media repository: %w", err)
		}

		// 🛡️  Copy the ISO File to the VIOS locally (ONLY ONCE PER VIOS!)
		if !viosMediaUploaded[viosUUID] {
			isoPath := fmt.Sprintf("%s/agent.ppc64le.iso", mountPoint)
			h.logger.Info("Uploading ISO to VIOS repository (this copies ~1GB and may take a few minutes)...", "iso", isoPath)
			err = h.hmcClient.CreateVirtualOpticalMedia(
				context.WithoutCancel(ctx), node.SystemName, viosUUID, viosName, mediaName, isoPath, 0, true, false)

			if err != nil && !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("failed to create optical media on VIOS: %w", err)
			}
			viosMediaUploaded[viosUUID] = true
		} else {
			h.logger.Info("ISO already uploaded to this VIOS. Skipping file transfer.", "vios", viosName, "media", mediaName)
		}

		// Aggregating Mappings into Bulk Map
		if bulkMap[sysUUID] == nil {
			bulkMap[sysUUID] = make(map[string]map[string][]string)
		}
		if bulkMap[sysUUID][viosUUID] == nil {
			bulkMap[sysUUID][viosUUID] = make(map[string][]string)
		}
		bulkMap[sysUUID][viosUUID][node.UUID] = append(bulkMap[sysUUID][viosUUID][node.UUID], mediaName)

		metaTracker[node.Hostname] = nodeMeta{sysUUID, viosUUID, viosName, mediaName, mountPoint}

		// Save state tracking
		if h.isoMappings == nil {
			h.isoMappings = []types.ISOMapping{}
		}

		exists := false
		for _, m := range h.isoMappings {
			if m.NodeName == node.Hostname {
				exists = true
				break
			}
		}

		if !exists {
			h.isoMappings = append(h.isoMappings, types.ISOMapping{
				NodeName:   node.Hostname,
				MediaName:  mediaName,
				VIOSUUID:   viosUUID,
				VIOSName:   viosName,
				LparUUID:   node.UUID,
				SystemName: node.SystemName,
				MountPoint: mountPoint,
				MappedAt:   time.Now().Format(time.RFC3339),
			})
		}
	}

	// 2. Execute Bulk Virtual Optical Mappings natively per VIOS
	if len(bulkMap) > 0 {
		h.logger.Info("Executing ATOMIC BULK MAP of Virtual Optical Media...")
		for sysUUID, viosMap := range bulkMap {
			for viosUUID, lparMediaMap := range viosMap {
				h.logger.Info("Bulk mapping on VIOS", "viosUUID", viosUUID, "lpar_count", len(lparMediaMap))

				_, err := h.hmcClient.CreateVirtualOpticalMapsMultiLpar(
					context.WithoutCancel(ctx), sysUUID, viosUUID, lparMediaMap)
				if err != nil {
					return fmt.Errorf("failed to bulk map optical media: %w", err)
				}
			}
		}
		h.logger.Info("✅ Bulk ISO mapping completed successfully")
	}

	// Persist ISO configurations to State (Safely append without overwriting Day-1)
	for _, newMapping := range h.isoMappings {
		exists := false
		for _, existingMapping := range state.ISOMappings {
			if existingMapping.NodeName == newMapping.NodeName {
				exists = true
				break
			}
		}
		if !exists {
			state.ISOMappings = append(state.ISOMappings, newMapping)
		}
	}
	state.VIOSAdminUsername = viosUsername
	state.VIOSAdminPassword = viosPassword
	state.VIOSAdminCreated = viosUserCreated
	_ = h.stateManager.SaveState(state)
	// ========================================================================
	// 2.5 IMMEDIATELY UNMOUNT NFS (Since ISOs are now safely inside VIOS)
	// ========================================================================
	h.logger.Info("Cleaning up temporary NFS mounts from VIOS...")
	mountsCleaned := make(map[string]bool)

	for i := range h.isoMappings {
		mp := h.isoMappings[i].MountPoint
		if mp == "" || mountsCleaned[mp] {
			continue
		}

		h.logger.Info("Unmounting NFS from VIOS", "vios", h.isoMappings[i].VIOSName, "mount", mp)
		// Shield from cancellation so we don't leave the VIOS hanging!
		_, err := hmc.UnmountNFS(context.WithoutCancel(ctx), h.hmcClient, h.isoMappings[i].SystemName, h.isoMappings[i].VIOSName, mp)

		if err != nil && !strings.Contains(err.Error(), "Could not find anything to unmount") && !strings.Contains(err.Error(), "not mounted") {
			h.logger.Warn("Failed to cleanly unmount NFS (will retry during teardown)", "error", err)
			continue
		}

		h.logger.Info("Removing mount directory from VIOS", "mount", mp)
		rmdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "rmdir %s" --admin`, h.isoMappings[i].SystemName, h.isoMappings[i].VIOSName, mp)
		_, _ = hmc.CliRunnerViaSSH(h.cfg.HMC.IP, viosUsername, viosPassword, rmdirCmd)

		mountsCleaned[mp] = true
	}

	// Update state to reflect unmounted status so Teardown ignores them later
	for i := range state.ISOMappings {
		if mountsCleaned[state.ISOMappings[i].MountPoint] {
			state.ISOMappings[i].MountPoint = ""
		}
	}
	for i := range h.isoMappings {
		if mountsCleaned[h.isoMappings[i].MountPoint] {
			h.isoMappings[i].MountPoint = ""
		}
	}
	_ = h.stateManager.SaveState(state)

	// 3. Save Profiles and Power On (PARALLELIZED & SHIELDED)
	h.logger.Info("Saving profiles and powering on all LPARs concurrently...")
	var wg sync.WaitGroup
	var stateMu sync.Mutex
	errCh := make(chan error, len(nodesToProcess))

	for _, n := range nodesToProcess {
		wg.Add(1)

		go func(targetNode *types.NodeConfig) {
			defer wg.Done()
			h.logger.Info("Configuring and booting", "lpar", targetNode.ExistingLPARName)

			// 🛡️ SHIELDED API CALLS: Protect the HMC session token from concurrent corruption
			apiTrafficMutex.RLock()
			_ = h.hmcClient.SaveCurrentLparConfig(context.WithoutCancel(ctx), targetNode.UUID, "default_profile", true)
			_ = h.hmcClient.SetPartitionBootString(context.WithoutCancel(ctx), targetNode.UUID, "cd/dvd-all")
	
			lparDetails, err := h.hmcClient.GetLogicalPartitionDetailed(ctx, targetNode.UUID)
			apiTrafficMutex.RUnlock()

			if err != nil {
				errCh <- fmt.Errorf("failed to fetch LPAR details for %s: %w", targetNode.Hostname, err)
				return
			}

			profileHref := lparDetails.AssociatedPartitionProfile.Href
			if len(profileHref) < 36 {
				errCh <- fmt.Errorf("invalid profile href format for LPAR %s: '%s'", targetNode.Hostname, profileHref)
				return
			}
			profileUUID := profileHref[len(profileHref)-36:]

			powerOnOpts := &hmc.PowerOnOptions{
				ProfileUUID: profileUUID,
				BootMode:    "norm",
				Keylock:     "normal",
			}

			apiTrafficMutex.RLock()
			_, err = h.hmcClient.PowerOnPartition(ctx, targetNode.UUID, powerOnOpts)
			apiTrafficMutex.RUnlock()

			if err != nil && !strings.Contains(err.Error(), "already running") {
				errCh <- fmt.Errorf("failed to power on LPAR %s: %w", targetNode.Hostname, err)
				return
			}

			// Update state tracking safely
			bootMarker := "booted_" + targetNode.Hostname
			stateMu.Lock()
			if !containsPhase(state.CompletedPhases, bootMarker) {
				state.CompletedPhases = append(state.CompletedPhases, bootMarker)
				_ = h.stateManager.SaveState(state)
			}
			stateMu.Unlock()

			h.logger.Info("LPAR booted successfully", "lpar", targetNode.ExistingLPARName)
		}(n)
	}

	wg.Wait()
	close(errCh)

	// 🛡️ AGGREGATE AND RETURN ERRORS
	var errs []string
	for err := range errCh {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("parallel boot encountered errors: %s", strings.Join(errs, "; "))
	}

	return nil
}

// CleanupISOMappings unmaps and deletes ISO media (called during teardown)
func (h *HMCProvider) CleanupISOMappings(ctx context.Context) error {
	// Load ISO mappings from state file if not in memory
	if len(h.isoMappings) == 0 && h.stateManager != nil {
		if state, err := h.stateManager.LoadState(); err == nil && state != nil {
			h.isoMappings = state.ISOMappings
			h.logger.Info("Loaded ISO mappings from state file", "count", len(h.isoMappings))
		}
	}

	if len(h.isoMappings) == 0 {
		h.logger.Info("No ISO mappings found to clean up")
		return nil
	}

	var viosUsername, viosPassword string

	// Ensure we don't accept an empty password from a corrupted state file
	if state, err := h.stateManager.LoadState(); err == nil && state != nil && state.VIOSAdminUsername != "" && state.VIOSAdminPassword != "" {
		viosUsername = state.VIOSAdminUsername
		viosPassword = state.VIOSAdminPassword
		h.logger.Info("Using viosadmin credentials from state file", "username", viosUsername)
	} else {
		var created bool
		var apiErr error // Fixed: explicit declaration prevents undefined 'err'
		// Shield from cancellation to prevent partially created VIOS admin account
		viosUsername, viosPassword, created, apiErr = h.hmcClient.EnsureVIOSAdminUser(context.WithoutCancel(ctx), h.cfg.HMC.Username, h.cfg.HMC.Password)
		if apiErr != nil || viosPassword == "" {
			h.logger.Warn("Failed to get viosadmin credentials via API, falling back to default", "error", apiErr)
			viosUsername, viosPassword = h.hmcClient.GetVIOSAdminCredentials()
		} else if created {
			h.logger.Info("Created viosadmin user for cleanup", "username", viosUsername)
		}
	}

	h.logger.Info("Cleaning up ISO mappings", "count", len(h.isoMappings))

	errorCount := 0

	// ========================================================================
	// 0. BULK UNMAP VIRTUAL OPTICAL MEDIA
	// ========================================================================
	h.logger.Info("Preparing Bulk Optical Unmapping...")
	// Structure: map[sysUUID]map[viosUUID]map[lparUUID][]string
	unmapTargets := make(map[string]map[string]map[string][]string)

	for _, mapping := range h.isoMappings {
		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(context.WithoutCancel(ctx), mapping.SystemName)
		if err != nil {
			h.logger.Warn("Failed to get system UUID for cleanup", "system", mapping.SystemName, "error", err)
			continue
		}

		if unmapTargets[sysUUID] == nil {
			unmapTargets[sysUUID] = make(map[string]map[string][]string)
		}
		if unmapTargets[sysUUID][mapping.VIOSUUID] == nil {
			unmapTargets[sysUUID][mapping.VIOSUUID] = make(map[string][]string)
		}
		unmapTargets[sysUUID][mapping.VIOSUUID][mapping.LparUUID] = append(
			unmapTargets[sysUUID][mapping.VIOSUUID][mapping.LparUUID], mapping.MediaName)
	}

	// Execute Bulk Unmap Per VIOS
	for sysUUID, viosMap := range unmapTargets {
		for viosUUID, lparMediaMap := range viosMap {
			h.logger.Info("Bulk unmapping optical media from VIOS...", "viosUUID", viosUUID, "lpar_count", len(lparMediaMap))

			_, err := h.hmcClient.DeleteVirtualOpticalMapsMultiLpar(
				context.WithoutCancel(ctx), sysUUID, viosUUID, lparMediaMap)

			if err != nil {
				h.logger.Error("Failed to bulk unmap optical media", "error", err)
				errorCount++
			} else {
				h.logger.Info("Successfully bulk unmapped optical media")
			}
		}
	}

	// Save LPAR Profiles safely now that media is unmapped (PARALLELIZED)
	h.logger.Info("Saving LPAR profiles concurrently after unmapping...")
	var wgSave sync.WaitGroup

	for _, m := range h.isoMappings {
		wgSave.Add(1)

		go func(targetMapping types.ISOMapping) {
			defer wgSave.Done()

			h.logger.Info("Saving LPAR profile", "node", targetMapping.NodeName)

			err := h.hmcClient.SaveCurrentLparConfig(context.WithoutCancel(ctx), targetMapping.LparUUID, "default_profile", true)
			if err != nil {
				h.logger.Warn("Failed to save LPAR profile", "node", targetMapping.NodeName, "error", err)
			}
		}(m)
	}

	// Wait for all profiles to finish saving to the Hypervisor
	wgSave.Wait()

	// ========================================================================
	// 1. DELETE VIRTUAL OPTICAL MEDIA (Per-Node Media)
	// ========================================================================

	// 🛡️  Deduplicate media deletion so we don't try to delete the shared ISO multiple times!
	mediaDeleted := make(map[string]bool)

	for _, mapping := range h.isoMappings {

		// Skip if we already deleted this exact media from this VIOS
		mediaKey := fmt.Sprintf("%s_%s", mapping.VIOSUUID, mapping.MediaName)
		if mediaDeleted[mediaKey] {
			continue
		}
		mediaDeleted[mediaKey] = true

		h.logger.Info(fmt.Sprintf("Checking repository for media: %s", mapping.MediaName))

		// Shield prerequisite lookup - teardown must not bypass deletion!
		_, err := h.hmcClient.GetVirtualOpticalMedia(context.WithoutCancel(ctx), mapping.SystemName, mapping.VIOSName, mapping.MediaName)

		if err == nil {
			h.logger.Info(fmt.Sprintf("Destroying optical payload: %s", mapping.MediaName))
			// Shield from cancellation - 1GB file deletion must complete to prevent VIOS filesystem corruption
			delErr := h.hmcClient.DeleteVirtualOpticalMedia(
				context.WithoutCancel(ctx),
				mapping.SystemName,
				mapping.VIOSName,
				mapping.MediaName)

			if delErr != nil {
				h.logger.Warn("Failed to delete optical media", "media", mapping.MediaName, "error", delErr)
				errorCount++
			} else {
				h.logger.Info("Successfully deleted optical media", "media", mapping.MediaName)
			}
		} else {
			if strings.Contains(err.Error(), "not found") {
				h.logger.Info("Media not found in repository. Skipping deletion.", "media", mapping.MediaName)
			} else {
				h.logger.Warn("Failed to verify media existence. Skipping deletion.", "error", err)
			}
		}
	}

	// ========================================================================
	// 2. UNMOUNT NFS & REMOVE DIRECTORY (Shared Mount - Do Once)
	// ========================================================================
	mountPointsProcessed := make(map[string]bool)

	for _, mapping := range h.isoMappings {
		if mapping.MountPoint == "" {
			continue
		}

		if mountPointsProcessed[mapping.MountPoint] {
			continue
		}
		mountPointsProcessed[mapping.MountPoint] = true

		h.logger.Info("Unmounting shared NFS from VIOS...", "mount_point", mapping.MountPoint, "vios", mapping.VIOSName)

		// Shield from cancellation to prevent locked VIOS mount daemon
		_, err := hmc.UnmountNFS(context.WithoutCancel(ctx), h.hmcClient, mapping.SystemName, mapping.VIOSName, mapping.MountPoint)

		if err != nil && (strings.Contains(err.Error(), "Could not find anything to unmount") || strings.Contains(err.Error(), "not mounted")) {
			h.logger.Info("Directory is already unmounted from VIOS", "mount_point", mapping.MountPoint)
		} else if err != nil {
			h.logger.Error("Failed to unmount NFS", "mount_point", mapping.MountPoint, "error", err)
		} else {
			h.logger.Info("Successfully unmounted NFS from VIOS", "mount_point", mapping.MountPoint)
		}

		h.logger.Info("Removing mount directory from VIOS", "mount_point", mapping.MountPoint)
		rmdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "rmdir %s" --admin`, mapping.SystemName, mapping.VIOSName, mapping.MountPoint)
		_, err = hmc.CliRunnerViaSSH(h.cfg.HMC.IP, viosUsername, viosPassword, rmdirCmd)

		if err != nil && (strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "not found")) {
			h.logger.Info("Mount directory already removed", "mount_point", mapping.MountPoint)
		} else if err != nil {
			h.logger.Warn("Failed to remove mount directory", "mount_point", mapping.MountPoint, "error", err)
		} else {
			h.logger.Info("Successfully removed mount directory", "mount_point", mapping.MountPoint)
		}
	}

	if errorCount > 0 {
		return fmt.Errorf("cleanup completed with %d errors. Some ISOs or mappings remain on the VIOS", errorCount)
	}

	if h.stateManager != nil {
		if state, err := h.stateManager.LoadState(); err == nil && state != nil {
			state.ISOMappings = nil
			if err := h.stateManager.SaveState(state); err != nil {
				h.logger.Warn("Failed to clear ISO mappings from state", "error", err)
			}
		}
	}

	h.isoMappings = nil
	return nil
}

// ensureMediaRepository checks if the VIOS Media Repository exists, and auto-creates it if missing
func (h *HMCProvider) ensureMediaRepository(ctx context.Context, systemName, viosUUID, viosName string) error {
	repoInfo, err := h.hmcClient.GetMediaRepositoryInfo(ctx, systemName, viosName)

	//  The HMC API can return success (err == nil) but SizeMB = 0 if the repository isn't created,
	// OR it can return an error if the repository doesn't exist. Handle both cases.
	if err == nil && repoInfo.SizeMB > 0 {
		h.logger.Debug("Media Repository already exists", "vios", viosName, "size_mb", repoInfo.SizeMB)
		return nil // Repository legitimately exists
	}

	// If we got an error OR size is 0, we need to check if repository actually exists before creating
	if err == nil && repoInfo.SizeMB == 0 {
		h.logger.Debug("Media Repository query returned size 0, checking if it actually exists...", "vios", viosName)
		// Size 0 might mean it exists but is empty, or it doesn't exist
		// We'll attempt to create it, and if it fails with "already exists", that's fine
	}

	h.logger.Info("Media Repository not found. Auto-creating...", "vios", viosName)

	// Calculate size requirements
	nodes := h.cfg.GetAllNodes()
	requiredMB := 1536 * len(nodes)
	if requiredMB < 10240 {
		requiredMB = 10240
	}
	requiredGB := float64(requiredMB) / 1024.0

	// Find suitable Volume Group
	vgs, vgErr := h.hmcClient.GetVolumeGroups(ctx, viosUUID)
	if vgErr != nil {
		return fmt.Errorf("failed to list volume groups: %w", vgErr)
	}

	var targetVG string
	for _, vg := range vgs {
		if strings.ToLower(vg.GroupName) == "rootvg" {
			continue
		}
		freeSpaceGB, parseErr := strconv.ParseFloat(vg.FreeSpace, 64)
		if parseErr == nil && freeSpaceGB >= requiredGB {
			targetVG = vg.GroupName
			break
		}
	}

	if targetVG == "" {
		for _, vg := range vgs {
			freeSpaceGB, parseErr := strconv.ParseFloat(vg.FreeSpace, 64)
			if parseErr == nil && freeSpaceGB >= requiredGB {
				targetVG = vg.GroupName
				h.logger.Warn("Using rootvg for Media Repository as no other VG has enough free space", "vg", vg.GroupName)
				break
			}
		}
	}

	if targetVG == "" {
		return fmt.Errorf("no volume group found with at least %.2f GB of free space", requiredGB)
	}

	h.logger.Info("Creating Media Repository", "size_mb", requiredMB, "vg", targetVG)
	// Shield from cancellation - modifying VIOS Volume Group must complete to prevent corruption
	if createErr := h.hmcClient.CreateMediaRepository(context.WithoutCancel(ctx), systemName, viosUUID, viosName, targetVG, requiredMB); createErr != nil {
		//  If repository already exists, that's actually OK - just log and continue
		if strings.Contains(createErr.Error(), "already exists") {
			h.logger.Info("Media Repository already exists (detected during creation attempt)", "vios", viosName)
			return nil
		}
		return fmt.Errorf("failed to create media repository: %w", createErr)
	}

	h.logger.Info("Media Repository created successfully")
	return nil
}
