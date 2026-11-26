//go:build !darwin && !linux

package hostclock

func boot() (string, error) { return "", errUnsupported }

func mono() (int64, error) { return 0, errUnsupported }
