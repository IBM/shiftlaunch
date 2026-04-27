package validation

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	hmc "github.com/sudeeshjohn/powerhmc-go"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/types"
	"go.yaml.in/yaml/v3"
)

// ============================================================================
// VALIDATOR STRUCT AND INITIALIZATION
// ============================================================================

// Validator validates configuration in three phases:
// 1. Config Validation (static, fast) - validates YAML structure and values
// 2. Controller Node Validation (Local) - validates local infrastructure
// 3. HMC Validation (HMC API) - validates Power systems and BYOI LPARs
type Validator struct {
	cfg          *types.AgentConfig
	exec         *localexec.LocalClient
	hmcClient    *hmc.HmcRestClient
	debug        bool
	errors       []string
	warnings     []string
	log          *logger.Logger
	workspaceDir string
}

// NewValidator creates a new validator
func NewValidator(cfg *types.AgentConfig, exec *localexec.LocalClient, debug bool) *Validator {
	fallbackLog, _ := logger.New(debug, "/dev/null")
	
	// Get workspace directory from environment or use default
	workspaceDir := os.Getenv("SHIFTLAUNCH_WORKSPACE")
	if workspaceDir == "" {
		workspaceDir = "/opt/shiftlaunch/clusters"
	}

	return &Validator{
		cfg:          cfg,
		exec:         exec,
		debug:        debug,
		errors:       []string{},
		warnings:     []string{},
		log:          fallbackLog,
		workspaceDir: workspaceDir,
	}
}

// SetHMCClient sets the HMC client for active validation
func (v *Validator) SetHMCClient(client *hmc.HmcRestClient) {
	v.hmcClient = client
}

// SetLogger sets the custom logger for deployment context
func (v *Validator) SetLogger(l *logger.Logger) {
	if l != nil {
		v.log = l
	}
}

// Validate performs comprehensive validation in phases
func (v *Validator) Validate(ctx context.Context) error {
	// Phase 1: Config Validation (static, fast)
	v.log.Info("Phase 1: Validating configuration...")

	v.validateController()
	v.validateHMC()
	v.validateNetwork(ctx)
	v.validateOpenShift()
	v.validateNodes()

	v.log.Info("✓ Configuration valid")

	// Phase 2: Local Controller Environment Validation
	if v.exec != nil {
		v.log.Info("Phase 2: Validating local controller environment...")
		v.validateLocalEnvironment(ctx)
		v.log.Info("✓ Local environment validated")
	}

	// Phase 3: HMC Validation (HMC API-based)
	if v.hmcClient != nil {
		v.log.Info("Phase 3: Validating pre-provisioned LPARs (BYOI mode)...")
		v.validateBYOILPARs()
		if v.cfg.Nodes.BootMethod == "iso" {
			v.validateMediaRepositorySpace()
		}
		v.log.Info("✓ HMC infrastructure validated")
	}

	// Phase 4: External Services Validation
	if v.exec != nil {
		hasExternalServices := !v.cfg.ManagedServices.DNS || !v.cfg.ManagedServices.DHCP || !v.cfg.ManagedServices.PXE || !v.cfg.ManagedServices.LoadBalancer

		if hasExternalServices {
			v.log.Info("Phase 4: Validating external services (BYOI mode)...")
			v.validateExternalServices(ctx)
			v.log.Info("✓ External services validated")
		}
	}

	// Print warnings
	if len(v.warnings) > 0 {
		v.log.Warn("Validation completed with warnings")
		for _, w := range v.warnings {
			v.log.Warn(fmt.Sprintf(" - %s", w))
		}
	}

	// Return errors if any
	if len(v.errors) > 0 {
		errMsg := "\n❌ Validation Errors:\n"
		for _, e := range v.errors {
			errMsg += fmt.Sprintf("   - %s\n", e)
		}
		return fmt.Errorf("%s", errMsg)
	}

	return nil
}

// ============================================================================
// PHASE 1: CONFIG VALIDATION (Static, Fast)
// ============================================================================

