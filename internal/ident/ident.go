// Package ident defines the dispatcher-side identity boundary.
package ident

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Ingress describes the receiving spool selected by the dispatcher.
type Ingress struct {
	Path  string
	Owner bool
}

// Proof contains the member envelope fields checked against the receiving spool.
type Proof struct {
	Generation string
	KeyDigest  string
}

// Resolver checks ingress evidence and resolves the caller within the transaction.
type Resolver interface {
	Resolve(ctx context.Context, tx store.Tx, ingress Ingress, proof Proof) (core.Caller, error)
}
