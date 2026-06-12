package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.ibm.com/sudeeshjohn/shiftlaunch/config"
	"github.ibm.com/sudeeshjohn/shiftlaunch/types"
)

var ocCmd = &cobra.Command{
	Use:   "oc [args...]",
	Short: "Execute OpenShift CLI (oc) commands against a managed cluster",
	GroupID: "utils",
	Long: `A convenience wrapper that automatically configures the KUBECONFIG 
and executes the 'oc' binary for a specific cluster.

Example:
  shiftlaunch oc --cluster my-cluster get nodes
  shiftlaunch oc --cluster my-cluster debug node/master-0`,
	
	// Keep flag parsing ENABLED so ShiftLaunch can read the --cluster flag
	DisableFlagParsing: false, 
	RunE: runOcWrapper,
}

func init() {
	// This tells Cobra to stop parsing flags the moment it hits 
	// the first command (like "get" or "debug"). This means flags like "-w" 
	// will be safely passed to 'oc' instead of breaking ShiftLaunch.
	ocCmd.Flags().SetInterspersed(false)
	
	rootCmd.AddCommand(ocCmd)
}

func runOcWrapper(cmd *cobra.Command, args []string) error {
	// 1. Disconnect ShiftLaunch's global graceful shutdown handler.
	// This allows you to Ctrl+C out of a 'watch' command natively.
	signal.Reset(os.Interrupt, syscall.SIGTERM)

	// 2. Load basic daemon config to find the workspace
	daemonCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load daemon config: %w", err)
	}

	// Because DisableFlagParsing is false, the global clusterName 
	// variable from root.go is perfectly populated!
	if clusterName == "" {
		return fmt.Errorf("you must specify a cluster using --cluster")
	}

	workspaceDir := filepath.Join(daemonCfg.Paths.WorkspaceDir, clusterName)
	stateManager := types.NewStateManager(clusterName)
	
	// 3. Ensure cluster actually exists and is deployed
	if _, err := stateManager.LoadState(); err != nil {
		return fmt.Errorf("could not load state for cluster '%s'. Is it deployed?", clusterName)
	}

	ocPath := filepath.Join(workspaceDir, "tools", "oc")
	kubeconfigPath := filepath.Join(workspaceDir, "install-dir", "auth", "kubeconfig")

	// 4. Verify tools exist
	if _, err := os.Stat(ocPath); os.IsNotExist(err) {
		return fmt.Errorf("oc binary not found at %s", ocPath)
	}
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		return fmt.Errorf("kubeconfig not found at %s", kubeconfigPath)
	}

	// 5. Execute the command interactively
	ocExec := exec.Command(ocPath, args...)
	
	// ---  Strip existing KUBECONFIG from shell to prevent bleed-through ---
	var cleanEnv []string
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "KUBECONFIG=") {
			cleanEnv = append(cleanEnv, env)
		}
	}
	// Force the execution to ONLY use the specified cluster's workspace config
	ocExec.Env = append(cleanEnv, "KUBECONFIG="+kubeconfigPath)

	// Bind standard streams so interactive commands like `oc debug` work!
	ocExec.Stdin = os.Stdin
	ocExec.Stdout = os.Stdout
	ocExec.Stderr = os.Stderr

	// Run it directly in the foreground
	if err := ocExec.Run(); err != nil {
		// Exit silently because the `oc` command will have already printed its own error to Stderr
		os.Exit(1)
	}

	return nil
}