// validateController validates the local controller node configuration
func (v *Validator) validateController() {
	c := v.cfg.Controller

	if c.NetworkInterface == "" {
		v.errors = append(v.errors, "controller.network_interface is required")
	}

	vip := v.cfg.Network.LoadBalancerIP
	if vip == "" {
		v.errors = append(v.errors, "network.loadbalancer_ip is missing")
	} else if !v.isValidIP(vip) {
		v.errors = append(v.errors, fmt.Sprintf("VIP '%s' is not a valid IP", vip))
	}
}

// validateHMC validates HMC configuration
func (v *Validator) validateHMC() {
	h := v.cfg.HMC

	if h.IP == "" {
		v.errors = append(v.errors, "hmc.ip is required")
	} else if !v.isValidIP(h.IP) && !v.isValidHostname(h.IP) {
		v.errors = append(v.errors, fmt.Sprintf("hmc.ip '%s' must be a valid IP address or hostname", h.IP))
	}

	if h.Username == "" {
		v.errors = append(v.errors, "hmc.username is required")
	}

	if h.Password == "" {
		v.errors = append(v.errors, "hmc.password is required")
	}
}

// validateNetwork validates network configuration
func (v *Validator) validateNetwork(ctx context.Context) {
	n := v.cfg.Network

	if v.cfg.OpenShift.BaseDomain == "" {
		v.errors = append(v.errors, "openshift.base_domain is required")
	}

	if n.MachineCIDR == "" {
		v.errors = append(v.errors, "network.machine_network_cidr is required")
	} else if !v.isValidCIDR(n.MachineCIDR) {
		v.errors = append(v.errors, fmt.Sprintf("network.machine_network_cidr '%s' is not a valid CIDR", n.MachineCIDR))
	}

	if n.Gateway == "" {
		v.errors = append(v.errors, "network.gateway is required")
	} else if !v.isValidIP(n.Gateway) {
		v.errors = append(v.errors, fmt.Sprintf("network.gateway '%s' is not a valid IP address", n.Gateway))
	}

	// Validate DNS forwarders are provided to prevent DNS resolution loops
	if len(n.DNSForwarders) == 0 {
		v.errors = append(v.errors, "network.dns_forwarders is required to prevent DNS resolution loops and ensure internet connectivity")
	}
	
	// Validate VIP is not already configured on the controller (conflict detection)
	if v.cfg.ManagedServices.LoadBalancer && n.LoadBalancerIP != "" {
		v.validateVIPNotInUse(ctx,n.LoadBalancerIP)
	}
}

// validateVIPNotInUse checks if the VIP is already configured on the controller interface
// or being used by another managed cluster
func (v *Validator) validateVIPNotInUse(ctx context.Context,vip string) {
	iface := v.cfg.Controller.NetworkInterface
	if iface == "" {
		return // Can't check without interface name
	}
	
	// Check 1: Is VIP configured on the controller interface?
	output, err := v.exec.Execute(ctx,fmt.Sprintf("ip addr show %s", iface))
	if err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("Could not check if VIP %s is already in use on interface: %v", vip, err))
	} else if strings.Contains(output, vip+"/") {
		// VIP is configured - check if it belongs to another cluster
		conflictingCluster := v.findClusterUsingVIP(vip)
		if conflictingCluster != "" {
			v.errors = append(v.errors, fmt.Sprintf(
				"VIP %s is already in use by cluster '%s'. "+
				"Please choose a different loadbalancer_ip or delete the conflicting cluster first.",
				vip, conflictingCluster))
		} else {
			v.errors = append(v.errors, fmt.Sprintf(
				"VIP %s is already configured on interface %s but no managed cluster found using it. "+
				"Please remove the VIP alias manually or choose a different loadbalancer_ip.",
				vip, iface))
		}
		return
	}
	
	// Check 2: Is VIP defined in any other managed cluster's config?
	conflictingCluster := v.findClusterUsingVIP(vip)
	if conflictingCluster != "" {
		v.errors = append(v.errors, fmt.Sprintf(
			"VIP %s is already configured for cluster '%s'. "+
			"Please choose a different loadbalancer_ip.",
			vip, conflictingCluster))
	}
}

