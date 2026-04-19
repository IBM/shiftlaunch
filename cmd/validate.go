package cmd

import (
	"context"
	"fmt"

	"github.com/sudeeshjohn/shiftlaunch/localexec"
	"github.com/sudeeshjohn/shiftlaunch/orchestrator"
	"github.com/sudeeshjohn/shiftlaunch/types"
	"github.com/sudeeshjohn/shiftlaunch/validation"
)

// Validate validates cluster configuration
func Validate(ctx context.Context,orch *orchestrator.Orchestrator, config *types.AgentConfig) error {
	log := orch.GetLogger()
	log.Info("=== Validating Configuration ===")

	clusterName := config.OpenShift.ClusterName
	log.Info(fmt.Sprintf("Validating cluster: %s", clusterName))

	// Initialize local executor for environment validation
	exec := localexec.NewLocalClient(log)

	// Create validator using the new signature: (cfg, executor, debug)
	validator := validation.NewValidator(config, exec, orch.GetDebug())
	
	// Inject the orchestrator's logger to capture validation details
	validator.SetLogger(log)

	// If HMC credentials are provided, the orchestrator/main should have 
	// initialized the HMC client which can be injected here for Phase 3 checks.
	// For now, we run the standard validation suite.
	if err := validator.Validate(ctx); err != nil {
		return fmt.Errorf("validation failed for cluster %s: %w", clusterName, err)
	}

	log.Info(fmt.Sprintf("✓ Cluster '%s' configuration is valid", clusterName))
	log.Info("=== All Validations Passed ===")
	
	return nil
}
