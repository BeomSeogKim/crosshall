package domain

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Wire verbs of the home side of a link (#400). The owner seat runs them
// for `crosshall link enroll` and `crosshall link serve`, which a spoke
// starts over the user's own ssh; like every owner verb they go through
// the owner spool and carry the "owner " prefix.
const (
	VerbLinkEnroll = "owner link enroll"
	VerbLinkHall   = "owner link hall"
	VerbLinkProve  = "owner link prove"
	VerbLinkVerify = "owner link verify"
)

// LinkEnroll records a device that links to this hall, or gives a device
// of that name a new secret.
type LinkEnroll struct {
	Name string `json:"name"`
	// Secret is the device's link secret in hex, at least 32 bytes.
	Secret string `json:"secret"`
}

func (LinkEnroll) command() {}

// Verb returns the wire verb.
func (LinkEnroll) Verb() string { return VerbLinkEnroll }

// LinkHall asks for the hall's identity.
type LinkHall struct{}

func (LinkHall) command() {}

// Verb returns the wire verb.
func (LinkHall) Verb() string { return VerbLinkHall }

// LinkProve asks for the home's proof for one handshake with a device,
// made with the secret this hall keeps.
type LinkProve struct {
	Device     string `json:"device"`
	HomeNonce  string `json:"home_nonce"`
	SpokeNonce string `json:"spoke_nonce"`
}

func (LinkProve) command() {}

// Verb returns the wire verb.
func (LinkProve) Verb() string { return VerbLinkProve }

// LinkVerify checks a device's proof for one handshake against the secret
// this hall keeps, so the secret never leaves the dispatcher.
type LinkVerify struct {
	Device     string `json:"device"`
	HomeNonce  string `json:"home_nonce"`
	SpokeNonce string `json:"spoke_nonce"`
	Proof      string `json:"proof"`
}

func (LinkVerify) command() {}

// Verb returns the wire verb.
func (LinkVerify) Verb() string { return VerbLinkVerify }

// LinkEnrollResult names the hall and the device's ID.
type LinkEnrollResult struct {
	Hall   string `json:"hall"`
	Device string `json:"device"`
	// Rotated is true when a device of that name existed: it keeps its ID
	// and its old secret no longer proves anything.
	Rotated bool `json:"rotated"`
}

// LinkHallResult names the hall.
type LinkHallResult struct {
	Hall string `json:"hall"`
}

// LinkProveResult carries the home's proof, or Known false for a device
// this hall has no record of.
type LinkProveResult struct {
	Known bool   `json:"known"`
	Proof string `json:"proof,omitempty"`
}

// LinkVerifyResult says whether the device is known and the proof holds,
// and under which credential generation: the link carries it on every
// call and is refused once the device enrolls again.
type LinkVerifyResult struct {
	Known         bool  `json:"known"`
	OK            bool  `json:"ok"`
	CredentialGen int64 `json:"credential_gen,omitempty"`
}

const minLinkSecret = 32

func linkOwnerOnly() core.Result {
	return Refuse(core.CodeOwnerOnly, "only the owner links devices to the hall",
		"run crosshall link from your own shell")
}

func hallID(ctx context.Context, tx store.Tx) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT hall_id FROM hall WHERE singleton = 1`).Scan(&id)
	return id, err
}

func (s *Service) linkEnroll(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd LinkEnroll, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return linkOwnerOnly(), nil
	}
	if !ValidName(cmd.Name) {
		return Fail(core.ExitUsage, "", fmt.Sprintf("invalid device name %q", cmd.Name),
			"a device name is 1-32 of a-z, 0-9 and -"), nil
	}
	secret, err := hex.DecodeString(cmd.Secret)
	if err != nil || len(secret) < minLinkSecret {
		return Fail(core.ExitUsage, "", fmt.Sprintf("a link secret is at least %d bytes in hex", minLinkSecret),
			"run crosshall link <ssh-target> on the device, which makes the secret"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "device:" + cmd.Name
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	hall, err := hallID(ctx, tx)
	if err != nil {
		return core.Result{}, err
	}
	var device string
	rotated := true
	// A new secret starts a new credential generation: links authenticated
	// under the old one are refused at their next call.
	err = tx.QueryRowContext(ctx, `UPDATE device SET secret = ?, secret_at_ms = ?, credential_gen = credential_gen + 1
		WHERE name = ? RETURNING device_id`,
		secret, ms(now), cmd.Name).Scan(&device)
	if errors.Is(err, sql.ErrNoRows) {
		rotated = false
		err = tx.QueryRowContext(ctx, `INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms)
			VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?) RETURNING device_id`,
			cmd.Name, secret, ms(now), ms(now)).Scan(&device)
	}
	if err != nil {
		return core.Result{}, err
	}
	// The audit names the device, never the secret.
	detail := map[string]any{"device": device, "name": cmd.Name, "rotated": rotated}
	if err := audit(ctx, tx, "device_linked", scope, owner, detail, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(LinkEnrollResult{Hall: hall, Device: device, Rotated: rotated})
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func (s *Service) linkHall(ctx context.Context, tx store.Tx, caller core.Caller) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return linkOwnerOnly(), nil
	}
	hall, err := hallID(ctx, tx)
	if err != nil {
		return core.Result{}, err
	}
	return OK(LinkHallResult{Hall: hall})
}

// deviceKey reads a device's secret and this hall's identity; known is
// false for a device with no record or no secret.
func deviceKey(ctx context.Context, tx store.Tx, device string) (hall string, secret []byte, gen int64, known bool, err error) {
	hall, err = hallID(ctx, tx)
	if err != nil {
		return "", nil, 0, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT secret, credential_gen FROM device WHERE device_id = ?`, device).Scan(&secret, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return hall, nil, 0, false, nil
	}
	return hall, secret, gen, err == nil && len(secret) > 0, err
}

func (s *Service) linkProve(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkProve) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return linkOwnerOnly(), nil
	}
	if !link.ValidNonce(cmd.HomeNonce) || !link.ValidNonce(cmd.SpokeNonce) {
		return Fail(core.ExitUsage, "", "a link proof needs two fresh nonces", "run crosshall link serve, which makes them"), nil
	}
	hall, secret, _, known, err := deviceKey(ctx, tx, cmd.Device)
	if err != nil {
		return core.Result{}, err
	}
	if !known {
		return OK(LinkProveResult{})
	}
	return OK(LinkProveResult{Known: true,
		Proof: link.Proof(secret, link.RoleHome, hall, cmd.Device, cmd.HomeNonce, cmd.SpokeNonce)})
}

func (s *Service) linkVerify(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkVerify) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return linkOwnerOnly(), nil
	}
	hall, secret, gen, known, err := deviceKey(ctx, tx, cmd.Device)
	if err != nil {
		return core.Result{}, err
	}
	ok := known && link.CheckProof(secret, link.RoleSpoke, hall, cmd.Device, cmd.HomeNonce, cmd.SpokeNonce, cmd.Proof)
	r := LinkVerifyResult{Known: known, OK: ok}
	if ok {
		r.CredentialGen = gen
	}
	return OK(r)
}
