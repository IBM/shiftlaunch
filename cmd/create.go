package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/IBM/shiftlaunch/infra/compute"
	"github.com/IBM/shiftlaunch/localexec"
	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/orchestrator"
	"github.com/IBM/shiftlaunch/types"
	"github.com/IBM/shiftlaunch/validation"
)

var (
	createPullSecret string
	createSSHKey     string
)

var createCmd = &cobra.Command{
	Use:           "create",
	Aliases:       []string{"deploy", "dep"},
	Short:         "Execute cluster deployment pipeline",
	GroupID:       "core",
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Execute the cluster deployment pipeline. Automatically resumes if a partial
deployment is detected.

The create command will:
- Validate the configuration
- Setup network services (DNS, DHCP, PXE, Load Balancer)
- Provision LPARs on HMC
- Install OpenShift cluster`,
	RunE: runCreate,
}

func init() {
	rootCmd.AddCommand(createCmd)
	createCmd.Flags().StringVar(&createPullSecret, "pull-secret", "", "Override path to OpenShift pull secret file")
	createCmd.Flags().StringVar(&createSSHKey, "ssh-key", "", "Override path to SSH public key file")
}

func runCreate(cmd *cobra.Command, args []string) error {
	cfg, daemonCfg, orch, err := loadConfig(true)
	if err != nil {
		return err
	}

	// Ensure logger file descriptor is closed when command completes.
	// The anonymous func is required so the pointer is evaluated at exit time,
	// not now — orch may be replaced with a new orchestrator further below if
	// the workspace was previously deleted.
	defer func() { orch.GetLogger().Close() }()

	ctx := GetContext()
	log := orch.GetLogger()

	// Apply CLI flag overrides for file paths
	if createPullSecret != "" {
		absPath, _ := filepath.Abs(createPullSecret)
		cfg.OpenShift.PullSecretFile = absPath
	}
	if createSSHKey != "" {
		cfg.OpenShift.SSHPublicKeyFile = createSSHKey
	}

	// Auto-Resume Detection
	autoResume := false
	stateManager := types.NewStateManager(cfg.OpenShift.ClusterName)
	var phasesBefore []string

	workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, cfg.OpenShift.ClusterName)

	// Check for previous deployment state
	if state, err := stateManager.LoadState(); err == nil && state != nil {
		phasesBefore = append([]string{}, state.CompletedPhases...)
		if (state.Status == "in_progress" || state.Status == "failed") && len(state.CompletedPhases) > 0 {
			autoResume = true
			state.ResumeCount++
			stateManager.SaveState(state)
			// Downgraded to Debug so it stays out of the user's terminal
			log.Debug("Detected incomplete deployment. Automatically resuming from last successful phase...",
				"cluster", cfg.OpenShift.ClusterName,
				"last_phase", state.CurrentPhase,
				"status", state.Status)
		}
	}

	// Handle workspace markers
	deletedMarker := filepath.Join(workspaceDir, ".deleted")
	if _, err := os.Stat(deletedMarker); err == nil {
		// Downgraded to Debug so it doesn't clutter the terminal
		log.Debug("Cluster was previously deleted. Wiping directory for a fresh deployment...", "cluster", cfg.OpenShift.ClusterName)
		os.RemoveAll(workspaceDir)
		os.MkdirAll(workspaceDir, 0755)

		// Recreate logger after workspace cleanup to ensure deployment.log is created
		logFilePath := filepath.Join(workspaceDir, "deployment.log")
		newLogger, err := logger.New(debug, logFilePath)
		if err != nil {
			log.Warn("Failed to recreate logger after workspace cleanup", "error", err)
		} else {
			// Update orchestrator with new logger
			orch = orchestrator.NewOrchestrator(cfg, daemonCfg, newLogger, workspaceDir, debug)
			log = orch.GetLogger()
			// Downgraded to Debug
			log.Debug("Logger recreated after workspace cleanup")
		}
	}

	managedMarker := filepath.Join(workspaceDir, ".managed")
	failedMarker := filepath.Join(workspaceDir, ".failed")
	existingConfigPath := filepath.Join(workspaceDir, "config.yaml")

	if _, err := os.Stat(failedMarker); err == nil {
		if configFile != existingConfigPath && configFile != "config.yaml" {
			log.Info("Resuming failed deployment with existing configuration",
				"cluster", cfg.OpenShift.ClusterName,
				"config", existingConfigPath)
		} else {
			log.Info("Resuming failed deployment", "cluster", cfg.OpenShift.ClusterName)
		}
	} else if _, err := os.Stat(managedMarker); err == nil {
		log.Error("Cluster is already managed and fully deployed", "cluster", cfg.OpenShift.ClusterName, "workspace", workspaceDir)
		log.Info("If you want to:")
		log.Info("  - View cluster status: shiftlaunch status --cluster " + cfg.OpenShift.ClusterName)
		log.Info("  - Delete the cluster: shiftlaunch delete --cluster " + cfg.OpenShift.ClusterName)
		log.Info("  - Deploy a new cluster: First delete the existing one, then create again")
		log.Error("Refusing to overwrite managed cluster to prevent data loss")

		// Return a short error so main.go still exits with status code 1
		return fmt.Errorf("cluster already managed")
	} else {
		// Save config for new cluster
		if _, err := os.Stat(existingConfigPath); err == nil {
			timestamp := time.Now().Format("20060102-150405")
			configBackupPath := filepath.Join(workspaceDir, fmt.Sprintf("config.yaml.backup.%s", timestamp))
			if err := os.Rename(existingConfigPath, configBackupPath); err != nil {
				log.Warn("Failed to backup existing config", "error", err)
			} else {
				log.Debug("Backed up existing config", "path", configBackupPath)
			}
		}

		// ---  Use yaml.Marshal to ensure CLI overrides are safely persisted to the workspace! ---
		data, err := yaml.Marshal(cfg)
		if err == nil {
			os.WriteFile(existingConfigPath, data, 0644)
		}
	}

	// ========================================================================
	// PRE-FLIGHT VALIDATION (Only run on fresh deployments!)
	// ========================================================================
	if !autoResume {
		// Print validation banner
		log.Info(fmt.Sprintf("=== Starting Validation: %s ===", cfg.OpenShift.ClusterName))

		// Change from Info to StartPhase to spin up the UI
		log.StartPhase("Running pre-flight validation checks...")

		exec := localexec.NewLocalClient(log)
		validator := validation.NewValidator(cfg, exec, debug)
		validator.SetLogger(log)

		// Attach HMC client for LPAR validation
		// The "Connecting to HMC..." Info logs inside here will be automatically intercepted by the spinner!
		if provider, perr := compute.NewProvider(cfg, log, debug); perr == nil {
			if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
				validator.SetHMCClient(hmcProvider.GetHMCClient())
				defer hmcProvider.Cleanup()
			}
		}

		// Cleanly end this initialization spinner before the actual Validator starts its [Check X/4] spinners
		log.EndPhase(true, "Validation prerequisites initialized")

		if valErr := validator.Validate(ctx); valErr != nil {
			return fmt.Errorf("validation failed for cluster %s: %w", cfg.OpenShift.ClusterName, valErr)
		}
	}
	// ========================================================================

	// Record command execution
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME")
	}

	cmdExec := types.CommandExecution{
		Command:      "create",
		StartTime:    time.Now().Format(time.RFC3339),
		Status:       "in_progress",
		User:         username,
		Hostname:     hostname,
		PID:          os.Getpid(),
		ConfigFile:   configFile,
		PhasesBefore: phasesBefore,
		Flags: map[string]string{
			"debug":   fmt.Sprintf("%v", debug),
			"cluster": cfg.OpenShift.ClusterName,
		},
	}

	// Execute deployment
	if autoResume {
		log.Info(fmt.Sprintf("=== Resuming Cluster Deployment: %s ===", cfg.OpenShift.ClusterName))
	} else {
		log.Info(fmt.Sprintf("=== Starting New Cluster Deployment: %s ===", cfg.OpenShift.ClusterName))
	}

	cmdStartTime := time.Now()
	err = orch.Deploy(ctx, autoResume)
	cmdDuration := time.Since(cmdStartTime)

	// Record command execution end
	cmdExec.EndTime = time.Now().Format(time.RFC3339)
	cmdExec.Duration = cmdDuration.String()
	if err != nil {
		cmdExec.Status = "failed"
		cmdExec.Error = err.Error()
	} else {
		cmdExec.Status = "success"
		log.Info("=== Cluster Deployment Completed Successfully ===")
	}

	// Update state with command execution
	if state, loadErr := stateManager.LoadState(); loadErr == nil && state != nil {
		cmdExec.PhasesAfter = append([]string{}, state.CompletedPhases...)
		state.Version = version
		stateManager.AddCommandExecution(state, cmdExec)
		stateManager.SaveState(state)
	}

	return err
}
