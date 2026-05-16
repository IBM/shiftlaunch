package infra

import (
	"strings"

	"github.com/charmbracelet/log"
	"github.com/sudeeshjohn/shiftlaunch/logger"
	hmc "github.ibm.com/sudeeshjohn/infra-go-sdk/phmc"
)

// logWriter acts as a bridge between the standard io.Writer and our custom logger
type logWriter struct {
	logger *logger.Logger
}

func (lw *logWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		// Route through Debug so it respects the spinner UI and file outputs
		lw.logger.Debug("[HMC API] " + msg)
	}
	return len(p), nil
}

// NewHMCLoggerAdapter creates an HMC logger that integrates safely with the spinner UI
func NewHMCLoggerAdapter(shiftlaunchLogger *logger.Logger, debug bool) *hmc.Logger {
	var level log.Level

	if debug {
		level = log.DebugLevel
	} else {
		// Suppress completely if not in debug mode
		level = log.ErrorLevel
	}

	// Route all HMC SDK traffic securely through our custom logging engine
	safeOutput := &logWriter{logger: shiftlaunchLogger}

	hmc.ReinitLogger(safeOutput)

	hmcLogger := hmc.NewLogger(level, safeOutput)
	hmcLogger.SetPrefix("") // Prefix handled by the logWriter

	return hmcLogger
}