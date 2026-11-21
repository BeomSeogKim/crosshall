package domain

import (
	"context"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const VerbAdapterSet = wire.VerbAdapterSet

// AdapterSet changes one or both switches for an adapter.
type AdapterSet struct{ wire.AdapterPolicyArgs }

func (AdapterSet) command()     {}
func (AdapterSet) Verb() string { return VerbAdapterSet }

// ValidAdapterName reports whether this adapter has a policy switch: the
// build supports it with a hook manifest.
func ValidAdapterName(adapter string) bool { return manifests.Has(adapter) }

func (s *Service) adapterSet(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd AdapterSet, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can change adapter policy", "run it from the owner's own shell, not an agent session"), nil
	}
	if req.OpKey == "" {
		return Fail(core.ExitUsage, "", "an operation key is required for this command", "run it through the crosshall command, which supplies the key"), nil
	}
	if !ValidAdapterName(cmd.Adapter) || cmd.Enabled == nil && cmd.Reminder == nil && cmd.Publish == nil ||
		cmd.Publish != nil && *cmd.Publish != "on" && *cmd.Publish != "off" && *cmd.Publish != "default" {
		return Fail(core.ExitUsage, "", "choose a supported adapter and at least one valid policy switch", "crosshall owner adapter set claude --enabled on"), nil
	}
	if cmd.Adapter == "agy" && cmd.Publish != nil && *cmd.Publish == "on" {
		return Fail(core.ExitUsage, "", "Antigravity has no publish adapter", "crosshall setup status"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "adapter:" + cmd.Adapter
	if result, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return result, err
	}
	before, err := store.ReadAdapterPolicy(ctx, tx, cmd.Adapter)
	if err != nil {
		return core.Result{}, err
	}
	after := before
	if cmd.Enabled != nil {
		after.Enabled = *cmd.Enabled
	}
	if cmd.Reminder != nil {
		after.Reminder = *cmd.Reminder
	}
	if cmd.Publish != nil {
		after.Publish = strings.TrimSuffix(*cmd.Publish, "default")
	}
	if after != before {
		// The publish switch is read live at reservation and at the final
		// fence, so changing it needs no new epoch and keeps routes.
		if after.Enabled != before.Enabled || after.Reminder != before.Reminder {
			after.Epoch++
		}
		after.ChangedAtMS = ms(now)
		var publish any
		if after.Publish != "" {
			publish = after.Publish == "on"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO adapter_policy(adapter, enabled, reminder, publish, epoch, changed_at_ms)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(adapter) DO UPDATE SET
			enabled = excluded.enabled, reminder = excluded.reminder, publish = excluded.publish, epoch = excluded.epoch,
			changed_at_ms = excluded.changed_at_ms`, after.Adapter, after.Enabled, after.Reminder, publish, after.Epoch, after.ChangedAtMS)
		if err != nil {
			return core.Result{}, err
		}
		if err := refreshAdapterCapabilities(ctx, tx, after.Adapter, nil); err != nil {
			return core.Result{}, err
		}
		if after.Epoch != before.Epoch {
			if err := invalidatePublishRoutesForEpoch(ctx, tx, after.Adapter, now); err != nil {
				return core.Result{}, err
			}
		}
		if err := audit(ctx, tx, "adapter_policy_set", scope, owner, after, now); err != nil {
			return core.Result{}, err
		}
	}
	result, err := OK(after)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, owner, req.OpKey, scope, cmd, result, now)
}

func refreshMemberAdapterCapabilities(ctx context.Context, tx store.Tx, principal int64, adapter string) error {
	return refreshAdapterCapabilities(ctx, tx, adapter, &principal)
}

// Only the latest launch may contribute availability for a live principal.
// Its current profile determines the evidence level with or without policy.
// A nil principal refreshes every session of the specified adapter.
func refreshAdapterCapabilities(ctx context.Context, tx store.Tx, adapter string, principal *int64) error {
	_, err := tx.ExecContext(ctx, `WITH target_session AS (
		SELECT id FROM principal WHERE id = ?2 AND kind = 'session' AND vendor = ?1
		UNION ALL
		SELECT id FROM principal WHERE ?2 IS NULL AND kind = 'session' AND vendor = ?1
	), live_profile AS (
		SELECT l.principal_id, l.profile,
			json_extract(l.profile, '$.vendor_version') AS version,
			json_extract(l.profile, '$.platform') AS platform,
			json_extract(l.profile, '$.mode') AS mode,
			json_extract(l.profile, '$.manifest_rev') AS manifest_rev,
			l.started_hook_at_ms AS started_at_ms
		FROM target_session s JOIN adapter_launch l ON l.rowid = (
			SELECT latest.rowid FROM adapter_launch latest WHERE latest.principal_id = s.id
			ORDER BY latest.ended_hook_at_ms IS NOT NULL, latest.started_hook_at_ms DESC, latest.rowid DESC LIMIT 1
		)
		WHERE l.ended_hook_at_ms IS NULL AND json_extract(l.profile, '$.adapter') = ?1
			AND EXISTS (SELECT 1 FROM binding b WHERE b.principal_id = l.principal_id AND b.state = 'live')
	), capability_names(cap) AS (
		VALUES ('enroll'), ('observe'), ('boundary'), ('wake')
	), live_evidence AS (
		SELECT p.principal_id, n.cap, p.version, p.platform, p.mode,
			e.level AS exact_level, e.checked_at_ms AS exact_checked_at_ms, p.started_at_ms,
			`+store.CapabilityCovered("?1", "p.profile", "n.cap")+` AS covered,
			(SELECT MAX(shared.checked_at_ms) FROM capability_evidence shared WHERE shared.vendor = ?1
				AND shared.platform = p.platform AND shared.mode = p.mode AND shared.manifest_rev = p.manifest_rev
				AND shared.cap = n.cap AND shared.level = 'tested') AS shared_checked_at_ms
		FROM live_profile p CROSS JOIN capability_names n
		LEFT JOIN capability_evidence e ON e.vendor = ?1 AND e.vendor_version = p.version
			AND e.platform = p.platform AND e.mode = p.mode AND e.manifest_rev = p.manifest_rev AND e.cap = n.cap
	), projected AS (
		SELECT principal_id, cap, version, platform, mode, covered,
			CASE WHEN cap = 'wake' THEN 'unavailable' WHEN exact_level IS NOT NULL THEN exact_level
				WHEN covered THEN 'tested' ELSE 'candidate' END AS level,
			COALESCE(exact_checked_at_ms, CASE WHEN covered THEN shared_checked_at_ms END, started_at_ms) AS checked_at_ms
		FROM live_evidence
	)
	INSERT INTO capability (principal_id, cap, level, available, vendor_version, platform, mode, checked_at_ms)
	SELECT principal_id, cap, level, level <> 'unavailable' AND (level <> 'tested' OR covered) AND
		COALESCE((SELECT enabled FROM adapter_policy WHERE adapter = ?1), 0),
		version, platform, mode, checked_at_ms
	FROM projected
	UNION ALL
	SELECT c.principal_id, c.cap, CASE WHEN c.cap = 'wake' THEN 'unavailable' ELSE c.level END, 0,
		c.vendor_version, c.platform, c.mode, c.checked_at_ms
	FROM capability c JOIN target_session s ON s.id = c.principal_id
	WHERE NOT EXISTS (SELECT 1 FROM live_profile p WHERE p.principal_id = c.principal_id)
	ON CONFLICT (principal_id, cap) DO UPDATE SET level = excluded.level, available = excluded.available,
		vendor_version = excluded.vendor_version, platform = excluded.platform, mode = excluded.mode,
		checked_at_ms = excluded.checked_at_ms`, adapter, principal)
	return err
}
