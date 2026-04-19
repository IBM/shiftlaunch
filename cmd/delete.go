package cmd

import (
	"context"
	"fmt"

	"github.com/sudeeshjohn/shiftlaunch/orchestrator"
)

// Delete executes the cluster teardown process
func Delete(ctx context.Context,orch *orchestrator.Orchestrator) error {
	log := orch.GetLogger()

	if orch.IsDeleted() {
		fmt.Println("⚠️  Cluster is already deleted (found .deleted marker). Nothing to do.")
		return nil
	}

	log.Info("=== Starting Cluster Teardown ===")

	// The Orchestrator's Teardown method handles the safe BYOI power-off natively
	if err := orch.Teardown(ctx); err != nil {
		return err
	}

	log.Info("=== Cluster Teardown Completed Successfully ===")
	return nil
}