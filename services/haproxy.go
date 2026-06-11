package services

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
	"time"

	"github.ibm.com/sudeeshjohn/shiftlaunch/localexec"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

const haproxyTemplate = `# ==========================================
# Cluster: {{.ClusterName}}
# Type: {{.Type}}
# OCP Version: {{.OCPVersion}}
# VIP: {{.VIP}} (Single VIP for all services)
# Generated: {{.Timestamp}}
# ==========================================

defaults
    mode                    tcp
    log                     global
    option                  tcplog
    timeout connect         10s
    timeout client          1h
    timeout server          1h

# API Server (Port 6443)
listen {{.ClusterName}}-openshift-api-server
    bind {{.VIP}}:6443
    mode tcp
    option tcplog
    balance roundrobin
{{- if .IsSNO}}
    server {{.SNONode.Hostname}} {{.SNONode.IP}}:6443 check
{{- else}}
{{- if .Bootstrap}}
    server {{.Bootstrap.Hostname}} {{.Bootstrap.IP}}:6443 check inter 1s backup
{{- else if .IsISO}}
    {{- if .Masters}}
    server bootstrap {{(index .Masters 0).IP}}:6443 check inter 1s backup
    {{- end}}
{{- end}}
{{- range .Masters}}
    server {{.Hostname}} {{.IP}}:6443 check
{{- end}}
{{- end}}

# Machine Config Server (Port 22623)
listen {{.ClusterName}}-machine-config-server
    bind {{.VIP}}:22623
    mode tcp
    option tcplog
    balance roundrobin
{{- if .IsSNO}}
    server {{.SNONode.Hostname}} {{.SNONode.IP}}:22623 check
{{- else}}
{{- if .Bootstrap}}
    server {{.Bootstrap.Hostname}} {{.Bootstrap.IP}}:22623 check inter 1s backup
{{- else if .IsISO}}
    {{- if .Masters}}
    server bootstrap {{(index .Masters 0).IP}}:22623 check inter 1s backup
    {{- end}}
{{- end}}
{{- range .Masters}}
    server {{.Hostname}} {{.IP}}:22623 check
{{- end}}
{{- end}}

# Ingress HTTP (Port 80)
listen {{.ClusterName}}-ingress-http
    bind {{.VIP}}:80
    mode tcp
    option tcplog
    balance roundrobin
{{- if .IsSNO}}
    server {{.SNONode.Hostname}}-http {{.SNONode.IP}}:80 check
{{- else}}
{{- if .Workers}}
{{- range .Workers}}
    server {{.Hostname}}-http-router0 {{.IP}}:80 check
{{- end}}
{{- else}}
{{- range .Masters}}
    server {{.Hostname}}-http-router0 {{.IP}}:80 check
{{- end}}
{{- end}}
{{- end}}

# Ingress HTTPS (Port 443)
listen {{.ClusterName}}-ingress-https
    bind {{.VIP}}:443
    mode tcp
    option tcplog
    balance roundrobin
{{- if .IsSNO}}
    server {{.SNONode.Hostname}}-https {{.SNONode.IP}}:443 check
{{- else}}
{{- if .Workers}}
{{- range .Workers}}
    server {{.Hostname}}-https-router0 {{.IP}}:443 check
{{- end}}
{{- else}}
{{- range .Masters}}
    server {{.Hostname}}-https-router0 {{.IP}}:443 check
{{- end}}
{{- end}}
{{- end}}
`

// HAProxyGenerator generates HAProxy configuration for a cluster
type HAProxyGenerator struct {
	cfg   *types.AgentConfig
	debug bool
}

// NewHAProxyGenerator creates a new HAProxy generator
func NewHAProxyGenerator(cfg *types.AgentConfig, debug bool) *HAProxyGenerator {
	return &HAProxyGenerator{
		cfg:   cfg,
		debug: debug,
	}
}

// Generate generates the complete HAProxy configuration
func (h *HAProxyGenerator) Generate() (string, error) {
	tmpl, err := template.New("haproxy").Parse(haproxyTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse haproxy template: %w", err)
	}

	cfg := h.cfg

	// Ensure SNO node hostname is populated (fallback to cluster name if empty)
	var snoNode *types.NodeConfig
	
	// SAFE BOUNDS CHECK: Explicitly verify the slice has elements before accessing index 0
	if len(cfg.Nodes.SNO) > 0 {
		// --- FIX: Use a pointer so the modification persists globally ---
		snoNode = &cfg.Nodes.SNO[0]
		if snoNode.Hostname == "" {
			snoNode.Hostname = cfg.OpenShift.ClusterName
		}
	}

	clusterType := "Multi-Node"
	if cfg.IsSNO() {
		clusterType = "SNO"
	}

	// Create data structure for the template mapped to the new AgentConfig arrays
	data := struct {
		ClusterName string
		Type        string
		OCPVersion  string
		VIP         string
		Timestamp   string
		IsSNO       bool
		IsISO       bool
		SNONode     *types.NodeConfig
		Bootstrap   *types.NodeConfig
		Masters     []types.NodeConfig
		Workers     []types.NodeConfig
	}{
		ClusterName: cfg.OpenShift.ClusterName,
		Type:        clusterType,
		OCPVersion:  cfg.OpenShift.Version,
		VIP:         cfg.Network.LoadBalancerIP,
		Timestamp:   time.Now().Format(time.RFC3339),
		IsSNO:       cfg.IsSNO(),
		IsISO:       cfg.Nodes.BootMethod == "agent",
		SNONode:     snoNode,
	}

	if !data.IsSNO {
		// Pass the first instance of Bootstrap down
		if len(cfg.Nodes.Bootstrap) > 0 {
			data.Bootstrap = &cfg.Nodes.Bootstrap[0]
		}
		if len(cfg.Nodes.Masters) > 0 {
			data.Masters = cfg.Nodes.Masters
		}
		if len(cfg.Nodes.Workers) > 0 {
			data.Workers = cfg.Nodes.Workers
		}
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute haproxy template: %w", err)
	}

	return buf.String(), nil
}

// GetConfigPath returns the path where this config should be written
func (h *HAProxyGenerator) GetConfigPath(ctx context.Context) string {
	return fmt.Sprintf("/etc/haproxy/conf.d/10-%s.cfg", h.cfg.OpenShift.ClusterName)
}

// SetupHAProxy connects the generator to local execution for the Orchestrator
func SetupHAProxy(ctx context.Context,cfg *types.AgentConfig, exec *localexec.LocalClient) error {
	gen := NewHAProxyGenerator(cfg, false)
	
	configContent, err := gen.Generate()
	if err != nil {
		return err
	}

	configPath := gen.GetConfigPath(ctx)
	
	// NEW: Ensure the HAProxy conf.d directory actually exists before moving files
	exec.Execute(ctx,"sudo mkdir -p /etc/haproxy/conf.d")
	
	// THE FIX: Nuke the default boilerplate frontend/backends that hog port 5000
	exec.Execute(ctx, "sudo sed -i '/frontend main/,$d' /etc/haproxy/haproxy.cfg")
	
	// Write HAProxy configuration locally
	if err := exec.WriteFile(ctx, configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to write HAProxy config to %s: %w", configPath, err)
	}

	// Reload/Restart HAProxy service
	if err := exec.SystemctlRestart(ctx,"haproxy"); err != nil {
		return fmt.Errorf("failed to restart HAProxy: %w", err)
	}

	return nil
}