// findClusterUsingVIP searches all managed clusters to find if any is using the given VIP
func (v *Validator) findClusterUsingVIP(vip string) string {
	// List all directories in workspace
	entries, err := os.ReadDir(v.workspaceDir)
	if err != nil {
		return "" // Can't check, return empty
	}
	
	currentCluster := v.cfg.OpenShift.ClusterName
	
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
		managedMarker := filepath.Join(v.workspaceDir, clusterName, ".managed")
		failedMarker := filepath.Join(v.workspaceDir, clusterName, ".failed")
		
		if _, err1 := os.Stat(managedMarker); os.IsNotExist(err1) {
			if _, err2 := os.Stat(failedMarker); os.IsNotExist(err2) {
				continue // Not a managed or failed cluster
			}
		}
		
		// Check if cluster is deleted
		deletedMarker := filepath.Join(v.workspaceDir, clusterName, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			continue // Cluster is deleted, skip
		}
		
		// Read the cluster's config
		configPath := filepath.Join(v.workspaceDir, clusterName, "config.yaml")
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

// validateOpenShift validates OpenShift configuration
func (v *Validator) validateOpenShift() {
	o := v.cfg.OpenShift

	if o.Version == "" {
		v.errors = append(v.errors, "openshift.version is required")
	}

	if o.PullSecretFile == "" {
		v.errors = append(v.errors, "openshift.pull_secret_file is required")
	} else {
		if _, err := os.Stat(o.PullSecretFile); os.IsNotExist(err) {
			v.errors = append(v.errors, fmt.Sprintf("openshift.pull_secret_file '%s' does not exist locally", o.PullSecretFile))
		}
	}

	if o.SSHPublicKeyFile == "" {
		v.errors = append(v.errors, "openshift.ssh_public_key_file is required")
	}

	// Skip RHCOS validation for ISO boot (Agent installer downloads RHCOS automatically)
	if v.cfg.Nodes.BootMethod == "iso" {
		v.log.Info("Skipping RHCOS image validation for ISO boot (Agent installer downloads RHCOS automatically)")
		// Still validate OCP client config
		if o.OCPClientConfig.Client == "" {
			v.errors = append(v.errors, "openshift.ocp_client_config.ocp_client is required")
		}
		if o.OCPClientConfig.Installer == "" {
			v.errors = append(v.errors, "openshift.ocp_client_config.ocp_installer is required")
		}
		return // Skip RHCOS URL validation
	}

	// Validate RHCOS URLs for netboot
	if o.RHCOSImages.KernelURL == "" {
		v.errors = append(v.errors, "openshift.rhcos_images.kernel_url is required")
	}
	if o.RHCOSImages.InitramfsURL == "" {
		v.errors = append(v.errors, "openshift.rhcos_images.initramfs_url is required")
	}
	if o.RHCOSImages.RootfsURL == "" {
		v.errors = append(v.errors, "openshift.rhcos_images.rootfs_url is required")
	}

	v.validateChecksum(o.RHCOSImages.KernelCSUM, "openshift.rhcos_images.kernel_csum")
	v.validateChecksum(o.RHCOSImages.InitramfsCSUM, "openshift.rhcos_images.initramfs_csum")
	v.validateChecksum(o.RHCOSImages.RootfsCSUM, "openshift.rhcos_images.rootfs_csum")

	// Validate OCP client config
	if o.OCPClientConfig.Client == "" {
		v.errors = append(v.errors, "openshift.ocp_client_config.ocp_client is required")
	}
	if o.OCPClientConfig.Installer == "" {
		v.errors = append(v.errors, "openshift.ocp_client_config.ocp_installer is required")
	}
}

func (v *Validator) validateChecksum(checksum, fieldName string) {
	if checksum == "" {
		return // Empty is valid (optional field)
	}

	if len(checksum) != 64 {
		v.errors = append(v.errors, fmt.Sprintf("%s must be exactly 64 characters (got %d)", fieldName, len(checksum)))
		return
	}

	for _, c := range checksum {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			v.errors = append(v.errors, fmt.Sprintf("%s must contain only hexadecimal characters (0-9, a-f, A-F)", fieldName))
			return
		}
	}
}

// validateNodes validates node configuration
func (v *Validator) validateNodes() {
	if v.cfg.IsSNO() {
		v.validateSNONode()
	} else {
		v.validateMultiNodeCluster()
	}
}

