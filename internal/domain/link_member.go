package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// VerbLinkCall carries one member command from a linked device (#400).
const VerbLinkCall = "owner link call"

// LinkCall is a member command that a linked device's dispatcher
// authenticated against its own leaf and forwarded over the link. Device
// and CredentialGen come from the link's authenticated session, never
// from the device's frame.
type LinkCall struct {
	Device        string `json:"device"`
	CredentialGen int64  `json:"credential_gen"`
	Vendor        string `json:"vendor"`
	Key           string `json:"key"`
	// LeafGeneration and LeafEpoch name the device's leaf registration
	// the command was authenticated against.
	LeafGeneration string           `json:"leaf_generation"`
	LeafEpoch      int64            `json:"leaf_epoch"`
	Command        string           `json:"command"`
	Args           json.RawMessage  `json:"args,omitempty"`
	OpKey          string           `json:"op_key,omitempty"`
	Membership     *core.Membership `json:"membership,omitempty"`
	Ack            uint64           `json:"ack,omitempty"`
}

func (LinkCall) command() {}

// Verb returns the wire verb.
func (LinkCall) Verb() string { return VerbLinkCall }

// Refusal codes of forwarded commands.
const (
	CodeLinkStale    core.Code = "link_stale"
	CodeStaleLeaf    core.Code = "stale_leaf"
	CodeNotOnADevice core.Code = "not_on_a_linked_device_yet"
)

func notOnADeviceYet(what string) core.Result {
	return Refuse(CodeNotOnADevice, what+" is not available on a linked device yet",
		"run it from a session on the hall's home device")
}

// linkVerbs are the member commands a linked device may forward in v1.
var linkVerbs = map[string]bool{
	VerbJoin: true, VerbWho: true, VerbInbox: true, VerbRead: true, VerbAsk: true, VerbReply: true,
	VerbNote: true, VerbLog: true, VerbLeave: true, VerbFocus: true,
}

