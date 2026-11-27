//go:build linux

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const termiosRequest = syscall.TCGETS

func discardTerminalInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