// validateSNONode validates SNO node configuration
func (v *Validator) validateSNONode() {
	if len(v.cfg.Nodes.SNO) == 0 {
		v.errors = append(v.errors, "nodes.sno block is required for SNO deployment")
		return
	}

	sno := v.cfg.Nodes.SNO[0]

	if sno.IP == "" {
		v.errors = append(v.errors, "nodes.sno.ip is required")
	} else if !v.isValidIP(sno.IP) {
		v.errors = append(v.errors, fmt.Sprintf("nodes.sno.ip '%s' is not a valid IP address", sno.IP))
	}

	if sno.SystemName == "" {
		v.errors = append(v.errors, "nodes.sno.system_name is required")
	}

	if sno.ExistingLPARName == "" {
		v.errors = append(v.errors, "nodes.sno.existing_lpar_name is required in BYOI mode")
	}
}

// validateMultiNodeCluster validates multi-node cluster configuration
func (v *Validator) validateMultiNodeCluster() {
	if len(v.cfg.Nodes.Masters) == 0 {
		v.errors = append(v.errors, "nodes.masters is required for multi-node deployment")
		return
	}

	masterCount := len(v.cfg.Nodes.Masters)
	if masterCount < 3 {
		v.errors = append(v.errors, fmt.Sprintf("minimum 3 master nodes required, got %d", masterCount))
	}
	if masterCount%2 == 0 {
		v.errors = append(v.errors, fmt.Sprintf("master count must be odd for quorum, got %d", masterCount))
	}

	for i, master := range v.cfg.Nodes.Masters {
		if master.Hostname == "" {
			v.errors = append(v.errors, fmt.Sprintf("nodes.masters[%d].name is required", i))
		}
		if master.SystemName == "" {
			v.errors = append(v.errors, fmt.Sprintf("nodes.masters[%d].system_name is required", i))
		}
		if master.IP == "" {
			v.errors = append(v.errors, fmt.Sprintf("nodes.masters[%d].ip is required", i))
		} else if !v.isValidIP(master.IP) {
			v.errors = append(v.errors, fmt.Sprintf("nodes.masters[%d].ip '%s' is not a valid IP", i, master.IP))
		}
		if master.ExistingLPARName == "" {
			v.errors = append(v.errors, fmt.Sprintf("nodes.masters[%d].existing_lpar_name is required in BYOI mode", i))
		}
	}

	if len(v.cfg.Nodes.Bootstrap) > 0 {
		for i, bootstrap := range v.cfg.Nodes.Bootstrap {
			if bootstrap.Hostname == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.bootstrap[%d].name is required", i))
			}
			if bootstrap.SystemName == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.bootstrap[%d].system_name is required", i))
			}
			if bootstrap.IP == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.bootstrap[%d].ip is required", i))
			}
			if bootstrap.ExistingLPARName == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.bootstrap[%d].existing_lpar_name is required in BYOI mode", i))
			}
		}
	}

	if len(v.cfg.Nodes.Workers) > 0 {
		for i, worker := range v.cfg.Nodes.Workers {
			if worker.Hostname == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.workers[%d].name is required", i))
			}
			if worker.SystemName == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.workers[%d].system_name is required", i))
			}
			if worker.IP == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.workers[%d].ip is required", i))
			}
			if worker.ExistingLPARName == "" {
				v.errors = append(v.errors, fmt.Sprintf("nodes.workers[%d].existing_lpar_name is required in BYOI mode", i))
			}
		}
	}
}

// ============================================================================
// PHASE 2: LOCAL ENVIRONMENT VALIDATION (LocalExec-Based)
// ============================================================================

