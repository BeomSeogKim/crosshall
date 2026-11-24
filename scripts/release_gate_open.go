//go:build ignore

// Run from a repository root: go run ./scripts/release_gate_open.go <state-root>
// scripts/release-gate.sh uses it to migrate a store copy with the production
// opener, without the maintenance a dispatcher would run after it.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: release_gate_open.go <state-root>")
		os.Exit(2)
	}
	s, err := store.Open(context.Background(), os.Args[1])
	if err == nil {
		err = s.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
