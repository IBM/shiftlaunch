package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sudeeshjohn/shiftlaunch/types"
	"gopkg.in/yaml.v3"
)

// formatDuration formats a duration into a human-readable string matching the old architecture
func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// List prints all managed clusters and their current states from the local workspace
func List() error {
	workspaceBase := "/opt/shiftlaunch/clusters"

	entries, err := os.ReadDir(workspaceBase)
	if err != nil {
		fmt.Println("No clusters found or workspace directory does not exist.")
		fmt.Printf("Clusters are stored in the '%s' directory.\n", workspaceBase)
		return nil
	}

	fmt.Println("=== Managed Clusters ===")
	// --- FIX: Added "CLUSTER IP" column and adjusted spacing ---
	fmt.Printf("%-20s %-15s %-16s %-12s %-20s %-10s %-25s %-20s\n", 
		"CLUSTER NAME", "STATUS", "CLUSTER IP", "TYPE", "PHASE", "DURATION", "PRE-PROVISIONED", "LAST UPDATED")
	fmt.Printf("%s\n", strings.Repeat("-", 145)) // Extended dash line for new column

	visibleCount := 0

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		clusterName := entry.Name()

		// Skip clusters marked as deleted to match the old shouldExposeCluster behavior [cite: 3]
		deletedMarker := filepath.Join(workspaceBase, clusterName, ".deleted")
		if _, err := os.Stat(deletedMarker); err == nil {
			continue
		}

		stateFile := filepath.Join(workspaceBase, clusterName, "state.json")
		configFile := filepath.Join(workspaceBase, clusterName, "config.yaml")

		// Try to load state
		state, err := types.LoadState(clusterName)
		if err != nil {
			// --- FIX: Added "N/A" for the Cluster IP column in the fallback row ---
			fmt.Printf("%-20s %-15s %-16s %-12s %-20s %-10s %-25s %-20s\n",
				clusterName, "unknown", "N/A", "N/A", "N/A", "N/A", "N/A", "N/A")
			visibleCount++
			continue
		}

		// Extract and format the pre_provisioned items and cluster type
		clusterType := "Multi/LPAR" // Default
		preProvStr := "Unknown"
		clusterIP := "Unknown"      // --- FIX: Default value for Cluster IP ---

		data, err := os.ReadFile(configFile)
		if err == nil {
			var cfg types.AgentConfig
			if err := yaml.Unmarshal(data, &cfg); err == nil {
				
				// --- FIX: Extract the LoadBalancerIP (VIP) ---
				if cfg.Network.LoadBalancerIP != "" {
					clusterIP = cfg.Network.LoadBalancerIP
				}

				// Determine deployment type (SNO vs Multi-node)
				deploymentType := "Multi"
				if cfg.IsSNO() {
					deploymentType = "SNO"
				}

				// Evaluate new ManagedServices flags (inverted logic from BYOI pre-provisioned) [cite: 5]
				var prepItems []string
				if !cfg.ManagedServices.DNS {
					prepItems = append(prepItems, "DNS")
				}
				if !cfg.ManagedServices.DHCP {
					prepItems = append(prepItems, "DHCP")
				}
				if !cfg.ManagedServices.PXE {
					prepItems = append(prepItems, "PXE")
				}
				if !cfg.ManagedServices.LoadBalancer {
					prepItems = append(prepItems, "LB")
				}

				// Format the display string
				provisioningType := "LPAR"
				if len(prepItems) > 0 {
					preProvStr = strings.Join(prepItems, ",")
					provisioningType = "BYOI"
				} else {
					preProvStr = "None"
				}

				clusterType = fmt.Sprintf("%s/%s", deploymentType, provisioningType)
			}
		}

		// Format timestamp (Last Updated)
		timestamp := "N/A"
		// If EndTime exists, use it. Otherwise fallback to the file modification time. [cite: 6]
		if state.EndTime != "" {
			if t, err := time.Parse(time.RFC3339, state.EndTime); err == nil {
				timestamp = t.Format("2006-01-02 15:04:05")
			}
		} else {
			if info, err := os.Stat(stateFile); err == nil {
				timestamp = info.ModTime().Format("2006-01-02 15:04:05")
			}
		}

		// Calculate deployment duration
		duration := "N/A"
		if state.StartTime != "" {
			startTime, err := time.Parse(time.RFC3339, state.StartTime)
			if err == nil {
				var d time.Duration
				if state.EndTime != "" {
					// Deployment completed or failed
					endTime, err := time.Parse(time.RFC3339, state.EndTime)
					if err == nil {
						d = endTime.Sub(startTime)
					}
				} else if state.Status == "in_progress" {
					// Deployment still in progress
					d = time.Since(startTime)
					duration = formatDuration(d) + "*"
				}

				if d > 0 && duration == "N/A" {
					duration = formatDuration(d)
				}
			}
		}

		// Print the row
		// --- FIX: Injected clusterIP into the print formatting ---
		fmt.Printf("%-20s %-15s %-16s %-12s %-20s %-10s %-25s %-20s\n",
			clusterName,
			state.Status,
			clusterIP,
			clusterType,
			state.CurrentPhase,
			duration,
			preProvStr,
			timestamp)
			
		visibleCount++
	}

	if visibleCount == 0 {
		fmt.Println("No active clusters found.")
		fmt.Printf("Deleted preserved directories are hidden in the '%s' directory.\n", workspaceBase)
		return nil
	}

	fmt.Printf("\nTotal clusters: %d\n", visibleCount)
	return nil
}