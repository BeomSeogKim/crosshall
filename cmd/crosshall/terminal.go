//go:build darwin || linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal: a character device that
// answers the terminal attributes request. /dev/null and pipes do not.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), termiosRequest, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func flushTerminalInput(f *os.File) error {
	if !isTerminal(f) {
		return nil
	}
	return discardTerminalInput(int(f.Fd()))
}
