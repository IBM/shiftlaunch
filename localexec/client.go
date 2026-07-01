package localexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IBM/shiftlaunch/logger"
	"github.com/IBM/shiftlaunch/types"
)

type LocalClient struct {
	logger       *logger.Logger
	stateManager *types.StateManager
	state        *types.DeploymentState
}

func NewLocalClient(log *logger.Logger) *LocalClient {
	return &LocalClient{logger: log}
}

// NewLocalClientWithState creates a state-aware local client for granular resume
func NewLocalClientWithState(log *logger.Logger, sm *types.StateManager, state *types.DeploymentState) *LocalClient {
	return &LocalClient{
		logger:       log,
		stateManager: sm,
		state:        state,
	}
}

// Update Execute to take a context
func (l *LocalClient) Execute(ctx context.Context, command string) (string, error) {
	if l.logger != nil {
		l.logger.Debug("Executing local command", "command", command)
	}

	// Use CommandContext instead of Command
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(output))

	if err != nil {
		// If the context was canceled (e.g. Ctrl+C), report it cleanly
		if ctx.Err() != nil {
			return outStr, fmt.Errorf("command aborted by user: %w", ctx.Err())
		}
		l.logger.Debug("Command failed", "cmd", command, "error", err, "output", outStr)
		return outStr, fmt.Errorf("local execution failed: %w (output: %s)", err, outStr)
	}

	l.logger.Debug("Command succeeded", "output", outStr)
	return outStr, nil
}

// WriteFile writes content directly to the local filesystem (with sudo if needed)
func (l *LocalClient) WriteFile(ctx context.Context, path string, content []byte, perms os.FileMode) error {
	l.logger.Debug("Writing local file", "path", path)

	// Create temp file with the PID embedded so concurrent shiftlaunch processes
	// (e.g. two simultaneous `shiftlaunch create` runs) never share the same path.
	tmpPath := filepath.Join("/tmp", fmt.Sprintf("%s-%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmpPath, content, perms); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Move into place with sudo (required for /etc/ directories)
	mvCmd := fmt.Sprintf("sudo mv %s %s && sudo chmod %04o %s && sudo restorecon %s 2>/dev/null || true", tmpPath, path, perms, path, path)

	// Shield the move-and-permission chain.
	// If aborted mid-chain, the config file will have the wrong SELinux context, permanently breaking the service!
	if _, err := l.Execute(context.WithoutCancel(ctx), mvCmd); err != nil {
		return fmt.Errorf("failed to move file into place: %w", err)
	}

	return nil
}

func (l *LocalClient) SystemctlRestart(ctx context.Context, service string) error {
	l.logger.Debug("Restarting local service", "service", service)
	_, err := l.Execute(ctx, fmt.Sprintf("sudo systemctl restart %s", service))
	return err
}

func (l *LocalClient) SystemctlEnable(ctx context.Context, service string) error {
	_, err := l.Execute(ctx, fmt.Sprintf("sudo systemctl enable --now %s", service))
	return err
}

// ExecuteWithState checks if an event exists. If not, it runs the command and records the event.
func (l *LocalClient) ExecuteWithState(ctx context.Context, command string, eventID string) (string, error) {
	// If no state tracking, fall back to regular execution
	if eventID == "" || l.stateManager == nil || l.state == nil {
		return l.Execute(ctx, command)
	}

	// Check if already completed
	if l.stateManager.IsEventCompleted(l.state, eventID) {
		l.logger.Debug("Skipping command, already completed", "eventID", eventID, "command", command)
		return "", nil
	}

	// Execute the command
	outStr, err := l.Execute(ctx, command)

	// Record success
	if err == nil {
		if recordErr := l.stateManager.RecordCompletedEvent(l.state, eventID); recordErr != nil {
			l.logger.Warn("Failed to record completed event", "eventID", eventID, "error", recordErr)
		} else {
			l.logger.Debug("Recorded completed event", "eventID", eventID)
		}
	}

	return outStr, err
}

// WriteFileWithState safely writes a file and records it as an event
func (l *LocalClient) WriteFileWithState(ctx context.Context, path string, content []byte, perms os.FileMode, eventID string) error {
	// If no state tracking, fall back to regular write
	if eventID == "" || l.stateManager == nil || l.state == nil {
		return l.WriteFile(ctx, path, content, perms)
	}

	// Check if already completed
	if l.stateManager.IsEventCompleted(l.state, eventID) {
		l.logger.Debug("Skipping file write, already completed", "eventID", eventID, "file", path)
		return nil
	}

	// Write the file
	err := l.WriteFile(ctx, path, content, perms)

	// Record success
	if err == nil {
		if recordErr := l.stateManager.RecordCompletedEvent(l.state, eventID); recordErr != nil {
			l.logger.Warn("Failed to record completed event", "eventID", eventID, "error", recordErr)
		} else {
			l.logger.Debug("Recorded completed event", "eventID", eventID)
		}
	}

	return err
}

// SystemctlRestartWithState restarts a service and records it as an event
func (l *LocalClient) SystemctlRestartWithState(ctx context.Context, service string, eventID string) error {
	if eventID == "" || l.stateManager == nil || l.state == nil {
		return l.SystemctlRestart(ctx, service)
	}

	if l.stateManager.IsEventCompleted(l.state, eventID) {
		l.logger.Debug("Skipping service restart, already completed", "eventID", eventID, "service", service)
		return nil
	}

	err := l.SystemctlRestart(ctx, service)

	if err == nil {
		if recordErr := l.stateManager.RecordCompletedEvent(l.state, eventID); recordErr != nil {
			l.logger.Warn("Failed to record completed event", "eventID", eventID, "error", recordErr)
		}
	}

	return err
}

// SystemctlEnableWithState enables a service and records it as an event
func (l *LocalClient) SystemctlEnableWithState(ctx context.Context, service string, eventID string) error {
	if eventID == "" || l.stateManager == nil || l.state == nil {
		return l.SystemctlEnable(ctx, service)
	}

	if l.stateManager.IsEventCompleted(l.state, eventID) {
		l.logger.Debug("Skipping service enable, already completed", "eventID", eventID, "service", service)
		return nil
	}

	err := l.SystemctlEnable(ctx, service)

	if err == nil {
		if recordErr := l.stateManager.RecordCompletedEvent(l.state, eventID); recordErr != nil {
			l.logger.Warn("Failed to record completed event", "eventID", eventID, "error", recordErr)
		}
	}

	return err
}
