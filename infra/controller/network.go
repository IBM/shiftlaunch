package controller

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
)

// ExtractCIDRPrefix extracts the prefix from a CIDR notation (e.g., "192.0.2.0/20" -> "20")
func ExtractCIDRPrefix(cidr string) string {
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return "24" // Safe fallback
	}
	return parts[1]
}

// NetworkManager handles local network configuration for the Controller node
type NetworkManager struct {
	executor *localexec.LocalClient
	debug    bool
	logger   *logger.Logger
}

// NewNetworkManager creates a new local NetworkManager instance
func NewNetworkManager(executor *localexec.LocalClient, debug bool, log *logger.Logger) *NetworkManager {
	return &NetworkManager{
		executor: executor,
		debug:    debug,
		logger:   log,
	}
}

// AddVIPAlias appends a secondary IP to the EXISTING connection profile via nmcli
func (nm *NetworkManager) AddVIPAlias(ctx context.Context, iface, ip, cidr string) error {
	// Shield from cancellation so the VIP doesn't get orphaned in the NM profile without being activated!
	shieldedCtx := context.WithoutCancel(ctx)

	prefix := ExtractCIDRPrefix(cidr)
	nm.logger.Info("Appending persistent VIP via nmcli", "ip", ip, "interface", iface)

	fullIP := fmt.Sprintf("%s/%s", ip, prefix)

	// 1. Discover the connection name managing the interface (e.g., "eth0" or "Wired connection 1")
	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s | head -n1 | cut -d: -f2", iface)
	conName, err := nm.executor.Execute(shieldedCtx, getConCmd)
	conName = strings.TrimSpace(conName)
	if err != nil || conName == "" {
		return fmt.Errorf("failed to discover NetworkManager connection for interface %s: %v", iface, err)
	}

	// 2. Append the VIP to the existing profile
	// The '+' prefix ensures we add the IP without overwriting existing ones.
	addCmd := fmt.Sprintf("sudo nmcli connection modify \"%s\" +ipv4.addresses %s", conName, fullIP)
	if _, err := nm.executor.Execute(shieldedCtx, addCmd); err != nil {
		return fmt.Errorf("failed to modify connection profile: %v", err)
	}

	// 3. Apply changes without disrupting the connection
	reapplyCmd := fmt.Sprintf("sudo nmcli device reapply %s", iface)
	if _, err := nm.executor.Execute(shieldedCtx, reapplyCmd); err != nil {
		nm.logger.Warn("Device reapply failed, falling back to connection up", "error", err)
		upCmd := fmt.Sprintf("sudo nmcli connection up \"%s\"", conName)
		_, _ = nm.executor.Execute(shieldedCtx, upCmd)
	}

	return nil
}

// RemoveVIPAlias dynamically identifies and removes a specific VIP alias from the interface
// while leaving all other active cluster VIPs and the primary IP completely intact.
func (nm *NetworkManager) RemoveVIPAlias(ctx context.Context, iface, ip, cidr, controllerIP string) error {
	nm.logger.Info("Starting dynamic VIP cleanup using interface analysis", "interface", iface)

	// 1. Get the "Base IP" securely without dialing the internet (Airgap Safe)
	baseIPCmd := fmt.Sprintf("ip -4 addr show dev %s | grep -oP '(?<=inet\\s)\\d+(\\.\\d+){3}' | head -1", iface)
	baseIPOut, err := nm.executor.Execute(ctx, baseIPCmd)
	if err != nil {
		return fmt.Errorf("failed to determine primary interface IP: %v", err)
	}
	baseIP := strings.TrimSpace(baseIPOut)
	if baseIP == "" {
		return fmt.Errorf("kernel returned an empty primary IP for interface %s", iface)
	}

	nm.logger.Debug("Interface IP Analysis", "BaseIP", baseIP)

	// Prevent the orchestrator from pruning the controller's own primary IP
	if ip == baseIP {
		nm.logger.Warn("SAFETY ABORT: The requested VIP matches the kernel's Base IP. Refusing to remove.", "ip", ip)
		return nil
	}

	// 2. Discover the NetworkManager connection profile managing this interface
	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s 2>/dev/null | grep -v -i 'warning' | head -n1 | cut -d: -f2", iface)
	conNameOut, _ := nm.executor.Execute(ctx, getConCmd)
	conName := strings.TrimSpace(conNameOut)

	if conName == "" {
		nm.logger.Warn("Could not determine NetworkManager connection profile", "interface", iface)
		return nil
	}

	// 3. Get ALL IPv4 addresses currently configured on the interface.
	// grep -oP is used instead of awk column-splitting to be robust against
	// secondary interface labels or extra routing flags that would shift columns.
	getAllCmd := fmt.Sprintf("ip -o -4 addr show dev %s | grep -oP '\\d+\\.\\d+\\.\\d+\\.\\d+/\\d+'", iface)
	allIPsOut, err := nm.executor.Execute(ctx, getAllCmd)
	if err != nil {
		return fmt.Errorf("failed to retrieve IP addresses for interface %s: %v", iface, err)
	}

	// 4. The Targeted Method: Destroy ONLY the specific cluster VIP requested
	allIPs := strings.Split(strings.TrimSpace(allIPsOut), "\n")
	vipsRemoved := 0

	for _, ipWithCidr := range allIPs {
		ipWithCidr = strings.TrimSpace(ipWithCidr)
		if ipWithCidr == "" {
			continue
		}

		// Split the IP from its subnet mask (e.g., "10.20.181.100/24" -> "10.20.181.100")
		parts := strings.Split(ipWithCidr, "/")
		currentIP := parts[0]

		// STRICT MATCH: Only remove if the IP matches our cluster's specific VIP
		if currentIP == ip {
			nm.logger.Info("Targeting VIP for destruction", "vip", currentIP)

			// Remove the VIP from the NetworkManager profile to ensure it doesn't return on reboot
			delCmd := fmt.Sprintf("sudo nmcli connection modify \"%s\" -ipv4.addresses %s", conName, ipWithCidr)
			_, _ = nm.executor.Execute(ctx, delCmd)

			vipsRemoved++
		}
	}

	// 5. Apply the changes immediately without disrupting the connection
	if vipsRemoved > 0 {
		nm.logger.Info("Reapplying connection profile to flush destroyed VIPs", "connection", conName)
		reapplyCmd := fmt.Sprintf("sudo nmcli device reapply %s", iface)
		if _, err := nm.executor.Execute(ctx, reapplyCmd); err != nil {
			nm.logger.Debug("Reapply failed or unsupported, falling back to connection up...")
			upCmd := fmt.Sprintf("sudo nmcli connection up \"%s\"", conName)
			_, _ = nm.executor.Execute(ctx, upCmd)
		}
	} else {
		nm.logger.Info("Requested VIP not found on interface. Cleanup complete.")
	}

	return nil
}

