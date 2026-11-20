//go:build darwin

package ident

import (
	"encoding/binary"
	"errors"
	"syscall"
	"unsafe"
)

// processStartMS returns the wall-clock start of a running pid in Unix
// milliseconds. kinfo_proc begins with extern_proc: the start timeval at 0,
// p_stat at 36, p_pid at 40. A zombie (exited, not yet reaped) does not run.
func processStartMS(pid int) (int64, error) {
	mib := [4]int32{1 /* CTL_KERN */, 14 /* KERN_PROC */, 1 /* KERN_PROC_PID */, int32(pid)}
	var buf [1024]byte // kinfo_proc is 648 bytes on 64-bit darwin
	size := uintptr(len(buf))
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	if size < 44 || int32(binary.LittleEndian.Uint32(buf[40:44])) != int32(pid) {
		return 0, errors.New("process not found")
	}
	if buf[36] == 5 /* SZOMB */ {
		return 0, errors.New("process exited")
	}
	sec := int64(binary.LittleEndian.Uint64(buf[0:8]))
	usec := int64(binary.LittleEndian.Uint32(buf[8:12]))
	if sec <= 0 {
		return 0, errors.New("process start unavailable")
	}
	return sec*1000 + usec/1000, nil
}
