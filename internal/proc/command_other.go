//go:build !darwin && !linux

package proc

import "errors"

func processCommand(int) (string, []string, error) {
	return "", nil, errors.New("process command unavailable")
}
