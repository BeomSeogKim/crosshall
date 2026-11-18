// Command crosshall runs the local Crosshall CLI.
package main

import (
	"os"

	"github.com/BeomSeogKim/crosshall/internal/cli"
)

// Release builds set version with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	os.Exit(int(cli.Run(os.Args[1:], os.Stdout, os.Stderr, version)))
}
