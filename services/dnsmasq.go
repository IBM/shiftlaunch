package services

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/sudeeshjohn/shiftlaunch/config"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/types"
)

// =============================================================================
// TEMPLATES (Strictly Preserved with Original Comments)
// =============================================================================

// DNS-only configuration template
const dnsConfigTemplate = `# ============================================
# DNS Configuration for Cluster: {{.ClusterName}}
# Type: {{.Type}}
# OCP Version: {{.OCPVersion}}
# Generated: {{.Timestamp}}
# ============================================

# Prevent infinite loop: Do NOT read /etc/resolv.conf
# (Critical when helper node points to itself as DNS server)
no-resolv

# Explicit upstream DNS forwarders
{{- range .DNSForwarders}}
server={{.}}
{{- end}}

# DNS A Records for cluster nodes
{{- range .Nodes}}
address=/{{.Hostname}}/{{.IP}}
{{- end}}

# DNS A Records for OpenShift services
address=/api.{{.ClusterName}}.{{.BaseDomain}}/{{.VIP}}
{{- if .IsSNO}}
address=/api-int.{{.ClusterName}}.{{.BaseDomain}}/{{.IP}}
{{- else}}
address=/api-int.{{.ClusterName}}.{{.BaseDomain}}/{{.VIP}}
{{- end}}
address=/.apps.{{.ClusterName}}.{{.BaseDomain}}/{{.VIP}}

{{- if not .IsSNO}}
# etcd A Records
{{- range $index, $master := .Masters}}
address=/etcd-{{$index}}.{{$.ClusterName}}.{{$.BaseDomain}}/{{.IP}}
{{- end}}

# etcd SRV Records (for multi-node clusters only)
{{- range $index, $master := .Masters}}
srv-host=_etcd-server-ssl._tcp.{{$.ClusterName}}.{{$.BaseDomain}},etcd-{{$index}}.{{$.ClusterName}}.{{$.BaseDomain}},2380,0,{{$index}}
{{- end}}
{{- end}}
`

// DHCP-only configuration template
const dhcpConfigTemplate = `# ============================================
# DHCP Configuration for Cluster: {{.ClusterName}}
# Type: {{.Type}}
# OCP Version: {{.OCPVersion}}
# Generated: {{.Timestamp}}
# ============================================

# Network Bindings & Logging
interface={{.Interface}}
log-dhcp
dhcp-authoritative

# Static Subnet Definition (covers the whole {{.NetworkCIDR}} network)
dhcp-range={{.NetworkAddr}},static,{{.Netmask}},12h

# Network Options
dhcp-option=tag:{{.ClusterName}},option:router,{{.Gateway}}
dhcp-option=tag:{{.ClusterName}},option:dns-server,{{.DNSServer}}
dhcp-option=tag:{{.ClusterName}},option:domain-name,{{.ClusterName}}.{{.BaseDomain}}

# PXE Boot Options (Next-Server / Option 66 & 67)
dhcp-boot=tag:{{.ClusterName}},{{.ClusterName}}/core.elf,,{{.PXEServer}}

# Static DHCP assignments with MAC-to-IP bindings
{{- range .Nodes}}
{{- if .MACAddress}}
dhcp-host={{.MACAddress}},set:{{$.ClusterName}},{{.IP}},{{.Hostname}},infinite
{{- else}}
# MAC Address pending. Run the 'create_lpars' phase first to generate it!
# dhcp-host=<mac-address>,set:{{$.ClusterName}},{{.IP}},{{.Hostname}},infinite
{{- end}}
{{- end}}
`

// PXE/TFTP-only configuration template
const pxeConfigTemplate = `# ============================================
# PXE/TFTP Configuration for Cluster: {{.ClusterName}}
# Type: {{.Type}}
# OCP Version: {{.OCPVersion}}
# Generated: {{.Timestamp}}
# ============================================

# Network Interface Binding
interface={{.Interface}}

# TFTP/PXE Boot Configuration
enable-tftp
tftp-root=/var/lib/tftpboot
dhcp-boot=tag:{{.ClusterName}},{{.ClusterName}}/core.elf,,{{.PXEServer}}
`

