package hostclock

import (
	"errors"

	"golang.org/x/sys/unix"
)

// kern.bootsessionuuid is fixed for one boot; kern.boottime moves when
// the calendar clock is set.
func boot() (string, error) {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("hostclock: empty boot session")
	}
	return "darwin-" + id, nil
}

// CLOCK_MONOTONIC on darwin counts time asleep (mach_continuous_time).
func mono() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}