func (v *Validator) validateLocalEnvironment(ctx context.Context) {
	// 1. Check if SSH public key exists locally
	if v.cfg.OpenShift.SSHPublicKeyFile != "" {
		keyPath := os.ExpandEnv(v.cfg.OpenShift.SSHPublicKeyFile)
		checkCmd := fmt.Sprintf("test -f %s", keyPath)
		if _, err := v.exec.Execute(ctx,checkCmd); err != nil {
			v.errors = append(v.errors, fmt.Sprintf("SSH KEY MISSING: The public key file '%s' does not exist on the local system.", keyPath))
		}
	}

	// 2. Check for sufficient disk space locally (must have at least 10GB free)
	v.validateLocalDiskSpace(ctx)

	// 3. Check for Directory/Config Collisions
	clusterName := v.cfg.OpenShift.ClusterName
	httpDir := fmt.Sprintf("/var/www/html/%s", clusterName)
	dnsmasqPath := fmt.Sprintf("/etc/dnsmasq.d/*-%s-*.conf", clusterName)
	haproxyPath := fmt.Sprintf("/etc/haproxy/conf.d/99-%s.cfg", clusterName)

	checkCmd := fmt.Sprintf("if [ -d '%s' ] || ls %s 1> /dev/null 2>&1 || ls %s 1> /dev/null 2>&1; then echo 'exists'; else echo 'missing'; fi",
		httpDir, dnsmasqPath, haproxyPath)

	if out, err := v.exec.Execute(ctx,checkCmd); err == nil && strings.TrimSpace(out) == "exists" {
		v.errors = append(v.errors, fmt.Sprintf("CLUSTER COLLISION: Artifacts for '%s' already exist locally. Run the 'delete' command to clean them up first to prevent accidental overwrites.", clusterName))
	}

	// 4. Check for VIP Conflicts on the network
	vip := v.cfg.Network.LoadBalancerIP
	if vip != "" {
		iface := v.cfg.Controller.NetworkInterface
		
		// Guardrail: Prevent hijacking the controller's primary IP
		ipCmd := fmt.Sprintf("ip -4 addr show dev %s | grep -oP '(?<=inet\\s)\\d+(\\.\\d+){3}' | head -1", iface)
		if hostIP, err := v.exec.Execute(ctx,ipCmd); err == nil && strings.TrimSpace(hostIP) == vip {
			v.errors = append(v.errors, fmt.Sprintf(
				"VIP conflict: VIP '%s' cannot be the same as the controller's primary IP on %s.", vip, iface))
		}

		checkBoundCmd := fmt.Sprintf("ip addr show dev %s | grep -q '%s/'", iface, vip)
		if _, err := v.exec.Execute(ctx,checkBoundCmd); err != nil {
			// Not bound to us. Check if it's in use on the network.
			if v.cfg.ManagedServices.LoadBalancer { // Only ping if we expect to manage it
				pingCmd := fmt.Sprintf("ping -c 2 -W 2 %s", vip)
				if _, pingErr := v.exec.Execute(ctx,pingCmd); pingErr == nil {
					v.errors = append(v.errors, fmt.Sprintf("IP CONFLICT: The VIP %s is already actively responding on the network. Please choose an unused IP.", vip))
				}
			}
		}
	}
}

func (v *Validator) validateLocalDiskSpace(ctx context.Context) {
	// On a fresh OS, httpd isn't installed yet, so /var/www/html won't exist.
	// We create it silently here so the 'df' command doesn't crash.
	v.exec.Execute(ctx, "sudo mkdir -p /var/www/html")
	dfCmd := "df -BK --output=avail /var/www/html | tail -n 1 | tr -d 'K'"
	output, err := v.exec.Execute(ctx,dfCmd)
	if err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("Unable to check disk space locally: %v", err))
		return
	}

	var availableKB int
	trimmedOutput := strings.TrimSpace(output)
	if _, err := fmt.Sscanf(trimmedOutput, "%d", &availableKB); err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("Unable to parse disk space output '%s': %v", trimmedOutput, err))
		return
	}

	availableGB := float64(availableKB) / (1024 * 1024)
	requiredGB := 10.0

	if availableGB < requiredGB {
		v.errors = append(v.errors,
			fmt.Sprintf("INSUFFICIENT DISK SPACE: /var/www/html has only %.2f GB available, but at least %.0f GB is required.",
				availableGB, requiredGB))
	} else {
		v.log.Info(fmt.Sprintf("  ✓ Controller has %.2f GB available in /var/www/html", availableGB))
	}
}

// ============================================================================
// PHASE 3: HMC VALIDATION (HMC API-Based)
// ============================================================================

