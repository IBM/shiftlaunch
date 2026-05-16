package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sudeeshjohn/shiftlaunch/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// If the error is a Cobra syntax/flag error, it bypassed our custom logger. We must print it!
		if strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "invalid argument") || strings.Contains(err.Error(), "unknown command") {
			fmt.Fprintf(os.Stderr, "❌ CLI Error: %v\nRun 'shiftlaunch --help' for usage.\n", err)
		}
		// Exit with error code
		os.Exit(1)
	}
}

// Made with Bob