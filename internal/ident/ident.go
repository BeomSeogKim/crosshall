// Package ident defines the dispatcher-side identity boundary.
package ident

import (
	"context"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Ingress describes the receiving spool selected by the dispatcher.
type Ingress struct {
	Path  string
	Owner bool
	// Dir is the optional open leaf or owner directory used to claim this request.
	Dir *os.Root
}

// Proof carries the member route proof and optional version metadata.
type Proof struct {
	Generation    string
	KeyDigest     string
	VendorVersion string // diagnostic metadata only
}

// Resolution gives handlers the caller and a transaction-bound link for a first join.
type Resolution struct {
	Caller core.Caller
	Vendor string
	Key    string
	Enroll func(context.Context, store.Tx, int64) error
}

// Resolver checks ingress evidence and resolves the caller within the transaction.
type Resolver interface {
	Resolve(ctx context.Context, tx store.Tx, ingress Ingress, proof Proof) (Resolution, error)
}
