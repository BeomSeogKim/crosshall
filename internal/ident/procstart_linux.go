//go:build linux

package ident

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the kernel's fixed clock-tick unit for /proc times.
const userHZ = 100

// startErrorMS covers the 10 ms units of the start ticks and of /proc/uptime:
// the start is reported this much early, never late, so a running launch is
// never taken for a later process.
const startErrorMS = 2 * 1000 / userHZ

// processStartMS returns the wall-clock start of a running pid in Unix
// milliseconds, at most startErrorMS early: the boot time from /proc/uptime
// plus the start ticks. A zombie or dead process does not run.
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
	if state := fields[0]; state == "Z" || state == "X" || state == "x" {
		return 0, errors.New("process exited")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixMilli()
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	up, _, _ := strings.Cut(strings.TrimSpace(string(uptime)), " ")
	seconds, err := strconv.ParseFloat(up, 64)
	if err != nil {
		return 0, err
	}
	return now - int64(seconds*1000) + ticks*1000/userHZ - startErrorMS, nil
}