const grubTemplate = `# GRUB2 Config for {{.Hostname}} (Cluster: {{.ClusterName}})
# MAC: {{.MACAddress}}, Role: {{.Role}}
set default=0
set timeout=10

menuentry '{{.MenuLabel}}' {
  linux {{.ClusterName}}/rhcos/kernel {{.KernelParams}}
  initrd {{.ClusterName}}/rhcos/initramfs.img
}
`

// =============================================================================
// CONSOLIDATED MANAGER LOGIC
// =============================================================================

type DNSmasqManager struct {
	cfg       *types.AgentConfig
	daemonCfg *config.AgentDaemonConfig
	executor  *localexec.LocalClient
}

func NewDNSmasqManager(cfg *types.AgentConfig, daemonCfg *config.AgentDaemonConfig, exec *localexec.LocalClient) *DNSmasqManager {
	return &DNSmasqManager{
		cfg:       cfg,
		daemonCfg: daemonCfg,
		executor:  exec,
	}
}

// SetupServices configures the system-level dnsmasq records (DNS, DHCP, PXE Flags)
func (m *DNSmasqManager) SetupServices(ctx context.Context) error {
	// Configure DNS records
	if err := m.writeAndRestart(ctx,"10", "dns", dnsConfigTemplate); err != nil {
		return err
	}
	// Configure DHCP reservations
	if err := m.writeAndRestart(ctx,"20", "dhcp", dhcpConfigTemplate); err != nil {
		return err
	}
	// Configure PXE service flags
	return m.writeAndRestart(ctx,"30", "pxe", pxeConfigTemplate)
}

// ConfigurePXEBoot handles the physical artifacts: grub2 structure, core.elf, images, and grub configs
func (m *DNSmasqManager) ConfigurePXEBoot(ctx context.Context,workspaceDir string) error {
	clusterName := m.cfg.OpenShift.ClusterName
	tftpRoot := m.daemonCfg.Paths.TFTPRoot
	clusterTftpDir := filepath.Join(tftpRoot, clusterName)

	// 1. Setup GRUB2 environment
	m.executor.Execute(ctx,fmt.Sprintf("sudo grub2-mknetdir --net-directory=%s", tftpRoot))
	m.executor.Execute(ctx,fmt.Sprintf("sudo mkdir -p %s/rhcos", clusterTftpDir))

	// 2. Stage bootloader for Power systems [cite: 812]
	m.executor.Execute(ctx,fmt.Sprintf("sudo cp %s/boot/grub2/powerpc-ieee1275/core.elf %s/", tftpRoot, clusterTftpDir))

	// 3. Stage RHCOS artifacts from local workspace [cite: 821]
	m.executor.Execute(ctx,fmt.Sprintf("sudo cp %s/rhcos/kernel %s/rhcos/initramfs.img %s/rhcos/", workspaceDir, workspaceDir, clusterTftpDir))

	// 4. Generate MAC-specific GRUB configs [cite: 814]
	tmpl, _ := template.New("grub").Parse(grubTemplate)
	for _, node := range m.cfg.GetAllNodes() {
		if node.MACAddress == "" {
			continue
		}

		macFile := "grub.cfg-01-" + strings.ToLower(strings.ReplaceAll(node.MACAddress, ":", "-"))
		destPath := filepath.Join(clusterTftpDir, macFile)

		data := m.prepareGrubData(ctx,node)
		var buf bytes.Buffer
		tmpl.Execute( &buf, data)

		if err := m.executor.WriteFile(ctx,destPath, buf.Bytes(), 0644); err != nil {
			return err
		}
	}

	// 5. Finalize permissions and SELinux [cite: 786, 822]
	m.executor.Execute(ctx,fmt.Sprintf("sudo chown -R nobody:nobody %s", clusterTftpDir))
	m.executor.Execute(ctx,fmt.Sprintf("sudo restorecon -Rv %s", clusterTftpDir))

	return m.executor.SystemctlRestart(ctx,"dnsmasq")
}

