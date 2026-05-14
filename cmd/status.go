package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show cluster deployment status and endpoints",
	Long: `Displays the current deployment state, URLs, and credentials of a managed cluster.

The status command shows:
- Deployment phase and progress
- Cluster endpoints (API, Console)
- Node status
- Credentials and access information`,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	cfg, _, orch, err := loadConfig(true)
	if err != nil {
		return err
	}
	
	// Ensure logger file descriptor is closed when command completes
	defer orch.GetLogger().Close()

	ctx := GetContext()
	log := orch.GetLogger()

	clusterName := cfg.OpenShift.ClusterName
	log.Info("Checking cluster status", "cluster", clusterName)

	status := orch.GetClusterStatus(ctx)

	fmt.Println("================================================================================")
	fmt.Printf(" Deployment Status for: %s\n", clusterName)
	fmt.Println("================================================================================")
	fmt.Println()
	fmt.Println(status)
	fmt.Println("================================================================================")

	return nil
}
