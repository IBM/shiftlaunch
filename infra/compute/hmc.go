package compute

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
	hmc "github.ibm.com/sudeeshjohn/infra-go-sdk/phmc"
)

type HMCProvider struct {
	cfg          *types.AgentConfig
	hmcClient    *hmc.HmcRestClient
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
func (h *HMCProvider) GetHMCClient() *hmc.HmcRestClient {
	return h.hmcClient
}

// DiscoverMetadata loops through your nodes and queries the HMC for network adapter details
func (h *HMCProvider) DiscoverMetadata(ctx context.Context) error {
	h.logger.Info("Discovering LPAR metadata from HMC...")

	for _, node := range h.cfg.GetAllNodes() {
		h.logger.Debug("Querying", "system", node.SystemName, "lpar", node.ExistingLPARName)

		// 1. Get System UUID
		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName, true)
		if err != nil {
			return fmt.Errorf("failed to find system %s: %w", node.SystemName, err)
		}

		// 2. Get LPAR UUID
		// Pass false for debug to avoid logging all LPARs on the system
		lpars, err := h.hmcClient.GetLogicalPartitionsQuickAll(ctx, sysUUID, false)
		if err != nil {
			return err
		}

		for _, l := range lpars {
			if l.PartitionName == node.ExistingLPARName {
				node.UUID = l.UUID
				break
			}
		}
		if node.UUID == "" {
			return fmt.Errorf("LPAR '%s' not found on system %s", node.ExistingLPARName, node.SystemName)
		}

		// 3. Get LPAR Profile UUID (Required for Netboot)
		// Pass false for debug to avoid excessive logging
		profiles, err := h.hmcClient.GetLogicalPartitionProfiles(ctx, node.UUID, false)
		if err != nil || len(profiles) == 0 {
			return fmt.Errorf("no profile found for LPAR %s. A profile is required for network boot", node.ExistingLPARName)
		}
		node.ProfileUUID = profiles[0].UUID

		// 4. Get MAC and Location Code for DHCP/PXE
		// Pass false for debug to avoid excessive logging
		adapters, err := h.hmcClient.GetClientNetworkAdapters(ctx, sysUUID, node.UUID, false)
		if err != nil || len(adapters) == 0 {
			errMsg := "unknown error"
			if err != nil {
				errMsg = err.Error()
			}
			h.logger.Error("No network adapter found on LPAR", "lpar", node.ExistingLPARName, "reason", errMsg)
			return fmt.Errorf("no network adapter found on LPAR %s", node.ExistingLPARName)
		}

		node.MACAddress = hmc.FormatMACAddress(adapters[0].MACAddress)
		node.LocationCode = adapters[0].LocationCode

		// 5. Validate LPAR has attached storage
		volumes, err := h.hmcClient.GetAttachedVolumes(ctx, sysUUID, node.UUID, false)
		if err != nil || len(volumes) == 0 {
			errMsg := "no volumes found"
			if err != nil {
				errMsg = err.Error()
			}
			h.logger.Error("No storage/disk found attached to LPAR", "lpar", node.ExistingLPARName, "reason", errMsg)
			return fmt.Errorf("no storage/disk found attached to LPAR %s. Please attach at least one disk before deployment", node.ExistingLPARName)
		}

		h.logger.Info("Discovered", "lpar", node.ExistingLPARName, "mac", node.MACAddress, "uuid", node.UUID, "disks", len(volumes))

		// Save discovered node to state file
		if h.stateManager != nil {
			state, err := h.stateManager.LoadState()
			if err != nil || state == nil {
				state = &types.DeploymentState{
					ClusterName: h.cfg.OpenShift.ClusterName,
					Status:      "in_progress",
					StartTime:   time.Now().Format(time.RFC3339),
				}
			}

			// Create discovered node record
			discoveredNode := types.DiscoveredNode{
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
			}

			// Check if node already exists in state
			nodeExists := false
			for i, existing := range state.DiscoveredNodes {
				if existing.UUID == discoveredNode.UUID {
					// Update existing node
					state.DiscoveredNodes[i] = discoveredNode
					nodeExists = true
					break
				}
			}

			if !nodeExists {
				state.DiscoveredNodes = append(state.DiscoveredNodes, discoveredNode)
			}

			if err := h.stateManager.SaveState(state); err != nil {
				h.logger.Warn("Failed to save discovered node to state", "error", err)
			}
		}
	}

	// Log summary of discovered nodes
	if h.stateManager != nil {
		if state, err := h.stateManager.LoadState(); err == nil && state != nil {
			h.logger.Info("Saved discovered nodes to state file", "count", len(state.DiscoveredNodes))
		}
	}

	return nil
}

