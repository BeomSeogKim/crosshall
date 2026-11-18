// Package store defines the transaction boundary for the local database.
package store

import (
	"context"
	"database/sql"
)

// Tx exposes transaction operations to command handlers. The dispatcher
// controls commit and rollback.
type Tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
