package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// VerbLinkReceipt looks up what became of a forwarded command whose answer
// the device never received (#400).
const VerbLinkReceipt = "owner link receipt"

// CodeLinkFenced answers a receipt lookup that found no receipt. The home
// fenced the request in the same transaction, so it never runs later; it
// may have run before its receipt expired.
const CodeLinkFenced core.Code = "link_outcome_unknown"

// LinkReceipt names a forwarded request exactly as the device sent it:
// its session, operation key, command, raw arguments and membership. The
// leaf fields are not used: a lookup authorizes nothing.
type LinkReceipt struct {
	LinkCall
}

func (LinkReceipt) Verb() string { return VerbLinkReceipt }

func linkFenced() core.Result {
	return Refuse(CodeLinkFenced, "the hall has no receipt for this request and will not run it; it may have run before its receipt expired",
		"run the command again to send it as a new request")
}

// LinkSessionLimit bounds how long a link may carry requests after the
// home accepted it. A request is admitted only within it, however long a
// frame waited in a buffer, so a fence kept for store.RetentionWindow
// outlives every copy of the request it fences.
const LinkSessionLimit = 24 * time.Hour

// linkCredential refuses a link authenticated under a device credential
// that no longer holds, or accepted longer ago than LinkSessionLimit.
func linkCredential(ctx context.Context, tx store.Tx, call LinkCall, now time.Time) (*core.Result, error) {
	if call.LinkAtMS <= 0 || now.Sub(time.UnixMilli(call.LinkAtMS)) > LinkSessionLimit {
		r := Refuse(CodeLinkStale, "this link was accepted too long ago to carry requests", "reconnect the link")
		return &r, nil
	}
	var gen int64
	err := tx.QueryRowContext(ctx, `SELECT credential_gen FROM device WHERE device_id = ?`, call.Device).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) || err == nil && gen != call.CredentialGen {
		r := Refuse(CodeLinkStale, "this link was authenticated under a device credential that no longer holds",
			"reconnect the link")
		return &r, nil
	}
	return nil, err
}

// FindOrFenceLinkReceipt returns the committed receipt of a forwarded
// request, or fences its key so the request never runs, in one writer
// transaction. It touches no leaf epoch, selection or pickup, and never
// runs the command: recovery is receipt-only (#400, design 12h). The
// credential is the link's current one.
func FindOrFenceLinkReceipt(ctx context.Context, tx store.Tx, call LinkCall, now time.Time) (core.Result, error) {
	if refusal, err := linkCredential(ctx, tx, call, now); err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return core.Result{}, err
	}
	if call.Vendor == "" || call.Key == "" || call.OpKey == "" || call.Command == "" {
		return Fail(core.ExitUsage, "", "a receipt lookup names its session, operation key and command",
			"update crosshall on the linked device"), nil
	}
	request, err := newLinkRequest(call)
	if err != nil {
		return core.Result{}, err
	}
	principal, err := LinkMemberPrincipal(ctx, tx, call)
	if err != nil {
		return core.Result{}, err
	}
	if principal == 0 {
		if r, found, err := replayLinkFence(ctx, tx, call); err != nil || found {
			return r, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO link_fence (device_id, vendor, key, op_key, link_request, at_ms)
			VALUES (?, ?, ?, ?, ?, ?)`, call.Device, call.Vendor, call.Key, call.OpKey, request, ms(now))
		return linkFenced(), err
	}
	if r, found, err := ReplayLinkReceipt(ctx, tx, principal, call); err != nil || found {
		return r, err
	}
	r := linkFenced()
	stored, err := json.Marshal(r)
	if err != nil {
		return core.Result{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms, link_request)
		VALUES (?, ?, 'link', ?, '', ?, ?, ?)`, principal, call.OpKey, call.Command, string(stored), ms(now), request)
	return r, err
}

// replayLinkFence answers a request whose key a receipt lookup fenced
// before its session was known: the fenced outcome for the same request,
// a conflict for another.
func replayLinkFence(ctx context.Context, tx store.Tx, call LinkCall) (core.Result, bool, error) {
	if call.OpKey == "" {
		return core.Result{}, false, nil
	}
	var got string
	err := tx.QueryRowContext(ctx, `SELECT link_request FROM link_fence
		WHERE device_id = ? AND vendor = ? AND key = ? AND op_key = ?`, call.Device, call.Vendor, call.Key, call.OpKey).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, false, nil
	}
	if err != nil {
		return core.Result{}, false, err
	}
	want, err := newLinkRequest(call)
	if err != nil {
		return core.Result{}, false, err
	}
	if got != want {
		return Refuse(core.CodeKeyConflict,
			"this operation key was already used for a different command",
			"retry the original command, or drop --key to send a new one"), true, nil
	}
	return linkFenced(), true, nil
}