// validateBYOILPARs validates that all specified LPARs exist in BYOI mode
func (v *Validator) validateBYOILPARs() {
	v.log.Info("    Validating pre-provisioned LPAR existence...")

	allNodes := v.cfg.GetAllNodes()
	systemLPARCache := make(map[string]map[string]*hmc.LogicalPartitionQuick)

	validatedCount := 0
	for _, node := range allNodes {
		if node.ExistingLPARName == "" {
			continue
		}

		if _, cached := systemLPARCache[node.SystemName]; !cached {
			v.log.Info(fmt.Sprintf("      Querying system '%s' for LPARs...", node.SystemName))

			var systemUUID string
			var err error
			v.log.Capture(func() {
				systemUUID, _, err = v.hmcClient.GetManagedSystemByName(node.SystemName, true)
			})
			if err != nil {
				v.errors = append(v.errors, fmt.Sprintf("failed to get system '%s' for LPAR validation: %v", node.SystemName, err))
				continue
			}

			var lpars []hmc.LogicalPartitionQuick
			v.log.Capture(func() {
				lpars, err = v.hmcClient.GetLogicalPartitionsQuickAll(systemUUID, true)
			})
			if err != nil {
				v.errors = append(v.errors, fmt.Sprintf("failed to get LPARs for system '%s': %v", node.SystemName, err))
				continue
			}

			lparMap := make(map[string]*hmc.LogicalPartitionQuick)
			for i := range lpars {
				lparMap[lpars[i].PartitionName] = &lpars[i]
			}
			systemLPARCache[node.SystemName] = lparMap

			v.log.Debug(fmt.Sprintf("      Found %d LPARs on system '%s'", len(lpars), node.SystemName))
		}

		if lparMap, ok := systemLPARCache[node.SystemName]; ok {
			lpar, exists := lparMap[node.ExistingLPARName]
			if !exists {
				v.errors = append(v.errors, fmt.Sprintf(
					"LPAR '%s' not found on system '%s' (required for node '%s')",
					node.ExistingLPARName, node.SystemName, node.Hostname))
			} else {
				if lpar.PartitionState == "running" {
					v.errors = append(v.errors, fmt.Sprintf(
						"SAFETY LOCK: BYOI LPAR '%s' is currently RUNNING on system '%s'. Shiftlaunch refuses to overwrite a running LPAR to prevent accidental data loss. Please power it off manually before deploying.",
						node.ExistingLPARName, node.SystemName))
				} else {
					v.log.Info(fmt.Sprintf("      ✓ LPAR '%s' exists on system '%s' (state: %s, role: %s)",
						node.ExistingLPARName, node.SystemName, lpar.PartitionState, node.Role))
					validatedCount++
				}
			}
		}
	}

	if len(v.errors) == 0 {
		v.log.Info(fmt.Sprintf("    ✓ All %d pre-provisioned LPAR(s) validated successfully", validatedCount))
	}
}

// ============================================================================
// PHASE 4: EXTERNAL SERVICES VALIDATION
// ============================================================================

func (v *Validator) validateExternalServices(ctx context.Context) {
	if !v.cfg.ManagedServices.DNS {
		v.validateExternalDNS(ctx)
	}
	if !v.cfg.ManagedServices.DHCP {
		v.validateExternalDHCP()
	}
	if !v.cfg.ManagedServices.PXE {
		v.validateExternalPXE()
	}
	if !v.cfg.ManagedServices.LoadBalancer {
		v.validateExternalLoadBalancer(ctx)
	}
	if v.cfg.Nodes.BootMethod == "iso" && !v.cfg.ManagedServices.NFS {
		v.log.Info("    Validating external NFS configuration...")
		v.warnings = append(v.warnings, 
			"External NFS detected for ISO boot. Ensure your external NFS server is configured to export the ISO directory to the VIOS, or enable 'managed_services.nfs' in your config.")
	}
}

func (v *Validator) validateExternalDNS(ctx context.Context) {
	v.log.Info("    Validating external DNS server...")

	dnsServer := v.cfg.Network.Nameserver
	if dnsServer == "" {
		v.warnings = append(v.warnings, "DNS is external, but network.nameserver is empty. External DNS validation skipped.")
		return
	}

	testCmd := fmt.Sprintf("dig @%s google.com +short +time=2 +tries=1", dnsServer)
	if _, err := v.exec.Execute(ctx,testCmd); err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf(
			"External DNS server %s may not be reachable or responding. Ensure DNS is properly configured before deployment.", dnsServer))
	} else {
		v.log.Info(fmt.Sprintf("      ✓ External DNS server %s is reachable", dnsServer))
	}
}

