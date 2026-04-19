package types

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// CommandExecution tracks details of each command run
type CommandExecution struct {
	Command     string            `json:"command"`      // create, delete, validate, status, etc.
	StartTime   string            `json:"start_time"`
	EndTime     string            `json:"end_time,omitempty"`
	Duration    string            `json:"duration,omitempty"`
	Status      string            `json:"status"`       // success, failed, in_progress
	Error       string            `json:"error,omitempty"`
	User        string            `json:"user"`
	Hostname    string            `json:"hostname"`
	PID         int               `json:"pid"`
	ConfigFile  string            `json:"config_file,omitempty"`
	Flags       map[string]string `json:"flags,omitempty"`
	PhasesBefore []string         `json:"phases_before,omitempty"` // Phases completed before this command
	PhasesAfter  []string         `json:"phases_after,omitempty"`  // Phases completed after this command
}

// PhaseExecution tracks details of each phase execution
type PhaseExecution struct {
	Phase     string `json:"phase"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time,omitempty"`
	Duration  string `json:"duration,omitempty"`
	Status    string `json:"status"` // success, failed, skipped
	Error     string `json:"error,omitempty"`
	Attempts  int    `json:"attempts,omitempty"` // Number of retry attempts
}

// DeploymentState tracks the progress of the local agent execution
type DeploymentState struct {
	ClusterName      string              `json:"cluster_name"`
	Status           string              `json:"status"` // e.g., "in_progress", "completed", "failed", "deleted"
	CurrentPhase     string              `json:"current_phase"`
	CompletedPhases  []string            `json:"completed_phases"`
	StartTime        string              `json:"start_time"`
	EndTime          string              `json:"end_time,omitempty"`
	Error            string              `json:"error,omitempty"`
	Locked           bool                `json:"locked"` // Indicates if deployment is currently running
	LockTime         string              `json:"lock_time,omitempty"`
	
	// Enhanced tracking
	CommandHistory   []CommandExecution  `json:"command_history,omitempty"`
	PhaseHistory     []PhaseExecution    `json:"phase_history,omitempty"`
	ConfigBackups    []string            `json:"config_backups,omitempty"`    // List of config backup files
	LastConfigUpdate string              `json:"last_config_update,omitempty"`
	TotalDuration    string              `json:"total_duration,omitempty"`
	ResumeCount      int                 `json:"resume_count,omitempty"`      // Number of times resumed
	Version          string              `json:"version,omitempty"`           // ShiftLaunch version
}

// StateManager handles state file operations with locking and backup
type StateManager struct {
	clusterName string
	workspaceDir string
	lockFile    *os.File
}

// NewStateManager creates a new state manager for a cluster
func NewStateManager(clusterName string) *StateManager {
	return &StateManager{
		clusterName: clusterName,
		workspaceDir: filepath.Join("/opt/shiftlaunch/clusters", clusterName),
	}
}

// GetStatePath returns the path to the state file
func (sm *StateManager) GetStatePath() string {
	return filepath.Join(sm.workspaceDir, "state.json")
}

// GetLockPath returns the path to the lock file
func (sm *StateManager) GetLockPath() string {
	return filepath.Join(sm.workspaceDir, ".lock")
}

// GetDeletedMarkerPath returns the path to the deleted marker file
func (sm *StateManager) GetDeletedMarkerPath() string {
	return filepath.Join(sm.workspaceDir, ".deleted")
}

// GetManagedMarkerPath returns the path to the managed marker file
func (sm *StateManager) GetManagedMarkerPath() string {
	return filepath.Join(sm.workspaceDir, ".managed")
}

// AcquireLock attempts to acquire an exclusive lock on the cluster
func (sm *StateManager) AcquireLock() error {
	// Ensure workspace directory exists
	if err := os.MkdirAll(sm.workspaceDir, 0755); err != nil {
		return fmt.Errorf("failed to create workspace directory: %w", err)
	}

	lockPath := sm.GetLockPath()
	
	// Check if lock file exists and is stale
	if info, err := os.Stat(lockPath); err == nil {
		// Lock file exists, check if it's stale (older than 1 hour)
		if time.Since(info.ModTime()) > time.Hour {
			// Stale lock, remove it
			os.Remove(lockPath)
		} else {
			// Lock is fresh - another process is using it
			return fmt.Errorf("cluster '%s' is locked by another process (lock acquired at %s). If you're sure no other deployment is running, remove %s manually",
				sm.clusterName, info.ModTime().Format(time.RFC3339), lockPath)
		}
	}

	// Try to open existing lock file or create new one
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}

	// Apply file lock (advisory lock) - this will fail if another process holds it
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return fmt.Errorf("cluster '%s' is locked by another process. If you're sure no other deployment is running, remove %s manually",
			sm.clusterName, lockPath)
	}

	// Truncate and write lock metadata
	lockFile.Truncate(0)
	lockFile.Seek(0, 0)
	lockData := fmt.Sprintf("Locked at: %s\nPID: %d\n", time.Now().Format(time.RFC3339), os.Getpid())
	lockFile.WriteString(lockData)
	lockFile.Sync()

	sm.lockFile = lockFile
	return nil
}

