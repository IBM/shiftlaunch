package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sudeeshjohn/shiftlaunch/types"
)

const (
	// Base directory moved to the standard agent workspace path
	clustersDir         = "/opt/shiftlaunch/clusters"
	clusterConfigFile   = "config.yaml"
	clusterStateFile    = "state.json"
	clusterLockFile     = ".lock"
	clusterMetadataFile = ".managed"
	clusterDeletedFile  = ".deleted"
)

// getClusterDir returns the directory path for a cluster
func getClusterDir(clusterName string) string {
	return filepath.Join(clustersDir, clusterName)
}

// ensureClusterDir creates the cluster directory if it doesn't exist
func ensureClusterDir(clusterName string) error {
	dir := getClusterDir(clusterName)
	return os.MkdirAll(dir, 0755)
}

// getClusterConfigPath returns the path to the cluster's config file
func getClusterConfigPath(clusterName string) string {
	return filepath.Join(getClusterDir(clusterName), clusterConfigFile)
}

// getClusterStatePath returns the path to the cluster's state file
func getClusterStatePath(clusterName string) string {
	return filepath.Join(getClusterDir(clusterName), clusterStateFile)
}

// getClusterLockPath returns the path to the cluster directory lock marker
func getClusterLockPath(clusterName string) string {
	return filepath.Join(getClusterDir(clusterName), clusterLockFile)
}

// getClusterMetadataPath returns the path to the cluster directory metadata marker
func getClusterMetadataPath(clusterName string) string {
	return filepath.Join(getClusterDir(clusterName), clusterMetadataFile)
}

// getClusterDeletedPath returns the path to the cluster directory deleted marker
func getClusterDeletedPath(clusterName string) string {
	return filepath.Join(getClusterDir(clusterName), clusterDeletedFile)
}

// clusterDirExists checks if a cluster directory exists
func clusterDirExists(clusterName string) bool {
	dir := getClusterDir(clusterName)
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// clusterLockExists checks if a cluster directory is protected from overwrite
func clusterLockExists(clusterName string) bool {
	info, err := os.Stat(getClusterLockPath(clusterName))
	return err == nil && !info.IsDir()
}

// clusterDeletedExists checks if a cluster directory is marked as deleted
func clusterDeletedExists(clusterName string) bool {
	info, err := os.Stat(getClusterDeletedPath(clusterName))
	return err == nil && !info.IsDir()
}

// shouldExposeCluster returns whether the cluster directory should appear in list/status flows
func shouldExposeCluster(clusterName string) bool {
	return clusterDirExists(clusterName) && !clusterDeletedExists(clusterName)
}

// ensureClusterAvailableForNewDeployment prevents accidental overwrite of an existing cluster directory
func ensureClusterAvailableForNewDeployment(clusterName string) error {
	if !clusterDirExists(clusterName) {
		return nil
	}

	// If marked deleted, we allow redeployment
	if clusterDeletedExists(clusterName) {
		return nil
	}

	if clusterLockExists(clusterName) {
		return fmt.Errorf("Cluster '%s' is locked by a crashed process.\nTo unblock deployment, run: rm -f %s/%s/.lock", clusterName, clustersDir, clusterName)
	}

	return fmt.Errorf("cluster '%s' directory already exists locally. Refusing to overwrite", clusterName)
}

// writeClusterMetadata creates marker files showing the cluster directory is managed
func writeClusterMetadata(clusterName string) error {
	if err := os.Remove(getClusterDeletedPath(clusterName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to clear deleted marker: %w", err)
	}

	lockContent := []byte("managed cluster directory\n")
	if err := os.WriteFile(getClusterLockPath(clusterName), lockContent, 0644); err != nil {
		return fmt.Errorf("failed to create cluster lock file: %w", err)
	}

	metadataContent := []byte(fmt.Sprintf("cluster=%s\n", clusterName))
	if err := os.WriteFile(getClusterMetadataPath(clusterName), metadataContent, 0644); err != nil {
		return fmt.Errorf("failed to create cluster metadata file: %w", err)
	}

	return nil
}

// markClusterDeleted creates the .deleted marker file
func markClusterDeleted(clusterName string) error {
	deletedContent := []byte(fmt.Sprintf("cluster deleted at %s\n", time.Now().Format(time.RFC3339)))
	if err := os.WriteFile(getClusterDeletedPath(clusterName), deletedContent, 0644); err != nil {
		return fmt.Errorf("failed to create cluster deleted marker: %w", err)
	}
	return nil
}

// ensureClusterMetadata backfills marker files for existing managed cluster directories
func ensureClusterMetadata(clusterName string) error {
	if !clusterDirExists(clusterName) {
		return fmt.Errorf("cluster '%s' directory does not exist", clusterName)
	}

	if clusterDeletedExists(clusterName) {
		return nil
	}

	if clusterLockExists(clusterName) {
		return nil
	}

	return writeClusterMetadata(clusterName)
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return destFile.Sync()
}

// listClusters returns a list of all cluster directories in the workspace
func listClusters() ([]string, error) {
	if _, err := os.Stat(clustersDir); os.IsNotExist(err) {
		return []string{}, nil
	}

	entries, err := os.ReadDir(clustersDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read clusters directory: %w", err)
	}

	var clusters []string
	for _, entry := range entries {
		if entry.IsDir() {
			clusters = append(clusters, entry.Name())
		}
	}

	return clusters, nil
}

// findCluster is now adapted to check the cluster name against the current AgentConfig
func findCluster(config *types.AgentConfig, name string) bool {
	return config.OpenShift.ClusterName == name
}