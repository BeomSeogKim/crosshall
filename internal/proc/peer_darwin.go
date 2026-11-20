//go:build darwin

package proc

import "syscall"

const (
	solLocal     = 0
	localPeerPID = 2
)

func peerPID(fd uintptr) (int, error) {
	return syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
}