func (v *Validator) validateExternalDHCP() {
	v.log.Info("    Validating external DHCP configuration...")
	v.warnings = append(v.warnings,
		"External DHCP detected. Ensure DHCP server is configured with:\n"+
			"   - IP address pool covering cluster nodes\n"+
			"   - Correct gateway and DNS settings\n"+
			"   - Option 66 (TFTP server IP) if using external PXE\n"+
			"   - Option 67 (bootfile name: 'core.elf') if using external PXE")
}

func (v *Validator) validateExternalPXE() {
	v.log.Info("    Validating external PXE configuration...")
	v.warnings = append(v.warnings,
		"External PXE detected. Ensure PXE/TFTP server is configured with:\n"+
			"   - TFTP service running on port 69\n"+
			"   - RHCOS boot files (kernel, initramfs, rootfs) accessible\n"+
			"   - Proper file permissions for TFTP access\n"+
			"   - DHCP Option 66 pointing to PXE server IP\n"+
			"   - DHCP Option 67 set to 'core.elf'")
}

func (v *Validator) validateExternalLoadBalancer(ctx context.Context) {
	v.log.Info("    Validating external load balancer...")

	vip := v.cfg.Network.LoadBalancerIP
	if vip == "" {
		v.errors = append(v.errors, "Cannot validate external load balancer: cluster VIP not provided")
		return
	}

	ports := []struct {
		port int
		name string
	}{
		{6443, "Kubernetes API"},
		{22623, "Machine Config Server"},
		{443, "Ingress HTTPS"},
		{80, "Ingress HTTP"},
	}

	for _, p := range ports {
		testCmd := fmt.Sprintf("timeout 2 bash -c 'cat < /dev/null > /dev/tcp/%s/%d' 2>/dev/null", vip, p.port)
		if _, err := v.exec.Execute(ctx,testCmd); err != nil {
			v.warnings = append(v.warnings, fmt.Sprintf(
				"External load balancer port %d (%s) at %s is not responding. This is expected before cluster deployment, but ensure load balancer is configured.",
				p.port, p.name, vip))
		} else {
			v.log.Info(fmt.Sprintf("      ✓ Load balancer port %d (%s) is accessible", p.port, p.name))
		}
	}

	v.warnings = append(v.warnings,
		fmt.Sprintf("External load balancer detected at %s. Ensure it is configured with:\n"+
			"   - Port 6443: Forward to master nodes (Kubernetes API)\n"+
			"   - Port 22623: Forward to master nodes (Machine Config Server)\n"+
			"   - Port 443: Forward to worker nodes (Ingress HTTPS)\n"+
			"   - Port 80: Forward to worker nodes (Ingress HTTP)\n"+
			"   - Health checks enabled for all backend pools", vip))
}

// ============================================================================
// HELPER METHODS
// ============================================================================

func (v *Validator) isValidIP(ip string) bool {
	return net.ParseIP(ip) != nil
}

func (v *Validator) isValidCIDR(cidr string) bool {
	_, _, err := net.ParseCIDR(cidr)
	return err == nil
}