// --- Helper Functions ---

func (m *DNSmasqManager) writeAndRestart(ctx context.Context,prefix, name, tmplStr string) error {
	tmpl, err := template.New(name).Parse(tmplStr)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, m.prepareTemplateData(ctx)); err != nil {
		return err
	}

	path := fmt.Sprintf("/etc/dnsmasq.d/%s-%s-%s.conf", prefix, m.cfg.OpenShift.ClusterName, name)
	if err := m.executor.WriteFile(ctx, path, buf.Bytes(), 0644); err != nil {
		return err
	}

	return m.executor.SystemctlRestart(ctx,"dnsmasq")
}

func (m *DNSmasqManager) prepareTemplateData(ctx context.Context) map[string]interface{} {
	netConfig := m.cfg.Network
	isSno := m.cfg.IsSNO()

	networkAddr := netConfig.MachineCIDR
	if idx := strings.Index(networkAddr, "/"); idx > 0 {
		networkAddr = networkAddr[:idx]
	}

	calculatedNetmask := "255.255.255.0" // Safe fallback
	if _, ipnet, err := net.ParseCIDR(netConfig.MachineCIDR); err == nil {
		calculatedNetmask = net.IP(ipnet.Mask).String()
	}

	// Logic for DNSServer: If unmanaged, use customer's nameserver. Else, use local controller.
	dnsServer := m.cfg.Controller.IP
	if !m.cfg.ManagedServices.DNS && netConfig.Nameserver != "" {
		dnsServer = netConfig.Nameserver
	}

	// Logic for PXEServer: Default to controller IP. 
	// (If PXE is unmanaged but DHCP is managed, this should point to the customer's external PXE server).
	pxeServer := m.cfg.Controller.IP

	data := map[string]interface{}{
		"ClusterName":   m.cfg.OpenShift.ClusterName,
		"Type":          "UPI",
		"OCPVersion":    m.cfg.OpenShift.Version,
		"Timestamp":     time.Now().Format(time.RFC3339),
		"Interface":     m.cfg.Controller.NetworkInterface,
		"NetworkCIDR":   netConfig.MachineCIDR,
		"NetworkAddr":   networkAddr,
		"Netmask":       calculatedNetmask, // Common for Power network_cidr /20
		"Gateway":       netConfig.Gateway,
		"HelperIP":      m.cfg.Controller.IP,
		"DNSServer":     dnsServer, // <--- New Variable
		"PXEServer":     pxeServer, // <--- New Variable
		"BaseDomain":    m.cfg.OpenShift.BaseDomain,
		"VIP":           netConfig.LoadBalancerIP,
		"IsSNO":         isSno,
		"Nodes":         m.cfg.GetAllNodes(),
		"DNSForwarders": netConfig.DNSForwarders,
	}

	// Topology specific population
	if isSno && len(m.cfg.Nodes.SNO) > 0 {
		data["IP"] = m.cfg.Nodes.SNO[0].IP
		data["Type"] = "SNO"
	} else {
		data["Masters"] = m.cfg.Nodes.Masters
		data["Type"] = "Multi-Node"
	}

	return data
}

