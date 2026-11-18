package domain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbOwnerWho = "owner who"
	VerbOwnerLog = "owner log"
)

// OwnerWho lists bindings across all channels.
type OwnerWho struct{}

func (OwnerWho) command()     {}
func (OwnerWho) Verb() string { return VerbOwnerWho }

// OwnerLog reads a page of a named channel's history, like the member log.
type OwnerLog struct {
	Channel string `json:"channel"`
	Since   *int64 `json:"since,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (OwnerLog) command()     {}
func (OwnerLog) Verb() string { return VerbOwnerLog }

type OwnerWhoResult struct {
	Channels []OwnerWhoChannel `json:"channels"`
}

type OwnerWhoChannel struct {
	Name    string        `json:"name"`
	Topic   string        `json:"topic,omitempty"`
	Members []OwnerMember `json:"members"`
}

type OwnerMember struct {
	Member
	BindingState string `json:"binding_state"`
}

func (s *Service) ownerWho(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the owner can list bindings across channels", "crosshall who"), nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id, c.name, c.topic, b.id, b.alias, b.state, b.focus, p.vendor,
		COALESCE((SELECT MAX(o.observed_at_ms) FROM observation o WHERE o.principal_id=b.principal_id), b.joined_at_ms)
		FROM channel c LEFT JOIN binding b ON b.channel_id=c.id LEFT JOIN principal p ON p.id=b.principal_id
		ORDER BY c.id, b.id`)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	out := OwnerWhoResult{Channels: []OwnerWhoChannel{}}
	var previous int64
	for rows.Next() {
		var channel int64
		var name, topic string
		var binding, seen sql.NullInt64
		var alias, bindingState, focus, vendor sql.NullString
		if err := rows.Scan(&channel, &name, &topic, &binding, &alias, &bindingState, &focus, &vendor, &seen); err != nil {
			return core.Result{}, err
		}
		if len(out.Channels) == 0 || channel != previous {
			out.Channels = append(out.Channels, OwnerWhoChannel{Name: name, Topic: topic, Members: []OwnerMember{}})
			previous = channel
		}
		if binding.Valid {
			member := OwnerMember{
				Member: Member{
					Alias: alias.String, Focus: focus.String, Vendor: vendor.String, BindingID: binding.Int64,
					State: "unknown", LastCommandAgoS: max((ms(now)-seen.Int64)/1000, 0), Route: "pull",
				},
				BindingState: bindingState.String,
			}
			if member.BindingState == "departed" {
				member.Route = "unavailable"
			}
			ch := &out.Channels[len(out.Channels)-1]
			ch.Members = append(ch.Members, member)
		}
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

func (s *Service) ownerLog(ctx context.Context, tx store.Tx, caller core.Caller, cmd OwnerLog) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the named-channel log requires the owner", "crosshall log"), nil
	}
	if !ValidName(cmd.Channel) {
		return Fail(core.ExitUsage, "", "name the channel to inspect", "crosshall owner log <channel>"), nil
	}
	limit, bad := logLimit(Log{Since: cmd.Since, Limit: cmd.Limit})
	if bad != nil {
		return *bad, nil
	}
	ch, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel #%s", cmd.Channel), "crosshall owner who --all"), nil
	}
	out, err := ChannelLog(ctx, tx, ch.id, cmd.Since, limit)
	if err != nil {
		return core.Result{}, err
	}
	out.Channel = ch.name
	return OK(out)
}
