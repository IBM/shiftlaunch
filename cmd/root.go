package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.ibm.com/sudeeshjohn/shiftlaunch/config"
	"github.ibm.com/sudeeshjohn/shiftlaunch/infra/controller"
	"github.ibm.com/sudeeshjohn/shiftlaunch/logger"
	"github.ibm.com/sudeeshjohn/shiftlaunch/orchestrator"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

const version = "0.3.0-byoi-agent"

var (
	// Global flags
	configFile  string
	clusterName string
	debug       bool

	// Root context and orchestrator (shared across commands)
	rootCtx context.Context
	cancel  context.CancelFunc
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "shiftlaunch",
	Short: "ShiftLaunch - Boot OpenShift clusters on IBM Power Systems",
	Long: `ShiftLaunch Local Agent - A tool for bootstrapping OpenShift clusters on IBM Power Systems.

ShiftLaunch automates the deployment of OpenShift clusters by managing:
- Network services (DNS, DHCP, PXE, Load Balancer)
- OpenShift installation and configuration`,
	Version: version,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	cobra.OnInitialize(initConfig)

	// Define Docker-style Command Groups
	rootCmd.AddGroup(&cobra.Group{
		ID:    "core",
		Title: "Core Commands:",
	})
	rootCmd.AddGroup(&cobra.Group{
		ID:    "utils",
		Title: "Utility Commands:",
	})

	// Global persistent flags
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "config.yaml", "Path to agent configuration file")
	rootCmd.PersistentFlags().StringVar(&clusterName, "cluster", "", "Cluster name override")
	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug output to terminal")

	// Make the CLI quiet on errors (Like Docker)
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true

	// Add dynamic completion for the --cluster flag
	rootCmd.RegisterFlagCompletionFunc("cluster", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Get list of existing clusters
		clusters, err := listClusters()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		return clusters, cobra.ShellCompDirectiveNoFileComp
	})

	// Setup graceful shutdown
	setupSignalHandler()
}

func initConfig() {
	// Configuration initialization happens per-command as needed
}

// setupSignalHandler sets up graceful shutdown on interrupt signals
func setupSignalHandler() {
	rootCtx, cancel = context.WithCancel(context.Background())

	// Buffer of 2 to catch rapid double-presses
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigCh
		pterm.Println()
		pterm.Warning.Println("Interrupt signal received!")
		pterm.Warning.Println("Attempting graceful shutdown (waiting for active VIOS/HMC operations to complete)...")
		pterm.Warning.Println("Press Ctrl+C again to forcefully terminate immediately (may cause VIOS corruption).")
		cancel()

		<-sigCh
		pterm.Println()
		pterm.Error.Println("Force quitting immediately!")
		os.Exit(1)
	}()
}

// GetContext returns the root context for commands
func GetContext() context.Context {
	return rootCtx
}

