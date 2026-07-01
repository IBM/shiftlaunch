package logger

import (
	"fmt"
	"io"
	"os"
	"time"

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

	pterm.SetDefaultOutput(os.Stderr)

	// 1. Attempt to open the log file if a path is provided
	if logPath != "" {
		file, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			// Create file logger (colors will be auto-disabled for file)
			fileOpts := log.Options{
				ReportTimestamp: true,
				Prefix:          "ShiftLaunch",
				// File always captures full debug — the debug flag only gates the terminal.
				Level: log.DebugLevel,
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

	if err != nil {
		// Log file is optional — report to stderr but do not abort the caller.
		fmt.Fprintf(os.Stderr, "warning: could not open log file %q: %v\n", logPath, err)
	}

	return &Logger{
		consoleLogger: consoleLogger,
		fileLogger:    fileLogger,
		file:          file,
		debug:         debug,
	}, nil
}

func (l *Logger) Info(msg string, keyvals ...interface{}) {
	// 1. ALWAYS write every single piece of telemetry to the background log
	if l.fileLogger != nil {
		l.fileLogger.Info(msg, keyvals...)
	}

	formattedMsg := formatKV(msg, keyvals...)

	// 2. Feed the intermediate logs into the spinner!
	if l.activeSpinner != nil && !l.debug {
		plainText := pterm.RemoveColorFromString(formattedMsg)

		// 🛡️ THE DYNAMIC FIX
		// Get the actual width of the user's terminal window
		termWidth := pterm.GetTerminalWidth()
		if termWidth <= 0 {
			termWidth = 80 // Safe fallback if terminal width cannot be detected
		}

		// Reserve ~18 characters for the spinner prefix ("▀ ") and timer suffix (" (12m34s)")
		maxLen := termWidth - 18

		// Only truncate if the text is ACTUALLY going to wrap and break the UI
		if len(plainText) > maxLen {
			plainText = plainText[:maxLen-3] + "..."
		}

		l.activeSpinner.UpdateText(plainText + "\033[K")
		return
	}

	// 3. Otherwise, print cleanly to terminal
	l.consoleLogger.Info(formattedMsg)
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
		termWidth := pterm.GetTerminalWidth()
		if termWidth <= 0 {
			termWidth = 80
		}
		maxLen := termWidth - 18
		if len(formatted) > maxLen {
			formatted = formatted[:maxLen-3] + "..."
		}
		// Force Error to respect the Stderr stream lock
		fmt.Fprint(os.Stderr, "\r\033[2K")
		pterm.Error.WithWriter(os.Stderr).Println(formatted)
	} else {
		l.consoleLogger.Error(formatted)
	}
}

func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	if l.fileLogger != nil {
		l.fileLogger.Warn(msg, keyvals...)
	}
	formatted := formatKV(msg, keyvals...)

	if l.activeSpinner != nil && !l.debug {
		termWidth := pterm.GetTerminalWidth()
		if termWidth <= 0 {
			termWidth = 80
		}
		maxLen := termWidth - 18
		if len(formatted) > maxLen {
			formatted = formatted[:maxLen-3] + "..."
		}
		// Force Warning to respect the Stderr stream lock
		fmt.Fprint(os.Stderr, "\r\033[2K")
		pterm.Warning.WithWriter(os.Stderr).Println(formatted)
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
	return os.Stderr
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
		pterm.NewStyle(pterm.FgCyan, pterm.Bold).Println(msg)
		return
	}

	if l.activeSpinner != nil {
		_ = l.activeSpinner.Stop()
	}

	// 1. THE STREAM SYNC  Explicitly map to Stderr
	// 2. THE CLEANUP  Force the thread to erase itself when stopped
	spinner, _ := pterm.DefaultSpinner.
		WithWriter(os.Stderr).
		WithRemoveWhenDone(true).
		Start(pterm.Cyan(msg))

	l.activeSpinner = spinner

	// 3.   Yield to the Go scheduler!
	// Archiving takes <1ms. We MUST pause for a tiny fraction of a second to guarantee
	// the background thread wakes up and stabilizes before EndPhase tries to kill it!
	time.Sleep(10 * time.Millisecond)
}

// EndPhase cleanly stops the spinner and marks it with a check or cross
func (l *Logger) EndPhase(success bool, msg string) {
	if l.activeSpinner == nil {
		return
	}

	spinner := l.activeSpinner
	l.activeSpinner = nil

	// Safely stop the animation. Because of the 10ms yield, the thread is
	// guaranteed to be stable and will cleanly erase its line.
	_ = spinner.Stop()

	// Print the pristine status message directly to Stderr to prevent desyncs
	if success {
		pterm.Success.WithWriter(os.Stderr).Println(pterm.Cyan(msg))
	} else {
		pterm.Error.WithWriter(os.Stderr).Println(pterm.Red(msg))
	}
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
