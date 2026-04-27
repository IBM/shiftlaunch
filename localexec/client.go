package localexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sudeeshjohn/shiftlaunch/logger"
)

type LocalClient struct {
	logger *logger.Logger
}

func NewLocalClient(log *logger.Logger) *LocalClient {
	return &LocalClient{logger: log}
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
func (l *LocalClient) WriteFile(ctx context.Context,path string, content []byte, perms os.FileMode) error {
	l.logger.Debug("Writing local file", "path", path)

	// Create temp file
	tmpPath := filepath.Join("/tmp", filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmpPath, content, perms); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Move into place with sudo (required for /etc/ directories)
	mvCmd := fmt.Sprintf("sudo mv %s %s && sudo chmod %04o %s && sudo restorecon %s 2>/dev/null || true", tmpPath, path, perms, path, path)
	if _, err := l.Execute(ctx,mvCmd); err != nil {
		return fmt.Errorf("failed to move file into place: %w", err)
	}

	return nil
}

func (l *LocalClient) SystemctlRestart(ctx context.Context, service string) error {
	l.logger.Info("Restarting local service", "service", service)
	_, err := l.Execute(ctx, fmt.Sprintf("sudo systemctl restart %s", service))
	return err
}

func (l *LocalClient) SystemctlEnable(ctx context.Context, service string) error {
	_, err := l.Execute(ctx, fmt.Sprintf("sudo systemctl enable --now %s", service))
	return err
}