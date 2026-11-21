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
	Publish string `json:"publish,omitempty"`
	// TurnEnd is the owner's turn-end switch for the adapter: "on", "off",
	// or empty when the build's evidence decides.
	TurnEnd     string `json:"turn_end,omitempty"`
	Epoch       uint64 `json:"epoch"`
	ChangedAtMS int64  `json:"changed_at_ms"`
}

// ReadAdapterPolicy reads policy in the caller's transaction. A missing row
// represents a disabled adapter at epoch zero.
func ReadAdapterPolicy(ctx context.Context, tx Tx, adapter string) (AdapterPolicy, error) {
	policy := AdapterPolicy{Adapter: adapter}
	var publish, turnEnd sql.NullBool
	err := tx.QueryRowContext(ctx,
		`SELECT enabled, reminder, publish, turn_end, epoch, changed_at_ms FROM adapter_policy WHERE adapter = ?`, adapter).
		Scan(&policy.Enabled, &policy.Reminder, &publish, &turnEnd, &policy.Epoch, &policy.ChangedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	policy.Publish, policy.TurnEnd = switchWord(publish), switchWord(turnEnd)
	return policy, err
}

// TurnEndSwitchOn holds unless the owner turned the turn-end check off for
// the adapter the expression names.
func TurnEndSwitchOn(adapter string) string {
	return "COALESCE((SELECT tp.turn_end FROM adapter_policy tp WHERE tp.adapter = " + adapter + "), 1) = 1"
}
