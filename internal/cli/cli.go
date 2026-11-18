// Package cli parses command-line arguments and renders command results.
package cli

import (
	"fmt"
	"io"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

const usage = `Usage: crosshall <command>

Commands:
  help     Show this help
  version  Print the build version
`

// Run executes the command line and returns the exit status.
func Run(args []string, stdout, stderr io.Writer, version string) core.ExitCode {
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, usage)
		return core.ExitOK
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return core.ExitOK
	}
	fmt.Fprintln(stderr, "Usage: crosshall <help|version>")
	return core.ExitUsage
}
