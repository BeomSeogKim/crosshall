package proc

import (
	"errors"
	"strconv"
	"strings"
)

// Start is when a process started. Identity is a kernel value that every
// reader gets the same for the life of the process, whatever its clock or
// when it reads; it names the process together with its PID. AtMS is the
// wall-clock start in Unix milliseconds, at most startErrorMS early, for
// comparisons with hook times only.
type Start struct {
	Identity int64
	AtMS     int64
}

// userHZ is the Linux kernel's fixed clock-tick unit for /proc times.
const userHZ = 100

// startErrorMS covers the 10 ms units of the Linux start ticks and of the
// boot estimate: a start is reported up to this much early, never late, so
// a running launch is never taken for a later process. On Linux a PID
// reused within this margin after a launch's first hook reads as that
// launch.
const startErrorMS = 2 * 1000 / userHZ

// linuxStart reads a /proc/<pid>/stat line. Its identity is the start in
// clock ticks after boot (field 22), which the kernel fixes; its wall-clock
// start adds bootMS, an estimate that differs between processes and after
// a clock step, so it never enters the identity.
func linuxStart(stat string, bootMS int64) (Start, error) {
	// The command name may contain spaces; fields resume after its ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return Start{}, errors.New("malformed process status")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return Start{}, errors.New("malformed process status")
	}
	if state := fields[0]; state == "Z" || state == "X" || state == "x" {
		return Start{}, errProcessGone
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || ticks < 0 {
		return Start{}, errors.New("malformed process status")
	}
	// +1 keeps the identity positive for a process started at tick 0.
	return Start{Identity: ticks + 1, AtMS: bootMS + ticks*1000/userHZ - startErrorMS}, nil
}

// processStartMS is the wall-clock start of a running pid.
func processStartMS(pid int) (int64, error) {
	start, err := processStart(pid)
	return start.AtMS, err
}
