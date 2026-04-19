package compute

import (
	"context"
	"fmt"
	"strings"
	"time"

	hmc "github.com/sudeeshjohn/powerhmc-go"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

type HMCProvider struct {
	cfg       *types.AgentConfig
	hmcClient *hmc.HmcRestClient
	logger    *logger.Logger
	debug     bool
}

// GetHMCClient returns the underlying HMC REST client for external use (e.g., validation)
func (h *HMCProvider) GetHMCClient() *hmc.HmcRestClient {
	return h.hmcClient
}

// DiscoverMetadata loops through your nodes and queries the HMC for network adapter details
func (h *HMCProvider) DiscoverMetadata(ctx context.Context) error {
	h.logger.Info("🔍 Discovering LPAR metadata from HMC...")

	for _, node := range h.cfg.GetAllNodes() {
		h.logger.Debug("Querying", "system", node.SystemName, "lpar", node.ExistingLPARName)

		// 1. Get System UUID
		sysUUID, _, err := h.hmcClient.GetManagedSystemByName(node.SystemName, true)
		if err != nil {
			return fmt.Errorf("failed to find system %s: %w", node.SystemName, err)
		}

		// 2. Get LPAR UUID
		// Pass false for debug to avoid logging all LPARs on the system
		lpars, err := h.hmcClient.GetLogicalPartitionsQuickAll(sysUUID, false)
		if err != nil { return err }
		
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
		profiles, err := h.hmcClient.GetLogicalPartitionProfiles(node.UUID, false)
		if err != nil || len(profiles) == 0 {
			return fmt.Errorf("no profile found for LPAR %s. A profile is required for network boot", node.ExistingLPARName)
		}
		node.ProfileUUID = profiles[0].UUID

		// 4. Get MAC and Location Code for DHCP/PXE
		// Pass false for debug to avoid excessive logging
		adapters, err := h.hmcClient.GetClientNetworkAdapters(sysUUID, node.UUID, false)
		if err != nil || len(adapters) == 0 {
			return fmt.Errorf("no network adapter found on LPAR %s", node.ExistingLPARName)
		}

		node.MACAddress = hmc.FormatMACAddress(adapters[0].MACAddress)
		node.LocationCode = adapters[0].LocationCode

		h.logger.Info("✓ Discovered", "lpar", node.ExistingLPARName, "mac", node.MACAddress, "uuid", node.UUID)
	}
	return nil
}

// BootNode executes the lpar_netboot command via REST API for a single node
func (h *HMCProvider) BootNode(ctx context.Context, node *types.NodeConfig) error {
	h.logger.Info("Processing LPAR for netboot", "lpar", node.ExistingLPARName)

	var lparDetailed *hmc.LogicalPartitionDetailed
	var err error
	maxRetries := 3
	
	// Retry loop for 406/Intermittent HMC errors with re-authentication
	for i := 0; i < maxRetries; i++ {
		lparDetailed, err = h.hmcClient.GetLogicalPartitionDetailed(node.UUID, true)
		if err == nil {
			break
		}
		
		// Check if error is 406 (session issue) and re-authenticate
		if strings.Contains(err.Error(), "406") && i < maxRetries-1 {
			h.logger.Warn(fmt.Sprintf("HMC returned 406 error (attempt %d/%d). Logging out and re-authenticating...", i+1, maxRetries), "error", err)
			
			// Logout from old session first
			if logoutErr := h.hmcClient.Logoff(); logoutErr != nil {
				h.logger.Debug("Logout failed (session may already be invalid)", "error", logoutErr)
			}
			
			// Wait a moment for HMC to clean up
			time.Sleep(2 * time.Second)
			
			// Re-authenticate with fresh session
			if loginErr := h.hmcClient.Login(h.cfg.HMC.Username, h.cfg.HMC.Password, h.debug); loginErr != nil {
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
		h.logger.Info("⚠️  LPAR is active. Powering off before network boot...", "state", lparDetailed.PartitionState)

		h.logger.Info("Closing virtual terminal...")
		_ = h.hmcClient.CloseVirtualTerminalViaSsh(
			h.cfg.HMC.IP,
			h.cfg.HMC.Username,
			h.cfg.HMC.Password,
			node.SystemName,
			node.ExistingLPARName,
			true,
		)

		_, err = h.hmcClient.PowerOffPartition(node.UUID, "Immediate", false, true)
		if err != nil {
			return fmt.Errorf("failed to power off LPAR: %w", err)
		}
		h.logger.Info("✓ LPAR powered off successfully")
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
	_, err = h.hmcClient.PowerOnPartition(node.UUID, &hmc.PowerOnOptions{
		ProfileUUID: profileUUID,
		BootMode:    "of", // Boot to Open Firmware
	}, true)
	if err != nil {
		return fmt.Errorf("failed to power on LPAR for adapter registration: %w", err)
	}

	h.logger.Info("⏳ Waiting 20 seconds for LPAR to reach Open Firmware and register adapters...")
	time.Sleep(20 * time.Second)

	h.logger.Info("Powering off LPAR for profile query...")
	_, err = h.hmcClient.PowerOffPartition(node.UUID, "Immediate", false, true)
	if err != nil {
		return fmt.Errorf("failed to power off LPAR: %w", err)
	}

	h.logger.Info("⏳ Waiting 10 seconds for LPAR to fully power off...")
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

	// --- FIX: Corrected the struct type here ---
	var bootDevices []hmc.NetworkBootDevice
	for i := 0; i < maxRetries; i++ {
		bootDevices, err = h.hmcClient.GetNetworkBootDevices(node.UUID, profileUUID, true)
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
	h.logger.Info("✓ Authoritative location code found", "location", authoritativeLocationCode)

	// =========================================================================
	// STEP 3: Network boot using authoritative location code
	// =========================================================================
	h.logger.Info("⏳ Waiting 5 seconds for LPAR to initialize before initiating netboot...")
	time.Sleep(5 * time.Second)

	// --- CRITICAL FIX: Aggressively drop the terminal session before netboot ---
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

	status, err := h.hmcClient.PowerOnPartition(node.UUID, options, true)
	if err != nil {
		return fmt.Errorf("failed to execute network boot: %w", err)
	}

	h.logger.Info("✓ Network boot initiated successfully", "lpar", node.ExistingLPARName, "status", status)

	h.logger.Info("Saving profile to persist configuration...")
	_ = h.hmcClient.SaveCurrentLparConfig(node.UUID, "default_profile", true, true)

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
		_, err := h.hmcClient.PowerOffPartition(node.UUID, "Immediate", false, true)
		if err != nil {
			h.logger.Warn("LPAR power off returned an error (may already be off)", "lpar", node.ExistingLPARName, "error", err)
		} else {
			h.logger.Info("✓ Power off signal accepted", "lpar", node.ExistingLPARName)
			powerOffCount++
		}
	}
	
	h.logger.Info("Power off complete", "powered_off", powerOffCount, "skipped", skippedCount, "total", len(nodes))
	return nil
}

// Cleanup logs out from HMC and cleans up resources
func (h *HMCProvider) Cleanup() error {
	h.logger.Debug("Logging out from HMC session...")
	if err := h.hmcClient.Logoff(); err != nil {
		h.logger.Debug("HMC logout failed (session may already be invalid)", "error", err)
		return err
	}
	h.logger.Debug("Successfully logged out from HMC")
	return nil
}