package types

import (
	"fmt"
)

// AgentConfig represents the root of the shiftlaunch config.yaml
type AgentConfig struct {
	ManagedServices    ManagedServicesConfig    `yaml:"managed_services"`
	Controller         ControllerConfig         `yaml:"controller"`
	HMC                HMCConfig                `yaml:"hmc"`
	Network            NetworkConfig            `yaml:"network"`
	OpenShift          OpenShiftConfig          `yaml:"openshift"`
	Nodes              ClusterNodesConfig       `yaml:"nodes"`
	DisconnectedConfig DisconnectedConfig       `yaml:"disconnected,omitempty"`
}

// ManagedServicesConfig defines which infrastructure services are managed locally by ShiftLaunch
type ManagedServicesConfig struct {
	DNS          bool `yaml:"dns"`
	DHCP         bool `yaml:"dhcp"`
	PXE          bool `yaml:"pxe"`
	LoadBalancer bool `yaml:"load_balancer"`
	NFS          bool `yaml:"nfs"`
	Proxy        bool `yaml:"proxy"`
	Registry     bool `yaml:"registry"`
}

// DisconnectedConfig holds configuration for disconnected/airgapped deployments
type DisconnectedConfig struct {
	Enabled          bool   `yaml:"enabled"`
	ReleaseType      string `yaml:"release_type,omitempty"` // "official" (default) or "ci"
	RegistryImage    string `yaml:"registry_image"`
	RegistryHostname string `yaml:"registry_hostname,omitempty"`
	RegistryUsername string `yaml:"registry_username"`
	RegistryPassword string `yaml:"registry_password"`
	RegistryCAFile   string `yaml:"registry_ca_file,omitempty"`
	AutoMirror       bool   `yaml:"auto_mirror"`
	ReleaseImage     string `yaml:"release_image"`
	LocalRepo        string `yaml:"local_repo"`
}

// ControllerConfig defines the controller (bastion) node configuration
type ControllerConfig struct {
	NetworkInterface string `yaml:"network_interface"`
	IP               string `yaml:"ip,omitempty"` // Allow manual override
}

// HMCConfig defines IBM Hardware Management Console connection details
type HMCConfig struct {
	IP       string `yaml:"ip"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// NetworkConfig defines cluster network configuration
type NetworkConfig struct {
	LoadBalancerIP string   `yaml:"loadbalancer_ip"`
	MachineCIDR    string   `yaml:"machine_network_cidr"`
	Gateway        string   `yaml:"gateway"`
	Nameserver     string   `yaml:"nameserver,omitempty"`
	DNSForwarders  []string `yaml:"dns_forwarders"`
}

// OpenShiftConfig defines OpenShift cluster configuration and artifact URLs
type OpenShiftConfig struct {
	ClusterName        string          `yaml:"cluster_name"`
	Version            string          `yaml:"version"`
	ReleaseType        string          `yaml:"release_type"`
	BaseDomain         string          `yaml:"base_domain"`
	ClusterNetworkCIDR string          `yaml:"cluster_network_cidr"`
	HostPrefix         int             `yaml:"cluster_network_host_prefix"`
	ServiceNetwork     string          `yaml:"service_network"`
	PullSecretFile     string          `yaml:"pull_secret_file"`
	SSHPublicKeyFile   string          `yaml:"ssh_public_key_file"`
	ForceOCPDownload   bool            `yaml:"force_ocp_download,omitempty"`
	RHCOSImages        RHCOSURLs       `yaml:"rhcos_images"`
	OCPClientConfig    OCPClientConfig `yaml:"ocp_client_config"`
}

// RHCOSURLs defines Red Hat CoreOS image URLs and checksums
type RHCOSURLs struct {
	KernelURL     string `yaml:"kernel_url"`
	KernelCSUM    string `yaml:"kernel_csum,omitempty"`
	InitramfsURL  string `yaml:"initramfs_url"`
	InitramfsCSUM string `yaml:"initramfs_csum,omitempty"`
	RootfsURL     string `yaml:"rootfs_url"`
	RootfsCSUM    string `yaml:"rootfs_csum,omitempty"`
	ChecksumURL   string `yaml:"checksum_url,omitempty"`
	ISOURL        string `yaml:"iso_url,omitempty"`
}

// OCPClientConfig defines OpenShift client tool URLs and checksums
type OCPClientConfig struct {
	Client           string `yaml:"ocp_client"`
	ClientCSUM       string `yaml:"client_csum,omitempty"`
	Installer        string `yaml:"ocp_installer"`
	InstallerCSUM    string `yaml:"installer_csum,omitempty"`
	MirrorClient     string `yaml:"ocp_mirror_client,omitempty"`
	MirrorClientCSUM string `yaml:"mirror_client_csum,omitempty"`
	ChecksumURL      string `yaml:"checksum_url,omitempty"`
}

// ClusterNodesConfig defines cluster topology and node configurations
type ClusterNodesConfig struct {
	BootMethod string       `yaml:"boot_method"`
	SNO        []NodeConfig `yaml:"sno,omitempty"`
	Bootstrap  []NodeConfig `yaml:"bootstrap,omitempty"`
	Masters    []NodeConfig `yaml:"masters,omitempty"`
	Workers    []NodeConfig `yaml:"workers,omitempty"`
}

// NodeConfig defines individual node configuration and runtime metadata
type NodeConfig struct {
	Hostname         string `yaml:"name"`
	IP               string `yaml:"ip"`
	ExistingLPARName string `yaml:"existing_lpar_name"`
	SystemName       string `yaml:"system_name"`
	MACAddress       string `yaml:"mac_address,omitempty"` // Optional manual override

	// Runtime populated fields discovered via HMC API
	LocationCode string `yaml:"-"`
	ProfileUUID  string `yaml:"-"`
	UUID         string `yaml:"-"`
	Role         string `yaml:"-"` // e.g., "bootstrap", "master", "worker"
}

// IsSNO dynamically determines if the cluster is Single Node OpenShift based on the YAML structure
func (c *AgentConfig) IsSNO() bool {
	return len(c.Nodes.SNO) > 0
}

// GetAllNodes returns pointers to all node configs so the Orchestrator/HMC can inject MAC addresses
// If SNO is defined, it ONLY returns SNO nodes and ignores bootstrap/masters/workers
func (c *AgentConfig) GetAllNodes() []*NodeConfig {
	var nodes []*NodeConfig
	
	// If SNO is defined, ONLY return SNO nodes
	if c.IsSNO() {
		for i := range c.Nodes.SNO {
			c.Nodes.SNO[i].Role = "sno"
			nodes = append(nodes, &c.Nodes.SNO[i])
		}
		return nodes
	}
	
	// Otherwise, return multi-node topology
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

// Validate ensures structural integrity of the configuration
func (c *AgentConfig) Validate() error {
	if c.IsSNO() {
		if len(c.Nodes.Bootstrap) > 0 || len(c.Nodes.Masters) > 0 || len(c.Nodes.Workers) > 0 {
			return fmt.Errorf("configuration error: 'sno' block is defined, but 'bootstrap', 'masters', or 'workers' are also present")
		}
	} else {
		if len(c.Nodes.Bootstrap) == 0 || len(c.Nodes.Masters) == 0 {
			return fmt.Errorf("configuration error: multi-node deployments require at least 'bootstrap' and 'masters' blocks")
		}
	}
	return nil
}