func (v *Validator) isValidHostname(hostname string) bool {
	hostnameRegex := regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?)*$`)
	return hostnameRegex.MatchString(hostname)
}
// validateMediaRepositorySpace ensures the VIOS has a Media Repository with enough space for Agent ISO files.
// If it doesn't exist, it will intelligently auto-discover a Volume Group and create it.
func (v *Validator) validateMediaRepositorySpace() {
	v.log.Info("    Validating VIOS Media Repository capacity...")

	nodes := v.cfg.GetAllNodes()
	if len(nodes) == 0 {
		return
	}

	// Grab the system name from the first node to locate the active VIOS
	systemName := nodes[0].SystemName

	// Use underscore first to drop the struct, grab the string UUID second
	_, sysUUID, err := v.hmcClient.GetManagedSystemByNameQuick(systemName, v.debug)
	if err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf("Could not resolve system UUID for repository check: %v", err))
		return
	}

	// Find the active VIOS
	viosList, err := v.hmcClient.GetVirtualIOServersQuick(sysUUID, v.debug)
	if err != nil || len(viosList) == 0 {
		v.warnings = append(v.warnings, "Could not retrieve VIOS list for repository check")
		return
	}

	var activeViosName string
	var activeViosUUID string
	for _, vios := range viosList {
		if vios.PartitionState == "running" && vios.RMCState == "active" {
			activeViosName = vios.PartitionName
			activeViosUUID = vios.UUID
			break
		}
	}

	if activeViosName == "" {
		v.warnings = append(v.warnings, "Could not find an active VIOS with RMC to check Media Repository capacity")
		return
	}

	// Calculate requirements: Agent ISO is roughly 1.5GB (1536 MB) per node
	nodeCount := len(nodes)
	requiredMB := 1536 * nodeCount
	
	// Force a sensible minimum of 10GB (10240 MB) so the repo isn't undersized
	if requiredMB < 10240 {
		requiredMB = 10240
	}
	requiredGB := float64(requiredMB) / 1024.0

	// 1. Try to fetch the existing repository info
	repoInfo, err := v.hmcClient.GetMediaRepositoryInfo(systemName, activeViosName, v.debug)
	
	// 2. If it fails, the repository is likely missing. Let's auto-create it!
	if err != nil {
		v.log.Info(fmt.Sprintf("      Media Repository not found on VIOS '%s'. Attempting to auto-create...", activeViosName))
		
		// Discover a suitable Volume Group
		vgs, vgErr := v.hmcClient.GetVolumeGroups(activeViosUUID, v.debug)
		if vgErr != nil {
			v.warnings = append(v.warnings, fmt.Sprintf("Failed to list Volume Groups for auto-creating Media Repository: %v", vgErr))
			return
		}
		
		var targetVG string
		// First pass: Prefer a VG that is NOT rootvg and has enough free space
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
		
		// Second pass: Fallback to rootvg if absolutely necessary
		if targetVG == "" {
			for _, vg := range vgs {
				freeSpaceGB, parseErr := strconv.ParseFloat(vg.FreeSpace, 64)
				if parseErr == nil && freeSpaceGB >= requiredGB {
					targetVG = vg.GroupName
					v.log.Warn(fmt.Sprintf("      ⚠️ Warning: Using '%s' for Media Repository as no other VG has %.2f GB free", vg.GroupName, requiredGB))
					break
				}
			}
		}
		
		if targetVG == "" {
			v.errors = append(v.errors, fmt.Sprintf("Cannot auto-create Media Repository on VIOS '%s': No Volume Group found with at least %.2f GB of free space.", activeViosName, requiredGB))
			return
		}
		
		// Create the Media Repository using the SDK
		v.log.Info(fmt.Sprintf("      Auto-creating %d MB Media Repository in VG '%s' on VIOS '%s'...", requiredMB, targetVG, activeViosName))
		createErr := v.hmcClient.CreateMediaRepository(systemName, activeViosUUID, activeViosName, targetVG, requiredMB, v.debug)
		if createErr != nil {
			v.errors = append(v.errors, fmt.Sprintf("Failed to auto-create Media Repository: %v", createErr))
			return
		}
		
		v.log.Info("      ✓ Media Repository created successfully!")
		return
	}

	// 3. If repository already exists, validate its free space
	v.log.Info(fmt.Sprintf("      Repository Size: %d MB | Free: %d MB | Required: %d MB (%d nodes)", 
		repoInfo.SizeMB, repoInfo.FreeMB, requiredMB, nodeCount))

	if repoInfo.FreeMB < requiredMB {
		v.errors = append(v.errors, fmt.Sprintf(
			"VIOS MEDIA REPOSITORY FULL: VIOS '%s' only has %d MB free, but %d MB is required.\n"+
			"   Solution 1: Clean up old ISOs via HMC (rmvopt).\n"+
			"   Solution 2: Expand the repository using 'chrep -size'.", 
			activeViosName, repoInfo.FreeMB, requiredMB))
	} else {
		v.log.Info("      ✓ Sufficient space available in VIOS Media Repository")
	}
}