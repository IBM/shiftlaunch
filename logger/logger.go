package logger

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/log"
)

type Logger struct {
	consoleLogger *log.Logger
	fileLogger    *log.Logger
	file          *os.File
	debug         bool
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
	// For the file: Keep the raw structured data
	if l.fileLogger != nil {
		l.fileLogger.Info(msg, keyvals...)
	}

	// For the console: If there are key-values, format them nicely
	if len(keyvals) > 0 && len(keyvals)%2 == 0 {
		var formattedMsg string = msg + " ("
		for i := 0; i < len(keyvals); i += 2 {
			formattedMsg += fmt.Sprintf("%v: %v", keyvals[i], keyvals[i+1])
			if i < len(keyvals)-2 {
				formattedMsg += ", "
			}
		}
		formattedMsg += ")"
		l.consoleLogger.Info(formattedMsg)
	} else {
		l.consoleLogger.Info(msg)
	}
}

func (l *Logger) Debug(msg string, keyvals ...interface{}) {
	l.consoleLogger.Debug(msg, keyvals...)
	if l.fileLogger != nil {
		l.fileLogger.Debug(msg, keyvals...)
	}
}

func (l *Logger) Error(msg string, keyvals ...interface{}) {
	l.consoleLogger.Error(msg, keyvals...)
	if l.fileLogger != nil {
		l.fileLogger.Error(msg, keyvals...)
	}
}

func (l *Logger) Warn(msg string, keyvals ...interface{}) {
	l.consoleLogger.Warn(msg, keyvals...)
	if l.fileLogger != nil {
		l.fileLogger.Warn(msg, keyvals...)
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

// Phase prints a phase header message
func (l *Logger) Phase(msg string, keyvals ...interface{}) {
	l.consoleLogger.Info(msg, keyvals...)
	if l.fileLogger != nil {
		l.fileLogger.Info(msg, keyvals...)
	}
}

// Close closes the log file if it was opened
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
