//go:build darwin

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const termiosRequest = syscall.TIOCGETA

func discardTerminalInput(fd int) error {
	const fRead = 0x1 // FREAD in sys/fcntl.h selects the input queue for TIOCFLUSH.
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fRead)
}
