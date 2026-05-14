package infra

import (
	"io"

	"github.com/charmbracelet/log"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	hmc "github.ibm.com/sudeeshjohn/infra-go-sdk/phmc"
)

// NewHMCLoggerAdapter creates an HMC logger that integrates with shiftlaunch's logging system.
// It writes all HMC API logs to the deployment log file and optionally to the terminal based on debug flag.
func NewHMCLoggerAdapter(shiftlaunchLogger *logger.Logger, debug bool) *hmc.Logger {
	var terminalOutput io.Writer
	var level log.Level

	if debug {
		// In debug mode: write to file AND show in terminal
		terminalOutput = shiftlaunchLogger.TerminalOnly()
		level = log.DebugLevel
	} else {
		// In normal mode: write to file only, suppress terminal output completely
		terminalOutput = io.Discard
		level = log.DebugLevel // Set to DebugLevel so API calls are logged to file
	}

	// Always write to file, optionally write to terminal
	output := io.MultiWriter(shiftlaunchLogger.FileOnly(), terminalOutput)

	// Reinitialize the global hmcLogger used by utility functions
	// This ensures payload creation logs also go to the right destination
	hmc.ReinitLogger(output)

	// Create HMC logger with the appropriate output destination
	hmcLogger := hmc.NewLogger(level, output)
	hmcLogger.SetPrefix("[HMC]")

	return hmcLogger
}