// ReleaseLock releases the cluster lock
func (sm *StateManager) ReleaseLock() error {
	if sm.lockFile == nil {
		return nil
	}

	// Release file lock
	syscall.Flock(int(sm.lockFile.Fd()), syscall.LOCK_UN)
	
	// Close and remove lock file
	sm.lockFile.Close()
	os.Remove(sm.GetLockPath())
	sm.lockFile = nil
	
	return nil
}

// LoadState reads the state file from disk
func (sm *StateManager) LoadState() (*DeploymentState, error) {
	path := sm.GetStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	
	var state DeploymentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveState writes the state file to disk with backup
func (sm *StateManager) SaveState(state *DeploymentState) error {
	if err := os.MkdirAll(sm.workspaceDir, 0755); err != nil {
		return err
	}
	
	path := sm.GetStatePath()
	
	// Create backup if state file exists (Terraform-style)
	if _, err := os.Stat(path); err == nil {
		backupPath := path + ".backup"
		
		// Read existing state
		existingData, err := os.ReadFile(path)
		if err == nil {
			// Write backup
			if err := os.WriteFile(backupPath, existingData, 0644); err != nil {
				// Log warning but don't fail
				fmt.Fprintf(os.Stderr, "Warning: Failed to create state backup: %v\n", err)
			}
		}
	}
	
	// Marshal state to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	
	// Write to temporary file first (atomic write)
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return err
	}
	
	// Rename temp file to actual state file (atomic operation)
	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return err
	}
	
	return nil
}

// IsDeleted checks if the cluster has been deleted
func (sm *StateManager) IsDeleted() bool {
	_, err := os.Stat(sm.GetDeletedMarkerPath())
	return err == nil
}

// MarkDeleted creates the .deleted marker file
func (sm *StateManager) MarkDeleted() error {
	deletedPath := sm.GetDeletedMarkerPath()
	content := fmt.Sprintf("Deleted at: %s\n", time.Now().Format(time.RFC3339))
	return os.WriteFile(deletedPath, []byte(content), 0644)
}

// ClearDeleted removes the .deleted marker file (for new deployments)
func (sm *StateManager) ClearDeleted() error {
	deletedPath := sm.GetDeletedMarkerPath()
	if _, err := os.Stat(deletedPath); err == nil {
		return os.Remove(deletedPath)
	}
	return nil
}

// MarkManaged creates the .managed marker file
func (sm *StateManager) MarkManaged() error {
	managedPath := sm.GetManagedMarkerPath()
	content := fmt.Sprintf("Managed by ShiftLaunch\nCreated at: %s\n", time.Now().Format(time.RFC3339))
	return os.WriteFile(managedPath, []byte(content), 0644)
}

// IsManaged checks if the cluster is managed by ShiftLaunch
func (sm *StateManager) IsManaged() bool {
	_, err := os.Stat(sm.GetManagedMarkerPath())
	return err == nil
}

// AddCommandExecution adds a command execution record to the state
func (sm *StateManager) AddCommandExecution(state *DeploymentState, cmd CommandExecution) {
	if state.CommandHistory == nil {
		state.CommandHistory = []CommandExecution{}
	}
	state.CommandHistory = append(state.CommandHistory, cmd)
}

// AddPhaseExecution adds a phase execution record to the state
func (sm *StateManager) AddPhaseExecution(state *DeploymentState, phase PhaseExecution) {
	if state.PhaseHistory == nil {
		state.PhaseHistory = []PhaseExecution{}
	}
	state.PhaseHistory = append(state.PhaseHistory, phase)
}

// AddConfigBackup records a config backup file
func (sm *StateManager) AddConfigBackup(state *DeploymentState, backupPath string) {
	if state.ConfigBackups == nil {
		state.ConfigBackups = []string{}
	}
	state.ConfigBackups = append(state.ConfigBackups, backupPath)
	state.LastConfigUpdate = time.Now().Format(time.RFC3339)
}

// Legacy functions for backward compatibility

// LoadState reads the state file from disk (legacy function)
func LoadState(clusterName string) (*DeploymentState, error) {
	sm := NewStateManager(clusterName)
	return sm.LoadState()
}

// Save writes the state file to disk (legacy method)
func (s *DeploymentState) Save() error {
	sm := NewStateManager(s.ClusterName)
	return sm.SaveState(s)
}

// GetFailedMarkerPath returns the path to the failed marker file
func (sm *StateManager) GetFailedMarkerPath() string {
	return filepath.Join(sm.workspaceDir, ".failed")
}

// MarkFailed creates the .failed marker file
func (sm *StateManager) MarkFailed() error {
	failedPath := sm.GetFailedMarkerPath()
	content := fmt.Sprintf("Deployment failed at: %s\n", time.Now().Format(time.RFC3339))
	return os.WriteFile(failedPath, []byte(content), 0644)
}

// ClearFailed removes the .failed marker file
func (sm *StateManager) ClearFailed() error {
	failedPath := sm.GetFailedMarkerPath()
	if _, err := os.Stat(failedPath); err == nil {
		return os.Remove(failedPath)
	}
	return nil
}

// IsFailed checks if the cluster deployment is in a failed state
func (sm *StateManager) IsFailed() bool {
	_, err := os.Stat(sm.GetFailedMarkerPath())
	return err == nil
}