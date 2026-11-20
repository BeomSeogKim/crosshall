//go:build !darwin && !linux

package ident

import "errors"

func processStartMS(int) (int64, error) { return 0, errors.New("process start unavailable") }
