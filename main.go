package main

import (
	"os"

	"github.com/sudeeshjohn/shiftlaunch/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// Error is already logged via logger.Error() in the code
		// This ensures we exit with error code without duplicate terminal output
		os.Exit(1)
	}
}

// Made with Bob