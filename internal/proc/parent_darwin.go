//go:build darwin

package proc

import "golang.org/x/sys/unix"

// processParent returns a running pid's command name and parent pid.
func processParent(pid int) (string, int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", 0, err
	}
	if int(kp.Proc.P_pid) != pid {
		return "", 0, errProcessGone
	}
	return unix.ByteSliceToString(kp.Proc.P_comm[:]), int(kp.Eproc.Ppid), nil
}
