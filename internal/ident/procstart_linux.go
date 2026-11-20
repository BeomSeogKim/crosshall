//go:build linux

package ident

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// userHZ is the kernel's fixed clock-tick unit for /proc times.
const userHZ = 100

// startErrorMS covers the 10 ms units of the start ticks and of the boot
// estimate: a start is reported up to this much early, never late, so a
// running launch is never taken for a later process. On Linux a PID reused
// within this margin after a launch's first hook reads as that launch.
const startErrorMS = 2 * 1000 / userHZ

// boot holds this process's one estimate of the boot time. Each estimate
// from the wall clock and /proc/uptime lands in a different 10 ms bucket;
// one estimate gives a process the same start on every call.
var boot struct {
	sync.Mutex
	ms int64
}

func bootMS() (int64, error) {
	boot.Lock()
	defer boot.Unlock()
	if boot.ms != 0 {
		return boot.ms, nil
	}
	now := time.Now().UnixMilli()
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	up, _, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	seconds, err := strconv.ParseFloat(up, 64)
	if err != nil {
		return 0, err
	}
	boot.ms = now - int64(seconds*1000)
	return boot.ms, nil
}

// processStartMS returns the wall-clock start of a running pid in Unix
// milliseconds, at most startErrorMS early and the same on every call: the
// boot estimate plus the start ticks. A zombie or dead process does not run.
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
	at, err := bootMS()
	if err != nil {
		return 0, err
	}
	return at + ticks*1000/userHZ - startErrorMS, nil
}
