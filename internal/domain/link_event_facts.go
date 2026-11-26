package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type linkHookInput struct {
	id      uint64
	member  link.Member
	token   link.HookToken
	kind    string
	payload []byte
	command Command
	written *link.Written
}

func parseLinkHookInput(kind string, payload []byte) (linkHookInput, error) {
	in := linkHookInput{kind: kind, payload: payload}
	if kind == link.TypeWritten {
		var written link.Written
		if err := json.Unmarshal(payload, &written); err != nil {
			return in, err
		}
		in.id, in.member, in.token, in.written = written.ID, written.Member, written.Token, &written
		if written.OfferID <= 0 || written.Nonce == "" || !validRef(written.Nonce) || written.WrittenAtMS <= 0 {
			return in, errors.New("invalid written offer")
		}
	} else {
		var fact link.Fact
		if err := json.Unmarshal(payload, &fact); err != nil {
			return in, err
		}
		in.id, in.member, in.token = fact.ID, fact.Member, fact.Token
		if fact.Kind != VerbHookEnroll && fact.Kind != VerbHookEvent {
			return in, errors.New("invalid linked hook verb")
		}
		cmd, err := Decode(fact.Kind, fact.Body)
		if err != nil {
			return in, err
		}
		in.command = cmd
		switch cmd := in.command.(type) {
		case HookEnroll:
			if !validLinkHookProfile(in.member, cmd.Profile, cmd.LaunchRef, cmd.EventRef, cmd.LaunchVerified) ||
				!ValidName(cmd.Source) || cmd.LaunchHosted && !cmd.LaunchVerified || cmd.PIDNamespace != "" || cmd.Host != nil ||
				cmd.VendorHome != "" || cmd.RepoRoot != "" || cmd.Repository != "" || cmd.CreateChannel || cmd.Epoch > math.MaxInt64 {
				return in, errors.New("invalid linked hook enrollment")
			}
		case HookEvent:
			if !validLinkHookProfile(in.member, cmd.Profile, cmd.LaunchRef, cmd.EventRef, cmd.LaunchVerified) ||
				!validRef(cmd.TurnRef) || cmd.PIDNamespace != "" || cmd.Host != nil || cmd.VendorHome != "" ||
				cmd.EnrollRepoRoot != "" || cmd.EnrollRepository != "" || cmd.EnrollCreate || cmd.Epoch > math.MaxInt64 {
				return in, errors.New("invalid linked hook event")
			}
			switch cmd.Fact {
			case FactPromptSubmitted, FactTurnFinishing, FactApprovalRequested, FactSessionEnded:
			case FactIdlePrompt:
				if cmd.TurnRef != "" || cmd.Receipt != "" {
					return in, errors.New("invalid linked idle fact")
				}
			default:
				return in, errors.New("unknown linked hook fact")
			}
		}
	}
	t := in.token
	if in.id == 0 || in.id > math.MaxInt64 || !ValidName(in.member.Vendor) || in.member.Key == "" ||
		len(in.member.Key) > 4096 || t.Boot == "" || !validRef(t.Boot) || t.LeafGeneration == "" ||
		!validRef(t.LeafGeneration) || t.LeafSeq == 0 || t.LeafSeq > math.MaxInt64 || t.MonoNS < 0 ||
		t.Membership.BindingID <= 0 || t.Membership.Generation <= 0 {
		return in, errors.New("invalid linked hook token")
	}
	return in, nil
}

func validLinkHookProfile(member link.Member, p wire.HookProfile, launch, event string, verified bool) bool {
	return p.Adapter == member.Vendor && validProfile(p) && validRef(launch) && launch != "" &&
		validRef(event) && event != "" && (!verified || launch != SessionLaunch)
}

func (s *Service) linkFact(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkFact, now time.Time) (core.Result, error) {
	payload, err := json.Marshal(cmd.Fact)
	if err != nil {
		return core.Result{}, err
	}
	return s.recordLinkHookInput(ctx, tx, caller, cmd.LinkEventPeer, link.TypeFact, payload, now)
}

func (s *Service) linkWritten(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkWritten, now time.Time) (core.Result, error) {
	payload, err := json.Marshal(cmd.Written)
	if err != nil {
		return core.Result{}, err
	}
	return s.recordLinkHookInput(ctx, tx, caller, cmd.LinkEventPeer, link.TypeWritten, payload, now)
}

