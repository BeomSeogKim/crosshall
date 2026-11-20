//go:build !darwin && !linux

package proc

import "errors"

func peerPID(uintptr) (int, error) {
	return 0, errors.New("peer PID check unsupported on this platform")
}
