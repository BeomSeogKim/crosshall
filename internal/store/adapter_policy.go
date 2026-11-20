package store

import (
	"context"
	"database/sql"
	"errors"
)

// AdapterPolicy is the owner's authorization for an adapter.
type AdapterPolicy struct {
	Adapter  string `json:"adapter"`
	Enabled  bool   `json:"enabled"`
	Reminder bool   `json:"reminder"`
	// Publish is the owner's publish switch for the adapter: "on", "off", or
	// empty when the hall switch decides.
	Publish     string `json:"publish,omitempty"`
	Epoch       uint64 `json:"epoch"`
	ChangedAtMS int64  `json:"changed_at_ms"`
}

// ReadAdapterPolicy reads policy in the caller's transaction. A missing row
// represents a disabled adapter at epoch zero.
func ReadAdapterPolicy(ctx context.Context, tx Tx, adapter string) (AdapterPolicy, error) {
	policy := AdapterPolicy{Adapter: adapter}
	var publish sql.NullBool
	err := tx.QueryRowContext(ctx,
		`SELECT enabled, reminder, publish, epoch, changed_at_ms FROM adapter_policy WHERE adapter = ?`, adapter).
		Scan(&policy.Enabled, &policy.Reminder, &publish, &policy.Epoch, &policy.ChangedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	policy.Publish = switchWord(publish)
	return policy, err
}
