package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/sudeeshjohn/shiftlaunch/config"
	"github.com/sudeeshjohn/shiftlaunch/logger"
)

var (
	exportKubeconfig string
	exportStdout     bool
)

var exportCmd = &cobra.Command{
	Use:     "export",
	Short:   "Export cluster resources",
	GroupID: "utils",
	Long:    `Export cluster resources such as kubeconfig.`,
}

var exportKubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Export cluster kubeconfig",
	Long: `Exports cluster kubeconfig with context name customization.

The export kubeconfig command will:
- Locate the cluster's kubeconfig file
- Merge it with the specified kubeconfig file (or default)
- Set the context name to match the cluster name`,
	RunE: runExportKubeconfig,
}

func init() {
	rootCmd.AddCommand(exportCmd)
	exportCmd.AddCommand(exportKubeconfigCmd)

	exportKubeconfigCmd.Flags().StringVar(&exportKubeconfig, "kubeconfig", "", "Destination kubeconfig path (default: $KUBECONFIG or $HOME/.kube/config)")
	exportKubeconfigCmd.Flags().BoolVar(&exportStdout, "stdout", false, "Print kubeconfig to stdout instead of file")
	
	// Note: --cluster is inherited globally from root.go
}

func runExportKubeconfig(cmd *cobra.Command, args []string) error {
	// clusterName is a global variable populated by root.go
	if clusterName == "" {
		return fmt.Errorf("cluster name is required. Use the --cluster flag")
	}
	return ExportKubeconfig(clusterName, exportKubeconfig, exportStdout)
}

// KubeconfigStructure represents the kubeconfig YAML structure
type KubeconfigStructure struct {
	APIVersion     string              `yaml:"apiVersion"`
	Kind           string              `yaml:"kind"`
	Clusters       []KubeconfigCluster `yaml:"clusters"`
	Contexts       []KubeconfigContext `yaml:"contexts"`
	CurrentContext string              `yaml:"current-context"`
	Users          []KubeconfigUser    `yaml:"users"`
}

type KubeconfigCluster struct {
	Name    string                 `yaml:"name"`
	Cluster map[string]interface{} `yaml:"cluster"`
}

type KubeconfigContext struct {
	Name    string                 `yaml:"name"`
	Context map[string]interface{} `yaml:"context"`
}

type KubeconfigUser struct {
	Name string                 `yaml:"name"`
	User map[string]interface{} `yaml:"user"`
}

// ExportKubeconfig exports cluster kubeconfig with context name modification
func ExportKubeconfig(clusterName, kubeconfigPath string, stdout bool) error {
	// 1. Validate cluster name is provided
	if clusterName == "" {
		return fmt.Errorf("cluster name is required. Use the --cluster flag")
	}

	// 2. Load daemon config to get workspace directory
	daemonCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load daemon configuration: %w", err)
	}

	// 3. Validate cluster exists in workspace
	if err := validateClusterExists(daemonCfg.Paths.WorkspaceDir, clusterName); err != nil {
		return err
	}

	// 4. Read source kubeconfig from workspace
	sourceKC, err := readSourceKubeconfig(daemonCfg.Paths.WorkspaceDir, clusterName)
	if err != nil {
		return err
	}

	// 5. Modify context name to cluster name
	modifyContextName(sourceKC, clusterName)

	// 6. If stdout, print and return
	if stdout {
		data, err := yaml.Marshal(sourceKC)
		if err != nil {
			return fmt.Errorf("failed to marshal kubeconfig: %w", err)
		}
		fmt.Print(string(data))
		return nil
	}

	// 7. Determine destination path
	destPath, err := resolveDestinationPath(kubeconfigPath)
	if err != nil {
		return err
	}

	// Initialize a console-only logger for a consistent UI
	log, _ := logger.New(false, "")

	// 8. Merge with existing kubeconfig if it exists
	var finalKC *KubeconfigStructure
	if existingKC, err := readExistingKubeconfig(destPath); err == nil {
		finalKC = mergeKubeconfigs(existingKC, sourceKC)
		log.Info("Merged kubeconfig context", "cluster", clusterName, "destination", destPath)
	} else {
		finalKC = sourceKC
		log.Info("Created new kubeconfig", "destination", destPath)
	}

	// 9. Write merged kubeconfig to destination
	if err := writeKubeconfig(finalKC, destPath); err != nil {
		return err
	}

	log.Info("Successfully exported kubeconfig", "cluster", clusterName, "current_context", clusterName)
	return nil
}