func (s *Service) recordLinkHookInput(ctx context.Context, tx store.Tx, caller core.Caller, peer LinkEventPeer, kind string, payload []byte, now time.Time) (core.Result, error) {
	if refusal, err := authorizeLinkEventPeer(ctx, tx, caller, peer); refusal != nil || err != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return core.Result{}, err
	}
	in, invalid := parseLinkHookInput(kind, payload)
	if in.id == 0 || in.id > math.MaxInt64 {
		return Fail(core.ExitUsage, "", "invalid linked hook record ID", "check the linked device's outbox"), nil
	}
	stream, err := readLinkEventStream(ctx, tx, peer)
	if errors.Is(err, sql.ErrNoRows) {
		return refuseLinkEvents("hook records require the current subscribed link"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	result := link.Recorded{ID: in.id}
	var storedKind, storedPayload string
	err = tx.QueryRowContext(ctx, `SELECT kind, payload, status, code FROM link_event_input
		WHERE device_id = ? AND credential_gen = ? AND boot = ? AND id = ?`, peer.Device, peer.CredentialGen, in.token.Boot, in.id).
		Scan(&storedKind, &storedPayload, &result.Status, &result.Code)
	if err == nil {
		if storedKind != kind || storedPayload != string(payload) {
			return OK(link.Recorded{ID: in.id, Status: link.RecordedStale, Code: string(core.CodeKeyConflict)})
		}
		return OK(result)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, err
	}
	if invalid == nil && stream.boot != in.token.Boot {
		return OK(link.Recorded{ID: in.id, Status: link.RecordedStale, Code: "boot_scope"})
	}
	if invalid != nil {
		result.Status, result.Code = link.RecordedStale, "invalid_input"
		in.token.LeafSeq = 0
	} else {
		result.Status, result.Code, err = s.applyLinkHookInput(ctx, tx, peer, in, false, now)
		if err != nil {
			return core.Result{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO link_event_input
		(device_id, credential_gen, id, vendor, key, boot, leaf_generation, leaf_seq, kind, payload, status, code, recorded_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, peer.Device, peer.CredentialGen, in.id, in.member.Vendor,
		in.member.Key, in.token.Boot, in.token.LeafGeneration, in.token.LeafSeq, kind, string(payload), result.Status, result.Code, ms(now))
	if err != nil {
		return core.Result{}, err
	}
	return OK(result)
}

type linkHookScope struct {
	link.MemberScope
	principal int64
	binding   hookBinding
}

func currentLinkHookScope(ctx context.Context, tx store.Tx, peer LinkEventPeer, member link.Member) (linkHookScope, bool, error) {
	var scope linkHookScope
	err := tx.QueryRowContext(ctx, `SELECT k.principal_id, k.leaf_generation
		FROM device_key k JOIN principal p ON p.id = k.principal_id AND p.device_id = k.device_id
		WHERE k.device_id = ? AND k.credential_gen = ? AND k.vendor = ? AND k.key = ? AND k.state = 'live'
			AND p.kind = 'session' AND p.vendor = k.vendor`,
		peer.Device, peer.CredentialGen, member.Vendor, member.Key).Scan(&scope.principal, &scope.LeafGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return scope, false, nil
	}
	if err != nil {
		return scope, false, err
	}
	scope.binding, _, err = bindingOf(ctx, tx, scope.principal)
	scope.Membership = link.Membership{BindingID: scope.binding.id, Generation: scope.binding.selectionGen}
	scope.RouteEpoch = scope.binding.routeEpoch
	return scope, scope.binding.id != 0 && err == nil, err
}

func linkHookCut(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, scope linkHookScope, snapshot bool,
) (status, code string, err error) {
	var cut link.MemberScope
	var boot, baseline string
	var leafSeq, eventSeq uint64
	var credentialGen int64
	var absent, gap, snapshotPending bool
	err = tx.QueryRowContext(ctx, `SELECT c.boot, c.leaf_generation, c.binding_id, c.membership_generation, c.route_epoch,
		c.leaf_seq, c.absent, c.event_seq, s.boot, s.credential_gen, s.trimmed > s.acked AND c.event_seq < s.trimmed,
		EXISTS (SELECT 1 FROM link_event_stream r WHERE r.device_id = c.device_id AND r.stream = ? AND r.snapshot_seq IS NOT NULL)
		FROM link_event_cut c JOIN link_event_cursor s ON s.device_id = c.device_id
		WHERE c.device_id = ? AND c.vendor = ? AND c.key = ?`, peer.Stream, peer.Device, in.member.Vendor, in.member.Key).
		Scan(&boot, &cut.LeafGeneration, &cut.Membership.BindingID, &cut.Membership.Generation, &cut.RouteEpoch,
			&leafSeq, &absent, &eventSeq, &baseline, &credentialGen, &gap, &snapshotPending)
	if err == nil && baseline != "" && baseline != in.token.Boot && !snapshotPending {
		return link.RecordedStale, "boot_scope", nil
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && (boot != in.token.Boot || baseline != boot ||
		credentialGen != peer.CredentialGen || cut != scope.MemberScope || gap || snapshotPending && !snapshot) {
		return link.RecordedQuarantined, "awaiting_cut", nil
	}
	if err != nil {
		return "", "", err
	}
	if absent || in.token.LeafSeq <= leafSeq {
		return link.RecordedStale, "leaf_cut", nil
	}
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM device_event
		WHERE device_id = ? AND seq > ? AND
		(kind = 'membership' OR (kind = 'invalidate' AND json_extract(body, '$.reason') IN ('move', 'depart', 'route', 'policy')))
		AND json_extract(body, '$.vendor') = ? AND json_extract(body, '$.key') = ?)`,
		peer.Device, eventSeq, in.member.Vendor, in.member.Key).Scan(&pending)
	if pending {
		return link.RecordedQuarantined, "awaiting_cut", err
	}
	return "", "", err
}

func (s *Service) applyLinkHookInput(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, snapshot bool, now time.Time) (string, string, error) {
	scope, found, err := currentLinkHookScope(ctx, tx, peer, in.member)
	if err != nil {
		return "", "", err
	}
	if !found || scope.LeafGeneration != in.token.LeafGeneration || scope.Membership != in.token.Membership {
		return link.RecordedStale, "membership_scope", nil
	}
	if status, reason, err := linkHookCut(ctx, tx, peer, in, scope, snapshot); status != "" || err != nil {
		return status, reason, err
	}
	var same, quarantined bool
	var lastSeq uint64
	var lastAt int64
	err = tx.QueryRowContext(ctx, `SELECT credential_gen = ? AND boot = ? AND leaf_generation = ?,
		leaf_seq, effective_at_ms, quarantined
		FROM link_event_fact_head WHERE device_id = ? AND vendor = ? AND key = ?`,
		peer.CredentialGen, in.token.Boot, scope.LeafGeneration, peer.Device, in.member.Vendor, in.member.Key).
		Scan(&same, &lastSeq, &lastAt, &quarantined)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if same && quarantined {
		return link.RecordedQuarantined, "rewound_leaf", nil
	}
	if same && in.token.LeafSeq <= lastSeq {
		if snapshot {
			return link.RecordedStale, "rewound_leaf", nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE link_event_fact_head SET quarantined = 1
			WHERE device_id = ? AND vendor = ? AND key = ?`, peer.Device, in.member.Vendor, in.member.Key)
		return link.RecordedQuarantined, "rewound_leaf", err
	}
	// Accepted sequence supplies the order. The local projection uses home time
	// for expiry and a distinct position when two facts land in one millisecond.
	at := max(ms(now), lastAt+1)
	var ok bool
	if in.written != nil {
		ok, err = confirmLinkHookOffer(ctx, tx, peer, in, scope, now)
	} else {
		ok, err = applyLinkFact(ctx, tx, peer, in, scope, at, now)
	}
	if err != nil {
		return "", "", err
	}
	if !ok {
		return link.RecordedStale, "fact_scope", nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO link_event_fact_head
		(device_id, vendor, key, credential_gen, boot, leaf_generation, binding_id, membership_generation, route_epoch, leaf_seq, effective_at_ms, quarantined)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT (device_id, vendor, key) DO UPDATE SET credential_gen = excluded.credential_gen, boot = excluded.boot,
			leaf_generation = excluded.leaf_generation, binding_id = excluded.binding_id,
			membership_generation = excluded.membership_generation, route_epoch = excluded.route_epoch,
			leaf_seq = excluded.leaf_seq, effective_at_ms = excluded.effective_at_ms, quarantined = 0`,
		peer.Device, in.member.Vendor, in.member.Key, peer.CredentialGen, in.token.Boot, scope.LeafGeneration,
		scope.Membership.BindingID, scope.Membership.Generation, scope.RouteEpoch, in.token.LeafSeq, at)
	return link.RecordedApplied, "", err
}

func (s *Service) resolveLinkFactCuts(ctx context.Context, tx store.Tx, peer LinkEventPeer, ack link.Ack, snapshot bool, now time.Time) error {
	for _, cut := range ack.Cuts {
		if snapshot {
			if _, err := tx.ExecContext(ctx, `UPDATE link_event_fact_head SET quarantined = 0
				WHERE device_id = ? AND vendor = ? AND key = ? AND EXISTS (
					SELECT 1 FROM link_event_cut c WHERE c.device_id = link_event_fact_head.device_id
					AND c.vendor = link_event_fact_head.vendor AND c.key = link_event_fact_head.key AND c.boot = ?
					AND c.leaf_generation = ? AND c.binding_id = ? AND c.membership_generation = ? AND c.route_epoch = ?
					AND c.event_seq = ? AND c.leaf_seq >= link_event_fact_head.leaf_seq)`,
				peer.Device, cut.Vendor, cut.Key, ack.Boot, cut.Scope.LeafGeneration, cut.Scope.Membership.BindingID,
				cut.Scope.Membership.Generation, cut.Scope.RouteEpoch, cut.Seq); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind, payload, boot FROM link_event_input
			WHERE device_id = ? AND credential_gen = ? AND vendor = ? AND key = ? AND status = 'quarantined'
			ORDER BY leaf_seq, id`, peer.Device, peer.CredentialGen, cut.Vendor, cut.Key)
		if err != nil {
			return err
		}
		type pendingInput struct{ kind, payload, boot string }
		var pending []pendingInput
		for rows.Next() {
			var p pendingInput
			if err := rows.Scan(&p.kind, &p.payload, &p.boot); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, p)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, p := range pending {
			in, err := parseLinkHookInput(p.kind, []byte(p.payload))
			if err != nil {
				return err
			}
			status, code := link.RecordedStale, "boot_scope"
			if p.boot == ack.Boot {
				status, code, err = s.applyLinkHookInput(ctx, tx, peer, in, snapshot, now)
				if err != nil {
					return err
				}
			}
			if status != link.RecordedQuarantined {
				if _, err := tx.ExecContext(ctx, `UPDATE link_event_input SET status = ?, code = ?
					WHERE device_id = ? AND credential_gen = ? AND boot = ? AND id = ?`,
					status, code, peer.Device, peer.CredentialGen, p.boot, in.id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func applyLinkFact(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, scope linkHookScope, at int64, now time.Time) (bool, error) {
	switch cmd := in.command.(type) {
	case HookEnroll:
		if open, err := policyOpen(ctx, tx, in.member.Vendor, cmd.Epoch); err != nil || !open {
			return false, err
		}
		if scope.binding.state != "live" {
			if !scope.binding.byHook || scope.binding.byOwner || cmd.Source == "compact" {
				return false, nil
			}
			if _, err := rejoin(ctx, tx, scope.binding, scope.principal, in.member.Vendor, now); err != nil {
				return false, err
			}
		}
		_, err := startLinkHookLaunch(ctx, tx, peer, in, scope, cmd.Profile, cmd.LaunchRef, cmd.Epoch, cmd.LaunchVerified, cmd.LaunchHosted, at)
		return err == nil, err
	case HookEvent:
		if scope.binding.state != "live" {
			return false, nil
		}
		if open, err := policyOpen(ctx, tx, in.member.Vendor, cmd.Epoch); err != nil || !open {
			return false, err
		}
		return observeLinkHookEvent(ctx, tx, peer, in, scope, cmd, at, now)
	default:
		return false, errors.New("domain: unexpected linked hook command")
	}
}

func startLinkHookLaunch(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, scope linkHookScope,
	profile wire.HookProfile, launch string, epoch uint64, verified, hosted bool, at int64,
) (string, error) {
	projection := fmt.Sprintf("linked-%x", sha256.Sum256(in.payload))
	if err := registerLaunch(ctx, tx, scope.principal, profile, projection, false, "", at); err != nil {
		return "", err
	}
	_, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = ?
		WHERE principal_id = ? AND ended_hook_at_ms IS NULL AND launch_ref <> ?`, at, scope.principal, projection)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO link_event_launch
		(principal_id, device_id, credential_gen, boot, leaf_generation, binding_id, membership_generation,
			route_epoch, launch_ref, projection_ref, policy_epoch, verified, hosted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (principal_id) DO UPDATE SET device_id = excluded.device_id, credential_gen = excluded.credential_gen,
			boot = excluded.boot, leaf_generation = excluded.leaf_generation, binding_id = excluded.binding_id,
			membership_generation = excluded.membership_generation, route_epoch = excluded.route_epoch, launch_ref = excluded.launch_ref,
			projection_ref = excluded.projection_ref, policy_epoch = excluded.policy_epoch, verified = excluded.verified, hosted = excluded.hosted`,
		scope.principal, peer.Device, peer.CredentialGen, in.token.Boot, scope.LeafGeneration, scope.binding.id,
		scope.binding.selectionGen, scope.RouteEpoch, launch, projection, epoch, verified, hosted)
	if err != nil {
		return "", err
	}
	err = markLinkOfferDirty(ctx, tx, peer.Device, in.member, link.InvalidateRoute)
	return projection, err
}

func observeLinkHookEvent(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, scope linkHookScope,
	cmd HookEvent, at int64, now time.Time,
) (bool, error) {
	var projection, launch string
	var policy uint64
	var verified, live bool
	err := tx.QueryRowContext(ctx, `SELECT l.projection_ref, l.launch_ref, l.policy_epoch, l.verified,
			a.ended_hook_at_ms IS NULL AND a.fenced = 0
		FROM link_event_launch l JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
		WHERE l.principal_id = ? AND l.device_id = ? AND l.credential_gen = ? AND l.boot = ? AND l.leaf_generation = ?
			AND l.binding_id = ? AND l.membership_generation = ? AND l.route_epoch = ?`,
		scope.principal, peer.Device, peer.CredentialGen, in.token.Boot, scope.LeafGeneration, scope.binding.id,
		scope.binding.selectionGen, scope.RouteEpoch).Scan(&projection, &launch, &policy, &verified, &live)
	if errors.Is(err, sql.ErrNoRows) || err == nil && live && launch == cmd.LaunchRef && policy != cmd.Epoch {
		if cmd.Fact != FactPromptSubmitted {
			return false, nil
		}
		verified = cmd.LaunchVerified
		projection, err = startLinkHookLaunch(ctx, tx, peer, in, scope, cmd.Profile, cmd.LaunchRef, cmd.Epoch, verified, false, at)
	} else if err == nil && (!live || launch != cmd.LaunchRef) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	before, err := presenceOf(ctx, tx, scope.principal, now)
	if err != nil {
		return false, err
	}
	var fresh any
	if limit := factCapMS[cmd.Fact]; limit > 0 {
		fresh = ms(now) + limit
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO observation
		(principal_id, kind, origin, source, observed_at_ms, fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
		VALUES (?, ?, 'unknown', ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?) ON CONFLICT DO NOTHING`,
		scope.principal, cmd.Fact, evidence(cmd.Profile), at, fresh, projection, cmd.EventRef, cmd.TurnRef, at, profileJSON(cmd.Profile))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err == nil, err
	}
	switch cmd.Fact {
	case FactIdlePrompt, FactPromptSubmitted, FactTurnFinishing:
		if _, _, err := reconcileTurns(ctx, tx, scope.principal, projection, cmd.EventRef, at); err != nil {
			return false, err
		}
	case FactSessionEnded:
		if verified && cmd.LaunchVerified {
			if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = ?
				WHERE principal_id = ? AND launch_ref = ? AND ended_hook_at_ms IS NULL`, at, scope.principal, projection); err != nil {
				return false, err
			}
			return true, endSession(ctx, tx, scope.binding, scope.principal, now)
		}
	}
	return true, presenceChanged(ctx, tx, scope.binding.channel, scope.principal, before, now)
}

func confirmLinkHookOffer(ctx context.Context, tx store.Tx, peer LinkEventPeer, in linkHookInput, scope linkHookScope, now time.Time) (bool, error) {
	if scope.binding.state != "live" {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE offer SET written_at_ms = COALESCE(written_at_ms, ?1)
		WHERE id = ?2 AND nonce = ?3 AND principal_id = ?4 AND generation = ?5 AND binding_id = ?6 AND route_epoch = ?7
			AND device_credential_gen = ?8 AND device_membership_gen = ?9 AND receipt_mode = 'provisional'
			AND EXISTS (SELECT 1 FROM link_event_launch l JOIN adapter_launch a
					ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
				JOIN adapter_policy p ON p.adapter = ?10 AND p.enabled = 1 AND p.epoch = l.policy_epoch
				WHERE l.principal_id = ?4 AND l.device_id = ?11 AND l.credential_gen = ?8 AND l.boot = ?12
					AND l.leaf_generation = ?5 AND l.binding_id = ?6 AND l.route_epoch = ?7 AND l.membership_generation = ?9
					AND l.launch_ref = offer.launch_ref AND l.policy_epoch = offer.epoch AND a.ended_hook_at_ms IS NULL AND a.fenced = 0)`,
		ms(now), in.written.OfferID, in.written.Nonce, scope.principal, scope.LeafGeneration, scope.binding.id, scope.RouteEpoch,
		peer.CredentialGen, scope.binding.selectionGen, in.member.Vendor, peer.Device, in.token.Boot)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
