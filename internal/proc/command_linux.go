//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
)

// processCommand returns a running pid's executable path and its arguments.
func processCommand(pid int) (string, []string, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	exe, err := os.Readlink(dir + "/exe")
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return "", nil, err
	}
	return exe, strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
}
