package logger

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/log"
	"github.com/pterm/pterm"
)

type Logger struct {
	consoleLogger *log.Logger
	fileLogger    *log.Logger
	file          *os.File
	debug         bool
	activeSpinner *pterm.SpinnerPrinter
}

// New sets up the dual-writer logging system with separate console and file loggers
func New(debug bool, logPath string) (*Logger, error) {
	var file *os.File
	var fileLogger *log.Logger
	var err error

	// 1. Attempt to open the log file if a path is provided
	if logPath != "" {
		file, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil && file != nil {
			// Create file logger (colors will be auto-disabled for file)
			fileOpts := log.Options{
				ReportTimestamp: true,
				Prefix:          "ShiftLaunch",
			}
			if debug {
				fileOpts.Level = log.DebugLevel
			} else {
				fileOpts.Level = log.InfoLevel
			}
			fileLogger = log.NewWithOptions(file, fileOpts)
		}
	}

	// 2. Create console logger (clean UI for the human)
	consoleOpts := log.Options{
		ReportTimestamp: false, // Turn off dates/times in terminal
		Prefix:          "",    // Turn off the "ShiftLaunch:" prefix
	}
	if debug {
		consoleOpts.Level = log.DebugLevel
	} else {
		consoleOpts.Level = log.InfoLevel
	}
	consoleLogger := log.NewWithOptions(os.Stderr, consoleOpts)

	return &Logger{
		consoleLogger: consoleLogger,
		fileLogger:    fileLogger,
		file:          file,
		debug:         debug,
	}, err
}

func (l *Logger) Info(msg string, keyvals ...interface{}) {
	if l.fileLogger != nil {
		l.fileLogger.Info(msg, keyvals...)
	}

	formattedMsg := formatKV(msg, keyvals...)

	// INTERCEPT: If a spinner is active, update its text instead of printing a new line!
	if l.activeSpinner != nil && !l.debug {
		// Truncate strings that are too long so they don't wrap and break the terminal
		if len(formattedMsg) > 85 {
			formattedMsg = formattedMsg[:82] + "..."
		}
		
		// Pad with spaces to exactly 85 characters.
		// This overwrites ghost characters and aligns the pterm timer on the right!
		paddedMsg := fmt.Sprintf("%-85s", formattedMsg)
		l.activeSpinner.UpdateText(paddedMsg)
	} else {
		l.consoleLogger.Info(formattedMsg)
	}
}

func (l *Logger) Debug(msg string, keyvals ...interface{}) {
	l.consoleLogger.Debug(msg, keyvals...)
	if l.fileLogger != nil {
		l.fileLogger.Debug(msg, keyvals...)
	}
}

func (l *Logger) Error(msg string, keyvals ...interface{}) {
	if l.fileLogger != nil {
		l.fileLogger.Error(msg, keyvals...)
	}
	formatted := formatKV(msg, keyvals...)
	
	if l.activeSpinner != nil && !l.debug {
		// Apply same padding for consistency
		if len(formatted) > 85 {
			formatted = formatted[:82] + "..."
		}
		paddedMsg := fmt.Sprintf("%-85s", formatted)
		pterm.Error.Println(paddedMsg)
	} else {
		l.consoleLogger.Error(formatted)
	}
}

func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	if l.fileLogger != nil {
		l.fileLogger.Warn(msg, keyvals...)
	}
	formatted := formatKV(msg, keyvals...)
	
	// pterm handles printing warnings safely above an active spinner
	if l.activeSpinner != nil && !l.debug {
		// Apply same padding for consistency
		if len(formatted) > 85 {
			formatted = formatted[:82] + "..."
		}
		paddedMsg := fmt.Sprintf("%-85s", formatted)
		pterm.Warning.Println(paddedMsg)
	} else {
		l.consoleLogger.Warn(formatted)
	}
}

// Capture safely executes a wrapped function.
func (l *Logger) Capture(f func()) {
	f()
}

// TerminalOnly returns an io.Writer that only writes to the console
func (l *Logger) TerminalOnly() io.Writer {
	return os.Stdout
}

// FileOnly returns an io.Writer that only writes to the log file
func (l *Logger) FileOnly() io.Writer {
	if l.file != nil {
		return l.file
	}
	return io.Discard
}

// StartPhase begins a spinner. If in debug mode, it falls back to a standard header.
func (l *Logger) StartPhase(msg string) {
	if l.fileLogger != nil {
		l.fileLogger.Info("=== " + msg + " ===")
	}

	if l.debug {
		//pterm.Println()
		pterm.NewStyle(pterm.FgCyan, pterm.Bold).Println(msg)
		return
	}

	//pterm.Println()
	spinner, _ := pterm.DefaultSpinner.WithText(pterm.Cyan(msg)).Start()
	l.activeSpinner = spinner
}

// EndPhase cleanly stops the spinner and marks it with a check or cross
func (l *Logger) EndPhase(success bool, msg string) {
	if l.activeSpinner == nil {
		return
	}
	if success {
		l.activeSpinner.Success(pterm.Cyan(msg))
	} else {
		l.activeSpinner.Fail(pterm.Red(msg))
	}
	l.activeSpinner = nil
}

// Phase prints a highly visible header to the console, while keeping the file log clean
// DEPRECATED: Use StartPhase/EndPhase for spinner-based phases
func (l *Logger) Phase(msg string, keyvals ...interface{}) {
	// Write standard plain text to the deployment.log file
	if l.fileLogger != nil {
		l.fileLogger.Info(msg, keyvals...)
	}

	// Format specifically for the Phase header: append " key=value"
	formattedMsg := msg
	if len(keyvals) > 0 && len(keyvals)%2 == 0 {
		for i := 0; i < len(keyvals); i += 2 {
			formattedMsg += fmt.Sprintf(" %v=%v", keyvals[i], keyvals[i+1])
		}
	}

	// For the console: Print as bold cyan text (no background banner)
	pterm.NewStyle(pterm.FgCyan, pterm.Bold).Println(formattedMsg)
}

// Close closes the log file if it was opened
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// formatKV replicates the native charmbracelet key=value formatting with dimmed keys
func formatKV(msg string, keyvals ...interface{}) string {
	if len(keyvals) == 0 || len(keyvals)%2 != 0 {
		return msg
	}

	formattedMsg := msg
	for i := 0; i < len(keyvals); i += 2 {
		// pterm.FgGray gives the key and '=' that subtle, dimmed aesthetic
		dimmedKey := pterm.FgGray.Sprintf(" %v=", keyvals[i])
		value := fmt.Sprintf("%v", keyvals[i+1])
		formattedMsg += dimmedKey + value
	}
	return formattedMsg
}
