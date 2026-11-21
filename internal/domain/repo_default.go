package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbChannelDefault = "owner channel default"

// ChannelDefault sets the channel used by a repository's channel-less joins.
type ChannelDefault struct {
	Channel  string `json:"channel"`
	RepoRoot string `json:"repo_root"`
}

func (ChannelDefault) command()     {}
func (ChannelDefault) Verb() string { return VerbChannelDefault }

type ChannelDefaultResult struct {
	Channel  string `json:"channel"`
	RepoRoot string `json:"repo_root"`
}

func validRepoRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root && ValidText("repository root", root) == ""
}

func (s *Service) channelDefault(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd ChannelDefault, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can set a repository default", "run it from the owner's own shell, not an agent session"), nil
	}
	if req.OpKey == "" {
		return Fail(core.ExitUsage, "", "an operation key is required for this command", "run it through the crosshall command, which supplies the key"), nil
	}
	if !ValidName(cmd.Channel) || !validRepoRoot(cmd.RepoRoot) {
		return Fail(core.ExitUsage, "", "a repository default needs a channel name and an absolute repository root", "crosshall owner channel default <channel> --repo <dir>"), nil
	}
	channel, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel named %s", cmd.Channel), "crosshall owner status --all lists the channels"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "repo:" + cmd.RepoRoot
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return r, err
	}
	if channel.state != "active" {
		return Refuse(core.CodeArchived, fmt.Sprintf("#%s is archived", channel.name), "choose an active channel: crosshall owner status --all"), nil
	}
	// The owner's command makes the default theirs, also when it names the
	// channel that was created for the repository.
	write, err := tx.ExecContext(ctx, `INSERT INTO repo_default(repo_root, channel_id) VALUES (?, ?)
		ON CONFLICT(repo_root) DO UPDATE SET channel_id = excluded.channel_id, automatic = 0
		WHERE repo_default.channel_id <> excluded.channel_id OR repo_default.automatic <> 0`, cmd.RepoRoot, channel.id)
	if err != nil {
		return core.Result{}, err
	}
	changed, err := write.RowsAffected()
	if err != nil {
		return core.Result{}, err
	}
	out := ChannelDefaultResult{Channel: channel.name, RepoRoot: cmd.RepoRoot}
	if changed != 0 {
		if err := audit(ctx, tx, "channel_default_set", scope, owner, out, now); err != nil {
			return core.Result{}, err
		}
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

// defaultChannel resolves an opted-in repository without creating a channel.
func defaultChannel(ctx context.Context, tx store.Tx, repoRoot string) (channelRow, bool, error) {
	var c channelRow
	err := tx.QueryRowContext(ctx, `SELECT c.id, c.name, c.topic, c.state
		FROM repo_default d JOIN channel c ON c.id = d.channel_id WHERE d.repo_root = ?`, repoRoot).
		Scan(&c.id, &c.name, &c.topic, &c.state)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// A committed join replays its original channel even after a default change,
// provided that channel still accepts joins.
func replayRepoJoin(ctx context.Context, tx store.Tx, principal int64, key, scope string, cmd Join) (core.Result, bool, error) {
	r, found, err := replay(ctx, tx, principal, key, scope, cmd)
	if err != nil || !found || r.Status != core.ExitOK {
		return r, found, err
	}
	var original JoinResult
	if err := json.Unmarshal(r.Data, &original); err != nil {
		return core.Result{}, false, fmt.Errorf("domain: stored join result: %w", err)
	}
	ch, exists, err := channelByName(ctx, tx, original.Channel)
	if err != nil {
		return core.Result{}, false, err
	}
	if !exists {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel #%s", original.Channel), "the owner can list the channels (crosshall owner status --all); a new session (for a shell, crosshall run -- sh) can join an active channel: crosshall join <channel> --as <alias>"), true, nil
	}
	if ch.state != "active" {
		return Refuse(core.CodeArchived, fmt.Sprintf("#%s is archived", ch.name), "the owner can list the active channels (crosshall owner status --all); a new session (for a shell, crosshall run -- sh) can join an active channel: crosshall join <channel> --as <alias>"), true, nil
	}
	return r, true, nil
}

// AdoptRepoDefault moves the default kept under a main checkout's path to
// its repository's identity, where defaults are kept now. The owner's own
// default always wins over one crosshall created: it replaces an automatic
// default at the identity. Only a default the owner set at the identity
// itself, which is the later command, stays in place of the older one.
func AdoptRepoDefault(ctx context.Context, tx store.Tx, root, identity string, now time.Time) error {
	var old, current int64
	var oldAutomatic, automatic bool
	err := tx.QueryRowContext(ctx, `SELECT channel_id, automatic FROM repo_default WHERE repo_root = ?`, root).Scan(&old, &oldAutomatic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	outcome := "moved"
	err = tx.QueryRowContext(ctx, `SELECT channel_id, automatic FROM repo_default WHERE repo_root = ?`, identity).Scan(&current, &automatic)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `UPDATE repo_default SET repo_root = ? WHERE repo_root = ?`, identity, root)
	case err != nil:
	case automatic && !oldAutomatic:
		outcome = "replaced_automatic"
		if _, err = tx.ExecContext(ctx, `UPDATE repo_default SET channel_id = ?, automatic = 0 WHERE repo_root = ?`, old, identity); err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM repo_default WHERE repo_root = ?`, root)
		}
	default:
		outcome = "superseded"
		_, err = tx.ExecContext(ctx, `DELETE FROM repo_default WHERE repo_root = ?`, root)
	}
	if err != nil {
		return err
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return err
	}
	detail := map[string]any{"from": root, "outcome": outcome, "channel_id": old}
	if outcome != "moved" {
		detail["identity_channel_id"] = current
	}
	return audit(ctx, tx, "channel_default_adopted", "repo:"+identity, owner, detail, now)
}
