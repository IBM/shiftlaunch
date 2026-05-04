package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/sudeeshjohn/shiftlaunch/config"
	"github.com/sudeeshjohn/shiftlaunch/infra/controller"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	"github.com/sudeeshjohn/shiftlaunch/orchestrator"
	"github.com/sudeeshjohn/shiftlaunch/types"
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
- HMC LPAR provisioning
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

	// Global persistent flags
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "config.yaml", "Path to agent configuration file")
	rootCmd.PersistentFlags().StringVar(&clusterName, "cluster", "", "Cluster name override")
	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug output to terminal")

	// Setup graceful shutdown
	setupSignalHandler()
}

func initConfig() {
	// Configuration initialization happens per-command as needed
}

// setupSignalHandler sets up graceful shutdown on interrupt signals
func setupSignalHandler() {
	rootCtx, cancel = context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\n\n[WARNING] Interrupt signal received! Attempting graceful shutdown...")
		cancel()
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
		workspaceConfig := filepath.Join(daemonCfg.Paths.WorkspaceDir, clusterName, "config.yaml")
		if _, err := os.Stat(workspaceConfig); err == nil {
			configPath = workspaceConfig
		}
	}

	// Load cluster config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read configuration file: %w\n(Hint: Provide a valid config.yaml or specify the cluster name if it was already created)", err)
	}

	var cfg types.AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse YAML configuration: %w", err)
	}

	// Auto-discover Controller IP
	if cfg.Controller.NetworkInterface != "" {
		ip, err := controller.GetInterfaceIPv4(cfg.Controller.NetworkInterface)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to auto-discover Controller IP on interface %s: %w", cfg.Controller.NetworkInterface, err)
		}
		cfg.Controller.IP = ip
	} else {
		return nil, nil, nil, fmt.Errorf("controller.network_interface must be specified in the configuration file")
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
		fmt.Printf(" Warning: Could not setup file logging: %v. Using console only.\n", err)
		appLogger, _ = logger.New(debug, "")
	}

	// Create orchestrator
	orch := orchestrator.NewOrchestrator(&cfg, daemonCfg, appLogger, workspaceDir, debug)

	return &cfg, daemonCfg, orch, nil
}

// Made with Bob
