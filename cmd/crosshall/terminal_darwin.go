//go:build darwin

package main

import "syscall"

const termiosRequest = syscall.TIOCGETA
