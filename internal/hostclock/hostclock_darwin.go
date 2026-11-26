package hostclock

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func boot() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("darwin-%d.%06d", tv.Sec, tv.Usec), nil
}

// CLOCK_MONOTONIC on darwin counts time asleep (mach_continuous_time).
func mono() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}
