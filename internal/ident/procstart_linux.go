//go:build linux

package ident

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// userHZ is the kernel's fixed clock-tick unit for /proc times.
const userHZ = 100

// processStartMS returns the wall-clock start of pid in Unix milliseconds,
// to the second of the boot time.
func processStartMS(pid int) (int64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// The command name may contain spaces; fields resume after its ')'.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, errors.New("malformed process status")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return 0, errors.New("malformed process status")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, err
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, err
			}
			return boot*1000 + ticks*1000/userHZ, nil
		}
	}
	return 0, errors.New("boot time unavailable")
}
