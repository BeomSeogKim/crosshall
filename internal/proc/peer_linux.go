//go:build linux

package proc

import "syscall"

func peerPID(fd uintptr) (int, error) {
	ucred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(ucred.Pid), nil
}
