package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sudeeshjohn/shiftlaunch/types"
)

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Power off LPARs and remove local services",
	Long: `Safely tears down a cluster, unmaps storage, and cleans up local services.

The delete command will:
- Power off all cluster LPARs
- Remove network services (DNS, DHCP, PXE, Load Balancer)
- Clean up local workspace files
- Mark the cluster as deleted`,
	RunE: runDelete,
}

func init() {
	rootCmd.AddCommand(deleteCmd)
}

func runDelete(cmd *cobra.Command, args []string) error {
	cfg, _, orch, err := loadConfig(true)
	if err != nil {
		return err
	}
	
	// Ensure logger file descriptor is closed when command completes
	defer orch.GetLogger().Close()

	ctx := GetContext()
	log := orch.GetLogger()

	if orch.IsDeleted() {
		fmt.Println(" Cluster is already deleted (found .deleted marker). Nothing to do.")
		return nil
	}

	// Record command execution
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME")
	}

	stateManager := types.NewStateManager(cfg.OpenShift.ClusterName)
	var phasesBefore []string
	if state, err := stateManager.LoadState(); err == nil && state != nil {
		phasesBefore = append([]string{}, state.CompletedPhases...)
	}

	cmdExec := types.CommandExecution{
		Command:      "delete",
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

	log.Info("=== Starting Cluster Teardown ===")

	cmdStartTime := time.Now()
	err = orch.Teardown(ctx)
	cmdDuration := time.Since(cmdStartTime)

	// Record command execution end
	cmdExec.EndTime = time.Now().Format(time.RFC3339)
	cmdExec.Duration = cmdDuration.String()
	if err != nil {
		cmdExec.Status = "failed"
		cmdExec.Error = err.Error()
	} else {
		cmdExec.Status = "success"
		log.Info("=== Cluster Teardown Completed Successfully ===")
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
