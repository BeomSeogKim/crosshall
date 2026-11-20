package domain

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbPublish      = "publish"
	VerbOwnerPublish = "owner publish"
)

// Publish is a member's own publish switch. Off keeps the member on pull;
// on clears only the member's own off, never an owner's.
type Publish struct {
	On bool `json:"on"`
}

func (Publish) command()     {}
func (Publish) Verb() string { return VerbPublish }

// OwnerPublish sets an owner switch: for the hall, or for one member by
// binding ID. Switch is on, off or default (member only), or resume, which
// lifts a member's automatic suspension.
type OwnerPublish struct {
	Hall      bool   `json:"hall,omitempty"`
	BindingID int64  `json:"binding_id,omitempty"`
	Switch    string `json:"switch"`
}

func (OwnerPublish) command()     {}
func (OwnerPublish) Verb() string { return VerbOwnerPublish }

// PublishSwitchResult reports a switch change and, for a member, the
// publish state that follows from every switch.
type PublishSwitchResult struct {
	Hall      bool           `json:"hall,omitempty"`
	BindingID int64          `json:"binding_id,omitempty"`
	Channel   string         `json:"channel,omitempty"`
	Alias     string         `json:"alias,omitempty"`
	Switch    string         `json:"switch"`
	Changed   bool           `json:"changed"`
	Publish   *PublishStatus `json:"publish,omitempty"`
}

func (s *Service) publish(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Publish, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	scope := "channel:" + m.channelName
	if r, found, err := replay(ctx, tx, m.principal, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	out := PublishSwitchResult{BindingID: m.binding, Channel: m.channelName, Alias: m.alias, Switch: "off"}
	if cmd.On {
		out.Switch = "on"
	}
	res, err := tx.ExecContext(ctx, `UPDATE binding SET publish_quiet = ? WHERE id = ? AND publish_quiet <> ?`, !cmd.On, m.binding, !cmd.On)
	if err != nil {
		return core.Result{}, err
	}
	if out.Changed, err = changedOne(res); err != nil {
		return core.Result{}, err
	}
	if out.Changed {
		if err := audit(ctx, tx, "publish_switch", "binding:"+strconv.FormatInt(m.binding, 10), m.principal,
			map[string]string{"by": "member", "switch": out.Switch}, now); err != nil {
			return core.Result{}, err
		}
	}
	status, err := publishStatusOf(ctx, tx, m.binding, now)
	if err != nil {
		return core.Result{}, err
	}
	out.Publish = publishWord(status)
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	result, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, m.principal, req.OpKey, scope, cmd, result, now)
}

func (s *Service) ownerPublish(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd OwnerPublish, now time.Time) (core.Result, error) {
	const usage = "crosshall owner publish hall on|off, member <binding-id> on|off|default, or resume <binding-id>"
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can change publish switches", "run it from the owner's own shell, not an agent session"), nil
	}
	valid := cmd.Hall && cmd.BindingID == 0 && (cmd.Switch == "on" || cmd.Switch == "off") ||
		!cmd.Hall && cmd.BindingID > 0 && (cmd.Switch == "on" || cmd.Switch == "off" || cmd.Switch == "default" || cmd.Switch == "resume")
	if !valid || req.OpKey == "" {
		return Fail(core.ExitUsage, "", "choose the hall or a binding, and a switch", usage), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "publish:hall"
	if !cmd.Hall {
		scope = "binding:" + strconv.FormatInt(cmd.BindingID, 10)
	}
	if result, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return result, err
	}
	out := PublishSwitchResult{Hall: cmd.Hall, BindingID: cmd.BindingID, Switch: cmd.Switch}
	var res sql.Result
	switch {
	case cmd.Hall:
		res, err = tx.ExecContext(ctx, `INSERT INTO hall_publish (id, enabled, changed_at_ms) VALUES (1, ?1, ?2)
			ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, changed_at_ms = excluded.changed_at_ms
			WHERE hall_publish.enabled <> excluded.enabled`, cmd.Switch == "on", ms(now))
	default:
		err = tx.QueryRowContext(ctx, `SELECT c.name, b.alias FROM binding b JOIN channel c ON c.id = b.channel_id WHERE b.id = ?`,
			cmd.BindingID).Scan(&out.Channel, &out.Alias)
		if errors.Is(err, sql.ErrNoRows) {
			return Fail(core.ExitUsage, "", "no such binding", "crosshall owner who --all lists the bindings"), nil
		}
		if err != nil {
			return core.Result{}, err
		}
		switch cmd.Switch {
		case "resume":
			res, err = tx.ExecContext(ctx, `UPDATE binding SET publish_resumed_at_ms = ? WHERE id = ?`, ms(now), cmd.BindingID)
		case "default":
			res, err = tx.ExecContext(ctx, `UPDATE binding SET publish_owner = NULL WHERE id = ? AND publish_owner IS NOT NULL`, cmd.BindingID)
		default:
			res, err = tx.ExecContext(ctx, `UPDATE binding SET publish_owner = ?1 WHERE id = ?2 AND publish_owner IS NOT ?1`,
				cmd.Switch == "on", cmd.BindingID)
		}
	}
	if err != nil {
		return core.Result{}, err
	}
	if out.Changed, err = changedOne(res); err != nil {
		return core.Result{}, err
	}
	if out.Changed {
		if err := audit(ctx, tx, "publish_switch", scope, owner, map[string]string{"by": "owner", "switch": cmd.Switch}, now); err != nil {
			return core.Result{}, err
		}
	}
	if !cmd.Hall {
		if out.Publish, err = publishStatusOf(ctx, tx, cmd.BindingID, now); err != nil {
			return core.Result{}, err
		}
	}
	result, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, owner, req.OpKey, scope, cmd, result, now)
}

func publishStatusOf(ctx context.Context, tx store.Tx, binding int64, now time.Time) (*PublishStatus, error) {
	status, found, err := store.ReadPublishState(ctx, tx, binding, now)
	if !found || err != nil {
		return nil, err
	}
	return &status, nil
}

func changedOne(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	return n > 0, err
}
