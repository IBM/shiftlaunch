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

	"github.com/IBM/shiftlaunch/config"
	"github.com/IBM/shiftlaunch/infra/controller"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/orchestrator"
	"github.com/IBM/shiftlaunch/types"
)

// version is dynamically injected via ldflags at compile time.
// "dev-build" is the fallback if someone builds it without the Makefile.
var version = "dev-build"

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
	// UX OPTIMIZATION & INTERLOCK EVALUATION FOR ENUM ISOLATION ROLES
	// ========================================================================
	if cfg.OpenShift.ReleaseType == "" {
		cfg.OpenShift.ReleaseType = "official"
	}

	// ========================================================================
	// REGISTRY ZERO-CONFIG AUTO-RESOLVER
	// ========================================================================
	// If the user requests an airgap but omits the registry block, auto-build it!
	if cfg.Network.IsolationLevel == "air-gapped" {
		if cfg.Services.Registry == nil {
			cfg.Services.Registry = &types.ServiceRegistry{}
		}
	}

	// Implicit enforcement: If local registry management is active, lock down isolation state
	if cfg.Services.Registry.IsManaged() {
		cfg.Network.IsolationLevel = "air-gapped"
	}
	if cfg.Services.Proxy.IsManaged() {
		cfg.Network.IsolationLevel = "restricted-network"
	}

	switch cfg.Network.IsolationLevel {
	case "air-gapped":
		// Clear proxy variables completely to guarantee strict airgap enforcement
		proxyVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"}
		for _, v := range proxyVars {
			os.Unsetenv(v)
		}

		if cfg.Services.Registry.IsManaged() {
			cfg.Services.Registry.AutoMirror = true
			if cfg.Services.Registry.RegistryImage == "" {
				cfg.Services.Registry.RegistryImage = "docker.io/library/registry:3.1.1"
			}
			if cfg.Services.Registry.Username == "" {
				cfg.Services.Registry.Username = "admin"
			}
			if cfg.Services.Registry.Password == "" {
				cfg.Services.Registry.Password = "admin"
			}
			if cfg.Services.Registry.LocalRepo == "" {
				cfg.Services.Registry.LocalRepo = "ocp4/openshift4"
			}
		}

		if cfg.Services.Registry.ReleaseImage == "" && cfg.OpenShift.Version != "" {
			versionTag := cfg.OpenShift.Version
			if len(strings.Split(versionTag, ".")) == 2 {
				versionTag = versionTag + ".0"
			}
			cfg.Services.Registry.ReleaseImage = fmt.Sprintf("quay.io/openshift-release-dev/ocp-release:%s-ppc64le", versionTag)
		}

	case "restricted-network":
		if cfg.Services.Proxy.GetHTTP() != "" {
			os.Setenv("HTTP_PROXY", cfg.Services.Proxy.GetHTTP())
			os.Setenv("HTTPS_PROXY", cfg.Services.Proxy.GetHTTPS())
			os.Setenv("http_proxy", cfg.Services.Proxy.GetHTTP())
			os.Setenv("https_proxy", cfg.Services.Proxy.GetHTTPS())
			if cfg.Services.Proxy.GetNoProxy() != "" {
				os.Setenv("NO_PROXY", cfg.Services.Proxy.GetNoProxy())
				os.Setenv("no_proxy", cfg.Services.Proxy.GetNoProxy())
			}
		}
	default:
		cfg.Network.IsolationLevel = "connected"
	}

	// Auto-fill OpenShift mirror URLs if missing (Controller downloads these regardless of cluster isolation)
	if cfg.OpenShift.Version != "" {
		majorMinor := cfg.OpenShift.Version
		if parts := strings.Split(cfg.OpenShift.Version, "."); len(parts) >= 2 {
			majorMinor = parts[0] + "." + parts[1]
		}

		// Pre-release versions (ec, rc, candidate, nightly, etc.) do not have stable
		// mirror paths. Skip auto-resolving URLs and let validation enforce that the
		// user explicitly provides them.
		if !isPreReleaseVersion(cfg.OpenShift.Version) {
			if cfg.OpenShift.OCPClientConfig.Client == "" {
				cfg.OpenShift.OCPClientConfig.Client = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/clients/ocp/%s/openshift-client-linux.tar.gz", cfg.OpenShift.Version)
			}
			if cfg.OpenShift.OCPClientConfig.Installer == "" {
				cfg.OpenShift.OCPClientConfig.Installer = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/clients/ocp/%s/openshift-install-linux.tar.gz", cfg.OpenShift.Version)
			}
			// Auto-resolver for the oc-mirror plugin: only needed for air-gapped managed-registry
			// deployments with official releases (ci builds use 'oc adm release mirror' instead).
			if cfg.OpenShift.OCPClientConfig.MirrorClient == "" &&
				cfg.Network.IsolationLevel == "air-gapped" &&
				cfg.Services.Registry.IsManaged() &&
				cfg.OpenShift.ReleaseType != "ci" {
				cfg.OpenShift.OCPClientConfig.MirrorClient = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/clients/ocp/%s/oc-mirror.tar.gz", cfg.OpenShift.Version)
			}
			if cfg.Nodes.BootMethod != "agent" {
				if cfg.OpenShift.RHCOSImages.KernelURL == "" {
					cfg.OpenShift.RHCOSImages.KernelURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-kernel.ppc64le", majorMinor)
				}
				if cfg.OpenShift.RHCOSImages.InitramfsURL == "" {
					cfg.OpenShift.RHCOSImages.InitramfsURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-initramfs.ppc64le.img", majorMinor)
				}
				if cfg.OpenShift.RHCOSImages.RootfsURL == "" {
					cfg.OpenShift.RHCOSImages.RootfsURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/rhcos-live-rootfs.ppc64le.img", majorMinor)
				}
				if cfg.OpenShift.RHCOSImages.ChecksumURL == "" {
					cfg.OpenShift.RHCOSImages.ChecksumURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/sha256sum.txt", majorMinor)
				}
			}
			if cfg.OpenShift.RHCOSImages.ChecksumURL == "" {
				cfg.OpenShift.RHCOSImages.ChecksumURL = fmt.Sprintf("https://mirror.openshift.com/pub/openshift-v4/ppc64le/dependencies/rhcos/%s/latest/sha256sum.txt", majorMinor)
			}
		}
	}

	if cfg.OpenShift.ClusterNetworkCIDR == "" {
		cfg.OpenShift.ClusterNetworkCIDR = "10.128.0.0/14"
	}
	if cfg.OpenShift.HostPrefix == 0 {
		cfg.OpenShift.HostPrefix = 23
	}
	if cfg.OpenShift.ServiceNetwork == "" {
		cfg.OpenShift.ServiceNetwork = "172.30.0.0/16"
	}

	if cfg.Network.ControllerIP == "" {
		if cfg.Network.ControllerInterface != "" {
			ip, err := controller.GetInterfaceIPv4(cfg.Network.ControllerInterface)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to auto-discover Controller IP on interface %s: %w", cfg.Network.ControllerInterface, err)
			}
			cfg.Network.ControllerIP = ip
		} else {
			return nil, nil, nil, fmt.Errorf("network.controller_interface must be specified in the configuration file")
		}
	}

	// ========================================================================
	// LOAD BALANCER ZERO-CONFIG AUTO-RESOLVER
	// ========================================================================
	if cfg.Services.LoadBalancer == nil {
		cfg.Services.LoadBalancer = &types.ServiceLoadBalancer{}
	}

	// 1. If an external enterprise LB is provided, that IP becomes the OpenShift VIP
	if cfg.Services.LoadBalancer.ExternalLoadBalancer != "" {
		cfg.Services.LoadBalancer.VIP = cfg.Services.LoadBalancer.ExternalLoadBalancer
	}

	// 2. If NO VIP is specified at all, default to using the Controller's primary IP!
	if cfg.Services.LoadBalancer.VIP == "" {
		cfg.Services.LoadBalancer.VIP = cfg.Network.ControllerIP
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

// Made with Bob
