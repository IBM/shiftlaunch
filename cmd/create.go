package cmd

import (
	"context"

	"github.com/sudeeshjohn/shiftlaunch/orchestrator"
)

// Create executes the cluster deployment pipeline
func Create(ctx context.Context, orch *orchestrator.Orchestrator, resume bool) error {
	log := orch.GetLogger()

	if resume {
		log.Info("=== Resuming Cluster Deployment ===")
	} else {
		log.Info("=== Starting Cluster Deployment ===")
	}

	// The Orchestrator's Deploy method now handles the linear pipeline natively
	if err := orch.Deploy(ctx,resume); err != nil {
		return err
	}

	log.Info("=== Cluster Deployment Completed Successfully ===")
	return nil
}