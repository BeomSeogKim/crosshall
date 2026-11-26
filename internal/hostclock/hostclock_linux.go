package hostclock

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func boot() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errUnsupported
	}
	return "linux-" + id, nil
}

// CLOCK_BOOTTIME counts time suspended; CLOCK_MONOTONIC does not.
func mono() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}
