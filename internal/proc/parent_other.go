//go:build !darwin && !linux

package proc

import "errors"

func processParent(int) (string, int, error) { return "", 0, errors.New("process parent unavailable") }
