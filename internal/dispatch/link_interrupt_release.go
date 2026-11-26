//go:build !testvendorhome

package dispatch

func linkInterrupt(int) error { return nil }
