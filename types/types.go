package types

import (
	"fmt"
)

// AgentConfig represents the root of the ShiftLaunch configuration.
type AgentConfig struct {
	Services  ServicesConfig     `yaml:"services"`
	Network   NetworkConfig      `yaml:"network"`
	OpenShift OpenShiftConfig    `yaml:"openshift"`
	HMC       HMCConfig          `yaml:"hmc"`
	Nodes     ClusterNodesConfig `yaml:"nodes"`
}

// ServicesConfig maps the structural infrastructure configurations.
// Services use pointer-based duck typing: if a service block is present in YAML,
// it's considered "requested". If external_* fields are empty, it's locally managed.
type ServicesConfig struct {
	DNS          *ServiceDNS          `yaml:"dns,omitempty"`
	DHCP         *ServiceDHCP         `yaml:"dhcp,omitempty"`
	PXE          *ServicePXE          `yaml:"pxe,omitempty"`
	LoadBalancer *ServiceLoadBalancer `yaml:"load_balancer,omitempty"`
	NFS          *ServiceNFS          `yaml:"nfs,omitempty"`
	Proxy        *ServiceProxy        `yaml:"proxy,omitempty"`
	Registry     *ServiceRegistry     `yaml:"registry,omitempty"`
}

// ServiceDNS represents the DNS service configuration.
type ServiceDNS struct {
	ExternalNameserver string `yaml:"external_nameserver,omitempty"`
}

// IsManaged returns true if DNS should be managed locally
// If the block is commented out (nil) OR empty, it defaults to TRUE (managed locally)
func (s *ServiceDNS) IsManaged() bool {
	if s == nil {
		return true // Default: Commented out means ShiftLaunch manages it locally!
	}
	return s.ExternalNameserver == ""
}

// GetExternal safely returns the external nameserver (empty if nil or not set)
func (s *ServiceDNS) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalNameserver
}

type ServiceDHCP struct {
	ExternalDHCPServer string `yaml:"external_dhcp_server,omitempty"`
}

// IsManaged returns true if DHCP should be managed locally
// If the block is commented out (nil) OR empty, it defaults to TRUE (managed locally)
func (s *ServiceDHCP) IsManaged() bool {
	if s == nil {
		return true // Default: Commented out means ShiftLaunch manages it locally!
	}
	return s.ExternalDHCPServer == ""
}

// GetExternal safely returns the external DHCP server
func (s *ServiceDHCP) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalDHCPServer
}

type ServicePXE struct {
	ExternalPXEServer string `yaml:"external_pxe_server,omitempty"`
}

// IsManaged returns true if PXE should be managed locally
// If the block is commented out (nil) OR empty, it defaults to TRUE (managed locally)
func (s *ServicePXE) IsManaged() bool {
	if s == nil {
		return true // Default: Commented out means ShiftLaunch manages it locally!
	}
	return s.ExternalPXEServer == ""
}

// GetExternal safely returns the external PXE server
func (s *ServicePXE) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalPXEServer
}

type ServiceLoadBalancer struct {
	VIP                  string `yaml:"vip,omitempty"`
	ExternalLoadBalancer string `yaml:"external_lb_ip,omitempty"`
}

// IsManaged returns true if LoadBalancer should be managed locally
// If the block is commented out (nil) OR empty, it defaults to TRUE (managed locally)
func (s *ServiceLoadBalancer) IsManaged() bool {
	if s == nil {
		return true // Default: Commented out means ShiftLaunch manages it locally!
	}
	return s.ExternalLoadBalancer == ""
}

// GetVIP safely returns the VIP address
func (s *ServiceLoadBalancer) GetVIP() string {
	if s == nil {
		return ""
	}
	return s.VIP
}

// GetExternal safely returns the external load balancer IP
func (s *ServiceLoadBalancer) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalLoadBalancer
}

type ServiceNFS struct {
	ExternalNFSServer string `yaml:"external_nfs_server,omitempty"`
	ExternalNFSPath   string `yaml:"external_nfs_path,omitempty"`
}

// IsManaged returns true if NFS should be managed locally
// If the block is commented out (nil) OR empty, it defaults to TRUE (managed locally)
func (s *ServiceNFS) IsManaged() bool {
	if s == nil {
		return true // Default: Commented out means ShiftLaunch manages it locally!
	}
	return s.ExternalNFSServer == ""
}

