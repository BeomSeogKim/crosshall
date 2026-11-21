package domain

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbChannelAuto = "owner channel auto"

// ChannelAuto is the owner's switch for automatic joining. On, a session
// that starts in a Git repository with no channel gets the repository's own
// channel, created at that start. Off, only repositories with a default join.
type ChannelAuto struct {
	On bool `json:"on"`
	// Initial sets the switch only if the owner never set it: installing an
	// agent's hooks turns it on once and never overrides a later choice.
	Initial bool `json:"initial,omitempty"`
}

func (ChannelAuto) command()     {}
func (ChannelAuto) Verb() string { return VerbChannelAuto }

type ChannelAutoResult struct {
	On      bool `json:"on"`
	Changed bool `json:"changed"`
}

func (s *Service) channelAuto(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd ChannelAuto, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can switch automatic joining", "run it from the owner's own shell, not an agent session"), nil
	}
	if req.OpKey == "" {
		return Fail(core.ExitUsage, "", "an operation key is required for this command", "run it through the crosshall command, which supplies the key"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	const scope = "channel:auto"
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return r, err
	}
	was, err := autoJoinOn(ctx, tx)
	if err != nil {
		return core.Result{}, err
	}
	out := ChannelAutoResult{On: cmd.On, Changed: was != cmd.On}
	if cmd.Initial {
		var set bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hall_auto_join)`).Scan(&set); err != nil {
			return core.Result{}, err
		}
		if set {
			out = ChannelAutoResult{On: was}
		}
	}
	if out.Changed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hall_auto_join (id, enabled, changed_at_ms) VALUES (1, ?1, ?2)
			ON CONFLICT(id) DO UPDATE SET enabled = excluded.enabled, changed_at_ms = excluded.changed_at_ms`,
			cmd.On, ms(now)); err != nil {
			return core.Result{}, err
		}
		if err := audit(ctx, tx, "channel_auto", scope, owner, cmd, now); err != nil {
			return core.Result{}, err
		}
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

// AutoJoinOn reports the owner's switch; no row means off.
func AutoJoinOn(ctx context.Context, tx store.Tx) (bool, error) { return autoJoinOn(ctx, tx) }

func autoJoinOn(ctx context.Context, tx store.Tx) (bool, error) {
	var on bool
	err := tx.QueryRowContext(ctx, `SELECT enabled FROM hall_auto_join WHERE id = 1`).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return on, err
}

// createRepoChannel makes the repository's own channel and binds it to the
// repository root, for the session that is about to join it.
func createRepoChannel(ctx context.Context, tx store.Tx, repoRoot string, actor int64, now time.Time) (channelRow, error) {
	name, err := freeChannelName(ctx, tx, repoChannelName(repoRoot))
	if err != nil {
		return channelRow{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO channel (name, topic, state, created_at_ms, join_pool) VALUES (?, '', 'active', ?, ?)`,
		name, ms(now), DefaultJoinPool)
	if err != nil {
		return channelRow{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return channelRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repo_default(repo_root, channel_id, automatic) VALUES (?, ?, 1)`, repoRoot, id); err != nil {
		return channelRow{}, err
	}
	detail := map[string]string{"by": "hook", "repo_root": repoRoot}
	return channelRow{id: id, name: name, state: "active"}, audit(ctx, tx, "channel_created", "channel:"+name, actor, detail, now)
}

// repoChannelName turns a repository directory's name into a channel name.
func repoChannelName(repoRoot string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(repoRoot)), ".git")
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 32 {
		name = name[:32]
	}
	if name = strings.TrimRight(name, "-"); name == "" {
		return "repo"
	}
	return name
}

// freeChannelName returns name, or name with the first free numeric suffix.
func freeChannelName(ctx context.Context, tx store.Tx, name string) (string, error) {
	for n := 1; ; n++ {
		candidate := name
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			candidate = strings.TrimRight(name[:min(len(name), 32-len(suffix))], "-") + suffix
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel WHERE name = ?`, candidate).Scan(&taken); err != nil {
			return "", err
		}
		if taken == 0 {
			return candidate, nil
		}
	}
}
