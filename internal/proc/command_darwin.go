//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// processCommand returns a running pid's executable path, as the kernel
// recorded it at exec, and its arguments (kern.procargs2: argc, the path,
// padding, then the arguments, then the environment).
func processCommand(pid int) (string, []string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", nil, err
	}
	if len(raw) < 4 {
		return "", nil, errors.New("short process arguments")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end <= 0 {
		return "", nil, errors.New("malformed process arguments")
	}
	exe := string(rest[:end])
	rest = bytes.TrimLeft(rest[end:], "\x00")
	args := make([]string, 0, argc)
	for range argc {
		end = bytes.IndexByte(rest, 0)
		if end < 0 {
			return "", nil, errors.New("malformed process arguments")
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}
	return exe, args, nil
}
