package controller

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
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
func (nm *NetworkManager) AddVIPAlias(ctx context.Context,iface, ip, cidr string) error {
	prefix := ExtractCIDRPrefix(cidr)
	nm.logger.Info("Appending persistent VIP via nmcli", "ip", ip, "interface", iface)

	fullIP := fmt.Sprintf("%s/%s", ip, prefix)

	// 1. Discover the connection name managing the interface (e.g., "eth0" or "Wired connection 1")
	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s | head -n1 | cut -d: -f2", iface)
	conName, err := nm.executor.Execute(ctx,getConCmd)
	conName = strings.TrimSpace(conName)
	if err != nil || conName == "" {
		return fmt.Errorf("failed to discover NetworkManager connection for interface %s: %v", iface, err)
	}

	// 2. Append the VIP to the existing profile
	// The '+' prefix ensures we add the IP without overwriting existing ones.
	addCmd := fmt.Sprintf("sudo nmcli connection modify \"%s\" +ipv4.addresses %s", conName, fullIP)
	if _, err := nm.executor.Execute(ctx,addCmd); err != nil {
		return fmt.Errorf("failed to modify connection profile: %v", err)
	}

	// 3. Apply changes without disrupting the connection
	reapplyCmd := fmt.Sprintf("sudo nmcli device reapply %s", iface)
	if _, err := nm.executor.Execute(ctx,reapplyCmd); err != nil {
		nm.logger.Warn("Device reapply failed, falling back to connection up", "error", err)
		upCmd := fmt.Sprintf("sudo nmcli connection up \"%s\"", conName)
		_, _ = nm.executor.Execute(ctx,upCmd)
	}

	return nil
}

// RemoveVIPAlias prunes only the cluster VIP from the primary connection
func (nm *NetworkManager) RemoveVIPAlias(ctx context.Context,iface, ip, cidr, controllerIP string) error {
	// --- CRITICAL SAFETY CHECK ---
	// Prevent the orchestrator from pruning the controller's own primary IP
	if ip == controllerIP {
		nm.logger.Warn("SAFETY ABORT: Refusing to remove primary management IP", "ip", ip)
		return nil
	}

	prefix := ExtractCIDRPrefix(cidr)
	fullIP := fmt.Sprintf("%s/%s", ip, prefix)

	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s | head -n1 | cut -d: -f2", iface)
	conName, _ := nm.executor.Execute(ctx,getConCmd)
	conName = strings.TrimSpace(conName)

	if conName != "" {
		nm.logger.Info("Pruning persistent VIP from connection", "ip", fullIP, "connection", conName)
		
		// 1. Remove ONLY the specific VIP from the profile
		delCmd := fmt.Sprintf("sudo nmcli connection modify \"%s\" -ipv4.addresses %s", conName, fullIP)
		_, _ = nm.executor.Execute(ctx,delCmd)
		
		// 2. USE REAPPLY INSTEAD OF UP
		reapplyCmd := fmt.Sprintf("sudo nmcli device reapply %s", iface)
		if _, err := nm.executor.Execute(ctx,reapplyCmd); err != nil {
			nm.logger.Debug("Reapply failed or unsupported, falling back to connection up...")
			upCmd := fmt.Sprintf("sudo nmcli connection up \"%s\"", conName)
			_, _ = nm.executor.Execute(ctx,upCmd)
		}
	}
	return nil
}

// CheckVIPExists checks if a VIP is already configured on an interface
func (nm *NetworkManager) CheckVIPExists(ctx context.Context,iface, ip string) (bool, error) {
	checkCmd := fmt.Sprintf("sudo ip addr show dev %s | grep -q '%s/'", iface, ip)
	_, err := nm.executor.Execute(ctx, checkCmd)
	// grep returns non-zero (exit status 1) if not found, which localexec surfaces as an error
	return err == nil, nil
}

// VerifyVIPPersistence checks if the VIP is defined in the connection profile
func (nm *NetworkManager) VerifyVIPPersistence(ctx context.Context,iface, ip string) (bool, error) {
	getConCmd := fmt.Sprintf("nmcli -t -f GENERAL.CONNECTION device show %s | head -n1 | cut -d: -f2", iface)
	conName, _ := nm.executor.Execute(ctx, getConCmd)
	conName = strings.TrimSpace(conName)

	if conName == "" {
		return false, nil
	}

	checkCmd := fmt.Sprintf("nmcli -g ipv4.addresses connection show \"%s\" | grep -q '%s/'", conName, ip)
	_, err := nm.executor.Execute(ctx,checkCmd)
	
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