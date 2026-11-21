package proc

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PIDNamespace combines the kernel boot ID with this process's PID namespace
// inode. The boot component distinguishes inodes reused across a reboot.
func PIDNamespace() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	bootID := strings.TrimSpace(string(raw))
	digits := strings.ReplaceAll(bootID, "-", "")
	if len(bootID) != 36 || len(digits) != 32 || strings.Trim(digits, "0123456789abcdef") != "" {
		return ""
	}
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return ""
	}
	return bootID + ":" + strconv.FormatUint(uint64(stat.Ino), 10)
}

func probeProcessStart(pid int) (Start, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Start{}, err
	}
	if !localProcFS(string(status), os.Getpid()) {
		return Start{}, errors.New("procfs PID namespace unavailable or different")
	}
	return processStart(pid)
}

func localProcFS(status string, pid int) bool {
	// NSpid begins at the procfs mount's namespace and ends at our own.
	for line := range strings.SplitSeq(status, "\n") {
		if values, ok := strings.CutPrefix(line, "NSpid:"); ok {
			pids := strings.Fields(values)
			return len(pids) == 1 && pids[0] == strconv.Itoa(pid)
		}
	}
	return false
}