// BootNode routes to the appropriate boot method (netboot or ISO)
func (h *HMCProvider) BootNode(ctx context.Context, node *types.NodeConfig) error {
	// Route based on boot method
	if h.cfg.Nodes.BootMethod == "iso" {
		return h.bootNodeWithISO(ctx, node)
	}

	// Default to netboot
	return h.networkBootLpar(ctx, node)
}

// networkBootLpar executes the lpar_netboot command via REST API for a single node
func (h *HMCProvider) networkBootLpar(ctx context.Context, node *types.NodeConfig) error {
	h.logger.Info("Processing LPAR for netboot", "lpar", node.ExistingLPARName)

	var lparDetailed *hmc.LogicalPartitionDetailed
	var err error
	maxRetries := 3

	// Retry loop for 406/Intermittent HMC errors with re-authentication
	for i := 0; i < maxRetries; i++ {
		lparDetailed, err = h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID, true)
		if err == nil {
			break
		}

		// Check if error is 406 (session issue) and re-authenticate
		if strings.Contains(err.Error(), "406") && i < maxRetries-1 {
			h.logger.Warn(fmt.Sprintf("HMC returned 406 error (attempt %d/%d). Logging out and re-authenticating...", i+1, maxRetries), "error", err)

			// Logout from old session first
			if logoutErr := h.hmcClient.Logoff(ctx); logoutErr != nil {
				h.logger.Debug("Logout failed (session may already be invalid)", "error", logoutErr)
			}

			// Wait a moment for HMC to clean up
			time.Sleep(2 * time.Second)

			// Re-authenticate with fresh session
			if loginErr := h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug); loginErr != nil {
				h.logger.Warn("Re-authentication failed", "error", loginErr)
			} else {
				h.logger.Info("Successfully re-authenticated with HMC")
			}
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
		_ = h.hmcClient.CloseVirtualTerminalViaSsh(
			h.cfg.HMC.IP,
			h.cfg.HMC.Username,
			h.cfg.HMC.Password,
			node.SystemName,
			node.ExistingLPARName,
			true,
		)

		_, err = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false, true)
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
	}, true)
	if err != nil {
		return fmt.Errorf("failed to power on LPAR for adapter registration: %w", err)
	}

	h.logger.Info("Waiting 20 seconds for LPAR to reach Open Firmware and register adapters...")
	time.Sleep(20 * time.Second)

	h.logger.Info("Powering off LPAR for profile query...")
	_, err = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false, true)
	if err != nil {
		return fmt.Errorf("failed to power off LPAR: %w", err)
	}

	h.logger.Info("Waiting 10 seconds for LPAR to fully power off...")
	time.Sleep(10 * time.Second)

	_ = h.hmcClient.CloseVirtualTerminalViaSsh(
		h.cfg.HMC.IP,
		h.cfg.HMC.Username,
		h.cfg.HMC.Password,
		node.SystemName,
		node.ExistingLPARName,
		true,
	)

	// =========================================================================
	// STEP 2: Get authoritative location code from profile
	// =========================================================================
	h.logger.Info("Retrieving network boot device information from profile...")

	var bootDevices []hmc.NetworkBootDevice
	for i := 0; i < maxRetries; i++ {
		bootDevices, err = h.hmcClient.GetNetworkBootDevices(ctx, node.UUID, profileUUID, true)
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
	_ = h.hmcClient.CloseVirtualTerminalViaSsh(
		h.cfg.HMC.IP,
		h.cfg.HMC.Username,
		h.cfg.HMC.Password,
		node.SystemName,
		node.ExistingLPARName,
		true,
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

	status, err := h.hmcClient.PowerOnPartition(ctx, node.UUID, options, true)
	if err != nil {
		return fmt.Errorf("failed to execute network boot: %w", err)
	}

	h.logger.Info("Network boot initiated successfully", "lpar", node.ExistingLPARName, "status", status)

	h.logger.Info("Saving profile to persist configuration...")
	_ = h.hmcClient.SaveCurrentLparConfig(ctx, node.UUID, "default_profile", true, true)

	return nil
}

func (h *HMCProvider) PowerOffNodes(ctx context.Context) error {
	// We must discover metadata first to populate the node.UUID fields during a teardown run
	h.logger.Info("Fetching LPAR UUIDs from HMC for teardown...")

	nodes := h.cfg.GetAllNodes()
	h.logger.Info("Found nodes to power off", "count", len(nodes))

	if err := h.DiscoverMetadata(ctx); err != nil {
		h.logger.Warn("Failed to discover some metadata during teardown, some LPARs may not power off", "error", err)
	}

	h.logger.Info("Sending shutdown signals to all managed LPARs...")
	powerOffCount := 0
	skippedCount := 0

	for _, node := range nodes {
		if node.UUID == "" {
			h.logger.Warn("Skipping power off for LPAR (UUID not found)", "lpar", node.ExistingLPARName)
			skippedCount++
			continue
		}

		h.logger.Info("Attempting to power off LPAR", "lpar", node.ExistingLPARName, "uuid", node.UUID)

		// Send the immediate power off signal.
		// If the LPAR is already off, the HMC returns an error which we catch and log as debug.
		_, err := h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false, true)
		if err != nil {
			// Extract just the first line of the error for cleaner logging
			errMsg := strings.Split(err.Error(), "\n")[0]
			h.logger.Warn("LPAR power off returned an error (may already be off)", "lpar", node.ExistingLPARName, "details", errMsg)
		} else {
			h.logger.Info("Power off signal accepted", "lpar", node.ExistingLPARName)
			powerOffCount++
		}
	}

	h.logger.Info("Power off signals sent", "powered_off", powerOffCount, "skipped", skippedCount, "total", len(nodes))

	if powerOffCount > 0 {
		h.logger.Info("Waiting 15 seconds for LPARs to fully transition to powered-off state...")
		time.Sleep(15 * time.Second)
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
	lparDetails, err := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID, h.debug)
	if err == nil && (lparDetails.PartitionState == "running" || lparDetails.PartitionState == "open firmware") {
		h.logger.Info("LPAR is active. Powering off before ISO boot...", "state", lparDetails.PartitionState)
		_, _ = h.hmcClient.PowerOffPartition(ctx, node.UUID, "Immediate", false, true)
		h.logger.Info("Waiting 15 seconds for LPAR to fully power off...")
		time.Sleep(15 * time.Second)
	}

	// Step 2: Ensure viosadmin user exists (required for VIOS operations)
	h.logger.Info("Checking viosadmin user on HMC")
	viosUsername, viosPassword, viosUserCreated, err := h.hmcClient.EnsureVIOSAdminUser(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug)
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
	sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, node.SystemName, true)
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
	// STEP 6: MOUNT NFS IF NOT ALREADY MOUNTED (SHARED MOUNT PER VIOS)
	// ========================================================================
	nfsServer := h.cfg.Controller.IP
	exportPath := fmt.Sprintf("/opt/shiftlaunch/clusters/%s/install-dir", h.cfg.OpenShift.ClusterName)

	// Check if we've already mounted NFS for this VIOS
	if !h.viosMounted[viosUUID] {
		h.logger.Info("Creating mount directory on VIOS", "path", mountPoint)

		// Create mount directory
		mkdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "mkdir -p %s" --admin`, node.SystemName, viosName, mountPoint)
		if _, err := hmc.CliRunnerViaSsh(h.cfg.HMC.IP, viosUsername, viosPassword, mkdirCmd, h.debug); err != nil {
			return fmt.Errorf("failed to create mount directory: %w", err)
		}

		h.logger.Info("Mounting NFS on VIOS", "server", nfsServer, "export", exportPath, "mount", mountPoint)

		// Mount NFS with retry logic
		var mountErr error
		maxRetries := 3
		for i := 0; i < maxRetries; i++ {
			_, mountErr = hmc.MountNFS(ctx, h.hmcClient, node.SystemName, viosName, nfsServer, exportPath, mountPoint, "3", h.debug)
			if mountErr == nil || strings.Contains(mountErr.Error(), "already mounted") {
				mountErr = nil
				break
			}
			if (strings.Contains(mountErr.Error(), "500") || strings.Contains(mountErr.Error(), "session is null")) && i < maxRetries-1 {
				h.logger.Warn(fmt.Sprintf("HMC session corrupted during NFS mount (attempt %d/%d). Re-authenticating...", i+1, maxRetries))
				_ = h.hmcClient.Logoff(ctx)
				time.Sleep(2 * time.Second)
				_ = h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug)
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
	if err := h.hmcClient.Login(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug); err != nil {
		return fmt.Errorf("failed to refresh HMC session: %w", err)
	}
	time.Sleep(3 * time.Second)

	// Use CreateVirtualOpticalMedia with read-only flag
	err = h.hmcClient.CreateVirtualOpticalMedia(
		ctx,              // ctx
		node.SystemName,  // sysName
		viosUUID,         // viosUUID
		viosName,         // viosName
		mediaName,        // mediaName
		isoPath,          // sourceFile (path to ISO on NFS mount)
		0,                // sizeMB (not used when sourceFile is provided)
		true,             // readOnly (create with -ro flag)
		true,             // nfsLink (false to copy and respect -ro flag)
		h.debug,          // debug
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

	mappings, mapCheckErr := h.hmcClient.GetViosSCSIMappings(ctx, viosUUID, h.debug)
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
		_, err = h.hmcClient.DeleteVirtualOpticalMaps(ctx, sysUUID, viosUUID, node.UUID, mediaToUnmap, h.debug)
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

		_, err = h.hmcClient.CreateVirtualOpticalMaps(ctx, sysUUID, viosUUID, node.UUID, []string{mediaName}, h.debug)
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

	err = h.hmcClient.SaveCurrentLparConfig(ctx, node.UUID, profileName, true, h.debug)
	if err != nil {
		return fmt.Errorf("failed to save partition profile: %w", err)
	}

	// ========================================================================
	// STEP 9.5: SET BOOT STRING TO PRIORITIZE ISO
	// ========================================================================
	h.logger.Info("Setting Pending Boot String to 'cd/dvd-all'...")

	err = h.hmcClient.SetPartitionBootString(ctx, node.UUID, "cd/dvd-all", h.debug)
	if err != nil {
		h.logger.Warn("Failed to set boot string (may require manual SMS boot)", "error", err)
	} else {
		h.logger.Info("Boot string set to 'cd/dvd-all'")
	}

	// ========================================================================
	// STEP 10: GET PROFILE UUID AND POWER ON
	// ========================================================================
	lparDetails2, err2 := h.hmcClient.GetLogicalPartitionDetailed(ctx, node.UUID, h.debug)
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

	_, err = h.hmcClient.PowerOnPartition(ctx, node.UUID, powerOnOpts, h.debug)
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
	sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, systemName, h.debug)
	if err != nil {
		return "", "", err
	}

	viosList, err := h.hmcClient.GetVirtualIOServersQuick(ctx, sysUUID, h.debug)
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

	activeVIOSMap, err := h.hmcClient.GetActiveVIOSServers(ctx, sysUUID, viosUUIDs, h.debug)
	if err != nil {
		return "", "", err
	}

	for uuid, details := range activeVIOSMap {
		return uuid, details.PartitionName, nil
	}

	return "", "", fmt.Errorf("no active VIOS found on system %s", systemName)
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
		viosUsername, viosPassword, created, apiErr = h.hmcClient.EnsureVIOSAdminUser(ctx, h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug)
		if apiErr != nil || viosPassword == "" {
			h.logger.Warn("Failed to get viosadmin credentials via API, falling back to default", "error", apiErr)
			viosUsername, viosPassword = h.hmcClient.GetVIOSAdminCredentials()
		} else if created {
			h.logger.Info("Created viosadmin user for cleanup", "username", viosUsername)
		}
	}

	h.logger.Info("Cleaning up ISO mappings", "count", len(h.isoMappings))

	for _, mapping := range h.isoMappings {
		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(ctx, mapping.SystemName, h.debug)
		if err != nil {
			h.logger.Warn("Failed to get system UUID for cleanup", "system", mapping.SystemName, "error", err)
			continue
		}

		h.logger.Info("Unmapping optical media", "node", mapping.NodeName, "media", mapping.MediaName)

		_, err = h.hmcClient.DeleteVirtualOpticalMaps(
			ctx,
			sysUUID,
			mapping.VIOSUUID,
			mapping.LparUUID,
			[]string{mapping.MediaName},
			h.debug)
		if err != nil {
			h.logger.Error("Failed to unmap optical media", "media", mapping.MediaName, "error", err)
		} else {
			h.logger.Info("Successfully unmapped optical media", "media", mapping.MediaName)

			h.logger.Info("Saving LPAR profile after unmapping", "node", mapping.NodeName)
			profileName := "default_profile"
			err = h.hmcClient.SaveCurrentLparConfig(ctx, mapping.LparUUID, profileName, true, h.debug)
			if err != nil {
				h.logger.Warn("Failed to save LPAR profile after unmapping", "node", mapping.NodeName, "profile", profileName, "error", err)
			} else {
				h.logger.Info("Successfully saved LPAR profile", "node", mapping.NodeName, "profile", profileName)
			}
		}
	}

	// ========================================================================
	// 1. DELETE VIRTUAL OPTICAL MEDIA (Per-Node Media)
	// ========================================================================
	for _, mapping := range h.isoMappings {
		h.logger.Info("Checking VIOS repository for optical media...", "media", mapping.MediaName, "vios", mapping.VIOSName)

		_, err := h.hmcClient.GetVirtualOpticalMedia(ctx, mapping.SystemName, mapping.VIOSName, mapping.MediaName, h.debug)

		if err == nil {
			h.logger.Info("Media found in repository. Deleting...", "media", mapping.MediaName)
			delErr := h.hmcClient.DeleteVirtualOpticalMedia(
				ctx,
				mapping.SystemName,
				mapping.VIOSName,
				mapping.MediaName,
				h.debug)

			if delErr != nil {
				h.logger.Warn("Failed to delete optical media", "media", mapping.MediaName, "error", delErr)
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

		_, err := hmc.UnmountNFS(ctx, h.hmcClient, mapping.SystemName, mapping.VIOSName, mapping.MountPoint, h.debug)

		if err != nil && (strings.Contains(err.Error(), "Could not find anything to unmount") || strings.Contains(err.Error(), "not mounted")) {
			h.logger.Info("Directory is already unmounted from VIOS", "mount_point", mapping.MountPoint)
		} else if err != nil {
			h.logger.Error("Failed to unmount NFS", "mount_point", mapping.MountPoint, "error", err)
		} else {
			h.logger.Info("Successfully unmounted NFS from VIOS", "mount_point", mapping.MountPoint)
		}

		h.logger.Info("Removing mount directory from VIOS", "mount_point", mapping.MountPoint)
		rmdirCmd := fmt.Sprintf(`viosvrcmd -m %s -p %s -c "rmdir %s" --admin`, mapping.SystemName, mapping.VIOSName, mapping.MountPoint)
		_, err = hmc.CliRunnerViaSsh(h.cfg.HMC.IP, viosUsername, viosPassword, rmdirCmd, h.debug)

		if err != nil && (strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "not found")) {
			h.logger.Info("Mount directory already removed", "mount_point", mapping.MountPoint)
		} else if err != nil {
			h.logger.Warn("Failed to remove mount directory", "mount_point", mapping.MountPoint, "error", err)
		} else {
			h.logger.Info("Successfully removed mount directory", "mount_point", mapping.MountPoint)
		}
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
	repoInfo, err := h.hmcClient.GetMediaRepositoryInfo(ctx, systemName, viosName, h.debug)
	
	// FIX: The HMC API can return success (err == nil) but SizeMB = 0 if the repository isn't created,
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
	vgs, vgErr := h.hmcClient.GetVolumeGroups(ctx, viosUUID, h.debug)
	if vgErr != nil {
		return fmt.Errorf("failed to list volume groups: %w", vgErr)
	}

	var targetVG string
	for _, vg := range vgs {
		if strings.ToLower(vg.GroupName) == "rootvg" { continue }
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
	if createErr := h.hmcClient.CreateMediaRepository(ctx, systemName, viosUUID, viosName, targetVG, requiredMB, h.debug); createErr != nil {
		// FIX: If repository already exists, that's actually OK - just log and continue
		if strings.Contains(createErr.Error(), "already exists") {
			h.logger.Info("Media Repository already exists (detected during creation attempt)", "vios", viosName)
			return nil
		}
		return fmt.Errorf("failed to create media repository: %w", createErr)
	}

	h.logger.Info("Media Repository created successfully")
	return nil
}