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
	"sync"

	hmc "github.com/IBM/infra-go-sdk/phmc"
	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
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
	hmcClient    *hmc.RestClient
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
func (v *Validator) SetHMCClient(client *hmc.RestClient) {
	v.hmcClient = client
}

// SetLogger sets the custom logger for deployment context
func (v *Validator) SetLogger(l *logger.Logger) {
	if l != nil {
		v.log = l
	}
}

// Validate performs comprehensive validation in checks
func (v *Validator) Validate(ctx context.Context) error {
	// Check 1: Config Validation (static, fast)
	v.log.StartPhase("[Check 1/4] Validating configuration syntax and parameters...")

	v.validateController()
	v.validateHMC()
	v.validateNetwork(ctx)
	v.validateOpenShift()
	v.validateDisconnected()
	v.validateNodes()

	v.log.EndPhase(true, "[Check 1/4] Configuration valid")

	// Check 2: Local Controller Environment Validation
	if v.exec != nil {
		v.log.StartPhase("[Check 2/4] Validating local controller environment and resources...")
		v.validateLocalEnvironment(ctx)
		v.log.EndPhase(true, "[Check 2/4] Local environment validated")
	}

	// Check 3: HMC Validation (HMC API-based)
	if v.hmcClient != nil {
		v.log.StartPhase("[Check 3/4] Validating HMC connectivity and LPAR readiness...")
		v.validateBYOILPARs()
		if v.cfg.Nodes.BootMethod == "agent" {
			v.validateMediaRepositorySpace()
		}
		v.log.EndPhase(true, "[Check 3/4] HMC infrastructure validated")
	}

	// Check 4: External Services Validation
	if v.exec != nil {
		hasExternalServices := !v.cfg.Services.DNS.IsManaged() || !v.cfg.Services.DHCP.IsManaged() ||
			!v.cfg.Services.PXE.IsManaged() || !v.cfg.Services.LoadBalancer.IsManaged()

		if hasExternalServices {
			v.log.StartPhase("[Check 4/4] Validating external unmanaged services...")
			v.validateExternalServices(ctx)
			v.log.EndPhase(true, "[Check 4/4] External services validated")
		} else {
			v.log.StartPhase("[Check 4/4] Validating external unmanaged services...")
			v.log.EndPhase(true, "[Check 4/4] Skipping external services (all services are managed)")
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
	if v.cfg.Network.ControllerInterface == "" {
		v.errors = append(v.errors, "network.controller_interface is required")
	}

	// VIP validation is now handled by the auto-resolver in cmd/root.go
	// The VIP will be automatically set to either:
	// 1. The external_lb_ip if provided
	// 2. The controller_ip if no VIP is specified (zero-config mode)
	// 3. The explicitly provided vip value
	vip := v.cfg.Services.LoadBalancer.VIP
	if vip != "" && !v.isValidIP(vip) {
		v.errors = append(v.errors, fmt.Sprintf("services.load_balancer.vip '%s' is not a valid IP", vip))
	}
}

// validateHMC validates HMC configuration
func (v *Validator) validateHMC() {
	h := v.cfg.HMC
	if h.IP == "" {
		v.errors = append(v.errors, "hmc.ip is required")
	}
	if h.Username == "" {
		v.errors = append(v.errors, "hmc.username is required")
	}
	if h.Password == "" || h.Password == "YOUR_HMC_PASSWORD" {
		v.errors = append(v.errors, "hmc.password must be set to your actual physical console password")
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
		return
	} else if !v.isValidCIDR(n.MachineCIDR) {
		v.errors = append(v.errors, fmt.Sprintf("network.machine_network_cidr '%s' is not a valid CIDR", n.MachineCIDR))
		return
	}

	_, ipNet, err := net.ParseCIDR(n.MachineCIDR)
	if err == nil {
		if n.Gateway != "" {
			gwIP := net.ParseIP(n.Gateway)
			if gwIP == nil || !ipNet.Contains(gwIP) {
				v.errors = append(v.errors, fmt.Sprintf("network.gateway '%s' is not within the machine_network_cidr '%s'", n.Gateway, n.MachineCIDR))
			}
		}
		vip := v.cfg.Services.LoadBalancer.VIP
		if vip != "" {
			vipIP := net.ParseIP(vip)
			if vipIP == nil || !ipNet.Contains(vipIP) {
				v.errors = append(v.errors, fmt.Sprintf("services.load_balancer.vip '%s' is not within the machine_network_cidr '%s'", vip, n.MachineCIDR))
			}
		}
	}

	if n.Gateway == "" {
		v.errors = append(v.errors, "network.gateway is required")
	}

	// Enforce strict logic dependencies on DNS profiles
	if v.cfg.Services.DNS.IsManaged() {
		if len(v.cfg.Network.UpstreamNameservers) == 0 {
			v.errors = append(v.errors, "services.dns.dns_forwarders requires at least one address when local DNS management is active")
		}
	} else {
		if v.cfg.Services.DNS.GetExternal() == "" {
			v.errors = append(v.errors, "services.dns.external_nameserver parameter must be configured when local DNS is disabled")
		}
	}

	if v.cfg.Services.LoadBalancer.IsManaged() && v.cfg.Services.LoadBalancer.GetVIP() != "" {
		v.validateVIPNotInUse(ctx, v.cfg.Services.LoadBalancer.GetVIP())
	}
}

// validateVIPNotInUse checks if the VIP is already configured on the controller interface
// or being used by another managed cluster
func (v *Validator) validateVIPNotInUse(ctx context.Context, vip string) {
	iface := v.cfg.Network.ControllerInterface
	ctrlIP := v.cfg.Network.ControllerIP

	// ZERO-CONFIG CHECK: If it is the controller IP, it is allowed to be on the interface!
	if iface == "" || vip == ctrlIP {
		return
	}

	output, err := v.exec.Execute(ctx, fmt.Sprintf("ip addr show %s", iface))
	if err == nil && strings.Contains(output, vip+"/") {
		conflictingCluster := v.findClusterUsingVIP(vip)
		if conflictingCluster != "" {
			v.errors = append(v.errors, fmt.Sprintf("VIP %s is already in use by cluster '%s'. Choose a different VIP", vip, conflictingCluster))
		} else {
			v.errors = append(v.errors, fmt.Sprintf("VIP %s is already configured on interface %s. Remove it manually or use a different IP", vip, iface))
		}
		return
	}

	conflictingCluster := v.findClusterUsingVIP(vip)
	if conflictingCluster != "" {
		v.errors = append(v.errors, fmt.Sprintf("VIP %s is already assigned to cluster '%s' in configurations", vip, conflictingCluster))
	}
}

func (v *Validator) findClusterUsingVIP(vip string) string {
	entries, err := os.ReadDir(v.workspaceDir)
	if err != nil {
		return ""
	}
	currentCluster := v.cfg.OpenShift.ClusterName

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		clusterName := entry.Name()
		if clusterName == currentCluster {
			continue
		}

		if _, err1 := os.Stat(filepath.Join(v.workspaceDir, clusterName, ".managed")); os.IsNotExist(err1) {
			if _, err2 := os.Stat(filepath.Join(v.workspaceDir, clusterName, ".failed")); os.IsNotExist(err2) {
				continue
			}
		}
		if _, err := os.Stat(filepath.Join(v.workspaceDir, clusterName, ".deleted")); err == nil {
			continue
		}

		configPath := filepath.Join(v.workspaceDir, clusterName, "config.yaml")
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		var tempCfg types.AgentConfig
		if err := yaml.Unmarshal(data, &tempCfg); err == nil {
			if tempCfg.Services.LoadBalancer != nil && tempCfg.Services.LoadBalancer.GetVIP() == vip {
				return clusterName
			}
		}
	}
	return ""
}

// validateOpenShift validates OpenShift configuration
func (v *Validator) validateOpenShift() {
	o := v.cfg.OpenShift

	if o.Version == "" {
		v.errors = append(v.errors, "openshift.version is required")
	}

	// Validate strict enum for Release Type
	if o.ReleaseType != "official" && o.ReleaseType != "ci" {
		v.errors = append(v.errors, fmt.Sprintf("openshift.release_type must be either 'official' or 'ci', got '%s'", o.ReleaseType))
	}

	if o.PullSecretFile == "" {
		v.errors = append(v.errors, "openshift.pull_secret_file is required")
	} else {
		// Safely expand ~ to $HOME just in case
		secretPath := os.ExpandEnv(strings.ReplaceAll(o.PullSecretFile, "~", "$HOME"))
		if _, err := os.Stat(secretPath); os.IsNotExist(err) {
			v.errors = append(v.errors, fmt.Sprintf("openshift.pull_secret_file '%s' does not exist locally", o.PullSecretFile))
		}
	}

	if o.SSHPublicKeyFile == "" {
		v.errors = append(v.errors, "openshift.ssh_public_key_file is required")
	} else {
		// Safely expand ~ to $HOME
		keyPath := os.ExpandEnv(strings.ReplaceAll(o.SSHPublicKeyFile, "~", "$HOME"))
		if _, err := os.Stat(keyPath); os.IsNotExist(err) {
			v.errors = append(v.errors, fmt.Sprintf("openshift.ssh_public_key_file '%s' does not exist locally", o.SSHPublicKeyFile))
		}
	}

	preRelease := isPreReleaseVersion(o.Version)

	// Skip RHCOS validation for Agent boot (Agent installer downloads RHCOS automatically)
	if v.cfg.Nodes.BootMethod == "agent" {
		v.log.Info("Skipping RHCOS image validation for Agent ISO boot (Agent installer downloads RHCOS automatically)")
		// OCP client URLs are always required for agent boot
		if o.OCPClientConfig.Client == "" {
			if preRelease {
				v.errors = append(v.errors, fmt.Sprintf("openshift.ocp_client_config.ocp_client is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
			} else {
				v.errors = append(v.errors, "openshift.ocp_client_config.ocp_client is required")
			}
		}
		if o.OCPClientConfig.Installer == "" {
			if preRelease {
				v.errors = append(v.errors, fmt.Sprintf("openshift.ocp_client_config.ocp_installer is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
			} else {
				v.errors = append(v.errors, "openshift.ocp_client_config.ocp_installer is required")
			}
		}
		return // Skip RHCOS URL validation
	}

	// Validate RHCOS URLs for netboot
	if o.RHCOSImages.KernelURL == "" {
		if preRelease {
			v.errors = append(v.errors, fmt.Sprintf("openshift.rhcos_images.kernel_url is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
		} else {
			v.errors = append(v.errors, "openshift.rhcos_images.kernel_url is required")
		}
	}
	if o.RHCOSImages.InitramfsURL == "" {
		if preRelease {
			v.errors = append(v.errors, fmt.Sprintf("openshift.rhcos_images.initramfs_url is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
		} else {
			v.errors = append(v.errors, "openshift.rhcos_images.initramfs_url is required")
		}
	}
	if o.RHCOSImages.RootfsURL == "" {
		if preRelease {
			v.errors = append(v.errors, fmt.Sprintf("openshift.rhcos_images.rootfs_url is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
		} else {
			v.errors = append(v.errors, "openshift.rhcos_images.rootfs_url is required")
		}
	}

	// Validate OCP client config for netboot (already validated above for agent boot)
	if o.OCPClientConfig.Client == "" {
		if preRelease {
			v.errors = append(v.errors, fmt.Sprintf("openshift.ocp_client_config.ocp_client is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
		} else {
			v.errors = append(v.errors, "openshift.ocp_client_config.ocp_client is required")
		}
	}
	if o.OCPClientConfig.Installer == "" {
		if preRelease {
			v.errors = append(v.errors, fmt.Sprintf("openshift.ocp_client_config.ocp_installer is required: version '%s' is a pre-release and has no stable mirror path — provide the URL explicitly", o.Version))
		} else {
			v.errors = append(v.errors, "openshift.ocp_client_config.ocp_installer is required")
		}
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

func (v *Validator) validateDisconnected() {
	mode := v.cfg.Network.IsolationLevel
	if mode != "connected" && mode != "restricted-network" && mode != "air-gapped" {
		v.errors = append(v.errors, fmt.Sprintf("network.isolation_level must be 'connected', 'restricted-network', or 'air-gapped'. Got: '%s'", mode))
		return
	}

	if mode == "air-gapped" {
		reg := v.cfg.Services.Registry
		if !reg.IsManaged() && reg.GetExternal() == "registry.example.com" {
			v.errors = append(v.errors, "CRITICAL CONFIGURATION ERROR: network.isolation_level is locked to 'air-gapped', but services.registry.external_reqistry_server is still set to the default placeholder value ('registry.example.com'). Please update it to your actual enterprise registry or switch isolation modes.")
			return
		}
		// Note: The check for missing registry is no longer needed because cmd/root.go
		// automatically injects an empty ServiceRegistry{} if air-gapped mode is detected
		// without an explicit registry configuration (Registry Zero-Config Auto-Resolver)
	}

	if mode == "restricted-network" {
		proxy := v.cfg.Services.Proxy
		if !proxy.IsManaged() && proxy.GetHTTP() == "" {
			v.errors = append(v.errors, "Proxy configuration conflict: network.isolation_level is set to 'restricted-network' but local Squid management is disabled and no external_http_proxy path was supplied.")
		}
	}
}

func (v *Validator) validateNodes() {
	if err := v.cfg.Validate(); err != nil {
		v.errors = append(v.errors, err.Error())
		return
	}

	if v.cfg.Nodes.BootMethod == "agent" && !v.cfg.Services.NFS.IsManaged() {
		v.errors = append(v.errors, "Boot method 'agent' requires services.nfs.enabled to be set to true so ShiftLaunch can transfer assets to the storage subsystem")
	}

	// Validate external NFS configuration
	if !v.cfg.Services.NFS.IsManaged() && v.cfg.Services.NFS.GetExternal() != "" {
		if v.cfg.Services.NFS.GetExternalPath() == "" {
			v.errors = append(v.errors, "services.nfs.external_nfs_path is required when using external NFS (services.nfs.external_server is set)")
		}
	}

	// Track uniqueness to prevent copy-paste errors
	seenIPs := make(map[string]string)
	seenHosts := make(map[string]string)
	seenLPARs := make(map[string]string)

	_, ipNet, _ := net.ParseCIDR(v.cfg.Network.MachineCIDR)

	for _, node := range v.cfg.GetAllNodes() {
		// 1. Uniqueness Checks
		if existing, ok := seenIPs[node.IP]; ok {
			v.errors = append(v.errors, fmt.Sprintf("duplicate IP detected: '%s' is used by both '%s' and '%s'", node.IP, existing, node.Hostname))
		}
		seenIPs[node.IP] = node.Hostname

		if _, ok := seenHosts[node.Hostname]; ok {
			v.errors = append(v.errors, fmt.Sprintf("duplicate hostname detected: '%s' is defined multiple times", node.Hostname))
		}
		seenHosts[node.Hostname] = node.Hostname

		if existing, ok := seenLPARs[node.ExistingLPARName]; ok {
			v.errors = append(v.errors, fmt.Sprintf("duplicate LPAR target detected: '%s' is targeted by both '%s' and '%s'", node.ExistingLPARName, existing, node.Hostname))
		}
		seenLPARs[node.ExistingLPARName] = node.Hostname

		// 2. Subnet Verification
		if ipNet != nil {
			nodeIP := net.ParseIP(node.IP)
			if nodeIP != nil && !ipNet.Contains(nodeIP) {
				v.errors = append(v.errors, fmt.Sprintf("node IP '%s' (%s) is outside the defined machine_network_cidr '%s'", node.IP, node.Hostname, v.cfg.Network.MachineCIDR))
			}
		}
	}

	if v.cfg.IsSNO() {
		v.validateSNONode()
	} else {
		v.validateMultiNodeCluster()
	}
}

func (v *Validator) validateSNONode() {
	sno := v.cfg.Nodes.Masters[0]

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
	masterCount := len(v.cfg.Nodes.Masters)
	workerCount := len(v.cfg.Nodes.Workers)
	bootstrapCount := len(v.cfg.Nodes.Bootstrap)

	// ========================================================================
	// TOPOLOGY ENFORCEMENT: Boot Method Constraints
	// ========================================================================
	if v.cfg.Nodes.BootMethod == "netboot" {
		if bootstrapCount != 1 {
			v.errors = append(v.errors, fmt.Sprintf("netboot deployment requires exactly 1 bootstrap node, got %d. Please define the 'bootstrap' block in your config.yaml", bootstrapCount))
		}
		if masterCount != 3 {
			v.errors = append(v.errors, fmt.Sprintf("netboot deployment requires exactly 3 master nodes, got %d", masterCount))
		}
		if workerCount < 2 {
			v.errors = append(v.errors, fmt.Sprintf("netboot deployment requires a minimum of 2 worker nodes, got %d", workerCount))
		}
	} else if v.cfg.Nodes.BootMethod == "agent" {
		// Agent-based installer dynamically handles bootstrap within a master node.
		// Defining an explicit bootstrap node here is a user error.
		if bootstrapCount > 0 {
			v.errors = append(v.errors, fmt.Sprintf("agent boot method does not use a standalone bootstrap node, but %d were defined. Please remove the 'bootstrap' block from your config.yaml", bootstrapCount))
		}
		// Standard Quorum Checks for Agent
		if masterCount < 3 {
			v.errors = append(v.errors, fmt.Sprintf("minimum 3 master nodes required for highly available agent deployment, got %d", masterCount))
		} else if masterCount%2 == 0 {
			v.errors = append(v.errors, fmt.Sprintf("master count must be odd for quorum, got %d", masterCount))
		}
	}

	// ========================================================================
	// PARAMETER VALIDATION: Masters
	// ========================================================================
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

	// ========================================================================
	// PARAMETER VALIDATION: Bootstrap
	// ========================================================================
	if bootstrapCount > 0 {
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

	// ========================================================================
	// PARAMETER VALIDATION: Workers
	// ========================================================================
	if workerCount > 0 {
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
	//Validate the physical interface actually exists on this host
	iface := v.cfg.Network.ControllerInterface
	if iface != "" {
		checkCmd := fmt.Sprintf("ip link show %s >/dev/null 2>&1 && echo 'exists' || echo 'missing'", iface)
		if out, err := v.exec.Execute(ctx, checkCmd); err == nil && strings.TrimSpace(out) == "missing" {
			v.errors = append(v.errors, fmt.Sprintf("FATAL: controller.network_interface '%s' does not exist on this machine. Run 'ip a' to find the correct interface", iface))
		}
	}

	// Check for sufficient disk space locally (must have at least 10GB free)
	v.validateLocalDiskSpace(ctx)

	// Check for Directory/Config Collisions
	clusterName := v.cfg.OpenShift.ClusterName
	httpDir := fmt.Sprintf("/var/www/html/%s", clusterName)
	dnsmasqPath := fmt.Sprintf("/etc/dnsmasq.d/*-%s-*.conf", clusterName)
	haproxyPath := fmt.Sprintf("/etc/haproxy/conf.d/99-%s.cfg", clusterName)

	checkCmd := fmt.Sprintf("if [ -d '%s' ] || ls %s 1> /dev/null 2>&1 || ls %s 1> /dev/null 2>&1; then echo 'exists'; else echo 'missing'; fi",
		httpDir, dnsmasqPath, haproxyPath)

	if out, err := v.exec.Execute(ctx, checkCmd); err == nil && strings.TrimSpace(out) == "exists" {
		v.errors = append(v.errors, fmt.Sprintf("CLUSTER COLLISION: Artifacts for '%s' already exist locally. Run the 'delete' command to clean them up first to prevent accidental overwrites.", clusterName))
	}

	// 4. Check for VIP Conflicts on the network
	vip := v.cfg.Services.LoadBalancer.VIP
	if vip != "" {
		iface := v.cfg.Network.ControllerInterface
		ctrlIP := v.cfg.Network.ControllerIP

		// ZERO-CONFIG CHECK: If the VIP is intentionally set to the Controller IP, it's perfectly fine!
		if vip != ctrlIP {
			checkBoundCmd := fmt.Sprintf("ip addr show dev %s | grep -q '%s/'", iface, vip)
			if _, err := v.exec.Execute(ctx, checkBoundCmd); err != nil {
				// Not bound to us. Check if it's in use on the network.
				if v.cfg.Services.LoadBalancer.IsManaged() {
					pingCmd := fmt.Sprintf("ping -c 2 -W 2 %s", vip)
					if _, pingErr := v.exec.Execute(ctx, pingCmd); pingErr == nil {
						v.errors = append(v.errors, fmt.Sprintf("IP CONFLICT: The VIP %s is already actively responding on the network. Please choose an unused IP.", vip))
					}
				}
			}
		}
	}

	// 5. Check for Node IP Conflicts on the network
	v.validateNodeIPsNotAlive(ctx)

	// ========================================================================
	// 6. Check for Local Port Collisions (IP-Aware)
	// ========================================================================
	if v.cfg.Services.LoadBalancer.IsManaged() {
		v.log.Debug("Checking for local port collisions for managed HAProxy...")
		
		requiredPorts := []int{80, 443, 6443, 22623}
		vip := v.cfg.Services.LoadBalancer.GetVIP()
		
		for _, port := range requiredPorts {
			// Check if bound to 0.0.0.0, *, [::], or explicitly to our VIP
			checkCmd := fmt.Sprintf("sudo ss -tlpn | grep -E '(0\\.0\\.0\\.0|\\*|\\[::\\]|%s):%d ' | grep -v -i 'haproxy'", vip, port)
			if _, err := v.exec.Execute(ctx, checkCmd); err == nil {
				v.errors = append(v.errors, fmt.Sprintf("PORT COLLISION: Local TCP port %d is already bound globally or specifically to %s by a non-HAProxy process. Please stop the conflicting service.", port, vip))
			}
		}
	}

	if v.cfg.Services.DNS.IsManaged() {
		ctrlIP := v.cfg.Network.ControllerIP

		if _, err := v.exec.Execute(ctx, fmt.Sprintf("sudo ss -ulpn | grep -E '(0\\.0\\.0\\.0|\\*|\\[::\\]|%s):53 ' | grep -v -i 'dnsmasq'", ctrlIP)); err == nil {
			v.errors = append(v.errors, "PORT COLLISION: Local UDP port 53 is already bound globally or to the controller IP by a non-dnsmasq process. The managed DNS service will fail to start.")
		}
		if _, err := v.exec.Execute(ctx, fmt.Sprintf("sudo ss -tlpn | grep -E '(0\\.0\\.0\\.0|\\*|\\[::\\]|%s):53 ' | grep -v -i 'dnsmasq'", ctrlIP)); err == nil {
			v.errors = append(v.errors, "PORT COLLISION: Local TCP port 53 is already bound globally or to the controller IP by a non-dnsmasq process. The managed DNS service will fail to start.")
		}
	}

	if v.cfg.Network.IsolationLevel == "air-gapped" && v.cfg.Services.Registry.IsManaged() {
		ctrlIP := v.cfg.Network.ControllerIP
		// 1. Check if port 5000 is bound to our IP or globally
		if _, err := v.exec.Execute(ctx, fmt.Sprintf("sudo ss -tlpn | grep -E '(0\\.0\\.0\\.0|\\*|\\[::\\]|%s):5000 '", ctrlIP)); err == nil {

			// 2. Port is bound. Ask the port if it's our multi-tenant registry!
			user := v.cfg.Services.Registry.GetUser()
			pass := v.cfg.Services.Registry.GetPass()
			verifyCmd := fmt.Sprintf("env HTTP_PROXY='' HTTPS_PROXY='' http_proxy='' https_proxy='' curl -s -o /dev/null -w '%%{http_code}' --connect-timeout 3 -u %s:%s -k https://%s:5000/v2/", user, pass, ctrlIP)

			httpCode, _ := v.exec.Execute(ctx, verifyCmd)
			if strings.TrimSpace(httpCode) != "200" {
				v.errors = append(v.errors, "PORT COLLISION: Local TCP port 5000 is already bound by an unknown process (or multi-tenant registry authentication failed). The managed container registry will fail to start.")
			} else {
				v.log.Debug("Active multi-tenant registry detected on port 5000. Validation passed.")
			}
		}
	}

	if v.cfg.Services.Proxy.IsManaged() {
		ctrlIP := v.cfg.Network.ControllerIP
		if _, err := v.exec.Execute(ctx, fmt.Sprintf("sudo ss -tlpn | grep -E '(0\\.0\\.0\\.0|\\*|\\[::\\]|%s):3128 ' | grep -v -i 'squid'", ctrlIP)); err == nil {
			v.errors = append(v.errors, "PORT COLLISION: Local TCP port 3128 is already bound globally or to the controller IP by a non-squid process. The managed proxy gateway will fail to start.")
		}
	}
}

func (v *Validator) validateLocalDiskSpace(ctx context.Context) {
	// On a fresh OS, httpd isn't installed yet, so /var/www/html won't exist.
	// We create it silently here so the 'df' command doesn't crash.
	v.exec.Execute(ctx, "sudo mkdir -p /var/www/html")
	dfCmd := "df -BK --output=avail /var/www/html | tail -n 1 | tr -d 'K'"
	output, err := v.exec.Execute(ctx, dfCmd)
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
		v.log.Debug(fmt.Sprintf("Controller has %.2f GB available in /var/www/html", availableGB))
	}

	//  Validate registry capacity for Airgapped deployments
	if v.cfg.Network.IsolationLevel == "air-gapped" && v.cfg.Services.Registry.IsManaged() {
		v.exec.Execute(ctx, "sudo mkdir -p /opt/registry/data")
		regDfCmd := "df -BK --output=avail /opt/registry/data | tail -n 1 | tr -d 'K'"
		regOutput, regErr := v.exec.Execute(ctx, regDfCmd)
		if regErr != nil {
			v.warnings = append(v.warnings, fmt.Sprintf("Unable to check registry disk space: %v", regErr))
			return
		}

		var regAvailKB int
		regTrimmed := strings.TrimSpace(regOutput)
		if _, err := fmt.Sscanf(regTrimmed, "%d", &regAvailKB); err != nil {
			v.warnings = append(v.warnings, fmt.Sprintf("Unable to parse registry disk space output '%s': %v", regTrimmed, err))
			return
		}

		regAvailGB := float64(regAvailKB) / (1024 * 1024)
		// A full OCP mirror usually needs at least 60GB
		regRequiredGB := 40.0

		if regAvailGB < regRequiredGB {
			v.errors = append(v.errors,
				fmt.Sprintf("INSUFFICIENT DISK SPACE: Airgap mirroring enabled, but /opt/registry/data only has %.2f GB available (%.0f GB required).",
					regAvailGB, regRequiredGB))
		} else {
			v.log.Debug(fmt.Sprintf("Registry partition has %.2f GB available for image mirroring", regAvailGB))
		}
	}
}

// ============================================================================
// PHASE 3: HMC VALIDATION (HMC API-Based)
// ============================================================================

// validateBYOILPARs validates that all specified LPARs exist in BYOI mode
func (v *Validator) validateBYOILPARs() {
	v.log.Info("Validating pre-provisioned LPAR existence...")

	allNodes := v.cfg.GetAllNodes()
	systemLPARCache := make(map[string]map[string]*hmc.LogicalPartitionQuick)

	validatedCount := 0
	for _, node := range allNodes {
		if node.ExistingLPARName == "" {
			continue
		}

		if _, cached := systemLPARCache[node.SystemName]; !cached {
			v.log.Info(fmt.Sprintf("Querying system '%s' for LPARs...", node.SystemName))

			var systemUUID string
			var err error
			v.log.Capture(func() {
				systemUUID, _, err = v.hmcClient.GetManagedSystemByName(context.Background(), node.SystemName)
			})
			if err != nil {
				v.errors = append(v.errors, fmt.Sprintf("failed to get system '%s' for LPAR validation: %v", node.SystemName, err))
				continue
			}

			var lpars []hmc.LogicalPartitionQuick
			v.log.Capture(func() {
				lpars, err = v.hmcClient.GetLogicalPartitionsQuickAll(context.Background(), systemUUID)
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

			v.log.Debug(fmt.Sprintf("Found %d LPARs on system '%s'", len(lpars), node.SystemName))
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
						"SAFETY LOCK: BYOI LPAR '%s' is currently RUNNING on system '%s'. ShiftLaunch refuses to overwrite a running LPAR to prevent accidental data loss. Please power it off manually before deploying.",
						node.ExistingLPARName, node.SystemName))
				} else {
					v.log.Info(fmt.Sprintf("LPAR '%s' exists on system '%s' (state: %s, role: %s)",
						node.ExistingLPARName, node.SystemName, lpar.PartitionState, node.Role))
					validatedCount++
				}
			}
		}
	}

	if len(v.errors) == 0 {
		v.log.Info(fmt.Sprintf("All %d pre-provisioned LPAR(s) validated successfully", validatedCount))
	}
}

// ============================================================================
// PHASE 4: EXTERNAL SERVICES VALIDATION
// ============================================================================

func (v *Validator) validateExternalServices(ctx context.Context) {
	if !v.cfg.Services.DNS.IsManaged() {
		v.validateExternalDNS(ctx)
	}
	if !v.cfg.Services.DHCP.IsManaged() {
		v.validateExternalDHCP()
	}
	//  Only validate external PXE if we are actually using network boot!
	if !v.cfg.Services.PXE.IsManaged() && v.cfg.Nodes.BootMethod != "agent" {
		v.validateExternalPXE()
	}
	if !v.cfg.Services.LoadBalancer.IsManaged() {
		v.validateExternalLoadBalancer(ctx)
	}
	// NFS validation removed - now enforced as hard error in validateNodes()
}

func (v *Validator) validateExternalDNS(ctx context.Context) {
	v.log.Info("Validating external DNS server...")

	dnsServer := v.cfg.Services.DNS.GetExternal()
	if dnsServer == "" {
		v.warnings = append(v.warnings, "DNS is external, but network.nameserver is empty. External DNS validation skipped.")
		return
	}

	testCmd := fmt.Sprintf("dig @%s google.com +short +time=2 +tries=1", dnsServer)
	if _, err := v.exec.Execute(ctx, testCmd); err != nil {
		v.warnings = append(v.warnings, fmt.Sprintf(
			"External DNS server %s may not be reachable or responding. Ensure DNS is properly configured before deployment.", dnsServer))
	} else {
		v.log.Info(fmt.Sprintf("External DNS server %s is reachable", dnsServer))
	}
}

func (v *Validator) validateExternalDHCP() {
	v.log.Info("Validating external DHCP configuration...")

	// Check if all nodes have static IPs configured
	allNodesHaveStaticIP := true
	nodes := v.cfg.GetAllNodes()
	for _, node := range nodes {
		if node.IP == "" {
			allNodesHaveStaticIP = false
			break
		}
	}

	// Skip DHCP warning if using static IPs
	if allNodesHaveStaticIP {
		v.log.Debug("Static IPs configured for all nodes via NMState. DHCP not required.")
		return
	}

	//  Provide contextual warnings based on the boot method
	if v.cfg.Nodes.BootMethod == "agent" {
		v.warnings = append(v.warnings,
			"External DHCP detected. Ensure DHCP server is configured with:\n"+
				"   - IP address pool covering cluster nodes (or use static IPs via NMState)\n"+
				"   - Correct gateway and DNS settings")
	} else {
		v.warnings = append(v.warnings,
			"External DHCP detected. Ensure DHCP server is configured with:\n"+
				"   - IP address pool covering cluster nodes\n"+
				"   - Correct gateway and DNS settings\n"+
				"   - Option 66 (TFTP server IP) if using external PXE\n"+
				"   - Option 67 (bootfile name: 'core.elf') if using external PXE")
	}
}

func (v *Validator) validateExternalPXE() {
	v.log.Info("Validating external PXE configuration...")
	v.warnings = append(v.warnings,
		"External PXE detected. Ensure PXE/TFTP server is configured with:\n"+
			"   - TFTP service running on port 69\n"+
			"   - RHCOS boot files (kernel, initramfs, rootfs) accessible\n"+
			"   - Proper file permissions for TFTP access\n"+
			"   - DHCP Option 66 pointing to PXE server IP\n"+
			"   - DHCP Option 67 set to 'core.elf'")
}

func (v *Validator) validateExternalLoadBalancer(ctx context.Context) {
	v.log.Info("Validating external load balancer...")

	vip := v.cfg.Services.LoadBalancer.VIP
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
		if _, err := v.exec.Execute(ctx, testCmd); err != nil {
			v.warnings = append(v.warnings, fmt.Sprintf(
				"External load balancer port %d (%s) at %s is not responding. This is expected before cluster deployment, but ensure load balancer is configured.",
				p.port, p.name, vip))
		} else {
			v.log.Info(fmt.Sprintf("Load balancer port %d (%s) is accessible", p.port, p.name))
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
// If it doesn't exist, it verifies a Volume Group exists with sufficient space for Phase 5 to auto-create it.
func (v *Validator) validateMediaRepositorySpace() {
	v.log.Info("Validating VIOS Media Repository capacity...")

	nodes := v.cfg.GetAllNodes()
	if len(nodes) == 0 {
		return
	}

	// 1. Group nodes by their target physical system to calculate per-system space requirements
	systemNodeCount := make(map[string]int)
	for _, node := range nodes {
		systemNodeCount[node.SystemName]++
	}

	// 2. Validate each unique system independently
	for systemName, count := range systemNodeCount {
		v.log.Info(fmt.Sprintf("Validating Media Repository on system '%s' for %d node(s)...", systemName, count))

		_, sysUUID, err := v.hmcClient.GetManagedSystemByNameQuick(context.Background(), systemName)
		if err != nil {
			v.warnings = append(v.warnings, fmt.Sprintf("Could not resolve system UUID for repository check on '%s': %v", systemName, err))
			continue
		}

		// Find the active VIOS
		viosList, err := v.hmcClient.GetVirtualIOServersQuick(context.Background(), sysUUID)
		if err != nil || len(viosList) == 0 {
			v.warnings = append(v.warnings, fmt.Sprintf("Could not retrieve VIOS list for repository check on '%s'", systemName))
			continue
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
			v.warnings = append(v.warnings, fmt.Sprintf("Could not find an active VIOS with RMC to check Media Repository capacity on '%s'", systemName))
			continue
		}

		// Calculate ACTUAL requirements strictly for the nodes hosted on THIS system
		actualRequiredMB := 1536 * count

		// What we will request if we have to auto-create it (enforce 10GB minimum for creation)
		createRequestMB := actualRequiredMB
		if createRequestMB < 10240 {
			createRequestMB = 10240
		}
		requiredGB := float64(createRequestMB) / 1024.0

		// 1. Try to fetch the existing repository info
		repoInfo, err := v.hmcClient.GetMediaRepositoryInfo(context.Background(), systemName, activeViosName)

		// 2. If it fails OR SizeMB is 0, the repository is missing. Verify we HAVE the capacity to auto-create it later.
		if err != nil || repoInfo.SizeMB == 0 {
			v.log.Info(fmt.Sprintf("Media Repository not found on VIOS '%s' (or size is 0). Verifying auto-creation capacity...", activeViosName))

			// Discover a suitable Volume Group
			vgs, vgErr := v.hmcClient.GetVolumeGroups(context.Background(), activeViosUUID)
			if vgErr != nil {
				v.warnings = append(v.warnings, fmt.Sprintf("Failed to list Volume Groups to verify auto-creation on '%s': %v", activeViosName, vgErr))
				continue
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
						v.log.Warn(fmt.Sprintf("Warning: Will use '%s' for Media Repository as no other VG has %.2f GB free", vg.GroupName, requiredGB))
						break
					}
				}
			}

			if targetVG == "" {
				v.errors = append(v.errors, fmt.Sprintf("Cannot auto-create Media Repository on VIOS '%s': No Volume Group found with at least %.2f GB of free space.", activeViosName, requiredGB))
				continue
			}

			v.log.Info(fmt.Sprintf("✓ Sufficient space found in VG '%s'. ShiftLaunch will auto-create the repository during deployment.", targetVG))
			continue
		}

		// 3. If repository already exists, validate against ACTUAL required space, not the bloated creation minimum!
		v.log.Info(fmt.Sprintf("Repository Size: %d MB | Free: %d MB | Actually Required: %d MB (%d nodes)",
			repoInfo.SizeMB, repoInfo.FreeMB, actualRequiredMB, count))

		if repoInfo.FreeMB < actualRequiredMB {
			v.errors = append(v.errors, fmt.Sprintf(
				"VIOS MEDIA REPOSITORY FULL: VIOS '%s' on system '%s' only has %d MB free, but %d MB is required.\n"+
					"   Solution 1: Clean up old ISOs via HMC (rmvopt).\n"+
					"   Solution 2: Expand the repository using 'chrep -size'.",
				activeViosName, systemName, repoInfo.FreeMB, actualRequiredMB))
		} else {
			v.log.Info(fmt.Sprintf("✓ Sufficient space available in VIOS Media Repository on '%s'", activeViosName))
		}
	}
}

// validateNodeIPsNotAlive pings every node IP to ensure it is not already in use by another machine
func (v *Validator) validateNodeIPsNotAlive(ctx context.Context) {
	v.log.Info("Checking network to ensure all node IPs are available (parallel ping)...")

	var wg sync.WaitGroup
	var mu sync.Mutex
	conflictErrors := []string{}

	for _, node := range v.cfg.GetAllNodes() {
		if node.IP == "" {
			continue
		}

		wg.Add(1)
		go func(n *types.NodeConfig) {
			defer wg.Done()

			// Ping the IP with 2 packets and 2 second timeout
			// If the command SUCCEEDS (exit code 0), it means the IP answered us!
			pingCmd := fmt.Sprintf("ping -c 2 -W 2 %s >/dev/null 2>&1", n.IP)

			if _, err := v.exec.Execute(ctx, pingCmd); err == nil {
				mu.Lock()
				conflictErrors = append(conflictErrors, fmt.Sprintf(
					"IP CONFLICT: The IP %s (assigned to %s) is already actively responding on the network! Please choose an unused IP.",
					n.IP, n.Hostname))
				mu.Unlock()
			}
		}(node)
	}

	// Wait for all pings to complete
	wg.Wait()

	// Add all collected errors to validator errors
	v.errors = append(v.errors, conflictErrors...)
}
// isPreReleaseVersion returns true if the version string contains any pre-release
// marker that would not have a stable path on mirror.openshift.com.
// Examples: 4.21.0-ec.1, 4.21.0-rc.2, 4.21.0-candidate, 4.21.0-0.nightly-2025-01-01
func isPreReleaseVersion(version string) bool {
	lower := strings.ToLower(version)
	for _, marker := range []string{"ec", "rc", "candidate", "nightly", "pre", "alpha", "beta"} {
		if strings.Contains(lower, "-"+marker) {
			return true
		}
	}
	return false
}
