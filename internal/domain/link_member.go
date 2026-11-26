package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// VerbLinkCall carries one member command from a linked device (#400).
const VerbLinkCall = "owner link call"

// LinkCall is a member command that a linked device's dispatcher
// authenticated against its own leaf and forwarded over the link. Device,
// CredentialGen and Admission come from the link's authenticated session,
// never from the device's frame.
type LinkCall struct {
	Device        string `json:"device"`
	CredentialGen int64  `json:"credential_gen"`
	// Admission is the device's admission generation the link that
	// carried this request was accepted under.
	Admission int64  `json:"admission"`
	Vendor    string `json:"vendor"`
	Key       string `json:"key"`
	// LeafGeneration and LeafEpoch name the device's leaf registration
	// the command was authenticated against.
	LeafGeneration string           `json:"leaf_generation"`
	LeafEpoch      int64            `json:"leaf_epoch"`
	Command        string           `json:"command"`
	Args           json.RawMessage  `json:"args,omitempty"`
	OpKey          string           `json:"op_key,omitempty"`
	Membership     *core.Membership `json:"membership,omitempty"`
	Ack            uint64           `json:"ack,omitempty"`
	WakePickup     *link.HookToken  `json:"wake_pickup,omitempty"`
}

func (LinkCall) command() {}

// Verb returns the wire verb.
func (LinkCall) Verb() string { return VerbLinkCall }

// Refusal codes of forwarded commands.
const (
	CodeLinkStale    core.Code = "link_stale"
	CodeStaleLeaf    core.Code = "stale_leaf"
	CodeNotOnADevice core.Code = "not_on_a_linked_device_yet"
	// CodeLinkResultTooLarge refuses a command whose answer would not fit
	// one link frame; the command is undone.
	CodeLinkResultTooLarge core.Code = "link_result_too_large"
)

func notOnADeviceYet(what string) core.Result {
	return Refuse(CodeNotOnADevice, what+" is not available on a linked device yet",
		"run it from a session on the hall's home device")
}

// linkVerbs are the member commands a linked device may forward in v1. A
// wait is forwarded poll by poll: each is one read-only snapshot, which
// also keeps the home's record that the member waits (#400, 12l).
var linkVerbs = map[string]bool{
	VerbJoin: true, VerbWho: true, VerbInbox: true, VerbRead: true, VerbAsk: true, VerbReply: true,
	VerbNote: true, VerbLog: true, VerbLeave: true, VerbFocus: true, VerbWait: true,
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
	// Reenrolled is set for the first call of a known key since its
	// device enrolled again: the device may have started a fresh store
	// (its home was restored to an older one, #400) and lost the stamp,
	// so a call without one is admitted against the home's selection.
	Reenrolled bool
}

// ResolveLinkMember authorizes a forwarded command in the writer
// transaction, in order: the link's credential generation, then the
// device's key and leaf registration. A refusal commits nothing.
func ResolveLinkMember(ctx context.Context, tx store.Tx, call LinkCall, now time.Time) (LinkMember, *core.Result, error) {
	if refusal, err := linkCredential(ctx, tx, call); err != nil || refusal != nil {
		return LinkMember{}, refusal, err
	}
	if call.Vendor == "" || call.Key == "" || call.LeafGeneration == "" || call.LeafEpoch <= 0 {
		r := Fail(core.ExitUsage, "", "a forwarded command names its session and leaf",
			"update crosshall on the linked device")
		return LinkMember{}, &r, nil
	}
	var principal, epoch, keyGen int64
	var leafGen, state, kind string
	var deviceOf sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT k.principal_id, k.leaf_generation, k.leaf_epoch, k.credential_gen, k.state,
		p.kind, p.device_id
		FROM device_key k JOIN principal p ON p.id = k.principal_id
		WHERE k.device_id = ? AND k.vendor = ? AND k.key = ?`, call.Device, call.Vendor, call.Key).
		Scan(&principal, &leafGen, &epoch, &keyGen, &state, &kind, &deviceOf)
	if errors.Is(err, sql.ErrNoRows) {
		if r, found, err := replayLinkFence(ctx, tx, call); err != nil || found {
			return LinkMember{}, &r, err
		}
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
	reenrolled := false
	switch {
	case keyGen < call.CredentialGen:
		// The device enrolled again: its leaf counter starts a new scope,
		// and requests of the old credential died before this lookup.
		reenrolled = true
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
	// As the native resolver: the selection is kept for admission and
	// replay, and the caller is a member only while that binding is live.
	caller := core.Caller{Role: core.RoleUnenrolled, PrincipalID: &principal}
	var selected core.Membership
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT s.binding_id, s.generation, b.state = 'live'
		FROM member_selection s JOIN binding b ON b.id = s.binding_id AND b.principal_id = s.principal_id
		WHERE s.principal_id = ?`, principal).Scan(&selected.BindingID, &selected.Generation, &live)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LinkMember{}, nil, err
	}
	if err == nil {
		caller.Membership = &selected
	}
	if live {
		caller.Role = core.RoleMember
		caller.BindingID = &selected.BindingID
	}
	return LinkMember{Caller: caller, Reenrolled: reenrolled}, nil, nil
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