// validateClusterExists checks if the cluster directory exists in workspace
func validateClusterExists(workspaceDir, clusterName string) error {
	clusterDir := filepath.Join(workspaceDir, clusterName)
	if _, err := os.Stat(clusterDir); os.IsNotExist(err) {
		return fmt.Errorf("cluster '%s' not found in workspace at %s\nUse 'shiftlaunch list' to see available clusters", clusterName, workspaceDir)
	}
	return nil
}

// readSourceKubeconfig reads and parses the kubeconfig from cluster workspace
func readSourceKubeconfig(workspaceDir, clusterName string) (*KubeconfigStructure, error) {
	kubeconfigPath := filepath.Join(workspaceDir, clusterName, "install-dir", "auth", "kubeconfig")

	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubeconfig not found for cluster '%s' at %s\nHas the cluster been deployed successfully?", clusterName, kubeconfigPath)
	}

	data, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read kubeconfig: %w", err)
	}

	var kubeconfig KubeconfigStructure
	if err := yaml.Unmarshal(data, &kubeconfig); err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig (invalid YAML format): %w", err)
	}

	return &kubeconfig, nil
}

// modifyContextName changes the context name to match the cluster name
func modifyContextName(kubeconfig *KubeconfigStructure, newContextName string) {
	// Store old context name for reference
	oldContextName := kubeconfig.CurrentContext

	// Update all contexts that match the old current context
	for i := range kubeconfig.Contexts {
		if kubeconfig.Contexts[i].Name == oldContextName {
			kubeconfig.Contexts[i].Name = newContextName
		}
	}

	// Update current-context to new name
	kubeconfig.CurrentContext = newContextName
}

// resolveDestinationPath determines the kubeconfig destination path
// Priority: flag > $KUBECONFIG > $HOME/.kube/config
func resolveDestinationPath(kubeconfigFlag string) (string, error) {
	// Priority 1: Use flag value if provided
	if kubeconfigFlag != "" {
		return kubeconfigFlag, nil
	}

	// Priority 2: Use $KUBECONFIG environment variable if set
	if envPath := os.Getenv("KUBECONFIG"); envPath != "" {
		return envPath, nil
	}

	// Priority 3: Use default $HOME/.kube/config
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".kube", "config"), nil
}

// readExistingKubeconfig reads an existing kubeconfig file if it exists
func readExistingKubeconfig(path string) (*KubeconfigStructure, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var kubeconfig KubeconfigStructure
	if err := yaml.Unmarshal(data, &kubeconfig); err != nil {
		return nil, err
	}

	return &kubeconfig, nil
}

// mergeKubeconfigs merges two kubeconfig structures
// New entries overwrite existing ones with the same name
func mergeKubeconfigs(existing, new *KubeconfigStructure) *KubeconfigStructure {
	merged := &KubeconfigStructure{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: new.CurrentContext, // Use new context as current
	}

	// Merge clusters (avoid duplicates by name, new overwrites existing)
	clusterMap := make(map[string]KubeconfigCluster)
	for _, cluster := range existing.Clusters {
		clusterMap[cluster.Name] = cluster
	}
	for _, cluster := range new.Clusters {
		clusterMap[cluster.Name] = cluster // Overwrite if exists
	}
	for _, cluster := range clusterMap {
		merged.Clusters = append(merged.Clusters, cluster)
	}

	// Merge contexts (avoid duplicates by name, new overwrites existing)
	contextMap := make(map[string]KubeconfigContext)
	for _, context := range existing.Contexts {
		contextMap[context.Name] = context
	}
	for _, context := range new.Contexts {
		contextMap[context.Name] = context // Overwrite if exists
	}
	for _, context := range contextMap {
		merged.Contexts = append(merged.Contexts, context)
	}

	// Merge users (avoid duplicates by name, new overwrites existing)
	userMap := make(map[string]KubeconfigUser)
	for _, user := range existing.Users {
		userMap[user.Name] = user
	}
	for _, user := range new.Users {
		userMap[user.Name] = user // Overwrite if exists
	}
	for _, user := range userMap {
		merged.Users = append(merged.Users, user)
	}

	return merged
}

// writeKubeconfig writes the kubeconfig to the specified path
func writeKubeconfig(kubeconfig *KubeconfigStructure, destPath string) error {
	// Ensure directory exists
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", destDir, err)
	}

	// Marshal to YAML
	data, err := yaml.Marshal(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to marshal kubeconfig: %w", err)
	}

	// Write with appropriate permissions (0600 for kubeconfig security)
	if err := os.WriteFile(destPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write kubeconfig to %s: %w", destPath, err)
	}

	return nil
}