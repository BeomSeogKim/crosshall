//go:build conformance && darwin

package conformance

import (
	"os"
	"syscall"
	"unsafe"
)

// openPTY opens a pseudo-terminal and returns its controlling side and the
// path of the side a child process uses as its terminal.
func openPTY() (*os.File, string, error) {
	control, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var name [128]byte
	for _, call := range []struct{ request, arg uintptr }{
		{syscall.TIOCPTYGRANT, 0}, {syscall.TIOCPTYUNLK, 0}, {syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))},
	} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, control.Fd(), call.request, call.arg); errno != 0 {
			control.Close()
			return nil, "", errno
		}
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	return control, string(name[:end]), nil
}
