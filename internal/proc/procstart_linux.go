//go:build linux

package proc

import (
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

// processStart reads a running pid's start; see linuxStart. A zombie or
// dead process does not run.
func processStart(pid int) (Start, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Start{}, procStatError(pid, err)
	}
	start, err := linuxStart(string(raw), 0)
	if err != nil {
		return Start{}, err
	}
	at, err := bootMS()
	if err != nil {
		// A missing wall-clock estimate does not erase the kernel identity.
		return Start{Identity: start.Identity}, err
	}
	start.AtMS += at
	return start, nil
}

func procStatError(pid int, err error) error {
	// A procfs mount can hide a live PID, so confirm absence with the kernel.
	if pid > 0 && pid <= math.MaxInt32 && errors.Is(err, os.ErrNotExist) && syscall.Kill(pid, 0) == syscall.ESRCH {
		return errProcessGone
	}
	return err
}