// CheckVIPExists checks if a VIP is already configured on an interface
func (nm *NetworkManager) CheckVIPExists(ctx context.Context, iface, ip string) (bool, error) {
	checkCmd := fmt.Sprintf("sudo ip addr show dev %s | grep -q '%s/'", iface, ip)
	_, err := nm.executor.Execute(ctx, checkCmd)
	// grep returns non-zero (exit status 1) if not found, which localexec surfaces as an error
	return err == nil, nil
}

// VerifyVIPPersistence checks if the VIP is defined in the connection profile
func (nm *NetworkManager) VerifyVIPPersistence(ctx context.Context, iface, ip string) (bool, error) {
	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s | head -n1 | cut -d: -f2", iface)
	conName, _ := nm.executor.Execute(ctx, getConCmd)
	conName = strings.TrimSpace(conName)

	if conName == "" {
		return false, nil
	}

	checkCmd := fmt.Sprintf("nmcli -g ipv4.addresses connection show \"%s\" | grep -q '%s/'", conName, ip)
	_, err := nm.executor.Execute(ctx, checkCmd)

	return err == nil, nil
}

// GetInterfaceIPv4 discovers the primary IPv4 address of a given network interface (e.g., "eth0")
func GetInterfaceIPv4(ifaceName string) (string, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return "", fmt.Errorf("network interface '%s' not found: %w", ifaceName, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", fmt.Errorf("failed to get addresses for interface '%s': %w", ifaceName, err)
	}

	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}

		if ip == nil || ip.IsLoopback() {
			continue
		}

		// Ensure it is an IPv4 address
		ip = ip.To4()
		if ip != nil {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("no valid IPv4 address found on interface '%s'", ifaceName)
}

// AddHostsEntry appends the cluster API endpoints to the controller's /etc/hosts file
// so the local openshift-install binary can resolve them without modifying the system DNS.
func (nm *NetworkManager) AddHostsEntry(ctx context.Context, clusterName, baseDomain, vip string) error {
	nm.logger.Debug("Appending cluster API endpoints to local /etc/hosts")

	domain := fmt.Sprintf("%s.%s", clusterName, baseDomain)

	// The installer strictly needs api and api-int. We add console and oauth for convenience.
	entry := fmt.Sprintf("%s api.%s api-int.%s console-openshift-console.apps.%s oauth-openshift.apps.%s",
		vip, domain, domain, domain, domain)
	marker := fmt.Sprintf("# ShiftLaunch-Cluster-API: %s", clusterName)

	// Clean up any stale entries first (this is internally shielded now)
	nm.RemoveHostsEntry(ctx, clusterName)

	// Shield from cancellation! Writing to /etc/hosts must never be aborted.
	shieldedCtx := context.WithoutCancel(ctx)

	// Append the new entry
	addCmd := fmt.Sprintf("echo '%s %s' | sudo tee -a /etc/hosts > /dev/null", entry, marker)
	_, err := nm.executor.Execute(shieldedCtx, addCmd)
	return err
}

// RemoveHostsEntry safely removes the cluster's specific API endpoints from the hosts file
func (nm *NetworkManager) RemoveHostsEntry(ctx context.Context, clusterName string) error {
	// Shield from cancellation! Killing sed -i mid-execution will
	// permanently destroy the OS /etc/hosts file!
	shieldedCtx := context.WithoutCancel(ctx)

	marker := fmt.Sprintf("# ShiftLaunch-Cluster-API: %s", clusterName)
	// STRICT MATCH FIX: Add the '$' anchor so 'my-cluster' doesn't delete 'my-cluster1'
	delCmd := fmt.Sprintf("sudo sed -i '/%s$/d' /etc/hosts", marker)
	_, err := nm.executor.Execute(shieldedCtx, delCmd)
	return err
}
