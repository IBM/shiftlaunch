package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sudeeshjohn/shiftlaunch/infra/compute"
	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/validation"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate cluster configuration against infrastructure",
	Long: `Performs pre-flight checks against the YAML and physical HMC infrastructure.

The validate command will check:
- Configuration file syntax and completeness
- Network connectivity and prerequisites
- HMC connectivity and LPAR existence
- Resource availability`,
	RunE: runValidate,
}

func init() {
	rootCmd.AddCommand(validateCmd)
}

func runValidate(cmd *cobra.Command, args []string) error {
	cfg, _, orch, err := loadConfig(true)
	if err != nil {
		return err
	}

	ctx := GetContext()
	log := orch.GetLogger()

	log.Info("=== Validating Configuration ===")
	log.Info("Validating cluster", "cluster", cfg.OpenShift.ClusterName)

	// Initialize local executor for environment validation
	exec := localexec.NewLocalClient(log)

	// Create validator
	validator := validation.NewValidator(cfg, exec, debug)
	validator.SetLogger(log)

	// Set up HMC client for Phase 3 validation (LPAR existence checks)
	provider, err := compute.NewProvider(cfg, log, debug)
	if err != nil {
		log.Warn("Could not connect to HMC for validation. Skipping Phase 3 (LPAR validation).", "error", err)
	} else {
		// Extract the HMC client from the provider
		if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
			validator.SetHMCClient(hmcProvider.GetHMCClient())
		}
	}

	if err := validator.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed for cluster %s: %w", cfg.OpenShift.ClusterName, err)
	}

	log.Info("Cluster configuration is valid", "cluster", cfg.OpenShift.ClusterName)
	log.Info("=== All Validations Passed ===")

	return nil
}