func (m *DNSmasqManager) prepareGrubData(ctx context.Context,node *types.NodeConfig) interface{} {
	role := node.Role
	ctrlIP := m.cfg.Controller.IP
	clusterName := m.cfg.OpenShift.ClusterName

	label := "Install Master"
	ign := "master.ign"

	var params []string
	if role == "sno" {
		label = "Install Single Node OpenShift"
		ign = "bootstrap.ign"
		params = []string{
			"ip=dhcp",
			"rd.neednet=1",
			"ignition.platform.id=metal",
			"ignition.firstboot",
			fmt.Sprintf("coreos.live.rootfs_url=http://%s:%d/%s/rhcos/rootfs.img", ctrlIP, m.daemonCfg.Network.HTTPPort, clusterName),
			fmt.Sprintf("coreos.inst.ignition_url=http://%s:%d/%s/ignition/%s", ctrlIP, m.daemonCfg.Network.HTTPPort, clusterName, ign),
		}
	} else {
		if role == "worker" {
			ign = "worker.ign"
			label = "Install Worker"
		} else if role == "bootstrap" {
			ign = "bootstrap.ign"
			label = "Install Bootstrap"
		}
		params = []string{
			fmt.Sprintf("initrd=%s/rhcos/initramfs.img", clusterName),
			"nomodeset",
			"rd.neednet=1",
			"ip=dhcp",
			"coreos.inst=yes",
			fmt.Sprintf("coreos.inst.install_dev=%s", m.daemonCfg.Paths.InstallDevice),
			fmt.Sprintf("coreos.live.rootfs_url=http://%s:%d/%s/rhcos/rootfs.img", ctrlIP, m.daemonCfg.Network.HTTPPort, clusterName),
			fmt.Sprintf("coreos.inst.ignition_url=http://%s:%d/%s/ignition/%s", ctrlIP, m.daemonCfg.Network.HTTPPort, clusterName, ign),
		}
	}

	return struct {
		Hostname, ClusterName, MACAddress, Role, MenuLabel, KernelParams string
	}{
		Hostname:     node.Hostname,
		ClusterName:  clusterName,
		MACAddress:   node.MACAddress,
		Role:         role,
		MenuLabel:    label,
		KernelParams: strings.Join(params, " "),
	}
}

// Cleanup removes all cluster-specific fragments and artifacts [cite: 824]
func (m *DNSmasqManager) Cleanup(ctx context.Context) {
	cluster := m.cfg.OpenShift.ClusterName
	m.executor.Execute(ctx,fmt.Sprintf("sudo rm -f /etc/dnsmasq.d/*-%s-*.conf", cluster))
	m.executor.Execute(ctx,fmt.Sprintf("sudo rm -rf /var/lib/tftpboot/%s", cluster))
	m.CleanupLeases(ctx)
	_ = m.executor.SystemctlRestart(ctx,"dnsmasq")
}

// CleanupLeases cleans the local lease file [cite: 442]
func (m *DNSmasqManager) CleanupLeases(ctx context.Context) {
	leaseFile := "/var/lib/dnsmasq/dnsmasq.leases"
	for _, node := range m.cfg.GetAllNodes() {
		if node.MACAddress != "" {
			mac := strings.ToLower(strings.ReplaceAll(node.MACAddress, "-", ":"))
			m.executor.Execute(ctx,fmt.Sprintf("sudo sed -i '/%s/d' %s", mac, leaseFile))
		}
		if node.IP != "" {
			m.executor.Execute(ctx,fmt.Sprintf("sudo sed -i '/%s/d' %s", node.IP, leaseFile))
		}
	}
}
// SetupDNS configures the DNS service records locally
func (m *DNSmasqManager) SetupDNS(ctx context.Context) error {
	return m.writeAndRestart(ctx,"10", "dns", dnsConfigTemplate)
}

// SetupDHCP configures DHCP reservations locally
func (m *DNSmasqManager) SetupDHCP(ctx context.Context) error {
	return m.writeAndRestart(ctx,"20", "dhcp", dhcpConfigTemplate)
}

// SetupPXEService configures the dnsmasq TFTP/PXE settings
func (m *DNSmasqManager) SetupPXEService(ctx context.Context) error {
	return m.writeAndRestart(ctx,"30", "pxe", pxeConfigTemplate)
}