// loadConfig loads and validates the configuration file
func loadConfig(requireConfig bool) (*types.AgentConfig, *config.AgentDaemonConfig, *orchestrator.Orchestrator, error) {
	// Load daemon config
	daemonCfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to initialize daemon configuration: %w", err)
	}

	if !requireConfig {
		return nil, daemonCfg, nil, nil
	}

	// Determine config path
	configPath := configFile
	if clusterName != "" {
		workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, clusterName)
		
		// 1. SMART CHECK: Is the cluster explicitly marked as deleted?
		deletedMarker := filepath.Join(workspaceDir, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			return nil, nil, nil, fmt.Errorf("cluster '%s' has been deleted. Run 'shiftlaunch prune' to permanently remove its workspace", clusterName)
		}

		// 2. Prefer the workspace config if it exists
		workspaceConfig := filepath.Join(workspaceDir, "config.yaml")
		if _, err := os.Stat(workspaceConfig); err == nil {
			configPath = workspaceConfig
		} else if configFile == "config.yaml" {
			// 3. PRUNED CHECK: If workspace config is missing, and the local default config is missing, it's a ghost cluster!
			if _, err := os.Stat(configFile); os.IsNotExist(err) {
				return nil, nil, nil, fmt.Errorf("cluster '%s' not found. It may have been pruned or never created", clusterName)
			}
		}
	}

	// Load cluster config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read configuration file: %v\n(Hint: Provide a valid config.yaml or specify an existing cluster using --cluster)", err)
	}

	var cfg types.AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse YAML configuration: %w", err)
	}

	// ========================================================================
	// STRICT AIRGAP ENFORCEMENT
	// If the user disabled the proxy, actively sanitize the Go runtime environment
	// to prevent terminal ghost variables from breaking child processes.
	// ========================================================================
	if !cfg.ManagedServices.Proxy {
		proxyVars := []string{
			"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
			"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
		}
		for _, envVar := range proxyVars {
			os.Unsetenv(envVar)
		}
	}
	// ========================================================================

	// ========================================================================
	// UX OPTIMIZATION: Auto-fill Disconnected settings if empty
	// ========================================================================
	if cfg.ManagedServices.Registry {
		cfg.DisconnectedConfig.Enabled = true
		cfg.DisconnectedConfig.AutoMirror = true // Force mirror if managing registry
		
		if cfg.DisconnectedConfig.RegistryImage == "" {
			cfg.DisconnectedConfig.RegistryImage = "docker.io/library/registry:3.1.1"
		}
		if cfg.DisconnectedConfig.RegistryUsername == "" {
			cfg.DisconnectedConfig.RegistryUsername = "admin"
		}
		if cfg.DisconnectedConfig.RegistryPassword == "" {
			cfg.DisconnectedConfig.RegistryPassword = "admin"
		}
		if cfg.DisconnectedConfig.LocalRepo == "" {
			cfg.DisconnectedConfig.LocalRepo = "ocp4/openshift4"
		}
		// Default to official releases (uses oc-mirror v2)
		if cfg.DisconnectedConfig.ReleaseType == "" {
			cfg.DisconnectedConfig.ReleaseType = "official"
		}
		
		// Auto-generate the release_image based purely on the openshift.version field
		if cfg.DisconnectedConfig.ReleaseImage == "" && cfg.OpenShift.Version != "" {
			versionTag := cfg.OpenShift.Version
			// If they only put "4.21", default to "4.21.0". If they put "4.21.14", use it directly.
			if len(strings.Split(versionTag, ".")) == 2 {
				versionTag = versionTag + ".0"
			}
			cfg.DisconnectedConfig.ReleaseImage = fmt.Sprintf("quay.io/openshift-release-dev/ocp-release:%s-ppc64le", versionTag)
		}
	}
	// ========================================================================

	// ========================================================================
	// UX OPTIMIZATION 2: Auto-fill OpenShift mirror URLs if left empty!
	// ========================================================================
	if cfg.OpenShift.Version != "" {
		// Extract major.minor (e.g. "4.21" from "4.21.14") for RHCOS paths
		majorMinor := cfg.OpenShift.Version
		if parts := strings.Split(cfg.OpenShift.Version, "."); len(parts) >= 2 {
			majorMinor = parts[0] + "." + parts[1]
		}

		// Auto-fill Client and Installer tarballs
		if cfg.OpenShift.OCPClientConfig.Client == "" {
			cfg.OpenShift.OCPClientConfig.Client = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/clients/ocp/%s/openshift-client-linux.tar.gz", cfg.OpenShift.Version)
		}
		if cfg.OpenShift.OCPClientConfig.Installer == "" {
			cfg.OpenShift.OCPClientConfig.Installer = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/clients/ocp/%s/openshift-install-linux.tar.gz", cfg.OpenShift.Version)
		}
		
		// Auto-fill RHCOS URLs (Only needed for Netboot, but safe to generate)
		if cfg.Nodes.BootMethod != "agent" {
			if cfg.OpenShift.RHCOSImages.KernelURL == "" {
				cfg.OpenShift.RHCOSImages.KernelURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-kernel-ppc64le", majorMinor)
			}
			if cfg.OpenShift.RHCOSImages.InitramfsURL == "" {
				cfg.OpenShift.RHCOSImages.InitramfsURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-initramfs.ppc64le.img", majorMinor)
			}
			if cfg.OpenShift.RHCOSImages.RootfsURL == "" {
				cfg.OpenShift.RHCOSImages.RootfsURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-rootfs.ppc64le.img", majorMinor)
			}
		}
	}
	// ========================================================================

	// Auto-discover Controller IP ONLY if not explicitly provided in YAML
	if cfg.Controller.IP == "" {
		if cfg.Controller.NetworkInterface != "" {
			ip, err := controller.GetInterfaceIPv4(cfg.Controller.NetworkInterface)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to auto-discover Controller IP on interface %s: %w", cfg.Controller.NetworkInterface, err)
			}
			cfg.Controller.IP = ip
		} else {
			return nil, nil, nil, fmt.Errorf("controller.network_interface must be specified in the configuration file")
		}
	}

	// Override cluster name if provided via flag
	if clusterName != "" {
		cfg.OpenShift.ClusterName = clusterName
	} else {
		clusterName = cfg.OpenShift.ClusterName
	}

	if clusterName == "" {
		return nil, nil, nil, fmt.Errorf("cluster name must be specified in config or via --cluster flag")
	}

	// Setup workspace
	workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, clusterName)
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	// Setup logging
	logFilePath := filepath.Join(workspaceDir, "deployment.log")
	appLogger, err := logger.New(debug, logFilePath)
	if err != nil {
		// Instantiate the fallback logger FIRST, then use it to warn the user
		appLogger, _ = logger.New(debug, "")
		appLogger.Warn("Could not setup file logging. Using console only.", "error", err)
	}

	// Create orchestrator
	orch := orchestrator.NewOrchestrator(&cfg, daemonCfg, appLogger, workspaceDir, debug)

	return &cfg, daemonCfg, orch, nil
}

// Made with Bob