// linkRequest identifies a forwarded request exactly as it arrived: its
// verb and raw arguments, and the membership it was admitted under. An
// exact retry resends the same bytes, so no handler's canonical form is
// needed to recognize it.
type linkRequest struct {
	Hash       string           `json:"hash"`
	Membership *core.Membership `json:"membership"`
}

func newLinkRequest(call LinkCall) (string, error) {
	sum := sha256.Sum256(append([]byte(call.Command+"\x00"), call.Args...))
	raw, err := json.Marshal(linkRequest{Hash: hex.EncodeToString(sum[:]), Membership: call.Membership})
	return string(raw), err
}

// ReplayLinkReceipt returns the committed receipt of a forwarded command
// for this member and key, before any admission check: a retry after a
// lost response gets the original receipt even if the member has since
// moved, and is never run again. A read's receipt is its snapshot,
// shown again against the channel it was read in, with no new pickup. A
// key used for another request or membership is a conflict.
func ReplayLinkReceipt(ctx context.Context, tx store.Tx, principal int64, call LinkCall) (core.Result, bool, error) {
	if call.OpKey == "" {
		return core.Result{}, false, nil
	}
	want, err := newLinkRequest(call)
	if err != nil {
		return core.Result{}, false, err
	}
	var scope, stored string
	var got sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT scope, result, link_request FROM op WHERE principal_id = ? AND op_key = ?`,
		principal, call.OpKey).Scan(&scope, &stored, &got)
	if errors.Is(err, sql.ErrNoRows) {
		// A fence written while the session was still unknown holds
		// after it joined under another key.
		return replayLinkFence(ctx, tx, call)
	}
	if err != nil {
		return core.Result{}, false, err
	}
	if !got.Valid || got.String != want {
		return Refuse(core.CodeKeyConflict,
			"this operation key was already used for a different command",
			"retry the original command, or drop --key to send a new one"), true, nil
	}
	var r core.Result
	if err := json.Unmarshal([]byte(stored), &r); err != nil {
		return core.Result{}, false, fmt.Errorf("domain: stored result for key: %w", err)
	}
	if call.Command != VerbRead || r.Status != core.ExitOK {
		return r, true, nil
	}
	var snapshot readSnapshot
	if err := json.Unmarshal(r.Data, &snapshot); err != nil {
		return core.Result{}, false, err
	}
	member := memberView{principal: principal, name: strings.TrimPrefix(scope, "channel:")}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM channel WHERE name = ?`, member.name).Scan(&member.channel); err != nil {
		return core.Result{}, false, err
	}
	out, err := hydrateRead(ctx, tx, member, snapshot)
	if err != nil {
		return core.Result{}, false, err
	}
	r, err = OK(out)
	return r, true, err
}

// RecordLinkRequest notes the forwarded request on its op record, for
// ReplayLinkReceipt.
func RecordLinkRequest(ctx context.Context, tx store.Tx, principal int64, call LinkCall) error {
	if call.OpKey == "" {
		return nil
	}
	raw, err := newLinkRequest(call)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE op SET link_request = ? WHERE principal_id = ? AND op_key = ?`, raw, principal, call.OpKey)
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
