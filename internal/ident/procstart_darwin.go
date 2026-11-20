//go:build darwin

package ident

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"
)

// processStartMS returns the wall-clock start of pid in Unix milliseconds.
// kinfo_proc begins with extern_proc, whose first member is the start timeval.
func processStartMS(pid int) (int64, error) {
	mib := [4]int32{1 /* CTL_KERN */, 14 /* KERN_PROC */, 1 /* KERN_PROC_PID */, int32(pid)}
	var buf [1024]byte // kinfo_proc is 648 bytes on 64-bit darwin
	size := uintptr(len(buf))
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	if size < 16 {
		return 0, errors.New("process not found")
	}
	sec := int64(binary.LittleEndian.Uint64(buf[0:8]))
	usec := int64(binary.LittleEndian.Uint32(buf[8:12]))
	if sec <= 0 {
		return 0, errors.New("process start unavailable")
	}
	return sec*1000 + usec/1000, nil
}