// GetExternal safely returns the external NFS server
func (s *ServiceNFS) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalNFSServer
}

// GetExternalPath safely returns the external NFS export path
func (s *ServiceNFS) GetExternalPath() string {
	if s == nil {
		return ""
	}
	return s.ExternalNFSPath
}

type ServiceProxy struct {
	ExternalHTTPProxy  string `yaml:"external_http_proxy,omitempty"`
	ExternalHTTPSProxy string `yaml:"external_https_proxy,omitempty"`
	NoProxy            string `yaml:"no_proxy,omitempty"`
}

// IsManaged returns true if Proxy should be managed locally
func (s *ServiceProxy) IsManaged() bool {
	return s != nil && s.ExternalHTTPProxy == ""
}

// GetHTTP safely returns the external HTTP proxy
func (s *ServiceProxy) GetHTTP() string {
	if s == nil {
		return ""
	}
	return s.ExternalHTTPProxy
}

// GetHTTPS safely returns the external HTTPS proxy
func (s *ServiceProxy) GetHTTPS() string {
	if s == nil {
		return ""
	}
	return s.ExternalHTTPSProxy
}

// GetNoProxy safely returns the no_proxy configuration
func (s *ServiceProxy) GetNoProxy() string {
	if s == nil {
		return ""
	}
	return s.NoProxy
}

type ServiceRegistry struct {
	AutoMirror       bool   `yaml:"-"` // Internal tracking field
	ExternalHostname string `yaml:"external_registry_server,omitempty"`
	Username         string `yaml:"username,omitempty"`
	Password         string `yaml:"password,omitempty"`
	CACertFile       string `yaml:"ca_cert_file,omitempty"`
	RegistryImage    string `yaml:"-"` // Managed runtime tracking
	ReleaseImage     string `yaml:"-"` // Managed runtime tracking
	LocalRepo        string `yaml:"-"` // Managed runtime tracking
}

// IsManaged returns true if Registry should be managed locally
func (s *ServiceRegistry) IsManaged() bool {
	return s != nil && s.ExternalHostname == ""
}

// GetExternal safely returns the external registry hostname
func (s *ServiceRegistry) GetExternal() string {
	if s == nil {
		return ""
	}
	return s.ExternalHostname
}

// GetUser safely returns the registry username
func (s *ServiceRegistry) GetUser() string {
	if s == nil {
		return ""
	}
	return s.Username
}

// GetPass safely returns the registry password
func (s *ServiceRegistry) GetPass() string {
	if s == nil {
		return ""
	}
	return s.Password
}

// GetCACert safely returns the CA certificate file path
func (s *ServiceRegistry) GetCACert() string {
	if s == nil {
		return ""
	}
	return s.CACertFile
}

// NetworkConfig holds layout bounds and localization assignments.
type NetworkConfig struct {
	IsolationLevel      string   `yaml:"isolation_level"` // "connected", "restricted-network", "air-gapped"
	ControllerInterface string   `yaml:"controller_interface"`
	MachineCIDR         string   `yaml:"machine_network_cidr"`
	Gateway             string   `yaml:"gateway"`
	UpstreamNameservers []string `yaml:"dns_forwarders,omitempty"` // Moved from ServiceDNS
	ControllerIP        string   `yaml:"-"`                        // Auto-Discovered
}

