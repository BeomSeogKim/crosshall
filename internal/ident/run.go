package ident

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// OwnerGate is called after parsing the owner command prefix.
func OwnerGate() string {
	if AllowOwner(os.LookupEnv, true) {
		return ""
	}
	return "owner commands require a separate owner shell"
}

// RunChild implements the CLI's member launcher.
func RunChild(argv []string) core.ExitCode {
	if err := Exec(argv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return core.ExitError
	}
	return core.ExitOK
}

// Exec starts a member launch in place, preserving the command's arguments,
// terminal and exit status.
func Exec(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("missing command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, childEnvironment(os.Environ()))
}

func childEnvironment(environ []string) []string {
	child := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if name == RouteVariable || isVendorVariable(name) {
			continue
		}
		child = append(child, entry)
	}
	var random [16]byte
	rand.Read(random[:])
	return append(child, RouteVariable+"="+hex.EncodeToString(random[:]))
}

func isVendorVariable(name string) bool {
	for _, row := range vendorVariables {
		if row.name == name {
			return true
		}
	}
	return false
}