// LinkCommand decodes a forwarded command in its link shape: only the
// allowed verbs, unknown fields refused, and nothing that would make the
// home act on the device's processes, paths or other sessions.
func LinkCommand(verb string, args json.RawMessage) (Command, *core.Result) {
	if !linkVerbs[verb] {
		r := notOnADeviceYet(fmt.Sprintf("%q", verb))
		return nil, &r
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	cmd, err := Decode(verb, args)
	if err != nil {
		r := Fail(core.ExitUsage, "", "invalid command arguments", "crosshall help")
		return nil, &r
	}
	switch c := cmd.(type) {
	case Join:
		if c.Channel == "" {
			r := Fail(core.ExitUsage, "", "a join from a linked device names its channel", "crosshall join <channel>")
			return nil, &r
		}
		switch {
		case c.Launch != nil:
			r := notOnADeviceYet("binding a join to an agent process")
			return nil, &r
		case c.Takeover:
			r := notOnADeviceYet("join --takeover")
			return nil, &r
		case c.RepoRoot != "":
			r := notOnADeviceYet("a repository default channel")
			return nil, &r
		}
	case Who:
		if c.ExitTracking {
			r := notOnADeviceYet("exit tracking")
			return nil, &r
		}
	}
	return cmd, nil
}

// LinkMember is the member a forwarded command runs as.
type LinkMember struct {
	Caller core.Caller
	// Enroll is set for a first join of an unknown key: it records the
	// device key in the join's transaction.
	Enroll func(context.Context, store.Tx, int64) error
}

// ResolveLinkMember authorizes a forwarded command in the writer
// transaction, in order: the link's credential generation, then the
// device's key and leaf registration. A refusal commits nothing.
func ResolveLinkMember(ctx context.Context, tx store.Tx, call LinkCall, now time.Time) (LinkMember, *core.Result, error) {
	var gen int64
	err := tx.QueryRowContext(ctx, `SELECT credential_gen FROM device WHERE device_id = ?`, call.Device).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) || err == nil && gen != call.CredentialGen {
		r := Refuse(CodeLinkStale, "this link was authenticated under a device credential that no longer holds",
			"reconnect the link")
		return LinkMember{}, &r, nil
	}
	if err != nil {
		return LinkMember{}, nil, err
	}
	if call.Vendor == "" || call.Key == "" || call.LeafGeneration == "" || call.LeafEpoch <= 0 {
		r := Fail(core.ExitUsage, "", "a forwarded command names its session and leaf",
			"update crosshall on the linked device")
		return LinkMember{}, &r, nil
	}
	var principal, epoch, keyGen int64
	var leafGen, state, kind string
	var deviceOf sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT k.principal_id, k.leaf_generation, k.leaf_epoch, k.credential_gen, k.state,
		p.kind, p.device_id
		FROM device_key k JOIN principal p ON p.id = k.principal_id
		WHERE k.device_id = ? AND k.vendor = ? AND k.key = ?`, call.Device, call.Vendor, call.Key).
		Scan(&principal, &leafGen, &epoch, &keyGen, &state, &kind, &deviceOf)
	if errors.Is(err, sql.ErrNoRows) {
		if call.Command != VerbJoin {
			r := Refuse(core.CodeNotEnrolled, "this session has not joined the hall", "crosshall join <channel>")
			return LinkMember{}, &r, nil
		}
		return LinkMember{Caller: core.Caller{Role: core.RoleUnenrolled}, Enroll: enrollDeviceKey(call, now)}, nil, nil
	}
	if err != nil {
		return LinkMember{}, nil, err
	}
	if state != "live" || kind != "session" || deviceOf.String != call.Device {
		r := Refuse(core.CodeNotEnrolled, "this session's membership is no longer live", "crosshall join <channel>")
		return LinkMember{}, &r, nil
	}
	switch {
	case keyGen < call.CredentialGen:
		// The device enrolled again: its leaf counter starts a new scope,
		// and requests of the old credential died before this lookup.
	case call.LeafEpoch == epoch && call.LeafGeneration == leafGen:
	case call.LeafEpoch > epoch:
		// A newer registration of the same session on the device.
	default:
		r := Refuse(CodeStaleLeaf, "this command came from an earlier registration of the session on its device",
			"run the command again from the current session")
		return LinkMember{}, &r, nil
	}
	if call.LeafEpoch != epoch || call.LeafGeneration != leafGen || keyGen != call.CredentialGen {
		if _, err := tx.ExecContext(ctx, `UPDATE device_key SET leaf_generation = ?, leaf_epoch = ?, credential_gen = ?
			WHERE device_id = ? AND vendor = ? AND key = ?`,
			call.LeafGeneration, call.LeafEpoch, call.CredentialGen, call.Device, call.Vendor, call.Key); err != nil {
			return LinkMember{}, nil, err
		}
	}
	membership, err := SelectedMembership(ctx, tx, principal)
	if err != nil {
		return LinkMember{}, nil, err
	}
	caller := core.Caller{Role: core.RoleMember, PrincipalID: &principal}
	if membership.BindingID != 0 {
		binding := membership.BindingID
		caller.BindingID, caller.Membership = &binding, &membership
	}
	return LinkMember{Caller: caller}, nil, nil
}

func enrollDeviceKey(call LinkCall, now time.Time) func(context.Context, store.Tx, int64) error {
	return func(ctx context.Context, tx store.Tx, principal int64) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO device_key
			(device_id, vendor, key, principal_id, leaf_generation, leaf_epoch, credential_gen, state, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'live', ?)`,
			call.Device, call.Vendor, call.Key, principal, call.LeafGeneration, call.LeafEpoch, call.CredentialGen, ms(now)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE principal SET device_id = ? WHERE id = ?`, call.Device, principal)
		return err
	}
}

// ReplayLinkReceipt returns the committed receipt of a forwarded command
// for this member and key, before any admission check: a retry after a
// lost response gets the original receipt even if the member has since
// moved, and is never run again. A key used for another command or
// another membership is a conflict.
func ReplayLinkReceipt(ctx context.Context, tx store.Tx, principal int64, key string, cmd Command, membership *core.Membership) (core.Result, bool, error) {
	if key == "" {
		return core.Result{}, false, nil
	}
	hash, err := payloadHash(cmd)
	if err != nil {
		return core.Result{}, false, err
	}
	var verb, gotHash, stored string
	var gotMembership sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT verb, payload_hash, result, membership FROM op WHERE principal_id = ? AND op_key = ?`,
		principal, key).Scan(&verb, &gotHash, &stored, &gotMembership)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, false, nil
	}
	if err != nil {
		return core.Result{}, false, err
	}
	want, err := json.Marshal(membership)
	if err != nil {
		return core.Result{}, false, err
	}
	if verb != cmd.Verb() || gotHash != hash || !gotMembership.Valid || gotMembership.String != string(want) {
		return Refuse(core.CodeKeyConflict,
			"this operation key was already used for a different command",
			"retry the original command, or drop --key to send a new one"), true, nil
	}
	var r core.Result
	if err := json.Unmarshal([]byte(stored), &r); err != nil {
		return core.Result{}, false, fmt.Errorf("domain: stored result for key: %w", err)
	}
	return r, true, nil
}

// RecordLinkMembership notes the membership a forwarded command was
// admitted under on its op record, for ReplayLinkReceipt.
func RecordLinkMembership(ctx context.Context, tx store.Tx, principal int64, key string, membership *core.Membership) error {
	if key == "" {
		return nil
	}
	raw, err := json.Marshal(membership)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE op SET membership = ? WHERE principal_id = ? AND op_key = ?`, string(raw), principal, key)
	return err
}

// LinkMemberPrincipal returns the principal a device key maps to, or 0.
func LinkMemberPrincipal(ctx context.Context, tx store.Tx, call LinkCall) (int64, error) {
	var principal int64
	err := tx.QueryRowContext(ctx, `SELECT principal_id FROM device_key WHERE device_id = ? AND vendor = ? AND key = ?`,
		call.Device, call.Vendor, call.Key).Scan(&principal)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return principal, err
}

// SelectedMembership returns a principal's selected channel binding and
// its generation, or a zero membership.
func SelectedMembership(ctx context.Context, tx store.Tx, principal int64) (core.Membership, error) {
	var m core.Membership
	err := tx.QueryRowContext(ctx, `SELECT binding_id, generation FROM member_selection WHERE principal_id = ?`, principal).
		Scan(&m.BindingID, &m.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return m, err
}

// onADevice reports whether any of the principals works on a linked
// device. Restore, takeover and session moves fence a key and a leaf in
// one transaction, and a device member's leaf is on its device, so the
// home refuses them for device members in v1 (#400).
func onADevice(ctx context.Context, tx store.Tx, principals ...int64) (bool, error) {
	for _, p := range principals {
		if p == 0 {
			continue
		}
		var device sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT device_id FROM principal WHERE id = ?`, p).Scan(&device)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil || device.Valid {
			return device.Valid, err
		}
	}
	return false, nil
}