type HMCConfig struct {
	IP       string `yaml:"ip"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type OpenShiftConfig struct {
	ClusterName        string          `yaml:"cluster_name"`
	Version            string          `yaml:"version"`
	ReleaseType        string          `yaml:"release_type"`
	BaseDomain         string          `yaml:"base_domain"`
	ClusterNetworkCIDR string          `yaml:"cluster_network_cidr,omitempty"`
	HostPrefix         int             `yaml:"cluster_network_host_prefix,omitempty"`
	ServiceNetwork     string          `yaml:"service_network,omitempty"`
	PullSecretFile     string          `yaml:"pull_secret_file"`
	SSHPublicKeyFile   string          `yaml:"ssh_public_key_file"`
	ForceOCPDownload   bool            `yaml:"force_ocp_download,omitempty"`
	RHCOSImages        RHCOSURLs       `yaml:"rhcos_images,omitempty"`
	OCPClientConfig    OCPClientConfig `yaml:"ocp_client_config,omitempty"`
}

type RHCOSURLs struct {
	KernelURL    string `yaml:"kernel_url,omitempty"`
	InitramfsURL string `yaml:"initramfs_url,omitempty"`
	RootfsURL    string `yaml:"rootfs_url,omitempty"`
	ChecksumURL  string `yaml:"checksum_url,omitempty"`
}

type OCPClientConfig struct {
	Client       string `yaml:"ocp_client,omitempty"`
	Installer    string `yaml:"ocp_installer,omitempty"`
	MirrorClient string `yaml:"ocp_mirror_client,omitempty"`
	ChecksumURL  string `yaml:"checksum_url,omitempty"`
}

type ClusterNodesConfig struct {
	BootMethod string       `yaml:"boot_method"`
	Bootstrap  []NodeConfig `yaml:"bootstrap,omitempty"`
	Masters    []NodeConfig `yaml:"masters,omitempty"`
	Workers    []NodeConfig `yaml:"workers,omitempty"`
}

type NodeConfig struct {
	Hostname         string `yaml:"name"`
	IP               string `yaml:"ip"`
	ExistingLPARName string `yaml:"existing_lpar_name"`
	SystemName       string `yaml:"system_name"`
	MACAddress       string `yaml:"mac_address,omitempty"`
	LocationCode     string `yaml:"-"`
	ProfileUUID      string `yaml:"-"`
	UUID             string `yaml:"-"`
	Role             string `yaml:"-"`
}

// IsSNO dynamically computes if the target topology forms an SNO pattern based purely on masters count.
func (c *AgentConfig) IsSNO() bool {
	return len(c.Nodes.Masters) == 1
}

// GetAllNodes aggregates node references, completely isolating the inventory track if SNO rules trigger.
func (c *AgentConfig) GetAllNodes() []*NodeConfig {
	var nodes []*NodeConfig

	if c.IsSNO() {
		if len(c.Nodes.Masters) > 0 {
			c.Nodes.Masters[0].Role = "sno"
			nodes = append(nodes, &c.Nodes.Masters[0])
		}
		return nodes
	}

	for i := range c.Nodes.Bootstrap {
		c.Nodes.Bootstrap[i].Role = "bootstrap"
		nodes = append(nodes, &c.Nodes.Bootstrap[i])
	}
	for i := range c.Nodes.Masters {
		c.Nodes.Masters[i].Role = "master"
		nodes = append(nodes, &c.Nodes.Masters[i])
	}
	for i := range c.Nodes.Workers {
		c.Nodes.Workers[i].Role = "worker"
		nodes = append(nodes, &c.Nodes.Workers[i])
	}

	return nodes
}

// Validate processes configuration layout rules before triggering pipelines.
func (c *AgentConfig) Validate() error {
	if c.IsSNO() {
		if len(c.Nodes.Workers) > 0 || len(c.Nodes.Bootstrap) > 0 {
			return fmt.Errorf("topology conflict: exactly 1 master node is defined, triggering Single Node OpenShift (SNO) mode. However, you still have targets listed under 'workers' or 'bootstrap'. Please remove those extra nodes to proceed")
		}
		return nil
	}

	if len(c.Nodes.Masters) < 3 {
		return fmt.Errorf("quorum error: multi-node clusters require a minimum of 3 control plane 'masters' nodes, found %d", len(c.Nodes.Masters))
	}
	if len(c.Nodes.Masters)%2 == 0 {
		return fmt.Errorf("quorum error: master node count must be an odd number to maintain etcd quorum, found %d", len(c.Nodes.Masters))
	}
	if c.Nodes.BootMethod == "netboot" && len(c.Nodes.Bootstrap) == 0 {
		return fmt.Errorf("topology error: netboot environments require exactly 1 target assigned within the 'bootstrap' parameter block")
	}
	if c.Nodes.BootMethod == "agent" && len(c.Nodes.Bootstrap) > 0 {
		return fmt.Errorf("topology error: agent boot architectures do not use a standalone bootstrap node. Please remove the 'bootstrap' block")
	}
	return nil
}

// Made with Bob
