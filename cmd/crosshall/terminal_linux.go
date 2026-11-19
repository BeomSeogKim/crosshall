//go:build linux

package main

import "syscall"

const termiosRequest = syscall.TCGETS
