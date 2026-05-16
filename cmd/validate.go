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
	SilenceUsage: true, // Suppress usage menu on validation errors
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
	defer orch.GetLogger().Close()

	ctx := GetContext()
	log := orch.GetLogger()

	// Initialize local executor
	exec := localexec.NewLocalClient(log)
	validator := validation.NewValidator(cfg, exec, debug)
	validator.SetLogger(log)

	// Set up HMC client
	provider, err := compute.NewProvider(cfg, log, debug)
	if err != nil {
		log.Warn("Could not connect to HMC for validation. Skipping HMC infrastructure checks.", "error", err)
	} else {
		if hmcProvider, ok := provider.(*compute.HMCProvider); ok {
			validator.SetHMCClient(hmcProvider.GetHMCClient())
		}
	}

	// The validator handles all its own Phase UI and error printing now
	if err := validator.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed for cluster %s", cfg.OpenShift.ClusterName)
	}

	// Just a single, clean success message at the end
	log.Info("✅ All validations passed successfully!")

	return nil
}
