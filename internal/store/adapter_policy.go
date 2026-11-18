package store

import (
	"context"
	"database/sql"
	"errors"
)

// AdapterPolicy is the owner's authorization for an adapter.
type AdapterPolicy struct {
	Adapter     string `json:"adapter"`
	Enabled     bool   `json:"enabled"`
	Reminder    bool   `json:"reminder"`
	Epoch       uint64 `json:"epoch"`
	ChangedAtMS int64  `json:"changed_at_ms"`
}

// ReadAdapterPolicy reads policy in the caller's transaction. A missing row
// represents a disabled adapter at epoch zero.
func ReadAdapterPolicy(ctx context.Context, tx Tx, adapter string) (AdapterPolicy, error) {
	policy := AdapterPolicy{Adapter: adapter}
	err := tx.QueryRowContext(ctx,
		`SELECT enabled, reminder, epoch, changed_at_ms FROM adapter_policy WHERE adapter = ?`, adapter).
		Scan(&policy.Enabled, &policy.Reminder, &policy.Epoch, &policy.ChangedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return policy, err
}
