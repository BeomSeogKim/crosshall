//go:build conformance && linux

package conformance

import (
	"fmt"
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
	var unlock, number int32
	for _, call := range []struct{ request, arg uintptr }{
		{syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))}, {syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))},
	} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, control.Fd(), call.request, call.arg); errno != 0 {
			control.Close()
			return nil, "", errno
		}
	}
	return control, fmt.Sprintf("/dev/pts/%d", number), nil
}
