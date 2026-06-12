package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

var infoJSON bool

var infoCmd = &cobra.Command{
	Use:     "info",
	Aliases: []string{"inf", "status", "inspect"},
	Short:   "Show cluster deployment status and endpoints",
	GroupID: "core",
	Long: `Displays the current deployment state, URLs, and credentials of a managed cluster.

The info command shows:
- Deployment phase and progress
- Cluster endpoints (API, Console)
- Node status
- Credentials and access information`,
	RunE: runInfo,
}
// init function is for initializing the command
func init() {
	rootCmd.AddCommand(infoCmd)
	infoCmd.Flags().BoolVar(&infoJSON, "json", false, "Output curated cluster info in JSON format")
}
// runInfo function is for running the info command
func runInfo(cmd *cobra.Command, args []string) error {
	// Grab daemonCfg so we can accurately resolve the workspace directory for the credentials
	cfg, daemonCfg, orch, err := loadConfig(true)
	if err != nil {
		return err
	}
	defer orch.GetLogger().Close()

	ctx := GetContext()
	log := orch.GetLogger()

	// INTERCEPT: If --json is passed, construct a curated API-style JSON object
	if infoJSON {
		stateManager := types.NewStateManager(cfg.OpenShift.ClusterName)
		state, err := stateManager.LoadState()
		if err != nil {
			return fmt.Errorf("failed to load state: %w", err)
		}

		clusterType := "Multi-Node"
		if cfg.IsSNO() {
			clusterType = "SNO"
		}

		// Calculate deployment duration dynamically
		duration := "N/A"
		if state.StartTime != "" {
			startTime, err := time.Parse(time.RFC3339, state.StartTime)
			if err == nil {
				var d time.Duration
				if state.EndTime != "" {
					endTime, err := time.Parse(time.RFC3339, state.EndTime)
					if err == nil {
						d = endTime.Sub(startTime)
					}
				} else if state.Status == "in_progress" {
					d = time.Since(startTime)
				}

				if d > 0 {
					hours := int(d.Hours())
					minutes := int(d.Minutes()) % 60
					seconds := int(d.Seconds()) % 60
					if hours > 0 {
						duration = fmt.Sprintf("%dh %dm", hours, minutes)
					} else if minutes > 0 {
						duration = fmt.Sprintf("%dm %ds", minutes, seconds)
					} else {
						duration = fmt.Sprintf("%ds", seconds)
					}
					// Add an asterisk if it's still running
					if state.Status == "in_progress" {
						duration += "*"
					}
				}
			}
		}

		// Build the node array
		var nodes []map[string]string
		for _, node := range state.DiscoveredNodes {
			nodes = append(nodes, map[string]string{
				"hostname":    node.Hostname,
				"role":        node.Role,
				"ip":          node.IP,
				"mac_address": node.MACAddress,
			})
		}

		// Evaluate Proxy Configuration
		proxyMode, proxyURL := "none", ""
		if cfg.ManagedServices.Proxy {
			proxyMode = "managed"
			proxyURL = fmt.Sprintf("http://%s:3128", cfg.Controller.IP)
		} else if cfg.ExternalProxy.HTTPProxy != "" {
			proxyMode = "external"
			proxyURL = cfg.ExternalProxy.HTTPProxy
		}

		// Evaluate Registry Configuration
		registryMode, registryURL := "official", ""
		if cfg.ManagedServices.Registry {
			registryMode = "managed"
			registryURL = fmt.Sprintf("%s:5000", cfg.Controller.IP)
		} else if cfg.DisconnectedConfig.Enabled {
			registryMode = "external"
			registryURL = cfg.DisconnectedConfig.RegistryHostname
		}

		// Build the highly enriched core JSON response
		output := map[string]interface{}{
			"name":             state.ClusterName,
			"status":           state.Status,
			"phase":            state.CurrentPhase,
			"services": map[string]interface{}{
				"managed_dns":           cfg.ManagedServices.DNS,
				"managed_dhcp":          cfg.ManagedServices.DHCP && cfg.Nodes.BootMethod != "agent",
				"managed_pxe":           cfg.ManagedServices.PXE && cfg.Nodes.BootMethod != "agent",
				"managed_load_balancer": cfg.ManagedServices.LoadBalancer,
				"managed_nfs":           cfg.ManagedServices.NFS && cfg.Nodes.BootMethod == "agent",
				"proxy_mode":            proxyMode,
				"proxy_url":             proxyURL,
				"registry_mode":         registryMode,
				"registry_url":          registryURL,
			},
			"type":             clusterType,
			"ocp_version":      cfg.OpenShift.Version,
			"boot_method":      cfg.Nodes.BootMethod,
			"base_domain":      cfg.OpenShift.BaseDomain,
			"machine_cidr":     cfg.Network.MachineCIDR,
			"cluster_cidr":     cfg.OpenShift.ClusterNetworkCIDR,
			"start_time":       state.StartTime,
			"end_time":         state.EndTime,
			"duration":         duration,
			"completed_phases": state.CompletedPhases,
			"resume_count":     state.ResumeCount,
			"nodes":            nodes,
		}

		// If the cluster is completed, inject the endpoints and credentials!
		if state.Status == "completed" {
			baseDomain := cfg.OpenShift.BaseDomain
			clusterDomain := fmt.Sprintf("%s.%s", state.ClusterName, baseDomain)
			vip := cfg.Network.LoadBalancerIP

			output["endpoints"] = map[string]string{
				"api":        fmt.Sprintf("https://api.%s:6443", clusterDomain),
				"console":    fmt.Sprintf("https://console-openshift-console.apps.%s", clusterDomain),
				"oauth":      fmt.Sprintf("https://oauth-openshift.apps.%s", clusterDomain),
				"prometheus": fmt.Sprintf("https://prometheus-k8s-openshift-monitoring.apps.%s", clusterDomain),
				"grafana":    fmt.Sprintf("https://grafana-openshift-monitoring.apps.%s", clusterDomain),
			}

			credentials := make(map[string]string)
			workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, state.ClusterName)
			kubeconfigPath := filepath.Join(workspaceDir, "install-dir", "auth", "kubeconfig")
			pwPath := filepath.Join(workspaceDir, "install-dir", "auth", "kubeadmin-password")

			if _, err := os.Stat(kubeconfigPath); err == nil {
				credentials["kubeconfig"] = kubeconfigPath
			}
			if pwData, err := os.ReadFile(pwPath); err == nil {
				credentials["password"] = string(pwData)
			}
			output["credentials"] = credentials

			output["hosts_entry"] = fmt.Sprintf("%s api.%s console-openshift-console.apps.%s oauth-openshift.apps.%s prometheus-k8s-openshift-monitoring.apps.%s grafana-openshift-monitoring.apps.%s",
				vip, clusterDomain, clusterDomain, clusterDomain, clusterDomain, clusterDomain)
		}

		jsonData, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(jsonData))
		return nil
	}

	// Human-readable output logic
	clusterName := cfg.OpenShift.ClusterName
	log.Debug("Checking cluster information", "cluster", clusterName)

	status := orch.GetClusterStatus(ctx)
	fmt.Print(status)

	return nil
}
