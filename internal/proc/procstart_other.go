//go:build !darwin && !linux

package proc

import "errors"

func processStart(int) (Start, error) { return Start{}, errors.New("process start unavailable") }
