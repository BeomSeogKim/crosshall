//go:build !linux

package proc

func PIDNamespace() string { return "" }

func probeProcessStart(pid int) (Start, error) { return processStart(pid) }
